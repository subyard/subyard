#!/usr/bin/env python3
"""Closed runtime differences and opt-in owner observation output."""
import copy
import importlib.util
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

PROFILE = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(PROFILE))
import runtime

spec = importlib.util.spec_from_file_location('openlit_dashboard', PROFILE/'dashboard.py')
dashboard = importlib.util.module_from_spec(spec)
spec.loader.exec_module(dashboard)


class RuntimeDiagnostics(unittest.TestCase):
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
