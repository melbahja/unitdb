package unitdb

import (
	"encoding/binary"
	"fmt"
	"io"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/unit-io/bpool"
	"github.com/unit-io/unitdb/hash"
)

type (
	winEntry struct {
		contract uint64
		seq      uint64
	}
	winBlock struct {
		contract   uint64
		topicHash  uint64
		winEntries [seqsPerWindowBlock]winEntry
		next       int64 //next stores offset that links multiple winBlocks for a topic hash. Most recent offset is stored into the trie to iterate entries in reverse order)
		entryIdx   uint16

		dirty  bool
		leased bool
	}
)

func (e winEntry) time() uint32 {
	return 0
}

func (e winEntry) Seq() uint64 {
	return e.seq
}

// MarshalBinary serialized window block into binary data
func (w winBlock) MarshalBinary() []byte {
	buf := make([]byte, blockSize)
	data := buf
	for i := 0; i < seqsPerWindowBlock; i++ {
		e := w.winEntries[i]
		binary.LittleEndian.PutUint64(buf[:8], e.seq)
		buf = buf[8:]
	}
	binary.LittleEndian.PutUint64(buf[:8], w.contract)
	binary.LittleEndian.PutUint64(buf[8:16], w.topicHash)
	binary.LittleEndian.PutUint64(buf[16:24], uint64(w.next))
	binary.LittleEndian.PutUint16(buf[24:26], w.entryIdx)
	return data
}

// UnmarshalBinary de-serialized window block from binary data
func (w *winBlock) UnmarshalBinary(data []byte) error {
	for i := 0; i < seqsPerWindowBlock; i++ {
		_ = data[8] // bounds check hint to compiler; see golang.org/issue/14808
		w.winEntries[i].seq = binary.LittleEndian.Uint64(data[:8])
		data = data[8:]
	}
	w.contract = binary.LittleEndian.Uint64(data[:8])
	w.topicHash = binary.LittleEndian.Uint64(data[8:16])
	w.next = int64(binary.LittleEndian.Uint64(data[16:24]))
	w.entryIdx = binary.LittleEndian.Uint16(data[24:26])
	return nil
}

type windowHandle struct {
	winBlock
	file
	offset int64
}

func winBlockOffset(idx int32) int64 {
	return (int64(blockSize) * int64(idx))
}

func (wh *windowHandle) read() error {
	buf, err := wh.file.Slice(wh.offset, wh.offset+int64(blockSize))
	if err != nil {
		return err
	}
	return wh.UnmarshalBinary(buf)
}

// func (wh *windowHandle) write() error {
// 	if wh.entryIdx == 0 {
// 		return nil
// 	}
// 	buf := wh.MarshalBinary()
// 	_, err := wh.file.WriteAt(buf, wh.offset)

// 	return err
// }

// A "thread" safe timeWindows.
// To avoid lock bottlenecks timeWindows are dived to several (nShards).
type timeWindows struct {
	sync.RWMutex
	windows    []*windows
	consistent *hash.Consistent
}

type windows struct {
	expiry map[int64]windowEntries // map[expiryHash]windowEntries
	timeWindow
	mu sync.RWMutex // Read Write mutex, guards access to internal collection.
}

// newTimeWindows creates a new concurrent timeWindows.
func newTimeWindows() *timeWindows {
	w := &timeWindows{
		windows:    make([]*windows, nShards),
		consistent: hash.InitConsistent(int(nShards), int(nShards)),
	}

	for i := 0; i < nShards; i++ {
		w.windows[i] = &windows{expiry: make(map[int64]windowEntries), timeWindow: timeWindow{friezedEntries: make(map[uint64]windowEntries), entries: make(map[uint64]windowEntries)}}
	}

	return w
}

// getWindows returns shard under given key
func (w *timeWindows) getWindows(key uint64) *windows {
	w.RLock()
	defer w.RUnlock()
	return w.windows[w.consistent.FindBlock(key)]
}

type timeWindowEntry interface {
	Seq() uint64
	time() uint32
}

type windowEntries []timeWindowEntry
type timeWindow struct {
	freezed        bool
	entries        map[uint64]windowEntries // map[topicHash]windowEntries
	friezedEntries map[uint64]windowEntries
}

type (
	timeOptions struct {
		expDurationType     time.Duration
		maxExpDurations     int
		windowDurationType  time.Duration
		maxWindowDurations  int
		backgroundKeyExpiry bool
	}
	timeWindowBucket struct {
		sync.RWMutex
		file
		*timeWindows
		windowIdx          int32
		earliestExpiryHash int64
		opts               *timeOptions
	}
)

func (src *timeOptions) copyWithDefaults() *timeOptions {
	opts := timeOptions{}
	if src != nil {
		opts = *src
	}
	if opts.expDurationType == 0 {
		opts.expDurationType = time.Minute
	}
	if opts.windowDurationType == 0 {
		opts.expDurationType = time.Hour
	}
	if opts.maxExpDurations == 0 {
		opts.maxExpDurations = 1
	}
	if opts.maxWindowDurations == 0 {
		opts.maxWindowDurations = 24
	}
	return &opts
}

func newTimeWindowBucket(f file, opts *timeOptions) *timeWindowBucket {
	l := &timeWindowBucket{file: f, windowIdx: -1}
	l.timeWindows = newTimeWindows()
	l.opts = opts.copyWithDefaults()
	return l
}

type windowWriter struct {
	*timeWindowBucket
	winBlocks map[int32]winBlock // map[windowIdx]winBlock

	buffer *bpool.Buffer

	leasing map[int32][]uint64 // map[blockIdx][]seq
}

func newWindowWriter(wb *timeWindowBucket, buf *bpool.Buffer) *windowWriter {
	return &windowWriter{winBlocks: make(map[int32]winBlock), timeWindowBucket: wb, buffer: buf, leasing: make(map[int32][]uint64)}
}

func (wb *timeWindowBucket) expireOldEntries(maxResults int) []timeWindowEntry {
	if !wb.opts.backgroundKeyExpiry {
		return nil
	}
	var expiredEntries []timeWindowEntry
	startTime := uint32(time.Now().Unix())

	if atomic.LoadInt64(&wb.earliestExpiryHash) > int64(startTime) {
		return expiredEntries
	}

	for i := 0; i < nShards; i++ {
		// get windows shard
		ws := wb.timeWindows.windows[i]
		ws.mu.Lock()
		defer ws.mu.Unlock()
		windowTimes := make([]int64, 0, len(ws.expiry))
		for windowTime := range ws.expiry {
			windowTimes = append(windowTimes, windowTime)
		}
		sort.Slice(windowTimes[:], func(i, j int) bool { return windowTimes[i] < windowTimes[j] })
		for i := 0; i < len(windowTimes); i++ {
			if windowTimes[i] > int64(startTime) || len(expiredEntries) > maxResults {
				break
			}
			windowEntries := ws.expiry[windowTimes[i]]
			expiredEntriesCount := 0
			for i := range windowEntries {
				entry := windowEntries[i]
				if entry.time() < startTime {
					expiredEntries = append(expiredEntries, entry)
					expiredEntriesCount++
				}
			}
			if expiredEntriesCount == len(windowEntries) {
				delete(ws.expiry, windowTimes[i])
			}
		}
	}
	atomic.StoreInt64(&wb.earliestExpiryHash, 0)
	return expiredEntries
}

// addExpiry adds expiry for entries expiring. Entries expires in future are not added to expiry window
func (wb *timeWindowBucket) addExpiry(e timeWindowEntry) error {
	if !wb.opts.backgroundKeyExpiry {
		return nil
	}
	timeExpiry := int64(time.Unix(int64(e.time()), 0).Truncate(wb.opts.expDurationType).Add(1 * wb.opts.expDurationType).Unix())
	atomic.CompareAndSwapInt64(&wb.earliestExpiryHash, 0, timeExpiry)

	// get windows shard
	ws := wb.getWindows(uint64(e.time()))
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if expiryWindow, ok := ws.expiry[timeExpiry]; ok {
		expiryWindow = append(expiryWindow, e)
		ws.expiry[timeExpiry] = expiryWindow
	} else {
		ws.expiry[timeExpiry] = windowEntries{e}
	}

	return nil
}

func (wb *timeWindowBucket) add(topicHash uint64, e timeWindowEntry) error {
	// get windows shard
	ws := wb.getWindows(topicHash)
	ws.mu.Lock()
	defer ws.mu.Unlock()

	if ws.freezed {
		if _, ok := ws.friezedEntries[topicHash]; ok {
			ws.friezedEntries[topicHash] = append(ws.friezedEntries[topicHash], e)
		} else {
			ws.friezedEntries[topicHash] = []timeWindowEntry{e}
		}
		return nil
	}
	if _, ok := ws.entries[topicHash]; ok {
		ws.entries[topicHash] = append(ws.entries[topicHash], e)
	} else {
		ws.entries[topicHash] = []timeWindowEntry{e}
	}
	return nil
}

func (w *timeWindow) reset() error {
	w.entries = make(map[uint64]windowEntries)
	return nil
}

func (w *timeWindow) freeze() error {
	w.freezed = true
	return nil
}

func (w *timeWindow) unFreeze() error {
	w.freezed = false
	for h := range w.friezedEntries {
		w.entries[h] = append(w.entries[h], w.friezedEntries[h]...)
	}
	w.friezedEntries = make(map[uint64]windowEntries)
	return nil
}

// foreachTimeWindow iterates timewindow entries during sync or recovery process to write entries to window file
// it takes timewindow snapshot to iterate and deletes blocks from timewindow
func (wb *timeWindowBucket) foreachTimeWindow(freeze bool, f func(last bool, w map[uint64]windowEntries) (bool, error)) (err error) {
	for i := 0; i < nShards; i++ {
		ws := wb.timeWindows.windows[i]
		ws.mu.RLock()
		wEntries := make(map[uint64]windowEntries)
		if freeze {
			ws.freeze()
		}
		for h, entries := range ws.entries {
			wEntries[h] = entries
		}
		ws.mu.RUnlock()
		stop, err1 := f(i == nShards-1, wEntries)
		if stop || err1 != nil {
			err = err1
			if freeze {
				ws.mu.Lock()
				ws.unFreeze()
				ws.mu.Unlock()
			}
			continue
		}
		if freeze {
			ws.mu.Lock()
			ws.reset()
			ws.unFreeze()
			ws.mu.Unlock()
		}
	}
	return err
}

// foreachWindowBlock iterates winBlocks on DB init to store topic hash and last offset of topic into trie.
func (wb *timeWindowBucket) foreachWindowBlock(f func(windowHandle) (bool, error)) (err error) {
	winBlockIdx := int32(0)
	nWinBlocks := wb.windowIndex()
	for winBlockIdx < nWinBlocks {
		off := winBlockOffset(winBlockIdx)
		b := windowHandle{file: wb.file, offset: off}
		if err := b.read(); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if stop, err := f(b); stop || err != nil {
			return err
		}
		winBlockIdx++
	}
	return nil
}

// ilookup lookups window entries from timeWindowBucket. These entries are not yet sync to DB
func (wb *timeWindowBucket) ilookup(topicHash uint64, limit int) (winEntries []winEntry) {
	winEntries = make([]winEntry, 0)
	// get windows shard
	ws := wb.getWindows(topicHash)
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	var l int
	wEntries := ws.friezedEntries[topicHash]
	if len(wEntries) > 0 {
		l = limit
		if len(wEntries) < limit {
			l = len(wEntries)
		}
		for _, we := range wEntries[len(wEntries)-l:] { // most recent entries are appended to the end so get the entries from end
			winEntries = append(winEntries, we.(winEntry))
		}
	}
	wEntries = ws.entries[topicHash]
	if len(wEntries) > 0 {
		l = limit - l
		if len(wEntries) < l {
			l = len(wEntries)
		}
		for _, we := range wEntries[len(wEntries)-l:] {
			winEntries = append(winEntries, we.(winEntry))
		}
	}
	sort.Slice(winEntries[:], func(i, j int) bool {
		return winEntries[i].Seq() > winEntries[j].Seq()
	})
	return winEntries
}

// lookup lookups window entries from window file.
func (wb *timeWindowBucket) lookup(topicHash uint64, off int64, skip int, limit int) (winEntries []winEntry, nextOff int64) {
	winEntries = make([]winEntry, 0)
	next := func(off int64, f func(windowHandle) (bool, error)) error {
		for {
			b := windowHandle{file: wb.file, offset: off}
			if err := b.read(); err != nil {
				return err
			}
			if stop, err := f(b); stop || err != nil {
				return err
			}
			if b.next == 0 {
				return nil
			}
			off = b.next
		}
	}
	count := 0
	err := next(off, func(curb windowHandle) (bool, error) {
		b := &curb
		if b.topicHash != topicHash {
			return true, nil
		}
		if skip > count+int(b.entryIdx) {
			count += int(b.entryIdx)
			return false, nil
		}
		if len(winEntries) > limit-int(b.entryIdx) {
			limit = limit - len(winEntries)
			winEntries = append(winEntries, b.winEntries[b.entryIdx-uint16(limit):b.entryIdx]...)
			nextOff = b.next
			return true, nil
		}
		winEntries = append(winEntries, b.winEntries[:b.entryIdx]...)
		return false, nil
	})
	if err != nil {
		return winEntries, nextOff
	}
	sort.Slice(winEntries[:], func(i, j int) bool {
		return winEntries[i].Seq() > winEntries[j].Seq()
	})
	return winEntries, nextOff
}

func (w winBlock) validation(topicHash uint64) error {
	if w.topicHash != topicHash {
		return fmt.Errorf("timeWindow.write: validation failed block topicHash %d, topicHash %d", w.topicHash, topicHash)
	}
	return nil
}

func (wb *windowWriter) del(seq uint64, bIdx int32) error {
	off := int64(blockSize * uint32(bIdx))
	w := windowHandle{file: wb.file, offset: off}
	if err := w.read(); err != nil {
		return err
	}
	entryIdx := -1
	for i := 0; i < int(w.entryIdx); i++ {
		e := w.winEntries[i]
		if e.seq == seq { //record exist in db
			entryIdx = i
			break
		}
	}
	if entryIdx == -1 {
		return nil // no entry in db to delete
	}
	w.entryIdx--

	i := entryIdx
	for ; i < entriesPerIndexBlock-1; i++ {
		w.winEntries[i] = w.winEntries[i+1]
	}
	w.winEntries[i] = winEntry{}

	return nil
}

// append appends window entries to buffer
func (wb *windowWriter) append(topicHash uint64, off int64, wEntries windowEntries) (newOff int64, err error) {
	var w winBlock
	var ok bool
	var winIdx int32
	if off == 0 {
		wb.windowIdx++
		winIdx = wb.windowIdx
	} else {
		winIdx = int32(off / int64(blockSize))
	}
	w, ok = wb.winBlocks[winIdx]
	if !ok && off > 0 {
		if winIdx <= wb.windowIdx {
			wh := windowHandle{file: wb.file, offset: off}
			if err := wh.read(); err != nil && err != io.EOF {
				fmt.Println("windowWriter.append: topicHash, off ", topicHash, off, err)
				return off, err
			}
			w = wh.winBlock
			w.validation(topicHash)
			w.leased = true
		}
	}
	w.topicHash = topicHash
	for _, we := range wEntries {
		if we.Seq() == 0 {
			continue
		}
		entryIdx := 0
		for i := 0; i < seqsPerWindowBlock; i++ {
			e := w.winEntries[i]
			if e.seq == we.Seq() { //record exist in db
				entryIdx = -1
				break
			}
		}
		if entryIdx == -1 {
			continue
		}
		if w.entryIdx == seqsPerWindowBlock {
			topicHash := w.topicHash
			next := int64(blockSize * uint32(winIdx))
			wb.winBlocks[winIdx] = w
			wb.windowIdx++
			winIdx = wb.windowIdx
			w = winBlock{topicHash: topicHash, next: next}
		}
		if w.leased {
			wb.leasing[winIdx] = append(wb.leasing[winIdx], we.Seq())
		}
		w.winEntries[w.entryIdx] = winEntry{seq: we.Seq()}
		w.dirty = true
		w.entryIdx++
	}

	wb.winBlocks[winIdx] = w
	return int64(blockSize * uint32(winIdx)), nil
}

func (wb *windowWriter) write() error {
	for bIdx, w := range wb.winBlocks {
		if !w.leased || !w.dirty {
			continue
		}
		off := int64(blockSize * uint32(bIdx))
		if _, err := wb.WriteAt(w.MarshalBinary(), off); err != nil {
			return err
		}
		w.dirty = false
		wb.winBlocks[bIdx] = w
	}

	// sort blocks by blockIdx
	var blockIdx []int
	for bIdx := range wb.winBlocks {
		if wb.winBlocks[bIdx].leased || !wb.winBlocks[bIdx].dirty {
			continue
		}
		blockIdx = append(blockIdx, int(bIdx))
	}
	sort.Ints(blockIdx)

	winBlocks, err := blockRange(blockIdx)
	if err != nil {
		return err
	}
	// fmt.Println("timeWindow.write: winBlocks ", winBlocks)
	bufOff := int64(0)
	for _, blocks := range winBlocks {
		if len(blocks) == 1 {
			bIdx := int32(blocks[0])
			off := int64(blockSize * uint32(bIdx))
			w := wb.winBlocks[bIdx]
			buf := w.MarshalBinary()
			if _, err := wb.WriteAt(buf, off); err != nil {
				return err
			}
			w.dirty = false
			wb.winBlocks[bIdx] = w
			continue
		}
		blockOff := int64(blockSize * uint32(blocks[0]))
		for bIdx := int32(blocks[0]); bIdx <= int32(blocks[1]); bIdx++ {
			w := wb.winBlocks[bIdx]
			wb.buffer.Write(w.MarshalBinary())
			w.dirty = false
			wb.winBlocks[bIdx] = w
		}
		blockData, err := wb.buffer.Slice(bufOff, wb.buffer.Size())
		if err != nil {
			return err
		}
		if _, err := wb.WriteAt(blockData, blockOff); err != nil {
			return err
		}
		bufOff = wb.buffer.Size()
	}
	return nil
}

func (wb *windowWriter) rollback() error {
	for bIdx, seqs := range wb.leasing {
		for _, seq := range seqs {
			if err := wb.del(seq, bIdx); err != nil {
				return err
			}
		}
	}
	return nil
}

func (wb *timeWindowBucket) windowIndex() int32 {
	return wb.windowIdx
}

func (wb *timeWindowBucket) setWindowIndex(windowIdx int32) error {
	wb.windowIdx = windowIdx
	return nil
}

// func (wb *timeWindowBucket) nextWindowIndex() int32 {
// 	wb.windowIdx++
// 	return wb.windowIdx
// }
