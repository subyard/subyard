---
name: subyard-test-vms
description: Get a disposable VM with root from the operator's test-VM pool, run commands in it across separate calls, then release it.
---

[Delivery and owner setup](https://github.com/Subyard/Subyard/blob/master/docs/test-vms.md#pool-facts-for-agents).

Client (use the full path across separate calls; `--help` lists options):

```sh
bash /usr/local/share/subyard/skills/subyard-test-vms/client.sh status
```

`acquire --slot N --purpose short-label` prints a handle; pass it to
`exec HANDLE -- COMMAND [ARG...]` (root), then `release HANDLE` when done.
Handles under `~/.subyard/e2e/handles/` contain private route, host-key and lease credentials.
Acquisition defaults to one standard VM; attribution uses guest facts and the yard name as project.
Use `--image debian-13|ubuntu-24.04|ubuntu-26.04` to choose the lease OS (default Debian 13).

Slot states: `available`: acquire; `held` or `busy`: refusals show
yard, project, run, purpose and expiry;
`provisioning`, `draining`, `recovering`: retry later;
`capacity`: wait, optionally `acquire --slot N --wait 20m`;
`quarantined`, `stopped`, `legacy*`, `unavailable`: ask the operator.

Running `exec HANDLE -- true` resets the idle timer. `client.sh leases` lists handles;
release `lost`/`no-keeper` handles and acquire again.

The VM has root and public Internet egress; it cannot reach this yard, the broker,
other slots, the host or private networks. Only this client reaches it. Nothing in the VM survives
release or expiry; copy results out with `exec` first. The outer yard remains operator-owned.
Diagnostics: `client.sh logs --slot N -n 50` (own leases), `client.sh errors` (pool failures).
After any test-VM failure, an `E2E_QUARANTINE` line or slowness, read `client.sh errors`
and report the records and the printed operator command (`yard test-vms logs --slot N`).
A slot with `hold_until` is held after repeated failures: use another slot or wait; only the
operator lifts the hold (`yard test-vms recover --slot N`).
