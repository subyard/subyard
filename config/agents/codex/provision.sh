#!/usr/bin/env bash
# Reconcile the pinned native Codex CLI without touching user authorization.
set -euo pipefail

VERSION="${CODEX_VERSION:-}"
SHA256_AMD64="${CODEX_SHA256_AMD64:-}"
SHA256_ARM64="${CODEX_SHA256_ARM64:-}"
INSTALL_ROOT="${CODEX_INSTALL_ROOT:-/opt/subyard/codex}"
RUNTIME_PATH="$INSTALL_ROOT/codex"
BIN_LINK="${CODEX_BIN_LINK:-/usr/local/bin/codex}"
CHECK_PATH="${CODEX_CHECK_PATH:-/usr/local/bin/codex-check}"
RELEASE_BASE_URL="${CODEX_RELEASE_BASE_URL:-https://github.com/openai/codex/releases/download}"
ARCH="${CODEX_ARCH:-}"
DEV_USER="${DEV_USER:-dev}"
MANAGED_CHECK_MARKER='# Managed by Subyard Codex provision.'

die() { printf 'Codex provision: %s\n' "$*" >&2; exit 1; }

if [ "$(id -u)" -ne 0 ] && [ "${CODEX_TEST_ALLOW_NON_ROOT:-0}" != 1 ]; then
  die "must run as root"
fi

[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+([+-][0-9A-Za-z.-]+)?$ ]] \
  || die "CODEX_VERSION must be an exact version (not '${VERSION:-empty}')"
[[ "$SHA256_AMD64" =~ ^[0-9a-f]{64}$ ]] \
  || die "CODEX_SHA256_AMD64 must be a lowercase SHA-256"
[[ "$SHA256_ARM64" =~ ^[0-9a-f]{64}$ ]] \
  || die "CODEX_SHA256_ARM64 must be a lowercase SHA-256"
for path in "$INSTALL_ROOT" "$BIN_LINK" "$CHECK_PATH"; do
  case "$path" in /*) ;; *) die "install paths must be absolute" ;; esac
done
[ -n "$RELEASE_BASE_URL" ] || die "CODEX_RELEASE_BASE_URL is empty"
case "$DEV_USER" in ''|*[!A-Za-z0-9._-]*) die "DEV_USER is invalid" ;; esac

if [ -z "$ARCH" ]; then
  command -v dpkg >/dev/null 2>&1 || die "dpkg is required to detect the architecture"
  ARCH="$(dpkg --print-architecture)"
fi
case "$ARCH" in
  amd64)
    TARGET=x86_64-unknown-linux-musl
    EXPECTED_SHA256="$SHA256_AMD64"
    ;;
  arm64)
    TARGET=aarch64-unknown-linux-musl
    EXPECTED_SHA256="$SHA256_ARM64"
    ;;
  *) die "unsupported architecture '$ARCH' (supported: amd64, arm64)" ;;
esac

command -v curl >/dev/null 2>&1 || die "curl is required"
command -v sha256sum >/dev/null 2>&1 || die "sha256sum is required"
command -v tar >/dev/null 2>&1 || die "tar is required"
command -v od >/dev/null 2>&1 || die "od is required"
command -v stat >/dev/null 2>&1 || die "stat is required"

expected_owner="$(id -u):$(id -g)"
if [ "$(id -u)" -eq 0 ]; then expected_owner=0:0; fi

is_native_binary() {
  local magic
  magic="$(LC_ALL=C od -An -tx1 -N4 "$1" 2>/dev/null | tr -d '[:space:]')"
  [ "$magic" = 7f454c46 ]
}

is_expected_binary() {
  local path="$1" output
  [ -f "$path" ] && [ ! -L "$path" ] && [ -x "$path" ] || return 1
  [ "$(stat -c '%a' "$path" 2>/dev/null)" = 755 ] || return 1
  [ "$(stat -c '%u:%g' "$path" 2>/dev/null)" = "$expected_owner" ] || return 1
  is_native_binary "$path" || return 1
  output="$("$path" --version 2>/dev/null)" || return 1
  [ "$output" = "codex-cli $VERSION" ] || return 1
  "$path" app-server --help >/dev/null 2>&1
}

# Never replace a command or package check that is not visibly ours.
if [ -L "$BIN_LINK" ]; then
  [ "$(readlink "$BIN_LINK")" = "$RUNTIME_PATH" ] \
    || die "$BIN_LINK is managed by another install — leaving it unchanged"
elif [ -e "$BIN_LINK" ]; then
  die "$BIN_LINK already exists outside the Subyard-managed runtime — leaving it unchanged"
fi
if [ -L "$CHECK_PATH" ] || { [ -e "$CHECK_PATH" ] && [ ! -f "$CHECK_PATH" ]; }; then
  die "$CHECK_PATH is not a managed regular file — leaving it unchanged"
elif [ -f "$CHECK_PATH" ] && ! grep -Fxq "$MANAGED_CHECK_MARKER" "$CHECK_PATH"; then
  die "$CHECK_PATH is managed by another install — leaving it unchanged"
fi
if [ -L "$INSTALL_ROOT" ] || { [ -e "$INSTALL_ROOT" ] && [ ! -d "$INSTALL_ROOT" ]; }; then
  die "$INSTALL_ROOT is not a managed directory — leaving it unchanged"
fi

install_check() {
  local check_dir check_stage q_runtime q_link q_version q_user q_owner
  check_dir="$(dirname "$CHECK_PATH")"
  mkdir -p "$check_dir"
  check_stage="$(mktemp "$check_dir/.codex-check.XXXXXX")"
  printf -v q_runtime '%q' "$RUNTIME_PATH"
  printf -v q_link '%q' "$BIN_LINK"
  printf -v q_version '%q' "$VERSION"
  printf -v q_user '%q' "$DEV_USER"
  printf -v q_owner '%q' "$expected_owner"
  cat > "$check_stage" <<EOF
#!/usr/bin/env bash
$MANAGED_CHECK_MARKER
set -euo pipefail
runtime=$q_runtime
link=$q_link
version=$q_version
dev_user="\${CODEX_CHECK_DEV_USER:-$q_user}"
expected_owner=$q_owner
die() { printf 'codex-check: %s\\n' "\$*" >&2; exit 1; }
[ -f "\$runtime" ] && [ ! -L "\$runtime" ] && [ -x "\$runtime" ] \
  || die "managed runtime is missing"
[ "\$(stat -c '%a' "\$runtime" 2>/dev/null)" = 755 ] || die "managed runtime mode is not 0755"
[ "\$(stat -c '%u:%g' "\$runtime" 2>/dev/null)" = "\$expected_owner" ] \
  || die "managed runtime owner drifted"
[ -L "\$link" ] && [ "\$(readlink "\$link")" = "\$runtime" ] \
  || die "canonical command does not point to the managed runtime"
dev_home="\$(getent passwd "\$dev_user" | cut -d: -f6)"
[ -n "\$dev_home" ] || die "could not resolve home for \$dev_user"
run_as_dev() {
  if [ "\$(id -un)" = "\$dev_user" ]; then
    env HOME="\$dev_home" CODEX_HOME="\$dev_home/.codex" "\$@"
  else
    command -v runuser >/dev/null 2>&1 || die "runuser is required"
    runuser -u "\$dev_user" -- env HOME="\$dev_home" CODEX_HOME="\$dev_home/.codex" "\$@"
  fi
}
[ "\$(run_as_dev "\$runtime" --version 2>/dev/null)" = "codex-cli \$version" ] \
  || die "runtime version does not match \$version"
run_as_dev "\$runtime" app-server --help >/dev/null 2>&1 \
  || die "Codex app-server readiness check failed"
printf 'codex %s ready\\n' "\$version"
EOF
  chmod 0755 "$check_stage"
  if [ "$(id -u)" -eq 0 ]; then chown 0:0 "$check_stage"; fi
  mv -fT -- "$check_stage" "$CHECK_PATH"
}

publish_command() {
  mkdir -p "$(dirname "$BIN_LINK")"
  if [ ! -L "$BIN_LINK" ]; then ln -s "$RUNTIME_PATH" "$BIN_LINK"; fi
  [ "$(readlink "$BIN_LINK")" = "$RUNTIME_PATH" ] \
    || die "canonical command verification failed"
}

install -d -m 0755 "$INSTALL_ROOT"
if [ "$(id -u)" -eq 0 ]; then chown 0:0 "$INSTALL_ROOT"; fi

if is_expected_binary "$RUNTIME_PATH"; then
  publish_command
  install_check
  "$CHECK_PATH" >/dev/null
  printf 'Codex %s is already installed at %s\n' "$VERSION" "$BIN_LINK"
  exit 0
fi

tmp="$(mktemp -d)"
stage=""
cleanup() {
  rm -rf "$tmp"
  [ -z "$stage" ] || rm -f "$stage"
}
trap cleanup EXIT

artifact="codex-${TARGET}.tar.gz"
entry="codex-${TARGET}"
url="${RELEASE_BASE_URL%/}/rust-v${VERSION}/${artifact}"
archive="$tmp/$artifact"
curl --fail --silent --show-error --location --output "$archive" "$url" \
  || die "download failed: $url"

actual_sha256="$(sha256sum "$archive" | cut -d' ' -f1)"
[ "$actual_sha256" = "$EXPECTED_SHA256" ] \
  || die "checksum mismatch for Codex $VERSION ($ARCH)"
mapfile -t members < <(tar -tzf "$archive" 2>/dev/null) \
  || die "Codex archive is invalid"
[ "${#members[@]}" -eq 1 ] && [ "${members[0]}" = "$entry" ] \
  || die "Codex archive must contain only $entry"
mkdir "$tmp/extract"
tar -xzf "$archive" -C "$tmp/extract" "$entry" 2>/dev/null \
  || die "$entry is missing from the archive"
source_bin="$tmp/extract/$entry"
[ -f "$source_bin" ] && [ ! -L "$source_bin" ] \
  || die "$entry is not a regular file"
is_native_binary "$source_bin" || die "$entry is not a native Linux binary"

# Validate fully before the atomic rename so a bad release cannot break the old CLI.
stage="$(mktemp "$INSTALL_ROOT/.codex.XXXXXX")"
install -m 0755 "$source_bin" "$stage"
if [ "$(id -u)" -eq 0 ]; then chown 0:0 "$stage"; fi
is_expected_binary "$stage" \
  || die "staged binary failed version or app-server validation for '$VERSION'"
mv -fT -- "$stage" "$RUNTIME_PATH"
stage=""

publish_command
install_check
is_expected_binary "$RUNTIME_PATH" || die "installed runtime verification failed"
"$CHECK_PATH" >/dev/null
printf 'Installed Codex %s at %s\n' "$VERSION" "$BIN_LINK"
