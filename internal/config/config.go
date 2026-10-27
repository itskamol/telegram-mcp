// Package config loads non-secret settings and resolves on-disk locations.
//
// Secrets never live here: the session is stored in SQLite, encrypted with a
// key held in the OS keychain. config.toml only carries values that are safe
// to read in plain text.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

const appName = "telegram-mcp"

// DefaultAccount is the label used when no account is given.
const DefaultAccount = "default"

var accountRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// Config is the contents of config.toml after env overrides.
type Config struct {
	APIID   int    `toml:"api_id"`
	APIHash string `toml:"api_hash"`

	// ReadOnly leaves the write tools unregistered.
	ReadOnly bool `toml:"read_only"`
	// AllowedChatIDs, when set, limits every chat-scoped tool to these
	// marked IDs (user > 0, group < 0, channel -100...).
	AllowedChatIDs []int64 `toml:"allowed_chat_ids"`
	// MaxDelete caps message IDs per delete_message call. Default 20.
	MaxDelete int `toml:"max_delete"`

	// AllowedRoots are the only directories download_media may write into;
	// "~/" is expanded. Default: ~/Downloads/telegram-mcp.
	AllowedRoots []string `toml:"allowed_roots"`
	// MaxDownloadMB caps a single download_media file. Default 50.
	MaxDownloadMB int `toml:"max_download_mb"`
	// MaxUploadMB caps a single send_file file. Default 50.
	MaxUploadMB int `toml:"max_upload_mb"`

	// WatchChatIDs are chats whose new messages are buffered for
	// get_new_messages (marked ids, as list_chats returns them).
	WatchChatIDs []int64 `toml:"watch_chat_ids"`
	// WatchTopics limits a forum chat to some topics, keyed by chat id:
	// { "-1001889276137" = [1] } keeps only General.
	WatchTopics map[string][]int `toml:"watch_topics"`
	// FeedRetentionDays drops buffered messages older than this. Default 7.
	FeedRetentionDays int `toml:"feed_retention_days"`
}

// Watch returns watched chats and their topic filters. Topic keys must be
// watched chats, and with allowed_chat_ids set every watched chat must be
// allowed too, so the buffer can never hold a chat the tools refuse.
func (c Config) Watch() (map[int64][]int, error) {
	w := make(map[int64][]int, len(c.WatchChatIDs))
	for _, id := range c.WatchChatIDs {
		if len(c.AllowedChatIDs) > 0 && !slices.Contains(c.AllowedChatIDs, id) {
			return nil, fmt.Errorf("watch_chat_ids: %d is not in allowed_chat_ids", id)
		}
		w[id] = nil
	}
	for key, topics := range c.WatchTopics {
		id, err := strconv.ParseInt(key, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("watch_topics: key %q is not a chat id", key)
		}
		if _, ok := w[id]; !ok {
			return nil, fmt.Errorf("watch_topics: chat %d is not in watch_chat_ids", id)
		}
		if len(topics) == 0 {
			return nil, fmt.Errorf("watch_topics: chat %d has an empty topic list; remove the key to watch the whole chat", id)
		}
		w[id] = topics
	}
	return w, nil
}

// Defaults used when the setting is unset.
const (
	DefaultMaxDelete     = 20
	DefaultMaxDownloadMB = 50
	DefaultMaxUploadMB   = 50
	DefaultRetentionDays = 7
)

// Load reads config.toml if present and applies TELEGRAM_API_ID /
// TELEGRAM_API_HASH overrides. A missing file is not an error.
func Load(p Paths) (Config, error) {
	var c Config
	if _, err := toml.DecodeFile(p.Config, &c); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Config{}, fmt.Errorf("read %s: %w", p.Config, err)
	}
	if v := os.Getenv("TELEGRAM_API_ID"); v != "" {
		id, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("TELEGRAM_API_ID is not a number")
		}
		c.APIID = id
	}
	if v := os.Getenv("TELEGRAM_API_HASH"); v != "" {
		c.APIHash = v
	}
	if c.MaxDelete <= 0 {
		c.MaxDelete = DefaultMaxDelete
	}
	if c.MaxDownloadMB <= 0 {
		c.MaxDownloadMB = DefaultMaxDownloadMB
	}
	if c.MaxUploadMB <= 0 {
		c.MaxUploadMB = DefaultMaxUploadMB
	}
	if c.FeedRetentionDays <= 0 {
		c.FeedRetentionDays = DefaultRetentionDays
	}
	if _, err := c.Watch(); err != nil {
		return Config{}, err
	}
	home, _ := os.UserHomeDir()
	if len(c.AllowedRoots) == 0 && home != "" {
		c.AllowedRoots = []string{filepath.Join(home, "Downloads", appName)}
	}
	for i, r := range c.AllowedRoots {
		if strings.HasPrefix(r, "~/") && home != "" {
			r = filepath.Join(home, r[2:])
		}
		if !filepath.IsAbs(r) {
			return Config{}, fmt.Errorf("allowed_roots: %q must be an absolute path", r)
		}
		c.AllowedRoots[i] = filepath.Clean(r)
	}
	return c, nil
}

// Validate reports missing API credentials with a hint on where to get them.
func (c Config) Validate(p Paths) error {
	if c.APIID == 0 || c.APIHash == "" {
		return fmt.Errorf("api_id and api_hash are required: get them at https://my.telegram.org/apps "+
			"and put them in %s (or TELEGRAM_API_ID / TELEGRAM_API_HASH)", p.Config)
	}
	return nil
}

// ValidateAccount keeps account labels safe to use as keychain and DB keys.
func ValidateAccount(name string) error {
	if !accountRe.MatchString(name) {
		return fmt.Errorf("invalid account %q: use 1-32 chars of a-z, 0-9, _ or -", name)
	}
	return nil
}
