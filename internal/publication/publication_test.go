package publication

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var testCandidate = Candidate{RepositoryRoot: "/tmp/repository", CandidateSnapshot: strings.Repeat("a", 64), VerifiedSnapshot: strings.Repeat("a", 64), EnvironmentID: "environment_1", PolicyVersion: "policy_1", Verified: true}

type fakeRunner struct {
	commands []Command
	output   string
}

func (r *fakeRunner) Run(_ context.Context, _ string, executable string, args ...string) (string, error) {
	r.commands = append(r.commands, Command{Executable: executable, Arguments: append([]string(nil), args...)})
	return r.output, nil
}

func TestPublicationDryRunAndAuthorization(t *testing.T) {
	runner := &fakeRunner{}
	adapter, err := New(Gate{}, runner, func(context.Context, string) (string, error) { return testCandidate.CandidateSnapshot, nil })
	if err != nil {
		t.Fatal(err)
	}
	request := Request{Kind: KindGitHubPR, DryRun: true, Base: "main", Head: "candidate", Title: "test PR", Body: "verified candidate"}
	result, err := adapter.Publish(context.Background(), testCandidate, request)
	if err != nil || !result.DryRun || len(runner.commands) != 0 || result.Commands[0].Executable != "gh" {
		t.Fatalf("dry-run = %#v commands=%#v err=%v", result, runner.commands, err)
	}
	jjResult, err := adapter.Publish(context.Background(), testCandidate, Request{Kind: KindJJCheckpoint, DryRun: true, Title: "jj checkpoint"})
	if err != nil || jjResult.Commands[0].Executable != "jj" {
		t.Fatalf("jj dry-run = %#v, %v", jjResult, err)
	}
	request.DryRun = false
	if _, err := adapter.Publish(context.Background(), testCandidate, request); !errors.Is(err, ErrDisabled) {
		t.Fatalf("disabled error = %v", err)
	}
	adapter, err = New(Gate{Enabled: true, AllowExternalMutation: true, ConfirmationToken: "confirm"}, runner, func(context.Context, string) (string, error) { return testCandidate.CandidateSnapshot, nil })
	if err != nil {
		t.Fatal(err)
	}
	request.ConfirmationToken = "wrong"
	if _, err := adapter.Publish(context.Background(), testCandidate, request); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong confirmation = %v", err)
	}
	request.ConfirmationToken = "confirm"
	if _, err := adapter.Publish(context.Background(), testCandidate, request); err != nil {
		t.Fatalf("authorized PR = %v", err)
	}
}

func TestGitPushRechecksLocalBareRemote(t *testing.T) {
	root := t.TempDir()
	remote := filepath.Join(t.TempDir(), "remote.git")
	runGit(t, root, "init", "-q", "-b", "main")
	runGit(t, root, "config", "user.name", "RepoWorker Test")
	runGit(t, root, "config", "user.email", "repoworker-test@example.invalid")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("candidate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "README.md")
	runGit(t, root, "commit", "-q", "-m", "initial")
	runGit(t, remote, "init", "-q", "--bare")
	runGit(t, root, "remote", "add", "origin", remote)
	candidate := testCandidate
	candidate.RepositoryRoot = root
	runner := OSRunner{}
	adapter, err := New(Gate{Enabled: true, AllowExternalMutation: true, ConfirmationToken: "confirm"}, runner, func(context.Context, string) (string, error) { return candidate.CandidateSnapshot, nil })
	if err != nil {
		t.Fatal(err)
	}
	request := Request{Kind: KindGitPush, DryRun: true, Remote: "origin", Ref: "main", ExpectedRemoteRef: ""}
	result, err := adapter.Publish(context.Background(), candidate, request)
	if err != nil || !result.DryRun || result.RemoteBefore != "" {
		t.Fatalf("dry push = %#v, %v", result, err)
	}
	request.DryRun = false
	request.ConfirmationToken = "confirm"
	result, err = adapter.Publish(context.Background(), candidate, request)
	if err != nil || len(result.RemoteAfter) != 40 {
		t.Fatalf("push = %#v, %v", result, err)
	}
	request.ExpectedRemoteRef = strings.Repeat("0", 40)
	if _, err := adapter.Publish(context.Background(), candidate, request); !errors.Is(err, ErrStale) {
		t.Fatalf("stale push = %v", err)
	}
}

func TestPublicationRejectsStaleCandidateAndSecrets(t *testing.T) {
	runner := &fakeRunner{}
	adapter, err := New(Gate{Enabled: true, AllowExternalMutation: true, ConfirmationToken: "confirm"}, runner, func(context.Context, string) (string, error) { return strings.Repeat("b", 64), nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Publish(context.Background(), testCandidate, Request{Kind: KindGitHubPR, DryRun: true, Base: "main", Head: "candidate", Title: "ok", Body: "ok"}); !errors.Is(err, ErrStale) {
		t.Fatalf("stale candidate = %v", err)
	}
	request := Request{Kind: KindGitHubPR, DryRun: true, Base: "main", Head: "candidate", Title: "ok", Body: "Authorization: Bearer hidden"}
	adapter, _ = New(Gate{}, runner, func(context.Context, string) (string, error) { return testCandidate.CandidateSnapshot, nil })
	if _, err := adapter.Publish(context.Background(), testCandidate, request); !errors.Is(err, ErrRejected) {
		t.Fatalf("secret body = %v", err)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	command := exec.Command("git", args...)
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}
