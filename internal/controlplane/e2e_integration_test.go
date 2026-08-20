package controlplane

import (
	"context"
	"encoding/json"
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
	if os.Getenv("REPOWORKER_REAL_E2E") != "1" && os.Getenv("REPOWORKER_REAL_GATE") != "1" {
		t.Skip("set REPOWORKER_REAL_E2E=1 to run the real Apple control-plane flow")
	}
	containerBinary, err := exec.LookPath("container")
	if err != nil {
		t.Fatalf("NOT RUN: Apple container prerequisite missing: container CLI is unavailable")
	}
	if output, err := exec.Command(containerBinary, "machine", "list").CombinedOutput(); err != nil || !containsRunningMachine(string(output)) {
		t.Fatalf("NOT RUN: Apple container machine is unavailable: run `container machine create --name repoworker alpine:3.22` and retry (output: %s)", output)
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
	provider, err := security.NewTrustedPrincipalProvider("e2e-caller")
	if err != nil {
		t.Fatal(err)
	}
	operator, err := security.NewExplicitOperatorAuthority("e2e-operator")
	if err != nil {
		t.Fatal(err)
	}
	plane, err := Open(ctx, Config{RepositoryRoot: root, StateRoot: stateRoot, Image: image, PrincipalProvider: provider, OperatorAuthority: operator})
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
	// Persist a second running loop at its durable transition, stop the old
	// runtime, then close the entire plane. Reopen must recover the workspace,
	// provision a fresh runtime generation, and resume this run.
	if _, err := plane.RuntimeStop(ctx, record.Generation.ID); err != nil {
		t.Fatalf("RuntimeStop(before reopen) error = %v", err)
	}
	recoveryRun, err := plane.EventsCreate(ctx, "task_e2e_reopen", record.Generation.ID, finalPlan.EnvironmentID)
	if err != nil {
		t.Fatalf("EventsCreate(recovery) error = %v", err)
	}
	recoveryConfig, err := json.Marshal(loopConfig{GenerationID: record.Generation.ID, RuntimeID: runtimeRecord.ID, Target: finalPlan.Target})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plane.Events.AppendEvent(ctx, recoveryRun.ID, "loop.config", string(recoveryConfig)); err != nil {
		t.Fatalf("AppendEvent(recovery config) error = %v", err)
	}
	if err := plane.Events.UpdateRunStatus(ctx, recoveryRun.ID, "running"); err != nil {
		t.Fatalf("UpdateRunStatus(recovery) error = %v", err)
	}
	if err := plane.Close(); err != nil {
		t.Fatalf("Close(before reopen) error = %v", err)
	}
	plane, err = Open(ctx, Config{RepositoryRoot: root, StateRoot: stateRoot, Image: image, PrincipalProvider: provider, OperatorAuthority: operator})
	if err != nil {
		t.Fatalf("Open(recovery) error = %v", err)
	}
	resumed, err := plane.ResumeLoop(ctx, recoveryRun.ID)
	if err != nil || resumed.Status != "running" {
		t.Fatalf("ResumeLoop(recovery) = %#v, error = %v", resumed, err)
	}
	var recoveredStatus LoopStatus
	for ctx.Err() == nil {
		recoveredStatus, err = plane.LoopStatus(ctx, recoveryRun.ID)
		if err != nil {
			t.Fatalf("LoopStatus(recovery) error = %v", err)
		}
		if recoveredStatus.State.Phase == loop.PhaseCompleted || recoveredStatus.State.Phase == loop.PhaseFailed || recoveredStatus.State.Phase == loop.PhaseHumanCheckpoint {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if recoveredStatus.State.Phase != loop.PhaseCompleted {
		t.Fatalf("recovered loop state = %#v, run = %#v", recoveredStatus.State, recoveredStatus.Run)
	}
	currentRuntime, err := plane.RuntimeStatus(ctx, record.Generation.ID)
	if err != nil || currentRuntime.ID == runtimeRecord.ID || currentRuntime.LeaseGeneration <= runtimeRecord.LeaseGeneration || currentRuntime.State != "RUNNING" {
		t.Fatalf("recovered runtime = %#v, old=%#v, error=%v", currentRuntime, runtimeRecord, err)
	}
	runtimeRecord = currentRuntime
	record, err = plane.WorkspaceStatus(ctx, record.Generation.ID)
	if err != nil {
		t.Fatal(err)
	}
	finalPlan, err = plane.PlanVerification(ctx, record.Generation.ID, intelligence.Target{})
	if err != nil {
		t.Fatal(err)
	}
	result, err = plane.RunVerification(ctx, finalPlan.PlanDigest, record.Generation.ID, runtimeRecord.ID)
	if err != nil || !result.Passed {
		t.Fatalf("post-recovery verification = %#v, error = %v", result, err)
	}
	publicationPlan, err := plane.PublicationPlan(ctx, record.Generation.ID, finalPlan.PlanDigest, publication.Request{Kind: publication.KindGitHubPR, Base: "main", Head: "candidate", Title: "verified candidate", Body: "plan only"})
	if err != nil || !publicationPlan.DryRun || len(publicationPlan.Commands) == 0 {
		t.Fatalf("publication plan = %#v, error = %v", publicationPlan, err)
	}
	if len(plane.Security.AuditEvents()) == 0 {
		t.Fatal("authenticated security audit is empty")
	}

	confirmationBinding, err := plane.IntegrationConfirmationBinding(ctx, record.Generation.ID)
	if err != nil {
		t.Fatal(err)
	}
	confirmation, err := plane.IssueOperatorConfirmation(ctx, security.OperatorConfirmationRequest{Binding: confirmationBinding, Class: security.ConfirmationDestructive, TTL: 10 * time.Minute})
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
