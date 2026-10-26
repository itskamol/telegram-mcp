package tele

import (
	"context"
	"fmt"
	"strings"

	"github.com/gotd/td/telegram/message/entity"
	"github.com/gotd/td/telegram/message/html"
	"github.com/gotd/td/tg"

	"github.com/itskamol/telegram-mcp-go/internal/sanitize"
	"github.com/itskamol/telegram-mcp-go/internal/storage"
)

// Parse modes accepted by the send and edit tools.
const (
	ModePlain = "plain"
	ModeHTML  = "html"
)

// Formatted is message text with its Telegram formatting entities.
type Formatted struct {
	Text     string
	Entities []tg.MessageEntityClass
}

// ParseHTML converts Telegram-flavoured HTML (b, i, u, s, code, pre, a,
// blockquote, tg-spoiler) into plain text plus entities. resolve maps
// tg://user?id= links to users; a link it cannot resolve is kept as plain
// text (gotd drops the entity rather than failing).
func ParseHTML(text string, resolve entity.UserResolver) (Formatted, error) {
	if resolve == nil {
		resolve = func(id int64) (tg.InputUserClass, error) {
			return nil, fmt.Errorf("user mentions are not supported here")
		}
	}
	var b entity.Builder
	if err := html.HTML(strings.NewReader(text), &b, html.Options{UserResolver: resolve}); err != nil {
		return Formatted{}, fmt.Errorf("invalid HTML: %w", err)
	}
	t, ents := b.Complete()
	return Formatted{Text: t, Entities: ents}, nil
}

// MentionResolver resolves tg://user?id= links through the peer cache, so
// only users the account has already seen can be mentioned.
func (s *Service) MentionResolver(ctx context.Context) entity.UserResolver {
	return func(id int64) (tg.InputUserClass, error) {
		p, err := s.Peers.Get(ctx, storage.KindUser, id)
		if err != nil {
			return nil, fmt.Errorf("cannot mention user %d: not among known users", id)
		}
		return &tg.InputUser{UserID: p.ID, AccessHash: p.AccessHash}, nil
	}
}

// Topic is one forum topic of a supergroup.
type Topic struct {
	ID         int    `json:"id"`
	Title      string `json:"title"`
	Unread     int    `json:"unread,omitempty"`
	TopMessage int    `json:"top_message"`
	Closed     bool   `json:"closed,omitempty"`
	Pinned     bool   `json:"pinned,omitempty"`
}

// Topics lists forum topics of a supergroup with topics enabled.
func (s *Service) Topics(ctx context.Context, r Resolved, query string, limit int) ([]Topic, error) {
	if r.Kind != storage.KindChannel {
		return nil, fmt.Errorf("chat %d is not a supergroup, so it has no topics", r.Marked)
	}
	res, err := s.API.MessagesGetForumTopics(ctx, &tg.MessagesGetForumTopicsRequest{Peer: r.Input, Q: query, Limit: limit})
	if err != nil {
		return nil, fmt.Errorf("get topics: %w", err)
	}
	if err := s.cache(ctx, res.Users, res.Chats); err != nil {
		return nil, err
	}
	out := make([]Topic, 0, len(res.Topics))
	for _, tc := range res.Topics {
		if t, ok := tc.(*tg.ForumTopic); ok {
			out = append(out, Topic{ID: t.ID, Title: sanitize.Name(t.Title), Unread: t.UnreadCount,
				TopMessage: t.TopMessage, Closed: t.Closed, Pinned: t.Pinned})
		}
	}
	return out, nil
}

// TopicHistory lists messages of one forum topic, newest first.
func (s *Service) TopicHistory(ctx context.Context, r Resolved, topicID, limit, offsetID int) ([]Message, error) {
	res, err := s.API.MessagesGetReplies(ctx, &tg.MessagesGetRepliesRequest{
		Peer: r.Input, MsgID: topicID, Limit: limit, OffsetID: offsetID,
	})
	if err != nil {
		return nil, fmt.Errorf("get topic messages: %w", err)
	}
	return s.messages(ctx, res)
}
