package install

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func read(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestAddCreatesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Claude", "claude_desktop_config.json")
	changed, err := AddToDesktopConfig(path, "telegram", Server{Command: "/bin/tg", Args: []string{"serve"}}, false)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	srv := read(t, path)["mcpServers"].(map[string]any)["telegram"].(map[string]any)
	if srv["command"] != "/bin/tg" {
		t.Fatalf("entry: %v", srv)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
}

func TestAddKeepsOtherKeysAndBacksUp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.json")
	orig := `{"globalShortcut":"Ctrl+Space","mcpServers":{"other":{"command":"x","args":[],"env":{"K":"V"}}}}`
	if err := os.WriteFile(path, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := AddToDesktopConfig(path, "telegram", Server{Command: "/bin/tg", Args: []string{"serve"}}, false); err != nil {
		t.Fatal(err)
	}
	m := read(t, path)
	if m["globalShortcut"] != "Ctrl+Space" {
		t.Error("top-level key lost")
	}
	other := m["mcpServers"].(map[string]any)["other"].(map[string]any)
	if other["env"].(map[string]any)["K"] != "V" {
		t.Error("other server's env lost")
	}
	if b, _ := os.ReadFile(path + ".bak"); string(b) != orig {
		t.Error("backup missing or different")
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o644 {
		t.Errorf("mode changed to %v", fi.Mode().Perm())
	}
}

func TestAddIdempotentAndConflict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.json")
	s := Server{Command: "/bin/tg", Args: []string{"serve"}}
	if _, err := AddToDesktopConfig(path, "telegram", s, false); err != nil {
		t.Fatal(err)
	}
	if changed, err := AddToDesktopConfig(path, "telegram", s, false); err != nil || changed {
		t.Fatalf("rerun: changed=%v err=%v", changed, err)
	}
	ro := Server{Command: "/bin/tg", Args: []string{"serve", "--read-only"}}
	if _, err := AddToDesktopConfig(path, "telegram", ro, false); !errors.Is(err, ErrExists) {
		t.Fatalf("conflict: %v", err)
	}
	if changed, err := AddToDesktopConfig(path, "telegram", ro, true); err != nil || !changed {
		t.Fatalf("force: changed=%v err=%v", changed, err)
	}
}

func TestAddRefusesBrokenJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers": `), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := AddToDesktopConfig(path, "telegram", Server{Command: "x"}, false); err == nil {
		t.Fatal("broken JSON overwritten")
	}
	if b, _ := os.ReadFile(path); string(b) != `{"mcpServers": ` {
		t.Fatal("file changed")
	}
}
