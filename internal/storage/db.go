// Package storage is the SQLite database: encrypted sessions and the peer
// cache. It uses the pure-Go modernc driver so the binary stays CGO-free.
package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"os"

	"github.com/gofrs/flock"
	_ "modernc.org/sqlite"
)

// ErrLocked means another telegram-mcp process holds the database.
//
// Two processes on one auth key make Telegram return AUTH_KEY_DUPLICATED and
// drop both connections, so a second instance must refuse to start.
var ErrLocked = errors.New("another telegram-mcp process is using this profile")

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
	account    TEXT PRIMARY KEY,
	ciphertext BLOB NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS peers (
	account     TEXT    NOT NULL,
	kind        TEXT    NOT NULL CHECK (kind IN ('user', 'chat', 'channel')),
	id          INTEGER NOT NULL,
	access_hash INTEGER NOT NULL DEFAULT 0,
	username    TEXT,
	title       TEXT,
	updated_at  INTEGER NOT NULL,
	PRIMARY KEY (account, kind, id)
);
CREATE INDEX IF NOT EXISTS peers_username ON peers (account, username);
`

// DB is an open, locked database.
type DB struct {
	SQL  *sql.DB
	lock *flock.Flock
}

// Open locks path+".lock", opens the database in WAL mode and applies the
// schema. The database file is restricted to the owner.
func Open(path string) (*DB, error) {
	lock := flock.New(path + ".lock")
	ok, err := lock.TryLock()
	if err != nil {
		return nil, fmt.Errorf("lock database: %w", err)
	}
	if !ok {
		return nil, ErrLocked
	}

	db, err := sql.Open("sqlite", "file:"+path+
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		_ = lock.Unlock()
		return nil, err
	}
	// One writer keeps SQLite free of SQLITE_BUSY between our own goroutines.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		_ = lock.Unlock()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	for _, f := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(f, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			_ = db.Close()
			_ = lock.Unlock()
			return nil, fmt.Errorf("restrict %s: %w", f, err)
		}
	}
	return &DB{SQL: db, lock: lock}, nil
}

// Close closes the database and releases the lock.
func (d *DB) Close() error {
	err := d.SQL.Close()
	if uerr := d.lock.Unlock(); err == nil {
		err = uerr
	}
	return err
}
