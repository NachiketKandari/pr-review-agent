package diff

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseURL(t *testing.T) {
	gh := "github.com"
	corp := "github.iseccorp.in"
	cases := []struct {
		raw   string
		want  Ref
		error bool
	}{
		{"https://github.com/octocat/Hello-World/pull/123", Ref{gh, "octocat", "Hello-World", 123}, false},
		{"https://github.com/octocat/Hello-World/pull/123/", Ref{gh, "octocat", "Hello-World", 123}, false},
		{"https://github.com/octocat/Hello-World/pull/123/files", Ref{gh, "octocat", "Hello-World", 123}, false},
		{"https://github.com/octocat/Hello-World/pull/123#issuecomment-1", Ref{gh, "octocat", "Hello-World", 123}, false},
		{"github.com/octocat/Hello-World/pull/123", Ref{gh, "octocat", "Hello-World", 123}, false},
		{"www.github.com/octocat/Hello-World/pull/123", Ref{gh, "octocat", "Hello-World", 123}, false},
		{"octocat/Hello-World/pull/123", Ref{gh, "octocat", "Hello-World", 123}, false},
		{"  https://github.com/a/b/pull/1  ", Ref{gh, "a", "b", 1}, false},

		// GitHub Enterprise hosts resolve to their own API.
		{"https://github.iseccorp.in/team/proj/pull/12", Ref{corp, "team", "proj", 12}, false},
		{"github.iseccorp.in/team/proj/pull/12", Ref{corp, "team", "proj", 12}, false},
		{"https://github.iseccorp.in/team/proj/pull/12/files", Ref{corp, "team", "proj", 12}, false},

		{"https://github.com/octocat/Hello-World/pull/0", Ref{}, true},
		{"https://github.com/octocat/Hello-World/pull/abc", Ref{}, true},
		{"https://github.com/octocat/Hello-World/pull/", Ref{}, true},
		{"https://github.com/octocat/Hello-World/issues/123", Ref{}, true},
		{"https://github.com/octocat/Hello-World", Ref{}, true},
		{"https://gitlab.com/octocat/Hello-World", Ref{}, true},
		{"http://github.com/octocat/Hello-World/pull/1", Ref{}, true},
		{"https://github.com/octocat", Ref{}, true},
		{"", Ref{}, true},
		{"not even a url", Ref{}, true},
		{"git@github.com:octocat/Hello-World.git", Ref{}, true},
	}
	for _, tc := range cases {
		got, err := ParseURL(tc.raw)
		if tc.error {
			if err == nil {
				t.Errorf("ParseURL(%q) = %+v, want error", tc.raw, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseURL(%q): %v", tc.raw, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseURL(%q) = %+v, want %+v", tc.raw, got, tc.want)
		}
	}
}

func TestRefURLs(t *testing.T) {
	ref := Ref{Host: "github.com", Owner: "octocat", Repo: "Hello-World", Number: 42}
	if got := ref.GitHubURL(); got != "https://github.com/octocat/Hello-World/pull/42" {
		t.Errorf("GitHubURL = %q", got)
	}
	if got := ref.apiBase(); got != "https://api.github.com" {
		t.Errorf("github.com apiBase = %q", got)
	}
	if got := ref.String(); got != "octocat/Hello-World#42" {
		t.Errorf("String = %q", got)
	}

	ent := Ref{Host: "github.iseccorp.in", Owner: "team", Repo: "proj", Number: 7}
	if got := ent.apiBase(); got != "https://github.iseccorp.in/api/v3" {
		t.Errorf("enterprise apiBase = %q", got)
	}
	if got := ent.GitHubURL(); got != "https://github.iseccorp.in/team/proj/pull/7" {
		t.Errorf("enterprise GitHubURL = %q", got)
	}
	if got := ent.apiBase(); got != "https://github.iseccorp.in/api/v3" {
		t.Errorf("apiBase = %q", got)
	}

	// Zero host defaults to github.com.
	zero := Ref{Owner: "a", Repo: "b", Number: 1}
	if got := zero.apiBase(); got != "https://api.github.com" {
		t.Errorf("zero-host apiBase = %q", got)
	}
	if got := zero.GitHubURL(); got != "https://github.com/a/b/pull/1" {
		t.Errorf("zero-host GitHubURL = %q", got)
	}
}

const sampleDiff = `diff --git a/README.md b/README.md
index a1b2c3d..e4f5a6b 100644
--- a/README.md
+++ b/README.md
@@ -1,3 +1,4 @@
 # title
+new line here
 old line
-removed line
diff --git a/internal/worker.go b/internal/worker.go
new file mode 100644
index 0000000..abc1234
--- /dev/null
+++ b/internal/worker.go
@@ -0,0 +1,3 @@
+package worker
+
+func Run() {}
diff --git a/assets/logo.png b/assets/logo.png
index 1111111..2222222 100644
Binary files a/assets/logo.png and b/assets/logo.png differ
diff --git a/cmd/root/main.go b/cmd/root/main.go
deleted file mode 100644
index abc..def 100644
--- a/cmd/root/main.go
+++ /dev/null
@@ -1,3 +0,0 @@
-package main
-
-func main() {}
`

func TestParseDiff(t *testing.T) {
	files := ParseDiff([]byte(sampleDiff))
	if len(files) != 3 {
		t.Fatalf("expected 3 files, got %d: %+v", len(files), files)
	}

	readme := files[0]
	if readme.Path != "README.md" {
		t.Errorf("file 0 path = %q", readme.Path)
	}
	if readme.Additions != 1 || readme.Deletions != 1 {
		t.Errorf("README counts = +%d/-%d, want +1/-1", readme.Additions, readme.Deletions)
	}

	worker := files[1]
	if worker.Path != "internal/worker.go" {
		t.Errorf("file 1 path = %q", worker.Path)
	}
	if worker.Additions != 3 || worker.Deletions != 0 {
		t.Errorf("worker counts = +%d/-%d, want +3/-0", worker.Additions, worker.Deletions)
	}

	main := files[2]
	if main.Path != "cmd/root/main.go" {
		t.Errorf("file 2 path = %q", main.Path)
	}
	if main.Additions != 0 || main.Deletions != 3 {
		t.Errorf("main counts = +%d/-%d, want +0/-3", main.Additions, main.Deletions)
	}

	if !strings.Contains(readme.Text, "diff --git") {
		t.Error("file text missing diff header")
	}
	if strings.HasSuffix(readme.Text, "\n") {
		t.Error("file text has trailing newline")
	}
}

func TestParseDiffBinarySkipped(t *testing.T) {
	files := ParseDiff([]byte(sampleDiff))
	for _, f := range files {
		if f.Path == "assets/logo.png" {
			t.Error("binary file not skipped")
		}
	}
}

func TestParseDiffEmpty(t *testing.T) {
	if files := ParseDiff(nil); len(files) != 0 {
		t.Errorf("empty diff parsed %d files", len(files))
	}
	if files := ParseDiff([]byte("  \n  ")); len(files) != 0 {
		t.Errorf("whitespace diff parsed %d files", len(files))
	}
}

func TestFetchHeadersAndAuth(t *testing.T) {
	var gotAuth, gotAccept, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		gotUA = r.Header.Get("User-Agent")
		if r.URL.Path != "/repos/octocat/Hello-World/pulls/42" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("X-RateLimit-Remaining", "59")
		w.Header().Set("X-RateLimit-Limit", "60")
		w.Header().Set("X-RateLimit-Reset", "1800000000")
		fmt.Fprint(w, "diff --git a/x b/x\n")
	}))
	t.Cleanup(srv.Close)

	ref := Ref{Host: "github.com", Owner: "octocat", Repo: "Hello-World", Number: 42}
	if _, err := getAt(context.Background(), nil, srv.URL, ref, "ghp_secret", diffAccept, "/repos/octocat/Hello-World/pulls/42", "diff"); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer ghp_secret" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotAccept != diffAccept {
		t.Errorf("Accept = %q, want %q", gotAccept, diffAccept)
	}
	if gotUA == "" {
		t.Error("User-Agent not set")
	}

	if _, err := getAt(context.Background(), nil, srv.URL, ref, "", diffAccept, "/repos/octocat/Hello-World/pulls/42", "diff"); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "" {
		t.Errorf("Authorization set for anonymous fetch: %q", gotAuth)
	}
}

func TestFetchErrors(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		headers    map[string]string
		body       string
		wantSubstr string
	}{
		{"not found", http.StatusNotFound, nil, `{"message":"Not Found"}`, "not found or the repository is private"},
		{"rate limited", http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "0"}, `{"message":"API rate limit exceeded"}`, "rate limit exceeded"},
		{"forbidden other", http.StatusForbidden, nil, `{"message":"Resource protected"}`, "rejected"},
		{"unauthorized", http.StatusUnauthorized, nil, `{"message":"Bad credentials"}`, "authentication failed"},
		{"unprocessable", http.StatusUnprocessableEntity, nil, `{"message":"Diff too large"}`, "too large"},
		{"server error", http.StatusInternalServerError, nil, `boom`, "HTTP 500"},
	}
	ref := Ref{Host: "github.com", Owner: "octocat", Repo: "Hello-World", Number: 7}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			t.Cleanup(srv.Close)

			_, err := getAt(context.Background(), nil, srv.URL, ref, "tok", diffAccept, "/x", "diff")
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error = %q, want substring %q", err, tc.wantSubstr)
			}
		})
	}
}

func TestFetchMeta(t *testing.T) {
	seen := map[string]bool{}
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		seen[r.URL.Path] = true
		switch r.URL.Path {
		case "/repos/acme/widgets/pulls/9":
			fmt.Fprint(w, `{"title":"Add retry to fetch","body":"Closes ABC-123. Keeps on trying.","head":{"ref":"feature/ABC-123-retry"}}`)
		case "/repos/acme/widgets/pulls/9/commits":
			fmt.Fprint(w, `[
				{"commit":{"message":"feat: retry fetch calls\n\nAdds three attempts with backoff."}},
				{"commit":{"message":"fix: typo in comment"}}
			]`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	ref := Ref{Host: "github.com", Owner: "acme", Repo: "widgets", Number: 9}
	m, err := fetchMeta(context.Background(), ref, "tok", nil, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !seen["/repos/acme/widgets/pulls/9"] || !seen["/repos/acme/widgets/pulls/9/commits"] {
		t.Errorf("requested paths = %v", seen)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if m.Title != "Add retry to fetch" || m.Body != "Closes ABC-123. Keeps on trying." {
		t.Errorf("meta = %+v", m)
	}
	if m.HeadRef != "feature/ABC-123-retry" {
		t.Errorf("HeadRef = %q", m.HeadRef)
	}
	if len(m.Commits) != 2 || !strings.Contains(m.Commits[0], "feat: retry fetch calls") {
		t.Errorf("commits = %v", m.Commits)
	}
}

func TestFetchMetaNullBodyAndError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widgets/pulls/9":
			fmt.Fprint(w, `{"title":"t","body":null,"head":{"ref":"main"}}`)
		case "/repos/acme/widgets/pulls/9/commits":
			fmt.Fprint(w, `[]`)
		default:
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message":"nope"}`)
		}
	}))
	t.Cleanup(srv.Close)
	ref := Ref{Host: "github.com", Owner: "acme", Repo: "widgets", Number: 9}

	m, err := fetchMeta(context.Background(), ref, "tok", nil, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if m.Body != "" || m.Title != "t" {
		t.Errorf("meta = %+v (null body must read as empty)", m)
	}

	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv2.Close)
	if _, err := fetchMeta(context.Background(), ref, "tok", nil, srv2.URL); err == nil {
		t.Error("expected error when PR metadata is not found")
	} else if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q", err)
	}
}
