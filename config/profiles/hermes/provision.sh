#!/usr/bin/env bash
# subyard-provision-check-v1
# Install the pinned, headless Hermes runtime in one dedicated yard.
set -euo pipefail

check_only=0
case "${1:-}" in
  --check) check_only=1; shift ;;
  "") ;;
  *) printf 'hermes provision: unknown argument %s\n' "$1" >&2; exit 2 ;;
esac
[ "$#" -eq 0 ] || { printf 'hermes provision: unexpected argument\n' >&2; exit 2; }

die() { printf 'hermes provision: %s\n' "$*" >&2; exit 1; }
if [ "$(id -u)" -ne 0 ] && [ "${HERMES_TEST_ALLOW_NON_ROOT:-0}" != 1 ]; then
  die "must run as root"
fi

profile_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root_prefix="${HERMES_TEST_ROOT:-}"
rooted() { printf '%s%s' "$root_prefix" "$1"; }

: "${HERMES_VERSION:?}"
: "${HERMES_TAG:?}"
: "${HERMES_COMMIT:?}"
: "${HERMES_SOURCE_SHA256:?}"
: "${HERMES_PYTHON_VERSION:?}"
: "${HERMES_UV_VERSION:?}"
: "${HERMES_UV_AMD64_SHA256:?}"
: "${HERMES_UV_ARM64_SHA256:?}"
: "${HERMES_NODE_VERSION:?}"
: "${HERMES_NPM_VERSION:?}"
: "${HERMES_NODE_AMD64_SHA256:?}"
: "${HERMES_NODE_ARM64_SHA256:?}"
: "${HERMES_AGENT_BROWSER_VERSION:?}"
: "${HERMES_AGENT_BROWSER_SHA256:?}"
: "${HERMES_PLAYWRIGHT_VERSION:?}"
: "${HERMES_HOME:=/srv/hermes}"
: "${HERMES_PORT:=9119}"

DEV_USER="${DEV_USER:-dev}"
DEV_GROUP="${DEV_GROUP:-$(id -gn "$DEV_USER")}"
DEV_HOME="${HERMES_DEV_HOME:-$(getent passwd "$DEV_USER" | cut -d: -f6)}"
DEV_HOME="${DEV_HOME:-/home/$DEV_USER}"

install_root="$(rooted /opt/hermes-agent)"
source_root="$install_root/source"
venv_root="$install_root/venv"
uv_bin="$install_root/bin/uv"
python_root="$install_root/python"
node_root="$install_root/node"
agent_browser_root="$install_root/agent-browser"
agent_browser="$agent_browser_root/bin/agent-browser.js"
playwright_root="$install_root/playwright"
browser_marker="$install_root/.subyard-browser-runtime"
cache_root="$(rooted /var/cache/subyard/hermes-uv)"
state_root="$(rooted "$HERMES_HOME")"
etc_root="$(rooted /etc/subyard)"
runtime_env="$etc_root/hermes-runtime.env"
libexec_root="$(rooted /usr/local/libexec)"
bin_root="$(rooted /usr/local/bin)"
sbin_root="$(rooted /usr/local/sbin)"
unit_root="$(rooted /etc/systemd/system)"
ready="$state_root/.provider-ready"
transaction_marker="${install_root}.transaction"
rollback_runtime="${install_root}.rollback"
rollback_state_name=.subyard-rollback-state
managed_paths=(
  "$bin_root/node"
  "$bin_root/npm"
  "$bin_root/npx"
  "$bin_root/agent-browser"
  "$bin_root/hermes"
  "$runtime_env"
  "$libexec_root/subyard-hermes-pin-check"
  "$libexec_root/subyard-hermes-verify-backup"
  "$sbin_root/hermes-provider-ready"
  "$sbin_root/hermes-backup-create"
  "$sbin_root/hermes-backup-finalize"
  "$sbin_root/hermes-restore"
  "$unit_root/hermes-serve.service"
  "$ready"
)
transaction_active=0
previous_runtime=""
service_was_active=0
service_was_enabled=0
orphaned_rollback=0

if [ "$check_only" -eq 1 ]; then
  wanted_version="$HERMES_VERSION"
  wanted_tag="$HERMES_TAG"
  wanted_commit="$HERMES_COMMIT"
  wanted_node="$HERMES_NODE_VERSION"
  wanted_npm="$HERMES_NPM_VERSION"
  wanted_browser="$HERMES_AGENT_BROWSER_VERSION"
  wanted_playwright="$HERMES_PLAYWRIGHT_VERSION"
  changed=0
  [ ! -e "$transaction_marker" ] && [ ! -L "$transaction_marker" ] || changed=1
  [ ! -e "$rollback_runtime" ] && [ ! -L "$rollback_runtime" ] || changed=1
  if [ ! -r "$runtime_env" ] || [ -L "$runtime_env" ]; then
    changed=1
  elif ! (
    # shellcheck disable=SC1090
    . "$runtime_env"
    [ "$HERMES_VERSION" = "$wanted_version" ] \
      && [ "$HERMES_TAG" = "$wanted_tag" ] \
      && [ "$HERMES_COMMIT" = "$wanted_commit" ] \
      && [ "$HERMES_NODE_VERSION" = "$wanted_node" ] \
      && [ "$HERMES_NPM_VERSION" = "$wanted_npm" ] \
      && [ "$HERMES_AGENT_BROWSER_VERSION" = "$wanted_browser" ] \
      && [ "$HERMES_PLAYWRIGHT_VERSION" = "$wanted_playwright" ] \
      && [ "$HERMES_HOME" = "$state_root" ] \
      && [ "$HERMES_INSTALL_ROOT" = "$install_root" ] \
      && [ -x "$libexec_root/subyard-hermes-pin-check" ] \
      && HERMES_RUNTIME_ENV="$runtime_env" \
        "$libexec_root/subyard-hermes-pin-check" --runtime-only >/dev/null 2>&1
  ); then
    changed=1
  fi
  for pair in \
    "hermes-pin-check:$libexec_root/subyard-hermes-pin-check" \
    "verify-backup.py:$libexec_root/subyard-hermes-verify-backup" \
    "hermes-provider-ready:$sbin_root/hermes-provider-ready" \
    "hermes-backup-create:$sbin_root/hermes-backup-create" \
    "hermes-backup-finalize:$sbin_root/hermes-backup-finalize" \
    "hermes-restore:$sbin_root/hermes-restore"; do
    source_path="$profile_dir/${pair%%:*}"
    destination="${pair#*:}"
    [ -f "$destination" ] && [ ! -L "$destination" ] \
      && cmp -s -- "$source_path" "$destination" || changed=1
  done
  [ "$changed" -eq 0 ] && exit 0
  exit 10
fi

export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq ca-certificates curl openssl procps xz-utils

case "$(dpkg --print-architecture)" in
  amd64)
    uv_target=x86_64-unknown-linux-gnu
    uv_sha="$HERMES_UV_AMD64_SHA256"
    node_arch=x64
    node_sha="$HERMES_NODE_AMD64_SHA256"
    ;;
  arm64)
    uv_target=aarch64-unknown-linux-gnu
    uv_sha="$HERMES_UV_ARM64_SHA256"
    node_arch=arm64
    node_sha="$HERMES_NODE_ARM64_SHA256"
    ;;
  *) die "unsupported architecture" ;;
esac

download() {
  url="$1"
  output="$2"
  curl --fail --location --silent --show-error \
    --retry 5 --retry-all-errors --connect-timeout 30 "$url" -o "$output"
}

write_transaction_marker() {
  install -d -m 0755 "$(dirname "$transaction_marker")"
  marker_tmp="$(mktemp "${transaction_marker}.XXXXXX")"
  printf '%s\n' subyard-hermes-runtime-transaction-v1 > "$marker_tmp"
  chmod 0600 "$marker_tmp"
  mv -fT -- "$marker_tmp" "$transaction_marker"
}

validate_transaction_marker() {
  [ -f "$transaction_marker" ] && [ ! -L "$transaction_marker" ] \
    && [ "$(<"$transaction_marker")" = subyard-hermes-runtime-transaction-v1 ] \
    || die "invalid Hermes runtime transaction marker"
}

backup_managed_state() { # <state-directory>
  backup_root="$1"
  [ ! -e "$backup_root" ] && [ ! -L "$backup_root" ] \
    || die "stale managed-state backup exists"
  install -d -m 0700 "$backup_root"
  service_was_active=0
  service_was_enabled=0
  if systemctl is-active --quiet hermes-serve.service >/dev/null 2>&1; then
    service_was_active=1
  fi
  if systemctl is-enabled --quiet hermes-serve.service >/dev/null 2>&1; then
    service_was_enabled=1
  fi
  printf '%s\n' "$service_was_active" > "$backup_root/service-was-active"
  printf '%s\n' "$service_was_enabled" > "$backup_root/service-was-enabled"
  for index in "${!managed_paths[@]}"; do
    path="${managed_paths[$index]}"
    if [ -e "$path" ] || [ -L "$path" ]; then
      [ ! -d "$path" ] || [ -L "$path" ] \
        || die "managed path is unexpectedly a directory: $path"
      cp -a -- "$path" "$backup_root/item-$index"
      : > "$backup_root/item-$index.present"
    fi
  done
}

restore_managed_state() { # <state-directory>
  backup_root="$1"
  [ -d "$backup_root" ] && [ ! -L "$backup_root" ] \
    && [ -r "$backup_root/service-was-active" ] \
    && [ -r "$backup_root/service-was-enabled" ] \
    || return 1
  service_was_active="$(<"$backup_root/service-was-active")"
  service_was_enabled="$(<"$backup_root/service-was-enabled")"
  case "$service_was_active" in 0|1) ;; *) return 1 ;; esac
  case "$service_was_enabled" in 0|1) ;; *) return 1 ;; esac
  for index in "${!managed_paths[@]}"; do
    path="${managed_paths[$index]}"
    if [ -e "$path" ] || [ -L "$path" ]; then
      [ ! -d "$path" ] || [ -L "$path" ] || return 1
      rm -f -- "$path" || return 1
    fi
    if [ -e "$backup_root/item-$index.present" ]; then
      [ -e "$backup_root/item-$index" ] || [ -L "$backup_root/item-$index" ] \
        || return 1
      parent="$(dirname "$path")"
      if [ ! -d "$parent" ]; then
        install -d -m 0755 "$parent" || return 1
      fi
      cp -a -- "$backup_root/item-$index" "$path" || return 1
    fi
  done
}

restore_saved_state() { # <state-directory>
  backup_root="$1"
  restore_managed_state "$backup_root" || return 1
  service_was_active="$(<"$backup_root/service-was-active")"
  service_was_enabled="$(<"$backup_root/service-was-enabled")"
  systemctl daemon-reload >/dev/null 2>&1 || return 1
  if [ "$service_was_enabled" = 1 ]; then
    systemctl enable hermes-serve.service >/dev/null 2>&1 || return 1
  else
    systemctl disable hermes-serve.service >/dev/null 2>&1 || return 1
  fi
  if [ "$service_was_active" = 1 ]; then
    systemctl start hermes-serve.service >/dev/null 2>&1 || return 1
  else
    systemctl stop hermes-serve.service >/dev/null 2>&1 || return 1
  fi
  rm -f -- "$transaction_marker" || return 1
  rm -rf -- "$backup_root" || return 1
}

remove_partial_runtime() {
  [ ! -e "$install_root" ] && [ ! -L "$install_root" ] && return 0
  [ -d "$install_root" ] && [ ! -L "$install_root" ] \
    || die "refusing to remove an unsafe partial runtime"
  rm -rf -- "$install_root"
}

restore_previous_runtime() { # <rollback-directory>
  rollback="$1"
  backup_root="$rollback/$rollback_state_name"
  [ -d "$rollback" ] && [ ! -L "$rollback" ] \
    && [ -r "$rollback/.subyard-commit" ] \
    || return 1
  systemctl stop hermes-serve.service >/dev/null 2>&1 || true
  remove_partial_runtime || return 1
  mv "$rollback" "$install_root" || return 1
  backup_root="$install_root/$rollback_state_name"
  if [ "${HERMES_TEST_ABORT_AFTER_RUNTIME_RESTORE:-0}" = 1 ]; then
    trap - EXIT INT TERM
    exit 137
  fi
  restore_saved_state "$backup_root"
}

rollback_on_exit() {
  status=$?
  trap - EXIT INT TERM
  [ "$status" -ne 0 ] || return 0
  if [ "$transaction_active" = 1 ] && [ -n "$previous_runtime" ] \
    && [ -e "$previous_runtime" ]; then
    set +e
    restore_previous_runtime "$previous_runtime"
    restore_status=$?
    if [ "$restore_status" -eq 0 ]; then
      rm -f -- "$transaction_marker"
      printf 'hermes provision: failed transaction rolled back to the previous runtime\n' >&2
    else
      printf 'hermes provision: automatic runtime rollback failed; transaction state retained\n' >&2
    fi
  fi
  exit "$status"
}

if [ -e "$transaction_marker" ] || [ -L "$transaction_marker" ]; then
  validate_transaction_marker
fi
if [ -e "$rollback_runtime" ] || [ -L "$rollback_runtime" ]; then
  [ -d "$rollback_runtime" ] && [ ! -L "$rollback_runtime" ] \
    || die "invalid Hermes rollback runtime"
  if [ ! -e "$transaction_marker" ]; then
    [ -r "$install_root/.subyard-commit" ] \
      || die "orphaned rollback runtime has no committed replacement"
    orphaned_rollback=1
  else
    [ -r "$rollback_runtime/.subyard-commit" ] \
      && [ -d "$rollback_runtime/$rollback_state_name" ] \
      && [ ! -L "$rollback_runtime/$rollback_state_name" ] \
      || die "invalid Hermes rollback transaction"
    if [ ! -r "$install_root/.subyard-commit" ]; then
      restore_previous_runtime "$rollback_runtime" \
        || die "could not recover the interrupted Hermes runtime transaction"
      rm -f -- "$transaction_marker"
    else
      previous_runtime="$rollback_runtime"
      transaction_active=1
      [ -r "$rollback_runtime/$rollback_state_name/service-was-active" ] \
        || die "rollback runtime has no service-state record"
      [ -r "$rollback_runtime/$rollback_state_name/service-was-enabled" ] \
        || die "rollback runtime has no service-enable record"
      service_was_active="$(<"$rollback_runtime/$rollback_state_name/service-was-active")"
      service_was_enabled="$(<"$rollback_runtime/$rollback_state_name/service-was-enabled")"
    fi
  fi
elif [ -e "$install_root/$rollback_state_name" ] \
  || [ -L "$install_root/$rollback_state_name" ]; then
  [ -d "$install_root/$rollback_state_name" ] \
    && [ ! -L "$install_root/$rollback_state_name" ] \
    || die "invalid embedded Hermes rollback state"
  if [ -e "$transaction_marker" ]; then
    systemctl stop hermes-serve.service >/dev/null 2>&1 || true
    restore_saved_state "$install_root/$rollback_state_name" \
      || die "could not recover saved Hermes managed and service state"
  else
    rm -rf -- "${install_root:?}/$rollback_state_name"
  fi
elif [ -e "$transaction_marker" ]; then
  if [ -r "$install_root/.subyard-commit" ]; then
    transaction_active=1
  else
    remove_partial_runtime
    rm -f -- "$transaction_marker"
  fi
fi

if [ "$transaction_active" = 1 ]; then
  trap rollback_on_exit EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
fi

old_commit=""
[ ! -r "$install_root/.subyard-commit" ] || old_commit="$(<"$install_root/.subyard-commit")"
browser_contract="node=$HERMES_NODE_VERSION npm=$HERMES_NPM_VERSION agent-browser=$HERMES_AGENT_BROWSER_VERSION agent-browser-sha256=$HERMES_AGENT_BROWSER_SHA256 playwright=$HERMES_PLAYWRIGHT_VERSION"
runtime_current=0
if [ "$old_commit" = "$HERMES_COMMIT" ] \
  && [ -r "$install_root/.subyard-source-sha256" ] \
  && [ "$(<"$install_root/.subyard-source-sha256")" = "$HERMES_SOURCE_SHA256" ] \
  && [ -x "$uv_bin" ] && [ -r "$source_root/uv.lock" ] \
  && [ -r "$source_root/package-lock.json" ] \
  && [ -x "$node_root/bin/node" ] && [ -x "$node_root/bin/npm" ] \
  && [ -x "$node_root/bin/npx" ] && [ -x "$agent_browser" ] \
  && [ "$("$node_root/bin/node" --version 2>/dev/null)" = "v$HERMES_NODE_VERSION" ] \
  && [ "$(PATH="$node_root/bin:$PATH" "$node_root/bin/npm" --version 2>/dev/null)" = \
    "$HERMES_NPM_VERSION" ] \
  && [ "$(PATH="$node_root/bin:$PATH" "$agent_browser" --version 2>/dev/null)" = \
    "agent-browser $HERMES_AGENT_BROWSER_VERSION" ] \
  && [ -r "$browser_marker" ] && [ "$(<"$browser_marker")" = "$browser_contract" ] \
  && [ -n "$(find "$playwright_root" -type f -name chrome -perm /111 -print -quit 2>/dev/null)" ]; then
  runtime_current=1
fi

if [ "$orphaned_rollback" = 1 ]; then
  [ "$runtime_current" = 1 ] \
    || die "cannot clean an interrupted rollback after an unverified replacement"
  rm -rf -- "$rollback_runtime"
fi

if [ -n "$old_commit" ] && [ "$old_commit" != "$HERMES_COMMIT" ]; then
  verified="$state_root/.last-verified-backup"
  [ -r "$verified" ] && grep -Fxq "commit=$old_commit" "$verified" \
    || die "pin update requires a verified backup of commit $old_commit"
fi
if [ -e "$install_root" ] && [ -z "$old_commit" ]; then
  die "$install_root exists without a Subyard commit marker"
fi

runtime_owner=root
[ "$(id -u)" -eq 0 ] || runtime_owner="$DEV_USER"
if [ "$runtime_current" != 1 ]; then
  if [ -n "$previous_runtime" ]; then
    trap - EXIT INT TERM
    transaction_active=0
    restore_previous_runtime "$previous_runtime" \
      || die "could not roll back an invalid interrupted runtime"
    rm -f -- "$transaction_marker"
    exec bash "$0" "$@"
  fi
  if [ -e "$install_root" ]; then
    write_transaction_marker
    backup_managed_state "$install_root/$rollback_state_name"
    systemctl stop hermes-serve.service >/dev/null 2>&1 || true
    if [ "${HERMES_TEST_ABORT_AFTER_SERVICE_STOP:-0}" = 1 ]; then
      trap - EXIT INT TERM
      exit 137
    fi
    mv "$install_root" "$rollback_runtime"
    previous_runtime="$rollback_runtime"
  else
    write_transaction_marker
  fi
  transaction_active=1
  trap rollback_on_exit EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM

  if [ "${HERMES_TEST_ABORT_DURING_RUNTIME:-0}" = 1 ]; then
    install -d -m 0755 "$install_root"
    trap - EXIT INT TERM
    exit 137
  fi

  set +e
  (
    set -e
    work="$(mktemp -d)"
    trap 'rm -rf -- "$work"' EXIT
    source_archive="$work/hermes-source.tar.gz"
    uv_archive="$work/uv.tar.gz"
    node_dist="node-v$HERMES_NODE_VERSION-linux-$node_arch"
    node_archive="$work/$node_dist.tar.xz"
    agent_browser_archive="$work/agent-browser-$HERMES_AGENT_BROWSER_VERSION.tgz"

    download \
      "https://codeload.github.com/NousResearch/hermes-agent/tar.gz/$HERMES_COMMIT" \
      "$source_archive"
    printf '%s  %s\n' "$HERMES_SOURCE_SHA256" "$source_archive" | sha256sum -c -
    download \
      "https://github.com/astral-sh/uv/releases/download/$HERMES_UV_VERSION/uv-$uv_target.tar.gz" \
      "$uv_archive"
    printf '%s  %s\n' "$uv_sha" "$uv_archive" | sha256sum -c -
    download \
      "https://nodejs.org/dist/v$HERMES_NODE_VERSION/$node_dist.tar.xz" \
      "$node_archive"
    printf '%s  %s\n' "$node_sha" "$node_archive" | sha256sum -c -
    download \
      "https://registry.npmjs.org/agent-browser/-/agent-browser-$HERMES_AGENT_BROWSER_VERSION.tgz" \
      "$agent_browser_archive"
    printf '%s  %s\n' "$HERMES_AGENT_BROWSER_SHA256" "$agent_browser_archive" \
      | sha256sum -c -

    install -d -m 0755 "$source_root" "$install_root/bin" "$python_root" \
      "$node_root" "$agent_browser_root" "$playwright_root" "$cache_root/npm"
    tar -xzf "$source_archive" -C "$source_root" --strip-components=1 \
      --no-same-owner --no-same-permissions
    tar -xzf "$uv_archive" -C "$work"
    install -m 0755 "$work/uv-$uv_target/uv" "$uv_bin"
    case "$("$uv_bin" --version)" in
      "uv $HERMES_UV_VERSION"|"uv $HERMES_UV_VERSION ($uv_target)") ;;
      *) die "downloaded uv has an unexpected version" ;;
    esac
    tar -xJf "$node_archive" -C "$node_root" --strip-components=1 \
      --no-same-owner --no-same-permissions
    tar -xzf "$agent_browser_archive" -C "$agent_browser_root" --strip-components=1 \
      --no-same-owner --no-same-permissions
    [ "$("$node_root/bin/node" --version)" = "v$HERMES_NODE_VERSION" ] \
      || die "downloaded Node.js has an unexpected version"
    [ "$(PATH="$node_root/bin:$PATH" "$node_root/bin/npm" --version)" = \
      "$HERMES_NPM_VERSION" ] \
      || die "downloaded npm has an unexpected version"

    (
      cd "$source_root"
      UV_CACHE_DIR="$cache_root" UV_PYTHON_INSTALL_DIR="$python_root" \
        "$uv_bin" python install "$HERMES_PYTHON_VERSION"
      UV_CACHE_DIR="$cache_root" UV_PYTHON_INSTALL_DIR="$python_root" \
        UV_PROJECT_ENVIRONMENT="$venv_root" \
        "$uv_bin" sync --locked --no-dev --python "$HERMES_PYTHON_VERSION"
    )
    [ -x "$venv_root/bin/hermes" ]
    actual_version="$("$venv_root/bin/python" -c \
      'from hermes_cli import __version__; print(__version__)')"
    [ "$actual_version" = "$HERMES_VERSION" ]

    "$venv_root/bin/python" - "$source_root/package-lock.json" \
      "$HERMES_AGENT_BROWSER_VERSION" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as handle:
    lock = json.load(handle)
actual = lock.get("packages", {}).get("node_modules/agent-browser", {}).get("version")
if actual != sys.argv[2]:
    raise SystemExit("locked agent-browser version mismatch")
PY
    [ -x "$agent_browser" ] || die "agent-browser package entrypoint is missing"
    [ "$(PATH="$node_root/bin:$PATH" "$agent_browser" --version)" = \
      "agent-browser $HERMES_AGENT_BROWSER_VERSION" ] \
      || die "verified agent-browser has an unexpected version"
    PATH="$node_root/bin:$agent_browser_root/bin:$PATH" \
      npm_config_cache="$cache_root/npm" PLAYWRIGHT_BROWSERS_PATH="$playwright_root" \
      "$node_root/bin/npx" --yes "playwright@$HERMES_PLAYWRIGHT_VERSION" \
        install --with-deps chromium
    [ -n "$(find "$playwright_root" -type f -name chrome -perm /111 -print -quit)" ] \
      || die "Playwright Chromium was not installed"

    printf '%s\n' "$HERMES_COMMIT" > "$install_root/.subyard-commit"
    printf '%s\n' "$HERMES_SOURCE_SHA256" > "$install_root/.subyard-source-sha256"
    printf '%s\n' "$browser_contract" > "$browser_marker"
    chown -R "$runtime_owner:$runtime_owner" "$install_root" "$cache_root"
    chmod -R go-w "$install_root"
  )
  install_status=$?
  set -e
  if [ "$install_status" -ne 0 ]; then
    if [ -z "$previous_runtime" ]; then
      remove_partial_runtime
      rm -f -- "$transaction_marker"
      transaction_active=0
      trap - EXIT INT TERM
    fi
    die "runtime installation failed"
  fi
else
  (
    cd "$source_root"
    UV_CACHE_DIR="$cache_root" UV_PYTHON_INSTALL_DIR="$python_root" \
      UV_PROJECT_ENVIRONMENT="$venv_root" \
      "$uv_bin" sync --locked --no-dev --python "$HERMES_PYTHON_VERSION"
  )
fi

browser_executable="$(find "$playwright_root" -type f -name chrome \
  -perm /111 -print -quit 2>/dev/null)"
[ -n "$browser_executable" ] && [ -x "$browser_executable" ] \
  || die "Playwright Chromium executable is missing"

install -d -m 0700 -o "$DEV_USER" -g "$DEV_GROUP" \
  "$state_root" "$state_root/workspace"
serve_env="$state_root/.serve.env"
if [ -e "$serve_env" ]; then
  [ -f "$serve_env" ] && [ ! -L "$serve_env" ] \
    || die "$serve_env must be a regular file"
  token="$(sed -n 's/^HERMES_DASHBOARD_SESSION_TOKEN=//p' "$serve_env")"
  [ "$(wc -l < "$serve_env")" -eq 1 ] || die "invalid session-token file"
  [[ "$token" =~ ^[0-9a-f]{64}$ ]] || die "invalid session token"
else
  token="$(openssl rand -hex 32)"
  tmp_token="$(mktemp "$state_root/.serve.env.XXXXXX")"
  printf 'HERMES_DASHBOARD_SESSION_TOKEN=%s\n' "$token" > "$tmp_token"
  chown "$DEV_USER:$DEV_GROUP" "$tmp_token"
  chmod 0600 "$tmp_token"
  mv "$tmp_token" "$serve_env"
fi
chown "$DEV_USER:$DEV_GROUP" "$serve_env"
chmod 0600 "$serve_env"
unset token

install -d -m 0755 "$etc_root" "$libexec_root" "$bin_root" "$sbin_root" "$unit_root"
managed_symlink() {
  name="$1"
  target="$2"
  destination="$bin_root/$name"
  if [ -e "$destination" ] || [ -L "$destination" ]; then
    [ -L "$destination" ] \
      || die "$destination exists and is not managed by the Hermes profile"
    case "$(readlink "$destination")" in
      "$install_root"/*) ;;
      *) die "$destination points outside the Hermes runtime" ;;
    esac
  fi
  pending="$bin_root/.$name.$$"
  [ ! -e "$pending" ] && [ ! -L "$pending" ] \
    || die "stale canonical-link candidate exists"
  ln -s "$target" "$pending"
  mv -Tf -- "$pending" "$destination"
}
managed_symlink node "$node_root/bin/node"
managed_symlink npm "$node_root/bin/npm"
managed_symlink npx "$node_root/bin/npx"

agent_browser_command="$bin_root/agent-browser"
agent_browser_tmp="$(mktemp "$bin_root/.agent-browser.XXXXXX")"
{
  printf '#!/usr/bin/env bash\n'
  printf 'set -euo pipefail\n'
  printf 'export PATH=%q:"$PATH"\n' "$node_root/bin"
  printf 'export PLAYWRIGHT_BROWSERS_PATH=%q\n' "$playwright_root"
  printf 'export AGENT_BROWSER_EXECUTABLE_PATH=%q\n' "$browser_executable"
  printf 'exec %q "$@"\n' "$agent_browser"
} > "$agent_browser_tmp"
chmod 0755 "$agent_browser_tmp"
if [ "$(id -u)" -eq 0 ]; then chown 0:0 "$agent_browser_tmp"; fi
mv -fT -- "$agent_browser_tmp" "$agent_browser_command"

launcher="$bin_root/hermes"
launcher_tmp="$(mktemp "$bin_root/.hermes.XXXXXX")"
sed -e "s|@HERMES_HOME@|$state_root|g" \
  -e "s|@HERMES_ENTRYPOINT@|$venv_root/bin/hermes|g" \
  -e "s|@HERMES_NODE_BIN@|$node_root/bin|g" \
  -e "s|@HERMES_BROWSER_BIN@|$agent_browser_root/bin|g" \
  -e "s|@HERMES_PLAYWRIGHT_BROWSERS@|$playwright_root|g" \
  -e "s|@HERMES_BROWSER_EXECUTABLE@|$browser_executable|g" \
  "$profile_dir/hermes" > "$launcher_tmp"
chmod 0755 "$launcher_tmp"
if [ "$(id -u)" -eq 0 ]; then chown 0:0 "$launcher_tmp"; fi
mv -fT -- "$launcher_tmp" "$launcher"
{
  printf 'HERMES_VERSION=%q\n' "$HERMES_VERSION"
  printf 'HERMES_TAG=%q\n' "$HERMES_TAG"
  printf 'HERMES_COMMIT=%q\n' "$HERMES_COMMIT"
  printf 'HERMES_PORT=%q\n' "$HERMES_PORT"
  printf 'HERMES_HOME=%q\n' "$state_root"
  printf 'HERMES_INSTALL_ROOT=%q\n' "$install_root"
  printf 'HERMES_SOURCE=%q\n' "$source_root"
  printf 'HERMES_VENV=%q\n' "$venv_root"
  printf 'HERMES_NODE_VERSION=%q\n' "$HERMES_NODE_VERSION"
  printf 'HERMES_NPM_VERSION=%q\n' "$HERMES_NPM_VERSION"
  printf 'HERMES_AGENT_BROWSER_VERSION=%q\n' "$HERMES_AGENT_BROWSER_VERSION"
  printf 'HERMES_PLAYWRIGHT_VERSION=%q\n' "$HERMES_PLAYWRIGHT_VERSION"
  printf 'HERMES_BROWSER_RUNTIME_MARKER=%q\n' "$browser_marker"
  printf 'HERMES_BROWSER_RUNTIME_CONTRACT=%q\n' "$browser_contract"
  printf 'HERMES_NODE=%q\n' "$node_root/bin/node"
  printf 'HERMES_NPM=%q\n' "$node_root/bin/npm"
  printf 'HERMES_NPX=%q\n' "$node_root/bin/npx"
  printf 'HERMES_AGENT_BROWSER=%q\n' "$agent_browser"
  printf 'HERMES_PLAYWRIGHT_BROWSERS_PATH=%q\n' "$playwright_root"
  printf 'HERMES_BROWSER_EXECUTABLE=%q\n' "$browser_executable"
  printf 'HERMES_NODE_COMMAND=%q\n' "$bin_root/node"
  printf 'HERMES_NPM_COMMAND=%q\n' "$bin_root/npm"
  printf 'HERMES_NPX_COMMAND=%q\n' "$bin_root/npx"
  printf 'HERMES_AGENT_BROWSER_COMMAND=%q\n' "$agent_browser_command"
  printf 'HERMES_DEV_USER=%q\n' "$DEV_USER"
  printf 'HERMES_DEV_GROUP=%q\n' "$DEV_GROUP"
  printf 'HERMES_DEV_HOME=%q\n' "$DEV_HOME"
  printf 'HERMES_LAUNCHER=%q\n' "$launcher"
  printf 'HERMES_RUNTIME_OWNER=%q\n' "$(stat -c '%u:%g' "$launcher")"
  printf 'HERMES_PIN_CHECK=%q\n' "$libexec_root/subyard-hermes-pin-check"
  printf 'HERMES_VERIFY_BACKUP=%q\n' "$libexec_root/subyard-hermes-verify-backup"
} > "$runtime_env"
chmod 0644 "$runtime_env"

install -m 0755 "$profile_dir/hermes-pin-check" \
  "$libexec_root/subyard-hermes-pin-check"
install -m 0755 "$profile_dir/verify-backup.py" \
  "$libexec_root/subyard-hermes-verify-backup"
install -m 0755 "$profile_dir/hermes-provider-ready" \
  "$sbin_root/hermes-provider-ready"
install -m 0755 "$profile_dir/hermes-backup-create" \
  "$sbin_root/hermes-backup-create"
install -m 0755 "$profile_dir/hermes-backup-finalize" \
  "$sbin_root/hermes-backup-finalize"
install -m 0755 "$profile_dir/hermes-restore" \
  "$sbin_root/hermes-restore"

unit_tmp="$(mktemp)"
sed -e "s|@DEV_USER@|$DEV_USER|g" \
  -e "s|@DEV_GROUP@|$DEV_GROUP|g" \
  -e "s|@DEV_HOME@|$DEV_HOME|g" \
  -e "s|@HERMES_BROWSER_EXECUTABLE@|$browser_executable|g" \
  "$profile_dir/hermes-serve.service" > "$unit_tmp"
install -m 0644 "$unit_tmp" "$unit_root/hermes-serve.service"
rm -f -- "$unit_tmp"

if [ -e "$ready" ] && { [ ! -f "$ready" ] || [ "$(<"$ready")" != "$HERMES_COMMIT" ]; }; then
  rm -f -- "$ready"
fi

systemctl daemon-reload
if [ -r "$ready" ] && [ "$(<"$ready")" = "$HERMES_COMMIT" ]; then
  systemctl enable hermes-serve.service >/dev/null
  if [ "$transaction_active" = 1 ] && [ "$service_was_active" = 1 ]; then
    systemctl start hermes-serve.service >/dev/null
  else
    systemctl try-restart hermes-serve.service >/dev/null 2>&1 || true
  fi
else
  systemctl disable --now hermes-serve.service >/dev/null 2>&1 || true
fi

if [ "${HERMES_TEST_FAIL_AFTER_PUBLICATION:-0}" = 1 ]; then
  die "injected failure after runtime publication"
fi

if [ "$transaction_active" = 1 ]; then
  rm -f -- "$transaction_marker"
  transaction_active=0
  trap - EXIT INT TERM
  if [ -n "$previous_runtime" ] && [ -e "$previous_runtime" ]; then
    rm -rf -- "$previous_runtime"
  fi
fi

printf 'hermes provision OK: version=%s commit=%s provider_ready=%s\n' \
  "$HERMES_VERSION" "$HERMES_COMMIT" \
  "$([ -r "$ready" ] && printf yes || printf no)"
