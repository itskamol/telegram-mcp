package feed

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/itskamol/telegram-mcp-go/internal/storage"
	"github.com/itskamol/telegram-mcp-go/internal/tele"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "feed.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s, err := NewStore(context.Background(), db, "default")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func msg(chat int64, id, editDate int, text string) tele.Message {
	return tele.Message{ChatID: chat, ID: id, Text: text, EditDate: editDate, Edited: editDate != 0}
}

func put(t *testing.T, s *Store, kind string, m tele.Message) bool {
	t.Helper()
	changed, err := s.Upsert(context.Background(), kind, 0, m, int(time.Now().Unix()))
	if err != nil {
		t.Fatal(err)
	}
	return changed
}

func seqOf(t *testing.T, s *Store, chat int64, id int) int64 {
	t.Helper()
	var seq int64
	if err := s.db.SQL.QueryRow(`SELECT seq FROM feed_messages WHERE chat_id = ? AND message_id = ?`, chat, id).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	return seq
}

const group = -1001889276137

func TestUpsertIdempotentAndEdits(t *testing.T) {
	s := newStore(t)
	if !put(t, s, storage.KindChannel, msg(group, 10, 0, "salom")) {
		t.Fatal("first insert reported no change")
	}
	first := seqOf(t, s, group, 10)

	// Redelivery of the same version (at-least-once from gotd).
	if put(t, s, storage.KindChannel, msg(group, 10, 0, "salom")) {
		t.Error("redelivery reported a change")
	}
	if seqOf(t, s, group, 10) != first {
		t.Error("redelivery moved seq")
	}

	// A newer edit gets a new seq so consumers see it again.
	if !put(t, s, storage.KindChannel, msg(group, 10, 1000, "salom (tahrir)")) {
		t.Fatal("edit reported no change")
	}
	edited := seqOf(t, s, group, 10)
	if edited <= first {
		t.Errorf("edit seq %d not after %d", edited, first)
	}
	// An older edit arriving late must not roll the text back.
	if put(t, s, storage.KindChannel, msg(group, 10, 500, "eski")) {
		t.Error("older edit applied")
	}
	// The counter did not move for the ignored writes.
	put(t, s, storage.KindChannel, msg(group, 11, 0, "keyingi"))
	if got := seqOf(t, s, group, 11); got != edited+1 {
		t.Errorf("next seq %d, want %d: ignored writes consumed seq numbers", got, edited+1)
	}
}

func TestDeletedMessageSeqIsNeverReused(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	put(t, s, storage.KindChannel, msg(group, 1, 0, "a"))
	put(t, s, storage.KindChannel, msg(group, 2, 0, "b"))
	b, _ := s.Fetch(ctx, "c", 0, 10)
	if _, err := s.Fetch(ctx, "c", b.Cursor, 10); err != nil { // ack both
		t.Fatal(err)
	}
	if _, err := s.DeleteChannel(ctx, group, []int{2}); err != nil { // the max seq goes away
		t.Fatal(err)
	}
	put(t, s, storage.KindChannel, msg(group, 3, 0, "c"))
	got, _ := s.Fetch(ctx, "c", 0, 10)
	if len(got.Items) != 1 || got.Items[0].ID != 3 {
		t.Fatalf("new message after deleting the newest one was not delivered: %+v", got.Items)
	}
}

func TestFetchAtLeastOnce(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	for i := 1; i <= 5; i++ {
		put(t, s, storage.KindChannel, msg(group, i, 0, "m"))
	}

	b1, err := s.Fetch(ctx, "digest", 0, 3)
	if err != nil || len(b1.Items) != 3 || !b1.HasMore {
		t.Fatalf("first batch: %+v %v", b1, err)
	}
	// The agent crashed before processing: a new session calls without ack
	// and must get the very same batch again.
	again, _ := s.Fetch(ctx, "digest", 0, 3)
	if again.Cursor != b1.Cursor || again.Items[0].Seq != b1.Items[0].Seq {
		t.Fatalf("unacknowledged batch not redelivered: %+v", again)
	}
	// Processed: ack with the cursor and get the rest.
	b2, _ := s.Fetch(ctx, "digest", b1.Cursor, 3)
	if len(b2.Items) != 2 || b2.HasMore || b2.Items[0].ID != 4 {
		t.Fatalf("second batch: %+v", b2)
	}
	// Acks never move backwards.
	b3, _ := s.Fetch(ctx, "digest", 1, 3)
	if len(b3.Items) != 2 || b3.Items[0].ID != 4 {
		t.Fatalf("stale ack rewound the consumer: %+v", b3)
	}
	// Acknowledging something never delivered is refused.
	if _, err := s.Fetch(ctx, "digest", 999, 3); !errors.Is(err, ErrAckAhead) {
		t.Fatalf("ack ahead: %v", err)
	}
	// Consumers are independent.
	other, _ := s.Fetch(ctx, "mentions", 0, 10)
	if len(other.Items) != 5 {
		t.Fatalf("second consumer saw %d", len(other.Items))
	}
}

func TestDeleteNonChannelSparesSupergroups(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	put(t, s, storage.KindChannel, msg(group, 42, 0, "supergroup 42"))
	put(t, s, storage.KindUser, msg(777, 42, 0, "private 42"))

	if _, err := s.DeleteNonChannel(ctx, []int{42}); err != nil {
		t.Fatal(err)
	}
	b, _ := s.Fetch(ctx, "c", 0, 10)
	if len(b.Items) != 1 || b.Items[0].ChatID != group {
		t.Fatalf("delete of private message 42 hit the supergroup: %+v", b.Items)
	}
}

func TestPruneReportsGap(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	old := int(time.Now().Add(-10 * 24 * time.Hour).Unix())
	if _, err := s.Upsert(ctx, storage.KindChannel, 0, msg(group, 1, 0, "old"), old); err != nil {
		t.Fatal(err)
	}
	put(t, s, storage.KindChannel, msg(group, 2, 0, "new"))
	if n, err := s.Prune(ctx, time.Now().Add(-7*24*time.Hour)); err != nil || n != 1 {
		t.Fatalf("prune: %d %v", n, err)
	}
	b, _ := s.Fetch(ctx, "late", 0, 10)
	if !b.Gap || len(b.Items) != 1 {
		t.Fatalf("consumer behind retention: %+v", b)
	}
	b, _ = s.Fetch(ctx, "late", b.Cursor, 10)
	if b.Gap {
		t.Error("gap reported after catching up")
	}
}

func TestConsumerChats(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	const other = -1001234
	put(t, s, storage.KindChannel, msg(group, 1, 0, "a"))
	put(t, s, storage.KindChannel, msg(other, 2, 0, "b"))
	put(t, s, storage.KindChannel, msg(group, 3, 0, "c"))

	if _, _, err := s.SetFilter(ctx, "pos", &[]int64{group}, ""); err != nil {
		t.Fatal(err)
	}
	b, err := s.Fetch(ctx, "pos", 0, 10)
	if err != nil || len(b.Items) != 2 || b.Items[0].ID != 1 || b.Items[1].ID != 3 {
		t.Fatalf("filtered batch: %+v %v", b, err)
	}
	// The filter is remembered and keeps the offset.
	b, _ = s.Fetch(ctx, "pos", b.Cursor, 10)
	if len(b.Items) != 0 {
		t.Fatalf("after ack: %+v", b.Items)
	}
	// Other consumers still read everything.
	if all, _ := s.Fetch(ctx, "all", 0, 10); len(all.Items) != 3 {
		t.Fatalf("unfiltered consumer saw %d", len(all.Items))
	}
	// Back to all chats: nothing before the acknowledged offset returns.
	if _, _, err := s.SetFilter(ctx, "pos", &[]int64{}, ""); err != nil {
		t.Fatal(err)
	}
	put(t, s, storage.KindChannel, msg(other, 4, 0, "d"))
	b, _ = s.Fetch(ctx, "pos", 0, 10)
	if len(b.Items) != 1 || b.Items[0].ID != 4 {
		t.Fatalf("after clearing the filter: %+v", b.Items)
	}
}

func TestStoreAddsChatIDsToOldConsumers(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(filepath.Join(t.TempDir(), "old.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// The table as v0.5.0 created it, with a consumer in it.
	if _, err := db.SQL.Exec(`CREATE TABLE feed_consumers (
		account TEXT NOT NULL, name TEXT NOT NULL,
		committed INTEGER NOT NULL DEFAULT 0, delivered INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (account, name));
		INSERT INTO feed_consumers VALUES ('default', 'pos', 7, 7)`); err != nil {
		t.Fatal(err)
	}
	for range 2 { // and again, on the next start
		if _, err := NewStore(ctx, db, "default"); err != nil {
			t.Fatal(err)
		}
	}
	var committed int64
	var chats string
	if err := db.SQL.QueryRow(`SELECT committed, chat_ids FROM feed_consumers WHERE name = 'pos'`).Scan(&committed, &chats); err != nil {
		t.Fatal(err)
	}
	if committed != 7 || chats != "" {
		t.Fatalf("migrated consumer: committed %d, chat_ids %q", committed, chats)
	}
}

func TestConsumerDirection(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	own := msg(group, 2, 0, "own")
	own.Out = true
	put(t, s, storage.KindChannel, msg(group, 1, 0, "in"))
	put(t, s, storage.KindChannel, own)
	put(t, s, storage.KindChannel, msg(group, 3, 0, "in"))

	f, _, err := s.SetFilter(ctx, "bot", nil, DirectionIn)
	if err != nil || f.Direction != DirectionIn || len(f.ChatIDs) != 0 {
		t.Fatalf("set direction: %+v %v", f, err)
	}
	b, err := s.Fetch(ctx, "bot", 0, 10)
	if err != nil || len(b.Items) != 2 || b.Items[0].ID != 1 || b.Items[1].ID != 3 || b.Filter.Direction != DirectionIn {
		t.Fatalf("incoming batch: %+v %v", b, err)
	}
	// Changing the chats keeps the direction, and the other way round.
	if f, _, _ = s.SetFilter(ctx, "bot", &[]int64{group}, ""); f.Direction != DirectionIn {
		t.Fatalf("chat change reset direction: %+v", f)
	}
	f, committed, err := s.SetFilter(ctx, "bot", nil, DirectionBoth)
	if err != nil || len(f.ChatIDs) != 1 || committed != 0 {
		t.Fatalf("direction change: %+v %d %v", f, committed, err)
	}
	// Widening applies after the acknowledged offset only: the own message
	// skipped before cursor 3 does not come back; a new one does.
	b, _ = s.Fetch(ctx, "bot", b.Cursor, 10)
	if len(b.Items) != 0 {
		t.Fatalf("acknowledged messages came back: %+v", b.Items)
	}
	own.ID = 4
	put(t, s, storage.KindChannel, own)
	if b, _ = s.Fetch(ctx, "bot", 0, 10); len(b.Items) != 1 || b.Items[0].ID != 4 || !b.Items[0].Out {
		t.Fatalf("after widening: %+v", b.Items)
	}
	if _, _, err := s.SetFilter(ctx, "bot", nil, "incoming"); err == nil {
		t.Error("unknown direction accepted")
	}
}

func TestStoreFillsOutForOldMessages(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(filepath.Join(t.TempDir(), "old.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// The table as v0.6.0 created it, with an own and an incoming message.
	if _, err := db.SQL.Exec(`CREATE TABLE feed_messages (
		account TEXT NOT NULL, chat_id INTEGER NOT NULL, message_id INTEGER NOT NULL, peer_kind TEXT NOT NULL,
		topic_id INTEGER NOT NULL DEFAULT 0, date_unix INTEGER NOT NULL, edit_date INTEGER NOT NULL DEFAULT 0,
		seq INTEGER NOT NULL, body TEXT NOT NULL, PRIMARY KEY (account, chat_id, message_id));
		INSERT INTO feed_messages VALUES ('default', 5, 1, 'user', 0, 0, 0, 1, '{"id":1,"out":true}');
		INSERT INTO feed_messages VALUES ('default', 5, 2, 'user', 0, 0, 0, 2, '{"id":2}')`); err != nil {
		t.Fatal(err)
	}
	for range 2 { // and again, on the next start
		if _, err := NewStore(ctx, db, "default"); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := db.SQL.Query(`SELECT message_id, out FROM feed_messages ORDER BY message_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[int]int{}
	for rows.Next() {
		var id, out int
		if err := rows.Scan(&id, &out); err != nil {
			t.Fatal(err)
		}
		got[id] = out
	}
	if got[1] != 1 || got[2] != 0 {
		t.Fatalf("out after migration: %v", got)
	}
}
