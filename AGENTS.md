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

Before suggesting CLI commands, check the [generated CLI reference](docs/cli-reference.md)
or the command's `--help`. Registry completion flags apply to whole command families,
not every subcommand. Regenerate help changes with `make cli-docs` and verify with
`make cli-docs-check`.

Do not rely on umask for exact file modes. Preserve permission validation; use
`testkit.WriteFile` for exact fixture modes and `testkit.TempDir` for private roots.

## Private overlay
If `private/AGENTS.md` exists locally (it is gitignored and lives in the separate private repo),
read and follow it **in addition** to this file. It carries private, non-public working rules

## Testing

Before building, running, or changing tests, read and follow the
[testing guide](docs/testing.md) and the [agent E2E VM guide](docs/test-vms.md).

Host-free tests and manual local reproductions must keep disposable Git repositories and
short-lived test worktrees under `/tmp`, outside managed workspace trees, even when `TMPDIR`
points into the checkout. Create test worktrees from fixture-owned repositories. Keep `.build`
for artifacts and logs; run catalog/load reproductions only on leased disposable VMs as
described in the testing guides.
