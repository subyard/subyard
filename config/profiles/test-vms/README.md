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
disposable-VM acceptance lane.
