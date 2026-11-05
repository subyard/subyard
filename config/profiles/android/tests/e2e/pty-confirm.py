#!/usr/bin/env python3
"""Run a command on a pseudo-terminal and answer each confirmation prompt yes.

Every prompt is answered only after it appears, so a parent command cannot
consume the answer meant for a child operation's own confirmation.
"""
import os
import pty
import sys


def main() -> int:
    argv = sys.argv[1:]
    if not argv:
        print("usage: pty-confirm.py COMMAND [ARG...]", file=sys.stderr)
        return 2
    pid, fd = pty.fork()
    if pid == 0:
        os.execvp(argv[0], argv)
    pending, answered = b"", 0
    while True:
        try:
            data = os.read(fd, 4096)
        except OSError:
            break
        if not data:
            break
        sys.stdout.buffer.write(data)
        sys.stdout.flush()
        pending += data
        while b"Proceed?" in pending:
            pending = pending.split(b"Proceed?", 1)[1]
            os.write(fd, b"y\n")
            answered += 1
        pending = pending[-64:]
    _, status = os.waitpid(pid, 0)
    print(f"pty_prompts_answered={answered}")
    return os.waitstatus_to_exitcode(status)


if __name__ == "__main__":
    sys.exit(main())
