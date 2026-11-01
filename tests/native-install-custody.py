#!/usr/bin/env python3
"""Owning host-installer fence checks; no root access or external commands."""
import fcntl
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from unittest import mock

source = Path(__file__).resolve().parents[1]/'scripts/lib/native-install-custody.py'
spec = importlib.util.spec_from_file_location('native_install_custody', source)
custody = importlib.util.module_from_spec(spec)
spec.loader.exec_module(custody)
stage_spec = importlib.util.spec_from_file_location('native_installed_stage', source.with_name('native-installed-stage.py'))
stage = importlib.util.module_from_spec(stage_spec)
stage_spec.loader.exec_module(stage)


class NativeInstallCustody(unittest.TestCase):
    def setUp(self):
        self.root = tempfile.TemporaryDirectory(prefix='subyard-native-install-test-', dir='/tmp')
        self.addCleanup(self.root.cleanup)
        self.path = Path(self.root.name)
        self.path.chmod(0o700)
        self.record = self.path/'generation.json'

    def test_delayed_first_claim_and_old_generation_stay_fenced(self):
        a, b, c = 'a'*64, 'b'*64, 'c'*64
        custody.claim(self.record, 'host-power', ('0', a), b)
        for old in ('0', a):
            with self.assertRaises(ValueError): custody.claim(self.record, 'host-power', (old,), a)
        custody.claim(self.record, 'host-power', ('0', a), b)
        custody.claim(self.record, 'host-power', (b,), c)
        for old in ('0', a, b):
            with self.assertRaises(ValueError): custody.claim(self.record, 'host-power', (old,), a)
        self.assertEqual(custody.observe(self.record, 'host-power'), c)
        self.assertEqual(self.record.stat().st_mode & 0o777, 0o644)

    def test_busy_and_replaced_actual_lock_refuse(self):
        lock = self.path/'lock'
        first = os.open(lock, os.O_CREAT|os.O_EXCL|os.O_RDWR, 0o600)
        self.addCleanup(os.close, first)
        os.fchmod(first, 0o600)
        second = os.open(lock, os.O_RDWR)
        self.addCleanup(os.close, second)
        custody.validate_lock(lock, first)
        with self.assertRaises(BlockingIOError): custody.validate_lock(lock, second)
        lock.unlink()
        lock.touch(mode=0o600)
        with self.assertRaises(ValueError): custody.validate_lock(lock, first)

    def test_unrelated_or_unsafe_record_refuses(self):
        custody.claim(self.record, 'host-power', ('0',), 'a'*64)
        with self.assertRaises(ValueError): custody.claim(self.record, 'host-power', ('b'*64,), 'c'*64)
        with self.assertRaises(ValueError): custody.observe(self.record, 'test-vms-host-sink')
        self.record.chmod(0o666)
        with self.assertRaises(ValueError): custody.observe(self.record, 'host-power')

    def test_broker_stage_finish_preserves_exact_owner_uid_path_and_live_parent(self):
        parent = Path('/tmp')/('subyard-native-installed-'+str(os.getuid()))
        parent.mkdir(mode=0o700, exist_ok=True)
        binding, recipe, owner = hashlib.sha256(os.fsencode(self.path)).hexdigest(), 'b'*64, 'test-vm-broker'
        directory = parent/(binding+'-'+owner)
        directory.mkdir(mode=0o700)
        def cleanup():
            if directory.exists():
                directory.chmod(0o700)
                shutil.rmtree(directory)
        self.addCleanup(cleanup)
        info = directory.stat()
        marker = dict(version=1, binding=binding, recipe=recipe, owner=owner,
                      device=info.st_dev, inode=info.st_ino, parent=os.getpid())
        def write_marker():
            (directory/'.stage.json').write_text(json.dumps(marker, separators=(',', ':'))+'\n')
            (directory/'.stage.json').chmod(0o600)
        write_marker()
        directory.chmod(0o500)
        for path, selected in ((str(directory), 'foreign'), (str(directory)+'-foreign', owner)):
            with self.assertRaises(ValueError): stage.finish(path, binding, recipe, selected)
        with mock.patch.object(stage.os, 'getuid', return_value=os.getuid()+1):
            with self.assertRaises(ValueError): stage.finish(str(directory), binding, recipe, owner)
        stage.finish(str(directory), binding, recipe, owner)
        self.assertTrue(directory.exists())
        child = subprocess.Popen(['true'])
        child.wait()
        marker['parent'] = child.pid
        write_marker()
        stage.finish(str(directory), binding, recipe, owner)
        self.assertFalse(directory.exists())

    def test_root_broker_stage_finish_dispatches_to_operator(self):
        script = '''set -eu
SCRIPT_DIR=$1
id() { printf '0\\n'; }
python3() { return 91; }
runuser() { printf '%s\\n' "$@"; }
. "$SCRIPT_DIR/lib/native-installed-stage.sh"
subyard_prepared_installed_finish 0
'''
        values = dict(SUBYARD_PREPARED_INSTALLED_CLEANUP='1', SUBYARD_PREPARED_INSTALLED_OWNER='test-vm-broker',
                      SUBYARD_USER='operator', SUBYARD_PREPARED_INSTALLED_STAGE='/fixture-stage',
                      SUBYARD_PREPARED_INSTALLED_BINDING='a'*64, SUBYARD_PREPARED_INSTALLED_RECIPE='b'*64)
        result = subprocess.run(['bash', '-c', script, 'finish', str(source.parents[1])],
                                env=os.environ | values, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        self.assertEqual(result.returncode, 0)
        self.assertEqual(result.stdout.decode().splitlines(), ['-u', 'operator', '--', 'python3', '-B',
            str(source.with_name('native-installed-stage.py')), 'finish', '/fixture-stage', 'a'*64, 'b'*64, 'test-vm-broker'])


if __name__ == '__main__': unittest.main()
