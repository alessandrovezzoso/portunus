// Package speech turns the model's text into text fit for the ear: cut into
// sentences as it streams, stripped of what only makes sense on a screen, and
// with numbers spelled out the way an Italian speaker says them.
//
// Everything here is a pure function of its input, so it is tested with
// tables of examples and never touches audio.
package speech

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Splitter collects text as it streams from the model and hands out whole
// sentences, so speech can start with the first one while the model is still
// writing the next.
//
// Two rules shape it:
//   - A sentence ends at . ! ? ; or a line break, but only when whitespace
//     follows: "3.5" and "14:30" are never cut, and neither are abbreviations
//     such as "Sig." or "Dott.".
//   - Sentences shorter than MinChars are merged with the next one. Kokoro
//     sounds wrong on very short utterances, and the holding phrase already
//     covers the wait for the first sentence.
//
// A sentence that runs past MaxChars with no end in sight is cut at its last
// comma or space, so one long sentence cannot hold speech back for ever.
type Splitter struct {
	MinChars int
	MaxChars int

	buf strings.Builder
}

// Default limits for NewSplitter. 25 characters is about 15 tokens, the
// shortest text Kokoro reads naturally; 200 is a long spoken sentence.
const (
	defaultMinChars = 25
	defaultMaxChars = 200
)

// NewSplitter returns a splitter with the default limits.
func NewSplitter() *Splitter {
	return &Splitter{MinChars: defaultMinChars, MaxChars: defaultMaxChars}
}

// abbreviations end with a period that does not end a sentence. They are
// compared in lower case, without the period.
var abbreviations = map[string]bool{
	"sig": true, "sigg": true, "sig.ra": true, "sig.na": true, "dott": true, "dott.ssa": true,
	"ing": true, "avv": true, "geom": true, "prof": true, "prof.ssa": true, "arch": true,
	"rag": true, "dr": true, "egr": true, "gent": true, "spett": true, "ecc": true,
	"es": true, "tel": true, "cell": true, "n": true, "nr": true, "num": true, "pag": true,
	"art": true, "p": true, "c.a": true, "s.p.a": true, "s.r.l": true, "srl": true,
	"spa": true,
}

// Push adds streamed text and returns the sentences it completed, in order.
func (s *Splitter) Push(text string) []string {
	s.buf.WriteString(text)
	var out []string
	for {
		sentence, ok := s.next()
		if !ok {
			return out
		}
		out = append(out, sentence)
	}
}

// Flush returns what is left once the model has finished, or "" if nothing
// is. It is the last sentence of the reply, so the minimum length does not
// apply.
func (s *Splitter) Flush() string {
	rest := strings.TrimSpace(s.buf.String())
	s.buf.Reset()
	return rest
}

// next cuts the first complete sentence off the buffer.
func (s *Splitter) next() (string, bool) {
	text := s.buf.String()
	start := 0
	for {
		end := s.boundary(text, start)
		if end < 0 {
			break
		}
		sentence := strings.TrimSpace(text[:end])
		if utf8.RuneCountInString(sentence) >= s.MinChars {
			s.keep(text[end:])
			return sentence, true
		}
		start = end // too short: look for the next boundary and merge
	}

	if utf8.RuneCountInString(text) > s.MaxChars {
		cut := lastBreak(text, s.MaxChars)
		s.keep(text[cut:])
		return strings.TrimSpace(text[:cut]), true
	}
	return "", false
}

// keep replaces the buffer with rest.
func (s *Splitter) keep(rest string) {
	s.buf.Reset()
	s.buf.WriteString(rest)
}

// boundary returns the index just past the first sentence end at or after
// from, or -1 if the text has none yet.
func (s *Splitter) boundary(text string, from int) int {
	for i := from; i < len(text); i++ {
		c := text[i]
		switch c {
		case '\n':
			return i + 1
		case '.', '!', '?', ';':
		default:
			continue
		}
		// Swallow a run of closing marks: "?!", "...", a closing quote.
		j := i + 1
		for j < len(text) && strings.IndexByte(".!?\"'»)", text[j]) >= 0 {
			j++
		}
		if j >= len(text) {
			return -1 // the next chunk decides whether whitespace follows
		}
		r, _ := utf8.DecodeRuneInString(text[j:])
		if !unicode.IsSpace(r) {
			i = j - 1
			continue // "3.5", "S.p.A", "www.example": not a sentence end
		}
		if c == '.' && j == i+1 && isAbbreviation(text[:i]) {
			continue
		}
		return j
	}
	return -1
}

// isAbbreviation reports whether the word ending text is a known abbreviation
// or a single capital letter (an initial, as in "M. Rossi").
func isAbbreviation(text string) bool {
	start := strings.LastIndexFunc(text, func(r rune) bool { return unicode.IsSpace(r) || r == '(' })
	word := text[start+1:]
	if word == "" {
		return false
	}
	if r, size := utf8.DecodeRuneInString(word); size == len(word) && unicode.IsUpper(r) {
		return true
	}
	return abbreviations[strings.ToLower(word)]
}

// lastBreak returns where to cut text that is too long: after the last comma
// within limit characters, else after the last space, else at limit.
func lastBreak(text string, limit int) int {
	end := len(text)
	n := 0
	for i := range text { // i walks bytes, n counts characters
		if n == limit {
			end = i
			break
		}
		n++
	}
	if i := strings.LastIndex(text[:end], ","); i > 0 {
		return i + 1
	}
	if i := strings.LastIndexFunc(text[:end], unicode.IsSpace); i > 0 {
		return i + 1
	}
	return end
}
