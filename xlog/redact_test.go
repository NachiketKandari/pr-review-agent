package xlog

import "testing"

func TestRedactKey(t *testing.T) {
	secret := []string{
		"Authorization", "authorization", "api-key", "apiKey", "api_key",
		"X-API-Key", "GITHUB_TOKEN", "github_token", "GitHub-Token", "token",
		"clientSecret", "password", "api header",
	}
	for _, k := range secret {
		if !RedactKey(k) {
			t.Errorf("RedactKey(%q) = false, want true", k)
		}
	}
	safe := []string{"path", "repo", "owner", "X-RateLimit-Remaining", "model", "URL", "Content-Type", ""}
	for _, k := range safe {
		if RedactKey(k) {
			t.Errorf("RedactKey(%q) = true, want false", k)
		}
	}
}

func TestRedact(t *testing.T) {
	secrets := []string{
		"Bearer abc.def.ghi",
		"bearer abc",
		"Basic dXNlcjpwYXNz",
		"sk-9f8a7b6c5d4e3f2a1b0c",
		"ghp_1234567890abcdefghij",
		"github_pat_11ABCDEF1234567890",
		"gho_xyz",
		"xoxb-123456",
	}
	for _, v := range secrets {
		if Redact(v) != Redacted {
			t.Errorf("Redact(%q) = %q, want %q", v, Redact(v), Redacted)
		}
	}
	ok := []string{"deepseek-v4-flash", "main.go", "octocat", ""}
	for _, v := range ok {
		if Redact(v) != v {
			t.Errorf("Redact(%q) changed a safe value", v)
		}
	}
}

func TestRedactValue(t *testing.T) {
	if got := RedactValue("Authorization", "Bearer supersecret"); got != Redacted {
		t.Errorf("Authorization header not redacted: %q", got)
	}
	if got := RedactValue("GITHUB_TOKEN", "ghp_abc"); got != Redacted {
		t.Errorf("GITHUB_TOKEN not redacted: %q", got)
	}
	if got := RedactValue("some-odd-key", "ghp_notsecret"); got != Redacted {
		t.Errorf("token-looking value under safe key not redacted: %q", got)
	}
	if got := RedactValue("path", "src/main.go"); got != "src/main.go" {
		t.Errorf("safe value mangled: %q", got)
	}
	if got := RedactValue("path", ""); got != "" {
		t.Errorf("empty value mangled: %q", got)
	}
}

func TestSafeURL(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"https://api.github.com/repos/octo/hello/pulls/12?access_token=abc&x=1",
			"https://api.github.com/repos/octo/hello/pulls/12"},
		{"https://api.deepseek.com/v1/chat/completions?api_key=sk-abc#frag",
			"https://api.deepseek.com/v1/chat/completions"},
		{"https://user:pass@example.com/path",
			"https://example.com/path"},
		{"https://github.com/octo/hello/pull/12", "https://github.com/octo/hello/pull/12"},
		{"", ""},
		{":// not a url", ":// not a url"},
	}
	for _, tc := range cases {
		if got := SafeURL(tc.in); got != tc.want {
			t.Errorf("SafeURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
