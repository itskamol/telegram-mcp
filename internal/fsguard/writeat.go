package fsguard

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
)

// ErrBadPart means a WriteAt did not fit the part layout: misaligned,
// short before the end, past the end, or a part written twice.
var ErrBadPart = errors.New("download part does not fit the file")

// ErrIncomplete means write returned without filling every part.
var ErrIncomplete = errors.New("download is incomplete")

// ErrWriteInFlight means Reset was called while a WriteAt was still
// writing: the caller broke the contract that all writes are done.
var ErrWriteInFlight = errors.New("reset while a part is still being written")

// PartWriter is the io.WriterAt that WriteNewAt hands to write.
//
// A file of the right length can still have a hole: a part that was never
// written reads as zeros in a sparse file. So each part is accepted exactly
// once, at its own offset, and the file is kept only when every part was.
type PartWriter struct {
	f        io.WriterAt // the temp file; tests swap it
	size     int64
	partSize int64
	written  atomic.Int64

	mu       sync.Mutex
	done     []bool // one per part
	inFlight int    // parts marked whose disk write has not returned
	err      error  // first refusal; sticky, so write cannot swallow it
}

func newPartWriter(f io.WriterAt, size, partSize int64) *PartWriter {
	return &PartWriter{f: f, size: size, partSize: partSize, done: make([]bool, (size+partSize-1)/partSize)}
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
	// Check, mark and count as in flight in one critical section: two
	// writers of the same part must not both pass, and Reset must see every
	// marked part whose bytes are not on disk yet.
	w.mu.Lock()
	dup := w.done[i]
	w.done[i] = true
	if !dup {
		w.inFlight++
	}
	w.mu.Unlock()
	if dup {
		return 0, w.refuse(fmt.Errorf("%w: part at offset %d written twice", ErrBadPart, off))
	}

	written, err := w.f.WriteAt(p, off)
	w.mu.Lock()
	w.written.Add(int64(written))
	w.inFlight--
	w.mu.Unlock()
	if err != nil {
		return written, w.refuse(err)
	}
	return written, nil
}

// Written returns the bytes accepted so far.
func (w *PartWriter) Written() int64 { return w.written.Load() }

// Reset forgets every part for a restart from the first one. With a write
// still in flight it changes nothing and returns ErrWriteInFlight, so an
// old write can never count toward the new attempt. The file is not
// truncated: the new attempt must rewrite every part anyway. A refusal
// stays: it was a bug, not an expired reference.
func (w *PartWriter) Reset() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.inFlight > 0 {
		return ErrWriteInFlight
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
		w := newPartWriter(f, size, partSize)
		if err := write(w); err != nil {
			return err
		}
		return w.check()
	})
}
