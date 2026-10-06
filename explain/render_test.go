package explain

import (
	"strings"
	"testing"
)

func TestRenderProducesSelfContainedPage(t *testing.T) {
	spec := Spec{
		Title:    "Retries now back off with jitter",
		Subtitle: "Prepared 1 October 2026 · PR #482",
		Sections: []Section{
			{ID: "background", Heading: "Background", HTML: `<p>The client sent every request at once.</p>`},
			{ID: "intuition", Heading: "Intuition", HTML: `<div class="diagram"><div class="flow"><div class="box">send</div><div class="arrow">&rarr;</div><div class="box fail">500</div></div></div>`},
			{ID: "code", Heading: "Code walkthrough", HTML: "<pre><code>delay := backoff(attempt)</code></pre>"},
		},
		Quiz: []Question{{
			Question: "Why is the first retry immediate?",
			Options: []Option{
				{Text: "The delay is computed before the first attempt", Correct: true, Feedback: "The multiplier applies after the first send."},
				{Text: "The jitter returned a negative delay", Feedback: "Jitter only ever adds time."},
			},
		}},
	}
	page, err := Render(spec)
	if err != nil {
		t.Fatal(err)
	}

	// Self-contained: one file, no external fetches.
	for _, forbidden := range []string{"<script src", "<link rel=\"stylesheet\"", "http://", "https://"} {
		if strings.Contains(page, forbidden) {
			t.Errorf("page is not self-contained; found %q", forbidden)
		}
	}
	if !strings.HasPrefix(page, "<!DOCTYPE html>") || !strings.HasSuffix(strings.TrimSpace(page), "</html>") {
		t.Error("page is missing its doctype or closing tag")
	}
	// The style and the quiz behaviour are inlined.
	if !strings.Contains(page, ".quiz-opt") || !strings.Contains(page, "addEventListener") {
		t.Error("page is missing its CSS or quiz JavaScript")
	}
	// Table of contents covers every section plus the quiz.
	for _, want := range []string{`href="#background"`, `href="#intuition"`, `href="#code"`, `href="#quiz"`} {
		if !strings.Contains(page, want) {
			t.Errorf("table of contents missing %s", want)
		}
	}
	// Section anchors must match the section ids, or the links break.
	if !strings.Contains(page, `<h2 id="background">Background</h2>`) {
		t.Error("section heading/anchor mismatch")
	}
	// Model-authored markup passes through untouched: the diagram classes the
	// prompt teaches must survive to the page.
	if !strings.Contains(page, `<div class="box fail">500</div>`) {
		t.Error("model-authored diagram markup was mangled")
	}
	// Exactly one option is marked correct, and its feedback is carried.
	if strings.Count(page, `data-correct="true"`) != 1 {
		t.Errorf("correct markers = %d, want 1", strings.Count(page, `data-correct="true"`))
	}
	if !strings.Contains(page, "The multiplier applies after the first send.") {
		t.Error("option feedback missing from the page")
	}
}

func TestRenderEscapesEverythingButSectionHTML(t *testing.T) {
	spec := Spec{
		// A title carrying markup must not become markup: the title is
		// text the model chose, and it sits outside the section body.
		Title: `<script>alert(1)</script> & "quotes"`,
		Sections: []Section{
			{ID: "background", Heading: "Background", HTML: "<p>kept</p>"},
		},
		Quiz: []Question{{
			Question: `is <b>bold</b>?`,
			Options:  []Option{{Text: `<img src=x onerror=alert(1)>`, Correct: true, Feedback: "a < b"}},
		}},
	}
	page, err := Render(spec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(page, "<script>alert(1)</script>") {
		t.Error("title markup was not escaped")
	}
	if strings.Contains(page, "<img src=x") || strings.Contains(page, "onerror=alert(1)>") {
		t.Error("quiz option markup was not escaped")
	}
	if strings.Contains(page, "<b>bold</b>?") {
		t.Error("question markup was not escaped")
	}
	// The section body is the one place markup is meant to survive.
	if !strings.Contains(page, "<p>kept</p>") {
		t.Error("section HTML should pass through as markup")
	}
}

func TestRenderRejectsEmptySpec(t *testing.T) {
	if _, err := Render(Spec{Title: "x"}); err == nil {
		t.Error("expected an error for a spec with no content")
	}
}

// A page with no quiz must not render the quiz heading or the score line.
func TestRenderWithoutQuiz(t *testing.T) {
	page, err := Render(Spec{
		Title:    "x",
		Sections: []Section{{ID: "background", Heading: "Background", HTML: "<p>y</p>"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The CSS and JS are inlined unconditionally (one page, one set of
	// assets), so check the rendered body: a page with no quiz must not
	// show a quiz heading or a score line.
	body := page[strings.Index(page, "<body>"):]
	if strings.Contains(body, `<h2 id="quiz">`) || strings.Contains(body, `id="quiz-score"`) {
		t.Errorf("quiz scaffolding rendered without a quiz:\n%s", body)
	}
}

func TestSanitizeHTMLStripsActiveContent(t *testing.T) {
	cases := []struct {
		name, in, wantKept, wantGone string
	}{
		{"script tag", `<p>a</p><script>alert(1)</script>`, "<p>a</p>", "alert(1)"},
		{"uppercase script", `<SCRIPT>alert(1)</SCRIPT>`, "", "alert(1)"},
		{"iframe", `<iframe src="http://x"></iframe><p>a</p>`, "<p>a</p>", "iframe"},
		{"event handler", `<p onclick="steal()">a</p>`, ">a</p>", "onclick"},
		{"single-quoted handler", `<p onmouseover='x()'>a</p>`, ">a</p>", "onmouseover"},
		{"style block", `<style>body{display:none}</style><p>a</p>`, "<p>a</p>", "display:none"},
		// The allowed vocabulary must be left completely alone.
		{"diagram classes", `<div class="diagram"><div class="box fail">500</div></div>`,
			`<div class="diagram"><div class="box fail">500</div></div>`, ""},
		{"callout", `<div class="callout"><strong>token</strong> &mdash; a unit</div>`,
			`<div class="callout"><strong>token</strong> &mdash; a unit</div>`, ""},
		{"code block", "<pre><code>if a &lt; b { x++ }</code></pre>",
			"<pre><code>if a &lt; b { x++ }</code></pre>", ""},
		{"table", "<table><thead><th>a</th></thead><tbody><tr><td>1</td></tr></tbody></table>",
			"<table><thead><th>a</th></thead><tbody><tr><td>1</td></tr></tbody></table>", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeHTML(tc.in)
			if tc.wantKept != "" && !strings.Contains(got, tc.wantKept) {
				t.Errorf("kept markup missing:\n got %q\nwant substring %q", got, tc.wantKept)
			}
			if tc.wantGone != "" && strings.Contains(got, tc.wantGone) {
				t.Errorf("dangerous content survived: %q in %q", tc.wantGone, got)
			}
		})
	}
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Retries now back off with jitter": "retries-now-back-off-with-jitter",
		"  spaced  out  ":                  "spaced-out",
		"UPPER_snake.case":                 "upper-snake-case",
		"a/b/c: file.go (v2)":              "a-b-c-file-go-v2",
		"--leading and trailing--":         "leading-and-trailing",
		"":                                 "",
		"!!!":                              "",
		"numbers 123 stay":                 "numbers-123-stay",
	}
	for in, want := range cases {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

// A long title must yield a usable filename fragment, not a 400-character one.
func TestSlugifyCapsLength(t *testing.T) {
	got := Slugify(strings.Repeat("word ", 100))
	if len(got) > maxSlugLen {
		t.Errorf("slug length = %d, want <= %d", len(got), maxSlugLen)
	}
	if got == "" || strings.HasSuffix(got, "-") {
		t.Errorf("slug = %q, want a clean, non-empty fragment", got)
	}
}
