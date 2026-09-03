# pr-review-agent

CLI tool that talks to OpenAI-compatible chat models (vLLM, DeepSeek, OpenAI, LM Studio, etc.) using a Continue-style `config.yaml`.

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

```sh
go run . "Hello, who are you?"                 # streams the response
go run . -no-stream "Hello"                    # plain, non-streamed response
go run . -model qwen "Hello"                   # pick model by name, substring, or index
go run . -config config.yaml "Hello"           # use a different config file
```

Flags:

| Flag        | Default       | Description                                      |
|-------------|---------------|--------------------------------------------------|
| `-config`   | `local.yaml`  | path to config file                              |
| `-model`    | first model   | model name, substring, or index                  |
| `-no-stream`| `false`       | wait for the full response instead of streaming  |
| `-insecure` | `false`       | skip TLS certificate verification                |
| `-ca`       | empty         | path to a CA bundle file                         |
| `-timeout`  | config or 2h  | request timeout, e.g. `2m`                       |

## Layout

```
main.go            CLI entry point
config/            Continue-style yaml parsing and model selection
llm/               OpenAI-compatible HTTP client (chat, SSE streaming, models)
```

## Roadmap

- `go run . "pr-link"` to fetch a PR diff and review it
- Chunked review of large diffs
- Deep Go-aware review using go/ast for Go repositories
