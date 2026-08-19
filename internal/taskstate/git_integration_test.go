package taskstate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestGitInspectorCreateAndResume(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	stateRoot := t.TempDir()
	runTestGit(t, repoRoot, "init", "-b", "main")
	runTestGit(t, repoRoot, "config", "user.name", "RepoWorker Test")
	runTestGit(t, repoRoot, "config", "user.email", "repoworker@example.invalid")
	writeGitTestFile(t, filepath.Join(repoRoot, "README.md"), "one\n")
	runTestGit(t, repoRoot, "add", "README.md")
	runTestGit(t, repoRoot, "commit", "-m", "initial")

	store, err := New(repoRoot, stateRoot)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	created, err := store.Create(context.Background(), "continue")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if created.Branch != "main" || created.BaseSHA == "" || created.CurrentHeadSHA != created.BaseSHA {
		t.Fatalf("created = %#v", created)
	}

	writeGitTestFile(t, filepath.Join(repoRoot, "README.md"), "two\n")
	runTestGit(t, repoRoot, "add", "README.md")
	runTestGit(t, repoRoot, "commit", "-m", "second")

	resumed, err := store.Resume(context.Background(), created.TaskID)
	if err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if resumed.CurrentHeadSHA == created.CurrentHeadSHA || resumed.VerificationState != "RED" {
		t.Fatalf("resumed = %#v", resumed)
	}
}

func runTestGit(t *testing.T, repoRoot string, args ...string) {
	t.Helper()
	commandArgs := append([]string{"-C", repoRoot}, args...)
	command := exec.Command("git", commandArgs...)
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}

func writeGitTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write git test file: %v", err)
	}
}
