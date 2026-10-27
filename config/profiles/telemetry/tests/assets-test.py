#!/usr/bin/env python3
"""Prepare real load-driver bytes and consume only checksum-bound product inputs."""
import hashlib
import http.server
import io
import json
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile
import threading
import unittest

ROOT = Path(__file__).resolve().parents[4]
E2E = Path(__file__).parent/'e2e'
PRODUCT = 'config/profiles/telemetry/bin/subyard-telemetry'


class AssetsTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix='subyard-telemetry-assets-', dir='/tmp')
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.release = self.root/'.subyard-acceptance/release'
        self.release.mkdir(parents=True)
        helper = self.root/'tests/helpers/release-candidate.sh'
        helper.parent.mkdir(parents=True)
        shutil.copyfile(ROOT/'tests/helpers/release-candidate.sh', helper)

    def package(self, *, missing=False, unbound=False, corrupt=False):
        payload = b'#!/bin/sh\nexit 0\n'
        files = {'bin/yard': payload, 'bin/yard-engine': payload}
        if not missing:
            files[PRODUCT] = payload
        manifest = ''.join(f'{hashlib.sha256(body).hexdigest()}  ./{name}\n'
                           for name, body in files.items() if not (unbound and name == PRODUCT))
        files['runtime-files.sha256'] = manifest.encode()
        if corrupt:
            files[PRODUCT] += b'corrupt\n'
        archive = self.release/'fixture.tar.gz'
        with tarfile.open(archive, 'w:gz') as bundle:
            for name, body in files.items():
                member = tarfile.TarInfo(name)
                member.size, member.mode = len(body), 0o755
                bundle.addfile(member, io.BytesIO(body))
        (self.release.parent/'candidate.json').write_text(json.dumps(dict(
            runtime_file=archive.name, runtime_sha256=hashlib.sha256(archive.read_bytes()).hexdigest())))
        return archive

    def prepare(self):
        return subprocess.run(['bash', '-euc', '. "$1"; telemetry_prepare_assets "$2" "$2/assets"',
            'fixture', str(E2E/'assets.sh'), str(self.root)], capture_output=True, text=True, timeout=120)

    def test_missing_unbound_or_tampered_runtime_refuses_before_build(self):
        self.assertNotEqual(self.prepare().returncode, 0)
        for scenario in ('missing', 'unbound', 'corrupt', 'checksum'):
            with self.subTest(scenario=scenario):
                archive = self.package(**({scenario: True} if scenario != 'checksum' else {}))
                if scenario == 'checksum':
                    with archive.open('ab') as stream:
                        stream.write(b'tampered archive')
                self.assertNotEqual(self.prepare().returncode, 0)
                self.assertFalse((self.root/'assets').exists())

    def test_real_driver_and_stack_consumer_share_prepared_assets(self):
        self.package()
        for name in ('go.mod', 'go.sum', 'dev/telemetry-compare/main.go'):
            destination = self.root/name
            destination.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(ROOT/name, destination)
        result = self.prepare()
        self.assertEqual(result.returncode, 0, result.stderr)
        assets = self.root/'assets'
        self.assertEqual(assets.stat().st_mode & 0o777, 0o700)
        self.assertEqual((assets/'subyard-telemetry').read_bytes(), b'#!/bin/sh\nexit 0\n')
        command = ['python3', '-B', str(E2E/'measure.py'), '--stack', '--preflight', '--assets', str(assets)]
        result = subprocess.run(command, capture_output=True, text=True, timeout=5)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(set(json.loads(result.stdout)['assets']), {'standalone', 'driver'})
        for name in ('subyard-telemetry', 'otlp-load'):
            path = assets/name
            path.rename(assets/(name+'.hidden'))
            self.assertNotEqual(subprocess.run(command, capture_output=True, timeout=5).returncode, 0)
            (assets/(name+'.hidden')).rename(path)
        token = self.root/'token'
        token.write_text('synthetic-fixture-token')
        received = []
        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *_): pass
            def do_POST(self):
                received.append((self.path, self.headers.get('Authorization')))
                self.rfile.read(int(self.headers['Content-Length']))
                self.send_response(200)
                self.end_headers()
        with http.server.HTTPServer(('127.0.0.1', 0), Handler) as server:
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            try:
                result = subprocess.run([str(assets/'otlp-load'), '--duration', '150ms', '--rate', '30',
                    '--seed', '1', '--token-file', str(token), '--endpoint', f'http://127.0.0.1:{server.server_port}'],
                    capture_output=True, text=True, timeout=5)
            finally:
                server.shutdown()
                thread.join()
        self.assertEqual(result.returncode, 0, result.stderr)
        offered = json.loads(result.stdout)
        self.assertGreater(offered['accepted_requests'], 0)
        self.assertEqual(offered['rejected_requests'], 0)
        self.assertTrue(received)
        self.assertTrue(all(path in ('/v1/logs', '/v1/metrics', '/v1/traces') and auth == 'Bearer synthetic-fixture-token'
                            for path, auth in received))


if __name__ == '__main__':
    unittest.main()
