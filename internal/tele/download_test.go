package tele

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

const homeDC = 2

// fileDC serves one file over upload.getFile and records each request.
// Download goroutines call it concurrently. Like Telegram, it refuses a
// limit other than the 1 MB part and an offset that is not a multiple of
// it, and answers past the end with no bytes; so every download test also
// checks the part layout.
type fileDC struct {
	data   []byte
	floods map[int64]string // offset -> flood error type_seconds, answered once
	// expired is a file reference that has expired for parts at or past
	// expiredFrom: they get FILE_REFERENCE_EXPIRED, earlier parts are served.
	expired     []byte
	expiredFrom int64

	mu       sync.Mutex
	requests []*tg.UploadGetFileRequest
	other    []bin.Encoder // anything that is not upload.getFile
	closed   bool
}

func (d *fileDC) Invoke(_ context.Context, in bin.Encoder, out bin.Decoder) error {
	req, ok := in.(*tg.UploadGetFileRequest)
	d.mu.Lock()
	if !ok {
		d.other = append(d.other, in)
		d.mu.Unlock()
		return fmt.Errorf("fileDC: unexpected %T", in)
	}
	d.requests = append(d.requests, req)
	flood, isFlood := d.floods[req.Offset]
	delete(d.floods, req.Offset)
	d.mu.Unlock()
	if isFlood {
		return tgerr.New(420, flood)
	}
	if loc, ok := req.Location.(*tg.InputDocumentFileLocation); ok && d.expired != nil &&
		bytes.Equal(loc.FileReference, d.expired) && req.Offset >= d.expiredFrom {
		return tgerr.New(400, "FILE_REFERENCE_EXPIRED")
	}

	if req.Limit != DownloadPartSize {
		return tgerr.New(400, "LIMIT_INVALID")
	}
	if req.Offset < 0 || req.Offset%DownloadPartSize != 0 {
		return tgerr.New(400, "OFFSET_INVALID")
	}
	start := min(req.Offset, int64(len(d.data)))
	end := min(start+int64(req.Limit), int64(len(d.data)))
	var b bin.Buffer
	if err := (&tg.UploadFile{Type: &tg.StorageFilePartial{}, Bytes: d.data[start:end]}).Encode(&b); err != nil {
		return err
	}
	return out.Decode(&b)
}

func (d *fileDC) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	return nil
}

// offsets returns how many times each offset was requested.
func (d *fileDC) offsets() map[int64]int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := map[int64]int{}
	for _, r := range d.requests {
		n[r.Offset]++
	}
	return n
}

func (d *fileDC) calls() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.requests) + len(d.other)
}

// fakePools hands out one fileDC per DC and records each DC call.
type fakePools struct {
	dcs  map[int]*fileDC
	this int // Config().ThisDC

	mu     sync.Mutex
	opened []int   // dc of each DC call
	max    []int64 // max of each DC call
}

func (p *fakePools) DC(_ context.Context, dc int, max int64) (telegram.CloseInvoker, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.opened = append(p.opened, dc)
	p.max = append(p.max, max)
	d, ok := p.dcs[dc]
	if !ok {
		return nil, fmt.Errorf("fakePools: no DC %d", dc)
	}
	return d, nil
}

func (p *fakePools) Config() tg.Config { return tg.Config{ThisDC: p.this} }

// memSink is an in-memory PartSink. onReset, if set, runs at each Reset
// and its error is Reset's.
type memSink struct {
	mu      sync.Mutex
	buf     []byte
	resets  int
	onReset func() error
}

func (m *memSink) WriteAt(p []byte, off int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if end := int(off) + len(p); end > len(m.buf) {
		m.buf = append(m.buf, make([]byte, end-len(m.buf))...)
	}
	return copy(m.buf[off:], p), nil
}

func (m *memSink) Reset() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resets++
	if m.onReset != nil {
		if err := m.onReset(); err != nil {
			return err
		}
	}
	m.buf = nil
	return nil
}

func fileData(size int) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

func noRefetch(context.Context) (MediaFile, error) {
	return MediaFile{}, fmt.Errorf("unexpected refetch")
}

// A file on another DC is fetched only from that DC's pool, never through
// the home connection, so no request can come back with FILE_MIGRATE. The
// pool is sized to the request concurrency (a pool connection carries one
// request at a time), opened once, and closed by Close. A file on the home
// DC, or on any DC while the home DC is not known yet, uses the home
// connection and opens no pool.
func TestDownloadFileRoutesToFileDC(t *testing.T) {
	ctx := context.Background()
	data := fileData(2<<20 + 512<<10) // three parts, the last one short
	loc := &tg.InputDocumentFileLocation{ID: 1, AccessHash: 2, FileReference: []byte{3}}
	file := func(dc int) MediaFile {
		return MediaFile{Info: MediaInfo{Size: int64(len(data))}, Location: loc, DC: dc}
	}
	download := func(t *testing.T, s *Service, dc int) {
		t.Helper()
		sink := &memSink{}
		if err := s.DownloadFile(ctx, file(dc), sink, noRefetch); err != nil {
			t.Fatalf("DownloadFile: %v", err)
		}
		if !bytes.Equal(sink.buf, data) {
			t.Fatalf("content differs: %d bytes, want %d", len(sink.buf), len(data))
		}
	}

	t.Run("other DC", func(t *testing.T) {
		home, far := &fileDC{data: data}, &fileDC{data: data}
		pools := &fakePools{dcs: map[int]*fileDC{4: far}, this: homeDC}
		s := &Service{API: tg.NewClient(home), Pools: pools}

		download(t, s, 4)
		download(t, s, 4)
		if n := home.calls(); n != 0 {
			t.Errorf("home connection got %d requests, want 0", n)
		}
		if len(pools.opened) != 1 || pools.opened[0] != 4 {
			t.Errorf("pools opened %v, want [4] once for two downloads", pools.opened)
		}
		if len(pools.max) != 1 || pools.max[0] != downloadConcurrency {
			t.Errorf("pool max %v, want [%d]", pools.max, downloadConcurrency)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if !far.closed {
			t.Error("Close did not close the DC pool")
		}
	})

	for _, c := range []struct {
		name     string
		this, dc int
	}{
		{"home DC", homeDC, homeDC},
		{"home DC not known yet", 0, 4},
	} {
		t.Run(c.name, func(t *testing.T) {
			home := &fileDC{data: data}
			pools := &fakePools{dcs: map[int]*fileDC{4: {data: data}}, this: c.this}
			s := &Service{API: tg.NewClient(home), Pools: pools}

			download(t, s, c.dc)
			if len(pools.opened) != 0 {
				t.Errorf("pools opened %v, want none", pools.opened)
			}
			if home.calls() == 0 {
				t.Error("home connection got no requests")
			}
		})
	}
}

// Each finished download logs one line for the measurement plan: where it
// came from and how long it took, nothing about the file or the chat.
func TestDownloadFileLogsTiming(t *testing.T) {
	data := fileData(1500)
	var buf bytes.Buffer
	s := &Service{API: tg.NewClient(&fileDC{}), Log: slog.New(slog.NewTextHandler(&buf, nil)),
		Pools: &fakePools{dcs: map[int]*fileDC{4: {data: data}}, this: homeDC}}
	f := MediaFile{Info: MediaInfo{Size: 1500}, Location: &tg.InputDocumentFileLocation{ID: 1}, DC: 4}
	if err := s.DownloadFile(context.Background(), f, &memSink{}, noRefetch); err != nil {
		t.Fatal(err)
	}
	line := buf.String()
	for _, want := range []string{"download done", "dc=4", "concurrency=8", "bytes=1500", "elapsed="} {
		if !strings.Contains(line, want) {
			t.Errorf("log %q lacks %q", line, want)
		}
	}
	if strings.Count(line, "\n") != 1 {
		t.Errorf("log has %d lines, want 1", strings.Count(line, "\n"))
	}
}
