package dlclient

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type in struct {
	Chat      string `json:"chat"`
	MessageID int    `json:"message_id"`
	Dir       string `json:"dir,omitempty"`
}

type out struct {
	Status     string `json:"status"`
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	Downloaded int64  `json:"downloaded,omitempty"`
	EtaSeconds int64  `json:"eta_seconds,omitempty"`
}

// session connects to a server whose download_media answers "downloading"
// `pending` times, then done (or fails, if fail is set).
func session(t *testing.T, pending int32, fail error, calls *atomic.Int32, got *atomic.Value) *mcp.ClientSession {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "0"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "download_media"}, func(_ context.Context, _ *mcp.CallToolRequest, a in) (*mcp.CallToolResult, out, error) {
		n := calls.Add(1)
		got.Store(a)
		if n <= pending {
			return nil, out{Status: "downloading", Path: "/d/f.bin", Size: 300, Downloaded: int64(n) * 100, EtaSeconds: 9}, nil
		}
		if fail != nil {
			return nil, out{}, fail
		}
		return nil, out{Status: "done", Path: "/d/f (1).bin", Size: 300}, nil
	})
	st, ct := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "c"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// Run calls download_media until done, reports progress once per
// "downloading" answer, and returns the final path, which may differ from
// the planned one.
func TestRunUntilDone(t *testing.T) {
	var calls atomic.Int32
	var got atomic.Value
	cs := session(t, 2, nil, &calls, &got)
	var progress bytes.Buffer
	args := map[string]any{"chat": "me", "message_id": 5, "dir": "x"}

	path, err := Run(context.Background(), cs, args, &progress)
	if err != nil || path != "/d/f (1).bin" {
		t.Fatalf("Run = %q, %v; want the final path", path, err)
	}
	if n := calls.Load(); n != 3 {
		t.Errorf("download_media called %d times, want 3", n)
	}
	if a := got.Load().(in); a.Chat != "me" || a.MessageID != 5 || a.Dir != "x" {
		t.Errorf("arguments = %+v", a)
	}
	lines := strings.Split(strings.TrimSpace(progress.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "100") || !strings.Contains(lines[1], "200") {
		t.Errorf("progress = %q, want one line per downloading answer with the bytes", progress.String())
	}
}

// A failed download ends Run with the tool's error, at once.
func TestRunFailure(t *testing.T) {
	var calls atomic.Int32
	var got atomic.Value
	cs := session(t, 1, errors.New("download stalled: no data for 2m0s"), &calls, &got)
	_, err := Run(context.Background(), cs, map[string]any{"chat": "me", "message_id": 5}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "stalled") {
		t.Errorf("err = %v, want the tool's error", err)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("download_media called %d times, want 2", n)
	}
}

// Stopping the command stops the waiting; the job is the server's.
func TestRunCancelled(t *testing.T) {
	var calls atomic.Int32
	var got atomic.Value
	cs := session(t, 1000, nil, &calls, &got)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Run(ctx, cs, map[string]any{"chat": "me", "message_id": 5}, &bytes.Buffer{}); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}
