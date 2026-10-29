package tele

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

const (
	// downloadPartSize is the largest part upload.getFile serves.
	downloadPartSize = 1 << 20
	// downloadConcurrency is the number of parts requested at once. A DC
	// pool connection carries one request at a time, so pools have exactly
	// this many connections.
	downloadConcurrency = 8
)

// Pools opens connections to other DCs and reports the home DC;
// *telegram.Client satisfies it.
type Pools interface {
	DC(ctx context.Context, dc int, max int64) (telegram.CloseInvoker, error)
	Config() tg.Config
}

// PartSink is where DownloadFile writes; Reset empties it for a restart.
type PartSink interface {
	io.WriterAt
	Reset() error
}

// DownloadFile writes f to w in parallel on f's DC. refetch reloads the
// message when the file reference expired.
func (s *Service) DownloadFile(ctx context.Context, f MediaFile, w PartSink,
	refetch func(context.Context) (MediaFile, error)) error {
	api, err := s.fileAPI(ctx, f.DC)
	if err != nil {
		return err
	}
	_, err = downloader.NewDownloader().WithPartSize(downloadPartSize).
		Download(api, f.Location).WithThreads(downloadConcurrency).Parallel(ctx, w)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	return nil
}

// poolOpenTimeout bounds one pool opening (dial and auth transfer).
const poolOpenTimeout = 30 * time.Second

var errServiceClosed = errors.New("service closed")

// dcCache holds the pools opened to other DCs, one per DC.
//
// An opening runs in its own goroutine, on a context of its own, never the
// caller's: a caller that gives up only stops waiting, and the opening
// still serves everyone else waiting for that DC. No lock is held while a
// DC is being opened, so a slow handshake holds up only its own DC.
type dcCache struct {
	base context.Context // parent of every opening; the client's run context

	mu      sync.Mutex
	entries map[int]*dcEntry
	closed  bool
	cancel  context.CancelFunc // cancels every opening; set with ctx
	ctx     context.Context
	openers sync.WaitGroup
}

// dcEntry is one DC's pool, or its opening while done is open.
type dcEntry struct {
	done chan struct{}
	pool telegram.CloseInvoker // set when done closes, unless err is
	err  error
}

// fileAPI returns the client to fetch a file stored on dc. Files on other
// DCs go straight to a pool there: through the home connection every part
// would first be refused with FILE_MIGRATE. While the home DC is unknown
// (0), or the file's DC is, the home connection is used, so a pool is never
// opened on the home DC with the primary key.
func (s *Service) fileAPI(ctx context.Context, dc int) (downloader.Client, error) {
	if s.Pools == nil || dc == 0 {
		return tg.NewClient(floodLimit{s.API.Invoker()}), nil
	}
	if home := s.Pools.Config().ThisDC; home == 0 || dc == home {
		return tg.NewClient(floodLimit{s.API.Invoker()}), nil
	}

	c := &s.dcs
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errServiceClosed
	}
	e, ok := c.entries[dc]
	if !ok {
		if c.ctx == nil {
			c.ctx, c.cancel = context.WithCancel(cmp.Or(c.base, context.Background()))
		}
		if c.entries == nil {
			c.entries = map[int]*dcEntry{}
		}
		e = &dcEntry{done: make(chan struct{})}
		c.entries[dc] = e
		c.openers.Add(1)
		go s.openDC(dc, e)
	}
	c.mu.Unlock()

	select {
	case <-e.done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if e.err != nil {
		return nil, e.err
	}
	return tg.NewClient(floodLimit{e.pool}), nil
}

// openDC opens dc's pool for e. A failure is not cached: the entry is
// dropped, so the next download tries again. A pool that arrives after
// Close is closed here, since Close has already collected the others.
func (s *Service) openDC(dc int, e *dcEntry) {
	c := &s.dcs
	defer c.openers.Done()
	defer close(e.done) // after the fields below are set, on every path

	ctx, cancel := context.WithTimeout(c.ctx, poolOpenTimeout)
	defer cancel()
	pool, err := s.Pools.DC(ctx, dc, downloadConcurrency)

	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case err != nil:
		e.err = fmt.Errorf("connect to DC %d: %w", dc, err)
		delete(c.entries, dc)
	case c.closed:
		e.err = errors.Join(errServiceClosed, pool.Close())
	default:
		e.pool = pool
	}
}

// Close cancels the openings still running, waits for them, and closes
// every pool. Nothing opens after it.
func (s *Service) Close() error {
	c := &s.dcs
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	if c.cancel != nil {
		c.cancel()
	}
	c.mu.Unlock()

	// No opener starts once closed is set, so Wait cannot miss one.
	c.openers.Wait()

	c.mu.Lock()
	entries := c.entries
	c.entries = nil
	c.mu.Unlock()
	var errs []error
	for _, e := range entries {
		if e.pool != nil {
			errs = append(errs, e.pool.Close())
		}
	}
	return errors.Join(errs...)
}

// maxFloodSleep is the longest flood wait a download sleeps through, the
// same limit as the client's flood waiter.
const maxFloodSleep = time.Minute

// FloodError is a flood wait too long to sleep through inside a download.
// It is deliberately not a tgerr.Error, so gotd's downloader, which sleeps
// through any FLOOD_WAIT it sees, gives up instead.
type FloodError struct{ Seconds int }

func (e *FloodError) Error() string { return fmt.Sprintf("flood wait of %d seconds", e.Seconds) }

// floodLimit passes flood waits up to maxFloodSleep on to the downloader,
// which sleeps and retries them, and turns longer ones into a FloodError.
// It wraps both the pools, which bypass the client's middleware, and the
// home connection, whose waiter returns long waits as the original error.
type floodLimit struct{ next tg.Invoker }

func (f floodLimit) Invoke(ctx context.Context, in bin.Encoder, out bin.Decoder) error {
	err := f.next.Invoke(ctx, in, out)
	if d, ok := tgerr.AsFloodWait(err); ok && d > maxFloodSleep {
		return &FloodError{Seconds: int(d / time.Second)}
	}
	return err
}
