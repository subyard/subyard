#!/usr/bin/env python3
"""Run the guest preconditions with absent native CLIs and an unavailable Compose plugin."""
from pathlib import Path
import subprocess
import tempfile
import unittest
import sys
import json
import shutil

PROFILE = Path(__file__).resolve().parents[1]


class GuestTest(unittest.TestCase):
    def test_disable_removes_only_owned_services_and_retains_attached_network(self):
        with tempfile.TemporaryDirectory(prefix='subyard-openlit-disable-', dir='/tmp') as temporary:
            root = Path(temporary)
            source, state, commands = root/'source', root/'state', root/'bin'
            for path in (source, state, commands):
                path.mkdir(mode=0o700)
            for path in PROFILE.iterdir():
                if path.is_file():
                    shutil.copyfile(path, source/path.name)
            (source/'tomli_w.py').write_text('# fixture transport asset\n')
            (state/'marker').write_text('subyard-openlit-v1\n')
            (state/'marker').chmod(0o600)
            (state/'database.env').write_text('OPENLIT_DB_PASSWORD=synthetic\n')
            (state/'database.env').chmod(0o600)
            (root/'home').mkdir(mode=0o700)
            calls = root/'calls'
            driver = commands/'driver'
            driver.write_text('''#!/usr/bin/python3
import json,os,pathlib,subprocess,sys
name=pathlib.Path(sys.argv[0]).name
args=sys.argv[1:]
base=pathlib.Path(__file__).resolve().parents[1]
if name in ('docker','systemctl'):
    with (base/'calls').open('a') as out: out.write(json.dumps([name,*args])+'\\n')
    if name=='docker':
        if 'down' in args or 'pull' in args or ('up' in args and not (base/'enable').exists()): sys.exit(99)
        if args[-2:]==['config','--images']: print('fixture/clickhouse\\nfixture/openlit\\nfixture/collector')
elif name=='id': print(0)
elif name=='getent': print('dev:x:0:0::'+str(base/'home')+':/bin/bash')
elif name=='stat':
    code=args[1]; mode=oct(pathlib.Path(args[2]).stat().st_mode & 0o777)[2:]
    print('0:0:'+mode if code=='%u:%g:%a' else '0')
elif name=='install':
    filtered=[]; pos=0
    while pos<len(args):
        if args[pos] in ('-o','-g'): pos+=2
        else: filtered.append(args[pos]); pos+=1
    sys.exit(subprocess.run(['/usr/bin/install',*filtered]).returncode)
elif name=='python3':
    if args[1]=='locked-guest': os.execv('/bin/bash',['bash',args[2],'--runtime-lock-fd','9',*args[3:]])
    with (base/'calls').open('a') as out: out.write(json.dumps([name,pathlib.Path(args[0]).name,args[1]])+'\\n')
    if pathlib.Path(args[0]).name=='trace_cutover.py' and args[1]=='fence': sys.exit(int((base/'fence-exit').read_text()))
    if pathlib.Path(args[0]).name=='runtime.py': print('{"state":"current"}')
else: sys.exit(99)
''')
            driver.chmod(0o755)
            for name in ('docker', 'systemctl', 'id', 'getent', 'stat', 'install', 'python3'):
                (commands/name).symlink_to(driver)
            for name in ('dirname', 'cut', 'head', 'cat', 'cmp', 'mktemp', 'mv', 'chmod', 'timeout'):
                (commands/name).symlink_to(Path('/usr/bin')/name)
            code = (source/'guest.sh').read_text()
            for original, replacement in (
                    ('state=/var/lib/subyard/openlit', 'state='+str(state)),
                    ('unit=/etc/systemd/system/subyard-openlit.service', 'unit='+str(root/'unit')),
                    ('cutover_unit=/etc/systemd/system/subyard-openlit-trace-cutover.service', 'cutover_unit='+str(root/'cutover-unit')),
                    ('cutover_timer=/etc/systemd/system/subyard-openlit-trace-cutover.timer', 'cutover_timer='+str(root/'cutover-timer'))):
                code = code.replace(original, replacement)
            (source/'guest.sh').write_text(code)
            result = subprocess.run(['/bin/bash', str(source/'guest.sh'), 'apply', '0', 'dev', 'codex'],
                                    env={'PATH': str(commands)}, capture_output=True, text=True, timeout=20)
            self.assertEqual(result.returncode, 0, result.stderr)
            rows = [json.loads(line) for line in calls.read_text().splitlines()]
            docker = [row for row in rows if row[0] == 'docker']
            self.assertEqual(len(docker), 3)  # version, scoped stop, scoped rm
            self.assertEqual(docker[1][-6:], ['stop', '--timeout', '20', 'clickhouse', 'openlit', 'collector'])
            self.assertEqual(docker[2][-5:], ['rm', '--force', 'clickhouse', 'openlit', 'collector'])
            self.assertNotIn('network', [arg for row in rows for arg in row])
            self.assertIn(['systemctl', 'disable', '--now', 'subyard-openlit-trace-cutover.timer'], rows)
            self.assertTrue((state/'database.env').exists())
            # Actual guest dispatch must preserve a deferred fence success and
            # propagate protected-state refusal before timer/publication writes.
            (root/'enable').write_text('fixture\n')
            for status in (0, 1):
                calls.write_text('')
                (root/'fence-exit').write_text(str(status))
                result = subprocess.run(['/bin/bash', str(source/'guest.sh'), 'apply', '1', 'dev', 'codex'],
                                        env={'PATH': str(commands)}, capture_output=True, text=True, timeout=20)
                self.assertEqual(result.returncode, status, result.stderr)
                rows = [json.loads(line) for line in calls.read_text().splitlines()]
                self.assertIn(['python3', 'trace_cutover.py', 'fence'], rows)
                self.assertEqual(['python3', 'runtime.py', 'publish'] in rows, status == 0)
                self.assertEqual(['systemctl', 'enable', '--now', 'subyard-openlit-trace-cutover.timer'] in rows, status == 0)

    def test_missing_selected_agents_reach_real_runtime_prerequisite_without_mutation(self):
        with tempfile.TemporaryDirectory(prefix='subyard-openlit-guest-', dir='/tmp') as temporary:
            root = Path(temporary)
            commands = root/'bin'
            commands.mkdir()
            for name in ('dirname', 'cut', 'chmod', 'flock'):
                (commands/name).symlink_to(Path('/usr/bin')/name)
            (commands/'python3').symlink_to(sys.executable)
            scripts = {
                'id': '#!/bin/sh\nprintf "0\\n"\n',
                'getent': '#!/bin/sh\nprintf "dev:x:0:0::'+str(root/'home')+':/bin/bash\\n"\n',
                'docker': '#!/bin/sh\nexit 1\n',
            }
            for name, script in scripts.items():
                path = commands/name
                path.write_text(script)
                path.chmod(0o755)
            code = (PROFILE/'guest.sh').read_text().replace('state=/var/lib/subyard/openlit', 'state='+str(root/'state'))
            code = code.replace('unit=/etc/systemd/system/subyard-openlit.service', 'unit='+str(root/'unit'))
            code = code.replace('cutover_unit=/etc/systemd/system/subyard-openlit-trace-cutover.service', 'cutover_unit='+str(root/'cutover-unit'))
            code = code.replace('cutover_timer=/etc/systemd/system/subyard-openlit-trace-cutover.timer', 'cutover_timer='+str(root/'cutover-timer'))
            code = code.replace('/run/lock/subyard-openlit.lock', str(root/'lock'))
            # This precondition test doubles only the root-only lock handoff;
            # the migration suite owns actual file/FD/flock boundaries.
            (root/'trace_cutover.py').write_text('import os,sys\nif sys.argv[1] == "locked-guest": os.execv("/bin/bash", ["bash", sys.argv[2], "--runtime-lock-fd", "9", *sys.argv[3:]])\n')
            guest = root/'guest.sh'
            guest.write_text(code)
            result = subprocess.run(['/bin/bash', str(guest), 'apply', '1', 'dev', 'claude codex'],
                                    env={'PATH': str(commands)}, capture_output=True, text=True, timeout=10)
            self.assertEqual(result.returncode, 1)
            self.assertIn('Docker Compose is missing; run yard init', result.stderr)
            self.assertFalse((root/'state').exists())
            self.assertFalse((root/'unit').exists())
            self.assertFalse((root/'home').exists())


if __name__ == '__main__':
    unittest.main()
