# Task state invariants

RepoWorker task handoff is development state, not publish authority.

The implementation must preserve these invariants:

1. RepoWorker generates every `task_id`.
2. A task is bound to one canonical repository identity and one Git branch.
3. `base_sha` is captured when the task is created.
4. `current_head_sha` may advance only after local Git metadata is re-read.
5. A moved HEAD forces `verification_state` to `RED`.
6. This layer never manufactures `GREEN` or `last_verified_sha`.
7. Persistent state lives outside the repository and is atomically replaced.
8. Corrupt, oversized, unknown-field, mismatched, or malformed state fails closed.
9. MCP-visible failures are sanitized to `request rejected`.
10. Git access is fixed local metadata inspection only; no shell, hooks, prompts, network access, GitHub credentials, publishing, or arbitrary Git arguments.

Final publish authority remains reserved for the future independent Gatekeeper.
