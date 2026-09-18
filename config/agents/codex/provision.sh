#!/usr/bin/env bash
# Reconcile the current stable native Codex CLI without touching user authorization.
set -euo pipefail

INSTALL_ROOT="${CODEX_INSTALL_ROOT:-/opt/subyard/codex}"
RUNTIME_PATH="$INSTALL_ROOT/codex"
BIN_LINK="${CODEX_BIN_LINK:-/usr/local/bin/codex}"
CHECK_PATH="${CODEX_CHECK_PATH:-/usr/local/bin/codex-policy-check}"
REQUIREMENTS_PATH="${CODEX_REQUIREMENTS_PATH:-/etc/codex/requirements.toml}"
RELEASE_API_URL=https://api.github.com/repos/openai/codex/releases/latest
RELEASE_BASE_URL=https://github.com/openai/codex/releases/download
ARCH="${CODEX_ARCH:-}"
DEV_USER="${DEV_USER:-dev}"
MANAGED_CHECK_MARKER='# Managed by Subyard Codex provision.'

# Native requirements apply across projects, CODEX_HOME and client launch modes.
# Keep this in the self-contained provision hook copied into the yard.
requirements() {
  cat <<'REQUIREMENTS'
# Managed by Subyard Codex provision.
allowed_approval_policies = ["on-request"]
allowed_approvals_reviewers = ["user"]

[rules]
prefix_rules = [
  { pattern = [{ token = "git" }, { any_of = ["commit", "push"] }], decision = "prompt", justification = "Creating or amending commits and pushing require explicit operator approval." },
]
REQUIREMENTS
}

die() { printf 'Codex provision: %s\n' "$*" >&2; exit 1; }

if [ "$(id -u)" -ne 0 ] && [ "${CODEX_TEST_ALLOW_NON_ROOT:-0}" != 1 ]; then
  die "must run as root"
fi

for path in "$INSTALL_ROOT" "$BIN_LINK" "$CHECK_PATH" "$REQUIREMENTS_PATH"; do
  case "$path" in /*) ;; *) die "install paths must be absolute" ;; esac
done
case "$DEV_USER" in ''|*[!A-Za-z0-9._-]*) die "DEV_USER is invalid" ;; esac

if [ -z "$ARCH" ]; then
  command -v dpkg >/dev/null 2>&1 || die "dpkg is required to detect the architecture"
  ARCH="$(dpkg --print-architecture)"
fi
case "$ARCH" in
  amd64)
    TARGET=x86_64-unknown-linux-musl
    ;;
  arm64)
    TARGET=aarch64-unknown-linux-musl
    ;;
  *) die "unsupported architecture '$ARCH' (supported: amd64, arm64)" ;;
esac

command -v curl >/dev/null 2>&1 || die "curl is required"
command -v jq >/dev/null 2>&1 || die "jq is required"
command -v sort >/dev/null 2>&1 || die "sort is required"
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

is_working_binary() {
  local path="$1" output
  [ -f "$path" ] && [ ! -L "$path" ] && [ -x "$path" ] || return 1
  [ "$(stat -c '%a' "$path" 2>/dev/null)" = 755 ] || return 1
  [ "$(stat -c '%u:%g' "$path" 2>/dev/null)" = "$expected_owner" ] || return 1
  is_native_binary "$path" || return 1
  output="$("$path" --version 2>/dev/null)" || return 1
  [[ "$output" =~ ^codex-cli\ ([0-9]+\.[0-9]+\.[0-9]+([+-][0-9A-Za-z.-]+)?)$ ]] || return 1
  BINARY_VERSION="${BASH_REMATCH[1]}"
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
requirements_dir="$(dirname "$REQUIREMENTS_PATH")"
if [ -L "$requirements_dir" ] || { [ -e "$requirements_dir" ] && [ ! -d "$requirements_dir" ]; }; then
  die "$requirements_dir is not a regular directory — leaving it unchanged"
fi
if [ -L "$REQUIREMENTS_PATH" ] || { [ -e "$REQUIREMENTS_PATH" ] && [ ! -f "$REQUIREMENTS_PATH" ]; }; then
  die "$REQUIREMENTS_PATH is not a managed regular file — leaving it unchanged"
elif [ -f "$REQUIREMENTS_PATH" ] && ! grep -Fxq "$MANAGED_CHECK_MARKER" "$REQUIREMENTS_PATH"; then
  die "$REQUIREMENTS_PATH is managed by another policy — leaving it unchanged"
fi

install_requirements() {
  local requirements_stage
  install -d -m 0755 "$requirements_dir"
  requirements_stage="$(mktemp "$requirements_dir/.requirements.XXXXXX")"
  requirements > "$requirements_stage"
  chmod 0644 "$requirements_stage"
  if [ "$(id -u)" -eq 0 ]; then
    chown 0:0 "$requirements_dir" "$requirements_stage"
  fi
  mv -fT -- "$requirements_stage" "$REQUIREMENTS_PATH"
}

install_check() {
  local check_dir check_stage q_runtime q_link q_user q_owner q_requirements requirements_sha
  check_dir="$(dirname "$CHECK_PATH")"
  mkdir -p "$check_dir"
  check_stage="$(mktemp "$check_dir/.codex-policy-check.XXXXXX")"
  printf -v q_runtime '%q' "$RUNTIME_PATH"
  printf -v q_link '%q' "$BIN_LINK"
  printf -v q_user '%q' "$DEV_USER"
  printf -v q_owner '%q' "$expected_owner"
  printf -v q_requirements '%q' "$REQUIREMENTS_PATH"
  requirements_sha="$(requirements | sha256sum | cut -d' ' -f1)"
  cat > "$check_stage" <<EOF
#!/usr/bin/env bash
$MANAGED_CHECK_MARKER
set -euo pipefail
runtime=$q_runtime
link=$q_link
dev_user="\${CODEX_CHECK_DEV_USER:-$q_user}"
expected_owner=$q_owner
requirements=$q_requirements
die() { printf 'codex-policy-check: %s\\n' "\$*" >&2; exit 1; }
[ -f "\$requirements" ] && [ ! -L "\$requirements" ] \
  || die "managed approval requirements are missing"
[ ! -L "\$(dirname "\$requirements")" ] \
  && [ "\$(stat -c '%a:%u:%g' "\$(dirname "\$requirements")")" = "755:\$expected_owner" ] \
  || die "managed approval requirements directory drifted"
[ "\$(stat -c '%a:%u:%g' "\$requirements")" = "644:\$expected_owner" ] \
  || die "managed approval requirements permissions drifted"
[ "\$(sha256sum "\$requirements" | cut -d' ' -f1)" = '$requirements_sha' ] \
  || die "managed approval requirements drifted; run yard init"
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
output="\$(run_as_dev "\$runtime" --version 2>/dev/null)" || die "runtime version check failed"
[[ "\$output" =~ ^codex-cli\ ([0-9]+\.[0-9]+\.[0-9]+([+-][0-9A-Za-z.-]+)?)\$ ]] \
  || die "runtime version response is invalid"
version="\${BASH_REMATCH[1]}"
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

tmp="$(mktemp -d)"
stage=""
cleanup() {
  rm -rf "$tmp"
  [ -z "$stage" ] || rm -f "$stage"
}
trap cleanup EXIT

artifact="codex-${TARGET}.tar.gz"
entry="codex-${TARGET}"
metadata="$tmp/release.json"
# GitHub supplies each official release asset's SHA-256 digest. Bind the tag,
# filename, URL and digest from one response; never fall back to an unchecked URL.
curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
  --connect-timeout 20 --max-time 60 --header 'Accept: application/vnd.github+json' \
  --output "$metadata" "$RELEASE_API_URL" || die "latest release metadata download failed"
release="$(jq -ers --arg artifact "$artifact" --arg base "$RELEASE_BASE_URL" '
  if length == 1 then .[0] else error("expected one release") end |
  select(.draft == false and .prerelease == false) |
  .tag_name as $tag |
  select($tag | test("^rust-v(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)$")) |
  [.assets[] | select(.name == $artifact)] |
  select(length == 1) | .[0] |
  select(.state == "uploaded") |
  select(.browser_download_url == ($base + "/" + $tag + "/" + $artifact)) |
  select(.digest | test("^sha256:[0-9a-f]{64}$")) |
  [($tag | ltrimstr("rust-v")), (.digest | ltrimstr("sha256:")), .browser_download_url] |
  @tsv
' "$metadata" 2>/dev/null)" || die "latest release metadata lacks a verified stable artifact for $ARCH"
IFS=$'\t' read -r VERSION EXPECTED_SHA256 url <<< "$release"

if is_working_binary "$RUNTIME_PATH"; then
  # A stale latest-release response must not roll back a newer working install.
  current_base="${BINARY_VERSION%%[-+]*}"
  newest_base="$(printf '%s\n' "$current_base" "$VERSION" | sort -V | tail -n 1)"
  if [ "$BINARY_VERSION" = "$VERSION" ] || \
    { [ "$current_base" != "$VERSION" ] && [ "$newest_base" = "$current_base" ]; } || \
    { [ "$current_base" = "$VERSION" ] && [[ "$BINARY_VERSION" = "$VERSION"+* ]]; }; then
    publish_command
    install_requirements
    install_check
    "$CHECK_PATH" >/dev/null
    printf 'Codex %s is already current at %s\n' "$BINARY_VERSION" "$BIN_LINK"
    exit 0
  fi
fi

archive="$tmp/$artifact"
curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
  --connect-timeout 20 --max-time 300 --output "$archive" "$url" \
  || die "release artifact download failed"

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
is_working_binary "$stage" && [ "$BINARY_VERSION" = "$VERSION" ] \
  || die "staged binary failed version or app-server validation for '$VERSION'"
mv -fT -- "$stage" "$RUNTIME_PATH"
stage=""

publish_command
install_requirements
install_check
is_working_binary "$RUNTIME_PATH" && [ "$BINARY_VERSION" = "$VERSION" ] \
  || die "installed runtime verification failed"
"$CHECK_PATH" >/dev/null
printf 'Installed Codex %s at %s\n' "$VERSION" "$BIN_LINK"
