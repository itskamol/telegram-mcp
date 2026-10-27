package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/itskamol/telegram-mcp-go/internal/feed"
)

// Feed gives get_new_messages its buffer; nil when no chat is watched.
type Feed struct {
	Store  *feed.Store
	Notify *feed.Notifier
	Scope  feed.Scope // what config.toml lets the server buffer
}

// maxWait stays below ToolTimeout so a long poll ends with an answer.
const maxWait = 45 * time.Second

var consumerRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

type newMessagesIn struct {
	Consumer    string `json:"consumer,omitempty" jsonschema:"name of this reader, kept by the server across sessions; default \"default\". Use one name per task, e.g. daily-digest"`
	AckCursor   int64  `json:"ack_cursor,omitempty" jsonschema:"cursor from the previous answer, once those messages are handled: acknowledges every message up to and including it, not the ones this call returns. Omit it and the same messages come again"`
	Limit       int    `json:"limit,omitempty" jsonschema:"max messages, default 50, max 100"`
	WaitSeconds int    `json:"wait_seconds,omitempty" jsonschema:"if nothing is new, wait up to this many seconds (max 45) for a message instead of answering empty"`
	// Deprecated: set_feed_filter replaces it. It stays for one release so
	// that sessions still holding the old tool list keep working; it does
	// exactly what set_feed_filter(chat_ids) does, direction untouched.
	ChatIDs *[]ChatRef `json:"chat_ids,omitempty" jsonschema:"deprecated, use set_feed_filter: the same as set_feed_filter with these chat_ids, saved for this consumer, its direction kept"`
}

// consumerName applies the default name and checks it.
func consumerName(name string) (string, error) {
	if name == "" {
		return "default", nil
	}
	if !consumerRe.MatchString(name) {
		return "", errors.New("consumer must be 1-64 letters, digits, '.', '_' or '-'")
	}
	return name, nil
}

func (s *Server) getNewMessages(ctx context.Context, _ *mcp.CallToolRequest, in newMessagesIn) (*mcp.CallToolResult, feed.Batch, error) {
	consumer, err := consumerName(in.Consumer)
	if err != nil {
		return nil, feed.Batch{}, err
	}
	if in.ChatIDs != nil {
		if _, _, err := s.setFilter(ctx, consumer, in.ChatIDs, ""); err != nil {
			return nil, feed.Batch{}, err
		}
	}
	limit := clamp(in.Limit, 50, 100)
	wait := min(time.Duration(max(in.WaitSeconds, 0))*time.Second, maxWait)
	timer := time.NewTimer(wait)
	defer timer.Stop()

	for {
		// Take Ready before reading, or a message stored in between would
		// not wake this wait.
		ready := s.feed.Notify.Ready()
		b, err := s.feed.Store.Fetch(ctx, consumer, in.AckCursor, limit)
		if err != nil || len(b.Items) > 0 || wait == 0 {
			return nil, b, err
		}
		select {
		case <-ready:
		case <-timer.C:
			return nil, b, nil
		case <-ctx.Done():
			return nil, feed.Batch{}, ctx.Err()
		}
	}
}

type feedFilterIn struct {
	Consumer string `json:"consumer,omitempty" jsonschema:"consumer to set, as named in get_new_messages; default \"default\""`
	// A pointer: an empty list (back to all chats) differs from no list.
	ChatIDs   *[]ChatRef `json:"chat_ids,omitempty" jsonschema:"read only these chats; [] reads every watched chat again; omit to keep the current list. Only chats the user set to be watched in config.toml can be chosen"`
	Direction string     `json:"direction,omitempty" jsonschema:"in (messages to this account), out (its own messages) or both; omit to keep the current one"`
}

type feedFilterOut struct {
	Consumer string `json:"consumer"`
	feed.Filter
	Cursor int64 `json:"cursor" jsonschema:"this consumer's acknowledged position; the filter applies to messages after it"`
}

func (s *Server) setFeedFilter(ctx context.Context, _ *mcp.CallToolRequest, in feedFilterIn) (*mcp.CallToolResult, feedFilterOut, error) {
	consumer, err := consumerName(in.Consumer)
	if err != nil {
		return nil, feedFilterOut{}, err
	}
	f, cursor, err := s.setFilter(ctx, consumer, in.ChatIDs, in.Direction)
	if err != nil {
		return nil, feedFilterOut{}, err
	}
	return nil, feedFilterOut{Consumer: consumer, Filter: f, Cursor: cursor}, nil
}

// setFilter checks and stores a consumer's filter; nil chats or an empty
// direction keep that part. The direction is checked first, so a bad one
// fails before any chat is resolved through Telegram.
func (s *Server) setFilter(ctx context.Context, consumer string, refs *[]ChatRef, direction string) (feed.Filter, int64, error) {
	if err := feed.CheckDirection(direction); err != nil {
		return feed.Filter{}, 0, err
	}
	var chats *[]int64
	if refs != nil {
		ids, err := s.feedChats(ctx, *refs)
		if err != nil {
			return feed.Filter{}, 0, err
		}
		chats = &ids
	}
	return s.feed.Store.SetFilter(ctx, consumer, chats, direction)
}

// feedChats resolves a chat filter. It can only narrow what the user set
// to be buffered: a chat outside it would just never get messages, which
// an agent would read as a quiet chat.
func (s *Server) feedChats(ctx context.Context, refs []ChatRef) ([]int64, error) {
	chats := make([]int64, 0, len(refs))
	for _, ref := range refs {
		r, err := s.chat(ctx, ref)
		if err != nil {
			return nil, err
		}
		if !s.feed.Scope.Has(r.Marked) {
			return nil, fmt.Errorf("chat %d is not watched; ask the user to add it to watch_chat_ids in config.toml", r.Marked)
		}
		chats = append(chats, r.Marked)
	}
	return chats, nil
}
