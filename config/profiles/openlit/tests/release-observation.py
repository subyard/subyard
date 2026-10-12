"""Exact absent-package observation for the released-updater fixture."""
import shlex


def read_only_commands(package):
    # Match the shipped transport, including its guest script; execute neither.
    transport = (package / "owner.sh").read_text().split("    bash -euo pipefail -c ")
    if len(transport) != 2:
        raise ValueError("OpenLIT observation transport changed")
    arguments = shlex.split(transport[1])
    if arguments[1:] != ["subyard", "$mode", "$selected", "$DEV_USER", "$agents", "${runtime_args[@]}"]:
        raise ValueError("OpenLIT observation arguments changed")
    guest = ["exec", "yard", "--project", "subyard", "--"]
    command = guest + ["bash", "-euo", "pipefail", "-c", arguments[0],
                       "subyard", "observe", "0", "dev", "codex"]
    return [(command, '{"state":"absent","actual":"","desired":""}\n')]
