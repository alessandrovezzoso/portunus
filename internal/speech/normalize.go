package speech

import (
	"regexp"
	"strconv"
	"strings"
)

// Kokoro hands Italian text to eSpeak NG for pronunciation, and eSpeak reads
// digits as whole numbers: "02 4567 8901" becomes "quattromilacinquecento…".
// NormalizeItalian rewrites numbers the way a person says them before the
// text reaches the TTS. The prompt asks the model to write numbers in words
// already; this catches what slips through.
//
// The order of the rules matters: an e-mail or a date contains digits that the
// later, more general rules would otherwise read the wrong way.
var (
	emailRe    = regexp.MustCompile(`[\w.+-]+@[\w-]+(?:\.[\w-]+)+`)
	dateRe     = regexp.MustCompile(`\b([0-3]?\d)/([01]?\d)/(\d{4})\b`)
	timeRe     = regexp.MustCompile(`\b([01]?\d|2[0-3]):([0-5]\d)\b`)
	phoneRe    = regexp.MustCompile(`(?:\+\d{1,3}[ .-]?)?\d(?:[ .-]?\d){5,}`)
	percentRe  = regexp.MustCompile(`(\d+(?:,\d+)?)\s?%`)
	euroPreRe  = regexp.MustCompile(`€\s?(\d[\d.,]*)`)
	euroPostRe = regexp.MustCompile(`(\d[\d.,]*)\s?€`)
	thousandRe = regexp.MustCompile(`\b[1-9]\d{0,2}(?:\.\d{3})+\b`)
	decimalRe  = regexp.MustCompile(`\b(\d+),(\d+)\b`)
	integerRe  = regexp.MustCompile(`\b\d+\b`)
)

var months = [...]string{"", "gennaio", "febbraio", "marzo", "aprile", "maggio", "giugno",
	"luglio", "agosto", "settembre", "ottobre", "novembre", "dicembre"}

// pauseTagRe matches Kokoro's inline pause tags, such as "[pause:0.4s]". They
// are instructions to the TTS, not text: their digits must stay digits.
var pauseTagRe = regexp.MustCompile(`\[pause:[^\]]*\]`)

// NormalizeItalian spells out the numbers in text, leaving pause tags alone.
func NormalizeItalian(text string) string {
	var out strings.Builder
	last := 0
	for _, loc := range pauseTagRe.FindAllStringIndex(text, -1) {
		out.WriteString(normalize(text[last:loc[0]]))
		out.WriteString(text[loc[0]:loc[1]])
		last = loc[1]
	}
	out.WriteString(normalize(text[last:]))
	return out.String()
}

func normalize(text string) string {
	text = emailRe.ReplaceAllStringFunc(text, spellEmail)
	text = dateRe.ReplaceAllStringFunc(text, func(m string) string {
		p := dateRe.FindStringSubmatch(m)
		day, month, year := atoi(p[1]), atoi(p[2]), atoi(p[3])
		if day < 1 || day > 31 || month < 1 || month > 12 {
			return m
		}
		d := Number(day)
		if day == 1 {
			d = "primo" // "il primo marzo", never "l'uno marzo"
		}
		return d + " " + months[month] + " " + Number(year)
	})
	text = timeRe.ReplaceAllStringFunc(text, func(m string) string {
		p := timeRe.FindStringSubmatch(m)
		return spellTime(atoi(p[1]), atoi(p[2]))
	})
	// "€ 50" becomes "50 euro" first, so the amount is read by the rules
	// below like any other number.
	text = euroPreRe.ReplaceAllString(text, "$1 euro")
	text = euroPostRe.ReplaceAllString(text, "$1 euro")
	// Amounts with a thousands separator ("1.500", "100.000") come before
	// phone numbers, which they would otherwise look like.
	text = thousandRe.ReplaceAllStringFunc(text, func(m string) string {
		return spellInteger(strings.ReplaceAll(m, ".", ""))
	})
	text = phoneRe.ReplaceAllStringFunc(text, spellPhoneOrNumber)
	text = percentRe.ReplaceAllString(text, "$1 per cento")
	text = decimalRe.ReplaceAllStringFunc(text, func(m string) string {
		p := decimalRe.FindStringSubmatch(m)
		return spellInteger(p[1]) + " virgola " + spellInteger(p[2])
	})
	return integerRe.ReplaceAllStringFunc(text, spellInteger)
}

// spellEmail reads an address the way it is dictated on the phone.
func spellEmail(m string) string {
	r := strings.NewReplacer("@", " chiocciola ", ".", " punto ", "-", " trattino ", "_", " trattino basso ")
	return r.Replace(m)
}

// spellTime reads 14:30 as "quattordici e trenta" and 9:00 as "nove".
func spellTime(h, m int) string {
	hour := Number(h)
	if h == 1 {
		hour = "una" // "l'una", the hour is feminine
	}
	if m == 0 {
		return hour
	}
	return hour + " e " + Number(m)
}

// spellPhoneOrNumber handles a run of six or more digits. A phone number, or
// anything with separators, a leading zero or a plus, is read digit by digit
// in its own groups. A plain run of six digits is an amount and is read as a
// number; seven or more plain digits can only be a code, and are read digit
// by digit in groups of three.
func spellPhoneOrNumber(m string) string {
	plain := strings.Trim(m, "0123456789") == "" && !strings.HasPrefix(m, "0")
	if plain && len(m) <= 6 {
		return spellInteger(m)
	}

	var groups []string
	if plain || !strings.ContainsAny(m, " .-") {
		digits := strings.TrimPrefix(m, "+")
		for len(digits) > 4 {
			groups = append(groups, digits[:3])
			digits = digits[3:]
		}
		groups = append(groups, digits)
		if strings.HasPrefix(m, "+") {
			groups[0] = "+" + groups[0]
		}
	} else {
		groups = strings.FieldsFunc(m, func(r rune) bool { return r == ' ' || r == '.' || r == '-' })
	}

	spoken := make([]string, 0, len(groups))
	for _, g := range groups {
		var words []string
		for _, c := range g {
			if c == '+' {
				words = append(words, "più")
				continue
			}
			words = append(words, Number(int(c-'0')))
		}
		spoken = append(spoken, strings.Join(words, " "))
	}
	// Commas make Kokoro pause between groups, as a person would.
	return strings.Join(spoken, ", ")
}

// spellInteger spells a string of digits, or reads it digit by digit when it
// is too long to be a number anyone says aloud.
func spellInteger(digits string) string {
	if len(digits) > 9 {
		return spellPhoneOrNumber(digits)
	}
	return Number(atoi(digits))
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

var units = [...]string{"zero", "uno", "due", "tre", "quattro", "cinque", "sei", "sette", "otto",
	"nove", "dieci", "undici", "dodici", "tredici", "quattordici", "quindici", "sedici",
	"diciassette", "diciotto", "diciannove"}

var tens = [...]string{"", "", "venti", "trenta", "quaranta", "cinquanta", "sessanta",
	"settanta", "ottanta", "novanta"}

// Number spells n in Italian words, as one word the way Italian writes
// compound numbers: 2026 is "duemilaventisei". It covers up to 999 999 999.
func Number(n int) string {
	if n < 0 {
		return "meno " + Number(-n)
	}
	if n == 0 {
		return "zero"
	}

	var parts []string
	if m := n / 1_000_000; m > 0 {
		if m == 1 {
			parts = append(parts, "un milione")
		} else {
			parts = append(parts, below1000(m)+" milioni")
		}
	}
	var rest strings.Builder
	switch t := (n / 1000) % 1000; {
	case t == 1:
		rest.WriteString("mille")
	case t > 1:
		rest.WriteString(below1000(t) + "mila")
	}
	if r := n % 1000; r > 0 {
		rest.WriteString(below1000(r))
	}
	if rest.Len() > 0 {
		parts = append(parts, rest.String())
	}
	w := strings.Join(parts, " ")

	// A compound that ends in "tre" takes the accent: ventitré, centotré.
	if n > 3 && n%10 == 3 && strings.HasSuffix(w, "tre") {
		w = strings.TrimSuffix(w, "tre") + "tré"
	}
	return w
}

func below1000(n int) string {
	h, r := n/100, n%100
	var s string
	switch {
	case h == 1:
		s = "cento"
	case h > 1:
		s = units[h] + "cento"
	}
	if r == 0 {
		return s
	}
	if h > 0 && r >= 80 && r < 90 {
		s = strings.TrimSuffix(s, "o") // centottanta, not centoottanta
	}
	return s + below100(r)
}

func below100(n int) string {
	if n < 20 {
		return units[n]
	}
	t, u := tens[n/10], n%10
	if u == 0 {
		return t
	}
	if u == 1 || u == 8 {
		t = t[:len(t)-1] // ventuno, ventotto: the vowels meet and one drops
	}
	return t + units[u]
}
