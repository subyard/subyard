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
fallback, OpenSSH client default and Orca systemd environment. Both SSH initialization and
explicit unlock use it; repeated
grants do not restart Orca. See [temporary SSH access](ssh-agent.md) for the public command contract
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
five-minute plan lifetime. Integration mutations require this contract. Their controller keeps
one SSH stdio session from owner assessment through central confirmation and owner execution;
a disconnect discards the plan. `integration.status` uses the same read-only owner query.

Other command families retain their existing routing until explicitly migrated. Legacy execution
still refuses a controller-side plan routed to a remote owner with `remote_owner_required`;
controller plans cannot be transferred to another session. Command-specific stale checks and the
release-transition authorization contract remain distinct from session-level plan storage.

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
schema is unchanged. Resource preparation currently uses its dedicated non-RPC pipeline, so this
exit-status contract does not imply an RPC resource-preparation interface.

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
operator maintains a configurable pool of disposable two-VM pairs. The canonical pool, exact-slot,
lease, nested-slot, and cleanup contract lives in [Agent E2E VM pool](test-vms.md). The required
external release smoke is `dev/e2e/p0-acceptance.sh --slot N`; the periodic and risk-selected
compatibility matrix is `dev/e2e/p0-acceptance.sh --slot N --lane full`. GitHub workflows do not run
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

Run `./tests/run.sh`, the recursive ShellCheck command used by CI, and `git diff --check` before
submitting changes.
