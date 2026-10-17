# Security boundary

Subyard's default yard is a restricted, unprivileged Incus container. This separates the yard's
users, files, processes, devices, and managed network from the owner host while mapping container
root to an unprivileged host identity.

A container is not a virtual machine: it runs on the owner host's Linux kernel. Unprivileged Incus
policy reduces what a compromised yard process can do to that kernel, but it does not create a
separate guest kernel. Subyard also supports an explicitly configured Incus VM yard when that
separate guest-kernel boundary is needed.

Run the audit at any time:

```bash
yard security
```

The command checks the declared configuration and, when Incus is reachable, the live project,
instance, devices, proxies, and relevant syscall policy. If live state is unavailable it reports a
static-only result rather than claiming to have checked the running boundary. Use
`yard security --require-live` when absence of live evidence must fail the audit.

## Host files and control sockets

Managed host mounts must stay under the selected yard's `HOST_BASE`. Named yards derive separate
host-mount roots. Subyard rejects `/` as a managed in-yard mount target and rejects Docker, Incus,
and LXD control sockets in both yard and project-environment mount declarations.

`yard bind PATH` is the deliberate exception to the managed-root rule. It attaches exactly the
selected local host directory as an Incus disk at that project's workspace. Yard processes can then
change the host files in that directory, so the command warns that encapsulation is reduced. Bind is
local-only; a remote yard must use `sync` or `clone`.

Do not manually attach host control sockets or broad host paths to a yard. Such unmanaged devices
can bypass the policy that `yard security` is designed to verify.

## SSH credentials

Opt-in SSH agent forwarding permits git push and other host access from inside the yard
using a forwarded, write-enabled credential while the SSH session is active.
No private key is copied into the yard, but any process that can reach the forwarded agent
can use that credential; agent ask-rules are a UX safeguard, not a security boundary.

The managed [per-yard SSH agent](ssh-agent.md) narrows this grant to one selected yard and one
encrypted key with a required lifetime of at most 24 hours. It still delegates signing capability to
processes running as the yard's developer user until the grant expires or is locked. Locking prevents
new authentication but cannot terminate connections or transfers that are already established.

Subyard protects the owner-host boundary. It does not isolate credentials between agents or
processes operating as the same user inside one yard. Use separate yards when workloads must not
share a delegated credential or other in-yard state.

## Project environments

A project targeting an active environment profile runs in its own L2 Docker container inside
the yard. The runner uses the yard developer's non-root UID/GID and enables `no-new-privileges`;
older containers need an explicit rebuild before they can start under this policy. Images must
support that identity and provide any required toolchain and writable home. The profile's
workspace and protected input file are mounted only into that project's container. The input
file has mode `0600`, is read-only inside L2, and its contents do not enter environment variables
or the public manifest.

Writable `CACHES` entries must be clean directories below `/srv/cache/`. Existing paths with
symlink ancestors or another UID/GID are refused before container creation or replacement.
Installed dependencies such as `node_modules`, virtual environments and Docker runtime data
remain private to the project. `ENV_MOUNTS` accepts only explicit read-only dependency/toolchain
assets below `/srv/cache/`, `/opt/`, `/usr/local/` or `/usr/lib/`; resolved sources cannot escape
those roots. Reserved project, runtime and credential paths cannot be replaced by these mounts.
Declarations naming Docker, Incus or LXD control sockets are rejected through both mount paths.

Choose the execution boundary to match the workload:

- The fast lane uses project containers for cooperating builds and ordinary tests. Shared
  dependency caches are disposable shared state. OpenClaw's supported wrappers and participating
  images coordinate operations through the same lock; custom tools must use its `sy-cache`
  helper or keep private caches. A writable shared cache is not protection from deliberate deletion.
- Serialized operations use the owning cache lock or [resource lease](reference/resource-leases.md).
  Destructive maintenance excludes installs, and stale leases cannot release or stop a successor.
  SDK maintenance and emulator state remain under their owning profile's locks and lifecycle.
- Tests requiring hostile-code, kernel, user-namespace or stronger network/credential isolation
  use a separately leased [disposable VM](test-vms.md), with private state and explicit grants.

L2 containers use the yard Docker bridge. There is no general L2-to-L1 egress firewall contract;
the host NetworkManager guard and inter-yard network policy protect different boundaries.
Session history is deliberately shared where configured and must not contain credential files.
Neither a shared session store nor a cache is a secret store. These restrictions do not strengthen
the same-user L1 credential boundary described above.

Selected static staging and QA secrets belong in the [host-side encrypted credential ledger](keys.md).
The ledger root stays outside repositories, `HOST_BASE`, and every managed yard mount. Its whole store
is never mounted into a yard; only files authorized for the selected consumer context are
materialized. It intentionally does not manage coding-agent OAuth or session stores.

## Nested test VMs

A trusted yard with the `test-vms` profile can opt in to a root-owned pool of nested Incus VMs. This
widens the yard's device and syscall boundary enough for that dedicated facility, but does not give
the yard the physical owner's Incus socket or L0 management access.

Agents receive only a broker lease for one explicit slot and its retained VM pair. Lease release or
expiry revokes forwarding and guest keys, trims and stops the pair, and preserves its disks for the
next lease. Confirmed pool reconfiguration or outer-yard teardown can remove retained pairs;
verified quarantine recovery deletes and rebuilds a failed pair. See
[Agent E2E VM pool](test-vms.md) for the exact ownership, fencing, network, lifecycle, and cleanup
contract.
