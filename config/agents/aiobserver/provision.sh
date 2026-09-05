#!/usr/bin/env bash
# Install and reconcile the pinned AI Observer file watcher inside one enabled yard.
set -euo pipefail

IMAGE='tobilg/ai-observer:0.5.0@sha256:e2d8f8fdf5e0b55b2cbb1f0db84288eca4d8c3727f9ec569a92704c3a2ecc30f'
VERSION=0.5.0
CONTAINER=subyard-ai-observer
UNIT=subyard-ai-observer.service
OWNER_LABEL=ai-observer-v1
FILE_MARKER='# Managed by Subyard AI Observer provision.'
STATE_MARKER_VALUE=subyard-ai-observer-v1
DEV_USER="${DEV_USER:-dev}"
INTEGRATIONS="${CODING_TOOL_INTEGRATIONS:-}"
CONTEXT="${AI_OBSERVER_CONTEXT:-}"
TEST_ROOT="${AI_OBSERVER_TEST_ROOT:-}"

die() { printf 'AI Observer provision: %s\n' "$*" >&2; exit 1; }

if [ -n "$TEST_ROOT" ]; then
  case "$TEST_ROOT" in /*) ;; *) die 'AI_OBSERVER_TEST_ROOT must be absolute' ;; esac
  [ "${AI_OBSERVER_TEST_ALLOW_NON_ROOT:-0}" = 1 ] \
    || die 'AI_OBSERVER_TEST_ROOT requires AI_OBSERVER_TEST_ALLOW_NON_ROOT=1'
else
  [ "$(id -u)" -eq 0 ] || die 'must run as root'
fi

case "$DEV_USER" in ''|*[!A-Za-z0-9._-]*|-*|.|..) die 'invalid developer user' ;; esac
if [ -n "$CONTEXT" ] && [[ ! "$CONTEXT" =~ ^[0-9a-f]{64}$ ]]; then
  die 'AI_OBSERVER_CONTEXT must be a lowercase SHA-256 when set'
fi
id -u "$DEV_USER" >/dev/null 2>&1 || die "developer user '$DEV_USER' does not exist"
DEV_UID="$(id -u "$DEV_USER")"
DEV_GID="$(id -g "$DEV_USER")"
if [ -n "$TEST_ROOT" ] && [ -n "${AI_OBSERVER_TEST_DEV_HOME:-}" ]; then
  DEV_HOME="$AI_OBSERVER_TEST_DEV_HOME"
else
  DEV_HOME="$(getent passwd "$DEV_USER" | cut -d: -f6)"
fi
[ -n "$DEV_HOME" ] || die "could not resolve home for $DEV_USER"
case "$DEV_HOME" in /*) ;; *) die 'developer home must be absolute' ;; esac

root_path() { printf '%s%s' "$TEST_ROOT" "$1"; }
STATE_ROOT="$(root_path /srv/agents/ai-observer)"
DATA_DIR="$STATE_ROOT/data"
EMPTY_ROOT="$STATE_ROOT/empty"
BIN_PATH="$(root_path /usr/local/bin/ai-observer)"
CHECK_PATH="$(root_path /usr/local/bin/ai-observer-check)"
UNIT_PATH="$(root_path /etc/systemd/system/$UNIT)"
MANAGED_MARKER="$(root_path /etc/subyard/ai-observer/managed)"
EXPECTED_FILE_OWNER="$DEV_UID:$DEV_GID"
[ -z "$TEST_ROOT" ] && EXPECTED_FILE_OWNER=0:0

for command in cmp curl docker grep readlink sha256sum stat systemctl timeout; do
  command -v "$command" >/dev/null 2>&1 || die "$command is required"
done
timeout 10 docker info >/dev/null 2>&1 || die 'Docker is unavailable'

safe_bind_path() {
  case "$1" in
    *:*|*\"*|*\\*|*$'\n'*) die "unsupported character in session path: $1" ;;
  esac
}

selected() {
  local candidate
  for candidate in $INTEGRATIONS; do
    [ "$candidate" != "$1" ] || return 0
  done
  return 1
}

prepare_session_source() {
  local integration="$1" source="$2" empty="$3" resolved
  if selected "$integration"; then
    if [ -L "$source" ]; then
      resolved="$(readlink -f -- "$source")" || die "$source is a dangling symlink"
      [ -d "$resolved" ] || die "$source does not resolve to a directory"
    else
      if [ -e "$source" ] && [ ! -d "$source" ]; then
        die "$source is not a directory"
      fi
      install -d -m 0700 -o "$DEV_UID" -g "$DEV_GID" "$source"
      resolved="$(readlink -f -- "$source")"
    fi
  else
    resolved="$empty"
  fi
  safe_bind_path "$resolved"
  printf '%s' "$resolved"
}

for path in "$STATE_ROOT" "$DATA_DIR" "$EMPTY_ROOT"; do
  [ ! -L "$path" ] || die "$path must not be a symlink"
  [ ! -e "$path" ] || [ -d "$path" ] || die "$path is not a directory"
done
install -d -m 0700 -o "$DEV_UID" -g "$DEV_GID" "$STATE_ROOT" "$DATA_DIR"
install -d -m 0755 -o "${EXPECTED_FILE_OWNER%:*}" -g "${EXPECTED_FILE_OWNER#*:}" "$EMPTY_ROOT"
install -d -m 0555 -o "${EXPECTED_FILE_OWNER%:*}" -g "${EXPECTED_FILE_OWNER#*:}" \
  "$EMPTY_ROOT/claude" "$EMPTY_ROOT/codex"
chmod 0555 "$EMPTY_ROOT"
safe_bind_path "$DATA_DIR"

CLAUDE_SOURCE="$(prepare_session_source claude "$DEV_HOME/.claude/projects" "$EMPTY_ROOT/claude")"
CODEX_SOURCE="$(prepare_session_source codex "$DEV_HOME/.codex/sessions" "$EMPTY_ROOT/codex")"
BIND_DATA="$DATA_DIR:/app/data:rw"
BIND_CLAUDE="$CLAUDE_SOURCE:/sessions/claude:ro"
BIND_CODEX="$CODEX_SOURCE:/sessions/codex:ro"
EXPECTED_BINDS="[\"$BIND_DATA\",\"$BIND_CLAUDE\",\"$BIND_CODEX\"]"
EXPECTED_PORTS='{"8080/tcp":[{"HostIp":"127.0.0.1","HostPort":"8080"}]}'
SPEC="$(printf '%s\n' \
  "$IMAGE" "$CONTEXT" "$DEV_UID:$DEV_GID" "$BIND_DATA" "$BIND_CLAUDE" "$BIND_CODEX" \
  '127.0.0.1:8080:8080' 'AI_OBSERVER_CLAUDE_PATH=/sessions/claude' \
  'AI_OBSERVER_CODEX_PATH=/sessions/codex' \
  'AI_OBSERVER_DATABASE_PATH=/app/data/ai-observer.duckdb' \
  'AI_OBSERVER_API_PORT=8080' 'watch all --backfill' | sha256sum | cut -d' ' -f1)"

container_exists() { timeout 10 docker container inspect "$CONTAINER" >/dev/null 2>&1; }
container_value() { timeout 10 docker inspect -f "$1" "$CONTAINER" 2>/dev/null; }
container_is_owned() {
  [ "$(container_value '{{ index .Config.Labels "org.subyard.managed" }}')" = "$OWNER_LABEL" ]
}
container_is_expected_candidate() {
  container_is_owned &&
    [ "$(container_value '{{ index .Config.Labels "org.subyard.ai-observer.spec" }}')" = "$SPEC" ]
}
container_matches() {
  local env
  container_is_owned || return 1
  [ "$(container_value '{{ index .Config.Labels "org.subyard.ai-observer.spec" }}')" = "$SPEC" ] || return 1
  [ "$(container_value '{{.Config.Image}}')" = "$IMAGE" ] || return 1
  [ "$(container_value '{{.Config.User}}')" = "$DEV_UID:$DEV_GID" ] || return 1
  [ "$(container_value '{{json .Config.Cmd}}')" = '["watch","all","--backfill"]' ] || return 1
  [ "$(container_value '{{json .HostConfig.Binds}}')" = "$EXPECTED_BINDS" ] || return 1
  [ "$(container_value '{{json .HostConfig.PortBindings}}')" = "$EXPECTED_PORTS" ] || return 1
  [ "$(container_value '{{.HostConfig.RestartPolicy.Name}}')" = no ] || return 1
  env="$(container_value '{{range .Config.Env}}{{println .}}{{end}}')" || return 1
  grep -Fxq 'AI_OBSERVER_CLAUDE_PATH=/sessions/claude' <<<"$env" || return 1
  grep -Fxq 'AI_OBSERVER_CODEX_PATH=/sessions/codex' <<<"$env" || return 1
  grep -Fxq 'AI_OBSERVER_DATABASE_PATH=/app/data/ai-observer.duckdb' <<<"$env" || return 1
  grep -Fxq 'AI_OBSERVER_API_PORT=8080' <<<"$env"
}

if container_exists && ! container_is_owned; then
  die "container '$CONTAINER' is not managed by Subyard; leaving it unchanged"
fi

validate_managed_file() {
  local path="$1" marker="$2"
  if [ -e "$path" ] || [ -L "$path" ]; then
    [ -f "$path" ] && [ ! -L "$path" ] \
      || die "$path is not a managed regular file; leaving it unchanged"
    [ "$(stat -c '%u:%g' "$path")" = "$EXPECTED_FILE_OWNER" ] \
      || die "$path ownership is not managed by Subyard; leaving it unchanged"
    grep -Fxq "$marker" "$path" \
      || die "$path is managed by another install; leaving it unchanged"
  fi
}
validate_managed_file "$BIN_PATH" "$FILE_MARKER"
validate_managed_file "$CHECK_PATH" "$FILE_MARKER"
validate_managed_file "$UNIT_PATH" "$FILE_MARKER"
if [ -e "$MANAGED_MARKER" ] || [ -L "$MANAGED_MARKER" ]; then
  validate_managed_file "$MANAGED_MARKER" "$STATE_MARKER_VALUE"
fi

temporary="$(mktemp -d /tmp/subyard-ai-observer.XXXXXX)"
cleanup() { find "$temporary" -depth -delete; }
trap cleanup EXIT HUP INT TERM

printf -v q_image '%q' "$IMAGE"
printf -v q_version '%q' "$VERSION"
printf -v q_container '%q' "$CONTAINER"
printf -v q_unit '%q' "$UNIT"
printf -v q_owner_label '%q' "$OWNER_LABEL"
printf -v q_spec '%q' "$SPEC"
printf -v q_user '%q' "$DEV_UID:$DEV_GID"
printf -v q_binds '%q' "$EXPECTED_BINDS"
printf -v q_ports '%q' "$EXPECTED_PORTS"
printf -v q_marker '%q' "$MANAGED_MARKER"
printf -v q_unit_path '%q' "$UNIT_PATH"
printf -v q_check_path '%q' "$CHECK_PATH"
printf -v q_file_owner '%q' "$EXPECTED_FILE_OWNER"
printf -v q_test_mode '%q' "$([ -n "$TEST_ROOT" ] && printf 1 || printf 0)"

cat >"$temporary/ai-observer" <<EOF
#!/usr/bin/env bash
$FILE_MARKER
set -euo pipefail
image=$q_image
version=$q_version
container=$q_container
unit=$q_unit
owner_label=$q_owner_label
managed_marker=$q_marker
unit_path=$q_unit_path
check_path=$q_check_path
expected_file_owner=$q_file_owner
test_mode=$q_test_mode
die() { printf 'ai-observer: %s\\n' "\$*" >&2; exit 1; }
container_value() { timeout 10 docker inspect -f "\$1" "\$container" 2>/dev/null; }
container_exists() { timeout 10 docker container inspect "\$container" >/dev/null 2>&1; }
container_owned() {
  [ "\$(container_value '{{ index .Config.Labels "org.subyard.managed" }}')" = "\$owner_label" ]
}
verify_control_files() {
  [ -f "\${BASH_SOURCE[0]}" ] && [ ! -L "\${BASH_SOURCE[0]}" ] \
    || die 'managed wrapper is not a regular file'
  [ "\$(stat -c '%u:%g' "\${BASH_SOURCE[0]}")" = "\$expected_file_owner" ] \
    || die 'managed wrapper ownership drifted'
  grep -Fxq '$FILE_MARKER' "\${BASH_SOURCE[0]}" || die 'managed wrapper marker is missing'
  [ -f "\$managed_marker" ] && [ ! -L "\$managed_marker" ] \
    || die 'managed state marker is missing'
  [ "\$(stat -c '%u:%g' "\$managed_marker")" = "\$expected_file_owner" ] \
    || die 'managed state marker ownership drifted'
  grep -Fxq '$STATE_MARKER_VALUE' "\$managed_marker" || die 'managed state marker drifted'
  [ -f "\$unit_path" ] && [ ! -L "\$unit_path" ] \
    || die 'managed unit is missing'
  [ "\$(stat -c '%u:%g' "\$unit_path")" = "\$expected_file_owner" ] \
    || die 'managed unit ownership drifted'
  grep -Fxq '$FILE_MARKER' "\$unit_path" || die 'managed unit marker is missing'
  [ -f "\$check_path" ] && [ ! -L "\$check_path" ] \
    || die 'managed readiness check is missing'
  [ "\$(stat -c '%u:%g' "\$check_path")" = "\$expected_file_owner" ] \
    || die 'managed readiness check ownership drifted'
  grep -Fxq '$FILE_MARKER' "\$check_path" || die 'managed readiness check marker is missing'
}
verify_owned_container() {
  container_exists || die 'managed container is missing'
  container_owned || die 'same-name container is not managed by Subyard'
}
case "\${1:-}" in
  run)
    verify_control_files
    verify_owned_container
    exec docker start --attach "\$container"
    ;;
  stop)
    verify_control_files
    verify_owned_container
    [ "\$(container_value '{{.State.Running}}')" != true ] \
      || timeout 20 docker stop --time 10 "\$container" >/dev/null
    ;;
  disable)
    [ "\$(id -u)" -eq 0 ] || [ "\$test_mode" = 1 ] || die 'disable must run as root'
    verify_control_files
    if container_exists; then container_owned || die 'refusing to disable a foreign same-name container'; fi
    timeout 30 systemctl disable --now "\$unit"
    ;;
  status)
    verify_control_files
    verify_owned_container
    "\$check_path"
    printf 'dashboard http://127.0.0.1:8080/\\n'
    ;;
  logs)
    shift
    verify_control_files
    verify_owned_container
    exec docker logs "\$@" "\$container"
    ;;
  --version)
    verify_control_files
    verify_owned_container
    if [ "\$(container_value '{{.State.Running}}')" = true ]; then
      exec timeout 10 docker exec "\$container" /app/ai-observer --version
    fi
    printf 'ai-observer %s\\n' "\$version"
    ;;
  *)
    printf 'Usage: ai-observer {status|logs|disable|--version}\\n' >&2
    exit 2
    ;;
esac
EOF

cat >"$temporary/ai-observer-check" <<EOF
#!/usr/bin/env bash
$FILE_MARKER
set -euo pipefail
image=$q_image
container=$q_container
unit=$q_unit
owner_label=$q_owner_label
spec=$q_spec
expected_user=$q_user
expected_binds=$q_binds
expected_ports=$q_ports
test_mode=$q_test_mode
die() { printf 'ai-observer-check: %s\\n' "\$*" >&2; exit 1; }
check_timeout=20
if [ "\$test_mode" = 1 ]; then check_timeout="\${AI_OBSERVER_CHECK_TIMEOUT_SECONDS:-20}"; fi
case "\$check_timeout" in ''|*[!0-9]*|0) die 'invalid readiness timeout' ;; esac
if [ "\${AI_OBSERVER_CHECK_INNER:-0}" != 1 ]; then
  exec timeout --foreground "\$check_timeout" env AI_OBSERVER_CHECK_INNER=1 "\$0"
fi
container_value() { timeout 10 docker inspect -f "\$1" "\$container" 2>/dev/null; }
timeout 10 systemctl is-enabled --quiet "\$unit" || die 'unit is not enabled'
timeout 10 systemctl is-active --quiet "\$unit" || die 'unit is not active'
timeout 10 docker container inspect "\$container" >/dev/null 2>&1 || die 'managed container is missing'
[ "\$(container_value '{{ index .Config.Labels "org.subyard.managed" }}')" = "\$owner_label" ] \
  || die 'container ownership marker drifted'
[ "\$(container_value '{{ index .Config.Labels "org.subyard.ai-observer.spec" }}')" = "\$spec" ] \
  || die 'container specification drifted'
[ "\$(container_value '{{.Config.Image}}')" = "\$image" ] || die 'container image drifted'
[ "\$(container_value '{{.Config.User}}')" = "\$expected_user" ] || die 'container user drifted'
[ "\$(container_value '{{json .Config.Cmd}}')" = '["watch","all","--backfill"]' ] \
  || die 'container command drifted'
[ "\$(container_value '{{json .HostConfig.Binds}}')" = "\$expected_binds" ] \
  || die 'container mounts drifted'
[ "\$(container_value '{{json .HostConfig.PortBindings}}')" = "\$expected_ports" ] \
  || die 'container port binding drifted'
[ "\$(container_value '{{.HostConfig.RestartPolicy.Name}}')" = no ] \
  || die 'container restart ownership drifted'
env="\$(container_value '{{range .Config.Env}}{{println .}}{{end}}')" || die 'container environment unavailable'
grep -Fxq 'AI_OBSERVER_CLAUDE_PATH=/sessions/claude' <<<"\$env" || die 'Claude session path drifted'
grep -Fxq 'AI_OBSERVER_CODEX_PATH=/sessions/codex' <<<"\$env" || die 'Codex session path drifted'
grep -Fxq 'AI_OBSERVER_DATABASE_PATH=/app/data/ai-observer.duckdb' <<<"\$env" \
  || die 'database path drifted'
grep -Fxq 'AI_OBSERVER_API_PORT=8080' <<<"\$env" || die 'API port drifted'
[ "\$(container_value '{{.State.Running}}')" = true ] || die 'container is not running'
curl --fail --silent --show-error --connect-timeout 2 --max-time 5 \
  http://127.0.0.1:8080/health >/dev/null || die 'HTTP readiness failed'
printf 'ai-observer %s ready\\n' $q_version
EOF

cat >"$temporary/$UNIT" <<EOF
$FILE_MARKER
[Unit]
Description=Subyard AI Observer
Wants=network-online.target
After=network-online.target docker.service
Requires=docker.service

[Service]
Type=simple
ExecStart=$BIN_PATH run
ExecStop=$BIN_PATH stop
Restart=on-failure
RestartSec=5s
TimeoutStartSec=60s
TimeoutStopSec=30s

[Install]
WantedBy=multi-user.target
EOF
printf '%s\n' "$STATE_MARKER_VALUE" >"$temporary/managed"
chmod 0755 "$temporary/ai-observer" "$temporary/ai-observer-check"
chmod 0644 "$temporary/$UNIT" "$temporary/managed"

files_match=1
cmp -s "$temporary/ai-observer" "$BIN_PATH" || files_match=0
cmp -s "$temporary/ai-observer-check" "$CHECK_PATH" || files_match=0
cmp -s "$temporary/$UNIT" "$UNIT_PATH" || files_match=0
cmp -s "$temporary/managed" "$MANAGED_MARKER" || files_match=0

if container_matches && [ "$files_match" = 1 ] &&
  timeout 10 systemctl is-enabled --quiet "$UNIT" &&
  timeout 10 systemctl is-active --quiet "$UNIT" &&
  "$CHECK_PATH" >/dev/null; then
  printf 'AI Observer %s is already ready.\n' "$VERSION"
  exit 0
fi

if ! timeout 10 docker image inspect "$IMAGE" >/dev/null 2>&1; then
  timeout 600 docker pull "$IMAGE" >/dev/null \
    || die "could not pull pinned image $IMAGE"
fi
timeout 10 docker image inspect "$IMAGE" >/dev/null 2>&1 \
  || die "pinned image $IMAGE is unavailable after pull"

backup_root="$temporary/rollback"
mkdir "$backup_root"
backup_file() {
  local path="$1" name="$2"
  if [ -e "$path" ]; then cp -a -- "$path" "$backup_root/$name"; printf 1; else printf 0; fi
}
bin_existed="$(backup_file "$BIN_PATH" bin)"
check_existed="$(backup_file "$CHECK_PATH" check)"
unit_existed="$(backup_file "$UNIT_PATH" unit)"
marker_existed="$(backup_file "$MANAGED_MARKER" marker)"
was_enabled=0; was_active=0
timeout 10 systemctl is-enabled --quiet "$UNIT" && was_enabled=1 || true
timeout 10 systemctl is-active --quiet "$UNIT" && was_active=1 || true
old_container=0
container_exists && old_container=1
rollback_container=''
candidate_created=0

restore_file() {
  local path="$1" name="$2" existed="$3"
  if [ "$existed" = 1 ]; then
    install -d -m 0755 "$(dirname "$path")"
    cp -a -- "$backup_root/$name" "$path"
  else
    rm -f -- "$path"
  fi
}

rollback() {
  timeout 30 systemctl stop "$UNIT" >/dev/null 2>&1 || true
  if [ "$candidate_created" = 1 ] && container_exists && container_is_expected_candidate; then
    timeout 20 docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
  fi
  if [ -n "$rollback_container" ] &&
    timeout 10 docker container inspect "$rollback_container" >/dev/null 2>&1; then
    timeout 10 docker rename "$rollback_container" "$CONTAINER" >/dev/null 2>&1 || true
  fi
  restore_file "$BIN_PATH" bin "$bin_existed"
  restore_file "$CHECK_PATH" check "$check_existed"
  restore_file "$UNIT_PATH" unit "$unit_existed"
  restore_file "$MANAGED_MARKER" marker "$marker_existed"
  timeout 10 systemctl daemon-reload >/dev/null 2>&1 || true
  if [ "$was_enabled" = 1 ]; then
    timeout 10 systemctl enable "$UNIT" >/dev/null 2>&1 || true
  else
    timeout 10 systemctl disable "$UNIT" >/dev/null 2>&1 || true
  fi
  if [ "$was_active" = 1 ] && [ "$old_container" = 1 ]; then
    timeout 30 systemctl start "$UNIT" >/dev/null 2>&1 || true
  fi
}

install_atomic() {
  local source="$1" destination="$2" mode="$3" stage
  install -d -m 0755 "$(dirname "$destination")"
  stage="$(mktemp "$(dirname "$destination")/.ai-observer.XXXXXX")"
  install -m "$mode" -o "${EXPECTED_FILE_OWNER%:*}" -g "${EXPECTED_FILE_OWNER#*:}" \
    "$source" "$stage"
  mv -fT -- "$stage" "$destination"
}

install_atomic "$temporary/ai-observer" "$BIN_PATH" 0755
install_atomic "$temporary/ai-observer-check" "$CHECK_PATH" 0755
install_atomic "$temporary/$UNIT" "$UNIT_PATH" 0644
install_atomic "$temporary/managed" "$MANAGED_MARKER" 0644

container_changed=0
if ! container_matches; then
  container_changed=1
  if [ "$old_container" = 1 ]; then
    timeout 30 systemctl stop "$UNIT" >/dev/null 2>&1 || true
    if [ "$(container_value '{{.State.Running}}')" = true ]; then
      timeout 20 docker stop --time 10 "$CONTAINER" >/dev/null \
        || { rollback; die 'could not stop existing managed container'; }
    fi
    rollback_container="$CONTAINER.rollback.$$"
    if timeout 10 docker container inspect "$rollback_container" >/dev/null 2>&1; then
      rollback
      die "temporary rollback container '$rollback_container' already exists"
    fi
    timeout 10 docker rename "$CONTAINER" "$rollback_container" \
      || { rollback; die 'could not stage existing managed container'; }
  fi
  if ! timeout 60 docker create \
    --name "$CONTAINER" \
    --label "org.subyard.managed=$OWNER_LABEL" \
    --label "org.subyard.ai-observer.spec=$SPEC" \
    --user "$DEV_UID:$DEV_GID" \
    --restart no \
    --publish 127.0.0.1:8080:8080 \
    --volume "$BIND_DATA" \
    --volume "$BIND_CLAUDE" \
    --volume "$BIND_CODEX" \
    --env AI_OBSERVER_CLAUDE_PATH=/sessions/claude \
    --env AI_OBSERVER_CODEX_PATH=/sessions/codex \
    --env AI_OBSERVER_DATABASE_PATH=/app/data/ai-observer.duckdb \
    --env AI_OBSERVER_API_PORT=8080 \
    "$IMAGE" watch all --backfill >/dev/null; then
    if container_exists && container_is_expected_candidate; then
      candidate_created=1
    fi
    rollback
    die 'could not create the managed container; previous runtime restored'
  fi
  candidate_created=1
fi

timeout 10 systemctl daemon-reload || { rollback; die 'systemd reload failed; previous runtime restored'; }
timeout 10 systemctl enable "$UNIT" || { rollback; die 'could not enable unit; previous runtime restored'; }
if [ "$was_active" = 0 ] && [ "$container_changed" = 0 ] &&
  [ "$(container_value '{{.State.Running}}')" = true ]; then
  timeout 20 docker stop --time 10 "$CONTAINER" >/dev/null \
    || { rollback; die 'could not recover container under systemd ownership'; }
fi
if [ "$was_active" = 1 ] && [ "$container_changed" = 0 ]; then
  timeout 60 systemctl restart "$UNIT" \
    || { rollback; die 'unit restart failed; previous runtime restored'; }
else
  timeout 60 systemctl start "$UNIT" \
    || { rollback; die 'unit start failed; previous runtime restored'; }
fi

ready=0
for _ in $(seq 1 30); do
  if "$CHECK_PATH" >/dev/null 2>&1; then ready=1; break; fi
  sleep 1
done
if [ "$ready" != 1 ]; then
  rollback
  die 'readiness failed; previous runtime restored'
fi

if [ -n "$rollback_container" ]; then
  timeout 20 docker rm "$rollback_container" >/dev/null \
    || { rollback; die 'could not retire previous managed container'; }
fi
printf 'AI Observer %s installed and ready.\n' "$VERSION"
