package hub

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// shortDir keeps socket paths under the 104-byte limit; t.TempDir() on
// macOS is too long once the test name is in it.
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "hub")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

type echoIn struct {
	Text string `json:"text"`
}

func newServer(tools ...string) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	for _, name := range tools {
		mcp.AddTool(srv, &mcp.Tool{Name: name}, func(_ context.Context, _ *mcp.CallToolRequest, in echoIn) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: name + ":" + in.Text}}}, nil, nil
		})
	}
	return srv
}

// relayClient starts Relay on path and returns an MCP client talking
// through it, as a second `serve` process would for Claude.
func relayClient(t *testing.T, ctx context.Context, path string) *mcp.ClientSession {
	t.Helper()
	toRelayR, toRelayW := io.Pipe()
	fromRelayR, fromRelayW := io.Pipe()
	go func() {
		_ = Relay(ctx, path, toRelayR, fromRelayW, 2*time.Second)
		fromRelayW.Close()
	}()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "c"}, nil).
		Connect(ctx, &mcp.IOTransport{Reader: fromRelayR, Writer: toRelayW}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func TestRelayedSessions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := shortDir(t)
	path := SocketPath(dir, "default", false)

	ln, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("socket mode %v", fi.Mode().Perm())
	}
	var wg sync.WaitGroup
	wg.Go(func() { Serve(ctx, ln, newServer("echo"), slog.Default()) })

	// Two relays at once, like chat plus a Code session.
	a, b := relayClient(t, ctx, path), relayClient(t, ctx, path)
	for i, cs := range []*mcp.ClientSession{a, b} {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"text": "hi"}})
		if err != nil {
			t.Fatalf("client %d: %v", i, err)
		}
		if got := res.Content[0].(*mcp.TextContent).Text; got != "echo:hi" {
			t.Fatalf("client %d got %q", i, got)
		}
	}

	cancel()
	wg.Wait()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("socket left behind after shutdown")
	}
}

func TestReadOnlySocketIsSeparate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := shortDir(t)
	full, ro := SocketPath(dir, "default", false), SocketPath(dir, "default", true)
	if full == ro {
		t.Fatal("read-only and full share a socket")
	}

	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	for path, srv := range map[string]*mcp.Server{full: newServer("read", "write"), ro: newServer("read")} {
		ln, err := Listen(path)
		if err != nil {
			t.Fatal(err)
		}
		wg.Go(func() { Serve(ctx, ln, srv, slog.Default()) })
	}

	res, err := relayClient(t, ctx, ro).ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range res.Tools {
		if tl.Name == "write" {
			t.Fatal("read-only relay reached a write tool")
		}
	}
}

func TestRelayWithoutPrimary(t *testing.T) {
	path := SocketPath(shortDir(t), "work", false)
	start := time.Now()
	err := Relay(context.Background(), path, strings.NewReader(""), io.Discard, 300*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "different --account") {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) < 300*time.Millisecond {
		t.Fatal("gave up before the wait elapsed")
	}
}

func TestListenReplacesStaleSocketAndChecksLength(t *testing.T) {
	path := SocketPath(shortDir(t), "default", false)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := Listen(path)
	if err != nil {
		t.Fatalf("stale file not replaced: %v", err)
	}
	ln.Close()

	long := filepath.Join("/tmp", strings.Repeat("x", 120), "serve-default.sock")
	if _, err := Listen(long); err == nil || !strings.Contains(err.Error(), "longer than") {
		t.Fatalf("long path: %v", err)
	}
}
