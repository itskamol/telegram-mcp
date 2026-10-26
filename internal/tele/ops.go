package tele

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/gotd/td/tg"

	"github.com/itskamol/telegram-mcp-go/internal/sanitize"
	"github.com/itskamol/telegram-mcp-go/internal/storage"
)

// Chat is one dialog as the tools return it.
type Chat struct {
	ID       int64  `json:"id"`
	Kind     string `json:"kind"`
	Title    string `json:"title"`
	Username string `json:"username,omitempty"`
	Unread   int    `json:"unread,omitempty"`
}

// Message is one message as the tools return it.
type Message struct {
	ID       int    `json:"id"`
	ChatID   int64  `json:"chat_id"`
	Date     string `json:"date"`
	FromID   int64  `json:"from_id,omitempty"`
	FromName string `json:"from_name,omitempty"`
	Out      bool   `json:"out,omitempty"`
	Text     string `json:"text,omitempty"`
	ReplyTo  int    `json:"reply_to,omitempty"`
	Media    string `json:"media,omitempty"`
	Edited   bool   `json:"edited,omitempty"`
	Forward  bool   `json:"forwarded,omitempty"`
	Service  string `json:"service,omitempty"`
}

// Dialogs lists the most recent chats and refreshes the peer cache.
func (s *Service) Dialogs(ctx context.Context, limit int) ([]Chat, error) {
	res, err := s.API.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
		OffsetPeer: &tg.InputPeerEmpty{},
		Limit:      limit,
	})
	if err != nil {
		return nil, fmt.Errorf("get dialogs: %w", err)
	}
	d, ok := res.AsModified()
	if !ok {
		return nil, nil
	}
	if err := s.cache(ctx, d.GetUsers(), d.GetChats()); err != nil {
		return nil, err
	}

	out := make([]Chat, 0, len(d.GetDialogs()))
	for _, dc := range d.GetDialogs() {
		dlg, ok := dc.(*tg.Dialog)
		if !ok {
			continue // folders
		}
		kind, id := peerKind(dlg.Peer)
		c := Chat{ID: MarkedID(kind, id), Kind: kind, Unread: dlg.UnreadCount}
		if p, err := s.Peers.Get(ctx, kind, id); err == nil {
			c.Title, c.Username = sanitize.Name(p.Title), sanitize.Name(p.Username)
		}
		if kind == storage.KindUser && id == s.SelfID {
			c.Title = "Saved Messages"
		}
		out = append(out, c)
	}
	return out, nil
}

// SearchContacts searches your contacts and global usernames by name.
func (s *Service) SearchContacts(ctx context.Context, q string, limit int) ([]Chat, error) {
	res, err := s.API.ContactsSearch(ctx, &tg.ContactsSearchRequest{Q: q, Limit: limit})
	if err != nil {
		return nil, fmt.Errorf("search contacts: %w", err)
	}
	if err := s.cache(ctx, res.Users, res.Chats); err != nil {
		return nil, err
	}
	var out []Chat
	for _, p := range slices.Concat(res.MyResults, res.Results) {
		kind, id := peerKind(p)
		pr, err := s.Peers.Get(ctx, kind, id)
		if err != nil {
			continue
		}
		c := Chat{ID: MarkedID(kind, id), Kind: kind, Title: sanitize.Name(pr.Title), Username: sanitize.Name(pr.Username)}
		if !slices.ContainsFunc(out, func(x Chat) bool { return x.ID == c.ID }) {
			out = append(out, c)
		}
	}
	return out, nil
}

// History returns messages older than offsetID (0 = newest), newest first.
func (s *Service) History(ctx context.Context, r Resolved, limit, offsetID int) ([]Message, error) {
	res, err := s.API.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
		Peer: r.Input, Limit: limit, OffsetID: offsetID,
	})
	if err != nil {
		return nil, fmt.Errorf("get history: %w", err)
	}
	return s.messages(ctx, res)
}

// Search finds messages in one chat containing q.
func (s *Service) Search(ctx context.Context, r Resolved, q string, limit int) ([]Message, error) {
	res, err := s.API.MessagesSearch(ctx, &tg.MessagesSearchRequest{
		Peer: r.Input, Q: q, Filter: &tg.InputMessagesFilterEmpty{}, Limit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("search messages: %w", err)
	}
	return s.messages(ctx, res)
}

// Get fetches specific messages of one chat. IDs that do not exist or
// belong to another chat are left out.
func (s *Service) Get(ctx context.Context, r Resolved, ids []int) ([]Message, error) {
	in := make([]tg.InputMessageClass, len(ids))
	for i, id := range ids {
		in[i] = &tg.InputMessageID{ID: id}
	}
	var res tg.MessagesMessagesClass
	var err error
	if ch, ok := r.InputChannel(); ok {
		res, err = s.API.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{Channel: ch, ID: in})
	} else {
		// Outside channels message IDs are per account, not per chat: this
		// returns whatever chat each ID lives in, so filter by chat below.
		res, err = s.API.MessagesGetMessages(ctx, in)
	}
	if err != nil {
		return nil, fmt.Errorf("get messages: %w", err)
	}
	msgs, err := s.messages(ctx, res)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(msgs, func(m Message) bool { return m.ChatID != r.Marked || m.Service == "empty" }), nil
}

// Send posts text, optionally as a reply, and returns the new message ID.
func (s *Service) Send(ctx context.Context, r Resolved, text string, replyTo int) (int, error) {
	req := &tg.MessagesSendMessageRequest{Peer: r.Input, Message: text, RandomID: randomID()}
	if replyTo != 0 {
		req.ReplyTo = &tg.InputReplyToMessage{ReplyToMsgID: replyTo}
	}
	upd, err := s.API.MessagesSendMessage(ctx, req)
	if err != nil {
		return 0, fmt.Errorf("send message: %w", err)
	}
	return sentID(upd, req.RandomID), nil
}

// Forward copies messages from one chat to another and returns the new IDs.
func (s *Service) Forward(ctx context.Context, from Resolved, ids []int, to Resolved) ([]int, error) {
	rnd := make([]int64, len(ids))
	for i := range rnd {
		rnd[i] = randomID()
	}
	upd, err := s.API.MessagesForwardMessages(ctx, &tg.MessagesForwardMessagesRequest{
		FromPeer: from.Input, ID: ids, RandomID: rnd, ToPeer: to.Input,
	})
	if err != nil {
		return nil, fmt.Errorf("forward: %w", err)
	}
	out := make([]int, 0, len(rnd))
	for _, r := range rnd {
		if id := sentID(upd, r); id != 0 {
			out = append(out, id)
		}
	}
	return out, nil
}

// Edit replaces a message's text.
func (s *Service) Edit(ctx context.Context, r Resolved, id int, text string) error {
	_, err := s.API.MessagesEditMessage(ctx, &tg.MessagesEditMessageRequest{Peer: r.Input, ID: id, Message: text})
	if err != nil {
		return fmt.Errorf("edit message: %w", err)
	}
	return nil
}

// Delete removes messages of one chat. revoke deletes them for everyone in
// private chats and basic groups; in channels deletion is always global.
func (s *Service) Delete(ctx context.Context, r Resolved, ids []int, revoke bool) error {
	if ch, ok := r.InputChannel(); ok {
		_, err := s.API.ChannelsDeleteMessages(ctx, &tg.ChannelsDeleteMessagesRequest{Channel: ch, ID: ids})
		if err != nil {
			return fmt.Errorf("delete messages: %w", err)
		}
		return nil
	}
	// messages.deleteMessages takes no peer, so a wrong ID would delete a
	// message in some other chat. Only delete IDs confirmed to be in r.
	found, err := s.Get(ctx, r, ids)
	if err != nil {
		return err
	}
	if len(found) != len(ids) {
		var missing []int
		for _, id := range ids {
			if !slices.ContainsFunc(found, func(m Message) bool { return m.ID == id }) {
				missing = append(missing, id)
			}
		}
		return fmt.Errorf("messages %v are not in chat %d; nothing was deleted", missing, r.Marked)
	}
	if _, err := s.API.MessagesDeleteMessages(ctx, &tg.MessagesDeleteMessagesRequest{ID: ids, Revoke: revoke}); err != nil {
		return fmt.Errorf("delete messages: %w", err)
	}
	return nil
}

// MarkRead marks the whole chat as read.
func (s *Service) MarkRead(ctx context.Context, r Resolved) error {
	var err error
	if ch, ok := r.InputChannel(); ok {
		_, err = s.API.ChannelsReadHistory(ctx, &tg.ChannelsReadHistoryRequest{Channel: ch})
	} else {
		_, err = s.API.MessagesReadHistory(ctx, &tg.MessagesReadHistoryRequest{Peer: r.Input})
	}
	if err != nil {
		return fmt.Errorf("mark as read: %w", err)
	}
	return nil
}

func (s *Service) messages(ctx context.Context, res tg.MessagesMessagesClass) ([]Message, error) {
	m, ok := res.AsModified()
	if !ok {
		return nil, nil
	}
	if err := s.cache(ctx, m.GetUsers(), m.GetChats()); err != nil {
		return nil, err
	}
	names := map[int64]string{}
	for _, u := range m.GetUsers() {
		if u, ok := u.(*tg.User); ok {
			names[MarkedID(storage.KindUser, u.ID)] = sanitize.Name(userName(u))
		}
	}
	for _, c := range m.GetChats() {
		if c, ok := c.(*tg.Channel); ok {
			names[MarkedID(storage.KindChannel, c.ID)] = sanitize.Name(c.Title)
		}
	}

	out := make([]Message, 0, len(m.GetMessages()))
	for _, mc := range m.GetMessages() {
		switch msg := mc.(type) {
		case *tg.Message:
			x := Message{
				ID:      msg.ID,
				ChatID:  markedPeer(msg.PeerID),
				Date:    unix(msg.Date),
				Out:     msg.Out,
				Text:    sanitize.Text(msg.Message),
				Media:   mediaKind(msg.Media),
				Edited:  msg.EditDate != 0,
				Forward: msg.FwdFrom.FromID != nil || msg.FwdFrom.FromName != "" || msg.FwdFrom.Date != 0,
			}
			if from, ok := msg.GetFromID(); ok {
				x.FromID = markedPeer(from)
			} else if !msg.Out {
				x.FromID = x.ChatID // private chats and channel posts omit from_id
			}
			x.FromName = names[x.FromID]
			if h, ok := msg.ReplyTo.(*tg.MessageReplyHeader); ok {
				x.ReplyTo = h.ReplyToMsgID
			}
			out = append(out, x)
		case *tg.MessageService:
			x := Message{ID: msg.ID, ChatID: markedPeer(msg.PeerID), Date: unix(msg.Date), Out: msg.Out,
				Service: strings.TrimPrefix(msg.Action.TypeName(), "messageAction")}
			if from, ok := msg.GetFromID(); ok {
				x.FromID = markedPeer(from)
				x.FromName = names[x.FromID]
			}
			out = append(out, x)
		case *tg.MessageEmpty:
			out = append(out, Message{ID: msg.ID, Service: "empty"})
		}
	}
	return out, nil
}

func mediaKind(m tg.MessageMediaClass) string {
	switch m := m.(type) {
	case nil, *tg.MessageMediaEmpty:
		return ""
	case *tg.MessageMediaDocument:
		if d, ok := m.Document.(*tg.Document); ok {
			for _, a := range d.Attributes {
				switch a := a.(type) {
				case *tg.DocumentAttributeAudio:
					if a.Voice {
						return "voice"
					}
					return "audio"
				case *tg.DocumentAttributeVideo:
					if a.RoundMessage {
						return "video_note"
					}
					return "video"
				case *tg.DocumentAttributeSticker:
					return "sticker"
				case *tg.DocumentAttributeAnimated:
					return "gif"
				}
			}
		}
		return "document"
	default:
		return strings.TrimPrefix(m.TypeName(), "messageMedia")
	}
}

// sentID finds the ID Telegram assigned to the message sent with randomID.
func sentID(u tg.UpdatesClass, randomID int64) int {
	var updates []tg.UpdateClass
	switch u := u.(type) {
	case *tg.UpdateShortSentMessage:
		return u.ID
	case *tg.Updates:
		updates = u.Updates
	case *tg.UpdatesCombined:
		updates = u.Updates
	}
	for _, x := range updates {
		if x, ok := x.(*tg.UpdateMessageID); ok && x.RandomID == randomID {
			return x.ID
		}
	}
	return 0
}

func randomID() int64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(errors.New("crypto/rand failed"))
	}
	return int64(binary.LittleEndian.Uint64(b[:]))
}

func unix(t int) string { return time.Unix(int64(t), 0).UTC().Format(time.RFC3339) }
