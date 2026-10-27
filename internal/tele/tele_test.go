package tele

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

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
	if got := mediaKind(&tg.MessageMediaPhoto{}); got != "photo" {
		t.Fatalf("photo: %q", got)
	}
	if got := mediaKind(nil); got != "" {
		t.Fatalf("nil: %q", got)
	}
}

// Telegram lists a document's attributes in no fixed order, and several
// kinds carry a video attribute: the kind must not depend on which comes
// first (seen live: a video sticker came back as "video").
func TestMediaKindPrefersStickerAndGIF(t *testing.T) {
	doc := func(attrs ...tg.DocumentAttributeClass) tg.MessageMediaClass {
		return &tg.MessageMediaDocument{Document: &tg.Document{Attributes: attrs}}
	}
	name := &tg.DocumentAttributeFilename{FileName: "x"}
	cases := map[string]struct {
		media tg.MessageMediaClass
		want  string
	}{
		"video sticker":    {doc(&tg.DocumentAttributeVideo{W: 512, H: 512}, name, &tg.DocumentAttributeSticker{Alt: "🤝"}), "sticker"},
		"animated sticker": {doc(&tg.DocumentAttributeImageSize{W: 512, H: 512}, &tg.DocumentAttributeSticker{Alt: "😂"}, name), "sticker"},
		"gif":              {doc(&tg.DocumentAttributeVideo{W: 422, H: 280}, &tg.DocumentAttributeAnimated{}, name), "gif"},
		"round video":      {doc(&tg.DocumentAttributeVideo{RoundMessage: true}), "video_note"},
		"video":            {doc(&tg.DocumentAttributeVideo{}, name), "video"},
		"voice":            {doc(&tg.DocumentAttributeAudio{Voice: true}), "voice"},
		"music":            {doc(name, &tg.DocumentAttributeAudio{}), "audio"},
		"file":             {doc(name), "document"},
	}
	for n, c := range cases {
		if got := mediaKind(c.media); got != c.want {
			t.Errorf("%s: %q, want %q", n, got, c.want)
		}
	}
}

func TestPickPhotoSize(t *testing.T) {
	sizes := []tg.PhotoSizeClass{
		&tg.PhotoStrippedSize{Type: "i"},
		&tg.PhotoSize{Type: "s", W: 90, H: 60, Size: 1000},
		&tg.PhotoSize{Type: "x", W: 800, H: 533, Size: 50000},
		&tg.PhotoSizeProgressive{Type: "y", W: 1280, H: 853, Sizes: []int{10, 90000}},
		&tg.PhotoSizeProgressive{Type: "w", W: 2560, H: 1706, Sizes: []int{10, 300000}},
	}
	if got := pickPhotoSize(sizes, 0); got.Type != "w" || got.Size != 300000 {
		t.Fatalf("largest: %+v", got)
	}
	if got := pickPhotoSize(sizes, 1280); got.Type != "y" {
		t.Fatalf("<=1280: %+v", got)
	}
	if got := pickPhotoSize(sizes, 50); got.Type != "s" {
		t.Fatalf("fallback to smallest: %+v", got)
	}
}

func TestParseHTML(t *testing.T) {
	f, err := ParseHTML(`<b>Salom</b>, <a href="https://example.uz">havola</a> 1 &lt; 2 <i>👋 hi</i>`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.Text != "Salom, havola 1 < 2 👋 hi" {
		t.Fatalf("text %q", f.Text)
	}
	if len(f.Entities) != 3 {
		t.Fatalf("entities %v", f.Entities)
	}
	if b, ok := f.Entities[0].(*tg.MessageEntityBold); !ok || b.Offset != 0 || b.Length != 5 {
		t.Errorf("bold %+v", f.Entities[0])
	}
	if u, ok := f.Entities[1].(*tg.MessageEntityTextURL); !ok || u.URL != "https://example.uz" {
		t.Errorf("link %+v", f.Entities[1])
	}
	// Offsets are UTF-16 units: the emoji counts as 2.
	if i, ok := f.Entities[2].(*tg.MessageEntityItalic); !ok || i.Offset != 20 || i.Length != 5 {
		t.Errorf("italic %+v", f.Entities[2])
	}

	if _, err := ParseHTML("<b>x</i>", nil); err == nil {
		t.Error("mismatched tags accepted")
	}
	// gotd drops a mention it cannot resolve instead of failing: the text
	// stays, nobody gets mentioned.
	if f, err := ParseHTML(`<a href="tg://user?id=1">x</a>`, nil); err != nil || f.Text != "x" || len(f.Entities) != 0 {
		t.Errorf("unresolvable mention: %q %v %v", f.Text, f.Entities, err)
	}
}

func TestExplain(t *testing.T) {
	cases := map[string]string{
		"MESSAGE_NOT_MODIFIED": "nothing was changed",
		"PEER_ID_INVALID":      "list_chats",
		"FLOOD_WAIT_30":        "wait 30 seconds",
		"SLOWMODE_WAIT_10":     "slow mode",
	}
	for msg, want := range cases {
		if got := Explain(tgerr.New(400, msg)).Error(); !strings.Contains(got, want) || !strings.Contains(got, "[") {
			t.Errorf("%s: %q", msg, got)
		}
	}
	unknown := tgerr.New(400, "SOMETHING_NEW")
	if Explain(unknown) != error(unknown) {
		t.Error("unknown RPC error changed")
	}
	plain := errors.New("plain")
	if Explain(plain) != plain {
		t.Error("non-RPC error changed")
	}
	if !IsDuplicate(fmt.Errorf("send: %w", tgerr.New(400, "RANDOM_ID_DUPLICATE"))) {
		t.Error("wrapped duplicate not detected")
	}
}

func TestFormatModes(t *testing.T) {
	f, err := Format("**qalin** va [havola](https://example.uz) `kod`", ModeMarkdown, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.Text != "qalin va havola kod" || len(f.Entities) != 3 {
		t.Fatalf("markdown: %q %v", f.Text, f.Entities)
	}
	// snake_case identifiers must not turn into italics.
	if f, _ := Format("call get_me_now please", ModeMarkdown, nil); f.Text != "call get_me_now please" || len(f.Entities) != 0 {
		t.Errorf("snake_case: %q %v", f.Text, f.Entities)
	}
	if f, _ := Format("**not bold**", ModePlain, nil); f.Text != "**not bold**" {
		t.Errorf("plain changed text: %q", f.Text)
	}
	if _, err := Format("x", "rich", nil); err == nil {
		t.Error("unknown mode accepted")
	}
}

func TestDatesAreLocal(t *testing.T) {
	got := unix(1_700_000_000)
	want := time.Unix(1_700_000_000, 0).Local().Format(time.RFC3339)
	if got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestMatches(t *testing.T) {
	c := Chat{Title: "Ish Guruhi", Username: "work_team"}
	for _, q := range []string{"ish", "guruhi", "work", "team"} {
		if !matches(c, q) {
			t.Errorf("%q did not match", q)
		}
	}
	if matches(c, "oila") {
		t.Error("unrelated query matched")
	}
}

func TestHiddenLinks(t *testing.T) {
	// "👋 " is 3 UTF-16 units, so "havola" starts at offset 3.
	text := "👋 havola va https://x.uz"
	ents := []tg.MessageEntityClass{
		&tg.MessageEntityTextURL{Offset: 3, Length: 6, URL: "https://github.com/itskamol"},
		&tg.MessageEntityURL{Offset: 13, Length: 12},              // visible already
		&tg.MessageEntityTextURL{Offset: 50, Length: 3, URL: "x"}, // out of range: skipped
	}
	got := hiddenLinks(text, ents)
	if len(got) != 1 || got[0].Text != "havola" || got[0].URL != "https://github.com/itskamol" {
		t.Fatalf("got %+v", got)
	}
	if hiddenLinks("plain", nil) != nil {
		t.Error("links invented for plain text")
	}
	if got := mediaKind(&tg.MessageMediaWebPage{}); got != "link_preview" {
		t.Errorf("webpage kind = %q", got)
	}
}

func TestReplyAndTopic(t *testing.T) {
	cases := []struct {
		h              tg.MessageReplyHeader
		reply, topicID int
	}{
		{tg.MessageReplyHeader{ReplyToMsgID: 5}, 5, 0},                                        // plain reply
		{tg.MessageReplyHeader{ForumTopic: true, ReplyToMsgID: 33}, 0, 33},                    // message in topic 33
		{tg.MessageReplyHeader{ForumTopic: true, ReplyToMsgID: 40, ReplyToTopID: 33}, 40, 33}, // reply inside topic 33
	}
	for _, c := range cases {
		if r, tp := replyAndTopic(&c.h); r != c.reply || tp != c.topicID {
			t.Errorf("%+v: got reply %d topic %d", c.h, r, tp)
		}
	}
	if replyTarget(0, generalTopic) != nil {
		t.Error("General topic sent as a reply target")
	}
	if r, ok := replyTarget(7, generalTopic).(*tg.InputReplyToMessage); !ok || r.ReplyToMsgID != 7 || r.TopMsgID != 0 {
		t.Errorf("reply in General: %+v", r)
	}
	if topicTarget(generalTopic) != 0 || topicTarget(33) != 33 {
		t.Error("topicTarget")
	}
}

// gotd sets the request's flag bits from its fields when it encodes, so
// no_webpage reaches Telegram only if Encode turns NoWebpage into flag 1.
func TestSendRequestNoPreview(t *testing.T) {
	for _, off := range []bool{false, true} {
		req := sendRequest(Resolved{Input: &tg.InputPeerSelf{}}, Formatted{Text: "https://example.com"}, 0, 0, 1, off)
		if err := req.Encode(&bin.Buffer{}); err != nil {
			t.Fatal(err)
		}
		if req.Flags.Has(1) != off {
			t.Errorf("noPreview=%v: no_webpage flag = %v", off, req.Flags.Has(1))
		}
	}
}
