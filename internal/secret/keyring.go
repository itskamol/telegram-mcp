// Package secret keeps the per-account session encryption key in the OS
// keychain (macOS Keychain, Windows Credential Manager, Linux Secret Service).
//
// Only a 32-byte key lives in the keychain, not the session itself: Windows
// caps credential blobs at 2560 bytes and a gotd session is larger than that.
package secret

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/zalando/go-keyring"
)

const service = "telegram-mcp"

// KeySize is the AES-256 key length.
const KeySize = 32

// ErrNoKey means the keychain holds no key for the account.
var ErrNoKey = errors.New("no session key in keychain")

// KeyStore fetches and removes per-account encryption keys.
type KeyStore interface {
	// Key returns the account's key. With create, a missing key is generated
	// and saved; without it, ErrNoKey is returned.
	Key(account string, create bool) ([]byte, error)
	Delete(account string) error
}

// Keyring is the OS keychain implementation of KeyStore.
type Keyring struct{}

func (Keyring) Key(account string, create bool) ([]byte, error) {
	enc, err := keyring.Get(service, account)
	switch {
	case err == nil:
		key, err := base64.StdEncoding.DecodeString(enc)
		if err != nil || len(key) != KeySize {
			return nil, fmt.Errorf("keychain entry %s/%s is corrupt", service, account)
		}
		return key, nil
	case !errors.Is(err, keyring.ErrNotFound):
		return nil, fmt.Errorf("read keychain: %w", err)
	case !create:
		return nil, ErrNoKey
	}

	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	if err := keyring.Set(service, account, base64.StdEncoding.EncodeToString(key)); err != nil {
		return nil, fmt.Errorf("write keychain: %w", err)
	}
	return key, nil
}

func (Keyring) Delete(account string) error {
	if err := keyring.Delete(service, account); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("delete keychain entry: %w", err)
	}
	return nil
}
