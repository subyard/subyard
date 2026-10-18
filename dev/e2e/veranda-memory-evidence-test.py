#!/usr/bin/python3
"""Host-free completed-case retention and owned cleanup boundary checks."""
import contextlib
import ast
import hashlib
import importlib.util
import io
import json
import os
import shlex
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

sys.dont_write_bytecode = True
ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('memory_evidence', ROOT / 'dev/e2e/veranda-memory-research.py')
memory = importlib.util.module_from_spec(spec)
spec.loader.exec_module(memory)
fixture = memory.fixture


class EvidenceChecks(unittest.TestCase):
    @contextlib.contextmanager
    def prepared_assets_fixture(self):
        with tempfile.TemporaryDirectory(prefix='veranda-prepared-contract-', dir='/tmp') as temporary:
            root = Path(temporary); root.chmod(0o700)
            assets = root / 'assets'; assets.mkdir(mode=0o700)
            for name in ('runtime', 'image'): (assets / name).mkdir(mode=0o700)
            engine = root / 'yard'; payload = bytearray(64); payload[:4] = b'\x7fELF'; payload[5] = 1; payload[18] = 62
            engine.write_bytes(payload); engine.chmod(0o755)
            archive = assets / 'runtime/subyard-0.99.1-linux-amd64.tar.gz'
            with tarfile.open(archive, 'w:gz') as bundle:
                entry = tarfile.TarInfo('bin/yard-engine'); entry.mode = 0o755; entry.size = len(payload)
                bundle.addfile(entry, io.BytesIO(payload))
            digest = fixture.prepared.digest
            (Path(str(archive) + '.sha256')).write_text(digest(archive) + '  ' + archive.name + '\n')
            (Path(str(archive) + '.manifest.json')).write_text(json.dumps({'version': '0.99.1', 'kind': 'runtime', 'os': 'linux', 'arch': 'amd64'}))
            (Path(str(archive) + '.provenance.json')).write_text(json.dumps({'version': '0.99.1', 'artifact': archive.name, 'sha256': digest(archive)}))
            (assets / 'image/metadata.tar.xz').write_bytes(b'not a native image; validator contract only')
            files = []
            for path in sorted(assets.rglob('*')):
                if path.is_file():
                    path.chmod(0o600)
                    files.append({'path': path.relative_to(assets).as_posix(), 'sha256': digest(path), 'bytes': path.stat().st_size, 'mode': 0o600})
            data = {'schemaVersion': 1, 'engine': {'version': '0.99.1', 'sha256': digest(engine), 'architecture': 'amd64'},
                    'helpers': [], 'runtime': {'bundle': archive.relative_to(assets).as_posix()},
                    'image': {'source': 'images:debian/13', 'fingerprint': 'a' * 64, 'type': 'container', 'architecture': 'x86_64', 'parts': ['image/metadata.tar.xz']}, 'files': files}
            manifest = assets / 'prepared-assets.json'; manifest.write_text(json.dumps(data)); manifest.chmod(0o600)
            yield root, assets, engine, data

    def test_prepared_assets_reject_missing_symlink_digest_engine_and_version(self):
        for fault in ('valid', 'missing', 'symlink', 'digest', 'engine', 'version', 'mode', 'extra'):
            with self.subTest(fault=fault), self.prepared_assets_fixture() as (root, assets, engine, data):
                path = assets / 'image/metadata.tar.xz'
                if fault == 'missing': path.unlink()
                elif fault == 'symlink': path.unlink(); path.symlink_to(engine)
                elif fault == 'digest': path.write_bytes(b'corrupt')
                elif fault == 'engine': engine.write_bytes(engine.read_bytes() + b'changed')
                elif fault == 'mode': path.chmod(0o644)
                elif fault == 'extra': (assets / 'unexpected').touch()
                if fault == 'valid': self.assertEqual(fixture.prepared.validate(assets, engine, '0.99.1', root), data)
                else:
                    with self.assertRaises((AssertionError, FileNotFoundError)):
                        fixture.prepared.validate(assets, engine, 'other' if fault == 'version' else '0.99.1', root)

    def test_prepared_runtime_still_installs_verifies_migrates_and_checks_ready(self):
        with self.prepared_assets_fixture() as (root, assets, engine, data):
            state = root / 'state'; state.mkdir(mode=0o700); env = {'SUBYARD_HOME': str(root / 'data')}; commands = []
            def command(argv, current, log, **options):
                commands.append(argv)
                if '--bundle' in argv:
                    installed = Path(env['YARD_RUNTIME_ROOT']) / 'current/bin/yard-engine'
                    installed.parent.mkdir(parents=True); installed.write_bytes(engine.read_bytes())
                if '--check' in argv: log.write('{"outcome":{"status":"ready"}}')
                return 0
            with patch.object(fixture.subprocess, 'check_output', return_value='yard 0.99.1\n'), \
                 patch.object(fixture, 'ROOT', root), patch.object(fixture, 'run_probe', side_effect=command):
                fixture.prepare_release(state, engine, env, assets)
            self.assertEqual(len(commands), 4)
            self.assertIn('install-runtime-release.sh', commands[0][1])
            self.assertEqual(commands[-2][-2:], ['migrate', '--yes'])
            self.assertEqual(commands[-1][-3:], ['migrate', '--check', '--json'])

    def test_prepared_image_requires_exact_fingerprint_type_and_architecture(self):
        image = {'fingerprint': 'a' * 64, 'type': 'container', 'architecture': 'x86_64'}
        self.assertEqual(fixture.prepared.verify_image([dict(image)], image), image)
        for rows in ([], [dict(image, fingerprint='b' * 64)], [dict(image, type='virtual-machine')],
                     [dict(image, architecture='aarch64')], [dict(image), dict(image)]):
            with self.subTest(rows=rows), self.assertRaises(AssertionError): fixture.prepared.verify_image(rows, image)

    def test_prepared_image_pin_keeps_canonical_init_and_teardown_in_one_context(self):
        with self.prepared_assets_fixture() as (root, assets, engine, data):
            state = root / 'veranda-wayland-prepared'; state.mkdir(mode=0o700)
            platform = root / 'platform'; platform.mkdir(mode=0o700); (platform / 'incus').mkdir(mode=0o700)
            config = state / 'config'; (config / 'yards/prepared').mkdir(parents=True)
            yard_config = config / 'yards/prepared/config.env'; yard_config.write_text('')
            env = {'HOME': str(root), 'SUBYARD_CONFIG_HOME': str(config)}
            events = []
            @contextlib.contextmanager
            def image(*arguments):
                events.append('image-open')
                try: yield 'fixture:' + 'a' * 64
                finally: events.append('image-close')
            def product(argv, current, log, **options):
                events.append(argv[3:-1])
                self.assertEqual(yard_config.read_text(), 'YARD_IMAGE=fixture:' + 'a' * 64 + '\nYARD_IMAGE_FALLBACK=fixture:' + 'a' * 64 + '\n')
                return 0
            with patch.object(fixture, 'ROOT', root), \
                 patch.object(fixture, 'platform_environment', return_value=(platform, 'prepared', env)), \
                 patch.object(fixture, 'product_stop_check'), patch.object(fixture, 'prepare_release') as release, \
                 patch.object(fixture.subprocess, 'check_output', return_value='yard 0.99.1\n'), \
                 patch.object(fixture.subprocess, 'run'), patch.object(fixture, 'run_probe', side_effect=product), \
                 patch.object(fixture.prepared, 'image_remote', side_effect=image):
                with fixture.prepared_platform(state, engine, prepared_assets=assets): events.append('gui')
            release.assert_called_once_with(state, engine, env, assets)
            self.assertEqual(events, ['image-open', ['init'], ['teardown', '--keep-data'], 'gui', ['teardown'], 'image-close'])

    def test_prepared_image_public_remote_is_pinned_and_owned_cleanup_runs_on_failure(self):
        for fault in ('none', 'consumer', 'type', 'preexisting', 'listener', 'tls', 'certificate-missing', 'certificate-malformed'):
            with self.subTest(fault=fault), self.prepared_assets_fixture() as (root, assets, engine, data):
                state = root / 'veranda-wayland-owned'; state.mkdir(mode=0o700)
                home = root / 'home'; home.mkdir(mode=0o700)
                remote = 'veranda-assets-owned'; certificate = '-----BEGIN CERTIFICATE-----\ndGVzdA==\n-----END CERTIFICATE-----\n'
                image = dict(data['image'], public=True, aliases=[{'name': remote}])
                images = [image] if fault == 'preexisting' else []
                remotes = {'local': {'Addr': 'unix://', 'Public': False}}
                listener = ['127.0.0.1:1234' if fault == 'listener' else '']
                commands = []
                def command(argv, timeout):
                    commands.append(argv[1:]); words = argv[1:]
                    if words == ['version']: return 'Client version: fixture\nServer version: fixture\n'
                    if '--help' in words: return '--public --protocol'
                    if words[:2] == ['image', 'list']: return json.dumps(images)
                    if words[:2] == ['remote', 'list']: return json.dumps(remotes)
                    if words[:2] == ['remote', 'get-default']: return 'local'
                    if words[:2] == ['config', 'get']: return listener[0]
                    if words[0] == 'query':
                        # `incus query /1.0` prints the response Metadata directly.
                        environment = {'certificate': certificate, 'certificate_fingerprint': 'fixture', 'server_name': 'fixture'}
                        if fault == 'certificate-missing': del environment['certificate']
                        if fault == 'certificate-malformed': environment['certificate'] = 'invalid PEM'
                        return json.dumps({'api_version': '1.0', 'environment': environment})
                    if words[:2] == ['image', 'import']:
                        images.append(dict(image, type='virtual-machine') if fault == 'type' else image)
                    elif words[:2] == ['image', 'delete']: images.clear()
                    elif words[:2] == ['config', 'set']: listener[0] = words[3]
                    elif words[:2] == ['config', 'unset']: listener[0] = ''
                    elif words[:2] == ['remote', 'add']:
                        self.assertNotIn('--accept-certificate', words)
                        self.assertEqual((home / '.config/incus/servercerts' / (remote + '.crt')).read_text(), certificate)
                        remotes[remote] = {'Addr': words[3], 'Public': True}
                    elif words[:2] == ['remote', 'remove']: del remotes[remote]
                    else: self.fail('unexpected command ' + str(words))
                    return ''
                tls = SimpleNamespace(getpeercert=lambda binary_form: b'wrong' if fault == 'tls' else b'test')
                context = SimpleNamespace(wrap_socket=lambda *a, **k: contextlib.nullcontext(tls))
                reservation = SimpleNamespace(bind=lambda *a: None, getsockname=lambda: ('127.0.0.1', 2345))
                with patch.object(fixture.prepared.socket, 'socket', return_value=contextlib.nullcontext(reservation)), \
                     patch.object(fixture.prepared.socket, 'create_connection', return_value=contextlib.nullcontext(None)), \
                     patch.object(fixture.prepared.ssl, 'SSLContext', return_value=context):
                    def consume():
                        with fixture.prepared.image_remote(assets, data, state, {'HOME': str(home)}, command) as reference:
                            self.assertEqual(reference, remote + ':' + image['fingerprint'])
                            if fault == 'consumer': raise RuntimeError('consumer failed')
                    if fault == 'none': consume()
                    else:
                        expected = {'certificate-missing': KeyError, 'certificate-malformed': ValueError}.get(fault, (AssertionError, RuntimeError))
                        with self.assertRaises(expected): consume()
                self.assertEqual(remotes, {'local': {'Addr': 'unix://', 'Public': False}})
                self.assertFalse((home / '.config/incus/servercerts' / (remote + '.crt')).exists())
                if fault in ('preexisting', 'listener'):
                    self.assertFalse(any(words[:2] == ['image', 'import'] and '--help' not in words for words in commands))
                elif fault != 'type':
                    self.assertFalse(images)
                    self.assertEqual(listener[0], '')
                if fault.startswith('certificate-'):
                    self.assertFalse((home / '.config').exists())
                    self.assertFalse(any(words[:2] in (['image', 'import'], ['config', 'set'], ['remote', 'add'])
                                         and '--help' not in words for words in commands))

    def test_named_focus_and_button_reads_recover_before_single_physical_input(self):
        spec = importlib.util.spec_from_file_location('gtk_input', ROOT / 'veranda/desktop/wayland-input.py')
        driver = importlib.util.module_from_spec(spec); spec.loader.exec_module(driver)
        class ApiError(Exception): domain, code = 'atspi_error', 1
        atspi = SimpleNamespace(Role=SimpleNamespace(PUSH_BUTTON=43),
            StateType=SimpleNamespace(FOCUSED='focused', SHOWING='showing', DEFUNCT='defunct'))
        for predicate in (driver.focus_matches, driver.focused_button):
            reads, commands, diagnostics = [], [], {}
            def read(end):
                reads.append(True)
                def name():
                    if len(reads) == 1: raise ApiError()
                    return 'Connect'
                return [SimpleNamespace(get_state_set=lambda: SimpleNamespace(contains=lambda state: state!='defunct'),
                    get_process_id=lambda: 7, get_name=name, get_role=lambda: 43)]
            with patch.object(driver.time, 'sleep'):
                matched = driver.coherent_observation(read, lambda items: predicate(items, 7, 'Connect', atspi),
                    driver.time.monotonic() + 20, diagnostics, ApiError, 'activate-focus')
            self.assertTrue(matched)
            commands.append('space')  # Dispatch occurs only after the coherent predicate returns.
            self.assertEqual(commands, ['space'])
            self.assertEqual(len(reads), 2)
            self.assertEqual(diagnostics['read_error_count'], 1)
        wrong_role = SimpleNamespace(get_state_set=lambda: SimpleNamespace(contains=lambda state: state!='defunct'),
            get_process_id=lambda: 7, get_name=lambda: 'Connect', get_role=lambda: 29)
        self.assertFalse(driver.focused_button([wrong_role], 7, 'Connect', atspi))

    def test_coherent_owner_read_recovers_replaced_getter_without_resending_home(self):
        spec = importlib.util.spec_from_file_location('gtk_input', ROOT / 'veranda/desktop/wayland-input.py')
        driver = importlib.util.module_from_spec(spec); spec.loader.exec_module(driver)
        class ApiError(Exception):
            domain, code = 'atspi_error', 1
        state = {'focused': 3, 'replacement': False, 'reads': 0}
        commands, diagnostics = [], {}
        def read(deadline):
            state['reads'] += 1
            def get_parent():
                if state['replacement']:
                    state['replacement'] = False
                    raise ApiError('private payload must never be retained')
                return {'order': ['Local owner', 'Local owner / default', 'Remote owner', 'Remote owner / default'],
                        'owners': [True, False, True, False], 'focused': state['focused']}
            return SimpleNamespace(get_parent=get_parent)
        def send(key):
            commands.append(key)
            state.update(focused=0, replacement=True)
        with patch.object(driver.time, 'sleep'):
            driver.navigate_owner(lambda: driver.coherent_observation(read, lambda row: row.get_parent(),
                driver.time.monotonic() + 20, diagnostics, ApiError, 'navigation'), send, 'Local owner')
        self.assertEqual(commands, ['Home'])
        self.assertEqual(state['reads'], 3)
        self.assertEqual(diagnostics['read_error_count'], 1)
        self.assertNotIn('private payload', json.dumps(diagnostics))

    def test_coherent_owner_read_deadline_and_fatal_boundaries(self):
        spec = importlib.util.spec_from_file_location('gtk_input', ROOT / 'veranda/desktop/wayland-input.py')
        driver = importlib.util.module_from_spec(spec); spec.loader.exec_module(driver)
        class ApiError(Exception):
            def __init__(self, domain='atspi_error', code=1): self.domain, self.code = domain, code
        now, reads, diagnostics = [0.0], [], {}
        def broken(row): reads.append(now[0]); raise ApiError()
        with patch.object(driver.time, 'monotonic', side_effect=lambda: now[0]), \
                patch.object(driver.time, 'sleep', side_effect=lambda seconds: now.__setitem__(0, now[0] + seconds)):
            with self.assertRaises(TimeoutError):
                driver.coherent_observation(lambda end: None, broken, .12, diagnostics, ApiError, 'final-focus')
        self.assertEqual(len(reads), 3)
        self.assertEqual(now[0], .12)
        self.assertEqual(diagnostics['first_read_error']['site'], 'final-focus')
        for error in (ApiError(code=99), ApiError(domain='other'), AssertionError('foreign_accessible_node')):
            reads = []
            def fatal(row): reads.append(True); raise error
            with self.subTest(error=type(error).__name__), self.assertRaises(type(error)) as caught:
                driver.coherent_observation(lambda end: None, fatal, driver.time.monotonic() + 20, {}, ApiError, 'navigation')
            self.assertIs(caught.exception, error)
            self.assertEqual(reads, [True])
        commands = []
        error = ApiError()
        def failed_input(key): commands.append(key); raise error
        with self.assertRaises(ApiError) as caught:
            driver.navigate_owner(lambda: {'order': ['owner', 'yard'], 'owners': [True, False], 'focused': 1}, failed_input, 'owner')
        self.assertIs(caught.exception, error)
        self.assertEqual(commands, ['Home'])

    def test_coherent_diagnostic_error_is_tagged_separately(self):
        spec = importlib.util.spec_from_file_location('gtk_input', ROOT / 'veranda/desktop/wayland-input.py')
        driver = importlib.util.module_from_spec(spec); spec.loader.exec_module(driver)
        class ApiError(Exception):
            domain, code = 'atspi_error', 0
        def stale_state(): raise ApiError('unsafe diagnostic payload')
        reads = iter([[SimpleNamespace(get_state_set=stale_state)], []])
        atspi = SimpleNamespace(StateType=SimpleNamespace(FOCUSED='focused'))
        diagnostics = {}
        with patch.object(driver.time, 'sleep'):
            driver.coherent_observation(lambda end: next(reads),
                lambda items: driver.owner_focus_observation(items, 7, 'owner', atspi),
                driver.time.monotonic() + 20, diagnostics, ApiError, 'diagnostic-focus')
        self.assertEqual(diagnostics['diagnostic_read_error_count'], 1)
        self.assertEqual(diagnostics['last_diagnostic_read_error']['category'], 'diagnostic')
        self.assertGreater(diagnostics['last_diagnostic_read_error']['line'], 0)
        self.assertNotIn('unsafe diagnostic payload', json.dumps(diagnostics))

    def test_postclose_persisted_selection_rejects_wrong_yard_in_every_lifetime(self):
        spec = importlib.util.spec_from_file_location('gtk_short', ROOT / 'veranda/desktop/wayland-short.py')
        short = importlib.util.module_from_spec(spec); spec.loader.exec_module(short)
        with tempfile.TemporaryDirectory(prefix='veranda-postclose-', dir='/tmp') as temporary:
            path=Path(temporary)/'navigation.json'
            for scenario in ('connect','remote-restore','local-restore'):
                selected={'connectionId':None if scenario=='remote-restore' else 'saved-owner',
                          'yardName':None if scenario=='local-restore' else 'default',
                          'tab':'Overview' if scenario=='local-restore' else 'Projects'}
                path.write_text(json.dumps(selected));path.chmod(0o600)
                short.verify_persisted_selection(json.loads(path.read_text()),'saved-owner',scenario)
                for field,bad in (('yardName','fleet-19'),('connectionId','unrelated-owner'),('tab','Settings')):
                    invalid=dict(selected);invalid[field]=bad;path.write_text(json.dumps(invalid))
                    with self.subTest(scenario=scenario,field=field),self.assertRaises(AssertionError):
                        short.verify_persisted_selection(json.loads(path.read_text()),'saved-owner',scenario)

    def test_owner_navigation_reaches_predecessor_without_forward_tab_cycle(self):
        spec = importlib.util.spec_from_file_location('gtk_input', ROOT / 'veranda/desktop/wayland-input.py')
        driver = importlib.util.module_from_spec(spec); spec.loader.exec_module(driver)
        order = ['Local owner', 'Local owner / default', 'Remote owner', 'Remote owner / default']
        for target in ('Local owner', 'Remote owner'):
            state = {'order': order, 'owners': [True, False, True, False], 'focused': None}
            commands = []
            def send(key):
                commands.append(key)
                if key == 'Tab': state['focused'] = 3 if state['focused'] is None else None
                elif key == 'Home': state['focused'] = 0
                elif key == 'Down': state['focused'] += 1
            driver.navigate_owner(lambda: dict(state), send, target)
            self.assertIn('Home', commands)
            self.assertEqual(state['focused'], order.index(target))
            self.assertLessEqual(len(commands), 64)
        for fault in ('reorder', 'stale-focus'):
            state = {'order': order, 'owners': [True, False, True, False], 'focused': 3}
            commands = []
            def send(key):
                commands.append(key)
                if fault == 'reorder': state['order'] = list(reversed(order))
                else: state['focused'] = 3
            with self.subTest(fault=fault), self.assertRaises(AssertionError):
                driver.navigate_owner(lambda: dict(state), send, 'Remote owner')
            self.assertEqual(commands, ['Home'])

    def test_named_control_exits_navigation_without_changing_selected_yard(self):
        spec = importlib.util.spec_from_file_location('gtk_input', ROOT / 'veranda/desktop/wayland-input.py')
        driver = importlib.util.module_from_spec(spec); spec.loader.exec_module(driver)
        for fault in ('none', 'f6-stays', 'f6-changes-selection', 'tab-enters-navigation'):
            state = {'order': ['owner', 'owner / default', 'owner / fleet-19'],
                     'owners': [True, False, False], 'focused': 1, 'scope': 'owner / default',
                     'heading': True, 'matched': False}
            commands = []
            def send(key):
                commands.append(key)
                if key == 'F6':
                    if fault != 'f6-stays': state['focused'] = None
                    if fault == 'f6-changes-selection': state['scope'] = 'owner / fleet-19'
                else:
                    if state['focused'] is not None:
                        state['scope'] = 'owner / fleet-19'  # Actual R22 forward-Tab hazard.
                    if fault == 'tab-enters-navigation': state['focused'] = 2
                    else: state['matched'] = True
            if fault == 'none':
                driver.focus_control(lambda: dict(state), send, 'Projects')
                self.assertEqual(state['scope'], 'owner / default')
                self.assertEqual(commands, ['F6', 'Tab'])
            else:
                with self.subTest(fault=fault), self.assertRaises(AssertionError):
                    driver.focus_control(lambda: dict(state), send, 'Projects')
                self.assertEqual(commands[0], 'F6')
                self.assertLessEqual(len(commands), 2)

    def test_control_focus_retries_read_without_redispatching_f6(self):
        spec = importlib.util.spec_from_file_location('gtk_input', ROOT / 'veranda/desktop/wayland-input.py')
        driver = importlib.util.module_from_spec(spec); spec.loader.exec_module(driver)
        class ApiError(Exception): domain, code = 'atspi_error', 1
        state = {'order': ['owner', 'owner / default'], 'owners': [True, False],
                 'focused': 1, 'scope': 'owner / default', 'heading': True, 'matched': False}
        calls, reads, diagnostics = [], [], {}
        def observe(items):
            reads.append(True)
            if len(reads) == 2: raise ApiError()
            return dict(state)
        def send(key):
            calls.append(key)
            if key == 'F6': state['focused'] = None
            else: state['matched'] = True
        with patch.object(driver.time, 'sleep'):
            driver.focus_control(lambda: driver.coherent_observation(lambda end: None, observe,
                driver.time.monotonic()+20, diagnostics, ApiError, 'named-focus'), send, 'Projects')
        self.assertEqual(calls, ['F6', 'Tab'])
        self.assertEqual(diagnostics['read_error_count'], 1)

    def test_remote_panel_controls_do_not_read_hidden_workspace_heading(self):
        spec = importlib.util.spec_from_file_location('gtk_input', ROOT / 'veranda/desktop/wayland-input.py')
        driver = importlib.util.module_from_spec(spec); spec.loader.exec_module(driver)
        atspi = SimpleNamespace(Role=SimpleNamespace(PUSH_BUTTON=43),
            StateType=SimpleNamespace(SHOWING='showing', FOCUSED='focused', DEFUNCT='defunct'))
        activate = next(node for node in ast.walk(ast.parse((ROOT / 'veranda/desktop/wayland-input.py').read_text()))
                        if isinstance(node, ast.FunctionDef) and node.name == 'activate')
        def button(name, pid=7, role=43, showing=True):
            return SimpleNamespace(get_process_id=lambda: pid, get_role=lambda: role, get_name=lambda: name,
                get_state_set=lambda: SimpleNamespace(contains=lambda state: state=='focused' or (state=='showing' and showing)))
        for name in ('Connect remote host', 'Connect', 'Cancel', 'Accept and connect'):
            commands=[];workspace_reads=[];node=button(name)
            def hidden_workspace():
                workspace_reads.append(True)
                return {'order':['Local owner'], 'owners':[True], 'focused':None,
                        'scope':'Local owner', 'heading':False, 'matched':True}
            driver.focus_target(lambda:driver.focused_button([node],7,name,atspi),
                hidden_workspace,commands.append,name)
            self.assertEqual(commands, [])  # No F6, no workspace heading requirement.
            self.assertEqual(workspace_reads, [])
            for nodes in ([button(name,pid=8)], [button(name,role=29)], [button(name,showing=False)], [node,button(name)]):
                namespace={'focus':lambda *args,**kwargs:None, 'nodes':lambda observe,site:observe(nodes),
                           'pid':7, 'Atspi':atspi, 'focused_button':driver.focused_button,
                           'keys':lambda *args:commands.append(args)}
                exec(compile(ast.Module(body=[activate],type_ignores=[]),'actual-activate','exec'),namespace)
                with self.assertRaises(AssertionError): namespace['activate'](name)
                self.assertEqual(commands, [])
            with self.assertRaises(AssertionError):
                driver.focus_target(lambda:True,hidden_workspace,commands.append,name,workspace=True)

    def test_review_decline_preserves_scope_after_navigator_button_acquisition(self):
        spec = importlib.util.spec_from_file_location('gtk_input', ROOT / 'veranda/desktop/wayland-input.py')
        driver = importlib.util.module_from_spec(spec); spec.loader.exec_module(driver)
        state={'scope':'Local owner / default','heading':True,'button_focused':False}
        calls=[]
        def acquire():
            calls.append('acquire')
            state.update(scope='Local owner / fleet-19',button_focused=True)
        def read_entry():calls.append('observe');return dict(state)
        entry=driver.open_review(acquire,read_entry,lambda:calls.append('space'))
        self.assertEqual(calls,['acquire','observe','space'])
        self.assertEqual(entry['scope'],'Local owner / fleet-19')
        self.assertTrue(driver.entry_scope_preserved(dict(state),entry))
        # All sidebar labels can still exist after an unintended selection change.
        state['sidebar']=['Local owner / default','Local owner / fleet-19']
        state['scope']='Local owner / default'
        self.assertFalse(driver.entry_scope_preserved(state,entry))
        for fault in ('heading','button_focused'):
            invalid=dict(entry);invalid[fault]=False;sent=[]
            with self.assertRaises(AssertionError):driver.open_review(lambda:None,lambda:invalid,lambda:sent.append('space'))
            self.assertEqual(sent,[])

    def test_selected_scope_requires_navigation_selection_and_workspace_heading(self):
        spec = importlib.util.spec_from_file_location('gtk_input', ROOT / 'veranda/desktop/wayland-input.py')
        driver = importlib.util.module_from_spec(spec); spec.loader.exec_module(driver)
        atspi = SimpleNamespace(Role=SimpleNamespace(LIST=31, LIST_ITEM=32, LABEL=29),
            StateType=SimpleNamespace(SHOWING='showing', FOCUSED='focused', DEFUNCT='defunct', SELECTED='selected'))
        class Node:
            def __init__(self, role, name='', states=('showing',)):
                self.role,self.name,self.states,self.children,self.parent=role,name,set(states),[],None
            def get_role(self): return self.role
            def get_name(self): return self.name
            def get_process_id(self): return 7
            def get_parent(self): return self.parent
            def get_child_count(self): return len(self.children)
            def get_child_at_index(self,index): return self.children[index]
            def get_state_set(self): return SimpleNamespace(contains=lambda state:state in self.states)
        nav=Node(31,'Owner hosts and yards');workspace=Node(39)
        rows=[Node(32,'owner / default'),Node(32,'owner / fleet-19')]
        labels=[Node(29,'default'),Node(29,'fleet-19')]
        for row,label in zip(rows,labels):row.parent=nav;row.children=[label];label.parent=row
        nav.children=rows;rows[1].states.add('selected')
        heading=Node(29,'owner / fleet-19');heading.parent=workspace
        button=Node(37,'Projects',('showing','focused'));button.parent=workspace
        items=[nav,workspace,heading,button,*rows,*labels]
        self.assertIn('owner / default', {node.get_name() for node in items})
        self.assertFalse(driver.selected_scope(driver.navigation_context(items,7,atspi),'owner / default'))
        rows[1].states.remove('selected');rows[0].states.add('selected');heading.name='owner / default'
        self.assertTrue(driver.selected_scope(driver.navigation_context(items,7,atspi),'owner / default'))
        heading.name='wrong context'
        self.assertFalse(driver.selected_scope(driver.navigation_context(items,7,atspi),'owner / default'))
        rows[1].states.add('selected')
        with self.assertRaises(AssertionError):driver.navigation_context(items,7,atspi)

    def test_navigation_snapshot_rejects_unowned_or_ambiguous_lists_before_input(self):
        spec = importlib.util.spec_from_file_location('gtk_input', ROOT / 'veranda/desktop/wayland-input.py')
        driver = importlib.util.module_from_spec(spec); spec.loader.exec_module(driver)
        atspi = SimpleNamespace(Role=SimpleNamespace(LIST=31, LIST_ITEM=32, LABEL=29),
                                StateType=SimpleNamespace(SHOWING='showing', FOCUSED='focused', DEFUNCT='defunct'))
        class Node:
            def __init__(self, role, name='', states=('showing',)):
                self.role, self.name, self.states, self.pid = role, name, set(states), 7
                self.parent, self.children = None, []
            def get_role(self): return self.role
            def get_name(self): return self.name
            def get_process_id(self): return self.pid
            def get_parent(self): return self.parent
            def get_child_count(self): return len(self.children)
            def get_child_at_index(self, index): return self.children[index]
            def get_state_set(self): return SimpleNamespace(contains=lambda state: state in self.states)
        for fault in ('valid', 'list-role', 'list-name', 'list-pid', 'row-pid', 'row-parent',
                      'label-parent', 'hidden', 'defunct', 'duplicate-list', 'duplicate-row', 'multiple-focus'):
            nav = Node(31, 'Owner hosts and yards'); row = Node(32, states=('showing', 'focused'))
            label = Node(29, 'Local owner'); nav.children = [row]; row.parent = nav
            row.children = [label]; label.parent = row; items = [nav, row, label]
            if fault == 'list-role': nav.role = 39
            elif fault == 'list-name': nav.name = 'Other list'
            elif fault == 'list-pid': nav.pid = 8
            elif fault == 'row-pid': row.pid = 8
            elif fault == 'row-parent': row.parent = Node(31)
            elif fault == 'label-parent': label.parent = nav
            elif fault == 'hidden': row.states.discard('showing')
            elif fault == 'defunct': row.states.add('defunct')
            elif fault == 'duplicate-list': items.append(Node(31, nav.name))
            elif fault == 'duplicate-row': nav.children.append(row)
            elif fault == 'multiple-focus': label.states.add('focused')
            commands = []
            with self.subTest(fault=fault):
                if fault == 'valid':
                    self.assertEqual(driver.navigation_snapshot(items, 7, atspi)['focused'], 0)
                else:
                    with self.assertRaises(AssertionError):
                        driver.navigate_owner(lambda: driver.navigation_snapshot(items, 7, atspi), commands.append, 'Local owner')
                    self.assertEqual(commands, [])

    def test_owner_focus_mode_is_one_local_lifetime(self):
        spec = importlib.util.spec_from_file_location('gtk_short', ROOT / 'veranda/desktop/wayland-short.py')
        short = importlib.util.module_from_spec(spec); spec.loader.exec_module(short)
        self.assertEqual(short.short_scenarios(False, True), ['owner-focus'])
        self.assertEqual(short.short_scenarios(False, False), ['local'])
        self.assertEqual(short.short_scenarios(True, False), ['connect', 'remote-restore', 'local-restore'])
        with self.assertRaises(ValueError): short.short_scenarios(True, True)

    def test_owner_focus_uses_unique_visible_owned_row_child_relation(self):
        spec = importlib.util.spec_from_file_location('gtk_input', ROOT / 'veranda/desktop/wayland-input.py')
        driver = importlib.util.module_from_spec(spec); spec.loader.exec_module(driver)
        atspi = SimpleNamespace(Role=SimpleNamespace(LIST_ITEM=32, LABEL=29, PUSH_BUTTON=43),
                                StateType=SimpleNamespace(SHOWING='showing', FOCUSED='focused', DEFUNCT='defunct'))
        class Node:
            def __init__(self, role, name='', states=('showing',), pid=7):
                self.role, self.name, self.states, self.pid = role, name, set(states), pid
                self.parent, self.children = None, []
            def get_role(self): return self.role
            def get_name(self): return self.name
            def get_process_id(self): return self.pid
            def get_parent(self): return self.parent
            def get_child_count(self): return len(self.children)
            def get_child_at_index(self, index): return self.children[index]
            def get_state_set(self): return SimpleNamespace(contains=lambda state: state in self.states)
        def pair():
            row = Node(32, states=('showing', 'focused')); label = Node(29, 'remote')
            row.children = [label]; label.parent = row
            return row, label
        row, label = pair()
        self.assertTrue(driver.focus_matches([row, label], 7, 'remote', atspi, owner_row=True))
        observation = driver.owner_focus_observation([row, label], 7, 'remote', atspi)
        self.assertEqual(observation['reason'], 'matched')
        relation = observation['owner_rows'][0]
        self.assertTrue(relation['parent_equals_row'])
        self.assertTrue(relation['parent']['in_snapshot'])
        self.assertEqual(relation['parent']['wrapper_id'], relation['row']['wrapper_id'])
        row.states.discard('focused')
        button = Node(43, 'Refresh', ('showing', 'focused'))
        observation = driver.owner_focus_observation([row, label, button], 7, 'remote', atspi)
        self.assertEqual(observation['reason'], 'focused_not_owner_candidate')
        self.assertEqual(observation['focused'][0]['name'], 'Refresh')
        row.states.add('focused')
        self.assertFalse(driver.focus_matches([row, label], 7, 'remote', atspi))
        class Proxy:
            def __init__(self, node): self.node = node
            def __getattr__(self, name): return getattr(self.node, name)
            def __eq__(self, other): return self.node == getattr(other, 'node', other)
            def __hash__(self): return hash(self.node)
        # Separate equal proxy wrappers must preserve the actual parent/child link.
        label.parent = Proxy(row); row.children = [Proxy(label)]
        self.assertTrue(driver.focus_matches([row, label], 7, 'remote', atspi, owner_row=True))
        observation = driver.owner_focus_observation([row, label], 7, 'remote', atspi)
        self.assertEqual(observation['reason'], 'matched')
        relation = observation['owner_rows'][0]
        self.assertNotEqual(relation['parent']['wrapper_id'], relation['row']['wrapper_id'])
        self.assertEqual(relation['parent']['accessible_hash'], relation['row']['accessible_hash'])
        for case in ('container', 'child-role', 'row-pid', 'child-pid', 'parent', 'hidden-child',
                     'hidden-row', 'stale-child', 'stale-row', 'missing-child', 'missing-row', 'duplicate-child', 'duplicate-row'):
            row, label = pair(); items = [row, label]
            if case == 'container': row.role = 39
            elif case == 'child-role': label.role = 43
            elif case == 'row-pid': row.pid = 8
            elif case == 'child-pid': label.pid = 8
            elif case == 'parent': label.parent = Node(32)
            elif case == 'hidden-child': label.states.clear()
            elif case == 'hidden-row': row.states.discard('showing')
            elif case == 'stale-child': label.states.add('defunct')
            elif case == 'stale-row': row.states.add('defunct')
            elif case == 'missing-child': items.remove(label)
            elif case == 'missing-row': items.remove(row)
            elif case == 'duplicate-child':
                other = Node(29, 'remote'); other.parent = row; row.children.append(other); items.append(other)
            elif case == 'duplicate-row': items.extend(pair())
            with self.subTest(case=case):
                self.assertFalse(driver.focus_matches(items, 7, 'remote', atspi, owner_row=True))
                observation = driver.owner_focus_observation(items, 7, 'remote', atspi)
                self.assertNotEqual(observation['reason'], 'matched')
                self.assertFalse(observation['predicate_result'])
        button = Node(43, 'Connect', ('showing', 'focused'))
        self.assertTrue(driver.focus_matches([button], 7, 'Connect', atspi))
        self.assertFalse(driver.focus_matches([button], 7, 'Connect', atspi, owner_row=True))
        self.assertFalse(driver.focus_matches([button], 7, 'Cancel', atspi))
        button.pid = 8
        self.assertFalse(driver.focus_matches([button], 7, 'Connect', atspi))

    def test_cancel_flow_uses_visible_text_control_and_failed_lifetime_retains_history(self):
        def load(name, path):
            spec = importlib.util.spec_from_file_location(name, ROOT / path)
            value = importlib.util.module_from_spec(spec); spec.loader.exec_module(value)
            return value
        driver = load('gtk_input', 'veranda/desktop/wayland-input.py')
        short = load('gtk_short', 'veranda/desktop/wayland-short.py')
        atspi = SimpleNamespace(Role=SimpleNamespace(TEXT=61), StateType=SimpleNamespace(SHOWING='showing'))
        def node(showing):
            return SimpleNamespace(get_role=lambda: 61, get_name=lambda: 'Remote SSH destination',
                                   get_state_set=lambda: SimpleNamespace(contains=lambda state: showing))
        self.assertFalse(driver.cancellation_flow_closed([node(True)], atspi))
        self.assertTrue(driver.cancellation_flow_closed([node(False)], atspi))
        history = [{'pid': 12, 'start_ticks': 34, 'alive_at_end': True}]
        tree = SimpleNamespace(sample=lambda: {'processes': history}, process_history=lambda: history)
        lifetime = {'input_exit_code': 1}
        short.retain_lifetime_snapshot(lifetime, tree, SimpleNamespace(poll=lambda: None))
        self.assertEqual(lifetime['input_exit_code'], 1)
        self.assertEqual(lifetime['observed_process_history'], history)
        self.assertIsNone(lifetime['gui_exit_at_snapshot'])
        def failed(): raise OSError('snapshot failed')
        short.retain_lifetime_snapshot(lifetime, SimpleNamespace(sample=failed), SimpleNamespace(poll=lambda: None))
        self.assertEqual(lifetime['process_snapshot_failure_type'], 'OSError')
        self.assertEqual(lifetime['input_exit_code'], 1)

    def test_manual_block_uses_owned_proc_socket_and_private_nonce_receipt(self):
        spec = importlib.util.spec_from_file_location('ssh_fixture', ROOT / 'veranda/desktop/ssh-fixture.py')
        helper = importlib.util.module_from_spec(spec); spec.loader.exec_module(helper)
        with tempfile.TemporaryDirectory(prefix='veranda-ssh-attribution-', dir='/tmp') as temporary:
            root = Path(temporary); root.chmod(0o700)
            proc = root / 'proc'; (proc / 'net').mkdir(parents=True)
            binary = root / 'ssh'; binary.write_bytes(b'fixture only'); binary.chmod(0o700)
            alias = 'veranda-' + 'b' * 48
            for pid, parent, start in ((7, 1, 10), (9, 7, 12), (10, 99, 13)):
                path = proc / str(pid); (path / 'fd').mkdir(parents=True)
                fields = ['0'] * 22; fields[0] = 'S'; fields[1] = str(parent); fields[19] = str(start)
                (path / 'stat').write_text(str(pid) + ' (fixture) ' + ' '.join(fields))
                (path / 'exe').symlink_to(binary)
                (path / 'cmdline').write_bytes('\0'.join(['ssh', '-o', 'HostKeyAlias=' + alias, '--', '127.0.0.1', 'yard rpc --stdio', '']).encode())
                (path / 'fd/3').symlink_to('socket:[42]')
            (proc / 'net/tcp').write_text('header\n 0: 0100007F:ABE0 0100007F:AAFB 01 0:0 00:0 0 1000 0 42\n')
            gui = {'pid': 7, 'start': 10}; clients = helper.owned_ssh(gui, proc)
            self.assertEqual([row['pid'] for row in clients], [9])
            self.assertEqual(clients[0]['peers'], [['127.0.0.1', 44000, '127.0.0.1', 43771]])
            with self.assertRaises(AssertionError): helper.owned_ssh({'pid': 7, 'start': 99}, proc)
            arm = {'nonce': 'a' * 32, 'gui': gui, 'port': 43771, 'baseline': []}
            helper.private_json(root / 'block', arm)
            stored = dict(clients[0], alias='veranda-stored-' + 'b' * 48)
            with patch.object(helper, 'owned_ssh', return_value=[stored]):
                self.assertFalse(helper.maybe_block(root, helper.HOST_RPC, '127.0.0.1 44000 127.0.0.1 43771'))
            self.assertFalse((root / ('blocked-' + arm['nonce'] + '.json')).exists())
            with patch.object(helper, 'owned_ssh', return_value=clients):
                self.assertTrue(helper.maybe_block(root, helper.HOST_RPC, '127.0.0.1 44000 127.0.0.1 43771'))
            marker = root / ('blocked-' + arm['nonce'] + '.json')
            self.assertEqual(marker.stat().st_mode & 0o777, 0o600)
            receipt = helper.read_private_json(marker)
            self.assertEqual(helper.verify_block(arm, receipt, clients), clients[0])
            marker.chmod(0o644)
            with self.assertRaises(AssertionError): helper.read_private_json(marker)

    def test_captured_ssh_routes_and_cancel_oracle(self):
        spec = importlib.util.spec_from_file_location('ssh_fixture', ROOT / 'veranda/desktop/ssh-fixture.py')
        helper = importlib.util.module_from_spec(spec); spec.loader.exec_module(helper)
        yards = ['default', 'fleet-01', 'fleet-19']
        for argv in (['yard', 'rpc', '--stdio'], *(['yard', '-Y', yard, 'rpc', '--stdio'] for yard in yards)):
            with self.subTest(argv=argv): self.assertTrue(helper.allowed_rpc(argv, yards))
        for argv in (['yard', '-Y', 'fleet-02', 'rpc', '--stdio'], ['yard', '-Y', '../fleet-01', 'rpc', '--stdio'],
                     ['yard', 'rpc', '--stdio', 'extra'], ['yard', 'shell'], ['sh', '-c', 'yard rpc --stdio']):
            with self.subTest(argv=argv): self.assertFalse(helper.allowed_rpc(argv, yards))
        self.assertTrue(helper.cancel_complete(True, True, False))
        for values in ((False, True, False), (True, False, False), (True, True, True)):
            self.assertFalse(helper.cancel_complete(*values))

    def test_manual_ssh_attribution_rejects_background_stale_and_ambiguous_receipts(self):
        spec = importlib.util.spec_from_file_location('ssh_fixture', ROOT / 'veranda/desktop/ssh-fixture.py')
        helper = importlib.util.module_from_spec(spec); spec.loader.exec_module(helper)
        arm = {'nonce': 'a' * 32, 'gui': {'pid': 7, 'start': 10}, 'port': 43771, 'baseline': [[8, 11]]}
        peer = ['127.0.0.1', 44000, '127.0.0.1', 43771]
        client = {'pid': 9, 'start': 12, 'parent': 7, 'uid': 1000, 'alias': 'veranda-' + 'b' * 48,
                  'route': ['yard', 'rpc', '--stdio'], 'peers': [peer]}
        receipt = {'nonce': arm['nonce'], 'gui': arm['gui'], 'peer': peer, 'client': client, 'route': client['route']}
        self.assertEqual(helper.verify_block(arm, receipt, [client]), client)
        for changed in (dict(client, pid=8, start=11), dict(client, alias='veranda-stored-' + 'b' * 48),
                        dict(client, route=['yard', '-Y', 'fleet-01', 'rpc', '--stdio']), dict(client, peers=[])):
            with self.subTest(client=changed): self.assertIsNone(helper.match_manual(arm, peer, [changed]))
        for changed in (dict(receipt, nonce='c' * 32), dict(receipt, gui={'pid': 7, 'start': 99}),
                        dict(receipt, client=dict(client, start=99))):
            with self.subTest(receipt=changed), self.assertRaises(AssertionError):
                helper.verify_block(arm, changed, [client])
        with self.assertRaises(AssertionError): helper.match_manual(arm, peer, [client, dict(client, pid=10)])
        self.assertEqual(helper.peer_tuple('127.0.0.1 44000 127.0.0.1 43771'), peer)
        for invalid in ('10.0.0.1 44000 127.0.0.1 43771', '127.0.0.1 0 127.0.0.1 43771'):
            with self.subTest(peer=invalid), self.assertRaises(AssertionError): helper.peer_tuple(invalid)

    def test_connection_review_distinguishes_explicit_first_key_consent_from_known_key(self):
        spec = importlib.util.spec_from_file_location('gtk_input', ROOT / 'veranda/desktop/wayland-input.py')
        driver = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(driver)
        destination, fingerprint = 'fixture@127.0.0.1:43771', 'SHA256:fixture'
        first = destination + '\nFingerprint: ' + fingerprint + '\nVerify the fingerprint independently before accepting.'
        known = destination + '\nPrevious fingerprint: ' + fingerprint + '\nFingerprint: ' + fingerprint + '\nConnect using the previously accepted SSH host key.'
        self.assertTrue(driver.review_ready({first, 'Accept and connect'}, destination, fingerprint))
        self.assertTrue(driver.review_ready({known, 'Connect'}, destination, fingerprint, known_key=True))
        for names, trusted in (({known, 'Connect'}, False), ({first, 'Accept and connect'}, True),
                               ({first.replace('Verify', 'Ignore'), 'Accept and connect'}, False),
                               ({first, 'Connect'}, False), ({known, 'Accept and connect'}, True),
                               ({known.replace('Previous fingerprint: ' + fingerprint, 'Previous fingerprint: SHA256:other'), 'Connect'}, True)):
            with self.subTest(names=names, trusted=trusted):
                self.assertFalse(driver.review_ready(names, destination, fingerprint, known_key=trusted))
        for detail, button, trusted in ((first, 'Accept and connect', False), (known, 'Connect', True)):
            for wrong in (detail.replace(destination, 'other@127.0.0.1:43771'),
                          detail.replace('Fingerprint: ' + fingerprint, 'Fingerprint: SHA256:other'),
                          'prefix' + detail):
                with self.subTest(detail=wrong, trusted=trusted):
                    self.assertFalse(driver.review_ready({wrong, button}, destination, fingerprint, known_key=trusted))

    def test_ssh_preflight_uses_the_owner_rpc_suffix_exactly_once(self):
        def load(name, path):
            spec = importlib.util.spec_from_file_location(name, ROOT / path)
            module = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(module)
            return module
        short = load('gtk_short', 'veranda/desktop/wayland-short.py')
        owner = load('gtk_owner', 'dev/e2e/veranda-owner.py')
        with patch.object(owner.subprocess, 'Popen') as launch, patch.object(owner.threading.Thread, 'start'):
            owner.Owner(short.ssh_owner_argv(Path('/owned'), '43771', 'fixture'), env={})
        argv = launch.call_args.args[0]
        self.assertEqual(shlex.split(' '.join(argv[argv.index('--') + 2:])), ['yard', 'rpc', '--stdio'])

    def test_x11_readiness_requires_live_owned_services_and_actual_ewmh_before_gui(self):
        spec = importlib.util.spec_from_file_location('gtk_short', ROOT / 'veranda/desktop/wayland-short.py')
        short = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(short)
        live = SimpleNamespace(poll=lambda: None)
        dead = SimpleNamespace(poll=lambda: 1)
        for scenario in ('ready', 'display-dead', 'manager-dead', 'timeout', 'ewmh-unready'):
            now = [0.0]
            def advance(seconds): now[0] += seconds
            replies = [SimpleNamespace(returncode=0, stdout='1280 800\n', stderr=''),
                       SimpleNamespace(returncode=0, stdout='4\n', stderr='')]
            if scenario == 'ewmh-unready':
                replies = replies[:1] + [SimpleNamespace(returncode=0, stdout='0\n', stderr='')] * 2
            with self.subTest(scenario=scenario), \
                    patch.object(short.fixture, 'private_log', side_effect=lambda path: contextlib.nullcontext(io.StringIO())), \
                    patch.object(short.time, 'monotonic', side_effect=lambda: now[0]), \
                    patch.object(short.time, 'sleep', side_effect=advance), \
                    patch.object(short.subprocess, 'run', side_effect=(subprocess.TimeoutExpired('xdotool', .1)
                                 if scenario == 'timeout' else replies)) as query, \
                    patch.object(short.subprocess, 'Popen') as gui:
                def launch():
                    short.wait_x11_ready(dead if scenario == 'display-dead' else live, None, {}, .1, Path('display.log'))
                    short.wait_x11_ready(live, dead if scenario == 'manager-dead' else live, {}, .1, Path('wm.log'))
                    gui(['gui'])
                if scenario == 'ready':
                    launch()
                    gui.assert_called_once_with(['gui'])
                    self.assertEqual([call.args[0] for call in query.call_args_list],
                                     [['xdotool', 'getdisplaygeometry'], ['xdotool', 'get_num_desktops']])
                else:
                    with self.assertRaises((AssertionError, TimeoutError)):
                        launch()
                    gui.assert_not_called()
                    if scenario == 'display-dead': query.assert_not_called()
                    if scenario == 'manager-dead': self.assertEqual(query.call_count, 1)
                    if scenario in ('timeout', 'ewmh-unready'): self.assertEqual(now[0], .1)

    def test_default_input_requires_the_actual_body_and_expected_typed_focus(self):
        spec = importlib.util.spec_from_file_location('gtk_input', ROOT / 'veranda/desktop/wayland-input.py')
        driver = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(driver)
        atspi = SimpleNamespace(Role=SimpleNamespace(LABEL=29, LIST_ITEM=32, PUSH_BUTTON=43, PAGE_TAB=37),
                                StateType=SimpleNamespace(SHOWING='showing', SELECTED='selected', FOCUSED='focused'))
        def node(role, name, *states):
            return SimpleNamespace(get_role=lambda: role, get_name=lambda: name,
                                   get_state_set=lambda: SimpleNamespace(contains=lambda state: state in states))
        selected = node(32, 'Local owner / default', 'showing', 'selected')
        body = node(29, 'Local owner / default', 'showing')
        summary = 'Yard state: NOT_CREATED\n10 projects\nObserved: 2026-10-18T08:47:03Z'
        for role, name, physical_key in ((43, 'Refresh', 'space'), (37, 'Projects', 'space'),
                                         (29, summary, 'shift+Tab')):
            correct = node(role, name, 'showing', 'focused')
            for items in ([selected, node(29, 'Local owner / fleet-01', 'showing'), correct],
                          [selected, body, node(43, 'Reconcile', 'showing', 'focused')],
                          [selected, body, node(43 if role == 29 else 29, name, 'showing', 'focused')],
                          [selected, body, node(role, name + ' wrong', 'showing', 'focused')]):
                with self.subTest(role=role, name=name, items=items), \
                        patch.object(driver.time, 'monotonic', return_value=0), \
                        patch.object(driver, 'x11_key') as key:
                    with self.assertRaises(AssertionError):
                        driver.default_input(items, atspi, (role, name), 1, lambda: key(77, physical_key))
                    key.assert_not_called()
            with patch.object(driver.time, 'monotonic', return_value=0), \
                    patch.object(driver, 'x11_key') as key:
                driver.default_input([selected, body, correct], atspi, (role, name), 1, lambda: key(77, physical_key))
                key.assert_called_once_with(77, physical_key)

    def test_whole_accessible_tree_retry_never_authorizes_partial_or_unowned_input(self):
        spec = importlib.util.spec_from_file_location('gtk_input', ROOT / 'veranda/desktop/wayland-input.py')
        driver = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(driver)
        class Node:
            def __init__(self, children=(), pid=77, count=None):
                self.children, self.pid, self.count = children, pid, count
            def get_process_id(self): return self.pid
            def get_state_set(self): return SimpleNamespace(contains=lambda state: False)
            def get_child_count(self): return len(self.children) if self.count is None else self.count
            def get_child_at_index(self, index): return self.children[index]
        for broken in (Node(count=-1), Node([None]), Node([Node(count=-1)])):
            with self.subTest(broken=broken), patch.object(driver.time, 'monotonic', return_value=0), \
                    patch.object(driver.time, 'sleep'), patch.object(driver, 'x11_key') as key:
                trees = iter((broken, Node([Node()])))
                diagnostics = {}
                items = driver.complete_tree(lambda deadline: driver.owned_tree(next(trees), 77, 'defunct', deadline), 1, diagnostics)
                self.assertEqual(len(items), 2)
                key(77, 'space')
                key.assert_called_once_with(77, 'space')
                self.assertEqual(diagnostics['incomplete_tree_retries'], 1)
        now = [0.0]
        def advance(seconds): now[0] += seconds
        with patch.object(driver.time, 'monotonic', side_effect=lambda: now[0]), \
                patch.object(driver.time, 'sleep', side_effect=advance), patch.object(driver, 'x11_key') as key:
            diagnostics = {}
            with self.assertRaises(TimeoutError):
                driver.complete_tree(lambda deadline: driver.owned_tree(Node(count=-1), 77, 'defunct', deadline), .1, diagnostics)
                key(77, 'space')
            key.assert_not_called()
            self.assertEqual(now[0], .1)
            self.assertEqual(diagnostics['incomplete_tree_retries'], 2)
        deep = Node()
        for _ in range(33): deep = Node([deep])
        large = Node([Node([Node() for _ in range(512)]) for _ in range(4)])
        for tree, reason in ((Node(pid=88), 'foreign_accessible_node'),
                             (Node(count=513), 'accessible_child_limit_exceeded'),
                             (deep, 'accessible_depth_exceeded'),
                             (large, 'accessible_node_limit_exceeded')):
            with self.subTest(reason=reason), patch.object(driver.time, 'monotonic', return_value=0), \
                    patch.object(driver.time, 'sleep') as retry, patch.object(driver, 'x11_key') as key:
                with self.assertRaisesRegex(AssertionError, reason):
                    driver.complete_tree(lambda deadline: driver.owned_tree(tree, 77, 'defunct', deadline), 1, {})
                    key(77, 'space')
                retry.assert_not_called()
                key.assert_not_called()

    def test_failed_diagnostics_preserve_original_failure_and_remove_partial_image(self):
        spec = importlib.util.spec_from_file_location('gtk_input', ROOT / 'veranda/desktop/wayland-input.py')
        driver = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(driver)
        original = {'original_exit_code': 1, 'failure_type': 'TimeoutError', 'failure_stage': 'local-connected'}
        result = dict(original)
        def failed():
            raise subprocess.TimeoutExpired(['diagnostic'], 5)
        driver.retain_failure_diagnostics(result, failed)
        self.assertEqual({key: result[key] for key in original}, original)
        self.assertEqual(result['diagnostic_failure_type'], 'TimeoutExpired')
        with tempfile.TemporaryDirectory(prefix='veranda-diagnostic-', dir='/tmp') as temporary:
            capture = Path(temporary) / 'failure.png'
            with patch.dict(os.environ, DISPLAY=':99'), \
                    patch.object(driver.subprocess, 'run', side_effect=subprocess.TimeoutExpired(['capture'], 5)):
                with self.assertRaises(subprocess.TimeoutExpired):
                    driver.capture_view('x11', capture)
            self.assertFalse(capture.exists())

    def test_x11_physical_input_refuses_foreign_or_ambiguous_windows(self):
        spec = importlib.util.spec_from_file_location('gtk_input', ROOT / 'veranda/desktop/wayland-input.py')
        driver = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(driver)
        for windows, owner in ((b'12\n13\n', b'77\n'), (b'12\n', b'88\n')):
            with self.subTest(windows=windows, owner=owner), \
                    patch.object(driver.subprocess, 'check_output', side_effect=[windows, owner]), \
                    patch.object(driver.subprocess, 'run') as physical:
                with self.assertRaises(AssertionError):
                    driver.x11_key(77, 'alt+r')
                physical.assert_not_called()
        with patch.object(driver.subprocess, 'check_output') as windows, \
                patch.object(driver.subprocess, 'run') as physical:
            with self.assertRaises(ValueError):
                driver.x11_key(77, 'Delete')
            windows.assert_not_called()
            physical.assert_not_called()
        with patch.object(driver.subprocess, 'check_output', side_effect=[b'12\n', b'77\n', b'12\n', b'77\n']), \
                patch.object(driver.subprocess, 'run') as physical:
            driver.x11_key(77, 'F6')
            self.assertEqual(physical.call_args_list[-1].args[0],
                             ['xdotool', 'key', '--clearmodifiers', 'F6'])

    def test_x11_translation_preserves_shortcuts_text_and_per_event_guards(self):
        spec = importlib.util.spec_from_file_location('gtk_input', ROOT / 'veranda/desktop/wayland-input.py')
        driver = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(driver)
        with patch.object(driver, 'x11_key') as key, patch.object(driver, 'x11_input') as text:
            owned = unittest.mock.Mock()
            driver.x11_keys(77, ('-M', 'ctrl', '-k', 'a', '-m', 'ctrl', '庭院 مرحبا α',
                                 '-k', 'Return', '-k', 'Down', '-k', 'F6'), owned)
            self.assertEqual([call.args for call in key.call_args_list],
                             [(77, 'ctrl+a'), (77, 'Return'), (77, 'Down'), (77, 'F6')])
            text.assert_called_once_with(77, ['type', '--clearmodifiers', '--delay', '25', '--', '庭院 مرحبا α'])
            self.assertEqual(owned.call_count, 5)
            key.reset_mock(); text.reset_mock(); owned.reset_mock()
            for arguments in (('-k', 'Tab', '-k'), ('-M', 'alt', '-k', 'r'), ('-k', 'Delete'),
                              ('-k', 'F5'), ('-M', 'alt', '-k', 'F6'),
                              ('-M', 'ctrl', 'text', '-m', 'ctrl'), ('--window', '12')):
                with self.subTest(arguments=arguments), self.assertRaises(ValueError):
                    driver.x11_keys(77, arguments, owned)
            key.assert_not_called(); text.assert_not_called(); owned.assert_not_called()

    def test_x11_activation_must_retain_the_exact_owned_window(self):
        spec = importlib.util.spec_from_file_location('gtk_input', ROOT / 'veranda/desktop/wayland-input.py')
        driver = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(driver)
        for replies in ([b'12\n', b'77\n', b'13\n'], [b'12\n', b'77\n', b'12\n', b'88\n']):
            with self.subTest(replies=replies), patch.object(driver.subprocess, 'check_output', side_effect=replies), \
                    patch.object(driver.subprocess, 'run') as physical:
                with self.assertRaisesRegex(AssertionError, 'owned_x11_window_changed'):
                    driver.x11_key(77, 'Return')
                self.assertEqual(physical.call_args_list[0].args[0], ['xdotool', 'windowactivate', '--sync', '12'])
                self.assertEqual(physical.call_count, 1)

    def test_x11_remote_and_ui_reach_the_vm_guard(self):
        spec = importlib.util.spec_from_file_location('gtk_short', ROOT / 'veranda/desktop/wayland-short.py')
        short = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(short)
        for flags in (['--remote'], ['--remote', '--ui']):
            stderr = io.StringIO()
            with self.subTest(flags=flags), patch.object(sys, 'argv', ['short', 'gui', 'engine', '--x11', *flags]), \
                    patch.dict(os.environ, SUBYARD_E2E_VM='0'), \
                    patch.object(short.subprocess, 'run') as native, contextlib.redirect_stderr(stderr):
                with self.assertRaises(SystemExit) as error:
                    short.main()
                self.assertEqual(error.exception.code, 2)
                self.assertIn('requires an allocated unprivileged disposable VM', stderr.getvalue())
                native.assert_not_called()

    def test_fresh_release_uses_installed_owner_and_refuses_a_different_engine(self):
        for same_engine in (False, True):
            with self.subTest(same_engine=same_engine), \
                    tempfile.TemporaryDirectory(prefix='veranda-release-', dir='/tmp') as temporary:
                state = Path(temporary)
                engine = state / 'yard'
                engine.write_bytes(b'candidate')
                env = {'SUBYARD_CONFIG_HOME': str(state / 'config'), 'SUBYARD_HOME': str(state / 'data')}
                commands = []
                def native(argv, current, log, **options):
                    commands.append(argv)
                    self.assertIs(current, env)
                    self.assertIs(options['privileged'], True)
                    if '--output-dir' in argv:
                        release = Path(argv[-1])
                        release.mkdir()
                        (release / 'subyard-0.1.1-linux-amd64.tar.gz').touch()
                    elif '--bundle' in argv:
                        installed = Path(env['YARD_RUNTIME_ROOT']) / 'current/bin/yard-engine'
                        installed.parent.mkdir(parents=True)
                        installed.write_bytes(b'candidate' if same_engine else b'other')
                    elif '--check' in argv:
                        log.write('{"outcome":{"status":"ready"}}\n')
                    return 0
                with patch.object(fixture.subprocess, 'check_output', return_value='yard 0.1.1\n'), \
                        patch.object(fixture, 'run_probe', side_effect=native):
                    if same_engine:
                        fixture.prepare_release(state, engine, env)
                    else:
                        with self.assertRaisesRegex(RuntimeError, 'differs from the candidate'):
                            fixture.prepare_release(state, engine, env)
                self.assertEqual(len(commands), 5 if same_engine else 2)
                if same_engine:
                    launcher = str(state / 'data/runtime/current/bin/yard')
                    self.assertEqual(commands[-2:], [[launcher, 'migrate', '--yes'],
                                                    [launcher, 'migrate', '--check', '--json']])

    def test_only_documented_singleton_vm1_reaches_artifact_validation(self):
        class ReachedArtifactValidation(Exception):
            pass
        base = {'SUBYARD_E2E_VM': '1', 'SUBYARD_E2E_VM_COUNT': '1',
                'SUBYARD_E2E_PURPOSE': 'veranda-memory-research',
                'SUBYARD_E2E_RUN_ID': '1234abcd', 'SUBYARD_E2E_SLOT': 'slot-001',
                'SUBYARD_E2E_CPU_PER_VM': '4', 'SUBYARD_E2E_MEMORY_PER_VM': '4GiB',
                'SUBYARD_E2E_DISK_PER_VM': '20GiB', 'SUBYARD_E2E_BASE_FINGERPRINT': 'a' * 64}
        for kind in ('subyard-pair', 'android-test'):
            for changes, valid in (({}, True), ({'SUBYARD_E2E_VM_COUNT': '2'}, False),
                                   ({'SUBYARD_E2E_VM_COUNT': None}, False),
                                   ({'SUBYARD_E2E_VM': '2'}, False),
                                   ({'SUBYARD_E2E_CPU_PER_VM': None}, False),
                                   ({'SUBYARD_E2E_MEMORY_PER_VM': 'private-sentinel'}, False),
                                   ({'SUBYARD_E2E_PURPOSE': 'other'}, False)):
                env = dict(base, SUBYARD_E2E_TYPE=kind)
                env.update(changes)
                env = {key: value for key, value in env.items() if value is not None}
                with self.subTest(kind=kind, changes=changes), patch.dict(os.environ, env, clear=True), \
                        patch.object(memory.os, 'getuid', return_value=1000), \
                        patch.object(sys, 'argv', ['memory', 'fixed.deb', 'fixed-engine']), \
                        patch.object(memory.Path, 'lstat', side_effect=ReachedArtifactValidation) as artifact, \
                        patch.object(memory.subprocess, 'run') as native, \
                        patch.object(memory.subprocess, 'Popen') as child:
                    with self.assertRaises(ReachedArtifactValidation if valid else ValueError):
                        memory.main()
                    self.assertEqual(artifact.call_count, 1 if valid else 0)
                    native.assert_not_called()
                    child.assert_not_called()

    def test_platform_names_with_underscore_suffix_keep_one_owned_lifecycle(self):
        for suffix in ('1_nd72r3', 'trailing_', '________'):
            for fail_body in (False, True):
                with self.subTest(suffix=suffix, fail_body=fail_body), \
                        tempfile.TemporaryDirectory(prefix='veranda-platform-name-', dir='/tmp') as temporary:
                    root = Path(temporary)
                    root.chmod(0o700)
                    home, state = root / 'home', root / ('veranda-wayland-' + suffix)
                    for directory in (home, home / '.cache', state):
                        directory.mkdir(mode=0o700)
                        directory.chmod(0o700)
                    platform = home / '.cache/subyard-e2e-platform'
                    commands, events, pending_markers = [], [], []
                    engine = root / 'yard'
                    fixture.probe.private_file(engine, 'not executed')
                    original = subprocess.CalledProcessError(17, ['fixed-body'])
                    def host_command(argv, **options):
                        # Simulate only owned install/marker operations; never invoke sudo.
                        self.assertEqual(argv[:2], ['sudo', '-n'])
                        if argv[2:4] == ['install', '-d']:
                            self.assertEqual(Path(argv[-1]), platform)
                            platform.mkdir(mode=0o711)
                            platform.chmod(0o711)
                        elif argv[2] == 'install':
                            self.assertEqual(argv[-4:-2], ['-m', '0600'])
                            pending_markers.append(Path(argv[-1]).name)
                            fixture.probe.private_file(Path(argv[-1]), Path(argv[-2]).read_text())
                        else:
                            self.assertEqual(argv[2:4], ['mv', '-fT'])
                            Path(argv[-2]).replace(argv[-1])
                        return SimpleNamespace(returncode=0)
                    def product(argv, env, log, **options):
                        self.assertEqual(argv[0], str(engine))
                        self.assertEqual(argv[1], '-Y')
                        self.assertEqual(argv[-1], '--yes')
                        self.assertIs(options['privileged'], True)
                        # The actual generated argv must satisfy the reported Incus boundary.
                        self.assertRegex('subyard-' + argv[2], r'^[A-Za-z0-9][A-Za-z0-9-]*[A-Za-z0-9]$')
                        commands.append(argv)
                        if argv[3] == 'init':
                            (platform / 'incus').mkdir(mode=0o700)
                            (platform / 'incus').chmod(0o700)
                        return 0
                    with patch.object(fixture.pwd, 'getpwuid', return_value=SimpleNamespace(pw_dir=str(home))), \
                            patch.object(fixture.subprocess, 'run', side_effect=host_command), \
                            patch.object(fixture, 'run_probe', side_effect=product), \
                            patch.object(fixture, 'product_stop_check'), \
                            patch.object(fixture, 'prepare_release') as release, \
                            patch.object(fixture.probe, 'isolated_defaults'):
                        if fail_body:
                            with self.assertRaises(subprocess.CalledProcessError) as caught:
                                with fixture.prepared_platform(state, engine, lambda *event: events.append(event)):
                                    raise original
                            self.assertIs(caught.exception, original)
                        else:
                            with fixture.prepared_platform(state, engine, lambda *event: events.append(event)):
                                pass
                    release.assert_called_once()
                    self.assertEqual(release.call_args.args[2]['SUBYARD_CONFIG_HOME'], str(state / 'prepare-config'))
                    self.assertEqual(release.call_args.args[2]['SUBYARD_HOME'], str(state / 'prepare-data'))
                    self.assertEqual([argv[3:-1] for argv in commands],
                                     [['init'], ['teardown', '--keep-data'], ['teardown']])
                    names = {argv[2] for argv in commands}
                    self.assertEqual(len(names), 1)
                    self.assertEqual([path.name for path in (state / 'prepare-config/yards').iterdir()], list(names))
                    self.assertEqual(pending_markers, ['.subyard-e2e-platform-marker.' + next(iter(names))])
                    self.assertEqual((platform / '.subyard-e2e-platform-marker').read_text(),
                                     'subyard-e2e-platform-v1\n')
                    self.assertEqual(events, [('start', 'platform', None, 'started'), ('end', 'platform', 0, 'passed')])

    def test_installer17_survives_end_phase_reporting_failure(self):
        args = SimpleNamespace(active_case='F', active_repeat=1, phase='case-install', child_exit=None)
        original, frames = subprocess.CalledProcessError(17, ['fixed-installer']), []
        def emit(text, **options):
            frame = json.loads(text.split(' ', 1)[1])
            frames.append(frame)
            if frame['state'] == 'end':
                raise BrokenPipeError('closed evidence stream')
        with patch.object(memory, 'print', side_effect=emit, create=True), \
                patch.object(memory.time, 'monotonic', side_effect=[10, 11]), \
                contextlib.redirect_stderr(io.StringIO()):
            with self.assertRaises(subprocess.CalledProcessError) as caught:
                with memory.case_phase(args, 'install'):
                    raise original
        self.assertIs(caught.exception, original)
        self.assertEqual(frames, [{'schema_version': 1, 'case': 'F', 'repeat': 1, 'stage': 'install',
                                  'state': 'start', 'duration_seconds': 0, 'exit_code': None, 'status': 'started'},
                                 {'schema_version': 1, 'case': 'F', 'repeat': 1, 'stage': 'install',
                                  'state': 'end', 'duration_seconds': 1, 'exit_code': 17, 'status': 'failed'}])
        self.assertEqual((args.phase, args.child_exit), ('case-install', None))

    def test_successful_native_body_cannot_hide_failed_phase_reporting(self):
        args = SimpleNamespace(active_case='M', active_repeat=2, phase='case-probe', child_exit=None)
        original, calls = BrokenPipeError('closed evidence stream'), []
        def emit(text, **options):
            if json.loads(text.split(' ', 1)[1])['state'] == 'end':
                raise original
        with patch.object(memory, 'print', side_effect=emit, create=True):
            with self.assertRaises(BrokenPipeError) as caught:
                with memory.case_phase(args, 'probe'):
                    calls.append('native completed')
                    args.child_exit = 0
        self.assertIs(caught.exception, original)
        self.assertEqual(calls, ['native completed'])
        self.assertEqual((args.phase, args.child_exit), ('case-probe', 0))

    def test_failed_probe_keeps_actual_child_exit_separate_from_cleanup_failure(self):
        for child_exit in (17, 0):
            with self.subTest(child_exit=child_exit):
                args = SimpleNamespace(active_case='N', active_repeat=3, phase='cgroup-cleanup', child_exit=child_exit)
                original, capture = subprocess.CalledProcessError(124, ['fixed-cleanup']), io.StringIO()
                with contextlib.redirect_stdout(capture):
                    with self.assertRaises(subprocess.CalledProcessError) as caught:
                        with memory.case_phase(args, 'probe'):
                            raise original
                frame = json.loads(capture.getvalue().splitlines()[-1].split(' ', 1)[1])
                self.assertIs(caught.exception, original)
                self.assertEqual((frame['status'], frame['exit_code']), ('failed', child_exit))
                self.assertEqual((args.phase, args.child_exit), ('cgroup-cleanup', child_exit))

    def test_guard_and_unavailable_probe_exit_remain_null(self):
        for stage, child_exit in (('verification', 0), ('probe', None)):
            with self.subTest(stage=stage):
                args = SimpleNamespace(active_case='A', active_repeat=1, phase='case-metrics', child_exit=child_exit)
                original, capture = ValueError('guard failed'), io.StringIO()
                with contextlib.redirect_stdout(capture):
                    with self.assertRaises(ValueError) as caught:
                        with memory.case_phase(args, stage):
                            raise original
                frame = json.loads(capture.getvalue().splitlines()[-1].split(' ', 1)[1])
                self.assertIs(caught.exception, original)
                self.assertEqual((frame['status'], frame['exit_code']), ('failed', None))

    def test_legacy_comparison_cases_keep_successful_timing_frames(self):
        for case in ('A', 'D-image', 'C-on-A'):
            with self.subTest(case=case):
                args = SimpleNamespace(active_case=case, active_repeat=1, phase='case-install', child_exit=None)
                capture = io.StringIO()
                with contextlib.redirect_stdout(capture):
                    with memory.case_phase(args, 'install'):
                        pass
                frames = [json.loads(line.split(' ', 1)[1]) for line in capture.getvalue().splitlines()]
                self.assertEqual([frame['state'] for frame in frames], ['start', 'end'])
                self.assertEqual([(frame['case'], frame['stage']) for frame in frames], [(case, 'install')] * 2)
                self.assertEqual((frames[-1]['status'], frames[-1]['exit_code']), ('passed', 0))
                self.assertGreaterEqual(frames[-1]['duration_seconds'], 0)

    def test_same_deb_content_skips_all_repeated_installs(self):
        with tempfile.TemporaryDirectory(prefix='veranda-install-', dir='/tmp') as directory:
            Path(directory).chmod(0o700)
            a, alias = Path(directory) / 'a.deb', Path(directory) / 'alias.deb'
            for path in (a, alias):
                path.write_bytes(b'candidate A')
                path.chmod(0o600)
            original_read = Path.read_bytes
            executable = b'installed executable A'
            binary_sha = hashlib.sha256(executable).hexdigest()
            cache = hashlib.sha256(a.read_bytes()).hexdigest()
            def read(path):
                return executable if path == Path('/usr/bin/subyard-veranda') else original_read(path)
            with patch.object(memory.Path, 'read_bytes', read), \
                    patch.object(memory, 'installed_binary', return_value=binary_sha) as payload, \
                    patch.object(memory.subprocess, 'run') as installer:
                for index in range(9):
                    cache, actual = memory.install_candidate(a if index % 2 else alias, cache, io.StringIO())
                    self.assertEqual(actual, binary_sha)
                installer.assert_not_called()
                self.assertEqual(payload.call_count, 9)
            self.assertEqual(cache, hashlib.sha256(b'candidate A').hexdigest())

    def test_changed_deb_and_reversal_each_reinstall_once(self):
        with tempfile.TemporaryDirectory(prefix='veranda-install-', dir='/tmp') as directory:
            Path(directory).chmod(0o700)
            a, b = Path(directory) / 'a.deb', Path(directory) / 'b.deb'
            for path, content in ((a, b'candidate A'), (b, b'candidate B')):
                path.write_bytes(content)
                path.chmod(0o600)
            original_read, installed = Path.read_bytes, [b'executable A']
            expected = {a: b'executable A', b: b'executable B'}
            def read(path):
                return installed[0] if path == Path('/usr/bin/subyard-veranda') else original_read(path)
            def payload(path):
                return hashlib.sha256(expected[path]).hexdigest()
            def install(argv, **options):
                installed[0] = expected[Path(argv[-1])]
            log, cache = io.StringIO(), hashlib.sha256(a.read_bytes()).hexdigest()
            with patch.object(memory.Path, 'read_bytes', read), \
                    patch.object(memory, 'installed_binary', side_effect=payload), \
                    patch.object(memory.subprocess, 'run', side_effect=install) as installer:
                for path in (a, b, b, a, a):
                    cache, binary_sha = memory.install_candidate(path, cache, log)
                    self.assertEqual(cache, hashlib.sha256(original_read(path)).hexdigest())
                    self.assertEqual(binary_sha, payload(path))
                self.assertEqual([call.args[0][-1] for call in installer.call_args_list], [str(b), str(a)])
                for call in installer.call_args_list:
                    self.assertEqual(call.args[0][:-1], ['sudo', '-n', 'env', 'DEBIAN_FRONTEND=noninteractive',
                                                       'apt-get', 'install', '--reinstall', '-y', '--no-install-recommends'])
                    self.assertEqual(call.kwargs, {'stdout': log, 'stderr': subprocess.STDOUT, 'check': True, 'timeout': 180})

    def test_failed_installer_preserves_native17_and_cached_identity(self):
        original = subprocess.CalledProcessError(17, ['fixed-installer'])
        cache = ('a' * 64, 'b' * 64)
        with patch.object(memory.Path, 'read_bytes', return_value=b'changed candidate'), \
                patch.object(memory, 'installed_binary', return_value='c' * 64), \
                patch.object(memory.subprocess, 'run', side_effect=original):
            with self.assertRaises(subprocess.CalledProcessError) as caught:
                cache = memory.install_candidate(Path('changed.deb'), cache[0], io.StringIO())
        self.assertIs(caught.exception, original)
        self.assertEqual(caught.exception.returncode, 17)
        self.assertEqual(cache, ('a' * 64, 'b' * 64))

    def test_payload_executable_mismatch_cannot_advance_cache(self):
        content = b'candidate A'
        cache = (hashlib.sha256(content).hexdigest(), 'b' * 64)
        with patch.object(memory.Path, 'read_bytes', return_value=content), \
                patch.object(memory, 'installed_binary', return_value='c' * 64), \
                patch.object(memory.subprocess, 'run') as installer:
            with self.assertRaises(ValueError):
                cache = memory.install_candidate(Path('candidate.deb'), cache[0], io.StringIO())
            installer.assert_not_called()
        self.assertEqual(cache, (hashlib.sha256(content).hexdigest(), 'b' * 64))

    def test_fixture_watchdog_preserves_original_identity_deletion_script(self):
        calls = []
        with patch.object(fixture.subprocess, 'run', side_effect=lambda argv, **options: calls.append((argv, options))):
            fixture.cleanup_state(Path('/var/tmp/veranda-wayland-owned'), (1001, 2, 3))
        argv, options = calls[0]
        self.assertEqual(argv[:6], ['sudo', '-n', 'timeout', '--signal=TERM', '--kill-after=2s', '10s'])
        self.assertEqual(argv[6:8], [sys.executable, '-c'])
        self.assertEqual(hashlib.sha256(argv[8].encode()).hexdigest(),
                         '93182e864656a69a11da6a3f198551d8f83b2a23d780708c24743eb500068d59')
        self.assertEqual(argv[9:], ['/var/tmp/veranda-wayland-owned', '1001', '2', '3'])
        self.assertEqual(options['timeout'], 15)
        self.assertIs(options['check'], True)
        for name in ('stdin', 'stdout', 'stderr'):
            self.assertEqual(options[name], subprocess.DEVNULL)

    def test_original_body_error_survives_cleanup_failure(self):
        original = ValueError('native failure')
        events = []
        def fail():
            raise subprocess.CalledProcessError(124, ['fixed-cleanup'])
        with contextlib.redirect_stderr(io.StringIO()):
            with self.assertRaises(ValueError) as caught:
                with fixture.cleanup_on_exit(fail, 'fixture', lambda *event: events.append(event)):
                    raise original
        self.assertIs(caught.exception, original)
        self.assertEqual(events, [('start', 'fixture', None, 'started'), ('end', 'fixture', 124, 'failed')])

    def test_failed_reporting_still_cleans_and_preserves_action_error(self):
        calls = []
        def observer(*_):
            raise BrokenPipeError()
        with self.assertRaises(BrokenPipeError):
            with fixture.cleanup_on_exit(lambda: calls.append('cleaned'), 'fixture', observer):
                pass
        self.assertEqual(calls, ['cleaned'])
        original = subprocess.CalledProcessError(17, ['fixed-cleanup'])
        def fail():
            raise original
        with self.assertRaises(subprocess.CalledProcessError) as caught:
            with fixture.cleanup_on_exit(fail, 'fixture', observer):
                pass
        self.assertIs(caught.exception, original)

    def test_faulting_stderr_cannot_replace_original_body_error(self):
        class FaultingStderr(io.StringIO):
            def write(self, _):
                raise BrokenPipeError('diagnostic destination closed')
        original = ValueError('native failure')
        calls = []
        def fail():
            calls.append('cleanup-attempted')
            raise subprocess.CalledProcessError(124, ['fixed-cleanup'])
        with contextlib.redirect_stderr(FaultingStderr()):
            with self.assertRaises(ValueError) as caught:
                with fixture.cleanup_on_exit(fail, 'fixture'):
                    raise original
        self.assertIs(caught.exception, original)
        self.assertEqual(calls, ['cleanup-attempted'])

    def run_main(self, fixture_failure=False, native_failure=False):
        with tempfile.TemporaryDirectory(prefix='veranda-memory-evidence-', dir='/tmp') as directory:
            state = Path(directory)
            state.chmod(0o700)
            deb, engine = state / 'candidate.deb', state / 'engine'
            for path in (deb, engine):
                fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
                os.fchmod(fd, 0o600)
                os.close(fd)
            setup = {'screen': {'width': 1920, 'height': 1080, 'refresh_millihertz': 60000, 'scale': 1}}
            case = {'case': 'A', 'repeat': 1, 'deb_sha256': 'a' * 64,
                    'installed_binary_sha256': 'b' * 64, 'allocation_mode': None,
                    'metrics': {'inputs': {'binary_sha256': 'b' * 64, 'engine_sha256': 'c' * 64}}}
            commands, order = [], []
            def command(argv, **options):
                commands.append((argv, options))
                if argv[0] == 'dpkg-query':
                    return SimpleNamespace(returncode=1)
                return SimpleNamespace(returncode=0)
            def output(argv):
                return 'Package: subyard-veranda\nVersion: 0.1.1\nArchitecture: amd64' if argv[0] == 'dpkg-deb' else 'engine 0.1.1'
            def cleanup_state(*_):
                order.append('fixture')
                if fixture_failure:
                    raise subprocess.CalledProcessError(124, ['fixed-fixture'])
            @contextlib.contextmanager
            def platform(state, engine, observer):
                with fixture.cleanup_on_exit(lambda: order.append('platform'), 'platform', observer):
                    yield
            def measure(state, args, cleanup):
                with fixture.cleanup_on_exit(lambda: cleanup.update(compositor='passed'), 'compositor', args.cleanup_observer):
                    with fixture.cleanup_on_exit(lambda: cleanup.update(cgroup='passed'), 'app cgroup', args.cleanup_observer):
                        args.child_exit = 0
                    memory.record_case(args, case, setup)
                    if native_failure:
                        args.child_exit = 9
                        raise ValueError('later native probe failure')
                return setup
            environment = {'SUBYARD_E2E_VM': '1', 'SUBYARD_E2E_TYPE': 'android-test',
                           'SUBYARD_E2E_VM_COUNT': '1', 'SUBYARD_E2E_CPU_PER_VM': '4',
                           'SUBYARD_E2E_MEMORY_PER_VM': '8GiB', 'SUBYARD_E2E_DISK_PER_VM': '40GiB',
                           'SUBYARD_E2E_BASE_FINGERPRINT': 'a' * 64,
                           'SUBYARD_E2E_PURPOSE': 'veranda-memory-research',
                           'SUBYARD_E2E_RUN_ID': '1234abcd', 'SUBYARD_E2E_SLOT': 'slot-001'}
            capture = io.StringIO()
            with patch.dict(os.environ, environment), patch.object(sys, 'argv', ['memory', str(deb), str(engine)]), \
                    patch.object(memory.os, 'getuid', return_value=1001), \
                    patch.object(memory.subprocess, 'run', side_effect=command), \
                    patch.object(memory.probe, 'command_output', side_effect=output), \
                    patch.object(memory.probe, 'resource_witness', return_value={'schema_version': 1}), \
                    patch.object(memory, 'group_command'), patch.object(memory, 'measure', side_effect=measure), \
                    patch.object(fixture, 'create_state', return_value=(state, (1001, 2, 3))), \
                    patch.object(fixture, 'cleanup_state', side_effect=cleanup_state), \
                    patch.object(fixture, 'prepared_platform', platform), \
                    contextlib.redirect_stdout(capture), contextlib.redirect_stderr(io.StringIO()):
                rc = memory.main()
            frames = [(line.split(' ', 1)[0], json.loads(line.split(' ', 1)[1])) for line in capture.getvalue().splitlines()]
            case_index = next(i for i, item in enumerate(frames) if item[0] == 'VERANDA_MEMORY_CASE')
            fixture_start = next(i for i, (prefix, value) in enumerate(frames)
                                 if prefix == 'VERANDA_MEMORY_CLEANUP' and value['label'] == 'fixture' and value['phase'] == 'start')
            self.assertLess(case_index, fixture_start)
            self.assertEqual(frames[case_index][1], {'schema_version': 1, 'case': case, 'setup': setup})
            self.assertEqual(frames[-1][0], 'VERANDA_MEMORY_RESULT')
            receipt = frames[-1][1]
            self.assertEqual(receipt['cases'], [case])
            events = receipt['cleanup_events']
            self.assertEqual({event['label'] for event in events}, {'cgroup', 'compositor', 'platform', 'package', 'fixture'})
            for label in {'cgroup', 'compositor', 'platform', 'package', 'fixture'}:
                self.assertEqual([event['phase'] for event in events if event['label'] == label], ['start', 'end'])
            self.assertEqual(order, ['platform', 'fixture'])
            package = next((argv, options) for argv, options in commands if 'remove' in argv)
            self.assertEqual(package[0], ['sudo', '-n', 'timeout', '--signal=TERM', '--kill-after=2s', '60s',
                                          'apt-get', 'remove', '-y', 'subyard-veranda'])
            self.assertEqual(package[1]['timeout'], 65)
            self.assertIs(package[1]['check'], True)
            return rc, receipt

    def test_case_survives_fixture_failure_without_false_measured_result(self):
        rc, receipt = self.run_main(fixture_failure=True)
        self.assertEqual(rc, 1)
        self.assertEqual(receipt['result'], 'failed')
        self.assertEqual(receipt['cleanup']['fixture'], 'failed')
        self.assertEqual(receipt['phase'], 'fixture-cleanup')
        self.assertEqual(receipt['cleanup_events'][-1],
                         {'phase': 'end', 'label': 'fixture', 'exit_code': 124, 'status': 'failed'})

    def test_original_native_failure_survives_later_failed_fixture(self):
        rc, receipt = self.run_main(fixture_failure=True, native_failure=True)
        self.assertEqual(rc, 1)
        self.assertEqual(receipt['child_exit'], 9)
        self.assertEqual(receipt['cleanup']['fixture'], 'failed')
        self.assertEqual(receipt['result'], 'failed')

    def test_measured_result_only_after_complete_owned_cleanup(self):
        rc, receipt = self.run_main()
        self.assertEqual(rc, 0)
        self.assertEqual(receipt['result'], 'measured')
        self.assertEqual(receipt['phase'], 'complete')
        self.assertTrue(all(value == 'passed' for value in receipt['cleanup'].values()))


if __name__ == '__main__':
    unittest.main()
