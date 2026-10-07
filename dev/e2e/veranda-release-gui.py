#!/usr/bin/env python3
"""Narrow release GUI acceptance; run only through veranda-release-gui.sh."""
import ctypes
import importlib.util
import json
import os
from pathlib import Path
import re
import selectors
import shlex
import signal
import stat
import statistics
import struct
import subprocess
import sys
import time
import zlib

sys.dont_write_bytecode = True

CASES = frozenset(context + '-' + pair for context in ['default', 'named', 'remote']
                  for pair in ['next-match', 'base-next', 'next-base'])
STAGES = frozenset(['preparation', 'preparation.ssh', 'preparation.display', 'preparation.accessibility',
                   'launch.ready', 'launch.accessible', 'launch.connect-link', 'select.owner',
                   'matched.heading', 'matched.yard', 'matched.details', 'matched.alerts', 'matched.reconcile',
                   'select.yard', 'remote.form', 'remote.destination', 'remote.window', 'remote.focus',
                   'remote.input', 'remote.readback', 'remote.assess', 'remote.fingerprint',
                   'remote.consent', 'remote.connect', 'remote.saved', 'remote.route', 'remote.reload',
                   'remote.select', 'remote.state', 'route', 'mismatch.message', 'mismatch.mutations',
                   'growth.removal', 'growth.idle', 'growth.budget',
                   'screenshot', 'case.cleanup', 'cleanup', 'session'])
CATEGORIES = frozenset(['timeout', 'ambiguity', 'accessible_api', 'accessible_action', 'accessible_bounds',
                       'process', 'screenshot', 'ownership', 'fixture_data', 'assertion', 'cleanup', 'unexpected', 'resource_budget'])
SELECTION_STATES = frozenset(context + '-' + heading for context in ['owner', 'yard', 'none']
                            for heading in ['overview-name', 'overview-text', 'projects-name', 'projects-text', 'absent']) | {'unavailable', 'ambiguous', 'unobserved'}
SELECTION_CHECKPOINTS = frozenset(['tree', 'node-role', 'node-name', 'pressed-state',
                                 'heading-text-interface', 'heading-text-count', 'heading-text-content', 'selection-state'])
YARD_CHECKPOINTS = frozenset(['tree', 'state-set', 'defunct-recheck', 'child-count', 'child-index', 'node-role', 'node-name'])
SELECTION_FAILURES = frozenset(point + ':' + category for point in SELECTION_CHECKPOINTS
                              for category in ['timeout', 'accessible_api', 'attribute_error', 'type_error', 'accessible_bounds', 'unexpected'])
YARD_RECHECK_FAILURES = frozenset(point + ':' + kind + '_' + state for point in ['node-role', 'node-name']
                                 for kind in ['application_gone', 'ipc', 'other_glib']
                                 for state in ['defunct', 'live', 'recheck_failed'])
YARD_FAILURES = frozenset(point + ':' + category for point in YARD_CHECKPOINTS
                         for category in ['timeout', 'accessible_api', 'attribute_error', 'type_error', 'accessible_bounds', 'unexpected']) | YARD_RECHECK_FAILURES
FINGERPRINT_CHECKPOINTS = (YARD_CHECKPOINTS - {'node-role'}) | {'text-interface', 'text-count', 'text-content'}
FINGERPRINT_FAILURES = frozenset(point + ':' + category for point in FINGERPRINT_CHECKPOINTS
                               for category in ['timeout', 'accessible_api', 'attribute_error', 'type_error', 'accessible_bounds', 'unexpected'])
RECONCILE_CHECKPOINTS = YARD_CHECKPOINTS | {'button-find', 'enabled-state-set', 'enabled-state-contains'}
RECONCILE_FAILURES = frozenset(point + ':' + category for point in RECONCILE_CHECKPOINTS
                             for category in ['timeout', 'accessible_api', 'attribute_error', 'type_error', 'accessible_bounds', 'unexpected']) | YARD_RECHECK_FAILURES
HEADING_CHECKPOINTS = YARD_CHECKPOINTS | {'unique'}
HEADING_KINDS = frozenset(['timeout', 'accessible_api', 'attribute_error', 'type_error', 'accessible_bounds',
                           'unexpected', 'ambiguity', 'fixture_data', 'assertion', 'process'])
HEADING_CASES = {'local': {'default-next-match', 'named-next-match', 'named-base-next', 'named-next-base'},
                 'pre-remote': {case for case in CASES if case.startswith('remote-')},
                 'post-save': {case for case in CASES if case.startswith('remote-')},
                 'post-reload': {'remote-next-match'}}
HEADING_FAILURES = frozenset(site + '.' + point + ':' + kind for site in HEADING_CASES
                            for point in HEADING_CHECKPOINTS for kind in HEADING_KINDS) | frozenset(
                            site + '.' + detail for site in HEADING_CASES for detail in YARD_RECHECK_FAILURES)
METADATA_CASES = {'launch.connect-link': CASES,
                  'select.yard': {case for case in CASES if case.startswith('named-')},
                  'matched.alerts': {'named-next-match'},
                  **{stage: {case for case in CASES if case.startswith('remote-')}
                     for stage in ['remote.form', 'remote.destination', 'remote.assess', 'remote.consent',
                                   'remote.connect', 'remote.saved', 'remote.state']}}
REMOTE_SELECT_FAILURES = frozenset(['owner-lookup:timeout', 'owner-action:timeout'])
MAX_FAILURE_PNG = 512 * 1024
MAX_STDERR_SCAN = 64 * 1024
MAX_DIAGNOSTIC = 2048
ACCESSIBLE_LIMITS = {'node_count': (0, 2048), 'depth': (0, 32),
                     'child_count': (0, 2048), 'text_length': (0, 32768)}
STDERR_SIGNATURES = {'gtk_critical': b'Gtk-CRITICAL', 'gobject_critical': b'GLib-GObject-CRITICAL',
                     'egl_warning': b'libEGL warning', 'sandbox_error': b'bwrap:',
                     'rust_panic': b'panicked at', 'x_io_error': b'X IO error'}
STDERR_SIGNATURES.update({name: b'VERANDA_DIAGNOSTIC ' + name.encode('ascii') for name in (
    'frontend_boundary', 'frontend_error', 'frontend_unhandled_rejection', 'web_process_crashed',
    'web_process_memory_limit', 'web_process_terminated_by_api', 'web_process_termination_unknown',
    'web_process_responsive', 'web_process_unresponsive')})
PROCESS_ROLES = frozenset(['app', 'web', 'network', 'ssh', 'rpc', 'other'])
WEB_NAMES = frozenset(['WebKitWebProcess', 'WebKitWebProces'])  # Linux comm truncates to 15 bytes.
NETWORK_NAMES = frozenset(['WebKitNetworkProcess', 'WebKitNetworkPr'])


def valid_failure_png(value):
    if not 57 <= len(value) <= MAX_FAILURE_PNG: return False
    header = b'IHDR' + struct.pack('>IIBBBBB', 1280, 800, 8, 2, 0, 0, 0)
    if value[:33] != b'\x89PNG\r\n\x1a\n' + struct.pack('>I', 13) + header + struct.pack('>I', zlib.crc32(header)): return False
    length = int.from_bytes(value[33:37], 'big')
    if value[37:41] != b'IDAT' or len(value) != length + 57: return False
    if value[41 + length:45 + length] != struct.pack('>I', zlib.crc32(value[37:41 + length])): return False
    return value[-12:] == b'\x00\x00\x00\x00IEND\xaeB`\x82'


def selection_state(owners, yards, overview, projects, unavailable):
    if owners + yards > 1 or overview and projects:
        return 'ambiguous'
    if unavailable:
        return 'unavailable'
    context = 'owner' if owners else 'yard' if yards else 'none'
    heading = 'overview-' + overview if overview else 'projects-' + projects if projects else 'absent'
    return context + '-' + heading


class FixtureFailure(Exception):
    def __init__(self, category):
        assert category in CATEGORIES
        self.category = category


class AccessibleBoundFailure(FixtureFailure):
    def __init__(self, kind, observed):
        super().__init__('accessible_bounds')
        self.bound = {'kind': kind, 'observed': max(-2147483648, min(observed, 2147483647)),
                      'clamped': not -2147483648 <= observed <= 2147483647}


class ChildCountFailure(FixtureFailure):
    def __init__(self):
        super().__init__('accessible_api')


def accessible_bound(kind, observed):
    minimum, maximum = ACCESSIBLE_LIMITS[kind]
    if not minimum <= observed <= maximum:
        raise AccessibleBoundFailure(kind, observed)


def require(condition, category):
    if not condition:
        raise FixtureFailure(category)


def interface_text(interface, text_api, checkpoint=None):
    # get_text_iface returns the same Accessible GObject. Its get_text()
    # accessor shadows Text.get_text(start, end), so call the interface explicitly.
    if checkpoint: checkpoint('heading-text-count')
    accessible_bound('text_length', text_api.get_character_count(interface))
    if checkpoint: checkpoint('heading-text-content')
    return text_api.get_text(interface, 0, -1)


def accessible_text(nodes, text_api, checkpoint):
    parts = []; checkpoint('tree')
    for node in nodes():
        checkpoint('node-name'); name = node.get_name()
        if name: parts.append(name[:2048])
        checkpoint('text-interface'); interface = node.get_text_iface()
        if interface:
            parts.append(interface_text(interface, text_api, lambda point: checkpoint(point.removeprefix('heading-'))))
    return '\n'.join(parts)


def reconcile_ready(find, enabled, checkpoint):
    checkpoint('button-find'); node = find()
    if node is None: return False
    checkpoint('enabled-state-set'); states = node.get_state_set()
    checkpoint('enabled-state-contains'); return states.contains(enabled)


def input_window(windows, pid, expected_pid, expected_start, actual_start, actual_uid):
    require(isinstance(windows, bytes) and len(windows) <= 1024 and windows.isascii(), 'ownership')
    identifiers = windows.split()
    require(len(identifiers) == 1 and identifiers[0].isdigit() and 0 < len(identifiers[0]) <= 20
            and int(identifiers[0]) > 0, 'ambiguity')
    require(pid == expected_pid and actual_start == expected_start and actual_uid == os.geteuid(), 'ownership')
    return identifiers[0].decode('ascii')


def observe_selection(nodes, buttons, heading_role, pressed_state, checkpoint, text_api):
    owners = yards = 0; overview = projects = None; unavailable = False
    checkpoint('tree')
    for node in nodes():
        checkpoint('node-role'); role = node.get_role()
        if role not in buttons | {heading_role}: continue
        checkpoint('node-name'); name = node.get_name() or ''
        if role in buttons and name.startswith(('Show owner ', 'Show yard ')):
            checkpoint('pressed-state')
            if node.get_state_set().contains(pressed_state):
                if name.startswith('Show owner '): owners += 1
                else: yards += 1
        elif role == heading_role:
            if name == 'Overview': overview = 'name'
            if name == 'Projects': projects = 'name'
            checkpoint('heading-text-interface'); interface = node.get_text_iface()
            if interface:
                contents = interface_text(interface, text_api, checkpoint)
                if overview != 'name' and contents == 'Overview': overview = 'text'
                if projects != 'name' and contents == 'Projects': projects = 'text'
            unavailable |= name == 'Fleet unavailable'
    checkpoint('selection-state')
    return selection_state(owners, yards, overview, projects, unavailable)


def selection_failure(error, checkpoint, api_error):
    if isinstance(error, TimeoutError): category = 'timeout'
    elif isinstance(error, (api_error, ChildCountFailure)): category = 'accessible_api'
    elif isinstance(error, AttributeError): category = 'attribute_error'
    elif isinstance(error, TypeError): category = 'type_error'
    elif isinstance(error, FixtureFailure) and error.category == 'accessible_bounds': category = 'accessible_bounds'
    else: category = 'unexpected'
    assert checkpoint in SELECTION_CHECKPOINTS | YARD_CHECKPOINTS | FINGERPRINT_CHECKPOINTS | RECONCILE_CHECKPOINTS | HEADING_CHECKPOINTS
    return checkpoint + ':' + category


def first_failure(previous, error, checkpoint, api_error):
    return previous or selection_failure(error, checkpoint, api_error)


def heading_failure(previous, error, site, point, api_error, detail=None):
    if previous: return previous
    assert site in HEADING_CASES and point in HEADING_CHECKPOINTS
    if detail is not None:
        assert detail in YARD_RECHECK_FAILURES and detail.split(':')[0] == point
        return site + '.' + detail
    detail = selection_failure(error, point, api_error)
    if isinstance(error, FixtureFailure) and error.category in HEADING_KINDS: detail = point + ':' + error.category
    elif isinstance(error, (TimeoutError, subprocess.TimeoutExpired)): detail = point + ':timeout'
    elif isinstance(error, subprocess.CalledProcessError): detail = point + ':process'
    elif isinstance(error, (OSError, ValueError)): detail = point + ':fixture_data'
    elif isinstance(error, AssertionError): detail = point + ':assertion'
    return site + '.' + detail


def heading_category(detail):
    assert detail in HEADING_FAILURES
    if detail.split('.', 1)[1] in YARD_RECHECK_FAILURES: return 'accessible_api'
    kind = detail.split(':')[1]
    return {'attribute_error': 'accessible_api', 'type_error': 'unexpected'}.get(kind, kind)


def yard_error_detail(error, point, recheck):
    assert point in {'node-role', 'node-name'}
    # Pinned libatspi collapses D-Bus error names into IPC; never inspect its message.
    kind = 'other_glib'
    if error.domain == 'atspi_error':
        if error.code == 0: kind = 'application_gone'
        elif error.code == 1: kind = 'ipc'
    try: state = 'defunct' if recheck() else 'live'
    except BaseException: state = 'recheck_failed'
    return point + ':' + kind + '_' + state


def yard_state(node, api, end):
    if time.monotonic() + 0.5 >= end: raise TimeoutError()
    api.set_timeout(250, 500)
    try: return node.get_state_set().contains(api.StateType.DEFUNCT)
    finally: api.set_timeout(500, 1000)


def yard_node_matches(node, buttons, expected, api_error, checkpoint, failed, recheck):
    point = 'node-role'; checkpoint(point)
    try:
        if node.get_role() not in buttons: return False
        point = 'node-name'; checkpoint(point)
        return node.get_name() == expected
    except api_error as error:
        detail = yard_error_detail(error, point, lambda: recheck(node))
        if detail == 'node-role:application_gone_defunct': return False
        failed(error, point, detail)
        raise  # Other role/name failures remain fatal.


def accessible_nodes(start, defunct, api_error, checkpoint=None, failed=None):
    queue = [(start, 0)]; result = []
    while queue:
        node, depth = queue.pop()
        if node is None: continue  # GetChildAtIndex permits NULL for a removed child.
        try:
            point = 'state-set'
            if checkpoint: checkpoint(point)
            if node.get_state_set().contains(defunct): continue
            accessible_bound('node_count', len(result) + 1)
            accessible_bound('depth', depth)
            point = 'child-count'
            if checkpoint: checkpoint(point)
            count = node.get_child_count()
            # libatspi returns -1 on failure, including paths without GError.
            # Discard this observation: GetState can synthesize DEFUNCT on IPC failure.
            if count == -1: raise ChildCountFailure()
            accessible_bound('child_count', count)
            children = []
            for i in range(count):
                point = 'child-index'
                if checkpoint: checkpoint(point)
                children.append((node.get_child_at_index(i), depth + 1))
        except api_error as error:
            # A disappeared object reports DEFUNCT. Other API errors retain
            # their original failure rather than becoming an empty tree.
            if checkpoint: checkpoint('defunct-recheck')
            try:
                if node.get_state_set().contains(defunct): continue
            except BaseException:
                if failed: failed(error, point)
                raise
            if failed: failed(error, point)
            raise
        result.append(node); queue.extend(children)
    return result


def growth_result(samples, warmup, cycles):
    first = [sample['rssBytes'] for sample in samples if sample['cycle'] <= 10]
    last = [sample['rssBytes'] for sample in samples if sample['cycle'] >= 91]
    first_median = statistics.median(first) if len(first) == 10 else None
    last_median = statistics.median(last) if len(last) == 10 else None
    growth = max(0, last_median - first_median) if first_median is not None and last_median is not None else None
    return {'warmupCompleted': warmup, 'cyclesCompleted': cycles, 'idleSettleSeconds': 30,
            'samples': samples, 'firstMedianBytes': first_median, 'lastMedianBytes': last_median,
            'growthBytes': growth, 'limitBytes': 10 * 1024 * 1024,
            'withinLimit': growth <= 10 * 1024 * 1024 if growth is not None else None}


def growth_self_test():
    diagnostic_self_test()
    samples = [{'cycle': cycle, 'rssBytes': (100 if cycle <= 10 else 110) * 1024 * 1024, 'processCount': 4}
               for cycle in [*range(1, 11), *range(91, 101)]]
    samples[0]['rssBytes'] *= 4  # One startup outlier must not replace the idle median.
    metrics = growth_result(samples, 10, 100)
    assert metrics['growthBytes'] == 10 * 1024 * 1024 and metrics['withinLimit'] is True
    result = {'passed': ['remote-next-match'], 'failedCase': None, 'cleanup': True,
              'failureStage': None, 'failureCategory': None, 'selectionState': None, 'selectionFailure': None,
              'growth': metrics}
    validate_summary(result, 0, True)
    for sample in samples[10:]: sample['rssBytes'] += 1
    result['growth'] = growth_result(samples, 10, 100)
    try: validate_summary(result, 0, True)
    except AssertionError: pass
    else: raise AssertionError('over-budget growth accepted')
    result.update(passed=[], failedCase='remote-next-match', failureStage='growth.budget', failureCategory='resource_budget')
    validate_summary(result, 1, True)
    result['growth'] = growth_result(samples[:3], 10, 3)
    result.update(failureStage='growth.idle', failureCategory='timeout')
    validate_summary(result, 1, True)
    assert result['growth']['withinLimit'] is None
    print('veranda-gui-growth: median, budget, incomplete-receipt and safe diagnostic checks passed')
    return 0


def validate_summary(result, rc, growth=False):
    assert type(rc) is int
    assert isinstance(result, dict) and set(result) == {'passed', 'failedCase', 'cleanup', 'failureStage', 'failureCategory', 'selectionState', 'selectionFailure'} | ({'growth'} if growth else set())
    assert isinstance(result['passed'], list) and len(result['passed']) <= 9
    assert all(isinstance(case, str) and case in CASES for case in result['passed'])
    assert len(set(result['passed'])) == len(result['passed'])
    assert result['failedCase'] is None or isinstance(result['failedCase'], str) and result['failedCase'] in CASES | {'preparation'}
    assert isinstance(result['cleanup'], bool)
    assert result['failureStage'] is None or isinstance(result['failureStage'], str) and result['failureStage'] in STAGES
    assert result['failureCategory'] is None or isinstance(result['failureCategory'], str) and result['failureCategory'] in CATEGORIES
    if result['failureStage'] == 'matched.heading':
        assert isinstance(result['selectionState'], str) and result['selectionState'] in SELECTION_STATES
    else:
        assert result['selectionState'] is None
    if isinstance(result['selectionFailure'], str) and result['selectionFailure'] in HEADING_FAILURES:
        assert result['failureStage'] == 'matched.heading'
        assert result['failedCase'] in HEADING_CASES[result['selectionFailure'].split('.')[0]]
        assert result['failureCategory'] == heading_category(result['selectionFailure'])
    elif result['selectionState'] == 'unobserved':
        assert isinstance(result['selectionFailure'], str) and result['selectionFailure'] in SELECTION_FAILURES
    elif result['failureStage'] in {'matched.yard', 'remote.fingerprint', 'matched.reconcile'}:
        allowed = {'matched.yard': YARD_FAILURES, 'remote.fingerprint': FINGERPRINT_FAILURES,
                   'matched.reconcile': RECONCILE_FAILURES}[result['failureStage']]
        if result['failureStage'] == 'remote.fingerprint': assert result['failedCase'] in {case for case in CASES if case.startswith('remote-')}
        if result['failureStage'] == 'matched.reconcile': assert result['failedCase'] in {'named-next-match', 'remote-next-match'}
        assert isinstance(result['selectionFailure'], str) and result['selectionFailure'] in allowed
        detail = result['selectionFailure'].split(':')[1]
        category = result['failureCategory']
        if result['selectionFailure'] in YARD_RECHECK_FAILURES or detail in {'attribute_error', 'accessible_api'}: assert category == 'accessible_api'
        elif detail == 'timeout': assert category == 'timeout'
        elif detail == 'accessible_bounds': assert category == 'accessible_bounds'
        elif detail == 'type_error': assert category == 'unexpected'
        else: assert category not in {'accessible_api', 'timeout', 'accessible_bounds'}
    elif result['failureStage'] in METADATA_CASES and result['selectionFailure'] is not None:
        assert result['failedCase'] in METADATA_CASES[result['failureStage']]
        assert isinstance(result['selectionFailure'], str) and result['selectionFailure'] in YARD_RECHECK_FAILURES
        assert result['failureCategory'] == 'accessible_api'
    elif result['failureStage'] == 'remote.select' and result['selectionFailure'] is not None:
        assert result['failedCase'] in {case for case in CASES if case.startswith('remote-')}
        assert result['failureCategory'] == 'timeout'
        assert isinstance(result['selectionFailure'], str) and result['selectionFailure'] in REMOTE_SELECT_FAILURES
    else:
        assert result['selectionFailure'] is None
    expected = {'remote-next-match'} if growth else CASES
    if growth:
        metrics = result['growth']
        assert isinstance(metrics, dict)
        assert type(metrics['warmupCompleted']) is int and 0 <= metrics['warmupCompleted'] <= 10
        assert type(metrics['cyclesCompleted']) is int and 0 <= metrics['cyclesCompleted'] <= 100
        assert metrics['warmupCompleted'] == 10 or metrics['cyclesCompleted'] == 0
        samples = metrics['samples']
        assert isinstance(samples, list) and len(samples) <= 20
        measured = [index for index in range(1, metrics['cyclesCompleted'] + 1) if index <= 10 or index >= 91]
        assert [sample['cycle'] for sample in samples] == measured
        for sample in samples:
            assert set(sample) == {'cycle', 'rssBytes', 'processCount'}
            assert type(sample['cycle']) is int
            assert type(sample['rssBytes']) is int and sample['rssBytes'] > 0
            assert type(sample['processCount']) is int and sample['processCount'] > 0
        assert metrics == growth_result(samples, metrics['warmupCompleted'], metrics['cyclesCompleted'])
        assert set(result['passed']) <= expected
    if rc == 0:
        assert set(result['passed']) == expected and result['failedCase'] is None and result['cleanup']
        assert result['failureStage'] is None and result['failureCategory'] is None
        if growth: assert metrics['cyclesCompleted'] == 100 and metrics['withinLimit'] is True
    else:
        assert result['failureStage'] is not None and result['failureCategory'] is not None
        assert result['failedCase'] is not None or set(result['passed']) == expected


def summary_self_test():
    import gi
    gi.require_version('Atspi', '2.0')
    from gi.repository import Atspi, GLib, GObject
    def png_chunk(kind, payload):
        return struct.pack('>I', len(payload)) + kind + payload + struct.pack('>I', zlib.crc32(kind + payload))
    png = b'\x89PNG\r\n\x1a\n' + png_chunk(b'IHDR', struct.pack('>IIBBBBB', 1280, 800, 8, 2, 0, 0, 0))
    png += png_chunk(b'IDAT', zlib.compress(b'\0' * (800 * (1 + 1280 * 3)))) + png_chunk(b'IEND', b'')
    assert valid_failure_png(png)
    for invalid_png in [b'unsafe private value', png[:20], png + b'unsafe private value',
                        png[:16] + b'\0\0\0\1' + png[20:], png[:41] + b'X' + png[42:], b'\0' * (MAX_FAILURE_PNG + 1)]:
        assert not valid_failure_png(invalid_png)
    class ApiError(Exception): pass
    class TextAPI:
        def get_character_count(interface): return len(interface.contents)
        def get_text(interface, start, end):
            assert (start, end) == (0, -1)
            return interface.contents
    class TextAccessible(TextAPI):
        contents = 'Overview'
        def get_text(self): return self  # Accessible accessor shadows Text method.
    interface = TextAccessible()
    try: interface.get_text(0, -1)
    except TypeError: pass
    else: raise AssertionError('binding collision was not exercised')
    assert interface_text(interface, TextAPI) == 'Overview'
    assert input_window(b'42\n', 7, 7, 11, 11, os.geteuid()) == '42'
    for windows, pid, start, uid in [(b'42\n43\n', 7, 11, os.geteuid()),
                                    (b'unsafe private value', 7, 11, os.geteuid()),
                                    (b'42\n', 8, 11, os.geteuid()),
                                    (b'42\n', 7, 12, os.geteuid()),
                                    (b'42\n', 7, 11, os.geteuid() + 1)]:
        try: input_window(windows, pid, 7, 11, start, uid)
        except FixtureFailure: pass
        else: raise AssertionError('unowned or ambiguous input target accepted')
    class SelectionNode:
        def __init__(self, role, name, pressed=False):
            self.role = role; self.name = name; self.pressed = pressed
        def get_role(self): return self.role
        def get_name(self): return self.name
        def get_state_set(self):
            assert self.name is not None  # Unnamed buttons must never reach state lookup.
            return self
        def contains(self, _): return self.pressed
        def get_text_iface(self): return None
    checkpoints = []
    observed = [SelectionNode('button', None, True), SelectionNode('button', 'Show owner Local owner', True),
                SelectionNode('button', 'Show yard fixture', False), SelectionNode('heading', 'Overview')]
    assert observe_selection(lambda: observed, {'button'}, 'heading', 'pressed', checkpoints.append, TextAPI) == 'owner-overview-name'
    assert checkpoints[-1] == 'selection-state'
    assert selection_failure(AttributeError('unsafe private value'), 'node-name', ApiError) == 'node-name:attribute_error'
    assert selection_failure(ApiError('unsafe private value'), 'tree', ApiError) == 'tree:accessible_api'
    original = first_failure(None, ApiError('unsafe original'), 'child-count', ApiError)
    assert first_failure(original, AttributeError('unsafe cleanup'), 'defunct-recheck', ApiError) == 'child-count:accessible_api'
    class Node:
        def __init__(self, children=(), dead=False, removed=False, broken=False):
            self.children = children; self.dead = dead; self.removed = removed; self.broken = broken
        def get_state_set(self): return self
        def contains(self, _): return self.dead
        def get_child_count(self):
            if self.removed: self.dead = True; raise ApiError()
            if self.broken: raise ApiError()
            return len(self.children)
        def get_child_at_index(self, index): return self.children[index]
    live = Node(); parent = Node([None, Node(dead=True), Node(removed=True), live])
    assert accessible_nodes(parent, 'defunct', ApiError) == [parent, live]
    try: accessible_nodes(Node(broken=True), 'defunct', ApiError)
    except ApiError: pass
    else: raise AssertionError('live API failure was hidden')
    class RecheckFailure(Node):
        reads = 0
        def get_state_set(self):
            self.reads += 1
            if self.reads == 2: raise AttributeError('unsafe recheck')
            return self
    points = []; failures = []
    def preserve(error, point):
        failures.append(first_failure(failures[-1] if failures else None, error, point, ApiError))
    try: accessible_nodes(RecheckFailure(broken=True), 'defunct', ApiError, points.append, preserve)
    except AttributeError: pass
    else: raise AssertionError('live recheck failure was hidden')
    preserve(AttributeError('unsafe cleanup'), 'defunct-recheck')
    assert points[-1] == 'defunct-recheck' and failures == ['child-count:accessible_api'] * 2
    assert selection_failure(AttributeError('unsafe private value'), 'state-set', ApiError) == 'state-set:attribute_error'
    class FailingAccessible(GObject.Object):
        def __init__(self, error, point, state):
            super().__init__()
            self.error = error; self.point = point; self.state = state; self.reads = 0
        def get_role(self):
            if self.point == 'node-role': raise self.error
            return Atspi.Role.PUSH_BUTTON
        def get_name(self): raise self.error
        def get_state_set(self):
            self.reads += 1
            if self.state == 'recheck_failed': raise AttributeError('unsafe secondary payload')
            return Atspi.StateSet.new([Atspi.StateType.DEFUNCT] if self.state == 'defunct' else [])
    details = []
    for domain, code, kind in [('atspi_error', 0, 'application_gone'), ('atspi_error', 1, 'ipc'),
                               ('atspi_error', 99, 'other_glib'), ('unsafe private domain', 1, 'other_glib')]:
        error = GLib.Error.new_literal(GLib.quark_from_string(domain), 'unsafe private message', code)
        for point in ['node-role', 'node-name']:
            for state in ['defunct', 'live', 'recheck_failed']:
                node = FailingAccessible(error, point, state); captured = []
                def capture(original_error, original_point, detail):
                    assert original_error is error and original_point == point
                    captured.append(detail)
                try:
                    matched = yard_node_matches(node, {Atspi.Role.PUSH_BUTTON}, 'Show yard fixture', GLib.Error, lambda _: None,
                                                capture, lambda current: yard_state(current, Atspi, time.monotonic() + 1))
                except GLib.Error as raised: assert raised is error
                else:
                    assert point == 'node-role' and kind == 'application_gone' and state == 'defunct'
                    assert matched is False and not captured and node.reads == 1
                    details.append('node-role:application_gone_defunct')
                    continue
                assert captured == [point + ':' + kind + '_' + state] and node.reads == 1
                assert first_failure(captured[0], AttributeError('unsafe cleanup'), 'defunct-recheck', GLib.Error) == captured[0]
                details.extend(captured)
    node = FailingAccessible(error, 'node-role', 'live')
    assert yard_error_detail(error, 'node-role', lambda: yard_state(node, Atspi, time.monotonic())) == 'node-role:other_glib_recheck_failed'
    assert node.reads == 0  # Insufficient original wait budget never starts a state query.
    assert set(details) == YARD_RECHECK_FAILURES and 'unsafe' not in json.dumps(details)
    class CurrentYard(GObject.Object):
        def get_role(self): return Atspi.Role.PUSH_BUTTON
        def get_name(self): return 'Show yard fixture'
    gone = GLib.Error.new_literal(GLib.quark_from_string('atspi_error'), 'unsafe original removal', 0)
    removed = FailingAccessible(gone, 'node-role', 'defunct'); failures = []
    def match(current):
        return yard_node_matches(current, {Atspi.Role.PUSH_BUTTON}, 'Show yard fixture', GLib.Error, lambda _: None,
                                 lambda error, point, detail: failures.append(detail),
                                 lambda node: yard_state(node, Atspi, time.monotonic() + 1))
    assert not any(match(current) for current in [removed]) and not failures
    assert any(match(current) for current in [removed, CurrentYard()]) and not failures
    expired_node = FailingAccessible(gone, 'node-role', 'defunct')
    try:
        yard_node_matches(expired_node, {Atspi.Role.PUSH_BUTTON}, 'Show yard fixture', GLib.Error, lambda _: None,
                          lambda error, point, detail: failures.append(detail), lambda node: yard_state(node, Atspi, time.monotonic()))
    except GLib.Error as raised: assert raised is gone
    else: raise AssertionError('expired removal check was suppressed')
    assert failures == ['node-role:application_gone_recheck_failed'] and expired_node.reads == 0
    attribute = AttributeError('unsafe original attribute')
    broken_node = FailingAccessible(attribute, 'node-role', 'defunct'); points = []
    try: yard_node_matches(broken_node, {Atspi.Role.PUSH_BUTTON}, 'Show yard fixture', GLib.Error, points.append, lambda *_: None, lambda _: True)
    except AttributeError as raised: assert raised is attribute
    else: raise AssertionError('attribute failure was suppressed')
    assert points == ['node-role'] and broken_node.reads == 0
    class TextNode(GObject.Object):
        def __init__(self, point=None, error=None):
            super().__init__(); self.point = point; self.error = error
        def get_name(self):
            if self.point == 'node-name': raise self.error
            return 'fixture'
        def get_text_iface(self):
            if self.point == 'text-interface': raise self.error
            return self
        def get_text(self): return self  # Accessible accessor, not Text.get_text.
    class NodeTextAPI:
        def get_character_count(node):
            if node.point == 'text-count': raise node.error
            return 7
        def get_text(node, start, end):
            assert (start, end) == (0, -1)
            if node.point == 'text-content': raise node.error
            return 'fixture'
    assert Atspi.Text.get_text.__doc__ == 'get_text(self, start_offset:int, end_offset:int) -> str'
    assert accessible_text(lambda: [TextNode()], NodeTextAPI, lambda _: None) == 'fixture\nfixture'
    for point in ['node-name', 'text-interface', 'text-count', 'text-content']:
        for error in [GLib.Error.new_literal(GLib.quark_from_string('atspi_error'), 'unsafe private fingerprint payload', 1),
                      AttributeError('unsafe private fingerprint payload')]:
            points = []
            try: accessible_text(lambda: [TextNode(point, error)], NodeTextAPI, points.append)
            except (GLib.Error, AttributeError) as raised:
                assert raised is error and points[-1] == point
                original_detail = first_failure(None, raised, points[-1], GLib.Error)
            else: raise AssertionError('original text API failure was hidden')
            assert original_detail == point + (':accessible_api' if isinstance(error, GLib.Error) else ':attribute_error')
            assert first_failure(original_detail, TypeError('unsafe cleanup'), 'tree', GLib.Error) == original_detail
            assert 'unsafe' not in json.dumps(original_detail)
    class ReadyButton(GObject.Object):
        def __init__(self, enabled=True, error=None):
            super().__init__(); self.enabled = enabled; self.error = error
        def get_state_set(self):
            if self.error: raise self.error
            states = Atspi.StateSet.new([])
            if self.enabled: states.add(Atspi.StateType.ENABLED)
            return states
    calls = []
    def changing_find():
        calls.append(None)
        return ReadyButton() if len(calls) == 1 else None
    try: changing_find().get_state_set().contains(Atspi.StateType.ENABLED) if changing_find() else False
    except AttributeError: pass
    else: raise AssertionError('previous duplicate lookup race was not reproduced')
    assert len(calls) == 2
    calls.clear(); points = []
    assert reconcile_ready(changing_find, Atspi.StateType.ENABLED, points.append) and len(calls) == 1
    assert points == ['button-find', 'enabled-state-set', 'enabled-state-contains']
    assert not reconcile_ready(lambda: None, Atspi.StateType.ENABLED, lambda _: None)
    assert not reconcile_ready(lambda: ReadyButton(False), Atspi.StateType.ENABLED, lambda _: None)
    for error in [GLib.Error.new_literal(GLib.quark_from_string('atspi_error'), 'unsafe private reconcile payload', 1),
                  AttributeError('unsafe private reconcile payload')]:
        points = []
        try: reconcile_ready(lambda: ReadyButton(error=error), Atspi.StateType.ENABLED, points.append)
        except (GLib.Error, AttributeError) as raised:
            assert raised is error and points[-1] == 'enabled-state-set'
            detail = first_failure(None, raised, points[-1], GLib.Error)
        else: raise AssertionError('original readiness API failure was hidden')
        assert detail == 'enabled-state-set:' + ('accessible_api' if isinstance(error, GLib.Error) else 'attribute_error')
        assert first_failure(detail, TypeError('unsafe cleanup'), 'tree', GLib.Error) == detail
        assert 'unsafe' not in json.dumps(detail)
    deep = Node()
    for _ in range(33): deep = Node([deep])
    for oversized in [deep, Node([Node() for _ in range(2048)])]:
        try: accessible_nodes(oversized, 'defunct', ApiError)
        except FixtureFailure as failure: assert failure.category == 'accessible_bounds'
        else: raise AssertionError('accessible tree bound was removed')
    success = {'passed': sorted(CASES), 'failedCase': None, 'cleanup': True,
               'failureStage': None, 'failureCategory': None, 'selectionState': None, 'selectionFailure': None}
    validate_summary(success, 0)
    failure = dict(success, passed=[], failedCase='default-next-match', failureStage='launch.accessible', failureCategory='timeout')
    validate_summary(failure, 1)
    heading_failure = dict(failure, failureStage='matched.heading', selectionState='owner-absent')
    validate_summary(heading_failure, 1)
    observer_failure = dict(heading_failure, selectionState='unobserved', selectionFailure='node-name:attribute_error')
    validate_summary(observer_failure, 1)
    yard_failure = dict(failure, failureStage='matched.yard', failureCategory='accessible_api', selectionFailure=original)
    validate_summary(yard_failure, 1)
    for detail in YARD_RECHECK_FAILURES:
        validate_summary(dict(yard_failure, selectionFailure=detail), 1)
    fingerprint_failure = dict(failure, failedCase='remote-next-match', failureStage='remote.fingerprint',
                               failureCategory='accessible_api', selectionFailure='text-content:accessible_api')
    validate_summary(fingerprint_failure, 1)
    for detail in FINGERPRINT_FAILURES:
        category = detail.split(':')[1]
        mapped = {'attribute_error': 'accessible_api', 'type_error': 'unexpected'}.get(category, category)
        validate_summary(dict(fingerprint_failure, selectionFailure=detail, failureCategory=mapped), 1)
    reconcile_failure = dict(failure, failedCase='named-next-match', failureStage='matched.reconcile',
                             failureCategory='accessible_api', selectionFailure='enabled-state-set:accessible_api')
    for detail in RECONCILE_FAILURES:
        category = detail.split(':')[1]
        mapped = 'accessible_api' if detail in YARD_RECHECK_FAILURES else {'attribute_error': 'accessible_api', 'type_error': 'unexpected'}.get(category, category)
        validate_summary(dict(reconcile_failure, selectionFailure=detail, failureCategory=mapped), 1)
    for site, cases in HEADING_CASES.items():
        for detail in YARD_RECHECK_FAILURES:
            for case in cases:
                validate_summary(dict(heading_failure, failedCase=case, selectionFailure=site + '.' + detail,
                                      failureCategory='accessible_api'), 1)
    for stage, cases in METADATA_CASES.items():
        for detail in YARD_RECHECK_FAILURES:
            for case in cases:
                validate_summary(dict(failure, failedCase=case, failureStage=stage, selectionState=None,
                                      selectionFailure=detail, failureCategory='accessible_api'), 1)
    assert selection_state(1, 0, 'name', None, False) == 'owner-overview-name'
    assert selection_state(1, 0, 'text', None, False) == 'owner-overview-text'
    assert selection_state(0, 1, None, 'name', False) == 'yard-projects-name'
    assert selection_state(0, 0, False, False, False) == 'none-absent'
    assert selection_state(1, 0, False, False, True) == 'unavailable'
    assert selection_state(1, 1, None, None, False) == 'ambiguous'
    invalid = [(dict(success, passed=success['passed'][:-1]), 0), (dict(success, passed=[success['passed'][0]] * 9), 0),
               (dict(success, cleanup=False), 0), (dict(success, rawError='unsafe private value'), 0)]
    for field in ['failedCase', 'failureStage', 'failureCategory']:
        invalid.append((dict(failure, **{field: '/private/unsafe token=value'}), 1))
    invalid.append((dict(failure, passed=['unsafe private value']), 1))
    invalid.append((dict(heading_failure, selectionState='/private/unsafe token=value'), 1))
    invalid.append((dict(heading_failure, selectionState=None), 1))
    invalid.append((dict(observer_failure, selectionFailure=None), 1))
    invalid.append((dict(observer_failure, selectionFailure='/private/unsafe token=value'), 1))
    invalid.append((dict(heading_failure, selectionFailure='node-name:attribute_error'), 1))
    invalid.extend([(dict(yard_failure, selectionFailure=None), 1),
                    (dict(yard_failure, selectionFailure='heading-text-content:accessible_api'), 1),
                    (dict(yard_failure, selectionFailure='node-name:/private/unsafe'), 1),
                    (dict(yard_failure, failureCategory='timeout'), 1),
                    (dict(yard_failure, selectionState='owner-absent'), 1),
                    (dict(yard_failure, failureStage='matched.details'), 1)])
    for detail in ['state-set:ipc_live', 'node-role:unknown_live', 'node-role:ipc_unknown',
                   'node-role:unsafe private domain', 'node-role:ipc_live:unsafe private message']:
        invalid.append((dict(yard_failure, selectionFailure=detail), 1))
    invalid.extend([(dict(yard_failure, selectionFailure='node-role:ipc_live', failureCategory='unexpected'), 1),
                    (dict(heading_failure, selectionState='unobserved', selectionFailure='node-role:ipc_live'), 1)])
    invalid.extend([(dict(fingerprint_failure, selectionFailure=None), 1),
                    (dict(fingerprint_failure, selectionFailure='node-role:accessible_api'), 1),
                    (dict(fingerprint_failure, selectionFailure='text-content:/private/unsafe token=value'), 1),
                    (dict(fingerprint_failure, failureCategory='timeout'), 1),
                    (dict(fingerprint_failure, failureStage='matched.yard'), 1),
                    (dict(fingerprint_failure, failedCase='named-next-match'), 1),
                    (dict(fingerprint_failure, selectionState='unobserved'), 1)])
    invalid.extend([(dict(reconcile_failure, selectionFailure=None), 1),
                    (dict(reconcile_failure, selectionFailure='button-find:/private/unsafe token=value'), 1),
                    (dict(reconcile_failure, failureCategory='timeout'), 1),
                    (dict(reconcile_failure, failureStage='matched.yard'), 1),
                    (dict(reconcile_failure, failedCase='named-base-next'), 1),
                    (dict(reconcile_failure, selectionState='unobserved'), 1)])
    for value, rc in invalid:
        try:
            validate_summary(value, rc)
        except AssertionError:
            continue
        raise AssertionError('unsafe or incomplete summary accepted')
    print('ok: bounded release GUI summary validation')
    return 0


def private(path, directory=False):
    info = path.lstat()
    require(info.st_uid == os.geteuid(), 'ownership')
    require(stat.S_ISDIR(info.st_mode) if directory else stat.S_ISREG(info.st_mode), 'ownership')
    require(stat.S_IMODE(info.st_mode) == (0o700 if directory else 0o600), 'ownership')
    require(directory or info.st_nlink == 1, 'ownership')


def read(path, limit=131072):
    private(path)
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(fd) as stream:
        info = os.fstat(stream.fileno())
        assert stat.S_ISREG(info.st_mode) and info.st_uid == os.geteuid() and info.st_nlink == 1 and stat.S_IMODE(info.st_mode) == 0o600
        value = stream.read(limit + 1)
    assert len(value) <= limit
    return value


def write(path, value):
    private(path.parent, True)
    temporary = path.with_name(path.name + '.new')
    fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, 'w') as stream:
        os.fchmod(stream.fileno(), 0o600)
        stream.write(value)
    if path.exists() or path.is_symlink():
        private(path)
    os.replace(temporary, path)


def failure_png(fixture):
    """Export only the fixed screenshot from this marked synthetic fixture."""
    try:
        private(fixture, True)
        require(read(fixture / '.marker', 128) == 'subyard-veranda-release-gui-v1\n', 'ownership')
        path = fixture / 'failure.png'; private(path)
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
        with os.fdopen(fd, 'rb') as stream:
            info = os.fstat(stream.fileno())
            require(stat.S_ISREG(info.st_mode) and info.st_uid == os.geteuid() and info.st_nlink == 1
                    and stat.S_IMODE(info.st_mode) == 0o600 and info.st_size <= MAX_FAILURE_PNG, 'ownership')
            value = stream.read(MAX_FAILURE_PNG + 1)
        return value if valid_failure_png(value) else None
    except (OSError, FixtureFailure, AssertionError, UnicodeError):
        return None  # Optional evidence never changes the test outcome.


def private_case_stderr(path):
    require(path.name == 'gui-stderr.log' and path.parent.name in CASES, 'ownership')
    private(path.parent.parent, True)
    require(read(path.parent.parent / '.marker', 128) == 'subyard-veranda-release-gui-v1\n', 'ownership')
    private(path.parent, True)


def private_stderr(path):
    """Open only an owned case file; unavailable diagnostics do not prevent launch."""
    try:
        private_case_stderr(path)
        write(path, '')
        private(path)
        fd = os.open(path, os.O_WRONLY | os.O_APPEND | os.O_NOFOLLOW)
        with os.fdopen(fd, 'ab') as stream:
            info = os.fstat(stream.fileno())
            require(stat.S_ISREG(info.st_mode) and info.st_uid == os.geteuid() and info.st_nlink == 1
                    and stat.S_IMODE(info.st_mode) == 0o600, 'ownership')
            return os.dup(stream.fileno())
    except (OSError, FixtureFailure, AssertionError):
        return None


def stderr_diagnostic(path):
    try:
        private_case_stderr(path); private(path)
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
        with os.fdopen(fd, 'rb') as stream:
            info = os.fstat(stream.fileno())
            require(stat.S_ISREG(info.st_mode) and info.st_uid == os.geteuid() and info.st_nlink == 1
                    and stat.S_IMODE(info.st_mode) == 0o600, 'ownership')
            value = stream.read(MAX_STDERR_SCAN + 1)
        return {'scannedBytes': min(len(value), MAX_STDERR_SCAN), 'truncated': len(value) > MAX_STDERR_SCAN,
                'signatures': sorted(name for name, signature in STDERR_SIGNATURES.items()
                                     if signature in value[:MAX_STDERR_SCAN])}
    except (OSError, FixtureFailure, AssertionError):
        return None


def process_diagnostic(app, tree, live):
    counts = dict.fromkeys(PROCESS_ROLES, 0)
    owned = {pid: item for pid, item in live.items() if tree.known.get(pid) == item['start']}
    require(len(owned) <= 2048, 'ownership')
    for pid, item in owned.items():
        name = item['name']
        role = ('app' if pid == app.pid else 'web' if name in WEB_NAMES else 'network' if name in NETWORK_NAMES
                else 'ssh' if name == 'ssh' else 'rpc' if name in {'yard', 'yard-0.1.0', 'yard-0.1.1'} else 'other')
        counts[role] += 1
    rc = app.poll()
    state = 'running' if app.pid in owned and rc is None else 'exited' if rc is not None else 'unavailable'
    result = {'counts': counts, 'webSeen': bool(tree.names & WEB_NAMES), 'appState': state,
              'appExitCode': rc if type(rc) is int and -255 <= rc <= 255 else None}
    require(state != 'exited' or result['appExitCode'] is not None, 'ownership')
    return result


def validate_diagnostic(result):
    assert type(result) is dict and set(result) == {'schemaVersion', 'failedCase', 'failureStage',
                                                  'failureCategory', 'accessibleBound', 'stderr', 'processes'}
    assert type(result['schemaVersion']) is int and result['schemaVersion'] == 1
    assert result['failedCase'] in CASES | {'preparation'}
    assert result['failureStage'] in STAGES and result['failureCategory'] in CATEGORIES
    bound = result['accessibleBound']
    if bound is not None:
        assert type(bound) is dict and set(bound) == {'kind', 'observed', 'clamped'}
        assert bound['kind'] in ACCESSIBLE_LIMITS and result['failureCategory'] == 'accessible_bounds'
        assert type(bound['observed']) is int and -2147483648 <= bound['observed'] <= 2147483647
        assert type(bound['clamped']) is bool
        if bound['clamped']: assert bound['observed'] in {-2147483648, 2147483647}
        minimum, maximum = ACCESSIBLE_LIMITS[bound['kind']]
        assert not minimum <= bound['observed'] <= maximum
    stderr = result['stderr']
    if stderr is not None:
        assert type(stderr) is dict and set(stderr) == {'scannedBytes', 'truncated', 'signatures'}
        assert type(stderr['scannedBytes']) is int and 0 <= stderr['scannedBytes'] <= MAX_STDERR_SCAN
        assert type(stderr['truncated']) is bool and (not stderr['truncated'] or stderr['scannedBytes'] == MAX_STDERR_SCAN)
        assert type(stderr['signatures']) is list and len(stderr['signatures']) <= len(STDERR_SIGNATURES)
        assert all(type(name) is str and name in STDERR_SIGNATURES for name in stderr['signatures'])
        assert stderr['signatures'] == sorted(set(stderr['signatures']))
    processes = result['processes']
    if processes is not None:
        assert type(processes) is dict and set(processes) == {'counts', 'webSeen', 'appState', 'appExitCode'}
        counts = processes['counts']
        assert type(counts) is dict and set(counts) == PROCESS_ROLES
        assert all(type(value) is int and 0 <= value <= 2048 for value in counts.values())
        assert sum(counts.values()) <= 2048 and counts['app'] <= 1
        assert type(processes['webSeen']) is bool and processes['appState'] in {'running', 'exited', 'unavailable'}
        rc = processes['appExitCode']
        assert (type(rc) is int and -255 <= rc <= 255) if processes['appState'] == 'exited' else rc is None
        if processes['appState'] == 'running': assert counts['app'] == 1
        if counts['web']: assert processes['webSeen']
    assert len(json.dumps(result).encode('ascii')) <= MAX_DIAGNOSTIC


def failure_diagnostic(fixture):
    try:
        private(fixture, True)
        require(read(fixture / '.marker', 128) == 'subyard-veranda-release-gui-v1\n', 'ownership')
        result = json.loads(read(fixture / 'diagnostic.json', MAX_DIAGNOSTIC))
        validate_diagnostic(result)
        return result
    except (OSError, FixtureFailure, AssertionError, ValueError, TypeError, UnicodeError):
        return None


def diagnostic_self_test():
    import tempfile
    from types import SimpleNamespace
    class ApiError(Exception): pass
    class Node:
        def __init__(self, children=(), count=None): self.children = children; self.count = count
        def get_state_set(self): return self
        def contains(self, _): return False
        def get_child_count(self): return len(self.children) if self.count is None else self.count
        def get_child_at_index(self, index): return self.children[index]
    deep = Node()
    for _ in range(33): deep = Node([deep])
    trees = [(Node([Node() for _ in range(2048)]), 'node_count', 2049), (deep, 'depth', 33),
             (Node(count=-2), 'child_count', -2), (Node(count=2049), 'child_count', 2049)]
    for tree, kind, observed in trees:
        try: accessible_nodes(tree, 'defunct', ApiError)
        except AccessibleBoundFailure as error:
            assert error.category == 'accessible_bounds' and error.bound == {'kind': kind, 'observed': observed, 'clamped': False}
        else: raise AssertionError('distinct accessible bound was not retained')
    class SentinelNode(Node):
        reads = 0
        def get_state_set(self):
            self.reads += 1
            return self
        def contains(self, _): return self.reads > 1
        def get_child_at_index(self, _): raise AssertionError('failed child count was used')
    sentinel = SentinelNode(count=-1); points = []
    # The live sibling is visited first; its partial result must not escape.
    try: accessible_nodes(Node([sentinel, Node()]), 'defunct', ApiError, points.append)
    except ChildCountFailure as error:
        assert error.category == 'accessible_api' and points[-1] == 'child-count'
        detail = first_failure(None, error, points[-1], ApiError)
        assert detail == 'child-count:accessible_api'
        assert first_failure(detail, TimeoutError(), 'tree', ApiError) == detail
    else: raise AssertionError('incomplete sentinel observation was accepted')
    assert sentinel.reads == 1  # A failed GetState must not turn this into a skipped subtree.
    for count in [-1, 32769]:
        text_api = SimpleNamespace(get_character_count=lambda _: count, get_text=lambda *_: 'unsafe private text')
        try: interface_text(None, text_api)
        except AccessibleBoundFailure as error:
            assert error.bound == {'kind': 'text_length', 'observed': count, 'clamped': False}
        else: raise AssertionError('text bound was not retained')
    for kind, (minimum, maximum) in ACCESSIBLE_LIMITS.items():
        accessible_bound(kind, minimum); accessible_bound(kind, maximum)
    result = {'schemaVersion': 1, 'failedCase': 'remote-next-match', 'failureStage': 'growth.removal',
              'failureCategory': 'accessible_bounds', 'accessibleBound': {'kind': 'depth', 'observed': 33, 'clamped': False},
              'stderr': None, 'processes': None}
    validate_diagnostic(result)  # Missing optional observations preserve the failure.
    for kind in ACCESSIBLE_LIMITS:
        try: accessible_bound(kind, 1 << 40)
        except AccessibleBoundFailure as error:
            validate_diagnostic(dict(result, accessibleBound=error.bound))
            assert error.bound['observed'] == 2147483647 and error.bound['clamped'] is True
    tree = SimpleNamespace(known={1: 11, 2: 22, 3: 33}, names={'WebKitWebProces', '/private/unsafe'})
    live = {1: {'start': 11, 'name': 'subyard-veranda'}, 2: {'start': 222, 'name': 'WebKitWebProces'},
            3: {'start': 33, 'name': '/private/unsafe token=value'}}
    processes = process_diagnostic(SimpleNamespace(pid=1, poll=lambda: None), tree, live)
    assert processes['counts']['web'] == 0 and processes['counts']['app'] == 1 and processes['counts']['other'] == 1
    assert processes['webSeen'] and processes['appState'] == 'running'
    validate_diagnostic(dict(result, processes=processes))
    live[2]['start'] = 22
    assert process_diagnostic(SimpleNamespace(pid=1, poll=lambda: None), tree, live)['counts']['web'] == 1
    exited = process_diagnostic(SimpleNamespace(pid=1, poll=lambda: -11), tree, {})
    assert exited['appState'] == 'exited' and exited['appExitCode'] == -11
    validate_diagnostic(dict(result, processes=exited))
    with tempfile.TemporaryDirectory(prefix='veranda-gui-diagnostic-') as directory:
        fixture = Path(directory); fixture.chmod(0o700)
        write(fixture / '.marker', 'subyard-veranda-release-gui-v1\n')
        case = fixture / 'remote-next-match'; case.mkdir(mode=0o700); case.chmod(0o700)
        path = case / 'gui-stderr.log'
        assert failure_diagnostic(fixture) is None and stderr_diagnostic(path) is None
        fd = private_stderr(path); assert fd is not None
        with os.fdopen(fd, 'ab') as stream:
            stream.write(b'/private/unsafe token=value\xff\nGtk-CRITICAL\nlibEGL warning\n')
        private(path)
        stderr = stderr_diagnostic(path)
        assert stderr['signatures'] == ['egl_warning', 'gtk_critical'] and not stderr['truncated']
        write(fixture / 'diagnostic.json', json.dumps(dict(result, stderr=stderr, processes=processes)))
        assert failure_diagnostic(fixture) == dict(result, stderr=stderr, processes=processes)
        assert 'unsafe' not in json.dumps(failure_diagnostic(fixture))
        for signature in STDERR_SIGNATURES.values():
            path.write_bytes(signature + b' /private/unsafe')
            assert len(stderr_diagnostic(path)['signatures']) == 1
        path.write_bytes(b'\0' * MAX_STDERR_SCAN + b'Gtk-CRITICAL /private/unsafe')
        assert stderr_diagnostic(path) == {'scannedBytes': MAX_STDERR_SCAN, 'truncated': True, 'signatures': []}
        path.chmod(0o644); assert stderr_diagnostic(path) is None; path.chmod(0o600)
        backing = case / 'backing'; path.rename(backing); path.symlink_to(backing)
        assert stderr_diagnostic(path) is None and private_stderr(path) is None
        path.unlink(); backing.rename(path)
        hardlink = case / 'hardlink'; os.link(path, hardlink)
        assert stderr_diagnostic(path) is None; hardlink.unlink()
        case.chmod(0o755); assert stderr_diagnostic(path) is None; case.chmod(0o700)
        for unsafe in [dict(result, rawError='/private/unsafe'), dict(result, failureCategory='/private/unsafe')]:
            write(fixture / 'diagnostic.json', json.dumps(unsafe))
            assert failure_diagnostic(fixture) is None
        write(fixture / '.marker', 'unsafe private marker\n')
        assert failure_diagnostic(fixture) is None and stderr_diagnostic(path) is None
    for invalid in [dict(result, rawError='/private/unsafe'), dict(result, failureCategory='/private/unsafe'),
                    dict(result, accessibleBound={'kind': 'depth', 'observed': 32, 'clamped': False}),
                    dict(result, accessibleBound={'kind': '/private/unsafe', 'observed': 33, 'clamped': False}),
                    dict(result, stderr={'scannedBytes': 1, 'truncated': False, 'signatures': ['/private/unsafe']}),
                    dict(result, processes=dict(processes, rawNames=['/private/unsafe'])),
                    dict(result, processes=dict(processes, counts=dict(processes['counts'], other=2049)))]:
        try: validate_diagnostic(invalid)
        except (AssertionError, TypeError): pass
        else: raise AssertionError('unsafe diagnostic accepted')


def main():
    import gi
    gi.require_version('Atspi', '2.0')
    from gi.repository import Atspi, GLib
    root, fixture, owner = map(Path, sys.argv[1:4])
    yard, storage = sys.argv[4:6]
    growth = sys.argv[6:] == ['--growth']
    assert sys.argv[6:] in [[], ['--growth']]
    growth_samples = []; warmup_completed = cycles_completed = 0
    private(owner, True); private(fixture, True)
    assert read(fixture / '.marker', 128) == 'subyard-veranda-release-gui-v1\n'
    assert read(owner / '.marker', 128) == 'subyard-veranda-owner-v1-' + owner.name.split('.')[-1].lower() + '\n'
    assert os.environ.get('DBUS_SESSION_BUS_ADDRESS') and str(fixture).startswith(str(owner) + '/')
    assert ctypes.CDLL(None).prctl(36, 1, 0, 0, 0) == 0  # Reap owned orphaned helpers.
    spec = importlib.util.spec_from_file_location('measure', root / 'dev/measure-veranda.py')
    measure = importlib.util.module_from_spec(spec); spec.loader.exec_module(measure)
    children = []; app = None; app_tree = None; gui = None; display = None; server = None
    app_stderr = None; bound_failure = None
    passed = []; current = 'preparation'; cleanup = True; stage = 'preparation'; category = None; selection = None; selection_error = None; selection_point = 'tree'; yard_point = 'tree'; fingerprint_point = 'tree'; reconcile_point = 'tree'; yard_end = time.monotonic()
    heading_point = 'tree'; heading_site = 'local'; observing = False
    remote_select_point = None
    def boundary(value):
        nonlocal stage
        assert value in STAGES
        stage = value
    duration = 1800 if growth else 300
    deadline = time.monotonic() + duration
    alarm_error = TimeoutError()
    signal.signal(signal.SIGALRM, lambda *_: (_ for _ in ()).throw(alarm_error))
    signal.signal(signal.SIGTERM, lambda *_: (_ for _ in ()).throw(TimeoutError()))
    signal.signal(signal.SIGINT, lambda *_: (_ for _ in ()).throw(TimeoutError()))
    signal.alarm(duration)
    base_env = {'PATH': '/usr/sbin:/usr/bin:/bin', 'LANG': 'C.UTF-8',
                'HOME': str(fixture / 'owner-home'), 'SUBYARD_OPERATOR_HOME': str(fixture / 'operator'),
                'SUBYARD_CONFIG_HOME': str(owner / 'config'), 'SUBYARD_HOME': str(owner / 'data'),
                'SUBYARD_REPOSITORY_ROOT': str(root), 'SUBYARD_NO_AUDIT': '1', 'STORAGE_PATH': storage}

    def spawn(args, env, log):
        write(fixture / log, '')
        with open(fixture / log, 'ab') as output:
            process = subprocess.Popen(args, env=env, stdout=output, stderr=output, start_new_session=True)
        tree = measure.Tree(process.pid); tree.sample(); children.append((process, tree))
        return process

    def stop(process, tree):
        tree.sample()
        for sig in [signal.SIGTERM, signal.SIGKILL]:
            live = measure.processes()
            for pid, start in tree.known.items():
                if live.get(pid, {}).get('start') == start:
                    try: os.kill(pid, sig)
                    except ProcessLookupError: pass
            try: process.wait(timeout=3)
            except subprocess.TimeoutExpired: continue
            # The direct child exiting does not prove its WebKit/RPC descendants exited.
            end = time.monotonic() + 2
            while time.monotonic() < end:
                live = measure.processes()
                remaining = [pid for pid, start in tree.known.items() if live.get(pid, {}).get('start') == start]
                for pid in remaining:
                    try: os.waitpid(pid, os.WNOHANG)
                    except ChildProcessError: pass
                if not remaining: return
                time.sleep(0.05)
        live = measure.processes()
        assert not any(live.get(pid, {}).get('start') == start for pid, start in tree.known.items())

    def wait(predicate, seconds=20):
        nonlocal yard_end
        def snapshot():
            return (selection_error, heading_site, selection_point, heading_point,
                    yard_point, fingerprint_point, reconcile_point)
        def restore(saved):
            nonlocal selection_error, heading_site, selection_point, heading_point
            nonlocal yard_point, fingerprint_point, reconcile_point
            (selection_error, heading_site, selection_point, heading_point,
             yard_point, fingerprint_point, reconcile_point) = saved
        def failed(error):
            heading_failed(error, heading_point)
            yard_failure(error, yard_point)
            fingerprint_failure(error, fingerprint_point)
            reconcile_failure(error, reconcile_point)
        baseline = snapshot(); first = None
        try:
            end = min(deadline - 12, time.monotonic() + seconds)
            yard_end = end
            while time.monotonic() < end:
                if app_tree: app_tree.sample()
                for _, tree in children: tree.sample()
                try:
                    value = predicate()
                except (GLib.Error, ChildCountFailure) as error:
                    if isinstance(error, GLib.Error) and (error.domain != 'atspi_error' or error.code not in {0, 1}): raise
                    failed(error)
                    if first is None: first = (error, snapshot())
                    if time.monotonic() >= end:
                        restore(first[1]); raise first[0]
                    restore(baseline)
                    time.sleep(0.05)
                    continue  # Discard the complete observation; never accept its partial result.
                if value:
                    if first: restore(baseline)
                    return value
                time.sleep(0.05)
            if first: restore(first[1]); raise first[0]
            raise TimeoutError()
        except BaseException as error:
            if error is alarm_error and first and time.monotonic() >= deadline:
                restore(first[1]); raise first[0]
            failed(error)
            raise

    def yard_checkpoint(value):
        nonlocal yard_point
        assert value in YARD_CHECKPOINTS
        if stage == 'matched.yard': yard_point = value

    def yard_failure(error, point, detail=None):
        nonlocal selection_error
        if stage == 'matched.yard' and selection_error is None:
            selection_error = detail or selection_failure(error, point, GLib.Error)

    def nodes(start=None):
        if stage == 'matched.heading' and not observing:
            return accessible_nodes(start or gui, Atspi.StateType.DEFUNCT, GLib.Error, heading_checkpoint, heading_failed)
        if stage == 'remote.fingerprint':
            return accessible_nodes(start or gui, Atspi.StateType.DEFUNCT, GLib.Error, fingerprint_checkpoint, fingerprint_failure)
        if stage == 'matched.reconcile':
            return accessible_nodes(start or gui, Atspi.StateType.DEFUNCT, GLib.Error, reconcile_checkpoint, reconcile_failure)
        return accessible_nodes(start or gui, Atspi.StateType.DEFUNCT, GLib.Error, yard_checkpoint, yard_failure)

    def heading_checkpoint(value):
        nonlocal heading_point
        assert value in HEADING_CHECKPOINTS
        if stage == 'matched.heading' and not observing: heading_point = value

    def heading_failed(error, point, detail=None):
        nonlocal selection_error
        if stage == 'matched.heading' and not observing:
            selection_error = heading_failure(selection_error, error, heading_site, point, GLib.Error, detail)

    def fingerprint_checkpoint(value):
        nonlocal fingerprint_point
        assert value in FINGERPRINT_CHECKPOINTS
        if stage == 'remote.fingerprint': fingerprint_point = value

    def fingerprint_failure(error, point):
        nonlocal selection_error
        if stage == 'remote.fingerprint':
            selection_error = first_failure(selection_error, error, point, GLib.Error)

    def reconcile_checkpoint(value):
        nonlocal reconcile_point
        assert value in RECONCILE_CHECKPOINTS
        if stage == 'matched.reconcile': reconcile_point = value

    def reconcile_failure(error, point, detail=None):
        nonlocal selection_error
        if stage == 'matched.reconcile' and selection_error is None:
            selection_error = detail or selection_failure(error, point, GLib.Error)

    buttons = {Atspi.Role.PUSH_BUTTON, Atspi.Role.TOGGLE_BUTTON}

    def selection_checkpoint(value):
        nonlocal selection_point
        assert value in SELECTION_CHECKPOINTS
        selection_point = value

    def find(name, roles):
        nonlocal selection_error
        found = []
        for node in nodes():
            point = 'node-role'
            try:
                heading_checkpoint(point)
                reconcile_checkpoint(point)
                if node.get_role() not in roles: continue
                point = 'node-name'
                heading_checkpoint(point)
                reconcile_checkpoint(point)
                if node.get_name() == name: found.append(node)
            except GLib.Error as error:
                if stage == 'matched.heading' and not observing:
                    detail = yard_error_detail(error, point, lambda: yard_state(node, Atspi, min(yard_end, deadline - 12)))
                    heading_failed(error, point, detail)
                elif stage == 'matched.reconcile':
                    detail = yard_error_detail(error, point, lambda: yard_state(node, Atspi, min(yard_end, deadline - 12)))
                    reconcile_failure(error, point, detail)
                elif stage in METADATA_CASES and not observing and selection_error is None:
                    selection_error = yard_error_detail(error, point, lambda: yard_state(node, Atspi, min(yard_end, deadline - 12)))
                raise
        heading_checkpoint('unique')
        require(len(found) <= 1, 'ambiguity')
        return found[0] if found else None

    def act(node):
        require(node.get_state_set().contains(Atspi.StateType.ENABLED), 'accessible_action')
        interface = node.get_action_iface()
        require(interface and interface.get_n_actions() == 1 and interface.do_action(0), 'accessible_action')

    def click(name, roles=buttons):
        act(wait(lambda: find(name, roles)))

    def enter_destination(entry, destination):
        require(re.fullmatch(r'[a-z_][a-z0-9_-]{0,31}@127\.0\.0\.1:[0-9]{4,5}', destination) is not None, 'fixture_data')
        require(1024 <= int(destination.rsplit(':', 1)[1]) <= 65535, 'fixture_data')
        server_start = next(tree.known.get(server.pid) for process, tree in children if process is server)
        app_start = app_tree.known.get(app.pid)
        require(server_start is not None and app_start is not None, 'ownership')

        def owned_input():
            private(owner, True); private(fixture, True); private(fixture / current, True)
            require(read(fixture / '.marker', 128) == 'subyard-veranda-release-gui-v1\n', 'ownership')
            require(re.fullmatch(r':[0-9]{1,6}', display) is not None
                    and app.poll() is None and server.poll() is None, 'ownership')
            live = measure.processes()
            for process, start in [(server, server_start), (app, app_start)]:
                require(live.get(process.pid, {}).get('start') == start
                        and Path('/proc', str(process.pid)).stat().st_uid == os.geteuid(), 'ownership')
            return live

        def xdo(args, capture=False):
            owned_input()
            remaining = min(3, deadline - 12 - time.monotonic())
            require(remaining > 0, 'timeout')
            result = subprocess.run(['xdotool', *args], env=dict(base_env, DISPLAY=display),
                                    stdout=subprocess.PIPE if capture else subprocess.DEVNULL,
                                    stderr=subprocess.DEVNULL, check=True, timeout=remaining)
            if capture:
                require(len(result.stdout) <= 1024 and result.stdout.isascii(), 'ownership')
                return result.stdout

        boundary('remote.window')
        windows = xdo(['search', '--onlyvisible', '--pid', str(app.pid)], True)
        require(len(windows.split()) == 1, 'ambiguity')
        candidate = windows.strip()
        require(candidate.isdigit() and len(candidate) <= 20, 'ownership')
        pid = xdo(['getwindowpid', candidate.decode('ascii')], True).strip()
        require(pid.isdigit() and len(pid) <= 20, 'ownership')
        live = owned_input()
        window = input_window(windows, int(pid), app.pid, app_start, live[app.pid]['start'],
                              Path('/proc', str(app.pid)).stat().st_uid)
        boundary('remote.focus')
        xdo(['windowfocus', '--sync', window])
        component = entry.get_component_iface()
        require(component is not None and Atspi.Component.grab_focus(component), 'accessible_action')
        wait(lambda: entry.get_state_set().contains(Atspi.StateType.FOCUSED))
        require(xdo(['getwindowfocus'], True).strip() == window.encode('ascii'), 'ownership')
        interface = entry.get_text_iface()
        require(interface is not None and interface_text(interface, Atspi.Text) == '', 'accessible_action')
        boundary('remote.input')
        # No --window: xdotool uses real XTEST events for the verified focus.
        xdo(['type', '--clearmodifiers', '--delay', '5', '--', destination])
        boundary('remote.readback')
        wait(lambda: interface_text(interface, Atspi.Text) == destination)

    def text(start=None):
        return accessible_text(lambda: nodes(start), Atspi.Text, fingerprint_checkpoint)

    def route(transport, expected_yard, version):
        records = [json.loads(line) for line in read(fixture / (transport + '-route.log')).splitlines()]
        assert all(set(item) == {'route', 'version'} and item['route'] in {'default', 'default-yard', 'named'}
                   and item['version'] in {'0.1.0', '0.1.1'} for item in records)
        return {'route': expected_yard, 'version': version} in records

    def launch(version, case):
        nonlocal app, app_tree, gui, app_stderr
        boundary('launch.ready')
        paths = {name: case / name for name in ['home', 'data', 'cache', 'config', 'runtime']}
        for path in paths.values():
            path.mkdir(mode=0o700, exist_ok=True); path.chmod(0o700); private(path, True)
        env = dict(base_env, PATH=str(fixture / 'bin') + ':' + base_env['PATH'], HOME=str(paths['home']),
                   XDG_DATA_HOME=str(paths['data']), XDG_CACHE_HOME=str(paths['cache']),
                   XDG_CONFIG_HOME=str(paths['config']), XDG_RUNTIME_DIR=str(paths['runtime']),
                   DISPLAY=display, SSH_AUTH_SOCK=str(fixture / 'agent.sock'),
                   DBUS_SESSION_BUS_ADDRESS=os.environ['DBUS_SESSION_BUS_ADDRESS'], VERANDA_RESOURCE_PROBE='1')
        app_stderr = case / 'gui-stderr.log'
        stderr_fd = private_stderr(app_stderr)
        try:
            app, app_tree, _ = measure.launch(fixture / ('base' if version == '0.1.0' else 'next') / 'usr/bin/subyard-veranda',
                                             env, 20, stderr=stderr_fd if stderr_fd is not None else subprocess.DEVNULL)
        finally:
            if stderr_fd is not None: os.close(stderr_fd)
        def accessible():
            desktop = Atspi.get_desktop(0)
            found = []
            for index in range(desktop.get_child_count()):
                candidate = desktop.get_child_at_index(index)
                if candidate is None: continue
                try:
                    if candidate.get_state_set().contains(Atspi.StateType.DEFUNCT): continue
                    if candidate.get_process_id() in app_tree.known: found.append(candidate)
                except GLib.Error:
                    if candidate.get_state_set().contains(Atspi.StateType.DEFUNCT): continue
                    raise
            require(len(found) <= 1, 'ambiguity')
            return found[0] if found else None
        boundary('launch.accessible'); gui = wait(accessible)
        boundary('launch.connect-link'); wait(lambda: find('+ Connect remote host', buttons))

    def close():
        nonlocal app, app_tree, gui
        if app: stop(app, app_tree)
        app = app_tree = gui = None

    def matched(named=False, site='local'):
        nonlocal heading_site, heading_point
        assert site in HEADING_CASES
        heading_site = site; heading_point = 'tree'
        boundary('matched.heading')
        wait(lambda: find('Projects' if named else 'Overview', {Atspi.Role.HEADING}))
        boundary('matched.yard')
        def yard_present():
            yard_checkpoint('tree')
            for node in nodes():
                if yard_node_matches(node, buttons, 'Show yard ' + yard, GLib.Error, yard_checkpoint, yard_failure,
                                     lambda current: yard_state(current, Atspi, min(yard_end, deadline - 12))): return True
            return False
        wait(yard_present)
        boundary('matched.details')
        wait(lambda: 'Loading host details' not in text() and 'Loading yard details' not in text())
        boundary('matched.alerts')
        assert not any(n.get_role() == Atspi.Role.ALERT for n in nodes())
        if named:
            assert find(yard, {Atspi.Role.HEADING})
            boundary('matched.reconcile')
            wait(lambda: reconcile_ready(lambda: find('Reconcile yard', buttons), Atspi.StateType.ENABLED, reconcile_checkpoint))

    def mismatch(version, engine):
        expected = f'Veranda {version} and Subyard {engine} are incompatible. Install both from the same release.'
        boundary('mismatch.message')
        wait(lambda: expected in text())
        boundary('mismatch.mutations')
        mutations = {'Start yard', 'Stop yard', 'Reconcile yard', 'Reconcile profile', 'Apply plan', 'Create yard',
                     'Preview creation', 'Preview change', 'Preview unset', 'Preview pull', 'Preview push',
                     'Preview repository connection', 'Sync now', 'Edit'}
        assert not any(n.get_role() in buttons and n.get_name() in mutations
                       and n.get_state_set().contains(Atspi.StateType.ENABLED) for n in nodes())

    try:
        # Ensure startup cannot cache the named session before the version switch.
        # This is inventory configuration only; no default Incus yard is initialized.
        default = owner / 'config/yards/default'
        assert not default.exists() and not default.is_symlink()
        default.mkdir(mode=0o700)
        write(default / 'config.env', 'YARD_KIND=container\nSSH_PORT=64996\nLIMITS_MEMORY=512MiB\n'
              + 'HOST_BASE=' + str(fixture / 'default-host') + '\nRESTRICTED_DISK_PATHS=' + str(fixture / 'default-host') + '\nENVIRONMENT_PROFILES=\nCODING_TOOL_INTEGRATIONS=\n')
        (fixture / 'bin').mkdir(mode=0o700)
        # Fixed RPC forms only; these wrappers never rewrite a protocol response.
        for transport in ['local', 'remote']:
            write(fixture / (transport + '-route.log'), '')
            source = 'import json,os,pathlib,shlex,sys\n'
            source += 'root=pathlib.Path(' + repr(str(fixture)) + ')\nyard=' + repr(yard) + '\n'
            source += 'args=' + ('sys.argv[1:]' if transport == 'local' else 'shlex.split(os.environ.get("SSH_ORIGINAL_COMMAND",""))') + '\n'
            if transport == 'remote': source += 'assert args and args[0]=="yard"\nargs=args[1:]\n'
            source += 'assert args in [["rpc","--stdio"],["-Y","default","rpc","--stdio"],["-Y",yard,"rpc","--stdio"]]\n'
            source += 'version=(root/' + repr(transport + '-version') + ').read_text().strip()\nassert version in ["0.1.0","0.1.1"]\n'
            source += 'route="default" if args[0]!="-Y" else "default-yard" if args[1]=="default" else "named"\n'
            source += 'with open(root/' + repr(transport + '-route.log') + ',"a") as output: output.write(json.dumps({"route":route,"version":version})+"\\n")\n'
            source += 'engine=str(root/("yard-"+version))\nos.execve(engine,[engine]+args,' + repr(base_env) + ')\n'
            path = fixture / 'bin' / ('yard' if transport == 'local' else 'owner-rpc')
            write(path, '#!/usr/bin/python3\n' + source); path.chmod(0o700)
        boundary('preparation.ssh')
        for key in ['host-key', 'agent-key']:
            subprocess.run(['ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-f', str(fixture / key)],
                           env=base_env, check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=5)
            (fixture / key).chmod(0o600); (fixture / (key + '.pub')).chmod(0o600)
            private(fixture / key)
        write(fixture / 'authorized_keys', read(fixture / 'agent-key.pub'))
        fingerprint = subprocess.check_output(['ssh-keygen', '-lf', str(fixture / 'host-key.pub'), '-E', 'sha256'],
                                             env=base_env, stderr=subprocess.DEVNULL, timeout=3).decode().split()[1]
        assert fingerprint.startswith('SHA256:') and len(fingerprint) < 100
        agent_env = dict(base_env, SSH_AUTH_SOCK=str(fixture / 'agent.sock'))
        spawn(['ssh-agent', '-D', '-a', agent_env['SSH_AUTH_SOCK']], base_env, 'agent.log')
        wait(lambda: (fixture / 'agent.sock').is_socket(), 3)
        subprocess.run(['ssh-add', str(fixture / 'agent-key')], env=agent_env, check=True,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=3)
        import pwd
        account = pwd.getpwuid(os.geteuid()).pw_name
        config = ['ListenAddress 127.0.0.1', 'HostKey ' + str(fixture / 'host-key'),
                  'PidFile ' + str(fixture / 'sshd.pid'), 'AuthorizedKeysFile ' + str(fixture / 'authorized_keys'),
                  'AllowUsers ' + account, 'AuthenticationMethods publickey', 'PasswordAuthentication no',
                  'KbdInteractiveAuthentication no', 'UsePAM no', 'PermitRootLogin prohibit-password',
                  # /var/tmp is shared; exact private fixture ownership/modes are checked above.
                  'StrictModes no', 'AllowTcpForwarding no', 'AllowAgentForwarding no', 'X11Forwarding no',
                  'PermitTTY no', 'PermitUserRC no', 'MaxStartups 16', 'PerSourcePenalties no', 'LogLevel QUIET',
                  'ForceCommand ' + str(fixture / 'bin/owner-rpc')]
        write(fixture / 'sshd_config', '\n'.join(config) + '\n')
        write(fixture / 'sshd.log', ''); write(fixture / 'known_hosts', '')
        helper = 'source ' + shlex.quote(str(root / 'tests/helpers/loopback-sshd.sh')) + '\n'
        helper += 'start_loopback_sshd ' + shlex.quote(str(fixture)) + ' /usr/sbin/sshd 43771 || exit 1\n'
        helper += 'printf "%s" "$port" > ' + shlex.quote(str(fixture / 'port')) + '\nwait "$sshd_pid"\n'
        write(fixture / 'port', '')
        listener = spawn(['/bin/bash', '-c', helper], base_env, 'listener.log')
        listener_tree = next(tree for process, tree in children if process is listener)
        port = wait(lambda: read(fixture / 'port', 5), 12)
        assert port.isdecimal() and 1024 <= int(port) <= 65535
        destination = account + '@127.0.0.1:' + port
        boundary('preparation.display')
        readfd, writefd = os.pipe()
        server = subprocess.Popen(['Xvfb', '-displayfd', str(writefd), '-screen', '0', '1280x800x24', '-nolisten', 'tcp'],
                                  env=base_env, pass_fds=(writefd,), stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)
        children.append((server, measure.Tree(server.pid))); os.close(writefd)
        with selectors.DefaultSelector() as selector:
            selector.register(readfd, selectors.EVENT_READ); assert selector.select(3)
        display = ':' + os.read(readfd, 32).decode().strip(); os.close(readfd)
        boundary('preparation.accessibility')
        subprocess.run(['gsettings', 'set', 'org.gnome.desktop.interface', 'toolkit-accessibility', 'true'],
                       env=dict(os.environ, DISPLAY=display), check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=3)
        Atspi.set_timeout(500, 1000); Atspi.init()
        for context in ['remote'] if growth else ['default', 'named', 'remote']:
            for pair, version, engine in [('next-match', '0.1.1', '0.1.1')] if growth else [('next-match', '0.1.1', '0.1.1'), ('base-next', '0.1.0', '0.1.1'), ('next-base', '0.1.1', '0.1.0')]:
                current = context + '-' + pair
                case = fixture / current; case.mkdir(mode=0o700)
                for transport in ['local', 'remote']:
                    write(fixture / (transport + '-route.log'), '')
                    write(fixture / (transport + '-version'), engine if context == 'default' else version)
                launch(version, case)
                if context != 'default' or version == engine:
                    boundary('select.owner')
                    owners = wait(lambda: [n for n in nodes() if n.get_role() in buttons and (n.get_name() or '').startswith('Show owner ')])
                    require(len(owners) == 1, 'ambiguity')
                    act(owners[0]); matched(site='pre-remote' if context == 'remote' else 'local')
                if context == 'named':
                    assert not route('local', 'named', version)
                    write(fixture / 'local-version', engine)
                    boundary('select.yard')
                    click('Show yard ' + yard)
                if context == 'remote':
                    listener_tree.sample()
                    listener_baseline = dict(listener_tree.known)
                    boundary('remote.form')
                    click('+ Connect remote host')
                    boundary('remote.destination')
                    entry = wait(lambda: find('SSH destination', {Atspi.Role.ENTRY}))
                    enter_destination(entry, destination)
                    boundary('remote.assess')
                    click('Test connection')
                    boundary('remote.fingerprint')
                    wait(lambda: fingerprint in text())
                    boundary('remote.consent')
                    click('I verified and accept this host key', {Atspi.Role.CHECK_BOX})
                    boundary('remote.connect')
                    click('Connect and save')
                    boundary('remote.saved')
                    wait(lambda: find('Repair connection', buttons))
                    matched(site='post-save')
                    boundary('remote.route')
                    assert route('remote', 'default', version)
                    if growth:
                        # Keep the same GUI/WebKit/native/local RPC processes throughout.
                        # The first destination uses owned XTEST; Svelte retains it thereafter.
                        for cycle in range(-9, 101):
                            app_tree.sample(); listener_tree.sample()
                            live = measure.processes()
                            ssh = {pid: start for pid, start in app_tree.known.items()
                                   if live.get(pid, {}).get('start') == start and live[pid]['name'] == 'ssh'}
                            remote_children = {pid: start for pid, start in listener_tree.known.items()
                                               if listener_baseline.get(pid) != start and live.get(pid, {}).get('start') == start}
                            require(ssh and remote_children, 'process')
                            boundary('growth.removal')
                            click('Remove connection'); click('Confirm removal')
                            def removed():
                                live = measure.processes()
                                gone = all(live.get(pid, {}).get('start') != start for pid, start in (ssh | remote_children).items())
                                owners = [node for node in nodes() if node.get_role() in buttons
                                          and (node.get_name() or '').startswith('Show owner ')]
                                return gone and len(owners) == 1 and find('+ Connect remote host', buttons) and not find('Confirm removal', buttons)
                            wait(removed)
                            require(not any(info['name'] == 'ssh' for info in app_tree.sample()['processes']), 'process')
                            if cycle <= 0:
                                warmup_completed += 1
                            else:
                                if cycle <= 10 or cycle >= 91:
                                    boundary('growth.idle')
                                    # Sample during settling to retain descendant identities; no UI input.
                                    until = time.monotonic() + 30
                                    while time.monotonic() < until:
                                        require(app.poll() is None and time.monotonic() < deadline - 12, 'timeout')
                                        app_tree.sample(); time.sleep(0.1)
                                    sample = app_tree.sample()
                                    growth_samples.append({'cycle': cycle, 'rssBytes': sample['rss_bytes'],
                                                           'processCount': sample['process_count']})
                                cycles_completed = cycle
                            if cycle == 100: break
                            boundary('remote.form'); click('+ Connect remote host')
                            boundary('remote.destination')
                            entry = wait(lambda: find('SSH destination', {Atspi.Role.ENTRY}))
                            require(interface_text(entry.get_text_iface(), Atspi.Text) == destination, 'assertion')
                            boundary('remote.assess'); click('Test connection')
                            boundary('remote.fingerprint'); wait(lambda: fingerprint in text())
                            boundary('remote.consent'); click('I verified and accept this host key', {Atspi.Role.CHECK_BOX})
                            boundary('remote.connect'); click('Connect and save')
                            boundary('remote.saved'); wait(lambda: find('Repair connection', buttons))
                            matched(site='post-save')
                        boundary('growth.budget')
                        require(growth_result(growth_samples, warmup_completed, cycles_completed)['withinLimit'], 'resource_budget')
                        boundary('case.cleanup'); close(); passed.append(current)
                        continue
                    boundary('remote.reload')
                    close(); write(fixture / 'remote-route.log', '')
                    write(fixture / 'remote-version', engine); launch(version, case)
                    # Refresh may select the compatible local owner: select the saved remote explicitly.
                    def remote_button():
                        found = [n for n in nodes() if n.get_role() in buttons and (n.get_name() or '').startswith('Show owner ')
                                 and destination in text(n)]
                        require(len(found) <= 1, 'ambiguity')
                        return found[0] if found else None
                    remote_select_point = 'owner-lookup'
                    boundary('remote.select')
                    node = wait(remote_button)
                    remote_select_point = 'owner-action'
                    act(node)
                    boundary('remote.state')
                    wait(lambda: find('Repair connection', buttons) or find('Fleet unavailable', {Atspi.Role.HEADING}))
                boundary('route')
                wait(lambda: route('remote' if context == 'remote' else 'local', 'named' if context == 'named' else 'default', engine))
                if version == engine: matched(context == 'named', site='post-reload' if context == 'remote' else 'local')
                else: mismatch(version, engine)
                boundary('screenshot')
                write(fixture / (current + '.png'), '')
                require(measure.screenshot(display, fixture / (current + '.png')), 'screenshot')
                (fixture / (current + '.png')).chmod(0o600); private(fixture / (current + '.png'))
                boundary('case.cleanup')
                close(); passed.append(current)
    except FixtureFailure as failure:
        category = failure.category
        if isinstance(failure, AccessibleBoundFailure): bound_failure = failure.bound
    except (TimeoutError, subprocess.TimeoutExpired):
        category = 'timeout'
    except (GLib.Error, AttributeError):
        category = 'accessible_api'
    except subprocess.CalledProcessError:
        category = 'process'
    except (OSError, ValueError):
        category = 'fixture_data'
    except AssertionError:
        category = 'assertion'
    except RuntimeError:
        category = 'process' if stage == 'launch.ready' else 'unexpected'
    except BaseException:
        category = 'unexpected'  # Never export exception text, UI contents, keys or paths.
    finally:
        if (stage == 'remote.select' and category == 'timeout' and selection_error is None
                and remote_select_point in {'owner-lookup', 'owner-action'}):
            selection_error = remote_select_point + ':timeout'
        if stage == 'matched.heading' and category is not None:
            observing = True
            if selection_error in HEADING_FAILURES: category = heading_category(selection_error)
            try:
                selection = observe_selection(nodes, buttons, Atspi.Role.HEADING, Atspi.StateType.PRESSED, selection_checkpoint, Atspi.Text)
            except BaseException as error:
                selection = 'unobserved'; selection_error = first_failure(selection_error, error, selection_point, GLib.Error)
        if category is not None:
            try:
                private(owner, True); private(fixture, True)
                require(read(fixture / '.marker', 128) == 'subyard-veranda-release-gui-v1\n', 'ownership')
                processes = None
                if app and app_tree:
                    try:
                        app_tree.sample()
                        processes = process_diagnostic(app, app_tree, measure.processes())
                    except BaseException: pass
                diagnostic = {'schemaVersion': 1, 'failedCase': current, 'failureStage': stage,
                              'failureCategory': category, 'accessibleBound': bound_failure,
                              'stderr': stderr_diagnostic(app_stderr) if app_stderr is not None else None,
                              'processes': processes}
                validate_diagnostic(diagnostic)
                write(fixture / 'diagnostic.json', json.dumps(diagnostic))
            except BaseException:
                pass  # Optional diagnostics preserve the original failure and cleanup.
        if category is not None and app and app_tree and display and server and time.monotonic() < deadline:
            try:
                # This display was created above, never inherited. The app's
                # private case directories contain only synthetic owner data.
                private(owner, True); private(fixture, True)
                require(read(fixture / '.marker', 128) == 'subyard-veranda-release-gui-v1\n', 'ownership')
                require(current in CASES and app.poll() is None and server.poll() is None, 'ownership')
                private(fixture / current, True)
                for name in ['home', 'data', 'cache', 'config', 'runtime']:
                    private(fixture / current / name, True)
                app_tree.sample(); live = measure.processes()
                require(app.pid in app_tree.known and live.get(app.pid, {}).get('start') == app_tree.known[app.pid], 'ownership')
                write(fixture / 'failure.png', '')
                if measure.screenshot(display, fixture / 'failure.png'):
                    (fixture / 'failure.png').chmod(0o600); private(fixture / 'failure.png')
            except BaseException:
                pass  # Preserve the original stage/category and cleanup result.
        signal.alarm(0)
        try: close()
        except BaseException: cleanup = False
        for process, tree in reversed(children):
            try: stop(process, tree)
            except BaseException: cleanup = False
        if not cleanup and category is None:
            stage = 'cleanup'; category = 'cleanup'
        private(fixture, True)
        assert read(fixture / '.marker', 128) == 'subyard-veranda-release-gui-v1\n'
        expected_count = 1 if growth else 9
        result = {'passed': passed, 'failedCase': None if len(passed) == expected_count else current, 'cleanup': cleanup,
                  'failureStage': stage if category else None, 'failureCategory': category, 'selectionState': selection, 'selectionFailure': selection_error}
        if growth:
            result['growth'] = growth_result([sample for sample in growth_samples if sample['cycle'] <= cycles_completed], warmup_completed, cycles_completed)
        validate_summary(result, 0 if len(passed) == expected_count and cleanup else 1, growth)
        write(fixture / 'summary.json', json.dumps(result))
    return 0 if len(passed) == expected_count and cleanup else 1


def session():
    """Reap the private bus and activated services, including independent groups."""
    root, fixture = map(Path, sys.argv[2:4]); private(fixture, True)
    assert read(fixture / '.marker', 128) == 'subyard-veranda-release-gui-v1\n'
    assert ctypes.CDLL(None).prctl(36, 1, 0, 0, 0) == 0
    spec = importlib.util.spec_from_file_location('measure', root / 'dev/measure-veranda.py')
    measure = importlib.util.module_from_spec(spec); spec.loader.exec_module(measure)
    process = subprocess.Popen(['dbus-run-session', '--', sys.executable, __file__, *sys.argv[2:]], start_new_session=True)
    growth = sys.argv[7:] == ['--growth']
    tree = measure.Tree(process.pid); end = time.monotonic() + (1825 if growth else 325)
    def interrupted(*_): raise TimeoutError()
    signal.signal(signal.SIGTERM, interrupted); signal.signal(signal.SIGINT, interrupted)
    try:
        while process.poll() is None and time.monotonic() < end:
            tree.sample(); time.sleep(0.05)
        rc = process.poll() if process.poll() is not None else 124
    except TimeoutError: rc = 124
    finally:
        tree.sample()
        for pid, info in measure.processes().items():
            if info['parent'] == os.getpid(): tree.known[pid] = info['start']
        for sig in [signal.SIGTERM, signal.SIGKILL]:
            live = measure.processes()
            for pid, info in live.items():
                if info['parent'] == os.getpid(): tree.known[pid] = info['start']
            for pid, start in tree.known.items():
                if live.get(pid, {}).get('start') == start:
                    try: os.kill(pid, sig)
                    except ProcessLookupError: pass
            until = time.monotonic() + 3
            while time.monotonic() < until:
                try:
                    while os.waitpid(-1, os.WNOHANG)[0]: pass
                except ChildProcessError: break
                time.sleep(0.05)
        live = measure.processes()
        remaining = any(live.get(pid, {}).get('start') == start for pid, start in tree.known.items())
        if remaining: rc = 3
        summary = fixture / 'summary.json'
        result = json.loads(read(summary, 4096)) if summary.exists() else {
            'passed': [], 'failedCase': 'preparation', 'cleanup': not remaining,
            'failureStage': 'session', 'failureCategory': 'timeout' if rc == 124 else 'process', 'selectionState': None, 'selectionFailure': None}
        if growth and 'growth' not in result: result['growth'] = growth_result([], 0, 0)
        if remaining: result['cleanup'] = False
        if rc != 0 and result['failureCategory'] is None:
            result['failureStage'] = 'cleanup' if remaining else 'session'
            result['failureCategory'] = 'cleanup' if remaining else 'timeout' if rc == 124 else 'process'
        validate_summary(result, rc, growth)
        write(summary, json.dumps(result))
    return rc


if __name__ == '__main__':
    sys.exit(growth_self_test() if sys.argv[1:] == ['--growth-self-test']
             else summary_self_test() if sys.argv[1:] == ['--summary-self-test']
             else session() if sys.argv[1:2] == ['--session'] else main())
