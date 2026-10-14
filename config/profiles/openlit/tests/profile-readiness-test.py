#!/usr/bin/env python3
"""Exercise public readiness polling through the real subprocess boundary."""
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

SCRIPT = Path(__file__).parent/'e2e/profile-readiness.py'
READY = {'yard': 'fixture', 'id': 'openlit', 'enabled': True, 'configured': True,
         'runtime': 'ready', 'ready': True}
PENDING = {**READY, 'runtime': 'pending', 'ready': False}


class ProfileReadinessTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='subyard-openlit-readiness-', dir='/tmp')
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.engine = self.root/'yard'
        self.engine.write_text('''#!/usr/bin/env python3
import json, pathlib, subprocess, sys, time
root = pathlib.Path(__file__).parent
with (root/'calls').open('a') as stream: stream.write(json.dumps(sys.argv[1:])+'\\n')
rows = json.loads((root/'responses').read_text())
row = rows.pop(0) if len(rows) > 1 else rows[0]
(root/'responses').write_text(json.dumps(rows or [row]))
if row.get('spawn'): subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(10)'])
time.sleep(row.get('delay', 0))
print(row.get('raw', json.dumps(row.get('value'))))
print('PRIVATE_SENTINEL', file=sys.stderr)
raise SystemExit(row.get('exit', 0))
''')
        self.engine.chmod(0o700)

    def run_helper(self, responses, mode='wait', timeout=3, failed_exit=None):
        (self.root/'responses').write_text(json.dumps(responses))
        argv = [sys.executable, '-B', str(SCRIPT), mode, '--engine', str(self.engine),
                '--timeout', str(timeout)]
        if mode == 'wait':
            argv += ['--yard', 'fixture']
        if failed_exit is not None:
            argv += ['--failed-exit', str(failed_exit)]
        result = subprocess.run(argv, capture_output=True, text=True, timeout=5)
        self.assertNotIn('PRIVATE_SENTINEL', result.stdout+result.stderr)
        return result

    def calls(self):
        return [json.loads(line) for line in (self.root/'calls').read_text().splitlines()]

    def test_pending_then_ready_uses_only_selected_public_status(self):
        result = self.run_helper([{'value': PENDING}, {'value': READY}])
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.calls(), [['-Y', 'fixture', 'profile', 'status', 'openlit', '--json']]*2)
        records = [json.loads(line.split('=', 1)[1]) for line in result.stdout.splitlines()]
        self.assertEqual([row['runtime'] for row in records], ['pending', 'ready'])
        self.assertNotIn('fixture', result.stdout)

    def test_pending_deadline_and_slow_child_share_one_bound(self):
        for row in ({'value': PENDING}, {'value': READY, 'delay': 1}, {'value': READY, 'spawn': True}):
            with self.subTest(row=row):
                result = self.run_helper([row], timeout=0.15)
                self.assertEqual(result.returncode, 124, result.stderr)
                self.assertIn('deadline', result.stdout)

    def test_unexpected_state_identity_type_or_projection_is_terminal(self):
        rows = [
            {'raw': 'PRIVATE_SENTINEL'}, {'raw': '{"id":"openlit","id":"openlit"}'},
            {'value': {**READY, 'yard': 'foreign'}}, {'value': {**READY, 'id': 'neighbor'}},
            {'value': {**READY, 'ready': 1}}, {'value': {**READY, 'ready': False}},
            {'value': {**PENDING, 'enabled': False}}, {'value': {**PENDING, 'configured': False}},
            {'value': {**PENDING, 'runtime': 'unavailable'}}, {'value': {**PENDING, 'runtime': 'removed'}},
            {'value': {**READY, 'extra': 'PRIVATE_SENTINEL'}}, {'value': READY, 'exit': 23},
        ]
        for row in rows:
            with self.subTest(row=row):
                (self.root/'calls').unlink(missing_ok=True)
                result = self.run_helper([row, {'value': READY}])
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(len(self.calls()), 1)
                if row.get('exit'):
                    self.assertEqual(result.returncode, 23)

    def test_release_diagnostics_are_host_wide_bounded_and_preserve_failure(self):
        report = {'schemaVersion': 1,
                  'outcome': {'status': 'migration-required', 'code': 'transition-required',
                              'reachedGoal': False, 'message': 'PRIVATE_SENTINEL'},
                  'blockers': [{'code': 'resource-conflict', 'resource': '/PRIVATE_SENTINEL',
                                'message': 'PRIVATE_SENTINEL'}],
                  'decisions': [{'scope': 'activation', 'resource': '/PRIVATE_SENTINEL',
                                 'decision': 'canonicalize', 'result': 'converged'}],
                  'consequences': ['PRIVATE_SENTINEL']}
        result = self.run_helper([{'value': report}], mode='diagnose', failed_exit=7)
        self.assertEqual(result.returncode, 7)
        self.assertEqual(self.calls(), [['migrate', '--check', '--json']])
        projection = json.loads(result.stdout.split('=', 1)[1])
        self.assertEqual(projection['blocker_codes'], {'resource-conflict': 1})
        self.assertEqual(projection['activation_decision_count'], 1)
        self.assertEqual(projection['failed_exit'], 7)
        for row in ({'value': report, 'exit': 23}, {'raw': 'PRIVATE_SENTINEL'},
                    {'value': {**report, 'schemaVersion': True}},
                    {'value': {**report, 'blockers': report['blockers']*33}},
                    {'value': {**report, 'decisions': report['decisions']*65}},
                    {'value': {**report, 'outcome': {**report['outcome'], 'code': []}}},
                    {'value': {**report, 'blockers': [{'code': []}]}},
                    {'value': {**report, 'outcome': {**report['outcome'], 'code': 'PRIVATE_SENTINEL'}}}):
            with self.subTest(row=row):
                result = self.run_helper([row], mode='diagnose', failed_exit=7)
                self.assertEqual(result.returncode, 7)
                self.assertIn('category', result.stdout)


if __name__ == '__main__':
    unittest.main()
