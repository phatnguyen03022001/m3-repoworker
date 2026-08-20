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
	"github.com/tienphat/m3-repoworker/internal/process"
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
	shellProcess, err := plane.ProcessStart(ctx, record.Generation.ID, runtimeRecord.ID, "sh", []string{"-lc", "printf shell-ok && printf ' PATH=%s\\n' \"$PATH\" && command -v go && go test ./... && printf a | tr a b > shell-output && test ! -e /Users/repoworker-host-path"}, "/workspace", 2*time.Minute)
	if err != nil {
		t.Fatalf("ProcessStart(shell) error = %v", err)
	}
	shellOutcome, err := plane.ProcessWait(ctx, shellProcess)
	shellChunks, err := plane.ProcessRead(ctx, shellProcess, 0, 128)
	if err != nil || shellOutcome.ExitCode != 0 {
		t.Fatalf("shell outcome = %#v, error = %v, output = %q", shellOutcome, err, processChunksText(shellChunks))
	}
	if err != nil || !strings.Contains(processChunksText(shellChunks), "shell-ok") {
		t.Fatalf("shell output = %#v, error = %v", shellChunks, err)
	}
	if candidateOutput, readErr := os.ReadFile(filepath.Join(record.Generation.Path, "shell-output")); readErr != nil || string(candidateOutput) != "b" {
		t.Fatalf("shell candidate output = %q, error = %v", candidateOutput, readErr)
	}
	if _, err := os.Stat(filepath.Join(root, "shell-output")); !os.IsNotExist(err) {
		t.Fatalf("shell mutation escaped into live repository: %v", err)
	}
	t.Log("shell success and candidate isolation passed")
	failingShell, err := plane.ProcessStart(ctx, record.Generation.ID, runtimeRecord.ID, "sh", []string{"-lc", "printf shell-failure >&2; exit 23"}, "/workspace", time.Minute)
	if err != nil {
		t.Fatalf("ProcessStart(failing shell) error = %v", err)
	}
	failingOutcome, err := plane.ProcessWait(ctx, failingShell)
	if err != nil || failingOutcome.ExitCode != 23 {
		t.Fatalf("failing shell outcome = %#v, error = %v", failingOutcome, err)
	}
	t.Log("failing shell exit status passed")
	timeoutShell, err := plane.ProcessStart(ctx, record.Generation.ID, runtimeRecord.ID, "sh", []string{"-lc", "sleep 30 & wait"}, "/workspace", 100*time.Millisecond)
	if err != nil {
		t.Fatalf("ProcessStart(timeout shell) error = %v", err)
	}
	timeoutOutcome, err := plane.ProcessWait(ctx, timeoutShell)
	if err != nil || !timeoutOutcome.TimedOut {
		t.Fatalf("timeout shell outcome = %#v, error = %v", timeoutOutcome, err)
	}
	t.Log("timeout shell cleanup passed")
	t.Log("planning verification")
	plan, err := plane.PlanVerification(ctx, record.Generation.ID, intelligence.Target{})
	if err != nil {
		t.Fatalf("PlanVerification() error = %v", err)
	}
	t.Log("verification plan ready")
	run, err := plane.StartLoop(ctx, "task_e2e", record.Generation.ID, runtimeRecord.ID, "--- a/main.go\n+++ b/main.go\n@@ -1,3 +1,3 @@\n package main\n \n-func main() {}\n+func main() { println(\"candidate\") }\n", plan)
	if err != nil {
		t.Fatalf("StartLoop() error = %v", err)
	}
	t.Log("loop started")

	var status LoopStatus
	for ctx.Err() == nil {
		status, err = plane.LoopStatus(ctx, run.ID)
		if err != nil {
			t.Fatalf("LoopStatus() error = %v", err)
		}
		if status.State.Phase == loop.PhaseCompleted || status.State.Phase == loop.PhaseFailed || status.State.Phase == loop.PhaseHumanCheckpoint || status.Run.Status == "failed" || status.Run.Status == "stopped" {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if status.State.Phase != loop.PhaseCompleted {
		events, _ := plane.Events.ListEvents(ctx, run.ID, 0, 100)
		t.Fatalf("loop state = %#v, run = %#v, events = %#v", status.State, status.Run, events)
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
		if recoveredStatus.State.Phase == loop.PhaseCompleted || recoveredStatus.State.Phase == loop.PhaseFailed || recoveredStatus.State.Phase == loop.PhaseHumanCheckpoint || recoveredStatus.Run.Status == "failed" || recoveredStatus.Run.Status == "stopped" {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if recoveredStatus.State.Phase != loop.PhaseCompleted {
		events, _ := plane.Events.ListEvents(ctx, recoveryRun.ID, 0, 100)
		t.Fatalf("recovered loop state = %#v, run = %#v, events = %#v", recoveredStatus.State, recoveredStatus.Run, events)
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

func processChunksText(chunks []process.Chunk) string {
	var builder strings.Builder
	for _, chunk := range chunks {
		builder.WriteString(chunk.Data)
	}
	return builder.String()
}
