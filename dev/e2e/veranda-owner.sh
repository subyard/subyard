#!/usr/bin/env bash
# Focused typed owner RPC acceptance on one disposable allocation.
set -Eeuo pipefail
umask 022
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
die() { printf 'veranda-owner: %s\n' "$*" >&2; exit 1; }
[ "${SUBYARD_E2E_VM:-}" = 1 ] || die 'run through dev/agent-e2e.sh on allocated VM1'
case "${SUBYARD_E2E_VERANDA_BOOTSTRAP_ONLY:-0}" in
  0|1) ;;
  *) die 'SUBYARD_E2E_VERANDA_BOOTSTRAP_ONLY must be 0 or 1' ;;
esac
case "${SUBYARD_E2E_VERANDA_NATIVE_ONLY:-0}" in
  0) ;;
  1)
    [ "${SUBYARD_E2E_VERANDA_NATIVE:-0}" = 1 ] || die 'native-only mode requires native acceptance'
    [ "${SUBYARD_E2E_VERANDA_BOOTSTRAP_ONLY:-0}" = 0 ] || die 'native-only and bootstrap-only modes are independent'
    ;;
  *) die 'SUBYARD_E2E_VERANDA_NATIVE_ONLY must be 0 or 1' ;;
esac
case "${SUBYARD_E2E_VERANDA_RELEASE_GUI_ONLY:-0}" in
  0) ;;
  1)
    [ "${SUBYARD_E2E_VERANDA_BOOTSTRAP_ONLY:-0}" = 0 ] \
      && [ "${SUBYARD_E2E_VERANDA_NATIVE_ONLY:-0}" = 0 ] \
      && [ "${SUBYARD_E2E_VERANDA_NATIVE:-0}" = 0 ] \
      && [ "${SUBYARD_E2E_VERANDA_TERMINAL:-0}" = 0 ] \
      && [ "${SUBYARD_E2E_VERANDA_EDITOR:-0}" = 0 ] || die 'release GUI acceptance is an independent mode'
    ;;
  *) die 'SUBYARD_E2E_VERANDA_RELEASE_GUI_ONLY must be 0 or 1' ;;
esac
case "${SUBYARD_E2E_VERANDA_GUI_GROWTH:-0}" in
  0) ;;
  1) [ "${SUBYARD_E2E_VERANDA_RELEASE_GUI_ONLY:-0}" = 1 ] || die 'GUI growth requires independent release GUI mode' ;;
  *) die 'SUBYARD_E2E_VERANDA_GUI_GROWTH must be 0 or 1' ;;
esac
case "${SUBYARD_E2E_VERANDA_GUI_GROWTH_CYCLES-20}" in
  20|100) ;;
  *) die 'SUBYARD_E2E_VERANDA_GUI_GROWTH_CYCLES must be 20 or 100' ;;
esac
[ "${SUBYARD_E2E_VERANDA_GUI_GROWTH:-0}" = 1 ] || [ -z "${SUBYARD_E2E_VERANDA_GUI_GROWTH_CYCLES+x}" ] \
  || die 'growth cycle selection requires independent GUI growth mode'
case "${SUBYARD_E2E_VERANDA_GUI_WAYLAND_GROWTH:-0}" in
  0) ;;
  1)
    [ "${SUBYARD_E2E_VERANDA_GUI_GROWTH:-0}" = 1 ] && [ "${SUBYARD_E2E_TYPE:-}" = android-test ] \
      || die 'Wayland growth requires independent GUI growth on android-test VM1'
    ;;
  *) die 'SUBYARD_E2E_VERANDA_GUI_WAYLAND_GROWTH must be 0 or 1' ;;
esac
for command in go python3 sudo sg git; do command -v "$command" >/dev/null || die "$command is required"; done
sudo -n true || die 'allocated VM requires passwordless sudo'
YARD_ENGINE="$ROOT/.build/yard"
if [ "${SUBYARD_E2E_VERANDA_GUI_WAYLAND_GROWTH:-0}" = 1 ]; then
  YARD_ENGINE="${SUBYARD_E2E_VERANDA_GUI_ENGINE:?frozen current engine is required}"
  python3 - "$YARD_ENGINE" 2>/dev/null <<'PY' || die 'unsafe frozen engine'
import os,pathlib,stat,sys
info=pathlib.Path(sys.argv[1]).lstat()
assert stat.S_ISREG(info.st_mode) and info.st_uid==os.geteuid() and info.st_nlink==1
assert os.access(sys.argv[1],os.X_OK) and not stat.S_IMODE(info.st_mode)&0o022
PY
  [ "$(timeout 5s "$YARD_ENGINE" --version)" = "$(basename -- "$YARD_ENGINE") 0.1.1" ] || die 'frozen engine release version mismatch'
else
  "$ROOT/dev/build-engine.sh"
fi
STATE="$(mktemp -d /var/tmp/subyard-veranda-owner.XXXXXX)"
chmod 0700 "$STATE"
token="$(printf '%s' "${STATE##*.}" | tr '[:upper:]' '[:lower:]')"
MARKER="subyard-veranda-owner-v1-$token"
YARD_NAME="vo-$token"
CREATED_YARD_NAME="$YARD_NAME-new"
PROFILE_NAME="veranda-fixture-$token"
printf '%s\n' "$MARKER" > "$STATE/.marker"
chmod 0600 "$STATE/.marker"
export SUBYARD_OPERATOR_HOME="$HOME"
export SUBYARD_CONFIG_HOME="$STATE/config" SUBYARD_HOME="$STATE/data"
export STORAGE_PATH="$HOME/.cache/subyard-e2e-platform/incus/incus/storage"
export SUBYARD_NO_AUDIT=1 SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1 MIN_DISK_GIB=1
yard_in() {
  local selected_yard="$1" invocation
  shift
  if ! id -nG | tr ' ' '\n' | grep -Fxq incus-admin \
    && id -nG "$(id -un)" | tr ' ' '\n' | grep -Fxq incus-admin; then
    printf -v invocation '%q ' "$YARD_ENGINE" -Y "$selected_yard" "$@"
    sg incus-admin -c "exec $invocation"
  else
    "$YARD_ENGINE" -Y "$selected_yard" "$@"
  fi
}
yard() { yard_in "$YARD_NAME" "$@"; }
cleanup() {
  local rc=$?
  trap - EXIT INT TERM ERR
  set +e
  if [ -f "$STATE/.marker" ] && [ "$(cat "$STATE/.marker")" = "$MARKER" ]; then
    # Preserve the platform substrate while tearing down this exact fixture yard.
    install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/platform-sentinel"
    printf 'SSH_PORT=64997\n' > "$SUBYARD_CONFIG_HOME/yards/platform-sentinel/config.env"
    chmod 0600 "$SUBYARD_CONFIG_HOME/yards/platform-sentinel/config.env"
    if [ -f "$SUBYARD_CONFIG_HOME/yards/$CREATED_YARD_NAME/config.env" ]; then
      yard_in "$CREATED_YARD_NAME" teardown --yes >> "$STATE/cleanup.log" 2>&1 || rc=3
    fi
    yard teardown --yes > "$STATE/cleanup.log" 2>&1 || rc=3
    if [ "$rc" = 0 ]; then sudo -n find "$STATE" -depth -delete || rc=3; fi
  fi
  [ "$rc" = 0 ] || printf 'veranda-owner: failed fixture retained in allocated VM\n' >&2
  exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
install -d -m 0700 "$SUBYARD_CONFIG_HOME" "$SUBYARD_CONFIG_HOME/yards" \
  "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME" "$STATE/project"
profile="$ROOT/config/profiles/$PROFILE_NAME"
[ ! -e "$profile" ] || die 'fixture profile already exists'
install -d -m 0755 "$profile"
printf 'PROFILE_NAME=%s\n' "$PROFILE_NAME" > "$profile/profile.conf"
cat > "$profile/profile.json" <<'JSON'
{"schema_version":1,"selected_provision_only":true,"settings":[{"name":"VERANDA_SAMPLE_LIMIT","type":"integer","default":"7","scopes":["shipped","yard"],"syncable":true,"application":"next-command","minimum":1,"maximum":12}]}
JSON
cat > "$profile/provision.sh" <<'SH'
#!/usr/bin/env bash
# subyard-provision-check-v1
set -euo pipefail
case "${1:-}" in ''|--check) ;; *) exit 2 ;; esac
true
SH
chmod 0644 "$profile/profile.conf" "$profile/profile.json"
chmod 0755 "$profile/provision.sh"
cat > "$profile/yard.env" <<EOF
# $MARKER named bootstrap preset
YARD_KIND=container
SSH_PORT=$((36000 + $$ % 1000))
LIMITS_MEMORY=512MiB
HOST_BASE=$STATE/created-host
RESTRICTED_DISK_PATHS=$STATE/created-host
FORWARD_SSH_AGENT=0
ENVIRONMENT_PROFILES=
CODING_TOOL_INTEGRATIONS=
HOST_CLAUDE_MD=
HOST_CODEX_AGENTS_MD=
HOST_OPENCODE_AGENTS_MD=
EOF
chmod 0644 "$profile/yard.env"
printf 'CODING_TOOL_INTEGRATIONS=\n' > "$SUBYARD_CONFIG_HOME/config.env"
chmod 0600 "$SUBYARD_CONFIG_HOME/config.env"
cat > "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME/config.env" <<EOF
# $MARKER
YARD_KIND=container
SSH_PORT=$((34000 + $$ % 1000))
HOST_BASE=$STATE/host
RESTRICTED_DISK_PATHS=$STATE/host
FORWARD_SSH_AGENT=0
ENVIRONMENT_PROFILES=$PROFILE_NAME
VERANDA_SAMPLE_LIMIT=8
HOST_CLAUDE_MD=
HOST_CODEX_AGENTS_MD=
HOST_OPENCODE_AGENTS_MD=
EOF
chmod 0600 "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME/config.env"
if [ "${SUBYARD_E2E_VERANDA_BOOTSTRAP_ONLY:-0}" = 0 ]; then
  printf 'bounded synthetic project\n' > "$STATE/project/example.txt"
  chmod 0644 "$STATE/project/example.txt"
  git -C "$STATE/project" init -q
  git -C "$STATE/project" add -- example.txt
  git -C "$STATE/project" -c user.name='Subyard Test' -c user.email=test@invalid commit -qm 'Seed owner RPC fixture'
fi
platform="$HOME/.cache/subyard-e2e-platform"
if [ -L "$HOME/.cache" ] || [ -L "$platform" ]; then
  die 'platform root must be plain'
fi
sudo -n install -d -o "$(id -u)" -g "$(id -g)" -m 0711 "$platform"
printf 'veranda-owner: product init\n'
yard init --yes
yard start --yes
if [ "${SUBYARD_E2E_VERANDA_BOOTSTRAP_ONLY:-0}" = 0 ]; then
  yard init --yes
fi
printf '%s\n' subyard-e2e-platform-v1 > "$STATE/platform-marker"
sudo -n install -o "$(id -u)" -g "$(id -g)" -m 0600 "$STATE/platform-marker" "$platform/.subyard-e2e-platform-marker.$token"
sudo -n mv -fT "$platform/.subyard-e2e-platform-marker.$token" "$platform/.subyard-e2e-platform-marker"
yard check
if [ "${SUBYARD_E2E_VERANDA_BOOTSTRAP_ONLY:-0}" = 0 ]; then
  yard sync "$STATE/project" --yes
  install -d -m 0700 "$STATE/settings-source/shared"
  printf '{"schemaVersion":1}\n' > "$STATE/settings-source/subyard-config.json"
  printf 'CODING_TOOL_INTEGRATIONS=\n' > "$STATE/settings-source/shared/config.env"
  chmod 0600 "$STATE/settings-source/subyard-config.json" "$STATE/settings-source/shared/config.env"
  git -C "$STATE/settings-source" init -q
  git -C "$STATE/settings-source" add -- subyard-config.json shared/config.env
  git -C "$STATE/settings-source" -c user.name='Subyard Test' -c user.email=test@invalid commit -qm 'Seed owner sync fixture'
  yard config sync connect "$STATE/settings-source" --checkout "$STATE/settings-checkout" --yes
fi
if [ "${SUBYARD_E2E_VERANDA_RELEASE_GUI_ONLY:-0}" = 1 ]; then
  gui_base="${SUBYARD_E2E_VERANDA_GUI_BASE_DEB:-$ROOT/.build/veranda-package-noop-base.deb}"
  gui_next="${SUBYARD_E2E_VERANDA_GUI_NEXT_DEB:-$ROOT/.build/veranda-package-noop-next.deb}"
  gui_args=("$gui_base" "$gui_next" "$YARD_NAME" "$STATE")
  if [ "${SUBYARD_E2E_VERANDA_GUI_GROWTH:-0}" = 1 ]; then
    gui_args=(--growth "$gui_next" "$YARD_NAME" "$STATE")
  fi
  if [ "${SUBYARD_E2E_VERANDA_GUI_WAYLAND_GROWTH:-0}" = 1 ]; then
    gui_args=(--wayland-growth "$gui_next" "$YARD_ENGINE" "$YARD_NAME" "$STATE")
  fi
  printf -v gui_scenario '%q ' bash "$ROOT/dev/e2e/veranda-release-gui.sh" \
    "${gui_args[@]}"
  if ! id -nG | tr ' ' '\n' | grep -Fxq incus-admin \
    && id -nG "$(id -un)" | tr ' ' '\n' | grep -Fxq incus-admin; then
    sg incus-admin -c "exec $gui_scenario"
  else
    bash "$ROOT/dev/e2e/veranda-release-gui.sh" "${gui_args[@]}"
  fi
  if [ "${SUBYARD_E2E_VERANDA_GUI_GROWTH:-0}" = 1 ]; then
    printf 'ok: focused Veranda whole-GUI connection growth; release matrix/native/owner/bootstrap suites excluded\n'
  else
    printf 'ok: focused Veranda release-version GUI matrix; native/owner/bootstrap suites excluded\n'
  fi
  exit 0
fi
if [ "${SUBYARD_E2E_VERANDA_NATIVE_ONLY:-0}" = 1 ]; then
  printf 'veranda-owner: native-only acceptance; framed owner/bootstrap suite excluded\n'
else
  printf 'veranda-owner: framed RPC physical scenario\n'
  printf -v scenario '%q ' python3 "$ROOT/dev/e2e/veranda-owner.py" "$ROOT/.build/yard" "$YARD_NAME" "$PROFILE_NAME" "$STATE"
  if ! id -nG | tr ' ' '\n' | grep -Fxq incus-admin \
    && id -nG "$(id -un)" | tr ' ' '\n' | grep -Fxq incus-admin; then
    sg incus-admin -c "exec $scenario"
  else
    python3 "$ROOT/dev/e2e/veranda-owner.py" "$ROOT/.build/yard" "$YARD_NAME" "$PROFILE_NAME" "$STATE"
  fi
fi
case "${SUBYARD_E2E_VERANDA_NATIVE:-0}" in
  0) ;;
  1)
    [ -x "$ROOT/.build/veranda-native-test" ] || die 'native acceptance requires the staged candidate test binary'
    printf -v native_scenario '%q ' bash "$ROOT/dev/e2e/veranda-native.sh" \
      "$ROOT/.build/veranda-native-test" "$ROOT/.build/yard" "$YARD_NAME" "$STATE"
    if ! id -nG | tr ' ' '\n' | grep -Fxq incus-admin \
      && id -nG "$(id -un)" | tr ' ' '\n' | grep -Fxq incus-admin; then
      sg incus-admin -c "exec $native_scenario"
    else
      bash "$ROOT/dev/e2e/veranda-native.sh" "$ROOT/.build/veranda-native-test" \
        "$ROOT/.build/yard" "$YARD_NAME" "$STATE"
    fi
    ;;
  *) die 'SUBYARD_E2E_VERANDA_NATIVE must be 0 or 1' ;;
esac
if [ "${SUBYARD_E2E_VERANDA_NATIVE_ONLY:-0}" = 1 ]; then
  printf 'ok: focused Veranda native local/SSH acceptance; owner/bootstrap suite excluded\n'
else
  printf 'ok: focused Veranda owner RPC physical acceptance\n'
fi
