---
name: subyard-cleanup
description: Inspect disk usage and clean explicitly disposable caches, or recreate a selected yard while preserving registered projects. Also use before deleting files through a variable or command-substituted path.
---

`yard status --space` measures storage; use `yard cleanup --help` for options.
Soft cleanup removes only profile-declared `CLEANUP_CACHES`, preserving roots,
permissions and nested mounts without following symlinks. Toolchains are not caches.

For manual deletion, use a literal path or an empty-safe variable (`rm -rf -- "${dir:?}"/*`).
For command-substituted paths, verify exactly one existing directory before deletion;
never infer safety from name/age/size or delete `/tmp` wholesale.

Review the hard-cleanup operation plan before confirming.
Hard cleanup stops and recreates the selected yard. It backs up its native data volume
independently and restores only complete registered project wrappers and registration data.
External bind sources stay on the host; other guest data, including agent authorizations
and histories, is discarded. The guest must be bootable enough to inspect its backup.
Unregistered workspaces, nested project mounts and external Git dependencies can refuse cleanup.
It can recover selected-yard migration failures but does not reset shared migration history.

After interruption, keep the same configuration/runtime and rerun hard cleanup for the
same yard. Do not delete its backup or recovery checkpoint; cleanup retires them after verification.

[Hard-cleanup plans and recovery limits](https://github.com/Subyard/Subyard/blob/master/docs/control-plane.md#selected-yard-cleanup).
