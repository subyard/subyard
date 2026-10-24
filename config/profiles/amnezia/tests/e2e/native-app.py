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
import csv
import hashlib
import ipaddress
import io
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
VERSION = '5.0.3.0'
ASSET = 'AmneziaVPN_5.0.3.0_linux_x64.run'
URL = f'https://github.com/amnezia-vpn/amnezia-client/releases/download/{VERSION}/{ASSET}'
# GitHub release asset metadata, checked 2026-10-05; never execute an unchecked asset.
DIGEST = '0335f2643f58c4d7494be4c6d47582574fa7e5a463450e9a47b0c5c2eda797c2'
LAUNCHER = '/opt/AmneziaVPN/bin/AmneziaVPN'
GUI_BINARY = LAUNCHER
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


def run(command, timeout=30, capture=False, env=None, input_data=None, operation='external'):
    require(operation in ('external', 'apt-update', 'apt-install', 'installer-help', 'installer-install',
                          'pointer-click', 'pointer-tab', 'frame-capture', 'ocr',
                          'clipboard-write', 'clipboard-clear', 'keyboard-input'),
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
         'libfontconfig1', 'libxcb-shape0', 'libegl1', 'imagemagick', 'tesseract-ocr'], timeout=300,
        operation='apt-install')
    require('eng' in run(['tesseract', '--list-langs'], capture=True).decode().splitlines(),
            'English OCR data is unavailable')
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


class VisibleText:
    """Geometry of one uniquely recognized phrase; text never leaves OCR memory."""
    def __init__(self, rectangle):
        self.rectangle = rectangle

    def queryComponent(self):
        return self

    def getExtents(self, coordinates):
        return self.rectangle


def visible_text(tsv, names, frame, scale=1):
    """Accept complete lines, including geometrically adjacent wrapped lines."""
    normalized = lambda value: ' '.join(value.lower().split())
    targets = {normalized(name) for name in names}
    lines = collections.defaultdict(list)
    for row in csv.DictReader(io.StringIO(tsv), delimiter='\t'):
        if row['level'] != '5' or not row['text'].strip():
            continue
        x, y, width, height = (int(row[key]) for key in ('left', 'top', 'width', 'height'))
        confidence = float(row['conf'])
        valid = (80 <= confidence <= 100 and x >= 0 and y >= 0 and width > 0 and height > 0
                 and x + width <= frame.width * scale and y + height <= frame.height * scale)
        key = tuple(int(row[key]) for key in ('page_num', 'block_num', 'par_num', 'line_num'))
        lines[key].append((x, y, width, height, row['text'], valid))
    matches = {}
    # Sparse-text OCR can put each wrapped line in a separate block. Use screen
    # order and the adjacency guards below, rather than its paragraph IDs.
    ordered = sorted(lines, key=lambda key: (key[0], min(word[1] for word in lines[key]),
                                             min(word[0] for word in lines[key])))
    for index, key in enumerate(ordered):
        words, previous = [], None
        for next_key in ordered[index:]:
            if next_key[0] != key[0]:
                break
            line = sorted(lines[next_key])
            left, top = min(word[0] for word in line), min(word[1] for word in line)
            right = max(word[0] + word[2] for word in line)
            bottom = max(word[1] + word[3] for word in line)
            if previous:
                old_left, old_top, old_right, old_bottom = previous
                if (top < old_bottom or top - old_bottom > 2 * max(bottom - top, old_bottom - old_top)
                        or right <= old_left or left >= old_right):
                    break
            words.extend(line)
            phrase = normalized(' '.join(word[4] for word in words))
            if phrase in targets:
                x, y = min(word[0] for word in words), min(word[1] for word in words)
                end_x = max(word[0] + word[2] for word in words)
                end_y = max(word[1] + word[3] for word in words)
                matches[(x, y, end_x - x, end_y - y)] = all(word[5] for word in words)
            if not any(target.startswith(phrase + ' ') for target in targets):
                break
            previous = (left, top, right, bottom)
    if len(matches) != 1 or not all(matches.values()):
        return []
    x, y, width, height = next(iter(matches))
    left, top = x // scale, y // scale
    right = (x + width + scale - 1) // scale
    bottom = (y + height + scale - 1) // scale
    return [VisibleText(SimpleNamespace(x=frame.x + left, y=frame.y + top,
                                        width=right - left, height=bottom - top))]


def installation_evidence(nodes):
    # 5.0.3.0: main2.qml opens PopupType for PageController.showErrorMessage;
    # pageController.cpp renders the public ErrorCode prefix in that modal popup.
    # Read bounded text in memory, but return only numeric codes and fixed UI flags.
    controls = ('Installing', 'Cancel installation', 'Connection', 'Users', 'Share')
    visible = set()
    errors = set()
    for node in nodes:
        try:
            label = node.name
            if isinstance(label, str) and label.strip() in controls:
                visible.add(label.strip())
            parent = node
            for _ in range(32):
                if parent is None:
                    break
                if parent.getRoleName() == 'dialog':
                    break
                parent = parent.parent
            else:
                continue
            if parent is None or parent.getRoleName() != 'dialog':
                continue
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
    return {'error_codes': sorted(errors)[:8], 'visible_controls': sorted(visible)}


def wait_native_installation(gui, ready):
    def outcome():
        errors = gui.installation_evidence()['error_codes']
        if errors:
            return 'error', errors
        return ('ready', []) if ready() else False

    def started():
        state = outcome()
        if state:
            return state
        return ('started', []) if gui.matching('Installing') else False

    def verify(state):
        require(state[0] != 'error', 'native installation failed ErrorCode='
                + ','.join(str(code) for code in state[1]))

    verify(gui.wait(started, 'native installer startup'))
    verify(gui.wait(outcome, 'native installation completion', 360))


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
        nodes = self.nodes()
        named = [node for node in nodes if node.name.strip() in names]
        if named:
            return named
        frames = [node for node in nodes if node.getRole() in (self.api.ROLE_FRAME, self.api.ROLE_WINDOW)]
        if len(frames) != 1:
            return []
        state = json.loads(private_file(SESSION))
        app = state['app']
        require(process_identity(app['pid']) == app['start']
                and frames[0].getApplication().get_process_id() == app['pid']
                and os.environ.get('DISPLAY') == state['environment']['DISPLAY'],
                'OCR requires the owned application session')
        frame = frames[0].queryComponent().getExtents(self.api.DESKTOP_COORDS)
        require(frame.x >= 0 and frame.y >= 0 and 0 < frame.width <= 1024 and 0 < frame.height <= 900
                and frame.x + frame.width <= 1024 and frame.y + frame.height <= 900,
                'OCR requires one bounded application frame')
        image = run(['import', '-window', 'root', '-crop',
                     f'{frame.width}x{frame.height}+{frame.x}+{frame.y}', '-resize', '200%', 'png:-'],
                    capture=True, timeout=5, operation='frame-capture')
        require(len(image) <= 12582912, 'OCR frame capture is too large')
        tsv = run(['tesseract', 'stdin', 'stdout', '-l', 'eng', '--psm', '11', 'tsv'],
                  capture=True, input_data=image, timeout=5, operation='ocr')
        require(len(tsv) <= 262144, 'OCR output is too large')
        return visible_text(tsv.decode(), names, frame, scale=2)

    def click_node(self, node):
        evidence = getattr(self, 'share_evidence', None)
        receipt = None
        if isinstance(evidence, dict):
            role = 'unknown'
            try:
                candidate = node.getRoleName()
                if candidate in ('button', 'page tab', 'text', 'label'):
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

    def click(self, names, seconds=30, description='a named control'):
        nodes = self.wait(lambda: self.matching(names), description, seconds)
        evidence = getattr(self, 'share_evidence', None)
        if isinstance(evidence, dict):
            evidence['candidate_count'] = min(len(nodes), 3000)
        # Qt labels inside custom clickable rows also work through their geometry.
        self.click_node(nodes[0])
        time.sleep(0.35)

    def cards(self, role):
        nodes = self.nodes()
        frames = [node for node in nodes if node.getRole() in (self.api.ROLE_FRAME, self.api.ROLE_WINDOW)]
        if len(frames) != 1:
            return []
        frame = frames[0].queryComponent().getExtents(self.api.DESKTOP_COORDS)
        cards = []
        for node in nodes:
            if node.getRole() != role or not node.getState().contains(self.api.STATE_ENABLED):
                continue
            rectangle = node.queryComponent().getExtents(self.api.DESKTOP_COORDS)
            # Both pinned wizard card components fill the page with 16 px side margins.
            if (abs(rectangle.x - frame.x - 16) <= 2
                    and abs(rectangle.width - frame.width + 32) <= 2
                    and rectangle.height >= 64 and rectangle.y >= frame.y
                    and rectangle.y + rectangle.height <= frame.y + frame.height):
                cards.append((rectangle.y, rectangle.y + rectangle.height, node))
        cards.sort(key=lambda item: item[0])
        if any(left[1] > right[0] for left, right in zip(cards, cards[1:])):
            return []
        return cards

    def click_setup_card(self, names, manual=False):
        role = self.api.ROLE_RADIO_BUTTON if manual else self.api.ROLE_PUSH_BUTTON
        description = 'manual installation card' if manual else 'self-hosted server card'

        def target():
            named = self.matching(names)
            if named:
                return named[0]
            cards = self.cards(role)
            if (not (len(cards) == 2 if manual else 2 <= len(cards) <= 6)
                    or any(item[2].name.strip() for item in cards)):
                return None
            # ConfigSource variants start with VPN by Amnezia, then Self-hosted VPN.
            # Easy has exactly one Automatic delegate, followed by its Manual footer.
            if not manual and abs(cards[1][0] - cards[0][1] - 16) > 2:
                return None
            return cards[1][2]

        self.click_node(self.wait(target, description, 90 if manual else 30))
        time.sleep(0.35)
        if manual:
            def selected():
                cards = self.cards(role)
                return len(cards) == 2 and cards[1][2].getState().contains(self.api.STATE_CHECKED)
            self.wait(selected, 'manual installation selection')

    def click_awg(self):
        def target():
            named = self.matching('AmneziaWG')
            if named:
                return named[0]
            nodes = self.nodes()
            frames = [node for node in nodes if node.getRole() in (self.api.ROLE_FRAME, self.api.ROLE_WINDOW)]
            if (len(frames) != 1 or self.fields()
                    or any(node.getRole() == self.api.ROLE_RADIO_BUTTON for node in nodes)):
                return None
            frame = frames[0].queryComponent().getExtents(self.api.DESKTOP_COORDS)
            buttons = [node for node in nodes if node.getRole() == self.api.ROLE_PUSH_BUTTON]
            back, arrows = [], []
            for node in buttons:
                if node.name.strip() or not node.getState().contains(self.api.STATE_ENABLED):
                    return None
                rectangle = node.queryComponent().getExtents(self.api.DESKTOP_COORDS)
                if abs(rectangle.width - 40) > 2 or abs(rectangle.height - 40) > 2:
                    return None
                if abs(rectangle.x - frame.x - 8) <= 2 and abs(rectangle.y - frame.y - 20) <= 2:
                    back.append(node)
                elif (abs(rectangle.x - frame.x - frame.width + 56) <= 2
                      and rectangle.y >= frame.y + 76
                      and rectangle.y + rectangle.height <= frame.y + frame.height):
                    arrows.append((rectangle.y, node))
            arrows.sort(key=lambda item: item[0])
            if (len(back) != 1 or not 4 <= len(arrows) <= 5 or len(buttons) != len(arrows) + 1
                    or any(right[0] - left[0] < 70 for left, right in zip(arrows, arrows[1:]))):
                return None
            # PageSetupWizardProtocols sorts installPageOrder; permitted Awg2 is first.
            # LabelWithButtonType exposes its 40 px arrow rather than its plain Text title.
            return arrows[0][1]

        self.click_node(self.wait(target, 'AmneziaWG protocol control'))
        self.wait(lambda: len(self.fields()) == 1 and self.matching('Install'), 'AmneziaWG port screen')

    def click_saved_server(self):
        def target():
            nodes = self.nodes()
            frames = [node for node in nodes if node.getRole() in (self.api.ROLE_FRAME, self.api.ROLE_WINDOW)]
            buttons = [node for node in nodes if node.getRole() == self.api.ROLE_PUSH_BUTTON]
            if (len(frames) != 1 or len(buttons) != 2 or self.fields() or not self.matching('Servers')
                    or any(node.getRole() == self.api.ROLE_RADIO_BUTTON for node in nodes)):
                return None
            state = json.loads(private_file(SESSION))
            app = state['app']
            require(process_identity(app['pid']) == app['start']
                    and frames[0].getApplication().get_process_id() == app['pid']
                    and os.environ.get('DISPLAY') == state['environment']['DISPLAY'],
                    'server settings require the owned application session')
            frame = frames[0].queryComponent().getExtents(self.api.DESKTOP_COORDS)
            if (frame.x < 0 or frame.y < 0 or frame.width <= 0 or frame.height <= 0
                    or frame.x + frame.width > 1024 or frame.y + frame.height > 900):
                return None
            back, arrows = [], []
            for button in buttons:
                if not all(button.getState().contains(value) for value in
                           (self.api.STATE_SHOWING, self.api.STATE_ENABLED)):
                    return None
                parent = button.get_parent()
                for _ in range(100):
                    if parent is None or parent.getRole() in (self.api.ROLE_FRAME, self.api.ROLE_WINDOW):
                        break
                    parent = parent.get_parent()
                if parent != frames[0]:
                    return None
                rectangle = button.queryComponent().getExtents(self.api.DESKTOP_COORDS)
                if (abs(rectangle.width - 40) > 2 or abs(rectangle.height - 40) > 2
                        or rectangle.x < frame.x or rectangle.y < frame.y
                        or rectangle.x + rectangle.width > frame.x + frame.width
                        or rectangle.y + rectangle.height > frame.y + frame.height):
                    return None
                if abs(rectangle.x - frame.x - 8) <= 2 and abs(rectangle.y - frame.y - 20) <= 2:
                    back.append(button)
                elif abs(rectangle.x - frame.x - frame.width + 56) <= 2 and rectangle.y >= frame.y + 76:
                    arrows.append(button)
            # PageSettingsServersList has one row per saved server; its plain
            # title is unregistered, while LabelWithButtonType exposes the arrow.
            return arrows[0] if len(back) == len(arrows) == 1 else None

        self.click_node(self.wait(target, 'saved native administration server'))
        self.wait(lambda: self.matching('Management'), 'native server management page')

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

    def tab(self, index):
        frames = self.wait(lambda: [node for node in self.nodes()
                                   if node.getRoleName() in ('frame', 'window')], 'main application window')
        frame = frames[0].queryComponent().getExtents(self.api.DESKTOP_COORDS)
        # PageStart.qml 319-428: four icon-only tabs with 96 px side padding.
        x = frame.x + 96 + int((frame.width - 192) * (index + 0.5) / 4)
        y = frame.y + frame.height - 32
        run(['xdotool', 'mousemove', '--sync', str(x), str(y), 'click', '1'], operation='pointer-tab')
        time.sleep(0.5)

    def diagnostics(self):
        # Never dump accessible names/values: they can contain credentials/configs.
        return dict(collections.Counter(node.getRoleName() for node in self.nodes()))

    def installation_evidence(self):
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
        return installation_evidence(nodes)


def setup(gui, args):
    key = private_file(args.key_file).decode()
    require('PRIVATE KEY-----' in key and key.startswith('-----BEGIN '), 'administrative key is not a private key')
    if gui.matching("Let's get started"):
        gui.click("Let's get started", description='setup welcome control')
    else:
        gui.tab(3)
    gui.click_setup_card('Self-hosted VPN')
    gui.wait(lambda: len(gui.fields()) == 3, 'server credential fields')
    # PageSetupWizardCredentials.qml: hostname, username, secret, in that order.
    gui.fill([f'{args.host}:{args.ssh_port}', args.user, key])
    del key
    gui.click(('Continue', 'Start configuring'), description='server credential continuation')
    gui.click_setup_card('Manual', manual=True)
    gui.click('Continue', description='manual installation continuation')
    gui.click_awg()
    gui.fill([str(args.udp_port)])
    gui.click('Install', description='native installation control')
    def ready():
        # Share only becomes usable for a full-access native server connection.
        controls = lambda: all(gui.matching(label) for label in ('Connection', 'Users', 'Share'))
        if controls():
            return True
        if gui.fields() or gui.matching('Installing'):
            return False
        gui.tab(1)
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
                  owned_editable_candidates=0, error_codes=[], predicate='unavailable')
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
        result['error_codes'] = gui.installation_evidence()['error_codes']
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
    gui.share_evidence['stage'] = 'connection'
    gui.tab(1)
    gui.click('Connection', description='native share connection control')
    gui.fill([args.name])
    # PageShare retains the selected native format after an export. Avoid reopening
    # its drawer, which would display both the current value and the same choice.
    if not gui.matching('AmneziaWG native format'):
        gui.share_evidence['stage'] = 'format'
        gui.click(('For the AmneziaVPN app', 'AmneziaWG native format'), description='native export format selector')
        gui.click('AmneziaWG native format', description='native AWG export format')
    gui.share_evidence['stage'] = 'generation'
    gui.click('Share', description='native client generation control')
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
    gui.tab(1)


def revoke(gui, args):
    gui.tab(1)
    gui.click('Users', description='native users control')
    gui.click(args.name, seconds=90, description='native named user control')
    gui.click('Revoke', description='native revocation control')
    gui.click('Continue', description='native revocation confirmation')
    def removed():
        users = gui.matching('Users')
        return (len(users) == 1 and users[0].getRole() == gui.api.ROLE_RADIO_BUTTON
                and users[0].getState().contains(gui.api.STATE_CHECKED)
                and not any(gui.matching(label) for label in ('Revoke', 'Continue', 'Close'))
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
                  data_rows={key: 0 for key in ('scan_title', 'scan_description', 'reboot', 'remove', 'clear', 'reset')},
                  error_codes=[], classification='ownership-or-access-unavailable', target=flags)
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
        labels = {'Check the server for previously installed Amnezia services': 'scan_title',
                  'Add them to the application if they were not displayed': 'scan_description',
                  'Reboot server': 'reboot', 'Remove server from application': 'remove',
                  'Clear server from Amnezia software': 'clear', 'Reset API config': 'reset'}
        for node in nodes:
            try:
                value = control_flags(node)
                if value['defunct'] is not False or not all(value[key] is True for key in ('owned', 'showing', 'enabled', 'in_frame')):
                    continue
                label = node.name.strip()
                if label == 'Management' and node.getRoleName() == 'page tab':
                    tabs.append(value)
                if label in labels:
                    result['data_rows'][labels[label]] += 1
            except Exception:
                pass
        result['management_tabs'] = len(tabs)
        if len(tabs) == 1:
            result['management_checked'], result['management_selected'] = tabs[0]['checked'], tabs[0]['selected']
        result['error_codes'] = installation_evidence(nodes)['error_codes']
        selected = result['management_checked'] is True or result['management_selected'] is True
        result['classification'] = ('selected-management-scan-visible' if selected and
                                    (result['data_rows']['scan_title'] or result['data_rows']['scan_description']) else
                                    'selected-management-scan-missing' if selected else 'management-page-unconfirmed')
    except Exception:
        pass
    return result


def discovery_match(gui, label, key):
    # Record only calls already required by discovery, with the same short circuit.
    try:
        nodes = gui.matching(label)
    except Exception as error:
        gui.discover_evidence['matches'][key] = dict(count=None, failure='matching-failure' if isinstance(error, Failure)
                                                   else 'matching-exception')
        raise
    gui.discover_evidence['matches'][key] = dict(count=min(len(nodes), 3000), failure='none')
    return nodes


def activate_management(gui):
    # Pinned PageSettingsServerInfo.qml binds Management Return/Enter to index 2.
    # Keep one activation within the existing named-control wait budget.
    deadline = time.monotonic() + 30
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


def discover(gui, args):
    gui.discover_evidence = dict(stage='servers-control', matches={})
    gui.share_evidence = gui.discover_evidence  # Existing bounded action receipt, emitted only on failure.
    gui.tab(2)
    gui.click('Servers', description='native servers control')
    # The fixture owns exactly one admin server. No client import adds another.
    gui.discover_evidence['stage'] = 'saved-server'
    gui.click_saved_server()
    gui.discover_evidence['stage'] = 'management-control'
    activate_management(gui)
    # The row title is capped at two elided lines; its complete description
    # belongs to the same stock MouseArea and invokes the same scan handler.
    gui.discover_evidence['stage'] = 'scan-control'
    scan = gui.wait(lambda: discovery_match(gui, 'Check the server for previously installed Amnezia services', 'title')
                    or discovery_match(gui, 'Add them to the application if they were not displayed', 'description'),
                    'native service discovery control')
    gui.discover_evidence['stage'] = 'scan-dispatch'
    gui.click_node(scan[0])
    gui.discover_evidence['stage'] = 'scan-completion'
    gui.wait(lambda: gui.matching(('All installed containers have been added to the application',
                                 'No new installed containers found')), 'native server scan', 120)
    gui.discover_evidence['stage'] = 'scan-close'
    gui.click('Close', description='native server scan close control')
    gui.discover_evidence['stage'] = 'retained-user'
    gui.tab(1)
    gui.click('Users', description='discovered native users control')
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


def discovery_self_check():
    from unittest import mock
    api = SimpleNamespace(ROLE_FRAME=1, ROLE_WINDOW=2, ROLE_PUSH_BUTTON=3, ROLE_TEXT=8,
                          STATE_ENABLED=5, STATE_CHECKED=6, STATE_SHOWING=9,
                          STATE_DEFUNCT=12, STATE_SELECTED=13, STATE_FOCUSABLE=15,
                          STATE_FOCUSED=16, STATE_ACTIVE=17, DESKTOP_COORDS=0)
    session_bytes = b'{"app":{"pid":42,"start":"owned"},"environment":{"DISPLAY":":synthetic"}}'
    for scenario in ('ready', 'focus-after-deadline', 'focus-near-deadline', 'focus-return', 'not-accepted', 'action-exception', 'read-only', 'unknown-match',
                     'focus-unavailable', 'focus-refused', 'focus-exception', 'focus-unconfirmed', 'foreign-management',
                     'inactive-frame', 'ambiguous-management', 'focus-no-selection'):
        state = dict(page='info', clock=0.0, dispatches=0, focus_requests=0, focused=False, keys=0, pointers=0,
                     clock_consumed=False, key_timeouts=[])
        def node(label, role, action=lambda: True, parent=None):
            item = SimpleNamespace(name=label, parent=parent)
            item.getApplication = lambda: SimpleNamespace(get_process_id=lambda: 42)
            item.getRole = lambda: role
            item.getRoleName = lambda: {1: 'frame', 3: 'button', 8: 'text', 14: 'page tab'}[role]
            item.get_parent = lambda: parent
            def contains(flag):
                if (label == 'Management' and flag == 16 and state['focused'] and not state['clock_consumed']
                        and scenario in ('focus-after-deadline', 'focus-near-deadline')):
                    state['clock'] += 31 if scenario == 'focus-after-deadline' else 29.9
                    state['clock_consumed'] = True
                return (flag in (5, 9) or
                flag in (6, 13) and label == 'Management' and state['page'] in ('data', 'scan-complete') or
                flag == 15 and label == 'Management' and scenario != 'focus-unavailable' or
                flag == 16 and label == 'Management' and state['focused'] or
                flag == 17 and role == 1 and scenario != 'inactive-frame')
            item.getState = lambda: SimpleNamespace(contains=contains)
            def focus():
                state['focus_requests'] += 1
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
        controls = [node('Management', 14, management, frame),
                    node('Check the server for previously installed Amnezia services', 8, scan, frame),
                    node('All installed containers have been added to the application', 8, parent=frame),
                    node('Close', 3, parent=frame), node('Servers', 14, parent=frame),
                    node('Users', 14, parent=frame), node('retained-test-user', 8, parent=frame),
                    node('Remove server from application', 8, parent=frame)]
        if scenario.startswith('focus-') or scenario == 'inactive-frame':
            controls[0].queryAction = mock.Mock(side_effect=NotImplementedError('synthetic-private-action'))
        if scenario == 'foreign-management':
            controls[0].getApplication = lambda: SimpleNamespace(get_process_id=lambda: 99)
        def current_nodes():
            valid = {'Management', 'Servers'}
            if state['page'] == 'data':
                valid.add('Remove server from application')
                if scenario != 'read-only':
                    valid.add(controls[1].name)
            if state['page'] == 'scan-complete':
                valid.update((controls[2].name, 'Close'))
            if state['page'] == 'users':
                valid.update(('Users', 'retained-test-user'))
            return [frame] + [item for item in controls if item.name in valid] + (
                [node('Management', 14, management, frame)] if scenario == 'ambiguous-management' else [])
        def matching(labels):
            if labels == controls[1].name and scenario == 'unknown-match':
                raise RuntimeError('synthetic-private-matching')
            labels = (labels,) if isinstance(labels, str) else labels
            return [item for item in current_nodes() if item.name in labels]
        gui = GUI.__new__(GUI)
        gui.api, gui.nodes, gui.matching = api, current_nodes, matching
        gui.click_saved_server = lambda: None  # Isolate the next native boundary.
        gui.tab = lambda index: state.update(page='users') if index == 1 else None
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
                mock.patch.object(time, 'sleep', side_effect=lambda seconds: state.update(clock=state['clock'] + seconds)):
            try:
                discover(gui, SimpleNamespace(name='retained-test-user'))
            except Failure as error:
                expected = {
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
                }.get(scenario, 'GUI timed out waiting for native service discovery control')
                require(scenario not in ('ready', 'focus-return') and str(error) == expected,
                        'Management activation changed its safe failure boundary')
            else:
                require(scenario in ('ready', 'focus-return'), 'unready discovery was accepted')
                require(('native server scan', 120) in waits and ('retained native user after discovery', 90) in waits,
                        'discovery observation changed existing completion waits')
            snapshot = discovery_snapshot(gui)
            if scenario in ('ready', 'focus-return', 'read-only', 'unknown-match'):
                require(('native service discovery control', 30) in waits, 'discovery changed the control wait')
            refused = scenario in ('focus-after-deadline', 'focus-unavailable', 'focus-refused', 'focus-exception', 'focus-unconfirmed', 'foreign-management',
                                   'inactive-frame', 'ambiguous-management')
            require(state['dispatches'] == (0 if refused else 1), 'Management activation was repeated or bypassed refusal')
            require(state['pointers'] == 0, 'Management activation used coordinate input')
            require(state['keys'] == int(scenario in ('focus-return', 'focus-no-selection', 'focus-near-deadline')),
                    'Management key escaped its focused single activation')
            require(state['focus_requests'] == int(scenario in ('focus-return', 'focus-refused',
                    'focus-exception', 'focus-unconfirmed', 'focus-no-selection', 'inactive-frame',
                    'focus-after-deadline', 'focus-near-deadline')),
                    'Management focus was retried or unnecessarily requested')
            if scenario == 'focus-after-deadline':
                require(not state['key_timeouts'], 'expired Management activation dispatched a keyboard command')
            if scenario == 'focus-near-deadline':
                require(len(state['key_timeouts']) == 1 and 0 < state['key_timeouts'][0] <= 0.100001
                        and state['clock'] <= state['activation_deadline'] + 0.000001,
                        'Management keyboard command or settle exceeded its deadline')
            if scenario == 'read-only':
                require(snapshot['classification'] == 'selected-management-scan-missing'
                        and snapshot['data_rows']['remove'] == 1, 'selected page without scan was not distinguished')
            elif scenario in ('not-accepted', 'action-exception'):
                require(snapshot['classification'] == 'management-page-unconfirmed'
                        and gui.discover_evidence['action']['dispatch_error'] == scenario,
                        'failed Management dispatch was not distinguished')
            elif scenario == 'unknown-match':
                require(gui.discover_evidence['matches']['title'] == dict(count=None, failure='matching-exception')
                        and snapshot['classification'] == 'selected-management-scan-visible',
                        'matching failure was mistaken for a missing scan row')
            elif scenario in ('ready', 'focus-return'):
                state['page'] = 'data'
                require(discovery_snapshot(gui)['classification'] == 'selected-management-scan-visible',
                        'ready selected Management page was not distinguished')
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
            require(unavailable['classification'] == 'ownership-or-access-unavailable'
                    and unavailable['management_tabs'] == 0, 'unknown ownership was treated as selected native state')
            evidence = json.dumps(dict(gui.discover_evidence, snapshot=snapshot, unavailable=unavailable))
            require('synthetic-private' not in evidence and 'Management' not in evidence,
                    'discovery diagnostics exposed native labels, body or exception text')
            for item in controls:
                item.queryText.assert_not_called()


def self_check():
    discovery_self_check()
    from unittest import mock
    from types import SimpleNamespace
    dialog = SimpleNamespace(name='', getRoleName=lambda: 'dialog', parent=None)
    frame = SimpleNamespace(name='', getRoleName=lambda: 'frame', parent=None)
    def message(value, parent=dialog):
        return SimpleNamespace(name=value, getRoleName=lambda: 'text', parent=parent,
                               queryText=lambda: SimpleNamespace(characterCount=len(value),
                                   getText=lambda start, end: value[start:end]))
    secret = 'synthetic-private-key-value'
    evidence = installation_evidence([message('ErrorCode: 305. ' + secret), message('Installing'),
                                      message('Connection', frame), message(secret)])
    require(evidence == {'error_codes': [305], 'visible_controls': ['Connection', 'Installing']},
            'native terminal error or progress evidence was lost')
    require(secret not in json.dumps(evidence), 'installation evidence exposed private text')
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
        require(not installation_evidence([message(value, parent)])['error_codes'],
                'unconfirmed or malformed error text became a terminal failure')
    waits = []
    observed = SimpleNamespace(installation_evidence=lambda: {'error_codes': []},
                               matching=lambda label: label == 'Installing')
    def wait(action, description, seconds=30):
        waits.append((description, seconds))
        return action() or require(False, 'synthetic installer made no progress')
    observed.wait = wait
    checks = iter((False, True))
    wait_native_installation(observed, lambda: next(checks))
    require(waits == [('native installer startup', 30), ('native installation completion', 360)],
            'installer observation changed the completion deadline')
    observed.installation_evidence = lambda: {'error_codes': [305]}
    with mock.patch.object(observed, 'matching') as matching:
        try:
            wait_native_installation(observed, lambda: True)
        except Failure as error:
            require(str(error) == 'native installation failed ErrorCode=305',
                    'terminal error lost its safe numeric diagnostic')
        else:
            raise Failure('terminal error was mistaken for installation success')
        require(not matching.called, 'terminal installation error kept driving the GUI')
    observed.matching = lambda label: label == 'Installing'
    states = iter(({'error_codes': []}, {'error_codes': [203]}))
    observed.installation_evidence = lambda: next(states)
    try:
        wait_native_installation(observed, lambda: False)
    except Failure as error:
        require(str(error) == 'native installation failed ErrorCode=203',
                'error after installer progress was replaced by a timeout')
    else:
        raise Failure('installer progress hid a subsequent terminal error')
    gui = GUI.__new__(GUI)
    gui.api = SimpleNamespace(ROLE_FRAME=1, ROLE_WINDOW=2, ROLE_PUSH_BUTTON=3,
                              ROLE_RADIO_BUTTON=4, STATE_ENABLED=5, STATE_CHECKED=6,
                              STATE_EDITABLE=7, ROLE_TEXT=8, STATE_SHOWING=9,
                              STATE_FOCUSABLE=10, STATE_FOCUSED=11, STATE_DEFUNCT=12, DESKTOP_COORDS=0)
    def fixture_node(role, x, y, width, height, checked=False):
        node = mock.Mock(name='synthetic-private-node')
        node.name = ''
        node.getRole.return_value = role
        node.getState.return_value.contains.side_effect = lambda state: state == 5 or (state == 6 and checked)
        node.queryComponent.return_value.getExtents.return_value = SimpleNamespace(
            x=x, y=y, width=width, height=height)
        return node
    frame = fixture_node(1, 0, 0, 380, 800)
    first = fixture_node(3, 16, 335, 348, 103)
    second = fixture_node(3, 16, 454, 348, 95)
    gui.matching = lambda names: []
    gui.wait = lambda action, *args: action() or require(False, 'synthetic layout rejected')
    gui.click_node = mock.Mock()
    with mock.patch.object(time, 'sleep'):
        gui.nodes = lambda: [frame, first, second]
        gui.click_setup_card('Self-hosted VPN')
        gui.click_node.assert_called_once_with(second)
        for nodes in ([frame, first], [frame, first, fixture_node(3, 16, 460, 348, 95)],
                      [frame, first, fixture_node(3, 17, 400, 348, 95)]):
            gui.nodes = lambda: nodes
            gui.click_node.reset_mock()
            try:
                gui.click_setup_card('Self-hosted VPN')
            except Failure:
                pass
            else:
                raise Failure('unexpected wizard layout was accepted')
            require(not gui.click_node.called, 'unexpected wizard layout clicked a control')
        for checked in (False, True):
            automatic = fixture_node(4, 16, 200, 348, 150)
            manual = fixture_node(4, 16, 390, 348, 95, checked=checked)
            gui.nodes = lambda: [frame, automatic, manual]
            try:
                gui.click_setup_card('Manual', manual=True)
            except Failure:
                require(not checked, 'checked manual selection was rejected')
            else:
                require(checked, 'unchecked manual selection was accepted')
    editable = fixture_node(8, 32, 152, 316, 20)
    editable.getState.return_value.contains.side_effect = lambda state: state in (5, 7)
    editable.queryEditableText.side_effect = NotImplementedError('synthetic-private-interface')
    readonly = fixture_node(8, 32, 241, 316, 20)
    gui.nodes = lambda: [frame, readonly, editable]
    require(gui.fields() == [editable], 'editable fields without the EditableText interface were rejected')
    editable.queryEditableText.assert_not_called()
    back = fixture_node(3, 8, 20, 40, 40)
    arrows = [fixture_node(3, 324, y, 40, 40) for y in (274, 395, 508, 621)]
    gui.matching = lambda names: [second] if names == 'Install' else []
    gui.nodes = lambda: [frame, back, *arrows]
    gui.click_node = mock.Mock(side_effect=lambda node: setattr(gui, 'nodes', lambda: [frame, editable]))
    gui.click_awg()
    gui.click_node.assert_called_once_with(arrows[0])
    for nodes in ([frame, back, *arrows, editable], [frame, back, back, *arrows],
                  [frame, back, *arrows, arrows[0]]):
        gui.nodes = lambda: nodes
        gui.click_node.reset_mock()
        try:
            gui.click_awg()
        except Failure:
            pass
        else:
            raise Failure('ambiguous protocol page was accepted')
        require(not gui.click_node.called, 'ambiguous protocol page clicked a control')
    header = 'level\tpage_num\tblock_num\tpar_num\tline_num\tword_num\tleft\ttop\twidth\theight\tconf\ttext\n'
    def ocr_line(number, words, y=20, confidence=95, x=16, block=1, page=1):
        return ''.join(f'5\t{page}\t{block}\t1\t{number}\t{index + 1}\t{x + index * 60}\t{y}\t50\t12\t{confidence}\t{word}\n'
                       for index, word in enumerate(words.split()))
    rectangle = SimpleNamespace(x=0, y=0, width=380, height=800)
    ocr_gui = GUI.__new__(GUI)
    ocr_gui.api = gui.api
    ocr_gui.wait = gui.wait
    samples = ((ocr_line(1, 'Share'), 'share', True),
               (ocr_line(1, 'AmneziaWG native') + ocr_line(2, 'format', y=36), 'AmneziaWG native format', True),
               (ocr_line(1, 'Share') + ocr_line(2, 'Share', y=70), 'Share', False),
               (ocr_line(1, 'Share') + ocr_line(2, 'Share', y=70, confidence=20), 'Share', False),
               (ocr_line(1, 'Share', confidence=79), 'Share', False),
               (ocr_line(1, 'Share', x=370), 'Share', False),
               (ocr_line(1, 'Share VPN Access'), 'Share', False),
               (ocr_line(1, 'AmneziaWG native') + ocr_line(2, 'format', y=100), 'AmneziaWG native format', False),
               (ocr_line(1, 'Check the server for', block=3)
                + ocr_line(1, 'previously installed Amnezia', y=36, block=2)
                + ocr_line(1, 'services', y=52),
                'Check the server for previously installed Amnezia services', True),
               (ocr_line(1, 'AmneziaWG native') + ocr_line(1, 'format', y=36, block=2),
                'AmneziaWG native format', True),
               (ocr_line(1, 'AmneziaWG native') + ocr_line(1, 'format', y=100, block=2),
                'AmneziaWG native format', False),
               (ocr_line(1, 'AmneziaWG native') + ocr_line(1, 'format', y=36, block=2, confidence=79),
                'AmneziaWG native format', False),
               (ocr_line(1, 'AmneziaWG native') + ocr_line(1, 'format', y=36, page=2),
                'AmneziaWG native format', False),
               (ocr_line(1, 'AmneziaWG native') + ocr_line(1, 'format', y=36, block=2)
                + ocr_line(1, 'AmneziaWG native', y=100, block=3)
                + ocr_line(1, 'format', y=116, block=4), 'AmneziaWG native format', False))
    with mock.patch.object(time, 'sleep'), mock.patch.object(sys.modules[__name__], 'run') as click:
        for rows, target, accepted in samples:
            ocr_gui.matching = lambda names: visible_text(header + rows, (target,), rectangle)
            click.reset_mock()
            try:
                ocr_gui.click(target)
            except Failure:
                require(not accepted, 'valid visible phrase was rejected')
            else:
                require(accepted, 'ambiguous or unsafe visible phrase was accepted')
            require(click.called == accepted, 'unsafe visible phrase clicked a control')
    offset_frame = SimpleNamespace(x=121, y=83, width=100, height=90)
    scaled = visible_text(header + ocr_line(1, 'Share', x=17, y=21), ('Share',), offset_frame, scale=2)
    require(len(scaled) == 1 and vars(scaled[0].rectangle) == dict(x=129, y=93, width=26, height=7),
            'scaled OCR did not preserve offset frame bounds and odd-pixel coverage')
    for rows in (ocr_line(1, 'Share', x=151), ocr_line(1, 'Share', y=169),
                 ocr_line(1, 'Share', confidence=79),
                 ocr_line(1, 'Share') + ocr_line(2, 'Share', y=70)):
        require(not visible_text(header + rows, ('Share',), offset_frame, scale=2),
                'scaled OCR accepted an unsafe or ambiguous phrase')
    for already_ready in (False, True):
        wizard = mock.Mock()
        wizard.installation_evidence.return_value = {'error_codes': [], 'visible_controls': []}
        stage = ['credentials']
        wizard.matching.side_effect = lambda label: [object()] if (
            label == "Let's get started" or (stage[0] == 'installed' and already_ready
                                             and label in ('Connection', 'Users', 'Share'))) else []
        wizard.fields.side_effect = lambda: [object()] * (3 if stage[0] == 'credentials' else 1)
        wizard.click_awg.side_effect = lambda: stage.__setitem__(0, 'port')
        wizard.click.side_effect = lambda label, **kwargs: stage.__setitem__(0, 'installed') if label == 'Install' else None
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
    for scenario in ('removed', 'missing page', 'wrong tab', 'wrong role',
                     'details open', 'confirmation open', 'error open', 'retained row'):
        revocation = mock.Mock()
        revocation.api = gui.api
        users = fixture_node(4 if scenario != 'wrong role' else 1, 16, 300, 174, 56,
                             checked=scenario != 'wrong tab')
        present = {'Users': [users]} if scenario != 'missing page' else {}
        for case, label in (('details open', 'Revoke'), ('confirmation open', 'Continue'),
                            ('error open', 'Close'), ('retained row', 'synthetic-private-client')):
            if scenario == case:
                present[label] = [object()]
        revocation.matching.side_effect = lambda label: present.get(label, [])
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
    click_gui.matching = lambda names: [button, dialog]
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
        export_gui.matching = lambda names: [outgoing, current]
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
        server_frame = fixture_node(1, 0, 0, 380, 680)
        server_frame.getApplication.return_value.get_process_id.return_value = 42
        back = fixture_node(3, 8, 20, 40, 40)
        arrow = fixture_node(3, 324, 138, 40, 40)
        off_frame = fixture_node(3, 324, 660, 40, 40)
        foreign = fixture_node(3, 324, 138, 40, 40)
        for node in (back, arrow, off_frame, foreign):
            node.get_parent.return_value = server_frame
            node.getState.return_value.contains.side_effect = lambda value: value in (5, 9)
        foreign.get_parent.return_value = dialog
        gui.fields = lambda: []
        gui.matching = lambda label: [object()] if label in ('Servers', 'Management') else []
        gui.click_node = mock.Mock()
        for nodes, accepted in (([server_frame, back, arrow], True),
                                ([server_frame, back, arrow, arrow], False),
                                ([server_frame, back], False),
                                ([server_frame, back, off_frame], False),
                                ([server_frame, back, foreign], False)):
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
                gui.click_node.assert_called_once_with(arrow)
        gui.nodes = lambda: [server_frame, back, arrow]
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
                      'pointer-click', 'pointer-tab', 'frame-capture', 'ocr',
                      'clipboard-write', 'clipboard-clear', 'keyboard-input'):
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
                if args.mode == 'setup':
                    print('native_app_installation_evidence=' + json.dumps(gui.installation_evidence(), sort_keys=True),
                          file=sys.stderr)
                if args.mode == 'share':
                    print('native_app_share_evidence=' + json.dumps(share_diagnostics(gui), sort_keys=True),
                          file=sys.stderr)
            except Exception:
                pass
            if args.mode == 'discover':
                print('native_app_discover_evidence=' + json.dumps(dict(
                    getattr(gui, 'discover_evidence', {}), snapshot=discovery_snapshot(gui)), sort_keys=True),
                      file=sys.stderr)
        return 1


if __name__ == '__main__':
    sys.exit(main())
