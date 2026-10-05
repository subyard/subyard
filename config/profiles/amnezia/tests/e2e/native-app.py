#!/usr/bin/env python3
"""Drive the unmodified AmneziaVPN 5.0.3.0 GUI on leased VM2.

install
setup --host IPV4 --ssh-port PORT --user USER --key-file FILE --udp-port PORT
share --name NAME --output-file FILE
revoke --name NAME
discover
close
self-check

The controller interleaves these actions with independent traffic probes. This
helper never implements installation, peer generation or revocation on the server.
Upstream reference: github.com/amnezia-vpn/amnezia-client/tree/5.0.3.0
"""

import argparse
import collections
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import re
import signal
import stat
import subprocess
import sys
import tempfile
import time
import urllib.request


ROOT = Path('/var/tmp/subyard-amnezia-native-app')
MARKER = 'subyard-amnezia-native-app-v1'
VERSION = '5.0.3.0'
ASSET = 'AmneziaVPN_5.0.3.0_linux_x64.run'
URL = f'https://github.com/amnezia-vpn/amnezia-client/releases/download/{VERSION}/{ASSET}'
# GitHub release asset metadata, checked 2026-10-05; never execute an unchecked asset.
DIGEST = '0335f2643f58c4d7494be4c6d47582574fa7e5a463450e9a47b0c5c2eda797c2'
LAUNCHER = '/opt/AmneziaVPN/bin/AmneziaVPN'
GUI_BINARY = '/opt/AmneziaVPN/client/AmneziaVPN'
SESSION = ROOT / 'session.json'


class Failure(Exception):
    pass


def require(condition, message):
    if not condition:
        raise Failure(message)


def private_dir(path, create=False):
    if create and not path.exists():
        path.mkdir(mode=0o700)
    info = path.lstat()
    require(stat.S_ISDIR(info.st_mode) and info.st_uid == 0
            and stat.S_IMODE(info.st_mode) == 0o700,
            'fixture directories must be root-owned mode 0700, without symlinks')


def private_file(path):
    private_dir(path.parent)
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(fd, 'rb') as stream:
        info = os.fstat(stream.fileno())
        require(stat.S_ISREG(info.st_mode) and info.st_uid == 0
                and stat.S_IMODE(info.st_mode) == 0o600,
                'input files must be root-owned regular files with mode 0600')
        data = stream.read(262145)
    require(0 < len(data) <= 262144, 'input file is empty or too large')
    return data


def write_private(path, data):
    require(not path.is_symlink(), 'refusing a symlink in private fixture state')
    fd, name = tempfile.mkstemp(dir=path.parent, prefix='.native-')
    try:
        with os.fdopen(fd, 'wb') as stream:
            os.fchmod(stream.fileno(), 0o600)
            stream.write(data)
        os.replace(name, path)
    finally:
        if os.path.exists(name):
            os.unlink(name)


def guard():
    lease = Path('/run/subyard-e2e-lease.json')
    require(os.geteuid() == 0 and os.environ.get('SUBYARD_E2E_VM') == '2'
            and lease.is_file() and not lease.is_symlink() and lease.stat().st_uid == 0,
            'run only as root on allocated VM2')
    if ROOT.exists() or ROOT.is_symlink():
        private_dir(ROOT)
        require(private_file(ROOT / '.marker').decode().strip() == MARKER,
                'refusing a foreign native-app fixture directory')
    else:
        ROOT.mkdir(mode=0o700)
        write_private(ROOT / '.marker', (MARKER + '\n').encode())
    # Native exports are additionally chmod/validated before use; inherited umask
    # only protects their transient creation, never substitutes for exact modes.
    os.umask(0o077)


def run(command, timeout=30, capture=False, env=None, input_data=None):
    try:
        result = subprocess.run(command, input=input_data, timeout=timeout, env=env,
                                stdout=subprocess.PIPE if capture else subprocess.DEVNULL,
                                stderr=subprocess.DEVNULL, check=False)
    except (OSError, subprocess.TimeoutExpired):
        raise Failure('external command unavailable or timed out') from None
    require(result.returncode == 0, 'external command failed')
    return result.stdout if capture else None


def install():
    if (ROOT / '.installed').exists():
        require(private_file(ROOT / '.installed').decode().strip() == VERSION
                and Path(LAUNCHER).is_file(), 'owned application installation is incomplete')
        return
    require(not Path('/opt/AmneziaVPN').exists(), 'refusing to replace an unowned application installation')
    run(['apt-get', 'update', '-qq'], timeout=180)
    run(['apt-get', 'install', '-y', '-qq', 'xvfb', 'xdotool', 'xclip', 'dbus-x11',
         'at-spi2-core', 'python3-pyatspi', 'libxcb-cursor0', 'libxcb-xinerama0',
         'libxcb-icccm4', 'libxcb-keysyms1', 'libopengl0', 'libxkbcommon-x11-0'], timeout=300)
    asset = ROOT / ASSET
    digest = hashlib.sha256()
    try:
        require(not asset.is_symlink(), 'refusing a symlink for the native installer')
        fd = os.open(asset, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW, 0o600)
        with os.fdopen(fd, 'wb') as output, urllib.request.urlopen(URL, timeout=60) as response:
            os.fchmod(output.fileno(), 0o600)
            while chunk := response.read(1048576):
                digest.update(chunk)
                output.write(chunk)
    except (OSError, ValueError):
        raise Failure('official application download failed') from None
    require(digest.hexdigest() == DIGEST, 'official application asset digest does not match')
    asset.chmod(0o700)
    env = dict(os.environ, QT_QPA_PLATFORM='offscreen', LANG='C.UTF-8', LC_ALL='C.UTF-8')
    help_text = run([str(asset), '--help'], capture=True, env=env).decode(errors='replace')
    require('--confirm-command' in help_text and '--accept-licenses' in help_text,
            'official installer does not support the expected native CLI')
    run([str(asset), '--accept-licenses', '--default-answer', '--confirm-command', 'install'],
        timeout=300, env=env)
    require(Path(LAUNCHER).is_file(), 'native installer did not create the application launcher')
    # The official installer starts its GUI after installation. It inherited the
    # offscreen platform; close exactly that owned executable before Xvfb launch.
    for process in Path('/proc').iterdir():
        if process.name.isdigit():
            try:
                if os.readlink(process / 'exe') == GUI_BINARY:
                    os.kill(int(process.name), signal.SIGTERM)
            except (OSError, ProcessLookupError):
                pass
    write_private(ROOT / '.installed', (VERSION + '\n').encode())


def process_identity(pid):
    try:
        return Path(f'/proc/{pid}/stat').read_text().rsplit(')', 1)[1].split()[19]
    except (OSError, IndexError):
        return None


def spawn(command, env):
    child = subprocess.Popen(command, env=env, stdin=subprocess.DEVNULL,
                             stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                             start_new_session=True)
    identity = process_identity(child.pid)
    require(identity is not None, 'GUI session process did not start')
    return {'pid': child.pid, 'start': identity, 'group': True}


def stop_process(record):
    if record and process_identity(record['pid']) == record['start']:
        try:
            if record.get('group'):
                os.killpg(record['pid'], signal.SIGTERM)
            else:
                os.kill(record['pid'], signal.SIGTERM)
        except ProcessLookupError:
            pass
        deadline = time.monotonic() + 5
        while process_identity(record['pid']) == record['start'] and time.monotonic() < deadline:
            time.sleep(0.1)
        if process_identity(record['pid']) == record['start']:
            try:
                if record.get('group'):
                    os.killpg(record['pid'], signal.SIGKILL)
                else:
                    os.kill(record['pid'], signal.SIGKILL)
            except ProcessLookupError:
                pass


def save_session(state):
    write_private(SESSION, json.dumps(state).encode())


def session(reopen=False):
    require((ROOT / '.installed').exists(), 'run install before native GUI actions')
    if SESSION.exists():
        state = json.loads(private_file(SESSION))
        require(all(process_identity(state[key]['pid']) == state[key]['start']
                    for key in ('xvfb', 'dbus')), 'saved GUI session is unavailable; run close then retry')
        os.environ.update(state['environment'])
        if reopen:
            stop_process(state.get('app'))
            time.sleep(1)
        if reopen or process_identity(state['app']['pid']) != state['app']['start']:
            state['app'] = spawn([LAUNCHER], dict(os.environ))
            save_session(state)
        return state
    display = next((f':{number}' for number in range(90, 110)
                    if not Path(f'/tmp/.X11-unix/X{number}').exists()
                    and not Path(f'/tmp/.X{number}-lock').exists()), None)
    require(display is not None, 'no unused fixture X display is available')
    env = dict(os.environ, DISPLAY=display, LANG='C.UTF-8', LC_ALL='C.UTF-8',
               QT_QUICK_BACKEND='software', QT_LINUX_ACCESSIBILITY_ALWAYS_ON='1',
               XDG_CONFIG_HOME=str(ROOT / 'config'), XDG_DATA_HOME=str(ROOT / 'data'),
               XDG_CACHE_HOME=str(ROOT / 'cache'), XDG_RUNTIME_DIR=str(ROOT / 'run'))
    for name in ('config', 'data', 'cache', 'run'):
        private_dir(ROOT / name, create=True)
    state = {}
    try:
        state['xvfb'] = spawn(['Xvfb', display, '-screen', '0', '1024x900x24', '-nolisten', 'tcp'], env)
        deadline = time.monotonic() + 10
        while not Path(f'/tmp/.X11-unix/X{display[1:]}').exists():
            require(time.monotonic() < deadline, 'fixture X display did not become ready')
            time.sleep(0.1)
        bus = run(['dbus-daemon', '--session', '--fork', '--print-address=1', '--print-pid=1'],
                  capture=True, env=env).decode().splitlines()
        require(len(bus) == 2 and bus[1].isdigit(), 'fixture session bus did not start')
        pid = int(bus[1])
        state['dbus'] = {'pid': pid, 'start': process_identity(pid), 'group': False}
        env['DBUS_SESSION_BUS_ADDRESS'] = bus[0]
        state['environment'] = {key: env[key] for key in (
            'DISPLAY', 'LANG', 'LC_ALL', 'QT_QUICK_BACKEND', 'QT_LINUX_ACCESSIBILITY_ALWAYS_ON',
            'XDG_CONFIG_HOME', 'XDG_DATA_HOME', 'XDG_CACHE_HOME', 'XDG_RUNTIME_DIR', 'DBUS_SESSION_BUS_ADDRESS')}
        os.environ.update(state['environment'])
        state['app'] = spawn([LAUNCHER], env)
        save_session(state)
        return state
    except Exception:
        for key in ('app', 'dbus', 'xvfb'):
            stop_process(state.get(key))
        raise


def close():
    if SESSION.exists():
        state = json.loads(private_file(SESSION))
        for key in ('app', 'dbus', 'xvfb'):
            stop_process(state.get(key))
        SESSION.unlink()


class GUI:
    def __init__(self):
        import pyatspi
        self.api = pyatspi
        self.wait(lambda: self.nodes(), 'application accessibility registration', 45)

    def nodes(self):
        result = []
        desktop = self.api.Registry.getDesktop(0)
        for application in desktop:
            if 'amnezia' not in application.name.lower():
                continue
            pending = [application]
            while pending and len(result) < 3000:
                node = pending.pop()
                try:
                    state = node.getState()
                    if state.contains(self.api.STATE_SHOWING) and not state.contains(self.api.STATE_DEFUNCT):
                        result.append(node)
                    pending.extend(reversed(list(node)))
                except Exception:
                    continue
        return result

    def wait(self, action, description, seconds=30):
        deadline = time.monotonic() + seconds
        while time.monotonic() < deadline:
            try:
                result = action()
                if result:
                    return result
            except Exception:
                pass
            time.sleep(0.25)
        raise Failure(f'GUI timed out waiting for {description}')

    def matching(self, names):
        if isinstance(names, str):
            names = (names,)
        return [node for node in self.nodes() if node.name.strip() in names]

    def click_node(self, node):
        try:
            actions = node.queryAction()
            for index in range(actions.nActions):
                if actions.getName(index) in ('click', 'press', 'activate'):
                    if actions.doAction(index):
                        return
        except Exception:
            pass
        rectangle = node.queryComponent().getExtents(self.api.DESKTOP_COORDS)
        require(rectangle.width > 0 and rectangle.height > 0, 'GUI target has no visible geometry')
        run(['xdotool', 'mousemove', '--sync', str(rectangle.x + rectangle.width // 2),
             str(rectangle.y + rectangle.height // 2), 'click', '1'])

    def click(self, names, seconds=30):
        nodes = self.wait(lambda: self.matching(names), 'a named control', seconds)
        # Qt labels inside custom clickable rows also work through their geometry.
        self.click_node(nodes[0])
        time.sleep(0.35)

    def fields(self):
        fields = []
        for node in self.nodes():
            try:
                if node.getState().contains(self.api.STATE_EDITABLE):
                    node.queryEditableText()
                    rectangle = node.queryComponent().getExtents(self.api.DESKTOP_COORDS)
                    fields.append((rectangle.y, rectangle.x, node))
            except Exception:
                pass
        return [item[2] for item in sorted(fields, key=lambda item: item[:2])]

    def fill(self, values):
        fields = self.wait(lambda: self.fields() if len(self.fields()) == len(values) else None,
                           'the expected input fields')
        for node, value in zip(fields, values):
            self.fill_node(node, value)

    def fill_node(self, node, value):
        try:
            require(node.queryEditableText().setTextContents(value), 'GUI input was rejected')
        except Exception:
            # The private key stays on stdin to xclip, never in argv/env.
            self.click_node(node)
            run(['xdotool', 'key', 'ctrl+a'])
            run(['xclip', '-selection', 'clipboard'], input_data=value.encode())
            try:
                run(['xdotool', 'key', 'ctrl+v'])
                time.sleep(0.2)
            finally:
                run(['xclip', '-selection', 'clipboard'], input_data=b'')

    def key(self, value):
        run(['xdotool', 'key', value])
        time.sleep(0.25)

    def tab(self, index):
        frames = self.wait(lambda: [node for node in self.nodes()
                                   if node.getRoleName() in ('frame', 'window')], 'main application window')
        frame = frames[0].queryComponent().getExtents(self.api.DESKTOP_COORDS)
        # PageStart.qml 319-428: four icon-only tabs with 96 px side padding.
        x = frame.x + 96 + int((frame.width - 192) * (index + 0.5) / 4)
        y = frame.y + frame.height - 32
        run(['xdotool', 'mousemove', '--sync', str(x), str(y), 'click', '1'])
        time.sleep(0.5)

    def diagnostics(self):
        # Never dump accessible names/values: they can contain credentials/configs.
        return dict(collections.Counter(node.getRoleName() for node in self.nodes()))


def setup(gui, args):
    key = private_file(args.key_file).decode()
    require('PRIVATE KEY-----' in key and key.startswith('-----BEGIN '), 'administrative key is not a private key')
    if gui.matching("Let's get started"):
        gui.click("Let's get started")
    else:
        gui.tab(3)
    gui.click('Self-hosted VPN')
    # PageSetupWizardCredentials.qml: hostname, username, secret, in that order.
    gui.fill([f'{args.host}:{args.ssh_port}', args.user, key])
    del key
    gui.click(('Continue', 'Start configuring'))
    gui.click('Manual', seconds=90)
    gui.click('Continue')
    gui.click('AmneziaWG')
    gui.fill([str(args.udp_port)])
    gui.click('Install')
    gui.wait(lambda: gui.matching('Cancel installation'), 'native installer startup')
    gui.wait(lambda: not gui.matching('Cancel installation'), 'native installation completion', 360)
    # Share tab only becomes usable for a full-access native server connection.
    gui.tab(1)
    gui.wait(lambda: gui.matching('Share VPN Access'), 'native administration connection')


def share(gui, args):
    output = args.output_file.absolute()
    private_dir(output.parent)
    require(not output.exists() and not output.is_symlink(), 'refusing to overwrite a client export')
    require(output.suffix == '.conf', 'native AWG export needs a .conf filename')
    gui.tab(1)
    gui.click('Connection')
    gui.fill([args.name])
    gui.click('For the AmneziaVPN app')
    gui.click('AmneziaWG native format')
    gui.click('Share')
    gui.wait(lambda: gui.matching('Show connection settings'), 'native client generation', 90)
    gui.click('Share')
    # Native QFileDialog keyboard navigation works without copying its contents.
    gui.wait(lambda: gui.matching(('Save', 'Save AmneziaWG config')), 'native export dialog')
    def filename_field():
        for field in gui.fields():
            text = field.queryText().getText(0, -1)
            if 'amnezia_for_awg' in text:
                return field
        return None
    field = gui.wait(filename_field, 'native export filename input')
    gui.fill_node(field, str(output))
    gui.click('Save')
    gui.wait(lambda: output.is_file() and output.stat().st_size > 0, 'native client export', 30)
    require(not output.is_symlink(), 'native export unexpectedly became a symlink')
    output.chmod(0o600)
    data = private_file(output)
    require(b'[Interface]' in data and b'[Peer]' in data and b'Endpoint = ' in data,
            'native AWG export is incomplete')
    gui.tab(1)


def revoke(gui, args):
    gui.tab(1)
    gui.click('Users')
    gui.click(args.name, seconds=90)
    gui.click('Revoke')
    gui.click('Continue')
    gui.wait(lambda: gui.matching('Config revoked'), 'native revocation completion', 90)
    gui.wait(lambda: not gui.matching(args.name), 'revoked user removal')


def discover(gui, args):
    gui.tab(2)
    gui.click('Servers')
    # The fixture owns exactly one admin server. No client import adds another.
    gui.wait(lambda: gui.matching('Server 1'), 'saved native administration server')
    gui.click('Server 1')
    gui.click('Management')
    gui.click('Check the server for previously installed Amnezia services')
    gui.wait(lambda: gui.matching(('All installed containers have been added to the application',
                                 'No new installed containers found')), 'native server scan', 120)
    gui.tab(1)
    gui.click('Users')
    gui.wait(lambda: gui.matching(args.name), 'retained native user after discovery', 90)


def port(value):
    value = int(value)
    if not 1 <= value <= 65535:
        raise argparse.ArgumentTypeError('port must be 1..65535')
    return value


def name(value):
    if not re.fullmatch(r'[a-zA-Z0-9_-]{1,20}', value):
        raise argparse.ArgumentTypeError('name must contain 1..20 ASCII letters, digits, underscores or hyphens')
    return value


def self_check():
    from unittest import mock
    require(port('1') == 1 and port('65535') == 65535, 'valid port parsing failed')
    require(name('acceptance-two') == 'acceptance-two', 'valid fixture name parsing failed')
    for parser, value in ((port, '0'), (port, '65536'), (name, '../key'), (name, 'a' * 21)):
        try:
            parser(value)
        except (ValueError, argparse.ArgumentTypeError):
            pass
        else:
            raise Failure('invalid fixture input was accepted')
    # An unmarked/non-VM2 caller must fail before making fixture directories.
    for vm in ('', '1', '2'):
        with mock.patch.dict(os.environ, {'SUBYARD_E2E_VM': vm}), \
                mock.patch('os.geteuid', return_value=0), \
                mock.patch.object(Path, 'is_file', return_value=False), \
                mock.patch.object(Path, 'mkdir') as mkdir:
            try:
                guard()
            except Failure:
                pass
            else:
                raise Failure('unleased invocation was accepted')
            require(not mkdir.called, 'unleased invocation created fixture state')


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    modes = parser.add_subparsers(dest='mode', required=True)
    for mode in ('install', 'close', 'self-check'):
        modes.add_parser(mode)
    command = modes.add_parser('discover')
    command.add_argument('--name', type=name, default='acceptance-two')
    command = modes.add_parser('setup')
    command.add_argument('--host', type=lambda value: str(ipaddress.IPv4Address(value)), required=True)
    command.add_argument('--ssh-port', type=port, required=True)
    command.add_argument('--user', type=name, required=True)
    command.add_argument('--key-file', type=Path, required=True)
    command.add_argument('--udp-port', type=port, required=True)
    command = modes.add_parser('share')
    command.add_argument('--name', type=name, required=True)
    command.add_argument('--output-file', type=Path, required=True)
    command = modes.add_parser('revoke')
    command.add_argument('--name', type=name, required=True)
    args = parser.parse_args()
    gui = None
    try:
        if args.mode == 'self-check':
            self_check()
        else:
            guard()
        if args.mode == 'self-check':
            pass
        elif args.mode == 'install':
            install()
        elif args.mode == 'close':
            close()
        else:
            session(reopen=args.mode == 'discover')
            gui = GUI()
            if args.mode == 'setup':
                setup(gui, args)
            elif args.mode == 'share':
                share(gui, args)
            elif args.mode == 'revoke':
                revoke(gui, args)
            else:
                discover(gui, args)
        print(f'native_app_mode={args.mode} result=pass')
        return 0
    except Exception as error:
        message = str(error) if isinstance(error, Failure) else type(error).__name__
        print(f'native_app_mode={args.mode} result=fail reason={message}', file=sys.stderr)
        if gui:
            try:
                print('native_app_visible_role_counts=' + json.dumps(gui.diagnostics(), sort_keys=True), file=sys.stderr)
            except Exception:
                pass
        return 1


if __name__ == '__main__':
    sys.exit(main())
