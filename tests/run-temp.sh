#!/usr/bin/env bash
# Real exec, directory locks, interruption and conservative recovery, without the full suite.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
python3 -B - "$ROOT" <<'PY'
import fcntl
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import uuid
from unittest.mock import patch

source = Path(sys.argv[1]) / 'dev'
# Fixture scratch templates honor the runner's TMPDIR; guest-command contract
# strings are not local scratch allocations.
for path in [*source.parent.glob('config/profiles/*/tests/**/*.sh'), *source.parent.glob('tests/**/*.sh')]:
    assert not re.search(r'\$\(mktemp (?:-d )?/tmp/', path.read_text()), path
prefix = f'subyard-test-run-{os.getuid()}-fixture-{uuid.uuid4().hex}-'
environment = {k: v for k, v in os.environ.items() if not k.startswith('SUBYARD_RUN_TEMP')}

with tempfile.TemporaryDirectory(prefix='subyard-run-temp-test-', dir='/tmp') as temporary:
    fixture = Path(temporary)
    helper = fixture / 'test-run-temp.py'
    helper.write_text((source / helper.name).read_text().replace(
        'PREFIX = "syr."', f'PREFIX = {prefix!r}'))
    spec = importlib.util.spec_from_file_location('run_temp', helper)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    wrapper = fixture / 'with-test-temp.sh'
    shutil.copyfile(source / wrapper.name, wrapper)
    command = ['bash', str(wrapper)]
    processes = []
    detached = []
    protected = []

    def roots():
        return set(Path('/tmp').glob(prefix + '*'))

    def run(*args, code=0):
        result = subprocess.run(command + list(args), env=environment, capture_output=True, text=True)
        assert result.returncode == code, (result.returncode, result.stdout, result.stderr)
        return result.stdout.strip()

    def wait_for(path):
        deadline = time.monotonic() + 10
        while not path.exists():
            assert time.monotonic() < deadline, f'timed out waiting for {path}'
            time.sleep(.02)

    def blocked(name, body=''):
        marker = fixture / name
        fifo = fixture / (name + '.fifo')
        os.mkfifo(fifo)
        script = body + '\nprintf "%s" "$TMPDIR" > "$1"\nread -r _ < "$2"'
        process = subprocess.Popen(['python3', str(helper), 'enter', 'bash', '-c', script, '_', str(marker), str(fifo)],
                                   env=environment, start_new_session=True)
        processes.append(process)
        wait_for(marker)
        return process, Path(marker.read_text())

    try:
        # Success/failure delete all scratch. Nested entry reuses the root without sweeping.
        for code in (0, 23):
            scratch = Path(run('bash', '-c', 'test "$TMPDIR" = "$GOTMPDIR"; mktemp -d; exit "$1"', '_', str(code), code=code))
            assert scratch.parent.name.startswith(prefix) and scratch.parent.parent == Path('/tmp')
            assert not scratch.exists(), scratch
            assert not roots(), roots()
        run('bash', '-c', 'before=$TMPDIR; bash "$1" bash -c \'test "$TMPDIR" = "$1"\' _ "$before"', '_', str(wrapper))
        assert not roots(), roots()

        # A matching PID marker alone is not inherited ownership.
        result = subprocess.run(['bash', '-c', 'export SUBYARD_RUN_TEMP_ENTERED=$$; exec bash "$@"',
                                 '_', str(wrapper), 'bash', '-c', 'printf "%s" "$TMPDIR"'],
                                env={**environment, 'TMPDIR': '/tmp'}, capture_output=True, text=True, check=True)
        assert Path(result.stdout).name.startswith(prefix), result
        assert not roots(), roots()

        for mode in (0o500, 0):
            process, root = blocked('permissions-' + str(mode))
            child = root / 'restricted'
            child.mkdir()
            (child / 'file').touch()
            child.chmod(mode)
            os.kill(process.pid, signal.SIGKILL)
            process.wait(timeout=10)
            run('true')
            assert not root.exists(), root

        # Once authorized, a failed sweep reports the retained root in one line.
        process, root = blocked('sweep-failure')
        os.kill(process.pid, signal.SIGKILL)
        process.wait(timeout=10)
        import io
        diagnostic = io.StringIO()
        parent = os.open('/tmp', os.O_RDONLY | os.O_DIRECTORY)
        try:
            with patch.object(module, 'remove', side_effect=PermissionError('fixture refusal')), patch('sys.stderr', diagnostic):
                module.sweep(parent)
        finally:
            os.close(parent)
        assert len(diagnostic.getvalue().splitlines()) == 1 and str(root) in diagnostic.getvalue()
        run('true')

        # The timer sweep removes only abandoned roots created more than the given days ago.
        # Every entry sweeps unlocked roots, so all three exist before any is abandoned.
        aged = [blocked(name) for name in ('aged-old', 'aged-new', 'aged-live')]
        for process, _ in aged[:2]:
            os.kill(process.pid, signal.SIGKILL)
            process.wait(timeout=10)
        for _, root in (aged[0], aged[2]):
            old = time.time() - 8 * 86400
            os.utime(root / module.RECEIPT, (old, old), follow_symlinks=False)
        unrelated = Path(tempfile.mkdtemp(prefix='subyard-run-temp-unrelated-', dir='/tmp'))
        os.utime(unrelated, (time.time() - 30 * 86400,) * 2)
        try:
            for days in ('0', 'x'):
                assert subprocess.run(['python3', str(helper), 'sweep', days], env=environment,
                                      capture_output=True).returncode != 0, days
            subprocess.run(['python3', str(helper), 'sweep', '7'], env=environment, check=True)
            assert [root.exists() for _, root in aged] == [False, True, True], aged
            assert unrelated.is_dir()
        finally:
            os.kill(aged[2][0].pid, signal.SIGKILL)
            aged[2][0].wait(timeout=10)
            unrelated.rmdir()
        run('true')
        assert not roots(), roots()

        # Failed cleanup preserves the command status, including a successful command.
        original = helper.read_text()
        helper.write_text(original.replace('if sys.argv[1] == "clean":',
                                          'if sys.argv[1] == "clean":\n        raise OSError("fixture cleanup failure")'))
        try:
            run('bash', '-c', 'exit 23', code=23)
            run('true', code=1)
        finally:
            helper.write_text(original)
        run('true')

        # PID-only TERM forwards promptly and waits for the command to finish cleanup.
        marker, verdict = fixture / 'term-ready', fixture / 'term-verdict'
        worker = fixture / 'term-worker.py'
        worker.write_text('''import os
from pathlib import Path
import signal
import sys
import time
marker, verdict = map(Path, sys.argv[1:])
root = Path(os.environ['TMPDIR'])
def stop(signum, _frame):
    time.sleep(.5)
    (root / 'term-cleanup').touch()
    verdict.write_text('ok')
    sys.exit(128 + signum)
signal.signal(signal.SIGTERM, stop)
marker.write_text(str(root))
while True:
    time.sleep(30)
''')
        with (fixture / 'term.out').open('wb') as output:
            process = subprocess.Popen(command + ['python3', str(worker), str(marker), str(verdict)],
                                       env=environment, stdout=output, stderr=output, start_new_session=True)
            processes.append(process)
            wait_for(marker)
            root = Path(marker.read_text())
            started = time.monotonic()
            os.kill(process.pid, signal.SIGTERM)
            assert process.wait(timeout=5) == 143
            assert time.monotonic() - started < 5
            assert verdict.read_text() == 'ok' and not root.exists()

        # A detached fd holder outlives cleanup's grace without losing its scratch.
        original = helper.read_text()
        helper.write_text(original.replace('GRACE = 5', 'GRACE = .15'))
        marker, release = fixture / 'detached-ready', fixture / 'detached-release'
        descendant = fixture / 'detached.py'
        descendant.write_text('''import os
from pathlib import Path
import sys
import time
marker, release = map(Path, sys.argv[1:])
root = Path(os.environ['TMPDIR'])
marker.write_text(f'{os.getpid()} {root}')
while not release.exists():
    assert root.is_dir(), 'detached descendant lost scratch'
    time.sleep(.02)
''')
        try:
            launch = '''import subprocess, sys, time
from pathlib import Path
subprocess.Popen([sys.executable, *sys.argv[1:]], close_fds=False, start_new_session=True,
                 stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
while not Path(sys.argv[2]).exists():
    time.sleep(.01)
sys.exit(23)
'''
            result = subprocess.run(command + ['python3', '-c', launch, str(descendant), str(marker), str(release)],
                                    env=environment, capture_output=True, text=True, timeout=5)
            assert result.returncode == 23, result
            pid, root = marker.read_text().split()
            detached.append(int(pid))
            root = Path(root)
            assert root.exists(), 'cleanup removed a locked root'
            assert len(result.stderr.splitlines()) == 1 and 'still in use' in result.stderr, result.stderr
            print(result.stderr.strip())
            run('true')
            assert root.exists(), 'next run removed a live detached descendant root'
            release.touch()
            deadline = time.monotonic() + 5
            while root.exists():
                assert time.monotonic() < deadline, 'detached lock did not close'
                time.sleep(.02)
                run('true')
        finally:
            release.touch()
            if marker.exists():
                try:
                    os.kill(int(marker.read_text().split()[0]), signal.SIGKILL)
                except ProcessLookupError:
                    pass
            helper.write_text(original)

        for whole_group in (False, True):
            process, root = blocked('kill-' + str(whole_group), 'sleep 60 &' if whole_group else '')
            (os.killpg if whole_group else os.kill)(process.pid, signal.SIGKILL)
            process.wait(timeout=10)
            assert root.exists()
            run('true')
            assert not roots(), roots()

        # Receipt-less, forged, and symlink candidates are never deletion authority.
        for name in ('unknown', 'no-receipt', 'fake', 'oversized', 'typed'):
            path = Path('/tmp') / (prefix + name)
            path.mkdir(mode=0o700)
            protected.append(path)
            (path / 'sentinel').touch()
        fake = protected[2] / '.test-run.json'
        fake.write_text(json.dumps({'version': 1, 'root': [0, 0], 'uid': os.getuid()}))
        fake.chmod(0o600)
        for path in protected[3:]:
            info = path.stat()
            state = {'version': True if path.name.endswith('typed') else 1,
                     'root': [info.st_dev, info.st_ino], 'uid': os.getuid()}
            receipt = path / '.test-run.json'
            receipt.write_text(json.dumps(state) + (' ' * 1024 if path.name.endswith('oversized') else ''))
            receipt.chmod(0o600)
        link = Path('/tmp') / (prefix + 'symlink')
        link.symlink_to(protected[0], target_is_directory=True)
        protected.append(link)
        run('true')
        assert roots() == set(protected), roots()
        assert all((path / 'sentinel').exists() for path in protected)

        process, live = blocked('live')
        run('true')
        assert live.exists(), 'concurrent runner lost its root'
        os.kill(process.pid, signal.SIGKILL)
        process.wait(timeout=10)
        run('true')
        assert roots() == set(protected), roots()

        # The namespace descendant inherits the directory fd without special propagation.
        if shutil.which('bwrap'):
            ready, release = fixture / 'namespace-ready', fixture / 'release'
            body = ('bwrap --ro-bind / / --bind ' + str(fixture) + ' ' + str(fixture) + ' --unshare-pid --proc /proc -- bash -ec '
                    + "'test -d /proc/self/fd/$SUBYARD_RUN_TEMP_FD; touch " + str(ready)
                    + '; while [ ! -f ' + str(release) + ' ]; do sleep .02; done' + "' &\n"
                    + 'while [ ! -f ' + str(ready) + ' ]; do sleep .02; done')
            process, root = blocked('namespace', body)
            os.kill(process.pid, signal.SIGKILL)
            process.wait(timeout=10)
            run('true')
            assert root.exists(), 'namespace descendant lost inherited ownership'
            release.touch()
            deadline = time.monotonic() + 10
            while root.exists():
                assert time.monotonic() < deadline, 'namespace lock did not close'
                time.sleep(.02)
                run('true')
        else:
            print('SKIP: bwrap namespace descendant (bwrap unavailable)')

        logs = fixture / 'logs'
        logs.mkdir()
        for index in range(12):
            path = logs / f'run.{index:02}'
            path.mkdir()
            os.utime(path, (index, index))
        active = os.open(logs / 'run.00', os.O_RDONLY | os.O_DIRECTORY)
        fcntl.flock(active, fcntl.LOCK_EX)
        subprocess.run(['python3', str(helper), 'retain', str(logs)], check=True)
        assert (logs / 'run.00').exists(), 'retention deleted active logs'
        assert not (logs / 'run.01').exists(), 'retention stopped at an active run'
        os.close(active)
        subprocess.run(['python3', str(helper), 'retain', str(logs)], check=True)
        assert sorted(p.name for p in logs.iterdir()) == [f'run.{i:02}' for i in range(2, 12)]

        for error in (FileNotFoundError, PermissionError):
            stale = logs / 'run.old'
            stale.mkdir(exist_ok=True)
            os.utime(stale, (0, 0))
            original_open = os.open

            def fail_open(path, *args, **kwargs):
                if path == 'run.old':
                    raise error('fixture retention race or refusal')
                return original_open(path, *args, **kwargs)

            with patch.object(module.os, 'open', side_effect=fail_open):
                module.retain(logs)
            assert stale.exists()
        # Concurrent deletion during enumeration is harmless too.
        original_stat = os.stat

        def vanished(path, *args, **kwargs):
            if path == 'run.old':
                raise FileNotFoundError('fixture concurrent delete')
            return original_stat(path, *args, **kwargs)

        with patch.object(module.os, 'stat', side_effect=vanished):
            module.retain(logs)
        subprocess.run(['python3', str(helper), 'retain', str(logs)], check=True)
        assert roots() == set(protected), roots()
    finally:
        (fixture / 'release').touch()
        for pid in detached:
            try:
                os.kill(pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
        for process in processes:
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            process.wait(timeout=10)
        # Only this fixture's explicitly created paths; never sweep generic /tmp entries.
        for path in protected:
            if path.is_symlink():
                path.unlink()
            else:
                shutil.rmtree(path)
        run('true')
print('ok: test run cleanup, crash recovery, inherited ownership and log retention')
PY

# Lane controllers that source dev/agent-e2e.sh keep their scratch in an owned run root.
python3 -B - "$ROOT" <<'PY'
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import tempfile
import time

root = Path(sys.argv[1])
# Every core controller that allocates local lease scratch in /tmp enters a run root first;
# profile controllers are covered by their owning profile lanes.
for path in sorted((root / 'dev/e2e').glob('*.sh')):
    text = path.read_text()
    if re.search(r'LOCAL_TEMP="\$\(mktemp -d "?(\$\{TMPDIR:-/tmp\}|/tmp)/', text):
        assert 'enter_lane_run_temp "' in text, f'{path} allocates scratch outside a run root'

environment = {k: v for k, v in os.environ.items() if not k.startswith('SUBYARD_RUN_TEMP')}
# A controller started by any process of a run reuses its root instead of entering another.
inside = ['python3', str(root / 'dev/test-run-temp.py'), 'inside']
assert subprocess.run(inside, env={**environment, 'TMPDIR': '/tmp'}).returncode == 1
subprocess.run(['bash', str(root / 'dev/with-test-temp.sh'), 'bash', '-c', '"$@" || exit; :', '_', *inside],
               env=environment, check=True, timeout=60)
with tempfile.TemporaryDirectory(prefix='subyard-controller-run-root-', dir='/tmp') as temporary:
    fixture = Path(temporary)
    marker, hook = fixture / 'scratch', fixture / 'bash-env'
    # Stop P0 just before it contacts the broker, after it has allocated its local scratch.
    hook.write_text('''trap 'if [[ "$BASH_COMMAND" == acquire_lease ]]; then
  printf "%s\\n" "$LOCAL_TEMP" > "$CONTROLLER_MARKER"; kill -"$CONTROLLER_SIGNAL" "$$"; fi' DEBUG
''')
    environment.update(TMPDIR='/tmp', BASH_ENV=str(hook), CONTROLLER_MARKER=str(marker),
                       SUBYARD_E2E_STATE_DIR=str(fixture / 'state'))
    command = ['bash', 'dev/e2e/p0-acceptance.sh', '--slot', '1', '--lane', 'release-smoke']

    def scratch():
        path = Path(marker.read_text().strip())
        assert path.parent.name.startswith('syr.') and path.parent.parent == Path('/tmp'), path
        return path

    # A killed process group leaves its root unlocked and in place.
    process = subprocess.Popen(command, cwd=root, env={**environment, 'CONTROLLER_SIGNAL': 'STOP'},
                               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)
    try:
        deadline = time.monotonic() + 120
        while not marker.exists() or not marker.read_text().endswith('\n'):
            assert process.poll() is None and time.monotonic() < deadline, 'controller did not reach allocation'
            time.sleep(.05)
        left = scratch()
    finally:
        os.killpg(process.pid, signal.SIGKILL)
        process.wait(timeout=10)
    deadline = time.monotonic() + 10
    while subprocess.run(['pgrep', '-g', str(process.pid)], capture_output=True).returncode == 0:
        assert time.monotonic() < deadline, 'killed controller group did not exit'
        time.sleep(.05)
    assert left.is_dir() and (left.parent / '.test-run.json').is_file(), left

    # The next controller run removes it, and its wrapper removes the root of a controller killed alone.
    marker.unlink()
    result = subprocess.run(command, cwd=root, env={**environment, 'CONTROLLER_SIGNAL': 'KILL'},
                            capture_output=True, text=True, timeout=120)
    assert result.returncode == 137, (result.returncode, result.stderr)
    assert not left.parent.exists(), 'next controller run kept an abandoned run root'
    assert not scratch().parent.exists(), 'wrapper kept a killed controller run root'
print('ok: sourced lane controllers keep scratch in owned run roots')
PY
