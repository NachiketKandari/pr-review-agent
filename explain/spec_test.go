package explain

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/NachiketKandari/pr-review-agent/diff"
)

func TestDecodeJSONRepairsModelOutput(t *testing.T) {
	want := struct {
		Background string `json:"background"`
		Code       string `json:"code"`
	}{}

	cases := map[string]string{
		"clean":                      `{"background":"a","code":"b"}`,
		"fenced":                     "Here you go:\n```json\n{\"background\":\"a\",\"code\":\"b\"}\n```\nHope that helps!",
		"prose-wrapped":              `Sure! {"background":"a","code":"b"} — let me know if you need more.`,
		"trailing-comma":             "{\"background\":\"a\",\"code\":\"b\",}",
		"trailing-comma-fenced":      "```\n{\"background\":\"a\",\"code\":\"b\",}\n```",
		"trailing-comma-in-array":    `{"background":"a","code":"b","xs":[1,2,]}`,
		"leading-and-trailing-space": "\n\n  {\"background\":\"a\",\"code\":\"b\"}  \n\n",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			var got struct {
				Background string `json:"background"`
				Code       string `json:"code"`
			}
			if err := DecodeJSON(raw, &got); err != nil {
				t.Fatalf("DecodeJSON(%q): %v", raw, err)
			}
			if got.Background != "a" || got.Code != "b" {
				t.Errorf("got %+v, want background=a code=b", got)
			}
		})
	}
	_ = want
}

// Output that is not JSON at all must fail rather than being half-parsed.
func TestDecodeJSONRejectsNonJSON(t *testing.T) {
	for _, raw := range []string{
		"",
		"   \n ",
		"I'm sorry, I cannot help with that.",
		"```\nno object here\n```",
		`{"background": "unterminated`,
		"{} {}",
	} {
		var out map[string]any
		if err := DecodeJSON(raw, &out); err == nil {
			t.Errorf("DecodeJSON(%q) = nil error, want a failure", raw)
		}
	}
}

// A well-formed reply must survive untouched: the repairs must not corrupt
// valid input, especially string contents that look like syntax.
func TestDecodeJSONLeavesValidInputAlone(t *testing.T) {
	type payload struct {
		Text string `json:"text"`
	}
	var got payload
	raw := "{\"text\":\"a comma, then } a brace, and a fence marker: ``` still text\"}"
	if err := DecodeJSON(raw, &got); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Text, "a comma, then } a brace") {
		t.Errorf("text = %q, want the string contents preserved verbatim", got.Text)
	}
}

func TestNormalizeDropsEmptyAndBrokenContent(t *testing.T) {
	spec := Spec{
		Sections: []Section{
			{ID: "background", Heading: "Background", HTML: "<p>a</p>"},
			{ID: "empty", Heading: "Empty", HTML: "   "},    // no body
			{ID: "code", HTML: "<pre><code>b</code></pre>"}, // heading derived
		},
		Quiz: []Question{
			{Question: "good", Options: []Option{
				{Text: "a", Correct: true},
				{Text: "b"},
			}},
			{Question: "no correct answer", Options: []Option{{Text: "a"}, {Text: "b"}}},
			{Question: "two correct answers", Options: []Option{
				{Text: "a", Correct: true}, {Text: "b", Correct: true},
			}},
			{Question: "one option", Options: []Option{{Text: "a", Correct: true}}},
			{Question: "", Options: []Option{{Text: "a", Correct: true}, {Text: "b"}}},
		},
	}
	spec.Normalize()

	if len(spec.Sections) != 2 {
		t.Errorf("sections = %d, want 2 (the empty one dropped)", len(spec.Sections))
	}
	if spec.Sections[1].Heading != "code" {
		t.Errorf("derived heading = %q, want %q", spec.Sections[1].Heading, "code")
	}
	if len(spec.Quiz) != 1 || spec.Quiz[0].Question != "good" {
		t.Errorf("quiz = %+v, want only the well-formed question", spec.Quiz)
	}
}

func TestNormalizeCapsTheQuizAtFive(t *testing.T) {
	var qs []Question
	for i := 0; i < 9; i++ {
		qs = append(qs, Question{
			Question: "q",
			Options:  []Option{{Text: "a", Correct: true}, {Text: "b"}},
		})
	}
	spec := Spec{Quiz: qs}
	spec.Normalize()
	if len(spec.Quiz) != MaxQuizQuestions {
		t.Errorf("quiz length = %d, want %d", len(spec.Quiz), MaxQuizQuestions)
	}
}

// Trimming an over-long option list must never discard the correct answer.
func TestCapOptionsKeepsTheCorrectAnswer(t *testing.T) {
	opts := []Option{
		{Text: "1"}, {Text: "2"}, {Text: "3"}, {Text: "4"},
		{Text: "5", Correct: true}, {Text: "6"},
	}
	kept := capOptions(opts, 3)
	if len(kept) != 3 {
		t.Fatalf("kept %d options, want 3", len(kept))
	}
	correct := 0
	for _, o := range kept {
		if o.Correct {
			correct++
		}
	}
	if correct != 1 {
		t.Errorf("kept %d correct options, want exactly 1: %+v", correct, kept)
	}
}

func TestNormalizeFillsTitleAndSlug(t *testing.T) {
	spec := Spec{Sections: []Section{{ID: "background", HTML: "<p>a</p>"}}}
	spec.Normalize()
	if spec.Title == "" || spec.Slug == "" {
		t.Errorf("title = %q, slug = %q, want both filled", spec.Title, spec.Slug)
	}
	// Normalize must be idempotent, since Render and the pipeline both call it.
	once := *(&spec)
	once.Normalize()
	if once.Title != spec.Title || once.Slug != spec.Slug || len(once.Sections) != len(spec.Sections) {
		t.Errorf("Normalize is not idempotent: %+v vs %+v", once, spec)
	}
}

func TestSpecUnmarshalFromSavedFile(t *testing.T) {
	// The .spec.json written beside a page must be loadable and renderable,
	// which is what makes re-rendering free.
	saved := `{
	  "title": "Retries back off",
	  "subtitle": "Prepared 1 October 2026",
	  "slug": "retries-back-off",
	  "sections": [
	    {"id": "background", "heading": "Background", "html": "<p>a</p>"},
	    {"id": "intuition", "heading": "Intuition", "html": "<p>b</p>"}
	  ],
	  "quiz": [
	    {"question": "q?", "options": [
	      {"text": "yes", "correct": true, "feedback": "right"},
	      {"text": "no", "feedback": "wrong"}
	    ]}
	  ]
	}`
	var spec Spec
	if err := json.Unmarshal([]byte(saved), &spec); err != nil {
		t.Fatal(err)
	}
	page, err := Render(spec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(page, "Retries back off") || !strings.Contains(page, "right") {
		t.Error("a saved spec did not render its content")
	}
}

func TestCleanLine(t *testing.T) {
	cases := map[string]string{
		"Retries back off":           "Retries back off",
		"  \n Retries back off \n\n": "Retries back off",
		`"Retries back off"`:         "Retries back off",
		"**Retries back off**":       "Retries back off",
		"line one\nline two":         "line one",
		"   \n\n   ":                 "",
	}
	for in, want := range cases {
		if got := cleanLine(in); got != want {
			t.Errorf("cleanLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIntentTitleFallsBackToRepo(t *testing.T) {
	ref := testRef()
	if got := intentTitle("Title: add retry backoff\nSource branch: f\n", ref); got != "add retry backoff" {
		t.Errorf("got %q, want the PR title", got)
	}
	if got := intentTitle("Source branch: f\n", ref); got == "" {
		t.Error("expected a repository-based fallback")
	}
	if got := intentTitle("", diff.Ref{}); got == "" {
		t.Error("expected a non-empty fallback even with no ref")
	}
}
