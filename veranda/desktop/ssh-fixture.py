#!/usr/bin/env python3
"""Exact disposable SSH routes and read-only manual-attempt attribution."""
import json
import os
from pathlib import Path
import re
import shlex
import socket
import stat

HOST_RPC = ['yard', 'rpc', '--stdio']


def allowed_rpc(argv, yards):
    assert yards and len(set(yards)) == len(yards) and all(re.fullmatch(r'[a-z0-9][a-z0-9-]{0,62}', name) for name in yards)
    return argv == HOST_RPC or (len(argv) == 5 and argv[:2] == ['yard', '-Y']
                                and argv[2] in yards and argv[3:] == ['rpc', '--stdio'])


def cancel_complete(flow_closed, store_unchanged, attempt_alive):
    return flow_closed and store_unchanged and not attempt_alive


def private_json(path, value):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, 'w') as output:
        os.fchmod(output.fileno(), 0o600)
        json.dump(value, output)


def read_private_json(path):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(fd) as source:
        info = os.fstat(source.fileno())
        assert stat.S_ISREG(info.st_mode) and info.st_uid == os.getuid() and info.st_nlink == 1
        assert stat.S_IMODE(info.st_mode) == 0o600 and info.st_size <= 16384
        return json.load(source)


def identity(pid, proc=Path('/proc')):
    try:
        path = proc / str(pid)
        fields = (path / 'stat').read_text().rsplit(')', 1)[1].split()
        return {'pid': pid, 'start': int(fields[19]), 'parent': int(fields[1]), 'uid': path.stat().st_uid}
    except (OSError, ValueError, IndexError):
        return None


def peer_tuple(value):
    fields = value.split()
    assert len(fields) == 4 and fields[0] == fields[2] == '127.0.0.1'
    assert all(part.isdecimal() and 0 < int(part) < 65536 for part in (fields[1], fields[3]))
    return [fields[0], int(fields[1]), fields[2], int(fields[3])]


def tcp_rows(text):
    def address(value):
        host, port = value.split(':')
        return socket.inet_ntoa(bytes.fromhex(host)[::-1]), int(port, 16)
    rows = {}
    for line in text.splitlines()[1:]:
        fields = line.split()
        if len(fields) >= 10 and fields[3] == '01':
            local, remote = address(fields[1]), address(fields[2])
            rows[fields[9]] = [*local, *remote]
    return rows


def owned_ssh(gui, proc=Path('/proc')):
    current = identity(gui['pid'], proc)
    assert current and current['start'] == gui['start'] and current['uid'] == os.getuid(), 'owned_gui_changed'
    processes = {int(p.name): value for p in proc.iterdir() if p.name.isdecimal()
                 if (value := identity(int(p.name), proc)) and value['uid'] == os.getuid()}
    chosen = {gui['pid']}
    while True:
        descendants = {pid for pid, value in processes.items() if value['parent'] in chosen}
        if descendants <= chosen:
            break
        chosen |= descendants
    sockets = tcp_rows((proc / 'net/tcp').read_text())
    result = []
    for pid in sorted(chosen - {gui['pid']}):
        path = proc / str(pid)
        try:
            if (path / 'exe').resolve(strict=True).name != 'ssh':
                continue
            argv = (path / 'cmdline').read_bytes().decode().strip('\0').split('\0')
            delimiter = argv.index('--')
            route = shlex.split(argv[delimiter + 2]) if len(argv) == delimiter + 3 else []
            aliases = [value.removeprefix('HostKeyAlias=') for value in argv if value.startswith('HostKeyAlias=')]
            if len(aliases) != 1:
                continue
            tuples = [sockets[match[1]] for fd in (path / 'fd').iterdir()
                      if (match := re.fullmatch(r'socket:\[([0-9]+)\]', os.readlink(fd))) and match[1] in sockets]
            again = identity(pid, proc)
            if again == processes[pid]:
                result.append({**again, 'alias': aliases[0], 'route': route, 'peers': tuples})
        except (OSError, ValueError, UnicodeError):
            continue
    return result


def match_manual(arm, peer, clients):
    assert re.fullmatch(r'[0-9a-f]{32}', arm['nonce']) and peer[3] == arm['port']
    baseline = {tuple(pair) for pair in arm['baseline']}
    matches = [client for client in clients
               if (client['pid'], client['start']) not in baseline
               and re.fullmatch(r'veranda-[0-9a-f]{48}', client['alias'])
               and client['route'] == HOST_RPC and peer in client['peers']]
    assert len(matches) <= 1, 'ambiguous_manual_ssh'
    return matches[0] if matches else None


def maybe_block(root, argv, connection):
    block = root / 'block'
    if argv != HOST_RPC or not block.exists():
        return False
    arm = read_private_json(block)
    peer = peer_tuple(connection)
    client = match_manual(arm, peer, owned_ssh(arm['gui']))
    if client is None:
        return False
    private_json(root / ('blocked-' + arm['nonce'] + '.json'),
                 {'nonce': arm['nonce'], 'gui': arm['gui'], 'peer': peer, 'client': client, 'route': argv})
    return True


def verify_block(arm, receipt, clients):
    assert receipt['nonce'] == arm['nonce'] and receipt['gui'] == arm['gui'] and receipt['route'] == HOST_RPC
    client = match_manual(arm, receipt['peer'], clients)
    assert client is not None and client == receipt['client'], 'blocked_ssh_identity_changed'
    return client
