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
