package speech

import (
	"regexp"
	"strings"
	"unicode"
)

// The model is asked to write plain spoken Italian, but a prompt is a request,
// not a guarantee. Sanitize is the guarantee: whatever only makes sense on a
// screen is removed before the text reaches the TTS, which would otherwise
// read "asterisco asterisco" or spell out a URL.
var (
	mdLink     = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	bareURL    = regexp.MustCompile(`(?i)\b(?:https?://|www\.)\S+`)
	listMarker = regexp.MustCompile(`(?m)^\s*(?:[-*+•·]|\d{1,2}[.)])\s+`)
	heading    = regexp.MustCompile(`(?m)^\s*#{1,6}\s*`)
	quoteMark  = regexp.MustCompile(`(?m)^\s*>\s?`)
	markup     = strings.NewReplacer("**", "", "__", "", "*", "", "`", "", "~~", "", "|", " ")
)

// Sanitize strips markdown, list markers, links, URLs and emoji from one
// sentence, and collapses whitespace.
func Sanitize(text string) string {
	text = mdLink.ReplaceAllString(text, "$1")
	text = bareURL.ReplaceAllString(text, "")
	text = listMarker.ReplaceAllString(text, "")
	text = heading.ReplaceAllString(text, "")
	text = quoteMark.ReplaceAllString(text, "")
	text = markup.Replace(text)
	text = strings.Map(dropEmoji, text)
	return strings.Join(strings.Fields(text), " ")
}

// dropEmoji removes pictographs and the invisible characters that build them
// (variation selectors, zero-width joiners, skin tones). The degree sign is a
// symbol too, but it carries meaning, so it stays.
func dropEmoji(r rune) rune {
	switch {
	case r == '°':
		return r
	case r == 0x200D, r >= 0xFE00 && r <= 0xFE0F, r >= 0x1F3FB && r <= 0x1F3FF:
		return -1
	case unicode.Is(unicode.So, r), unicode.Is(unicode.Cs, r):
		return -1
	}
	return r
}
