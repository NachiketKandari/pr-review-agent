package review

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/NachiketKandari/pr-review-agent/chunk"
	"github.com/NachiketKandari/pr-review-agent/diff"
	"github.com/NachiketKandari/pr-review-agent/llm"
)

type fakeLLM struct {
	chatCalls   []llm.ChatRequest
	chatResp    func(call int, req llm.ChatRequest) (string, error)
	streamCalls []llm.ChatRequest
	streamResp  func(call int, req llm.ChatRequest) (string, error)
}

func (f *fakeLLM) Chat(ctx context.Context, req llm.ChatRequest) (string, error) {
	call := len(f.chatCalls)
	f.chatCalls = append(f.chatCalls, req)
	if f.chatResp == nil {
		return "", nil
	}
	return f.chatResp(call, req)
}

func (f *fakeLLM) StreamChat(ctx context.Context, req llm.ChatRequest, onDelta func(string)) error {
	call := len(f.streamCalls)
	f.streamCalls = append(f.streamCalls, req)
	if f.streamResp != nil {
		text, err := f.streamResp(call, req)
		if err != nil {
			return err
		}
		// Emit in two deltas to prove accumulation across deltas.
		mid := len(text) / 2
		onDelta(text[:mid])
		onDelta(text[mid:])
	}
	return nil
}

func smallFile(path string) diff.File {
	return diff.File{
		Path: path,
		Text: "diff --git a/" + path + " b/" + path + "\n--- a/" + path + "\n+++ b/" + path + "\n@@ -1,10 +1,10 @@\n+line one\n+line two\n",
	}
}

// medFile is a per-file diff of ~415 estimated tokens.
func medFile(path string) diff.File {
	var b strings.Builder
	b.WriteString("diff --git a/" + path + " b/" + path + "\n--- a/" + path + "\n+++ b/" + path + "\n@@ -1,80 +1,80 @@\n")
	for i := 0; i < 80; i++ {
		b.WriteString("+        xxxxxxxxxx\n")
	}
	return diff.File{Path: path, Text: b.String()}
}

func testRef() diff.Ref { return diff.Ref{Owner: "octocat", Repo: "hello", Number: 7} }

func TestNewValidationAndDefaults(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Error("expected error without model")
	}
	if _, err := New(Options{Model: "m"}); err == nil {
		t.Error("expected error without client")
	}
	a, err := New(Options{Model: "m", Client: &fakeLLM{}})
	if err != nil {
		t.Fatal(err)
	}
	if a.maxChunk != 8000 || a.maxResponse != 2048 {
		t.Errorf("defaults = chunk %d resp %d, want 8000/2048", a.maxChunk, a.maxResponse)
	}
	if a.temperature != 0.2 {
		t.Errorf("temperature = %v, want 0.2", a.temperature)
	}
	if !strings.Contains(a.systemPrompt, "code review") || !strings.Contains(a.chunkPrompt, "{{diff}}") ||
		!strings.Contains(a.mergePrompt, "{{findings}}") {
		t.Error("default prompts not applied")
	}
}

func TestPromptBuilding(t *testing.T) {
	a, err := New(Options{
		Model:        "deepseek-v4-flash",
		Client:       &fakeLLM{},
		SystemPrompt: "CUSTOM SYSTEM",
		ChunkPrompt:  "chunk {{index}}/{{total}} files={{files}}: {{diff}}",
		MergePrompt:  "merge these: {{findings}}",
	})
	if err != nil {
		t.Fatal(err)
	}
	c := chunk.Chunk{Index: 2, Total: 3, Files: []string{"a.go", "b.go"}, Text: "THE DIFF BODY"}
	msgs := a.buildMessages(c, "ENRICHED")
	if len(msgs) != 2 || msgs[0].Role != "system" || msgs[1].Role != "user" {
		t.Fatalf("messages = %+v", msgs)
	}
	if msgs[0].Content != "CUSTOM SYSTEM" {
		t.Errorf("system = %q", msgs[0].Content)
	}
	user, _ := msgs[1].Content.(string)
	if !strings.Contains(user, "chunk 2/3 files=a.go, b.go") || !strings.Contains(user, "THE DIFF BODY") {
		t.Errorf("chunk prompt = %q", user)
	}
	if !strings.Contains(user, "ENRICHED") {
		t.Errorf("enrichment not prepended: %q", user)
	}

	mmsgs := a.buildMergeMessages([]string{"F1", "F2"})
	mu, _ := mmsgs[1].Content.(string)
	if !strings.Contains(mu, "F1") || !strings.Contains(mu, "F2") {
		t.Errorf("merge prompt = %q", mu)
	}
}

func TestReviewSingleChunkStreamsFinalMerge(t *testing.T) {
	fake := &fakeLLM{
		chatResp: func(call int, req llm.ChatRequest) (string, error) {
			if call != 0 {
				t.Errorf("expected only one Chat call, got #%d", call)
			}
			if req.MaxTokens != 2048 {
				t.Errorf("MaxTokens = %d, want 2048", req.MaxTokens)
			}
			if req.Temperature == nil || *req.Temperature != 0.2 {
				t.Errorf("temperature = %v, want 0.2", req.Temperature)
			}
			return "## chunk findings", nil
		},
		streamResp: func(call int, req llm.ChatRequest) (string, error) {
			return "## merged review\nfinal output", nil
		},
	}
	a, err := New(Options{Model: "m", Client: fake})
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	want := "## merged review\nfinal output"
	got, err := a.Review(context.Background(), testRef(), []diff.File{smallFile("a.go")}, "", &out)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("Review text = %q, want %q", got, want)
	}
	if out.String() != want {
		t.Errorf("streamed output = %q, want %q", out.String(), want)
	}
	if len(fake.chatCalls) != 1 || len(fake.streamCalls) != 1 {
		t.Errorf("calls = chat %d stream %d, want 1/1", len(fake.chatCalls), len(fake.streamCalls))
	}

	// The final merge must receive the chunk finding.
	streamMsg, _ := fake.streamCalls[0].Messages[1].Content.(string)
	if !strings.Contains(streamMsg, "## chunk findings") {
		t.Errorf("merge input missing chunk findings: %q", streamMsg)
	}
}

// Two chunks are produced; their findings fit the budget so the merge is
// single-level and streamed.
func TestReviewTwoChunksSingleFinalMerge(t *testing.T) {
	files := []diff.File{medFile("a.go"), medFile("b.go"), medFile("c.go"), medFile("d.go")}
	fake := &fakeLLM{
		chatResp: func(call int, req llm.ChatRequest) (string, error) {
			return "## findings chunk", nil
		},
		streamResp: func(call int, req llm.ChatRequest) (string, error) {
			return "FINAL", nil
		},
	}
	a, err := New(Options{Model: "m", Client: fake, MaxChunkTokens: 1000})
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	got, err := a.Review(context.Background(), testRef(), files, "", &out)
	if err != nil {
		t.Fatal(err)
	}
	if got != "FINAL" {
		t.Errorf("Review text = %q", got)
	}
	if len(fake.chatCalls) != 2 {
		t.Errorf("Chat calls = %d, want 2 chunk reviews", len(fake.chatCalls))
	}
	if len(fake.streamCalls) != 1 {
		t.Errorf("StreamChat calls = %d, want 1", len(fake.streamCalls))
	}
	for _, req := range fake.chatCalls {
		user, _ := req.Messages[1].Content.(string)
		if strings.Contains(user, "{{diff}}") || strings.Contains(user, "{{files}}") {
			t.Errorf("chunk prompt placeholders unresolved: %q", user)
		}
	}
}

// reduce-level test: findings far over budget are merged in groups by
// non-streamed calls before a final streamed merge.
func TestReduceGroupsOverflow(t *testing.T) {
	fake := &fakeLLM{
		chatResp: func(call int, req llm.ChatRequest) (string, error) {
			// Groups of ~1800 tokens each; merged down to 400 tokens.
			return strings.Repeat("M", 1600), nil
		},
		streamResp: func(call int, req llm.ChatRequest) (string, error) {
			return "FINAL", nil
		},
	}
	a, err := New(Options{Model: "m", Client: fake, MaxChunkTokens: 2000})
	if err != nil {
		t.Fatal(err)
	}
	parts := make([]string, 4)
	for i := range parts {
		parts[i] = strings.Repeat("F", 2400) // ~600 tokens
	}
	var out bytes.Buffer
	if _, err := a.reduce(context.Background(), testRef(), parts, &out, 0); err != nil {
		t.Fatal(err)
	}
	// 4 parts (2400 tokens) > budget 2000: pack into [3, 1], two
	// non-streamed group merges, then a streamed final merge.
	if len(fake.chatCalls) != 2 {
		t.Errorf("Chat calls = %d, want 2 group merges", len(fake.chatCalls))
	}
	if len(fake.streamCalls) != 1 {
		t.Errorf("StreamChat calls = %d, want 1", len(fake.streamCalls))
	}
}

// reduce-level test: even after a merge round the results overflow, so a
// second non-streamed round runs before the final stream.
func TestReduceRecursiveRounds(t *testing.T) {
	fake := &fakeLLM{
		chatResp: func(call int, req llm.ChatRequest) (string, error) {
			switch {
			case call < 4: // first round: groups of 3 -> keep large
				return strings.Repeat("M", 2800), nil // ~700 tokens
			default: // second round: pair merges -> small
				return strings.Repeat("N", 1200), nil // ~300 tokens
			}
		},
		streamResp: func(call int, req llm.ChatRequest) (string, error) {
			return "FINAL", nil
		},
	}
	a, err := New(Options{Model: "m", Client: fake, MaxChunkTokens: 2000})
	if err != nil {
		t.Fatal(err)
	}
	parts := make([]string, 10)
	for i := range parts {
		parts[i] = strings.Repeat("F", 2400) // ~600 tokens each; 6000 total
	}
	var out bytes.Buffer
	if _, err := a.reduce(context.Background(), testRef(), parts, &out, 0); err != nil {
		t.Fatal(err)
	}
	if len(fake.chatCalls) != 6 {
		t.Errorf("Chat calls = %d, want 6 (4 round-1 merges + 2 round-2 merges)", len(fake.chatCalls))
	}
	if len(fake.streamCalls) != 1 {
		t.Errorf("StreamChat calls = %d, want 1 final stream", len(fake.streamCalls))
	}
}

// A lone finding bigger than the whole merge budget still terminates via a
// direct streamed merge.
func TestReduceSingleOversizedFinding(t *testing.T) {
	fake := &fakeLLM{
		streamResp: func(call int, req llm.ChatRequest) (string, error) {
			return "FINAL", nil
		},
	}
	a, err := New(Options{Model: "m", Client: fake, MaxChunkTokens: 2000})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	big := strings.Repeat("F", 40000) // ~10000 tokens, > budget
	if _, err := a.reduce(context.Background(), testRef(), []string{big}, &out, 0); err != nil {
		t.Fatal(err)
	}
	if len(fake.chatCalls) != 0 {
		t.Errorf("Chat calls = %d, want 0", len(fake.chatCalls))
	}
	if len(fake.streamCalls) != 1 {
		t.Errorf("StreamChat calls = %d, want 1", len(fake.streamCalls))
	}
}

func TestBackgroundContextReachesSystemPrompts(t *testing.T) {
	fake := &fakeLLM{
		chatResp:   func(call int, req llm.ChatRequest) (string, error) { return "C", nil },
		streamResp: func(call int, req llm.ChatRequest) (string, error) { return "F", nil },
	}
	a, err := New(Options{Model: "m", Client: fake})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	bg := "Goal: fix ABC-123 retry behaviour. See Jira ABC-123."
	if _, err := a.Review(context.Background(), testRef(), []diff.File{smallFile("a.go")}, bg, &out); err != nil {
		t.Fatal(err)
	}
	system, _ := fake.chatCalls[0].Messages[0].Content.(string)
	if !strings.Contains(system, bg) || !strings.Contains(system, "Pull request intent") {
		t.Errorf("chunk system prompt missing intent context: %q", system)
	}
	mergeSystem, _ := fake.streamCalls[0].Messages[0].Content.(string)
	if !strings.Contains(mergeSystem, bg) {
		t.Errorf("merge system prompt missing intent context: %q", mergeSystem)
	}
}

func TestReviewNoChunks(t *testing.T) {
	fake := &fakeLLM{}
	a, err := New(Options{Model: "m", Client: fake})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	got, err := a.Review(context.Background(), testRef(), nil, "", &out)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" || out.Len() != 0 {
		t.Errorf("empty review produced %q (buffer %d bytes)", got, out.Len())
	}
	if len(fake.chatCalls) != 0 || len(fake.streamCalls) != 0 {
		t.Error("no model calls expected for an empty diff")
	}
}

func TestEnricherSeamCalledOnce(t *testing.T) {
	var enrichCalls int
	fake := &fakeLLM{
		chatResp:   func(call int, req llm.ChatRequest) (string, error) { return "C", nil },
		streamResp: func(call int, req llm.ChatRequest) (string, error) { return "F", nil },
	}
	enricher := enrichFunc(func(ctx context.Context, files []diff.File) (string, error) {
		enrichCalls++
		if len(files) != 1 {
			t.Errorf("Enrich got %d files, want 1", len(files))
		}
		return "SYMBOL TABLE CONTEXT", nil
	})
	a, err := New(Options{Model: "m", Client: fake, Enricher: enricher})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := a.Review(context.Background(), testRef(), []diff.File{smallFile("a.go")}, "", &out); err != nil {
		t.Fatal(err)
	}
	if enrichCalls != 1 {
		t.Errorf("Enrich calls = %d, want 1", enrichCalls)
	}
	user, _ := fake.chatCalls[0].Messages[1].Content.(string)
	if !strings.Contains(user, "SYMBOL TABLE CONTEXT") {
		t.Errorf("chunk prompt missing enrichment: %q", user)
	}
}

type enrichFunc func(ctx context.Context, files []diff.File) (string, error)

func (f enrichFunc) Enrich(ctx context.Context, files []diff.File) (string, error) {
	return f(ctx, files)
}
