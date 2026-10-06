package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NachiketKandari/pr-review-agent/diff"
	"github.com/NachiketKandari/pr-review-agent/explain"
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

// The page and its spec are written together, and the spec must be loadable —
// that pairing is what makes re-rendering free.
func TestWriteExplanationSavesPageAndSpec(t *testing.T) {
	dir := t.TempDir()
	page := filepath.Join(dir, "page.html")
	f := cfgFlags{outputPath: page}

	spec := explain.Spec{
		Title:    "Retries now back off with jitter",
		Slug:     "retries-now-back-off-with-jitter",
		Sections: []explain.Section{{ID: "intuition", Heading: "Intuition", HTML: "<p>x</p>"}},
		// Two options with exactly one correct: a question that survives
		// Normalize, so it must survive the round trip too.
		Quiz: []explain.Question{{
			Question: "Why?",
			Options: []explain.Option{
				{Text: "because", Correct: true, Feedback: "yes"},
				{Text: "no reason", Feedback: "the backoff is computed per attempt"},
			},
		}},
	}
	got, err := writeExplanation(f, diff.Ref{Owner: "o", Repo: "r", Number: 1}, spec, "<html>page</html>")
	if err != nil {
		t.Fatal(err)
	}
	if got != page {
		t.Errorf("path = %q, want the -output path %q", got, page)
	}
	pageBytes, err := os.ReadFile(page)
	if err != nil {
		t.Fatal(err)
	}
	if string(pageBytes) != "<html>page</html>" {
		t.Errorf("page contents = %q", pageBytes)
	}

	specPath := filepath.Join(dir, "page.spec.json")
	specBytes, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("the spec must sit beside the page: %v", err)
	}
	var back explain.Spec
	if err := json.Unmarshal(specBytes, &back); err != nil {
		t.Fatalf("saved spec does not load: %v", err)
	}
	if back.Title != spec.Title || len(back.Sections) != 1 || len(back.Quiz) != 1 {
		t.Errorf("saved spec lost content: %+v", back)
	}
	// And it must re-render.
	if _, err := explain.Render(back); err != nil {
		t.Errorf("saved spec does not render: %v", err)
	}
}

// Without -output the filename is date-prefixed from the slug, matching the
// skill's render.py convention.
func TestWriteExplanationDerivesFilename(t *testing.T) {
	dir := t.TempDir()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(wd) })

	got, err := writeExplanation(cfgFlags{}, diff.Ref{}, explain.Spec{
		Title:    "Retries back off",
		Sections: []explain.Section{{ID: "background", HTML: "<p>x</p>"}},
	}, "<html></html>")
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Base(got)
	if !strings.HasPrefix(base, timeYearMonthDay()) || !strings.Contains(base, "retries-back-off") {
		t.Errorf("filename = %q, want a date prefix and the slug", base)
	}
	if !strings.HasSuffix(base, ".html") {
		t.Errorf("filename = %q, want an .html suffix", base)
	}
	if _, err := os.Stat(got); err != nil {
		t.Errorf("page not written: %v", err)
	}
}

func timeYearMonthDay() string { return time.Now().Format("2006-01-02") }
