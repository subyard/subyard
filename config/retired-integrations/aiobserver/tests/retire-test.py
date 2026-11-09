#!/usr/bin/env python3
"""Predecessor ownership, stale assessment and resumable control retirement."""
import copy
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
        self.patchers = [patch.object(retirement,'run',self.native_run),patch.object(retirement,'guest',self.guest),patch.object(retirement,'incus',self.incus),
                         patch.object(retirement,'wait_guest_ready',lambda snapshot: None)]
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

    def test_equivalent_docker_mount_enumeration_preserves_observation(self):
        self.snapshot['config'][retirement.PROVISION]='a'*64
        self.container['Mounts'] = [
            {'Type':'bind','Source':'/srv/agents','Destination':'/data',
             'Mode':'ro','RW':False,'Propagation':'rprivate'},
            {'Type':'volume','Name':'observer-state','Source':'/var/lib/docker/volumes/observer-state/_data',
             'Destination':'/state','Driver':'local','Mode':'z','RW':True,'Propagation':''},
        ]
        first,_ = retirement.observe()
        self.container['Mounts'].reverse()
        repeated,_ = retirement.observe()
        self.assertEqual(first, repeated,
                         'equivalent Docker mount enumeration changed exact retirement observation')
        self.assertEqual(self.effects, [])
        self.apply(first)
        self.assertEqual(retirement.observe()[0],{'state':'absent','actual':'','desired':''})
        self.assertIsNone(self.container)
        self.assertEqual(self.service,'')
        self.assertEqual(self.snapshot['config'],{})
        self.assertEqual(self.snapshot['devices'],{})
        self.assertIn(('timeout','20','docker','stop','--time','10','owned-container'),self.effects)
        self.assertFalse(any('/srv/' in str(effect) or 'volume' in effect for effect in self.effects))

    def test_changed_retirement_evidence_still_refuses_before_effects(self):
        self.container['Mounts']=[{'Type':'bind','Source':'/srv/agents','Destination':'/data','RW':False}]
        self.container['Config']['Env']=['FIRST=one','SECOND=two']
        self.container['HostConfig']={'ReadonlyRootfs':False}
        self.snapshot['config'][retirement.PROVISION]='a'*64
        container, snapshot, service=copy.deepcopy(self.container),copy.deepcopy(self.snapshot),self.service
        for change in ('mount-source','mount-destination','mount-mode','mount-field','mount-duplicate',
                       'container-id','container-running','container-image','config-env-order',
                       'host-config','service','provision','route'):
            with self.subTest(change=change):
                self.container,self.snapshot,self.service=copy.deepcopy(container),copy.deepcopy(snapshot),service
                self.effects=[]
                observed,_=retirement.observe()
                if change=='mount-source': self.container['Mounts'][0]['Source']='/srv/other'
                elif change=='mount-destination': self.container['Mounts'][0]['Destination']='/other'
                elif change=='mount-mode': self.container['Mounts'][0]['RW']=True
                elif change=='mount-field': self.container['Mounts'][0]['Propagation']='rshared'
                elif change=='mount-duplicate': self.container['Mounts'].append(copy.deepcopy(self.container['Mounts'][0]))
                elif change=='container-id': self.container['Id']='different'
                elif change=='container-running': self.container['State']['Running']=False
                elif change=='container-image': self.container['Image']='other-image'
                elif change=='config-env-order': self.container['Config']['Env'].reverse()
                elif change=='host-config': self.container['HostConfig']['ReadonlyRootfs']=True
                elif change=='service': self.service=service.replace('active\n','')
                elif change=='provision': self.snapshot['config'][retirement.PROVISION]='b'*64
                elif change=='route': self.snapshot['devices']['ai-observer']['connect']='tcp:127.0.0.1:9999'
                with self.assertRaises(ValueError): self.apply(observed)
                self.assertEqual(self.effects,[])

    def test_docker_empty_value_rewrite_preserves_observation(self):
        self.container['Config']['Env']=None
        self.container['HostConfig']={'Dns':None,'Binds':[],'Tmpfs':None,'ReadonlyRootfs':False,'Memory':0}
        observed,_=retirement.observe()
        self.container['Config']['Env']=[]
        self.container['HostConfig']={'Dns':[],'Binds':None,'Tmpfs':{},'ReadonlyRootfs':False,'Memory':0}
        self.assertEqual(retirement.observe()[0],observed)
        for change in ({'Dns':['192.0.2.1']},{'ReadonlyRootfs':True},{'Memory':1}):
            with self.subTest(change=change):
                changed=dict(self.container['HostConfig'],**change)
                current,self.container['HostConfig']=self.container['HostConfig'],changed
                self.assertNotEqual(retirement.observe()[0],observed)
                self.container['HostConfig']=current

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


class GuestReadinessTest(unittest.TestCase):
    """A booting guest gets a bounded wait; an unanswered settled guest does not."""
    def readiness(self, started, answers, clock_step=1.0):
        calls, clock = [], [0.0]
        def run(args, **options):
            calls.append(args)
            answer = answers[min(len(calls)-1, len(answers)-1)]
            code = answer if type(answer) is int else 0 if answer else 1
            return subprocess.CompletedProcess(args, code, b'', b'')
        def monotonic():
            clock[0] += clock_step
            return clock[0]
        snapshot = {'status': 'Running', 'last_used_at': started}
        with patch.object(retirement.subprocess, 'run', run), patch.object(retirement.time, 'monotonic', monotonic), \
             patch.object(retirement.time, 'sleep', lambda seconds: None):
            try:
                retirement.wait_guest_ready(snapshot)
                return 'ready', calls
            except ValueError as error:
                return str(error), calls

    def stamp(self, age):
        import datetime
        moment = datetime.datetime.now(datetime.timezone.utc) - datetime.timedelta(seconds=age)
        return moment.strftime('%Y-%m-%dT%H:%M:%S.123456789Z')

    def test_booting_guest_is_awaited_until_it_answers(self):
        result, calls = self.readiness(self.stamp(5), [False, False, True])
        self.assertEqual((result, len(calls)), ('ready', 3))
        self.assertEqual(calls[-1][-1], retirement.READY_PROBE)

    def test_booting_guest_wait_is_bounded(self):
        result, calls = self.readiness(self.stamp(5), [False], clock_step=30.0)
        self.assertEqual(result, 'native-unavailable')
        self.assertLess(len(calls), 4)

    def test_unfinished_boot_is_not_ready_while_the_guest_boots(self):
        # The probe's own boot wait timed out: keep waiting, then report it.
        unfinished = retirement.UNFINISHED
        result, calls = self.readiness(self.stamp(5), [unfinished, unfinished, True])
        self.assertEqual((result, len(calls)), ('ready', 3))
        result, calls = self.readiness(self.stamp(5), [unfinished], clock_step=30.0)
        self.assertEqual(result, 'native-unavailable')
        self.assertLess(len(calls), 4)

    def test_probe_reports_unfinished_boot_after_its_timeout(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as root:
            for state, code in (('', retirement.UNFINISHED), ('starting', retirement.UNFINISHED), ('running', 0), ('degraded', 0)):
                fake = Path(root)/'systemctl'
                fake.write_text(f"#!/bin/sh\nprintf '%s' '{state}'\nexit 1\n")
                fake.chmod(0o700)
                timeout = Path(root)/'timeout'
                timeout.write_text('#!/bin/sh\nshift\nexec "$@"\n')
                timeout.chmod(0o700)
                result = subprocess.run(['sh', '-c', retirement.READY_PROBE], env=dict(os.environ, PATH=f'{root}:/usr/bin:/bin'), timeout=30)
                self.assertEqual(result.returncode, code, state)

    def test_settled_guest_that_answers_is_observed_as_is(self):
        result, calls = self.readiness(self.stamp(3600), [retirement.UNFINISHED])
        self.assertEqual((result, len(calls)), ('ready', 1))

    def test_settled_guest_without_agent_is_unavailable_at_once(self):
        result, calls = self.readiness(self.stamp(3600), [False])
        self.assertEqual((result, len(calls)), ('native-unavailable', 1))
        result, calls = self.readiness('', [False])
        self.assertEqual((result, len(calls)), ('native-unavailable', 1))

    def test_stopped_guest_is_not_awaited(self):
        calls = []
        def run(args, **options):
            calls.append(args)
            return subprocess.CompletedProcess(args, 1, b'', b'')
        with patch.object(retirement.subprocess, 'run', run), patch.object(retirement.time, 'sleep', lambda seconds: self.fail('waited')):
            with self.assertRaises(ValueError) as raised:
                retirement.wait_guest_ready({'status': 'Stopped', 'last_used_at': self.stamp(5)})
        self.assertEqual((str(raised.exception), len(calls)), ('native-unavailable', 1))


class LifecycleDiagnosticTest(unittest.TestCase):
    """The core parser requires `<phase> <reason>`; a bare reason is discarded."""
    def test_failure_record_names_the_lifecycle_phase(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as root:
            fake = Path(root)/'incus'
            fake.write_text('#!/bin/sh\nexit 1\n')
            fake.chmod(0o700)
            environment = dict(os.environ, PATH=f'{root}:/usr/bin:/bin', INCUS_PROJECT='fixture', YARD_INSTANCE_NAME='yard')
            for arguments, phase in ((['observe'], 'inspect'), (['apply', 'op-retire', 'a', 'b'], 'apply')):
                result = subprocess.run([sys.executable, '-B', str(PACKAGE/'retire.py'), *arguments], env=environment, capture_output=True, text=True, timeout=30)
                self.assertEqual(result.returncode, 1, result.stdout)
                self.assertEqual(result.stderr.strip().splitlines()[-1], f'subyard-lifecycle-diagnostic-v1 {phase} native-unavailable')


if __name__=='__main__': unittest.main()
