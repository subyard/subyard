# Control-plane architecture

Subyard's production entrypoint and control plane are a native Go engine. Bash is limited to narrow
physical adapters for platform mutations. Each operation has one path: Go
owns workflow and validated context, side effects stay behind explicit ports, and a migration slice
deletes the replaced shell path without growing production code.

## Implementation map

```text
bin/yard                                source-tree and release-runtime launcher
.build/yard                             ignored source-development engine
<runtime>/current/bin/yard-engine       verified amd64/arm64 production engine
cmd/yard                                native CLI/RPC entrypoint
internal/
  ├── command, config, domain           manifest and immutable context
  ├── cli/prepared_command.go           shared core-command prepare/execute ownership
  ├── application, credential           routing/reconciliation and credential DAG policy
  ├── ownerapi                          typed owner queries and session preparation
  ├── state, migration, rpc              atomic state, schema checks and framed sessions
  └── adapters/                          Incus, release, metadata and local/SSH transports
scripts/
  ├── NN-*.sh                           host/platform mutation leaves
  ├── lib/                              shared platform adapter contracts
  └── e2e-lab/                          opt-in nested-VM physical backend
config/profiles/<profile>/
  ├── provision.sh                      optional in-yard toolchain
  └── resources/<resource>/             profile-owned lifecycle mechanics
```

The Go engine owns global yard selection, validated config, operation identity/audit, remote-plane
selection, project state/resolution, read-only status/inventory, credential DAG decisions, official
Incus calls and the versioned stdio RPC. The source launcher executes only an explicit `.build/yard`
development candidate. Installed commands use an immutable, checksum/provenance-verified runtime
containing its launcher, engine, scripts, registry and completion files; `current`/`previous` switch
the whole runtime and production never reads a source checkout. Non-interactive mutations share the
Go-owned plan, consequences, confirmation, operation ID, audit, events and cancellation path across
CLI and RPC. Go owns reconciliation order, retries and transactions. A shell leaf may probe or
mutate one physical boundary; it does not select stages, route operations or make policy decisions.
Go owns release selection, download and CLI/RPC planning. The installer only verifies and activates
a prepared bundle. A separate first-install bootstrap is excluded from release runtimes.

Materialized coding-agent assets share resolution and safe source reads in
`internal/config/materialized.go`. `internal/adapters/configmaterial` implements the guest JSON/TOML
apply/observe boundary with an embedded Python program, using the base guest Python installation
and an embedded TOML writer. The Go callers send desired content through typed stdin and consume only convergence
and fingerprints. Runtime provisioning, config refresh/status and release activation use the same
field ownership baseline; other consumers retain byte-exact behavior. See
[File settings](configuration.md#file-settings) for the ownership and interrupted-write contract.

Every guest config file write is a compare-and-write over a consent token: `absent`, or `sha256:`
and the digest of the bytes the refresh may replace; a destination that is not a readable regular
file has no token and always refuses. In one guest exec the writer walks the parent with
`O_NOFOLLOW`, checks the destination's hash and stages a temporary file holding only its own
payload. For `absent` it publishes the file with `link`, so an existing destination refuses as
`changed`; otherwise it re-checks the destination's inode and hash and renames over it. The window
between that last re-check and the rename is the accepted residual: a tool write inside it can be
replaced. A crash leaves at most its own staged copy beside the destination, never guest bytes.
Missing parent directories are created only after the write is decided, and a converged file only
gets its owner and mode back. The skills refresh is excluded from this contract; it replaces
Subyard-owned skill trees.

Each guest config file is observed once per assessment (`configmaterial.Observe`): raw files
together in one exec, each structured file through its own observation, which also reports the
token of its exact bytes. The file's one state is `converged`, `drift`, `unreadable`,
`unowned-invalid` (a structured document that is not valid and that Subyard does not own),
`unassessed` (a completed guest observation that assessed nothing, such as one over a broken
Subyard baseline) or, after a refused write, `changed`; guest text never selects or fills a state.
`configmaterial.Refusals` renders the last four with their next step: replace or remove an
unreadable file, correct or remove an unowned invalid one, or just rerun for an unassessed or a
changed one. The step runs `config apply`. `init`, `config status`, `config apply` and release
readiness use this observation and these refusals.

`config apply` takes the tokens it shows at confirmation and its writes compare with exactly those
tokens. A file the confirmation did not cover is written over the writer's own observation made
right before it writes. Each refused file is reported, the yard's other files are written, and the
inventory commit runs only when nothing was refused. The plan rechecks the target set, desired
configuration and instance identity; drift itself may change, because each write rechecks its own
file. An `init --configs` child receives the tokens through `--config-consent-fd N`, an inherited
file holding JSON; a release without that option rejects it before any write. Every other refresh
plans for itself and observes the tokens immediately before writing: a direct `init` or
`init --configs`, an init of a stopped or uninitialized yard, an integration apply and the follow-up
after an update. A release activation repair writes over the tokens its authorized plan read. A yard
with a refused file, or one that cannot be assessed, is refused at plan time; every other yard of the
plan is still refreshed, and the plan lists the refused yard instead of reporting no drift.

Only the writer renders a refused file's step. An `init --configs` child of `config apply` (also
under `config sync --apply`) reports the files it refused on an unlinked inherited file named by
`SUBYARD_CONFIG_REFUSALS_FD`, which an older release ignores: per file only its destination and
state, never text. The parent renders those refusals for
its own yard exactly as an in-process refresh does, in place of the child's exit status. A missing,
empty, oversized or malformed report, or a state outside that vocabulary, relays nothing, and the
next step reruns the child: `init --configs`.

Integration selection uses the shared prepared-command boundary, the protected per-yard config
writer and `reconcileruntime.IntegrationPlan` / `ApplyIntegrations`. The same runtime path is used
by full initialization after core substrate provisioning. `scripts/reconcile-integrations.sh` is a
bounded package/proxy/project-hook leaf. A per-yard lock serializes confirmed desired publication
and reconciliation. Guest ownership evidence covers structured fields, plain files, derived
session links and known service receipts; evidence conflicts preserve artifacts.
See [per-yard selection](configuration.md#per-yard-coding-tool-selection) for user-visible semantics.

The project-hook pass verdict comes from its exit status. With approved paths, the
wrapper opens fd 3 for advisory hook labels. Core filters labels against the approved
HookPlan, deduplicates them and renders them in stable order. Hooks are root-owned,
digest-pinned operator code running as the same UID as the dispatcher, so core makes
no claim about who wrote a record. Diagnostics contain only fixed text, the yard name
and approved paths. Hook stdout and stderr never enter the diagnostic. Older installed
dispatchers receive the generic diagnostic until initialization or release activation
updates them through the dispatcher content-digest contract.

Explicit initialization also binds the selected yard's authoritative project records and
guest metadata observations before approval. Its generic metadata step atomically restores
missing canonical metadata before the installed project hooks run. Matching legacy metadata,
including absent `yard`, `identityVersion`, `target` and `importedAt`, is preserved. It also
repairs captured developer-owned root group/other write permissions, preserving all other mode
bits, and narrows otherwise valid metadata with group/other write permissions to `0600` without
rewriting it. Present stale `yard`, `identityVersion`, `target` and `importedAt` values are
atomically rewritten from the owner record. Repairs appear in the operation plan without a warning or
additional prompt. Missing roots, unsafe ownership or paths, hard-linked, non-regular,
oversized or invalid metadata, conflicting `schema`, `projectId`, `name` or `mode`, and stale
observations stop initialization; guest directories never authorize additional projects.
Inspection reports every refusing project together in ProjectID order, with the path and
specific corrective action. Apply still requires the exact expected snapshot of each captured
root, `src` and metadata file; directories bind identity, not size or timestamps.
Legacy bind wrappers and sources may remain root-owned with the developer's group
and without world-write access. Only captured missing or stale-content metadata in a non-writable
legacy bind wrapper uses privileged publication; the new file belongs to the developer
with mode `0600`. Wrapper and source ownership and permissions remain unchanged.
Read-only project inventory and status do not perform this repair.
When missing or rewritten metadata changes a profile's native hook catalog, its runtime observer
can consume an optional stdin object with `schema: 1` and `missingMetadata` entries
containing canonical `root` and metadata `payload` for missing or stale-content files.
This is a read-only projection:
native repos, groups, registration state and source facts remain actual observations.
Init captures the projected hook binding before approval, rechecks the original
binding before publication, and requires normal readback to match the projected
binding afterward. This preserves catalog guards on the first repair. A profile that
does not support the projection fails verification if publication changes its binding.
The Orca observer uses captured shipped sources even while installed helpers await repair.

`AGENT_<name>_CHECK` is a package-owned installation/convergence probe; release activation
may require it to succeed. Long-running service initialization belongs outside that gate.
A configuration-only `PROVISION` hook may omit `COMMAND` when it declares a safe mandatory
`CHECK`; the check then supplies convergence without requiring a CLI installation. A hook
with a nonempty `COMMAND` still requires a valid executable name. These rules are shared by
init, prepared integration changes and activation; shell output describes integration readiness.
Optional `AGENT_<name>_HEALTH` probes report advisory service health for `yard status` and
`yard integration status`: one JSON object with only `state`, one of `ready`, `starting`,
`failed`, or `unknown`. Queries bound execution to five seconds and accept at most 1024 bytes;
an unavailable, invalid, or timed-out probe reports `unknown`. Health never enters a release
plan or its fingerprint. Installation can be converged while a service is still starting.

Profiles may declare `unmanaged_integrations` observations with an integration ID,
a literal guest command array and a fixed public detail. Status runs these bounded,
read-only probes only for selected profiles and unselected integrations, appending
the developer home and UID as arguments. Only the closed `{"state":"unmanaged"}`
result publishes that informational state; absent, invalid or failed observations
publish nothing. These probes cannot select software, block activation or produce
warnings. Profile packages own detection and its tests.

Optional shipped `config/agents/*/integration.json` declarations extend the same
package loader and operation-local catalog with immutable inputs, typed settings,
existing agent metadata, native lifecycle ownership and dashboard metadata. Core
uses the existing exact integration facts and shared bounded hook executor;
integration packages own provisioning, retirement and route receipts. See the
[shipped integration contract](reference/shipped-integration-contract.md).

Semantic profile/catalog, scalar-layer and resource contracts accept metadata or readers before
physical binding. See [functional declaration boundaries](reference/functional-boundaries.md)
for consumer APIs, adapter responsibilities and regression coverage.

## Stable interfaces

### Commands

`config/commands.registry` is pipe-delimited:

```text
name|aliases|handler|arg0|remote|effect|confirmation|visibility|section|completion|display|summary|options|verbs
```

- `remote` is `local`, `forward`, or `deny`.
- `effect` is conservatively `read` or `mutate`; a mixed command is `mutate`.
- `confirmation` is a manifest marker: `never` or `dynamic`. Missing and unknown values fail closed.
  Typed action assessment resolves the concrete policy to `never`, `prompt-default-yes`, or
  `prompt-default-no`. Launch/session actions such as `code`, `shell`, and `start` do not prompt.
  `--yes` supplies confirmation consent; it does not bypass preconditions, stale checks, or guards.

- `handler` is a script under `scripts/`, or a reserved dispatcher adapter such as `@help`/`@rpc`.
- `completion` names a provider consumed by both Bash and Zsh completion; `options` and `verbs`
  carry their shared token lists.
- Public dispatch, aliases, `yard --list`, top-level help, and completion metadata all use this
  registry. `yard --command-manifest` exposes the validated machine-readable rows.

The prepared native Incus installer grants the approved operator `incus-admin` membership and
a named access ACL on the current default root-owned socket. Trusted standalone adapters without
a captured dispatcher retain group membership and require a fresh group session. The ACL preserves socket ownership, mode,
the existing mask and unrelated entries, and lets the original approved parent continue without
restarting or preparing another action. Custom endpoints fail closed. No directory default ACL
or service hook persists the grant: replacing the socket removes it. Removing group membership
alone does not revoke a surviving named socket ACL; remove that operator's entry as well.
The approved network stage grants the same captured UID read/write access to the fixed native
host policy lock. Native setup captures the kernel UID and passes its resolved account name to
both elevated adapters, even when ambient sudo or user names describe another caller. It rejects
any supplied operator name that differs from that caller. Non-root Incus readiness also checks
both fixed native ACL tools: missing tools are installed by the approved Incus stage even when
the server is already running; unsafe existing tools fail closed.
This inode-bound ACL preserves ownership, mode, the mask and unrelated access;
removing membership requires revoking this entry too. Recreating the lock or rebooting removes
the grant. Active UFW rule files keep their existing
group-based permissions. When the original parent lacks that group in its kernel credentials,
post-apply verification alone can read them through its already authorized, noninteractive sudo
context, after its own policy-lock check succeeds. Planning never uses that elevation.
NetworkManager planning reads its effective configuration directly and cannot use cached sudo
credentials. Approved lifecycle readers retain their explicitly authorized noninteractive readback.

Profile resource commands use the separate `.res` interface below because profiles own those
commands and mechanics.

Bash and Zsh completion obtain yard and project candidates exclusively from the read-only
`yard list --complete-yards`, `yard list --complete-projects` and
`yard list --complete-profiles` providers. They preserve successful
newline-delimited records. An unavailable, failing or empty provider yields only `default` for
yards and no dynamic project candidates. Completion never reads yard registrations, project state,
HostID or configuration files. Profile completion resolves shipped packages before owner,
network or power observations, including descriptorless and resource-only packages.

The installer configures both shells' startup files, including Zsh's `ZDOTDIR` when set; explicit
`YARD_SHELL_RC` and `YARD_LOGIN_RC` overrides limit installation to the selected startup files.
Interactive completion initialization binds Tab to forward menu completion and Shift-Tab to
backward menu completion in Emacs and Vi insert modes, without changing the selected editing mode
or Enter. Ambiguous matches are listed while cycling. These bindings apply throughout the shell
and are safe to initialize repeatedly.
New sessions use the stable runtime `current` path, so upgrades load the matching completion files.

Core commands share one preparation and execution pipeline in `internal/cli/prepared_command.go`:

```text
CLI / operation.plan → manifest → resolveCoreCommand → prepareCommand
                                                        ↓
                                                preparedCommand
                                                        ↓
                         CLI prompt / operation.execute confirmation
                                                        ↓
                                    Execute → stale check → reserve → apply → commit
                                                        ↓
                                                      Close
```

Before context loading, `internal/cli/admission.go` classifies each CLI invocation once: teardown
pre-admission, the manifest effect refined by verb-level readers, early dispatch and the
release-gate policy. `Run` keeps developer-identity admission, the pending hard-cleanup fence and
init migrations. A non-ready gate admits by the selected yard
([yard-scoped release gate](#yard-scoped-release-gate)); the admission is one value the invocation
installs before dispatch. `operation.plan` and `operation.execute` share one operation admission in
the same file: hard-cleanup and developer-identity recovery, then the same release gate.

The handler-family resolver binds preparation and physical leaves in one place. Startup rejects
unknown internal handlers. A prepared command owns canonical arguments, resolved context, the
operation plan and one execution closure capturing its typed state. Plans with `confirmation:never`
are already confirmed; prompt policies remain unconfirmed until consent. Its idempotent
cleanup releases an outstanding project reservation and any retained release handle. CLI and RPC
share assessment, execution and successful project-state commit; CLI owns human previews and
prompts, while RPC owns bounded session storage, events and protocol errors. Dedicated query,
terminal, configuration and credential workflows retain explicit resolver classifications.

Remote project execution holds a shared controller lock for its registered owner from
post-confirmation route validation through physical work and state commit or abort. Host
removal takes the exclusive lock, refreshes authoritative inventory after waiting, and refuses
remaining projects or unknown routing state. The empty project store's regular `.lock` file
does not count as a project. Mutation locks live outside the removable routing tree; a prepared
command whose registration was removed fails before performing project work.
Project admission and execution check the selected yard's current role on its owner.
Remote checks use a read-only owner query after route validation, without loading
the owner's yard name as a controller registration or opening project state.

### Temporary SSH-key access

`ssh-agent` is a dedicated owner-local credential workflow. The CLI validates the explicit key
and TTL, assesses access changes before mutation, and keeps passphrase input on the operator TTY.
`internal/adapters/sshagentruntime` owns an isolated OpenSSH agent and a detached, expiring worker
per owner data home and yard. Its private control endpoint is never exposed to the guest. A pinned
SSH connection opens a reverse Unix listener in the guest; only SSH2 identity-list and sign
requests reach the isolated agent. Native key lifetime and worker cancellation enforce expiry,
including closing existing agent channels. The worker consumes its launch record and cannot
restore an unlocked key after restart. Status and revocation remain available during release
recovery, while unlock uses the normal mutation gate. This workflow does not use RPC credential
payloads or the persistent `yard keys` ledger.

The shared `scripts/ssh-agent-environment.sh` physical leaf installs and checks the guest shell
fallback and OpenSSH client default. Shipped `profile.json` declarations supply service-specific
`guest_environment` hooks; SSH initialization, readiness and explicit unlock invoke every declared
hook, including deselected profiles with installed services. The Orca profile owns its systemd
drop-in and durable refresh marker; repeated grants do not restart an already-current service. See [temporary SSH access](ssh-agent.md) for the public command contract
and the distinction between expiring signatures and already authenticated SSH sessions.

### RPC

`yard rpc --stdio` is the only machine protocol. Each frame is a four-byte big-endian length
followed by at most 1 MiB of JSON. A session must call `rpc.negotiate` first; responses and ordered
events carry protocol version, request/operation ID and typed errors. A `cancel` frame targets an
active operation ID. The bounded writer queue never drops responses; events that outrun the
client are coalesced into an observable sequence gap, and a client that reads nothing for the
write stall timeout has its operations cancelled.
Negotiation also returns the engine build version, supported protocol range and capabilities so a
rolling controller/owner-host mismatch is explicit. Calls may carry an RFC 3339 deadline; expiry and
explicit cancellation produce different typed errors.
The `yard-status-profile-dashboards-v1` capability selects `yard.status.profile-dashboards`
with the existing status response schema. Controllers fall back to `yard.status` when the
capability is absent. Legacy status omits VM profile dashboard URLs that require guest
SSH routing and reports an update-controller diagnostic instead.

The outer event `sequence` and `revision` are one monotonic per-session stream; adapter-local Incus
revisions remain typed event data and cannot make the RPC revision move backwards after a snapshot.

The switched surface exposes `command.list`, `context.get`, `operation.route`, `operation.plan`,
`operation.execute`, `integration.status`, `project.list`, `owner.inventory`, `yard.status`,
`yard.status.profile-dashboards`, `profile.list`, `settings.list`, `host.sync.status`, `session.prepare`, `credential.list`, `credential.status`,
`incus.events`, `system.snapshot`, `system.resync` and `system.ping`. `operation.plan` accepts every
public mutating core command whose handler family supports preparation. Interactive terminal and
protected credential-payload commands keep their dedicated transport rather than treating human
stdin/stdout as a typed result. Its server-side plan is bounded and single-use; execution requires an
explicit `confirmed=true`. The `operation-exact-plan-v1` capability adds `exact:true` to
`operation.plan`. Its response contains `schema`, the owner `plan`, its `digest` and `expiresAt`.
Exact execution requires that digest in the same RPC session, consumes the plan once and rejects
expiry, mismatched bindings and replay. The binding covers the owner context, arguments, public
plan and captured private assessment fingerprint. Request deadlines remain separate from the
five-minute plan lifetime. Integration and provision mutations use owner preparation over one SSH
stdio session. Lifecycle, initialization, teardown, test-VM administration, safe project actions,
credential metadata, configuration changes and mutating v2 resources use the same owner session.
`provision --list`, status queries and terminal sessions retain their query/session transport.
Bounded actions keep `never` confirmation; project export uses the owner session to bind its source
and controller destination. Protected credential values and SSH passphrases require their owner-local transport.

`operation-steps-v1` is a separate completeness capability. A mutating controller requires it and
sends `exact:true, stepSchema:1`; the response must include `stepSchema:1` and a nonempty validated
ordered `plan.steps`. An owner refuses an invocation without complete native steps with
`operation_steps_unsupported`. The older exact envelope alone does not establish complete effects.
There is no mutation fallback to an older owner. Each step has a stable ID, bounded target, safe
observed fact, fixed desired state, `apply|skip|conditional` decision, preconditions, optional earlier
dependencies, consequence and verified postcondition. CLI preview and confirmation consequences
come from these same steps. Private inputs and retained protected drafts contribute only to the
hidden digest binding.

For example, a synthetic resource can approve `service.install` on one registered guest and a
dependent conditional `service.start` on that guest's named service. The latter has observation
`unknown`, a fixed desired service identity and a native guard after installation. It cannot add
another service or guest during execution. A native refresh may skip an approved effect only after
proving its original desired state; a previously skipped target requiring work, a replaced target,
different inputs or expanded scope returns `plan_stale`. Guards run again at the owning write
boundary under existing locks. A successful adapter response is followed by native observation of
the postcondition. Dependent failures can occur after earlier approved writes and use their existing
cleanup/recovery; the operation envelope does not create a cross-component transaction.

Plans and active project transfers share a 64-operation session bound, including executions
in progress. `operation.discard` closes an unused plan; expired plans are pruned on further planning.
A confirmed execute attempt consumes its ID even when binding validation or apply fails;
`confirmed:false` leaves it available. Disconnect/cancellation releases retained drafts and aborts
unfinished admissions. Remote project sync retains controller archive bytes, binds a source digest
to one owner admission, transfers through the existing data plane, then calls
`project.copy.finalize` with the same digest. Finalization consumes the admission once, verifies the
content tree and metadata, and commits through the native store. `project.copy.abort` discards it.
Automatic project naming approves one independent copy within the owner's workspace namespace;
an explicit name remains exact and refuses collisions.

Remote clone and removal run on the authoritative owner even when their public command registry
uses controller-facing local routing. Discovered project selectors resolve to the stable owner
project ID. Owner execution commits the native record; the controller invalidates its inventory
and removes an obsolete cached record without issuing a second owner mutation. Clone retains the
prepared Git revision through checkout and verifies HEAD before native registration.
An empty source binds an unborn repository with no refs; execution rejects newly advertised refs
and verifies the empty working tree before native registration.

Project export retains the controller archive, its digest and an exact private patch destination.
The controller reads the authoritative record through `project.list` in the same owner session,
validates its identity against the discovered project, and reads the original source locally.
The owner resolves the project ID from its native registry and binds the guest source tree;
controller host paths remain opaque on the owner. The existing data plane prepares the portable
diff, and `project.copy.finalize` verifies the transferred baseline and unchanged native owner
source before the controller publishes the patch. Publication verifies destination identity,
patch bytes and exact permissions. Abort, disconnect and cancellation clean only operation-owned
temporary state. Export keeps bounded-write policy and adds no confirmation prompt.

Legacy execution still refuses a controller-side plan routed to a remote owner with
`remote_owner_required`; controller plans cannot be transferred to another session. Durable release
and config-sync recovery retain their existing authorization independently of outer plan expiry.

The full snapshot contains one revision over context, public commands, project inventory, yard status and
redacted credential metadata; `snapshot.ready` and Incus events use the same ordered event channel.
Human CLI output is never parsed as a fallback API. Secret-like fields are rejected recursively from
RPC parameters, Incus event metadata is allowlisted, and stdout contains frames only.

`owner.inventory` is advertised as `owner-inventory-v1`. It returns one bounded schema containing
the persisted owner `hostId`, observation time, every real local yard and each yard's authoritative
project registry. It excludes controller aliases, absolute host paths and secrets. Controllers cache
the complete response by HostID for 30 seconds; replacement is atomic, so removals cannot leave
per-record ghosts. A failed refresh keeps the last good response only as explicitly stale data and
makes an incomplete aggregate command fail.

The [Yard RPC v1 schemas and golden frames](../api/yard-rpc/v1/README.md) describe the
language-neutral desktop contract. `profile.list` (`profile-list-v1`) projects the current profile
catalog, selection provenance, role eligibility and observed native convergence without starting a
stopped yard. `settings.list` (`settings-list-v1`) returns typed non-secret effective settings and
provenance, preserving multiline and list representations. `host.sync.status`
(`host-sync-status-v1`) observes sanitized Git registration and redacted credential metadata offline;
it does not fetch or expose checkout paths, credentials or payloads. `session.prepare`
(`session-prepare-v1`) resolves an explicit host/yard/project into a fixed launch descriptor without
launching a tool or authorizing an additional SSH key. Desktop mutations continue through the same
single-use owner exact plan rather than a parallel domain API.

These query services and their typed projections live in `internal/ownerapi`, independently of the
desktop client and RPC transport. The CLI boundary decodes requests, verifies the bound owner
context, reloads persisted configuration where required, converts query errors and enforces response
frame limits. Existing domain operations remain in `internal/application`; `internal/rpc` owns the
framed protocol and session lifecycle.

`yard-bootstrap-v1` allows an exact `init --profile <preset>` plan to specify `targetYard` on an
existing owner session. The native loader assesses the preset and absent definition without
publishing either; confirmation and execution use the ordinary init input and stale-state guards.
The session's own context remains immutable. `profile.list.hasYardPreset` reports available preset
files without exposing their paths or contents.

### Config and context

The Go engine parses assignment-only config without executing shell, selects the local/named/remote
yard, applies generic defaults, normalizes paths and validates the complete context before dispatch.
It passes the validated environment to shell adapters with `SUBYARD_CONFIG_LOADED=1`; their existing
boundary consumes that view without sourcing config again. Migrated leaves fail closed without
`SUBYARD_ENGINE_CONTEXT=1`.

The validated context contract includes:

- `ACCESS_KIND=local|remote` and `YARD_KIND=container|vm`;
- a valid local `SSH_PORT`, or `OWNER_ENDPOINT` for a remote yard;
- `YARD_IMAGE` as desired L1 input, distinct from the observed Incus image fingerprint and from the
  L2 `PROJECT_ENV_BASE_IMAGE`;
- independent `ENVIRONMENT_PROFILES` and `CODING_TOOL_INTEGRATIONS` selections;
- absolute normalized runtime paths;
- `HOST_BASE == RESTRICTED_DISK_PATHS`, never a broad host root;
- validated UID, shift mode, sudo, and SSH-agent policy values.

Source-only domain modules do not load configuration themselves.

Configuration refusals that may be shown are `config.SourceError`, `config.SettingsFileConflict`,
the retired-template diagnostic and `domain.ConfigRefusal`. A domain refusal is a closed code
with a fixed template and next step, built only from
validated profile, setting, integration or yard-kind names. A producer that must keep an
unvalidated value in its error text appends it after the wrapped refusal; the renderer prints only
the refusal. Producers collect independent role and profile
refusals with `domain.CollectRefusals`. `config.Diagnose` is the single CLI renderer for load,
`config set` and `config validate` failures; it walks the collected causes and marks any other
cause unclassified instead of printing its text. A raw OS or parse cause attached with
`domain.WithRefusalCause` is explained by its refusal; joined or multi-wrapped causes stay
independent. Boundary-owned fixed texts and sentinels enter as `PublicConfigDiagnostic` or
`config.KnownCause`; operation sentinels such as a stale plan keep their own text. The CLI renders
only configuration validation and usage causes this way; other preparation failures keep their
fixed diagnostics. An integration is named only when the shipped catalog contains it.
RPC errors carry the typed refusals `config.Diagnose` collects, with each code's stable name
from `ConfigRefusalCode.Name`, and every release-gate blocker with its typed action, in the
error's `data` ([Yard RPC errors](../api/yard-rpc/v1/README.md#error-refusals)); other causes
only mark that data incomplete.
Independent load stages merge their causes into one stable collection.

Configuration diagnostics guarantee that no setting value, secret or absolute path is printed,
including the retired-template guidance; that every typed code renders its fixed text and next
step, with next steps printed only for config commands (the alias-repair retry applies to every
command); and that all refusals within a stage are
reported in stable order. A refusal can hide a later stage only along these edges, each
named in the loader code:

- D1: the bootstrap chain, including command alias conflicts, stops at its first failed step.
- D2: yard identity (name, registration, template) resolves before sources are selected.
- D3: source files apply in precedence order. A refused file contributes its independent findings
  and leaves its values unapplied. Evaluation continues through the remaining source files.
- D4: native target discovery covers source and command-override refusals only.
- D5: unresolved integration roots select nothing for later checks.
- D6: any earlier refusal (command override, asset source, integration, role or profile
  constraint) skips profile resolution, which only adds shipped package integrity refusals.
- D7: within a check, the SSH port range follows its parse, path checks follow an absolute path,
  the disk minimum follows its size parse and CPU validation follows its resolution.
- D8: any refusal up to the context skips saved endpoints, derived ports and all collisions.
- D9: a derived-port source failure skips collisions and the web preview check.
- D10: an unreadable saved resource endpoint skips only collisions of the unset listener ports it
  would provide (every unset profile listener when the resource metadata itself is unreadable);
  explicit ports keep their collisions.

`HostID` is persisted owner-host identity; the OS hostname supplies only its initial snapshot.
`yard host rename` transactionally moves a registered Git owner subtree, commits the exact move
locally, and updates identity/cache provenance without changing runtime resource names. Explicit
`--source-host-id` recovery validates preserved settings before loading broken effective configuration.
`yard host add <owner-endpoint>` first lets OpenSSH resolve the endpoint (including ordinary aliases
and proxy configuration), shows the concrete SSH server-key SHA256 fingerprint, HostID and discovered
yards, then atomically stores the connection, strict host-key pin and initial inventory cache after
confirmation. Controller refreshes use only that managed key with `StrictHostKeyChecking=yes`.

When the pinned key is unchanged, an already registered controller adopts a new authoritative HostID
automatically on active status/yard/project-list queries after the 30-second cache TTL, and
atomically moves its connection, cache and HostID-scoped project-routing state. Offline peers
keep explicitly stale last-known data and converge on a successful reconnect query. Idle peers
have no background refresh guarantee. Operator endpoints, aliases and proxy settings stay unchanged.
Collisions fail before mutation and the old HostID is not retained as an alias. A changed SSH key is
an integrity failure: refresh preserves the last cache as stale data and instructs the operator to
run `yard host repair <old-host-id>`. Repair shows the old/new fingerprints and old/observed HostIDs;
one explicit confirmation may accept both changes through the same recoverable migration. `yard host
remove` first performs a strict read-only authoritative inventory refresh, refuses live project
references, and removes only controller-owned connection, trust, cache and routing state. Legacy
remote-route records are a separate compatibility store and must be removed explicitly first.
One-minor legacy discovery may retain an explicitly stale, untrusted inventory snapshot for offline
listing. A later confirmed `yard host add` of the same endpoint and authoritative HostID upgrades that
snapshot atomically to managed SSH trust; it does not require deletion or manual state repair.

Ordinary CLI SSH calls share an invocation-scoped gate in `internal/sshtrust`. Transports identify
their SSH target before handing off command arguments or stdin. The gate resolves OpenSSH config,
preserves each host-key namespace, and reuses registered owner pins. Missing keys are negotiated in
a private temporary file with authentication disabled, then verified through a trusted owner for
registered yard routes. The typed `ssh.trust` action requires consent before persistent trust is
added; login and exact-key verification must pass again afterward. Existing keys use strict checking
and cannot enter first trust or bypass explicit repair. Network deadlines start after this prerequisite,
so reviewing a fingerprint does not exhaust a status or inventory request's network timeout.

Private configuration Git transport is separate: it uses the operator's Git and OpenSSH
configuration, not this gate. A configuration command that plans and applies against an SSH remote
shares one OpenSSH control master per endpoint through `GIT_SSH_COMMAND`: Git's first connection
starts it in a private temporary directory, and it persists until the prepared command closes
(`internal/cli/config_git_ssh.go`). Each master reports its own PID over its socket after the Git
command that started it; it is pinned by a pidfd and accepted only as this user's process with the
exact `ssh: <socket> [mux]` title. A watchdog, the engine re-run as the hidden
`_config-git-ssh-watchdog` verb, reads a pipe whose only writer is the CLI process and pins each
reported master the same way. On EOF from a normal close or process death, including SIGKILL, it
requests a bounded exit from every master socket, terminates still-running masters through their
pidfds, removes the sockets and then the empty directory; a directory with any other entry is left
and reported. A normal close then repeats that stop for anything left. The verb accepts only a pipe
as input and only a real, caller-owned mode-0700 session directory. Only executables that register
the verb (the engine and the CLI test binary) start the watchdog; other programs linking the CLI
package keep only the normal-close cleanup. Signals
go only through pidfds of verified masters and through the exit requests' own process handles. A
master that never verified, or one left when both the CLI process and the watchdog are killed by
SIGKILL, survives until it is stopped manually. An explicit operator SSH command, variant or
multiplexing setting disables this sharing; see [Versioned private configuration](configuration.md#versioned-private-configuration).

Structured system adapters are selected from the validated command manifest and receive only declared
non-secret context keys. Metadata uses a dedicated file descriptor and protected input uses stdin.
Leaf commands report diagnostics normally; the runner converts their exit status into a typed result.
The runner supplies a fixed `PATH`, enforces output/time limits and terminates the process group on
cancellation.

### Project state and routing

The native `internal/state` store is the only project-state implementation. Project state is one
owner-only JSON file per project ID. Schema 1 requires typed identity, name,
host/yard paths, mode, and SSH host; target/profile and yard-origin markers are optional compatible
fields. New records use identity version 2: the canonical safe name is also the project ID and the
workspace is `/srv/workspaces/<name>/src`. Name admission is case-insensitive and serialized by
durable operation reservations; automatic basename collisions receive `-2`, `-3`, and so on, while
an explicit `--name` collision fails before physical mutation. Existing project IDs and workspace
paths are never renamed. A source fingerprint is stored separately for repeat admission and path
routing; it is not part of project identity. Reads reject corrupt JSON, filename/identity mismatch,
invalid targets, and unknown schema versions. Writes use a mode-0600 candidate in the same
directory, validate it, then atomically rename it over the prior record. When a store is opened,
valid owner-owned schema-1 records whose mode matches the original Bash writer's `0666 & umask`
output are tightened in place to `0600` through a no-follow file descriptor; symlinks, malformed
records and anomalous modes remain fail-closed. Release upgrades do not replay this repair as a
perpetual desired-state guard; opening the native store remains the owner of that invariant.
Store open also converges legacy duplicate display names deterministically without changing their
IDs or paths. Incus and Docker consumers derive collision-free technical names with byte-wise
`_hh` escaping; Docker image suffixes add a leading `p` and escape uppercase and punctuation so
the repository name stays lowercase-safe. These encodings are not project identities or selectors.

### Installation check

`yard check` is a read-only installation check. It runs before owner recovery, audit and the
release gate, with read-only SSH trust, also with `--yes`. It never applies repairs, starts
yards, requests sudo or fetches release candidates. Readers that normally wait run in an
observation mode instead: yard loads and the release inspection hold a shared configuration
lock tried for at most two seconds (a writer is a `config.root.lock` blocker, and the readers of
loaded yards are then `not-inspected`), the release inspection tries the owner lock once (a
holder means release state is changing) and is bounded like status release readiness, and the
host observation never probes the yard SSH port. Like `yard status`, it may create owner lock
coordination files under `/tmp/subyard-owner-locks-<uid>`; it writes nothing in the
configuration or data homes.
Configuration is judged from persisted sources, as `config validate` does; host and state
readers use the command environment. Sections run in a fixed order and each reports every
independent finding:

- `config`: schema of every registered local yard with config validate's repair, including alias
  repair; ignored profile-only and retired assignments; legacy inputs; every unsafe managed config
  path, where an operator-owned group- or world-writable path has the exact `chmod go-w` repair;
  native target and host listener collisions between local yards.
- `release`: `migrate --check`'s inspection of the installed release, including the activation
  owners of every local yard. Typed pending configuration is `pending`.
- `host`: the `check --host` facts of the default yard as notices and blockers, without its
  new-yard SSH port probes, and credential store initialization.
- `state`: pending owner-inventory, hard cleanup and host rename journals, flat registrations
  that `config repair-registration` repairs, and each local yard's project store.

Yard substrate is not inspected and the text report ends with that line; run
`yard -Y NAME status` and `yard -Y NAME security` for each yard. `-Y NAME` with plain `check`
is a usage error; `yard [-Y NAME] check --host` keeps the original host preflight and output.

Owners return keyed states: drift, pending, blocked, unknown, deferred or notice; no items means
convergence. The aggregator maps unknown and blocked to `blocker` and deferred to
`not-inspected`. A finding ID is `section.key`, built from fixed tokens and resource names whose
bytes outside `[A-Za-z0-9._-]` are escaped as `%XX`, without hashes or ordinals. IDs are stable
identifiers; a dot inside a resource name stays a dot, so they are not parsed back into parts. An invalid or duplicate key, or drift without a next
action, is a programming error. `--json` prints `schemaVersion: 1`, a verdict `status` (`ok`,
`drift` or `blocked`), `summary` counts by class and ordered `sections`, each with `id`, `status`
and `findings` (`id`, `class`, `resource`, `installed`, `desired`, `message`, `next`). Exit 0
allows pending, not-inspected and notice findings; exit 1 reports drift or blockers; exit 2
reports invocation, contract or output errors. As for every command, a closed output pipe ends the
process with `SIGPIPE`.

### Release migrations

Release-owned one-time transitions are declared in
[`config/release-transition.json`](../config/release-transition.json). The
[`internal/releasetransition`](../internal/releasetransition) module owns read-only inspection,
authorization binding, protected evidence, per-domain epoch advancement, stable runtime links,
forward recovery and post-activation reconciliation. A completed one-time migration is durable
history and is never reopened to repair later drift.

After a successful `yard update` or `yard migrate`, the activated runtime reads `profile list`
for every local yard. A selected profile whose read-only owning provision check reports changes,
for example a profile provisioned before its capability registration existed, is not a migration
and does not affect release readiness. An interactive run offers it through the ordinary scoped
`yard -Y NAME provision PROFILE` plan and confirmation; `--yes` and non-interactive runs only name
that command. A declined or failed offer leaves the release result unchanged, and the next update
or migrate observes the profile again. `yard profile list` shows the same pending provision.
The offer belongs to the runtime that runs the command: an update started by an older runtime
does not offer it, so run `yard migrate` once after that update. Init-embedded migration does not
offer it either: init keeps its own plan and names its own provision continuation.

Registered local yards run missing compatibility migrations before ordinary init admission.
Activation checks `user.subyard.name` before reading any guest settings. A foreign marker
is a state refusal naming the owner and repair route. Local registered target collisions
report every shared instance and /srv volume with address keys, values and
source files/scopes. Operators repair conflicting addresses at yard scope; configuration
loading, init and release migrations never change addresses automatically.

Their native ownership and selected resource receipts use per-yard steps in the same release
journal. The migration cannot be rolled back, so existing state does not stop it: local devices
are taken under management as they are and ordinary `yard init` owns later convergence. Device
refusals remain for a missing or different `srv` device, because reattaching it would change
persistent data, the `ALLOWS_HOST_ACCESS=false` boundary, a selected resource device that fails its
recipe or receipt, and local and expanded devices that disagree. Identity, project policy,
inventory, network and instance configuration stay checked. Working resource endpoints are retained: a native endpoint is imported only over a
shipped default or unset setting, and an operator-set or saved value is never rewritten or
compared. A refusing assessment, including `--continue-legacy`, reports every problem it can determine,
for every registered yard.
The guest temporary-storage step lists each unmarked yard in the read-only update plan.
A running yard restarts: active agent sessions, processes and connections are lost, `/tmp`
is reset, and persistent data is kept. A stopped yard stays stopped and finishes at its
next start; run `yard -Y NAME start`, then `yard migrate` to repair any deferred core
and profile runtimes. This guarded migrate also removes selected retired integrations and records
proof, including an already-clean guest. A stopped yard with both jobs pending receives the
same sequence in its retirement reminder.
The normal update confirmation, or `--yes`, authorizes the listed restarts
before any migration or journal write. Decline or non-terminal input without `--yes`
executes nothing; a changed restart set requires a fresh plan. Read-only checks never restart.
An update already interrupted on a released version finishes under its exact sealed owner's
rules, including that version's separate restart confirmation where required.

Each running yard applies the existing guest policy, restarts and verifies it before setting
`user.subyard.disk_tmp=v1`. Host and guest safety checks run before stopping the yard.
That approval also covers interrupting connected sessions. The stop runs
`lifecycle-guard.sh stop --interrupt-sessions`: it still pauses new SSH connections and profile
owner services and probes for sessions, but warns about VS Code Remote-SSH, SSH shells, the SSH agent
forwarder or an unverifiable session state instead of refusing. An ordinary `yard stop` without
that approval still refuses them. The same restart after a `yard start` that finishes a pending
yard follows the start plan, which discloses it.
A yard-local failure warns and leaves its marker absent while the remaining yards and release
update continue. Readiness and sorted warnings read the marker and actual power state. Pending yards do not hold profile/core runtime readiness;
host key-tools and host-wide owners remain strict, so a pending route-owning test-yard can
still prevent activation. Desired power stays running after a failed restart, allowing the
ordinary boot reconciler to retry it. A plain candidate `yard update` prepares only pending
running yards for retry with the normal confirmation. For a stopped pending yard, run
`yard -Y NAME start`, then `yard migrate`; update never boots it. Start completes the
marker with the normal restart and does not repair installed runtimes. Start reports pending completion as a warning
if the same yard remains running, and as an error if it does not. The physical migration
also prints its restart consequence immediately before stopping each yard, including
when a released caller omits plan output with `--yes`.
If an update retry reveals runtime drift, its existing activation follow-up prints a fresh
repair plan and obtains that plan's ordinary confirmation. After an explicit start,
`yard migrate` provides that guarded repair instead, or the yard's own `yard -Y NAME init`; until
one of them completes only that yard is limited to its repair commands.
Persistent data and instance identity are retained. Read-only checks never retry.

A repair of the active release by `yard migrate` or `yard update` uses recorded installed
init-owned values after a completed journal, including a completed rollback, regardless
of the repair request's direction. The same
view survives interruption when the unfinished journal's source equals its target, with matching
artifact and registry. Ordinary updates and rollbacks whose source differs from their target
use desired configuration on resume. The installed view covers the integration applied snapshot,
including identity, profiles and effective integration selection, and core-guest receipts,
including recorded provisioning inputs such as `CCUSAGE_PROVISION`. Complete integration snapshots
also record absence. Init-owned values without an installed record, including profile-owned
settings, remain desired in both approving and resuming processes. Repairs by their owning runtimes
use ordinary plan steps; these values are not listed as pending init. Unavailable unrecorded
desired inputs retain their ordinary failures. Test-yard route inspection retains its ordinary
desired configuration loading.

`activeReleaseRepair` in `internal/cli/release_configuration.go` makes the single view decision
from the protected journal, the current release link and the verified request. The activation
yard loader shares one `releaseConfiguration` across its owners, migration children and runtime platforms.
Each yard's context is built once with `installedReleaseTarget`: applied integration inputs,
identity, effective selection, profiles and protected core receipts share one overlay. Consumers
using that loader observe and apply the resulting context without selecting another view.
Pending-init detection compares desired inputs with this same overlay, normalizing effective
selections and the installed architecture's usage pin.
Binding owners preserves ordinary applicability inputs. Non-repair observations keep their
ordinary configuration view; completed materialized readiness remains release-wide independently
of that view, so selecting another yard cannot hide drift. Typed integration ownership
refusals remain visible in initialization, status and migration readiness. When every refusal
of a yard is typed pending configuration with a public next step, release repair of the active
release defers that yard as a whole, like a stopped yard: it becomes one finding of a successful
observation, its refusals joined behind `yard NAME:` (beyond the protocol bound, their count),
with the yard's own init as the next step only when every refusal names it and otherwise another
step, never another yard's init; its desired configuration stays in the activation's Desired
without drift, and its consequences and saved integration plan
leave the activation. The materialized-config owner records the deferral once per activation,
reusing its own observation when another owner needs the decision first; the profile and
core-guest runtime owners and the test-VM broker stage read the same record, so none of them
runs a guest write in a deferred yard, including on resume, and gaining or resolving a finding
before a resume leaves the journal's observation scope unchanged. Host-level owners (host key
tools, host power) are repaired as usual; the test-yard route-consumer owner does not read the
deferral. The other yards are still assessed
and repaired. Any other refusal keeps every refusal, pending ones included, as one observation
failure; a yard refused before its assessment, or more than 256 pending yards, cannot be
deferred, and the refusal names `yard migrate --check`. An ordinary update, or a prospective
source publication, does not defer: it stops before activation with the pending refusals and
their next steps. A gate whose inspection still reports any owner's activation drift is never pending
configuration. The engine reports deferred findings as one bounded warning while other release work remains; when
the active, completed release has no other work, each finding is its own
`activation.materialized-config.pending-configuration` blocker of an `operator-action-required`
outcome. Recorded [yard-scoped runtime failures](#yard-scoped-runtime-failures)
of a profile runtime that declares that scope use the same shape, as
`activation.<owner>.pending-configuration` blockers; no other owner's pending configuration is
admitted. Admission of commands around such findings follows the
[yard-scoped release gate](#yard-scoped-release-gate). Invalid history, unfinished
transactions and unclassified materialization failures still block mutation. A local typed
selection reduction in this state validates the sealed original source and complete protected
history, locks and rechecks that evidence, then writes only the desired selection; it grants
no activation or historical recovery authority. Drift beside a pending finding is ordinary drift
for selection reduction and its repair permit.

A yard whose last init did not finish is left to that init. Init's instance stage writes
`user.subyard.initialized=false` and only its finalize stage writes `true`; `start` and `stop`
keep the value. Like a stopped yard, such a yard is neither observed nor applied by the profile
runtime, core guest runtime and materialized-config owners or the test-VM broker stage, so a
failed first init leaves the release ready and other yards' commands open. Each engine process
decides this once per yard and may only narrow it: a yard init enters during a transaction stays
out of that transaction. The yard gets one release warning naming `yard -Y NAME init`; after that
init finishes, release work observes the yard again and its failures block as before. Host key
tools are never deferred. An engine without this rule observes the yard and fails closed.
The boot power reconciler never starts or stops such a yard, in any state, except for its
fail-closed stop of every running yard after a host network policy failure; it reports the yard
as not initialized with `yard -Y NAME init` as the next step and restores the other yards.
The first finished init also writes `user.subyard.ever_initialized=true`, which nothing clears;
a re-init that clears `initialized=true`, and the import of an unmanaged yard, write it too, and
legacy continuation and adoption accept it as power-owner metadata. A yard that has never
finished init refuses `start` before any power change with `yard NAME has not completed its first
init; run yard -Y NAME init`. Its `stop` and `teardown` keep the SSH listener fence and pause every
installed owner service, since a failed first init may already have installed some. When its
guest also lacks the substrate provisioning installs first (python3 and
`/srv/agents`, by an explicit answer of a successful guest probe), owner services get
`SUBYARD_GUEST_NEVER_PROVISIONED=1` and an owner whose state lives only in the guest skips its
guest pause, resume and removal; `stop` checks SSH sessions for all users; and `init` defers its
profile runtimes like a stopped yard's, plans its project hooks conditionally and reads its
applied integration state as absent only if no integration inventory exists. Release repair
decides that a yard is left to its init from a context loaded without any guest read, before
every part of context resolution that can read the guest. Host key tools and registered-yard
migrations, which change only host state, are not deferred and use that desired context; service
registration withholds itself while any such yard waits for its init. A working yard whose
re-init failed keeps the strict checks. Boot rechecks each yard under the host network policy
lock right before every power or runtime effect, holding only the lock for a stop so a policy
failure never blocks it, and init marks an existing yard uninitialized under that lock. Boot's
host network check always covers the valid bridges of every managed yard, including those left
to their init.

Declared persist directories are adopted by a separate guest program
(`internal/adapters/reconcileruntime/integration_persist.py`), not by the artifact
inventory. It drops to the developer account, uses descriptor-relative no-follow
access, and derives transfer state from the filesystem and one descriptor beside the
source. Before its single exchange it neither deletes nor overwrites anything;
destructive calls exist only in the stage discard, the backup deletion and the removal
of its own records, which a source-level test enforces. Traversal never crosses a mount.
Its observation is part of the integration plan fingerprint; the inventory receives a
projection of the exact links the transfer publishes, so the initial inventory plan stays
bound across the exchange. A backup or stage finish cannot remove is listed by status and
never blocks anything. Only typed transfer conflicts are pending configuration;
transport failures and program crashes remain blocking inspection failures.

Before confirmation, the plan lists local yards in name order and differing init-owned
integration settings in setting order as `NAME  installed -> desired`. Sensitive settings show
only `changed`; missing applied snapshots show
`installed integration settings unknown (no applied baseline)`; stopped or absent yards show
`stopped; not inspected`. Unavailable yards show `not inspected (unavailable)`, with a safe reason
when desired integration sources cannot be read. They receive no init step and do not add a
release blocker. This listing is printed even when no yard is pending. Ordinary configuration
and update loading remain strict. Detection compares effective integration selection, applied integration inputs,
identity and profile selection, and core guest receipts. Suppressed requests and irrelevant
consumer settings do not stay pending after successful init. The usage helper receipt records
only the guest architecture's archive and matches either configured architecture pin.
Resource limits and other init-owned settings outside these recorded inputs are not detected;
apply those changes with explicit `yard -Y NAME init`, as before.

After repair reaches `ready`, including on resume, ordinary `yard -Y NAME init` runs for each
disclosed pending yard in name order. Plan steps put these inits after repair. Before each init,
the command rechecks that yard's listing. An unchanged listing uses the original confirmation.
Explicit `--yes` or `ASSUME_YES=1` also approves a changed listing. Otherwise a changed listing
uses init's own plan and confirmation if a terminal is available; without a terminal it prints
the new listing and continuation command without running init. Unreadable desired integration
sources remain uninspected. Each init uses ordinary admission and guards; refusals are collected
in stable order with each `yard -Y NAME init` continuation, leaving release repair complete.
A failed init can leave assets for its desired selection before its hook boundary; the ownership
records it introduced stay marked uncommitted until a successful init commits them. Installed-view
readiness and repair own only the committed installed scope: they neither report uncommitted
records outside that scope as drift nor retire or drop them, so the release stays ready and the
next successful init commits or retires them. Committed records that leave the installed scope
are still retired, even when a failed apply left intent labels on them.
Retirement of deselected owned file, link and structured assets follows the hook boundary: the
inventory commit removes the files and links, and structured owned fields are released only after
the hooks succeed. A failed init that deselects an installed integration therefore leaves those
assets and their committed records in place; for them installed-view readiness stays converged,
repair has nothing to restore, and the next successful init retires them. A removed owned asset,
foreign bytes or metadata remain drift or ownership conflicts. Effects that still precede the
hooks are not covered: lifecycle handlers that retire a deselected lifecycle integration, and the
project-hook manifest `/etc/subyard/agent-project-hooks`, which inventory apply rewrites when the
deselected integration declares a project hook. A failed init that changes the bytes of a file
that stays selected is not covered either; installed-view readiness reports that file as drift.
RPC execution prints the listing and commands without running in-process inits. With no pending
integration settings, no init steps are added.
When a running yard's repair would change installed integration state while its integration
configuration awaits init, materialized-config activation also inspects that desired
configuration read-only, as `yard integration status` does; a planned source publication is
inspected through its prospective files. This happens in every observation of the repair,
including planning and `--check`, so a refusal precedes any effect. An ownership conflict the guest
reports there, such as changed owned fields of a deselected integration, refuses the repair:
restoring the installed state first would let the following init retire the operator's change.
A failed inspection also refuses, with its own diagnostic rather than a conflict. Fix what
`yard -Y NAME integration status` reports, then run `yard migrate`. When that init would instead
release ownership of a deselected integration's changed or missing owned configuration in place,
the repair defers that yard with a pending-configuration finding whose next step is
`yard -Y NAME init` and repairs the other yards; an independent conflict in the same view
refuses the repair together with it. The guest keeps a release's reason
until fresh ownership, so the finding also holds after the guest released ownership and before
the integration inventory commit forgets the record; without it, the repair would overlay the
released document. After that commit, while the installed configuration still selects the
document, the installed-view inspection itself reports the same pending-configuration
finding. A deliberate enable from the desired selection is not refused and takes fresh ownership. That init is admitted and
performs the release without first restoring the installed state. Desired integration sources
that cannot be read are not inspected, as in the pending-init listing, and the repair proceeds.
Each yard is observed independently: refusals of all yards are reported together in target
order (default first, then by name), and only findings that depend on a yard's failed read are
skipped. When they exceed the diagnostic bound, the summary keeps the first refusal and names
how many were omitted and the status views that report them in full.

Go is the production release owner. Historical transitions retain their exact sealed
owners and frozen process and recovery protocols. Unsupported experimental owner
metadata is refused before executable probing and cannot enter historical metadata
compatibility. The stopped [Rust owner evaluation](reference/rust-release-owner-evaluation.md)
records the decision to keep release ownership in Go.

When switching releases after source migrations are complete, materialized-config
activation observes and reconciles all registered local yards, using the same scope
as the completed release's readiness check. Fresh plans whose prepared source work
only advances the ledger also assess all local yards before authorization. Actual
settings, registration or ingress mutations retain their selected-yard scope across
recovery because they can change configuration or rename registrations. Historical
journals keep their recorded scope; a reconstructed all-local scope may resume only
when its complete observation binding matches that journal exactly. Config
reconciliation leaves stopped or absent yards untouched; a saved running integration
plan is skipped if its target has stopped; target and integration scope remain in the plan.

After selected-scope source work completes, a fresh readiness inspection can find
activation drift in another local yard. In that case, one `yard update` prepares
at most one follow-up repair of the exact active release. The first transaction
and its authorization retain their original scope. The follow-up reloads persisted
configuration, performs a fresh read-only assessment, prints its own operation plan
and requests its own effect-selected confirmation before execution. `--yes`
answers that prompt; it does not bypass guards or verification. No newer release
is selected for this phase.

This follow-up requires a complete journal, no blockers, and a final inspection
with status `migration-required`, code `transition-required`, and the expected
release both active and targeted. Other failures retain ordinary failure handling.
Decline, EOF or non-terminal input without explicit consent leaves the release
active and the remaining yards unrepaired: the command explains the remaining
plan and `yard update` retry, retains `migration-required`, and exits nonzero.
A second non-ready inspection is reported as a failure without another follow-up.
Each phase retains its own update-history attempt. An interruption between phases
is recoverable with ordinary `yard update`.

Remote CLI updates use the same owner-side flow. An RPC execution, including an
exact pinned plan, authorizes only its stored plan and never extends consent to
the follow-up. It returns `transition_required` with the active release, pending
readiness and remaining plan; the controller must request and authorize a fresh
operation plan. `update --check --json` remains read-only.

Activation plans explain each affected yard's optional selection from that yard's loaded resolver
snapshot, including inherited host Git selection and local empty overrides. Read-only component
inventory uses the same snapshot; yard convergence and component health have separate scopes.

When an update plan includes host-power repair, the CLI prepares root authorization after
consent and before repeating candidate verification and native inspection. The host-power
installer always needs root. Read-only checks, declined plans and plans without that repair
do not request this early authorization; other activation owners retain their own privilege
assessment. Authentication does not replace native revalidation or durable recovery guards.

The compiled `local-ssh-port-settings-v2-to-v3` migration advances the settings epoch to 3 and
pins persistent Git-derived SSH ports into local yard settings. It does not modify source Git or
managed cache. Local publication binds exact bytes, ownership, modes and parent directories;
absent targets use no-replace publication with durable private parent creation. Explicit sync
owns cache sanitation and prepared Git source cleanup. The new epoch has no legacy rollback
compatibility policy. Recognized retained owners keep their exact previous compiled catalog,
registry and journal identity; unknown catalog digests remain blocked.

An original activation-only reconciling journal with a complete ledger can admit an independently
consented local optional-selection reduction. Source admission verifies sealed assets and the
original owner's schema-2 recovery capability without performing activation or rewriting history.
Source publication rechecks protected metadata, tooling, all affected registrations and scalar
inputs under their existing locks. Recovery then needs fresh sealed-owner consent bound to the
changed inputs. Migration steps, source ingress, successor recovery histories and ordinary
configuration apply do not enter this source-reduction boundary.

Current route-consumer activation uses each managed yard's effective host-access policy.
Yards that forbid host access do not receive or probe the shared E2E route mount; a missing
mount on such a yard is converged. Native device ownership checks still apply to every
managed yard, and an existing forbidden mount blocks inspection until its owning lifecycle
removes it. The historical one-time route migration retains its frozen contract.

Ordinary `init`, including profile provisioning, checks host-wide release readiness after
all lifecycle effects and before reporting successful completion. Remaining drift returns
the release outcome and safe continuation; it does not reopen completed migrations or run
an automatic migration from `start`. An explicit normal `init` can repair its selected
yard's core guest and profile runtime drift through a fresh bound assessment;
host key-tools and unselected runtime drift remain strict. A selected init still assesses
and applies the exact guest facts, including a yard whose disk-/tmp marker is pending.
Other activation repair requires
a fresh assessment and authorization.

The standalone installer completes the supported current-release transition after creating
the first runtime link and before publishing CLI entrypoints. Its initial installation
consent includes this boundary. An interrupted first installation retains protected recovery
evidence; rerunning the installer uses the existing-runtime transition to resume it before
publishing entrypoints. Publication-only staging leaves migration state untouched.

Release activation reads Incus only for an installation that can own native yards. A cold
installation keeps nothing in its configuration home except first-install release bookkeeping
and host-wide settings (`config.env`, `overrides/shared`, `overrides/host`): no owner HostID,
yard registration or config file, Git settings, applied-state record, retained previous
release or upgraded history. Init writes the HostID before it creates any native yard, so
such an installation owns none, and its release activation observes an absent server
(`coldInstallation` in `internal/cli/release_native_scope.go`, applied in
`releaseActivationOperation`). While the installation is cold, which in practice means
before its first init, installing, rerunning the installer and the first `yard init`
(including plain `yard -Y NEW init`) work when Incus is installed but this account cannot
open its socket. Such an init plans with its native owner check deferred; after its approved
Incus stage grants access it repeats the strict owner and target-collision checks before any
instance stage or yard write, so it never takes over another yard's instance. Other init
modes and teardown never defer. A native yard left behind by another installation is foreign
until init adopts it. Any unreadable or unexpected fact, including an unknown runtime root,
makes the installation warm. A warm installation without socket access, including any update
after the first init, fails closed: each local yard reports its own typed
`domain.IncusAccessRefusal`, default first and then by name, with one next step (a new login
session or `newgrp incus-admin`, after `sudo usermod -aG incus-admin "$USER"` when the account
is not a member). Interrupted transitions report the same refusal per journal row. The Incus
client types only a permission error of the socket dial as `domain.ErrIncusAccessDenied`.
Profile runtime handlers that run the `incus` CLI themselves report their own
permission-denied observation failure instead.

Schema-1 installation identity and protected metadata publication are shared by bootstrap,
runtime publication, candidate activation and ownership consumers. The candidate advertises
`installation-runtime-v1` and assesses missing runtime inventory as an activation step,
including same-release reuse and updates delivered by supported historical callers. Inspection
never repairs metadata. Publication binds the native directory, exact file snapshot and owner
epoch under the existing owner barrier; matching metadata stays unchanged. Runtime inventory
does not claim bootstrap entrypoints or native project ownership. Full bootstrap retains its
separate inventory and preserves previous shell and cache claims. A transient download cache
does not change an established installation cache root; private release drafts have their own
runtime scope and never donate identity to the live installation.
The manifest-bound `config/installation-capabilities.json` admits the common installer
leaf; retained older payloads without that contract keep their historical installer semantics.
The current full bootstrap requires this capability and a successful candidate API inspection
before activation or entrypoint changes.

Limited legacy continuation assesses selected native declarations against observed device,
mount and privilege facts, using the ordinary extras policy and owning host-memory validator.
It grants only bounded normal reconciliation after explicit consent. An already selected named
provision hook uses its own verified native prerequisites and the ordinary provision planner;
unrelated selected hooks and native repairs do not expand that operation. Missing prerequisites
retain the assessed init and ownership boundary.

Activation observers do not print diagnostics. Return expected drift as
`Converged: false`, persistent notices as `V2ActivationObservation.Warnings`, and
failures as errors. Converged readiness may return `Diagnostics` describing state;
these are neither warnings nor consequences and never make a skip step stale.
The operation boundary renders current `Outcome.Warnings`
once, sorted and deduplicated. Public error details use `ActivationDiagnostic`;
raw guest errors stay private.
Profile handlers may emit one `subyard-profile-observation-diagnostic-v1 ERROR FILE:LINE`
record on stderr when observation fails. The adapter accepts only a closed error-class
vocabulary, a regular Python source basename in that profile package, and a bounded line
number. Exception messages and arbitrary stderr remain private. The release refusal retains
this validated location and names the read-only `yard -Y NAME config paths` command
for inspecting configuration while ordinary mutation admission remains blocked.
An owner may explicitly publish one `subyard-profile-observation-refusal-v1` JSON
record with public `message` and `retry` fields (each at most 480 bytes, no control
characters; retry starts with `run `); a failing `rollback-apply` may publish one too, and
other stderr of the rollback actions is not shown. Unknown fields, duplicate records and malformed
values are ignored. This deliberate public record may name a configuration file and
a preservation command; ordinary stderr and exception text remain private.
State-drift refusals identify each affected yard and each independently known
parameter in stable order. The complete status view renders `yard NAME:` followed
by `PARAM  installed -> desired` rows. Sensitive values render only `changed`.
For changed file bytes at the same recorded path, render `content changed`.
A missing applied baseline is unknown, and a stopped yard is not
inspected; neither is replaced by desired values. Report independent failures
together, preserving their classification and a concrete repair route.
The release protocol retains its bounded single-line diagnostics. A shortened
summary names the number of omitted yards or findings and points to the owning
status view for complete detail. Diagnostics evidence must not persist raw
secrets or file contents, and older records without that evidence remain unknown. Optional evidence binds to committed applied settings and inventory entries;
changed or unsupported evidence is ignored, while unsafe inventory permissions and ownership still refuse.
This detects older writers changing those committed records, but cannot prove an unrecorded source-path
change that produced identical records and bytes, or an older writer changing a sensitive value
that was never recorded in applied settings or artifact entries. Such activity cannot establish
reliable source or sensitive-value history without separate writer provenance. Status and pending-init share normalized comparisons,
consumer selection and guest receipts; unknown evidence alone never schedules an init.
Registered-migration summaries drop names before reason classes when the frozen message bound is reached. Full filtered verdict details are available on the stderr of `yard update --check --json`; `yard status` and resource-shutdown probes discard this diagnostic stream. Mixed dependency failures retain their original stderr detail and yard identity; raw dependency text does not enter the JSON report.
Mutation refusals retain the verified inspection’s public decisions and blockers;
a later readiness observation does not replace the refused resource or reason.

Recoverable reconcile failures retain validated public diagnostics after reobservation;
ambiguous state and protected transition guards retain their own recovery instructions.
The original public failure also remains visible when recovery observation adds a blocker
or cannot establish a safe state. The recovery status and next action continue to describe
the controlling guard. Settings blockers identify the yard without exposing assignment values.
Configuration failures retain the failing yard, inspect/apply/verify phase, source role,
path relative to config home, canonical key and line when known. Their public reason is fixed;
assignment values, arbitrary parser text and guest stderr are never rendered. Guest-reported strings
are decoded as opaque guest text and matched against host-declared values; only the matched
host value, a fixed reason or a short digest is rendered. Installed configuration values
(applied diagnostic file paths and non-sensitive applied settings) are host-recorded and
shape-validated, so `yard config status` may show them. A suggested
`yard -Y <yard> config status` remains read-only during recovery and preserves this source context.
Post-apply observation failure retains its adapter diagnostic; invalid observation requires
operator action, while valid remaining drift stays recoverable. This holds also when a failed
apply left the owner in another valid state: resume binds only the desired scope and reconciles
from fresh facts.
Failed update summaries render the validated execution outcome immediately; they do not
run another inspection or claim that final readiness was verified.
Stage adapters may implement `VerifyStageWithDiagnostic` for post-apply validation.
Ordinary observations continue to return expected drift as a state; post-apply failures
return the observed component reason through `ActivationDiagnostic`. The test VM broker
uses bounded, source-owned readiness reasons for its host sink, backend and guest checks.
Base guest provisioning masks `tmp.mount` for every container and VM, independently
of optional profiles. Policy application waits at most two minutes for the guest's
native system manager to finish booting (running or degraded), since exec readiness
can precede the system bus and boot mounts. A newly created instance completes a
guarded first-boot restart when necessary and waits for completed guest boot again
before filesystem and network inspection. Repeat provisioning only installs the
next-boot policy: it never stops a live `/tmp`, removes its files or closes sockets.
Prepared native creation carries that restart authority only from the current
successful instance POST through its fixed shell portions, bound to the returned
UUID and owner token. A retained create result establishes identity, without
granting permission to restart a guest that may already contain active work.
Provision readiness checks the actual backing filesystem, root ownership and mode
1777 as well as the mask. An active tmpfs remains unconverged; post-apply diagnostics
ask the owner to close sessions and stop/start that yard. After apply, provision
verification names every unmet condition that does not depend on another unmet or
unreadable one, in check order, including each guest `/tmp` policy finding, after any
inspection failures. Ordinary observation still stops at the first of either.
Intentionally stopped yards retain their deferred policy marker and verify the actual
filesystem when running.
Initial observation failures also retain these reasons; repairable drift stays a state.

Starts wait for the guest agent, up to ten minutes for a VM. A guest reboot that Incus
serves with a new QEMU process passes through STOPPED for about two seconds; an instance
that stays STOPPED for 30 seconds is not booting, so the wait ends at once with that
cause. The first in-guest reboot of a new VM always needs such a restart (Incus marks
it `volatile.vm.needs_reset`), and Incus 6.0 can lose it when the old QEMU monitor
disconnects. Incus 6.0 also sets that mark on any configuration change to a running VM,
so a later reboot from inside the guest can leave the yard stopped the same way; start
it again with `yard start`. Initialization therefore starts a VM it created in the same invocation
once more through the guarded start. A second loss, or a lost boot of an existing yard,
refuses with the instance's boot-log command and the init or start command to retry;
an existing yard is never restarted on the operator's behalf.

The same guest policy installs bounded native temporary cleanup. The early
`subyard-tmp-shutdown.service` starts before `sysinit.target`, so its stop command
runs after ordinary services and before local filesystems unmount. It invokes
`systemd-tmpfiles` with an explicit `/etc/subyard-guest-tmpfiles.conf`, clearing
the contents of `/tmp` while retaining its inode and mode 1777. Symlink targets
and descendant mounts are preserved. A separate `subyard-tmp-boot.service`
recovers leftovers before workloads start, including after an emergency hard stop.
It runs before native tmpfiles setup, which recreates vendor service directories.
Live installation enables boot recovery without running it and only arms the
shutdown service. Manual disarming refuses whole-`/tmp` deletion outside native
shutdown. Each cleanup has a 30-second native deadline; failure, timeout or an
unsafe traversal root fails the native unit and is visible through its Result
and journal. Readiness does not depend on that Result: no apply can change a
previous boot's outcome, and runtime ageing removes what an interrupted cleanup
left. The CLI's stopped message confirms physical power state, not cleanup
success. A hard stop provides no shutdown-cleanup guarantee; an already stopped
yard remains stopped. Journals across reboot depend on guest journal persistence.

Runtime cleanup is hourly, with an initial check 15 minutes after boot, and only
ages the explicit opt-in `/tmp/subyard-disposable` tree. Files and directories
whose modification time is older than one day are eligible. Active producers
must hold a BSD `flock` on their own descendant operation directory for the full
operation; a lock on the configured root or an ordinary open file descriptor
does not provide that protection. Fresh or locked data is preserved. Vendor
tmpfiles rules and the vendor clean timer remain unchanged.

A separate hourly `subyard-tmp-age.timer` ages generic `/tmp` content; its first
run is 15 minutes after boot or installation, so installation itself removes
nothing. It invokes `systemd-tmpfiles --clean` with an explicit
`/etc/subyard-guest-tmp-age.conf`: an entry is eligible when it is older than two
days by its access, modification, change and birth times (directories: all but
change time). Before each run it reads the working directory, root, executable,
open descriptors and file mappings of every process, and the socket table of every
network namespace, and excludes each top-level `/tmp` entry they reference, comparing bytes in the C locale with every byte outside
`[A-Za-z0-9._-]` widened to a `?` glob. It refuses to run without that view: when a
process other than one exiting during the scan cannot be read. tmpfiles itself
keeps flocked files and directories, bound sockets and mounts; the policy also keeps
the opt-in disposable tree, every exclusion and directory under `/tmp` the
installed tmpfiles configuration declares, and the vendor exclusions: current-boot
`systemd-private-*` service directories, SSH agent directories, X server locks and
the X socket directories.
A path another mount namespace maps from `/tmp` under a different name is not seen;
it is kept only while it stays fresh. On storage mounted `noatime`, reading a file
does not keep it fresh. The run has a 15-minute deadline at idle I/O
priority; an interrupted run resumes on the next hour. Service runtime directories
should normally use systemd `RuntimeDirectory=` instead. Cleanup refuses symlink, foreign-owned,
incorrect-mode or mounted traversal roots, including a bind mount at the opt-in
root. A transitional live tmpfs `/tmp` also refuses whole-root cleanup until the
requested restart activates disk backing. `/var/tmp`, `/srv`, credentials,
workspaces and operation-owned producer scratch are outside blanket cleanup.

To roll back cleanup, disable `subyard-tmp-boot.service`,
`subyard-tmp-shutdown.service`, `subyard-tmp-clean.timer` and `subyard-tmp-age.timer`
inside the selected guest, then stop the timers and shutdown service. The latter
reports a deliberate refusal when manually stopped; active temporary files remain
intact. After verifying exact policy content, root ownership and modes, remove only
those four unit files, `subyard-tmp-clean.service`, `subyard-tmp-age.service`,
`/usr/local/lib/subyard-guest-tmp`, `/usr/local/lib/subyard-guest-tmp-age`,
`/etc/subyard-guest-tmpfiles.conf` and `/etc/subyard-guest-tmp-age.conf`, then reload
the manager. A release rollback to 0.32.x keeps the ageing timer running: that
release neither refuses nor manages its files, which are removed as above. Keep
the disk policy's `tmp.mount` mask unless separately reverting disk backing. Repeating initialization restores cleanup without clearing live
data. Foreign policy files or unit overrides are preserved and refused.
Route-consumer ownership conflicts identify the affected yard, validated native instance
and route device, with a read-only Incus inspection command. Configuration inspection
failures retain safe source context and direct operators to the affected yard's config status.
Materialized-config observation failures identify the yard and inspection phase, with a
read-only config or integration status command. Known integration ownership conflicts retain
their validated path and inspection command across the release boundary; they must not send
`migrate --check` back to itself as the only next step.

Each compiled capability classifies its bounded resources as preserve, transform, canonicalize,
reset or block before confirmation. An authorized reset is a successful, journaled one-time result;
unknown or ambiguous state produces a structured operator-action outcome without overwriting it.
Settings migration classifies direct yard templates against the verified candidate's configuration
catalog and shipped template contract. Supported templates outside the migration's resource scope
are preserved byte for byte; unknown, dynamic or duplicate template assignments still block.

`yard migrate --check` reports readiness of the exact installed `current` release: per-domain
recorded and required epochs, applied and pending migration IDs, transaction/step checkpoints,
runtime reconciliation actions, the verified owner's consequences (including per-yard selection
and source explanations), and blockers. Add `--json` for the versioned machine-readable report;
its optional `consequences` field preserves those explanations when the owner supplies them.
A repair of the active release explains the installed selection it observes as
`from installed configuration (applied record)`; desired init-owned selection changes are not
activation work there and appear in the `yard migrate` plan's pending `yard init` listing.
A successful inspection exits 0 even when work remains; inspect `outcome.status` for
readiness. Unavailable or unsupported inspection never means ready.

`yard status` also reports installed release readiness for each yard's owner host:
`release OK (ready)`, `release NOT OK` with the state, reason and next action, or
`release unknown` when verification is unavailable. This is the same read-only
inspection as `yard migrate --check`, bounded to 30 seconds. It never downloads a
candidate or repairs the installation, and a non-ready release does not prevent
the remaining yard status from being displayed. Remote status requests the
`release-readiness-v1` capability separately from inventory, so a cached yard
inventory cannot turn old release readiness into a current `OK`. Older owners or
unreachable release observers report `unknown`. Readiness describes the installed
release; it does not guarantee compatibility with a future update candidate.

Migration history has a separate schema 1 compact representation at
`release-transition/v2/history-checkpoint.json`: registry provenance, per-domain epoch, compacted
prefix boundary and digest, and applied suffix. Before publication, `release-transition/v2/ledger.json`
is authoritative; afterward the checkpoint is authoritative and the legacy ledger remains a
byte-identical, read-only compatibility projection. Each read checks its recorded existence,
fingerprint and epochs. Changed or corrupt projection data blocks rather than becoming a new baseline.
Prefix proof is validated against the trusted registry, including an explicit retired-prefix proof
when a future registry raises its floor. Suffix completion updates only the checkpoint through CAS;
after verified readiness, the assessed transition compacts that completed suffix into its prefix proof.

The separately sealed optional [`config/release-checkpoint.json`](../config/release-checkpoint.json)
marker selects `migration-history-checkpoint-v1` writer support from the verified owner assets.
Retained assets without that marker do not enable initial conversion; a new engine always reads an
existing checkpoint. Marker selection does not extend process V1 or journal V2 or authorize conversion under
an older grant. Initial conversion requires an assessed, authorized transition after verified
readiness, preserving unfinished journal bindings. The checkpoint capability and a stable
source-ledger fingerprint bind conversion consent into the existing
`ObservationScope`; no journal field or separate authorization artifact is added. Resume accepts
either the exact legacy scope, which preserves the old grant without conversion, or the exact marked
scope, whose conversion consent survives restart. An ordinary marked update includes conversion in
its single assessment and consent; completed legacy history requires a fresh assessed grant.
Publication runs under the shared lock after verified readiness. Failure before or after atomic
publication preserves one authoritative record and a retryable path. The main registry retains its two migration IDs.
Older released binaries consume the frozen projection; acceptance must prove delegation to the
checkpoint-capable owner rather than assume they decode the compact format. See the
[migration retirement contract](reference/resumable-release-transition-design.md#migration-retirement).

Before advancing an existing checkpoint, the exact current/from release must itself carry the
verified, sealed checkpoint-reader marker. An unaware current release must first activate a
compatible reader bridge at matching checkpoint and projection epochs; sealing only the future
target does not satisfy this prerequisite. Initial conversion without an existing checkpoint can
record the full migration result in the legacy projection. Retaining that projection does not make
rollback eligible after checkpoint epochs advance. The checkpoint-aware runtime boundary validates
authoritative history against the exact sealed target registry before dispatching rollback to an
unaware retained target. Its projection must match the checkpoint epochs and pinned data; the captured
checkpoint and projection must remain unchanged through execution. The original registry-based
transition owner, catalog and journal bindings are preserved. A marked target validates the checkpoint
itself; a target without a transition registry retains the existing active-owner route.

`releasetransition.HistoryCompatibility` decides whether a target release may own the active
migration history. It compares the parsed registries by canonical digest (`identical`, `differs` or
`absent`), and `PlanDownPath` relates them. The relation is `identity` for one registry, or
`equivalent` when one registry is the other with completed prefixes retired: the unretired registry
retired through the other's floors (`RegistryV2.RetirePrefixes`) has the retired one's canonical
digest, so the unretired registry's own `MigrationPrefixDigest` proves every retired digest and
later definitions are unchanged. In either direction the selected target must parse the active
registry before it can serve a request. `PlanDownPath` calls the registry parser with the target's
sealed `registryFeatures`; a missing or unknown feature retains the v0.30.0 parser rules. A
registry unreadable by that target refuses with `registry-unreadable`, together with the registry
refusal when the pair is also undeclared.
The underlying relation remains available so unreadability cannot hide independently determined
declaration or first-publication refusals.

The current parser first accepts every registry readable under the v0.30.0 rules unchanged.
Otherwise, `retired-prefix-dependencies` admits only a whole registry canonically equal to the
frozen v0.30.0 registry retired through its own floors. Every dependency of a retired migration
must also be retired; an unclosed set refuses entirely. The same proved retired IDs reserve names
and satisfy surviving dependencies. Foreign definitions, reused IDs and foreign prefix digests
cannot expand this closed language.
The feature's meaning is frozen while any release declaring it can be a rollback target.
Of the 71 nonempty v0.30.0 retirements, the golden enumerates
25 accepted forms: 5 strict forms and 20 additional dependency-closed forms. This release keeps
all historical definitions and handlers; their removal belongs to a later release.
Any other relation is `refuse`; `steps` is not produced
yet. For an admitted relation it then reads the authoritative ledger or checkpoint through the
target registry and its checkpoint-reader marker. Checkpoint reading checks prefix digests and
epochs, not the writer's registry digest, so either release of an equivalent pair reads history
the other wrote, and an equivalent admission writes nothing. The verdict carries the kind, the
registry relation, whether the target is the retiring `successor`, and the collected refusals; a
verdict that is not admitted always carries a refusal. `identity` is admitted. `equivalent` is
admitted only within a declared pair: rollback to the predecessor the active release declares, or
the return to a successor that declares the active release as its predecessor. An undeclared
equivalent target, such as an older release, keeps the registry refusal and is not read. Callers
that decide by the registry read `RegistryAdmitted`, so an admitted equivalent target behaves like
identical registries for source-floor advice, first-publication evidence and bootstrap
publication, and an undeclared one like differing registries. Only an equivalent relation depends
on the newer release's predecessor declaration. The target's reader declaration must also be
verified; a failure is collected with independently known registry and history refusals at the
runtime boundary. The engine applies the same verdict to its own and the rollback target's
parsed registries and its own declaration; identical sealed bytes stand in for a target registry it
cannot parse. The sealed
[`config/release-rollback.json`](../config/release-rollback.json) declares the canonical registry
digest of the release's one rollback predecessor. Its migration and profile-format lists are empty
in schema 1. Its optional `registryFeatures` declares compiled registry readers, bound by a repository
test to `RegistryReaderFeatures`; packaging adds no capability check. It is verified from the sealed manifest like the checkpoint marker, and a release
without it declares no predecessor. A release parses its own declaration strictly. Another
release's declaration, which can come from a newer release, is read for
`predecessorRegistryDigest` and known `registryFeatures`: unknown features, fields, list entries
and schema versions above 1 are tolerated, while the size bound, a single JSON object, a schema version of at least 1 and a
well-formed digest are still required. Later schemas therefore keep that field and its meaning.

### Declared rollback boundaries

The newer active release performs rollback. Its sealed `config/release-rollback.json` may add
`boundaries` to declare blocking state changes or accepted agent integration cleanup limitations.
Each entry has:

- `component`: `registry`, `profile:ACTIVATION_ID`, or `agents`.
- `throughVersion`: the last affected published version, without `v`.
- `reason` and `recovery`: single-line explanations, at most 512 bytes each. Recovery describes
  the manual next step; it grants no automatic cleanup authority.
- For `agents`, `sourceDigest` binds the reviewed integration sources and `files` lists the
  affected yard paths (absolute or `~/` relative to the yard developer's home), sorted and unique.

Sort entries by numeric semantic `throughVersion`, then `component`. Retain a boundary while
incompatible releases remain possible targets. For `registry` and `profile:ACTIVATION_ID`, an older target at or below the
version is refused with `rollback-incompatible`, the reason and a manual recovery hint. This
applies both to `yard update --rollback` and explicit older version selection, before profile
preparation or release mutation. Independent registry, configuration, VM and profile findings
remain visible. These blocking boundaries do not make incompatible history readable. A blocking
declaration is not bypassed by selecting an older version or running bootstrap.
Keep the newer release active until a repair release supplies a supported down path, or restore
a complete pre-upgrade backup into a clean installation of the older release. Restoring only
release binaries is insufficient; restore the corresponding settings, history and yard state.
Newer targets and same-release repair are unaffected.

An `agents` entry is a **non-blocking accepted limitation**. Rollback and profile preparation
proceed through their existing compatibility checks. The operator-facing plan output and result
show the affected yard paths and manual cleanup instructions, also documented in release notes.
When the retained release cannot plan the rollback, the refusal names these paths and recovery
instead of the generic next step, because a file it refuses there is the likely cause.
Agent declarations neither authorize automatic removal nor add integration rollback hooks. Stop
agent sessions before rollback and complete the documented cleanup before restarting them.

`TestPublishedRollbackGate` compares the current source to the latest version in
`dev/e2e/published-upgrade-baselines.tsv`. Its checked-in snapshot keeps this check offline and
usable in shallow checkouts. After adding a published baseline, fetch its release tag and run
`python3 dev/update-rollback-baseline.py`; review the snapshot and registry against the published
release. The test rejects a stale snapshot. There is no additional packaging-time gate.

A changed registry must pass the shared `HistoryCompatibility` / `RegistryAdmitted` rule,
including the target reader features and the predecessor declaration of whichever release is
the retiring successor, or carry a `registry` boundary. A format change for an activation ID
present in both releases needs its existing `prepares_rollback_to` contract (whose handler owns
`rollback-observe` and `rollback-apply`) or a `profile:ACTIVATION_ID` boundary. Introduced or
removed runtime owners are skipped, matching runtime admission. Agent integrations have no
rollback hooks: changes to tracked inputs in the existing integration asset inventory need a
reviewed non-blocking `agents` declaration with manual cleanup. This covers integration metadata,
package provisioning and lifecycle sources, every declared artifact and approval variant, shipped
configuration defaults, the usage provisioner and `config/projects-changed.sh` (installed as
`/usr/local/libexec/subyard/projects-changed`). Referenced assets under `tests/` are included;
unreferenced and ignored files are excluded, while untracked files count, as the release bundle
ships them. Inventory records use sorted full repository-relative paths, content hashes and the
Git executable flag. Both the gate and snapshot
updater use the same Go inventory. Its failure prints the current source digest; a later source
edit requires renewed review and an updated digest, so an old declaration cannot silently cover new files.

For example, an integration that replaces the shared Claude policy with drop-ins
`48-`, `49-` and `51-` adds their exact paths under
`/etc/claude-code/managed-settings.d/` to `files`, updates `sourceDigest`, and documents stopping
Claude, backing up and removing those Subyard-owned drop-ins, then restoring the previous
release's policy after rollback and before restarting agents. List complete filenames, not globs; preserve
operator-owned policy. The current declaration covers the approval hooks and managed agent
configuration and project dispatcher changed or added since 0.30.0. Review cleanup inside each
affected yard, including custom installation paths. v0.31.0 shipped these agent changes without
this declaration; the next release introduces it. `throughVersion: "0.30.0"` describes affected
rollback targets, not its owning release.
It does not refuse the supported rollback to v0.30.0.

Release selection compares the profile runtime formats that the active release and an older or
equal target declare for each activation ID. A newer target is exempt only when both versions are
established. `yard update --version` refuses any difference, and so
do both routes when a profile set cannot be read. When the target is the retained release and the
active release prepares that format, the next step is `yard update --rollback`, the only route down.
A rollback refuses a difference that the active release does not prepare. The other owners are
observed with the rollback compatibility findings. The active release's `rollback-observe` runs for
each owner in every local yard, from the verified active root. `absent` passes. `current` passes once
the target's own `observe`, from its root, accepts the prepared state; otherwise the next step is
`yard migrate`. A stopped yard refuses with `yard -Y NAME start`, and a failed observation refuses, with the owner's
own refusal and next command when it reports one.
`stale` needs preparation. Each local yard's settings are also read with the target release; settings
it cannot read refuse before any conversion. These findings are reported together with the retained
release's installed-runtime check.

Rollback through a profile hook takes two commands. When every other rollback admission passes and
only owners that need preparation remain, the first `yard update --rollback` plans the
`update.prepare-rollback` action. That plan shows one conversion step per owner and declared
consequence, and nothing else. Each step shows its consequence whole and names its yards, shortening
only a long yard list; the confirmation binds every yard. A preparation with more steps than a plan
can show is refused before any conversion. Before the first conversion, execution reruns the same
rollback admission, which must offer exactly the confirmed plan; a refusal converts nothing and keeps
its own next step, such as `yard -Y NAME start`. It then runs each `rollback-apply` that this
admission observed from the verified active root, with each yard's settings read again there and
still naming the confirmed instance, under the yard's integration lock. The apply assessment also binds the confirmed
consequence. Afterwards the admission runs once more as the gate,
and must offer the ordinary rollback that the second command would plan. The command keeps no journal
or update history and stops with the active release active. It reports `migration-required` with the
next step `yard update --rollback`.

The conversions can be resumed by rerunning that command, and abandoned with `yard migrate`, which
repairs the owners forward. The active release now reports the prepared owners as drift. A rollback
from that state is admitted only when the active release's journal is complete without blockers, its
other decisions only preserve, and every unconverged activation owner is one it prepares for the
retained release. Yard attribution is not consulted: `rollback-observe` alone routes each yard. `stale`
continues the preparation of that yard, a stopped yard or a failed observation refuses with that
yard's next step, and `current` or `absent` everywhere leaves the ordinary rollback. The second
`yard update --rollback` is then the ordinary rollback with its own confirmation. Its execution
observes the prepared owners again before the target runs. Data is not converted.

`yard update --rollback` runs in the newer release, which prepares the state for the older one;
afterwards the older release reads only its own files, so new fields in the newer release's metadata
do not block that rollback. The older release refuses only its own `yard update --rollback` back to
the newer release; the way back is `yard update --version VERSION`.

`yard-engine _history-compatibility RUNTIME_ROOT CONFIG_HOME TARGET_RELEASE` prints this verdict as
JSON for a release published under `RUNTIME_ROOT/releases`. It assembles its input like the
runtime's installed-engine selection, including first publication. Run it from the active
release's verified engine, `RUNTIME_ROOT/current/bin/yard-engine`: that engine selects rollback
targets and owns the declaration, while older releases lack the verb. It needs no repository,
takes no lock and writes nothing. Read `admitted`, `kind`, `registry`, `successor`,
`declaredPredecessor`, `declaredSuccessor` and `refusals`, with their
causes, from the JSON instead of relying on the exit code: 0 means admitted and 1 refused, while 2
covers usage and verification errors, including a missing config home, and prints no JSON.

`yard migrate` assesses and confirms the necessary work, then uses the same verified transition
owner, authorization, lock, journal and convergence engine as `yard update`. It does not download
or publish a release, choose a version, or rotate runtime links to another release. Completed
migrations stay completed; runtime drift gets a new repair transaction. A ready installation
needs no confirmation, and successful apply exits 0 only after verifying readiness.

This is a host-wide command for local yards; run it without a yard selector on the owner host.
It remains reachable without loading unrelated yard configuration. `--yes` supplies automation
consent; release-selection and rollback flags are not accepted. If an unfinished update still
needs to activate another release, the report directs the operator to `yard update`. Once that
target is current, `yard migrate` can resume the existing transaction. Retained rollback recovery
continues to use the journal's verified transition owner. Owners that cannot verify runtime
reconciliation require an update to supported tooling.

The [runtime installer](../scripts/install-runtime-release.sh) verifies, unpacks and publishes an
immutable candidate. It may create the first `current` link during a clean bootstrap, but it does
not activate an update, perform rollback, or invoke mutating `_migrate` verbs. Installed update,
same-version retry, source import and explicit rollback all use the candidate-owned
`_release-transition` process contract. Rollback is a newly inspected `activate-previous` goal and
requires an intact retained runtime within its compatibility horizon.

Fresh rollback with pending settings, owner-registration or source-install changes also requires
the retained owner to advertise safe prospective source activation. An unsupported owner refuses
with `rollback-incompatible` before consent or mutation. Complete current-release migrations
with `yard migrate`, then retry `yard update --rollback`; otherwise retain an owner with
compatible rollback source planning. Retrying the unchanged retained owner cannot add that support. Compatible rollback without source work remains available. An already-authorized
transaction continues with its original pinned owner rather than acquiring a new grant.
Retained inspections can omit registered-yard and guest-disk work from their Decisions.
For these opaque kinds, the caller checks the exact retained registry and authoritative
ledger/checkpoint. Pending opaque work requires the same capability even when it may
ultimately be a no-op; completed opaque work does not. A stopped yard with an absent disk-/tmp
marker alone does not block rollback once these ledger epochs are complete. Unverifiable
history refuses rollback.

An installed runtime whose bundled installer predates immutable `--publish-only` cannot be changed
retroactively. Re-running the verified standalone bootstrap is its one-time bridge: the downloaded
candidate runs the ordinary assessed `ReleaseTransition` from a verified private draft.
After confirmation, update reverifies the plan and publishes the exact draft before convergence.
Atomic publication requires the draft and live release store to share a mount.
Bootstrap validates runtime ancestry on every path before staging or running release code.
For first install, update, and `--publish-only`, bootstrap checks the bundle's SHA-256 and provenance
and queries its engine from a sealed execution image before writing the cache or runtime. The
manifest's `bootstrapDraft` field allows an early refusal. Online verification uses private
temporary files and removes them on return. Older releases use their own tagged bootstrap asset.
For v0.30.0, download `subyard-install.sh` from that release and run
`bash subyard-install.sh --version 0.30.0`. Its published assets include the bootstrap, and its
tagged script defines `--version`. [v0.30.0 release assets](https://github.com/Subyard/Subyard/releases/expanded_assets/v0.30.0) · [v0.30.0 bootstrap source](https://github.com/Subyard/Subyard/blob/v0.30.0/dev/bootstrap-runtime.sh#L47-L63)
Publication-only staging first appeared in the v0.18.9 installer.
[v0.18.9 bootstrap options](https://github.com/Subyard/Subyard/blob/v0.18.9/dev/bootstrap-runtime.sh#L17-L29)

A candidate already in the live release store still has to meet retained-release compatibility.
If a prior attempt left a published, incompatible candidate without a matching authorized
transition, keep the active release and select a different verified forward release.
The superseded runtime is never allowed to call mutating `_migrate` verbs against the candidate.

The cross-release contract is independent of migration implementation types. The frozen
[`protocol/v1`](../internal/releasetransition/protocol/v1) module defines the exact JSON fields,
enum values, bounds and confirmation semantics understood by the released v0.11.2 updater.
Explicit adapters project internal requests and results onto that contract. A V1 request receives
a V1 response; adding internal fields or outcomes must not silently extend the V1 wire format.
Unknown protocol versions are rejected before authorization-channel access or transition work.
Strict decoding remains part of the contract.

A completed journal followed by activation drift needs a fresh plan and authorization. The
released V1 protected caller requires the historical transaction ID, but rejects that ID on
`migration-required`. Its compatibility inspection therefore uses `recovering/recovery-pending`
with the historical ID, a fresh `plan-v1` token, a changed assessment, and **no `resume`**. This
presentation does not resume or authorize completed history: apply creates a new transaction,
and only an unfinished transaction can reuse its grant. The current caller validates the historical
identity and actual links, then restores the canonical `migration-required` presentation. The
Module's ordinary `Inspect`, journal bytes, ledger, and convergence responses retain their semantics.

The frozen [`journal/v2`](../internal/releasetransition/journal/v2) module similarly owns the existing
durable journal representation, including nested evidence and archived predecessor journals.
Canonical field order, omitted fields and the trailing newline are preserved because fingerprints
bind those bytes. An old updater must still be able to read the journal and select its verified
transition owner after interruption or rollback. New migration internals do not add fields to that
shared representation automatically; runtime-specific recovery changes require an explicit,
compatible storage design.

Changed inputs in an unfinished activation-only transaction have a bounded fresh-plan path.
It requires a forward `reconciling` journal with no source ingress or migration steps, the target
already active, exact runtime links, an unchanged verified owner/artifact/registry/catalog and
a completed migration ledger. Each drifting activation owner must provide a safe native plan
that validates current ownership; observation hashes alone do not prove safe partial application.
The fresh assessment binds the predecessor journal, ledger, links, actual state, desired scope and
native plans. `yard migrate` or `yard update` presents its consequences and requires new consent;
the predecessor's grant and resume token cannot authorize changed inputs.

This path uses the separate strict process schema 2, negotiated against the same sealed owner. Ordinary calls
remain V1. Owners without that capability retain exact-input resume and never receive replacement
writes. A V1 retained caller can inspect and resume the successor through its verified owner.
The journal stays canonical V2. The original `capabilities` probe advertises only
`activation-only-replacement-v1`. Immutable receipt schema 1 under
`release-transition/recovery/v1/transactions` stores the canonical predecessor and initial successor,
ledger fingerprint, owner, links and native plan bindings outside the frozen V2 cleanup graph.
Under the shared update lock, the owner rechecks the assessment, publishes the receipt, then uses
CAS to replace the exact current journal. A crash before CAS retains the original journal and
reuses the exact receipt after fresh authorization; after CAS, ordinary resume completes the
successor. Links and completed ledger bytes stay unchanged. Replacement chains, settings journals
(including verified steps), source migrations, foreign state and unproven partial apply remain
excluded. The protocol does not reconstruct original inputs from a hash or reset settings.

Historical migration-bearing owner replacement used `migration-owner-capabilities` and
`migration-ledger-owner-replacement-v1`. The current engine retains the capability probe and
reads its immutable receipts under `release-transition/migration-owner/v1/transactions` and
successor completion records. Receipt validation binds the exact official Linux amd64 v0.21.0
manifest and registry, canonical predecessor and initial successor, frozen projection/checkpoint,
completed evidence and any retained source/previous-release transactions. These references remain
protected from cleanup; an unfinished predecessor never becomes activation-ready history.
The current engine does not select or create that historical replacement. Its registry differs
from the v0.21.0 registry: that recovery belongs to a verified historical standalone candidate
through v0.25.1, followed by the v0.28.1 hop. Unknown artifacts, conflicting graphs and foreign
state still refuse. Frozen wire meanings and retained rollback checks are unchanged.

New callers first probe `lifecycle-capabilities` for `activation-only-replacement-v2`, and
fall back to the original probe when it is unsupported. Only a successful lifecycle probe selects
the explicit request `contract`. Receipt schema 2 uses the `recovery-v2-` transaction prefix and
`release-transition/recovery/v2/transactions`; canonical terminal records live under its sibling
`archive`. Protected mutable `frontiers/<original-transaction>.json` records the exact predecessor,
generation and latest authorized cancellation witness. The opaque successor identity binds that
predecessor fingerprint and generation. The plan binds current native inputs and the exact frontier
reservation, so returning to earlier desired inputs cannot select a cancelled generation. Fresh consent
can cancel that reservation only while the exact predecessor is still selected and no current,
prepared or ordinary archived journal references the successor. Only an exact prepared Authorized
successor CAS file can be invalidated. Cancellation evidence is published durably, the frontier
advances by exactly one generation through CAS, then exact live/pending records are removed and
fsynced before a new receipt and journal CAS. Pending frontier state must prove the exact next
generation. After interruption, changed inputs bind the existing cancellation witness and need a
new grant; apply finishes unchanged durable intent without restoring earlier desired inputs.
Read-only inspection and declined consent leave state unchanged. Generation exhaustion blocks.
Legacy V1 reservations keep their exact-input retry
semantics and are never cancelled automatically. Neither contract permits replacement chains.

Generationless V2 receipts from an earlier prerelease candidate are unsupported by the current
frontier contract. Preserve their exact journal, receipt and verified draft owner/assets; resume
through that original owner rather than adopting or rebaselining the evidence. The checked local tag
history has no tag containing the generationless change `382a4aa`, and the v0.17.8 tree has no recovery
contract implementation. This is local-tag evidence, not verification of the published release API.
Released compatibility gates cover the supported frozen V1 baselines separately.

Completion evidence captures the exact completed successor journal before overwrite. Exact predecessor
completion can instead close a never-selected receipt; absence from current state alone supplies no
proof. V1 live receipts retire only with terminal proof and no required predecessor/successor references;
their reservations are never cancelled. Cleanup runs under the shared lock after readiness and before
admission, with a 32-record budget per phase. Older cancellations compact before readiness only when
the frontier preserves the latest witness and retired-generation proof. Current, pending and ordinary
archived journals and live reservation bindings remain roots. A retained release ID alone does not pin
every transaction it once owned; verified rollback eligibility and exact unfinished owners remain
separate requirements. Unreferenced terminal history is trimmed toward 32; protected history can exceed it.

Modern live, archive and frontier namespaces each allow 512 logical records and 32 MiB of physical
record bytes. Matching published/pending pairs count once logically; both files consume bytes. The
ordinary frozen V2 transaction graph and V1 live namespace retain 256-record limits. Eligible cleanup
precedes admission, including at archive capacity; protected exhaustion, unknown or conflicting state blocks.
An active V2 successor validates its own terminal record directly, so unrelated historical archive
corruption blocks cleanup or new admission without blocking its exact resume. Retirement failures
leave evidence intact and add `recovery cleanup is pending` to the ready outcome. Failed terminal
capture retains live evidence and reports pending cleanup even when a completed journal is replaced.
There is no age-based deletion or background cleanup daemon.

The release-transition journal is authoritative recovery state, not an operator transcript. A
separate structured update history under `$SUBYARD_HOME/logs/updates` records each committed
activation or rollback attempt, direct preparation failure, and declined confirmation with a unique
attempt ID, operation ID, direction, bounded phase events, verified source/target identity when
available, and a safe terminal status/code. It retains the newest 30 attempts and never copies hook
output, error text, environment values, or journal JSON. Local `yard logs --updates [-n N]` reads
that history and `yard logs --audit [-n N]` reads the current command audit file plus five retained
1 MiB rotations without loading yard configuration or Incus. Each `yard rpc --stdio` owner also
appends one record per failed request (time, request and operation ID, method, code and the full
local error text) to the private `$SUBYARD_HOME/logs/rpc-errors.log`, keeping one previous
256 KiB generation; `yard logs --rpc [-n N]` shows the newest records. Records are best effort:
the session queues at most 64 and writes them after the response under the owner barrier of the
installation generation it started in. A record that cannot take the barrier and the log lock
within 250 ms is dropped, as are records beyond the queue.
Explicit yard selectors continue through normal owner routing.

Introduce a new protocol by first shipping support alongside V1 while continuing to send V1.
Only a subsequent release may replace the default protocol with owners that support it. An optional
same-owner recovery extension may use its separate contract only after an explicit capability probe
and proof that retained V1 callers can read, select and resume its unchanged V2 successor. Retain
the old reader, writer and semantics throughout the supported upgrade, retained-runtime rollback
and unfinished-transaction recovery horizon. A runtime version bump alone never changes these
contracts. Do not weaken validation or silently discard a new safety requirement to fit an old
response: the compatibility adapter must preserve its meaning.

Before publication, `dev/verify-release-upgrades.py` runs unmodified checksum-pinned old callers
through update and `--check` refusal, verifies the hop instruction reaches stderr, and checks
that protected configuration, active links and project data remain unchanged. The pre-anchor
migration and original-owner recovery matrix is frozen in the v0.28.0 tag. Current Go reader tests
retain accepted V2 history and recovery contracts; installed direct upgrade, rollback and the
one-time hop chains belong to the published-upgrade and P0 VM lanes.

Materialization no longer composes preview instructions or copies host instruction files;
guest capability skills own preview guidance.
The Release workflow retains the compatibility check's output with its other failure diagnostics.

### Legacy upgrades

Current code carries migrations only for direct updates from the anchor release
[`UpdateAnchorVersion`](../internal/adapters/releaseruntime/source_policy.go) (v0.28.0) or newer;
older sources must first take the hop below. Direct updates, the installer over an existing installation and offline update require installed
**v0.28.0 or newer**. Sources from v0.11.0 to below v0.28.0 refuse before any change and print
`yard update --version 0.28.1 && yard update`: the hop is the v0.28.1 fix release, because the
anchor itself can stop with a running VM yard (see below). After later boundary moves the refusal
prints every hop from the installed version. Rollback targets must be at the current
migration epochs: since the NetworkManager device guard (`power-metadata` epoch 4) that excludes
v0.29.0 and every older release. The
[approved support boundary](reference/legacy-migration-support.md#approved-support-boundary)
owns the complete rules, including unfinished transitions below the anchor.

Sources before **v0.11.0** retain the full-uninstall route; no V1 upgrade bridge exists. Accepted
V2 history and evidence remain readable independently of fresh update admission.
Supported installations are assumed to have no V1 journals. Ordinary commands, including
configuration repair and source-history assessment, ignore `migrations/state.json` and
`migrations/transactions/` without reading or deleting them. There is no V1 mutation gate;
stray V1 files are repaired manually if needed. Current tooling refuses an unfinished V2
`legacy-v1-import` journal with `compatibility ingress operation is unavailable` before writes,
including retained-owner inspection, resume or replacement. Completed V2 legacy provenance,
evidence, recovery bindings,
history checkpoints and projections retain their existing validation and readers. The static
`config/migrations.json` header remains for old released callers' actionable refusal.
An installed v0.28.x updater's completed-metadata fallback can refuse an installation that
completed V2 import but retained committed V1 files. An unfinished original V1 journal under
completed V2 import can cause an earlier v0.28.x source-history refusal; the same manual step
applies. Once no update is running, move aside only `migrations/state.json`
and the entire `migrations/transactions/` directory to a private backup outside the configuration
home, then retry; no other cleanup or reinstall is needed. Keep `migrations/update.lock`,
`release-transition/` and all other data in place. See the
[retained committed V1 workaround](reference/legacy-migration-support.md#retained-committed-v1-files-with-v028x-updaters).
For a source before v0.11.0, stage verified current cleanup tooling in its existing runtime store:

```bash
VERSION=<current-version>
curl -fsSL "https://github.com/Subyard/Subyard/releases/download/v${VERSION}/subyard-install.sh" \
  | bash -s -- --version "$VERSION" --publish-only
"<printed-candidate-path>/bin/yard" uninstall --full
```

Keep the existing installation's configured data, config, runtime and cache roots when staging
and running the printed candidate. Supply `--runtime-root <existing-runtime-root>` when it is
custom. Staging publishes immutable tooling and its ownership inventory; it does not activate,
upgrade or migrate the old installation. The full removal preview identifies owned resources and
asks for destructive confirmation. Full uninstall removes the installation's owned yard data,
settings, credentials, sessions, backups and runtime. Unknown or foreign ownership refuses the
relevant deletion; names and an old version alone do not prove ownership.

Full teardown accepts retained producers only from the current approved source. A reserved
owner name or a former registration does not widen that source's labels; an unproved producer
refuses removal. The pre-v0.11 test yard `e2e-yard` is never loaded or updated by release
activation; readiness warns with its recreation commands and ordinary yards still update. Its own
`init` refuses until the operator runs `yard -Y e2e-yard teardown --reset-config`, then
`yard service install test-vms`.

Legacy installations may lack creation receipts for host directories. Exact native association
can establish a registered legacy yard's bounded project/instance/volume scope; unrelated native
resources or conflicting ownership refuse that scope. Unrecorded legacy host namespaces, including
backups, sessions and host mounts, and shared infrastructure without installation ownership remain
preserved and are listed in the plan and final result. Full removal does not claim those paths
were deleted. Modern receipt-owned namespaces retain their existing deletion and stale-plan guards.


Full uninstall works independently of migration readiness, including pending or invalid V1/V2
journals. If interrupted, follow its printed recovery command and review the retained plan before
confirming continuation. After verified cleanup, stop. Install the desired version separately;
cleanup does not preserve data or automatically reinstall. See [full removal](configuration.md#full-uninstall)
and the [support inventory](reference/legacy-migration-support.md).

### Yard-scoped runtime failures

A profile runtime may declare `"failure_scope": "yard"`. Its typed failure affects only one
running yard: the release completes and that yard waits for its own init. A failure is typed only
when the declared handler exits non-zero from `apply` and writes exactly one stderr record
`subyard-profile-runtime-diagnostic-v1 NONCE apply|verify yard-local`. `NONCE` is the value of
`SUBYARD_PROFILE_RUNTIME_DIAGNOSTIC_NONCE`, which the core creates for each apply and the handler
must not pass to guests, so relayed guest output cannot forge a record. The record shares the closed
parser and reasons of the lifecycle diagnostic protocol; an `inspect` record, a duplicate or
malformed record, a record from an undeclared owner, cancellation, a timeout, output overflow and
a mismatch that the core finds when it verifies the result after a successful exit stay blocking.
So do plan-stale refusals, core runtimes, host and installation owners and any other failure.
A handler reports `yard-local` only when a guest-side command itself ran and failed and the guest
still answers; Incus transport errors, host staging, file pushes, safety guards and a failed cleanup
in the same apply stay blocking. The Orca handler proves this by running each such command through a
guest wrapper that maps the command's failure to status 200, which Incus never returns.

The owner records the typed failure under the configuration root as
`activation-pending/v1/YARD/OWNER.json`, bound to the yard instance, the actual and desired
fingerprints the failure left and the digest of the profile package (the declared handler and
every resource it may execute, except `tests/`), and continues with its other yards. Shared core
scripts are not part of that digest.
Every observation matches records in any journal state: a stale yard with a matching record is
converged with a pending finding, and any other stale yard, including a recorded one whose facts
changed, is unconverged. The release journal therefore completes only through the engine's
ordinary verification, for the repair of the active release and for the activation of an ordinary
update alike. A blocking failure keeps the release recovering; its recorded yards stay findings.

The engine alone turns findings into release state. Completed history of the goal whose every owner
was observed and converged reports each finding as an `activation.<owner>.pending-configuration`
blocker with that yard's init step; a release ready proof never accepts findings. While other
release work remains, findings are warnings, so `yard migrate` still plans and repairs that work.
`yard migrate` and `yard update` then exit non-zero; their result names every finding within the
diagnostic bound and states that the release is ready, and `yard migrate --check`, `config status`
and `yard status` show each one. A finding names the yard, the profile, the diagnostic command
`yard -Y NAME profile list`, that the profile is not applied, and its exit: fix the cause and run
`yard -Y NAME init`, which repeats the same apply from the partial state. Its next step is the
exact `yard -Y NAME init`. When those exact commands exceed the 480-byte record message, the
finding names the yard once with the same steps, and a yard name too long even for that is left
to the next step, so the failure is still recorded.
The handler may print its own further exit on its stderr beside the typed record; the core
message stays generic. Orca names `yard -Y NAME orca remove` and then `yard -Y NAME init`: the
resource verb is a profile resource mutation, so the pending-configuration admission below admits
it, and once Orca is removed the runtime observes `absent` and the yard is ready again.

Commands around these findings follow the [yard-scoped release gate](#yard-scoped-release-gate);
a yard with only such findings keeps unrelated commands, its plain init and profile resource verbs.
While other release work remains, findings are one
bounded warning. A successful init of the yard, a converged or
re-applied owner for that yard and teardown of the yard remove the record through its known owner
path; other entries of the record directory are never touched.

### Plain update selection

A plain `yard update`, also with `--force`, selects the stable channel's latest release, but never
one with a lower semver than the active release (pre-releases order before their release). When
the installed version is newer, nothing is downloaded or changed. With a ready protected inspection,
or without any journal, update and `--check` succeed and report that the installed version is newer
than the latest published release, the active release's readiness, warnings and next step, and the
explicit downgrade commands; the `--check --json` report adds them as `installedNewer`. Any other
inspection result keeps its ordinary report or refusal, with the same note. Re-running the
installer without `--version` refuses before any prompt or download when the active release's
X.Y.Z core is greater than the latest release: it changes nothing and names two re-runs,
`--version <installed>` to re-run the installed release (which also completes an interrupted
install; an unpublished build needs its own assets) and `--version <latest>` to downgrade.
Only an explicit `--version VERSION` or `YARD_RELEASE_VERSION` selects an older release, under the
ordinary candidate inspection. An equal version keeps the ordinary same-release inspection.

### Yard-scoped release gate
Mutating commands consult the release gate (`internal/cli/admission.go`,
`internal/cli/yard_scoped_gate.go`). It is host-wide only for host causes: an unfinished, invalid or
unmatched journal, pending migrations, links, protected-inspection failures, host owners
(installation, host key tools, test-VM broker, test-yard route consumers, host power), native target
collisions and ambiguous topology. Every such refusal is text: the release state with active and
target releases, one deduplicated cause and one next step. RPC keeps the structured outcome; `yard
migrate --check` and `--json` report every finding and keep their exit codes.

When the journal of the active release is complete and every remaining cause belongs to yards, the
gate decides by the selected yard alone. The engine reports those causes in one closed grammar,
after plan binding and only for completed history: decisions with scope `yard-attribution`, resource
`activation.<owner>` and result `<kind> <yard>`, where the kind is `drift`, `failed`, `pending`,
`config` or `unreachable`. It emits all of them or none; the existing activation decisions are
unchanged, so an older CLI stays closed. The CLI accepts only its own yard-scoped owners (profile
runtimes, core guest runtime, materialized configuration), reads the journal and ledger itself, and
treats anything outside the grammar as host-wide.

The gate decides on facts the command itself prepared (`internal/cli/yard_gate_verdict.go`). Each
prepare attaches a gate subject from the request its handler already parsed and from its plan: the
repair set member it is, why it cannot be that repair, whether a yard with only pending findings
keeps it, the yards whose desired configuration it writes, and the other yards it acts on. One pure
verdict decides a subject against the yard gate and reports every refusal in a stable order: the
selected yard, the other yards it writes, then the yards it acts on. Three rules fix the contract:

1. A command is gated by every yard whose desired configuration it writes or that it acts on: a
   host, shared or Git setting writes every yard; a sync writes the yards its plan changes, and
   every yard for a change it cannot place in one yard or a push to Git; `config apply --all-local`
   skips and reports the other yards that need repair or have pending findings. Host commands that
   act on yards are gated by those yards, the selected one included: every network policy command by
   each yard its plan stops or starts (its updated yards and every yard pending start) and by the
   endpoints of a link or unlink: a yard that needs repair refuses it, and so does a restarted yard
   with pending findings. A command that declares no subject writes every yard and is never a
   repair.
2. A yard that needs repair admits only its repair set: plain `init` and `init --reset` (other init
   modes change what the yard is or do only part of its convergence), `teardown`, `start`, `stop`,
   `shell`, `config`, `profile` or `integration disable` and a profile resource's declared shutdown
   whose prepared assessment is a reversible shutdown without persistent data or external effects;
   the refusal names its next step, usually its own init. `config` is local authoring at any scope,
   `apply` and `sync`, and as a repair it may write or refresh other yards that need repair; Git
   authoring is refused with the manual repair (edit the file, then `yard -Y NAME config validate`).
   A yard with only typed pending findings keeps unrelated commands, its plain init and resource
   verbs, and refuses every write of its desired configuration. Other yards stay open, and so do
   host commands that act on no yard that refuses them.
3. For RPC the admission at `operation.execute` is the one of record. A plan made under a repair is
   refused as plan-stale when the release history or installed source changed by execute; that
   binding is compared again under the owner barrier.

Admission before dispatch decides by handler identity and the handler's own parser on the selected
yard only, so an invocation that cannot be admitted is refused before its prepare; it records an
explicit kind (ready, read, exempt, open, repair or bounded stop) that travels with the prepared
command. Paths before the gate, the completed-history fallback repairs and desktop client
integration, which writes only host-local client files, are exempt and own their admission.
Tests that prepare commands directly install an explicit fixture admission; execution without a
kind fails in tests and production alike. The early verdict runs on that admission's snapshot
before consent. The binding decision runs in `preparedCommand.Execute` after the owner barrier: an
open admission decides its subject on its snapshot without a new inspection; a repair is rechecked
against fresh release state and its subject decided on that state; a read admission runs no plan
with an effect, and a plan that cannot show it is a reader counts as one with an effect.
Non-prepared mutators (`shell`, `ssh-agent`, `_authorize`, `_state`, `_project-state`,
`_vm-start-policy` and older resource handlers) are a closed list decided by handler identity; an
older resource handler on a yard that needs repair is refused before consent. `--check` makes a
reader only where the handler's parser reads it as the flag: bare `config sync` and `update`, never
as another option's value. An owner's failure in one yard carries what it observed in the others,
and a named yard whose configuration or installed context cannot be loaded is that yard's finding,
so one failed yard never hides another's finding.

A repair admission, or the bounded stop below, records the release history (journal, ledger
projection and checkpoint) it inspected, read before and after the inspection. Except for the
bounded stop, it also binds the running tooling to the installed source: the CLI must be the
published runtime of the completed journal's target, under `releases/<target>`, and its source proof
must name that journal's transaction and artifact, so a development build or another staged or
retained runtime with the same registry and catalog is refused. A `shell` into a yard that needs
repair is exempt from this binding, so any tooling can diagnose the yard. `preparedCommand.Execute`
rechecks it once, right after it acquires the owner barrier that every journal writer holds: the
history and the source binding must be unchanged and a fresh inspection, which binds the runtime
links to that history, must admit the command again; otherwise it is refused as plan-stale before
any effect. Still under the barrier, the history must be unchanged after the effect, and a repair
other than init and teardown names what its yard still needs. An adapter never runs for a repair
admission that has not passed its recheck. Admission never takes the release update lock; the lock
order stays owner barrier, release update lock (converge and bounded in-handler configuration
repairs), per-yard lock. A `config apply` admitted as a repair applies in its own process with the
migration child's integration ownership checks; on an open yard it runs the ordinary child, which
inherits the owner barrier and is admitted on its own. After `config sync` publishes a change, its
consumer refresh is decided again on the state after publication: on a ready release it runs the
ordinary child; otherwise it runs in process only as a repair of those consumers, when each is open
or limited to repair only by drift and the installed source is bound, with release history checked
after the refresh; otherwise each refused consumer is reported with the one next step it needs. An
init succeeds when its own yard converged and reports the other yards' findings. The default yard's
own invalid configuration underlies every yard, so then every yard admits only its repair commands.
An invalid configuration of another yard is that yard's finding; host power and host key tools serve
the yards that load. `yard status` states the release once, as `release active and complete; yard
NAME needs attention`, and each finding under its yard; it never prints the protocol word `ready`
for a non-ready status.

Recovery advice is typed until it is printed (`internal/domain/recovery.go`): the gate and the
installed-source prerequisite are observed once, typed as findings, and one selector picks one
action that the gate admits and that is not the command just attempted. Status, refusals, sync and RPC
use the same selection and one renderer, `domain.RenderNext`. A refused installed source ranks first
and names its failed predicate, so no repair is advised behind it; its step is the command that makes
the predicate hold: `yard migrate` finishes an unfinished transition or a pending protected
publication with its sealed owner, `yard update` installs a fresh release whose journal, history and
source proof verify, and `yard migrate --check` explains an unreadable store. These remedies run past
the gate, so their step also names `yard migrate --check` for when the predicate still fails. Drift
that `init` left is named by `config status`. A repair run with other
tooling names the installed `yard` command instead. An owner's retry ranks next and is printed
verbatim when it is not one of the closed commands and not the command just attempted; invalid
configuration advises `config validate` after it is corrected. A refused config file names its
own step through `configmaterial.Refusals`.

A yard whose last init did not finish is left to that init, as described under [release
migrations](#release-migrations); release repair of a yard holds the per-yard lock init holds, so
release repair and init of one yard never interleave.

After a failed observation or apply in a running yard, the core probes its guest agent: when Incus
reports the yard running but a trivial command cannot run or does not finish, the yard's finding is
`unreachable`, keeps the handler's public diagnostic and names one step at a time: `yard migrate`,
after it `yard -Y NAME stop --force`, and after that `yard -Y NAME start`. A guest exit status is an answer, not agent loss. Ordinary updates still refuse before
activation for a yard with invalid configuration, an unreachable agent or an unknown failure; they
proceed without a stopped, absent or uninitialized yard. The guest temporary-storage step keeps its
own rules above. While an activation of the active release is unfinished, including when its
inspection is refused by an observation such as a lost guest agent, `yard -Y NAME stop` and `stop
--force` of a local yard are admitted, so the activation can finish without it; teardown is not.

Releases before this gate could lock a host after a failed first init of a new yard: every command
was refused until the broken yard stopped. On such a host, stop that yard's instance once with
`incus stop <instance> --project <project>`; readiness then returns and `yard update` installs a
release with this gate.

### Interrupted release recovery

An unfinished forward update pins its exact target, so a plain `yard update` only resumes it. To
leave a failed update, publish a fixed release and select it explicitly: a release newer than the
pinned target replaces the unfinished journal, before or after activation. When a plain resume
fails and the stable channel already publishes a newer release, the failure report names that
release and the exact `yard update --version` commands, with `--check` first. The lookup runs only
after the failure, never for `--offline` or an explicit selection, and an unreachable channel adds
nothing. Only the newer release's own inspection decides whether it may replace the journal. A host
whose active launcher predates this contract refuses a different version, so it uses the new release's own launcher:

```bash
VERSION=<fixed version>
curl -fsSL --proto '=https' --tlsv1.2 \
  "https://github.com/Subyard/Subyard/releases/download/v${VERSION}/subyard-install.sh" \
  | bash -s -- --publish-only --version "$VERSION"
# Run the launcher in the candidate directory that the installer printed:
<candidate>/bin/yard update --offline --version "$VERSION" --check
<candidate>/bin/yard update --offline --version "$VERSION"
```

A launcher that has this contract accepts `yard update --version "$VERSION"` directly.

The newer release plans from the links it observes and requires fresh consent; the earlier grant
authorizes nothing. It does not undo completed effects: restarts, guest policy and rewritten settings stay
as they are, and it migrates from them. If those effects refuse the new plan, correct the named setting
by hand and retry. The replaced journal stays byte-exact under
`release-transition-replaced-journals/v1/` in the configuration directory. An unclosed transaction directory joins it when
the successor closes, so a fix that also fails can be replaced by a later release. Re-running the installer selects this
contract too when it picks a newer release, and `--yes` consents without showing the replacement line. A directory whose evidence is already
complete counts as closed even if its journal stopped earlier: it stays under ordinary retention, and after that only the
journal copy remains. Full uninstall removes them. Add `--runtime-root` on a host with a custom runtime root.

Below the [update boundary](reference/legacy-migration-support.md#approved-support-boundary),
releases after v0.28.x refuse replacement while the installed source is from v0.11.0 to below
v0.28.0. For generic newer-release recovery described above, `$VERSION` must name a compatible
v0.28.x fix release. This does not include the v0.11.1 and v0.17.3 defects: their older,
matching-registry recovery releases and commands are listed below. After recovery, take the
published v0.28.1 hop, then update normally.

An older or equal version, `--rollback`, `--force`, and an experimental owner keep the exact-target refusal, as do source
migrations and the v0.11.1, v0.17.3 and v0.21.0 owners with their own historical contracts.
An explicit `--version` with an unfinished journal and an installed release from v0.11.0 to
below v0.28.0 returns `source-anchor-required` before selecting another candidate, together with
the installed owner's inspection refusal or operator-action blockers when it reports them. Its
`next:` first finishes the installed owner's journal, then takes the hop if needed. Plain `yard update`
keeps exact-owner resume and its existing recovery hints. The current engine creates neither
v0.11.1 post-activation superseded archives nor migration-owner replacements and does not delegate
v0.17.3 activation repair. It continues to read their retained evidence. The candidate refuses a journal that
already superseded another, a live recovery reservation, and links the journal does not allow.
A `recovery-v2-` successor is eligible only when it is reconciling, activation-only (zero migration
steps), forward, without source ingress, with validated recovery provenance and its target already
active. Older recovery and migration-owner successors still refuse replacement. The journal and
its recovery receipt chain are copied outside protected history before successor publication;
exact copied live records retire after verified successor completion, preserving readability by
retained engines including 0.27.0.

An interrupted integration apply may already have written every desired artifact. Its recorded
pending digests remain unfinished work: retry observes lifecycles, runs hooks, commits inventory,
verifies convergence and captures applied settings. It accepts only owned bytes matching the
recorded pending or committed digests. To leave a recovery dead end on 0.27.0, explicitly select
v0.28.1, which has the integration resume fix and recovery replacement support:
`yard update --version 0.28.1`, then `yard update`.

Published 0.28.0 stops an update at the first running VM yard whose pending disk-backed `/tmp`
migration it restarts, typically on a direct update from a release before 0.27.0: its migration
guard counts the project low-level grant that its own VM start policy writes as an outside
change. The earlier release stays active, yards and data stay intact, and every later update
reports that the journal-selected release cannot provide a valid transition inspection. Since
0.28.1 the registered-yard binding treats that exact grant on a VM yard project as product
output; any other value stays bound. To leave the stopped update, run the
[installer](getting-started.md) for 0.28.1 or a later 0.28.x release; it replaces the unfinished journal through
its own launcher whatever the active release is. An active launcher from 0.26.0 or later also
accepts `yard update --version 0.28.1`. Releases after v0.28.x cannot replace that journal: below
the anchor they refuse and name the v0.28.1 installer as the repair, and a launcher before 0.26.0
never reaches that refusal because it asks only the journal-selected engine. Such an install
runs the 0.28.1 installer first, then updates normally.

0.28.0 and 0.28.1 stop the same way at such a yard whose project carries an explicit
`restricted.virtual-machines.lowlevel=block`, which the start policy replaces with the grant.
Later releases treat that value as product output too. A replacement must be newer than the
release the unfinished journal selected, and releases after v0.28.x refuse such a journal while
the active release is below the anchor. A stop under 0.28.1 on an install below 0.28.0 is
therefore a known limitation: it is left only with a later 0.28.x fix release (a defective hop
is repaired in the anchor series). The earlier release stays active and yards and data stay
intact. Subyard never writes `block` itself, so only a manually set value reaches this stop.

Historical v0.11.1 can stop after activation at `reconciling` with
`transition.observation-scope`. The official v0.11.2 standalone installer owns that recovery
with the original registry. Run that historical installer, rather than asking the current
release to select v0.11.2:

```bash
PATCH_VERSION=0.11.2
(
  set -eu
  installer="$(mktemp)"
  trap 'rm -f -- "$installer"' EXIT
  curl -fsSL --proto '=https' --tlsv1.2 \
    "https://github.com/Subyard/Subyard/releases/download/v${PATCH_VERSION}/subyard-install.sh" \
    -o "$installer"
  bash "$installer" --version "$PATCH_VERSION"
)
```

The installer is interactive by default; review its replacement plan. Preserve journals,
ledgers, transaction evidence and runtime links until recovery completes. Then take the
published v0.28.1 hop and update normally. The current release only reads the retained
superseded archives; with an explicit `--version` it refuses this unfinished below-anchor
installation with `source-anchor-required`.

Published v0.17.3 can reopen completed settings inspection during a same-release activation repair:
a zero-step `reconciling` journal reports one `yard.<name>` unsupported-template blocker.
The verified historical v0.18.6 bridge retains the matching v0.17.3 registry and catalog and
owns delegated recovery with the original sealed assets. After its runtime installer publishes
v0.18.6 without activating it, use that candidate's exact printed launcher path (`<candidate-release>`
is the verified v0.18.6 release identity):

```bash
"<runtime-root>/releases/<candidate-release>/bin/yard" -Y default migrate --check --json
"<runtime-root>/releases/<candidate-release>/bin/yard" -Y default migrate
yard migrate --check
```

Then take the published v0.28.1 hop and run `yard update`. The installed v0.17.3 launcher cannot
perform the delegated repair. Preserve settings, authorization and transaction evidence.
The current caller keeps its historical retry hints, but its engine cannot delegate this repair;
with an explicit `--version` it refuses the unfinished below-anchor installation with
`source-anchor-required`. These two recoveries require their historical registries, which differ
from v0.28.x and the current release.

Destructive activation owners can implement `V2ActivationPreflighter`. After consent, the transition
runs these checks under its update lock before opening a journal and again on interrupted resume;
independent refusals use `domain.CollectRefusals`. Service recreation uses this boundary to prepare
the NetworkManager guard binding and verify guard and route safety before removing its runtime.
Preparation preserves the runtime and is repeatable after interruption.

Registry v2 contains only compiled, typed one-time capabilities. Activation reconcilers separately
refresh materialized config, an already-active test-VM broker and an installed host power runtime;
they do not change the migration ledger and never start an inactive service as an update side
effect. The old `config/migrations.json` reader remains only as a bounded compatibility seam for
recognized unfinished release journals until the minimum supported release is advanced.

When an explicit rollback targets a retained pre-v2 runtime, that verified runtime remains the
artifact target while the verified active v2 runtime owns inspection, convergence and recovery.
The superseded global v1 layout remains immutable history. Compatibility-sensitive materialized
runtime such as the host power reconciler stays at the v2 owner's verified form instead of
reinstalling a known-incompatible pre-v2 unit during rollback.

Before a project adapter starts, Go resolves paths/names/qualified selectors across yards, loads the
owning context, validates the typed record and supplies a `SUBYARD_PROJECT_*` snapshot. Physical
project adapters require that snapshot; they do not reload config, parse selectors or open state.
Operation options such as remove mode and image rebuild are passed as validated fields.
After a successful mutating adapter, Go atomically publishes or deletes controller state and, for a
remote yard, converges the owner endpoint before publishing controller state.

Native `clone`, `sync`, `bind`, `remove`, `code` and `export` actions use `@project`; they have no
shell handlers. The in-yard VS Code session probe is a lifecycle safety leaf. The retired project
handlers and `state/*` shims must not return.

`code` uses the resolved context's ordinary `sshHost` alias for its controller workspace.
Launching VS Code does not inspect or bind preview ports or mutate SSH configuration. SSH
convergence treats older snippets containing the retired `.code` alias as expected drift;
repeat `yard init` or refresh `yard remote add` to replace them. No one-time release migration is
needed: ordinary reconciliation owns these generated artifacts. Saved VS Code recent entries
using `ssh-remote+<alias>.code` must be reopened through `yard code` after convergence.
Owner-inventory project resolution retains a matching explicitly selected remote alias.
Desktop session targets also use the ordinary alias and carry no preview port or forward.

Controller-side desktop exports use the `@client-projects` prepared command family.
`internal/clientprojects` carries the canonical identity, ordinary L1 SSH alias and
authoritative project paths for exactly one yard. Preparation fetches fresh selected-owner
inventory through the existing pinned RPC client without cache publication, registration
recovery or identity adoption; the read-only SSH trust gate refuses unknown keys throughout
the proxy chain. The bounded L1 probe checks the configured user/port, instance hostname and
workspace root before client assessment. Execution rechecks the inventory and route before
the client's captured file plan applies. No project directory discovery or per-project RPC
is involved. Client serialization, protected JSON publication and URL launch belong to
`internal/adapters/codexdesktop`, composed at `internal/cli/client_integrations.go`; they
require no server profile. `client.projects-export` is a bounded local action without a
confirmation prompt. `--check` displays the assessment without writes, desktop publication
locks or GUI launch, while retaining normal configuration read coherence. Desktop actions
are refused at the generic RPC planning boundary and run only on the native controller.

Core provisioning atomically installs `subyard-preview` as root-owned mode `0755`. Running-yard
convergence checks its bytes and metadata; stopped yards use the installed source-hash marker.
The helper serves guest loopback `127.0.0.1:8765` in the foreground. Provisioning installs
root-owned mode `0644` endpoint metadata at `/etc/subyard/preview.json`: version `1`, host and port,
plus `bindHost` for an externally published VM endpoint. The owner address is the active Tailscale
IPv4, otherwise the private RFC1918 IPv4 source address of the default route. Public, link-local
and wildcard addresses are not published. Both container and VM yards use the same selection.
The owner port defaults to the SSH host port plus 30000, wrapping into `1024..65535`
(`2222` gives `32222`), with `WEB_PREVIEW_HOST_PORT` as the override.

Container proxy devices forward to guest loopback. VM proxies use `nat=true` and a pinned primary
IPv4; the helper additionally binds that exact guest address, retaining loopback for manual tunnels.
The instance stage owns IPv4 pinning; preview and SSH consume that pin. The network-policy stage
repairs an existing owner preview route before calculating ingress. Fresh instances prepare the route
after instance creation and pinning, then reconcile ingress before provisioning the guest metadata.
Exact ownership receipts and local device checks prevent replacing foreign or divergent routes.
The network policy preserves the owned VM preview ingress when isolation is enabled.
Repeat init after address or proxy changes. The helper validates the bounded endpoint file before
listening and prints its URL only after all required listeners bind. A missing file retains the
source-checkout loopback URL. Direct access requires network reachability and, for Tailscale,
Tailnet policy access. Private-network endpoints are visible to reachable devices on that network.

The `subyard-preview` capability skill delivers preview guidance to Codex, Claude Code and OpenCode
at user scope. Runtime refresh migrates inventory-owned guest blocks of old instruction copies only
with matching content and metadata evidence and refuses ambiguous or drifted content. Subyard no
longer copies host instruction files: an unchanged inventory-owned copy is removed and its
ownership released; a changed one is refused by path and kept. Agent instructions come from the
[user-dotfiles](user-dotfiles.md) profile.

Agents share the helper's printed URL and provide the operator-machine tunnel recipe from
`subyard-preview --help` only on request. The operator supplies their SSH alias; it cannot be inferred
uniquely inside the yard. See the [guest skills contract](../config/skills/README.md) for package
sources, protected ownership, discovery adapters, refresh and removal.

Remote registration, trust repair, removal and listing are native. Preparation probes the trusted
owner and scans the yard key without local mutation; old and new fingerprints enter the operation
plan before confirmation. Apply consumes that prepared evidence and atomically rolls back local
context, SSH config, trust and cache files if the data-plane verification fails.

Project-environment profile validation, mount/device policy and lifecycle planning also belong to
Go. A remaining shell hook may only execute the prepared Incus or Docker operation.

### Reconciliation stages

Go owns the typed stage registry, labels, order, live plan, resume behavior and finalization. It
re-checks immediately before apply, verifies immediately afterward and stops on failure. A rerun
skips converged stages; no completion marker replaces a live probe.

No reconciliation dispatcher or sourceable stage modules remain. Native probes own Incus, project,
instance, mount, provision, SSH and power state. Go invokes explicit package-manager, network,
storage, systemd, credential and nested-VM leaves for physical checks or mutations.
Registered-yard discovery and legacy power-metadata import are native. Shell only applies the
selected yard's guarded start/stop boundary.

`reconcileruntime.IncusReader` composes the required read-only Incus, instance-list and
project/shared teardown inventory ports. The runtime dependency and CLI wiring require
this interface, so production adapters and injected fakes must provide every method at
compile time. Write capabilities remain separate. This guarantees method availability;
nil dependencies, observation errors, inventory contents, ownership, stale-plan checks
and physical postconditions still require runtime checks and behavioral tests.

### Yard network policy

`yard network status`, `link A B`, `unlink A B`, `isolation on|off`, and `reconcile`
manage one physical host. Bare names and selectors qualified with the local HostID are
accepted; cross-host links require a separate transport and are not supported.
Isolation is opt-in and is never enabled by installation or upgrade. Links remain saved
when isolation is disabled. Each pair permits traffic in both directions; the graph
does not add transitive permissions. While isolation is off, links do not restrict traffic.

```sh
yard network link alpha beta
yard network status --json
yard network isolation on --yes
yard network unlink alpha beta --yes
yard network isolation off --yes
```

`internal/yardnetwork` owns the policy, read-only planning, stale-state rejection and
recovery. The default Incus project's `user.subyard.network_policy` key stores versioned
JSON with desired/applied revisions, links, original NIC settings, fixed identities and
pending restarts/cleanup. `internal/adapters/incusclient/network.go` performs exact,
ETag-checked Incus operations. It preserves unrelated project and profile fields.

The supported topology is a standalone Incus host using nftables, restricted per-yard
projects, and one shared managed IPv4 bridge with DHCP in the default project's
network namespace for all yards. Cross-bridge
isolation fails preflight because overlapping addresses and NAT make source identity
ambiguous. Disabling isolation remains available for recovery. Each yard's default profile owns its sole
primary `eth0`; foreign ACLs, local NIC overrides and additional NICs fail preflight.
Each profile receives a separate ACL, pinned IPv4/MAC identity and spoofing filters.
Address allocation includes DHCP and static reservations across every project sharing
the bridge; a fixed address claimed by another network identity fails preflight.
Ingress permits only explicit IPv4 peers and the bridge gateway's SSH relay; default
ingress denies IPv6 as well. Egress, DHCP and DNS remain available. This controls new
direct network flows, not application-level forwarding deliberately provided by a peer.

On Incus 6.0.6, NIC-filter changes are not atomic and updating an attached ACL can fail
for restricted-project references. Apply therefore stops affected running yards, detaches
their ACLs, updates rules, reattaches while stopped, verifies, then restores prior power.
The typed assessment includes the interruption. Active connections close during this
restart; removing an ACL allow rule alone would not flush established conntrack entries.
On failure, desired/applied revisions remain different and pending restarts persist;
managed starts refuse incomplete policy. Run `yard network reconcile --yes` to retry,
or explicitly select `isolation off` to restore the recorded original NIC settings.

These power changes are clean and never forced. A stop lets Incus wait up to 180 seconds
for the guest to power off, above the slowest measured clean stop of a nested VM yard;
a start waits 30 seconds. The client waits 15 seconds longer than Incus, so Incus reports
the operation's own result. A failed stop that leaves the yard running keeps it running:
reconcile repeats the clean stop. If the guest never powers off, only
`incus stop --force <instance> --project <project>` ends it, which can lose data the
guest has not written; the error names the exact instance and project. `yard stop --force`
only bypasses session guards and does not help here. Until a timed-out clean stop ends,
Incus refuses other changes to that instance. A resource bring-up that fails then cannot
close its own route; its error states whether owned ingress remains published, is closed
or is unknown, and names `yard -Y <yard> <command> <shutdown>` to finish after that stop ends.
Rollback cleanup of the isolation ACL has five minutes, enough for one stop and one start.

The fixed root-owned `/run/lock/subyard-network/policy.lock` serializes policy application
and managed starts. Host network setup, boot-reconciler installation during upgrades,
and boot reconciliation initialize it without replacing an existing validated lock.
The `network-policy` init stage runs after host networking and before instance creation;
ordinary starts, init finalization, the test VM backend, boot restoration and teardown
use the same policy service. Existing NetworkManager and host-route guards still apply.

### Profile startup readiness

Explicit `yard start` waits for installed profiles declaring `startup_readiness` after
physical startup, desired power commit, publication retry scheduling, any pending `/tmp`
restart and startup resource activation. A readiness refusal leaves the yard running with
`desired=running` and reports every transient profile's public reason and hint in stable
order. Physical adapter entry attributes startup observations to this invocation. The
30-second readiness polling budget begins when the profile wait starts, after the agent
is ready; physical boot time does not consume it. Shorter caller deadlines still apply.
They use the applied account and profile selection, including retained disabled declarations.
Missing applied account evidence is stable init repair work; desired identity is never a
fallback. Pending identity recovery skips these service probes.

Positive startup evidence includes a pending unit/timer start job or a container health
check starting. During this invocation's anchored startup window, GitHub and Telemetry
also treat auto-restart as `starting`; GitHub connections and receivers may still
be coming up. Unit activation timestamps never shorten an anchored invocation's window.
Failed services and inactive units without a pending job remain stable drift; `migrate`
owns their repair. Without an invocation anchor, probes retain their standalone semantics.
A successful startup probe establishes service health,
not complete installation convergence. Invalid or unavailable probes refuse without exposing
native output. Checks return state and diagnostics; the operation boundary renders refusals.

Temporary identity-inspection boots, rename recovery, migration and repair restarts, prepared
power finalization and native host boot restoration do not run these probes. Their owners must
still reach account and service repair. An explicit start also waits for an already running
yard. Undeclared profiles, infrastructure, external accounts and background workloads retain
their existing behavior.

### Boot address readiness

The boot power reconciler inspects effective Incus devices before starting an initialized,
managed yard with `desired_power=running`. A host-bound, non-NAT TCP/UDP proxy that listens
on a missing local IPv4 or IPv6 address puts the yard in `WAITING_FOR_ADDRESS`. Loopback,
wildcard, Unix-socket, NAT and instance-bound listeners do not require this wait.
The exact proxy binding and desired power remain unchanged; other ready yards can start.

`yard status` and `yard yards` render this start state. Detailed status lists the missing
addresses. `yard.status`, owner inventory and `yard yards --json` preserve the physical
`state` and add `startState: "waiting-for-address"` and `waitingForAddresses` while waiting.
`internal/application/start_readiness.go` owns this derived state; it is not a second
persisted power intent or a cached replacement for Incus state.

The reconciler checks active local interfaces once per waiting yard and exits with
temporary status 75. The installed systemd unit treats that as expected and retries after
30 seconds, with no process kept alive between attempts. It uses local interface inspection
and the local Incus API, without DNS, connection probes or external network requests.
Once the address appears, the next attempt performs the ordinary guarded start. Local
observation errors, malformed listener endpoints, port conflicts and network-guard failures remain
errors; a permanent failure ends host-wide reconciliation even if another yard is waiting.

The targeted real-Incus/PID1 check is `dev/e2e/proxy-address-wait.sh`, run through the
allocated VM runner. It exercises delayed address appearance, independent starts and
automatic recovery, and measures cumulative retry CPU and IP traffic in a dedicated slice.
IP traffic results require a successful local positive control and exclude the Incus daemon.

### Optional owner publication

Receipt-owned optional owner listeners are reconciled independently of desired power.
The power reconciler snapshots an exact, local, non-NAT device in version 1 metadata
under `user.subyard.deferred_publication.<device>` and removes that device atomically
using the observed Incus ETag when its listen IP is absent. The original owning receipt,
IP, port and connect endpoint remain unchanged. The yard can start with local access;
Detailed status, owner inventory and the owner status summary report
`deferredPublications` separately from the yard's physical running state.

The installed root-owned unit contains a versioned snapshot of public owning contracts
from the candidate paired with its copied binary. Generic codecs recognize preview,
integration and profile dashboards and explicitly declared resource ownership receipts; device names
alone never prove ownership. Foreign, inherited and unreceipted devices retain the normal
address prerequisite. Legacy resource devices lacking a receipt require an ordinary,
explicit owning operation to establish ownership; exact recipe matching alone does not
authorize automatic boot adoption.

An expected publication wait uses the existing 30-second temporary-failure retry, including
when the yard is already running. When the exact address returns, the reconciler validates
the saved device against its current owning receipt and atomically restores it without
restarting the yard. Explicit start schedules this retry after committing running intent.
Publication restore failures do not enter the security-critical running-runtime failure
path. Host network and policy guards continue to fail closed.

The compiled `nm-device-guard-v1` capability advances `power-metadata` from epoch 3 to 4. It
is the reader boundary of the `[device-subyard]` NetworkManager guard; the
`host-network-guard` activation owner rewrites a released guard file without reloading
NetworkManager and records one pending host reboot. Subyard never reloads or restarts
NetworkManager. Whether NetworkManager read the guard is proven only by a boot: the root-owned
record `/var/lib/subyard/nm-guard-state` (`boot_id inode:ctime sha256 held|unread init|migration`)
binds one regular guard file, by its identity (compared only for equality), to the SHA-256 of its
static content (without comments, blank lines or the exact Incus bridge names, which are pinned
unmanaged one by one instead). The guard counts as read when the record binds the file on disk
and is `held` or names another boot. Subyard removes the record durably before any change to the
static content, then renames the guard in and records it `unread` for this boot; `held` needs
NetworkManager explicitly inactive at both samples, or a migration with evidence that it holds the
released guard. A bridge-only rewrite keeps boot and state and refreshes the identity. A guard
without a binding record is `unbound`: the record is missing, unreadable or foreign, the identity
or hash differs (a hand edit or restore), or the guard is a symbolic link. Its next step is
`yard init` (which records it `unread`, replacing a link with a file), then one reboot. A
NetworkManager restart never counts. No yard starts while the guard is unread or unbound: init,
early installation included, stops before anything creates devices; update and teardown leave the
reboot to the operator.

The record's guarantee has a boundary. It binds the guard file, not the rest of NetworkManager's
configuration: an administrator who restarts NetworkManager on a different configuration and then
restores the files on disk, for example by swapping or restoring the `conf.d` directory or by
adding a temporary higher-priority file such as `zzz.conf`, restarting and removing it again, is
outside what the record can detect. Reboot after such changes. `held` also trusts systemd's view
of NetworkManager: "explicitly inactive at both samples" means `systemctl is-active` reported it
inactive, so a NetworkManager process running outside its unit is not seen. Released power readers reject the new
guard, and their init would rewrite the released guard and reload NetworkManager, so
rollback to a release below this epoch is refused before activation as `rollback-incompatible`.
`yard update --rollback` does not cross this boundary.

The compiled `owner-publication-state-v1` capability advances `power-metadata` from epoch 1
to 2 before runtime activation. Existing canonical power and ownership values retain their
formats. The protected capture/apply/verify ledger establishes the reader boundary; explicit
publication writes and installation of a publication-capable power unit require that committed
epoch. The unit snapshots `readerEpoch: 2` with its owning contracts. A copied engine without
this unit contract retains required-listener checks and cannot defer optional publication.
Supported rollback to an epoch-1 owner is refused before activation because historical owners
cannot honor detached publication state, including Orca cleanup. The inspection reports this
loss of rollback eligibility before authorization. Recovery restores only a still-matching
receipt; owning removal clears its journal before retiring that receipt.
The unit contract envelope is version 2, independently of the unchanged durable receipt
version and power-metadata epoch. Version 2 adds cleanup-only contracts from retired route
declarations: they validate and cancel saved restoration authority without restoring a
retired exposure. Current engines also read the exact closed version 1 envelope; version 1
cannot carry cleanup-only fields. Older epoch-2 engines cannot read version 2 unit snapshots.
Installation writes the copied binary and its generated unit sequentially. Convergence
requires matching engine bytes, the exact generated unit and fresh systemd manager state;
this check does not establish an atomic native-file snapshot. A mismatched older binary
rejects version 2 before publication effects. Matching durable metadata epochs do not imply
compatible unit envelopes. Deferred selected profiles keep their original endpoint identity
while status withholds the URL until publication resumes.
Unknown journal/contract versions, ownership drift and conflicting local/effective devices
are errors.

### Credential ledger


The host-scoped ledger is physically outside the checkout and every managed yard mount. Its shared
Git store contains signed SOPS/age ciphertext; local-only records and identity keys never enter that
store.

The native credential runtime owns revision policy, cryptography, storage, materialization and peer
transport. Its RPC view projects only allowlisted metadata and never exposes encrypted payloads.
Secret payload enters only through protected stdin or a mode-0400/0600 file and is never placed in
command arguments, environment metadata, audit output, or a revision's unencrypted fields.

The public revision shape remains `config/keys/revision.schema.json`. Revision DAG, recipient
intersection, revoke/tombstone behavior, assignment epoch, append-only verification, quarantine,
local-only isolation, and fail-closed exclusive handoff are conformance contracts.

### Profile resources

A resource descriptor is `config/profiles/<profile>/resources/<name>.res` with:

```text
COMMAND=<yard-command>
HANDLER=resources/<name>/handler.sh
TITLE="..."
ACTION="<local-id> <public-verb> <assessment-class> <recovery-class>"
ACTION="..."
BRINGUP=<verb>
SHUTDOWN=<verb>
STARTUP=bringup                   # optional first-start activation after provisioning
PROXY="..."                       # optional typed owner-host proxy contract
DASHBOARD="http HOST_SETTING PORT_SETTING /path" # optional browser endpoint metadata
ENDPOINT_DEFAULTS="tailscale-self 6768" # optional automatic owner endpoint policy
BOOTSTRAP=profile                 # optional profile selection and init on bring-up
CONTROLLER_SESSION="view"         # optional session verbs run on the controller for remote yards
CONTEXT_ENV="SAMPLE_RUN_CONTEXT"   # optional inherited nonsecret metadata for resource apply
```

`ACTION` is repeatable and is the source of the public verb list. Its assessment and recovery classes
bind each operation to the shared typed confirmation policy. The engine owns confirmation input.
Non-session handlers run with standard input connected to the null device. Session actions
inherit operator input; terminal sessions take foreground control and return it on exit or
cancellation. Session cancellation sends TERM to the handler's process group and allows up to
one minute for owned-resource cleanup before KILL. When any session ends, the engine also
terminates remaining members of its process group, retaining the handler's identity until
cleanup completes. Handlers can still supply their own pipes or here-documents to child processes.
Each resource process group remains cancellable as a unit. Declared read-only verbs remain
available during an unfinished release transition; verbs with any non-read action remain gated.
At least one action is required, and the `BRINGUP` and `SHUTDOWN` verbs must be declared by actions.
`HANDLER` is relative to the owning profile.

`CONTROLLER_SESSION` opts declared session verbs into controller-local execution when a
remote yard is selected. Other verbs retain owner routing. The controller validates the
registered owner SSH trust and supplies the handler with a private structured transport
description in `SUBYARD_RESOURCE_SESSION_TRANSPORT`. The reserved
`yard RESOURCE --session-wire VERB` invocation is accepted only on the selected owner for a
declared controller session and launches the handler in `wire` mode with
`SUBYARD_RESOURCE_VERB`. The profile owns its wire protocol and resource authentication; core owns
route selection, SSH trust, session environment and cancellation. GUI environment variables
remain on the controller. Controller routing is rejected for non-session actions.

`CONTEXT_ENV` explicitly allows up to 16 unique uppercase environment names (at most 64 characters
each) into that resource's apply process. Without a declaration, existing environment filtering
is unchanged. Preparation and help do not inherit these values. Shell/loader variables, interactive
environment and core runtime, config and transport selector namespaces cannot be declared. Prepared
context and catalog settings always take precedence. Missing values, values over 4096 bytes and
values containing NUL, escape, CR or LF are omitted without changing the native resource operation.
Values are passed directly as process environment entries and are never rendered into commands or
diagnostics. This metadata grants no resource authority and does not change the profile's wire protocol.

Registry validation rejects unknown descriptor fields, path traversal, duplicate names/commands or
local action IDs, collisions with core commands, invalid actions, and missing executables. The handler
owns every lifecycle verb including the silent `is-up` probe. Core code discovers, dispatches,
probes, and renders hints from the descriptor. `BOOTSTRAP=profile` opts into a composed bring-up:
select the profile, reconcile the yard, then apply the resource under one typed confirmation.
Its bring-up action must use `bootstrap-change` with recoverable mutation metadata.
`ENDPOINT_DEFAULTS` requires a proxy contract and supplies its host/port setting names through
that contract. The engine previews and atomically records owner-local endpoint assignments;
read-only configuration/status calls never allocate a port. See the shipped `.res` files for complete examples.
`DASHBOARD` is explicit because a TCP proxy does not imply HTTP. Detailed status publishes its URL
only while the resource's `is-up` probe succeeds and the referenced host and port settings are
valid.

Resource handlers reserve prepare exit status 2 for invalid command-line arguments. The shared
`svc_usage_error` helper exits with status 2; the dispatcher classifies this as
`resource_usage_invalid` and returns CLI exit status 2; rejected arguments cannot reach apply. Help and an omitted verb return 0,
while other prepare failures, precondition failures, and invalid plans return 1. The successful plan
v1 schema remains available for dedicated actions. Mutating exact resource invocations use
`yard.resource-action-assessment.v2` with public `steps` and an optional private SHA-256 `binding`.
The engine retains the descriptor, handler identity, configuration and native binding, refreshes
under the resource's owning lock, and passes the approved bounded plan to native apply. A v2 handler
implements the read-only `verify` phase: its returned steps must retain the same targets and desired
states and report `skip` with observed state equal to desired. The generic RPC endpoint dispatches
safe mutating resource invocations through this prepared path; v1 handlers fail closed for exact
RPC mutation. Bring-up and shutdown of an owner public-ingress resource run only through this
prepared path, which admits the owner barrier before the yard lock; a v1 plan or non-exact action
for them is refused before any lock or change. Read, session and protected actions keep their
dedicated contracts. Public fields are
bounded safe text; payloads, commands and protected content never enter the resource projection.
Native identities use explicit namespaces such as `incus:`, `container:` or `invocation:`;
safe fingerprints carry a digest label. Long unlabeled identifiers are checked as possible
credential payloads. Namespace labels do not permit protected paths or credential assignments.

`STARTUP=bringup` opts a selected dedicated VM resource into first-start activation. It requires
an owner IPv4 UDP proxy and the declared bring-up/shutdown actions. Successful selected-profile
provisioning records owner-local `pending` intent only when no intent exists. Explicit
`init --profile NAME` includes that profile's provisioning in the initialization assessment;
plain `init` remains infrastructure reconciliation. Repeat provisioning preserves intent. An exact
already-owned legacy ingress without startup intent remains unarmed; partial or foreign ingress
is rejected rather than treated as a new service.

The first explicit `yard start` assesses the handler in trusted `prepare-start` mode while the VM
is stopped, including its exact endpoint and host network effects. After one confirmation it starts
the VM, verifies the ordinary handler preconditions, and applies bring-up with normal ingress
verification and rollback. A failed activation retains pending intent for a later explicit retry.
Only one selected startup resource per yard is supported. Ordinary host boot does not perform a
pending first activation or execute profile handlers.

Successful bring-up records `enabled`; explicit shutdown records `disabled`, even before first
activation. Guest service enablement and owned ingress handle later starts and reboots. Repeated
provisioning and yard starts preserve explicit disablement until the resource's bring-up verb is
invoked. Owner intent is separate from ingress ownership and pending ingress cleanup.

### Dedicated profiles and VM capabilities

Core code consumes profile declarations; it must not branch on a profile name or service port.
A profile's `profile.conf` can declare `PROVISION_SCOPE=dedicated`. Such a hook requires
explicit selection and a named yard whose shipped role declares
`EXCLUSIVE_ENVIRONMENT_PROFILE=<profile>`. An empty `ENVIRONMENT_PROFILES` excludes dedicated
profile provisioning and selected-resource probing; discovering a descriptor never enables its service.

Shipped yard roles may declare `REQUIRED_YARD_KIND=vm`, `ALLOWS_CODING_TOOLS=false`,
`ALLOWS_HOST_ACCESS=false` and `ALLOWS_PROJECTS=false`. Host-access exclusion rejects mounts,
links, arbitrary device/capability declarations, SSH agent forwarding and
nested E2E access. A profile whose runtime needs an owner-host route, such as the test-VM broker
route mount, declares `REQUIRES_HOST_ACCESS=1` in `profile.conf`; selecting it in such a role is
refused. These constraints are validated after configuration precedence is resolved and report
every independent refusal together.
Before startup and during convergence, effective Incus devices are checked too: inherited host
mappings and local root-disk overrides cannot bypass the role or its root disk bound.

Only a profile's own `profile.conf` declares `YARD_MOUNTS`, `YARD_CAPS` and `YARD_DEVICES`; the
extras resolver evaluates it without operator values for them and reports every independent
declaration refusal together. The catalog allows these settings only at shipped scope. The loader
discards each operator assignment, at any scope or in the command environment, in one place, as it
discards retired settings, while the file is read: its expression, and a default of the name inside
another record's expression, is evaluated in isolation, so no later record sees it or any default
it performs, and neither its evaluation error nor a required check of the name refuses loading.
Such a nested default is not reported. Persistent-settings
parsing for `config set`/`unset`, `init` and config sync discards it the same way. A non-empty one
is returned as configuration state (`Loaded.IgnoredAssignments`) with its exact repair. It is never
a refusal: `config show`, `status`, `update --check` and the init plan render it, `update --check`
also as the structured `configuration` field of its JSON report for every local yard that loads,
with the load refusals of the others on stderr, and `config unset` remains available at operator
scopes. Config sync accepts and materializes a persisted one
unchanged. The init plan names each owned `yx-*` extras device that the pending extras stage
detaches, resolved as `09-yard-extras.sh` resolves it. A declaration refusal refuses planning. A
yard without an instance detaches nothing. When the instance cannot be read, for example while
Incus is stopped, the plan states that any attached `yx-*` device no selected profile requests is
detached. The extras stage observation carries each declared device of a VM yard as a "not
applicable to VM yards" diagnostic that `init` renders on the converged stage; it is readiness state, not a refusal or a warning. Ignoring and reporting operator
assignments is migration code for files written before these settings became profile-only; it can
be removed once operator files no longer contain them.

Every VM yard is headless. The owner sets a fixed local `raw.qemu.conf` whose empty
`[device "qemu_gpu"]` section removes the Incus default emulated GPU (virtio-vga); with
`VM_FREE_PAGE_REPORTING=1` it also carries the balloon reporting property. The yard's restricted
project grants `restricted.virtual-machines.lowlevel=allow` for this override only; settings and
profiles never supply raw QEMU text. `incus console --type=vga` therefore shows no display; the
serial console, `incus exec` and SSH are unchanged.

QEMU reads `raw.qemu.conf` only when it starts, and Incus refuses to change it on a running VM.
New VMs receive the value before their first boot. An existing VM receives it lazily, with no init
required after an update: each product
start of a stopped VM (`power_start_guarded` for init, `yard start` and restarts, the boot
reconciler, hard cleanup, `yard vpn up` and `yard network reconcile`) first writes the headless form of its owner value through one
engine policy (`yard _vm-start-policy` from shell), under the instance ETag of the read that
checked it. That start keeps an installed reporting property and leaves a foreign override as
found. Host-reboot restoration makes only non-reporting yards headless; it starts reporting
yards with their value unchanged because it bypasses admission and must preserve rollback
readability. A reporting yard becomes headless at its next start through an admitted yard command.
After a host reboot, a reporting yard therefore keeps its emulated display and the "headless VM
device policy applies at the next yard start" readiness diagnostic until that admitted start.
Hard cleanup is a recovery route for an authorized rollback and also bypasses admission: its starts
preserve reporting values, and recreation creates reporting VMs with the earlier reporting-only
value. Non-reporting VMs are still created headless. The internal preservation signal comes only
from the hard-cleanup runtime, never from the caller's environment.
When the restricted project lacks the low-level grant, the start adds only that key, under
the project ETag, and only to a project whose installation ownership names the managed instance
being started; any other project is left as found for init. A missing grant is converged for an
otherwise ready existing VM with a previous owner value, so project readiness does not mask its
next-start diagnostic or require init after an update. Init writes the value itself only on
a stopped VM it is about to start; an intentionally stopped VM receives it at its next product
start, and a running VM is never stopped for it. A running or intentionally stopped VM with a
previous owner value (none, or the reporting-only value of earlier releases) is converged with the
readiness diagnostic "headless VM device policy applies at the next yard start"; it is neither
drift nor a warning, and no restart is disclosed. Enabling reporting keeps its existing guarded
restart, because its guest check needs the property. Any other `raw.qemu*` key or value on the
instance, an inherited `raw.qemu.conf`, or `raw.qemu*` in the project default profile refuses
readiness and init with every finding; the product never overwrites it. Adoption takes any owner
value as found. Plans that span an approved start bind an owner value as its start-written form,
so that exact change does not make them stale. A VM started directly through Incus becomes
headless at its next product start.

Before rollback planning, journal reinspection and execution, the active engine inspects every
local reporting VM yard. If its local `raw.qemu.conf` is the headless-plus-reporting owner value,
a target without the bound `_vm-start-policy` command refuses with `rollback-incompatible`, naming
affected yards in `YardNames` order (default first, then by name), with an omitted-findings count
and a pointer to complete detail on `yard update --rollback` stderr when the diagnostic bound
is exceeded. The inspection returns all findings; the operation boundary renders them once.
Independent configuration-layout and VM reader refusals are combined before bounding the summary.
Stay on the current release until a compatible retained release is available. The target is queried through `--command-effect _vm-start-policy` using its
verified command manifest; a version alone never proves reader compatibility. Headless yards
without reporting and the older reporting-only value do not block rollback.

The generic VM settings are:

| Setting | Contract |
| --- | --- |
| `VM_FREE_PAGE_REPORTING=1` | Add the existing virtio balloon's fixed reporting property to the owner override and set guest reporting order 1; require the QEMU `free-page-reporting` property and guest negotiation, and reject conflicting raw configuration. No arbitrary QEMU input is accepted. |
| `VM_PIN_IPV4=1` | Pin the private primary NIC to its observed DHCP address after ownership and collision checks. After the guarded boot, wait a bounded time (60 s) for the guest address and its lease, then refuse naming the last observation. |
| `VM_CPU_WEIGHT` | Optional Linux cgroup-v2 CPU weight (1–10000; normal weight 100). Persist local owned VM metadata and attach its verified QEMU process to a transient systemd scope in the root slice. Refuse placement that would bypass an ancestor CPU, memory, task, cpuset or I/O ceiling. |
| `ROOT_DISK_SIZE` | Bound the root disk at creation; refuse implicit resizing of an existing VM. |
| `SRV_VOLUME_TYPE=block` | Attach an owned custom block volume and mount its ext4 filesystem at `/srv` by UUID. |
| `SRV_VOLUME_SIZE` | Required explicit size for block storage; refuse foreign volumes or size changes. |

The block-volume adapter formats only a newly marked blank volume. It never hides nonempty `/srv`
or overwrites an existing filesystem. The ordinary teardown state-retention policy still applies.

Explicit `teardown --reset-config` extends the typed teardown assessment with metadata-only local
settings inventory, namespace identities and the yard fallback ownership marker. Settings removal
runs under the configuration root lock only after native physical teardown verification; local
registrations are removed last. Stale settings, marker or namespace changes refuse apply. The
marker is separate from the deleted yard scope and suppresses only nonlocal yard scalar/asset
fallback, preserving shared/host resolution and the authored source. Completed named reset retries
never infer a new physical deletion target from defaults. The physical boundary fences reachable
SSH listeners, rejects proven active sessions, and uses existing profile service pause/remove hooks;
absent SSH and an unavailable guest agent are valid cleanup states. Incus daemon unavailability
fails closed. See [configuration reset](configuration.md#resetting-one-yard).

Reporting returns unused guest pages to the immediate owner; it does not change configured memory,
inflate the balloon, or reserve capacity for other workloads.
Reporting has no tested-version allowlist for Incus or QEMU; the general Incus minimum still applies.
The owner persists guest reporting order 1 through a root-owned `/etc/tmpfiles.d` rule and applies it
after the VM agent becomes ready. The guest readiness check verifies the negotiated reporting feature,
the exact rule and its permissions, and the live kernel order; drift remains a read-only diagnostic.
Reporting and IPv4 pinning are opt-in enforcement capabilities: `0` or an unset value does not undo
an override or address pin already installed on an existing VM, nor revoke its project permission.
These inputs are not runtime on/off switches.

CPU weight is a relative share during host CPU competition, not a reservation or quota.
The VM's configured vCPU and memory limits remain in effect. Setup, ordinary product start,
network reconciliation restarts and host boot reapply the persisted scheduling policy to all
QEMU threads. Unset CPU weight preserves an already installed policy. Readiness inspects the
process ownership, root-slice placement and live `cpu.weight` without changing the host.
Start plans include a separate `vm-cpu` step for persisted CPU weight: apply for live drift,
skip for convergence, or conditional on the approved power transition. CLI and RPC recheck
these steps before startup work and privilege preparation; new work after a skipped assessment
returns `plan_stale` before authorization.
Hosts with constrained source ancestors retain their existing cgroup placement and return a
diagnostic: copying an aggregate service ceiling onto each VM would not preserve that ceiling.
An instance restarted directly through Incus requires the next product start or init to restore
its scope; the product does not install a polling service for external lifecycle actions.

### Explicit owner TCP or UDP ingress

A profile can declare one exact IPv4 NAT route to a pinned VM address:

```text
PROXY="service-port RESOURCE_SERVICE_IPV4 RESOURCE_SERVICE_PORT RESOURCE_SERVICE_INTERFACE udp:guest:12345 owner-metadata-v1 owner-ipv4-udp"
```

`RESOURCE_<ID>_IPV4`, `_INTERFACE` and `_PORT` are typed yard/command settings. The IPv4 must be an
explicit owner address on the selected interface; wildcard publication is forbidden. The descriptor
declares the guest port. `tcp:guest:22` with `owner-ipv4-tcp` declares TCP;
`udp:guest:host-port` (or TCP) uses the configured owner port inside the VM too. Its bring-up and
shutdown use `public-ingress-change reversible` action metadata. Automatic port allocation and
`BOOTSTRAP` are not supported for public owner routes. `STARTUP=bringup` is restricted to UDP ingress.
Explicit provisioning of a selected profile in a local named VM yard can fill missing owner
IPv4/interface settings from one unambiguous active public IPv4. Explicit values take precedence;
ambiguous or non-public-only hosts receive a manual-configuration diagnostic. Discovery is local
to the owner, is included in the provisioning assessment, and writes both settings atomically
after successful guest provisioning. It rechecks the address, configuration and absence of ingress
before writing. It never enables the service or publishes a route. These local settings override
Git fallback settings; source registration does not prohibit the local write.

The handler prepares the concrete endpoint and runtime effects without mutation. After one shared
confirmation, the engine serializes the operation with yard configuration changes, rechecks the
plan, applies the handler and reconciles the matching ingress ACL. The route must have the exact
declared device shape and `user.subyard.resource.<device>` ownership fingerprint. Foreign or
modified same-name devices are refused. Network isolation allows only the selected, declared and
owned route; it does not grant broad transport access. Failed network reconciliation invokes the handler's
internal `rollback-ingress` under the original bring-up operation to close its route. The engine
verifies that both the proxy and ownership marker are absent before accepting a no-op or removing
the matching ACL allowance, and checks closure again after ACL cleanup.
Bring-up requires a converged network policy. Shutdown and rollback may remove only that route's
persisted approval and exact transport allowance, including after an interrupted shutdown; they refuse
unrelated ACL, NIC or project drift instead of reconciling it as part of a resource action.

Isolation persists the approved route's device, transport, owner endpoint and guest port in the existing
network-policy binding. Boot restoration derives its ownership fingerprint from those parameters
and the binding's guest address without loading profile files. It requires an exact match with the
current local and effective proxy, ownership marker, pinned guest address and ACL. An omitted
transport means UDP for compatibility. Older policies with a stored fingerprint remain readable
after validation; subsequent writes omit that redundant
field. Missing or changed approval blocks managed starts. Ordinary reconciliation continues to
use selected profile contracts; shutdown and deselection remove the persisted allowance.
After a managed VM is newly started during owner boot, the reconciler checks its current owned
public UDP route under the host network lock and clears only stale, untranslated IPv4 UDP
connection-tracking entries for that exact owner address and port. A missing host `conntrack`
tool is repaired by the VM prerequisite stage of `init`; boot never installs packages.
A changed public UDP bring-up performs the same bounded cleanup after route and service
verification, restricted to the selected resource endpoint. This lets an existing client reconnect
after sending packets while the service was disabled. Root authorization happens after confirmation
and before activation; a no-op bring-up does not request privileges or clear connections.

A resource may also declare a native application management endpoint:

```text
MANAGEMENT="NativeApp RESOURCE_SERVICE_IPV4 RESOURCE_ADMIN_PORT admin"
```

The declaration names the application, host and port settings, and SSH user. Detailed yard status
renders these public values as a management hint for an active resource; it never reads or returns
credentials. This does not imply a browser dashboard or enable the administrative route.

Before changing an active route's endpoint, template or profile selection through `config set/unset`,
run its shutdown verb. The read-only shutdown assessment must report no remaining enabled runtime
or ingress. After manual profile deselection, `init` assesses the dedicated resource's declared
shutdown, closes its exact owned route, and runs that shutdown under the same operation. Unverified
guest shutdown retains the pending ownership marker and blocks further initialization so a retry
cannot silently reopen the service. Guest data is preserved. Stop the resource before editing its
files manually; invalid configuration or ambiguous ownership fails closed and requires correction
before reconciliation.

When completed release history leaves a yard limited to its repair commands, for example after its
runtime observation failed, the local owner can still assess and confirm that yard's declared
shutdown when its handler prepares a v2 plan with a reversible shutdown assessment. The repair
admission binds the installed source and tooling and is rechecked under the owner barrier after
consent against the same history, the same source binding and a fresh inspection, which binds the
runtime links; bring-up, unfinished history, host causes and changed bindings remain gated. This
authorizes only resource cleanup and preserves unverified guest cleanup; it never resumes or applies
release activation.

## Test topology

`./tests/run.sh` verifies gofmt, vet, race tests, a fuzz smoke and a static build; syntax-checks every
nested shell file; validates that each top-level test belongs to exactly one suite; then runs:

- `tests/suites/unit.list`: pure and filesystem-local policy;
- `tests/suites/contract.list`: CLI, context, registry, convergence, and security contracts;
- `tests/suites/integration.list`: process tests with temporary roots and fake external commands.

CI selects Go from `go.mod`, runs the same suite and recursively ShellChecks all Bash entrypoints,
modules, profile handlers and tests. The fake Incus Unix server implements official-client REST,
async-operation WebSockets, errors, cancellation and event disconnects. Synthetic credential fixtures
contain no real secret. The opt-in E2E VM subset is documented in
[`real-host-acceptance.md`](real-host-acceptance.md).

## E2E VM acceptance lane

Host-free fakes cannot prove Incus, kernel, network, mount, systemd, or real SSH behavior. The
operator maintains a configurable pool of disposable one- or two-VM allocations. The canonical pool, exact-slot,
lease, nested-slot, and cleanup contract lives in [Agent E2E VM pool](test-vms.md). The
release smoke is `dev/e2e/p0-acceptance.sh --slot N`; the full compatibility matrix is
`dev/e2e/p0-acceptance.sh --slot N --lane full`. Choose checks using the [dev-flow](https://github.com/Dmitry-Borodin/dev-flow) check kinds
and the Subyard parameters in
[Subyard dev-flow](../.agents/skills/subyard-dev-flow/SKILL.md#choose-checks-by-risk). GitHub workflows do not run
either VM gate. Do not run them on the operator host or in the privileged outer yard.

The full matrix covers the following physical boundaries:

1. For both a container and VM context: `yard -Y <context> init`, rerun it as a no-op, introduce one
   safe managed drift (for example the ccusage convergence marker), rerun to repair it, then reboot
   and confirm desired power.
2. Sync a synthetic repository, verify `list`, `shell`, `export`, `remove`, and an optional L2
   `up → info → down` cycle. Exercise each active profile resource's bring-up/status/shutdown path.
3. From a second controller, run ordinary `list`, wait past the 30-second inventory TTL after an
   owner-local add/remove, and confirm automatic appearance/removal without importing the first
   controller's host path. Verify `list --live` only forces the same typed refresh.
4. Register a dedicated remote owner, verify owner lifecycle forwarding and direct
   `sync → list → export → remove`, rotate only a test host key, and confirm an unreachable owner
   produces the documented diagnostic/cache behavior.
5. On the two E2E VMs, run `keys trust → add synthetic shared/exclusive records → sync →
   concurrent compatible and incompatible heads → resolve → exclusive move`; verify pinned tools,
   the persistent timer, SSH transport, consumer permissions, redaction, and payload absence from
   argv/env/log/diff.
6. Remove candidate resources and worktrees; allocation teardown remains an operator action.

Capture results outside the public repository and never include credentials or private host names.

## Adding a command, stage, or resource

- Command: add one validated registry row and a Go use case. Add Shell only for a physical leaf, then
  extend a contract/integration test. Do not add another dispatch list.
- Stage: add one Go descriptor and a typed probe/apply port; add no-op, drift, failed-verify, and
  resume coverage. Add shell only for an unavoidable physical operation.
- Profile resource: keep mechanics below its profile, add a `.res` descriptor and executable
  handler, implement silent `is-up`, and test at least probe plus reverse lifecycle behavior.

Choose validation for these changes using the skill's risk-based test-selection policy.


## Profile extensions

Environment-profile selection resolves once during configuration loading. The authored request,
presence and provenance remain available for authoring and inventory; the normalized effective
`ENVIRONMENT_PROFILES` is the common input to extras, provisioning, resources and project admission.
An absent setting uses eligible descriptor-declared defaults, and an explicitly empty setting
selects nothing. Project-owned requests cannot expand the yard set. Import and `up` recheck
membership before mutation; `down` and removal remain available after deselection.

A valid, current guest-facts snapshot differing from the desired snapshot only in
`selected_profiles` represents pending configuration application. That difference
does not require a release transition or block ordinary commands. `config set`
saves the selection without touching the guest; normal `yard init` applies it and
publishes matching facts through the existing helper-refresh begin/commit sequence.
Version or yard-identity differences, unsafe or invalid facts, incomplete refreshes
and unfinished release transitions retain their strict readiness checks. Other
helper or profile runtime drift is still assessed independently.

The native `internal/profile` package inventory binds shipped config, provision hooks, presets,
resource descriptors and optional profile declarations. It includes descriptorless and resource-only
packages. Provisioning evaluates the assignment layer in Go, forwards only its declared public
keys and carries the complete package source binding through check, apply and verify. The producer
captures that approved bundle before starting Incus; changed sources, unsafe file kinds, hardlinks
and symlinked native references refuse before effects. Resource handlers retain their separate
in-profile intermediate-symlink policy. Retained repository directory descriptors remain pinned.
Operator `profiles/<name>/profile.env` belongs to the separate protected secret boundary.

L2 profiles declare writable dependency directories in `CACHES` and read-only toolchain assets
as `source:destination:ro` in `ENV_MOUNTS`. The runner and static security audit share the same
path policy. Native startup also checks resolved asset sources, cache symlinks and UID/GID;
existing containers must have the non-root/no-new-privileges policy or require `up --rebuild`.
Profile images own their compatible tools and writable home. See the
[project environment boundary](security.md#project-environments) and
[stateful resource lease contract](reference/resource-leases.md).

`yard profile enable|disable|setup|status <id>` operates on one shipped profile declaring
`owner_service`. Enable and disable require a running, existing yard, preserve neighboring profile
selections and invoke only the selected hook. They do not enter the general init reconciler. Setup
reuses the profile's declarative credential conversation on the owner host without selecting it or
starting the yard. Credential-owner initialization, when needed, is the only shared reconcile stage
in this path. One typed plan and confirmation precede credential import, the CAS selection write and
native apply/verify. Failed runtime apply retains the desired selection for retry; disabling a
profile preserves its shared credentials. Core tests use synthetic profiles; provider lifecycle and
external-service acceptance remain profile-owned. These commands currently require the owner host.

A container profile can declare `YARD_CAPS="host-memory"` to request the fixed read-only
owner `/proc/meminfo` bind at `/var/lib/subyard/host-meminfo`. The generic reconciler validates
ownership, repairs drift without restarting the yard and verifies the live procfs source before
publishing its runtime. This supplies physical memory counters when LXCFS virtualizes the
container's view; it grants no host shell or Incus access. VM profiles use their own physical
procfs counters. Roles that forbid host access also forbid this capability.

Optional behavior and its tests belong to `config/profiles/<name>/`. A shipped `profile.json`
(schema version `1`) extends existing profile provisioning with declarations read by
`internal/profile`; profiles without that file retain their existing provision hook behavior.
This is a local shipped-package contract, not a registry of operator-supplied executable code.
Unknown fields, unsupported versions, unsafe relative paths and duplicate consumer IDs fail closed.

- `default_yards` supplies selection when `ENVIRONMENT_PROFILES` is absent. An explicit list,
  including an empty list, wins; matching `disabled_when` conditions disable selection.
  Config resolves one effective set for all consumers, with requested values and presence kept
  separately for authoring and diagnostics. Provision hooks without a descriptor are never
  selected implicitly. `selected_provision_only` remains readable compatibility metadata;
  every provision hook now uses the effective set.
- `native` lists Go package directories and artifact paths relative to the profile. Development
  and release builds discover these declarations; installed profiles contain the native artifacts.
- `consumers` declares credential ID, zone, relative materialization path and format (`file`,
  `rsa-private-key` or `rsa-private-key-with-settings`). The latter stores the setup's declared
  fields and RSA key in one encrypted, atomically materialized JSON revision. Optional `legacy_path`
  identifies the previous relative consumer path for migration and cleanup. Zone `*` accepts any
  validated credential zone; a single `{zone}` filename
  placeholder expands to that zone. Dynamic paths reserve their parent subtree, and overlapping
  materialization paths fail before execution. An optional `stop_handler` names a shipped executable
  that receives dispatcher path, yard context and zone, with no credential payload. The internal
  exchange boundary invokes it only after consent and requires successful stop verification before
  publishing an exclusive assignment. Hook output is bounded and discarded; cancellation terminates
  its process group. Core owns protected storage, transfer and generic format validation.
- `credential_import_exclusions` declares up to 32 alternatives, each containing 1–8 literal
  slash-delimited directory fragments of at most 256 bytes. Import rejects a canonical source path
  when all fragments of any alternative occur anywhere in it, in any order. Fragments are normalized
  directory names, without traversal, wildcards or control characters. Only shipped declarations
  supply this policy; generic coding-tool authentication-store exclusions remain core-owned.
- `setup` declares nonsecret fields, prompts, config filename and an owned credential consumer.
  Interactive init and scoped profile enable/setup prepare these inputs before their single confirmation. They check
  descriptor/config/source drift before applying; automation never answers profile prompts.
  `sync_fields` includes declared fields with the key in the encrypted consumer; local key override
  paths remain excluded. Existing protected settings and key-only entries can migrate under normal
  init consent; synchronized settings restore without interactive input. Validation is generic;
  concrete fields and broker behavior remain owned by the profile.
- `owner_service` names an executable Bash hook. Core invokes it only with prepared engine context,
  passing `SUBYARD_PROFILE_SELECTED=0|1`, including unselected profiles so they can clean up.
  `--check` inspects convergence; `--yes` reconciles; `--remove` removes owned service state.
  `--pause` writes exactly `paused` when it stops an active service, otherwise nothing;
  `--resume` restores that service. Core retains the paused profile IDs and restores earlier
  services if a later pause fails. Hooks own their service-specific identity and recovery guards.
  `SUBYARD_PROFILE_STOPPED=1` asks readiness checks to honor the yard's stopped intent.
  Init's post-apply check passes `SUBYARD_PROFILE_POST_APPLY=1`; a hook may then accept
  an external source that apply could not fetch and warned about, while its ordinary check
  schedules the next init's retry.
  Readiness drift returns exit 1; other native failures retain their exit codes. Hooks may
  write exactly one `subyard-profile-readiness-v1 CODE` line to stderr, where `CODE` is
  `boot-link-missing`, `boot-link-target-mismatch`, `unit-fragment-mismatch`,
  `boot-dependency-unloaded`, `unit-inactive`, or `transport-unavailable`. The dispatcher
  captures at most 128 bytes and discards malformed, duplicate, oversized or arbitrary
  output. Adapters opt in with `SUBYARD_PROFILE_DIAGNOSTICS=1` to receive a validated
  `reason=CODE` after the existing owner/native-exit check record; callers without the
  opt-in retain that original record. Scoped human status displays `diagnostic: CODE`
  from the same observation. Runtime observation and status JSON stay unchanged.
  A converged check (exit 0) may instead write exactly one
  `subyard-profile-advisory-v1 CODE` line, where `CODE` is `storage-capacity`; the same
  128-byte capture and opt-in apply, and the dispatcher reports it as
  `profile service advisory owner=PROFILE code=CODE`. An advisory never changes
  convergence or readiness. The profile declares each code it may report under
  `advisories` with a fixed remediation of at most 240 safe characters; an undeclared
  code is not shown. Scoped status prints `warning: CODE: REMEDIATION` and adds it to
  the JSON `warnings` list, and `yard status` lists a `warning` shared row named by
  the code with the remediation as its hint.
  Scoped disable invokes only that profile's `--remove`, carrying metadata bindings for its declared
  managed paths so replacement artifacts fail stale guards before deletion.
- `startup_readiness` optionally names an executable Bash hook relative to the profile
  (it may be the same file as `owner_service`). Core calls it with `--startup-check`,
  prepared engine context and `SUBYARD_PROFILE_SELECTED=1`, for the applied profile selection.
  It must perform read-only observation and return exit 0 with one JSON object, at most
  1024 bytes: `{"state":"ready"}`, `{"state":"drift"}`, or
  `{"state":"starting","reason":"transport is starting","hint":"inspect the service, then retry yard start"}`.
  `starting` requires a public reason and corrective hint, each at most 240 ASCII characters;
  use fixed profile-owned prose, without secrets or native output. Text admits letters,
  digits, spaces and `,;:._()'/-`. Unknown fields, states, control characters and extra JSON
  refuse. A missing handler, unsafe path or non-executable file fails declaration validation.
  `drift` means stable repair work, not ongoing startup. Observe the installed service even
  when candidate payloads differ, so boot progress cannot keep changing the deferred repair
  plan. Do not treat a failed full convergence check as proof of `starting`.
  Core supplies `SUBYARD_STARTUP_REMAINING_MS` as a nonnegative decimal budget, recomputed
  for each probe from this invocation's readiness deadline. Owner transports forward it explicitly to
  guest probes; host and guest monotonic epochs need not match. Core owns expiry: a value
  of `0` must retain transient classification for the final observation, so expiry produces
  a refusal rather than turning a crash loop into stable drift. Each started observation
  gets the existing bounded observation allowance, respecting caller cancellation, even
  if little polling budget remains. A polling deadline cannot turn a healthy response into
  a probe failure. Observations and cancellation/drain may finish after that deadline;
  they cannot hang or reset the polling budget.
  Core polls transient owners concurrently once per second under one deadline and stops polling an owner
  once it is ready or has stable drift. Cancellation kills each probe's process group, with
  up to one additional second for output drain. Lifecycle-only cleanup declarations cannot
  opt in; infrastructure needs its own owning readiness contract.
- `managed_paths` declares owned `data`/`operator` paths for teardown assessment, with optional
  `{yard}` substitution; the hook still owns physical cleanup and ownership checks. Teardown and
  `init --reset` carry approved metadata bindings in `SUBYARD_TEARDOWN_ARTIFACTS`. Removal hooks
  call `teardown_guard guard-artifact PATH` from `scripts/lib/host.sh` (the engine's hidden
  `_teardown-guard` command) before service mutations and `teardown_guard remove-artifact PATH`
  at deletion. Bindings cover device/inode, UID/GID, mode, size, mtime,
  ctime and symlink targets without reading file contents. The bounded guard permits an approved
  artifact to disappear, rejects additions or replacement, and returns exit 75 for `plan_stale`.

Descriptors contain no secrets. Config and credential data stay outside immutable release roots.
Profile changes must preserve existing persisted paths and update/rollback behavior or declare a
migration. Core contract tests use synthetic profiles; concrete implementations, composition checks
and live acceptance belong to profile runners. Release verification aggregates their results as
specified in [testing](testing.md).

### Profile settings and installed runtime hooks

The optional `telemetry` profile owns agent-run semantics, native exporter settings,
process sampling and SQLite persistence outside the control plane. Generic VM owner
observation and Android's owning observer expose bounded read-only resource records;
they do not acknowledge another consumer's event journal. Existing broker acquire
correlation IDs retain their meaning. See the [telemetry guide](telemetry.md) and
[collection contracts](reference/agent-telemetry.md).

The shipped `config/profiles/*/profile.json` v1 descriptors may declare `settings`, `runtime`
and `guest_environment`. These extend the existing profile registry; operator configuration cannot
supply declarations or executable hooks. Profile setting names must not collide with core fields,
dynamic core namespaces or another profile. The loader resolves one operation-local catalog before
reading configuration layers. Validation, provenance, field discovery, authoring, sync and command
context use that catalog. Defaults retain lowest precedence. A `host_listener` port participates
in generic owner-port collision checks.

A `runtime` declaration contains an `activation_id` and relative executable `handler`. The ID is
unique across profile and core activation stages and remains durable across release transitions.
Optional `failure_scope: "yard"` declares that a typed failure of the hook leaves only its yard
pending init; see [yard-scoped runtime failures](#yard-scoped-runtime-failures). Custody runtimes
and profiles with host owner or infrastructure services cannot declare it.
The hook accepts `observe`, or `apply OPERATION_ID ACTUAL_SHA256 DESIRED_SHA256`, and emits only a
bounded JSON object with `state`, `actual`, `desired`, optional `hook_binding` and optional
`ownership_absent`. States are `absent`, `deferred`, `current` and `stale`. Installed states
carry lowercase SHA-256 fingerprints. Observe must not mutate state.
A `stale` observation may add one `consequence`: a short sentence of at most 200 bytes from a
plain-text alphabet, such as a service restart that apply performs. Update, migrate and `yard init`
plans disclose it before their single confirmation; release plans name the yards once per distinct
consequence. The text must not depend on live session state, because the plan binds it.
Core bounds execution and output, checks the assessment before apply, and verifies the resulting
state separately. Hook paths retain the updater's pinned directory descriptor; symbolic links
below that anchor are rejected. Missing/stopped yards are absent/deferred without starting them.
Hooks run in activation-ID order before integration project hooks; update and rollback inspect all local yards.
Release activation reconciles the approved owners in one pass. Stale runtimes can provide
validated configuration diagnostics when config status requests them.

A runtime may declare a positive integer `format` for its installed state. A runtime without one,
including every runtime up to v0.30, has format 1. A release that changes that format declares a new
one, and `prepares_rollback_to` names the one earlier format its handler converts state back to.
Such a handler also accepts `rollback-observe` and `rollback-apply OPERATION_ID ACTUAL_SHA256
DESIRED_SHA256`. Both follow the output, assessment and verification contract of `observe` and
`apply`, reported against the prepared format. `rollback-apply` need not be atomic, but it must be
rerunnable from any state it can leave, and both `rollback-observe` and the active release's `observe`
report such an intermediate state as `stale`. `rollback-observe` must refuse any drift that is not
a preparation state, since a rollback from prepared owners relies on it to route each yard.
[Release migrations](#release-migrations) describe
how update and rollback use the declared formats.

Releases up to v0.30 decode `profile.json` strictly and cannot read a release that declares `format`.
Their own `yard update --rollback` refuses such a retained release as unable to verify installed
runtime activation, and changes nothing. From such a release, the way back is the forward
`yard update --version VERSION`.

Unpublished `.subyard-config.*` files left by an interrupted materializer are outside its explicit
artifact inventory. Later observation and replacement ignore them, including similarly named
symlinks, without reading or deleting unrelated directory entries.

The Orca profile retains its existing `orca-runtime` identity and wire fingerprints.
`ownership_absent: true` is valid only with `current` and its ordinary equal SHA-256
digests. It proves no owned artifacts remain for retained-owner admission; successful
cleanup still converges to the approved desired digest. It does not relax apply verification.

A manifest may still declare `runtime.custody: true`, and a declaring handler may still accept
`custody-observe`, `custody-apply` and `custody-fence`. These actions belonged to the removed
native release-owner protocol; the engine no longer invokes them. It still invokes the ordinary
`observe` and `apply` actions and, for developer identity owners, `identity-observe` and
`identity-apply`.

GitHub declares `github-runtime`, reusing its owner-service lifecycle for release delivery,
restart and verification. Its authenticated status includes the loaded broker's executable
SHA-256 and protocol; `subyard-github status --runtime` returns the loaded broker and guest
client identities. Ordinary status retains its configured-only JSON. Readiness requires the
installed owner and guest files and both loaded identities to match the shipped executable.
No GitHub API call or key import is required for runtime activation.

Git-fetch declares `git-fetch-runtime`, reusing its owned guest lifecycle and
installation lock. Release refresh checks exact worker/unit assets and loaded
systemd policy, preserves a disabled timer, and leaves absent installations
absent even when selected. Deselected installed files remain release consumers;
refresh keeps their timer disabled. Observations bind public file metadata and
manager readiness without inspecting repositories, URLs or credentials.
Ordinary apply, pause, resume and release refresh invalidate an older custody generation under
the existing guest installation lock before their first effect; an already current no-op does not
change it.

Core also registers `core-guest-runtime` and `key-tools-runtime` for already installed helpers.
Inspection of an exactly matching completed release validates installed consumers
against their applied settings; ordinary init continues to assess desired settings.
The protected integration inventory retains validated non-secret applied inputs
only after successful native verification. Its complete snapshot preserves absent
inputs as absent. Inspection reads the protected applied developer identity before
validating home-bound inventory and released paths, then retains the ordinary
strict path and ownership checks for that applied owner. Generic profile observers
receive the same applied target only during exact completed-release inspection.
Without an integration baseline, current machine-bound guest facts and the protected
core skills receipt can establish a unique home/passwd identity; absent evidence
never authorizes an inferred owner. Core guest observation retains its own receipt
projection, and host key observation remains independent of Incus. Ordinary stop
and teardown bind the applied developer identity before confirmation and recheck it
before physical owner-service and session guards. Only that identity enters those
hooks; desired power, resource scope and provisioning inputs stay unchanged.
Full teardown retains the nonsecret identity in its existing per-yard recovery
context, while old resumed plans retain their original context. Legacy capture requires a converged
native observation and a fresh fingerprint CAS before desired authoring. Failed
application retains the previous inputs. Neither projection nor capture suppresses
source, ownership, missing-file or native runtime readiness checks, and unfinished
release transitions retain their complete desired activation contract.
Provisioning's preview/ccusage ownership markers authorize bounded helper refresh; missing
artifacts with those markers remain drift. Unprovisioned guests without the markers are absent.
Preview delivery, capability skills and the pinned ccusage installer are shared with ordinary
provisioning; release refresh does not rerun base packages or profile provisioning. Skill refresh
updates static content for successfully registered profile providers, removes disabled providers,
and checks canonical bytes, permissions and discovery links without restarting profile services. Existing tool publication
or a durable key-identity file permits age/SOPS repair; activation never initializes credentials.
The tool installers publish protected receipts binding verified archive pins and installed
binary SHA-256 values. Missing receipts require a checksum-verified installation, rather than
accepting an existing version string. An installed credential timer receives refreshed unit
files and manager reload while retaining enabled/running intent. A protected ownership receipt
or exact shipped legacy unit recipes must establish timer ownership before refresh; a same-name
foreign unit is refused without changing either peer. The first verified legacy refresh publishes
the receipt. Both absent timer units stay absent, including when an old receipt remains. Host key
tools and timers are inspected independently of Incus installation or guest availability. Stopped
guest deferral is expected drift and does not emit a warning or require a separate init command.

Runtime owners bind safe fresh recovery plans to complete per-yard native observations after
partial delivery. New authorization and the native compare-and-swap guards remain required;
completed migrations are not replayed for runtime repair. The input-free schema-2
`runtime-activation-capabilities` process probe reports the sealed engine's compiled activation
IDs. It leaves frozen recovery capability responses unchanged. Rollback refuses a retained
release that loses an installed profile's runtime owner, including deselected owner or guest
artifacts, or a selected guest-only runtime contract. When host receipts are absent, before
losing an owner admission asks that one native observer to prove guest absence; stopped, missing or failed
observations cannot prove absence. That observation is rechecked before effects.
A historical owner without the new probe may retain
the identical installed core helper payloads, timer templates and native archive pins; different
installed contracts require an explicitly advertised target activation owner. Proven absent
consumers impose no payload or pin requirement. Each installed local yard uses its own effective
pins and tool roots; stopped guests remain conservative when absence cannot be proved. Unavailable
observations refuse rollback. Refusal happens before runtime-link mutation, and installed presence,
metadata and effective settings are rechecked after consent.

The separate input-free schema-2 `rollback-source-activation-capabilities` probe advertises the
exact `rollback-source-activation-v1` tuple: process schema 2, journal schema 2 and receipt schema 0.
It grants no execution or recovery authority. Before offering a fresh affected rollback for
confirmation, the caller validates the ordinary typed inspection and this capability from the
verified retained engine. Mutating source decisions, including unknown source scopes, require it;
known activation, metadata and legacy-V1 identity-only work does not. Preserve-only and ledger-only
plans keep the ordinary compatibility path. Missing, malformed or mismatched probes refuse;
release versions alone do not establish support. Execution rechecks the exact sealed owner.
Completed history for a different goal does not supply or suppress the new goal's source plan.

Optional `runtime.projects_changed_hooks` declares installed hooks owned by that runtime.
Entries must be unique, clean absolute paths directly inside
`/usr/local/libexec/subyard/projects-changed.d`, without carriage returns, line feeds or NUL. Core captures the
bounded installed hook inventory, source fingerprints and project-root scope before consent,
then rejects expansion or changed captured inputs before invoking a hook.
Core binds each registered project root's directory identity (mode, owner, group, device,
inode), its `src` identity and the content of its `.subyard-meta.json`, without traversing source
trees. Other workspace entries are checked only for membership: a registered root must remain
listed, while added worktrees, build outputs and unregistered siblings do not stale the plan.
The installed dispatcher still compares its own observation with the latest host observation
before any hook runs; init observes again and dispatches up to three times in all when only that
observation moved, then reports that workspace entries kept changing.
Native hooks own discovery and bind their known nested-root inputs through `hook_binding`.
An observation's optional `hook_binding` is a lowercase SHA-256 digest of safe native
registration metadata, known-root identities and Git-resolved repository locations.
Core retains it privately and supplies `SUBYARD_PROJECT_HOOK_BINDING` to the corresponding
declared hook for its native compare-and-swap check before effects. It is not a public plan fact
and must not fingerprint credentials, protected contents or secret values. Genuinely unavailable
observations may resolve within the explicitly approved conditional hook and root scope; this
does not authorize additional hook paths or project roots.
A hook exits nonzero only when its pass failed or its state is undetermined. Retained state
that the owner accepts is not a failure; the owner's status command reports it. A failed
hook during release activation names the yard and the fixed failure class (unsuccessful
exit, timeout, cancellation or unavailable execution) and never includes hook output.
One successful, verified hook invocation satisfies its prepared operation, including when
integration repair runs it before committing ownership. Repeated internal calls still verify
captured inputs; a new explicit init prepares a new plan and retries the hooks.

A `guest_environment` handler accepts `check|ensure DEV_USER` through the existing root guest
execution boundary. Its source is read only from the validated shipped profile. `check` reports
readiness; `ensure` performs the previously assessed repair. Profile selection does not suppress
repair of an installed service. Core owns transport, confirmation and orchestration; each profile
owns its service paths, diagnostics and restart mechanics.

Profile declarations may expose a `dashboard` with an owned listener setting, container marker
and device, guest port, scheme and path. The owning owner service/runtime binds route and guest
readiness. Core projects selected, ready dashboards into shared status and renders VM/remote
SSH instructions using shipped metadata, without extending the closed owner-response schema.
Selected dashboard owner checks and container URL verification share one 30-second read-only
work deadline, respecting shorter caller deadlines. Exhaustion leaves unverified dashboards down
and launches no further checks; cancellation terminates the check's process group with up to
two additional seconds for cleanup/drain. Direct remote yard-status processes have a 38-second
execution timeout plus their existing two-second cleanup/drain allowance. SSH trust setup retains
its existing timeout policy. These bounds preserve the full owning readiness check.
Removed integrations retain only inert selection and discarded-input compatibility metadata,
without guest cleanup, proof publication or automatic starts.
An observation that reads guest services waits, within its observe limit, for a running guest
that started recently and is still booting; a guest that is stopped, has been up longer, or does
not answer within that bound reports `native-unavailable`.

Full uninstall is a scope of the existing prepared teardown. It aggregates local yard executions
through the same observation, stale checks, apply and verification methods. `internal/uninstall` owns
ancestor-pinned filesystem inventory, removal and private publication, serves shell and profile
adapters through the hidden `_teardown-guard` engine command, and carries installation ownership and the exact durable retry record. Installer entrypoints record actual
runtime/data/config/cache/bin and shell integration paths; project and shared-resource owners
persist public installation contexts while setup is still partial. Host mount setup records
which namespaces it created, retaining pre-existing content as foreign. Assessment performs
no elevation and captures native metadata rather than credential or settings payloads.

Full uninstall does not require migration readiness or execute a migration, regardless of source
version or pending/invalid V1/V2 journals. CLI scope selection precedes teardown release admission;
only selected-yard teardown uses that gate. Full removal retains its independent installation
ownership, confirmation, session, stale inventory and saved-cleanup recovery checks.

Shipped profiles may contribute a `full_cleanup` executable and a bounded
`full_cleanup_environment` allowlist. The read-only `--assess` response declares exact artifact
bindings, system/user unit groups, owned data namespaces and preservation diagnostics. Core
checks the generic contract and synchronously drains approved units before their data. A
`conditional` response adds a shipped read-only `--verify-private` proof after consent,
before stopping producers, again after draining, and before resource removal. The saved
nonsecret source context cannot expand during retry. Unknown private ownership refuses
dependent cleanup; private record contents never become adapter output or recovery data. A
`lifecycle_only` declaration contributes cleanup hooks and exact managed paths for full installation
cleanup, without provisioning, activation or enablement. Its paths remain outside selected-yard teardown;
the test-VM host sink uses this contract. Per-yard profile services continue through their
existing owning `owner_service` hooks.

The prepared local mutation boundary holds a stable operator/installation lock outside removed
roots and checks the prepared installation generation after acquisition. Full retains the owner
lock inode while atomically publishing a separate nonsecret epoch beside it. Interrupted
private epoch publication resumes the saved before/after intent; old queued mutations and
audit/cache writers refuse the advanced generation, including when foreign root contents remain.
Read/session writers require existing installation ownership and skip pending uninstall roots.
Full retains the host network lock through resource, data, runtime and recovery verification.
An interrupted caller's physical file leaf must retain that same owner barrier before continuing.
Private unreadable trees
receive only an exact conditional root approval; the privileged leaf durably publishes refined
native metadata before its first unlink. Fresh-process retries permit disappearing approved
entries and refuse replacements, additions, unsafe permissions, symlink ancestors and mounted
owned descendants. Explicit owner namespaces preserve unrecorded content in custom roots.

### Owner infrastructure services

A shipped profile can declare `infrastructure_service` with a unique `id`, `default_yard`,
`required_settings`, relative `route_root` and optional executable `binding_handler`. This is a runtime
role; ordinary prepared init, power and teardown remain its lifecycle owners. Core never identifies
services from a container or profile name. The test-vms profile owns its broker and allocation semantics.

The optional `registration_activation` binds the service to its declared native stage owner.
A forward transition, including a same-release gate, recreates a registered runtime whose engine
changed, as its stage owner reports, instead of updating it in place; settings and other drift
with an unchanged engine are repaired in place by the native owner. After proving the exact registration (owner, yard,
project, instance, UUID and ownership) it interrupts the runtime's open SSH sessions with the
approved migration stop, runs ordinary `yard -Y YARD teardown --yes`, then
`yard service install ID --yard YARD [--pool POOL] --yes`. The plan names the interrupted sessions,
the deleted instance, Incus project and `/srv` volume with their VMs, leases and base images.
The shared bridge, storage pool, guard binding, power reconciler and yard settings are preserved,
including for the last managed yard; the power owner updates its retained unit. The registration
is recreated with the new runtime UUID. A running runtime and a stopped one
whose desired power is `running` are recreated when their engine changed; a runtime stopped by
the operator is left untouched. An unproven, foreign, remote or multiply registered runtime keeps its previous engine
with a warning naming `yard service remove`, then `yard service install`; later gates keep that
refusal. Failed Incus or registry reads block the stage with a retry. Matching settings without a
runtime, including after a failed install, warn on every readiness check with
`yard service install ID --yard YARD [--pool POOL]`; a registration left by an interrupted teardown
adds `yard service remove ID [--pool POOL]` first. An unfinished init warns the same way. Such a
runtime keeps its declared state in the plan, so a retried update resumes an interrupted
transition and finishes with that warning. Rollback to an older release
updates the runtime in place through that release's own activation.
Without a registration the native owner updates the runtime in place, and registration attaches
metadata after that owner reconciles. Its conditional plan line names the service
and default pool; successful registration reports the exact runtime. Exactly one proven owned
runtime is registered; several candidates and proof/write failures report guidance without changing
readiness. Zero runtimes produce a hint only when matching yard settings exist. Observation and
same-release repair remain native. Existing entries,
including interrupted publications, are never changed; explicit `yard service register` repairs them.
An unreadable yard configuration withholds registration because its service role is unknown and is reported separately.

The protected owner registry is `$SUBYARD_HOME/services/SERVICE/POOL.json`. Each binding carries
owner HostID, exact yard/project/instance, native UUID, stable installation ownership digest and a random
registration generation. Registration verifies native ownership again under the ordinary owner mutation
barrier. A same-name resource cannot replace it. The safe public projection is published under the
profile's `route_root/services/SERVICE/POOL.json`; it contains only those public identities and a yard
route locator. It contains no full `Context`, configuration roots, owner SSH endpoint or lease authority.
Readers require exact protected modes and ownership. A registration written before an interrupted
public publication remains unavailable until repeating registration repairs that same generation.

`yard service` manages the generic binding and delegates install/start/stop/update/remove to the
existing prepared lifecycle. Service address resolution precedes workspace configuration inspection;
remote owners use their registered trusted connection and the same exact owner plan/execute protocol.
Plans bind registration generation and native runtime identity. Ordinary teardown retires captured
service metadata after native cleanup; full uninstall includes the registry and profile publication paths.
Retained runtime admission refuses loss of an installed service declaration/publication contract.

The optional binding handler observes through `--project NAME --instance NAME`. It emits
bounded JSON `{schema_version:1,binding:SHA256,facts:[SAFE_TEXT]}`. Core pins its source and validates
its bounded public output, captures its digest/facts for service stop/removal and compares them before
effects. After consent, `--apply-binding SHA256` must fence admission atomically with its native scope
check; it returns exit 75 for stale scope. `--resume` reopens admission after explicit service start.
The ordinary lifecycle retains physical drain/cleanup. A stopped runtime uses the existing exact native ownership/no-op rules. The test-vms handler
projects allocation state, slot generation and lease epoch, excluding heartbeats/deadlines and secret
capabilities. Agents consume only the trusted public facade publication, retaining the captured route
for their existing lease. They gain no owner lifecycle, Incus or service-container shell access.

The test-vms native fence uses the existing lease lock and publishes pool schema 3 only while
admission is closed. New readers support normal schema 2 and fenced schema 3. Old readers reject
schema 3 before writes, so retained workers cannot drop the admission guard. Existing holders remain
renewable/releasable through the current worker. Normal broker startup and explicit service start
restore schema 2/admission; registration and ordinary unfenced operation retain schema 2. If stop is
interrupted after fencing, admission stays closed until explicit start or a completed stop/start retry.
An older installed broker can be registered and observed without restart; fencing refuses until
`service update` installs the supported broker. Remote service callers also require `owner-services-v1`
before using old command names, preventing fallback to a historical owner's selected-yard routing.
Owner-service SSH transport uses trusted installation paths independently of the controller's workspace
configuration. If the RPC session yard is invalid, the existing stdio endpoint advertises a reduced
capability set and accepts only declared owner-service plan, execute and discard calls. Other reads
and mutations requiring an unadvertised capability return `capability_missing` before native effects;
undeclared methods return `method_not_found`. Planning a non-service command in this reduced context
still returns `owner_configuration_invalid`. A service plan resolves its real runtime context before
the existing release/admission gates; the restricted session does not
bypass those gates. Remote status validates native service settings on the selected owner.

## Selected-yard cleanup

`cleanup` defaults to cache-only soft cleanup. Selected profiles own explicit
`CLEANUP_CACHES` declarations; the core uses descriptor traversal that preserves
roots, metadata and nested mounts without following symlinks. General temporary
storage and runtime relocation are outside this operation.

Hard cleanup keeps a private checkpoint under the owner data home's `cleanup/`
directory and a token-owned independent native volume copy in the default Incus
project. It stops the selected yard before copying. For inspection, it boots the
original yard on its own disk, then attaches a read-only copy by explicit device
identity; duplicate VM filesystem UUIDs are never used for mount selection.
Native copy completion and project content verification precede destructive work.
Existing reset teardown guards remove the old environment. Normal creation and
provisioning restore a fresh environment from the known configuration; project
wrappers are restored before provisioning/hooks, excluding every external bind
source. Exact registrations, reservations, modes, symlinks and hardlink topology
survive. Backup remains available until the result is verified.

Interrupted cleanup resumes through `cleanup --hard` for the same selected yard.
The original guest must support bounded backup inspection; an unbootable operating
system requires separate recovery. A selected migration's old journal binding can
be retired only by an exact receipt tied to the recreated native identity; shared
history and other yards remain under the existing release-transition contract.

### Developer identity reconciliation

Plain init can carry a same-UID/GID account rename as a disclosed prerequisite of
its existing plan. The approval binds desired configuration, source, native yard
identity, project inventory and the conditional ordinary init scope. After the
prerequisite, init discards old account observations, reassesses, and refuses any
new scope before consuming the original approval for the normal continuation.

For a stopped yard, a protected local hint records only the verified applied
identity, native yard UUID and source/release binding. A matching current hint
avoids repeated inspection boots; a missing legacy hint requires one disclosed boot.
The hint never authorizes account changes. A new transition captures normal
post-boot service state; an interrupted one retains its original approved owner
intent and re-quiesces those owners after a reboot. Until init commits the rename,
completed-release observations use the verified applied identity; without an
integration inventory or core skills receipt that establishes it, authoring refuses.

Profiles declare an optional `developer-identity.json` sidecar with schema `1`
and mode `rebind` or `independent`. Rebind owners implement `identity-observe
<target-user> <source-binding>` and `identity-apply <source-binding>` through their
runtime handler. Their bounded recipe carries original and target account identity
and owned state. Apply reads the fixed protected guest journal through the core
authority helper, quiesces at `prepared`, then rebinds at `account-ready`. Owners
preserve unrelated state and accept only their recorded old or projected new
artifacts on retry. Core does not rewrite arbitrary profile JSON or branch on
profile names; an installed owner without this contract refuses identity changes.
A refusing owner prints one stderr line `<Owner> identity: <reason>`, built by the
core authority helper's `refusal`; core shows only that line. Reasons that are not
fixed owner prose become a generic retry line, so file contents never reach the
operator.

A private host marker and guest journal fence other mutations during recovery,
including direct and RPC operations and runtime installation. Independently assessed
full removal retains its existing producer fencing and deletion guards. Only plain init
with the same captured desired configuration and source may continue. Account
mutation fences new SSH sessions, requires an empty developer UID, retains numeric
ownership, and moves only the canonical home on its existing filesystem. Shadow
account and subordinate-ID recovery accepts only captured old or projected rows
under native-compatible locks. Core publishes the applied identity last, after
owner and metadata verification, and retires identity authority before ordinary
init continues. A subsequent hook failure retains the new identity and the prior
applied values of other settings.
