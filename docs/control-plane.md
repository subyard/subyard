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

`AGENT_<name>_CHECK` is a package-owned installation/convergence probe; release activation
may require it to succeed. Long-running service initialization belongs outside that gate.
Optional `AGENT_<name>_HEALTH` probes report advisory service health for `yard status` and
`yard integration status`: one JSON object with only `state`, one of `ready`, `starting`,
`failed`, or `unknown`. Queries bound execution to five seconds and accept at most 1024 bytes;
an unavailable, invalid, or timed-out probe reports `unknown`. Health never enters a release
plan or its fingerprint. Installation can be converged while a service is still starting.

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
`yard list --complete-yards` and `yard list --complete-projects` providers. They preserve successful
newline-delimited records. An unavailable, failing or empty provider yields only `default` for
yards and no dynamic project candidates. Completion never reads yard registrations, project state,
HostID or configuration files. Profile discovery still reads shipped `config/profiles/*/profile.conf`
until it shares the component resolver.

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
unconfirmed operation plan and one execution closure capturing its typed state. Its idempotent
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
The outer event `sequence` and `revision` are one monotonic per-session stream; adapter-local Incus
revisions remain typed event data and cannot make the RPC revision move backwards after a snapshot.

The switched surface exposes `command.list`, `context.get`, `operation.route`, `operation.plan`,
`operation.execute`, `integration.status`, `project.list`, `owner.inventory`, `yard.status`, `credential.list`, `credential.status`,
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

`HostID` is explicit owner-host identity. `yard host rename` changes it transactionally on the owner.
`yard host add <owner-endpoint>` first lets OpenSSH resolve the endpoint (including ordinary aliases
and proxy configuration), shows the concrete SSH server-key SHA256 fingerprint, HostID and discovered
yards, then atomically stores the connection, strict host-key pin and initial inventory cache after
confirmation. Controller refreshes use only that managed key with `StrictHostKeyChecking=yes`.

When the pinned key is unchanged, an already registered controller adopts a new authoritative HostID
automatically and atomically moves its connection, cache and HostID-scoped project-routing state.
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
When switching releases after source migrations are complete, materialized-config
activation observes and reconciles all registered local yards, using the same scope
as the completed release's readiness check. Pending source migrations retain their
selected-yard scope across recovery because they can rename yard registrations.
This config reconciliation leaves stopped or absent yards untouched.

Activation observers do not print diagnostics. Return expected drift as
`Converged: false`, persistent notices as `V2ActivationObservation.Warnings`, and
failures as errors. The operation boundary renders current `Outcome.Warnings`
once, sorted and deduplicated. Public error details use `ActivationDiagnostic`;
raw guest errors stay private.
Recoverable reconcile failures retain validated public diagnostics after reobservation;
ambiguous state and protected transition guards retain their own recovery instructions.
Materialized-config observation failures identify the yard and inspection phase, with a
read-only config or integration status command. Known integration ownership conflicts retain
their validated path and inspection command across the release boundary; they must not send
`migrate --check` back to itself as the only next step.

Each compiled capability classifies its bounded resources as preserve, transform, canonicalize,
reset or block before confirmation. An authorized reset is a successful, journaled one-time result;
unknown or ambiguous state produces a structured operator-action outcome without overwriting it.

`yard migrate --check` reports readiness of the exact installed `current` release: per-domain
recorded and required epochs, applied and pending migration IDs, transaction/step checkpoints,
runtime reconciliation actions and blockers. Add `--json` for the versioned machine-readable
report. A successful inspection exits 0 even when work remains; inspect `outcome.status` for
readiness. Unavailable or unsupported inspection never means ready.

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
Only a subsequent release may start using the new protocol with owners that support it. Retain
the old reader, writer and semantics throughout the supported upgrade, retained-runtime rollback
and unfinished-transaction recovery horizon. A runtime version bump alone never changes these
contracts. Do not weaken validation or silently discard a new safety requirement to fit an old
response: the compatibility adapter must preserve its meaning.

Before publication, `dev/verify-release-upgrades.py` runs the unmodified, checksum-pinned v0.11.2
updater against the built candidate on the runner's architecture. It verifies inspection, activation,
the completed fixed point, completed activation drift inspection, rollback and forward retry. It also kills a real update after journaled
activation and uses the old updater to resume the same authorized candidate transaction. This
released-binary check complements tests of the frozen codecs; rebuilding both ends from current
source does not establish cross-release compatibility.

### Legacy upgrades

Runtimes older than v0.11.0 use a legacy updater that cannot authorize the current release
transition. Its rejected migration endpoint prints a standalone installer command pinned to the
candidate version. The old invocation leaves runtime links and protected settings unchanged.
Use that exact version in both the download URL and installer arguments. When piping the installer
to Bash, pass `--yes` explicitly after reviewing the intended upgrade:

```bash
VERSION=<candidate-version>
curl -fsSL "https://github.com/Subyard/Subyard/releases/download/v${VERSION}/subyard-install.sh" \
  | bash -s -- --version "$VERSION" --yes
```

Without explicit consent, a noninteractive pipe is rejected. Alternatively, download the same
pinned installer to a file and run it from an interactive terminal. The standalone installer
publishes and verifies the candidate before the candidate plans the transition. `--yes` does not
bypass compatibility, configuration ownership or stale-plan checks.

If both `yards/<name>/config.env` and `yards/<name>.env` exist, the transition names both paths and
stops before changing configuration. Inspect them locally to confirm that the nested registration
is the intended active configuration. Run the published candidate's `bin/yard` command using its
exact path under the configured runtime root:

```bash
"<runtime-root>/releases/<candidate-release>/bin/yard" -Y default config repair-registration <name> --check
"<runtime-root>/releases/<candidate-release>/bin/yard" -Y default config repair-registration <name>
```

`-Y default` selects the local owner context even when the ambient yard uses a retired template.
The repair shows its exact scope and asks once. It keeps the nested file unchanged and atomically
moves the flat file to `recovery/yard-registrations/<name>.env` under the configuration root, without
printing its contents or overwriting an existing archive. A changed registration invalidates the
plan; an unfinished release transition blocks repair. Rerun the pinned installer afterward to
inspect and authorize the now-unambiguous upgrade.

`dev/verify-release-upgrades.py` also runs the unchanged, checksum-pinned v0.9.1 updater to verify
the refusal instruction, noninteractive consent, duplicate registration repair and completed
standalone transition.

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

`code` uses the resolved context's dedicated `codeSshHost` alias for its controller workspace.
The alias shares the yard's identity and host-key pins, but forwards only controller loopback
`127.0.0.1:8765` to yard loopback `127.0.0.1:8765`. Its separate control-socket prefix and
`ControlPersist no` isolate preview from ordinary yard connections. SSH convergence requires
both aliases so repeated init upgrades older snippets. The controller checks port availability
before launching VS Code; `ExitOnForwardFailure yes` handles a later bind race.
It also checks the dedicated alias through OpenSSH's effective configuration. A legacy
Subyard-managed snippet containing only the normal alias is atomically extended before
launch, retaining its transport, identity and host-key pins. Unmanaged or unavailable
configuration fails before VS Code starts with initialization or registration repair guidance.
Owner-inventory project resolution retains a matching explicitly selected remote alias,
so the resolved code alias stays consistent.

Core provisioning atomically installs `subyard-preview` as root-owned mode `0755`. Running-yard
convergence checks its bytes and metadata; stopped yards use the installed source-hash marker.
The helper always serves guest loopback `127.0.0.1:8765` in the foreground. Provisioning installs
root-owned mode `0644` endpoint metadata at `/etc/subyard/preview.json`: version `1`, host and port.
For a container yard with an active owner Tailscale IPv4 address, an owner proxy listens only on
that exact address and forwards to the guest helper. Its port defaults to the SSH host port plus
30000, wrapping into `1024..65535` (`2222` gives `32222`), with `WEB_PREVIEW_HOST_PORT` as the override. VM yards and owners
without an active Tailscale address install the loopback endpoint. Repeat init after address or
proxy changes. The helper validates the bounded endpoint file before listening and prints its URL
only after binding; a missing file retains the legacy/source-checkout loopback URL. Direct Tailscale
access requires device reachability and Tailnet policy access, plus the running helper; loopback
access also needs the dedicated `code` SSH session.
Selected supported agent instruction adapters preserve host text and add a short preview block
through the existing inventory. Initial legacy adoption can accept exact original source bytes
after consent; that digest is input-only and does not change the stored ownership schema.

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
```

`ACTION` is repeatable and is the source of the public verb list. Its assessment and recovery classes
bind each operation to the shared typed confirmation policy. The engine owns confirmation input.
Non-session handlers run with standard input connected to the null device. Session actions
inherit operator input; terminal sessions take foreground control and return it on exit or
cancellation. When a terminal session ends, the engine also terminates remaining members of its
process group. Handlers can still supply their own pipes or here-documents to child processes.
Each resource process group remains cancellable as a unit. Declared read-only verbs remain
available during an unfinished release transition; verbs with any non-read action remain gated.
At least one action is required, and the `BRINGUP` and `SHUTDOWN` verbs must be declared by actions.
`HANDLER` is relative to the owning profile.
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
A profile's `profile.conf` can declare `PROVISION_SCOPE=dedicated`. Such a hook is excluded from
implicit all-profile provisioning and requires a named yard whose shipped role declares
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
| `ROOT_DISK_SIZE` | Bound the root disk at creation; refuse implicit resizing of an existing VM. |
| `SRV_VOLUME_TYPE=block` | Attach an owned custom block volume and mount its ext4 filesystem at `/srv` by UUID. |
| `SRV_VOLUME_SIZE` | Required explicit size for block storage; refuse foreign volumes or size changes. |

The block-volume adapter formats only a newly marked blank volume. It never hides nonempty `/srv`
or overwrites an existing filesystem. The ordinary teardown state-retention policy still applies.
Reporting returns unused guest pages to the immediate owner; it does not change configured memory,
inflate the balloon, or reserve capacity for other workloads.
The owner persists guest reporting order 1 through a root-owned `/etc/tmpfiles.d` rule and applies it
after the VM agent becomes ready. The guest readiness check verifies the negotiated reporting feature,
the exact rule and its permissions, and the live kernel order; drift remains a read-only diagnostic.
Reporting and IPv4 pinning are opt-in enforcement capabilities: `0` or an unset value does not undo
an override or address pin already installed on an existing VM, nor revoke its project permission.
These inputs are not runtime on/off switches.

### Explicit owner UDP ingress

A profile can declare one exact IPv4 NAT route to a pinned VM address:

```text
PROXY="service-port RESOURCE_SERVICE_IPV4 RESOURCE_SERVICE_PORT RESOURCE_SERVICE_INTERFACE udp:guest:12345 owner-metadata-v1 owner-ipv4-udp"
```

`RESOURCE_<ID>_IPV4`, `_INTERFACE` and `_PORT` are typed yard/command settings. The IPv4 must be an
explicit owner address on the selected interface; wildcard publication is forbidden. The descriptor
declares the guest port. Its bring-up and shutdown use `public-ingress-change reversible` action
metadata. Automatic port allocation and `BOOTSTRAP` are not supported for public UDP routes.
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
owned route; it does not grant broad UDP access. Failed network reconciliation invokes the handler's
internal `rollback-ingress` under the original bring-up operation to close its route. The engine
verifies that both the proxy and ownership marker are absent before accepting a no-op or removing
the matching ACL allowance, and checks closure again after ACL cleanup.
Bring-up requires a converged network policy. Shutdown and rollback may remove only that route's
persisted approval and exact UDP allowance, including after an interrupted shutdown; they refuse
unrelated ACL, NIC or project drift instead of reconciling it as part of a resource action.
For Amnezia, that rollback also disables the guest runtime while preserving VPN state; a later
bring-up re-enables it.

Isolation persists the approved route's device, owner endpoint and guest port in the existing
network-policy binding. Boot restoration derives its ownership fingerprint from those parameters
and the binding's guest address without loading profile files. It requires an exact match with the
current local and effective proxy, ownership marker, pinned guest address and ACL. Older policies
with a stored fingerprint remain readable after validation; subsequent writes omit that redundant
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

Before changing an active route's endpoint, template or profile selection through `config set/unset`,
run its shutdown verb. The read-only shutdown assessment must report no remaining enabled runtime
or ingress. After manual profile deselection, `init` assesses the dedicated resource's declared
shutdown, closes its exact owned route, and runs that shutdown under the same operation. Unverified
guest shutdown retains the pending ownership marker and blocks further initialization so a retry
cannot silently reopen the service. Guest data is preserved. Stop the resource before editing its
files manually; invalid configuration or ambiguous ownership fails closed and requires correction
before reconciliation.

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

Optional behavior and its tests belong to `config/profiles/<name>/`. A shipped `profile.json`
(schema version `1`) extends existing profile provisioning with declarations read by
`internal/profile`; profiles without that file retain their existing provision hook behavior.
This is a local shipped-package contract, not a registry of operator-supplied executable code.
Unknown fields, unsupported versions, unsafe relative paths and duplicate consumer IDs fail closed.

- `default_yards` supplies selection when `ENVIRONMENT_PROFILES` is absent. An explicit list,
  including an empty list, wins; matching `disabled_when` conditions disable selection.
  `selected_provision_only` applies this selection to implicit provisioning.
- `native` lists Go package directories and artifact paths relative to the profile. Development
  and release builds discover these declarations; installed profiles contain the native artifacts.
- `consumers` declares credential ID, zone, relative materialization path and format (`file` or
  `rsa-private-key`). Zone `*` accepts any validated credential zone; a single `{zone}` filename
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
  Interactive init prepares these inputs before its existing single confirmation. It checks
  descriptor/config/source drift before applying; automation never answers profile prompts.
- `owner_service` names an executable Bash hook. Core invokes it only with prepared engine context,
  passing `SUBYARD_PROFILE_SELECTED=0|1`, including unselected profiles so they can clean up.
  `--check` inspects convergence; `--yes` reconciles; `--remove` removes owned service state.
  `--pause` writes exactly `paused` when it stops an active service, otherwise nothing;
  `--resume` restores that service. Core retains the paused profile IDs and restores earlier
  services if a later pause fails. Hooks own their service-specific identity and recovery guards.
  `SUBYARD_PROFILE_STOPPED=1` asks readiness checks to honor the yard's stopped intent.
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
bounded JSON object with `state`, `actual`, `desired` and optional `hook_binding`. States are `absent`, `deferred`, `current`
and `stale`; installed states carry lowercase SHA-256 fingerprints. Observe must not mutate state.
Core bounds execution and output, checks the assessment before apply, and verifies the resulting
state separately. Hook paths retain the updater's pinned directory descriptor; symbolic links
below that anchor are rejected. Missing/stopped yards are absent/deferred without starting them.
Hooks run in activation-ID order before integration project hooks; update and rollback inspect all local yards.
The Orca profile retains its existing `orca-runtime` identity and wire fingerprints.

Optional `runtime.projects_changed_hooks` declares installed hooks owned by that runtime.
Entries must be unique, clean absolute paths directly inside
`/usr/local/libexec/subyard/projects-changed.d`, without carriage returns, line feeds or NUL. Core captures the
bounded installed hook inventory, source fingerprints and project-root scope before consent,
then rejects expansion or changed captured inputs before invoking a hook. An observation's
optional `hook_binding` is a lowercase SHA-256 digest of safe native registration metadata.
Core retains it privately and supplies `SUBYARD_PROJECT_HOOK_BINDING` to the corresponding
declared hook for its native compare-and-swap check before effects. It is not a public plan fact
and must not fingerprint credentials, protected contents or secret values. Genuinely unavailable
observations may resolve within the explicitly approved conditional hook and root scope; this
does not authorize additional hook paths or project roots.

A `guest_environment` handler accepts `check|ensure DEV_USER` through the existing root guest
execution boundary. Its source is read only from the validated shipped profile. `check` reports
readiness; `ensure` performs the previously assessed repair. Profile selection does not suppress
repair of an installed service. Core owns transport, confirmation and orchestration; each profile
owns its service paths, diagnostics and restart mechanics.
