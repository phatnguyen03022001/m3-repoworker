package runtime

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tienphat/m3-repoworker/internal/process"
	"github.com/tienphat/m3-repoworker/internal/security"
	"github.com/tienphat/m3-repoworker/internal/workspace"
)

func TestAppleContainerRealLifecycle(t *testing.T) {
	binary, err := exec.LookPath("container")
	if err != nil {
		t.Fatalf("NOT RUN: Apple container prerequisite missing: install/container CLI is unavailable")
	}
	machineOutput, err := exec.Command(binary, "machine", "list").CombinedOutput()
	if err != nil || !strings.Contains(string(machineOutput), "running") {
		t.Fatalf("NOT RUN: Apple container machine is unavailable: run `container machine create --name repoworker alpine:3.22` and retry (output: %s)", machineOutput)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	liveRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(liveRoot, "fixture.txt"), []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stateRoot := t.TempDir()
	repository, err := workspace.OpenRepository(liveRoot, stateRoot)
	if err != nil {
		t.Fatalf("OpenRepository() error = %v", err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	generation, err := repository.Materialize(ctx)
	if err != nil {
		t.Fatalf("Materialize() error = %v", err)
	}
	lease, err := repository.AcquireLease(ctx, generation.ID, "apple-integration", 10*time.Minute)
	if err != nil {
		t.Fatalf("AcquireLease() error = %v", err)
	}

	manager, err := NewManager(repository, filepath.Join(stateRoot, "runtimes"), AppleContainerAdapter{Binary: binary}, LimaAdapter{})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.ReserveRuntime(ctx, lease, "apple-integration"); err != nil {
		t.Fatalf("ReserveRuntime() error = %v", err)
	}
	containerID := ""
	t.Cleanup(func() {
		if containerID != "" {
			_ = exec.Command(binary, "stop", containerID).Run()
			_ = exec.Command(binary, "delete", containerID).Run()
		}
	})
	runtimeRecord, err := manager.Create(ctx, RuntimeSpec{TaskID: "task_apple_integration", Generation: generation, Lease: lease, WorkspacePath: generation.Path, LiveRepositoryPath: liveRoot, Image: "alpine:3.22", CPU: 1, MemoryBytes: 256 << 20, Network: security.NetworkNone}, "apple-container")
	if err != nil {
		t.Fatalf("real container create error = %v", err)
	}
	containerID = runtimeRecord.ExternalID
	staleLease := lease
	staleLease.FencingGeneration++
	if _, err := manager.Start(ctx, generation.ID, staleLease); err == nil {
		t.Fatal("stale lease unexpectedly started Apple container")
	}
	started, err := manager.Start(ctx, generation.ID, lease)
	if err != nil {
		t.Fatalf("real container start error = %v", err)
	}

	starter := process.ContainerStarter{Binary: binary, Resolve: func(context.Context, string) (string, error) { return started.ExternalID, nil }}
	supervisor, err := process.New(starter, filepath.Join(stateRoot, "spill"))
	if err != nil {
		t.Fatal(err)
	}
	echo, err := supervisor.Start(ctx, process.ProcessSpec{TaskID: "task_apple_integration", WorkspaceGeneration: generation.ID, LeaseGeneration: lease.FencingGeneration, RuntimeID: started.ID, Execution: security.CompiledExecution{Backend: "apple-container", Executable: "/bin/echo", Arguments: []string{"apple-container-ok"}, CWD: "/workspace"}, Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("supervised Apple process start error = %v", err)
	}
	outcome, err := echo.Wait(ctx)
	if err != nil || outcome.ExitCode != 0 {
		t.Fatalf("supervised Apple process outcome = %#v, error = %v", outcome, err)
	}
	chunks, err := echo.Read(0, 16)
	if err != nil || !strings.Contains(chunksText(chunks), "apple-container-ok") {
		t.Fatalf("supervised Apple output = %#v, error = %v", chunks, err)
	}

	touch, err := supervisor.Start(ctx, process.ProcessSpec{TaskID: "task_apple_integration", WorkspaceGeneration: generation.ID, LeaseGeneration: lease.FencingGeneration, RuntimeID: started.ID, Execution: security.CompiledExecution{Backend: "apple-container", Executable: "/bin/touch", Arguments: []string{"/workspace/candidate-marker"}, CWD: "/workspace"}, Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("candidate mutation process start error = %v", err)
	}
	if outcome, err := touch.Wait(ctx); err != nil || outcome.ExitCode != 0 {
		t.Fatalf("candidate mutation outcome = %#v, error = %v", outcome, err)
	}
	if _, err := os.Stat(filepath.Join(generation.Path, "candidate-marker")); err != nil {
		t.Fatalf("candidate mutation missing from TaskWorkspace: %v", err)
	}
	if _, err := os.Stat(filepath.Join(liveRoot, "candidate-marker")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("candidate mutation escaped into live repository: %v", err)
	}

	networkCheck := exec.CommandContext(ctx, binary, "exec", started.ExternalID, "/bin/sh", "-c", "wget -T 2 -O - https://example.com >/dev/null 2>&1")
	if err := networkCheck.Run(); err == nil {
		t.Fatal("network-denied Apple container unexpectedly reached external URL")
	}
	inspectOutput, err := exec.CommandContext(ctx, binary, "inspect", started.ExternalID).CombinedOutput()
	if err != nil || !strings.Contains(strings.ToLower(string(inspectOutput)), "memory") {
		t.Fatalf("Apple resource inspect = %s, error = %v", inspectOutput, err)
	}

	// Re-open the persisted runtime manager as a crash simulation. Recovery
	// must stop/delete the external container idempotently before any later
	// workspace mutation is attempted.
	recovered, err := NewManager(repository, filepath.Join(stateRoot, "runtimes"), AppleContainerAdapter{Binary: binary}, LimaAdapter{})
	if err != nil {
		t.Fatal(err)
	}
	if err := recovered.Recover(ctx); err != nil {
		t.Fatalf("runtime recovery error = %v", err)
	}
	recoveredRecord, err := recovered.Status(ctx, generation.ID)
	if err != nil || recoveredRecord.State != StateStopped {
		t.Fatalf("recovered runtime = %#v, error = %v", recoveredRecord, err)
	}
	if err := repository.ReleaseRuntime(ctx, lease, "apple-integration"); err != nil {
		t.Fatalf("ReleaseRuntime() error = %v", err)
	}
	if err := repository.ReleaseLease(ctx, lease); err != nil {
		t.Fatalf("ReleaseLease() error = %v", err)
	}
	if err := repository.AssertLease(ctx, lease); !errors.Is(err, workspace.ErrStaleFence) {
		t.Fatalf("released lease error = %v, want stale fence", err)
	}
	if err := repository.DiscardGeneration(ctx, generation.ID); err != nil {
		t.Fatalf("DiscardGeneration() error = %v", err)
	}
}

func chunksText(chunks []process.Chunk) string {
	var builder strings.Builder
	for _, chunk := range chunks {
		_, _ = io.WriteString(&builder, chunk.Data)
	}
	return builder.String()
}
