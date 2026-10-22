#!/usr/bin/env python3
"""Exact route ownership and combined assessment behavior."""
import contextlib
import ctypes
import io
import subprocess
import importlib.util
import json
import os
from pathlib import Path
import sys
import tempfile
import threading
import time
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
        if args==('incus','query','/1.0/instances?recursion=1&all-projects=true'):
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

    def test_deferred_route_keeps_original_origin_and_converges_without_publication(self):
        self.defer()
        value,_,actual,desired,origin=dashboard.observe(True,'dev','claude codex')
        self.assertEqual(value['state'],'current')
        self.assertEqual(value['actual'],value['desired'])
        self.assertEqual(actual,desired)
        self.assertEqual(origin,'http://100.64.1.2:22222')
        with patch.object(sys,'argv',['dashboard.py','check','1','dev','claude codex']):
            dashboard.main()
        self.assertNotIn(dashboard.DEVICE,self.snapshot['devices'])
        self.assertEqual(self.effects,[])

    def test_deferred_route_does_not_hide_native_drift_or_recipe_change(self):
        self.defer()
        original=json.dumps(self.snapshot,sort_keys=True)
        self.native={'state':'stale','actual':'c'*64,'desired':'a'*64}
        with patch.object(sys,'argv',['dashboard.py','check','1','dev','claude codex']):
            with self.assertRaises(ValueError): dashboard.main()
        self.native={'state':'current','actual':'a'*64,'desired':'a'*64}
        self.environment['OPENLIT_HOST_PORT']='22223'
        with patch.object(sys,'argv',['dashboard.py','check','1','dev','claude codex']):
            with self.assertRaisesRegex(ValueError,'recipe changed'): dashboard.main()
        self.assertEqual(json.dumps(self.snapshot,sort_keys=True),original)
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
        with patch.object(sys,'argv',['dashboard.py','check','1','dev','claude codex']):
            with self.assertRaises(ValueError): dashboard.main()
        with patch.object(sys,'argv',['dashboard.py','refresh','1','dev','claude codex',before['actual'],before['desired']]):
            with self.assertRaises(ValueError): dashboard.main()
        self.assertEqual(self.effects,[])

    def test_vm_has_no_owner_route(self):
        self.environment['YARD_KIND']='vm'
        value,_,actual,desired,origin=dashboard.observe(True,'dev','claude codex')
        self.assertEqual(value['state'],'stale')
        self.assertEqual(desired,{'marker':'','device':None})
        self.assertEqual(origin,'http://127.0.0.1:22222')

    def test_refresh_cannot_report_current_for_a_different_desired_assessment(self):
        before = dashboard.observe(True,'dev','claude codex')[0]
        after = dict(before, actual='b'*64, desired='b'*64)
        observation = (before, self.native, {}, {}, '')
        with patch.object(dashboard,'observe',side_effect=[observation, (after,)]), \
             patch.object(dashboard,'route',return_value=({}, {}, '')), \
             patch.object(dashboard,'publish'), \
             patch.object(sys,'argv',['dashboard.py','refresh','1','dev','claude codex',before['actual'],before['desired']]):
            with self.assertRaisesRegex(ValueError,'desired assessment changed'):
                dashboard.main()

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
                # A scaled real download outlives the former cold transport.
                subprocess.run([sys.executable,'-c','import time; time.sleep(1)'],check=True,timeout=options['timeout']/1000)
            return b''
        with patch.object(dashboard,'run',side_effect=transport),patch.object(dashboard,'cold_run',side_effect=transport):
            for mode in ('apply','observe','pause','resume','refresh'):
                self.assertEqual(ORIGINAL_GUEST(mode,True,'dev','claude codex',''), '')
                self.assertEqual(dashboard.PHASE,'guest-'+mode)
        self.assertEqual(calls,[('apply',1560),('observe',30),('pause',120),('resume',120),('refresh',110)])

    def test_guest_failure_projects_only_allowlisted_progress(self):
        captured=io.StringIO()
        error=subprocess.CalledProcessError(1,['incus'],stderr=b'PRIVATE_SENTINEL\nopenlit: guest stage=image-pull\nopenlit: pinned image pull exit=124\n')
        with patch.object(dashboard.subprocess,'run',side_effect=error),contextlib.redirect_stderr(captured):
            with self.assertRaises(subprocess.CalledProcessError): ORIGINAL_RUN('incus')
        self.assertEqual(captured.getvalue(),'openlit: guest progress=image-pull\nopenlit: guest image-pull exit=124\n')

    def test_host_transport_retains_the_checked_owner_descriptor(self):
        reader, writer = os.pipe()
        try:
            os.set_blocking(reader, False)
            with patch.object(dashboard,'owner_descriptors',return_value=(writer,)):
                ORIGINAL_RUN(sys.executable,'-c','import os;os.write('+str(writer)+',b"retained")')
            self.assertEqual(os.read(reader,32),b'retained')
        finally:
            os.close(reader)
            os.close(writer)

    def test_compose_failure_reason_is_closed_and_keeps_native_error(self):
        for message, reason in (
                ('application not healthy after 3m0s', 'wait-deadline'),
                ('dependency failed to start: container PRIVATE_SENTINEL is unhealthy', 'unhealthy'),
                ('dependency failed to start: PRIVATE_SENTINEL', 'dependency-start'),
                ('PRIVATE_SENTINEL unknown timeout payload', 'unknown')):
            with self.subTest(reason=reason):
                error=subprocess.CalledProcessError(1,['incus'],stderr=(
                    'PRIVATE_SENTINEL\nopenlit: guest stage=compose-start\n'+message+'\n').encode())
                captured=io.StringIO()
                with patch.object(dashboard.subprocess,'run',side_effect=error),contextlib.redirect_stderr(captured):
                    with self.assertRaises(subprocess.CalledProcessError) as caught: ORIGINAL_RUN('incus')
                self.assertIs(caught.exception,error)
                self.assertEqual(captured.getvalue(),'openlit: guest progress=compose-start\nopenlit: compose-start reason='+reason+'\n')


class ColdTransportTest(unittest.TestCase):
    def setUp(self):
        self.temporary=tempfile.TemporaryDirectory(prefix='openlit-cold-',dir='/tmp')
        self.addCleanup(self.temporary.cleanup)
        self.root=Path(self.temporary.name); self.root.chmod(0o700)
        self.profile=self.root/'checkout/config/profiles/openlit'
        self.profile.mkdir(parents=True,mode=0o700); self.profile.chmod(0o700)
        self.bin=self.root/'bin'; self.bin.mkdir(mode=0o700); self.bin.chmod(0o700)
        self.published=self.root/'published'
        def write(path,text,mode=0o600):
            path.write_text(text); path.chmod(mode)
        self.write=write
        write(self.bin/'incus', '#!/usr/bin/env python3\nimport os,sys\na=sys.argv[sys.argv.index("--")+1:]; os.execvp(a[0],a)\n',0o700)
        for name in dashboard.FILES: write(self.profile/name,'')
        library=self.profile.parents[2]/'internal/adapters/configmaterial'
        library.mkdir(parents=True,mode=0o700); library.chmod(0o700); write(library/'tomli_w.py','')
        # Exercise the owning pull command, with only its seconds scaled. Incus
        # is a real exec transport: stdin and cancellation cross a native pipe.
        source=(PROFILE/'guest.sh').read_text()
        pull=source[source.index('  if [ "$missing" = 1 ]; then'):source.index('\npublish_asset()')]
        pull=pull.rsplit('\nfi',1)[0].replace('pull_limit=900','pull_limit=0.9').replace('pull_limit=20','pull_limit=0.02')
        write(self.profile/'guest.sh', '#!/bin/bash\nset -euo pipefail\nmode=$1\nmissing=1\nroot='+str(self.profile)+'\nstate='+str(self.root)+'\nstage(){ :; }\ndie(){ exit 1; }\n'+pull+'\ntouch '+str(self.published)+'\n')
        write(self.bin/'docker','#!/bin/bash\nsleep 0.3\n',0o700)
        self.patchers=[patch.object(dashboard,'ROOT',self.profile),
            patch.object(dashboard,'ENV',{'INCUS_PROJECT':'fixture','YARD_INSTANCE_NAME':'yard'}),
            patch.dict(os.environ,{'PATH':str(self.bin)+os.pathsep+os.environ['PATH']}),
            patch.dict(dashboard.GUEST_TIMEOUTS,{'apply':0.9}),
            patch.object(dashboard,'COLD_CANCEL_SECONDS',0.2,create=True),
            patch.object(dashboard,'COLD_KILL_SECONDS',0.05,create=True)]
        for p in self.patchers: p.start(); self.addCleanup(p.stop)

    def test_cold_pull_crosses_old_leaf_budget_with_normal_input_eof(self):
        with patch.dict(dashboard.GUEST_TIMEOUTS, {'apply':5}):
            dashboard.guest('apply',True,'dev','claude codex','')
        self.assertTrue(self.published.exists())

    def hanging_pull(self):
        self.pids=self.root/'children'
        self.write(self.bin/'docker', '#!/usr/bin/env python3\nimport os,signal,time\nsignal.signal(signal.SIGTERM,signal.SIG_IGN)\nchild=os.fork()\nif child==0:\n while True: time.sleep(1)\nopen('+repr(str(self.pids))+',"w").write(str(os.getpid())+" "+str(child)+" "+str(os.getppid()))\nwhile True: time.sleep(1)\n',0o700)
        # Reap this fixture's orphaned descendants ourselves instead of relying
        # on the test host's init. Production's direct child is reaped remotely.
        libc=ctypes.CDLL(None,use_errno=True)
        previous=ctypes.c_int()
        self.assertEqual(libc.prctl(37,ctypes.byref(previous),0,0,0),0)
        self.assertEqual(libc.prctl(36,1,0,0,0),0)
        self.addCleanup(lambda: libc.prctl(36,previous.value,0,0,0))
        self.addCleanup(self.reap_helpers)

    def reap_helpers(self):
        if not self.pids.exists(): return
        pending=set(map(int,self.pids.read_text().split()))
        if hasattr(self,'transport') and self.transport.exists():
            pending.add(int(self.transport.read_text()))
        end=time.monotonic()+1
        while pending and time.monotonic()<end:
            for pid in list(pending):
                try:
                    if os.waitpid(pid,os.WNOHANG)[0]: pending.remove(pid)
                except ChildProcessError:
                    try: os.kill(pid,0)
                    except ProcessLookupError: pending.remove(pid)
            if pending: time.sleep(0.01)
        if pending:
            for pid in pending:
                try: os.kill(pid,9)
                except ProcessLookupError: pass
            for pid in pending:
                try: os.waitpid(pid,0)
                except ChildProcessError: pass
        self.assertFalse(bool(pending),'guest cancellation left a live owned descendant')

    def test_hung_aggregate_kills_descendants_before_publication(self):
        self.hanging_pull()
        started=time.monotonic()
        with self.assertRaises(subprocess.CalledProcessError) as caught:
            dashboard.guest('apply',True,'dev','claude codex','')
        self.assertEqual(caught.exception.returncode,124)
        self.assertLess(time.monotonic()-started,1.4)
        self.assertTrue(self.pids.exists())
        self.assertFalse(self.published.exists())
        self.reap_helpers()

    def test_nested_phase_timeout_cannot_escape_cold_group(self):
        self.hanging_pull()
        source=(PROFILE/'guest.sh').read_text()
        compose=source[source.index('compose() {'):source.index('\nnative()')]
        self.write(self.profile/'guest.sh','#!/bin/bash\nset -euo pipefail\nmode=$1\nroot='+str(self.profile)+'\nstate='+str(self.root)+'\n'+compose+'\ncompose config --images\n: # Keep Bash alive; do not optimize the nested timeout into exec.\ntouch '+str(self.published)+'\n')
        with self.assertRaises(subprocess.SubprocessError):
            dashboard.guest('apply',True,'dev','claude codex','')
        self.assertTrue(self.pids.exists())
        self.assertFalse(self.published.exists())
        self.reap_helpers()

    def test_stopped_cold_leader_stage_is_removed_after_forced_kill(self):
        self.hanging_pull()
        stage_marker=self.root/'archive-stage'
        self.write(self.profile/'guest.sh','#!/bin/bash\nset -euo pipefail\numask 077\nprintf "%s" "$(dirname "${BASH_SOURCE[0]}")" > '+str(stage_marker)+'\nchmod 600 '+str(stage_marker)+'\ndocker &\nfor attempt in {1..100}; do [ ! -e '+str(self.pids)+' ] || break; sleep 0.005; done\nleader="$(python3 -c \'import os; print(os.getpgrp())\')"\nkill -STOP "$leader"\nwait\n: # Keep the inner Bash alive while its child ignores TERM.\n')
        try:
            with self.assertRaises(subprocess.SubprocessError):
                dashboard.guest('apply',True,'dev','claude codex','')
            self.assertTrue(stage_marker.exists())
            stage=Path(stage_marker.read_text())
            self.assertEqual(stage.parent,Path('/tmp'))
            self.assertTrue(stage.name.startswith('subyard-openlit.'))
            self.reap_helpers()
            self.assertFalse(stage.exists(),'forced KILL left the owned cold archive directory')
        finally:
            # Baseline RED cleanup is test-owned and occurs only after the
            # product result is measured; it must not make that result pass.
            if stage_marker.exists():
                stage=Path(stage_marker.read_text())
                if stage.exists():
                    self.assertFalse(stage.is_symlink())
                    self.assertEqual(stage.stat().st_uid,os.getuid())
                    self.assertEqual(stage.stat().st_mode&0o777,0o700)
                    import shutil
                    shutil.rmtree(stage)

    def test_real_caller_cancel_closes_transport_and_reaps_guest_group(self):
        self.hanging_pull()
        done=threading.Event()
        def cancel():
            end=time.monotonic()+1
            while not done.wait(0.005) and time.monotonic()<end:
                if self.pids.exists():
                    os.kill(os.getpid(),15)
                    return
        timer=threading.Thread(target=cancel)
        timer.start()
        started=time.monotonic()
        try:
            # Manual cancellation must win before the unrelated guest watchdog.
            with patch.dict(dashboard.GUEST_TIMEOUTS, {'apply':5}), self.assertRaises(SystemExit) as caught:
                dashboard.guest('apply',True,'dev','claude codex','')
            self.assertEqual(caught.exception.code,143)
        finally:
            done.set(); timer.join(timeout=1)
        self.assertLess(time.monotonic()-started,0.8)
        self.assertTrue(self.pids.exists())
        self.assertFalse(self.published.exists())
        self.reap_helpers()

    def test_refresh_cancel_closes_transport_and_reaps_guest_group(self):
        # Refresh has the same foreground lifetime as cold apply, including a
        # nested timeout whose child ignores TERM and must be killed as a group.
        self.hanging_pull()
        source=(PROFILE/'guest.sh').read_text()
        compose=source[source.index('compose() {'):source.index('\nnative()')]
        self.write(self.profile/'guest.sh','#!/bin/bash\nset -euo pipefail\nmode=$1\nroot='+str(self.profile)+'\nstate='+str(self.root)+'\n'+compose+'\ncompose config --images\ntouch '+str(self.published)+'\n')
        with patch.dict(dashboard.GUEST_TIMEOUTS, {'refresh':0.9}):
            with self.assertRaises(subprocess.SubprocessError):
                dashboard.guest('refresh',True,'dev','claude codex','')
        self.assertTrue(self.pids.exists())
        self.assertFalse(self.published.exists())
        self.reap_helpers()

    def test_refresh_caller_loss_closes_guest_liveness_pipe(self):
        self.hanging_pull()
        self.transport=self.root/'transport'
        self.write(self.bin/'incus','#!/usr/bin/env python3\nimport os,sys\nopen('+repr(str(self.transport))+',"w").write(str(os.getpid()))\na=sys.argv[sys.argv.index("--")+1:];os.execvp(a[0],a)\n',0o700)
        self.write(self.profile/'guest.sh','#!/bin/bash\nset -euo pipefail\ndocker &\nwait\ntouch '+str(self.published)+'\n')
        program = ('import importlib.util\nfrom pathlib import Path\n'
                   'spec=importlib.util.spec_from_file_location("dashboard",'+repr(str(PROFILE/'dashboard.py'))+')\n'
                   'dashboard=importlib.util.module_from_spec(spec);spec.loader.exec_module(dashboard)\n'
                   'dashboard.ROOT=Path('+repr(str(self.profile))+')\n'
                   'dashboard.ENV={"INCUS_PROJECT":"fixture","YARD_INSTANCE_NAME":"yard"}\n'
                   'dashboard.GUEST_TIMEOUTS["refresh"]=5\n'
                   'dashboard.COLD_CANCEL_SECONDS=0.2;dashboard.COLD_KILL_SECONDS=0.05\n'
                   'dashboard.guest("refresh",True,"dev","claude codex","")\n')
        caller = subprocess.Popen([sys.executable,'-c',program], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        try:
            end=time.monotonic()+2
            while not self.pids.exists() and caller.poll() is None and time.monotonic()<end:
                time.sleep(0.005)
            self.assertTrue(self.pids.exists())
            caller.kill()
            self.assertEqual(caller.wait(timeout=1),-9)
            self.reap_helpers()
            self.assertFalse(self.published.exists())
        finally:
            if caller.poll() is None:
                caller.kill()
                caller.wait(timeout=1)

    def test_refresh_retains_its_pull_subdeadline(self):
        with self.assertRaises(subprocess.CalledProcessError) as caught:
            dashboard.guest('refresh',True,'dev','claude codex','')
        self.assertEqual(caught.exception.returncode,1)
        self.assertFalse(self.published.exists())

    def test_cold_budget_is_not_reset_for_host_publication(self):
        self.write(self.bin/'docker','#!/bin/bash\nexit 0\n',0o700)
        def observe(*args):
            ORIGINAL_RUN(sys.executable,'-c','import time; time.sleep(0.1)')
            return {},{'state':'stale','actual':'a','desired':'b'},{},{},''
        def fresh_route(*args):
            self.assertTrue(self.published.exists())
            ORIGINAL_RUN(sys.executable,'-c','import time; time.sleep(0.8)')
            return {},{},''
        started=time.monotonic()
        with patch.object(dashboard,'observe',side_effect=observe),patch.object(dashboard,'route',side_effect=fresh_route) as route_mock,patch.object(dashboard,'publish') as publish,patch.object(sys,'argv',['dashboard.py','apply','1','dev','claude codex']):
            with self.assertRaises(subprocess.TimeoutExpired): dashboard.main()
            route_mock.assert_called_once_with(True)
            publish.assert_not_called()
        self.assertLess(time.monotonic()-started,1.3)
        self.assertIsNone(dashboard.COLD_DEADLINE)


if __name__=='__main__': unittest.main()
