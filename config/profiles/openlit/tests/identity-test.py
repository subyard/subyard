#!/usr/bin/env python3
"""Identity receipt, trace boundary and container preservation checks."""
import copy
import importlib.util
import json
from pathlib import Path
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[4]
PROFILE = Path(__file__).resolve().parents[1]
sys.path[:0] = [str(PROFILE), str(ROOT/'config/identity')]
spec = importlib.util.spec_from_file_location('openlit_identity', PROFILE/'identity.py')
m = importlib.util.module_from_spec(spec); spec.loader.exec_module(m)


class Identity(unittest.TestCase):
    def test_receipt_rebind_preserves_containers_and_retired_trace(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as temporary:
            state = Path(temporary); state.chmod(0o700)
            containers = [dict(id='a'*64, image='sha256:'+'b'*64, service='openlit', state='running', health='healthy')]
            receipt = dict(schema=1, desired='a'*64, containers=containers)
            m.native.write(state/'receipt.json', json.dumps(receipt))
            record = dict(phase='retired', home='/home/old', uid=1001, retained='trace-state')
            active = ['active']; calls = []
            def capture(state, source, unit, home, selected, agents, user):
                return dict(state='current', desired=('a' if user == 'old' else 'b')*64), dict(timer=dict(ActiveState=active[0], UnitFileState='enabled'), containers=copy.deepcopy(containers), native={'inode':123}, account_volume_owned=True)
            def run(arguments):
                calls.append(arguments)
                if arguments[-1] == m.TIMER: active[0] = 'inactive' if arguments[1] == 'stop' else 'active'
            def save(state, changed): record.update(changed)
            with patch.object(m.pwd, 'getpwnam', return_value=SimpleNamespace(pw_uid=1001,pw_gid=1002,pw_dir='/home/old')), patch.object(m.grp,'getgrgid',return_value=SimpleNamespace(gr_name='old')), patch.object(m.runtime,'capture',capture), patch.object(m.runtime,'recipe',return_value='b'*64), patch.object(m.runtime,'run',run), patch.object(m.trace_cutover,'record',side_effect=lambda state:copy.deepcopy(record)), patch.object(m.trace_cutover,'admission',return_value=None), patch.object(m.trace_cutover,'save',save):
                with patch.object(m.authority,'load',side_effect=FileNotFoundError): recipe=m.execute('identity-observe',True,'old',[],['new','source'],state=state)
                journal=dict(phase='prepared',plan={})
                with patch.object(m.authority,'load',return_value=(journal,recipe)):
                    m.execute('identity-apply',True,'old',[],['source'],state=state)
                    self.assertEqual(active[0],'inactive')
                    journal['phase']='account-ready'
                    with patch.object(m.pwd,'getpwnam',side_effect=KeyError('old account gone')):
                        m.execute('identity-apply',True,'old',[],['source'],state=state)
                        m.execute('identity-apply',True,'old',[],['source'],state=state)
                    self.assertEqual(json.loads((state/'receipt.json').read_text()),dict(receipt,desired='b'*64))
                    self.assertEqual(record,dict(phase='retired',home='/home/new',uid=1001,retained='trace-state'))
                    self.assertEqual(active[0],'active')
                    self.assertTrue(all(command[0]=='systemctl' and command[-1] in (m.TIMER,m.SERVICE) for command in calls))
                    before=list(calls); containers[0]['id']='c'*64
                    with self.assertRaises(ValueError):m.execute('identity-apply',True,'old',[],['source'],state=state)
                    self.assertEqual(calls,before)

    def test_pending_trace_refuses(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as temporary:
            state=Path(temporary)
            with patch.object(m.trace_cutover,'admission',return_value={'pending':True}):
                with self.assertRaisesRegex(ValueError,'pending trace admission'):m.trace_projection(state,{}, {})
            with patch.object(m.trace_cutover,'admission',return_value=None),patch.object(m.trace_cutover,'record',return_value={'phase':'prepared'}):
                with self.assertRaisesRegex(ValueError,'pending or foreign'):m.trace_projection(state,{}, {})


if __name__=='__main__':unittest.main()
