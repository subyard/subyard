"""Launch the yard's Codex without Orca's automatically supplied YOLO flag."""

import os
import sys


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
        os.execvp("codex", ["codex", *config_arguments(sys.argv[1:])])
    except OSError:
        print("Codex executable unavailable in the yard", file=sys.stderr)
        raise SystemExit(127)
