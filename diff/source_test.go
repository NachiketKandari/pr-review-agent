package diff

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestSourceContextReadsThePreImage(t *testing.T) {
	stubGit(t, func(args []string) (string, error) {
		joined := strings.Join(args, " ")
		switch {
		case joined == "-C . show abc123:internal/auth.go":
			return "package auth\n\nfunc Issue() string {\n\treturn \"token\"\n}\n", nil
		case strings.Contains(joined, "show"):
			return "other file contents\n", nil
		}
		return "", errors.New("unexpected git call: " + joined)
	})

	files := []File{{Path: "internal/auth.go"}}
	got := SourceContext(context.Background(), ".", "abc123", files, 6, 3000)

	if !strings.Contains(got, "internal/auth.go") {
		t.Errorf("context missing the file name: %q", got)
	}
	if !strings.Contains(got, "func Issue()") {
		t.Errorf("context missing the file body: %q", got)
	}
	if !strings.Contains(got, "abc123") {
		t.Errorf("context should name the rev it was read at: %q", got)
	}
}

// A file added by the change has no pre-image; git fails, and that must be a
// skip rather than an error.
func TestSourceContextSkipsFilesMissingAtRev(t *testing.T) {
	stubGit(t, func(args []string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "new.go") {
			return "", errors.New("path 'new.go' does not exist in 'abc123'")
		}
		return "package old\n", nil
	})

	files := []File{{Path: "new.go"}, {Path: "old.go"}}
	got := SourceContext(context.Background(), ".", "abc123", files, 6, 3000)
	if !strings.Contains(got, "old.go") {
		t.Errorf("expected the existing file to be read: %q", got)
	}
	if strings.Contains(got, "new.go") {
		t.Errorf("a file with no pre-image should be skipped: %q", got)
	}
}

func TestSourceContextCapsFileCountAndSize(t *testing.T) {
	long := strings.Repeat("x", 50000)
	stubGit(t, func(args []string) (string, error) { return long, nil })

	var files []File
	for i := 0; i < 10; i++ {
		files = append(files, File{Path: fmt.Sprintf("f%02d.go", i)})
	}
	// 2 files, 400 tokens: 800 chars shared, so ~400 per file.
	got := SourceContext(context.Background(), ".", "rev", files, 2, 400)

	if strings.Count(got, "### ") != 2 {
		t.Errorf("read %d files, want the cap of 2:\n%s", strings.Count(got, "### "), got)
	}
	if !strings.Contains(got, "f00.go") || strings.Contains(got, "f05.go") {
		t.Error("expected the first files in diff order")
	}
	if len(got) > 400*4*2 {
		t.Errorf("context is %d chars, want it near the 1600-char budget", len(got))
	}
	// A truncated file must announce it, or the model will write about the
	// part it never saw. (A single-line file is cut mid-line, so accept
	// either of the two honest phrasings.)
	if !strings.Contains(got, "context budget") {
		t.Errorf("a truncated file should say so:\n%s", got)
	}
}

// One long line cannot be cut on a line boundary, so it is cut mid-line — but
// the model must still be told the file is incomplete.
func TestSourceContextFlagsMidLineTruncation(t *testing.T) {
	stubGit(t, func(args []string) (string, error) {
		return strings.Repeat("x", 4000), nil // no newlines at all
	})
	got := SourceContext(context.Background(), ".", "rev", []File{{Path: "min.js"}}, 1, 100)
	if !strings.Contains(got, "min.js") {
		t.Fatalf("expected the file to be read: %q", got)
	}
	if !strings.Contains(got, "cut off mid-line") {
		t.Errorf("a mid-line cut must be flagged: %q", got)
	}
}

// Without a clone or a rev there is nothing to read, and that is not an error.
func TestSourceContextWithoutCloneOrRev(t *testing.T) {
	// Any git call here would be a bug: there is nothing to read.
	stubGit(t, func(args []string) (string, error) {
		t.Errorf("unexpected git call: %v", args)
		return "", errors.New("should not be called")
	})
	files := []File{{Path: "a.go"}}
	if got := SourceContext(context.Background(), "", "rev", files, 6, 3000); got != "" {
		t.Errorf("no dir: got %q, want empty", got)
	}
	if got := SourceContext(context.Background(), ".", "", files, 6, 3000); got != "" {
		t.Errorf("no rev: got %q, want empty", got)
	}
	if got := SourceContext(context.Background(), ".", "rev", nil, 6, 3000); got != "" {
		t.Errorf("no files: got %q, want empty", got)
	}
	// Nonsensical budgets must not panic or yield a zero-width share.
	if got := SourceContext(context.Background(), ".", "rev", files, 0, 0); got != "" {
		t.Errorf("zero budget: got %q, want empty", got)
	}
}

// A budget too small to hold anything must be refused, not honoured with a
// zero-width slice that would drop every file silently.
func TestSourceContextRefusesAnUnusableBudget(t *testing.T) {
	stubGit(t, func(args []string) (string, error) {
		t.Errorf("unexpected git call for an unusable budget: %v", args)
		return "", nil
	})
	if got := SourceContext(context.Background(), ".", "rev", []File{{Path: "a.go"}}, 100, 1); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// git failing entirely is a skip, not a crash: the explainer falls back to
// the diff alone.
func TestSourceContextTolerantOfGitFailure(t *testing.T) {
	stubGit(t, func(args []string) (string, error) { return "", errors.New("git is unhappy") })
	files := []File{{Path: "a.go"}}
	if got := SourceContext(context.Background(), ".", "rev", files, 6, 3000); got != "" {
		t.Errorf("got %q, want empty when every read fails", got)
	}
}

func TestSourceContextTruncatesOnLineBoundaries(t *testing.T) {
	body := "line one\nline two\nline three\nline four\nline five\n"
	stubGit(t, func(args []string) (string, error) { return body, nil })

	// 20 chars holds "line one\nline two\nlin": the cut falls back to the
	// last newline, so the head ends on a whole line and "line three" is
	// reported as left out.
	got, dropped := trimToChars(body, 20)
	if !strings.HasSuffix(got, "line two") {
		t.Errorf("truncation cut mid-line: %q", got)
	}
	// Five lines in, one kept, four left out (the trailing newline makes the
	// empty tail count as a line).
	if dropped != 4 {
		t.Errorf("dropped = %d lines, want 4", dropped)
	}
	// A file that is one single long line still has to be cut, and the caller
	// still has to learn that it was.
	mince, drop1 := trimToChars(strings.Repeat("x", 500), 100)
	if len(mince) > 100 || drop1 != 0 {
		t.Errorf("single-line file: got (%d chars, %d dropped)", len(mince), drop1)
	}
	// Under budget: nothing dropped.
	if got, dropped := trimToChars(body, 1000); got != strings.TrimRight(body, "\n") || dropped != 0 {
		t.Errorf("short input: got (%q, %d), want it untouched", got, dropped)
	}
}
