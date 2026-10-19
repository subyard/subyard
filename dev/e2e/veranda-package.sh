#!/usr/bin/env bash
# Install/upgrade/rollback acceptance only on a disposable allocated VM1.
set -Eeuo pipefail
umask 077
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
die() { printf 'veranda-package: %s\n' "$*" >&2; exit 1; }
[ "${SUBYARD_E2E_VM:-}" = 1 ] || die 'run through dev/agent-e2e.sh on allocated VM1'
BACKEND=x11
if [ "${1:-}" = --wayland ]; then BACKEND=wayland; shift; fi
[ "$#" = 2 ] || die 'expected [--wayland] staged baseline and candidate Debian packages'
for command in sudo apt-get dpkg dpkg-query dpkg-deb python3 sha256sum realpath tar; do
  command -v "$command" >/dev/null || die "$command is required"
done
sudo -n test -f /run/subyard-e2e-lease.json || die 'an allocated VM lease marker is required'
if dpkg-query -W subyard-veranda >/dev/null 2>&1; then die 'Veranda must be absent on this fresh fixture VM'; fi
BASE="$(realpath -- "$1")" NEXT="$(realpath -- "$2")"
architecture="$(dpkg --print-architecture)"
check_input() {
  local file="$1" version="$2" files
  [ -f "$file" ] || die 'staged package is missing'
  [ "$(dpkg-deb -f "$file" Package)" = subyard-veranda ] || die 'unexpected package name'
  [ "$(dpkg-deb -f "$file" Version)" = "$version" ] || die 'unexpected package version'
  [ "$(dpkg-deb -f "$file" Architecture)" = "$architecture" ] || die 'package architecture differs from VM'
  files="$(dpkg-deb --fsys-tarfile "$file" | tar -tf -)"
  if printf '%s\n' "$files" | grep -Eq '(^|/)(yard|sy|core|yard-engine|incus)(/|$)'; then die 'desktop package bundles a CLI or owner runtime'; fi
}
BASE_VERSION="$(dpkg-deb -f "$BASE" Version)"
NEXT_VERSION="$(dpkg-deb -f "$NEXT" Version)"
for version in "$BASE_VERSION" "$NEXT_VERSION"; do
  [[ "$version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] && [ "${#version}" -le 64 ] || die 'package version must be stable product MAJOR.MINOR.PATCH'
done
dpkg --compare-versions "$BASE_VERSION" lt "$NEXT_VERSION" || die 'candidate version must be greater than baseline'
check_input "$BASE" "$BASE_VERSION"
check_input "$NEXT" "$NEXT_VERSION"
STATE="$(mktemp -d /tmp/subyard-veranda-package.XXXXXX)"
chmod 0700 "$STATE"
MARKER="subyard-veranda-package-v1-${STATE##*.}"
printf '%s\n' "$MARKER" > "$STATE/.marker"
chmod 0600 "$STATE/.marker"
installed=0
apt_logged() {
  local log="$1"; shift
  # Logs deliberately belong to the unprivileged fixture owner, not root.
  # shellcheck disable=SC2024
  sudo -n env LC_ALL=C apt-get "$@" > "$STATE/$log" 2>&1
}
cleanup() {
  local rc=$?
  trap - EXIT INT TERM ERR
  set +e
  if [ -d "$STATE" ] && [ ! -L "$STATE" ] && [ "$(stat -c '%a:%u' "$STATE")" = "700:$(id -u)" ] \
    && [ -f "$STATE/.marker" ] && [ ! -L "$STATE/.marker" ] && [ "$(cat "$STATE/.marker")" = "$MARKER" ]; then
    if [ "$installed" = 1 ]; then apt_logged remove.log -y remove subyard-veranda || rc=3; fi
    if [ "$rc" = 0 ]; then find "$STATE" -depth -delete || rc=3; fi
  else rc=3; fi
  [ "$rc" = 0 ] || printf 'veranda-package: failed fixture retained at %s\n' "$STATE" >&2
  exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
cli_snapshot() {
  local name file
  for name in yard sy core yard-engine; do
    for file in "/usr/bin/$name" "/usr/local/bin/$name"; do
      if [ -e "$file" ]; then sha256sum -- "$file"; else printf 'absent %s\n' "$file"; fi
    done
  done
}
cli_snapshot > "$STATE/cli-before"
apt_logged apt-update.log update -qq
# All normal package cost stages finish before installing any fixture tools.
# This includes GTK libraries shared with Openbox/GI/AT-SPI in ordinary GUI cost.
dpkg-query -W -f='${binary:Package}\t${Installed-Size}\t${Status}\n' > "$STATE/packages-before-gui"
package_metrics() {
  dpkg-query -W -f='${binary:Package}\t${Installed-Size}\t${Status}\n' > "$STATE/packages-after-$2"
  python3 - "$STATE/packages-before-gui" "$STATE/packages-before-$2" "$STATE/packages-after-$2" "$1" "$STATE/apt-$2-package.log" "$2" <<'PYMETRICS'
import pathlib, re, sys
before, phase_before, after, archive, log, phase = sys.argv[1:]
def packages(text):
    result = {}
    for line in text.splitlines():
        name, size, status = line.split('\t', 2)
        if status == 'install ok installed': result[name] = int(size or 0)
    return result
baseline = packages(pathlib.Path(before).read_text())
previous = packages(pathlib.Path(phase_before).read_text())
current = packages(pathlib.Path(after).read_text())
app = next(size for name, size in current.items() if name.split(':')[0] == 'subyard-veranda')
extra = {name: size for name, size in current.items() if name not in baseline and name.split(':')[0] != 'subyard-veranda'}
phase_new = {name: size for name, size in current.items() if name not in previous and name.split(':')[0] != 'subyard-veranda'}
os_delta = sum(size for name,size in current.items() if name.split(':')[0] != 'subyard-veranda') - sum(size for name,size in previous.items() if name.split(':')[0] != 'subyard-veranda')
print(f'PACKAGE_METRICS\t{phase}\tcontext=normal-lifecycle-before-fixture-tools\tdebBytes={pathlib.Path(archive).stat().st_size}\tappInstalledKiB={app}\tnewOSDependencyInstalledKiB={sum(extra.values())}\tnewOSDependencyPackages={len(extra)}\tphaseNewOSDependencyInstalledKiB={sum(phase_new.values())}\tphaseOSInstalledSizeDeltaKiB={os_delta}', flush=True)
# Diagnostic apt summary only; download can be zero when the VM cache is warm.
summaries = [line for line in pathlib.Path(log).read_text().splitlines() if re.fullmatch(r'Need to get [0-9.,/ ]+(?:[kMGT]?B)(?:/[0-9., ]+[kMGT]?B)? of archives\.', line)]
print('APT_DOWNLOAD_SUMMARY\t' + phase + '\t' + (' | '.join(summaries) if summaries else 'No Need to get summary reported; cache/download bytes unavailable'), flush=True)
PYMETRICS
}
install -d -m 0700 "$STATE/home" "$STATE/data/subyard/veranda" "$STATE/cache" "$STATE/config" \
  "$STATE/runtime" "$STATE/operator" "$STATE/owner-config" "$STATE/owner-data"
printf '%s\n' "$MARKER" > "$STATE/data/subyard/veranda/package-preserved.txt"
chmod 0600 "$STATE/data/subyard/veranda/package-preserved.txt"
printf '' > "$STATE/owner-config/config.env"
chmod 0600 "$STATE/owner-config/config.env"
launch_installed() {
  python3 - "$ROOT" "$STATE" "$1" "$BACKEND" <<'PY'
import contextlib, importlib.util, os, pathlib, selectors, stat, subprocess, sys, threading, time
sys.dont_write_bytecode = True
root, state = map(pathlib.Path, sys.argv[1:3])
phase, backend = sys.argv[3:5]
assert backend in ('x11', 'wayland')
assert phase in ("install", "upgrade", "rollback")
spec = importlib.util.spec_from_file_location('veranda_probe', root / 'dev/measure-veranda.py')
probe = importlib.util.module_from_spec(spec)
spec.loader.exec_module(probe)
def launch_logged(command, name, **options):
    # Drain independently of GUI/driver waits; retain only a bounded private prefix.
    fd = os.open(state / ('gtk-' + phase + '-' + name + '-stderr.log'),
                 os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    os.fchmod(fd, 0o600)
    read_fd, write_fd = os.pipe()
    def drain():
        with os.fdopen(read_fd, 'rb') as source, os.fdopen(fd, 'wb', buffering=0) as log:
            remaining = 2 * 1024 * 1024
            while data := source.read1(65536):
                try:
                    log.write(data[:remaining])
                    remaining = max(0, remaining - len(data))
                except OSError:
                    remaining = 0  # Keep draining; diagnostics cannot block the child.
    worker = threading.Thread(target=drain, daemon=True)
    try:
        try:
            worker.start()
        except BaseException:
            os.close(read_fd)
            os.close(fd)
            raise
        process = subprocess.Popen(command, stderr=write_fd, **options)
    finally:
        os.close(write_fd)
    return process, worker

cleanup_errors = []
def stop_owned(process, tree):
    try:
        tree.stop(process)
    except BaseException as error:
        cleanup_errors.append(type(error).__name__)

read_fd = None
server = None
try:
    display = None
    if backend == 'x11':
        read_fd, write_fd = os.pipe()
        try:
            server = subprocess.Popen(['Xvfb', '-displayfd', str(write_fd), '-screen', '0', '1280x800x24', '-nolisten', 'tcp'],
                                      pass_fds=(write_fd,), stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        finally:
            os.close(write_fd)
        display = ':' + probe.read_display_number(read_fd, time.monotonic() + 5)
    display_state = state / ('display-' + phase)
    probe.private_dir(display_state)
    probe.private_dir(display_state / 'runtime')
    environment = {'PATH':'/usr/bin:/bin', 'LANG':'C.UTF-8',
        'HOME':str(state / 'home'), 'XDG_DATA_HOME':str(state / 'data'),
        'XDG_CACHE_HOME':str(state / 'cache'), 'XDG_CONFIG_HOME':str(state / 'config'),
        'XDG_RUNTIME_DIR':str(display_state / 'runtime'), 'SUBYARD_OPERATOR_HOME':str(state / 'operator'),
        'SUBYARD_CONFIG_HOME':str(state / 'owner-config'), 'SUBYARD_HOME':str(state / 'owner-data'),
        'SUBYARD_REPOSITORY_ROOT':str(root)}
    if display is not None: environment['DISPLAY'] = display
    environment.update(GTK_A11Y='atspi', GDK_BACKEND=backend, GSK_RENDERER='cairo',
                       SUBYARD_E2E_VM='1', DBUS_SESSION_BUS_ADDRESS='unix:path=' + str(display_state / 'session-bus'))
    services=[]
    app=None; tree=None; stderr_workers=[]
    try:
        for command in [['dbus-daemon','--session','--nofork','--address=' + environment['DBUS_SESSION_BUS_ADDRESS']], *([['openbox']] if backend == 'x11' else [])]:
            service=subprocess.Popen(command, env=environment, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)
            services.append((service,probe.Tree(service.pid)))
        deadline=time.monotonic()+5
        while not (display_state / 'session-bus').is_socket():
            if time.monotonic()>=deadline: raise RuntimeError('private session bus readiness timeout')
            time.sleep(.05)
        if backend == 'wayland':
            probe.private_dir(display_state / 'compositor')
            probe.private_file(display_state / 'compositor/rc.xml', '<labwc_config><core><xwayland>no</xwayland></core></labwc_config>\n')
            environment.update(WLR_BACKENDS='headless', WLR_HEADLESS_OUTPUTS='1', WLR_RENDERER='pixman',
                WLR_LIBINPUT_NO_DEVICES='1', LABWC_UPDATE_ACTIVATION_ENV='0',
                XDG_CONFIG_DIRS=str(display_state / 'compositor'), XDG_SESSION_TYPE='wayland')
            compositor, compositor_stderr = launch_logged(['labwc', '-V', '-C', str(display_state / 'compositor')],
                'compositor', env=environment, stdout=subprocess.DEVNULL, start_new_session=True)
            services.append((compositor, probe.Tree(compositor.pid)))
            stderr_workers.append(compositor_stderr)
            deadline = time.monotonic() + 15
            while True:
                if compositor.poll() is not None: raise RuntimeError('owned labwc exited before readiness')
                if time.monotonic() >= deadline: raise RuntimeError('owned labwc readiness timed out')
                sockets = [path for path in (display_state / 'runtime').glob('wayland-*') if stat.S_ISSOCK(path.lstat().st_mode)]
                if len(sockets) == 1 and 'pixman' in (state / ('gtk-' + phase + '-compositor-stderr.log')).read_text().lower():
                    environment['WAYLAND_DISPLAY'] = str(sockets[0])
                    break
                time.sleep(.05)
        if backend == 'wayland':
            window = probe.WlrToplevel(environment['WAYLAND_DISPLAY'], 'org.subyard.Veranda')
            window.require_empty()
        app, app_stderr=launch_logged(['/usr/bin/subyard-veranda'],'app',env=environment,stdout=subprocess.DEVNULL,start_new_session=True)
        stderr_workers.append(app_stderr)
        tree=probe.Tree(app.pid)
        driver, driver_stderr=launch_logged(['/usr/bin/python3',str(root / 'dev/e2e/veranda-package-ui.py'),str(app.pid), *(['--wayland'] if backend == 'wayland' else [])],'driver',env=environment,stdout=subprocess.PIPE,start_new_session=True)
        stderr_workers.append(driver_stderr)
        driver_tree=probe.Tree(driver.pid)
        try:
            deadline=time.monotonic()+30
            while driver.poll() is None:
                tree.sample()
                if time.monotonic()>=deadline: raise RuntimeError('GTK package driver deadline exceeded')
                time.sleep(.05)
            output=driver.communicate(timeout=2)[0]
            if driver.returncode: raise RuntimeError('GTK package readiness/close driver failed')
            receipt=__import__('json').loads(output)
            if not receipt.get('natural_close_requested'): raise RuntimeError('missing GTK natural close request')
            if app.wait(timeout=10)!=0: raise RuntimeError('GTK natural close failed')
            deadline=time.monotonic()+3
            while True:
                tree.sample()
                current=probe.processes()
                remaining=[pid for pid,start in tree.known.items() if pid!=app.pid and pid in current and current[pid]['start']==start]
                if not remaining: break
                if time.monotonic()>=deadline: raise RuntimeError('observed owned GTK children survived natural close')
                time.sleep(.05)
            if backend == 'wayland':
                window.require_empty()
                receipt['initially_empty_owned_compositor'] = True
            receipt['natural_close_exit_code']=0
            receipt['observed_owned_children_cleanup']=True
            print('PACKAGE_GTK ' + __import__('json').dumps(receipt),flush=True)
        finally:
            stop_owned(driver, driver_tree)
    finally:
        original_failure = sys.exc_info()[0] is not None
        if tree is not None: stop_owned(app, tree)
        for service,owned in reversed(services): stop_owned(service, owned)
        try:
            if (display_state / 'session-bus').exists(): (display_state / 'session-bus').unlink()
        except OSError as error:
            cleanup_errors.append(type(error).__name__)
        for worker in stderr_workers:
            with contextlib.suppress(BaseException): worker.join(timeout=2)
        if cleanup_errors:
            with contextlib.suppress(BaseException):
                print('PACKAGE_GTK_CLEANUP ' + __import__('json').dumps({'failure_types': cleanup_errors}), file=sys.stderr, flush=True)
            if not original_failure: raise RuntimeError('owned GTK fixture cleanup failed')
finally:
    if read_fd is not None: os.close(read_fd)
    if server is not None:
        server.terminate()
        try: server.wait(timeout=3)
        except subprocess.TimeoutExpired: server.kill(); server.wait(timeout=3)
PY
}
phase() {
  local label="$1" file="$2" version="$3" mode="$4" desktop start=$SECONDS
  if [ "$mode" = package ]; then
    dpkg-query -W -f='${binary:Package}\t${Installed-Size}\t${Status}\n' > "$STATE/packages-before-$label"
  fi
  installed=1
  apt_logged "apt-$label-$mode.log" install -y --allow-downgrades "$file"
  [ "$(dpkg-query -W -f='${Status}' subyard-veranda)" = 'install ok installed' ] || die 'package was not configured'
  [ "$(dpkg-query -W -f='${Version}' subyard-veranda)" = "$version" ] || die 'installed version differs'
  desktop="$(dpkg-query -L subyard-veranda | sed -n '/\.desktop$/p')"
  [ "$desktop" = '/usr/share/applications/org.subyard.Veranda.desktop' ] || die 'unexpected desktop entry'
  if [ "$mode" = gtk ]; then desktop-file-validate "$desktop" > "$STATE/desktop-$label.log" 2>&1; fi
  [ "$(sed -n 's/^Exec=//p' "$desktop")" = subyard-veranda ] || die 'desktop Exec does not name the installed client'
  if [ "$(PATH=/usr/bin:/bin command -v subyard-veranda)" != /usr/bin/subyard-veranda ] || [ ! -x /usr/bin/subyard-veranda ]; then die 'desktop executable is unavailable'; fi
  if dpkg-query -L subyard-veranda | grep -Eq '(^|/)(yard|sy|core|yard-engine|incus)(/|$)'; then die 'installed desktop package owns an owner CLI/runtime'; fi
  [ "$(cat "$STATE/data/subyard/veranda/package-preserved.txt")" = "$MARKER" ] || die 'app data marker was lost'
  cli_snapshot > "$STATE/cli-after"
  cmp "$STATE/cli-before" "$STATE/cli-after" || die 'installed CLI files changed'
  [ "$(/usr/bin/subyard-veranda --version)" = "Subyard Veranda $version" ] || die 'installed GUI/client product version differs'
  if [ "$mode" = package ]; then
    package_metrics "$file" "$label"
  else
    launch_installed "$label" > "$STATE/gui-$label.log" 2>&1
    cat "$STATE/gui-$label.log"
  fi
  [ "$(cat "$STATE/data/subyard/veranda/package-preserved.txt")" = "$MARKER" ] || die 'GUI launch lost app data marker'
  printf 'PASS\t%s\t%s\t%s\t%s seconds\n' "$label" "$mode" "$version" "$((SECONDS - start))"
}
phase install "$BASE" "$BASE_VERSION" package
phase upgrade "$NEXT" "$NEXT_VERSION" package
phase rollback "$BASE" "$BASE_VERSION" package
# GTK drivers are introduced only after the normal apt lifecycle and costs are
# recorded. The second sequence proves UI launch/close with each installed GUI;
# its warm-cache apt logs must not be substituted for clean-machine cost logs.
display_tools=(xvfb openbox xdotool)
if [ "$BACKEND" = wayland ]; then display_tools=(labwc wlrctl wtype); fi
apt_logged fixture-tools.log install -y "${display_tools[@]}" dbus-x11 at-spi2-core python3-gi gir1.2-atspi-2.0 desktop-file-utils
phase install "$BASE" "$BASE_VERSION" gtk
phase upgrade "$NEXT" "$NEXT_VERSION" gtk
phase rollback "$BASE" "$BASE_VERSION" gtk
printf 'PASS\tVeranda Debian install, upgrade, rollback and isolated %s GUI launch\n' "$BACKEND"
