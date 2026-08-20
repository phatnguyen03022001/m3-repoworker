# Security model

The repository root is canonicalized and assigned an opaque filesystem
identity. State is required outside the repository and uses private directory,
database, WAL/SHM, and file permissions. Task state decodes strict schemas and
rejects corruption, unknown fields, identity mismatches, likely secrets, and
ambiguous migration state.

Workspace materialization rejects symlink entries and excludes live Git/cache
and generated binary trees. Integration paths are repository-relative and
validated against traversal, symlink, and absolute-path escapes. Live writes
use a held root descriptor, `openat` with `O_NOFOLLOW`, atomic temp-file
replacement, `fsync`, and directory synchronization.

A lease is an expiring ownership record with a monotonic fence. An expired
lease quarantines the generation; an old lease cannot renew, reserve a runtime,
or mutate the live repository. Candidate and base digests prevent TOCTOU
integration. Full principal/capability policy is the M3.2 boundary.

M3.2 adds repository enrollment bound to repository/filesystem identities,
single-use session nonces, typed mount/network/execution compilation, and
one-time confirmation tokens for destructive and publication classes. A live
repository cannot be compiled as a runtime mount, full network is denied, and
host shell executables are rejected. Audit detail is bounded and redacted.
