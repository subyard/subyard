#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../" && pwd)"
HOOK="$ROOT/config/profiles/android/provision.sh"
tmp="$(mktemp -d /tmp/subyard-android-check.XXXXXX)"
trap 'rm -rf -- "$tmp"' EXIT
test_root="$tmp/root"
fake_bin="$tmp/bin"
mkdir -p "$fake_bin" "$test_root/srv/cache/gradle" \
  "$test_root/srv/cache/android-sdk/.subyard/bin" "$test_root/etc/profile.d" \
  "$test_root/etc/systemd/system" "$test_root/usr/local/lib/subyard-android"

# One explicit fixture selection drives preparation and the real shell/CLI check.
fixture_versions="$(python3 -c 'import json,sys; m=json.load(open(sys.argv[1])); print(m["android_api"]+"\t"+m["jdk"]["major"]+"\t"+m["build_tools"])' "$ROOT/config/profiles/android/toolchain.json")"
IFS=$'\t' read -r fixture_api fixture_jdk fixture_build <<< "$fixture_versions"
common_env=(
  PATH="$fake_bin:$PATH"
  TMPDIR=/tmp
  ANDROID_TEST_ROOT="$test_root"
  DEV_USER="$(id -un)"
  ANDROID_API="$fixture_api"
  JDK_VERSION="$fixture_jdk"
  BUILD_TOOLS_VERSION="$fixture_build"
  ANDROID_SDK_ROOT=/srv/cache/android-sdk
  ANDROID_PUBLIC_ROOT=/srv/cache/android-sdk/.subyard
  GRADLE_USER_HOME=/srv/cache/gradle
  YARD_GRADLE_USER_HOME=/srv/cache/gradle
)

# Populate the same selected identities used by production check, without downloads.
env "${common_env[@]}" python3 -B - "$ROOT/config/profiles/android" "$test_root" <<'PYTOOLCHAIN'
import importlib.util, os, pathlib, sys
profile, root = map(pathlib.Path, sys.argv[1:])
spec = importlib.util.spec_from_file_location('fixture', profile / 'tests/toolchain-fixture.py')
fixture = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fixture)
entries = fixture.toolchain.selected(profile / 'toolchain.json', environment=os.environ, architecture='x86_64')
for name, entry in entries.items():
    fixture.fixture(pathlib.Path(str(root) + entry['destination']), name, entry)
PYTOOLCHAIN
cat > "$test_root/etc/profile.d/subyard-android.sh" <<ENV
export JAVA_HOME="/opt/jdk-$fixture_jdk"
export ANDROID_HOME="/srv/cache/android-sdk"
export ANDROID_SDK_ROOT="/srv/cache/android-sdk"
export GRADLE_USER_HOME="/srv/cache/gradle"
export PATH="/srv/cache/android-sdk/.subyard/bin:/opt/jdk-$fixture_jdk/bin:/srv/cache/android-sdk/platform-tools:/srv/cache/android-sdk/cmdline-tools/latest/bin:\$PATH"
ENV
printf '{"sdk_root":"/srv/cache/android-sdk","state_root":"/var/lib/subyard-android","public_root":"/srv/cache/android-sdk/.subyard","size":2,"api":%s,"device":"phone","variant":"google_apis","abi":"x86_64","gpu":"host","dev_user":"%s","runtime_user":"subyard-android","java_home":"/opt/jdk-%s","image_policy":"latest-stable"}\n' "$fixture_api" "$(id -un)" "$fixture_jdk" \
  > "$test_root/etc/subyard-android.json"
cat > "$test_root/etc/systemd/system/subyard-android-pool.service" <<'UNIT'
ExecStart=/usr/bin/python3 /usr/local/lib/subyard-android/pool.py serve
KillMode=control-group
TimeoutStopSec=120
MemoryAccounting=yes
MemoryMax=2G
MemorySwapMax=2G
Slice=system.slice
UNIT
for source in pool.py client.py runtime.sh; do
  cp "$ROOT/config/profiles/android/$source" "$test_root/usr/local/lib/subyard-android/$source"
done
printf '#!/usr/bin/env bash\nexit 0\n' > "$test_root/srv/cache/android-sdk/.subyard/bin/android-broker"
chmod +x "$test_root/srv/cache/android-sdk/.subyard/bin/android-broker"
env "${common_env[@]}" /usr/bin/python3 -c 'import socket, sys; s=socket.socket(socket.AF_UNIX); s.bind(sys.argv[1])' \
  "$test_root/srv/cache/android-sdk/.subyard/control.sock"
for command in curl unzip flock setsid cage Xwayland socat systemctl slirp4netns ip; do
  printf '#!/usr/bin/env bash\nexit 0\n' > "$fake_bin/$command"
  chmod +x "$fake_bin/$command"
done
printf '#!/usr/bin/env bash\nprintf "%%s\\n" "%s:%s"\n' "$(id -u)" "$(id -g)" > "$fake_bin/docker"
chmod +x "$fake_bin/docker"
printf '#!/usr/bin/env bash\nexit 99\n' > "$fake_bin/apt-get"
chmod +x "$fake_bin/apt-get"
cat > "$fake_bin/dpkg-query" <<'EOF'
#!/usr/bin/env bash
[ "${@: -1}" != "${FAKE_ANDROID_MISSING_LIBRARY:-}" ] || exit 1
printf 'install ok installed\n'
EOF
chmod +x "$fake_bin/dpkg-query"

# Only this disposable fixture models an x86_64 toolchain on every CI architecture.
# The production helper has no architecture override; toolchain-test.py checks rejection.
cat > "$fake_bin/python3" <<'PYTHON'
#!/usr/bin/python3
import os
from pathlib import Path
import platform
import runpy
import sys
if len(sys.argv) > 1 and Path(sys.argv[1]).name == 'toolchain.py':
    platform.machine = lambda: 'x86_64'
    sys.argv = sys.argv[1:]
    runpy.run_path(sys.argv[0], run_name='__main__')
else:
    os.execv('/usr/bin/python3', ['/usr/bin/python3', *sys.argv[1:]])
PYTHON
chmod +x "$fake_bin/python3"


# Exercise the actual protected registration contract inside the disposable root.
mkdir -p "$test_root/usr/local/libexec/subyard" "$tmp/guest-home"
cat > "$test_root/usr/local/libexec/subyard/agent-skills" <<WRAPPER
import runpy,sys
sys.argv += ['--root', '$test_root']
runpy.run_path('$ROOT/config/skills/deliver.py',run_name='__main__')
WRAPPER
chmod 0755 "$test_root/usr/local/libexec/subyard/agent-skills"
cat > "$fake_bin/getent" <<GETENT
#!/usr/bin/env bash
printf '%s:x:%s:%s:fixture:%s:/bin/bash\n' '$(id -un)' '$(id -u)' '$(id -g)' '$tmp/guest-home'
GETENT
chmod 0755 "$fake_bin/getent"
env "${common_env[@]}" python3 "$ROOT/config/skills/deliver.py" apply --root "$test_root" --provider profile-android \
  --source "$ROOT/config/profiles/android/skills" --home "$tmp/guest-home" --uid "$(id -u)" --gid "$(id -g)"


expect_check() {
  local expected="$1" status=0
  shift
  env "${common_env[@]}" "$@" bash "$HOOK" --check >/dev/null 2>&1 || status=$?
  [ "$status" -eq "$expected" ] || {
    printf 'FAIL: Android check at line %s: status=%s, want %s\n' "${BASH_LINENO[0]}" "$status" "$expected" >&2; exit 1;
  }
}

before="$(find "$tmp" -type f -exec sha256sum {} + | sort | sha256sum)"
expect_check 0
after="$(find "$tmp" -type f -exec sha256sum {} + | sort | sha256sum)"
[ "$before" = "$after" ] || { printf 'FAIL: Android check mutated state\n' >&2; exit 1; }

# Static content drift belongs to common config refresh, not SDK/pool provisioning.
printf '\nChanged guidance.\n' >> "$test_root/usr/local/share/subyard/skills/subyard-android/SKILL.md"
expect_check 0
receipt="$test_root/var/lib/subyard/agent-skills/profile-android.json"
mv "$receipt" "$tmp/registration"
expect_check 10
mv "$tmp/registration" "$receipt"
chmod 0644 "$receipt"
expect_check 10
chmod 0600 "$receipt"


# The renamed client is required; retired aliases must trigger upgrade cleanup.
client_bin="$test_root/srv/cache/android-sdk/.subyard/bin"
mv "$client_bin/android-broker" "$tmp/android-broker"
expect_check 10
mv "$tmp/android-broker" "$client_bin/android-broker"
for retired in yard yard-emu; do
  ln -s missing-client "$client_bin/$retired"
  expect_check 10
  rm "$client_bin/$retired"
done
expect_check 0

# Runtime dependencies must converge even when SDK binaries already exist.
for library in libpulse0 libnss3; do
  expect_check 10 FAKE_ANDROID_MISSING_LIBRARY="$library"
done

# SDK command discovery remains part of ordinary builds, independent of emulator leases.
profile_env="$test_root/etc/profile.d/subyard-android.sh"
cp "$profile_env" "$tmp/profile-env"
sed -i 's@:/srv/cache/android-sdk/cmdline-tools/latest/bin@@' "$profile_env"
expect_check 10
cp "$tmp/profile-env" "$profile_env"

rm -f "$test_root/srv/cache/android-sdk/platform-tools/adb"
expect_check 10

# Exercise the owning download boundary with native curl against a local server.
# No provision/install commands or remote downloads run on the host.
env "${common_env[@]}" PATH=/usr/bin:/bin python3 - "$ROOT/config/profiles/android/provision-helpers.sh" <<'PYDOWNLOAD'
import http.server
import os
import pathlib
import socket
import struct
import subprocess
import sys
import tempfile
import threading

helper = pathlib.Path(sys.argv[1])
canary = 'android-download-private-canary'
payload = b'complete toolchain input\n'

class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_GET(self):
        self.server.calls += 1
        if self.server.scenario == 'transient' and self.server.calls >= 3:
            self.send_response(200)
            self.send_header('Content-Length', str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)
            return
        # A partial response followed by a reset exercises curl's transport retry.
        self.send_response(200)
        self.send_header('Content-Length', '100000')
        self.end_headers()
        self.wfile.write(canary.encode())
        self.wfile.flush()
        self.connection.setsockopt(socket.SOL_SOCKET, socket.SO_LINGER, struct.pack('ii', 1, 0))
        self.close_connection = True

with tempfile.TemporaryDirectory(prefix='subyard-android-download-', dir='/tmp') as directory:
    root = pathlib.Path(directory)
    destination = root / 'download'
    def download(url, stage, env=None):
        env = dict(os.environ if env is None else env, NO_PROXY='127.0.0.1', no_proxy='127.0.0.1')
        return subprocess.run(['bash', '-eu', '-c', '. "$4"; android_download "$1" "$2" "$3"',
                               'android-download-test', url, str(destination), stage, str(helper)],
                              env=env, capture_output=True, timeout=15)

    for scenario in ('transient', 'exhausted'):
        stage = 'jdk' if scenario == 'transient' else 'cmdline-tools'
        with http.server.HTTPServer(('127.0.0.1', 0), Handler) as server:
            server.scenario, server.calls = scenario, 0
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            try:
                result = download(f'http://127.0.0.1:{server.server_port}/{canary}', stage)
            finally:
                server.shutdown()
                thread.join()
            assert canary.encode() not in result.stdout + result.stderr
            assert result.stdout == b''
            if scenario == 'transient':
                assert result.returncode == 0 and server.calls == 3
                assert destination.read_bytes() == payload  # No retained partial response.
            else:
                assert result.returncode != 0 and server.calls == 4
                assert not destination.exists()

    # Shorten only the fixture's clock: native timeout must remain fatal and reap curl.
    fake_bin = root / 'bin'
    fake_bin.mkdir(mode=0o700)
    pid_file = root / 'curl.pid'
    scripts = {
        'timeout': '#!/bin/bash\nshift 3\nexec /usr/bin/timeout --signal=TERM --kill-after=1s 0.2s "$@"\n',
        'curl': f'#!/bin/bash\nprintf "%s\\n" "$$" > "{pid_file}"\nexec sleep 20\n',
    }
    for name, text in scripts.items():
        path = fake_bin / name
        path.write_text(text)
        path.chmod(0o700)
    # A missing output is unknown, rather than a claim that the transfer saw zero bytes.
    for existing_output in (True, False):
        if existing_output:
            destination.write_text(canary)
        result = download('http://127.0.0.1/unused', 'jdk',
                          dict(os.environ, PATH=f'{fake_bin}:{os.environ["PATH"]}'))
        assert result.returncode == 124 and not destination.exists()
        assert canary.encode() not in result.stdout + result.stderr and not result.stdout
        try:
            os.kill(int(pid_file.read_text()), 0)
        except ProcessLookupError:
            pass
        else:
            raise AssertionError('timed-out curl remains alive')
print('ok: bounded Android downloads recover or fail without publishing partial inputs')
PYDOWNLOAD

# The declaration keeps the pre-existing migration target visible to the generic runner.
env "${common_env[@]}" python3 - "$ROOT/config/profiles/android" "$tmp" <<'PYLEGACY'
import pathlib, subprocess, sys
profile, fixture = map(pathlib.Path, sys.argv[1:])
paths = (profile / 'provision-tmp-paths').read_text().splitlines()
assert paths == ['/tmp/subyard-android']
legacy = fixture / "legacy ' root"
legacy.mkdir()
unrelated = legacy / 'operator-avd'
unrelated.write_bytes(b'untouched')
def refresh(root):
    script = '''before=$(pwd; set +o; trap -p)
. "$1"
[ "$before" = "$(pwd; set +o; trap -p)" ]
android_refresh_legacy_helpers "$2" "$3"
'''
    subprocess.run(['bash', '-euc', script,
                    'android-legacy-test', str(profile/'provision-helpers.sh'), str(root), str(profile)],
                   check=True)
refresh(legacy)
assert unrelated.read_bytes() == b'untouched'
missing = fixture/'missing-legacy'
refresh(missing)
assert not missing.exists()
target = fixture/'foreign-target'
target.mkdir()
linked = fixture/'linked-legacy'
linked.symlink_to(target, target_is_directory=True)
refresh(linked)
assert linked.is_symlink() and not list(target.iterdir())
for name in ('emulator-run.sh', 'emulator-control.sh'):
    assert (legacy / name).read_bytes() == (profile / name).read_bytes()
    assert (legacy / name).stat().st_mode & 0o777 == 0o755
PYLEGACY

printf 'ok: Android provision check\n'
