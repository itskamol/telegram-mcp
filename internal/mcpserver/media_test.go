package mcpserver

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/itskamol/telegram-mcp-go/internal/audit"
	"github.com/itskamol/telegram-mcp-go/internal/tele"
)

const (
	testSelf   = 1
	testHomeDC = 2
	testFileDC = 4
)

// fakeTelegram answers messages.getMessages with message 5 in Saved
// Messages, its media taken in turn from media (the last one repeats), and
// upload.getFile from data, refusing the expired file reference. It serves
// both as the home connection and as a DC pool.
type fakeTelegram struct {
	media   []tg.MessageMediaClass
	data    []byte
	expired []byte
	gate    chan struct{} // if set, getFile waits until it is closed
	fail    error         // if set, the next getFile fails with it, once

	mu      sync.Mutex
	fetches int
	getFile int
}

func (f *fakeTelegram) Invoke(ctx context.Context, in bin.Encoder, out bin.Decoder) error {
	var reply bin.Encoder
	switch req := in.(type) {
	case *tg.MessagesGetMessagesRequest:
		f.mu.Lock()
		m := f.media[min(f.fetches, len(f.media)-1)]
		f.fetches++
		f.mu.Unlock()
		reply = &tg.MessagesMessages{Messages: []tg.MessageClass{
			&tg.Message{ID: 5, PeerID: &tg.PeerUser{UserID: testSelf}, Media: m},
		}}
	case *tg.UploadGetFileRequest:
		f.mu.Lock()
		f.getFile++
		fail := f.fail
		f.fail = nil
		f.mu.Unlock()
		if fail != nil {
			return fail
		}
		if f.gate != nil {
			select {
			case <-f.gate:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if ref := fileRef(req.Location); f.expired != nil && bytes.Equal(ref, f.expired) {
			return tgerr.New(400, "FILE_REFERENCE_EXPIRED")
		}
		start := min(req.Offset, int64(len(f.data)))
		end := min(start+int64(req.Limit), int64(len(f.data)))
		reply = &tg.UploadFile{Type: &tg.StorageFilePartial{}, Bytes: f.data[start:end]}
	default:
		return fmt.Errorf("fakeTelegram: unexpected %T", in)
	}
	var b bin.Buffer
	if err := reply.Encode(&b); err != nil {
		return err
	}
	return out.Decode(&b)
}

func (f *fakeTelegram) Close() error { return nil }

func (f *fakeTelegram) counts() (fetches, getFile int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fetches, f.getFile
}

func fileRef(l tg.InputFileLocationClass) []byte {
	switch l := l.(type) {
	case *tg.InputDocumentFileLocation:
		return l.FileReference
	case *tg.InputPhotoFileLocation:
		return l.FileReference
	}
	return nil
}

// onePool opens only testFileDC, always to the same fake.
type onePool struct{ dc *fakeTelegram }

func (p onePool) DC(_ context.Context, dc int, _ int64) (telegram.CloseInvoker, error) {
	if dc != testFileDC {
		return nil, fmt.Errorf("onePool: no DC %d", dc)
	}
	return p.dc, nil
}

func (onePool) Config() tg.Config { return tg.Config{ThisDC: testHomeDC} }

func mediaServer(t *testing.T, home, far *fakeTelegram) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "audit.log")
	log, err := audit.Open(logPath, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	svc := &tele.Service{API: tg.NewClient(home), Pools: onePool{far}, SelfID: testSelf}
	t.Cleanup(func() { svc.Close() })
	dl := NewDownloads(context.Background())
	t.Cleanup(dl.Shutdown)
	s := &Server{svc: svc, audit: log, downloads: dl,
		policy: Policy{AllowedRoots: []string{dir}, MaxDownload: 10 << 20}}
	auditPaths.Store(s, logPath)
	return s, dir
}

// auditPaths maps a test server to its audit file.
var auditPaths sync.Map

func fileBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

// download_media fetches a document on another DC from that DC, never
// through the home connection, and when its reference has expired it
// fetches the message again through the handler and finishes.
func TestDownloadMediaOnFileDC(t *testing.T) {
	data := fileBytes(2<<20 + 512<<10)
	doc := func(ref string) tg.MessageMediaClass {
		return &tg.MessageMediaDocument{Document: &tg.Document{
			ID: 9, AccessHash: 1, FileReference: []byte(ref), DCID: testFileDC, Size: int64(len(data)),
			MimeType:   "application/octet-stream",
			Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: "f.bin"}},
		}}
	}
	home := &fakeTelegram{media: []tg.MessageMediaClass{doc("old"), doc("new")}}
	far := &fakeTelegram{data: data, expired: []byte("old")}
	s, dir := mediaServer(t, home, far)

	_, out, err := s.downloadMedia(context.Background(), nil, downloadIn{Chat: "me", MessageID: 5})
	if err != nil {
		t.Fatalf("download_media: %v", err)
	}
	real, _ := filepath.EvalSymlinks(dir) // a temp dir may sit behind a symlink (macOS)
	if out.Path != filepath.Join(real, "f.bin") || out.Size != int64(len(data)) {
		t.Errorf("out = %+v", out)
	}
	if got, _ := os.ReadFile(out.Path); !bytes.Equal(got, data) {
		t.Errorf("file differs: %d bytes, want %d", len(got), len(data))
	}
	if fetches, getFile := home.counts(); getFile != 0 || fetches != 2 {
		t.Errorf("home: %d getFile (want 0), %d message fetches (want 2: first and refetch)", getFile, fetches)
	}
}

// view_photo also fetches from the photo's DC, sequentially into memory.
func TestViewPhotoOnFileDC(t *testing.T) {
	img := fileBytes(3000)
	photo := &tg.MessageMediaPhoto{Photo: &tg.Photo{
		ID: 3, AccessHash: 1, FileReference: []byte("r"), DCID: testFileDC,
		Sizes: []tg.PhotoSizeClass{&tg.PhotoSize{Type: "x", W: 800, H: 600, Size: len(img)}},
	}}
	home := &fakeTelegram{media: []tg.MessageMediaClass{photo}}
	far := &fakeTelegram{data: img}
	s, _ := mediaServer(t, home, far)

	res, _, err := s.viewPhoto(context.Background(), nil, mediaIn{Chat: "me", MessageID: 5})
	if err != nil {
		t.Fatalf("view_photo: %v", err)
	}
	if ic, ok := res.Content[0].(*mcp.ImageContent); !ok || !bytes.Equal(ic.Data, img) {
		t.Errorf("image content differs")
	}
	if _, getFile := home.counts(); getFile != 0 {
		t.Errorf("home got %d getFile, want 0", getFile)
	}
}

// background is a document on another DC whose parts are held back until
// the test opens the gate.
func background(t *testing.T) (s *Server, dir string, data []byte, far *fakeTelegram) {
	t.Helper()
	data = fileBytes(3 << 20)
	doc := &tg.MessageMediaDocument{Document: &tg.Document{
		ID: 9, AccessHash: 1, FileReference: []byte("r"), DCID: testFileDC, Size: int64(len(data)),
		MimeType:   "application/octet-stream",
		Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: "big.bin"}},
	}}
	home := &fakeTelegram{media: []tg.MessageMediaClass{doc}}
	far = &fakeTelegram{data: data, gate: make(chan struct{})}
	s, dir = mediaServer(t, home, far)
	s.downloads.wait = 50 * time.Millisecond
	return s, dir, data, far
}

func auditLines(t *testing.T, s *Server, tool string) []string {
	t.Helper()
	path, _ := auditPaths.Load(s)
	b, err := os.ReadFile(path.(string))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.Contains(l, `"tool":"`+tool+`"`) {
			out = append(out, l)
		}
	}
	return out
}

// A download that outlasts the wait returns status downloading and goes
// on in the server; calling again does not start a second one, and once it
// is done the same call returns the path. One audit line, at the end.
func TestDownloadMediaInBackground(t *testing.T) {
	s, dir, data, far := background(t)
	call := func() downloadOut {
		t.Helper()
		_, out, err := s.downloadMedia(context.Background(), nil, downloadIn{Chat: "me", MessageID: 5})
		if err != nil {
			t.Fatalf("download_media: %v", err)
		}
		return out
	}
	real, _ := filepath.EvalSymlinks(dir)

	first := call()
	if first.Status != "downloading" || first.Size != int64(len(data)) || first.Path != filepath.Join(real, "big.bin") {
		t.Fatalf("first call = %+v, want downloading with planned path and size", first)
	}
	if second := call(); second.Status != "downloading" {
		t.Fatalf("second call = %+v, want still downloading", second)
	}
	if n := len(auditLines(t, s, "download_media")); n != 0 {
		t.Errorf("%d audit lines while running, want 0", n)
	}

	close(far.gate)
	var done downloadOut
	for range 100 {
		if done = call(); done.Status == "done" {
			break
		}
		time.Sleep(20 * time.Millisecond) // polling the tool, as a client would
	}
	if done.Status != "done" || done.Path != filepath.Join(real, "big.bin") {
		t.Fatalf("after release = %+v, want done", done)
	}
	if got, _ := os.ReadFile(done.Path); !bytes.Equal(got, data) {
		t.Error("file differs")
	}
	// 3 parts, plus at most 8 requests past the end: one download, not two.
	if _, getFile := far.counts(); getFile > 3+8 {
		t.Errorf("%d getFile requests: more than one download ran", getFile)
	}
	if n := len(auditLines(t, s, "download_media")); n != 1 {
		t.Errorf("%d audit lines, want 1", n)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("dir has %d entries, want only big.bin", len(entries))
	}
}

// A call that gives up only stops waiting: the job goes on and the next
// call finds it.
func TestDownloadMediaCallCancelKeepsJob(t *testing.T) {
	s, _, _, far := background(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.downloadMedia(ctx, nil, downloadIn{Chat: "me", MessageID: 5})

	_, out, err := s.downloadMedia(context.Background(), nil, downloadIn{Chat: "me", MessageID: 5})
	if err != nil || out.Status != "downloading" {
		t.Fatalf("after a cancelled call: %+v %v, want the job still downloading", out, err)
	}
	close(far.gate)
}

// Stopping the server cancels the job: no file is linked, no .part is
// left, and the job writes one audit line with its error.
func TestDownloadMediaShutdownCancels(t *testing.T) {
	s, dir, _, _ := background(t)
	if _, out, err := s.downloadMedia(context.Background(), nil, downloadIn{Chat: "me", MessageID: 5}); err != nil || out.Status != "downloading" {
		t.Fatalf("start: %+v %v", out, err)
	}
	s.downloads.Shutdown()
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("dir has %d entries after Shutdown, want none", len(entries))
	}
	lines := auditLines(t, s, "download_media")
	if len(lines) != 1 || !strings.Contains(lines[0], `"error"`) {
		t.Errorf("audit = %v, want one line with an error", lines)
	}
}

// A failed download reports its error once; the next call downloads again
// instead of getting the old error for the rest of the result TTL.
func TestDownloadMediaRetryAfterFailure(t *testing.T) {
	s, _, data, far := background(t)
	far.fail = tgerr.New(400, "FILE_ID_INVALID")
	close(far.gate)
	_, out, err := s.downloadMedia(context.Background(), nil, downloadIn{Chat: "me", MessageID: 5})
	if err == nil {
		t.Fatalf("first call = %+v, want the error", out)
	}
	_, out, err = s.downloadMedia(context.Background(), nil, downloadIn{Chat: "me", MessageID: 5})
	if err != nil || out.Status != "done" || out.Size != int64(len(data)) {
		t.Fatalf("retry = %+v %v, want done", out, err)
	}
	if n := len(auditLines(t, s, "download_media")); n != 2 {
		t.Errorf("%d audit lines, want 2 (the failure and the retry)", n)
	}
}

// fakeJournal records the partial files a download announces. Like the
// database behind the real one, it refuses a cancelled context.
type fakeJournal struct {
	mu   sync.Mutex
	open map[string]bool
	adds int
}

func (j *fakeJournal) Add(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.open == nil {
		j.open = map[string]bool{}
	}
	j.open[path] = true
	j.adds++
	return nil
}

func (j *fakeJournal) Remove(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	delete(j.open, path)
	return nil
}

func (j *fakeJournal) paths() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []string
	for p := range j.open {
		out = append(out, p)
	}
	return out
}

// While a download runs its temp file is in the journal, so a crash can be
// cleaned up at the next start; when it ends, by success or by shutdown,
// the file is gone and so is the entry.
func TestDownloadMediaJournalsPartialFile(t *testing.T) {
	for _, end := range []string{"done", "shutdown"} {
		t.Run(end, func(t *testing.T) {
			s, dir, _, far := background(t)
			jr := &fakeJournal{}
			s.downloads.Journal = jr
			if _, out, err := s.downloadMedia(context.Background(), nil, downloadIn{Chat: "me", MessageID: 5}); err != nil || out.Status != "downloading" {
				t.Fatalf("start: %+v %v", out, err)
			}
			open := jr.paths()
			if len(open) != 1 || filepath.Dir(open[0]) != filepath.Dir(filepath.Join(evalDir(t, dir), "x")) ||
				!strings.HasPrefix(filepath.Base(open[0]), ".telegram-mcp-") {
				t.Fatalf("journal while running = %v, want the one .part in %s", open, dir)
			}
			if _, err := os.Stat(open[0]); err != nil {
				t.Fatalf("journaled file is not there: %v", err)
			}

			if end == "done" {
				close(far.gate)
				for range 100 {
					if _, out, _ := s.downloadMedia(context.Background(), nil, downloadIn{Chat: "me", MessageID: 5}); out.Status == "done" {
						break
					}
					time.Sleep(20 * time.Millisecond)
				}
			} else {
				s.downloads.Shutdown()
			}
			if left := jr.paths(); len(left) != 0 {
				t.Errorf("journal after %s = %v, want empty", end, left)
			}
			if _, err := os.Stat(open[0]); !os.IsNotExist(err) {
				t.Errorf("temp file after %s: %v", end, err)
			}
		})
	}
}

func evalDir(t *testing.T, dir string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return real
}
