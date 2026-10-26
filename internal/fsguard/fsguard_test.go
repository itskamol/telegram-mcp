package fsguard

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSafeName(t *testing.T) {
	cases := map[string]string{
		"report.pdf":       "report.pdf",
		"../../etc/passwd": "_.._etc_passwd",
		"..":               "fb",
		".bashrc":          "bashrc",
		`C:\Windows\x.exe`: "C__Windows_x.exe",
		"a\x00b\nc.txt":    "a_b_c.txt",
		"what?.txt":        "what_.txt",
		"trailing. ":       "trailing",
		"":                 "fb",
		"rasm 📷.jpg":       "rasm 📷.jpg",
	}
	for in, want := range cases {
		if got := SafeName(in, "fb"); got != want {
			t.Errorf("SafeName(%q) = %q, want %q", in, got, want)
		}
	}
	long := SafeName(strings.Repeat("я", 200)+".jpg", "fb")
	if len(long) > maxNameBytes || !strings.HasSuffix(long, ".jpg") || !utf8.ValidString(long) {
		t.Errorf("long name: %d bytes %q", len(long), long)
	}
}

func TestResolveDir(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	roots := []string{root}

	for _, ok := range []string{"", "sub", filepath.Join(root, "sub")} {
		if _, err := ResolveDir(roots, ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"..", "../x", "sub/../..", outside, "escape", "missing"} {
		if _, err := ResolveDir(roots, bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := ResolveDir(nil, ""); err == nil {
		t.Error("no roots accepted")
	}

	fresh := filepath.Join(t.TempDir(), "new", "root")
	if got, err := ResolveDir([]string{fresh}, ""); err != nil || !strings.HasSuffix(got, filepath.Join("new", "root")) {
		t.Errorf("default root not created: %q %v", got, err)
	}
}

func TestWriteNewNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) func(io.Writer) error {
		return func(w io.Writer) error { _, err := io.WriteString(w, s); return err }
	}
	p1, err := WriteNew(dir, "a.txt", 100, write("one"))
	if err != nil {
		t.Fatal(err)
	}
	p2, err := WriteNew(dir, "a.txt", 100, write("two"))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(p2) != "a (1).txt" {
		t.Fatalf("second name = %s", p2)
	}
	if b, _ := os.ReadFile(p1); string(b) != "one" {
		t.Fatalf("first file overwritten: %q", b)
	}

	_, err = WriteNew(dir, "big.txt", 3, write("toolong"))
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("limit: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("leftover files after failed write: %v", entries)
	}
}
