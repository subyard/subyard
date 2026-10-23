#!/usr/bin/env python3
"""Run the real shebang boundary; native failure and capture are independent."""
import hashlib
import json
import os
from pathlib import Path
import signal
import subprocess
import tempfile
import unittest


WRAPPER = Path(__file__).with_name('runtime-hook-trace.py')


class HookTraceTest(unittest.TestCase):
    def test_setting_boundary_preserves_native_status_without_hook_observations(self):
        source = WRAPPER.with_name('release-activation-guest.sh').read_text()
        boundary = 'set_git_mode() {'+source.split('set_git_mode() {',1)[1].split('\n}\n',1)[0]+'\n}\n'
        self.source.write_text('authenticated fixture source\n')
        for status in (0,23):
            with self.subTest(native_exit=status):
                prefix = self.root/f'setting-{status}'
                environment = dict(self.environment,STATE=str(self.root),MODE='normal',ROOT=str(WRAPPER.parents[5]),
                    FINAL_SOURCE=str(self.root),TRACE_REAL_BASH='/bin/bash',NATIVE_EXIT=str(status),
                    EXPECTED_SOURCE=str(self.source),MIGRATION_MARKER=str(prefix)+'-migrated')
                script = r'''
hash() { sha256sum "$EXPECTED_SOURCE" | cut -d' ' -f1; }
yard() { if [ "$1" = config ]; then return "$NATIVE_EXIT"; fi; printf migrated > "$MIGRATION_MARKER"; }
claim_units() { :; }
die() { exit 2; }
'''+boundary+'\nset_git_mode broker-first "$1"\n'
                result = subprocess.run(['/bin/bash','-euc',script,'fixture',str(prefix)],
                    env=environment,capture_output=True,text=True,timeout=5)
                self.assertEqual(result.returncode,status,result.stderr)
                capture = json.loads(Path(str(prefix)+'-capture.log').read_text())
                self.assertEqual(capture,dict(status='not-observed',observations=0,completed=0,incomplete=0))
                self.assertEqual(Path(str(prefix)+'-migrated').exists(),status == 0)

    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='subyard-hook-trace-', dir='/tmp')
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.source = self.root/'runtime.sh'
        self.log = self.root/'capture.jsonl'
        wrappers = self.root/'wrappers'
        wrappers.mkdir()
        wrapper = wrappers/'bash'
        wrapper.write_bytes(WRAPPER.read_bytes())
        wrapper.chmod(0o700)
        self.environment = dict(os.environ, PATH=str(wrappers)+':'+os.environ['PATH'],
            SUBYARD_TRACE_REAL_BASH='/bin/bash', SUBYARD_TRACE_HOOK_SOURCE=str(self.source),
            SUBYARD_TRACE_HOOK_LOG=str(self.log))

    def invoke(self, body, arguments=('observe',), authenticate=True):
        self.source.write_text('#!/usr/bin/env bash\n'+body)
        self.source.chmod(0o700)
        self.environment['SUBYARD_TRACE_HOOK_SHA256'] = (
            hashlib.sha256(self.source.read_bytes()).hexdigest() if authenticate else '0'*64)
        before = self.source.read_bytes()
        result = subprocess.run([str(self.source), *arguments], env=self.environment,
                                capture_output=True, timeout=5)
        self.assertEqual(self.source.read_bytes(), before)
        return result

    def records(self):
        return [json.loads(line) for line in self.log.read_text().splitlines()]

    def test_actual_failure_preserves_native_output_and_classifies_only_fixed_reason(self):
        result = self.invoke("printf '{\"state\":\"absent\"}\\n'\nprintf '%s\\n' 'git-fetch: foreign loaded service unit; inspect owned timer state' 'credential-sentinel-do-not-capture' >&2\nexit 2\n")
        self.assertEqual(result.returncode, 2)
        self.assertEqual(result.stdout, b'{"state":"absent"}\n')
        self.assertEqual(result.stderr, b'git-fetch: foreign loaded service unit; inspect owned timer state\ncredential-sentinel-do-not-capture\n')
        records = self.records()
        self.assertEqual(records[0]['kind'], 'start')
        end = records[1]
        self.assertEqual(end['exit_code'], 2)
        self.assertEqual(end['reasons'], ['foreign loaded service unit'])
        self.assertGreaterEqual(end['duration_ms'], 0)
        self.assertNotIn('credential-sentinel', self.log.read_text())
        verified = subprocess.run(['/usr/bin/python3', str(WRAPPER), '--verify', str(self.log)], capture_output=True)
        self.assertEqual(verified.returncode, 0, verified.stderr)

    def test_nonmatching_and_changed_source_delegate_without_tracing(self):
        result = self.invoke('printf delegated\nexit 7\n', ('apply',))
        self.assertEqual((result.returncode, result.stdout), (7, b'delegated'))
        self.assertFalse(self.log.exists())
        result = self.invoke('printf unchanged\nexit 9\n', authenticate=False)
        self.assertEqual((result.returncode, result.stdout), (9, b'unchanged'))
        self.assertEqual([item['kind'] for item in self.records()], ['authentication-failed'])

    def test_success_signal_oversize_and_capture_failure_preserve_status(self):
        result = self.invoke('printf current\nexit 0\n')
        self.assertEqual((result.returncode, result.stdout), (0, b'current'))
        result = self.invoke('kill -TERM $$\n')
        self.assertEqual(result.returncode, -signal.SIGTERM)
        self.assertEqual(self.records()[-1]['exit_code'], -signal.SIGTERM)
        result = self.invoke('kill -KILL $$\n')
        self.assertEqual(result.returncode, -signal.SIGKILL)
        self.assertEqual(self.records()[-1]['exit_code'], -signal.SIGKILL)
        result = self.invoke("printf '%300s' x >&2\nprintf '\\n' >&2\nexit 3\n")
        self.assertEqual(result.returncode, 3)
        self.assertEqual(self.records()[-1]['reasons'], [])
        self.log.chmod(0o644)
        result = self.invoke('exit 6\n')
        self.assertEqual(result.returncode, 6)

    def test_pinned_path_resolves_to_authenticated_target(self):
        self.source.write_text('#!/usr/bin/env bash\nprintf pinned\n')
        self.source.chmod(0o700)
        self.environment['SUBYARD_TRACE_HOOK_SHA256'] = hashlib.sha256(self.source.read_bytes()).hexdigest()
        fd = os.open(self.root, os.O_RDONLY | os.O_DIRECTORY)
        try:
            pinned = f'/proc/{os.getpid()}/fd/{fd}/runtime.sh'
            result = subprocess.run([pinned, 'observe'], env=self.environment,
                                    capture_output=True, timeout=5)
        finally:
            os.close(fd)
        self.assertEqual((result.returncode, result.stdout), (0, b'pinned'))
        self.assertEqual(self.records()[-1]['exit_code'], 0)


if __name__ == '__main__':
    unittest.main()
