package config

import (
	"os"
	"path/filepath"
	"testing"
)

const sampleYAML = `name: Local Config
version: 1.0.0
schema: v1
models:
  - name: vLLM qwen3-VL-30B-A3B
    provider: openai
    model: /opt/vllm/models/qwen3-VL-30B-A3B-Instruct-AWQ
    apiBase: http://vllm.example.com:8002/v1
    apiKey: dummy
  - name: fallback
    provider: openai
    model: fallback-model
    apiBase: http://localhost:8080/v1/
    apiKey: other
    requestOptions:
      verifySsl: false
      timeout: 60
      proxy: http://proxy.internal:3128
      headers:
        X-Org: eng
`

func writeSample(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(sampleYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad(t *testing.T) {
	cfg, err := Load(writeSample(t))
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(cfg.Models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(cfg.Models))
	}

	first := cfg.Models[0]
	if first.APIBase != "http://vllm.example.com:8002/v1" {
		t.Errorf("apiBase = %q", first.APIBase)
	}
	if first.APIKey != "dummy" {
		t.Errorf("apiKey = %q", first.APIKey)
	}
	if first.Model != "/opt/vllm/models/qwen3-VL-30B-A3B-Instruct-AWQ" {
		t.Errorf("model = %q", first.Model)
	}

	second := cfg.Models[1]
	if second.RequestOptions == nil {
		t.Fatal("requestOptions not parsed")
	}
	if second.RequestOptions.VerifySSL == nil || *second.RequestOptions.VerifySSL {
		t.Errorf("verifySsl = %v", second.RequestOptions.VerifySSL)
	}
	if second.RequestOptions.Proxy != "http://proxy.internal:3128" {
		t.Errorf("proxy = %q", second.RequestOptions.Proxy)
	}
	if second.RequestOptions.Headers["X-Org"] != "eng" {
		t.Errorf("headers = %v", second.RequestOptions.Headers)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

// An old-style config without review/github sections must keep parsing
// exactly as before (backward compatibility).
func TestLoadBackwardCompatible(t *testing.T) {
	cfg, err := Load(writeSample(t))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Review.SystemPrompt != "" || cfg.Review.MaxChunkTokens != 0 {
		t.Errorf("review defaults leaked into old config: %+v", cfg.Review)
	}
	if cfg.Github.Token != "" {
		t.Errorf("github token unexpectedly set: %q", cfg.Github.Token)
	}
}

func TestLoadReviewAndGithubSections(t *testing.T) {
	yaml := `name: Review Config
version: 1.0.0
schema: v1
models:
  - name: deepseek
    provider: deepseek
    model: deepseek-v4-flash
    apiBase: https://api.deepseek.com/v1
    apiKey: sk-x
review:
  model: deepseek-v4-flash
  systemPrompt: |
    Be thorough.
  chunkPrompt: ""
  mergePrompt: ""
  maxChunkTokens: 4000
  maxResponseTokens: 1024
  temperature: 0.5
github:
  token: ghp_local
  diffToken: share_link_token_abc
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Review.Model != "deepseek-v4-flash" {
		t.Errorf("review.model = %q", cfg.Review.Model)
	}
	if cfg.Review.SystemPrompt != "Be thorough.\n" {
		t.Errorf("review.systemPrompt = %q", cfg.Review.SystemPrompt)
	}
	if cfg.Review.MaxChunkTokens != 4000 || cfg.Review.MaxResponseTokens != 1024 {
		t.Errorf("review token limits = %d/%d", cfg.Review.MaxChunkTokens, cfg.Review.MaxResponseTokens)
	}
	if cfg.Review.Temperature != 0.5 {
		t.Errorf("review.temperature = %v", cfg.Review.Temperature)
	}
	if cfg.Github.Token != "ghp_local" {
		t.Errorf("github.token = %q", cfg.Github.Token)
	}
	if cfg.Github.DiffToken != "share_link_token_abc" {
		t.Errorf("github.diffToken = %q", cfg.Github.DiffToken)
	}
}

func TestModelSelection(t *testing.T) {
	cfg, err := Load(writeSample(t))
	if err != nil {
		t.Fatal(err)
	}

	if got, err := cfg.Model(""); err != nil || got.Name != "vLLM qwen3-VL-30B-A3B" {
		t.Fatalf("default model = %+v, err = %v", got, err)
	}
	if got, err := cfg.Model("0"); err != nil || got.Model != "/opt/vllm/models/qwen3-VL-30B-A3B-Instruct-AWQ" {
		t.Fatalf("model 0 = %+v, err = %v", got, err)
	}
	if got, err := cfg.Model("1"); err != nil || got.Name != "fallback" {
		t.Fatalf("model 1 = %+v, err = %v", got, err)
	}
	if got, err := cfg.Model("fallback"); err != nil || got.APIKey != "other" {
		t.Fatalf("model by name = %+v, err = %v", got, err)
	}
	if _, err := cfg.Model("9"); err == nil {
		t.Fatal("expected error for out of range index")
	}
	if _, err := cfg.Model("missing"); err == nil {
		t.Fatal("expected error for unknown model name")
	}
}
