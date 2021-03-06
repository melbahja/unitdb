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
	"encoding/binary"
	"fmt"

	"github.com/unit-io/unitdb/fs"
)

const (
	slotSize         = 16
	blockSize uint32 = 4096
)

type (
	slot struct {
		seq       uint64
		topicSize uint16
		valueSize uint32
		msgOffset int64

		cacheBlock []byte // block from memdb if it exist
	}

	block struct {
		entries  [entriesPerIndexBlock]slot
		baseSeq  uint64
		next     uint32
		entryIdx uint16

		dirty  bool
		leased bool
	}

	blockHandle struct {
		block
		file   fs.FileManager
		offset int64
	}
)

func startBlockIndex(seq uint64) int32 {
	return int32(float64(seq-1) / float64(entriesPerIndexBlock))
}

func blockOffset(idx int32) int64 {
	if idx == -1 {
		return int64(headerSize)
	}
	return int64(headerSize + (blockSize * uint32(idx)))
}

func (s slot) mSize() uint32 {
	return idSize + uint32(s.topicSize) + s.valueSize
}

func (b block) validation(blockIdx int32) error {
	startBlockIdx := startBlockIndex(b.entries[0].seq)
	if startBlockIdx != blockIdx {
		return fmt.Errorf("validation failed blockIdx %d, startBlockIdx %d", blockIdx, startBlockIdx)
	}
	return nil
}

// MarshalBinary serialized entries block into binary data.
func (b block) MarshalBinary() []byte {
	buf := make([]byte, blockSize)
	data := buf

	b.baseSeq = b.entries[0].seq
	binary.LittleEndian.PutUint64(buf[:8], b.baseSeq)
	buf = buf[8:]
	for i := 0; i < entriesPerIndexBlock; i++ {
		s := b.entries[i]
		seq := uint16(0)
		if s.seq != 0 {
			seq = uint16(int16(s.seq-b.baseSeq) + entriesPerIndexBlock)
		}
		binary.LittleEndian.PutUint16(buf[:2], seq) // marshal relative seq
		binary.LittleEndian.PutUint16(buf[2:4], s.topicSize)
		binary.LittleEndian.PutUint32(buf[4:8], s.valueSize)
		binary.LittleEndian.PutUint64(buf[8:16], uint64(s.msgOffset))
		buf = buf[16:]
	}
	binary.LittleEndian.PutUint32(buf[:4], b.next)
	binary.LittleEndian.PutUint16(buf[4:6], b.entryIdx)
	return data
}

// UnmarshalBinary de-serialized entries block from binary data.
func (b *block) UnmarshalBinary(data []byte) error {
	b.baseSeq = binary.LittleEndian.Uint64(data[:8])
	data = data[8:]
	for i := 0; i < entriesPerIndexBlock; i++ {
		_ = data[16] // bounds check hint to compiler; see golang.org/issue/14808
		seq := int16(binary.LittleEndian.Uint16(data[:2]))
		if seq == 0 {
			b.entries[i].seq = uint64(seq)
		} else {
			b.entries[i].seq = b.baseSeq + uint64(seq) - entriesPerIndexBlock // unmarshal from relative sequence
		}
		b.entries[i].topicSize = binary.LittleEndian.Uint16(data[2:4])
		b.entries[i].valueSize = binary.LittleEndian.Uint32(data[4:8])
		b.entries[i].msgOffset = int64(binary.LittleEndian.Uint64(data[8:16]))
		data = data[16:]
	}
	b.next = binary.LittleEndian.Uint32(data[:4])
	b.entryIdx = binary.LittleEndian.Uint16(data[4:6])
	return nil
}

func (bh *blockHandle) read() error {
	buf, err := bh.file.Slice(bh.offset, bh.offset+int64(blockSize))
	if err != nil {
		return err
	}
	return bh.UnmarshalBinary(buf)
}
