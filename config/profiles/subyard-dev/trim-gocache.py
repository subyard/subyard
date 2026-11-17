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


DIRECTORY_FLAGS = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW


def identity(info):
    return (info.st_dev, info.st_ino, info.st_mode, info.st_mtime_ns,
            info.st_ctime_ns, info.st_size, info.st_blocks)


def entry(parent, name, device):
    """Snapshot a file or flat Go executable entry; refuse unsafe contents."""
    info = os.stat(name, dir_fd=parent, follow_symlinks=False)
    if info.st_dev != device:
        return None
    if stat.S_ISREG(info.st_mode):
        return info.st_mtime, info.st_blocks * 512, identity(info), []
    if not stat.S_ISDIR(info.st_mode) or not name.endswith("-d"):
        return None
    directory = os.open(name, DIRECTORY_FLAGS, dir_fd=parent)
    try:
        if identity(os.fstat(directory)) != identity(info):
            return None
        children = []
        for child in sorted(os.listdir(directory)):
            child_info = os.stat(child, dir_fd=directory, follow_symlinks=False)
            if not stat.S_ISREG(child_info.st_mode) or child_info.st_dev != device:
                return None
            children.append((child, identity(child_info)))
        return (max([info.st_mtime] + [item[1][3] / 1e9 for item in children]),
                (info.st_blocks + sum(item[1][-1] for item in children)) * 512,
                identity(info), children)
    finally:
        os.close(directory)


def trim(root, limit):
    # Go coalesces mtime updates for an hour. Keep another hour of margin,
    # measured before scanning, and skip entries changed during this trim.
    cutoff = time.time() - 2 * 3600
    root_fd = os.open(root, DIRECTORY_FLAGS)
    try:
        device = os.fstat(root_fd).st_dev
        total = 0
        entries = []
        for shard in os.listdir(root_fd):
            if not re.fullmatch(r"[0-9a-f]{2}", shard):
                continue
            try:
                directory = os.open(shard, DIRECTORY_FLAGS, dir_fd=root_fd)
            except (FileNotFoundError, NotADirectoryError):
                continue
            try:
                if os.fstat(directory).st_dev != device:
                    continue
                for name in os.listdir(directory):
                    try:
                        before = entry(directory, name, device)
                    except FileNotFoundError:
                        continue
                    if before is not None:
                        total += before[1]
                        entries.append((before[0], shard, name, before))
            finally:
                os.close(directory)
        if total <= limit:
            return
        for mtime, shard, name, before in sorted(entries):
            if total <= limit * 4 // 5 or mtime > cutoff:
                break
            try:
                directory = os.open(shard, DIRECTORY_FLAGS, dir_fd=root_fd)
                try:
                    if os.fstat(directory).st_dev != device:
                        continue
                    if entry(directory, name, device) != before:
                        continue
                    if stat.S_ISDIR(before[2][2]):
                        executable_dir = os.open(name, DIRECTORY_FLAGS, dir_fd=directory)
                        try:
                            if identity(os.fstat(executable_dir)) != before[2]:
                                continue
                            for child, _ in before[3]:
                                os.unlink(child, dir_fd=executable_dir)
                        finally:
                            os.close(executable_dir)
                        os.rmdir(name, dir_fd=directory)
                    else:
                        os.unlink(name, dir_fd=directory)
                    total -= before[1]
                finally:
                    os.close(directory)
            except (FileNotFoundError, NotADirectoryError):
                continue
    finally:
        os.close(root_fd)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--validate", action="store_true")
    parser.add_argument("root")
    parser.add_argument("limit", type=limit_bytes)
    args = parser.parse_args()
    if not args.validate:
        trim(args.root, args.limit)
