#!/usr/bin/env python3
"""Network diagnostics project safe results and keep direct-IP probes bounded."""
import contextlib
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import unittest
from unittest.mock import patch

SCRIPT = Path(__file__).parent/'e2e/network-facts.py'
spec = importlib.util.spec_from_file_location('network_facts', SCRIPT)
facts = importlib.util.module_from_spec(spec)
spec.loader.exec_module(facts)


class NetworkFactsTest(unittest.TestCase):
    def test_probe_modes_keep_host_sni_discard_tokens_and_redact_stderr(self):
        calls = []
        def run(argv, **options):
            calls.append((argv, options))
            code = 200 if '/token?' in argv[-1] else 401
            return subprocess.CompletedProcess(argv, 0, f'{code} 0.01 0.02 0.03 0.04 44.1.2.3'.encode(), b'PRIVATE_SENTINEL')
        output = io.StringIO()
        with patch.object(facts, 'registry_addresses', return_value=['44.1.2.4']), patch.object(facts, 'route_facts', return_value={'observed': False}), patch.object(facts.subprocess, 'run', side_effect=run), contextlib.redirect_stdout(output):
            facts.probe('guest', ['44.1.2.3'])
        text = output.getvalue()
        self.assertNotIn('PRIVATE_SENTINEL', text)
        records = [json.loads(line.split('=', 1)[1]) for line in text.splitlines()]
        self.assertEqual(records[0]['shared_dns_matches'], 0)
        self.assertEqual(records[0]['dns_ipv4_count'], 1)
        resolved = next(row for row in records if row['mode'] == 'registry-direct-resolved-ipv4')
        self.assertEqual(resolved['resolved_ipv4'], '44.1.2.3')
        self.assertEqual(resolved['tls_stage'], 'http-response')
        self.assertEqual(resolved['remote_ipv4'], '44.1.2.3')
        default = next(row for row in records if row['mode'] == 'registry-default')
        self.assertNotIn('remote_ipv4', default)
        self.assertTrue(all(row['location'] == 'guest' for row in records))
        self.assertTrue(all(options['timeout'] == 11 for _, options in calls))
        self.assertEqual(len(calls), 7)
        fixed = next(argv for argv, _ in calls if '--resolve' in argv)
        self.assertIn('registry-1.docker.io:443:44.1.2.3', fixed)
        self.assertEqual(fixed[-1], 'https://registry-1.docker.io/v2/')
        self.assertIn('--noproxy', fixed)
        self.assertIn('--output', fixed)
        self.assertIn('/dev/null', fixed)
        self.assertTrue(all('/blobs/' not in argv[-1] and '/manifests/' not in argv[-1] for argv, _ in calls))

    def test_timeout_and_partial_handshake_are_not_connectivity_success(self):
        with patch.object(facts.subprocess, 'run', return_value=subprocess.CompletedProcess([], 28, b'000 0.02 0.03 0 10.0 44.1.2.3', b'PRIVATE_SENTINEL')):
            partial = facts.curl_facts('https://registry-1.docker.io/v2/', [])
        self.assertEqual(partial['exit_code'], 28)
        self.assertEqual(partial['http_status'], 0)
        self.assertEqual(partial['tls_stage'], 'tcp-connected')
        with patch.object(facts.subprocess, 'run', side_effect=subprocess.TimeoutExpired([], 11, output=b'PRIVATE_SENTINEL', stderr=b'PRIVATE_SENTINEL')):
            timed = facts.curl_facts('https://registry-1.docker.io/v2/', [])
        self.assertEqual(timed, {'category': 'TimeoutExpired', 'tls_stage': 'unknown'})
        self.assertNotIn('PRIVATE_SENTINEL', json.dumps((partial, timed)))

    def test_bad_curl_projection_and_private_remote_addresses_are_hidden(self):
        for raw in (b'PRIVATE_SENTINEL', b'401 PRIVATE_SENTINEL 0.1 0.2 0.3'):
            with patch.object(facts.subprocess, 'run', return_value=subprocess.CompletedProcess([], 0, raw, b'')):
                value = facts.curl_facts('https://registry-1.docker.io/v2/', [])
            self.assertEqual(value['category'], 'invalid-curl-projection')
            self.assertNotIn('PRIVATE_SENTINEL', json.dumps(value))
        with patch.object(facts.subprocess, 'run', return_value=subprocess.CompletedProcess([], 0, b'401 0 0.1 0.2 0.3 10.2.3.4', b'')):
            value = facts.curl_facts('https://registry-1.docker.io/v2/', [])
        self.assertFalse(value['remote_public_ipv4'])
        self.assertNotIn('10.2.3.4', json.dumps(value))

    def test_dns_projection_is_public_ipv4_only_and_bounded(self):
        raw = '\n'.join(f'{address} STREAM PRIVATE_SENTINEL' for address in ('10.0.0.1', '44.1.2.6', '44.1.2.5', '44.1.2.4', '44.1.2.3', '44.1.2.2', '::1'))
        with patch.object(facts.subprocess, 'run', return_value=subprocess.CompletedProcess([], 0, raw.encode(), b'PRIVATE_SENTINEL')) as run:
            addresses = facts.registry_addresses()
        self.assertEqual(addresses, ['44.1.2.2', '44.1.2.3', '44.1.2.4', '44.1.2.5'])
        self.assertEqual(run.call_args.kwargs['timeout'], 3)

    def test_invalid_shared_ips_fail_before_any_probe(self):
        for values in (['10.1.2.3'], ['::1'], ['44.1.2.3:443'], ['44.1.2.3', '44.1.2.3'], ['44.1.2.'+str(i) for i in range(1, 6)]):
            args = ['network-facts.py', 'probe', '--location', 'guest']
            for value in values:
                args += ['--registry-ip', value]
            with self.subTest(values=values), patch('sys.argv', args), patch.object(facts, 'probe') as probe, contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(facts.main(), 2)
                probe.assert_not_called()


if __name__ == '__main__':
    unittest.main()
