#!/usr/bin/env python3
"""Best-effort periodic Go build cache trim; never traverse links or devices."""
import argparse
import os
import re
import stat
import time


def limit_bytes(value):
    match = re.fullmatch(r"([1-9][0-9]*)([KMGT]?)", value)
    if not match:
        raise argparse.ArgumentTypeError("limit must be a positive integer with optional K/M/G/T suffix")
    return int(match[1]) * 1024 ** ("", "K", "M", "G", "T").index(match[2])


def trim(root, limit):
    flags = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW
    with_fd = os.open(root, flags)
    try:
        device = os.fstat(with_fd).st_dev
        total = 0
        entries = []
        for shard in os.listdir(with_fd):
            if not re.fullmatch(r"[0-9a-f]{2}", shard):
                continue
            try:
                directory = os.open(shard, flags, dir_fd=with_fd)
            except (FileNotFoundError, NotADirectoryError):
                continue
            try:
                if os.fstat(directory).st_dev != device:
                    continue
                for name in os.listdir(directory):
                    try:
                        info = os.stat(name, dir_fd=directory, follow_symlinks=False)
                    except FileNotFoundError:
                        continue
                    if stat.S_ISREG(info.st_mode) and info.st_dev == device:
                        total += info.st_blocks * 512
                        entries.append((info.st_mtime, shard, name, info))
            finally:
                os.close(directory)
        if total <= limit:
            return
        cutoff = time.time() - 3600
        for mtime, shard, name, before in sorted(entries):
            if total <= limit * 4 // 5 or mtime > cutoff:
                break
            try:
                directory = os.open(shard, flags, dir_fd=with_fd)
                try:
                    if os.fstat(directory).st_dev != device:
                        continue
                    now = os.stat(name, dir_fd=directory, follow_symlinks=False)
                    # Recheck use/identity immediately before unlinking. Concurrent
                    # Go writers may lose a cache hit, never a build output.
                    if (now.st_dev, now.st_ino, now.st_mtime_ns, now.st_size) != (
                        before.st_dev, before.st_ino, before.st_mtime_ns, before.st_size
                    ):
                        continue
                    os.unlink(name, dir_fd=directory)
                    total -= before.st_blocks * 512
                finally:
                    os.close(directory)
            except FileNotFoundError:
                continue
    finally:
        os.close(with_fd)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--validate", action="store_true")
    parser.add_argument("root")
    parser.add_argument("limit", type=limit_bytes)
    args = parser.parse_args()
    if not args.validate:
        trim(args.root, args.limit)
