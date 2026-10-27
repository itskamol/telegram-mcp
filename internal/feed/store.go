// Package feed buffers new messages of watched chats so an agent can pull
// them later: Telegram pushes updates, MCP tools can only be pulled.
//
// The buffer keeps the latest version of each message (an UPSERT table,
// not an event log). Every insert or newer edit gets the next value of a
// per-account counter, so consumers read "everything with seq > my
// offset". The counter never goes back: deriving seq from MAX(seq)+1
// would hand a deleted message's seq to the next one, which a consumer
// that already acknowledged that seq would never see.
package feed

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/itskamol/telegram-mcp-go/internal/storage"
	"github.com/itskamol/telegram-mcp-go/internal/tele"
)

const schema = `
CREATE TABLE IF NOT EXISTS feed_messages (
	account     TEXT    NOT NULL,
	chat_id     INTEGER NOT NULL,
	message_id  INTEGER NOT NULL,
	peer_kind   TEXT    NOT NULL,
	topic_id    INTEGER NOT NULL DEFAULT 0,
	date_unix   INTEGER NOT NULL,
	edit_date   INTEGER NOT NULL DEFAULT 0,
	seq         INTEGER NOT NULL,
	body        TEXT    NOT NULL,
	out         INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (account, chat_id, message_id)
);
CREATE INDEX IF NOT EXISTS feed_messages_seq ON feed_messages (account, seq);
CREATE TABLE IF NOT EXISTS feed_counter (
	account        TEXT PRIMARY KEY,
	seq            INTEGER NOT NULL DEFAULT 0,
	pruned_through INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS feed_consumers (
	account   TEXT    NOT NULL,
	name      TEXT    NOT NULL,
	committed INTEGER NOT NULL DEFAULT 0,
	delivered INTEGER NOT NULL DEFAULT 0,
	chat_ids  TEXT    NOT NULL DEFAULT '',
	direction TEXT    NOT NULL DEFAULT 'both',
	PRIMARY KEY (account, name)
);
CREATE TABLE IF NOT EXISTS feed_dead_letters (
	account    TEXT    NOT NULL,
	chat_id    INTEGER NOT NULL,
	message_id INTEGER NOT NULL,
	error      TEXT    NOT NULL,
	at         INTEGER NOT NULL
);
`

// Item is one buffered message as get_new_messages returns it.
type Item struct {
	Seq int64 `json:"seq" jsonschema:"position in the buffer, one counter across all chats and edits; not a message id"`
	tele.Message
}

// Directions a consumer can read: messages to this account, its own
// messages, or both.
const (
	DirectionIn   = "in"
	DirectionOut  = "out"
	DirectionBoth = "both"
)

// Filter is what a consumer reads of the buffer.
type Filter struct {
	ChatIDs   []int64 `json:"chat_ids" jsonschema:"chats this consumer reads; empty means every watched chat"`
	Direction string  `json:"direction" jsonschema:"in (messages to this account), out (its own) or both"`
}

// Batch is one get_new_messages answer.
type Batch struct {
	Items   []Item `json:"messages"`
	Cursor  int64  `json:"cursor" jsonschema:"pass as ack_cursor once these messages are handled"`
	HasMore bool   `json:"has_more"`
	// Gap means messages this consumer had not acknowledged were pruned
	// by retention before it read them.
	Gap    bool   `json:"gap,omitempty"`
	Filter Filter `json:"filter" jsonschema:"what this consumer reads, as set with set_feed_filter"`
}

// Store is the per-account buffer.
type Store struct {
	db      *storage.DB
	account string
}

// ErrAckAhead means an ack_cursor larger than anything delivered.
var ErrAckAhead = errors.New("ack_cursor was never delivered to this consumer; pass the cursor of the previous answer")

// added lists columns that older versions did not create, with the
// statement that fills them in for existing rows.
var added = []struct{ table, column, ddl, fill string }{
	{"feed_consumers", "chat_ids", `ALTER TABLE feed_consumers ADD COLUMN chat_ids TEXT NOT NULL DEFAULT ''`, ""},
	// Bodies hold out as a JSON boolean that is left out when false:
	// json_extract reads true as 1 and a missing field as NULL.
	{"feed_messages", "out", `ALTER TABLE feed_messages ADD COLUMN out INTEGER NOT NULL DEFAULT 0`,
		`UPDATE feed_messages SET out = COALESCE(json_extract(body, '$.out'), 0)`},
	// After out: watch takes a direction column as proof that out exists.
	{"feed_consumers", "direction", `ALTER TABLE feed_consumers ADD COLUMN direction TEXT NOT NULL DEFAULT 'both'`, ""},
}

// NewStore creates the tables if needed and adds columns that older
// versions did not have.
func NewStore(ctx context.Context, db *storage.DB, account string) (*Store, error) {
	if _, err := db.SQL.ExecContext(ctx, schema); err != nil {
		return nil, fmt.Errorf("feed schema: %w", err)
	}
	for _, c := range added {
		if err := addColumn(ctx, db, c.table, c.column, c.ddl, c.fill); err != nil {
			return nil, fmt.Errorf("feed schema: add %s.%s: %w", c.table, c.column, err)
		}
	}
	return &Store{db: db, account: account}, nil
}

// addColumn adds and fills a column in one transaction, so a crash can
// never leave the column added but unfilled.
func addColumn(ctx context.Context, db *storage.DB, table, column, ddl, fill string) error {
	tx, err := db.SQL.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	var has bool
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) > 0 FROM pragma_table_info(?) WHERE name = ?`,
		table, column).Scan(&has); err != nil || has {
		return err
	}
	if _, err := tx.ExecContext(ctx, ddl); err != nil {
		return err
	}
	if fill != "" {
		if _, err := tx.ExecContext(ctx, fill); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// inChats is the SQL condition for a consumer's chat filter, stored as a
// JSON array of chat ids; empty means every buffered chat. It takes the
// filter twice.
const inChats = `(? = '' OR chat_id IN (SELECT value FROM json_each(?)))`

// inDirection is the SQL condition for a consumer's direction. It takes
// the direction twice.
const inDirection = `(? = 'both' OR out = (? = 'out'))`

// SetFilter changes what a consumer reads and returns the result with the
// consumer's acknowledged offset. A nil chats or an empty direction leaves
// that part as it is; an empty chats list means every buffered chat.
//
// A new filter applies after the acknowledged offset only: messages not
// acknowledged yet are delivered under it, acknowledged ones never come
// back, even if the filter skipped them. One offset serves every chat and
// direction, so bringing those back would mean delivering everything
// after them again.
func (s *Store) SetFilter(ctx context.Context, consumer string, chats *[]int64, direction string) (Filter, int64, error) {
	if err := CheckDirection(direction); err != nil {
		return Filter{}, 0, err
	}
	tx, err := s.db.SQL.BeginTx(ctx, nil)
	if err != nil {
		return Filter{}, 0, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO feed_consumers (account, name) VALUES (?, ?)`,
		s.account, consumer); err != nil {
		return Filter{}, 0, err
	}
	if chats != nil {
		list := ""
		if len(*chats) > 0 {
			b, err := json.Marshal(*chats)
			if err != nil {
				return Filter{}, 0, err
			}
			list = string(b)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE feed_consumers SET chat_ids = ? WHERE account = ? AND name = ?`,
			list, s.account, consumer); err != nil {
			return Filter{}, 0, err
		}
	}
	if direction != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE feed_consumers SET direction = ? WHERE account = ? AND name = ?`,
			direction, s.account, consumer); err != nil {
			return Filter{}, 0, err
		}
	}
	var committed int64
	var list, dir string
	if err := tx.QueryRowContext(ctx, `SELECT committed, chat_ids, direction FROM feed_consumers WHERE account = ? AND name = ?`,
		s.account, consumer).Scan(&committed, &list, &dir); err != nil {
		return Filter{}, 0, err
	}
	f, err := filterOf(list, dir)
	if err != nil {
		return Filter{}, 0, err
	}
	return f, committed, tx.Commit()
}

// CheckDirection accepts a direction, or "" for "leave it as it is".
func CheckDirection(d string) error {
	switch d {
	case "", DirectionIn, DirectionOut, DirectionBoth:
		return nil
	}
	return fmt.Errorf("direction must be %s, %s or %s", DirectionIn, DirectionOut, DirectionBoth)
}

// filterOf decodes a consumer's stored chat list and direction.
func filterOf(chats, direction string) (Filter, error) {
	f := Filter{ChatIDs: []int64{}, Direction: direction}
	if chats == "" {
		return f, nil
	}
	if err := json.Unmarshal([]byte(chats), &f.ChatIDs); err != nil {
		return Filter{}, fmt.Errorf("consumer chat_ids: %w", err)
	}
	return f, nil
}

// Upsert stores a new message or a newer edit of one and reports whether
// the buffer changed. Redelivery of the same version changes nothing and
// takes no seq.
func (s *Store) Upsert(ctx context.Context, peerKind string, topicID int, m tele.Message, dateUnix int) (bool, error) {
	body, err := json.Marshal(m)
	if err != nil {
		return false, err
	}
	tx, err := s.db.SQL.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	next, err := s.counter(ctx, tx)
	if err != nil {
		return false, err
	}
	next++
	res, err := tx.ExecContext(ctx, `
		INSERT INTO feed_messages (account, chat_id, message_id, peer_kind, topic_id, date_unix, edit_date, seq, body, out)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (account, chat_id, message_id) DO UPDATE
			SET body = excluded.body, edit_date = excluded.edit_date, seq = excluded.seq
			WHERE excluded.edit_date > feed_messages.edit_date`,
		s.account, m.ChatID, m.ID, peerKind, topicID, dateUnix, m.EditDate, next, string(body), m.Out)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil // same version again: leave the counter alone
	}
	if _, err := tx.ExecContext(ctx, `UPDATE feed_counter SET seq = ? WHERE account = ?`, next, s.account); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *Store) counter(ctx context.Context, tx *sql.Tx) (int64, error) {
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO feed_counter (account) VALUES (?)`, s.account); err != nil {
		return 0, err
	}
	var seq int64
	err := tx.QueryRowContext(ctx, `SELECT seq FROM feed_counter WHERE account = ?`, s.account).Scan(&seq)
	return seq, err
}

// DeleteChannel removes messages of one channel or supergroup.
func (s *Store) DeleteChannel(ctx context.Context, chatID int64, ids []int) (bool, error) {
	return s.delete(ctx, `chat_id = ?`, chatID, ids)
}

// DeleteNonChannel removes messages of private chats and basic groups.
// Their ids are unique per account, so Telegram names no chat; channels
// number messages per channel and must not be touched by these ids.
func (s *Store) DeleteNonChannel(ctx context.Context, ids []int) (bool, error) {
	return s.delete(ctx, `peer_kind <> ?`, storage.KindChannel, ids)
}

func (s *Store) delete(ctx context.Context, where string, arg any, ids []int) (bool, error) {
	if len(ids) == 0 {
		return false, nil
	}
	tx, err := s.db.SQL.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	var removed int64
	for _, id := range ids {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM feed_messages WHERE account = ? AND `+where+` AND message_id = ?`, s.account, arg, id)
		if err != nil {
			return false, err
		}
		n, _ := res.RowsAffected()
		removed += n
	}
	return removed > 0, tx.Commit()
}

// Fetch returns up to limit messages after the consumer's committed
// offset. ack (the cursor of the previous answer, once processed) is
// committed first; without it the same messages come again, so a session
// that died mid-batch loses nothing (at-least-once).
func (s *Store) Fetch(ctx context.Context, consumer string, ack int64, limit int) (Batch, error) {
	tx, err := s.db.SQL.BeginTx(ctx, nil)
	if err != nil {
		return Batch{}, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	var committed, delivered, pruned int64
	chats, direction := "", DirectionBoth
	err = tx.QueryRowContext(ctx, `SELECT committed, delivered, chat_ids, direction FROM feed_consumers WHERE account = ? AND name = ?`,
		s.account, consumer).Scan(&committed, &delivered, &chats, &direction)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Batch{}, err
	}
	filter, err := filterOf(chats, direction)
	if err != nil {
		return Batch{}, err
	}
	if ack > delivered {
		return Batch{}, ErrAckAhead
	}
	committed = max(committed, ack)
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(pruned_through), 0) FROM feed_counter WHERE account = ?`,
		s.account).Scan(&pruned); err != nil {
		return Batch{}, err
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT seq, body FROM feed_messages WHERE account = ? AND seq > ? AND `+inChats+` AND `+inDirection+`
		ORDER BY seq LIMIT ?`,
		s.account, committed, chats, chats, direction, direction, limit+1)
	if err != nil {
		return Batch{}, err
	}
	b := Batch{Cursor: committed, Gap: committed < pruned, Filter: filter}
	for rows.Next() {
		var it Item
		var body string
		if err := rows.Scan(&it.Seq, &body); err != nil {
			rows.Close()
			return Batch{}, err
		}
		if err := json.Unmarshal([]byte(body), &it.Message); err != nil {
			rows.Close()
			return Batch{}, err
		}
		b.Items = append(b.Items, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Batch{}, err
	}
	if len(b.Items) > limit {
		b.Items, b.HasMore = b.Items[:limit], true
	}
	if len(b.Items) > 0 {
		b.Cursor = b.Items[len(b.Items)-1].Seq
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO feed_consumers (account, name, committed, delivered) VALUES (?, ?, ?, ?)
		ON CONFLICT (account, name) DO UPDATE SET committed = excluded.committed, delivered = excluded.delivered`,
		s.account, consumer, committed, max(delivered, b.Cursor)); err != nil {
		return Batch{}, err
	}
	if b.Items == nil {
		b.Items = []Item{}
	}
	return b, tx.Commit()
}

// Prune drops messages older than cutoff and remembers the highest seq it
// dropped, so a consumer behind it is told about the gap.
func (s *Store) Prune(ctx context.Context, cutoff time.Time) (int64, error) {
	tx, err := s.db.SQL.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	var top sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(seq) FROM feed_messages WHERE account = ? AND date_unix < ?`,
		s.account, cutoff.Unix()).Scan(&top); err != nil {
		return 0, err
	}
	if !top.Valid {
		return 0, nil
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM feed_messages WHERE account = ? AND date_unix < ?`, s.account, cutoff.Unix())
	if err != nil {
		return 0, err
	}
	if _, err := s.counter(ctx, tx); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE feed_counter SET pruned_through = MAX(pruned_through, ?) WHERE account = ?`,
		top.Int64, s.account); err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, tx.Commit()
}

// DeadLetter records a message that could not be buffered, so the loss is
// visible instead of silent.
func (s *Store) DeadLetter(ctx context.Context, chatID int64, messageID int, cause error) error {
	_, err := s.db.SQL.ExecContext(ctx,
		`INSERT INTO feed_dead_letters (account, chat_id, message_id, error, at) VALUES (?, ?, ?, ?, ?)`,
		s.account, chatID, messageID, cause.Error(), time.Now().Unix())
	return err
}
