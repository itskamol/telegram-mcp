package dlclient

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type sendIn struct {
	Chat           string `json:"chat"`
	Path           string `json:"path"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

type sendOut struct {
	Status      string `json:"status,omitempty"`
	ChatID      int64  `json:"chat_id"`
	MessageID   int    `json:"message_id,omitempty"`
	Uploaded    int64  `json:"uploaded,omitempty"`
	Size        int64  `json:"size,omitempty"`
	AlreadySent bool   `json:"already_sent,omitempty"`
}

// sendSession connects to a server whose send_file answers "uploading"
// `pending` times, then sent (or fails, if fail is set). With noSend the
// server has no send_file, as a read-only one.
func sendSession(t *testing.T, pending int32, fail error, noSend bool, calls *atomic.Int32, keys *[]string, mu *sync.Mutex) *mcp.ClientSession {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "0"}, nil)
	if noSend {
		mcp.AddTool(srv, &mcp.Tool{Name: "get_me"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			return nil, nil, nil
		})
	} else {
		mcp.AddTool(srv, &mcp.Tool{Name: "send_file"}, func(_ context.Context, _ *mcp.CallToolRequest, a sendIn) (*mcp.CallToolResult, sendOut, error) {
			n := calls.Add(1)
			mu.Lock()
			*keys = append(*keys, a.IdempotencyKey)
			mu.Unlock()
			if n <= pending {
				return nil, sendOut{Status: "uploading", ChatID: 1, Uploaded: int64(n) * 100, Size: 300}, nil
			}
			if fail != nil {
				return nil, sendOut{}, fail
			}
			return nil, sendOut{Status: "sent", ChatID: 1, MessageID: 42}, nil
		})
	}
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

// Send calls send_file with the same arguments until sent and returns the
// message id. It adds no idempotency_key: the server derives one from the
// arguments, so these calls, and a send_file call with the same arguments,
// are one upload; a made-up key would make them a second message.
func TestSendUntilSent(t *testing.T) {
	var calls atomic.Int32
	var keys []string
	var mu sync.Mutex
	cs := sendSession(t, 2, nil, false, &calls, &keys, &mu)
	var progress bytes.Buffer

	id, err := Send(context.Background(), cs, map[string]any{"chat": "me", "path": "/r/f.bin"}, &progress)
	if err != nil || id != 42 {
		t.Fatalf("Send = %d, %v; want 42", id, err)
	}
	if len(keys) != 3 || keys[0] != "" || keys[1] != "" || keys[2] != "" {
		t.Errorf("idempotency keys per call = %q, want none added on all 3", keys)
	}
	if strings.Count(progress.String(), "uploading") != 2 {
		t.Errorf("progress = %q, want one uploading line per answer", progress.String())
	}
}

// A key given by the caller is used as is.
func TestSendKeepsGivenKey(t *testing.T) {
	var calls atomic.Int32
	var keys []string
	var mu sync.Mutex
	cs := sendSession(t, 1, nil, false, &calls, &keys, &mu)
	if _, err := Send(context.Background(), cs, map[string]any{"chat": "me", "path": "/r/f.bin", "idempotency_key": "mine"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0] != "mine" || keys[1] != "mine" {
		t.Errorf("keys = %q, want [mine mine]", keys)
	}
}

func TestSendFailure(t *testing.T) {
	var calls atomic.Int32
	var keys []string
	var mu sync.Mutex
	cs := sendSession(t, 1, errors.New("CHAT_WRITE_FORBIDDEN"), false, &calls, &keys, &mu)
	if _, err := Send(context.Background(), cs, map[string]any{"chat": "me", "path": "/r/f.bin"}, &bytes.Buffer{}); err == nil ||
		!strings.Contains(err.Error(), "CHAT_WRITE_FORBIDDEN") {
		t.Errorf("err = %v, want the tool's error", err)
	}
}

// A read-only server has no send_file: a clear error, nothing called.
func TestSendReadOnly(t *testing.T) {
	var calls atomic.Int32
	var keys []string
	var mu sync.Mutex
	cs := sendSession(t, 0, nil, true, &calls, &keys, &mu)
	if _, err := Send(context.Background(), cs, map[string]any{"chat": "me", "path": "/r/f.bin"}, &bytes.Buffer{}); !errors.Is(err, ErrNoSendFile) {
		t.Errorf("err = %v, want ErrNoSendFile", err)
	}
}
