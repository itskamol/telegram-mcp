package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/gofrs/flock"
)

func TestResolvePathsXDG(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows keeps %AppData%")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TELEGRAM_MCP_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "relative/ignored")
	t.Setenv("XDG_STATE_HOME", "/xdg/state")

	p, err := ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		p.Config: filepath.Join(home, ".config", "telegram-mcp", "config.toml"),
		p.DB:     filepath.Join(home, ".local", "share", "telegram-mcp", "telegram-mcp.db"),
		p.Audit:  "/xdg/state/telegram-mcp/audit.log",
	}
	for got, w := range want {
		if got != w {
			t.Errorf("got %s, want %s", got, w)
		}
	}
}

func TestHomeOverridePutsAllInOneDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TELEGRAM_MCP_HOME", dir)
	p, err := ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Dirs()) != 1 || p.Dirs()[0] != dir {
		t.Fatalf("dirs = %v", p.Dirs())
	}
	if LegacyDir() != "" {
		t.Fatal("legacy migration must be off with TELEGRAM_MCP_HOME")
	}
}

func legacyProfile(t *testing.T) string {
	t.Helper()
	legacy := filepath.Join(t.TempDir(), "legacy")
	if err := os.Mkdir(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"config.toml":          "api_id = 1\n",
		"telegram-mcp.db":      "db",
		"telegram-mcp.db-wal":  "wal",
		"audit.log":            "{}\n",
		"telegram-mcp.db.lock": "",
	} {
		if err := os.WriteFile(filepath.Join(legacy, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return legacy
}

func xdgPaths(t *testing.T) Paths {
	root := t.TempDir()
	return newPaths(filepath.Join(root, "config"), filepath.Join(root, "data"), filepath.Join(root, "state"))
}

func TestMigrate(t *testing.T) {
	legacy, p := legacyProfile(t), xdgPaths(t)
	moved, err := Migrate(legacy, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(moved) != 4 {
		t.Fatalf("moved %v", moved)
	}
	for path, body := range map[string]string{p.Config: "api_id = 1\n", p.DB: "db", p.DB + "-wal": "wal", p.Audit: "{}\n"} {
		if b, err := os.ReadFile(path); err != nil || string(b) != body {
			t.Errorf("%s: %q %v", path, b, err)
		}
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Error("empty legacy dir left behind")
	}
	if fi, _ := os.Stat(p.DataDir); fi.Mode().Perm() != 0o700 {
		t.Errorf("data dir mode %v", fi.Mode().Perm())
	}

	// Second run: nothing left to do.
	if moved, err := Migrate(legacy, p); err != nil || moved != nil {
		t.Fatalf("rerun: %v %v", moved, err)
	}
}

func TestMigrateRefusesConflict(t *testing.T) {
	legacy, p := legacyProfile(t), xdgPaths(t)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.DB, []byte("newer"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(legacy, p); err == nil {
		t.Fatal("conflict accepted")
	}
	// Nothing moved, not even the files without a conflict.
	if _, err := os.Stat(filepath.Join(legacy, "config.toml")); err != nil {
		t.Fatal("config.toml moved despite the conflict")
	}
	if b, _ := os.ReadFile(p.DB); string(b) != "newer" {
		t.Fatal("existing database overwritten")
	}
}

func TestMigrateRefusesRunningServer(t *testing.T) {
	legacy, p := legacyProfile(t), xdgPaths(t)
	lock := flock.New(filepath.Join(legacy, "telegram-mcp.db.lock"))
	if ok, err := lock.TryLock(); !ok || err != nil {
		t.Fatal("could not take lock")
	}
	defer lock.Unlock()
	if _, err := Migrate(legacy, p); err == nil {
		t.Fatal("migrated under a running server")
	}
	if _, err := os.Stat(filepath.Join(legacy, "telegram-mcp.db")); err != nil {
		t.Fatal("database moved under a running server")
	}
}

func TestMigrateSameDirMovesOnlyData(t *testing.T) {
	// Linux before XDG: legacy == ~/.config/telegram-mcp == ConfigDir.
	legacy := legacyProfile(t)
	root := t.TempDir()
	p := newPaths(legacy, filepath.Join(root, "data"), filepath.Join(root, "state"))
	moved, err := Migrate(legacy, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(moved) != 3 {
		t.Fatalf("moved %v", moved)
	}
	if _, err := os.Stat(p.Config); err != nil {
		t.Fatal("config.toml should stay put")
	}
}
