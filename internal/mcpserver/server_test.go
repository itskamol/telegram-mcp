package mcpserver

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/itskamol/telegram-mcp-go/internal/audit"
)

// connect runs the server in memory. The Telegram service is nil: these
// tests only cover what happens before any Telegram call.
func connect(t *testing.T, p Policy) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	log, err := audit.Open(filepath.Join(t.TempDir(), "audit.log"), "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })

	st, ct := mcp.NewInMemoryTransports()
	srv := New(nil, p, log, "test")
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func toolNames(t *testing.T, cs *mcp.ClientSession) map[string]*mcp.Tool {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*mcp.Tool{}
	for _, tl := range res.Tools {
		out[tl.Name] = tl
	}
	return out
}

var (
	readTools  = []string{"get_me", "list_chats", "resolve_username", "search_contacts", "list_messages", "search_messages"}
	writeTools = []string{"send_message", "reply_to_message", "forward_message", "edit_message", "delete_message", "mark_as_read"}
)

func TestToolList(t *testing.T) {
	tools := toolNames(t, connect(t, Policy{MaxDelete: 20}))
	if len(tools) != 12 {
		t.Fatalf("got %d tools, want 12", len(tools))
	}
	for _, n := range readTools {
		if tl := tools[n]; tl == nil || !tl.Annotations.ReadOnlyHint {
			t.Errorf("%s missing or not read-only", n)
		}
	}
	for _, n := range writeTools {
		tl := tools[n]
		if tl == nil || tl.Annotations.ReadOnlyHint {
			t.Errorf("%s missing or marked read-only", n)
			continue
		}
		destructive := n == "edit_message" || n == "delete_message"
		if *tl.Annotations.DestructiveHint != destructive {
			t.Errorf("%s destructiveHint = %v", n, *tl.Annotations.DestructiveHint)
		}
	}
}

func TestReadOnlyHidesWriteTools(t *testing.T) {
	tools := toolNames(t, connect(t, Policy{ReadOnly: true, MaxDelete: 20}))
	for _, n := range writeTools {
		if tools[n] != nil {
			t.Errorf("%s registered in read-only mode", n)
		}
	}
	if len(tools) != len(readTools) {
		t.Fatalf("got %d tools, want %d", len(tools), len(readTools))
	}
}

func TestValidationBeforeTelegram(t *testing.T) {
	cs := connect(t, Policy{MaxDelete: 2})
	cases := []struct {
		tool string
		args map[string]any
		want string
	}{
		{"send_message", map[string]any{"chat": "me", "text": "  "}, "text is empty"},
		{"send_message", map[string]any{"chat": "me", "text": strings.Repeat("x", 4097)}, "4096"},
		{"reply_to_message", map[string]any{"chat": "me", "text": "hi"}, "message_id"},
		{"delete_message", map[string]any{"chat": "me", "message_ids": []int{1, 2, 3}}, "at most 2"},
		{"delete_message", map[string]any{"chat": "me", "message_ids": []int{}}, "empty"},
		{"forward_message", map[string]any{"from_chat": "me", "to_chat": "me", "message_ids": []int{}}, "1 to 100"},
		{"search_messages", map[string]any{"chat": "me", "query": ""}, "query is empty"},
	}
	for _, c := range cases {
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: c.tool, Arguments: c.args})
		if err != nil {
			t.Errorf("%s: protocol error %v", c.tool, err)
			continue
		}
		text := res.Content[0].(*mcp.TextContent).Text
		if !res.IsError || !strings.Contains(text, c.want) {
			t.Errorf("%s %v: IsError=%v %q, want error containing %q", c.tool, c.args, res.IsError, text, c.want)
		}
	}
}

func TestAllowlist(t *testing.T) {
	open := Policy{}
	if !open.allows(-1001) {
		t.Fatal("empty allowlist blocks")
	}
	p := Policy{AllowedChatIDs: []int64{42, -1001234}}
	if !p.allows(42) || !p.allows(-1001234) || p.allows(43) {
		t.Fatal("allowlist mismatch")
	}
	if !slices.Equal(p.AllowedChatIDs, []int64{42, -1001234}) {
		t.Fatal("allowlist mutated")
	}
}
