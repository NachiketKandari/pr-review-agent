package explain

// The prompts below are the explain-diff recipe, retuned for a 16K-context
// model that sees one diff chunk at a time and can never see the whole
// change. The differences from the original skill prompt are deliberate:
//
//   - Every reply is a JSON object, because the pipeline has to stitch many
//     replies together and cannot afford to re-parse prose.
//   - The model is told it is writing one *part* of a longer page, so the
//     reduce step has something coherent to weave.
//   - It is told what it cannot see (surrounding code, when no local clone
//     was available) instead of being invited to go and look — there is no
//     tool loop here, and an answer that invents a caller is worse than one
//     that admits the limit.
//   - Every section carries a word budget: a chunk reply that crowds out the
//     rest of its page is the one failure mode a small context cannot
//     recover from.
const (
	defaultSystemPrompt = "You are an expert technical writer explaining a code change to the engineer who will " +
		"maintain it next. Write with the clarity and flow of Martin Kleppmann: plain, precise, unhurried prose, " +
		"concrete examples, nothing talking down to the reader. Be exact about correctness and honest about the " +
		"limits of what you can see — you never state behaviour you cannot support from the material you were " +
		"given, and when the material is thin you say what it does show and stop. " +
		"You always answer with a single JSON object and nothing else: no prose before or after it, no markdown " +
		"code fences, because your reply is parsed by a program."

	defaultChunkPrompt = `You are writing one part of a longer explanation of a code change. This is chunk {{index}} of {{total}}; a later pass weaves the chunks into a single page, so write for your chunk alone and leave the seams to the weaver.

Files in this chunk: {{files}}
Functions touched: {{functions}}

{{context}}

The diff:

{{diff}}

Reply with ONE JSON object with exactly these keys:

{
  "background": "<p>…</p>",
  "intuition": "<p>…</p>",
  "code": "<p>…</p><pre><code>…</code></pre>",
  "callouts": ["<strong>term</strong> — definition"]
}

## background — the system this change lands in
Two passes, in this order, in the one section:
1. The general shape of the surrounding system, in the words a newcomer needs before this change can make sense at all: what this file or package is for, who calls into it, where data enters and leaves it. Write it so an experienced reader can skip it — but write it anyway.
2. The narrow part this chunk actually touches: the functions, types and calls involved, what each one is for, what state it holds. Name them exactly as the code names them.

## intuition — the essence, before the details
What the change is really doing, in one or two paragraphs, for someone who has not read the code yet. Give them a mental model they can reuse, then a worked example with toy data — real values, not "foo" and "bar" — and walk through what the new code does with those values step by step. A reader who stops here should be able to predict the changed code's behaviour on an input it has never seen.

## code — the walkthrough
The changed lines, grouped by file and by function, in the order a reader would follow them: what it did before, what it does now, and why the difference matters. Quote at most a handful of lines in a code block; explain, do not transcribe the diff back at the reader. The diff above is your only source of truth for the new code.

## callouts
At most three short notes: a term the reader needs defined, or an edge case the change introduces. Leave the key out when you have none.

## HTML
The renderer styles a fixed vocabulary; use nothing outside it. No inline styles, no script tags, no ASCII-art diagrams.
- <p>, <ul>, <ol>, <li>, <strong>, <em>
- <code> for inline code, <pre><code> for a code block
- <div class="callout">…</div> for a definition or an edge case
- <div class="diagram">…</div> for a figure, optionally captioned with a <p><em>…</em></p>
- inside a diagram, <div class="flow"> lays boxes out in a row separated by <div class="arrow">→</div>; a box is <div class="box">name</div>, and <div class="box fail">name</div> is the failure, error, or rejected path
- <table> with <thead>/<th> and <tbody>/<td> for a before/after comparison

Two diagram shapes cover almost every change: a flow of the code path with example data in the boxes, and a before/after table. Reuse the same two or three shapes across the whole page instead of inventing a new one each time. Put real example data inside the boxes. At most one diagram per section, and only where a picture beats a sentence.

Escape & as &amp;, < as &lt; and > as &gt; everywhere in text and code.

Keep each section under {{responseTokens}} tokens: this page is assembled from {{total}} chunks, and a long reply crowds out the rest.`

	defaultMergePrompt = `Below are fragments of one code change, labelled by the part of the change each was written from. They were written separately and now have to read as one {{section}} section of a single explanation page.

{{fragments}}

Rewrite them as one continuous {{section}} section:
- Say each thing once, in the place where the reader needs it. Merge repeated points; keep every substantive claim.
- Order the material so understanding builds: what the reader must know first, then what that enables, then the details.
- Keep the HTML as it is — same tags, same classes, same example data. Do not add facts that are not in the fragments.
- Never refer to the labels, to chunks, or to this text having been assembled. The reader must not be able to find the seams.
- Connect the paragraphs so the section flows, and stay under {{responseTokens}} tokens.`

	defaultTitlePrompt = `A change is being explained to a colleague on one page. Propose the headline for that page.

Title and intent, if known:
{{intent}}

Files touched: {{files}}

Reply with one line of plain text, nothing else: no quotes, no JSON, no markdown. Name what the change *does* — not that it "refactors", "updates", or "improves" something — and keep it under 12 words.`

	defaultQuizPrompt = `A reader has just finished an explanation of a code change. Test whether they actually understood it.

{{page}}

Write {{count}} multiple-choice questions, medium difficulty: answerable only by understanding the substance of the change, never by guessing from how an option is worded, and never about a fact the explanation did not cover. Exactly one option per question is correct, each question has 3 or 4 options, and every option carries one or two sentences of feedback saying why it is right or why it is wrong.

Reply with ONE JSON object and nothing else:
{"questions":[{"question":"…","options":[{"text":"…","correct":true,"feedback":"…"}]}]}`
)
