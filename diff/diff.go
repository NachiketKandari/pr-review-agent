// Package diff handles GitHub pull-request references: URL parsing, diff
// fetching from the GitHub REST API, and unified-diff parsing.
package diff

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/NachiketKandari/pr-review-agent/xlog"
)

const (
	userAgent   = "pr-review-agent/1.0"
	diffAccept  = "application/vnd.github.v3.diff"
	diffMaxBody = 64 << 20 // 64 MiB safety cap on diff bodies
)

// Ref identifies a GitHub pull request.
type Ref struct {
	Owner  string
	Repo   string
	Number int
}

// String returns a compact, log-friendly "owner/repo#N" form.
func (r Ref) String() string { return fmt.Sprintf("%s/%s#%d", r.Owner, r.Repo, r.Number) }

// GitHubURL returns the canonical pull-request page URL.
func (r Ref) GitHubURL() string {
	return fmt.Sprintf("https://github.com/%s/%s/pull/%d", r.Owner, r.Repo, r.Number)
}

func (r Ref) apiURL() string {
	return fmt.Sprintf("https://api.github.com/repos/%s/%s/pulls/%d",
		url.PathEscape(r.Owner), url.PathEscape(r.Repo), r.Number)
}

// ParseURL parses a GitHub pull-request reference from a full URL
// (https://github.com/owner/repo/pull/N), a scheme-less URL
// (github.com/owner/repo/pull/N), or a shortened form
// (owner/repo/pull/N). Anything after the PR number (e.g. "/files",
// "/commits", a fragment) is ignored. Non-GitHub URLs produce a clear
// error.
func ParseURL(raw string) (Ref, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return Ref{}, fmt.Errorf("empty pull request URL")
	}

	explicitHost := false
	var rest string
	switch {
	case strings.HasPrefix(s, "https://"):
		explicitHost = true
		rest = strings.TrimPrefix(s, "https://")
	case strings.HasPrefix(s, "http://"):
		return Ref{}, fmt.Errorf("not a GitHub URL (only https://github.com/owner/repo/pull/N is supported): %q", raw)
	case strings.HasPrefix(s, "www.github.com/"):
		explicitHost = true
		rest = strings.TrimPrefix(s, "www.")
	case strings.HasPrefix(s, "github.com/"):
		explicitHost = true
		rest = s
	default:
		rest = s // shortened form: owner/repo/pull/N
	}
	rest = strings.TrimSuffix(rest, "/")

	if explicitHost {
		host, remainder, _ := strings.Cut(rest, "/")
		if !strings.EqualFold(host, "github.com") {
			return Ref{}, fmt.Errorf("not a GitHub URL (host %q is not github.com): %q", host, raw)
		}
		rest = remainder
	}

	// Drop query strings and fragments (e.g. "?diff=split",
	// "#issuecomment-1"); they never carry PR identity.
	if i := strings.IndexAny(rest, "?#"); i >= 0 {
		rest = rest[:i]
	}
	rest = strings.TrimSuffix(rest, "/")

	segs := strings.Split(rest, "/")
	pullIdx := -1
	for i, seg := range segs {
		if seg == "pull" {
			pullIdx = i
			break
		}
	}
	if pullIdx < 2 || pullIdx+1 >= len(segs) {
		return Ref{}, fmt.Errorf("not a GitHub pull request URL (expected .../owner/repo/pull/N): %q", raw)
	}

	owner, repo := segs[pullIdx-2], segs[pullIdx-1]
	if owner == "" || repo == "" {
		return Ref{}, fmt.Errorf("not a GitHub pull request URL (expected .../owner/repo/pull/N): %q", raw)
	}
	num, err := strconv.Atoi(segs[pullIdx+1])
	if err != nil || num <= 0 {
		return Ref{}, fmt.Errorf("invalid pull request number %q in %q", segs[pullIdx+1], raw)
	}

	ref := Ref{Owner: owner, Repo: repo, Number: num}
	xlog.Info("parsed PR URL", "pr", ref.String(), "owner", ref.Owner, "repo", ref.Repo, "number", ref.Number)
	return ref, nil
}

// ResolveToken resolves the GitHub token: the config-supplied token wins,
// then the GITHUB_TOKEN environment variable, then unauthenticated access.
// The chosen source is logged; the token itself is never logged.
func ResolveToken(cfgToken string) string {
	if cfgToken != "" {
		xlog.Info("github token: using config value")
		return cfgToken
	}
	if env := os.Getenv("GITHUB_TOKEN"); env != "" {
		xlog.Info("github token: using GITHUB_TOKEN environment variable")
		return env
	}
	xlog.Warn("no github token: unauthenticated fetch (public repositories only, lower rate limits)")
	return ""
}

// Fetch retrieves the unified diff of ref from the GitHub REST API. When
// token is non-empty it is sent as a Bearer credential; otherwise the
// request is unauthenticated. Errors are mapped to friendly messages for
// 404 (not found / private), 403 (rate limit), 401 (bad credentials), and
// 422 (PR too large to diff, common on big PRs).
func Fetch(ctx context.Context, ref Ref, token string) ([]byte, error) {
	return fetch(ctx, ref.apiURL(), ref, token)
}

func fetch(ctx context.Context, apiURL string, ref Ref, token string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", diffAccept)
	req.Header.Set("User-Agent", userAgent)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", ref.String(), err)
	}
	defer resp.Body.Close()
	durationMs := time.Since(start).Milliseconds()

	rlRemaining, rlLimit, rlReset := rateLimit(resp)

	if resp.StatusCode != http.StatusOK {
		msg := apiErrorMessage(resp)
		switch resp.StatusCode {
		case http.StatusNotFound:
			return nil, fmt.Errorf("PR %s not found or the repository is private / does not exist", ref.String())
		case http.StatusForbidden:
			if rlRemaining == 0 {
				return nil, fmt.Errorf("GitHub API rate limit exceeded for %s (limit %d, resets %s); set github.token or GITHUB_TOKEN for higher limits",
					ref.String(), rlLimit, rlReset.Format(time.RFC3339))
			}
			return nil, fmt.Errorf("GitHub API rejected the request for %s: %s", ref.String(), msg)
		case http.StatusUnauthorized:
			return nil, fmt.Errorf("GitHub API authentication failed for %s: check github.token / GITHUB_TOKEN", ref.String())
		case http.StatusUnprocessableEntity:
			return nil, fmt.Errorf("GitHub could not produce a diff for %s (the PR may be too large): %s", ref.String(), msg)
		default:
			return nil, fmt.Errorf("GitHub API returned HTTP %d for %s: %s", resp.StatusCode, ref.String(), msg)
		}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, diffMaxBody))
	if err != nil {
		return nil, fmt.Errorf("read diff body for %s: %w", ref.String(), err)
	}

	xlog.Info("fetched PR diff",
		"pr", ref.String(),
		"diff_bytes", len(body),
		"duration_ms", durationMs,
		"rate_limit_remaining", rlRemaining,
		"rate_limit_limit", rlLimit,
		"rate_limit_reset", rlReset.Format(time.RFC3339),
	)
	return body, nil
}

// File is one file touched by a pull request, with its parsed text ready
// for chunking.
type File struct {
	Path      string
	Additions int
	Deletions int
	Binary    bool
	Text      string
}

// ParseDiff splits a unified diff into per-file sections. Each section is
// kept verbatim (from its "diff --git" line onward) so chunk boundaries
// never cut across files. Binary files are reported as skipped; file paths
// and +/- line counts are parsed from the section headers.
func ParseDiff(data []byte) []File {
	if len(data) == 0 {
		return nil
	}
	raw := string(data)

	var files []File
	sections := strings.Split(raw, "\ndiff --git ")
	for i, sec := range sections {
		if i == 0 {
			// Content before the first "diff --git" (e.g. a commit header)
			// is not a file section. With the v3 diff media type there is
			// none; tolerate it either way.
			if !strings.HasPrefix(strings.TrimSpace(sec), "diff --git") {
				if strings.TrimSpace(sec) != "" {
					xlog.Debug("ignoring pre-diff text", "bytes", len(sec))
				}
				continue
			}
		} else {
			sec = "diff --git " + sec
		}
		if f := parseSection(sec); f.Path != "" {
			if f.Binary {
				xlog.Debug("skipping binary file", "file", f.Path)
				continue
			}
			files = append(files, f)
		}
	}
	return files
}

// parseSection parses one "diff --git" file section into a File.
func parseSection(sec string) File {
	f := File{Text: strings.TrimRight(sec, "\n")}

	if (strings.Contains(sec, "Binary files ") && strings.Contains(sec, "differ")) ||
		strings.Contains(sec, "GIT binary patch") {
		f.Path = pathsFromDiffGit(sec).new
		f.Binary = true
		return f
	}

	for _, line := range strings.Split(sec, "\n") {
		switch {
		case strings.HasPrefix(line, "+++ b/"):
			f.Path = strings.TrimPrefix(line, "+++ b/")
		case strings.HasPrefix(line, "+++ a/"):
			f.Path = strings.TrimPrefix(line, "+++ a/")
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
			f.Additions++
		case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---"):
			f.Deletions++
		}
	}
	if f.Path == "" {
		f.Path = pathsFromDiffGit(sec).new
	}
	return f
}

type paths struct{ old, new string }

// pathsFromDiffGit parses the "diff --git a/old b/new" header line. When a
// rename has occurred the a/ and b/ paths differ; b/ is preferred for the
// "new" path.
func pathsFromDiffGit(sec string) paths {
	var p paths
	line := strings.SplitN(sec, "\n", 2)[0]
	fields := strings.Fields(line)
	if len(fields) >= 4 && strings.HasPrefix(fields[2], "a/") && strings.HasPrefix(fields[3], "b/") {
		p.old = strings.TrimPrefix(fields[2], "a/")
		p.new = strings.TrimPrefix(fields[3], "b/")
	} else if len(fields) >= 4 && strings.HasPrefix(fields[3], "b/") {
		p.new = strings.TrimPrefix(fields[3], "b/")
	}
	if p.new == "" {
		p.new = p.old
	}
	return p
}

// rateLimit reads X-RateLimit-* response headers; -1 / zero time mean unknown.
func rateLimit(resp *http.Response) (remaining, limit int64, reset time.Time) {
	remaining, limit = -1, -1
	if v := resp.Header.Get("X-RateLimit-Remaining"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			remaining = n
		}
	}
	if v := resp.Header.Get("X-RateLimit-Limit"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			limit = n
		}
	}
	if v := resp.Header.Get("X-RateLimit-Reset"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			reset = time.Unix(n, 0)
		}
	}
	return remaining, limit, reset
}

// apiErrorMessage extracts a GitHub-style {"message": "..."} error body.
func apiErrorMessage(resp *http.Response) string {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	var apiErr struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &apiErr); err == nil && apiErr.Message != "" {
		return apiErr.Message
	}
	return strings.TrimSpace(string(body))
}
