---
name: subyard-test-vms
description: Get a disposable VM with root from the operator's test-VM pool, run commands in it across separate calls, then release it.
---

The opt-in `test-vms-client` profile installs this skill and client without a checkout
or coding-tool integration; roles with `ALLOWS_HOST_ACCESS=false` refuse it.
Removing it from `ENVIRONMENT_PROFILES` and reconciling retires the client/skill;
a held lease's keeper still releases it after idle expiry.
[Delivery and owner setup](https://github.com/Subyard/Subyard/blob/master/docs/test-vms.md#pool-facts-for-agents).

Client (use the full path across separate calls; `--help` lists options):

```sh
bash /usr/local/share/subyard/skills/subyard-test-vms/client.sh status
```

`acquire --slot N --purpose short-label` prints a handle; pass it to
`exec HANDLE -- COMMAND [ARG...]` (root), then `release HANDLE` when done.
Handles under `~/.subyard/e2e/handles/` contain private route, host-key and lease credentials.
Acquisition defaults to one standard VM; attribution uses guest facts and the yard name as project.

Slot states: `available`: acquire; `held` or `busy`: refusals show
yard, project, run, purpose and expiry;
`provisioning`, `draining`, `recovering`: retry later;
`capacity`: wait, optionally `acquire --slot N --wait 20m`;
`quarantined`, `stopped`, `legacy*`, `unavailable`: ask the operator.

A keeper renews every 5 minutes. Without `exec`, `--idle` (default 20m, maximum 2h)
releases the lease; `exec HANDLE -- true` resets idle time. If the keeper stops,
expiry is 20 minutes after the last renewal. `client.sh leases` lists handles;
release `lost`/`no-keeper` handles and acquire again.

The VM has root and public Internet egress; it cannot reach this yard, the broker,
other slots, the host or private networks. Only this client reaches it. Nothing in the VM survives
release or expiry; copy results out with `exec` first. The outer yard remains operator-owned.
Diagnostics: `client.sh logs --slot N -n 50`.
