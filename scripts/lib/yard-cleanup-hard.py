#!/usr/bin/env python3
"""Inspect and restore registered project wrappers from a read-only native copy."""
import hashlib
import json
import os
from pathlib import Path
import stat
import subprocess
import sys


def inspect(request, srv='/srv'):
    srv = os.fspath(srv)
    projects = request['projects']
    workspace = srv + '/workspaces'
    expected = {p['projectId'] for p in projects}
    if any(not name or '/' in name or name in ('.', '..') for name in expected):
        raise ValueError('invalid registered project identifier')
    if not os.path.exists(workspace) and not expected:
        return {'projects': {}}
    if os.path.islink(workspace) or not os.path.isdir(workspace):
        raise ValueError('workspace root must be a real directory')
    if set(os.listdir(workspace)) != expected:
        raise ValueError('unregistered or missing workspace data requires inspection')
    roots = [workspace + '/' + p['projectId'] for p in projects]
    binds = {workspace + '/' + p['projectId'] + '/src' for p in projects if p['mode'] == 'bind'}
    mounts = {line.split()[4] for line in Path('/proc/self/mountinfo').read_text().splitlines()}
    if any(path.startswith(workspace + '/') and path not in binds for path in mounts):
        raise ValueError('foreign or nested mount inside project data')

    def snapshot_path(path):
        # Absolute Git paths keep their canonical /srv location after restore.
        if path.startswith('/srv/workspaces/'):
            path = srv + path[4:]
        resolved = os.path.realpath(path)
        if resolved.startswith('/srv/workspaces/'):
            resolved = os.path.realpath(srv + resolved[4:])
        return resolved

    def dependency(path):
        resolved = snapshot_path(path)
        return any(resolved == root or resolved.startswith(root + '/') for root in roots) and not any(resolved == bind or resolved.startswith(bind + '/') for bind in binds)

    records = {}
    hardlinks = {}
    project_paths = {}
    git_roots = []
    for root in sorted(roots):
        if os.path.islink(root) or not os.path.isdir(root):
            raise ValueError('project wrapper must be a real directory')
        paths = [root]
        for directory, dirs, files in os.walk(root, followlinks=False):
            dirs.sort()
            for name in sorted(dirs + files):
                path = directory + '/' + name
                if path in binds:
                    if name in dirs:
                        dirs.remove(name)
                    continue  # Host source and its mountpoint are never copied.
                paths.append(path)
        project_paths[root] = sorted(paths)
        for path in paths:
            if os.path.basename(path) == '.git':
                target = path
                if os.path.isfile(path) and not os.path.islink(path):
                    text = Path(path).read_text()
                    if not text.startswith('gitdir: '):
                        raise ValueError('unrecognized Git indirection')
                    target = os.path.join(os.path.dirname(path), text[8:].strip())
                if target.startswith('/srv/workspaces/'):
                    target = srv + target[4:]
                if not dependency(target):
                    raise ValueError('Git metadata depends on data outside the backup')
                git_roots.append(snapshot_path(target))
            elif os.path.isdir(path) and not os.path.islink(path) and os.path.isfile(path + '/HEAD') and os.path.isdir(path + '/objects'):
                git_roots.append(path)  # Bare repositories are complete project data too.
    for root, paths in project_paths.items():
        digest = hashlib.sha256()
        for path in paths:
            info = os.lstat(path)
            kind = stat.S_IFMT(info.st_mode)
            if kind not in (stat.S_IFREG, stat.S_IFDIR, stat.S_IFLNK):
                raise ValueError('project contains a socket or special file: ' + path)
            name = os.path.basename(path)
            if stat.S_ISLNK(info.st_mode) and any(path == git or path.startswith(git + '/') for git in git_roots) and not dependency(path):
                raise ValueError('Git metadata symlink depends on data outside the backup')
            if name == 'commondir' and stat.S_ISREG(info.st_mode):
                if not dependency(os.path.join(os.path.dirname(path), Path(path).read_text().strip())):
                    raise ValueError('Git common directory depends on data outside the backup')
            if path.endswith('/objects/info/alternates'):
                if not stat.S_ISREG(info.st_mode):
                    raise ValueError('Git alternates must be a regular file')
                for alternate in Path(path).read_text().splitlines():
                    if alternate and ('"' in alternate or not dependency(os.path.join(os.path.dirname(os.path.dirname(path)), alternate))):
                        raise ValueError('Git alternates depend on unverified external data')
            record = [os.path.relpath(path, workspace), info.st_mode, info.st_uid, info.st_gid]
            if stat.S_ISLNK(info.st_mode):
                record.append(os.readlink(path))
            elif stat.S_ISREG(info.st_mode):
                content = hashlib.sha256()
                with open(path, 'rb') as source:
                    for block in iter(lambda: source.read(1024 * 1024), b''):
                        content.update(block)
                # Compare hardlink relationships, never inode values across copies.
                first = hardlinks.setdefault((info.st_dev, info.st_ino), record[0])
                record.extend((info.st_size, content.hexdigest(), first))
            digest.update(json.dumps(record, separators=(',', ':')).encode() + b'\n')
        records[os.path.basename(root)] = digest.hexdigest()
    return {'projects': records}


def restore(request, backup, destination='/srv'):
    before = inspect(request, backup)
    if before != request['preserved']:
        raise ValueError('backup project content changed')
    projects = request['projects']
    if not projects:
        return before
    workspace = destination + '/workspaces'
    if os.path.islink(workspace):
        raise ValueError('restore workspace is linked')
    os.makedirs(workspace, exist_ok=True)
    # Existing entries may only be prior restoration output. tar never traverses
    # registered bind src; each such source remains outside this operation.
    if set(os.listdir(workspace)) - {p['projectId'] for p in projects}:
        raise ValueError('unexpected data in replacement workspace')
    for p in projects:
        root = workspace + '/' + p['projectId']
        if os.path.lexists(root) and (os.path.islink(root) or not os.path.isdir(root)):
            raise ValueError('unsafe replacement wrapper')
    command = ['tar', '-C', backup + '/workspaces', '-cpf', '-']
    command += ['--exclude=' + p['projectId'] + '/src' for p in projects if p['mode'] == 'bind']
    command += ['--'] + [p['projectId'] for p in sorted(projects, key=lambda p: p['projectId'])]
    producer = subprocess.Popen(command, stdout=subprocess.PIPE)
    try:
        result = subprocess.run(['tar', '-C', workspace, '-xpf', '-', '--same-owner'], stdin=producer.stdout)
    finally:
        producer.stdout.close()
        code = producer.wait()
    if code or result.returncode:
        raise ValueError('project restore failed; backup is retained')
    after = inspect(request, destination)
    if after != before:
        raise ValueError('restored project content differs; backup is retained')
    return after


if __name__ == '__main__':
    request = json.load(sys.stdin)
    action, root = sys.argv[1:3]
    result = restore(request, root) if action == 'restore' else inspect(request, root)
    print(json.dumps(result, separators=(',', ':')))
