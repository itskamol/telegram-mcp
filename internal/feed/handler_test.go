package feed

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/gotd/td/telegram/updates"
	"github.com/gotd/td/tg"

	"github.com/itskamol/telegram-mcp-go/internal/storage"
	"github.com/itskamol/telegram-mcp-go/internal/tele"
)

const (
	groupPlain = 1889276137 // -1001889276137 without the prefix
	noteTopic  = 33
)

type fixture struct {
	db    *storage.DB
	store *Store
	h     *Handler
}

func newFixture(t *testing.T, watch Watch) fixture {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store, err := NewStore(context.Background(), db, "default")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(store, NewNotifier(), watch, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.Service = &tele.Service{Peers: storage.NewPeers(db, "default"), SelfID: 1}
	h.Backoff = time.Millisecond
	return fixture{db, store, h}
}

func channelMsg(id int, reply tg.MessageReplyHeaderClass, text string) *tg.Message {
	m := &tg.Message{ID: id, PeerID: &tg.PeerChannel{ChannelID: groupPlain}, Message: text, Date: int(time.Now().Unix())}
	if reply != nil {
		m.ReplyTo = reply
	}
	return m
}

func stored(t *testing.T, f fixture) map[int]bool {
	t.Helper()
	b, err := f.store.Fetch(context.Background(), "test", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	out := map[int]bool{}
	for _, it := range b.Items {
		out[it.ID] = true
	}
	return out
}

func TestTopicFilterFailsClosed(t *testing.T) {
	f := newFixture(t, Watch{group: {1}}) // General only
	ctx := context.Background()

	f.h.message(ctx, tg.Entities{}, channelMsg(1, nil, "General, no header"))
	f.h.message(ctx, tg.Entities{}, channelMsg(2, &tg.MessageReplyHeader{ReplyToMsgID: 1}, "reply inside General"))
	f.h.message(ctx, tg.Entities{}, channelMsg(3, &tg.MessageReplyHeader{ForumTopic: true, ReplyToMsgID: noteTopic}, "secret in Note"))
	f.h.message(ctx, tg.Entities{}, channelMsg(4, &tg.MessageReplyHeader{ForumTopic: true, ReplyToMsgID: 50, ReplyToTopID: noteTopic}, "reply in Note"))
	f.h.message(ctx, tg.Entities{}, channelMsg(5, &tg.MessageReplyStoryHeader{}, "unknown header kind"))
	other := &tg.Message{ID: 6, PeerID: &tg.PeerChannel{ChannelID: 42}, Message: "unwatched chat", Date: 1}
	f.h.message(ctx, tg.Entities{}, other)
	f.h.message(ctx, tg.Entities{}, &tg.MessageService{ID: 7, PeerID: &tg.PeerChannel{ChannelID: groupPlain}})

	got := stored(t, f)
	want := map[int]bool{1: true, 2: true}
	for id := range got {
		if !want[id] {
			t.Errorf("message %d leaked into the buffer", id)
		}
	}
	for id := range want {
		if !got[id] {
			t.Errorf("General message %d not buffered", id)
		}
	}
}

func TestWholeChatWatchKeepsTopics(t *testing.T) {
	f := newFixture(t, Watch{group: nil})
	f.h.message(context.Background(), tg.Entities{}, channelMsg(3, &tg.MessageReplyHeader{ForumTopic: true, ReplyToMsgID: noteTopic}, "x"))
	b, _ := f.store.Fetch(context.Background(), "t", 0, 10)
	if len(b.Items) != 1 || b.Items[0].TopicID != noteTopic {
		t.Fatalf("whole-chat watch: %+v", b.Items)
	}
}

func TestPublishOnlyOnChange(t *testing.T) {
	f := newFixture(t, Watch{group: nil})
	ready := f.h.Notify.Ready()
	f.h.message(context.Background(), tg.Entities{}, channelMsg(1, nil, "a"))
	select {
	case <-ready:
	default:
		t.Fatal("new message did not wake waiters")
	}
	ready = f.h.Notify.Ready()
	f.h.message(context.Background(), tg.Entities{}, channelMsg(1, nil, "a")) // redelivery
	select {
	case <-ready:
		t.Fatal("redelivery woke waiters")
	default:
	}
}

func TestFatalBlocksHandler(t *testing.T) {
	f := newFixture(t, Watch{group: nil})
	f.h.Retries = 2
	f.db.Close() // every write, the dead letter included, now fails

	returned := make(chan struct{})
	go func() {
		f.h.message(context.Background(), tg.Entities{}, channelMsg(1, nil, "a"))
		close(returned)
	}()
	select {
	case err := <-f.h.Fatal:
		if err == nil {
			t.Fatal("nil fatal error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no fatal error reported")
	}
	select {
	case <-returned:
		t.Fatal("handler returned after a fatal error: gotd would persist this pts")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestShutdownDoesNotPersistPts(t *testing.T) {
	f := newFixture(t, nil)
	st, err := NewState(context.Background(), f.db, storage.NewPeers(f.db, "default"), "default")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := st.SetState(ctx, 0, updates.State{Pts: 100, Qts: 1, Date: 1, Seq: 1}); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := st.SetPts(cancelled, 0, 101); err == nil {
		t.Fatal("SetPts accepted a cancelled context")
	}
	got, _, _ := st.GetState(ctx, 0)
	if got.Pts != 100 {
		t.Fatalf("pts = %d after shutdown write, want 100", got.Pts)
	}
	// ForEachChannels may call SetChannelPts from its callback.
	_ = st.SetChannelPts(ctx, 0, 7, 5)
	done := make(chan error, 1)
	go func() {
		done <- st.ForEachChannels(ctx, 0, func(ctx context.Context, id int64, pts int) error {
			return st.SetChannelPts(ctx, 0, id, pts+1)
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ForEachChannels deadlocked on the single connection")
	}
}

func TestDeletes(t *testing.T) {
	f := newFixture(t, Watch{group: nil})
	ctx := context.Background()
	f.h.message(ctx, tg.Entities{}, channelMsg(1, nil, "a"))
	f.h.message(ctx, tg.Entities{}, channelMsg(2, nil, "b"))
	d := tg.NewUpdateDispatcher()
	f.h.Register(d)
	if err := d.Handle(ctx, &tg.Updates{Updates: []tg.UpdateClass{
		&tg.UpdateDeleteChannelMessages{ChannelID: groupPlain, Messages: []int{1}},
		&tg.UpdateDeleteMessages{Messages: []int{2}}, // private-chat ids: must not touch the channel
	}}); err != nil {
		t.Fatal(err)
	}
	if got := stored(t, f); got[1] || !got[2] {
		t.Fatalf("after deletes: %v", got)
	}
}
