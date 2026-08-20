package repo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
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

func TestCapabilityFDsAreCloseOnExec(t *testing.T) {
	root, _ := testWorkspace(t)
	workspace, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer workspace.Close()
	writeTestFile(t, filepath.Join(root, "file.txt"), "ok\n")
	file, _, err := workspace.openExistingRelative("file.txt", false)
	if err != nil {
		t.Fatalf("openExistingRelative() error = %v", err)
	}
	defer file.Close()
	for _, fd := range []uintptr{workspace.rootDir.Fd(), file.Fd()} {
		flags, err := unix.FcntlInt(fd, unix.F_GETFD, 0)
		if err != nil {
			t.Fatalf("F_GETFD error = %v", err)
		}
		if flags&unix.FD_CLOEXEC == 0 {
			t.Fatalf("fd %d missing FD_CLOEXEC", fd)
		}
	}
}

func TestGitStatusIsTypedDeterministicAndReadOnly(t *testing.T) {
	root := t.TempDir()
	for _, args := range [][]string{{"init", "-b", "main"}, {"config", "user.name", "RepoWorker Test"}, {"config", "user.email", "repoworker@example.invalid"}, {"commit", "--allow-empty", "-m", "initial"}} {
		command := exec.Command("git", append([]string{"-C", root}, args...)...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	workspace, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer workspace.Close()
	clean, err := workspace.GitStatus(context.Background())
	if err != nil {
		t.Fatalf("GitStatus(clean) error = %v", err)
	}
	if clean.Branch != "main" || len(clean.Head) != 40 || clean.Dirty || clean.ChangedCount != 0 || clean.Truncated || len(clean.ChangedPaths) != 0 || clean.RepositoryIdentity == "" || clean.TrustedRoot != clean.RepositoryIdentity {
		t.Fatalf("clean GitStatus() = %#v", clean)
	}
	if err := os.WriteFile(filepath.Join(root, "z.txt"), []byte("z\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dirty, err := workspace.GitStatus(context.Background())
	if err != nil {
		t.Fatalf("GitStatus(dirty) error = %v", err)
	}
	if !dirty.Dirty || dirty.ChangedCount != 2 || dirty.Truncated || strings.Join(dirty.ChangedPaths, ",") != "?? a.txt,?? z.txt" {
		t.Fatalf("dirty GitStatus() = %#v, want deterministic changed paths", dirty)
	}
}

func TestGitStatusRenameAndBoundedChangedPathsWithoutMutation(t *testing.T) {
	root := t.TempDir()
	for _, args := range [][]string{{"init", "-b", "main"}, {"config", "user.name", "RepoWorker Test"}, {"config", "user.email", "repoworker@example.invalid"}, {"commit", "--allow-empty", "-m", "initial"}} {
		if output, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	writeTestFile(t, filepath.Join(root, "old.txt"), "rename me\n")
	if output, err := exec.Command("git", "-C", root, "add", "old.txt").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, output)
	}
	if output, err := exec.Command("git", "-C", root, "commit", "-m", "add old file").CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, output)
	}
	if output, err := exec.Command("git", "-C", root, "mv", "old.txt", "renamed.txt").CombinedOutput(); err != nil {
		t.Fatalf("git mv: %v: %s", err, output)
	}
	indexBefore, err := os.ReadFile(filepath.Join(root, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close()
	rename, err := workspace.GitStatus(context.Background())
	if err != nil {
		t.Fatalf("GitStatus(rename) error = %v", err)
	}
	if !rename.Dirty || rename.ChangedCount != 1 || rename.Truncated || len(rename.ChangedPaths) != 1 || !strings.HasPrefix(rename.ChangedPaths[0], "R  renamed.txt") {
		t.Fatalf("rename GitStatus() = %#v", rename)
	}
	indexAfter, err := os.ReadFile(filepath.Join(root, ".git", "index"))
	if err != nil || string(indexAfter) != string(indexBefore) {
		t.Fatalf("GitStatus mutated the Git index: read error=%v equal=%v", err, string(indexAfter) == string(indexBefore))
	}

	for index := 0; index < maxGitChangedPathEntries+40; index++ {
		writeTestFile(t, filepath.Join(root, "many", fmt.Sprintf("file-%04d.txt", index)), "x\n")
	}
	bounded, err := workspace.GitStatus(context.Background())
	if err != nil {
		t.Fatalf("GitStatus(many) error = %v", err)
	}
	if !bounded.Dirty || bounded.ChangedCount != maxGitChangedPathEntries+41 || !bounded.Truncated || len(bounded.ChangedPaths) > maxGitChangedPathEntries {
		t.Fatalf("bounded GitStatus() = %#v", bounded)
	}
	returnedBytes := 0
	for _, path := range bounded.ChangedPaths {
		returnedBytes += len(path)
	}
	if returnedBytes > maxGitChangedPathBytes {
		t.Fatalf("returned changed paths = %d bytes, want <= %d", returnedBytes, maxGitChangedPathBytes)
	}
	again, err := workspace.GitStatus(context.Background())
	if err != nil || strings.Join(again.ChangedPaths, "\x00") != strings.Join(bounded.ChangedPaths, "\x00") || again.ChangedCount != bounded.ChangedCount || again.Truncated != bounded.Truncated {
		t.Fatalf("bounded GitStatus is not deterministic: first=%#v second=%#v error=%v", bounded, again, err)
	}
}

func TestParsePorcelainStatusRejectsMalformedOutput(t *testing.T) {
	for _, output := range [][]byte{
		[]byte("not a branch\x00?? file\x00"),
		[]byte("## main\x00X? file\x00"),
		[]byte("## main\x00?? bad\npath\x00"),
	} {
		if _, _, _, err := parsePorcelainStatus(output); !errors.Is(err, ErrRejected) {
			t.Fatalf("parsePorcelainStatus(%q) error = %v, want rejection", output, err)
		}
	}
}

func TestGitStatusFailsClosedForDetachedHead(t *testing.T) {
	root := t.TempDir()
	for _, args := range [][]string{{"init", "-b", "main"}, {"config", "user.name", "RepoWorker Test"}, {"config", "user.email", "repoworker@example.invalid"}, {"commit", "--allow-empty", "-m", "initial"}} {
		command := exec.Command("git", append([]string{"-C", root}, args...)...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	workspace, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close()
	if output, err := exec.Command("git", "-C", root, "checkout", "--detach", "HEAD").CombinedOutput(); err != nil {
		t.Fatalf("detach: %v: %s", err, output)
	}
	if _, err := workspace.GitStatus(context.Background()); !errors.Is(err, ErrRejected) {
		t.Fatalf("GitStatus(detached) error = %v, want rejection", err)
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

func TestReadRejectsFIFOWithoutBlocking(t *testing.T) {
	root, _ := testWorkspace(t)
	workspace, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer workspace.Close()
	fifo := filepath.Join(root, "pipe")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("Mkfifo() error = %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := workspace.Read("pipe")
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrRejected) {
			t.Fatalf("Read(FIFO) error = %v, want ErrRejected", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Read(FIFO) blocked")
	}
}

func TestReadRemainsBoundToOpenedRootAfterRename(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "repo")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	writeTestFile(t, filepath.Join(root, "identity.txt"), "original\n")
	workspace, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	identity := workspace.RootIdentity()

	moved := filepath.Join(parent, "repo-moved")
	if err := os.Rename(root, moved); err != nil {
		t.Fatalf("rename repo: %v", err)
	}
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("recreate old path: %v", err)
	}
	writeTestFile(t, filepath.Join(root, "identity.txt"), "replacement\n")

	_, content, err := workspace.Read("identity.txt")
	if err != nil {
		t.Fatalf("Read() after rename error = %v", err)
	}
	if content != "original\n" {
		t.Fatalf("Read() after rename = %q, want original root content", content)
	}
	if workspace.RootIdentity() != identity {
		t.Fatalf("root identity changed after rename")
	}
}

func TestPathBasedOperationsRejectReplacementRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "repo")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	writeTestFile(t, filepath.Join(root, "target.txt"), "original\n")
	workspace, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	moved := filepath.Join(parent, "repo-moved")
	if err := os.Rename(root, moved); err != nil {
		t.Fatalf("rename repo: %v", err)
	}
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("recreate old path: %v", err)
	}
	replacement := filepath.Join(root, "target.txt")
	writeTestFile(t, replacement, "replacement\n")
	result, err := workspace.Search(context.Background(), "original", "")
	if err != nil {
		t.Fatalf("Search() after root replacement error = %v", err)
	}
	if len(result.Matches) != 1 || result.Matches[0].Path != "target.txt" {
		t.Fatalf("Search() after root replacement = %#v, want original repo match", result)
	}
	patch := "--- a/target.txt\n+++ b/target.txt\n@@ -1 +1 @@\n-original\n+changed\n"
	if _, err := workspace.ApplyPatch(patch); err != nil {
		t.Fatalf("ApplyPatch() after root replacement error = %v", err)
	}
	if got := readTestFile(t, replacement); got != "replacement\n" {
		t.Fatalf("replacement repo modified: %q", got)
	}
	if got := readTestFile(t, filepath.Join(moved, "target.txt")); got != "changed\n" {
		t.Fatalf("original repo not patched: %q", got)
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

func TestSnapshotManifestIsDeterministicAndOmitsUnavailablePaths(t *testing.T) {
	root, outside := testWorkspace(t)
	workspace, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer workspace.Close()
	writeTestFile(t, filepath.Join(root, "src", "main.go"), "package main\n")
	writeTestFile(t, filepath.Join(root, ".env"), "hidden\n")
	writeTestFile(t, filepath.Join(root, ".cache", "go-build", "cache.txt"), "generated-marker\n")
	writeTestFile(t, filepath.Join(root, "bin", "repoworker"), "generated-marker\n")
	writeTestFile(t, filepath.Join(root, ".github", "workflows", "test.yml"), "visible-marker\n")
	writeTestFile(t, filepath.Join(root, "tools", "bin", "repoworker"), "visible-marker\n")
	writeTestFile(t, filepath.Join(outside, "outside.txt"), "outside\n")
	if err := os.Symlink(filepath.Join(outside, "outside.txt"), filepath.Join(root, ".cache", "escape")); err != nil {
		t.Fatalf("Symlink(excluded cache) error = %v", err)
	}

	for _, path := range []string{".cache/go-build/cache.txt", "bin/repoworker"} {
		if _, _, err := workspace.Read(path); !errors.Is(err, ErrRejected) {
			t.Fatalf("Read(%q) error = %v, want ErrRejected", path, err)
		}
	}
	if _, content, err := workspace.Read("tools/bin/repoworker"); err != nil || content != "visible-marker\n" {
		t.Fatalf("Read(nested bin) = %q, %v, want visible source", content, err)
	}
	hidden, err := workspace.Search(context.Background(), "generated-marker", "")
	if err != nil {
		t.Fatalf("Search(generated) error = %v", err)
	}
	if len(hidden.Matches) != 0 || hidden.Truncated {
		t.Fatalf("Search(generated) = %#v, want no matches without truncation", hidden)
	}
	visible, err := workspace.Search(context.Background(), "visible-marker", "")
	if err != nil {
		t.Fatalf("Search(visible) error = %v", err)
	}
	if len(visible.Matches) != 2 || visible.Truncated {
		t.Fatalf("Search(visible) = %#v, want two valid source matches", visible)
	}

	first, err := workspace.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	second, err := workspace.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot() second error = %v", err)
	}
	if first.SnapshotID == "" || first.SnapshotID != second.SnapshotID {
		t.Fatalf("snapshot ids = %q / %q, want stable non-empty id", first.SnapshotID, second.SnapshotID)
	}
	entries := make(map[string]SnapshotEntry, len(first.Entries))
	for _, entry := range first.Entries {
		entries[entry.Path] = entry
	}
	if _, ok := entries[".env"]; ok {
		t.Fatal("protected .env unexpectedly present in snapshot")
	}
	for path := range entries {
		if path == ".cache" || strings.HasPrefix(path, ".cache/") || path == "bin/repoworker" {
			t.Fatalf("generated path %q unexpectedly present in snapshot", path)
		}
	}
	for _, path := range []string{".github", ".github/workflows/test.yml", "tools/bin/repoworker"} {
		if _, ok := entries[path]; !ok {
			t.Fatalf("valid path %q missing from snapshot", path)
		}
	}
	if entry, ok := entries["src"]; !ok || entry.Type != "directory" {
		t.Fatalf("src entry = %#v, want directory", entry)
	}
	if entry, ok := entries["src/main.go"]; !ok || entry.Type != "regular" || len(entry.Digest) != 64 || entry.Size == 0 {
		t.Fatalf("src/main.go entry = %#v, want hashed regular file", entry)
	}

	writeTestFile(t, filepath.Join(root, "src", "main.go"), "package changed\n")
	third, err := workspace.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot() after change error = %v", err)
	}
	if third.SnapshotID == first.SnapshotID {
		t.Fatal("snapshot id did not change after file content changed")
	}
}

func TestSnapshotRejectsSymlink(t *testing.T) {
	root, outside := testWorkspace(t)
	workspace, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer workspace.Close()
	writeTestFile(t, filepath.Join(outside, "outside.txt"), "outside\n")
	if err := os.Symlink(filepath.Join(outside, "outside.txt"), filepath.Join(root, "escape")); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	if _, err := workspace.Snapshot(context.Background()); !errors.Is(err, ErrRejected) {
		t.Fatalf("Snapshot(symlink) error = %v, want ErrRejected", err)
	}
}

func TestSnapshotRejectsSpecialFileOutsideExclusions(t *testing.T) {
	root, _ := testWorkspace(t)
	workspace, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer workspace.Close()
	if err := unix.Mkfifo(filepath.Join(root, "pipe"), 0o600); err != nil {
		t.Fatalf("Mkfifo() error = %v", err)
	}
	if _, err := workspace.Snapshot(context.Background()); !errors.Is(err, ErrRejected) {
		t.Fatalf("Snapshot(FIFO) error = %v, want ErrRejected", err)
	}
}

func TestCreateFileIsNoOverwriteAndIdentityBound(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "repo")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	workspace, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer workspace.Close()

	moved := filepath.Join(parent, "repo-moved")
	if err := os.Rename(root, moved); err != nil {
		t.Fatalf("rename repo: %v", err)
	}
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("recreate old path: %v", err)
	}
	path, err := workspace.CreateFile("new.txt", "created\n")
	if err != nil || path != "new.txt" {
		t.Fatalf("CreateFile() = (%q, %v), want new.txt", path, err)
	}
	if got := readTestFile(t, filepath.Join(moved, "new.txt")); got != "created\n" {
		t.Fatalf("created file in original repo = %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, "new.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement repo unexpectedly received new.txt: %v", err)
	}
	if _, err := workspace.CreateFile("new.txt", "overwrite\n"); !errors.Is(err, ErrRejected) {
		t.Fatalf("CreateFile(existing) error = %v, want ErrRejected", err)
	}
	if got := readTestFile(t, filepath.Join(moved, "new.txt")); got != "created\n" {
		t.Fatalf("existing file overwritten: %q", got)
	}
	if _, err := workspace.CreateFile(".env", "hidden\n"); !errors.Is(err, ErrRejected) {
		t.Fatalf("CreateFile(.env) error = %v, want ErrRejected", err)
	}
}

func TestDeleteFileConfinesAndProtectsTargets(t *testing.T) {
	root, outside := testWorkspace(t)
	workspace, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer workspace.Close()

	writeTestFile(t, filepath.Join(root, "delete.txt"), "remove\n")
	writeTestFile(t, filepath.Join(root, ".env"), "protected\n")
	writeTestFile(t, filepath.Join(outside, "outside.txt"), "outside\n")
	if err := os.Symlink(filepath.Join(outside, "outside.txt"), filepath.Join(root, "outside-link")); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}

	path, err := workspace.DeleteFile("delete.txt")
	if err != nil || path != "delete.txt" {
		t.Fatalf("DeleteFile() = (%q, %v), want delete.txt", path, err)
	}
	if _, err := os.Stat(filepath.Join(root, "delete.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted file still exists: %v", err)
	}
	for _, path := range []string{".env", "outside-link", "../outside.txt"} {
		if _, err := workspace.DeleteFile(path); !errors.Is(err, ErrRejected) {
			t.Fatalf("DeleteFile(%q) error = %v, want ErrRejected", path, err)
		}
	}
	if got := readTestFile(t, filepath.Join(outside, "outside.txt")); got != "outside\n" {
		t.Fatalf("outside file changed: %q", got)
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
