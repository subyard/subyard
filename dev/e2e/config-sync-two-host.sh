#!/usr/bin/env bash
# Candidate and published-release two-host configuration sync acceptance.
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

# Published mode is confined to broker-allocated disposable guests.
PUBLISHED_VERSION=0.17.3
PUBLISHED_INSTALLER_SHA256=a78c10910c0886a8d01c0a2e77718b21ce58e45fa7f0de6064bc1f351d56028b
SSH_SERVICE=subyard-config-sync-ssh-e2e.service
REMOTE="ssh://root@sync-private$REMOTE_ROOT/remote.git"

published_scope() {
  case "${SUBYARD_E2E_VM:-}" in 1|2) ;; *) fail "published acceptance requires an allocated VM" ;; esac
  [ -n "${SUBYARD_E2E_RUN_ID:-}" ] || fail "published acceptance requires lease attribution"
  [ -f "$STATE/.subyard-config-sync-e2e" ] \
    && [ "$(cat "$STATE/.subyard-config-sync-e2e")" = "$SUBYARD_E2E_RUN_ID" ] \
    || fail "published fixture marker does not match this lease"
}

published_host() {
  published_scope
  name="$PEER"
  case "$name" in host-a|host-b) ;; *) fail "published phase requires host-a or host-b" ;; esac
  root="$STATE/$name"
  export HOME="$root/home" SUBYARD_OPERATOR_HOME="$root/home"
  export SUBYARD_CONFIG_HOME="$root/config" SUBYARD_HOME="$root/data"
  export SUBYARD_NO_AUDIT=1 GIT_TERMINAL_PROMPT=0
  unset SUBYARD_HOST_ID SUBYARD_REPOSITORY_ROOT
  export GIT_AUTHOR_NAME='Subyard E2E' GIT_AUTHOR_EMAIL=subyard-e2e@invalid
  export GIT_COMMITTER_NAME="$GIT_AUTHOR_NAME" GIT_COMMITTER_EMAIL="$GIT_AUTHOR_EMAIL"
  export GIT_SSH_COMMAND="ssh -F $root/ssh-config"
  # Empty SSH clones use the client default before a remote branch exists.
  export GIT_CONFIG_COUNT=2 GIT_CONFIG_KEY_1=init.defaultBranch GIT_CONFIG_VALUE_1=main
  yard="$root/bin/yard"
  checkout="$root/checkout"
}

published_install() {
  local installer="$STATE/subyard-install.sh" runtime bundle digest
  unset YARD_RELEASE_BASE_URL YARD_RELEASE_REPOSITORY YARD_RELEASE_TAG YARD_RUNTIME_ROOT YARD_RELEASE_CACHE
  curl -fsSL --proto '=https' --tlsv1.2 --connect-timeout 15 --max-time 180 \
    "https://github.com/Subyard/Subyard/releases/download/v$PUBLISHED_VERSION/subyard-install.sh" \
    -o "$installer"
  [ "$(sha256sum "$installer" | cut -d' ' -f1)" = "$PUBLISHED_INSTALLER_SHA256" ] \
    || fail "published installer checksum changed"
  chmod 0700 "$installer"
  install -d -m 0700 "$root" "$HOME" "$SUBYARD_CONFIG_HOME" "$SUBYARD_HOME" "$root/bin"
  YARD_BIN_DIR="$root/bin" YARD_SHELL_RC="$HOME/.bashrc" YARD_LOGIN_RC="$HOME/.profile" \
    bash "$installer" --version "$PUBLISHED_VERSION" --yes >/dev/null
  [ "$("$yard" --version)" = "yard $PUBLISHED_VERSION" ] || fail "wrong published release version"
  bundle="$SUBYARD_HOME/releases/$PUBLISHED_VERSION/subyard-$PUBLISHED_VERSION-linux-amd64.tar.gz"
  [ -f "$bundle" ] || fail "published acceptance requires amd64 runtime assets"
  digest="$(sha256sum "$bundle" | cut -d' ' -f1)"
  [ "$(cut -d' ' -f1 "$bundle.sha256")" = "$digest" ] || fail "published bundle checksum mismatch"
  jq -e --arg version "$PUBLISHED_VERSION" --arg digest "$digest" \
    '.version == $version and .sha256 == $digest and
     .canonicalRepository == "github.com/Subyard/Subyard" and
     (.sourceRevision | test("^[0-9a-f]{40}$"))' "$bundle.provenance.json" >/dev/null \
    || fail "published bundle provenance mismatch"
  runtime="$(readlink -f "$yard")"
  [ "$runtime" = "$SUBYARD_HOME/runtime/releases/$PUBLISHED_VERSION-${digest:0:12}/bin/yard" ] \
    || fail "published launcher is outside the verified immutable release"
  (cd "${runtime%/bin/yard}" && sha256sum -c runtime-files.sha256 >/dev/null) \
    || fail "installed immutable runtime checksum mismatch"
  printf '%s %s %s\n' "$PUBLISHED_VERSION" "$digest" \
    "$(jq -r '.sourceRevision' "$bundle.provenance.json")" >"$STATE/release-identity"
  printf 'ok: %s installed checksum-verified published v%s\n' "$name" "$PUBLISHED_VERSION"
}

published_server() {
  published_scope
  [ ! -e "$REMOTE_ROOT" ] || fail "refusing to reuse an existing private remote"
  [ ! -e "/run/systemd/system/$SSH_SERVICE" ] || fail "private SSH service already exists"
  install -d -m 0700 "$REMOTE_ROOT"
  printf '%s\n' "$SUBYARD_E2E_RUN_ID" >"$REMOTE_ROOT/.subyard-config-sync-e2e"
  chmod 0600 "$REMOTE_ROOT/.subyard-config-sync-e2e"
  git init --bare -q "$REMOTE_ROOT/remote.git"
  git --git-dir="$REMOTE_ROOT/remote.git" symbolic-ref HEAD refs/heads/main
  ssh-keygen -q -t ed25519 -N '' -f "$REMOTE_ROOT/server-key"
  cat >"$REMOTE_ROOT/git-only" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
case "${SSH_ORIGINAL_COMMAND:-}" in
  "git-upload-pack '/srv/subyard-config-sync-e2e/remote.git'") exec git-upload-pack /srv/subyard-config-sync-e2e/remote.git ;;
  "git-receive-pack '/srv/subyard-config-sync-e2e/remote.git'") exec git-receive-pack /srv/subyard-config-sync-e2e/remote.git ;;
  *) exit 126 ;;
esac
SH
  chmod 0700 "$REMOTE_ROOT/git-only"
  for host in host-a host-b; do
    printf 'restrict,command="%s/git-only" %s\n' "$REMOTE_ROOT" "$(cat "$STATE/$host.pub")"
  done >"$REMOTE_ROOT/authorized_keys"
  chmod 0600 "$REMOTE_ROOT/authorized_keys"
  cat >"$REMOTE_ROOT/sshd-config" <<EOF
Port 19422
ListenAddress 0.0.0.0
HostKey $REMOTE_ROOT/server-key
AuthorizedKeysFile $REMOTE_ROOT/authorized_keys
StrictModes yes
PubkeyAuthentication yes
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin prohibit-password
AllowUsers root
AllowTcpForwarding no
AllowAgentForwarding no
X11Forwarding no
PermitTTY no
UsePAM yes
UseDNS no
LogLevel ERROR
EOF
  chmod 0600 "$REMOTE_ROOT/sshd-config"
  install -d -m 0755 /run/sshd
  /usr/sbin/sshd -t -f "$REMOTE_ROOT/sshd-config"
  cat >"/run/systemd/system/$SSH_SERVICE" <<EOF
# subyard-config-sync-e2e $SUBYARD_E2E_RUN_ID
[Unit]
Description=Disposable authenticated configuration Git remote
[Service]
ExecStart=/usr/sbin/sshd -D -e -f $REMOTE_ROOT/sshd-config
EOF
  chmod 0644 "/run/systemd/system/$SSH_SERVICE"
  systemctl daemon-reload
  systemctl start "$SSH_SERVICE"
}

published_status_failure() {
  local output="$1" reason="$2"
  # Only public status fields: no config values, credential files or raw diagnostics.
  awk '/^  (owner-host|remote|branch|upstream|head|fetch|fetched-upstream|worktree|relation|recovery|applied-commit|generation|live):/ {
    printf "%.200s\n", $0
    if (++count == 14) exit
  }' <<<"$output" >&2
  fail "$reason"
}

published_status() {
  local expected="$1" output head generation manifest upstream upstream_ref
  output="$("$yard" config sync status)" || published_status_failure "$output" "$name did not converge"
  head="$(git -C "$checkout" rev-parse HEAD)"
  upstream_ref="$(git -C "$checkout" rev-parse --symbolic-full-name '@{upstream}')" \
    || published_status_failure "$output" "$name has no exact Git upstream"
  [ "$upstream_ref" = refs/remotes/origin/main ] \
    || published_status_failure "$output" "$name upstream is not the expected origin/main ref"
  # Match the released CLI's Git presentation while binding the full ref above.
  upstream="$(git -C "$checkout" rev-parse --abbrev-ref --symbolic-full-name '@{upstream}')"
  for line in "owner-host: $name" "remote: $REMOTE" 'branch: main' "upstream: $upstream" \
    "head: $head" "fetched-upstream: $head" "applied-commit: $head" \
    'worktree: clean (staged=0 unstaged=0 untracked=0 conflicts=0)' \
    'relation: up-to-date' 'recovery: none' 'live: converged'; do
    grep -Fxq "  $line" <<<"$output" || published_status_failure "$output" "$name missing status assertion: $line"
  done
  generation="$(awk '/^  generation:/ {print $2}' <<<"$output")"
  [[ "$generation" =~ ^[1-9][0-9]*$ ]] || published_status_failure "$output" "$name has no applied generation"
  manifest="$SUBYARD_CONFIG_HOME/.sync/manifest.json"
  jq -e --arg host "$name" --arg head "$head" --argjson generation "$generation" \
    '.hostId == $host and .sourceCommit == $head and .generation == $generation' \
    "$manifest" >/dev/null || published_status_failure "$output" "$name applied manifest differs from status"
  [ "$(cat "$SUBYARD_CONFIG_HOME/host-id")" = "$name" ] \
    && [ "$(stat -c %a "$SUBYARD_CONFIG_HOME/host-id")" = 600 ] \
    || fail "$name did not retain its protected HostID"
  "$yard" config show YARD_IMAGE >"$root/show.out"
  grep -Fq "effective: $expected" "$root/show.out" || fail "$name effective value differs"
  grep -Fq "$SUBYARD_CONFIG_HOME/.sync/settings/overrides/shared/config.env" "$root/show.out" \
    || fail "$name effective value is not persisted in the Git cache"
  grep -Eq "^YARD_IMAGE=['\"]?${expected}['\"]?$" \
    "$SUBYARD_CONFIG_HOME/.sync/settings/overrides/shared/config.env" \
    || fail "$name live persisted settings differ"
  printf '%s\n' "$generation" >"$root/generation"
  printf 'ok: %s commit=%s generation=%s clean/up-to-date/live-converged\n' "$name" "$head" "$generation"
}

published_snapshot() {
  local target="$1"
  (cd "$SUBYARD_CONFIG_HOME" && find . -type f -print0 | sort -z | xargs -0 sha256sum) >"$target.live"
  (cd "$SUBYARD_CONFIG_HOME" && find . -printf '%P %y %m\n' | sort) >>"$target.live"
  git ls-remote "$REMOTE" | sort >"$target.remote"
  git --git-dir="$REMOTE_ROOT/remote.git" for-each-ref --format='%(refname) %(objectname)' | sort >>"$target.remote"
  git -C "$checkout" rev-parse HEAD >"$target.head"
  git -C "$checkout" status --porcelain=v1 --untracked-files=all >"$target.worktree"
  sha256sum "$checkout/shared/config.env" >>"$target.worktree"
  sha256sum "$checkout/.git/index" >"$target.index"
}

published_blocked() {
  local label="$1" diagnostic="$2" operation
  published_snapshot "$root/before"
  for operation in push pull set; do
    case "$operation" in
      push) args=(config sync push --apply --yes) ;;
      pull) args=(config sync pull --apply --yes) ;;
      set) args=(config set YARD_IMAGE images:alpine/3.22 --scope shared --git --yes) ;;
    esac
    if "$yard" "${args[@]}" >"$root/blocked.out" 2>&1; then
      fail "$label $operation unexpectedly succeeded"
    fi
    grep -Fiq "$diagnostic" "$root/blocked.out" || fail "$label $operation failed for the wrong reason"
    published_snapshot "$root/after"
    for part in live remote head worktree index; do
      cmp -s "$root/before.$part" "$root/after.$part" || fail "$label $operation changed $part"
    done
  done
  printf 'ok: %s push/pull/typed-set fail closed without live, refs or checkout mutation\n' "$label"
}

published_fail_closed() {
  local saved remote_commit branch
  saved="$(git -C "$checkout" rev-parse HEAD)"
  branch="$(git -C "$checkout" symbolic-ref HEAD)"
  printf '\n# dirty fixture\n' >>"$checkout/shared/config.env"
  if "$yard" config sync status --offline >"$root/blocked.status"; then fail "dirty status succeeded"; fi
  grep -Fq 'worktree: dirty' "$root/blocked.status" || fail "missing dirty status"
  published_blocked dirty dirty
  git -C "$checkout" restore -- shared/config.env

  # Fixture-only Git commits intentionally put the same line in conflict.
  git clone -q "$REMOTE" "$STATE/divergence-candidate"
  printf '\n# remote conflict fixture\n' >>"$STATE/divergence-candidate/shared/config.env"
  git -C "$STATE/divergence-candidate" add shared/config.env
  git -C "$STATE/divergence-candidate" commit -q -m 'Create isolated remote conflict fixture'
  git -C "$STATE/divergence-candidate" push -q origin main
  remote_commit="$(git -C "$STATE/divergence-candidate" rev-parse HEAD)"
  printf '\n# local conflict fixture\n' >>"$checkout/shared/config.env"
  git -C "$checkout" add shared/config.env
  git -C "$checkout" commit -q -m 'Create isolated local conflict fixture'
  if "$yard" config sync status >"$root/blocked.status"; then fail "diverged status succeeded"; fi
  grep -Fq 'relation: diverged 1/1' "$root/blocked.status" || fail "missing diverged status"
  published_blocked diverged diverged
  git -C "$checkout" fetch -q origin
  if git -C "$checkout" merge --no-edit origin/main >"$root/merge.out" 2>&1; then fail "fixture merge did not conflict"; fi
  [ -n "$(git -C "$checkout" ls-files -u)" ] || fail "fixture did not create index conflicts"
  if "$yard" config sync status --offline >"$root/blocked.status"; then fail "conflict status succeeded"; fi
  grep -Eq 'conflicts=[1-9][0-9]*' "$root/blocked.status" || fail "missing conflict status"
  published_blocked conflict conflicted
  git -C "$checkout" merge --abort
  git --git-dir="$REMOTE_ROOT/remote.git" update-ref "$branch" "$saved" "$remote_commit"
  git -C "$checkout" reset --hard -q "$saved"
  git -C "$checkout" fetch -q origin
  "$yard" config sync pull --apply --yes >/dev/null
  published_status images:debian/13
}

if [[ "$MODE" = published-* ]]; then
  case "$MODE" in
    published-prepare)
      case "${SUBYARD_E2E_VM:-}" in 1|2) ;; *) fail "published acceptance requires an allocated VM" ;; esac
      [ -n "${SUBYARD_E2E_RUN_ID:-}" ] || fail "lease attribution is required"
      [ ! -e "$STATE" ] || fail "refusing to reuse published fixture state"
      reset_owned_directory "$STATE"
      printf '%s\n' "$SUBYARD_E2E_RUN_ID" >"$STATE/.subyard-config-sync-e2e"
      published_host
      published_install
      ;;
    published-server) published_server ;;
    published-auth)
      published_host
      address="${3:?published-auth requires the remote address}"
      cat >"$root/ssh-config" <<EOF
Host sync-private
    HostName $address
    Port 19422
    User root
    IdentityFile $root/client-key
    IdentitiesOnly yes
    BatchMode yes
    StrictHostKeyChecking yes
    HostKeyAlias sync-private
    UserKnownHostsFile $root/known-hosts
    GlobalKnownHostsFile /dev/null
    LogLevel ERROR
EOF
      chmod 0600 "$root/client-key" "$root/ssh-config" "$root/known-hosts"
      wait_remote "$REMOTE"
      # Both authenticated keys are limited to the isolated repository.
      shell_rc=0
      ssh -F "$root/ssh-config" sync-private true >/dev/null 2>&1 || shell_rc=$?
      [ "$shell_rc" = 126 ] || fail "Git authentication did not enforce the command restriction"
      if GIT_SSH_COMMAND="ssh -F $root/ssh-config -o PubkeyAuthentication=no -o PreferredAuthentications=none" \
        git ls-remote "$REMOTE" >/dev/null 2>&1; then
        fail "private remote allowed access without synthetic authentication"
      fi
      ;;
    published-connect)
      published_host
      args=(config sync connect "$REMOTE" --host-id "$name" --checkout "$checkout" --yes)
      [ "$name" != host-a ] || args+=(--init)
      "$yard" "${args[@]}" >/dev/null
      ;;
    published-change)
      published_host
      value="${3:?published-change requires a typed value}"
      previous="$(jq -r '.generation' "$SUBYARD_CONFIG_HOME/.sync/manifest.json")"
      "$yard" config set YARD_IMAGE "$value" --scope shared --git --yes >/dev/null
      "$yard" config sync push --apply --yes >/dev/null
      published_status "$value"
      [ "$(cat "$root/generation")" -gt "$previous" ] || fail "$name generation did not advance"
      ;;
    published-pull)
      published_host
      value="${3:?published-pull requires a typed value}"
      previous="$(jq -r '.generation' "$SUBYARD_CONFIG_HOME/.sync/manifest.json")"
      if "$yard" config sync status >"$root/before-pull.status"; then fail "$name was not behind before pull"; fi
      grep -Fq 'relation: behind 1' "$root/before-pull.status" || fail "$name missing behind relation"
      "$yard" config sync pull --apply --yes >/dev/null
      published_status "$value"
      [ "$(cat "$root/generation")" -gt "$previous" ] || fail "$name pull generation did not advance"
      ;;
    published-verify) published_host; published_status "${3:?expected typed value required}" ;;
    published-fail-closed) published_host; [ "$name" = host-a ] || fail "fail-closed phase owns host A remote"; published_fail_closed ;;
    published-cleanup)
      if [ -e "$STATE" ]; then
        published_scope
        if [ -e "$REMOTE_ROOT" ]; then
          [ -f "$REMOTE_ROOT/.subyard-config-sync-e2e" ] \
            && [ "$(cat "$REMOTE_ROOT/.subyard-config-sync-e2e")" = "$SUBYARD_E2E_RUN_ID" ] \
            || fail "refusing to remove unowned private remote"
          if [ -e "/run/systemd/system/$SSH_SERVICE" ]; then
            grep -Fxq "# subyard-config-sync-e2e $SUBYARD_E2E_RUN_ID" "/run/systemd/system/$SSH_SERVICE" \
              || fail "refusing to remove unowned private SSH service"
            systemctl stop "$SSH_SERVICE"
            rm -f "/run/systemd/system/$SSH_SERVICE"
            systemctl daemon-reload
            ! systemctl is-active --quiet "$SSH_SERVICE" || fail "private SSH service remains active"
          fi
          rm -rf -- "$REMOTE_ROOT"
        fi
        rm -rf -- "$STATE"
      fi
      [ ! -e "$STATE" ] && [ ! -e "$REMOTE_ROOT" ] || fail "published fixture cleanup incomplete"
      printf 'ok: published configuration-sync marker-owned fixtures removed\n'
      ;;
    *) fail "unknown published phase" ;;
  esac
  exit 0
fi

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
    "$yard" config set YARD_IMAGE images:debian/12 --scope shared --git --yes
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
    "$yard" config set YARD_IMAGE images:debian/13 --scope shared --git --yes
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
