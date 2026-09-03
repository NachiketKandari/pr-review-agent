package chunk

import (
	"strings"
	"testing"

	"github.com/NachiketKandari/pr-review-agent/diff"
)

func TestEstimateTokens(t *testing.T) {
	if got := EstimateTokens("abcd"); got != 1 {
		t.Errorf("EstimateTokens(\"abcd\") = %d, want 1", got)
	}
	if got := EstimateTokens(""); got != 0 {
		t.Errorf("EstimateTokens(\"\") = %d, want 0", got)
	}
}

// file builds a fake per-file diff section. Each line is 18 characters plus
// a newline (~19 chars, ~5 tokens at len/4).
func file(path string, nLines int, more ...string) diff.File {
	var b strings.Builder
	b.WriteString("diff --git a/" + path + " b/" + path + "\n")
	b.WriteString("index 0000000..1111111 100644\n")
	b.WriteString("--- a/" + path + "\n")
	b.WriteString("+++ b/" + path + "\n")
	for _, m := range more {
		b.WriteString(m)
	}
	for i := 0; i < nLines; i++ {
		b.WriteString("+        xxxxxxxxxx\n")
	}
	return diff.File{Path: path, Text: b.String()}
}

func TestBuildGroupsAtFileBoundaries(t *testing.T) {
	// ~500 tokens per file, budget 1000: files a+b share chunk 1, c is chunk 2.
	a := file("a.go", 90)
	b := file("b.go", 90)
	c := file("c.go", 90)

	chunks, err := Build([]diff.File{a, b, c}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(chunks))
	}
	if chunks[0].Index != 1 || chunks[0].Total != 2 {
		t.Errorf("chunk 0 metadata = %d/%d", chunks[0].Index, chunks[0].Total)
	}
	if chunks[1].Index != 2 || chunks[1].Total != 2 {
		t.Errorf("chunk 1 metadata = %d/%d", chunks[1].Index, chunks[1].Total)
	}
	if len(chunks[0].Files) != 2 || chunks[0].Files[0] != "a.go" || chunks[0].Files[1] != "b.go" {
		t.Errorf("chunk 0 files = %v", chunks[0].Files)
	}
	if len(chunks[1].Files) != 1 || chunks[1].Files[0] != "c.go" {
		t.Errorf("chunk 1 files = %v", chunks[1].Files)
	}
	for _, c := range chunks {
		if c.Tokens > 1000 {
			t.Errorf("chunk %d tokens %d exceeds budget", c.Index, c.Tokens)
		}
		if !strings.Contains(c.Text, "diff --git") {
			t.Errorf("chunk %d lost its diff headers", c.Index)
		}
	}
}

func TestBuildOneChunkWhenEverythingFits(t *testing.T) {
	chunks, err := Build([]diff.File{file("a.go", 5), file("b.go", 5)}, 8000)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 {
		t.Fatalf("expected a single chunk, got %d", len(chunks))
	}
	if chunks[0].Index != 1 || chunks[0].Total != 1 {
		t.Errorf("metadata = %d/%d", chunks[0].Index, chunks[0].Total)
	}
	if len(chunks[0].Files) != 2 {
		t.Errorf("files = %v", chunks[0].Files)
	}
}

func TestBuildSplitsOversizedFileAtHunks(t *testing.T) {
	// One file, three hunks of ~500 tokens each, budget 1000. The greedy
	// packer must produce several chunks that each fit the budget while
	// keeping every @@ hunk header intact in exactly one chunk.
	header := "diff --git a/big.go b/big.go\nindex 0000000..1111111 100644\n--- a/big.go\n+++ b/big.go\n"
	var b strings.Builder
	b.WriteString(header)
	for _, marker := range []string{"@@ -1,90 +1,90 @@", "@@ -200,90 +200,90 @@", "@@ -400,90 +400,90 @@"} {
		b.WriteString(marker + "\n")
		for i := 0; i < 90; i++ {
			b.WriteString("+        xxxxxxxxxx\n")
		}
	}
	f := diff.File{Path: "big.go", Text: b.String()}

	chunks, err := Build([]diff.File{f}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 2 {
		t.Fatalf("expected the oversized file to span multiple chunks, got %d", len(chunks))
	}
	for _, c := range chunks {
		if c.Tokens > 1000 {
			t.Errorf("chunk %d has %d tokens, want <= 1000", c.Index, c.Tokens)
		}
		if len(c.Files) != 1 || c.Files[0] != "big.go" {
			t.Errorf("chunk %d files = %v", c.Index, c.Files)
		}
	}
	found := map[string]bool{}
	for _, c := range chunks {
		for _, marker := range []string{"@@ -1,90", "@@ -200,90", "@@ -400,90"} {
			if strings.Contains(c.Text, marker) {
				found[marker] = true
			}
		}
	}
	for _, marker := range []string{"@@ -1,90", "@@ -200,90", "@@ -400,90"} {
		if !found[marker] {
			t.Errorf("hunk %q missing from all chunks", marker)
		}
	}
}

func TestBuildHardLineSplitWithoutHunks(t *testing.T) {
	// A pathological "diff" with no @@ markers, far exceeding the budget.
	var b strings.Builder
	for i := 0; i < 2000; i++ {
		b.WriteString("+        xxxxxxxxxx\n")
	}
	f := diff.File{Path: "huge.txt", Text: b.String()}

	chunks, err := Build([]diff.File{f}, 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	for _, c := range chunks {
		if EstimateTokens(c.Text) > 500 {
			t.Errorf("chunk %d has %d tokens, want <= 500", c.Index, EstimateTokens(c.Text))
		}
	}
}

func TestBuildSingleOversizedHunkSplitAtLines(t *testing.T) {
	var b strings.Builder
	b.WriteString("diff --git a/wall.go b/wall.go\n+++ b/wall.go\n@@ -0,0 +1,2000 @@\n")
	for i := 0; i < 2000; i++ {
		b.WriteString("+        xxxxxxxxxx\n")
	}
	f := diff.File{Path: "wall.go", Text: b.String()}

	chunks, err := Build([]diff.File{f}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 2 {
		t.Fatalf("expected a single oversized hunk to be split, got %d chunk(s)", len(chunks))
	}
	for _, c := range chunks {
		if c.Tokens > 1000 {
			t.Errorf("chunk %d has %d tokens, want <= 1000", c.Index, c.Tokens)
		}
	}
}

func TestBuildEdgeCases(t *testing.T) {
	if chunks, err := Build(nil, 1000); err != nil || len(chunks) != 0 {
		t.Errorf("empty input: chunks=%d err=%v, want 0/nil", len(chunks), err)
	}
	if _, err := Build(nil, 0); err == nil {
		t.Error("zero budget should error")
	}
	if _, err := Build(nil, -5); err == nil {
		t.Error("negative budget should error")
	}

	bin := diff.File{Path: "x.png", Binary: true, Text: "Binary files differ"}
	empty := diff.File{Path: "y.go", Text: "   \n  "}
	chunks, err := Build([]diff.File{bin, empty}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 0 {
		t.Errorf("binary and empty files produced %d chunks, want 0", len(chunks))
	}
}
