package feed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/itskamol/telegram-mcp-go/internal/storage"
	"github.com/itskamol/telegram-mcp-go/internal/tele"
)

// lines is an io.Writer that collects Notice lines safely across goroutines.
type lines struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lines) notices(t *testing.T) []Notice {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Notice
	for _, s := range strings.Split(strings.TrimSpace(l.buf.String()), "\n") {
		if s == "" {
			continue
		}
		var n Notice
		if err := json.Unmarshal([]byte(s), &n); err != nil {
			t.Fatalf("not a JSON line: %q", s)
		}
		out = append(out, n)
	}
	return out
}

// waitFor polls the collected lines until want of them exist.
func (l *lines) waitFor(t *testing.T, want int) []Notice {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if n := l.notices(t); len(n) >= want {
			return n
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("got %d lines, want %d", len(l.notices(t)), want)
	return nil
}

type rig struct {
	path   string
	out    *lines
	cancel context.CancelFunc
	done   chan error
}

func startWatch(t *testing.T, path, consumer string, stale time.Duration) *rig {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &rig{path: path, out: &lines{}, cancel: cancel, done: make(chan error, 1)}
	w := &Watcher{Path: path, Account: "default", Consumer: consumer, Every: 20 * time.Millisecond,
		Stale: stale, Out: r.out, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	go func() { r.done <- w.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-r.done:
		case <-time.After(3 * time.Second):
			t.Error("watcher did not stop on cancel")
		}
	})
	return r
}

// writer opens the database like the server does and returns its store.
func writer(t *testing.T, path string) (*storage.DB, *Store) {
	t.Helper()
	db, err := storage.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewStore(context.Background(), db, "default")
	if err != nil {
		t.Fatal(err)
	}
	return db, s
}

func add(t *testing.T, s *Store, chat int64, id, edit int) {
	t.Helper()
	if _, err := s.Upsert(context.Background(), storage.KindChannel, 0,
		tele.Message{ChatID: chat, ID: id, EditDate: edit}, int(time.Now().Unix())); err != nil {
		t.Fatal(err)
	}
}

const watched = -1004325193310

func TestWatchAnnouncesNewMessagesOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "w.db")
	db, s := writer(t, path)
	defer db.Close()
	add(t, s, watched, 1, 0) // before watch started: a new consumer is not told

	r := startWatch(t, path, "default", time.Hour)
	time.Sleep(100 * time.Millisecond)
	add(t, s, watched, 1142, 0)
	add(t, s, watched, 1143, 0)
	// Two commits may come as one notice or two; together they cover both.
	var got []Notice
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got = r.out.notices(t); len(got) > 0 && got[len(got)-1].LastID == 1143 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	total := 0
	for _, n := range got {
		if n.ChatID != watched || n.FirstID < 1142 {
			t.Fatalf("notice for an old message or another chat: %+v", got)
		}
		total += n.Count
	}
	if total != 2 || got[len(got)-1].LastID != 1143 {
		t.Fatalf("notices %+v", got)
	}

	// Unrelated writes change data_version but must not wake anyone again.
	before := len(r.out.notices(t))
	for i := range 5 {
		if err := storage.NewPeers(db, "default").Put(context.Background(),
			[]storage.Peer{{Kind: storage.KindUser, ID: int64(i), Title: "x"}}); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(150 * time.Millisecond)
	if n := len(r.out.notices(t)); n != before {
		t.Fatalf("duplicate wake-up: %d lines after unrelated writes, want %d", n, before)
	}

	// An edit is a new version and is announced.
	add(t, s, watched, 1142, 99)
	got = r.out.waitFor(t, before+1)
	if got[len(got)-1].FirstID != 1142 || got[len(got)-1].Count != 1 {
		t.Fatalf("edit notice %+v", got[len(got)-1])
	}
}

func TestWatchAnnouncesUnacknowledgedBacklogAtStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "w.db")
	db, s := writer(t, path)
	defer db.Close()
	add(t, s, watched, 10, 0)
	add(t, s, watched, 11, 0)
	b, _ := s.Fetch(context.Background(), "default", 0, 1) // delivered 10, acked nothing
	add(t, s, watched, 12, 0)
	_ = b

	r := startWatch(t, path, "default", time.Hour)
	got := r.out.waitFor(t, 1)
	if got[0].FirstID != 10 || got[0].LastID != 12 || got[0].Count != 3 {
		t.Fatalf("backlog notice %+v", got[0])
	}
}

func TestWatchWaitsForDatabaseAndTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "late.db")
	r := startWatch(t, path, "default", time.Hour) // nothing exists yet
	time.Sleep(100 * time.Millisecond)

	plain, err := storage.Open(path) // database without feed tables
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	s, err := NewStore(context.Background(), plain, "default") // now the tables appear
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	add(t, s, watched, 7, 0)
	if got := r.out.waitFor(t, 1); got[0].FirstID != 7 {
		t.Fatalf("notice %+v", got[0])
	}
}

func TestWatchFollowsReplacedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.db")
	db, s := writer(t, path)
	add(t, s, watched, 1, 0)
	r := startWatch(t, path, "default", time.Hour)
	time.Sleep(100 * time.Millisecond)
	db.Close()

	for _, f := range []string{path, path + "-wal", path + "-shm"} {
		os.Remove(f) // a new inode will take the name
	}
	db2, s2 := writer(t, path)
	defer db2.Close()
	add(t, s2, watched, 500, 0)
	got := r.out.waitFor(t, 1)
	if got[len(got)-1].FirstID != 500 {
		t.Fatalf("watcher stayed on the unlinked file: %+v", got)
	}
}

func TestWatchReannouncesStaleAndStopsAfterAck(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.db")
	db, s := writer(t, path)
	defer db.Close()
	if _, err := s.Fetch(context.Background(), "default", 0, 10); err != nil { // consumer exists
		t.Fatal(err)
	}
	r := startWatch(t, path, "default", 150*time.Millisecond)
	time.Sleep(60 * time.Millisecond)
	add(t, s, watched, 1, 0)

	got := r.out.waitFor(t, 2) // first notice, then the same one again: never acknowledged
	if got[0] != got[1] {
		t.Fatalf("reminder differs from the notice: %+v", got)
	}
	b, _ := s.Fetch(context.Background(), "default", 0, 10)
	if _, err := s.Fetch(context.Background(), "default", b.Cursor, 10); err != nil { // ack
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	n := len(r.out.notices(t))
	time.Sleep(400 * time.Millisecond)
	if m := len(r.out.notices(t)); m != n {
		t.Fatalf("announced again after the ack: %d -> %d lines", n, m)
	}
}

func TestWatchDoesNotReannounceWhatTheAgentReadAhead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.db")
	db, s := writer(t, path)
	defer db.Close()
	if _, err := s.Fetch(context.Background(), "default", 0, 10); err != nil {
		t.Fatal(err)
	}
	r := startWatch(t, path, "default", time.Hour)
	time.Sleep(60 * time.Millisecond)
	add(t, s, watched, 1, 0)
	r.out.waitFor(t, 1)
	add(t, s, watched, 2, 0)
	// The agent reads and acks both before the watcher has looked again.
	b, _ := s.Fetch(context.Background(), "default", 0, 10)
	if _, err := s.Fetch(context.Background(), "default", b.Cursor, 10); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	for _, n := range r.out.notices(t)[1:] {
		if n.FirstID == 1 {
			t.Fatalf("message 1 announced again: %+v", r.out.notices(t))
		}
	}
}

type brokenPipe struct{}

func (brokenPipe) Write([]byte) (int, error) { return 0, errors.New("write: broken pipe") }

func TestWatchStopsWhenOutputBreaks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p#?.db")
	db, s := writer(t, path)
	defer db.Close()
	w := &Watcher{Path: path, Account: "default", Consumer: "default", Every: 20 * time.Millisecond,
		Stale: time.Hour, Out: brokenPipe{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	done := make(chan error, 1)
	go func() { done <- w.Run(context.Background()) }()
	time.Sleep(60 * time.Millisecond)
	add(t, s, watched, 1, 0)
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "broken pipe") {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("watcher kept running with nobody reading its output")
	}
}

func TestIsNoTable(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "n.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, missing := db.SQL.Exec(`SELECT * FROM feed_messages`)
	_, syntax := db.SQL.Exec(`SELEC 1`)
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"missing table", missing, true},
		{"syntax error, same SQLITE_ERROR code", syntax, false},
		{"not from SQLite, same words", errors.New("no such table: feed_messages"), false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := isNoTable(c.err); got != c.want {
			t.Errorf("%s: isNoTable(%v) = %v", c.name, c.err, got)
		}
	}
}

// stuckPipe blocks every Write until released, like a pipe whose reader
// hangs with a full buffer.
type stuckPipe struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *stuckPipe) Write(b []byte) (int, error) {
	p.once.Do(func() { close(p.entered) })
	<-p.release
	return len(b), nil
}

func TestBlockedOutputDoesNotStarveCheckpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.db")
	db, s := writer(t, path)
	defer db.Close()
	pipe := &stuckPipe{entered: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	w := &Watcher{Path: path, Account: "default", Consumer: "default", Every: 20 * time.Millisecond,
		Stale: time.Hour, Out: pipe, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	defer func() { cancel(); close(pipe.release); <-done }()

	time.Sleep(60 * time.Millisecond)
	add(t, s, watched, 1, 0)
	add(t, s, -1001889276137, 2, 0) // a second chat, so there is more to read
	select {
	case <-pipe.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("watcher never wrote")
	}
	// The watcher is stuck writing. The server keeps writing and must be
	// able to checkpoint the WAL back to zero meanwhile.
	for i := range 50 {
		add(t, s, watched, 100+i, 0)
	}
	var busy, logFrames, done2 int
	if err := db.SQL.QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &done2); err != nil {
		t.Fatal(err)
	}
	if busy != 0 {
		t.Fatalf("checkpoint busy=%d: the stuck watcher holds a read transaction", busy)
	}
}

func TestWatchAnnouncesOnlyConsumerChats(t *testing.T) {
	path := filepath.Join(t.TempDir(), "w.db")
	db, s := writer(t, path)
	defer db.Close()
	if err := s.SetChats(context.Background(), "pos", []int64{watched}); err != nil {
		t.Fatal(err)
	}

	r := startWatch(t, path, "pos", time.Hour)
	time.Sleep(100 * time.Millisecond)
	add(t, s, -1001234, 1, 0)
	add(t, s, watched, 2, 0)
	r.out.waitFor(t, 1)
	time.Sleep(100 * time.Millisecond)
	got := r.out.notices(t)
	if len(got) != 1 || got[0].ChatID != watched || got[0].Count != 1 {
		t.Fatalf("notices %+v, want only chat %d", got, watched)
	}
}

func TestWatchReadsConsumersWithoutChatIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, s := writer(t, path)
	defer db.Close()
	// A server of an older version: no chat_ids column.
	if _, err := db.SQL.Exec(`ALTER TABLE feed_consumers DROP COLUMN chat_ids`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`INSERT INTO feed_consumers (account, name, committed, delivered) VALUES ('default', 'old', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	add(t, s, watched, 5, 0) // unacknowledged backlog
	r := startWatch(t, path, "old", time.Hour)
	if got := r.out.waitFor(t, 1); got[0].FirstID != 5 {
		t.Fatalf("notices %+v", got)
	}
}
