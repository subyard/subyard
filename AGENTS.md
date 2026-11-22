# Subyard — agent instructions

This is the **public** repository. Keep everything here generic and in English; no private
data. Architecture, accepted decisions and release-transition design live in `docs/reference/`.
Operator-specific requirements and planning may live in the optional private overlay.

## Development workflow

For features, fixes, refactoring, tests, and developer documentation, develop with the
[dev-flow](https://github.com/Dmitry-Borodin/dev-flow) skill (process) and
[Subyard dev-flow](.agents/skills/subyard-dev-flow/SKILL.md) (Subyard parameters, checks, tools).
The `user-skills` profile installs dev-flow; if it is not loaded, stop and ask the operator.

Optional-profile behavior and tests belong to the owning profile, not core. Core owns generic
extension contracts and synthetic contract tests; do not add profile-specific branches or assertions.
Moving tests must preserve release acceptance coverage of core and all shipped profiles.

Readiness checks return state and diagnostics; the operation boundary renders them.
Expected drift is not a warning. Follow the
[activation diagnostic contract](docs/control-plane.md#release-migrations).

A check whose result is shown to the operator — a command preflight, `--check`, `validate`,
`status`, readiness, a release gate verdict or a migration refusal — reports every refusal it can
determine independently, in stable order, through `domain.CollectRefusals`. It may skip only a
finding that needs data from a read that failed. Helpers below that boundary (parsers,
single-value validators, file and guest reads, apply steps after the first mutation) may fail
fast; the boundary check calls independent helpers separately and collects their results.
Review applies this rule to the boundary check being changed, not to helpers it calls.

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
