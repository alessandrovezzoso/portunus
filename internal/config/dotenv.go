package config

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// ReadDotEnv reads and parses the .env file at path. The returned error wraps
// the os error, so callers can tell a missing file apart with errors.Is and
// fs.ErrNotExist.
func ReadDotEnv(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	// Read-only: a failed Close cannot lose data.
	defer func() { _ = f.Close() }()

	values, err := ParseDotEnv(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return values, nil
}

// ParseDotEnv parses the contents of a .env file into a map.
//
// The format is deliberately small, because a config file that needs a manual
// is a config file that gets misread:
//
//   - KEY=VALUE, one per line, with whitespace around key and value trimmed;
//   - blank lines, and comment lines whose first non-blank character is '#';
//   - an optional "export " prefix, so the file can also be sourced by a shell;
//   - a value may be wrapped in matching single or double quotes, which are
//     removed and keep the inner text exactly as written.
//
// There is no interpolation, no escape processing and no inline comment: a '#'
// after the '=' is part of the value, because passwords can contain one.
//
// A key that appears twice is an error rather than "last one wins": in a file
// that is edited by hand, a duplicate is almost always a mistake, and silently
// picking one copy is exactly the kind of surprise this loader exists to avoid.
func ParseDotEnv(r io.Reader) (map[string]string, error) {
	values := make(map[string]string)
	firstSeen := make(map[string]int)

	scanner := bufio.NewScanner(r)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(strings.TrimSuffix(scanner.Text(), "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")

		key, value, found := strings.Cut(line, "=")
		if !found {
			return nil, fmt.Errorf("line %d: expected KEY=VALUE", lineNo)
		}
		key = strings.TrimSpace(key)
		if !validKey(key) {
			return nil, fmt.Errorf("line %d: invalid key %q", lineNo, key)
		}
		value, err := unquote(strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("line %d: %s: %w", lineNo, key, err)
		}
		if prev, dup := firstSeen[key]; dup {
			return nil, fmt.Errorf("line %d: %s is already set on line %d", lineNo, key, prev)
		}

		firstSeen[key] = lineNo
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return values, nil
}

// validKey accepts the same names a POSIX shell accepts for variables, so a
// .env file never holds a key that the shell environment could not.
func validKey(key string) bool {
	if key == "" {
		return false
	}
	for i, c := range key {
		switch {
		case c == '_', c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

func unquote(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	quote := value[0]
	if quote != '"' && quote != '\'' {
		return value, nil
	}
	if len(value) < 2 || value[len(value)-1] != quote {
		return "", fmt.Errorf("unterminated %c quote", quote)
	}
	return value[1 : len(value)-1], nil
}
