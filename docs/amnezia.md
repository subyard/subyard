# Amnezia VPN yard

The optional `amnezia` profile runs AmneziaWG in a dedicated named Incus VM. Ordinary installation,
update, status and provisioning do not select it. The core supplies VM, storage, profile and typed
resource APIs; the profile owns its VPN runtime, firewall and lifecycle.

## Create and enable

Use an amd64 owner with KVM, Incus 6.0.6 and a supported QEMU reporting implementation (8.2.x or
10.0.x). The preset uses Debian 13, one virtual CPU, 1 GiB RAM, a 10 GiB root disk and a separate
2 GiB state block volume. It requests Free Page Reporting and a pinned private IPv4. It excludes
coding integrations, work projects, host mounts, shared credentials, agent forwarding and nested
VM access. The SSH port defaults to 2225; select another unused port if necessary.

```sh
yard -Y vpn init --profile amnezia
```

This initializes the dedicated VM and provisions the selected profile, including the pinned VPN
runtime. A separate `provision amnezia` is not needed. Repeat `init --profile amnezia` to retry an
interrupted installation. Plain `init` continues to reconcile the base yard without installing
environment-profile toolchains.

If this first initialization added you to `incus-admin`, open a new login session before
continuing so subsequent commands receive that group membership. Then start the yard:

```sh
yard -Y vpn start
yard -Y vpn vpn status
```

Profile initialization and explicit provisioning fill missing owner IPv4/interface settings when
exactly one suitable public IPv4 is assigned to an active owner interface. The proposed settings
appear in the operation assessment and are saved together. Existing nonempty settings and the
port are preserved. Repeating provisioning does not switch a saved endpoint.
No external IP-discovery service is used: an upstream NAT address is not a local owner address.

Fresh initialization and provisioning leave the VPN stopped and arm its first activation on `yard start`.
The first start assesses the exact public endpoint and service effects before one confirmation;
it enables the guest service and publishes the owned UDP route. Later stops and starts preserve
the enabled service. An explicit `vpn down` disables it persistently, including across repeated
provisioning and yard restarts; use `vpn up` to enable it again.

If there is no unique public address/interface, provisioning leaves the missing settings empty
and prints which settings to supply. Set them before the first `yard start`. Specify an interface
to narrow discovery, or an exact owner IPv4 to discover its interface, then rerun provisioning.
Private and special-use addresses are never selected automatically; an explicitly supplied private IPv4 is allowed for test owners.
For a manual endpoint or a different port:

```sh
yard -Y vpn config set RESOURCE_VPN_IPV4 203.0.113.10 --scope yard
yard -Y vpn config set RESOURCE_VPN_INTERFACE eth0 --scope yard
yard -Y vpn config set RESOURCE_VPN_PORT 51820 --scope yard
yard -Y vpn start
```

Replace the documentation address and interface with the owner's real values. The advertised
address must be assigned directly to that interface; a VPS behind another NAT is not covered.
Source-managed configuration must receive endpoint settings through its registered source.
Provider firewalls must independently permit that exact UDP port. First activation and explicit
`vpn up` reject socket/proxy/forward collisions and foreign ownership metadata. Repeating them
preserves keys and peers. If activation fails after the VM starts, fix the reported cause and
retry `yard start`; the failed first activation remains pending.

The profile uses the upstream AmneziaWG image pinned in
[`release.env`](../config/profiles/amnezia/release.env), with AmneziaVPN 5.0.3.0 as the protocol
reference. It does not give the Amnezia app SSH administration of the owner or install its remote
server manager. Subyard is the single writer of this runtime; use the app to import client access.

## Client access and network policy

First enablement creates one client at `/srv/amnezia/client.conf` inside the VM. This file contains
private keys. Export it on the owner into a private directory, without printing it to the terminal:

```sh
install -d -m 0700 ./vpn-access
install -m 0600 /dev/null ./vpn-access/client.conf
incus exec yard-vpn --project subyard-vpn -- \
  cat /srv/amnezia/client.conf > ./vpn-access/client.conf
```

Use the instance and project names shown by `yard -Y vpn config show` if they differ. Transfer the
file through a protected channel. In a compatible AmneziaVPN client, add a connection, choose
**Connection settings file**, select `client.conf`, and connect; see the upstream
[file import instructions](https://docs.amnezia.org/documentation/instructions/connect-via-config/).
Do not put it in a repository, issue, chat, log or shared clipboard. This first
version manages one client; concurrent devices need distinct peer addresses and are not supported
by the resource command.

The client routes IPv4 and IPv6 into the tunnel. The service forwards IPv4 Internet traffic, uses
MTU 1280 and supplies public DNS addresses. IPv6 is intentionally dropped inside the tunnel.
Client-side split routing or disabling the tunnel changes this protection. Decrypted traffic is
blocked from private, loopback, metadata and special-use networks, from the owner's endpoint and
from VM management services. ICMP echo to the tunnel address is allowed for diagnostics.
Owner SSH and other yards retain their existing network policy.

## Stop, update and recover

```sh
yard -Y vpn vpn down
yard -Y vpn vpn status
```

`down` removes the owned public ingress before disabling the guest service. It retains the entire
state directory. If the guest agent is unavailable, `down` still closes an owned ingress and retains
pending cleanup; retry `down` when guest access returns to verify shutdown. Start the yard first if
it is stopped. To change the endpoint or deselect the profile, run `down` first; configuration
commands reject changes while its service remains enabled.
If a profile was manually deselected, `init` assesses and closes its exact owned ingress and calls
the profile's declared shutdown. If guest shutdown cannot be verified, it retains cleanup intent
and stops reconciliation with recovery instructions. Removing a UDP proxy alone does not terminate
an established tunnel.
A failed bring-up rollback also disables the guest service while preserving keys and peers; run
`up` again to re-enable it. Bring-up also clears stale untranslated UDP connection state for the
verified endpoint, allowing an existing client to reconnect after a disabled interval. This host
step may require sudo authentication after the operation confirmation.

Guest service enablement survives a restart. A disabled service stays disabled. VM boot itself
follows Subyard's managed desired-power workflow and host network guards; no independent Incus
autostart bypass is installed. Stopping another yard does not stop this VM. Free Page Reporting
returns idle guest memory to the immediate owner but does not reserve RAM or CPU priority.

The VPN service requires the dedicated `/srv` state volume to be mounted. If it is unavailable,
bring-up stops without creating replacement keys on the VM root disk; repair the mount through
`yard -Y vpn init` before retrying `vpn up`.

For a runtime update: protect a current state backup, run `vpn down`, update the Subyard runtime,
run `yard -Y vpn init --profile amnezia`, then `vpn up`. The image digest changes
only with the profile release. Provisioning preserves service enablement and keys; it does not run
an independent upstream updater. A service already running an older image requires explicit `up`
to replace it.

Back up `/srv/amnezia` while the service is down, using an encrypted backup tool and a destination
outside the VPS. Preserve root ownership, directory mode `0700` and file mode `0600`. Keep the
matching pinned profile version and non-secret endpoint settings with the recovery instructions.
For recovery, initialize a fresh dedicated VM and disable its pending startup with `vpn down`.
Start the VM, restore that directory and the endpoint settings, then run `vpn up` and verify the client. Do not restore onto an active
service or merge two independently generated state directories. `teardown --keep-data` retains the
state volume; ordinary destructive teardown can remove it and requires its existing confirmation.

The owner IP, uplink, physical memory and outage boundary remain shared with neighboring yards.
Limits bound this VM; they do not guarantee latency, bandwidth, provider filtering behavior or
recovery from loss of the VPS without an external backup.

## Acceptance check

On an available allocated two-VM test slot, run:

```sh
config/profiles/amnezia/tests/e2e/acceptance.sh --slot N
```

The check creates a dedicated VPN VM on the first test host and a compatible upstream AmneziaWG
client on the second. It verifies handshake, DNS, MTU, Internet transfer, management/private-network
denial, isolation, state preservation, a neighboring workload, enabled and disabled reboot recovery,
and two allocation/free RSS cycles before and after owner reboot. The RSS check keeps guest RAM
unchanged and uses distinct pages to exclude deduplication. Test credentials stay in private files
inside the disposable lease. This does not test the graphical client, provider filtering or production
VPS performance. See [test VM ownership and allocation](test-vms.md).

Use `--lane reboot` for a focused fresh check of enabled and disabled restart/reboot recovery,
client connectivity, and two Free Page Reporting cycles after reboot. It creates its own fixtures
and does not reuse a released lease. Both lanes keep client traffic active during enabled owner
reboots and verify recovery with isolation enabled and disabled.

Use `--lane recovery` for a focused fresh check of ingress closure while the guest agent is
unavailable, shutdown retry, and refusal to create state on an unmounted `/srv`. It also checks
systemd mount restoration, unchanged keys and peers, and client connectivity after recovery.

Use `--lane startup` to check the shortened `init --profile` → `start` workflow, repeated profile
initialization, client connectivity, and enabled/explicitly disabled stop/start and host reboot
recovery. This lane omits the workload and Free Page Reporting measurements.

Use `--lane disabled` for a fresh first activation with isolation already enabled, followed by
repeated shutdown, disabled stop/start/provision/reboot recovery, and explicit re-enablement.

Use `--lane reconnect` for first activation with isolation enabled, then shutdown and an owner
reboot with retained client traffic. It verifies a stale untranslated UDP flow exists before
explicit bring-up and that the same client reconnects without replacing its configuration.
