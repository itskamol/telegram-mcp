package mcpserver

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/itskamol/telegram-mcp-go/internal/feed"
)

// Feed gives get_new_messages its buffer; nil when no chat is watched.
type Feed struct {
	Store  *feed.Store
	Notify *feed.Notifier
}

// maxWait stays below ToolTimeout so a long poll ends with an answer.
const maxWait = 45 * time.Second

var consumerRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

type newMessagesIn struct {
	Consumer    string `json:"consumer,omitempty" jsonschema:"name of this reader, kept by the server across sessions; default \"default\". Use one name per task, e.g. daily-digest"`
	AckCursor   int64  `json:"ack_cursor,omitempty" jsonschema:"cursor from the previous answer, once those messages are handled; omit it and the same messages come again"`
	Limit       int    `json:"limit,omitempty" jsonschema:"max messages, default 50, max 100"`
	WaitSeconds int    `json:"wait_seconds,omitempty" jsonschema:"if nothing is new, wait up to this many seconds (max 45) for a message instead of answering empty"`
}

func (s *Server) getNewMessages(ctx context.Context, _ *mcp.CallToolRequest, in newMessagesIn) (*mcp.CallToolResult, feed.Batch, error) {
	consumer := in.Consumer
	if consumer == "" {
		consumer = "default"
	}
	if !consumerRe.MatchString(consumer) {
		return nil, feed.Batch{}, errors.New("consumer must be 1-64 letters, digits, '.', '_' or '-'")
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
