---
name: subyard-cleanup
description: Inspect disk usage and clean explicitly disposable caches, or recreate a selected yard while preserving registered projects.
---

# Yard cleanup

Use `yard status --space` to inspect storage and `yard cleanup --help` to inspect the current command contract. Select the yard explicitly when several yards exist.

`yard cleanup` and `yard cleanup --soft` remove only directories declared by the selected environment profiles in `CLEANUP_CACHES`. They preserve each cache root and its permissions, do not follow symlinks, and preserve nested mounts. Missing directories are harmless. An active build may need a retry. Installed toolchains, SDKs and browser runtimes are not disposable caches.

General temporary files, editor recovery data, credentials, agent histories, project build outputs and undeclared directories need manual inspection. Identify their owner and purpose before removing anything; do not infer a safe deletion target from its name, age or size. Never suggest blanket `/tmp` deletion.

`yard cleanup --hard` stops the selected yard, independently copies its native data volume outside the resources that teardown deletes, verifies its registered project trees, and uses the ordinary lifecycle to create a fresh environment. Only complete registered project wrappers and exact registration/reservation data are restored. External bind sources stay on the host; their contents are neither copied nor replaced. Other guest data, including agent authorizations, history and user environment changes outside projects, is discarded.

Before deletion, cleanup refuses unregistered workspaces, special project files, nested project mounts and Git metadata dependencies outside the backup (including external alternates). The existing guest must be bootable enough to inspect its read-only backup. Cleanup can recover broken selected-yard migrations; it does not reset shared migration history.

A failed or interrupted hard cleanup retains an independent backup and a private recovery checkpoint. Keep the same configuration and runtime, then rerun `yard cleanup --hard` for that yard. Do not manually delete the backup volume or checkpoint. The command removes them only after restoration and verification succeed.

Review the operation plan before confirming. Report measured space changes as observations: active builds, hardlinks, open files and shared storage can affect the result.
