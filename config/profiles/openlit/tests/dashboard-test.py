#!/usr/bin/env python3
"""Exact route ownership and combined assessment behavior."""
import contextlib
import io
import subprocess
import importlib.util
import json
import os
from pathlib import Path
import sys
import unittest
from unittest.mock import patch

PROFILE = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('dashboard', PROFILE/'dashboard.py')
dashboard = importlib.util.module_from_spec(spec)
spec.loader.exec_module(dashboard)
ORIGINAL_RUN=dashboard.run
ORIGINAL_GUEST=dashboard.guest

DEFERRED="user.subyard.deferred_publication.openlit-dashboard"

class DashboardTest(unittest.TestCase):
    def setUp(self):
        self.environment={'INCUS_PROJECT':'fixture','YARD_INSTANCE_NAME':'yard','YARD_KIND':'container','OPENLIT_HOST_PORT':'22222'}
        self.snapshot={'config':{'user.subyard.openlit_dashboard':'v1:22222'},'devices':{'openlit-dashboard':{'type':'proxy','bind':'host','listen':'tcp:127.0.0.1:22222','connect':'tcp:127.0.0.1:3000'}}}
        self.native={'state':'current','actual':'a'*64,'desired':'a'*64}
        self.instances=[]
        self.owner_host='127.0.0.1'
        self.effects=[]
        self.patchers=[patch.object(dashboard,'ENV',self.environment),patch.object(dashboard,'run',self.run_native),patch.object(dashboard,'guest',self.guest)]
        for patcher in self.patchers: patcher.start(); self.addCleanup(patcher.stop)

    def run_native(self,*args,**options):
        if args[0]=='bash': return (self.owner_host+'\n').encode()
        if args[:2]==('incus','list'):
            self.assertEqual(args[2:],('--all-projects','--format','json'))
            return json.dumps(self.instances).encode()
        if args[:2]==('incus','query'):
            return json.dumps([] if 'recursion=1' in args[2] else self.snapshot).encode()
        self.effects.append(args)
        raise AssertionError(args)

    def guest(self,mode,*args):
        self.assertEqual(mode,'observe')
        return json.dumps(self.native)

    def test_current_route_and_runtime_are_read_only(self):
        value,*_=dashboard.observe(True,'dev','claude codex')
        self.assertEqual(value['state'],'current')
        self.assertEqual(value['actual'],value['desired'])
        self.assertEqual(self.effects,[])

    def defer(self):
        marker='v2:100.64.1.2:22222'
        device={'type':'proxy','bind':'host','connect':'tcp:127.0.0.1:3000','listen':'tcp:100.64.1.2:22222'}
        self.snapshot['config']={dashboard.KEY:marker, DEFERRED:json.dumps({'version':1,'ownership':marker,'device':device})}
        self.snapshot['devices']={}
        return device

    def test_deferred_route_keeps_original_origin_and_withholds_readiness(self):
        self.defer()
        value,_,actual,desired,origin=dashboard.observe(True,'dev','claude codex')
        self.assertEqual(value['state'],'current')
        self.assertEqual(value['actual'],value['desired'])
        self.assertEqual(actual,desired)
        self.assertEqual(origin,'http://100.64.1.2:22222')
        with patch.object(sys,'argv',['dashboard.py','check','1','dev','claude codex']):
            with self.assertRaises(ValueError): dashboard.main()
        self.assertEqual(self.effects,[])

    def test_deferred_receipt_is_strict_and_bound_before_guest(self):
        self.defer()
        for value in ('{}', self.snapshot['config'][DEFERRED]+' {}',
                      self.snapshot['config'][DEFERRED].replace('"version": 1','"version": true'),
                      self.snapshot['config'][DEFERRED].replace('"ownership":','"extra":0,"ownership":')):
            with self.subTest(value=value), patch.dict(self.snapshot['config'],{DEFERRED:value}):
                with patch.object(dashboard,'guest',return_value=json.dumps(self.native)) as guest:
                    with self.assertRaises(ValueError): dashboard.observe(True,'dev','claude codex')
                    guest.assert_not_called()

    def test_pending_deferred_receipt_preserves_origin_and_cas_binding(self):
        self.defer()
        before=dashboard.observe(True,'dev','claude codex')[0]
        marker='v2:pending:100.64.1.2:22222'
        self.snapshot['config'][dashboard.KEY]=marker
        saved=json.loads(self.snapshot['config'][DEFERRED]); saved['ownership']=marker
        self.snapshot['config'][DEFERRED]=json.dumps(saved)
        after,_,actual,desired,origin=dashboard.observe(True,'dev','claude codex')
        self.assertEqual(after['state'],'current')
        self.assertEqual(actual,desired)
        self.assertNotEqual(after['actual'],before['actual'])
        self.assertEqual(origin,'http://100.64.1.2:22222')

    def test_cancel_revokes_restore_before_fresh_owned_read(self):
        device=self.defer()
        actual,desired,_=dashboard.route(False)
        def mutation(*args):
            self.effects.append(args)
            if args[:2]==('config','unset'):
                self.snapshot['config'].pop(args[3])
                if args[3]==DEFERRED:
                    # Restore committed just before journal revocation. The next
                    # ownership read must discover and remove this exact device.
                    self.snapshot['devices'][dashboard.DEVICE]=device
            elif args[:3]==('config','device','remove'):
                self.snapshot['devices'].pop(dashboard.DEVICE)
            return b''
        with patch.object(dashboard,'incus',side_effect=mutation): dashboard.publish(actual,desired)
        self.assertEqual(self.effects[0],('config','unset','yard',DEFERRED))
        self.assertEqual(self.snapshot,{'config':{},'devices':{}})

    def test_cancel_refuses_foreign_restore_and_failed_revoke(self):
        for foreign in (True,False):
            self.defer()
            actual,desired,_=dashboard.route(False)
            def mutation(*args):
                if not foreign: raise OSError('revoke failed')
                self.snapshot['config'].pop(DEFERRED)
                self.snapshot['devices'][dashboard.DEVICE]={'type':'disk'}
                return b''
            with patch.object(dashboard,'incus',side_effect=mutation):
                with self.assertRaises((ValueError,OSError)): dashboard.publish(actual,desired)
            self.assertIn(dashboard.KEY,self.snapshot['config'])
        self.assertIn(DEFERRED,self.snapshot['config'])

    def test_stopped_peer_reserves_listener_across_projects_before_guest(self):
        device=self.snapshot['devices']['openlit-dashboard']
        for project,name,device_name in (('other','peer','openlit-dashboard'),
                                         ('other','yard','openlit-dashboard'),
                                         ('fixture','yard','other-device')):
            with self.subTest(project=project,name=name,device=device_name):
                self.instances=[{'project':project,'name':name,'status':'Stopped',
                                 'expanded_devices':{device_name:device}}]
                with patch.object(dashboard,'guest',return_value=json.dumps(self.native)) as guest:
                    with self.assertRaisesRegex(ValueError,'reserved'):
                        dashboard.observe(True,'dev','claude codex')
                    guest.assert_not_called()
                self.assertEqual(self.effects,[])
        # Only the exact current project, instance and device may be excluded.
        self.instances=[{'project':'fixture','name':'yard','status':'Running',
                         'expanded_devices':{'openlit-dashboard':device}}]
        self.assertEqual(dashboard.observe(True,'dev','claude codex')[0]['state'],'current')

    def test_deferred_stopped_peer_reserves_its_owned_future_listener(self):
        device=self.defer()
        peer_config=dict(self.snapshot['config'])
        self.snapshot={'config':{dashboard.KEY:peer_config[dashboard.KEY]},'devices':{dashboard.DEVICE:device}}
        self.owner_host='100.64.1.2'
        for project,name in (('other','peer'),('other','yard')):
            self.instances=[{'project':project,'name':name,'status':'Stopped','config':peer_config,'devices':{}}]
            with patch.object(dashboard,'guest',return_value=json.dumps(self.native)) as guest:
                with self.assertRaisesRegex(ValueError,'reserved'): dashboard.observe(True,'dev','claude codex')
                guest.assert_not_called()
        # A foreign journal is never accepted as proof of an owned listener.
        self.instances[0]['config']={**peer_config,dashboard.KEY:'foreign'}
        with self.assertRaises(ValueError): dashboard.route(True)
        self.instances=[{'project':'fixture','name':'yard','status':'Stopped','config':peer_config,'devices':{}}]
        self.assertEqual(dashboard.observe(True,'dev','claude codex')[0]['state'],'current')

    def test_foreign_and_inherited_routes_are_rejected_before_guest(self):
        self.snapshot['devices']['openlit-dashboard']['connect']='tcp:127.0.0.1:4318'
        with self.assertRaises(ValueError): dashboard.observe(True,'dev','claude codex')
        self.assertEqual(self.effects,[])

    def test_pending_route_cannot_publish_ready_and_changes_binding(self):
        before=dashboard.observe(True,'dev','claude codex')[0]
        self.snapshot['config']['user.subyard.openlit_dashboard']='v1:pending:22222'
        after=dashboard.observe(True,'dev','claude codex')[0]
        self.assertEqual(after['state'],'stale')
        self.assertNotEqual(after['actual'],before['actual'])
        self.assertEqual(after['desired'],before['desired'])
        with patch.object(sys,'argv',['dashboard.py','refresh','1','dev','claude codex',before['actual'],before['desired']]):
            with self.assertRaises(ValueError): dashboard.main()
        self.assertEqual(self.effects,[])

    def test_vm_has_no_owner_route(self):
        self.environment['YARD_KIND']='vm'
        value,_,actual,desired,origin=dashboard.observe(True,'dev','claude codex')
        self.assertEqual(value['state'],'stale')
        self.assertEqual(desired,{'marker':'','device':None})
        self.assertEqual(origin,'http://127.0.0.1:22222')

    def test_pause_preserves_exact_instance_route_identity(self):
        before=json.dumps(self.snapshot,sort_keys=True)
        with patch.object(dashboard,'guest',return_value='paused'), patch.object(sys,'argv',['dashboard.py','pause','1','dev','claude codex']):
            dashboard.main()
        self.assertEqual(json.dumps(self.snapshot,sort_keys=True),before)
        self.assertEqual(self.effects,[])

    def test_remove_preserves_only_instance_bound_teardown_route(self):
        for keep_data in ('0','1'):
            with self.subTest(keep_data=keep_data):
                self.environment.update(SUBYARD_TEARDOWN_KEEP_DATA=keep_data,SUBYARD_TEARDOWN_INVENTORY=json.dumps([{'kind':'instance','name':'yard','binding':'b'*64}]))
                self.native={'state':'absent','actual':'','desired':''}
                before=json.dumps(self.snapshot,sort_keys=True)
                with patch.object(sys,'argv',['dashboard.py','apply','0','dev','claude codex']): dashboard.main()
                self.assertEqual(json.dumps(self.snapshot,sort_keys=True),before)
                self.assertEqual(self.effects,[])
        # Scoped disable/other instances do not inherit teardown's route retention.
        self.environment['SUBYARD_TEARDOWN_INVENTORY']=json.dumps([{'kind':'instance','name':'other','binding':'b'*64}])
        _,desired,_=dashboard.route(False)
        self.assertEqual(desired,{'marker':'','device':None})
        self.environment.pop('SUBYARD_TEARDOWN_KEEP_DATA')
        _,desired,_=dashboard.route(False)
        self.assertEqual(desired,{'marker':'','device':None})

    def test_scoped_remove_cleans_owned_route(self):
        self.native={'state':'absent','actual':'','desired':''}
        removed=[]
        def publish(actual,desired):
            removed.append(desired)
            self.snapshot['config']={}
            self.snapshot['devices']={}
        with patch.object(dashboard,'publish',side_effect=publish),patch.object(sys,'argv',['dashboard.py','apply','0','dev','claude codex']): dashboard.main()
        self.assertEqual(removed,[{'marker':'','device':None}])

    def test_resume_keeps_owned_route_and_restores_native_service(self):
        before=json.dumps(self.snapshot,sort_keys=True)
        calls=[]
        def guest(mode,*args):
            calls.append(mode)
            return json.dumps(self.native) if mode=='observe' else ''
        with patch.object(dashboard,'guest',side_effect=guest),patch.object(sys,'argv',['dashboard.py','resume','1','dev','claude codex']): dashboard.main()
        self.assertEqual(calls,['resume'])
        self.assertEqual(json.dumps(self.snapshot,sort_keys=True),before)
        self.assertEqual(self.effects,[])

    def test_transport_preserves_cold_apply_and_bounds_other_modes(self):
        calls=[]
        def transport(*args,**options):
            mode=args[args.index('subyard')+1]
            calls.append((mode,options['timeout']))
            if mode=='apply':
                # A scaled actual child crosses the old transport's pull frontier
                # while remaining inside the cold apply's existing inner budgets.
                subprocess.run([sys.executable,'-c','import time; time.sleep(2.5)'],check=True,timeout=options['timeout']/100)
            return b''
        with patch.object(dashboard,'run',side_effect=transport):
            for mode in ('apply','observe','pause','resume','refresh'):
                self.assertEqual(ORIGINAL_GUEST(mode,True,'dev','claude codex',''), '')
                self.assertEqual(dashboard.PHASE,'guest-'+mode)
        self.assertEqual(calls,[('apply',900),('observe',30),('pause',120),('resume',120),('refresh',110)])

    def test_guest_failure_projects_only_allowlisted_progress(self):
        captured=io.StringIO()
        error=subprocess.CalledProcessError(1,['incus'],stderr=b'PRIVATE_SENTINEL\nopenlit: guest stage=image-pull\nopenlit: pinned image pull exit=124\n')
        with patch.object(dashboard.subprocess,'run',side_effect=error),contextlib.redirect_stderr(captured):
            with self.assertRaises(subprocess.CalledProcessError): ORIGINAL_RUN('incus')
        self.assertEqual(captured.getvalue(),'openlit: guest progress=image-pull\nopenlit: guest image-pull exit=124\n')


if __name__=='__main__': unittest.main()
