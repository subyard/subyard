#!/usr/bin/env python3
"""Small owning checks for bounded Android fixture evidence."""
import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import signal
import struct
import subprocess
import sys
import tempfile
import time
import zlib
from types import SimpleNamespace
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[5]
E2E = ROOT / 'config/profiles/android/tests/e2e'


# Run the actual manifest validator against semantic output variations, not SDK
# substitutes. Native compilation/signing remains the allocated VM's contract.
sdk_source = (E2E / 'android-sdk-build.sh').read_text()
manifest_validator = sdk_source.split("python3 - <<'PY'\n", 1)[1].split('\nPY\n', 1)[0]
badging = """package: versionName='1.0' name='org.subyard.acceptance' versionCode='1' compileSdkVersion='36'
minSdkVersion:'26'
targetSdkVersion:'36'
application: icon='' label='Subyard SDK acceptance'
launchable-activity: label='' name='org.subyard.acceptance.MainActivity' icon=''
"""
with tempfile.TemporaryDirectory(prefix='android-sdk-manifest-', dir='/tmp') as directory:
    metadata = Path(directory, 'badging.txt')
    for value, failure in (
            (badging, None),
            (badging.replace('minSdkVersion:', 'sdkVersion:'), None),
            (badging.replace("minSdkVersion:'26'", "minSdkVersion:'25'"), 'minimum API'),
            (badging.replace("targetSdkVersion:'36'", "targetSdkVersion:'35'"), 'target API'),
            (badging.replace("name='org.subyard.acceptance'", "name='org.other.app'"), 'package'),
            (badging.replace('Subyard SDK acceptance', 'wrong label'), 'resource label'),
            (badging.replace('org.subyard.acceptance.MainActivity', 'org.other.Activity'), 'launcher Activity')):
        metadata.write_text(value)
        metadata.chmod(0o600)
        result = subprocess.run([sys.executable, '-c', manifest_validator], cwd=directory,
                                capture_output=True, text=True)
        assert result.returncode == (0 if failure is None else 1), result.stderr
        if failure is not None:
            assert failure in result.stderr and 'observed' in result.stderr, result.stderr

# Failed SDK assertions keep their status and bounded public diagnostics before
# removing the marker-owned build directory.
sdk_cleanup = sdk_source[sdk_source.index('stage=build\n'):sdk_source.index("trap 'exit 130' INT")]
with tempfile.TemporaryDirectory(prefix='android-sdk-cleanup-', dir='/tmp') as directory:
    work = Path(directory, 'build')
    work.mkdir(mode=0o700)
    (work / '.marker').write_text('subyard-android-sdk-build-v1\n')
    (work / 'badging.txt').write_text('x' * 9000)
    result = subprocess.run(['bash', '-c', 'set -euo pipefail\nwork="$1"\n' + sdk_cleanup
                             + '\nstage=manifest\nexit 23\n', '_', str(work)],
                            capture_output=True, text=True)
    assert result.returncode == 23 and not work.exists()
    assert 'stage=manifest exit=23' in result.stderr
    assert result.stderr.splitlines()[-1] == 'x' * 4096


def load(name):
    spec = importlib.util.spec_from_file_location(name, E2E / (name + '.py'))
    value = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(value)
    return value


# Capture the actual bounded Images.ensure failure from a real child process,
# without constructing Images on the live pool root or exposing credentials.
sdk_diagnostic = load('android-sdk-diagnostic')
pool_spec = importlib.util.spec_from_file_location('sdk_diagnostic_pool', E2E.parents[1] / 'pool.py')
sdk_pool_module = importlib.util.module_from_spec(pool_spec)
pool_spec.loader.exec_module(sdk_pool_module)
with tempfile.TemporaryDirectory(prefix='android-sdk-diagnostic-', dir='/tmp') as name:
    directory = Path(name)
    sdk = directory / 'sdk'
    manager = sdk / 'cmdline-tools/latest/bin/sdkmanager'
    manager.parent.mkdir(parents=True)
    manager.write_text('#!/usr/bin/env python3\nprint("Error: unsupported SDK option")\n'
                       'print("https://private-user:private-pass@example.test/repository?token=private-query")\n'
                       'print("Authorization: Bearer private-bearer")\n'
                       'print("token=private-token /private/fixture/config")\nraise SystemExit(7)\n')
    manager.chmod(0o755)
    image = dict(key='a' * 64, package='system-images;android-36;google_apis;x86_64',
                 revision='1.0.0', size=16, sha1='b' * 40)
    live = directory / 'live/download-123456789012345678901234'
    live.mkdir(parents=True)
    (live / '.download-owner.json').write_text(json.dumps(dict(schema=sdk_pool_module.SCHEMA, kind='image-download')))
    config = dict(sdk_root=str(sdk), size=1)
    with patch.object(sdk_pool_module.Images, 'resolve', return_value=image), \
            patch.object(sdk_pool_module.shutil, 'disk_usage', return_value=SimpleNamespace(free=64 * 1024**3)):
        result = sdk_diagnostic.diagnose(sdk_pool_module, config, directory / 'private')
    assert result['retry'] == 'failed' and result['original_failure_preserved']
    assert result['sdk_output_present'] and result['sdk_exit_code'] == 7
    assert 'unsupported SDK option' in result['sdk_error'] and live.is_dir()
    assert not list((directory / 'private/images').glob('download-*'))
    assert all(value not in json.dumps(result) for value in
               ('private-user', 'private-pass', 'private-query', 'private-bearer', 'private-token', '/private/fixture'))
    assert len(sdk_diagnostic.public_error(b'x' * 16385)) <= 4096
    for api in (35, 36):
        selected = dict(image, package=f'system-images;android-{api};google_apis;x86_64')
        with patch.object(sdk_pool_module.Images, 'resolve', return_value=selected) as resolve, \
                patch.object(sdk_pool_module.shutil, 'disk_usage', return_value=SimpleNamespace(free=64 * 1024**3)):
            result = sdk_diagnostic.diagnose(sdk_pool_module, config, directory / f'api-{api}', api)
        assert resolve.call_args.args[0]['api'] == api
        assert result['api'] == api and result['sdk_exit_code'] == 7 and result['original_failure_preserved']
    for api in (34, 37, '35', True):
        try:
            sdk_diagnostic.diagnose(sdk_pool_module, config, directory / 'invalid', api)
        except ValueError:
            pass
        else:
            raise AssertionError('invalid diagnostic API accepted')
    for arguments in (['--api', '34'], ['--api', '37'], ['--api', '35', 'extra'], ['--unknown']):
        invalid = subprocess.run([sys.executable, '-B', str(E2E / 'android-sdk-diagnostic.py'), *arguments],
                                 capture_output=True, text=True)
        assert invalid.returncode == 2 and not invalid.stdout
    with patch.object(sdk_pool_module.Images, 'resolve', return_value=image), \
            patch.object(sdk_pool_module.Images, 'ensure', return_value=directory):
        retry = sdk_diagnostic.diagnose(sdk_pool_module, config, directory / 'retry')
    assert retry['retry'] == 'succeeded' and retry['original_failure_preserved']

    # External TERM must unwind the real method's finally, despite the SDK
    # starting in a separate session. Its child and download stage must disappear.
    child_identity = directory / 'sdk-pids'
    manager.write_text('#!/usr/bin/env python3\nimport os, pathlib, subprocess, sys, time\n'
                       'child=subprocess.Popen([sys.executable,"-c","import time; time.sleep(60)"])\n'
                       f'pathlib.Path({str(child_identity)!r}).write_text(str(os.getpid())+" "+str(child.pid))\n'
                       'time.sleep(60)\n')
    worker = '''import importlib.util, pathlib, signal, sys
from types import SimpleNamespace
from unittest.mock import patch
def load(path):
    spec=importlib.util.spec_from_file_location(path.stem, path)
    module=importlib.util.module_from_spec(spec); spec.loader.exec_module(module); return module
diagnostic=load(pathlib.Path(sys.argv[1])); pool=load(pathlib.Path(sys.argv[2]))
signal.signal(signal.SIGTERM, diagnostic.interrupted)
try:
    with patch.object(pool.Images, 'resolve', return_value=__import__('json').loads(sys.argv[5])), patch.object(pool.shutil, 'disk_usage', return_value=SimpleNamespace(free=64*1024**3)):
        diagnostic.diagnose(pool, dict(sdk_root=sys.argv[3], size=1), pathlib.Path(sys.argv[4]))
except InterruptedError:
    pass
assert not list(pathlib.Path(sys.argv[4]).glob('images/download-*'))
raise SystemExit(23)
'''
    process = subprocess.Popen([sys.executable, '-B', '-c', worker, str(E2E / 'android-sdk-diagnostic.py'),
                                str(E2E.parents[1] / 'pool.py'), str(sdk), str(directory / 'cancelled'),
                                json.dumps(image)], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    pids = []
    try:
        deadline = time.monotonic() + 5
        while not child_identity.exists() and time.monotonic() < deadline:
            time.sleep(.02)
        pids = [int(value) for value in child_identity.read_text().split()]
        process.send_signal(signal.SIGTERM)
        stdout, stderr = process.communicate(timeout=5)
        assert process.returncode == 23, (process.returncode, stdout, stderr)
        for pid in pids:
            status = Path('/proc', str(pid), 'stat')
            assert not status.exists() or status.read_text().split(') ', 1)[1].startswith('Z ')
    finally:
        if process.poll() is None:
            process.kill()
            process.wait()
        if pids:
            with contextlib.suppress(ProcessLookupError):
                os.killpg(pids[0], signal.SIGKILL)

# The failed owning image lanes retry; even diagnostic failure cannot replace the
# original status. Execute the owning shell branch with a harmless transport.
runtime_source = (E2E / 'android-pool-runtime.sh').read_text()
repeat_start = runtime_source.index('android_repeat_provision() {')
repeat_function = runtime_source[repeat_start:runtime_source.index('\n}\n', repeat_start) + 3]
with tempfile.TemporaryDirectory(prefix='android-repeat-provision-', dir='/tmp') as directory:
    phase, label = 'toolchain-repeat', 'toolchain_repeat'
    for downloaded in (False, True):
        output = ('android provision: download stage=jdk state=start' if downloaded
                  else 'android provision: component=jdk state=reused')
        prelude = ('set -euo pipefail\nstate="$1"\n'
                   'android_phase_begin() { :; }; android_phase_end() { :; }; android_network_received() { :; }\n'
                   'yard() { [ "$*" = "provision --yes" ]; printf "%s\\n" "$REPEAT_OUTPUT"; }\n')
        result = subprocess.run(['bash', '-c', prelude + repeat_function
                                 + '\nandroid_repeat_provision "$2" "$3"\n', '_', directory, phase, label],
                                capture_output=True, text=True, env=dict(os.environ, REPEAT_OUTPUT=output))
        assert result.returncode == (1 if downloaded else 0), result.stderr
        assert (f'android {label}=PASS archive_downloads=0' in result.stdout) is not downloaded
diagnostic_branch = runtime_source[runtime_source.index('  if [ "$status" -ne 0 ] && { [ "$lane" = sdk-images ]'):
                                   runtime_source.index('\n  if [ -n "$monitor_pid" ]')]
for lane, phase, api, diagnostic_status, called in (('sdk-images', 'images', 36, 0, True),
                                                  ('sdk-images', 'images', 36, 17, True),
                                                  ('sdk-images', 'setup', 36, 0, False),
                                                  ('memory-lease', 'images', 35, 0, True),
                                                  ('memory-lease', 'images', 36, 17, True),
                                                  ('memory-lease', 'images', 34, 0, False),
                                                  ('projects', 'images', 35, 0, False)):
    result = subprocess.run(['bash', '-c', 'set -euo pipefail\n'
                             'root="$1"; lane="$2"; failed_phase="$3"; sdk_api="$4"; diagnostic_status="$5"\n'
                             'status=23; project=fixture; instance=fixture; incus_binary=(incus)\n'
                             'timeout() { printf "%s\\n" "$*"; return "$diagnostic_status"; }\n'
                             + diagnostic_branch + '\nexit "$status"\n', '_', str(ROOT), lane, phase,
                             str(api), str(diagnostic_status)], capture_output=True, text=True)
    assert result.returncode == 23 and (f'python3 - --api {api}' in result.stdout) == called

# Execute the actual normal prepare block: failing its first API35 or the
# sdk-images-only API36 forwards that API to the real helper's strict CLI.
start = runtime_source.rindex('android_phase_begin images\n')
prepare = runtime_source[start:runtime_source.index('\nandroid_phase_end 0', start)]
for lane, api in (('memory-lease', 35), ('sdk-images', 36)):
    result = subprocess.run(['bash', '-c', 'set -euo pipefail\n'
                             'root="$1"; lane="$2"; project=fixture; instance=fixture; incus_binary=(incus)\n'
                             'android_phase_begin() { :; }; android_network_received() { :; }; yard() { return 23; }\n'
                             'timeout() { python3 -B "$root/config/profiles/android/tests/e2e/android-sdk-diagnostic.py" --api "$sdk_api" --help >/dev/null; printf "api=%s\\n" "$sdk_api"; }\n'
                             'cleanup() { status=$?; failed_phase=images;\n' + diagnostic_branch
                             + '\nexit "$status"; }; trap cleanup EXIT\n' + prepare,
                             '_', str(ROOT), lane], capture_output=True, text=True)
    assert result.returncode == 23 and result.stdout.splitlines()[-1] == f'api={api}', result.stderr

# A successful SDK prepare closes its phase before any telemetry hook. Execute
# those actual dispatch blocks: each failed hook retains23 without an SDK retry.
start = runtime_source.index('  android_phase_begin images\n', runtime_source.index('if [ "$lane" = memory-lease ]; then'))
hooks = runtime_source[start:runtime_source.index('  android_phase_begin memory-lease\n', start)]
for hook, phase in (('PROFILE_EXERCISE', 'telemetry-profile'), ('NATIVE_EXERCISE', 'telemetry-native'),
                    ('RESOURCE_OBSERVER', 'telemetry-storage')):
    result = subprocess.run(['bash', '-c', 'set -euo pipefail\n'
                             'root="$1"; lane=memory-lease; android_fixture=runtime; project=fixture; instance=fixture\n'
                             'sdk_api=""; state=/tmp/fixture; YARD_BIN=fixture; YARD_NAME=fixture; incus_binary=(incus)\n'
                             'SUBYARD_E2E_PROFILE_EXERCISE=""; SUBYARD_E2E_NATIVE_EXERCISE=""; SUBYARD_E2E_RESOURCE_OBSERVER=""\n'
                             'printf -v "SUBYARD_E2E_$2" %s fixture\n'
                             'SUBYARD_TELEMETRY_NATIVE_STATE=fixture; SUBYARD_TELEMETRY_DATA_DIR=fixture\n'
                             '. "$root/config/profiles/android/tests/e2e/android-pool-phases.sh"\n'
                             'yard() { return 0; }; android_candidate_command() { return 23; }\n'
                             'timeout() { printf "unexpected-sdk-diagnostic\\n"; }\n'
                             'cleanup() { status=$?; trap - EXIT; failed_phase="${android_phase:-}"; android_phase_end "$status";\n'
                             + diagnostic_branch + '\nexit "$status"; }; trap cleanup EXIT\n' + hooks,
                             '_', str(ROOT), hook], capture_output=True, text=True)
    assert result.returncode == 23 and 'unexpected-sdk-diagnostic' not in result.stdout, result.stderr
    assert 'runtime-images state=end' in result.stdout and 'exit_code=0' in result.stdout
    assert f'runtime-{phase} state=end' in result.stdout and 'exit_code=23' in result.stdout


capture = load('android-pool-capture')
viewer = load('android-pool-viewer')
monitor = load('android-pool-monitor')
lifecycle = load('android-pool-lifecycle')
remote_viewer = load('android-pool-remote-viewer')

# Canonical registration uses the fixture's actual owner inventory RPC route.
# Keep the forced command allowlist closed to unrelated host operations.
remote_source = (E2E / 'android-pool-remote.sh').read_text()
remote_evidence = load('android-remote-evidence')
context_start = remote_source.index('controller_context() {')
controller_context = remote_source[context_start:remote_source.index('\n}\n', context_start) + 3]
with tempfile.TemporaryDirectory(prefix='android-controller-context-', dir='/tmp') as directory:
    script = ('''set -euo pipefail
work="$1"; controller_home="$1/controller-home"
android_candidate_command() { "$@"; }
export YARD_RUNTIME_ROOT=/owner/runtime SUBYARD_REPOSITORY_ROOT=/owner/source YARD_ENGINE_PATH=/owner/engine
''' + controller_context + '''
controller_context bash -euc '
  [ "$SUBYARD_CONFIG_HOME" = "$1/controller-config" ]
  [ "$SUBYARD_HOME" = "$1/controller-data" ]
  [ "$YARD_RUNTIME_ROOT" = "$SUBYARD_HOME/runtime" ]
  [ "$HOME" = "$1/controller-home" ]
  [ -z "${SUBYARD_REPOSITORY_ROOT:-}${YARD_ENGINE_PATH:-}" ]
' _ "$work"
''')
    subprocess.run(['bash', '-c', script, '_', directory], check=True, capture_output=True)
# Execute the actual six-argument run and cleanup with local transport failures.
remote_cleanup = remote_source[remote_source.index('cleanup() {'):remote_source.index('\ntrap cleanup EXIT')]
remote_report = remote_source[remote_source.index('report_remote() {'):remote_source.index('\ntrap \'report_remote')]
remote_modes = remote_source[remote_source.index('case "$#:${7:-}" in'):remote_source.index('\nesac') + 5]
run_start = remote_source.index('if [ -n "$run_payload" ] && [ -z "$viewer_tools" ]; then')
remote_run = remote_source[run_start:remote_source.index('\nremote_stage=owner-unchanged', run_start)]
for command_exit, cleanup_exit in ((17, 3), (23, 0), (23, 3)):
    with tempfile.TemporaryDirectory(prefix='android-remote-evidence-', dir='/tmp') as directory:
        work = Path(directory) / 'remote'
        work.mkdir()
        (work / '.marker').write_text('subyard-android-remote-v1\n')
        prelude = '''set -euo pipefail
evidence_helper="$1/config/profiles/android/tests/e2e/android-remote-evidence.py"
work="$2"; viewer_tools=''; viewer_lease=''; run_payload=/fixture/payload
sshd_job=''; remote_stage=run; remote_mode=unknown; remote_command_exit=''
fail() { printf 'android-pool-remote: %s\\n' "$*" >&2; exit 1; }
controller() {
  if [ "$1" = run ]; then
    remote_command_exit="$FAKE_COMMAND_EXIT"
    printf 'android-pool-remote: rejected SSH command private-diagnostic-canary\\n' >&2
    printf '36\\n'
    return "$remote_command_exit"
  fi
  remote_command_exit=0
  printf '%s\\n' '{"slots":[{"state":"available"},{"state":"available"}]}'
}
sudo() {
  [ "${1:-}" != -n ] || shift
  [ "$FAKE_CLEANUP_EXIT" = 0 ] || return "$FAKE_CLEANUP_EXIT"
  "$@"
}
'''
        script = (prelude + remote_modes + '\n' + remote_report + '\n' + remote_cleanup
                  + '\ntrap cleanup EXIT\n' + remote_run + '\nremote_stage=complete\n')
        result = subprocess.run(['bash', '-c', script, '_', str(ROOT), str(work), 'yard', 'project', 'instance', '/fixture/adb'],
            capture_output=True, text=True, env=dict(os.environ, FAKE_COMMAND_EXIT=str(command_exit),
                                                    FAKE_CLEANUP_EXIT=str(cleanup_exit)))
        expected = 1 if command_exit != 23 else cleanup_exit
        assert result.returncode == expected, result.stderr
        assert 'private-diagnostic-canary' not in result.stdout + result.stderr
        diagnostic = Path(directory) / 'final-adb.log'
        diagnostic.write_text(result.stderr)
        observed = remote_evidence.summary(diagnostic, result.returncode)
        assert observed['mode'] == 'run' and observed['cleanup_exit'] == cleanup_exit
        assert observed['native_exit'] == (1 if command_exit != 23 else 0)
        assert observed['stage'] == ('run' if command_exit != 23 else 'complete')
        if command_exit != 23:
            assert observed['command_exit'] == command_exit
            assert observed['error'] == 'ssh-command-refused'
        if cleanup_exit == 0:
            assert not work.exists()
with tempfile.TemporaryDirectory(prefix='android-remote-invalid-', dir='/tmp') as directory:
    diagnostic = Path(directory) / 'invalid.log'
    diagnostic.write_text(remote_evidence.PREFIX + '{"stage":"private-diagnostic-canary"}\n')
    assert remote_evidence.summary(diagnostic, 1)['stage'] == 'unknown'
    assert 'private-diagnostic-canary' not in json.dumps(remote_evidence.summary(diagnostic, 1))
dispatch = '#!/usr/bin/python3\n' + remote_source.split("source = '''#!/usr/bin/python3\n", 1)[1].split("'''.replace('VALUES'", 1)[0]
dispatch = dispatch.replace('VALUES', repr(('/fixture/source', '/fixture/state', 'owner-yard',
                                          '/fixture/home', '/fixture/storage', '/fixture/payload', '/candidate/yard')), 1)
for request, expected in (
        (['exec', 'yard', 'rpc', '--stdio'], ['/candidate/yard', 'rpc', '--stdio']),
        (['yard', '-Y', 'owner-yard', 'emu', '--session-wire', 'view'],
         ['/candidate/yard', '-Y', 'owner-yard', 'emu', '--session-wire', 'view']),
        (['SUBYARD_OPERATION_ID=fixture-run', 'yard', '-Y', 'owner-yard', 'emu', 'run', '--', '/usr/bin/python3', '/fixture/payload'],
         ['/candidate/yard', '-Y', 'owner-yard', 'emu', 'run', '--', '/usr/bin/python3', '/fixture/payload']),
        (['SUBYARD_OPERATION_ID=fixture-run', 'yard', '-Y', 'owner-yard', 'emu', 'run', '--', '/usr/bin/python3', '/other/payload'], None),
        (['exec', 'yard', 'stop'], None),
        (['yard', '-Y', 'other-yard', 'emu', '--session-wire', 'view'], None)):
    calls = []
    def dispatched(program, arguments, environment):
        calls.append((program, arguments, environment))
    with patch.dict(os.environ, SSH_ORIGINAL_COMMAND=shlex.join(['--', 'bash', '-lc', shlex.join(request)]),
                    YARD_RUNTIME_ROOT='/unrelated-caller-runtime'), \
            patch.object(os, 'execve', side_effect=dispatched):
        try:
            exec(compile(dispatch, '<remote-owner-dispatch>', 'exec'), {})
        except SystemExit:
            assert expected is None
        else:
            assert expected is not None and calls[0][1] == expected
            assert calls[0][2]['SUBYARD_CONFIG_HOME'] == '/fixture/state/config'
            assert calls[0][2]['YARD_RUNTIME_ROOT'] == '/fixture/state/runtime'
    assert bool(calls) is (expected is not None)

# Local viewer cleanup retains detached descendant identities and preserves an
# unrelated process, even after the descendant is reparented away from its owner.
with tempfile.TemporaryDirectory(prefix='android-viewer-identities-') as temporary:
    marker = Path(temporary, 'child')
    parent = subprocess.Popen([sys.executable, '-c',
        'import pathlib,subprocess,sys; child=subprocess.Popen(["sleep","30"],start_new_session=True); '
        'pathlib.Path(sys.argv[1]).write_text(str(child.pid)); sys.stdin.read()', str(marker)],
        stdin=subprocess.PIPE)
    unrelated = subprocess.Popen(['sleep', '30'])
    owned = {parent.pid: remote_viewer.process_identity(parent.pid)}
    child = None
    try:
        deadline = remote_viewer.time.monotonic() + 5
        while not marker.exists() and remote_viewer.time.monotonic() < deadline:
            remote_viewer.time.sleep(0.01)
        child = int(marker.read_text())
        remote_viewer.observe_descendants(owned)
        assert child in owned and unrelated.pid not in owned
        identity = owned[child]
        assert identity['group'] == child and remote_viewer.live_identity(identity)
        parent.communicate(timeout=5)
        assert remote_viewer.live_identity(identity)
        remote_viewer.signal_owned(identity, remote_viewer.signal.SIGTERM)
        deadline = remote_viewer.time.monotonic() + 5
        while remote_viewer.live_identity(identity) and remote_viewer.time.monotonic() < deadline:
            remote_viewer.time.sleep(0.01)
        assert not remote_viewer.live_identity(identity) and unrelated.poll() is None
        with patch.object(remote_viewer, 'process_identity', return_value=dict(identity, start='reused')):
            assert not remote_viewer.live_identity(identity)
    finally:
        if child in owned:
            remote_viewer.signal_owned(owned[child], remote_viewer.signal.SIGKILL)
        parent.kill()
        parent.wait(timeout=5)
        unrelated.kill()
        unrelated.wait(timeout=5)

# A native viewer/ADB child can reparent before the first observation. Its
# fixture capture marker and exact executable must still establish ownership.
with tempfile.TemporaryDirectory(prefix='android-viewer-orphan-') as temporary:
    marker = Path(temporary, 'child')
    executable = Path(temporary, 'scrcpy')
    shutil.copy2(shutil.which('sleep'), executable)
    capture_id = str(Path(temporary, 'capture'))
    environment = dict(os.environ, SUBYARD_E2E_VIEWER_CAPTURE=capture_id)
    parent = subprocess.Popen([sys.executable, '-c',
        'import pathlib,subprocess,sys; child=subprocess.Popen([sys.argv[1],"30"],start_new_session=True); '
        'pathlib.Path(sys.argv[2]).write_text(str(child.pid))', str(executable), str(marker)],
        env=environment, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    parent_identity = remote_viewer.process_identity(parent.pid)
    unrelated = subprocess.Popen([str(executable), '30'],
        env=dict(environment, SUBYARD_E2E_VIEWER_CAPTURE=capture_id + '-foreign'))
    foreign_executable = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(30)'],
                                         env=environment)
    child_identity = None
    try:
        parent.wait(timeout=5)
        child = int(marker.read_text())
        child_identity = remote_viewer.process_identity(child)
        assert child_identity is not None and child_identity['parent'] != parent.pid
        owned = {parent.pid: parent_identity}
        remote_viewer.observe_descendants(owned, capture_id, {executable.resolve()})
        assert child in owned and unrelated.pid not in owned and foreign_executable.pid not in owned
        leftovers = [identity for identity in owned.values() if remote_viewer.live_identity(identity)]
        assert leftovers == [child_identity], 'reparented native child must fail process absence'
        remote_viewer.signal_owned(owned[child], remote_viewer.signal.SIGTERM)
        deadline = remote_viewer.time.monotonic() + 5
        while remote_viewer.live_identity(owned[child]) and remote_viewer.time.monotonic() < deadline:
            remote_viewer.time.sleep(0.01)
        assert not remote_viewer.live_identity(owned[child])
        assert unrelated.poll() is None and foreign_executable.poll() is None
    finally:
        if child_identity is not None:
            remote_viewer.signal_owned(child_identity, remote_viewer.signal.SIGKILL)
        parent.kill()
        parent.wait(timeout=5)
        unrelated.kill()
        unrelated.wait(timeout=5)
        foreign_executable.kill()
        foreign_executable.wait(timeout=5)

# The full recovery verifier checks the live unit in the yard's systemd namespace.
recovery_check = (E2E / 'android-pool-recovery.sh').read_text().split(
    'cat > "$work/check.py" <<\'PY\'\n', 1)[1].split('\nPY\n', 1)[0]
with tempfile.TemporaryDirectory(prefix='android-host-cgroup-') as temporary:
    lease_path = Path(temporary, 'lease.json')
    lease_path.write_text(json.dumps(dict(token='private-token', allocation=dict(request=dict(device='phone', api=36)))))
    lease_path.chmod(0o600)
    unit = 'subyard-android-slot-001-2.service'
    allocation = dict(state='held', slot_id='001', generation=2, expires_at='2026-10-09T00:20:00Z')
    for code, group in ((0, '/system.slice/' + unit), (0, '/system.slice/subyard-android-pool.service'),
                        (0, ''), (1, '/system.slice/' + unit)):
        with patch.dict(sys.modules, client=SimpleNamespace(rpc=lambda *args, **kwargs: allocation)), \
                patch.object(sys, 'argv', ['guest', 'allocation', str(lease_path)]), \
                patch.object(sys, 'path', list(sys.path)), \
                patch('subprocess.run', return_value=SimpleNamespace(returncode=code, stdout=group.encode())), \
                contextlib.redirect_stdout(io.StringIO()) as output:
            try:
                exec(compile(recovery_check, '<recovery-check>', 'exec'), {})
            except AssertionError:
                assert code != 0 or group != '/system.slice/' + unit
                assert not output.getvalue()
            else:
                assert code == 0 and group == '/system.slice/' + unit
                assert json.loads(output.getvalue()) == ['001', 2, allocation['expires_at']]
        assert ('001' in output.getvalue()) is (code == 0 and group == '/system.slice/' + unit)
        assert 'private-token' not in output.getvalue()

# A fast boot must still leave enough rendered time for an owned viewer heartbeat.
for heartbeat, renew in ((300, True), (300, False), (True, True), (0, True), (1201, True)):
    with tempfile.TemporaryDirectory(prefix='android-remote-renewal-') as temporary:
        clock, process_state = [0], {}
        def remote_run(arguments, **kwargs):
            if arguments[1:3] == ['host', 'add']:
                return SimpleNamespace(returncode=0, stdout=b'')
            if arguments[1:3] == ['host', 'list']:
                return SimpleNamespace(returncode=0, stdout=b'HOST ID OWNER ENDPOINT\nfixture-owner android-e2e-owner\n')
            mode = arguments[-2]
            if mode == 'heartbeat':
                result = heartbeat
            elif mode == 'status':
                active = process_state and clock[0] < process_state['ends']
                slot = dict(slot_id=1, generation=1 + process_state.get('runs', 0),
                            state='held' if active else 'available')
                if active:
                    slot['expires_at'] = '2026-10-09T00:%02d:00Z' % (30 if renew and clock[0] >= heartbeat else 25)
                result = [slot]
            else:
                assert mode in ('wires', 'runtimes')
                result = 0 if mode == 'wires' else dict(primary=0, units=0)
            return SimpleNamespace(returncode=0, stdout=json.dumps(result).encode())
        def remote_process(arguments, **kwargs):
            wrapper = Path(temporary, 'viewer-bin/scrcpy').read_text()
            assert arguments[6] == 'fixture-owner/owner'
            process_state['runs'] = process_state.get('runs', 0) + 1
            process_state['ends'] = clock[0] + int(re.search(r'seconds=([0-9]+)', wrapper)[1])
            process_state['cancelled'] = False
            kwargs['stdout'].write(b'INFO: Texture: 640x360\n')
            kwargs['stdout'].flush()
            return SimpleNamespace(pid=12345, wait=lambda timeout: 0)
        def remote_wait(*arguments):
            if clock[0] >= process_state['ends']:
                return SimpleNamespace(si_code=remote_viewer.os.CLD_EXITED,
                                       si_status=143 if process_state['cancelled'] else 0)
            return None
        def remote_identity(pid):
            return dict(pid=pid, start='123', parent=12345 if pid != 12345 else 1, group=pid,
                        state='Z' if clock[0] >= process_state.get('ends', 0) else 'S')
        def remote_descendants(owned, *capture):
            owned.update({pid: remote_identity(pid) for pid in (12346, 12347, 12348)})
        def remote_arguments(pid):
            return {12346: [b'/candidate/yard', b'-Y', b'fixture-owner/owner', b'emu', b'view'],
                    12347: [b'/controller/tools/subyard-e2e-scrcpy/scrcpy'],
                    12348: [b'/controller/tools/platform-tools/adb']}.get(pid, [])
        def remote_signal(identity, number):
            if number == remote_viewer.signal.SIGTERM:
                assert identity['pid'] == 12346
                assert process_state['runs'] == 1 and clock[0] >= heartbeat
                process_state['cancelled'] = True
                process_state['ends'] = clock[0]
        with patch.object(sys, 'argv', ['remote-viewer', '/candidate/yard', temporary,
                                       '/controller/tools', 'owner', 'project', 'instance', '']), \
                patch.object(remote_viewer.subprocess, 'run', side_effect=remote_run), \
                patch.object(remote_viewer.subprocess, 'Popen', side_effect=remote_process), \
                patch.object(remote_viewer.os, 'waitid', side_effect=remote_wait), \
                patch.object(remote_viewer.os, 'killpg'), \
                patch.object(remote_viewer, 'process_identity', side_effect=remote_identity), \
                patch.object(remote_viewer, 'observe_descendants', side_effect=remote_descendants), \
                patch.object(remote_viewer, 'process_arguments', side_effect=remote_arguments), \
                patch.object(remote_viewer, 'process_executable', side_effect=lambda pid:
                             Path('/controller/tools/platform-tools/adb') if pid == 12348 else None), \
                patch.object(remote_viewer, 'signal_owned', side_effect=remote_signal), \
                patch.object(remote_viewer.signal, 'signal'), \
                patch.object(remote_viewer.time, 'monotonic', side_effect=lambda: clock[0]), \
                patch.object(remote_viewer.time, 'sleep', side_effect=lambda seconds: clock.__setitem__(0, clock[0] + seconds)), \
                contextlib.redirect_stdout(io.StringIO()) as output:
            try:
                remote_viewer.main()
                assert type(heartbeat) is int and 1 <= heartbeat <= 1200 and renew
            except RuntimeError as exc:
                expected = ('remote standalone viewer did not acquire and renew its own lease'
                            if type(heartbeat) is int and 1 <= heartbeat <= 1200 else 'invalid public heartbeat interval')
                assert str(exc) == expected and (not renew or type(heartbeat) is not int or not 1 <= heartbeat <= 1200)
        assert ('standalone-view=PASS lease=renewed-released' in output.getvalue()) is (heartbeat == 300 and renew)
        assert ('standalone-cancellation=PASS' in output.getvalue()) is (heartbeat == 300 and renew)
        if heartbeat == 300 and renew:
            assert process_state['runs'] == 1, 'renewal and cancellation must share one emulator boot'

# One emulator has a primary and an egress unit; idle cleanup requires neither.
for units, expected in (
        ('', dict(primary=0, units=0)),
        ('subyard-android-slot-001-2.service loaded active running private-description\n'
         'subyard-android-slot-001-2-egress.service loaded active running private-description\n',
         dict(primary=1, units=2))):
    with patch.dict(sys.modules, client=SimpleNamespace()), \
            patch.object(sys, 'argv', ['guest', 'runtimes', '']), \
            patch.object(sys, 'path', list(sys.path)), \
            patch('subprocess.run', return_value=SimpleNamespace(returncode=0, stdout=units)), \
            contextlib.redirect_stdout(io.StringIO()) as output:
        exec(compile(remote_viewer.GUEST_CHECK, '<guest-check>', 'exec'), {})
    assert json.loads(output.getvalue()) == expected and 'private-' not in output.getvalue()

# Execute the private guest protocol: confirmed Doze, restoration, bounded CPU
# projection and failed/unknown observations never expose helper output.
cpu_sample = ('CPU usage from 100ms to 0ms ago (private timestamp):\n'
              '  80% 123/system_server: 60% user + 20% kernel\n'
              '    70% 124/private-thread: 50% user + 20% kernel\n'
              '   +10% 125/system_server: 10% user + 0% kernel\n'
              '  20% 126/surfaceflinger: 10% user + 10% kernel\n'
              ' +30% 127/dex2oat64: 20% user + 10% kernel / faults: 2 minor 1 major\n'
              ' -5% 128/artd: 5% user + 0% kernel\n'
              '  3.5% 129/private-process-token: 1.5% user + 2% kernel\n'
              '40% TOTAL: 25% user + 10% kernel + 5% iowait\n')
for display_mode, failure in (
        ('idle-display', None), ('idle-display', 'sleep-timeout'),
        ('idle-display', 'force-timeout'), ('idle-display', 'missing-deep'),
        ('idle-display', 'missing-force'), ('idle-display', 'cpu-timeout'),
        ('idle-display', 'cpu-missing'), ('idle-display', 'cpu-malformed'),
        ('idle-display', 'cpu-total'),
        ('idle-display', 'cpu-limit'), ('idle-display', 'not-held'),
        ('wake-display', None), ('wake-display', 'wake-timeout'),
        ('wake-display', 'missing-force'), ('wake-display', 'missing-deep')):
    now, samples = [0], []
    lease = dict(token='private-idle-display-token')
    def idle_adb(value, *arguments, timeout=120):
        assert value is lease
        samples.append(arguments)
        print('private display helper output')
        if arguments == ('shell', 'dumpsys', 'cpuinfo'):
            assert timeout == 5
            if failure == 'cpu-timeout':
                raise lifecycle.CommandTimeout('private CPU timeout')
            if failure == 'cpu-missing':
                return 'private process output without a summary'
            if failure == 'cpu-malformed':
                return cpu_sample.replace('80% 123/', 'invalid% 123/')
            if failure == 'cpu-total':
                return cpu_sample.replace('40% TOTAL:', '140% TOTAL:')
            if failure == 'cpu-limit':
                return cpu_sample + 'private-process-token' * 65536
            return cpu_sample
        if arguments[:3] == ('shell', 'cmd', 'deviceidle'):
            assert timeout == 10
            operation = arguments[3:]
            if operation == ('force-idle', 'deep'):
                if failure == 'force-timeout':
                    raise lifecycle.CommandTimeout('private Doze timeout')
                return 'private ignored force response'
            if operation == ('unforce',):
                return 'private ignored restoration response'
            assert operation in (('get', 'deep'), ('get', 'force'))
            if failure == 'missing-' + operation[1]:
                return 'private invalid state'
            return ('IDLE' if display_mode == 'idle-display' else 'ACTIVE') if operation[1] == 'deep' else (
                'true' if display_mode == 'idle-display' else 'false')
        if arguments == ('shell', 'input', 'keyevent', 'KEYCODE_WAKEUP'):
            assert display_mode == 'wake-display' and timeout == 30
            if failure == 'wake-timeout':
                raise lifecycle.CommandTimeout('slow private display wake')
            return ''
        if arguments == ('shell', 'input', 'keyevent', 'KEYCODE_SLEEP'):
            assert display_mode == 'idle-display'
            return ''
        assert arguments == ('shell', 'dumpsys', 'power') and timeout <= 5
        now[0] += timeout
        if failure == 'sleep-timeout':
            raise lifecycle.CommandTimeout('slow private display')
        return 'mWakefulness=Awake' if len(samples) == 2 else 'mWakefulness=Asleep'
    def idle_helper(path):
        assert path == '/opt/subyard-e2e-lifecycle.py'
        return dict(idle_display=lifecycle.idle_display, adb=lifecycle.adb)
    guest_client = SimpleNamespace(read_lease=lambda path: lease,
                                   rpc=lambda operation, **kwargs: dict(state='available' if failure == 'not-held' else 'held'))
    succeeds = failure is None or failure.startswith('cpu-') or failure == 'not-held'
    with patch.dict(sys.modules, client=guest_client), \
            patch.object(sys, 'argv', ['guest', display_mode, '/private/guest-lease.json']), \
            patch.object(sys, 'path', list(sys.path)), \
            patch('runpy.run_path', side_effect=idle_helper), \
            patch.object(lifecycle, 'adb', side_effect=idle_adb), \
            patch.object(lifecycle.time, 'monotonic', side_effect=lambda: now[0]), \
            patch.object(lifecycle.time, 'sleep') as sleep, contextlib.redirect_stdout(io.StringIO()) as idle_output:
        try:
            exec(compile(remote_viewer.GUEST_CHECK, '<guest-check>', 'exec'), {})
            assert succeeds, 'guest accepted failed display setup'
        except (lifecycle.Failure, RuntimeError):
            assert not succeeds
    raw = idle_output.getvalue()
    assert 'private' not in raw and '123' not in raw and '127' not in raw
    if succeeds:
        projected = json.loads(raw)
        if display_mode == 'wake-display':
            assert projected is True
        else:
            assert projected['held'] is (failure != 'not-held')
            assert projected['forced'] is True and projected['deep'] == 'IDLE'
            for observation in (projected['cpu_before'], projected['cpu_after']):
                if failure and failure.startswith('cpu-'):
                    assert observation['observation'] == 'unknown' and observation['total_percent'] is None
                    assert all(value is None for value in observation['process_percent'].values())
                else:
                    assert observation == dict(observation='ok', total_percent=40.0,
                                               process_percent=dict(framework=80.0, composer=20.0,
                                                                    optimizer=35.0, other=3.5))
    else:
        assert raw == ''
        if failure != 'sleep-timeout':
            assert samples[-1] == ('shell', 'cmd', 'deviceidle', 'unforce')
    if failure == 'sleep-timeout':
        assert len(samples) == 5
    if succeeds and display_mode == 'idle-display':
        assert samples[-1] == ('shell', 'dumpsys', 'cpuinfo')
        assert samples.count(('shell', 'dumpsys', 'cpuinfo')) == 2
        assert sleep.call_args.args == (6,)

# Completed HTTP evidence survives a guest nc error; remote errors remain enums.
private_probe = 'private-token-and-address'
response = 'HTTP/1.0 204 No Content\r\nPrivate-Header: ' + private_probe + '\r\n\r\n'
for code, raw, error, expected in (
        (0, response + '\nSUBYARD_NC_EXIT=1\n', 'unexpected ' + private_probe, 'nc_exit=1(error=unknown)'),
        (0, '\nSUBYARD_NC_EXIT=1\n', 'nc: getaddrinfo: ' + private_probe, 'nc_exit=1(error=dns)'),
        (0, '\nSUBYARD_NC_EXIT=1\n', 'nc: Unknown option W ' + private_probe, 'nc_exit=1(error=unsupported_option)'),
        (0, '\nSUBYARD_NC_EXIT=1\n', 'nc: Connection refused ' + private_probe, 'nc_exit=1(error=connect)'),
        (1, '', 'adb: ' + private_probe, 'adb_transport_failed'),
        (0, response, private_probe, 'invalid_nc_sentinel'),
        (0, response + '\nSUBYARD_NC_EXIT=256\n', private_probe, 'invalid_nc_sentinel'),
        (0, response + '\nSUBYARD_NC_EXIT=' + private_probe + '\n', '', 'invalid_nc_sentinel'),
        (0, response + '\nSUBYARD_NC_EXIT=0\nSUBYARD_NC_EXIT=1\n', '', 'invalid_nc_sentinel'),
        (0, response + '\nSUBYARD_NC_EXIT=0\ntrailing', '', 'invalid_nc_sentinel')):
    result = lifecycle.network_result(SimpleNamespace(returncode=code, stdout=raw, stderr=error))
    assert result == ('204' if raw.startswith('HTTP/') else 'none', expected)
    assert private_probe not in str(result)

lease = dict(endpoint=private_probe, allocation=dict(slot_id='001', generation=2,
                                                    android_serial=private_probe))
completion = SimpleNamespace(returncode=0, stdout=response + '\nSUBYARD_NC_EXIT=1\n',
                             stderr=private_probe)
with patch.object(lifecycle, 'call', side_effect=[completion]), \
        patch.object(lifecycle, 'adb', return_value='GLES: SwiftShader') as renderer, \
        patch.object(lifecycle.time, 'monotonic', side_effect=[0, 0, 0, 91, 91]), \
        patch.object(lifecycle.time, 'sleep'), contextlib.redirect_stdout(io.StringIO()) as output:
    lifecycle.require_network_and_renderer(lease)
renderer.assert_called_once_with(lease, 'shell', 'dumpsys', 'SurfaceFlinger')
assert 'DNS/TCP egress and software renderer verified' in output.getvalue()
assert private_probe not in output.getvalue()

with patch.object(lifecycle.subprocess, 'run', side_effect=subprocess.TimeoutExpired(
        private_probe, 20, output=response.encode(), stderr=private_probe.encode())):
    try:
        lifecycle.call(['unused'])
        raise AssertionError('timeout accepted as a command completion')
    except lifecycle.CommandTimeout as exc:
        assert exc.http_status == '204' and private_probe not in str(exc)

# Run the actual observer shell with command-local stubs: failures are unknown,
# empty successful observations are absent, and policy-table defaults are found.
for ip_body, expected in (
        ('return 23', 'ipv4=unknown default_route=unknown'),
        ('return 0', 'ipv4=0 default_route=0'),
        ('case "$*" in "-4 addr show scope global") printf " inet private-address/24\\n";; '
         '"-4 route show table all") printf "default via private-address dev wlan0 table 1030\\n";; '
         '*) return 23;; esac', 'ipv4=1 default_route=1')):
    def observer_adb(_lease, *arguments, timeout):
        assert arguments[0] == 'shell' and timeout == 5
        program = ('getprop() { printf "1\\n"; }; pidof() { printf "596\\n"; }; '
                   'service() { printf "Service wifi: found\\n"; }; '
                   'settings() { printf "1\\n"; }; ip() { ' + ip_body + '; }; ' + arguments[1])
        result = subprocess.run(['/bin/sh', '-c', program], capture_output=True, text=True, check=True)
        return result.stdout.strip()
    with patch.object(lifecycle, 'adb', side_effect=observer_adb), \
            contextlib.redirect_stdout(io.StringIO()) as output:
        lifecycle.observe_network(lease, 'acquisition')
    assert expected in output.getvalue()
    assert 'acquisition observation slot=001 gen=2 adb=ok boot=1 framework=596' in output.getvalue()
    assert 'framework_exit=0 wifi_exit=0' in output.getvalue()
    assert ('ipv4_exit=23 route_exit=23' if ip_body == 'return 23' else
            'ipv4_exit=0 route_exit=0') in output.getvalue()
    assert 'private-' not in output.getvalue()

with patch.object(lifecycle, 'adb', side_effect=lifecycle.Failure(private_probe)), \
        contextlib.redirect_stdout(io.StringIO()) as output:
    lifecycle.observe_network(lease)
assert 'adb=failed boot=unknown framework=unknown' in output.getvalue()
assert 'ipv4=unknown default_route=unknown' in output.getvalue()
assert private_probe not in output.getvalue()
with patch.object(lifecycle, 'adb', return_value='boot=1\nboot_exit=23\nframework=596\n'
                  'framework_exit=23\nwifi_service=found\nwifi_service_exit=23\n'
                  'wifi=1\nwifi_exit=23\nipv4=1\nipv4_exit=23\n'
                  'default_route=1\nroute_exit=23\n'), \
        contextlib.redirect_stdout(io.StringIO()) as output:
    lifecycle.observe_network(lease)
assert 'boot=unknown framework=unknown wifi_service=unknown wifi=unknown' in output.getvalue()
assert 'ipv4=unknown default_route=unknown' in output.getvalue()

# Exact pinned v4.1 layout: device64, codec4, session12, then packets.
# Session/config/keyframe bits are 63/62/61, respectively.
session = struct.pack('>III', 0x80000000, 640, 360)
resized_session = struct.pack('>III', 0x80000001, 360, 640)
metadata = b'private-device-name'.ljust(64, b'\0') + b'h264' + session
configuration = struct.pack('>QI', 1 << 62, 3) + b'cfg'
frame = struct.pack('>QI', (1 << 61) | 42, 7) + b'private'
with contextlib.redirect_stdout(io.StringIO()) as output:
    observed = capture.VideoObservation()
    for byte in metadata + configuration + frame + resized_session + configuration + frame[:-1]:
        observed.feed(bytes([byte]))
    assert observed.metadata == 'complete' and observed.frames == 1
    assert observed.dimensions == (360, 640) and observed.stage == 'payload' and observed.remaining == 1
    assert len(observed.header) <= 12
    observed.emit('failed', 23, 'server_socket')
assert 'private' not in output.getvalue()
summary = viewer.capture_summary(output.getvalue(), 23)
assert summary['metadata'] == 'complete' and summary['complete_frames'] == 1
assert summary['child_exit'] == 23 and summary['relay'] == 'server_socket'
assert summary['result'] == 'failed' and not summary['rendered']

for bad in (b'leak', b'h264' + struct.pack('>III', 0x80000000, 0, 360),
            b'h264' + struct.pack('>III', 0x80000002, 640, 360)):
    observed = capture.VideoObservation()
    observed.feed(b'\0' * 64 + bad)
    assert observed.stage == 'malformed' and observed.frames == 0
with contextlib.redirect_stdout(io.StringIO()):
    for length in (0, 16 * 1024 * 1024 + 1):
        observed = capture.VideoObservation()
        observed.feed(metadata + struct.pack('>QI', 42, length))
        assert observed.stage == 'malformed' and not observed.header and observed.frames == 0
    observed = capture.VideoObservation()
    observed.feed(metadata[:-1])
    assert observed.metadata == 'pending' and observed.dimensions is None and len(observed.header) == 11
    observed = capture.VideoObservation()
    observed.feed(metadata + struct.pack('>QI', 42, 16 * 1024 * 1024))
    for _ in range(16):
        observed.feed(b'x' * 65536)
    assert observed.stage == 'payload' and observed.frames == 0 and not observed.header
    observed = capture.VideoObservation()
    observed.feed(metadata[:-12] + frame)
    assert observed.stage == 'malformed' and observed.metadata == 'pending'


class Sink:
    def sendall(self, _data):
        raise OSError('private failure')

observed = capture.VideoObservation()
try:
    capture.ObservingSocket(Sink(), observed).sendall(metadata)
    raise AssertionError('failed forwarding accepted')
except OSError:
    pass
assert observed.metadata == 'pending'

class RecordingSink:
    def __init__(self):
        self.sent = bytearray()
    def sendall(self, data):
        self.sent.extend(data)

sink, observed = RecordingSink(), capture.VideoObservation()
with contextlib.redirect_stdout(io.StringIO()):
    wrapped = capture.ObservingSocket(sink, observed)
    wrapped.sendall(metadata[:5])
    wrapped.sendall(metadata[5:] + frame)
assert bytes(sink.sent) == metadata + frame and observed.frames == 1

secret = 'private-token-and-endpoint'
memory_error = ('Android capacity: insufficient memory: required=3758096384 available=4294967295 '
                'reserve=536870912 pending=0 source=cgroup; no slot was preempted')
expected_capacity = dict(kind='memory', required_bytes=3758096384, available_bytes=4294967295,
                         reserve_bytes=536870912, pending_bytes=0, limiting_source='cgroup')
for suffix in ('', '; runtime was stopped'):
    summary = viewer.capture_summary(memory_error + suffix, 1)
    assert summary['capacity'] == expected_capacity and viewer.valid_viewer_summary(summary)
    assert viewer.safe_memory_refusal(summary)
# Only a bounded, failed, non-rendering memory refusal with a real deficit is expected.
for raw, code in ((memory_error, 0), (memory_error, 124),
                  (memory_error + '\nINFO: Texture: 640x360', 1),
                  (memory_error.replace('available=4294967295', 'available=4294967296'), 1),
                  (memory_error.replace('required=3758096384', 'required=0'), 1),
                  ('Android capacity: insufficient space for image download and extraction', 1),
                  ('Android capacity: ' + secret, 1),
                  ('Android boot_timeout: ' + secret, 1), (secret * 262144, 1)):
    assert not viewer.safe_memory_refusal(viewer.capture_summary(raw, code))

# Standalone mode never borrows a lease and must leave its pool idle on success.
for diagnostic, before_state, after_state in ((False, 'available', 'available'),
                                            (True, 'available', 'available'),
                                            (False, 'held', 'available'),
                                            (False, 'available', 'held')):
    client = SimpleNamespace(rpc=lambda operation: next(statuses))
    statuses = iter(dict(slots=[dict(state=state)]) for state in (before_state, after_state))
    with patch.object(viewer.importlib.util, 'spec_from_file_location'), \
            patch.object(viewer.importlib.util, 'module_from_spec', return_value=client), \
            patch.object(viewer.os, 'getuid', return_value=1000), \
            patch.object(sys, 'argv', ['viewer', '--standalone', *(['--native-debug'] if diagnostic else [])]), \
            patch.object(viewer, 'view') as view, contextlib.redirect_stdout(io.StringIO()):
        try:
            viewer.main()
            assert before_state == after_state == 'available'
        except RuntimeError:
            assert 'held' in (before_state, after_state)
    if before_state == 'available':
        view.assert_called_once_with(['--device', 'phone', '--api', '35', '--purpose', 'viewer-acceptance'],
                                     'standalone-capture', native_debug=diagnostic)
    else:
        view.assert_not_called()
for message, kind in viewer.CAPACITY_DISK_ERRORS.items():
    summary = viewer.capture_summary('Android capacity: ' + message + '; runtime was stopped', 1)
    assert summary['capacity'] == dict(kind=kind) and viewer.valid_viewer_summary(summary)
for raw in (memory_error + secret, memory_error.replace('source=cgroup', 'source=' + secret),
            memory_error.replace('pending=0', 'pending=-1'),
            memory_error.replace('required=3758096384', 'required=1152921504606846977'),
            'Android capacity: ' + secret,
            'Android capacity: insufficient space for image download and extraction ' + secret):
    summary = viewer.capture_summary(raw, 1)
    assert 'capacity' not in summary and secret not in json.dumps(summary)
    assert summary['pool_error'] == 'capacity' and viewer.valid_viewer_summary(summary)
summary = viewer.capture_summary('Android stale: private\n' + memory_error, 1)
assert summary['pool_error'] == 'stale' and 'capacity' not in summary
for key, invalid in (('required_bytes', True), ('available_bytes', -1), ('reserve_bytes', 1 << 61),
                     ('pending_bytes', secret), ('limiting_source', secret), ('extra', secret)):
    summary = viewer.capture_summary(memory_error, 1)
    summary['capacity'][key] = invalid
    assert not viewer.valid_viewer_summary(summary)
for capacity in (None, [], dict(kind=secret), dict(kind='memory'), dict(kind='userdata', extra=secret)):
    summary = viewer.capture_summary('Android capacity: unknown', 1)
    summary['capacity'] = capacity
    assert not viewer.valid_viewer_summary(summary)

# Execute the actual guest projection; private status/config values never leave it.
for memory in (dict(total_bytes=8 * 1024**3, available_bytes=4 * 1024**3, reserve_bytes=536870912,
                    limiting_source='physical', private=secret, pending_bytes=True,
                    visible_available_bytes=-1, cgroup_headroom_bytes=1 << 61), [secret]):
    guest_client = SimpleNamespace(rpc=lambda operation: dict(memory=memory, private=secret,
                                   broker_memory=dict(ram_current_bytes=123, swap_current_bytes=True, private=secret)))
    config = dict(state_root='/private/pool-root', size=2, token=secret)
    with patch.dict(sys.modules, client=guest_client), \
            patch.object(sys, 'argv', ['guest', 'capacity', '/private/guest-lease.json']), \
            patch.object(sys, 'path', list(sys.path)), \
            patch.object(Path, 'read_text', return_value=json.dumps(config)), \
            patch('shutil.disk_usage', return_value=SimpleNamespace(free=123456789)), \
            contextlib.redirect_stdout(io.StringIO()) as capacity_output:
        exec(compile(remote_viewer.GUEST_CHECK, '<guest-check>', 'exec'), {})
    raw = capacity_output.getvalue()
    assert secret not in raw and '/private' not in raw
    projected = json.loads(raw)
    assert projected['disk_reserve_bytes'] == 5 * 1024**3
    assert projected['images_free_bytes'] == projected['runtimes_free_bytes'] == 123456789
    assert projected['pending_bytes'] is projected['visible_available_bytes'] is projected['cgroup_headroom_bytes'] is None
    assert projected['limiting_source'] == ('physical' if type(memory) is dict else 'unknown')
    assert projected['total_bytes'] == (8 * 1024**3 if type(memory) is dict else None)
    assert projected['broker_memory'] == dict(ram_current_bytes=123, swap_current_bytes=None)

for pool_error in ('boot_timeout', 'stale', 'transport', 'heartbeat'):
    summary = viewer.capture_summary(f'Android {pool_error}: {secret}\n', 1)
    assert summary['pool_error'] == pool_error and summary['result'] == 'failed'
    assert viewer.valid_viewer_summary(summary) and secret not in json.dumps(summary)
summary = viewer.capture_summary('Android boot_timeout: private\nAndroid stale: private\n', 1)
assert summary['pool_error'] == 'boot_timeout'
for raw in (f'Android {secret}: private\n', 'prefix Android stale: private\n',
            'Android stale_suffix: private\n'):
    summary = viewer.capture_summary(raw, 1)
    assert 'pool_error' not in summary and secret not in json.dumps(summary)
for invalid in (secret, [], {}, 23):
    summary = viewer.capture_summary('', 1)
    summary['pool_error'] = invalid
    assert not viewer.valid_viewer_summary(summary)

malicious = {'metadata': secret, 'complete_frames': 2, 'stream': 'observed',
             'result': 'passed', 'child_exit': 0, 'relay': secret}
for raw in ('android-capture {malformed', 'android-capture ' + json.dumps(malicious),
            'INFO: Texture: ' + secret, secret, 'x' * 262145):
    summary = viewer.capture_summary(raw, 23)
    assert secret not in json.dumps(summary) and summary['result'] == 'failed'
    assert summary['complete_frames'] == 'unknown'
for code in (0, 23, 124):
    summary = viewer.capture_summary(b'x' * 262145, code)
    assert summary['output'] == 'limit' and summary['rendered'] == 'unknown'
    assert summary['viewer_exit'] == code and summary['result'] != 'passed'
    assert summary['complete_frames'] == 'unknown' and summary['metadata'] == 'unknown'

summary = viewer.capture_summary('INFO: Texture: 640x360\n', 124)
assert summary['result'] == 'timeout' and summary['child_exit'] == 'unknown'
assert viewer.capture_summary('INFO: Texture: 640x360\n', 0)['result'] == 'passed'
assert viewer.capture_summary('Android viewer: initial media byte after 60.0s: TimeoutError\n', 23)['relay'] == 'initial_media_byte'
assert viewer.capture_summary('', 139)['child_exit'] == 'unknown'
for child_status in (-11, 139, 23):
    summary = viewer.capture_summary(f'Android viewer: child returncode {child_status}\n', 139)
    assert summary['child_returncode'] == child_status
    assert summary['child_exit'] == (child_status if child_status >= 0 else 128 - child_status)
    assert viewer.valid_viewer_summary(summary)
for invalid in ('-65', '256', secret, '11 ' + secret):
    summary = viewer.capture_summary('Android viewer: child returncode ' + invalid + '\n', 139)
    assert summary['child_exit'] == 'unknown' and 'child_returncode' not in summary
    assert secret not in json.dumps(summary)

# Epoch observations identify the selected lease and complete packets, never media.
public_context = dict(SUBYARD_EMU_SLOT='002', SUBYARD_EMU_GENERATION='7', SUBYARD_EMU_API='35',
                      ANDROID_SERIAL=secret, ADB_SERVER_SOCKET=secret)
with patch.object(capture.time, 'time_ns', side_effect=[1000000000000, 1001000000000, 1002000000000, 1003000000000]), \
        contextlib.redirect_stdout(io.StringIO()) as output:
    observed = capture.VideoObservation()
    observed.observe_context(public_context)
    observed.feed(metadata[:-1])
    assert observed.timing['metadata_at_ms'] == 0
    observed.feed(metadata[-1:] + configuration + frame[:-1])
    assert observed.timing['metadata_at_ms'] == 1001000 and observed.timing['first_frame_at_ms'] == 0
    observed.feed(frame[-1:] + resized_session + frame)
    assert observed.timing['first_frame_at_ms'] == 1002000
    observed.finish()
timing = dict(slot=2, generation=7, api=35, context_at_ms=1000000,
              metadata_at_ms=1001000, first_frame_at_ms=1002000, finished_at_ms=1003000)
summary = viewer.capture_summary(output.getvalue(), 23)
assert summary['timing'] == timing and viewer.valid_viewer_summary(summary)
assert secret not in output.getvalue() and 'private' not in output.getvalue()
for key, bad in [('SUBYARD_EMU_SLOT', secret), ('SUBYARD_EMU_SLOT', '000'),
                 ('SUBYARD_EMU_GENERATION', '2147483648'), ('SUBYARD_EMU_API', '100'),
                 ('SUBYARD_EMU_API', True)]:
    observed = capture.VideoObservation()
    observed.observe_context(dict(public_context, **{key: bad}))
    assert observed.timing is None
for invalid in (dict(timing, extra=secret), dict(timing, slot=True), dict(timing, slot=0),
                dict(timing, generation=2147483648), dict(timing, api=100),
                dict(timing, finished_at_ms=4102444800001), dict(timing, metadata_at_ms=secret),
                dict(timing, metadata_at_ms=999999), dict(timing, first_frame_at_ms=1004000),
                dict(timing, metadata_at_ms=0), dict(timing, finished_at_ms=999999)):
    assert not viewer.valid_capture_timing(invalid)
    summary = viewer.capture_summary('android-capture-timing ' + json.dumps(invalid), 23)
    assert 'timing' not in summary and secret not in json.dumps(summary)
    assert not viewer.valid_viewer_summary(dict(summary, timing=invalid))
assert viewer.valid_capture_timing(dict(timing, metadata_at_ms=0, first_frame_at_ms=0))
assert viewer.valid_capture_timing(dict(timing, first_frame_at_ms=0))

# Exercise the helper's actual context/relay wrapping without installed runtime access.
server = SimpleNamespace(process_request=lambda request, address: None, failure=None)
fake_client = SimpleNamespace(context=lambda *args: public_context,
                              start_relay=lambda *args: None, rpc=lambda *args, **kwargs: None,
                              start_viewer_relay=lambda *args: server,
                              bridge=lambda local, channel: local.sendall(metadata + frame), Error=RuntimeError,
                              subprocess=subprocess)
def run_client(_arguments):
    assert fake_client.context({}, secret) is public_context
    fake_client.start_relay(secret, secret)
    relay = fake_client.start_viewer_relay(secret, secret)
    sink = RecordingSink()
    relay.process_request(sink, None)
    fake_client.bridge(sink, None)
    fake_client.rpc('release', token=secret)
    assert bytes(sink.sent) == metadata + frame
    return 0
fake_client.main = run_client
spec = SimpleNamespace(loader=SimpleNamespace(exec_module=lambda module: None))
with patch.object(capture.importlib.util, 'spec_from_file_location', return_value=spec), \
        patch.object(capture.importlib.util, 'module_from_spec', return_value=fake_client), \
        contextlib.redirect_stdout(io.StringIO()) as output:
    assert capture.main() == 0
summary = viewer.capture_summary(output.getvalue(), 0)
assert summary['complete_frames'] == 1 and summary['child_exit'] == 0
assert summary['timing']['slot'] == 2 and summary['timing']['api'] == 35
assert summary['timing']['first_frame_at_ms'] >= summary['timing']['metadata_at_ms'] > 0
assert secret not in output.getvalue() and 'private' not in output.getvalue()

scrcpy_log = ('INFO: Time limit reached\n'
              '[server] ERROR: Capture/encoding error: ' + secret + '\n'
              '[server] ERROR: Video encoding error\n'
              '[server] DEBUG: Screen streaming stopped\n'
              "ERROR: Demuxer 'video': stream configuration error on the device\n"
              'INFO: Time limit reached\n' + secret)
summary = viewer.capture_summary(scrcpy_log, 23)
assert summary['scrcpy_events'] == list(viewer.SCRCPY_EVENTS)
assert secret not in json.dumps(summary) and viewer.valid_viewer_summary(summary)
for events in ([secret], ['time_limit_reached'] * 2, [True], [{}], secret):
    assert not viewer.valid_viewer_summary(dict(summary, scrcpy_events=events))
assert 'scrcpy_events' not in viewer.capture_summary('INFO: Time limit reached ' + secret, 23)

for name, classification in [*viewer.ENCODERS.items(), (secret, 'other')]:
    summary = viewer.capture_summary(f"[server] DEBUG: Using video encoder: '{name}'\n", 23)
    assert summary['encoder'] == classification and secret not in json.dumps(summary)
    assert viewer.valid_viewer_summary(summary)
assert not viewer.valid_viewer_summary(dict(summary, encoder=secret))

# Stock screencap uses a streamed PNG. Only its header/end, byte count and exit
# are observed; pixels and arbitrary command errors never enter evidence.
def png_chunk(kind, data):
    return struct.pack('>I', len(data)) + kind + data + struct.pack('>I', zlib.crc32(kind + data))
def png(width=2):
    return (b'\x89PNG\r\n\x1a\n' + png_chunk(b'IHDR', struct.pack('>IIBBBBB', width, 1, 8, 6, 0, 0, 0))
            + png_chunk(b'IDAT', zlib.compress(b'\0' + b'\x01\x02\x03\x04' * 2)) + png_chunk(b'IEND', b''))
real_popen = subprocess.Popen
for data, code, expected in ((png(), 0, 'png_complete'), (png()[:-1], 0, 'malformed'),
                             (png(9000), 0, 'malformed'), (secret.encode(), 0, 'malformed'),
                             (png()[:29] + b'bad!' + png()[33:], 0, 'malformed'),
                             (png(), 23, 'command_failed')):
    def spawn(command, **kwargs):
        assert command == ['/srv/cache/android-sdk/platform-tools/adb', 'exec-out', 'screencap', '-p']
        assert kwargs['env']['ADB_SERVER_SOCKET'] == secret
        script = f'import os; os.write(1, {data!r}); raise SystemExit({code})'
        return real_popen([sys.executable, '-B', '-c', script], **kwargs)
    with patch.object(capture.subprocess, 'Popen', side_effect=spawn):
        evidence = capture.framebuffer_observation(public_context)
    assert evidence['status'] == expected and evidence['bytes'] == len(data)
    assert evidence['width'] == (2 if expected == 'png_complete' else 0)
    assert viewer.valid_framebuffer(evidence) and secret not in json.dumps(evidence)
def overflow(command, **kwargs):
    return real_popen([sys.executable, '-B', '-c', 'import os\nfor _ in range(260): os.write(1, b"x" * 65536)'], **kwargs)
with patch.object(capture.subprocess, 'Popen', side_effect=overflow):
    evidence = capture.framebuffer_observation(public_context)
assert evidence['status'] == 'limit' and viewer.valid_framebuffer(evidence)
def stalled(command, **kwargs):
    return real_popen([sys.executable, '-B', '-c', 'import time; time.sleep(60)'], **kwargs)
with patch.object(capture.subprocess, 'Popen', side_effect=stalled), \
        patch.object(capture.time, 'monotonic', side_effect=[0, 11]):
    evidence = capture.framebuffer_observation(public_context)
assert evidence['status'] == 'timeout' and viewer.valid_framebuffer(evidence)
with patch.object(capture.subprocess, 'Popen', side_effect=OSError(secret)):
    evidence = capture.framebuffer_observation(public_context)
assert evidence['status'] == 'unavailable' and secret not in json.dumps(evidence)

framebuffer = dict(status='png_complete', bytes=75, width=2, height=1,
                   memory_total_bytes=8589934592, memory_available_bytes=1073741824, swap_used_bytes=0,
                   started_at_ms=1000000, finished_at_ms=1001000)
summary = viewer.capture_summary('android-framebuffer ' + json.dumps(framebuffer), 23)
assert summary['framebuffer'] == framebuffer and summary['result'] == 'failed'
assert viewer.valid_viewer_summary(summary)
for invalid in (dict(framebuffer, extra=secret), dict(framebuffer, status=secret),
                dict(framebuffer, width=True), dict(framebuffer, bytes=45),
                dict(framebuffer, width=9000), dict(framebuffer, bytes=16 * 1024 * 1024 + 65537),
                dict(framebuffer, swap_used_bytes=secret), dict(framebuffer, memory_available_bytes=True),
                dict(framebuffer, memory_total_bytes=(1 << 60) + 1), dict(framebuffer, swap_used_bytes=-2),
                dict(framebuffer, finished_at_ms=999999), dict(framebuffer, status='timeout')):
    assert not viewer.valid_framebuffer(invalid)
    assert 'framebuffer' not in viewer.capture_summary('android-framebuffer ' + json.dumps(invalid), 23)
    assert not viewer.valid_viewer_summary(dict(summary, framebuffer=invalid))
with patch.object(capture.Path, 'read_text', return_value='MemTotal: 8388608 kB\nMemAvailable: 1048576 kB\nSwapTotal: 1024 kB\nSwapFree: 512 kB\n' + secret), \
        patch.object(capture.subprocess, 'Popen', side_effect=OSError(secret)):
    evidence = capture.framebuffer_observation(public_context)
assert evidence['memory_total_bytes'] == 8589934592 and evidence['memory_available_bytes'] == 1073741824
assert evidence['swap_used_bytes'] == 524288 and secret not in json.dumps(evidence)
with patch.object(capture.Path, 'read_text', side_effect=OSError(secret)), \
        patch.object(capture.subprocess, 'Popen', side_effect=OSError(secret)):
    evidence = capture.framebuffer_observation(public_context)
assert evidence['memory_total_bytes'] == evidence['memory_available_bytes'] == evidence['swap_used_bytes'] == -1
assert viewer.valid_framebuffer(evidence)

# Only one selected owned release is observed, while its relay is still alive.
# A failing observation never skips or changes the original release operation.
for payload, failure, calls in ((metadata, False, 1), (metadata, True, 1), (metadata + frame, False, 0)):
    releases = []
    relay = SimpleNamespace(process_request=lambda request, address: None, failure=None)
    fake_client = SimpleNamespace(context=lambda *args: public_context, start_relay=lambda *args: None,
                                 rpc=lambda operation, **values: releases.append((operation, values['token'])),
                                 start_viewer_relay=lambda *args: relay,
                                 bridge=lambda local, channel: local.sendall(payload), Error=RuntimeError,
                                 subprocess=subprocess)
    def main(_arguments):
        fake_client.context({}, secret)
        fake_client.start_relay(secret, secret)
        server = fake_client.start_viewer_relay(secret, secret)
        sink = RecordingSink(); server.process_request(sink, None); fake_client.bridge(sink, None)
        fake_client.rpc('release', token='other')
        fake_client.rpc('release', token=secret)
        fake_client.rpc('release', token=secret)
        return 0
    fake_client.main = main
    with patch.object(capture.importlib.util, 'spec_from_file_location', return_value=spec), \
            patch.object(capture.importlib.util, 'module_from_spec', return_value=fake_client), \
            patch.object(capture, 'framebuffer_observation', side_effect=RuntimeError(secret) if failure else None,
                         return_value=framebuffer) as probe, contextlib.redirect_stdout(io.StringIO()) as output:
        assert capture.main() == 0
    assert probe.call_count == calls
    assert releases == [('release', 'other'), ('release', secret), ('release', secret)]
    assert secret not in output.getvalue()
    summary = viewer.capture_summary(output.getvalue(), 0)
    assert ('framebuffer' in summary) == bool(calls and not failure)

valid = b'  mScreenState=OFF\nprivate-token-and-endpoint\nSUBYARD_QUERY_EXIT=0\n'
assert monitor.display_values('ok', valid) == {'query': 'ok', 'screen_state': 'OFF'}
for status, raw in [('timeout', valid), ('limit', valid), ('transport_unavailable', valid),
                    ('ok', valid.replace(b'=0\n', b'=1\n')), ('ok', b'mScreenState=OFF\n'),
                    ('ok', valid + b'x'), ('ok', valid.replace(b'OFF', secret.encode())),
                    ('ok', valid.replace(b'OFF', b'OFF\xff')), ('ok', b'x' * 65537), (secret, valid),
                    ('ok', b'mScreenState=OFF\nmScreenState=ON\nSUBYARD_QUERY_EXIT=0\n')]:
    result = monitor.display_values(status, raw)
    assert result['screen_state'] == 'unknown' and secret not in json.dumps(result)

# A validated framework PID change captures one bounded private logcat window.
# Initial/unknown PIDs and repeated samples do not manufacture restart evidence.
crash_log = ('--------- beginning of crash\n'
             'E/AndroidRuntime(101):     at android.os.BinderProxy.transact(Unknown Source)\n'
             f'E/Watchdog( 101): *** WATCHDOG KILLING SYSTEM PROCESS: Blocked in handler on main thread ({secret}) for 60s, '
             f'Blocked in monitor com.android.server.Watchdog$BinderThreadMonitor on monitor thread ({secret}) for 60s\n'
             f'I/watchdog( 101): [{secret}]\n'
             f'W/Watchdog(101): {secret} annotated stack trace:\n'
             'W/Watchdog(101):     at com.android.server.Watchdog$BinderThreadMonitor.monitor(Watchdog.java:448)\n'
             'W/Watchdog(101):     - waiting to lock <0x0123abcd> (a private.CapabilityIdentifier7)\n'
             'W/Watchdog(101):     at com.android.server.display.CapabilityIdentifier7.privateMethod7(Source.java:1)\n'
             'W/Watchdog(101): *** GOODBYE!\n'
             'W/Watchdog(101):     at android.os.BinderProxy.transactNative(Native Method)\n'
             f'E/AndroidRuntime( 101): *** FATAL EXCEPTION IN SYSTEM PROCESS: {secret}\n'
             f'E/AndroidRuntime( 101): java.lang.IllegalStateException: {secret}\n'
             'E/AndroidRuntime(101):     at com.android.server.display.CapabilityIdentifier7.privateMethod7(Source.java:1)\n'
             'E/AndroidRuntime(101):     at android.os.Parcel.createExceptionOrNull(Parcel.java:1)\n'
             f'E/AndroidRuntime( 101): Caused by: android.os.DeadObjectException: {secret}\n'
             'E/AndroidRuntime(101):     at android.os.BinderProxy.transactNative(Native Method)\n'
             'E/AndroidRuntime(101):     at android.view.SurfaceControl$CapabilityIdentifier7.privateMethod7(Unknown Source)\n'
             f'E/AndroidRuntime( 101): java.lang.OutOfMemoryError: {secret}\n'
             f'E/AndroidRuntime( 101): private.Exception: {secret}\n'
             f'E/AndroidRuntime(101): unrelated log message {secret}\n'
             'E/AndroidRuntime(101):     at android.os.BinderProxy.transact(Unknown Source)\n'
             f'F/libc    ( 101): Fatal signal 6 (SIGABRT), {secret}\n'
             f'F/libc( 101): Fatal signal 65 {secret}\n'
             f'E/AndroidRuntime( 999): java.lang.NullPointerException: {secret}\n'
             f'D/WifiClientModeImpl[wlan0]( 101): {secret}\n'
             f'I/NetworkMonitor/100( 101): {secret}\n'
             f'D/Framework dynamic tag( 101): {secret}\n'
             '\nSUBYARD_QUERY_EXIT=0\n').encode()
history, slot = {}, dict(slot_id='001', generation=7)
with patch.object(monitor, 'shell_read', side_effect=[('ok', crash_log), ('timeout', crash_log)]) as probe:
    for status, raw in [('ok', b'framework=\n'), ('timeout', b'framework=101\n'),
                        ('ok', b'framework=2147483648\n'), ('ok', b'framework=101\n'),
                        ('ok', b'framework=\n'), ('ok', b'framework=101\nframework=202\n'),
                        ('timeout', b'framework=202\n')]:
        assert monitor.framework_restart(slot, history, status, raw) is None
    probe.assert_not_called()
    observed = monitor.framework_restart(slot, history, 'ok', b'framework=202\n')
    assert observed == dict(previous_pid=101, current_pid=202, restart=1, query='ok',
                            evidence='matching_records', lines=26, record_limit=256, line_limit=1024,
                            counts=dict(watchdog=2, java_fatal=1, java_oom=1, native_fatal=1),
                            java_exceptions=['DeadObjectException', 'IllegalStateException', 'OutOfMemoryError'],
                            native_signals=[6], stack_subsystems=['binder', 'display', 'display', 'binder', 'binder', 'composer'],
                            stack_components=['binder', 'composer', 'display'],
                            public_frames=['com.android.server.Watchdog$BinderThreadMonitor.monitor',
                                           'android.os.BinderProxy.transactNative'],
                            watchdog_handlers=['binder', 'main', 'monitor'], stack_capped=False)
    assert monitor.framework_restart(slot, history, 'ok', b'framework=202\n') is None
    failed = monitor.framework_restart(slot, history, 'ok', b'framework=303\n')
    assert failed['query'] == 'timeout' and failed['evidence'] == 'unknown' and failed['counts'] is None
    assert monitor.framework_restart(slot, history, 'ok', b'framework=303\n') is None
    assert monitor.framework_restart(dict(slot, generation=8), history, 'ok', b'framework=404\n') is None
    assert probe.call_count == 2
    command = probe.call_args_list[0].args
    assert command[0] == slot and command[2:] == (5, 65536)
    assert all(argument in command[1] for argument in ('-b crash', '-b system', '-b events',
                                                       '-t 256', '-v brief', '--pid=101'))
assert secret not in json.dumps([observed, failed])
assert 'CapabilityIdentifier7' not in json.dumps(observed) and 'privateMethod7' not in json.dumps(observed)
trace_prefix = b'E/AndroidRuntime(101): *** FATAL EXCEPTION IN SYSTEM PROCESS: private\n'
stack_frame = b'E/AndroidRuntime(101):     at com.android.server.wifi.CapabilityIdentifier7.privateMethod7(Source.java:1)\n'
deep_cause = (b'E/AndroidRuntime(101): Caused by: android.os.DeadObjectException: private\n'
              b'E/AndroidRuntime(101):     at android.view.SurfaceControl$CapabilityIdentifier7.privateMethod7(Unknown Source)\n')
limited = monitor.restart_values('ok', trace_prefix + stack_frame * 17 + deep_cause + b'SUBYARD_QUERY_EXIT=0\n', 101)
assert limited['stack_subsystems'] == ['wifi'] * 16 and limited['stack_capped']
assert limited['stack_components'] == ['composer', 'wifi']
assert 'CapabilityIdentifier7' not in json.dumps(limited)
private_frame = b'E/AndroidRuntime(101):     at android.os.BinderProxy.transactNative(/private/path.java:1)\n'
rejected = monitor.restart_values('ok', trace_prefix + private_frame + stack_frame + b'SUBYARD_QUERY_EXIT=0\n', 101)
assert rejected['stack_subsystems'] == [] and rejected['public_frames'] == []
assert 'private/path' not in json.dumps(rejected)
for status, raw in [('timeout', crash_log), ('limit', crash_log), ('transport_unavailable', crash_log),
                    ('ok', crash_log.removesuffix(b'SUBYARD_QUERY_EXIT=0\n')),
                    ('ok', crash_log.replace(b'EXIT=0', b'EXIT=1')), ('ok', crash_log + b'x'),
                    ('ok', b'x' * 65537), (secret, crash_log),
                    ('ok', secret.encode() + b'\nSUBYARD_QUERY_EXIT=0\n'),
                    ('ok', b'I/Watchdog(101): pending\n' * 1025 + b'SUBYARD_QUERY_EXIT=0\n')]:
    result = monitor.restart_values(status, raw, 101)
    assert result['evidence'] == 'unknown' and result['counts'] is None
    assert result['stack_subsystems'] is None and result['stack_components'] is None and result['public_frames'] is None
    assert secret not in json.dumps(result)
empty = monitor.restart_values('ok', b'\nSUBYARD_QUERY_EXIT=0\n', 101)
assert empty['query'] == 'ok' and empty['evidence'] == 'unknown' and not any(empty['counts'].values())
# Multiline records may render more lines than logcat's 256-record window.
multiline = monitor.restart_values('ok', b'E/AndroidRuntime(101): private stack frame\n' * 300 + crash_log, 101)
assert multiline['query'] == 'ok' and multiline['lines'] == 326
assert multiline['counts'] == observed['counts'] and secret not in json.dumps(multiline)

# Early boot tracks real PID changes and takes only two scheduled scoped windows.
startup_log = ('SUBYARD_GUEST_MEM_TOTAL_KB=2048000\nSUBYARD_GUEST_MEM_AVAILABLE_KB=123456\n'
               'SUBYARD_GUEST_OOM_KILL=2\nSUBYARD_GUEST_DATA_AVAILABLE_KIB=98765\n'
               'SUBYARD_CORE_PID_system_server=202\nSUBYARD_CORE_PID_zygote=303\n'
               'SUBYARD_CORE_PID_zygote64=\nSUBYARD_CORE_PID_surfaceflinger=404\n'
               f'E/AndroidRuntime(101): *** FATAL EXCEPTION IN SYSTEM PROCESS: {secret}\n'
               f'E/AndroidRuntime(101): java.lang.OutOfMemoryError: {secret}\n'
               f'E/Watchdog(101): *** WATCHDOG KILLING SYSTEM PROCESS: {secret}\n'
               f'F/libc(303): Fatal signal 6 (SIGABRT), {secret}\n'
               f'F/libc(405): Fatal signal 11 (SIGSEGV), {secret} in tid 405 (render), pid 405 (surfaceflinger)\n'
               f'E/AndroidRuntime(999): FATAL EXCEPTION: {secret}\n'
               f'E/AndroidRuntime(999): java.lang.NullPointerException: {secret}\n'
               f'F/libc(999): Fatal signal 9, pid 999 ({secret})\n'
               f'F/libc(998): Fatal signal 9, pid 999 (surfaceflinger)\n'
               f'D/WifiClientModeImpl[wlan0](202): {secret}\n'
               '\nSUBYARD_QUERY_EXIT=0\n').encode()
scoped = monitor.restart_values('ok', startup_log)
assert scoped['query'] == 'ok' and scoped['evidence'] == 'matching_records'
assert scoped['counts'] == dict(watchdog=1, java_fatal=1, java_oom=1, native_fatal=2)
assert scoped['core_roles'] == ['surfaceflinger', 'system_server', 'zygote']
assert scoped['java_exceptions'] == ['OutOfMemoryError'] and scoped['native_signals'] == [6, 11]
assert scoped['guest'] == dict(mem_total_kb=2048000, mem_available_kb=123456, oom_kill=2, data_available_kib=98765)
assert secret not in json.dumps(scoped)
for invalid in (secret.encode(), b'-1', b'18446744073709551616', b'1 kB', b''):
    projected = monitor.restart_values('ok', startup_log.replace(b'MEM_AVAILABLE_KB=123456', b'MEM_AVAILABLE_KB=' + invalid))
    assert projected['guest']['mem_available_kb'] is None and projected['guest']['mem_total_kb'] == 2048000
    assert secret not in json.dumps(projected)
duplicate = monitor.restart_values('ok', b'SUBYARD_GUEST_OOM_KILL=3\n' + startup_log)
assert duplicate['guest']['oom_kill'] is None
for status, raw in [('timeout', startup_log), ('limit', startup_log),
                    ('ok', startup_log.removesuffix(b'SUBYARD_QUERY_EXIT=0\n')),
                    ('ok', startup_log.replace(b'system_server=202', b'system_server=2147483648'))]:
    result = monitor.restart_values(status, raw)
    assert result['evidence'] == 'unknown' and result['counts'] is None and secret not in json.dumps(result)
    assert all(value is None for value in result['guest'].values())

# Yard-visible counters read bounded kernel fixtures, never Android or process data.
cpu_raw = b'cpu  10 20 30 40 50 60 70 80 90 100\ncpu0 private-counter-token\n'
mem_raw = b'MemTotal:       8388608 kB\nMemAvailable:   1048576 kB\nPrivate: private-counter-token\n'
yard_sample = dict(cpu_ticks=dict(user=10, nice=20, system=30, idle=40, iowait=50, irq=60, softirq=70, steal=80),
                     ticks_per_second=100, mem_total_kb=8388608, mem_available_kb=1048576)
with tempfile.TemporaryDirectory(prefix='subyard-native-observation-') as directory:
    proc = Path(directory)
    for stat_raw, memory_raw, expected_cpu, expected_memory in (
            (cpu_raw, mem_raw, yard_sample['cpu_ticks'], (8388608, 1048576)),
            (None, None, None, (None, None)),
            (b'cpu private-counter-token\n', mem_raw, None, (8388608, 1048576)),
            (b'cpu 1 2 3 4 5 6 7\n', mem_raw, None, (8388608, 1048576)),
            (b'cpu 1 2 3 4 5 6 7 8', mem_raw, None, (8388608, 1048576)),
            (b'cpu -1 2 3 4 5 6 7 8\n', mem_raw, None, (8388608, 1048576)),
            (b'cpu 18446744073709551616 2 3 4 5 6 7 8\n', mem_raw, None, (8388608, 1048576)),
            (b'cpu ' + b'1 ' * 300 + b'\n', mem_raw, None, (8388608, 1048576)),
            (cpu_raw, b'MemTotal: private-counter-token kB\nMemAvailable: 0 kB\n', yard_sample['cpu_ticks'], (None, 0)),
            (cpu_raw, b'MemTotal: 1 kB\nMemTotal: 2 kB\n', yard_sample['cpu_ticks'], (None, None)),
            (cpu_raw, b'MemTotal: 1 kB\nMemAvailable: 2 kB', yard_sample['cpu_ticks'], (None, None)),
            (cpu_raw, mem_raw + b'x' * 8192, yard_sample['cpu_ticks'], (None, None))):
        for name, raw in (('stat', stat_raw), ('meminfo', memory_raw)):
            path = proc / name
            if raw is None:
                path.unlink(missing_ok=True)
            else:
                path.write_bytes(raw)
        with patch.object(monitor.os, 'sysconf', return_value=100):
            observed_yard = monitor.yard_observation(proc)
        assert observed_yard == dict(cpu_ticks=expected_cpu, ticks_per_second=100,
                                       mem_total_kb=expected_memory[0], mem_available_kb=expected_memory[1])
        assert 'private' not in json.dumps(observed_yard) and 'guest' not in json.dumps(observed_yard)
    for invalid_ticks in (0, -1, True, 100.5, 'private-counter-token'):
        with patch.object(monitor.os, 'sysconf', return_value=invalid_ticks):
            assert monitor.yard_observation(proc)['ticks_per_second'] is None
    with patch.object(monitor.os, 'sysconf', side_effect=OSError(secret)):
        assert monitor.yard_observation(proc)['ticks_per_second'] is None

clock, scheduled = [0], []
yard_samples = []
def yard_snapshot():
    yard_samples.append(clock[0])
    return yard_sample
def display_snapshot(slot):
    assert yard_samples[-1] == clock[0], 'native counters must precede blocking ADB queries'
    return dict(query='timeout', screen_state='unknown')
ages = iter((451, 481, 1101, 1131, 1161))
def monitor_sleep(_seconds):
    clock[0] = next(ages)
def early_shell(slot, command, seconds, limit, first_line=False):
    if first_line:
        assert command == 'echo framework=$(pidof system_server)' and (seconds, limit) == (3, 128)
        return 'ok', b'framework=101\n' if clock[0] == 0 else b'framework=202\n'
    if limit == 256:
        assert 'echo framework=$(pidof system_server)' in command and seconds == 5
        return 'ok', ('framework=202\nwifi_service=0\nwifi=0\nwifi_radio=enabled\n'
                      f'wifi_radio={secret}\nipv4=0\n').encode()
    assert (seconds, limit) == (5, 65536) and '-t 256' in command
    scheduled.append(command)
    if '--pid=101' in command:
        return 'ok', crash_log
    assert 'for role in system_server zygote zygote64 surfaceflinger' in command
    assert 'while read key value unit rest' in command and 'df -k /data' in command
    return ('ok', startup_log) if clock[0] < 1100 else ('timeout', startup_log)
with patch.object(monitor, 'initialize'), patch.object(monitor, 'slot_states', return_value=[(slot, 'provisioning')]), \
        patch.object(monitor, 'metrics', return_value=None), \
        patch.object(monitor, 'yard_observation', side_effect=yard_snapshot), \
        patch.object(monitor, 'display_observation', side_effect=display_snapshot), \
        patch.object(monitor, 'property_value', side_effect=lambda slot, name:
                     '36' if name == 'ro.build.version.sdk' else '1' if name == 'sys.boot_completed' and clock[0] >= 1101 else '0'), \
        patch.object(monitor, 'shell_read', side_effect=early_shell), \
        patch.object(monitor.Path, 'exists', side_effect=lambda: clock[0] >= 1161), \
        patch.object(monitor.time, 'monotonic', side_effect=lambda: clock[0]), \
        patch.object(monitor.time, 'sleep', side_effect=monitor_sleep), \
        patch.object(sys, 'argv', ['monitor', '/run/subyard-e2e-android-monitor-test.stop']), \
        contextlib.redirect_stdout(io.StringIO()) as early_output:
    monitor.main()
reports = [json.loads(line.removeprefix('android-boot-monitor ')) for line in early_output.getvalue().splitlines()
           if line.startswith('android-boot-monitor {')]
assert [report['sys.boot_completed'] for report in reports] == ['0', '0', '0', '1', '1', '1']
assert yard_samples == [0, 451, 481, 1101, 1131, 1161]
assert all(report['yard_visible'] == yard_sample for report in reports)
assert len(scheduled) == 3 and sum('framework_restart' in report for report in reports) == 1
windows = [report['startup_crash'] for report in reports if 'startup_crash' in report]
assert [window['threshold_seconds'] for window in windows] == [450, 1100]
assert windows[0]['counts'] == scoped['counts']
assert windows[1]['query'] == 'timeout' and windows[1]['evidence'] == 'unknown'
assert all(value is None for value in windows[1]['guest'].values())
assert all('wifi_radio=enabled' in report['network'] for report in reports if 'network' in report)
assert secret not in early_output.getvalue()

# Execute the generated POSIX kernel-only extraction against private fixtures.
with tempfile.TemporaryDirectory(prefix='subyard-guest-observation-') as directory:
    fixture = Path(directory)
    (fixture / 'meminfo').write_text('MemTotal: 3276800 kB\nMemAvailable: 1000 kB\nPrivate: ' + secret + '\n')
    (fixture / 'vmstat').write_text('oom_kill 7\nprivate ' + secret + '\n')
    kernel = scheduled[1].split('for role in ', 1)[0]
    kernel = kernel.replace('/proc/meminfo', '"$1/meminfo"').replace('/proc/vmstat', '"$1/vmstat"')
    for available in ('Available', 'unsupported'):
        script = ('df() { printf "%s\\n" "Filesystem 1K-blocks Used ' + available + ' Use% Mounted" '
                  '"private-path 2048 1024 1024 50% /private/data"; }; ' + kernel)
        native = subprocess.run(['sh', '-c', script, 'guest', directory], capture_output=True, timeout=5)
        assert native.returncode == 0 and not native.stderr and secret.encode() not in native.stdout
        assert b'private' not in native.stdout
        projection = monitor.restart_values('ok', native.stdout + startup_log[startup_log.index(b'SUBYARD_CORE_PID_'):])
        assert projection['guest'] == dict(mem_total_kb=3276800, mem_available_kb=1000, oom_kill=7,
                                           data_available_kib=1024 if available == 'Available' else None)

# CPU pressure shares the existing memory-pressure numeric projection.
unit = 'subyard-emu-001-7.service'
numeric_pressure = 'some avg10=1.00 avg60=0.20 avg300=0.10 total=123'
def metric_read(path):
    name = str(path)
    if name == '/proc/123/cgroup':
        return '0::/system.slice/' + unit + '\n'
    if path.name.endswith('.pressure'):
        return numeric_pressure + '\n' + secret
    if path.name == 'memory.stat':
        return 'anon 123\nfile 321\nprivate 987\nshmem -1\nkernel ' + str(1 << 61) + '\n' + secret
    return 'usage_usec 123' if path.name == 'cpu.stat' else 'oom 0' if path.name == 'memory.events' else '123'
with patch.object(monitor, 'Runtime', SimpleNamespace(unit=lambda slot: unit), create=True), \
        patch.object(monitor, 'host_netns_inode', 1, create=True), \
        patch.object(monitor.subprocess, 'run', return_value=SimpleNamespace(returncode=0,
                          stdout=f'ControlGroup=/system.slice/{unit}\nMainPID=123\nActiveState=active\n')), \
        patch.object(monitor.Path, 'read_text', metric_read), \
        patch.object(monitor.Path, 'open', return_value=io.BytesIO(
            b'private memory header\nRss: 456 kB\nPss: 234 kB\nPrivate: secret\n')), \
        patch.object(monitor.os, 'stat', return_value=SimpleNamespace(st_ino=2)):
    measured = monitor.metrics(slot)
assert measured['cpu.pressure'] == measured['memory.pressure'] == [numeric_pressure]
assert measured['memory.stat'] == dict(anon=123, file=321)
assert measured['main_process_memory_kb'] == dict(rss=456, pss=234)
assert secret not in json.dumps(measured)

with patch.object(monitor, 'initialize'), patch.object(monitor, 'slot_states', return_value=[]), \
        patch.object(monitor, 'yard_observation', return_value=yard_sample), \
        patch.object(monitor.Path, 'exists', return_value=True), \
        patch.object(sys, 'argv', ['monitor', '/run/subyard-e2e-android-monitor-test.stop']), \
        contextlib.redirect_stdout(io.StringIO()) as output:
    monitor.main()
assert '"state": "idle"' in output.getvalue()

# Exercise real cleanup bodies with host operations replaced by bounded local stubs.
with tempfile.TemporaryDirectory() as directory:
    for attempted, initial, teardown, expected in ((1, 23, 1, 23), (1, 0, 1, 3), (1, 0, 0, 0),
                                                   (0, 23, 99, 23)):
        source = (E2E / 'android-pool-runtime.sh').read_text()
        cleanup = source[source.index('cleanup() {'):source.index('\ntrap cleanup EXIT')]
        state = Path(directory) / f'owned-{attempted}-{initial}-{teardown}'
        state.mkdir()
        (state / '.marker').write_text('subyard-android-pool-runtime-v1\n')
        script = ('set -euo pipefail\n. "$1/android-pool-phases.sh"\n'
                  'android_fixture=runtime\nandroid_phase_begin check\n'
                  'state="$2"; project=""; instance=""; monitor_pid=""; monitor_stop=""; monitor_host_stop="$2/stop"; lane=diagnostics\n'
                  'incus_binary=(false); hold_seconds=0; teardown_status="$3"; init_attempted="$5"\n'
                  'yard() { printf called > "$state/teardown-called"; return "$teardown_status"; }; sudo() { shift; "$@"; }\n' + cleanup +
                  '\ntrap cleanup EXIT\nexit "$4"\n')
        result = subprocess.run(['bash', '-c', script, '_', str(E2E), str(state), str(teardown), str(initial), str(attempted)], capture_output=True, text=True)
        assert result.returncode == expected, result.stderr
        assert f'exit_code={initial}' in result.stdout
        assert not state.exists()
        if teardown and attempted:
            assert 'exit_code=3' in result.stdout
        if not attempted:
            assert 'android cleanup=passed' in result.stdout and 'exit_code=3' not in result.stdout
    for outcome, initial, expected in (('clean', 0, 0), ('clean', 23, 23),
                                       ('instance', 0, 3), ('project', 0, 3),
                                       ('state', 0, 3), ('user-file', 0, 3),
                                       ('volume', 0, 3), ('inventory-error', 0, 3),
                                       ('pool-gone', 0, 0)):
        state = Path(directory) / f'full-{outcome}-{initial}'
        definition = state / 'config/yards/fixture/config.env'
        owned = definition.parent / 'projects'
        owned.mkdir(parents=True)
        definition.write_text('ENVIRONMENT_PROFILES=android\n')
        definition.chmod(0o600)
        (state / '.marker').write_text('subyard-android-pool-runtime-v1\n')
        (state / 'config/android-acceptance-user-file').write_text('preserved')
        binary = state / 'fixture-incus'
        binary.write_text('#!' + sys.executable + '\nimport json,os,sys\n'
                          'assert sys.argv[1:] == ["storage","volume","list","default","--all-projects","--format=json"]\n'
                          'print(json.dumps([dict(type="custom",name="yard-srv-fixture")] if os.environ["BACKING_OUTCOME"] == "volume" else []))\n')
        binary.chmod(0o755)
        script = '''set -euo pipefail
. "$1/android-pool-phases.sh"
android_fixture=runtime; android_phase_begin check
state="$2"; outcome="$3"; lane=full; YARD_NAME=fixture; project=fixture
instance=fixture; monitor_pid=''; monitor_stop=''; monitor_host_stop="$state/stop"; init_attempted=1
SUBYARD_CONFIG_HOME="$state/config"; hold_seconds=0
sdk_pool=default; sdk_volume=yard-srv-fixture; incus_binary=("$state/fixture-incus")
export BACKING_OUTCOME="$outcome"
definition_checksum="$(sha256sum "$SUBYARD_CONFIG_HOME/yards/fixture/config.env")"
yard() {
  [ "$*" = 'teardown --yes' ] || return 99
  [ "$outcome" = state ] || rm -rf -- "$SUBYARD_CONFIG_HOME/yards/fixture/projects"
  [ "$outcome" != user-file ] || printf changed > "$SUBYARD_CONFIG_HOME/android-acceptance-user-file"
}
incus() {
  case "$*" in
    'list --all-projects --format=json')
      if [ "$outcome" = instance ]; then printf '[{"project":"fixture"}]\n'; else printf '[]\n'; fi ;;
    'project list --format=json')
      if [ "$outcome" = project ]; then printf '[{"name":"fixture"}]\n'; else printf '[]\n'; fi ;;
    'storage list --format=json')
      [ "$outcome" != inventory-error ] || return 23
      if [ "$outcome" = pool-gone ]; then printf '[]\n'; else printf '[{"name":"default"}]\n'; fi ;;
    *) return 99 ;;
  esac
}
sudo() { shift; "$@"; }
'''
        result = subprocess.run(['bash', '-c', script + cleanup + '\ntrap cleanup EXIT\nexit "$4"\n',
                                 '_', str(E2E), str(state), outcome, str(initial)],
                                capture_output=True, text=True)
        assert result.returncode == expected, result.stderr
        assert not state.exists()
        assert ('android cleanup=passed' in result.stdout) is (expected == initial)
    log = Path(directory) / 'viewer.log'
    log.write_text(secret + '\nINFO: Texture: 640x360\n')
    with contextlib.redirect_stdout(io.StringIO()) as output:
        viewer.report_log(log, 23)
    assert secret not in output.getvalue() and '"child_exit": "unknown"' in output.getvalue()

# A later fixture failure preserves each completed capture's own result.
attached = dict(metadata='complete', complete_frames=3, relay='no_failure_observed',
                child_exit=0, viewer_exit=0, rendered=True, result='passed', output='bounded', stream='observed')
standalone = dict(attached, complete_frames=0, viewer_exit=1, rendered=False, result='failed')
with tempfile.TemporaryDirectory() as directory:
    log = Path(directory) / 'viewer.log'
    log.write_text('android viewer evidence: ' + json.dumps(attached) + '\n'
                   'E2E_PHASE phase=fixture/viewer-attached-capture state=end duration_seconds=72 exit_code=0\n'
                   'android viewer evidence: ' + json.dumps(standalone) + '\n'
                   'E2E_PHASE phase=fixture/viewer-standalone-capture state=end duration_seconds=936 exit_code=1\n')
    with contextlib.redirect_stdout(io.StringIO()) as output:
        viewer.report_log(log, 1)
    reported = [json.loads(line.removeprefix('android viewer evidence: '))
                for line in output.getvalue().splitlines() if line.startswith('android viewer evidence: ')]
    assert reported == [attached, standalone]
    assert 'android fixture evidence: {"fixture_exit": 1, "output": "bounded"}' in output.getvalue()
    assert 'android capture evidence: ' not in output.getvalue()
    assert 'duration_seconds=72 exit_code=0' in output.getvalue()
    assert 'duration_seconds=936 exit_code=1' in output.getvalue()
    log.write_text('android viewer evidence: ' + json.dumps(attached) + '\n'
                   'android viewer evidence: ' + json.dumps(dict(attached, complete_frames=5)) + '\n')
    with contextlib.redirect_stdout(io.StringIO()) as output:
        viewer.report_log(log, 0)
    reported = [json.loads(line.removeprefix('android viewer evidence: '))
                for line in output.getvalue().splitlines() if line.startswith('android viewer evidence: ')]
    assert reported == [attached, dict(attached, complete_frames=5)]
    assert 'android fixture evidence: {"fixture_exit": 0, "output": "bounded"}' in output.getvalue()
    assert 'android capture evidence: ' not in output.getvalue()
    assert '"rendered": false' not in output.getvalue() and '"result": "failed"' not in output.getvalue()
    for code in (0, 23, 124):
        log.write_bytes(b'x' * 262145 + secret.encode())
        with contextlib.redirect_stdout(io.StringIO()) as output:
            viewer.report_log(log, code)
        assert output.getvalue().strip() == 'android fixture evidence: ' + json.dumps(dict(fixture_exit=code, output='limit'), sort_keys=True)
    for invalid in (dict(attached, extra=secret), dict(attached, metadata=secret),
                    dict(attached, child_exit=256), dict(attached, viewer_exit=True),
                    dict(attached, child_returncode=True), dict(attached, child_returncode=-65),
                    dict(attached, child_returncode=secret), dict(attached, child_returncode=-11),
                    dict(attached, complete_frames=True), dict(attached, result=secret),
                    dict(attached, rendered=False), dict(attached, output=secret)):
        assert not viewer.valid_viewer_summary(invalid)
        log.write_text('android viewer evidence: ' + json.dumps(invalid))
        with contextlib.redirect_stdout(io.StringIO()) as output:
            viewer.report_log(log, 1)
        assert secret not in output.getvalue() and 'android viewer evidence: ' not in output.getvalue()

# The timeout path emits evidence and keeps the original failure exit.
class FakeViewer:
    pid = 999999
    def __init__(self, stream, exit_code, timeout, rendered=True):
        if rendered:
            stream.write(b'INFO: Texture: 640x360\n')
        stream.flush()
        self.exit_code, self.timeout = exit_code, timeout
    def wait(self, timeout):
        if self.timeout:
            self.timeout = False
            raise subprocess.TimeoutExpired('private-command', timeout)
        return self.exit_code

for code, timed_out, rendered, delay, expected in ((23, False, True, False, 23), (0, True, True, False, 124),
                                                (0, False, False, False, 1), (0, False, True, True, 1)):
    def spawn(*args, **kwargs):
        return FakeViewer(kwargs['stdout'], code, timed_out, rendered)
    with patch.object(viewer.subprocess, 'Popen', side_effect=spawn), \
            patch.object(viewer.os, 'killpg'), contextlib.redirect_stdout(io.StringIO()) as output:
        try:
            viewer.view([], 'attached-capture', delay_server=delay)
            raise AssertionError('viewer failure accepted')
        except SystemExit as exc:
            assert exc.code == expected
    assert f'exit_code={expected}' in output.getvalue()
    record = next(json.loads(line.removeprefix('android viewer evidence: '))
                  for line in output.getvalue().splitlines() if line.startswith('android viewer evidence: '))
    assert record['viewer_exit'] == expected and record['result'] == ('timeout' if timed_out else 'failed')
    assert record['child_exit'] == 'unknown'
    assert 'private-command' not in output.getvalue()
    if timed_out:
        assert '"result": "timeout"' in output.getvalue()

def spawn_limited(*args, **kwargs):
    process = FakeViewer(kwargs['stdout'], 0, False)
    kwargs['stdout'].write(b'x' * 262145)
    return process

with patch.object(viewer.subprocess, 'Popen', side_effect=spawn_limited), contextlib.redirect_stdout(io.StringIO()) as output:
    try:
        viewer.view([], 'attached-capture')
        raise AssertionError('oversized capture accepted')
    except SystemExit as exc:
        assert exc.code == 1
record = next(json.loads(line.removeprefix('android viewer evidence: '))
              for line in output.getvalue().splitlines() if line.startswith('android viewer evidence: '))
assert record['rendered'] == 'unknown' and record['viewer_exit'] == 1 and record['result'] == 'failed'

# Invalid direct helper invocations reject before fixture or VM access.
for arguments in ([], ['r', 's', 'y', 'p'], ['r', 's', 'y', 'p', 'i', '--unknown'],
                  ['r', 's', 'y', 'p', 'i', '--viewer-only', '--recovery-only']):
    result = subprocess.run(['bash', str(E2E / 'android-pool-recovery.sh'), *arguments], capture_output=True, text=True)
    assert result.returncode != 0 and 'usage:' in result.stderr
    assert 'E2E_PHASE' not in result.stdout and 'requires the allocated' not in result.stderr
for arguments in (['--lane'], ['--lane', 'unknown'], ['--lane', 'viewer', 'extra'], ['--unknown']):
    result = subprocess.run(['bash', str(E2E / 'android-pool-runtime.sh'), *arguments], capture_output=True, text=True)
    assert result.returncode == 2
    assert 'requires VM1' not in result.stderr and 'E2E_PHASE' not in result.stdout

# Execute the existing mode/prelude/dispatch blocks without guest access.
runtime_source = (E2E / 'android-pool-runtime.sh').read_text()
recovery_source = (E2E / 'android-pool-recovery.sh').read_text()
# Execute the SDK-only early return with harmless transport substitutes. This
# proves dispatch and failure propagation, not the SDK's native build behavior.
full_start = runtime_source.index('if [ "$lane" = full ]; then\n  android_phase_begin sdk-build\n')
full_end = runtime_source.index('\nincus --project "$project" exec "$instance" --user', full_start)
with tempfile.TemporaryDirectory() as directory:
    state = Path(directory)
    definition = state / 'config/yards/fixture/config.env'
    definition.parent.mkdir(parents=True)
    definition.write_text('ENVIRONMENT_PROFILES=android\n')
    prelude = '''set -euo pipefail
lane=full; root="$1"; state="$2"; project=fixture; instance=fixture; YARD_NAME=fixture
SUBYARD_CONFIG_HOME="$state/config"
incus_binary=(fixture-incus)
android_phase_begin() { printf 'phase=%s\n' "$1"; }; android_phase_end() { :; }
timeout() { printf 'build transport\n'; return "${BUILD_EXIT:-0}"; }
yard() {
  case "$*" in
    'config show SRV_POOL') printf 'effective: default\n' ;;
    'config show SRV_VOLUME') printf 'effective: yard-srv-fixture\n' ;;
    *) return 2 ;;
  esac
}
incus() {
  case "$*" in
    '--project fixture config device get fixture srv pool') printf 'default\n' ;;
    '--project fixture config device get fixture srv source') printf 'yard-srv-fixture\n' ;;
    '--project fixture config device get fixture srv path') printf '/srv\n' ;;
    *) return 99 ;;
  esac
}
'''
    for build_exit in (0, 23):
        result = subprocess.run(['bash', '-c', prelude + runtime_source[full_start:full_end]
                                 + '\nprintf reached-diagnostics\n', '_', str(ROOT), directory],
                                capture_output=True, text=True,
                                env=dict(os.environ, BUILD_EXIT=str(build_exit)))
        assert result.returncode == build_exit, result.stderr
        assert 'reached-diagnostics' not in result.stdout
        assert ('android_sdk_build=passed' in result.stdout) is (build_exit == 0)
        assert result.stdout.count('build transport') == 1
        assert all(name not in result.stdout for name in ('phone-recovery', 'prepare-images', 'projects'))

# The focused projects lane reaches its fixture only after readiness and both images.
projects_start = runtime_source.index('if [ "$lane" = projects ]; then\n')
projects_end = runtime_source.index('\nincus --project "$project" exec "$instance" --user', projects_start)
projects_prelude = '''set -euo pipefail
lane="$1"; root="$2"; state='/fixture state'; YARD_NAME=fixture; project=fixture-project; instance=fixture-instance
android_phase_begin() { :; }; android_phase_end() { :; }
incus() { printf 'readiness\\n'; [ "$FAIL_AT" != readiness ] || return 23; }
yard() { printf '%s\\n' "$*"; [ "$FAIL_AT" != "${@: -1}" ] || return 23; }
bash() {
  printf 'fixture-argc=%s\\n' "$#"
  printf 'fixture-arg=%s\\n' "$@"
  [ "$FAIL_AT" != fixture ] || return 23
}
'''
projects_expected = [
    'readiness', 'emu cache prepare --api 35', 'emu cache prepare --api 36',
    'fixture-argc=6', f'fixture-arg={ROOT}/config/profiles/android/tests/e2e/android-pool-projects.sh',
    f'fixture-arg={ROOT}', 'fixture-arg=/fixture state', 'fixture-arg=fixture',
    'fixture-arg=fixture-project', 'fixture-arg=fixture-instance', 'android_projects=passed',
]
for failure, count in (('', len(projects_expected)), ('readiness', 1), ('35', 2), ('36', 3), ('fixture', 10)):
    result = subprocess.run(['bash', '-c', projects_prelude + runtime_source[projects_start:projects_end]
                             + '\nprintf "reached-phone-viewer-setup\\n"\n', '_', 'projects', str(ROOT)],
                            capture_output=True, text=True, env=dict(os.environ, FAIL_AT=failure))
    assert result.returncode == (23 if failure else 0), result.stderr
    assert result.stdout.splitlines() == projects_expected[:count], result.stdout
for lane in ('full', 'diagnostics', 'recovery', 'viewer', 'viewer-native-debug', 'sdk-images', 'memory-lease'):
    result = subprocess.run(['bash', '-c', projects_prelude + runtime_source[projects_start:projects_end]
                             + '\nprintf "continue-existing-lane\\n"\n', '_', lane, str(ROOT)],
                            capture_output=True, text=True, env=dict(os.environ, FAIL_AT=''))
    assert result.returncode == 0 and result.stdout == 'continue-existing-lane\n', result.stderr

# Reconstruct the legacy box with different Docker IDs and a stable canonical name.
projects_source = (E2E / 'android-pool-projects.sh').read_text()
find_start = projects_source.index('find_box() {')
find_end = projects_source.index('\nbox_a=', find_start)
legacy_start = projects_source.index('legacy_box="$(find_box "$name_legacy")"')
legacy_end = projects_source.index('\ndiagnostic_logs=("$work/legacy-check.log")', legacy_start)
with tempfile.TemporaryDirectory(prefix='subyard-android-legacy-', dir='/tmp') as directory:
    fixture = Path(directory)
    (fixture / 'bin').mkdir()
    manifest = fixture / 'env-meta/fixture-project/profile.json'
    manifest.parent.mkdir(parents=True)
    manifest.write_text('{"devices":[]}\n')
    (fixture / 'container').write_text('old-id subyard-box-fixture-project\n')
    docker = fixture / 'bin/docker'
    docker.write_text('''#!/usr/bin/env bash
set -euo pipefail
read -r container_id container_name < "$TEST_ROOT/container" || true
case "$1" in
  ps) [ -z "$container_id" ] || printf '%s\\n' "$container_id" ;;
  inspect)
    target="${@: -1}"
    [ "$target" = "$container_id" ] || [ "$target" = "$container_name" ] || exit 1
    case "$3" in
      '{{.Name}}') printf '/%s\\n' "$container_name" ;;
      *subyard.project*) printf 'fixture-project\\n' ;;
      *subyard.env*) printf '1\\n' ;;
      *subyard.profile*) printf 'android\\n' ;;
      *Mounts*) printf '/srv/workspaces/fixture-project/src\\n' ;;
      *) exit 2 ;;
    esac ;;
  rm) [ "$3" = "$container_id" ]; : > "$TEST_ROOT/container" ;;
  run) [ "$3" = --name ]; printf 'new-id %s\\n' "$4" > "$TEST_ROOT/container" ;;
  *) exit 2 ;;
esac
''')
    docker.chmod(0o755)
    prelude = '''set -euo pipefail
work="$TEST_ROOT"; name_legacy=fixture-project; legacy_sdk=/fixture-sdk; legacy_gradle=/fixture-gradle
bounded_incus_exec() { shift; "$@"; }
'''
    reconstruction = projects_source[legacy_start:legacy_end].replace('/srv/env-meta/', str(fixture / 'env-meta') + '/')
    result = subprocess.run(['bash', '-c', prelude + projects_source[find_start:find_end] + '\n' + reconstruction
                             + '\n[ "$legacy_box" = new-id ]\n'
                             + '[ "$(docker inspect -f "{{.Name}}" "$legacy_box")" = /subyard-box-fixture-project ]\n'],
                            capture_output=True, text=True,
                            env=dict(os.environ, TEST_ROOT=directory, PATH=str(fixture / 'bin') + ':' + os.environ['PATH']))
    assert result.returncode == 0, result.stderr + (fixture / 'legacy-up.log').read_text()
    assert json.loads(manifest.read_text())['devices'] == ['kvm']

mode_block = recovery_source[recovery_source.index('viewer_only=0'):recovery_source.index('root="$1"')]
for arguments, expected in ((['r', 's', 'y', 'p', 'i'], '0 0'),
                            (['r', 's', 'y', 'p', 'i', '--viewer-only'], '1 0'),
                            (['r', 's', 'y', 'p', 'i', '--viewer-native-debug'], '1 0'),
                            (['r', 's', 'y', 'p', 'i', '--recovery-only'], '0 1'),
                            (['r', 's', 'y', 'p', 'i', '--viewer-only', '--recovery-only'], None),
                            (['r', 's', 'y', 'p', 'i', '--unknown'], None)):
    script = 'set -euo pipefail\nfail() { exit 2; }\n' + mode_block + '\nprintf "%s %s\\n" "$viewer_only" "$recovery_only"'
    result = subprocess.run(['bash', '-c', script, '_', *arguments], capture_output=True, text=True)
    assert result.returncode == (0 if expected is not None else 2)
    if expected is not None:
        assert result.stdout.strip() == expected

remote_start = runtime_source.index('if [ "$lane" != viewer ] && [ "$lane" != viewer-native-debug ] && [ "$lane" != sdk-images ]; then\nandroid_phase_begin remote-owner-route')
remote_end = runtime_source.index('android_phase_begin images', remote_start)
images_start = remote_end
images_end = runtime_source.index('android_phase_end 0', images_start)
dispatch_start = runtime_source.index('viewer_args=()')
dispatch_end = runtime_source.index('android_phase_end 0', dispatch_start)
for lane, mode in (('diagnostics', ''), ('viewer', '--viewer-only'), ('recovery', '--recovery-only'), ('viewer-native-debug', '--viewer-native-debug'), ('sdk-images', '')):
    prelude = ('set -euo pipefail\nlane="$1"; root=/fixture; state=/fixture; YARD_NAME=fixture; project=fixture; instance=fixture\n'
               'android_phase_begin() { :; }; android_network_received() { :; }; incus() { :; }; timeout() { printf "%s\\n" "$*"; };\n'
               'yard() { if [ "$*" = "emu catalog" ]; then printf \'%s\\n\' \'{"images":[{"api":36,"variant":"google_apis","abi":"x86_64","cached":true}]}\'; else printf "%s\\n" "$*"; fi; }\n')
    result = subprocess.run(['bash', '-c', prelude + runtime_source[remote_start:remote_end], '_', lane], capture_output=True, text=True, check=True)
    assert ('android-pool-remote.sh' in result.stdout) == (lane not in ('viewer', 'viewer-native-debug', 'sdk-images'))
    result = subprocess.run(['bash', '-c', prelude + runtime_source[images_start:images_end], '_', lane], capture_output=True, text=True, check=True)
    assert result.stdout.count('emu cache prepare --api 35') == (0 if lane == 'sdk-images' else 1 if lane in ('viewer', 'viewer-native-debug') else 2)
    assert result.stdout.count('emu cache prepare --api 36') == 1
    if lane == 'sdk-images':
        # Execute the real early exit before any monitor/emulator/capture dispatch.
        end = runtime_source.index('# Monitor all viewer', images_end)
        result = subprocess.run(['bash', '-c', prelude + 'android_phase_end() { :; };\n'
                                 + runtime_source[images_start:end] + '\nprintf reached-after-images\n', '_', lane],
                                capture_output=True, text=True, check=True)
        assert result.stdout.splitlines() == ['android phase=prepare-images', 'emu cache prepare --api 36',
                                              'emu cache prepare --api 36',
                                              'android image_cache_repeat=PASS cached_before=true']
        continue
    result = subprocess.run(['bash', '-c', prelude + runtime_source[dispatch_start:dispatch_end], '_', lane], capture_output=True, text=True, check=True)
    assert 'android-pool-recovery.sh' in result.stdout
    assert ('--viewer-only' in result.stdout) == (mode == '--viewer-only')
    assert ('--recovery-only' in result.stdout) == (mode == '--recovery-only')
    assert ('--viewer-native-debug' in result.stdout) == (mode == '--viewer-native-debug')

with tempfile.TemporaryDirectory() as directory:
    log = Path(directory) / 'failure.log'
    log.write_text(secret)
    for initial, guest_code, expected in ((23, 1, 23), (0, 1, 3), (0, 0, 0)):
        cleanup = recovery_source[recovery_source.index('cleanup() {'):recovery_source.index('\ntrap cleanup EXIT')]
        script = ('set -euo pipefail\n. "$1/config/profiles/android/tests/e2e/android-pool-phases.sh"\n'
                  'root="$1"; work="$2"; phase_log="$2/failure.log"; lease_dir=/fixture; guest_status="$3"\n'
                  'android_fixture=recovery; android_phase_begin check\n'
                  'guest() { return "$guest_status"; }; incus_exec() { return 124; }\n' + cleanup +
                  '\ntrap cleanup EXIT\nexit "$4"\n')
        result = subprocess.run(['bash', '-c', script, '_', str(ROOT), directory, str(guest_code), str(initial)], capture_output=True, text=True)
        assert result.returncode == expected, result.stderr
        assert secret not in result.stdout + result.stderr
        if guest_code:
            assert 'exit_code=3' in result.stdout

# Diagnostic wrapping occurs at the actual client's Popen boundary, only for scrcpy.
dispatched = []
def dispatch(command, *args, **kwargs):
    dispatched.append((command, args, kwargs))
    return SimpleNamespace(returncode=139)
process_module = SimpleNamespace(Popen=dispatch)
native_client = SimpleNamespace(context=lambda *args: public_context,
                                start_relay=lambda *args: None, rpc=lambda *args, **kwargs: None,
                                start_viewer_relay=lambda *args: server,
                                bridge=lambda *args: None, Error=RuntimeError,
                                subprocess=process_module)
stock = ['scrcpy', '--time-limit=30', '--max-size=640', '--no-audio', '--verbosity=debug',
         '--no-control', '--port=1337', '--tunnel-host=127.0.0.1', '--tunnel-port=1337']
def run_native(arguments):
    assert arguments == ['view', '--lease-file', secret]
    values = native_client.context({}, secret)
    native_client.subprocess.Popen(stock, env=values, start_new_session=True)
    native_client.subprocess.Popen(['adb', 'version'], env=values)
    return 139
native_client.main = run_native
with patch.object(capture.importlib.util, 'spec_from_file_location', return_value=spec), \
        patch.object(capture.importlib.util, 'module_from_spec', return_value=native_client), \
        patch.object(capture.sys, 'argv', ['capture', '--native-debug', 'view', '--lease-file', secret]), \
        contextlib.redirect_stdout(io.StringIO()) as output:
    assert capture.main() == 139
assert process_module.Popen is dispatch
assert dispatched[0][0] == [sys.executable, capture.NATIVE_HELPER, '--gdb', '/usr/bin/gdb', '--', *stock]
assert dispatched[0][2]['env'] is public_context and dispatched[0][2]['start_new_session'] is True
assert dispatched[1][0] == ['adb', 'version'] and dispatched[1][2]['env'] is public_context
assert stock[0] == 'scrcpy' and secret not in output.getvalue()

# Ordinary captures use the bounded stock-client window, while non-viewer calls
# and the native-debug reproducer retain their original dispatch.
dispatched.clear()
stock_window = [part for part in stock if part != '--time-limit=30']
def run_window(arguments):
    assert arguments == ['view', '--lease-file', secret]
    native_client.subprocess.Popen(stock_window, env=public_context, start_new_session=True)
    native_client.subprocess.Popen(['adb', 'version'], env=public_context)
    return 23
native_client.main = run_window
with patch.object(capture.importlib.util, 'spec_from_file_location', return_value=spec), \
        patch.object(capture.importlib.util, 'module_from_spec', return_value=native_client), \
        patch.object(capture.sys, 'argv', ['capture', 'view', '--lease-file', secret]), \
        contextlib.redirect_stdout(io.StringIO()) as output:
    assert capture.main() == 23
assert process_module.Popen is dispatch
assert dispatched[0][0] == [sys.executable, capture.WINDOW_HELPER, '--', '/opt/subyard-e2e-scrcpy/scrcpy', *stock_window[1:]]
assert dispatched[0][2]['env'] is public_context and dispatched[0][2]['start_new_session'] is True
assert dispatched[1][0] == ['adb', 'version'] and secret not in output.getvalue()

native_stack = dict(signal=11, thread=42, frames=[dict(module='scrcpy', symbol='sc_native_crash', offset=1234)])
for native_result in (dict(inferior_returncode=-11, wrapper_status=139, diagnostic='complete'),
                      dict(inferior_returncode=None, wrapper_status=125, diagnostic='invalid_control')):
    raw = ('scrcpy-native-stack ' + json.dumps(native_stack) + '\n'
           + 'scrcpy-native-result ' + json.dumps(native_result) + '\n'
           + 'Android viewer: child returncode ' + str(native_result['wrapper_status']) + '\n')
    summary = viewer.capture_summary(raw, native_result['wrapper_status'])
    assert summary['native_debug'] == dict(stack=native_stack, result=native_result)
    assert summary['child_returncode'] == native_result['wrapper_status'] and viewer.valid_viewer_summary(summary)
    with tempfile.TemporaryDirectory() as directory:
        log = Path(directory) / 'native.log'
        log.write_text('android viewer evidence: ' + json.dumps(summary) + '\n' + secret)
        with contextlib.redirect_stdout(io.StringIO()) as projected:
            viewer.report_log(log, 1)
        assert secret not in projected.getvalue()
        record = json.loads(next(line.split(': ', 1)[1] for line in projected.getvalue().splitlines()
                                 if line.startswith('android viewer evidence: ')))
        assert record['native_debug'] == summary['native_debug']
for malformed in (dict(native_stack, secret=secret), dict(native_stack, thread=True),
                  dict(native_stack, frames=[dict(module='scrcpy', symbol=secret, offset=1)])):
    raw = 'scrcpy-native-stack ' + json.dumps(malformed) + '\nINFO: Texture: 640x360\n'
    summary = viewer.capture_summary(raw, 0)
    assert 'native_debug' not in summary and summary['result'] == 'failed'
    assert secret not in json.dumps(summary)
for malformed in (dict(inferior_returncode=None, wrapper_status=0, diagnostic='complete'),
                  dict(inferior_returncode=True, wrapper_status=0, diagnostic='complete'),
                  dict(inferior_returncode=-11, wrapper_status=0, diagnostic='complete'),
                  dict(inferior_returncode=0, wrapper_status=0, diagnostic='invalid_control'),
                  dict(inferior_returncode=None, wrapper_status=125, diagnostic=secret),
                  dict(inferior_returncode=-65, wrapper_status=193, diagnostic='complete')):
    summary = viewer.capture_summary('scrcpy-native-result ' + json.dumps(malformed) + '\nINFO: Texture: 640x360\n', 0)
    assert summary['result'] == 'failed' and 'native_debug' not in summary
    assert secret not in json.dumps(summary)
complete = dict(inferior_returncode=0, wrapper_status=0, diagnostic='complete')
summary = viewer.capture_summary('scrcpy-native-result ' + json.dumps(complete) + '\n', 0)
assert summary['result'] != 'passed'  # GDB success still requires the original Texture assertion.
summary = viewer.capture_summary('scrcpy-native-result ' + json.dumps(complete) + '\nINFO: Texture: 640x360\n', 0)
assert summary['result'] == 'passed' and viewer.valid_viewer_summary(summary)


# Run the actual owner PATH-shim setup with harmless executables and no VM calls.
owner_start = recovery_source.index('owner_viewer_path=')
owner_end = recovery_source.index('owner_lease=', owner_start)
with tempfile.TemporaryDirectory() as directory:
    fixture = Path(directory)
    work = fixture / 'work'
    tools = work / 'subyard-e2e-scrcpy'
    tools.mkdir(parents=True)
    stock_script = tools / 'scrcpy'
    stub = "#!/usr/bin/python3\nimport json,os,sys\nprint(json.dumps(dict(argv=sys.argv[1:], adb=os.environ['ADB'], renderer=os.environ['SDL_RENDER_DRIVER'])))\n"
    stock_script.write_text(stub)
    stock_script.chmod(0o755)
    fixture_helper = fixture / 'config/profiles/android/tests/helpers/scrcpy-native-debug.py'
    fixture_helper.parent.mkdir(parents=True)
    fixture_helper.write_text(stub)
    fixture_helper.with_name('scrcpy-view-window.py').write_text(stub)
    for diagnostic in ('0', '1'):
        script = ('set -euo pipefail\nroot="$1"; work="$2"; native_debug="$3"\n'
                  + recovery_source[owner_start:owner_end]
                  + '\nenv PATH="$owner_viewer_path" ADB=private-test-value SDL_RENDER_DRIVER=software scrcpy "${viewer_window_args[@]}" --max-size=640 --no-audio --verbosity=debug\n')
        result = subprocess.run(['bash', '-c', script, '_', str(fixture), str(work), diagnostic],
                                capture_output=True, text=True, check=True)
        record = json.loads(result.stdout)
        argv = (['--time-limit=30'] if diagnostic == '1' else []) + ['--max-size=640', '--no-audio', '--verbosity=debug']
        if diagnostic == '1':
            assert record['argv'][:3] == ['--gdb', '/usr/bin/gdb', '--']
            assert Path(record['argv'][3]).resolve() == stock_script
            assert record['argv'][4:] == argv
        else:
            assert record['argv'][0] == '--'
            assert Path(record['argv'][1]).resolve() == stock_script
            assert record['argv'][2:] == argv
        assert record['adb'] == 'private-test-value' and record['renderer'] == 'software'

# The viewer's real launch path preserves its deadline and fails without native completion.
for diagnostic, native_output, code, expected in (
        (False, '', 0, 0), (True, '', 0, 1),
        (True, 'scrcpy-native-result ' + json.dumps(complete) + '\n', 0, 0),
        (True, 'scrcpy-native-result ' + json.dumps(dict(inferior_returncode=-11, wrapper_status=139, diagnostic='complete')) + '\n', 139, 139)):
    def launch(command, **kwargs):
        assert command[:6] == ['xvfb-run', '-a', '-s', '-screen 0 1280x800x24 -nolisten tcp', sys.executable, '/opt/subyard-e2e-capture.py']
        assert command[6:] == ([ '--native-debug'] if diagnostic else []) + ['view', '--lease-file', secret, '--'] + (['--time-limit=30'] if diagnostic else []) + ['--max-size=640', '--no-audio', '--verbosity=debug']
        assert kwargs['env']['SDL_RENDER_DRIVER'] == 'software'
        kwargs['stdout'].write(('INFO: Texture: 640x360\n' + native_output).encode())
        def wait(timeout):
            assert timeout == 1320
            return code
        return SimpleNamespace(wait=wait)
    with patch.object(viewer.subprocess, 'Popen', side_effect=launch), contextlib.redirect_stdout(io.StringIO()) as projected:
        try:
            viewer.view(['--lease-file', secret], 'local-dispatch', native_debug=diagnostic)
            assert expected == 0
        except SystemExit as error:
            assert error.code == expected
    assert secret not in projected.getvalue()

print('ok: Android capture, display, privacy, bounds, lane and cleanup evidence')
