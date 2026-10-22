#!/usr/bin/env python3
"""Exercise owner cleanup diagnostics without provisioning or changing its exits."""
from pathlib import Path
import re
import subprocess
import unittest

OWNER = Path(__file__).parent/'e2e/owner.sh'


class OwnerDiagnosticsTest(unittest.TestCase):
    def test_cleanup_distinguishes_exit_and_signals_without_replacing_status(self):
        source = OWNER.read_text()
        # Execute the actual cleanup and its trap registrations with no owned
        # resources. The full owner entrypoint requires an allocated VM.
        cleanup = source[source.index('\ncleanup() {'):source.index('\nensure_owner_incus\n')]
        setup = '''set -euo pipefail
STATE='' YARD_NAME='' secondary_name='' occupied_pid='' tunnel_pid=''
synthetic_address='' synthetic_address_active='' auth_only='' native_loss='' kind=container
SECONDS=17
'''
        for command, trigger, status in (
                ('exit 0', 'EXIT', 0),
                ('exit 7', 'EXIT', 7),
                ('kill -TERM $$', 'TERM', 0),
                ('kill -INT $$', 'INT', 0),
                ('bash -c \'kill -TERM "$PPID"; exit 23\'', 'TERM', 23),
                ('bash -c \'kill -INT "$PPID"; exit 23\'', 'INT', 23)):
            with self.subTest(trigger=trigger):
                result = subprocess.run(['bash', '-c', setup+cleanup+'\n'+command],
                                        capture_output=True, text=True, timeout=5)
                self.assertEqual(result.returncode, status, result.stderr)
                self.assertEqual(result.stdout, '')
                match = re.fullmatch(
                    r'openlit_e2e_cleanup_entry trigger='+trigger
                    +r' elapsed_seconds=([0-9]+) exit_code='+str(status)+r'\n', result.stderr)
                self.assertIsNotNone(match, result.stderr)
                self.assertGreaterEqual(int(match[1]), 17)

    def test_closed_stderr_preserves_failure_and_owned_child_cleanup(self):
        source = OWNER.read_text()
        cleanup = source[source.index('\ncleanup() {'):source.index('\nensure_owner_incus\n')]
        setup = '''set -euo pipefail
STATE='' YARD_NAME='' secondary_name='' occupied_pid=fixture tunnel_pid=''
synthetic_address='' synthetic_address_active='' auth_only='' native_loss='' kind=container
kill() { printf 'kill-owned-child\\n'; }
wait() { printf 'wait-owned-child\\n'; }
'''
        result = subprocess.run(['bash', '-c', setup+cleanup+'\nexec 2>&-\nexit 7'],
                                capture_output=True, text=True, timeout=5)
        self.assertEqual(result.returncode, 7)
        self.assertEqual(result.stdout, 'kill-owned-child\nwait-owned-child\n')
        self.assertEqual(result.stderr, '')


if __name__ == '__main__':
    unittest.main()
