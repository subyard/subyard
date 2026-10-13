#!/usr/bin/env python3
"""Predecessor ownership, stale assessment and resumable control retirement."""
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

PACKAGE = Path(__file__).resolve().parents[1]
os.environ.setdefault('INCUS_PROJECT', 'fixture')
os.environ.setdefault('YARD_INSTANCE_NAME', 'yard')
spec = importlib.util.spec_from_file_location('retirement', PACKAGE/'retire.py')
retirement = importlib.util.module_from_spec(spec)
spec.loader.exec_module(retirement)

DEFERRED="user.subyard.deferred_publication.ai-observer"

class RetirementTest(unittest.TestCase):
    def setUp(self):
        self.snapshot = {'config': {'user.subyard.ai_observer_proxy': 'v1:22222'},
                         'devices': {'ai-observer': {'type':'proxy','bind':'host','listen':'tcp:127.0.0.1:22222','connect':'tcp:127.0.0.1:8080'}}}
        self.container = {'Name':'/subyard-ai-observer','Id':'owned-container','Image':'image',
                          'Config':{'Labels':{'org.subyard.managed':'ai-observer-v1'}},
                          'HostConfig':{},'Mounts':[],'State':{'Running':True}}
        self.service = 'owned control hashes\nactive\nenabled\n'
        self.effects = []
        self.patchers = [patch.object(retirement,'run',self.native_run),patch.object(retirement,'guest',self.guest),patch.object(retirement,'incus',self.incus)]
        for patcher in self.patchers: patcher.start(); self.addCleanup(patcher.stop)

    def native_run(self,*args,**options):
        self.assertEqual(args[:2],('incus','query'))
        return json.dumps(self.snapshot).encode()

    def guest(self,*args):
        if args[:3] == ('sh','-eu','-c') and args[3] == retirement.CONTROL_ASSESSMENT:
            return self.service.encode()
        if args[:5] == ('timeout','10','docker','container','ls'):
            return b'subyard-ai-observer\n' if self.container else b''
        if args[:5] == ('timeout','10','docker','container','inspect'):
            return json.dumps([self.container]).encode()
        self.effects.append(args)
        if args[:4] == ('timeout','20','docker','stop'): self.container['State']['Running']=False
        elif args[:4] == ('timeout','10','docker','rm'): self.container=None
        elif args[:3] == ('sh','-eu','-c') and 'rm -f --' in args[3]: self.service=''
        return b''

    def incus(self,*args):
        self.effects.append(args)
        if args[:3] == ('config','device','remove'): self.snapshot['devices'].pop('ai-observer')
        elif args[:2] == ('config','unset'): self.snapshot['config'].pop(args[3])
        return b''

    def apply(self,observed):
        with patch.object(sys,'argv',['retire.py','apply','op-retire',observed['actual'],observed['desired']]): retirement.main()

    def test_owned_retirement_and_repeat_preserve_data(self):
        observed,_=retirement.observe()
        self.apply(observed)
        self.assertEqual(retirement.observe()[0],{'state':'absent','actual':'','desired':''})
        self.assertFalse(any('/srv/' in str(effect) or 'volume' in effect for effect in self.effects))

    def defer(self):
        device=self.snapshot['devices'].pop('ai-observer')
        device['listen']='tcp:100.64.1.2:22222'
        self.snapshot['config'][retirement.PROXY]='v2:100.64.1.2:22222'
        self.snapshot['config'][DEFERRED]=json.dumps({'version':1,'ownership':self.snapshot['config'][retirement.PROXY],'device':device})
        return device

    def test_deferred_retirement_revokes_before_controls_and_removes_just_restored(self):
        device=self.defer()
        observed,_=retirement.observe()
        original=self.incus
        def restore(*args):
            result=original(*args)
            if args==('config','unset','yard',DEFERRED):
                self.snapshot['devices']['ai-observer']=device
            return result
        with patch.object(retirement,'incus',side_effect=restore): self.apply(observed)
        self.assertEqual(self.effects[0],('config','unset','yard',DEFERRED))
        self.assertEqual(retirement.observe()[0]['state'],'absent')

    def test_invalid_or_changed_deferred_receipt_refuses_retirement(self):
        self.defer()
        observed,_=retirement.observe()
        self.snapshot['config'][DEFERRED]+=' {}'
        with self.assertRaises(ValueError): self.apply(observed)
        self.assertEqual(self.effects,[])

    def test_foreign_restore_stops_before_controls(self):
        self.defer()
        observed,_=retirement.observe()
        def foreign(*args):
            self.incus(*args)
            self.snapshot['devices']['ai-observer']={'type':'disk'}
        with patch.object(retirement,'incus',side_effect=foreign):
            with self.assertRaises(ValueError): self.apply(observed)
        self.assertEqual(len(self.effects),1)
        self.assertTrue(self.service)

    def test_foreign_route_or_container_has_no_effect(self):
        for conflict in ('route','container','inherited'):
            with self.subTest(conflict=conflict):
                self.setUp()
                if conflict=='route': self.snapshot['devices']['ai-observer']['connect']='tcp:127.0.0.1:9999'
                elif conflict=='container': self.container['Config']['Labels']={}
                else: self.snapshot['expanded_devices']={}
                with self.assertRaises(ValueError): retirement.observe()
                self.assertEqual(self.effects,[])

    def test_stale_container_or_route_refuses_apply(self):
        observed,_=retirement.observe()
        self.container['Id']='different'
        with self.assertRaises(ValueError): self.apply(observed)
        self.assertEqual(self.effects,[])

    def test_interrupted_native_removal_resumes(self):
        observed,_=retirement.observe()
        def interrupted(*args): raise OSError('interrupted')
        with patch.object(retirement,'incus',interrupted):
            with self.assertRaises(OSError): self.apply(observed)
        self.assertIsNone(self.container)
        self.assertFalse(self.service)
        self.apply(retirement.observe()[0])
        self.assertEqual(retirement.observe()[0]['state'],'absent')


if __name__=='__main__': unittest.main()
