#!/usr/bin/env python3
"""Closed runtime differences and opt-in owner observation output."""
import copy
import importlib.util
import json
import io
from contextlib import redirect_stdout
import os
from pathlib import Path
import sys
import tempfile
import time
import unittest
from unittest.mock import patch

PROFILE = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(PROFILE))
import runtime

spec = importlib.util.spec_from_file_location('openlit_dashboard', PROFILE/'dashboard.py')
dashboard = importlib.util.module_from_spec(spec)
spec.loader.exec_module(dashboard)


class RuntimeDiagnostics(unittest.TestCase):
    def test_startup_uses_only_positive_service_and_container_transitions(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as temporary:
            state = Path(temporary)/'state'
            state.mkdir(mode=0o700)
            runtime.native.write(state/'marker', runtime.MARKER)
            unit = Path(temporary)/'subyard-openlit.service'
            unit.write_text('# Managed by Subyard OpenLIT profile.\n')
            unit.chmod(0o644)
            rows = [dict(Config=dict(Labels={'com.docker.compose.project':'subyard-openlit',
                                            'com.docker.compose.service':name}),
                         State=dict(Status='running', Health=dict(Status='healthy')))
                    for name in sorted(runtime.SERVICES)]
            manager = dict(ActiveState='active', SubState='running', UnitFileState='enabled', LoadState='loaded', FragmentPath=str(unit))
            timer = dict(ActiveState='active', UnitFileState='enabled', Job='')
            receiver_failure = False
            def run(arguments):
                if arguments[0] == 'curl':
                    if receiver_failure: raise ValueError('native runtime probe failed')
                    return b'405' if '/v1/' in arguments[-1] else b'ok'
                if arguments[0] == 'systemctl':
                    fields = timer if arguments[2] == 'subyard-openlit-trace-cutover.timer' else manager
                    return '\n'.join(key+'='+value for key,value in fields.items()).encode()
                if arguments[1] == 'ps': return b'one two three'
                return json.dumps(rows).encode()
            with patch.object(runtime, 'run', side_effect=run):
                self.assertEqual(runtime.startup(state, unit, True), {'state':'ready'})
                for active, job, expected in [('inactive','','drift'), ('failed','','drift'),
                                             ('inactive','17','starting'), ('active','','ready')]:
                    timer.update(ActiveState=active, Job=job)
                    self.assertEqual(runtime.startup(state, unit, True)['state'], expected)
                timer.update(ActiveState='inactive', Job='')
                manager.update(ActiveState='activating', SubState='start')
                self.assertEqual(runtime.startup(state, unit, True)['reason'], 'stack service is activating')
                timer['Job']='17'
                manager['ActiveState']='failed'
                self.assertEqual(runtime.startup(state, unit, True)['reason'], 'trace cutover timer start job is pending')
                timer.update(ActiveState='active', Job='')
                for active, sub, expected in [('inactive','dead','drift'), ('failed','failed','drift'),
                                             ('activating','auto-restart','drift'), ('activating','start','starting')]:
                    manager.update(ActiveState=active, SubState=sub)
                    self.assertEqual(runtime.startup(state, unit, True)['state'], expected)
                manager.update(ActiveState='active', SubState='running')
                rows[0]['State']['Health']['Status']='starting'
                self.assertEqual(runtime.startup(state, unit, True)['state'], 'starting')
                rows[0]['State']['Health']['Status']='unhealthy'
                self.assertEqual(runtime.startup(state, unit, True), {'state':'drift'})
                self.assertEqual(runtime.startup(state, unit, False), {'state':'drift'})
                rows[0]['State']['Health']['Status']='healthy'
                receiver_failure = True
                manager['ActiveEnterTimestampMonotonic']=str(time.monotonic_ns()//1000)
                self.assertEqual(runtime.startup(state, unit, True)['state'], 'starting')
                manager['ActiveEnterTimestampMonotonic']='1'
                self.assertEqual(runtime.startup(state, unit, True), {'state':'drift'})
                for budget in ('1000', '0'):
                    with patch.dict(os.environ, {'SUBYARD_STARTUP_REMAINING_MS':budget}):
                        self.assertEqual(runtime.startup(state, unit, True)['state'], 'starting')

    def test_startup_dashboard_transport_skips_full_convergence_and_route(self):
        result = {'state':'starting','reason':'stack containers are starting','hint':'inspect container health'}
        with patch.object(sys, 'argv', ['dashboard.py','startup','1','installed-user','']), \
             patch.object(dashboard, 'guest', return_value=json.dumps(result)) as guest, \
             patch.object(dashboard, 'observe', side_effect=AssertionError('startup ran full convergence')), \
             redirect_stdout(io.StringIO()) as output:
            dashboard.operation()
        self.assertEqual(json.loads(output.getvalue()), result)
        guest.assert_called_once_with('startup', True, 'installed-user', '', '')

    def test_startup_guest_transport_forwards_owner_budget(self):
        with patch.dict(dashboard.ENV, {'YARD_INSTANCE_NAME':'fixture','INCUS_PROJECT':'fixture',
                                       'SUBYARD_STARTUP_REMAINING_MS':'1234'}), \
             patch.object(dashboard, 'run', return_value=b'{"state":"ready"}') as run:
            self.assertEqual(json.loads(dashboard.guest('startup', True, 'fixture', '', '')), {'state':'ready'})
        self.assertIn('--env', run.call_args.args)
        self.assertIn('SUBYARD_STARTUP_REMAINING_MS=1234', run.call_args.args)

    def test_absent_stale_and_current_native_state(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as temporary:
            state = Path(temporary)/'state'
            observation = dict(state='stale', actual='a'*64, desired='b'*64)
            facts = dict(containers=[], service=dict(ActiveState='inactive', UnitFileState='disabled'),
                         timer=dict(ActiveState='active', UnitFileState='enabled'), private='private-sentinel')
            def observe():
                with patch.object(runtime, 'capture', return_value=(copy.deepcopy(observation), copy.deepcopy(facts))):
                    return runtime.observe(state, PROFILE, 'unit', '/home/fixture', True, [], 'fixture')
            self.assertEqual(observe()['diagnostics'],
                             [dict(parameter='openlit.runtime', installed='absent', desired='present')])
            state.mkdir(mode=0o700)
            for active, installed in [('inactive', 'inactive'), ('private-sentinel', 'unknown')]:
                facts['service']['ActiveState'] = active
                result = observe()
                self.assertEqual(result['diagnostics'], [
                    dict(parameter='openlit.service.ActiveState', installed=installed, desired='active'),
                    dict(parameter='openlit.service.UnitFileState', installed='disabled', desired='enabled'),
                    dict(parameter='openlit.receipt', installed='mismatch', desired='matched')])
                self.assertNotIn('private-sentinel', json.dumps(result))
            observation.update(state='current', actual='b'*64)
            facts['service'] = dict(ActiveState='active', UnitFileState='enabled')
            runtime.native.write(state/'receipt.json', json.dumps(dict(schema=1, desired='b'*64, containers=[])))
            self.assertEqual(observe(), observation)

    def test_dashboard_opt_in_preserves_observation_and_closed_rows(self):
        native_rows = [dict(parameter='openlit.receipt', installed='mismatch', desired='matched')]
        for name, selected, state, changed_route, expected in [
            ('native drift', True, 'stale', False, native_rows),
            ('route drift', True, 'current', True, [dict(parameter='openlit.route', installed='changed', desired='current')]),
            ('current', True, 'current', False, []),
            ('absent', False, 'absent', False, []),
        ]:
            with self.subTest(name=name):
                native = dict(state=state, actual='a'*64, desired='b'*64)
                if state == 'stale':
                    native['diagnostics'] = native_rows
                observations = []
                for enabled in ('0', '1'):
                    with patch.dict(os.environ, {'SUBYARD_PROFILE_DIAGNOSTICS': enabled}), \
                         patch.object(dashboard, 'route', return_value=({'port': 1}, {'port': 2 if changed_route else 1}, 'private-sentinel')), \
                         patch.object(dashboard, 'guest', return_value=json.dumps(native)):
                        result, returned_native, *_ = dashboard.observe(selected, 'fixture', '')
                    self.assertEqual(returned_native, {key: value for key, value in native.items() if key != 'diagnostics'})
                    self.assertNotIn('private-sentinel', json.dumps(result))
                    observations.append(result)
                self.assertNotIn('diagnostics', observations[0])
                if not expected:
                    self.assertNotIn('diagnostics', observations[1])
                self.assertEqual(observations[1].pop('diagnostics', []), expected)
                self.assertEqual(observations[0], observations[1])


if __name__ == '__main__':
    unittest.main()
