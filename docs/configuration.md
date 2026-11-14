# Subyard configuration

Subyard combines immutable settings shipped with its runtime, persistent local settings, and
temporary command overrides into one effective configuration. Use the CLI to inspect that result;
the files remain the source of truth.

```sh
yard config fields
yard config fields SSH_PORT
yard config show
yard config show SSH_PORT
yard -Y default config show
yard config paths
```

`config fields` is the public typed field reference. It reports the shipped default, kind, type,
allowed scopes, syncability, merge mode, application mode and domain owner from the same catalog
used by the production resolver. Optional profiles declare their typed scalar fields and defaults
in shipped `profile.json`; these declarations join the core catalog for each operation. `config show` lists effective non-secret settings, their winning
scope and source, and how they are consumed. Passing one setting name shows every applicable layer
as `effective`, `overridden`, or `unset`. Unknown fields, wrong scopes and invalid values fail
closed; secret inputs and unrelated environment variables are not settings.

## Storage roles

The default configuration root is `~/.config/subyard`. It contains several roles, not one monolithic
configuration:

| Path | Role |
|---|---|
| `overrides/shared/config.env` | Local explicitly shareable scalar settings |
| `.sync/settings/` | Offline Git fallback settings and file assets, managed by sync |
| `config.env` | Host-wide scalar settings |
| `overrides/shared/` | Explicitly shareable non-secret file settings |
| `overrides/host/` | File settings specific to this owner host |
| `yards/<name>/config.env` | Yard definition and scalar settings, including `default` |
| `yards/<name>/overrides/` | File settings specific to one yard |
| `.yard-reset-<name>` | Local ownership marker suppressing previous nonlocal yard fallback after an explicit reset |
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

## Resetting one yard

Ordinary `yard teardown` preserves the selected yard's definition and local settings.
`--keep-data` also retains its project, volume and shared infrastructure. To recreate a named yard
with a fresh preset, use:

```sh
yard -Y demo teardown --reset-config
yard -Y demo init --profile <profile>
```

`--reset-config` cannot be combined with `--keep-data`. It removes the captured selected-yard local
registration, legacy local flat definition and all local overrides after physical cleanup verifies.
An interrupted physical cleanup retains the configuration for retry; overrides are removed before
registrations so an interrupted settings cleanup retains the original target identity. Active SSH
sessions block teardown; an unfinished guest without SSH or an unavailable guest agent does not
require a separate stop. An unavailable Incus daemon remains an error.

The reset preserves HostID, credentials, shared and host settings, neighboring yards, and shared
bridge/pool resources still in use. It leaves Git-authored definitions, their offline cache, and
repository legacy definitions intact. A protected local marker prevents their yard scalars and
file assets from silently reactivating the removed registration or influencing its replacement.
A new local registration keeps that boundary; shared and host fallback still applies. Sync can
refresh the retained Git source without registering the reset yard again.

The default yard supports the same local reset and remains the implicit default context. Named
yards disappear from configured inventory after reset. A repeated completed reset is available on
the owner host with the same explicit local command; remote inventory cannot select a removed name.
Recreate the yard on its owner before selecting it through remote inventory again.

## Scalar settings

Common portable values may go in `overrides/shared/config.env` only when `config fields` lists the
`shared` scope. Host-wide values go in `config.env`:

```sh
DEV_SUDO=1
```

Each yard has its own settings, including `default`:

```sh
# ~/.config/subyard/yards/default/config.env
SSH_PORT=2222
```

Scalar precedence is:

```text
shipped defaults, named-yard derivations and selected shipped profile
  -> Git shared -> Git host -> Git yard settings
  -> local shared -> local host -> local yard settings
  -> current command environment
```

The default yard accepts scalar overrides in `yards/default/config.env`, after host-wide
settings. Its optional override file does not register another yard or apply to named yards.
`yard config set NAME VALUE --scope yard` selects this layer when `-Y` is omitted.
A command environment value is temporary and has the
highest precedence. It is never persisted by config sync. `yard [-Y <yard>] config show <SETTING>`
is the authoritative explanation of the actual chain, including derived values. Start from
[`config/settings.env.example`](../config/settings.env.example).

`init` validates the effective `INCUS_PROJECT` before confirmation or host changes, including
with `--reset` and `--configs`. Incus project names cannot contain underscores, must start and
end with a letter or digit, and must be 2–64 bytes long. Named yards default to `subyard-<yard>`;
set an explicit valid `INCUS_PROJECT` when that derivation is invalid. Names are preserved exactly.
Configuration inspection and editing remain available to repair existing settings. An underscore
in the yard name also requires a valid `YARD_INSTANCE_NAME` override because Incus instance names
use hostname rules.

Host/shared values for `INCUS_PROJECT`, `YARD_INSTANCE_NAME`, `SSH_HOST`, `SRV_VOLUME`,
`HOST_BASE` and `RESTRICTED_DISK_PATHS` are inherited by every yard as before. Generic
values keep their existing named-yard derivations. Loading or initializing a yard never
changes its resolved addresses automatically.

Two local configured yards cannot share a project/instance pair or a project/`SRV_VOLUME`
pair. The implicit `default` is always included; remote registrations are
excluded. Init, release activation, teardown, hard cleanup, config status and config apply refuse every
known collision, naming both yards, keys, source files/scopes and any already known marker
owner. Set the conflicting keys at yard scope for the non-owner. When ownership is
unknown, inspect the instance marker before choosing which yard to reconfigure. For a
shared `/srv` volume, separate its data before changing the volume address. A foreign `user.subyard.name` marker also refuses guest
inspection and power metadata writes. No live yard or data is renamed, moved, deleted or
relabeled automatically.

When every release gate blocker is a native-target collision or foreign marker, local
`config set` and `config unset` of `INCUS_PROJECT`, `YARD_INSTANCE_NAME` and `SRV_VOLUME`
remain available at yard or host scope. Other blockers and Git/shared-scope writes retain
their ordinary admission checks.

For a new named yard under custom host values, create its protected `config.env` definition
and set its native addresses at yard scope before init:

```sh
yard -Y demo config set INCUS_PROJECT subyard-demo --scope yard
yard -Y demo config set YARD_INSTANCE_NAME yard-demo --scope yard
yard -Y demo config set SSH_HOST yard-demo --scope yard
yard -Y demo config set SRV_VOLUME yard-srv-demo --scope yard
yard -Y demo init
```

An unmarked existing instance is preserved; a conflicting definition still requires the
same explicit yard-scope repair.

### Git identity

`GIT_USER_NAME` and `GIT_USER_EMAIL` are syncable string settings. Set them in shared,
host or yard scope, or override them for one command. `yard init` writes each nonempty
setting to the developer's global Git `user.name` or `user.email` inside the yard. Its
`git-identity` stage observes and repairs drift only for those configured identity keys;
other Git settings remain untouched, apart from the existing `safe.directory` policy.
An unset or empty setting keeps the host Git identity fallback, without owning that key
or reporting identity drift.

Save portable identity settings in shared scope and synchronize them through the existing
private config Git workflow. Run `yard config sync pull` on each owner host, then
`yard init` to apply the identity changes; there is no automatic pull. Host-side commits,
including config sync and typed writers with `--git`, continue to use the host's Git identity.

The former `$DATA_HOME/gitconfig` drop-in is no longer supported. Release readiness refuses
activation while it exists, suggests commands to import its identity keys, and lists other
keys for manual preservation. Move or delete the file after preserving its settings;
Subyard never edits or deletes it during migration.

### Yard resource limits

`LIMITS_CPU` and `LIMITS_MEMORY` are optional Incus resource limits for both container and VM
yards. A nonempty value is applied at creation and reconciled on existing yards by `yard init`.
Changing a value in configuration takes effect on the next init; readiness checks report a
differing local instance limit as drift.

Before changing a limit on a running yard, init stops it through the existing guarded lifecycle
and then uses the normal temporary-power and finalization flow to restore its desired power.
An intentionally stopped yard returns to stopped after init. Matching limits are left untouched.
Empty or unset values leave manually installed and inherited Incus limits in place; clearing a
setting does not remove a previously applied limit. Subyard imposes no default CPU or memory cap.

`VM_CPU_WEIGHT` optionally assigns a VM a host CPU scheduling weight from 1 to 10000.
The normal Linux cgroup weight is 100; larger values receive a larger relative share under
competition. The owner persists this policy and reapplies it through init, start, network
reconciliation and host boot. It requires systemd and cgroup v2 with unrestricted source
ancestors: Subyard refuses to bypass operator service or slice ceilings. Unset values retain
an installed policy. A direct external Incus restart requires a subsequent product start or
init to restore the scheduling scope.

## File settings

Known file settings, such as coding-agent configuration and rules, start with a shipped file and may
be replaced by the matching file under `overrides/shared`, `overrides/host`, or a yard's
`overrides` directory (including `yards/default/overrides`). Git-derived files are cached under
`.sync/settings/` using the same relative paths. Their precedence is shipped, Git shared/host/yard,
local shared/host/yard, then a command override. A local shared file therefore overrides a Git yard file.

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

Legacy TOML baselines bind the original template's exact bytes, including comments.
An integration can ship historical templates in `config/agents/<id>/legacy-config/*.toml`
so initial integration adoption can verify an unchanged older document after the template changes.
The historical file must match the baseline's SHA-256 and owned paths, and the document must still
match all historical owned values. Runtime additions remain untouched. Unknown templates and
changed owned fields remain conflicts; the new template alone is not proof of prior ownership.
For releases that wrote JSON/TOML before baselines existed, the complete file must instead match a
shipped historical template byte for byte. It must be a developer-owned regular file with mode
`0644`, one hard link, no prior lock or receipt, and no historical owned paths missing from the
current template. Assessment is read-only; ordinary application then publishes ownership evidence.
JSON predecessors use `legacy-config/*.json` in the owning integration. A raw config can pin one
historical digest in `legacy-config/<destination-basename>.sha256`; it is accepted only during
confirmed initial adoption with the usual file metadata checks, and is not persisted as ownership.

Subyard serializes its own writes per asset and atomically replaces the destination before updating
the baseline. Retrying an interrupted application converges. Invalid JSON or an invalid baseline
fails without overwriting it; diagnostics omit configuration contents. Running tools do not share
Subyard's lock: application reads the current file and verifies its result, but cannot serialize
arbitrary concurrent third-party writes. Invalid TOML, like invalid JSON, fails without overwriting
the current document or printing its contents.

## Per-yard coding tool selection

```sh
yard integration status --json
yard -Y default integration enable codex
yard -Y default integration disable codex
yard -Y default integration status codex
```

`CODING_TOOL_INTEGRATIONS` selects the complete requested set through profiles and
persistent configuration. Integration enable/disable writes a yard override in
`yards/<name>/config.env`. An empty assignment explicitly selects no integrations;
an absent assignment remains distinguishable from empty. The compatibility input
`AGENTS=none` means empty. Unknown IDs, duplicate IDs and dependency cycles are errors.
Dependencies declared with `AGENT_<id>_DEPENDS` are computed separately: enabling such an
integration also makes its dependency effective, and disabling a dependency that a selected
integration still requests is rejected. Status reports the requested
set, effective set, dependency reasons, configuration source and observed yard readiness.
An ID filters membership and dependency details; readiness describes the whole yard.

For an absent named yard, `yard -Y example init` creates a protected local definition and
chooses an available owner-local `SSH_PORT` in its confirmed plan. It explicitly saves empty
`YARD_TEMPLATE`, `ENVIRONMENT_PROFILES` and `CODING_TOOL_INTEGRATIONS`, so the new base yard
selects neither a preset, environment profiles nor coding tools. These are independent settings.
An existing yard's plain init preserves its saved selections and endpoint; an interrupted init
resumes through the same command. `--profile <name>` retains its named-yard preset behavior.

For registered yards, `yard init` uses the selection resolved from profiles and persistent configuration;
no tool-selection argument or explicit per-yard assignment is required. Default and
named yards inherit `claude codex opencode pi` unless their profiles or
settings select another set. Init preserves inheritance, so later profile changes
remain effective. It only canonicalizes an explicit local legacy `AGENTS` assignment;
registered configuration sources are consumed without a local selection write.
Subyard does not infer intent from installed binaries or change other yards during
adoption. The default yard uses the same scalar and file configuration layout as
named yards. An explicit empty selection stays empty. A local selection overrides the Git fallback;
use `config set CODING_TOOL_INTEGRATIONS "<requested tools>" --scope yard --git` to save the
selection in the registered source and immediately commit and push it.

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
owned fields while retaining the document and unrelated fields. If owned fields changed after
Subyard applied them, for example an operator removed or edited one, they no longer prove which
values Subyard wrote. Retirement then releases their ownership and leaves the document
byte-for-byte unchanged instead of refusing. `yard integration status` warns which file and
fields stay. The command that performs the release prints a result line naming the yard, file
and fields that Subyard no longer manages. Remove those fields by hand if they are unwanted.
A later enable applies the shipped fields again with fresh ownership. An interrupted retirement
still accepts only the exact recorded before and after fields, and missing or unproven ownership
evidence still blocks retirement. A disable after such a change while release activation is
stale stores only the selection. `yard migrate --check` then names `yard -Y NAME init`, and
`yard migrate` refuses, because its installed-view repair would first restore the changed fields.
Run `yard -Y NAME init` to perform the release; a yard left in this state by an earlier release
recovers the same way. Materialization baselines
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
also need a protected matching materialization baseline or the exact pre-baseline JSON/TOML proof
described above. Unrelated fields and
session targets remain preserved. Unknown or changed state fails before adoption.
`yard integration status` assesses this same initial adoption without changing files or
creating inventory. Matching legacy artifacts appear as pending work; a conflict identifies
every independently determinable artifact or prerequisite that prevents adoption.
Release activation reconciles running ordinary yards before refreshing their configs,
including unfinished inventory application. Stopped yards remain unchanged.
Integration enable/disable does not perform this initial adoption. Interrupted
release updates resume within the same approved desired artifact scope.

A retired integration ID stays valid in authored settings: it is filtered at load, listed as
retired and ignored in `yard integration status`, and its old settings are discarded without
rewriting the file. `yard integration enable <id>` and `yard config set` refuse it, while
`yard integration disable <id>` removes it from the yard's selection. A dependency that only the
retired integration requested stops being effective, keeps its binaries and authorization, and
can be selected again, for example with `yard integration enable codex`. While a selected retired
integration has no recorded retirement, `yard init` can temporarily start a stopped yard
whose disk-backed `/tmp` policy is complete, remove it and restore stopped power. If both
retirement and disk-backed `/tmp` are pending, run `yard -Y NAME start`, then `yard migrate`;
the same sequence finishes both and records verified retirement, including an already-clean guest.
An update does not boot a stopped yard. Paseo removal
also removes foreign units and all state except a non-empty real `/srv/agents/paseo/worktrees/`
directory, which stays with one warning. To use Paseo, install an older release. See the
[retired package contract](reference/shipped-integration-contract.md#retired-package-compatibility).
A previously accepted `AGENT_<name>_CLEANUP` value is ignored in the same way.

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

Integration enable/disable saves the requested selection locally, including when a Git source is
registered. `config sync --apply` refreshes file consumers and does not replace the integration
lifecycle. To publish the selection, use `config set CODING_TOOL_INTEGRATIONS "<requested tools>"
--scope yard --git`; a pre-existing local selection remains effective until it is unset.

`integration status [--json]` lists the whole coding-integration catalog for the selected yard.
Each row explains requested and effective selection, dependencies, role suppression and the
winning and overridden sources. Component health is inspected only for selected components;
the top-level `observed` field describes yard convergence (`observed_scope: "yard"`). A ready
yard does not establish that an unselected component is running. `profile list [--json]` lists
all shipped environment profiles, including provision hooks, resources and role-forbidden
profiles. Its selection reasons distinguish declared defaults, explicit empty lists
and inherited Git selection from local overrides. When `ENVIRONMENT_PROFILES` is absent,
only eligible profiles declaring this yard in `default_yards` are selected. An explicit
empty list selects no profile, provision hook, resource or extras. A profile without a
descriptor must be selected explicitly. The same resolved effective set governs provision,
extras, resources and project target admission; project settings cannot silently expand it.
Activate a profile before importing or starting a project that targets it. Existing projects
can still be stopped or removed after their profile is deselected.

The shipped `test-vms` role has `ALLOWS_CODING_TOOLS=false`. Its inherited tools are
suppressed, explicit nonempty selections fail, and agent-only utilities such as
`ccusage` are excluded. Cleanup of an existing running test yard uses the same ownership
checks and preserves broker keys, inner VMs, profile resources and test data. The role
does not restrict arbitrary software installed or launched by the operator. GitHub
remains an optional environment profile; Orca remains a profile resource. `ccusage`
is a core utility in ordinary yards, not a selectable integration.

## Capability skills inside the yard

Provisioned capabilities install user-level skills available from any project, including projects
without a Subyard checkout. Descriptions let agents select relevant guidance: core provides static
web preview, Android provides toolchain and broker-managed emulator use, and GitHub retains its
existing temporary authorization workflow. Skill presence records successful provisioning;
bounded live probes establish current readiness separately.

Codex discovers `~/.agents/skills`, Claude Code `~/.claude/skills`, and OpenCode
`~/.config/opencode/skills`. These directories link to one canonical installed copy per skill.
Links are ready before installing a client and persist through client enable/disable. Successful
profile provisioning registers profile skills. Static refresh updates content and removes disabled
providers without restarting capability services. In an installed runtime, missing links or other
skill drift can require a fresh assessed repair: run `yard migrate --check`, then `yard migrate
--yes` on the owner host without a yard selector. Completed release history does not authorize
ordinary initialization to repair core runtime drift. Foreign skills and conflicting paths are
preserved and diagnosed.

Preview guidance has moved out of global instructions. Recognized old inventory-owned blocks are
removed during refresh while retaining other text; changed/ambiguous content requires inspection.
Host instruction sources and frozen historical recovery recipes remain intact. See the
[delivery contract](../config/skills/README.md) for provider integration and ownership details.

## Coding-agent compatibility

Codex, Claude Code, OpenCode and pi can run directly in the yard. Their versions are not a
Subyard compatibility allowlist. Claude provisioning resolves the official `latest` native release,
verifies its platform SHA-256 against the versioned manifest, and atomically installs it under
`/opt/subyard/claude` with `/usr/local/bin/claude` pointing to it. A newer working managed installation
is preserved; an existing command owned by another installer is left unchanged and reported as a conflict.
The package check verifies both the native CLI and the managed permission policy without downloading updates
or accessing user authorization. When the Codex provisioning hook runs, it resolves the current
stable official release, verifies the `codex-package-<target>.tar.gz` asset against that release's SHA-256
metadata, and installs the whole package under root-owned `/opt/subyard/codex/releases/<version>`.
`/usr/local/bin/codex` points to `/opt/subyard/codex/codex`, a link to that release's `bin/codex`, which keeps
the path a retained release's hook replaces on rollback. A newer working installation is preserved.
Provisioning keeps the published release, the one it replaced and any release a running process uses, and
removes the rest.
Legacy `CODEX_VERSION` and `CODEX_SHA256_*` assignments are ignored when loading existing
settings; new writes to these retired fields are rejected. Already-converged yards do not check for upstream updates
on every `yard init`, and `yard status` does not install or update agents.

In a yard selecting Claude or Codex, only provisioning replaces `/usr/local/bin/claude` and
`/usr/local/bin/codex`. By process image, a paired Orca terminal and Orca's native chat run these
managed binaries for both agents, and Orca's Codex launcher and a login shell resolve to the managed
Codex. An explicit path to another executable runs that executable.

Claude can also be enabled after Orca has already installed and run its own copy.
The confirmed `yard integration enable claude` or `yard init` plan adopts only the
integration's declared persist directories (`~/.claude/projects` and
`~/.claude-memory`), and only where a real directory exists. Adoption never stops
sessions, never replaces a target entry and never crosses a mount point. The source and
an existing target must be owned by the developer and not writable by group or others:

1. Preflight lists every refusal at once: unsafe roots, unsafe, special or unreadable
   entries, a mount point in the source or the target, a source and target that are the
   same directory or contain each other (compared by directory identity, not by path),
   unsupported renames on either filesystem and missing space (the source size plus a
   1 GiB reserve on the sessions volume). Nothing is written before it passes.
2. It records the transfer in `<directory>.subyard-transfer.json`, copies every entry
   into a flat stage `.subyard-stage-<yard>-<id>` beside the target under new names
   created exclusively, then makes one catch-up pass that copies changed files again.
3. It renames the copies into the target without replacing anything. The target wins:
   an entry that already exists there is not transferred, and where the target has a
   non-directory in place of a source directory, that whole subtree is not transferred.
   The plan lists, bounded with a count, the entries that already exist in the target;
   their source copies are deleted with the backup. Only directories the transfer
   creates get the source modes; existing target directories are never changed. If the
   target refuses entries (permissions or space), every refused path is reported as
   pending configuration and there is no exchange; the next `yard init` restarts it.
4. One atomic exchange replaces the directory with a link to the target; the original
   directory lands at `<directory>.subyard-backup-<id>`. The path is never absent.
5. Finish removes the transfer's own stage copies, the backup and the record. Nothing
   is deleted or overwritten before the exchange.

Accepted limits: lines a session writes after the catch-up pass, or through a
descriptor held across the exchange, are lost with the backup, as are source entries
that were not transferred because the target already had them. A session that opens its
files by path writes into the target after the exchange. A foreign file deliberately
given the transfer's random copy prefix inside its stage is removed with the stage. Mount
points are recognized by mount identity on each opened descriptor, so a bind mount of the
same filesystem counts. The configured roots and their ancestors are trusted: root inside
the yard mounting over them during an operation is outside this guarantee. Process
interruption is tested; power-loss durability rests on the fsync order. The exchange
requires `renameat2(RENAME_EXCHANGE)` and `RENAME_NOREPLACE`. ZFS-backed yards are
unverified.

Finish deletes a backup only while it is still the recorded original directory on its
parent's mount. It never follows a link or enters a mount. Anything finish cannot remove
(a changed backup, a mount inside it, foreign entries in the stage, or a filesystem error
such as a denied access) is kept and the record is removed anyway. `yard integration status` lists each retained
`<directory>.subyard-backup-*` and `.subyard-stage-<yard>-*` by name, with its size (or
unknown) and an inspection command; remove it manually once its data is no longer needed.
Retained items are state, not a warning, and never block activation, init or deselection.

Transfer state is independent of the selection. An adoption interrupted before the
exchange restarts on the next `yard init`: its stage copies are discarded and the transfer
starts again; copies already in the target stay and win. If the integration is deselected
first, the transfer is cancelled: the directory is unchanged, staged copies are discarded
and copies already in the target are kept. An adoption interrupted after the exchange is
finished by init, deselection or release activation. After deselection finish also
removes the link, but only while the transfer record names it and it points to the
recorded target; the data remains in the target. A link that a finished transfer
published but an interrupted reconcile never recorded as an owned artifact is removed by
init or disable of the deselected integration, disclosed in the plan, only while it is the
developer's link to exactly the configured target; release activation leaves it. Release activation finishes a
committed transfer but never starts or restarts one; finishing there is best-effort, and
another pending item can defer it to the next `yard init`, which completes it. A new
adoption stays pending configuration until `yard init`. That plain `yard init` of the yard is admitted as the
release repair even without configuration drift, when every pending item names it as the
next step. Each typed transfer conflict is pending configuration with a next step and none blocks
unrelated commands. Transport failures and program crashes stop the operation as inspection failures.
`yard init` binds the transfer it disclosed: if the source, target, operation or the
listed existing entries change before provisioning executes it, nothing is transferred
and the next `yard init` shows the new plan. A plan that went stale in other ways stops
that operation and is not pending configuration. A repeated run without changes does nothing.

The destination JSON settings retain foreign fields, including Orca hooks, model
and theme, while Subyard applies `autoMemoryDirectory` and its `permissions` fields.
A document containing only foreign fields can be adopted without a historical
baseline. Unknown plain instruction files, unsafe metadata and changed ownership
evidence still require explicit inspection or moving the foreign file aside.

Orca's agent-view command (`claude --plugin-dir …/orca-agent-view`) uses the same
managed command as its terminal and native chat. Initialization removes only the
recognized native `~/.local/bin/claude` shadow link, retaining its version files and
running sessions. The managed update lock prevents later Claude launches from
recreating it. Custom absolute executable paths retain their explicit meaning.

When Claude is not selected, `yard integration status claude` and detailed
`yard -Y NAME status`
report an Orca-installed copy as **unmanaged**. It remains self-updating and receives
no managed commit gate, persist links or instructions. This is informational state,
without a readiness refusal or warning. Orca does not depend on or automatically
enable the Claude integration.

- Claude: the managed drop-in sets `env.DISABLE_UPDATES = "1"`. `claude update` and `claude install`
  each print that updates are disabled by the administrator and create no native installation, and an
  interactive launch under Orca's data home installs nothing; with the lock removed, the same launch
  installs a native link.
- Codex: unlike Claude's locked binary, **Codex's daemon updates itself from OpenAI**, as upstream ships it.
  Subyard does not change that. The command itself stays with provisioning:
  `/etc/codex/requirements.toml` pins `check_for_update_on_startup = false`, because the startup notice
  points to installation options that would shadow the managed command, and `codex update` does not change
  the managed command.

#### Where Codex runs

An interactive `codex` from an SSH or VS Code shell prefers upstream's shared app-server daemon:

- **Plain `codex`:** the client is `/usr/local/bin/codex`, but its turns, tools and model requests run on a
  copy of the package in `~/.codex/packages/app-server-daemon`, owned by the developer. The first plain
  client makes that copy and starts the daemon, which keeps running after the client exits. Later clients
  join it. If the daemon is stopped, the next plain client starts it again from the selected copy, not from
  `/usr/local/bin/codex`.
- **`codex --no-daemon`:** runs entirely on the `/opt` release. So does `codex exec`, including while a daemon
  runs.
- **Orca:** terminal and native chat launches add `--no-daemon`, run the `/opt` release and do not start a
  daemon. Its launcher skips that for `agents`, `queue` and `--remote` launches, and for every launch when the
  server sets `ORCA_CODEX_ISOLATE=0`; those use the daemon as a plain client does.
- **Disconnects:** when a plain client's terminal dies during a slow tool, the tool finishes on the daemon
  and its result reaches the model. Under `--no-daemon` the tool stops with the client.

#### Codex version drift

The daemon and the command can run different versions. `codex app-server daemon version` reports both, as
`appServerVersion` and `cliVersion`.

- Provisioning a new release moves the command only. Open sessions and new plain clients stay on the running
  daemon, and a newer client warns that the background service is older.
- With no `~/.codex/app-server-daemon/settings.json`, the updater is on and replaces the daemon with OpenAI's
  latest release, so the daemon can then be newer than the command. Open sessions survived this.
- `updater.autoUpdateEnabled` in that file is yours to set. Provisioning does not read or change it, or stop
  or remove the daemon. With `false`, the daemon stays on its version until `codex app-server daemon update`
  installs the latest release.
- A daemon update or restart waits for running work only briefly, so it can stop a long tool. Open sessions
  stayed connected.

See `codex app-server daemon --help` for the commands.

The approval policy held on every daemon version tested (0.162.1, 0.165.0, 0.166.0): a `git commit` prompted
and named the command, a declined one kept history, and an approved one added one commit. That includes a
daemon started before the policy was installed and a client on a different version. It remains a Codex
command policy, enforced by the Codex process, and plain turns run that process from a copy the developer owns.

#### The shared Codex daemon environment

All plain clients share one daemon, and their tools see the environment of the client that started it, not
their own. A variable set for one client did not reach its tools, and every other client's tools saw the
starter's value. Under `--no-daemon`, each client's tools see its own environment. Subyard's default
credentials do not depend on this: the default `SSH_AUTH_SOCK` is the stable `~/.ssh/subyard-agent.sock`,
GitHub tokens are per `subyard-github run` process, and Orca tokens go only to `--no-daemon` launches. A
forwarded per-session agent socket (`FORWARD_SSH_AGENT=1`) is captured from the starter and can go stale
when that session ends; use `codex --no-daemon` for work that needs it.

To keep one client out of the shared daemon, use `codex --no-daemon`. To turn off daemon auto-start for a
yard, `[features] daemon_auto_start = false` in the requirements file is honored and user configuration
cannot override it, but Subyard does not ship that, it does not stop a running daemon, and `codex agents`
still starts one.

#### Codex disk use and daemon recovery

Each release is about 430-520 MB under `/opt`. Each selected daemon copy adds about 470 MB in the developer's
home under `~/.codex/packages/app-server-daemon/releases`. Replaced copies are not removed automatically.

If `~/.codex/packages/app-server-daemon` is removed while its state files remain, plain `codex` fails and
`--no-daemon` still works. Stop any daemon, then remove `~/.codex/app-server-daemon/daemon*` and
`~/.codex/app-server-control`; the next plain client copies the package again.

#### Converging existing yards

Provisioning installs the approval policy before it publishes the binary and before it changes
anything in the developer's home, so no state below can prevent the policy.

A yard on an earlier release has a bare binary at `/opt/subyard/codex/codex` and a Claude drop-in without the
update lock. `yard update` sees the changed provisioning hooks and converges both: a link to the package release
replaces the bare binary, and the drop-in gains the lock. A session still running the bare binary keeps running, and
`yard status` reads `release OK (ready)` afterwards. An upstream daemon package from earlier use stays, and plain
clients keep using it.

`yard update --rollback` runs the retained release's hooks, which put a bare binary back and remove the lock. The
package stays under `releases` until a later upgrade prunes it, and updating forward again converges the same way.

Readiness trusts the installed checks. Running an earlier release's hook by hand on a current yard removes what it
does not know about, such as the Claude lock, and installs a check that still passes, so `yard status` stays ready.
The same applies to Codex, where the earlier hook puts a bare binary back at `/opt/subyard/codex/codex`, and
`yard init` repairs neither. Copying the current release's `config/agents/claude/provision.sh` and
`config/agents/codex/provision.sh` into the yard and running them as root restores the link, the lock and both
checks.

`yard init` and release activation converge a yard that already has a self-installed link, even when
provisioning is otherwise current. They remove only the exact native link: `~/.local/bin/claude` pointing
to an absolute `.../claude/versions/X.Y.Z` owned by the developer, or `~/.local/bin/codex` pointing to
`~/.codex/packages/standalone/current/bin/codex`, in a directory the developer owns and can write.
Version directories, `~/.codex/packages` and unknown files stay, so a running session keeps working; the
next session uses the managed binary. Until then `yard status` shows `release NOT OK (migration-required)`
with next step `yard migrate`. `claude-policy-check` or `codex-policy-check`, run directly, names the link
and says to run `yard init`; `yard status` does not show that check's output. Running an upstream
installer by hand recreates the link, and the next `yard init` removes it again.

Any other file at those link paths stays unchanged and does not fail readiness, because only the developer can
decide it, though a login shell can run it instead of the managed binary. `yard status` stays
`release OK (ready)` and does not show a `Conflict:` line for it. The line appears when you run the package
check directly, and in `yard init` output only when provisioning runs, so a `yard init` with nothing to
provision prints none.

### Commit and push approval prompts

`CODING_TOOL_GIT_APPROVAL` is a syncable setting with the values `prompt` (default) and `off`.
It governs Claude and Codex only; OpenCode keeps its home ask patterns. The prompts live only in
the root-owned files that provisioning writes, the Claude managed drop-in and the Codex managed
requirements; the Claude and Codex home defaults carry no Git rules. To turn the prompts off for
one yard:

```sh
yard -Y <yard> config set CODING_TOOL_GIT_APPROVAL off --scope yard
yard -Y <yard> init
```

Set `prompt` the same way to restore them; shared or host scope applies the choice to more yards.
Until `yard init` applies a change, it is listed as a pending init-owned setting.
`off` removes only these rules: the Claude update lock and the Codex approval policy, reviewer and
`rm` rule stay. `claude-policy-check`, `codex-policy-check` and `yard status` verify the installed
mode, so `off` is expected state, not drift. Existing sessions keep their loaded policy until restarted.
With prompts off, the remaining boundary for pushed changes is the remote's branch protection.

Limits:

- When Claude runs under an organization's server-managed settings, the yard's drop-in is not the
  effective source, so that policy must carry the commit/push ask rules itself.
- A release without this setting refuses it wherever it is set, and refuses an installed snapshot
  that records `off`. Before switching to such a release, run
  `yard -Y <yard> config unset CODING_TOOL_GIT_APPROVAL --scope <scope>` at every scope that sets it
  (add `--git` for a synced assignment), then `yard init`.
- Updating an installation older than v0.14.0 (Codex) or v0.23.0 (Claude): agent sessions started
  after the home defaults are refreshed and before that agent's provision hook writes the managed
  policy get no commit/push prompt. Hooks run in the yard's agent order and stop at the first
  failure, so a failed earlier hook extends this until the next successful `yard init`.

### Codex permissions across projects

The default yard configuration permits local edits, builds, tests and network access without
approval. In `prompt` mode, managed rules classify canonical direct `git commit` (including
`--amend`) and `git push` as requiring user approval across projects. Repeated single-use approval also depends
on the client's response behavior:

| Client | Yard mode | Approval evidence |
| --- | --- | --- |
| Terminal | Run `codex` without permission overrides. | The owning VM scenario checks local work, denial and repeated commit/amend/push prompts. |
| VS Code Codex extension | Select **Custom (config.toml)**. | Loads the yard defaults; client-specific denial and repeated single-use approval still need acceptance. |
| Orca terminal | Use **Manual** with empty Codex arguments; Subyard also removes the stock YOLO argument. | The owning VM scenario exercises the paired terminal, including denial and repeated Git prompts. |
| Orca built-in Codex chat | Use **Manual**; the Subyard adapter reads the effective Codex configuration for the chat directory. | The managed relay exposes only single-use choices and cancels session or rule-amendment responses. The owning fixture exercises the stock native-chat approval RPC and reconnect; the graphical Desktop UI is not separately certified. |

The terminal and Orca checks belong to the profile's
[`codex-permissions` VM scenario](test-vms.md).
Its coverage does not certify every installed client version or active session. In particular,
the Orca terminal and built-in chat are separate approval surfaces.

Codex provisioning installs root-owned `/etc/codex/requirements.toml` using Codex's native
[managed requirements](https://learn.chatgpt.com/docs/enterprise/managed-configuration).
It requires `on-request`, the `user` reviewer and, in `prompt` mode, commit/push prompt rules
independently of the home rules. Project settings, profiles, alternate `CODEX_HOME` and client flags cannot remove
these requirements. Incompatible overrides such as `never` or automatic review cannot take effect.
The home config supplies the existing `danger-full-access` yard sandbox default; stricter sandbox
choices remain available. Use **Custom** in clients that offer permission presets: unrestricted
filesystem access and mandatory command approval are separate settings.

The home rules explicitly allow `rm`, including recursive cleanup of build fixtures. Without a
matching allow rule, Codex can still request approval for removal commands under `on-request`,
even with `danger-full-access`. The allow rule relies on the yard as the isolation boundary;
it does not limit removal to the current project. Managed commit/push prompts remain in force.

After updating Subyard, run `yard init` to reconcile existing yards, then start new Codex sessions.
Existing sessions must be restarted to load the policy. `codex-policy-check` checks the installed policy's
contents, ownership and permissions; `yard init` repairs managed drift. Provisioning refuses to
overwrite an existing requirements file owned by another policy.

This is a Codex command policy, not an OS boundary against another executable, alternative Git
spellings or direct writes to Git metadata. Yard root can change the requirements. Enterprise-managed
requirements can also have higher precedence; their effective policy needs separate acceptance.
The native terminal UI offers single-use approval for these managed rules. A raw app-server
client can submit `acceptForSession` even when the request advertises only `accept` and `cancel`;
a native probe confirmed that the next matching commit then runs without another prompt.
This is protocol behavior, not evidence that a terminal or IDE offers that choice. Managed
clients must preserve single-use responses. The Orca managed relay filters both offered decisions
and responses, cancels unsupported responses, rejects duplicate/stale approval IDs, and terminates
the native child when its client disconnects with an unresolved approval. It does not save approval
rules. Unknown approval protocols fail closed. VS Code's actual approval UI remains unverified;
**Custom** alone does not establish its single-use guarantee.

`yard status` runs the installed policy check and one offline Codex rule check in a running yard:

| State | Evidence |
| --- | --- |
| `rules-ok` | The installed policy, including its commit/push prompt rules in `prompt` mode, passed readiness; Codex's native home-rules matcher left the checked local commands ungated. The hint names the mode. |
| `incompatible` | Installed policy drifted, the native command failed, or the home rules did not meet the contract. Run `yard init` or review the rules. |
| `unverified` | Claude, OpenCode and pi have no supported offline approval check in Subyard. |
| `missing` / `?` | The CLI was not found, or the check was unavailable, timed out, or not run because the yard is stopped. |

The probe runs as the developer user with the default home CLI directories followed by system
directories on `PATH`. It uses the CLI capability directly, without interpreting its version
number. Standard guest utilities bound execution time and captured output. Raw CLI output and
configuration never appear in status; the guest returns only a result code. No model APIs,
agent sessions or updates are started by the check.

Codex uses its offline [`execpolicy check`](https://learn.chatgpt.com/docs/agent-configuration/rules)
to check that `git status` and `sh dev-check.sh` are not gated by a home rule. The commit/push
rules are verified by the installed policy check; an operator's own home Git rule is accepted.
These commands are arguments to the matcher; Git and scripts are never
executed. All default-home `*.rules` files participate.

`rules-ok` proves installed policy readiness and the home matcher result. Subyard does not inspect
active sessions or prove that a client displays approval requests. Other Git spellings such as
`git -C` and `git -c` are outside this check; matching raw `bash -lc` input also does not model
Codex's runtime shell decomposition. Single-use approval and denial require runtime acceptance.

### Other agent permission modes

For Claude, OpenCode and pi, status states the missing active-client evidence without launching
sessions. Installation checks do not certify an interactive approval UI:

| Backend | Shipped yard setting | Limit of the evidence |
| --- | --- | --- |
| Claude Code | `permissions.defaultMode = "bypassPermissions"`, with Bash allow and, in `prompt` mode, native managed commit/push ask rules. | Native SDK checks cover bypass/default, repeated Git approval, session allow updates, local/project allows and preservation of user/project denies. Those checks also cover `auto` with a synthetic classifier and the installed managed binary; they use the client's own classifier requests, so server-side auto-mode review is not covered. A native terminal fixture with explicit ask settings exposes only Yes/No and repeats the prompt. The Orca `managed-agents` lane covers stock Orca terminal arguments and Orca's built-in Claude chat with a synthetic model. Other IDE clients remain unverified; Subyard publishes no single-use guarantee for them. |
| OpenCode V1 | Global allow, with commit/push ask patterns under `permission.bash`. | **Unsupported for the strict single-use contract.** Native `once` prompts again; `always` permits a later matching command without another prompt. Project/agent/UI overrides can weaken the defaults. Direct launch remains available. |
| pi | `defaultProjectTrust = "always"`. | Project trust is not a per-command approval gate; Subyard supplies no verified outer gate. |

`bypassPermissions` is the standard Claude mode in a yard, with `autoCompactEnabled: true`. Claude Code can offer on first launch to make
auto mode the default, with **Yes** preselected; decline it. Accepting writes
`permissions.defaultMode = "auto"` to `~/.claude/settings.json`, which `yard config status` reports as drift
and `yard config apply` restores. Declining leaves `yard config status` clean.

Claude provisioning installs its own root-owned drop-in at
`/etc/claude-code/managed-settings.d/50-subyard-git-approval.json`. It adds ask rules (in `prompt` mode) and the
update lock described above; it preserves other managed files and user/project denies, and
refuses foreign files at its own managed path. It does not enable `allowManagedPermissionRulesOnly`,
which would discard those user/project restrictions. `claude-policy-check` verifies the managed
runtime and its command link, the drop-in's contents and permissions, and the absence of a native
self-installed link; it does not show that an active client loaded the policy. Both `claude-policy-check`
and `codex-policy-check` report every refusal they can determine, not only the first.
In a new client, inspect `/status` for the managed file/drop-ins source and `/permissions` for
the effective rules. A higher-priority enterprise policy or a CLI without native drop-in support
needs its own native acceptance; file presence does not prove that the client loaded it.
Use `claude --permission-mode default` and one-time approval when inspecting native terminal behavior; the yard's
standard bypass mode is also covered by the SDK fixture. A native CLI terminal check using
`--permission-mode default --settings <fixture>` offered only Yes/No, approved two separate
commits only after two Yes responses, and retained the original HEAD after two No responses.
This terminal check used explicit ask settings; the installed managed-drop-in precedence and
session-update checks use the SDK. In `auto`, the same checks prompt for commit, amend and push, and
those commands appear in no request to the classifier.

Claude launched from Orca has its own acceptance under the installed drop-in, with a synthetic
model. A paired Orca terminal running the stock `claude --dangerously-skip-permissions` command shows
a prompt naming each commit, amend and push. An approved commit adds exactly one commit and an approved
amend adds none, a declined command keeps `HEAD` and the commit count, and the process image is the
managed binary. Orca's built-in Claude chat raises a pending approval naming each Git command, with
**Allow**, **Allow for session** and **Deny**, through Orca's agent session interface, with the same
commit counts. **Allow for session** does not suppress the next commit or push prompt.

Push is checked only as far as the prompt. The fixture remote is unreachable, so these checks show
neither a completed push nor that a declined push would otherwise have reached a remote. They also do
not cover real accounts or the rendering of Orca's desktop window. The prompt checks decline the auto mode
offer; a separate launch accepts it only to read the drift it causes. No other IDE selection has completed client
acceptance, so Subyard publishes no single-use guarantee for those clients.

Orca's Claude hook install and terminal launch keep `autoMemoryDirectory` and `permissions` in
`~/.claude/settings.json`. If those keys drift while Claude is selected, `yard config status` reports
drift and `yard status` shows `release NOT OK (migration-required)`; `yard config apply` restores them.

Claude's [SDK permission evaluation](https://code.claude.com/docs/en/agent-sdk/permissions)
checks explicit ask rules before permission-mode defaults. Native managed rules still prompt after
the SDK submits session allow updates; that evidence does not establish the layout or choices of
another terminal configuration or an IDE. See [managed settings](https://code.claude.com/docs/en/managed-settings)
for drop-in precedence and `/status` inspection. OpenCode's
[permissions](https://opencode.ai/docs/permissions/) include automatic approval and an `always`
response; a client that permits persistent approval does not provide the single-use contract.
Run `opencode-permission-check` in the intended project to inspect its native resolved V1 defaults
without printing configuration. It reports configured protected/ordinary decisions and the count
of agent permission overrides, then exits 3 with `unsupported-single-use` even when all checked
Git commands ask. An unavailable native config/schema exits 2 with `unverified`. This diagnostic
uses `opencode debug config --pure` (external plugins disabled), bounds native output to 1 MiB
and ten seconds, and emits only fixed decisions (`allow`, `ask`, `deny`, `unknown`). It does not inspect saved session
approvals, enforce rules, or certify an active client. In direct OpenCode, choose `once` or reject
each request; `always` and auto approval are outside the contract. Experimental V2 plugins are
not a managed Subyard surface. These limits also apply when Orca launches OpenCode.
Direct use remains available. Unsupported approval guarantees do not mean an agent cannot run,
and none of these settings creates an OS security boundary.

## Applying changes

Settings are resolved on every `yard` command. The `APPLIES` column in `yard config show` identifies
the consumer:

- `next command` means the resolver uses the new value on the next invocation;
- `yard init` means the value controls infrastructure or provisioning reconciled by `yard init`;
- `config apply` means a file setting can be refreshed in a running local yard with
  `yard config apply`.

Settings marked `yard init` store desired intent. On an initialized yard, changing
them does not require `yard migrate` or block ordinary commands before application.
Run `yard init` to reconcile the desired settings. Release readiness retains recorded installed
init-owned values while checking current helper bytes, protected receipts and ownership.
Repair after a completed release transition, including a completed rollback, uses that
installed view. An interrupted repair whose source equals its target retains the same view;
ordinary unfinished updates and rollbacks keep their desired view.
The repair direction does not change this choice. The installed view covers the integration
applied snapshot, including identity, profiles and effective integration selection, and core-guest
receipts, including recorded provisioning inputs such as `CCUSAGE_PROVISION`. Complete integration
snapshots also record absence. Init-owned values without an installed record, including
profile-owned settings, remain desired in both approving and resuming processes, including their
ordinary failures. Repairs by their owning runtimes use ordinary plan steps; these values are not
listed as pending init. Pending integration detection shares the activation yard loader's installed
overlay. Test-yard route inspection retains its ordinary desired configuration loading.

After the release is ready, fresh and resumed repairs list pending init-owned integration
settings and run ordinary `yard -Y NAME init` for the disclosed yards. Detection covers effective
integration selection, recorded integration inputs, identity and profile selection, and core
guest receipts. It does not detect resource limits or other init-owned changes outside those
records; apply those with explicit `yard init`, as before. Stopped, absent and unavailable yards
are listed without inspection. An unreadable desired integration source makes that yard
unavailable for pending detection; this inspection does not add a release blocker.
Uninspected yards are listed even when none is pending. Ordinary configuration loading,
including the outer update command, keeps its existing source validation.

Before each post-repair init, the listing is compared with the confirmed listing. Unchanged
settings use that confirmation; explicit `--yes` or `ASSUME_YES=1` also approves changed settings.
Otherwise changed settings require init's own interactive confirmation, or a printed continuation
command when no terminal is available. Ordinary admission and guards apply; all refusals are
reported while release repair stays complete. Settings irrelevant to effective consumers do not
remain pending after successful init. See [Release migrations](control-plane.md#release-migrations).

A retained shipped provisioning path follows the active release only when the current value is
that release's own copy; operator paths stay exact. A path captured through a process handle
follows that release's copy of the same shipped file even while another value awaits init.
`yard profile enable` and `yard profile disable` record only the owner-service profile they apply.
Release repair restores the recorded installed profile selection; pending desired selection belongs to
ordinary init. Corrupt installed artifacts and unsafe configuration retain their existing guards.

| Installed consumer | Settings applied by `yard init` |
| --- | --- |
| Preview endpoint | `WEB_PREVIEW_HOST_PORT` |
| Guest usage reporter | `CCUSAGE_VERSION`, `CCUSAGE_SHA256_AMD64`, `CCUSAGE_SHA256_ARM64` |
| Coding tools and skills | `CODING_TOOL_INTEGRATIONS`, `CODING_TOOL_GIT_APPROVAL`, `ENVIRONMENT_PROFILES`, `ALLOWS_CODING_TOOLS`, `DEV_USER` |
| Custom integration inputs | `AGENT_<id>_COMMAND`, `_CHECK`, `_HEALTH`, `_PROJECTS_CHANGED`, `_DEPENDS`, `_PERSIST` and configured provisioning paths |
| Credential tools | `SUBYARD_AGE_VERSION`, `SUBYARD_SOPS_VERSION` and their `SHA256_AMD64`/`SHA256_ARM64` pins |

A failed `integration enable` can be retried directly after its hook is repaired.
The retry obtains a fresh native plan and retains the completed release journal.
The protected integration inventory retains validated non-secret applied inputs.
A failed apply keeps the previous baseline; successful verification publishes the
new one. Before editing an older installation without that baseline, authoring
captures its independently verified current configuration. Start a stopped affected
yard first; an unverified or conflicting legacy installation must be reconciled
before its old settings can be adopted. Host-scoped writes check every affected
local yard, including whether its own override masks the change.
Changing `DEV_USER` on an installed yard is reconciled by plain `yard init`; no
release migration is required. The confirmed plan renames the verified existing
account and its private primary group, keeps their numeric UID and GID, and moves
the canonical home in place. Managed profile owners rebind their own account
references; user data and unrelated configuration remain intact. `DEV_UID` changes
are not account migrations. For a stopped yard, the same plan may temporarily start
it for inspection and restores its desired power afterwards.

The rename briefly stops the managed SSH listener and affected profile services.
Existing developer processes must exit first; init refuses to kill them. Systemd
lingering moves to the new login; while it is enabled, init refuses as long as the
developer's user manager runs services. Foreign accounts, homes, shared primary
groups, nested mounts, unsafe ownership and installed owners without rename support
refuse before the account changes.

If interrupted, retry plain `yard init` with the same desired configuration and
installed source; other mutations stay blocked until it completes. `teardown --full`
remains available for complete removal. Once the rename is committed, a later init
hook failure leaves the new account usable; rerun init for the remaining settings.

Changing `ENVIRONMENT_PROFILES` takes effect for project admission on the next
command. Retained project cleanup, yard stop and shell access remain available
when only the saved guest-facts selection awaits application. A valid current
facts snapshot with a different `selected_profiles` list does not make the release
unready. Run `yard init` to apply the selection and refresh the snapshot; saving a
setting does not write guest facts. Version, identity, integrity and incomplete
helper-refresh failures remain release readiness failures.

`yard config status [--all-local]` checks only materialized file settings in running local yards.
`yard config apply [--all-local]` refreshes those consumers after confirmation. Neither command is a
Git transport command. Configuration readers (`fields`, `show`, `paths`, `status`) remain available
while a release transition needs attention. If a completed, verified release needs only materialized
configuration repair, `config apply` can perform that bounded repair after its normal confirmation.
It rechecks the protected release state before writing and requires release readiness afterward.
Repair uses persisted configuration; differing command overrides are rejected before confirmation.
An unfinished or invalid release transition still requires `yard update`; config apply does not
perform unrelated activation work. When several yards drift, use `--all-local` to cover them all.

Use the typed persistent writers to save locally or in Git:

```sh
yard config set <SETTING> <VALUE> --scope shared|host|yard [--local | --git]
yard config unset <SETTING> --scope shared|host|yard [--local | --git]
yard config import <FILE_SETTING> <path> --scope shared|host|yard [--local | --git]
yard config edit <FILE_SETTING> --scope shared|host|yard [--local | --git]
```

Select a non-default yard with `-Y <yard>` before using `--scope yard`. Each writer validates the
catalog type and scope, rejects secret-looking content and asks before changing the persistent
configuration. `config edit` requires `VISUAL` or `EDITOR` to name one executable.

Persistent set/unset writers canonicalize every member of the selected legacy alias family,
including removal of all aliases on unset. They preserve unrelated records, comments and existing
permissions. Before consent they load the prospective scoped configuration through the normal
resolver, including every affected local registration for host/shared changes. Relevant source,
profile and asset dependencies are bound by content and filesystem identity and rechecked before
publication; changed inputs require a fresh assessment. Transient selection overrides cannot hide
invalid persistent profile or integration constraints.

`yard config validate [--all-local] [--json]` checks scoped schema without requiring a successful
initial default-yard load. It is read-only and does not claim materialized configuration or release
readiness. `config status` reports materialized assets only; use release assessment for release readiness.

A conflicting literal legacy/canonical source can prevent ordinary loading and release assessment.
Choose the desired value explicitly; verified installed or separately staged tooling exposes a bounded
source repair before those gates:

```sh
yard -Y default config repair-alias ENVIRONMENT_PROFILES '' --scope yard --local --check
yard -Y default config repair-alias ENVIRONMENT_PROFILES '' --scope yard --local --yes
```

Repair accepts one to four explicit canonical setting/value pairs in one local scope/file, limited to
`YARD_IMAGE`, `YARD_IMAGE_FALLBACK`, `ENVIRONMENT_PROFILES` and `CODING_TOOL_INTEGRATIONS`.
An actual change needs a related literal legacy assignment; canonical-only inputs permit exact desired
repetition. Dynamic assignments, remote/inventory selectors, arbitrary source paths, Git authoring,
identity/routing/sensitive settings and foreign shipped assets are excluded. A source checkout cannot
perform this ingress. Preview and diagnostics identify keys and scope without displaying values.
The repair validates all affected configuration, requires its own consent and preserves protected release
history and links. Its result is load-valid source, with a separate release assessment still required.
Supported unfinished activation-only history also requires a schema-2 capability probe of its exact
sealed original owner. That owner subsequently needs separate fresh consent for changed-input recovery;
staging newer tooling does not add capabilities to an old caller. Unsupported metadata or unresolved
intent requires its exact original owner or the reported supported bridge, without editing journals.

An optional activation hook can fail after migrations have completed. To reduce its persistent
selection while that activation remains unfinished, preview an explicit local source change:

```sh
yard -Y default config reduce-selection CODING_TOOL_INTEGRATIONS '' --scope yard --local --check
yard -Y default config reduce-selection CODING_TOOL_INTEGRATIONS '' --scope yard --local
yard migrate --check
yard migrate
```

This source-only operation requires its own consent and leaves the interrupted journal and
runtime links intact. It admits only a reduction of the current persistent optional list, with
all affected yards validated, a complete original migration ledger, no unfinished migration
steps and a supported capability probe of the exact sealed original owner. Temporary optional
selection overrides are rejected. Typed local selection `config set`, integration disable and
profile disable use the same boundary when activation blocks ordinary mutation. A subsequent
activation recovery needs a newly assessed plan and separate consent from its sealed owner.
Exact operation planning and remote/RPC source reduction are unavailable; run the explicit
owner-local preview above. Ordinary configuration apply retains its completed-history gate.

After a completed transition, local selection `config set`, integration disable and profile
disable can also reduce desired source while materialized configuration is stale. They use
ordinary configuration authoring with fresh consent, validated terminal history and sealed
assets; other activation resources must remain current. The write preserves release history,
links, file modes and unrelated settings, and does not run hooks or materialize an intermediate
inherited selection. Run a separately assessed `yard migrate` to apply the final desired state.
Temporary overrides and any effective selection expansion remain blocked at this boundary.

`SSH_PORT` is an owner-local listener setting. New Git authoring is rejected. Static literal
legacy Git assignments remain readable until explicit cleanup; dynamic legacy assignments are
blocked. The release settings migration pins each affected local yard's existing persistent
effective port, preserving winning local files and leaving external Git source and managed cache
untouched. Command overrides do not participate. Explicit config sync transactionally removes
legacy SSH assignments from its owned cache and adds local pins when removal would change a
port. Prepared Git push or selected Git authoring separately removes them from the selected
owner's source after consent, retaining empty tracked yard definitions, other owners and assets.
Existing port collisions, permissions, ownership and stale-input checks still apply. Policies
for other listeners remain with their declaring core or profile settings; listener classification
alone does not make a setting portable.

The default is local storage; `--local` makes that choice explicit and needs no Git source.
`--git` saves only the selected assignment or file in the registered source, creates one commit
with an automatic message that omits the value, and pushes the exact upstream immediately.
`config set` with `--git` accepts several `SETTING VALUE` pairs in one scope and saves them in
one commit and push, for example
`yard config set GIT_USER_NAME 'Jane Doe' GIT_USER_EMAIL jane@example.com --scope shared --git`.
Every pair is validated before planning, and each setting may appear once. Constraints between
settings, such as a yard template that forbids coding tools, apply to the combined result. It
does not publish unrelated local overrides. Local values still take precedence: an explicit empty
assignment is a value, while `config unset` removes the local assignment and reveals the Git
fallback. `config unset --git` removes the Git assignment and leaves any local value intact.
File settings have no `unset` command; removing a local file override requires an explicit file
removal. Secrets and runtime state never enter Git settings.

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

Saving or unsetting a canonical scalar replaces or removes its legacy assignments in the same
target file, preserving unrelated settings and comments. An existing conflict must be resolved
before commands can load that file; `config set` and `config unset` do not bypass validation.
If a conditional default in a preserved record would reintroduce the setting or its legacy alias,
authoring fails without changing the file; simplify that conditional assignment first.
For an existing literal conflict, use the bounded `config repair-alias` ingress described under
[Applying changes](#applying-changes), choosing the complete intended selection explicitly.
Then retry the scoped profile action and check its status to complete pending runtime work.

## Renaming an owner

The persisted `host-id` is authoritative after initialization. Changing the OS hostname does
not change this identity, SSH endpoints or runtime resource names.

`yard host rename NEW` previews the exact owner transition. With a registered Git source it
moves every tracked setting in `hosts/OLD/` to `hosts/NEW/`, preserving content and modes, and
includes a separate local Git commit in the same confirmation. It does not push. The source
must be clean, its applied commit current, Git identity configured, and the destination absent.
Local overrides, yard ports, credentials, projects and physical resources retain their identities.
A durable journal binds the source registration, HEAD, index, paths, manifest and local baseline;
interrupted execution resumes without overwriting unrelated edits or a foreign Git index lock.

To repair an earlier identity-only rename whose old subtree remains, explicitly select it:

```sh
yard host rename NEW --source-host-id OLD
```

This ingress validates the preserved source and local layers before loading the broken effective
configuration, and restores missing cache files. It never guesses another owner's subtree.
An exact completed recovery retry reports that settings are already migrated without another
commit or confirmation. Identity collisions with registered remote owners are refused.
Installed-runtime repair verifies sealed tooling, protected release metadata, ledger and links
again under the update lock. Unfinished legacy migration remains blocked. Unfinished forward V2
history admits only explicit source recovery when its exact sealed original owner supports the
existing activation recovery capability; release history and authority remain unchanged. Source
repair does not itself assert release readiness or execute the original release transition.

Connected controllers refresh trusted owner inventory on active `status`, `yards` and `list`
queries after the 30-second cache TTL. Thus an active query learns a same-key rename after cache
expiry, plus its bounded SSH request time; an idle or offline controller has no wall-clock
convergence guarantee. Offline queries report stale last-known data and reconnect queries retry
adoption automatically. Operator SSH aliases, Destination and ProxyJump stay unchanged. Older
controllers lacking authenticated adoption retain stale data or refuse the mismatch until updated;
the owner inventory protocol remains unchanged. Changed pinned keys require explicit repair.

## Versioned private configuration

Keep private desired settings in a separate clean Git checkout. Do not turn the entire
`$SUBYARD_CONFIG_HOME` into a checkout: it also contains project state, credential records, generated
consumers and support tools. `.gitignore`, a symlink farm or recursive `rsync --delete` is not an
ownership boundary.

Git is optional. `config set`, `config import` and `config edit` save locally by default, and
`yard provision <profile>` can add a profile locally whether or not a source is registered.
Use `--git` on a typed writer to save only that setting in Git with immediate commit and push.

Sync stores Git-derived scalar and file settings under `$SUBYARD_CONFIG_HOME/.sync/settings/`,
using their normal relative paths. The cache provides offline fallback; it never replaces persistent
local overrides. Local shared/host/yard settings take precedence over all Git settings. Absence
means fallback, while an explicit empty local assignment remains effective.

The next sync migrates an older whole-file import: files matching the recorded digest move to the
Git cache, and changed files remain local to preserve operator edits. Before changing a legacy
managed local file, complete this migration with `yard config sync` for a registered source, or
`yard config sync <checkout>` for an existing committed checkout imported directly. Restore that
checkout first if it is unavailable. Sync prints its plan and requires confirmation; `--yes`
provides explicit automation consent. Neither `--apply` nor a network pull is needed to separate
the storage roles. Test-yard recreation guidance includes this prerequisite only while the old
Git ownership remains. Runtime rollback verifies
that the retained release supports the installed Git cache before planning or executing the rollback.

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
`hosts/<HostID>/yards/<yard>/` entry for a non-default yard still requires `config.env`, because
the directory declares a yard that needs an unambiguous scalar definition. Scalar assignments must be syncable in their
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
yard config sync push --apply
```

`sync status` fetches by default and reports registration, sanitized remote, branch/upstream,
checkout HEAD, dirty/conflict counts, ahead/behind/diverged relation, last fetch, applied commit,
generation and recovery state. `--offline` uses cached refs. A fetch/auth failure still prints the
available local diagnostics and exits non-zero. Automation is manual.

`sync pull` permits only a clean fast-forward of the exact upstream, validates the candidate before
one confirmation, then refreshes the Git cache transactionally. `sync push` transports already
committed checkout changes to the exact upstream without force; it never exports local overrides
or creates a commit. Its optional `-m`/`--message` argument remains accepted for compatibility.
Use typed writers with `--git` to create and push a setting change. Dirty, conflicted, detached,
upstream-less or diverged checkouts fail closed; Subyard does not stash, merge, rebase, reset or
resolve conflicts.

`sync pull`, `sync push` and typed writers with `--git` fetch the upstream once to plan. A change
fetches it once more after confirmation, before its first write, and fails if the upstream moved;
the push itself never forces. A no-op makes no further remote request. Over SSH, one command
shares one OpenSSH connection per remote host across these requests, so a key without an agent
asks for its passphrase at most once, however long the confirmation waits. Git's first connection
authenticates with your usual OpenSSH configuration. Its master socket lives in a private
temporary directory and is closed when the command ends, including when the process is killed.
Only if both the command and its cleanup helper are killed with SIGKILL, or the connection's
process was never verified as the command's own, can the connection outlive the command; stop it with
`ssh -O exit -o ControlPath=<socket> <host>` or by ending its `ssh: <socket> [mux]` process.
Unless you set `ServerAliveInterval` yourself, the shared connection uses
`ServerAliveInterval=15`, so a connection dropped during the confirmation ends instead of hanging;
the next request then opens a new connection and asks for the passphrase again.
The decision uses the remote URLs after Git's `insteadOf` and `pushInsteadOf` rewriting. Subyard
leaves Git's SSH selection unchanged when `GIT_SSH_COMMAND`, `GIT_SSH`, `GIT_SSH_VARIANT`,
`core.sshCommand`, `ssh.variant`, or the remote's `uploadpack` or `receivepack` is set; when `ssh -G`, given the remote command Git runs
(`git-upload-pack`, and `git-receive-pack` for a push), reports your own `ControlMaster` or
`ControlPath`, or, for endpoints that resolve to the same control socket (for example two `Host`
aliases of one host, port and user), configurations that differ, such as a fetch-only deploy key and
a push key; or when no short private socket path is available. HTTPS and file remotes are unaffected.

`ssh -G` reports an explicit `ControlMaster no` or `ControlPath none` the same way as the default,
so such a setting does not prevent sharing. While the command runs, including during its
confirmation, other processes of the same user can open sessions through its master socket without
the key. To opt out, run the command with `GIT_SSH_COMMAND=ssh`, or set `core.sshCommand` in the
registered checkout, for example `git -C <checkout> config core.sshCommand ssh`.

Native `sync pull` and `sync push` remove group/world write permissions from the registered
checkout before importing it. Permission repair is included in their confirmed plan even when
the committed settings and live configuration already match. Declining leaves the checkout
unchanged.

Git does not store these write permissions. An external Git checkout or merge can recreate files
or directories with unsafe modes. After publishing source edits upstream, use `config sync pull`
to import them through the protected workflow. If bare `config sync` or `--check` reports
`source file is group/world writable` or `source directory is group/world writable` for a clean
registered checkout, use `config sync pull` to restore safe permissions and import the source.
The candidate must still pass source validation and unmanaged-target adoption checks before
permission repair can be confirmed. The read-only commands never repair permissions silently.

`--check` is read-only, never prompts, and exits non-zero when an apply or local manifest update is
needed. A changing sync prints the source commit and exact redacted managed-path plan, then asks once.
`--apply` composes the import with `yard config apply` for affected running local yards under the
same top-level confirmation. It does not run `yard init`, start, stop, teardown, project operations
or remote fan-out. Remaining follow-up commands are printed by application mode.

An existing unmanaged Git-cache target requires a reviewed first import with `--adopt`. Later cache
drift is restored only through the confirmed exact plan. A path removed from Git is deleted only
when the manifest owned its previous exact digest; local overrides remain untouched. Removing a
yard definition fails while its Incus yard or project state still exists; sync never becomes
teardown. Removing the selected host subtree removes only its previously managed cache paths,
subject to the same digest, drift and in-use guards.

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

### Enabling a profile with a registered source

`yard provision <profile>` adds a missing profile to the selected yard's local settings, preserving
its existing effective selections, and provisions it under one confirmation. This works with or
without a registered source. An already selected profile can also be provisioned directly.

To save the selection in Git explicitly, inspect the current list and preserve all entries:

```sh
yard -Y default config show ENVIRONMENT_PROFILES
yard -Y default config set ENVIRONMENT_PROFILES "<current entries> android" --scope yard --git
yard -Y default provision android
```

The Git save commits and pushes immediately. An existing local `ENVIRONMENT_PROFILES` assignment
still masks the Git value; remove it with `yard -Y default config unset ENVIRONMENT_PROFILES
--scope yard` when you want to use the fallback. `--apply` refreshes materialized file settings and
is not required for scalar profile selection. Remote writes and provisioning run on the selected
owner host.

Retired shipped integrations are normalized only by their compatibility declaration. Old
`aiobserver` selection entries and `AI_OBSERVER_HOST_PORT` assignments are ignored without
rewriting their source files; other choices and explicit empty selections retain their authority.
The OpenLIT profile is disabled. Historical `ENVIRONMENT_PROFILES` entries remain authored
but are omitted from the effective selection; enable requests are refused by profile policy.
Normal reconciliation removes its managed telemetry and dashboard publication while preserving
existing data. See [OpenLIT](openlit.md) and [Observer retirement](openlit.md#predecessor-retirement).

## Full uninstall

`yard teardown` and its `uninstall` alias offer a scope menu in an interactive terminal.
The default removes only the selected yard. The second option removes this installation and
all its local yards on the owner host. Scope selection precedes the read-only inventory and
one final `Proceed? [y/N]` confirmation. `--full` selects the full scope explicitly;
`--yes` and `ASSUME_YES=1` never select it. Remote selectors cannot perform full removal.

Full uninstall is available without running or completing migrations, for every source version.
Pending or invalid V1/V2 migration metadata does not gate full removal. Its own ownership,
cleanup inventory, session, stale-plan and confirmation checks still apply. Selected-yard
teardown retains its ordinary release admission.

```sh
yard teardown --full
```

Full removal includes local settings, owned encrypted credential ledgers and identities,
SSH transport state, caches, logs, runtime releases, launchers, matching host services, and
installation-created host namespaces containing secrets, agent sessions and backups. Their
loss is irreversible. `--full --keep-data` is invalid; `--reset-config` is redundant with
`--full` and remains accepted. Ordinary teardown retains its selected-yard behavior.

The exact plan lists owned targets and preservation boundaries. Foreign Incus consumers retain
shared pools and backing data. Bound host projects, user Git repositories, unrecorded contents
inside custom roots, other installations, shared packages, account lingering and unrelated shell,
SSH and user-service state remain preserved. PATH entries serving other commands remain usable.
Unknown ownership or changed native identities cause a nonzero refusal; a captured directory
name alone does not establish ownership of all its contents.

Legacy installations may lack creation receipts for host directories. Exact native association
can establish a registered legacy yard's bounded project/instance/volume scope; unrelated native
resources or conflicting ownership refuse that scope. Unrecorded legacy host namespaces, including
backups, sessions and host mounts, and shared infrastructure without installation ownership remain
preserved and are listed in the plan and final result. Full removal does not claim those paths
were deleted. Modern receipt-owned namespaces retain their existing deletion and stale-plan guards.


After consent, full removal saves the exact plan and a private recovery runtime beside the data
root at `<data-root>.uninstall`. Producers stop synchronously before their data; yard resources
and data precede runtime removal. If interrupted, retain that directory and run the exact recovery
command printed by the failed operation. Its entrypoint remains usable after config/runtime loss:

```sh
/exact/data-root.uninstall/runtime/bin/yard _uninstall-resume /exact/data-root.uninstall
```

Retry uses the saved owner identity and approved paths, with a fresh confirmation. An ordinary
local `teardown --full` also detects this saved plan when its original data root is selected.
The recovery directory disappears only after verified cleanup. Stable, secret-free account
coordination locks and atomic nonsecret epoch files under `/tmp/subyard-owner-locks-<uid>` and the shared host-network lock under
`/run/lock/subyard-network` remain: removing an active lock inode would let queued operations
bypass serialization. Queued mutations and late audit/cache writers recheck their original
installation generation before writing.

### Adopting an existing yard

After completing the supported release transition, run `yard init --adopt`
through the installed local owner launcher to register an existing Subyard yard
for normal management. This is an explicit lifecycle operation, separate from
release migration. Review its single operation plan, which includes registration and ordinary
initialization; one confirmation authorizes both. `--yes` accepts this operation
only when `--adopt` is explicitly selected.

Adoption verifies protected persisted configuration, owner HostID, installed
source and completed release history, native instance UUID, dedicated project
inventory and the instance configuration keys of configured capabilities; ordinary
init validates capability devices and host sources.
Existing local devices are taken under management as they are, whatever their name, type or
options; ordinary init owns any later convergence. Device refusals remain for a missing or
different `srv` device, because reattaching it would change persistent data, the
`ALLOWS_HOST_ACCESS=false` boundary, a selected loopback or Tailscale resource device that fails
its recipe, a foreign or inherited resource receipt, a project root or NIC with a different type,
path, pool or network, and local and expanded devices that disagree. Selected owner-IPv4 routes,
their pending receipts and startup intent, and other project root and NIC properties enter as
found. Unknown workloads, snapshots,
uninventoried enabled namespaces, temporary configuration overrides and partial or foreign
ownership are refused. The existing project policy must already be converged. A refusal lists
every problem the assessment can determine.

Existing project bind devices are selected when the protected local registry
ties their exact generated name, source, destination and options to the selected
yard. Adoption binds those records before consent; other `ws-` devices are retained
without establishing project ownership, and an unsafe registry still refuses. Physical-memory
telemetry devices and markers are accepted in any state, as are volatile keys of existing local
devices and preview route markers. The registration substep leaves telemetry unchanged; the
same command then reconciles it through the ordinary init stages, which refuse what they cannot
converge.

The operation first atomically publishes complete project registration with
adopted provenance, preserving instance identity, disks, workloads and desired
power state during that substep. It then executes the ordinary init stages
already captured in the same plan, including their disclosed workload effects,
and restores desired power. No follow-up command is required. If initialization
fails after registration, the registration remains valid; rerun the same
`yard init --adopt` to assess and finish the remaining work. Registration creates
no resource-creation receipts: full removal
preserves shared infrastructure and unrecorded host namespaces whose creation
ownership was not proven. Its versioned ownership marker makes older releases
refuse the adopted project rather than infer creation ownership on rollback.

An existing narrow continuation record is retired only after native publication
is verified. If interrupted between publication and retirement, rerun
`yard init --adopt` to verify the same identity and retire the exact old record.
`--adopt` cannot be combined with `--continue-legacy`, `--configs`, `--reset`
or `--profile`.

### Continuing an existing legacy yard

After upgrading through the supported release transition, a legacy project may
lack installation creation ownership. Use the installed local launcher to run
`yard init --continue-legacy` once. The read-only plan requires protected owner
configuration, completed verified release history, an existing native instance
UUID and the already-converged dedicated project policy. Review the exact plan;
`--yes` accepts it only when `--continue-legacy` is explicitly selected.
An implicit default yard may use its existing protected mode0600 host
configuration when the local yard record is absent. Assessment binds that file
and the absence of a yard record, then rechecks both after confirmation.

The private continuation record permits normal reconciliation of that same
owner and instance. Later plain `yard init` reuses it after checking the identity.
The grant also binds the existing project, default-profile and instance security
policy, including local devices, ID mappings, mount sources and permissions.
Continuation supports the canonical non-nested unprivileged container setup;
unknown instance configuration, local NIC overrides and additional devices are
refused. Exact managed AppArmor compatibility and static-preview publication
changes remain allowed, alongside normal power and runtime state changes.
Shipped owners may declare a paired disabled-backend publication contract.
Its schema and complete protected settings remain bound; only a validated
release digest and an explicitly compatible older serialization may differ.
An older disabled backend's missing budget fields do not prove historical
budget enforcement. For legacy continuation admission, a declared source identity
must equal the protected yard identity, even when explicitly supplied. This
constraint does not change the backend's other supported source forms.
Unknown permissions, unsupported profile configuration and later policy changes
are refused, including on a new command or a repeated opt-in. Any pending native
network-policy changes require their separate assessed operation before granting
continuation. Runtime power, observed addresses and release links are not pinned.

It grants no reset, project creation or full removal ownership; full removal
refuses while this continuation-only authority remains. It cannot be combined
with `--configs`, `--reset` or `--profile`. Unknown resources, conflicting or
partial ownership, unsafe records and changed identities are refused without
adoption. Configuration roots may be customized, but temporary scalar overrides
must not redirect the persisted owner context. Do not add installation markers
manually. Supported release rollback and roll-forward retain this narrow grant.

### Disposable profile caches

`CACHES` retains its project-environment sharing contract. A profile may opt a
safe subset into `yard cleanup --soft` with `CLEANUP_CACHES`, a space-separated
list of absolute directories referencing the same configured cache variables.
Paths must be below `/srv/cache` or a private developer-home subdirectory;
project, credential and runtime-state paths are rejected. Declare only data
that can safely be regenerated during an active build. Installed Go toolchains,
SDKs and browser runtimes must not be included, even when stored below a cache
root. Cleanup does not acquire cache-writer locks; affected builds may need a retry.
