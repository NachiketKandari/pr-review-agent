package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadPromptFile(t *testing.T) {
	dir := t.TempDir()

	trimmed := filepath.Join(dir, "prompt.txt")
	if err := os.WriteFile(trimmed, []byte("  Review this diff.\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(empty, []byte("   \n"), 0o644); err != nil {
		t.Fatal(err)
	}

	text, err := readPromptFile(trimmed)
	if err != nil {
		t.Fatalf("readPromptFile(%q): %v", trimmed, err)
	}
	if text != "Review this diff." {
		t.Errorf("got %q, want %q", text, "Review this diff.")
	}

	if _, err := readPromptFile(empty); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("empty file: got err %v, want an \"empty\" error", err)
	}

	if _, err := readPromptFile(filepath.Join(dir, "missing.txt")); err == nil {
		t.Error("missing file: got nil error, want an error")
	}
}
