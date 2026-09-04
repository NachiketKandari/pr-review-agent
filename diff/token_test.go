package diff

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestResolveTokenOrder(t *testing.T) {
	orig := externalCommand
	t.Cleanup(func() { externalCommand = orig })

	t.Run("config wins", func(t *testing.T) {
		os.Unsetenv("GITHUB_TOKEN")
		calls := 0
		externalCommand = func(ctx context.Context, name string, args []string, stdin string, env ...string) (string, error) {
			calls++
			return "ghp_external", nil
		}
		if got := ResolveToken("cfg-token", "github.iseccorp.in"); got != "cfg-token" {
			t.Errorf("got %q", got)
		}
		if calls != 0 {
			t.Error("external helpers must not run when a config token exists")
		}
	})

	t.Run("env var wins over helpers", func(t *testing.T) {
		os.Setenv("GITHUB_TOKEN", "env-token")
		calls := 0
		externalCommand = func(ctx context.Context, name string, args []string, stdin string, env ...string) (string, error) {
			calls++
			return "ghp_external", nil
		}
		if got := ResolveToken("", "github.iseccorp.in"); got != "env-token" {
			t.Errorf("got %q", got)
		}
		if calls != 0 {
			t.Error("external helpers must not run when GITHUB_TOKEN is set")
		}
		os.Unsetenv("GITHUB_TOKEN")
	})

	t.Run("gh CLI account is used", func(t *testing.T) {
		os.Unsetenv("GITHUB_TOKEN")
		externalCommand = func(ctx context.Context, name string, args []string, stdin string, env ...string) (string, error) {
			if name != "gh" {
				t.Errorf("expected gh, got %q", name)
			}
			return "ghp_from_gh_cli", nil
		}
		if got := ResolveToken("", "github.iseccorp.in"); got != "ghp_from_gh_cli" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("git credentials used when gh is unavailable", func(t *testing.T) {
		os.Unsetenv("GITHUB_TOKEN")
		externalCommand = func(ctx context.Context, name string, args []string, stdin string, env ...string) (string, error) {
			switch name {
			case "gh":
				return "", &execExitError{}
			case "git":
				if !strings.Contains(stdin, "host=github.iseccorp.in") {
					t.Errorf("credential input = %q", stdin)
				}
				return "username=me\npassword=ghp_from_git_credentials\n", nil
			default:
				t.Errorf("unexpected command %q", name)
				return "", nil
			}
		}
		if got := ResolveToken("", "github.iseccorp.in"); got != "ghp_from_git_credentials" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("nothing found falls back to unauthenticated", func(t *testing.T) {
		os.Unsetenv("GITHUB_TOKEN")
		externalCommand = func(ctx context.Context, name string, args []string, stdin string, env ...string) (string, error) {
			return "", &execExitError{}
		}
		if got := ResolveToken("", "github.com"); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}

type execExitError struct{}

func (e *execExitError) Error() string { return "exit status 1" }
