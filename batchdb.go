/*
 * Copyright 2020 Saffat Technologies, Ltd.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package unitdb

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/unit-io/bpool"
)

type (
	batchIndex struct {
		delFlag bool
		offset  int64
	}
	tinyBatch struct {
		sync.RWMutex
		ID      int64
		managed bool
		buffer  *bpool.Buffer

		entryCount uint32
		size       int64
		entries    []uint64
		index      []batchIndex

		doneChan chan struct{}
	}
)

func (db *DB) newTinyBatch() *tinyBatch {
	// Backoff to limit excess memroy usage
	db.mem.Backoff()
	tinyBatch := &tinyBatch{ID: db.timeID(), buffer: db.bufPool.Get(), doneChan: make(chan struct{})}
	return tinyBatch
}

func (b *tinyBatch) timeID() int64 {
	return atomic.LoadInt64(&b.ID)
}

func (b *tinyBatch) len() uint32 {
	return atomic.LoadUint32(&b.entryCount)
}

func (b *tinyBatch) incount() uint32 {
	return atomic.AddUint32(&b.entryCount, 1)
}

func (b *tinyBatch) reset() {
	b.Lock()
	defer b.Unlock()
	atomic.StoreUint32(&b.entryCount, 0)
	b.size = 0
	b.entries = b.entries[:0]
	b.index = b.index[:0]
}

func (b *tinyBatch) abort() {
	b.reset()
	close(b.doneChan)
}

// setManaged sets batch managed.
func (b *tinyBatch) setManaged() {
	b.managed = true
}

// unsetManaged sets batch unmanaged.
func (b *tinyBatch) unsetManaged() {
	b.managed = false
}

type batchPool struct {
	db           *DB
	maxBatches   int
	writeQueue   chan *tinyBatch
	batchQueue   chan *tinyBatch
	waitingQueue queue
	stoppedChan  chan struct{}
	stopOnce     sync.Once
	stopped      int32
	waiting      int32
	wait         bool
}

// batchdb manages the batch execution.
type batchdb struct {
	*batchPool
	bufPool *bpool.BufferPool

	//tiny Batch
	tinyBatchLockC chan struct{}
	tinyBatch      *tinyBatch
}

func (db *DB) newBatchPool(maxBatches int) *batchPool {
	// There must be at least one batch.
	if maxBatches < 1 {
		maxBatches = 1
	}

	pool := &batchPool{
		db:          db,
		maxBatches:  maxBatches,
		writeQueue:  make(chan *tinyBatch, 1),
		batchQueue:  make(chan *tinyBatch),
		stoppedChan: make(chan struct{}),
	}

	// start the batch dispatcher
	go pool.dispatch()

	return pool
}

func (db *DB) initbatchdb(opts *options) error {
	bdb := &batchdb{
		bufPool:        bpool.NewBufferPool(opts.bufferSize, &bpool.Options{MaxElapsedTime: 10 * time.Second}),
		tinyBatchLockC: make(chan struct{}, 1),
	}

	db.batchdb = bdb
	bdb.batchPool = db.newBatchPool(nPoolSize)
	bdb.tinyBatch = db.newTinyBatch()

	go db.tinyBatchLoop(opts.tinyBatchWriteInterval)
	return nil
}

// size returns maximum number of concurrent batches.
func (p *batchPool) size() int {
	return p.maxBatches
}

// stop tells dispatcher to exit, and wether or not complete queued batches.
func (p *batchPool) stop(wait bool) {
	// Acquire tinyBatch write lock
	p.db.tinyBatchLockC <- struct{}{}
	defer func() {
		<-p.db.tinyBatchLockC
	}()
	p.stopOnce.Do(func() {
		atomic.StoreInt32(&p.stopped, 1)
		p.wait = wait
		// Close write queue and wait for currently running batches to finish
		close(p.writeQueue)
	})
	<-p.stoppedChan
}

// stopWait stops batch pool and wait for all queued batches to complete.
func (p *batchPool) stopWait() {
	p.stop(true)
}

// stopped returns true if batch pool has been stopped.
func (p *batchPool) isStopped() bool {
	return atomic.LoadInt32(&p.stopped) != 0
}

// waitQueueSize returns count of batches in waitingQueue.
func (p *batchPool) waitQueueSize() int {
	return int(atomic.LoadInt32(&p.waiting))
}

// write enqueues a batch to write.
func (p *batchPool) write(tinyBatch *tinyBatch) {
	if tinyBatch != nil {
		p.writeQueue <- tinyBatch
	}
}

// witeWait enqueues the given batch and waits for it to be executed.
func (p *batchPool) writeWait(tinyBatch *tinyBatch) {
	if tinyBatch == nil {
		return
	}
	p.writeQueue <- tinyBatch
	<-tinyBatch.doneChan
}

// batch starts a new batch.
func (db *DB) batch() *Batch {
	opts := &options{}
	WithDefaultBatchOptions().set(opts)
	opts.batchOptions.encryption = db.encryption == 1
	b := &Batch{opts: opts, db: db, tinyBatchLockC: make(chan struct{}, 1), tinyBatchGroup: make(map[int64]*tinyBatch)}
	b.tinyBatch = db.newTinyBatch()
	return b
}

// Batch executes a function within the context of a read-write managed transaction.
// If no error is returned from the function then the transaction is written.
// If an error is returned then the entire transaction is rolled back.
// Any error that is returned from the function or returned from the write is
// returned from the Batch() method.
//
// Attempting to manually commit or rollback within the function will cause a panic.
func (db *DB) Batch(fn func(*Batch, <-chan struct{}) error) error {
	b := db.batch()

	b.setManaged()
	b.commitComplete = make(chan struct{})
	if b.opts.writeInterval != 0 {
		go b.writeLoop(b.opts.writeInterval)
	}
	// fmt.Println("Batch: batch started... ", b.tinyBatch.timeID())
	// If an error is returned from the function then rollback and return error.
	if err := fn(b, b.commitComplete); err != nil {
		b.Abort()
		close(b.commitComplete)
		return err
	}
	b.unsetManaged()
	return b.Commit()
}

// tinyBatchLoop handles tiny batches.
func (db *DB) tinyBatchLoop(interval time.Duration) {
	db.closeW.Add(1)
	defer db.closeW.Done()
	tinyBatchTicker := time.NewTicker(interval)
	for {
		select {
		case <-db.closeC:
			tinyBatchTicker.Stop()
			return
		case <-tinyBatchTicker.C:
			if db.tinyBatch.len() != 0 {
				db.tinyBatchLockC <- struct{}{}
				db.batchPool.write(db.tinyBatch)
				db.tinyBatch = db.newTinyBatch()
				<-db.tinyBatchLockC
			}
		}
	}
}

// dispatch handles tiny batch commit for the batches queue.
func (p *batchPool) dispatch() {
	defer close(p.stoppedChan)
	timeout := time.NewTimer(2 * time.Second)
	var batchCount int
	var idle bool
Loop:
	for {
		// As long as batches are in waiting queue, incoming
		// batch are put into the waiting queueand batches to run are taken from waiting queue.
		if p.waitingQueue.len() != 0 {
			if !p.processWaitingQueue() {
				break Loop
			}
			continue
		}

		select {
		case tinyBatch, ok := <-p.writeQueue:
			if !ok {
				break Loop
			}
			select {
			case p.batchQueue <- tinyBatch:
			default:
				if batchCount < nPoolSize {
					go p.commit(tinyBatch, p.batchQueue)
					batchCount++
				} else {
					// Enqueue batch to be executed later.
					p.waitingQueue.push(tinyBatch)
					atomic.StoreInt32(&p.waiting, int32(p.waitingQueue.len()))
				}
			}
			idle = false
		case <-timeout.C:
			if idle && batchCount > 0 {
				if p.killIdleBatch() {
					batchCount--
				}
			}
			idle = true
			timeout.Reset(2 * time.Second)
		}
	}

	// If instructed to wait, then run batches that are already in queue.
	if p.wait {
		p.runQueuedBatches()
	}

	// Stop all remaining tinyBatch as it become ready.
	for batchCount > 0 {
		p.batchQueue <- nil
		batchCount--
	}

	timeout.Stop()
}

// commit run initial tinyBatch commit, then start tinyBatch waiting for more.
func (p *batchPool) commit(tinyBatch *tinyBatch, batchQueue chan *tinyBatch) {
	if err := p.db.tinyCommit(tinyBatch); err != nil {
		logger.Error().Err(err).Str("context", "tinyCommit").Msgf("Error committing tinyBatch")
		p.db.rollback(tinyBatch)
	}

	go p.tinyCommit(batchQueue)
}

// tinyCommit commits batch and stops when it receive a nil batch.
func (p *batchPool) tinyCommit(batchQueue chan *tinyBatch) {
	// abort time window entries
	defer p.db.abort()

	for tinyBatch := range batchQueue {
		if tinyBatch == nil {
			return
		}

		if err := p.db.tinyCommit(tinyBatch); err != nil {
			logger.Error().Err(err).Str("context", "tinyCommit").Msgf("Error committing tinyBatch")
			p.db.rollback(tinyBatch)
		}
	}
}

// processWaiting queue puts new batches onto the waiting queue,
// removes batches from the waiting queue. Returns false if batchPool is stopped.
func (p *batchPool) processWaitingQueue() bool {
	select {
	case b, ok := <-p.writeQueue:
		if !ok {
			return false
		}
		p.waitingQueue.push(b)
	case p.batchQueue <- p.waitingQueue.front():
		p.waitingQueue.pop()
	}
	atomic.StoreInt32(&p.waiting, int32(p.waitingQueue.len()))
	return true
}

func (p *batchPool) killIdleBatch() bool {
	select {
	case p.batchQueue <- nil:
		return true
	default:
		return false
	}
}

// runQueuedBatches removes each batch from the waiting queue and
// process it until queue is empty.
func (p *batchPool) runQueuedBatches() {
	if p.waitingQueue.len() != 0 {
		p.batchQueue <- p.waitingQueue.pop()
		atomic.StoreInt32(&p.waiting, int32(p.waitingQueue.len()))
	}
}

type queue struct {
	buf   []*tinyBatch
	head  int
	tail  int
	count int
}

// len returns the number of elements currently stored in the queue.
func (q *queue) len() int {
	return q.count
}

// push appends an element to the back of the queue.
func (q *queue) push(elem *tinyBatch) {
	q.grow()

	q.buf[q.tail] = elem
	// calculate new tail position.
	q.tail = (q.tail + 1) & (len(q.buf) - 1) // bitwise modulus
	q.count++
}

// pop removes and return an element from front of the queue.
func (q *queue) pop() *tinyBatch {
	if q.count <= 0 {
		panic("batchPool: pop called on empty queue")
	}
	elem := q.buf[q.head]
	q.buf[q.head] = nil
	// Calculate new head position.
	q.head = (q.head + 1) & (len(q.buf) - 1) // bitwise modulus
	q.count--
	q.shrink()
	return elem
}

// front returns element at the front of the queue. This is the element
// that would be returned by pop().
func (q *queue) front() *tinyBatch {
	if q.count <= 0 {
		panic("batchPool: pop called on empty queue")
	}
	return q.buf[q.head]
}

// at returns element at index i in the queue without removing element from the queue.
// at(0) refers to first element and is same as front(). at(len()0-1) refers to the last element.
func (q *queue) at(i int) *tinyBatch {
	if i < 0 || i > q.count {
		panic("batchPool: at called with index out of range")
	}
	// bitwise modulus
	return q.buf[(q.head+i)&(len(q.buf)-1)]
}

// grow resizes the queue to fit exactly twice its current content.
func (q *queue) grow() {
	if len(q.buf) == 0 {
		q.buf = make([]*tinyBatch, nPoolSize)
		return
	}
	if q.count == len(q.buf) {
		q.resize()
	}
}

// shrink resizes the queue down if bugger if 1/4 full.
func (q *queue) shrink() {
	if len(q.buf) > nPoolSize && (q.count<<2) == len(q.buf) {
		q.resize()
	}
}

// resize resizes the queue to fit exactly twice its current content.
func (q *queue) resize() {
	newBuf := make([]*tinyBatch, q.count<<1)
	if q.tail > q.head {
		copy(newBuf, q.buf[q.head:q.tail])
	} else {
		n := copy(newBuf, q.buf[q.head:])
		copy(newBuf[n:], q.buf[:q.tail])
	}

	q.head = 0
	q.tail = q.count
	q.buf = newBuf
}
