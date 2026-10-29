package mcpserver

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

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

	mu      sync.Mutex
	fetches int
	getFile int
}

func (f *fakeTelegram) Invoke(_ context.Context, in bin.Encoder, out bin.Decoder) error {
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
		f.mu.Unlock()
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
	log, err := audit.Open(filepath.Join(t.TempDir(), "audit.log"), "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	svc := &tele.Service{API: tg.NewClient(home), Pools: onePool{far}, SelfID: testSelf}
	t.Cleanup(func() { svc.Close() })
	return &Server{svc: svc, audit: log,
		policy: Policy{AllowedRoots: []string{dir}, MaxDownload: 10 << 20}}, dir
}

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
