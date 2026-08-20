# Recovery

Startup performs SQLite integrity and foreign-key checks before task mutation.
Workspace integration journals are replayed before a new mutation. If the
live file digest equals the journal after-digest, recovery advances the cursor;
if it equals neither the before- nor after-digest, the journal is quarantined
and startup fails closed. A before-digest step is also quarantined because
recovery cannot safely assume a lease or replay an external filesystem
transaction.

Legacy JSON migration is strict and one-time. Valid records are inserted into
SQLite in one transaction and renamed to `.migrated`; corrupt, mismatched, or
ambiguous records stop startup. There is no dual-write path.

Run recovery reopens the event store only after SQLite integrity checks. Events
are replayed from a caller-owned sequence cursor; missing or invalid payloads
fail closed. Checkpoints retain the candidate snapshot, environment, policy,
and verifier state needed to resume without guessing. Artifact reads verify
path containment, size, and digest before exposing bytes. Retention removes
only terminal runs and their private artifacts. FSEvents-style notifications
are advisory invalidation hints; snapshot authority remains digest verification
against the live repository and workspace generation.

The autonomous loop replays its `loop.state` events from sequence zero in
bounded pages and resumes at the last persisted phase. Model proposals are
revalidated against the run binding before the local authority executes them.
Failed action fingerprints are persisted, so a retry cannot repeat the same
failed action; the retry budget is bounded and exhausted loops fail closed.
Destructive or ambiguous plans stop at a human checkpoint.

Publication adapters revalidate the verified candidate immediately before and
after a mutation. Git push also reads the expected remote ref immediately
before the push and fails closed on a mismatch. All external adapters are
disabled unless explicitly enabled, default to dry-run, and require a scoped
confirmation token; credentials are never placed in command arguments.
