#!/usr/bin/env python3
"""Exercise actual guest CAS, collector bind replacement, update and rollback."""
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import time


STATE = Path('/var/lib/subyard/openlit')
UNIT = Path('/etc/systemd/system/subyard-openlit.service')
FILES = ('guest.sh', 'native.py', 'runtime.py', 'trace_cutover.py', 'account.py', 'tomli_w.py', 'compose.yaml', 'collector.yaml', 'openlit-collector.yaml', 'subyard-openlit-trace-cutover.service', 'subyard-openlit-trace-cutover.timer')


def command_failure(phase, success, returncode, stderr, started, category):
    # Child output can contain private state. Project only closed stage/reason labels.
    stderr = stderr or b''
    if isinstance(stderr, bytes):
        stderr = stderr.decode(errors='replace')
    stages = re.findall(r'^openlit: guest stage=(guard|image-inventory|image-pull|assets|native-disable|compose-start|account-bootstrap|unit-start|native-enable|publication)$', stderr, re.MULTILINE)
    stage = stages[-1] if stages else 'unknown'
    reason = 'unknown'
    if stage == 'compose-start':
        for label, pattern in (
                ('wait-deadline', r'^(?:application not healthy after [0-9a-z.]+|timed out waiting for .+)$'),
                ('unhealthy', r'^(?:dependency failed to start: )?container .+ is unhealthy$'),
                ('dependency-start', r'^dependency failed to start: .+$')):
            if re.search(pattern, stderr[-65536:], re.MULTILINE):
                reason = label
                break
    details = {'phase': phase, 'expected': 'success' if success else 'refusal',
               'returncode': returncode, 'category': category,
               'elapsed_ms': max(0, int((time.monotonic()-started)*1000)),
               'guest_stage': stage, 'compose_reason': reason}
    return RuntimeError('guest runtime acceptance command had an unexpected result: '+json.dumps(details, sort_keys=True))


def command(arguments, success=True, phase='observe'):
    if phase not in ('observe', 'refresh'):
        raise ValueError('unsupported runtime acceptance phase')
    started = time.monotonic()
    try:
        result = subprocess.run(arguments, capture_output=True, timeout=115)
    except subprocess.TimeoutExpired as error:
        raise command_failure(phase, success, None, error.stderr, started, 'parent-timeout') from None
    if (result.returncode == 0) != success:
        category = 'exit' if result.returncode else 'unexpected-success'
        raise command_failure(phase, success, result.returncode, result.stderr, started, category) from None
    return result.stdout


def receipt():
    return json.loads((STATE/'receipt.json').read_text())


def observation_failure(phase, observation, expected=None):
    # Observations contain private fingerprints; report only closed predicates.
    if phase not in ('baseline', 'post-apply'):
        raise ValueError('unsupported runtime acceptance comparison')
    state = observation.get('state') if isinstance(observation, dict) else None
    shape = (isinstance(observation, dict) and set(observation) == {'state', 'actual', 'desired'}
             and all(isinstance(value, str) for value in observation.values()))
    details = {'phase': phase, 'category': 'not-current' if phase == 'baseline' else 'observation-mismatch',
               'state': state if state in ('current', 'stale', 'absent') else 'unknown', 'shape': shape}
    if expected is not None:
        details['keys_match_returned'] = isinstance(observation, dict) and observation.keys() == expected.keys()
        for field in ('state', 'actual', 'desired'):
            details[field+'_matches_returned'] = (isinstance(observation, dict) and field in observation
                                                  and field in expected and observation[field] == expected[field])
    return 'guest runtime acceptance observation failed: '+json.dumps(details, sort_keys=True)


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
                assessment['actual'], assessment['desired']], success, phase='refresh')
            if success:
                value = json.loads(output)
                assert value['state'] == 'current' and value['actual'] == assessment['desired']
                assert (verified := observe()) == value, observation_failure('post-apply', verified, value)

        assets = {'collector.yaml': 0o644, 'openlit-collector.yaml': 0o600}
        original = observe()
        assert original['state'] == 'current', observation_failure('baseline', original)
        original_receipt = receipt()
        before = {name: (source/name).read_bytes() for name in assets}
        updated = {name: content+b'\n# Acceptance runtime recipe update.\n' for name, content in before.items()}
        for name in assets:
            (source/name).write_bytes(updated[name])
        approved = observe()
        assert approved['state'] == 'stale' and approved['desired'] != original['desired']
        # One changed source invalidates the entire approved two-asset recipe.
        (source/'collector.yaml').write_bytes(updated['collector.yaml']+b'# Changed assessment.\n')
        apply(approved, success=False)
        assert receipt() == original_receipt
        assert all((STATE/name).read_bytes() == before[name] for name in assets)
        (source/'collector.yaml').write_bytes(updated['collector.yaml'])
        # Publish both host paths atomically while old containers retain old binds.
        for name, permissions in assets.items():
            previous = (STATE/name).stat().st_ino
            temporary = STATE/(name+'.acceptance')
            temporary.write_bytes(updated[name])
            temporary.chmod(permissions)
            temporary.replace(STATE/name)
            assert (STATE/name).stat().st_ino != previous
            assert (STATE/name).stat().st_mode & 0o777 == permissions
        stale = observe()
        assert stale['state'] == 'stale' and receipt() == original_receipt
        # Fresh runtime verification reads both mounted inodes before publication.
        apply(stale)
        updated_receipt = receipt()
        assert updated_receipt['containers'] != original_receipt['containers']
        for name, permissions in assets.items():
            assert (STATE/name).read_bytes() == updated[name]
            assert (STATE/name).stat().st_mode & 0o777 == permissions
            print('ok: guest CAS rejects changed source; '+name+' bind and receipt converge after update')
            (source/name).write_bytes(before[name])
        rollback = observe()
        assert rollback['state'] == 'stale' and rollback['desired'] == original['desired']
        apply(rollback)
        assert receipt()['desired'] == original_receipt['desired']
        assert receipt()['containers'] != updated_receipt['containers']
        for name, permissions in assets.items():
            assert (STATE/name).read_bytes() == before[name]
            assert (STATE/name).stat().st_mode & 0o777 == permissions
            print('ok: '+name+' rollback restores the shipped recipe with verified container identity')



if __name__ == '__main__':
    main()
