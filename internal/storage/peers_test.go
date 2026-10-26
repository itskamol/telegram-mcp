package storage

import (
	"context"
	"errors"
	"testing"
)

func TestPeersCache(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t)
	p := NewPeers(db, "alice")

	if _, err := p.Get(ctx, KindUser, 1); !errors.Is(err, ErrPeerNotCached) {
		t.Fatalf("empty cache: %v", err)
	}
	if err := p.Put(ctx, []Peer{
		{Kind: KindUser, ID: 1, AccessHash: 111, Username: "Kamol", Title: "Kamol"},
		{Kind: KindChannel, ID: 2, AccessHash: 222, Title: "News"},
	}); err != nil {
		t.Fatal(err)
	}

	// A "min" entity without access_hash must not erase the known one.
	if err := p.Put(ctx, []Peer{{Kind: KindUser, ID: 1, Username: "kamol_new", Title: "K"}}); err != nil {
		t.Fatal(err)
	}
	got, err := p.Get(ctx, KindUser, 1)
	if err != nil || got.AccessHash != 111 || got.Title != "K" {
		t.Fatalf("after min update: %+v, %v", got, err)
	}

	if got, err := p.ByUsername(ctx, "KAMOL_NEW"); err != nil || got.ID != 1 {
		t.Fatalf("by username: %+v, %v", got, err)
	}
	if _, err := p.ByUsername(ctx, "kamol"); !errors.Is(err, ErrPeerNotCached) {
		t.Fatal("old username still resolves")
	}

	// Caches are per account.
	if _, err := NewPeers(db, "bob").Get(ctx, KindChannel, 2); !errors.Is(err, ErrPeerNotCached) {
		t.Fatal("bob sees alice's peers")
	}
}
