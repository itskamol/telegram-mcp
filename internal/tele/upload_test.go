package tele

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// uploadDC records upload parts and answers messages.sendMedia. If hold is
// set, big parts wait for it to close, announcing themselves on arrived,
// so a test can see how many are in flight at once.
type uploadDC struct {
	hold    chan struct{}
	arrived chan struct{}
	flood   string // the first big part fails with this, once

	mu          sync.Mutex
	big, small  map[int]int // part -> bytes
	inFlight    int
	maxInFlight int
	sends       int
	closed      bool
}

func newUploadDC() *uploadDC { return &uploadDC{big: map[int]int{}, small: map[int]int{}} }

func (d *uploadDC) Invoke(_ context.Context, in bin.Encoder, out bin.Decoder) error {
	var reply bin.Encoder = &tg.BoolTrue{}
	switch req := in.(type) {
	case *tg.UploadSaveBigFilePartRequest:
		d.mu.Lock()
		if d.flood != "" {
			f := d.flood
			d.flood = ""
			d.mu.Unlock()
			return tgerr.New(420, f)
		}
		if _, dup := d.big[req.FilePart]; dup {
			d.mu.Unlock()
			return errors.New("part sent twice")
		}
		d.big[req.FilePart] = len(req.Bytes)
		d.inFlight++
		d.maxInFlight = max(d.maxInFlight, d.inFlight)
		d.mu.Unlock()
		if d.arrived != nil {
			d.arrived <- struct{}{}
		}
		if d.hold != nil {
			<-d.hold
		}
		d.mu.Lock()
		d.inFlight--
		d.mu.Unlock()
	case *tg.UploadSaveFilePartRequest:
		d.mu.Lock()
		d.small[req.FilePart] = len(req.Bytes)
		d.mu.Unlock()
	case *tg.MessagesSendMediaRequest:
		d.mu.Lock()
		d.sends++
		d.mu.Unlock()
		reply = &tg.Updates{Updates: []tg.UpdateClass{&tg.UpdateMessageID{ID: 77, RandomID: req.RandomID}}}
	default:
		return errors.New("uploadDC: unexpected request")
	}
	var b bin.Buffer
	if err := reply.Encode(&b); err != nil {
		return err
	}
	return out.Decode(&b)
}

func (d *uploadDC) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	return nil
}

func (d *uploadDC) parts() (big, small int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.big), len(d.small)
}

func tempFile(t *testing.T, size int) Upload {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f.bin")
	b := make([]byte, size)
	rand.Read(b)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return Upload{File: f, Size: int64(size), Name: "f.bin", MimeType: "application/octet-stream", Uploaded: &atomic.Int64{}}
}

const uploadPart = 512 << 10

// A big file goes to the home pool, 512 KB parts, 8 in flight at once,
// every part once; the primary connection only sends the message.
func TestSendFileBigThroughHomePool(t *testing.T) {
	primary, pool := newUploadDC(), newUploadDC()
	pool.hold, pool.arrived = make(chan struct{}), make(chan struct{}, 64)
	s := &Service{API: tg.NewClient(primary), Pools: &fakePools{this: homeDC, home: pool}}
	up := tempFile(t, 21<<20) // 42 parts of 512 KB

	type result struct {
		id  int
		err error
	}
	done := make(chan result, 1)
	go func() {
		id, err := s.SendFile(context.Background(), Resolved{Input: &tg.InputPeerSelf{}}, up, Formatted{}, 0, 0, 5)
		done <- result{id, err}
	}()
	for i := range downloadConcurrency {
		within(t, pool.arrived, "part "+string(rune('1'+i))+" of 8 in flight at once")
	}
	close(pool.hold)
	r := within(t, done, "SendFile to finish")
	if r.err != nil || r.id != 77 {
		t.Fatalf("SendFile = %d, %v", r.id, r.err)
	}

	big, small := pool.parts()
	if big != 42 || small != 0 {
		t.Errorf("pool got %d big and %d small parts, want 42 big", big, small)
	}
	for part, n := range pool.big {
		if n != uploadPart {
			t.Errorf("part %d is %d bytes, want %d", part, n, uploadPart)
		}
	}
	if pool.maxInFlight != downloadConcurrency {
		t.Errorf("%d parts in flight at most, want %d", pool.maxInFlight, downloadConcurrency)
	}
	if b, sm := primary.parts(); b+sm != 0 || primary.sends != 1 {
		t.Errorf("primary: %d parts, %d sends; want 0 parts, 1 send", b+sm, primary.sends)
	}
	if got := up.Uploaded.Load(); got != up.Size {
		t.Errorf("Uploaded = %d, want %d", got, up.Size)
	}
}

// A small file (10 MB or less) is sent in order, as the protocol needs,
// but through the pool and in 512 KB parts.
func TestSendFileSmallThroughHomePool(t *testing.T) {
	primary, pool := newUploadDC(), newUploadDC()
	s := &Service{API: tg.NewClient(primary), Pools: &fakePools{this: homeDC, home: pool}}
	up := tempFile(t, 3<<20+5) // 7 parts, the last 5 bytes

	if _, err := s.SendFile(context.Background(), Resolved{Input: &tg.InputPeerSelf{}}, up, Formatted{}, 0, 0, 5); err != nil {
		t.Fatal(err)
	}
	if big, small := pool.parts(); big != 0 || small != 7 {
		t.Errorf("pool got %d big and %d small parts, want 7 small", big, small)
	}
	if pool.small[0] != uploadPart || pool.small[6] != 5 {
		t.Errorf("part sizes %d … %d, want %d … 5", pool.small[0], pool.small[6], uploadPart)
	}
	if b, sm := primary.parts(); b+sm != 0 {
		t.Errorf("primary got %d parts, want 0", b+sm)
	}
}

// Without a home pool the upload still works, over the primary connection.
func TestSendFileHomePoolFallback(t *testing.T) {
	primary := newUploadDC()
	s := &Service{API: tg.NewClient(primary), Pools: &fakePools{this: homeDC, poolErr: errors.New("dial failed")}}
	if _, err := s.SendFile(context.Background(), Resolved{Input: &tg.InputPeerSelf{}}, tempFile(t, 11<<20), Formatted{}, 0, 0, 5); err != nil {
		t.Fatal(err)
	}
	if big, _ := primary.parts(); big != 22 {
		t.Errorf("primary got %d big parts, want 22", big)
	}
}

// A flood wait over a minute fails the upload at once instead of being
// slept through, as for downloads.
func TestSendFileLongFloodFailsFast(t *testing.T) {
	primary, pool := newUploadDC(), newUploadDC()
	pool.flood = "FLOOD_WAIT_120"
	s := &Service{API: tg.NewClient(primary), Pools: &fakePools{this: homeDC, home: pool}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := s.SendFile(ctx, Resolved{Input: &tg.InputPeerSelf{}}, tempFile(t, 11<<20), Formatted{}, 0, 0, 5)
	var flood *FloodError
	if !errors.As(err, &flood) || flood.Seconds != 120 {
		t.Errorf("err = %v, want a FloodError of 120 s", err)
	}
	if primary.sends != 0 {
		t.Error("the message was sent after a failed upload")
	}
}
