package mcpserver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// gatedRun is a job body that blocks until release is closed, counting
// how many times it started and reporting progress through written.
type gatedRun struct {
	release chan struct{}
	entered chan struct{} // one value per run, once its progress is set
	started atomic.Int32
	written atomic.Int64
	path    string
	err     error
	ctxErr  atomic.Value // the job context's error when it returned
}

func newGatedRun() *gatedRun {
	return &gatedRun{release: make(chan struct{}), entered: make(chan struct{}, 16)}
}

func (g *gatedRun) Written() int64 { return g.written.Load() }

func (g *gatedRun) run(ctx context.Context, j *job) (string, error) {
	g.started.Add(1)
	j.setProgress(g)
	g.entered <- struct{}{}
	select {
	case <-g.release:
		return g.path, g.err
	case <-ctx.Done():
		g.ctxErr.Store(ctx.Err())
		return "", ctx.Err()
	}
}

func waitDone(t *testing.T, j *job) {
	t.Helper()
	select {
	case <-j.done:
	case <-time.After(5 * time.Second):
		t.Fatal("job did not finish")
	}
}

var info = jobInfo{planned: "/x/f.bin", size: 10, mime: "application/octet-stream"}

// A second start for a running key returns the same job and runs nothing.
func TestDownloadsOneJobPerKey(t *testing.T) {
	d := NewDownloads(context.Background())
	t.Cleanup(d.Shutdown)
	g := newGatedRun()
	j1, err := d.start(jobKey{1, 5}, info, g.run)
	if err != nil {
		t.Fatal(err)
	}
	j2, err := d.start(jobKey{1, 5}, jobInfo{planned: "/other/name.bin"}, g.run)
	if err != nil {
		t.Fatal(err)
	}
	if j1 != j2 {
		t.Error("second start made a new job")
	}
	<-g.entered // the job body runs in its own goroutine
	g.written.Store(7)
	if got := j2.downloaded(); got != 7 {
		t.Errorf("downloaded = %d, want 7", got)
	}
	close(g.release)
	waitDone(t, j1)
	if n := g.started.Load(); n != 1 {
		t.Errorf("job body ran %d times, want 1", n)
	}
}

// At most two jobs run; a third distinct one is refused, not queued, and
// is accepted once one of the two has finished.
func TestDownloadsLimit(t *testing.T) {
	d := NewDownloads(context.Background())
	t.Cleanup(d.Shutdown)
	a, b, c := newGatedRun(), newGatedRun(), newGatedRun()
	ja, _ := d.start(jobKey{1, 1}, info, a.run)
	d.start(jobKey{1, 2}, info, b.run)
	if _, err := d.start(jobKey{1, 3}, info, c.run); !errors.Is(err, errTooManyDownloads) {
		t.Fatalf("third job: err = %v, want errTooManyDownloads", err)
	}
	if c.started.Load() != 0 {
		t.Fatal("refused job ran")
	}
	close(a.release)
	waitDone(t, ja)
	if _, err := d.start(jobKey{1, 3}, info, c.run); err != nil {
		t.Fatalf("third job after one finished: %v", err)
	}
	close(b.release)
	close(c.release)
}

// A finished download is answered from the registry while its file is
// still there with its size; once the file is gone, or the result is older
// than the TTL, the next start downloads again.
func TestDownloadsDoneKept(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f.bin")
	if err := os.WriteFile(file, make([]byte, info.size), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0)
	d := NewDownloads(context.Background())
	d.now = func() time.Time { return now }
	t.Cleanup(d.Shutdown)
	key := jobKey{1, 5}

	g := newGatedRun()
	g.path = file
	close(g.release)
	j, _ := d.start(key, info, g.run)
	waitDone(t, j)

	again, err := d.start(key, info, g.run)
	if err != nil || again != j || g.started.Load() != 1 {
		t.Fatalf("repeat start: same=%v err=%v runs=%d; want the finished job, no run", again == j, err, g.started.Load())
	}
	if path, err := again.result(); path != file || err != nil {
		t.Errorf("result = %q, %v", path, err)
	}

	os.Remove(file)
	fresh, _ := d.start(key, info, g.run)
	if fresh == j {
		t.Fatal("deleted file still reported as done")
	}
	waitDone(t, fresh)

	// A new finished result, then past the TTL.
	if err := os.WriteFile(file, make([]byte, info.size), 0o600); err != nil {
		t.Fatal(err)
	}
	cur, _ := d.start(key, info, g.run)
	waitDone(t, cur)
	runs := g.started.Load()
	now = now.Add(d.ttl + time.Second)
	next, _ := d.start(key, info, g.run)
	if next == cur {
		t.Fatal("result older than the TTL was reused")
	}
	waitDone(t, next)
	if g.started.Load() != runs+1 {
		t.Errorf("runs = %d, want %d", g.started.Load(), runs+1)
	}
}

// A failure is kept until it has been reported, then forgotten, so a
// retry downloads again instead of getting the old error.
func TestDownloadsFailureForgottenAfterReport(t *testing.T) {
	d := NewDownloads(context.Background())
	t.Cleanup(d.Shutdown)
	key := jobKey{1, 5}
	g := newGatedRun()
	g.err = errors.New("boom")
	close(g.release)
	j, _ := d.start(key, info, g.run)
	waitDone(t, j)
	again, _ := d.start(key, info, g.run)
	if _, err := again.result(); again != j || err == nil {
		t.Fatalf("before the report: same=%v err=%v", again == j, err)
	}
	d.forget(j)
	retry, _ := d.start(key, info, g.run)
	if retry == j {
		t.Fatal("forgotten failure was not retried")
	}
	waitDone(t, retry)
	if n := g.started.Load(); n != 2 {
		t.Errorf("runs = %d, want 2", n)
	}
}

// Shutdown cancels running jobs and waits for them; nothing starts after.
func TestDownloadsShutdown(t *testing.T) {
	d := NewDownloads(context.Background())
	g := newGatedRun()
	j, _ := d.start(jobKey{1, 5}, info, g.run)
	d.Shutdown()
	select {
	case <-j.done:
	default:
		t.Fatal("Shutdown returned before the job ended")
	}
	if err, _ := g.ctxErr.Load().(error); !errors.Is(err, context.Canceled) {
		t.Errorf("job context error = %v, want canceled", err)
	}
	if _, err := d.start(jobKey{1, 6}, info, newGatedRun().run); !errors.Is(err, errStopping) {
		t.Errorf("start after Shutdown: err = %v, want errStopping", err)
	}
}
