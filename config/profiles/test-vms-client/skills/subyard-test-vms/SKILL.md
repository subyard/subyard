---
name: subyard-test-vms
description: Get a disposable VM with root from the operator's test-VM pool, run commands in it across separate calls, then release it.
---

# Test VMs

Client (repeat the full path; every call is a separate process):

```sh
bash /usr/local/share/subyard/skills/subyard-test-vms/client.sh status
```

A **slot** is one place in the operator's pool. A **lease** gives you fresh VMs in that slot until
you release it. `acquire` prints a **handle**; pass it to later calls.

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

Lifetime: a keeper renews the lease every 5 minutes. After `--idle` (default 30m) without `exec`
it releases the lease. If the keeper stops, the broker expires the lease 20 minutes after the last
renewal. `client.sh leases` lists your handles; release a `lost` or `no-keeper` one and acquire again.

Reach: the VM has root and public Internet egress. It cannot reach this yard, the broker, other
slots, the host or private networks. Only this client reaches the VM. Nothing in the VM survives
release or expiry; copy results out with `exec` first.

Diagnostics: `client.sh logs --slot N -n 50`.
