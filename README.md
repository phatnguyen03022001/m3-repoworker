# RepoWorker

RepoWorker is a local Model Context Protocol (MCP) server. It exposes closed-
world repository status, confined repository read/search/patch tools, and
persistent development task handoff over stdio.

## Local development

Requirements: Go 1.26.6, Git, and Lima (only for validating the supplied VM
template).

```sh
make verify
```

The command runs formatting, static analysis, normal and race-detector tests,
the MCP integration tests, `go mod verify`, a reproducible local build, and
Lima template validation. The executable is written to `bin/repoworker`.

To run the server directly, explicitly configure the repository root:

```sh
go run ./cmd/repoworker -repo-root /absolute/path/to/repository
```

Persistent task state defaults to the per-user configuration directory outside
the repository. An explicit absolute state directory may be supplied when
needed:

```sh
go run ./cmd/repoworker \
  -repo-root /absolute/path/to/repository \
  -state-dir /absolute/path/to/private/state
```

The same `-repo-root` argument must be present when another process such as
`tunnel-client` launches `bin/repoworker`; the binary intentionally has no
implicit repository root. If `-state-dir` is supplied it must also remain
outside the configured repository.

The server uses newline-delimited JSON on standard input and output. Do not
write human-readable logs to standard output: that stream is reserved for MCP
messages.

## Security boundaries

- The MCP server uses stdio only; it does not open a network listener.
- A repository root is mandatory, must be an existing absolute directory, and
  is canonicalized before use. Repo paths are always relative to that root.
  Absolute paths, `..` traversal, Windows volume paths, backslashes, and
  symlink escapes are rejected.
- `repo_read` and `repo_search` only handle bounded UTF-8 regular files. They
  do not follow symlinks while searching and never expose `.git`, `.env`,
  `.env.*`, common credential/secret/token stores, private keys, or common key
  stores. Source-code files such as `token.go` remain accessible so security
  and authentication code can still be edited.
- Repository search is bounded by query size, per-file size, match count,
  scanned file count, scanned bytes, MCP output size, and per-match preview
  size. Search also honors MCP request cancellation. A truncated result is
  reported with `truncated: true`.
- `apply_patch` accepts exactly one existing, non-symlink UTF-8 file and one
  strict unified diff (`--- a/path`, `+++ b/path`). Every hunk must match
  exactly, later and multi-hunk offsets are validated against the original
  file, and the replacement is atomic. File mutations are serialized so two
  concurrent patches cannot both commit from the same stale snapshot.
- `apply_patch` rejects file creation, deletion, renames, multi-file patches,
  no-context changes, and protected paths.
- `task_create` generates the task identifier itself, binds the task to a hash
  of the canonical repository root plus the current Git branch and commit, and
  initializes verification state to `RED`.
- `task_status` reads only persisted state. `task_resume` refuses a repository
  or branch mismatch; if the branch HEAD moved, it updates `current_head_sha`
  and forces the task back to `RED`.
- Task state is written outside the repository with private directory/file
  permissions, atomic replace, file and directory sync, bounded strict JSON
  decoding, and repository-identity checks. Corrupt or mismatched state fails
  closed.
- Git use is limited to fixed local metadata reads (`rev-parse` and
  `symbolic-ref`) with no shell, hooks, prompts, network operations, or arbitrary
  command input. RepoWorker still has no GitHub credentials and cannot publish.
- Rejected MCP requests return only `request rejected`, never a filesystem
  path, repository root, protected filename, task-state location, or underlying
  host error.
- `repo_status`, `repo_read`, `repo_search`, and `task_status` are read-only,
  idempotent, non-destructive, and closed-world. `apply_patch` is explicitly
  destructive. `task_create` and `task_resume` mutate only RepoWorker-owned
  handoff state and are non-destructive to repository contents.
- `repoworker-prod.yaml` preserves the hardened Lima isolation baseline: no
  host mounts, no configured guest networks, SSH over VSOCK disabled, and an
  ignored forwarding rule covering all guest application ports. Do not relax
  these settings without an explicit security review.
- Keep local credentials only in ignored `.env` or `.env.local` files. Never
  add real credentials to source, tests, docs, generated artifacts, or task
  state.

## Task handoff state

A task persists these development fields across RepoWorker restarts:

- `task_id`
- `repo_root_identity`
- `branch`
- `base_sha`
- `current_head_sha`
- `last_verified_sha`
- `verification_state` (`RED` or `GREEN`)
- `failed_checks`
- `next_action`

`last_verified_sha` and `GREEN` are reserved for the verification layer. This
phase never manufactures a GREEN state.

## Current scope

RepoWorker currently implements repository status, confined read/search,
strict patch application, and persistent task create/status/resume handoff.
Verification execution, Gatekeeper authority, candidate binding, and publishing
are intentionally not implemented yet. A green local `make verify` is a
development check, not final publish authorization.
