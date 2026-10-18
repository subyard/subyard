#!/usr/bin/env python3
"""Read GTK accessibility, send actual Wayland/XTEST keys, then close naturally."""
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time


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
    # Only the local SHORT keys; no remote/UI key translation.
    if key not in ('alt+r', 'Tab', 'shift+Tab', 'space', 'alt+F4'):
        raise ValueError('unsupported local X11 key')
    windows = subprocess.check_output(['xdotool', 'search', '--onlyvisible', '--pid', str(pid)], timeout=3).splitlines()
    assert len(windows) == 1 and windows[0].isdigit(), 'owned_x11_window_unavailable'
    window = windows[0].decode()
    assert int(subprocess.check_output(['xdotool', 'getwindowpid', window], timeout=3)) == pid, 'owned_x11_window_changed'
    subprocess.run(['xdotool', 'windowactivate', '--sync', window], check=True, timeout=3)
    # Omitting --window asks XTEST to deliver physical input to the focused window.
    subprocess.run(['xdotool', 'key', '--clearmodifiers', key], check=True, timeout=3)


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


def main():
    import gi
    gi.require_version("Atspi", "2.0")
    from gi.repository import Atspi, Gio, GLib
    pid = int(os.environ["VERANDA_GTK_PID"])
    backend = os.environ.get('VERANDA_GTK_BACKEND', 'wayland')
    assert backend in ('x11', 'wayland')
    assert backend != 'x11' or (os.environ.get('VERANDA_GTK_SCENARIO', 'local') == 'local' and os.environ.get('VERANDA_GTK_UI') != '1')
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

        def nodes():
            items = complete_tree(read_tree, read_deadline or time.monotonic() + 20, snapshot_state)
            if (os.environ.get('VERANDA_GTK_SCENARIO', 'local') == 'local'
                    and os.environ.get('VERANDA_GTK_UI') != '1' and time.monotonic() >= snapshot_state['next']):
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
            assert backend == 'wayland'
            owned()
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
        def focus(name):
            for count in range(65):
                if any(node.get_name() == name and node.get_state_set().contains(Atspi.StateType.FOCUSED) for node in nodes()): return
                assert count < 64, 'focus_target_unavailable'
                keys('-k', 'Tab')
        def review():
            keys('-M', 'alt', '-k', 'c', '-m', 'alt')
            wait(lambda: 'Connect remote host' in names())
            keys(destination, '-k', 'Return')
            wait(lambda: any(os.environ['VERANDA_GTK_FINGERPRINT'] in name and 'Verify' in name for name in names()))
        def projects():
            focus('Projects')
            keys('-k', 'space')
            expected = {f"project-00-{index:02}" for index in range(10)}
            wait(lambda: expected <= names())
        if scenario == 'connect':
            stage = 'initial-local-fleet'
            wait(lambda: 'Connected' in names() and 'Local owner / default' in names())
            stage = 'first-key-decline'
            review()
            # Screenshot records the actual native public-fingerprint review.
            subprocess.run(['grim', str(Path(os.environ['VERANDA_GTK_CAPTURE']).with_name('trust.png'))], check=True, stderr=subprocess.DEVNULL, timeout=5)
            keys('-M', 'alt', '-k', 'n', '-m', 'alt')
            wait(lambda: 'Local owner / default' in names())
            root = Path(os.environ['XDG_DATA_HOME']) / 'subyard/veranda'
            assert not (root / 'connections.json').exists(), 'decline_registered_host'
            result['first_key_decline_preserved_empty_store'] = True
            stage = 'first-key-accept'
            review()
            keys('-M', 'alt', '-k', 'a', '-m', 'alt')
            wait(lambda: destination in names() and 'Connected' in names() and any('20 yards' in name for name in names()))
            result['authenticated_owner_discovered_and_saved'] = True
            saved = (root / 'connections.json').read_bytes()
            result['saved_store_sha256'] = hashlib.sha256(saved).hexdigest()
            stage = 'cancel-active-connect'
            block = Path(os.environ['VERANDA_GTK_BLOCK'])
            block.write_text('block-v1\n'); block.chmod(0o600)
            review()
            focus('Connect')
            keys('-k', 'space')
            wait(lambda: block.with_name('blocked-start').exists())
            keys('-M', 'alt', '-k', 'n', '-m', 'alt')
            wait(lambda: 'Connected' in names() and not any('Cancelling connection' in name for name in names()))
            assert (root / 'connections.json').read_bytes() == saved, 'cancel_changed_store'
            block.unlink()
            result['active_connect_cancel_preserved_store'] = True
            stage = 'remote-refresh'
            before = wait(observed)
            time.sleep(1.1)
            keys('-M', 'alt', '-k', 'r', '-m', 'alt')
            wait(lambda: (value := observed()) and value != before)
            result['remote_refresh_observation_changed'] = True
            stage = 'remote-yard-navigation'
            focus(destination)
            keys('-k', 'Down')
            wait(lambda: destination + ' / default' in names())
            projects()
            result['visible_remote_project_names'] = 10
        elif scenario == 'remote-restore':
            stage = 'restore-remote-selection'
            wait(lambda: destination + ' / default' in names() and 'Connected' in names())
            expected = {f"project-00-{index:02}" for index in range(10)}
            wait(lambda: expected <= names())
            result['remote_owner_yard_tab_restored'] = True
            stage = 'switch-local-owner'
            focus(destination + ' / default')
            keys('-k', 'Home')
            wait(lambda: 'Local owner' in names() and any('20 yards' in name for name in names()))
            keys('-k', 'Down')
            wait(lambda: 'Local owner / default' in names())
            projects()
            result['local_owner_selected_and_saved'] = True
        elif scenario == 'local-restore':
            stage = 'restore-local-selection'
            wait(lambda: 'Local owner / default' in names() and 'Connected' in names())
            expected = {f"project-00-{index:02}" for index in range(10)}
            wait(lambda: expected <= names())
            result['local_owner_yard_tab_restored'] = True
            stage = 'select-saved-remote-owner'
            focus('Local owner / default')
            keys('-k', 'Home')
            # 20 local yards follow the local owner; the next row is saved remote.
            keys(*sum((['-k', 'Down'] for _ in range(21)), []))
            wait(lambda: destination in names() and 'Connected' in names() and any('20 yards' in name for name in names()))
            result['saved_remote_selected_with_fresh_inventory'] = True
            stage = 'repair-host-key'
            block = Path(os.environ['VERANDA_GTK_BLOCK'])
            rotate = block.with_name('rotate')
            rotate.write_text('rotate-v1\n'); rotate.chmod(0o600)
            wait(lambda: block.with_name('rotated').exists())
            keys('-M', 'alt', '-k', 'k', '-m', 'alt')
            wait(lambda: 'Repair host key' in names())
            focus('Remote SSH destination')
            keys('-k', 'Return')
            wait(lambda: any(os.environ['VERANDA_GTK_REPLACEMENT'] in name and 'Previous fingerprint:' in name and os.environ['VERANDA_GTK_FINGERPRINT'] in name for name in names()))
            root = Path(os.environ['XDG_DATA_HOME']) / 'subyard/veranda'
            previous = json.loads((root / 'connections.json').read_bytes())['connections'][0]
            keys('-M', 'alt', '-k', 'a', '-m', 'alt')
            wait(lambda: destination in names() and 'Connected' in names() and any('20 yards' in name for name in names()))
            current = json.loads((root / 'connections.json').read_bytes())['connections'][0]
            assert current['id'] == previous['id'] and current['hostId'] == previous['hostId'], 'repair_changed_owner'
            assert current['fingerprint'] == os.environ['VERANDA_GTK_REPLACEMENT'], 'repair_missing_replacement'
            result['separate_replacement_repair_preserved_owner'] = True
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
                focus(destination + ' / default')
                keys('-k', 'Up')
                wait(lambda: destination in names() and any('20 yards' in name for name in names()))
            def tab(name):
                focus(name); keys('-k', 'space')
            if scenario == 'connect':
                stage = 'yard-profiles-exact-plan-discard'
                tab('Profiles')
                wait(lambda: any('Effective profile selection:' in name for name in names()))
                focus('Review profile selection'); keys('-k', 'space')
                wait(lambda: any('Confirmation:' in name and 'Expires:' in name for name in names()))
                keys('-M', 'alt', '-k', 'd', '-m', 'alt')
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
                keys('-M', 'alt', '-k', 'r', '-m', 'alt')
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
                focus(destination); keys('-k', 'Down')
                wait(lambda: destination + ' / default' in names()); projects()
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
        if os.environ.get('VERANDA_GTK_SCENARIO', 'local') == 'local' and os.environ.get('VERANDA_GTK_UI') != '1':
            def collect():
                diagnostic = {'atspi': dict(last_accessibility), 'tree': list(last_tree),
                              'snapshot': dict(snapshot_state)}
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
