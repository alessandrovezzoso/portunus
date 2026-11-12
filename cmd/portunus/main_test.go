package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alessandrovezzoso/portunus/internal/call"
)

func TestReadGreeting(t *testing.T) {
	dir := t.TempDir()

	ok := filepath.Join(dir, "greeting.txt")
	if err := os.WriteFile(ok, []byte("\n  Buongiorno, risponde l'assistente automatico.  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readGreeting(ok)
	if err != nil || got != "Buongiorno, risponde l'assistente automatico." {
		t.Errorf("readGreeting = %q, %v; want the trimmed text", got, err)
	}

	empty := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(empty, []byte(" \n\t"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readGreeting(empty); err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Errorf("empty file: error = %v, want one saying it is empty", err)
	}

	missing := filepath.Join(dir, "absent", "greeting.txt")
	if _, err := readGreeting(missing); err == nil || !strings.Contains(err.Error(), filepath.Join(dir, "absent", "example.greeting.txt")) {
		t.Errorf("missing file: error = %v, want one pointing at the example next to it", err)
	}
}

func TestReadHolding(t *testing.T) {
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	got, err := readHolding(filepath.Join(dir, "holding.txt"), log)
	if err != nil || len(got) != len(call.DefaultHoldingPhrases) {
		t.Errorf("no file: %q, %v; want the defaults", got, err)
	}

	file := filepath.Join(dir, "mine.txt")
	_ = os.WriteFile(file, []byte("# one per line\nCerto, un momento.\n\n  Bene, un attimo.  \n"), 0o600)
	got, err = readHolding(file, log)
	if err != nil || len(got) != 2 || got[1] != "Bene, un attimo." {
		t.Errorf("file: %q, %v; want the two phrases, trimmed, without the comment", got, err)
	}

	empty := filepath.Join(dir, "empty.txt")
	_ = os.WriteFile(empty, []byte("# nothing\n"), 0o600)
	if _, err := readHolding(empty, log); err == nil {
		t.Error("a file with no phrases must be an error, not a silent fallback")
	}
}
