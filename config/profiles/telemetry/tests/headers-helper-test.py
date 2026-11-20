#!/usr/bin/env python3
"""The OTLP headers helper prints only the collector Authorization header."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import types
import unittest

PROFILE = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(PROFILE))
sys.path.insert(0, str(PROFILE.parents[2]/'internal/adapters/configmaterial'))
import guest
import native

FAILURES = {b'subyard-telemetry: collector token unavailable\n', b'subyard-telemetry: invalid collector token\n',
            b'subyard-telemetry: unexpected arguments\n'}


class HeadersHelperTest(unittest.TestCase):
    def setUp(self):
        self.root = Path(self.enterContext(tempfile.TemporaryDirectory(dir='/tmp')))
        self.auth = self.root/'private/auth.token'
        self.auth.parent.mkdir(mode=0o700)
        self.helper = self.root/'otlp-headers'
        self.helper.write_bytes(guest.headers_helper(self.auth))
        self.helper.chmod(0o755)
        self.shells = [shell for shell in ('/bin/sh', shutil.which('dash'), shutil.which('bash')) if shell]

    def run_helper(self, shell, *args):
        # No PATH: the helper relies on shell builtins and absolute paths only.
        return subprocess.run([shell, str(self.helper), *args], env={}, capture_output=True, timeout=10, cwd=self.root)

    def test_installed_helper_is_root_executable_at_the_policy_path(self):
        source = self.root/'source'
        source.mkdir()
        for path in PROFILE.glob('*.py'):
            shutil.copyfile(path, source/path.name)
        shutil.copyfile(guest.tomli_w.__file__, source/'tomli_w.py')
        account = types.SimpleNamespace(pw_uid=1000, pw_gid=1000, pw_dir='/home/dev')
        runtime = guest.Runtime(source, 'dev', False, ['claude', 'codex'], account=account)
        self.assertEqual(str(runtime.helper), native.HELPER)
        self.assertEqual(runtime.assets[runtime.helper], (guest.headers_helper(runtime.auth), 0o755))
        self.assertIn(b'</var/lib/subyard/telemetry-private/auth.token ', runtime.assets[runtime.helper][0])

    def test_valid_token_prints_one_authorization_header(self):
        token = '0123456789abcdef'*4
        for ending in ('\n', ''):
            self.auth.write_text(token+ending)
            for shell in self.shells:
                with self.subTest(shell=shell, newline=bool(ending)):
                    result = self.run_helper(shell)
                    self.assertEqual((result.returncode, result.stderr), (0, b''))
                    self.assertEqual(result.stdout, b'{"Authorization":"Bearer '+token.encode()+b'"}\n')
                    self.assertEqual(json.loads(result.stdout), {'Authorization': 'Bearer '+token})

    def test_failures_are_fixed_text_without_token_material(self):
        # A synthetic 64-digit value; failures must not echo any part of it.
        sample = 'f0e1d2c3b4a5'*5
        cases = [('missing', None, ()), ('empty', '', ()), ('short', sample[:63], ()), ('long', sample+'abcde', ()),
                 ('uppercase', sample.upper()[:64], ()), ('arguments', 'a'*64, ('ignored',)),
                 ('spaces', ' '+'a'*63, ()), ('directory', 'directory', ())]
        for name, content, args in cases:
            if self.auth.is_dir():
                self.auth.rmdir()
            elif self.auth.exists():
                self.auth.unlink()
            if content == 'directory':
                self.auth.mkdir()
            elif content is not None:
                self.auth.write_text(content+'\n')
            for shell in self.shells:
                with self.subTest(case=name, shell=shell):
                    result = self.run_helper(shell, *args)
                    self.assertEqual(result.returncode, 1)
                    self.assertEqual(result.stdout, b'')
                    self.assertIn(result.stderr, FAILURES)
                    self.assertNotIn(sample[:12].encode(), result.stderr)
                    self.assertNotIn(sample[:12].upper().encode(), result.stderr)

    def test_observation_accepts_exactly_the_tokens_the_helper_exports(self):
        token = '0123456789abcdef'*4
        owner = types.SimpleNamespace(auth=self.auth, uid=os.getuid())
        for content in (token, token+'\n', token+'\r\n', token+' \n', ' '+token+'\n', '\n'+token+'\n',
                        token+'\n\n', token+'\nextra\n', token.upper()+'\n', token[:-1]+'\n'):
            self.auth.write_text(content)
            self.auth.chmod(0o600)
            try:
                observed = guest.Runtime.token(owner) == token
            except ValueError:
                observed = False
            for shell in self.shells:
                with self.subTest(content=content.encode(), shell=shell):
                    # Observation may be stricter than the helper, never looser.
                    exported = self.run_helper(shell).returncode == 0
                    self.assertTrue(exported or not observed)
                    self.assertEqual(observed, content in (token, token+'\n'))

    def test_activation_identity_differs_from_the_bearer_releases(self):
        # v0.28-v0.30 put the bearer into home documents. Core refuses a rollback
        # whose retained profile runtime has a different activation identity.
        runtime = json.loads((PROFILE/'profile.json').read_text())['runtime']
        self.assertNotIn(runtime['activation_id'], {'telemetry-runtime'})
        self.assertEqual(runtime['activation_id'], 'telemetry-runtime-v2')

    def test_only_a_provisioned_codex_older_than_the_helper_is_reported(self):
        link = self.root/'codex'
        self.assertTrue(guest.codex_helper_supported(link))
        for target, supported in (('releases/0.169.9/bin/codex', False), ('releases/0.9.0/bin/codex', False),
                                  ('releases/0.170.0/bin/codex', True), ('releases/1.0.0/bin/codex', True),
                                  ('releases/0.171.2/bin/codex', True), ('/usr/bin/codex', True)):
            with self.subTest(target=target):
                if link.is_symlink():
                    link.unlink()
                os.symlink(target, link)
                self.assertEqual(guest.codex_helper_supported(link), supported)


if __name__ == '__main__':
    unittest.main()
