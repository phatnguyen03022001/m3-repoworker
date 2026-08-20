# Architecture

The production composition root is internal/controlplane. It opens the live
repository authority and isolated workspace repository, authenticates the
local-dev principal, opens durable task/memory/event state, recovers runtime
records, and wires environment, scheduler, process, publication, and loop
services before MCP registration.

The runtime path is:

1. workspace_create materializes a leased candidate generation.
2. runtime_create/start binds one Apple container to that generation.
3. process_run accepts typed executable/argv/cwd/timeout only and starts
   through the container backend and supervised process groups.
4. verification_plan/run binds native intelligence commands to candidate,
   environment, policy, and repository identities.
5. run_* persists bounded events; loop_* resumes from durable state and
   refreshes bindings after candidate changes.
6. publication_plan produces a plan; publication_execute revalidates the
   candidate and confirmation before any allowed mutation.
7. workspace_integrate applies the recovery-safe, lease-fenced journal.

The internal packages remain intentionally separated: task state, workspace,
security, process, runtime, scheduler, environment, intelligence, events,
loop, and publication each own their contracts. MCP is an adapter over the
control plane, not a generic host API.
