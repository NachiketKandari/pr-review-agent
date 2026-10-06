package explain

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/NachiketKandari/pr-review-agent/chunk"
	"github.com/NachiketKandari/pr-review-agent/diff"
	"github.com/NachiketKandari/pr-review-agent/llm"
)

type fakeLLM struct {
	chatCalls []llm.ChatRequest
	chatResp  func(call int, req llm.ChatRequest) (string, error)
}

func (f *fakeLLM) Chat(ctx context.Context, req llm.ChatRequest) (string, error) {
	call := len(f.chatCalls)
	f.chatCalls = append(f.chatCalls, req)
	if f.chatResp == nil {
		return "", nil
	}
	return f.chatResp(call, req)
}

func smallFile(path string) diff.File {
	return diff.File{
		Path:      path,
		Additions: 2,
		Deletions: 1,
		Text:      "diff --git a/" + path + " b/" + path + "\n--- a/" + path + "\n+++ b/" + path + "\n@@ -1,10 +1,10 @@\n+line one\n+line two\n",
	}
}

// chunkJSON is a well-formed chunk reply.
const chunkJSON = `{"background":"<p>How the parser works today.</p>",` +
	`"intuition":"<p>The essence, with a toy trace.</p>",` +
	`"code":"<p>The walkthrough.</p><pre><code>x := 1</code></pre>",` +
	`"callouts":["<strong>token</strong> — a unit of work"]}`

const quizJSON = `{"questions":[{"question":"What does the new code return?","options":[` +
	`{"text":"A token","correct":true,"feedback":"Right, it builds a token."},` +
	`{"text":"A token and its index","correct":false,"feedback":"The index is not returned."}]}]}`

const titleReply = "Retries now back off exponentially with jitter"

// userText is the first user message: the instruction being sent.
func userText(req llm.ChatRequest) string {
	if len(req.Messages) < 2 {
		return ""
	}
	s, _ := req.Messages[1].Content.(string)
	return s
}

// allText is every message joined, for detecting the format-retry, which is
// appended as an extra user turn after the original instruction.
func allText(req llm.ChatRequest) string {
	var b strings.Builder
	for _, m := range req.Messages {
		if s, ok := m.Content.(string); ok {
			b.WriteString(s)
			b.WriteString("\n")
		}
	}
	return b.String()
}

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
	// 16K tuning: a ~7K chunk plus a ~3K JSON reply leaves room for the
	// system prompt, the intent and the pre-image context.
	if a.maxChunk != 7000 || a.maxResponse != 3000 || a.quizTokens != 2000 {
		t.Errorf("defaults = chunk %d resp %d quiz %d, want 7000/3000/2000",
			a.maxChunk, a.maxResponse, a.quizTokens)
	}
	if a.temperature != 0.4 {
		t.Errorf("temperature = %v, want 0.4", a.temperature)
	}
	for name, p := range map[string]string{
		"chunk": a.chunkPrompt, "merge": a.mergePrompt,
		"quiz": a.quizPrompt, "title": a.titlePrompt,
	} {
		if strings.TrimSpace(p) == "" {
			t.Errorf("default %s prompt missing", name)
		}
	}
	if !strings.Contains(a.chunkPrompt, "{{diff}}") || !strings.Contains(a.chunkPrompt, "{{context}}") {
		t.Error("chunk prompt must carry the diff and the source-context placeholders")
	}
	if !strings.Contains(a.mergePrompt, "{{fragments}}") || !strings.Contains(a.quizPrompt, "{{page}}") {
		t.Error("merge/quiz prompts must carry their placeholders")
	}
}

// A one-chunk change should cost exactly one chunk call, one title call and
// one quiz call, and produce all three sections plus the quiz.
func TestExplainSingleChunk(t *testing.T) {
	fake := &fakeLLM{chatResp: func(call int, req llm.ChatRequest) (string, error) {
		u := userText(req)
		switch {
		case strings.Contains(u, "headline"):
			return titleReply, nil
		case strings.Contains(u, "multiple-choice"):
			return quizJSON, nil
		case strings.Contains(u, "one part of a longer explanation"):
			return chunkJSON, nil
		}
		// A section merge is legitimate (fragments joined into one
		// section); anything else means an unrouted prompt.
		if strings.Contains(u, "Rewrite them as one continuous") {
			return "<p>woven</p>", nil
		}
		t.Errorf("unexpected prompt: %q", u[:min(60, len(u))])
		return "", nil
	}}
	a, err := New(Options{Model: "m", Client: fake})
	if err != nil {
		t.Fatal(err)
	}

	spec, err := a.Explain(context.Background(), testRef(), []diff.File{smallFile("a.go")}, "Title: add tokens", nil)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Title != titleReply {
		t.Errorf("title = %q, want %q", spec.Title, titleReply)
	}
	if len(spec.Sections) != 3 {
		t.Fatalf("sections = %d, want 3 (background, intuition, code)", len(spec.Sections))
	}
	for i, want := range []string{"background", "intuition", "code"} {
		if spec.Sections[i].ID != want {
			t.Errorf("section %d id = %q, want %q", i, spec.Sections[i].ID, want)
		}
	}
	if !strings.Contains(spec.Sections[0].HTML, "callout") {
		t.Errorf("callouts missing from background: %q", spec.Sections[0].HTML)
	}
	if len(spec.Quiz) != 1 || !spec.Quiz[0].Options[0].Correct {
		t.Errorf("quiz = %+v", spec.Quiz)
	}
	if len(fake.chatCalls) != 3 {
		t.Errorf("calls = %d, want 3 (title, chunk, quiz)", len(fake.chatCalls))
	}
	if !strings.Contains(spec.Slug, "retries") {
		t.Errorf("slug = %q, want it derived from the title", spec.Slug)
	}
}

// The whole point of the 16K port: a change too big for one call must be
// split, and each section woven back together from several fragments.
func TestExplainMergesSectionsWhenTheyOverflow(t *testing.T) {
	var merges int
	fake := &fakeLLM{chatResp: func(call int, req llm.ChatRequest) (string, error) {
		u := userText(req)
		switch {
		case strings.Contains(u, "headline"):
			return titleReply, nil
		case strings.Contains(u, "multiple-choice"):
			return quizJSON, nil
		case strings.Contains(u, "Rewrite them as one continuous"):
			merges++
			if !strings.Contains(u, "Background") && !strings.Contains(u, "Intuition") && !strings.Contains(u, "Code walkthrough") {
				t.Errorf("merge prompt missing section name: %q", u)
			}
			return "<p>woven</p>", nil
		}
		return chunkJSON, nil
	}}
	// A tiny budget forces several chunks and, in turn, section merges.
	a, err := New(Options{Model: "m", Client: fake, MaxChunkTokens: 60})
	if err != nil {
		t.Fatal(err)
	}
	files := []diff.File{smallFile("a.go"), smallFile("b.go"), smallFile("c.go")}
	spec, err := a.Explain(context.Background(), testRef(), files, "Title: add tokens", nil)
	if err != nil {
		t.Fatal(err)
	}
	if merges == 0 {
		t.Error("expected at least one section merge with a small chunk budget")
	}
	if len(spec.Sections) != 3 {
		t.Errorf("sections = %d, want 3", len(spec.Sections))
	}
}

// No call may carry the raw placeholders, and the diff must actually be sent.
func TestExplainChunkPromptResolvesPlaceholders(t *testing.T) {
	fake := &fakeLLM{chatResp: func(call int, req llm.ChatRequest) (string, error) {
		switch {
		case strings.Contains(userText(req), "headline"):
			return titleReply, nil
		case strings.Contains(userText(req), "multiple-choice"):
			return quizJSON, nil
		}
		return chunkJSON, nil
	}}
	a, err := New(Options{Model: "m", Client: fake})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Explain(context.Background(), testRef(), []diff.File{smallFile("a.go")}, "intent", nil); err != nil {
		t.Fatal(err)
	}
	for _, req := range fake.chatCalls {
		u := userText(req)
		if strings.Contains(u, "{{diff}}") || strings.Contains(u, "{{context}}") || strings.Contains(u, "{{page}}") {
			t.Errorf("unresolved placeholder: %q", u)
		}
	}
}

// The diff must reach the model, and the pre-image context must be announced
// as absent rather than silently missing when there is no clone.
func TestExplainSendsDiffAndSaysWhenSourceIsMissing(t *testing.T) {
	var chunkPrompt string
	fake := &fakeLLM{chatResp: func(call int, req llm.ChatRequest) (string, error) {
		switch {
		case strings.Contains(userText(req), "headline"):
			return titleReply, nil
		case strings.Contains(userText(req), "multiple-choice"):
			return quizJSON, nil
		}
		chunkPrompt = userText(req)
		return chunkJSON, nil
	}}
	a, err := New(Options{Model: "m", Client: fake})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Explain(context.Background(), testRef(), []diff.File{smallFile("a.go")}, "intent", nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(chunkPrompt, "line one") {
		t.Errorf("chunk prompt missing the diff body: %q", chunkPrompt)
	}
	if !strings.Contains(chunkPrompt, "a.go") {
		t.Errorf("chunk prompt missing the file list: %q", chunkPrompt)
	}
	if !strings.Contains(chunkPrompt, "no surrounding source available") {
		t.Errorf("chunk prompt should state that no source context is available: %q", chunkPrompt)
	}
}

// A model that breaks the JSON format once must not cost the chunk: the
// pipeline retries with the format restated.
func TestExplainRetriesOnceOnBadJSON(t *testing.T) {
	var chunkCalls, retries int
	fake := &fakeLLM{chatResp: func(call int, req llm.ChatRequest) (string, error) {
		u := userText(req)
		switch {
		case strings.Contains(u, "headline"):
			return titleReply, nil
		case strings.Contains(u, "multiple-choice"):
			return quizJSON, nil
		case strings.Contains(allText(req), "Output format reminder"):
			retries++
			return chunkJSON, nil
		}
		chunkCalls++
		return "I'm sorry, here is the explanation you asked for.", nil
	}}
	a, err := New(Options{Model: "m", Client: fake})
	if err != nil {
		t.Fatal(err)
	}
	spec, err := a.Explain(context.Background(), testRef(), []diff.File{smallFile("a.go")}, "intent", nil)
	if err != nil {
		t.Fatal(err)
	}
	if chunkCalls != 1 || retries != 1 {
		t.Errorf("chunk calls = %d, retries = %d, want 1/1", chunkCalls, retries)
	}
	if len(spec.Sections) != 3 {
		t.Errorf("sections = %d, want the retried chunk to contribute all 3", len(spec.Sections))
	}
}

// A chunk that never parses is dropped, and the page still renders from the
// chunks that did.
func TestExplainDropsUnusableChunk(t *testing.T) {
	var chunkCalls int
	fake := &fakeLLM{chatResp: func(call int, req llm.ChatRequest) (string, error) {
		u := userText(req)
		switch {
		case strings.Contains(u, "headline"):
			return titleReply, nil
		case strings.Contains(u, "multiple-choice"):
			return quizJSON, nil
		case strings.Contains(allText(req), "Output format reminder"):
			return "still not json", nil
		}
		chunkCalls++
		return "prose, not an object", nil
	}}
	a, err := New(Options{Model: "m", Client: fake})
	if err != nil {
		t.Fatal(err)
	}
	spec, err := a.Explain(context.Background(), testRef(), []diff.File{smallFile("a.go")}, "intent", nil)
	if err == nil {
		t.Error("expected an error when every chunk fails to produce usable text")
	}
	if spec.Title != "" {
		t.Errorf("expected an empty spec, got %+v", spec)
	}
	if chunkCalls != 1 {
		t.Errorf("chunk calls = %d, want 1 (a single retry, not a loop)", chunkCalls)
	}
}

// A missing quiz is a valid outcome: the page is still a good page.
func TestExplainSurvivesQuizFailure(t *testing.T) {
	fake := &fakeLLM{chatResp: func(call int, req llm.ChatRequest) (string, error) {
		switch {
		case strings.Contains(userText(req), "headline"):
			return titleReply, nil
		case strings.Contains(userText(req), "multiple-choice"):
			return "I could not write questions.", nil
		}
		return chunkJSON, nil
	}}
	a, err := New(Options{Model: "m", Client: fake})
	if err != nil {
		t.Fatal(err)
	}
	spec, err := a.Explain(context.Background(), testRef(), []diff.File{smallFile("a.go")}, "intent", nil)
	if err != nil {
		t.Fatalf("a bad quiz must not fail the run: %v", err)
	}
	if len(spec.Quiz) != 0 {
		t.Errorf("quiz = %+v, want none", spec.Quiz)
	}
	if len(spec.Sections) != 3 {
		t.Errorf("sections = %d, want 3", len(spec.Sections))
	}
}

// A failed headline call falls back to the PR title rather than failing.
func TestExplainFallsBackToPRTitle(t *testing.T) {
	fake := &fakeLLM{chatResp: func(call int, req llm.ChatRequest) (string, error) {
		u := userText(req)
		switch {
		case strings.Contains(u, "headline"):
			return "", nil
		case strings.Contains(u, "multiple-choice"):
			return quizJSON, nil
		}
		return chunkJSON, nil
	}}
	a, err := New(Options{Model: "m", Client: fake})
	if err != nil {
		t.Fatal(err)
	}
	spec, err := a.Explain(context.Background(), testRef(), []diff.File{smallFile("a.go")}, "Title: add retry backoff\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Title != "add retry backoff" {
		t.Errorf("title = %q, want the PR title", spec.Title)
	}
}

func TestExplainEmptyDiffMakesNoCalls(t *testing.T) {
	fake := &fakeLLM{}
	a, err := New(Options{Model: "m", Client: fake})
	if err != nil {
		t.Fatal(err)
	}
	spec, err := a.Explain(context.Background(), testRef(), nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Title != "" || len(spec.Sections) != 0 {
		t.Errorf("expected an empty spec, got %+v", spec)
	}
	if len(fake.chatCalls) != 0 {
		t.Errorf("calls = %d, want 0 for an empty diff", len(fake.chatCalls))
	}
}

func TestIntentReachesSystemPrompt(t *testing.T) {
	fake := &fakeLLM{chatResp: func(call int, req llm.ChatRequest) (string, error) {
		switch {
		case strings.Contains(userText(req), "headline"):
			return titleReply, nil
		case strings.Contains(userText(req), "multiple-choice"):
			return quizJSON, nil
		}
		return chunkJSON, nil
	}}
	a, err := New(Options{Model: "m", Client: fake})
	if err != nil {
		t.Fatal(err)
	}
	intent := "Title: fix ABC-123 retry behaviour"
	if _, err := a.Explain(context.Background(), testRef(), []diff.File{smallFile("a.go")}, intent, nil); err != nil {
		t.Fatal(err)
	}
	system, _ := fake.chatCalls[0].Messages[0].Content.(string)
	if !strings.Contains(system, intent) || !strings.Contains(system, "Pull request intent") {
		t.Errorf("system prompt missing intent: %q", system)
	}
}

// Every call must stay inside the configured budget, which is the whole
// reason this mode exists.
func TestExplainRespectsTokenBudgets(t *testing.T) {
	fake := &fakeLLM{chatResp: func(call int, req llm.ChatRequest) (string, error) {
		switch {
		case strings.Contains(userText(req), "headline"):
			return titleReply, nil
		case strings.Contains(userText(req), "multiple-choice"):
			return quizJSON, nil
		}
		return chunkJSON, nil
	}}
	a, err := New(Options{Model: "m", Client: fake, MaxResponseTokens: 1500, MaxQuizTokens: 900})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Explain(context.Background(), testRef(), []diff.File{smallFile("a.go")}, "intent", nil); err != nil {
		t.Fatal(err)
	}
	sawQuiz := false
	for _, req := range fake.chatCalls {
		u := userText(req)
		if strings.Contains(u, "multiple-choice") {
			sawQuiz = true
			if req.MaxTokens != 900 {
				t.Errorf("quiz MaxTokens = %d, want 900", req.MaxTokens)
			}
			continue
		}
		if strings.Contains(u, "headline") {
			continue // the headline call is a single short line by design
		}
		if req.MaxTokens != 1500 {
			t.Errorf("chunk/merge MaxTokens = %d, want 1500", req.MaxTokens)
		}
	}
	if !sawQuiz {
		t.Error("expected a quiz call")
	}
}

// The quiz prompt must fit the budget: it carries the finished prose, which
// is the largest thing the pipeline ever assembles.
func TestPageTextRespectsBudget(t *testing.T) {
	big := strings.Repeat("word ", 4000) // ~4000 tokens
	sections := []Section{
		{ID: "background", Heading: "Background", HTML: "<p>" + big + "</p>"},
		{ID: "intuition", Heading: "Intuition", HTML: "<p>" + big + "</p>"},
		{ID: "code", Heading: "Code walkthrough", HTML: "<p>" + big + "</p>"},
	}
	got := pageText(sections, 1000)
	if tokens := chunk.EstimateTokens(got); tokens > 1000 {
		t.Errorf("page text = %d tokens, want <= 1000", tokens)
	}
	if strings.Contains(got, "<p>") {
		t.Error("page text should be stripped of markup before it is sent to the quiz call")
	}
	for _, h := range []string{"Background", "Intuition", "Code walkthrough"} {
		if !strings.Contains(got, h) {
			t.Errorf("page text missing the %s heading", h)
		}
	}
}

func testRef() diff.Ref { return diff.Ref{Owner: "octocat", Repo: "hello", Number: 7} }

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestSpecRoundTripsThroughJSON(t *testing.T) {
	spec := Spec{
		Title:    "Retries back off",
		Subtitle: "Prepared 1 October 2026",
		Slug:     "retries-back-off",
		Sections: []Section{{ID: "intuition", Heading: "Intuition", HTML: "<p>x</p>"}},
		Quiz:     []Question{{Question: "q", Options: []Option{{Text: "a", Correct: true, Feedback: "yes"}}}},
	}
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	var back Spec
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.Title != spec.Title || len(back.Sections) != 1 || len(back.Quiz) != 1 {
		t.Errorf("round trip lost data: %+v", back)
	}
	if !back.Quiz[0].Options[0].Correct || back.Quiz[0].Options[0].Feedback != "yes" {
		t.Errorf("option round trip lost data: %+v", back.Quiz[0].Options[0])
	}
}
