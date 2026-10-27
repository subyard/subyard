# Agent E2E VM pool

The `test-vms` profile runs a root-owned lease broker inside a trusted outer yard. Its configured
pool contains one or more shared slots and defaults to two. Each lease selects `subyard-pair`
(one or two standard VMs) or `android-test` (one larger VM). Slots own isolated inner Incus projects and networks;
each acquire creates fresh VM disks from a versioned immutable base. Release fences access and
deletes the disposable disks. No guest state survives a lease as a supported continuation mechanism.

The outer yard remains operator-owned. Agents can acquire inner slots, but cannot start a stopped
outer yard, enter its shell, reach its Incus socket or invoke arbitrary lifecycle commands.

## Running tests

Choose checks using [Subyard dev-flow](../.agents/skills/subyard-dev-flow/SKILL.md#choose-checks-by-risk).
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

When a selected check needs real GNU/Linux hosts, use an allocated `test-vms` slot.
The operator owns outer-yard start, stop and teardown; the
root broker owns inner slot create, start and stop; agents acquire leases only.

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
controller loss and profile status rendering. It also builds and smoke-tests the
native amd64 Paseo artifact. Run its local prerequisites before allocation:

```sh
bash tests/provision-profile-check.sh
bash config/profiles/android/tests/android-provision-check.sh
bash dev/e2e/producer-lifecycle.sh --preflight
dev/agent-e2e.sh --slot "$slot" --vm-count 1 --vm 1 --purpose producer-lifecycle -- \
  timeout --signal=TERM --kill-after=60 5400 bash dev/e2e/producer-lifecycle.sh
```

This requires Linux user/mount/PID namespaces locally, one standard disposable VM,
passwordless guest root helpers, and the existing Incus platform fixture. It creates
one marker-owned nested container and removes it before the native build. It does
not impose per-operation memory, byte or inode quotas, and does not cover ARM or
release acceptance.

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

`test-vms status` prints JSON with indentation and line breaks. Add `--json` for compact
machine output; both formats contain the same fields.

A `test-vms` initialization also converges a root-owned physical-host log sink and its one-minute
timer. The outer yard receives no mount or socket that can write the host log root.

A fresh yard with the effective `test-vms` capability starts with desired power `running`. Later
`yard -Y test-yard stop` and `start` persist the normal managed power intent. Agent acquire never
changes it. Stopping the outer yard drains all active slots before shutdown.

`E2E_VM_SLOT_COUNT` defaults to `2`. Slots are shared between environment types, rather than
partitioned into separate pools. Increasing the count adds empty slots. Shrinking fails closed
while retiring slots are held, provisioning, draining or quarantined.

| Type | Guests | RAM per guest | Virtual disk per guest |
| --- | ---: | ---: | ---: |
| `subyard-pair` | 1 or 2 | 4 GiB | 20 GiB |
| `android-test` | 1 | 8 GiB | 40 GiB |

These are initial type defaults; the broker reports the actual CPU, RAM and disk contract in the
grant. Virtual capacity is distinct from physical storage usage and retained image/cache costs.
`--vm-count 1` requests a single standard guest; omitted count preserves the type defaults
(two standard guests or one Android guest). The historical `subyard-pair` type name is retained
so singleton and pair allocations reuse the same prepared base.

On x86_64, broker-owned VMs expose ACPI S3 (`deep`) for real suspend with RTC wakeup.
The broker applies a fixed `raw.qemu.conf` override to enable the q35 ICH9-LPC S3
global, which Incus disables by default. Restricted projects allow VM low-level
configuration for this broker-owned setting; their resource, device and network
restrictions remain in place. The forced agent facade accepts no QEMU configuration
and provides no Incus access. Other architectures retain the default low-level
restriction and receive no ICH9 override.

The broker reserves each requested environment's RAM atomically while it is being provisioned,
including concurrent requests and the base-image builder. Once the grant is held, occupied RAM
is accounted for by host available memory; ready, stopped and empty environments retain no RAM
growth promise. Working VMs and builders have autostart disabled; each new allocation passes admission.
An interrupted provisioning request retains its admitted RAM promise until cleanup finishes,
because an in-flight start may still consume it.
Successful native starts alone do not release that promise: boot, tool and transport readiness
can still be incomplete. The existing persisted phase remains `provisioning` until `MarkHeld`;
it conservatively keeps the full promise even while started guests occupy physical pages.
Earlier release would require crash-safe phase evidence and compatibility for retained readers.
Bounded disk growth remains reserved for existing allocations. Admission also accounts for
configured safety reserves, measured image size and filesystem/pool headroom.
Memory admission uses the lower of visible memory/cgroup headroom and physical-owner
`MemAvailable` on both sides of usage sampling. An enabled backend receives one fixed read-only
Incus file bind from the owner's `/proc/meminfo` to `/var/lib/subyard/host-meminfo`; setup and active
broker updates reconcile it without restarting a running outer yard. The candidate verifies live
procfs and the exact read-only file mount before replacing the installed broker engine. Missing,
malformed or unverified physical counters refuse admission rather than falling back to LXCFS.
Swap is not RAM headroom. Status reports `physical_available_bytes` and
`physical_available_known` separately from the limiting visible cgroup; this does not establish
outer-host cgroup limits, peaks or OOM counters.
The visible cgroup walk follows the Incus daemon's guest-launch ancestry, preserving finite
daemon and slice ceilings. The broker worker's separate process ceiling is not a VM budget.
No per-VM resident memory measurement is needed. Status reports pending RAM promises through
`reserved_vm_memory_bytes` and per-slot `memory_commitment_bytes` and
`remaining_memory_growth_bytes`; held slots report zero in these fields. With no other pending
requests, a standard 4.5 GiB singleton needs 8.5 GiB available and a 9 GiB pair needs 13 GiB
available to preserve the default 4 GiB host reserve. Other provisioning requests and builders
can require additional headroom.
`resources.memory` also reports raw `visible_available_bytes`, `cgroup_headroom_bytes`
(applicable only when `limit_available=true`) and `limiting_source` as `visible_meminfo`,
`visible_cgroup` or `verified_physical_meminfo`. `resources.memory_admission` shows the
effective available bytes, reserve, pending VM and builder promises, and saturating headroom:
`max(0, effective available - reserve - pending VM - pending builder)`. Swap never increases it.
An active builder serializes new admission under the image lock until confirmed cleanup; status
reports its separate promise and a conservative headroom bound because the retained builder
record does not identify an associated requesting slot.
A typed `capacity` refusal identifies `memory` or `disk` and is safe to retry after resources are
freed. Memory refusals include this request's required RAM, type/count and the same numeric
admission components in the existing bounded message, preserving the released response envelope.
An occupied-slot refusal remains a distinct `busy` outcome.
It does not quarantine a healthy slot. Partial provisioning failures are cleaned up and
recovered automatically after 1, 5 and 15 minutes, then hourly while the slot remains eligible.

Physical headroom is checked against the entire backing filesystem. With the `dir` driver,
the disk budget instead charges the inner daemon's image cache and allocated blocks in its VM
and VM-snapshot directories, plus outstanding growth and builder commitments. This includes
retained and orphan VM disks; unrelated files elsewhere on the backing filesystem do not consume
the broker budget. VM symlinks contribute only their own allocated blocks and are never followed;
symlinked storage roots are rejected. Status reports this charge separately as `budget_used_bytes`. For `btrfs`
and `zfs`, budget accounting retains the conservative whole-pool usage bound. Missing or unsafe
usage measurements refuse admission; they never waive the budget or physical reserve.

Deleting a file inside a `dir`-backed guest need not immediately reduce the host blocks charged
to its virtual disk. The disk-exhaustion check observed guest free space return after closing
the temporary file, while host allocation stayed high until release deleted the root volume.
Use the broker's physical usage and remaining-growth fields when assessing headroom.

Admission settings use shipped defaults, Git shared/host/yard, local shared/host/yard, then command
configuration precedence and are
installed by `yard init`. These initial defaults still require workload peak measurements:

| Setting | Initial value | Purpose |
| --- | ---: | --- |
| `E2E_DISK_BUDGET` | `0GiB` | Optional total disk quota; `0GiB` means no fixed ceiling |
| `E2E_CACHE_BUDGET` | `24GiB` | Base and build cache budget |
| `E2E_DISK_RESERVE` | `5GiB` | Free physical storage reserve |
| `E2E_MEMORY_RESERVE` | `4GiB` | Memory headroom outside VM commitments |
| `E2E_VM_OVERHEAD` | `512MiB` | Additional RAM reserved per VM |

Broker workers have a separate 4 GiB process budget: 2 GiB RAM and up to 2 GiB swap.
Setup installs the native `subyardtestvms.slice` budget and reapplies it on repeat runs;
service dependencies recreate it after restart. `resources.broker_memory` reports its own
RAM/swap use and limits, separately from guest commitments and host admission. Its verified
allocation scope is the visible cgroup namespace only; hidden owner-host ancestor evidence
remains unavailable. An allocation ceiling below 2 GiB RAM fails closed.
The 4 GiB host safety reserve is a separate admission margin. The broker's occupied pages
already affect available memory, so its configured 4 GiB budget is not subtracted a second time.
Incus creates guest and builder processes outside the broker's process budget; their VM limits
and per-VM overhead apply independently.
Lifecycle and reaper units run directly in the slice. Other workers run in transient services
created by PID1; the original process execs
`systemd-run` as its native stdio transport, retaining the caller's existing resource ceilings.
Direct Incus commands with an invisible parent first exec a small transport waiter at the
original attached PID; its child then has a visible requester to verify. The waiter forwards
literal arguments and stdin, preserves the child exit status and exits promptly on cancellation.
A root-private native credential carries request inputs; capabilities stay out of unit
Environment and command properties. A persistent drop-in restores `LoadCredential` only for
this worker family after container images' type-wide credential reset, preserving their other
settings. The protected source filename matches the exact native unit. The worker verifies both
the transport and original requester's PID/start identities and its exact service membership. If either dies, cancellation
begins within one second and a bounded fallback terminates the service; systemd removes its
remaining child processes and collects the transient unit. The minute reaper removes protected
handoff files left by a crashed launch.

Values must be positive `MiB` or `GiB` sizes, except `E2E_DISK_BUDGET=0GiB`, which disables
the optional quota. Physical free-space reserves and outstanding VM/builder commitments always
apply. Status reports `budgets.disk_bytes=0` for an unlimited quota. Existing positive configured
quotas remain effective; set the yard value to `0GiB` and run `yard init` to remove one.
Recipe installation uses a fixed root-owned path; there is no public setting that lets an agent
substitute executable recipe sources.

Both types use the generic host baseline: Go bootstrap, compiler/build utilities, ShellCheck,
Git, curl, jq, ripgrep, SSH, archive tools and the product Incus installer. The `android-test`
name selects the larger VM resource contract only; its image contains no Android SDK, emulator,
renderer configuration or application setup. Those belong to the separate Android environment
workflow. Images contain no project data, credentials or project caches. Go's toolchain remains
selected by `go.mod`. Releasing a lease deletes its working VM disks and preserves the reusable
base image, subject to the ordinary refresh and cache-retention policy.

Refresh a type explicitly through the normal operator action flow:

```sh
yard -Y test-yard test-vms refresh subyard-pair
yard -Y test-yard test-vms refresh android-test
```

Refresh reserves private builder resources, asks for confirmation with a default of Yes, and
publishes a new fingerprint only after validation. It neither acquires a slot nor replaces active
lease disks. Failed refresh keeps the previous base and reports a bounded failure code. Later acquisition uses the recorded
retry deadline and normal recovery backoff.

Bases refresh when the recipe changes, on an explicit operator request, or after seven days.
A candidate becomes usable only after its validation passes; failed builds never replace the
published fingerprint. A previous base may serve only while compatible, unrevoked and unexpired.
Used bases remain pinned until their leases end. Disposable disks, builder disks and obsolete bases
are cleaned separately. Legacy retained slots require explicit safe retirement before reuse;
normal agent acquisition never deletes an unclassified legacy allocation. An active legacy lease
can renew and release, but release retains its old disks until explicit operator retirement:

```sh
yard -Y test-yard test-vms retire-legacy --slot N
```

This operation asks for destructive confirmation with a default of No. Review and preserve any
needed legacy data first. An empty, correctly marked legacy project can be adopted automatically.

Ordinary P0 uses the standard `subyard-pair` resources (4 GiB / 20 GiB per guest).
Smoke, full and peer scenarios request two guests; independent single-host lanes request one.
An explicit `SUBYARD_P0_NESTED_VM=2` retains two guests for that selector.
They check broker installation, legacy migration, active/inactive runtime updates and rollback,
without recursively allocating another four-VM broker pool. Android acceptance uses one
`android-test` VM (8 GiB / 40 GiB); it does not need a pair or 16 GiB guests.

Physical candidate-broker cross-slot isolation and quarantine/rebuild remain a separate, explicit
diagnostic (`SUBYARD_P0_BROKER_RECOVERY_ONLY=1`). That diagnostic creates nested VM pairs and
checks capacity before setup; it is not an automatic full P0 requirement. Select it only when the
changed broker behavior needs that physical evidence and a suitable target is available. Its
capacity requirements are not a reason to enlarge the ordinary development pool. Broker admission,
fencing and recovery logic also have focused host-free race tests, which do not substitute for
physical isolation evidence. The operator-owned outer broker's successful lease does not prove
the candidate broker code.

## Agent workflow

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
status before it sends the exact typed `acquire-v3` request. Legacy acquire is not supported, and the runner never downgrades in
response to status or acquire failure. Only a typed busy response plus bounded `--wait` permits
another request for the same slot; a transport failure or any other unknown outcome ends the
attempt. A `held` busy response
contains exactly the safe owner fields `display_label`, `yard`, `project`, `run`, `purpose`,
`acquired_at` and `expires_at`. Other unavailable states carry no owner object. The runner validates
the complete response before retrying; a missing, extra or malformed owner field makes the outcome
unknown and prevents another acquire.

Explicit `--vm-count` requests additionally require `vm-count-v1`. The facade accepts
`acquire-v3 TYPE client_id fingerprint yard project run purpose key_type key_blob slot_id [vm_count]`.
The optional count is exactly `1` or `2`; Android supports only `1`. Unsupported counts are
rejected before slot acquisition. Older requests omit the count and preserve their type default.
An older broker without the capability refuses explicit-count runs before acquisition; update
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

The default pool has slots 1 and 2, but callers must use the configured capacity reported by status.
The live lease acceptance always exercises one exact slot and, when a second exists, adds concurrent
cross-slot isolation. Additional slots remain unselected and must retain the same state, lease epoch
and resource generation throughout the run.

### Test lanes and gates

The focused configuration check uses one fresh pair and a synthetic private SSH Git remote.
It installs the same runtime artifact built from the current worktree on both hosts, verifies two-way
configuration convergence, and rejects dirty, diverged and conflicted checkouts without changing
live settings or remote refs:

```sh
bash dev/e2e/config-sync-acceptance.sh --slot "$slot"
```

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
`--vm` accepts `1` or `2` and defaults to `1`. It changes
network policy only inside the disposable VM and does not enable isolation on the operator's host.
The skill's risk criteria determine whether the focused network lane needs broader lifecycle coverage.

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

The focused AppArmor regression creates a temporary container yard on VM1, exercises failed
capability probes and both real Incus AppArmor transitions, and verifies Docker and runtime
preservation. It restores its marked systemd override and tears down its yard. Run it on a free
slot whose inner Incus starts with AppArmor enabled; it is targeted evidence, not a full P0:

```sh
dev/agent-e2e.sh --slot "$slot" --purpose apparmor-probe --vm 1 -- \
  bash dev/e2e/incus-apparmor-probe.sh
```

The Incus group regression uses a temporary operator to exercise the first named `init` before
`incus-admin` membership is active. It verifies native named socket and policy-lock ACL activation
in the original prepared process, retained lock identity, explicit command overrides, independent
yard SSH ports and an idempotent retry. It removes the marked operator ACLs, account and yard
after the check:

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
Existing before/after categories report APT index bytes in both scopes; reclaiming them still
requires a fresh physical-capacity admission result.
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
dev/agent-e2e.sh --slot "$slot" --type android-test --purpose broker-memory-boundary --vm 1 -- \
  bash dev/e2e/broker-memory-boundary.sh
```

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
represent authentic historical releases. Actual retired-source refusal, staged full uninstall
and separate clean installation belong to `dev/verify-release-upgrades.py --only-legacy-retirement`
and the ordinary release compatibility check; native deletion belongs to `full-uninstall`.
The authenticated v0.11.1/v0.11.2 recovery fixture retains its independent historical contract.

| Lane | Prerequisites and timeout | Mutable scope | Classification |
| --- | --- | --- | --- |
| `./tests/run.sh` | Go toolchain; bounded by CI | temporary host-free roots and `.build/yard` | full host-free suite |
| `dev/process-coverage.sh` | Go toolchain; selected host-free process contracts | `.build/coverage` and test-owned temporary roots | optional coverage diagnostic |
| `smoke` (default) | both allocated VMs; capacity/dependency preflight and one reboot | marked Incus, release and peer fixtures described below | publication smoke; see skill policy |
| `boundary` | one broker lease; SSH connect deadlines | read-only facade, routes and negative probes | required in smoke and full |
| `transport` | both allocated VMs; bounded SSH disconnect probe | one marker-owned remote sleep and temporary controller log | required in smoke and full |
| `nested-teardown` | frozen current candidate, VM2, KVM and nested Incus; bounded install, boot and cleanup waits | marker-owned outer VM and nested yard, each bootstrapped through the public installer; data-boundary fixtures | targeted diagnostic; required in full |
| `full-uninstall` | one allocated VM and installed current candidate; bounded phase/reboot deadlines and a sink delay longer than 60 seconds | marker-owned yards, producers, credentials and installation roots; preserve foreign resources and selected-yard neighbors | singleton held-lease uninstall, interruption/retry and reboot acceptance |
| `dependencies` | immutable image baseline; 20-minute cold Go download deadline | marker-owned cold caches only | periodic targeted bootstrap diagnostic |
| `real-incus` | VM1 when targeted/smoke and in the full owner chain; KVM, persistent Incus pool and 15-minute mutation deadlines | marked project, container, VM and image aliases | required in smoke and full |
| `profile-resource` | VM1 and current candidate | marked bound-resource profile | targeted physical diagnostic; generic resource consent/state checks run locally |
| `release` | VM1 fixture with two-VM allocation/preflight; bounded nested install/boot deadlines | fresh candidate yards, current and legacy convergence | targeted diagnostic; covered by the full owner chain |
| `source-upgrade` | VM1 when targeted; VM2 worker in full; two bounded reboots | marked source-install/migration fixture | targeted diagnostic; required in full |
| `power-systemd` | VM1 when targeted; VM2 worker in full; real Incus, Ubuntu 24.04/systemd 255; 900-second image-cache fill, 600-second local launch, 300-second restart and bounded TERM-to-KILL Incus commands | test-owned image alias, marker-owned parser project plus snapshotted/restored host power runtime | targeted diagnostic; required in full |
| `reboot-verify` | VM1, real Incus and cached image preparation; published v0.11.0 upgrade, incompatible rollback refusal and authentic v0.21.0 compatible rollback/candidate fixture; two boot checks with bounded power reconciliation | marked upgrade fixture, two guest reboots, snapshotted/restored host power runtime | targeted transport/recovery diagnostic |
| `release-smoke` | VM1, pinned v0.14.0 installer, candidate package and one reboot | marked yard/project plus retained operator and guest data | singleton targeted checkpoint/reboot evidence; also required in smoke and full |
| `peer` | both VMs and synthetic keys | marked cross-owner RPC, project and credential fixtures | smoke covers fresh init/projects/RPC; full adds offline and credential scenarios |
| `peer-cleanup`, `cleanup` | current disposable allocation | exact marked fixtures and run worktrees | standalone idempotent cleanup/verifier |
| `--lane full` | all prerequisites above | union of the marked scopes | periodic manual and risk-selected exhaustive matrix; includes release smoke |

The owner chain recovers two authentic v0.11.1 histories through the checksum-pinned official
v0.11.2 bridge. This preserves the closed recovery policy and its historical metadata writers.
Each completed or interrupted/resumed bridge then
upgrades through the public installer to the unmodified frozen current candidate. The fixture
records the bridge and current runtime hashes separately and verifies current readiness,
materialized configuration and retained migration history. The historical updater requires the
default power boundary: before each history, the fixture refuses any existing default helper,
unit, enablement or custody metadata. After current assertions, supported selected-yard teardown
removes the exact historical yard; full uninstall then removes the remaining installation and
must remove installed resources and enablement before reuse. Default power install locks and
generation records remain as coordination fences. The first history refuses preexisting fences;
the second accepts only the exact validated state captured around its own successful first
cleanup. Busy, unsafe or changed coordination state refuses reuse without deleting barriers.
Producer cleanup prepares the same operator's systemd user manager on the disposable VM when
absent, without enabling lingering or restarting an existing manager.
Failed histories retain their marked fixture state while a default power artifact or producer
remains; fixture fallback cleanup never stops or deletes global default power resources.
The fixture declares a stable plain Codex config artifact before historical recovery, while
retaining the authentic shipped rules through the bridge. Only after the bridge completes,
it selects rules copied from the authenticated active bridge, verifying the guest rules match
before initial adoption. The default TOML path must remain absent before and after the current
upgrade. This lane covers instruction composition and native integration readiness, not
structured TOML migration.

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
retirement, desired-retained failure and retry, and stopped-yard rejection without model API calls:

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

The pinned legacy-upgrade fixture installs the published v0.14.0 ordinary default, then exercises
candidate-owned planning, exact ownership adoption, disable/re-enable preservation,
rollback with the retained config writer, and a forward retry:

```sh
dev/agent-e2e.sh --slot "$slot" --purpose integration-legacy-upgrade --vm 1 -- \
  bash dev/e2e/integration-legacy-upgrade.sh
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
It does not establish another agent's workload health or exercise nested broker recovery.

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
broker. This check does not replace broker recovery or Android application acceptance.
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
acquire. Its authentic checksum-pinned 0.19.0 owner upgrades through its supported updater to the
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
0.20.0, select the authentic published predecessor explicitly:

```sh
bash config/profiles/github/tests/e2e/release-activation.sh --slot "$slot" --baseline 0.20.0 --preflight
SUBYARD_E2E_CANDIDATE_BUNDLE=/absolute/path/to/candidate-bundle.tar.gz \
SUBYARD_E2E_CANDIDATE_SHA256=<bundle-sha256> \
  bash config/profiles/github/tests/e2e/release-activation.sh --slot "$slot" --baseline 0.20.0
```

The controller packages its explicitly synthetic compatible successor (the next patch version
with an `activation-fixture` suffix) before lease acquisition; the original candidate's verified
assets remain intact. The guest uses the actual checksum-pinned 0.20.0 installer/updater, verified
against immutable published release metadata, two container yards and a config-only absent yard.
Normal invocation runs the same cheap local checks as `--preflight` before packaging/allocation;
`--preflight` exits before allocation. A valid local shape alone does not authenticate an archive:
normal execution still checks its actual bytes, architecture, engine version and pinned installer.
The default `--baseline 0.19.0` preserves the original 0.19.1 activation fixture path. A first transition may
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

For targeted migration and activation recovery acceptance, freeze a candidate with
`dev/release-acceptance.py prepare`, then transport its `candidate-bundle.tar.gz` through
`SUBYARD_E2E_CANDIDATE_BUNDLE` and `SUBYARD_E2E_CANDIDATE_SHA256`. Run
`bash dev/e2e/reliable-migrations-recovery.sh run NUMERIC_TOKEN` with the standard wrapper,
one available slot and `--vm-count 1 --vm 1`. The fixture installs checksum-pinned v0.17.3,
changes persistent file settings in default and named local yards, verifies real guest convergence
through public update, then interrupts activation-only reconciliation through an owned systemctl
wrapper. Further interruptions leave immutable receipts before journal CAS and exercise a desired-input
cycle, including return to earlier inputs. Each replacement needs new consent, durable cancellation
and the next protected frontier generation before ready. Declined consent preserves the predecessor,
migration history and links; ready repetition checks the same fixed point. Stopped, absent and remote
yards are preserved. Checkpoint writer acceptance also requires interruption at publication and
reboot/resume, with the compact authority and unchanged legacy projection verified separately.
The release smoke's existing prepare/reboot/finish controller interrupts atomic checkpoint
publication before reboot, compares the checkpoint, legacy projection and completed journal before
the fresh updater resumes, and then checks the exact release links and unchanged history. Publication
follows journal completion but precedes final updater cleanup. Record this segment's result explicitly;
a recovery-only pass cannot substitute for it.
The same singleton fixture retains the v0.17.3 case and then creates a fresh marked v0.18.6
operator/project fixture. The owning test-yard helper initializes the real canonical test yard with one
slot and its normal broker route, without acquiring/refreshing slots, building a cold base or creating
an inner VM. It checks actual local/expanded readonly devices and guest reads of route/known-host files,
early staged source repair against completed and authentically interrupted original history, separate
original-owner recovery consent, candidate convergence and repetition. Root-readable persistent agent
assets are checked in a fresh registration. Host sink files, service/timer state and host power runtime
are snapshotted/restored; all extra projects and owned images use marked cleanup.

For checkpoint publication before reboot, `dev/e2e/p0-acceptance.sh --lane release-smoke --slot N`
exposes the existing prepare/reboot/finish phase on one standard VM. This targeted result cannot be
reported as complete smoke or full P0 acceptance; those lanes retain their existing two-VM contracts.

Namespace capacity, aggregate-byte limits and sequences beyond 512 cancellations are host-free stress
contracts, not claims of this physical fixture. This targeted evidence does not replace release smoke
or shipped-profile acceptance.

Live account/service checks follow the [external-check contract](reference/release-publication.md#external-checks).
Required synthetic VM fixtures, including OpenClaw staging safety/lifecycle, establish their own
boundaries; they do not prove real service behavior.

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
additional systemd parser fixtures from `power-systemd`. The exact published v0.11.0,
the inclusive supported migration floor, remains the genuine legacy-upgrade baseline.
After its verified update, explicit `yard init --continue-legacy --yes` establishes only
bounded normal-reconcile authority; repeated plain init and both reboots must retain that
grant without adding installation ownership. Its incompatible
rollback must refuse without changing release links, protected V2 checkpoint/history,
ledger, journal, native identity or grant, including retained V1 evidence or its absence.
Fresh sources below v0.11.0 remain refused; accepted completed V2 history and recovery
retain their separate supported contracts.
The guest verifies checksum-pinned official v0.21.0 assets and compiled activation
capabilities before creating the cold legacy baseline. Public update checks must identify
the exact unblocked target before installing v0.21.0, restoring the candidate, then
performing a real compatible rollback and candidate roll-forward. Both passages preserve
the limited grant, native policy and absence of installation ownership; final normal init
and full-removal refusal verify that the grant never acquires destructive authority.

List or run one lane:

```sh
dev/e2e/p0-acceptance.sh --list-lanes
dev/e2e/p0-acceptance.sh --slot "$slot"
dev/e2e/p0-acceptance.sh --slot "$slot" --lane full
dev/e2e/p0-acceptance.sh --slot "$slot" --lane peer
dev/e2e/p0-acceptance.sh --slot "$slot" --lane source-upgrade
SUBYARD_P0_WAIT_SECONDS=1200 \
  dev/e2e/p0-acceptance.sh --slot "$slot" --lane power-systemd
```

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
VM2 independently runs nested teardown, container-image preparation, source upgrade and power-systemd in that order.
Independent source-upgrade, power-systemd, reboot-verify and release-smoke lanes reconcile Incus and cache
their container image without repeating the real-Incus contract's container/VM launches and Go tests.
The owner fixture packages its candidate once and reuses those exact assets across its independent
predecessor installations, active/inactive broker checks and rollback scenarios.
The two chains join before the shared release-smoke phase and full peer checks, followed by cleanup
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
a collection of targeted passes does not replace that gate. No Markdown parser executes the plan.

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
script invocation or one interactive SSH session. Once leased, the agent has unrestricted root
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
yard -Y test-yard test-vms recover --slot "$slot"
```

## Broker diagnostics

Agents can read their own lease diagnostics through the existing restricted SSH facade:

```sh
dev/agent-e2e.sh --logs --slot 1 -n 50
dev/agent-e2e.sh --logs --json
dev/agent-e2e.sh --incident <incident_id>
```

These reads never acquire, renew, release or recover a lease. The private random controller
`client-id` is a diagnostic read capability; keep it private along with the controller identity.
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

Its released-source case installs checksum-pinned actual v0.10.0, lets that runtime create a yard,
refuses upgrade without changing protected config or links, stages current cleanup tooling without
activation, refuses an unrelated project profile, and interrupts full removal at config deletion.
A fresh retained recovery process finishes the same plan; a separate clean installation does not
reuse old configuration. Unproven legacy host namespaces and shared infrastructure remain explicitly
preserved. The same lane retains its modern ownership, foreign-control and reboot checks.

Its marker-owned fixture runs lifecycle commands as an ordinary isolated owner, with root helpers
confined to the leased VM. It includes direct/custom installer-only and bootstrap-only cases,
default/named/stopped/partial ownership, credentials and the real host sink, a connected SSH refusal,
foreign workload/shared-volume/user-service/SSH/shell controls, a bound Git project and an ordinary
foreign file inside the custom host root. Reboot readiness checks transport and a changed boot ID;
it does not require the power reconciler which full removal has retired. Host-free
`tests/full-uninstall.sh` additionally removes Incus from the actual executable path so the normal
P0 Incus baseline cannot mask the cold-install boundary. These fixtures do not establish acceptance
until their native phases and cleanup have completed against the frozen current candidate.
