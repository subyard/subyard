#!/usr/bin/env python3
"""Remove recognized preview blocks only from inventory-owned instruction files."""
import hashlib
import json
import os
from pathlib import Path
import stat
import sys

from deliver import safe, write

LEGACY = b'<!-- subyard-preview -->\nFor a static web preview, run `subyard-preview <relative-static-dir>` from the Git workspace and keep it running with your background/async process mechanism. Share the printed URL. An owner Tailscale URL requires device reachability and Tailnet policy access; a loopback URL requires an active preview-enabled `yard code` SSH session. The helper must stay running for either URL.\n<!-- /subyard-preview -->'
CURRENT = b'<!-- subyard-preview -->\nFor a static web preview, run `subyard-preview <relative-static-dir>` from the Git workspace and keep it running with your background/async process mechanism. Share the printed URL. Owner URLs require network reachability (and Tailnet policy access for Tailscale). A loopback URL is local to the yard; provide the operator-machine tunnel recipe from `subyard-preview --help` only when the operator requests it. The helper must stay running.\n<!-- /subyard-preview -->'


def migrate(home, uid, inventory):
    safe(inventory)
    if not inventory.exists():
        return
    parent = inventory.parent.stat()
    if not stat.S_ISDIR(parent.st_mode) or parent.st_uid != os.geteuid() or parent.st_mode & 0o022:
        raise ValueError('unsafe integration inventory directory')
    info = inventory.stat()
    if not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o077:
        raise ValueError('unsafe integration inventory')
    state = json.loads(inventory.read_bytes())
    if state.get('schema') != 1:
        raise ValueError('unknown integration inventory')
    destinations = {str(home / path) for path in ('.claude/CLAUDE.md', '.codex/AGENTS.md', '.config/opencode/AGENTS.md')}
    changes = []
    for entry in state['entries']:
        if entry.get('kind') != 'file' or entry.get('path') not in destinations:
            continue
        path = Path(entry['path'])
        safe(path)
        if not path.exists():
            continue
        data = path.read_bytes()
        if b'<!-- subyard-preview -->' not in data and b'<!-- /subyard-preview -->' not in data:
            continue
        if data.count(b'<!-- subyard-preview -->') != 1 or data.count(b'<!-- /subyard-preview -->') != 1:
            raise ValueError('ambiguous preview markers: ' + str(path))
        block = next((body for body in (LEGACY, CURRENT) if body in data), None)
        if block is None:
            raise ValueError('unrecognized preview block: ' + str(path))
        before = hashlib.sha256(data).hexdigest()
        if before not in (entry['digest'], entry.get('pending_digest'), entry.get('config_pending_digest')):
            raise ValueError('owned instruction drift: ' + str(path))
        metadata = path.stat()
        if not stat.S_ISREG(metadata.st_mode) or (metadata.st_uid,metadata.st_gid) != (uid,uid) or stat.S_IMODE(metadata.st_mode) != 0o644:
            raise ValueError('unsafe instruction metadata: ' + str(path))
        after = data.replace(block, b'', 1)
        # Publish both identities before replacement so an interrupted migration
        # can be retried using the inventory's existing pending contract.
        entry['config_pending_digest'] = hashlib.sha256(after).hexdigest()
        entry['preview_migrated'] = True
        changes.append((path, after, metadata.st_gid, entry))
    if not changes:
        return
    encode = lambda: (json.dumps(state, sort_keys=True, separators=(',', ':'))).encode()
    write(inventory, encode(), 0o600, os.geteuid(), os.getegid())
    for path, data, gid, entry in changes:
        write(path, data, 0o644, uid, gid)
        entry['digest'] = entry.pop('config_pending_digest')
    write(inventory, encode(), 0o600, os.geteuid(), os.getegid())


if __name__ == '__main__':
    try:
        migrate(Path(sys.argv[1]), int(sys.argv[2]), Path('/var/lib/subyard/integrations/inventory.json'))
    except (OSError, ValueError, KeyError) as error:
        print('preview migration: ' + str(error), file=sys.stderr)
        sys.exit(1)
