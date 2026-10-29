package fsguard

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

const (
	testPart  = 1 << 20
	testLimit = 10 << 20
)

// content returns size bytes whose pattern does not repeat at part
// boundaries, so a part written at the wrong offset changes the file.
func content(size int64) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

// writePart writes part i of data, as the downloader would.
func writePart(w *PartWriter, data []byte, i int) error {
	off := int64(i) * testPart
	end := min(off+testPart, int64(len(data)))
	_, err := w.WriteAt(data[off:end], off)
	return err
}

// assertEmpty fails if dir holds anything, a final file or a .part.
func assertEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("left behind: %s", e.Name())
	}
}

// Every part is written once, last part first, from one goroutine per
// part, so the order and the concurrency are what Parallel produces.
func TestWriteNewAtSizes(t *testing.T) {
	for _, size := range []int64{1, 2 * testPart, 2*testPart + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			dir := t.TempDir()
			data := content(size)
			parts := int((size + testPart - 1) / testPart)

			path, err := WriteNewAt(dir, "f.bin", size, testLimit, testPart, func(w *PartWriter) error {
				var wg sync.WaitGroup
				errs := make([]error, parts)
				for i := parts - 1; i >= 0; i-- {
					wg.Go(func() { errs[i] = writePart(w, data, i) })
				}
				wg.Wait()
				return errors.Join(errs...)
			})
			if err != nil {
				t.Fatalf("WriteNewAt: %v", err)
			}
			if path != filepath.Join(dir, "f.bin") {
				t.Errorf("path = %s", path)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, data) {
				t.Errorf("content differs: %d bytes, want %d", len(got), len(data))
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 1 {
				t.Errorf("dir has %d entries, want only f.bin", len(entries))
			}
		})
	}
}

// A missing middle part leaves a sparse file of the right length: the
// length check alone would keep it.
func TestWriteNewAtHole(t *testing.T) {
	dir := t.TempDir()
	data := content(3 * testPart)
	_, err := WriteNewAt(dir, "f.bin", int64(len(data)), testLimit, testPart, func(w *PartWriter) error {
		return errors.Join(writePart(w, data, 0), writePart(w, data, 2))
	})
	if !errors.Is(err, ErrIncomplete) {
		t.Errorf("err = %v, want ErrIncomplete", err)
	}
	assertEmpty(t, dir)
}

// Only the part that ends the file may be shorter than partSize.
func TestWriteNewAtShortMiddlePart(t *testing.T) {
	dir := t.TempDir()
	data := content(2 * testPart)
	var shortErr error
	_, err := WriteNewAt(dir, "f.bin", int64(len(data)), testLimit, testPart, func(w *PartWriter) error {
		_, shortErr = w.WriteAt(data[:testPart/2], 0)
		// The rest arrives correctly and the short write's error is
		// swallowed: the refusal must still stick.
		return errors.Join(writePart(w, data, 0), writePart(w, data, 1))
	})
	if !errors.Is(shortErr, ErrBadPart) {
		t.Errorf("short WriteAt err = %v, want ErrBadPart", shortErr)
	}
	if !errors.Is(err, ErrBadPart) {
		t.Errorf("err = %v, want ErrBadPart", err)
	}
	assertEmpty(t, dir)
}

// A part written twice is refused, even with different bytes, and the
// refusal sticks when write ignores it.
func TestWriteNewAtDuplicatePart(t *testing.T) {
	dir := t.TempDir()
	data := content(2 * testPart)
	other := bytes.Repeat([]byte{0xff}, testPart)
	var dupErr error
	_, err := WriteNewAt(dir, "f.bin", int64(len(data)), testLimit, testPart, func(w *PartWriter) error {
		if err := writePart(w, data, 0); err != nil {
			return err
		}
		_, dupErr = w.WriteAt(other, 0)
		return writePart(w, data, 1)
	})
	if !errors.Is(dupErr, ErrBadPart) {
		t.Errorf("duplicate WriteAt err = %v, want ErrBadPart", dupErr)
	}
	if !errors.Is(err, ErrBadPart) {
		t.Errorf("err = %v, want ErrBadPart", err)
	}
	assertEmpty(t, dir)
}

// Two goroutines racing for one part: exactly one write is accepted. A
// check-then-mark in two locked steps lets both through; -race cannot see
// that, so the outcome is counted, many times.
func TestWriteNewAtConcurrentDuplicate(t *testing.T) {
	const part = 64
	for range 300 {
		dir := t.TempDir()
		var errs [2]error
		_, err := WriteNewAt(dir, "f.bin", part, testLimit, part, func(w *PartWriter) error {
			start := make(chan struct{})
			var wg sync.WaitGroup
			for g := range errs {
				wg.Go(func() {
					<-start
					_, errs[g] = w.WriteAt(bytes.Repeat([]byte{byte(g)}, part), 0)
				})
			}
			close(start)
			wg.Wait()
			return nil
		})
		ok, bad := 0, 0
		for _, e := range errs {
			switch {
			case e == nil:
				ok++
			case errors.Is(e, ErrBadPart):
				bad++
			}
		}
		if ok != 1 || bad != 1 {
			t.Fatalf("WriteAt results %v, want one nil and one ErrBadPart", errs)
		}
		if !errors.Is(err, ErrBadPart) {
			t.Fatalf("err = %v, want ErrBadPart", err)
		}
		assertEmpty(t, dir)
	}
}

// Writes that a bitmap indexed by off/partSize would count as a valid
// part. In each case the refusal is swallowed and the correct parts follow,
// so only the refusal itself can fail the download.
func TestWriteNewAtRefusesMisfit(t *testing.T) {
	cases := []struct {
		name string
		size int64
		off  int64
		n    int64
	}{
		// Full length but between parts: off/partSize would mark part 0
		// while the first half MB stays a hole.
		{"unaligned", 2 * testPart, testPart / 2, testPart},
		// Full length, aligned, but runs past the end of the file.
		{"past end", 2*testPart + 1, 2 * testPart, testPart},
		// Ends exactly at size but spans two parts: only part 0 would be
		// marked.
		{"two parts in one", 2 * testPart, 0, 2 * testPart},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			data := content(c.size)
			parts := int((c.size + testPart - 1) / testPart)
			var misfitErr error
			_, err := WriteNewAt(dir, "f.bin", c.size, testLimit, testPart, func(w *PartWriter) error {
				_, misfitErr = w.WriteAt(make([]byte, c.n), c.off)
				var errs []error
				for i := range parts {
					errs = append(errs, writePart(w, data, i))
				}
				return errors.Join(errs...)
			})
			if !errors.Is(misfitErr, ErrBadPart) {
				t.Errorf("misfit WriteAt err = %v, want ErrBadPart", misfitErr)
			}
			if !errors.Is(err, ErrBadPart) {
				t.Errorf("err = %v, want ErrBadPart", err)
			}
			assertEmpty(t, dir)
		})
	}
}

// Eight parts plus a short last one, one goroutine per part, started in a
// seeded random order. The seed is logged so a failure can be replayed.
func TestWriteNewAtShuffled(t *testing.T) {
	const size = 8*testPart + 123
	data := content(size)
	parts := int((size + testPart - 1) / testPart)
	for seed := uint64(1); seed <= 20; seed++ {
		dir := t.TempDir()
		order := rand.New(rand.NewPCG(seed, 0)).Perm(parts)
		path, err := WriteNewAt(dir, "f.bin", size, testLimit, testPart, func(w *PartWriter) error {
			var wg sync.WaitGroup
			errs := make([]error, parts)
			for _, i := range order {
				wg.Go(func() { errs[i] = writePart(w, data, i) })
			}
			wg.Wait()
			return errors.Join(errs...)
		})
		if err != nil {
			t.Fatalf("seed %d, order %v: %v", seed, order, err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("seed %d, order %v: content differs", seed, order)
		}
	}
}

// An empty write is a no-op: it is accepted but marks no part.
func TestWriteNewAtEmptyWrite(t *testing.T) {
	t.Run("then all parts", func(t *testing.T) {
		dir := t.TempDir()
		data := content(2 * testPart)
		path, err := WriteNewAt(dir, "f.bin", int64(len(data)), testLimit, testPart, func(w *PartWriter) error {
			if n, err := w.WriteAt(nil, 0); n != 0 || err != nil {
				t.Errorf("empty WriteAt = %d, %v; want 0, nil", n, err)
			}
			if w.Written() != 0 {
				t.Errorf("Written() = %d after an empty write", w.Written())
			}
			return errors.Join(writePart(w, data, 0), writePart(w, data, 1))
		})
		if err != nil {
			t.Fatalf("WriteNewAt: %v", err)
		}
		if got, _ := os.ReadFile(path); !bytes.Equal(got, data) {
			t.Error("content differs")
		}
	})
	t.Run("alone", func(t *testing.T) {
		dir := t.TempDir()
		_, err := WriteNewAt(dir, "f.bin", 1, testLimit, testPart, func(w *PartWriter) error {
			_, err := w.WriteAt(nil, 0)
			return err
		})
		if !errors.Is(err, ErrIncomplete) {
			t.Errorf("err = %v, want ErrIncomplete", err)
		}
		assertEmpty(t, dir)
	})
}

// Reset after some parts empties the file; a full second attempt then
// produces exactly the data, with nothing left of the first.
func TestWriteNewAtReset(t *testing.T) {
	dir := t.TempDir()
	data := content(3 * testPart)
	stale := bytes.Repeat([]byte{0xff}, len(data))
	path, err := WriteNewAt(dir, "f.bin", int64(len(data)), testLimit, testPart, func(w *PartWriter) error {
		if err := errors.Join(writePart(w, stale, 0), writePart(w, stale, 2)); err != nil {
			return err
		}
		if err := w.Reset(); err != nil {
			return err
		}
		if w.Written() != 0 {
			t.Errorf("Written() = %d after Reset", w.Written())
		}
		return errors.Join(writePart(w, data, 0), writePart(w, data, 1), writePart(w, data, 2))
	})
	if err != nil {
		t.Fatalf("WriteNewAt: %v", err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, data) {
		t.Error("content differs: stale bytes survived Reset")
	}
}

// blockingFile holds the write at offset 0 inside the "disk write" until
// released; other writes return at once.
type blockingFile struct {
	entered, release chan struct{}
}

func (b *blockingFile) WriteAt(p []byte, off int64) (int, error) {
	if off == 0 {
		close(b.entered)
		<-b.release
	}
	return len(p), nil
}

// Reset while a write is between marking its part and finishing the disk
// write would let that old write count toward the new attempt. It must
// refuse with ErrWriteInFlight and change nothing; once the write is done,
// Reset works and forgets every part.
func TestPartWriterResetDuringWrite(t *testing.T) {
	const part = 64
	f := &blockingFile{entered: make(chan struct{}), release: make(chan struct{})}
	w := newPartWriter(f, 2*part, part)
	if _, err := w.WriteAt(make([]byte, part), part); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	var werr error
	wg.Go(func() { _, werr = w.WriteAt(make([]byte, part), 0) })
	<-f.entered

	if err := w.Reset(); !errors.Is(err, ErrWriteInFlight) {
		t.Errorf("Reset during a write: err = %v, want ErrWriteInFlight", err)
	}
	if got := w.Written(); got != part {
		t.Errorf("Written() = %d after the refused Reset, want %d", got, part)
	}

	close(f.release)
	wg.Wait()
	if werr != nil {
		t.Fatalf("WriteAt: %v", werr)
	}
	if err := w.Reset(); err != nil {
		t.Fatalf("Reset after the write: %v", err)
	}
	if got := w.Written(); got != 0 {
		t.Errorf("Written() = %d after Reset, want 0", got)
	}
	// Both parts are forgotten: writing them again is not a duplicate.
	f.entered, f.release = make(chan struct{}), make(chan struct{})
	close(f.release)
	for _, off := range []int64{0, part} {
		if _, err := w.WriteAt(make([]byte, part), off); err != nil {
			t.Errorf("WriteAt(%d) after Reset: %v", off, err)
		}
	}
	if err := w.check(); err != nil {
		t.Errorf("check after a full second attempt: %v", err)
	}
}

// When write fails, its error comes back and nothing is left on disk.
func TestWriteNewAtWriteFails(t *testing.T) {
	dir := t.TempDir()
	data := content(2 * testPart)
	boom := errors.New("boom")
	_, err := WriteNewAt(dir, "f.bin", int64(len(data)), testLimit, testPart, func(w *PartWriter) error {
		if err := writePart(w, data, 0); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want boom", err)
	}
	assertEmpty(t, dir)
}

// size is checked against limit before anything exists, and size must be
// positive: 0 is not "unknown" here, callers with an unknown size stream.
func TestWriteNewAtRefusesSize(t *testing.T) {
	for _, c := range []struct {
		name string
		size int64
		want error
	}{
		{"above limit", testLimit + 1, ErrTooLarge},
		{"zero", 0, nil},
		{"negative", -1, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			called := false
			_, err := WriteNewAt(dir, "f.bin", c.size, testLimit, testPart, func(*PartWriter) error {
				called = true
				return nil
			})
			if err == nil || (c.want != nil && !errors.Is(err, c.want)) {
				t.Errorf("err = %v, want %v", err, cmp.Or(c.want, errors.New("an error")))
			}
			if called {
				t.Error("write was called")
			}
			assertEmpty(t, dir)
		})
	}
}

// An existing file is never replaced; the new one gets "name (1)".
func TestWriteNewAtNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "f.bin")
	if err := os.WriteFile(old, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	data := content(1)
	path, err := WriteNewAt(dir, "f.bin", 1, testLimit, testPart, func(w *PartWriter) error {
		return writePart(w, data, 0)
	})
	if err != nil {
		t.Fatalf("WriteNewAt: %v", err)
	}
	if path != filepath.Join(dir, "f (1).bin") {
		t.Errorf("path = %s, want f (1).bin", path)
	}
	if got, _ := os.ReadFile(old); string(got) != "old" {
		t.Errorf("existing file changed to %q", got)
	}
}
