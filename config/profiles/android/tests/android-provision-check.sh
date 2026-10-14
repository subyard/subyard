#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../" && pwd)"
HOOK="$ROOT/config/profiles/android/provision.sh"
tmp="$(mktemp -d)"
trap 'rm -rf -- "$tmp"' EXIT
test_root="$tmp/root"
fake_bin="$tmp/bin"
mkdir -p "$fake_bin" "$test_root/opt/jdk-17/bin" \
  "$test_root/srv/cache/android-sdk/cmdline-tools/latest/bin" \
  "$test_root/srv/cache/android-sdk/platform-tools" \
  "$test_root/srv/cache/android-sdk/platforms/android-36" \
  "$test_root/srv/cache/android-sdk/build-tools/36.0.0" \
  "$test_root/srv/cache/android-sdk/emulator" \
  "$test_root/srv/cache/android-sdk/licenses" "$test_root/srv/cache/gradle" \
  "$test_root/srv/cache/android-sdk/.subyard/bin" "$test_root/etc/profile.d" \
  "$test_root/etc/systemd/system" "$test_root/usr/local/lib/subyard-android"

for command in java sdkmanager adb emulator; do
  case "$command" in
    java) path="$test_root/opt/jdk-17/bin/java" ;;
    sdkmanager) path="$test_root/srv/cache/android-sdk/cmdline-tools/latest/bin/sdkmanager" ;;
    adb) path="$test_root/srv/cache/android-sdk/platform-tools/adb" ;;
    emulator) path="$test_root/srv/cache/android-sdk/emulator/emulator" ;;
  esac
  printf '#!/usr/bin/env bash\nexit 0\n' > "$path"
  chmod +x "$path"
done
printf 'accepted\n' > "$test_root/srv/cache/android-sdk/licenses/android-sdk-license"
cat > "$test_root/etc/profile.d/subyard-android.sh" <<'ENV'
export JAVA_HOME="/opt/jdk-17"
export ANDROID_HOME="/srv/cache/android-sdk"
export ANDROID_SDK_ROOT="/srv/cache/android-sdk"
export GRADLE_USER_HOME="/srv/cache/gradle"
export PATH="/srv/cache/android-sdk/.subyard/bin:/opt/jdk-17/bin:/srv/cache/android-sdk/platform-tools:/srv/cache/android-sdk/cmdline-tools/latest/bin:$PATH"
ENV
printf '{"sdk_root":"/srv/cache/android-sdk","state_root":"/var/lib/subyard-android","public_root":"/srv/cache/android-sdk/.subyard","size":2,"api":36,"device":"phone","variant":"google_apis","abi":"x86_64","gpu":"host","dev_user":"%s","runtime_user":"subyard-android","java_home":"/opt/jdk-17"}\n' "$(id -un)" \
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
/usr/bin/python3 -c 'import socket, sys; s=socket.socket(socket.AF_UNIX); s.bind(sys.argv[1])' \
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

common_env=(
  PATH="$fake_bin:$PATH"
  ANDROID_TEST_ROOT="$test_root"
  DEV_USER="$(id -un)"
  ANDROID_API=36
  JDK_VERSION=17
  BUILD_TOOLS_VERSION=36.0.0
  SYSTEM_IMAGE='system-images;android-36;google_apis;x86_64'
  ANDROID_SDK_ROOT=/srv/cache/android-sdk
  GRADLE_USER_HOME=/srv/cache/gradle
)

before="$(find "$tmp" -type f -exec sha256sum {} + | sort | sha256sum)"
env "${common_env[@]}" bash "$HOOK" --check >/dev/null
after="$(find "$tmp" -type f -exec sha256sum {} + | sort | sha256sum)"
[ "$before" = "$after" ] || { printf 'FAIL: Android check mutated state\n' >&2; exit 1; }

# The renamed client is required; retired aliases must trigger upgrade cleanup.
client_bin="$test_root/srv/cache/android-sdk/.subyard/bin"
mv "$client_bin/android-broker" "$tmp/android-broker"
status=0
env "${common_env[@]}" bash "$HOOK" --check >/dev/null || status=$?
[ "$status" -eq 10 ] || { printf 'FAIL: missing broker client accepted\n' >&2; exit 1; }
mv "$tmp/android-broker" "$client_bin/android-broker"
for retired in yard yard-emu; do
  ln -s missing-client "$client_bin/$retired"
  status=0
  env "${common_env[@]}" bash "$HOOK" --check >/dev/null || status=$?
  [ "$status" -eq 10 ] || { printf 'FAIL: retired client accepted\n' >&2; exit 1; }
  rm "$client_bin/$retired"
done
env "${common_env[@]}" bash "$HOOK" --check >/dev/null

# Runtime dependencies must converge even when SDK binaries already exist.
for library in libpulse0 libnss3; do
  status=0
  env "${common_env[@]}" FAKE_ANDROID_MISSING_LIBRARY="$library" bash "$HOOK" --check >/dev/null || status=$?
  [ "$status" -eq 10 ] || { printf 'FAIL: missing %s status=%s, want 10\n' "$library" "$status" >&2; exit 1; }
done

# SDK command discovery remains part of ordinary builds, independent of emulator leases.
profile_env="$test_root/etc/profile.d/subyard-android.sh"
cp "$profile_env" "$tmp/profile-env"
sed -i 's@:/srv/cache/android-sdk/cmdline-tools/latest/bin@@' "$profile_env"
status=0
env "${common_env[@]}" bash "$HOOK" --check >/dev/null || status=$?
[ "$status" -eq 10 ] || { printf 'FAIL: missing SDK tools PATH status=%s, want 10\n' "$status" >&2; exit 1; }
cp "$tmp/profile-env" "$profile_env"

# Command-line Tools 23 can install components without the legacy licenses directory.
rm -f "$test_root/srv/cache/android-sdk/licenses/android-sdk-license"
env "${common_env[@]}" bash "$HOOK" --check >/dev/null

rm -f "$test_root/srv/cache/android-sdk/platform-tools/adb"
set +e
env "${common_env[@]}" bash "$HOOK" --check >/dev/null
status=$?
set -e
[ "$status" -eq 10 ] || { printf 'FAIL: Android drift status=%s, want 10\n' "$status" >&2; exit 1; }

# Exercise the owning download boundary with native curl against a local server.
# No provision/install commands or remote downloads run on the host.
python3 - "$HOOK" <<'PYDOWNLOAD'
import http.server
import os
import pathlib
import re
import socket
import struct
import subprocess
import sys
import tempfile
import threading

source = pathlib.Path(sys.argv[1]).read_text()
start = source.index('android_download() {')
function = source[start:source.index('\n}\n', start) + 3]
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
        return subprocess.run(['bash', '-eu', '-c', function + '\nandroid_download "$1" "$2" "$3"',
                               'android-download-test', url, str(destination), stage],
                              env=env, capture_output=True, timeout=15)

    for scenario in ('transient', 'exhausted'):
        stage = 'jdk' if scenario == 'transient' else 'cmdline-tools'
        start_line = f'android provision: download stage={stage} state=start\n'.encode()
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
                assert result.stderr == start_line
            else:
                assert result.returncode != 0 and server.calls == 4
                assert not destination.exists()
                assert re.fullmatch(start_line +
                    rb'android provision: download stage=cmdline-tools state=failed exit=\d+ elapsed_seconds=\d+ last_output_bytes=' +
                    str(len(canary)).encode() + rb'\n', result.stderr)

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
        result = download('http://127.0.0.1/unused', 'catalog',
                          dict(os.environ, PATH=f'{fake_bin}:{os.environ["PATH"]}'))
        assert result.returncode == 124 and not destination.exists()
        count = str(len(canary)).encode() if existing_output else b'unknown'
        assert re.fullmatch(
            rb'android provision: download stage=catalog state=start\n'
            rb'android provision: download stage=catalog state=failed exit=124 elapsed_seconds=\d+ last_output_bytes=' +
            count + rb'\n', result.stderr)
        assert canary.encode() not in result.stdout + result.stderr and not result.stdout
        try:
            os.kill(int(pid_file.read_text()), 0)
        except ProcessLookupError:
            pass
        else:
            raise AssertionError('timed-out curl remains alive')
print('ok: bounded Android downloads recover or fail without publishing partial inputs')
PYDOWNLOAD

printf 'ok: Android provision check\n'
