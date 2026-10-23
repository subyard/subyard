#!/usr/bin/env python3
"""Exercise native observation against the released-updater's strict shims."""
import json
from pathlib import Path
import runpy
import shutil
import subprocess
import sys
import tempfile
import unittest


PROFILE = Path(__file__).resolve().parents[1]
ROOT = PROFILE.parents[2]


class ReleaseObservationTest(unittest.TestCase):
    def test_actual_absence_transport_and_manager_fail_closed(self):
        harness = runpy.run_path(str(ROOT / 'dev/verify-release-upgrades.py'))
        provider = runpy.run_path(str(PROFILE / 'tests/release-observation.py'))
        with tempfile.TemporaryDirectory(prefix='subyard-github-release-', dir='/tmp') as temporary:
            root = Path(temporary)
            runtime = root / 'runtime'
            package = runtime / 'config/profiles/github'
            (package / 'bin').mkdir(parents=True)
            # The broker is fingerprinted but never executed by absence inspection.
            for name in ('profile.json', 'owner.sh', 'runtime.sh', 'runtime.py', 'provision.sh',
                         'SKILL.md', 'skills.json', 'bin/subyard-github', 'bin/git-remote-subyard-github', 'git_wiring.py'):
                shutil.copy2(PROFILE / name, package / name)
            (package / 'bin/broker').write_text('unexecuted synthetic broker input\n')
            (runtime / 'config/skills').mkdir()
            shutil.copy2(ROOT / 'config/skills/deliver.py', runtime / 'config/skills/deliver.py')
            (runtime / 'scripts/lib').mkdir(parents=True)
            for name in ('engine-context.sh', 'ui.sh', 'host.sh', 'download.sh'):
                shutil.copy2(ROOT / 'scripts/lib' / name, runtime / 'scripts/lib' / name)
            shutil.copy2(ROOT / 'scripts/lib-power.sh', runtime / 'scripts/lib-power.sh')
            shims = root / 'commands'
            shims.mkdir()
            control, rejected = root / 'control.json', root / 'rejected'
            control.write_text(json.dumps({'instances': [{'info': {'status': 'Running'}}]}))
            harness['write_observation_incus'](shims, control, rejected)
            environment = {
                'PATH': str(shims) + ':/usr/bin:/bin', 'HOME': str(root),
                'SUBYARD_ENGINE_CONTEXT': '1', 'SUBYARD_ENGINE_CONTEXT_SCHEMA': '1',
                'SUBYARD_OPERATOR_HOME': str(root), 'SUBYARD_CONFIG_DIR': str(runtime / 'config'),
                'SUBYARD_CONFIG_HOME': str(root / 'config'), 'SUBYARD_HOME': str(root / 'data'),
                'STORAGE_PATH': str(root), 'HOST_BASE': str(root), 'RESTRICTED_DISK_PATHS': str(root),
                'ACCESS_KIND': 'local', 'YARD_KIND': 'container', 'YARD_INSTANCE_NAME': 'yard',
                'INCUS_PROJECT': 'subyard', 'INCUS_BRIDGE': 'fixture', 'SSH_HOST': '127.0.0.1',
                'DEV_USER': 'dev', 'DEV_UID': '1000', 'DEV_SUDO': '0',
                'FORWARD_SSH_AGENT': '0', 'NESTED_E2E_VMS': '0', 'ENVIRONMENT_PROFILES': '',
            }

            def observe():
                result = subprocess.run(['bash', str(package / 'runtime.sh'), 'observe'],
                                        env=environment, capture_output=True, text=True, timeout=30)
                self.assertEqual(result.returncode, 0, result.stderr)
                return json.loads(result.stdout)

            observed = observe()
            self.assertEqual(observed['state'], 'current')
            self.assertTrue(observed['ownership_absent'])
            self.assertEqual(observed['actual'], observed['desired'])
            self.assertFalse(rejected.exists())
            # The fixture-only launcher observes original returns; source bytes,
            # script argv, output/status and all returned digests stay unchanged.
            tracer = PROFILE/'tests/e2e/runtime-observer-trace.py'
            trace_source = runpy.run_path(str(tracer))
            trace_commands = root/'trace-commands'
            trace_commands.mkdir()
            core_argv = ['exec', 'yard', '--project', 'subyard', '--', 'sh', '-eu', '-c', trace_source['CORE_FILES']]
            (trace_commands/'incus').write_text(
                '#!'+sys.executable+'\nimport os,sys\n'
                'if sys.argv[1:] == '+repr(core_argv)+':\n'
                ' print("synthetic-public-core-files"); sys.exit(int(os.environ.get("SUBYARD_TRACE_CORE_PROBE_STATUS","0")))\n'
                'else: os.execv('+repr(str(shims/'incus'))+',sys.argv)\n')
            (trace_commands/'incus').chmod(0o755)
            original_source = (package/'runtime.py').read_bytes()
            for selected in ('', 'github'):
                trace_log = root/('selected-trace.log' if selected else 'absent-trace.log')
                plain_env = dict(environment, ENVIRONMENT_PROFILES=selected)
                plain = subprocess.run(['bash', str(package/'runtime.sh'), 'observe'], env=plain_env,
                                       capture_output=True, text=True, timeout=30)
                trace_env = dict(plain_env, PATH=str(trace_commands)+':'+environment['PATH'],
                                 SUBYARD_TRACE_REAL_PYTHON=sys.executable,
                                 SUBYARD_TRACE_GITHUB_SOURCE=str(package/'runtime.py'),
                                 SUBYARD_TRACE_GITHUB_LOG=str(trace_log), GH_TOKEN='synthetic-private-marker-do-not-log')
                traced = subprocess.run([sys.executable, str(tracer), str(package/'runtime.py'), 'observe'],
                                        env=trace_env, capture_output=True, text=True, timeout=30)
                self.assertEqual((traced.returncode, traced.stdout, traced.stderr),
                                 (plain.returncode, plain.stdout, plain.stderr))
                alias = root/('selected-alias' if selected else 'absent-alias')
                alias.symlink_to(package, target_is_directory=True)
                traced_alias = subprocess.run([sys.executable, str(tracer), str(alias/'runtime.py'), 'observe'],
                                              env=trace_env, capture_output=True, text=True, timeout=30)
                self.assertEqual((traced_alias.returncode, traced_alias.stdout, traced_alias.stderr),
                                 (plain.returncode, plain.stdout, plain.stderr))
                self.assertEqual((package/'runtime.py').read_bytes(), original_source)
                self.assertNotIn('synthetic-private-marker-do-not-log', trace_log.read_text())
                records = [json.loads(line) for line in trace_log.read_text().splitlines()]
                self.assertFalse(any(item['kind'] == 'capture-error' for item in records))
                result = next(item['public'] for item in records if item['kind'] == 'result')
                self.assertEqual(result, json.loads(plain.stdout))
                desired = next(item for item in records if item['kind'] == 'desired')
                self.assertEqual(desired['digest'], result['desired'])
                if selected:
                    actual = next(item for item in records if item['kind'] == 'actual-facts')
                    self.assertEqual(actual['digest'], result['actual'])
                self.assertEqual(trace_log.stat().st_mode & 0o777, 0o600)
                self.assertFalse(rejected.exists())
                verified = subprocess.run([sys.executable, str(tracer), '--verify', str(trace_log), 'default'],
                                          capture_output=True, text=True, timeout=10)
                self.assertEqual(verified.returncode, 0, verified.stderr)
            invalid_env = dict(trace_env, SUBYARD_YARD='invalid/yard')
            plain = subprocess.run([sys.executable, str(package/'runtime.py'), 'observe'],
                                   env=invalid_env, capture_output=True, text=True, timeout=10)
            traced = subprocess.run([sys.executable, str(tracer), str(package/'runtime.py'), 'observe'],
                                    env=invalid_env, capture_output=True, text=True, timeout=10)
            self.assertEqual(plain.returncode, 1)
            self.assertEqual((traced.returncode, traced.stdout, traced.stderr),
                             (plain.returncode, plain.stdout, plain.stderr))
            unavailable_core = dict(trace_env, SUBYARD_TRACE_CORE_PROBE_STATUS='79')
            plain = subprocess.run([sys.executable, str(package/'runtime.py'), 'observe'],
                                   env=unavailable_core, capture_output=True, text=True, timeout=10)
            traced = subprocess.run([sys.executable, str(tracer), str(package/'runtime.py'), 'observe'],
                                    env=unavailable_core, capture_output=True, text=True, timeout=10)
            self.assertEqual((traced.returncode, traced.stdout, traced.stderr),
                             (plain.returncode, plain.stdout, plain.stderr))
            last = json.loads(Path(trace_env['SUBYARD_TRACE_GITHUB_LOG']).read_text().splitlines()[-1])
            self.assertEqual(last['kind'], 'result')
            self.assertFalse(last['core_guest_files']['available'])
            for entry in provider['read_only_commands'](PROFILE):
                arguments, output, *status = entry
                status = status[0] if status else 0
                result = subprocess.run([str(shims / 'incus'), *arguments], input=b'\xff' * 131072,
                                        capture_output=True, timeout=10)
                self.assertEqual(result.returncode, status)
                self.assertEqual(result.stdout.decode(), output)
                for changed in (arguments + ['unexpected'], ['exec', 'foreign', *arguments[2:]]):
                    result = subprocess.run([str(shims / 'incus'), *changed],
                                            capture_output=True, timeout=10)
                    self.assertEqual(result.returncode, 99)
                    self.assertTrue(rejected.exists())
                    rejected.unlink()
            archive = provider['read_only_commands'](PROFILE)[1][0]
            for changed in (archive[:-1] + ['--yes'],
                            archive[:12] + [archive[12] + '\n'] + archive[13:]):
                result = subprocess.run([str(shims / 'incus'), *changed], capture_output=True, timeout=10)
                self.assertEqual(result.returncode, 99)
                rejected.unlink()
            program, arguments, output, status = provider['read_only_host_commands'](PROFILE)[0]
            self.assertEqual(program, 'systemctl')
            result = subprocess.run([str(shims / program), *arguments], capture_output=True, timeout=10)
            self.assertEqual((result.stdout.decode(), result.returncode), (output, status))
            for changed in (
                ['--user', 'start', arguments[2]], ['--user', 'stop', arguments[2]],
                ['--user', 'restart', arguments[2]], arguments + ['unexpected'],
                arguments[:2] + ['foreign.service'] + arguments[3:],
                arguments[:-1] + ['--property=ActiveState'],
            ):
                result = subprocess.run([str(shims / 'systemctl'), *changed], capture_output=True, timeout=10)
                self.assertEqual(result.returncode, 99)
                self.assertTrue(rejected.exists())
                rejected.unlink()
            for command in ('incus', 'systemctl'):
                path = shims / command
                original = path.read_text()
                path.write_text('#!/bin/sh\ncat >/dev/null\nexit 1\n')
                observed = observe()
                self.assertFalse(observed.get('ownership_absent', False))
                path.write_text(original)


if __name__ == '__main__':
    unittest.main()
