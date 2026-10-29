package mcpserver

import (
	"context"
	"errors"
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
)

var (
	errTooManyDownloads = errors.New("2 downloads are already running; try again when one finishes")
	errStopping         = errors.New("the server is stopping")
)

// jobKey identifies a download: one job per message, whatever dir or name.
type jobKey struct {
	chat int64
	msg  int
}

// jobInfo is what a status answer reports before the job is done.
type jobInfo struct {
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

	// Set before done closes; read only after.
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
	ctx    context.Context
	cancel context.CancelFunc
	wait   time.Duration
	ttl    time.Duration
	now    func() time.Time

	mu      sync.Mutex
	jobs    map[jobKey]*job
	running int
	closed  bool
	wg      sync.WaitGroup
}

// NewDownloads returns a registry whose jobs live until ctx ends or
// Shutdown is called.
func NewDownloads(ctx context.Context) *Downloads {
	ctx, cancel := context.WithCancel(ctx)
	return &Downloads{ctx: ctx, cancel: cancel, wait: downloadWait, ttl: downloadResultTTL,
		now: time.Now, jobs: map[jobKey]*job{}}
}

// start returns the job for key, starting run in the background if there
// is none to reuse. A finished job is reused only while its result is
// fresh and, if it succeeded, its file is still there with its size.
func (d *Downloads) start(key jobKey, info jobInfo, run func(context.Context, *job) (string, error)) (*job, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil, errStopping
	}
	if j, ok := d.jobs[key]; ok {
		select {
		case <-j.done:
			if d.now().Sub(j.finished) <= d.ttl && (j.err != nil || fileIs(j.path, j.info.size)) {
				return j, nil
			}
			delete(d.jobs, key)
		default:
			return j, nil // still running
		}
	}
	if d.running >= maxRunningDownloads {
		return nil, errTooManyDownloads
	}

	j := &job{key: key, info: info, done: make(chan struct{})}
	d.jobs[key] = j
	d.running++
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		path, err := run(d.ctx, j)
		d.mu.Lock()
		j.path, j.err, j.finished = path, err, d.now()
		d.running--
		d.mu.Unlock()
		close(j.done)
	}()
	return j, nil
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
// returns. Nothing starts after it.
func (d *Downloads) Shutdown() {
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()
	d.cancel()
	d.wg.Wait()
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
