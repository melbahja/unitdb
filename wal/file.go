package wal

import (
	"encoding"
	"os"

	"github.com/unit-io/tracedb/fs"
)

type file struct {
	fs.FileManager
	fb         freeBlock
	size       int64
	targetSize int64
}

func openFile(name string, targetSize int64) (file, error) {
	fileFlag := os.O_CREATE | os.O_RDWR
	fileMode := os.FileMode(0666)
	fs := fs.FileIO

	fi, err := fs.OpenFile(name, fileFlag, fileMode)
	f := file{}
	if err != nil {
		return f, err
	}
	f.FileManager = fi

	stat, err := fi.Stat()
	if err != nil {
		return f, err
	}
	f.size = stat.Size()
	f.targetSize = targetSize

	return f, err
}

func (f *file) allocate(size uint32) (int64, error) {
	if size == 0 {
		panic("unable to allocate zero bytes")
	}
	// do not allocate freeblocks until target size has reached for the log to avoid fragmentation
	if f.targetSize > (f.size+int64(size)) || (f.targetSize < (f.size+int64(size)) && f.fb.size < int64(size)) {
		off := f.size
		if err := f.Truncate(off + int64(size)); err != nil {
			return 0, err
		}
		f.size += int64(size)
		return off, nil
	}
	off := f.fb.offset
	f.fb.size -= int64(size)
	f.fb.offset += int64(size)
	return off, nil
}

func (f *file) append(data []byte) error {
	off := f.size
	if _, err := f.WriteAt(data, off); err != nil {
		return err
	}
	f.size += int64(len(data))
	return nil
}

func (f *file) readRaw(off, size int64) ([]byte, error) {
	return f.Slice(off, off+size)
}

func (f *file) writeMarshalableAt(m encoding.BinaryMarshaler, off int64) error {
	buf, err := m.MarshalBinary()
	if err != nil {
		return err
	}
	_, err = f.WriteAt(buf, off)
	return err
}

func (f *file) readUnmarshalableAt(m encoding.BinaryUnmarshaler, size uint32, off int64) error {
	buf := make([]byte, size)
	if _, err := f.ReadAt(buf, off); err != nil {
		return err
	}
	return m.UnmarshalBinary(buf)
}
