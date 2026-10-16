#!/usr/bin/env bash
# Sourced by the owning disposable-VM fixture after OpenClaw provisioning.

openclaw_project_isolation() {
	local instance="${1:?}" project="${2:?}" attempt
  local root="/tmp/subyard-p0-project-$TOKEN"
  local name_a="IsolationA$TOKEN" name_b="IsolationB$TOKEN"
  local box_a box_b uid gid shared lock_inode worker
  project_contract_clean_tree "$root" "$MARKER"
  install -d -m 0700 "$root/a" "$root/b"
  printf '%s\n' "$MARKER" > "$root/.subyard-p0-marker"
  printf 'first\n' > "$root/a/project.marker"
  printf 'second\n' > "$root/b/project.marker"
  install -d -m 0700 "$SUBYARD_CONFIG_HOME/secrets/profiles/openclaw"
  printf 'SYNTHETIC_PROJECT_SECRET=first\n' > "$SUBYARD_CONFIG_HOME/secrets/profiles/openclaw/profile.env"
  chmod 0600 "$SUBYARD_CONFIG_HOME/secrets/profiles/openclaw/profile.env"
  yard sync "$root/a" --name "$name_a" --target openclaw --yes >/dev/null
  yard up "$name_a" --yes >/dev/null
  printf 'SYNTHETIC_PROJECT_SECRET=second\n' > "$SUBYARD_CONFIG_HOME/secrets/profiles/openclaw/profile.env"
  yard sync "$root/b" --name "$name_b" --target openclaw --yes >/dev/null
  yard up "$name_b" --yes >/dev/null
  box_a="$(incus exec "$instance" --project "$project" -- docker ps -aq --filter "label=subyard.project=$name_a")"
  box_b="$(incus exec "$instance" --project "$project" -- docker ps -aq --filter "label=subyard.project=$name_b")"
  [[ "$box_a" =~ ^[a-f0-9]{12,64}$ && "$box_b" =~ ^[a-f0-9]{12,64}$ && "$box_a" != "$box_b" ]] \
    || die 'isolation projects did not receive separate containers'
  uid="$(yard shell -- id -u)"
  gid="$(yard shell -- id -g)"
  shared="/srv/cache/npm/.subyard-isolation-$TOKEN"
  incus exec "$instance" --project "$project" -- runuser -u dev -- install -d -m 0700 "$shared"
  lock_inode="$(incus exec "$instance" --project "$project" -- stat -c '%d:%i' /srv/cache/.sy-cache.lock)"
  # Use each container's configured identity, not an exec-time user override.
  for worker in "$box_a" "$box_b"; do
    incus exec "$instance" --project "$project" -- docker exec "$worker" sh -eu -c '
      [ "$(id -u):$(id -g)" = "$1:$2" ]
      grep -Eq "^NoNewPrivs:[[:space:]]+1$" /proc/self/status
      [ "$(stat -c %d:%i /srv/cache/.sy-cache.lock)" = "$3" ]
      [ ! -w /srv/cache/.sy-cache.lock ] && [ ! -w /usr/local/bin/sy-cache ]
      for path in /var/run/docker.sock /run/docker.sock /var/lib/incus/unix.socket /srv/env-secrets /srv/workspaces; do
        [ ! -e "$path" ]
      done
      [ "$(stat -c %a /run/subyard/profile.env)" = 600 ]
      [ -r /run/subyard/profile.env ]
      mkdir -p node_modules .venv /tmp/private-runtime
      touch node_modules/private-marker .venv/private-marker /tmp/private-runtime/private-marker
    ' _ "$uid" "$gid" "$lock_inode"
  done
  incus exec "$instance" --project "$project" -- docker exec "$box_a" sh -eu -c '
    grep -Fxq SYNTHETIC_PROJECT_SECRET=first /run/subyard/profile.env
    [ "$(cat project.marker)" = first ]
    printf only-first > node_modules/first-only
    printf only-first > .venv/first-only
    printf only-first > /tmp/private-runtime/first-only
  '
  incus exec "$instance" --project "$project" -- docker exec "$box_b" sh -eu -c '
    grep -Fxq SYNTHETIC_PROJECT_SECRET=second /run/subyard/profile.env
    [ "$(cat project.marker)" = second ]
    [ ! -e node_modules/first-only ] && [ ! -e .venv/first-only ] && [ ! -e /tmp/private-runtime/first-only ]
  '
  # A real shared operation in L2 A permits L2 B readers and excludes both an
  # L2 B writer and L1 maintenance, through the very same read-only-bound inode.
  incus exec "$instance" --project "$project" -- timeout 40 docker exec "$box_a" \
    sy-cache shared -- sh -eu -c '
      touch "$1/reader-ready"
      for attempt in $(seq 1 200); do
        [ ! -e "$1/release-reader" ] || exit 0
        sleep 0.1
      done
      exit 1
    ' _ "$shared" > "$STATE/isolation-reader.log" 2>&1 &
  worker=$!
  for ((attempt=0; attempt<100; attempt++)); do
    incus exec "$instance" --project "$project" -- test -f "$shared/reader-ready" && break
    sleep 0.1
  done
  incus exec "$instance" --project "$project" -- test -f "$shared/reader-ready" || die 'L2 reader did not acquire cache lock'
  incus exec "$instance" --project "$project" -- docker exec "$box_b" sy-cache shared -- true
  if incus exec "$instance" --project "$project" -- docker exec "$box_b" \
    env SY_CACHE_WAIT_SECONDS=1 sy-cache exclusive -- true; then
    die 'L2 exclusive mutation bypassed a concurrent shared cache operation'
  fi
  if yard shell -- env SY_CACHE_WAIT_SECONDS=1 sy-cache exclusive -- true; then
    die 'L1 maintenance bypassed a concurrent L2 cache operation'
  fi
  incus exec "$instance" --project "$project" -- docker exec "$box_b" touch "$shared/release-reader"
  wait "$worker"
  # Reverse direction: L1 exclusive maintenance excludes an L2 cache writer.
  incus exec "$instance" --project "$project" -- timeout 40 runuser -u dev -- \
    sy-cache exclusive -- sh -eu -c '
      touch "$1/writer-ready"
      for attempt in $(seq 1 200); do
        [ ! -e "$1/release-writer" ] || exit 0
        sleep 0.1
      done
      exit 1
    ' _ "$shared" > "$STATE/isolation-writer.log" 2>&1 &
  worker=$!
  for ((attempt=0; attempt<100; attempt++)); do
    incus exec "$instance" --project "$project" -- test -f "$shared/writer-ready" && break
    sleep 0.1
  done
  incus exec "$instance" --project "$project" -- test -f "$shared/writer-ready" || die 'L1 writer did not acquire cache lock'
  if incus exec "$instance" --project "$project" -- docker exec "$box_a" \
    env SY_CACHE_WAIT_SECONDS=1 sy-cache shared -- true; then
    die 'L2 cache writer bypassed L1 maintenance'
  fi
  incus exec "$instance" --project "$project" -- docker exec "$box_b" touch "$shared/release-writer"
  wait "$worker"
  incus exec "$instance" --project "$project" -- docker exec "$box_a" sy-cache exclusive -- true
  # Deselection denies fresh admission and up, while retained cleanup stays usable.
  yard config set ENVIRONMENT_PROFILES '' --scope yard --local --yes >/dev/null
  if yard up "$name_a" --yes > "$STATE/isolation-deselected-up.log" 2>&1; then
    die 'deselected profile admitted a project startup'
  fi
  if yard sync "$root/a" --name "Rejected$TOKEN" --target openclaw --yes > "$STATE/isolation-deselected-import.log" 2>&1; then
    die 'deselected profile admitted a new project'
  fi
  yard down "$name_a" --yes >/dev/null
  yard remove "$name_a" --yes >/dev/null
  yard down "$name_b" --yes >/dev/null
  yard remove "$name_b" --yes >/dev/null
  yard config set ENVIRONMENT_PROFILES openclaw --scope yard --local --yes >/dev/null
  incus exec "$instance" --project "$project" -- runuser -u dev -- rm -r -- "$shared"
  rm -- "$SUBYARD_CONFIG_HOME/secrets/profiles/openclaw/profile.env"
  project_contract_clean_tree "$root" "$MARKER"
  printf 'ok: parallel L2 UID, private state, per-consumer secrets, cache locking and active admission\n'
}
