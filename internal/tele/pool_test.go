package tele

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
)

// opened is what a gatedPools.DC call hands back.
type opened struct {
	pool telegram.CloseInvoker
	err  error
}

var errGateClosed = errors.New("gate closed by test cleanup")

// gatedPools blocks each DC call until the test sends its result on that
// DC's gate. It ignores ctx, like a handshake that completes anyway, so
// the tests decide every ordering themselves.
type gatedPools struct {
	gates   map[int]chan opened
	entered chan int // the dc of each DC call, as it starts

	mu    sync.Mutex
	calls []int
	ctxs  []context.Context
}

func newGatedPools(dcs ...int) *gatedPools {
	p := &gatedPools{gates: map[int]chan opened{}, entered: make(chan int, 16)}
	for _, dc := range dcs {
		p.gates[dc] = make(chan opened, 2)
	}
	return p
}

func (p *gatedPools) DC(ctx context.Context, dc int, _ int64) (telegram.CloseInvoker, error) {
	p.mu.Lock()
	p.calls = append(p.calls, dc)
	p.ctxs = append(p.ctxs, ctx)
	p.mu.Unlock()
	p.entered <- dc
	r, ok := <-p.gates[dc]
	if !ok || (r.pool == nil && r.err == nil) {
		return nil, errGateClosed
	}
	return r.pool, r.err
}

func (p *gatedPools) Pool(int64) (telegram.CloseInvoker, error) {
	return nil, errors.New("gatedPools: no home pool")
}

func (p *gatedPools) Config() tg.Config { return tg.Config{ThisDC: homeDC} }

func (p *gatedPools) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

// release unblocks every DC call still waiting, so a failed test does not
// leave goroutines stuck holding locks.
func (p *gatedPools) release(t *testing.T) {
	t.Cleanup(func() {
		for _, g := range p.gates {
			close(g)
		}
	})
}

// within waits for ch, failing instead of hanging if it never comes. It is
// a guard against a deadlock, not a way to order events.
func within[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out: %s", what)
	}
	var zero T
	return zero
}

// done runs f in a goroutine and returns a channel with its error.
func done(f func() error) <-chan error {
	ch := make(chan error, 1)
	go func() { ch <- f() }()
	return ch
}

// A slow handshake with one DC must not hold up downloads from another.
func TestPoolOpenDoesNotBlockOtherDCs(t *testing.T) {
	data := fileData(1000)
	pools := newGatedPools(4, 5)
	pools.release(t)
	pools.gates[5] <- opened{pool: &fileDC{data: data}}
	s := &Service{API: tg.NewClient(&fileDC{}), Pools: pools}
	file := func(dc int) MediaFile {
		return MediaFile{Info: MediaInfo{Size: 1000}, Location: &tg.InputDocumentFileLocation{ID: 1}, DC: dc}
	}

	go s.DownloadFile(context.Background(), file(4), &memSink{}, noRefetch)
	if dc := within(t, pools.entered, "DC 4 opening"); dc != 4 {
		t.Fatalf("first open was DC %d", dc)
	}
	err := within(t, done(func() error {
		return s.DownloadFile(context.Background(), file(5), &memSink{}, noRefetch)
	}), "download from DC 5 while DC 4 is still opening")
	if err != nil {
		t.Errorf("download from DC 5: %v", err)
	}
}

// The caller that started an opening may give up; one still waiting for
// the same DC gets the pool, and the DC is opened only once.
func TestPoolOpenOutlivesFirstCaller(t *testing.T) {
	pools := newGatedPools(4)
	pools.release(t)
	s := &Service{API: tg.NewClient(&fileDC{}), Pools: pools}

	ctxA, cancelA := context.WithCancel(context.Background())
	errA := done(func() error { _, err := s.fileAPI(ctxA, 4); return err })
	within(t, pools.entered, "DC 4 opening")
	errB := done(func() error { _, err := s.fileAPI(context.Background(), 4); return err })

	cancelA()
	if err := within(t, errA, "cancelled first caller to return"); !errors.Is(err, context.Canceled) {
		t.Errorf("first caller: err = %v, want context.Canceled", err)
	}
	pools.gates[4] <- opened{pool: &fileDC{}}
	if err := within(t, errB, "second caller to get the pool"); err != nil {
		t.Errorf("second caller: %v", err)
	}
	if n := pools.callCount(); n != 1 {
		t.Errorf("DC opened %d times, want 1", n)
	}
}

// Close during an opening cancels it and waits for it; a pool that still
// arrives is closed, not cached, and nothing opens after Close.
func TestPoolCloseDuringOpen(t *testing.T) {
	pools := newGatedPools(4)
	pools.release(t)
	s := &Service{API: tg.NewClient(&fileDC{}), Pools: pools}

	errA := done(func() error { _, err := s.fileAPI(context.Background(), 4); return err })
	within(t, pools.entered, "DC 4 opening")
	closed := done(s.Close)

	pools.mu.Lock()
	openCtx := pools.ctxs[0]
	pools.mu.Unlock()
	within(t, openCtx.Done(), "Close to cancel the opening's context")
	select {
	case err := <-closed:
		t.Fatalf("Close returned (%v) while the opening was still running", err)
	default:
	}

	late := &fileDC{}
	pools.gates[4] <- opened{pool: late}
	if err := within(t, closed, "Close to return after the opening ended"); err != nil {
		t.Errorf("Close: %v", err)
	}
	if !late.closed {
		t.Error("the pool that arrived after Close was not closed")
	}
	if err := within(t, errA, "caller to return"); err == nil {
		t.Error("caller got a pool from a closed service")
	}
	after := done(func() error { _, err := s.fileAPI(context.Background(), 4); return err })
	if err := within(t, after, "fileAPI after Close to return"); err == nil {
		t.Error("fileAPI after Close succeeded")
	}
	if n := pools.callCount(); n != 1 {
		t.Errorf("DC opened %d times, want 1 (none after Close)", n)
	}
}

// A failed opening is not remembered: the next download tries again, and
// the pool it gets is then reused.
func TestPoolOpenErrorNotCached(t *testing.T) {
	pools := newGatedPools(4)
	pools.release(t)
	pools.gates[4] <- opened{err: errors.New("export authorization failed")}
	pools.gates[4] <- opened{pool: &fileDC{}}
	s := &Service{API: tg.NewClient(&fileDC{}), Pools: pools}

	if _, err := s.fileAPI(context.Background(), 4); err == nil {
		t.Fatal("first open: no error")
	}
	for i := range 2 {
		if _, err := s.fileAPI(context.Background(), 4); err != nil {
			t.Fatalf("open %d after the error: %v", i+2, err)
		}
	}
	if n := pools.callCount(); n != 2 {
		t.Errorf("DC opened %d times, want 2", n)
	}
}
