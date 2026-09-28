# Amnezia profile checks

These tests belong to the profile. The Python suite checks the owner handler and
guest runtime; the Go tests load the shipped preset and resource descriptor through
Subyard's configuration and resource APIs.

From the repository root:

```sh
bash config/profiles/amnezia/tests/run.sh
config/profiles/amnezia/tests/e2e/acceptance.sh --slot N --lane full
config/profiles/amnezia/tests/e2e/acceptance.sh --slot N --lane recovery
```

Live checks use fresh allocated test VMs and the shared `dev/agent-e2e.sh` lease
API. Follow [the VM guide](../../../../docs/test-vms.md) before running them.
The shared Free Page Reporting probe remains under `dev/e2e/` because it tests a
generic VM capability. See [Amnezia acceptance coverage](../../../../docs/amnezia.md#acceptance-check).

`bash dev/test-profiles.sh` discovers profile host-free runners for CI. Files under
profile `tests/` directories are never included in runtime release bundles.
`bash dev/test-profiles.sh --e2e --slot N` discovers profile live entrypoints instead.
