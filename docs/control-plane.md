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

Integration selection uses the shared prepared-command boundary, the protected per-yard config
writer and `reconcileruntime.IntegrationPlan` / `ApplyIntegrations`. The same runtime path is used
by full initialization after core substrate provisioning. `scripts/reconcile-integrations.sh` is a
bounded package/proxy/project-hook leaf. A per-yard lock serializes confirmed desired publication
and reconciliation. Guest ownership evidence covers structured fields, plain files, instruction
files, derived session links and known service receipts; evidence conflicts preserve artifacts.
See [per-yard selection](configuration.md#per-yard-coding-tool-selection) for user-visible semantics.

Explicit initialization also binds the selected yard's authoritative project records and
guest metadata observations before approval. Its generic metadata step atomically restores
missing canonical metadata before the installed project hooks run. Matching legacy metadata
is preserved. Missing roots, unsafe ownership or paths, conflicting identity and stale
observations stop initialization; guest directories never authorize additional projects.
Legacy bind wrappers and sources may remain root-owned with the developer's group
and without world-write access. Only captured missing metadata in a non-writable
legacy bind wrapper uses privileged publication; the new file belongs to the developer
with mode `0600`. Wrapper and source ownership and permissions remain unchanged.
Read-only project inventory and status do not perform this repair.
When missing metadata changes a profile's native hook catalog, its runtime observer
can consume an optional stdin object with `schema: 1` and `missingMetadata` entries
containing canonical `root` and metadata `payload`. This is a read-only projection:
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
active operation ID, and a bounded writer queue closes a client that cannot keep up.
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

### Release migrations

Release-owned one-time transitions are declared in
[`config/release-transition.json`](../config/release-transition.json). The
[`internal/releasetransition`](../internal/releasetransition) module owns read-only inspection,
authorization binding, protected evidence, per-domain epoch advancement, stable runtime links,
forward recovery and post-activation reconciliation. A completed one-time migration is durable
history and is never reopened to repair later drift.

Rust release ownership is experimental and explicitly built/checked. Ordinary builds,
packages, CI and releases continue to use the Go owner without Rust tooling.

The Rust release owner in [`release-owner/`](../release-owner) implements the whole
release operation: inspection, release policy, consent admission, update lease, protected
history and journals, link publication, activation, recovery and retirement. Go retains
CLI/RPC presentation, frozen format codecs, historical exact owners and the scoped native
component helper. Component recipes and native observations carry their owning semantic
contracts; the helper does not choose aggregate release eligibility or call the Go reducer.
The frontend integration remains gated pending complete functional and candidate acceptance.

A manifest-bound `config/release-owner.json` declares the exact Rust owner and Go helper.
Verification checks the complete inventory and seals both executable images before any
execution. A retained frontend preparation holds its original private owner session through
consent or close; a public plan digest alone does not recreate execution authority. Late
native admission transfers the original account lock open file description after consent
or checked durable continuation. The helper retains that descriptor and the update lease
through accepted native work and drain. All holders close descriptors without explicit
unlock. Cancellation or owner death can leave an unknown native result; neither proves
closure or permits automatic replay.

A complete frozen journal can still have terminal owner archive cleanup pending. That
continuation uses the original owner preparation, token, resume plan and transaction;
it receives no new consent grant. Rust checks the original retained bundle and source
artifacts under the update lease before cleanup. Missing original facts block pending
cleanup, while already clean history does not require artifacts that normal retention
has expired. Private checked release references protect pending evidence; a missing
reference field is a protocol error, never an empty retention set. The frontend retains
verified original directory descriptors through its session. The release frontend and installer
currently perform no runtime-directory garbage collection; protected history and native transaction
retirement remain owner responsibilities. A different requested goal must finish this original
cleanup through its supported path before a fresh plan is admitted.

Owner selection precedes inspection. A declared Rust runtime uses Rust, a supported
historical target uses its exact sealed Go owner, and pending work retains its original
owner and artifacts. No failure chooses a different reducer. Once a verified Rust contract is
selected, image, admission, startup and protocol failures preserve that selection; they cannot
enter the historical Go completed-metadata compatibility path. Frozen process protocol v1
and its schema-2 recovery contracts remain unchanged; private owner/native messages are
separate. The narrow historical registry-absent rollback and standalone delegate contracts
retain their original scope and compatibility guards. See the
[ownership and recovery design](reference/rust-release-owner-design.md).

When switching releases after source migrations are complete, materialized-config
activation observes and reconciles all registered local yards, using the same scope
as the completed release's readiness check. Fresh plans whose prepared source work
only advances the ledger also assess all local yards before authorization. Actual
settings, registration or ingress mutations retain their selected-yard scope across
recovery because they can change configuration or rename registrations. Historical
journals keep their recorded scope; a reconstructed all-local scope may resume only
when its complete observation binding matches that journal exactly. Config
reconciliation leaves stopped or absent yards untouched.

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
an automatic migration from `start`. A later activation repair requires a fresh assessment
and authorization.

The standalone installer completes the supported current-release transition after creating
the first runtime link and before publishing CLI entrypoints. Its initial installation
consent includes this boundary. An interrupted first installation retains protected recovery
evidence; rerunning the installer uses the existing-runtime transition to resume it before
publishing entrypoints. Publication-only staging leaves migration state untouched.

Activation observers do not print diagnostics. Return expected drift as
`Converged: false`, persistent notices as `V2ActivationObservation.Warnings`, and
failures as errors. The operation boundary renders current `Outcome.Warnings`
once, sorted and deduplicated. Public error details use `ActivationDiagnostic`;
raw guest errors stay private.

The historical AI Observer [typed observation pilot](reference/typed-semantic-observations.md)
used one package-owned Go producer for native projection and semantic hashes.
Mount records form an unordered multiset; other arrays preserve order. Native
helper executable bytes bind the package. Its optional closed failure transport
retains the integration, yard and inspect/apply/verify phase without changing
public observation, process, journal or grant schemas.
Recoverable reconcile failures retain validated public diagnostics after reobservation;
ambiguous state and protected transition guards retain their own recovery instructions.
The original public failure also remains visible when recovery observation adds a blocker
or cannot establish a safe state. The recovery status and next action continue to describe
the controlling guard. Settings blockers identify the yard without exposing assignment values.
Configuration failures retain the failing yard, inspect/apply/verify phase, source role,
path relative to config home, canonical key and line when known. Their public reason is fixed;
assignment values, arbitrary parser text and guest stderr are never rendered. A suggested
`yard -Y <yard> config status` remains read-only during recovery and preserves this source context.
Post-apply observation failure retains its adapter diagnostic; invalid observation requires
operator action, while valid remaining drift stays recoverable.
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
ask the owner to close sessions and stop/start that yard. Intentionally stopped yards
retain their deferred policy marker and verify the actual filesystem when running.
Initial observation failures also retain these reasons; repairable drift stays a state.

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
and journal. The CLI's stopped message confirms physical power state, not cleanup
success. A hard stop provides no shutdown-cleanup guarantee; an already stopped
yard remains stopped. Journals across reboot depend on guest journal persistence.

Runtime cleanup is hourly, with an initial check 15 minutes after boot, and only
ages the explicit opt-in `/tmp/subyard-disposable` tree. Files and directories
whose modification time is older than one day are eligible. Active producers
must hold a BSD `flock` on their own descendant operation directory for the full
operation; a lock on the configured root or an ordinary open file descriptor
does not provide that protection. Fresh or locked data is preserved. Generic
`/tmp` content is outside this additional timer; vendor tmpfiles rules and the
vendor clean timer remain unchanged. Service runtime directories should normally
use systemd `RuntimeDirectory=` instead. Cleanup refuses symlink, foreign-owned,
incorrect-mode or mounted traversal roots, including a bind mount at the opt-in
root. A transitional live tmpfs `/tmp` also refuses whole-root cleanup until the
requested restart activates disk backing. `/var/tmp`, `/srv`, credentials,
workspaces and operation-owned producer scratch are outside blanket cleanup.

To roll back cleanup, disable `subyard-tmp-boot.service`,
`subyard-tmp-shutdown.service` and `subyard-tmp-clean.timer` inside the selected
guest, then stop the timer and shutdown service. The latter reports a deliberate
refusal when manually stopped; active temporary files remain intact. After
verifying exact policy content, root ownership and modes, remove only those
three unit files, `subyard-tmp-clean.service`,
`/usr/local/lib/subyard-guest-tmp` and `/etc/subyard-guest-tmpfiles.conf`, then reload
the manager. Keep the disk policy's `tmp.mount` mask unless separately reverting
disk backing. Repeating initialization restores cleanup without clearing live
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

An installed runtime whose bundled installer predates immutable `--publish-only` cannot be changed
retroactively. Re-running the verified standalone bootstrap is its one-time bridge: the downloaded
installer publishes the candidate without touching stable links, then the candidate performs the
ordinary assessed `ReleaseTransition`. The superseded runtime is never allowed to call mutating
`_migrate` verbs against the candidate.

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

Migration-bearing owner defects use a separate `migration-owner-capabilities` probe and
`migration-ledger-owner-replacement-v1` contract. Only an explicitly selected verified standalone
candidate (`--offline` with its exact `--version`) can request it. Its candidate-native proof reads
canonical protected records and sealed assets directly, validates the independent domain histories
from frozen projection to checkpoint authority and all completed evidence triples, then assesses
current candidate activation. Initial support pins the exact official Linux amd64 v0.21.0 owner;
retained terminal artifacts may belong only to its journal's exact source or previous release.
The previous release is independently sealed and must have the identical verified source registry.
Receipt replay validates these identities and evidence; recorded transaction IDs remain protected
from cleanup. Other earlier releases and arbitrary same-registry targets are unsupported, and
unknown artifacts and graphs remain blocked. A fresh grant publishes immutable typed predecessor
and initial-successor evidence under `release-transition/migration-owner/v1/transactions` before
current-journal CAS. The original activation remains unfinished superseded evidence; ordinary V2
resume belongs to the candidate successor. This exception does not widen either activation-only
recovery contract, change frozen V1/V2 wire meanings, or bypass retained rollback gates.

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
1 MiB rotations without loading yard configuration or Incus. Explicit yard selectors continue
through normal owner routing.

Introduce a new protocol by first shipping support alongside V1 while continuing to send V1.
Only a subsequent release may replace the default protocol with owners that support it. An optional
same-owner recovery extension may use its separate contract only after an explicit capability probe
and proof that retained V1 callers can read, select and resume its unchanged V2 successor. Retain
the old reader, writer and semantics throughout the supported upgrade, retained-runtime rollback
and unfinished-transaction recovery horizon. A runtime version bump alone never changes these
contracts. Do not weaken validation or silently discard a new safety requirement to fit an old
response: the compatibility adapter must preserve its meaning.

Before publication, `dev/verify-release-upgrades.py` runs the unmodified, checksum-pinned v0.11.2
and v0.18.1 updaters against the built candidate on the runner's architecture. It verifies inspection,
activation, the completed fixed point, completed activation drift inspection, rollback and forward retry.
It also kills a real update after journaled activation and uses the old updater to resume the same
authorized candidate transaction. This released-binary check complements tests of the frozen codecs;
rebuilding both ends from current source does not establish cross-release compatibility. Both released callers also exercise checkpoint-reader
bridge ordering and rollback refusal after authoritative epochs exceed the frozen projection. The v0.18.1
caller reports completed activation drift as a fresh migration-required plan; v0.11.2 retains its frozen
recovering presentation. Neither reuses completed authorization.

The same harness runs the checksum-pinned v0.17.3 engine to create and interrupt an activation-only
repair and reproduce its unrelated-template blocker. A standalone candidate with the same registry
resumes with the original assets. When the latest candidate advances the registry, the harness proves
its refusal without mutation, resumes through the checksum-pinned v0.18.6 registry/catalog bridge,
then separately authorizes the latest ordinary update and verifies the new publication epoch while
preserving historical domains. Its synthetic Incus backend changes
only observed running/stopped state; settings, ledger and original journal bindings must survive
recovery. Privileged commands are rejected by fixture shims.

Historical frozen materialization recipes retain preview composition from the bound runtime's
`config/preview/instructions.md` asset, or the exact legacy fallback for older runtimes. Recovery
restores those descriptor flags and source guards without rewriting authorized bytes. Current
materialization stops composing preview instructions; guest capability skills own new delivery.
The Release workflow retains the compatibility check's output with its other failure diagnostics.

### Legacy upgrades

The minimum supported source is **v0.11.0, inclusive**. Fresh upgrades/imports from earlier
installations and new rollback targets below that boundary refuse before changing active runtime
links or protected state. There is no V1 upgrade bridge. Existing accepted V2 history and exact
unfinished V2 owners remain supported, including retained legacy evidence. Process V1, Ledger V2
and V2 checkpoint-reader bridges have independent compatibility contracts.

For an older installation, stage verified current cleanup tooling in its existing runtime store:

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

### Interrupted release recovery

A v0.11.1 runtime can stop after activation with journal checkpoint `reconciling` and blocker
resource `transition.observation-scope`. That exact state is recovered only by the standalone
installer from a newer supported patch release; the active v0.11.1 command remains fail-closed.
Until that patch is published, do not delete the journal, ledger, transaction evidence, or runtime
links, and do not retry mutating commands. Download the official HTTPS asset and let its verified
candidate own the replacement plan, archive, compare-and-swap, reconciliation, and link update:

```bash
PATCH_VERSION=0.11.2 # or a later supported patch release
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

The command is interactive by default. Add `--yes` only for an intentional non-interactive run
after reviewing the reported changes.

The published v0.17.3 engine can also reopen completed settings migrations while resuming a
same-release activation repair. Its journal has no migration steps, checkpoint `reconciling`, and
one `yard.<name>` blocker saying the template is unsupported. A newer verified standalone
candidate with the same registry can resume that exact transaction using its engine and the
original owner's verified runtime assets. It preserves the original authorization, observation
scope, settings, ledger and stable runtime links; it does not replace the journal or migrate the
unrelated yard. Other blocked transactions retain their existing recovery rules.

If the latest candidate has a different registry or catalog, publish a verified compatible bridge
first; v0.18.6 retains the exact v0.17.3 registry and catalog. Resume through that bridge's standalone
launcher, then separately authorize the ordinary update to the latest release. The latest caller's
refusal does not permit editing the original journal, catalog or unrelated yard settings.

After the runtime installer publishes the verified candidate without activating it, use its exact
launcher path to inspect and resume the installed release before retrying the ordinary update:

```bash
"<runtime-root>/releases/<candidate-release>/bin/yard" -Y default migrate --check --json
"<runtime-root>/releases/<candidate-release>/bin/yard" -Y default migrate
yard migrate --check
yard update
```

The standalone candidate's blocked update reports this recovery path. The installed v0.17.3
launcher cannot perform the delegated repair. Keep its configuration and durable transaction
records intact until recovery completes.

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
at user scope. Current instruction adapters preserve host text and omit recognized old managed
preview blocks. Runtime refresh migrates inventory-owned guest blocks only with matching content
and metadata evidence; it preserves unmarked text, refuses ambiguous/drifted content, and releases
retained text when its host source disappears. Host instruction sources remain unchanged. Historical
frozen operations keep their original materialization recipe for recovery.

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

The fixed root-owned `/run/lock/subyard-network/policy.lock` serializes policy application
and managed starts. Host network setup, boot-reconciler installation during upgrades,
and boot reconciliation initialize it without replacing an existing validated lock.
The `network-policy` init stage runs after host networking and before instance creation;
ordinary starts, init finalization, the test VM backend, boot restoration and teardown
use the same policy service. Existing NetworkManager and host-route guards still apply.

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

### Coding-integration cleanup hooks

The shipped coding-integration profile metadata may declare
`AGENT_<name>_CLEANUP` alongside its provisioning, readiness and persistence fields. The value is
a trusted regular, non-symbolic-link source file with the same allowed scopes as
`AGENT_<name>_PROVISION`. Cleanup metadata declares a known integration even when that integration
is absent from `CODING_TOOL_INTEGRATIONS`; cleanup never implies selection and does not rewrite the
requested set. This contract is separate from the environment-profile resource registry below.

The generic `yard integration cleanup <name>` workflow requires a running yard and a declared hook.
The owner sends the hook through protected input and executes it in the yard as
`sh -eu -s -- observe|apply DEV_USER DEV_UID EXPECTED_FINGERPRINT`. Observe is read-only and returns
exactly `{fingerprint, changed, steps}` as JSON: the fingerprint is a lowercase SHA-256, `changed`
is boolean and every step is bounded public-safe text. A nonzero meaningful failure may return
exactly `{code, message}`, with a safe-name code and bounded public-safe message. Hook diagnostics
must never contain secrets or raw configuration contents.

Core owns the typed action assessment, confirmation and stale-plan checks. Apply receives the
approved observation fingerprint and must recheck it before changing state; it may return the same observation shape.
Core then observes again and accepts success only when the hook reports convergence. This protocol
lets each integration define its own bounded, reversible cleanup while the command, safety gates and
operator interaction remain generic.

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
RPC mutation. Read, session and protected actions keep their dedicated contracts. Public fields are
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
links, host instruction files, arbitrary device/capability declarations, SSH agent forwarding and
nested E2E access. These constraints are validated after configuration precedence is resolved.
Before startup and during convergence, effective Incus devices are checked too: inherited host
mappings and local root-disk overrides cannot bypass the role or its root disk bound.

The generic VM settings are:

| Setting | Contract |
| --- | --- |
| `VM_FREE_PAGE_REPORTING=1` | Set the existing virtio balloon's fixed reporting property and guest reporting order 1; reject unsupported Incus/QEMU or conflicting raw configuration. No arbitrary QEMU input is accepted. |
| `VM_PIN_IPV4=1` | Pin the private primary NIC to its observed DHCP address after ownership and collision checks. |
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

When completed release history is blocked by unavailable runtime observation, the local owner can
still assess and confirm the exact declared public-ingress shutdown. Its private admission permit
binds the installed source, completed journal, ledger, links, configuration and native cleanup plan;
these are rechecked under the release lock after consent. Bring-up, unfinished history, unrelated
blockers and changed bindings remain gated. This authorizes only resource cleanup and preserves
unverified guest cleanup; it never resumes or applies release activation.

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
`dev/e2e/p0-acceptance.sh --slot N --lane full`. Choose checks using
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
  Scoped disable invokes only that profile's `--remove`, carrying metadata bindings for its declared
  managed paths so replacement artifacts fail stale guards before deletion.
- `managed_paths` declares owned `data`/`operator` paths for teardown assessment, with optional
  `{yard}` substitution; the hook still owns physical cleanup and ownership checks. Teardown and
  `init --reset` carry approved metadata bindings in `SUBYARD_TEARDOWN_ARTIFACTS`. Removal hooks
  use `scripts/lib/teardown-plan.py guard-artifact PATH` before service mutations and
  `remove-artifact PATH` at deletion. Bindings cover device/inode, UID/GID, mode, size, mtime,
  ctime and symlink targets without reading file contents. The bounded guard permits an approved
  artifact to disappear, rejects additions or replacement, and returns exit 75 for `plan_stale`.

Descriptors contain no secrets. Config and credential data stay outside immutable release roots.
Profile changes must preserve existing persisted paths and update/rollback behavior or declare a
migration. Core contract tests use synthetic profiles; concrete implementations, composition checks
and live acceptance belong to profile runners. Release verification aggregates their results as
specified in [testing](testing.md).

### Profile settings and installed runtime hooks

The shipped `config/profiles/*/profile.json` v1 descriptors may declare `settings`, `runtime`
and `guest_environment`. These extend the existing profile registry; operator configuration cannot
supply declarations or executable hooks. Profile setting names must not collide with core fields,
dynamic core namespaces or another profile. The loader resolves one operation-local catalog before
reading configuration layers. Validation, provenance, field discovery, authoring, sync and command
context use that catalog. Defaults retain lowest precedence. A `host_listener` port participates
in generic owner-port collision checks.

A `runtime` declaration contains an `activation_id` and relative executable `handler`. The ID is
unique across profile and core activation stages and remains durable across release transitions.
The hook accepts `observe`, or `apply OPERATION_ID ACTUAL_SHA256 DESIRED_SHA256`, and emits only a
bounded JSON object with `state`, `actual`, `desired`, optional `hook_binding` and optional
`ownership_absent`. States are `absent`, `deferred`, `current`
and `stale`; installed states carry lowercase SHA-256 fingerprints. Observe must not mutate state.
Core bounds execution and output, checks the assessment before apply, and verifies the resulting
state separately. Hook paths retain the updater's pinned directory descriptor; symbolic links
below that anchor are rejected. Missing/stopped yards are absent/deferred without starting them.
Hooks run in activation-ID order before integration project hooks; update and rollback inspect all local yards.
The Orca profile retains its existing `orca-runtime` identity and wire fingerprints.
`ownership_absent: true` is valid only with `current` and its ordinary equal SHA-256
digests. It proves no owned artifacts remain for retained-owner admission; successful
cleanup still converges to the approved desired digest. It does not relax apply verification.

An optional `runtime.custody: true` declares resource generation fencing for the private native
owner protocol. Its handler additionally accepts `custody-observe`,
`custody-apply OPERATION_ID ACTUAL_SHA256 DESIRED_SHA256 EXPECTED_GENERATION GENERATION`, and
`custody-fence EXPECTED_GENERATION GENERATION SUCCESSOR`. Custody observations contain only
`{"generation":"0"}` for an absent generation or a lowercase SHA-256 generation. Inspection
does not claim a generation. A consented apply durably claims the exact predecessor or checks its
own generation under the component's actual mutation lock, retaining that lock through verification.
A recovery fence admits only the original predecessor, original generation or its own idempotent
successor; it fsyncs and verifies that successor under the same lock before acknowledging it.
Busy locks and unrelated generation drift refuse. Every relevant native invocation must carry
this recipe. This capability does not infer closure of other transports, change ordinary Go
mutation admission, or make a fence proof of desired runtime convergence.

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
Its native custody handler retains the existing guest installation lock across generation CAS,
source/baseline validation, unit stop, asset publication, reload, enable/start and verification.
The durable generation is a protected profile-owned sibling of the installed runtime directory,
so claiming it does not itself change the approved runtime baseline. Recovery also fences a delayed
first claim from an absent generation; subsequent work uses a fresh owner-bound generation.
Ordinary apply, pause, resume and release refresh invalidate older native claims under that
same lock before their first effect. An already current no-op does not change the generation.
The deterministic native generation includes its observed predecessor, so a later fresh plan
cannot restore an older generation after recovery has fenced it.

The private native helper retains its own configuration and repository directory descriptors.
Source reads use those retained descriptors; durable recipes use stable logical release labels.
Accepted work retains the original update and account lock open file descriptions until native
drain, including after its Rust owner exits. The two fixed host installers carry the original
account descriptor through sudo as redirected stdin and validate its exact protected inode after
privilege entry, before creating files. A replaced descriptor refuses installation. Their sorted
locks derive from every actual installed file, remain outside replaced files, and cover the full
installation or removal and verification. Ordinary installer calls also invalidate older claims.

Native prepared payloads retain the approved bytes of the existing fixed installed helpers,
registered profile packages and host installer dependencies. Recipe transfer uses bounded chunks;
the original full recipe digest remains bound to the prepared target. Fresh helpers restore those
bytes from the durable bundle and revalidate original source, resource and instance guards before
execution. Configuration, data and operator roots retain their separate roles. Read-only script
probes qualify the helper's retained repository descriptor with its PID; mutation wrappers consume
a private immutable execution tree. A successful drained wrapper may remove its exact marked tree;
unproved opaque work retains its bounded stage and pending invocation rather than replaying it.

Owner-registration effect verification observes progress against the original preparation and
recovery token. Preparing the newly current registration describes another state and cannot
replace that original effect proof.

Activation targets use the owning component's projection of that original preparation, including
future yard names. This activation inventory stays separate from the original settings inventory.
Fresh-process recovery reconstructs the same projection from the durable preparation before
reobserving the actual installed state; it does not prepare a new target after the rename.

Terminal owner-registration archive cleanup is a separate closed component operation. It consumes
the original preparation and recovery token, reobserves the owning desired state, and deletes only
the component's bound local archives. Fixed read-only inventory queries submit no mutations. A
returned local failure proves quiescence while preserving the cleanup obligation; Rust owns its
pending outcome and retains the original recovery proof. Recovery of an interrupted local cleanup
requires newly held original account/configuration/update custody and exact cleanup facts. This
contract cannot close a different operation's remote or opaque work.

Native effects preserve the existing supported 75-minute component allowance, with a separate
30-second drain and durable publication reserve. Component-specific shorter deadlines still apply.
The owner permits a 76-minute mutation exchange and 75-minute 40-second armed shutdown allowance;
read-only queries retain their shorter bound. Expiration, cancellation and local process death
never constitute a native terminal witness.

Core also registers `core-guest-runtime` and `key-tools-runtime` for already installed helpers.
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

Optional `runtime.projects_changed_hooks` declares installed hooks owned by that runtime.
Entries must be unique, clean absolute paths directly inside
`/usr/local/libexec/subyard/projects-changed.d`, without carriage returns, line feeds or NUL. Core captures the
bounded installed hook inventory, source fingerprints and project-root scope before consent,
then rejects expansion or changed captured inputs before invoking a hook.
Core captures canonical workspace roots and their direct metadata without traversing source
trees. Native hooks own discovery and bind their known nested-root inputs through `hook_binding`.
An observation's optional `hook_binding` is a lowercase SHA-256 digest of safe native
registration metadata and known-root filesystem identities.
Core retains it privately and supplies `SUBYARD_PROJECT_HOOK_BINDING` to the corresponding
declared hook for its native compare-and-swap check before effects. It is not a public plan fact
and must not fingerprint credentials, protected contents or secret values. Genuinely unavailable
observations may resolve within the explicitly approved conditional hook and root scope; this
does not authorize additional hook paths or project roots.
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
Retired integration declarations live under `config/retired-integrations/`; they permit only
bounded cleanup lifecycle and discarded-input metadata, with no provisioning or native binaries.

Full uninstall is a scope of the existing prepared teardown. It aggregates local yard executions
through the same observation, stale checks, apply and verification methods. The existing
`teardown-plan.py` owns ancestor-pinned filesystem inventory, removal and private publication;
`internal/uninstall` carries installation ownership and the exact durable retry record. Installer entrypoints record actual
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
