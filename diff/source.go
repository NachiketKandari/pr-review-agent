package diff

import (
	"context"
	"fmt"
	"strings"

	"github.com/NachiketKandari/pr-review-agent/xlog"
)

// SourceContext returns the pre-image of the changed files — the code as it
// looked at rev, before the change — read from the local clone at dir.
//
// The explain-diff recipe tells its agent to "broadly explore surrounding
// code" for the background section. A pipeline with no tool loop has to
// choose that context mechanically instead, and it cannot ask: so it takes
// the changed files themselves, from before the change, and spends a fixed
// budget on them. That is the most valuable context available without an
// index: the file the change edits is where its callers, types and helpers
// live.
//
// The budget is shared and spent in order: at most maxFiles files are read
// (the first maxFiles of the diff, which is the order a reviewer would read
// them in) and each is trimmed to an equal share of maxTokens*4 characters,
// cut on a line boundary. A file that does not exist at rev — a file added
// by this change — is skipped, since its pre-image is nothing. Every failure
// is non-fatal: git missing, no clone, an unreadable file. The result is
// context, not a requirement.
func SourceContext(ctx context.Context, dir, rev string, files []File, maxFiles, maxTokens int) string {
	if dir == "" || rev == "" {
		return ""
	}
	if maxFiles <= 0 || maxTokens <= 0 {
		return ""
	}

	limit := maxFiles
	if len(files) < limit {
		limit = len(files)
	}
	if limit == 0 {
		return ""
	}
	// A budget too small to give every file a usable share is refused
	// outright: returning an empty string is honest, whereas handing the
	// model a few characters per file invites it to write about a stub.
	if maxTokens*4/limit < minSourceFileChars {
		xlog.Debug("source context budget too small to be useful",
			"max_tokens", maxTokens, "files", limit, "min_per_file_chars", minSourceFileChars)
		return ""
	}
	share := maxTokens * 4 / limit // characters, so ~maxTokens tokens overall

	var b strings.Builder
	used := 0
	for i, f := range files {
		if i >= maxFiles {
			break
		}
		out, err := gitRun(ctx, dir, "show", rev+":"+f.Path)
		if err != nil || strings.TrimSpace(out) == "" {
			// Almost always a file this change adds: there is no "before".
			xlog.Debug("no pre-image for file at rev", "file", f.Path, "rev", rev, "error", err)
			continue
		}
		text, dropped := trimToChars(out, share)
		truncated := dropped > 0 || len(strings.TrimRight(text, "\n")) < len(strings.TrimRight(out, "\n"))
		b.WriteString(fmt.Sprintf("\n### %s (as it was at %s, before this change)\n", f.Path, rev))
		b.WriteString(text)
		if truncated {
			// Say so either way: a model told a file is complete will write
			// about the tail it never saw, and one told a file is partial
			// stops inventing its contents.
			if dropped > 0 {
				fmt.Fprintf(&b, "\n… %d more line%s of this file were left out to stay inside the context budget\n",
					dropped, pluralLines(dropped))
			} else {
				b.WriteString("\n… this file was cut off mid-line to stay inside the context budget\n")
			}
		}
		used++
	}

	if used == 0 {
		return ""
	}
	return strings.TrimSpace(b.String())
}

// trimToChars cuts text to at most n characters on a line boundary and
// reports how many lines were left out.
func trimToChars(text string, n int) (string, int) {
	if len(text) <= n {
		return strings.TrimRight(text, "\n"), 0
	}
	cut := strings.LastIndexByte(text[:n], '\n')
	if cut <= 0 {
		cut = n
	}
	head := text[:cut]
	total := strings.Count(text, "\n")
	kept := strings.Count(head, "\n")
	return head, total - kept
}

// minSourceFileChars keeps a per-file share usable when the overall budget is
// small: a few hundred characters is enough to show a file's shape.
const minSourceFileChars = 400

func pluralLines(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
