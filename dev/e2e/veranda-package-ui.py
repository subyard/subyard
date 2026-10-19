#!/usr/bin/env python3
"""Actual GTK accessibility readiness and natural close inside the package VM fixture."""
import importlib.util
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time


def gtk_ready(items, atspi, *, configured=False, deadline=None, diagnostics=None):
    """A controllable accessibility boundary, never a paint timestamp."""
    visible = []
    for node in items:
        if deadline is not None and time.monotonic() >= deadline:
            raise TimeoutError('gtk_readiness_timeout')
        states = node.get_state_set()
        if states.contains(atspi.StateType.SHOWING):
            visible.append((node.get_role(), node.get_name(),
                            states.contains(atspi.StateType.SENSITIVE), states.contains(atspi.StateType.SELECTED),
                            states.contains(atspi.StateType.ENABLED)))
    labels = [name for role, name, _, _, _ in visible if role == atspi.Role.LABEL]
    buttons = [name for role, name, sensitive, _, _ in visible
               if role == atspi.Role.PUSH_BUTTON and sensitive]
    # GTK4 emits SENSITIVE for non-disabled widgets, independently of raw ENABLED.
    raw_enabled = [name for role, name, _, _, enabled in visible if role == atspi.Role.PUSH_BUTTON and enabled]
    summaries = [name for name in labels if re.fullmatch(r'Yard state: NOT_CREATED\n10 projects\nObserved: [^\n]+', name or '')]
    if diagnostics is not None:
        diagnostics['last_readiness'] = {'complete': True, 'tree_nodes': len(items), 'visible_nodes': len(visible),
            'enabled_refresh': raw_enabled.count('Refresh'), 'enabled_connect': raw_enabled.count('Connect remote host'),
            'sensitive_refresh': buttons.count('Refresh'), 'sensitive_connect': buttons.count('Connect remote host'),
            'connected_labels': labels.count('Connected'), 'observed_labels': sum('Observed:' in (name or '') for name in labels),
            'default_summaries': len(summaries)}
    if buttons.count('Refresh') != 1:
        return None
    if configured:
        expected = {'Local owner / default', *('Local owner / fleet-' + str(index).zfill(2) for index in range(1, 20))}
        all_rows = [(name, selected) for role, name, _, selected, _ in visible if role == atspi.Role.LIST_ITEM]
        rows = [name for name, _ in all_rows if name.startswith('Local owner / ')]
        selected = [name for name, selected in all_rows if selected]
        if (labels.count('Connected') == 1 and labels.count('Local owner / default') == 1
                and len(summaries) == 1 and len(rows) == 20 and set(rows) == expected
                and len(all_rows) == 21
                and selected == ['Local owner / default']):
            return {'kind': 'gtk-controllable-configured-overview', 'visible_yards': 20,
                    'default_project_summary': 10, 'project_name_widgets_verified': False}
        return None  # A healthy configured owner cannot pass via an error screen.
    if buttons.count('Connect remote host') != 1:
        return None
    if 'Connected' in labels and any('Observed:' in name for name in labels):
        return {'kind': 'gtk-controllable-fleet'}
    if any(re.match(r'^[a-z][a-z0-9_]*: \S', name or '') for name in labels):
        return {'kind': 'gtk-controllable-actionable-owner-error'}
    return None


class GtkObserver:
    """Read only: exact process identity and fresh, complete owned trees."""
    def __init__(self, pid, *, executable=None):
        self.pid = pid
        self.process = Path('/proc') / str(pid)
        self.identity = self.current_identity()
        assert self.identity[0] == os.getuid(), 'foreign_gui_uid'
        if executable is not None:
            assert self.process.joinpath('exe').resolve() == Path(executable).resolve(), 'unexpected_gui_executable'
        import gi
        gi.require_version('Atspi', '2.0')
        from gi.repository import Atspi, Gio, GLib
        self.atspi, self.context, self.api_error = Atspi, GLib.MainContext.default(), GLib.Error
        connection = Gio.DBusConnection.new_for_address_sync(os.environ['DBUS_SESSION_BUS_ADDRESS'],
            Gio.DBusConnectionFlags.AUTHENTICATION_CLIENT | Gio.DBusConnectionFlags.MESSAGE_BUS_CONNECTION, None, None)
        connection.call_sync('org.a11y.Bus', '/org/a11y/bus', 'org.freedesktop.DBus.Properties', 'Set',
            GLib.Variant('(ssv)', ('org.a11y.Status', 'IsEnabled', GLib.Variant('b', True))),
            None, Gio.DBusCallFlags.NONE, 5000, None)
        Atspi.set_timeout(250, 500)
        Atspi.init()
        spec = importlib.util.spec_from_file_location('veranda_owned_tree', Path(__file__).resolve().parents[2] / 'veranda/desktop/wayland-input.py')
        self.driver = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(self.driver)
        self.diagnostics = {'incomplete_tree_retries': 0, 'observation_seconds': 0, 'observations': 0}

    def current_identity(self):
        fields = self.process.joinpath('stat').read_text().rsplit(')', 1)[1].split()
        assert fields[0] not in ('Z', 'X'), 'owned_gui_terminated'
        executable = self.process.joinpath('exe').stat()
        return self.process.stat().st_uid, int(fields[19]), executable.st_dev, executable.st_ino

    def owned(self):
        assert self.current_identity() == self.identity, 'owned_gui_identity_changed'

    def read(self, deadline):
        def walk(until):
            self.owned()
            for _ in range(64):
                if not self.context.pending(): break
                self.context.iteration(False)
            desktop = self.atspi.get_desktop(0)
            desktop.clear_cache()
            count = desktop.get_child_count()
            if count < 0: raise self.driver.IncompleteTree()
            assert count <= 512, 'accessible_child_limit_exceeded'
            apps = []
            for index in range(count):
                if time.monotonic() >= until: raise TimeoutError('accessible_tree_unavailable')
                app = desktop.get_child_at_index(index)
                if app is None: raise self.driver.IncompleteTree()
                if app.get_process_id() == self.pid: apps.append(app)
            assert len(apps) <= 1, 'duplicate_owned_accessible_application'
            self.diagnostics.update(desktop_applications=count, owned_applications=len(apps))
            if not apps: raise self.driver.IncompleteTree()
            apps[0].clear_cache()
            return self.driver.owned_tree(apps[0], self.pid, self.atspi.StateType.DEFUNCT, until)
        started = time.monotonic()
        try:
            items = self.driver.complete_tree(walk, deadline, self.diagnostics)
            self.owned()
            return items
        finally:
            self.diagnostics['observation_seconds'] += time.monotonic() - started
            self.diagnostics['observations'] += 1

    def ready(self, deadline, *, configured=False):
        started = time.monotonic()
        try:
            # Getter failures invalidate the complete observation, never its fragments.
            result = self.driver.coherent_observation(self.read,
                lambda items: gtk_ready(items, self.atspi, configured=configured, deadline=deadline, diagnostics=self.diagnostics),
                deadline, self.diagnostics, self.api_error, 'resource-readiness')
            if time.monotonic() >= deadline: raise TimeoutError('gtk_readiness_timeout')
            return result
        finally:
            self.diagnostics['readiness_probe_seconds'] = self.diagnostics.get('readiness_probe_seconds', 0) + time.monotonic() - started


def package_diagnostic(phase, observer=None, error=None):
    try:
        diagnostics = observer.diagnostics if observer is not None else {}
        result = {'phase': phase, 'failure_type': type(error).__name__ if error is not None else None}
        for key in ('desktop_applications', 'owned_applications', 'observations', 'incomplete_tree_retries', 'read_error_count'):
            value = diagnostics.get(key)
            result[key] = value if type(value) is int and 0 <= value <= 1_000_000 else None
        cached = diagnostics.get('last_readiness', {})
        result['last_complete_readiness'] = {key: value for key, value in cached.items()
            if key in ('complete', 'tree_nodes', 'visible_nodes', 'enabled_refresh', 'enabled_connect',
                       'connected_labels', 'observed_labels', 'default_summaries', 'sensitive_refresh', 'sensitive_connect')
            and (type(value) is bool or type(value) is int and 0 <= value <= 2048)}
        print('VERANDA_PACKAGE_UI_DIAGNOSTIC ' + json.dumps(result), file=sys.stderr, flush=True)
    except BaseException:
        pass  # Diagnostics cannot replace the readiness/physical-input failure.


def wayland_close(observer, window):
    """Physical close only after owned-process and singleton state acknowledgement."""
    observer.owned()
    window.set('visible')
    observer.owned()
    acknowledgement = window.check('visible')
    observer.owned()
    subprocess.run(['wtype', '-s', '100', '-M', 'alt', '-k', 'F4', '-m', 'alt', '-s', '100'], check=True, timeout=3)
    return acknowledgement


def main():
    if os.environ.get('SUBYARD_E2E_VM') != '1' or os.getuid() == 0:
        raise SystemExit('requires an allocated unprivileged VM')
    observer = None
    phase = 'arguments'
    try:
        assert len(sys.argv) in (2, 3) and (len(sys.argv) == 2 or sys.argv[2] == '--wayland'), 'unexpected_arguments'
        pid = int(sys.argv[1])
        wayland = len(sys.argv) == 3
        phase = 'observer-init'
        package_diagnostic(phase)
        observer = GtkObserver(pid)
        phase = 'readiness'
        package_diagnostic(phase, observer)
        deadline = time.monotonic() + 20
        kind = None
        while time.monotonic() < deadline:
            kind = observer.ready(deadline)
            if kind: break
            time.sleep(.05)
        if not kind: raise RuntimeError('GTK readiness timeout')
        phase = 'window-identity'
        package_diagnostic(phase, observer)
        observer.owned()
        window_state = None
        if wayland:
            spec = importlib.util.spec_from_file_location('veranda_probe', Path(__file__).resolve().parents[2] / 'dev/measure-veranda.py')
            probe = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(probe)
            window = probe.WlrToplevel(os.environ['WAYLAND_DISPLAY'], 'org.subyard.Veranda')
            phase = 'window-activation-and-natural-close'
            package_diagnostic(phase, observer)
            window_state = wayland_close(observer, window)
        else:
            windows=subprocess.check_output(['xdotool','search','--onlyvisible','--pid',str(pid)],timeout=3).splitlines()
            assert len(windows)==1 and windows[0].isdigit()
            window=windows[0].decode()
            assert int(subprocess.check_output(['xdotool','getwindowpid',window],timeout=3))==pid
            phase = 'window-activation'
            package_diagnostic(phase, observer)
            subprocess.run(['xdotool','windowactivate','--sync',window],check=True,timeout=3)
            phase = 'natural-close-request'
            package_diagnostic(phase, observer)
            subprocess.run(['xdotool','key','--clearmodifiers','alt+F4'],check=True,timeout=3)
        print(json.dumps({'gtk_readiness':kind['kind'], 'natural_close_requested':True,
                          'backend':'wayland' if wayland else 'x11', 'gtk_renderer':'cairo',
                          'window_state':window_state}),flush=True)
        return 0
    except BaseException as error:
        package_diagnostic(phase, observer, error)
        return 1


if __name__=='__main__': sys.exit(main())
