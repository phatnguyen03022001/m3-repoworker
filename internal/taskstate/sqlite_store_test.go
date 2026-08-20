package taskstate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSQLiteStoreContractAndPrivateWAL(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	stateRoot := t.TempDir()
	inspector := &fakeInspector{state: RepositoryState{Branch: MainBranch, Head: strings.Repeat("a", 40)}}
	repoFSID, err := filesystemIdentityAtPath(repoRoot)
	if err != nil {
		t.Fatalf("filesystemIdentityAtPath() error = %v", err)
	}
	store, err := NewSQLiteWithInspector(repoRoot, repoFSID, stateRoot, inspector)
	if err != nil {
		t.Fatalf("NewSQLiteWithInspector() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	created, err := store.Create(context.Background(), "sqlite handoff")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	loaded, err := store.Status(context.Background(), created.TaskID)
	if err != nil || loaded.TaskID != created.TaskID {
		t.Fatalf("Status() = %#v, %v", loaded, err)
	}

	databasePath := filepath.Join(stateRoot, created.RepoRootIdentity, databaseFileName)
	info, err := os.Stat(databasePath)
	if err != nil {
		t.Fatalf("stat database: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("database mode = %o, want 600", info.Mode().Perm())
	}
	directoryInfo, err := os.Stat(filepath.Dir(databasePath))
	if err != nil {
		t.Fatalf("stat database directory: %v", err)
	}
	if directoryInfo.Mode().Perm() != 0o700 {
		t.Fatalf("database directory mode = %o, want 700", directoryInfo.Mode().Perm())
	}
	if _, err := os.Stat(databasePath + "-wal"); err != nil {
		t.Fatalf("WAL file missing: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(databasePath))
	if err != nil {
		t.Fatalf("read database directory: %v", err)
	}
	for _, entry := range entries {
		if entry.Name() == created.TaskID+".json" {
			t.Fatal("SQLite store unexpectedly emitted a JSON task file")
		}
	}

	var synchronous, foreignKeys int
	if err := store.db.QueryRow("PRAGMA synchronous").Scan(&synchronous); err != nil || synchronous != 2 {
		t.Fatalf("synchronous pragma = %d, %v", synchronous, err)
	}
	if err := store.db.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		t.Fatalf("foreign_keys pragma = %d, %v", foreignKeys, err)
	}
}

func TestSQLiteStoreMigratesLegacyJSONExactlyOnce(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	stateRoot := t.TempDir()
	inspector := &fakeInspector{state: RepositoryState{Branch: MainBranch, Head: strings.Repeat("a", 40)}}
	legacy, err := NewWithInspector(repoRoot, stateRoot, inspector)
	if err != nil {
		t.Fatalf("NewWithInspector() error = %v", err)
	}
	created, err := legacy.Create(context.Background(), "legacy handoff")
	if err != nil {
		t.Fatalf("legacy Create() error = %v", err)
	}
	repoFSID := created.RepoFSIdentity
	store, err := NewSQLiteWithInspector(repoRoot, repoFSID, stateRoot, inspector)
	if err != nil {
		t.Fatalf("NewSQLiteWithInspector() migration error = %v", err)
	}
	loaded, err := store.Status(context.Background(), created.TaskID)
	if err != nil || loaded.NextAction != created.NextAction {
		t.Fatalf("migrated state = %#v, %v", loaded, err)
	}
	if _, err := os.Stat(filepath.Join(stateRoot, created.RepoRootIdentity, created.TaskID+".json.migrated")); err != nil {
		t.Fatalf("migrated legacy file missing: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	second, err := NewSQLiteWithInspector(repoRoot, repoFSID, stateRoot, inspector)
	if err != nil {
		t.Fatalf("second SQLite open error = %v", err)
	}
	_ = second.Close()
}

func TestSQLiteStoreRejectsCorruptLegacyMigration(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	stateRoot := t.TempDir()
	inspector := &fakeInspector{state: RepositoryState{Branch: MainBranch, Head: strings.Repeat("a", 40)}}
	legacy, err := NewWithInspector(repoRoot, stateRoot, inspector)
	if err != nil {
		t.Fatalf("NewWithInspector() error = %v", err)
	}
	created, err := legacy.Create(context.Background(), "legacy handoff")
	if err != nil {
		t.Fatalf("legacy Create() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateRoot, created.RepoRootIdentity, created.TaskID+".json"), []byte("{broken\n"), 0o600); err != nil {
		t.Fatalf("corrupt legacy state: %v", err)
	}
	if _, err := NewSQLiteWithInspector(repoRoot, created.RepoFSIdentity, stateRoot, inspector); !errors.Is(err, ErrRejected) {
		t.Fatalf("corrupt migration error = %v, want ErrRejected", err)
	}
}

func TestSQLiteStoreRejectsMetadataIdentityMismatch(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	stateRoot := t.TempDir()
	inspector := &fakeInspector{state: RepositoryState{Branch: MainBranch, Head: strings.Repeat("a", 40)}}
	repoFSID, err := filesystemIdentityAtPath(repoRoot)
	if err != nil {
		t.Fatalf("filesystemIdentityAtPath() error = %v", err)
	}
	store, err := NewSQLiteWithInspector(repoRoot, repoFSID, stateRoot, inspector)
	if err != nil {
		t.Fatalf("NewSQLiteWithInspector() error = %v", err)
	}
	databasePath := filepath.Join(stateRoot, store.repoIdentity, databaseFileName)
	if _, err := store.db.Exec("UPDATE metadata SET value=? WHERE key='repo_filesystem_identity'", strings.Repeat("b", 64)); err != nil {
		t.Fatalf("corrupt metadata: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := NewSQLiteWithInspector(repoRoot, repoFSID, stateRoot, inspector); !errors.Is(err, ErrRejected) {
		t.Fatalf("identity mismatch error = %v, want ErrRejected", err)
	}
	if _, err := os.Stat(databasePath); err != nil {
		t.Fatalf("database disappeared after failed reopen: %v", err)
	}
}
