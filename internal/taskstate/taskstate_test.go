package taskstate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeInspector struct {
	state RepositoryState
	err   error
}

func (f *fakeInspector) Snapshot(context.Context, string) (RepositoryState, error) {
	return f.state, f.err
}

func TestCreatePersistsAndStatusSurvivesRestart(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	stateRoot := t.TempDir()
	inspector := &fakeInspector{state: RepositoryState{Branch: "main", Head: strings.Repeat("a", 40)}}
	store, err := NewWithInspector(repoRoot, stateRoot, inspector)
	if err != nil {
		t.Fatalf("NewWithInspector() error = %v", err)
	}

	created, err := store.Create(context.Background(), "continue parser work")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if !taskIDRE.MatchString(created.TaskID) {
		t.Fatalf("task id = %q", created.TaskID)
	}
	if created.Branch != "main" || created.BaseSHA != inspector.state.Head || created.CurrentHeadSHA != inspector.state.Head {
		t.Fatalf("created state = %#v", created)
	}
	if created.VerificationState != "RED" || created.LastVerifiedSHA != "" || created.NextAction != "continue parser work" {
		t.Fatalf("created state = %#v", created)
	}

	restarted, err := NewWithInspector(repoRoot, stateRoot, inspector)
	if err != nil {
		t.Fatalf("restart NewWithInspector() error = %v", err)
	}
	loaded, err := restarted.Status(context.Background(), created.TaskID)
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if loaded.TaskID != created.TaskID || loaded.RepoRootIdentity != created.RepoRootIdentity || loaded.NextAction != created.NextAction {
		t.Fatalf("loaded state = %#v, want %#v", loaded, created)
	}

	info, err := os.Stat(restarted.taskPath(created.TaskID))
	if err != nil {
		t.Fatalf("stat state: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("state mode = %o, want 600", info.Mode().Perm())
	}
}

func TestResumeRefreshesHeadAndForcesRed(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	stateRoot := t.TempDir()
	inspector := &fakeInspector{state: RepositoryState{Branch: "feature/task", Head: strings.Repeat("a", 40)}}
	store, err := NewWithInspector(repoRoot, stateRoot, inspector)
	if err != nil {
		t.Fatalf("NewWithInspector() error = %v", err)
	}
	created, err := store.Create(context.Background(), "next")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	state, err := store.load(created.TaskID)
	if err != nil {
		t.Fatalf("load() error = %v", err)
	}
	state.VerificationState = "GREEN"
	state.LastVerifiedSHA = state.CurrentHeadSHA
	state.FailedChecks = []string{"old-check"}
	if err := store.save(state); err != nil {
		t.Fatalf("save() error = %v", err)
	}

	inspector.state.Head = strings.Repeat("b", 40)
	resumed, err := store.Resume(context.Background(), created.TaskID)
	if err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if resumed.CurrentHeadSHA != inspector.state.Head || resumed.VerificationState != "RED" {
		t.Fatalf("resumed = %#v", resumed)
	}
	if resumed.LastVerifiedSHA != strings.Repeat("a", 40) {
		t.Errorf("last verified sha = %q", resumed.LastVerifiedSHA)
	}
	if len(resumed.FailedChecks) != 0 {
		t.Errorf("failed checks = %#v, want cleared", resumed.FailedChecks)
	}
}

func TestResumeRejectsBranchSwitch(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	stateRoot := t.TempDir()
	inspector := &fakeInspector{state: RepositoryState{Branch: "main", Head: strings.Repeat("a", 40)}}
	store, err := NewWithInspector(repoRoot, stateRoot, inspector)
	if err != nil {
		t.Fatalf("NewWithInspector() error = %v", err)
	}
	created, err := store.Create(context.Background(), "")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	inspector.state.Branch = "other"
	if _, err := store.Resume(context.Background(), created.TaskID); !errors.Is(err, ErrRejected) {
		t.Fatalf("Resume() error = %v, want ErrRejected", err)
	}
}

func TestCorruptOrMismatchedStateFailsClosed(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	stateRoot := t.TempDir()
	inspector := &fakeInspector{state: RepositoryState{Branch: "main", Head: strings.Repeat("a", 40)}}
	store, err := NewWithInspector(repoRoot, stateRoot, inspector)
	if err != nil {
		t.Fatalf("NewWithInspector() error = %v", err)
	}
	created, err := store.Create(context.Background(), "")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if err := os.WriteFile(store.taskPath(created.TaskID), []byte("{not-json\n"), 0o600); err != nil {
		t.Fatalf("corrupt state: %v", err)
	}
	if _, err := store.Status(context.Background(), created.TaskID); !errors.Is(err, ErrRejected) {
		t.Fatalf("Status(corrupt) error = %v, want ErrRejected", err)
	}
	if _, err := store.Status(context.Background(), "../escape"); !errors.Is(err, ErrRejected) {
		t.Fatalf("Status(invalid id) error = %v, want ErrRejected", err)
	}
}

func TestStateRootMustStayOutsideRepository(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	inside := filepath.Join(repoRoot, ".repoworker-state")
	inspector := &fakeInspector{state: RepositoryState{Branch: "main", Head: strings.Repeat("a", 40)}}
	if _, err := NewWithInspector(repoRoot, inside, inspector); !errors.Is(err, ErrRejected) {
		t.Fatalf("NewWithInspector(state inside repo) error = %v, want ErrRejected", err)
	}
}

func TestCreateRejectsInvalidInspectorAndOversizedAction(t *testing.T) {
	t.Parallel()

	store, err := NewWithInspector(t.TempDir(), t.TempDir(), &fakeInspector{state: RepositoryState{Branch: "", Head: "bad"}})
	if err != nil {
		t.Fatalf("NewWithInspector() error = %v", err)
	}
	if _, err := store.Create(context.Background(), ""); !errors.Is(err, ErrRejected) {
		t.Fatalf("Create(invalid repo state) error = %v, want ErrRejected", err)
	}

	store.inspector = &fakeInspector{state: RepositoryState{Branch: "main", Head: strings.Repeat("a", 40)}}
	if _, err := store.Create(context.Background(), strings.Repeat("x", maxNextActionBytes+1)); !errors.Is(err, ErrRejected) {
		t.Fatalf("Create(oversized action) error = %v, want ErrRejected", err)
	}
}

func TestLegacyTaskWithoutFilesystemIdentityFailsClosed(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	stateRoot := t.TempDir()
	inspector := &fakeInspector{state: RepositoryState{Branch: "main", Head: strings.Repeat("a", 40)}}
	store, err := NewWithInspector(repoRoot, stateRoot, inspector)
	if err != nil {
		t.Fatalf("NewWithInspector() error = %v", err)
	}
	created, err := store.Create(context.Background(), "")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	created.Version = 1
	created.RepoFSIdentity = ""
	data, err := json.Marshal(created)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if err := os.WriteFile(store.taskPath(created.TaskID), append(data, '\n'), 0o600); err != nil {
		t.Fatalf("write legacy state: %v", err)
	}
	if _, err := store.Status(context.Background(), created.TaskID); !errors.Is(err, ErrRejected) {
		t.Fatalf("Status(legacy state) error = %v, want ErrRejected", err)
	}
}

func TestJSONStateStoreContract(t *testing.T) {
	repoRoot := t.TempDir()
	stateRoot := t.TempDir()
	inspector := &fakeInspector{state: RepositoryState{Branch: "main", Head: strings.Repeat("a", 40)}}
	factory := func() StateStore {
		store, err := NewWithInspector(repoRoot, stateRoot, inspector)
		if err != nil {
			t.Fatalf("NewWithInspector() error = %v", err)
		}
		return store
	}
	runStateStoreContract(t, factory, inspector)
}

func runStateStoreContract(t *testing.T, factory func() StateStore, inspector *fakeInspector) {
	t.Helper()
	ctx := context.Background()
	first := factory()
	created, err := first.Create(ctx, "contract handoff")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if created.VerificationState != "RED" || created.CurrentHeadSHA != inspector.state.Head {
		t.Fatalf("created state = %#v", created)
	}

	restarted := factory()
	loaded, err := restarted.Status(ctx, created.TaskID)
	if err != nil {
		t.Fatalf("Status() after restart error = %v", err)
	}
	if loaded.TaskID != created.TaskID || loaded.NextAction != created.NextAction {
		t.Fatalf("loaded state = %#v, want task %q", loaded, created.TaskID)
	}

	inspector.state.Head = strings.Repeat("b", 40)
	resumed, err := restarted.Resume(ctx, created.TaskID)
	if err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if resumed.CurrentHeadSHA != inspector.state.Head || resumed.VerificationState != "RED" {
		t.Fatalf("resumed state = %#v", resumed)
	}

	inspector.state.Branch = "other"
	if _, err := restarted.Resume(ctx, created.TaskID); !errors.Is(err, ErrRejected) {
		t.Fatalf("Resume(branch mismatch) error = %v, want ErrRejected", err)
	}
}

func TestFilesystemIdentityMismatchFailsClosed(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	stateRoot := t.TempDir()
	inspector := &fakeInspector{state: RepositoryState{Branch: "main", Head: strings.Repeat("a", 40)}}
	identity := strings.Repeat("1", 64)
	store, err := newBoundWithInspector(repoRoot, identity, stateRoot, inspector)
	if err != nil {
		t.Fatalf("NewWithInspector() error = %v", err)
	}
	created, err := store.Create(context.Background(), "")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if created.RepoFSIdentity != identity {
		t.Fatalf("filesystem identity = %q, want %q", created.RepoFSIdentity, identity)
	}

	restarted, err := newBoundWithInspector(repoRoot, strings.Repeat("2", 64), stateRoot, inspector)
	if err != nil {
		t.Fatalf("restart NewWithInspector() error = %v", err)
	}
	if _, err := restarted.Status(context.Background(), created.TaskID); !errors.Is(err, ErrRejected) {
		t.Fatalf("Status(mismatched filesystem identity) error = %v, want ErrRejected", err)
	}
}
