# E2E VM acceptance

The default `./tests/run.sh` is host-free. Live acceptance uses the disposable pool documented in
[Agent E2E VM pool](test-vms.md#agent-workflow); that guide is the source of truth for setup,
exact-slot selection, leases, recovery, and the outer-yard boundary. Choose checks using
[Subyard dev-flow](../.agents/skills/subyard-dev-flow/SKILL.md#choose-checks-by-risk).
To run a release smoke, select an available slot from redacted status and pass it explicitly:

```sh
dev/agent-e2e.sh --status
slot=1
dev/e2e/p0-acceptance.sh --slot "$slot"
```

GitHub workflows do not access the VM pool. The full matrix command is
`dev/e2e/p0-acceptance.sh --slot "$slot" --lane full`; the skill defines when smoke or full coverage applies.

Never run these checks on the operator host, in the privileged outer yard, or in a working yard.

## Official Incus client contract

Inside an allocated E2E VM, the server/extensions half can be checked without creating an instance:

```sh
SUBYARD_REAL_INCUS_SOCKET=/var/lib/incus/unix.socket \
go test -tags realincus ./internal/adapters/incusclient -run '^TestRealIncusServerContract$'
```

The full acceptance runner creates its own marked container and VM, then runs:

```sh
SUBYARD_REAL_INCUS_SOCKET=/var/lib/incus/unix.socket \
SUBYARD_REAL_INCUS_CONTAINER_PROJECT=subyard-e2e-container \
SUBYARD_REAL_INCUS_CONTAINER_INSTANCE=yard-e2e-container \
SUBYARD_REAL_INCUS_VM_PROJECT=subyard-e2e-vm \
SUBYARD_REAL_INCUS_VM_INSTANCE=yard-e2e-vm \
bash tests/real-host/incus-contract.sh
```

This checks the same server, instance, async exec, stdio-flush, operation-event delivery and
event-cancellation semantics covered by the fake Unix/WebSocket server. It executes only `printf`
inside each selected running instance.

## Versioned configuration source

Run the shared-only source gate on an allocated VM:

```sh
dev/agent-e2e.sh --slot "$slot" --vm 1 -- bash ./dev/e2e/config-source-shared-only.sh
```

The gate uses a marked temporary root and a loopback OpenSSH Git remote. It verifies declined and
confirmed onboarding, local HostID persistence, shared and host provenance, explicit external Git
transport, idempotent sync and safe host-overlay removal. Its trap removes the Git remote, checkout,
live configuration and SSH process; it never changes the outer yard lifecycle.

## Platform and release checks

Temporary shared SSH-agent access has a focused disposable-host check:

```sh
dev/agent-e2e.sh --slot "$slot" --purpose orca-ssh-agent --vm 1 -- \
  env SUBYARD_E2E_ORCA_BOOTSTRAP=1 SUBYARD_E2E_ORCA_SSH_AGENT=1 \
  bash tests/real-host/orca-bootstrap.sh
```

It unlocks a generated encrypted key through a real terminal and checks Git pushes
from a yard and an already-open paired Orca terminal, cross-yard isolation,
rejected agent mutations, wrong passphrases, cancellation, explicit revocation and
expiry. Grants do not restart Orca. See [test VMs](test-vms.md) for fixture scope
and bounded debugging options. Reconnect deadline and worker failure behavior
also have focused runtime tests with real OpenSSH agents.

The default release smoke checks the pool boundary and disconnect handling, exercises real Incus on
VM1, then upgrades a checksum-pinned published v0.14.0 runtime to the candidate. It runs candidate
`init` twice, preserves operator and guest data across one reboot, exports the changed project,
rolls back and forward, and tears the yard down without removing the installed runtime. Its scoped
two-owner peer phase starts a fresh candidate yard twice and exercises real remote projects and RPC;
offline recovery and credential exchange remain in the full peer phase. Cleanup and a final boundary
check close the run.

The explicit `--lane full` matrix remains the exhaustive compatibility and recovery run. It retains
historical migration, broker, nested teardown, source upgrade, power/systemd and full peer scenarios,
and includes the same release-smoke phase before peer acceptance. It assumes the host-free core,
loopback SSH/crypto and engine-release contracts have already passed, so it does not repeat them on
the VMs. Only these disposable VMs observe real KVM and kernel behavior.

Exercise a synthetic project through `sync`, ordinary TTL-refreshed `list`, forced `list --live`,
`shell`, `export`, and `remove`; test an
active profile resource through bring-up/status/shutdown. Android emulator process checks must stay
user-scoped and argv-anchored.

The host-free `tests/engine-release.sh` proves engine and full-runtime checksums/provenance,
offline and incomplete-download behavior, atomic upgrade/rollback layout, stdio half-close and
supported/unsupported protocol negotiation. On the E2E lane, install two versioned runtimes on VM1,
connect from VM2 over SSH stdio, upgrade the owner while the controller stays on the previous
version, and then run `yard update --rollback`. The upgrade path
is covered by the [source-upgrade lane](../dev/e2e/p0-source-upgrade.sh): pre-0.1 and v0.1 paths,
reboots, old-path removal, and rollback/roll-forward for default and named yards.

If guarded yard restoration fails during an owner-host boot, inspect the bounded host-side journal
before running `yard init` or manually starting the yard:

```sh
sudo journalctl -b -u subyard-power-reconcile.service --no-pager -n 200
```

The reconciler keeps Incus autostart disabled and uses bounded systemd retries for transient Incus,
storage, or host-network readiness failures. A persistent failure remains failed and visible in this
journal instead of bypassing the route guards.

Use two synthetic credential peers to exercise pinned SOPS/age tooling and the real SSH path:
reciprocal trust, a shared record, an exclusive assignment move, sync, materialization and revoke.
Also exercise a disposable remote yard through its real SSH identity and RPC transport. Repeat cold
CLI startup, idle RPC RSS/CPU, snapshot latency and package-size measurements; compare them with the
host-free baseline in `docs/development.md`. Record results outside the public repository without
host names, credentials or payloads.

Branch CI and tagged Release workflows use one prepared-context entrypoint for the exact pinned
binaries, real crypto and loopback OpenSSH contracts:

```sh
bash tests/real-host/adapter-contracts.sh
```

The entrypoint creates a complete temporary engine context, installs the versions and checksums
pinned in `config/host.env`, and removes its operator/config/data roots on exit. Its fixtures create
only temporary synthetic peer ledgers, check that plaintext never enters them, decrypt through the
second peer and verify revoke materialization. They do not replace the real SSH
peer/exclusive-handoff check.

If OpenSSH server is installed, a non-privileged loopback gate verifies the real SSH handshake,
temporary host/client keys, strict host-key checking and the framed RPC stream without touching the
system daemon.

This closes the OpenSSH transport implementation itself; the two-E2E-VM run remains responsible
for routing, disconnect and exclusive-handoff behavior across a real host boundary.

The same ephemeral server also exercises real credential Git/SSH exchange together with the pinned
age/SOPS binaries.

It verifies reciprocal trust roles, the retained SSH route, signed encrypted sync, remote decrypt,
plaintext isolation and revoke. The two-E2E-VM lane still verifies host identity separation,
failure/reconnect and an exclusive handoff with real consumers.

## GitHub broker

The focused fixture uses a synthetic App key and never mints a real GitHub token. The controller
keeps one exact slot lease while it prepares, reboots, verifies recovery, and cleans up the VM.

```sh
dev/e2e/github-broker.sh --slot "$slot"
```

Add `--hermes` to run the broader Hermes profile fixture under the same lease; `--wait 60m`
extends lease acquisition when the pool is busy.

This checks profile defaults, persistent owner service, SSH reconnect, crash recovery, reboot,
explicit disable and preservation of Hermes state. Real GitHub App permissions and token use
still require an operator-configured installation and an authorized sandbox repository.
