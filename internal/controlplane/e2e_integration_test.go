package controlplane

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tienphat/m3-repoworker/internal/intelligence"
	"github.com/tienphat/m3-repoworker/internal/loop"
	"github.com/tienphat/m3-repoworker/internal/publication"
	"github.com/tienphat/m3-repoworker/internal/security"
)

func TestControlPlaneRealM3EndToEnd(t *testing.T) {
	if os.Getenv("REPOWORKER_REAL_E2E") != "1" {
		t.Skip("set REPOWORKER_REAL_E2E=1 to run the real Apple control-plane flow")
	}
	containerBinary, err := exec.LookPath("container")
	if err != nil {
		t.Skip("Apple container prerequisite missing: container CLI is unavailable")
	}
	if output, err := exec.Command(containerBinary, "machine", "list").CombinedOutput(); err != nil || !containsRunningMachine(string(output)) {
		t.Skip("Apple container prerequisite missing: run `container machine create --name repoworker alpine:3.22` and retry")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	root := t.TempDir()
	writeFixtureFile(t, filepath.Join(root, "go.mod"), "module example.invalid/controlplanee2e\n\ngo 1.25\n")
	writeFixtureFile(t, filepath.Join(root, "main.go"), "package main\n\nfunc main() {}\n")
	initFixtureGit(t, root)
	stateRoot := t.TempDir()
	image := os.Getenv("REPOWORKER_APPLE_VERIFY_IMAGE")
	if image == "" {
		image = "golang:1.25-alpine"
	}
	plane, err := Open(ctx, Config{RepositoryRoot: root, StateRoot: stateRoot, Image: image})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = plane.Close() })

	_, initialLive, err := plane.Repo.Read("main.go")
	if err != nil {
		t.Fatal(err)
	}
	record, err := plane.CreateWorkspace(ctx, "task_e2e")
	if err != nil {
		t.Fatalf("CreateWorkspace() error = %v", err)
	}
	runtimeRecord, err := plane.RuntimeCreate(ctx, record.Generation.ID, "", "apple-container", 1, 512<<20)
	if err != nil {
		t.Fatalf("RuntimeCreate() error = %v", err)
	}
	if _, err := plane.RuntimeStart(ctx, record.Generation.ID); err != nil {
		t.Fatalf("RuntimeStart() error = %v", err)
	}
	plan, err := plane.PlanVerification(ctx, record.Generation.ID, intelligence.Target{})
	if err != nil {
		t.Fatalf("PlanVerification() error = %v", err)
	}
	run, err := plane.StartLoop(ctx, "task_e2e", record.Generation.ID, runtimeRecord.ID, "--- a/main.go\n+++ b/main.go\n@@ -1,3 +1,3 @@\n package main\n \n-func main() {}\n+func main() { println(\"candidate\") }\n", plan)
	if err != nil {
		t.Fatalf("StartLoop() error = %v", err)
	}

	var status LoopStatus
	for ctx.Err() == nil {
		status, err = plane.LoopStatus(ctx, run.ID)
		if err != nil {
			t.Fatalf("LoopStatus() error = %v", err)
		}
		if status.State.Phase == loop.PhaseCompleted || status.State.Phase == loop.PhaseFailed || status.State.Phase == loop.PhaseHumanCheckpoint {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if status.State.Phase != loop.PhaseCompleted {
		t.Fatalf("loop state = %#v, run = %#v", status.State, status.Run)
	}
	currentRecord, err := plane.WorkspaceStatus(ctx, record.Generation.ID)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := os.ReadFile(filepath.Join(currentRecord.Generation.Path, "main.go"))
	if err != nil || string(candidate) == initialLive {
		t.Fatalf("candidate mutation missing: err=%v content=%q", err, candidate)
	}
	_, live, err := plane.Repo.Read("main.go")
	if err != nil || live != initialLive {
		t.Fatalf("live repository changed before integration: %q -> %q", initialLive, live)
	}

	finalPlan, err := plane.PlanVerification(ctx, record.Generation.ID, intelligence.Target{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := plane.RunVerification(ctx, finalPlan.PlanDigest, record.Generation.ID, runtimeRecord.ID)
	if err != nil || !result.Passed {
		t.Fatalf("final verification = %#v, error = %v", result, err)
	}
	publicationPlan, err := plane.PublicationPlan(ctx, record.Generation.ID, finalPlan.PlanDigest, publication.Request{Kind: publication.KindGitHubPR, Base: "main", Head: "candidate", Title: "verified candidate", Body: "plan only"})
	if err != nil || !publicationPlan.DryRun || len(publicationPlan.Commands) == 0 {
		t.Fatalf("publication plan = %#v, error = %v", publicationPlan, err)
	}
	if len(plane.Security.AuditEvents()) == 0 {
		t.Fatal("authenticated security audit is empty")
	}

	confirmation, err := plane.IssueConfirmation(ctx, security.ConfirmationDestructive)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := plane.Integrate(ctx, record.Generation.ID, confirmation.Token)

	if err != nil || journal.State == "" {
		t.Fatalf("integration journal = %#v, error = %v", journal, err)
	}
	_, live, err = plane.Repo.Read("main.go")
	if err != nil || live == initialLive {
		t.Fatalf("live repository did not change after verified integration: %q -> %q", initialLive, live)
	}
	if _, err := plane.RuntimeStop(ctx, record.Generation.ID); err != nil {
		t.Fatalf("RuntimeStop() error = %v", err)
	}
	if err := plane.DiscardWorkspace(ctx, record.Generation.ID); err != nil {
		t.Fatalf("DiscardWorkspace() error = %v", err)
	}
}

func containsRunningMachine(output string) bool {
	return len(output) > 0 && strings.Contains(output, "running")
}
