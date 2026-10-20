# Test VM host-sink lifecycle

The test-vms yard preset remains in config/yards/profiles/test-vms.env.
This package contributes only a read-only full-uninstall ownership assessment.
It does not add provisioning, profile enablement or runtime activation.

Its shipped full_cleanup hook recognizes the exact host-sink service, timer,
engine helper and enablement link for the selected installation's data root.
The generic teardown boundary synchronously drains those approved units before
removing their files or data. A sink serving another data root is preserved.
Private retained evidence adds a conditional read-only ownership proof to the
central plan. After consent, the owner checks bounded source metadata, including
host companions tied to approved incidents. Core repeats the check after draining
producers and before resource deletion. Unknown or foreign evidence refuses
dependent cleanup without printing records.

Run its host-free ownership checks with config/profiles/test-vms/tests/run.sh.
Installed-runtime, delayed sink and reboot checks belong to the full-uninstall
disposable-VM acceptance lane. The profile
[`tests/e2e/acceptance.sh`](tests/e2e/acceptance.sh) runs both the supported-upgrade cold-acquire
and full-uninstall lanes by default. Its lane inventory keeps both mandatory for release collection.
The cold lane requires the collector's checksum-verified frozen public candidate.

The owning runner executes the installer identity regression before
`TestBaseRecipeInstallerContract`. That test unpacks the production recipe bundle and runs the
real base recipe and installer for both environments with only physical effects replaced.
`tests/run.sh --preflight` selects those prerequisites and rejects missing or skipped recipe cases;
a collector-bound `make verify` PASS can satisfy them through the existing evidence contract.

Recipe/archive producer, installer or called library, broker activation and release migration
changes require `tests/e2e/acceptance.sh --slot N --lane broker-cold-upgrade`. It installs the
checksum-pinned published 0.19.0 runtime, initializes a disposable broker owner, and invokes its
supported updater with the frozen candidate. The owner and lease state survive the update.
With no cached base, the candidate's first acquire builds and validates its base, grants a
singleton, proves SSH and a second boot, releases it, and returns available without pending
builders or VMs. The fixture reuses `dev/e2e/staging-reliability.sh --supported-upgrade` and its
marker-owned cleanup. Host-free checks do not establish real package, NetworkManager,
Incus, systemd or boot behavior.
