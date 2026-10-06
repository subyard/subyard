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
    require(operation in ('external', 'apt-update', 'apt-install', 'installer-help', 'installer-install'),
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
                    capture=True, timeout=5)
        require(len(image) <= 12582912, 'OCR frame capture is too large')
        tsv = run(['tesseract', 'stdin', 'stdout', '-l', 'eng', '--psm', '11', 'tsv'],
                  capture=True, input_data=image, timeout=5)
        require(len(tsv) <= 262144, 'OCR output is too large')
        return visible_text(tsv.decode(), names, frame, scale=2)

    def click_node(self, node):
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
            # Qt can replace the target before replying. Dispatch once; the
            # caller's next-screen or output postcondition verifies delivery.
            try:
                selected[0].doAction(selected[1])
            except Exception:
                pass
            return
        rectangle = node.queryComponent().getExtents(self.api.DESKTOP_COORDS)
        require(rectangle.width > 0 and rectangle.height > 0, 'GUI target has no visible geometry')
        run(['xdotool', 'mousemove', '--sync', str(rectangle.x + rectangle.width // 2),
             str(rectangle.y + rectangle.height // 2), 'click', '1'])

    def click(self, names, seconds=30, description='a named control'):
        nodes = self.wait(lambda: self.matching(names), description, seconds)
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
    # Cancel is shown only while the server is busy, not during an ordinary install.
    gui.wait(lambda: gui.matching('Installing') or ready(), 'native installer startup')
    gui.wait(ready, 'native installation completion', 360)


def export_controls(gui, location=False, accepting=False):
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
        return None
    button = buttons[0]
    dialog = frame(button)
    if dialog is None:
        return None
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
        return None
    field = fields[0]
    field.queryText()
    bounds = dialog.queryComponent().getExtents(gui.api.DESKTOP_COORDS)
    for node in (button, field):
        rectangle = node.queryComponent().getExtents(gui.api.DESKTOP_COORDS)
        x, y = rectangle.x + rectangle.width // 2, rectangle.y + rectangle.height // 2
        if (rectangle.width <= 0 or rectangle.height <= 0 or not 0 <= x < 1024 or not 0 <= y < 900
                or not bounds.x <= x < bounds.x + bounds.width
                or not bounds.y <= y < bounds.y + bounds.height):
            return None
    return button, field


def share(gui, args):
    output = args.output_file.absolute()
    private_dir(output.parent)
    require(not output.exists() and not output.is_symlink(), 'refusing to overwrite a client export')
    require(output.suffix == '.conf', 'native AWG export needs a .conf filename')
    gui.tab(1)
    gui.click('Connection', description='native share connection control')
    gui.fill([args.name])
    # PageShare retains the selected native format after an export. Avoid reopening
    # its drawer, which would display both the current value and the same choice.
    if not gui.matching('AmneziaWG native format'):
        gui.click(('For the AmneziaVPN app', 'AmneziaWG native format'), description='native export format selector')
        gui.click('AmneziaWG native format', description='native AWG export format')
    gui.click('Share', description='native client generation control')
    gui.wait(lambda: gui.matching('Show connection settings'), 'native client generation', 90)
    gui.click('Share', description='native export sharing control')
    # The native dialog's default basename is platform-dependent. Select its
    # editable filename field beside the actual accept button, not the title.
    gui.wait(lambda: export_controls(gui), 'native export dialog')
    # Qt resolves filename text relative to currentFolder. Navigate with its
    # absolute-path breadcrumb before entering a basename in the filename field.
    gui.key('ctrl+l')
    _, directory = gui.wait(lambda: export_controls(gui, location=True), 'native export directory input')
    gui.fill_node(directory, str(output.parent))
    gui.key('Return')
    button, field = gui.wait(lambda: export_controls(gui), 'native export filename input')
    gui.fill_node(field, output.name)
    # Accessible Press does not move focus; Qt commits selectedFile only when
    # filename editing finishes. Tab commits before the single accept action.
    gui.key('Tab')
    gui.wait(lambda: not field.getState().contains(gui.api.STATE_FOCUSED), 'native export filename commit')
    button, _ = gui.wait(lambda: export_controls(gui, accepting=True), 'native export save control')
    gui.click_node(button)
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


def discover(gui, args):
    gui.tab(2)
    gui.click('Servers', description='native servers control')
    # The fixture owns exactly one admin server. No client import adds another.
    gui.click_saved_server()
    gui.click('Management', description='native server management control')
    # The row title is capped at two elided lines; its complete description
    # belongs to the same stock MouseArea and invokes the same scan handler.
    scan = gui.wait(lambda: gui.matching('Check the server for previously installed Amnezia services')
                    or gui.matching('Add them to the application if they were not displayed'),
                    'native service discovery control')
    gui.click_node(scan[0])
    gui.wait(lambda: gui.matching(('All installed containers have been added to the application',
                                 'No new installed containers found')), 'native server scan', 120)
    gui.click('Close', description='native server scan close control')
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


def self_check():
    from unittest import mock
    from types import SimpleNamespace
    gui = GUI.__new__(GUI)
    gui.api = SimpleNamespace(ROLE_FRAME=1, ROLE_WINDOW=2, ROLE_PUSH_BUTTON=3,
                              ROLE_RADIO_BUTTON=4, STATE_ENABLED=5, STATE_CHECKED=6,
                              STATE_EDITABLE=7, ROLE_TEXT=8, STATE_SHOWING=9,
                              STATE_FOCUSABLE=10, STATE_FOCUSED=11, DESKTOP_COORDS=0)
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
    action = button.queryAction.return_value
    action.nActions = 1
    for action_name, accepted in (('Press', True), ('Press', False),
                                  ('Press', RuntimeError('synthetic-private-value')), ('SetFocus', True)):
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
    button.queryAction.side_effect = RuntimeError('synthetic-private-value')
    with mock.patch.object(sys.modules[__name__], 'run') as geometry:
        GUI.click_node(gui, button)
    require(geometry.call_count == 1, 'unavailable native action lost the geometry fallback')
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
    session = json.dumps({'app': {'pid': 42, 'start': 'synthetic-start'},
                          'environment': {'DISPLAY': ':synthetic'}}).encode()
    with mock.patch.object(sys.modules[__name__], 'private_file', return_value=session), \
            mock.patch.object(sys.modules[__name__], 'process_identity', return_value='synthetic-start'), \
            mock.patch.dict(os.environ, {'DISPLAY': ':synthetic'}):
        gui.nodes = lambda: [dialog, save, filename, filetype]
        gui.fields = lambda: [filename, filetype]
        require(export_controls(gui) == (save, filename),
                'native export selected its title or disabled filter instead of filename and accept button')
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
    for operation in ('external', 'apt-update', 'apt-install', 'installer-help', 'installer-install'):
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
            except Exception:
                pass
        return 1


if __name__ == '__main__':
    sys.exit(main())
