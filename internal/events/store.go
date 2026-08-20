// Package events persists bounded runs, ordered events, artifacts and
// checkpoints. It is the durable replay boundary for M3.8.
package events

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

var (
	ErrRejected = errors.New("events request rejected")
	ErrNotFound = errors.New("events record not found")
)

const (
	maxPayloadBytes = 1 << 20
	maxPageSize     = 1000
)

//go:embed migrations/*.sql
var migrations embed.FS

type Run struct {
	ID                string    `json:"id"`
	TaskID            string    `json:"task_id"`
	RepositoryID      string    `json:"repository_id"`
	GenerationID      string    `json:"generation_id"`
	EnvironmentID     string    `json:"environment_id"`
	PolicyVersion     string    `json:"policy_version"`
	CandidateSnapshot string    `json:"candidate_snapshot"`
	Status            string    `json:"status"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type Event struct {
	RunID     string    `json:"run_id"`
	Sequence  int64     `json:"sequence"`
	Type      string    `json:"type"`
	Payload   string    `json:"payload"`
	CreatedAt time.Time `json:"created_at"`
}

type Artifact struct {
	ID            string    `json:"id"`
	RunID         string    `json:"run_id"`
	Kind          string    `json:"kind"`
	Path          string    `json:"path"`
	ContentDigest string    `json:"content_digest"`
	Size          int64     `json:"size"`
	CreatedAt     time.Time `json:"created_at"`
}

type Checkpoint struct {
	ID                string    `json:"id"`
	RunID             string    `json:"run_id"`
	CandidateSnapshot string    `json:"candidate_snapshot"`
	EnvironmentID     string    `json:"environment_id"`
	PolicyVersion     string    `json:"policy_version"`
	State             string    `json:"state"`
	Payload           string    `json:"payload"`
	CreatedAt         time.Time `json:"created_at"`
}

type Store struct {
	db           *sql.DB
	root         string
	databasePath string
	mu           sync.Mutex
}

func Open(path string) (*Store, error) {
	if path == "" || !filepath.IsAbs(path) {
		return nil, ErrRejected
	}
	root := filepath.Dir(path)
	if err := os.MkdirAll(root, 0o700); err != nil || os.Chmod(root, 0o700) != nil {
		return nil, ErrRejected
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, ErrRejected
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, ErrRejected
	}
	_ = file.Close()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, ErrRejected
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db, root: root, databasePath: path}
	if err := store.initialize(context.Background()); err != nil {
		_ = db.Close()
		return nil, ErrRejected
	}
	return store, nil
}

func (s *Store) initialize(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, "PRAGMA busy_timeout=5000"); err != nil {
		return err
	}
	var mode string
	if err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode); err != nil || strings.ToLower(mode) != "wal" {
		return ErrRejected
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA synchronous=FULL"); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA foreign_keys=ON"); err != nil {
		return err
	}
	migration, err := migrations.ReadFile("migrations/001_initial.sql")
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, string(migration)); err != nil {
		return err
	}
	var result string
	if err := s.db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result); err != nil || result != "ok" {
		return ErrRejected
	}
	for _, sidecar := range []string{s.databasePath, s.databasePath + "-wal", s.databasePath + "-shm"} {
		if err := os.Chmod(sidecar, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			return ErrRejected
		}
	}
	return nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

func (s *Store) CreateRun(ctx context.Context, run Run) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx == nil || s.db == nil || !validRun(run) {
		return ErrRejected
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO runs(run_id, task_id, repository_id, generation_id, environment_id, policy_version, candidate_snapshot, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, run.ID, run.TaskID, run.RepositoryID, run.GenerationID, run.EnvironmentID, run.PolicyVersion, run.CandidateSnapshot, run.Status, run.CreatedAt.Format(time.RFC3339Nano), run.UpdatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return ErrRejected
	}
	return nil
}

func (s *Store) UpdateRunStatus(ctx context.Context, runID, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx == nil || s.db == nil || !validOpaque(runID) || !validStatus(status) {
		return ErrRejected
	}
	result, err := s.db.ExecContext(ctx, "UPDATE runs SET status=?, updated_at=? WHERE run_id=?", status, time.Now().UTC().Format(time.RFC3339Nano), runID)
	if err != nil {
		return ErrRejected
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) GetRun(ctx context.Context, runID string) (Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx == nil || s.db == nil || !validOpaque(runID) {
		return Run{}, ErrRejected
	}
	var run Run
	var created, updated string
	err := s.db.QueryRowContext(ctx, `SELECT run_id, task_id, repository_id, generation_id, environment_id, policy_version, candidate_snapshot, status, created_at, updated_at FROM runs WHERE run_id=?`, runID).Scan(&run.ID, &run.TaskID, &run.RepositoryID, &run.GenerationID, &run.EnvironmentID, &run.PolicyVersion, &run.CandidateSnapshot, &run.Status, &created, &updated)
	if err != nil {
		return Run{}, ErrNotFound
	}
	run.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return Run{}, ErrRejected
	}
	run.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
	if err != nil {
		return Run{}, ErrRejected
	}
	return run, nil
}

func (s *Store) AppendEvent(ctx context.Context, runID, eventType, payload string) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx == nil || s.db == nil || !validOpaque(runID) || !validOpaque(eventType) || !validPayload(payload) {
		return Event{}, ErrRejected
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Event{}, ErrRejected
	}
	var sequence int64
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(sequence), 0) + 1 FROM run_events WHERE run_id=?", runID).Scan(&sequence); err != nil {
		_ = tx.Rollback()
		return Event{}, ErrRejected
	}
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM runs WHERE run_id=?", runID).Scan(&exists); err != nil || exists != 1 {
		_ = tx.Rollback()
		return Event{}, ErrNotFound
	}
	now := time.Now().UTC()
	_, err = tx.ExecContext(ctx, "INSERT INTO run_events(run_id, sequence, event_type, payload, created_at) VALUES (?, ?, ?, ?, ?)", runID, sequence, eventType, payload, now.Format(time.RFC3339Nano))
	if err != nil {
		_ = tx.Rollback()
		return Event{}, ErrRejected
	}
	if err := tx.Commit(); err != nil {
		return Event{}, ErrRejected
	}
	return Event{RunID: runID, Sequence: sequence, Type: eventType, Payload: payload, CreatedAt: now}, nil
}

func (s *Store) ListEvents(ctx context.Context, runID string, after int64, limit int) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx == nil || s.db == nil || !validOpaque(runID) || after < 0 || limit <= 0 || limit > maxPageSize {
		return nil, ErrRejected
	}
	rows, err := s.db.QueryContext(ctx, "SELECT run_id, sequence, event_type, payload, created_at FROM run_events WHERE run_id=? AND sequence>? ORDER BY sequence LIMIT ?", runID, after, limit)
	if err != nil {
		return nil, ErrRejected
	}
	defer rows.Close()
	result := []Event{}
	for rows.Next() {
		var event Event
		var created string
		if err := rows.Scan(&event.RunID, &event.Sequence, &event.Type, &event.Payload, &created); err != nil {
			return nil, ErrRejected
		}
		event.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return nil, ErrRejected
		}
		result = append(result, event)
	}
	if err := rows.Err(); err != nil {
		return nil, ErrRejected
	}
	return result, nil
}

func (s *Store) AddArtifact(ctx context.Context, runID, kind, source string) (Artifact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx == nil || s.db == nil || !validOpaque(runID) || !validOpaque(kind) || source == "" || !filepath.IsAbs(source) {
		return Artifact{}, ErrRejected
	}
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 1<<30 {
		return Artifact{}, ErrRejected
	}
	digest, size, err := fileDigest(source)
	if err != nil {
		return Artifact{}, ErrRejected
	}
	id := "artifact_" + randomHex(16)
	destination := filepath.Join(s.root, id+"-"+digest)
	if err := copyAtomic(source, destination); err != nil {
		return Artifact{}, ErrRejected
	}
	now := time.Now().UTC()
	_, err = s.db.ExecContext(ctx, "INSERT INTO artifacts(artifact_id, run_id, kind, path, content_digest, size, created_at) SELECT ?, ?, ?, ?, ?, ?, ? WHERE EXISTS (SELECT 1 FROM runs WHERE run_id=?)", id, runID, kind, destination, digest, size, now.Format(time.RFC3339Nano), runID)
	if err != nil {
		_ = os.Remove(destination)
		return Artifact{}, ErrRejected
	}
	return Artifact{ID: id, RunID: runID, Kind: kind, Path: destination, ContentDigest: digest, Size: size, CreatedAt: now}, nil
}

func (s *Store) ReadArtifact(ctx context.Context, artifactID string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx == nil || s.db == nil || !validOpaque(artifactID) {
		return nil, ErrRejected
	}
	var path, digest string
	var size int64
	if err := s.db.QueryRowContext(ctx, "SELECT path, content_digest, size FROM artifacts WHERE artifact_id=?", artifactID).Scan(&path, &digest, &size); err != nil {
		return nil, ErrNotFound
	}
	if !pathWithin(s.root, path) {
		return nil, ErrRejected
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() != size {
		return nil, ErrRejected
	}
	data, err := os.ReadFile(path)
	if err != nil || digestBytes(data) != digest {
		return nil, ErrRejected
	}
	return data, nil
}

func (s *Store) SaveCheckpoint(ctx context.Context, checkpoint Checkpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx == nil || s.db == nil || !validCheckpoint(checkpoint) {
		return ErrRejected
	}
	var exists int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM runs WHERE run_id=?", checkpoint.RunID).Scan(&exists); err != nil || exists != 1 {
		return ErrNotFound
	}
	_, err := s.db.ExecContext(ctx, "INSERT INTO checkpoints(checkpoint_id, run_id, candidate_snapshot, environment_id, policy_version, state, payload, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)", checkpoint.ID, checkpoint.RunID, checkpoint.CandidateSnapshot, checkpoint.EnvironmentID, checkpoint.PolicyVersion, checkpoint.State, checkpoint.Payload, checkpoint.CreatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return ErrRejected
	}
	return nil
}

func (s *Store) LatestCheckpoint(ctx context.Context, runID string) (Checkpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx == nil || s.db == nil || !validOpaque(runID) {
		return Checkpoint{}, ErrRejected
	}
	var checkpoint Checkpoint
	var created string
	err := s.db.QueryRowContext(ctx, "SELECT checkpoint_id, run_id, candidate_snapshot, environment_id, policy_version, state, payload, created_at FROM checkpoints WHERE run_id=? ORDER BY created_at DESC, checkpoint_id DESC LIMIT 1", runID).Scan(&checkpoint.ID, &checkpoint.RunID, &checkpoint.CandidateSnapshot, &checkpoint.EnvironmentID, &checkpoint.PolicyVersion, &checkpoint.State, &checkpoint.Payload, &created)
	if err != nil {
		return Checkpoint{}, ErrNotFound
	}
	checkpoint.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return Checkpoint{}, ErrRejected
	}
	return checkpoint, nil
}

func (s *Store) GC(ctx context.Context, before time.Time, limit int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx == nil || s.db == nil || before.IsZero() || limit <= 0 || limit > maxPageSize {
		return 0, ErrRejected
	}
	rows, err := s.db.QueryContext(ctx, "SELECT run_id FROM runs WHERE status IN ('completed', 'failed', 'stopped') AND updated_at < ? ORDER BY updated_at LIMIT ?", before.UTC().Format(time.RFC3339Nano), limit)
	if err != nil {
		return 0, ErrRejected
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return 0, ErrRejected
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return 0, ErrRejected
	}
	deleted := 0
	for _, id := range ids {
		artifactRows, err := s.db.QueryContext(ctx, "SELECT path FROM artifacts WHERE run_id=?", id)
		if err != nil {
			return deleted, ErrRejected
		}
		var paths []string
		for artifactRows.Next() {
			var artifactPath string
			if err := artifactRows.Scan(&artifactPath); err != nil {
				_ = artifactRows.Close()
				return deleted, ErrRejected
			}
			paths = append(paths, artifactPath)
		}
		if err := artifactRows.Err(); err != nil {
			_ = artifactRows.Close()
			return deleted, ErrRejected
		}
		_ = artifactRows.Close()
		result, err := s.db.ExecContext(ctx, "DELETE FROM runs WHERE run_id=?", id)
		if err != nil {
			return deleted, ErrRejected
		}
		count, _ := result.RowsAffected()
		deleted += int(count)
		for _, artifactPath := range paths {
			if err := os.Remove(artifactPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return deleted, ErrRejected
			}
		}
	}
	return deleted, nil
}

// FSEventsAdapter is an invalidation hint. It never mutates repository state
// and never replaces a snapshot digest check.
type FSEventsAdapter struct {
	mu    sync.Mutex
	stale bool
	paths []string
}

func NewFSEventsAdapter() *FSEventsAdapter { return &FSEventsAdapter{} }
func (f *FSEventsAdapter) Handle(paths []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stale = true
	f.paths = append([]string(nil), paths...)
}
func (f *FSEventsAdapter) Stale() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.stale }
func (f *FSEventsAdapter) Consume() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	paths := append([]string(nil), f.paths...)
	f.paths = nil
	f.stale = false
	return paths
}

func validRun(run Run) bool {
	return validOpaque(run.ID) && validOpaque(run.TaskID) && validIdentity(run.RepositoryID) && validOpaque(run.GenerationID) && validOpaque(run.EnvironmentID) && validOpaque(run.PolicyVersion) && validIdentity(run.CandidateSnapshot) && validStatus(run.Status) && !run.CreatedAt.IsZero() && !run.UpdatedAt.IsZero()
}
func validCheckpoint(checkpoint Checkpoint) bool {
	return validOpaque(checkpoint.ID) && validOpaque(checkpoint.RunID) && validIdentity(checkpoint.CandidateSnapshot) && validOpaque(checkpoint.EnvironmentID) && validOpaque(checkpoint.PolicyVersion) && validOpaque(checkpoint.State) && validPayload(checkpoint.Payload) && !checkpoint.CreatedAt.IsZero()
}
func validStatus(status string) bool {
	return status == "created" || status == "running" || status == "completed" || status == "failed" || status == "stopped"
}
func validPayload(payload string) bool {
	return payload != "" && len(payload) <= maxPayloadBytes && json.Valid([]byte(payload)) && !strings.ContainsRune(payload, 0) && !containsSecret(payload)
}
func containsSecret(value string) bool {
	lower := strings.ToLower(value)
	if strings.Contains(lower, "authorization") && strings.Contains(lower, "bearer") {
		return true
	}
	for _, marker := range []string{"-----begin private key-----", "ghp_", "github_pat_", "sk-proj-", "\"password\"", "\"secret\"", "\"access_token\"", "private_key"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}
func validOpaque(value string) bool {
	return value != "" && len(value) <= 256 && !strings.ContainsAny(value, "\x00\r\n/\\")
}
func validIdentity(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
func digestBytes(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func fileDigest(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	sum := sha256.New()
	size, err := io.Copy(sum, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(sum.Sum(nil)), size, nil
}
func copyAtomic(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".artifact-")
	if err != nil {
		return err
	}
	tempPath := temporary.Name()
	defer os.Remove(tempPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := io.Copy(temporary, input); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, destination)
}
func pathWithin(parent, candidate string) bool {
	relative, err := filepath.Rel(parent, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}
func randomHex(size int) string {
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		return "0"
	}
	return hex.EncodeToString(data)
}
