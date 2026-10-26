// Package fsguard keeps downloads inside the configured allowed roots.
//
// The model chooses directory and file names, and those can come from a
// prompt-injected message, so every path is treated as hostile: names are
// reduced to a single safe component, directories must resolve (symlinks
// included) inside a root, and files are never overwritten.
package fsguard

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxNameBytes keeps names well below every filesystem's 255-byte limit.
const maxNameBytes = 120

// ErrTooLarge means the content exceeded the size limit.
var ErrTooLarge = errors.New("file exceeds the size limit")

// SafeName reduces name to one harmless path component. fallback is used
// when nothing usable is left.
func SafeName(name, fallback string) string {
	name = strings.Map(func(r rune) rune {
		switch {
		case r == '/' || r == '\\' || r == ':' || unicode.IsControl(r):
			return '_'
		case strings.ContainsRune(`<>"|?*`, r): // reserved on Windows
			return '_'
		}
		return r
	}, name)
	name = strings.TrimLeft(strings.TrimSpace(name), ".") // no hidden files, no ".."
	name = strings.TrimRight(name, ". ")                  // Windows strips these
	if name == "" {
		name = fallback
	}
	if len(name) > maxNameBytes {
		ext := filepath.Ext(name)
		if len(ext) > 16 {
			ext = ""
		}
		base := name[:maxNameBytes-len(ext)]
		for !utf8.ValidString(base) { // don't split a multi-byte rune
			base = base[:len(base)-1]
		}
		name = base + ext
	}
	return name
}

// ResolveDir returns the real path of dir, which must lie inside one of
// roots. An empty dir means the first root, which is created if missing.
// A relative dir is taken relative to the first root.
func ResolveDir(roots []string, dir string) (string, error) {
	if len(roots) == 0 {
		return "", errors.New("no allowed_roots configured")
	}
	if dir == "" {
		if err := os.MkdirAll(roots[0], 0o700); err != nil {
			return "", fmt.Errorf("create %s: %w", roots[0], err)
		}
		dir = roots[0]
	} else if !filepath.IsAbs(dir) {
		dir = filepath.Join(roots[0], dir)
	}

	dir = filepath.Clean(dir)
	outside := fmt.Errorf("%s is outside allowed_roots (%s)", dir, strings.Join(roots, ", "))
	if !slices.ContainsFunc(roots, func(r string) bool { return within(r, dir) }) {
		return "", outside // ".." escapes, caught before touching the disk
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("directory %s does not exist inside allowed_roots", dir)
	}
	for _, root := range roots {
		rr, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		if within(rr, real) {
			return real, nil
		}
	}
	return "", outside // a symlink inside a root points out of it
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}

// WriteNew streams content into dir/name without ever replacing an existing
// file: a busy name becomes "name (1).ext", "name (2).ext", and so on.
// Data goes to a temp file first, so a failed or oversized download leaves
// nothing behind. It returns the final path.
func WriteNew(dir, name string, limit int64, write func(io.Writer) error) (string, error) {
	tmp, err := os.CreateTemp(dir, ".telegram-mcp-*.part")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())

	werr := write(&limitWriter{w: tmp, left: limit})
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return "", werr
	}

	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 0; i < 1000; i++ {
		final := filepath.Join(dir, name)
		if i > 0 {
			final = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", base, i, ext))
		}
		// Link fails if final exists, unlike Rename, so nothing is clobbered.
		err := os.Link(tmp.Name(), final)
		if err == nil {
			return final, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
	return "", fmt.Errorf("too many files named %s in %s", name, dir)
}

type limitWriter struct {
	w    io.Writer
	left int64
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > l.left {
		return 0, ErrTooLarge
	}
	n, err := l.w.Write(p)
	l.left -= int64(n)
	return n, err
}

// LimitedBuffer collects up to limit bytes in memory.
type LimitedBuffer struct {
	Bytes []byte
	Limit int
}

func (b *LimitedBuffer) Write(p []byte) (int, error) {
	if len(b.Bytes)+len(p) > b.Limit {
		return 0, ErrTooLarge
	}
	b.Bytes = append(b.Bytes, p...)
	return len(p), nil
}
