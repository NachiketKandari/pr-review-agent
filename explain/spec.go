// Package explain turns a code change into a rich, interactive HTML
// explanation: a narrative walkthrough of what the change does and why,
// followed by an auto-graded quiz on the substance of the change.
//
// It is the explain-diff skill
// (https://gist.github.com/ankitg12/8e808d387799de4e9839bc393f8e6405)
// ported to a 16K-context model with no tool use. The skill leans on an agent
// that can read the repository and hold the whole change in its context;
// neither is available here, so the same recipe is expressed as the
// map-reduce pipeline this tool already uses for reviews:
//
//   - The model only ever writes *content*, never page boilerplate, and only
//     as a JSON object: one small reply per diff chunk (the map step), then
//     per-section rewrites until each section fits the context budget (the
//     reduce step), then one quiz reply. [Render] owns the HTML, the CSS, the
//     JavaScript and the file naming — the same factoring the skill gets from
//     its render.py.
//   - "Broadly explore the surrounding code" becomes a mechanical step: when
//     a local clone is available, [diff.SourceContext] reads the pre-image of
//     the changed files, length-capped, so the model can describe the system
//     the change lands in instead of guessing at it. When no clone is
//     available the model is told to describe only what the diff shows.
//   - Every reply is capped and validated. Malformed JSON is repaired once
//     (code fences, surrounding prose, trailing commas); a chunk that still
//     fails is dropped and a page without a quiz is a valid outcome. Nothing
//     in the pipeline can overflow the context window silently.
//
// The renderer is deliberately a template, not a sanitizer: model-authored
// HTML reaches the page, so [sanitizeHTML] strips script/style/iframe tags and
// inline event handlers as defense in depth.
package explain

import (
	"encoding/json"
	"fmt"
	"html"
	"regexp"
	"strings"

	"github.com/NachiketKandari/pr-review-agent/xlog"
)

// Spec is the content of one explanation page. It is the on-disk
// intermediate: the tool writes it next to the rendered page so the HTML can
// be regenerated (pr-review-agent -render spec.json) without spending a
// single model call, exactly what the skill's render.py consumes.
type Spec struct {
	Title    string     `json:"title"`
	Subtitle string     `json:"subtitle,omitempty"`
	Slug     string     `json:"slug,omitempty"`
	Sections []Section  `json:"sections"`
	Quiz     []Question `json:"quiz,omitempty"`
}

// Section is one narrative chapter of the page (background, intuition, the
// code walkthrough). HTML is model-authored markup in the renderer's
// vocabulary; see the chunk prompt for the exact list.
type Section struct {
	ID      string `json:"id"`
	Heading string `json:"heading"`
	HTML    string `json:"html"`
}

// Question is one quiz item: a prompt, its options, and a per-option
// explanation shown after an answer is picked.
type Question struct {
	Question string   `json:"question"`
	Options  []Option `json:"options"`
}

// Option is one multiple-choice answer. Exactly one option of a question may
// be Correct.
type Option struct {
	Text     string `json:"text"`
	Correct  bool   `json:"correct"`
	Feedback string `json:"feedback,omitempty"`
}

// MaxQuizQuestions caps the quiz, matching the skill's five questions.
const MaxQuizQuestions = 5

const (
	maxQuizOptions = 6
	maxSlugLen     = 60
)

var (
	slugNonWord   = regexp.MustCompile(`[^a-z0-9]+`)
	slugDashes    = regexp.MustCompile(`-{2,}`)
	fenceLine     = regexp.MustCompile("(?m)^[ \t]*```.*$")
	trailingComma = regexp.MustCompile(`,(\s*[}\]])`)
	htmlTag       = regexp.MustCompile(`<[^>]*>`)
)

// Normalize fills in the derived fields the renderer relies on and drops
// content that would render badly: a section with no body, or a quiz
// question without exactly one correct answer, is removed rather than shown
// in a broken state. It is safe to call more than once.
func (s *Spec) Normalize() {
	if strings.TrimSpace(s.Title) == "" {
		s.Title = "Code change"
	}
	s.Slug = Slugify(firstNonEmpty(s.Slug, s.Title))

	sections := make([]Section, 0, len(s.Sections))
	for _, sec := range s.Sections {
		sec.HTML = strings.TrimSpace(sec.HTML)
		if sec.HTML == "" {
			continue
		}
		if sec.ID == "" {
			sec.ID = sec.Heading
		}
		sec.ID = Slugify(sec.ID)
		if sec.ID == "" {
			continue
		}
		if sec.Heading == "" {
			sec.Heading = sec.ID
		}
		sections = append(sections, sec)
	}
	s.Sections = sections
	s.Quiz = normalizeQuiz(s.Quiz)
}

// normalizeQuiz keeps only well-formed questions (a prompt, at least two
// options, exactly one of them correct), caps the number of options per
// question, and caps the quiz at MaxQuizQuestions. A question the model got
// wrong is dropped with a warning: a quiz that cannot be graded fairly is
// worse than a shorter one.
func normalizeQuiz(questions []Question) []Question {
	out := make([]Question, 0, len(questions))
	for _, q := range questions {
		q.Question = strings.TrimSpace(q.Question)
		opts := make([]Option, 0, len(q.Options))
		correct := 0
		for _, o := range q.Options {
			o.Text = strings.TrimSpace(o.Text)
			o.Feedback = strings.TrimSpace(o.Feedback)
			if o.Text == "" {
				continue
			}
			if o.Correct {
				correct++
			}
			opts = append(opts, o)
		}
		switch {
		case q.Question == "" || len(opts) < 2:
			xlog.Warn("dropping quiz question: no prompt or too few options",
				"options", len(opts))
			continue
		case correct != 1:
			xlog.Warn("dropping quiz question: exactly one option must be correct",
				"question", clip(q.Question, 80), "correct_options", correct)
			continue
		}
		q.Options = capOptions(opts, maxQuizOptions)
		out = append(out, q)
		if len(out) == MaxQuizQuestions {
			if len(questions) > len(out) {
				xlog.Info("quiz truncated", "kept", len(out), "offered", len(questions))
			}
			break
		}
	}
	return out
}

// capOptions trims an over-long option list, never dropping the correct
// answer: it is kept first, then as many distractors as room allows.
func capOptions(opts []Option, max int) []Option {
	if len(opts) <= max {
		return opts
	}
	kept := make([]Option, 0, max)
	for _, o := range opts {
		if o.Correct {
			kept = append(kept, o)
		}
	}
	for _, o := range opts {
		if len(kept) == max {
			break
		}
		if o.Correct {
			continue
		}
		kept = append(kept, o)
	}
	return kept
}

// Slugify reduces text to a lowercase, dash-separated filename fragment.
func Slugify(text string) string {
	s := slugDashes.ReplaceAllString(slugNonWord.ReplaceAllString(strings.ToLower(text), "-"), "-")
	s = strings.Trim(s, "-")
	if len(s) > maxSlugLen {
		s = strings.Trim(strings.TrimRight(s[:maxSlugLen], "-"), "-")
	}
	return s
}

// DecodeJSON decodes the first JSON value in raw into v, repairing the ways a
// language model usually breaks JSON: a sentence around the object, a markdown
// code fence, and trailing commas. The repairs are applied only after a plain
// decode fails, so well-formed input is never rewritten.
func DecodeJSON(raw string, v any) error {
	s := fenceLine.ReplaceAllString(raw, "")
	s = strings.TrimSpace(s)
	if s == "" {
		return fmt.Errorf("empty model response")
	}

	firstErr := json.Unmarshal([]byte(s), v)
	if firstErr == nil {
		return nil
	}

	// The object may be wrapped in a sentence, fenced, or both, and may carry
	// a trailing comma. Each repair is applied on its own, then in
	// combination, because a model tends to break the format in one way at a
	// time but not reliably that way.
	type repair struct {
		name string
		fn   func(string) string
	}
	extract := outermostObject
	repairs := []repair{
		{"unfenced", func(s string) string { return strings.TrimSpace(fenceLine.ReplaceAllString(s, "")) }},
		{"outermost object", extract},
		{"trailing commas", func(s string) string { return trailingComma.ReplaceAllString(s, "$1") }},
	}

	// Breadth-first over the repair combinations, cheapest first, so the
	// common cases (fence, wrapper, one stray comma) are all one or two
	// passes deep.
	seen := map[string]bool{s: true}
	frontier := []string{s}
	for depth := 1; depth <= len(repairs) && len(frontier) > 0; depth++ {
		var next []string
		for _, candidate := range frontier {
			for _, r := range repairs {
				out := r.fn(candidate)
				if out == "" || seen[out] {
					continue
				}
				seen[out] = true
				if err := json.Unmarshal([]byte(out), v); err == nil {
					return nil
				}
				next = append(next, out)
			}
		}
		frontier = next
	}
	return fmt.Errorf("model response is not valid JSON: %w", firstErr)
}

// outermostObject returns the substring from the first "{" to the last "}",
// which is where a model puts its JSON when it also wrote a sentence around
// it.
func outermostObject(s string) string {
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end <= start {
		return ""
	}
	return s[start : end+1]
}

// stripTags removes markup and unescapes entities, for feeding rendered
// sections back to the model as plain prose.
func stripTags(s string) string {
	return strings.TrimSpace(html.UnescapeString(htmlTag.ReplaceAllString(s, " ")))
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
