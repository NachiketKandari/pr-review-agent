package diff

import "testing"

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
