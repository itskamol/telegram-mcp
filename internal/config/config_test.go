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

func TestWatch(t *testing.T) {
	c := Config{WatchChatIDs: []int64{-1001889276137, 42}, WatchTopics: map[string][]int{"-1001889276137": {1}}}
	w, err := c.Watch()
	if err != nil || len(w) != 2 || len(w[-1001889276137]) != 1 || w[42] != nil {
		t.Fatalf("watch: %v %v", w, err)
	}
	bad := []Config{
		{WatchTopics: map[string][]int{"-100": {1}}},                           // topic for unwatched chat
		{WatchChatIDs: []int64{5}, WatchTopics: map[string][]int{"5": {}}},     // empty topic list
		{WatchChatIDs: []int64{5}, WatchTopics: map[string][]int{"five": {1}}}, // bad key
		{WatchChatIDs: []int64{5}, AllowedChatIDs: []int64{6}},                 // watched but not allowed
	}
	for i, c := range bad {
		if _, err := c.Watch(); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
}
