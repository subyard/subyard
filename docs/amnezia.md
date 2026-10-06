# AmneziaVPN yard

The optional `amnezia` profile provides a dedicated named Incus VM for the stock AmneziaVPN app.
Subyard manages the VM, persistent storage, SSH administration route and VPN network boundary.
AmneziaVPN installs and manages the VPN server, containers, protocols and clients through SSH.
There is no browser administration panel or Subyard client registry.

## Create the environment

Use an amd64 owner with KVM, Incus 6.0.6 and a supported QEMU reporting implementation (8.2.x or
10.0.x). The preset uses Debian 13, with Ubuntu 24.04 as its image fallback, one virtual CPU,
1 GiB RAM, a 10 GiB root disk and a separate 2 GiB state block volume. It requests Free Page
Reporting and a pinned private IPv4. Coding integrations, work projects, host mounts, shared
credentials, agent forwarding and nested VM access are excluded.

Budget CPU and RAM for the VPN and owner alongside busy coding yards. Use the shared
[yard resource limits](configuration.md#yard-resource-limits) to cap those yards, including
`default`; `yard init` reconciles nonempty limits on existing instances. Choose limits for the
owner's capacity and workload. The VPN preset's limits are ceilings rather than reserved capacity.

```sh
yard -Y vpn init --profile amnezia
yard -Y vpn start
yard -Y vpn vpn status
yard -Y vpn status
```

Initialization prepares Docker, containerd, persistent storage and a dedicated SSH administrator.
It does not install a VPN server. Repeat `init --profile amnezia` to reconcile or retry environment
preparation. Plain `init` reconciles the base yard without selecting another environment profile.
If initialization added you to `incus-admin`, open a new login session before subsequent commands
so they receive that group membership.

Three ports have separate purposes:

| Setting | Default | Purpose |
| --- | --- | --- |
| `SSH_PORT` | TCP 2225 | Subyard's normal loopback SSH transport |
| `RESOURCE_VPN_ADMIN_PORT` | TCP 2226 | Owner IPv4 to guest SSH port 22, for AmneziaVPN |
| `RESOURCE_VPN_PORT` | UDP 51820 | Owner IPv4 to the same guest UDP port, for AmneziaWG |

The VPN resource forwards the configured UDP port; it does not assume the app's randomly chosen
port. Both public resource ports must be in `1024..65535` and unused on their exact owner address.
Provider firewalls must separately allow the configured UDP port and, while administering the
server, the configured TCP port.

Profile initialization and explicit provisioning fill missing owner IPv4/interface settings when
exactly one suitable public IPv4 is assigned to an active owner interface. Existing nonempty
settings and ports are preserved. No external IP-discovery service is used. If discovery is
ambiguous, supply the endpoint before starting the yard:

```sh
yard -Y vpn config set RESOURCE_VPN_IPV4 203.0.113.10 --scope yard
yard -Y vpn config set RESOURCE_VPN_INTERFACE eth0 --scope yard
yard -Y vpn config set RESOURCE_VPN_PORT 51820 --scope yard
yard -Y vpn config set RESOURCE_VPN_ADMIN_PORT 2226 --scope yard
yard -Y vpn start
```

Replace the documentation address and interface with the owner's real values. The address must
be assigned directly to an active owner interface; an upstream NAT address is not covered.
Private addresses are not selected automatically, although explicitly configured private IPv4
addresses are allowed for test owners. Source-managed configuration must receive settings through
its registered source.

Fresh initialization arms the UDP resource's first activation on `yard start`. That activation
assesses the endpoint and publishes the owned route after the operation's confirmation. It can
be ready before a VPN server has been installed. Later stop/start operations preserve the network
enablement choice. An explicit `vpn down` disables it persistently, including across repeated
provisioning and restarts; use `vpn up` to enable it again.

## Install and manage through AmneziaVPN

Open the administration route explicitly:

```sh
yard -Y vpn vpn-admin up
yard -Y vpn vpn-admin status
yard -Y vpn status
```

The selected yard's detailed status includes a hint such as
`Manage in AmneziaVPN: amnezia@203.0.113.10:2226`. It identifies the application, address, port and
user without exposing credentials. Resource status and RPC responses do not contain private keys.
Initialization adds the dedicated `amnezia` account to the VM's `sudo` group and grants it
passwordless administrative access, which the native installer requires. These rights apply
inside the VPN VM.

Export its private key into a protected file on the owner, without printing it:

```sh
install -d -m 0700 ./vpn-access
install -m 0600 /dev/null ./vpn-access/admin.key
incus exec yard-vpn --project subyard-vpn -- \
  cat /srv/amnezia/admin.key > ./vpn-access/admin.key
chmod 0600 ./vpn-access/admin.key
```

Use the instance and project names shown by `yard -Y vpn config show` if they differ. The export
uses the documented [Incus exec command](https://linuxcontainers.org/incus/docs/main/reference/manpages/incus/exec/).
Transfer the file to the administrator's device through a protected channel. This key grants full
administrative access to the dedicated VM; keep it out of repositories, issues, chat and logs.
Subyard generates an ED25519 key, supported by the
[native credentials screen](https://github.com/amnezia-vpn/amnezia-client/blob/5.0.3.0/client/ui/qml/Pages2/PageSetupWizardCredentials.qml).
When entering it in the app, include the complete key with its BEGIN and END lines.

Use the stock [AmneziaVPN app](https://github.com/amnezia-vpn/amnezia-client/releases/tag/5.0.3.0),
with 5.0.3.0 as this profile's acceptance reference:

1. Add a **Self-hosted VPN** connection.
2. Enter the owner's advertised IPv4 and administration port, for example `203.0.113.10:2226`,
   username `amnezia`, and the exported SSH private key.
3. Choose **Manual**, select **AmneziaWG**, and set its UDP port to `RESOURCE_VPN_PORT`, normally
   `51820`, before installing.
4. Let the app install the server and create its administrator connection.

The upstream [self-hosted setup guide](https://docs.amnezia.org/documentation/instructions/install-vpn-on-server/)
explains the SSH workflow. The
[5.0.3.0 protocol settings screen](https://github.com/amnezia-vpn/amnezia-client/blob/5.0.3.0/client/ui/qml/Pages2/PageSetupWizardProtocolSettings.qml)
supports choosing the port before installation. Automatic setup chooses a random AmneziaWG port,
which will not necessarily match Subyard's route. Use the owner's advertised address rather than
the guest's private address: the native app also uses this host address for client configurations.

After setup, close the administration route when it is not needed:

```sh
yard -Y vpn vpn-admin down
```

This closes public SSH administration while VPN traffic can continue through the separate UDP
route. Reopen it before managing clients, checking the server or updating protocols. If adding
an existing native server to another administrator's app, enter the same credentials and choose
**Skip setup**, then **Management** → **Check the server for previously installed Amnezia services**;
see [native server discovery](https://docs.amnezia.org/documentation/instructions/check-server/).

## Share and revoke clients

Use the app's **Share VPN Access** screen to create and name a separate guest client for each
device. Select the server, AmneziaWG protocol and a format supported by the recipient. The app can
share an AmneziaVPN key/file or export a native AmneziaWG `.conf` file. Treat exported access as a
secret and keep local files in a private directory with mode `0600`.

To remove access, open **Users**, select the named client, choose **Revoke**, and confirm with **Continue**.
This removes that client's peer while retaining the other clients. Creating, sharing and revoking
clients are native app operations; Subyard does not generate a first client or edit the app's peer
registry. See the official [sharing and revocation guide](https://docs.amnezia.org/documentation/instructions/share-connection/).

Subyard's network boundary blocks decrypted VPN traffic to external private, loopback, metadata
and special-use networks, the owner's configured endpoint and VM management services. IPv6
traffic cannot leave the VM through its uplink. The native app owns client routes, DNS and protocol settings;
client-side split routing or disconnecting the tunnel changes what traffic reaches this boundary.
Owner SSH and neighboring yards retain their existing network policy.

## Status and network control

```sh
yard -Y vpn vpn status
yard -Y vpn vpn-admin status
```

The resource's JSON distinguishes environment/network readiness from native VPN installation:

| Field | Meaning |
| --- | --- |
| `ready` | The selected resource's environment and owned route are ready |
| `network_enabled` | The VPN network boundary is enabled |
| `ingress` | The selected resource's owned ingress device exists |
| `vpn_installed` | A recognized native AmneziaWG container exists |
| `vpn_running` | A native container is running and listening on the configured UDP port |
| `management` | AmneziaVPN application, host, administration port and username |
| `vpn_port` | The configured VPN UDP port |

An installed or running container does not by itself prove a working client connection. In
particular, the app's container can remain running while `vpn down` has disabled the network
boundary. A ready administration route does not imply that VPN installation has completed.

```sh
yard -Y vpn vpn down
yard -Y vpn vpn-admin down
yard -Y vpn stop
```

`vpn down` removes the owned UDP ingress and disables VPN forwarding in the guest. Native
containers, images, keys and peers remain app-owned and are retained. `vpn-admin down` removes
only the administration route. Stopping the yard stops the VM and its processes. If the guest
agent is unavailable, VPN shutdown still closes owned ingress and retains pending boundary
cleanup; retry `vpn down` when guest access returns. Removing a UDP proxy alone does not prove
that an established tunnel has terminated before guest cleanup converges.

Close both routes before changing their endpoint settings or deselecting the profile. `vpn up`
and `vpn-admin up` reject endpoint collisions and foreign ownership metadata. Changing the UDP
port also requires changing it in AmneziaVPN and may require exporting updated client access.
A failed bring-up can be retried without replacing the native server or its clients.

VM boot follows Subyard's managed desired-power workflow and host network guards. No independent
Incus autostart bypass is installed. Stopping another yard does not stop this VM. Free Page
Reporting returns idle memory to the immediate owner without reserving RAM or CPU priority.

## Update, backup and recovery

AmneziaVPN owns installation and updates of its server containers and images. Subyard does not
pin or replace them during environment provisioning. Repeating profile initialization preserves
native server state and the administrator key. Check free space before native image updates: the
preset's separate state volume is 2 GiB, and existing volumes are not automatically enlarged.

The environment sets public DNS resolvers for Docker image builds. Native container networks may
communicate inside the dedicated VM, including Amnezia's DNS sidecar. Container traffic cannot
reach VM-local management services or external private networks through either Docker bridge.
Amnezia continues to own client DNS settings.

Persistent Docker and containerd storage lives under `/srv/amnezia/docker` and
`/srv/amnezia/containerd`. Native configuration remains in the app's container storage, rather
than a Subyard client configuration file. Docker and containerd require the dedicated `/srv`
volume; if it is unavailable, environment preparation refuses to create replacement state on
the root disk. Repair the mount through `yard -Y vpn init` before retrying.

Back up the entire `/srv/amnezia` state with the VM stopped, using an encrypted backup tool and
a destination outside the VPS. Preserve ownership and permissions, including root ownership,
mode `0700` on the state root and `0600` on the administrator key. Preserve the internal Docker
and containerd permissions; do not apply those modes recursively. Keep non-secret endpoint
settings and the administrator app's own backup with the recovery instructions.

Restore a consistent state volume through the normal Incus/storage recovery workflow while the
VM is stopped. Reconcile the dedicated profile, reopen administration, and check the restored
server in AmneziaVPN before enabling the VPN route and verifying clients. Do not merge independently initialized
state directories. `teardown --keep-data` retains the state volume; destructive teardown can
remove it and requires its existing confirmation.

State created by the old Subyard-managed custom VPN runtime is refused. Use a fresh dedicated
named yard for the native app environment. There is no automatic conversion or implicit removal
of existing users; retain the old environment and backup until its replacement is verified and
you have decided how to retire it.

The owner IP, uplink, physical memory and outage boundary remain shared with neighboring yards.
VM limits do not guarantee latency, bandwidth, provider filtering behavior or recovery from loss
of the VPS without an external backup.

## Acceptance check

On an available allocated two-VM test slot, run:

```sh
config/profiles/amnezia/tests/e2e/acceptance.sh --slot N --lane full
```

The acceptance workflow runs stock AmneziaVPN 5.0.3.0 under a virtual display on the second test
VM. Through its GUI, it installs the server over dedicated SSH using Manual AmneziaWG and the
configured port, creates two named clients, exports their native configurations, and revokes one
while the other retains access. It also closes/reopens administration and rediscovers the native
server. Independent compatible clients probe the exported configurations.

The lifecycle checks cover repeated environment preparation, network shutdown/re-enablement,
private/management-network denial, isolation, state preservation, neighboring workload behavior,
VM and owner reboot recovery, unavailable guest-agent cleanup, mount recovery and Free Page
Reporting cycles. Credentials remain in protected files inside the disposable lease. This describes
acceptance coverage, not a recorded passing run; it does not establish provider filtering behavior
or production VPS performance. See [test VM ownership and allocation](test-vms.md).
