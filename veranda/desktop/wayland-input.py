#!/usr/bin/env python3
"""Read GTK accessibility, send actual Wayland/XTEST keys, then close naturally."""
import hashlib
import contextlib
import importlib.util
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import sys
import time


spec = importlib.util.spec_from_file_location('ssh_fixture', Path(__file__).with_name('ssh-fixture.py'))
ssh_fixture = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ssh_fixture)


class IncompleteTree(Exception):
    pass


def process_identity(pid):
    process = Path('/proc') / str(pid)
    fields = (process / 'stat').read_text().rsplit(')', 1)[1].split()
    assert fields[0] not in ('Z', 'X'), 'owned_process_terminated'
    exe = (process / 'exe').stat()
    return process.stat().st_uid, int(fields[19]), exe.st_dev, exe.st_ino


def input_progress(result, stage, phase, site, key, snapshot, tree, state):
    """Bounded cached diagnostics only; never read accessibility or typed text."""
    label = lambda value, size: value if isinstance(value, str) and re.fullmatch(r'[a-z0-9:-]{1,' + str(size) + '}', value) else 'other'
    number = lambda value: value if type(value) is int and 0 <= value <= 1_000_000_000 else None
    navigation = result.get('selection_trace', [{}])[-1] if result.get('selection_trace') else {}
    focus = result.get('scope_focus_trace', [{}])[-1] if result.get('scope_focus_trace') else {}
    reads = result.get('navigation_read', {})
    data = {'stage': label(stage, 80), 'phase': phase if phase in ('read-start', 'read-end', 'key-start', 'key-end') else 'other',
            'site': label(site.split(':', 1)[0], 40), 'key': key if key in ('Tab', 'F6', 'other', 'none') else 'other',
            'key_commands': number(result.get('physical_key_commands')), 'cached_selected': number(navigation.get('selected')),
            'cached_navigation_focus': number(navigation.get('focused')), 'cached_control_focus': number(focus.get('focused')),
            'cached_heading': navigation.get('heading') is True, 'cached_control_matched': focus.get('matched') is True,
            'cached_tree_nodes': min(len(tree), 2048), 'cached_focused_roles': [number(node.get('role')) for node in tree
                if 'FOCUSED' in node.get('states', ())][:4], 'snapshot_truncated': snapshot.get('truncated') is True,
            'incomplete_reads': number(reads.get('incomplete_tree_retries', 0)), 'read_errors': number(reads.get('read_error_count', 0))}
    signature = json.dumps(data, sort_keys=True)
    previous = state.setdefault('last', {})
    if previous.get(data['phase']) == signature:
        return
    previous[data['phase']] = signature
    state['count'] = state.get('count', 0) + 1
    now = time.monotonic()
    data.update(sequence=state['count'], truncated=state['count'] >= 512,
                elapsed_seconds=round(max(0, now - state.setdefault('started', now)), 3))
    encoded = json.dumps(data, sort_keys=True)
    assert len(encoded.encode()) <= 1024
    try:
        if state.get('checkpoint') is not None:
            ssh_fixture.replace_private_json(state['checkpoint'], data)
    except BaseException as error:
        result['checkpoint_failure_type'] = type(error).__name__
    if state['count'] > 512:
        return
    try:
        print('VERANDA_GTK_PROGRESS ' + encoded, flush=True)
    except BaseException as error:
        result['progress_failure_type'] = type(error).__name__


def read_input_checkpoint(path):
    # Validate an exact, scalar-only schema before including child evidence.
    path = Path(path)
    root = ssh_fixture.private_root(path.parent)
    try:
        fd = os.open(path.name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=root)
        with os.fdopen(fd) as source:
            info = os.fstat(source.fileno())
            assert stat.S_ISREG(info.st_mode) and info.st_uid == os.getuid() and info.st_nlink == 1
            assert stat.S_IMODE(info.st_mode) == 0o600 and info.st_size <= 1024
            value = json.loads(source.read(1025))
    finally:
        os.close(root)
    integers = {'key_commands', 'cached_selected', 'cached_navigation_focus', 'cached_control_focus',
                'cached_tree_nodes', 'incomplete_reads', 'read_errors'}
    booleans = {'cached_heading', 'cached_control_matched', 'snapshot_truncated', 'truncated'}
    assert type(value) is dict and set(value) == integers | booleans | {
        'stage', 'phase', 'site', 'key', 'cached_focused_roles', 'sequence', 'elapsed_seconds'}
    for key in integers:
        assert value[key] is None or type(value[key]) is int and 0 <= value[key] <= 1_000_000_000
    assert all(type(value[key]) is bool for key in booleans)
    assert type(value['sequence']) is int and 1 <= value['sequence'] <= 1_000_000_000
    assert type(value['elapsed_seconds']) in (float, int) and 0 <= value['elapsed_seconds'] <= 1_000_000_000
    assert isinstance(value['stage'], str) and re.fullmatch(r'[a-z0-9:-]{1,80}', value['stage'])
    assert isinstance(value['site'], str) and re.fullmatch(r'[a-z0-9-]{1,40}', value['site'])
    assert value['phase'] in ('read-start', 'read-end', 'key-start', 'key-end', 'other')
    assert value['key'] in ('Tab', 'F6', 'other', 'none')
    assert type(value['cached_focused_roles']) is list and len(value['cached_focused_roles']) <= 4
    assert all(role is None or type(role) is int and 0 <= role <= 1_000_000_000 for role in value['cached_focused_roles'])
    return value


def profile_plan_discard(tab, wait, names, focus, keys, activate, scope, result):
    """The shared exact physical profile flow; diagnostic callers claim no acceptance."""
    tab('Profiles', scope)
    wait(lambda: any('Effective profile selection:' in name for name in names()))
    focus('Review profile selection'); keys('-k', 'space')
    wait(lambda: any('Confirmation:' in name and 'Expires:' in name for name in names()))
    activate('Discard plan')
    wait(lambda: any('Plan discarded.' in name for name in names()))
    result['typed_profile_exact_plan_discard'] = True


def owned_tree(app, pid, defunct, deadline):
    queue, items = [(app, 0)], []
    while queue:
        if time.monotonic() >= deadline:
            raise TimeoutError('accessible_tree_unavailable')
        node, depth = queue.pop()
        if node is None:
            raise IncompleteTree()
        assert node.get_process_id() == pid, 'foreign_accessible_node'
        assert depth <= 32, 'accessible_depth_exceeded'
        assert len(items) < 2048, 'accessible_node_limit_exceeded'
        if node.get_state_set().contains(defunct):
            raise IncompleteTree()
        count = node.get_child_count()
        if count < 0:
            raise IncompleteTree()
        assert count <= 512, 'accessible_child_limit_exceeded'
        items.append(node)
        for index in range(count):
            if time.monotonic() >= deadline:
                raise TimeoutError('accessible_tree_unavailable')
            child = node.get_child_at_index(index)
            if child is None:
                raise IncompleteTree()
            queue.append((child, depth + 1))
    return items


def complete_tree(read, deadline, diagnostics):
    while time.monotonic() < deadline:
        try:
            items = read(deadline)
            if time.monotonic() >= deadline:
                break
            return items
        except IncompleteTree:
            diagnostics['incomplete_tree_retries'] = diagnostics.get('incomplete_tree_retries', 0) + 1
            time.sleep(min(.05, max(0, deadline - time.monotonic())))
    raise TimeoutError('accessible_tree_unavailable')


def coherent_observation(read, observe, deadline, diagnostics, api_error, site):
    def attempt(end):
        try:
            return observe(read(end))
        except api_error as error:
            point = {'function': 'coherent_observation', 'line': 0}
            diagnostic = False
            traceback = error.__traceback__
            while traceback is not None:
                if traceback.tb_frame.f_code.co_filename == __file__:
                    point = {'function': traceback.tb_frame.f_code.co_name, 'line': traceback.tb_lineno}
                    diagnostic = diagnostic or point['function'] in ('owner_focus_observation', 'describe')
                traceback = traceback.tb_next
            # Never retain error messages: libatspi can include private UI text.
            detail = dict(point, site=site, error_type=type(error).__name__,
                          domain='atspi_error' if error.domain == 'atspi_error' else 'other',
                          code=error.code if isinstance(error.code, int) else None,
                          category='diagnostic' if diagnostic else 'predicate')
            diagnostics['read_error_count'] = diagnostics.get('read_error_count', 0) + 1
            diagnostics.setdefault('first_read_error', detail)
            diagnostics['last_read_error'] = detail
            if diagnostic:
                diagnostics['diagnostic_read_error_count'] = diagnostics.get('diagnostic_read_error_count', 0) + 1
                diagnostics['last_diagnostic_read_error'] = detail
            # Same narrow libatspi read policy as veranda-release-gui.py. Discard
            # the entire observation; this callback can never send physical input.
            if error.domain == 'atspi_error' and error.code in (0, 1):
                raise IncompleteTree() from error
            raise
    return complete_tree(attempt, deadline, diagnostics)


def default_scope(items, atspi):
    assert any(node.get_role() == atspi.Role.LABEL and node.get_name() == 'Local owner / default'
               and node.get_state_set().contains(atspi.StateType.SHOWING) for node in items), 'local_scope_changed'
    assert any(node.get_role() == atspi.Role.LIST_ITEM and node.get_name() == 'Local owner / default'
               and node.get_state_set().contains(atspi.StateType.SELECTED) for node in items), 'local_scope_changed'


def default_input(items, atspi, expected_focus, deadline, send):
    default_scope(items, atspi)
    if expected_focus is not None:
        focused = {(node.get_role(), node.get_name()) for node in items
                   if node.get_state_set().contains(atspi.StateType.FOCUSED)}
        assert focused == {expected_focus}, 'focus_target_unavailable'
    if time.monotonic() >= deadline:
        raise TimeoutError('accessible_tree_unavailable')
    send()


def x11_key(pid, key):
    if not re.fullmatch(r'(?:(?:alt|ctrl|shift)\+)*(?:[a-z]|F4|F6|Tab|ISO_Left_Tab|space|Return|Down|Up|Home|Left)', key):
        raise ValueError('unsupported X11 key')
    x11_input(pid, ['key', '--clearmodifiers', key])


def x11_input(pid, arguments):
    windows = subprocess.check_output(['xdotool', 'search', '--onlyvisible', '--pid', str(pid)], timeout=3).splitlines()
    assert len(windows) == 1 and windows[0].isdigit(), 'owned_x11_window_unavailable'
    window = windows[0].decode()
    assert int(subprocess.check_output(['xdotool', 'getwindowpid', window], timeout=3)) == pid, 'owned_x11_window_changed'
    subprocess.run(['xdotool', 'windowactivate', '--sync', window], check=True, timeout=3)
    assert subprocess.check_output(['xdotool', 'getactivewindow'], timeout=3).strip() == windows[0], 'owned_x11_window_changed'
    assert int(subprocess.check_output(['xdotool', 'getwindowpid', window], timeout=3)) == pid, 'owned_x11_window_changed'
    # Omitting --window asks XTEST to deliver physical input to the focused window.
    subprocess.run(['xdotool', *arguments], check=True, timeout=5)


def x11_keys(pid, arguments, owned):
    # Translate only the wtype grammar used by these scenarios. Validate the
    # whole command before input, and recheck PID/start and window per event.
    modifiers, events, index = [], [], 0
    while index < len(arguments):
        value = arguments[index]
        if value in ('-M', '-m', '-k'):
            index += 1
            if index >= len(arguments):
                raise ValueError('incomplete physical input')
            name = arguments[index]
            if value == '-M':
                if name not in ('alt', 'ctrl', 'shift') or name in modifiers:
                    raise ValueError('unsupported physical modifier')
                modifiers.append(name)
            elif value == '-m':
                modifiers.remove(name)
            else:
                key = '+'.join([*modifiers, name])
                if not re.fullmatch(r'(?:(?:alt|ctrl|shift)\+)*(?:[a-z]|F4|F6|Tab|ISO_Left_Tab|space|Return|Down|Up|Home|Left)', key):
                    raise ValueError('unsupported X11 key')
                events.append(('key', key))
        else:
            if modifiers or value.startswith('-') or not value or len(value) > 512 or any(ord(char) < 32 for char in value):
                raise ValueError('unsupported physical text')
            events.append(('text', value))
        index += 1
    if modifiers:
        raise ValueError('unreleased physical modifier')
    for kind, value in events:
        if kind == 'key':
            owned()
            x11_key(pid, value)
        else:
            # xdotool temporarily remaps Unicode keycodes. A busy GTK consumer
            # can read queued events after those mappings have been restored.
            # GTK's native Unicode entry uses stable physical ASCII keys.
            for chunk in re.findall(r'[\x20-\x7e]+|[^\x20-\x7e]', value):
                owned()
                if len(chunk) == 1 and not 32 <= ord(chunk) < 127:
                    x11_input(pid, ['key', '--clearmodifiers', '--delay', '25',
                                   'ctrl+shift+u', *format(ord(chunk), 'x'), 'Return'])
                else:
                    x11_input(pid, ['type', '--clearmodifiers', '--delay', '25', '--', chunk])


def retain_failure_diagnostics(result, collect):
    try:
        result['failure_diagnostics'] = collect()
    except BaseException as error:
        result['diagnostic_failure_type'] = type(error).__name__


def capture_view(backend, capture, env=None, deadline=None):
    env = os.environ if env is None else env
    def timeout():
        remaining = 5 if deadline is None else min(5, deadline - time.monotonic())
        assert remaining > 0, 'diagnostic_deadline_expired'
        return remaining
    if deadline is not None: timeout()
    assert capture.parent.resolve() == capture.parent and not capture.exists()
    fd = os.open(capture, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    os.fchmod(fd, 0o600)
    original = os.fstat(fd)
    try:
        with os.fdopen(fd, 'wb') as output:
            if backend == 'wayland':
                subprocess.run(['grim', '-'], check=True, env=env,
                               stdout=output, stderr=subprocess.DEVNULL, timeout=timeout())
        if backend == 'x11':
            code = ('import importlib.util,pathlib,sys\n'
                    'spec=importlib.util.spec_from_file_location("probe",sys.argv[1])\n'
                    'probe=importlib.util.module_from_spec(spec);spec.loader.exec_module(probe)\n'
                    'assert probe.screenshot(sys.argv[2],pathlib.Path(sys.argv[3]))\n')
            subprocess.run([sys.executable, '-c', code,
                str(Path(__file__).resolve().parents[2] / 'dev/measure-veranda.py'),
                env['DISPLAY'], str(capture)], env=env, check=True, timeout=timeout(),
                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        info = capture.lstat()
        assert (os.path.samestat(original, info) and stat.S_ISREG(info.st_mode) and info.st_uid == os.getuid()
                and stat.S_IMODE(info.st_mode) == 0o600 and info.st_nlink == 1 and info.st_size <= 2 * 1024 * 1024)
        if deadline is not None: assert time.monotonic() < deadline, 'diagnostic_deadline_expired'
    except BaseException:
        # A partial diagnostic image must not break the parent's evidence retention.
        if capture.exists() and os.path.samestat(original, capture.lstat()):
            capture.unlink()
        raise


def focus_matches(items, pid, name, atspi, owner_row=False):
    def visible_owned(node):
        states = node.get_state_set()
        return (node.get_process_id() == pid and states.contains(atspi.StateType.SHOWING)
                and not states.contains(atspi.StateType.DEFUNCT))
    focused = [node for node in items if node.get_state_set().contains(atspi.StateType.FOCUSED)]
    if len(focused) != 1 or not visible_owned(focused[0]):
        return False
    if not owner_row:
        return focused[0].get_name() == name
    matches = []
    for row in items:
        if (not visible_owned(row) or row.get_role() != atspi.Role.LIST_ITEM
                or row.get_name() or row.get_child_count() != 1):
            continue
        child = row.get_child_at_index(0)
        # AT-SPI may return another GObject wrapper for the same accessible.
        # Compare accessible equality, never Python wrapper identity or flat order.
        if (child is not None and child in items and visible_owned(child)
                and child.get_role() == atspi.Role.LABEL and child.get_name() == name
                and child.get_parent() == row):
            matches.append(row)
    return len(matches) == 1 and focused[0] == matches[0]


def navigation_snapshot(items, pid, atspi):
    def valid(node, role):
        if node is None:
            raise IncompleteTree()
        states = node.get_state_set()
        assert (node.get_process_id() == pid and node.get_role() == role
                and states.contains(atspi.StateType.SHOWING)
                and not states.contains(atspi.StateType.DEFUNCT)), 'focus_target_unavailable'
        if node not in items:
            raise IncompleteTree()
        return states
    lists = [node for node in items if node.get_role() == atspi.Role.LIST
             and node.get_name() == 'Owner hosts and yards']
    assert len(lists) == 1, 'focus_target_unavailable'
    navigation = lists[0]
    valid(navigation, atspi.Role.LIST)
    assert 0 < navigation.get_child_count() <= 64, 'focus_target_unavailable'
    rows, order, owners, selected = [], [], [], []
    for index in range(navigation.get_child_count()):
        row = navigation.get_child_at_index(index)
        states = valid(row, atspi.Role.LIST_ITEM)
        if states.contains(atspi.StateType.SELECTED):
            selected.append(index)
        assert row.get_parent() == navigation and row.get_child_count() == 1, 'focus_target_unavailable'
        child = row.get_child_at_index(0)
        valid(child, atspi.Role.LABEL)
        assert child.get_parent() == row, 'focus_target_unavailable'
        rows.append(row)
        owners.append(not row.get_name())
        order.append(row.get_name() or child.get_name())
    assert all(order) and len(set(order)) == len(order), 'focus_target_unavailable'
    focused = [node for node in items if node.get_state_set().contains(atspi.StateType.FOCUSED)]
    # Closing the focused SSH panel can leave scope visible without a focus target.
    assert len(focused) <= 1, 'focus_target_unavailable'
    if focused:
        valid(focused[0], focused[0].get_role())
    return {'order': order, 'owners': owners,
            'selected_indices': selected,
            'focused': rows.index(focused[0]) if focused and focused[0] in rows else None}


def focused_button(items, pid, name, atspi):
    focused = [node for node in items if node.get_state_set().contains(atspi.StateType.FOCUSED)]
    return (focus_matches(items, pid, name, atspi) and len(focused) == 1
            and focused[0].get_role() == atspi.Role.PUSH_BUTTON)


def focused_editable(items, pid, name, atspi):
    matches = [node for node in items if node.get_process_id() == pid
               and node.get_role() == atspi.Role.TEXT and node.get_name() == name
               and node.get_state_set().contains(atspi.StateType.SHOWING)
               and not node.get_state_set().contains(atspi.StateType.DEFUNCT)]
    return (matches[0] if len(matches) == 1 and focus_matches(items, pid, name, atspi)
            and matches[0].get_state_set().contains(atspi.StateType.FOCUSED)
            and matches[0].get_state_set().contains(atspi.StateType.EDITABLE) else None)


def remote_editor_ready(items, pid, atspi):
    editor = focused_editable(items, pid, 'Remote SSH destination', atspi)
    if editor is None: return False
    parent = editor.get_parent()
    if (parent not in items or parent.get_process_id() != pid or parent.get_role() != atspi.Role.PANEL
            or not parent.get_state_set().contains(atspi.StateType.SHOWING)
            or parent.get_state_set().contains(atspi.StateType.DEFUNCT)):
        return False
    titles = [node for node in items if node.get_process_id() == pid and node.get_role() == atspi.Role.LABEL
              and node.get_name() == 'Connect remote host' and node.get_parent() == parent
              and node.get_state_set().contains(atspi.StateType.SHOWING)
              and not node.get_state_set().contains(atspi.StateType.DEFUNCT)]
    return len(titles) == 1


def record_ime_read(observation, count, text, value, proof):
    """Bounded original read data for the fixture-owned empty Yard name field."""
    assert set(proof) == {'scope', 'owned', 'unique_editor', 'focused', 'same_editor'} and all(value is True for value in proof.values())
    raw_type = type(text).__name__ if type(text) in (str, type(None), bytes, int, bool, float, list, tuple, dict) else 'other'
    row = {'character_count': count, 'raw_type': raw_type,
           'raw_length': len(text) if isinstance(text, str) else None,
           'normalized_kind': 'none' if value is None else 'empty' if value == '' else 'nonempty',
           'codepoints': [ord(char) for char in text[:16]] if isinstance(text, str) else [],
           'truncated': isinstance(text, str) and len(text) > 16, 'proof': dict(proof)}
    if len(observation['reads']) == 16:
        observation['reads'].pop(0)
        observation['dropped'] += 1
    observation['reads'].append(row)


def interface_text(interface, text_api, observation=None, proof=None):
    # Accessible.get_text() shadows the Text interface's content method in GI.
    count = text_api.get_character_count(interface)
    assert type(count) is int and 0 <= count <= 32768, 'accessible_text_limit_exceeded'
    text = text_api.get_text(interface, 0, count)
    value = text if isinstance(text, str) and len(text) == count else None
    if observation is not None:
        try:
            record_ime_read(observation, count, text, value, proof)
        except BaseException as error:
            with contextlib.suppress(BaseException): observation['failure_type'] = type(error).__name__
    return value


def committed_host_editor(items, pid, scope, expected, atspi, observation):
    assert selected_scope(navigation_context(items, pid, atspi), scope), 'local_scope_changed'
    node = focused_editable(items, pid, 'Yard name', atspi)
    assert node is not None, 'focus_target_unavailable'
    value = interface_text(node.get_text_iface(), atspi.Text)
    observation.update(committed_complete=value is not None,
                       committed_length=len(value) if value is not None else None,
                       committed_equal=value == expected)
    return value == expected


def persisted_host_draft(root, connection_id, expected, observation):
    path = root / 'desktop.json'
    present = path.exists()
    observation.update(file_present=present, draft_read_complete=False, matching_scope=False,
                       matching_draft_count=0, draft_value_equal=False)
    if not present: return False
    drafts = json.loads(path.read_bytes())['drafts']
    scoped = [draft for draft in drafts if draft['scope'] == {'connection': connection_id, 'yard': None}]
    matches = [draft for draft in scoped if draft['field'] == 'create:name']
    equal = len(matches) == 1 and matches[0]['value'] == expected
    observation.update(draft_read_complete=True, matching_scope=bool(scoped),
                       matching_draft_count=min(len(matches), 2), draft_value_equal=equal)
    return equal


def overview_observed_label(items, pid, expected_scope, atspi):
    assert selected_scope(navigation_context(items, pid, atspi), expected_scope), 'local_scope_changed'
    matches = [node for node in items if node.get_process_id() == pid and node.get_role() == atspi.Role.LABEL
               and node.get_state_set().contains(atspi.StateType.SHOWING)
               and not node.get_state_set().contains(atspi.StateType.DEFUNCT)
               and re.search(r'(?:^|\n)Observed: [^\n]+\Z', node.get_name())]
    return matches[0] if len(matches) == 1 else None


def retained_observed(node, pid, atspi, owned):
    owned()
    assert (node.get_process_id() == pid and node.get_role() == atspi.Role.LABEL
            and not node.get_state_set().contains(atspi.StateType.DEFUNCT)), 'focus_target_unavailable'
    # GTK hides a non-current page's Name; the retained selectable Label's Text stays current.
    text = interface_text(node.get_text_iface(), atspi.Text)
    match = re.search(r'(?:^|\n)Observed: ([0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]{1,9})?Z)\Z', text) if text is not None else None
    return match[1] if match else None


def selection_ranges(interface, text_api, length):
    count = text_api.get_n_selections(interface)
    assert type(count) is int and 0 <= count <= 1, 'refresh_reset_selection'
    ranges = []
    for index in range(count):
        selected = text_api.get_selection(interface, index)
        start, end = selected.start_offset, selected.end_offset
        assert type(start) is int and type(end) is int and 0 <= start <= end <= length, 'refresh_reset_selection'
        ranges.append((start, end))
    return ranges


def selection_baseline(interface, text_api, length):
    caret = interface.get_caret_offset()
    ranges = selection_ranges(interface, text_api, length)
    assert (type(caret) is int and 0 <= caret <= length and len(ranges) == 1
            and ranges[0][0] < ranges[0][1] and caret in ranges[0]), 'refresh_reset_selection'
    return caret, ranges


def editor_continuity(items, pid, atspi, original, text, caret, selections):
    assert focused_button(items, pid, 'Refresh', atspi), 'focus_target_unavailable'
    editors = [node for node in items if node.get_process_id() == pid and node.get_role() == atspi.Role.TEXT
               and node.get_name() == 'Yard name' and node.get_state_set().contains(atspi.StateType.SHOWING)
               and node.get_state_set().contains(atspi.StateType.EDITABLE)
               and not node.get_state_set().contains(atspi.StateType.DEFUNCT)]
    assert len(editors) == 1, 'focus_target_unavailable'
    node = editors[0]
    assert node == original, 'refresh_replaced_editor'
    assert interface_text(node.get_text_iface(), atspi.Text) == text, 'unicode_gtk_text_mismatch'
    assert node.get_text_iface().get_caret_offset() == caret, 'refresh_reset_caret'
    assert selection_ranges(node.get_text_iface(), atspi.Text, len(text)) == selections, 'refresh_reset_selection'
    return True


def focus_control(read, send, name, trace=None):
    baseline = None
    left_navigation = False
    for count in range(65):
        snapshot = read()
        context = (tuple(snapshot['order']), tuple(snapshot['owners']), snapshot['scope'])
        assert snapshot['heading'], 'local_scope_changed'
        if baseline is None: baseline = context
        assert context == baseline, 'local_scope_changed'
        if trace is not None:
            trace.append({**{key: snapshot[key] for key in ('focused', 'scope', 'heading', 'matched')},
                          'target': name, 'step': count})
        if left_navigation:
            assert snapshot['focused'] is None, 'focus_target_unavailable'
        if snapshot['matched']: return
        assert count < 64, 'focus_target_unavailable'
        key = 'F6' if snapshot['focused'] is not None else 'Tab'
        left_navigation |= key == 'F6'
        if trace is not None: trace[-1]['physical_key'] = key
        send(key)


def focus_target(read_named, read_workspace, send, name, workspace=False, trace=None):
    if workspace:
        return focus_control(read_workspace, send, name, trace)
    # Navigator/SSH-panel controls remain available while the workspace is hidden.
    # These observations retain the original owned visible named-focus policy.
    for count in range(65):
        if read_named(): return
        assert count < 64, 'focus_target_unavailable'
        send('Tab')


def navigation_context(items, pid, atspi):
    snapshot = navigation_snapshot(items, pid, atspi)
    selected = snapshot['selected_indices']
    assert len(selected) == 1, 'local_scope_changed'
    scope = snapshot['order'][selected[0]]
    headings = [node for node in items if node.get_process_id() == pid
                and node.get_role() == atspi.Role.LABEL and node.get_name() == scope
                and node.get_state_set().contains(atspi.StateType.SHOWING)
                and not node.get_state_set().contains(atspi.StateType.DEFUNCT)
                and node.get_parent() in items and node.get_parent().get_process_id() == pid
                and node.get_parent().get_role() != atspi.Role.LIST_ITEM]
    return dict(snapshot, selected=selected[0], scope=scope, heading=len(headings) == 1)


def selected_scope(snapshot, expected):
    return snapshot['scope'] == expected and snapshot['heading']


def workspace_tab_ready(items, pid, name, expected_scope, atspi):
    assert selected_scope(navigation_context(items, pid, atspi), expected_scope), 'local_scope_changed'
    focused = [node for node in items if node.get_state_set().contains(atspi.StateType.FOCUSED)]
    assert (focus_matches(items, pid, name, atspi) and len(focused) == 1
            and focused[0].get_role() == atspi.Role.PAGE_TAB), 'focus_target_unavailable'
    return True


def open_review(acquire, read_entry, send):
    acquire()
    entry = read_entry()
    assert entry['heading'] and entry['button_focused'], 'focus_target_unavailable'
    send()
    return entry


def entry_scope_preserved(current, entry):
    return selected_scope(current, entry['scope'])


def navigate_owner(read, send, target, trace=None):
    order, owners, expected = None, None, None
    for count in range(65):
        snapshot = read()
        if trace is not None:
            trace.append(dict(snapshot, target=target, expected=expected, step=count))
        if order is None:
            order, owners = snapshot['order'], snapshot['owners']
            assert order.count(target) == 1 and owners[order.index(target)], 'focus_target_unavailable'
        current = snapshot['focused']
        assert expected is None or current == expected, 'focus_target_unavailable'
        target_index = order.index(target)
        if current == target_index:
            assert (snapshot['order'].count(target) == 1
                    and snapshot['order'][:target_index + 1] == order[:target_index + 1]
                    and snapshot['owners'][:target_index + 1] == owners[:target_index + 1]), 'focus_target_unavailable'
            return
        assert snapshot['order'] == order and snapshot['owners'] == owners, 'focus_target_unavailable'
        assert count < 64, 'focus_target_unavailable'
        if current is None:
            key = 'Tab'
        elif expected is None:
            key, expected = 'Home', 0
        else:
            key, expected = 'Down', current + 1
        if trace is not None:
            trace[-1]['physical_key'] = key
        send(key)


def diagnostic_navigate_owner(read, send, target, trace, evidence, next_deadline, now=time.monotonic, validate=None, capture=None):
    """One post-rejection observation, never another key or a replacement verdict."""
    deadline, current = None, None
    def observe():
        nonlocal deadline, current
        deadline, current = next_deadline(), None
        current = read(deadline)
        return current
    def scalars(view):
        return None if view is None else {key: view.get(key) for key in ('focused', 'selected_indices', 'focus_probe')}
    try:
        navigate_owner(observe, send, target, trace)
        if validate is not None:
            deadline, current = next_deadline(), None
            validate(deadline)
    except AssertionError as error:
        if str(error) == 'focus_target_unavailable':
            evidence.update(original_failure='focus_target_unavailable', original=scalars(current),
                            original_provenance='NOT_COLLECTED', observation_deadline=deadline, convergence_attempted=False)
            if deadline is not None and now() < deadline:
                evidence['convergence_attempted'] = True
                evidence['fresh_provenance'] = 'fresh-post-rejection'
                try:
                    fresh = (capture or read)(deadline)
                    evidence['fresh'] = scalars(fresh)
                    evidence['fresh_within_original_deadline'] = now() < deadline
                except Exception as secondary:
                    evidence['fresh_failure_type'] = type(secondary).__name__
        raise


def opaque_focus_relations(items, pid, atspi, deadline):
    """Fresh actual parent/child links only; no UI text or inferred ancestry."""
    started = time.monotonic()
    def valid(node):
        assert time.monotonic() < deadline, 'focus_diagnostic_deadline'
        matches = [index for index, item in enumerate(items) if node == item]
        assert len(matches) == 1, 'focus_diagnostic_relationship'
        states = node.get_state_set()
        assert (node.get_process_id() == pid and states.contains(atspi.StateType.SHOWING)
                and not states.contains(atspi.StateType.DEFUNCT)), 'focus_diagnostic_relationship'
        return matches[0] + 1
    navigation = [node for node in items if node.get_role() == atspi.Role.LIST
                  and node.get_name() == 'Owner hosts and yards']
    windows = [node for node in items if node.get_role() == atspi.Role.FRAME
               and node.get_state_set().contains(atspi.StateType.SHOWING)]
    focused = [node for node in items if node.get_state_set().contains(atspi.StateType.FOCUSED)]
    assert len(navigation) == len(windows) == 1 and len(focused) <= 1, 'focus_diagnostic_relationship'
    window, nav = windows[0], navigation[0]
    window_id, nav_id = valid(window), valid(nav)
    def chain(node):
        links = []
        for _ in range(16):
            ordinal = valid(node)
            assert ordinal not in links, 'focus_diagnostic_relationship'
            links.append(ordinal)
            if node == window: return links
            parent = node.get_parent()
            if parent is None: raise IncompleteTree()
            valid(parent)
            count = parent.get_child_count()
            assert 0 < count <= 128, 'focus_diagnostic_relationship'
            matches = 0
            for index in range(count):
                assert time.monotonic() < deadline, 'focus_diagnostic_deadline'
                child = parent.get_child_at_index(index)
                if child is None: raise IncompleteTree()
                matches += child == node
            assert matches == 1, 'focus_diagnostic_relationship'
            node = parent
        raise AssertionError('focus_diagnostic_relationship')
    nav_chain = chain(nav)
    focus_chain = chain(focused[0]) if focused else []
    return {'status': 'OBSERVED', 'started_monotonic': started, 'ended_monotonic': time.monotonic(),
            'pid': pid, 'navigation': nav_id, 'window': window_id, 'navigation_ancestors': nav_chain,
            'focused': valid(focused[0]) if focused else None,
            'focused_role': int(focused[0].get_role()) if focused else None,
            'focused_ancestors': focus_chain, 'navigation_contains_focus': nav_id in focus_chain,
            'focus_contains_navigation': bool(focus_chain and focus_chain[0] in nav_chain)}


def owner_focus_observation(items, pid, name, atspi):
    """Whitelist actual accessible relations; never derive ancestry from order."""
    def describe(node):
        if node is None:
            return None
        value = {'pid': node.get_process_id(), 'wrapper_id': id(node),
                 'accessible_hash': hash(node), 'in_snapshot': node in items}
        if value['pid'] == pid:
            states = node.get_state_set()
            value.update(role=int(node.get_role()), name=node.get_name(),
                         states=[key for key in ('SHOWING', 'FOCUSED', 'DEFUNCT')
                                 if states.contains(getattr(atspi.StateType, key))])
        return value
    focused = [node for node in items if node.get_state_set().contains(atspi.StateType.FOCUSED)]
    rows, matches = [], []
    for row in items:
        if row.get_process_id() != pid or row.get_role() != atspi.Role.LIST_ITEM or row.get_name():
            continue
        entry = {'row': describe(row), 'child_count': row.get_child_count()}
        reason = 'row_not_visible_owned'
        if 'SHOWING' in entry['row']['states'] and 'DEFUNCT' not in entry['row']['states']:
            reason = 'child_count_not_one'
            if entry['child_count'] == 1:
                child = row.get_child_at_index(0)
                entry['child'] = describe(child)
                parent = child.get_parent() if child is not None and child.get_process_id() == pid else None
                entry['parent'] = describe(parent)
                entry['parent_equals_row'] = parent == row
                reason = 'child_not_in_owned_snapshot'
                if child is not None and child in items and child.get_process_id() == pid:
                    states = child.get_state_set()
                    reason = 'child_not_visible_label_target'
                    if (states.contains(atspi.StateType.SHOWING) and not states.contains(atspi.StateType.DEFUNCT)
                            and child.get_role() == atspi.Role.LABEL and child.get_name() == name):
                        reason = 'parent_not_row'
                        if parent == row:
                            reason = 'candidate'
                            matches.append(row)
        entry['reason'] = reason
        rows.append(entry)
    if len(focused) != 1:
        reason = 'focused_count_not_one'
    elif (focused[0].get_process_id() != pid
          or not focused[0].get_state_set().contains(atspi.StateType.SHOWING)
          or focused[0].get_state_set().contains(atspi.StateType.DEFUNCT)):
        reason = 'focused_not_visible_owned'
    elif len(matches) != 1:
        reason = 'candidate_count_not_one'
    elif focused[0] != matches[0]:
        reason = 'focused_not_owner_candidate'
    else:
        reason = 'matched'
    return {'focused': [describe(node) for node in focused], 'owner_rows': rows,
            'candidate_count': len(matches), 'reason': reason,
            'predicate_result': focus_matches(items, pid, name, atspi, owner_row=True)}


def review_ready(names, destination, fingerprint, known_key=False):
    button = 'Connect' if known_key else 'Accept and connect'
    if button not in names:
        return False
    for name in names:
        lines = name.splitlines()
        if not lines or lines[0] != destination or 'Fingerprint: ' + fingerprint not in lines:
            continue
        if known_key:
            if ('Previous fingerprint: ' + fingerprint in lines
                    and 'Connect using the previously accepted SSH host key.' in lines):
                return True
        elif ('Verify the fingerprint independently before accepting.' in lines
              and not any(line.startswith('Previous fingerprint:') for line in lines)):
            return True
    return False


def cancellation_flow_closed(items, atspi):
    return not any(node.get_role() == atspi.Role.TEXT and node.get_name() == 'Remote SSH destination'
                   and node.get_state_set().contains(atspi.StateType.SHOWING) for node in items)



def local_ime_draft(root, committed='\uac00'):
    path = root / 'desktop.json'
    if not path.exists(): return committed == ''
    info = path.lstat()
    assert (stat.S_ISREG(info.st_mode) and info.st_uid == os.getuid() and info.st_nlink == 1
            and stat.S_IMODE(info.st_mode) == 0o600 and info.st_size <= 128 * 1024), 'ime_draft_unavailable'
    drafts = [draft for draft in json.loads(path.read_bytes())['drafts']
              if draft['scope'] == {'connection': None, 'yard': None} and draft['field'] == 'create:name']
    return (not drafts and committed == '') or len(drafts) == 1 and drafts[0]['value'] == committed


def compose_ime(keys, read, wait, capture, persisted):
    """Only physical ASCII keys enter GTK; preedit pixels need separate inspection."""
    # GTK4.18.6 GtkEntry's AT-SPI GetText reads committed EntryBuffer, not preedit layout.
    assert read() == '', 'ime_initial_editor_not_empty'
    keys('-M', 'shift', '-k', 'space', '-m', 'shift')
    keys('-k', 'r')
    assert read() == '', 'ime_preedit_committed_early'
    capture('consonant', '')
    keys('-k', 'k')
    assert read() == '', 'ime_preedit_committed_early'
    capture('syllable', '')
    keys('-M', 'shift', '-k', 'space', '-m', 'shift')
    wait(lambda: read() == '\uac00')
    wait(persisted)
    capture('committed', '\uac00')


def main():
    import gi
    gi.require_version("Atspi", "2.0")
    from gi.repository import Atspi, Gio, GLib
    pid = int(os.environ["VERANDA_GTK_PID"])
    backend = os.environ.get('VERANDA_GTK_BACKEND', 'wayland')
    assert backend in ('x11', 'wayland')
    started = time.monotonic()
    stage = "accessibility"
    result = {"original_exit_code": 1, "physical_key_commands": 0, 'physical_keys': [],
              "accessibility_actions_used": False, "backend": backend}
    def identity():
        return process_identity(pid)
    original_identity = identity()
    assert original_identity[0] == os.getuid()
    last_tree, last_accessibility = [], {}
    ime_observation = None
    snapshot_state = {'next': 0, 'truncated': False}
    progress_state = {'started': started, 'checkpoint': Path(os.environ['VERANDA_GTK_CHECKPOINT']) if os.environ.get('VERANDA_GTK_CHECKPOINT') else None}
    def progress(phase, site='navigation', key='none'):
        input_progress(result, stage, phase, site, key, snapshot_state, last_tree, progress_state)
    try:
        connection = Gio.DBusConnection.new_for_address_sync(os.environ["DBUS_SESSION_BUS_ADDRESS"],
            Gio.DBusConnectionFlags.AUTHENTICATION_CLIENT | Gio.DBusConnectionFlags.MESSAGE_BUS_CONNECTION, None, None)
        connection.call_sync("org.a11y.Bus", "/org/a11y/bus", "org.freedesktop.DBus.Properties", "Set",
            GLib.Variant("(ssv)", ("org.a11y.Status", "IsEnabled", GLib.Variant("b", True))),
            None, Gio.DBusCallFlags.NONE, 5000, None)
        Atspi.set_timeout(500, 1000)
        Atspi.init()
        context = GLib.MainContext.default()

        def owned():
            assert identity() == original_identity, 'owned_process_identity_changed'

        read_deadline = None

        def read_tree(deadline):
            owned()
            for _ in range(64):
                if not context.pending(): break
                context.iteration(False)
            desktop = Atspi.get_desktop(0)
            desktop.clear_cache()
            count = desktop.get_child_count()
            if count < 0:
                raise IncompleteTree()
            assert count <= 512, 'accessible_child_limit_exceeded'
            applications = []
            for index in range(count):
                if time.monotonic() >= deadline: raise TimeoutError('accessible_tree_unavailable')
                applications.append(desktop.get_child_at_index(index))
            if any(app is None for app in applications):
                raise IncompleteTree()
            apps = [app for app in applications if app and app.get_process_id() == pid]
            last_accessibility.update(desktop_applications=len(applications), matching_applications=len(apps))
            assert len(apps) <= 1, 'duplicate_owned_accessible_application'
            if not apps:
                raise IncompleteTree()
            app = apps[0]
            app.clear_cache()
            return owned_tree(app, pid, Atspi.StateType.DEFUNCT, deadline)

        def nodes(observe=None, site='navigation', deadline=None):
            progress('read-start', site)
            if observe is not None:
                value = coherent_observation(read_tree, observe, deadline if deadline is not None else read_deadline or time.monotonic() + 20,
                    result.setdefault('navigation_read', {}), GLib.Error, site)
                progress('read-end', site)
                return value
            items = complete_tree(read_tree, read_deadline or time.monotonic() + 20, snapshot_state)
            if time.monotonic() >= snapshot_state['next']:
                # Cache diagnostics during normal reads, at most once per second.
                # The failure path never makes another AT-SPI RPC.
                snapshot_state['next'] = time.monotonic() + 1
                snapshot_state.pop('error', None)
                deadline, snapshot = time.monotonic() + .25, []
                for node in items:
                    if time.monotonic() >= deadline:
                        break
                    try:
                        states = node.get_state_set()
                        snapshot.append({'pid': node.get_process_id(), 'role': int(node.get_role()),
                            'name': (node.get_name() or '').encode()[:128].decode(errors='ignore'),
                            'states': [name for name in ('SHOWING', 'FOCUSED', 'ENABLED', 'SELECTED')
                                       if states.contains(getattr(Atspi.StateType, name))]})
                    except BaseException as diagnostic_error:
                        snapshot_state['error'] = type(diagnostic_error).__name__
                        break
                snapshot_state['truncated'] = len(snapshot) != len(items)
                if not snapshot_state['truncated']:
                    last_tree[:] = snapshot
            progress('read-end', site)
            return items

        def wait(predicate, seconds=20, deadline=None):
            nonlocal read_deadline
            previous = read_deadline
            read_deadline = deadline if deadline is not None else time.monotonic() + seconds
            try:
                while time.monotonic() < read_deadline:
                    value = predicate()
                    if time.monotonic() >= read_deadline: break
                    if value: return value
                    time.sleep(.05)
                raise TimeoutError(stage)
            finally:
                read_deadline = previous

        def keys(*arguments):
            key = arguments[1] if len(arguments) == 2 and arguments[0] == '-k' and arguments[1] in ('Tab', 'F6') else 'other'
            progress('key-start', key=key)
            owned()
            if backend == 'x11':
                x11_keys(pid, arguments, owned)
            else:
                subprocess.run(["wtype", "-s", "100", *arguments, "-s", "100"], check=True,
                               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=5)
            result["physical_key_commands"] += 1
            progress('key-end', key=key)

        def local_key(key):
            if backend == 'x11':
                owned()
                x11_key(pid, key)
                result['physical_key_commands'] += 1
            else:
                keys(*{'alt+r': ('-M', 'alt', '-k', 'r', '-m', 'alt'),
                       'Tab': ('-k', 'Tab'), 'space': ('-k', 'space'),
                       'shift+Tab': ('-M', 'shift', '-k', 'ISO_Left_Tab', '-m', 'shift'),
                       'alt+F4': ('-M', 'alt', '-k', 'F4', '-m', 'alt')}[key])
            result['physical_keys'].append(key)

        def default_nodes(deadline):
            items = wait(nodes, deadline=deadline)
            default_scope(items, Atspi)
            return items

        def default_key(key, deadline, expected_focus=None):
            items = default_nodes(deadline)
            # Mnemonic/window shortcuts do not depend on the current control.
            assert expected_focus is not None or key in ('alt+r', 'alt+F4')
            default_input(items, Atspi, expected_focus, deadline, lambda: local_key(key))

        def observed():
            for node in nodes():
                match = re.search(r"Observed: ([^\n]+)", node.get_name())
                if match: return match[1]

        scenario = os.environ.get('VERANDA_GTK_SCENARIO', 'local')
        if scenario in ('owner-focus-connect', 'owner-focus-restore'):
            result['diagnostic_only'] = True
        destination = os.environ.get('VERANDA_GTK_DESTINATION', '')
        def names():
            return {node.get_name() for node in nodes() if node.get_state_set().contains(Atspi.StateType.SHOWING)}
        def connected_owner():
            visible = names()
            return destination in visible and 'Connected' in visible and any('20 yards' in name for name in visible)
        def focus(name, owner_row=False, workspace=False, expected_scope=None):
            if owner_row:
                diagnostic = scenario in ('owner-focus-connect', 'owner-focus-restore',
                                          'connect', 'remote-restore', 'local-restore')
                observation = [0]
                def read_navigation(deadline=None, auxiliary=False):
                    def observe(items):
                        snapshot = navigation_snapshot(items, pid, Atspi)
                        detail = owner_focus_observation(items, pid, name, Atspi) if scenario == 'owner-focus' else None
                        if auxiliary:
                            observation[0] += 1
                            try: snapshot['focus_probe'] = opaque_focus_relations(items, pid, Atspi, deadline)
                            except Exception as error:
                                snapshot['focus_probe'] = {'status': 'UNKNOWN', 'failure_type': type(error).__name__}
                            snapshot['focus_probe']['observation'] = observation[0]
                        return snapshot, detail
                    snapshot, detail = nodes(observe, deadline=deadline)
                    if detail is not None:
                        result.setdefault('focus_trace', []).append(dict(detail, target=name, owner_row=True))
                    return snapshot
                if diagnostic:
                    def validate_navigation(deadline):
                        assert nodes(lambda items: focus_matches(items, pid, name, Atspi, True),
                                     'final-focus', deadline=deadline), 'focus_target_unavailable'
                    diagnostic_navigate_owner(read_navigation, lambda key: keys('-k', key), name,
                        result.setdefault('owner_navigation', []), result.setdefault('focus_rejection', {}),
                        lambda: read_deadline if read_deadline is not None else time.monotonic() + 20,
                        validate=validate_navigation,
                        capture=lambda end: read_navigation(end, auxiliary=True))
                else:
                    navigate_owner(read_navigation, lambda key: keys('-k', key), name,
                                   result.setdefault('owner_navigation', []))
                    assert nodes(lambda items: focus_matches(items, pid, name, Atspi, True), 'final-focus'), 'focus_target_unavailable'
                return
            def read_control():
                def observe(items):
                    snapshot = navigation_context(items, pid, Atspi)
                    if expected_scope is not None:
                        assert selected_scope(snapshot, expected_scope), 'local_scope_changed'
                    snapshot['matched'] = focus_matches(items, pid, name, Atspi)
                    return snapshot
                return nodes(observe, 'named-focus:' + name)
            focus_target(lambda: nodes(lambda items: focus_matches(items, pid, name, Atspi), 'named-focus:' + name),
                         read_control, lambda key: keys('-k', key), name, workspace,
                         result.setdefault('scope_focus_trace', []))
        def activate(name, workspace=False):
            focus(name, workspace=workspace)
            assert nodes(lambda items: focused_button(items, pid, name, Atspi), 'activate-focus'), 'focus_target_unavailable'
            keys('-k', 'space')
        def review(known_key=False):
            def read_entry():
                def observe(items):
                    snapshot = navigation_context(items, pid, Atspi)
                    snapshot['button_focused'] = focused_button(items, pid, 'Connect remote host', Atspi)
                    return snapshot
                return nodes(observe, 'review-entry-before-space')
            entry = open_review(lambda: focus('Connect remote host'), read_entry, lambda: keys('-k', 'space'))
            result.setdefault('review_entry_scopes', []).append({key: entry[key] for key in ('scope', 'selected', 'heading', 'button_focused')})
            deadline = time.monotonic() + 20
            wait(lambda: nodes(lambda items: remote_editor_ready(items, pid, Atspi), 'remote-editor-before-text'), deadline=deadline)
            keys(destination)
            wait(lambda: nodes(lambda items: remote_editor_ready(items, pid, Atspi), 'remote-editor-before-return'), deadline=deadline)
            keys('-k', 'Return')
            wait(lambda: review_ready(names(), destination, os.environ['VERANDA_GTK_FINGERPRINT'], known_key), deadline=deadline)
            return entry
        def scope_snapshot(site):
            snapshot = nodes(lambda items: navigation_context(items, pid, Atspi), site)
            trace = result.setdefault('selection_trace', [])
            trace.append({**{key: snapshot[key] for key in ('focused', 'selected', 'scope', 'heading')}, 'site': site})
            del trace[:-128]
            return snapshot
        def wait_scope(expected, site):
            return wait(lambda: selected_scope(scope_snapshot(site), expected))
        def select_default(owner, prefix):
            nonlocal stage
            stage = prefix + ':owner-focus'
            focus(owner, owner_row=True)
            stage = prefix + ':default-key'
            snapshot = scope_snapshot(stage)
            index = snapshot['order'].index(owner)
            assert snapshot['focused'] == index, 'focus_target_unavailable'
            assert snapshot['order'][index + 1] == owner + ' / default' and not snapshot['owners'][index + 1], 'local_scope_changed'
            keys('-k', 'Down')
            stage = prefix + ':default-selected'
            wait_scope(owner + ' / default', stage)
        def projects(expected_scope, prefix):
            nonlocal stage
            stage = prefix + ':projects-before-focus'
            assert selected_scope(scope_snapshot(stage), expected_scope), 'local_scope_changed'
            stage = prefix + ':projects-focus'
            focus('Projects', workspace=True, expected_scope=expected_scope)
            stage = prefix + ':projects-activate'
            assert nodes(lambda items: workspace_tab_ready(items, pid, 'Projects', expected_scope, Atspi), stage)
            keys('-k', 'space')
            stage = prefix + ':projects-visible'
            expected = {f"project-00-{index:02}" for index in range(10)}
            def visible():
                def observe(items):
                    snapshot = navigation_context(items, pid, Atspi)
                    assert selected_scope(snapshot, expected_scope), 'local_scope_changed'
                    return expected <= {node.get_name() for node in items if node.get_state_set().contains(Atspi.StateType.SHOWING)}
                return nodes(observe, stage)
            wait(visible)
        def authenticate_owner():
            review()
            activate('Accept and connect')
            wait(connected_owner)
            result['authenticated_owner_discovered_and_saved'] = True
            root = Path(os.environ['XDG_DATA_HOME']) / 'subyard/veranda'
            saved = (root / 'connections.json').read_bytes()
            result['saved_store_sha256'] = hashlib.sha256(saved).hexdigest()
            return root, saved
        if scenario == 'connect':
            stage = 'initial-local-fleet'
            wait(lambda: 'Connected' in names())
            wait_scope('Local owner / default', stage)
            stage = 'first-key-decline'
            decline_entry = review()
            # Screenshot records the actual native public-fingerprint review.
            capture_view(backend, Path(os.environ['VERANDA_GTK_CAPTURE']).with_name('trust.png'))
            activate('Cancel')
            wait(lambda: entry_scope_preserved(scope_snapshot(stage), decline_entry))
            root = Path(os.environ['XDG_DATA_HOME']) / 'subyard/veranda'
            assert not (root / 'connections.json').exists(), 'decline_registered_host'
            result['first_key_decline_preserved_empty_store'] = True
            result['first_key_decline_preserved_entry_scope'] = True
            stage = 'first-key-accept'
            root, saved = authenticate_owner()
            stage = 'cancel-active-connect'
            block = Path(os.environ['VERANDA_GTK_BLOCK'])
            cancel_entry = review(known_key=True)
            gui = {'pid': pid, 'start': original_identity[1]}
            assert not cancellation_flow_closed(nodes(), Atspi), 'cancel_flow_not_visible'
            baseline = ssh_fixture.owned_ssh(gui)
            arm = {'nonce': os.urandom(16).hex(), 'gui': gui, 'port': int(destination.rsplit(':', 1)[1]),
                   'baseline': [[client['pid'], client['start']] for client in baseline]}
            result['manual_ssh_arm'] = arm
            ssh_fixture.private_json(block, arm)
            stage = 'cancel-authenticated-rpc-blocked'
            activate('Connect')
            marker = block.with_name('blocked-' + arm['nonce'] + '.json')
            wait(marker.exists)
            receipt = ssh_fixture.read_private_json(marker)
            result['attributed_manual_ssh_attempt'] = {'arm': arm, 'receipt': receipt}
            client = ssh_fixture.verify_block(arm, receipt, ssh_fixture.owned_ssh(gui))
            result['authenticated_rpc_block_observed'] = True
            stage = 'cancel-physical-action'
            activate('Cancel')
            stage = 'cancel-connection-completion'
            def cancelled():
                items = nodes()
                flow_closed = cancellation_flow_closed(items, Atspi)
                current = ssh_fixture.identity(client['pid'])
                alive = current is not None and current['start'] == client['start']
                unchanged = (root / 'connections.json').read_bytes() == saved
                assert unchanged, 'cancel_changed_store'
                scope_preserved = (not flow_closed or entry_scope_preserved(
                    scope_snapshot('cancel-entry-scope'), cancel_entry))
                return ssh_fixture.cancel_complete(flow_closed, unchanged, alive) and scope_preserved
            wait(cancelled)
            result['attributed_manual_ssh_dead_before_cleanup'] = True
            block.unlink()
            result['active_connect_cancel_preserved_store'] = True
            result['active_connect_cancel_preserved_entry_scope'] = True
            stage = 'remote-refresh'
            before = wait(observed)
            time.sleep(1.1)
            activate('Refresh', workspace=True)
            wait(lambda: (value := observed()) and value != before)
            result['remote_refresh_observation_changed'] = True
            stage = 'remote-yard-navigation'
            select_default(destination, 'remote-yard-navigation')
            projects(destination + ' / default', 'remote-yard-navigation')
            result['visible_remote_project_names'] = 10
        elif scenario == 'owner-focus-connect':
            result['diagnostic_only'] = True
            stage = 'diagnostic-authenticate-owner'
            wait(lambda: 'Connected' in names())
            wait_scope('Local owner / default', stage)
            authenticate_owner()
            stage = 'diagnostic-save-local-selection'
            select_default('Local owner', stage)
            projects('Local owner / default', stage)
            result['diagnostic_local_default_projects_saved'] = True
        elif scenario == 'remote-restore':
            stage = 'restore-remote-selection'
            wait(lambda: 'Connected' in names())
            wait_scope(destination + ' / default', stage)
            expected = {f"project-00-{index:02}" for index in range(10)}
            wait(lambda: expected <= names())
            result['remote_owner_yard_tab_restored'] = True
            stage = 'switch-local-owner'
            select_default('Local owner', 'switch-local-owner')
            projects('Local owner / default', 'switch-local-owner')
            result['local_owner_selected_and_saved'] = True
        elif scenario in ('local-restore', 'owner-focus-restore'):
            stage = 'restore-local-selection'
            wait(lambda: 'Connected' in names())
            wait_scope('Local owner / default', stage)
            expected = {f"project-00-{index:02}" for index in range(10)}
            wait(lambda: expected <= names())
            result['local_owner_yard_tab_restored'] = True
            stage = 'select-saved-remote-owner'
            focus(destination, owner_row=True)
            wait_scope(destination, stage)
            wait(connected_owner)
            result['saved_remote_selected_with_fresh_inventory'] = True
            if scenario == 'owner-focus-restore':
                result['diagnostic_only'] = True
                view = scope_snapshot(stage)
                assert len(view['order']) == 42 and sum(view['owners']) == 2, 'local_scope_changed'
                result['diagnostic_owner_rows'] = 42
                result['diagnostic_remote_owner_focused'] = True
            else:
                stage = 'repair-host-key'
                block = Path(os.environ['VERANDA_GTK_BLOCK'])
                rotate = block.with_name('rotate')
                rotate.write_text('rotate-v1\n'); rotate.chmod(0o600)
                wait(lambda: block.with_name('rotated').exists())
                activate('Repair host key', workspace=True)
                wait(lambda: 'Repair host key' in names())
                focus('Remote SSH destination')
                keys('-k', 'Return')
                wait(lambda: any(os.environ['VERANDA_GTK_REPLACEMENT'] in name and 'Previous fingerprint:' in name and os.environ['VERANDA_GTK_FINGERPRINT'] in name for name in names()))
                root = Path(os.environ['XDG_DATA_HOME']) / 'subyard/veranda'
                previous = json.loads((root / 'connections.json').read_bytes())['connections'][0]
                activate('Accept and connect')
                wait(connected_owner)
                current = json.loads((root / 'connections.json').read_bytes())['connections'][0]
                assert current['id'] == previous['id'] and current['hostId'] == previous['hostId'], 'repair_changed_owner'
                assert current['fingerprint'] == os.environ['VERANDA_GTK_REPLACEMENT'], 'repair_missing_replacement'
                result['separate_replacement_repair_preserved_owner'] = True
        elif scenario in ('ime', 'ime-restore'):
            stage = 'ime-local-owner'
            if scenario == 'ime':
                wait(lambda: 'Connected' in names())
                wait_scope('Local owner / default', stage)
                focus('Local owner', owner_row=True)
            wait(lambda: 'Connected' in names())
            wait_scope('Local owner', stage)
            stage = 'ime-yard-name-editor'
            if scenario == 'ime':
                focus('Yards', workspace=True, expected_scope='Local owner')
                assert nodes(lambda items: workspace_tab_ready(items, pid, 'Yards', 'Local owner', Atspi), stage)
                keys('-k', 'space')
            wait(lambda: 'Yard name' in names())
            focus('Yard name', workspace=True, expected_scope='Local owner')
            original_editor = None
            def read_ime_editor():
                def observe(items):
                    assert selected_scope(navigation_context(items, pid, Atspi), 'Local owner'), 'local_scope_changed'
                    focused = [node for node in items if node.get_state_set().contains(Atspi.StateType.FOCUSED)]
                    editors = [node for node in items if node.get_process_id() == pid and node.get_role() == Atspi.Role.TEXT
                        and node.get_name() == 'Yard name' and node.get_state_set().contains(Atspi.StateType.SHOWING)
                        and node.get_state_set().contains(Atspi.StateType.EDITABLE)
                        and not node.get_state_set().contains(Atspi.StateType.DEFUNCT)]
                    assert len(editors) == 1 and focused == editors, 'focus_target_unavailable'
                    assert original_editor is None or editors[0] == original_editor, 'ime_editor_replaced'
                    if ime_observation is None:
                        return editors[0], interface_text(editors[0].get_text_iface(), Atspi.Text)
                    proof = dict(scope=True, owned=True, unique_editor=True, focused=True, same_editor=True)
                    return editors[0], interface_text(editors[0].get_text_iface(), Atspi.Text, ime_observation, proof)
                return nodes(observe, stage)
            original_editor, initial = read_ime_editor()
            store = Path(os.environ['XDG_DATA_HOME']) / 'subyard/veranda'
            persisted = lambda: local_ime_draft(store)
            if scenario == 'ime':
                stage = 'ime-two-set-composition'
                assert local_ime_draft(store, ''), 'ime_preedit_committed_early'
                if os.environ.get('VERANDA_GTK_IME_OBSERVE') == '1':
                    ime_observation = {'reads': [], 'dropped': 0}
                result['ime_captures'] = {}
                def capture_ime(name, committed):
                    # Settling delay is not a preedit/paint measurement or assertion.
                    time.sleep(.25)
                    assert read_ime_editor()[1] == committed, 'ime_committed_text_mismatch'
                    assert local_ime_draft(store, committed), 'ime_preedit_committed_early'
                    capture = Path(os.environ['VERANDA_GTK_CAPTURE']).with_name('ime-' + ('preedit-' if name != 'committed' else '') + name + '.png')
                    capture_view(backend, capture)
                    assert read_ime_editor()[1] == committed, 'ime_committed_text_mismatch'
                    assert local_ime_draft(store, committed), 'ime_preedit_committed_early'
                    result['ime_captures'][name] = {'sha256': hashlib.sha256(capture.read_bytes()).hexdigest(),
                        'expected_preedit': {'consonant': '\u3131', 'syllable': '\uac00', 'committed': ''}[name],
                        'observed_committed_text': committed, 'scoped_draft_matches_committed_baseline': True, 'visual_observation': 'pending native screenshot inspection'}
                def ime_keys(*arguments):
                    read_ime_editor()  # Re-prove owned exact editor focus/scope before every physical command.
                    keys(*arguments)
                compose_ime(ime_keys, lambda: read_ime_editor()[1], wait, capture_ime, persisted)
                result['ime_physical_sequence'] = ['Shift+space', 'r', 'k', 'Shift+space']
                result['ime_committed_codepoint'] = 'U+AC00'
                result['ime_scoped_draft_saved'] = True
            else:
                stage = 'ime-restarted-committed-draft'
                assert initial == '\uac00' and persisted(), 'ime_draft_restart_missing'
                result['ime_committed_draft_restart_equal'] = True
            def saved_ime_context():
                path = store / 'navigation.json'
                if not path.exists(): return False
                selected = json.loads(path.read_bytes())
                return selected['connectionId'] is None and selected['yardName'] is None and selected['tab'] == 'Yards'
            wait(saved_ime_context)
            result['ime_scope'] = {'connection': None, 'yard': None, 'tab': 'Yards'}
            result['ime_visual_observation'] = 'pending native screenshot inspection'
        elif scenario == 'profile-diagnostic':
            stage = 'diagnostic-local-profile-ready'
            wait(lambda: 'Connected' in names())
            wait_scope('Local owner / default', stage)
            result['diagnostic_only'] = True
        elif scenario == 'owner-focus':
            stage = 'diagnostic-local-default'
            wait(lambda: 'Connected' in names())
            wait_scope('Local owner / default', stage)
            select_default('Local owner', 'diagnostic-local-default')
            result['named_default_yard_focused'] = True
            stage = 'diagnostic-local-refresh'
            before = wait(observed)
            time.sleep(1.1)
            activate('Refresh', workspace=True)
            wait(lambda: (value := observed()) and value != before)
            result['refresh_observation_changed'] = True
            stage = 'diagnostic-local-owner-focus'
            focus('Local owner', owner_row=True)
            result['local_owner_row_focused'] = True
        else:
            stage = "local-connected"
            wait(lambda: any(node.get_name() == "Connected" for node in nodes()))
            stage = "local-yard-summary"
            wait(lambda: any("Yard state: NOT_CREATED" in node.get_name() and "10 projects" in node.get_name() for node in nodes()))
            stage = "local-observed"
            before = wait(observed)
            result["local_default_selected"] = True
            stage = "physical-refresh"
            deadline = time.monotonic() + 20
            time.sleep(1.1)
            default_key('alt+r', deadline)
            # Shared mnemonics focus the button instead of activating it.
            wait(lambda: any(node.get_name() == 'Refresh' and node.get_state_set().contains(Atspi.StateType.FOCUSED) for node in nodes()), deadline=deadline)
            default_key('space', deadline, (Atspi.Role.PUSH_BUTTON, 'Refresh'))
            after = wait(lambda: (value if (value := observed()) and value != before else None), deadline=deadline)
            result["refresh_observation_changed"] = after != before
            stage = "physical-projects-tab"
            deadline = time.monotonic() + 20
            allowed = {(Atspi.Role.PUSH_BUTTON, name) for name in ('Refresh', 'Shell', 'Copy shell command',
                       'Start', 'Stop', 'Reconcile', 'Connect remote host')}
            allowed.update((Atspi.Role.PAGE_TAB, name) for name in ('Overview', 'Projects', 'Profiles', 'Diagnostics', 'Settings'))
            allowed.add((Atspi.Role.LABEL, 'Yard state: NOT_CREATED\n10 projects\nObserved: ' + after))
            for count in range(65):
                items = default_nodes(deadline)
                focused = {(node.get_role(), node.get_name()) for node in items if node.get_state_set().contains(Atspi.StateType.FOCUSED)}
                assert len(focused) == 1 and focused <= allowed, 'focus_target_unavailable'
                if focused == {(Atspi.Role.PAGE_TAB, 'Projects')}: break
                assert count < 64, 'focus_target_unavailable'
                previous_focus = next(iter(focused))
                default_input(items, Atspi, previous_focus, deadline, lambda: local_key('shift+Tab'))
                wait(lambda: any(node.get_state_set().contains(Atspi.StateType.FOCUSED) and (node.get_role(), node.get_name()) != previous_focus for node in nodes()), deadline=deadline)
            default_key('space', deadline, (Atspi.Role.PAGE_TAB, 'Projects'))
            expected = {f"project-00-{index:02}" for index in range(10)}
            wait(lambda: expected <= {node.get_name() for node in default_nodes(deadline) if node.get_state_set().contains(Atspi.StateType.SHOWING)}, deadline=deadline)
            default_nodes(deadline)
            result["visible_real_project_names"] = 10

        if os.environ.get('VERANDA_GTK_UI') == '1' or scenario == 'profile-diagnostic':
            unicode_draft = '庭院 مرحبا α'
            def host():
                focus(destination, owner_row=True)
                def ready(items):
                    if not selected_scope(navigation_context(items, pid, Atspi), destination): return False
                    visible = {node.get_name() for node in items if node.get_state_set().contains(Atspi.StateType.SHOWING)}
                    return destination in visible and 'Connected' in visible and any('20 yards' in name for name in visible)
                wait(lambda: nodes(ready, 'ui-host-selected'))
            def tab(name, expected_scope):
                focus(name, workspace=True, expected_scope=expected_scope)
                assert nodes(lambda items: workspace_tab_ready(items, pid, name, expected_scope, Atspi), 'ui-tab-activate')
                keys('-k', 'space')
            if scenario == 'profile-diagnostic':
                stage = 'yard-profiles-exact-plan-discard'
                profile_plan_discard(tab, wait, names, focus, keys, activate, 'Local owner / default', result)
            elif scenario == 'connect':
                stage = 'yard-profiles-exact-plan-discard'
                profile_plan_discard(tab, wait, names, focus, keys, activate, destination + ' / default', result)
                stage = 'host-refresh-overview-observation'
                host(); tab('Overview', destination)
                overview_detail = wait(lambda: nodes(lambda items: overview_observed_label(items, pid, destination, Atspi), stage))
                stage = 'host-unicode-editor-visible'
                tab('Yards', destination)
                wait(lambda: 'Yard name' in names())
                focus('Yard name')
                accepted_connections = json.loads(saved)['connections']
                assert len(accepted_connections) == 1
                accepted_connection_id = accepted_connections[0]['id']
                assert isinstance(accepted_connection_id, str) and accepted_connection_id
                stage = 'host-unicode-physical-typing'
                keys('-M', 'ctrl', '-k', 'a', '-m', 'ctrl', unicode_draft)
                deadline = time.monotonic() + 20
                root = Path(os.environ['XDG_DATA_HOME']) / 'subyard/veranda'
                observation = result.setdefault('unicode_observation', {})
                stage = 'host-unicode-committed-entry'
                wait(lambda: nodes(lambda items: committed_host_editor(items, pid, destination,
                    unicode_draft, Atspi, observation), stage), deadline=deadline)
                stage = 'host-unicode-persisted-draft'
                wait(lambda: persisted_host_draft(root, accepted_connection_id, unicode_draft, observation), deadline=deadline)
                node = next(node for node in nodes() if node.get_name() == 'Yard name' and node.get_state_set().contains(Atspi.StateType.FOCUSED))
                assert interface_text(node.get_text_iface(), Atspi.Text) == unicode_draft, 'unicode_gtk_text_mismatch'
                result['unicode_rtl_gtk_draft_saved'] = True
                stage = 'editor-refresh-baseline'
                original_editor = node
                keys('-k', 'Left', '-M', 'shift', '-k', 'Left', '-k', 'Left', '-m', 'shift')
                original_caret, original_selections = selection_baseline(node.get_text_iface(), Atspi.Text, len(unicode_draft))
                before_refresh = wait(lambda: retained_observed(overview_detail, pid, Atspi, owned))
                time.sleep(1.1)
                stage = 'editor-refresh-action'
                activate('Refresh', workspace=True)
                stage = 'editor-refresh-completion'
                wait(lambda: (value := retained_observed(overview_detail, pid, Atspi, owned)) and value != before_refresh)
                # Compare before keyboard refocus, which may select all entry text.
                stage = 'editor-refresh-continuity'
                assert nodes(lambda items: editor_continuity(items, pid, Atspi, original_editor,
                    unicode_draft, original_caret, original_selections), 'editor-refresh-continuity')
                result['editor_continuity_focus'] = 'Refresh'
                result['same_scope_refresh_retained_editor_caret_selection'] = True
                stage = 'host-sync-separated-metadata'
                tab('Sync', destination)
                wait(lambda: (visible := names()) and any('Configuration Git' in name for name in visible) and any('Credentials · REDACTED' in name for name in visible))
                result['typed_git_credentials_sections_visible'] = True
                select_default(destination, 'ui-remote-yard')
                projects(destination + ' / default', 'ui-remote-yard')
            elif scenario == 'remote-restore':
                stage = 'host-draft-restored-across-restart'
                # This lifetime already switched to local/default above. Inspect
                # the private saved draft without changing the selected local yard.
                root = Path(os.environ['XDG_DATA_HOME']) / 'subyard/veranda'
                assert any(draft['scope']['connection'] is not None and draft['scope']['yard'] is None and draft['field'] == 'create:name' and draft['value'] == unicode_draft for draft in json.loads((root / 'desktop.json').read_bytes())['drafts']), 'draft_restart_missing'
                result['draft_restart_preserved'] = True
            elif scenario == 'local-restore':
                stage = 'host-draft-rendered-after-owner-reselection'
                tab('Yards', destination); wait(lambda: 'Yard name' in names()); focus('Yard name')
                node = next(node for node in nodes() if node.get_name() == 'Yard name' and node.get_state_set().contains(Atspi.StateType.FOCUSED))
                assert interface_text(node.get_text_iface(), Atspi.Text) == unicode_draft, 'draft_restart_missing'
                result['draft_restart_rendered'] = True
                tab('Overview', destination)
        if scenario in ('connect', 'remote-restore', 'local-restore', 'owner-focus-connect', 'owner-focus-restore'):
            stage = 'saved-navigation-context'
            root = Path(os.environ['XDG_DATA_HOME']) / 'subyard/veranda'
            restored_owner = scenario in ('local-restore', 'owner-focus-restore')
            expected_yard = None if restored_owner else 'default'
            expected_tab = 'Overview' if restored_owner else 'Projects'
            def saved_context():
                record = json.loads((root / 'connections.json').read_bytes())['connections'][0]
                selected = json.loads((root / 'navigation.json').read_bytes())
                expected_connection = None if scenario in ('remote-restore', 'owner-focus-connect') else record['id']
                return (selected['connectionId'] == expected_connection
                        and selected['yardName'] == expected_yard and selected['tab'] == expected_tab)
            wait(saved_context)
            result['saved_owner_yard_tab_verified'] = True
        capture = Path(os.environ['VERANDA_GTK_CAPTURE'])
        capture_view(backend, capture)
        result['useful_view_capture_sha256'] = hashlib.sha256(capture.read_bytes()).hexdigest()
        stage = "physical-close"
        if scenario == 'local' and os.environ.get('VERANDA_GTK_UI') != '1':
            default_key('alt+F4', time.monotonic() + 20)
        else:
            local_key('alt+F4')
        result["original_exit_code"] = 0
        result["read_only_scope_verified"] = os.environ.get("VERANDA_GTK_UI") != "1" and scenario not in ('ime', 'ime-restore', 'profile-diagnostic')
        result["owner_mutation_executed"] = False
    except BaseException as error:
        result.update(failure_stage=stage, failure_type=type(error).__name__)
        if ime_observation is not None:
            result['ime_observation'] = ime_observation
        reasons = {'owned_process_terminated', 'owned_process_identity_changed',
                   'duplicate_owned_accessible_application', 'accessible_depth_exceeded',
                   'accessible_node_limit_exceeded', 'accessible_children_unavailable', 'foreign_accessible_node', 'local_scope_changed',
                   'accessible_child_limit_exceeded', 'accessible_text_limit_exceeded', 'focus_target_unavailable', 'decline_registered_host', 'cancel_changed_store', 'repair_changed_owner', 'repair_missing_replacement', 'unicode_gtk_text_mismatch', 'draft_restart_missing', 'refresh_replaced_editor', 'refresh_reset_caret', 'refresh_reset_selection', 'owned_x11_window_unavailable', 'owned_x11_window_changed', 'x11_capture_failed', 'ime_initial_editor_not_empty', 'ime_preedit_committed_early', 'ime_editor_replaced', 'ime_committed_text_mismatch', 'ime_draft_restart_missing', 'ime_draft_unavailable'}
        if isinstance(error, AssertionError) and str(error) in reasons:
            result['failure_reason'] = str(error)
        traceback = error.__traceback__
        while traceback is not None:
            if traceback.tb_frame.f_code.co_filename == __file__:
                result['failure_source_frame'] = {'function': traceback.tb_frame.f_code.co_name,
                                                  'line': traceback.tb_lineno}
            traceback = traceback.tb_next
        def collect():
            diagnostic = {'atspi': dict(last_accessibility), 'tree': list(last_tree),
                          'snapshot': dict(snapshot_state)}
            if 'attributed_manual_ssh_attempt' in result:
                client = result['attributed_manual_ssh_attempt']['receipt']['client']
                diagnostic['attributed_ssh_identity_at_failure'] = ssh_fixture.identity(client['pid'])
            try:
                current = identity()
                assert current == original_identity
                diagnostic['process'] = {'pid': pid, 'uid': current[0], 'start': current[1],
                    'exe_device': current[2], 'exe_inode': current[3], 'same_owned_identity': True}
                if backend == 'x11':
                    windows = subprocess.run(['xdotool', 'search', '--onlyvisible', '--pid', str(pid)],
                        capture_output=True, timeout=3)
                    diagnostic['windows'] = {'search_exit_code': windows.returncode, 'items': []}
                    deadline = time.monotonic() + 3
                    for window in windows.stdout[:4096].splitlines()[:16]:
                        if not window.isdigit() or time.monotonic() >= deadline:
                            break
                        owner = subprocess.check_output(['xdotool', 'getwindowpid', window.decode()], timeout=1)
                        diagnostic['windows']['items'].append({'id': window.decode(), 'pid': int(owner)})
            except BaseException as diagnostic_error:
                diagnostic['process_window_error'] = type(diagnostic_error).__name__
            if backend == 'x11':
                try:
                    capture = Path(os.environ['VERANDA_GTK_CAPTURE']).with_name('failure.png')
                    capture_view(backend, capture)
                    diagnostic['screenshot_sha256'] = hashlib.sha256(capture.read_bytes()).hexdigest()
                except BaseException as diagnostic_error:
                    diagnostic['screenshot_error'] = type(diagnostic_error).__name__
            return diagnostic
        retain_failure_diagnostics(result, collect)
    result['incomplete_tree_retries'] = snapshot_state.get('incomplete_tree_retries', 0)
    result["duration_seconds"] = time.monotonic() - started
    print("VERANDA_GTK_INPUT " + json.dumps(result), flush=True)
    return result["original_exit_code"]


if __name__ == "__main__":
    raise SystemExit(main())
