#!/usr/bin/env python3
"""Kill an E2E transition after its journaled atomic link activation."""

import argparse
import ctypes
import errno
import json
import os
import select
import signal
import stat
import struct
import subprocess
import sys
import time


IN_CREATE = 0x00000100
IN_MOVED_TO = 0x00000080
EVENT_HEADER = struct.Struct("iIII")


def fail(message: str) -> int:
    print(f"release-transition-post-cas-observer: {message}", file=sys.stderr)
    return 2


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--runtime-root", required=True)
    parser.add_argument("--journal", required=True)
    parser.add_argument("--source-transaction", required=True)
    parser.add_argument("--candidate-target", required=True)
    parser.add_argument("--marker", required=True)
    parser.add_argument("--timeout", type=float, default=120.0)
    parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    if args.command[:1] == ["--"]:
        args.command = args.command[1:]
    return args


def read_journal(path: str) -> dict:
    descriptor = os.open(path, os.O_RDONLY | os.O_CLOEXEC | os.O_NOFOLLOW)
    try:
        metadata = os.fstat(descriptor)
        if not stat.S_ISREG(metadata.st_mode):
            raise ValueError("journal is not a regular file")
        with os.fdopen(descriptor, "r", encoding="utf-8") as stream:
            descriptor = -1
            value = json.load(stream)
    finally:
        if descriptor >= 0:
            os.close(descriptor)
    if not isinstance(value, dict):
        raise ValueError("journal is not a JSON object")
    return value


def observe_activation(args: argparse.Namespace) -> dict | None:
    current = os.path.join(args.runtime_root, "current")
    if not stat.S_ISLNK(os.lstat(current).st_mode):
        return None
    active = os.readlink(current)
    if active != f"releases/{args.candidate_target}":
        return None
    journal = read_journal(args.journal)
    transaction = journal.get("transaction")
    checkpoint = journal.get("checkpoint")
    if (
        not isinstance(transaction, str)
        or not transaction
        or transaction == args.source_transaction
        or checkpoint not in {"activation-intent", "target-active", "reconciling"}
        or journal.get("goal", {}).get("target") != args.candidate_target
        or journal.get("goal", {}).get("direction") != "activate-target"
        or journal.get("releases", {}).get("target") != args.candidate_target
    ):
        return None
    return {
        "active": active,
        "checkpoint": checkpoint,
        "transaction": transaction,
    }


def write_marker(path: str, observation: dict) -> None:
    payload = (json.dumps(observation, sort_keys=True) + "\n").encode()
    descriptor = os.open(
        path,
        os.O_WRONLY
        | os.O_CREAT
        | os.O_EXCL
        | os.O_CLOEXEC
        | os.O_NOFOLLOW,
        0o600,
    )
    try:
        offset = 0
        while offset < len(payload):
            offset += os.write(descriptor, payload[offset:])
        os.fsync(descriptor)
    finally:
        os.close(descriptor)
    parent = os.open(os.path.dirname(path), os.O_RDONLY | os.O_CLOEXEC | os.O_DIRECTORY)
    try:
        os.fsync(parent)
    finally:
        os.close(parent)


def child_status(child: subprocess.Popen[bytes]) -> int:
    status = child.poll()
    if status is None:
        return -1
    if status < 0:
        return 128 - status
    return status


def interrupt_live_child(child: subprocess.Popen[bytes]) -> None:
    status = child.poll()
    if status is not None:
        fail_group(f"transition exited with status {child_status(child)} before exact activation")
    try:
        os.kill(child.pid, signal.SIGKILL)
    except ProcessLookupError:
        fail_group("transition exited before exact activation SIGKILL")
    status = child.wait()
    if status != -signal.SIGKILL:
        fail_group(f"transition exited with status {child_status(child)} before SIGKILL")


def kill_isolated_group(signum: signal.Signals) -> None:
    if os.getpid() != os.getpgrp():
        os._exit(125)
    if signum != signal.SIGKILL:
        signal.signal(signum, signal.SIG_DFL)
    os.killpg(os.getpgrp(), signum)
    os._exit(128 + signum)


def fail_group(message: str) -> None:
    print(f"release-transition-post-cas-observer: {message}", file=sys.stderr, flush=True)
    kill_isolated_group(signal.SIGKILL)


def forward_signal(signum: int, _frame: object) -> None:
    kill_isolated_group(signal.Signals(signum))


def main() -> int:
    args = parse_args()
    child: subprocess.Popen[bytes] | None = None
    if (
        not args.command
        or args.timeout <= 0
        or not os.path.isabs(args.runtime_root)
        or not os.path.isabs(args.journal)
        or not os.path.isabs(args.marker)
        or args.runtime_root == os.path.sep
        or not args.candidate_target
        or "/" in args.candidate_target
    ):
        return fail("invalid arguments")
    if os.getpid() != os.getpgrp():
        return fail("observer must be an isolated process-group leader")
    try:
        os.lstat(args.marker)
    except FileNotFoundError:
        pass
    else:
        return fail("observation marker already exists")

    libc = ctypes.CDLL(None, use_errno=True)
    descriptor = libc.inotify_init1(os.O_CLOEXEC | os.O_NONBLOCK)
    if descriptor < 0:
        code = ctypes.get_errno()
        return fail(f"inotify_init1: {os.strerror(code)}")
    try:
        watch = libc.inotify_add_watch(
            descriptor,
            os.fsencode(args.runtime_root),
            IN_CREATE | IN_MOVED_TO,
        )
        if watch < 0:
            code = ctypes.get_errno()
            return fail(f"inotify_add_watch: {os.strerror(code)}")

        for signum in (signal.SIGHUP, signal.SIGINT, signal.SIGTERM):
            signal.signal(signum, forward_signal)
        child = subprocess.Popen(args.command, stdin=subprocess.DEVNULL)
        if os.getpgid(child.pid) != os.getpgrp():
            child.kill()
            child.wait()
            return fail("transition child did not join the isolated process group")
        deadline = time.monotonic() + args.timeout
        while time.monotonic() < deadline:
            remaining = max(0.0, min(0.25, deadline - time.monotonic()))
            readable, _, _ = select.select([descriptor], [], [], remaining)
            if readable:
                try:
                    events = os.read(descriptor, 65536)
                except BlockingIOError:
                    events = b""
                offset = 0
                while offset + EVENT_HEADER.size <= len(events):
                    _, mask, _, length = EVENT_HEADER.unpack_from(events, offset)
                    offset += EVENT_HEADER.size
                    name = events[offset : offset + length].split(b"\0", 1)[0]
                    offset += length
                    if name != b"current" or not mask & IN_MOVED_TO:
                        continue
                    observation = observe_activation(args)
                    if observation is None:
                        continue
                    interrupt_live_child(child)
                    write_marker(args.marker, observation)
                    kill_isolated_group(signal.SIGKILL)
            status = child_status(child)
            if status >= 0:
                fail_group(f"transition exited with status {status} before exact activation")
        fail_group("timed out before an authorized current-link activation")
    except BaseException as error:
        if child is not None:
            detail = str(error) or type(error).__name__
            fail_group(detail)
        if isinstance(error, OSError) and error.errno == errno.ENOENT:
            return fail("observed transition state disappeared")
        if isinstance(error, Exception):
            return fail(str(error))
        raise
    finally:
        os.close(descriptor)


if __name__ == "__main__":
    raise SystemExit(main())
