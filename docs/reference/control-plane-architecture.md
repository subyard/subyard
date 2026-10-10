---
title: Architecture of the CLI, control plane and Subyard Veranda
status: architecture and design constraints
updated: 2026-10-10
note: >
  Current architectural boundaries and accepted design constraints. The implementation guide
  describes shipped interfaces; Veranda requirements do not claim that future GUI features are shipped.
---

# Architecture of the CLI, control plane and Subyard Veranda

This reference explains current system boundaries and the decisions that constrain further work.
[Control-plane implementation](../control-plane.md) describes the current components and implemented
interfaces. The production CLI/control plane is a Go engine with narrow Bash physical adapters.
Veranda's native client provides local/SSH RPC, trust storage and session contracts. Slint is the
Linux migration finalist after a matched native comparison; the GUI and cross-platform delivery remain
accepted design constraints, not claims of a shipped desktop application.

The runtime layers `L0 host → L1 yard → optional L2 project-env → nested test runtime` and profiles as
an orthogonal axis remain unchanged. A profile describes toolchains, caches, environment and devices
across L1 and/or L2; it is not another runtime layer. See [Workflows](../workflows.md) and
[Profile extensions](../control-plane.md#profile-extensions). This document describes the control plane.

## 1. Architectural decisions

- The product is **Subyard**, and its CLI is **`yard`**. The graphical application's full name is
  **Subyard Veranda**, shortened to **Veranda**. Its positioning is:
  **Veranda - Nice view into every yard.**
- Go owns the Linux owner-side engine, CLI/control plane and official Incus client. Bash owns
  bounded platform integration and safety guards for host/Incus/network/storage/systemd and profile
  hooks. Rust supplies Veranda's native client and presentation boundary. Slint is the Linux UI
  migration finalist; Tauri/WebKitGTK is excluded because of its unacceptable idle memory footprint.
- Veranda uses versioned Yard RPC. It duplicates neither owner domain logic nor Incus access.
- Each operation has exactly one production implementation path. A compatibility shim may redirect
  a call but owns no business logic.
- The accepted delivery contract places Yard and Veranda in one public monorepo under one product
  tag `vMAJOR.MINOR.PATCH`, with separately installable artifacts. The core installer and `yard update`
  remain CLI/engine-only; Veranda is delivered separately.
- The accepted compatibility policy supports an exact Yard/Veranda pair with the same product SemVer.
  Yard RPC versions and capabilities form an independent wire contract for typed negotiation,
  feature gating and actionable incompatibility diagnostics, without promising an arbitrary
  cross-version compatibility matrix.

The shared `Orchestrator` owns policy/confirmation, and core CLI/RPC commands share preparation in
[`internal/cli/prepared_command.go`](../../internal/cli/prepared_command.go). Exact owner plans bind
operation payloads through digest/expiry, single-use execution and native operation steps.
Architectural simplification should consolidate places that independently describe one operation
while preserving identity, confirmation, atomic state and recovery. A language change, daemon or
general workflow framework does not by itself remove that duplication.

## 2. System topology

The owner engine and CLI use the topology below. Veranda's local fleet is implemented; its remote
and Windows/macOS paths are accepted design constraints.

```text
Linux
  yard CLI ──────────────────────────────────┐
  Veranda ── local stdio ─────────────────────┤
  Veranda ── SSH stdio (remote mode) ─────────┤
                                              ├─ Go engine on Linux owner-host
Windows/macOS                                 │    ├─ Incus Go client → Incus Unix socket
  Veranda ── SSH stdio ──────────────────────┘    └─ structured runner → Bash adapters
```

- The Linux CLI can use the local engine or select a remote owner-host. Veranda must use the same
  owner-side boundary for its planned remote mode.
- Planned Windows/macOS Veranda clients are remote-only by default: first run offers SSH to a Linux
  owner-host. They require no local Incus, Go engine or privileged helper.
- Local and remote use the same RPC contract; only the transport differs.
- The engine has neither a public TCP API nor a mandatory persistent daemon.

### Domain identity topology

Canonical identity contract (accepted on 2026-07-23):

```text
OwnerHost (L0)
  ├─ ordinary Yard (L1: default/named; owner reached locally or remotely)
  │    └─ Project
  │         └─ optional ProjectEnv (L2)
  └─ profile-owned test Yard (special role, no coding agents)
```

- A yard's identity is the host-scoped pair `(owner-host, yard name)`; the same name on two
  owner-hosts does not identify one yard.
- `default` is an ordinary canonical yard name with an ordinary `yards/default/` registration.
  Yard-scoped commands use it when no selector is supplied; host-wide inventory commands retain
  their documented scope. This is not a separate yard identity or switchable alias.
- The operator sets `OwnerHost.HostID`. The first `yard setup` offers the current hostname as the
  default; Enter accepts it. After setup, this is a stored identity, not a live reference to the OS
  hostname. A later HostID rename is a separate confirmed migration of controller references, not a
  rename of yards.
- `local|remote` and SSH/stdio describe controller-side connection/access, not yard type or identity.
- `YardRuntime`, `YardKind (container|vm)`, `YardInstance` and `YardImage` describe yard runtime but
  do not replace the yard itself.
- `EnvironmentProfile` is an orthogonal contract for dependencies, settings and resources selected
  for a yard; a profile is not a yard, project, project environment or coding agent.
- `yard provision <profile>` enables that profile for the selected yard, reconciles prerequisites
  and installs its toolchain. A dedicated test yard is configured separately; provision does not
  create a companion yard or own the outer test pool's privileged lifecycle.
- A profile-owned test yard has an explicit role/capability `allows_coding_tools=false`, which is not
  inferred from its name. Coding agents do not run there; profile-owned brokers/VMs/resources are
  not agents.
- Controller-side host connections, endpoints, pinned host keys and trust state are separate entities.
  Registering or removing a connection does not create, rename or delete authoritative yards or
  projects on the owner-host.

| Entity | Identity | Owner / source of truth | Runtime representation |
|---|---|---|---|
| `OwnerHost` | operator-owned `HostID`, initial default = hostname | L0 host itself | Linux host + owner-side engine |
| Ordinary `Yard` | `(OwnerHost, yard name)`, including `default` | owner-host yard registry/state | separate L1 Incus instance + `/srv` |
| Profile-owned test yard | owner-host + explicit special role/context | owner-host profile/test registration | privileged companion L1; no coding agents |
| `Project` | `(Yard, project ID)` | selected owner-host/yard registry | workspace and project metadata |
| `ProjectEnv` | `(Project, environment instance)` | selected yard's lifecycle | optional L2 container |
| `EnvironmentProfile` | versioned profile ID | package/registry snapshot | desired dependencies, settings and resources for the selected yard |
| Host connection/trust | `(HostID, connection record)` | Veranda app-local store for GUI; controller config for CLI | pinned SSH trust + transport session |
| Yard runtime/image facts | yard + observed generation | owner-host/Incus | kind, instance, desired/observed image |

## 3. Technology ownership

| Layer | Technology | Owns | Does not own |
|---|---|---|---|
| CLI/control plane | Go | command registry, config/context, state/transactions, cross-yard routing, operation identity/audit, Incus operations/events, credential metadata/revision/trust/assignment/sync decisions, RPC | host-specific implementation details, profile process identity and secret payload |
| System/safety adapters | Bash | fresh-host bootstrap, bounded host `check/plan/apply/verify` mechanics, Incus/storage/network/UFW, mounts/idmap, systemd, destructive guards/rollback, pinned credential tools, protected crypto/materialization, profile/consumer hooks | command registry, cross-yard/credential business policy, RPC |
| Veranda native client | Rust | app lifecycle, capabilities, local/SSH transport, app-local connection/trust store and consent, credential references, typed presentation boundary | Incus, owner state/plans/mutations, ledger payload |
| Veranda native UI | Slint, Linux migration finalist | presentation, navigation, client state machine, progress/diagnostics | domain validation and authoritative operation result |
| In-yard desired state | Go-owned reconcile + Bash/component hooks | packages/users/files/services in an already created L1 | host/Incus/control plane |

In-yard provisioning uses Go-owned reconciliation with Bash/component hooks, rather than an
Ansible backend; see [decision 47](decisions-and-glossary.md#decision-47).

## 4. Control-plane composition

### Current entrypoint and retained Bash boundary

`bin/yard` launches the Go engine from `.build/yard` or a verified release runtime. Go owns
command/context/state, routing and stage order. `scripts/NN-*.sh`, install/lifecycle leaves and
profile hooks remain physical adapters.

The exact current map and stable interfaces are in [Control-plane implementation](../control-plane.md).
Keeping host guards in shell does not grant shell authority to select workflows, request consent
again or calculate desired selection.

### Internal Go engine boundaries

```text
presentation
  ├─ CLI
  └─ RPC stdio
        ↓
application
  ├─ use cases / orchestration
  ├─ domain routing + operation correlation
  └─ operation lifecycle
        ↓
domain
  ├─ yard/project identity and resolution
  ├─ state transitions and invariants
  └─ typed errors/events
        ↓
consumer-owned ports
  ├─ Incus
  ├─ ProjectStore / status facts
  ├─ CredentialMetadataReader / CredentialStatusReader / CredentialCrypto
  ├─ Clock / IDSource
  ├─ Prompter / AuditSink / EventSink
  ├─ AdapterRunner
  └─ RemoteTransport
        ↓
production adapters                 test adapters
  ├─ official Incus Go client         ├─ fake Incus REST/WebSocket
  ├─ OS filesystem/clock/exec         ├─ sandbox/in-memory state
  ├─ SSH                              ├─ manual clock/IDs/backoff
  └─ structured Bash runner           └─ scripted exec/SSH/prompt/audit
```

Dependencies should point towards consumer-owned ports. Domain/application policy must not depend
on the Incus client, `os/exec`, SSH or the host filesystem. Production packages must not import
`internal/testkit`. These are architectural constraints; the current package map is maintained in
[Control-plane implementation](../control-plane.md#implementation-map).

## 5. Required contracts

### Commands and context

- The command manifest contains the canonical name, aliases, handler, read-only/mutating status,
  execution plane, help, verbs/options and completion provider. It is the sole source of truth for
  dispatch, `--list`, help and completions.
- Configuration is loaded explicitly once with compatible precedence; the result is a normalized,
  immutable context. Pure libraries neither read nor mutate ambient globals.

### State and reconciliation

- The project store, cross-yard resolver, yard metadata and remote cache are separate. Schema/version
  and migration policy are explicit; writes are atomic.
- The Go-owned reconciler orders stages, builds a plan and invokes `check → apply → verify` through
  the stage runner. The top-level prompt belongs to the Go orchestrator. A stage adapter receives
  validated context and performs bounded physical checks/mutations; a Bash leaf has no second planner.
- The operation ID connects plan, confirmation, apply, verify, progress, cancellation and audit.

### Credential control plane

- One host-scoped ledger serves yard contexts as consumer/assignment targets. It is not part of
  project/yard state, is not mounted wholesale into L1 and is not included in a generic RPC snapshot.
- The domain owns the immutable revision DAG, signature/trust decisions, merge/conflict,
  revoke/tombstone, assignment epoch and sync scheduling. SOPS/age/Git/SSH, identity permissions,
  systemd timers and atomic consumer materialization are accessible only through narrow ports/adapters.
- The engine and clients see credential IDs and redacted metadata/head/assignment/sync status, never
  values. Secret-bearing add/import/rotate accept payloads only locally on the owner-host through
  protected stdin/FD/file handles; initial RPC neither carries secret payloads nor exposes remote
  ingestion APIs.
- The local-only ledger is physically separate and never exported. Exclusive handoff and staging
  start retain fail-closed authority/freshness checks; `remote add` or project sync do not implicitly
  establish peer trust.

### Adapter and safety boundary

- New Go-owned operations pass shell adapters only schema-validated structured input with an
  allowlist. Existing host reconcile/profile adapters receive one Go-validated context snapshot
  and operation ID; their typed stage/resource registry remains the sole production path. Machine
  results, human diagnostics/stderr and secrets use separate channels.
- Structured metadata contains only opaque credential references. Secret payloads are not serialized
  into JSON envelopes/RPC/events/audit and reach adapters only through a separate protected input channel.
- Domain commands do not construct `incus`/`ssh` quoting or know absolute platform paths.
- The Incus adapter returns authoritative effective configuration from `ExpandedConfig` and raw
  local configuration separately from `Config`; application decisions do not manually reproduce
  profile/local precedence. Incus normalizes an empty local value into an absent raw key with profile
  fallback. Config writers merge only owned metadata and preserve unrelated local keys.
- Before host mutation, Go-owned typed assessment and the central impact resolver choose `never`,
  `Proceed? [Y/n]` for reversible/reproducible changes, or `Proceed? [y/N]` for irreversible loss that
  is difficult to recover. The Bash safety adapter has no second prompt and rechecks destructive
  ownership guards fail closed. Before the top-level prompt, sudo, target mutation, lease acquisition
  and secret transfer are forbidden. `--yes` supplies automation/orchestrator consent only and
  bypasses neither guards nor verification.
- Secrets do not travel through argv, logs, diffs or unbounded context; temporary files have minimal
  permissions and cleanup.

### RPC and events

- `yard rpc --stdio` uses framed JSON, handshake/version negotiation, capability discovery, typed
  requests/responses/errors, operation IDs and deadlines/cancellation.
- Human CLI stdout is not an API. RPC frames, diagnostics/stderr and audit are separate channels.
- The server returns a complete `system.snapshot`/`system.resync` with a revision, then continues one
  session-wide ordered event stream. The client tracks sequences/revisions: a gap, restart or
  reconnect makes its state `stale` and requires a new complete `system.resync`.
- Backpressure is bounded; a slow client blocks neither the Incus listener nor operation completion.
- Local processes and SSH run the same command: `yard rpc --stdio`.

## 6. Testability inside a yard

An ordinary unprivileged L1 yard can run the host-free checks without the host Incus socket, root,
host systemd/UFW/NetworkManager or an external SSH owner. Port boundaries keep engine failure paths
testable there; physical acceptance remains separate. The [testing guide](../testing.md) defines the
current check selection and execution policy.

### Test ports, not a test mode

- Introduce interfaces only at side-effect boundaries. Do not wrap pure logic in interfaces solely
  for mockability.
- Production/test adapters implement the same consumer-owned ports. Domain logic has no `if test`
  branches or general `YARD_TEST_MODE` that disables guards or changes control flow.
- Product-owned paths arrive through validated `RuntimePaths`; tests isolate their filesystem roots
  in temporary directories. Fixture conventions follow the
  [testing guide](../testing.md#keep-tests-proportional), including `testkit.TempDir` for private roots
  and `testkit.WriteFile` for exact file modes.
- Clock, IDs, randomness and retry/backoff are controllable so timeout/cancellation/crash/reconnect
  can be checked deterministically without `sleep`.
- Substitute platform facts and profile process identity at their owner's boundary: KVM uses a
  fact seam, and process probe/kill uses the shared user-scoped argv-anchored predicate. Arbitrary
  host devices/processes are not test inputs.
- A fake models the observable contract rather than copying business logic. Run a shared conformance
  suite against fake and production adapters wherever production is safe in temp/loopback.

### Yard-safe test layers

1. Pure unit/property/fuzz: parsing, validation, planning, state transitions, routing, retries,
   redaction and protocol codecs.
2. Port conformance: in-memory/scripted adapters and safe production adapters.
3. Process integration: a real built `yard` with temporary XDG/HOME/runtime directories;
   stdout/stderr, exit codes, signals, cancellation, atomic writes and path bounds.
4. Fake Incus integration: a local Unix/HTTP server implementing the required subset of REST and
   WebSocket events; the real official Go client checks API extensions, async operations, errors,
   disconnects, event gaps and backpressure.
5. RPC/transport integration: local stdio and scripted SSH; handshake, framing, incompatible versions,
   disconnect/reconnect/resync and multiple clients.
6. Shell adapter contract: the same `check/plan/apply/verify` flow through temporary directories and
   fake binaries in `PATH`, without disabling safety logic.
7. Credential conformance: fake metadata readers/crypto and concrete credential-runtime adapters on
   temporary roots check revision DAGs, safe merge, tamper quarantine, the local-only boundary,
   assignment freshness and redaction; production tests use only synthetic payloads.

### Minimal real-host lane

Only a real owner-host can prove:

- Incus containers/VMs, storage, idmap/mounts and kernel/network namespaces;
- NetworkManager/UFW/systemd integration, reboot/resume and preservation of connectivity by host guards;
- pinned SOPS/age installation, host identity permissions, systemd credential timers and real SSH peers;
- the real SSH host-key/agent path between machines for remote yards;
- installed-binary packaging/upgrade/rollback and compatibility with supported Incus versions.

Physical acceptance should reuse the yard-safe conformance expectations rather than reimplementing
orchestration. Use the [VM guide](../test-vms.md) and [real-host acceptance guide](../real-host-acceptance.md)
for current ownership, execution and release requirements.

## 7. Veranda architecture

The current candidate provides local and pinned SSH fleet views, app-local connection trust,
typed owner operations and native session launches. The following constraints govern the
implementation; platform, reliability and resource acceptance remain pending.
See [Veranda](../veranda/README.md) for the implemented scope and verification limits.

```text
public Subyard monorepo
  ├─ cmd/yard + internal/             Go engine/CLI and RPC contracts
  └─ veranda/
       ├─ planned Slint UI (implementation and qualification pending)
       └─ typed presentation calls → native Rust client
            ├─ local child: yard rpc --stdio
            └─ SSH session: yard rpc --stdio on owner-host
```

- The UI has no arbitrary shell/file access; the native client exposes only typed, bounded actions.
- The Rust shell contains no owner domain rules and does not parse human CLI output.
- On every OS, connection/trust metadata lives in one Veranda app-local store in a private app-data
  directory. The native shell owns versioned records keyed by authoritative HostID, destinations,
  credential references and pinned public host keys; CLI controller registrations have separate
  authority. The GUI neither writes the CLI store nor registers desktop connections on remote owners.
- Private keys/passphrases/tokens are not copied into app state/UI/logs. Authentication uses the
  system SSH agent or key/keychain references. The store has bounded typed IPC, private
  permissions/ACLs, atomic updates and a captured-baseline stale guard. Native read-only assessment,
  consent, commit and verify follow the common confirmation policy; first-use host keys are explicitly
  accepted before a trusted RPC session, and replacement requires separate repair consent. Owner
  exact plans serve domain mutations.
- The UI explicitly distinguishes `connecting`, `connected`, `reconnecting`, `stale`, `incompatible`
  and `disconnected`. Losing a connection is not an operation's final result.
- Owner-mutating UI shows the server-side plan and confirms the exact operation payload; authoritative
  validation and results remain with the engine.
- Event bursts are coalesced/batched, I/O does not block the UI thread, and subscriptions close with
  their view/session.
- The first `Profiles` version uses current shipped profiles and Subyard owner-side
  selection/provision semantics; a future declarative component registry is not a dependency.
  The optional `profile.json` contract has `schema_version: 1`; existing profiles without a descriptor
  and resource-only profiles also appear in the narrow typed RPC projection. Veranda does not read
  package paths, run hooks or create its own registry/allowlist.
- A checkbox changes only selection through an owner exact plan. Deselecting does not uninstall;
  provisioning/reconcile remains a separate explicit owner-side operation under the current contract.
  `Applied` comes only from an actual owner check, not selection. A missing or inapplicable check
  gives `Unavailable`/`Not applicable`; a stopped yard is not started just to read its state.
- Incompatible product versions, RPC/profile schemas or required capabilities cause `incompatible`
  before the corresponding mutations. The UI reports Veranda/Subyard versions and recommends
  installing both products from one release. There is no automatic conversion or fallback through
  human CLI output.
- The Go engine and Rust client do not import each other's implementation types. The
  [versioned RPC contract](../control-plane.md#rpc) defines the engine/client boundary. Contract
  changes require conformance coverage for the engine and client, with separate typed presentation
  coverage at the native UI boundary. No shared runtime/library is introduced between the
  engine and Veranda.

## 8. Performance, reliability and delivery

<a id="veranda-resource-budgets"></a>

### 8.1. Veranda resource requirements (2026-10-05)

The requirements and methodology were accepted on 2026-10-05. Minimizing resource use takes priority.
Targets guide optimization; limits are release acceptance requirements. These are chosen requirements,
not results of an already completed benchmark. Spare budget does not justify extra processes,
polling, caches or dependencies.
Tauri/WebKitGTK was rejected on 2026-10-09 after historical idle measurements of roughly
500–600 MiB RSS. Its temporary 640 MiB software-rendering exception is revoked. Native UI
candidates must meet the limits below; [historical measurements](../veranda/webview-memory-reference.md)
remain available for comparison, without further WebView acceptance or optimization work.

The 2026-10-10 [matched native comparison](../veranda/native-framework-comparison.md) recorded
62.48 MiB desktop RSS for the patched Slint prototype, 76.72 MiB for GTK4 and 86.93 MiB for
Qt Widgets. Its 10-yard/100-project workload selects a migration finalist; it does not replace
the full application workloads and release limits below.

| Metric | Target | Limit |
| --- | --- | --- |
| Cold process start to first controllable fleet/error screen, p95 | ≤ 1 s | ≤ 2 s |
| Idle RAM, summed RSS of app-owned processes | ≤ 128 MiB | ≤ 256 MiB |
| Peak RAM during refresh/switch/event burst | ≤ 256 MiB | ≤ 384 MiB |
| Idle CPU, summed across processes, average over 120 s | ≤ 0.1% of one logical CPU | ≤ 0.5% of one logical CPU |
| Compressed GUI artifact / installed app payload | ≤ 20 / 40 MiB | ≤ 40 / 80 MiB |
| Switch of an already loaded host/yard to paint, p95 | ≤ 50 ms | ≤ 100 ms |
| Local snapshot request → paint, p95 | ≤ 500 ms | ≤ 1 s |
| Received RPC event → paint, p95 | ≤ 50 ms | ≤ 150 ms |
| Remote reconnect after network restoration to fresh fleet, p95 | ≤ 2 s | ≤ 5 s |
| Retained RSS growth after 20 measured connect/disconnect cycles | ≤ 5 MiB | ≤ 10 MiB |

Methodology for the first Linux release check:

- Baseline: Debian 13 amd64, 4 logical CPUs, 8 GiB RAM, SSD, graphical Wayland session at
  1920×1080 and 60 Hz. Record the actual CPU/GPU, kernel, compositor, UI toolkit and rendering backend
  for comparability; debug/dev servers are excluded. Windows/macOS repeat the requirements in their
  release lanes with recorded OS/toolkit versions.
- Ordinary workload: one owner, 20 yards and 200 project names. Stress: 5 owners, 100 yards,
  1000 project names and a burst of 100 events/s for 10 s. External terminals/editors, Incus and
  guest workloads are outside the app resource budget.
- RSS and CPU include the Rust shell, all app-owned helper/SSH processes and the local
  `yard rpc` child. Sum RSS explicitly without subtracting shared pages; Linux PSS may be reported
  separately. Measure idle after 30 s of quiescence with the window open and no input/events/network
  traffic; the app must not repaint continuously. Measure peaks under ordinary and stress workloads.
- Cold process start means a new app session with no surviving children, with a warm OS file cache;
  record p95 over 30 launches. Measure the first launch with a cold OS cache separately. An available,
  prepared local owner must provide a useful fleet within the same startup limit; an error screen
  does not substitute for a successful fleet on a healthy owner.
- UI/event timings start on the client. Local snapshot timing includes the engine round trip; report
  remote snapshot total time and RTT separately, while processing an already received response
  obeys the UI latency budget. Reconnect includes backoff, SSH, negotiation and snapshot with
  RTT ≤ 50 ms, a pinned host key and an available SSH agent; manual credential entry is outside the gate.
- As clarified on 2026-10-07, routine cycle checks use 20 measured connect/disconnect cycles after
  warmup. Compare median idle RSS of the first and last 10 cycles with the same workload and
  quiescence. This shorter check has less sensitivity to slow accumulation than 100 cycles;
  record the actual cycle count and do not reinterpret earlier results. Extend to 100 only for
  observed growth, noisy or ambiguous retention, or a specific recovery regression requiring
  longer exposure. Orphan processes, open sessions/subscriptions and unbounded queues are
  forbidden regardless of RSS. App-owned engine/SSH children terminate when the app exits.
- Package budgets include assets and bundled helpers; the CLI/engine ships separately. System
  WebViews and other OS prerequisites are outside the GUI artifact, but report their additional
  download/disk cost on a clean machine separately. Typed replay fixtures prove only client resource
  use/latency; real local/SSH sessions require separate checks.

### 8.2. Reliability and delivery

The engine delivery boundaries are current; combined Veranda release and platform packaging below
are accepted requirements for the GUI release.

- GUI development and targeted local/SSH checks can proceed independently of unrelated acceptance
  work. Affected features need their own fixes and acceptance before release. Required full, artifact
  and profile release checks remain; targeted GUI evidence does not replace them.
- The engine retains a separate delivery baseline; check Veranda against the requirements above
  before optimization and on the final release candidate.
- No permanent polling or background process without a demonstrated need.
- Veranda's accepted first release target is Linux local + remote. Planned Windows/macOS clients
  use the same remote-only RPC flow; platform packaging/signing/notarization does not change domain
  architecture.
- One `vMAJOR.MINOR.PATCH` tag publishes existing self-contained Yard runtime artifacts and separate
  Veranda platform packages. A release is ready only after every declared artifact is built and
  verified; platform signing jobs use isolated protected environments, without requiring a separate
  repository or release cadence.
- `subyard-install.sh` and `yard update` retain their CLI/engine-only contract. Veranda installs and
  updates separately; a common release provides matched versions rather than a transactional update
  across desktops and remote Linux owner-hosts.
- Engine and Veranda product SemVer match; only an exact matched pair is officially supported. RPC
  protocol versions change independently of product SemVer; capabilities extend the additive surface.
  A missing required capability or version mismatch makes the UI `incompatible` with precise
  update instructions.
- Current RPC v1 remains `min=1,max=1`; there is no requirement to implement a multi-version server
  in advance or promise N-1/N+1 compatibility. Decide whether a transitional v1+v2 release is needed
  at the first actual breaking wire change. Until then, release acceptance includes the exact pair,
  both mismatch directions and diagnostics.
- Update/rollback support explicit config/state migration. Rollback means the previous release,
  not two production implementations in one version.

Release transitions are owned by
[`internal/releasetransition`](../../internal/releasetransition), with compiled typed capabilities selected
by [`config/release-transition.json`](../../config/release-transition.json). Completed one-time migrations
are durable history and never reopen to repair later drift. Separate activation/repair reconciliation
converges runtime resources without changing migration history. Protected evidence, authorization
binding, durable checkpoints and verified forward recovery preserve the host and operator-owned state.
The [release migration contract](../control-plane.md#release-migrations) describes current supported
legacy compatibility and recovery cases; the
[resumable release transition design](resumable-release-transition-design.md) explains the decisions.

## 9. When `yardd` is needed

A persistent Yard daemon is not required by the current architecture. Introduce one only after a
separate decision establishes at least one of these scenarios:

- notifications while Veranda is closed;
- schedules;
- operations that must survive all clients disconnecting;
- a shared queue/lease for multiple clients.

Even then, `yardd` implements the same versioned Yard RPC and gives Veranda no direct Incus or host
shell access.

This gate concerns Yard engine operations. A long agent turn on provisioned `opencode serve`,
Codex app-server or another vendor server inside L1 is not a Yard operation and does not by itself
require `yardd`: the agent server/client retains session lifecycle, and Subyard supplies only server
provisioning and a connection descriptor. Client/transport disconnect must not cancel an active turn;
the agent server continues until it needs operator input, then waits without automatically granting
approval. Reconnection returns to the same session and shows accumulated results or pending requests;
explicit cancellation remains separate. This initial disconnect contract assumes that the agent
server, yard and owner-host keep running; it does not promise restart-safe execution or failover.
Agent transcripts, turns, questions and approvals remain with the agent server/client rather than
being added to Yard RPC or Veranda. For public backend and SSH access guidance, see
[Paseo](../paseo.md) and [SSH agent transport](../ssh-agent.md).

## 10. Related documents

- [Control-plane implementation](../control-plane.md) records current components and interfaces.
- [Decisions and glossary](decisions-and-glossary.md) records current accepted decisions and terminology.
- [Resumable release transition design](resumable-release-transition-design.md) describes the release
  activation and recovery design in detail.
