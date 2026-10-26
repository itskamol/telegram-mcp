// Package sanitize cleans text that strangers control (messages, names,
// titles) before it reaches the model.
//
// Tool results are JSON, so user content sits inside string values and cannot
// pose as structure. On top of that, invisible and control characters are
// removed (they can hide instructions from a human reviewing the transcript)
// and long values are truncated.
package sanitize

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Limits in runes.
const (
	MaxText = 4096
	MaxName = 256
)

// Text cleans a message body.
func Text(s string) string { return clean(s, MaxText, true) }

// Name cleans a single-line value: names, titles, usernames.
func Name(s string) string { return clean(s, MaxName, false) }

func clean(s string, limit int, multiline bool) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	var b strings.Builder
	b.Grow(min(len(s), limit*4))
	n := 0
	for _, r := range s {
		if drop(r, multiline) {
			continue
		}
		if !multiline && (r == '\n' || r == '\t') {
			r = ' '
		}
		if n == limit {
			b.WriteString("…[truncated]")
			break
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

func drop(r rune, multiline bool) bool {
	switch {
	case r == '\n' || r == '\t':
		return false
	case unicode.IsControl(r): // C0, DEL, C1, including \r
		return true
	case r >= 0x200B && r <= 0x200F, // zero-width space/joiners, LRM/RLM
		r >= 0x202A && r <= 0x202E,   // bidi embeddings and overrides
		r >= 0x2060 && r <= 0x2064,   // word joiner, invisible operators
		r >= 0x2066 && r <= 0x2069,   // bidi isolates
		r == 0x2028 || r == 0x2029,   // line/paragraph separators
		r == 0x00AD,                  // soft hyphen
		r == 0xFEFF,                  // BOM / zero-width no-break space
		r >= 0xE0000 && r <= 0xE007F: // tag characters ("ASCII smuggling")
		return true
	}
	return false
}
