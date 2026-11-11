# shellcheck shell=bash
# Source-only provision boundaries; callers own installation and shell options.
# Downloads resume within a bounded retry count; toolchain.py verifies the pinned
# checksum before consuming an archive, including a resumed partial file.
android_download() {
  local status=0 stage="$3" started=$SECONDS output_bytes=unknown attempt
  printf 'android provision: download stage=%s state=start\n' "$stage" >&2
  for attempt in 1 2 3 4; do
    # SDK/JDK archives need time on slow links; below 1 KiB/s for 120s is a stall.
    if curl --fail --silent --location --connect-timeout 15 \
      --speed-limit 1024 --speed-time 120 --continue-at - --output "$2" "$1"; then
      status=0
      break
    else
      status=$?
    fi
    [ "$attempt" = 4 ] || sleep 1
  done
  if [ -f "$2" ] && [ ! -L "$2" ]; then
    output_bytes="$(stat -c %s -- "$2" 2>/dev/null)" || output_bytes=unknown
    case "$output_bytes" in ''|*[!0-9]*) output_bytes=unknown ;; esac
  fi
  if [ "$status" -eq 0 ]; then
    printf 'android provision: download stage=%s state=complete elapsed_seconds=%s archive_bytes=%s\n' \
      "$stage" "$((SECONDS - started))" "$output_bytes" >&2
    return 0
  fi
  printf 'android provision: download stage=%s state=failed exit=%s elapsed_seconds=%s last_output_bytes=%s\n' \
    "$stage" "$status" "$((SECONDS - started))" "$output_bytes" >&2
  rm -f -- "$2"
  return "$status"
}


android_refresh_legacy_helpers() {
  local legacy_root="$1" profile_root="$2"
  if [ -d "$legacy_root" ] && [ ! -L "$legacy_root" ]; then
    install -m 0755 "$profile_root/emulator-run.sh" "$legacy_root"/emulator-run.sh
    install -m 0755 "$profile_root/emulator-control.sh" "$legacy_root"/emulator-control.sh
  fi
}

# local.properties is project-owned input. Reconcile only aliases in ordinary
# yard storage, never an entry in a retained workspace or an external mount.
android_reconcile_sdk_alias() {
  python3 -B - "$@" <<'PYSDKALIAS'
import os, pathlib, re, sys
mode, sdk, alias = sys.argv[1:4]
root = pathlib.Path(sys.argv[4]) if len(sys.argv) > 4 and sys.argv[4] else pathlib.Path('/')
mountinfo = pathlib.Path(sys.argv[5] if len(sys.argv) > 5 else '/proc/self/mountinfo')
def inside(path, parent):
    return path == parent or parent in path.parents
def unescape(value):
    return re.sub(r'\\([0-7]{3})', lambda match: chr(int(match[1], 8)), value)
try:
    entry = pathlib.PurePosixPath(alias)
    if not alias.startswith('/') or '..' in entry.parts or alias in ('/', sdk):
        raise ValueError('not an eligible absolute alias')
    destination = root / alias.lstrip('/')
    mounts = []
    inventory = mountinfo.read_text()
    for line in inventory.splitlines():
        fields = line.split()
        if len(fields) < 7 or '-' not in fields:
            raise ValueError('invalid mount inventory')
        mount = unescape(fields[4])
        if mount not in ('/', '/srv'):
            mounts.append(root / mount.lstrip('/'))
    if not inventory.strip():
        raise ValueError('missing mount inventory')
    def safe_parent():
        resolved = destination.parent.resolve() / destination.name
        if not inside(resolved, root.resolve()):
            return False
        protected = [root / 'srv/workspaces'] + mounts
        return not any(inside(path, parent) for path in (destination, resolved) for parent in protected)
    if not safe_parent():
        raise ValueError('project or mounted destination')
    try:
        current = destination.lstat()
    except FileNotFoundError:
        current = None
    if current is not None:
        # local.properties cannot establish ownership of an existing entry.
        print('android provision: existing sdk.dir alias left unchanged', file=sys.stderr)
        sys.exit(0)
    if mode == 'check':
        sys.exit(10)
    if mode != 'apply':
        raise ValueError('invalid alias operation')
    destination.parent.mkdir(parents=True, exist_ok=True)
    if not safe_parent():
        raise ValueError('alias parent changed')
    destination.symlink_to(root / sdk.lstrip('/'))
except (OSError, ValueError, RuntimeError):
    print('android provision: sdk.dir alias skipped outside ordinary yard storage', file=sys.stderr)
PYSDKALIAS
}
