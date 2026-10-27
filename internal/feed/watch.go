package feed

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/itskamol/telegram-mcp-go/internal/storage"
)

// Notice is one line of `telegram-mcp watch`: a chat has new or edited
// messages. It carries ids only; the agent reads the messages, and
// acknowledges them, through get_new_messages.
type Notice struct {
	ChatID  int64 `json:"chat_id"`
	FirstID int   `json:"first_id"`
	LastID  int   `json:"last_id"`
	Count   int   `json:"count"`
	// Edits counts the messages of Count that are edited versions; an
	// edit is announced like a new message.
	Edits int `json:"edits"`
}

// Watcher prints a Notice line whenever the buffer gains messages. It is
// a doorbell, not a delivery channel: it never writes to the database and
// never acknowledges anything, so delivery stays with the consumer's
// server-side offset.
//
// It runs in its own process next to the server and reads the database
// read-only. Another process cannot be pushed to, so it checks PRAGMA
// data_version every Every (about 3 µs of CPU; the value lives in the
// WAL-index header in -shm, no disk read) and queries only on a change.
type Watcher struct {
	Path     string
	Account  string
	Consumer string
	Every    time.Duration // data_version check interval
	// Stale re-announces messages the consumer has not acknowledged for
	// this long, so an agent that died mid-batch is woken again.
	Stale time.Duration
	Out   io.Writer
	Log   *slog.Logger

	// first is the database file as it was when Run began, if it existed.
	first os.FileInfo
}

// errReopen means the database file is missing or was replaced: a
// connection keeps reading the old, unlinked inode forever otherwise.
var errReopen = errors.New("database file missing or replaced")

// Run watches until ctx ends. It waits for a missing database or missing
// feed tables instead of failing; it returns an error only when Out can no
// longer be written (the reader of the pipe is gone).
func (w *Watcher) Run(ctx context.Context) error {
	w.first, _ = os.Stat(w.Path)
	for ctx.Err() == nil {
		err := w.follow(ctx)
		var we writeError
		if errors.As(err, &we) {
			return we.err
		}
		if err != nil && !errors.Is(err, errReopen) && ctx.Err() == nil {
			w.Log.Warn("watch: reopening the database", "err", err)
		}
		select {
		case <-time.After(w.Every):
		case <-ctx.Done():
		}
	}
	return nil
}

// cursor is the state of one open database.
type cursor struct {
	hw        int64 // highest seq announced
	floor     int64 // seq the consumer started from; a stale reset never goes below it
	committed int64 // consumer's acknowledged seq at the last check
	since     time.Time
}

func (w *Watcher) follow(ctx context.Context) error {
	opened, err := os.Stat(w.Path)
	if err != nil {
		return errReopen // not created yet: wait
	}
	db, err := storage.OpenReadOnly(w.Path)
	if err != nil {
		return err
	}
	defer db.Close()
	conn, err := db.Conn(ctx) // pinned: data_version is per connection
	if err != nil {
		return err
	}
	defer conn.Close()

	c, err := w.start(ctx, conn, w.first != nil && os.SameFile(w.first, opened))
	if err != nil {
		return err
	}
	tick := time.NewTicker(w.Every)
	defer tick.Stop()
	seen := int64(-1)
	for {
		if now, err := os.Stat(w.Path); err != nil || !os.SameFile(opened, now) {
			return errReopen
		}
		var v int64
		if err := conn.QueryRowContext(ctx, `PRAGMA data_version`).Scan(&v); err != nil {
			return err
		}
		if v != seen {
			seen = v
			if err := w.announce(ctx, conn, &c); err != nil {
				return err
			}
		}
		if err := w.restale(ctx, conn, &c); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// start picks where announcing begins: from the consumer's acknowledged
// offset if it exists (so unacknowledged messages are announced at once).
// A new consumer on the file that existed when watch started hears only
// about new messages; on a file created or replaced since, everything in
// it is new, so it starts from zero.
func (w *Watcher) start(ctx context.Context, conn *sql.Conn, original bool) (cursor, error) {
	cs, err := w.consumer(ctx, conn)
	if err != nil {
		return cursor{}, err
	}
	committed := cs.committed
	if !cs.found && original {
		if committed, err = w.counter(ctx, conn); err != nil {
			return cursor{}, err
		}
	}
	return cursor{hw: committed, floor: committed, committed: committed, since: time.Now()}, nil
}

// announce prints one Notice per chat for messages after the high-water
// mark, then moves the mark. A counter below the mark means the buffer
// was recreated: start again from the consumer's offset.
func (w *Watcher) announce(ctx context.Context, conn *sql.Conn, c *cursor) error {
	counter, err := w.counter(ctx, conn)
	if err != nil {
		return err
	}
	if counter < c.hw {
		c.hw, c.floor = c.committed, 0
		if c.hw > counter {
			c.hw = 0
		}
	}
	cs, err := w.consumer(ctx, conn)
	if err != nil {
		return err
	}
	notices, top, err := w.pending(ctx, conn, c.hw, cs)
	if err != nil {
		return err
	}
	// Written only after the read transaction is closed: a stuck reader of
	// Out would otherwise keep it open and starve the server's checkpoints.
	for _, n := range notices {
		if err := w.emit(n); err != nil {
			return err
		}
	}
	c.hw = top // only after every line was written
	return nil
}

// pending reads one Notice per chat with messages after hw that the
// consumer's filter lets through, and the highest seq among them, in one
// short read transaction.
func (w *Watcher) pending(ctx context.Context, conn *sql.Conn, hw int64, cs consumerState) ([]Notice, int64, error) {
	where, args := inChats, []any{w.Account, hw, cs.chats, cs.chats}
	if cs.directed {
		where += ` AND ` + inDirection
		args = append(args, cs.direction, cs.direction)
	}
	rows, err := conn.QueryContext(ctx, `
		SELECT chat_id, MIN(message_id), MAX(message_id), COUNT(*), SUM(edit_date > 0), MAX(seq)
		FROM feed_messages WHERE account = ? AND seq > ? AND `+where+`
		GROUP BY chat_id ORDER BY MIN(seq)`, args...)
	if isNoTable(err) {
		return nil, hw, nil
	}
	if err != nil {
		return nil, hw, err
	}
	defer rows.Close()
	var out []Notice
	top := hw
	for rows.Next() {
		var n Notice
		var maxSeq int64
		if err := rows.Scan(&n.ChatID, &n.FirstID, &n.LastID, &n.Count, &n.Edits, &maxSeq); err != nil {
			return nil, hw, err
		}
		out = append(out, n)
		top = max(top, maxSeq)
	}
	return out, top, rows.Err()
}

// restale rolls the high-water mark back to the consumer's offset when
// announced messages stay unacknowledged for Stale, so they are announced
// again in the same Notice format.
func (w *Watcher) restale(ctx context.Context, conn *sql.Conn, c *cursor) error {
	cs, err := w.consumer(ctx, conn)
	if err != nil {
		return err
	}
	committed := cs.committed
	if !cs.found {
		committed = c.floor
	}
	committed = max(committed, c.floor)
	switch {
	case committed >= c.hw:
		// All handled; the agent may even have read past what was
		// announced, which must not be announced again.
		c.hw, c.committed, c.since = committed, committed, time.Now()
	case committed != c.committed:
		c.committed, c.since = committed, time.Now() // progress: wait again
	case time.Since(c.since) >= w.Stale:
		c.hw, c.since = committed, time.Now()
		return w.announce(ctx, conn, c)
	}
	return nil
}

// consumerState is a consumer's acknowledged offset and filter.
type consumerState struct {
	committed int64
	chats     string // JSON array of chat ids; '' means every chat
	direction string
	found     bool
	// directed is false on a database of a server older than the
	// direction column, which has no feed_messages.out to filter on.
	directed bool
}

// consumer reads the consumer's acknowledged offset and filter. A server
// older than the chat_ids or direction column has no such filter.
func (w *Watcher) consumer(ctx context.Context, conn *sql.Conn) (consumerState, error) {
	cs := consumerState{direction: DirectionBoth, directed: true}
	const q = `FROM feed_consumers WHERE account = ? AND name = ?`
	err := conn.QueryRowContext(ctx, `SELECT committed, chat_ids, direction `+q, w.Account, w.Consumer).
		Scan(&cs.committed, &cs.chats, &cs.direction)
	if isNoColumn(err) {
		cs.directed = false
		err = conn.QueryRowContext(ctx, `SELECT committed, chat_ids `+q, w.Account, w.Consumer).Scan(&cs.committed, &cs.chats)
	}
	if isNoColumn(err) {
		err = conn.QueryRowContext(ctx, `SELECT committed `+q, w.Account, w.Consumer).Scan(&cs.committed)
	}
	switch {
	case errors.Is(err, sql.ErrNoRows), isNoTable(err):
		return consumerState{direction: DirectionBoth, directed: cs.directed}, nil
	case err != nil:
		return consumerState{}, err
	}
	cs.found = true
	return cs, nil
}

func (w *Watcher) counter(ctx context.Context, conn *sql.Conn) (int64, error) {
	var v int64
	err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM feed_counter WHERE account = ?`, w.Account).Scan(&v)
	if isNoTable(err) {
		return 0, nil // the server has not watched anything yet
	}
	return v, err
}

// writeError marks a failed write to Out, which ends Run.
type writeError struct{ err error }

func (e writeError) Error() string { return e.err.Error() }

// emit writes one line in a single Write: on a pipe, writes up to
// PIPE_BUF (512 bytes on macOS) are atomic, and a Notice is under 110.
func (w *Watcher) emit(n Notice) error {
	line, err := json.Marshal(n)
	if err != nil {
		return err
	}
	if _, err := w.Out.Write(append(line, '\n')); err != nil {
		return writeError{err}
	}
	return nil
}

// isNoTable reports a missing table, which means the server has not
// created the feed yet. SQLite gives it the generic SQLITE_ERROR code,
// shared with syntax errors and others, so the message is checked too;
// SQLITE_BUSY, I/O errors and anything not from SQLite never match.
func isNoTable(err error) bool { return isSQLiteError(err, "no such table") }

// isNoColumn reports a column a server of an older version did not create.
func isNoColumn(err error) bool { return isSQLiteError(err, "no such column") }

func isSQLiteError(err error, msg string) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code() == sqlite3.SQLITE_ERROR &&
		strings.Contains(se.Error(), msg)
}
