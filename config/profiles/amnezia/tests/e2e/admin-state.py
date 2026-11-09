#!/usr/bin/env python3
"""Check native client state read over administration SSH against the lane's baseline.

Usage: admin-state.py DIRECTORY admin-baseline|admin-check < clientsTable, PEERS line, live peer keys
Output holds counts and fixed messages only, never client keys or names.
Every refusal is reported, in a fixed order.
"""
import json
import os
from pathlib import Path
import subprocess
import sys

RETAINED_NAME, REVOKED_NAME, ADMIN_PREFIX = 'acceptance-two', 'acceptance-one', 'Admin ['


def public_key(directory, client):
    # The private key stays on stdin to the client's own native tool.
    lines = (directory / f'client-{client}.conf').read_text().splitlines()
    values = dict((key.strip(), value.strip()) for key, value in (line.split('=', 1) for line in lines if '=' in line))
    return subprocess.run(['docker', 'exec', '-i', f'subyard-amnezia-e2e-client-{client}', 'awg', 'pubkey'],
                          input=values['PrivateKey'] + '\n', capture_output=True, text=True, check=True,
                          timeout=15).stdout.strip()


def main(directory, mode):
    baseline_path = directory / 'admin-baseline.json'
    try:
        table, peers = sys.stdin.read().split('\nPEERS\n', 1)
        state = dict(clients={item['clientId']: item['userData']['clientName'] for item in json.loads(table)},
                     peers=sorted(peers.split()))
    except (ValueError, KeyError, TypeError):
        return 'administration SSH did not return native client state'
    if mode == 'admin-baseline':
        retained, revoked = public_key(directory, 'two'), public_key(directory, 'one')
    else:
        baseline = json.loads(baseline_path.read_text())
        retained, revoked = baseline['retained'], baseline['revoked']
    refusals = []
    if state['clients'].get(retained) != RETAINED_NAME or retained not in state['peers']:
        refusals.append('retained native client is missing from the server')
    if revoked in state['clients'] or revoked in state['peers'] or REVOKED_NAME in state['clients'].values():
        refusals.append('revoked native client is present on the server')
    if mode == 'admin-check':
        if state != baseline['state']:
            refusals.append('native client state changed since its baseline')
    else:
        # Stock app 5.1.0.6: a clientId is its peer's public key, and besides the shared
        # clients its installer adds one "Admin [platform]" client for the app itself.
        if set(state['clients']) != set(state['peers']):
            refusals.append('native clients and live peers differ')
        others = [name for key, name in state['clients'].items() if key != retained]
        if len(others) != 1 or not others[0].startswith(ADMIN_PREFIX):
            refusals.append('native clients are not exactly the retained client and the administrator')
    if refusals:
        return '\n'.join(refusals)
    if mode == 'admin-baseline':
        descriptor = os.open(baseline_path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(descriptor, 'w') as output:
            os.fchmod(output.fileno(), 0o600)
            json.dump(dict(state=state, retained=retained, revoked=revoked), output)
    print(f'ok: {mode} clients={len(state["clients"])} peers={len(state["peers"])} retained=present revoked=absent')
    return 0


if __name__ == '__main__':
    sys.exit(main(Path(sys.argv[1]), sys.argv[2]))
