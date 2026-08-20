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

func TestIntegrationPlanAppliesMultipleFilesWithDurableJournal(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	stateRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, "update.txt"), []byte("old\n"), 0o600); err != nil {
		t.Fatalf("write update: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "remove.txt"), []byte("remove\n"), 0o600); err != nil {
		t.Fatalf("write remove: %v", err)
	}
	repository, err := OpenRepository(repoRoot, stateRoot)
	if err != nil {
		t.Fatalf("OpenRepository() error = %v", err)
	}
	defer repository.Close()
	generation, err := repository.Materialize(context.Background())
	if err != nil {
		t.Fatalf("Materialize() error = %v", err)
	}
	lease, err := repository.AcquireLease(context.Background(), generation.ID, "task-a", time.Minute)
	if err != nil {
		t.Fatalf("AcquireLease() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(generation.Path, "update.txt"), []byte("new\n"), 0o640); err != nil {
		t.Fatalf("write candidate update: %v", err)
	}
	if err := os.Chmod(filepath.Join(generation.Path, "update.txt"), 0o640); err != nil {
		t.Fatalf("chmod candidate update: %v", err)
	}
	if err := os.Remove(filepath.Join(generation.Path, "remove.txt")); err != nil {
		t.Fatalf("remove candidate file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(generation.Path, "nested", "new.txt"), []byte("created\n"), 0o600); !errors.Is(err, os.ErrNotExist) {
		// The parent is intentionally absent; create it below to exercise the
		// FD-relative parent-directory creation path in ApplyIntegration.
		if err != nil {
			t.Fatalf("unexpected nested write error: %v", err)
		}
	}
	if err := os.Mkdir(filepath.Join(generation.Path, "nested"), 0o700); err != nil {
		t.Fatalf("mkdir candidate nested: %v", err)
	}
	if err := os.WriteFile(filepath.Join(generation.Path, "nested", "new.txt"), []byte("created\n"), 0o600); err != nil {
		t.Fatalf("write candidate nested: %v", err)
	}

	plan, err := repository.BuildIntegrationPlan(context.Background(), generation, lease)
	if err != nil {
		t.Fatalf("BuildIntegrationPlan() error = %v", err)
	}
	if len(plan.Steps) != 3 || plan.BaseSnapshot == plan.CandidateSnapshot {
		t.Fatalf("integration plan = %#v", plan)
	}
	journal, err := repository.ApplyIntegration(context.Background(), plan, lease)
	if err != nil {
		t.Fatalf("ApplyIntegration() error = %v", err)
	}
	if journal.State != integrationCommitted || journal.Cursor != len(plan.Steps) {
		t.Fatalf("journal = %#v", journal)
	}
	if got, err := os.ReadFile(filepath.Join(repoRoot, "update.txt")); err != nil || string(got) != "new\n" {
		t.Fatalf("updated live file = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "remove.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted live file still exists: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(repoRoot, "nested", "new.txt")); err != nil || string(got) != "created\n" {
		t.Fatalf("created live file = %q, %v", got, err)
	}
	info, err := os.Stat(filepath.Join(repoRoot, "update.txt"))
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("updated mode = %v, %v", info.Mode().Perm(), err)
	}
}

func TestIntegrationRejectsTOCTOUAndPreservesLiveFiles(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	stateRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, "file.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	repository, err := OpenRepository(repoRoot, stateRoot)
	if err != nil {
		t.Fatalf("OpenRepository() error = %v", err)
	}
	defer repository.Close()
	generation, err := repository.Materialize(context.Background())
	if err != nil {
		t.Fatalf("Materialize() error = %v", err)
	}
	lease, err := repository.AcquireLease(context.Background(), generation.ID, "task-a", time.Minute)
	if err != nil {
		t.Fatalf("AcquireLease() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(generation.Path, "file.txt"), []byte("candidate\n"), 0o600); err != nil {
		t.Fatalf("write candidate: %v", err)
	}
	plan, err := repository.BuildIntegrationPlan(context.Background(), generation, lease)
	if err != nil {
		t.Fatalf("BuildIntegrationPlan() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "file.txt"), []byte("concurrent\n"), 0o600); err != nil {
		t.Fatalf("write concurrent live change: %v", err)
	}
	if _, err := repository.ApplyIntegration(context.Background(), plan, lease); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("ApplyIntegration(TOCTOU) error = %v, want stale fence", err)
	}
	if got, err := os.ReadFile(filepath.Join(repoRoot, "file.txt")); err != nil || string(got) != "concurrent\n" {
		t.Fatalf("live file after rejected integration = %q, %v", got, err)
	}
}

func TestIntegrationRecoveryAdvancesAfterFilesystemStep(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	stateRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, "file.txt"), []byte("base\n"), 0o600); err != nil {
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
	lease, err := repository.AcquireLease(context.Background(), generation.ID, "task-a", time.Minute)
	if err != nil {
		t.Fatalf("AcquireLease() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(generation.Path, "file.txt"), []byte("candidate\n"), 0o600); err != nil {
		t.Fatalf("write candidate: %v", err)
	}
	plan, err := repository.BuildIntegrationPlan(context.Background(), generation, lease)
	if err != nil {
		t.Fatalf("BuildIntegrationPlan() error = %v", err)
	}
	if len(plan.Steps) != 1 {
		t.Fatalf("plan steps = %#v", plan.Steps)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "file.txt"), []byte("candidate\n"), 0o600); err != nil {
		t.Fatalf("simulate filesystem step: %v", err)
	}
	journal := IntegrationJournal{ID: "journal_recovery", State: integrationApplying, Cursor: 0, Plan: plan, UpdatedAt: time.Now().UTC()}
	journalPath := filepath.Join(repository.journalRoot, journal.ID+".json")
	if err := writeJSONAtomic(journalPath, journal, 0o600); err != nil {
		t.Fatalf("write crash journal: %v", err)
	}
	if err := repository.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	restarted, err := OpenRepository(repoRoot, stateRoot)
	if err != nil {
		t.Fatalf("recovery OpenRepository() error = %v", err)
	}
	defer restarted.Close()
	var recovered IntegrationJournal
	if err := readJSON(journalPath, &recovered); err != nil {
		t.Fatalf("read recovered journal: %v", err)
	}
	if recovered.State != integrationCommitted || recovered.Cursor != 1 {
		t.Fatalf("recovered journal = %#v", recovered)
	}
}

func TestIntegrationPlanRejectsInvalidPath(t *testing.T) {
	if validRelativePath("../escape") || validRelativePath("/absolute") || validRelativePath(".git/config") || validRelativePath(strings.ReplaceAll("a/b", "/", "\\")) {
		t.Fatal("invalid integration path accepted")
	}
}
