#!/usr/bin/env python3
"""Dependency-free native resource fixture; installed only in a disposable VM."""
import fcntl
import hashlib
import json
import os
from pathlib import Path
import sys

root = Path(__file__).resolve().parent
state = root / 'native-state'
mode = os.environ.get('SUBYARD_RESOURCE_MODE', '')


def observation():
    if not os.path.lexists(state):
        return 'absent', hashlib.sha256(b'absent').hexdigest()
    metadata = state.lstat()
    if state.is_symlink() or not state.is_file():
        raise RuntimeError('fixture state must be a native regular file')
    value = state.read_bytes()
    identity = (f'{metadata.st_dev}:{metadata.st_ino}:{metadata.st_mode}:'
                f'{metadata.st_uid}:{metadata.st_gid}:{metadata.st_size}:'
                f'{metadata.st_mtime_ns}:{metadata.st_ctime_ns}:').encode() + value
    return ('ready' if value == b'ready\n' else 'pending'), hashlib.sha256(identity).hexdigest()


def scope_binding():
    metadata = root.stat()
    identity = (f'{root}:{metadata.st_dev}:{metadata.st_ino}:{metadata.st_mode}:'
                f'{metadata.st_uid}:{metadata.st_gid}:up:ready:').encode()
    return hashlib.sha256(identity + Path(__file__).read_bytes()).hexdigest()


def assessment():
    observed, fingerprint = observation()
    binding = scope_binding()
    changed = observed != 'ready'
    consequence = 'Create the fixture-owned native runtime state'
    return {'schema': 'yard.resource-action-assessment.v2', 'action': 'up',
            'changed': changed, 'binding': binding,
            'consequences': [consequence] if changed else [],
            'steps': [{'id': 'fixture-native-state', 'target': 'fixture-owned runtime state',
                       'observed': observed + ' native state digest ' + fingerprint, 'desired': 'ready',
                       'decision': 'apply' if changed else 'skip',
                       'verify': 'Read back the fixture-owned native runtime state',
                       'consequence': consequence}]}


if sys.argv[1:] != ['up']:
    sys.exit(2)
if mode in ('prepare', 'verify'):
    result = assessment()
    if mode == 'verify':
        if result['changed'] or result['binding'] != os.environ.get('SUBYARD_RESOURCE_BINDING', ''):
            sys.exit(1)
        result['steps'][0]['observed'] = result['steps'][0]['desired']
    print(json.dumps(result))
else:
    # The native executor owns its lock, immediate input guard and readback.
    with (root / 'native-lock').open('a') as lock:
        os.chmod(lock.name, 0o600)
        fcntl.flock(lock, fcntl.LOCK_EX)
        approved_binding = os.environ.get('SUBYARD_RESOURCE_BINDING', '')
        try:
            approved_steps = json.loads(os.environ.get('SUBYARD_RESOURCE_STEPS', ''))
        except ValueError:
            approved_steps = []
        current = assessment()
        live = current['steps'][0]
        valid = (approved_binding == current['binding'] and
                 isinstance(approved_steps, list) and len(approved_steps) == 1 and
                 isinstance(approved_steps[0], dict))
        if valid:
            approved = approved_steps[0]
            valid = all(approved.get(key) == live.get(key) for key in
                        ('id', 'target', 'desired', 'verify', 'consequence'))
            # Native skip identity is still checked. Observed apply may converge
            # to skip without a write; observed skip may never acquire new work.
            if live['decision'] == 'apply':
                valid = valid and approved.get('decision') == 'apply' and approved.get('observed') == live['observed']
            elif approved.get('decision') == 'skip':
                valid = valid and approved.get('observed') == live['observed']
        if not valid:
            print('plan_stale: fixture native state changed', file=sys.stderr)
            sys.exit(1)
        if live['decision'] == 'apply':
            state.write_bytes(b'ready\n')
            os.chmod(state, 0o600)
        if assessment()['changed']:
            sys.exit(1)
