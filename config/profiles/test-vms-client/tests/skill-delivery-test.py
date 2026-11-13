#!/usr/bin/env python3
"""The client profile registers its skill and client, links Hermes, and retires on deselection."""
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

sys.dont_write_bytecode = True
PROFILE = Path(__file__).resolve().parents[1]
ROOT = PROFILE.parents[2]
SKILL = PROFILE / 'skills/subyard-test-vms'
LINKS = ('.agents/skills', '.claude/skills', '.config/opencode/skills', '.pi/agent/skills', '.hermes/skills')
spec = importlib.util.spec_from_file_location('deliver', ROOT / 'config/skills/deliver.py')
deliver = importlib.util.module_from_spec(spec)
spec.loader.exec_module(deliver)


class ClientProfile(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(dir='/tmp')
        self.root = Path(self.temporary.name)
        self.home = self.root / 'home' / 'agent'
        self.home.mkdir(parents=True)
        self.canonical = self.root / 'usr/local/share/subyard/skills/subyard-test-vms'

    def tearDown(self):
        self.temporary.cleanup()

    def provision(self, *arguments):
        environment = dict(os.environ, TEST_VMS_CLIENT_TEST_ROOT=str(self.root), DEV_USER='agent')
        return subprocess.run(['bash', str(PROFILE / 'provision.sh'), *arguments], env=environment,
                              capture_output=True, text=True).returncode

    def refresh(self, mode):
        # The same provider operation scripts/refresh-agent-skills.sh performs.
        args = type('Args', (), dict(root=self.root, home=self.home, uid=os.getuid(), gid=os.getgid(),
                                     provider='profile-test-vms-client', mode=mode, source=PROFILE / 'skills',
                                     registered=True, extra=False))()
        return deliver.delivery(args)

    def assert_delivered(self):
        for name in ('SKILL.md', 'client.sh', 'service-route.py'):
            path = self.canonical / name
            self.assertEqual(path.read_bytes(), (SKILL / name).read_bytes())
            self.assertEqual(path.stat().st_mode & 0o777, 0o644)
        self.assertEqual(self.canonical.stat().st_mode & 0o777, 0o755)
        for adapter in LINKS:
            link = self.home / adapter / 'subyard-test-vms'
            self.assertTrue(link.is_symlink(), adapter)
            self.assertEqual(os.readlink(link), str(self.canonical))

    def assert_retired(self):
        self.assertFalse(self.canonical.exists())
        for adapter in LINKS + ('.codex/skills',):
            self.assertFalse(os.path.lexists(self.home / adapter / 'subyard-test-vms'), adapter)
        self.assertFalse((self.root / 'var/lib/subyard/agent-skills/profile-test-vms-client.json').exists())

    def test_enable_refresh_disable_and_enable_again(self):
        self.assertEqual(self.provision('--check'), 10)
        self.assertEqual(self.provision(), 0)
        self.assert_delivered()
        self.assertEqual(self.provision('--check'), 0)
        receipt = json.loads((self.root / 'var/lib/subyard/agent-skills/profile-test-vms-client.json').read_text())
        self.assertTrue(receipt['extra'])
        # Static refresh keeps the Hermes and pi links recorded at registration.
        self.assertEqual(self.refresh('check'), 0)
        (self.home / '.hermes/skills/subyard-test-vms').unlink()
        self.assertEqual(self.refresh('check'), 10)
        self.assertEqual(self.refresh('apply'), 0)
        self.assert_delivered()
        self.assertEqual(self.provision(), 0)
        self.assertEqual(self.refresh('check'), 0)
        # Deselection: static refresh removes the client, skill and every link.
        self.assertEqual(self.refresh('check-removed'), 10)
        self.assertEqual(self.refresh('remove'), 0)
        self.assert_retired()
        self.assertEqual(self.refresh('check-removed'), 0)
        self.assertEqual(self.provision('--check'), 10)
        self.assertEqual(self.provision(), 0)
        self.assert_delivered()

    def test_unrelated_user_skill_survives_retirement(self):
        user = self.home / '.hermes/skills/own-skill'
        user.mkdir(parents=True)
        (user / 'SKILL.md').write_text('---\nname: own-skill\ndescription: mine\n---\n')
        self.assertEqual(self.provision(), 0)
        self.assertEqual(self.refresh('remove'), 0)
        self.assertTrue((user / 'SKILL.md').is_file())

    def test_static_transport_carries_the_client(self):
        files = json.loads(subprocess.check_output([sys.executable, '-B', str(ROOT / 'config/skills/package.py'), 'files', str(ROOT)]))
        for name in ('SKILL.md', 'client.sh', 'service-route.py'):
            self.assertIn(f'config/profiles/test-vms-client/skills/subyard-test-vms/{name}', files)
        self.assertFalse([path for path in files if '__pycache__' in path or path.endswith('.pyc')])

    def test_skill_states_the_short_contract(self):
        text = (SKILL / 'SKILL.md').read_text()
        self.assertTrue(text.startswith('---\nname: subyard-test-vms\ndescription: '))
        self.assertLessEqual(len(text.splitlines()), 45)
        for term in ('bash /usr/local/share/subyard/skills/subyard-test-vms/client.sh status',
                     'acquire --slot N', 'exec HANDLE --', 'release HANDLE', '`available`', '`held` or `busy`',
                     '`provisioning`, `draining`, `recovering`', '`capacity`', '--wait 20m',
                     '`quarantined`, `stopped`, `legacy*`, `unavailable`', 'every 5 minutes', '--idle',
                     '20 minutes', 'Nothing in the VM survives', 'cannot reach this', 'root'):
            self.assertIn(term, text)
        # Every documented command exists in the shipped client.
        usage = subprocess.run(['bash', str(SKILL / 'client.sh'), '--help'], capture_output=True, text=True, check=True).stdout
        for command in ('status', 'acquire --slot N', 'exec HANDLE', 'release HANDLE', 'leases', 'logs'):
            self.assertIn(command, usage)


if __name__ == '__main__':
    unittest.main()
