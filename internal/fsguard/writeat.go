package fsguard

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
)

// ErrBadPart means a WriteAt did not fit the part layout: misaligned,
// short before the end, past the end, or a part written twice.
var ErrBadPart = errors.New("download part does not fit the file")

// ErrIncomplete means write returned without filling every part.
var ErrIncomplete = errors.New("download is incomplete")

// PartWriter is the io.WriterAt that WriteNewAt hands to write.
//
// A file of the right length can still have a hole: a part that was never
// written reads as zeros in a sparse file. So each part is accepted exactly
// once, at its own offset, and the file is kept only when every part was.
type PartWriter struct {
	f        *os.File
	size     int64
	partSize int64
	written  atomic.Int64

	mu   sync.Mutex
	done []bool // one per part
	err  error  // first refusal; sticky, so write cannot swallow it
}

// WriteAt accepts p only as one whole part: at a multiple of partSize,
// partSize long, or shorter if it ends the file exactly. An empty p is a
// no-op.
func (w *PartWriter) WriteAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	n, end := int64(len(p)), off+int64(len(p))
	if off < 0 || off%w.partSize != 0 || n > w.partSize || end > w.size || (n < w.partSize && end != w.size) {
		return 0, w.refuse(fmt.Errorf("%w: %d bytes at offset %d of %d", ErrBadPart, n, off, w.size))
	}

	i := off / w.partSize
	// Check and mark in one critical section: two writers of the same part
	// must not both pass the check.
	w.mu.Lock()
	dup := w.done[i]
	w.done[i] = true
	w.mu.Unlock()
	if dup {
		return 0, w.refuse(fmt.Errorf("%w: part at offset %d written twice", ErrBadPart, off))
	}

	written, err := w.f.WriteAt(p, off)
	w.written.Add(int64(written))
	if err != nil {
		return written, w.refuse(err)
	}
	return written, nil
}

// Written returns the bytes accepted so far.
func (w *PartWriter) Written() int64 { return w.written.Load() }

// Reset empties the file for a restart from the first part. No WriteAt may
// run at the same time. A refusal stays: it was a bug, not an expired
// reference.
func (w *PartWriter) Reset() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.f.Truncate(0); err != nil {
		return err
	}
	clear(w.done)
	w.written.Store(0)
	return nil
}

func (w *PartWriter) refuse(err error) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err == nil {
		w.err = err
	}
	return err
}

// check reports why the file must not be kept, if it must not.
func (w *PartWriter) check() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	for i, ok := range w.done {
		if !ok {
			return fmt.Errorf("%w: part at offset %d missing", ErrIncomplete, int64(i)*w.partSize)
		}
	}
	if got := w.written.Load(); got != w.size {
		return fmt.Errorf("%w: %d of %d bytes", ErrIncomplete, got, w.size)
	}
	return nil
}

// WriteNewAt is WriteNew for writers that fill the file out of order, in
// parts of partSize bytes. size is the exact expected length and must be
// > 0; a size above limit fails before anything is created. The file is
// kept only if every part was written exactly once.
func WriteNewAt(dir, name string, size, limit, partSize int64, write func(*PartWriter) error) (string, error) {
	if size <= 0 || partSize <= 0 {
		return "", fmt.Errorf("size %d and part size %d must be positive", size, partSize)
	}
	if size > limit {
		return "", ErrTooLarge
	}
	return writeNew(dir, name, func(f *os.File) error {
		w := &PartWriter{f: f, size: size, partSize: partSize, done: make([]bool, (size+partSize-1)/partSize)}
		if err := write(w); err != nil {
			return err
		}
		return w.check()
	})
}
