#!/usr/bin/env python3
"""Android owns the installed skill's toolchain and broker instruction contract."""
import importlib.util
import os
from pathlib import Path
import tempfile
import unittest

PROFILE = Path(__file__).resolve().parents[1]
ROOT = PROFILE.parents[2]
spec = importlib.util.spec_from_file_location('deliver', ROOT/'config/skills/deliver.py')
deliver = importlib.util.module_from_spec(spec)
spec.loader.exec_module(deliver)


class AndroidSkill(unittest.TestCase):
    def test_delivery_and_content(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as temporary:
            root = Path(temporary)
            home = root/'home'
            home.mkdir()
            args = type('Args', (), dict(root=root, home=home, uid=os.getuid(), gid=os.getgid(), provider='profile-android', mode='apply', source=PROFILE/'skills', registered=False, extra=False))()
            deliver.delivery(args)
            skill = home/'.agents/skills/subyard-android/SKILL.md'
            content = skill.read_text()
            self.assertTrue(content.startswith('---\nname: subyard-android\ndescription: Build or test Android apps'))
            for term in ('/srv/cache/android-sdk','/opt/jdk-17','/srv/cache/gradle','/home/dev/.gradle','/etc/profile.d/subyard-android.sh','android-broker run --device phone --api 35 -- ./run-device-tests.sh','ANDROID_SERIAL','ADB_SERVER_SOCKET','timeout 30 android-broker status','readiness is unknown'):
                self.assertIn(term,content)
            self.assertEqual(skill.resolve(),(home/'.claude/skills/subyard-android/SKILL.md').resolve())
            args.mode='remove'
            deliver.delivery(args)
            self.assertFalse(skill.exists())


if __name__ == '__main__':
    unittest.main()
