package loop_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tienphat/m3-repoworker/internal/events"
	"github.com/tienphat/m3-repoworker/internal/loop"
)

var binding = loop.Binding{RepositoryID: strings.Repeat("c", 64), CandidateSnapshot: strings.Repeat("a", 64), EnvironmentID: "env_1", PolicyVersion: "policy_1"}

type fakeModel struct {
	diagnoses int
}

func (m *fakeModel) Inspect(context.Context, loop.Binding) (string, error) {
	return "fix the failing test", nil
}
func (m *fakeModel) Plan(_ context.Context, b loop.Binding, _ string) (loop.Plan, error) {
	return plan(b, "one"), nil
}
func (m *fakeModel) Diagnose(_ context.Context, b loop.Binding, _ loop.Failure, _ []string) (loop.Plan, error) {
	m.diagnoses++
	return plan(b, "two"), nil
}

type fakeAuthority struct {
	crashParallel bool
	failTargeted  bool
	parallelCalls int
	targetedCalls int
}

type humanModel struct{ fakeModel }

func (m *humanModel) Plan(_ context.Context, b loop.Binding, _ string) (loop.Plan, error) {
	result := plan(b, "human")
	result.Destructive = true
	return result, nil
}

func (a *fakeAuthority) ParallelCommands(context.Context, loop.Binding, []loop.Action) error {
	a.parallelCalls++
	if a.crashParallel {
		a.crashParallel = false
		panic("simulated server crash")
	}
	return nil
}
func (a *fakeAuthority) PatchCandidate(context.Context, loop.Binding, loop.Action) error { return nil }
func (a *fakeAuthority) TargetedTest(context.Context, loop.Binding, loop.Action) error {
	a.targetedCalls++
	if a.failTargeted {
		a.failTargeted = false
		return errors.New("targeted test failed")
	}
	return nil
}
func (a *fakeAuthority) FullVerify(context.Context, loop.Binding, loop.Action) error { return nil }
func (a *fakeAuthority) Checkpoint(context.Context, loop.Binding) error              { return nil }

func plan(b loop.Binding, suffix string) loop.Plan {
	return loop.Plan{
		Binding:      b,
		Commands:     []loop.Action{{ID: "command-" + suffix, Kind: "test", Fingerprint: "command-" + suffix}},
		Patch:        loop.Action{ID: "patch-" + suffix, Kind: "patch", Fingerprint: "patch-" + suffix},
		TargetedTest: loop.Action{ID: "targeted-" + suffix, Kind: "targeted_test", Fingerprint: "targeted-" + suffix},
		FullVerify:   loop.Action{ID: "full-" + suffix, Kind: "full_verify", Fingerprint: "full-" + suffix},
	}
}

func openRun(t *testing.T, id string) (*events.Store, loop.Request) {
	t.Helper()
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := store.CreateRun(context.Background(), events.Run{ID: id, TaskID: "task_1", RepositoryID: binding.RepositoryID, GenerationID: "generation_1", EnvironmentID: binding.EnvironmentID, PolicyVersion: binding.PolicyVersion, CandidateSnapshot: binding.CandidateSnapshot, Status: "running", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	return store, loop.Request{RunID: id, Binding: binding}
}

func TestLoopRunsFixedPhasesAndPersistsCheckpoint(t *testing.T) {
	store, request := openRun(t, "run_fixed")
	defer store.Close()
	model := &fakeModel{}
	authority := &fakeAuthority{}
	controller, err := loop.New(store, model, authority, 2)
	if err != nil {
		t.Fatal(err)
	}
	state, err := controller.Run(context.Background(), request)
	if err != nil || state.Phase != loop.PhaseCompleted {
		t.Fatalf("Run() = %#v, %v", state, err)
	}
	checkpoint, err := store.LatestCheckpoint(context.Background(), request.RunID)
	if err != nil || checkpoint.CandidateSnapshot != binding.CandidateSnapshot {
		t.Fatalf("checkpoint = %#v, %v", checkpoint, err)
	}
	eventsFound, err := store.ListEvents(context.Background(), request.RunID, 0, 100)
	if err != nil || len(eventsFound) < 8 {
		t.Fatalf("events = %d, %v", len(eventsFound), err)
	}
}

func TestLoopCrashResumeAndRetryConvergence(t *testing.T) {
	store, request := openRun(t, "run_resume")
	defer store.Close()
	model := &fakeModel{}
	authority := &fakeAuthority{crashParallel: true}
	controller, err := loop.New(store, model, authority, 2)
	if err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() { _ = recover() }()
		_, _ = controller.Run(context.Background(), request)
	}()
	state, err := controller.Run(context.Background(), request)
	if err != nil || state.Phase != loop.PhaseCompleted {
		t.Fatalf("resume = %#v, %v", state, err)
	}
	if authority.parallelCalls != 2 {
		t.Fatalf("parallel calls = %d, want 2", authority.parallelCalls)
	}

	store2, request2 := openRun(t, "run_retry")
	defer store2.Close()
	model2 := &fakeModel{}
	authority2 := &fakeAuthority{failTargeted: true}
	controller2, err := loop.New(store2, model2, authority2, 2)
	if err != nil {
		t.Fatal(err)
	}
	state, err = controller2.Run(context.Background(), request2)
	if err != nil || state.Phase != loop.PhaseCompleted {
		t.Fatalf("retry = %#v, %v", state, err)
	}
	if model2.diagnoses != 1 || authority2.targetedCalls != 2 {
		t.Fatalf("diagnoses=%d targeted=%d", model2.diagnoses, authority2.targetedCalls)
	}
}

func TestLoopRejectsAmbiguousPlanForHuman(t *testing.T) {
	store, request := openRun(t, "run_human")
	defer store.Close()
	model := &humanModel{}
	controller, err := loop.New(store, model, &fakeAuthority{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Run(context.Background(), request); !errors.Is(err, loop.ErrHumanCheckpoint) {
		t.Fatalf("destructive plan error = %v", err)
	}
	bad := loop.Request{RunID: request.RunID, Binding: loop.Binding{RepositoryID: "wrong", CandidateSnapshot: binding.CandidateSnapshot, EnvironmentID: binding.EnvironmentID, PolicyVersion: binding.PolicyVersion}}
	if _, err := controller.Run(context.Background(), bad); !errors.Is(err, loop.ErrRejected) {
		t.Fatalf("bad binding error = %v", err)
	}
}
