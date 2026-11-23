# Release notes

## Unreleased

### Agent approval policies

- Git and sudo approval policies are provisioned independently for Claude, Codex and OpenCode.
  Existing combined policies migrate on reprovision; the configured approval behavior stays the same.

### Rollback to 0.31.0

- **Agent cleanup is manual and does not refuse rollback.** Stop agent sessions before rollback.
  The plan output and result list the surviving files from the
  [sealed cleanup declaration](../config/release-rollback.json):
  `/etc/claude-code/managed-settings.d/48-subyard-update-lock.json`,
  `/etc/claude-code/managed-settings.d/49-subyard-approval-hook.json`,
  `/etc/claude-code/managed-settings.d/51-subyard-sudo-approval.json`,
  `/etc/codex/49-subyard-approval-hook.toml`, `/etc/codex/50-subyard-git-approval.rules.inc`, and
  `/etc/codex/51-subyard-sudo-approval.rules.inc`. After rollback, back up and remove only these
  Subyard-managed files in each affected yard, then run `yard init` before restarting agents.
  The surviving Claude hook and sudo rules otherwise remain active; Codex's fragments are inert.
  Preserve operator-owned policies and account for custom installation paths. See
  [agent permission modes](configuration.md#other-agent-permission-modes).

### NetworkManager guard

- Fix yard startup after host reboot on NetworkManager hosts using a decimal-comma locale
  (such as German or Russian). In 0.31.0, the guard record could fail to match the unchanged
  guard file, leaving yards stopped. Updates recognize these existing records and rewrite
  them with a locale-independent identity, without manual repair or an extra reboot.

### Rollback to v0.30.0

- Complete the 0.31.0 cleanup above as well as the following steps.
- **Agent cleanup is manual and does not refuse rollback.** Stop agent sessions before rolling
  back. The rollback plan output and result list the surviving files from this release's
  [sealed cleanup declaration](../config/release-rollback.json):
  `/etc/claude-code/managed-settings.d/50-subyard-git-approval.json`,
  `/etc/claude-code/subyard-git-approval-hook`, `/etc/codex/requirements.toml`,
  `/etc/codex/subyard-git-approval-hook`, `/usr/local/bin/claude-policy-check`,
  `/usr/local/bin/codex-policy-check`, `~/.codex/config.toml`, and
  `~/.config/opencode/opencode.jsonc`, and
  `/usr/local/libexec/subyard/projects-changed` (`~` is the yard developer's home).
  After rollback, back up these files in each affected yard, remove only Subyard-managed
  approval hook and policy files, restore the target release's agent configuration and project dispatcher, and run
  `yard init` before restarting agents. Preserve operator-owned content and account for custom
  installation paths. There are no agent integration rollback hooks or automatic cleanup.

## 0.31.0

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

- `yard update --rollback` to v0.30.0 works with the telemetry profile installed. It refuses
  while telemetry is enabled in a local yard, because v0.30.0 cannot open this release's
  telemetry store; run `yard -Y NAME profile disable telemetry` first. The first rollback
  command then converts the telemetry runtime to the v0.30.0 format, and the second one rolls
  back. No telemetry data is moved or converted, and telemetry stays disabled on v0.30.0. See
  [telemetry](telemetry.md#enable-and-migrate).
- After a rollback, v0.30.0 refuses its own `--rollback` back to this release, because it rejects
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
