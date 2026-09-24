# Agent E2E VM pool

The `test-vms` profile runs a root-owned lease broker inside a trusted outer yard. Its configured
pool contains one or more shared slots and defaults to two. Each lease selects `subyard-pair`
(two VMs) or `android-test` (one VM). Slots own isolated inner Incus projects and networks;
each acquire creates fresh VM disks from a versioned immutable base. Release fences access and
deletes the disposable disks. No guest state survives a lease as a supported continuation mechanism.

The outer yard remains operator-owned. Agents can acquire inner slots, but cannot start a stopped
outer yard, enter its shell, reach its Incus socket or invoke arbitrary lifecycle commands.

## Running tests

Choose checks using [Subyard dev-flow](../.agents/skills/subyard-dev-flow/SKILL.md#choose-checks-by-risk).
This guide describes VM access, prerequisites and execution. When building or running the full
host-free suite, use the current public worktree:

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
| `subyard-pair` | 2 | 4 GiB | 20 GiB |
| `android-test` | 1 | 8 GiB | 40 GiB |

These are initial type defaults; the broker reports the actual CPU, RAM and disk contract in the
grant. Virtual capacity is distinct from physical storage usage and retained image/cache costs.

The broker reserves the full requested environment's RAM and bounded disk growth atomically,
including concurrent provisioning, held VMs and the base-image builder. It accounts for current
outer-yard memory, configured safety reserves, measured image size and filesystem/pool headroom.
A typed `capacity` refusal identifies `memory` or `disk` and is safe to retry after resources are
freed. It does not quarantine a healthy slot. Partial provisioning failures are cleaned up and
recovered automatically after 1, 5 and 15 minutes, then hourly while the slot remains eligible.

Physical headroom is checked against the entire backing filesystem. With the `dir` driver,
the disk budget instead charges the inner daemon's image cache and allocated blocks in its VM
and VM-snapshot directories, plus outstanding growth and builder commitments. This includes
retained and orphan VM disks; unrelated files elsewhere on the backing filesystem do not consume
the broker budget. Status reports this charge separately as `budget_used_bytes`. For `btrfs`
and `zfs`, budget accounting retains the conservative whole-pool usage bound. Missing or unsafe
usage measurements refuse admission; they never waive the budget or physical reserve.

Deleting a file inside a `dir`-backed guest need not immediately reduce the host blocks charged
to its virtual disk. The disk-exhaustion check observed guest free space return after closing
the temporary file, while host allocation stayed high until release deleted the root volume.
Use the broker's physical usage and remaining-growth fields when assessing headroom.

Admission settings use ordinary shipped/shared/host/yard/command configuration precedence and are
installed by `yard init`. These initial defaults still require workload peak measurements:

| Setting | Initial value | Purpose |
| --- | ---: | --- |
| `E2E_DISK_BUDGET` | `0GiB` | Optional total disk quota; `0GiB` means no fixed ceiling |
| `E2E_CACHE_BUDGET` | `24GiB` | Base and build cache budget |
| `E2E_DISK_RESERVE` | `5GiB` | Free physical storage reserve |
| `E2E_MEMORY_RESERVE` | `2GiB` | Memory headroom outside VM commitments |
| `E2E_VM_OVERHEAD` | `512MiB` | Additional RAM reserved per VM |

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

The nested broker lanes (`release`, `full` and broker recovery) need a larger allocated test host:
at least 16 GiB RAM. Disk admission belongs to the broker: it checks physical headroom and
outstanding commitments, while the test scripts only report disk measurements. Their diagnostic
broker uses 2 GiB / 10 GiB guests, two concurrent pairs, normal safety reserves and the immutable
image publication peak. The default 4 GiB pair guest cannot host that nested matrix.
Use an operator-configured pool with larger pair limits; the lane checks capacity before setup.
These diagnostic limits do not validate the production Android type's 8 GiB / 40 GiB contract.

## Agent workflow

Prepare the persistent controller identity once:

```sh
dev/agent-e2e.sh --prepare
```

The persistent controller key stays under the agent user's `~/.subyard/e2e/`; every lease uses a
separate ephemeral guest key.

A standard caller reaches the outer yard through the provisioned yard-to-yard route. Any valid
Ed25519 controller key is admitted only to the versioned forced facade
(`status/acquire-v3/renew/release`). It never receives an L1 shell, PTY, file transfer, arbitrary
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

Status also reports visible memory/cgroup evidence, physical Incus pool driver and used/free bytes,
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

The runner requires `attribution-v2`, `environment-acquire-v3` and `disposable-v1` from read-only
status before it sends the exact typed `acquire-v3` request. Legacy acquire is not supported, and the runner never downgrades in
response to status or acquire failure. Only a typed busy response plus bounded `--wait` permits
another request for the same slot; a transport failure or any other unknown outcome ends the
attempt. A `held` busy response
contains exactly the safe owner fields `display_label`, `yard`, `project`, `run`, `purpose`,
`acquired_at` and `expires_at`. Other unavailable states carry no owner object. The runner validates
the complete response before retrying; a missing, extra or malformed owner field makes the outcome
unknown and prevents another acquire.

Use redacted status to inspect the configured pool, explicitly choose an available slot number, then
run against the selected environment in only that slot. P0 always requests `subyard-pair`. For example, after choosing slot 1:

```sh
slot=1
dev/agent-e2e.sh --slot "$slot" --purpose host-free-suite -- ./tests/run.sh
dev/agent-e2e.sh --slot "$slot" --wait 20m --purpose host-free-suite -- ./tests/run.sh
dev/e2e/p0-acceptance.sh --slot "$slot"
dev/agent-e2e.sh --slot "$slot" --purpose real-host-check --vm 1 -- \
  ./tests/some-real-host-check.sh
```

The runner filters private and ignored files, verifies the worktree bundle and removes its guest
worktree. Every lease-taking invocation prints `yard + project + run + purpose`, environment type,
VM count and base fingerprint. Human status includes the environment type and per-VM resources.
`--type android-test` selects one guest; omit `--vm` to run on all actual guests. Explicit `--vm 2`,
`--vm both`, `--ssh 2` and pair boundary checks fail before acquiring an Android lease. The default
`--type subyard-pair` preserves existing pair callers.

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
compatibility and recovery matrix. The skill defines when each applies. GitHub workflows do not
receive pool access or run these checks automatically. `--list-lanes` does not acquire and needs no slot.

During the full owner compatibility chain, before current `yard init`, VM1 seeds the legacy
convergence fixture with:

```sh
SUBYARD_E2E_LEGACY_FIXTURE=1 \
  dev/e2e/seed-test-vms-legacy-state.sh subyard-test-yard yard-test-yard
```

This fixture is restricted to disposable VM1 candidate yards.

The [change-impact reference](testing.md) documents selector invocation and output.
Test-selection policy, including how to interpret full-P0 recommendations, lives in the skill.

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
| `./tests/run.sh` | Go toolchain; bounded by CI | temporary host-free roots and `.build/yard` | full host-free suite |
| `dev/process-coverage.sh` | Go toolchain; selected host-free process contracts | `.build/coverage` and test-owned temporary roots | diagnostic coverage gate |
| `smoke` (default) | both allocated VMs; capacity/dependency preflight and one reboot | marked Incus, release and peer fixtures described below | publication smoke; see skill policy |
| `boundary` | one broker lease; SSH connect deadlines | read-only facade, routes and negative probes | required in smoke and full |
| `transport` | both allocated VMs; bounded SSH disconnect probe | one marker-owned remote sleep and temporary controller log | required in smoke and full |
| `nested-teardown` | VM2, KVM and nested Incus; bounded install, boot and cleanup waits | marker-owned outer VM, nested yard and data-boundary fixtures | targeted diagnostic; required in full |
| `dependencies` | immutable image baseline; 20-minute cold Go download deadline | marker-owned cold caches only | periodic targeted bootstrap diagnostic |
| `real-incus` | VM1 when targeted/smoke and in the full owner chain; VM2 also runs it as a full-matrix prerequisite; KVM, persistent Incus pool and 15-minute mutation deadlines | marked project, container, VM and image aliases | required in smoke and full |
| `profile-resource` | VM1 and current candidate | temporary dependency-free resource/state and bound-resource profile | targeted diagnostic; full covers only the dependency-free resource portion |
| `release` | VM1 fixture with two-VM allocation/preflight; bounded nested install/boot deadlines | fresh candidate yards, current and legacy convergence | targeted diagnostic; covered by the full owner chain |
| `source-upgrade` | VM1 when targeted; VM2 worker in full; two bounded reboots | marked source-install/migration fixture | targeted diagnostic; required in full |
| `power-systemd` | VM1 when targeted; VM2 worker in full; real Incus, Ubuntu 24.04/systemd 255; 900-second image-cache fill, 600-second local launch, 300-second restart and bounded TERM-to-KILL Incus commands | test-owned image alias, marker-owned parser project plus snapshotted/restored host power runtime | targeted diagnostic; required in full |
| `reboot-verify` | VM1, real Incus and cached image preparation; published v0.8.0/candidate fixture; two boot checks with bounded power reconciliation | marked upgrade fixture, two guest reboots, snapshotted/restored host power runtime | targeted transport/recovery diagnostic |
| `release-smoke` (internal phase) | VM1, pinned v0.14.0 installer, candidate package and one reboot | marked yard/project plus retained operator and guest data | required in smoke and full; not a standalone lane |
| `peer` | both VMs and synthetic keys | marked cross-owner RPC, project and credential fixtures | smoke covers fresh init/projects/RPC; full adds offline and credential scenarios |
| `peer-cleanup`, `cleanup` | current disposable allocation | exact marked fixtures and run worktrees | standalone idempotent cleanup/verifier |
| `--lane full` | all prerequisites above | union of the marked scopes | periodic manual and risk-selected exhaustive matrix; includes release smoke |

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
Other modes are `vm`, `default`, `special` (fresh test-vms role), and `upgrade` (owned
ordinary-yard artifacts retired when adopting the test-vms role). The `default` mode installs
all five fresh-default integrations and needs their normal package download access. Test config,
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

For SSH fixture debugging, capture evidence before the lease ends. A fixture retained with
`SUBYARD_E2E_ORCA_KEEP_FAILED=1` is inspectable only within the same active lease. A new invocation
gets clean VMs, so `SUBYARD_E2E_ORCA_RESUME` cannot continue a previous lease. Record the failure,
evidence and remaining checks in the current task plan and rerun the independent segment.

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

The full matrix keeps the long historical owner, release and broker chain on VM1. VM2 independently
runs nested teardown, a real-Incus platform check, source upgrade and power-systemd in that order.
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
retained. Copy evidence needed for longer-lived tasks before that retention window expires.
Evidence never contains lease credentials, guest endpoints, command payloads or ambient environment.

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
`SUBYARD_E2E_PURPOSE`, `SUBYARD_E2E_SLOT`, `SUBYARD_E2E_VM`, `SUBYARD_E2E_TYPE`
and `SUBYARD_E2E_BASE_FINGERPRINT`.

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

A rebooting guest does not prevent fencing at the data route. No subsequent lease receives a prior
lease's disk or key. Generation, epoch, lease ID and server-side capability verification fence old
credentials after release and reuse.

Provisioning or cleanup failure quarantines only the affected slot. Before destructive recovery,
the broker fsyncs a local incident and verifies project/VM ownership, refusing foreign or ambiguous
resources. Recovery cleans the disposable allocation and returns an empty slot to the pool; the
next lease provisions its requested environment from a validated base. Failure to persist the
incident or prove ownership leaves the slot quarantined without deletion. Ordinary resource
shortage is a retryable admission refusal and does not quarantine a healthy empty slot.

The root reaper starts recovery immediately after the incident is durable. Failed rebuilds retry
after 1, 5 and 15 minutes, then hourly without an attempt limit. A temporary Incus, image, network
or capacity failure delays recovery; it does not turn quarantine into a permanent terminal state.
The manual command starts the same slot recovery workflow immediately:

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

The host sink also saves a bounded observation in `test-vms-broker-incidents/host/<incident_id>.json`:
visible host/allocation cgroup memory limits, current/peak values, OOM counters, outer instance state,
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
