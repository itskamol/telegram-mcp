package tele

import (
	"testing"

	"github.com/gotd/td/tg"

	"github.com/itskamol/telegram-mcp-go/internal/storage"
)

func TestMarkedIDRoundTrip(t *testing.T) {
	cases := []struct {
		kind   string
		id     int64
		marked int64
	}{
		{storage.KindUser, 777000, 777000},
		{storage.KindChat, 4567, -4567},
		{storage.KindChannel, 1234567890, -1001234567890},
	}
	for _, c := range cases {
		if got := MarkedID(c.kind, c.id); got != c.marked {
			t.Errorf("MarkedID(%s, %d) = %d, want %d", c.kind, c.id, got, c.marked)
		}
		kind, id := splitMarked(c.marked)
		if kind != c.kind || id != c.id {
			t.Errorf("splitMarked(%d) = %s %d", c.marked, kind, id)
		}
	}
}

func TestSentID(t *testing.T) {
	if got := sentID(&tg.UpdateShortSentMessage{ID: 5}, 1); got != 5 {
		t.Fatalf("short: %d", got)
	}
	u := &tg.Updates{Updates: []tg.UpdateClass{
		&tg.UpdateMessageID{ID: 10, RandomID: 1},
		&tg.UpdateMessageID{ID: 11, RandomID: 2},
	}}
	if got := sentID(u, 2); got != 11 {
		t.Fatalf("updates: %d", got)
	}
	if got := sentID(u, 3); got != 0 {
		t.Fatalf("unknown random id: %d", got)
	}
}

func TestMediaKind(t *testing.T) {
	voice := &tg.MessageMediaDocument{Document: &tg.Document{Attributes: []tg.DocumentAttributeClass{
		&tg.DocumentAttributeAudio{Voice: true},
	}}}
	if got := mediaKind(voice); got != "voice" {
		t.Fatalf("voice: %q", got)
	}
	if got := mediaKind(&tg.MessageMediaPhoto{}); got != "Photo" {
		t.Fatalf("photo: %q", got)
	}
	if got := mediaKind(nil); got != "" {
		t.Fatalf("nil: %q", got)
	}
}
