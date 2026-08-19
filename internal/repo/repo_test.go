package repo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestNewRequiresAbsoluteDirectory(t *testing.T) {
	t.Parallel()

	if _, err := New("relative"); !errors.Is(err, ErrConfig) {
		t.Fatalf("New(relative) error = %v, want ErrConfig", err)
	}
	if _, err := New(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, ErrConfig) {
		t.Fatalf("New(missing) error = %v, want ErrConfig", err)
	}
	gitDirectory := filepath.Join(t.TempDir(), ".git")
	if err := os.Mkdir(gitDirectory, 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	if _, err := New(gitDirectory); !errors.Is(err, ErrConfig) {
		t.Fatalf("New(.git) error = %v, want ErrConfig", err)
	}
}

func TestReadConfinesAccessAndProtectsSecrets(t *testing.T) {
	root, outside := testWorkspace(t)
	workspace, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	writeTestFile(t, filepath.Join(root, "src", "main.go"), "package main\n// needle\n")
	writeTestFile(t, filepath.Join(root, "src", "token.go"), "package auth\n")
	writeTestFile(t, filepath.Join(root, ".env"), "DATABASE_PASSWORD=not-for-output\n")
	writeTestFile(t, filepath.Join(root, ".env.local"), "API_KEY=not-for-output\n")
	writeTestFile(t, filepath.Join(root, "credentials.json"), "not-for-output\n")
	writeTestFile(t, filepath.Join(root, "id_rsa"), "not-for-output\n")
	writeTestFile(t, filepath.Join(root, ".git", "config"), "not-for-output\n")
	writeTestFile(t, filepath.Join(outside, "outside.txt"), "not-for-output\n")
	if err := os.Symlink(filepath.Join(outside, "outside.txt"), filepath.Join(root, "outside-link")); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, ".git", "config"), filepath.Join(root, "git-link")); err != nil {
		t.Fatalf("create protected symlink: %v", err)
	}

	path, content, err := workspace.Read("src/main.go")
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if path != "src/main.go" || content != "package main\n// needle\n" {
		t.Fatalf("Read() = (%q, %q), want safe source file", path, content)
	}
	if _, content, err := workspace.Read("src/token.go"); err != nil || content != "package auth\n" {
		t.Fatalf("Read(token.go) = (%q, %v), want source file to remain accessible", content, err)
	}

	for _, path := range []string{
		"/etc/passwd", "C:\\Windows\\win.ini", "../outside.txt", "src/../main.go",
		".git/config", ".env", ".env.local", "credentials.json", "id_rsa", "outside-link", "git-link",
	} {
		t.Run(path, func(t *testing.T) {
			_, _, err := workspace.Read(path)
			if !errors.Is(err, ErrRejected) {
				t.Fatalf("Read(%q) error = %v, want ErrRejected", path, err)
			}
		})
	}
}

func TestSearchSkipsProtectedAndSymlinkedFiles(t *testing.T) {
	root, outside := testWorkspace(t)
	workspace, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	writeTestFile(t, filepath.Join(root, "src", "main.go"), "needle in source\nsecond line\n")
	writeTestFile(t, filepath.Join(root, ".env"), "needle in env\n")
	writeTestFile(t, filepath.Join(root, ".git", "config"), "needle in git\n")
	writeTestFile(t, filepath.Join(root, "secrets", "value.txt"), "needle in secret\n")
	writeTestFile(t, filepath.Join(outside, "outside.txt"), "needle outside\n")
	if err := os.Symlink(outside, filepath.Join(root, "linked-outside")); err != nil {
		t.Fatalf("create directory symlink: %v", err)
	}

	result, err := workspace.Search(context.Background(), "needle", "")
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(result.Matches) != 1 {
		t.Fatalf("Search() matches = %#v, want exactly source match", result.Matches)
	}
	if match := result.Matches[0]; match.Path != "src/main.go" || match.Line != 1 || match.Text != "needle in source" {
		t.Errorf("match = %#v, want source match", match)
	}

	for _, scope := range []string{"/etc", "../", "src/../", ".git", ".env", "linked-outside"} {
		t.Run(scope, func(t *testing.T) {
			_, err := workspace.Search(context.Background(), "needle", scope)
			if !errors.Is(err, ErrRejected) {
				t.Fatalf("Search(%q) error = %v, want ErrRejected", scope, err)
			}
		})
	}
}

func TestSearchIsBounded(t *testing.T) {
	root, _ := testWorkspace(t)
	workspace, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	writeTestFile(t, filepath.Join(root, "matches.txt"), strings.Repeat("needle\n", maxMatches+1))
	result, err := workspace.Search(context.Background(), "needle", "")
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(result.Matches) != maxMatches || !result.Truncated {
		t.Errorf("Search() = %#v, want %d truncated matches", result, maxMatches)
	}
}

func TestSearchEnforcesRepositoryBudgetsAndCancellation(t *testing.T) {
	root, _ := testWorkspace(t)
	workspace, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	workspace.maxSearchFiles = 1
	workspace.maxSearchBytes = maxFileBytes
	writeTestFile(t, filepath.Join(root, "a.txt"), "needle a\n")
	writeTestFile(t, filepath.Join(root, "b.txt"), "needle b\n")

	result, err := workspace.Search(context.Background(), "needle", "")
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(result.Matches) != 1 || !result.Truncated {
		t.Fatalf("Search() = %#v, want one match and truncation at file budget", result)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := workspace.Search(ctx, "needle", ""); !errors.Is(err, ErrRejected) {
		t.Fatalf("Search(cancelled) error = %v, want ErrRejected", err)
	}
}

func TestSearchBoundsLongMatchText(t *testing.T) {
	root, _ := testWorkspace(t)
	workspace, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	line := strings.Repeat("a", maxMatchTextBytes) + "needle" + strings.Repeat("b", maxMatchTextBytes)
	writeTestFile(t, filepath.Join(root, "long.txt"), line+"\n")

	result, err := workspace.Search(context.Background(), "needle", "")
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(result.Matches) != 1 {
		t.Fatalf("Search() matches = %#v, want one match", result.Matches)
	}
	match := result.Matches[0]
	if !match.Truncated || len(match.Text) > maxMatchTextBytes || !strings.Contains(match.Text, "needle") {
		t.Fatalf("long match = %#v, want bounded preview containing query", match)
	}
}

func TestApplyPatchUsesExactContextAndRejectsUnsafeTargets(t *testing.T) {
	root, _ := testWorkspace(t)
	workspace, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	target := filepath.Join(root, "src", "main.go")
	writeTestFile(t, target, "package main\nold value\n")
	patch := "--- a/src/main.go\n+++ b/src/main.go\n@@ -1,2 +1,2 @@\n package main\n-old value\n+new value\n"
	path, err := workspace.ApplyPatch(patch)
	if err != nil {
		t.Fatalf("ApplyPatch() error = %v", err)
	}
	if path != "src/main.go" {
		t.Errorf("ApplyPatch() path = %q, want src/main.go", path)
	}
	if got := readTestFile(t, target); got != "package main\nnew value\n" {
		t.Errorf("patched file = %q", got)
	}

	before := readTestFile(t, target)
	for _, patch := range []string{
		"--- a/src/main.go\n+++ b/src/main.go\n@@ -1,2 +1,2 @@\n package main\n-wrong value\n+changed\n",
		"--- a/src/main.go\n+++ b/src/main.go\n@@ -1 +1 @@\n-new value\n+changed\n",
		"--- a/.env\n+++ b/.env\n@@ -1 +1 @@\n-old\n+new\n",
		"--- a/../outside\n+++ b/../outside\n@@ -1 +1 @@\n-old\n+new\n",
		"--- a//tmp/outside\n+++ b//tmp/outside\n@@ -1 +1 @@\n-old\n+new\n",
		"--- a/src/main.go\n+++ b/src/main.go\n@@ -1 +1 @@\n-old\n+new\n--- a/other.go\n+++ b/other.go\n@@ -1 +1 @@\n-old\n+new\n",
	} {
		if _, err := workspace.ApplyPatch(patch); !errors.Is(err, ErrRejected) {
			t.Errorf("ApplyPatch(%q) error = %v, want ErrRejected", patch, err)
		}
		if got := readTestFile(t, target); got != before {
			t.Fatalf("failed patch changed file: got %q, want %q", got, before)
		}
	}

	linkTarget := filepath.Join(root, "linked-target.txt")
	writeTestFile(t, linkTarget, "old\n")
	if err := os.Symlink(linkTarget, filepath.Join(root, "linked.txt")); err != nil {
		t.Fatalf("create internal symlink: %v", err)
	}
	linkPatch := "--- a/linked.txt\n+++ b/linked.txt\n@@ -1 +1 @@\n-old\n+new\n"
	if _, err := workspace.ApplyPatch(linkPatch); !errors.Is(err, ErrRejected) {
		t.Errorf("ApplyPatch(symlink) error = %v, want ErrRejected", err)
	}
	if got := readTestFile(t, linkTarget); got != "old\n" {
		t.Errorf("symlink target changed: %q", got)
	}
}

func TestApplyPatchHandlesLaterAndMultipleHunks(t *testing.T) {
	root, _ := testWorkspace(t)
	workspace, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	target := filepath.Join(root, "multi.txt")
	writeTestFile(t, target, "line1\nline2\nline3\nline4\nline5\nline6\n")
	patch := "--- a/multi.txt\n+++ b/multi.txt\n@@ -2,2 +2,2 @@\n line2\n-line3\n+LINE3\n@@ -5,2 +5,2 @@\n line5\n-line6\n+LINE6\n"

	if _, err := workspace.ApplyPatch(patch); err != nil {
		t.Fatalf("ApplyPatch(multi-hunk) error = %v", err)
	}
	if got, want := readTestFile(t, target), "line1\nline2\nLINE3\nline4\nline5\nLINE6\n"; got != want {
		t.Fatalf("patched file = %q, want %q", got, want)
	}
}

func TestApplyPatchSerializesConcurrentStalePatches(t *testing.T) {
	root, _ := testWorkspace(t)
	workspace, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	target := filepath.Join(root, "concurrent.txt")
	writeTestFile(t, target, "header\nold\nfooter\n")
	patchA := "--- a/concurrent.txt\n+++ b/concurrent.txt\n@@ -1,3 +1,3 @@\n header\n-old\n+A\n footer\n"
	patchB := "--- a/concurrent.txt\n+++ b/concurrent.txt\n@@ -1,3 +1,3 @@\n header\n-old\n+B\n footer\n"

	start := make(chan struct{})
	results := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for _, patch := range []string{patchA, patchB} {
		patch := patch
		go func() {
			ready.Done()
			<-start
			_, err := workspace.ApplyPatch(patch)
			results <- err
		}()
	}
	ready.Wait()
	close(start)

	successes := 0
	rejections := 0
	for range 2 {
		err := <-results
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrRejected):
			rejections++
		default:
			t.Fatalf("unexpected patch error: %v", err)
		}
	}
	if successes != 1 || rejections != 1 {
		t.Fatalf("concurrent patches: successes=%d rejections=%d, want 1/1", successes, rejections)
	}
	got := readTestFile(t, target)
	if got != "header\nA\nfooter\n" && got != "header\nB\nfooter\n" {
		t.Fatalf("concurrent patched file = %q", got)
	}
}

func testWorkspace(t *testing.T) (string, string) {
	t.Helper()
	return t.TempDir(), t.TempDir()
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	return string(content)
}
