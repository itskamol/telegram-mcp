package fsguard

import (
	"os"
	"path/filepath"
	"regexp"
)

// partName matches the temp files WriteNew and WriteNewAt create.
var partName = regexp.MustCompile(`^\.telegram-mcp-[0-9]+\.part$`)

// SweepParts removes the temp files a killed server left behind, given the
// paths its journal recorded. A path is removed only if it is absolute,
// named like our temp files, a regular file (not a symlink), and inside one
// of roots after resolving the directory's symlinks. It returns the paths
// it removed; anything else is left alone.
func SweepParts(roots, paths []string) []string {
	var removed []string
	for _, p := range paths {
		if !filepath.IsAbs(p) || !partName.MatchString(filepath.Base(p)) {
			continue
		}
		fi, err := os.Lstat(p)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		dir, err := filepath.EvalSymlinks(filepath.Dir(p))
		if err != nil || !insideRoots(roots, dir) {
			continue
		}
		if os.Remove(filepath.Join(dir, filepath.Base(p))) == nil {
			removed = append(removed, p)
		}
	}
	return removed
}

func insideRoots(roots []string, dir string) bool {
	for _, root := range roots {
		if rr, err := filepath.EvalSymlinks(root); err == nil && within(rr, dir) {
			return true
		}
	}
	return false
}
