package tele

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/gotd/td/tg"

	"github.com/itskamol/telegram-mcp-go/internal/storage"
)

func TestLookupBareGroupID(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	peers := storage.NewPeers(db, "default")
	if err := peers.Put(ctx, []storage.Peer{
		{Kind: storage.KindChannel, ID: 1889276137, AccessHash: 9, Title: "Test group"},
		{Kind: storage.KindChat, ID: 4567, Title: "Old group"},
		{Kind: storage.KindUser, ID: 42, AccessHash: 1, Title: "Ali"},
		{Kind: storage.KindChannel, ID: 42, AccessHash: 2, Title: "Channel 42"},
	}); err != nil {
		t.Fatal(err)
	}
	s := &Service{Peers: peers}

	cases := map[int64]string{
		1889276137: storage.KindChannel, // quoted without -100
		4567:       storage.KindChat,    // quoted without the minus
		42:         storage.KindUser,    // a user wins over a same-id channel
	}
	for id, want := range cases {
		p, err := s.lookup(ctx, storage.KindUser, id)
		if err != nil || p.Kind != want {
			t.Errorf("%d: got %q, %v; want %s", id, p.Kind, err, want)
		}
	}
	if _, err := s.lookup(ctx, storage.KindUser, 777); !errors.Is(err, storage.ErrPeerNotCached) {
		t.Errorf("unknown id: %v", err)
	}
	// An explicit -100 id never falls back to a user.
	if _, err := s.lookup(ctx, storage.KindChannel, 4567); !errors.Is(err, storage.ErrPeerNotCached) {
		t.Errorf("channel lookup fell back: %v", err)
	}
}

// Telegram pushes private messages as updateShortMessage, which gotd turns
// into a message with no users attached; the sender's name must then come
// from the peer cache, or get_new_messages loses from_name.
func TestConvertNamesSenderFromCache(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	peers := storage.NewPeers(db, "default")
	if err := peers.Put(ctx, []storage.Peer{{Kind: storage.KindUser, ID: 42, AccessHash: 1, Title: "Ahmadjon"}}); err != nil {
		t.Fatal(err)
	}
	s := &Service{Peers: peers, SelfID: 1}

	short := &tg.Message{ID: 7, PeerID: &tg.PeerUser{UserID: 42}, Message: "hello"}
	short.SetFromID(&tg.PeerUser{UserID: 42})
	m, ok, err := s.Convert(ctx, short, tg.Entities{})
	if err != nil || !ok {
		t.Fatalf("convert: %v %v", ok, err)
	}
	if m.FromID != 42 || m.FromName != "Ahmadjon" {
		t.Errorf("from = %d %q, want 42 Ahmadjon", m.FromID, m.FromName)
	}

	unknown := &tg.Message{ID: 8, PeerID: &tg.PeerUser{UserID: 99}, Message: "hi"}
	if m, _, err := s.Convert(ctx, unknown, tg.Entities{}); err != nil || m.FromID != 99 || m.FromName != "" {
		t.Errorf("unknown sender: %+v %v", m, err)
	}
}
