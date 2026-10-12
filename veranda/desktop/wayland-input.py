#!/usr/bin/env python3
"""Read GTK accessibility, send actual Wayland keys, then close naturally."""
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import time
import gi

gi.require_version("Atspi", "2.0")
from gi.repository import Atspi, Gio, GLib


def main():
    pid = int(os.environ["VERANDA_GTK_PID"])
    started = time.monotonic()
    stage = "accessibility"
    result = {"original_exit_code": 1, "physical_key_commands": 0,
              "accessibility_actions_used": False}
    def identity():
        process = Path('/proc') / str(pid)
        fields = (process / 'stat').read_text().rsplit(')', 1)[1].split()
        assert fields[0] not in ('Z', 'X'), 'owned_process_terminated'
        exe = (process / 'exe').stat()
        return process.stat().st_uid, int(fields[19]), exe.st_dev, exe.st_ino
    original_identity = identity()
    assert original_identity[0] == os.getuid()
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

        def nodes():
            owned()
            for _ in range(64):
                if not context.pending(): break
                context.iteration(False)
            desktop = Atspi.get_desktop(0)
            applications = [desktop.get_child_at_index(i) for i in range(desktop.get_child_count())]
            apps = [app for app in applications if app and app.get_process_id() == pid]
            assert len(apps) <= 1, 'duplicate_owned_accessible_application'
            if not apps: return []
            app = apps[0]
            app.clear_cache()
            queue, items = [(app, 0)], []
            while queue:
                node, depth = queue.pop()
                if not node: continue
                assert depth <= 32, 'accessible_depth_exceeded'
                assert len(items) < 2048, 'accessible_node_limit_exceeded'
                if node.get_state_set().contains(Atspi.StateType.DEFUNCT): continue
                items.append(node)
                count = node.get_child_count()
                assert count >= 0, 'accessible_children_unavailable'
                assert count <= 512, 'accessible_child_limit_exceeded'
                queue.extend((node.get_child_at_index(i), depth + 1) for i in range(count))
            return items

        def wait(predicate, seconds=20):
            deadline = time.monotonic() + seconds
            while time.monotonic() < deadline:
                value = predicate()
                if value: return value
                time.sleep(.05)
            raise TimeoutError(stage)

        def keys(*arguments):
            owned()
            subprocess.run(["wtype", "-s", "100", *arguments, "-s", "100"], check=True,
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=5)
            result["physical_key_commands"] += 1

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
            stage = "initial-local-fleet"
            wait(lambda: any(node.get_name() == "Connected" for node in nodes()))
            wait(lambda: any("Yard state: NOT_CREATED" in node.get_name() and "10 projects" in node.get_name() for node in nodes()))
            before = wait(observed)
            result["local_default_selected"] = True
            stage = "physical-refresh"
            time.sleep(1.1)
            keys("-M", "alt", "-k", "r", "-m", "alt")
            after = wait(lambda: (value if (value := observed()) and value != before else None))
            result["refresh_observation_changed"] = after != before
            stage = "physical-projects-tab"
            for count in range(65):
                buttons = [node for node in nodes() if node.get_name() == "Projects" and node.get_state_set().contains(Atspi.StateType.FOCUSED)]
                if buttons: break
                assert count < 64
                keys("-k", "Tab")
            keys("-k", "space")
            expected = {f"project-00-{index:02}" for index in range(10)}
            wait(lambda: expected <= {node.get_name() for node in nodes() if node.get_state_set().contains(Atspi.StateType.SHOWING)})
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
        assert capture.parent.resolve() == capture.parent and not capture.exists()
        fd = os.open(capture, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
        os.fchmod(fd, 0o600)
        with os.fdopen(fd, 'wb') as output:
            subprocess.run(['grim', '-'], check=True, env=os.environ,
                           stdout=output, stderr=subprocess.DEVNULL, timeout=5)
        assert capture.stat().st_nlink == 1 and capture.stat().st_size <= 2 * 1024 * 1024
        result['useful_view_capture_sha256'] = hashlib.sha256(capture.read_bytes()).hexdigest()
        stage = "physical-close"
        keys("-M", "alt", "-k", "F4", "-m", "alt")
        result["original_exit_code"] = 0
        result["read_only_scope_verified"] = os.environ.get("VERANDA_GTK_UI") != "1"
        result["owner_mutation_executed"] = False
    except BaseException as error:
        result.update(failure_stage=stage, failure_type=type(error).__name__)
        reasons = {'owned_process_terminated', 'owned_process_identity_changed',
                   'duplicate_owned_accessible_application', 'accessible_depth_exceeded',
                   'accessible_node_limit_exceeded', 'accessible_children_unavailable',
                   'accessible_child_limit_exceeded', 'focus_target_unavailable', 'decline_registered_host', 'cancel_changed_store', 'repair_changed_owner', 'repair_missing_replacement', 'unicode_gtk_text_mismatch', 'draft_restart_missing', 'refresh_replaced_editor', 'refresh_reset_caret', 'refresh_reset_selection'}
        if isinstance(error, AssertionError) and str(error) in reasons:
            result['failure_reason'] = str(error)
    result["duration_seconds"] = time.monotonic() - started
    print("VERANDA_GTK_INPUT " + json.dumps(result), flush=True)
    return result["original_exit_code"]


if __name__ == "__main__":
    raise SystemExit(main())
