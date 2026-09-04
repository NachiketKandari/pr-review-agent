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
```

### Review mode

A first argument that parses as a GitHub PR URL switches to review mode
(Go `flag` parsing means flags come before the URL):

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

1. **`.patch` link token** (for private repos whose org blocks API tokens):
   GitHub puts a `?token=...` value on shareable `.diff`/`.patch` links of
   private pull requests. Set `github.diffToken` (or `-diff-token`, or the
   `GITHUB_DIFF_TOKEN` env var) to that value and the tool downloads
   `https://<host>/<owner>/<repo>/pull/N.patch?token=...` — one request
   that returns both the diff and the commit messages (parsed from the
   patch headers into the review context). No API token is needed.
2. **GitHub REST API** otherwise: read-only GET requests for the diff and
   PR metadata (Bearer auth when a token is available).
3. **`-diff file.diff`** reviews a unified diff or `.patch` file you
   exported yourself (see below) and never contacts GitHub.

The flow then continues: split the diff into chunks (greedy at file
boundaries, then hunks, then lines) → review each chunk with one capped
model call → merge findings, recursively when they overflow the context
budget → stream the final merged review to stdout. The PR title,
description, and commit messages (from the API or from the `.patch`
headers) are injected into every prompt as "Pull request intent" so the
review knows what the change is supposed to achieve.

**Read-only by design:** the API path only uses GET endpoints (`/pulls/N`
diff, `/pulls/N` metadata, `/pulls/N/commits`) and the `.patch` route is a
single download. The tool can never create, merge, comment on, or
otherwise modify anything.

### Reviewing when API tokens for private repos are blocked

If your org does not allow tokens with private repository access, use the
shareable link token: open the private PR in your browser, click the
`.patch` download, and copy the `token=...` value from the URL. Put it in
`local.yaml`:

```yaml
github:
  diffToken: aBcDeF...   # the ?token= value from a private .patch/.diff link
```

Alternatively, export the patch yourself (the browser is already logged
in) and review the file — no GitHub access happens at all:

```sh
# save https://github.iseccorp.in/ORG/REPO/pull/871.patch as pr-871.patch
pr-review-agent -diff pr-871.patch "https://github.iseccorp.in/ORG/REPO/pull/871"
```

The URL is optional with `-diff`; without it the tool reviews the file
with no GitHub context.

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
  chunkPrompt: ""               # default in code; placeholders: {{index}} {{total}} {{files}} {{diff}} {{responseTokens}}
  mergePrompt: ""               # default in code; placeholder: {{findings}}
  maxChunkTokens: 8000          # chunk and merge context budget
  maxResponseTokens: 2048
  temperature: 0.2
github:
  token: ""                     # optional; GITHUB_TOKEN env also honored
  diffToken: ""                 # optional; ?token= from a private .patch/.diff link
```

Overriding a prompt is safe: the code only warns if your custom `chunkPrompt`
lacks the `{{diff}}` placeholder (the diff would not be sent).

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
| `-output`      | empty        | write the merged review to this file (review mode)     |
| `-chunk-tokens`| config       | override `review.maxChunkTokens` (review mode)         |
| `-diff`        | empty        | review a local unified diff or .patch file instead of fetching (review mode) |
| `-diff-token`  | empty        | the ?token= value from a private .patch/.diff link (overrides github.diffToken) |
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
diff/              PR URL parsing, GitHub/GitHub Enterprise diff + metadata fetch, unified-diff parsing
chunk/             len/4 token estimation, greedy chunk building
review/            map-reduce review agent (prompts, chunk reviews, merges)
xlog/              slog setup (stderr + JSON file), URL/token redaction
```

## Roadmap

- [x] `go run . "pr-link"` to fetch a PR diff and review it
- [x] Chunked review of large diffs
- [x] GitHub Enterprise hosts and read-only PR intent context (title, description, commit messages)
- [ ] Jira integration: detect the ticket key in the PR title/branch, fetch the
      ticket summary/description, and feed the acceptance goal into the review
- [ ] Deep Go-aware review (go/ast symbol tables, call graphs) — the review
      package already exposes an optional `Enricher` seam so this drops in
      without orchestrator changes
