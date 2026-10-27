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
	committed, found, err := w.committed(ctx, conn)
	if err != nil {
		return cursor{}, err
	}
	if !found && original {
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
	rows, err := conn.QueryContext(ctx, `
		SELECT chat_id, MIN(message_id), MAX(message_id), COUNT(*), MAX(seq)
		FROM feed_messages WHERE account = ? AND seq > ?
		GROUP BY chat_id ORDER BY MIN(seq)`, w.Account, c.hw)
	if isNoTable(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer rows.Close() // no read transaction stays open between polls
	top := c.hw
	for rows.Next() {
		var n Notice
		var maxSeq int64
		if err := rows.Scan(&n.ChatID, &n.FirstID, &n.LastID, &n.Count, &maxSeq); err != nil {
			return err
		}
		if err := w.emit(n); err != nil {
			return err
		}
		top = max(top, maxSeq)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	c.hw = top
	return nil
}

// restale rolls the high-water mark back to the consumer's offset when
// announced messages stay unacknowledged for Stale, so they are announced
// again in the same Notice format.
func (w *Watcher) restale(ctx context.Context, conn *sql.Conn, c *cursor) error {
	committed, found, err := w.committed(ctx, conn)
	if err != nil {
		return err
	}
	if !found {
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

func (w *Watcher) committed(ctx context.Context, conn *sql.Conn) (int64, bool, error) {
	var v int64
	err := conn.QueryRowContext(ctx, `SELECT committed FROM feed_consumers WHERE account = ? AND name = ?`,
		w.Account, w.Consumer).Scan(&v)
	switch {
	case errors.Is(err, sql.ErrNoRows), isNoTable(err):
		return 0, false, nil
	case err != nil:
		return 0, false, err
	}
	return v, true, nil
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
// PIPE_BUF (512 bytes on macOS) are atomic, and a Notice is under 100.
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

func isNoTable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table")
}
