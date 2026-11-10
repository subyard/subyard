#!/usr/bin/env bash
# Install the latest native Claude CLI and mandatory rules without touching authorization.
set -euo pipefail

INSTALL_ROOT="${CLAUDE_INSTALL_ROOT:-/opt/subyard/claude}"
RUNTIME_PATH="$INSTALL_ROOT/claude"
BIN_LINK="${CLAUDE_BIN_LINK:-/usr/local/bin/claude}"
RELEASE_BASE_URL=https://downloads.claude.ai/claude-code-releases
ARCH="${CLAUDE_ARCH:-}"
POLICY_DIR="${CLAUDE_POLICY_DIR:-/etc/claude-code/managed-settings.d}"
CHECK_PATH="${CLAUDE_POLICY_CHECK_PATH:-/usr/local/bin/claude-policy-check}"
MARKER='Subyard Claude permission policy'
DEV_USER="${DEV_USER:-dev}"
DEV_HOME="${CLAUDE_DEV_HOME:-}"
# Hermetic non-root callers name their fixture home; only root provisioning resolves the account.
if [ -z "$DEV_HOME" ] && [ "$(id -u)" = 0 ]; then
  DEV_HOME="$(getent passwd "$DEV_USER" | cut -d: -f6 || true)"
fi
expected_owner="$(id -u):$(id -g)"
die() { printf 'Claude policy: %s\n' "$*" >&2; exit 1; }
[ "$(id -u)" = 0 ] || [ "${CLAUDE_POLICY_TEST_ALLOW_NON_ROOT:-0}" = 1 ] || die 'must run as root'
for path in "$INSTALL_ROOT" "$BIN_LINK"; do
  case "$path" in /*) ;; *) die 'install paths must be absolute' ;; esac
done
case "$POLICY_DIR:$CHECK_PATH" in /*:/*) ;; *) die 'policy paths must be absolute' ;; esac
for path in "$INSTALL_ROOT" "$(dirname "$BIN_LINK")" "$(dirname "$CHECK_PATH")" "$(dirname "$POLICY_DIR")" "$POLICY_DIR"; do
  if [ -L "$path" ] || { [ -e "$path" ] && [ ! -d "$path" ]; }; then
    die 'managed directory is not a regular directory'
  fi
  if [ -d "$path" ]; then
    [ "$(stat -c '%u:%g' "$path")" = "$expected_owner" ] || die 'managed directory has a foreign owner'
    permissions="$(stat -c '%a' "$path")"
    (( (8#$permissions & 0022) == 0 )) || die 'managed directory is writable by another user'
  fi
done
policy="$POLICY_DIR/50-subyard-git-approval.json"
for path in "$policy" "$CHECK_PATH"; do
  if [ -L "$path" ] || { [ -e "$path" ] && [ ! -f "$path" ]; }; then
    die 'managed path is not a regular file'
  fi
  if [ -f "$path" ]; then
    grep -Fq "$MARKER" "$path" || die 'existing file belongs to another policy'
  fi
done
if [ -L "$BIN_LINK" ]; then
  [ "$(readlink "$BIN_LINK")" = "$RUNTIME_PATH" ] || die 'canonical command belongs to another installation'
elif [ -e "$BIN_LINK" ]; then
  die 'canonical command belongs to another installation'
fi
if [ -L "$RUNTIME_PATH" ] || { [ -e "$RUNTIME_PATH" ] && [ ! -f "$RUNTIME_PATH" ]; }; then
  die 'managed runtime is not a regular file'
fi
for tool in curl jq sha256sum sort od timeout; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool is required"
done
if [ -z "$ARCH" ]; then ARCH="$(dpkg --print-architecture)"; fi
case "$ARCH" in
  amd64) platform=linux-x64 ;;
  arm64) platform=linux-arm64 ;;
  *) die "unsupported architecture '$ARCH' (supported: amd64, arm64)" ;;
esac

is_working_binary() {
  local path="$1" output magic directory
  directory="$(dirname "$path")"
  [ -d "$directory" ] && [ ! -L "$directory" ] || return 1
  [ "$(stat -c '%a:%u:%g' "$directory")" = "755:$expected_owner" ] || return 1
  [ -f "$path" ] && [ ! -L "$path" ] && [ -x "$path" ] || return 1
  [ "$(stat -c '%a:%u:%g' "$path")" = "755:$expected_owner" ] || return 1
  magic="$(LC_ALL=C od -An -tx1 -N4 "$path" | tr -d '[:space:]')"
  [ "$magic" = 7f454c46 ] || return 1
  output="$(timeout 30 "$path" --version 2>/dev/null)" || return 1
  [[ "$output" =~ ^([0-9]+\.[0-9]+\.[0-9]+)\ \(Claude\ Code\)$ ]] || return 1
  BINARY_VERSION="${BASH_REMATCH[1]}"
}

# Developer-tree repairs apply only where the developer can make them; anything else is a conflict.
dev_owned_directory() {
  [ -d "$1" ] && [ ! -L "$1" ] && [ "$(stat -c '%U' "$1")" = "$DEV_USER" ] \
    && (( (8#$(stat -c '%a' "$1") & 0200) != 0 ))
}

# Claude's native installer publishes <home>/.local/bin/claude -> <data home>/claude/versions/X.Y.Z
# on its first update. Only that exact shape can be released: it shadows the managed runtime in
# login shells, while its version files stay in place for any session still running them.
native_shadow_state() {
  local link="$DEV_HOME/.local/bin/claude" target
  [ -n "$DEV_HOME" ] || { printf 'absent'; return; }
  if [ ! -L "$link" ]; then
    if [ -e "$link" ]; then printf 'foreign'; else printf 'absent'; fi
    return
  fi
  target="$(readlink "$link")"
  if [[ "$target" =~ ^/([^/]+/)*claude/versions/[0-9]+\.[0-9]+\.[0-9]+$ ]] \
    && [[ "$target" != */../* ]] && [ "$(stat -c '%U' "$link")" = "$DEV_USER" ] \
    && dev_owned_directory "$(dirname "$link")" \
    && { [ ! -e "$target" ] || { [ -f "$target" ] && [ ! -L "$target" ] && [ "$(stat -c '%U' "$target")" = "$DEV_USER" ]; }; }; then
    printf 'native'
  else
    printf 'foreign'
  fi
}
release_native_shadow() {
  [ "$(native_shadow_state)" = native ] || return 0
  local -a remove=(rm -f -- "$DEV_HOME/.local/bin/claude")
  [ "$(id -u)" != 0 ] || remove=(runuser -u "$DEV_USER" -- "${remove[@]}")
  if "${remove[@]}"; then
    printf 'Released the native self-installed claude link from %s/.local/bin\n' "$DEV_HOME"
  else
    printf 'Conflict: could not remove %s/.local/bin/claude.\n' "$DEV_HOME"
  fi
}

tmp="$(mktemp -d)"
stage=""
runtime_stage=""
cleanup() {
  rm -rf -- "$tmp"
  [ -z "$stage" ] || rm -f -- "$stage"
  [ -z "$runtime_stage" ] || rm -f -- "$runtime_stage"
}
trap cleanup EXIT
download() {
  curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
    --retry 3 --connect-timeout 20 --max-time 300 --output "$2" "$1"
}
download "$RELEASE_BASE_URL/latest" "$tmp/latest" || die 'latest version download failed'
VERSION="$(cat "$tmp/latest")"
[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die 'invalid latest release version'
download "$RELEASE_BASE_URL/$VERSION/manifest.json" "$tmp/manifest.json" || die 'release manifest download failed'
EXPECTED_SHA256="$(jq -ers --arg version "$VERSION" --arg platform "$platform" '
  if length == 1 then .[0] else error("expected one manifest") end |
  select(.version == $version) | .platforms[$platform] |
  select(.binary == "claude") | .checksum | select(test("^[0-9a-f]{64}$"))
' "$tmp/manifest.json" 2>/dev/null)" || die "manifest lacks a verified artifact for $ARCH"

install -d -m 0755 "$INSTALL_ROOT" "$(dirname "$BIN_LINK")"
current=0
if is_working_binary "$RUNTIME_PATH"; then
  # A stale latest pointer must not downgrade a newer working installation.
  newest="$(printf '%s\n' "$BINARY_VERSION" "$VERSION" | sort -V | tail -n 1)"
  [ "$newest" != "$BINARY_VERSION" ] || current=1
fi
if [ "$current" = 0 ]; then
  download "$RELEASE_BASE_URL/$VERSION/$platform/claude" "$tmp/claude" || die 'release artifact download failed'
  [ "$(sha256sum "$tmp/claude" | cut -d' ' -f1)" = "$EXPECTED_SHA256" ] || die "checksum mismatch for Claude $VERSION ($ARCH)"
  runtime_stage="$(mktemp "$INSTALL_ROOT/.claude.XXXXXX")"
  install -m 0755 "$tmp/claude" "$runtime_stage"
  is_working_binary "$runtime_stage" && [ "$BINARY_VERSION" = "$VERSION" ] || die 'staged CLI failed version validation'
fi
# The approval policy precedes the binary it governs and every developer-tree repair.
install -d -m 0755 "$(dirname "$POLICY_DIR")" "$POLICY_DIR" "$(dirname "$CHECK_PATH")"
stage="$(mktemp "$POLICY_DIR/.subyard.XXXXXX")"
cat > "$stage" <<'JSON'
{
  "$comment": "Subyard Claude permission policy",
  "env": {"DISABLE_UPDATES": "1"},
  "permissions": {
    "ask": ["Bash(git commit)", "Bash(git commit:*)", "Bash(git push)", "Bash(git push:*)"]
  }
}
JSON
chmod 0644 "$stage"
mv -fT -- "$stage" "$policy"
stage=""
if [ -n "$runtime_stage" ]; then
  mv -fT -- "$runtime_stage" "$RUNTIME_PATH"
  runtime_stage=""
fi
if [ ! -L "$BIN_LINK" ]; then ln -s "$RUNTIME_PATH" "$BIN_LINK"; fi
stage="$(mktemp "$(dirname "$CHECK_PATH")/.claude-policy-check.XXXXXX")"
{
  printf '#!/usr/bin/env bash\n# %s\nset -euo pipefail\n' "$MARKER"
  printf 'policy=%q\npolicy_dir=%q\nexpected_owner=%q\n' "$policy" "$POLICY_DIR" "$(id -u):$(id -g)"
  printf 'runtime=%q\nlink=%q\n' "$RUNTIME_PATH" "$BIN_LINK"
  printf 'DEV_USER=%q\nDEV_HOME=%q\n' "$DEV_USER" "$DEV_HOME"
  declare -f is_working_binary dev_owned_directory native_shadow_state
  cat <<'CHECK'
# Report every refusal that does not depend on a failed read, in this order.
refusals=0
refuse() { printf 'claude-policy-check: %s\n' "$*" >&2; refusals=$((refusals + 1)); }
[ ! -L "$policy_dir" ] && [ "$(stat -c '%a:%u:%g' "$policy_dir" 2>/dev/null)" = "755:$expected_owner" ] \
  || refuse 'managed policy directory drifted; run yard init'
if [ -f "$policy" ] && [ ! -L "$policy" ]; then
  [ "$(stat -c '%a:%u:%g' "$policy")" = "644:$expected_owner" ] \
    || refuse 'managed policy permissions drifted; run yard init'
  jq -e '. == {"$comment":"Subyard Claude permission policy","env":{"DISABLE_UPDATES":"1"},"permissions":{"ask":["Bash(git commit)","Bash(git commit:*)","Bash(git push)","Bash(git push:*)"]}}' "$policy" >/dev/null 2>&1 \
    || refuse 'managed policy drifted or could not be read; run yard init'
else
  refuse 'managed policy is missing; run yard init'
fi
[ -L "$link" ] && [ "$(readlink "$link")" = "$runtime" ] \
  || refuse 'canonical command does not point to the managed runtime; run yard init'
is_working_binary "$runtime" || refuse 'managed runtime failed validation; run yard init'
shadow="$(native_shadow_state)" || shadow=foreign
[ "$shadow" != native ] || refuse "a native self-installed link shadows $link; run yard init"
if [ "$refusals" = 0 ]; then
  printf 'Claude %s ready. ' "$BINARY_VERSION"
  printf 'Managed Claude Git ask rules and update lock installed; inspect /status and /permissions in the active client.\n'
fi
if [ "$shadow" = foreign ]; then
  printf 'Conflict: %s/.local/bin/claude is not Subyard-owned and was left unchanged; login shells can run it instead of %s.\n' "$DEV_HOME" "$link"
fi
[ "$refusals" = 0 ]
CHECK
} > "$stage"
chmod 0755 "$stage"
mv -fT -- "$stage" "$CHECK_PATH"
stage=""
release_native_shadow
# The integration adapter reruns the check and reports readiness once; keep only its conflicts here.
check_status=0
check_output="$("$CHECK_PATH")" || check_status=$?
printf '%s\n' "$check_output" | grep '^Conflict:' || true
[ "$check_status" = 0 ]

