# Release notes

## Unreleased

### Before you update

- **Settings overlay.** A checkout's `private/config.env` and `private/yards/*.env` no longer
  apply. Move each file whole only when its destination is absent: `private/config.env` to
  `~/.config/subyard/config.env`, `private/yards/NAME.env` to `~/.config/subyard/yards/NAME.env`
  when neither that file nor `yards/NAME/config.env` exists. If the destination exists, the
  overlay was not in effect: delete it or merge it by hand. Do not move it key by key with
  `yard config set`.
- **Literal settings only.** User settings files refuse `$NAME` and `${…}` references, including
  `${NAME:=value}`; replace each with its value. See
  [scalar settings](configuration.md#scalar-settings).

### Default changes

- **`DEV_SUDO` defaults to `prompt`** (v0.30.0: no grant). A yard without an assignment gets
  passwordless sudo on its next `yard init`, and Claude, Codex and OpenCode ask before sudo. To
  keep a yard without sudo, set `DEV_SUDO=no` and run `yard init`. `1` and `0` still work as
  aliases of `yes` and `no`; switch to the new values only after every owner host is updated.
  See [passwordless sudo inside the yard](configuration.md#passwordless-sudo-inside-the-yard).
- **Downgrading with `DEV_SUDO`.** Remove every `DEV_SUDO` assignment first; a yard that applied
  `yes` or `no` also runs `yard init` without it, which publishes the grant. Run `yard init` for
  every yard right after the downgrade to remove it; to keep sudo, set `DEV_SUDO=1` after the
  downgrade and before that init.
- **Git commit and push approval prompts are off by default** from the next `yard init`. Set
  `CODING_TOOL_GIT_APPROVAL=prompt` to keep them. See
  [commit and push approval prompts](configuration.md#commit-and-push-approval-prompts).
- New yards inherit the server's timezone, otherwise UTC.

### Telemetry profile

- **Codex 0.170.0 or later is required**; an older Codex's exports are rejected. Check with
  `codex --version`.
- **The store is archived and reset.** The first update from v0.30.0 keeps the old database
  read-only as `archive/telemetry.sqlite` and starts an empty store. The seven-day retention is
  gone: the store grows up to `TELEMETRY_DISK_BUDGET` (default `30GiB`) unless you remove old
  data with the collector's `prune --older-than`. See
  [usage and attribution](telemetry.md#usage-and-attribution).

### Rollback to v0.30.0

<!-- Rollback hooks: the rollback-hooks change replaces the next entry. -->
- While the telemetry profile is installed, v0.30.0 refuses `yard update --rollback`. Export the
  telemetry store first; see [telemetry rollback steps](telemetry.md#capture-policy-and-delivery).
  After a rollback, v0.30.0 refuses its own `--rollback` back to this release, because it rejects
  `advisories` in the telemetry `profile.json` and `approval_sources` in
  `config/agents/opencode/integration.json`; return with `yard update --version` and this
  release's version. Rollback to 0.29.0 or earlier stays refused.

### New

- `yard check` is a read-only installation check; the old host preflight is
  `yard [-Y NAME] check --host`. See [installation check](control-plane.md#installation-check).
- The `subyard-agent-bridge` skill lets Claude and Codex run a worker of the other vendor.
- On hosts that restrict user namespaces through AppArmor, such as Ubuntu 24.04, `yard init` loads
  `bwrap` profiles into the yard so Codex sandboxes work. See
  [where Codex runs](configuration.md#where-codex-runs).
- `test-vms` leases accept `--image debian-13|ubuntu-24.04|ubuntu-26.04`; Debian 13 stays the
  default. See [the agent E2E VM pool](test-vms.md#agent-e2e-vm-pool).
