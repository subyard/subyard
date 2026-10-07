#!/usr/bin/python3
"""Marker-owned virtual-interface replacement inside a disposable guest."""
import contextlib
import ctypes
import fcntl
import json
import os
import pathlib
import signal
import stat
import subprocess
import sys
import time

ADDRESSES = ('198.18.0.1', '198.18.0.2')
CHECKPOINTS = {
    'operation': {'setup', 'namespaces', 'links', 'listener', 'client', 'phase', 'remove', 'restore', 'assertions', 'receipt'},
    'cleanup': {'lock', 'supervisor', 'namespaces', 'links', 'terminate', 'kill', 'processes', 'delete_links', 'delete_namespaces', 'save'},
    'wait': {'client', 'listener'},
}
EXCEPTIONS = {ValueError: 'ValueError', OSError: 'OSError', TypeError: 'TypeError', KeyError: 'KeyError',
              IndexError: 'IndexError', subprocess.TimeoutExpired: 'TimeoutExpired',
              subprocess.CalledProcessError: 'CalledProcessError', SystemExit: 'SystemExit'}


def diagnostic(seam, checkpoint, error, child=None):
    if checkpoint not in CHECKPOINTS.get(seam, set()):
        raise ValueError('invalid interface diagnostic checkpoint')
    exception = next((name for kind, name in EXCEPTIONS.items() if isinstance(error, kind)), 'Other')
    status = str(child) if type(child) is int and -64 <= child <= 255 else 'unknown'
    print(f'veranda-native-interface: failure={seam} checkpoint={checkpoint} exception={exception} child={status}', flush=True)


def link_identity(value, name, alias):
    if not isinstance(value, list) or len(value) != 1 or not isinstance(value[0], dict):
        raise ValueError('ambiguous owned link')
    link = value[0]
    if (link.get('ifname') != name or link.get('ifalias') != alias
            or link.get('linkinfo', {}).get('info_kind') != 'veth'
            or type(link.get('ifindex')) is not int or link['ifindex'] <= 0
            or type(link.get('link_index')) is not int or link['link_index'] <= 0):
        raise ValueError('owned link identity changed')
    return {'index': link['ifindex'], 'peer': link['link_index']}


def receipt(value):
    if not isinstance(value, dict):
        raise ValueError('invalid interface replacement proof')
    fields = {'old_client_index', 'old_owner_index', 'new_client_index', 'new_owner_index',
              'client_rx_packets', 'client_tx_packets', 'owner_rx_packets', 'owner_tx_packets'}
    if (set(value) != fields | {'schema_version', 'interface_replacement', 'cleanup'}
            or type(value['schema_version']) is not int or value['schema_version'] != 1 or value['interface_replacement'] != 'passed'
            or value['cleanup'] != 'verified'
            or any(type(value[key]) is not int or not 0 < value[key] <= 18446744073709551615 for key in fields)
            or value['old_client_index'] == value['new_client_index']
            or value['old_owner_index'] == value['new_owner_index']):
        raise ValueError('incomplete interface replacement proof')
    return value


def namespace_processes(all_processes, job, namespaces, previous):
    owned = {job}
    for _ in range(128):
        added = {pid for pid, value in all_processes.items() if value['parent'] in owned}
        if added <= owned:
            break
        owned |= added
    known = {(value['pid'], value['start']) for value in previous}
    occupants = [value for value in all_processes.values() if value['namespace'] in namespaces]
    if len(occupants) > 128 or any(value['pid'] not in owned and (value['pid'], value['start']) not in known for value in occupants):
        raise ValueError('unknown namespace process')
    return occupants


class Fixture:
    def __init__(self, context, native, sshd, gid, groups, read):
        self.root, self.uid, self.port, tag, _, _, _ = context
        if self.uid == 0:
            raise ValueError('interface client requires the normal fixture UID')
        self.names = ('veranda-client-' + tag[-24:], 'veranda-owner-' + tag[-24:])
        self.links = ('vc' + tag[-10:], 'vo' + tag[-10:])
        self.alias = 'subyard-veranda-interface-v1-' + tag[-24:]
        self.native, self.sshd, self.gid, self.groups, self.read = native, sshd, gid, groups, read
        self.path = self.root / 'interface.state'
        self.state = None
        self.limit = time.monotonic() + 5

    @contextlib.contextmanager
    def locked(self):
        fd = os.open(self.root / 'interface.lock', os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
        try:
            info = os.fstat(fd)
            if not stat.S_ISREG(info.st_mode) or info.st_uid != 0 or info.st_nlink != 1 or stat.S_IMODE(info.st_mode) != 0o600:
                raise ValueError('unsafe interface lock')
            until = time.monotonic() + 5
            while True:
                try:
                    fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                    break
                except BlockingIOError:
                    if time.monotonic() >= until:
                        raise ValueError('interface lock deadline')
                    time.sleep(0.025)
            if self.path.exists():
                self.state = json.loads(self.read(self.path, 0, 65536))
                if self.state['marker'] != self.alias or self.state['names'] != list(self.names):
                    raise ValueError('interface state ownership mismatch')
            yield
        finally:
            os.close(fd)

    def write(self, name, value, uid=0):
        with (self.root / name).open('x') as output:
            os.fchmod(output.fileno(), 0o600)
            os.fchown(output.fileno(), uid, -1)
            output.write(value)
            output.flush()
            os.fsync(output.fileno())

    def save(self):
        self.write('interface.state.tmp', json.dumps(self.state, separators=(',', ':')))
        os.replace(self.root / 'interface.state.tmp', self.path)

    def ip(self, *args, namespace=None, numeric=False):
        command = ['/usr/sbin/ip']
        if namespace is not None:
            self.namespace(namespace)
            command += ['-n', self.names[namespace]]
        if numeric:
            command += ['-j', '-s', '-d']
        remaining = self.limit - time.monotonic()
        if remaining <= 0:
            raise ValueError('interface command deadline')
        result = subprocess.run(command + list(args), stdout=subprocess.PIPE,
                                stderr=subprocess.DEVNULL, timeout=min(2, remaining))
        if result.returncode or len(result.stdout) > 65536:
            raise ValueError('owned interface command failed')
        return json.loads(result.stdout) if numeric else None

    def namespace(self, side):
        path = pathlib.Path('/run/netns') / self.names[side]
        info = path.lstat()
        if (not stat.S_ISREG(info.st_mode) or info.st_uid != 0
                or [info.st_dev, info.st_ino] != self.state['namespaces'][side]):
            raise ValueError('network namespace identity changed')
        return path

    def links_now(self):
        result = []
        for side in range(2):
            value = self.ip('link', 'show', namespace=side, numeric=True)
            if {link['ifname'] for link in value} != {'lo', self.links[side]}:
                raise ValueError('unexpected namespace interface')
            link = next(link for link in value if link['ifname'] == self.links[side])
            result.append(link_identity([link], self.links[side], self.alias))
        if result[0]['peer'] != result[1]['index'] or result[1]['peer'] != result[0]['index']:
            raise ValueError('owned interface peers changed')
        if self.state['links'] is not None and result != self.state['links']:
            raise ValueError('owned interface generation changed')
        return result

    def create_links(self):
        for side in range(2):
            if any(link['ifname'] != 'lo' for link in self.ip('link', 'show', namespace=side, numeric=True)):
                raise ValueError('interface replacement namespace was not empty')
        self.ip('link', 'add', self.links[0], 'netns', self.names[0], 'type', 'veth',
                'peer', 'name', self.links[1], 'netns', self.names[1])
        for side in range(2):
            self.ip('link', 'set', 'dev', self.links[side], 'alias', self.alias, namespace=side)
            self.ip('address', 'add', ADDRESSES[side] + '/30', 'dev', self.links[side], namespace=side)
            self.ip('link', 'set', 'dev', self.links[side], 'up', namespace=side)
        self.state['links'] = self.links_now()
        self.save()

    def enter(self, side):
        path = self.namespace(side)

        def child():
            fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
            try:
                info = os.fstat(fd)
                if [info.st_dev, info.st_ino] != self.state['namespaces'][side]:
                    raise ValueError('opened namespace identity changed')
                if ctypes.CDLL(None, use_errno=True).setns(fd, 0x40000000):
                    raise OSError('network namespace entry failed')
            finally:
                os.close(fd)
            os.setgroups(self.groups)
            os.setgid(self.gid)
            os.setuid(self.uid)
        return child

    def process(self, pid):
        entry = pathlib.Path('/proc', str(pid))
        fields = (entry / 'stat').read_text().rsplit(')', 1)[1].split()
        net = (entry / 'ns/net').stat()
        return {'pid': pid, 'start': fields[19], 'uid': entry.stat().st_uid,
                'namespace': [net.st_dev, net.st_ino], 'parent': int(fields[1])}

    def listener(self):
        expected = self.state['listener']
        current = self.process(expected['pid'])
        if current != expected or current['uid'] != self.uid or current['namespace'] != self.state['namespaces'][1]:
            raise ValueError('interface listener identity changed')
        entry = pathlib.Path('/proc', str(current['pid']))
        if (entry / 'exe').resolve() != pathlib.Path(self.sshd).resolve():
            raise ValueError('interface listener executable changed')
        sockets = set()
        for fd in (entry / 'fd').iterdir():
            try:
                target = os.readlink(fd)
            except FileNotFoundError:
                continue
            if target.startswith('socket:[') and target.endswith(']'):
                sockets.add(target[8:-1])
        values = [line.split() for line in (entry / 'net/tcp').read_text().splitlines()[1:]]
        owned = {fields[9] for fields in values if fields[1] == f'020012C6:{self.port:04X}' and fields[3] == '0A' and fields[9] in sockets}
        if len(owned) != 1 or (self.state.get('listener_socket') is not None and sorted(owned) != self.state['listener_socket']):
            raise ValueError('interface listener socket changed')
        return sorted(owned)

    def control(self, action):
        print('native-stage: ssh.interface.' + action, flush=True)
        payload = 'interface-' + action + '-v1\n'
        if self.read(self.root / ('interface.' + action + '.request'), self.uid, 128) != payload.encode():
            raise ValueError('invalid interface request')
        self.listener()
        if action == 'remove':
            self.links_now()
            proof = {}
            for side, label in enumerate(['client', 'owner']):
                route = self.ip('route', 'get', ADDRESSES[1 - side], namespace=side, numeric=True)
                if len(route) != 1 or route[0].get('dev') != self.links[side] or route[0].get('prefsrc') != ADDRESSES[side]:
                    raise ValueError('interface route proof failed')
                link = self.ip('link', 'show', 'dev', self.links[side], namespace=side, numeric=True)[0]
                stats = link.get('stats64', link.get('stats', {}))
                for direction in ['rx', 'tx']:
                    count = stats[direction]['packets']
                    if type(count) is not int or not 0 < count <= 18446744073709551615:
                        raise ValueError('interface traffic not observed')
                    proof[label + '_' + direction + '_packets'] = count
                proof['old_' + label + '_index'] = self.state['links'][side]['index']
            self.state['proof'] = proof
            self.save()
            self.ip('link', 'delete', 'dev', self.links[0], namespace=0)
            for side in range(2):
                if any(link['ifname'] != 'lo' for link in self.ip('link', 'show', namespace=side, numeric=True)):
                    raise ValueError('old interface remained')
            self.state['links'] = None
            self.save()
        elif action == 'restore':
            if self.state['links'] is not None or not self.state.get('proof'):
                raise ValueError('interface replacement baseline missing')
            self.create_links()
            self.listener()
            for side, label in enumerate(['client', 'owner']):
                new = self.state['links'][side]['index']
                if new == self.state['proof']['old_' + label + '_index']:
                    raise ValueError('interface generation was reused')
                self.state['proof']['new_' + label + '_index'] = new
            self.save()
        else:
            raise ValueError('invalid interface action')
        self.write('interface.' + action + '.done', payload, self.uid)
        (self.root / ('interface.' + action + '.request')).unlink()

    def remember(self):
        all_processes = {}
        for path in pathlib.Path('/proc').iterdir():
            if not path.name.isdigit():
                continue
            try:
                all_processes[int(path.name)] = self.process(int(path.name))
            except (OSError, ValueError, IndexError):
                pass
        occupants = namespace_processes(all_processes, self.state['job']['pid'],
                                       self.state['namespaces'], self.state.get('processes', []))
        if any(value['uid'] != self.uid for value in occupants):
            raise ValueError('unknown namespace process')
        self.state['processes'] = occupants
        self.save()
        return occupants

    def cleanup(self):
        if self.state is None or self.state.get('cleaned'):
            return
        self.limit = time.monotonic() + 5
        self.checkpoint = 'namespaces'
        for side, expected in enumerate(self.state['namespaces']):
            if expected is not None:
                self.namespace(side)
            elif (pathlib.Path('/run/netns') / self.names[side]).exists():
                raise ValueError('unclaimed namespace collision')
        # Do not delete any namespace containing an unrecognized interface.
        self.checkpoint = 'links'
        if self.state['links'] is not None:
            self.links_now()
        else:
            for side, expected in enumerate(self.state['namespaces']):
                if expected is not None:
                    if any(link['ifname'] != 'lo' for link in self.ip('link', 'show', namespace=side, numeric=True)):
                        raise ValueError('unrecognized namespace interface')
        for sig in [signal.SIGTERM, signal.SIGKILL]:
            self.checkpoint = 'terminate' if sig == signal.SIGTERM else 'kill'
            for expected in self.remember():
                try:
                    if self.process(expected['pid']) == expected:
                        os.kill(expected['pid'], sig)
                except FileNotFoundError:
                    pass
            time.sleep(0.1)
        self.checkpoint = 'processes'
        if self.remember():
            raise ValueError('namespace process cleanup failed')
        if self.state['links'] is not None:
            self.checkpoint = 'delete_links'
            self.links_now()
            self.ip('link', 'delete', 'dev', self.links[0], namespace=0)
            self.state['links'] = None
            self.save()
        self.checkpoint = 'delete_namespaces'
        for side in range(2):
            if self.state['namespaces'][side] is not None:
                self.namespace(side)
                self.ip('netns', 'delete', self.names[side])
                if (pathlib.Path('/run/netns') / self.names[side]).exists():
                    raise ValueError('namespace cleanup failed')
                self.state['namespaces'][side] = None
                self.save()
        self.checkpoint = 'save'
        self.state['cleaned'] = True
        self.save()

    def run(self):
        until = time.monotonic() + 60
        listener = client = None
        passed = False
        owns_job = False
        primary_error = None
        self.checkpoint = 'setup'
        try:
            print('native-stage: ssh.interface.setup', flush=True)
            with self.locked():
                if self.state is not None:
                    raise ValueError('interface fixture collision')
                self.state = {'marker': self.alias, 'names': list(self.names), 'namespaces': [None, None],
                              'links': None, 'job': self.process(os.getpid()), 'processes': []}
                self.write('interface.state', json.dumps(self.state, separators=(',', ':')))
                owns_job = True
                if ctypes.CDLL(None).prctl(36, 1, 0, 0, 0):
                    raise ValueError('interface child ownership unavailable')
                signal.signal(signal.SIGTERM, lambda *_: sys.exit(143))
                self.limit = time.monotonic() + 10
                self.checkpoint = 'namespaces'
                for side in range(2):
                    path = pathlib.Path('/run/netns') / self.names[side]
                    if path.exists() or path.is_symlink():
                        raise ValueError('network namespace collision')
                    self.ip('netns', 'add', self.names[side])
                    info = path.lstat()
                    self.state['namespaces'][side] = [info.st_dev, info.st_ino]
                    self.save()
                    self.ip('link', 'set', 'dev', 'lo', 'up', namespace=side)
                self.checkpoint = 'links'
                self.create_links()
                self.checkpoint = 'listener'
                config = self.read(self.root / 'owner.conf', self.uid, 65536).decode()
                if config.count('ListenAddress 127.0.0.1\n') != 1:
                    raise ValueError('interface listener configuration changed')
                self.write('interface.owner.conf', config.replace('ListenAddress 127.0.0.1\n', 'ListenAddress 198.18.0.2\n'), self.uid)
                self.write('interface.owner.log', '', self.uid)
                with (self.root / 'interface.owner.log').open('ab') as log:
                    listener = subprocess.Popen([self.sshd, '-D', '-e', '-f', str(self.root / 'interface.owner.conf')],
                                                stdout=log, stderr=log, preexec_fn=self.enter(1), start_new_session=True)
                self.state['listener'] = self.process(listener.pid)
                while True:
                    try:
                        self.state['listener_socket'] = self.listener()
                        break
                    except (OSError, ValueError):
                        if listener.poll() is not None or time.monotonic() >= self.limit:
                            raise ValueError('interface listener readiness deadline')
                        time.sleep(0.025)
                self.save()
                environment = {'PATH': str(self.root / 'bin') + ':/usr/local/bin:/usr/bin:/bin',
                               'SSH_AUTH_SOCK': str(self.root / 'agent.sock'),
                               'VERANDA_TEST_NATIVE_CONTROL_ROOT': str(self.root), 'VERANDA_TEST_DISPOSABLE_OWNER': '1'}
                self.checkpoint = 'client'
                client = subprocess.Popen([self.native, 'native_interface_recovery', '--ignored', '--nocapture'],
                                          env=environment, preexec_fn=self.enter(0), start_new_session=True)
                self.remember()
            self.limit = min(until - 5, time.monotonic() + 45)
            removed = restored = False
            while client.poll() is None:
                self.checkpoint = 'phase'
                if time.monotonic() >= self.limit:
                    raise ValueError('interface phase deadline')
                for action, ready in [('remove', not removed), ('restore', removed and not restored)]:
                    if ready and (self.root / ('interface.' + action + '.request')).exists():
                        self.checkpoint = action
                        with self.locked():
                            self.control(action)
                        if action == 'remove':
                            removed = True
                        else:
                            restored = True
                time.sleep(0.025)
            self.checkpoint = 'assertions'
            if client.returncode != 0 or not restored or self.read(self.root / 'interface.finish.request', self.uid, 128) != b'interface-finish-v1\n':
                raise ValueError('interface native assertions incomplete')
            passed = True
        except BaseException as error:
            primary_error = error
            diagnostic('operation', self.checkpoint, error, client.returncode if client is not None else None)
            raise
        finally:
            cleanup_failed = False
            if owns_job:
                print('native-stage: ssh.interface.cleanup', flush=True)
                self.checkpoint = 'lock'
                try:
                    with self.locked():
                        self.cleanup()
                except BaseException as error:
                    diagnostic('cleanup', self.checkpoint, error, client.returncode if client is not None else None)
                    cleanup_failed = True
                    if primary_error is None:
                        raise
            if not cleanup_failed:
                for name, child in [('client', client), ('listener', listener)]:
                    if child is not None:
                        try:
                            child.wait(timeout=1)
                        except BaseException as error:
                            diagnostic('wait', name, error, child.returncode)
                            if primary_error is None:
                                raise
                            break
        if passed:
            try:
                value = receipt({'schema_version': 1, 'interface_replacement': 'passed', 'cleanup': 'verified', **self.state['proof']})
                self.write('interface.proof', json.dumps(value, separators=(',', ':')), self.uid)
                self.write('interface.run.done', 'interface-run-v1\n', self.uid)
                print('native-stage: ssh.interface.finish', flush=True)
            except BaseException as error:
                diagnostic('operation', 'receipt', error, client.returncode)
                raise


def main(action, context, native, sshd, gid, groups, read):
    fixture = Fixture(context, native, sshd, gid, groups, read)
    if action == 'interface-run':
        if read(fixture.root / 'interface.run.request', fixture.uid, 128) != b'interface-run-v1\n':
            raise ValueError('invalid interface segment request')
        fixture.run()
    elif action == 'interface-cleanup':
        fixture.checkpoint = 'lock'
        try:
            with fixture.locked():
                if fixture.state is None or fixture.state.get('cleaned'):
                    return
                fixture.checkpoint = 'supervisor'
                fixture.remember()
                expected = fixture.state['job']
            try:
                if fixture.process(expected['pid']) == expected:
                    entry = pathlib.Path('/proc', str(expected['pid']))
                    if expected['uid'] != 0 or (entry / 'exe').resolve() != pathlib.Path(sys.executable).resolve() or str(fixture.root / 'bin/packet-loss').encode() not in (entry / 'cmdline').read_bytes().split(b'\0'):
                        raise ValueError('interface supervisor identity changed')
                    os.kill(expected['pid'], signal.SIGTERM)
                    until = time.monotonic() + 5
                    while fixture.process(expected['pid']) == expected:
                        if time.monotonic() >= until:
                            os.kill(expected['pid'], signal.SIGKILL)
                            break
                        time.sleep(0.025)
            except FileNotFoundError:
                pass
            fixture.checkpoint = 'lock'
            with fixture.locked():
                fixture.cleanup()
        except BaseException as error:
            diagnostic('cleanup', fixture.checkpoint, error)
            raise
    else:
        raise ValueError('invalid interface helper action')


if __name__ == '__main__' and sys.argv[1:] == ['--self-test']:
    import io
    link = {'ifname': 'owned', 'ifalias': 'marker', 'ifindex': 3, 'link_index': 4, 'linkinfo': {'info_kind': 'veth'}}
    assert link_identity([link], 'owned', 'marker') == {'index': 3, 'peer': 4}
    for key, bad in [('ifalias', 'foreign'), ('ifindex', True), ('link_index', 0), ('linkinfo', {'info_kind': 'dummy'})]:
        try:
            link_identity([{**link, key: bad}], 'owned', 'marker')
        except ValueError:
            pass
        else:
            raise AssertionError('unsafe interface ownership accepted')
    fixture = Fixture.__new__(Fixture)
    fixture.names = fixture.links = ('client', 'owner')
    fixture.alias, fixture.state = 'marker', {'links': None}
    fixture.namespace = lambda _: None
    fixture.limit = time.monotonic() + 5
    def observed_links(command, **_):
        side = fixture.names.index(command[command.index('-n') + 1])
        observed = {**link, 'ifname': fixture.links[side], 'ifindex': 3 + side, 'link_index': 4 - side}
        observed.pop('linkinfo')
        if ('-d' in command or '-details' in command) and kind is not None:
            observed['linkinfo'] = {'info_kind': kind}
        return subprocess.CompletedProcess(command, 0, json.dumps([{'ifname': 'lo'}, observed]).encode())
    original_run = subprocess.run
    try:
        subprocess.run = observed_links
        kind = 'veth'
        assert fixture.links_now() == [{'index': 3, 'peer': 4}, {'index': 4, 'peer': 3}]
        for kind in [None, 'dummy']:
            try:
                fixture.links_now()
            except ValueError:
                pass
            else:
                raise AssertionError('missing or foreign observed link kind accepted')
    finally:
        subprocess.run = original_run
    value = {'schema_version': 1, 'interface_replacement': 'passed', 'cleanup': 'verified',
             'old_client_index': 3, 'old_owner_index': 4, 'new_client_index': 5, 'new_owner_index': 6,
             'client_rx_packets': 10, 'client_tx_packets': 11, 'owner_rx_packets': 11, 'owner_tx_packets': 10}
    assert receipt(value) == value
    for key, bad in [('schema_version', True), ('new_client_index', 3), ('new_owner_index', 4), ('client_rx_packets', 0),
                     ('owner_tx_packets', True), ('cleanup', 'unknown'), ('client_tx_packets', 18446744073709551616)]:
        try:
            receipt({**value, key: bad})
        except ValueError:
            pass
        else:
            raise AssertionError('incomplete interface proof accepted')
    for bad in [None, [], {key: item for key, item in value.items() if key != 'cleanup'}, {**value, 'foreign': 'unknown'}]:
        try:
            receipt(bad)
        except ValueError:
            pass
        else:
            raise AssertionError('malformed interface receipt accepted')
    process = {'pid': 10, 'parent': 9, 'start': '123', 'namespace': [1, 2], 'uid': 1000}
    assert namespace_processes({10: process}, 9, [[1, 2]], []) == [process]
    assert namespace_processes({10: {**process, 'parent': 1}}, 9, [[1, 2]], [process])
    for bad in [{**process, 'parent': 8}, {**process, 'parent': 1, 'start': '124'}]:
        try:
            namespace_processes({10: bad}, 9, [[1, 2]], [process] if bad['parent'] == 1 else [])
        except ValueError:
            pass
        else:
            raise AssertionError('foreign or reused namespace process accepted')
    class CollisionFixture(Fixture):
        def __init__(self):
            self.state = {'marker': 'existing', 'job': {'pid': 42, 'start': '123'}, 'namespaces': [[1, 2], [1, 3]]}
            self.cleanup_calls = self.lock_calls = 0

        @contextlib.contextmanager
        def locked(self):
            self.lock_calls += 1
            yield

        def cleanup(self):
            self.cleanup_calls += 1
            raise AssertionError('colliding run adopted existing resources')

    collision = CollisionFixture()
    previous_state = json.dumps(collision.state, sort_keys=True)
    try:
        with contextlib.redirect_stdout(io.StringIO()):
            collision.run()
    except ValueError as error:
        assert str(error) == 'interface fixture collision'
    else:
        raise AssertionError('duplicate interface run accepted')
    assert collision.cleanup_calls == 0 and collision.lock_calls == 1
    assert json.dumps(collision.state, sort_keys=True) == previous_state
    output = io.StringIO()
    with contextlib.redirect_stdout(output):
        for status in [-64, 0, 255, None, True, -65, 256, 'private-value']:
            diagnostic('wait', 'client', FileNotFoundError('private-value'), status)
    assert output.getvalue().splitlines() == [
        'veranda-native-interface: failure=wait checkpoint=client exception=OSError child=' + status
        for status in ['-64', '0', '255', 'unknown', 'unknown', 'unknown', 'unknown', 'unknown']]
    for seam, checkpoint in [('private-value', 'client'), ('wait', 'private-value'), ('operation', 'processes')]:
        try:
            diagnostic(seam, checkpoint, ValueError('private-value'))
        except ValueError:
            pass
        else:
            raise AssertionError('unsafe diagnostic checkpoint accepted')

    class FailingFixture(CollisionFixture):
        def __init__(self):
            super().__init__()
            self.state = None
            self.alias, self.names = 'marker', ['client', 'owner']

        def process(self, _):
            return {'pid': 42, 'start': '123'}

        def write(self, *_):
            pass

        def cleanup(self):
            self.cleanup_calls += 1
            self.checkpoint = 'processes'
            raise ValueError('private cleanup message')

    primary = RuntimeError('private operation message')
    def unavailable_ownership(*_):
        raise primary
    original_cdll = ctypes.CDLL
    failed = FailingFixture()
    output = io.StringIO()
    try:
        ctypes.CDLL = unavailable_ownership
        with contextlib.redirect_stdout(output):
            failed.run()
    except RuntimeError as error:
        assert error is primary
    else:
        raise AssertionError('operation failure lost during cleanup')
    finally:
        ctypes.CDLL = original_cdll
    assert failed.cleanup_calls == 1 and failed.lock_calls == 2
    assert output.getvalue().splitlines() == [
        'native-stage: ssh.interface.setup',
        'veranda-native-interface: failure=operation checkpoint=setup exception=Other child=unknown',
        'native-stage: ssh.interface.cleanup',
        'veranda-native-interface: failure=cleanup checkpoint=processes exception=ValueError child=unknown']
    print('ok: interface ownership and replacement receipt contracts')
