package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// maxRunningDownloads is how many downloads run at once; more are
	// refused, not queued, so "downloading" is always true.
	maxRunningDownloads = 2
	// downloadWait is how long download_media waits before answering
	// "downloading"; below ToolTimeout and the clients' own ~60 s limit.
	downloadWait = 45 * time.Second
	// downloadResultTTL is how long a finished download is remembered for
	// the calls that poll it.
	downloadResultTTL = 30 * time.Minute
	// downloadStall ends a job that has written nothing for this long.
	downloadStall = 2 * time.Minute
	// downloadMaxDuration ends a job however it is going.
	downloadMaxDuration = time.Hour
	// shutdownWait is how long Shutdown waits for jobs it cancelled.
	shutdownWait = 10 * time.Second
)

var (
	errTooManyDownloads = errors.New("2 downloads are already running; try again when one finishes")
	errTooManyUploads   = errors.New("2 uploads are already running; try again when one finishes")
	errStopping         = errors.New("the server is stopping")
	errServerStopped    = errors.New("canceled: server stopped")
	errStalled          = errors.New("download stalled")
	errTooLong          = errors.New("download took too long")
)

// jobKey identifies a job: a download by chat and message, whatever dir
// or name; an upload by chat and ref (its idempotency key, given or
// derived).
type jobKey struct {
	chat int64
	msg  int
	ref  string
}

// jobKind separates downloads from uploads: each has its own limit.
type jobKind int

const (
	kindDownload jobKind = iota
	kindUpload
)

// jobInfo is what a status answer reports before the job is done.
type jobInfo struct {
	kind    jobKind
	planned string // dir/name; the final path may get " (1)"
	size    int64
	mime    string
}

// job is one download running in the server, or its result.
type job struct {
	key      jobKey
	info     jobInfo
	done     chan struct{}
	progress atomic.Pointer[progress]

	started time.Time

	// Set before done closes; read only after. value is what an upload
	// job's body leaves for the answer (its sentOut).
	value    any
	path     string
	err      error
	finished time.Time
}

type progress struct{ p interface{ Written() int64 } }

// setProgress tells status answers where to read the bytes written.
func (j *job) setProgress(p interface{ Written() int64 }) { j.progress.Store(&progress{p}) }

func (j *job) downloaded() int64 {
	if p := j.progress.Load(); p != nil {
		return p.p.Written()
	}
	return 0
}

// result is the job's outcome; call it only after done has closed.
func (j *job) result() (string, error) { return j.path, j.err }

// Downloads runs download_media jobs in the server process, one per
// message, on the server's context rather than a tool call's: a call that
// ends only stops waiting. Every MCP server of the process shares one, so
// a read-only and a full session never download the same message twice.
type Downloads struct {
	// Log gets a line when Shutdown gives up on jobs; nil logs nothing.
	Log *slog.Logger

	ctx          context.Context
	cancel       context.CancelCauseFunc
	wait         time.Duration
	ttl          time.Duration
	stall        time.Duration
	maxDuration  time.Duration
	shutdownWait time.Duration
	now          func() time.Time

	// Journal records each job's temp file while it exists, so a killed
	// server's next start can remove it; nil records nothing.
	Journal interface {
		Add(ctx context.Context, path string) error
		Remove(ctx context.Context, path string) error
	}

	mu      sync.Mutex
	jobs    map[jobKey]*job
	running map[jobKind]int
	closed  bool
	wg      sync.WaitGroup
}

// NewDownloads returns a registry whose jobs live until ctx ends or
// Shutdown is called.
func NewDownloads(ctx context.Context) *Downloads {
	ctx, cancel := context.WithCancelCause(ctx)
	return &Downloads{ctx: ctx, cancel: cancel, wait: downloadWait, ttl: downloadResultTTL,
		stall: downloadStall, maxDuration: downloadMaxDuration, shutdownWait: shutdownWait,
		now: time.Now, jobs: map[jobKey]*job{}, running: map[jobKind]int{}}
}

// start is startOrFind for callers that do not need to know whether run
// was started.
func (d *Downloads) start(key jobKey, info jobInfo, run func(context.Context, *job) (string, error)) (*job, error) {
	j, _, err := d.startOrFind(key, info, run)
	return j, err
}

// startOrFind returns the job for key, starting run in the background if
// there is none to reuse; created says whether run was started, so a
// caller can hand the job what run needs (an open file) or release it. A
// finished job is reused only while its result is fresh and, for a
// download that succeeded, while its file is still there with its size.
// Downloads and uploads each run at most maxRunningDownloads at once.
func (d *Downloads) startOrFind(key jobKey, info jobInfo, run func(context.Context, *job) (string, error)) (*job, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil, false, errStopping
	}
	if j, ok := d.jobs[key]; ok {
		select {
		case <-j.done:
			if d.now().Sub(j.finished) <= d.ttl &&
				(j.err != nil || j.info.kind == kindUpload || fileIs(j.path, j.info.size)) {
				return j, false, nil
			}
			delete(d.jobs, key)
		default:
			return j, false, nil // still running
		}
	}
	if d.running[info.kind] >= maxRunningDownloads {
		if info.kind == kindUpload {
			return nil, false, errTooManyUploads
		}
		return nil, false, errTooManyDownloads
	}

	j := &job{key: key, info: info, done: make(chan struct{}), started: d.now()}
	d.jobs[key] = j
	d.running[info.kind]++
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		ctx, cancel := context.WithCancelCause(d.ctx)
		defer cancel(nil)
		ctx, cancelTimeout := context.WithTimeoutCause(ctx, d.maxDuration,
			fmt.Errorf("%w: over %s", errTooLong, d.maxDuration))
		defer cancelTimeout()
		go d.watch(ctx, cancel, j)
		path, err := run(ctx, j)
		if err != nil && ctx.Err() != nil {
			err = context.Cause(ctx) // why it was cancelled, not just that it was
		}
		d.mu.Lock()
		j.path, j.err, j.finished = path, err, d.now()
		d.running[info.kind]--
		d.mu.Unlock()
		close(j.done)
	}()
	return j, true, nil
}

// watch cancels j's context if j writes nothing for the stall time.
func (d *Downloads) watch(ctx context.Context, cancel context.CancelCauseFunc, j *job) {
	t := time.NewTicker(d.stall / 4)
	defer t.Stop()
	last, since := j.downloaded(), time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if n := j.downloaded(); n != last {
			last, since = n, time.Now()
		} else if time.Since(since) >= d.stall {
			cancel(fmt.Errorf("%w: no data for %s", errStalled, d.stall))
			return
		}
	}
}

// fileIs reports whether path is a regular file of size bytes.
func fileIs(path string, size int64) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular() && fi.Size() == size
}

// forget drops a finished job, so the next call starts again. A failure is
// forgotten once it has been reported: a retry should download, not get
// the old error.
func (d *Downloads) forget(j *job) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.jobs[j.key] == j {
		delete(d.jobs, j.key)
	}
}

// Shutdown cancels every running job and waits for them to end; their
// partial files are removed and their audit lines written before it
// returns. Nothing starts after it. A job still running after the wait
// limit is abandoned and logged: the startup sweep removes its file.
func (d *Downloads) Shutdown() {
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()
	d.cancel(errServerStopped)

	done := make(chan struct{})
	go func() { d.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d.shutdownWait):
		d.mu.Lock()
		downloads, uploads := d.running[kindDownload], d.running[kindUpload]
		d.mu.Unlock()
		if d.Log != nil {
			d.Log.Warn("jobs abandoned at shutdown", "downloads", downloads, "uploads", uploads, "waited", d.shutdownWait)
		}
	}
}

// waitFor waits for j until it is done, the wait is over, or ctx ends,
// leaving room before ctx's deadline for the answer.
func (d *Downloads) waitFor(ctx context.Context, j *job) error {
	wait := d.wait
	if dl, ok := ctx.Deadline(); ok {
		wait = min(wait, time.Until(dl)-5*time.Second)
	}
	t := time.NewTimer(max(wait, 0))
	defer t.Stop()
	select {
	case <-j.done:
	case <-t.C:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// etaSeconds is how long the rest of size takes at the speed so far,
// rounded up; 0 while there is no speed to go by, or nothing left.
func etaSeconds(downloaded, size int64, elapsed time.Duration) int64 {
	if downloaded <= 0 || elapsed <= 0 || downloaded >= size {
		return 0
	}
	perSecond := float64(downloaded) / elapsed.Seconds()
	return int64(math.Ceil(float64(size-downloaded) / perSecond))
}
