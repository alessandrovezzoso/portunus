package speech

import (
	"reflect"
	"strings"
	"testing"
)

func TestNumber(t *testing.T) {
	for n, want := range map[int]string{
		0: "zero", 1: "uno", 3: "tre", 8: "otto", 10: "dieci", 13: "tredici", 16: "sedici",
		17: "diciassette", 18: "diciotto", 19: "diciannove", 20: "venti", 21: "ventuno",
		23: "ventitré", 28: "ventotto", 30: "trenta", 33: "trentatré", 38: "trentotto",
		80: "ottanta", 81: "ottantuno", 88: "ottantotto", 99: "novantanove",
		100: "cento", 101: "centouno", 103: "centotré", 108: "centootto", 111: "centoundici",
		180: "centottanta", 188: "centottantotto", 200: "duecento", 333: "trecentotrentatré",
		999: "novecentonovantanove", 1000: "mille", 1001: "milleuno", 1003: "milletré",
		1100: "millecento", 1500: "millecinquecento", 2000: "duemila", 2026: "duemilaventisei",
		21000: "ventunomila", 23000: "ventitremila", 100000: "centomila",
		123456:  "centoventitremilaquattrocentocinquantasei",
		1000000: "un milione", 2000000: "due milioni", 1250000: "un milione duecentocinquantamila",
	} {
		if got := Number(n); got != want {
			t.Errorf("Number(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestNormalizeItalian(t *testing.T) {
	for in, want := range map[string]string{
		// Phone numbers, digit by digit, in the caller's own groups.
		"Il numero è 02 4567 8901.":  "Il numero è zero due, quattro cinque sei sette, otto nove zero uno.",
		"Mi chiami al 333 123 4567.": "Mi chiami al tre tre tre, uno due tre, quattro cinque sei sette.",
		"+39 333 123 4567":           "più tre nove, tre tre tre, uno due tre, quattro cinque sei sette",
		"3331234567":                 "tre tre tre, uno due tre, quattro cinque sei sette",
		"0245678901":                 "zero due quattro, cinque sei sette, otto nove zero uno",
		// Times, dates and years.
		"alle 14:30":    "alle quattordici e trenta",
		"alle 9:00":     "alle nove",
		"verso l'1:15":  "verso l'una e quindici",
		"il 12/11/2026": "il dodici novembre duemilaventisei",
		"dal 1/3/2027":  "dal primo marzo duemilaventisette",
		"nel 2026":      "nel duemilaventisei",
		// Amounts.
		"1.500 euro":       "millecinquecento euro",
		"€ 50":             "cinquanta euro",
		"€ 1.500":          "millecinquecento euro",
		"20 €":             "venti euro",
		"il 20%":           "il venti per cento",
		"3,5 metri":        "tre virgola cinque metri",
		"100.000 abitanti": "centomila abitanti",
		// E-mail addresses, the way they are dictated.
		"scriva a mario.rossi@example.com": "scriva a mario punto rossi chiocciola example punto com",
		// Left alone: words with digits, and text with no numbers.
		"il formato A4": "il formato A4",
		// Kokoro's pause tags are instructions, and keep their digits.
		"Sono 2 minuti. [pause:0.4s] Ciao": "Sono due minuti. [pause:0.4s] Ciao",
		"Buongiorno, come posso aiutarla?": "Buongiorno, come posso aiutarla?",
	} {
		if got := NormalizeItalian(in); got != want {
			t.Errorf("NormalizeItalian(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

func TestStripPauses(t *testing.T) {
	for in, want := range map[string]string{
		"Buongiorno, [pause:0.1s] sono Portunus. [pause:0.4s] Come posso aiutarla?": "Buongiorno, sono Portunus. Come posso aiutarla?",
		"[pause:0.3s]Salve.[pause:1s]":    "Salve.",
		"Nessuna pausa qui.":              "Nessuna pausa qui.",
		"Le parentesi [vere] restano.":    "Le parentesi [vere] restano.",
		"Alle 9:30 [pause:0.2s] apriamo.": "Alle 9:30 apriamo.",
	} {
		if got := StripPauses(in); got != want {
			t.Errorf("StripPauses(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

func TestSanitize(t *testing.T) {
	for in, want := range map[string]string{
		"**Certo!** Ecco cosa posso fare:": "Certo! Ecco cosa posso fare:",
		"- primo punto":                    "primo punto",
		"2. secondo punto":                 "secondo punto",
		"# Riepilogo":                      "Riepilogo",
		"Trova tutto su [il sito](https://example.com).": "Trova tutto su il sito.",
		"Visiti https://example.com per i dettagli.":     "Visiti per i dettagli.",
		"Grazie mille 😊🙏🏻":                               "Grazie mille",
		"Ci sono 20° oggi":                               "Ci sono 20° oggi",
		"  spazi   in   più  ":                           "spazi in più",
		"`codice` e *corsivo*":                           "codice e corsivo",
	} {
		if got := Sanitize(in); got != want {
			t.Errorf("Sanitize(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

// split feeds text to a splitter in small chunks, the way it streams from the
// model, and returns every sentence including the flushed rest.
func split(text string, chunk int) []string {
	s := NewSplitter()
	var out []string
	for len(text) > 0 {
		n := min(chunk, len(text))
		out = append(out, s.Push(text[:n])...)
		text = text[n:]
	}
	if rest := s.Flush(); rest != "" {
		out = append(out, rest)
	}
	return out
}

func TestSplitterSentences(t *testing.T) {
	text := "Certo, prendo nota del suo messaggio. Mi può dire il suo nome e cognome? " +
		"La richiameremo al più presto!"
	want := []string{
		"Certo, prendo nota del suo messaggio.",
		"Mi può dire il suo nome e cognome?",
		"La richiameremo al più presto!",
	}
	for _, chunk := range []int{1, 3, 7, 1000} {
		if got := split(text, chunk); !reflect.DeepEqual(got, want) {
			t.Errorf("chunks of %d:\n got %q\nwant %q", chunk, got, want)
		}
	}
}

func TestSplitterMergesShortSentences(t *testing.T) {
	// "Certo." alone is too short for Kokoro: it travels with the next one.
	got := split("Certo. Prendo nota del suo messaggio. Grazie.", 4)
	want := []string{"Certo. Prendo nota del suo messaggio.", "Grazie."}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSplitterKeepsAbbreviationsDecimalsAndTimes(t *testing.T) {
	text := "Il Sig. Rossi e la Dott.ssa Bianchi arrivano alle 14:30 con 3.5 chili di carte, ecc. e altro. Poi basta."
	got := split(text, 5)
	want := []string{
		"Il Sig. Rossi e la Dott.ssa Bianchi arrivano alle 14:30 con 3.5 chili di carte, ecc. e altro.",
		"Poi basta.",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}
	if got := split("Parli con M. Rossi della ditta, per favore.", 3); len(got) != 1 {
		t.Errorf("an initial must not end the sentence: %q", got)
	}
}

func TestSplitterCutsLongSentences(t *testing.T) {
	long := strings.Repeat("parola ", 40) + "fine, " + strings.Repeat("altra ", 20)
	got := split(long, 10)
	if len(got) < 2 {
		t.Fatalf("a %d-character sentence came out whole", len(long))
	}
	for _, s := range got {
		if n := len([]rune(s)); n > defaultMaxChars {
			t.Errorf("piece of %d characters exceeds %d", n, defaultMaxChars)
		}
	}
	if strings.Join(got, " ") != strings.TrimSpace(strings.Join(strings.Fields(long), " ")) {
		t.Error("cutting lost or changed text")
	}
}

func TestSplitterLineBreaksEndSentences(t *testing.T) {
	got := split("Ecco i passaggi da seguire\n- primo passaggio importante\n- secondo passaggio importante", 6)
	if len(got) != 3 {
		t.Errorf("got %q, want three pieces", got)
	}
}
