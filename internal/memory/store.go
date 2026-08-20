// Package memory provides immutable, bounded, full-text searchable memory.
package memory

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"database/sql"
	_ "modernc.org/sqlite"
)

const (
	maxQueryBytes   = 512
	maxContentBytes = 1 << 20
	maxSearchLimit  = 100
)

var (
	ErrRejected = errors.New("memory request rejected")
)

//go:embed migrations/*.sql
var migrations embed.FS

type Entry struct {
	ID           string    `json:"id"`
	RepositoryID string    `json:"repository_id"`
	Content      string    `json:"content"`
	CreatedAt    time.Time `json:"created_at"`
}

type Store struct {
	db *sql.DB
	mu sync.Mutex
}

type SearchResult struct {
	Entry Entry
	Rank  float64
}

func Open(path string) (*Store, error) {
	if path == "" || !filepath.IsAbs(path) {
		return nil, ErrRejected
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, ErrRejected
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
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
	if err := file.Close(); err != nil {
		return nil, ErrRejected
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, ErrRejected
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db}
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

func (s *Store) Append(ctx context.Context, repositoryID, content string) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx == nil || s.db == nil || !validIdentity(repositoryID) || !validContent(content) {
		return Entry{}, ErrRejected
	}
	id, err := newID()
	if err != nil {
		return Entry{}, ErrRejected
	}
	entry := Entry{ID: id, RepositoryID: repositoryID, Content: content, CreatedAt: time.Now().UTC()}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Entry{}, ErrRejected
	}
	result, err := tx.ExecContext(ctx, "INSERT INTO memory_entries(entry_id, repository_id, content, created_at) VALUES (?, ?, ?, ?)", entry.ID, entry.RepositoryID, entry.Content, entry.CreatedAt.Format(time.RFC3339Nano))
	if err == nil {
		var rowid int64
		err = tx.QueryRowContext(ctx, "SELECT rowid FROM memory_entries WHERE entry_id=?", entry.ID).Scan(&rowid)
		if err == nil {
			_, err = tx.ExecContext(ctx, "INSERT INTO memory_fts(rowid, content) VALUES (?, ?)", rowid, entry.Content)
		}
	}
	if err != nil {
		_ = tx.Rollback()
		return Entry{}, ErrRejected
	}
	if _, err := result.RowsAffected(); err != nil {
		_ = tx.Rollback()
		return Entry{}, ErrRejected
	}
	if err := tx.Commit(); err != nil {
		return Entry{}, ErrRejected
	}
	return entry, nil
}

func (s *Store) Search(ctx context.Context, repositoryID, query string, limit int) ([]SearchResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx == nil || s.db == nil || !validIdentity(repositoryID) || !validQuery(query) || limit <= 0 || limit > maxSearchLimit {
		return nil, ErrRejected
	}
	ftsQuery := quoteQuery(query)
	rows, err := s.db.QueryContext(ctx, `SELECT e.entry_id, e.repository_id, e.content, e.created_at, bm25(memory_fts)
		FROM memory_fts JOIN memory_entries e ON e.rowid=memory_fts.rowid
		WHERE e.repository_id=? AND memory_fts MATCH ?
		ORDER BY bm25(memory_fts), e.created_at, e.entry_id LIMIT ?`, repositoryID, ftsQuery, limit)
	if err != nil {
		return nil, ErrRejected
	}
	defer rows.Close()
	results := make([]SearchResult, 0, limit)
	for rows.Next() {
		var entry Entry
		var created string
		var rank float64
		if err := rows.Scan(&entry.ID, &entry.RepositoryID, &entry.Content, &created, &rank); err != nil {
			return nil, ErrRejected
		}
		entry.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return nil, ErrRejected
		}
		results = append(results, SearchResult{Entry: entry, Rank: rank})
	}
	if err := rows.Err(); err != nil {
		return nil, ErrRejected
	}
	return results, nil
}

func quoteQuery(query string) string {
	parts := strings.Fields(query)
	quoted := make([]string, 0, len(parts))
	for _, part := range parts {
		quoted = append(quoted, `"`+strings.ReplaceAll(part, `"`, `""`)+`"`)
	}
	return strings.Join(quoted, " AND ")
}

func validIdentity(value string) bool {
	return len(value) == 64 && utf8.ValidString(value) && strings.Trim(value, "0123456789abcdef") == ""
}

func validContent(value string) bool {
	return len(value) > 0 && len(value) <= maxContentBytes && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

func validQuery(value string) bool {
	return len(value) > 0 && len(value) <= maxQueryBytes && utf8.ValidString(value) && !strings.ContainsRune(value, 0) && strings.TrimSpace(value) != ""
}

func newID() (string, error) {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return "mem_" + hex.EncodeToString(digest[:16]), nil
}
