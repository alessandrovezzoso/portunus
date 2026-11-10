package config

import (
	"errors"
	"io/fs"
	"maps"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseDotEnv(t *testing.T) {
	input := strings.Join([]string{
		"# a comment",
		"",
		"   # an indented comment",
		"PLAIN=value",
		"  SPACED  =  value with spaces  ",
		"export EXPORTED=yes",
		`DOUBLE="  keeps spaces  "`,
		`SINGLE='it''s raw'`,
		"HASH=pa#ss # not a comment",
		`QUOTED_HASH="#start"`,
		"EMPTY=",
		`EMPTY_QUOTES=""`,
		"EQUALS=a=b=c",
		"CRLF=windows\r",
	}, "\n")

	got, err := ParseDotEnv(strings.NewReader(input))
	if err != nil {
		t.Fatalf("ParseDotEnv: %v", err)
	}

	want := map[string]string{
		"PLAIN":        "value",
		"SPACED":       "value with spaces",
		"EXPORTED":     "yes",
		"DOUBLE":       "  keeps spaces  ",
		"SINGLE":       "it''s raw",
		"HASH":         "pa#ss # not a comment",
		"QUOTED_HASH":  "#start",
		"EMPTY":        "",
		"EMPTY_QUOTES": "",
		"EQUALS":       "a=b=c",
		"CRLF":         "windows",
	}
	if !maps.Equal(got, want) {
		t.Errorf("ParseDotEnv:\n got %q\nwant %q", got, want)
	}
}

func TestParseDotEnvErrors(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string // substring of the error
	}{
		{"missing equals", "A=1\nJUSTAKEY\n", "line 2: expected KEY=VALUE"},
		{"empty key", "=value", "line 1: invalid key"},
		{"key with space", "MY KEY=1", "invalid key"},
		{"key starting with digit", "1KEY=1", "invalid key"},
		{"unterminated double quote", `A="open`, `line 1: A: unterminated " quote`},
		{"lone quote", `A='`, "unterminated ' quote"},
		{"duplicate", "A=1\n\nA=2", "line 3: A is already set on line 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseDotEnv(strings.NewReader(tt.input))
			if err == nil {
				t.Fatalf("ParseDotEnv(%q): expected an error", tt.input)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("ParseDotEnv(%q) error = %q, want it to contain %q", tt.input, err, tt.want)
			}
		})
	}
}

func TestReadDotEnvMissingFile(t *testing.T) {
	_, err := ReadDotEnv(filepath.Join(t.TempDir(), "absent.env"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ReadDotEnv on a missing file: got %v, want an error matching fs.ErrNotExist", err)
	}
}
