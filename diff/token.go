package diff

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/NachiketKandari/pr-review-agent/xlog"
)

// externalCommand runs an external command with optional stdin and returns
// trimmed stdout. It is a package variable so tests can stub it.
var externalCommand = func(ctx context.Context, name string, args []string, stdin string, extraEnv ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// ResolveToken resolves the GitHub token for host, trying, in order:
//
//  1. the config-supplied token (cfgToken),
//  2. the GITHUB_TOKEN environment variable,
//  3. the account the user is logged into on this machine for that host:
//     the gh CLI first, then the git credential helper (e.g. Git
//     Credential Manager on Windows, which stores the login used in a
//     browser or by git itself),
//  4. nothing (unauthenticated).
//
// The chosen source is logged; the token itself is never logged.
func ResolveToken(cfgToken, host string) string {
	if host == "" {
		host = "github.com"
	}
	if cfgToken != "" {
		xlog.Info("github token: using config value")
		return cfgToken
	}
	if env := os.Getenv("GITHUB_TOKEN"); env != "" {
		xlog.Info("github token: using GITHUB_TOKEN environment variable")
		return env
	}
	if t := tokenFromGHCLI(host); t != "" {
		xlog.Info("github token: using gh CLI account", "host", host)
		return t
	}
	if t := tokenFromGitCredentials(host); t != "" {
		xlog.Info("github token: using git credential helper", "host", host)
		return t
	}
	xlog.Warn("no github token anywhere (config, GITHUB_TOKEN, gh CLI, git credentials): unauthenticated fetch (public repositories only, lower rate limits)")
	return ""
}

// tokenFromGHCLI returns the token of the account the user is logged in as
// for host via the gh CLI ("gh auth token -h <host>").
func tokenFromGHCLI(host string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := externalCommand(ctx, "gh", []string{"auth", "token", "-h", host}, "")
	if err != nil {
		xlog.Debug("gh CLI unavailable or not logged in", "host", host, "error", err)
		return ""
	}
	return sanitizeToken(out)
}

// tokenFromGitCredentials asks the git credential helper (Git Credential
// Manager, macOS keychain, ...) for credentials stored for host. Prompts
// are suppressed so a missing entry simply returns nothing.
func tokenFromGitCredentials(host string) string {
	input := fmt.Sprintf("protocol=https\nhost=%s\n\n", host)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := externalCommand(ctx, "git",
		[]string{"credential", "fill"},
		input,
		"GCM_INTERACTIVE=never", "GIT_TERMINAL_PROMPT=0")
	if err != nil {
		xlog.Debug("git credential helper unavailable", "host", host, "error", err)
		return ""
	}
	token := ""
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(line, "password="); ok {
			token = v
		}
	}
	return sanitizeToken(token)
}

// sanitizeToken keeps only plausible token-shaped values so stray helper
// output can never be used (or logged) as a credential.
func sanitizeToken(t string) string {
	t = strings.TrimSpace(t)
	if len(t) < 6 {
		return ""
	}
	lower := strings.ToLower(t)
	if strings.HasPrefix(lower, "bearer ") || strings.HasPrefix(lower, "basic ") || strings.ContainsAny(t, " \t\n") {
		return ""
	}
	return t
}
