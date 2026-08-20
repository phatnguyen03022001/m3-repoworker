# Operations

Use `make verify` as the local authority. RepoWorker state must remain outside
the live checkout. If startup rejects the state directory, database integrity,
repository identity, migration, or workspace recovery, preserve the state
directory and inspect it before attempting repair.

Workspace generations can be discarded or quarantined by their owning layer;
never reset or overwrite the live repository to recover a task. Local
checkpoint commits are made on `main` only and are not pushed automatically.
