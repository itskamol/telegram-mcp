package fsguard

import (
	"bytes"
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
