package diff

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// twoCommitPatch is git format-patch output for a PR with two commits.
const twoCommitPatch = `From 24481f04da779874d1dd067f56c928b2590a52a7 Mon Sep 17 00:00:00 2001
From: Jane Dev <jane@example.com>
Date: Wed, 15 Jan 2020 19:01:26 -0800
Subject: [PATCH] Copy parsed flag values when constructing LocalFlags

Fixes #1019
---
 command.go | 8 ++++++++
 1 file changed, 8 insertions(+)

diff --git a/command.go b/command.go
index ab3cf69a9..bed188465 100644
--- a/command.go
+++ b/command.go
@@ -1418,6 +1418,13 @@ func (c *Command) LocalFlags() *flag.FlagSet {
 	addToLocal := func(f *flag.Flag) {
+		nf := *f
+		nf.Value = f.Value
+		nf.Changed = false
+		c.lflags.AddFlag(&nf)
 	}
 }
From 9aa5f8ee5bd34c2e0b1f05f4d93f92cd00e22c1e Mon Sep 17 00:00:00 2001
From: Jane Dev <jane@example.com>
Date: Wed, 15 Jan 2020 19:05:11 -0800
Subject: [PATCH 2/2] Fix changed flag for LocalFlags

Flag Sets.
---
 command_test.go | 12 ++++++++++++
 1 file changed, 12 insertions(+)

diff --git a/command_test.go b/command_test.go
index 1a2b3c4..5d6e7f8 100644
--- a/command_test.go
+++ b/command_test.go
@@ -1,5 +1,9 @@
 package cobra
+
+func TestLocalFlagsChanged(t *testing.T) {}
`

func TestPatchSubjects(t *testing.T) {
	subjects := patchSubjects([]byte(twoCommitPatch))
	if len(subjects) != 2 {
		t.Fatalf("got %d subjects: %v", len(subjects), subjects)
	}
	if subjects[0] != "Copy parsed flag values when constructing LocalFlags" {
		t.Errorf("subject 0 = %q", subjects[0])
	}
	if subjects[1] != "Fix changed flag for LocalFlags" {
		t.Errorf("subject 1 = %q", subjects[1])
	}
}

// Parsing a format-patch body must yield clean file sections without the
// email headers, and correct +/- counts.
func TestParseDiffFromPatch(t *testing.T) {
	files := ParseDiff([]byte(twoCommitPatch))
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}
	if files[0].Path != "command.go" || files[1].Path != "command_test.go" {
		t.Errorf("paths = %q, %q", files[0].Path, files[1].Path)
	}
	if files[0].Additions != 4 || files[0].Deletions != 0 {
		t.Errorf("command.go = +%d/-%d, want +4/-0", files[0].Additions, files[0].Deletions)
	}
	for _, f := range files {
		for _, junk := range []string{"Subject:", "From 24481", "From: Jane", "Mon Sep 17"} {
			if strings.Contains(f.Text, junk) {
				t.Errorf("file %q text contains patch header junk %q:\n%s", f.Path, junk, f.Text)
			}
		}
	}
}

func TestFetchPatch(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		if r.Header.Get("Authorization") != "" {
			t.Error("Authorization header must not be sent for the patch link route")
		}
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, twoCommitPatch)
	}))
	t.Cleanup(srv.Close)

	ref := Ref{Host: "github.iseccorp.in", Owner: "team", Repo: "proj", Number: 12}
	body, subjects, err := fetchPatch(context.Background(), srv.URL, ref, "shareTok_abc", nil)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/team/proj/pull/12.patch" {
		t.Errorf("path = %q", gotPath)
	}
	if gotQuery != "token=shareTok_abc" {
		t.Errorf("query = %q, want token=shareTok_abc", gotQuery)
	}
	if !strings.Contains(string(body), "diff --git") {
		t.Error("body missing diff")
	}
	if len(subjects) != 2 {
		t.Errorf("subjects = %v", subjects)
	}

	// No token: no query parameter at all.
	if _, _, err := fetchPatch(context.Background(), srv.URL, ref, "", nil); err != nil {
		t.Fatal(err)
	}
	if gotQuery != "" {
		t.Errorf("query = %q, want empty without a token", gotQuery)
	}
}

func TestFetchPatchErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	ref := Ref{Host: "github.com", Owner: "acme", Repo: "widgets", Number: 3}
	_, _, err := fetchPatch(context.Background(), srv.URL, ref, "badTok", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "HTTP 404") || !strings.Contains(err.Error(), "diffToken") {
		t.Errorf("error = %q", err)
	}
}
