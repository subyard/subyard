#!/usr/bin/env bash
# subyard-provision-check-v1
# config/profiles/openclaw/provision.sh — install the OpenClaw toolchain into the yard (run as root
# inside the yard by the Go provision workflow; idempotent). Vars: NODE_VERSION, COREPACK_VERSION,
# PNPM_VERSION, DEV_USER, OPTIONAL_FEATURES, and the cache contract forwarded from profile.conf
# (PIP_CACHE_DIR, npm_config_cache, npm_config_store_dir, PLAYWRIGHT_BROWSERS_PATH). Cache paths use
# DEV_USER tool configs to keep root writes out of the shared cache.
set -euo pipefail

check_only=0
case "${1:-}" in
  --check) check_only=1; shift ;;
  "") ;;
  *) printf 'openclaw provision: unknown argument %s\n' "$1" >&2; exit 2 ;;
esac
[ "$#" -eq 0 ] || { printf 'openclaw provision: unexpected argument\n' >&2; exit 2; }

NODE_VERSION="${NODE_VERSION:-24.15.0}"
COREPACK_VERSION="${COREPACK_VERSION:-0.31.0}"
PNPM_VERSION="${PNPM_VERSION:-11.2.2}"
DEV_USER="${DEV_USER:-dev}"
OPTIONAL_FEATURES="${OPTIONAL_FEATURES:-}"

# Capture cache paths for the dev-owned tool config before root tool setup.
CACHE_PIP="${PIP_CACHE_DIR:-}"
CACHE_NPM="${npm_config_cache:-}"
CACHE_PNPM_STORE="${npm_config_store_dir:-}"
CACHE_PLAYWRIGHT="${PLAYWRIGHT_BROWSERS_PATH:-}"
unset PIP_CACHE_DIR npm_config_cache npm_config_store_dir PLAYWRIGHT_BROWSERS_PATH

DEV_HOME="${OPENCLAW_DEV_HOME:-$(getent passwd "$DEV_USER" | cut -d: -f6)}"
: "${DEV_HOME:=/home/$DEV_USER}"

if [ "$check_only" -eq 1 ]; then
  root_prefix="${OPENCLAW_TEST_ROOT:-}"
  rooted() { printf '%s%s' "$root_prefix" "$1"; }
  changed=0
  node="$(rooted /usr/local/bin/node)"
  corepack="$(rooted /usr/local/bin/corepack)"
  pnpm="$(rooted /usr/local/bin/pnpm)"
  sy_cache="$(rooted /usr/local/bin/sy-cache)"
  python="$(rooted /opt/venv/bin/python)"
  pip="$(rooted /opt/venv/bin/pip)"
  [ -x "$node" ] && [ "$($node --version 2>/dev/null)" = "v${NODE_VERSION}" ] || changed=1
  [ -x "$corepack" ] && [ "$($corepack --version 2>/dev/null)" = "$COREPACK_VERSION" ] || changed=1
  [ -x "$pnpm" ] && [ "$($pnpm --version 2>/dev/null)" = "$PNPM_VERSION" ] || changed=1
  [ -x "$sy_cache" ] && [ -x "$python" ] && [ -x "$pip" ] || changed=1
  expected_owner="$(id -u "$DEV_USER"):$(id -g "$DEV_USER")"
  for cache in /srv/cache/pnpm /srv/cache/pip /srv/cache/npm; do
    directory="$(rooted "$cache")"
    [ -d "$directory" ] && [ ! -L "$directory" ] \
      && [ "$(stat -c '%u:%g' "$directory" 2>/dev/null)" = "$expected_owner" ] || changed=1
  done
  if [ -n "$CACHE_NPM" ]; then
    npmrc="$DEV_HOME/.npmrc"
    [ -f "$npmrc" ] && [ ! -L "$npmrc" ] \
      && [ "$(grep -Fxc '# >>> subyard-openclaw >>>' "$npmrc" 2>/dev/null)" -eq 1 ] \
      && [ "$(grep -Fxc "cache=$CACHE_NPM" "$npmrc" 2>/dev/null)" -eq 1 ] \
      && [ "$(grep -Fxc '# <<< subyard-openclaw <<<' "$npmrc" 2>/dev/null)" -eq 1 ] \
      || changed=1
  fi
  if [ -n "$CACHE_PIP" ]; then
    pip_conf="$DEV_HOME/.config/pip/pip.conf"
    [ -f "$pip_conf" ] && [ ! -L "$pip_conf" ] \
      && grep -Fxq "cache-dir = $CACHE_PIP" "$pip_conf" 2>/dev/null || changed=1
  fi
  profile_env="$(rooted /etc/profile.d/subyard-openclaw.sh)"
  environment_file="$(rooted /etc/environment)"
  docs="$(rooted /etc/subyard/openclaw-l1.md)"
  [ -f "$profile_env" ] && [ ! -L "$profile_env" ] \
    && grep -Fq 'SUBYARD_OPENCLAW_DOCS=/etc/subyard/openclaw-l1.md' "$profile_env" || changed=1
  [ -f "$environment_file" ] && [ ! -L "$environment_file" ] \
    && [ "$(grep -Fxc '# >>> subyard-openclaw >>>' "$environment_file" 2>/dev/null)" -eq 1 ] \
    && grep -Fxq 'SUBYARD_OPENCLAW_DOCS=/etc/subyard/openclaw-l1.md' "$environment_file" \
    || changed=1
  [ -f "$docs" ] && [ ! -L "$docs" ] \
    && grep -Fq '# OpenClaw in this yard' "$docs" || changed=1
  case " ${OPTIONAL_FEATURES:-} " in
    *" browser_tests "*)
      command -v chromium >/dev/null 2>&1 || changed=1
      [ -d "$(rooted /srv/cache/playwright)" ] || changed=1
      ;;
  esac
  case " ${OPTIONAL_FEATURES:-} " in
    *" sandbox_tests "*)
      for tool in dockerd-rootless-setuptool.sh fuse-overlayfs slirp4netns bwrap; do
        command -v "$tool" >/dev/null 2>&1 || changed=1
      done
      ;;
  esac
  [ "$changed" -eq 0 ] && exit 0
  exit 10
fi

export DEBIAN_FRONTEND=noninteractive

# 1. Node — pinned, from nodejs.org into /usr/local (shadows the distro node).
if [ "$(/usr/local/bin/node --version 2>/dev/null)" != "v${NODE_VERSION}" ]; then
  case "$(dpkg --print-architecture)" in
    amd64) na=x64 ;; arm64) na=arm64 ;; *) echo "unsupported arch" >&2; exit 1 ;;
  esac
  tmp="$(mktemp -d)"; base="node-v${NODE_VERSION}-linux-${na}"
  curl -fsSL "https://nodejs.org/dist/v${NODE_VERSION}/${base}.tar.xz" -o "$tmp/${base}.tar.xz"
  curl -fsSL "https://nodejs.org/dist/v${NODE_VERSION}/SHASUMS256.txt" -o "$tmp/sha.txt"
  ( cd "$tmp" && grep " ${base}.tar.xz\$" sha.txt | sha256sum -c - )
  # tar won't prune files a new release dropped, so an in-place major upgrade would merge the old
  # npm/corepack into the new one (broken deps, e.g. minipass mismatch). Replace them cleanly.
  rm -rf /usr/local/lib/node_modules/npm /usr/local/lib/node_modules/corepack
  tar -xJf "$tmp/${base}.tar.xz" -C /usr/local --strip-components=1
  rm -rf "$tmp"
fi

# 2. corepack + pnpm; arch-scoped shim. The shim also pins pnpm's STORE to the shared /srv/cache as a
#    pnpm-only CLI flag (baked from profile.conf), so `npm`/`npx` never see the (to them invalid)
#    store-dir key and never warn — and nothing is forced via global env onto unrelated yard tools.
/usr/local/bin/npm install -g "corepack@${COREPACK_VERSION}" >/dev/null
/usr/local/bin/corepack enable >/dev/null
/usr/local/bin/corepack prepare "pnpm@${PNPM_VERSION}" --activate >/dev/null
{
  cat <<'SH'
#!/usr/bin/env bash
# arch-scoped so ad-hoc installs don't pull win/mac/musl native variants. pnpm honors fetch-timeout
# only as a CLI flag (not .npmrc / npm_config_* env), and the yard's egress is slow on OpenClaw's huge
# native/ML tarballs → inject a generous one for install-type commands. store-dir (baked below) points
# pnpm at the shared store; it is a pnpm-only flag, so npm/npx never see it (no "Unknown config" warn).
# confirmModulesPurge: pointing pnpm at the shared store invalidates a node_modules built against any
# other one, and pnpm's purge prompt ABORTS with no TTY — which is every agent shell.
ft=; case "${1:-}" in install|add|update|up|i|fetch) ft="--fetch-timeout=1800000 --config.confirmModulesPurge=false" ;; esac
SH
  printf 'sd=%q\n' "$CACHE_PNPM_STORE"
  cat <<'SH'
LOCK=/srv/cache/.sy-cache.lock
runpnpm() { exec corepack pnpm \
  --config.supportedArchitectures.os=linux \
  --config.supportedArchitectures.cpu=current \
  --config.supportedArchitectures.libc=glibc \
  ${sd:+--config.store-dir="$sd"} "$@"; }
# Cache locking, DECOUPLED from the fetch-timeout above:
#   `store prune|clean` MUTATE the store -> EXCLUSIVE lock; re-exec ourself marked so a raw
#   `pnpm store prune` self-serializes (no reliance on sy-cache) without duplicating flags.
#   Store-WRITING verbs -> SHARED lock, BEST-EFFORT (a lock hiccup must NEVER block a build), held on
#   fd 8 across the exec so it spans the install. `flock -o` (exclusive) keeps the lock fd out of the
#   child tree; the SY_CACHE_* markers stop nested pnpm/sy-cache from re-locking or self-deadlocking.
if [ -z "${SY_CACHE_LOCKED:-}" ] && [ -e "$LOCK" ]; then
  if [ "${1:-}" = store ] && { [ "${2:-}" = prune ] || [ "${2:-}" = clean ]; }; then
    exec flock -o -w 3600 -x "$LOCK" env SY_CACHE_LOCKED=1 "$0" "$@"
  fi
  case "${1:-}" in
    install|add|update|up|i|fetch|dlx|exec|patch|patch-commit|rebuild|import|deploy)
      if [ -r "$LOCK" ]; then
        exec 8<"$LOCK" && flock -s -w 600 8 2>/dev/null && export SY_CACHE_SHARED_HELD=1 || true
      fi ;;
  esac
fi
runpnpm "$@" $ft
SH
} > /usr/local/bin/pnpm
chmod +x /usr/local/bin/pnpm

# 3. Python dev venv.
[ -x /opt/venv/bin/python ] || python3 -m venv /opt/venv
/opt/venv/bin/pip install -q --upgrade pip setuptools wheel

# 4. Shared caches: heal ownership left by older provision runs.
install -d -o "$DEV_USER" -g "$DEV_USER" /srv/cache/pnpm /srv/cache/pip /srv/cache/npm
chown -R "$DEV_USER:$DEV_USER" /srv/cache/pnpm /srv/cache/pip /srv/cache/npm

# 4d. Single-writer guard for cache-MUTATING ops. The pnpm store corrupts if a prune/clean runs while
#     an install writes it. The pnpm shim (step 2) auto-locks `pnpm store prune|clean` EXCLUSIVELY and
#     holds a SHARED lock during installs/fetches, so a mutation waits for in-flight installs and blocks
#     new ones. `sy-cache` adds the same exclusive guard for npm/pip and an explicit `all`. dev-owned lock.
[ -e /srv/cache/.sy-cache.lock ] || : > /srv/cache/.sy-cache.lock
chown "$DEV_USER:$DEV_USER" /srv/cache/.sy-cache.lock 2>/dev/null || true
cat > /usr/local/bin/sy-cache <<'SC'
#!/usr/bin/env bash
# sy-cache — serialize cache-MUTATING ops on the shared /srv/cache store under an EXCLUSIVE lock.
# pnpm installs/fetches hold a SHARED lock, so a mutation here waits for in-flight pnpm installs and
# blocks new ones — never corrupting the pnpm store mid-fetch. (npm/pip installs are NOT shared-locked,
# so don't run `clean`/`purge` while an npm/pip install is in flight.) A raw `pnpm store prune` is
# already auto-locked by the pnpm wrapper; this also covers npm/pip and an explicit `all`.
set -euo pipefail
LOCK="${SY_CACHE_LOCK:-/srv/cache/.sy-cache.lock}"
die() { printf 'sy-cache: %s\n' "$*" >&2; exit 1; }
usage() { cat <<'H'
sy-cache <prune|clean|purge|all|lock -- CMD...> — single-writer guard for /srv/cache mutations.
  prune  pnpm store prune     clean  npm cache clean --force     purge  pip cache purge
  all    all three in order   lock -- CMD...  run CMD under the exclusive lock
  --force  prune/all only: proceed even if the store has no hardlinked references (see below)
pnpm installs/fetches hold a shared lock, so these wait for in-flight pnpm installs. npm/pip installs
are NOT shared-locked — don't run clean/purge while an npm/pip install is running.
H
}
# prune removes packages nothing REFERENCES — and a reference is a hardlink out of the store. Across a
# mount boundary (bind-mounted workspace, store on the yard volume) pnpm cannot hardlink and copies
# instead, so nothing ever references the store and a prune empties all of it.
store_prune_is_a_wipe() {
  local store
  store=$(pnpm store path 2>/dev/null) || return 1
  [ -d "$store" ] || return 1
  [ -n "$(find "$store" -type f -print -quit 2>/dev/null)" ] || return 1   # empty store: nothing to lose
  [ -z "$(find "$store" -type f -links +1 -print -quit 2>/dev/null)" ]     # no file hardlinked out
}
guard_prune() {
  [ "${SY_CACHE_FORCE:-}" = 1 ] && return 0
  store_prune_is_a_wipe || return 0
  die "refusing: nothing hardlinks this store, so a prune would delete the WHOLE shared cache, not
     just orphans, and every agent would re-download it. --force to override."
}
run_locked() {
  [ -n "${SY_CACHE_SHARED_HELD:-}" ] && die "refusing to mutate the cache from inside a build holding the shared lock — run sy-cache outside the build"
  [ -n "${SY_CACHE_LOCKED:-}" ] && exec "$@"   # already inside an exclusive section — don't re-lock
  [ -e "$LOCK" ] || die "lock $LOCK missing — run 'yard provision openclaw'"
  exec flock -o -w 3600 -x "$LOCK" env SY_CACHE_LOCKED=1 "$@"
}
sub="${1:-}"; shift || true
# `case`, not `[ ] && …`: under set -e a loop ending in a false test returns 1 and kills the script.
for a in "$@"; do case "$a" in --force) export SY_CACHE_FORCE=1 ;; esac; done
case "$sub" in
  prune) guard_prune; run_locked pnpm store prune ;;
  clean) run_locked npm cache clean --force ;;
  purge) run_locked sh -c 'if command -v pip >/dev/null 2>&1; then exec pip cache purge; else exec /opt/venv/bin/pip cache purge; fi' ;;
  all)   guard_prune; run_locked sh -c 'set -e; pnpm store prune; npm cache clean --force; if command -v pip >/dev/null 2>&1; then pip cache purge; else /opt/venv/bin/pip cache purge; fi' ;;
  lock)  [ "${1:-}" = -- ] && shift; [ "$#" -gt 0 ] || die "usage: sy-cache lock -- CMD..."; run_locked "$@" ;;
  -h|--help|help) usage; exit 0 ;;
  "")    usage >&2; exit 2 ;;
  *)     usage >&2; die "unknown subcommand '$sub'" ;;
esac
SC
chmod +x /usr/local/bin/sy-cache

# 4b. Apply shared paths through dev tool configs so every shell agrees:
#     pnpm → shim; npm → ~/.npmrc; pip → ~/.config/pip/pip.conf.
if [ -n "$CACHE_NPM" ]; then
  npmrc="$DEV_HOME/.npmrc"; [ -f "$npmrc" ] || : > "$npmrc"
  sed -i '/^# >>> subyard-openclaw >>>$/,/^# <<< subyard-openclaw <<<$/d' "$npmrc" 2>/dev/null || true
  { echo "# >>> subyard-openclaw >>>"; echo "cache=$CACHE_NPM"; echo "# <<< subyard-openclaw <<<"; } >> "$npmrc"
  chown "$DEV_USER:$DEV_USER" "$npmrc"
fi
if [ -n "$CACHE_PIP" ]; then
  pipdir="$DEV_HOME/.config/pip"; install -d -o "$DEV_USER" -g "$DEV_USER" "$pipdir"
  printf '# GENERATED by Subyard openclaw provision.sh — shared pip cache.\n[global]\ncache-dir = %s\n' "$CACHE_PIP" > "$pipdir/pip.conf"
  chown "$DEV_USER:$DEV_USER" "$pipdir/pip.conf"
fi

# 4c. The few things that genuinely ARE env (no tool-config equivalent): the docs pointer, and the
#     Playwright browser-cache path when browser_tests is on. Neither affects npm, so neither warns.
#     Login shells via /etc/profile.d; non-login `yard shell -- cmd` via /etc/environment (pam_env).
browser_on=0
case " ${OPTIONAL_FEATURES:-} " in *" browser_tests "*) browser_on=1 ;; esac
{
  echo "# Subyard openclaw (L1) — GENERATED by provision.sh; do not edit. Cache wiring lives in the"
  echo "# dev tool config (pnpm shim / ~/.npmrc / pip.conf), NOT here."
  if [ "$browser_on" = 1 ] && [ -n "$CACHE_PLAYWRIGHT" ]; then
    echo "export PLAYWRIGHT_BROWSERS_PATH=\"$CACHE_PLAYWRIGHT\""
    echo "export PLAYWRIGHT_SKIP_BROWSER_GC=1 # shared cache: never GC another agent's browser"
  fi
  echo "export SUBYARD_OPENCLAW_DOCS=/etc/subyard/openclaw-l1.md"
} > /etc/profile.d/subyard-openclaw.sh
chmod 0644 /etc/profile.d/subyard-openclaw.sh
rm -f /etc/profile.d/subyard-openclaw-caches.sh   # stale name from when caches were (wrongly) in env

# Same handful for non-login PAM sessions. Idempotent: drop our marker block, keep other entries.
sed -i '/^# >>> subyard-openclaw >>>$/,/^# <<< subyard-openclaw <<<$/d' /etc/environment 2>/dev/null || true
{
  echo "# >>> subyard-openclaw >>>"
  if [ "$browser_on" = 1 ] && [ -n "$CACHE_PLAYWRIGHT" ]; then
    echo "PLAYWRIGHT_BROWSERS_PATH=$CACHE_PLAYWRIGHT"
    echo "PLAYWRIGHT_SKIP_BROWSER_GC=1"
  fi
  echo "SUBYARD_OPENCLAW_DOCS=/etc/subyard/openclaw-l1.md"
  echo "# <<< subyard-openclaw <<<"
} >> /etc/environment

# 5. OPTIONAL_FEATURES (off by default) — the heavy e2e capability.
for feat in $OPTIONAL_FEATURES; do
  case "$feat" in
    browser_tests)
      apt-get update -qq
      apt-get install -y -qq chromium fonts-liberation fonts-noto-color-emoji \
        libnss3 libgbm1 libxss1 libasound2t64 2>/dev/null \
        || apt-get install -y -qq chromium fonts-liberation libnss3 libgbm1
      install -d -o "$DEV_USER" -g "$DEV_USER" /srv/cache/playwright
      ;;
    sandbox_tests)
      # rootless docker — --force needed (rootful coexists); overlayfs is the native storage driver.
      apt-get update -qq
      apt-get install -y -qq docker-ce-rootless-extras fuse-overlayfs slirp4netns \
        bubblewrap dbus-user-session uidmap
      loginctl enable-linger "$DEV_USER" >/dev/null 2>&1 || true
      uid="$(id -u "$DEV_USER")"
      runuser -u "$DEV_USER" -- env XDG_RUNTIME_DIR="/run/user/$uid" \
        dockerd-rootless-setuptool.sh install --force >/dev/null 2>&1 || true
      # setup flips dev's default context to rootless — restore it for shared rootful project environments.
      runuser -u "$DEV_USER" -- env XDG_RUNTIME_DIR="/run/user/$uid" \
        docker context use default >/dev/null 2>&1 || true
      ;;
    *) echo "openclaw provision: unknown OPTIONAL_FEATURE '$feat' — skipping" >&2 ;;
  esac
done

# 6. Discoverable L1 how-to (self-serve onboarding): one persistent, machine/human-readable doc the
#    agent can find on its own. Pointer is exported as SUBYARD_OPENCLAW_DOCS (step 4c); the consumer
#    repo's AGENTS.md/CLAUDE.md should also point here (operator recommendation, we don't edit it).
#    Persistent /etc (NOT tmpfs /run/subyard): provision is operator-run once, not re-run on reboot.
install -d -m 0755 /etc/subyard
cat > /etc/subyard/openclaw-l1.md <<'DOC'
# OpenClaw in this yard — L1 build / test / live (self-serve)

You are in an **L1 yard**: several non-isolated agents share ONE machine and ONE set of caches.
BUILD and the unit suite run from your own checkout with no extra setup. The LIVE lane (a real model
move) is operator-gated — see below. There is no gateway and no Telegram in this lane.

Exact script names below follow the project's own `package.json`; confirm them with `pnpm run` if a
command is reported missing (the vendored project moves).

## Build
Run from your project workspace (the checkout you were given):

    pnpm install --frozen-lockfile      # uses the shared store; first run is slow on a cold cache
    pnpm build

## Test suite

    pnpm test:unit:fast                 # fast unit suite

The fuller matrices (e2e / docker-in-docker / browser / sandbox) need optional features and a
loopback gateway or docker socket that this plain L1 lane does not start. Run them only where the
operator enabled OPTIONAL_FEATURES (browser_tests / sandbox_tests) for this yard.

## Live model move, no gateway (operator-gated)

`sy-stage` appears on PATH ONLY after the operator runs `yard staging up <zone>` for a staging zone.
If it is absent, the live lane is not provisioned here yet — build and the unit suite still work.
When present:

    sy-stage test -- <your live-test command>          # e.g. OPENCLAW_LIVE_TEST=1 pnpm test:live

It runs your command in the staging runner (cwd /workspace) and injects the host-config STAGING
provider key as `ANTHROPIC_API_KEY` for THAT ONE subprocess only — never your shell env, never a log.
That injected key is what un-skips the project's key-gated live tests; with no key they skip cleanly.

- The project's own live suites gate on the provider key and on the project's own switch (e.g.
  `OPENCLAW_LIVE_TEST=1`, plus per-suite `OPENCLAW_LIVE_*` knobs) — pass those in your command. They
  do NOT read `SUBYARD_LIVE_MODEL`; that flag is only a Subyard convenience signal (set to 1 when a
  key is present) for your own wrappers to branch on, not a project contract.
- The key is host-config only; it is never in commits, logs, or your shell.

## Shared caches (you must understand this)

All agents in this yard share `/srv/cache` (pnpm store, npm, pip[, playwright]). The cache locations
are preconfigured for you — pnpm's store via the `pnpm` wrapper, npm + pip via your `~/.npmrc` and
`~/.config/pip/pip.conf` — so they apply in every shell (login or `yard shell -- <cmd>`). Do NOT
override the store/cache dirs per checkout, or you fork the cache and re-download everything.

- Cache mutations self-serialize: run `sy-cache prune|clean|purge|all` (or even a raw `pnpm store
  prune` — the pnpm wrapper locks it for you). The pnpm STORE is guarded by an exclusive lock that
  waits for in-flight `pnpm install`/fetch (which hold a shared lock) and blocks new ones, so a prune
  never corrupts the store mid-fetch. The npm/pip caches are NOT shared-locked — don't run `sy-cache
  clean`/`purge` while an `npm install` / `pip install` is in flight.
- DO NOT prune the pnpm store. Your workspace is a bind-mounted host dir — a different MOUNT than the
  store — so pnpm cannot hardlink into `node_modules` (EXDEV) and copies instead. Nothing references
  the store, so a prune deletes ALL of it and every agent re-downloads. (`sy-cache prune` now refuses;
  a raw `pnpm store prune` does not.) Hence the store dedupes DOWNLOADS, not disk: each checkout
  carries its own full `node_modules` (OpenClaw: ~3.3 GB). See profile.conf for the sharing rules.

## This lane does NOT
- start a gateway or connect Telegram (that is the staging/qa lane, operator-provisioned);
- give you any master credential — a live key, if present, is injected per-run and you never see it.
DOC
chmod 0644 /etc/subyard/openclaw-l1.md

echo "openclaw provision OK: node=$(/usr/local/bin/node --version) pnpm=$(/usr/local/bin/pnpm --version 2>/dev/null) venv=$([ -x /opt/venv/bin/python ] && echo yes)"
# Operator recommendation (Slice 3.2): the self-serve how-to is at /etc/subyard/openclaw-l1.md and is
# discoverable via $SUBYARD_OPENCLAW_DOCS. We do NOT edit the consumer's repo — hand this to them so an
# agent given only a prompt finds the lane. Printed here because that is where the operator will see it.
echo "openclaw provision: agent how-to at /etc/subyard/openclaw-l1.md (env: SUBYARD_OPENCLAW_DOCS)."
echo "  RECOMMEND adding to the project's AGENTS.md/CLAUDE.md:"
echo "    Building/testing in a Subyard L1 yard? Read \$SUBYARD_OPENCLAW_DOCS (/etc/subyard/openclaw-l1.md)."
