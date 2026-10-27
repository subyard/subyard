#!/usr/bin/env python3
"""Closed package-local source inventory and canonical skill transport."""

import argparse
import json
from pathlib import Path
import stat
import sys
import tarfile


def _path(root, relative):
    path = Path(relative)
    if path.is_absolute() or not path.parts or '..' in path.parts:
        raise ValueError('invalid skill source path')
    current = root
    for part in path.parts:
        current = current / part
        mode = current.lstat().st_mode
        if not (stat.S_ISDIR(mode) or stat.S_ISREG(mode)):
            raise ValueError('skill source must be a regular file or directory')
    return current


def _excluded(path):
    return any(part in ('tests', '__pycache__') for part in path.parts) or path.suffix == '.pyc'


def _inventory(root):
    root = Path(root)
    members = set()

    def add(relative):
        relative = Path(relative)
        if _excluded(relative):
            return
        path = _path(root, relative)
        members.add(relative.as_posix())
        if path.is_dir():
            for child in sorted(path.iterdir()):
                add(child.relative_to(root))

    add('config/skills')
    profiles = root / 'config/profiles'
    if profiles.exists() or profiles.is_symlink():
        _path(root, 'config/profiles')
        for package in sorted(profiles.iterdir()):
            _path(root, package.relative_to(root))
            for name in ('skills', 'skills.json'):
                source = package / name
                if not source.exists() and not source.is_symlink():
                    continue
                add(source.relative_to(root))
                if name == 'skills.json':
                    declarations = json.loads(source.read_text())
                    for files in declarations.values():
                        for relative in files.values():
                            # Validate before joining: absolute/parent paths must
                            # never be interpreted relative to another package.
                            source = _path(package, relative)
                            if not source.is_file():
                                raise ValueError('manifest source must be a regular file')
                            add(source.relative_to(root))
    return root, members


def source_files(root):
    root, members = _inventory(root)
    if not _path(root, 'scripts/refresh-agent-skills.sh').is_file():
        raise ValueError('skill wrapper must be a regular file')
    return sorted({'scripts/refresh-agent-skills.sh'} | {
        path for path in members if _path(root, path).is_file()})


def archive_members(root):
    root, members = _inventory(root)
    for name in list(members):
        members.update(parent.as_posix() for parent in Path(name).parents if parent != Path('.'))
    return sorted(members)


def write_archive(root, stream):
    root = Path(root)
    # Finish validation before emitting any member.
    members = archive_members(root)
    with tarfile.open(fileobj=stream, mode='w|') as archive:
        for name in members:
            path = _path(root, name)
            member = tarfile.TarInfo(name)
            member.mode = 0o700 if path.is_dir() else 0o600
            if path.is_dir():
                member.type = tarfile.DIRTYPE
                archive.addfile(member)
            else:
                member.size = path.stat().st_size
                with path.open('rb') as source:
                    archive.addfile(member, source)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('operation', choices=('archive', 'files'))
    parser.add_argument('root', type=Path)
    args = parser.parse_args()
    if args.operation == 'archive':
        write_archive(args.root, sys.stdout.buffer)
    else:
        print(json.dumps(source_files(args.root)))


if __name__ == '__main__':
    main()
