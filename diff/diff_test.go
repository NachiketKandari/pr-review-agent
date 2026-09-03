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
	cases := []struct {
		raw   string
		want  Ref
		error bool
	}{
		{"https://github.com/octocat/Hello-World/pull/123", Ref{"octocat", "Hello-World", 123}, false},
		{"https://github.com/octocat/Hello-World/pull/123/", Ref{"octocat", "Hello-World", 123}, false},
		{"https://github.com/octocat/Hello-World/pull/123/files", Ref{"octocat", "Hello-World", 123}, false},
		{"https://github.com/octocat/Hello-World/pull/123#issuecomment-1", Ref{"octocat", "Hello-World", 123}, false},
		{"github.com/octocat/Hello-World/pull/123", Ref{"octocat", "Hello-World", 123}, false},
		{"www.github.com/octocat/Hello-World/pull/123", Ref{"octocat", "Hello-World", 123}, false},
		{"octocat/Hello-World/pull/123", Ref{"octocat", "Hello-World", 123}, false},
		{"  https://github.com/a/b/pull/1  ", Ref{"a", "b", 1}, false},

		{"https://github.com/octocat/Hello-World/pull/0", Ref{}, true},
		{"https://github.com/octocat/Hello-World/pull/abc", Ref{}, true},
		{"https://github.com/octocat/Hello-World/pull/", Ref{}, true},
		{"https://github.com/octocat/Hello-World/issues/123", Ref{}, true},
		{"https://github.com/octocat/Hello-World", Ref{}, true},
		{"https://gitlab.com/octocat/Hello-World/pull/1", Ref{}, true},
		{"https://github.com/octocat/Hello-World", Ref{}, true},
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

	ref := Ref{Owner: "octocat", Repo: "Hello-World", Number: 42}
	apiURL := srv.URL + "/repos/octocat/Hello-World/pulls/42"
	if _, err := fetch(context.Background(), apiURL, ref, "ghp_secret"); err != nil {
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

	if _, err := fetch(context.Background(), apiURL, ref, ""); err != nil {
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
	ref := Ref{Owner: "octocat", Repo: "Hello-World", Number: 7}
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

			_, err := fetch(context.Background(), srv.URL, ref, "tok")
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error = %q, want substring %q", err, tc.wantSubstr)
			}
		})
	}
}
