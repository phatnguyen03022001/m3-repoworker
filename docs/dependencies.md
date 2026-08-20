# Dependencies

Pinned runtime dependencies currently include:

- `github.com/modelcontextprotocol/go-sdk v1.7.0`
- `golang.org/x/sys v0.47.0`
- `golang.org/x/sync v0.21.0`
- `modernc.org/sqlite v1.57.0`
- `github.com/creack/pty v1.1.18` (optional PTY adapter, M3.3)

SQLite is used through `database/sql` without CGO or an ORM. The modernc
dependency is introduced for M3.1 state and memory persistence. Run `go mod
verify` and `govulncheck ./...` after each dependency change; publication and
runtime-specific dependencies remain deferred until their milestones.
