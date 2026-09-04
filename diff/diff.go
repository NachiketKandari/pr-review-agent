// Package diff handles GitHub pull-request references: URL parsing, diff
// and PR-metadata fetching from the GitHub REST API, and unified-diff
// parsing.
//
// github.com and GitHub Enterprise hosts are both supported. Only read-only
// GET endpoints are ever called: the PR diff, PR metadata, and commit
// messages. No endpoint can create, merge, comment on, or otherwise modify
// anything.
package diff

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/NachiketKandari/pr-review-agent/xlog"
)

const (
	userAgent     = "pr-review-agent/1.0"
	diffAccept    = "application/vnd.github.v3.diff"
	jsonAccept    = "application/vnd.github+json"
	apiMaxBody    = 64 << 20 // 64 MiB safety cap on response bodies
	maxCommitMsgs = 30       // commit messages fetched per PR
)

// Ref identifies a GitHub pull request. Host is github.com or a GitHub
// Enterprise host such as github.iseccorp.in.
type Ref struct {
	Host   string
	Owner  string
	Repo   string
	Number int
}

// String returns a compact, log-friendly "owner/repo#N" form.
func (r Ref) String() string { return fmt.Sprintf("%s/%s#%d", r.Owner, r.Repo, r.Number) }

// GitHubURL returns the canonical pull-request page URL.
func (r Ref) GitHubURL() string {
	host := r.Host
	if host == "" {
		host = "github.com"
	}
	return fmt.Sprintf("https://%s/%s/%s/pull/%d", host, r.Owner, r.Repo, r.Number)
}

// apiBase maps the web host to the REST API base: github.com is served on
// api.github.com, GitHub Enterprise Server on https://<host>/api/v3.
func (r Ref) apiBase() string {
	host := r.Host
	if host == "" || strings.EqualFold(host, "github.com") {
		return "https://api.github.com"
	}
	return fmt.Sprintf("https://%s/api/v3", host)
}

// ParseURL parses a pull-request reference from a full URL
// (https://github.com/owner/repo/pull/N), a scheme-less URL
// (github.com/owner/repo/pull/N), or a shortened form
// (owner/repo/pull/N). github.com and GitHub Enterprise hosts
// (https://github.iseccorp.in/owner/repo/pull/N) are accepted; any other
// host produces a clear error. Anything after the PR number (e.g.
// "/files", "/commits", a fragment) is ignored.
func ParseURL(raw string) (Ref, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return Ref{}, fmt.Errorf("empty pull request URL")
	}

	// Accept an optional https:// prefix (the only supported scheme).
	if i := strings.Index(s, "://"); i >= 0 {
		if s[:i] != "https" {
			return Ref{}, fmt.Errorf("not a GitHub URL (only https://<github-host>/owner/repo/pull/N is supported): %q", raw)
		}
		s = s[i+3:]
	}
	s = strings.TrimSuffix(s, "/")

	// A leading segment containing a dot is a host
	// (github.com, github.iseccorp.in, www.github.com, ...); everything
	// else is the shortened owner/repo/pull/N form. Bare host names like
	// "github.com/..." and enterprise URLs therefore both resolve, while
	// "octocat/Hello-World/pull/123" keeps the default host.
	host := "github.com"
	rest := s
	if first, _, _ := strings.Cut(s, "/"); strings.Contains(first, ".") {
		h, remainder, ok := strings.Cut(s, "/")
		if !ok {
			return Ref{}, fmt.Errorf("not a GitHub pull request URL (expected .../owner/repo/pull/N): %q", raw)
		}
		h = strings.ToLower(strings.TrimPrefix(h, "www."))
		if !validHost(h) {
			return Ref{}, fmt.Errorf("not a GitHub URL (host %q is not a valid host): %q", h, raw)
		}
		host, rest = h, remainder
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

	ref := Ref{Host: host, Owner: owner, Repo: repo, Number: num}
	xlog.Info("parsed PR URL", "pr", ref.String(), "host", host, "owner", owner, "repo", repo, "number", num)
	return ref, nil
}

// validHost reports whether h is a plausible DNS host (letters, digits,
// dots, and hyphens only).
func validHost(h string) bool {
	for _, r := range h {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-':
		default:
			return false
		}
	}
	return true
}

func pullPath(ref Ref) string {
	return fmt.Sprintf("/repos/%s/%s/pulls/%d", url.PathEscape(ref.Owner), url.PathEscape(ref.Repo), ref.Number)
}

// Fetch retrieves the unified diff of ref from the GitHub REST API. When
// token is non-empty it is sent as a Bearer credential; otherwise the
// request is unauthenticated. hc may be nil for http.DefaultClient. Errors
// are mapped to friendly messages for 404 (not found / private), 403 (rate
// limit), 401 (bad credentials), and 422 (PR too large to diff).
func Fetch(ctx context.Context, ref Ref, token string, hc *http.Client) ([]byte, error) {
	return get(ctx, hc, ref, token, diffAccept, pullPath(ref), "diff")
}

// Meta carries the read-only PR context used to make the review aware of
// intent: title, description, head branch, and commit messages.
type Meta struct {
	Title   string
	Body    string
	HeadRef string
	Commits []string
}

// FetchMeta retrieves PR metadata (title, description, head branch) and its
// commit messages via read-only GET requests. Either call failing is a
// hard error; callers that only want best-effort context should degrade
// gracefully.
func FetchMeta(ctx context.Context, ref Ref, token string, hc *http.Client) (Meta, error) {
	return fetchMeta(ctx, ref, token, hc, "")
}

// fetchMeta is FetchMeta with an explicit API base override (used by tests).
func fetchMeta(ctx context.Context, ref Ref, token string, hc *http.Client, base string) (Meta, error) {
	pullBody, err := getAt(ctx, hc, base, ref, token, jsonAccept, pullPath(ref), "pull")
	if err != nil {
		return Meta{}, err
	}
	var pull struct {
		Title string  `json:"title"`
		Body  *string `json:"body"`
		Head  struct {
			Ref string `json:"ref"`
		} `json:"head"`
	}
	if err := json.Unmarshal(pullBody, &pull); err != nil {
		return Meta{}, fmt.Errorf("decode PR metadata for %s: %w", ref.String(), err)
	}

	m := Meta{
		Title:   strings.TrimSpace(pull.Title),
		HeadRef: pull.Head.Ref,
	}
	if pull.Body != nil {
		m.Body = strings.TrimSpace(*pull.Body)
	}

	commitsPath := fmt.Sprintf("%s/commits?per_page=%d", pullPath(ref), maxCommitMsgs)
	commitsBody, err := getAt(ctx, hc, base, ref, token, jsonAccept, commitsPath, "commits")
	if err != nil {
		return m, err
	}
	var commits []struct {
		Commit struct {
			Message string `json:"message"`
		} `json:"commit"`
	}
	if err := json.Unmarshal(commitsBody, &commits); err != nil {
		return Meta{}, fmt.Errorf("decode commit messages for %s: %w", ref.String(), err)
	}
	for _, c := range commits {
		if msg := strings.TrimSpace(c.Commit.Message); msg != "" {
			m.Commits = append(m.Commits, msg)
		}
	}
	return m, nil
}

// get performs one read-only GET against ref's API base and returns the
// body on 2xx. When hc is nil, http.DefaultClient is used. kind is one of
// "diff", "pull", or "commits" and controls the success log line.
func get(ctx context.Context, hc *http.Client, ref Ref, token, accept, path, kind string) ([]byte, error) {
	return getAt(ctx, hc, "", ref, token, accept, path, kind)
}

// getAt is get with an explicit API base override (used by tests).
func getAt(ctx context.Context, hc *http.Client, base string, ref Ref, token, accept, path, kind string) ([]byte, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	apiURL := base
	if apiURL == "" {
		apiURL = ref.apiBase()
	}
	apiURL += path

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", userAgent)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	start := time.Now()
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s from %s: %w", ref.String(), xlog.SafeURL(apiURL), err)
	}
	defer resp.Body.Close()
	durationMs := time.Since(start).Milliseconds()

	rlRemaining, rlLimit, rlReset := rateLimit(resp)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, statusErr(resp, ref, kind, rlRemaining, rlLimit, rlReset)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, apiMaxBody))
	if err != nil {
		return nil, fmt.Errorf("read %s response for %s: %w", kind, ref.String(), err)
	}

	if kind == "diff" {
		xlog.Info("fetched PR diff",
			"pr", ref.String(), "host", ref.Host,
			"diff_bytes", len(body),
			"duration_ms", durationMs,
			"rate_limit_remaining", rlRemaining,
			"rate_limit_limit", rlLimit,
			"rate_limit_reset", rlReset.Format(time.RFC3339))
	} else {
		xlog.Info("fetched github data",
			"pr", ref.String(), "host", ref.Host, "kind", kind,
			"bytes", len(body),
			"duration_ms", durationMs,
			"rate_limit_remaining", rlRemaining,
			"rate_limit_limit", rlLimit,
			"rate_limit_reset", rlReset.Format(time.RFC3339))
	}
	return body, nil
}

// statusErr maps non-2xx GitHub responses to friendly errors.
func statusErr(resp *http.Response, ref Ref, kind string, rlRemaining, rlLimit int64, rlReset time.Time) error {
	msg := apiErrorMessage(resp)
	switch resp.StatusCode {
	case http.StatusNotFound:
		return fmt.Errorf("PR %s not found or the repository is private / does not exist", ref.String())
	case http.StatusForbidden:
		if rlRemaining == 0 {
			return fmt.Errorf("GitHub API rate limit exceeded for %s (limit %d, resets %s); set github.token or GITHUB_TOKEN for higher limits",
				ref.String(), rlLimit, rlReset.Format(time.RFC3339))
		}
		return fmt.Errorf("GitHub API rejected the request for %s: %s", ref.String(), msg)
	case http.StatusUnauthorized:
		return fmt.Errorf("GitHub API authentication failed for %s: check github.token / GITHUB_TOKEN", ref.String())
	case http.StatusUnprocessableEntity:
		if kind == "diff" {
			return fmt.Errorf("GitHub could not produce a diff for %s (the PR may be too large): %s", ref.String(), msg)
		}
		return fmt.Errorf("GitHub could not process %s for %s: %s", kind, ref.String(), msg)
	default:
		return fmt.Errorf("GitHub API returned HTTP %d for %s: %s", resp.StatusCode, ref.String(), msg)
	}
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
