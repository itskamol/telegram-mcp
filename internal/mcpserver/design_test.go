package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gotd/td/tgerr"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/itskamol/telegram-mcp-go/internal/audit"
	"github.com/itskamol/telegram-mcp-go/internal/tele"
)

func TestChatRefUnmarshal(t *testing.T) {
	ok := map[string]ChatRef{
		`-1001234567890`: "-1001234567890", // beyond float32, must stay exact
		`777000`:         "777000",
		`"  @durov "`:    "@durov",
		`"me"`:           "me",
	}
	for in, want := range ok {
		var c ChatRef
		if err := json.Unmarshal([]byte(in), &c); err != nil || c != want {
			t.Errorf("%s: got %q, %v", in, c, err)
		}
	}
	for _, bad := range []string{`1.5`, `true`, `{}`, `[1]`} {
		var c ChatRef
		if err := json.Unmarshal([]byte(bad), &c); err == nil {
			t.Errorf("%s accepted as %q", bad, c)
		}
	}
}

func TestChatSchemaAcceptsNumberAndKeepsDescriptions(t *testing.T) {
	tools := toolNames(t, connect(t, Policy{MaxDelete: 20}))
	props := func(tool string) map[string]any {
		b, _ := json.Marshal(tools[tool].InputSchema)
		var s struct{ Properties map[string]any }
		_ = json.Unmarshal(b, &s)
		return s.Properties
	}
	chat := props("list_messages")["chat"].(map[string]any)
	if types, _ := json.Marshal(chat["type"]); !strings.Contains(string(types), "integer") || !strings.Contains(string(types), "string") {
		t.Errorf("chat type = %s, want integer and string", types)
	}
	fwd := props("forward_messages")
	if fwd["from_chat"].(map[string]any)["description"] == fwd["to_chat"].(map[string]any)["description"] {
		t.Error("from_chat and to_chat share one description")
	}
}

func TestEveryToolDescribesItsParameters(t *testing.T) {
	for name, tl := range toolNames(t, connect(t, Policy{MaxDelete: 20})) {
		b, _ := json.Marshal(tl.InputSchema)
		var s struct {
			Properties map[string]struct{ Description string }
		}
		_ = json.Unmarshal(b, &s)
		for p, d := range s.Properties {
			if d.Description == "" {
				t.Errorf("%s.%s has no description", name, p)
			}
		}
		if len(tl.Description) < 40 {
			t.Errorf("%s description too short to choose by: %q", name, tl.Description)
		}
	}
}

func TestPage(t *testing.T) {
	msgs := []tele.Message{{ID: 30}, {ID: 29}, {ID: 27}}
	if p := page(msgs, 3); !p.HasMore || p.NextOffsetID != 27 {
		t.Errorf("full page: %+v", p)
	}
	if p := page(msgs, 5); p.HasMore || p.NextOffsetID != 0 {
		t.Errorf("short page: %+v", p)
	}
	if p := page(nil, 5); p.Messages == nil {
		t.Error("nil messages should encode as []")
	}
}

func TestScopedKey(t *testing.T) {
	a := tele.RandomID(scopedKey("send_message", 1, "k"), 0)
	if a != tele.RandomID(scopedKey("send_message", 1, "k"), 0) {
		t.Error("same key, different random_id: a retry would send twice")
	}
	if a == tele.RandomID(scopedKey("send_message", 2, "k"), 0) {
		t.Error("same key in another chat collides")
	}
	if a == tele.RandomID(scopedKey("send_file", 1, "k"), 0) {
		t.Error("same key for another tool collides")
	}
	if scopedKey("send_message", 1, "") != "" {
		t.Error("no key must stay no key")
	}
	if tele.RandomID("", 0) == tele.RandomID("", 0) {
		t.Error("keyless random ids repeat")
	}
}

func TestGuardTimeoutAdvice(t *testing.T) {
	timeout := func(context.Context, *mcp.CallToolRequest, sendMessageIn) (*mcp.CallToolResult, sentOut, error) {
		return nil, sentOut{}, context.DeadlineExceeded
	}
	_, _, err := guard(writeAdd, timeout)(context.Background(), nil, sendMessageIn{IdempotencyKey: "k1"})
	if err == nil || !strings.Contains(err.Error(), "same idempotency_key is safe") {
		t.Errorf("with key: %v", err)
	}
	_, _, err = guard(writeAdd, timeout)(context.Background(), nil, sendMessageIn{})
	if err == nil || !strings.Contains(err.Error(), "check list_messages before retrying") {
		t.Errorf("without key: %v", err)
	}
	read := func(context.Context, *mcp.CallToolRequest, chatIn) (*mcp.CallToolResult, okOut, error) {
		return nil, okOut{}, context.DeadlineExceeded
	}
	if _, _, err := guard(readTelegram, read)(context.Background(), nil, chatIn{}); err == nil || strings.Contains(err.Error(), "gone through") {
		t.Errorf("read timeout: %v", err)
	}
}

func TestGuardExplainsTelegramErrors(t *testing.T) {
	h := func(context.Context, *mcp.CallToolRequest, chatIn) (*mcp.CallToolResult, okOut, error) {
		return nil, okOut{}, tgerr.New(400, "MESSAGE_NOT_MODIFIED")
	}
	_, _, err := guard(writeChange, h)(context.Background(), nil, chatIn{})
	if err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Errorf("got %v", err)
	}
}

func TestDuplicateSendIsSuccess(t *testing.T) {
	log, err := audit.Open(filepath.Join(t.TempDir(), "audit.log"), "test")
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	s := &Server{audit: log}
	_, out, err := s.sent("send_message", 5, 0, 0, "", tgerr.New(400, "RANDOM_ID_DUPLICATE"))
	if err != nil || !out.AlreadySent {
		t.Fatalf("duplicate: %+v %v", out, err)
	}
	if _, _, err := s.sent("send_message", 5, 0, 0, "", errors.New("boom")); err == nil {
		t.Fatal("real error swallowed")
	}
}
