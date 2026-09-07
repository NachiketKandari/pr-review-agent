package diff

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/NachiketKandari/pr-review-agent/xlog"
)

// gitEnv suppresses interactive prompts so a missing credential fails fast
// instead of hanging on a terminal.
const gitEnv = "GIT_TERMINAL_PROMPT=0"

var gitEnvPairs = []string{gitEnv, "GCM_INTERACTIVE=never"}

// OriginRepo identifies the repository of the clone at dir from its origin
// remote URL (host, owner, repo). It uses only local metadata, never
// contacts the network, and never needs an API token.
func OriginRepo(ctx context.Context, dir string) (Ref, error) {
	out, err := gitRun(ctx, dir, "remote", "get-url", "origin")
	if err != nil || strings.TrimSpace(out) == "" {
		return Ref{}, fmt.Errorf("%s is not a git clone with an origin remote", dir)
	}
	host, owner, repo, ok := parseOrigin(out)
	if !ok {
		return Ref{}, fmt.Errorf("cannot parse origin URL %q", out)
	}
	return Ref{Host: host, Owner: owner, Repo: repo}, nil
}

// RepoMatches reports whether the git clone at dir has origin pointing at
// ref's repository (same host, owner, and repo).
func RepoMatches(ctx context.Context, dir string, ref Ref) error {
	orepo, err := OriginRepo(ctx, dir)
	if err != nil {
		return err
	}
	if !strings.EqualFold(orepo.Host, ref.Host) || !strings.EqualFold(orepo.Owner, ref.Owner) || !strings.EqualFold(orepo.Repo, ref.Repo) {
		return fmt.Errorf("origin %s/%s (%s) does not match %s/%s (%s)",
			orepo.Owner, orepo.Repo, orepo.Host, ref.Owner, ref.Repo, ref.Host)
	}
	return nil
}

// RepoDiff produces the pull-request diff and commit messages for ref by
// running git inside the clone at dir: it fetches refs/pull/N/head and the
// default branch, then diffs merge-base(base)...head (the same shape as the
// GitHub PR diff). Auth is whatever git itself uses (SSH key, Git
// Credential Manager, ...); no API token is required.
func RepoDiff(ctx context.Context, dir string, ref Ref) ([]byte, Meta, error) {
	headRef := fmt.Sprintf("refs/pr-review/%d", ref.Number)
	if _, err := gitRun(ctx, dir, "fetch", "origin",
		fmt.Sprintf("refs/pull/%d/head:%s", ref.Number, headRef)); err != nil {
		return nil, Meta{}, fmt.Errorf("fetch PR head for %s (does this clone have access?): %w", ref.String(), err)
	}

	branch, err := defaultBranch(ctx, dir)
	if err != nil {
		return nil, Meta{}, err
	}
	base := "refs/remotes/origin/" + branch
	if _, err := gitRun(ctx, dir, "fetch", "origin", branch+":"+base); err != nil {
		return nil, Meta{}, fmt.Errorf("fetch base branch %q for %s: %w", branch, ref.String(), err)
	}

	out, err := gitRun(ctx, dir, "diff", "--no-ext-diff", base+"..."+headRef)
	if err != nil {
		return nil, Meta{}, fmt.Errorf("diff %s against %s: %w", headRef, base, err)
	}

	meta := Meta{
		HeadRef: "",
		BaseRef: branch, // the default branch is the assumed merge target
	}
	msgs, err := gitRun(ctx, dir, "log", "-n", strconv.Itoa(maxCommitMsgs), "--format=%B", base+".."+headRef)
	if err == nil {
		for _, m := range strings.Split(msgs, "\n\n") {
			if t := strings.TrimSpace(m); t != "" {
				meta.Commits = append(meta.Commits, t)
			}
		}
	}
	if len(meta.Commits) == 0 {
		xlog.Debug("no commit messages gathered from local clone", "pr", ref.String())
	}
	return []byte(out), meta, nil
}

// gitRun runs git -C dir <args...> with prompts disabled.
func gitRun(ctx context.Context, dir string, args ...string) (string, error) {
	full := append([]string{"-C", dir}, args...)
	return externalCommand(ctx, "git", full, "", gitEnvPairs...)
}

// defaultBranch returns the clone's default branch name, resolved via
// ls-remote --symref and falling back to local main/master.
func defaultBranch(ctx context.Context, dir string) (string, error) {
	if out, err := gitRun(ctx, dir, "ls-remote", "--symref", "origin", "HEAD"); err == nil {
		if b, ok := symrefBranch(out); ok {
			return b, nil
		}
	}
	for _, b := range []string{"main", "master", "develop"} {
		if _, err := gitRun(ctx, dir, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+b); err == nil {
			return b, nil
		}
	}
	return "", fmt.Errorf("cannot determine the default branch of the origin remote")
}

// symrefBranch extracts the branch from "git ls-remote --symref" output,
// e.g. "ref: refs/heads/main HEAD".
func symrefBranch(out string) (string, bool) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "ref:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				if b, ok := strings.CutPrefix(fields[1], "refs/heads/"); ok && b != "" {
					return b, true
				}
			}
		}
	}
	return "", false
}

// parseOrigin normalizes a git remote URL to (host, owner, repo). Handles
// https://host/owner/repo[.git], ssh://git@host/owner/repo[.git], and the
// scp-like git@host:owner/repo[.git] form.
func parseOrigin(raw string) (host, owner, repo string, ok bool) {
	s := strings.TrimSpace(raw)
	for _, scheme := range []string{"https://", "http://", "ssh://", "git://"} {
		if rest, found := strings.CutPrefix(s, scheme); found {
			s = rest
			break
		}
	}
	if rest, found := strings.CutPrefix(s, "git@"); found {
		s = rest
	}

	// scp-like form (host:owner/repo) wins when the colon precedes the
	// first slash; otherwise host[:port]/owner/repo.
	slash := strings.Index(s, "/")
	colon := strings.Index(s, ":")
	if colon >= 0 && (slash < 0 || colon < slash) {
		host = s[:colon]
		s = s[colon+1:]
	} else {
		if slash < 0 {
			return "", "", "", false
		}
		host = s[:slash]
		if j := strings.Index(host, ":"); j >= 0 {
			host = host[:j]
		}
		s = s[slash+1:]
	}
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".git")
	parts := strings.Split(s, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", "", false
	}
	return strings.ToLower(host), parts[0], parts[1], true
}

// fetchBranch refreshes origin/<branch> from the remote. Failure is not
// fatal: the branch may exist only locally, or the remote may be
// unreachable, and resolution falls back to local refs.
func fetchBranch(ctx context.Context, dir, branch string) {
	if _, err := gitRun(ctx, dir, "fetch", "--quiet", "origin", branch); err != nil {
		xlog.Debug("branch fetch failed; will try local refs",
			"dir", dir, "branch", branch, "error", err)
		return
	}
	xlog.Info("fetched branch from origin", "branch", branch, "dir", dir)
}

// resolveBranch resolves a branch name to a concrete ref inside the clone,
// preferring the remote-tracking ref (origin/<branch>, freshly fetched,
// which is what a GitHub PR would merge) and falling back to the local
// branch. Branches that exist only locally or only remotely both resolve.
func resolveBranch(ctx context.Context, dir, branch string) (string, error) {
	for _, ref := range []string{"origin/" + branch, branch} {
		out, err := gitRun(ctx, dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
		if err == nil && strings.TrimSpace(out) != "" {
			xlog.Info("branch resolved", "branch", branch, "ref", ref, "commit", strings.TrimSpace(out))
			return ref, nil
		}
	}
	return "", fmt.Errorf("branch %q not found locally or on origin", branch)
}

// BranchDiff produces the review diff and context for merging source into
// target inside the clone at dir, without a pull request or API token: it
// fetches both branches from origin (best effort), resolves each one, then
// diffs merge-base(target)...source (the same three-dot shape as a GitHub
// PR diff) and gathers commit messages from target..source. Auth is
// whatever git itself uses (SSH key, Git Credential Manager, ...).
func BranchDiff(ctx context.Context, dir, source, target string) ([]byte, Meta, error) {
	fetchBranch(ctx, dir, source)
	fetchBranch(ctx, dir, target)

	srcRef, err := resolveBranch(ctx, dir, source)
	if err != nil {
		return nil, Meta{}, fmt.Errorf("resolve source branch: %w", err)
	}
	tgtRef, err := resolveBranch(ctx, dir, target)
	if err != nil {
		return nil, Meta{}, fmt.Errorf("resolve target branch: %w", err)
	}

	out, err := gitRun(ctx, dir, "diff", "--no-ext-diff", tgtRef+"..."+srcRef)
	if err != nil {
		return nil, Meta{}, fmt.Errorf("diff %s into %s: %w", srcRef, tgtRef, err)
	}

	meta := Meta{
		HeadRef: source,
		BaseRef: target,
	}
	msgs, err := gitRun(ctx, dir, "log", "-n", strconv.Itoa(maxCommitMsgs), "--format=%B", tgtRef+".."+srcRef)
	if err == nil {
		for _, m := range strings.Split(msgs, "\n\n") {
			if t := strings.TrimSpace(m); t != "" {
				meta.Commits = append(meta.Commits, t)
			}
		}
	}
	if len(meta.Commits) == 0 {
		xlog.Debug("no commit messages gathered for the branch range",
			"source", source, "target", target)
	}
	return []byte(out), meta, nil
}
