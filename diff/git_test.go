package diff

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestParseOrigin(t *testing.T) {
	cases := []struct {
		raw         string
		host, owner string
		repo        string
		ok          bool
	}{
		{"https://github.com/octocat/Hello-World.git", "github.com", "octocat", "Hello-World", true},
		{"https://github.com/octocat/Hello-World", "github.com", "octocat", "Hello-World", true},
		{"https://github.iseccorp.in/team/proj.git", "github.iseccorp.in", "team", "proj", true},
		{"git@github.com:octocat/Hello-World.git", "github.com", "octocat", "Hello-World", true},
		{"git@github.iseccorp.in:team/proj.git", "github.iseccorp.in", "team", "proj", true},
		{"ssh://git@github.com/octocat/Hello-World.git", "github.com", "octocat", "Hello-World", true},
		{"http://example.com/a/b/", "example.com", "a", "b", true},
		{"git@github.com:octocat/Hello-World.git ", "github.com", "octocat", "Hello-World", true},
		{"", "", "", "", false},
		{"not a url", "", "", "", false},
		{"https://github.com/onlyone.git", "", "", "", false},
	}
	for _, tc := range cases {
		host, owner, repo, ok := parseOrigin(tc.raw)
		if ok != tc.ok || host != tc.host || owner != tc.owner || repo != tc.repo {
			t.Errorf("parseOrigin(%q) = (%q,%q,%q,%v), want (%q,%q,%q,%v)",
				tc.raw, host, owner, repo, ok, tc.host, tc.owner, tc.repo, tc.ok)
		}
	}
}

func TestSymrefBranch(t *testing.T) {
	out := "ref: refs/heads/main\tHEAD\nabc123\tHEAD\n"
	if b, ok := symrefBranch(out); !ok || b != "main" {
		t.Errorf("symrefBranch(%q) = (%q,%v), want (main,true)", out, b, ok)
	}
	if b, ok := symrefBranch("abc123\tHEAD\n"); ok || b != "" {
		t.Errorf("symrefBranch without ref line = (%q,%v)", b, ok)
	}
	if b, ok := symrefBranch("ref: refs/heads/master\tHEAD\n"); !ok || b != "master" {
		t.Errorf("symrefBranch master = (%q,%v)", b, ok)
	}
}

func TestCurrentBranchAndRefExists(t *testing.T) {
	ctx := context.Background()

	t.Run("current branch", func(t *testing.T) {
		stubGit(t, func(args []string) (string, error) {
			if strings.Join(args, " ") != "-C . branch --show-current" {
				t.Errorf("unexpected git args %q", strings.Join(args, " "))
			}
			return "feature\n", nil
		})
		if b, err := CurrentBranch(ctx, "."); err != nil || b != "feature" {
			t.Errorf("CurrentBranch = (%q, %v), want (feature, nil)", b, err)
		}
	})

	t.Run("detached HEAD falls back to rev-parse", func(t *testing.T) {
		stubGit(t, func(args []string) (string, error) {
			switch strings.Join(args, " ") {
			case "-C . branch --show-current":
				return "", errors.New("no branch")
			case "-C . rev-parse --abbrev-ref HEAD":
				return "main\n", nil
			}
			t.Errorf("unexpected git args %q", strings.Join(args, " "))
			return "", errors.New("unexpected")
		})
		if b, err := CurrentBranch(ctx, "."); err != nil || b != "main" {
			t.Errorf("CurrentBranch = (%q, %v), want (main, nil)", b, err)
		}
	})

	t.Run("detached HEAD with no branch errors", func(t *testing.T) {
		stubGit(t, func(args []string) (string, error) {
			if strings.Contains(strings.Join(args, " "), "rev-parse --abbrev-ref") {
				return "HEAD\n", nil
			}
			return "", errors.New("no branch")
		})
		if _, err := CurrentBranch(ctx, "."); err == nil {
			t.Error("expected an error on detached HEAD")
		}
	})

	t.Run("is git repo", func(t *testing.T) {
		stubGit(t, func(args []string) (string, error) { return "true\n", nil })
		if !IsGitRepo(ctx, ".") {
			t.Error("IsGitRepo = false, want true")
		}
		stubGit(t, func(args []string) (string, error) { return "", errors.New("not a repo") })
		if IsGitRepo(ctx, ".") {
			t.Error("IsGitRepo = true, want false")
		}
	})

	t.Run("ref exists", func(t *testing.T) {
		stubGit(t, func(args []string) (string, error) {
			if strings.Join(args, " ") != "-C . rev-parse --verify --quiet main^{commit}" {
				t.Errorf("unexpected git args %q", strings.Join(args, " "))
			}
			return "abc123\n", nil
		})
		if !RefExists(ctx, ".", "main") {
			t.Error("RefExists(main) = false, want true")
		}
		stubGit(t, func(args []string) (string, error) { return "", errors.New("bad ref") })
		if RefExists(ctx, ".", "nope") {
			t.Error("RefExists(nope) = true, want false")
		}
	})
}
