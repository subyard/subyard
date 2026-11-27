# Agent E2E VM pool

The `test-vms` profile declares an owner infrastructure service and runs a root-owned lease broker
inside its dedicated service container. Its configured
pool contains one or more shared slots and defaults to two. Each lease selects `subyard-pair`
(one or two standard VMs) or `android-test` (one larger VM). Slots own isolated inner Incus projects and networks;
each acquire creates fresh VM disks from a versioned immutable base. Release fences access and
deletes the disposable disks. No guest state survives a lease as a supported continuation mechanism.

Lease-taking commands accept `--image debian-13|ubuntu-24.04|ubuntu-26.04`; omission retains Debian 13.
The broker allowlists these choices rather than accepting arbitrary Incus images.
Each OS and environment type has its own current immutable base; pruning retains all four
current bases and every active lease pin. `yard test-vms refresh TYPE --image IMAGE` refreshes
only that combination, and status reports each base's image. Old callers and persisted leases
without image metadata retain their Debian behavior. Explicit selection requires the broker's
`image-v1` capability; clients refuse it on older brokers before acquiring a lease.


A Subyard update recreates a registered broker whose engine changed: open SSH sessions through it
end, its VMs, leases and base images are removed, its yard settings are kept, the registration is
recreated with the new runtime UUID, and the next acquire builds a fresh base. Agents holding a
lease acquire a new one. The engine is the broker's sealed engine, provisioning and recipe files;
`E2E_*` settings, a stopped broker, host units, routes and other drift are repaired in place. A
broker stopped by the operator is not changed. Rollback updates the broker in place.
Inner VM disks live in the default Incus pool on the yard root. Provisioning removes an earlier
release's `/srv/incus-e2e` pool directory from a `/srv` volume kept by `teardown --keep-data` once
no inner pool uses it.
The pre-v0.11 name `e2e-yard` is not updated; recreate it with
`yard -Y e2e-yard teardown --reset-config`, then `yard service install test-vms`.

The outer yard remains operator-owned. Agents can acquire inner slots, but cannot start a stopped
outer yard, enter its shell, reach its Incus socket or invoke arbitrary lifecycle commands.

## Pool facts for agents

Use the [generated CLI reference](cli-reference.md) and command `--help` for command
syntax. When diagnosis needs an owner-side command, give the operator the exact command;
agents own only their allocated inner VMs.

The `test-vms` service creation preset uses yard name `test-yard`, SSH port `2223` and
guest UID `1001` (avoiding the cloud image's UID `1000`). Existing runtimes retain their
configured settings. Workspace selection does not select the service.

Registration adopts a proven owned broker without initialization, container restart or lease
interruption. New-release reconciliation can register a uniquely proven owned broker while
retaining leases. Interrupted publication and owner HostID changes can require explicit
registration; use the [service activation contract](control-plane.md#owner-infrastructure-services)
for ownership checks and recovery. Service update reconciles the broker against the installed
runtime; ordinary runtime update installs releases. Acquisition never starts a stopped outer yard
or changes its desired power state.

Client delivery: the opt-in `test-vms-client` profile delivers the client and the `subyard-test-vms`
skill without a Subyard checkout or coding-tool integration; owner-side `yard -Y NAME provision
test-vms-client` adds it to the yard's local selection, and a role with `ALLOWS_HOST_ACCESS=false`
refuses it. Client and skill live in `/usr/local/share/subyard/skills/subyard-test-vms/`; static
skill refresh updates their discovery links, and removing the profile from `ENVIRONMENT_PROFILES`
and reconciling the yard retires them (a held lease's keeper still releases it after the idle limit).

For use from any yard without a checkout, see the
[`subyard-test-vms` skill](../config/profiles/test-vms-client/skills/subyard-test-vms/SKILL.md).
`dev/agent-e2e.sh` sources that same client and adds worktree copying, lanes and phase evidence;
its lease is bound to the wrapper and launching process.

### Capacity and test scope

Slots are shared across types. The default pool has two slots; shrinking refuses while retiring
slots are held, provisioning, draining or quarantined. Grants report the actual resource contract.

| Type | Guests | RAM per guest | Virtual disk per guest |
| --- | ---: | ---: | ---: |
| `subyard-pair` | 1 or 2 | 4 GiB | 20 GiB |
| `android-test` | 1 | 8 GiB | 40 GiB |

Admission reserves provisioning RAM and builder promises against the lower of visible
memory/cgroup headroom and verified physical-owner available RAM. Swap adds no RAM headroom;
missing physical counters refuse admission. With the default 512 MiB per-VM overhead and
4 GiB host reserve, a singleton needs 8.5 GiB available and a pair needs 13 GiB before other
pending requests or builders. Held VMs consume available RAM rather than retaining a growth
promise. The broker worker's separate process budget is not the VM budget.

Virtual disk capacity differs from physical usage and cache costs. The default disk quota is
unlimited, but the 5 GiB physical reserve and outstanding growth commitments still apply.
For `dir` storage, the disk budget charges image cache and allocated VM/snapshot blocks;
`btrfs` and `zfs` use conservative whole-pool usage. Deleting files inside a guest need not
reclaim host blocks until release deletes its volume. Use broker physical usage and growth
fields when diagnosing capacity; guest free space alone is insufficient.

Both types have the generic host baseline (Go bootstrap, build utilities, ShellCheck, Git,
network/archive tools and the product Incus installer). `android-test` selects larger resources;
Android SDK and emulator setup belong to the separate Android workflow. Images contain no
project data, credentials or project caches. On x86_64, broker VMs support ACPI S3 (`deep`)
with RTC wakeup; other architectures do not receive the broker's ICH9 S3 override.

Bases refresh on recipe changes, explicit request or after seven days. They take distro updates
only then: leased guests have the apt-daily timers masked, so an unattended upgrade cannot change
a guest during a lease or delay its release stop. Validation must pass before publication;
active leases pin their bases and keep their disks through refresh.
Legacy retained allocations need explicit safe retirement before reuse; releasing an active
legacy lease retains its old disks. Preserve needed data before proposing retirement.

Ordinary P0 uses the standard resources above: smoke, full and peer scenarios use two guests;
independent single-host lanes use one. `SUBYARD_P0_NESTED_VM=2` retains a pair for that selector.
Android acceptance uses one Android VM. A successful outer-broker lease does not prove the
candidate broker code. The host-free broker tests cover multi-slot quarantine, recovery and lease
scoping. `vm:staging-reliability` covers installed single-slot quarantine and recovery. The
test-vms `memory-boundary` lane checks the installed forwarding rule with two network namespaces
before acquiring its nested lease.

## Running tests

Choose checks using the [dev-flow](https://github.com/Dmitry-Borodin/dev-flow) check kinds and the Subyard parameters in
[Subyard dev-flow](../.agents/skills/subyard-dev-flow/SKILL.md#choose-checks-by-risk).
This guide describes VM access, prerequisites and execution. Normal checks use the current public
worktree, including uncommitted and untracked public files. Installed-runtime and reboot checks
package this source and install the resulting candidate. A published release is only an explicit
problem-reproduction baseline, as described in the skill's testing policy.
For the full host-free suite:

```sh
make build
./tests/run.sh
```

`make build` compiles the development binary at `.build/yard`; `go.mod` selects the Go toolchain.
CI additionally runs
`shellcheck -x -S warning` over the CLI, scripts, provision hooks, tests and Bash completion.
Linux CLI tests also require util-linux `script` to exercise resource commands with a real
controlling terminal, including terminal-input isolation and cancellation.

[Periodic E2E](testing.md#periodic-e2e-on-hosted-runners) also runs the long release VM checks on
GitHub-hosted runners, with a one-slot pool on each runner. Its results are findings, not
acceptance evidence.

For the combined existing-yard adoption and initialization regression, run
`bash dev/e2e/yard-adoption.sh --preflight`, package the current candidate with
`dev/release-acceptance.py prepare`, and pass its candidate bundle to the existing
agent controller. The one-VM command is
`dev/agent-e2e.sh --slot "$slot" --vm-count 1 --vm 1 --purpose yard-adoption -- timeout --signal=TERM --kill-after=60 1800 bash dev/e2e/yard-adoption.sh`.
The fixture installs that candidate and sequentially exercises a container and VM,
seeding only their project ownership metadata. It checks UUID, disk data, active
workload continuity, desired stopped state, and initialization in the same command.
This seeded native regression does not prove an authenticated historical upgrade.

The same fixture accepts `SUBYARD_E2E_ADOPTION_HOST_MEMORY_PROFILE=adoption-telemetry`
for a separate synthetic-profile candidate. Prepare that candidate from a disposable
plain source copy using the ordinary `dev/release-acceptance.py prepare` command;
add only `config/profiles/adoption-telemetry/profile.conf` containing
`PROFILE_NAME=adoption-telemetry` and `YARD_CAPS=host-memory`, plus an executable
`provision.sh` containing `#!/bin/sh`, `# subyard-provision-check-v1`, and `exit 0`.
Add nonempty `tests/e2e/acceptance.not-applicable` and
`tests/run.not-applicable` files explaining that the core yard-adoption fixture
owns this synthetic contract, so the normal profile inventory remains complete.
Use the same version as the ordinary candidate and set
`SUBYARD_E2E_ADOPTION_ENGINE_SHA256` to the ordinary candidate's engine checksum.
The fixture requires the installed synthetic candidate's engine to match exactly,
labels the added profile in evidence, and selects it during initial container setup.
It verifies the managed telemetry device before removing its device and ownership
marker to seed native absence, preserving the approved profile selection and guest facts.
It checks that one `init --adopt` registers
the yard and reconciles absent selected telemetry through ordinary init. It also
retains the registered-bind, container and VM checks. Keep both original candidate
receipts; synthetic profile assets do not establish shipped-profile acceptance.

When a selected check needs real GNU/Linux hosts, use an allocated `test-vms` slot.
The operator owns outer-yard start, stop and teardown; the
root broker owns inner slot create, start and stop; agents acquire leases only.

The independent continuation-grant lane is
`dev/e2e/p0-acceptance.sh --slot "$slot" --lane legacy-continuation`.
It reuses the power fixture's marked operator, authenticated published upgrade,
host-runtime restoration and two-reboot transport. After the installed upgrade,
it explicitly seeds a synthetic unmarked native project, confirms
`init --continue-legacy`, and verifies the unchanged protected grant, native UUID
and policy through repeated init and compatible rollback/forward. It is separate
from `reboot-verify` and is also included in the full P0 matrix.

The focused temporary-storage fixture uses one standard disposable VM and creates
its container and 1 GiB nested VM sequentially. It needs nested KVM, passwordless
root helpers in that allocation, and at least 10 GiB free on `/var/tmp`. Run its
owning local checks from the coverage table before acquiring a slot:

```sh
python3 dev/release-acceptance.py prepare --version 1.0.0-disk-tmp.e2e --output .build/disk-tmp-candidate
export SUBYARD_E2E_CANDIDATE_BUNDLE="$PWD/.build/disk-tmp-candidate/candidate-bundle.tar.gz"
export SUBYARD_E2E_CANDIDATE_SHA256
SUBYARD_E2E_CANDIDATE_SHA256="$(sha256sum "$SUBYARD_E2E_CANDIDATE_BUNDLE" | awk '{print $1}')"
dev/agent-e2e.sh --slot "$slot" --vm-count 1 --vm 1 --purpose disk-tmp -- \
  timeout --signal=TERM --kill-after=60 2700 bash dev/e2e/disk-tmp-acceptance.sh
```

It reuses the platform/image preflight and packaged-candidate installer, then
checks native materialized-config inspection and installs in the same fresh
`incus-admin` group context after platform preparation. Normal creation resolves
the supported remote image in each isolated yard project for both guest kinds;
it does not rely on a cache alias in the platform's default project.
It exercises ordinary minimal-yard initialization for both kinds. The fixture
explicitly configures `dev` with UID 1001, following the Debian-cloud
P0 convention, and verifies that identity before ordinary-user writes.
Rollback requires an actual root-owned native `tmp.mount` unit and preserves pre-existing boot links;
any exact link added to seed the tmpfs boot is removed without stopping the mount.
Native mask rollback
and restart establish an active tmpfs baseline; repeat initialization must preserve
a file, an unlinked open file and a Unix socket until the explicitly requested guest
restart. The fixture verifies a changed native container init PID or VM guest boot
ID and waits for the guest system manager to finish booting before inspecting the
mount. Failed cases retain bounded boot/mount properties and cleanup-step exits.
It checks actual disk backing, mode
1777, ordinary-user writes, repeat initialization and native teardown. The controller
retains responsibility for lease release; no outer-yard restart is involved.
Temporary-cleanup coverage also records an observation outside `/tmp` during
shutdown, after the cleanup unit and before unmount. It verifies freed blocks,
normal workload exit, hidden/nested payload removal, the unchanged root inode,
and protected symlink, bind-mount and tmpfs targets. Scoped periodic cleanup uses
controlled modification times and a real directory flock; it checks fresh and
unclassified data, unsafe traversal roots, native refusal/failure/timeout, live
policy rollback and boot recovery after a hard stop. This evidence comes from
the native shutdown observation, rather than inferring stop cleanup from boot
recovery. See the [guest cleanup contract](control-plane.md#release-migrations)
for policy scope, failure visibility and cleanup rollback.

To undo the policy on a yard, first verify that `/etc/systemd/system/tmp.mount` is
the `/dev/null` mask, then remove that mask with `systemctl unmask tmp.mount` inside
the selected guest. Its next normal stop/start restores the image's original mount
policy. Do not stop the live mount during rollback. Repeating `yard init` restores
the Subyard policy.

The focused producer lifecycle lane exercises operation-owned disk scratch, PID
namespace cancellation (including detached children), stale recovery, real Incus
controller loss and profile status rendering. Run its local prerequisites before allocation:

```sh
bash tests/provision-profile-check.sh
bash config/profiles/android/tests/android-provision-check.sh
bash dev/e2e/producer-lifecycle.sh --preflight
dev/agent-e2e.sh --slot "$slot" --vm-count 1 --vm 1 --purpose producer-lifecycle -- \
  timeout --signal=TERM --kill-after=60 5400 bash dev/e2e/producer-lifecycle.sh
```

This requires Linux user/mount/PID namespaces locally, one standard disposable VM,
passwordless guest root helpers, and the existing Incus platform fixture. It creates
one marker-owned nested container and removes it. It does
not impose per-operation memory, byte or inode quotas, and does not cover ARM or
release acceptance.

## Agent workflow

The agent resolves the owner's trusted public service registration and its pinned restricted
SSH facade route. `--owner ID` and `--pool ID` are exact selectors. The default has no yard-name or
SSH-config fallback. The operator's public route mount contains no owner configuration or lease
credentials. Controller state is isolated by owner, pool and registration generation; an acquired
lease keeps its captured endpoint and host-key pin through renewal/release even if publication changes.
Explicit `--yard NAME` remains available for operator-controlled pre-registration rollout transport.
It cannot be combined with service selectors and grants no container lifecycle or shell access.


Prepare the persistent controller identity once:

```sh
dev/agent-e2e.sh --prepare
```

The persistent controller key stays under the agent user's `~/.subyard/e2e/`; every lease uses a
separate ephemeral guest key.

A standard caller reaches the outer yard through the provisioned yard-to-yard route. Any valid
Ed25519 controller key is admitted only to the versioned forced facade
(`status/acquire-v3/renew/release/logs-v1/incident-v1`). It never receives an L1 shell, PTY, file transfer, arbitrary
forwarding or Incus access.

Inspect the redacted pool without acquiring:

```text
SLOT     STATE        YARD             PROJECT                          RUN        PURPOSE                  AGE      EXPIRES
slot-001 held         default          Subyard-2                        c291a4ef   release-migration        3m12s    in 16m48s
slot-002 available    -                -                                -          -                        -        -
```

```sh
dev/agent-e2e.sh --status
dev/agent-e2e.sh --status --json
```

Status also reports visible memory/cgroup evidence, verified physical-owner memory availability,
physical Incus pool driver and used/free bytes,
budget limits, VM virtual-capacity reservations, private builder reservations, and base fingerprints
with age/current/expired flags. Shared copy-on-write blocks are counted only by Incus pool usage;
virtual disk limits and compressed image sizes are separate quantities. Missing telemetry is an
explicit gap, including inaccessible L0 host evidence and unavailable measured builder peaks.
Status does not build, refresh, repair or collect garbage and bounds its Incus probes to three seconds.

The active holder is reported as `yard + project + run + purpose`. Project is the canonical Subyard
project name from managed workspace metadata, and run is a new public correlation ID per acquire.
Before metadata convergence, a safe enclosing legacy project ID is reported unchanged with
`yard=unknown`; the runner never strips a suffix or guesses a name. These fields are untrusted
display metadata: authorization and fencing still use hidden lease credentials. Status never
publishes controller fingerprints, lease IDs/capabilities, absolute checkout paths, Git credentials,
command lines, guest endpoints or the full failure reason. A
quarantined or recovering slot instead exposes bounded recovery metadata:
`last_failure_event_id`, `incident_id`, `recovery_attempt` and `next_recovery_at`. For an available
empty slot, the attribution columns are empty.

Frozen physical acceptance may set `SUBYARD_E2E_CONTROLLER_WORKSPACE` to the managed controller
checkout for lease attribution when candidate source runs from a temporary snapshot. The runner
still resolves and validates that checkout and its project metadata; this does not change the
source root copied to guests.

The runner requires `attribution-v2`, `environment-acquire-v3` and `disposable-v1` from read-only
status. When the broker advertises `process-acquire-v4`, the runner uses `acquire-v4` with the
same arguments as `acquire-v3`. Otherwise it retains the released `acquire-v3` protocol; that
older broker cannot provide immediate pre-grant cancellation. Legacy acquire is not supported,
and the runner never switches protocols in response to acquire failure. Only a typed busy or
capacity response plus bounded `--wait` permits another request for the same slot; a transport
failure or any other unknown outcome ends the attempt. Without `--wait`,
`SUBYARD_E2E_CAPACITY_WAIT` (for example `30m` or `90s`) bounds resource and slot-contention retries
with one deadline, including a slot that becomes `provisioning` or `held` while waiting for memory;
the release acceptance collector sets it to `120m` for VM checks unless
already set, and hosted CI collector selections set `30m` to stay inside their job limits. The runner retries only while another request can start before the deadline, so a
late grant is never needed. When the bound expires, the final typed capacity refusal and exit 4
remain visible. Only `provisioning`, `held`, `draining` and `recovering` are transient slot states.
Their refusal records `resource=slot state=STATE` with exit 4, immediately when no wait is
configured or after the deadline; the collector records it as retryable `capacity_slot`.
`quarantined` and `unavailable` fail immediately with exit 2 and require operator action.
The exact requested slot is retained throughout the wait. A `held` busy response
contains exactly the safe owner fields `display_label`, `yard`, `project`, `run`, `purpose`,
`acquired_at` and `expires_at`. Other unavailable states carry no owner object. The runner validates
the complete response before retrying; a missing, extra or malformed owner field makes the outcome
unknown and prevents another acquire.


`acquire-v4` streams a `reserved` grant before provisioning and an `ok` grant when ready.
The wrapper retains the early credentials for immediate release and acknowledges the ready grant
with `ack` after validation and keeper startup; held lease expiry then follows the existing policy.
Until ACK, the lease records its worker PID, start time and boot identity. The guard checks the
wrapper/requester each second; EOF, invalid frames, fifteen seconds without keepalive, explicit
release, or worker loss initiate cleanup. Release publishes `DRAINING` before waiting for the
lifecycle lock, allowing provisioning to observe cancellation within one second.

Cleanup uses a separate bounded context and preserves recovery state on failure. `AVAILABLE`
requires terminal native operations and verified absence of owned guests; empty inventory alone
cannot close a late Incus creation. Late guests retain generation/epoch checks. Lost replies use
the same recovery path, and the wrapper distinguishes unknown server cleanup from local cleanup.

`config/profiles/test-vms/tests/e2e/broker-memory-boundary.sh --pregrant-cancellation` checks early TERM/KILL, native working
creation cancellation, zero pending VM/builder memory, and fresh acquire/release. Its native
subscription is confirmed on a held warm guest; a bounded lifecycle-lock barrier binds the exact
requester/allocation before the observer signals on a matching live project/resource event.
The owning preflight covers JSON separators and bounded observer diagnostics. Add
`--working-creation-only` for that case's fresh warmup/cancel/reacquire sequence. Neither mode
runs unrelated pressure, update/rollback or held-expiry checks.

Explicit `--vm-count` requests additionally require `vm-count-v1`. The facade accepts
`acquire-v3 TYPE client_id fingerprint yard project run purpose key_type key_blob slot_id [vm_count [disk_size]]`.
The optional count is exactly `1` or `2`; Android supports only `1`. Unsupported counts are
rejected before slot acquisition. Older requests omit the count and preserve their type default.
Explicit disk requests require `disk-size-v1`, append the resolved count and exact `50GiB`
preset, and verify the returned size. Other disk sizes are rejected before acquisition.
An older broker without the relevant capability refuses explicit requests before acquisition; update
and initialize it through the normal operator workflow before using the new acceptance controllers.

Use redacted status to inspect the configured pool, explicitly choose an available slot number, then
run against the selected environment in only that slot. P0 always requests `subyard-pair`. For example, after choosing slot 1:

```sh
slot=1
dev/agent-e2e.sh --slot "$slot" --vm-count 1 --purpose host-free-suite -- ./tests/run.sh
dev/agent-e2e.sh --slot "$slot" --vm-count 1 --wait 20m --purpose host-free-suite -- ./tests/run.sh
dev/e2e/p0-acceptance.sh --slot "$slot"
dev/agent-e2e.sh --slot "$slot" --vm-count 1 --purpose real-host-check --vm 1 -- \
  ./tests/some-real-host-check.sh
```

The runner filters private and ignored files, verifies the worktree bundle and removes its guest
worktree. Guest archives, extracted sources and native build outputs use private directories under
`/var/tmp` on persistent storage; the runner rejects tmpfs and ramfs staging so release assets do not
consume the guest's RAM reserve. Every lease-taking invocation prints `yard + project + run + purpose`,
environment type, VM count and base fingerprint. Human status includes the environment type and per-VM resources.
`--type android-test` selects one guest; omit `--vm` to run on all actual guests. Explicit `--vm 2`,
`--vm both`, `--ssh 2` and pair boundary checks fail before acquiring an Android lease. The default
`--type subyard-pair` preserves existing pair callers.

For Android acceptance, request **one** `android-test` VM (8 GiB RAM / 40 GiB disk):

```sh
dev/agent-e2e.sh --slot "$slot" --type android-test --purpose android-pool-runtime -- \
  bash config/profiles/android/tests/e2e/android-pool-runtime.sh
```

A single VM can host the Android yard and both emulator pool slots. A pair is needed for tests
that actually coordinate two independent hosts, such as the full P0 matrix; it is not a general
requirement for Subyard development. `--vm 1` on the default `subyard-pair` only selects where
the payload runs: the broker still allocates both guests unless `--vm-count 1` is supplied.
That singleton reserves 4.5 GiB including VM overhead, rather than the pair's 9 GiB.
OpenClaw, GitHub, Hermes, Orca and subyard-dev acceptance request one standard guest; Amnezia
retains its owner/client pair. Android uses its larger guest except for the skills lane below. The generic Android VM base
does not include the SDK; the fixture provisions it.
The ordinary Android `--lane full` gate initializes and provisions a real yard, builds one
small launchable APK with the installed SDK tools, verifies APK semantics and its signature,
then removes the disposable SDK environment through normal marker-owned yard teardown. It
verifies the SDK's owned `/srv` backing and volume, instance, project and state removal,
preserving the reusable yard definition and an unrelated user file. It needs no Gradle download or emulator runtime.
See [Android acceptance](android.md#release-acceptance).

For core facts, refresh recovery and native Codex/Claude/OpenCode discovery without profiles:

```sh
bash dev/e2e/guest-environment.sh --preflight
dev/agent-e2e.sh --slot "$slot" --vm-count 1 --purpose guest-environment --vm 1 -- \
  bash dev/e2e/guest-environment.sh
```

Use the normal frozen candidate bundle/checksum inputs. The singleton needs 14 GiB free disk
and 2 GiB available RAM. pi/Hermes coverage checks discovery links only.

For agent skill delivery and native discovery, use the focused Android lane:

```sh
config/profiles/android/tests/e2e/acceptance.sh --slot N --lane skills
```

This lane uses one standard disposable VM, requiring 14 GiB free disk and 2 GiB available
RAM before setup. It installs the packaged candidate and provisions the Android capability, then checks
repeat static refresh, missing discovery-link repair through a fresh host-wide
`yard migrate` assessment followed by ordinary initialization,
and provider disable while preserving
the pool process and an unrelated user skill. It installs native Codex, Claude Code and
OpenCode clients after skill delivery and queries their loaders from a separate disposable
Git project inside the yard. The skill content comes from the installed runtime; no Subyard
checkout is mounted there. Codex uses `skills/list`; OpenCode requires its native `debug skill`
command; Claude's startup stream and outgoing skill metadata use a loopback mock API with a
synthetic key. These checks establish native metadata discovery, without provider credentials
or model inference. Description-based model selection and comprehension remain separate
checks. This lane allocates no emulator and does not replace the ordinary SDK/build gate.

For L2 isolation and project lifecycle changes, use `--lane projects` with
`config/profiles/android/tests/e2e/acceptance.sh --slot N`. This lane performs fresh yard setup,
namespace preflight, broker readiness and public API 35/36 image preparation before running the
existing two-project fixture. It checks parallel Gradle/APK builds, read-only SDK access,
private project caches, absence of direct devices and display sockets, broker device/lease
lifecycle, project rebuild and migration from the reconstructed legacy mount/device shape.
It exits through normal project and yard cleanup before phone/viewer setup. Physical viewer
and recovery coverage remains in the diagnostics lane.

The optional `--lane diagnostics` retains the broader phone/API 36, native/remote viewers,
recovery, APK installation, clean userdata, lease expiry and sequential API 35 phone/API 36
tablet checks, including the two-project Gradle builds and legacy rebuild fixture. On the
standard 8 GiB VM, it checks one emulator and a typed safe memory refusal for a second request,
preserving the first lease and runtime. It releases the first emulator before successful
standalone acquire, rendering, renewal and release. The idle-display recovery check returns
the device to ACTIVE before cancellation and cleanup. Attached viewer startup is deliberately
delayed by 20 seconds. These checks are separate from the ordinary release gate.

After a viewer-only change, pass `--lane viewer` to `config/profiles/android/tests/e2e/android-pool-runtime.sh`.
It checks owner, yard and remote controller viewers, including real SSH transport and rendered
frames, delayed startup, attachment without lease renewal or release, and a standalone viewer
after the borrowed lease is released, then cleans up. Remote controller coverage also checks cancellation
and EOF cleanup of wire processes. It prepares both API35 and
API36 images, skipping the separate remote workload-run and repeated API35 preparation assertions.
Those assertions remain in the diagnostics lane. It uses the same single VM.

After a recovery change, pass `--lane recovery` to
`config/profiles/android/tests/e2e/android-pool-runtime.sh`. This lane performs its own fresh setup and image preparation,
then checks pool and yard restarts, stale lease fencing, final remote owner execution, cache pruning
and cleanup. It boots both phones across the restart sequence and skips viewer subtests and scrcpy
installation. The diagnostics lane retains the owner, attached and standalone viewers before recovery.
It skips the separate L2 builds, concurrent device lifecycle and expiry checks; retain their
source-specific evidence when those paths are unchanged. The default remains the small SDK/build/removal
gate. Android fixtures emit bounded `E2E_PHASE` durations and exit codes. Diagnostic lanes also emit
validated numeric tool versions, capture observations and read-only numeric monitor samples.
The default SDK/build lane repeats provisioning and rejects another toolchain archive download.
Image-bearing runtime lanes repeat API 36 preparation after verifying its catalog cache entry.
They report preparation durations, installed image bytes and nested-yard `eth0` receive counters
before and after provisioning and image preparation. Counter differences include apt and metadata
traffic; missing counters are `unknown`, not zero. The smaller `--lane sdk-images` diagnostic
retains the same image cache measurement without booting an emulator.

Unknown, incomplete or timed-out display queries remain unknown. Scrcpy download and extraction
are measured separately; these timings do not establish a reusable dependency cache.

For first SSH trust and continuation of ordinary remote commands, run the focused fixture on a
free slot:

```sh
dev/agent-e2e.sh --slot "$slot" --purpose ssh-unknown-host --vm 1 -- \
  bash tests/real-host/ssh-unknown-host.sh
```

It initializes a disposable yard, uses an isolated owner SSH server and controller trust store,
removes only its test yard's key, and checks confirmation and continuation of `sync`. It never
edits the operator's SSH trust. This focused check does not replace the full P0 gate.

The default pool has slots 1 and 2. Use status to find the configured capacity. Host-free broker
tests check that exact-slot operations leave unselected slots and their allocation identities intact.

### Test lanes and gates

The focused configuration check uses one fresh pair and a synthetic private SSH Git remote.
It installs the same runtime artifact built from the current worktree on both hosts, verifies two-way
configuration convergence, and rejects dirty, diverged and conflicted checkouts without changing
live settings or remote refs:

```sh
bash dev/e2e/config-sync-acceptance.sh --slot "$slot"
```

[Periodic E2E](testing.md#periodic-e2e-on-hosted-runners) runs this check by default as
`periodic:config-sync`; a manual dispatch can select it with `checks=periodic:config-sync`.
It is not part of release acceptance. The [coverage ownership map](testing.md#coverage-ownership)
requires it for configuration-sync and configuration-source SSH/Git transport changes.
The hosted controller has a provisional 90-minute timeout plus 10-minute TERM cleanup grace,
pending measurement. It uses the standard pair: 9 GiB including VM overhead, with a 2 GiB hosted
memory reserve. The hosted wrapper uses 12 GiB VM disks when free space is below 65 GiB;
normal broker memory and disk admission still apply.

The boot check uses one standard VM. It packages the current worktree and installs it through the
supported installer, reboots the VM, observes the installed power service waiting for a missing
required proxy address while an owned optional publication keeps its yard and local workload
running. A marked boot service supplies both exact addresses; publication returns without restarting
the running yard. It also checks explicit start, retry/cancellation, ownership drift, independent
ready-yard startup, desired power, root-owned runtime/unit, host networking guards, the default
route and cleanup:

```sh
bash dev/e2e/proxy-address-boot.sh --slot "$slot"
```

Both controllers keep one lease through their phases and remove only their marked fixtures.
They report the source bundle and installed artifact identities. The boot check requires public
package/image download access. These focused results prove their current-candidate scenarios;
they do not replace the combined release acceptance gate or external-account checks.

Canonical remote project routing uses the focused preview fixture on a fresh pair:

```sh
dev/e2e/preview-acceptance.sh --slot N --remote-only --canonical
```

It checks `sync` and `code` through `HostID/yard`, real SSH forwarding, and owner
role refusal without changing controller or owner state.

Codex Desktop source and route acceptance reuses that disposable pair:

```sh
dev/e2e/preview-acceptance.sh --slot N --codex-export
```

This mode checks local default/named and remote alias/canonical selection, fresh
owner inventory, additive protected declarations, explicit import retries and
pre-write refusal of unavailable or L0-only SSH routes. It uses a synthetic
desktop URL handler for import-request dispatch. This is the export integration's
acceptance lane; physical desktop hardware and an external Tailscale account are
not required. The lane uses ordinary SSH routes between disposable VMs.

The same-host network policy acceptance creates synthetic local yards and verifies explicit
links, isolation toggles, spoofing protection and managed lifecycle behavior. Choose an available
slot from fresh status, then run:

```sh
dev/e2e/yard-network-policy.sh --slot N
dev/e2e/yard-network-policy.sh --slot N --vm 2  # select the second guest when needed
```

This controller owns one lease across setup, a selected-guest reboot and resumed validation;
`--vm` accepts `1` or `2` and defaults to `1`. With `SUBYARD_E2E_CANDIDATE_BUNDLE` it installs the
frozen candidate through the supported installer inside its marker-owned state; without one it
packages the transported worktree the same way. It changes
network policy only inside the disposable VM and does not enable isolation on the operator's host.
The skill's risk criteria determine whether the focused network lane needs broader lifecycle coverage.

The operator-extras upgrade lane installs published 0.28.0 with a host-scope `YARD_DEVICES=kvm`,
checks the attached `yx-dev-kvm`, then updates to the frozen candidate. The candidate must disclose
the ignored assignment in `config show` and `update --check --json`, name the device in the
unconfirmed `init` plan without changing it, remove it with the printed `config unset` and detach it
on `init`:

```sh
bash dev/e2e/operator-extras-upgrade.sh --preflight
SUBYARD_E2E_CANDIDATE_BUNDLE=/absolute/path/to/candidate-bundle.tar.gz \
SUBYARD_E2E_CANDIDATE_SHA256=<bundle-sha256> \
  dev/agent-e2e.sh --slot "$slot" --purpose operator-extras --vm-count 1 --vm 1 -- \
  bash dev/e2e/operator-extras-upgrade.sh
```

Periodic E2E includes optional `periodic:nm-guard/fresh` and `periodic:nm-guard/upgrade`
checks outside release acceptance. They use the workflow's selected lease image.
For a local run, append `--image ubuntu-24.04` or `--image ubuntu-26.04` to the command below.
Omission keeps Debian 13.

The NetworkManager guard lane owns its lease and reboots the VM itself. `--lane upgrade` runs a
published 0.29.0 init that stops after writing its guard, which lets NetworkManager take the
administrator's uplink; the candidate update must refuse and name the hand-back, then rewrite the
guard without touching NetworkManager, give a notice for an undecidable `docker*` list entry and
ask for one reboot, and rollback across the device-guard ledger boundary must be refused. Init,
start and stop work before the reboot; after it the boot reconciler restores the yard. `--lane
fresh` installs the candidate behind an except-only administrator list, an NM-managed `br0` and an
administrator file nobody has applied: the first init writes the guard and stops for a reboot
without touching NetworkManager, and a NetworkManager restart does not release it; after the
reboot init completes, new `eth9` veth and `tap` devices and a restarted
guest stay unmanaged, a second Incus bridge is listed and unmanaged, and the boot reconciler
restores the yard after another reboot:

```sh
bash dev/e2e/nm-admin-uplink.sh --preflight
SUBYARD_E2E_CANDIDATE_BUNDLE=/absolute/path/to/candidate-bundle.tar.gz \
SUBYARD_E2E_CANDIDATE_SHA256=<bundle-sha256> \
  dev/e2e/nm-admin-uplink.sh --slot "$slot" --lane upgrade
```

`dev/e2e/p0-acceptance.sh --slot N` runs the release smoke; `--lane full` runs the exhaustive
compatibility and recovery matrix. Release collection requests these long VM runs explicitly;
its default performs host-free/local checks without allocating. The skill defines when each applies. GitHub workflows do not
receive pool access or run these checks automatically. `--list-lanes` does not acquire and needs no slot.

During the full owner compatibility chain, before current `yard init`, VM1 seeds the legacy
convergence fixture with:

```sh
SUBYARD_E2E_LEGACY_FIXTURE=1 \
  dev/e2e/seed-test-vms-legacy-state.sh subyard-test-yard yard-test-yard
```

This fixture is restricted to disposable VM1 candidate yards.

The focused AppArmor regression installs the current packaged candidate through the supported
installer (including its committed release-transition ledger), then creates a temporary container
yard on VM1, exercises failed capability probes and both real Incus AppArmor transitions, and verifies Docker and runtime
preservation. It restores its marked systemd override and tears down its yard. Run it on a free
slot whose inner Incus starts with AppArmor enabled; a disabled baseline is refused. It runs in
Periodic E2E as `vm:incus-apparmor-probe`, outside the release catalog, and provides targeted
evidence, not a full P0. To use a frozen candidate, prepare and transport it as follows; without
candidate metadata, the fixture packages the transported current source inside the guest:

```sh
python3 dev/release-acceptance.py prepare --version 1.0.0-apparmor.e2e --output .build/apparmor-candidate
export SUBYARD_E2E_CANDIDATE_BUNDLE="$PWD/.build/apparmor-candidate/candidate-bundle.tar.gz"
export SUBYARD_E2E_CANDIDATE_SHA256
SUBYARD_E2E_CANDIDATE_SHA256="$(sha256sum "$SUBYARD_E2E_CANDIDATE_BUNDLE" | awk '{print $1}')"
dev/agent-e2e.sh --slot "$slot" --vm-count 1 --purpose apparmor-probe --vm 1 -- \
  bash dev/e2e/incus-apparmor-probe.sh
```

The Incus group regression uses a temporary operator whose Incus socket is unreachable:
it has no `incus-admin` membership. It installs the packaged candidate through the public
installer, which must publish the entrypoints and a ready release without touching socket
access. It then runs the first plain `init` of a new named yard, with no yard file written
beforehand and host-wide settings, including persisted power paths. It verifies native
named socket and policy-lock ACL activation in the original prepared process, retained lock
identity, an explicit `DEV_UID` command override, an SSH port of its own that leaves the
default yard's unchanged, and an idempotent retry.
It removes the marked operator ACLs, account and yard after the check:

```sh
dev/agent-e2e.sh --slot "$slot" --purpose incus-group-reexec --vm 1 -- \
  bash dev/e2e/incus-group-reexec.sh
```

The staging reliability fixture uses one Android VM for its 40 GiB disk, creates a marked
broker owner, and cold-builds one base with post-upgrade boot validation. It observes the
first boot of one small nested singleton and one stop/start under the same lease. It keeps
the normal 300-second boot budget and admission reserves. The first unexpected failure ends the
attempt; it never reacquires or injects a timeout. The two-hour controller cap includes cold
preparation; TERM has a two-minute cleanup grace.

Its owner preparation uses the product's project and instance adapters, then
`testvmsruntime.Backend.Apply` and native readiness verification from the installed
candidate. Cloud-init creates only the disposable owner's user and SSH prerequisite;
user, Python and SSH service checks precede broker setup. The product backend owns
Incus/QEMU installation, the live physical-memory bind, worker, recipes, controller
route and systemd resource limits. Generic Docker and development-tool provisioning
is omitted. This fixture proves the broker boot scenario, not whole-yard initialization.
Evidence export precedes native broker drain and exact marker-owned owner cleanup.
After both boots and a successful ordinary release, a fresh sequential singleton uses
the same owner and validated cached base. An existing one-shot Incus inventory fault
returns exit 1 for the exact project-list command during release, then delegates every
other command to real Incus. It exercises native fencing, quarantine and immutable
incident capture without a timeout, extra owner or concurrent guest. Physical-owner
host capture and immutable sink replay must pass before exact broker configuration
restoration and native reaper recovery. Recovery must advance one generation, preserve
the incident timeline and leave no allocation instances or root volumes. Its existing
6000-second recovery bound remains inside the unchanged 7200-second controller cap;
the cap can leave this gate incomplete. The controlled inventory failure proves this
boundary separately and does not explain an earlier boot stall.
Fault staging has an exact fixture marker and canonical nonsymlink files; backup and
current configuration content, mode and ownership are checked before restoration.
Cleanup restores the fault before restarting recovery or retiring the owner. A guard
refusal retains the owner and reports cleanup failure; shared cleanup cannot bypass it.
The fixture verifies an owned runtime drop-in that refuses explicit reaper starts
while its timer is stopped. It restores that drop-in before native recovery, and
refuses to restart the timer against configuration that could not be safely restored.

The 40 GiB virtual disk does not guarantee 35 GiB of usable free space after filesystem,
base-image and owner provisioning costs. The nested directory pool shares that physical
backing. After compilation and controller preparation, the fixture reclaims its marked
Go build cache, supported default Go build cache and the disposable VM's Go module cache
through guarded helpers. It also cleans the verified owner container's downloaded APT
archives and removes only package staging created by this run after installed-worker
verification. Installed runtimes, release caches, recipes, images and pins remain intact.
Once owner installation is complete, it also removes reproducible downloaded APT
indexes inside the verified disposable owner and allocated VM. The privileged VM
actor verifies its original schema-2 lease and exact fixture marker before deletion;
both actors require native APT's canonical lists directory. Nonblocking POSIX APT/dpkg
locks exclude concurrent package activity; complete ownership and nonsymlink checks
precede deletion. Package databases, sources, directories and lock inodes are retained.
The allocated VM actor also validates the canonical native APT archive directory and its
entries, then runs bounded `apt-get clean` after releasing its preflight locks so native
APT can acquire its own locks. Before/after categories report APT index bytes in both
scopes and archive bytes in the allocated VM. Failed archive cleanup stops the fixture;
reclamation still requires a fresh physical-capacity admission result.
Fixed numeric categories cover caches and staging in both the allocated guest and its
owner container; missing measurements are explicit. The owner provisioner copies the
compiled broker rather than building it inside the container.
After reclamation, fixed numeric disk-footprint categories also locate retained system,
source, fixture and Incus storage costs in both scopes. Parent and child categories
overlap and must not be summed; symlinked, inaccessible or timed-out paths report null.
These read-only measurements do not authorize deletion of installed or cached content.
It records numeric backing capacity and broker telemetry before and after reclamation,
before refusing insufficient or unknown capacity. The cold builder still requires 30 GiB
plus the normal 5 GiB physical reserve; reclamation does not guarantee admission.

Run the fixture with:

```sh
timeout --signal=TERM --kill-after=120 7200 dev/agent-e2e.sh \
  --slot "$slot" --type android-test --purpose staging-reliability --vm 1 -- \
  bash dev/e2e/staging-reliability.sh
```

Release acceptance runs the same fixture as `vm:staging-reliability` through
`dev/e2e/release-vm-check.sh`, whose lease purpose is `release-staging-reliability`. The allocated
VM APT actor accepts only that purpose, `staging-reliability` and the test-vms
`broker-cold-upgrade` lane.

The fixture emits source/installed-engine identities, installed Incus/QEMU/package versions,
guest kernel/agent identities when reachable, and five-second measurements of marker-verified
QEMU PID/start time, task state, wait channel, scheduler counters, PSI and cgroup counters.
It projects early and late RESET/SHUTDOWN/RESUME and system_reset/cont/quit records from existing
QMP log files, including the rotated first-boot log. Default runtime/log paths are checked;
missing measurements remain explicit. It never connects to Incus's QMP socket or consumes its
console ring buffer during a lifecycle operation. Console capture follows native completion.
Durable emergency incidents, host observations and boot samples are printed before marker-owned
cleanup. The reaper is restored even on failure; restoration failures change the fixture result.
Successful boots produce `causal_result=inconclusive_no_stall_observed`. This fixture does not
establish release acceptance or identify the cause of an intermittent kernel/agent stall.

The physical-memory regression creates a marked broker yard on VM1. It checks the live
read-only owner meminfo bind, rejects missing and ordinary-file sources, repairs a removed
device without restarting the yard, and verifies boot persistence. It holds a small nested
pair during ordinary update and rollback, checking lease identity, guest uptime and continued
renewal. Its cold base builder reserves 30 GiB of disk before the nested pair can start,
so use the generic 40 GiB `android-test` target; the standard 20 GiB guest has insufficient
disk headroom. The fixture verifies available disk before requesting the nested lease.
It also verifies the broker's separate 2 GiB RAM plus 2 GiB swap budget across setup and boot,
compares live owner counters with admission arithmetic, and runs a pair whose combined startup
budget is 5 GiB outside that process ceiling. Finally it kills the nested requesting wrapper,
checks that renewal stops and waits for the advertised 20-minute expiry and disposable cleanup,
then verifies SSH fencing and clean reacquisition. Run it with:

```sh
bash config/profiles/test-vms/tests/e2e/acceptance.sh --slot "$slot" --lane memory-boundary
```

Release acceptance declares it as `profile:test-vms/memory-boundary`, independently of
the cold-upgrade and full-uninstall lanes. The controller runs the normal test-vms local
prerequisites and leases one `android-test` VM for the full broker fixture, including
renewal, natural expiry and clean reacquisition. Telemetry calls the same fixture with
`--telemetry-resources` for short allocation/held-pair observations and ordinary release;
it leaves renewal and natural-expiry waits to this owner lane.

Owner and broker fixtures resolve the default-project container cache alias to its exact
fingerprint, then use that fingerprint on the public remote selected by
`P0_REAL_INCUS_CONTAINER_IMAGE` (default `images:debian/13/cloud`). The remote must be public
Incus or simplestreams and still serve that exact image; unavailable images and local-only
or OCI sources fail before owner setup. Public init creates the owner project with its own
installation records. The independent `real-incus` lane keeps its image override contract.

The owner fixture uses explicitly synthetic, supported-version bundles built from the
current frozen source and its ordinary V2 registry. It checks inactive broker preservation,
active helper replacement and rollback, then creates a fresh current-owned `test-yard`
through public update and init while retaining protected history. These bundles do not
represent authentic historical releases. Published-release fixtures start at v0.28.0. Native deletion and checkpoint
resume belong to `full-uninstall`; older product recovery readers and the schema-1 recovery plan refusal
retain host-free contracts.

| Lane | Prerequisites and timeout | Mutable scope | Classification |
| --- | --- | --- | --- |
| `./tests/run.sh` | Go toolchain; bounded by CI | temporary host-free roots and `.build/yard` | full host-free suite |
| `dev/process-coverage.sh` | Go toolchain; selected host-free process contracts | `.build/coverage` and test-owned temporary roots | optional coverage diagnostic |
| `smoke` (default) | both allocated VMs; capacity/dependency preflight and one reboot | marked Incus, release and peer fixtures described below | publication smoke; see skill policy |
| `boundary` | one broker lease; SSH connect deadlines | read-only facade, routes and negative probes | required in smoke and full |
| `transport` | both allocated VMs; bounded SSH disconnect probe | one marker-owned remote stdin reader that must end on disconnect, and a temporary controller log | required in smoke and full |
| `nested-teardown` | frozen current candidate, VM2, KVM and nested Incus; bounded install, boot and cleanup waits | marker-owned outer VM and nested yard, each bootstrapped through the public installer; data-boundary fixtures | targeted diagnostic; required in full |
| `full-uninstall` | one allocated VM and installed current candidate; bounded phase/reboot deadlines and a sink delay longer than 60 seconds | marker-owned yards, producers, credentials and installation roots; preserve foreign resources and selected-yard neighbors | singleton held-lease uninstall, interruption/retry and reboot acceptance |
| `dependencies` | immutable image baseline; 20-minute cold Go download deadline | marker-owned cold caches only | periodic targeted bootstrap diagnostic |
| `real-incus` | VM1 when targeted/smoke and in the full owner chain; KVM, persistent Incus pool and 15-minute mutation deadlines | marked project, container, VM and image aliases | required in smoke and full |
| `profile-resource` | VM1 and current candidate | marked bound-resource profile | targeted physical diagnostic; generic resource consent/state checks run locally |
| `newer-release` | VM1; published v0.31.0 direct pin with the staged candidate's migration registry, Go toolchain and prepared base image | marked fixture project and isolated runtime under `/var/tmp` | required in smoke and full |
| `release` | VM1 fixture with two-VM allocation/preflight; bounded nested install/boot deadlines | fresh candidate yards, current and legacy convergence | targeted diagnostic; covered by the full owner chain |
| `power-systemd` | VM1 when targeted; VM2 worker in full; real Incus, Ubuntu 24.04/systemd 255; 900-second image-cache fill, 600-second local launch, 300-second restart and bounded TERM-to-KILL Incus commands | test-owned image alias, marker-owned synthetic incompatible-unit parser project plus snapshotted/restored host power runtime | targeted diagnostic; required in full; release acceptance runs it as `vm:p0-power-systemd` |
| `reboot-verify` | VM1, real Incus and cached image preparation; published-anchor upgrade, refused anchor rollback and a candidate-built compatible rollback/roll-forward fixture; two boot checks with bounded power reconciliation: the desired-running yard is restored after the first and the desired-stopped yard stays stopped after the second | marked upgrade fixture, two guest reboots, snapshotted/restored host power runtime | targeted transport/recovery diagnostic |
| `legacy-continuation` | VM1 when targeted; VM2 worker in full; frozen installed candidate and two bounded reboots | explicitly synthetic unmarked native project, protected continuation grant and snapshotted/restored host power runtime | targeted grant lifecycle acceptance; required in full; release acceptance runs it as `vm:p0-legacy-continuation` |
| `yard-maintenance` | VM1, frozen candidate and pinned published 0.28.0 → 0.28.1 hop; one container and two bounded VMs, one weighted | Explicit synthetic pending-marker/foreign-policy drift after the supported hop on anchor-created VM yards, upfront restart disclosure and decline of the candidate's current-release retry, approved interruption of a live SSH session, isolated guest preflight refusal, ready warnings, marker-only selective retry and CPU scheduling | targeted migration acceptance; no host reboot |
| `init-settings` | VM1, frozen candidate; one container yard | `config set` to plain `init` without `migrate`, config apply/status and a configured guest file hash, ordinary commands before it, failed-hook retry and same-UID/GID `DEV_USER` rename with a native cut, stopped-yard recovery and a second rename | targeted; run for init-owned settings, release readiness or developer identity changes |
| `release-smoke` | VM1, shared published anchor, candidate package and one reboot | marked yard/project plus retained operator and guest data | singleton retained-data/reboot evidence; also required in smoke and full |
| `peer` | both VMs and synthetic keys | marked cross-owner RPC, project and credential fixtures | smoke and full both cover fresh init/projects/RPC, offline projects and credentials |
| `peer-cleanup`, `cleanup` | current disposable allocation | exact marked fixtures and run worktrees | standalone idempotent cleanup/verifier |
| `--lane full` | all prerequisites above | union of the marked scopes | periodic manual and risk-selected exhaustive matrix; includes release smoke |

The installed-owner release contract also creates a new named yard without a profile. It
checks protected empty selections, a distinct SSH port, retained `/srv` data through a
controlled init failure and ordinary retry, real SSH access, and unchanged neighboring yards.

Pre-anchor migration and original-owner recovery acceptance is frozen in the v0.28.0 tag.
Current Go contracts retain journal, checkpoint and projection reader coverage.
The yard-maintenance lane creates yards with v0.28.0, whose init completes disk-/tmp
policy. After the supported v0.28.1 hop, it explicitly reopens the two fixture VM markers and replaces their owned masks
with foreign policy to exercise pending-policy restart, refusal and retry on supported sources.
This drift is synthetic and does not claim historical producer coverage.
The newer-release fixture shares marker-owned runtime and cleanup helpers. It binds valid
power coordination state before preparing its isolated history; busy or unsafe coordination
state, or a changed bound generation, refuses without deleting barriers. Producer cleanup
prepares the same operator's systemd user manager when absent, without enabling lingering
or restarting an existing manager. Failed histories retain marked state while a power
artifact or producer remains; fallback cleanup never removes global default resources.

For P0 host-sink isolation and cleanup, the existing cleanup fixture has a focused physical mode:

```sh
dev/agent-e2e.sh --slot "$slot" --vm-count 1 --vm 1 --purpose p0-sink-lifecycle -- \
  bash tests/real-host/p0-cleanup-recovery.sh sink-lifecycle 123
```

It uses a real Incus producer and explicit systemd oneshots to verify fixture collection,
removal, and preservation of the common sink's disabled intent, events and incidents. It never
waits for a periodic sink tick. Host-free interruption and fresh-process cleanup checks remain
in `tests/p0-capacity.sh`; this focused mode does not replace a required full P0 run. Marker-owned tree deletion retries
automatically. Unsafe sink artifacts or custody metadata, and metadata-only or missing-service
inventories, return nonzero and retain the marked fixture root for inspection.

The integration selection fixtures use only marker-owned yards on allocated VM1. They exercise
fresh/default selection, legacy selection adoption with established inventory, evidence-backed
retirement, release of changed owned fields through init (after a refused migrate and directly),
desired-retained failure and retry, and stopped-yard rejection without model API calls:

```sh
dev/agent-e2e.sh --slot "$slot" --purpose integration-selection --vm 1 -- \
  bash dev/e2e/integration-selection.sh container
```

The `cold` mode first removes Incus from an empty disposable baseline, then runs the container
lifecycle through product installation. Container checks also stop the installed daemon and verify
that init refuses to replace an existing yard's inherited integration intent before restarting it.
The `local-first` mode connects an isolated Git source to an initialized container, enables an
integration locally, and verifies that source pull/sync preserve the local selection and explicit
empty override. Other modes are `vm`, `default`, `special` (fresh test-vms role), and `upgrade` (owned
ordinary-yard artifacts retired when adopting the test-vms role). The `default` mode installs
all four fresh-default integrations and needs their normal package download access. Test config,
physical project and instance names, and teardown are isolated from the retained host baseline.

The retained-lease disabled-components controller installs the published v0.28.0 baseline, updates
to the frozen candidate and reboots once. Its default payload belongs to the Hermes profile.

```sh
dev/e2e/disabled-components-release.sh --slot "$slot"
```

The published-baseline matrix reads immutable pins and an `anchor`, `hop`, `direct`, `refusal` or
fixture-only `pin` role from `dev/e2e/published-upgrade-baselines.tsv`. Direct upgrade starts at v0.28.0
with the [catalog's selected agents](testing.md#coverage-ownership). The v0.28.1 hop has no release
check of its own. Upgrade chains pass through it, and Periodic E2E runs it agent-free.
That hop lane also seeds explicitly synthetic retained schema-1 Git settings, verifies
that candidate update preserves them, then migrates them with `config sync` without
changing the running yard identity, power intent or retained data. A later
previous release distinct from both remains a direct baseline. When the baseline ships the
candidate's exact registry bytes, the lane also rolls back to it, checks Ready and unchanged
migration history, and updates to the candidate again. An owning profile lane can pass a
`config/profiles/PROFILE/tests/e2e/*.sh` scenario as the third argument; the lane sources it on
the candidate before the rollback, and requires the rollback target.
Host-free `dev/verify-release-upgrades.py` checks the actual v0.28.0 caller's read-only
inspection and update to the candidate, including completion and retained settings/data.
The second argument selects agents for direct upgrade rows. No active lane installs
published releases before v0.28.0.

```sh
SUBYARD_E2E_CANDIDATE_BUNDLE=/absolute/path/to/candidate-bundle.tar.gz \
SUBYARD_E2E_CANDIDATE_SHA256=<bundle-sha256> \
  bash dev/e2e/release-vm-check.sh published-upgrade/anchor --slot "$slot"
```

The remote fixture runs a temporary loopback OpenSSH server with a synthetic key forced to the
selected owner's RPC endpoint. It uses independent controller settings and exercises real framed
plan/execute, digest tampering, replay, public remote status/enable/disable and stopped refusal:

```sh
dev/agent-e2e.sh --slot "$slot" --purpose integration-remote --vm 1 -- \
  bash dev/e2e/integration-remote.sh
```

These are targeted lifecycle checks. See the skill for publication and broader coverage criteria.

To check disk exhaustion within one standard pair while other agents use their own slots:

```sh
bash dev/e2e/slot-disk-isolation.sh "$slot" .build/slot-disk-isolation-RUN
```

This uses one lease with two 4 GiB / 20 GiB guests. It fills VM1's root filesystem to
`ENOSPC`, verifies that VM2 remains writable, reclaims the fill file, then releases the
allocation and checks that its working reservation is gone. The output directory must be
new. The check uses the runner's ordinary admission and never changes resource limits.
`vm:staging-reliability` owns installed broker recovery coverage.

For the broker's physical storage adapter contract, use VM1 of one standard pair:

```sh
dev/agent-e2e.sh --slot "$slot" --type subyard-pair --purpose broker-storage-contract --vm 1 -- \
  bash dev/e2e/broker-storage-contract.sh
```

The launcher compiles as `dev`, then uses guest-local passwordless sudo for the opt-in test.
The test requires root and the runner's guest context. It creates a uniquely owned
`dir` pool and tiny empty VM images, with at most one 512 MiB firmware VM running inside the
allocated guest. It exercises production deletion guards, root/snapshot removal, pinned image
retention and pruning, and cleanup from a persisted builder record. It downloads no guest OS.
It does not run a complete nested broker or establish guest OS/agent readiness; use host-free
broker tests and the ordinary pair lifecycle checks for those separate contracts.

To check clean pair reuse with two sequential leases in one available slot:

```sh
python3 dev/e2e/environment-lifecycle.py --slot "$slot" --pair-only \
  --output-dir .build/pair-reuse-RUN
```

This mode requests only `subyard-pair`. It verifies cached base reuse, fresh VM1 identities,
absence of the previous lease's marker, resources, dev cache/KVM access, and release cleanup.
Other agents can keep using their slots; their usage may change aggregate storage samples.

To check the mixed environment pool, choose two available slots and run:

```sh
python3 dev/e2e/environment-lifecycle.py --slot "$slot" --peer-slot "$peer_slot" \
  --output-dir .build/environment-lifecycle-RUN
```

This controller holds independent leases through the runner. It checks concurrent pair requests,
mixed pair/Android allocations, warm base reuse, fresh VM1 identities and working state, guest
resource limits, dev cache ownership and KVM access, then verifies that release removes working
reservations. The output directory must be new; it contains bounded runner logs, redacted status
samples and `summary.json`. Add `--require-cold` after installing a changed recipe to require new
base fingerprints for both types. Equal fingerprints and sampled builder state do not establish
an exact build count. Only VM1 is directly measured; VM2 composition/readiness comes from the
broker. See `vm:staging-reliability` for broker recovery and the Android profile lanes for
application acceptance.
Use `--disk-isolation` to fill peer VM1's root filesystem to `ENOSPC` while checking that the
other held allocation remains responsive and writable. The writer is bounded by the guest disk
capacity and a five-minute deadline; its unlinked temporary file is reclaimed when closed or when
the process exits. This mode records storage samples before filling, while full, and after cleanup.

Android/GPU, real credentials and external-service profiles use separate explicitly prerequisite-
gated lanes. A generic dependency-free resource pass does not report those handlers green.

### Fixture preflight

For the installed credential worker and its SSH/SOPS connection contract, use the existing owning
profile runner mode:

```sh
bash config/profiles/github/tests/run.sh --preflight
bash config/profiles/github/tests/e2e/acceptance.sh --installed-worker --slot "$slot"
```

The physical controller itself enforces the local preflight before allocation; a separate manual
preflight is useful while repairing but is not an additional acceptance gate. It checks required
Go cases actually passed, executable Python fixture regressions and production filesystem ownership.
It also exercises the base image's actual Incus `--upgrade-only` installer boundary without an
installation identity; package-only upgrade does not publish yard ownership. Normal initialization
continues to require its prepared identity before side effects.
When launched by the release collector after `make verify`, it reuses that existing receipt only
after the collector's normal command/input/environment validation, and prints the reason.
Standalone runs execute fresh prerequisites. A failed or unknown binding refuses reuse.
The pair checks actual installed runtime admission, the real user manager and an observable due-peer
result; local passes do not establish those facts. Other units retain their own physical lanes.

For owner service registration, held-lease adoption, stale routing and container lifecycle, use
one Android disposable stage:

```sh
bash config/profiles/test-vms/tests/e2e/acceptance.sh --slot "$slot" --lane service-container --preflight
SUBYARD_E2E_CANDIDATE_BUNDLE=/absolute/path/to/candidate-bundle.tar.gz \
SUBYARD_E2E_CANDIDATE_SHA256=<bundle-sha256> \
  bash config/profiles/test-vms/tests/e2e/acceptance.sh --slot "$slot" --lane service-container
```

The lane installs the frozen current candidate in a marker-owned fixture, holds an allocation
across registration, verifies container/broker/guest continuity and the shared agent route, rejects
a stale exact service plan, then checks stop/start/reconcile/removal, verifies that an unconfirmed
fresh install creates no configuration or runtime, and installs/removes the declared service again.
For a pre-registration allocation owner, set
`SUBYARD_E2E_ROLLOUT_YARD=NAME` to pass an explicit outer route. This is rollout transport only
and never registers or updates the operator-owned allocation pool.

For the broker recipe and supported-upgrade boundary, use the test-vms owning controller:

```sh
bash config/profiles/test-vms/tests/e2e/acceptance.sh --slot "$slot" --lane broker-cold-upgrade --preflight
SUBYARD_E2E_CANDIDATE_BUNDLE=/absolute/path/to/candidate-bundle.tar.gz \
SUBYARD_E2E_CANDIDATE_SHA256=<bundle-sha256> \
  bash config/profiles/test-vms/tests/e2e/acceptance.sh --slot "$slot" --lane broker-cold-upgrade
```

The controller runs the installer regression before the archived recipe contract and refuses
missing, skipped or failed required cases before allocation. A validated collector `make verify`
PASS is reusable under the existing binding rules. It selects one Android disposable stage for
the owner, cold builder and singleton, with native memory and backing-storage preflight before
acquire. Its authentic TSV-pinned 0.28.0 owner upgrades through its supported updater to the
frozen candidate and retains broker state. No refresh warms the candidate base: the first acquire
runs the real recipe, validates a new base, boots a singleton, checks SSH and a second boot, then
releases and proves available without pending work. Candidate engine/recipe, base fingerprint and
native terminal/cleanup evidence are printed by the shared staging fixture. This changes only the
leased stage; the operator-owned allocation broker stays independent. Default profile acceptance
runs this lane and full-uninstall. Release collection runs these VM checks only when explicitly
requested; synthetic host-free migration/rollback gates remain part of the default release checks.

For GitHub/native-helper release activation, the owning singleton controller requires the same
frozen public bundle used by supported candidate transport. Include its normal
`.subyard-acceptance` metadata/assets; the lane derives the candidate version from that
checksum-verified package and checks the engine identity. For a current candidate newer than
0.28.0, use the authentic published predecessor from its direct TSV pin:

```sh
bash config/profiles/github/tests/e2e/release-activation.sh --slot "$slot" --baseline 0.28.0 --preflight
SUBYARD_E2E_CANDIDATE_BUNDLE=/absolute/path/to/candidate-bundle.tar.gz \
SUBYARD_E2E_CANDIDATE_SHA256=<bundle-sha256> \
  bash config/profiles/github/tests/e2e/release-activation.sh --slot "$slot" --baseline 0.28.0
```

`SUBYARD_E2E_ROLLOUT_YARD=NAME` passes the same explicit outer route as the test-vms lanes.

The controller packages its explicitly synthetic compatible successor (the next patch version
with an `activation-fixture` suffix) before lease acquisition; the original candidate's verified
assets remain intact. The guest uses the actual TSV-pinned 0.28.0 installer/updater, verified
against immutable published release metadata, two container yards and a config-only absent yard.
Normal invocation runs the same cheap local checks as `--preflight` before packaging/allocation;
`--preflight` exits before allocation. A valid local shape alone does not authenticate an archive:
normal execution still checks its actual bytes, architecture, engine version and pinned installer.
The default `--baseline 0.28.0` checks the current supported source. The synthetic successor tests
interruption after broker delivery with a distinct fixture broker binary. A transition may
download unchanged native tool pins once to establish trustworthy activation receipts. The lane
has a 60-minute work budget and bounded marker-owned cleanup; its terminal output retains native
phase logs and reports cleanup separately. It does not use external GitHub accounts or manage the
outer yard's lifecycle.

Use the skill's [check order](../.agents/skills/subyard-dev-flow/SKILL.md#choose-checks-by-risk)
before an expensive affected lane. Inspect the fixture's changed assumptions at its owning boundary:
required commands must be usable by the execution user, including the effective service user or
Incus group when applicable. Probe the actual CLI/API operation the workload needs; a version
string or successful unrelated operation does not establish that contract.

Reuse product readiness and the fixture's existing preflight. P0 already runs capacity and
dependency verification for every allocated guest; the broker verifies VM-agent/tool/transport
readiness before granting a lease. Nested instances still need their own agent readiness.
Add only a missing predicate needed by the affected fixture, before its expensive setup or workload.
Reconcile the installed baseline through its product owner when necessary.

Read-only broker status is planning evidence; atomic admission remains authoritative. Check nested
workload requirements against the actual allocation and backing storage before creating instances
or large fixture files. Keep readiness waits within a finite operation deadline with boot/service
startup margin, and stop on terminal errors. Preserve the original failure and bounded diagnostics
with the full log; polling must not reset the deadline or trigger a blind full retry.

Orca has three real-host fixtures. Run each on VM1 of an explicitly selected available slot:

```sh
dev/agent-e2e.sh --slot "$slot" --purpose orca-bootstrap --vm 1 -- \
  env SUBYARD_E2E_ORCA_BOOTSTRAP=1 bash config/profiles/orca/tests/e2e/orca-bootstrap.sh
dev/agent-e2e.sh --slot "$slot" --purpose orca-existing-yard --vm 1 -- \
  env SUBYARD_E2E_ORCA_BOOTSTRAP=1 SUBYARD_E2E_ORCA_EXISTING_YARD=1 \
  bash config/profiles/orca/tests/e2e/orca-bootstrap.sh
dev/agent-e2e.sh --slot "$slot" --purpose orca-resource --vm 1 -- \
  env SUBYARD_E2E_ORCA_RESOURCE=1 bash config/profiles/orca/tests/e2e/orca-resource.sh
dev/agent-e2e.sh --slot "$slot" --purpose orca-projects --vm 1 -- \
  env SUBYARD_E2E_ORCA_PROJECTS=1 bash config/profiles/orca/tests/e2e/orca-projects.sh
```

### Extended/manual Orca load diagnostic

This diagnostic is excluded from ordinary tests, profile acceptance and release
acceptance. Select an available slot and explicitly generate 1,000 or 1,400 roots
to exercise Subyard's admission limit:

```sh
bash config/profiles/orca/tests/e2e/load-diagnostic.sh --slot "$slot" --roots 1000
# Optional larger rejected workload, using a new disposable lease:
bash config/profiles/orca/tests/e2e/load-diagnostic.sh --slot "$slot" --roots 1400
# Independent cleanup/interruption check with fresh setup:
bash config/profiles/orca/tests/e2e/load-diagnostic.sh --slot "$slot" --roots 1400 --lane cleanup
```

The controller requests one standard VM with purpose `orca-load-diagnostic`. The
payload has a 45-minute deadline and a 30-second TERM-to-KILL grace period; allocation
and source transport precede that payload budget. The guest requires a matching lease,
run, slot and VM identity before mutation, so environment opt-in alone cannot enable
local execution. All mass Git roots and 100,001 flat files are generated inside the
leased disposable VM's nested test yard, outside the live controller checkout.
Marker-owned fixture cleanup runs on normal completion, failure and interruption;
the runner also releases and deletes the disposable allocation.
Mass pruning may need several existing 85-second registration cycles. Only an
exact budget-exhaustion result with fewer fixture records and preserved baseline
repository/tab IDs permits another batch, with at most six batches and a ten-minute
retry window. Existing per-invocation deadlines still apply.
RPC failures and pruning without progress fail the diagnostic. The cleanup lane
prepares a fresh fixture; it cannot resume a released lease or replace load measurements.

The workload uses stock Orca. Repository admission waits use only the repository
catalog and saved-tab inventory, and require at most 1,000 total records while
preserving baseline identities. Only the remaining capacity is admitted from the
generated roots and seed; the seed does not have to be among the accepted records.
The loaded fixture can therefore contain more Git roots than registered projects.
The load lane measures stock behavior, restarts the service and polls for 65 seconds.
It never switches or patches runtime bytes, and does not invoke sync while deliberately
rejected fixture roots remain on disk.

Cold measurements check their first RPC round immediately, report startup CPU
separately, allow a fixed 25-second settlement interval, then measure ten seconds
of steady idle and another RPC round. Loaded CPU, latency, worktree RPC failures
and omissions are numeric diagnostics, not promises of native runtime capacity.
Repository/tab read failures, lost baseline IDs, exceeded admission counts and
incomplete cleanup fail the diagnostic. Baseline and cleaned phases retain strict
RPC/completeness checks. Safe cleanup is checked immediately, after another timer
cycle and after controlled interruption.
The manual workload enables native main-thread diagnostics in its disposable service
and emits bounded numeric event-loop and sampled Git trace aggregates per phase,
including failed phases. Git durations include queue wait; these samples do not expose
exact child execution time, filesystem probe counts or queue depth. RPC failures retain
only allowlisted reasons. No raw journal, commands or trace attributes are published.
Do not run this workload through ordinary acceptance or recreate its fixtures locally.

Bootstrap installs a packaged candidate and exercises public commands through a real terminal.
For the narrow Codex configuration regression, run:

```sh
dev/agent-e2e.sh --slot "$slot" --purpose orca-codex-config --vm 1 -- \
  env SUBYARD_E2E_ORCA_BOOTSTRAP=1 SUBYARD_E2E_ORCA_CODEX_CONFIG=1 \
  bash config/profiles/orca/tests/e2e/orca-bootstrap.sh
```

This mode seeds representative TOML runtime additions, verifies that they do not block readiness,
repairs a deliberately changed managed policy through `config apply`, then verifies Orca restart,
saved-client connectivity and `migrate --check`. It skips the broader JSON import/sync and down/up
scenarios. The seeded additions model the observed drift; they do not prove which desktop action
writes each runtime field.

For Codex permissions across terminal and Orca launches:

```sh
dev/agent-e2e.sh --slot "$slot" --vm-count 1 --purpose codex-permissions --vm 1 -- \
  env SUBYARD_E2E_ORCA_BOOTSTRAP=1 SUBYARD_E2E_ORCA_CODEX_PERMISSIONS=1 \
  bash config/profiles/orca/tests/e2e/orca-bootstrap.sh
```

This focused mode installs the native Codex CLI and stock Orca in a disposable yard.
It verifies managed policy drift refusal, authorized release repair and subsequent
`yard init` readiness, repairs a legacy Codex login environment and missing chat launcher
without restarting Orca, and exercises runtime approval
with synthetic model responses and disposable Git repositories. Stock Orca native chat
starts and resumes a real Codex thread and writes outside the workspace to verify the
yard sandbox default. It uses no real agent credentials
or external model APIs. These checks do not replace interactive VS Code/Desktop acceptance or
establish a security boundary against alternative Git commands.

For managed Claude and Codex binaries and Claude Git prompts under stock Orca:

```sh
bash config/profiles/orca/tests/e2e/acceptance.sh --slot "$slot" --lane managed-agents
```

This focused mode selects both agents in a fresh installed yard. It checks by process image that login
shells and the Orca Codex launcher reach the managed binaries, and that launches under the Orca server's
`XDG_DATA_HOME` install nothing. As a positive control it removes the Claude update lock to obtain Claude's
real native link, seeds the Codex standalone link shape, and converges both through `yard migrate` and through
`yard init` while provisioning is current, keeping version files, unknown files, running legacy sessions of both
agents and the Orca server. Links outside the proven shape stay through `yard init`. The Codex package tree must be
root-owned and closed to group and other writes. From a login shell on the fresh yard it asserts by process image that
a plain Codex client runs its turns on a daemon copy in its home and finds the package helper, that `--no-daemon` stays
on the `/opt` release, that a Git prompt holds in both, and that a tool keeps running on the daemon after the client is
killed and stops with a `--no-daemon` client. The upstream Codex installer and daemon bootstrap then create a real
daemon package, which `yard init` must leave running while plain clients keep using it and the Git prompt holds.
The lane then deletes Orca's hooks, restarts the service
and requires the reinstalled hooks and a terminal launch to keep the managed Claude settings before any fixture
snapshot, and reads settings drift before repairing it, including the drift from accepting Claude's auto mode offer. Finally it launches Codex from a paired Orca terminal and
native chat, and drives Git prompts with a synthetic loopback model in stock Orca native Claude chat and in a
paired Orca terminal running `claude --dangerously-skip-permissions`. Its saved-chat stage saves Claude chats
under the stock Full access flag, removes the reset state to look like an installation from before the reset,
and requires `yard orca sync` to reset both chats, record the running Orca as dirty and warn until a restart. It
then drops the reset record, as a lost response, failed state write or crash would, and requires status to keep
warning, `yard orca up` to restart Orca once with a new runtime id and no process left with the skip flag, the
reset to complete with mode 0600, and the reopened chat to ask for Git approval. It needs the upstream Claude and
Codex downloads; it uses no agent account.

For a focused recheck of stock native-chat approval/reconnect, the managed relay and
native/paired terminals, use:

```sh
bash config/profiles/orca/tests/e2e/acceptance.sh --slot "$slot" --lane codex-approvals
```

It uses a fresh installed yard with Codex and the stock client, and skips Claude policy delivery and the Codex legacy-launcher/requirements drift
repair loops. The default full acceptance retains those checks. Reconnect here means a fresh
runtime RPC connection that reads and answers the same pending card; it does not certify the
graphical Desktop UI or transport-loss subscription recovery.

For temporary SSH-key access, run the focused current-candidate fixture:

```sh
dev/agent-e2e.sh --slot "$slot" --purpose orca-ssh-agent --vm 1 -- \
  env SUBYARD_E2E_ORCA_BOOTSTRAP=1 SUBYARD_E2E_ORCA_SSH_AGENT=1 \
  bash config/profiles/orca/tests/e2e/orca-bootstrap.sh
```

It initializes two isolated yards, starts stock Orca and uses a synthetic encrypted key with a
real passphrase terminal. It checks Git pushes from a guest shell and an existing paired Orca
terminal, wrong-passphrase rejection, guest agent-mutation denial, cross-yard isolation and
new-authentication failure after revocation and expiry. This mode skips the packaging, upgrade,
configuration and port-collision scenarios; it does not install coding-agent CLIs or call model APIs.
The focused SSH and Codex configuration modes reuse the pinned Orca package cache after SHA-256
verification when available. They verify runtime integration, not a fresh Orca package download.

For SSH fixture debugging, capture evidence before the lease ends. A fixture retained with
`SUBYARD_E2E_ORCA_KEEP_FAILED=1` is inspectable only within the same active lease. A new invocation
gets clean VMs, so `SUBYARD_E2E_ORCA_RESUME` cannot continue a previous lease. Record the failure,
evidence and remaining checks in the current task plan and rerun the independent segment.

Live account/service checks follow the [external-check contract](reference/release-publication.md#external-checks).
Required synthetic VM fixtures, including OpenClaw staging safety/lifecycle, establish their own
boundaries; they do not prove real service behavior.

The `stuck-recovery` lane (`profile:orca/stuck-recovery`) is periodic-only: it runs in Periodic
E2E and never gates a release. It starts from the published anchor with
Orca running, then gives a project a saved tab that survives an Orca restart, an agent-session
tab and a live terminal, plus a neighbour project with an agent-session tab and a live terminal,
and removes the first project. Updating to published 0.29.0 must stop in `recovery-ambiguous`,
and a plain `yard update` must not complete it. The lane records whether the newer-release
hint appeared, since the newest published release is outside its control. The candidate,
packaged as `0.29.1-orca-recovery-e2e` or the acceptance candidate's version and required to sort
above 0.29.0, then completes through `yard update --version` with no manual step. The lane
checks release readiness, the candidate's installed helper and the unchanged predecessor
Orca package. It checks that the removed record, its tabs and its live terminal are gone,
that the neighbour's agent-session tab and live terminal survive, that config and Orca status
are clean and that a steady hook pass succeeds. It prints one `orca-stuck-recovery-evidence`
line.

For a narrow predecessor upgrade check, set `SUBYARD_E2E_ORCA_UPGRADE_FROM` to an exact published
version and `SUBYARD_E2E_ORCA_UPGRADE_INSTALLER_SHA256` to that release's installer asset digest.
Use the `0.28.0` direct TSV pin for supported predecessor checks. Handler acceptance defaults
to that pin and supports only the direct path; configured pre-anchor sources receive the
product's update refusal.
This mode starts Orca on a converged published release with two local yards, changes the candidate's
managed Claude defaults, then runs the public update command. It checks all-local config convergence, release
readiness, lingering enabled without an Orca restart, Orca restart, a service restart on the next `up`,
the saved client grant, endpoint and runtime JSON before cleanup.
If the update command fails, the fixture reports its marked temporary state path and retains the
test yards for diagnosis on the disposable VM. Clean up those exact yards and marked state after
the investigation.
The existing-yard variant completes release activation with Claude and Pi selected before starting
Orca, then checks release convergence and materialized settings. The resource fixture verifies
paired stock clients, terminal input/output, service lifecycle, exact owner/loopback routes and a
paired desktop under Xvfb. It also opens a real guest SSH login, verifies the daemon
and a live native PTY in their scopes under the development user's manager, closes all
that user's logins, and waits beyond logind's user-stop delay. It checks unchanged daemon,
PTY and user-manager processes, terminal I/O across logout and service restart, verified
scope shutdown through `down`, retained lingering and subsequent `up` convergence. New
terminals must keep the daemon's environment across `restart` and pick up a service
environment change after `down` and `up`. A yard reboot must place the daemon in its scope.
`status` must report the daemon in its scope, a process in the service's cgroup, disabled
lingering, and its repair through `up`.
A bounded loopback DevTools driver uses the installed client's preload
API, reloads the renderer, verifies the remote project in its sidebar and requests normal closure.
Pairing capabilities stay in protected temporary files. Owner-address discovery is synthetic;
the fixture does not verify a real Tailscale account. Projects covers production local and SSH-remote
project actions, checkout discovery and preservation of identities and terminal tabs.

Standalone `reboot-verify` prepares its own power reconciler through the same marked
published-release upgrade fixture used by `power-systemd`. It works after the last test yard
was torn down, when the power service may be absent. This adds release installation and migration
to the two reboots; the fixture restores the previous host runtime afterward. It does not run the
additional systemd parser fixtures from `power-systemd`. The exact published v0.28.0,
the inclusive direct-update anchor, remains the published upgrade baseline.
Its verified update registers the local yard through the release journal, so repeated plain
init and both reboots must retain that ownership without a legacy continuation grant. The
first-release chains use staged candidate tooling to refuse their retained pre-anchor rollback
without changing links or protected state. Published releases sit below the candidate's
`power-metadata` epoch, so rollback to the anchor must be refused with `code=rollback-incompatible`
without changing links, checkpoint/history, ledger, native identity or retained V1 evidence.
The retained-release registry mismatch and rollback-compatibility blocker each have their own
evidence line. If the source provenance check refuses first, the fixture records `LIMIT`.
Host-free tests also cover the rollback-compatibility blocker. The release smoke and NetworkManager
guard lanes recognize these refusal paths. Release smoke compares runtime link metadata and the
V2 state tree's metadata and file hashes across the refusal. The same-epoch cycle uses a compatible
release built from the candidate source. A public `update --check --json` names its exact target
with a plan and no blockers. The lane updates to it, returns to the candidate, performs a real
rollback to it and rolls forward again. A last cycle updates to a release built from the candidate
source whose registry retires the definition-free `project-state` prefix and whose declaration
names the candidate's registry; `tests/helpers/registry-fixture` derives both with the engine's
registry code. The release's `_history-compatibility` verdict for the candidate must be an admitted
`equivalent`, a real rollback returns to the candidate, the candidate's verdict must admit the
retired release as its declared `successor`, and the lane rolls forward and back once more.
Both passages preserve native policy and the registered ownership. Fresh sources below v0.11.0
remain refused; accepted completed V2 history and recovery retain their separate supported
contracts. The guest verifies checksum-pinned official anchor assets before creating the cold
legacy baseline.

List or run one lane:

```sh
dev/e2e/p0-acceptance.sh --list-lanes
dev/e2e/p0-acceptance.sh --slot "$slot"
dev/e2e/p0-acceptance.sh --slot "$slot" --lane full
dev/e2e/p0-acceptance.sh --slot "$slot" --lane peer
SUBYARD_P0_WAIT_SECONDS=1200 \
  dev/e2e/p0-acceptance.sh --slot "$slot" --lane power-systemd
```

`--lane full` installs the frozen candidate in its fixtures and refuses before allocation unless
`SUBYARD_E2E_CANDIDATE_BUNDLE` and `SUBYARD_E2E_CANDIDATE_SHA256` name one prepared by
`python3 dev/release-acceptance.py prepare`; release acceptance passes them itself.
Release acceptance runs `vm:p0-full` as `--lane full --smoke-covered`: its required
`p0-release-smoke` runs on the same candidate bundle, so the full check reports newer-release,
release smoke and peer as covered instead of repeating them. It also adds `--split-covered`:
power-systemd and legacy-continuation run as the separate
`vm:p0-power-systemd` and `vm:p0-legacy-continuation` checks of the same candidate (hosted in
release preparation), so VM2 runs only nested teardown. A standalone `--lane full` runs them all.

After capacity preflight, boundary and transport pass, `--lane full` does not stop at the first failed
phase. The parallel owner and auxiliary chain both finish, auxiliary stages continue after a failure,
and newer-release, release smoke, peer, cleanup and final verification still run. After each failure
the controller removes every owned fixture on that phase's VMs and verifies capacity cleanup; a VM
that is not verified clean blocks later phases on it, and a failed `fixture-platform` blocks its
dependent auxiliary stages. Evidence records every phase as passed, failed or blocked. The run ends
with one `full P0 failed=... blocked=...` line in phase order and exits with the first failure status,
or 3 when cleanup failed.

`SUBYARD_P0_WAIT_SECONDS` is a non-negative number of seconds passed to the atomic broker acquire;
zero keeps the fail-fast default. The `power-systemd` parser project is fully ephemeral: the lane
removes its marker-owned project and restores the snapshotted host unit/runtime before the phase is
reported. The fixed `subyard-e2e-*` Ubuntu image alias is retained in the disposable allocation's
default Incus project, just like the real-Incus base-image aliases, so a later launch does not include
an unbounded remote transfer. Outer allocation teardown removes the alias with the Incus pool.
The image alias disappears with the disposable disk at release.

The cache fill and local launch both emit progress. Their independent positive-integer overrides are
`SUBYARD_SYSTEMD255_IMAGE_TIMEOUT_SECONDS` and
`SUBYARD_SYSTEMD255_LAUNCH_TIMEOUT_SECONDS`; restart uses
`SUBYARD_SYSTEMD255_RESTART_TIMEOUT_SECONDS`. A timed-out cache fill or launch fails once with its
operation and limit. The fixture never starts a second remote pull or launch after a timeout.

The full matrix keeps the historical owner, real-Incus contract, release and broker-migration chain on VM1.
VM2 independently runs nested teardown, container-image preparation, power-systemd and legacy-continuation in that order;
with `--split-covered` it runs nested teardown only.
Independent power-systemd, legacy-continuation, reboot-verify and release-smoke lanes reconcile Incus and cache
their container image without repeating the real-Incus contract's container/VM launches and Go tests.
The owner fixture packages its candidate once and reuses those exact assets across its independent
predecessor installations, active/inactive broker checks and rollback scenarios.
The two chains join before the newer-release replacement lane on VM1 and the shared release-smoke phase and full peer checks, followed by cleanup
and final boundary verification. Host-free `./tests/run.sh`, prepared loopback SSH/crypto contracts
and the owner engine-release contract run in their own required gates and are not repeated here.
The parallel matrix passes only after both chains pass. It has a
210-minute kernel-monotonic work deadline by default (`SUBYARD_P0_FULL_MATRIX_TIMEOUT_SECONDS`).
The controller reads `/proc/uptime`, so host wall-clock corrections cannot expire the matrix or its
shutdown grace periods early. On expiry, runner children get a bounded 30-second TERM grace and
10-second KILL grace before evidence and marker-guarded cleanup continue, leaving the remaining
broker lease time for final checks.

Each phase prints its bundle hash and duration. The runner keeps one bounded, redacted JSON evidence
record per public run under its private controller state, including each phase's result, source hash,
base fingerprint and allocation generation. It prints the evidence path; the latest 20 records are
retained. Copy records still needed by an active task into its own `.build` output before that
retention window expires.
Evidence never contains lease credentials, guest endpoints, command payloads or ambient environment.
Use [evidence handoff and cache cleanup](testing.md#evidence-handoff-and-cache-cleanup)
to retain concise key findings and remove completed task-owned artifacts. Logs remain temporary;
permanent artifact storage is not required.

The current task plan is the durable checklist: record passed, failed and pending segments, the
exact tested source and baseline identities, and evidence paths. Reassess prior passes after relevant
source or baseline changes. Do not infer test progress from VM files or a previous lease's slot.
`--resume` and `--keep-failed` are rejected by P0 with these instructions. Invoke only remaining
independent `--lane` segments; each prepares its prerequisites on fresh VMs. Reboot continuation
within one active lease still works. A required full P0 always runs fresh within a single lease;
a collection of targeted passes does not replace that gate. Release acceptance's full P0 is a fresh
`vm:p0-full` plus its split lane checks of the same frozen candidate; `--split-covered` refuses outside
the release collector. No Markdown parser executes the plan.

Open an unrestricted root guest session or run a root command:

```sh
dev/agent-e2e.sh --slot "$slot" --ssh 1
dev/agent-e2e.sh --slot "$slot" --ssh 2 -- id -u
```

The wrapper creates an ephemeral Ed25519 key per lease, starts a keeper that renews every five minutes
(300 seconds), and releases in its exit trap. Twenty minutes (1200 seconds) after the last successful
renewal expires a held lease; before its first renewal, that deadline starts when the grant becomes
held. Provisioning, boot and download deadlines are separate. More frequent renewals from older
clients remain valid, and changing the installed timing does not rewrite existing lease deadlines.

The keeper checks the wrapper and its original launching process once per second, including while
a renewal transport is blocked. It verifies process start times and parent identities, so PID reuse,
zombies and an orphaned detached wrapper cannot keep renewing. The launching process is the local
requester/session boundary; a live child alone does not establish that its requester is still active.
An interactive shell that intentionally keeps running remains a live requester. After wrapper or
requester death, the keeper stops and closes its timer and renewal transport; abrupt wrapper death
leaves the broker to expire and fence the lease through its ordinary reaper. For a
held slot, immediate busy, wait progress and timeout diagnostics use the bounded form
`owner=YARD/PROJECT run=RUN purpose=PURPOSE acquired=TIME expires=TIME label=LABEL`. They never include
a controller identity, lease credential, endpoint, host key or private path. The raw OpenSSH config
and lease capability are internal temporary files and are not an agent API.

Every wrapper invocation is a new lease for only the requested slot. For a two-VM `subyard-pair`,
`e2e-vm-1` and `e2e-vm-2` select its two guests; any singleton has only `e2e-vm-1`. These are
guests within the allocation, never global slot names. Stateful multi-step work must stay in one
script invocation or one interactive SSH session, or use a
[client handle](../config/profiles/test-vms-client/skills/subyard-test-vms/SKILL.md) across separate commands. Once leased, the agent has unrestricted root
in its allocated guests and may create arbitrary nested yards,
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
`SUBYARD_E2E_PURPOSE`, `SUBYARD_E2E_SLOT`, `SUBYARD_E2E_VM`, `SUBYARD_E2E_TYPE`,
`SUBYARD_E2E_VM_COUNT`, `SUBYARD_E2E_CPU_PER_VM`, `SUBYARD_E2E_MEMORY_PER_VM`,
`SUBYARD_E2E_DISK_PER_VM` and `SUBYARD_E2E_BASE_FINGERPRINT`. Count and resource
values come from the runner's validated actual environment grant, including configured
resource overrides. These exports do not extend the lease marker schema or provide
independent lease authentication.

Base publication requires recipe completion, a guarded stop/start of the builder, and
agent/cloud-init plus read-only tool readiness on the new boot. Validation includes the
Incus client and modules for the running kernel. The package manifest describes installed
packages; it is distinct from the fresh-boot validation marker. After validation, the builder
stops the Incus service/socket and removes regenerated daemon state before clearing machine
identities and publishing. A validation failure preserves the previous base and held pins.
The validation revision participates in recipe identity, so older cached bases cannot bypass
this gate. Validation is one attempt within existing deadlines; it does not promise to prevent
an intermittent kernel or agent stall.

## Lifecycle and fencing

Acquire atomically reserves an `available` slot as `provisioning` with the requested type and
resource budget. The broker creates one or two fresh guests from the selected validated base.
Only after every guest is ready does it install the ephemeral key, open forwarding through the
slot's dedicated data account and publish `held`.

Release, heartbeat expiry, operator drain or outer stop:

1. removes the data-account forwarding key and kills that account's sessions;
2. removes guest lease keys when agents are reachable;
3. verifies marker ownership, stops and deletes the disposable VM disks;
4. publishes `available` only after cleanup is verified.

Both standard and Android VM allocations use this expiry policy. The server reaper cadence is
independent of the five-minute renewal cadence: cleanup begins on its next ordinary pass after
expiry, without another five-minute delay. Shared bases and caches survive disposable allocation
cleanup; cleanup failures keep the slot unavailable through quarantine and recovery.

A rebooting guest does not prevent fencing at the data route. No subsequent lease receives a prior
lease's disk or key. Generation, epoch, lease ID and server-side capability verification fence old
credentials after release and reuse.

Provisioning or cleanup failure quarantines only the affected slot. Before destructive recovery,
the broker fsyncs a local incident and verifies project/VM ownership, refusing foreign or ambiguous
resources. Recovery cleans the disposable allocation and returns an empty slot to the pool; the
next lease provisions its requested environment from a validated base. Failure to persist the
incident or prove ownership leaves the slot quarantined without deletion. Ordinary resource
shortage is a retryable admission refusal and does not quarantine a healthy empty slot.

Guest lease-key and context cleanup shares a five-second budget per running VM before stop.
Quarantine key cleanup has one five-second phase budget. A cleanup timeout retains its
diagnostic evidence and leaves the parent context available for native stop and recovery;
cancellation of the parent still prevents force stop.

If a disposable guest remains `RUNNING` after recovery's 60-second graceful stop, recovery saves
a fresh durable incident and force-stop event before refreshing the exact project, generation and
lease-epoch markers. It then attempts a native force stop with a 30-second command budget and
verifies `STOPPED` within five seconds before deletion. Cancellation, unavailable state, changed
ownership or evidence-write failure prevents escalation. Initial release still uses graceful stop;
this fallback belongs only to quarantined disposable recovery, never retained legacy guests or
base-image builders. Incus or kernel failures can still delay recovery through its normal retry schedule.

The root reaper starts recovery immediately after the incident is durable. Failed rebuilds retry
after 1, 5 and 15 minutes, then hourly without an attempt limit. A temporary Incus, image, network
or capacity failure delays recovery; it does not turn quarantine into a permanent terminal state.
The manual command starts the same slot recovery workflow immediately:

```sh
yard test-vms recover --slot "$slot"
```

Loop protection holds a slot that keeps failing. A release or acquire quarantine counts; failed or
interrupted recoveries do not. The fifth counted quarantine within a sliding hour starts a hold:
recovery still cleans the slot at once, but the slot stays `quarantined` and unreserved until
`hold_until`, then the reaper recovers it again and publishes it. The first hold lasts 2 hours and
each consecutive hold doubles, up to 24 hours. The level returns to zero after 24 hours without a
counted quarantine or hold. `yard test-vms recover --slot "$slot"` lifts a hold at once and keeps
its level, so the next hold is longer. Status shows `hold_level`, `hold_started_at`, `hold_until`
and `hold_reset_at`; the journal marks the quarantine that started a hold with `hold_until` and
records `slot.hold_ended` with `hold_reason` `expired` or `manual`. A clock moved back never makes
a hold longer than 24 hours from the current time.

## Broker diagnostics

Agents can read their own lease diagnostics through the existing restricted SSH facade:

```sh
dev/agent-e2e.sh --logs --slot 1 -n 50
dev/agent-e2e.sh --logs --json
dev/agent-e2e.sh --incident <incident_id>
```

Any controller can also read the pool error journal, the broker's own failures across all slots:

```sh
dev/agent-e2e.sh --errors [--slot 1] [-n 50] [--json]
```

The broker records a journal entry for a failed or slow (30 seconds or more) release step,
quarantine, failed recovery, planned or failed force stop, a fatal reaper pass and a slow or
timed-out Incus call. Each entry has only typed fields: time, slot, resource generation, lease epoch,
state transition, `step` (`fence-and-stop`, `delete-allocation`, `readiness`, `recovery`),
`outcome` (`error`, `timeout`, `slow`), Incus `operation`, duration, recovery attempt and
`incident_id`. It never carries lease attribution, lease or client IDs, error text or guest output;
`own` marks entries of this controller's leases, whose full text `--logs` and `--incident` return.
An Incus stop, start, delete or restart (and a project or network create or delete) is slow after
30 seconds and other watched calls after 10 seconds; a still-running call is recorded at its
threshold, at most once a minute per operation, and again when it returns. Exec, file transfer,
image and publish calls keep their own phase budgets and are not watched. Journal entries follow
the archive's 30-day retention and are also capped at 10,000 entries. They and the controller's own
incidents remain readable after recovery and after a pool resize retires the slot. `--status` adds
one line with the count and latest entry of the last 24 hours.

When a release, provisioning or acquire meets a quarantine, the client prints the slot, its own run
and the incident from its own journal entry, and the operator command:

```text
E2E_QUARANTINE slot=slot-003 run=c291a4ef incident_id=01795371892254664249-1843ff2f64712754
agent-e2e: slot-003 was quarantined during release of run c291a4ef (incident 01795371892254664249-1843ff2f64712754); the broker recovers it automatically.
agent-e2e: tell the operator and ask them to run: yard test-vms logs --slot 3
```

Another holder's quarantined slot names only the slot and incident. Without a readable journal the
incident is `unavailable`. When the quarantine started a loop-protection hold, the machine line adds
`hold_until=<time>` and the client prints when the slot returns and the operator's lift command,
`yard test-vms recover --slot N`. `--status` prints the same line for every held slot.

These reads never acquire, renew, release or recover a lease. The private random controller
`client-id` is a diagnostic read capability; keep it private along with the controller identity.
After a refused foreground release, the runner and `client.sh release` print
`E2E_RELEASE_REFUSAL code=... message=...` and the same lease epoch's own events as
`E2E_RELEASE_EVENT` lines; the outcome is unchanged. For every drain (release, expiry, revoke,
drain-all or resize), `lease.stop_failed` names the drain step (`fence-and-stop` or
`delete-allocation`), keeps the native error with the elapsed graceful stop time, and both stop
outcome events carry the cleanup `duration_ms`.
The broker stamps each archived record with its captured private controller identity when writing
the record, starting before provisioning. Slot, resource generation and lease epoch remain
diagnostic metadata; reused counters do not transfer access to another controller.
Public yard/project/run labels, controller fingerprints and slot numbers do not authorize reads.
Only leases acquired by this controller after the broker gained `lease-logs-v1` are readable,
including failed acquisitions without a returned grant and released or expired leases. Another
holder's reuse of the same slot does not expose that holder's logs. Losing the private client ID
loses access to its history; reading never creates a replacement identity.

`--logs` returns up to 200 events (50 by default), bounded to 1 MiB of event JSON. Without
`--json` it prints one JSON object per line; `--json` returns an array. `--slot` optionally narrows
the controller's own history. `--incident` returns the referenced redacted diagnostic artifact,
including VM state, QEMU, console and QMP logs when collection verifies the project, allocation
and controller ownership markers on that VM. Unverified VM evidence, shared service journals,
legacy failure logs, unknown diagnostic sections and physical-owner artifacts are excluded from
agent responses. The root-private local archive survives host-sink acknowledgement. Acknowledgement
prunes old records and trims the archive to 512 MiB. Reads reject each record 30 days after creation,
even if acknowledgement is delayed. Reads require the
outer yard and broker to remain reachable. An older broker reports the missing capability and
requires the operator's normal runtime update or `yard init`; agents cannot update the outer yard.

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

Readiness polling passes a child context with the configured boot budget to each native
agent, host-key and SSH/sudo probe. Expired budgets do not start another probe, and late
success is rejected. The parent request remains available for cleanup after a local timeout.
Incident readiness fields retain only phase, timestamps, budget and classified latest-probe
metadata; they exclude probe arguments and output.

Before native collection, quarantine asks each owned running guest, the failed VM first, for
`systemctl list-jobs` and `journalctl -b -n 200` through the guest agent, within one five-second
budget. The incident keeps the redacted latest 64 KiB of each as `vm_N_guest_jobs` and
`vm_N_guest_journal`, or the reason they are unavailable (missing, stopped or unverified guest,
unresponsive agent, exhausted budget). A guest is never started for this, and only the controller
whose allocation markers verify on that VM can read these sections.

Local incident collection has a five-second budget with one-second native probes. It prioritizes
the failed VM state, one guest console log read after lifecycle completion and QMP logs,
without a responsive guest agent. Console output can retain kernel suspend or panic evidence. Project and bounded
service journal observations follow. Empty responses, probe failures and exhausted budgets are recorded as missing measurements;
the existing artifact redaction and size limits apply. QMP sections retain the latest 64 KiB after
redacting the complete log, with an explicit truncation marker; other sections retain their existing
limits. Force-stop events link the fresh incident
and report the graceful command and forced-stop durations when available.
Expanded instance metadata is projected to architecture, base fingerprint, CPU/RAM/root-disk
limits, explicit Secure Boot state and a known policy identifier or unknown-policy digest.
Raw configuration, cloud-init, device sources and QEMU configuration are excluded.
Local parsed CPU, I/O and memory PSI counters describe the broker allocation boundary;
verified physical-owner MemAvailable comes from the existing read-only bind. Missing
measurements are explicit, including unknown effective defaults.

The host sink also saves a bounded observation in `test-vms-broker-incidents/host/<incident_id>.json`:
visible host/allocation cgroup memory limits, current/peak values, OOM counters, parsed physical-host
CPU/I/O/memory PSI, outer instance state,
and classified kernel OOM events from the preceding 15 minutes. Collection has a five-second
budget and records missing measurements explicitly. Its timestamp is the collection time, not a
claim to have measured the failure's peak. Kernel messages and private cgroup paths are not copied.
Retries preserve the first observation; its retention follows the incident. An agent's broker
status cannot substitute for this L0 evidence across the outer allocation boundary.

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
broker. Release removes disposable VM disks; outer-yard teardown also removes base images and
the containing yard storage. An unavailable outer yard produces the stable `test environment unavailable` error
instead of attempting recovery.

A runtime release automatically installs the compatible physical-host sink before updating an
enabled producer. When the outer `test-yard` and broker service are active, it then verifies the
installed sink, broker engine and facade status without revoking held leases. A stopped, disabled
or never-initialized broker is not started as an update side effect; its next explicit `yard init`
performs the ordinary convergence. During the one-time owner migration, a running legacy fixed-VM
backend that predates the broker service is treated as its active predecessor.

A private base builder persists its capture requirement before boot/readiness begins and clears
it only after successful validation is durable. An interrupted build retains its phase even when
later failure-state writes fail; an unavailable readiness outcome is explicit. Before destructive
staging cleanup, the builder records its actual project/name in the existing incident spool.
Its pending capture and immutable incident receipt survive in the image registry; an evidence
or registry-receipt write failure preserves staging for retry.
Manual refresh has no allocation lease and uses the reserved incident identity `slot-000`, with
zero generation/epoch and no lease attribution; it never creates a pool slot. Builder incidents
retain the producing yard source and identify `resource_kind=base_builder` in diagnostics.
Verified builder cleanup emits an exact-incident `base_builder.cleaned` receipt. Host retention
uses that receipt only for the matching source and builder artifact, including builders created
for real allocation slots. Ordinary slot recovery/reuse cannot close builder evidence. The receipt
creates no allocation recovery transition. An unpersisted cleanup receipt leaves the observation open.

The targeted `full-uninstall` P0 lane uses the existing 8 GiB singleton allocation (`android-test`)
for its ordinary Linux fixture and holds one disposable lease across candidate packaging and
supported installation, selected-yard preservation, explicit full removal, fresh-process retry,
a delayed sink tick longer than one minute, guest reboot and repeated verification:

```sh
dev/e2e/p0-acceptance.sh --slot "$slot" --lane full-uninstall
```

Its released checkpoint case installs checksum-pinned v0.30.0 and interrupts full removal
at config deletion. The current candidate resumes that release-written checkpoint with its
retained runtime. The lane also retains current ownership, foreign-control and reboot checks;
it installs no published source older than v0.28.0.

Its marker-owned fixture runs lifecycle commands as an ordinary isolated owner, with root helpers
confined to the leased VM. It includes direct/custom installer-only and bootstrap-only cases,
default/named/stopped/partial ownership, credentials and the real host sink, a connected SSH refusal,
foreign workload/shared-volume/user-service/SSH/shell controls, a bound Git project and an ordinary
foreign file inside the custom host root. Reboot readiness checks transport and a changed boot ID;
it does not require the power reconciler which full removal has retired. Host-free
`tests/full-uninstall.sh` additionally removes Incus from the actual executable path so the normal
P0 Incus baseline cannot mask the cold-install boundary. These fixtures do not establish acceptance
until their native phases and cleanup have completed against the frozen current candidate.
