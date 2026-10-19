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

    def test_destination_requires_actual_owned_panel_and_unique_focused_editable_text(self):
        spec = importlib.util.spec_from_file_location('gtk_input', ROOT / 'veranda/desktop/wayland-input.py')
        driver = importlib.util.module_from_spec(spec); spec.loader.exec_module(driver)
        atspi = SimpleNamespace(Role=SimpleNamespace(PANEL=39, LABEL=29, TEXT=61, PUSH_BUTTON=43),
            StateType=SimpleNamespace(SHOWING='showing', FOCUSED='focused', EDITABLE='editable', DEFUNCT='defunct'))
        def node(role, name, *states, parent=None, pid=77):
            return SimpleNamespace(get_role=lambda: role, get_name=lambda: name, get_process_id=lambda: pid,
                get_parent=lambda: parent, get_state_set=lambda: SimpleNamespace(contains=lambda value: value in states))
        panel = node(39, '', 'showing'); other = node(39, '', 'showing')
        button = node(43, 'Connect remote host', 'showing')
        nav_label = node(29, 'Connect remote host', 'showing', parent=button)
        title = node(29, 'Connect remote host', 'showing', parent=panel)
        editor = node(61, 'Remote SSH destination', 'showing', 'focused', 'editable', parent=panel)
        valid = [panel, button, nav_label, title, editor]
        for allowed, items in ((True, valid), (False, [panel, button, nav_label, editor]),
                (False, [panel, other, title, node(61, 'Remote SSH destination', 'showing', 'focused', 'editable', parent=other)]),
                (False, [panel, title, node(29, 'Remote SSH destination', 'showing', 'focused', 'editable', parent=panel)]),
                (False, [panel, title, node(61, 'Remote SSH destination', 'showing', 'focused', parent=panel)]),
                (False, valid + [node(61, 'Remote SSH destination', 'showing', parent=panel)]),
                (False, [panel, title, node(61, 'Remote SSH destination', 'showing', 'focused', 'editable', parent=panel, pid=88)])):
            with self.subTest(allowed=allowed), patch.object(driver, 'x11_input') as physical:
                if driver.remote_editor_ready(items, 77, atspi):
                    physical(77, ['type', '--', '庭院 مرحبا α'])
                self.assertEqual(physical.call_count, int(allowed))

    def test_text_content_uses_explicit_interface_despite_accessible_method_collision(self):
        spec = importlib.util.spec_from_file_location('gtk_input', ROOT / 'veranda/desktop/wayland-input.py')
        driver = importlib.util.module_from_spec(spec); spec.loader.exec_module(driver)
        class Accessible:
            contents = '庭院 مرحبا α'
            def get_text(self):
                raise AssertionError('Accessible accessor must never read content')
        interface = Accessible()
        with self.assertRaises(TypeError):
            interface.get_text(0, -1)  # Exercise the real GI-shaped name collision.
        calls = []
        class TextAPI:
            def get_character_count(value):
                self.assertIs(value, interface)
                return len(value.contents)
            def get_text(value, start, end):
                calls.append((value, start, end))
                # GTK Editable clamps both offsets to Unicode character count.
                length = len(value.contents)
                return value.contents[max(0, min(start, length)):max(0, min(end, length))]
        self.assertEqual(driver.interface_text(interface, TextAPI), '庭院 مرحبا α')
        self.assertNotEqual(len(interface.contents), len(interface.contents.encode('utf-8')))
        self.assertEqual(calls, [(interface, 0, len(interface.contents))])
        with patch.object(TextAPI, 'get_text', return_value=interface.contents[:-1]):
            self.assertIsNone(driver.interface_text(interface, TextAPI))
        for count in (-1, 32769, True):
            with self.subTest(count=count), patch.object(TextAPI, 'get_character_count', return_value=count), \
                    patch.object(TextAPI, 'get_text') as content:
                with self.assertRaisesRegex(AssertionError, 'accessible_text_limit_exceeded'):
                    driver.interface_text(interface, TextAPI)
                content.assert_not_called()

    def test_unicode_delivery_and_scoped_persistence_share_one_deadline_and_private_scalars(self):
        spec = importlib.util.spec_from_file_location('gtk_input', ROOT / 'veranda/desktop/wayland-input.py')
        driver = importlib.util.module_from_spec(spec); spec.loader.exec_module(driver)
        tree = ast.parse((ROOT / 'veranda/desktop/wayland-input.py').read_text())
        branch = next(node for node in ast.walk(tree) if isinstance(node, ast.If)
                      and any(isinstance(item, ast.Assign) and isinstance(item.value, ast.Constant)
                              and item.value.value == 'host-unicode-editor-visible' for item in node.body))
        start = next(index for index, node in enumerate(branch.body) if isinstance(node, ast.Assign)
                     and isinstance(node.value, ast.Constant) and node.value.value == 'host-unicode-editor-visible')
        stop = next(index for index, node in enumerate(branch.body) if isinstance(node, ast.Assign)
                    and isinstance(node.value, ast.Constant) and node.value.value == 'editor-refresh-baseline')
        actual = compile(ast.Module(body=branch.body[start:stop], type_ignores=[]), 'actual-unicode-boundary', 'exec')
        atspi = SimpleNamespace(Role=SimpleNamespace(LIST=31, LIST_ITEM=32, LABEL=29, PANEL=39, TEXT=61),
            StateType=SimpleNamespace(SHOWING='showing', SELECTED='selected', FOCUSED='focused', EDITABLE='editable', DEFUNCT='defunct'))
        class Node:
            def __init__(self, role, name='', states=('showing',)):
                self.role, self.name, self.states, self.pid = role, name, set(states), 7
                self.parent, self.children, self.text = None, [], ''
            def get_role(self): return self.role
            def get_name(self): return self.name
            def get_process_id(self): return self.pid
            def get_parent(self): return self.parent
            def get_child_count(self): return len(self.children)
            def get_child_at_index(self, index): return self.children[index]
            def get_state_set(self): return SimpleNamespace(contains=lambda state: state in self.states)
            def get_text_iface(self): return self
        expected, wrong = '庭院 مرحبا α', 'PRIVATE_UNKNOWN_TEXT'
        accepted, foreign, destination = 'PRIVATE_ACCEPTED_ID', 'PRIVATE_FOREIGN_ID', 'PRIVATE_HOST'
        faults = ('exact', 'file-pending', 'wrong-text', 'incomplete-text', 'foreign-scope', 'duplicate-draft',
                  'malformed-tree', 'unowned-editor', 'wrong-focus', 'wrong-role', 'ambiguous-editor', 'late-text-change')
        for fault in faults:
            with self.subTest(fault=fault), tempfile.TemporaryDirectory(prefix='veranda-unicode-', dir='/tmp') as temporary:
                root = Path(temporary) / 'subyard/veranda'; root.mkdir(parents=True, mode=0o700)
                def document(connection=accepted, copies=1):
                    path = root / 'desktop.json'
                    path.write_text(json.dumps({'drafts': [{'scope': {'connection': connection, 'yard': None},
                        'field': 'create:name', 'value': expected}] * copies})); path.chmod(0o600)
                if fault != 'file-pending':
                    document(foreign if fault == 'foreign-scope' else accepted, 3 if fault == 'duplicate-draft' else 1)
                nav = Node(31, 'Owner hosts and yards'); row = Node(32, states=('showing', 'selected'))
                label = Node(29, destination); heading = Node(29, destination); panel = Node(39)
                nav.children = [row]; row.parent = nav; row.children = [label]; label.parent = row; heading.parent = panel
                editor = Node(61, 'Yard name', ('showing', 'focused', 'editable'))
                editor.text = wrong if fault == 'wrong-text' else expected
                items = [nav, row, label, heading, panel, editor]
                if fault == 'malformed-tree': nav.children = []
                if fault == 'unowned-editor': editor.pid = 8
                if fault == 'wrong-focus': editor.states.remove('focused')
                if fault == 'wrong-role': editor.role = 29
                if fault == 'ambiguous-editor': items.append(Node(61, 'Yard name', ('showing', 'focused', 'editable')))
                atspi.Text = SimpleNamespace(get_character_count=lambda value: len(value.text),
                    get_text=lambda value, start, end: value.text[start:end - 1] if fault == 'incomplete-text' else value.text[start:end])
                commands, deadlines, persisted, reads, clock_reads = [], [], [], [], []
                clock, result = [100], {'original_exit_code': 1}
                def monotonic(): clock_reads.append(True); return clock[0]
                def keys(*arguments):
                    self.assertEqual(arguments, ('-M', 'ctrl', '-k', 'a', '-m', 'ctrl', expected))
                    commands.append(arguments)
                def nodes(observe=None, site=None): return observe(items) if observe is not None else items
                def draft(*arguments):
                    persisted.append(True)
                    if fault == 'file-pending' and len(persisted) == 2: document()
                    value = driver.persisted_host_draft(*arguments)
                    if value and fault == 'late-text-change': editor.text = wrong
                    return value
                def wait(predicate, seconds=20, deadline=None):
                    end = clock[0] + seconds if deadline is None else deadline
                    if deadline is not None: deadlines.append(deadline)
                    while clock[0] < end:
                        value = predicate()
                        if value: return value
                        clock[0] += 1
                    raise TimeoutError(namespace['stage'])
                namespace = dict(vars(driver), pid=7, destination=destination, unicode_draft=expected, Atspi=atspi,
                    saved=json.dumps({'connections': [{'id': accepted}]}), result=result, nodes=nodes, wait=wait,
                    keys=keys, names=lambda: {'Yard name'}, focus=lambda name: None,
                    tab=lambda name, scope: self.assertEqual((name, scope), ('Yards', destination)),
                    time=SimpleNamespace(monotonic=monotonic), persisted_host_draft=draft)
                read_bytes = Path.read_bytes
                def read(path): reads.append(path); return read_bytes(path)
                failure = None
                with patch.dict(os.environ, XDG_DATA_HOME=temporary), patch.object(Path, 'read_bytes', read):
                    try: exec(actual, namespace)
                    except (TimeoutError, AssertionError) as error: failure = error
                self.assertEqual(len(commands), 1)  # No resend, including text/persistence timeouts.
                self.assertEqual(clock_reads, [True])
                self.assertTrue(deadlines and all(end == 120 for end in deadlines))
                self.assertLessEqual(clock[0], 120)
                self.assertEqual(result['original_exit_code'], 1)
                if fault in ('exact', 'file-pending'):
                    self.assertIsNone(failure); self.assertTrue(result['unicode_rtl_gtk_draft_saved'])
                    self.assertEqual(deadlines, [120, 120])
                    self.assertEqual(len(reads), 1)
                else:
                    self.assertIsNotNone(failure); self.assertNotIn('unicode_rtl_gtk_draft_saved', result)
                    if fault in ('wrong-text', 'incomplete-text', 'foreign-scope', 'duplicate-draft'):
                        self.assertIsInstance(failure, TimeoutError); self.assertEqual(clock[0], 120)
                        self.assertEqual(namespace['stage'], 'host-unicode-committed-entry' if fault in ('wrong-text', 'incomplete-text') else 'host-unicode-persisted-draft')
                    else: self.assertIsInstance(failure, AssertionError)
                if fault in ('wrong-text', 'incomplete-text'):
                    self.assertEqual(persisted, []); self.assertEqual(reads, [])
                if fault == 'duplicate-draft':
                    self.assertEqual(result['unicode_observation']['matching_draft_count'], 2)
                if fault == 'late-text-change':
                    self.assertEqual(str(failure), 'unicode_gtk_text_mismatch')  # Existing final assert still owns this boundary.
                observation = result.get('unicode_observation', {})
                self.assertTrue(all(value is None or type(value) in (bool, int) for value in observation.values()))
                self.assertTrue(all(value is None or type(value) is bool or 0 <= value <= 32768 for value in observation.values()))
                for private in (expected, wrong, accepted, foreign, destination, temporary):
                    self.assertNotIn(private, json.dumps(result))

    def test_editor_continuity_is_compared_with_refresh_focus_before_refocus(self):
        spec = importlib.util.spec_from_file_location('gtk_input', ROOT / 'veranda/desktop/wayland-input.py')
        driver = importlib.util.module_from_spec(spec); spec.loader.exec_module(driver)
        atspi = SimpleNamespace(Role=SimpleNamespace(PUSH_BUTTON=43, TEXT=61, LABEL=29),
            StateType=SimpleNamespace(SHOWING='showing', FOCUSED='focused', EDITABLE='editable', DEFUNCT='defunct'))
        def node(role, name, states):
            return SimpleNamespace(get_role=lambda: role, get_name=lambda: name, get_process_id=lambda: 77,
                get_state_set=lambda: SimpleNamespace(contains=lambda value: value in states))
        refresh = node(43, 'Refresh', ('showing','focused'))
        editor = node(61, 'Yard name', ('showing','editable'))
        iface = SimpleNamespace(contents='庭院 مرحبا α', get_caret_offset=lambda: 4, selections=[(2,4)])
        atspi.Text = SimpleNamespace(get_character_count=lambda interface: len(interface.contents),
            get_text=lambda interface, start, end: interface.contents[max(0, min(start, len(interface.contents))):max(0, min(end, len(interface.contents)))],
            get_n_selections=lambda interface: len(interface.selections),
            get_selection=lambda interface, index: SimpleNamespace(start_offset=interface.selections[index][0], end_offset=interface.selections[index][1]))
        editor.get_text_iface = lambda: iface
        self.assertEqual(driver.selection_baseline(iface,atspi.Text,len(iface.contents)),(4,[(2,4)]))
        for caret, selections in ((4,[]),(4,[(4,4)]),(-1,[(2,4)]),(11,[(2,4)]),
                                  (True,[(2,4)]),(3,[(2,4)])):
            iface.selections = selections
            with self.subTest(caret=caret,selections=selections), \
                    patch.object(iface,'get_caret_offset',return_value=caret), \
                    self.assertRaisesRegex(AssertionError,'refresh_reset_selection'):
                driver.selection_baseline(iface,atspi.Text,len(iface.contents))
        iface.selections = [(2,4)]
        self.assertTrue(driver.editor_continuity([refresh,editor],77,atspi,editor,'庭院 مرحبا α',4,[(2,4)]))
        with self.assertRaisesRegex(AssertionError,'refresh_reset_caret'):
            driver.editor_continuity([refresh,editor],77,atspi,editor,'庭院 مرحبا α',3,[(2,4)])
        for selections in ([], [(3,4)], [(-1,4)], [(2,11)], [(2,4),(5,6)]):
            iface.selections = selections
            if not selections:self.assertEqual(driver.selection_ranges(iface,atspi.Text,len(iface.contents)),[])
            with self.subTest(selections=selections), self.assertRaisesRegex(AssertionError,'refresh_reset_selection'):
                driver.editor_continuity([refresh,editor],77,atspi,editor,'庭院 مرحبا α',4,[(2,4)])
        iface.selections = [(2,4)]
        for items in ([node(43,'Copy shell command',('showing','focused')),editor],
                      [refresh,node(29,'Yard name',('showing','editable'))], [refresh,editor,editor]):
            with self.subTest(items=items), self.assertRaisesRegex(AssertionError,'focus_target_unavailable'):
                driver.editor_continuity(items,77,atspi,editor,'庭院 مرحبا α',4,[(2,4)])

    def test_retained_overview_text_observes_update_without_hidden_name_or_partial_timestamp(self):
        spec = importlib.util.spec_from_file_location('gtk_input', ROOT / 'veranda/desktop/wayland-input.py')
        driver = importlib.util.module_from_spec(spec); spec.loader.exec_module(driver)
        stamp = '2026-10-19T16:00:00.123456789Z'
        atspi = SimpleNamespace(Role=SimpleNamespace(LABEL=29),
            StateType=SimpleNamespace(SHOWING='showing', DEFUNCT='defunct'))
        def node(pid=77, role=29, states=('showing',), contents='Owner: synthetic\nObserved: '+stamp):
            value = SimpleNamespace(pid=pid, role=role, states=set(states), contents=contents, hidden=False)
            value.get_process_id=lambda:value.pid; value.get_role=lambda:value.role
            value.get_state_set=lambda:SimpleNamespace(contains=lambda state:state in value.states)
            value.get_name=lambda:'' if value.hidden else value.contents
            value.get_text_iface=lambda:value
            return value
        detail=node()
        atspi.Text=SimpleNamespace(get_character_count=lambda interface:len(interface.contents),
            get_text=lambda interface,start,end:interface.contents[start:end])
        with patch.object(driver,'navigation_context',return_value={'scope':'owner','heading':True}):
            self.assertIs(driver.overview_observed_label([detail],77,'owner',atspi),detail)
            for items in ([],[node(pid=88)],[node(states=('showing','defunct'))],[node(contents='No observation')],
                          [detail,node()],[node(role=61)]):
                with self.subTest(items=items):self.assertIsNone(driver.overview_observed_label(items,77,'owner',atspi))
        with patch.object(driver,'navigation_context',return_value={'scope':'other','heading':True}), \
                self.assertRaisesRegex(AssertionError,'local_scope_changed'):
            driver.overview_observed_label([detail],77,'owner',atspi)
        detail.hidden=True;detail.states.clear()
        with patch.object(detail,'get_name',side_effect=AssertionError('hidden Name must not be read')), \
                patch.object(driver,'process_identity') as unused, patch('builtins.print') as output:
            checks=[];owned=lambda:checks.append(True)
            self.assertEqual(driver.retained_observed(detail,77,atspi,owned),stamp)
            detail.contents='Owner: synthetic\nObserved: 2026-10-19T16:00:01Z'
            self.assertEqual(driver.retained_observed(detail,77,atspi,owned),'2026-10-19T16:00:01Z')
            with patch.object(atspi.Text,'get_text',return_value=detail.contents[:-1]):
                self.assertIsNone(driver.retained_observed(detail,77,atspi,owned))
            detail.contents='Owner: synthetic\nObserved: 2026-10-19T16:00:'
            self.assertIsNone(driver.retained_observed(detail,77,atspi,owned))
            self.assertEqual(len(checks),4);output.assert_not_called();unused.assert_not_called()
        for invalid in (node(pid=88),node(role=61),node(states=('defunct',))):
            with self.subTest(invalid=invalid), self.assertRaisesRegex(AssertionError,'focus_target_unavailable'):
                driver.retained_observed(invalid,77,atspi,lambda:None)
        with self.assertRaisesRegex(AssertionError,'GUI identity changed'):
            driver.retained_observed(detail,77,atspi,lambda:(_ for _ in ()).throw(AssertionError('GUI identity changed')))

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
        for fault in ('terminal-suffix-growth', 'nonterminal-suffix-growth', 'prefix-name', 'prefix-owner',
                      'target-owner', 'duplicate-target', 'changed-target', 'lost-focus', 'unexpected-focus'):
            owners = [True, False, True, False]
            state = {'order': order, 'owners': owners, 'focused': 3}
            commands, trace = [], []
            def send(key):
                commands.append(key)
                state['focused'] = 0 if key == 'Home' else state['focused'] + 1
                if fault == 'nonterminal-suffix-growth' and key == 'Home':
                    state['order'] = order + ['Remote owner / fleet-01']
                    state['owners'] = owners + [False]
                elif state['focused'] == 2:
                    if fault == 'terminal-suffix-growth':
                        state['order'] = order + ['Remote owner / fleet-01']
                        state['owners'] = owners + [False]
                    elif fault == 'prefix-name': state['order'] = [order[0], 'Changed prefix', *order[2:]]
                    elif fault == 'prefix-owner': state['owners'] = [False, *owners[1:]]
                    elif fault == 'target-owner': state['owners'] = [*owners[:2], False, owners[3]]
                    elif fault == 'duplicate-target':
                        state['order'] = order + ['Remote owner']; state['owners'] = owners + [True]
                    elif fault == 'changed-target': state['order'] = [*order[:2], 'Changed owner', order[3]]
                    elif fault == 'lost-focus':
                        state.update(focused=None, selected=2, heading=True)
                    elif fault == 'unexpected-focus': state['focused'] = 1
            with self.subTest(fault=fault):
                if fault == 'terminal-suffix-growth':
                    driver.navigate_owner(lambda: dict(state), send, 'Remote owner', trace)
                else:
                    with self.assertRaisesRegex(AssertionError, 'focus_target_unavailable'):
                        driver.navigate_owner(lambda: dict(state), send, 'Remote owner', trace)
                self.assertEqual(commands, ['Home'] if fault == 'nonterminal-suffix-growth' else ['Home', 'Down', 'Down'])
                self.assertEqual(len(trace), len(commands) + 1)  # Retain the rejected normal snapshot, without another read.
                self.assertNotIn('physical_key', trace[-1])

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
        def button(name, pid=7, role=43, showing=True, focused=True):
            return SimpleNamespace(get_process_id=lambda: pid, get_role=lambda: role, get_name=lambda: name,
                get_state_set=lambda: SimpleNamespace(contains=lambda state: (state=='focused' and focused) or (state=='showing' and showing)))
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
            for nodes in ([button(name,pid=8)], [button(name,role=29)], [button(name,showing=False)],
                          [button(name,focused=False)], [node,button(name)]):
                namespace={'focus':lambda *args,**kwargs:None, 'nodes':lambda observe,site:observe(nodes),
                           'pid':7, 'Atspi':atspi, 'focused_button':driver.focused_button,
                           'keys':lambda *args:commands.append(args)}
                exec(compile(ast.Module(body=[activate],type_ignores=[]),'actual-activate','exec'),namespace)
                with self.assertRaises(AssertionError): namespace['activate'](name)
                self.assertEqual(commands, [])
            namespace['nodes']=lambda observe,site:observe([node])
            namespace['activate'](name)
            self.assertEqual(commands,[('-k','space')])
            commands.clear()
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
        atspi = SimpleNamespace(Role=SimpleNamespace(LIST=31, LIST_ITEM=32, LABEL=29, PAGE_TAB=37),
            StateType=SimpleNamespace(SHOWING='showing', FOCUSED='focused', DEFUNCT='defunct', SELECTED='selected'))
        class Node:
            def __init__(self, role, name='', states=('showing',)):
                self.role,self.name,self.states,self.children,self.parent=role,name,set(states),[],None
                self.child_reads=0
            def get_role(self): return self.role
            def get_name(self): return self.name
            def get_process_id(self): return 7
            def get_parent(self): return self.parent
            def get_child_count(self): return len(self.children)
            def get_child_at_index(self,index):
                self.child_reads+=1
                return self.children[index]
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
        self.assertEqual(nav.child_reads,2)  # Selection reuses the validated rows, not a second fetch.
        rows[1].states.remove('selected')
        with self.assertRaises(AssertionError):driver.navigation_context(items,7,atspi)
        rows[1].states.add('selected')
        rows[1].states.remove('selected');rows[0].states.add('selected');heading.name='owner / default'
        self.assertTrue(driver.selected_scope(driver.navigation_context(items,7,atspi),'owner / default'))
        button.states.remove('focused')  # Cancel can hide the previously focused panel.
        snapshot=driver.navigation_context(items,7,atspi)
        self.assertIsNone(snapshot['focused'])
        self.assertTrue(driver.selected_scope(snapshot,'owner / default'))
        button.states.add('focused')
        projects = next(node for node in ast.walk(ast.parse((ROOT / 'veranda/desktop/wayland-input.py').read_text()))
                        if isinstance(node, ast.FunctionDef) and node.name == 'projects')
        harness=ast.parse("def harness():\n stage=''\n return projects\n")
        harness.body[0].body.insert(1,projects)
        baseline=driver.navigation_context(items,7,atspi)
        for fault in ('valid','lost-focus','multiple-focus','foreign','hidden','wrong-role','stale-scope'):
            tab=Node(37,'Projects',('showing','focused'));tab.parent=workspace
            actual=[node for node in items if node is not button]+[tab]
            if fault=='lost-focus':tab.states.remove('focused')
            elif fault=='multiple-focus':actual.append(Node(37,'Projects',('showing','focused')))
            elif fault=='foreign':tab.get_process_id=lambda:8
            elif fault=='hidden':tab.states.remove('showing')
            elif fault=='wrong-role':tab.role=29
            elif fault=='stale-scope':actual.remove(heading)
            actual.extend(Node(29,f'project-00-{index:02}') for index in range(10))
            commands=[]
            namespace={'scope_snapshot':lambda site:dict(baseline),'selected_scope':driver.selected_scope,
                       'navigation_context':driver.navigation_context,'focus_matches':driver.focus_matches,
                       'workspace_tab_ready':driver.workspace_tab_ready,
                       'focus':lambda *args,**kwargs:None,'nodes':lambda observe,site:observe(actual),
                       'pid':7,'Atspi':atspi,'keys':lambda *args:commands.append(args),
                       'wait':lambda predicate:self.assertTrue(predicate())}
            exec(compile(harness,'actual-projects','exec'),namespace)
            with self.subTest(project_activation=fault):
                if fault=='valid':namespace['harness']()('owner / default','test')
                else:
                    with self.assertRaises(AssertionError):namespace['harness']()('owner / default','test')
                self.assertEqual(commands,[('-k','space')] if fault=='valid' else [])
        heading.name='wrong context'
        self.assertFalse(driver.selected_scope(driver.navigation_context(items,7,atspi),'owner / default'))
        rows[1].states.add('selected')
        with self.assertRaises(AssertionError):driver.navigation_context(items,7,atspi)

        ui_tab=next(node for node in ast.walk(ast.parse((ROOT/'veranda/desktop/wayland-input.py').read_text()))
                    if isinstance(node,ast.FunctionDef) and node.name=='tab')
        owner=Node(32,states=('showing','selected','focused'));owner_label=Node(29,'owner')
        owner.parent=nav;owner.children=[owner_label];owner_label.parent=owner
        nav.children=[owner,*rows]
        for fault in ('valid','lost-focus','multiple-focus','foreign','hidden','wrong-role','stale-scope'):
            for row in rows:row.states={'showing'}
            owner.states={'showing','selected','focused'};heading.name='owner'
            tab=Node(37,'Yards');tab.parent=workspace
            refresh=Node(43,'Refresh');refresh.parent=workspace
            actual=[nav,workspace,heading,owner,owner_label,*rows,*labels,tab,refresh];commands=[]
            def focus(name,workspace=False,expected_scope=None):
                self.assertTrue(workspace);self.assertEqual(expected_scope,'owner')
                def read():
                    context=driver.navigation_context(actual,7,atspi)
                    self.assertTrue(driver.selected_scope(context,expected_scope))
                    return dict(context,matched=driver.focus_matches(actual,7,name,atspi))
                def send(key):
                    commands.append(key)
                    if key=='F6':owner.states.remove('focused');refresh.states.add('focused')
                    else:refresh.states.remove('focused');tab.states.add('focused')
                driver.focus_control(read,send,name)
                if fault=='lost-focus':tab.states.remove('focused')
                elif fault=='multiple-focus':refresh.states.add('focused')
                elif fault=='foreign':tab.get_process_id=lambda:8
                elif fault=='hidden':tab.states.remove('showing')
                elif fault=='wrong-role':tab.role=29
                elif fault=='stale-scope':owner.states.remove('selected');rows[0].states.add('selected');heading.name='owner / default'
            namespace={'focus':focus,'nodes':lambda observe,site:observe(actual),'pid':7,'Atspi':atspi,
                       'workspace_tab_ready':driver.workspace_tab_ready,'keys':lambda *args:commands.append(args)}
            exec(compile(ast.Module(body=[ui_tab],type_ignores=[]),'actual-ui-tab','exec'),namespace)
            with self.subTest(ui_tab=fault):
                if fault=='valid':namespace['tab']('Yards','owner')
                else:
                    with self.assertRaises(AssertionError):namespace['tab']('Yards','owner')
                self.assertEqual(commands,['F6','Tab',('-k','space')] if fault=='valid' else ['F6','Tab'])

        for fault in ('valid','changed-selection','f6-stays','post-f6-reentry'):
            for row in rows:row.states={'showing'}
            owner.states={'showing','selected'};heading.name='owner'
            tab=Node(37,'Sync');tab.parent=workspace
            overview=Node(37,'Overview');overview.parent=workspace
            refresh=Node(43,'Refresh',('showing','focused'));refresh.parent=workspace
            actual=[nav,workspace,heading,owner,owner_label,*rows,*labels,tab,overview,refresh];commands=[]
            def focus(name,workspace=False,expected_scope=None):
                self.assertTrue(workspace);self.assertEqual(expected_scope,'owner')
                def read():
                    context=driver.navigation_context(actual,7,atspi)
                    self.assertTrue(driver.selected_scope(context,expected_scope))
                    return dict(context,matched=driver.focus_matches(actual,7,name,atspi))
                def send(key):
                    commands.append(key)
                    if key=='F6':
                        if fault!='f6-stays':owner.states.remove('focused');overview.states.add('focused')
                    elif 'focused' in refresh.states:
                        refresh.states.remove('focused');owner.states.add('focused')
                        if fault=='changed-selection':
                            owner.states.remove('selected');rows[0].states.add('selected');heading.name='owner / default'
                    else:
                        overview.states.remove('focused')
                        (owner if fault=='post-f6-reentry' else tab).states.add('focused')
                driver.focus_control(read,send,name)
            namespace={'focus':focus,'nodes':lambda observe,site:observe(actual),'pid':7,'Atspi':atspi,
                       'workspace_tab_ready':driver.workspace_tab_ready,'keys':lambda *args:commands.append(args)}
            exec(compile(ast.Module(body=[ui_tab],type_ignores=[]),'actual-header-to-ui-tab','exec'),namespace)
            with self.subTest(header_ui_tab=fault):
                if fault=='valid':namespace['tab']('Sync','owner')
                else:
                    with self.assertRaises(AssertionError):namespace['tab']('Sync','owner')
                self.assertEqual(commands,['Tab','F6','Tab',('-k','space')] if fault=='valid'
                                 else ['Tab'] if fault=='changed-selection' else ['Tab','F6']
                                 if fault=='f6-stays' else ['Tab','F6','Tab'])

        host=next(node for node in ast.walk(ast.parse((ROOT/'veranda/desktop/wayland-input.py').read_text()))
                  if isinstance(node,ast.FunctionDef) and node.name=='host')
        for selected_host in (False,True):
            owner.states={'showing'};rows[0].states={'showing'};rows[1].states={'showing'}
            (owner if selected_host else rows[0]).states.add('selected')
            heading.name='owner' if selected_host else 'owner / default'
            actual=[nav,workspace,heading,owner,owner_label,*rows,*labels,Node(29,'Connected'),Node(29,'20 yards')]
            observed=[];namespace={'focus':lambda *args,**kwargs:None,'destination':'owner','pid':7,'Atspi':atspi,
                'navigation_context':driver.navigation_context,'selected_scope':driver.selected_scope,
                'nodes':lambda observe,site:observe(actual),'wait':lambda predicate:observed.append(predicate())}
            exec(compile(ast.Module(body=[host],type_ignores=[]),'actual-ui-host','exec'),namespace)
            namespace['host']();self.assertEqual(observed,[selected_host])

    def test_navigation_snapshot_rejects_unowned_or_ambiguous_lists_before_input(self):
        spec = importlib.util.spec_from_file_location('gtk_input', ROOT / 'veranda/desktop/wayland-input.py')
        driver = importlib.util.module_from_spec(spec); spec.loader.exec_module(driver)
        atspi = SimpleNamespace(Role=SimpleNamespace(LIST=31, LIST_ITEM=32, LABEL=29),
                                StateType=SimpleNamespace(SHOWING='showing', FOCUSED='focused', DEFUNCT='defunct', SELECTED='selected'))
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
                      'label-parent', 'hidden', 'defunct', 'duplicate-list', 'duplicate-row', 'multiple-focus',
                      'foreign-focus', 'hidden-focus', 'defunct-focus'):
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
            elif fault.endswith('-focus'):
                row.states.remove('focused')
                focus=Node(43,'Refresh',states=('showing','focused'))
                if fault=='foreign-focus':focus.pid=8
                elif fault=='hidden-focus':focus.states.remove('showing')
                else:focus.states.add('defunct')
                items.append(focus)
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

    def test_connected_owner_requires_all_labels_in_one_observation(self):
        ready=next(node for node in ast.walk(ast.parse((ROOT/'veranda/desktop/wayland-input.py').read_text()))
                   if isinstance(node,ast.FunctionDef) and node.name=='connected_owner')
        with patch('builtins.print'):
            observations=iter([{'remote'}, {'Connected','20 yards'}, {'remote','Connected','20 yards'}])
            reads=[]
            def names():
                reads.append(True)
                return next(observations)
            namespace={'names':names,'destination':'remote'}
            exec(compile(ast.Module(body=[ready],type_ignores=[]),'actual-connected-owner','exec'),namespace)
            self.assertFalse(namespace['connected_owner']())
            self.assertEqual(len(reads),1)
            self.assertFalse(namespace['connected_owner']())
            self.assertEqual(len(reads),2)
            self.assertTrue(namespace['connected_owner']())
            self.assertEqual(len(reads),3)

    def test_input_progress_is_bounded_deduplicated_cached_and_excludes_text(self):
        spec = importlib.util.spec_from_file_location('gtk_input', ROOT / 'veranda/desktop/wayland-input.py')
        driver = importlib.util.module_from_spec(spec); spec.loader.exec_module(driver)
        result={'physical_key_commands':0,'selection_trace':[{'selected':1,'focused':None,'heading':True,'scope':'private text'}]}
        snapshot={'truncated':False};cached=[{'role':37,'name':'private text','states':['FOCUSED']}];state={}
        with patch('builtins.print') as output, patch.object(driver,'owned_tree') as ui_read, \
             patch.object(driver.time,'monotonic') as clock:
            clock.return_value=10
            for _ in range(3):driver.input_progress(result,'first-key-decline','read-start','named-focus:private text','none',snapshot,cached,state)
            self.assertEqual(output.call_count,1)
            self.assertEqual(json.loads(output.call_args.args[0].split(' ',1)[1])['cached_focused_roles'],[37])
            clock.return_value=12.5
            driver.input_progress(result,'first-key-decline','read-start','named-focus:private text','none',snapshot,cached,state)
            self.assertEqual(output.call_count,1)  # Time alone cannot defeat deduplication.
            for index in range(600):
                result['physical_key_commands']=index
                driver.input_progress(result,'first-key-decline','key-start','navigation','Tab',snapshot,cached,state)
            self.assertEqual(output.call_count,512)
            frames=[call.args[0] for call in output.call_args_list]
            self.assertTrue(all(len(frame.encode())<=1050 and 'private text' not in frame for frame in frames))
            self.assertTrue(json.loads(frames[-1].split(' ',1)[1])['truncated'])
            self.assertEqual(json.loads(frames[-1].split(' ',1)[1])['elapsed_seconds'],2.5)
            self.assertTrue(all(call.kwargs=={'flush':True} for call in output.call_args_list))
            ui_read.assert_not_called()
        # Execute the actual coherent nodes path: progress cannot add a read.
        nodes=next(node for node in ast.walk(ast.parse((ROOT/'veranda/desktop/wayland-input.py').read_text()))
                   if isinstance(node,ast.FunctionDef) and node.name=='nodes')
        reads=[];namespace={'progress':lambda *args:None,'coherent_observation':driver.coherent_observation,
            'read_tree':lambda end:reads.append(end) or [],'read_deadline':None,'time':driver.time,
            'result':{},'GLib':SimpleNamespace(Error=RuntimeError)}
        exec(compile(ast.Module(body=[nodes],type_ignores=[]),'actual-nodes','exec'),namespace)
        self.assertEqual(namespace['nodes'](len,'navigation'),0)
        self.assertEqual(len(reads),1)

    def test_ime_is_two_local_lifetimes_and_conflicting_flags_fail_before_native_work(self):
        spec = importlib.util.spec_from_file_location('gtk_short', ROOT / 'veranda/desktop/wayland-short.py')
        short = importlib.util.module_from_spec(spec); spec.loader.exec_module(short)
        self.assertEqual(short.short_scenarios(False, False, True), ['ime', 'ime-restore'])
        self.assertEqual(short.keyboard_lifetime_ms(['ime', 'ime-restore'], short.workload_budget(False, False)), 240000)
        for flag in ('--remote', '--ui', '--owner-focus'):
            with self.subTest(flag=flag), patch.object(sys, 'argv', ['short', '/unused/gui', '/unused/engine', '--ime', flag]), \
                 patch.object(short.subprocess, 'run') as native, patch.object(short.subprocess, 'check_output') as query, \
                 contextlib.redirect_stderr(io.StringIO()):
                with self.assertRaises(SystemExit) as error: short.main()
                self.assertEqual(error.exception.code, 2)
                native.assert_not_called(); query.assert_not_called()

    def test_delayed_ime_setup_does_not_consume_window_manager_startup_budget(self):
        spec = importlib.util.spec_from_file_location('gtk_short', ROOT / 'veranda/desktop/wayland-short.py')
        short = importlib.util.module_from_spec(spec); spec.loader.exec_module(short)
        tree = ast.parse((ROOT / 'veranda/desktop/wayland-short.py').read_text())
        # Run the actual orchestration from IME setup through WM readiness.
        setup = next(node for node in ast.walk(tree) if isinstance(node, ast.If)
                     and any(isinstance(call, ast.Call) and isinstance(call.func, ast.Name)
                             and call.func.id == 'prepare_ime' for call in ast.walk(node)))
        body = next(node.body for node in ast.walk(tree) if isinstance(node, ast.With) and setup in node.body)
        start = body.index(setup)
        stop = next(index for index in range(start, len(body))
                    if isinstance(body[index], ast.Assign)
                    and any(isinstance(target, ast.Subscript) and isinstance(target.slice, ast.Constant)
                            and target.slice.value == 'lifetimes' for target in body[index].targets))
        now = [0.0]; live = SimpleNamespace(poll=lambda: None); setup_calls = []
        def delayed_setup(*args): setup_calls.append(True); now[0] += 15
        namespace = dict(vars(short), args=SimpleNamespace(ime=True, x11=True, remote=False, owner_focus=False, ui=False),
            state=Path('/unused'), env={}, result={}, display=live, x11_deadline=10,
            service=lambda *args: (live, None), prepare_ime=delayed_setup)
        with patch.object(short.time, 'monotonic', side_effect=lambda: now[0]), \
             patch.object(short.fixture, 'private_log', side_effect=lambda path: contextlib.nullcontext(io.StringIO())), \
             patch.object(short.subprocess, 'run', return_value=SimpleNamespace(returncode=0, stdout='4\n', stderr='')) as query:
            exec(compile(ast.Module(body=body[start:stop], type_ignores=[]), 'actual-ime-wm-startup', 'exec'), namespace)
        self.assertEqual(setup_calls, [True])
        self.assertEqual(namespace['x11_deadline'] - now[0], 10)
        self.assertEqual(query.call_args.args[0], ['xdotool', 'get_num_desktops'])
        self.assertEqual(query.call_args.kwargs['timeout'], 1)

    def test_owner_startup_waits_for_connected_before_strict_scope_and_projects(self):
        spec = importlib.util.spec_from_file_location('gtk_input', ROOT / 'veranda/desktop/wayland-input.py')
        driver = importlib.util.module_from_spec(spec); spec.loader.exec_module(driver)
        tree = ast.parse((ROOT / 'veranda/desktop/wayland-input.py').read_text())
        scope_wait = next(node for node in ast.walk(tree) if isinstance(node, ast.FunctionDef) and node.name == 'wait_scope')
        atspi = SimpleNamespace(Role=SimpleNamespace(LIST=31, LIST_ITEM=32, LABEL=29, PANEL=39),
                                StateType=SimpleNamespace(SHOWING='showing', SELECTED='selected', FOCUSED='focused', DEFUNCT='defunct'))
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
        for scenario in ('connect', 'owner-focus', 'remote-restore', 'local-restore'):
            branch = next(node for node in ast.walk(tree) if isinstance(node, ast.If)
                          and ast.unparse(node.test) == 'scenario == ' + repr(scenario))
            restoring = scenario in ('remote-restore', 'local-restore')
            stop = (next(index for index, node in enumerate(branch.body) if isinstance(node, ast.Assign)
                         and any(isinstance(target, ast.Subscript) and isinstance(target.slice, ast.Constant)
                                 and target.slice.value in ('remote_owner_yard_tab_restored', 'local_owner_yard_tab_restored')
                                 for target in node.targets)) + 1) if restoring else 3
            for fault in ('valid', 'wrong-scope', 'malformed', 'unowned'):
                with self.subTest(scenario=scenario, fault=fault):
                    expected_scope = ('remote' if scenario == 'remote-restore' else 'Local owner') + ' / default'
                    nav = Node(31, 'Owner hosts and yards')
                    row = Node(32, expected_scope if fault != 'wrong-scope' else 'Other owner / default', ('showing', 'selected'))
                    label = Node(29, 'default'); heading = Node(29, row.name); panel = Node(39)
                    nav.children = [row]; row.parent = nav; row.children = [label]; label.parent = row; heading.parent = panel
                    items = [nav, row, label, heading, panel]
                    if fault == 'malformed': nav.children = []
                    if fault == 'unowned': row.pid = 8
                    calls, commands, result = [], [], {}
                    ready = [False]
                    projects = {f'project-00-{index:02}' for index in range(10)}
                    def names():
                        calls.append('names'); ready[0] = calls.count('names') >= 3
                        return ({'Connected', *projects} if ready[0] else
                                {'Connecting…' if calls.count('names') == 1 else 'Inventory changed; refreshing…'})
                    def snapshot(site):
                        calls.append('scope')
                        return driver.navigation_context(items if ready[0] else [], 7, atspi)
                    def wait(predicate):
                        for attempt in range(5):
                            value = predicate()
                            if value: return value
                        raise TimeoutError('startup')
                    namespace = dict(vars(driver), scenario=scenario, destination='remote', names=names,
                                     scope_snapshot=snapshot, wait=wait, result=result, keys=commands.append)
                    actual = compile(ast.Module(body=[scope_wait, *branch.body[:stop]], type_ignores=[]), 'actual-restore-startup', 'exec')
                    if fault == 'valid':
                        exec(actual, namespace)
                        self.assertEqual(calls[:4], ['names', 'names', 'names', 'scope'])
                        if restoring:
                            self.assertTrue(result['remote_owner_yard_tab_restored' if scenario == 'remote-restore' else 'local_owner_yard_tab_restored'])
                            self.assertGreater(calls.count('names'), 3)  # Actual project-name predicate ran.
                        else:
                            self.assertEqual(calls, ['names', 'names', 'names', 'scope'])
                    else:
                        with self.assertRaises(TimeoutError if fault == 'wrong-scope' else AssertionError):
                            exec(actual, namespace)
                        self.assertFalse(result)
                    self.assertEqual(commands, [])

    def test_ime_startup_waits_through_empty_tree_before_strict_scope_in_both_lifetimes(self):
        spec = importlib.util.spec_from_file_location('gtk_input', ROOT / 'veranda/desktop/wayland-input.py')
        driver = importlib.util.module_from_spec(spec); spec.loader.exec_module(driver)
        tree = ast.parse((ROOT / 'veranda/desktop/wayland-input.py').read_text())
        branch = next(node for node in ast.walk(tree) if isinstance(node, ast.If)
                      and isinstance(node.test, ast.Compare) and isinstance(node.test.comparators[0], ast.Tuple)
                      and ast.literal_eval(node.test.comparators[0]) == ('ime', 'ime-restore'))
        stop = next(index for index, node in enumerate(branch.body) if isinstance(node, ast.Assign)
                    and isinstance(node.value, ast.Constant) and node.value.value == 'ime-yard-name-editor')
        scope_wait = next(node for node in ast.walk(tree) if isinstance(node, ast.FunctionDef) and node.name == 'wait_scope')
        atspi = SimpleNamespace(Role=SimpleNamespace(LIST=31))
        for scenario in ('ime', 'ime-restore'):
            with self.subTest(scenario=scenario):
                startup_reads = []; scopes = []; ready = [False]
                scope = ['Local owner / default' if scenario == 'ime' else 'Local owner']
                def names():
                    startup_reads.append(True)
                    ready[0] = len(startup_reads) >= 3
                    return {'Connected'} if ready[0] else set()
                def snapshot(site):
                    if not ready[0]:
                        return driver.navigation_context([], 7, atspi)
                    scopes.append(scope[0])
                    return {'scope': scope[0], 'heading': True}
                def wait(predicate):
                    for attempt in range(5):
                        value = predicate()
                        if value: return value
                    raise TimeoutError('startup')
                def focus(name, owner_row=False):
                    self.assertEqual((name, owner_row), ('Local owner', True))
                    scope[0] = name
                namespace = dict(vars(driver), scenario=scenario, names=names, scope_snapshot=snapshot,
                                 wait=wait, focus=focus)
                exec(compile(ast.Module(body=[scope_wait, *branch.body[:stop]], type_ignores=[]),
                             'actual-ime-startup', 'exec'), namespace)
                self.assertGreaterEqual(len(startup_reads), 3)
                self.assertEqual(scopes, ['Local owner / default', 'Local owner'] if scenario == 'ime' else ['Local owner'])

    def test_ime_uses_physical_two_set_keys_and_rejects_early_or_wrong_commits(self):
        spec = importlib.util.spec_from_file_location('gtk_input', ROOT / 'veranda/desktop/wayland-input.py')
        driver = importlib.util.module_from_spec(spec); spec.loader.exec_module(driver)
        toggle = ('-M', 'shift', '-k', 'space', '-m', 'shift')
        for readings, accepted in ((['', '', '', '\uac00'], True), (['', 'r'], False), (['', '', '', 'rk'], False)):
            with self.subTest(accepted=accepted):
                values = iter(readings); keys = []; captures = []; persisted = []
                def wait(predicate): self.assertTrue(predicate())
                def execute():
                    driver.compose_ime(lambda *args: keys.append(args), lambda: next(values), wait,
                        lambda *args: captures.append(args), lambda: persisted.append(True) or True)
                if accepted: execute()
                else:
                    with self.assertRaises(AssertionError): execute()
                self.assertEqual(keys[:2], [toggle, ('-k', 'r')])
                if accepted:
                    self.assertEqual(keys, [toggle, ('-k', 'r'), ('-k', 'k'), toggle])
                    self.assertEqual(captures, [('consonant', ''), ('syllable', ''), ('committed', '\uac00')])
                    self.assertEqual(persisted, [True])
                else:
                    self.assertNotIn(('committed', '\uac00'), captures)
                    self.assertEqual(persisted, [])

    def test_ime_draft_equality_requires_exact_local_owner_scope_and_field(self):
        spec = importlib.util.spec_from_file_location('gtk_input', ROOT / 'veranda/desktop/wayland-input.py')
        driver = importlib.util.module_from_spec(spec); spec.loader.exec_module(driver)
        with tempfile.TemporaryDirectory(prefix='veranda-ime-draft-', dir='/tmp') as temporary:
            root = Path(temporary); root.chmod(0o700)
            self.assertTrue(driver.local_ime_draft(root, ''))
            self.assertFalse(driver.local_ime_draft(root))
            for scope, field, value, expected in (({'connection': None, 'yard': None}, 'create:name', '\uac00', True),
                    ({'connection': 'remote', 'yard': None}, 'create:name', '\uac00', False),
                    ({'connection': None, 'yard': 'default'}, 'create:name', '\uac00', False),
                    ({'connection': None, 'yard': None}, 'setting:LABEL', '\uac00', False),
                    ({'connection': None, 'yard': None}, 'create:name', 'rk', False)):
                (root / 'desktop.json').write_text(json.dumps({'drafts': [{'scope': scope, 'field': field, 'value': value}]}))
                (root / 'desktop.json').chmod(0o600)
                self.assertEqual(driver.local_ime_draft(root), expected)
            (root / 'desktop.json').write_text(json.dumps({'drafts': []}))
            self.assertTrue(driver.local_ime_draft(root, ''))
            (root / 'desktop.json').chmod(0o644)
            with self.assertRaisesRegex(AssertionError, 'ime_draft_unavailable'):
                driver.local_ime_draft(root, '')

    def test_ime_setup_checks_real_script_readback_and_owns_a_foreground_private_daemon(self):
        spec = importlib.util.spec_from_file_location('gtk_short', ROOT / 'veranda/desktop/wayland-short.py')
        short = importlib.util.module_from_spec(spec); spec.loader.exec_module(short)
        for fault in ('none', 'settings', 'engine'):
            with self.subTest(fault=fault), tempfile.TemporaryDirectory(prefix='veranda-ime-setup-', dir='/tmp') as temporary:
                state = Path(temporary); state.chmod(0o700); settings = {}; services = []; env = {}; evidence = {}
                names = ('libim-ibus.so', 'ibus-engine-hangul', 'org.freedesktop.ibus.engine.hangul.gschema.xml')
                for name in names: (state / name).write_bytes(b'host-free metadata fixture')
                def run(command, **kwargs):
                    if command[0] == 'gsettings': settings[tuple(command[2:4])] = command[4]
                    else: self.assertEqual(command, ['ibus', 'engine', 'hangul'])
                    return SimpleNamespace(returncode=0)
                def query(command, **kwargs):
                    if command[:2] == ['gsettings', 'get']:
                        return "'wrong'" if fault == 'settings' else settings[tuple(command[2:4])]
                    if command[:2] == ['ibus', 'engine']: return 'wrong' if fault == 'engine' else 'hangul'
                    if command[:2] == ['dpkg-query', '-W']: return 'ibus fixture-version'
                    self.assertEqual(command[:2], ['dpkg-query', '-L'])
                    return '\n'.join(str(state / name) for name in names)
                def service(name, command):
                    services.append((name, command))
                    return SimpleNamespace(poll=lambda: None), None
                with patch.object(short.subprocess, 'run', side_effect=run), \
                     patch.object(short.subprocess, 'check_output', side_effect=query), patch.object(Path, 'is_socket', return_value=True):
                    if fault == 'none': short.prepare_ime(state, env, service, evidence)
                    else:
                        with self.assertRaisesRegex(AssertionError, 'ime_.*readback_mismatch'):
                            short.prepare_ime(state, env, service, evidence)
                if fault == 'settings': self.assertEqual(services, [])
                else:
                    self.assertEqual(len(services), 1)
                    name, command = services[0]
                    self.assertEqual(name, 'ibus'); self.assertIn('--single', command)
                    self.assertNotIn('--daemonize', command); self.assertNotIn('--replace', command)
                    self.assertIn('--address=unix:path=' + str(state / 'ibus-bus'), command)
                if fault == 'none':
                    self.assertEqual(evidence['stage'], 'ready')
                    self.assertEqual(env['GTK_IM_MODULE'], 'ibus')
                    self.assertEqual(env['GSETTINGS_BACKEND'], 'keyfile')
                    self.assertEqual(evidence['settings']['use-system-keyboard-layout'], 'true')

    def test_functional_workload_budgets_distinguish_expanded_remote_ui(self):
        spec = importlib.util.spec_from_file_location('gtk_short', ROOT / 'veranda/desktop/wayland-short.py')
        short = importlib.util.module_from_spec(spec); spec.loader.exec_module(short)
        self.assertEqual([short.workload_budget(remote, ui) for remote, ui in
            ((False, False), (False, True), (True, False), (True, True))], [90, 90, 360, 600])
        lifetimes = []
        for remote, ui, owner_focus in ((False, False, False), (False, True, False),
                (False, False, True), (True, False, False), (True, True, False)):
            scenarios = short.short_scenarios(remote, owner_focus)
            workload = short.workload_budget(remote, ui)
            lifetime = short.keyboard_lifetime_ms(scenarios, workload)
            self.assertGreaterEqual(lifetime, len(scenarios) * (workload + 15) * 1000)
            self.assertLessEqual(lifetime * 1000, 2**31 - 1)  # wtype converts milliseconds to microseconds.
            lifetimes.append(lifetime)
        self.assertEqual(lifetimes, [120000, 120000, 120000, 1170000, 1890000])

    def test_package_wayland_close_rejects_foreign_ambiguous_inactive_or_changed_owner(self):
        spec = importlib.util.spec_from_file_location('gtk_package', ROOT / 'dev/e2e/veranda-package-ui.py')
        package = importlib.util.module_from_spec(spec); spec.loader.exec_module(package)
        for fault in ('foreign-process', 'foreign-window', 'ambiguous', 'inactive', 'changed-owner', 'none'):
            with self.subTest(fault=fault):
                window = fixture.probe.WlrToplevel('/synthetic/wayland', 'org.subyard.Veranda')
                guards = []
                def owned():
                    guards.append(True)
                    if fault == 'foreign-process' or fault == 'changed-owner' and len(guards) > 1:
                        raise AssertionError('owned_gui_identity_changed')
                def command(action, *matches):
                    if action == 'list':
                        return {'foreign-window': 'foreign: title\n',
                                'ambiguous': 'org.subyard.Veranda: one\norg.subyard.Veranda: two\n'}.get(fault, 'org.subyard.Veranda: title\n')
                    if action == 'find':
                        self.assertEqual(matches, ('app_id:org.subyard.Veranda', 'state:unminimized', 'state:active'))
                        if fault == 'inactive': raise RuntimeError('no active window acknowledgement')
                    return ''
                with patch.object(window, 'command', side_effect=command), \
                     patch.object(fixture.probe.time, 'monotonic', side_effect=[0, 6] if fault == 'inactive' else None), \
                     patch.object(package.subprocess, 'run') as physical:
                    if fault == 'none':
                        receipt = package.wayland_close(SimpleNamespace(owned=owned), window)
                        self.assertTrue(receipt['activated_acknowledged'])
                        self.assertIn('protocol has no PID', receipt['binding'])
                        physical.assert_called_once_with(['wtype', '-s', '100', '-M', 'alt', '-k', 'F4', '-m', 'alt', '-s', '100'], check=True, timeout=3)
                        self.assertEqual(len(guards), 3)
                    else:
                        with self.assertRaises((RuntimeError, AssertionError)):
                            package.wayland_close(SimpleNamespace(owned=owned), window)
                        physical.assert_not_called()

    def test_package_wayland_nonempty_compositor_prevents_actual_app_launch(self):
        code = (ROOT / 'dev/e2e/veranda-package.sh').read_text().split("<<'PY'\n", 1)[1].split('\nPY\n', 1)[0]
        boundary = next(node for node in ast.walk(ast.parse(code)) if isinstance(node, ast.If)
            and any(isinstance(item, ast.Expr) and isinstance(item.value, ast.Call)
                    and isinstance(item.value.func, ast.Attribute) and item.value.func.attr == 'require_empty' for item in node.body))
        launch = next(node for node in ast.walk(ast.parse(code)) if isinstance(node, ast.Assign)
            and isinstance(node.value, ast.Call) and isinstance(node.value.func, ast.Name)
            and node.value.func.id == 'launch_logged' and isinstance(node.targets[0], ast.Tuple)
            and node.targets[0].elts[0].id == 'app')
        actual = compile(ast.Module(body=[boundary, launch], type_ignores=[]), 'actual-package-prelaunch', 'exec')
        for initial in ('foreign: existing\n', 'org.subyard.Veranda: existing\n', ''):
            with self.subTest(initial=bool(initial)), patch.object(fixture.probe.WlrToplevel, 'command', return_value=initial), \
                 patch.object(subprocess, 'run') as physical:
                launches = []
                namespace = {'backend': 'wayland', 'probe': fixture.probe,
                    'environment': {'WAYLAND_DISPLAY': '/synthetic/wayland'}, 'subprocess': subprocess,
                    'launch_logged': lambda *args, **kwargs: launches.append(args) or (None, None)}
                if initial:
                    with self.assertRaisesRegex(RuntimeError, 'initially empty'):
                        exec(actual, namespace)
                    self.assertEqual(launches, [])
                else:
                    exec(actual, namespace)
                    self.assertEqual(len(launches), 1)
                physical.assert_not_called()

    def test_package_cleanup_attempts_every_owned_tree_and_preserves_original_failure(self):
        code = (ROOT / 'dev/e2e/veranda-package.sh').read_text().split("<<'PY'\n", 1)[1].split('\nPY\n', 1)[0]
        parsed = ast.parse(code)
        stop = next(node for node in parsed.body if isinstance(node, ast.FunctionDef) and node.name == 'stop_owned')
        final = next(node.finalbody for node in ast.walk(parsed) if isinstance(node, ast.Try)
                     and node.finalbody and isinstance(node.finalbody[0], ast.Assign)
                     and any(isinstance(target, ast.Name) and target.id == 'original_failure' for target in node.finalbody[0].targets))
        for original in (False, True):
            with self.subTest(original=original), tempfile.TemporaryDirectory(prefix='veranda-package-cleanup-', dir='/tmp') as temporary:
                events = []
                def owned(name, fail=False):
                    def stop(process):
                        events.append((name, process))
                        if fail: raise RuntimeError('private cleanup detail')
                    return SimpleNamespace(stop=stop)
                namespace = {'cleanup_errors': [], 'sys': sys, 'contextlib': contextlib, 'display_state': Path(temporary),
                    'tree': owned('app', True), 'app': 1, 'services': [(2, owned('bus')), (3, owned('compositor'))],
                    'stderr_workers': [], '__import__': __import__}
                exec(compile(ast.Module(body=[stop], type_ignores=[]), 'actual-stop-owned', 'exec'), namespace)
                final_code = compile(ast.Module(body=final, type_ignores=[]), 'actual-package-cleanup', 'exec')
                with contextlib.redirect_stderr(io.StringIO()) as diagnostic:
                    if original:
                        with self.assertRaisesRegex(ValueError, 'original failure'):
                            try: raise ValueError('original failure')
                            finally: exec(final_code, namespace)
                    else:
                        with self.assertRaisesRegex(RuntimeError, 'fixture cleanup failed'):
                            exec(final_code, namespace)
                self.assertEqual(events, [('app', 1), ('compositor', 3), ('bus', 2)])
                self.assertNotIn('private', diagnostic.getvalue())

    def test_package_stderr_is_private_bounded_and_drained_during_wait(self):
        code=(ROOT/'dev/e2e/veranda-package.sh').read_text().split("<<'PY'\n",1)[1].split('\nPY\n',1)[0]
        launch=next(node for node in ast.parse(code).body if isinstance(node,ast.FunctionDef) and node.name=='launch_logged')
        with tempfile.TemporaryDirectory(prefix='veranda-package-stderr-',dir='/tmp') as temporary:
            state=Path(temporary);state.chmod(0o700)
            namespace={'os':os,'state':state,'phase':'install','threading':__import__('threading'),'subprocess':subprocess}
            exec(compile(ast.Module(body=[launch],type_ignores=[]),'actual-package-log','exec'),namespace)
            process,worker=namespace['launch_logged']([sys.executable,'-c',
                "import sys;sys.stderr.buffer.write(b'x'*(2*1024*1024+65536));sys.stderr.flush();print('done')"],
                'app',stdout=subprocess.PIPE)
            self.assertIsNone(process.stderr)  # communicate cannot race the private drain.
            self.assertEqual(process.communicate(timeout=5)[0],b'done\n')
            self.assertEqual(process.returncode,0)
            worker.join(timeout=2);self.assertFalse(worker.is_alive())
            path=state/'gtk-install-app-stderr.log'
            self.assertEqual(path.stat().st_size,2*1024*1024)
            self.assertEqual(path.stat().st_mode & 0o777,0o600)
            self.assertEqual(path.stat().st_nlink,1)
            fdopen=os.fdopen
            class FailedLog:
                def __init__(self,file):self.file=file
                def __enter__(self):return self
                def __exit__(self,*args):self.file.close()
                def write(self,data):raise OSError('diagnostic write failed')
            def failing_log(fd,mode,*args,**kwargs):
                file=fdopen(fd,mode,*args,**kwargs)
                return FailedLog(file) if mode=='wb' else file
            with patch.object(os,'fdopen',side_effect=failing_log):
                process,worker=namespace['launch_logged']([sys.executable,'-c',
                    "import sys;sys.stderr.buffer.write(b'x'*131072);sys.exit(17)"],
                    'driver',stdout=subprocess.PIPE)
                process.communicate(timeout=5);worker.join(timeout=2)
            self.assertEqual(process.returncode,17)  # Diagnostic I/O preserves the child's original failure.
            self.assertFalse(worker.is_alive())

    def test_package_diagnostic_uses_cached_readiness_without_extra_getters_or_text(self):
        spec=importlib.util.spec_from_file_location('gtk_package',ROOT/'dev/e2e/veranda-package-ui.py')
        package=importlib.util.module_from_spec(spec);spec.loader.exec_module(package)
        atspi=SimpleNamespace(Role=SimpleNamespace(LABEL=29,PUSH_BUTTON=43),
            StateType=SimpleNamespace(SHOWING='showing',ENABLED='enabled',SENSITIVE='sensitive',SELECTED='selected'))
        calls=[]
        def node(role,name):
            return SimpleNamespace(get_state_set=lambda:SimpleNamespace(contains=lambda state:state in ('showing','sensitive')),
                get_role=lambda:role,get_name=lambda:calls.append(True) or name)
        diagnostics={}
        self.assertEqual(package.gtk_ready([node(43,'Refresh'),node(43,'Connect remote host'),
            node(29,'private_error: private detail')],atspi,diagnostics=diagnostics)['kind'],
            'gtk-controllable-actionable-owner-error')
        diagnostics['last_readiness']['private_text']='private detail'
        before=len(calls);observer=SimpleNamespace(diagnostics=diagnostics)
        with contextlib.redirect_stderr(io.StringIO()) as output:
            package.package_diagnostic('readiness',observer,RuntimeError('private detail'))
        self.assertEqual(len(calls),before)
        frame=json.loads(output.getvalue().split(' ',1)[1])
        self.assertEqual(frame['failure_type'],'RuntimeError')
        self.assertEqual(frame['last_complete_readiness']['enabled_refresh'],0)
        self.assertEqual(frame['last_complete_readiness']['sensitive_refresh'],1)
        self.assertNotIn('private',output.getvalue())
        with patch('builtins.print',side_effect=OSError('private')):
            package.package_diagnostic('readiness',observer,RuntimeError('original'))

    def test_actual_input_watchdog_captures_before_cleanup_without_masking_timeout(self):
        spec = importlib.util.spec_from_file_location('gtk_short', ROOT / 'veranda/desktop/wayland-short.py')
        short = importlib.util.module_from_spec(spec); spec.loader.exec_module(short)
        loop=next(node for node in ast.walk(ast.parse((ROOT/'veranda/desktop/wayland-short.py').read_text()))
                  if isinstance(node,ast.While) and ast.unparse(node.test)=='action.poll() is None')
        for fault in ('none','capture','process','window','window-before'):
            with self.subTest(fault=fault), tempfile.TemporaryDirectory(prefix='veranda-watchdog-',dir='/tmp') as temporary:
                state=Path(temporary);events=[];lifetime={};identity=(os.getuid(),123,4,5)
                window=SimpleNamespace(window='12',singleton=lambda:'13' if fault=='window' else '12')
                def window_probe(*args):
                    if fault=='window-before':raise RuntimeError('private diagnostic detail')
                    return window
                def capture(argv,**options):
                    events.append('capture')
                    self.assertEqual(options['env']['DISPLAY'],':99')
                    if fault=='capture':raise subprocess.TimeoutExpired(['capture'],5)
                    Path(argv[-1]).write_bytes(b'owned screenshot')
                namespace={'action':SimpleNamespace(poll=lambda:None),'tree':SimpleNamespace(sample=lambda:events.append('sample')),
                    'time':short.time,'deadline':0,'capture_input_watchdog':short.capture_input_watchdog,
                    'lifetime':lifetime,'app':SimpleNamespace(pid=77,poll=lambda:None),'gui_identity':identity,
                    'result':{'backend':'x11'},'env':{'DISPLAY':':99'},'state':state,'args':SimpleNamespace(remote=True),
                    'workload_seconds':600}
                with patch.object(short.time,'monotonic',return_value=1), \
                     patch.object(short.input_driver,'process_identity',return_value=(999,123,4,5) if fault=='process' else identity), \
                     patch.object(short.probe,'X11Toplevel',side_effect=window_probe), \
                     patch.object(short.input_driver.subprocess,'run',side_effect=capture):
                    with self.assertRaisesRegex(AssertionError,'physical_input_watchdog'):
                        with fixture.cleanup_on_exit(lambda:events.append('cleanup'),'test'):
                            exec(compile(ast.Module(body=[loop],type_ignores=[]),'actual-watchdog','exec'),namespace)
                self.assertEqual(events,['sample','cleanup'] if fault in ('process','window-before') else ['sample','capture','cleanup'])
                self.assertIn('watchdog_capture_seconds',lifetime)
                if fault=='none':
                    self.assertEqual(lifetime['watchdog_capture']['workload_deadline_seconds'],600)
                    self.assertEqual((state/'failure.png').stat().st_mode & 0o777,0o600)
                else:
                    self.assertIn('watchdog_capture_failure_type',lifetime)
                    self.assertEqual(lifetime['watchdog_capture_failure_stage'],
                        {'capture':'screenshot','process':'process-before','window':'window-after','window-before':'window-before'}[fault])
                    self.assertNotIn('private diagnostic detail',json.dumps(lifetime))
                    self.assertFalse((state/'failure.png').exists())

    def test_failure_handler_retains_only_last_owned_source_frame_and_original_classification(self):
        spec = importlib.util.spec_from_file_location('gtk_input', ROOT / 'veranda/desktop/wayland-input.py')
        driver = importlib.util.module_from_spec(spec); spec.loader.exec_module(driver)
        tree = ast.parse((ROOT / 'veranda/desktop/wayland-input.py').read_text())
        handler = next(node for node in ast.walk(tree) if isinstance(node, ast.ExceptHandler)
                       and node.name == 'error' and node.body
                       and ast.unparse(node.body[0]).startswith('result.update(failure_stage=stage'))
        actual = ast.fix_missing_locations(ast.Module(body=[ast.Try(
            body=[ast.Expr(value=ast.Call(func=ast.Name(id='own_outer', ctx=ast.Load()), args=[], keywords=[]))],
            handlers=[handler], orelse=[], finalbody=[])], type_ignores=[]))
        for deeper_foreign in (False, True):
            with self.subTest(deeper_foreign=deeper_foreign):
                result = {'original_exit_code': 1}; collectors = []
                namespace = dict(vars(driver), result=result, stage='synthetic-stage', deeper_foreign=deeper_foreign,
                                 retain_failure_diagnostics=lambda value, collect: collectors.append(collect))
                private = 'PRIVATE_TRACE_MARKER'
                exec(compile("def foreign_private_frame():\n    raise RuntimeError('" + private + "')\n",
                             '/private/' + private, 'exec'), namespace)
                source = ("def own_outer():\n    own_inner()\n"
                          "def own_inner():\n    if deeper_foreign:\n        foreign_private_frame()\n"
                          "    else:\n        raise AssertionError('focus_target_unavailable')\n")
                exec(compile(source, driver.__file__, 'exec'), namespace)
                with patch.object(driver.subprocess, 'run') as rpc, \
                        patch.object(driver.subprocess, 'check_output') as read, \
                        patch.object(driver, 'capture_view') as capture:
                    exec(compile(actual, 'actual-exception-handler', 'exec'), namespace)
                    rpc.assert_not_called(); read.assert_not_called(); capture.assert_not_called()
                self.assertEqual(result['original_exit_code'], 1)
                self.assertEqual(result['failure_stage'], 'synthetic-stage')
                self.assertEqual(result['failure_type'], 'RuntimeError' if deeper_foreign else 'AssertionError')
                if deeper_foreign:
                    self.assertNotIn('failure_reason', result)
                else:
                    self.assertEqual(result['failure_reason'], 'focus_target_unavailable')
                self.assertEqual(result['failure_source_frame'], {'function': 'own_inner', 'line': 5 if deeper_foreign else 7})
                self.assertEqual(len(collectors), 1)  # Existing diagnostic callback remains deferred.
                self.assertNotIn(private, json.dumps(result))
                self.assertNotIn(driver.__file__, json.dumps(result))
                self.assertNotIn('foreign_private_frame', json.dumps(result))

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
