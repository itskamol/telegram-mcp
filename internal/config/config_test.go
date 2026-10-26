package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadFileAndEnvOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TELEGRAM_MCP_HOME", dir)
	t.Setenv("TELEGRAM_API_ID", "")
	t.Setenv("TELEGRAM_API_HASH", "")
	p, err := ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("api_id = 123\napi_hash = \"abc\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := Load(p)
	if err != nil || c.APIID != 123 || c.APIHash != "abc" {
		t.Fatalf("file: %+v, %v", c, err)
	}

	t.Setenv("TELEGRAM_API_ID", "456")
	c, err = Load(p)
	if err != nil || c.APIID != 456 || c.APIHash != "abc" {
		t.Fatalf("env override: %+v, %v", c, err)
	}

	t.Setenv("TELEGRAM_API_ID", "nope")
	if _, err := Load(p); err == nil {
		t.Fatal("non-numeric TELEGRAM_API_ID accepted")
	}
}

func TestMissingFileIsEmpty(t *testing.T) {
	t.Setenv("TELEGRAM_MCP_HOME", t.TempDir())
	t.Setenv("TELEGRAM_API_ID", "")
	t.Setenv("TELEGRAM_API_HASH", "")
	p, _ := ResolvePaths()
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Validate(p) == nil {
		t.Fatal("empty config passed Validate")
	}
}

func TestValidateAccount(t *testing.T) {
	for _, ok := range []string{"default", "work", "a-1_b"} {
		if err := ValidateAccount(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "Work", "../x", "a b", "-x", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"} {
		if ValidateAccount(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
