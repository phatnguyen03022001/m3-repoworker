CREATE TABLE IF NOT EXISTS memory_entries (
    rowid INTEGER PRIMARY KEY AUTOINCREMENT,
    entry_id TEXT NOT NULL UNIQUE,
    repository_id TEXT NOT NULL,
    content TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE VIRTUAL TABLE IF NOT EXISTS memory_fts USING fts5(
    content,
    content='memory_entries',
    content_rowid='rowid'
);

CREATE TRIGGER IF NOT EXISTS memory_entries_immutable_update
BEFORE UPDATE ON memory_entries
BEGIN
    SELECT RAISE(ABORT, 'memory entries are immutable');
END;

CREATE TRIGGER IF NOT EXISTS memory_entries_immutable_delete
BEFORE DELETE ON memory_entries
BEGIN
    SELECT RAISE(ABORT, 'memory entries are immutable');
END;
