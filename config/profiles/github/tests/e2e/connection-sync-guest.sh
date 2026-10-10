#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd -P)"
# Lease SSH starts in root's home; runuser preserves that inaccessible cwd.
cd "$ROOT"
MODE="${1:?fixture phase is required}"
case "${SUBYARD_E2E_VM:-}" in 1|2) ;; *) printf 'allocated VM required\n' >&2; exit 2 ;; esac
[[ "${SUBYARD_E2E_RUN_ID:-}" =~ ^[a-zA-Z0-9_-]+$ ]] || { printf 'safe lease identity required\n' >&2; exit 2; }
STATE="/tmp/subyard-github-connection-e2e-$SUBYARD_E2E_RUN_ID"
[ "$MODE" != cleanup ] || [ -e "$STATE" ] || exit 0
MARKER="$SUBYARD_E2E_RUN_ID"
PLATFORM="$HOME/.cache/subyard-github-connection-platform-$MARKER"
# shellcheck source=dev/e2e/lib-p0-capacity.sh
. "$ROOT/dev/e2e/lib-p0-capacity.sh"
YARD=connection-e2e
PROJECT=subyard-connection-e2e
INSTANCE=yard-connection-e2e
SSH_SERVICE=subyard-github-connection-ssh-e2e.service
AUTO_SERVICE=subyard-github-connection-auto-e2e.service
BROKER_SERVICE=subyard-github-connection-e2e.service
RELEASE="$ROOT/.build/github-connection-release"
fail() { printf 'github-connection-e2e: %s\n' "$*" >&2; exit 1; }
owned() { [ -f "$STATE/.marker" ] && [ "$(cat "$STATE/.marker")" = "$MARKER" ] || fail 'fixture ownership differs'; }
systemctl_user() {
  XDG_RUNTIME_DIR="/run/user/$(id -u)" DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$(id -u)/bus" systemctl --user "$@"
}
incus() { sudo -n /usr/bin/incus "$@"; }
context() {
  owned
  [ -d "$PLATFORM" ] && [ ! -L "$PLATFORM" ] && [ "$(cat "$PLATFORM/.marker" 2>/dev/null)" = "$MARKER" ] \
    || fail 'native platform cache ownership differs'
  export SUBYARD_OPERATOR_HOME="$HOME" SUBYARD_CONFIG_HOME="$STATE/config" SUBYARD_HOME="$STATE/data"
  export SUBYARD_KEYS_ROOT="$STATE/config/keys" SUBYARD_KEYS_TOOLS_DIR="$STATE/config/tools"
  export SUBYARD_KEYS_CONSUMER_ROOT="$STATE/config/generated" SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1
  export SUBYARD_NO_AUDIT=1 MIN_DISK_GIB=1 TMPDIR=/tmp
  export HOST_CLAUDE_MD='' HOST_CODEX_AGENTS_MD='' HOST_OPENCODE_AGENTS_MD=''
  export STORAGE_PATH
  STORAGE_PATH="$(incus storage get default source 2>/dev/null || true)"
  [ -n "$STORAGE_PATH" ] || STORAGE_PATH="$PLATFORM/incus/storage"
  p0_capacity_require_persistent_path "$PLATFORM" native-incus-parent >/dev/null
  export PATH="$STATE/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
  unset YARD_ENGINE_PATH SUBYARD_REPOSITORY_ROOT ENVIRONMENT_PROFILES
  CONNECTION="$SUBYARD_KEYS_CONSUMER_ROOT/github/connection.json"
  LEGACY="$SUBYARD_KEYS_CONSUMER_ROOT/github/github-app.pem"
}
yard() {
  if ! id -nG | tr ' ' '\n' | grep -Fxq incus-admin && id -nG "$(id -un)" | tr ' ' '\n' | grep -Fxq incus-admin; then
    local command
    printf -v command '%q ' "$STATE/bin/yard" -Y "$YARD" "$@"
    sg incus-admin -c "$command"
  else
    "$STATE/bin/yard" -Y "$YARD" "$@"
  fi
}
storage_diagnostics() {
  df -h -- "$STATE" "$ROOT" "$PLATFORM"
  df -i -- "$STATE" "$ROOT" "$PLATFORM"
  findmnt -n -o FSTYPE,SOURCE,SIZE,AVAIL -T "$STATE"
  findmnt -n -o FSTYPE,SOURCE,SIZE,AVAIL -T "$PLATFORM"
  timeout --foreground 15 du -sk -- "$STATE" "$ROOT" "$RELEASE" "$PLATFORM" || true
}
init() {
  local rc
  if incus project show "$PROJECT" >/dev/null 2>&1; then
    [ "$(incus project get "$PROJECT" user.subyard.e2e)" = "$MARKER" ] || fail 'fixture project ownership differs'
  else
    incus project create "$PROJECT" -c "user.subyard.e2e=$MARKER" >/dev/null
    # New instances inherit this marker even if ordinary init stops midway.
    incus profile set default "user.subyard.e2e=$MARKER" --project "$PROJECT"
  fi
  storage_diagnostics
  if yard init --yes; then :; else
    rc=$?
    storage_diagnostics >&2 || true
    return "$rc"
  fi
  incus config set "$INSTANCE" user.subyard.e2e "$MARKER" --project "$PROJECT"
}
status() {
  incus exec "$INSTANCE" --project "$PROJECT" -- runuser -u dev -- /usr/local/bin/subyard-github status 2>/dev/null | tr -d '\n'
}
wait_status() {
  local expected="$1" _
  for _ in $(seq 1 120); do
    [ "$(status || true)" = "$expected" ] && return 0
    sleep 1
  done
  fail "broker status did not become $expected"
}
new_key() {
  openssl genrsa -out "$STATE/app.pem" 2048 >/dev/null 2>&1
  chmod 0600 "$STATE/app.pem"
  openssl rsa -in "$STATE/app.pem" -pubout -out "$STATE/app-public.pem" >/dev/null 2>&1
}
credential() { yard keys list | awk -F '\t' '$8=="github-app" {print $1}'; }
encrypted() {
  if rg -q -- 'BEGIN (RSA )?PRIVATE KEY|"app_id"|"installation_id"' "$SUBYARD_KEYS_ROOT/shared" "$SUBYARD_KEYS_ROOT/local"; then
    fail 'connection plaintext entered the encrypted ledger'
  fi
}
issuer() {
  SUBYARD_GITHUB_CONNECTION_FIXTURE="$CONNECTION" SUBYARD_GITHUB_FIXTURE_PUBLIC_KEY="$STATE/app-public.pem" \
    SUBYARD_GITHUB_FIXTURE_APP="$1" SUBYARD_GITHUB_FIXTURE_INSTALLATION="$2" \
    "$RELEASE/connection-fixture.test" -test.run '^TestConnectionFixtureIssuer$' -test.v
}
case "$MODE" in
  package)
    [ "$SUBYARD_E2E_VM" = 1 ] || fail 'package only on VM1'
    [ ! -e "$RELEASE" ] || fail 'release output already exists'
    "$ROOT/dev/package-engine.sh" --output-dir "$RELEASE" --version github-connection-e2e --arch amd64 >/dev/null
    go -C "$ROOT" test -c -o "$RELEASE/connection-fixture.test" ./config/profiles/github/broker
    exit ;;
  prepare)
    [ ! -e "$STATE" ] || fail 'fixture state already exists'
    install -d -m 0711 "$STATE"
    printf '%s\n' "$MARKER" >"$STATE/.marker"; chmod 0600 "$STATE/.marker"
    install -d -m 0700 "$STATE/config/yards/$YARD" "$STATE/data" "$STATE/bin" "$STATE/installer-home"
    [ ! -L "$HOME/.cache" ] && [ ! -e "$PLATFORM" ] && [ ! -L "$PLATFORM" ] \
      || fail 'native platform cache must be a new plain fixture path'
    install -d -m 0711 "$PLATFORM"
    printf '%s\n' "$MARKER" >"$PLATFORM/.marker.tmp"
    chmod 0600 "$PLATFORM/.marker.tmp"
    mv "$PLATFORM/.marker.tmp" "$PLATFORM/.marker"
    # The supported installer activates an immutable current-worktree runtime.
    HOME="$STATE/installer-home" SUBYARD_HOME="$STATE/data" SUBYARD_CONFIG_HOME="$STATE/config" \
      YARD_BIN_DIR="$STATE/bin" YARD_SHELL_RC="$STATE/installer-home/.bashrc" YARD_LOGIN_RC="$STATE/installer-home/.profile" \
      YARD_RELEASE_BASE_URL="file://$RELEASE" YARD_RELEASE_VERSION=github-connection-e2e \
      "$RELEASE/subyard-install.sh" --yes >/dev/null
    context
    runtime="$(readlink -f "$STATE/bin/yard")"
    case "$runtime" in "$STATE/data/runtime/releases/github-connection-e2e-"*/bin/yard) ;; *) fail 'installed immutable runtime is missing' ;; esac
    (cd "${runtime%/bin/yard}" && sha256sum -c runtime-files.sha256 >/dev/null)
    printf 'ENVIRONMENT_PROFILES=github\nCODING_TOOL_INTEGRATIONS=\nHOST_BASE=%s/host\nRESTRICTED_DISK_PATHS=%s/host\nHOST_MOUNTS=\nHOST_LINKS=\nFORWARD_SSH_AGENT=0\n' "$STATE" "$STATE" >"$SUBYARD_CONFIG_HOME/config.env"
    chmod 0600 "$SUBYARD_CONFIG_HOME/config.env"
    printf 'INCUS_PROJECT=%s\nYARD_INSTANCE_NAME=%s\nSSH_PORT=39222\n' "$PROJECT" "$INSTANCE" >"$SUBYARD_CONFIG_HOME/yards/$YARD/config.env"
    chmod 0600 "$SUBYARD_CONFIG_HOME/yards/$YARD/config.env"
    named_port="$(yard config show SSH_PORT | sed -n 's/^effective: //p')"
    default_port="$("$STATE/bin/yard" config show SSH_PORT | sed -n 's/^effective: //p')"
    [ "$named_port" = 39222 ] && [ "$default_port" != "$named_port" ] \
      || fail 'fixture SSH port must belong only to its named yard'
    for unit in "$BROKER_SERVICE" subyard-keys-sync.service subyard-keys-sync.timer "$AUTO_SERVICE"; do
      [ ! -e "$HOME/.config/systemd/user/$unit" ] || fail 'user service fixture would overlap an existing unit'
    done
    [ ! -e "/run/systemd/system/$SSH_SERVICE" ] || fail 'SSH fixture unit exists'
    ssh-keygen -q -t ed25519 -N '' -f "$STATE/client-key"
    ssh-keygen -q -t ed25519 -N '' -f "$STATE/server-key"
    cat >"$STATE/bin/bash" <<'SH'
#!/bin/sh
if [ "${1:-}" = -lc ]; then shift; exec /bin/bash -c "${1:-}"; fi
exec /bin/bash "$@"
SH
    chmod 0755 "$STATE/bin/bash"
    printf '#!/bin/sh\nexec /usr/bin/ssh -F %s/ssh-config "$@"\n' "$STATE" >"$STATE/bin/ssh"
    chmod 0755 "$STATE/bin/ssh"
    ;;
  *) context ;;
esac
case "$MODE" in
  prepare) printf 'ok: VM%s installed current-worktree runtime\n' "$SUBYARD_E2E_VM" ;;
  public) cat "$STATE/client-key.pub" "$STATE/server-key.pub" ;;
  authorize)
    address="${2:?peer address required}"
    [[ "$address" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail 'invalid peer address'
    read -r peer_identity <"$STATE/peer-public"
    peer_host="$(sed -n '2p' "$STATE/peer-public")"
    printf 'restrict,command="%s" %s\n' "$STATE/remote-command" "$peer_identity" >"$STATE/authorized"
    printf 'peer %s\n' "$peer_host" >"$STATE/known-hosts"
    printf '#!/bin/bash\nset -euo pipefail\nexport SUBYARD_E2E_VM=%q SUBYARD_E2E_RUN_ID=%q\nexec bash %q remote\n' \
      "$SUBYARD_E2E_VM" "$SUBYARD_E2E_RUN_ID" "$ROOT/config/profiles/github/tests/e2e/connection-sync-guest.sh" >"$STATE/remote-command"
    chmod 0700 "$STATE/remote-command"
    cat >"$STATE/ssh-config" <<EOF
Host peer
    HostName $address
    User dev
    Port 19423
    IdentityFile $STATE/client-key
    IdentitiesOnly yes
    BatchMode yes
    HostKeyAlias peer
    StrictHostKeyChecking yes
    UserKnownHostsFile $STATE/known-hosts
    GlobalKnownHostsFile /dev/null
    LogLevel ERROR
EOF
    printf 'ACCESS_KIND=remote\nOWNER_ENDPOINT=peer\nOWNER_YARD_NAME=%s\n' "$YARD" >"$SUBYARD_CONFIG_HOME/yards/peer.env"
    chmod 0600 "$STATE/authorized" "$STATE/known-hosts" "$STATE/ssh-config" "$SUBYARD_CONFIG_HOME/yards/peer.env"
    cat >"$STATE/sshd-config" <<EOF
Port 19423
ListenAddress 0.0.0.0
HostKey $STATE/server-key
AuthorizedKeysFile $STATE/authorized
StrictModes no
PasswordAuthentication no
KbdInteractiveAuthentication no
UsePAM yes
AllowUsers dev
AllowTcpForwarding no
AllowAgentForwarding no
PermitTTY no
LogLevel ERROR
EOF
    sudo -n /usr/sbin/sshd -t -f "$STATE/sshd-config"
    printf '# GitHub connection fixture %s\n[Unit]\nDescription=Disposable GitHub connection SSH peer\n[Service]\nExecStart=/usr/sbin/sshd -D -e -f %s/sshd-config\n' "$MARKER" "$STATE" >"$STATE/ssh.service"
    sudo -n install -m 0644 "$STATE/ssh.service" "/run/systemd/system/$SSH_SERVICE"
    sudo -n systemctl daemon-reload
    sudo -n systemctl start "$SSH_SERVICE"
    ;;
  remote) exec /bin/bash -c "${SSH_ORIGINAL_COMMAND:?SSH command required}" ;;
  migrate)
    # First establish the owner through the supported init, then seed an old
    # PEM-only ledger revision before testing the second init's migration.
    init
    new_key
    install -d -m 0700 "$(dirname "$LEGACY")"
    install -m 0600 "$STATE/app.pem" "$LEGACY"
    printf '{"app_id":"123456","installation_id":42}\n' >"$SUBYARD_CONFIG_HOME/github-app.json"
    chmod 0600 "$SUBYARD_CONFIG_HOME/github-app.json"
    yard keys import "$STATE/app.pem" --label github-app --consumer github-app-key --yes >/dev/null
    original_credential="$(credential)"
    [ -n "$original_credential" ] || fail 'legacy PEM-only ledger revision is missing'
    cmp -s "$STATE/app.pem" "$LEGACY" || fail 'import changed the protected legacy PEM'
    [ ! -e "$CONNECTION" ] || fail 'ordinary import unexpectedly materialized the connection'
    init
    yard start --yes
    wait_status '{"configured":true}'
    [ "$(credential)" = "$original_credential" ] || fail 'migration did not preserve the existing credential'
    jq -e '.use_credential_settings == true and length == 1' "$SUBYARD_CONFIG_HOME/github-app.json" >/dev/null \
      || fail 'legacy local settings were not delegated to the coherent connection'
    issuer 123456 42
    encrypted
    ;;
  init) init; yard start --yes; wait_status '{"configured":false}' ;;
  sync) yard keys trust @peer --yes >/dev/null; yard keys sync @peer --now --yes >/dev/null; encrypted ;;
  repeat)
    before="$(sha256sum "$CONNECTION" | cut -d' ' -f1)"
    ledger_before="$(git -C "$SUBYARD_KEYS_ROOT/shared" rev-parse HEAD)"
    init
    [ "$(sha256sum "$CONNECTION" | cut -d' ' -f1)" = "$before" ] || fail 'repeat init changed the coherent connection'
    [ "$(git -C "$SUBYARD_KEYS_ROOT/shared" rev-parse HEAD)" = "$ledger_before" ] || fail 'repeat init created a migration revision'
    ;;
  verify)
    app="${2:?app required}"; installation="${3:?installation required}"
    [ ! -e "$SUBYARD_CONFIG_HOME/github-app.json" ] || fail 'receiver unexpectedly needs local ID input'
    [ "$(stat -c %a "$CONNECTION")" = 600 ] || fail 'connection bundle is not protected'
    wait_status '{"configured":true}'
    issuer "$app" "$installation"
    incus exec "$INSTANCE" --project "$PROJECT" -- test ! -e "$CONNECTION" || fail 'connection entered the yard'
    incus exec "$INSTANCE" --project "$PROJECT" -- test ! -e "$SUBYARD_CONFIG_HOME/github-app.json" || fail 'App config entered the yard'
    encrypted
    ;;
  rotate) new_key; yard keys rotate "$(credential)" --file "$STATE/app.pem" --yes >/dev/null ;;
  update)
    jq -n --rawfile key "$STATE/app.pem" '{schema_version:1,settings:{app_id:"654321",installation_id:84},private_key:$key}' >"$STATE/update.json"
    chmod 0600 "$STATE/update.json"
    yard keys rotate "$(credential)" --file "$STATE/update.json" --yes >/dev/null
    yard keys materialize global --yes >/dev/null
    wait_status '{"configured":true}'
    issuer 654321 84
    ;;
  auto-sync)
    due="$SUBYARD_KEYS_ROOT/state/peer.json"
    jq '.lastAttempt=0 | .lastSuccess=0 | .nextRetry=0' "$due" >"$STATE/due.json"
    chmod 0600 "$STATE/due.json"; mv -f "$STATE/due.json" "$due"
    service="$HOME/.config/systemd/user/$AUTO_SERVICE"
    cp "$HOME/.config/systemd/user/subyard-keys-sync.service" "$service"
    {
      printf '\n# GitHub connection fixture %s\n[Service]\nPrivateTmp=false\n' "$MARKER"
      for variable in SUBYARD_OPERATOR_HOME SUBYARD_CONFIG_HOME SUBYARD_HOME SUBYARD_KEYS_ROOT SUBYARD_KEYS_TOOLS_DIR SUBYARD_KEYS_CONSUMER_ROOT PATH; do
        printf 'Environment="%s=%s"\n' "$variable" "${!variable}"
      done
    } >>"$service"
    systemctl_user daemon-reload
    systemctl_user start "$AUTO_SERVICE"
    [ "$(systemctl_user show "$AUTO_SERVICE" -p Result --value)" = success ] || fail 'installed automatic sync service failed'
    jq -e '.lastSuccess > 0 and .lastSuccess == .lastAttempt and .nextRetry > .lastSuccess and .consecutiveFailures == 0' "$due" >/dev/null \
      || fail 'automatic worker did not synchronize the due peer'
    printf 'ok: installed automatic worker synchronized a due real SSH peer\n'
    ;;
  revoke) yard keys revoke "$(credential)" --yes >/dev/null ;;
  absent)
    [ ! -e "$CONNECTION" ] && [ ! -e "$LEGACY" ] || fail 'revoked or local-only connection reached the peer'
    wait_status '{"configured":false}'
    ;;
  local-only)
    yard keys import "$STATE/update.json" --label github-local-only --consumer github-app-key --local-only --yes >/dev/null
    yard keys materialize global --yes >/dev/null
    [ -f "$CONNECTION" ] || fail 'local-only connection was not retained locally'
    encrypted
    ;;
  cleanup)
    units_removed=0
    if incus project show "$PROJECT" >/dev/null 2>&1; then
      [ "$(incus project get "$PROJECT" user.subyard.e2e)" = "$MARKER" ] || fail 'refusing unowned project cleanup'
      if incus info "$INSTANCE" --project "$PROJECT" >/dev/null 2>&1; then
        [ "$(incus config get "$INSTANCE" user.subyard.e2e --expanded --project "$PROJECT")" = "$MARKER" ] || fail 'refusing unowned instance cleanup'
      fi
      yard teardown --yes >/dev/null
    fi
    for unit in "$BROKER_SERVICE" subyard-keys-sync.service subyard-keys-sync.timer "$AUTO_SERVICE"; do
      file="$HOME/.config/systemd/user/$unit"
      if [ -e "$file" ]; then
        case "$unit" in
          subyard-keys-sync.timer) cmp -s "$file" "$STATE/data/runtime/current/config/systemd/subyard-keys-sync.timer.in" || fail 'timer ownership differs' ;;
          *) rg -Fq "$STATE/data/runtime" "$file" || rg -Fq "$STATE/data/github-broker" "$file" || fail 'user service ownership differs' ;;
        esac
        systemctl_user disable --now "$unit" >/dev/null 2>&1 || true
        rm -f "$file"
        units_removed=1
      fi
    done
    [ "$units_removed" = 0 ] || systemctl_user daemon-reload
    if [ -e "/run/systemd/system/$SSH_SERVICE" ]; then
      rg -Fqx "# GitHub connection fixture $MARKER" "/run/systemd/system/$SSH_SERVICE" || fail 'SSH service ownership differs'
      sudo -n systemctl stop "$SSH_SERVICE"
      sudo -n rm -f "/run/systemd/system/$SSH_SERVICE"
      sudo -n systemctl daemon-reload
    fi
    owned; rm -rf -- "$STATE"
    printf 'ok: marker-owned connection fixtures removed\n'
    ;;
  *) fail 'unknown fixture phase' ;;
esac
