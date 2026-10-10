#!/usr/bin/python3
"""Host-free evidence retention and cleanup boundary checks; no guest access."""
import base64
import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile

sys.dont_write_bytecode = True
ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('sleep', ROOT / 'dev/e2e/veranda-sleep.py')
sleep = importlib.util.module_from_spec(spec)
spec.loader.exec_module(sleep)


def private(path, data):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    os.fchmod(fd, 0o600)
    with os.fdopen(fd, 'wb') as stream:
        stream.write(data)


def rejected(call):
    try:
        call()
    except (OSError, ValueError):
        return
    raise AssertionError('unsafe or missing artifact accepted')


with tempfile.TemporaryDirectory(prefix='veranda-sleep-evidence-') as directory:
    root = Path(directory)
    log = root / 'native.log'
    data = b'original native assertion failure\n'
    private(log, data)
    copied = sleep.evidence_log(log, os.geteuid())
    assert base64.b64decode(copied['data_base64']) == data and not copied['truncated'] and copied['stable']
    rejected(lambda: sleep.evidence_log(root / 'absent', os.geteuid()))
    (root / 'link').symlink_to(log)
    rejected(lambda: sleep.evidence_log(root / 'link', os.geteuid()))
    log.chmod(0o644)
    rejected(lambda: sleep.evidence_log(log, os.geteuid()))
    log.chmod(0o600)
    with log.open('r+b') as stream:
        stream.truncate(8 * 1024 * 1024 + 1)
    copied = sleep.evidence_log(log, os.geteuid())
    assert copied['truncated'] and copied['original_bytes'] == 8 * 1024 * 1024 + 1
    log.write_bytes(data)
    private(root / 'receipt.json', b'{"result":"failed","native_exit_code":101}')
    original_context, original_log = sleep.context, sleep.evidence_log
    sleep.context = lambda **_: (root, root / 'src', 'a' * 8, '1')
    sleep.evidence_log = lambda path: original_log(path, os.geteuid())
    output = io.StringIO()
    try:
        with contextlib.redirect_stdout(output):
            assert sleep.export_evidence() == 0
    finally:
        sleep.context, sleep.evidence_log = original_context, original_log
    frame = json.loads(output.getvalue())
    assert frame['result'] == 'complete' and frame['artifacts'][1]['name'] == 'native.log'
    assert base64.b64decode(frame['artifacts'][1]['data_base64']) == data

    # Run the actual wrapper boundary, replacing only its external guest edges.
    source = (ROOT / 'dev/e2e/veranda-sleep-acceptance.sh').read_text()
    functions = source[source.index('retain_evidence() {'):source.index('trap cleanup_acceptance EXIT')]
    for original, invalid, cleanup_failure in [(17, False, False), (17, True, False), (0, True, False),
                                                (0, False, True), (0, 'truncated', False), (17, 'unsafe', False)]:
        case = root / f'{original}-{invalid}-{cleanup_failure}'
        case.mkdir(mode=0o700)
        evidence = case / 'evidence'; evidence.mkdir(mode=0o700)
        frames = case / 'frames'; frames.mkdir(mode=0o700)
        for vm in [1, 2]:
            value = json.loads(json.dumps(frame)) if vm == 1 else {'schema_version': 1, 'vm': 2, 'result': 'complete',
                                           'artifacts': [dict(frame['artifacts'][1], name='owner-setup.log')]}
            if invalid is True and vm == 1:
                value = {'schema_version': 1, 'vm': 1, 'result': 'complete', 'artifacts': []}
            elif invalid == 'unsafe' and vm == 1:
                value['artifacts'][0]['name'] = '../escape'
            elif invalid == 'truncated' and vm == 1:
                value['result'] = 'incomplete'
                value['artifacts'][1]['truncated'] = True
                value['artifacts'][1]['original_bytes'] += 1
            private(frames / f'vm-{vm}.json', json.dumps(value).encode())
        script = '''set -u
EVIDENCE_DIR="$1"; FRAMES="$2"; TRACE="$3"; LOCAL_TEMP="$4"; FAIL="$5"
declare -A GUEST_DIRS=([1]=owned-vm1 [2]=owned-vm2)
owner_setup=1; owner_ready=0; client_started=1; evidence_failed=0
phase_start(){ :; }; phase_end(){ :; }
payload(){ cat "$FRAMES/vm-$1.json"; }
preview(){ printf 'preview-cleanup\n' >> "$TRACE"; }
cleanup_guest(){ printf 'cleanup-%s\n' "$1" >> "$TRACE"; [ "$1:$FAIL" != 1:True ]; }
release_lease(){ printf 'release\n' >> "$TRACE"; }
''' + functions + '\ntrap cleanup_acceptance EXIT\nexit "$6"\n'
        with tempfile.TemporaryDirectory(prefix='subyard-agent-e2e.') as local:
            run = subprocess.run(['bash', '-c', script, 'check', str(evidence), str(frames),
                                  str(case / 'trace'), local, str(cleanup_failure), str(original)],
                                 capture_output=True, text=True, timeout=10)
        assert run.returncode == (original or (3 if invalid or cleanup_failure else 0)), run.stderr
        summary = json.loads((evidence / 'summary.json').read_text())
        assert summary['original_exit_code'] == original
        assert summary['evidence_result'] == ('failed' if invalid else 'complete')
        assert summary['cleanup_failed'] == cleanup_failure
        assert (case / 'trace').read_text().splitlines() == ['cleanup-1', 'preview-cleanup', 'cleanup-2', 'release']
        assert (evidence / 'vm-2-owner-setup.log').read_bytes() == data
        if not invalid:
            assert (evidence / 'vm-1-native.log').read_bytes() == data
        if invalid == 'truncated':
            retained = json.loads((evidence / 'vm-1.summary.json').read_text())
            assert retained['result'] == 'incomplete' and retained['artifacts'][1]['truncated']
            assert (evidence / 'vm-1-native.log').read_bytes() == data
        assert not (case / 'escape').exists()
        assert all(path.stat().st_mode & 0o777 == 0o600 for path in evidence.iterdir())

print('ok: private complete/truncated/unsafe evidence, native failure retention, independent cleanup and original rc')
