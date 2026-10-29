// Package selfupdate replaces the running binary with a release from
// GitHub, verified against the release's checksums.txt: the same source
// and the same check as install.sh.
package selfupdate

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"golang.org/x/mod/semver"
)

// binName is the binary inside a release archive.
const binName = "telegram-mcp"

// projects are the names release archives have carried: telegram-mcp
// from v0.9.0 on, telegram-mcp-go before the repository was renamed.
var projects = []string{"telegram-mcp", "telegram-mcp-go"}

// maxDownload bounds an archive or checksums.txt; a release archive is
// about 10 MB.
const maxDownload = 100 << 20

// Source is where releases come from.
type Source struct {
	Repo     string // e.g. https://github.com/itskamol/telegram-mcp
	HTTP     *http.Client
	OS, Arch string
}

// Latest returns the tag of the latest release. GitHub redirects
// /releases/latest to /releases/tag/<tag>, so no API token is needed.
func (s Source) Latest(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, s.Repo+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("find the latest release: %w", err)
	}
	resp.Body.Close()
	tag := path.Base(resp.Request.URL.Path)
	if resp.StatusCode != http.StatusOK || !semver.IsValid(tag) {
		return "", fmt.Errorf("find the latest release: got %s at %s", resp.Status, resp.Request.URL)
	}
	return tag, nil
}

// Fetch downloads the release archive for s.OS/s.Arch, checks it against
// checksums.txt and returns the binary inside it.
func (s Source) Fetch(ctx context.Context, version string) ([]byte, error) {
	base := s.Repo + "/releases/download/" + version + "/"
	sums, err := s.get(ctx, base+"checksums.txt")
	if err != nil {
		return nil, err
	}
	name, want := "", ""
	for _, p := range projects {
		n := fmt.Sprintf("%s_%s_%s_%s.tar.gz", p, strings.TrimPrefix(version, "v"), s.OS, s.Arch)
		if sum, ok := lookup(sums, n); ok {
			name, want = n, sum
			break
		}
	}
	if name == "" {
		return nil, fmt.Errorf("release %s has no archive for %s/%s", version, s.OS, s.Arch)
	}

	archive, err := s.get(ctx, base+name)
	if err != nil {
		return nil, err
	}
	if got := sha256.Sum256(archive); hex.EncodeToString(got[:]) != want {
		return nil, fmt.Errorf("checksum mismatch for %s", name)
	}
	return extract(archive)
}

func (s Source) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	if len(b) > maxDownload {
		return nil, fmt.Errorf("download %s: larger than %d MB", url, maxDownload>>20)
	}
	return b, nil
}

// lookup finds name's hash in a checksums.txt ("<sha256>  <name>" lines).
func lookup(sums []byte, name string) (string, bool) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) == 2 && f[1] == name {
			return strings.ToLower(f[0]), true
		}
	}
	return "", false
}

// extract returns the telegram-mcp file from a .tar.gz.
func extract(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("read archive: %w", err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("no %s in the archive", binName)
		}
		if err != nil {
			return nil, fmt.Errorf("read archive: %w", err)
		}
		if h.Typeflag == tar.TypeReg && h.Name == binName {
			return io.ReadAll(io.LimitReader(tr, maxDownload))
		}
	}
}

// Newer reports whether latest is a newer release than current. A build
// without a release version ("dev") cannot be compared.
func Newer(current, latest string) (bool, error) {
	cur := "v" + strings.TrimPrefix(current, "v")
	if !semver.IsValid(cur) {
		return false, fmt.Errorf("version %q is not a release; built from source", current)
	}
	return semver.Compare(latest, cur) > 0, nil
}

// Brew reports whether exe was installed by Homebrew, which must then
// update it: replacing the file would leave brew's records wrong.
func Brew(exe string) bool {
	return strings.Contains(exe, "/Caskroom/") || strings.Contains(exe, "/Cellar/")
}

// Replace puts bin in place of exe: written to a temp file in the same
// directory, then renamed over exe. A running process keeps its old file,
// and a failure leaves exe as it was.
func Replace(exe string, bin []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(exe), "."+filepath.Base(exe)+"-*.new")
	if err != nil {
		return fmt.Errorf("write next to %s: %w", exe, err)
	}
	defer os.Remove(tmp.Name()) // no-op once renamed
	_, err = tmp.Write(bin)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o755)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), exe)
	}
	if err != nil {
		return fmt.Errorf("replace %s: %w", exe, err)
	}
	return nil
}
