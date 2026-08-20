# ADR 0011: M3 production control plane

## Decision

Use internal/controlplane as the single production composition root. Register
only typed MCP operations and keep the live repository authority separate from
leased TaskWorkspace generations. Apple container is the production runtime;
process execution is supervised and argv-only. Publication is a two-step
plan/execute API with fresh binding and scoped confirmation.

## Rationale

The package implementations already enforce the individual M3 invariants, but
tests and a legacy MCP server do not make a product. One composition root makes
recovery, authentication, runtime ownership, environment identity, durable
events, loop resume, and publication rechecks part of every production request.
Typed tools prevent a client from bypassing those authorities with host shell
or arbitrary paths.

## Consequences

The MCP surface is intentionally narrower than a general development shell.
External publication remains disabled by default. The real Apple and control-
plane E2E tests are required evidence, while Lima remains a validation/fallback
adapter rather than the production default.
