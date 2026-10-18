#!/usr/bin/env python3
"""Read GTK accessibility, send actual Wayland/XTEST keys, then close naturally."""
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time


spec = importlib.util.spec_from_file_location('ssh_fixture', Path(__file__).with_name('ssh-fixture.py'))
ssh_fixture = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ssh_fixture)


class IncompleteTree(Exception):
    pass


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
        owned()
        if kind == 'key':
            x11_key(pid, value)
        else:
            x11_input(pid, ['type', '--clearmodifiers', '--delay', '25', '--', value])


def retain_failure_diagnostics(result, collect):
    try:
        result['failure_diagnostics'] = collect()
    except BaseException as error:
        result['diagnostic_failure_type'] = type(error).__name__


def capture_view(backend, capture):
    assert capture.parent.resolve() == capture.parent and not capture.exists()
    fd = os.open(capture, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    os.fchmod(fd, 0o600)
    original = os.fstat(fd)
    try:
        with os.fdopen(fd, 'wb') as output:
            if backend == 'wayland':
                subprocess.run(['grim', '-'], check=True, env=os.environ,
                               stdout=output, stderr=subprocess.DEVNULL, timeout=5)
        if backend == 'x11':
            code = ('import importlib.util,pathlib,sys\n'
                    'spec=importlib.util.spec_from_file_location("probe",sys.argv[1])\n'
                    'probe=importlib.util.module_from_spec(spec);spec.loader.exec_module(probe)\n'
                    'assert probe.screenshot(sys.argv[2],pathlib.Path(sys.argv[3]))\n')
            subprocess.run([sys.executable, '-c', code,
                str(Path(__file__).resolve().parents[2] / 'dev/measure-veranda.py'),
                os.environ['DISPLAY'], str(capture)], check=True, timeout=5,
                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        assert capture.stat().st_nlink == 1 and capture.stat().st_size <= 2 * 1024 * 1024
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
        assert node is not None and node in items, 'focus_target_unavailable'
        states = node.get_state_set()
        assert (node.get_process_id() == pid and node.get_role() == role
                and states.contains(atspi.StateType.SHOWING)
                and not states.contains(atspi.StateType.DEFUNCT)), 'focus_target_unavailable'
    lists = [node for node in items if node.get_role() == atspi.Role.LIST
             and node.get_name() == 'Owner hosts and yards']
    assert len(lists) == 1, 'focus_target_unavailable'
    navigation = lists[0]
    valid(navigation, atspi.Role.LIST)
    assert 0 < navigation.get_child_count() <= 64, 'focus_target_unavailable'
    rows, order, owners = [], [], []
    for index in range(navigation.get_child_count()):
        row = navigation.get_child_at_index(index)
        valid(row, atspi.Role.LIST_ITEM)
        assert row.get_parent() == navigation and row.get_child_count() == 1, 'focus_target_unavailable'
        child = row.get_child_at_index(0)
        valid(child, atspi.Role.LABEL)
        assert child.get_parent() == row, 'focus_target_unavailable'
        rows.append(row)
        owners.append(not row.get_name())
        order.append(row.get_name() or child.get_name())
    assert all(order) and len(set(order)) == len(order), 'focus_target_unavailable'
    focused = [node for node in items if node.get_state_set().contains(atspi.StateType.FOCUSED)]
    assert len(focused) == 1, 'focus_target_unavailable'
    valid(focused[0], focused[0].get_role())
    return {'order': order, 'owners': owners,
            'focused': rows.index(focused[0]) if focused[0] in rows else None}


def focused_button(items, pid, name, atspi):
    focused = [node for node in items if node.get_state_set().contains(atspi.StateType.FOCUSED)]
    return (focus_matches(items, pid, name, atspi) and len(focused) == 1
            and focused[0].get_role() == atspi.Role.PUSH_BUTTON)


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
        left_navigation = True
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
    navigation = next(node for node in items if node.get_role() == atspi.Role.LIST
                      and node.get_name() == 'Owner hosts and yards')
    selected = [index for index in range(navigation.get_child_count())
                if navigation.get_child_at_index(index).get_state_set().contains(atspi.StateType.SELECTED)]
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
        if order is None:
            order, owners = snapshot['order'], snapshot['owners']
            assert order.count(target) == 1 and owners[order.index(target)], 'focus_target_unavailable'
        assert snapshot['order'] == order and snapshot['owners'] == owners, 'focus_target_unavailable'
        current = snapshot['focused']
        assert expected is None or current == expected, 'focus_target_unavailable'
        if trace is not None:
            trace.append(dict(snapshot, target=target, expected=expected, step=count))
        if current == order.index(target):
            return
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
        process = Path('/proc') / str(pid)
        fields = (process / 'stat').read_text().rsplit(')', 1)[1].split()
        assert fields[0] not in ('Z', 'X'), 'owned_process_terminated'
        exe = (process / 'exe').stat()
        return process.stat().st_uid, int(fields[19]), exe.st_dev, exe.st_ino
    original_identity = identity()
    assert original_identity[0] == os.getuid()
    last_tree, last_accessibility = [], {}
    snapshot_state = {'next': 0, 'truncated': False}
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

        def nodes(observe=None, site='navigation'):
            if observe is not None:
                return coherent_observation(read_tree, observe, read_deadline or time.monotonic() + 20,
                    result.setdefault('navigation_read', {}), GLib.Error, site)
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
            owned()
            if backend == 'x11':
                x11_keys(pid, arguments, owned)
            else:
                subprocess.run(["wtype", "-s", "100", *arguments, "-s", "100"], check=True,
                               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=5)
            result["physical_key_commands"] += 1

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
        destination = os.environ.get('VERANDA_GTK_DESTINATION', '')
        def names():
            return {node.get_name() for node in nodes() if node.get_state_set().contains(Atspi.StateType.SHOWING)}
        def focus(name, owner_row=False, workspace=False):
            if owner_row:
                def read_navigation():
                    def observe(items):
                        snapshot = navigation_snapshot(items, pid, Atspi)
                        diagnostic = owner_focus_observation(items, pid, name, Atspi) if scenario == 'owner-focus' else None
                        return snapshot, diagnostic
                    snapshot, diagnostic = nodes(observe)
                    if diagnostic is not None:
                        result.setdefault('focus_trace', []).append(dict(diagnostic, target=name, owner_row=True))
                    return snapshot
                navigate_owner(read_navigation, lambda key: keys('-k', key), name,
                               result.setdefault('owner_navigation', []))
                assert nodes(lambda items: focus_matches(items, pid, name, Atspi, True), 'final-focus'), 'focus_target_unavailable'
                return
            def read_control():
                def observe(items):
                    snapshot = navigation_context(items, pid, Atspi)
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
            wait(lambda: 'Connect remote host' in names())
            keys(destination, '-k', 'Return')
            wait(lambda: review_ready(names(), destination, os.environ['VERANDA_GTK_FINGERPRINT'], known_key))
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
            focus('Projects', workspace=True)
            stage = prefix + ':projects-activate'
            assert selected_scope(scope_snapshot(stage), expected_scope), 'local_scope_changed'
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
        if scenario == 'connect':
            stage = 'initial-local-fleet'
            wait_scope('Local owner / default', stage)
            wait(lambda: 'Connected' in names())
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
            review()
            activate('Accept and connect')
            wait(lambda: destination in names() and 'Connected' in names() and any('20 yards' in name for name in names()))
            result['authenticated_owner_discovered_and_saved'] = True
            saved = (root / 'connections.json').read_bytes()
            result['saved_store_sha256'] = hashlib.sha256(saved).hexdigest()
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
        elif scenario == 'remote-restore':
            stage = 'restore-remote-selection'
            wait_scope(destination + ' / default', stage)
            wait(lambda: 'Connected' in names())
            expected = {f"project-00-{index:02}" for index in range(10)}
            wait(lambda: expected <= names())
            result['remote_owner_yard_tab_restored'] = True
            stage = 'switch-local-owner'
            select_default('Local owner', 'switch-local-owner')
            projects('Local owner / default', 'switch-local-owner')
            result['local_owner_selected_and_saved'] = True
        elif scenario == 'local-restore':
            stage = 'restore-local-selection'
            wait_scope('Local owner / default', stage)
            wait(lambda: 'Connected' in names())
            expected = {f"project-00-{index:02}" for index in range(10)}
            wait(lambda: expected <= names())
            result['local_owner_yard_tab_restored'] = True
            stage = 'select-saved-remote-owner'
            focus(destination, owner_row=True)
            wait_scope(destination, stage)
            wait(lambda: destination in names() and 'Connected' in names() and any('20 yards' in name for name in names()))
            result['saved_remote_selected_with_fresh_inventory'] = True
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
            wait(lambda: destination in names() and 'Connected' in names() and any('20 yards' in name for name in names()))
            current = json.loads((root / 'connections.json').read_bytes())['connections'][0]
            assert current['id'] == previous['id'] and current['hostId'] == previous['hostId'], 'repair_changed_owner'
            assert current['fingerprint'] == os.environ['VERANDA_GTK_REPLACEMENT'], 'repair_missing_replacement'
            result['separate_replacement_repair_preserved_owner'] = True
        elif scenario == 'owner-focus':
            stage = 'diagnostic-local-default'
            wait_scope('Local owner / default', stage)
            wait(lambda: 'Connected' in names())
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

        if os.environ.get('VERANDA_GTK_UI') == '1':
            unicode_draft = '庭院 مرحبا α'
            def host():
                focus(destination, owner_row=True)
                wait(lambda: destination in names() and any('20 yards' in name for name in names()))
            def tab(name):
                focus(name); keys('-k', 'space')
            if scenario == 'connect':
                stage = 'yard-profiles-exact-plan-discard'
                tab('Profiles')
                wait(lambda: any('Effective profile selection:' in name for name in names()))
                focus('Review profile selection'); keys('-k', 'space')
                wait(lambda: any('Confirmation:' in name and 'Expires:' in name for name in names()))
                activate('Discard plan')
                wait(lambda: any('Plan discarded.' in name for name in names()))
                result['typed_profile_exact_plan_discard'] = True
                stage = 'host-unicode-draft'
                host(); tab('Yards')
                wait(lambda: 'Yard name' in names())
                focus('Yard name'); keys('-M', 'ctrl', '-k', 'a', '-m', 'ctrl', unicode_draft)
                root = Path(os.environ['XDG_DATA_HOME']) / 'subyard/veranda'
                wait(lambda: (root / 'desktop.json').exists() and any(draft['field'] == 'create:name' and draft['value'] == unicode_draft for draft in json.loads((root / 'desktop.json').read_bytes())['drafts']))
                node = next(node for node in nodes() if node.get_name() == 'Yard name' and node.get_state_set().contains(Atspi.StateType.FOCUSED))
                assert node.get_text_iface().get_text(0, -1) == unicode_draft, 'unicode_gtk_text_mismatch'
                result['unicode_rtl_gtk_draft_saved'] = True
                stage = 'same-scope-editor-refresh-continuity'
                original_editor = node
                keys('-k', 'Left', '-M', 'shift', '-k', 'Left', '-k', 'Left', '-m', 'shift')
                original_caret = node.get_text_iface().get_caret_offset()
                original_selections = node.get_text_iface().get_n_selections()
                before_refresh = wait(observed)
                time.sleep(1.1)
                activate('Refresh', workspace=True)
                wait(lambda: (value := observed()) and value != before_refresh)
                focus('Yard name')
                node = next(node for node in nodes() if node.get_name() == 'Yard name' and node.get_state_set().contains(Atspi.StateType.FOCUSED))
                assert node == original_editor, 'refresh_replaced_editor'
                assert node.get_text_iface().get_text(0, -1) == unicode_draft, 'unicode_gtk_text_mismatch'
                assert node.get_text_iface().get_caret_offset() == original_caret, 'refresh_reset_caret'
                assert node.get_text_iface().get_n_selections() == original_selections, 'refresh_reset_selection'
                result['same_scope_refresh_retained_editor_caret_selection'] = True
                stage = 'host-sync-separated-metadata'
                tab('Sync')
                wait(lambda: any('Configuration Git' in name for name in names()) and any('Credentials · REDACTED' in name for name in names()))
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
                tab('Yards'); wait(lambda: 'Yard name' in names()); focus('Yard name')
                node = next(node for node in nodes() if node.get_name() == 'Yard name' and node.get_state_set().contains(Atspi.StateType.FOCUSED))
                assert node.get_text_iface().get_text(0, -1) == unicode_draft, 'draft_restart_missing'
                result['draft_restart_rendered'] = True
        if scenario in ('connect', 'remote-restore', 'local-restore'):
            stage = 'saved-navigation-context'
            root = Path(os.environ['XDG_DATA_HOME']) / 'subyard/veranda'
            expected_yard = None if scenario == 'local-restore' else 'default'
            expected_tab = 'Overview' if scenario == 'local-restore' else 'Projects'
            def saved_context():
                record = json.loads((root / 'connections.json').read_bytes())['connections'][0]
                selected = json.loads((root / 'navigation.json').read_bytes())
                expected_connection = None if scenario == 'remote-restore' else record['id']
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
        result["read_only_scope_verified"] = os.environ.get("VERANDA_GTK_UI") != "1"
        result["owner_mutation_executed"] = False
    except BaseException as error:
        result.update(failure_stage=stage, failure_type=type(error).__name__)
        reasons = {'owned_process_terminated', 'owned_process_identity_changed',
                   'duplicate_owned_accessible_application', 'accessible_depth_exceeded',
                   'accessible_node_limit_exceeded', 'accessible_children_unavailable', 'foreign_accessible_node', 'local_scope_changed',
                   'accessible_child_limit_exceeded', 'focus_target_unavailable', 'decline_registered_host', 'cancel_changed_store', 'repair_changed_owner', 'repair_missing_replacement', 'unicode_gtk_text_mismatch', 'draft_restart_missing', 'refresh_replaced_editor', 'refresh_reset_caret', 'refresh_reset_selection', 'owned_x11_window_unavailable', 'owned_x11_window_changed', 'x11_capture_failed'}
        if isinstance(error, AssertionError) and str(error) in reasons:
            result['failure_reason'] = str(error)
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
