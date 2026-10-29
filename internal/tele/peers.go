// Package tele wraps the raw Telegram API calls the MCP tools need: peer
// resolution backed by the SQLite cache, and conversion of Telegram objects
// into plain, sanitized structs.
package tele

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/gotd/td/constant"
	"github.com/gotd/td/tg"

	"github.com/itskamol/telegram-mcp-go/internal/sanitize"
	"github.com/itskamol/telegram-mcp-go/internal/storage"
)

// Service is the Telegram side of the tools.
type Service struct {
	API *tg.Client
	// Pools reaches other DCs for downloads; nil downloads via API only.
	Pools Pools

	dcs   dcCache // DC pools opened for downloads, see download.go
	Peers *storage.Peers
	// SelfID and Me describe the logged-in account; set once at startup.
	SelfID int64
	Me     Me
}

// Me is the logged-in account.
type Me struct {
	Name     string
	Username string
}

// NewService builds a Service for the logged-in user and caches it as a peer.
func NewService(ctx context.Context, api *tg.Client, peers *storage.Peers, self *tg.User) (*Service, error) {
	s := &Service{API: api, Peers: peers, SelfID: self.ID,
		Me: Me{Name: sanitize.Name(userName(self)), Username: self.Username}}
	s.dcs.base = ctx
	return s, s.cache(ctx, []tg.UserClass{self}, nil)
}

// Self returns the logged-in account.
func (s *Service) Self(context.Context) (Me, error) { return s.Me, nil }

// Resolved is a peer ready for API calls.
type Resolved struct {
	Input    tg.InputPeerClass
	Marked   int64 // Bot API style ID: user > 0, chat < 0, channel -100...
	Kind     string
	Plain    int64
	Title    string
	Username string
}

// ChatOf describes a resolved peer the way list_chats does.
func ChatOf(r Resolved) Chat {
	return Chat{ID: r.Marked, Kind: r.Kind, Title: sanitize.Name(r.Title), Username: sanitize.Name(r.Username)}
}

// MarkedID converts a plain ID into the Bot API style ID the tools expose.
func MarkedID(kind string, id int64) int64 {
	var m constant.TDLibPeerID
	switch kind {
	case storage.KindUser:
		m.User(id)
	case storage.KindChat:
		m.Chat(id)
	default:
		m.Channel(id)
	}
	return int64(m)
}

// splitMarked reverses MarkedID.
func splitMarked(marked int64) (kind string, id int64) {
	m := constant.TDLibPeerID(marked)
	switch {
	case m.IsUser():
		return storage.KindUser, m.ToPlain()
	case m.IsChat():
		return storage.KindChat, m.ToPlain()
	default:
		return storage.KindChannel, m.ToPlain()
	}
}

// Resolve turns a tool's chat argument into a peer. Accepted forms: "me",
// a marked numeric ID ("123", "-456", "-100789") or a username with or
// without '@'. Users and channels missing from the cache trigger one dialog
// refresh before giving up.
func (s *Service) Resolve(ctx context.Context, ref string) (Resolved, error) {
	ref = strings.TrimSpace(ref)
	switch {
	case ref == "":
		return Resolved{}, errors.New("chat is required")
	case ref == "me" || ref == "self":
		return Resolved{Input: &tg.InputPeerSelf{}, Marked: s.SelfID, Kind: storage.KindUser, Plain: s.SelfID, Title: "Saved Messages"}, nil
	}

	if marked, err := strconv.ParseInt(ref, 10, 64); err == nil {
		return s.resolveID(ctx, marked)
	}
	return s.ResolveUsername(ctx, strings.TrimPrefix(ref, "@"))
}

func (s *Service) resolveID(ctx context.Context, marked int64) (Resolved, error) {
	kind, id := splitMarked(marked)
	if kind == storage.KindUser && id == s.SelfID {
		return Resolved{Input: &tg.InputPeerSelf{}, Marked: marked, Kind: kind, Plain: id, Title: "Saved Messages"}, nil
	}
	p, err := s.lookup(ctx, kind, id)
	if errors.Is(err, storage.ErrPeerNotCached) {
		if _, err := s.Dialogs(ctx, 200, ""); err != nil {
			return Resolved{}, err
		}
		p, err = s.lookup(ctx, kind, id)
	}
	if errors.Is(err, storage.ErrPeerNotCached) {
		return Resolved{}, fmt.Errorf("chat %d not found: it is not among your recent chats; "+
			"use list_chats, search_contacts or a @username", marked)
	}
	if err != nil {
		return Resolved{}, err
	}
	return toResolved(p), nil
}

// lookup finds a cached peer. A positive id that is no known user is
// also tried as a channel or basic group: people and other tools often
// quote a group's id without Bot API's -100 prefix (1889276137 for
// -1001889276137), and a user wins when both exist.
func (s *Service) lookup(ctx context.Context, kind string, id int64) (storage.Peer, error) {
	p, err := s.Peers.Get(ctx, kind, id)
	if kind != storage.KindUser || !errors.Is(err, storage.ErrPeerNotCached) {
		return p, err
	}
	for _, k := range []string{storage.KindChannel, storage.KindChat} {
		if p, err := s.Peers.Get(ctx, k, id); !errors.Is(err, storage.ErrPeerNotCached) {
			return p, err
		}
	}
	return storage.Peer{}, storage.ErrPeerNotCached
}

// ResolveUsername asks Telegram (the cache only short-circuits repeat calls).
func (s *Service) ResolveUsername(ctx context.Context, username string) (Resolved, error) {
	if username == "" {
		return Resolved{}, errors.New("empty username")
	}
	if p, err := s.Peers.ByUsername(ctx, username); err == nil {
		return toResolved(p), nil
	}
	res, err := s.API.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: username})
	if err != nil {
		return Resolved{}, fmt.Errorf("resolve @%s: %w", username, err)
	}
	if err := s.cache(ctx, res.Users, res.Chats); err != nil {
		return Resolved{}, err
	}
	kind, id := peerKind(res.Peer)
	p, err := s.Peers.Get(ctx, kind, id)
	if err != nil {
		return Resolved{}, fmt.Errorf("resolve @%s: %w", username, err)
	}
	return toResolved(p), nil
}

func toResolved(p storage.Peer) Resolved {
	r := Resolved{Marked: MarkedID(p.Kind, p.ID), Kind: p.Kind, Plain: p.ID, Title: p.Title, Username: p.Username}
	switch p.Kind {
	case storage.KindUser:
		r.Input = &tg.InputPeerUser{UserID: p.ID, AccessHash: p.AccessHash}
	case storage.KindChat:
		r.Input = &tg.InputPeerChat{ChatID: p.ID}
	default:
		r.Input = &tg.InputPeerChannel{ChannelID: p.ID, AccessHash: p.AccessHash}
	}
	return r
}

// InputChannel returns the channel form of a resolved channel or supergroup.
func (r Resolved) InputChannel() (*tg.InputChannel, bool) {
	c, ok := r.Input.(*tg.InputPeerChannel)
	if !ok {
		return nil, false
	}
	return &tg.InputChannel{ChannelID: c.ChannelID, AccessHash: c.AccessHash}, true
}

func peerKind(p tg.PeerClass) (string, int64) {
	switch p := p.(type) {
	case *tg.PeerUser:
		return storage.KindUser, p.UserID
	case *tg.PeerChat:
		return storage.KindChat, p.ChatID
	case *tg.PeerChannel:
		return storage.KindChannel, p.ChannelID
	}
	return "", 0
}

// ChatOfPeer returns the marked chat id and peer kind of a message's peer.
func ChatOfPeer(p tg.PeerClass) (int64, string) {
	kind, id := peerKind(p)
	return MarkedID(kind, id), kind
}

// markedPeer converts a tg.PeerClass straight to a marked ID.
func markedPeer(p tg.PeerClass) int64 {
	if p == nil {
		return 0
	}
	kind, id := peerKind(p)
	return MarkedID(kind, id)
}

// cache stores every user and chat that came with a response.
func (s *Service) cache(ctx context.Context, users []tg.UserClass, chats []tg.ChatClass) error {
	peers := make([]storage.Peer, 0, len(users)+len(chats))
	for _, u := range users {
		if u, ok := u.(*tg.User); ok {
			peers = append(peers, storage.Peer{
				Kind: storage.KindUser, ID: u.ID, AccessHash: u.AccessHash,
				Username: u.Username, Title: userName(u),
			})
		}
	}
	for _, c := range chats {
		switch c := c.(type) {
		case *tg.Chat:
			peers = append(peers, storage.Peer{Kind: storage.KindChat, ID: c.ID, Title: c.Title})
		case *tg.ChatForbidden:
			peers = append(peers, storage.Peer{Kind: storage.KindChat, ID: c.ID, Title: c.Title})
		case *tg.Channel:
			peers = append(peers, storage.Peer{
				Kind: storage.KindChannel, ID: c.ID, AccessHash: c.AccessHash,
				Username: c.Username, Title: c.Title,
			})
		case *tg.ChannelForbidden:
			peers = append(peers, storage.Peer{
				Kind: storage.KindChannel, ID: c.ID, AccessHash: c.AccessHash, Title: c.Title,
			})
		}
	}
	return s.Peers.Put(ctx, peers)
}

func userName(u *tg.User) string {
	name := strings.TrimSpace(u.FirstName + " " + u.LastName)
	if name == "" {
		name = "Deleted Account"
	}
	return name
}
