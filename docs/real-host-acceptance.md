# E2E VM acceptance

The default `./tests/run.sh` is host-free. Live acceptance uses the disposable pool documented in
[Agent E2E VM pool](test-vms.md#agent-workflow); that guide is the source of truth for setup,
exact-slot selection, leases, recovery, and the outer-yard boundary. After choosing an available
slot from redacted status, pass it explicitly to the continuous P0 gate:

```sh
dev/agent-e2e.sh --status
slot=1
dev/e2e/p0-acceptance.sh --slot "$slot"
```

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

VM1 installs the candidate runtime, runs `init` twice, repairs a legacy fixture and verifies storage,
network, systemd, Incus container/VM and rollback behavior. VM2 runs the full suite and transport
contracts. Only these disposable VMs observe real KVM and kernel behavior.

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

CI and tagged releases use one prepared-context entrypoint for the exact pinned binaries, real
crypto and loopback OpenSSH contracts:

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
