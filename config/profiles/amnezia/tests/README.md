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
config/profiles/amnezia/tests/e2e/acceptance.sh --slot N --lane maintenance
config/profiles/amnezia/tests/e2e/acceptance.sh --slot N --lane setup
config/profiles/amnezia/tests/e2e/acceptance.sh --slot N --lane share
```

Live checks use fresh allocated test VMs and the shared `dev/agent-e2e.sh` lease
API. Follow [the VM guide](../../../../docs/test-vms.md) before running them.
The fixture turns off the stock app's own "Check for updates automatically" setting
before every launch, so upstream publication cannot replace its pages; a visible update
page fails immediately with that reason. A GUI failure after the app registers for
accessibility leaves `native-failure.tar` in the lane's evidence directory: a screenshot
unless a page can show a client configuration or key or its controls could not be read,
control roles with fixture-known labels only, and the saved-server count. A failure before
that (the pre-launch settings refusal, a registration timeout, an update page seen while
registering) leaves that archive empty, with its reason in the lane log.
The `setup` diagnostic stops after the stock app installs its native server, before
sharing clients. It keeps the ordinary 360-second installation completion gate;
failures retain whether the stock app showed an error, its numeric code when the
accessibility tree exposes one, and bounded owned-server state.
The `share` diagnostic also exports the original two clients and checks their
distinct identities, reopens administration, scans the native server and verifies
the retained second user, then closes the app before image transfer or traffic probes.
Export failures retain fixed stage/action flags and bounded control counts, without
UI text or configuration values. The ordinary export deadlines remain unchanged.
Reboots, restarts and recovery check the server directly: SSH through the advertised
administration port, its `clientsTable` and live `awg show awg0 peers` against a baseline
of exactly the retained client and the app's own administrator client, the revoked client
absent. Every lane except `setup` and `resources` then runs one stock-app discovery,
reopening the original saved entry; a failure there stays fatal. For navigation diagnosis,
`reconnect` reaches that discovery after disabled owner reboot and explicit up, without
the full resource, load and recovery prefix.

For a separate administrative idempotence diagnostic, one leased owner can run
the existing `owner.sh` phases `bootstrap`, `init-isolated`, then `admin-down-check`
against the same candidate bundle. The last phase removes administrative ingress,
waits for the running fixture's agent after any ACL restart, then repeats the
same down command. It does not require native VPN installation or a GUI client.
The `maintenance` lane runs after the shared native clients and revocation. It
records what the stock host firewall script left in the guest's raw table, then
applies that script's TCP IP-options drop rule as root and checks administration,
retained and revoked client traffic beside the profile's nftables policy. It steps
`docker-ce` and `containerd.io` back one repository version and drives the app's
**Update server packages** action, declining its reboot question. Afterwards both
packages must match their repository candidates, the native state signature and
readiness must hold, and traffic, revocation and one discovery must still pass.

The ordinary `admin-down` phase additionally verifies the running VPN afterward;
public administrative down continues to support stopped or agentless yards.
The shared Free Page Reporting probe remains under `dev/e2e/` because it tests a
generic VM capability. See [Amnezia acceptance coverage](../../../../docs/amnezia.md#acceptance-check).

`bash dev/test-profiles.sh` discovers profile host-free runners for CI. Files under
profile `tests/` directories are never included in runtime release bundles.
`bash dev/test-profiles.sh --e2e --slot N` discovers profile live entrypoints instead.
