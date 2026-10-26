package storage

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gotd/td/session"
	"github.com/zalando/go-keyring"

	"github.com/itskamol/telegram-mcp-go/internal/secret"
)

func openTemp(t *testing.T) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}

func TestSessionRoundTrip(t *testing.T) {
	keyring.MockInit()
	ctx := context.Background()
	db, path := openTemp(t)
	s := NewSession(db, secret.Keyring{}, "alice")

	if _, err := s.LoadSession(ctx); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("empty load: got %v, want ErrNotFound", err)
	}

	data := []byte(`{"Version":1,"Data":{"AuthKey":"c2VjcmV0LWF1dGgta2V5"}}`)
	if err := s.StoreSession(ctx, data); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("round trip: got %q", got)
	}

	// The plaintext must not reach the database file.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	wal, _ := os.ReadFile(path + "-wal")
	if bytes.Contains(raw, []byte("c2VjcmV0")) || bytes.Contains(wal, []byte("c2VjcmV0")) {
		t.Fatal("session plaintext found on disk")
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("db mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestSessionBoundToAccount(t *testing.T) {
	keyring.MockInit()
	ctx := context.Background()
	db, _ := openTemp(t)

	if err := NewSession(db, secret.Keyring{}, "alice").StoreSession(ctx, []byte("a")); err != nil {
		t.Fatal(err)
	}
	// Copy alice's ciphertext under bob, who has his own key: must not open.
	if _, err := NewSession(db, secret.Keyring{}, "bob").keys.Key("bob", true); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`INSERT INTO sessions SELECT 'bob', ciphertext, updated_at FROM sessions WHERE account='alice'`); err != nil {
		t.Fatal(err)
	}
	if _, err := NewSession(db, secret.Keyring{}, "bob").LoadSession(ctx); err == nil {
		t.Fatal("bob decrypted alice's session")
	}
}

func TestMissingKey(t *testing.T) {
	keyring.MockInit()
	ctx := context.Background()
	db, _ := openTemp(t)
	s := NewSession(db, secret.Keyring{}, "alice")
	if err := s.StoreSession(ctx, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := (secret.Keyring{}).Delete("alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadSession(ctx); !errors.Is(err, ErrKeyMissing) {
		t.Fatalf("got %v, want ErrKeyMissing", err)
	}
}

func TestDelete(t *testing.T) {
	keyring.MockInit()
	ctx := context.Background()
	db, _ := openTemp(t)
	s := NewSession(db, secret.Keyring{}, "alice")
	if err := s.StoreSession(ctx, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.Exists(ctx); ok {
		t.Fatal("session row survived Delete")
	}
	if _, err := (secret.Keyring{}).Key("alice", false); !errors.Is(err, secret.ErrNoKey) {
		t.Fatalf("key survived Delete: %v", err)
	}
}

func TestSecondOpenIsLocked(t *testing.T) {
	_, path := openTemp(t)
	if _, err := Open(path); !errors.Is(err, ErrLocked) {
		t.Fatalf("second Open: got %v, want ErrLocked", err)
	}
}
