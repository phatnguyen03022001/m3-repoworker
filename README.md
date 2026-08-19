# RepoWorker

RepoWorker is a local Model Context Protocol (MCP) server. It exposes a
closed-world status tool plus confined repository read, search, and patch tools
over stdio.

## Local development

Requirements: Go 1.26.6 and Lima (only for validating the supplied VM template).

```sh
make verify
```

The command runs formatting, static analysis, normal and race-detector tests,
the MCP integration test, `go mod verify`, a reproducible local build, and Lima
template validation. The executable is written to `bin/repoworker`.

To run the server directly, explicitly configure the repository root:

```sh
go run ./cmd/repoworker -repo-root /absolute/path/to/repository
```

The same `-repo-root` argument must be present when another process such as
`tunnel-client` launches `bin/repoworker`; the binary intentionally has no
implicit repository root.

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
- Rejected MCP requests return only `request rejected`, never a filesystem
  path, repository root, protected filename, or underlying host error.
- `repo_status`, `repo_read`, and `repo_search` are declared read-only,
  idempotent, non-destructive, and closed-world. `apply_patch` is explicitly
  marked as a mutating, non-idempotent, destructive, closed-world operation.
- The process does not execute shell commands, call Git, load credentials, or
  make network requests.
- `repoworker-prod.yaml` preserves the hardened Lima isolation baseline: no
  host mounts, no configured guest networks, SSH over VSOCK disabled, and an
  ignored forwarding rule covering all guest application ports. Do not relax
  these settings without an explicit security review.
- Keep local credentials only in ignored `.env` or `.env.local` files. Never
  add real credentials to source, tests, docs, or generated artifacts.

## Current scope

RepoWorker currently implements repository status, confined read/search, and
strict patch application. Verification execution, persistent task handoff,
Gatekeeper authority, candidate binding, and publishing are intentionally not
implemented yet. A green local `make verify` is a development check, not final
publish authorization.
