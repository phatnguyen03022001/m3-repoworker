# Repository development policy

This repository is MAIN-ONLY.

- All development happens directly on local `main`.
- Do not create Git branches or worktrees backed by new branches.
- Do not automatically switch to or create feature, task, or agent branches.
- If the checkout is not on `main`, stop and return to `main` before modifying files.
- Local commits on `main` are allowed.
- Do not push or mutate remotes without explicit user instruction.
- Preserve existing intentional work; never discard it with destructive resets.

Before making changes, inspect `git status` and confirm the current branch is
`main`. After changes, run the repository's relevant verification and leave the
working tree clean when creating a local checkpoint.
