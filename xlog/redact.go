package xlog

import (
	"net/url"
	"strings"
)

// Redacted is the placeholder substituted for secret values.
const Redacted = "[REDACTED]"

// secretKey is a normalized (lowercase, no '-', '_' or ' ') set of keys
// whose values must never be logged.
var secretKey = map[string]bool{
	"authorization": true,
	"apikey":        true,
	"api_key":       true,
	"xapikey":       true,
	"apiheader":     true, // Azure: api-key header
	"token":         true,
	"accesstoken":   true,
	"githubtoken":   true,
	"clientsecret":  true,
	"password":      true,
	"secret":        true,
}

// redactedPrefix lists value shapes that always look like credentials,
// regardless of the key they were stored under.
var redactedPrefix = []string{
	"bearer ",
	"basic ",
	"sk-",
	"sk_",
	"ghp_",
	"gho_",
	"ghu_",
	"ghs_",
	"ghr_",
	"github_pat_",
	"xoxb-",
	"xoxp-",
}

// RedactKey reports whether a log key (header name, config field, env var)
// should never have its value logged. Unknown keys are treated as safe; use
// [Redact] for the value shape itself.
func RedactKey(key string) bool {
	if key == "" {
		return false
	}
	return secretKey[normalizeKey(key)]
}

// Redact returns v unchanged unless v looks like a credential (bearer/basic
// credentials, well-known API token prefixes), in which case it returns
// [Redacted]. Empty values are left alone.
func Redact(v string) string {
	if v == "" {
		return ""
	}
	lower := strings.ToLower(strings.TrimSpace(v))
	for _, p := range redactedPrefix {
		if strings.HasPrefix(lower, p) {
			return Redacted
		}
	}
	return v
}

// RedactValue is like [Redact] but also consults the key: values under
// secret-looking keys are always redacted even if they do not match a known
// prefix.
func RedactValue(key, value string) string {
	if value == "" {
		return ""
	}
	if RedactKey(key) {
		return Redacted
	}
	return Redact(value)
}

// SafeURL strips anything sensitive from a URL for logging: query
// parameters, fragments, and userinfo. On malformed input the raw string is
// returned unchanged rather than dropping the log line.
func SafeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.RawQuery = ""
	u.Fragment = ""
	u.User = nil
	if s := u.String(); s != "" {
		return s
	}
	return raw
}

func normalizeKey(key string) string {
	lower := strings.ToLower(key)
	return strings.Map(func(r rune) rune {
		switch r {
		case '-', '_', ' ', '.':
			return -1
		}
		return r
	}, lower)
}
