#!/usr/bin/env bash
# Shared helpers for lease holders in disposable E2E fixtures.
# shellcheck shell=bash
# HOLDER_STARTED_PID is read by the caller.
# shellcheck disable=SC2034

# A runtime mask loses to a local /etc unit. Refuse the reaper's explicit start
# instead; its timer is stopped separately while the incident is captured.
reaper_pause() {
  case "$1" in outer_root|outer_exec) ;; *) return 1 ;; esac
  "$1" python3 - "$2" "$3" <<'PYREAPERPAUSE'
import os,pathlib,re,stat,subprocess,sys
action,marker=sys.argv[1:]
if action not in ('install','verify','restore') or not re.fullmatch(r'subyard-p0-[0-9]+',marker):
    raise RuntimeError('invalid reaper pause request')
unit='subyard-test-vms-lease-reaper.service'
parent=pathlib.Path('/run/systemd/system')
directory=parent/(unit+'.d')
path=directory/'subyard-e2e-pause.conf'
body=('# '+marker+'\n[Unit]\nRefuseManualStart=yes\n').encode()
def owned(path,mode,kind):
    info=path.lstat()
    if path.resolve()!=path or not kind(info.st_mode) or info.st_uid!=os.geteuid() or info.st_gid!=os.getegid() or stat.S_IMODE(info.st_mode)!=mode:
        raise RuntimeError('unsafe reaper pause path')
    return info
def systemctl(*args):
    return subprocess.check_output(['systemctl',*args],text=True,timeout=30 if args[0]=='stop' else 10).strip()
def property(name):
    return systemctl('show',unit,'--property='+name,'--value')
for ancestor in (parent,parent.parent,parent.parent.parent):
    info=ancestor.lstat()
    if ancestor.resolve()!=ancestor or not stat.S_ISDIR(info.st_mode) or info.st_uid!=os.geteuid() or info.st_mode & 0o022:
        raise RuntimeError('unsafe reaper pause parent')
if action=='install':
    if directory.exists() or directory.is_symlink() or property('RefuseManualStart')!='no':
        raise RuntimeError('foreign reaper pause')
    directory.mkdir(mode=0o755)
    directory.chmod(0o755)
    fd=os.open(path,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
    with os.fdopen(fd,'wb') as stream:
        os.fchmod(stream.fileno(),0o600)
        stream.write(body)
        stream.flush()
        os.fsync(stream.fileno())
    systemctl('daemon-reload')
    systemctl('stop',unit)
owned(directory,0o755,stat.S_ISDIR)
info=owned(path,0o600,stat.S_ISREG)
if info.st_nlink!=1 or info.st_size!=len(body) or path.read_bytes()!=body or list(directory.iterdir())!=[path]:
    raise RuntimeError('foreign reaper pause content')
if action=='restore':
    path.unlink()
    directory.rmdir()
    systemctl('daemon-reload')
    expected='no'
else:
    expected='yes'
if property('RefuseManualStart')!=expected or property('ActiveState')!='inactive':
    raise RuntimeError('reaper pause was not effective')
PYREAPERPAUSE
}

start_holder_child() {
  set -m
  (set +m; "$@") &
  HOLDER_STARTED_PID=$!
  set +m
}

stop_holder_child() {
  local root="$1" now stop_deadline kill_deadline
  if ! kill -0 -- "-$root" >/dev/null 2>&1; then
    wait "$root" >/dev/null 2>&1 || true
    return 0
  fi
  kill -TERM -- "-$root" >/dev/null 2>&1 || true
  now="$(recovery_monotonic_seconds)"
  stop_deadline=$((now + HOLDER_STOP_GRACE_SECONDS))
  while kill -0 -- "-$root" >/dev/null 2>&1; do
    now="$(recovery_monotonic_seconds)"
    [ "$now" -lt "$stop_deadline" ] || break
    sleep 1
  done
  if kill -0 -- "-$root" >/dev/null 2>&1; then
    kill -KILL -- "-$root" >/dev/null 2>&1 || true
  fi
  now="$(recovery_monotonic_seconds)"
  kill_deadline=$((now + HOLDER_KILL_GRACE_SECONDS))
  while kill -0 -- "-$root" >/dev/null 2>&1; do
    now="$(recovery_monotonic_seconds)"
    [ "$now" -lt "$kill_deadline" ] || break
    sleep 1
  done
  if kill -0 -- "-$root" >/dev/null 2>&1; then
    printf 'lease-holder: holder process group %s survived bounded shutdown\n' \
      "$root" >&2
    return 1
  fi
  wait "$root" >/dev/null 2>&1 || true
}

hold_lease() (
  local client="$1" purpose="$2" requested_slot="$3" ready_temp
  export SUBYARD_E2E_STATE_DIR="$STATE_PARENT/$client"
  export SUBYARD_E2E_YARD="$YARD"
  # shellcheck source=dev/agent-e2e.sh
  . "$RUNNER"

  # Variables consumed by the sourced runner functions.
  # shellcheck disable=SC2034
  LOCAL_TEMP="$(mktemp -d "$STATE_PARENT/$client-runtime.XXXXXX")"
  LEASE_PURPOSE="$purpose"
  # shellcheck disable=SC2034
  LEASE_REQUESTED_SLOT="$requested_slot"
  holder_cleanup() {
    local rc=$?
    trap - EXIT INT TERM
    set +e
    if [ -n "$LEASE_KEEPER_PID" ]; then
      kill "$LEASE_KEEPER_PID" >/dev/null 2>&1 || true
      wait "$LEASE_KEEPER_PID" >/dev/null 2>&1 || true
      LEASE_KEEPER_PID=''
    fi
    release_lease || rc=1
    exit "$rc"
  }
  trap holder_cleanup EXIT INT TERM

  acquire_lease || { : > "$STATE_PARENT/$client.failed"; exit 1; }
  start_lease_keeper
  ready_temp="$(mktemp "$STATE_PARENT/.$client.ready.XXXXXX")"
  printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
    "$LEASE_SLOT" "$CLIENT_CONFIG" "$LEASE_PROJECT" "$LEASE_RUN" "$LEASE_PURPOSE" \
    "$LEASE_GENERATION" "$BASE_FINGERPRINT" \
    > "$ready_temp"
  mv -f "$ready_temp" "$STATE_PARENT/$client.ready"
  while [ ! -e "$STATE_PARENT/$client.release" ]; do sleep 1; done
)

recovery_monotonic_seconds() {
  local uptime _
  IFS=' ' read -r uptime _ < /proc/uptime \
    || die 'cannot read the kernel monotonic clock'
  uptime="${uptime%%.*}"
  [[ "$uptime" =~ ^[0-9]+$ ]] \
    || die 'kernel monotonic clock returned an invalid value'
  printf '%s\n' "$uptime"
}
