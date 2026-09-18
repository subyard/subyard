# Agent E2E VM pool

The `test-vms` profile runs a root-owned lease broker inside a trusted outer yard. Its configured
pool contains one or more slots and defaults to two. Each slot owns an isolated inner Incus project,
network and a retained pair of VMs. The VM disks survive release; the pair is stopped whenever it
has no lease. Before stopping a running retained guest, release best-effort trims free guest blocks
back to the storage pool. A trim failure does not weaken fencing or fail release.

The outer yard remains operator-owned. Agents can acquire inner slots, but cannot start a stopped
outer yard, enter its shell, reach its Incus socket or invoke arbitrary lifecycle commands.

## Running tests

Run the host-free build and test gates from the current public worktree:

```sh
make build
./tests/run.sh
```

`make build` compiles the development binary at `.build/yard`; `go.mod` selects the Go toolchain.
Run `./tests/run.sh` before finishing shell or CLI changes. CI additionally runs
`shellcheck -x -S warning` over the CLI, scripts, provision hooks, tests and Bash completion.
Linux CLI tests also require util-linux `script` to exercise resource commands with a real
controlling terminal, including terminal-input isolation and cancellation.

If there is any doubt that behavior is covered or a problem is reproduced, use an allocated
`test-vms` slot to reproduce and verify it on real GNU/Linux hosts. A green host-free test is not a
substitute for the available VM check. The operator owns outer-yard start, stop and teardown; the
root broker owns inner slot create, start and stop; agents acquire leases only.

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

The persistent controller key stays under the agent user's `~/.subyard/e2e/`; every lease uses a
separate ephemeral guest key.

A standard caller reaches the outer yard through the provisioned yard-to-yard route. Any valid
Ed25519 controller key is admitted only to the versioned forced facade
(`status/acquire-v2/renew/release`). It never receives an L1 shell, PTY, file transfer, arbitrary
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

The runner requires the broker's `attribution-v2` capability from read-only status before it sends
the exact `acquire-v2` request. Legacy acquire is not supported, and the runner never downgrades in
response to status or acquire failure. Only a typed busy response plus bounded `--wait` permits
another request for the same slot; a transport failure or any other unknown outcome ends the
attempt. A `held` busy response
contains exactly the safe owner fields `display_label`, `yard`, `project`, `run`, `purpose`,
`acquired_at` and `expires_at`. Other unavailable states carry no owner object. The runner validates
the complete response before retrying; a missing, extra or malformed owner field makes the outcome
unknown and prevents another acquire.

Use redacted status to inspect the configured pool, explicitly choose an available slot number, then
run against the two VMs in only that slot. For example, after choosing slot 1:

```sh
slot=1
dev/agent-e2e.sh --slot "$slot" --purpose host-free-suite -- ./tests/run.sh
dev/agent-e2e.sh --slot "$slot" --wait 20m --purpose host-free-suite -- ./tests/run.sh
dev/e2e/p0-acceptance.sh --slot "$slot"
dev/agent-e2e.sh --slot "$slot" --purpose real-host-check --vm 1 -- \
  ./tests/some-real-host-check.sh
```

The runner filters private and ignored files, verifies the worktree bundle and removes its guest
worktree. Every lease-taking invocation prints `yard + project + run + purpose` for attribution.

The default pool has slots 1 and 2, but callers must use the configured capacity reported by status.
The live lease acceptance always exercises one exact slot and, when a second exists, adds concurrent
cross-slot isolation. Additional slots remain unselected and must retain the same state, lease epoch
and resource generation throughout the run.

### Test lanes and gates

The same-host network policy acceptance creates synthetic local yards and verifies explicit
links, isolation toggles, spoofing protection and managed lifecycle behavior. Choose an available
slot from fresh status, then run:

```sh
dev/e2e/yard-network-policy.sh --slot N
dev/e2e/yard-network-policy.sh --slot N --vm 2  # select the second guest when needed
```

This controller owns one lease across setup, a selected-guest reboot and resumed validation;
`--vm` accepts `1` or `2` and defaults to `1`. It changes
network policy only inside the disposable VM and does not enable isolation on the operator's host.
A network implementation change still requires a fresh full P0 below.

`dev/e2e/p0-acceptance.sh --slot N` is the continuous P0 release gate. Addressable lanes require the
same explicit selector and are diagnostics: they shorten a rerun after a late failure but never turn
a partial pass into a fresh-install release result. `--list-lanes` does not acquire and needs no slot.

Before current `yard init`, VM1 seeds the legacy convergence fixture with:

```sh
SUBYARD_E2E_LEGACY_FIXTURE=1 \
  dev/e2e/seed-test-vms-legacy-state.sh subyard-test-yard yard-test-yard
```

This fixture is restricted to disposable VM1 candidate yards.

Use the advisory [change-impact testing workflow](testing.md) to select affected host-free checks
and targeted lanes for a diff. The selector only recommends checks; targeted evidence does not
replace this section's continuous full P0 release gate.

The focused AppArmor regression creates a temporary container yard on VM1, exercises failed
capability probes and both real Incus AppArmor transitions, and verifies Docker and runtime
preservation. It restores its marked systemd override and tears down its yard. Run it on a free
slot whose inner Incus starts with AppArmor enabled; it is targeted evidence, not a full P0:

```sh
dev/agent-e2e.sh --slot "$slot" --purpose apparmor-probe --vm 1 -- \
  bash dev/e2e/incus-apparmor-probe.sh
```

The Incus group regression uses a temporary operator to exercise the first named `init` before
`incus-admin` membership is active. It verifies the real `sg` restart, explicit command overrides,
independent yard SSH ports and an idempotent retry, then removes its marked operator and yard:

```sh
dev/agent-e2e.sh --slot "$slot" --purpose incus-group-reexec --vm 1 -- \
  bash dev/e2e/incus-group-reexec.sh
```

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
| `power-systemd` | VM1 when targeted; VM2 worker in full; real Incus, Ubuntu 24.04/systemd 255; 900-second image-cache fill, 600-second local launch, 300-second restart and bounded TERM-to-KILL Incus commands | test-owned image alias, marker-owned parser project plus snapshotted/restored host power runtime | targeted diagnostic; required inside continuous P0 |
| `reboot-verify` | VM1, real Incus and cached image preparation; published v0.8.0/candidate fixture; two boot checks with bounded power reconciliation | marked upgrade fixture, two guest reboots, snapshotted/restored host power runtime | targeted transport/recovery diagnostic |
| `peer` | both VMs and synthetic keys | marked cross-owner RPC, project and credential fixtures | targeted diagnostic; required inside continuous P0 |
| `peer-cleanup`, `cleanup` | same retained allocation | exact marked fixtures and run worktrees | standalone idempotent cleanup/verifier |
| `--slot N` (`full`) | all prerequisites above | union of the marked scopes | mandatory continuous release gate |

Android/GPU, real credentials and external-service profiles use separate explicitly prerequisite-
gated lanes. A generic dependency-free resource pass does not report those handlers green.

Orca has three real-host fixtures. Run each on VM1 of an explicitly selected available slot:

```sh
dev/agent-e2e.sh --slot "$slot" --purpose orca-bootstrap --vm 1 -- \
  env SUBYARD_E2E_ORCA_BOOTSTRAP=1 bash tests/real-host/orca-bootstrap.sh
dev/agent-e2e.sh --slot "$slot" --purpose orca-existing-yard --vm 1 -- \
  env SUBYARD_E2E_ORCA_BOOTSTRAP=1 SUBYARD_E2E_ORCA_EXISTING_YARD=1 \
  bash tests/real-host/orca-bootstrap.sh
dev/agent-e2e.sh --slot "$slot" --purpose orca-resource --vm 1 -- \
  env SUBYARD_E2E_ORCA_RESOURCE=1 bash tests/real-host/orca-resource.sh
dev/agent-e2e.sh --slot "$slot" --purpose orca-projects --vm 1 -- \
  env SUBYARD_E2E_ORCA_PROJECTS=1 bash tests/real-host/orca-projects.sh
```

Bootstrap installs a packaged candidate and exercises public commands through a real terminal.
For the narrow Codex configuration regression, run:

```sh
dev/agent-e2e.sh --slot "$slot" --purpose orca-codex-config --vm 1 -- \
  env SUBYARD_E2E_ORCA_BOOTSTRAP=1 SUBYARD_E2E_ORCA_CODEX_CONFIG=1 \
  bash tests/real-host/orca-bootstrap.sh
```

This mode seeds representative TOML runtime additions, verifies that they do not block readiness,
repairs a deliberately changed managed policy through `config apply`, then verifies Orca restart,
saved-client connectivity and `migrate --check`. It skips the broader JSON import/sync and down/up
scenarios. The seeded additions model the observed drift; they do not prove which desktop action
writes each runtime field.

For Codex permissions across terminal and Orca launches:

```sh
dev/agent-e2e.sh --slot "$slot" --purpose codex-permissions --vm 1 -- \
  env SUBYARD_E2E_ORCA_BOOTSTRAP=1 SUBYARD_E2E_ORCA_CODEX_PERMISSIONS=1 \
  bash tests/real-host/orca-bootstrap.sh
```

This focused mode installs the native Codex CLI and stock Orca in a disposable yard.
It verifies managed policy reconciliation through `yard init` and exercises runtime approval
with synthetic model responses and disposable Git repositories. It uses no real agent credentials
or external model APIs. These checks do not replace interactive VS Code/Desktop acceptance or
establish a security boundary against alternative Git commands.

For temporary SSH-key access, run the focused current-candidate fixture:

```sh
dev/agent-e2e.sh --slot "$slot" --purpose orca-ssh-agent --vm 1 -- \
  env SUBYARD_E2E_ORCA_BOOTSTRAP=1 SUBYARD_E2E_ORCA_SSH_AGENT=1 \
  bash tests/real-host/orca-bootstrap.sh
```

It initializes two isolated yards, starts stock Orca and uses a synthetic encrypted key with a
real passphrase terminal. It checks Git pushes from a guest shell and an existing paired Orca
terminal, wrong-passphrase rejection, guest agent-mutation denial, cross-yard isolation and
new-authentication failure after revocation and expiry. This mode skips the packaging, upgrade,
configuration and port-collision scenarios; it does not install coding-agent CLIs or call model APIs.
The focused SSH and Codex configuration modes reuse the pinned Orca package cache after SHA-256
verification when available. They verify runtime integration, not a fresh Orca package download.

For SSH fixture debugging, opt in to `SUBYARD_E2E_ORCA_KEEP_FAILED=1` to retain a failed marked
fixture after revoking its key grants. The runner still releases the VM lease. On that same slot,
rerun with `SUBYARD_E2E_ORCA_RESUME` set to the reported fixture directory; the current candidate
reconciles the existing yards and repeats the assertions. Successful runs remove the fixture.

For a narrow predecessor upgrade check, set `SUBYARD_E2E_ORCA_UPGRADE_FROM` to an exact published
version and `SUBYARD_E2E_ORCA_UPGRADE_INSTALLER_SHA256` to that release's installer asset digest.
This mode starts Orca on a converged published release with two local yards, changes the candidate's
managed Claude defaults, then runs the public update command. It checks all-local config convergence, release
readiness, Orca restart, the saved client grant, endpoint and runtime JSON before cleanup.
If the update command fails, the fixture reports its marked temporary state path and retains the
test yards for diagnosis on the disposable VM. Clean up those exact yards and marked state after
the investigation.
The existing-yard variant completes release activation with Claude and Pi selected before starting
Orca, then checks release convergence and materialized settings. The resource fixture verifies
paired stock clients, terminal input/output, service lifecycle, exact owner/loopback routes and a
paired desktop under Xvfb. A bounded loopback DevTools driver uses the installed client's preload
API, reloads the renderer, verifies the remote project in its sidebar and requests normal closure.
Pairing capabilities stay in protected temporary files. Owner-address discovery is synthetic;
the fixture does not verify a real Tailscale account. Projects covers production local and SSH-remote
project actions, checkout discovery and preservation of identities and terminal tabs.

Standalone `reboot-verify` prepares its own power reconciler through the same marked
published-release upgrade fixture used by `power-systemd`. It works after the last test yard
was torn down, when the power service may be absent. This adds release installation and migration
to the two reboots; the fixture restores the previous host runtime afterward. It does not run the
additional systemd parser fixtures from `power-systemd`.

List or run one lane:

```sh
dev/e2e/p0-acceptance.sh --list-lanes
dev/e2e/p0-acceptance.sh --slot "$slot" --lane peer
dev/e2e/p0-acceptance.sh --slot "$slot" --lane source-upgrade --resume
SUBYARD_P0_WAIT_SECONDS=1200 \
  dev/e2e/p0-acceptance.sh --slot "$slot" --lane power-systemd
```

`SUBYARD_P0_WAIT_SECONDS` is a non-negative number of seconds passed to the atomic broker acquire;
zero keeps the fail-fast default. The `power-systemd` parser project is fully ephemeral: the lane
removes its marker-owned project and restores the snapshotted host unit/runtime before the phase is
checkpointed. The fixed `subyard-e2e-*` Ubuntu image alias is retained in the disposable allocation's
default Incus project, just like the real-Incus base-image aliases, so a later launch does not include
an unbounded remote transfer. Outer allocation teardown removes the alias with the Incus pool.
`--resume` never depends on retaining the parser project's mutable resources.

The cache fill and local launch both emit progress. Their independent positive-integer overrides are
`SUBYARD_SYSTEMD255_IMAGE_TIMEOUT_SECONDS` and
`SUBYARD_SYSTEMD255_LAUNCH_TIMEOUT_SECONDS`; restart uses
`SUBYARD_SYSTEMD255_RESTART_TIMEOUT_SECONDS`. A timed-out cache fill or launch fails once with its
operation and limit. The fixture never starts a second remote pull or launch after a timeout.

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

Open an unrestricted root guest session or run a root command:

```sh
dev/agent-e2e.sh --slot "$slot" --ssh 1
dev/agent-e2e.sh --slot "$slot" --ssh 2 -- id -u
```

The wrapper creates an ephemeral Ed25519 key per lease, starts a keeper that renews once per minute,
and releases in its exit trap. Ten minutes without a successful heartbeat expires the lease. For a
held slot, immediate busy, wait progress and timeout diagnostics use the bounded form
`owner=YARD/PROJECT run=RUN purpose=PURPOSE acquired=TIME expires=TIME label=LABEL`. They never include
a controller identity, lease credential, endpoint, host key or private path. The raw OpenSSH config
and lease capability are internal temporary files and are not an agent API.

Every wrapper invocation is a new lease for only the requested slot. The printed `e2e-vm-1` and
`e2e-vm-2` selectors always mean the two guests inside that outer pair, never global slot names.
Stateful multi-step work must stay in one script invocation or one interactive SSH session. Once
leased, the agent has unrestricted root in both guests and may create arbitrary nested yards,
brokers and leases. Nested slots are a separate namespace: inspect the nested broker's local status
and choose its slot locally instead of copying the outer `SUBYARD_E2E_SLOT`. `--slot N` requests only
the corresponding broker lease; it never enables direct VM, Incus or raw SSH access.

If configured capacity provides multiple available pairs, test them independently with separate
top-level runner processes. Give each process a different explicitly chosen slot and purpose, and
let each wrapper own its bounded keeper, release trap and cleanup. Do not combine allocations into
one request or use one slot as affinity for another.

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
3. for each running guest, best-effort runs bounded `sync` and `fstrim -av` to return free blocks
   to the pool;
4. stops both VMs;
5. publishes `available` only after stop is verified.

The pair shares one bounded trim budget. A slow first trim therefore reduces the time available to
the second trim instead of consuming the request time reserved for both verified stops.

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
yard -Y test-yard test-vms recover --slot "$slot"
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
yard test-vms logs -n 50 --slot "$slot"
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
dev/agent-e2e.sh --slot "$slot" --verify-boundary
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
