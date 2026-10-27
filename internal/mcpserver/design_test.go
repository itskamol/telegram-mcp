package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/itskamol/telegram-mcp-go/internal/audit"
	"github.com/itskamol/telegram-mcp-go/internal/feed"
	"github.com/itskamol/telegram-mcp-go/internal/storage"
	"github.com/itskamol/telegram-mcp-go/internal/tele"
	"github.com/itskamol/telegram-mcp-go/internal/tele/teletest"
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

// is_contact is always present where it is known, and absent where it is
// not: a missing field must not be read as "not a contact".
func TestIsContactOnlyWhereKnown(t *testing.T) {
	if b, _ := json.Marshal(tele.Chat{ID: 5}); !strings.Contains(string(b), `"is_contact":false`) {
		t.Errorf("false is_contact dropped: %s", b)
	}
	tools := toolNames(t, connect(t, Policy{MaxDelete: 20}))
	for name, want := range map[string]bool{"list_chats": true, "search_contacts": true, "resolve_username": false} {
		b, _ := json.Marshal(tools[name].OutputSchema)
		if got := strings.Contains(string(b), "is_contact"); got != want {
			t.Errorf("%s output has is_contact = %v, want %v", name, got, want)
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
	read := func(context.Context, *mcp.CallToolRequest, markReadIn) (*mcp.CallToolResult, okOut, error) {
		return nil, okOut{}, context.DeadlineExceeded
	}
	if _, _, err := guard(readTelegram, read)(context.Background(), nil, markReadIn{}); err == nil || strings.Contains(err.Error(), "gone through") {
		t.Errorf("read timeout: %v", err)
	}
}

func TestGuardExplainsTelegramErrors(t *testing.T) {
	h := func(context.Context, *mcp.CallToolRequest, markReadIn) (*mcp.CallToolResult, okOut, error) {
		return nil, okOut{}, tgerr.New(400, "MESSAGE_NOT_MODIFIED")
	}
	_, _, err := guard(writeChange, h)(context.Background(), nil, markReadIn{})
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

func TestIDListsAreTypedArrays(t *testing.T) {
	for _, tool := range []string{"delete_messages", "forward_messages"} {
		b, _ := json.Marshal(toolNames(t, connect(t, Policy{MaxDelete: 20}))[tool].InputSchema)
		var s struct {
			Properties map[string]map[string]any
		}
		_ = json.Unmarshal(b, &s)
		if ty := s.Properties["message_ids"]["type"]; ty != "array" {
			t.Errorf("%s.message_ids type = %v, want array", tool, ty)
		}
	}
}

func TestOwnDeletesLeaveTheBuffer(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(filepath.Join(t.TempDir(), "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store, err := feed.NewStore(ctx, db, "default")
	if err != nil {
		t.Fatal(err)
	}
	const group = -1001889276137
	now := int(time.Now().Unix())
	for _, m := range []struct {
		kind string
		chat int64
		id   int
	}{{storage.KindChannel, group, 187}, {storage.KindChannel, group, 188}, {storage.KindUser, 5, 187}} {
		if _, err := store.Upsert(ctx, m.kind, 0, tele.Message{ChatID: m.chat, ID: m.id}, now); err != nil {
			t.Fatal(err)
		}
	}
	notify := feed.NewNotifier()
	ready := notify.Ready()
	s := &Server{feed: &Feed{Store: store, Notify: notify}}
	if err := s.forgetDeleted(ctx, tele.Resolved{Kind: storage.KindChannel, Marked: group}, []int{187}); err != nil {
		t.Fatal(err)
	}
	b, _ := store.Fetch(ctx, "c", 0, 10)
	left := map[string]bool{}
	for _, it := range b.Items {
		left[fmt.Sprintf("%d/%d", it.ChatID, it.ID)] = true
	}
	if left[fmt.Sprintf("%d/187", group)] || !left[fmt.Sprintf("%d/188", group)] || !left["5/187"] {
		t.Fatalf("after deleting %d/187: %v", group, left)
	}
	select {
	case <-ready:
	default:
		t.Error("waiters not woken by the delete")
	}
	if err := (&Server{}).forgetDeleted(ctx, tele.Resolved{}, []int{1}); err != nil {
		t.Error("no feed should be a no-op")
	}
}

func TestFeedChatIDsIsTypedArray(t *testing.T) {
	for _, schema := range []any{inputSchema[newMessagesIn](), inputSchema[feedFilterIn]()} {
		b, _ := json.Marshal(schema)
		var s struct {
			Properties map[string]struct {
				Type  any
				Items struct{ Type any }
			}
		}
		_ = json.Unmarshal(b, &s)
		p := s.Properties["chat_ids"]
		if p.Type != "array" || fmt.Sprint(p.Items.Type) != "[integer string]" {
			t.Fatalf("chat_ids schema: %s", b)
		}
	}
}

func newFeed(t *testing.T) *Feed {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store, err := feed.NewStore(context.Background(), db, "default")
	if err != nil {
		t.Fatal(err)
	}
	return &Feed{Store: store, Notify: feed.NewNotifier()}
}

// set_feed_filter only writes the local buffer's consumer table, like
// download_media writes local disk, so read-only mode keeps it.
func TestFeedToolsInReadOnly(t *testing.T) {
	tools := toolNames(t, connectFeed(t, Policy{ReadOnly: true, MaxDelete: 20}, newFeed(t)))
	if tl := tools["get_new_messages"]; tl == nil || !tl.Annotations.ReadOnlyHint {
		t.Error("get_new_messages missing or not read-only")
	}
	if tl := tools["set_feed_filter"]; tl == nil || tl.Annotations.ReadOnlyHint || *tl.Annotations.DestructiveHint {
		t.Error("set_feed_filter missing or mis-annotated")
	}
}

// Telegram never marks a Saved Messages message as out (seen live), so an
// agent reading and writing there would answer itself. The feed tools must
// say so where an agent picks a direction.
func TestFeedToolsWarnAboutSavedMessages(t *testing.T) {
	tools := toolNames(t, connectFeed(t, Policy{MaxDelete: 20}, newFeed(t)))
	if d := tools["get_new_messages"].Description; !strings.Contains(d, "Saved Messages") {
		t.Errorf("get_new_messages does not mention Saved Messages: %q", d)
	}
	b, _ := json.Marshal(tools["set_feed_filter"].InputSchema)
	var s struct {
		Properties map[string]struct{ Description string }
	}
	_ = json.Unmarshal(b, &s)
	if d := s.Properties["direction"].Description; !strings.Contains(d, "Saved Messages") {
		t.Errorf("set_feed_filter direction does not mention Saved Messages: %q", d)
	}
}

func TestFeedFilterContract(t *testing.T) {
	cs := connectFeed(t, Policy{MaxDelete: 20}, newFeed(t))
	call := func(tool string, args map[string]any) (map[string]any, string) {
		t.Helper()
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		text := res.Content[0].(*mcp.TextContent).Text
		if res.IsError {
			return nil, text
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(text), &out); err != nil {
			t.Fatalf("%s: %v in %s", tool, err, text)
		}
		return out, ""
	}

	// A bad direction fails before any chat is resolved (there is no
	// Telegram service here: resolving would panic).
	if _, errText := call("set_feed_filter", map[string]any{"chat_ids": []any{"@someone"}, "direction": "incoming"}); !strings.Contains(errText, "direction must be") {
		t.Fatalf("bad direction: %q", errText)
	}
	out, _ := call("set_feed_filter", map[string]any{"consumer": "bot", "direction": "in"})
	if out["direction"] != "in" || fmt.Sprint(out["chat_ids"]) != "[]" || out["consumer"] != "bot" {
		t.Fatalf("set: %v", out)
	}
	// The deprecated chat_ids of get_new_messages changes the chats only.
	out, _ = call("get_new_messages", map[string]any{"consumer": "bot", "chat_ids": []any{}})
	if f := out["filter"].(map[string]any); f["direction"] != "in" {
		t.Fatalf("deprecated chat_ids reset the direction: %v", f)
	}
	// Only a consumer: the current filter, unchanged.
	if out, _ = call("set_feed_filter", map[string]any{"consumer": "bot"}); out["direction"] != "in" {
		t.Fatalf("read back: %v", out)
	}
}

// A client can keep tool definitions from an older server for the rest of
// a session (seen in Claude Code after an upgrade and a reconnect). get_me's
// description names the version it came from and its answer the version
// that runs, so an agent can tell that its tool list is stale.
func TestGetMeExposesVersion(t *testing.T) {
	desc := toolNames(t, connect(t, Policy{MaxDelete: 20}))["get_me"].Description
	if !strings.Contains(desc, "telegram-mcp test") || !strings.Contains(desc, "stale") {
		t.Errorf("get_me description lacks its version or the stale-list advice: %q", desc)
	}
	s := &Server{svc: &tele.Service{SelfID: 1, Me: tele.Me{Name: "K"}}, version: "9.9.9"}
	_, out, err := s.getMe(context.Background(), nil, noInput{})
	if err != nil || out.Version != "9.9.9" {
		t.Errorf("get_me answer: %+v %v", out, err)
	}
}

// Editing a message to the text it already has is what a retry after a
// timeout does; Telegram answers MESSAGE_NOT_MODIFIED, which is success.
func TestEditToSameTextSucceeds(t *testing.T) {
	log, err := audit.Open(filepath.Join(t.TempDir(), "audit.log"), "test")
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	inv := &teletest.Invoker{Err: tgerr.New(400, "MESSAGE_NOT_MODIFIED")}
	s := &Server{svc: &tele.Service{API: tg.NewClient(inv), SelfID: 1}, audit: log}
	in := editMessageIn{Chat: "me", MessageID: 5, Text: "same"}
	if _, out, err := s.editMessage(context.Background(), nil, in); err != nil || !out.OK || !out.Unchanged {
		t.Fatalf("same text: %+v %v", out, err)
	}
	inv.Err = tgerr.New(400, "MESSAGE_AUTHOR_REQUIRED")
	if _, out, err := s.editMessage(context.Background(), nil, in); err == nil {
		t.Fatalf("other errors must still fail: %+v", out)
	}
}

// With watch_all a new consumer would read every chat of the account, so
// it must choose its chats before the first message reaches the agent.
func TestWatchAllNeedsAChoice(t *testing.T) {
	fd := newFeed(t)
	fd.Scope = feed.Scope{All: true}
	cs := connectFeed(t, Policy{MaxDelete: 20}, fd)
	call := func(tool string, args map[string]any) (bool, string) {
		t.Helper()
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		return res.IsError, res.Content[0].(*mcp.TextContent).Text
	}
	if isErr, text := call("get_new_messages", map[string]any{"consumer": "pos"}); !isErr || !strings.Contains(text, "set_feed_filter") {
		t.Fatalf("unchosen consumer under watch_all: %v %q", isErr, text)
	}
	if isErr, text := call("set_feed_filter", map[string]any{"consumer": "pos", "chat_ids": []any{}}); isErr {
		t.Fatalf("choose every chat: %q", text)
	}
	if isErr, text := call("get_new_messages", map[string]any{"consumer": "pos"}); isErr {
		t.Fatalf("after choosing: %q", text)
	}

	// Without watch_all the user listed the chats; nothing to choose.
	cs = connectFeed(t, Policy{MaxDelete: 20}, newFeed(t))
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_new_messages", Arguments: map[string]any{"consumer": "pos"}})
	if err != nil || res.IsError {
		t.Fatalf("watch_chat_ids consumer: %v %v", res, err)
	}
}
