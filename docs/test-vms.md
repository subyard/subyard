# Agent E2E VM pool

The `test-vms` profile runs a root-owned lease broker inside a trusted outer yard. The default pool
has two slots. Each slot owns an isolated inner Incus project, network and a retained pair of VMs.
The VM disks survive release; the pair is stopped whenever it has no lease.

The outer yard remains operator-owned. Agents can acquire inner slots, but cannot start a stopped
outer yard, enter its shell, reach its Incus socket or invoke arbitrary lifecycle commands.

## Operator setup

Register a yard with the profile:

```env
YARD_TEMPLATE=test-vms
SSH_PORT=2223
```

Then initialize it interactively:

```sh
yard -Y test-yard init
yard -Y test-yard status
yard -Y test-yard test-vms status
```

A `test-vms` initialization also converges a root-owned physical-host log sink and its one-minute
timer. The outer yard receives no mount or socket that can write the host log root.

A fresh yard with the effective `test-vms` capability starts with desired power `running`. Later
`yard -Y test-yard stop` and `start` persist the normal managed power intent. Agent acquire never
changes it. Stopping the outer yard drains all active slots before shutdown.

`E2E_VM_SLOT_COUNT` defaults to `2` and may be overridden through normal yard/operator config
precedence. Each slot consumes two VMs. Increasing the count adds empty slots; the VMs are created
only on first acquire. Shrink is fail-closed while a retiring slot is held, provisioning, draining
or quarantined. Lease release and age-based GC never remove retained resources. They are removed
only by confirmed operator configuration reconcile, destructive quarantine recovery described
below or outer-yard teardown.

Nested VM disks are thin-provisioned. Capacity checks do not reserve the sum of their virtual
maximum sizes: before creating a missing VM, the broker requires 1 GiB of initial headroom per
missing VM and keeps a fixed 5 GiB filesystem reserve. CPU, RAM and disk values remain hard per-VM
limits.

The other physical defaults are:

```env
E2E_VM_IMAGE=images:debian/13/cloud
E2E_VM_CPU=auto
E2E_VM_MEMORY=4GiB
E2E_VM_DISK=20GiB
E2E_VM_BOOT_TIMEOUT=300
```

The retained guest disks are the prepared dependency layer; Subyard does not maintain a second
custom image that could silently drift from `images:debian/13/cloud`. Before every lease is exposed,
the broker verifies a versioned baseline and reconciles it with bounded APT retries/timeouts. The
baseline includes the Go bootstrap, compiler/build utilities, ShellCheck, Git, curl, jq, ripgrep,
SSH and archive tools. Its revision marker changes with the package contract. Go's exact toolchain
and modules remain selected by `go.mod`. APT archives and repository metadata survive with the
retained disk; P0 may separately reclaim its disposable Go build and module caches. Dependency
reconciliation still runs the normal `apt-get update`; automated provisioning and capacity helpers
do not purge APT data to reclaim space.

## Agent workflow

Prepare the persistent controller identity once:

```sh
dev/agent-e2e.sh --prepare
```

A standard caller reaches the outer yard through the provisioned yard-to-yard route. Any valid
Ed25519 controller key is admitted only to the versioned forced facade
(`status/acquire/renew/release`). It never receives an L1 shell, PTY, file transfer, arbitrary
forwarding or Incus access.

Inspect the redacted pool without acquiring:

```text
SLOT     STATE        YARD             PROJECT                          RUN        PURPOSE                  AGE      EXPIRES
slot-001 held         default          Subyard-2                        c291a4ef   release-migration        3m12s    in 9m48s
slot-002 available    -                -                                -          -                        -        -
```

```sh
dev/agent-e2e.sh --status
dev/agent-e2e.sh --status --json
```

The active holder is reported as `yard + project + run + purpose`. Project is the canonical Subyard
project name from managed workspace metadata, and run is a new public correlation ID per acquire.
Before metadata convergence, a safe enclosing legacy project ID is reported unchanged with
`yard=unknown`; the runner never strips a suffix or guesses a name. These fields are untrusted
display metadata: authorization and fencing still use hidden lease credentials. Status never
publishes controller fingerprints, lease IDs/capabilities, absolute checkout paths, Git credentials,
command lines, guest endpoints or the full failure reason. A
quarantined or recovering slot instead exposes bounded recovery metadata:
`last_failure_event_id`, `incident_id`, `recovery_attempt` and `next_recovery_at`. For an available
retained slot, the attribution columns are empty.

New runners discover the broker's `attribution-v2` capability through read-only status and use the
typed `acquire-v2` command. During a rolling update, a new broker continues to accept legacy
schema-1 acquire requests. A new runner falls back to an opaque `<project>+<run>` legacy label only
after the old broker explicitly rejects the unsupported command before allocation; transport
failures and unknown outcomes are never retried.

Run against both VMs of one automatically selected slot:

```sh
dev/agent-e2e.sh --purpose host-free-suite -- ./tests/run.sh
dev/agent-e2e.sh --wait 20m --purpose host-free-suite -- ./tests/run.sh
dev/e2e/p0-acceptance.sh
dev/agent-e2e.sh --purpose real-host-check --vm 1 -- ./tests/some-real-host-check.sh
```

### Test lanes and gates

`dev/e2e/p0-acceptance.sh` without arguments is the only continuous P0 release gate. Addressable
lanes are diagnostics: they shorten a rerun after a late failure but never turn a partial pass into
a fresh-install release result.

Use the advisory [change-impact testing workflow](testing.md) to select affected host-free checks
and targeted lanes for a diff. The selector only recommends checks; targeted evidence does not
replace this section's continuous no-argument P0 release gate.

| Lane | Prerequisites and timeout | Mutable scope | Classification |
| --- | --- | --- | --- |
| `./tests/run.sh` | Go toolchain; bounded by CI | temporary host-free roots and `.build/yard` | required host-free gate |
| `dev/process-coverage.sh` | Go toolchain; selected host-free process contracts | `.build/coverage` and test-owned temporary roots | diagnostic coverage gate |
| `boundary` | one broker lease; SSH connect deadlines | read-only facade, routes and negative probes | required inside continuous P0 |
| `transport` | both allocated VMs; bounded SSH disconnect probe | one marker-owned remote sleep and temporary controller log | required inside continuous P0 |
| `nested-teardown` | VM2, KVM and nested Incus; bounded install, boot and cleanup waits | marker-owned outer VM, nested yard and data-boundary fixtures | required inside continuous P0 |
| `dependencies` | retained guest baseline; 20-minute cold Go download deadline | marker-owned cold caches only | periodic targeted bootstrap diagnostic |
| `real-incus` | VM1, KVM, persistent Incus pool; 15-minute mutation deadlines | marked project, container, VM and image aliases | required through `release` in continuous P0 |
| `profile-resource` | VM1 and current candidate | temporary dependency-free resource/state | required through `release` in continuous P0 |
| `release` | both VMs, capacity preflight; bounded nested install/boot deadlines | fresh candidate yards, current and legacy convergence | targeted diagnostic; required inside continuous P0 |
| `source-upgrade` | VM1 when targeted; VM2 worker in full; two bounded reboots | marked source-install/migration fixture | targeted diagnostic; required inside continuous P0 |
| `power-systemd` | VM1 when targeted; VM2 worker in full; real Incus, Ubuntu 24.04/systemd 255; 300-second image launch and bounded TERM-to-KILL Incus commands | marker-owned parser project plus snapshotted/restored host power runtime | targeted diagnostic; required inside continuous P0 |
| `reboot-verify` | held VM1 lease; two 3-minute boot windows | guest reboot only | targeted transport/recovery diagnostic |
| `peer` | both VMs and synthetic keys | marked cross-owner RPC, project and credential fixtures | targeted diagnostic; required inside continuous P0 |
| `peer-cleanup`, `cleanup` | same retained allocation | exact marked fixtures and run worktrees | standalone idempotent cleanup/verifier |
| no argument (`full`) | all prerequisites above | union of the marked scopes | mandatory continuous release gate |

Android/GPU, real credentials and external-service profiles use separate explicitly prerequisite-
gated lanes. A generic dependency-free resource pass does not report those handlers green.

List or run one lane:

```sh
dev/e2e/p0-acceptance.sh --list-lanes
dev/e2e/p0-acceptance.sh --lane peer
SUBYARD_P0_SLOT=1 dev/e2e/p0-acceptance.sh --lane source-upgrade --resume
SUBYARD_P0_WAIT_SECONDS=1200 dev/e2e/p0-acceptance.sh --lane power-systemd
```

`SUBYARD_P0_WAIT_SECONDS` is a non-negative number of seconds passed to the atomic broker acquire;
zero keeps the fail-fast default. The `power-systemd` fixture is fully ephemeral: the lane removes
its marker-owned systemd-255 project and restores the snapshotted host unit/runtime before the phase
is checkpointed. Its checkpoint resource inventory therefore records the fixture identity, but
`--resume` never depends on retaining its mutable resources.

The continuous gate keeps the long owner/release chain on VM1. VM2 independently runs nested
teardown, the controller suite, a real-Incus platform check, source upgrade and power-systemd in
that order. The two chains are joined before peer checks and cleanup. Their four logical phase
checkpoints are committed atomically only after both chains pass. The parallel matrix has a
210-minute kernel-monotonic work deadline by default (`SUBYARD_P0_FULL_MATRIX_TIMEOUT_SECONDS`).
The controller reads `/proc/uptime`, so host wall-clock corrections cannot expire the matrix or its
shutdown grace periods early. On expiry, runner children get a bounded 30-second TERM grace and
10-second KILL grace before evidence and marker-guarded cleanup continue, leaving the remaining
broker lease time for final checks.

Each phase prints its bundle hash and duration. The runner keeps one bounded, redacted JSON evidence
record per public run and one checkpoint per slot under its private controller state. A checkpoint
contains only slot resource generation, bundle hash, passed lanes and marker-owned inventory. Resume
fails closed after slot rebuild, selection of another slot or any public worktree change. Evidence
never contains lease credentials, guest endpoints, command payloads, controller paths or ambient
environment. The latest 20 records are retained.

Request one exact broker slot when a coordinated run requires it:

```sh
dev/agent-e2e.sh --slot 1 --purpose coordinated-check --vm 1 -- ./tests/some-real-host-check.sh
SUBYARD_P0_SLOT=1 dev/e2e/p0-acceptance.sh
```

The exact selector is part of atomic lease acquisition. A busy, quarantined, unavailable or unknown
slot fails explicitly without selecting a neighbor. The runner also verifies the returned `slot_id`
before opening guest transport and releases a mismatched grant without guest access.

Open an unrestricted root guest session or run a root command:

```sh
dev/agent-e2e.sh --ssh 1
dev/agent-e2e.sh --ssh 2 -- id -u
```

The wrapper creates an ephemeral Ed25519 key per lease, starts a keeper that renews once per minute,
and releases in its exit trap. Ten minutes without a successful heartbeat expires the lease. With
no `--wait`, an exhausted pool returns a distinct busy exit; `--wait` retries with a bounded timeout.
Busy output shows bounded holder attribution and wait progress without exposing credentials. The
raw OpenSSH config and lease capability are internal temporary files and are not an agent API.

Every wrapper invocation is a new lease and may receive a different physical slot. The printed
`e2e-vm-1` and `e2e-vm-2` selectors always mean the two guests in the current lease, never global
slot names. Stateful multi-step work must stay in one script invocation or one interactive SSH
session. `--slot N` requests only the corresponding broker lease; it never enables direct VM, Incus
or raw SSH access.

Before guest access, the runner prints the exact assignment and the broker installs the same public
context at `/run/subyard-e2e-lease.json`. Normal payloads also receive
`SUBYARD_E2E_YARD`, `SUBYARD_E2E_PROJECT`, `SUBYARD_E2E_RUN_ID`,
`SUBYARD_E2E_PURPOSE`, `SUBYARD_E2E_SLOT` and `SUBYARD_E2E_VM`.

## Lifecycle and fencing

Acquire atomically reserves an `available` slot as `provisioning`, then the root broker creates or
starts its pair. Only after both guests are ready does the broker install the ephemeral guest key,
open forwarding through that slot's dedicated data account and publish `held`.

Release, heartbeat expiry, operator drain or outer stop:

1. removes the data-account forwarding key and kills that account's sessions;
2. removes the ephemeral key from both guest root accounts when their agents are reachable;
3. stops both VMs;
4. publishes `available` only after stop is verified.

If a guest is rebooting and its agent is temporarily unavailable, the already-fenced data route
still permits a verified stop. The next acquire replaces every guest lease key before it publishes
forwarding, so the previous credential cannot become reachable again.

Provisioning, fencing or stop failure makes only that slot `quarantined`, and it is never handed to
another caller. Lease identity is fenced by slot generation, lease epoch, lease ID and a server-side
capability verifier; old credentials cannot revive after release or reuse.

Quarantine is destructive because these slots are disposable. Before deletion, the broker fsyncs
an immutable incident with the available project, VM and service diagnostics to its root-only local
spool. It then verifies the managed project and every existing VM marker, refuses projects with
foreign instances, deletes both VM disks in the slot pair, provisions both guests through the
normal fresh-acquire path, verifies their stop and increments `resource_generation` before making
the slot `available`. Deleting the disks also deletes their APT caches; ordinary release and
reacquire retain both. Failure to persist the local incident or any ambiguous ownership evidence
leaves the slot quarantined without deletion.

The root reaper starts recovery immediately after the incident is durable. Failed rebuilds retry
after 1, 5 and 15 minutes, then hourly without an attempt limit. A temporary Incus, image, network
or capacity failure delays recovery; it does not turn quarantine into a permanent terminal state.
The manual command starts the same full-pair workflow immediately:

```sh
yard -Y test-yard test-vms recover --slot 2
```

## Broker diagnostics

Broker and reaper lifecycle events from every `test-vms` pool on one physical owner host are
collected in:

```text
$SUBYARD_HOME/logs/test-vms-broker.jsonl
```

Read the host-wide structured log without selecting or starting a yard:

```sh
yard test-vms logs
yard test-vms logs -n 50 --slot 2
yard test-vms logs -f
```

Events contain UTC time, source pool, event ID, slot generation and lease epoch, state transition,
recovery timing and the available lease attribution. They do not contain capabilities, controller
fingerprints, keys, guest command payloads or agent output. Full bounded evidence is stored as an
immutable JSON artifact under `$SUBYARD_HOME/logs/test-vms-broker-incidents/` and referenced by
`incident_id`.

The broker writes its local durable spool first. The physical-host sink validates and ingests
records idempotently by ID, then acknowledges them; sink downtime therefore delays only host-wide
visibility and never blocks a rebuild. Unresolved incidents are retained. Resolved artifacts are
retained for 30 days, with a 512 MiB cap across resolved artifacts. Structured events are retained
for 90 days with a 128 MiB cap; events belonging to an incident that is still retained are protected
from event rotation, so its complete generation timeline remains available with the artifact.

## Isolation boundary

Every slot has its own restricted inner project and managed network. Guest root can use public
Internet egress for package installation, but forwarding between slot networks is denied. Guests
cannot reach L1 management, L0/private/metadata networks, the broker, its state/socket or inner
Incus. A normal development yard may still run its own L1-local Incus containers; it does not
receive the `NESTED_E2E_VMS=1` devices and policy required to run the operator-owned nested VM pool.

Run the negative transport checks after changes to routing, admission or SSH policy:

```sh
dev/agent-e2e.sh --verify-boundary
```

The operator owns outer `start`, `stop` and teardown. Agents use only leases allocated by the
broker. Outer-yard teardown removes the retained VM disks and their APT caches with the containing
yard storage. An unavailable outer yard produces the stable `test environment unavailable` error
instead of attempting recovery.

A runtime release automatically installs the compatible physical-host sink before updating an
enabled producer. When the outer `test-yard` and broker service are active, it then verifies the
installed sink, broker engine and facade status without revoking held leases. A stopped, disabled
or never-initialized broker is not started as an update side effect; its next explicit `yard init`
performs the ordinary convergence. During the one-time owner migration, a running legacy fixed-VM
backend that predates the broker service is treated as its active predecessor.
