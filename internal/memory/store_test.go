package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppendOnlyFTSStoreSearchesDeterministically(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "memory.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	repositoryID := strings.Repeat("a", 64)
	first, err := store.Append(context.Background(), repositoryID, "workspace generation is isolated")
	if err != nil {
		t.Fatalf("Append(first) error = %v", err)
	}
	second, err := store.Append(context.Background(), repositoryID, "verification binds a candidate snapshot")
	if err != nil {
		t.Fatalf("Append(second) error = %v", err)
	}
	if first.ID == second.ID || first.CreatedAt.IsZero() || second.CreatedAt.IsZero() {
		t.Fatalf("entries = %#v %#v", first, second)
	}
	results, err := store.Search(context.Background(), repositoryID, "candidate snapshot", 10)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(results) != 1 || results[0].Entry.ID != second.ID {
		t.Fatalf("Search() = %#v", results)
	}
	if _, err := store.db.Exec("UPDATE memory_entries SET content='mutated'"); err == nil {
		t.Fatal("immutable memory update unexpectedly succeeded")
	}
	if _, err := store.db.Exec("DELETE FROM memory_entries"); err == nil {
		t.Fatal("immutable memory delete unexpectedly succeeded")
	}
	if _, err := store.Search(context.Background(), repositoryID, strings.Repeat("x", maxQueryBytes+1), 1); !errors.Is(err, ErrRejected) {
		t.Fatalf("oversized Search() error = %v, want ErrRejected", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	restarted, err := Open(path)
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	defer restarted.Close()
	results, err = restarted.Search(context.Background(), repositoryID, "isolated", 10)
	if err != nil || len(results) != 1 || results[0].Entry.ID != first.ID {
		t.Fatalf("reopened Search() = %#v, %v", results, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("database mode = %v, %v", info.Mode().Perm(), err)
	}
}

func TestMemoryValidationIsDenyByDefault(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	if _, err := store.Append(context.Background(), "bad", "content"); !errors.Is(err, ErrRejected) {
		t.Fatalf("invalid identity error = %v", err)
	}
	if _, err := store.Append(context.Background(), strings.Repeat("b", 64), ""); !errors.Is(err, ErrRejected) {
		t.Fatalf("empty content error = %v", err)
	}
	if _, err := store.Search(context.Background(), strings.Repeat("b", 64), "", 10); !errors.Is(err, ErrRejected) {
		t.Fatalf("empty query error = %v", err)
	}
	if _, err := store.Search(context.Background(), strings.Repeat("b", 64), "content", 0); !errors.Is(err, ErrRejected) {
		t.Fatalf("invalid limit error = %v", err)
	}
}
