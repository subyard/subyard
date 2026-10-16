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

PROFILE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

DEV_HOME="${OPENCLAW_DEV_HOME:-$(getent passwd "$DEV_USER" | cut -d: -f6)}"
: "${DEV_HOME:=/home/$DEV_USER}"

render_browser_policy() {
  cat <<'SH'
# A bounded global-option scan locates the native command without flattening argv.
command_args=("$@")
browser_project="$PWD"
ambiguous_browser=0
while [ "${#command_args[@]}" -gt 0 ]; do
  option="${command_args[0]}"
  case "$option" in
    --) command_args=("${command_args[@]:1}"); break ;;
    --dir|-C|--prefix)
      [ "${#command_args[@]}" -ge 2 ] || { echo 'cache: missing global option value' >&2; exit 1; }
      browser_project="${command_args[1]}"; command_args=("${command_args[@]:2}") ;;
    --dir=*|--prefix=*) browser_project="${option#*=}"; command_args=("${command_args[@]:1}") ;;
    -C?*) browser_project="${option:2}"; command_args=("${command_args[@]:1}") ;;
    --filter|--filter-prod|-F|--workspace)
      ambiguous_browser=1; command_args=("${command_args[@]:2}") ;;
    -w)
      ambiguous_browser=1
      if [ "$manager" = pnpm ]; then command_args=("${command_args[@]:1}"); else command_args=("${command_args[@]:2}"); fi ;;
    --filter=*|--filter-prod=*|--workspace=*|--workspace-root|--workspaces|--ws|-r|--recursive|-F?*|-w?*)
      ambiguous_browser=1; command_args=("${command_args[@]:1}") ;;
    --package|-p) ambiguous_browser=1; command_args=("${command_args[@]:2}") ;;
    --package=*|-p?*|--global|-g|--global=*) ambiguous_browser=1; command_args=("${command_args[@]:1}") ;;
    --registry|--cache|--store-dir|--userconfig|--globalconfig|--location)
      command_args=("${command_args[@]:2}") ;;
    --*=*|-y|--yes|--no|--offline|--prefer-offline|--silent|--quiet)
      command_args=("${command_args[@]:1}") ;;
    -*) ambiguous_browser=1; command_args=("${command_args[@]:1}") ;;
    *) break ;;
  esac
done
# npm consumes selector options anywhere before its explicit command delimiter, including
# after the executable and arguments. npx leaves options after its first positional to the tool.
if [ "$manager" = npm ]; then
  for argument in "$@"; do
    case "$argument" in
      --) break ;;
      --package|-p|--package=*|-p?*|--global|-g|--global=*|-g?*|--workspace|-w|--workspace=*|--workspaces|--ws|-w?*) ambiguous_browser=1 ;;
    esac
  done
fi
browser_args=()
if [ "$manager" = npx ]; then
  browser_args=("${command_args[@]}")
else
  case "${command_args[0]:-}" in
    exec) browser_args=("${command_args[@]:1}") ;;
    x) [ "$manager" != npm ] || browser_args=("${command_args[@]:1}") ;;
    dlx) ambiguous_browser=1; browser_args=("${command_args[@]:1}") ;;
  esac
fi
while [ "${#browser_args[@]}" -gt 0 ]; do
  case "${browser_args[0]}" in
    --) browser_args=("${browser_args[@]:1}"); break ;;
    --dir|-C|--prefix) browser_project="${browser_args[1]:-}"; browser_args=("${browser_args[@]:2}") ;;
    --dir=*|--prefix=*) browser_project="${browser_args[0]#*=}"; browser_args=("${browser_args[@]:1}") ;;
    --workspace|--filter|--filter-prod|-F|-w) ambiguous_browser=1; browser_args=("${browser_args[@]:2}") ;;
    --workspace=*|--filter=*|--filter-prod=*|--workspaces|--ws|--workspace-root|-w?*|-F?*) ambiguous_browser=1; browser_args=("${browser_args[@]:1}") ;;
    --package|-p) ambiguous_browser=1; browser_args=("${browser_args[@]:2}") ;;
    --package=*|-p?*|--global|-g|--global=*) ambiguous_browser=1; browser_args=("${browser_args[@]:1}") ;;
    --registry|--cache|--store-dir|--userconfig|--globalconfig|--location)
      browser_args=("${browser_args[@]:2}") ;;
    --*=*|-y|--yes|--no|--offline|--prefer-offline|--silent|--quiet)
      browser_args=("${browser_args[@]:1}") ;;
    -*) ambiguous_browser=1; browser_args=("${browser_args[@]:1}") ;;
    *) break ;;
  esac
done
# Package selectors, global/dlx execution and workspace/unknown options cannot prove the
# executed CLI version from the local package; require an explicit versioned helper.
# Reject potential browser maintenance unless an explicit browser helper already chose the key.
if [ "$ambiguous_browser" -eq 1 ] && [ "${_SY_CACHE_MODE:-}" != exclusive ]; then
  possible_exec=0; possible_browser=0; possible_mutation=0
  [ "$manager" != npx ] || possible_exec=1
  for argument in "$@"; do
    case "$argument" in exec|x|dlx) possible_exec=1 ;; playwright|playwright@*|@playwright/test|@playwright/test@*) possible_browser=1 ;; install|uninstall) possible_mutation=1 ;; esac
  done
  if [ "$possible_exec$possible_browser$possible_mutation" = 111 ]; then
    echo 'use sy-cache browser VERSION -- CMD when browser CLI version is selected by options, dlx, or workspaces' >&2; exit 1
  fi
fi
browser_package="${browser_args[0]:-}"
if { [[ "$browser_package" = playwright || "$browser_package" = playwright@* || "$browser_package" = @playwright/test || "$browser_package" = @playwright/test@* ]]; } \
  && { [ "${browser_args[1]:-}" = install ] || [ "${browser_args[1]:-}" = uninstall ]; }; then
  mode=exclusive
  if [ "${_SY_CACHE_MODE:-}" != exclusive ]; then
    [ "$ambiguous_browser" -eq 0 ] || { echo 'use sy-cache browser VERSION -- CMD for workspace browser maintenance' >&2; exit 1; }
    [[ "$browser_package" = playwright ]] || { echo 'use sy-cache browser VERSION -- CMD for package-qualified browser installs' >&2; exit 1; }
    version="$(node -p 'require(require.resolve("playwright/package.json", {paths: [process.argv[1]]})).version' "$browser_project")"
    PLAYWRIGHT_BROWSERS_PATH="${SY_CACHE_BROWSER_ROOT:-/srv/cache/playwright}/$("$cache_tool" key browser "$version")"
    export PLAYWRIGHT_BROWSERS_PATH PLAYWRIGHT_SKIP_BROWSER_GC=1
  fi
fi
SH
}

render_pnpm_wrapper() {
  cat <<'SH'
#!/usr/bin/env bash
set -euo pipefail
SH
  printf 'sd=%q\n' "$CACHE_PNPM_STORE"
  printf 'pnpm_version=%q\n' "$PNPM_VERSION"
  cat <<'SH'
cache_tool="${SY_CACHE_TOOL:-/usr/local/bin/sy-cache}"
manager=pnpm
case "${1:-}" in --version|-v|--help|-h) exec corepack "pnpm@$pnpm_version" "$@" ;; esac
# One namespace per native ABI and pnpm release; pnpm owns the store's internal format version.
if [ -n "$sd" ]; then
  sd="$sd/$("$cache_tool" key pnpm "$pnpm_version" "node-$(node -p process.versions.modules)")"
fi
mode=shared
# Options may precede the verb. Conservatively lock every store command exclusively.
for argument in "$@"; do [ "$argument" != store ] || mode=exclusive; done
SH
  render_browser_policy
  cat <<'SH'

ft=()
case "${1:-}" in install|add|update|up|i|fetch) ft=(--fetch-timeout=1800000 --config.confirmModulesPurge=false) ;; esac
exec "$cache_tool" "$mode" -- corepack "pnpm@$pnpm_version" \
  --config.supportedArchitectures.os=linux \
  --config.supportedArchitectures.cpu=current \
  --config.supportedArchitectures.libc=glibc \
  ${sd:+--config.store-dir="$sd"} "$@" "${ft[@]}"
SH
}

render_npm_wrapper() {
  local entry="$1"
  cat <<'SH'
#!/usr/bin/env bash
set -euo pipefail
SH
  printf 'entry=%q\n' "$entry"
  cat <<'SH'
case "${1:-}" in --version|-v|--help|-h) exec node "$entry" "$@" ;; esac
mode=shared
# npm cache verify also removes entries; cache maintenance always takes the exclusive lock.
for argument in "$@"; do [ "$argument" != cache ] || mode=exclusive; done
cache_tool="${SY_CACHE_TOOL:-/usr/local/bin/sy-cache}"
manager=npm
case "$entry" in */npx-cli.js) manager=npx ;; esac
SH
  render_browser_policy
  cat <<'SH'

exec "${SY_CACHE_TOOL:-/usr/local/bin/sy-cache}" "$mode" -- node "$entry" "$@"
SH
}

repair_cache_lock() {
  local lock="$1" owner="$2"
  [ ! -L "$lock" ] || { echo 'openclaw provision: unsafe cache lock' >&2; return 1; }
  if [ ! -e "$lock" ]; then
    (set -o noclobber; : > "$lock") || return 1
  fi
  [ -f "$lock" ] || return 1
  # Keep the inode: existing installs can hold this lock during a provision repair.
  local repair_fd
  exec {repair_fd}<"$lock"
  flock -x -w 600 "$repair_fd"
  chown "$owner" "$lock"
  chmod 0644 "$lock"
  exec {repair_fd}<&-
}

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
  [ -x "$pnpm" ] && [ ! -L "$pnpm" ] \
    && cmp -s <(render_pnpm_wrapper) "$pnpm" \
    && [ "$($pnpm --version 2>/dev/null)" = "$PNPM_VERSION" ] || changed=1
  [ -x "$sy_cache" ] && cmp -s "$PROFILE_DIR/cache/sy-cache.sh" "$sy_cache" || changed=1
  for entry in npm npx; do
    wrapper="$(rooted /usr/local/bin/"$entry")"
    [ -x "$wrapper" ] && [ ! -L "$wrapper" ] \
      && cmp -s <(render_npm_wrapper "/usr/local/lib/node_modules/npm/bin/$entry-cli.js") "$wrapper" || changed=1
  done
  [ -x "$python" ] && [ -x "$pip" ] || changed=1
  site_dir="$("$python" -c 'import sysconfig; print(sysconfig.get_path("purelib"))' 2>/dev/null)" || { site_dir=; changed=1; }
  [ -f "$site_dir/subyard_cache.py" ] && [ ! -L "$site_dir/subyard_cache.py" ] \
    && cmp -s "$PROFILE_DIR/cache/subyard_cache.py" "$site_dir/subyard_cache.py" || changed=1
  [ -f "$site_dir/subyard_cache.pth" ] && [ ! -L "$site_dir/subyard_cache.pth" ] \
    && [ "$(cat "$site_dir/subyard_cache.pth")" = 'import subyard_cache' ] || changed=1
  [ -f "$site_dir/subyard_cache_root" ] && [ ! -L "$site_dir/subyard_cache_root" ] \
    && [ "$(cat "$site_dir/subyard_cache_root")" = "${CACHE_PIP:-/srv/cache/pip}" ] || changed=1
  expected_owner="$(id -u "$DEV_USER"):$(id -g "$DEV_USER")"
  lock="$(rooted /srv/cache/.sy-cache.lock)"
  [ -f "$lock" ] && [ ! -L "$lock" ] \
    && [ "$(stat -c '%u:%g:%a' "$lock" 2>/dev/null)" = "$expected_owner:644" ] || changed=1
  for cache in /srv/cache/pnpm /srv/cache/pip /srv/cache/npm /srv/cache/playwright; do
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
    && cmp -s "$PROFILE_DIR/openclaw-l1.md" "$docs" || changed=1
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
/usr/local/bin/node /usr/local/lib/node_modules/npm/bin/npm-cli.js install -g "corepack@${COREPACK_VERSION}" >/dev/null
# The profile supplies its own pnpm entry point. `corepack enable` rejects that regular file
# on a later apply; dispatching through `corepack pnpm@VERSION` needs no Corepack shim.
/usr/local/bin/corepack prepare "pnpm@${PNPM_VERSION}" --activate >/dev/null
# Replace the entry atomically: older provision runs left a symlink into Corepack's package.
# Writing through that link would overwrite Corepack's pnpm entry point.
pnpm_wrapper="$(mktemp /usr/local/bin/.subyard-pnpm.XXXXXX)"
render_pnpm_wrapper > "$pnpm_wrapper"
chmod 0755 "$pnpm_wrapper"
mv -Tf "$pnpm_wrapper" /usr/local/bin/pnpm

# 3. Python dev venv.
[ -x /opt/venv/bin/python ] || python3 -m venv /opt/venv
/opt/venv/bin/python -m pip --no-cache-dir install -q --upgrade pip setuptools wheel

# 4. Repair the existing lock before publishing helpers. Never replace its inode.
install -d -m 0755 /srv/cache
repair_cache_lock /srv/cache/.sy-cache.lock "$DEV_USER:$DEV_USER"
# Ownership repair is also a destructive shared-cache operation.
(
  flock -x -w 600 8
  install -d -o "$DEV_USER" -g "$DEV_USER" /srv/cache/pnpm /srv/cache/pip /srv/cache/npm /srv/cache/playwright
  chown -R "$DEV_USER:$DEV_USER" /srv/cache/pnpm /srv/cache/pip /srv/cache/npm /srv/cache/playwright
) 8</srv/cache/.sy-cache.lock
wrapper="$(mktemp /usr/local/bin/.subyard-cache.XXXXXX)"
install -m 0755 "$PROFILE_DIR/cache/sy-cache.sh" "$wrapper"
mv -Tf "$wrapper" /usr/local/bin/sy-cache
for entry in npm npx; do
  wrapper="$(mktemp /usr/local/bin/.subyard-npm.XXXXXX)"
  render_npm_wrapper "/usr/local/lib/node_modules/npm/bin/$entry-cli.js" > "$wrapper"
  chmod 0755 "$wrapper"
  mv -Tf "$wrapper" "/usr/local/bin/$entry"
done
# A .pth hook covers both pip launchers and python -m pip in this venv; sys.prefix stays intact.
site_dir="$(/opt/venv/bin/python -c 'import sysconfig; print(sysconfig.get_path("purelib"))')"
install -o root -g root -m 0644 "$PROFILE_DIR/cache/subyard_cache.py" "$site_dir/subyard_cache.py"
printf '%s\n' "${CACHE_PIP:-/srv/cache/pip}" > "$site_dir/subyard_cache_root"
chown root:root "$site_dir/subyard_cache_root"
chmod 0644 "$site_dir/subyard_cache_root"
printf 'import subyard_cache\n' > "$site_dir/subyard_cache.pth"
chown root:root "$site_dir/subyard_cache.pth"
chmod 0644 "$site_dir/subyard_cache.pth"

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
install -m 0644 "$PROFILE_DIR/openclaw-l1.md" /etc/subyard/openclaw-l1.md

echo "openclaw provision OK: node=$(/usr/local/bin/node --version) pnpm=$(/usr/local/bin/pnpm --version 2>/dev/null) venv=$([ -x /opt/venv/bin/python ] && echo yes)"
# Operator recommendation (Slice 3.2): the self-serve how-to is at /etc/subyard/openclaw-l1.md and is
# discoverable via $SUBYARD_OPENCLAW_DOCS. We do NOT edit the consumer's repo — hand this to them so an
# agent given only a prompt finds the lane. Printed here because that is where the operator will see it.
echo "openclaw provision: agent how-to at /etc/subyard/openclaw-l1.md (env: SUBYARD_OPENCLAW_DOCS)."
echo "  RECOMMEND adding to the project's AGENTS.md/CLAUDE.md:"
echo "    Building/testing in a Subyard L1 yard? Read \$SUBYARD_OPENCLAW_DOCS (/etc/subyard/openclaw-l1.md)."
