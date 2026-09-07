package diff

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// stubGit replaces externalCommand with a git-only stub and returns a
// pointer to the recorded argument lists.
func stubGit(t *testing.T, run func(args []string) (string, error)) *[][]string {
	t.Helper()
	orig := externalCommand
	var calls [][]string
	externalCommand = func(ctx context.Context, name string, args []string, stdin string, env ...string) (string, error) {
		if name != "git" {
			t.Errorf("expected git, got %q", name)
		}
		calls = append(calls, args)
		return run(args)
	}
	t.Cleanup(func() { externalCommand = orig })
	return &calls
}

func TestOriginRepo(t *testing.T) {
	t.Run("parses origin", func(t *testing.T) {
		stubGit(t, func(args []string) (string, error) {
			if got := strings.Join(args, " "); got != "-C . remote get-url origin" {
				t.Errorf("unexpected git args %q", got)
			}
			return "git@github.com:octocat/Hello-World.git\n", nil
		})
		ref, err := OriginRepo(context.Background(), ".")
		if err != nil {
			t.Fatal(err)
		}
		if ref.Host != "github.com" || ref.Owner != "octocat" || ref.Repo != "Hello-World" || ref.Number != 0 {
			t.Errorf("got %+v", ref)
		}
		if got := ref.String(); got != "octocat/Hello-World@github.com" {
			t.Errorf("String() = %q", got)
		}
	})

	t.Run("no origin remote", func(t *testing.T) {
		stubGit(t, func(args []string) (string, error) { return "", errors.New("not a repository") })
		if _, err := OriginRepo(context.Background(), "."); err == nil {
			t.Error("expected an error without an origin remote")
		}
	})
}

func TestResolveBranch(t *testing.T) {
	ctx := context.Background()

	t.Run("prefers the remote-tracking ref", func(t *testing.T) {
		var revs []string
		stubGit(t, func(args []string) (string, error) {
			if args[2] == "rev-parse" {
				revs = append(revs, args[5])
				return "1111111", nil
			}
			return "", nil
		})
		ref, err := resolveBranch(ctx, ".", "feature")
		if err != nil || ref != "origin/feature" {
			t.Errorf("got (%q, %v)", ref, err)
		}
		if len(revs) != 1 || revs[0] != "origin/feature^{commit}" {
			t.Errorf("rev-parse refs: %v", revs)
		}
	})

	t.Run("falls back to the local branch", func(t *testing.T) {
		stubGit(t, func(args []string) (string, error) {
			if args[2] == "rev-parse" && strings.HasPrefix(args[5], "origin/") {
				return "", errors.New("exit status 1")
			}
			return "2222222", nil
		})
		if ref, err := resolveBranch(ctx, ".", "feature"); err != nil || ref != "feature" {
			t.Errorf("got (%q, %v)", ref, err)
		}
	})

	t.Run("unknown branch", func(t *testing.T) {
		stubGit(t, func(args []string) (string, error) { return "", errors.New("exit status 1") })
		if _, err := resolveBranch(ctx, ".", "nope"); err == nil {
			t.Error("expected an error for an unknown branch")
		}
	})
}

func TestBranchDiff(t *testing.T) {
	calls := stubGit(t, func(args []string) (string, error) {
		switch sub := args[2]; sub {
		case "fetch":
			return "", nil
		case "rev-parse":
			if strings.HasPrefix(args[5], "origin/target") {
				return "", errors.New("exit status 1") // target exists only locally
			}
			return "deadbeef", nil
		case "diff":
			return "diff --git a/x.txt b/x.txt\nindex 111..222 100644\n", nil
		case "log":
			return "first commit\n\nsecond commit\n", nil
		default:
			return "", errors.New("unexpected git subcommand " + sub)
		}
	})

	body, meta, err := BranchDiff(context.Background(), ".", "feature", "target")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "diff --git a/x.txt") {
		t.Errorf("diff body = %q", string(body))
	}
	if meta.HeadRef != "feature" || meta.BaseRef != "target" {
		t.Errorf("meta branches = %q/%q", meta.HeadRef, meta.BaseRef)
	}
	if len(meta.Commits) != 2 || meta.Commits[0] != "first commit" || meta.Commits[1] != "second commit" {
		t.Errorf("commits = %q", meta.Commits)
	}

	var diffRange, logRange string
	for _, c := range *calls {
		if c[2] == "diff" {
			diffRange = c[4]
		}
		if c[2] == "log" {
			logRange = c[6]
		}
	}
	// source resolves on origin, target only locally
	if diffRange != "target...origin/feature" {
		t.Errorf("diff range = %q", diffRange)
	}
	if logRange != "target..origin/feature" {
		t.Errorf("log range = %q", logRange)
	}
}

func TestRefString(t *testing.T) {
	if got := (Ref{Host: "github.com", Owner: "o", Repo: "r", Number: 7}).String(); got != "o/r#7" {
		t.Errorf("PR String() = %q", got)
	}
	if got := (Ref{Host: "github.com", Owner: "o", Repo: "r"}).String(); got != "o/r@github.com" {
		t.Errorf("branch String() = %q", got)
	}
	if got := (Ref{Owner: "o", Repo: "r"}).String(); got != "o/r" {
		t.Errorf("hostless String() = %q", got)
	}
}
