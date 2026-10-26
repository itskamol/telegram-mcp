package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"

	"github.com/gofrs/flock"
)

// Paths holds every file location the binary touches, split the XDG way:
// what the user edits (config), what the program owns (data: the session
// database) and what it accumulates (state: the audit log). Keeping the
// database out of ~/.config stops it landing in a dotfiles repository.
type Paths struct {
	ConfigDir string // ~/.config/telegram-mcp
	DataDir   string // ~/.local/share/telegram-mcp
	StateDir  string // ~/.local/state/telegram-mcp

	Config string // <ConfigDir>/config.toml
	DB     string // <DataDir>/telegram-mcp.db
	Audit  string // <StateDir>/audit.log
}

const dbName = appName + ".db"

// ResolvePaths picks the directories. TELEGRAM_MCP_HOME puts everything in
// one directory, for tests and separate profiles. Otherwise XDG_CONFIG_HOME,
// XDG_DATA_HOME and XDG_STATE_HOME are honoured, with the XDG defaults on
// macOS too, as CLI tools like gh and git do. Windows keeps %AppData%.
func ResolvePaths() (Paths, error) {
	if dir := os.Getenv("TELEGRAM_MCP_HOME"); dir != "" {
		return newPaths(dir, dir, dir), nil
	}
	if runtime.GOOS == "windows" {
		base, err := os.UserConfigDir()
		if err != nil {
			return Paths{}, fmt.Errorf("locate config dir: %w", err)
		}
		dir := filepath.Join(base, appName)
		return newPaths(dir, dir, dir), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, fmt.Errorf("locate home dir: %w", err)
	}
	return newPaths(
		filepath.Join(xdg("XDG_CONFIG_HOME", home, ".config"), appName),
		filepath.Join(xdg("XDG_DATA_HOME", home, ".local", "share"), appName),
		filepath.Join(xdg("XDG_STATE_HOME", home, ".local", "state"), appName),
	), nil
}

// xdg returns $env, or home/def... when it is unset or relative (the spec
// says relative values must be ignored).
func xdg(env, home string, def ...string) string {
	if v := os.Getenv(env); filepath.IsAbs(v) {
		return v
	}
	return filepath.Join(append([]string{home}, def...)...)
}

func newPaths(configDir, dataDir, stateDir string) Paths {
	return Paths{
		ConfigDir: configDir,
		DataDir:   dataDir,
		StateDir:  stateDir,
		Config:    filepath.Join(configDir, "config.toml"),
		DB:        filepath.Join(dataDir, dbName),
		Audit:     filepath.Join(stateDir, "audit.log"),
	}
}

// Dirs lists the distinct directories.
func (p Paths) Dirs() []string {
	return slices.Compact([]string{p.ConfigDir, p.DataDir, p.StateDir})
}

// EnsureDirs creates the directories readable only by the owner.
func (p Paths) EnsureDirs() error {
	for _, d := range p.Dirs() {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", d, err)
		}
		if err := os.Chmod(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// LegacyDir is where versions before the XDG layout kept everything:
// os.UserConfigDir()/telegram-mcp. Empty when it does not apply.
func LegacyDir() string {
	if os.Getenv("TELEGRAM_MCP_HOME") != "" || runtime.GOOS == "windows" {
		return ""
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(base, appName)
}

// Migrate moves a profile from legacy into p and returns what it moved.
//
// It moves nothing unless every destination is free, so a half-migrated or
// already-used new profile is never mixed with the old one. The old
// database lock is held throughout: a server still running from the legacy
// location makes the migration fail instead of moving its files under it.
func Migrate(legacy string, p Paths) ([]string, error) {
	if legacy == "" {
		return nil, nil
	}
	moves := [][2]string{
		{filepath.Join(legacy, "config.toml"), p.Config},
		{filepath.Join(legacy, dbName), p.DB},
		{filepath.Join(legacy, dbName+"-wal"), p.DB + "-wal"},
		{filepath.Join(legacy, dbName+"-shm"), p.DB + "-shm"},
		{filepath.Join(legacy, "audit.log"), p.Audit},
	}
	moves = slices.DeleteFunc(moves, func(m [2]string) bool {
		_, err := os.Stat(m[0])
		return m[0] == m[1] || err != nil // same place, or nothing to move
	})
	if len(moves) == 0 {
		return nil, nil
	}
	for _, m := range moves {
		if _, err := os.Stat(m[1]); err == nil {
			return nil, fmt.Errorf("both %s and %s exist; move or delete one of them by hand", m[0], m[1])
		}
	}

	lock := flock.New(filepath.Join(legacy, dbName+".lock"))
	ok, err := lock.TryLock()
	if err != nil {
		return nil, fmt.Errorf("lock legacy profile: %w", err)
	}
	if !ok {
		return nil, errors.New("telegram-mcp is still running from " + legacy + "; stop it and try again")
	}
	defer lock.Unlock() //nolint:errcheck // lock file is removed below

	if err := p.EnsureDirs(); err != nil {
		return nil, err
	}
	var moved []string
	for _, m := range moves {
		if err := os.Rename(m[0], m[1]); err != nil {
			return moved, fmt.Errorf("move %s: %w (already moved: %v)", m[0], err, moved)
		}
		moved = append(moved, m[1])
	}
	_ = os.Remove(filepath.Join(legacy, dbName+".lock"))
	_ = os.Remove(legacy) // only succeeds if nothing else is left
	return moved, nil
}
