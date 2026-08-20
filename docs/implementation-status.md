# Implementation status

## M3.1

- State: implementation complete locally; checkpoint pending verification and
  local commit.
- Scope: SQLite task state with strict JSON migration, private WAL/SHM,
  integrity checks, isolated workspace generations, APFS clone/copy fallback,
  lease/runtime fencing, digest-bound FD-relative integration journals and
  immutable FTS5 memory.
- Tests: package unit tests are green for `internal/taskstate`,
  `internal/workspace`, and `internal/memory`.
- Decisions: see `docs/adr/0001-m3.1-transactional-workspace.md`.
- Remaining: run full fmt/vet/test/race/MCP/module/build/Lima verification,
  then commit on local `main`.
- Exact next action: run the M3.1 verification gate and inspect the diff before
  creating the checkpoint commit.
