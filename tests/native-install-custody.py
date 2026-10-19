#!/usr/bin/env python3
"""Owning host-installer fence checks; no root access or external commands."""
import fcntl
import importlib.util
import os
from pathlib import Path
import tempfile
import unittest

source = Path(__file__).resolve().parents[1]/'scripts/lib/native-install-custody.py'
spec = importlib.util.spec_from_file_location('native_install_custody', source)
custody = importlib.util.module_from_spec(spec)
spec.loader.exec_module(custody)


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


if __name__ == '__main__': unittest.main()
