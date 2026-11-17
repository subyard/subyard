# Test VM operator setup and client

## Operator setup

Install the service on the local owner:

```sh
yard service install test-vms
yard test-vms status
yard test-vms revoke --slot 2
```

The creation preset supplies the new service's `test-yard` name, SSH port `2223`, and
guest UID `1001`, avoiding the shipped cloud image's UID `1000`. Workspace
selection, including `-Y` and `SUBYARD_YARD`, does not select the service. Ordinary init
checks owner port collisions before effects. Existing runtimes keep their configured
settings, including an implicit UID from their existing template configuration.

A new-release update registers a uniquely proven owned broker after reconciliation, retaining its
runtime and leases. Same-release commands keep their normal behavior; registration findings
and manual recovery follow the [service activation contract](control-plane.md#owner-infrastructure-services).

Adopt an already owned broker without initialization, container restart or lease interruption:

```sh
yard service register test-vms --yard test-yard
```

Registration verifies installation ownership and the native instance UUID. A name or profile
match alone grants no authority. Repeat registration to repair an interrupted public route
publication; an unchanged registration keeps its generation. After an owner HostID rename, normal resolution
returns `service_owner_changed`; repeat explicit registration to prove the same installation/UUID
and republish the owner metadata with a new registration generation. Existing leases are retained.

`--owner ID` selects an explicitly registered owner connection; omission selects this installation's
local owner. `--pool ID` selects an exact registered pool. One registration is automatic; multiple
registrations return `service_ambiguous` until an exact pool is supplied. `service_absent` and
`service_unavailable` distinguish a missing registration from an unavailable runtime/publication.
On a host whose installed release has no `yard service register` yet, set
`SUBYARD_E2E_ROLLOUT_YARD=test-yard` for `dev/agent-e2e.sh` and the lanes it runs: it selects that
outer route instead of owner resolution, unless `--owner` or `--pool` is given.
Status JSON includes `service` with owner, service/pool, yard/project/instance, native UUID and
registration generation. Error JSON contains the same safe candidate identities. Resolution never
selects another owner, pool or workspace after failure.

Operator lifecycle uses the existing prepared yard reconciler:

```sh
yard service stop test-vms
yard service start test-vms
yard service update test-vms
yard service remove test-vms
```

`service update` reconciles this container against the currently installed runtime. Ordinary
`yard update` installs runtime releases. Stop drains active leases through the existing broker
boundary. Stop/removal plans bind the profile's slot generations and lease epochs; heartbeat
timestamps do not invalidate unchanged allocations. Registration/runtime or allocation drift refuses
the captured operation. Native fencing closes new admission under the lease lock before drain;
retained workers cannot rewrite the incompatible fenced schema. If stop fails after fencing,
existing leases keep their keys and explicit `service start` resumes admission. An older broker
requires `service update` before a fenced stop/removal; registration/status remain available. Removal retires the exact registration and public route after native cleanup.
Repeat removal to finish metadata cleanup when the original runtime is already absent. Ordinary
named-yard teardown and full uninstall also include owned service metadata.

For an additional pool, configure a distinct named yard with the profile and a nonconflicting SSH
port, then use `service install test-vms --yard NAME --pool ID`. Owner inventory labels registered
service runtimes `infrastructure-service`.

`test-vms status` prints JSON with indentation and line breaks. Add `--json` for compact
machine output; both formats contain the same fields.

A `test-vms` initialization also converges a root-owned physical-host log sink and its one-minute
timer. The outer yard receives no mount or socket that can write the host log root.

A fresh yard with the effective `test-vms` capability starts with desired power `running`. Later
`yard service stop test-vms` and `service start test-vms` persist the normal managed power intent. Agent acquire never
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
so singleton and pair allocations reuse the same prepared base. `--disk 50GiB` requests
that size per VM for either type; omission preserves the configured default. The preset is
refused if the configured default exceeds 50 GiB. Shared bases use the configured default size.

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
yard test-vms refresh subyard-pair
yard test-vms refresh android-test
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
yard test-vms retire-legacy --slot N
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

## Client for any yard

The shared broker client lives in the opt-in `test-vms-client` profile. Selecting it delivers the
client and the short `subyard-test-vms` skill into the yard's installed runtime, so agents in any
yard can use the pool without a Subyard checkout or a coding-tool integration, for example in the
Hermes yard:

```sh
yard -Y hermes provision test-vms-client
```

`provision` adds the profile to the yard's local selection. To remove it, set
`ENVIRONMENT_PROFILES` without `test-vms-client` and run `yard -Y NAME init`; static skill refresh
removes the client, the skill and its links. The route mount itself is unchanged: every yard
without `ALLOWS_HOST_ACCESS=false` receives the read-only route registry. A role with
`ALLOWS_HOST_ACCESS=false` refuses the profile. The client runs as the ordinary `dev` user and
needs no sudo.

The client and skill are one skill directory, `/usr/local/share/subyard/skills/subyard-test-vms/`,
owned by the shared skill receipts. It is linked for Codex, Claude Code, OpenCode, pi and
Hermes (`~/.hermes/skills`). Release refresh updates it with the other static skills.

```sh
client=/usr/local/share/subyard/skills/subyard-test-vms/client.sh
bash "$client" status
handle="$(bash "$client" acquire --slot 1 --purpose my-check)"
bash "$client" exec "$handle" -- apt-get install -y some-package
bash "$client" exec "$handle" -- systemctl status some-service
bash "$client" release "$handle"
```

`acquire` uses the same broker protocol, capabilities, attribution, exact slot, `--wait` and
typed busy/capacity refusals as the runner below, and requests one standard VM unless
`--vm-count 2` or `--type android-test` is given. Attribution uses the yard name from the guest
facts and `--project NAME`, defaulting to the yard name. It prints a handle: a private directory
under `~/.subyard/e2e/handles/` with the captured route and host-key pin, lease credentials and an
ephemeral guest key. `exec` runs a command as root in the leased VM and returns its exit status;
`--stdin` forwards input. `ssh HANDLE` opens an interactive root shell. Separate `exec` calls share
one lease and one VM until `release`. `leases` lists handles; `logs` and `incident` read this
controller's diagnostics as described below.

A detached keeper, started before a streaming grant is acknowledged, owns each handle. It renews
every five minutes only while that exact handle exists. Each renewal transport runs in its own
process group, checks the handle and keeper right before sending and is killed as a whole when
either is gone, so a released, replaced or removed handle or a killed keeper cannot renew. The
keeper releases the lease after `--idle` (default 20 minutes, between 1 minute and 2 hours)
without `exec` use; a running `exec` counts as use, and `exec HANDLE -- true` resets the timer.
`leases` shows `in-use` while an `exec` runs. A renewal transport failure or timeout is retried
every minute for up to fifteen minutes after the last successful renewal, below the broker's
twenty-minute expiry; an explicit broker rejection or a longer outage marks the handle `lost`.
After three failed idle releases the keeper also marks it `lost` and stops renewing, so broker
expiry reclaims the slot. If the keeper stops, `exec` refuses and the broker expires the lease
twenty minutes after its last renewal. Release a lost or keeper-less handle and acquire again.
Handle release prints the same `E2E_RELEASE` evidence as the runner.
Deselecting the profile does not interrupt a held lease: its running keeper still releases it
after the idle limit.

`dev/agent-e2e.sh` is the Subyard development special case of this client: it sources the same
file from the checkout and adds worktree copy, guest run directories, lanes and phase evidence.
Its lease remains bound to the wrapper and its launching process as described below.

`config/profiles/test-vms-client/tests/e2e/acceptance.sh --slot N` takes three sequential
singleton leases. The first installs the candidate through the supported installer on the leased
VM, provisions the profile into a Hermes-preset yard and checks the client, the skill, the Hermes
link and the unregistered-broker diagnostic, then checks retirement after deselection and the
refusal in a role without host access. The second runs an installed client copy outside the
checkout against the pool; the third runs `--verify-boundary`.

