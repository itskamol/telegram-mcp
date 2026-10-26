// Package install registers the binary as an MCP server in Claude clients.
//
// Only the command path and arguments are written: secrets stay in the
// keychain and SQLite, never in the client's config file.
package install

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
)

// Server is one mcpServers entry.
type Server struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

// ErrExists means an entry with that name points somewhere else.
var ErrExists = errors.New("an MCP server with this name already exists with different settings")

// DesktopConfigPath is claude_desktop_config.json for the current OS.
func DesktopConfigPath() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Application Support", "Claude", "claude_desktop_config.json"), nil
	case "windows":
		dir := os.Getenv("APPDATA")
		if dir == "" {
			return "", errors.New("%APPDATA% is not set")
		}
		return filepath.Join(dir, "Claude", "claude_desktop_config.json"), nil
	default:
		dir, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(dir, "Claude", "claude_desktop_config.json"), nil
	}
}

// AddToDesktopConfig adds or updates mcpServers[name] in the JSON file at
// path, keeping every other key. An existing different entry is replaced
// only with force. The previous file is kept as path+".bak". It reports
// whether the file changed.
func AddToDesktopConfig(path, name string, s Server, force bool) (bool, error) {
	cfg := map[string]any{}
	old, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		old = nil
	case err != nil:
		return false, err
	case len(bytes.TrimSpace(old)) > 0:
		if err := json.Unmarshal(old, &cfg); err != nil {
			return false, fmt.Errorf("%s is not valid JSON, not touching it: %w", path, err)
		}
	}

	servers, _ := cfg["mcpServers"].(map[string]any)
	if servers == nil {
		if _, present := cfg["mcpServers"]; present {
			return false, fmt.Errorf("%s: mcpServers is not an object, not touching it", path)
		}
		servers = map[string]any{}
	}
	want := map[string]any{"command": s.Command, "args": toAny(s.Args)}
	if cur, ok := servers[name]; ok {
		if reflect.DeepEqual(cur, want) {
			return false, nil
		}
		if !force {
			return false, ErrExists
		}
	}
	servers[name] = want
	cfg["mcpServers"] = servers

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	mode := os.FileMode(0o600)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	if old != nil {
		if err := os.WriteFile(path+".bak", old, mode); err != nil {
			return false, fmt.Errorf("write backup: %w", err)
		}
	}
	// Write next to the target and rename, so a crash never leaves half a file.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".claude_desktop_config-*.json")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(out, '\n')); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	return true, os.Rename(tmp.Name(), path)
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// Executable is the absolute path to write into client configs. It refuses
// paths that will not survive, like `go run` build caches.
func Executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", err
	}
	tmp, _ := filepath.EvalSymlinks(os.TempDir())
	if tmp != "" {
		if rel, err := filepath.Rel(tmp, real); err == nil && filepath.IsLocal(rel) {
			return "", fmt.Errorf("%s is in a temporary directory; install the binary first (e.g. into /usr/local/bin)", real)
		}
	}
	return stablePath(os.Args[0], real), nil
}

// stablePath prefers the command as it was invoked, such as Homebrew's
// /opt/homebrew/bin/telegram-mcp, over the resolved file: package managers
// keep that symlink pointing at the current version, while the versioned
// directory it resolves to (Caskroom/telegram-mcp/0.2.2) is deleted on
// upgrade. The invoked path is used only if it leads to this very binary.
func stablePath(arg0, real string) string {
	p := arg0
	if !strings.ContainsRune(arg0, filepath.Separator) {
		found, err := exec.LookPath(arg0)
		if err != nil {
			return real
		}
		p = found
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return real
	}
	if r, err := filepath.EvalSymlinks(abs); err != nil || r != real {
		return real
	}
	return abs
}
