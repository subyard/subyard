---
name: subyard-test-vms
description: Get a disposable VM with root from the operator's test-VM pool, run commands in it across separate calls, then release it.
---

# Test VMs

## Availability

The opt-in `test-vms-client` profile delivers this client and skill without a Subyard checkout
or coding-tool integration. Owner-side `yard -Y NAME provision test-vms-client` adds the profile
to the yard's local selection. A role with `ALLOWS_HOST_ACCESS=false` refuses it; other yards
receive the read-only route registry. Run the client as the ordinary `dev` user without sudo.

The client and skill live together in `/usr/local/share/subyard/skills/subyard-test-vms/`.
Static skill refresh updates them and their Codex, Claude Code, OpenCode, pi and Hermes discovery
links. Removing the profile from `ENVIRONMENT_PROFILES` and reconciling the yard retires them;
a held lease's running keeper still releases it after the idle limit.

## Use

Client (repeat the full path; every call is a separate process):

```sh
bash /usr/local/share/subyard/skills/subyard-test-vms/client.sh status
```

A **slot** is one place in the operator's pool. A **lease** gives you fresh VMs in that slot until
you release it. `acquire` prints a **handle**; pass it to later calls. Handles are private
directories under `~/.subyard/e2e/handles/` holding the captured route, host-key pin and lease
credentials; keep their contents private. Attribution uses the yard's guest facts and defaults
the project to the yard name. Acquisition defaults to one standard VM.

1. `... client.sh status`: choose a slot whose STATE is `available`.
2. `... client.sh acquire --slot N --purpose short-label`: prints the handle.
3. `... client.sh exec HANDLE -- COMMAND [ARG...]`: runs as root in the VM; repeat freely.
   Add `--stdin` to send input, e.g. `exec HANDLE --stdin -- sh -c 'cat > /root/f' < f`.
4. `... client.sh release HANDLE`: always release when done.

Slot states and refusals:

- `available`: acquire it.
- `held` or `busy`: taken; status and the refusal show yard, project, run, purpose and expiry.
- `provisioning`, `draining`, `recovering`: wait and retry.
- `capacity` (memory or disk): wait, or `acquire --slot N --wait 20m`.
- `quarantined`, `stopped`, `legacy*`, `unavailable`: ask the operator.

Lifetime: a keeper renews the lease every 5 minutes. After `--idle` (default 20m, at most 2h)
without `exec` it releases the lease; `exec HANDLE -- true` resets that timer. If the keeper stops,
the broker expires the lease 20 minutes after the last renewal. `client.sh leases` lists your
handles; release a `lost` or `no-keeper` one and acquire again.

Reach: the VM has root and public Internet egress. It cannot reach this yard, the broker, other
slots, the host or private networks. Only this client reaches the VM. Nothing in the VM survives
release or expiry; copy results out with `exec` first.

Diagnostics: `client.sh logs --slot N -n 50`.
