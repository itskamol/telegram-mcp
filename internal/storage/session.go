package storage

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/gotd/td/session"

	"github.com/itskamol/telegram-mcp-go/internal/secret"
)

// ErrKeyMissing means a session row exists but its keychain key is gone,
// so the session cannot be decrypted and the user has to log in again.
var ErrKeyMissing = errors.New("session key is missing from the keychain; run `telegram-mcp login` again")

// Session implements gotd's session.Storage for one account. Data is sealed
// with AES-256-GCM; the account name is the associated data, so a row copied
// to another account fails to open.
type Session struct {
	db      *DB
	keys    secret.KeyStore
	account string
}

var _ session.Storage = (*Session)(nil)

// NewSession binds the storage to an account.
func NewSession(db *DB, keys secret.KeyStore, account string) *Session {
	return &Session{db: db, keys: keys, account: account}
}

func (s *Session) LoadSession(ctx context.Context) ([]byte, error) {
	var sealed []byte
	err := s.db.SQL.QueryRowContext(ctx,
		`SELECT ciphertext FROM sessions WHERE account = ?`, s.account).Scan(&sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, session.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load session: %w", err)
	}

	key, err := s.keys.Key(s.account, false)
	if errors.Is(err, secret.ErrNoKey) {
		return nil, ErrKeyMissing
	}
	if err != nil {
		return nil, err
	}
	return open(key, sealed, []byte(s.account))
}

func (s *Session) StoreSession(ctx context.Context, data []byte) error {
	key, err := s.keys.Key(s.account, true)
	if err != nil {
		return err
	}
	sealed, err := seal(key, data, []byte(s.account))
	if err != nil {
		return err
	}
	_, err = s.db.SQL.ExecContext(ctx, `
		INSERT INTO sessions (account, ciphertext, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (account) DO UPDATE SET ciphertext = excluded.ciphertext, updated_at = excluded.updated_at`,
		s.account, sealed, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("store session: %w", err)
	}
	return nil
}

// Exists reports whether a session row is stored, without decrypting it.
func (s *Session) Exists(ctx context.Context) (bool, error) {
	var n int
	err := s.db.SQL.QueryRowContext(ctx,
		`SELECT count(*) FROM sessions WHERE account = ?`, s.account).Scan(&n)
	return n > 0, err
}

// Delete removes the session row, the account's cached peers and its key.
func (s *Session) Delete(ctx context.Context) error {
	tx, err := s.db.SQL.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	for _, q := range []string{
		`DELETE FROM sessions WHERE account = ?`,
		`DELETE FROM peers WHERE account = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, s.account); err != nil {
			return fmt.Errorf("delete session: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.keys.Delete(s.account)
}

// seal returns nonce || AES-GCM(data).
func seal(key, data, aad []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, data, aad), nil
}

func open(key, sealed, aad []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(sealed) < gcm.NonceSize() {
		return nil, errors.New("stored session is truncated")
	}
	nonce, ct := sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():]
	data, err := gcm.Open(nil, nonce, ct, aad)
	if err != nil {
		return nil, errors.New("stored session does not decrypt with the keychain key; run `telegram-mcp login` again")
	}
	return data, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
