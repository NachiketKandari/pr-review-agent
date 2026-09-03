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

The flow: parse the URL → fetch the unified diff from the GitHub API (Bearer
auth when a token is available) → split it into chunks (greedy at file
boundaries, then hunks, then lines) → review each chunk with one capped model
call → merge findings, recursively when they overflow the context budget →
stream the final merged review to stdout.

GitHub tokens resolve in this order: `github.token` in the config →
`GITHUB_TOKEN` env var → unauthenticated (public repos only, rate-limited).

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
```

Overriding a prompt is safe: the code only warns if your custom `chunkPrompt`
lacks the `{{diff}}` placeholder (the diff would not be sent).

Note for reasoning models (e.g. DeepSeek reasoner variants): `max_tokens`
counts hidden reasoning tokens, so a large share of `maxResponseTokens` can
be consumed before any visible output appears. Raise `maxResponseTokens`
(e.g. `6000`) if chunk reviews come back empty.

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
diff/              PR URL parsing, GitHub diff fetch, unified-diff parsing
chunk/             len/4 token estimation, greedy chunk building
review/            map-reduce review agent (prompts, chunk reviews, merges)
xlog/              slog setup (stderr + JSON file), URL/token redaction
```

## Roadmap

- [x] `go run . "pr-link"` to fetch a PR diff and review it
- [x] Chunked review of large diffs
- [ ] Deep Go-aware review (go/ast symbol tables, call graphs) — the review
      package already exposes an optional `Enricher` seam so this drops in
      without orchestrator changes
