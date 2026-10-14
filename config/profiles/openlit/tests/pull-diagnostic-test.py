#!/usr/bin/env python3
"""An ordinary enable failure survives same-guest diagnostic recovery."""
from pathlib import Path
import os
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).parent/'e2e/pull-diagnostic.sh'


class PullDiagnosticTest(unittest.TestCase):
    def exercise(self, ordinary, fallback=0, failed_observation=''):
        with tempfile.TemporaryDirectory(prefix='subyard-openlit-pull-', dir='/tmp') as temporary:
            root = Path(temporary)
            package = root/'config/profiles/openlit'
            (package/'tests/e2e').mkdir(parents=True)
            (package/'compose.yaml').write_text('services: {}\n')
            (package/'tests/e2e/failure-facts.py').write_text('fixture input\n')
            (package/'tests/e2e/network-only.sh').write_text('printf "network\\n" >> "$PULL_TEST_LOG"\n')
            env = {**os.environ, 'PULL_TEST_LOG': str(root/'calls'),
                   'PULL_TEST_ORDINARY': str(ordinary), 'PULL_TEST_FALLBACK': str(fallback),
                   'PULL_TEST_BAD_OBSERVATION': failed_observation}
            driver = '''
set -euo pipefail
. "$1"
yard() { printf 'ordinary:%s\n' "$*" >> "$PULL_TEST_LOG"; return "$PULL_TEST_ORDINARY"; }
incus() {
  local arg action='' phase=''
  for arg in "$@"; do
    case "$arg" in context|dependency) action=$arg ;; before|after-ordinary|after-fallback) phase=$arg ;; esac
  done
  cat >/dev/null
  printf '%s:%s\n' "$action" "$phase" >> "$PULL_TEST_LOG"
  if [ "$action" = dependency ]; then return "$PULL_TEST_FALLBACK"; fi
  [ "$phase" != "$PULL_TEST_BAD_OBSERVATION" ] || return 23
}
timeout() {
  printf 'timeout:%s %s %s\n' "$1" "$2" "$3" >> "$PULL_TEST_LOG"
  shift 3
  "$@"
}
openlit_pull_diagnostic "$2" "$2" instance project image1 image2 image3
'''
            result = subprocess.run(['bash', '-c', driver, 'fixture', str(SCRIPT), str(root)],
                                    env=env, capture_output=True, text=True, timeout=5)
            return result, (root/'calls').read_text().splitlines()

    def test_fallback_success_does_not_pass_or_replace_original_failure(self):
        for ordinary, fallback in ((1, 0), (23, 2)):
            with self.subTest(ordinary=ordinary, fallback=fallback):
                result, calls = self.exercise(ordinary, fallback)
                self.assertEqual(result.returncode, ordinary, result.stderr)
                self.assertEqual(sum(row.startswith('ordinary:') for row in calls), 1)
                self.assertEqual(calls.count('dependency:'), 1)
                self.assertIn('context:after-ordinary', calls)
                self.assertIn('context:after-fallback', calls)
                self.assertIn('CACHE_REUSED', result.stdout)
                self.assertIn('original-enable-exit='+str(ordinary), result.stdout)
                self.assertIn('quiet-causality=unknown', result.stdout)

    def test_ordinary_success_stops_without_fallback_or_full_acceptance(self):
        result, calls = self.exercise(0)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn('dependency:', calls)
        self.assertEqual(calls.count('network'), 2)
        self.assertIn('native-events-and-rendered-UI=not-tested', result.stdout)
        self.assertTrue(all(row == 'timeout:--signal=TERM --kill-after=5 175'
                            for row in calls if row.startswith('timeout:')))

    def test_post_observation_failure_does_not_skip_network_or_hide_enable_failure(self):
        for ordinary in (0, 1):
            result, calls = self.exercise(ordinary, failed_observation='after-ordinary')
            self.assertEqual(result.returncode, ordinary or 2, result.stderr)
            self.assertIn('context:after-ordinary', calls)
            self.assertEqual(calls.count('network'), 3 if ordinary else 2)
            self.assertEqual(calls.count('dependency:'), 1 if ordinary else 0)

    def test_failed_preflight_never_enables_or_pulls(self):
        result, calls = self.exercise(0, failed_observation='before')
        self.assertEqual(result.returncode, 2)
        self.assertEqual(calls.count('network'), 1)
        self.assertFalse(any(row.startswith('ordinary:') or row == 'dependency:' for row in calls))


if __name__ == '__main__':
    unittest.main()
