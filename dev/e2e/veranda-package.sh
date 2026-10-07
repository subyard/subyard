#!/usr/bin/env bash
# Install/upgrade/rollback acceptance only on a disposable allocated VM1.
set -Eeuo pipefail
umask 077
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
die() { printf 'veranda-package: %s\n' "$*" >&2; exit 1; }
[ "${SUBYARD_E2E_VM:-}" = 1 ] || die 'run through dev/agent-e2e.sh on allocated VM1'
[ "$#" = 2 ] || die 'expected staged baseline 0.1.0 and candidate 0.1.1 Debian packages'
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
check_input "$BASE" 0.1.0
check_input "$NEXT" 0.1.1
STATE="$(mktemp -d /var/tmp/subyard-veranda-package.XXXXXX)"
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
apt_logged fixture-tools.log install -y xvfb desktop-file-utils
# Separate fixture tools from normal GUI dependencies in incremental disk accounting.
dpkg-query -W -f='${binary:Package}\t${Installed-Size}\t${Status}\n' > "$STATE/packages-before-gui"
package_metrics() {
  python3 - "$STATE/packages-before-gui" "$1" "$STATE/apt-$2.log" "$2" <<'PYMETRICS'
import pathlib, re, subprocess, sys
before, archive, log, phase = sys.argv[1:]
def packages(text):
    result = {}
    for line in text.splitlines():
        name, size, status = line.split('\t', 2)
        if status == 'install ok installed': result[name] = int(size or 0)
    return result
baseline = packages(pathlib.Path(before).read_text())
current = packages(subprocess.check_output(['dpkg-query', '-W', '-f=${binary:Package}\t${Installed-Size}\t${Status}\n'], text=True))
app = next(size for name, size in current.items() if name.split(':')[0] == 'subyard-veranda')
extra = {name: size for name, size in current.items() if name not in baseline and name.split(':')[0] != 'subyard-veranda'}
print(f'PACKAGE_METRICS\t{phase}\tdebBytes={pathlib.Path(archive).stat().st_size}\tappInstalledKiB={app}\tnewOSDependencyInstalledKiB={sum(extra.values())}\tnewOSDependencyPackages={len(extra)}', flush=True)
# Diagnostic apt summary only; download can be zero when the VM cache is warm.
summaries = [line for line in pathlib.Path(log).read_text().splitlines() if re.fullmatch(r'Need to get [0-9.,/ ]+(?:[kMGT]?B)(?:/[0-9., ]+[kMGT]?B)? of archives\.', line)]
print('APT_DOWNLOAD_SUMMARY\t' + phase + '\t' + (' | '.join(summaries) if summaries else 'No Need to get summary reported; cache/download bytes unavailable'), flush=True)
PYMETRICS
}
install -d -m 0700 "$STATE/home" "$STATE/data/com.subyard.veranda" "$STATE/cache" "$STATE/config" \
  "$STATE/runtime" "$STATE/operator" "$STATE/owner-config" "$STATE/owner-data"
printf '%s\n' "$MARKER" > "$STATE/data/com.subyard.veranda/package-preserved.txt"
chmod 0600 "$STATE/data/com.subyard.veranda/package-preserved.txt"
printf '' > "$STATE/owner-config/config.env"
chmod 0600 "$STATE/owner-config/config.env"
launch_installed() {
  python3 - "$ROOT" "$STATE" <<'PY'
import importlib.util, os, pathlib, selectors, subprocess, sys, time
sys.dont_write_bytecode = True
root, state = map(pathlib.Path, sys.argv[1:])
spec = importlib.util.spec_from_file_location('veranda_probe', root / 'dev/measure-veranda.py')
probe = importlib.util.module_from_spec(spec)
spec.loader.exec_module(probe)
read_fd, write_fd = os.pipe()
server = subprocess.Popen(['Xvfb', '-displayfd', str(write_fd), '-screen', '0', '1280x800x24', '-nolisten', 'tcp'],
                          pass_fds=(write_fd,), stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
os.close(write_fd)
try:
    with selectors.DefaultSelector() as selector:
        selector.register(read_fd, selectors.EVENT_READ)
        if not selector.select(5): raise RuntimeError('Xvfb readiness timeout')
    display = ':' + os.read(read_fd, 32).decode().strip()
    if display == ':': raise RuntimeError('Xvfb failed before readiness')
    environment = {'PATH':'/usr/bin:/bin', 'LANG':'C.UTF-8', 'DISPLAY':display,
        'HOME':str(state / 'home'), 'XDG_DATA_HOME':str(state / 'data'),
        'XDG_CACHE_HOME':str(state / 'cache'), 'XDG_CONFIG_HOME':str(state / 'config'),
        'XDG_RUNTIME_DIR':str(state / 'runtime'), 'SUBYARD_OPERATOR_HOME':str(state / 'operator'),
        'SUBYARD_CONFIG_HOME':str(state / 'owner-config'), 'SUBYARD_HOME':str(state / 'owner-data'),
        'SUBYARD_REPOSITORY_ROOT':str(root), 'VERANDA_RESOURCE_PROBE':'1'}
    app, tree, ready = probe.launch(pathlib.Path('/usr/bin/subyard-veranda'), environment, 20)
    try:
        time.sleep(0.5)
        if app.poll() is not None: raise RuntimeError('installed GUI exited after ready')
        tree.sample()
        print(f'Installed GUI ready in {ready:.3f}s (fleet or actionable error)', flush=True)
    finally:
        tree.stop(app)
finally:
    os.close(read_fd)
    server.terminate()
    try: server.wait(timeout=3)
    except subprocess.TimeoutExpired: server.kill(); server.wait(timeout=3)
PY
}
phase() {
  local label="$1" file="$2" version="$3" desktop start=$SECONDS
  installed=1
  apt_logged "apt-$label.log" install -y --allow-downgrades "$file"
  [ "$(dpkg-query -W -f='${Status}' subyard-veranda)" = 'install ok installed' ] || die 'package was not configured'
  [ "$(dpkg-query -W -f='${Version}' subyard-veranda)" = "$version" ] || die 'installed version differs'
  desktop="$(dpkg-query -L subyard-veranda | sed -n '/\.desktop$/p')"
  [ "$desktop" = '/usr/share/applications/Subyard Veranda.desktop' ] || die 'unexpected desktop entry'
  desktop-file-validate "$desktop" > "$STATE/desktop-$label.log" 2>&1
  [ "$(sed -n 's/^Exec=//p' "$desktop")" = subyard-veranda ] || die 'desktop Exec does not name the installed client'
  if [ "$(PATH=/usr/bin:/bin command -v subyard-veranda)" != /usr/bin/subyard-veranda ] || [ ! -x /usr/bin/subyard-veranda ]; then die 'desktop executable is unavailable'; fi
  if dpkg-query -L subyard-veranda | grep -Eq '(^|/)(yard|sy|core|yard-engine|incus)(/|$)'; then die 'installed desktop package owns an owner CLI/runtime'; fi
  [ "$(cat "$STATE/data/com.subyard.veranda/package-preserved.txt")" = "$MARKER" ] || die 'app data marker was lost'
  cli_snapshot > "$STATE/cli-after"
  cmp "$STATE/cli-before" "$STATE/cli-after" || die 'installed CLI files changed'
  package_metrics "$file" "$label"
  launch_installed > "$STATE/gui-$label.log" 2>&1
  cat "$STATE/gui-$label.log"
  [ "$(cat "$STATE/data/com.subyard.veranda/package-preserved.txt")" = "$MARKER" ] || die 'GUI launch lost app data marker'
  printf 'PASS\t%s\t%s\t%s seconds\n' "$label" "$version" "$((SECONDS - start))"
}
phase install "$BASE" 0.1.0
phase upgrade "$NEXT" 0.1.1
phase rollback "$BASE" 0.1.0
printf 'PASS\tVeranda Debian install, upgrade, rollback and isolated GUI launch\n'
