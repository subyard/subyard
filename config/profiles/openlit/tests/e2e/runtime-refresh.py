#!/usr/bin/env python3
"""Exercise actual guest CAS, collector bind replacement, update and rollback."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile


STATE = Path('/var/lib/subyard/openlit')
UNIT = Path('/etc/systemd/system/subyard-openlit.service')
FILES = ('guest.sh', 'native.py', 'runtime.py', 'tomli_w.py', 'compose.yaml', 'collector.yaml')


def command(arguments, success=True):
    result = subprocess.run(arguments, capture_output=True, timeout=115)
    if (result.returncode == 0) != success:
        raise RuntimeError('guest runtime acceptance command had an unexpected result')
    return result.stdout


def receipt():
    return json.loads((STATE/'receipt.json').read_text())


def main():
    assert os.getuid() == 0
    with tempfile.TemporaryDirectory(prefix='subyard-openlit-runtime-e2e-', dir='/tmp') as directory:
        source = Path(directory)
        for name in FILES:
            shutil.copyfile(STATE/name, source/name)
        shutil.copyfile(UNIT, source/UNIT.name)
        def observe():
            return json.loads(command(['python3', '-B', str(source/'runtime.py'), 'observe',
                '--source', str(source), '--state', str(STATE), '--unit', str(UNIT),
                '--home', '/home/dev', '--user', 'dev', '--selected', '1', '--agents', 'claude codex']))
        def apply(assessment, success=True):
            output = command(['timeout', '--signal=TERM', '--kill-after=2s', '110', 'bash',
                str(source/'guest.sh'), 'refresh', '1', 'dev', 'claude codex',
                assessment['actual'], assessment['desired']], success)
            if success:
                value = json.loads(output)
                assert value['state'] == 'current' and value['actual'] == assessment['desired']
                assert observe() == value

        original = observe()
        assert original['state'] == 'current'
        original_receipt = receipt()
        original_asset = (source/'collector.yaml').read_bytes()
        updated_asset = original_asset+b'\n# Acceptance runtime recipe update.\n'
        (source/'collector.yaml').write_bytes(updated_asset)
        approved = observe()
        assert approved['state'] == 'stale' and approved['desired'] != original['desired']
        # A changed desired recipe invalidates approved CAS before guest mutation.
        (source/'collector.yaml').write_bytes(updated_asset+b'\n# Changed assessment.\n')
        apply(approved, success=False)
        assert receipt() == original_receipt
        assert (STATE/'collector.yaml').read_bytes() == original_asset
        (source/'collector.yaml').write_bytes(updated_asset)
        # Reproduce the old publication-before-apply failure with an atomic host
        # replacement. The old healthy container retains the original bind inode.
        temporary = STATE/'collector.yaml.acceptance'
        temporary.write_bytes(updated_asset)
        temporary.chmod(0o644)
        temporary.replace(STATE/'collector.yaml')
        stale = observe()
        assert stale['state'] == 'stale' and receipt() == original_receipt
        apply(stale)
        updated_receipt = receipt()
        assert updated_receipt['containers'] != original_receipt['containers']
        print('ok: guest CAS rejects changed source; actual collector bind and receipt converge after update')
        (source/'collector.yaml').write_bytes(original_asset)
        rollback = observe()
        assert rollback['state'] == 'stale' and rollback['desired'] == original['desired']
        apply(rollback)
        assert receipt()['desired'] == original_receipt['desired']
        assert receipt()['containers'] != updated_receipt['containers']
        print('ok: guest runtime rollback restores the shipped recipe with verified container identity')


if __name__ == '__main__':
    main()
