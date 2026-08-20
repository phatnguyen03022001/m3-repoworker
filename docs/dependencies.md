# Dependencies

Pinned runtime dependencies include:

- github.com/modelcontextprotocol/go-sdk v1.7.0
- golang.org/x/sys v0.47.0
- golang.org/x/sync v0.21.0
- modernc.org/sqlite v1.57.0
- github.com/creack/pty v1.1.18

SQLite uses database/sql without CGO or an ORM. Apple container and Lima
backends are CLI adapters; no runtime-specific Go SDK is required.

make bootstrap downloads modules into private repository-local .cache/go-mod
and .cache/go-path roots. make offline-verify sets GOPROXY=off and
GOSUMDB=off; scripts/cold-cache-verify.sh proves bootstrap and offline
verification from fresh private caches. Run go mod verify and govulncheck
./... after dependency changes.
