# Task state invariants

RepoWorker task state is durable operational state, not an independent
publication authority.

- Tasks and workspace generations are bound to one canonical repository and
  the main branch.
- State lives outside the checkout and rejects corrupt, oversized, unknown,
  mismatched, or malformed records.
- Verification state is derived only from a current candidate/environment/
  policy binding; a moved snapshot invalidates it.
- MCP failures are sanitized and high-confidence credential material is
  rejected from durable text.
- Runtime, process, event, loop, and publication operations use opaque IDs and
  scoped capabilities. No client can switch branches, create worktrees, or
  submit arbitrary shell.
- Integration remains local and lease-fenced. Publication is an explicit,
  plan-first control-plane operation with a separate confirmation gate; it is
  not implied by a green local check.
