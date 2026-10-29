package fsguard

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The writer reports its temp file while writing: the path the crash
// journal records.
func TestPartWriterTempPath(t *testing.T) {
	dir := t.TempDir()
	var tmp string
	var existed bool
	_, err := WriteNewAt(dir, "f.bin", 1, testLimit, testPart, func(w *PartWriter) error {
		tmp = w.TempPath()
		_, err := os.Stat(tmp)
		existed = err == nil
		return writePart(w, content(1), 0)
	})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(tmp) != dir || !partName.MatchString(filepath.Base(tmp)) || !existed {
		t.Errorf("TempPath = %q (existed %v), want a .telegram-mcp-*.part in %s", tmp, existed, dir)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Errorf("temp file still there after WriteNewAt: %v", err)
	}
}

// After a crash, the startup sweep removes the recorded partial files, and
// nothing else: not a recorded path with another name, not one outside the
// roots, not a symlink, wherever it points.
func TestSweepParts(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	mk := func(p string) string {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	partInRoot := mk(filepath.Join(root, ".telegram-mcp-1.part"))
	partInSub := mk(filepath.Join(sub, ".telegram-mcp-2.part"))
	otherName := mk(filepath.Join(root, "report.pdf"))
	partOutside := mk(filepath.Join(outside, ".telegram-mcp-3.part"))
	target := mk(filepath.Join(outside, "precious.txt"))
	link := filepath.Join(root, ".telegram-mcp-4.part")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(root, ".telegram-mcp-5.part") // already removed

	removed := SweepParts([]string{root}, []string{partInRoot, partInSub, otherName, partOutside, link, gone, "relative/.telegram-mcp-6.part"})
	slices.Sort(removed)
	want := []string{partInRoot, partInSub}
	slices.Sort(want)
	if !slices.Equal(removed, want) {
		t.Errorf("removed %v, want %v", removed, want)
	}
	for _, p := range []string{otherName, partOutside, target, link} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s was touched: %v", p, err)
		}
	}
	for _, p := range want {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s still there", p)
		}
	}
}
