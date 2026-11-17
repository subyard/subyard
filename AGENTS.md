# Subyard — agent instructions

This is the **public** repository. Keep everything here generic and in English; no private
data. Architecture, accepted decisions and release-transition design live in `docs/reference/`.
Operator-specific requirements and planning may live in the optional private overlay.

## Development workflow

For features, fixes, refactoring, tests, and developer documentation, use
[Subyard dev-flow](.agents/skills/subyard-dev-flow/SKILL.md).

Optional-profile behavior and tests belong to the owning profile, not core. Core owns generic
extension contracts and synthetic contract tests; do not add profile-specific branches or assertions.
Moving tests must preserve release acceptance coverage of core and all shipped profiles.

Readiness checks return state and diagnostics; the operation boundary renders them.
Expected drift is not a warning. Follow the
[activation diagnostic contract](docs/control-plane.md#release-migrations).

Checks report every refusal they can determine independently, in stable order, not only the first.
Only a finding that depends on a failed read may be skipped.
Use `domain.CollectRefusals` for each check being changed; no separate audit is required.

Before suggesting CLI commands, check the [generated CLI reference](docs/cli-reference.md)
or the command's `--help`. Registry completion flags apply to whole command families,
not every subcommand. Regenerate help changes with `make cli-docs` and verify with
`make cli-docs-check`.

Do not rely on umask for exact file modes. Preserve permission validation; use
`testkit.WriteFile` for exact fixture modes and `testkit.TempDir` for private roots.

## Private overlay
An operator may keep private rules, task plans and notes in `private/`, a separate repository
ignored here. If `private/AGENTS.md` exists, read and follow it in addition to this file.
The operator runs [subyard-private-sync](.agents/skills/subyard-private-sync/SKILL.md)
manually to sync `private/`; agents must never invoke it on their own.
If the overlay is missing and no other instructions say where task plans and notes live,
ask the operator.

@private/AGENTS.md

## Testing

Use the [testing reading map](docs/testing.md#reading-map-by-task-type) to choose the
sections to read for the task.

Host-free tests and manual local reproductions must keep disposable Git repositories and
short-lived test worktrees under `/tmp`, outside managed workspace trees, even when `TMPDIR`
points into the checkout. Create test worktrees from fixture-owned repositories. Keep `.build`
for artifacts and logs; run catalog/load reproductions only on leased disposable VMs as
described in the testing guides.
