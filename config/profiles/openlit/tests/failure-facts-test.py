#!/usr/bin/env python3
"""Native diagnostic failures remain bounded and never expose private output."""
import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
import types
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('facts', Path(__file__).parent/'e2e/failure-facts.py')
facts = importlib.util.module_from_spec(spec)
spec.loader.exec_module(facts)


class FailureFactsTest(unittest.TestCase):
    def test_registry_codes_and_timeouts_do_not_expose_credentials(self):
        calls = []
        def run(argv, **options):
            calls.append((argv, options))
            if 'ghcr.io/token' in argv[-1]:
                raise subprocess.TimeoutExpired(argv, 12, output=b'PRIVATE_SENTINEL', stderr=b'PRIVATE_SENTINEL')
            return subprocess.CompletedProcess(argv, 28 if len(calls) == 1 else 0,
                b'000' if len(calls) == 1 else b'401', b'PRIVATE_SENTINEL')
        with patch.object(facts.subprocess, 'run', side_effect=run):
            value = facts.registries()
        self.assertEqual(value['dockerhub'], {'exit_code': 28, 'http_status': 0})
        self.assertEqual(value['ghcr'], {'exit_code': 0, 'http_status': 401})
        self.assertEqual(value['ghcr-auth'], {'category': 'TimeoutExpired'})
        self.assertNotIn('PRIVATE_SENTINEL', json.dumps(value))
        self.assertEqual(len(calls), 4)
        self.assertTrue(all(options['timeout'] == 12 and '--max-time' in argv for argv, options in calls))

    def test_failure_projects_fixed_progress_cache_and_oom_facts(self):
        with tempfile.TemporaryDirectory(prefix='openlit-facts-', dir='/tmp') as directory:
            state = Path(directory)
            state.chmod(0o700)
            (state/'marker').write_text('subyard-openlit-v1\n')
            (state/'pull.log').write_text('PRIVATE_SENTINEL\n clickhouse Pulling\n openlit Pulling\n collector Error\nTLS handshake timeout\n')
            original = Path.stat
            def stat(path, **options):
                value = original(path, **options)
                return types.SimpleNamespace(st_uid=0, st_mode=value.st_mode) if path == state else value
            def run(argv, **options):
                if argv[0] == 'journalctl':
                    return subprocess.CompletedProcess(argv, 0, b'PRIVATE_SENTINEL\nOut of memory: Killed process\n', b'')
                return subprocess.CompletedProcess(argv, 1, b'PRIVATE_SENTINEL', b'PRIVATE_SENTINEL')
            with patch.object(Path, 'stat', stat), patch.object(facts, 'registries', return_value={}), patch.object(facts.subprocess, 'run', side_effect=run):
                value = facts.failure(state, ['a', 'b', 'c'])
            self.assertEqual(value['pull_category'], 'timeout')
            self.assertEqual(value['pull_progress'], [('clickhouse', 'Pulling'), ('collector', 'Error'), ('openlit', 'Pulling')])
            self.assertEqual(value['image_cache']['openlit'], {'inspect_exit': 1})
            self.assertTrue(value['journal_categories']['kernel']['oom_present'])
            self.assertFalse(value['files_present']['receipt.json'])
            self.assertNotIn('PRIVATE_SENTINEL', json.dumps(value))
            # A symlinked runtime directory must not expose even pull-log facts.
            link = state/'link'
            link.symlink_to(state, target_is_directory=True)
            with patch.object(facts, 'registries', return_value={}), patch.object(facts, 'cache', return_value={}), patch.object(facts, 'journals', return_value={}):
                unsafe = facts.failure(link, [])
            self.assertFalse(unsafe['runtime_directory_owned'])
            self.assertNotIn('pull_log_bytes', unsafe)


if __name__ == '__main__':
    unittest.main()
