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
	upGate  chan struct{} // if set, upload parts wait until it is closed
	sendErr error         // if set, the next messages.sendMedia fails with it, once

	mu        sync.Mutex
	fetches   int
	getFile   int
	partBytes int64 // upload bytes received
	sends     int   // messages.sendMedia that succeeded
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
	case *tg.UploadSaveFilePartRequest, *tg.UploadSaveBigFilePartRequest:
		if f.upGate != nil {
			select {
			case <-f.upGate:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		n := 0
		switch r := req.(type) {
		case *tg.UploadSaveFilePartRequest:
			n = len(r.Bytes)
		case *tg.UploadSaveBigFilePartRequest:
			n = len(r.Bytes)
		}
		f.mu.Lock()
		f.partBytes += int64(n)
		f.mu.Unlock()
		reply = &tg.BoolTrue{}
	case *tg.MessagesSendMediaRequest:
		f.mu.Lock()
		if err := f.sendErr; err != nil {
			f.sendErr = nil
			f.mu.Unlock()
			return err
		}
		f.sends++
		id := 100 + f.sends
		f.mu.Unlock()
		reply = &tg.Updates{Updates: []tg.UpdateClass{&tg.UpdateMessageID{ID: id, RandomID: req.RandomID}}}
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

func (onePool) Pool(int64) (telegram.CloseInvoker, error) {
	return nil, fmt.Errorf("onePool: no home pool")
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
		policy: Policy{AllowedRoots: []string{dir}, MaxDownload: 10 << 20, MaxUpload: 10 << 20}}
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

// A "downloading" answer tells the agent what to do next, in the answer
// itself: call again, or run the CLI that waits, and not poll the file,
// which appears only when complete.
func TestDownloadingAnswerSaysWhatNext(t *testing.T) {
	s, _, _, far := background(t)
	_, out, err := s.downloadMedia(context.Background(), nil, downloadIn{Chat: "me", MessageID: 5})
	if err != nil || out.Status != "downloading" {
		t.Fatalf("first call: %+v %v", out, err)
	}
	for _, want := range []string{"download_media", "telegram-mcp download --chat me --message 5", "not poll"} {
		if !strings.Contains(out.Next, want) {
			t.Errorf("next = %q, lacks %q", out.Next, want)
		}
	}
	if out.EtaSeconds != 0 {
		t.Errorf("eta_seconds = %d with nothing downloaded yet, want 0 (unknown)", out.EtaSeconds)
	}
	close(far.gate)
}

func TestETA(t *testing.T) {
	cases := []struct {
		downloaded, size int64
		elapsed          time.Duration
		want             int64
	}{
		{50, 100, 10 * time.Second, 10},
		{25, 100, 10 * time.Second, 30},
		{0, 100, 10 * time.Second, 0},        // no speed yet
		{50, 100, 0, 0},                      // no time yet
		{100, 100, 10 * time.Second, 0},      // done
		{1, 100, 100 * time.Millisecond, 10}, // rounds up, never 0 while running
	}
	for _, c := range cases {
		if got := etaSeconds(c.downloaded, c.size, c.elapsed); got != c.want {
			t.Errorf("etaSeconds(%d, %d, %s) = %d, want %d", c.downloaded, c.size, c.elapsed, got, c.want)
		}
	}
}

// The tool description points shell-capable agents at the CLI.
func TestDownloadDescriptionMentionsCLI(t *testing.T) {
	cs := connect(t, Policy{AllowedRoots: []string{t.TempDir()}, MaxDownload: 1 << 20})
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		if tool.Name == "download_media" {
			for _, want := range []string{"telegram-mcp download", "do not poll"} {
				if !strings.Contains(strings.ToLower(tool.Description), want) {
					t.Errorf("description lacks %q: %s", want, tool.Description)
				}
			}
			return
		}
	}
	t.Fatal("no download_media tool")
}

func (f *fakeTelegram) uploads() (bytes int64, sends int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.partBytes, f.sends
}

// uploadServer is a server whose uploads go over the home connection (the
// test pools have no home pool), with a 3 MB file "up.bin" in its root.
func uploadServer(t *testing.T) (*Server, *fakeTelegram, string) {
	t.Helper()
	home := &fakeTelegram{media: []tg.MessageMediaClass{&tg.MessageMediaEmpty{}}}
	s, dir := mediaServer(t, home, &fakeTelegram{})
	s.downloads.wait = 50 * time.Millisecond
	if err := os.WriteFile(filepath.Join(dir, "up.bin"), fileBytes(3<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	return s, home, dir
}

func sendUp(t *testing.T, s *Server, key string) (sentOut, error) {
	t.Helper()
	_, out, err := s.sendFile(context.Background(), nil, sendFileIn{Chat: "me", Path: "up.bin", AsDocument: true, IdempotencyKey: key})
	return out, err
}

// pollSent calls send_file until it answers "sent".
func pollSent(t *testing.T, s *Server, key string) sentOut {
	t.Helper()
	for range 200 {
		out, err := sendUp(t, s, key)
		if err != nil {
			t.Fatalf("send_file: %v", err)
		}
		if out.Status == "sent" {
			return out
		}
		time.Sleep(10 * time.Millisecond) // polling the tool, as a client would
	}
	t.Fatal("send_file never answered sent")
	return sentOut{}
}

// An upload that outlasts the wait answers "uploading" and goes on in the
// server; the same call again, even without an idempotency_key, is the
// same job and sends nothing twice; once done, it answers "sent". One
// audit line, at the end.
func TestSendFileInBackground(t *testing.T) {
	s, home, _ := uploadServer(t)
	home.upGate = make(chan struct{})

	first, err := sendUp(t, s, "")
	if err != nil || first.Status != "uploading" || first.Size != 3<<20 {
		t.Fatalf("first call = %+v, %v; want uploading with the size", first, err)
	}
	if !strings.Contains(first.Next, "send_file") || !strings.Contains(strings.ToLower(first.Next), "same") {
		t.Errorf("next = %q, want it to say: call send_file again with the same arguments", first.Next)
	}
	if again, err := sendUp(t, s, ""); err != nil || again.Status != "uploading" {
		t.Fatalf("second call = %+v, %v; want still uploading", again, err)
	}
	close(home.upGate)
	done := pollSent(t, s, "")
	if done.MessageID != 101 {
		t.Errorf("message id = %d, want 101", done.MessageID)
	}
	if _, sends := home.uploads(); sends != 1 {
		t.Errorf("%d messages sent, want 1", sends)
	}
	if n := len(auditLines(t, s, "send_file")); n != 1 {
		t.Errorf("%d audit lines, want 1", n)
	}
}

// After a send, the same call within the result TTL returns that message;
// after the TTL it sends again. A different idempotency_key is a new send.
func TestSendFileRepeatAndKeys(t *testing.T) {
	s, home, _ := uploadServer(t)
	now := time.Unix(1000, 0)
	s.downloads.now = func() time.Time { return now }

	a := pollSent(t, s, "")
	if b := pollSent(t, s, ""); b.MessageID != a.MessageID {
		t.Errorf("repeat within the TTL sent %d, want the first message %d", b.MessageID, a.MessageID)
	}
	if _, sends := home.uploads(); sends != 1 {
		t.Fatalf("%d sends after a repeat, want 1", sends)
	}
	now = now.Add(s.downloads.ttl + time.Second)
	pollSent(t, s, "")
	pollSent(t, s, "key-a")
	pollSent(t, s, "key-b")
	if _, sends := home.uploads(); sends != 4 {
		t.Errorf("%d sends, want 4 (first, after the TTL, key-a, key-b)", sends)
	}
}

// A failed send is reported once; the next call sends again.
func TestSendFileRetryAfterFailure(t *testing.T) {
	s, home, _ := uploadServer(t)
	home.sendErr = tgerr.New(400, "CHAT_WRITE_FORBIDDEN")
	if out, err := sendUp(t, s, ""); err == nil {
		t.Fatalf("first call = %+v, want the error", out)
	}
	if out := pollSent(t, s, ""); out.MessageID != 101 {
		t.Errorf("retry = %+v, want message 101", out)
	}
}

// The job sends the file the first call checked and opened: replacing the
// file at that path afterwards changes nothing. A path that is gone is
// refused at once, while the job goes on.
func TestSendFileHoldsTheOpenedFile(t *testing.T) {
	s, home, dir := uploadServer(t)
	home.upGate = make(chan struct{})
	if out, err := sendUp(t, s, "k"); err != nil || out.Status != "uploading" {
		t.Fatalf("first call = %+v, %v", out, err)
	}
	other := filepath.Join(dir, "other.bin")
	if err := os.WriteFile(other, fileBytes(1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(other, filepath.Join(dir, "up.bin")); err != nil { // a new file at the path
		t.Fatal(err)
	}
	close(home.upGate)
	pollSent(t, s, "k")
	if got, _ := home.uploads(); got != 3<<20 {
		t.Errorf("uploaded %d bytes, want the 3 MB file the first call opened", got)
	}

	os.Remove(filepath.Join(dir, "up.bin"))
	if _, err := sendUp(t, s, "k"); err == nil {
		t.Error("a call for a path that is gone was not refused")
	}
}

// Stopping the server cancels an upload: nothing is sent, one audit line
// says why.
func TestSendFileShutdownCancels(t *testing.T) {
	s, home, _ := uploadServer(t)
	home.upGate = make(chan struct{})
	if out, err := sendUp(t, s, ""); err != nil || out.Status != "uploading" {
		t.Fatalf("first call = %+v, %v", out, err)
	}
	s.downloads.Shutdown()
	if _, sends := home.uploads(); sends != 0 {
		t.Errorf("%d messages sent after shutdown, want 0", sends)
	}
	lines := auditLines(t, s, "send_file")
	if len(lines) != 1 || !strings.Contains(lines[0], "server stopped") {
		t.Errorf("audit = %v, want one line saying the server stopped", lines)
	}
}
