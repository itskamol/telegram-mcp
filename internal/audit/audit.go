// Package audit appends one JSON line per write action to audit.log.
//
// It records what was done and where (tool, chat, message IDs, outcome) but
// never message text: the log must be safe to keep and share for debugging.
package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// Entry is one audited action.
type Entry struct {
	Time     string `json:"time"`
	Account  string `json:"account"`
	Tool     string `json:"tool"`
	ChatID   int64  `json:"chat_id,omitempty"`
	ToChatID int64  `json:"to_chat_id,omitempty"`
	IDs      []int  `json:"message_ids,omitempty"`
	Path     string `json:"path,omitempty"`
	Error    string `json:"error,omitempty"`
}

// Log is an append-only audit file.
type Log struct {
	mu      sync.Mutex
	f       *os.File
	account string
}

// Open opens (or creates) the log, readable only by the owner.
func Open(path, account string) (*Log, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open audit log: %w", err)
	}
	return &Log{f: f, account: account}, nil
}

// Write records e. Failing to audit fails the caller's result reporting,
// not the Telegram action, which has already happened.
func (l *Log) Write(e Entry) error {
	e.Time = time.Now().UTC().Format(time.RFC3339)
	e.Account = l.account
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, err = l.f.Write(append(b, '\n'))
	return err
}

// Close closes the file.
func (l *Log) Close() error { return l.f.Close() }
