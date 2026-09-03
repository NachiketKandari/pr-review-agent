// Package review orchestrates map-reduce code reviews of pull-request
// diffs.
//
// Map: each diff chunk is reviewed by a non-streamed, response-capped model
// call. Reduce: chunk findings are merged — recursively when they overflow
// the context budget — until a single final merge can be streamed to the
// terminal.
//
// The model is accessed through the minimal [LLM] interface so tests can
// substitute a fake; *llm.Client implements it directly.
package review

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/NachiketKandari/pr-review-agent/chunk"
	"github.com/NachiketKandari/pr-review-agent/diff"
	"github.com/NachiketKandari/pr-review-agent/llm"
	"github.com/NachiketKandari/pr-review-agent/xlog"
)

// Defaults applied when Options leave fields zero.
const (
	defaultSystemPrompt = "You are a senior software engineer performing a code review of a GitHub pull request. " +
		"Focus on correctness, security, concurrency, performance, and maintainability. " +
		"Be specific: reference files and functions when possible, and quote code. " +
		"Ignore style nitpicks unless they affect correctness. " +
		"Output concise, actionable findings as Markdown. If the change is fine, say so briefly."
	defaultChunkPrompt = "Review the following chunk {{index}} of {{total}} from a pull request.\n" +
		"Files in this chunk: {{files}}\n\n{{diff}}\n\n" +
		"List concrete findings with file names. Keep the response under {{responseTokens}} tokens."
	defaultMergePrompt = "Below are the per-chunk reviews of one pull request, separated by ---.\n" +
		"Synthesize them into one deduplicated review. Merge duplicates, order findings by severity, " +
		"and keep every substantive finding. Format as Markdown.\n\n{{findings}}"
)

// LLM is the minimal model surface review needs. *llm.Client satisfies it.
type LLM interface {
	Chat(ctx context.Context, req llm.ChatRequest) (string, error)
	StreamChat(ctx context.Context, req llm.ChatRequest, onDelta func(string)) error
}

// Enricher is the Phase 2 seam for deeper per-file analysis (Go AST symbol
// tables, call graphs, function context). Nothing wires it up yet; code
// that does only has to satisfy this interface, with no orchestrator
// changes.
type Enricher interface {
	// Enrich returns extra context about the changed files, appended to
	// each chunk prompt.
	Enrich(ctx context.Context, files []diff.File) (string, error)
}

// Options configures a review Agent.
type Options struct {
	Model    string
	Client   LLM
	Enricher Enricher // optional; Phase 2 seam, nil by default

	SystemPrompt      string  // "" = default
	ChunkPrompt       string  // "" = default
	MergePrompt       string  // "" = default
	MaxChunkTokens    int     // 0 = 8000; chunk and merge context budget
	MaxResponseTokens int     // 0 = 2048; cap per model response
	Temperature       float64 // 0 = 0.2
}

// Agent runs one review pass over a fetched PR diff.
type Agent struct {
	model        string
	systemPrompt string
	chunkPrompt  string
	mergePrompt  string
	maxChunk     int
	maxResponse  int
	temperature  float64
	client       LLM
	enricher     Enricher
}

// New validates Options and applies defaults.
func New(opts Options) (*Agent, error) {
	if opts.Model == "" {
		return nil, fmt.Errorf("review: model is required")
	}
	if opts.Client == nil {
		return nil, fmt.Errorf("review: client is required")
	}

	a := &Agent{
		model:        opts.Model,
		systemPrompt: opts.SystemPrompt,
		chunkPrompt:  opts.ChunkPrompt,
		mergePrompt:  opts.MergePrompt,
		maxChunk:     opts.MaxChunkTokens,
		maxResponse:  opts.MaxResponseTokens,
		temperature:  opts.Temperature,
		client:       opts.Client,
		enricher:     opts.Enricher,
	}
	if a.systemPrompt == "" {
		a.systemPrompt = defaultSystemPrompt
	}
	if a.chunkPrompt == "" {
		a.chunkPrompt = defaultChunkPrompt
	} else if !strings.Contains(a.chunkPrompt, "{{diff}}") {
		xlog.Warn("review.chunkPrompt is set but has no {{diff}} placeholder; the diff text will not be sent")
	}
	if a.mergePrompt == "" {
		a.mergePrompt = defaultMergePrompt
	} else if !strings.Contains(a.mergePrompt, "{{findings}}") {
		xlog.Warn("review.mergePrompt is set but has no {{findings}} placeholder; chunk findings will not be sent")
	}
	if a.maxChunk <= 0 {
		a.maxChunk = 8000
	}
	if a.maxResponse <= 0 {
		a.maxResponse = 2048
	}
	if a.temperature <= 0 {
		a.temperature = 0.2
	}
	return a, nil
}

// Review runs the full map-reduce review of files, streaming the final
// merged review to out, and returns the full merged text (identical to what
// was streamed). Logs every stage: chunking, per-chunk review, intermediate
// merges, and the final merge.
func (a *Agent) Review(ctx context.Context, ref diff.Ref, files []diff.File, out io.Writer) (string, error) {
	xlog.Info("review start",
		"pr", ref.String(), "model", a.model,
		"files", len(files),
		"max_chunk_tokens", a.maxChunk, "max_response_tokens", a.maxResponse,
		"temperature", a.temperature)

	chunks, err := chunk.Build(files, a.maxChunk)
	if err != nil {
		return "", fmt.Errorf("chunk diff: %w", err)
	}
	if len(chunks) == 0 {
		xlog.Info("nothing to review; no parseable diff content", "pr", ref.String())
		return "", nil
	}

	enrichment := ""
	if a.enricher != nil {
		start := time.Now()
		enrichment, err = a.enricher.Enrich(ctx, files)
		if err != nil {
			return "", fmt.Errorf("enrich files: %w", err)
		}
		xlog.Info("enrichment complete", "pr", ref.String(),
			"duration_ms", time.Since(start).Milliseconds(), "chars", len(enrichment))
	}

	// Map: one capped, non-streamed review call per chunk.
	findings := make([]string, 0, len(chunks))
	for _, c := range chunks {
		start := time.Now()
		xlog.Info("chunk review start",
			"pr", ref.String(), "chunk", c.Index, "total", c.Total,
			"files", strings.Join(c.Files, ","), "tokens", c.Tokens)

		outText, err := a.chat(ctx, a.buildMessages(c, enrichment), a.model)
		if err != nil {
			return "", fmt.Errorf("chunk %d/%d review failed: %w", c.Index, c.Total, err)
		}
		if strings.TrimSpace(outText) == "" {
			xlog.Warn("chunk review returned empty output", "pr", ref.String(), "chunk", c.Index)
			continue
		}

		xlog.Info("chunk review complete",
			"pr", ref.String(), "chunk", c.Index, "total", c.Total,
			"duration_ms", time.Since(start).Milliseconds(),
			"response_chars", len(outText), "response_tokens_est", chunk.EstimateTokens(outText))
		findings = append(findings, strings.TrimSpace(outText))
	}

	if len(findings) == 0 {
		xlog.Warn("all chunk reviews came back empty; nothing to merge", "pr", ref.String())
		return "", nil
	}

	// Reduce: recursively merge until the combined findings fit the
	// context budget; the final merge streams to out.
	start := time.Now()
	finalText, err := a.reduce(ctx, ref, findings, out, 0)
	if err != nil {
		return "", fmt.Errorf("merge findings: %w", err)
	}
	xlog.Info("review complete",
		"pr", ref.String(),
		"duration_ms", time.Since(start).Milliseconds(),
		"output_chars", len(finalText), "output_tokens_est", chunk.EstimateTokens(finalText))
	return finalText, nil
}

// reduce consolidates findings into a single review. Non-final levels run
// non-streamed Chat merges (logging each), and the last level streams to
// out. depth bounds pathological cases where a single response keeps
// exceeding the budget.
func (a *Agent) reduce(ctx context.Context, ref diff.Ref, parts []string, out io.Writer, depth int) (string, error) {
	if len(parts) == 0 {
		return "", nil
	}

	if total := sumTokens(parts); len(parts) == 1 || total <= a.maxChunk || depth >= 8 {
		return a.streamMerge(ctx, ref, parts, out)
	}

	// Findings overflow the merge context: pack them into budget-sized
	// groups and merge each non-streamed, then recurse on the results.
	groups := pack(parts, a.maxChunk)
	merged := make([]string, 0, len(groups))
	for _, g := range groups {
		if len(g) == 1 && chunk.EstimateTokens(g[0]) > a.maxChunk {
			// A lone oversized finding (longer than one full merge
			// context) passes through; a later round may still combine
			// it once the rest of the findings shrink.
			merged = append(merged, g[0])
			continue
		}
		start := time.Now()
		xlog.Info("merge call start",
			"pr", ref.String(), "parts", len(g),
			"tokens", sumTokens(g), "streamed", false)
		res, err := a.mergeChat(ctx, g)
		if err != nil {
			return "", err
		}
		xlog.Info("merge call complete",
			"pr", ref.String(), "parts", len(g),
			"duration_ms", time.Since(start).Milliseconds(),
			"response_chars", len(res), "response_tokens_est", chunk.EstimateTokens(res))
		merged = append(merged, strings.TrimSpace(res))
	}
	return a.reduce(ctx, ref, merged, out, depth+1)
}

func (a *Agent) streamMerge(ctx context.Context, ref diff.Ref, parts []string, out io.Writer) (string, error) {
	messages := a.buildMergeMessages(parts)
	start := time.Now()
	xlog.Info("merge call start",
		"pr", ref.String(), "parts", len(parts),
		"tokens", sumTokens(parts), "streamed", true)

	var b strings.Builder
	write := func(delta string) {
		b.WriteString(delta)
		fmt.Fprint(out, delta)
	}
	if err := a.client.StreamChat(ctx, llm.ChatRequest{
		Model:       a.model,
		Messages:    messages,
		MaxTokens:   a.maxResponse,
		Temperature: floatPtr(a.temperature),
	}, write); err != nil {
		return "", fmt.Errorf("stream merge: %w", err)
	}

	text := b.String()
	xlog.Info("merge call complete",
		"pr", ref.String(), "parts", len(parts),
		"duration_ms", time.Since(start).Milliseconds(), "streamed", true,
		"output_chars", len(text), "output_tokens_est", chunk.EstimateTokens(text))
	return text, nil
}

// chat performs one non-streamed, response-capped model call.
func (a *Agent) chat(ctx context.Context, messages []llm.Message, model string) (string, error) {
	start := time.Now()
	out, err := a.client.Chat(ctx, llm.ChatRequest{
		Model:       model,
		Messages:    messages,
		MaxTokens:   a.maxResponse,
		Temperature: floatPtr(a.temperature),
	})
	if err != nil {
		return "", err
	}
	xlog.Debug("model chat complete", "model", model,
		"duration_ms", time.Since(start).Milliseconds(), "chars", len(out))
	return out, nil
}

// mergeChat performs a non-streamed merge of one budget-sized group.
func (a *Agent) mergeChat(ctx context.Context, group []string) (string, error) {
	return a.chat(ctx, a.buildMergeMessages(group), a.model)
}

func (a *Agent) buildMessages(c chunk.Chunk, enrichment string) []llm.Message {
	replacer := strings.NewReplacer(
		"{{index}}", fmt.Sprint(c.Index),
		"{{total}}", fmt.Sprint(c.Total),
		"{{files}}", strings.Join(c.Files, ", "),
		"{{diff}}", c.Text,
		"{{responseTokens}}", fmt.Sprint(a.maxResponse),
	)
	user := replacer.Replace(a.chunkPrompt)
	if enrichment != "" {
		user = "Extra context about the changed code:\n" + enrichment + "\n\n" + user
	}
	return []llm.Message{
		{Role: "system", Content: a.systemPrompt},
		{Role: "user", Content: user},
	}
}

func (a *Agent) buildMergeMessages(parts []string) []llm.Message {
	return []llm.Message{
		{Role: "system", Content: a.systemPrompt},
		{Role: "user", Content: strings.NewReplacer(
			"{{findings}}", strings.Join(parts, "\n\n---\n\n"),
		).Replace(a.mergePrompt)},
	}
}

// pack greedily groups items so each group's total estimated tokens stays
// within budget (a lone item may exceed it).
func pack(items []string, budget int) [][]string {
	var groups [][]string
	var cur []string
	total := 0
	for _, it := range items {
		t := chunk.EstimateTokens(it)
		if len(cur) > 0 && total+t > budget {
			groups = append(groups, cur)
			cur = nil
			total = 0
		}
		cur = append(cur, it)
		total += t
	}
	if len(cur) > 0 {
		groups = append(groups, cur)
	}
	return groups
}

func sumTokens(parts []string) int {
	total := 0
	for _, p := range parts {
		total += chunk.EstimateTokens(p)
	}
	return total
}

func floatPtr(f float64) *float64 { return &f }
