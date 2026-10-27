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
	"unicode/utf16"

	"github.com/gotd/td/telegram/message/peer"
	"github.com/gotd/td/telegram/query/dialogs"
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
	// IsContact marks users in the account's contact list, so an agent can
	// tell "my Ali" from a stranger with the same name. Always present: a
	// missing field read as "unknown", not as "not a contact".
	IsContact bool `json:"is_contact"`
}

// maxDialogScan bounds how many dialogs a title search walks through.
const maxDialogScan = 1000

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
	// TopicID is the forum topic the message is in; ReplyTo is then only
	// set for a real reply inside the topic.
	TopicID int    `json:"topic_id,omitempty"`
	Media   string `json:"media,omitempty"`
	// StickerEmoji is the emoji a sticker stands for, so its meaning needs
	// no download.
	StickerEmoji string `json:"sticker_emoji,omitempty"`
	Edited       bool   `json:"edited,omitempty"`
	Forward      bool   `json:"forwarded,omitempty"`
	Service      string `json:"service,omitempty"`
	// Links lists hyperlinks whose target is hidden behind other text
	// ([text](url)); plain URLs are already visible in Text.
	Links []Link `json:"links,omitempty"`
	// EditDate is Telegram's raw edit time, for ordering edits; 0 if never edited.
	EditDate int `json:"-"`
}

// Link is a hyperlink hidden behind text.
type Link struct {
	Text string `json:"text"`
	URL  string `json:"url"`
}

// Dialogs lists chats, most recent first, and refreshes the peer cache.
// With a query only chats whose title or username contains it (case
// insensitive) are returned, searching up to maxDialogScan dialogs.
func (s *Service) Dialogs(ctx context.Context, limit int, query string) ([]Chat, error) {
	query = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(query), "@"))
	scan := limit
	if query != "" {
		scan = maxDialogScan
	}
	// Telegram returns at most 100 dialogs per call; the iterator pages.
	it := dialogs.NewQueryBuilder(s.API).GetDialogs().BatchSize(min(scan, 100)).Iter()
	var out []Chat
	for seen := 0; seen < scan && len(out) < limit && it.Next(ctx); seen++ {
		e := it.Value()
		if err := s.cache(ctx, usersOf(e.Entities), chatsOf(e.Entities)); err != nil {
			return nil, err
		}
		dlg, ok := e.Dialog.(*tg.Dialog)
		if !ok {
			continue // folders
		}
		kind, id := peerKind(dlg.Peer)
		c := Chat{ID: MarkedID(kind, id), Kind: kind, Unread: dlg.UnreadCount}
		if p, err := s.Peers.Get(ctx, kind, id); err == nil {
			c.Title, c.Username = sanitize.Name(p.Title), sanitize.Name(p.Username)
		}
		if u, ok := e.Entities.Users()[id]; ok && kind == storage.KindUser {
			c.IsContact = u.Contact
		}
		if kind == storage.KindUser && id == s.SelfID {
			c.Title = "Saved Messages"
		}
		if query != "" && !matches(c, query) {
			continue
		}
		out = append(out, c)
	}
	if err := it.Err(); err != nil {
		return nil, fmt.Errorf("get dialogs: %w", err)
	}
	return out, nil
}

func matches(c Chat, query string) bool {
	return strings.Contains(strings.ToLower(c.Title), query) || strings.Contains(strings.ToLower(c.Username), query)
}

func usersOf(e peer.Entities) []tg.UserClass {
	out := make([]tg.UserClass, 0, len(e.Users()))
	for _, u := range e.Users() {
		out = append(out, u)
	}
	return out
}

func chatsOf(e peer.Entities) []tg.ChatClass {
	out := make([]tg.ChatClass, 0, len(e.Chats())+len(e.Channels()))
	for _, c := range e.Chats() {
		out = append(out, c)
	}
	for _, c := range e.Channels() {
		out = append(out, c)
	}
	return out
}

// SearchContacts searches contacts and known chats first, then public
// users and chats. Contacts are listed first and marked IsContact.
func (s *Service) SearchContacts(ctx context.Context, q string, limit int) ([]Chat, error) {
	res, err := s.API.ContactsSearch(ctx, &tg.ContactsSearchRequest{Q: q, Limit: limit})
	if err != nil {
		return nil, fmt.Errorf("search contacts: %w", err)
	}
	if err := s.cache(ctx, res.Users, res.Chats); err != nil {
		return nil, err
	}
	contact := map[int64]bool{}
	for _, u := range res.Users {
		if u, ok := u.(*tg.User); ok {
			contact[u.ID] = u.Contact
		}
	}
	var out []Chat
	for _, p := range slices.Concat(res.MyResults, res.Results) {
		kind, id := peerKind(p)
		pr, err := s.Peers.Get(ctx, kind, id)
		if err != nil {
			continue
		}
		c := Chat{ID: MarkedID(kind, id), Kind: kind, Title: sanitize.Name(pr.Title), Username: sanitize.Name(pr.Username),
			IsContact: kind == storage.KindUser && contact[id]}
		if !slices.ContainsFunc(out, func(x Chat) bool { return x.ID == c.ID }) {
			out = append(out, c)
		}
	}
	slices.SortStableFunc(out, func(a, b Chat) int { return boolRank(b.IsContact) - boolRank(a.IsContact) })
	return out, nil
}

func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
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

// Search finds messages in one chat containing q, newest first, older
// than offsetID when it is set.
func (s *Service) Search(ctx context.Context, r Resolved, q string, limit, offsetID int) ([]Message, error) {
	res, err := s.API.MessagesSearch(ctx, &tg.MessagesSearchRequest{
		Peer: r.Input, Q: q, Filter: &tg.InputMessagesFilterEmpty{}, Limit: limit, OffsetID: offsetID,
	})
	if err != nil {
		return nil, fmt.Errorf("search messages: %w", err)
	}
	return s.messages(ctx, res)
}

// Get fetches specific messages of one chat. IDs that do not exist or
// belong to another chat are left out.
func (s *Service) Get(ctx context.Context, r Resolved, ids []int) ([]Message, error) {
	res, err := s.fetch(ctx, r, ids)
	if err != nil {
		return nil, err
	}
	msgs, err := s.messages(ctx, res)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(msgs, func(m Message) bool { return m.ChatID != r.Marked || m.Service == "empty" }), nil
}

// Send posts text, optionally as a reply and/or into a forum topic, and
// returns the new message ID. randomID comes from RandomID; noPreview
// sends a link in the text without its preview.
func (s *Service) Send(ctx context.Context, r Resolved, f Formatted, replyTo, topicID int, randomID int64, noPreview bool) (int, error) {
	req := sendRequest(r, f, replyTo, topicID, randomID, noPreview)
	upd, err := s.API.MessagesSendMessage(ctx, req)
	if err != nil {
		return 0, fmt.Errorf("send message: %w", err)
	}
	return sentID(upd, req.RandomID), nil
}

func sendRequest(r Resolved, f Formatted, replyTo, topicID int, randomID int64, noPreview bool) *tg.MessagesSendMessageRequest {
	return &tg.MessagesSendMessageRequest{
		Peer: r.Input, Message: f.Text, Entities: f.Entities, RandomID: randomID,
		ReplyTo: replyTarget(replyTo, topicID), NoWebpage: noPreview,
	}
}

// replyAndTopic splits a reply header. In forums every message points at
// its topic's first message, which is not a reply: only a header naming
// both a topic (ReplyToTopID) and a message is one.
func replyAndTopic(h *tg.MessageReplyHeader) (replyTo, topicID int) {
	switch {
	case !h.ForumTopic:
		return h.ReplyToMsgID, 0
	case h.ReplyToTopID != 0:
		return h.ReplyToMsgID, h.ReplyToTopID
	default:
		return 0, h.ReplyToMsgID
	}
}

// generalTopic is the id of a forum's General topic, which is where
// messages without a topic go; it must not be sent as a reply target.
const generalTopic = 1

// replyTarget builds reply_to for a reply and/or a forum topic; nil if neither.
func replyTarget(replyTo, topicID int) tg.InputReplyToClass {
	if topicID == generalTopic {
		topicID = 0
	}
	switch {
	case replyTo != 0 && topicID != 0:
		return &tg.InputReplyToMessage{ReplyToMsgID: replyTo, TopMsgID: topicID}
	case replyTo != 0:
		return &tg.InputReplyToMessage{ReplyToMsgID: replyTo}
	case topicID != 0:
		// A topic is addressed by replying to its first message.
		return &tg.InputReplyToMessage{ReplyToMsgID: topicID}
	}
	return nil
}

// Forward copies messages from one chat to another, into topicID when it
// is a forum topic, and returns the new IDs. With an idempotency key a
// retry returns the first forward instead of forwarding again.
func (s *Service) Forward(ctx context.Context, from Resolved, ids []int, to Resolved, topicID int, key string) ([]int, error) {
	rnd := make([]int64, len(ids))
	for i := range rnd {
		rnd[i] = RandomID(key, i)
	}
	upd, err := s.API.MessagesForwardMessages(ctx, &tg.MessagesForwardMessagesRequest{
		FromPeer: from.Input, ID: ids, RandomID: rnd, ToPeer: to.Input,
		TopMsgID: topicTarget(topicID),
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

// topicTarget is the top_msg_id for a topic; 0 for none or General.
func topicTarget(topicID int) int {
	if topicID == generalTopic {
		return 0
	}
	return topicID
}

// Edit replaces a message's text.
func (s *Service) Edit(ctx context.Context, r Resolved, id int, f Formatted) error {
	_, err := s.API.MessagesEditMessage(ctx, &tg.MessagesEditMessageRequest{
		Peer: r.Input, ID: id, Message: f.Text, Entities: f.Entities,
	})
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

// MarkRead marks a chat as read up to and including message maxID, or
// all of it when maxID is 0.
func (s *Service) MarkRead(ctx context.Context, r Resolved, maxID int) error {
	var err error
	if ch, ok := r.InputChannel(); ok {
		_, err = s.API.ChannelsReadHistory(ctx, &tg.ChannelsReadHistoryRequest{Channel: ch, MaxID: maxID})
	} else {
		_, err = s.API.MessagesReadHistory(ctx, &tg.MessagesReadHistoryRequest{Peer: r.Input, MaxID: maxID})
	}
	if err != nil {
		return fmt.Errorf("mark as read: %w", err)
	}
	return nil
}

// Convert turns one message that came with an update, plus the users and
// chats the update carried, into a Message, caching those peers.
func (s *Service) Convert(ctx context.Context, msg tg.MessageClass, e tg.Entities) (Message, bool, error) {
	res := &tg.MessagesMessages{Messages: []tg.MessageClass{msg}}
	for _, u := range e.Users {
		res.Users = append(res.Users, u)
	}
	for _, c := range e.Chats {
		res.Chats = append(res.Chats, c)
	}
	for _, c := range e.Channels {
		res.Chats = append(res.Chats, c)
	}
	out, err := s.messages(ctx, res)
	if err != nil || len(out) == 0 || out[0].Service == "empty" {
		return Message{}, false, err
	}
	return out[0], true, nil
}

// TopicOf tells which forum topic a message is in. ok is false when that
// cannot be told, which callers filtering by topic must treat as "drop".
// In forums every message outside General carries a forum_topic reply
// header, so a message without one, or with a plain reply, is in General.
func TopicOf(m *tg.Message) (topicID int, ok bool) {
	switch h := m.ReplyTo.(type) {
	case nil:
		return generalTopic, true
	case *tg.MessageReplyHeader:
		if _, t := replyAndTopic(h); t != 0 {
			return t, true
		}
		return generalTopic, true
	default:
		return 0, false // story replies and future header kinds
	}
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
				Links:   hiddenLinks(msg.Message, msg.Entities),
			}
			x.EditDate = msg.EditDate
			if st, ok := sticker(msg.Media); ok {
				x.StickerEmoji = sanitize.Name(st.Alt)
			}
			if from, ok := msg.GetFromID(); ok {
				x.FromID = markedPeer(from)
			} else if !msg.Out {
				x.FromID = x.ChatID // private chats and channel posts omit from_id
			}
			x.FromName = s.senderName(ctx, names, x.FromID)
			if h, ok := msg.ReplyTo.(*tg.MessageReplyHeader); ok {
				x.ReplyTo, x.TopicID = replyAndTopic(h)
			}
			out = append(out, x)
		case *tg.MessageService:
			x := Message{ID: msg.ID, ChatID: markedPeer(msg.PeerID), Date: unix(msg.Date), Out: msg.Out,
				Service: strings.TrimPrefix(msg.Action.TypeName(), "messageAction")}
			if from, ok := msg.GetFromID(); ok {
				x.FromID = markedPeer(from)
				x.FromName = s.senderName(ctx, names, x.FromID)
			}
			out = append(out, x)
		case *tg.MessageEmpty:
			out = append(out, Message{ID: msg.ID, Service: "empty"})
		}
	}
	return out, nil
}

// senderName names a sender from the response's users and chats, or from
// the peer cache when the response did not carry them: Telegram pushes
// private and basic-group messages as short updates without any users,
// which left from_name empty in get_new_messages (seen live).
func (s *Service) senderName(ctx context.Context, names map[int64]string, marked int64) string {
	if name, ok := names[marked]; ok || marked == 0 {
		return name
	}
	kind, id := splitMarked(marked)
	p, err := s.Peers.Get(ctx, kind, id)
	if err != nil {
		return "" // best effort: an unknown sender still has from_id
	}
	return sanitize.Name(p.Title)
}

// sticker returns a message's sticker attribute, if it is a sticker.
func sticker(m tg.MessageMediaClass) (*tg.DocumentAttributeSticker, bool) {
	md, ok := m.(*tg.MessageMediaDocument)
	if !ok {
		return nil, false
	}
	d, ok := md.Document.(*tg.Document)
	if !ok {
		return nil, false
	}
	for _, a := range d.Attributes {
		if st, ok := a.(*tg.DocumentAttributeSticker); ok {
			return st, true
		}
	}
	return nil, false
}

// hiddenLinks extracts [text](url) links. Entity offsets count UTF-16
// code units, so the text is indexed in UTF-16.
func hiddenLinks(text string, ents []tg.MessageEntityClass) []Link {
	var units []uint16
	var out []Link
	for _, e := range ents {
		u, ok := e.(*tg.MessageEntityTextURL)
		if !ok {
			continue
		}
		if units == nil {
			units = utf16.Encode([]rune(text))
		}
		if u.Offset < 0 || u.Length < 0 || u.Offset+u.Length > len(units) {
			continue
		}
		out = append(out, Link{
			Text: sanitize.Name(string(utf16.Decode(units[u.Offset : u.Offset+u.Length]))),
			URL:  sanitize.Name(u.URL),
		})
	}
	return out
}

func mediaKind(m tg.MessageMediaClass) string {
	switch m := m.(type) {
	case nil, *tg.MessageMediaEmpty:
		return ""
	case *tg.MessageMediaWebPage:
		// A link preview, not a file: get_media_info reports it as not
		// downloadable, and the name says so up front.
		return "link_preview"
	case *tg.MessageMediaDocument:
		d, ok := m.Document.(*tg.Document)
		if !ok {
			return "document"
		}
		// Attributes come in no fixed order, and video stickers and GIFs
		// carry a video attribute too: collect them all, then decide, most
		// specific kind first.
		var sticker, gif, round, video, voice, audio bool
		for _, a := range d.Attributes {
			switch a := a.(type) {
			case *tg.DocumentAttributeSticker:
				sticker = true
			case *tg.DocumentAttributeAnimated:
				gif = true
			case *tg.DocumentAttributeVideo:
				video, round = true, round || a.RoundMessage
			case *tg.DocumentAttributeAudio:
				audio, voice = true, voice || a.Voice
			}
		}
		switch {
		case sticker:
			return "sticker"
		case gif:
			return "gif"
		case round:
			return "video_note"
		case video:
			return "video"
		case voice:
			return "voice"
		case audio:
			return "audio"
		}
		return "document"
	default:
		// messageMediaGeoLive -> "geolive", matching the lowercase kinds above.
		return strings.ToLower(strings.TrimPrefix(m.TypeName(), "messageMedia"))
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

// unix formats a Telegram timestamp in the machine's local time zone with
// its offset (2026-10-27T09:30:00+05:00), so "today" means the user's today.
func unix(t int) string { return time.Unix(int64(t), 0).Local().Format(time.RFC3339) }
