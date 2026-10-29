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
SSH_PORT=2223
YARD_TEMPLATE=test-vms
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
Dependencies are computed separately: enabling Paseo also makes Codex effective;
disabling Codex while Paseo still requests it is rejected. Status reports the requested
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
`yard integration status` assesses this same initial adoption without changing files or
creating inventory. Matching legacy artifacts appear as pending work; a conflict identifies
the first artifact or prerequisite that prevents adoption.
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

To inspect recovery, run `yard -Y default integration cleanup <name> --check`. Run the same command
without `--check` to review and confirm its consequences. Cleanup refuses integrations that are
still selected, including dependencies of selected integrations. Hooks must keep cleanup bounded
and reversible, preserve user data, and support safe retries after interruption.

On the local owner CLI, a completed release whose sole readiness blocker is the matching
integration ownership conflict can admit this explicit cleanup after verifying the sealed
owner, protected history and other activation contracts. The confirmed native cleanup plan
is rechecked under the protected release lock. It preserves selection and history; remaining
materialized configuration drift still needs a separately authorized migration. Unfinished
release history and unrelated blockers continue to require their original recovery path.
This recovery admission is unavailable through RPC operation planning.

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
stable official release and verifies the downloaded artifact against that release's SHA-256
metadata before replacing the managed binary. A newer working installation is preserved.
Legacy `CODEX_VERSION` and `CODEX_SHA256_*` assignments are ignored when loading existing
settings; new writes to these retired fields are rejected. Already-converged yards do not check for upstream updates
on every `yard init`, and `yard status` does not install or update agents.

### Codex permissions across projects

The default yard configuration permits local edits, builds, tests and network access without
approval. Managed rules classify canonical direct `git commit` (including `--amend`) and
`git push` as requiring user approval across projects. Repeated single-use approval also depends
on the client's response behavior:

| Client | Yard mode | Approval evidence |
| --- | --- | --- |
| Terminal | Run `codex` without permission overrides. | The owning VM scenario checks local work, denial and repeated commit/amend/push prompts. |
| VS Code Codex extension | Select **Custom (config.toml)**. | Loads the yard defaults; client-specific denial and repeated single-use approval still need acceptance. |
| Orca terminal | Use **Manual** with empty Codex arguments; Subyard also removes the stock YOLO argument. | The owning VM scenario exercises the paired terminal, including denial and repeated Git prompts. |
| Orca built-in Codex chat | Use **Manual**; the Subyard adapter reads the effective Codex configuration for the chat directory. | The managed relay exposes only single-use choices and cancels session or rule-amendment responses. The owning fixture exercises the stock native-chat approval RPC and reconnect; the graphical Desktop UI is not separately certified. |
| Paseo local SDK | Mode `auto` (**Default Permissions**) with explicit `on-request` and `danger-full-access`. | Native daemon checks cover denial, one approved commit, a new prompt for the repeat, and pending approval after reconnect. |
| Paseo Desktop | No fully verified Desktop selection is published. | The preset alone uses `workspace-write`; Desktop delivery of the SDK overrides needs acceptance. **Auto-review** conflicts with the user-only requirements. |

The terminal and Orca checks belong to the profile's
[`codex-permissions` VM scenario](test-vms.md).
Its coverage does not certify every installed client version or active session. In particular,
the Orca terminal and built-in chat are separate approval surfaces.

Codex provisioning installs root-owned `/etc/codex/requirements.toml` using Codex's native
[managed requirements](https://learn.chatgpt.com/docs/enterprise/managed-configuration).
It requires `on-request`, the `user` reviewer and commit/push prompt rules independently of the
home rules. Project settings, profiles, alternate `CODEX_HOME` and client flags cannot remove
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

### Other agent permission modes

For Claude, OpenCode and pi, status states the missing active-client evidence without launching
sessions. Installation checks do not certify an interactive approval UI:

| Backend | Shipped yard setting | Limit of the evidence |
| --- | --- | --- |
| Claude Code | `permissions.defaultMode = "bypassPermissions"`, with Bash allow and mandatory native managed commit/push ask rules. | Native SDK checks cover bypass/default, repeated Git approval, session allow updates, local/project allows and preservation of user/project denies. A native terminal fixture with explicit ask settings exposes only Yes/No and repeats the prompt. Actual IDE clients remain unverified; Subyard publishes no single-use guarantee for them. |
| OpenCode V1 | Global allow, with commit/push ask patterns under `permission.bash`. | **Unsupported for the strict single-use contract.** Native `once` prompts again; `always` permits a later matching command without another prompt. Project/agent/UI overrides can weaken the defaults. Direct launch remains available. |
| pi | `defaultProjectTrust = "always"`. | Project trust is not a per-command approval gate; Subyard supplies no verified outer gate. |

Claude provisioning installs its own root-owned drop-in at
`/etc/claude-code/managed-settings.d/50-subyard-git-approval.json`. It adds only ask rules;
it preserves other managed files and user/project denies, and refuses foreign files at its own
managed path. It does not enable `allowManagedPermissionRulesOnly`, which would discard those
user/project restrictions. `claude-policy-check` verifies installation and permissions only.
In a new client, inspect `/status` for the managed file/drop-ins source and `/permissions` for
the effective rules. A higher-priority enterprise policy or a CLI without native drop-in support
needs its own native acceptance; file presence does not prove that the client loaded it.
Use `claude --permission-mode default` and one-time approval when inspecting native terminal behavior; the yard's
existing bypass default is also covered by the SDK fixture. A native CLI terminal check using
`--permission-mode default --settings <fixture>` offered only Yes/No, approved two separate
commits only after two Yes responses, and retained the original HEAD after two No responses.
This terminal check used explicit ask settings; the installed managed-drop-in precedence and
session-update checks use the SDK. No actual Claude IDE selection has completed client
acceptance, including when launched from Orca, so Subyard publishes no single-use guarantee
for those clients.

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
with an automatic message that omits the value, and pushes the exact upstream immediately. It
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
managed local file, complete this migration with `yard config sync`. Runtime rollback verifies
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
