# Release notes

## Unreleased

### Yard disk use

- Yards no longer keep downloaded APT packages. Provisioning writes
  `/etc/apt/apt.conf.d/99subyard-no-archive-cache`, so `apt-get` removes downloaded archives
  after a successful install, and clears the archives that earlier provisioning kept.
  Rollback leaves the file in place; delete it in the yard to keep archives again.

### Test VM memory

- On x86_64, test-vms broker VMs enable Free Page Reporting on their virtio balloon, so RAM
  a guest frees returns to the host instead of staying resident in QEMU. A VM that is
  running during the update gets the setting when it next starts.
- The default per-VM admission overhead `E2E_VM_OVERHEAD` is 256 MiB instead of 512 MiB; a
  broker without its own value admits a 4 GiB pair with 12.5 GiB available instead of 13 GiB.
  Measured QEMU memory outside guest RAM was 60–120 MiB per VM.

### Telemetry disk budget

- **Above `TELEMETRY_DISK_BUDGET` (default `30GiB`) the collector now prunes automatically.**
  It removes the oldest raw OTLP, session correlations and samples first, never ones younger than
  30 days; usage, lifecycle and resource records stay. Below the budget nothing is pruned.
- **Raw OTLP is no longer refused near the budget.** Only a data partition short of its reserve
  still refuses it. A store held above the budget by the last 30 days keeps growing, and
  `stats`, `yard status` and `yard profile status telemetry` show the `storage-capacity` warning.
  See [usage and attribution](telemetry.md#usage-and-attribution).

### Init on a busy yard

- **`yard init` no longer fails with "operation plan is stale: project hook inputs changed" while
  agents work.** Creating or removing worktrees, build outputs or unregistered directories under
  `/srv/workspaces` during the prompt or provisioning no longer invalidates the plan. For a running
  yard, init still refuses when, after confirmation, a registered project root is removed or
  replaced, its `.subyard-meta.json` changes, or an installed project hook is added or changed.
- **Init of a stopped yard, and init that continues after a developer identity change, no longer
  refuses while `/srv/workspaces` holds directories that are not registered projects.** A missing
  registered project root still refuses. A stopped yard binds its project roots and metadata from
  its first observation after the start.
- When workspace entries change at the moment the hooks start, init observes again and tries
  up to three times in all. If they keep changing, init names that cause instead of reporting a
  failed agent hook; retry when the activity settles.

### Coding tool default

- **The shipped default no longer selects OpenCode.** Yards without an explicit
  `CODING_TOOL_INTEGRATIONS` selection now inherit `claude codex pi`. OpenCode remains available:
  select it explicitly, for example with `yard -Y NAME integration enable opencode`.
- **Before you update, select OpenCode explicitly to keep it.** Otherwise the update retires it in
  running yards that inherit the default, and the next `yard init` retires it in stopped ones.
  The plan lists each step. As with `yard integration disable opencode`, the OpenCode binary,
  credentials and the session database in the host agent-sessions store are preserved, while
  Subyard's OpenCode configuration fields, managed instructions and session-database link are
  removed. To keep the previous set:
  `yard config set CODING_TOOL_INTEGRATIONS 'claude codex opencode pi' --scope host --local`.
- After a rollback to 0.32.0, inheriting yards select OpenCode again; run `yard init` in any yard
  where it is not provisioned yet.

### Claude managed instructions

- Claude provisioning installs `/etc/claude-code/CLAUDE.md`, which tells Claude to delete only by
  literal absolute path; `claude-policy-check` reports its absence and drift. Disabling Claude keeps it.
- **Rollback to 0.32.0 keeps the file.** It is listed in the rollback plan output and result.
  After rollback, back it up and remove it in each affected yard if you do not want the rule.

### Full uninstall recovery

- **A full uninstall interrupted on 0.30.0 or older is no longer resumed by this release.**
  `yard _uninstall-resume` and `yard teardown --full` refuse its saved plan without changes.
  Resume it with the launcher retained in the recovery directory:
  `<data-root>.uninstall/runtime/bin/yard _uninstall-resume <data-root>.uninstall`. If that
  launcher is gone or fails, install 0.32.0, the last release that resumes such a plan, and run
  `yard _uninstall-resume <data-root>.uninstall`. The refusal prints the exact command.

### Agent instructions from a dotfiles repository

- **Host instruction files are no longer copied into yards.** `HOST_CLAUDE_MD`,
  `HOST_CODEX_AGENTS_MD` and `HOST_OPENCODE_AGENTS_MD` are retired and ignored; `yard check`
  names each line to remove. The next
  init removes an unchanged copy that an earlier release made; an edited copy is kept and its
  path is refused until you move it away.
- The new [`user-dotfiles`](user-dotfiles.md) profile follows the dotfiles convention of GitHub
  Codespaces and VS Code Dev Containers: it clones the yard's own minimal dotfiles repository,
  separate from host dotfiles (`USER_DOTFILES_SOURCE`), and right after the first clone runs its
  install script once (`USER_DOTFILES_INSTALL_COMMAND`, or the first of `install.sh`, `install`,
  `bootstrap.sh`, `bootstrap`, `script/bootstrap`, `setup.sh`, `setup` and `script/setup`;
  without one it links the repository's top-level dot entries into `~`). Disabling the profile
  runs the repository's `uninstall.sh` when it has one and keeps the clone. Like
  [user skills](user-skills.md), it is selected by default in the yard named `default` while its
  `ENVIRONMENT_PROFILES` is unset, so the next init there installs the shipped source
  `https://github.com/subyard/yard-dotfiles`, which links its `agents/AGENTS.md` to
  `~/.claude/CLAUDE.md`, `~/.codex/AGENTS.md` and `~/.config/opencode/AGENTS.md`. Set
  `USER_DOTFILES_SOURCE` to use another repository (a private one needs Git access in the
  yard), or run `yard -Y default profile disable user-dotfiles` to opt out. Subyard never
  updates the clone: after `git pull` in it, run its install script again yourself. For
  [user skills](user-skills.md), no `yard init` is needed after a pull.
- **Before rolling back to 0.32.0 or older,** run `yard -Y NAME profile disable user-dotfiles`
  in each yard that uses the profile, move any file or link left at an instruction path away,
  and unset `USER_DOTFILES_SOURCE` and `USER_DOTFILES_INSTALL_COMMAND` wherever you set them
  (`yard -Y NAME config unset SETTING --scope SCOPE`, with `--git` for a setting saved with
  `--git`). For 0.31.0 or older, also unset `USER_SKILL_SOURCES`, which those releases do not
  know. A rollback to 0.31.0 or 0.32.0 that misses a step is refused with "the retained release
  cannot provide a safe rollback plan" and this recovery, and one to 0.30.0 with a setting left
  with "cannot read the yard settings"; the rollback plan lists these steps. After updating
  again, set the unset settings again before re-enabling the profile, or run
  `yard -Y NAME config unset ENVIRONMENT_PROFILES --scope yard` to return to the default
  selection; see [user dotfiles](user-dotfiles.md).

## 0.32.0

### Before you update

- **Dead private-overlay references are removed automatically.** Update removes a host
  `AGENT_*` file assignment only when it uses the exact
  `$SUBYARD_CONFIG_DIR/../private/agents/...` reference (or its braced form), names a
  declared artifact, points to a confirmed missing path, and has a protected host file
  override. `yard update --check` and `yard config validate` identify the line to remove
  and the file-settings source that takes precedence. Update preserves the settings
  file's owner and mode. Other `$NAME` and `${…}` references still require literal values;
  verify their intended source before replacing them.
- **Legacy telemetry cleanup accepts materializer ownership.** Home agent files owned
  by the developer's `uid:uid` need no group change before updating. The developer's
  primary group remains supported; a foreign owner is reported with the expected owner
  and a repair command, alongside specific unsafe-file or parsing failures.

### Update and boot recovery

- Service recreation checks the replacement's host network safety before opening a release
  transition or deleting the old runtime. It records an unbound NetworkManager guard first;
  when a reboot is required, reboot once, then rerun `yard update`. Recreation retains the shared
  bridge, storage pool, guard binding and power reconciler so reinstalling the last yard does not
  require another reboot. The power stage updates the retained unit; recreation does not try to
  remove an older release’s unit using the new release’s template.
- `yard init` on a stopped yard accepts newly visible project-hook counts after its temporary
  identity inspection start; a new resource or operation step still refuses as a stale plan.
- Boot power reconciliation retries failed attempts every 30 seconds, including storage that
  becomes available only at login, without relying on Incus error text. NetworkManager guard
  failures still prevent starts. The journal and `yard status` name the required init or reboot;
  desired-running yards start automatically after the cause is resolved.

### Agent approval policies

- Git and sudo approval policies are provisioned independently for Claude, Codex and OpenCode.
  Existing combined policies migrate on reprovision; the configured approval behavior stays the same.

### User skills

- GitHub skill sources clone in yards with the GitHub profile. Its Git wiring sends github.com
  URLs through the `subyard-github` broker transport, which the user-skills clone did not allow,
  so these sources were skipped with a clone warning. Run `yard init` or
  `yard profile enable user-skills` after updating to clone them.

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
  guard file, leaving yards stopped. Updates recognize these existing records without manual
  repair or an extra reboot. Update and boot reconciliation keep valid legacy records bound
  without rewriting them. Init and guard publication canonicalize their identity while
  preserving their boot and read state; a failed canonicalization keeps the existing binding.

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
  `${NAME:=value}`; replace live references with their intended literal value. Later updates
  remove proven dead private-overlay file references automatically, as described above. See
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
