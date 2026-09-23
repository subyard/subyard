# Subyard configuration

Subyard combines immutable settings shipped with its runtime, persistent local settings, and
temporary command overrides into one effective configuration. Use the CLI to inspect that result;
the files remain the source of truth.

```sh
yard config fields
yard config fields SSH_PORT
yard config show
yard config show SSH_PORT
yard -Y demo config show
yard config paths
```

`config fields` is the public typed field reference. It reports the shipped default, kind, type,
allowed scopes, syncability, merge mode, application mode and domain owner from the same catalog
used by the production resolver. `config show` lists effective non-secret settings, their winning
scope and source, and how they are consumed. Passing one setting name shows every applicable layer
as `effective`, `overridden`, or `unset`. Unknown fields, wrong scopes and invalid values fail
closed; secret inputs and unrelated environment variables are not settings.

## Storage roles

The default configuration root is `~/.config/subyard`. It contains several roles, not one monolithic
configuration:

| Path | Role |
|---|---|
| `overrides/shared/config.env` | Explicitly shareable scalar settings |
| `config.env` | Host-wide scalar settings |
| `overrides/shared/` | Explicitly shareable non-secret file settings |
| `overrides/host/` | File settings specific to this owner host |
| `yards/<name>/config.env` | Yard definition and scalar settings, including `default` |
| `yards/<name>/overrides/` | File settings specific to one yard |
| `secrets/` | Secret inputs, not settings |
| `generated/` | Materialized consumers, not settings |
| `keys/` | Encrypted credential ledger and its state |
| `projects/` | Runtime project state |
| `tools/` | Subyard-managed support tools |

Run `yard config paths` to resolve these roles for the current installation and selected yard.
Immutable shipped defaults stay in the installed runtime and do not belong in the configuration root.

Managed configuration paths must be real (not symbolic links), operator-owned files and directories.
Subyard rejects any managed path that is writable by its group or by other users. The operator may
choose the group and other read bits for existing files; Subyard does not treat those read bits as a
confidentiality policy. New sensitive files are created with mode `0600` by default, without
silently changing the mode of existing files during update or apply.

## Scalar settings

Common portable values may go in `overrides/shared/config.env` only when `config fields` lists the
`shared` scope. Host-wide values go in `config.env`:

```sh
DEV_SUDO=1
```

A named yard has its own settings:

```sh
# ~/.config/subyard/yards/demo/config.env
SSH_PORT=2223
YARD_TEMPLATE=test-vms
```

Scalar precedence is:

```text
shipped defaults
  -> explicitly shareable scalar settings
  -> host-wide scalar settings
  -> named-yard derivations and selected shipped profile
  -> named-yard scalar settings
  -> current command environment
```

The default yard also accepts scalar overrides in `yards/default/config.env`, after host-wide
settings. Its optional override file does not register another yard or apply to named yards.
`yard config set NAME VALUE --scope yard` selects this layer when `-Y` is omitted.
A command environment value is temporary and has the
highest precedence. It is never persisted by config sync. `yard [-Y <yard>] config show <SETTING>`
is the authoritative explanation of the actual chain, including derived values. Start from
[`config/settings.env.example`](../config/settings.env.example).

## File settings

Known file settings, such as coding-agent configuration and rules, start with a shipped file and may
be replaced by the matching file under `overrides/shared`, `overrides/host`, or a yard's
`overrides` directory (including `yards/default/overrides`). Their precedence is shipped, shared, host, yard, then a command override.

These directories currently override known file settings only. They are not generic scalar
configuration directories.

For a coding-agent `CONFIG` whose destination ends in `.json` or `.toml`, Subyard owns the fields in the
selected template and preserves fields added by the running tool. This includes Orca's Claude
`hooks` and `statusLine`, and Codex's `hooks`, `projects` and `tui`, when the template does not define
them. The destination selects this policy;
importing a template from a renamed source does not change it. `RULES`, instruction files, JSONC
and other formats keep exact byte replacement and comparison.

JSON objects merge recursively. Arrays, scalars and `null` replace the whole value at their path;
an empty object requires an object but preserves added children. A later template removes fields
owned by the previous template, preserving unrelated siblings. Changing an object to a scalar or
array replaces that entire field. Removing a template's field before its first recorded application
cannot remove an older default automatically: first adoption preserves fields absent from the
current template.

TOML tables follow the same field ownership rules, including nested tables; arrays are owned as
whole values. Runtime additions and formatting changes do not cause drift, but changes to managed
values or TOML scalar types do. Applying an already matching TOML document preserves its text;
when managed values need changing, serialization normalizes formatting and removes comments while
preserving unmanaged values. TOML parsing uses Python 3.11 or newer in the yard, and the writer is
embedded in the release, so existing yards need no extra Python package for observation or repair.

`yard init --configs`, `yard config apply`, provisioning checks and release activation share this
ownership contract. Read-only checks compare managed fields and their ownership baseline,
so runtime additions do not trigger a release migration. The first application records a root-owned
baseline under `/var/lib/subyard/config-materialization` inside the yard. It contains only asset
identity, schema version, template digest, owned paths and a digest of the owned values, never
configuration values. A missing or
outdated baseline requires application even if the managed values already match.

Subyard serializes its own writes per asset and atomically replaces the destination before updating
the baseline. Retrying an interrupted application converges. Invalid JSON or an invalid baseline
fails without overwriting it; diagnostics omit configuration contents. Running tools do not share
Subyard's lock: application reads the current file and verifies its result, but cannot serialize
arbitrary concurrent third-party writes. Invalid TOML, like invalid JSON, fails without overwriting
the current document or printing its contents.

## Per-yard coding tool selection

```sh
yard integration status --json
yard -Y demo integration enable codex
yard -Y demo integration disable codex
yard -Y demo integration status codex
```

`CODING_TOOL_INTEGRATIONS` stores the complete requested set in the selected yard's
`yards/<name>/config.env`. An empty assignment explicitly selects no integrations;
an absent assignment remains distinguishable from empty. The compatibility input
`AGENTS=none` means empty. Unknown IDs, duplicate IDs and dependency cycles are errors.
Dependencies are computed separately: enabling Paseo also makes Codex effective;
disabling Codex while Paseo still requests it is rejected. Status reports the requested
set, effective set, dependency reasons, configuration source and observed yard readiness.
An ID filters membership and dependency details; readiness describes the whole yard.

The first owner-side initialization materializes the selection. A fresh default yard
gets `claude codex opencode pi aiobserver`; a fresh named yard gets an explicit empty
set. Existing yards retain their trustworthy requested configuration through bounded
adoption of that selected yard. Subyard does not infer intent from installed binaries
or change other yards during adoption. If previous intent cannot be established, set
`CODING_TOOL_INTEGRATIONS` explicitly with `yard config set --scope yard` before init.
The default yard uses the same scalar and file configuration layout as named yards.

Enable and disable require an existing, running yard with its core substrate ready.
A stopped or missing yard fails before confirmation or configuration changes. The
command never starts the yard. A running-state and exact-plan recheck also prevents
applying a plan after the yard has stopped. Remote changes are planned and executed
on the authoritative owner over one RPC session; the controller confirms the owner's
plan once. An older owner without the exact-plan capability must be upgraded first.

A confirmed operation saves desired configuration with a compare-and-swap guard,
then reconciles only the integration packages, configuration, links, hooks and owned
services. Full init uses the same integration reconciler. Failed application keeps
the requested set and returns an error; repeating the command repairs pending state.
An unchanged selection is a no-op only when the runtime is also converged. A temporary
selection override that conflicts with persistent configuration is rejected.

Disable stops only proven-owned services and removes only unchanged owned wiring.
Credentials, session history, session storage targets, explicit operator links,
unmanaged binaries and user CLI sessions are preserved. JSON/TOML retirement removes
owned fields while retaining the document and unrelated fields. Materialization baselines
keep the v0.14.0 format for retained-runtime rollback; extra retirement evidence uses a
protected companion record under the same lock. The root-owned
inventory under `/var/lib/subyard/integrations` records artifact identity and digests;
it is ownership evidence, not another desired-state store. Missing, legacy, corrupt
or changed ownership evidence can block cleanup with a conflict instead of deleting
unproven artifacts. After inventory initialization, newly created unrecorded runtime
files remain unmanaged and are preserved; selecting an integration does not authorize
replacing an occupied unowned file or link. Fix reported conflicts before retrying.
The shared project-hook dispatcher and hook list can acquire inventory evidence when
their bytes exactly match the desired core files, or the dispatcher matches its known
published predecessor, and their ownership, modes and parent directories are protected.
Ordinary legacy yards can establish their first inventory through a confirmed
`init` or release update when the persistent selection is unchanged. The plan
lists the paths it will manage. Existing plain files and links must match the
selected templates, targets and metadata exactly; existing JSON/TOML documents
also need a protected matching materialization baseline. Unrelated fields and
session targets remain preserved. Unknown or changed state fails before adoption.
Release activation reconciles running ordinary yards before refreshing their configs,
including unfinished inventory application. Stopped yards remain unchanged.
Integration enable/disable does not perform this initial adoption. Interrupted
release updates resume within the same approved desired artifact scope.

An integration may declare an `AGENT_<name>_CLEANUP` hook for explicit recovery of
installed state that is outside the normal ownership inventory. The hook is coding-integration
profile metadata, distinct from environment-profile resources. It remains discoverable when the
integration is not selected, so `yard integration cleanup <name>` can inspect or clean up a
disabled integration without changing the requested set. Cleanup sources use the same trusted
regular, non-symbolic-link file contract and scopes as provisioning hooks.

To inspect recovery, run `yard -Y <yard> integration cleanup <name> --check`. Run the same command
without `--check` to review and confirm its consequences. Cleanup refuses integrations that are
still selected, including dependencies of selected integrations. Hooks must keep cleanup bounded
and reversible, preserve user data, and support safe retries after interruption.

When an update is blocked before activation, its diagnostic names the downloaded candidate's
`bin/yard-engine` directly. Use that printed command on the owner host: the active older `yard`
may not support cleanup yet. After cleanup, repeat the update.

Cleanup uses a read-only observation followed by the shared typed confirmation and an exact-plan
recheck. Subyard sends the hook to the running yard and invokes it as
`sh -eu -s -- observe|apply DEV_USER DEV_UID EXPECTED_FINGERPRINT`. Observation writes exactly one
JSON object with a lowercase SHA-256 `fingerprint`, a `changed` boolean and public-safe `steps`.
A meaningful failure may instead write an object containing a safe-name `code` and bounded,
public-safe `message`. Apply receives the observed fingerprint, must recheck it before mutation,
and may return the same observation shape; Subyard observes again afterward to verify convergence. Hooks must not include
credentials, configuration contents or other secrets in fingerprints, steps, errors or output.

When a configuration source is registered, enable/disable rejects local selection
writes. Edit the selected yard's full requested set in that source, run `yard config
sync`, then `yard init` to reconcile. `config sync --apply` refreshes file consumers and
does not replace the integration lifecycle. The ordinary `config set` followed by
explicit `config sync push` workflow remains available.

The shipped `test-vms` role has `ALLOWS_CODING_TOOLS=false`. Its inherited tools are
suppressed, explicit nonempty selections fail, and agent-only utilities such as
`ccusage` are excluded. Cleanup of an existing running test yard uses the same ownership
checks and preserves broker keys, inner VMs, profile resources and test data. The role
does not restrict arbitrary software installed or launched by the operator. GitHub
remains an optional environment profile; Orca remains a profile resource. `ccusage`
is a core utility in ordinary yards, not a selectable integration.

## Coding-agent compatibility

Codex, Claude Code, OpenCode and pi can run directly in the yard. Their versions are not a
Subyard compatibility allowlist. When the Codex provisioning hook runs, it resolves the current
stable official release and verifies the downloaded artifact against that release's SHA-256
metadata before replacing the managed binary. A newer working installation is preserved.
Legacy `CODEX_VERSION` and `CODEX_SHA256_*` assignments are ignored when loading existing
settings; new writes to these retired fields are rejected. Already-converged yards do not check for upstream updates
on every `yard init`, and `yard status` does not install or update agents.

### Codex permissions across projects

The default yard configuration permits local edits, builds, tests and network access without
approval. Canonical direct `git commit` (including `--amend`) and `git push` require user approval.
The same policy applies to every project in the yard:

| Client | Yard mode |
| --- | --- |
| Terminal | Run `codex` without permission overrides. |
| VS Code Codex extension | Select **Custom (config.toml)**. |
| Orca | Use **Manual** with empty Codex arguments; Subyard also removes the stock YOLO argument. |

Codex provisioning installs root-owned `/etc/codex/requirements.toml` using Codex's native
[managed requirements](https://learn.chatgpt.com/docs/enterprise/managed-configuration).
It requires `on-request`, the `user` reviewer and commit/push prompt rules independently of the
home rules. Project settings, profiles, alternate `CODEX_HOME` and client flags cannot remove
these requirements. Incompatible overrides such as `never` or automatic review cannot take effect.
The home config supplies the existing `danger-full-access` yard sandbox default; stricter sandbox
choices remain available. Use **Custom** in clients that offer permission presets: unrestricted
filesystem access and mandatory command approval are separate settings.

After updating Subyard, run `yard init` to reconcile existing yards, then start new Codex sessions.
Existing sessions must be restarted to load the policy. `codex-policy-check` checks the installed policy's
contents, ownership and permissions; `yard init` repairs managed drift. Provisioning refuses to
overwrite an existing requirements file owned by another policy.

This is a Codex command policy, not an OS boundary against another executable, alternative Git
spellings or direct writes to Git metadata. Yard root can change the requirements. Enterprise-managed
requirements can also have higher precedence; their effective policy needs separate acceptance.
The native terminal UI offers single-use approval for these managed rules. A separate app-server
client can still submit a session-wide approval response; managed requirements do not disable that
protocol response. Single-use behavior in VS Code or other app-server clients needs its own
interactive acceptance.

`yard status` runs the installed policy check and one offline Codex rule check in a running yard:

| State | Evidence |
| --- | --- |
| `rules-ok` | The installed policy passed readiness; Codex's native home-rules matcher returned `prompt` for the checked commit/push forms and left the checked local commands ungated. |
| `incompatible` | Installed policy drifted, the native command failed, or the home rules did not meet the contract. Run `yard init` or review the rules. |
| `unverified` | Claude, OpenCode and pi have no supported offline approval check in Subyard. |
| `missing` / `?` | The CLI was not found, or the check was unavailable, timed out, or not run because the yard is stopped. |

The probe runs as the developer user with the default home CLI directories followed by system
directories on `PATH`. It uses the CLI capability directly, without interpreting its version
number. Standard guest utilities bound execution time and captured output. Raw CLI output and
configuration never appear in status; the guest returns only a result code. No model APIs,
agent sessions or updates are started by the check.

Codex uses its offline [`execpolicy check`](https://learn.chatgpt.com/docs/agent-configuration/rules)
for `git commit`, `git commit -m msg`, `git commit --amend`, `git push`, `git push origin main`
and `git push --force-with-lease`. It also checks that `git status` and `sh dev-check.sh` are
not gated by a rule. These commands are arguments to the matcher; Git and scripts are never
executed. All default-home `*.rules` files participate.

`rules-ok` proves installed policy readiness and the home matcher result. Subyard does not inspect
active sessions or prove that a client displays approval requests. Other Git spellings such as
`git -C` and `git -c` are outside this check; matching raw `bash -lc` input also does not model
Codex's runtime shell decomposition. Single-use approval and denial require runtime acceptance.

For Claude, OpenCode and pi, status states the missing evidence without parsing their configs
or launching diagnostics. In particular, Claude's shipped `bypassPermissions` mode skips prompts
according to its [CLI documentation](https://code.claude.com/docs/en/permissions#permission-modes);
ask entries alone do not establish enforcement. These diagnostics do not restrict direct use
of any agent or make its approval settings a security boundary.

## Applying changes

Settings are resolved on every `yard` command. The `APPLIES` column in `yard config show` identifies
the consumer:

- `next command` means the resolver uses the new value on the next invocation;
- `yard init` means the value controls infrastructure or provisioning reconciled by `yard init`;
- `config apply` means a file setting can be refreshed in a running local yard with
  `yard config apply`.

`yard config status [--all-local]` checks only materialized file settings in running local yards.
`yard config apply [--all-local]` refreshes those consumers after confirmation. Neither command is a
Git transport command. Configuration readers (`fields`, `show`, `paths`, `status`) remain available
while a release transition needs attention. If a completed, verified release needs only materialized
configuration repair, `config apply` can perform that bounded repair after its normal confirmation.
It rechecks the protected release state before writing and requires release readiness afterward.
Repair uses persisted configuration; differing command overrides are rejected before confirmation.
An unfinished or invalid release transition still requires `yard update`; config apply does not
perform unrelated activation work. When several yards drift, use `--all-local` to cover them all.

Use the typed persistent writers before publishing a change:

```sh
yard config set <SETTING> <VALUE> --scope shared|host|yard
yard config unset <SETTING> --scope shared|host|yard
yard config import <FILE_SETTING> <path> --scope shared|host|yard
yard config edit <FILE_SETTING> --scope shared|host|yard
```

Select a non-default yard with `-Y <yard>` before using `--scope yard`. Each writer validates the
catalog type and scope, rejects secret-looking content and asks before changing the persistent
configuration. `config edit` requires `VISUAL` or `EDITOR` to name one executable.

Register a remote owner once with `yard host add <user@host-or-ssh-alias>`. The confirmation includes
the concrete SSH server-key SHA256 fingerprint, authoritative HostID and discovered yards. Subsequent
refreshes use a controller-managed strict host-key pin; they never silently accept a changed key.
Use `yard host repair <HostID>` to review and explicitly accept a changed fingerprint (and an observed
HostID rename, if both changed). `yard host list` shows registered OwnerHosts, while `yard yards`
discovers their authoritative yards without creating controller-side yard aliases. `--all-local`
never changes remote owner hosts implicitly. The old `remote` command is an input-only compatibility
adapter for installations that still have per-yard contexts.
Legacy discovery snapshots remain untrusted. Confirmed `yard host add` upgrades an exact
same-endpoint, same-HostID snapshot atomically to managed SSH trust.

Ordinary remote commands also handle a missing SSH server key. Before sending the command,
Subyard shows the SSH target, key algorithm, SHA256 fingerprint and exact `known_hosts` namespace
and file. Missing keys along a ProxyJump route are reviewed together in one confirmation. For a
registered remote yard it checks the key against a scan through the owner using its existing or
proposed pin. Confirming first trust verifies the connection, saves only the reviewed keys and
continues the same invocation. Existing entries are preserved. Trust confirmation is a connection prerequisite;
it does not authorize a subsequent destructive command.

Declining, EOF or non-terminal input without `--yes`/`ASSUME_YES=1` leaves trust unchanged.
Automation consent still requires key verification and a successful login. Known keys need no
additional prompt. Changed keys remain blocked: use `yard host repair <HostID>` for a registered
owner or `yard remote repair-key <name>` for a compatibility yard route after verifying the change.
An authentication failure or an unreachable server does not trigger first trust.

## Entity and migration vocabulary

Subyard uses `OwnerHost → Yard → Project → optional ProjectEnv`. A yard is identified as
`<HostID>/<yard-name>`; `local` and `remote` describe access, while `container` and `vm` describe its
runtime kind. The desired L1 Incus image is separate from the image fingerprint observed on a
created instance. Project-environment Docker images are an L2 setting.

Canonical configuration names are:

| Legacy input (one-minor compatibility) | Canonical writer/output |
|---|---|
| `YARD_TYPE` | `ACCESS_KIND` |
| `INSTANCE_TYPE` | `YARD_KIND` |
| `INSTANCE_NAME` | `YARD_INSTANCE_NAME` |
| `REMOTE_DEST` | `OWNER_ENDPOINT` |
| `REMOTE_YARD` | `OWNER_YARD_NAME` |
| `BASE_IMAGE`, `BASE_IMAGE_FALLBACK` | `YARD_IMAGE`, `YARD_IMAGE_FALLBACK` |
| profile-level `BASE_IMAGE` | `PROJECT_ENV_BASE_IMAGE` |
| `YARD_PROFILES` | `ENVIRONMENT_PROFILES` |
| `AGENTS` | `CODING_TOOL_INTEGRATIONS` |

Legacy names are accepted only while reading an old input. A file or command environment containing
conflicting old and canonical values is rejected. `config set`, generated context, JSON and RPC
writers emit canonical names only.

## Versioned private configuration

Keep private desired settings in a separate clean Git checkout. Do not turn the entire
`$SUBYARD_CONFIG_HOME` into a checkout: it also contains project state, credential records, generated
consumers and support tools. `.gitignore`, a symlink farm or recursive `rsync --delete` is not an
ownership boundary.

Release installation and migration never ask for a Git URL or require network access. Connect the
private repository explicitly once on each physical owner host:

```sh
yard config sync connect \
  git@github.com:you/subyard-config.git \
  --host-id workstation-a
```

`sync connect` prepares the clone in a private temporary directory, validates its selected HostID
and exact adoption plan, then asks once before installing the checkout, registering its path and
applying that plan. The default destination is `~/.local/share/subyard-config`; use `--checkout` to
select an existing checkout or another destination. An existing checkout must have the requested
`origin`. A declined or invalid staged clone is removed.

Yard does not store Git credentials. Configure SSH or a Git credential helper on that owner host;
credential-bearing URLs, URL queries and fragments are rejected. Use `--init` only when connecting
an empty remote: after the same preview it creates and pushes a minimal initial manifest using the
operator account's configured Git identity. No background fetch, pull, commit or push runs
automatically.

The checkout root contains a tracked `subyard-config.json`:

```json
{
  "schemaVersion": 1
}
```

Its fixed managed layout is:

```text
subyard-config.json
shared/                         # optional common scope
  config.env
  overrides/
    agents/...
hosts/                          # optional host overlays
  <HostID>/                     # optional selected-host overlay
    config.env                  # optional host-wide scalars
    overrides/
      agents/...
    yards/
      <yard>/
        config.env              # required for an existing yard entry
        overrides/
          agents/...
```

`shared/` and the selected `hosts/<HostID>/` overlay are independent and optional. A source with
only the manifest is a valid empty desired state. A selected host directory may contain only
`overrides/` or `yards/`; its `config.env` is optional. An existing
`hosts/<HostID>/yards/<yard>/` entry still requires `config.env`, because the directory declares a
yard that needs an unambiguous scalar definition. Scalar assignments must be syncable in their
exact shared, host or yard scope. Versioned file settings are regular, non-executable files at
catalog-known paths below `overrides/agents`; path assignments in `config.env` are rejected. The
source manifest schema is
[`config/subyard-config.schema.json`](../config/subyard-config.schema.json).

The first sync snapshots an owner-host ID. By default it uses the current hostname; set
`SUBYARD_HOST_ID=<safe-id>` for the first invocation to choose another value. The saved
`$SUBYARD_CONFIG_HOME/host-id` is local identity state, is never imported from Git and is not renamed
by later syncs. It is not published or added to the source automatically. Each host selects only
`shared` and its exact `hosts/<HostID>` overlay, so another host subtree is neither applied nor an
enrollment requirement. Two hosts may have yards with the same name without collapsing them.

After onboarding, use the bounded sync commands for transport and history. The registered path is
available through `config sync path`, and bare `config sync` imports the current checkout without
network access:

```sh
yard config sync status
yard config sync pull --apply
yard config sync push -m "Update host configuration" --apply
```

`sync status` fetches by default and reports registration, sanitized remote, branch/upstream,
checkout HEAD, dirty/conflict counts, ahead/behind/diverged relation, last fetch, applied commit,
generation and recovery state. `--offline` uses cached refs. A fetch/auth failure still prints the
available local diagnostics and exits non-zero. Automation is manual.

`sync pull` permits only a clean fast-forward of the exact upstream, validates the candidate before
one confirmation, then imports it transactionally. `sync push` exports only explicit
catalog-known, syncable, non-secret persistent settings, creates one commit from `-m` using the
operator's Git identity, validates/imports it locally and pushes only `HEAD` to the exact upstream
without force. It never reads configuration back from a running container and never exports keys,
secrets, projects, generated state or arbitrary runtime files. Dirty, conflicted, detached,
upstream-less or diverged checkouts fail closed; Subyard does not stash, merge, rebase, reset or
resolve conflicts.

`--check` is read-only, never prompts, and exits non-zero when an apply or local manifest update is
needed. A changing sync prints the source commit and exact redacted managed-path plan, then asks once.
`--apply` composes the import with `yard config apply` for affected running local yards under the
same top-level confirmation. It does not run `yard init`, start, stop, teardown, project operations
or remote fan-out. Remaining follow-up commands are printed by application mode.

For bare checkout-to-live import, an existing unmanaged target requires a reviewed first import with
`--adopt`. `sync push` instead adopts only its own exact validated persistent export. Later local
edits are reported as managed drift and restored only through the confirmed exact plan. A path
removed from Git is deleted only when the local manifest owned its previous exact digest. Removing a
yard definition fails while its Incus yard or project state still exists; sync never becomes
teardown. Removing the selected host subtree means an intentionally empty host overlay: the exact
plan removes only its previously managed paths, subject to the same digest, drift and in-use guards.
Unmanaged local paths are left alone.

The source must be an operator-owned Git worktree root with a clean selected subtree. Tracked,
untracked, ignored, unmerged, symlinked, hard-linked, executable or group/world-writable inputs fail
closed. `projects`, desired power, observed Incus state, SSH trust, secrets, keys, generated
consumers, exports, logs, storage and support tools are outside the source schema and local manifest.

To roll back desired settings, check out or revert the intended Git revision and run the same check
and sync commands. An interrupted confirmed transaction is recovered before the next mutating sync;
`--check` reports pending recovery without changing the live root.

When invoked for a registered remote yard, the `config sync` family runs on that owner host. For
example, for HostID `owner-host` and its `default` yard:

```sh
yard -Y owner-host/default config sync connect \
  git@github.com:you/subyard-config.git \
  --host-id owner-host
yard -Y owner-host/default config sync status
yard -Y owner-host/default config sync pull --apply
```

The checkout and Git authentication stay on the owner host; the controller does not upload or cache
the repository and there is no implicit all-host fan-out.

### Bootstrapping an existing host

Before creating optional settings, classify current values with `yard config fields` and inspect
their provenance with `yard config show`. Only fields marked `syncable: yes` may be copied:

- portable fields that explicitly allow `shared` go to `shared/config.env`;
- host-specific fields go to `hosts/<HostID>/config.env`;
- named-yard fields go to `hosts/<HostID>/yards/<yard>/config.env`;
- catalog-known agent config and rules files keep their relative path below the matching
  `overrides/agents` directory;
- secrets, keys, generated consumers, project state, host identity, desired power and support tools
  stay local and are never copied.

A minimal empty remote can be initialized without reading or copying the whole live root:

```sh
yard config sync connect \
  git@github.com:you/subyard-config.git \
  --host-id replace-with-stable-host-id \
  --init
```

For an existing repository, connect it directly. To publish real persistent settings, change them
through the typed writer and let `sync push` build the exact managed export:

```sh
yard config sync connect \
  git@github.com:you/subyard-config.git \
  --host-id replace-with-stable-host-id
yard config set SSH_PORT 2222 --scope host
yard config sync push -m "Set host SSH port"
```

Do not copy `~/.config/subyard` recursively. In particular, do not add ignored secret or runtime
paths just to make the worktree appear clean: selected ignored and untracked source paths are
rejected. After `connect`, the checkout path and saved local `host-id` are authoritative. Each
additional owner host runs its own `sync connect`; a matching subtree does not need to exist in Git
when that host uses only shared settings.
