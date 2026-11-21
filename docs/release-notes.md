# Release notes

## Unreleased

### Defaults and approval prompts

- **`DEV_SUDO` now defaults to `prompt`.** v0.30.0 defaulted to no grant (`0`). A yard without a
  `DEV_SUDO` assignment now gets passwordless sudo for its developer account on its next
  `yard init`, and Claude, Codex and OpenCode ask before `sudo`, `su`, `doas`, `pkexec`,
  `sudoedit` and `run0`. In a running yard, `yard update` already reprovisions the agents with
  these prompts; the grant itself waits for `yard init`. `prompt` is a guardrail, not a boundary.
  To keep such a yard without sudo, run
  `yard -Y <yard> config set DEV_SUDO no --scope yard --yes` and then `yard init`. See
  [passwordless sudo inside the yard](configuration.md#passwordless-sudo-inside-the-yard).
- `DEV_SUDO` takes `prompt`, `yes` or `no`. `yes` gives passwordless sudo without prompts, as
  `1` did in v0.30.0; `no` gives no grant. The earlier values `1` and `0` keep working as
  deprecated aliases of `yes` and `no`, so yards that set them, including yards created from
  the Hermes and Amnezia presets, update unchanged; `yard check` and `yard config validate`
  name each one with its replacement. `DEV_SUDO` is now also accepted at shared scope. Run
  `yard init` after every change: release activation and `yard integration` update the agent
  prompts but not the sudoers grant. Older runtimes refuse `yes`, `no`, `prompt` and the shared
  scope, so switch from `1` and `0`, or publish a shared value, only after every owner host is
  updated.
- Before downgrading, remove every `DEV_SUDO` assignment, `1` and `0` included, and sync a
  Git-backed removal while this release still runs. A yard that applied `yes` or `no`, also
  through a kept `1` or `0`, must then run `yard init` without the assignment, because older
  runtimes refuse its installed record. After the downgrade, v0.30.0's next `yard init` removes
  the grant; set `DEV_SUDO=1` first where the yard should keep passwordless sudo.
- Claude, Codex, and OpenCode now default to running Git commits and pushes
  without a managed approval prompt. Existing yards keep their installed policy
  until the next `yard init`. Set `CODING_TOOL_GIT_APPROVAL=prompt` to keep or
  restore prompts. Restart agent sessions after init to load the new policy.
  See [commit and push approval prompts](configuration.md#commit-and-push-approval-prompts).
- With `CODING_TOOL_GIT_APPROVAL=prompt`, the Claude and Codex hook now refuses any literal
  mention of `git commit` or `git push` that the client's own prompt does not cover: anything
  other than a direct command, optionally after a plain prefix such as `cd <directory> &&`, or text
  in a plain read-only command such as `rg` or `echo`. Codex also refuses them in a command with
  a redirect, variable, `$(…)` or backtick substitution, newline or comment, which its prompt
  rules do not match. Some commands that only name them, such as
  `gh pr create --title 'git push fix'`, are refused as well; run the direct command or a plain
  form instead.
- New yards inherit the server's timezone when available in the guest and otherwise default to UTC.

### Configuration

- **Move settings out of a source checkout before updating.** Settings in a checkout's
  `private/config.env` and yard definitions in `private/yards/*.env` no longer apply. Move host
  settings into the host file, for example with `yard config set NAME VALUE --scope host`, or
  into the Git host layer, and yard definitions into the local or Git yard layer.
- User settings files at shared, host and yard scope, local and Git, accept literal values only.
  A `$NAME` or `${…}` reference, including `${NAME:=value}`, is a read error that names the file
  and line; replace it with the value. Single quotes keep dollar text literal. Shipped defaults
  are unchanged. See [scalar settings](configuration.md#scalar-settings).
- Each local scope has one settings file, and edits go to it. A flat `yards/NAME.env` now also
  applies to the `default` yard. When both `yards/NAME.env` and `yards/NAME/config.env` exist,
  loading refuses until you merge them into one file.
- `yard config set` no longer drops an unrelated setting that shared a record with the edited one,
  and values with apostrophes read back unchanged. CRLF inside a quoted multiline value is now
  preserved, so converting such a file's line endings can change its values.
- `yard config set SETTING VALUE [SETTING VALUE ...]` saves several settings in one atomic local
  write. It checks cross-setting constraints against the combined result, reports every refusal
  and writes nothing if any check fails.

### Installation check

- `yard check` is now a read-only installation check of configuration, release readiness, the
  host and state journals. It reports every finding and never repairs, starts yards, requests sudo
  or fetches releases. `--json` prints a versioned report; the exit code is 0 when clean, 1 for
  drift or blockers and 2 for usage errors. See [installation check](control-plane.md#installation-check).
- The previous host preflight is now `yard [-Y NAME] check --host`. `yard -Y NAME check` is a
  usage error, so update scripts that call it. `check --host` forwarded to an owner on an older
  release fails with an unknown option.

### Updates and rollback

- `yard update --rollback`, `yard update --version` to a retained or previous release, and
  offline standalone replacement refuse before any change when the target's migration registry
  differs from the active one, and name the next step. Registries that differ only by retired
  migration prefixes pass when the newer release declares the pair. This release has the same
  registry as v0.30.0.
- Re-running the standalone installer on an existing installation now hands the candidate to
  `yard update` as a private draft, so it can cross a migration registry change. A release whose
  engine cannot take the draft, such as v0.30.0, is refused before any change; install it with
  that release's own `subyard-install.sh`, for example `bash subyard-install.sh --version 0.30.0`.
- When an activation step fails after it has partly applied, the update now reports a retryable
  `recovering/dependency-unavailable` instead of `operator-action-required/recovery-ambiguous`.
  Retry `yard update`.
- Installations older than 0.28.0 still update in two steps,
  `yard update --version 0.28.1 && yard update`, and the refusal prints that chain.
<!-- Rollback hooks: the rollback-hooks change replaces the next entry. -->
- **Rollback to v0.30.0.** While the telemetry profile is installed, also disabled with its
  collector unit still present, v0.30.0 refuses `yard update --rollback` itself with
  `dependency-unavailable` for `activation.telemetry-runtime`, and this release stays active.
  Export the telemetry store first: the older collector cannot open a store above a third of its
  1 GiB bound; see [telemetry rollback steps](telemetry.md#capture-policy-and-delivery). After a
  rollback, v0.30.0 reads this release's profile metadata strictly and refuses its own
  `yard update --rollback` back to it; return with `yard update --version` and this release's
  version. Rollback to 0.29.0 or earlier stays refused as `rollback-incompatible`.

### Telemetry profile

- The first update from v0.30.0 archives the existing database read-only as
  `archive/telemetry.sqlite` (or the next free `archive-N`) and starts an empty store. Export,
  `show-run`, `stats` and `query` keep working on archives. Runs in flight are marked
  `interrupted_by_upgrade`, and pending v0.30 outbox records are not imported.
- Exporters are now configured through root-owned system policy files,
  `/etc/claude-code/managed-settings.d/60-subyard-telemetry.json` and
  `/etc/codex/managed_config.toml`, not home documents. The update restores the earlier home
  values and removes the collector token from them. The policies no longer contain the token: a
  root-owned helper supplies the `Authorization` header. Codex needs 0.170.0 or later; an older
  Codex exports without it and the receiver rejects the data, so check `codex --version`. Restart
  running agents to load the new policy.
- Stored data keeps session, run and trace IDs, host, yard, project and task labels, model and tool
  names, token counts, cost and durations. Prompts, tool inputs and outputs, response text, error
  messages, headers and operator emails and account IDs are dropped. Raw IDs make exports
  linkable, so keep them private.
- Usage is attributed per model request, including Claude subagents, Codex child threads and
  cross-vendor workers. New views `model_requests`, `tool_outputs` and `model_turn_triggers`
  answer per-task usage queries. See [usage and attribution](telemetry.md#usage-and-attribution).
- The fixed 1 GiB store bound is replaced by the soft `TELEMETRY_DISK_BUDGET` (default `30GiB`).
  When space runs short only raw OTLP is refused; usage and lifecycle records keep flowing, and
  `yard status` shows a `storage-capacity` warning. Nothing is pruned automatically; remove old
  data with the collector's `prune --older-than`.
- Forward updates from v0.30.0 with telemetry enabled no longer refuse the collector units as
  foreign, and managed runs shorter than about a second keep their telemetry.

### Codex sandbox on AppArmor hosts

- On hosts that restrict unprivileged user namespaces through AppArmor, such as Ubuntu 24.04,
  `yard init` now loads the distribution's `bwrap` and `unpriv_bwrap` profiles into the yard's own
  AppArmor namespace, so restricted Codex sandboxes work. The host policy and sysctl stay
  unchanged, and the approval plan shows the change. This happens only when Codex or Claude Code
  is selected; `yard init` no longer fails in this step for other yards. To opt out, remove
  `codex` from `CODING_TOOL_INTEGRATIONS`, set `BWRAP_APPARMOR=off` and run `yard init`. A
  release update that cannot verify this policy names the yard and points to
  `yard -Y NAME integration status`. See [where Codex runs](configuration.md#where-codex-runs).

### Test VMs

- An update recreates a running, registered test-vms broker whose engine changed. Open SSH
  sessions through it end, and its VMs, leases and base images are rebuilt; its settings and pool
  are kept. Agents holding a lease acquire a new one. A stopped broker is not changed, and
  rollback updates the broker in place.
- Leases accept `--image debian-13|ubuntu-24.04|ubuntu-26.04`; Debian 13 stays the default.
  `yard test-vms refresh TYPE --image IMAGE` refreshes one base. An explicit image needs a
  broker from this release. See [the agent E2E VM pool](test-vms.md#agent-e2e-vm-pool).
