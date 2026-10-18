#!/usr/bin/env bash
# A release refresh preserves an inactive timer and never starts sync or a manager.
set -euo pipefail
export TMPDIR=/tmp
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d /tmp/subyard-runtime-refresh.XXXXXX)"
trap 'rm -rf -- "$TMP"' EXIT
# shellcheck source=tests/helpers/test-context.sh
. "$ROOT/tests/helpers/test-context.sh"
setup_test_context "$TMP/context"
export SUBYARD_KEYS_SYSTEMD_DIR="$TMP/units" SYSTEMCTL_LOG="$TMP/systemctl.log" SYSTEMCTL_NEED_RELOAD="$TMP/need-reload"
mkdir -p "$TMP/bin" "$SUBYARD_KEYS_SYSTEMD_DIR"
cat > "$TMP/bin/systemctl" <<'SH'
#!/bin/sh
printf '%s\n' "$*" >> "$SYSTEMCTL_LOG"
case "$*" in
  '--user show-environment') [ "${SYSTEMCTL_MANAGER_STOPPED:-0}" = 0 ] ;;
  '--user daemon-reload') rm -f -- "$SYSTEMCTL_NEED_RELOAD" ;;
  '--user show subyard-keys-sync.service --property=NeedDaemonReload --value'|'--user show subyard-keys-sync.timer --property=NeedDaemonReload --value') if [ -e "$SYSTEMCTL_NEED_RELOAD" ]; then printf 'yes\n'; else printf 'no\n'; fi ;;
  *) exit 99 ;;
esac
SH
chmod 0755 "$TMP/bin/systemctl"
export PATH="$TMP/bin:$PATH"
export YARD_RUNTIME_ROOT="$TMP/runtime"
mkdir -p "$YARD_RUNTIME_ROOT/current/bin"
printf '#!/bin/sh\nexit 0\n' > "$YARD_RUNTIME_ROOT/current/bin/yard"
chmod 0755 "$YARD_RUNTIME_ROOT/current/bin/yard"
# A pre-receipt installation has the exact public recipe. One retained peer
# establishes ownership without allowing an unrelated same-name file.
sed "s|@YARD_BIN@|$YARD_RUNTIME_ROOT/current/bin/yard|g" "$ROOT/tests/fixtures/systemd/subyard-keys-sync-v0.21.2.service.in" \
  > "$SUBYARD_KEYS_SYSTEMD_DIR/subyard-keys-sync.service"
chmod 0644 "$SUBYARD_KEYS_SYSTEMD_DIR/subyard-keys-sync.service"
bash "$ROOT/scripts/install-keys-auto-sync.sh" --refresh --yes
if rg -q 'enable|start|restart|sync --' "$SYSTEMCTL_LOG"; then exit 1; fi
rg -Fxq -- '--user daemon-reload' "$SYSTEMCTL_LOG"
bash "$ROOT/scripts/install-keys-auto-sync.sh" --check-refresh
# Host identity must survive the user unit so verified root/operator ownership
# stays meaningful; the worker still cannot acquire privileges or create set-ID files.
grep -Fxq 'NoNewPrivileges=true' "$SUBYARD_KEYS_SYSTEMD_DIR/subyard-keys-sync.service"
grep -Fxq 'RestrictSUIDSGID=true' "$SUBYARD_KEYS_SYSTEMD_DIR/subyard-keys-sync.service"
if rg -q '^(PrivateTmp|PrivateUsers|ProtectControlGroups|ProtectKernelModules|ProtectKernelTunables)=' \
  "$SUBYARD_KEYS_SYSTEMD_DIR/subyard-keys-sync.service"; then exit 1; fi
for receipt in absent present; do
  for stopped in 0 1; do
    units="$TMP/previous-$receipt-$stopped"
    mkdir -m 0700 "$units"
    if [ "$receipt" = present ]; then
      SUBYARD_KEYS_SYSTEMD_DIR="$units" SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1 \
        bash "$ROOT/scripts/install-keys-auto-sync.sh" --yes >/dev/null
    fi
    sed "s|@YARD_BIN@|$YARD_RUNTIME_ROOT/current/bin/yard|g" \
      "$ROOT/tests/fixtures/systemd/subyard-keys-sync-v0.21.2.service.in" > "$units/subyard-keys-sync.service"
    chmod 0644 "$units/subyard-keys-sync.service"
    install -m 0644 "$ROOT/config/systemd/subyard-keys-sync.timer.in" "$units/subyard-keys-sync.timer"
    : > "$SYSTEMCTL_LOG"
    SUBYARD_KEYS_SYSTEMD_DIR="$units" bash "$ROOT/scripts/install-keys-auto-sync.sh" --check-owned
    if SUBYARD_KEYS_SYSTEMD_DIR="$units" bash "$ROOT/scripts/install-keys-auto-sync.sh" --check-refresh; then exit 1; fi
    SUBYARD_KEYS_SYSTEMD_DIR="$units" SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=0 SYSTEMCTL_MANAGER_STOPPED="$stopped" \
      bash "$ROOT/scripts/install-keys-auto-sync.sh" --refresh --yes
    cmp -s <(sed "s|@YARD_BIN@|$YARD_RUNTIME_ROOT/current/bin/yard|g" "$ROOT/config/systemd/subyard-keys-sync.service.in") \
      "$units/subyard-keys-sync.service"
    [ "$(stat -c %a "$units/.subyard-keys-sync.json")" = 600 ]
    SUBYARD_KEYS_SYSTEMD_DIR="$units" SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=0 SYSTEMCTL_MANAGER_STOPPED="$stopped" \
      bash "$ROOT/scripts/install-keys-auto-sync.sh" --check-refresh
    if rg -q 'enable|start|restart|sync --' "$SYSTEMCTL_LOG"; then exit 1; fi
    if [ "$stopped" = 0 ]; then
      rg -Fxq -- '--user daemon-reload' "$SYSTEMCTL_LOG"
    elif rg -Fq -- '--user daemon-reload' "$SYSTEMCTL_LOG"; then exit 1; fi
  done
done
: > "$SYSTEMCTL_LOG"
# Restore the original live-manager case for idempotence and reload recovery.
touch "$SYSTEMCTL_NEED_RELOAD"
bash "$ROOT/scripts/install-keys-auto-sync.sh" --refresh --yes
before="$(rg -c -- '--user daemon-reload' "$SYSTEMCTL_LOG")"
# A fresh sg invocation changes its primary group, not the operator or safe modes.
KEY_TIMER_REAL_ID="$(command -v id)"
export KEY_TIMER_REAL_ID
cat > "$TMP/bin/id" <<'SH'
#!/usr/bin/env bash
if [ "$*" = '-g' ]; then printf '4294967294\n'; else exec "$KEY_TIMER_REAL_ID" "$@"; fi
SH
chmod 0755 "$TMP/bin/id"
group_inode="$(stat -c '%i' "$SUBYARD_KEYS_SYSTEMD_DIR/subyard-keys-sync.service")"
bash "$ROOT/scripts/install-keys-auto-sync.sh" --check-refresh
bash "$ROOT/scripts/install-keys-auto-sync.sh" --refresh --yes
[ "$(rg -c -- '--user daemon-reload' "$SYSTEMCTL_LOG")" = "$before" ]
[ "$(stat -c '%i' "$SUBYARD_KEYS_SYSTEMD_DIR/subyard-keys-sync.service")" = "$group_inode" ]
# Recovery after file publication must still reload the old loaded definitions.
touch "$SYSTEMCTL_NEED_RELOAD"
if bash "$ROOT/scripts/install-keys-auto-sync.sh" --check-refresh; then exit 1; fi
inode="$(stat -c '%i' "$SUBYARD_KEYS_SYSTEMD_DIR/subyard-keys-sync.service")"
bash "$ROOT/scripts/install-keys-auto-sync.sh" --refresh --yes
[ "$(stat -c '%i' "$SUBYARD_KEYS_SYSTEMD_DIR/subyard-keys-sync.service")" = "$inode" ]
bash "$ROOT/scripts/install-keys-auto-sync.sh" --check-refresh
SUBYARD_KEYS_SYSTEMD_DIR="$TMP/absent" bash "$ROOT/scripts/install-keys-auto-sync.sh" --refresh --yes >"$TMP/absent.log" 2>&1 && exit 1
[ ! -e "$TMP/absent" ]
# A trusted receipt repairs later drift; it is created only after an owned recipe.
printf 'changed managed unit\n' > "$SUBYARD_KEYS_SYSTEMD_DIR/subyard-keys-sync.service"
bash "$ROOT/scripts/install-keys-auto-sync.sh" --refresh --yes
bash "$ROOT/scripts/install-keys-auto-sync.sh" --check-refresh
[ "$(stat -c '%a' "$SUBYARD_KEYS_SYSTEMD_DIR/.subyard-keys-sync.json")" = 600 ]
foreign="$TMP/foreign"
mkdir -p "$foreign"
printf 'foreign user unit\n' > "$foreign/subyard-keys-sync.service"
chmod 0644 "$foreign/subyard-keys-sync.service"
foreign_before="$(stat -c '%i:%s:%Y:%Z' "$foreign/subyard-keys-sync.service")"
if SUBYARD_KEYS_SYSTEMD_DIR="$foreign" bash "$ROOT/scripts/install-keys-auto-sync.sh" --refresh --yes > "$TMP/foreign.log" 2>&1; then exit 1; fi
[ "$(stat -c '%i:%s:%Y:%Z' "$foreign/subyard-keys-sync.service")" = "$foreign_before" ]
[ "$(cat "$foreign/subyard-keys-sync.service")" = 'foreign user unit' ]
[ ! -e "$foreign/subyard-keys-sync.timer" ] && [ ! -e "$foreign/.subyard-keys-sync.json" ]
for unsafe in changed mode symlink; do
  units="$TMP/unsafe-$unsafe"
  mkdir -m 0700 "$units"
  sed "s|@YARD_BIN@|$YARD_RUNTIME_ROOT/current/bin/yard|g" \
    "$ROOT/tests/fixtures/systemd/subyard-keys-sync-v0.21.2.service.in" > "$units/subyard-keys-sync.service"
  chmod 0644 "$units/subyard-keys-sync.service"
  case "$unsafe" in
    changed) printf '# foreign modification\n' >> "$units/subyard-keys-sync.service" ;;
    mode) chmod 0664 "$units/subyard-keys-sync.service" ;;
    symlink) mv "$units/subyard-keys-sync.service" "$units/target"; ln -s target "$units/subyard-keys-sync.service" ;;
  esac
  if SUBYARD_KEYS_SYSTEMD_DIR="$units" bash "$ROOT/scripts/install-keys-auto-sync.sh" --refresh --yes > "$TMP/unsafe.log" 2>&1; then exit 1; fi
  [ ! -e "$units/.subyard-keys-sync.json" ] && [ ! -e "$units/subyard-keys-sync.timer" ]
done
# Removing both files retires timer intent, even when its prior receipt remains.
rm -- "$SUBYARD_KEYS_SYSTEMD_DIR/subyard-keys-sync.service" "$SUBYARD_KEYS_SYSTEMD_DIR/subyard-keys-sync.timer"
if bash "$ROOT/scripts/install-keys-auto-sync.sh" --refresh --yes > "$TMP/retired.log" 2>&1; then exit 1; fi
[ ! -e "$SUBYARD_KEYS_SYSTEMD_DIR/subyard-keys-sync.service" ] && [ ! -e "$SUBYARD_KEYS_SYSTEMD_DIR/subyard-keys-sync.timer" ]
printf 'ok: installed timer refresh preserves disabled/stopped and absent intent\n'
