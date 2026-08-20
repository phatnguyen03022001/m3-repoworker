package taskstate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const (
	sqliteSchemaVersion = 1
	databaseFileName    = "state.db"
	migrationMarker     = "legacy_json_migration"
)

//go:embed migrations/*.sql
var sqliteMigrations embed.FS

// SQLiteStore is the production task state store. It intentionally keeps the
// repository identity and inspector in the domain layer instead of trusting
// values supplied by a client or read from the database alone.
type SQLiteStore struct {
	repoRoot     string
	stateRoot    string
	repoIdentity string
	repoFSID     string
	databasePath string
	inspector    Inspector
	db           *sql.DB
	mu           sync.Mutex
}

var _ StateStore = (*SQLiteStore)(nil)

// NewSQLiteWithInspector is testable construction for the production store.
// The inspector is still required so tests can exercise the same state
// transitions without invoking Git.
func NewSQLiteWithInspector(repoRoot, repoFSID, stateRoot string, inspector Inspector) (*SQLiteStore, error) {
	canonicalRepo, err := canonicalDirectory(repoRoot, false)
	if err != nil || !identityRE.MatchString(repoFSID) || inspector == nil {
		return nil, ErrRejected
	}
	return newSQLiteWithInspector(canonicalRepo, repoFSID, stateRoot, inspector)
}

func newSQLiteWithInspector(repoRoot, repoFSID, stateRoot string, inspector Inspector) (*SQLiteStore, error) {
	if repoRoot == "" || !filepath.IsAbs(repoRoot) || filepath.Clean(repoRoot) != repoRoot ||
		!identityRE.MatchString(repoFSID) || inspector == nil {
		return nil, ErrRejected
	}
	if stateRoot == "" || !filepath.IsAbs(stateRoot) || pathWithin(repoRoot, filepath.Clean(stateRoot)) {
		return nil, ErrRejected
	}
	canonicalState, err := canonicalDirectory(stateRoot, true)
	if err != nil || pathWithin(repoRoot, canonicalState) {
		return nil, ErrRejected
	}
	digest := sha256Digest(repoRoot)
	repoStateDir := filepath.Join(canonicalState, digest)
	if err := os.MkdirAll(repoStateDir, 0o700); err != nil || os.Chmod(repoStateDir, 0o700) != nil {
		return nil, ErrRejected
	}
	canonicalDir, err := filepath.EvalSymlinks(repoStateDir)
	if err != nil || filepath.Clean(canonicalDir) != filepath.Clean(repoStateDir) {
		return nil, ErrRejected
	}
	databasePath := filepath.Join(canonicalDir, databaseFileName)
	if err := ensurePrivateDatabaseFile(databasePath); err != nil {
		return nil, ErrRejected
	}
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		return nil, ErrRejected
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &SQLiteStore{
		repoRoot:     repoRoot,
		stateRoot:    canonicalState,
		repoIdentity: digest,
		repoFSID:     repoFSID,
		databasePath: databasePath,
		inspector:    inspector,
		db:           db,
	}
	if err := store.initialize(context.Background(), canonicalDir); err != nil {
		_ = db.Close()
		return nil, ErrRejected
	}
	return store, nil
}

func sha256Digest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func ensurePrivateDatabaseFile(path string) error {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func (s *SQLiteStore) initialize(ctx context.Context, stateDir string) error {
	if ctx == nil || s.db == nil {
		return ErrRejected
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA busy_timeout=5000"); err != nil {
		return err
	}
	var journalMode string
	if err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&journalMode); err != nil || strings.ToLower(journalMode) != "wal" {
		return ErrRejected
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA synchronous=FULL"); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA foreign_keys=ON"); err != nil {
		return err
	}
	if err := applySQLiteMigrations(ctx, s.db); err != nil {
		return err
	}
	if err := s.validatePragmas(ctx); err != nil {
		return err
	}
	if err := s.validateMetadata(ctx); err != nil {
		return err
	}
	if err := s.migrateLegacyJSON(ctx, stateDir); err != nil {
		return err
	}
	if err := s.integrityCheck(ctx); err != nil {
		return err
	}
	return enforceSQLitePermissions(s.databasePath)
}

func enforceSQLitePermissions(databasePath string) error {
	for _, path := range []string{databasePath, databasePath + "-wal", databasePath + "-shm"} {
		if err := os.Chmod(path, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func applySQLiteMigrations(ctx context.Context, db *sql.DB) error {
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version < 0 || version > sqliteSchemaVersion {
		return ErrRejected
	}
	for next := version + 1; next <= sqliteSchemaVersion; next++ {
		name := "migrations/" + formatMigrationNumber(next) + "_initial.sql"
		migration, err := sqliteMigrations.ReadFile(name)
		if err != nil {
			return ErrRejected
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(migration)); err != nil {
			_ = tx.Rollback()
			return err
		}
		if _, err := tx.ExecContext(ctx, "PRAGMA user_version="+formatMigrationNumber(next)); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func formatMigrationNumber(number int) string {
	if number < 10 {
		return "00" + strconv.Itoa(number)
	}
	if number < 100 {
		return "0" + strconv.Itoa(number)
	}
	return strconv.Itoa(number)
}

func (s *SQLiteStore) validatePragmas(ctx context.Context) error {
	var synchronous, foreignKeys int
	if err := s.db.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&synchronous); err != nil || synchronous != 2 {
		return ErrRejected
	}
	if err := s.db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		return ErrRejected
	}
	return nil
}

func (s *SQLiteStore) validateMetadata(ctx context.Context) error {
	metadata := map[string]string{}
	rows, err := s.db.QueryContext(ctx, "SELECT key, value FROM metadata")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return ErrRejected
		}
		if _, exists := metadata[key]; exists {
			return ErrRejected
		}
		metadata[key] = value
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(metadata) == 0 {
		_, err := s.db.ExecContext(ctx,
			"INSERT INTO metadata(key, value) VALUES (?, ?), (?, ?), (?, ?)",
			"repo_root_identity", s.repoIdentity,
			"repo_filesystem_identity", s.repoFSID,
			migrationMarker, "pending")
		return err
	}
	if metadata["repo_root_identity"] != s.repoIdentity || metadata["repo_filesystem_identity"] != s.repoFSID || metadata[migrationMarker] == "" {
		return ErrRejected
	}
	return nil
}

func (s *SQLiteStore) integrityCheck(ctx context.Context) error {
	var result string
	if err := s.db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result); err != nil || strings.ToLower(result) != "ok" {
		return ErrRejected
	}
	rows, err := s.db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return ErrRejected
	}
	defer rows.Close()
	if rows.Next() {
		return ErrRejected
	}
	return rows.Err()
}

func (s *SQLiteStore) migrateLegacyJSON(ctx context.Context, stateDir string) error {
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return ErrRejected
	}
	legacy := make([]string, 0)
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".json") {
			legacy = append(legacy, entry.Name())
		}
	}
	sort.Strings(legacy)
	var marker string
	if err := s.db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key=?", migrationMarker).Scan(&marker); err != nil {
		return ErrRejected
	}
	if marker == "complete" {
		if len(legacy) != 0 {
			return ErrRejected
		}
		return nil
	}
	if marker != "pending" {
		return ErrRejected
	}
	if len(legacy) == 0 {
		_, err := s.db.ExecContext(ctx, "UPDATE metadata SET value='complete' WHERE key=?", migrationMarker)
		return err
	}
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM tasks").Scan(&count); err != nil {
		return ErrRejected
	}
	if count != 0 {
		return ErrRejected
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, name := range legacy {
		state, err := readLegacyStateFile(filepath.Join(stateDir, name), s)
		if err != nil {
			_ = tx.Rollback()
			return ErrRejected
		}
		if err := insertSQLiteState(ctx, tx, state); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, "UPDATE metadata SET value='complete' WHERE key=?", migrationMarker); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, name := range legacy {
		if err := os.Rename(filepath.Join(stateDir, name), filepath.Join(stateDir, name+".migrated")); err != nil {
			return ErrRejected
		}
	}
	directory, err := os.Open(stateDir)
	if err != nil {
		return ErrRejected
	}
	defer directory.Close()
	return directory.Sync()
}

func readLegacyStateFile(path string, store *SQLiteStore) (State, error) {
	file, err := os.Open(path)
	if err != nil {
		return State{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxStateBytes {
		return State{}, ErrRejected
	}
	decoder := json.NewDecoder(io.LimitReader(file, maxStateBytes+1))
	decoder.DisallowUnknownFields()
	var state State
	if err := decoder.Decode(&state); err != nil {
		return State{}, ErrRejected
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return State{}, ErrRejected
	}
	if err := validateState(state); err != nil || state.RepoRootIdentity != store.repoIdentity || state.RepoFSIdentity != store.repoFSID {
		return State{}, ErrRejected
	}
	return state, nil
}

func (s *SQLiteStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

func (s *SQLiteStore) Create(ctx context.Context, nextAction string) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx == nil || !validNextAction(nextAction) || s.db == nil {
		return State{}, ErrRejected
	}
	repoState, err := s.inspector.Snapshot(ctx, s.repoRoot)
	if errors.Is(err, ErrMainOnly) {
		return State{}, ErrMainOnly
	}
	if err != nil || !validRepositoryState(repoState) {
		return State{}, ErrRejected
	}
	if repoState.Branch != MainBranch {
		return State{}, ErrMainOnly
	}
	for attempts := 0; attempts < 4; attempts++ {
		taskID, err := newTaskID()
		if err != nil {
			return State{}, ErrRejected
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		state := State{
			Version: stateVersion, TaskID: taskID, RepoRootIdentity: s.repoIdentity,
			RepoFSIdentity: s.repoFSID, Branch: repoState.Branch, BaseSHA: repoState.Head,
			CurrentHeadSHA: repoState.Head, VerificationState: "RED", FailedChecks: []string{},
			NextAction: nextAction, CreatedAt: now, UpdatedAt: now,
		}
		if err := validateState(state); err != nil {
			return State{}, ErrRejected
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return State{}, ErrRejected
		}
		err = insertSQLiteState(ctx, tx, state)
		if err == nil {
			err = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
		if err == nil {
			return state, nil
		}
	}
	return State{}, ErrRejected
}

func (s *SQLiteStore) Status(ctx context.Context, taskID string) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx == nil || s.db == nil || !taskIDRE.MatchString(taskID) {
		return State{}, ErrRejected
	}
	return s.loadSQLiteState(ctx, s.db, taskID)
}

func (s *SQLiteStore) Resume(ctx context.Context, taskID string) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx == nil || s.db == nil || !taskIDRE.MatchString(taskID) {
		return State{}, ErrRejected
	}
	state, err := s.loadSQLiteState(ctx, s.db, taskID)
	if err != nil {
		return State{}, ErrRejected
	}
	repoState, err := s.inspector.Snapshot(ctx, s.repoRoot)
	if errors.Is(err, ErrMainOnly) {
		return State{}, ErrMainOnly
	}
	if err != nil || !validRepositoryState(repoState) {
		return State{}, ErrRejected
	}
	if repoState.Branch != MainBranch {
		return State{}, ErrMainOnly
	}
	if repoState.Branch != state.Branch {
		return State{}, ErrRejected
	}
	if repoState.Head == state.CurrentHeadSHA {
		return state, nil
	}
	state.CurrentHeadSHA = repoState.Head
	state.VerificationState = "RED"
	state.FailedChecks = []string{}
	state.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return State{}, ErrRejected
	}
	if err := updateSQLiteState(ctx, tx, state); err != nil {
		_ = tx.Rollback()
		return State{}, ErrRejected
	}
	if err := tx.Commit(); err != nil {
		return State{}, ErrRejected
	}
	return state, nil
}

func (s *SQLiteStore) RequireMain(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx == nil || s.db == nil {
		return ErrRejected
	}
	repoState, err := s.inspector.Snapshot(ctx, s.repoRoot)
	if errors.Is(err, ErrMainOnly) {
		return ErrMainOnly
	}
	if err != nil {
		return ErrRejected
	}
	if repoState.Branch != MainBranch {
		return ErrMainOnly
	}
	return nil
}

func insertSQLiteState(ctx context.Context, tx *sql.Tx, state State) error {
	if err := validateState(state); err != nil {
		return ErrRejected
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO tasks(
		task_id, version, repo_root_identity, repo_filesystem_identity, branch,
		base_sha, current_head_sha, last_verified_sha, verification_state,
		next_action, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		state.TaskID, state.Version, state.RepoRootIdentity, state.RepoFSIdentity, state.Branch,
		state.BaseSHA, state.CurrentHeadSHA, state.LastVerifiedSHA, state.VerificationState,
		state.NextAction, state.CreatedAt, state.UpdatedAt)
	if err != nil {
		return err
	}
	for position, check := range state.FailedChecks {
		if _, err := tx.ExecContext(ctx, "INSERT INTO task_failed_checks(task_id, position, check_name) VALUES (?, ?, ?)", state.TaskID, position, check); err != nil {
			return err
		}
	}
	return nil
}

func updateSQLiteState(ctx context.Context, tx *sql.Tx, state State) error {
	if err := validateState(state); err != nil {
		return ErrRejected
	}
	result, err := tx.ExecContext(ctx, `UPDATE tasks SET
		version=?, repo_root_identity=?, repo_filesystem_identity=?, branch=?,
		base_sha=?, current_head_sha=?, last_verified_sha=?, verification_state=?,
		next_action=?, created_at=?, updated_at=? WHERE task_id=?`,
		state.Version, state.RepoRootIdentity, state.RepoFSIdentity, state.Branch,
		state.BaseSHA, state.CurrentHeadSHA, state.LastVerifiedSHA, state.VerificationState,
		state.NextAction, state.CreatedAt, state.UpdatedAt, state.TaskID)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return ErrRejected
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM task_failed_checks WHERE task_id=?", state.TaskID); err != nil {
		return err
	}
	for position, check := range state.FailedChecks {
		if _, err := tx.ExecContext(ctx, "INSERT INTO task_failed_checks(task_id, position, check_name) VALUES (?, ?, ?)", state.TaskID, position, check); err != nil {
			return err
		}
	}
	return nil
}

func (s *SQLiteStore) loadSQLiteState(ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, taskID string) (State, error) {
	var state State
	err := queryer.QueryRowContext(ctx, `SELECT version, task_id, repo_root_identity,
		repo_filesystem_identity, branch, base_sha, current_head_sha, last_verified_sha,
		verification_state, next_action, created_at, updated_at
		FROM tasks WHERE task_id=?`, taskID).Scan(
		&state.Version, &state.TaskID, &state.RepoRootIdentity, &state.RepoFSIdentity,
		&state.Branch, &state.BaseSHA, &state.CurrentHeadSHA, &state.LastVerifiedSHA,
		&state.VerificationState, &state.NextAction, &state.CreatedAt, &state.UpdatedAt)
	if err != nil {
		return State{}, ErrRejected
	}
	rows, err := queryer.QueryContext(ctx, "SELECT position, check_name FROM task_failed_checks WHERE task_id=? ORDER BY position", taskID)
	if err != nil {
		return State{}, ErrRejected
	}
	defer rows.Close()
	for rows.Next() {
		var position int
		var check string
		if err := rows.Scan(&position, &check); err != nil || position != len(state.FailedChecks) {
			return State{}, ErrRejected
		}
		state.FailedChecks = append(state.FailedChecks, check)
	}
	if err := rows.Err(); err != nil || validateState(state) != nil || state.RepoRootIdentity != s.repoIdentity || state.RepoFSIdentity != s.repoFSID || state.TaskID != taskID {
		return State{}, ErrRejected
	}
	return state, nil
}
