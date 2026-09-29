#!/usr/bin/env python3
"""Real Android lease lifecycle; run as dev inside the fixture-owned Android yard."""
import calendar
import json
import os
import re
import signal
import socket
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
import time
EMU = os.environ.get("YARD_EMU", "android-broker")
ADB = os.environ.get("ADB", "/srv/cache/android-sdk/platform-tools/adb")
if not Path(ADB).is_file():
    ADB = "adb"
class Failure(Exception):
    pass
def call(args, timeout=90, env=None):
    try:
        return subprocess.run(args, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                              stderr=subprocess.PIPE, text=True, timeout=timeout, env=env)
    except (OSError, subprocess.TimeoutExpired) as exc:
        raise Failure("command unavailable or timed out") from exc
def read_lease(path, device, api):
    try:
        info = path.stat()
        if info.st_uid != os.getuid() or info.st_mode & 0o077:
            raise ValueError
        lease = json.loads(path.read_text())
        allocation = lease["allocation"]
        if (not isinstance(lease["token"], str) or not isinstance(lease["endpoint"], str) or
                not isinstance(allocation["slot_id"], str) or
                not isinstance(allocation["generation"], int) or
                allocation["request"]["device"] != device or allocation["request"]["api"] != api):
            raise ValueError
        return lease
    except (OSError, KeyError, TypeError, ValueError, json.JSONDecodeError) as exc:
        raise Failure("invalid lease response") from exc
def acquire(directory, name, device, api):
    path = directory / (name + ".json")
    result = call([EMU, "acquire", "--device", device, "--api", str(api),
                   "--lease-file", str(path)], timeout=1320)
    if result.returncode != 0 or not path.is_file():
        kind = re.search(r'^Android ([a-z_]+):', result.stderr)
        child = re.search(r'\(exit (-?\d+)\)', result.stderr)
        raise Failure(f"acquire failed (client={result.returncode}, "
                      f"code={kind[1] if kind else 'unknown'}, installer={child[1] if child else 'unknown'})")
    return path, read_lease(path, device, api)
def adb(lease, *args, timeout=120):
    allocation = lease["allocation"]
    env = dict(os.environ, ADB_SERVER_SOCKET="localfilesystem:" + lease["endpoint"],
               ANDROID_SERIAL=allocation["android_serial"])
    result = call([ADB, *args], timeout=timeout, env=env)
    if result.returncode != 0:
        detail = re.sub(r'(?i)(token|credential|secret|password|authorization|api[_-]?key)[=: ]\S+',
                        '[redacted]', result.stderr.strip())
        detail = re.sub(r'\b[0-9a-fA-F]{64}\b', '[redacted]', detail)
        raise Failure(f"ADB command failed (exit {result.returncode}): {detail[:400] or 'no stderr'}")
    return result.stdout.strip()
class Leases:
    def __init__(self):
        self.paths, self.lock = set(), threading.Lock()
        self.stop, self.failed = threading.Event(), threading.Event()
        self.thread = threading.Thread(target=self._heartbeat, daemon=True)
    def start(self):
        self.thread.start()
    def add(self, path):
        with self.lock:
            self.paths.add(path)
    def release(self, path):
        with self.lock:
            result = call([EMU, "release", "--lease-file", str(path)], timeout=120)
            if result.returncode != 0:
                raise Failure("release failed")
            self.paths.discard(path)
    def _heartbeat(self):
        while not self.stop.wait(45):
            with self.lock:
                for path in self.paths:
                    try:
                        if call([EMU, "renew", "--lease-file", str(path)], timeout=30).returncode != 0:
                            self.failed.set()
                    except Failure:
                        self.failed.set()
    def check(self):
        if self.failed.is_set():
            raise Failure("lease renewal failed")
    def close_and_release(self):
        self.stop.set()
        self.thread.join(timeout=35)
        with self.lock:
            paths = list(self.paths)
            self.paths.clear()
        for path in paths:
            try:
                call([EMU, "release", "--lease-file", str(path)], timeout=120)
            except Failure:
                pass
def identity(lease):
    allocation = lease['allocation']
    env = dict(os.environ, ADB_SERVER_SOCKET="localfilesystem:" + lease['endpoint'],
               ANDROID_SERIAL=allocation['android_serial'])
    observed = []
    for name in ('ro.build.version.sdk', 'ro.product.cpu.abi'):
        try:
            result = call([ADB, 'shell', 'getprop', name], timeout=120, env=env)
            value = result.stdout.strip()
            if name.endswith('.sdk'):
                value = value if re.fullmatch(r'[0-9]{1,3}', value) else 'other'
            else:
                value = value if value in ('x86_64', 'x86', 'arm64-v8a') else 'other'
            observed.append((value, result.returncode))
        except Failure:
            observed.append(('unavailable', 'timeout'))
    return observed
def identity_ok(lease, api, observed):
    return observed[0] == (str(api), 0) and observed[1] == ('x86_64', 0) \
        and bool(lease['allocation'].get('image_revision'))
def identity_diagnostic(lease, api, observed):
    allocation = lease['allocation']
    env = dict(os.environ, ADB_SERVER_SOCKET="localfilesystem:" + lease['endpoint'],
               ANDROID_SERIAL=allocation['android_serial'])
    try:
        result = call([ADB, 'shell', 'getprop ro.build.version.sdk; '
                       'getprop ro.product.cpu.abi; getprop sys.boot_completed'],
                      timeout=20, env=env)
        lines = result.stdout.splitlines()
        direct = [lines[index].strip() if index < len(lines) else '' for index in range(3)]
        direct_sdk = direct[0] if re.fullmatch(r'[0-9]{1,3}', direct[0]) else 'other'
        direct_abi = direct[1] if direct[1] in ('x86_64', 'x86', 'arm64-v8a') else 'other'
        direct_boot = direct[2] if direct[2] in ('0', '1') else 'other'
        direct_rc = result.returncode
    except Failure:
        direct_sdk = direct_abi = direct_boot = 'unavailable'
        direct_rc = 'timeout'
    slot = allocation['slot_id'] if re.fullmatch(r'[0-9]{3}', allocation['slot_id']) else 'invalid'
    generation = allocation['generation'] if 0 <= allocation['generation'] < 1000000000 else 'invalid'
    print(f'android pool lifecycle identity slot={slot} gen={generation} request_api={api} '
          f'observed_sdk={observed[0][0]} sdk_rc={observed[0][1]} '
          f'observed_abi={observed[1][0]} abi_rc={observed[1][1]} '
          f'direct_sdk={direct_sdk} direct_abi={direct_abi} '
          f'direct_boot={direct_boot} direct_rc={direct_rc}', file=sys.stderr, flush=True)
def require_sdk(lease, api):
    observed = identity(lease)
    if not identity_ok(lease, api, observed):
        identity_diagnostic(lease, api, observed)
        raise Failure('unexpected Android API, ABI or image revision')
def require_sdk_pair(phone, tablet):
    checks = [(phone, 35, identity(phone)), (tablet, 36, identity(tablet))]
    if not all(identity_ok(lease, api, observed) for lease, api, observed in checks):
        for lease, api, observed in checks:
            identity_diagnostic(lease, api, observed)
        raise Failure('concurrent Android API, ABI or image revision mismatch')
    result = call([EMU, 'catalog'])
    if result.returncode != 0:
        raise Failure('catalog unavailable while validating allocations')
    images = json.loads(result.stdout)['images']
    for lease, api, size, density in ((phone, 35, '1080x1920', '420'),
                                       (tablet, 36, '800x1280', '160')):
        allocation = lease['allocation']
        request = allocation['request']
        if not any(image['api'] == api and image['variant'] == request['variant']
                   and image['abi'] == request['abi']
                   and image['revision'] == allocation['image_revision'] and image['cached']
                   for image in images):
            raise Failure('allocation image differs from prepared catalog revision')
        if (adb(lease, 'shell', 'wm', 'size') != 'Physical size: ' + size or
                adb(lease, 'shell', 'wm', 'density') != 'Physical density: ' + density):
            raise Failure('live device dimensions or density differ from preset')
    phase('both APIs, image revisions and physical presets verified')
def require_network_and_renderer(lease):
    # Android ships toybox nc; a DNS name plus HTTP response proves DNS and TCP egress.
    phase('verify DNS/TCP egress')
    # ConnectivityService/DNS can finish configuring after the guest receives IPv4.
    # Bound external reachability here; pool readiness must not depend on this public site.
    deadline = time.monotonic() + 90
    last_error = 'unexpected HTTP response'
    while time.monotonic() < deadline:
        try:
            reply = adb(lease, 'shell', "printf 'GET /generate_204 HTTP/1.0\\r\\nHost: connectivitycheck.gstatic.com\\r\\n\\r\\n' "
                        '| toybox nc -4 -w 10 -W 15 connectivitycheck.gstatic.com 80',
                        timeout=min(20, max(0.1, deadline - time.monotonic())))
            if re.match(r'HTTP/1\.[01] 204\b', reply):
                break
            last_error = 'unexpected HTTP response'
        except Failure as exc:
            last_error = str(exc)
        time.sleep(min(2, max(0, deadline - time.monotonic())))
    else:
        raise Failure('Android DNS/TCP egress did not return HTTP 204 within 90 seconds: ' + last_error)
    phase('verify software renderer')
    graphics = adb(lease, 'shell', 'dumpsys', 'SurfaceFlinger')
    renderer = next((line.strip() for line in graphics.splitlines() if 'GLES:' in line), '')
    if not re.search(r'SwiftShader|llvmpipe|lavapipe', renderer, re.I):
        raise Failure('software renderer was not reported by SurfaceFlinger')
    phase('DNS/TCP egress and software renderer verified')
def require_busy(directory):
    path = directory / "third.json"
    result = call([EMU, "acquire", "--device", "phone", "--api", "34", "--wait", "0",
                   "--lease-file", str(path)], timeout=30)
    if result.returncode != 3 or not result.stderr.startswith("Android busy:") or path.exists():
        raise Failure("third allocation was not busy")
    started = time.monotonic()
    result = call([EMU, "acquire", "--api", "35", "--wait", "1", "--lease-file", str(path)], timeout=30)
    if result.returncode != 4 or not result.stderr.startswith("Android timeout:") or path.exists():
        raise Failure("busy wait did not time out")
    if time.monotonic() - started > 10:
        raise Failure("busy wait exceeded its bound")
def idle_display(lease):
    # Keep the lease and Android running while its unused software-rendered display sleeps.
    adb(lease, 'shell', 'input', 'keyevent', 'KEYCODE_SLEEP')
    deadline = time.monotonic() + 20
    while time.monotonic() < deadline:
        if re.search(r'\bmWakefulness=Asleep\b', adb(lease, 'shell', 'dumpsys', 'power', timeout=5)):
            phase('idle display asleep; lease remains held')
            return
        time.sleep(1)
    raise Failure('idle display did not sleep')
def require_cancellation():
    process = subprocess.Popen([EMU, 'run', '--api', '35', '--', 'sleep', '60'],
                               stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    try:
        deadline = time.monotonic() + 30
        while True:
            status = call([EMU, 'status'])
            if any(slot['state'] == 'provisioning' for slot in json.loads(status.stdout)['slots']):
                break
            if process.poll() is not None or time.monotonic() >= deadline:
                raise Failure('no provisioning allocation to cancel')
            time.sleep(0.2)
        process.send_signal(signal.SIGTERM)
        process.communicate(timeout=90)
        if process.returncode != 143:
            raise Failure('cancelled wrapper did not preserve SIGTERM status')
        status = json.loads(call([EMU, 'status']).stdout)
        if any(slot['state'] != 'available' for slot in status['slots']):
            raise Failure('cancelled provisioning did not free its slot')
    finally:
        if process.poll() is None:
            process.kill()
        process.communicate()
def require_stale(path):
    result = call([EMU, "renew", "--lease-file", str(path)], timeout=30)
    if result.returncode != 1 or not result.stderr.startswith("Android stale:"):
        raise Failure("released lease renewed")
def slot_status(slot_id, generation):
    result = call([EMU, "status"], timeout=30)
    try:
        if result.returncode != 0:
            raise ValueError
        slot = next(slot for slot in json.loads(result.stdout)["slots"] if slot["slot_id"] == slot_id)
        if slot["generation"] != generation:
            raise ValueError
        return slot["state"]
    except (KeyError, StopIteration, TypeError, ValueError, json.JSONDecodeError) as exc:
        raise Failure("pool status did not identify lease slot") from exc
def require_adb_unusable(lease):
    # Query the old relay directly: an adb CLI could start an unowned server here.
    with socket.socket(socket.AF_UNIX) as stream:
        stream.settimeout(5)
        try:
            stream.connect(lease["endpoint"])
            command = b"host:transport:emulator-5554"
            stream.sendall(f"{len(command):04x}".encode() + command)
            if stream.recv(4):
                raise Failure("inactive lease endpoint still returned ADB data")
        except (FileNotFoundError, ConnectionRefusedError, ConnectionResetError, BrokenPipeError):
            pass
        except TimeoutError as exc:
            raise Failure("inactive lease endpoint did not close") from exc
def require_expiry(directory):
    path, lease = acquire(directory, "expiry", "phone", 35)
    allocation = lease["allocation"]
    try:
        expiry = calendar.timegm(time.strptime(allocation["expires_at"], "%Y-%m-%dT%H:%M:%SZ")) - time.time()
    except (KeyError, TypeError, ValueError, OverflowError) as exc:
        raise Failure("lease expiry was invalid") from exc
    if not 0 < expiry <= 600:
        raise Failure("lease expiry was outside the advertised TTL")
    if slot_status(allocation["slot_id"], allocation["generation"]) != "held":
        raise Failure("lease was not held before expiry")
    earliest = time.monotonic() + expiry - 2
    deadline, progress = earliest + 62, time.monotonic() + 60
    while slot_status(allocation["slot_id"], allocation["generation"]) != "available":
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise Failure("lease did not expire within advertised TTL")
        if time.monotonic() >= progress:
            phase("waiting for lease expiry")
            progress += 60
        time.sleep(min(10, remaining))
    if time.monotonic() < earliest:
        raise Failure("idle lease was released before its advertised expiry")
    require_stale(path)
    require_adb_unusable(lease)
def require_prune_protection():
    result = call([EMU, "cache", "prune", "--dry-run"], timeout=60)
    try:
        value, skipped = json.loads(result.stdout), json.loads(result.stdout)["skipped"]
        if result.returncode != 0 or value["dry_run"] is not True or value["removed"]:
            raise ValueError
        for api in (35, 36):
            if not any(item.startswith(f"system-images;android-{api};google_apis;x86_64@") for item in skipped):
                raise ValueError
    except (KeyError, TypeError, ValueError, json.JSONDecodeError) as exc:
        raise Failure("cache dry-run did not protect active images") from exc
def phase(name):
    print(f"android pool lifecycle: {name}", flush=True)
def main():
    if os.geteuid() == 0:
        raise Failure("must run as the unprivileged yard user")
    leases, step = Leases(), "initialization"
    leases.start()
    try:
        with tempfile.TemporaryDirectory(prefix="subyard-android-retained-") as temporary:
            directory = Path(temporary)
            try:
                phase('cancel provisioning')
                step = 'cancel provisioning'; require_cancellation()
                phase("acquire phone")
                step = "acquire phone"; phone_path, phone = acquire(directory, "phone-old", "phone", 35); leases.add(phone_path)
                step = 'network and renderer'; require_network_and_renderer(phone)
                step = 'idle phone display'; idle_display(phone)
                phase("acquire tablet")
                step = "acquire tablet"; tablet_path, tablet = acquire(directory, "tablet", "tablet", 36); leases.add(tablet_path)
                if (phone['endpoint'] == tablet['endpoint'] or
                        phone['allocation']['slot_id'] == tablet['allocation']['slot_id']):
                    raise Failure('concurrent leases share an endpoint or slot')
                phase("verify concurrent leases")
                step = "initial ADB"; require_sdk_pair(phone, tablet); leases.check()
                step = "tablet network and renderer"; require_network_and_renderer(tablet)
                phase("verify busy and renew")
                step = "busy allocation"; require_busy(directory)
                step = "explicit renew"
                if call([EMU, "renew", "--lease-file", str(phone_path)], timeout=30).returncode != 0:
                    raise Failure("explicit renew failed")
                phase("verify userdata and cache")
                marker = "/data/local/tmp/subyard-retained-marker"
                step = "userdata marker"; adb(phone, "shell", f"printf retained > {marker}")
                if adb(phone, "shell", "cat", marker) != "retained":
                    raise Failure("userdata marker was not written")
                step = "install L2 APK"
                apk = "app/build/outputs/apk/debug/app-debug.apk"
                if "Success" not in adb(phone, "install", "-t", apk).splitlines():
                    raise Failure("L2 APK installation did not succeed")
                if not adb(phone, "shell", "pm", "path", "org.subyard.parallel.a").startswith("package:"):
                    raise Failure("L2 APK was not installed")
                phase("L2 APK install verified")
                step = "cache protection"; require_prune_protection(); leases.check()
                step = 'idle tablet display'; idle_display(tablet)
                phase("release and reacquire phone")
                step = "release phone"; leases.release(phone_path); require_stale(phone_path)
                require_adb_unusable(phone)
                step = "reacquire phone"; new_path, new_phone = acquire(directory, "phone-new", "phone", 35); leases.add(new_path)
                if (new_phone["allocation"]["slot_id"] != phone["allocation"]["slot_id"] or
                        new_phone["allocation"]["generation"] <= phone["allocation"]["generation"]):
                    raise Failure("phone slot was not freshly reallocated")
                require_sdk(new_phone, 35)
                require_adb_unusable(phone)
                step = "fresh L2 APK state"
                if adb(new_phone, "shell", "pm", "list", "packages", "org.subyard.parallel.a"):
                    raise Failure("L2 APK survived release and reacquire")
                if adb(new_phone, "shell", "test", "!", "-e", marker) != "":
                    raise Failure("userdata survived release")
                phase("fresh L2 APK and userdata verified")
                phase("verify stale release fencing")
                step = "stale idempotent release"
                if call([EMU, "release", "--lease-file", str(phone_path)], timeout=120).returncode != 0:
                    raise Failure("stale release failed")
                require_sdk(new_phone, 35); leases.check()
                phase("release leases")
                step = "release successors"; leases.release(new_path); leases.release(tablet_path)
                phase("verify lease expiry")
                step = "lease expiry"; require_expiry(directory)
                print("android pool lifecycle: PASS")
                return 0
            finally:
                leases.close_and_release()
                # Also clean a lease saved just before parsing or assertion failed.
                for path in directory.glob("*.json"):
                    try:
                        call([EMU, "release", "--lease-file", str(path)], timeout=120)
                    except Failure:
                        pass
    except Exception as exc:
        detail = str(exc) if isinstance(exc, Failure) else type(exc).__name__
        print(f"android pool lifecycle: FAIL at {step}: {detail}", file=sys.stderr)
        return 1
if __name__ == "__main__":
    raise SystemExit(main())
