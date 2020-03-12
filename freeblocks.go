package tracedb

import (
	"encoding/binary"
	"sort"
	"sync"

	"github.com/unit-io/tracedb/hash"
)

// A "thread" safe freeslot.
// To avoid lock bottlenecks slots are divided into several shards (nShards).
// type freeslots []*freeslot

type freeslots struct {
	slots      []*freeslot
	consistent *hash.Consistent
}

type freeslot struct {
	sm           map[uint64]bool
	sync.RWMutex // Read Write mutex, guards access to internal collection.
}

// newFreeSlots creates a new concurrent free slots.
func newFreeSlots() freeslots {
	s := freeslots{
		slots:      make([]*freeslot, nShards+1),
		consistent: hash.InitConsistent(int(nShards), int(nShards)),
	}

	for i := 0; i <= nShards; i++ {
		s.slots[i] = &freeslot{sm: make(map[uint64]bool)}
	}

	return s
}

// getShard returns shard under given contract
func (fss *freeslots) getShard(contract uint64) *freeslot {
	return fss.slots[fss.consistent.FindBlock(contract)]
}

// get first free seq
func (fss *freeslots) get(contract uint64) (ok bool, seq uint64) {
	// Get shard
	shard := fss.getShard(contract)
	shard.Lock()
	defer shard.Unlock()
	for seq, ok = range shard.sm {
		delete(shard.sm, seq)
		return ok, seq
	}

	return false, seq
}

func (fss *freeslots) free(seq uint64) (ok bool) {
	// Get shard
	shard := fss.getShard(seq)
	shard.Lock()
	defer shard.Unlock()
	if ok := shard.sm[seq]; ok {
		return !ok
	}
	shard.sm[seq] = true
	return true
}

func (fs *freeslot) len() int {
	return len(fs.sm)
}

// A "thread" safe freeblocks.
// To avoid lock bottlenecks slots are divided into several shards (nShards).
// type freeblocks []*freeblock
type freeblocks struct {
	blocks                []*shard
	size                  int64 // total size of free blocks
	minimumFreeBlocksSize int64 // minimum free blocks size before free blocks are reused for new allocation.
	consistent            *hash.Consistent
}

type freeblock struct {
	offset int64
	size   uint32
}

type shard struct {
	blocks       []freeblock
	cache        map[int64]bool // cache free offset
	sync.RWMutex                // Read Write mutex, guards access to internal collection.
}

// newFreeBlocks creates a new concurrent freeblocks.
func newFreeBlocks(minimumSize int64) freeblocks {
	fb := freeblocks{
		blocks:                make([]*shard, nShards),
		minimumFreeBlocksSize: minimumSize,
		consistent:            hash.InitConsistent(int(nShards), int(nShards)),
	}

	for i := 0; i < nShards; i++ {
		fb.blocks[i] = &shard{cache: make(map[int64]bool)}
	}

	return fb
}

// getShard returns shard under given contract
func (fb *freeblocks) getShard(contract uint64) *shard {
	return fb.blocks[fb.consistent.FindBlock(contract)]
}

func (s *shard) search(size uint32) int {
	// limit search to first 100 freeblocks
	return sort.Search(100, func(i int) bool {
		return s.blocks[i].size >= size
	})
}

// contains checks whether a message id is in the set.
func (s *shard) contains(off int64) bool {
	for _, v := range s.blocks {
		if v.offset == off {
			return true
		}
	}
	return false
}

func (s *shard) defrag() {
	l := len(s.blocks)
	if l <= 1 {
		return
	}
	// limit fragmentation to first 1000 freeblocks
	if l > 1000 {
		l = 1000
	}
	sort.Slice(s.blocks[:l], func(i, j int) bool {
		return s.blocks[i].offset < s.blocks[j].offset
	})
	var merged []freeblock
	curOff := s.blocks[0].offset
	curSize := s.blocks[0].size
	for i := 1; i < l; i++ {
		if curOff+int64(curSize) == s.blocks[i].offset {
			curSize += s.blocks[i].size
			delete(s.cache, s.blocks[i].offset)
		} else {
			merged = append(merged, freeblock{size: curSize, offset: curOff})
			curOff = s.blocks[i].offset
			curSize = s.blocks[i].size
		}
	}
	merged = append(merged, freeblock{offset: curOff, size: curSize})
	sort.Slice(merged, func(i, j int) bool {
		return merged[i].size < merged[j].size
	})
	copy(s.blocks[:l], merged)
}

func (fb *freeblocks) defrag() {
	for i := 0; i < nShards; i++ {
		shard := fb.blocks[i]
		shard.defrag()
	}
}

func (fb *freeblocks) free(off int64, size uint32) {
	if size == 0 {
		panic("unable to free zero bytes")
	}
	shard := fb.getShard(uint64(off))
	shard.Lock()
	defer shard.Unlock()
	// Verify that block is not already free.
	if shard.cache[off] {
		return
	}
	// }
	shard.blocks = append(shard.blocks, freeblock{offset: off, size: size})
	shard.cache[off] = true
	fb.size += int64(size)
}

func (fb *freeblocks) allocate(size uint32) int64 {
	if size == 0 {
		panic("unable to allocate zero bytes")
	}
	if fb.size < fb.minimumFreeBlocksSize {
		return -1
	}
	shard := fb.getShard(uint64(size))
	shard.Lock()
	defer shard.Unlock()
	if len(shard.blocks) < 100 {
		return -1
	}
	i := shard.search(size)
	if i >= len(shard.blocks) {
		return -1
	}
	off := shard.blocks[i].offset
	if shard.blocks[i].size == size {
		copy(shard.blocks[i:], shard.blocks[i+1:])
		shard.blocks[len(shard.blocks)-1] = freeblock{}
		shard.blocks = shard.blocks[:len(shard.blocks)-1]
	} else {
		shard.blocks[i].size -= size
		shard.blocks[i].offset += int64(size)
	}
	delete(shard.cache, off)
	fb.size -= int64(size)
	return off
}

// MarshalBinary serializes freeblocks into binary data
func (s *shard) MarshalBinary() ([]byte, error) {
	size := s.binarySize()
	buf := make([]byte, size)
	data := buf
	binary.LittleEndian.PutUint32(data[:4], uint32(len(s.blocks)))
	data = data[4:]
	for i := 0; i < len(s.blocks); i++ {
		binary.LittleEndian.PutUint64(data[:8], uint64(s.blocks[i].offset))
		binary.LittleEndian.PutUint32(data[8:12], s.blocks[i].size)
		data = data[12:]
	}
	return buf, nil
}

func (s *shard) binarySize() uint32 {
	return uint32((4 + (8+4)*len(s.blocks))) // FIXME: this is ugly
}

func (fb *freeblocks) read(f file, off int64) error {
	if off == -1 {
		return nil
	}

	var size uint32
	offset := off
	for i := 0; i < nShards; i++ {
		shard := fb.blocks[i]
		buf := make([]byte, 4)
		if _, err := f.ReadAt(buf, offset); err != nil {
			return err
		}
		n := binary.LittleEndian.Uint32(buf)
		size += n
		buf = make([]byte, (4+8)*n)
		if _, err := f.ReadAt(buf, offset+4); err != nil {
			return err
		}
		for i := uint32(0); i < n; i++ {
			blockOff := int64(binary.LittleEndian.Uint64(buf[:8]))
			blockSize := binary.LittleEndian.Uint32(buf[8:12])
			if blockOff != 0 {
				shard.blocks = append(shard.blocks, freeblock{size: blockSize, offset: blockOff})
				fb.size += int64(blockSize)
			}
			buf = buf[12:]
		}
		offset += int64(12 * n)
	}
	fb.free(off, align(4+size*12))
	return nil
}

func (fb *freeblocks) write(f file) (int64, error) {
	if len(fb.blocks) == 0 {
		return -1, nil
	}
	var marshaledSize uint32
	var buf []byte
	for i := 0; i < nShards; i++ {
		shard := fb.blocks[i]
		marshaledSize += align(shard.binarySize())
		data, err := shard.MarshalBinary()
		buf = append(buf, data...)
		if err != nil {
			return -1, err
		}
	}
	off, err := f.extend(marshaledSize)
	if err != nil {
		return -1, err
	}
	_, err = f.WriteAt(buf, off)
	return off, err
}
