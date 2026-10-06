package explain

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/NachiketKandari/pr-review-agent/chunk"
	"github.com/NachiketKandari/pr-review-agent/diff"
	"github.com/NachiketKandari/pr-review-agent/llm"
	"github.com/NachiketKandari/pr-review-agent/xlog"
)

// Defaults applied when Options leaves a field zero. The budgets are tuned
// for a 16K-context model: a ~7K-token chunk plus a ~3K-token JSON reply
// leaves room for the system prompt, the PR intent, and the pre-image source
// context without ever crossing the window. Unlike review mode, explanation
// replies are long (markup, not bullets), so the chunk budget is smaller and
// the response cap proportionally larger.
const (
	defaultMaxChunkTokens    = 7000
	defaultMaxResponseTokens = 3000
	defaultQuizTokens        = 2000
	defaultTitleTokens       = 120
	defaultTemperature       = 0.4

	// maxMergeDepth bounds the reduce recursion when even merged fragments
	// keep overflowing the budget.
	maxMergeDepth = 8
)

// sectionOrder is the order the page is written in, and the vocabulary the
// chunk prompt's JSON keys map onto.
var sectionOrder = []struct{ id, heading, key string }{
	{"background", "Background", "background"},
	{"intuition", "Intuition", "intuition"},
	{"code", "Code walkthrough", "code"},
}

// LLM is the minimal model surface explanation needs. *llm.Client satisfies
// it. There is no streaming: the artifact is a file, so there is nothing to
// stream to a terminal as it is written.
type LLM interface {
	Chat(ctx context.Context, req llm.ChatRequest) (string, error)
}

// Source locates the pre-image of the change — the code as it looked before
// — inside a local git clone, so the explanation can describe the system the
// change lands in instead of guessing at callers and types. It is the
// mechanical stand-in for the skill's "broadly explore the surrounding
// code": a language model on its own has no way to go and look.
//
// A nil Source, or one with an empty Dir or Rev, is not an error: the
// explanation then works from the diff alone and the prompts say so, so the
// model marks its guesses rather than inventing them.
type Source struct {
	Dir       string // local clone directory (the current dir by default)
	Rev       string // rev whose tree holds the pre-image (a merge base or base branch)
	MaxFiles  int    // 0 = 6
	MaxTokens int    // 0 = 3000, shared by all files
}

// Options configures an explain Agent.
type Options struct {
	Model  string
	Client LLM

	SystemPrompt      string  // "" = default
	ChunkPrompt       string  // "" = default; supports {{index}} {{total}} {{files}} {{functions}} {{diff}} {{context}} {{responseTokens}}
	MergePrompt       string  // "" = default; supports {{section}} {{fragments}} {{responseTokens}}
	QuizPrompt        string  // "" = default; supports {{page}} {{count}} {{responseTokens}}
	TitlePrompt       string  // "" = default; supports {{intent}} {{files}}
	MaxChunkTokens    int     // 0 = 7000; input budget per call
	MaxResponseTokens int     // 0 = 3000; cap on chunk and merge replies
	MaxQuizTokens     int     // 0 = 2000
	Temperature       float64 // 0 = 0.4 (prose, not analysis)
}

// Agent runs one explanation pass over a fetched diff.
type Agent struct {
	model        string
	systemPrompt string
	chunkPrompt  string
	mergePrompt  string
	quizPrompt   string
	titlePrompt  string
	maxChunk     int
	maxResponse  int
	quizTokens   int
	temperature  float64
	client       LLM
	intent       string // PR intent set per Explain call
}

// New validates Options and applies defaults.
func New(opts Options) (*Agent, error) {
	if opts.Model == "" {
		return nil, fmt.Errorf("explain: model is required")
	}
	if opts.Client == nil {
		return nil, fmt.Errorf("explain: client is required")
	}

	a := &Agent{
		model:        opts.Model,
		systemPrompt: opts.SystemPrompt,
		chunkPrompt:  opts.ChunkPrompt,
		mergePrompt:  opts.MergePrompt,
		quizPrompt:   opts.QuizPrompt,
		titlePrompt:  opts.TitlePrompt,
		maxChunk:     opts.MaxChunkTokens,
		maxResponse:  opts.MaxResponseTokens,
		quizTokens:   opts.MaxQuizTokens,
		temperature:  opts.Temperature,
		client:       opts.Client,
	}
	if a.systemPrompt == "" {
		a.systemPrompt = defaultSystemPrompt
	}
	if a.chunkPrompt == "" {
		a.chunkPrompt = defaultChunkPrompt
	} else if !strings.Contains(a.chunkPrompt, "{{diff}}") {
		xlog.Warn("explain.chunkPrompt is set but has no {{diff}} placeholder; the diff text will not be sent")
	}
	if a.mergePrompt == "" {
		a.mergePrompt = defaultMergePrompt
	} else if !strings.Contains(a.mergePrompt, "{{fragments}}") {
		xlog.Warn("explain.mergePrompt is set but has no {{fragments}} placeholder; the fragments will not be sent")
	}
	if a.quizPrompt == "" {
		a.quizPrompt = defaultQuizPrompt
	} else if !strings.Contains(a.quizPrompt, "{{page}}") {
		xlog.Warn("explain.quizPrompt is set but has no {{page}} placeholder; the page text will not be sent")
	}
	if a.titlePrompt == "" {
		a.titlePrompt = defaultTitlePrompt
	} else if !strings.Contains(a.titlePrompt, "{{intent}}") {
		xlog.Warn("explain.titlePrompt is set but has no {{intent}} placeholder; the PR intent will not be sent")
	}
	if a.maxChunk <= 0 {
		a.maxChunk = defaultMaxChunkTokens
	}
	if a.maxResponse <= 0 {
		a.maxResponse = defaultMaxResponseTokens
	}
	if a.quizTokens <= 0 {
		a.quizTokens = defaultQuizTokens
	}
	if a.temperature <= 0 {
		a.temperature = defaultTemperature
	}
	return a, nil
}

// fragment is one labelled piece of a section on its way to the page.
type fragment struct {
	label string
	html  string
}

// chunkNotes is one chunk's JSON reply: the three narrative sections plus
// optional callouts.
type chunkNotes struct {
	Background string   `json:"background"`
	Intuition  string   `json:"intuition"`
	Code       string   `json:"code"`
	Callouts   []string `json:"callouts"`
}

type quizReply struct {
	Questions []Question `json:"questions"`
}

// Explain writes the whole explanation of files and returns the page spec;
// rendering it is Render's job. intent is the PR title, description and
// commit messages (the same context review mode injects), and src optionally
// points at a local clone holding the pre-image.
//
// Every stage is logged: the source context, each chunk, each merge level,
// the title, and the quiz, so a page can be traced back to the calls that
// produced it. Failures are contained rather than fatal: a chunk whose
// output cannot be parsed is dropped, and a quiz that cannot be generated
// simply is not on the page.
func (a *Agent) Explain(ctx context.Context, ref diff.Ref, files []diff.File, intent string, src *Source) (Spec, error) {
	a.intent = intent
	defer func() { a.intent = "" }()
	xlog.Info("explain start",
		"pr", ref.String(), "model", a.model,
		"files", len(files), "intent_chars", len(intent),
		"max_chunk_tokens", a.maxChunk, "max_response_tokens", a.maxResponse,
		"temperature", a.temperature)

	chunks, err := chunk.Build(files, a.maxChunk)
	if err != nil {
		return Spec{}, fmt.Errorf("chunk diff: %w", err)
	}
	if len(chunks) == 0 {
		xlog.Info("nothing to explain; no parseable diff content", "pr", ref.String())
		return Spec{}, nil
	}

	source := a.sourceContext(ctx, files, src)

	// Map: one capped, JSON-only reply per chunk. Each reply contributes a
	// fragment to every section.
	bySection := map[string][]fragment{}
	for _, c := range chunks {
		label := fmt.Sprintf("chunk %d/%d — %s", c.Index, c.Total, strings.Join(c.Files, ", "))
		start := time.Now()
		xlog.Info("chunk explain start",
			"pr", ref.String(), "chunk", c.Index, "total", c.Total,
			"files", strings.Join(c.Files, ","), "functions", strings.Join(c.Functions, ","),
			"tokens", c.Tokens)

		var notes chunkNotes
		if err := a.askJSON(ctx, a.buildChunkMessages(c, source), a.maxResponse, &notes); err != nil {
			xlog.Warn("chunk explanation unusable; dropping it from the page",
				"pr", ref.String(), "chunk", c.Index, "error", err)
			continue
		}
		xlog.Info("chunk explain complete",
			"pr", ref.String(), "chunk", c.Index, "total", c.Total,
			"duration_ms", time.Since(start).Milliseconds(),
			"response_chars", chunk.EstimateTokens(notes.String())*4)
		for _, s := range sectionOrder {
			html := sanitizeHTML(strings.TrimSpace(notes.section(s.key)))
			// Callouts ride along inside the background fragment of the
			// same chunk. Appending them as a fragment of their own would
			// make even a single-chunk change look like a section that
			// needs weaving, and cost a merge call for nothing.
			if s.id == "background" {
				html += sanitizeHTML(calloutsHTML(notes.Callouts))
			}
			if html = strings.TrimSpace(html); html != "" {
				bySection[s.id] = append(bySection[s.id], fragment{label: label, html: html})
			}
		}
	}

	if len(bySection["background"])+len(bySection["intuition"])+len(bySection["code"]) == 0 {
		return Spec{}, fmt.Errorf("every chunk of the diff failed to produce usable explanation text")
	}

	// Reduce: weave each section's fragments into one, recursively when they
	// overflow the context budget.
	title := a.headline(ctx, ref, files)
	if title == "" {
		title = intentTitle(intent, ref)
	}
	spec := Spec{
		Title:    title,
		Subtitle: subtitle(ref, files),
	}
	for _, s := range sectionOrder {
		html := a.reduceSection(ctx, ref, s, bySection[s.id], 0)
		if html == "" {
			continue
		}
		spec.Sections = append(spec.Sections, Section{ID: s.id, Heading: s.heading, HTML: html})
	}
	spec.Quiz = a.quiz(ctx, spec.Sections)
	spec.Normalize()

	xlog.Info("explain complete",
		"pr", ref.String(),
		"sections", len(spec.Sections), "quiz_questions", len(spec.Quiz),
		"html_bytes", chunk.EstimateTokens(renderableText(spec))*4)
	return spec, nil
}

// section returns the named part of a chunk reply.
func (n chunkNotes) section(key string) string {
	switch key {
	case "background":
		return n.Background
	case "intuition":
		return n.Intuition
	case "code":
		return n.Code
	}
	return ""
}

// String is a rough size of the whole reply, for logs.
func (n chunkNotes) String() string {
	return n.Background + n.Intuition + n.Code + strings.Join(n.Callouts, "")
}

// calloutsHTML wraps the chunk's callouts in the renderer's callout
// markup. An empty list renders nothing.
func calloutsHTML(callouts []string) string {
	var b strings.Builder
	for _, c := range callouts {
		if c = strings.TrimSpace(c); c == "" {
			continue
		}
		b.WriteString(`<div class="callout">` + c + "</div>")
	}
	return b.String()
}

// reduceSection weaves one section's fragments into a single block of HTML.
// A lone fragment is already the whole section. Otherwise the fragments are
// packed into budget-sized groups, each group is rewritten by one call, and
// the results are reduced again.
func (a *Agent) reduceSection(ctx context.Context, ref diff.Ref, s struct{ id, heading, key string }, frags []fragment, depth int) string {
	if len(frags) == 0 {
		return ""
	}
	if len(frags) == 1 {
		xlog.Info("section kept as written", "pr", ref.String(), "section", s.id, "fragments", 1)
		return frags[0].html
	}

	if sumTokens(frags) <= a.maxChunk || depth >= maxMergeDepth {
		start := time.Now()
		xlog.Info("section merge start",
			"pr", ref.String(), "section", s.id, "fragments", len(frags),
			"tokens", sumTokens(frags), "depth", depth)
		merged, err := a.mergeSection(ctx, s.heading, frags)
		if err != nil {
			xlog.Warn("section merge failed; joining the fragments unmerged",
				"pr", ref.String(), "section", s.id, "error", err)
			return joinFragments(frags)
		}
		xlog.Info("section merge complete",
			"pr", ref.String(), "section", s.id, "fragments", len(frags),
			"duration_ms", time.Since(start).Milliseconds(),
			"response_tokens_est", chunk.EstimateTokens(merged))
		return merged
	}

	groups := pack(frags, a.maxChunk)
	merged := make([]fragment, 0, len(groups))
	for i, g := range groups {
		if len(g) == 1 && chunk.EstimateTokens(g[0].html) > a.maxChunk {
			// A lone fragment bigger than the whole budget passes through; a
			// later round may still have room for it once its neighbours
			// have been rewritten down.
			merged = append(merged, g[0])
			continue
		}
		out, err := a.mergeSection(ctx, s.heading, g)
		if err != nil {
			xlog.Warn("section merge failed; joining the fragments unmerged",
				"pr", ref.String(), "section", s.id, "error", err)
			merged = append(merged, fragment{label: g[0].label, html: joinFragments(g)})
			continue
		}
		merged = append(merged, fragment{label: fmt.Sprintf("part %d of %d", i+1, len(groups)), html: out})
	}
	return a.reduceSection(ctx, ref, s, merged, depth+1)
}

// mergeSection rewrites a group of fragments into one continuous section.
func (a *Agent) mergeSection(ctx context.Context, section string, frags []fragment) (string, error) {
	messages := []llm.Message{
		{Role: "system", Content: a.systemText()},
		{Role: "user", Content: strings.NewReplacer(
			"{{section}}", section,
			"{{fragments}}", labelFragments(frags),
			"{{responseTokens}}", strconv.Itoa(a.maxResponse),
		).Replace(a.mergePrompt)},
	}
	res, err := a.client.Chat(ctx, llm.ChatRequest{
		Model:       a.model,
		Messages:    messages,
		MaxTokens:   a.maxResponse,
		Temperature: floatPtr(a.temperature),
	})
	if err != nil {
		return "", err
	}
	return sanitizeHTML(strings.TrimSpace(res)), nil
}

// quiz asks for the closing questions, given the finished prose. Markup is
// stripped first: the questions test the substance, and the tags would only
// spend the context budget. A quiz that cannot be generated is not fatal —
// the page is still a good page.
func (a *Agent) quiz(ctx context.Context, sections []Section) []Question {
	page := pageText(sections, a.maxChunk)
	if page == "" {
		return nil
	}
	var reply quizReply
	messages := []llm.Message{
		{Role: "system", Content: a.systemText()},
		{Role: "user", Content: strings.NewReplacer(
			"{{page}}", page,
			"{{count}}", strconv.Itoa(MaxQuizQuestions),
			"{{responseTokens}}", strconv.Itoa(a.quizTokens),
		).Replace(a.quizPrompt)},
	}
	start := time.Now()
	xlog.Info("quiz start", "page_tokens", chunk.EstimateTokens(page), "questions", MaxQuizQuestions)
	if err := a.askJSON(ctx, messages, a.quizTokens, &reply); err != nil {
		xlog.Warn("quiz could not be generated; the page ships without one", "error", err)
		return nil
	}
	xlog.Info("quiz complete",
		"duration_ms", time.Since(start).Milliseconds(),
		"questions", len(reply.Questions))
	return reply.Questions
}

// headline proposes the page title. It is one tiny call whose failure costs
// nothing: the PR title, or the repository, becomes the headline instead.
func (a *Agent) headline(ctx context.Context, ref diff.Ref, files []diff.File) string {
	messages := []llm.Message{
		{Role: "system", Content: a.systemText()},
		{Role: "user", Content: strings.NewReplacer(
			"{{intent}}", clip(a.intent, 1200),
			"{{files}}", fileList(files),
		).Replace(a.titlePrompt)},
	}
	res, err := a.client.Chat(ctx, llm.ChatRequest{
		Model:       a.model,
		Messages:    messages,
		MaxTokens:   defaultTitleTokens,
		Temperature: floatPtr(a.temperature),
	})
	if err != nil {
		xlog.Warn("headline call failed; using the pull request title", "error", err)
		return ""
	}
	title := cleanLine(res)
	if title == "" {
		xlog.Warn("headline call returned nothing usable; using the pull request title")
		return ""
	}
	return title
}

// sourceContext reads the pre-image of the changed files from a local clone.
// Every failure is non-fatal: no clone, no rev, a file that does not exist
// there (it is new in this change), a slow git — all of them just mean the
// explanation works from the diff alone.
func (a *Agent) sourceContext(ctx context.Context, files []diff.File, src *Source) string {
	if src == nil || src.Dir == "" || src.Rev == "" {
		xlog.Info("no local clone to read the pre-image from; explaining from the diff alone")
		return ""
	}
	maxFiles, maxTokens := src.MaxFiles, src.MaxTokens
	if maxFiles <= 0 {
		maxFiles = 6
	}
	if maxTokens <= 0 {
		maxTokens = 3000
	}
	start := time.Now()
	context := diff.SourceContext(ctx, src.Dir, src.Rev, files, maxFiles, maxTokens)
	if context == "" {
		return ""
	}
	xlog.Info("gathered pre-image source context",
		"rev", src.Rev, "dir", src.Dir,
		"duration_ms", time.Since(start).Milliseconds(),
		"tokens", chunk.EstimateTokens(context))
	return context
}

// buildChunkMessages renders the map prompt for one chunk, substituting the
// chunk, the intent, and the pre-image source context.
func (a *Agent) buildChunkMessages(c chunk.Chunk, source string) []llm.Message {
	noSource := "There is no surrounding source available for this change. Describe only what the diff itself shows, " +
		"and where you would need a caller, a type or a config value that the diff does not show, say so plainly " +
		"instead of assuming one."
	if source == "" {
		source = noSource
	}
	user := strings.NewReplacer(
		"{{index}}", strconv.Itoa(c.Index),
		"{{total}}", strconv.Itoa(c.Total),
		"{{files}}", strings.Join(c.Files, ", "),
		"{{functions}}", strings.Join(c.Functions, ", "),
		"{{diff}}", c.Text,
		"{{context}}", source,
		"{{responseTokens}}", strconv.Itoa(a.maxResponse),
	).Replace(a.chunkPrompt)
	return []llm.Message{
		{Role: "system", Content: a.systemText()},
		{Role: "user", Content: user},
	}
}

// askJSON performs one capped, non-streamed call and decodes the reply as
// JSON. A reply that will not parse gets exactly one more attempt with the
// format restated — cheap, because the bad reply is not echoed back into the
// context — and after that the caller decides what to do about it.
func (a *Agent) askJSON(ctx context.Context, messages []llm.Message, maxTokens int, out any) error {
	res, err := a.chat(ctx, messages, maxTokens)
	if err != nil {
		return err
	}
	if err := DecodeJSON(res, out); err == nil {
		return nil
	} else {
		xlog.Warn("model reply was not valid JSON; asking once more for the object only", "error", err)
	}

	retry := append(append([]llm.Message{}, messages...), llm.Message{
		Role: "user",
		Content: "Output format reminder: reply with a single raw JSON object and nothing else — " +
			"no prose, no markdown code fences, no trailing commas, and no unescaped newlines inside strings.",
	})
	res, err = a.chat(ctx, retry, maxTokens)
	if err != nil {
		return err
	}
	return DecodeJSON(res, out)
}

func (a *Agent) chat(ctx context.Context, messages []llm.Message, maxTokens int) (string, error) {
	start := time.Now()
	res, err := a.client.Chat(ctx, llm.ChatRequest{
		Model:       a.model,
		Messages:    messages,
		MaxTokens:   maxTokens,
		Temperature: floatPtr(a.temperature),
	})
	if err != nil {
		return "", err
	}
	xlog.Debug("model chat complete", "model", a.model,
		"duration_ms", time.Since(start).Milliseconds(), "chars", len(res))
	return res, nil
}

// systemText returns the system prompt with the PR intent appended, so the
// page is anchored to what the change is supposed to achieve.
func (a *Agent) systemText() string {
	if a.intent == "" {
		return a.systemPrompt
	}
	return a.systemPrompt + "\n\n## Pull request intent\n" + a.intent
}

// labelFragments renders fragments for a merge prompt. The labels exist so
// the merge can order and attribute; the merge prompt tells the model to
// keep them out of the page.
func labelFragments(frags []fragment) string {
	var b strings.Builder
	for _, f := range frags {
		b.WriteString("\n### " + f.label + "\n")
		b.WriteString(f.html)
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

// joinFragments is the fallback when a merge call fails: the fragments are
// concatenated rather than lost, with their labels dropped so the page has
// no visible seams.
func joinFragments(frags []fragment) string {
	parts := make([]string, 0, len(frags))
	for _, f := range frags {
		parts = append(parts, f.html)
	}
	return strings.Join(parts, "\n")
}

// pageText renders the finished sections as plain prose for the quiz call,
// within budget. Sections are weighted so the code walkthrough — the densest
// part of the page — still reaches the quiz.
func pageText(sections []Section, budget int) string {
	if budget <= 0 {
		return ""
	}
	weights := map[string]int{"background": 1, "intuition": 1, "code": 2}
	remaining := budget
	var b strings.Builder
	for i, s := range sections {
		weight := weights[s.ID]
		if weight == 0 {
			weight = 1
		}
		share := budget * weight / 5
		if i == len(sections)-1 {
			share = remaining
		}
		if share > remaining {
			share = remaining
		}
		text := clip(stripTags(s.HTML), share)
		remaining -= chunk.EstimateTokens(text)
		if strings.TrimSpace(text) == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("### " + s.Heading + "\n" + text)
		if remaining <= 0 {
			break
		}
	}
	return strings.TrimSpace(b.String())
}

// subtitle is the one-line context under the headline: when the page was
// written, what it explains, and how much of it there is.
func subtitle(ref diff.Ref, files []diff.File) string {
	added, deletions := 0, 0
	for _, f := range files {
		added += f.Additions
		deletions += f.Deletions
	}
	what := ref.Owner + "/" + ref.Repo
	if ref.Number > 0 {
		what = "PR #" + strconv.Itoa(ref.Number)
	}
	return fmt.Sprintf("Prepared %s · %s · %d file%s changed (+%d/−%d)",
		time.Now().Format("2 January 2006"), what, len(files),
		plural(len(files), "", "s"), added, deletions)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// intentTitle recovers the PR title from the intent block, for when the
// headline call did not deliver one.
func intentTitle(intent string, ref diff.Ref) string {
	for _, line := range strings.Split(intent, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "Title:"); ok {
			if v = strings.TrimSpace(v); v != "" {
				return v
			}
		}
	}
	if ref.Owner != "" {
		return "What changed in " + ref.Owner + "/" + ref.Repo
	}
	return "What changed"
}

// cleanLine takes the first non-empty line of a reply and trims decoration
// a model may wrap a one-line answer in.
func cleanLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		line = strings.Trim(line, "*_`\"'")
		if line != "" {
			return clip(line, 120)
		}
	}
	return ""
}

// fileList names the changed files for the headline call, capped so a
// 200-file diff does not crowd out the instruction.
func fileList(files []diff.File) string {
	const maxFiles = 12
	paths := make([]string, 0, len(files))
	for _, f := range files {
		if len(paths) == maxFiles {
			paths = append(paths, fmt.Sprintf("and %d more", len(files)-maxFiles))
			break
		}
		paths = append(paths, f.Path)
	}
	if len(paths) == 0 {
		return "(not known)"
	}
	return strings.Join(paths, ", ")
}

// pack greedily groups fragments so each group fits the token budget; a lone
// oversized fragment is its own group.
func pack(frags []fragment, budget int) [][]fragment {
	var groups [][]fragment
	var cur []fragment
	total := 0
	for _, f := range frags {
		t := chunk.EstimateTokens(f.html)
		if len(cur) > 0 && total+t > budget {
			groups = append(groups, cur)
			cur = nil
			total = 0
		}
		cur = append(cur, f)
		total += t
	}
	if len(cur) > 0 {
		groups = append(groups, cur)
	}
	return groups
}

func sumTokens(frags []fragment) int {
	total := 0
	for _, f := range frags {
		total += chunk.EstimateTokens(f.html)
	}
	return total
}

func floatPtr(f float64) *float64 { return &f }
