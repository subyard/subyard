#!/usr/bin/env python3
"""Drive the unmodified AmneziaVPN 5.1.0.6 GUI on leased VM2.

install
setup --host IPV4 --ssh-port PORT --user USER --key-file FILE --udp-port PORT
share --name NAME --output-file FILE
revoke --name NAME
discover
close
self-check

The controller interleaves these actions with independent traffic probes. This
helper never implements installation, peer generation or revocation on the server.
Upstream reference: github.com/amnezia-vpn/amnezia-client/tree/5.1.0.6
"""

import argparse
import collections
import configparser
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
from types import SimpleNamespace
import urllib.request


ROOT = Path('/var/tmp/subyard-amnezia-native-app')
MARKER = 'subyard-amnezia-native-app-v1'
VERSION = '5.1.0.6'
ASSET = 'AmneziaVPN_5.1.0.6_linux_x64.run'
URL = f'https://github.com/amnezia-vpn/amnezia-client/releases/download/{VERSION}/{ASSET}'
# GitHub release asset metadata (tag commit ac175c8a), checked 2026-11-05; never execute an unchecked asset.
DIGEST = 'f60a4f373332dac8465bd9b8e4f233ee3aa32131fe124d31ca70bc499c73496f'
LAUNCHER = '/opt/AmneziaVPN/bin/AmneziaVPN'
GUI_BINARY = LAUNCHER
SESSION = ROOT / 'session.json'
SETTINGS = ROOT / 'config' / 'AmneziaVPN.ORG' / 'AmneziaVPN.conf'
FAILURE_EVIDENCE = ROOT / 'failure'
# PageUpdate.qml: every state of the button that only this page shows, except its generic Retry.
UPDATE_PAGE_BUTTONS = ('Update app', 'Downloading update...', 'Install update')
# PageStart.qml TabBarButtonType names. The bar is hidden while a page is pushed above its tab page.
MAIN_TABS = ('Home', 'Share', 'Settings', 'Add')
AWG_FORMAT = 'AmneziaWG native format'
SCAN_CONTROL = 'Check the server for previously installed Amnezia services'
UPDATE_PACKAGES = 'Update server packages'
# Evidence keeps only these fixture-known labels. Other accessible names can hold private values.
EVIDENCE_LABELS = frozenset(UPDATE_PAGE_BUTTONS + MAIN_TABS + (
    'AmneziaVPN', "Let's get started", 'Self-hosted VPN', 'Manual', 'AmneziaWG', 'Continue', 'Back',
    'Install', 'Cancel installation', 'Connect', 'Connection', 'Users', 'Revoke', 'Rename',
    'For the AmneziaVPN app', AWG_FORMAT, 'Copy', 'Show connection settings', 'Save', 'Save AmneziaWG config',
    'Servers', 'Protocols', 'Services', 'Management', SCAN_CONTROL, 'Close application', 'Close', 'Cancel',
    'Retry', UPDATE_PACKAGES, 'Reboot server', 'Reboot', 'Not now'))


class Failure(Exception):
    pass


class TimedOut(Failure):
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


def run(command, timeout=30, capture=False, env=None, input_data=None, operation='external'):
    require(operation in ('external', 'apt-update', 'apt-install', 'installer-help', 'installer-install',
                          'pointer-click', 'frame-capture', 'clipboard-write', 'clipboard-clear', 'keyboard-input'),
            'invalid external operation label')
    diagnostic = f'external command failed operation={operation}'
    try:
        result = subprocess.run(command, input=input_data, timeout=timeout, env=env,
                                stdout=subprocess.PIPE if capture else subprocess.DEVNULL,
                                stderr=subprocess.DEVNULL, check=False)
    except subprocess.TimeoutExpired:
        raise Failure(diagnostic + ' status=timeout') from None
    except OSError as error:
        number = error.errno
        code = str(number) if type(number) is int and 0 <= number <= 4095 else 'unknown'
        raise Failure(diagnostic + f' status=unavailable errno={code}') from None
    require(result.returncode == 0, diagnostic + f' status=exit exit_code={result.returncode}')
    return result.stdout if capture else None


def install():
    if (ROOT / '.installed').exists():
        require(private_file(ROOT / '.installed').decode().strip() == VERSION
                and Path(LAUNCHER).is_file(), 'owned application installation is incomplete')
        return
    require(not Path('/opt/AmneziaVPN').exists(), 'refusing to replace an unowned application installation')
    run(['apt-get', 'update', '-qq'], timeout=180, operation='apt-update')
    run(['apt-get', 'install', '-y', '-qq', 'xvfb', 'xdotool', 'xclip', 'dbus-x11',
         'at-spi2-core', 'python3-pyatspi', 'libxcb-cursor0', 'libxcb-xinerama0',
         'libxcb-icccm4', 'libxcb-keysyms1', 'libopengl0', 'libxkbcommon-x11-0',
         'libfontconfig1', 'libxcb-shape0', 'libegl1', 'imagemagick'], timeout=300,
        operation='apt-install')
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
    help_text = run([str(asset), '--help'], capture=True, env=env,
                    operation='installer-help').decode(errors='replace')
    require('--confirm-command' in help_text and '--accept-licenses' in help_text,
            'official installer does not support the expected native CLI')
    run([str(asset), '--accept-licenses', '--default-answer', '--confirm-command', 'install'],
        timeout=300, env=env, operation='installer-install')
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


def app_settings():
    require(not SETTINGS.is_symlink(), 'refusing a symlink for the stock app settings')
    settings = configparser.RawConfigParser(strict=False)
    settings.optionxform = str
    settings.read_string(SETTINGS.read_text())
    return settings


def require_update_check_off(started=False):
    # The stock app checks once at startup, then pushes PageUpdate over the current page.
    # Its own setting skips that request: "Check for updates automatically" in
    # PageSettingsApplication.qml, read by coreController.cpp checkForAppUpdates.
    # Upstream publication then cannot change these pages; no DNS or network is altered.
    if not started and not SETTINGS.exists():
        private_dir(SETTINGS.parent, create=True)
        write_private(SETTINGS, b'[Conf]\nautoUpdateCheck=false\n')
    settings = app_settings()
    require(settings.get('Conf', 'autoUpdateCheck', fallback=None) == 'false',
            'stock app settings do not disable its update check')
    # amneziaApplication.cpp writes permFixed at every start: it proves this is the file in use.
    require(not started or settings.get('General', 'permFixed', fallback=None) == 'true',
            'stock app is not using the settings file that disables its update check')


def saved_servers():
    # Saved servers hold administration credentials: only their count leaves here.
    try:
        value = app_settings().get('Servers', 'serversList', fallback=None)
        if value is None:
            return dict(state='no-server-list', count=0)
        # QSettings INI escaping around a QByteArray of JSON.
        value = value[1:-1] if value.startswith('"') and value.endswith('"') else value
        raw = value.encode('ascii').decode('unicode_escape').encode('latin-1')
        require(raw.startswith(b'@ByteArray(') and raw.endswith(b')'), 'unknown server list encoding')
        servers = json.loads(raw[11:-1])
        require(isinstance(servers, list) and all(isinstance(item, dict) for item in servers),
                'unknown server list structure')
        return dict(state='parsed', count=len(servers))
    except Exception:
        return dict(state='unknown', count=None)


def failure_evidence(gui, mode):
    # Small files for the controller's failure capture. A share page can show a client
    # configuration and an editable field can hold the SSH key, so neither is photographed.
    # Show connection settings marks the export page, wherever it is reached from.
    report = dict(mode=mode, saved_servers=saved_servers(), screenshot='omitted', controls=[])
    private_dir(FAILURE_EVIDENCE, create=True)
    nodes, frames, private = gui.nodes(), [], mode == 'share'
    # A node that was not read could be the private one.
    if not gui.walk_complete or len(nodes) > 500:
        private, report['screenshot'] = True, 'unreadable'
    for node in nodes[:500]:
        try:
            flags = node.getState()
            label = node.name.strip()
            private = private or flags.contains(gui.api.STATE_EDITABLE) or label == 'Show connection settings'
        except Exception:
            private, report['screenshot'] = True, 'unreadable'
            continue
        # Geometry decides nothing private. The application root has none on any ordinary page.
        try:
            rectangle = node.queryComponent().getExtents(gui.api.DESKTOP_COORDS)
            if node.getRole() in (gui.api.ROLE_FRAME, gui.api.ROLE_WINDOW):
                frames.append(rectangle)
            report['controls'].append(dict(
                role=node.getRoleName(), label=label if label in EVIDENCE_LABELS else 'other' if label else '',
                rectangle=[rectangle.x, rectangle.y, rectangle.width, rectangle.height],
                states=[name for name in ('enabled', 'checked', 'focused', 'editable')
                        if flags.contains(getattr(gui.api, 'STATE_' + name.upper()))]))
        except Exception:
            continue
    try:
        if not private and len(frames) == 1:
            frame = frames[0]
            image = run(['import', '-window', 'root', '-crop', f'{frame.width}x{frame.height}+{frame.x}+{frame.y}', 'png:-'],
                        capture=True, timeout=30, operation='frame-capture')
            if len(image) <= 2097152:
                write_private(FAILURE_EVIDENCE / 'screen.png', image)
                report['screenshot'] = 'screen.png'
    except Exception:
        report['screenshot'] = 'unavailable'
    write_private(FAILURE_EVIDENCE / 'controls.json', (json.dumps(report, sort_keys=True) + '\n').encode())
    return report


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
            require_update_check_off()
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
    require_update_check_off()
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


def dialog_of(node):
    # Qt exposes each QML Popup (SnackbarType, SheetType, LoadingOverlayType) as a dialog.
    parent = node.parent
    for _ in range(32):
        if parent is None or parent.getRoleName() == 'dialog':
            return parent
        parent = parent.parent
    return None


def outermost(nodes):
    # BasicCardType repeats its name on its chevron IconButtonType child, which
    # clicks the card: a match nested inside another match is the same control.
    def nested(node):
        try:
            parent = node.parent
            for _ in range(32):
                if parent is None:
                    return False
                if any(parent == other for other in nodes):
                    return True
                parent = parent.parent
        except Exception:
            pass
        return False
    return [node for node in nodes if not nested(node)]


def installation_evidence(nodes):
    # main2.qml shows PageController.showErrorMessage in a closable SnackbarType: its Close
    # button is accessible, while its plain Text message is not, so a code is read only if
    # an accessible text carries it. Read bounded text in memory, but return only numeric
    # codes and fixed UI flags.
    controls = ('Cancel installation', 'Connection', 'Users', 'Share')
    visible = set()
    errors = set()
    shown = False
    for node in nodes:
        try:
            label = node.name
            if isinstance(label, str) and label.strip() in controls:
                visible.add(label.strip())
            if node.getRoleName() == 'progress bar':
                visible.add('progress bar')
            if dialog_of(node) is None:
                continue
            if node.getRoleName() in ('push button', 'button') and isinstance(label, str) and label.strip() == 'Close':
                shown = True
            texts = [label] if isinstance(label, str) and len(label) <= 4096 else []
            if node.getRoleName() in ('text', 'label'):
                try:
                    text = node.queryText()
                    count = text.characterCount
                    if type(count) is int and 0 < count <= 4096:
                        texts.append(text.getText(0, count))
                except Exception:
                    pass
            for text in texts:
                if not isinstance(text, str) or len(text) > 4096:
                    continue
                match = re.match(r'^ErrorCode: ([1-9][0-9]{2,3})\.\s', text.strip())
                if match:
                    errors.add(int(match[1]))
        except Exception:
            continue
    return {'error_codes': sorted(errors)[:8], 'error_shown': shown or bool(errors),
            'visible_controls': sorted(visible)}


def notification_shown(nodes):
    # A SnackbarType notification has no accessible child: its message is a plain Text item
    # and its action button stays hidden. Busy overlays, sheets and errors expose children.
    dialogs, occupied = [], []
    for node in nodes:
        try:
            if node.getRoleName() == 'dialog':
                dialogs.append(node)
            else:
                parent = dialog_of(node)
                if parent is not None:
                    occupied.append(parent)
        except Exception:
            continue
    return any(not any(dialog == other for other in occupied) for dialog in dialogs)


def wait_native_installation(gui, ready):
    def outcome():
        evidence = gui.installation_evidence()
        if evidence['error_shown']:
            return evidence['error_codes'] or ['unavailable']
        # PageSetupWizardInstalling.qml shows a progress bar: no click until it leaves.
        if 'progress bar' in evidence['visible_controls']:
            return None
        return ready() and 'ready'

    result = gui.wait(outcome, 'native installation completion', 360)
    require(result == 'ready', 'native installation failed ErrorCode=' + ','.join(str(code) for code in result))


class GUI:
    def __init__(self):
        import pyatspi
        self.api = pyatspi
        self.wait(lambda: self.nodes(), 'application accessibility registration', 45)

    def nodes(self):
        result = []
        self.walk_complete = True
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
                    self.walk_complete = False
        return result

    def owned_nodes(self):
        state = json.loads(private_file(SESSION))
        app = state['app']
        require(process_identity(app['pid']) == app['start'], 'owned application session is unavailable')
        nodes = []
        for node in self.nodes():
            try:
                if node.getApplication().get_process_id() == app['pid']:
                    nodes.append(node)
            except Exception:
                pass
        return nodes

    def refuse_update_page(self):
        try:
            present = any(node.name.strip() in UPDATE_PAGE_BUTTONS for node in self.nodes())
        except Exception:  # A node can vanish during a page transition; the next poll repeats this.
            return
        require(not present, 'stock app is showing its update page despite its disabled update check')

    def wait(self, action, description, seconds=30):
        deadline = time.monotonic() + seconds
        last_error = ''
        while time.monotonic() < deadline:
            self.refuse_update_page()
            try:
                result = action()
                if result:
                    return result
            except Exception as error:
                # Failure text is fixture-authored; any other exception can quote UI values.
                last_error = ': last error ' + (str(error) if isinstance(error, Failure) else type(error).__name__)
            time.sleep(0.25)
        raise TimedOut(f'GUI timed out waiting for {description}{last_error}')

    def roles(self, *names):
        return tuple(getattr(self.api, 'ROLE_' + name) for name in names)

    def matching(self, names, roles=None):
        # 5.1.0.6 controls carry accessible names: Accessible.name or their text.
        if isinstance(names, str):
            names = (names,)
        return outermost([node for node in self.nodes()
                          if node.name.strip() in names and (roles is None or node.getRole() in roles)])

    def unique(self, names, roles):
        found = self.matching(names, roles)
        return found if len(found) == 1 else None

    def showing(self, node):
        try:
            state = node.getState()
            return state.contains(self.api.STATE_SHOWING) and not state.contains(self.api.STATE_DEFUNCT)
        except Exception:
            return False

    def click_node(self, node):
        evidence = getattr(self, 'share_evidence', None)
        receipt = None
        if isinstance(evidence, dict):
            role = 'unknown'
            try:
                candidate = node.getRoleName()
                if candidate in ('button', 'push button', 'radio button', 'list item', 'page tab', 'text', 'label'):
                    role = candidate
            except Exception:
                pass
            receipt = {'role': role, 'method': 'pointer', 'dispatch_error': 'none',
                       'stage': evidence['stage'], 'candidate_count': evidence.get('candidate_count', 1)}
            evidence['action'] = receipt
        selected = None
        try:
            actions = node.queryAction()
            for index in range(actions.nActions):
                if actions.getName(index).casefold() in ('click', 'press', 'activate'):
                    selected = (actions, index)
                    break
        except Exception:
            pass
        if selected is not None:
            if receipt is not None:
                receipt['method'] = 'atspi'
            # Qt can replace the target before replying. Dispatch once; the
            # caller's next-screen or output postcondition verifies delivery.
            try:
                accepted = selected[0].doAction(selected[1])
                if receipt is not None and accepted is False:
                    receipt['dispatch_error'] = 'not-accepted'
            except Exception as error:
                if receipt is not None:
                    receipt['dispatch_error'] = 'timeout' if isinstance(error, TimeoutError) else 'action-exception'
                pass
            return
        try:
            rectangle = node.queryComponent().getExtents(self.api.DESKTOP_COORDS)
            require(rectangle.width > 0 and rectangle.height > 0, 'GUI target has no visible geometry')
            run(['xdotool', 'mousemove', '--sync', str(rectangle.x + rectangle.width // 2),
                 str(rectangle.y + rectangle.height // 2), 'click', '1'], operation='pointer-click')
        except Exception:
            if receipt is not None:
                receipt['dispatch_error'] = 'pointer-exception'
            raise

    def click(self, names, seconds=30, description='a named control', roles=None, unique=False):
        def target():
            nodes = self.matching(names, roles)
            # A page transition briefly shows both pages; unique waits for one of them.
            return nodes if nodes and (not unique or len(nodes) == 1) else None
        nodes = self.wait(target, description, seconds)
        evidence = getattr(self, 'share_evidence', None)
        if isinstance(evidence, dict):
            evidence['candidate_count'] = min(len(nodes), 3000)
        self.click_node(nodes[0])
        time.sleep(0.35)

    def tab(self, name):
        require(name in MAIN_TABS, 'unknown main tab')
        self.click(name, description=f'main {name} tab', roles=self.roles('PAGE_TAB'), unique=True)

    def back_to_tabs(self):
        # NavigationBarType's Back closes one pushed page. The fixture opens at most three.
        for _ in range(3):
            if self.unique('Settings', self.roles('PAGE_TAB')):
                return
            back = self.wait(lambda: self.unique('Back', self.roles('PUSH_BUTTON')), 'page back control')[0]
            self.click_node(back)
            self.wait(lambda: not self.showing(back), 'closed pushed page')
        self.wait(lambda: self.unique('Settings', self.roles('PAGE_TAB')), 'main tab bar')

    def click_saved_server(self):
        rows = []

        def target():
            rows.clear()
            nodes = self.owned_nodes()
            names = [node.name.strip() for node in nodes]
            # PageSettingsServersList: a NavigationBar Back and one ListItemType per saved server.
            # PageSettings stays showing during the push transition; its footer row marks it.
            if ('Close application' in names or names.count('Back') != 1 or self.fields()
                    or any(node.getRole() == self.api.ROLE_RADIO_BUTTON for node in nodes)):
                return None
            items = [node for node in nodes if node.getRole() == self.api.ROLE_LIST_ITEM
                     and node.getState().contains(self.api.STATE_ENABLED)]
            rows.append(len(items))
            return items[0] if len(items) == 1 else None

        try:
            row = self.wait(target, 'saved native administration server')
        except TimedOut as timeout:
            # The fixture owns exactly one saved server, so another count is a hard failure.
            raise Failure(f'Servers page must list one saved server: rows={rows[0]}' if rows else
                          f'Servers page was not reached after selecting Servers ({timeout})') from None
        self.click_node(row)
        self.wait(lambda: self.matching('Management', self.roles('PAGE_TAB')), 'native server management page')

    def fields(self):
        fields = []
        for node in self.nodes():
            try:
                if node.getState().contains(self.api.STATE_EDITABLE):
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
            run(['xdotool', 'key', 'ctrl+a'], operation='keyboard-input')
            run(['xclip', '-selection', 'clipboard'], input_data=value.encode(), operation='clipboard-write')
            try:
                run(['xdotool', 'key', 'ctrl+v'], operation='keyboard-input')
                time.sleep(0.2)
            finally:
                run(['xclip', '-selection', 'clipboard'], input_data=b'', operation='clipboard-clear')

    def key(self, value, deadline=None):
        remaining = 30 if deadline is None else deadline - time.monotonic()
        require(remaining > 0, 'native Management activation deadline expired')
        run(['xdotool', 'key', value], operation='keyboard-input', timeout=remaining)
        remaining = 0.25 if deadline is None else deadline - time.monotonic()
        require(remaining > 0, 'native Management activation deadline expired')
        time.sleep(min(0.25, remaining))

    def diagnostics(self):
        # Never dump accessible names/values: they can contain credentials/configs.
        return dict(collections.Counter(node.getRoleName() for node in self.nodes()))

    def installation_evidence(self):
        return installation_evidence(self.owned_nodes())


def setup(gui, args):
    key = private_file(args.key_file).decode()
    require('PRIVATE KEY-----' in key and key.startswith('-----BEGIN '), 'administrative key is not a private key')
    button, radio, item = gui.roles('PUSH_BUTTON'), gui.roles('RADIO_BUTTON'), gui.roles('LIST_ITEM')
    # The fixture's fresh app state always opens on PageSetupWizardStart.
    gui.click("Let's get started", description='setup welcome control', roles=button)
    # PageSetupWizardConfigSource.qml: BasicCardType carries its title as the accessible name.
    gui.click('Self-hosted VPN', description='self-hosted server card', roles=button, unique=True)
    gui.wait(lambda: len(gui.fields()) == 3, 'server credential fields')
    # PageSetupWizardCredentials.qml: hostname, username, secret, in that order.
    gui.fill([f'{args.host}:{args.ssh_port}', args.user, key])
    del key
    gui.click('Continue', description='server credential continuation', roles=button, unique=True)
    # Continue first checks the SSH connection, then opens PageSetupWizardEasy.
    gui.click('Manual', seconds=90, description='manual installation card', roles=radio, unique=True)

    def manual_selected():
        cards = gui.unique('Manual', radio)
        return cards and cards[0].getState().contains(gui.api.STATE_CHECKED)

    gui.wait(manual_selected, 'manual installation selection')
    gui.click('Continue', description='manual installation continuation', roles=button, unique=True)
    # PageSetupWizardProtocols.qml: one ListItemType per installable protocol; AmneziaWG is awg2.
    gui.click('AmneziaWG', description='AmneziaWG protocol control', roles=item, unique=True)
    gui.wait(lambda: len(gui.fields()) == 1 and gui.unique('Install', button), 'AmneziaWG port screen')
    gui.fill([str(args.udp_port)])
    gui.click('Install', description='native installation control', roles=button, unique=True)

    def ready():
        # Share only becomes usable for a full-access native server connection.
        controls = lambda: (gui.unique('Connection', radio) and gui.unique('Users', radio)
                            and gui.unique('Share', button))
        if controls():
            return True
        if gui.fields() or not gui.unique('Share', gui.roles('PAGE_TAB')):
            return False
        gui.tab('Share')
        return controls()
    wait_native_installation(gui, ready)


def export_controls(gui, location=False, accepting=False, evidence=None):
    def rejected(reason):
        if evidence is not None:
            evidence['predicate'] = reason
        return None

    def frame(node):
        for _ in range(100):
            if node is None:
                return None
            if node.getRole() in (gui.api.ROLE_FRAME, gui.api.ROLE_WINDOW):
                return node
            node = node.get_parent()
        return None

    def usable(node, enabled=True):
        state = node.getState()
        # Qt disables Save and clears its focusable state while editing the path.
        return state.contains(gui.api.STATE_SHOWING) and (not enabled or all(
            state.contains(value) for value in (gui.api.STATE_FOCUSABLE, gui.api.STATE_ENABLED)))

    buttons = [node for node in gui.nodes() if node.getRole() == gui.api.ROLE_PUSH_BUTTON
               and node.name.strip() in ('Save', 'Save AmneziaWG config') and usable(node, accepting)]
    if len(buttons) != 1:
        return rejected('save-candidate-count')
    button = buttons[0]
    dialog = frame(button)
    if dialog is None:
        return rejected('frame-unavailable')
    state = json.loads(private_file(SESSION))
    app = state['app']
    require(process_identity(app['pid']) == app['start']
            and dialog.getApplication().get_process_id() == app['pid']
            and os.environ.get('DISPLAY') == state['environment']['DISPLAY'],
            'client export requires the owned application dialog')
    fields = [node for node in gui.fields() if node.getRole() == gui.api.ROLE_TEXT
              and usable(node) and frame(node) == dialog
              and (not location or node.getState().contains(gui.api.STATE_FOCUSED))]
    if len(fields) != 1:
        return rejected('editable-candidate-count')
    field = fields[0]
    field.queryText()
    bounds = dialog.queryComponent().getExtents(gui.api.DESKTOP_COORDS)
    for node in (button, field):
        rectangle = node.queryComponent().getExtents(gui.api.DESKTOP_COORDS)
        x, y = rectangle.x + rectangle.width // 2, rectangle.y + rectangle.height // 2
        if (rectangle.width <= 0 or rectangle.height <= 0 or not 0 <= x < 1024 or not 0 <= y < 900
                or not bounds.x <= x < bounds.x + bounds.width
                or not bounds.y <= y < bounds.y + bounds.height):
            return rejected('geometry-outside-bounds')
    if evidence is not None:
        evidence['predicate'] = 'ready'
    return button, field


def click_export_share(gui):
    def target():
        evidence = {'raw_candidates': 0, 'eligible_candidates': 0, 'current_page': False}
        gui.share_evidence['export_target'] = evidence
        state = json.loads(private_file(SESSION))
        app = state['app']
        require(process_identity(app['pid']) == app['start']
                and os.environ.get('DISPLAY') == state['environment']['DISPLAY'],
                'client export requires the owned application session')
        nodes = []
        for node in gui.nodes()[:3000]:
            try:
                if node.getApplication().get_process_id() == app['pid']:
                    nodes.append(node)
            except Exception:
                pass
        frames = [node for node in nodes if node.getRole() in (gui.api.ROLE_FRAME, gui.api.ROLE_WINDOW)
                  and node.getState().contains(gui.api.STATE_SHOWING)
                  and not node.getState().contains(gui.api.STATE_DEFUNCT)]
        if len(frames) != 1:
            return None
        frame = frames[0]
        bounds = frame.queryComponent().getExtents(gui.api.DESKTOP_COORDS)
        if (bounds.x < 0 or bounds.y < 0 or bounds.width <= 0 or bounds.height <= 0
                or bounds.x + bounds.width > 1024 or bounds.y + bounds.height > 900):
            return None

        def eligible(node):
            state = node.getState()
            if (not all(state.contains(value) for value in (gui.api.STATE_SHOWING, gui.api.STATE_ENABLED))
                    or state.contains(gui.api.STATE_DEFUNCT)):
                return False
            parent = node
            for _ in range(100):
                if parent is None or parent == frame:
                    break
                parent = parent.get_parent()
            if parent != frame:
                return False
            rectangle = node.queryComponent().getExtents(gui.api.DESKTOP_COORDS)
            return (rectangle.width > 0 and rectangle.height > 0
                    and bounds.x <= rectangle.x and bounds.y <= rectangle.y
                    and rectangle.x + rectangle.width <= bounds.x + bounds.width
                    and rectangle.y + rectangle.height <= bounds.y + bounds.height)

        buttons = [node for node in nodes if node.getRole() == gui.api.ROLE_PUSH_BUTTON]
        shares = [node for node in buttons if node.name.strip() == 'Share']
        candidates = [node for node in shares if eligible(node)]
        landmarks = [node for node in buttons if node.name.strip() == 'Show connection settings' and eligible(node)]
        evidence.update(raw_candidates=len(shares), eligible_candidates=len(candidates), current_page=len(landmarks) == 1)
        if len(candidates) != 1 or len(landmarks) != 1:
            return None
        gui.share_evidence['candidate_count'] = 1
        return candidates[0]

    # PageStart pushes PageShareConnection with a transition; outgoing Share
    # nodes can remain accessible. Dispatch only the unique in-frame export control.
    gui.click_node(gui.wait(target, 'native export sharing control', 30))
    time.sleep(0.35)


def share_diagnostics(gui):
    # Keep only fixture-authored flags/counts; never expose node names or text.
    result = dict(getattr(gui, 'share_evidence', {}))
    result.update(owned_frames=0, owned_modals=0, owned_save_candidates=0,
                  owned_editable_candidates=0, error_codes=[], error_shown=None, predicate='unavailable')
    try:
        state = json.loads(private_file(SESSION))
        app = state['app']
        require(process_identity(app['pid']) == app['start'], 'owned application session is unavailable')
        for node in gui.nodes()[:3000]:
            try:
                if node.getApplication().get_process_id() != app['pid']:
                    continue
                role = node.getRoleName()
                result['owned_frames'] += int(role in ('frame', 'window'))
                result['owned_modals'] += int(role == 'dialog')
                result['owned_editable_candidates'] += int(node.getState().contains(gui.api.STATE_EDITABLE))
                result['owned_save_candidates'] += int(node.getRole() == gui.api.ROLE_PUSH_BUTTON
                    and node.name.strip() in ('Save', 'Save AmneziaWG config'))
            except Exception:
                continue
        installation = gui.installation_evidence()
        result['error_codes'], result['error_shown'] = installation['error_codes'], installation['error_shown']
        export_controls(gui, evidence=result)
    except Exception:
        result['predicate'] = 'ownership-or-access-unavailable'
    return result


def share(gui, args):
    gui.share_evidence = {'stage': 'prepare'}
    output = args.output_file.absolute()
    private_dir(output.parent)
    require(not output.exists() and not output.is_symlink(), 'refusing to overwrite a client export')
    require(output.suffix == '.conf', 'native AWG export needs a .conf filename')
    button, radio = gui.roles('PUSH_BUTTON'), gui.roles('RADIO_BUTTON')
    gui.share_evidence['stage'] = 'connection'
    gui.tab('Share')
    # PageShare.qml: SegmentedControlType exposes Connection and Users as radio buttons.
    gui.click('Connection', description='native share connection control', roles=radio, unique=True)
    gui.fill([args.name])
    # The Connection format DropDownType is a button named after its current value; its
    # RadioListType sheet names each format. A reselected Share tab starts from the default.
    if not gui.unique(AWG_FORMAT, button):
        gui.share_evidence['stage'] = 'format'
        gui.click('For the AmneziaVPN app', description='native export format selector', roles=button, unique=True)
        gui.click(AWG_FORMAT, description='native AWG export format', roles=radio, unique=True)
        gui.wait(lambda: gui.unique(AWG_FORMAT, button) and not gui.matching(AWG_FORMAT, radio),
                 'selected native AWG export format')
    gui.share_evidence['stage'] = 'generation'
    gui.click('Share', description='native client generation control', roles=button, unique=True)
    gui.wait(lambda: gui.matching('Show connection settings'), 'native client generation', 90)
    gui.share_evidence['stage'] = 'export-dialog'
    click_export_share(gui)
    # The native dialog's default basename is platform-dependent. Select its
    # editable filename field beside the actual accept button, not the title.
    gui.wait(lambda: export_controls(gui), 'native export dialog')
    gui.share_evidence['stage'] = 'directory'
    # Qt resolves filename text relative to currentFolder. Navigate with its
    # absolute-path breadcrumb before entering a basename in the filename field.
    gui.key('ctrl+l')
    _, directory = gui.wait(lambda: export_controls(gui, location=True), 'native export directory input')
    gui.fill_node(directory, str(output.parent))
    gui.key('Return')
    gui.share_evidence['stage'] = 'filename'
    button, field = gui.wait(lambda: export_controls(gui), 'native export filename input')
    gui.fill_node(field, output.name)
    # Accessible Press does not move focus; Qt commits selectedFile only when
    # filename editing finishes. Tab commits before the single accept action.
    gui.key('Tab')
    gui.wait(lambda: not field.getState().contains(gui.api.STATE_FOCUSED), 'native export filename commit')
    button, _ = gui.wait(lambda: export_controls(gui, accepting=True), 'native export save control')
    gui.share_evidence.update(stage='save', candidate_count=1)
    gui.click_node(button)
    gui.share_evidence['stage'] = 'file'
    gui.wait(lambda: output.is_file() and output.stat().st_size > 0, 'native client export', 30)
    require(not output.is_symlink(), 'native export unexpectedly became a symlink')
    output.chmod(0o600)
    data = private_file(output)
    require(b'[Interface]' in data and b'[Peer]' in data and b'Endpoint = ' in data,
            'native AWG export is incomplete')
    # PageShareConnection is pushed above PageShare, which hides the main tab bar.
    gui.share_evidence['stage'] = 'return'
    gui.back_to_tabs()


def revoke(gui, args):
    button, radio = gui.roles('PUSH_BUTTON'), gui.roles('RADIO_BUTTON')
    gui.tab('Share')
    gui.click('Users', description='native users control', roles=radio, unique=True)
    # PageShare.qml: each client is a ListItemType opening ShareClientInfoSheet.
    gui.click(args.name, seconds=90, description='native named user control', roles=gui.roles('LIST_ITEM'), unique=True)
    gui.click('Revoke', description='native revocation control', roles=button, unique=True)
    # QuestionDrawer.qml: the confirmation sheet's yes button.
    gui.click('Continue', description='native revocation confirmation', roles=button, unique=True)
    def removed():
        users = gui.unique('Users', radio)
        return (users and users[0].getState().contains(gui.api.STATE_CHECKED)
                and not any(gui.matching(label, button) for label in ('Revoke', 'Continue', 'Close'))
                and not gui.matching(args.name))
    # The success notification lasts three seconds; require the durable Users
    # page with the target row removed and its confirmation/details closed.
    gui.wait(removed, 'native revocation completion', 90)


def discovery_snapshot(gui, target=None):
    # Observe fixed flags only. Geometry, UI labels and private state never leave
    # this function, and observation failure never changes action delivery.
    flags = dict(owned=None, showing=None, enabled=None, in_frame=None, defunct=None, checked=None, selected=None,
                 focusable=None, focused=None)
    result = dict(session_owned=False, owned_frames=0, frame_eligible=False,
                  frame_active=None,
                  management_tabs=0, management_checked=None, management_selected=None,
                  error_codes=[], target=flags)
    try:
        state = json.loads(private_file(SESSION))
        app = state['app']
        if process_identity(app['pid']) != app['start'] or os.environ.get('DISPLAY') != state['environment']['DISPLAY']:
            return result
        result['session_owned'] = True
        nodes = []
        for node in gui.nodes()[:3000]:
            try:
                if node.getApplication().get_process_id() == app['pid']:
                    nodes.append(node)
            except Exception:
                pass
        frames = [node for node in nodes if node.getRole() in (gui.api.ROLE_FRAME, gui.api.ROLE_WINDOW)
                  and node.getState().contains(gui.api.STATE_SHOWING)
                  and not node.getState().contains(gui.api.STATE_DEFUNCT)]
        result['owned_frames'] = len(frames)
        if len(frames) != 1:
            return result
        frame = frames[0]
        bounds = frame.queryComponent().getExtents(gui.api.DESKTOP_COORDS)
        if (bounds.x < 0 or bounds.y < 0 or bounds.width <= 0 or bounds.height <= 0
                or bounds.x + bounds.width > 1024 or bounds.y + bounds.height > 900):
            return result
        result['frame_eligible'] = True
        active = getattr(gui.api, 'STATE_ACTIVE', None)
        result['frame_active'] = bool(frame.getState().contains(active)) if active is not None else None

        def control_flags(node):
            value = dict(flags)
            try:
                value['owned'] = node.getApplication().get_process_id() == app['pid']
                for key, attribute in (('showing', 'STATE_SHOWING'), ('enabled', 'STATE_ENABLED'),
                                       ('checked', 'STATE_CHECKED'), ('selected', 'STATE_SELECTED'), ('defunct', 'STATE_DEFUNCT'),
                                       ('focusable', 'STATE_FOCUSABLE'), ('focused', 'STATE_FOCUSED')):
                    constant = getattr(gui.api, attribute, None)
                    value[key] = bool(node.getState().contains(constant)) if constant is not None else None
                parent = node
                for _ in range(100):
                    if parent is None or parent == frame:
                        break
                    parent = parent.get_parent()
                rectangle = node.queryComponent().getExtents(gui.api.DESKTOP_COORDS)
                value['in_frame'] = bool(parent == frame and rectangle.width > 0 and rectangle.height > 0
                                         and bounds.x <= rectangle.x and bounds.y <= rectangle.y
                                         and rectangle.x + rectangle.width <= bounds.x + bounds.width
                                         and rectangle.y + rectangle.height <= bounds.y + bounds.height)
            except Exception:
                pass
            return value

        if target is not None:
            result['target'] = control_flags(target)
            return result
        tabs = []
        for node in nodes:
            try:
                value = control_flags(node)
                if value['defunct'] is not False or not all(value[key] is True for key in ('owned', 'showing', 'enabled', 'in_frame')):
                    continue
                if node.name.strip() == 'Management' and node.getRoleName() == 'page tab':
                    tabs.append(value)
            except Exception:
                pass
        result['management_tabs'] = len(tabs)
        if len(tabs) == 1:
            result['management_checked'], result['management_selected'] = tabs[0]['checked'], tabs[0]['selected']
        result['error_codes'] = installation_evidence(nodes)['error_codes']
    except Exception:
        pass
    return result


def activate_management(gui):
    # Pinned TabButtonType.qml binds Return/Enter to its click, selecting the Management page.
    # Keep one activation within the existing named-control wait budget.
    deadline = time.monotonic() + 30
    nodes = gui.wait(lambda: gui.matching('Management'), 'native server management control',
                     max(0, deadline - time.monotonic()))
    # Pinned PageType.qml moves focus to the page when its StackView push finishes.
    # Use GUI.click's 350 ms settle, then acquire the current accessible target.
    time.sleep(min(0.35, max(0, deadline - time.monotonic())))
    nodes = gui.wait(lambda: gui.matching('Management'), 'native server management control',
                     max(0, deadline - time.monotonic()))
    require(len(nodes) == 1, 'native Management control is ambiguous')
    node = nodes[0]
    snapshot = discovery_snapshot(gui, node)
    target = snapshot['target']
    require(node.getRoleName() == 'page tab' and snapshot['session_owned'] and snapshot['frame_eligible']
            and all(target[key] is True for key in ('owned', 'showing', 'enabled', 'in_frame'))
            and target['defunct'] is False, 'native Management target is not eligible')
    evidence = gui.discover_evidence
    evidence['candidate_count'] = 1
    evidence['management_target'] = target
    receipt = dict(role='page tab', method='atspi', dispatch_error='none',
                   stage='management-control', candidate_count=1)
    evidence['action'] = receipt
    selected = None
    try:
        actions = node.queryAction()
        for index in range(actions.nActions):
            if actions.getName(index).casefold() in ('click', 'press', 'activate'):
                selected = (actions, index)
                break
    except Exception:
        pass
    if selected is not None:
        try:
            accepted = selected[0].doAction(selected[1])
        except Exception as error:
            receipt['dispatch_error'] = 'timeout' if isinstance(error, TimeoutError) else 'action-exception'
            raise Failure('native Management action failed') from None
        if accepted is False:
            receipt['dispatch_error'] = 'not-accepted'
            raise Failure('native Management activation was not accepted')
    else:
        receipt['method'] = 'focus-return'
        require(target['focusable'] is True, 'native Management control is not focusable')
        try:
            focused = node.queryComponent().grabFocus()
        except Exception:
            receipt['dispatch_error'] = 'focus-exception'
            raise Failure('native Management focus is unavailable') from None
        if focused is False:
            receipt['dispatch_error'] = 'focus-refused'
            raise Failure('native Management focus was rejected')
        def focused_target():
            current = discovery_snapshot(gui, node)
            flags = current['target']
            return (current['session_owned'] and current['frame_eligible'] and current['frame_active'] is True
                    and flags['defunct'] is False
                    and all(flags[key] is True for key in ('owned', 'showing', 'enabled', 'in_frame', 'focusable', 'focused')))
        gui.wait(focused_target, 'focused native Management control', max(0, deadline - time.monotonic()))
        gui.key('Return', deadline=deadline)
    evidence['stage'] = 'management-selection'
    def selected_page():
        current = discovery_snapshot(gui)
        return (current['session_owned'] and current['frame_eligible'] and current['management_tabs'] == 1
                and (current['management_checked'] is True or current['management_selected'] is True))
    gui.wait(selected_page, 'selected native Management page', max(0, deadline - time.monotonic()))


def open_server_data(gui):
    item = gui.roles('LIST_ITEM')
    gui.discover_evidence = dict(stage='settings-page')
    gui.share_evidence = gui.discover_evidence  # Existing bounded action receipt, emitted only on failure.
    gui.tab('Settings')
    # PageSettings.qml footer row: unlike its Servers row, this also rules out the Servers page.
    gui.wait(lambda: gui.matching('Close application', item), 'Settings page')
    gui.discover_evidence['stage'] = 'servers-control'
    gui.click('Servers', description='native servers control', roles=item, unique=True)
    # The fixture owns exactly one admin server. No client import adds another.
    gui.discover_evidence['stage'] = 'saved-server'
    gui.click_saved_server()
    gui.discover_evidence['stage'] = 'management-control'
    activate_management(gui)
    return item


def discover(gui, args):
    radio = gui.roles('RADIO_BUTTON')
    item = open_server_data(gui)
    # PageSettingsServerData.qml: the scan row is a ListItemType named by its title.
    gui.discover_evidence['stage'] = 'scan-control'
    scan = gui.wait(lambda: gui.unique(SCAN_CONTROL, item), 'native service discovery control')
    # The scan result is a three-second notification; a persistent error snackbar has Close.
    gui.wait(lambda: not notification_shown(gui.owned_nodes()), 'cleared native notification')
    gui.discover_evidence['stage'] = 'scan-dispatch'
    gui.click_node(scan[0])
    gui.discover_evidence['stage'] = 'scan-completion'

    def scanned():
        nodes = gui.owned_nodes()
        if installation_evidence(nodes)['error_shown']:
            return 'error'
        # PageController.disableControls keeps page controls disabled under the busy overlay.
        control = [node for node in nodes if node.name.strip() == SCAN_CONTROL and node.getRole() in item
                   and node.getState().contains(gui.api.STATE_ENABLED)]
        return 'scanned' if len(control) == 1 and notification_shown(nodes) else None

    require(gui.wait(scanned, 'native server scan', 120) == 'scanned', 'native server scan showed an error')
    gui.discover_evidence['stage'] = 'retained-user'
    gui.back_to_tabs()
    gui.tab('Share')
    gui.click('Users', description='discovered native users control', roles=radio, unique=True)
    gui.wait(lambda: gui.matching(args.name, item), 'retained native user after discovery', 90)


def update_packages(gui, args):
    button = gui.roles('PUSH_BUTTON')
    item = open_server_data(gui)
    # PageSettingsServerData.qml: the row asks Continue/Cancel, then runs
    # update_server_packages.sh over SSH from PageDeinstalling.
    gui.discover_evidence['stage'] = 'packages-control'
    control = gui.wait(lambda: gui.unique(UPDATE_PACKAGES, item), 'native package update control')
    gui.wait(lambda: not notification_shown(gui.owned_nodes()), 'cleared native notification')
    gui.click_node(control[0])
    gui.discover_evidence['stage'] = 'packages-confirmation'
    gui.click('Continue', description='native package update confirmation', roles=button, unique=True)
    gui.discover_evidence['stage'] = 'packages-completion'

    def finished():
        nodes = gui.owned_nodes()
        if installation_evidence(nodes)['error_shown']:
            return 'error'
        # Success closes the progress page and asks Reboot/Not now; a failure shows an error snackbar.
        names = collections.Counter(node.name.strip() for node in nodes
                                    if node.getRole() in button and gui.showing(node))
        return 'updated' if names['Reboot'] == 1 and names['Not now'] == 1 else None

    require(gui.wait(finished, 'native package update', 900) == 'updated', 'native package update showed an error')
    # Kernel updates wait for a later reboot; the profile's reboot lanes cover restarts.
    gui.discover_evidence['stage'] = 'packages-reboot-question'
    gui.click('Not now', description='native post-update reboot refusal', roles=button, unique=True)
    gui.wait(lambda: not gui.matching('Reboot', button), 'closed native reboot question')
    gui.discover_evidence['stage'] = 'packages-return'
    gui.back_to_tabs()


def port(value):
    value = int(value)
    if not 1 <= value <= 65535:
        raise argparse.ArgumentTypeError('port must be 1..65535')
    return value


def name(value):
    if not re.fullmatch(r'[a-zA-Z0-9_-]{1,20}', value):
        raise argparse.ArgumentTypeError('name must contain 1..20 ASCII letters, digits, underscores or hyphens')
    return value


def discovery_self_check():
    from unittest import mock
    api = SimpleNamespace(ROLE_FRAME=1, ROLE_WINDOW=2, ROLE_PUSH_BUTTON=3, ROLE_RADIO_BUTTON=4, ROLE_TEXT=8,
                          ROLE_PAGE_TAB=14, ROLE_LIST_ITEM=18,
                          STATE_ENABLED=5, STATE_CHECKED=6, STATE_SHOWING=9,
                          STATE_DEFUNCT=12, STATE_SELECTED=13, STATE_FOCUSABLE=15,
                          STATE_FOCUSED=16, STATE_ACTIVE=17, DESKTOP_COORDS=0)
    session_bytes = b'{"app":{"pid":42,"start":"owned"},"environment":{"DISPLAY":":synthetic"}}'
    for scenario in ('ready', 'update-page', 'settings-page-missing',
                     'servers-matching-exception', 'focus-delayed-default', 'focus-after-deadline', 'focus-near-deadline', 'focus-return', 'not-accepted', 'action-exception', 'read-only', 'unknown-match',
                     'focus-unavailable', 'focus-refused', 'focus-exception', 'focus-unconfirmed', 'foreign-management',
                     'inactive-frame', 'ambiguous-management', 'focus-no-selection', 'scan-error', 'scan-silent'):
        state = dict(page='info', tab=0, clock=0.0, dispatches=0, focus_requests=0, focused=False, keys=0, pointers=0,
                     clock_consumed=False, key_timeouts=[], default_focus_due=None,
                     default_focus_reset=False, delayed_focus_sample=False, focus_requested_at=None)
        def advance(seconds):
            state['clock'] += seconds
            if (scenario == 'focus-delayed-default' and state['default_focus_due'] is not None
                    and not state['default_focus_reset'] and state['clock'] >= state['default_focus_due']):
                state['focused'] = False
                state['default_focus_reset'] = True
        def node(label, role, action=lambda: True, parent=None):
            item = SimpleNamespace(name=label, parent=parent)
            item.getApplication = lambda: SimpleNamespace(get_process_id=lambda: 42)
            item.getRole = lambda: role
            item.getRoleName = lambda: {1: 'frame', 3: 'push button', 4: 'radio button', 8: 'text', 14: 'page tab',
                                        18: 'list item', 20: 'dialog'}[role]
            item.get_parent = lambda: parent
            def contains(flag):
                if (label == 'Management' and flag == 16 and scenario == 'focus-delayed-default'
                        and state['focus_requests'] and not state['delayed_focus_sample']):
                    # One native accessibility round trip misses a brief focus
                    # acquired before PageType's 200 ms default-focus timer.
                    state['delayed_focus_sample'] = True
                    advance(0.25)
                if (label == 'Management' and flag == 16 and state['focused'] and not state['clock_consumed']
                        and scenario in ('focus-after-deadline', 'focus-near-deadline')):
                    state['clock'] = state['activation_deadline'] + (1 if scenario == 'focus-after-deadline' else -0.1)
                    state['clock_consumed'] = True
                return (flag in (5, 9) or
                flag in (6, 13) and label == 'Management' and state['page'] in ('data', 'scan-complete') or
                flag == 15 and label == 'Management' and scenario != 'focus-unavailable' or
                flag == 16 and label == 'Management' and state['focused'] or
                flag == 17 and role == 1 and scenario != 'inactive-frame')
            item.getState = lambda: SimpleNamespace(contains=contains)
            def focus():
                state['focus_requests'] += 1
                state['focus_requested_at'] = state['clock']
                if scenario == 'focus-exception':
                    raise RuntimeError('synthetic-private-focus')
                state['focused'] = scenario not in ('focus-refused', 'focus-unconfirmed')
                return scenario != 'focus-refused'
            item.queryComponent = lambda: SimpleNamespace(getExtents=lambda coordinates: SimpleNamespace(
                x=0 if role == 1 else 16, y=0 if role == 1 else 100,
                width=380 if role == 1 else 300, height=800 if role == 1 else 40), grabFocus=focus)
            item.queryAction = lambda: SimpleNamespace(nActions=1, getName=lambda index: 'press',
                                                      doAction=lambda index: action())
            item.queryText = mock.Mock(side_effect=RuntimeError('synthetic-private-body'))
            return item
        frame = node('', 1)
        def management():
            state['dispatches'] += 1
            if scenario == 'not-accepted':
                return False
            if scenario == 'action-exception':
                raise RuntimeError('synthetic-private-action')
            state['page'] = 'data'
            return True
        def scan():
            state['page'] = 'scan-complete'
            return True
        # The scan notification is an empty SnackbarType dialog: its message is not accessible.
        controls = [node('Management', 14, management, frame), node(SCAN_CONTROL, 18, scan, frame),
                    node('', 20, parent=frame), node('Servers', 18, parent=frame),
                    node('Users', 4, parent=frame), node('retained-test-user', 18, parent=frame),
                    node('Close application', 18, parent=frame), node('Update app', 3, parent=frame)]
        error_dialog = node('', 20, parent=frame)
        error_dialog.parent = frame
        error_close = node('Close', 3, parent=error_dialog)
        if scenario.startswith('focus-') or scenario == 'inactive-frame':
            controls[0].queryAction = mock.Mock(side_effect=NotImplementedError('synthetic-private-action'))
        if scenario == 'foreign-management':
            controls[0].getApplication = lambda: SimpleNamespace(get_process_id=lambda: 99)
        def current_nodes():
            valid = {'Management', 'Servers'}
            if scenario != 'settings-page-missing':
                valid.add('Close application')
            if scenario == 'update-page' and state['tab'] == 'Settings':
                valid.add('Update app')
            if state['page'] in ('data', 'scan-complete') and scenario != 'read-only':
                valid.add(SCAN_CONTROL)
            if state['page'] == 'scan-complete' and scenario not in ('scan-error', 'scan-silent'):
                valid.add('')
            if state['page'] == 'users':
                valid.update(('Users', 'retained-test-user'))
            extra = [error_dialog, error_close] if scenario == 'scan-error' and state['page'] == 'scan-complete' else []
            return [frame] + [item for item in controls if item.name in valid] + extra + (
                [node('Management', 14, management, frame)] if scenario == 'ambiguous-management' else [])
        def matching(labels, roles=None):
            if labels == 'Servers' and scenario == 'servers-matching-exception':
                raise RuntimeError('synthetic-private-navigation')
            if labels == SCAN_CONTROL and scenario == 'unknown-match':
                raise RuntimeError('synthetic-private-matching')
            labels = (labels,) if isinstance(labels, str) else labels
            return [item for item in current_nodes() if item.name in labels and (roles is None or item.getRole() in roles)]
        gui = GUI.__new__(GUI)
        gui.api, gui.nodes, gui.matching = api, current_nodes, matching
        # Entering the server page moves focus when its push finishes, after the Servers click.
        gui.click_saved_server = lambda: state.update(default_focus_due=state['clock'] + 0.2)
        gui.tab = lambda name: state.update(tab=name, page='users' if name == 'Share' else state['page'])
        gui.back_to_tabs = lambda: state.update(back=state.get('back', 0) + 1)
        def external(command, **kwargs):
            if command != ['xdotool', 'key', 'Return']:
                state['pointers'] += 1
                raise Failure('unexpected Management keyboard command')
            require(0 < kwargs['timeout'] <= state['activation_deadline'] - state['clock'],
                    'Management keyboard timeout exceeded deadline')
            state['key_timeouts'].append(kwargs['timeout'])
            state['keys'] += 1
            state['dispatches'] += 1
            if scenario != 'focus-no-selection':
                state['page'] = 'data'
            state['clock'] += 0.08
            return subprocess.CompletedProcess(command, 0)
        waits = []
        def wait(action, description, seconds=30):
            if description == 'native server management control':
                state['activation_deadline'] = state['clock'] + seconds
            waits.append((description, seconds))
            return GUI.wait(gui, action, description, seconds)
        gui.wait = wait
        with mock.patch.object(sys.modules[__name__], 'private_file', return_value=session_bytes), \
                mock.patch.object(sys.modules[__name__], 'process_identity', return_value='owned'), \
                mock.patch.dict(os.environ, DISPLAY=':synthetic'), \
                mock.patch.object(subprocess, 'run', side_effect=external), \
                mock.patch.object(time, 'monotonic', side_effect=lambda: state['clock']), \
                mock.patch.object(time, 'sleep', side_effect=advance):
            try:
                discover(gui, SimpleNamespace(name='retained-test-user'))
            except Failure as error:
                expected = {
                    'update-page': 'stock app is showing its update page despite its disabled update check',
                    'settings-page-missing': 'GUI timed out waiting for Settings page',
                    'servers-matching-exception': 'GUI timed out waiting for native servers control: last error RuntimeError',
                    'unknown-match': 'GUI timed out waiting for native service discovery control: last error RuntimeError',
                    'not-accepted': 'native Management activation was not accepted',
                    'action-exception': 'native Management action failed',
                    'focus-unavailable': 'native Management control is not focusable',
                    'focus-refused': 'native Management focus was rejected',
                    'focus-exception': 'native Management focus is unavailable',
                    'focus-unconfirmed': 'GUI timed out waiting for focused native Management control',
                    'focus-after-deadline': 'native Management activation deadline expired',
                    'focus-near-deadline': 'GUI timed out waiting for selected native Management page',
                    'foreign-management': 'native Management target is not eligible',
                    'inactive-frame': 'GUI timed out waiting for focused native Management control',
                    'ambiguous-management': 'native Management control is ambiguous',
                    'focus-no-selection': 'GUI timed out waiting for selected native Management page',
                    'scan-error': 'native server scan showed an error',
                    'scan-silent': 'GUI timed out waiting for native server scan',
                }.get(scenario, 'GUI timed out waiting for native service discovery control')
                require(scenario not in ('ready', 'focus-return', 'focus-delayed-default') and str(error) == expected,
                        'Management activation changed its safe failure boundary: ' + scenario)
                require(scenario != 'update-page' or state['clock'] == 0, 'update page failure waited for a timeout')
            else:
                require(scenario in ('ready', 'focus-return', 'focus-delayed-default'), 'unready discovery was accepted')
                require(('native server scan', 120) in waits and ('retained native user after discovery', 90) in waits,
                        'discovery observation changed existing completion waits')
            snapshot = discovery_snapshot(gui)
            if scenario in ('ready', 'focus-return', 'focus-delayed-default', 'read-only', 'unknown-match',
                            'scan-error', 'scan-silent'):
                require(('native service discovery control', 30) in waits, 'discovery changed the control wait')
            require(state.get('back', 0) == int(scenario in ('ready', 'focus-return', 'focus-delayed-default')),
                    'discovery did not return to the main tab bar exactly once after its scan')
            refused = scenario in ('update-page', 'settings-page-missing',
                                   'servers-matching-exception', 'focus-after-deadline', 'focus-unavailable', 'focus-refused', 'focus-exception', 'focus-unconfirmed', 'foreign-management',
                                   'inactive-frame', 'ambiguous-management')
            require(state['dispatches'] == (0 if refused else 1), 'Management activation was repeated or bypassed refusal')
            require(state['pointers'] == 0, 'Management activation used coordinate input')
            if scenario == 'servers-matching-exception':
                require(gui.discover_evidence['stage'] == 'servers-control', 'Servers failure lost its stage')
            require(state['keys'] == int(scenario in ('focus-return', 'focus-delayed-default', 'focus-no-selection', 'focus-near-deadline')),
                    'Management key escaped its focused single activation')
            require(state['focus_requests'] == int(scenario in ('focus-return', 'focus-delayed-default', 'focus-refused',
                    'focus-exception', 'focus-unconfirmed', 'focus-no-selection', 'inactive-frame',
                    'focus-after-deadline', 'focus-near-deadline')),
                    'Management focus was retried or unnecessarily requested')
            if scenario == 'focus-delayed-default':
                require(state['default_focus_reset'] and state['focus_requested_at'] >= state['default_focus_due']
                        and state['clock'] <= state['activation_deadline'],
                        'Management focus raced the page default or exceeded its deadline')
            if scenario == 'focus-after-deadline':
                require(not state['key_timeouts'], 'expired Management activation dispatched a keyboard command')
            if scenario == 'focus-near-deadline':
                require(len(state['key_timeouts']) == 1 and 0 < state['key_timeouts'][0] <= 0.100001
                        and state['clock'] <= state['activation_deadline'] + 0.000001,
                        'Management keyboard command or settle exceeded its deadline')
            if scenario in ('not-accepted', 'action-exception'):
                require(snapshot['management_checked'] is not True
                        and gui.discover_evidence['action']['dispatch_error'] == scenario,
                        'failed Management dispatch was not distinguished')
            elif scenario in ('read-only', 'unknown-match'):
                require(snapshot['management_checked'] is True, 'selected page without scan was not distinguished')
            foreign = node('Management', 14, parent=frame)
            foreign.getApplication = lambda: SimpleNamespace(get_process_id=lambda: 99)
            gui.nodes = lambda: current_nodes() + [foreign, node('synthetic-private-label', 8, parent=frame)]
            expected_tabs = 0 if scenario == 'foreign-management' else 2 if scenario == 'ambiguous-management' else 1
            require(discovery_snapshot(gui)['management_tabs'] == expected_tabs,
                    'foreign Management node entered diagnostics')
            defunct = node('Management', 14, parent=frame)
            defunct.getState = lambda: SimpleNamespace(contains=lambda flag: flag in (5, 9, 12))
            with mock.patch.object(gui, 'nodes', return_value=[frame, defunct]):
                stale = discovery_snapshot(gui, defunct)
                require(stale['target']['defunct'] is True and discovery_snapshot(gui)['management_tabs'] == 0,
                        'defunct target was confused with eligible Management state')
            with mock.patch.object(sys.modules[__name__], 'process_identity', return_value='foreign'):
                unavailable = discovery_snapshot(gui)
            require(not unavailable['session_owned'] and unavailable['management_tabs'] == 0,
                    'unknown ownership was treated as selected native state')
            evidence = json.dumps(dict(gui.discover_evidence, snapshot=snapshot, unavailable=unavailable))
            require('synthetic-private' not in evidence and 'Management' not in evidence,
                    'discovery diagnostics exposed native labels, body or exception text')
            for item in controls:
                item.queryText.assert_not_called()


def update_check_self_check():
    from unittest import mock
    module = sys.modules[__name__]
    def make_private(path, create=False):
        if create:
            path.mkdir(mode=0o700, exist_ok=True)
    with tempfile.TemporaryDirectory(dir='/tmp') as temporary, \
            mock.patch.object(module, 'private_dir', side_effect=make_private), \
            mock.patch.object(module, 'SETTINGS', Path(temporary) / 'AmneziaVPN.ORG' / 'AmneziaVPN.conf'):
        def refused(message, **kwargs):
            try:
                require_update_check_off(**kwargs)
            except Failure as error:
                require(str(error) == message, 'update check refusal changed: ' + str(error))
            else:
                raise Failure('update check state was accepted: ' + message)
        not_in_use = 'stock app is not using the settings file that disables its update check'
        enabled = 'stock app settings do not disable its update check'
        require(saved_servers()['state'] == 'unknown', 'unreadable settings were reported as an empty server list')
        require_update_check_off()
        require(module.SETTINGS.read_bytes() == b'[Conf]\nautoUpdateCheck=false\n', 'fresh settings were not seeded')
        refused(not_in_use, started=True)
        require(saved_servers() == dict(state='no-server-list', count=0), 'absent server list changed')
        secret = 'synthetic-private-key-value'
        servers = json.dumps([dict(hostName='192.0.2.1', userName='amnezia', password=secret, port=2226), {}])
        encoded = '"@ByteArray(' + servers.replace('\\', '\\\\').replace('"', '\\"') + ')"'
        started = f'[Conf]\nautoUpdateCheck=false\n\n[General]\npermFixed=true\n\n[Servers]\nserversList={encoded}\n'
        module.SETTINGS.write_text(started)
        require_update_check_off(started=True)
        observed = saved_servers()
        require(observed == dict(state='parsed', count=2) and secret not in json.dumps(observed),
                'saved server count is incorrect')
        for text, message in (('[Conf]\nautoUpdateCheck=true\n', enabled), ('[General]\npermFixed=true\n', enabled)):
            module.SETTINGS.write_text(text)
            refused(message)
        for value in ('@ByteArray({})', '@ByteArray([null])', '@Variant(synthetic-private)', '"@ByteArray([])'):
            module.SETTINGS.write_text(f'[Servers]\nserversList={value}\n')
            require(saved_servers() == dict(state='unknown', count=None),
                    'unknown server list encoding was reported as absence')
    gui = GUI.__new__(GUI)
    gui.nodes = lambda: [SimpleNamespace(name='Update app')]
    action = mock.Mock()
    try:
        gui.wait(action, 'synthetic page')
    except Failure as error:
        require(str(error) == 'stock app is showing its update page despite its disabled update check',
                'update page lost its explicit reason')
    else:
        raise Failure('update page was accepted')
    action.assert_not_called()
    gui.nodes = lambda: []
    for error, reason in ((RuntimeError('synthetic-private-value'), ': last error RuntimeError'),
                          (Failure('external command failed operation=frame-capture status=timeout'),
                           ': last error external command failed operation=frame-capture status=timeout')):
        try:
            gui.wait(mock.Mock(side_effect=error), 'synthetic page', 0.01)
        except Failure as timeout:
            require(str(timeout) == 'GUI timed out waiting for synthetic page' + reason,
                    'timeout lost its safe cause or exposed a value: ' + str(timeout))
        else:
            raise Failure('failing action was accepted')


def failure_evidence_self_check():
    from unittest import mock
    module = sys.modules[__name__]
    api = SimpleNamespace(ROLE_FRAME=1, ROLE_WINDOW=2, STATE_ENABLED=5, STATE_CHECKED=6, STATE_FOCUSED=7,
                          STATE_EDITABLE=8, DESKTOP_COORDS=0)
    def node(label, role, flags=(5,)):
        return SimpleNamespace(name=label, getRole=lambda: role, getRoleName=lambda: 'frame' if role == 1 else 'button',
                               getState=lambda: SimpleNamespace(contains=lambda flag: flag in flags),
                               queryComponent=lambda: SimpleNamespace(getExtents=lambda coordinates: SimpleNamespace(
                                   x=0, y=0, width=380, height=680)))
    frame, update = node('AmneziaVPN', 1), node('Update app', 3)
    private = node('synthetic-private-config', 3)
    field = node('', 3, flags=(5, 8))
    export_page = node('Show connection settings', 3)
    def fail(*_):
        raise RuntimeError('node vanished')
    def failing(base, read):
        return SimpleNamespace(**{**vars(base), read: fail})
    unnamed = type('Unnamed', (), dict(name=property(fail), getState=staticmethod(update.getState)))()
    partial_walk = [frame, update]
    for mode, nodes, image, screenshot in (('discover', [frame, update, private], b'png', 'screen.png'),
                                           ('setup', [frame, field], b'png', 'omitted'),
                                           ('share', [frame, update], b'png', 'omitted'),
                                           ('revoke', [frame, export_page], b'png', 'omitted'),
                                           ('discover', [frame, update], b'x' * 2097153, 'omitted'),
                                           ('discover', [frame, failing(node('', 3), 'queryComponent'), update],
                                            b'png', 'screen.png'),
                                           ('discover', [frame] + [update] * 499, b'png', 'screen.png'),
                                           ('revoke', [frame, failing(export_page, 'queryComponent')], b'png', 'omitted'),
                                           ('revoke', [frame, failing(field, 'queryComponent')], b'png', 'omitted'),
                                           ('revoke', [frame, failing(update, 'getState')], b'png', 'unreadable'),
                                           ('revoke', [frame, unnamed], b'png', 'unreadable'),
                                           ('revoke', [frame] + [update] * 500 + [export_page], b'png', 'unreadable'),
                                           ('revoke', partial_walk, b'png', 'unreadable')):
        with tempfile.TemporaryDirectory(dir='/tmp') as temporary, \
                mock.patch.object(module, 'FAILURE_EVIDENCE', Path(temporary)), \
                mock.patch.object(module, 'private_dir'), \
                mock.patch.object(module, 'saved_servers', return_value=dict(state='parsed', count=1)), \
                mock.patch.object(module, 'run', return_value=image) as capture:
            report = failure_evidence(SimpleNamespace(api=api, nodes=lambda: nodes,
                                                      walk_complete=nodes is not partial_walk), mode)
            stored = (Path(temporary) / 'controls.json').read_text()
            require(json.loads(stored) == report and report['screenshot'] == screenshot
                    and report['saved_servers']['count'] == 1, 'failure evidence changed: ' + mode)
            require((Path(temporary) / 'screen.png').exists() == (screenshot == 'screen.png'),
                    'failure screenshot does not match its report')
            require(capture.called == (mode == 'discover'), 'a page that can show private values was photographed')
            require('synthetic-private' not in stored, 'failure evidence exposed a private accessible name')
            require(update not in nodes or 'Update app' in [item['label'] for item in report['controls']],
                    'failure evidence lost a known control label')


def self_check():
    update_check_self_check()
    failure_evidence_self_check()
    discovery_self_check()
    from unittest import mock
    from types import SimpleNamespace
    dialog = SimpleNamespace(name='', getRoleName=lambda: 'dialog', parent=None)
    frame = SimpleNamespace(name='', getRoleName=lambda: 'frame', parent=None)
    def message(value, parent=dialog, role='text'):
        return SimpleNamespace(name=value, getRoleName=lambda: role, parent=parent,
                               queryText=lambda: SimpleNamespace(characterCount=len(value),
                                   getText=lambda start, end: value[start:end]))
    secret = 'synthetic-private-key-value'
    bar = SimpleNamespace(name='', getRoleName=lambda: 'progress bar', parent=frame)
    require('progress bar' in installation_evidence([bar])['visible_controls'], 'installation progress was not observed')
    evidence = installation_evidence([message('ErrorCode: 305. ' + secret), message('Installing'),
                                      message('Connection', frame), message(secret)])
    require(evidence == {'error_codes': [305], 'error_shown': True, 'visible_controls': ['Connection']},
            'native terminal error or progress evidence was lost')
    require(secret not in json.dumps(evidence), 'installation evidence exposed private text')
    # The 5.1.0.6 error snackbar exposes only its Close button; its message is not accessible.
    snackbar = SimpleNamespace(name='', getRoleName=lambda: 'dialog', parent=frame)
    close = message('Close', snackbar, 'push button')
    require(installation_evidence([snackbar, close]) == {'error_codes': [], 'error_shown': True, 'visible_controls': []},
            'an accessible-text-free native error was not observed')
    require(not installation_evidence([message('Close', frame, 'push button')])['error_shown'],
            'a page Close control was mistaken for a native error')
    notification = SimpleNamespace(name='', getRoleName=lambda: 'dialog', parent=frame)
    busy = SimpleNamespace(name='', getRoleName=lambda: 'dialog', parent=frame)
    spinner = message('', busy, 'animation')
    for nodes, shown in (([frame, notification], True), ([frame, busy, spinner], False),
                         ([frame, snackbar, close], False), ([frame, bar], False),
                         ([frame, busy, spinner, notification], True)):
        require(notification_shown(nodes) == shown, 'native notification state was misread')
    owned = message('ErrorCode: 305. ' + secret)
    owned.getApplication = lambda: SimpleNamespace(get_process_id=lambda: 7)
    foreign = message('ErrorCode: 203. ' + secret)
    foreign.getApplication = lambda: SimpleNamespace(get_process_id=lambda: 8)
    session_gui = GUI.__new__(GUI)
    session_gui.nodes = lambda: [owned, foreign]
    with mock.patch.object(sys.modules[__name__], 'private_file',
                           return_value=b'{"app":{"pid":7,"start":19}}'), \
            mock.patch.object(sys.modules[__name__], 'process_identity', return_value=19):
        require(session_gui.installation_evidence()['error_codes'] == [305],
                'foreign application text entered installation diagnostics')
    for value, parent in (('ErrorCode: 305. ' + secret, frame), ('ErrorCode: 0. No error', dialog),
                          ('ErrorCode: 305', dialog), ('prefix ErrorCode: 305. ' + secret, dialog),
                          ('ErrorCode: 99999. ' + secret, dialog), ('ErrorCode: 305. ' + 'x' * 4096, dialog)):
        observed = installation_evidence([message(value, parent)])
        require(not observed['error_codes'] and not observed['error_shown'],
                'unconfirmed or malformed error text became a terminal failure')
    waits = []
    def wait(action, description, seconds=30):
        waits.append((description, seconds))
        for _ in range(4):
            result = action()
            if result:
                return result
        raise Failure('synthetic installer made no progress')
    clear = {'error_codes': [], 'error_shown': False, 'visible_controls': []}
    progress = {'error_codes': [], 'error_shown': False, 'visible_controls': ['progress bar']}
    probe = mock.Mock(return_value=True)
    wait_native_installation(SimpleNamespace(wait=wait, installation_evidence=mock.Mock(
        side_effect=(progress, progress, clear))), probe)
    require(probe.call_count == 1, 'installer progress drove the GUI')
    require(waits == [('native installation completion', 360)], 'installation lost its one completion deadline')
    for states, reason in ((({'error_codes': [305], 'error_shown': True, 'visible_controls': []},), '305'),
                           ((progress, {'error_codes': [203], 'error_shown': True, 'visible_controls': ['progress bar']}), '203'),
                           ((progress, {'error_codes': [], 'error_shown': True, 'visible_controls': []}), 'unavailable')):
        probe = mock.Mock()
        try:
            wait_native_installation(SimpleNamespace(wait=wait, installation_evidence=mock.Mock(side_effect=states)), probe)
        except Failure as error:
            require(str(error) == 'native installation failed ErrorCode=' + reason,
                    'terminal error lost its safe numeric diagnostic')
        else:
            raise Failure('terminal error was mistaken for installation success')
        require(not probe.called, 'terminal installation error kept driving the GUI')
    gui = GUI.__new__(GUI)
    gui.api = SimpleNamespace(ROLE_FRAME=1, ROLE_WINDOW=2, ROLE_PUSH_BUTTON=3,
                              ROLE_RADIO_BUTTON=4, STATE_ENABLED=5, STATE_CHECKED=6,
                              STATE_EDITABLE=7, ROLE_TEXT=8, STATE_SHOWING=9,
                              STATE_FOCUSABLE=10, STATE_FOCUSED=11, STATE_DEFUNCT=12,
                              ROLE_PAGE_TAB=14, ROLE_LIST_ITEM=18, DESKTOP_COORDS=0)
    def fixture_node(role, x, y, width, height, checked=False):
        node = mock.Mock(name='synthetic-private-node')
        node.name = ''
        node.getRole.return_value = role
        node.getState.return_value.contains.side_effect = lambda state: state == 5 or (state == 6 and checked)
        node.queryComponent.return_value.getExtents.return_value = SimpleNamespace(
            x=x, y=y, width=width, height=height)
        return node
    frame = fixture_node(1, 0, 0, 380, 800)
    gui.wait = lambda action, *args: action() or require(False, 'synthetic layout rejected')
    editable = fixture_node(8, 32, 152, 316, 20)
    editable.getState.return_value.contains.side_effect = lambda state: state in (5, 7)
    editable.queryEditableText.side_effect = NotImplementedError('synthetic-private-interface')
    readonly = fixture_node(8, 32, 241, 316, 20)
    gui.nodes = lambda: [frame, readonly, editable]
    require(gui.fields() == [editable], 'editable fields without the EditableText interface were rejected')
    editable.queryEditableText.assert_not_called()

    def named(label, role, showing=lambda: True):
        node = fixture_node(role, 16, 100, 300, 40)
        node.name = label
        node.getState.return_value.contains.side_effect = lambda state: state in (5, 9) and showing()
        return node
    # PageSetupWizardConfigSource: a card and its chevron share one name; distinct cards do not.
    card = named('Self-hosted VPN', 3)
    chevron = named('Self-hosted VPN', 3)
    other_card = named('Self-hosted VPN', 3)
    card.parent, chevron.parent, other_card.parent = None, card, None
    cards = GUI.__new__(GUI)
    cards.api = gui.api
    cards.nodes = lambda: [card, chevron]
    require(cards.unique('Self-hosted VPN', (3,)) == [card], 'a setup card and its chevron were treated as two controls')
    cards.nodes = lambda: [card, chevron, other_card]
    require(cards.unique('Self-hosted VPN', (3,)) is None, 'two separate setup cards were accepted as one')
    # Main tabs: the role keeps the bottom Share tab apart from PageShare's Share button.
    tabs = [named('Share', 14), named('Share', 3), named('Settings', 14)]
    navigation = GUI.__new__(GUI)
    navigation.api, navigation.wait = gui.api, gui.wait
    navigation.nodes = lambda: tabs
    navigation.click_node = mock.Mock()
    with mock.patch.object(time, 'sleep'):
        navigation.tab('Share')
        navigation.click_node.assert_called_once_with(tabs[0])
        tabs.append(named('Share', 14))
        navigation.click_node.reset_mock()
        try:
            navigation.tab('Share')
        except Failure:
            pass
        else:
            raise Failure('ambiguous main tab was accepted')
        navigation.click_node.assert_not_called()
    # Back closes pushed pages until the main tab bar returns, at most three pages.
    for depth, accepted in ((0, True), (2, True), (3, True), (4, False)):
        stack = list(range(depth))
        backs = {}
        def back(level):
            return backs.setdefault(level, named('Back', 3, showing=lambda: level in stack))
        navigation.nodes = lambda: [back(stack[-1])] if stack else [named('Settings', 14)]
        navigation.click_node = mock.Mock(side_effect=lambda node: stack.pop())
        try:
            navigation.back_to_tabs()
        except Failure:
            require(not accepted, 'returning to the main tab bar was rejected')
        else:
            require(accepted, 'an unexpected page depth was accepted')
        require(navigation.click_node.call_count == min(depth, 3), 'Back was clicked more often than pages were open')
    for already_ready in (False, True):
        wizard = mock.Mock()
        wizard.api = gui.api
        wizard.roles.side_effect = lambda *names: names
        wizard.installation_evidence.return_value = clear
        stage = ['credentials']
        manual = fixture_node(4, 16, 390, 348, 95, checked=True)
        def unique(label, roles):
            if label == 'Manual' and roles == ('RADIO_BUTTON',):
                return [manual]
            if label == 'Install' and roles == ('PUSH_BUTTON',):
                return [object()]
            if (stage[0] == 'installed' and already_ready
                    and (label, roles) in (('Connection', ('RADIO_BUTTON',)), ('Users', ('RADIO_BUTTON',)),
                                           ('Share', ('PUSH_BUTTON',)))):
                return [object()]
            return None
        wizard.unique.side_effect = unique
        wizard.fields.side_effect = lambda: [object()] * (3 if stage[0] == 'credentials' else 1)
        def click(label, **kwargs):
            if label in ('AmneziaWG', 'Install'):
                stage[0] = 'port' if label == 'AmneziaWG' else 'installed'
        wizard.click.side_effect = click
        wizard.wait.side_effect = gui.wait
        with mock.patch.object(sys.modules[__name__], 'private_file', return_value=b'-----BEGIN PRIVATE KEY-----\n'):
            try:
                setup(wizard, SimpleNamespace(key_file=Path('/synthetic-key'), host='192.0.2.1',
                                              ssh_port=22, user='synthetic-user', udp_port=51820))
            except Failure:
                require(not already_ready, 'fast native installation was rejected')
            else:
                require(already_ready, 'returned port screen was accepted as an installation')
        wizard.tab.assert_not_called()
        require([call.args[0] for call in wizard.click.call_args_list] == [
            "Let's get started", 'Self-hosted VPN', 'Continue', 'Manual', 'Continue', 'AmneziaWG', 'Install'],
            'native setup changed its stock control sequence')
    # Update server packages: success asks Reboot/Not now; the fixture never reboots from the app.
    for outcome in ('updated', 'error', 'duplicate question'):
        updater = mock.Mock()
        updater.discover_evidence = {}
        updater.roles.side_effect = lambda *names: names
        updater.api = gui.api
        updater.showing.return_value = True
        stage = ['data']
        question = [named('Reboot', 'PUSH_BUTTON'), named('Not now', 'PUSH_BUTTON')]
        if outcome == 'duplicate question':
            question.append(named('Not now', 'PUSH_BUTTON'))
        snackbar = SimpleNamespace(name='', getRoleName=lambda: 'dialog', parent=None, getRole=lambda: 20)
        error = [snackbar, SimpleNamespace(name='Close', getRoleName=lambda: 'push button', parent=snackbar, getRole=lambda: 3)]
        def nodes():
            if stage[0] == 'running':
                stage[0] = 'finished'
                return []
            if stage[0] == 'finished':
                return error if outcome == 'error' else question
            return []
        updater.owned_nodes.side_effect = nodes
        updater.unique.side_effect = lambda label, roles: [object()] if label == UPDATE_PACKAGES else None
        def clicked(label, **kwargs):
            if label == 'Continue':
                stage[0] = 'running'
            elif label == 'Not now':
                stage[0] = 'closed'
        updater.click.side_effect = clicked
        updater.matching.side_effect = lambda label, roles=None: question[:1] if stage[0] == 'finished' else []
        updater.wait.side_effect = wait
        with mock.patch.object(sys.modules[__name__], 'open_server_data', return_value=('LIST_ITEM',)):
            try:
                update_packages(updater, SimpleNamespace())
            except Failure as failure:
                require(outcome != 'updated', 'native package update success was rejected')
                require(outcome != 'error' or str(failure) == 'native package update showed an error',
                        'a native package update error was not reported at once')
            else:
                require(outcome == 'updated', 'a failed or ambiguous native package update was accepted')
        clicks = [call.args[0] for call in updater.click.call_args_list]
        require(clicks == (['Continue', 'Not now'] if outcome == 'updated' else ['Continue']),
                'native package update rebooted the server or skipped its confirmation')
        require(updater.back_to_tabs.called == (outcome == 'updated'), 'package update did not return to the tab bar')
    for scenario in ('removed', 'missing page', 'wrong tab', 'duplicate users',
                     'details open', 'confirmation open', 'error open', 'retained row'):
        revocation = mock.Mock()
        revocation.api = gui.api
        revocation.roles.side_effect = lambda *names: names
        users = fixture_node(4, 16, 300, 174, 56, checked=scenario != 'wrong tab')
        present = {'Users': [users] * (2 if scenario == 'duplicate users' else 1)} if scenario != 'missing page' else {}
        for case, label in (('details open', 'Revoke'), ('confirmation open', 'Continue'),
                            ('error open', 'Close'), ('retained row', 'synthetic-private-client')):
            if scenario == case:
                present[label] = [object()]
        revocation.unique.side_effect = lambda label, roles: present[label] if len(present.get(label, [])) == 1 else None
        revocation.matching.side_effect = lambda label, roles=None: present.get(label, [])
        revocation.wait.side_effect = gui.wait
        try:
            revoke(revocation, SimpleNamespace(name='synthetic-private-client'))
        except Failure:
            require(scenario != 'removed', 'durable native revocation state was rejected')
        else:
            require(scenario == 'removed', 'transitional native revocation state was accepted')
        require(all(call.args[0] != 'Config revoked' for call in revocation.matching.call_args_list),
                'native revocation still depends on a transient notification')
    button = fixture_node(3, 16, 648, 348, 56)
    button.getRoleName.return_value = 'button'
    gui.share_evidence = {'stage': 'export-dialog'}
    action = button.queryAction.return_value
    action.nActions = 1
    for action_name, accepted in (('Press', True), ('Press', False),
                                  ('Press', RuntimeError('synthetic-private-value')),
                                  ('Press', TimeoutError('synthetic-private-value')), ('SetFocus', True)):
        action.getName.return_value = action_name
        action.doAction.return_value = accepted if isinstance(accepted, bool) else None
        action.doAction.side_effect = accepted if isinstance(accepted, Exception) else None
        action.doAction.reset_mock()
        with mock.patch.object(sys.modules[__name__], 'run') as geometry:
            GUI.click_node(gui, button)
        require(geometry.called == (action_name != 'Press'),
                'native action selection or geometry fallback changed')
        require(action.doAction.call_count == (1 if action_name == 'Press' else 0),
                'native action was repeated or accepted an unrelated action')
        receipt = gui.share_evidence['action']
        expected = ('timeout' if isinstance(accepted, TimeoutError) else 'action-exception'
                    if isinstance(accepted, Exception) else 'not-accepted' if accepted is False else 'none')
        require(receipt['method'] == ('atspi' if action_name == 'Press' else 'pointer')
                and receipt['dispatch_error'] == expected and receipt['role'] == 'button',
                'share action diagnostics lost delivery evidence')
        require('synthetic-private' not in json.dumps(receipt), 'share action diagnostics exposed an exception')
    click_gui = GUI.__new__(GUI)
    click_gui.share_evidence = {'stage': 'export-dialog'}
    click_gui.wait = gui.wait
    click_gui.matching = lambda names, roles=None: [button, dialog]
    click_gui.click_node = mock.Mock()
    with mock.patch.object(time, 'sleep'):
        GUI.click(click_gui, 'Share')
    require(click_gui.share_evidence['candidate_count'] == 2, 'share candidate ambiguity was not recorded')
    click_gui.click_node.assert_called_once_with(button)
    button.queryAction.side_effect = RuntimeError('synthetic-private-value')
    with mock.patch.object(sys.modules[__name__], 'run') as geometry:
        GUI.click_node(gui, button)
    require(geometry.call_count == 1, 'unavailable native action lost the geometry fallback')
    with mock.patch.object(sys.modules[__name__], 'run', side_effect=Failure('synthetic-private-value')):
        try:
            GUI.click_node(gui, button)
        except Failure:
            require(gui.share_evidence['action']['dispatch_error'] == 'pointer-exception',
                    'share pointer failure lost its safe category')
        else:
            raise Failure('share diagnostics swallowed a pointer failure')
    dialog = fixture_node(1, -30, 120, 440, 550)
    dialog.name = 'Save AmneziaWG config'
    dialog.getApplication.return_value.get_process_id.return_value = 42
    save = fixture_node(3, 270, 602, 124, 40)
    save.name = 'Save AmneziaWG config'
    filename = fixture_node(8, -10, 557, 198, 40)
    filetype = fixture_node(8, -10, 602, 158, 40)
    for node in (save, filename, filetype):
        node.get_parent.return_value = dialog
        node.getState.return_value.contains.side_effect = lambda value: value in (5, 7, 9, 10)
    filetype.getState.return_value.contains.side_effect = lambda value: value in (7, 9, 10)
    for node, role in ((dialog, 'frame'), (save, 'button'), (filename, 'text'), (filetype, 'text')):
        node.getRoleName.return_value = role
        node.getApplication.return_value.get_process_id.return_value = 42
    filename.name = 'synthetic-private-key-value'
    session = json.dumps({'app': {'pid': 42, 'start': 'synthetic-start'},
                          'environment': {'DISPLAY': ':synthetic'}}).encode()
    with mock.patch.object(sys.modules[__name__], 'private_file', return_value=session), \
            mock.patch.object(sys.modules[__name__], 'process_identity', return_value='synthetic-start'), \
            mock.patch.dict(os.environ, {'DISPLAY': ':synthetic'}):
        export_frame = fixture_node(1, 0, 0, 380, 680)
        outgoing = fixture_node(3, -364, 180, 348, 56)
        current = fixture_node(3, 16, 180, 348, 56)
        landmark = fixture_node(3, 16, 300, 348, 56)
        outgoing.name = current.name = 'Share'
        landmark.name = 'Show connection settings'
        for node in (export_frame, outgoing, current, landmark):
            node.getApplication.return_value.get_process_id.return_value = 42
            node.getState.return_value.contains.side_effect = lambda value: value in (5, 9)
            node.get_parent.return_value = export_frame
        export_gui = GUI.__new__(GUI)
        export_gui.api = gui.api
        export_gui.share_evidence = {'stage': 'export-dialog'}
        export_gui.nodes = lambda: [export_frame, outgoing, current, landmark]
        export_gui.matching = lambda names, roles=None: [outgoing, current]
        export_gui.wait = mock.Mock(side_effect=lambda action, description, seconds: action()
                                   or require(False, 'synthetic export target rejected'))
        export_gui.click_node = mock.Mock()
        with mock.patch.object(time, 'sleep'):
            click_export_share(export_gui)
        require(export_gui.click_node.call_args.args[0] is current,
                'native export clicked the outgoing first Share candidate')
        export_gui.click_node.assert_called_once_with(current)
        require(export_gui.wait.call_args.args[2] == 30, 'native export control budget changed')
        require(export_gui.share_evidence['export_target'] == {
            'raw_candidates': 2, 'eligible_candidates': 1, 'current_page': True},
            'native export target evidence lost the outgoing candidate')
        for scenario in ('duplicate', 'missing-landmark', 'stale-landmark', 'foreign', 'defunct'):
            nodes = [export_frame, outgoing, current, landmark]
            if scenario == 'duplicate':
                nodes.append(current)
            elif scenario == 'missing-landmark':
                nodes.remove(landmark)
            elif scenario == 'stale-landmark':
                landmark.queryComponent.return_value.getExtents.return_value.x = 380
            elif scenario == 'foreign':
                current.getApplication.return_value.get_process_id.return_value = 99
            else:
                current.getState.return_value.contains.side_effect = lambda value: value in (5, 9, 12)
            export_gui.nodes = lambda: nodes
            export_gui.click_node.reset_mock()
            try:
                click_export_share(export_gui)
            except Failure:
                pass
            else:
                raise Failure('ambiguous or unavailable native export target was accepted')
            export_gui.click_node.assert_not_called()
            require('synthetic-private' not in json.dumps(export_gui.share_evidence),
                    'native export target evidence exposed UI data')
            landmark.queryComponent.return_value.getExtents.return_value.x = 16
            current.getApplication.return_value.get_process_id.return_value = 42
            current.getState.return_value.contains.side_effect = lambda value: value in (5, 9)
        gui.nodes = lambda: [dialog, save, filename, filetype]
        gui.fields = lambda: [filename, filetype]
        require(export_controls(gui) == (save, filename),
                'native export selected its title or disabled filter instead of filename and accept button')
        for nodes, fields, reason in (([dialog, save, filename, filetype], [filename, filetype], 'ready'),
                                      ([dialog, filename], [filename], 'save-candidate-count'),
                                      ([dialog, save, filename], [], 'editable-candidate-count'),
                                      ([dialog, save, save, filename], [filename], 'save-candidate-count')):
            gui.nodes = lambda: nodes
            gui.fields = lambda: fields
            diagnostic = share_diagnostics(gui)
            require(diagnostic['predicate'] == reason and diagnostic['owned_frames'] == 1,
                    'share diagnostics lost distinct export predicate states')
            require('synthetic-private' not in json.dumps(diagnostic), 'share diagnostics exposed UI text')
        gui.nodes = lambda: [dialog, save, filename, filetype]
        gui.fields = lambda: [filename, filetype]
        error_node = message('ErrorCode: 305. synthetic-private-key-value')
        error_node.getApplication = lambda: SimpleNamespace(get_process_id=lambda: 42)
        error_node.getRole = lambda: gui.api.ROLE_TEXT
        error_node.getState = lambda: SimpleNamespace(contains=lambda state: False)
        gui.nodes = lambda: [dialog, save, filename, error_node]
        diagnostic = share_diagnostics(gui)
        require(diagnostic['error_codes'] == [305] and 'synthetic-private' not in json.dumps(diagnostic),
                'share diagnostics lost or exposed the native terminal error')
        with mock.patch.object(sys.modules[__name__], 'process_identity', return_value='foreign-start'):
            diagnostic = share_diagnostics(gui)
        require(diagnostic['predicate'] == 'ownership-or-access-unavailable'
                and diagnostic['error_codes'] == [] and diagnostic['owned_frames'] == 0,
                'share diagnostics inspected a stale application session')
        gui.nodes = lambda: [dialog, save, filename, filetype]
        filename.queryText.return_value.getText.assert_not_called()
        gui.nodes = lambda: [dialog, save, save, filename, filetype]
        require(export_controls(gui) is None, 'ambiguous native export buttons were accepted')
        gui.nodes = lambda: [dialog, save, filename, filetype]
        gui.fields = lambda: [filename, filename, filetype]
        require(export_controls(gui) is None, 'ambiguous native export fields were accepted')
        gui.fields = lambda: [filename, filetype]
        save.getState.return_value.contains.side_effect = lambda value: value == 9
        require(export_controls(gui) == (save, filename), 'disabled pre-fill Save hid the native export dialog')
        require(export_controls(gui, accepting=True) is None, 'disabled Save was accepted for native export')
        directory = fixture_node(8, -10, 170, 360, 40)
        directory.get_parent.return_value = dialog
        directory.getState.return_value.contains.side_effect = lambda value: value in (5, 7, 9, 10, 11)
        gui.fields = lambda: [directory, filename, filetype]
        require(export_controls(gui, location=True) == (save, directory),
                'native export directory input was not scoped to its focused dialog field')
        gui.fields = lambda: [directory, directory, filename, filetype]
        require(export_controls(gui, location=True) is None, 'ambiguous native directory inputs were accepted')
        # PageSettingsServersList: one Back control and one ListItemType per saved server.
        server_frame = fixture_node(1, 0, 0, 380, 680)
        back = fixture_node(3, 8, 20, 40, 40)
        row = fixture_node(18, 16, 138, 348, 56)
        second_row = fixture_node(18, 16, 218, 348, 56)
        footer = fixture_node(18, 16, 600, 348, 56)
        selector = fixture_node(4, 16, 300, 348, 56)
        foreign = fixture_node(18, 16, 138, 348, 56)
        back.name, footer.name = 'Back', 'Close application'
        for node in (server_frame, back, row, second_row, footer, selector, foreign):
            node.getApplication.return_value.get_process_id.return_value = 42
            node.getState.return_value.contains.side_effect = lambda value: value in (5, 9)
        foreign.getApplication.return_value.get_process_id.return_value = 99
        gui.fields = lambda: []
        gui.matching = lambda label, roles=None: [object()] if label == 'Management' else []
        gui.click_node = mock.Mock()
        for nodes, accepted in (([server_frame, back, row], True),
                                ([server_frame, back, row, foreign], True),
                                ([server_frame, back, row, second_row], False),
                                ([server_frame, back], False),
                                ([server_frame, back, foreign], False),
                                ([server_frame, back, row, footer], False),
                                ([server_frame, back, row, selector], False),
                                ([server_frame, row], False)):
            gui.nodes = lambda: nodes
            gui.click_node.reset_mock()
            try:
                gui.click_saved_server()
            except Failure:
                require(not accepted, 'owned saved-server control was rejected')
            else:
                require(accepted, 'ambiguous or foreign saved-server control was accepted')
            require(gui.click_node.call_count == int(accepted), 'unsafe saved-server layout clicked a control')
            if accepted:
                gui.click_node.assert_called_once_with(row)
        synthetic_wait = gui.wait
        gui.wait = lambda action, description, seconds=30: GUI.wait(gui, action, description, 0.01)
        for nodes, reason in (([server_frame, back], 'Servers page must list one saved server: rows=0'),
                              ([server_frame, back, row, second_row], 'Servers page must list one saved server: rows=2'),
                              ([server_frame, row], 'Servers page was not reached after selecting Servers '
                               '(GUI timed out waiting for saved native administration server)')):
            gui.nodes = lambda: nodes
            gui.click_node.reset_mock()
            try:
                gui.click_saved_server()
            except Failure as error:
                require(str(error) == reason, 'saved server failure lost its reason: ' + str(error))
            else:
                raise Failure('a lost or duplicated saved server was accepted')
            gui.click_node.assert_not_called()
        gui.wait = synthetic_wait
        gui.nodes = lambda: [server_frame, back, row]
        gui.click_node.reset_mock()
        with mock.patch.object(sys.modules[__name__], 'process_identity', return_value='foreign-start'):
            try:
                gui.click_saved_server()
            except Failure:
                pass
            else:
                raise Failure('foreign saved-server session was accepted')
        gui.click_node.assert_not_called()
    command = ['synthetic-private-program', 'synthetic-private-argument']
    with mock.patch.object(subprocess, 'run', return_value=subprocess.CompletedProcess(
            command, 42, b'synthetic-private-stdout', b'synthetic-private-stderr')):
        try:
            run(command, capture=True)
        except Failure as error:
            require(str(error) == 'external command failed operation=external status=exit exit_code=42',
                    'external command diagnostics are missing or expose private values')
        else:
            raise Failure('failed external command was accepted')
    for operation in ('external', 'apt-update', 'apt-install', 'installer-help', 'installer-install',
                      'pointer-click', 'frame-capture', 'clipboard-write', 'clipboard-clear', 'keyboard-input'):
        prefix = f'external command failed operation={operation}'
        outcomes = ((subprocess.CompletedProcess(command, -6, b'synthetic-private-stdout', b'synthetic-private-stderr'),
                     prefix + ' status=exit exit_code=-6'),
                    (OSError(2, 'synthetic-private-error', 'synthetic-private-path'),
                     prefix + ' status=unavailable errno=2'),
                    (OSError('synthetic-private-error'), prefix + ' status=unavailable errno=unknown'),
                    (subprocess.TimeoutExpired(command, 30, output=b'synthetic-private-stdout',
                                               stderr=b'synthetic-private-stderr'), prefix + ' status=timeout'))
        for outcome, diagnostic in outcomes:
            with mock.patch.object(subprocess, 'run',
                                   return_value=outcome if isinstance(outcome, subprocess.CompletedProcess) else None,
                                   side_effect=outcome if isinstance(outcome, Exception) else None) as external:
                try:
                    run(command, capture=True, operation=operation,
                        env={'synthetic-private-env': 'synthetic-private-value'}, input_data=b'synthetic-private-key')
                except Failure as error:
                    require(str(error) == diagnostic, 'external command diagnostics expose private values')
                else:
                    raise Failure('failed external command was accepted')
                require(external.call_args.kwargs['stderr'] == subprocess.DEVNULL,
                        'external command stderr was not suppressed')
    with mock.patch.object(subprocess, 'run', return_value=subprocess.CompletedProcess(command, 0, b'captured', b'')):
        require(run(command, capture=True) == b'captured' and run(command) is None,
                'successful external command output changed')
    with mock.patch.object(subprocess, 'run') as external:
        try:
            run(command, operation='synthetic-private-label')
        except Failure as error:
            require(str(error) == 'invalid external operation label', 'invalid operation label was exposed')
        else:
            raise Failure('invalid operation label was accepted')
        require(not external.called, 'invalid operation label ran an external command')
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
    modes.add_parser('update-packages')
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
            session(reopen=args.mode in ('discover', 'update-packages'))
            gui = GUI()
            require_update_check_off(started=True)
            if args.mode == 'setup':
                setup(gui, args)
            elif args.mode == 'share':
                share(gui, args)
            elif args.mode == 'revoke':
                revoke(gui, args)
            elif args.mode == 'update-packages':
                update_packages(gui, args)
            else:
                discover(gui, args)
        print(f'native_app_mode={args.mode} result=pass')
        return 0
    except Exception as error:
        message = str(error) if isinstance(error, Failure) else type(error).__name__
        print(f'native_app_mode={args.mode} result=fail reason={message}', file=sys.stderr)
        if gui:
            try:
                evidence = failure_evidence(gui, args.mode)
                print('native_app_failure_evidence=' + json.dumps(dict(
                    saved_servers=evidence['saved_servers'], screenshot=evidence['screenshot']), sort_keys=True),
                      file=sys.stderr)
            except Exception:
                pass
            try:
                print('native_app_visible_role_counts=' + json.dumps(gui.diagnostics(), sort_keys=True), file=sys.stderr)
                if args.mode == 'setup':
                    print('native_app_installation_evidence=' + json.dumps(gui.installation_evidence(), sort_keys=True),
                          file=sys.stderr)
                if args.mode == 'share':
                    print('native_app_share_evidence=' + json.dumps(share_diagnostics(gui), sort_keys=True),
                          file=sys.stderr)
            except Exception:
                pass
            if args.mode in ('discover', 'update-packages'):
                print('native_app_discover_evidence=' + json.dumps(dict(
                    getattr(gui, 'discover_evidence', {}), snapshot=discovery_snapshot(gui)), sort_keys=True),
                      file=sys.stderr)
        return 1


if __name__ == '__main__':
    sys.exit(main())
