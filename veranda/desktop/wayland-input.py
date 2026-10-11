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
        result["read_only_scope_verified"] = True
    except BaseException as error:
        result.update(failure_stage=stage, failure_type=type(error).__name__)
        reasons = {'owned_process_terminated', 'owned_process_identity_changed',
                   'duplicate_owned_accessible_application', 'accessible_depth_exceeded',
                   'accessible_node_limit_exceeded', 'accessible_children_unavailable',
                   'accessible_child_limit_exceeded'}
        if isinstance(error, AssertionError) and str(error) in reasons:
            result['failure_reason'] = str(error)
    result["duration_seconds"] = time.monotonic() - started
    print("VERANDA_GTK_INPUT " + json.dumps(result), flush=True)
    return result["original_exit_code"]


if __name__ == "__main__":
    raise SystemExit(main())
