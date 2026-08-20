# Architecture

The production composition root is internal/controlplane. It opens the live
repository authority only after a transport authentication boundary returns a
principal. A connector may supply signed HTTP metadata, or an explicitly
configured trusted local transport may supply a principal; there is no
`local-dev` fallback and no MCP argument for identity. The session binds that
principal to the transport authentication context and repository identity.
It opens durable task/memory/event state, recovers runtime records, validates
and rehydrates workspace generations, and only then exposes mutation services.

The runtime path is:

1. `repo_status` exposes only opaque repository/principal/session identities;
   `repo_git_status` exposes a bounded, read-only Git summary.
2. workspace_create materializes a leased candidate generation.
3. runtime_create/start binds one Apple container to that generation.
4. process_run accepts typed executable/argv/cwd/timeout only and starts
   through the container backend and supervised process groups.
5. verification_plan/run binds native intelligence commands to candidate,
   environment, policy, and repository identities.
6. run_* persists bounded events; loop_* persists its configuration and state,
   resumes from durable state after reopen, and refreshes bindings after
   candidate changes. A stopped or stale runtime is never resurrected: resume
   provisions a new runtime generation and obtains a new lease fence.
7. publication_plan produces a plan; publication_execute revalidates the
   candidate and confirmation before any allowed mutation.
8. workspace_integrate applies the recovery-safe, lease-fenced journal.

The internal packages remain intentionally separated: task state, workspace,
security, process, runtime, scheduler, environment, intelligence, events,
loop, and publication each own their contracts. MCP is an exact closed-world
adapter over the control plane, not a generic host API. The production set is
the 31 tools listed in README; operator confirmation is outside that
autonomous surface. Plan tools register a pending binding in the control
plane; only the private operator socket can convert that pending record into a
token. Mutating tool calls are guarded at the MCP receiving boundary using the
SDK request metadata and session handle, before the typed handler runs.
