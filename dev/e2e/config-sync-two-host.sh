#!/usr/bin/env bash
# Candidate-only two-host versioned configuration sync acceptance.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MODE="${1:-}"
PEER="${2:-}"
STATE="/var/lib/subyard-config-sync-e2e"
REMOTE_ROOT="/srv/subyard-config-sync-e2e"
SERVICE="subyard-config-sync-e2e.service"
VERSION="config-sync-e2e"

if [ "$(id -u)" -ne 0 ]; then
  exec sudo -n env HOME=/root USER=root LOGNAME=root PATH="$PATH" "$0" "$@"
fi
export GIT_CONFIG_COUNT=1
export GIT_CONFIG_KEY_0=safe.directory
export GIT_CONFIG_VALUE_0="$ROOT"

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

reset_owned_directory() {
  local path="$1" marker
  marker="$path/.subyard-config-sync-e2e"
  if [ -e "$path" ]; then
    [ -f "$marker" ] || fail "refusing to reset unmarked path $path"
    rm -rf -- "$path"
  fi
  install -d -m 0700 "$path"
  : >"$marker"
  chmod 0600 "$marker"
}

install_candidate_release() {
  local name="$1" root release yard
  root="$STATE/$name"
  release="$ROOT/.build/config-sync-release"
  yard="$root/bin/yard"
  install -d -m 0700 "$root/home" "$root/config" "$root/data" "$root/bin"
  "$ROOT/dev/package-engine.sh" \
    --output-dir "$release" --version "$VERSION" >/dev/null
  HOME="$root/home" \
    SUBYARD_HOME="$root/data" \
    SUBYARD_CONFIG_HOME="$root/config" \
    YARD_BIN_DIR="$root/bin" \
    YARD_SHELL_RC="$root/home/.bashrc" \
    YARD_LOGIN_RC="$root/home/.profile" \
    YARD_RELEASE_BASE_URL="file://$release" \
    YARD_RELEASE_VERSION="$VERSION" \
    "$release/subyard-install.sh" --yes >/dev/null
  [ "$("$yard" --version)" = "yard $VERSION" ] \
    || fail "$name did not activate the installed candidate release"
  case "$(readlink -f "$yard")" in
    "$root/data/runtime/releases/$VERSION-"*/bin/yard) ;;
    *) fail "$name yard command does not resolve to the installed immutable release" ;;
  esac
  printf 'installed: %s -> %s\n' "$("$yard" --version)" "$(readlink -f "$yard")"
}

configure_host() {
  local name="$1" root
  root="$STATE/$name"
  export HOME="$root/home"
  export SUBYARD_OPERATOR_HOME="$HOME"
  export SUBYARD_CONFIG_HOME="$root/config"
  export SUBYARD_HOME="$root/data"
  export SUBYARD_HOST_ID="$name"
  export SUBYARD_NO_AUDIT=1
  export GIT_AUTHOR_NAME="Subyard E2E"
  export GIT_AUTHOR_EMAIL="subyard-e2e@invalid"
  export GIT_COMMITTER_NAME="$GIT_AUTHOR_NAME"
  export GIT_COMMITTER_EMAIL="$GIT_AUTHOR_EMAIL"
  install -d -m 0700 "$HOME" "$SUBYARD_CONFIG_HOME" "$SUBYARD_HOME"
  install -m 0600 /dev/null "$SUBYARD_CONFIG_HOME/config.env"
}

install_git_daemon() {
  systemctl disable --now "$SERVICE" >/dev/null 2>&1 || true
  if test -e "$REMOTE_ROOT"; then
    test -f "$REMOTE_ROOT/.subyard-config-sync-e2e" \
      || fail "refusing to reset unmarked remote root"
    rm -rf -- "$REMOTE_ROOT"
  fi
  install -d -o root -g root -m 0755 "$REMOTE_ROOT"
  touch "$REMOTE_ROOT/.subyard-config-sync-e2e"
  git init --bare -q "$REMOTE_ROOT/remote.git"
  git --git-dir="$REMOTE_ROOT/remote.git" config daemon.receivepack true
  cat >"/etc/systemd/system/$SERVICE" <<UNIT
[Unit]
Description=Disposable Subyard config-sync Git daemon
After=network-online.target

[Service]
User=root
Group=root
ExecStart=/usr/bin/git daemon --reuseaddr --export-all --enable=receive-pack --base-path=$REMOTE_ROOT --listen=0.0.0.0 --port=19418 $REMOTE_ROOT/remote.git
Restart=on-failure

[Install]
WantedBy=multi-user.target
UNIT
  systemctl daemon-reload
  systemctl enable --now "$SERVICE"
}

wait_remote() {
  local url="$1" attempt
  for ((attempt = 0; attempt < 30; attempt++)); do
    if git ls-remote "$url" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  fail "Git remote did not become reachable: $url"
}

case "$MODE" in
  host-a-setup)
    reset_owned_directory "$STATE"
    install_git_daemon
    configure_host host-a
    install_candidate_release host-a
    yard="$STATE/host-a/bin/yard"
    "$yard" config sync connect \
      "file://$REMOTE_ROOT/remote.git" --host-id host-a \
      --checkout "$STATE/host-a/checkout" --init --yes
    "$yard" config set YARD_IMAGE images:debian/12 --scope shared --yes
    "$yard" config sync push -m "Host A shared setting" --yes
    "$yard" config sync status --offline
    printf 'ok: host A initialized and pushed shared configuration\n'
    ;;
  host-b-roundtrip)
    [ -n "$PEER" ] || fail "host-b-roundtrip requires the host A address"
    reset_owned_directory "$STATE"
    configure_host host-b
    install_candidate_release host-b
    yard="$STATE/host-b/bin/yard"
    remote="git://$PEER:19418/remote.git"
    wait_remote "$remote"
    "$yard" config sync connect "$remote" --host-id host-b \
      --checkout "$STATE/host-b/checkout" --yes
    show_output="$("$yard" config show YARD_IMAGE)"
    grep -Fq 'effective: images:debian/12' <<<"$show_output" \
      || fail "host B did not import host A's shared setting"
    "$yard" config set YARD_IMAGE images:debian/13 --scope shared --yes
    "$yard" config sync push -m "Host B shared setting" --yes
    "$yard" config sync status --offline
    printf 'ok: host B imported host A and pushed the reverse change\n'
    ;;
  host-a-verify)
    configure_host host-a
    yard="$STATE/host-a/bin/yard"
    [ "$("$yard" --version)" = "yard $VERSION" ] \
      || fail "host A installed candidate release is unavailable"
    status_output="$STATE/host-a-before-pull.status"
    if "$yard" config sync status >"$status_output"; then
      fail "host A status unexpectedly converged before the reverse pull"
    fi
    grep -Fq 'relation: behind 1' "$status_output" \
      || fail "host A did not report the reverse remote change"
    "$yard" config sync pull --apply --yes
    show_output="$("$yard" config show YARD_IMAGE)"
    grep -Fq 'effective: images:debian/13' <<<"$show_output" \
      || fail "host A did not import host B's shared setting"
    "$yard" config sync status --offline

    checkout="$("$yard" config sync path)"
    printf '\n# diagnostic dirty state\n' >>"$checkout/shared/config.env"
    dirty_output="$STATE/host-a-dirty.status"
    if "$yard" config sync status --offline >"$dirty_output"; then
      fail "dirty checkout status unexpectedly succeeded"
    fi
    grep -Fq 'worktree: dirty' "$dirty_output" \
      || fail "dirty checkout was not diagnosed"
    git -C "$checkout" restore -- shared/config.env
    "$yard" config sync status --offline
    printf 'ok: host A imported the reverse change and diagnosed dirty state\n'
    ;;
  cleanup)
    if [ -e "$STATE" ]; then
      [ -f "$STATE/.subyard-config-sync-e2e" ] \
        || fail "refusing to remove unmarked local state"
      rm -rf -- "$STATE"
    fi
    systemctl disable --now "$SERVICE" >/dev/null 2>&1 || true
    if test -e "$REMOTE_ROOT"; then
      test -f "$REMOTE_ROOT/.subyard-config-sync-e2e" \
        || fail "refusing to remove unmarked remote state"
      rm -rf -- "$REMOTE_ROOT"
    fi
    rm -f "/etc/systemd/system/$SERVICE"
    systemctl daemon-reload
    printf 'ok: config-sync candidate acceptance state removed\n'
    ;;
  *)
    fail "usage: $0 host-a-setup | host-b-roundtrip HOST_A_ADDRESS | host-a-verify | cleanup"
    ;;
esac
