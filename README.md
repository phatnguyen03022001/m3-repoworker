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
a reproducible local build, and Lima template validation. The executable is
written to `bin/repoworker`.

To run the server directly, explicitly configure the repository root:

```sh
go run ./cmd/repoworker -repo-root /absolute/path/to/repository
```

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
  `.env.*`, credential, secret, token, private-key, or common key-store files.
  Rejected MCP requests return only `request rejected`, never a filesystem
  path, root, or underlying error.
- `apply_patch` accepts exactly one existing, non-symlink UTF-8 file and one
  strict unified diff (`--- a/path`, `+++ b/path`). Every hunk must match
  exactly, and the replacement is atomic. It rejects file creation, deletion,
  renames, multi-file patches, no-context changes, and protected paths.
- `repo_status`, `repo_read`, and `repo_search` are declared read-only,
  idempotent, non-destructive, and closed-world. `apply_patch` is explicitly
  marked as a mutating, non-idempotent, destructive, closed-world operation.
- The process does not execute shell commands, call Git, load credentials, or
  make network requests.
- `repoworker-prod.yaml` preserves the Lima isolation baseline: no host mounts,
  no configured guest networks, and an ignored forwarding rule covering all
  guest ports. Do not relax these settings without an explicit security review.
- Keep local credentials only in ignored `.env` or `.env.local` files. Never
  add real credentials to source, tests, docs, or generated artifacts.
