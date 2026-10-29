package storage

import (
	"context"
	"time"
)

// Partials records the temp files of running downloads, so that a server
// killed mid-download can remove them at its next start.
type Partials struct {
	db      *DB
	account string
}

// NewPartials returns the partial-download journal of one account.
func NewPartials(db *DB, account string) *Partials { return &Partials{db: db, account: account} }

// Add records a temp file before the download writes to it.
func (p *Partials) Add(ctx context.Context, path string) error {
	_, err := p.db.SQL.ExecContext(ctx,
		`INSERT OR REPLACE INTO partial_downloads (account, path, started) VALUES (?, ?, ?)`,
		p.account, path, time.Now().Unix())
	return err
}

// Remove forgets a temp file once it is gone.
func (p *Partials) Remove(ctx context.Context, path string) error {
	_, err := p.db.SQL.ExecContext(ctx,
		`DELETE FROM partial_downloads WHERE account = ? AND path = ?`, p.account, path)
	return err
}

// Take returns every recorded temp file and clears the journal; called at
// startup, before any download runs.
func (p *Partials) Take(ctx context.Context) ([]string, error) {
	rows, err := p.db.SQL.QueryContext(ctx,
		`DELETE FROM partial_downloads WHERE account = ? RETURNING path`, p.account)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	return paths, rows.Err()
}
