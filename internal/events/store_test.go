package events

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testRun() Run {
	now := time.Now().UTC()
	return Run{ID: "run_1", TaskID: "task_1", RepositoryID: strings.Repeat("a", 64), GenerationID: "gen_1", EnvironmentID: "env_1", PolicyVersion: "m3.2-v1", CandidateSnapshot: strings.Repeat("b", 64), Status: "running", CreatedAt: now, UpdatedAt: now}
}

func TestDurableRunsEventsArtifactsAndCheckpoints(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	run := testRun()
	if err := store.CreateRun(context.Background(), run); err != nil {
		t.Fatalf("CreateRun() error = %v", err)
	}
	first, err := store.AppendEvent(context.Background(), run.ID, "started", `{"step":"inspect"}`)
	if err != nil {
		t.Fatalf("AppendEvent(first) error = %v", err)
	}
	second, err := store.AppendEvent(context.Background(), run.ID, "output", `{"cursor":1}`)
	if err != nil {
		t.Fatalf("AppendEvent(second) error = %v", err)
	}
	if first.Sequence != 1 || second.Sequence != 2 {
		t.Fatalf("event sequence = %#v %#v", first, second)
	}
	events, err := store.ListEvents(context.Background(), run.ID, 1, 10)
	if err != nil || len(events) != 1 || events[0].Sequence != 2 {
		t.Fatalf("ListEvents() = %#v, %v", events, err)
	}
	if _, err := store.AppendEvent(context.Background(), "missing", "event", `{}`); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing event error = %v", err)
	}
	artifactSource := filepath.Join(t.TempDir(), "log.txt")
	if err := os.WriteFile(artifactSource, []byte("log\n"), 0o600); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	artifact, err := store.AddArtifact(context.Background(), run.ID, "log", artifactSource)
	if err != nil {
		t.Fatalf("AddArtifact() error = %v", err)
	}
	if got, err := store.ReadArtifact(context.Background(), artifact.ID); err != nil || string(got) != "log\n" {
		t.Fatalf("ReadArtifact() = %q, %v", got, err)
	}
	checkpoint := Checkpoint{ID: "checkpoint_1", RunID: run.ID, CandidateSnapshot: run.CandidateSnapshot, EnvironmentID: run.EnvironmentID, PolicyVersion: run.PolicyVersion, State: "verified", Payload: `{"passed":true}`, CreatedAt: time.Now().UTC()}
	if err := store.SaveCheckpoint(context.Background(), checkpoint); err != nil {
		t.Fatalf("SaveCheckpoint() error = %v", err)
	}
	latest, err := store.LatestCheckpoint(context.Background(), run.ID)
	if err != nil || latest.ID != checkpoint.ID {
		t.Fatalf("LatestCheckpoint() = %#v, %v", latest, err)
	}
	if err := store.UpdateRunStatus(context.Background(), run.ID, "completed"); err != nil {
		t.Fatalf("UpdateRunStatus() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	restarted, err := Open(path)
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	defer restarted.Close()
	restartedEvents, err := restarted.ListEvents(context.Background(), run.ID, 0, 10)
	if err != nil || len(restartedEvents) != 2 {
		t.Fatalf("restarted events = %#v, %v", restartedEvents, err)
	}
}

func TestEventRetentionAndInvalidationHint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	run := testRun()
	run.ID = "run_old"
	run.Status = "completed"
	run.UpdatedAt = time.Now().UTC().Add(-time.Hour)
	run.CreatedAt = run.UpdatedAt
	if err := store.CreateRun(context.Background(), run); err != nil {
		t.Fatalf("CreateRun() error = %v", err)
	}
	if _, err := store.AppendEvent(context.Background(), run.ID, "done", `{}`); err != nil {
		t.Fatalf("AppendEvent() error = %v", err)
	}
	artifactSource := filepath.Join(t.TempDir(), "artifact")
	if err := os.WriteFile(artifactSource, []byte("data"), 0o600); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	artifact, err := store.AddArtifact(context.Background(), run.ID, "data", artifactSource)
	if err != nil {
		t.Fatalf("AddArtifact() error = %v", err)
	}
	deleted, err := store.GC(context.Background(), time.Now().UTC(), 10)
	if err != nil || deleted != 1 {
		t.Fatalf("GC() = %d, %v", deleted, err)
	}
	if _, err := store.GetRun(context.Background(), run.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GC run = %v", err)
	}
	if _, err := os.Stat(artifact.Path); err == nil {
		t.Fatal("GC left artifact file")
	}
	hint := NewFSEventsAdapter()
	if hint.Stale() {
		t.Fatal("new hint is stale")
	}
	hint.Handle([]string{"file.go"})
	if !hint.Stale() {
		t.Fatal("hint did not become stale")
	}
	paths := hint.Consume()
	if len(paths) != 1 || hint.Stale() {
		t.Fatalf("hint consume = %#v stale=%v", paths, hint.Stale())
	}
}

func TestEventsRejectSecretsAndCorruptArtifacts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	run := testRun()
	if err := store.CreateRun(context.Background(), run); err != nil {
		t.Fatalf("CreateRun() error = %v", err)
	}
	if _, err := store.AppendEvent(context.Background(), run.ID, "secret", `{"authorization":"Bearer hidden"}`); !errors.Is(err, ErrRejected) {
		t.Fatalf("secret event error = %v", err)
	}
	if _, err := store.AppendEvent(context.Background(), run.ID, "bad", "not-json"); !errors.Is(err, ErrRejected) {
		t.Fatalf("invalid payload error = %v", err)
	}
	source := filepath.Join(t.TempDir(), "artifact")
	if err := os.WriteFile(source, []byte("valid"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	artifact, err := store.AddArtifact(context.Background(), run.ID, "data", source)
	if err != nil {
		t.Fatalf("AddArtifact() error = %v", err)
	}
	if err := os.WriteFile(artifact.Path, []byte("corrupt"), 0o600); err != nil {
		t.Fatalf("corrupt artifact: %v", err)
	}
	if _, err := store.ReadArtifact(context.Background(), artifact.ID); !errors.Is(err, ErrRejected) {
		t.Fatalf("corrupt artifact error = %v", err)
	}
}
