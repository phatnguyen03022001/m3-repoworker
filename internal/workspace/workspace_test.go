package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMaterializeIsolatedGenerationWithSnapshotBinding(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	stateRoot := t.TempDir()
	if err := os.Mkdir(filepath.Join(repoRoot, ".git"), 0o700); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	if err := os.Mkdir(filepath.Join(repoRoot, ".cache"), 0o700); err != nil {
		t.Fatalf("mkdir .cache: %v", err)
	}
	if err := os.Mkdir(filepath.Join(repoRoot, "bin"), 0o700); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "main.txt"), []byte("before\n"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, ".cache", "ignored"), []byte("cache\n"), 0o600); err != nil {
		t.Fatalf("write cache: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "bin", "dev-tool"), []byte("#!/bin/sh\nprintf dev-tool\n"), 0o700); err != nil {
		t.Fatalf("write dev tool: %v", err)
	}
	repository, err := OpenRepository(repoRoot, stateRoot)
	if err != nil {
		t.Fatalf("OpenRepository() error = %v", err)
	}
	generation, err := repository.Materialize(context.Background())
	if err != nil {
		t.Fatalf("Materialize() error = %v", err)
	}
	if generation.Path == repoRoot || pathWithin(repoRoot, generation.Path) {
		t.Fatalf("generation path %q is not isolated from %q", generation.Path, repoRoot)
	}
	if generation.CandidateSnapshot == "" || generation.RepositoryID != repository.RootIdentity() {
		t.Fatalf("generation binding = %#v", generation)
	}
	content, err := os.ReadFile(filepath.Join(generation.Path, "main.txt"))
	if err != nil || string(content) != "before\n" {
		t.Fatalf("generation content = %q, %v", content, err)
	}
	if _, err := os.Stat(filepath.Join(generation.Path, ".git")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("live .git copied into generation: %v", err)
	}
	if _, err := os.Stat(filepath.Join(generation.Path, ".cache")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache copied into generation: %v", err)
	}
	if mode, err := os.Stat(filepath.Join(generation.Path, "bin", "dev-tool")); err != nil || mode.Mode().Perm() != 0o700 {
		t.Fatalf("repo-local development tool missing or mode changed: %v, %v", err, mode)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "main.txt"), []byte("after\n"), 0o600); err != nil {
		t.Fatalf("mutate source: %v", err)
	}
	content, err = os.ReadFile(filepath.Join(generation.Path, "main.txt"))
	if err != nil || string(content) != "before\n" {
		t.Fatalf("generation changed with live source = %q, %v", content, err)
	}
}

func TestLeaseFencingAndRuntimeReservation(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	stateRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, "main.txt"), []byte("source\n"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	repository, err := OpenRepository(repoRoot, stateRoot)
	if err != nil {
		t.Fatalf("OpenRepository() error = %v", err)
	}
	generation, err := repository.Materialize(context.Background())
	if err != nil {
		t.Fatalf("Materialize() error = %v", err)
	}
	first, err := repository.AcquireLease(context.Background(), generation.ID, "task-a", time.Minute)
	if err != nil {
		t.Fatalf("AcquireLease() error = %v", err)
	}
	if first.FencingGeneration == 0 {
		t.Fatal("first lease did not receive a fence")
	}
	if _, err := repository.AcquireLease(context.Background(), generation.ID, "task-b", time.Minute); !errors.Is(err, ErrLeaseBusy) {
		t.Fatalf("second AcquireLease() error = %v, want busy", err)
	}
	if err := repository.ReserveRuntime(context.Background(), first, "runtime-a"); err != nil {
		t.Fatalf("ReserveRuntime() error = %v", err)
	}
	if err := repository.ReserveRuntime(context.Background(), first, "runtime-b"); !errors.Is(err, ErrLeaseBusy) {
		t.Fatalf("second ReserveRuntime() error = %v, want busy", err)
	}
	if err := repository.AssertLease(context.Background(), first); err != nil {
		t.Fatalf("AssertLease() error = %v", err)
	}
	if err := repository.ReleaseRuntime(context.Background(), first, "runtime-a"); err != nil {
		t.Fatalf("ReleaseRuntime() error = %v", err)
	}
	if err := repository.ReleaseLease(context.Background(), first); err != nil {
		t.Fatalf("ReleaseLease() error = %v", err)
	}
	second, err := repository.AcquireLease(context.Background(), generation.ID, "task-b", time.Minute)
	if err != nil {
		t.Fatalf("reacquire lease error = %v", err)
	}
	if second.FencingGeneration <= first.FencingGeneration {
		t.Fatalf("fence did not advance: first=%d second=%d", first.FencingGeneration, second.FencingGeneration)
	}
	if err := repository.AssertLease(context.Background(), first); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("old lease assertion error = %v, want stale fence", err)
	}
}

func TestExpiredLeaseQuarantinesGeneration(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	stateRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, "main.txt"), []byte("source\n"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	repository, err := OpenRepository(repoRoot, stateRoot)
	if err != nil {
		t.Fatalf("OpenRepository() error = %v", err)
	}
	generation, err := repository.Materialize(context.Background())
	if err != nil {
		t.Fatalf("Materialize() error = %v", err)
	}
	lease, err := repository.AcquireLease(context.Background(), generation.ID, "task-a", time.Millisecond)
	if err != nil {
		t.Fatalf("AcquireLease() error = %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	if _, err := repository.AcquireLease(context.Background(), generation.ID, "task-b", time.Minute); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("expired AcquireLease() error = %v, want stale fence", err)
	}
	if err := repository.AssertLease(context.Background(), lease); !errors.Is(err, ErrRejected) && !errors.Is(err, ErrStaleFence) {
		t.Fatalf("quarantined lease assertion error = %v", err)
	}
	if _, err := repository.AcquireLease(context.Background(), generation.ID, "task-c", time.Minute); !errors.Is(err, ErrRejected) {
		t.Fatalf("quarantined reacquire error = %v, want rejected", err)
	}
}

func TestCloseOpenRecoveryRehydratesGenerationAndAdvancesFence(t *testing.T) {
	repoRoot := t.TempDir()
	stateRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, "main.txt"), []byte("source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	firstRepository, err := OpenRepository(repoRoot, stateRoot)
	if err != nil {
		t.Fatalf("OpenRepository(first) error = %v", err)
	}
	generation, err := firstRepository.Materialize(context.Background())
	if err != nil {
		t.Fatalf("Materialize() error = %v", err)
	}
	firstLease, err := firstRepository.AcquireLease(context.Background(), generation.ID, "caller-a", time.Minute)
	if err != nil {
		t.Fatalf("AcquireLease(first) error = %v", err)
	}
	if err := firstRepository.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	restarted, err := OpenRepository(repoRoot, stateRoot)
	if err != nil {
		t.Fatalf("OpenRepository(restart) error = %v", err)
	}
	defer restarted.Close()
	if err := restarted.Recover(context.Background()); err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	rehydrated, err := restarted.LoadGeneration(context.Background(), generation.ID)
	if err != nil || rehydrated != generation {
		t.Fatalf("LoadGeneration() = %#v, %v; want %#v", rehydrated, err, generation)
	}
	secondLease, err := restarted.AcquireLease(context.Background(), generation.ID, "caller-b", time.Minute)
	if err != nil {
		t.Fatalf("AcquireLease(second) error = %v", err)
	}
	if secondLease.FencingGeneration <= firstLease.FencingGeneration {
		t.Fatalf("recovery did not advance fence: first=%d second=%d", firstLease.FencingGeneration, secondLease.FencingGeneration)
	}
	if err := restarted.AssertLease(context.Background(), firstLease); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("old lease assertion error = %v, want stale fence", err)
	}
}

func TestRecoverQuarantinesCorruptedGenerationRecord(t *testing.T) {
	repoRoot := t.TempDir()
	stateRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, "main.txt"), []byte("source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repository, err := OpenRepository(repoRoot, stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	generation, err := repository.Materialize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(generation.Path, ".generation.json"), []byte("{corrupt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	restarted, err := OpenRepository(repoRoot, stateRoot)
	if err != nil {
		t.Fatalf("OpenRepository(restart) error = %v", err)
	}
	defer restarted.Close()
	if err := restarted.Recover(context.Background()); err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	if _, err := restarted.LoadGeneration(context.Background(), generation.ID); !errors.Is(err, ErrRejected) {
		t.Fatalf("LoadGeneration(corrupt) error = %v, want rejection", err)
	}
	entries, err := os.ReadDir(filepath.Join(restarted.workspaceRoot, "quarantine"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("quarantine entries = %d, %v; want one record", len(entries), err)
	}
}

func TestRecoverQuarantinesInterruptedTemporaryGeneration(t *testing.T) {
	repoRoot := t.TempDir()
	stateRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, "main.txt"), []byte("source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repository, err := OpenRepository(repoRoot, stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	temporary := filepath.Join(repository.workspaceRoot, ".tmp-gen_interrupted")
	if err := os.Mkdir(temporary, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := OpenRepository(repoRoot, stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if err := restarted.Recover(context.Background()); err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	if _, err := os.Stat(temporary); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary generation still present: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(restarted.workspaceRoot, "quarantine"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("quarantine entries = %d, %v; want one temporary record", len(entries), err)
	}
}

func TestMaterializeRejectsSymlinkEscape(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	stateRoot := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0o600); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(repoRoot, "linked")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	repository, err := OpenRepository(repoRoot, stateRoot)
	if err != nil {
		t.Fatalf("OpenRepository() error = %v", err)
	}
	if _, err := repository.Materialize(context.Background()); !errors.Is(err, ErrRejected) {
		t.Fatalf("Materialize(symlink) error = %v, want ErrRejected", err)
	}
	entries, err := os.ReadDir(filepath.Join(stateRoot, repository.RootIdentity(), "workspaces"))
	if err != nil {
		t.Fatalf("read workspace root: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".tmp-") {
			t.Fatalf("temporary generation leaked: %s", entry.Name())
		}
	}
}
