package controlplane

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/tienphat/m3-repoworker/internal/intelligence"
	"github.com/tienphat/m3-repoworker/internal/loop"
	"github.com/tienphat/m3-repoworker/internal/repo"
	m3runtime "github.com/tienphat/m3-repoworker/internal/runtime"
	"github.com/tienphat/m3-repoworker/internal/security"
)

func TestOpenBindsWorkspaceEnvironmentAndCandidateOnly(t *testing.T) {
	if _, err := exec.LookPath("container"); err != nil {
		t.Skip("Apple container CLI is unavailable")
	}
	root := t.TempDir()
	writeFixtureFile(t, filepath.Join(root, "go.mod"), "module example.invalid/controlplane\n\ngo 1.26.6\n")
	writeFixtureFile(t, filepath.Join(root, "main.go"), "package main\n\nfunc main() {}\n")
	initFixtureGit(t, root)

	provider, err := security.NewTrustedPrincipalProvider("controlplane-test")
	if err != nil {
		t.Fatal(err)
	}
	plane, err := Open(context.Background(), Config{RepositoryRoot: root, StateRoot: t.TempDir(), Image: "fixture-image", PrincipalProvider: provider})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = plane.Close() })

	before, err := plane.Repo.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	record, err := plane.CreateWorkspace(context.Background(), "task_fixture")
	if err != nil {
		t.Fatalf("CreateWorkspace() error = %v", err)
	}
	if record.Generation.Path == root || filepath.Dir(record.Generation.Path) == root {
		t.Fatalf("workspace path %q is not isolated from live root %q", record.Generation.Path, root)
	}
	plan, err := plane.PlanVerification(context.Background(), record.Generation.ID, intelligence.Target{})
	if err != nil {
		t.Fatalf("PlanVerification() error = %v", err)
	}
	if plan.EnvironmentID == "" || plan.EnvironmentID == "environment-pending" || plan.CandidateSnapshot != record.Generation.CandidateSnapshot {
		t.Fatalf("plan binding = %#v", plan)
	}
	after, err := plane.Repo.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if before.SnapshotID != after.SnapshotID {
		t.Fatalf("live snapshot changed before integration: %s -> %s", before.SnapshotID, after.SnapshotID)
	}

	candidate, err := repo.New(record.Generation.Path)
	if err != nil {
		t.Fatal(err)
	}
	patch := "--- a/main.go\n+++ b/main.go\n@@ -1,3 +1,3 @@\n package main\n \n-func main() {}\n+func main() { println(\"candidate\") }\n"
	if _, err := candidate.ApplyPatch(patch); err != nil {
		_ = candidate.Close()
		t.Fatalf("candidate patch = %v", err)
	}
	_ = candidate.Close()
	if err := plane.Repository.AssertGeneration(context.Background(), record.Generation, record.Lease); err != nil {
		t.Fatalf("AssertGeneration() before refresh = %v", err)
	}
	refreshed, err := plane.Repository.RefreshGeneration(context.Background(), record.Generation, record.Lease)
	if err != nil {
		t.Fatalf("RefreshGeneration() error = %v", err)
	}
	plane.mu.Lock()
	plane.workspaces[record.Generation.ID] = WorkspaceRecord{Generation: refreshed, Lease: record.Lease}
	plane.mu.Unlock()
	if _, err := plane.IntegrationPlan(context.Background(), record.Generation.ID); err != nil {
		t.Fatalf("IntegrationPlan() after candidate mutation = %v", err)
	}
	if _, err := plane.RunVerification(context.Background(), plan.PlanDigest, record.Generation.ID, "runtime_missing"); !errors.Is(err, ErrStale) {
		t.Fatalf("stale verification error = %v, want ErrStale", err)
	}
	if err := plane.DiscardWorkspace(context.Background(), record.Generation.ID); err != nil {
		t.Fatalf("DiscardWorkspace() error = %v", err)
	}
	if _, err := os.Stat(record.Generation.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("discarded workspace still exists: %v", err)
	}
}

func TestCloseOpenResumeLoopReprovisionsRuntime(t *testing.T) {
	root := t.TempDir()
	writeFixtureFile(t, filepath.Join(root, "go.mod"), "module example.invalid/reopen\n\ngo 1.26.6\n")
	writeFixtureFile(t, filepath.Join(root, "main.go"), "package main\n\nfunc main() {}\n")
	initFixtureGit(t, root)
	fakeBin := t.TempDir()
	stateFile := filepath.Join(fakeBin, "container-state")
	logFile := filepath.Join(fakeBin, "container.log")
	cacheRoot := filepath.Join(fakeBin, "go-cache")
	moduleCache := filepath.Join(fakeBin, "go-mod-cache")
	fakeContainer := filepath.Join(fakeBin, "container")
	script := fmt.Sprintf(`#!/bin/sh
set -eu
state=%q
log=%q
export HOME=%q
export GOCACHE=%q
export GOMODCACHE=%q
command="${1:-}"
if [ "$#" -gt 0 ]; then shift; fi
printf '%%s %%s\\n' "$command" "$*" >> "$log"
case "$command" in
create)
  workspace=""
  for argument in "$@"; do
    case "$argument" in
      source=*,target=/workspace*) workspace="${argument#source=}"; workspace="${workspace%%,target=/workspace*}" ;;
    esac
  done
  printf '%%s' "$workspace" > "$state"
  ;;
start|stop|rm)
  ;;
exec)
  while [ "$#" -gt 0 ]; do
    case "${1:-}" in
      --workdir|--env) shift 2 ;;
      *) break ;;
    esac
  done
  runtime="${1:-}"
  if [ -z "$runtime" ]; then exit 1; fi
  shift
  workspace=$(cat "$state")
  cd "$workspace"
  sleep 2
  set +e
  "$@" >> "$log" 2>&1
  status=$?
  set -e
  printf 'exec-exit %%s\\n' "$status" >> "$log"
  exit "$status"
  ;;
*)
  ;;
esac
	`, stateFile, logFile, fakeBin, cacheRoot, moduleCache)
	if err := os.WriteFile(fakeContainer, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	provider, err := security.NewTrustedPrincipalProvider("reopen-caller")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stateRoot := t.TempDir()
	first, err := Open(ctx, Config{RepositoryRoot: root, StateRoot: stateRoot, Image: "fixture-image", PrincipalProvider: provider})
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}
	record, err := first.CreateWorkspace(ctx, "task_reopen")
	if err != nil {
		first.Close()
		t.Fatalf("CreateWorkspace() error = %v", err)
	}
	runtimeRecord, err := first.RuntimeCreate(ctx, record.Generation.ID, "", "apple-container", 1, 128<<20)
	if err != nil {
		first.Close()
		t.Fatalf("RuntimeCreate() error = %v", err)
	}
	if _, err := first.RuntimeStart(ctx, record.Generation.ID); err != nil {
		first.Close()
		t.Fatalf("RuntimeStart() error = %v", err)
	}
	plan, err := first.PlanVerification(ctx, record.Generation.ID, intelligence.Target{})
	if err != nil {
		first.Close()
		t.Fatalf("PlanVerification() error = %v", err)
	}
	patch := "--- a/main.go\n+++ b/main.go\n@@ -1,3 +1,3 @@\n package main\n \n-func main() {}\n+func main() { println(\"reopened\") }\n"
	run, err := first.StartLoop(ctx, "task_reopen", record.Generation.ID, runtimeRecord.ID, patch, plan)
	if err != nil {
		first.Close()
		t.Fatalf("StartLoop() error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	second, err := Open(ctx, Config{RepositoryRoot: root, StateRoot: stateRoot, Image: "fixture-image", PrincipalProvider: provider})
	if err != nil {
		t.Fatalf("Open(second) error = %v", err)
	}
	status, err := second.ResumeLoop(ctx, run.ID)
	if err != nil {
		second.Close()
		t.Fatalf("ResumeLoop() error = %v", err)
	}
	if status.ID != run.ID || status.Status != "running" {
		second.Close()
		t.Fatalf("ResumeLoop() = %#v, want running same run", status)
	}
	deadline := time.Now().Add(15 * time.Second)
	var final LoopStatus
	for time.Now().Before(deadline) {
		final, err = second.LoopStatus(ctx, run.ID)
		if err != nil {
			second.Close()
			t.Fatal(err)
		}
		if final.State.Phase == loop.PhaseCompleted || final.State.Phase == loop.PhaseFailed || final.State.Phase == loop.PhaseHumanCheckpoint {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	defer second.Close()
	if final.State.Phase != loop.PhaseCompleted {
		log, _ := os.ReadFile(logFile)
		t.Fatalf("reopened loop state = %#v, run = %#v, container log = %s", final.State, final.Run, log)
	}
	recreated, err := second.RuntimeStatus(ctx, record.Generation.ID)
	if err != nil || recreated.ID == runtimeRecord.ID || recreated.LeaseGeneration <= runtimeRecord.LeaseGeneration || recreated.State != m3runtime.StateRunning {
		t.Fatalf("recreated runtime = %#v, old=%#v, error=%v", recreated, runtimeRecord, err)
	}
	if _, err := os.Stat(filepath.Join(record.Generation.Path, "main.go")); err != nil {
		t.Fatal(err)
	}
}

func writeFixtureFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func initFixtureGit(t *testing.T, root string) {
	t.Helper()
	for _, args := range [][]string{{"init", "-b", "main"}, {"config", "user.name", "RepoWorker Test"}, {"config", "user.email", "repoworker@example.invalid"}, {"add", "."}, {"commit", "-m", "initial"}} {
		command := exec.Command("git", append([]string{"-C", root}, args...)...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
}
