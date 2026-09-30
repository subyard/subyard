"""Preserve yard approvals and Orca's per-terminal Codex launch behavior."""

import os
import subprocess
import sys


def launch_arguments(arguments):
    # Orca 1.4.218's codex-shell-function.ts skips an existing shell function.
    # Keep its preparation and whole-argument isolation rule in our scoped launcher.
    preflight = os.environ.get("ORCA_CODEX_LAUNCH_PREFLIGHT")
    if preflight and os.access(preflight, os.X_OK):
        try:
            subprocess.run([preflight, "agent", "hooks", "prepare-codex"],
                           stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                           stderr=subprocess.DEVNULL)
        except OSError:
            pass
    result = config_arguments(arguments)
    if os.environ.get("ORCA_CODEX_ISOLATE") == "0" or any(
        argument in ("agents", "queue", "--no-daemon", "--remote")
        or argument.startswith("--remote=") for argument in result
    ):
        return result
    try:
        help_result = subprocess.run(["codex", "--help"], stdin=subprocess.DEVNULL,
                                     stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                                     text=True, timeout=5)
        if help_result.returncode == 0 and "--no-daemon" in help_result.stdout:
            result.insert(0, "--no-daemon")
    except (OSError, subprocess.TimeoutExpired):
        pass
    return result


def config_arguments(arguments):
    result = []
    options = True
    for argument in arguments:
        if options and argument == "--dangerously-bypass-approvals-and-sandbox":
            continue
        result.append(argument)
        if argument == "--":
            options = False
    return result


if __name__ == "__main__":
    try:
        os.execvp("codex", ["codex", *launch_arguments(sys.argv[1:])])
    except OSError:
        print("Codex executable unavailable in the yard", file=sys.stderr)
        raise SystemExit(127)
