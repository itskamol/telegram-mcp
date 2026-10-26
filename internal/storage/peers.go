package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Peer kinds as stored in the peers table.
const (
	KindUser    = "user"
	KindChat    = "chat"
	KindChannel = "channel"
)

// Peer is one cached user, basic group or channel. MTProto needs the
// access_hash to address users and channels, and only hands it out alongside
// other results, so every response that carries entities feeds this cache.
type Peer struct {
	Kind       string
	ID         int64 // plain Telegram ID, not the marked -100... form
	AccessHash int64
	Username   string
	Title      string
}

// Peers is the per-account peer cache.
type Peers struct {
	db      *DB
	account string
}

// NewPeers binds the cache to an account.
func NewPeers(db *DB, account string) *Peers {
	return &Peers{db: db, account: account}
}

// Put upserts peers in one transaction. A zero access hash never overwrites
// a known one: Telegram sends "min" entities without it.
func (p *Peers) Put(ctx context.Context, peers []Peer) error {
	if len(peers) == 0 {
		return nil
	}
	tx, err := p.db.SQL.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO peers (account, kind, id, access_hash, username, title, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (account, kind, id) DO UPDATE SET
			access_hash = CASE WHEN excluded.access_hash != 0 THEN excluded.access_hash ELSE peers.access_hash END,
			username    = excluded.username,
			title       = excluded.title,
			updated_at  = excluded.updated_at`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	now := time.Now().Unix()
	for _, pr := range peers {
		var username any
		if pr.Username != "" {
			username = strings.ToLower(pr.Username)
		}
		if _, err := stmt.ExecContext(ctx, p.account, pr.Kind, pr.ID, pr.AccessHash, username, pr.Title, now); err != nil {
			return fmt.Errorf("cache peer: %w", err)
		}
	}
	return tx.Commit()
}

// ErrPeerNotCached means the cache has no such peer.
var ErrPeerNotCached = errors.New("peer not in cache")

// Get looks a peer up by kind and plain ID.
func (p *Peers) Get(ctx context.Context, kind string, id int64) (Peer, error) {
	return p.one(ctx, `WHERE account = ? AND kind = ? AND id = ?`, p.account, kind, id)
}

// ByUsername looks a peer up by username, case-insensitively, without '@'.
func (p *Peers) ByUsername(ctx context.Context, username string) (Peer, error) {
	return p.one(ctx, `WHERE account = ? AND username = ?`, p.account, strings.ToLower(username))
}

func (p *Peers) one(ctx context.Context, where string, args ...any) (Peer, error) {
	var pr Peer
	var username, title sql.NullString
	err := p.db.SQL.QueryRowContext(ctx,
		`SELECT kind, id, access_hash, username, title FROM peers `+where+` LIMIT 1`, args...).
		Scan(&pr.Kind, &pr.ID, &pr.AccessHash, &username, &title)
	if errors.Is(err, sql.ErrNoRows) {
		return Peer{}, ErrPeerNotCached
	}
	pr.Username, pr.Title = username.String, title.String
	return pr, err
}
