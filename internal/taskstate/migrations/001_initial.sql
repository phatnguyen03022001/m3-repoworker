CREATE TABLE IF NOT EXISTS metadata (
    key TEXT PRIMARY KEY NOT NULL,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS tasks (
    task_id TEXT PRIMARY KEY NOT NULL,
    version INTEGER NOT NULL,
    repo_root_identity TEXT NOT NULL,
    repo_filesystem_identity TEXT NOT NULL,
    branch TEXT NOT NULL,
    base_sha TEXT NOT NULL,
    current_head_sha TEXT NOT NULL,
    last_verified_sha TEXT NOT NULL DEFAULT '',
    verification_state TEXT NOT NULL,
    next_action TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    CHECK (verification_state IN ('RED', 'GREEN'))
);

CREATE TABLE IF NOT EXISTS task_failed_checks (
    task_id TEXT NOT NULL,
    position INTEGER NOT NULL,
    check_name TEXT NOT NULL,
    PRIMARY KEY (task_id, position),
    FOREIGN KEY (task_id) REFERENCES tasks(task_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS task_failed_checks_task_idx
    ON task_failed_checks(task_id, position);
