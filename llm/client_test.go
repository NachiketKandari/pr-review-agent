package llm

import (
	"testing"
	"time"
)

func TestEndpointResolution(t *testing.T) {
	cases := []struct {
		apiBase string
		path    string
		want    string
	}{
		{"http://vllm.example.com:8002/v1", "chat/completions", "http://vllm.example.com:8002/v1/chat/completions"},
		{"http://vllm.example.com:8002/v1/", "chat/completions", "http://vllm.example.com:8002/v1/chat/completions"},
		{"https://api.deepseek.com/v1", "chat/completions", "https://api.deepseek.com/v1/chat/completions"},
		{"https://api.deepseek.com", "models", "https://api.deepseek.com/models"},
	}
	for _, tc := range cases {
		c, err := New(Options{APIBase: tc.apiBase, Timeout: time.Second})
		if err != nil {
			t.Fatalf("New(%q): %v", tc.apiBase, err)
		}
		if got := c.endpoint(tc.path); got != tc.want {
			t.Errorf("endpoint(%q) with base %q = %q, want %q", tc.path, tc.apiBase, got, tc.want)
		}
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("expected error for empty apiBase")
	}
	if _, err := New(Options{APIBase: "ftp://example.com"}); err == nil {
		t.Fatal("expected error for unsupported scheme")
	}
	if _, err := New(Options{APIBase: "http://example.com", CABundlePath: "/does/not/exist"}); err == nil {
		t.Fatal("expected error for missing CA bundle")
	}
}
