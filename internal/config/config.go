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
	"strconv"

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
}

// DefaultMaxDelete is used when max_delete is unset.
const DefaultMaxDelete = 20

// Paths holds every file location the binary touches.
type Paths struct {
	Dir    string // ~/Library/Application Support/telegram-mcp on macOS
	Config string // <Dir>/config.toml
	DB     string // <Dir>/telegram-mcp.db
	Audit  string // <Dir>/audit.log
}

// ResolvePaths picks the per-user config directory. TELEGRAM_MCP_HOME
// overrides it, which tests and multi-profile setups rely on.
func ResolvePaths() (Paths, error) {
	dir := os.Getenv("TELEGRAM_MCP_HOME")
	if dir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return Paths{}, fmt.Errorf("locate config dir: %w", err)
		}
		dir = filepath.Join(base, appName)
	}
	return Paths{
		Dir:    dir,
		Config: filepath.Join(dir, "config.toml"),
		DB:     filepath.Join(dir, appName+".db"),
		Audit:  filepath.Join(dir, "audit.log"),
	}, nil
}

// EnsureDir creates the config directory readable only by the owner.
func (p Paths) EnsureDir() error {
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", p.Dir, err)
	}
	return os.Chmod(p.Dir, 0o700)
}

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
