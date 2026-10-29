package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tarGz returns a .tar.gz holding the given files.
func tarGz(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, data := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		tw.Write(data)
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// release is a fake GitHub: /releases/latest redirects to the tag, and
// /releases/download/<tag>/<file> serves files; checksums lists them
// unless a test overrides it.
type release struct {
	tag       string
	files     map[string][]byte
	checksums string
}

func (r *release) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.URL.Path == "/releases/latest":
			http.Redirect(w, req, "/releases/tag/"+r.tag, http.StatusFound)
		case strings.HasPrefix(req.URL.Path, "/releases/tag/"):
			fmt.Fprint(w, "release page")
		case req.URL.Path == "/releases/download/"+r.tag+"/checksums.txt":
			fmt.Fprint(w, r.checksums)
		case strings.HasPrefix(req.URL.Path, "/releases/download/"+r.tag+"/"):
			data, ok := r.files[strings.TrimPrefix(req.URL.Path, "/releases/download/"+r.tag+"/")]
			if !ok {
				http.NotFound(w, req)
				return
			}
			w.Write(data)
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newRelease builds a release whose archive for linux/amd64, named after
// project, holds bin as telegram-mcp.
func newRelease(t *testing.T, tag, project string, bin []byte) *release {
	t.Helper()
	name := fmt.Sprintf("%s_%s_linux_amd64.tar.gz", project, strings.TrimPrefix(tag, "v"))
	archive := tarGz(t, map[string][]byte{"telegram-mcp": bin, "README.md": []byte("readme")})
	return &release{tag: tag, files: map[string][]byte{name: archive},
		checksums: fmt.Sprintf("%x  %s\n", sha256.Sum256(archive), name)}
}

func source(srv *httptest.Server) Source {
	return Source{Repo: srv.URL, HTTP: srv.Client(), OS: "linux", Arch: "amd64"}
}

func TestLatest(t *testing.T) {
	r := newRelease(t, "v1.2.3", "telegram-mcp", []byte("bin"))
	got, err := source(r.server(t)).Latest(context.Background())
	if err != nil || got != "v1.2.3" {
		t.Errorf("Latest = %q, %v; want v1.2.3", got, err)
	}
}

// The archive is found under the current project name and under the one
// used before the repository was renamed.
func TestFetch(t *testing.T) {
	for _, project := range []string{"telegram-mcp", "telegram-mcp-go"} {
		t.Run(project, func(t *testing.T) {
			r := newRelease(t, "v1.2.3", project, []byte("new binary"))
			got, err := source(r.server(t)).Fetch(context.Background(), "v1.2.3")
			if err != nil || string(got) != "new binary" {
				t.Errorf("Fetch = %q, %v", got, err)
			}
		})
	}
}

// Anything that does not check out is refused, before a byte is written.
func TestFetchRefuses(t *testing.T) {
	cases := map[string]func(r *release){
		"checksum mismatch": func(r *release) {
			r.checksums = strings.Repeat("0", 64) + "  telegram-mcp_1.2.3_linux_amd64.tar.gz\n"
		},
		"no archive for this platform": func(r *release) {
			r.checksums = "abc  telegram-mcp_1.2.3_darwin_arm64.tar.gz\n"
		},
		"archive listed but missing": func(r *release) {
			r.files = map[string][]byte{}
		},
		"no binary in the archive": func(r *release) {
			a := tarGz(t, map[string][]byte{"README.md": []byte("x")})
			r.files = map[string][]byte{"telegram-mcp_1.2.3_linux_amd64.tar.gz": a}
			r.checksums = fmt.Sprintf("%x  telegram-mcp_1.2.3_linux_amd64.tar.gz\n", sha256.Sum256(a))
		},
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRelease(t, "v1.2.3", "telegram-mcp", []byte("bin"))
			breakIt(r)
			if got, err := source(r.server(t)).Fetch(context.Background(), "v1.2.3"); err == nil {
				t.Errorf("Fetch accepted it: %q", got)
			}
		})
	}
}

func TestNewer(t *testing.T) {
	cases := []struct {
		current, latest string
		newer, err      bool
	}{
		{"0.9.1", "v0.9.2", true, false},
		{"0.9.2", "v0.9.2", false, false},
		{"0.10.0", "v0.9.2", false, false}, // numeric, not string order
		{"0.9.2", "v0.10.0", true, false},
		{"dev", "v0.9.2", false, true},
		{"0.9.2-pre.f7a6784", "v0.9.2", true, false},
	}
	for _, c := range cases {
		newer, err := Newer(c.current, c.latest)
		if newer != c.newer || (err != nil) != c.err {
			t.Errorf("Newer(%q, %q) = %v, %v; want %v, error %v", c.current, c.latest, newer, err, c.newer, c.err)
		}
	}
}

func TestBrew(t *testing.T) {
	for path, want := range map[string]bool{
		"/opt/homebrew/Caskroom/telegram-mcp/0.9.2/telegram-mcp":     true,
		"/home/linuxbrew/.linuxbrew/Cellar/telegram-mcp/0.9.2/bin/x": true,
		"/home/kamol/.local/bin/telegram-mcp":                        false,
		"/usr/local/bin/telegram-mcp":                                false,
	} {
		if got := Brew(path); got != want {
			t.Errorf("Brew(%q) = %v, want %v", path, got, want)
		}
	}
}

// The binary is replaced whole, executable, with no temp file left; a
// running copy keeps its old file (rename, not overwrite).
func TestReplace(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "telegram-mcp")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	running, err := os.Open(exe) // like the process image of a running serve
	if err != nil {
		t.Fatal(err)
	}
	defer running.Close()

	if err := Replace(exe, []byte("new binary")); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	got, _ := os.ReadFile(exe)
	fi, _ := os.Stat(exe)
	if string(got) != "new binary" || fi.Mode().Perm() != 0o755 {
		t.Errorf("after Replace: %q mode %v", got, fi.Mode().Perm())
	}
	old := make([]byte, 3)
	running.ReadAt(old, 0)
	if string(old) != "old" {
		t.Errorf("the open copy changed to %q: the file was overwritten in place", old)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("dir has %d entries, want only the binary", len(entries))
	}
}
