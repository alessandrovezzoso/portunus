package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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

	if _, err := readGreeting(filepath.Join(dir, "missing.txt")); err == nil || !strings.Contains(err.Error(), "example.greeting.txt") {
		t.Errorf("missing file: error = %v, want one pointing at the example", err)
	}
}
