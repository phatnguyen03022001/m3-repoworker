package controlplane

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/tienphat/m3-repoworker/internal/intelligence"
	"github.com/tienphat/m3-repoworker/internal/repo"
)

func TestOpenBindsWorkspaceEnvironmentAndCandidateOnly(t *testing.T) {
	if _, err := exec.LookPath("container"); err != nil {
		t.Skip("Apple container CLI is unavailable")
	}
	root := t.TempDir()
	writeFixtureFile(t, filepath.Join(root, "go.mod"), "module example.invalid/controlplane\n\ngo 1.26.6\n")
	writeFixtureFile(t, filepath.Join(root, "main.go"), "package main\n\nfunc main() {}\n")
	initFixtureGit(t, root)

	plane, err := Open(context.Background(), Config{RepositoryRoot: root, StateRoot: t.TempDir(), Image: "fixture-image"})
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
