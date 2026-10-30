# Amnezia profile checks

These tests belong to the profile. The Python suite checks the owner handler and
guest runtime; the Go tests load the shipped preset and resource descriptor through
Subyard's configuration and resource APIs.

From the repository root:

```sh
bash config/profiles/amnezia/tests/run.sh
config/profiles/amnezia/tests/e2e/acceptance.sh --slot N --lane full
config/profiles/amnezia/tests/e2e/acceptance.sh --slot N --lane recovery
config/profiles/amnezia/tests/e2e/acceptance.sh --slot N --lane resources
config/profiles/amnezia/tests/e2e/acceptance.sh --slot N --lane setup
config/profiles/amnezia/tests/e2e/acceptance.sh --slot N --lane share
```

Live checks use fresh allocated test VMs and the shared `dev/agent-e2e.sh` lease
API. Follow [the VM guide](../../../../docs/test-vms.md) before running them.
The `setup` diagnostic stops after the stock app installs its native server, before
sharing clients. It keeps the ordinary 360-second installation completion gate;
failures retain numeric native error codes and bounded owned-server state.
The `share` diagnostic also exports the original two clients and checks their
distinct identities, reopens administration, scans the native server and verifies
the retained second user, then closes the app before image transfer or traffic probes.
Export failures retain fixed stage/action flags and bounded control counts, without
UI text or configuration values. The ordinary export deadlines remain unchanged.
For navigation diagnosis, `reconnect` reaches disabled owner reboot, explicit up
and discovery without the full resource, load and recovery prefix. It performs
two discoveries; the full lane performs four. Whether the two additional earlier
reopens affect the late GUI state remains unconfirmed, so this diagnostic does
not reproduce the full prior navigation history.

For a separate administrative idempotence diagnostic, one leased owner can run
the existing `owner.sh` phases `bootstrap`, `init-isolated`, then `admin-down-check`
against the same candidate bundle. The last phase removes administrative ingress,
waits for the running fixture's agent after any ACL restart, then repeats the
same down command. It does not require native VPN installation or a GUI client.
The ordinary `admin-down` phase additionally verifies the running VPN afterward;
public administrative down continues to support stopped or agentless yards.
The shared Free Page Reporting probe remains under `dev/e2e/` because it tests a
generic VM capability. See [Amnezia acceptance coverage](../../../../docs/amnezia.md#acceptance-check).

`bash dev/test-profiles.sh` discovers profile host-free runners for CI. Files under
profile `tests/` directories are never included in runtime release bundles.
`bash dev/test-profiles.sh --e2e --slot N` discovers profile live entrypoints instead.
