package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tienphat/m3-repoworker/internal/security"
	"github.com/tienphat/m3-repoworker/internal/workspace"
)

func testRuntimeFixture(t *testing.T) (*workspace.Repository, workspace.Generation, workspace.Lease, RuntimeSpec, string) {
	t.Helper()
	repoRoot := t.TempDir()
	stateRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	repository, err := workspace.OpenRepository(repoRoot, stateRoot)
	if err != nil {
		t.Fatalf("OpenRepository() error = %v", err)
	}
	generation, err := repository.Materialize(context.Background())
	if err != nil {
		t.Fatalf("Materialize() error = %v", err)
	}
	lease, err := repository.AcquireLease(context.Background(), generation.ID, "task-a", time.Minute)
	if err != nil {
		t.Fatalf("AcquireLease() error = %v", err)
	}
	spec := RuntimeSpec{TaskID: "task_a", Generation: generation, Lease: lease, WorkspacePath: generation.Path, LiveRepositoryPath: repoRoot, Image: "alpine:latest", CPU: 2, MemoryBytes: 128 << 20, Network: security.NetworkNone, MountReadOnly: false}
	return repository, generation, lease, spec, filepath.Join(stateRoot, "runtimes")
}

func TestRuntimeLifecycleBindsGenerationLeaseAndCleansUp(t *testing.T) {
	repository, generation, lease, spec, runtimeState := testRuntimeFixture(t)
	defer repository.Close()
	adapter := &FakeAdapter{BackendName: "test"}
	manager, err := NewManager(repository, runtimeState, adapter)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	created, err := manager.Create(context.Background(), spec, "test")
	if err != nil || created.State != StateReady || created.LeaseGeneration != lease.FencingGeneration {
		t.Fatalf("Create() = %#v, %v", created, err)
	}
	if _, err := manager.Create(context.Background(), spec, "test"); !errors.Is(err, ErrRejected) {
		t.Fatalf("duplicate Create() error = %v", err)
	}
	running, err := manager.Start(context.Background(), generation.ID, lease)
	if err != nil || running.State != StateRunning {
		t.Fatalf("Start() = %#v, %v", running, err)
	}
	stopped, err := manager.Stop(context.Background(), generation.ID, lease)
	if err != nil || stopped.State != StateStopped {
		t.Fatalf("Stop() = %#v, %v", stopped, err)
	}
	if err := manager.Delete(context.Background(), generation.ID, lease); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(runtimeState, created.ID+".runtime.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("runtime state remains after delete: %v", err)
	}
	if strings.Join(adapter.Calls, ",") != "create,start,stop,delete" {
		t.Fatalf("adapter calls = %#v", adapter.Calls)
	}
}

func TestRuntimeRejectsLiveMountOverlapAndFenceMismatch(t *testing.T) {
	repository, generation, lease, spec, runtimeState := testRuntimeFixture(t)
	defer repository.Close()
	manager, err := NewManager(repository, runtimeState, &FakeAdapter{BackendName: "test"})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	bad := spec
	bad.WorkspacePath = filepath.Join(spec.LiveRepositoryPath, "task")
	bad.Generation.Path = bad.WorkspacePath
	if _, err := manager.Create(context.Background(), bad, "test"); !errors.Is(err, ErrRejected) {
		t.Fatalf("overlap Create() error = %v", err)
	}
	if _, err := manager.Create(context.Background(), spec, "test"); err != nil {
		t.Fatalf("valid Create() error = %v", err)
	}
	stale := lease
	stale.FencingGeneration++
	if _, err := manager.Start(context.Background(), generation.ID, stale); !errors.Is(err, ErrRejected) {
		t.Fatalf("stale Start() error = %v", err)
	}
}

func TestRuntimeRecoveryStopsPersistedActiveRuntime(t *testing.T) {
	repository, generation, lease, spec, runtimeState := testRuntimeFixture(t)
	defer repository.Close()
	firstAdapter := &FakeAdapter{BackendName: "test"}
	manager, err := NewManager(repository, runtimeState, firstAdapter)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	created, err := manager.Create(context.Background(), spec, "test")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if _, err := manager.Start(context.Background(), generation.ID, lease); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	restartedAdapter := &FakeAdapter{BackendName: "test"}
	restarted, err := NewManager(repository, runtimeState, restartedAdapter)
	if err != nil {
		t.Fatalf("restarted NewManager() error = %v", err)
	}
	if err := restarted.Recover(context.Background()); err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	if strings.Join(restartedAdapter.Calls, ",") != "stop,delete" {
		t.Fatalf("recovery calls = %#v", restartedAdapter.Calls)
	}
	if created.State == StateQuarantined {
		t.Fatal("created runtime unexpectedly quarantined")
	}
}

func TestStoppedRuntimeIsRecreatedForFreshLeaseAfterRestart(t *testing.T) {
	repository, generation, lease, spec, runtimeState := testRuntimeFixture(t)
	defer repository.Close()
	adapter := &FakeAdapter{BackendName: "test"}
	manager, err := NewManager(repository, runtimeState, adapter)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.ReserveRuntime(context.Background(), lease, "task-a"); err != nil {
		t.Fatal(err)
	}
	created, err := manager.Create(context.Background(), spec, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Start(context.Background(), generation.ID, lease); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Stop(context.Background(), generation.ID, lease); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReleaseRuntime(context.Background(), lease, "task-a"); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReleaseLease(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	freshLease, err := repository.AcquireLease(context.Background(), generation.ID, "task-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	freshSpec := spec
	freshSpec.Lease = freshLease
	recreated, err := manager.Create(context.Background(), freshSpec, "test")
	if err != nil {
		t.Fatalf("Create(recreated) error = %v", err)
	}
	if recreated.ID == created.ID || recreated.LeaseGeneration != freshLease.FencingGeneration || recreated.State != StateReady {
		t.Fatalf("recreated runtime = %#v, old=%#v lease=%#v", recreated, created, freshLease)
	}
}

func TestRuntimeRecreationFailureDoesNotManufactureReadyState(t *testing.T) {
	repository, generation, lease, spec, runtimeState := testRuntimeFixture(t)
	defer repository.Close()
	adapter := &FakeAdapter{BackendName: "test"}
	manager, err := NewManager(repository, runtimeState, adapter)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.ReserveRuntime(context.Background(), lease, "task-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(context.Background(), spec, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Start(context.Background(), generation.ID, lease); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Stop(context.Background(), generation.ID, lease); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReleaseRuntime(context.Background(), lease, "task-a"); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReleaseLease(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	freshLease, err := repository.AcquireLease(context.Background(), generation.ID, "task-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	adapter.Err = errors.New("backend unavailable")
	freshSpec := spec
	freshSpec.Lease = freshLease
	if _, err := manager.Create(context.Background(), freshSpec, "test"); !errors.Is(err, ErrRejected) {
		t.Fatalf("Create(failed recreation) error = %v, want rejection", err)
	}
	record, err := manager.Status(context.Background(), generation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != StateQuarantined {
		t.Fatalf("failed recreation state = %s, want QUARANTINED", record.State)
	}
}

type recordingRunner struct {
	Args []string
}

func (r *recordingRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.Args = append([]string{name}, args...)
	return nil, nil
}

func TestAppleContainerAdapterBuildsIsolatedNoNetworkCommand(t *testing.T) {
	runner := &recordingRunner{}
	adapter := AppleContainerAdapter{Runner: runner, Binary: "/opt/homebrew/bin/container"}
	repoRoot := "/private/repo"
	workspacePath := "/private/state/gen_1"
	spec := RuntimeSpec{WorkspacePath: workspacePath, LiveRepositoryPath: repoRoot, Image: "alpine:latest", CPU: 4, MemoryBytes: 256 << 20, Network: security.NetworkNone, MountReadOnly: true}
	if _, err := adapter.Create(context.Background(), spec, "runtime_test"); err != nil {
		t.Fatalf("adapter Create() error = %v", err)
	}
	command := strings.Join(runner.Args, " ")
	for _, required := range []string{"create", "--network none", "--cpus 4", "--memory 268435456", "source=/private/state/gen_1,target=/workspace,readonly", "alpine:latest"} {
		if !strings.Contains(command, required) {
			t.Fatalf("container command %q missing %q", command, required)
		}
	}
	if strings.Contains(command, repoRoot) {
		t.Fatalf("live repository leaked into container command: %q", command)
	}
}
