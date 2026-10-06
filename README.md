# pr-review-agent

CLI tool that talks to OpenAI-compatible chat models (vLLM, DeepSeek, OpenAI, LM Studio, etc.) using a Continue-style `config.yaml` — and reviews GitHub pull requests with a chunked, map-reduce flow.

## Requirements

- Go 1.26+

## Setup

```sh
git clone https://github.com/NachiketKandari/pr-review-agent.git
cd pr-review-agent

cp config.example.yaml local.yaml
```

Edit `local.yaml` and fill in your model details:

```yaml
name: Local Config
version: 1.0.0
schema: v1
models:
  - name: DeepSeek V4 Flash
    provider: deepseek
    model: deepseek-v4-flash
    apiBase: https://api.deepseek.com/v1
    apiKey: sk-...
```

`local.yaml` and `config.yaml` are gitignored, so your keys stay local.

Optional per-model settings:

```yaml
    requestOptions:
      verifySsl: false          # skip TLS verification
      caBundlePath: /path/ca.pem
      timeout: 60               # seconds
      proxy: http://proxy:3128
      headers:
        X-Custom: value
```

## Usage

### Chat mode

```sh
go run . "Hello, who are you?"                 # streams the response
go run . -no-stream "Hello"                    # plain, non-streamed response
go run . -model qwen "Hello"                   # pick model by name, substring, or index
go run . -config config.yaml "Hello"           # use a different config file
go run . -prompt-file prompt.txt               # read the message from a text file
```

`-prompt-file` points at a text file whose content is used as the message —
handy for long or multiline prompts that are awkward to quote on the command
line. Surrounding whitespace in the file is trimmed; the file must not be
empty. If both a message and `-prompt-file` are given, the file wins (a
warning is logged).

### Explain mode

`-explain` is a separate mode, not a review style. It fetches the change the
same way a review does — every source below works — and instead of listing
findings it writes a self-contained interactive HTML page that explains the
change:

```sh
go run . -explain "feature/auth" "main"          # explain a branch merge
go run . -explain "octocat/Hello-World/pull/123" # explain a pull request
go run . -explain -open "main"                   # ...and open it in a browser
go run . -explain -output auth.html "fix/login" "main"
```

The page has four parts, mirroring the
[explain-diff](https://gist.github.com/ankitg12/8e808d387799de4e9839bc393f8e6405)
skill: **Background** (the existing system, from a beginner's view and then
narrowed to the change), **Intuition** (the essence, with a worked toy
example and diagrams that carry example data), a **code walkthrough** grouped
by file and function, and a **quiz** of five multiple-choice questions. Click
an option and the page says whether it was right and why — every option has
its own explanation, not just the correct one. The page is one file: no
external CSS, JavaScript, or fonts, so it can be mailed or attached to a PR.

Output goes to `YYYY-MM-DD-<slug>.html` in the current directory, overridable
with `-output`. The content spec it was rendered from is saved beside it as
`.spec.json`, so the prose can be edited by hand and re-rendered without
another model call:

```sh
go run . -render 2026-10-01-retries-back-off.spec.json -output review.html
```

`-render` needs no config, no model, and no network.

#### How it fits a 16K context

The skill relies on an agent that can read the repository and hold the whole
change at once. Neither is available to a CLI with a 16K window, so the same
recipe becomes the map-reduce flow review already uses:

- **One capped call per chunk.** The diff is split by the same function-aware
  chunker as review. Each chunk gets one reply, and the reply is a JSON object
  (`background`, `intuition`, `code`, `callouts`) — never prose, because the
  pipeline has to stitch many replies together and cannot afford to re-parse
  it.
- **Per-section weaving.** Fragments are reduced into one section at a time,
  recursively while they overflow the budget, so a large change never asks for
  more context than the window holds. Budgets default to 7000 in / 3000 out,
  leaving room for the system prompt, the PR intent, and the source context.
- **Surrounding code is read mechanically, not requested.** The skill tells its
  agent to "broadly explore the surrounding code". With no tool loop, the
  explainer instead reads the *pre-image* of the changed files from your local
  clone (`git show` at the merge base), budget-capped and line-truncated, and
  tells the model when a file was cut short. With no clone — a `.patch` file
  or the API route — it says the surrounding source is unavailable, and the
  model is instructed to describe only what the diff shows rather than invent
  a caller.
- **Failures degrade instead of aborting.** Malformed JSON is repaired once
  (code fences, surrounding prose, trailing commas); a chunk that still fails
  is dropped with a warning; a quiz that cannot be generated simply is not on
  the page. A quiz question without exactly one correct option is dropped
  rather than shown ungradable.

Model-authored markup is sanitized before it reaches the page: script-like
elements and inline event handlers are stripped, so a diff that happens to
contain one (a test fixture, a doc example) cannot turn the explanation into
an active page. Everything else — titles, questions, options, feedback — is
escaped.

#### Optional explain config

`explain:` is entirely separate from `review:`; the two share no prompts or
budgets, and enabling `-explain` never changes a review run.

```yaml
explain:
  model: deepseek-v4-flash      # optional; else -model, then review.model
  systemPrompt: ""              # "" = built-in default
  chunkPrompt: ""               # one chunk of the diff
  mergePrompt: ""               # weaves one section's fragments
  quizPrompt: ""                # the closing questions
  titlePrompt: ""               # the page headline
  maxChunkTokens: 7000          # input budget per call (16K-tuned)
  maxResponseTokens: 3000
  maxQuizTokens: 2000
  temperature: 0.4              # prose, not analysis
  sourceMaxFiles: 6             # pre-image files to read
  sourceMaxTokens: 3000         # total pre-image budget
```

`-prompt-file` overrides `explain.systemPrompt` in explain mode, exactly as it
overrides `review.systemPrompt` in review mode.

### Review mode

#### Branch review (primary form)

Give the source branch (the one being merged) and the target branch (the
one being merged into). The tool works inside a local git clone — run it
from the repository, or point `-repo` at one — fetches both branches from
origin with git itself (your existing SSH key / credential manager
authenticates; no API token and no PR link involved), then diffs
merge-base(target)...source, the same three-dot shape as a GitHub PR diff:

```sh
go run . "feature/auth" "main"               # review feature/auth into main
go run . "main"                             # review the current branch into main
go run . -repo /path/to/clone "fix" "release/v2"
go run . -output review.md "my-branch" "main"  # also save to file
```

The single-branch form reviews the branch you have checked out into the
target branch — the common case when you are sitting on your feature branch
and want it reviewed against `main`. The source branch is resolved with
`git branch --show-current` (falling back to `git rev-parse`), and the
argument must be a real ref, so a lone chat message is never misread as a
branch review (outside a git clone it stays a chat).

Branch resolution prefers the freshly fetched `origin/<branch>` (what a PR
would actually merge) and falls back to the local branch, so branches that
exist only locally or only remotely both work. Commit messages from
`target..source` are added to the review context.

#### PR review (fallback)

A first argument that parses as a GitHub PR URL still switches to PR review
mode (Go `flag` parsing means flags come before the URL):

```sh
go run . "https://github.com/octocat/Hello-World/pull/123"
go run . "github.com/octocat/Hello-World/pull/123"        # scheme-less
go run . "octocat/Hello-World/pull/123"                    # shortened
go run . -output review.md "octocat/Hello-World/pull/123"  # also save to file
```

GitHub Enterprise works the same way — paste any Enterprise PR URL and the
tool talks to that host's API (`https://<host>/api/v3`):

```sh
go run . "https://github.iseccorp.in/team/proj/pull/12"
```

Because the review model and the GitHub client share one HTTP setup, the
`-ca`, `-insecure`, and `requestOptions.proxy`/`caBundlePath` settings apply
to Enterprise TLS as well (corporate proxies/private CAs).

The diff is obtained in this order of preference:

1. **Local git clone** (works without any token): run the tool from inside
   a clone of the repository, or point `-repo` at one. The tool fetches
   `refs/pull/N/head` and the default branch with git itself — using your
   existing git credentials (SSH key, Git Credential Manager) — then diffs
   `merge-base(base)...head` and reads the commit messages from `git log`.
   This is the route to use when your org blocks API tokens for private
   repositories. Works for open PRs; merged/closed PRs yield an empty diff
   (their commits are already in the base branch), in which case the tool
   falls through to the next route.
2. **`-diff file`** reviews a unified diff or `.patch` file you exported
   yourself (e.g. saved from the browser) and never contacts GitHub.
3. **`.patch` link token**: set `github.diffToken` (or `-diff-token`, or
   `GITHUB_DIFF_TOKEN`) to the `?token=...` value GitHub puts on
   shareable `.patch`/`.diff` links of private PRs; the patch is downloaded
   from the web endpoint, which also yields the commit messages.
4. **GitHub REST API** otherwise: read-only GET requests for the diff and
   PR metadata (Bearer auth when a token is available).

The flow then continues: split the diff into chunks (greedy at file
boundaries; oversized files split at function boundaries — consecutive
`@@` hunks git attributes to the same function — then at hunk boundaries,
then at lines as a last resort) → review each chunk with one capped model
call → weave each file's chunk findings into one per-file review
(preserving cross-function context inside the file) → merge the file
reviews, recursively when they overflow the context budget → stream the
final merged review to stdout. Chunk and response budgets default to
10000/4000, tuned for a 16K-context model (~10K input + 4K response + ~2K
headroom for the system prompt and PR intent). The PR title,
description, and commit messages are injected into every prompt as "Pull
request intent" so the review knows what the change is supposed to achieve.

**Read-only by design:** the API path only uses GET endpoints (`/pulls/N`
diff, `/pulls/N` metadata, `/pulls/N/commits`) and the `.patch` route is a
single download. The tool can never create, merge, comment on, or
otherwise modify anything.

### Reviewing when API tokens for private repos are blocked

If your org does not allow tokens with private repository access, use the
git route: make sure you have a clone of the repository (your normal git
login — SSH key or Git Credential Manager — must be able to pull it), then
run the tool from inside that clone, or from anywhere with
`-repo C:\path\to\the\clone`:

```bat
cd C:\path\to\mutual-fund-go-be
git pull
pr-review-agent.exe "https://github.iseccorp.in/ORG/REPO/pull/871"
rem or, from anywhere:
pr-review-agent.exe -repo C:\path\to\mutual-fund-go-be "https://github.iseccorp.in/ORG/REPO/pull/871"
```

The PR must be open (review before merge); merged PRs need a token or a
manual export. Alternatively export the patch yourself (the browser is
already logged in) and review the file — no GitHub access happens at all:

```sh
# save https://github.iseccorp.in/ORG/REPO/pull/871.patch as pr-871.patch
pr-review-agent -diff pr-871.patch "https://github.iseccorp.in/ORG/REPO/pull/871"
```

The URL is optional with `-diff`; without it the tool reviews the file
with no GitHub context.

#### If you do have a token

When you have an API token with private repo access, nothing else is
needed — set it and the tool uses the REST API route automatically:

```yaml
github:
  token: ghp_xxxx...   # API token (config > GITHUB_TOKEN env > gh CLI account > git credentials)
```

If you have a shareable link token instead (the `?token=...` value on a
private `.patch`/`.diff` link), set `diffToken`:

```yaml
github:
  diffToken: aBcDeF...   # the ?token= value from a private .patch/.diff link
```

GitHub API tokens resolve in this order:

1. `github.token` in the config,
2. the `GITHUB_TOKEN` environment variable,
3. the account you are logged in as on this machine for that host — the
   `gh` CLI (`gh auth token -h <host>`) and then the git credential helper
   (Git Credential Manager on Windows, the macOS keychain, ...), which
   reuse the login you already have, e.g. after signing in once via
   `gh auth login --hostname github.iseccorp.in --web` or storing a token
   in Git Credential Manager,
4. unauthenticated (public repos only, rate-limited).

No explicit token in config or env is needed when step 3 succeeds. Tokens
are read at runtime and never logged.

### Optional review config

```yaml
review:
  model: deepseek-v4-flash      # optional override (else -model flag, else first model)
  systemPrompt: |
    You are a senior code reviewer. Focus on correctness, security, ...
  chunkPrompt: ""               # default in code; placeholders: {{index}} {{total}} {{files}} {{functions}} {{diff}} {{responseTokens}}
  filePrompt: ""                # default in code; weaves one file's chunk findings; placeholders: {{file}} {{findings}}
  mergePrompt: ""               # default in code; placeholder: {{findings}}
  maxChunkTokens: 10000         # chunk and merge context budget (16K-tuned)
  maxResponseTokens: 4000
  temperature: 0.2
github:
  token: ""                     # optional; GITHUB_TOKEN env also honored
  diffToken: ""                 # optional; ?token= from a private .patch/.diff link
```

Overriding a prompt is safe: the code only warns if your custom `chunkPrompt`
lacks the `{{diff}}` placeholder (the diff would not be sent).

You can also override the review instructions for a single run with
`-prompt-file` — its content replaces `review.systemPrompt` (the chunk and
merge prompts come from the config as usual), which is useful for one-off
review styles without editing `local.yaml`:

```sh
go run . -prompt-file review-instructions.txt "feature/auth" "main"
go run . -prompt-file review-instructions.txt "octocat/Hello-World/pull/123"
```

Note for reasoning models (e.g. DeepSeek reasoner variants): `max_tokens`
counts hidden reasoning tokens, so a large share of `maxResponseTokens` can
be consumed before any visible output appears. Two mitigations: raise
`maxResponseTokens` (e.g. `6000`), and instruct the model to answer
immediately — add "Do NOT think at length - give your answer immediately."
to `review.systemPrompt`, which reliably stops empty responses.

### Flags

| Flag           | Default      | Description                                            |
|----------------|--------------|--------------------------------------------------------|
| `-config`      | `local.yaml` | path to config file                                    |
| `-model`       | first model  | model name, substring, or index (chat) / review.model override (review) |
| `-no-stream`   | `false`      | wait for the full response instead of streaming        |
| `-insecure`    | `false`      | skip TLS certificate verification                      |
| `-ca`          | empty        | path to a CA bundle file                               |
| `-timeout`     | config or 2h | request timeout, e.g. `2m`                             |
| `-output`      | empty        | write the merged review to this file (review mode) / the page to this path (explain mode) |
| `-chunk-tokens`| config       | override the per-call context budget (`review.maxChunkTokens` / `explain.maxChunkTokens`) |
| `-explain`     | `false`      | write a rich interactive HTML page explaining the change instead of reviewing it |
| `-open`        | `false`      | open the generated page in a browser (explain mode)    |
| `-render`      | empty        | re-render a saved `.spec.json` to HTML; no config, model, or network |
| `-diff`        | empty        | review a local unified diff or .patch file instead of fetching (review mode) |
| `-diff-token`  | empty        | the ?token= value from a private .patch/.diff link (overrides github.diffToken) |
| `-repo`        | current dir  | path to a local clone of the repo to diff with git (falls back to API) |
| `-prompt-file` | empty        | read the prompt from this text file (chat: the message; review: overrides `review.systemPrompt`) |
| `-log-file`    | empty        | append structured JSON logs to this file               |
| `-debug`       | `false`      | Info level + HTTP-level detail (sanitized URLs, status codes, durations) |
| `-quiet`       | `false`      | errors and warnings only                               |

## Logging

All logs go to **stderr** (and optionally to `-log-file` as append-mode JSON);
stdout stays clean for streamed chat/review output. Levels: `Info` by default,
`-quiet` suppresses to errors/warnings, `-debug` adds HTTP-level records.

Redaction is a hard requirement and is enforced by helpers in `xlog`
(`SafeURL`, `Redact`, `RedactValue`), covered by unit tests: API keys,
`Authorization` headers, and `GITHUB_TOKEN` values are never logged, and
logged URLs are stripped of query parameters, fragments, and userinfo. The
GitHub token and LLM API keys are only ever read from config/env, never
logged.

Logged lifecycle (review mode): config load + model selection → PR URL parse →
fetch (repo, PR number, diff size, duration, rate-limit headers) → chunking
decisions (per-chunk token estimates, file/hunk splits) → each chunk review
start/complete/duration → merge calls → `-output` write → final success. Every
fatal path logs a structured error with package/PR/chunk context.

## Layout

```
main.go            CLI entry point: chat + review mode, flag wiring
config/            Continue-style yaml parsing and model selection
llm/               OpenAI-compatible HTTP client (chat, SSE streaming, models)
diff/              PR URL parsing, branch review (local git diff), GitHub/GitHub Enterprise diff + metadata fetch, unified-diff parsing, pre-image source context
chunk/             len/4 token estimation, greedy chunk building (file → function → hunk → line)
review/            map-reduce review agent (prompts, chunk reviews, per-file weave, merges)
explain/           explain mode: 16K map-reduce page writer, JSON repair, HTML renderer + CSS/JS
xlog/              slog setup (stderr + JSON file), URL/token redaction
```

## Roadmap

- [x] `go run . "source_branch" "target_branch"` to review a branch merge from a local clone (no token needed)
- [x] `go run . "target_branch"` to review the current branch into the target branch (inside a git clone)
- [x] `go run . "pr-link"` to fetch a PR diff and review it
- [x] `-explain` to write a rich interactive HTML explanation of a change
      (background, intuition, code walkthrough, auto-graded quiz), chunked and
      budgeted for a 16K context, with the content spec saved for free
      re-rendering
- [x] Chunked review of large diffs (function-aware chunking + per-file weave)
- [x] GitHub Enterprise hosts and read-only PR intent context (title, description, commit messages)
- [ ] Jira integration: detect the ticket key in the PR title/branch, fetch the
      ticket summary/description, and feed the acceptance goal into the review
- [ ] Deep Go-aware review (go/ast symbol tables, call graphs) — the review
      package already exposes an optional `Enricher` seam so this drops in
      without orchestrator changes
