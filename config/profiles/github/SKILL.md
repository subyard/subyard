---
name: subyard-github
description: Request temporary GitHub App authorization for commands. Use for every GitHub CLI (gh) command, because gh without subyard-github run is not authorized.
---

<!-- subyard-managed-v1 -->
Run GitHub CLI commands as `subyard-github run -- gh ...`, rather than `gh auth login`.
For an arbitrary command or ambiguous repository, use
`subyard-github run --repo owner/repo -- <command> [args...]`; the child receives `GH_REPO`.
App repository/permission grants apply; tokens cannot combine installations.
Direct Git commands do not mint an initial CLI token; use `--repo` when a Git hook needs `GH_TOKEN`.

`subyard-github status --git` reports the owner-selected Git mode: `off` preserves
original authentication; `broker-first` tries App access first; `broker-last` tries
original access first. These modes support ordinary GitHub HTTPS/SSH Git commands,
but do not authorize `gh`. Explicit `run` keeps HTTPS broker-only and SSH original.
App pushes need Contents write and, for workflow changes, Workflows write.
Never automatically retry a failed/partial push under another identity.

See [Git modes, owner configuration and compatibility limits](https://github.com/Subyard/Subyard/blob/master/docs/github.md#ordinary-git-with-original-authentication-fallback).
