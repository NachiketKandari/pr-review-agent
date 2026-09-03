// Package chunk splits PR diffs into review-sized pieces.
//
// Splitting is greedy at file boundaries, then at hunk (@@) boundaries for
// oversized files, then at line boundaries as a last resort. Every grouping
// and split decision is logged so a chunked review can be audited.
package chunk

import (
	"fmt"
	"strings"

	"github.com/NachiketKandari/pr-review-agent/diff"
	"github.com/NachiketKandari/pr-review-agent/xlog"
)

// Chunk is one self-contained diff piece for a single model review call.
type Chunk struct {
	Index  int
	Total  int
	Files  []string // file paths in order of appearance
	Text   string
	Tokens int // estimated token count
}

// EstimateTokens is a len/4 heuristic: ~4 characters per token.
func EstimateTokens(text string) int {
	return len(text) / 4
}

// Build splits the parsed files into chunks of at most maxChunkTokens
// estimated tokens. Binary files are skipped. A chunk never contains a
// partial hunk unless one hunk alone exceeds the budget.
func Build(files []diff.File, maxChunkTokens int) ([]Chunk, error) {
	if maxChunkTokens <= 0 {
		return nil, fmt.Errorf("maxChunkTokens must be > 0, got %d", maxChunkTokens)
	}

	pieces := make([]piece, 0)
	for _, f := range files {
		if f.Binary {
			continue
		}
		if strings.TrimSpace(f.Text) == "" {
			xlog.Debug("skipping empty diff section", "file", f.Path)
			continue
		}
		pieces = append(pieces, splitFile(f, maxChunkTokens)...)
	}

	// Greedy grouping: close a chunk when the next piece would exceed the
	// budget, so every chunk except an oversized lone piece fits.
	var groups [][]piece
	var cur []piece
	curTokens := 0
	for _, p := range pieces {
		if len(cur) > 0 && curTokens+p.tokens > maxChunkTokens {
			groups = append(groups, cur)
			cur = nil
			curTokens = 0
		}
		cur = append(cur, p)
		curTokens += p.tokens
	}
	if len(cur) > 0 {
		groups = append(groups, cur)
	}

	chunks := make([]Chunk, 0, len(groups))
	for _, g := range groups {
		chunks = append(chunks, assemble(g))
	}
	for i := range chunks {
		chunks[i].Index = i + 1
		chunks[i].Total = len(chunks)
		xlog.Info("chunk created",
			"chunk", chunks[i].Index,
			"total", chunks[i].Total,
			"files", strings.Join(chunks[i].Files, ","),
			"tokens", chunks[i].Tokens,
		)
	}
	return chunks, nil
}

type piece struct {
	path   string
	text   string
	tokens int
}

func assemble(pieces []piece) Chunk {
	c := Chunk{Text: joinPieces(pieces)}
	c.Tokens = EstimateTokens(c.Text)
	seen := map[string]bool{}
	for _, p := range pieces {
		if !seen[p.path] {
			seen[p.path] = true
			c.Files = append(c.Files, p.path)
		}
	}
	return c
}

func joinPieces(pieces []piece) string {
	var b strings.Builder
	for i, p := range pieces {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(p.text)
	}
	return b.String()
}

// splitFile returns a single piece when the file fits the budget; otherwise
// it splits at hunk boundaries, with line-level splitting as a last resort.
func splitFile(f diff.File, max int) []piece {
	tokens := EstimateTokens(f.Text)
	if tokens <= max {
		return []piece{{path: f.Path, text: f.Text, tokens: tokens}}
	}
	xlog.Info("file exceeds chunk budget; splitting",
		"file", f.Path, "estimated_tokens", tokens, "max_chunk_tokens", max)

	if hunks := hunkRanges(f.Text); len(hunks) > 0 {
		var pieces []piece
		for _, h := range hunks {
			text := strings.Join(h.lines, "\n")
			if t := EstimateTokens(text); t > max {
				pieces = append(pieces, splitLines(f.Path, text, max)...)
			} else {
				pieces = append(pieces, piece{path: f.Path, text: text, tokens: t})
			}
		}
		xlog.Info("split oversized file at hunk boundaries",
			"file", f.Path, "pieces", len(pieces))
		return pieces
	}

	// No usable hunk boundaries, or a lone hunk still exceeds the budget:
	// hard line split.
	xlog.Info("splitting oversized file at line boundaries (last resort)",
		"file", f.Path)
	return splitLines(f.Path, f.Text, max)
}

// hunkRange is a set of contiguous lines belonging to one hunk, starting at
// its "@@" header. For the first hunk the pre-hunk diff headers (diff --git,
// index, ---/+++) are kept so the reviewer still sees the file path.
type hunkRange struct {
	lines []string
}

// hunkRanges groups the lines of a diff into hunks. Returns nil when the
// text has fewer than two hunks (nothing to split at) or no hunks at all.
func hunkRanges(text string) []hunkRange {
	lines := strings.Split(text, "\n")
	var starts []int
	for i, ln := range lines {
		if strings.HasPrefix(ln, "@@") {
			starts = append(starts, i)
		}
	}
	if len(starts) < 2 {
		return nil
	}

	var out []hunkRange
	// Keep the diff headers (lines before the first @@) attached to the
	// first hunk.
	start := 0
	for i := 1; i < len(starts); i++ {
		out = append(out, hunkRange{lines: lines[start:starts[i]]})
		start = starts[i]
	}
	out = append(out, hunkRange{lines: lines[start:]})
	return out
}

// splitLines hard-splits text into line-aligned pieces of at most max
// tokens. Each piece except the last is allowed to slightly exceed the
// budget only when a single line alone is oversized.
func splitLines(path, text string, max int) []piece {
	var pieces []piece
	start := 0
	acc := 0
	flush := func(end int, cur int) {
		sub := strings.Join(strings.Split(text, "\n")[start:end], "\n")
		pieces = append(pieces, piece{path: path, text: sub, tokens: cur})
		start = end
		acc = 0
	}
	lines := strings.Split(text, "\n")
	for i, ln := range lines {
		t := len(ln) + 1 // account for the newline
		if acc > 0 && acc+t > max*4 {
			flush(i, acc/4)
		}
		acc += t
	}
	if start < len(lines) {
		sub := strings.Join(lines[start:], "\n")
		pieces = append(pieces, piece{path: path, text: sub, tokens: EstimateTokens(sub)})
	}
	return pieces
}
