#!/usr/bin/env bash
# Bootstrap a host from release assets.
set -euo pipefail

REPOSITORY="${YARD_RELEASE_REPOSITORY:-Subyard/Subyard}"
CHANNEL=stable
VERSION="${YARD_RELEASE_VERSION:-}"
DATA_HOME="${SUBYARD_HOME:-$HOME/.subyard}"
CONFIG_HOME="${SUBYARD_CONFIG_HOME:-${XDG_CONFIG_HOME:-$HOME/.config}/subyard}"
RUNTIME_ROOT="${YARD_RUNTIME_ROOT:-$DATA_HOME/runtime}"
CACHE_ROOT="${YARD_RELEASE_CACHE:-$DATA_HOME/releases}"
BIN_DIR="${YARD_BIN_DIR:-$HOME/.local/bin}"
OFFLINE=0
PUBLISH_ONLY=0
ASSUME_YES="${ASSUME_YES:-0}"

# Package generation embeds the exact current publisher for older selected
# helpers. A source checkout uses that same script directly.
publisher_sha='@SUBYARD_PUBLISHER_SHA256@'
publisher_payload() {
  cat <<'SUBYARD_EMBEDDED_PUBLISHER'
# SUBYARD_EMBEDDED_PUBLISHER_CONTENT
SUBYARD_EMBEDDED_PUBLISHER
}

# Startup parents and the binary directory may be existing operator directories.
# Unlike install -d, mkdir never resets their permissions or ownership. Create
# only missing components, with private parents and an explicit final mode.
ensure_directory() {
  local directory="$1" mode="${2:-0700}" parent
  [ -d "$directory" ] && return 0
  if [ -e "$directory" ] || [ -L "$directory" ] || [ -z "$directory" ]; then
    printf 'bootstrap-runtime: directory path is unavailable\n' >&2
    return 2
  fi
  parent="$(dirname -- "$directory")"
  [ "$parent" != "$directory" ] || return 2
  ensure_directory "$parent" || return
  # A concurrent creator also owns its directory metadata; never chmod it.
  mkdir -m "$mode" -- "$directory" || [ -d "$directory" ]
}

validate_data_directory() {
  local mode
  [ -d "$DATA_HOME" ] && [ ! -L "$DATA_HOME" ] \
    && [ "$(stat -c '%u' "$DATA_HOME")" = "$(id -u)" ] || return 1
  mode="$(stat -c '%a' "$DATA_HOME")"
  (( (8#$mode & 0022) == 0 ))
}

while [ $# -gt 0 ]; do
  case "$1" in
    --channel) [ $# -ge 2 ] || { printf 'bootstrap-runtime: --channel needs stable\n' >&2; exit 2; }; CHANNEL="$2"; shift 2 ;;
    --version) [ $# -ge 2 ] || { printf 'bootstrap-runtime: --version needs a value\n' >&2; exit 2; }; VERSION="$2"; shift 2 ;;
    --runtime-root) [ $# -ge 2 ] || { printf 'bootstrap-runtime: --runtime-root needs a path\n' >&2; exit 2; }; RUNTIME_ROOT="$2"; shift 2 ;;
    --offline) OFFLINE=1; shift ;;
    --publish-only) PUBLISH_ONLY=1; shift ;;
    -y|--yes) ASSUME_YES=1; shift ;;
    -h|--help)
      printf 'Usage: bootstrap-runtime.sh [--version VERSION] [--offline] [--runtime-root PATH] [--publish-only] [--yes]\n'
      exit 0 ;;
    *) printf 'bootstrap-runtime: unknown option: %s\n' "$1" >&2; exit 2 ;;
  esac
done

# Validate existing installation ownership without changing the target.
if [ -e "$DATA_HOME" ] || [ -L "$DATA_HOME" ]; then
  validate_data_directory \
    || { printf 'bootstrap-runtime: unsafe installation data directory\n' >&2; exit 1; }
fi
[ "$PUBLISH_ONLY" = 0 ] || [ -n "$VERSION" ] \
  || { printf 'bootstrap-runtime: --publish-only requires an explicitly chosen --version\n' >&2; exit 2; }

[ "$CHANNEL" = stable ] || { printf 'bootstrap-runtime: unsupported channel: %s\n' "$CHANNEL" >&2; exit 2; }
case "$RUNTIME_ROOT" in /*) ;; *) printf 'bootstrap-runtime: runtime root must be absolute\n' >&2; exit 2 ;; esac
[ "$RUNTIME_ROOT" != / ] || { printf 'bootstrap-runtime: refusing filesystem root\n' >&2; exit 2; }
existing_runtime=0
if [ -e "$RUNTIME_ROOT/current" ] || [ -L "$RUNTIME_ROOT/current" ]; then
  existing_runtime=1
fi

case "$(uname -s)/$(uname -m)" in
  Linux/x86_64) os=linux; arch=amd64 ;;
  Linux/aarch64|Linux/arm64) os=linux; arch=arm64 ;;
  *) printf 'bootstrap-runtime: unsupported platform: %s/%s\n' "$(uname -s)" "$(uname -m)" >&2; exit 2 ;;
esac
for dependency in jq sha256sum tar gzip; do
  command -v "$dependency" >/dev/null 2>&1 \
    || { printf 'bootstrap-runtime: %s is required\n' "$dependency" >&2; exit 2; }
done
if [ "$OFFLINE" = 0 ]; then
  case "${YARD_RELEASE_BASE_URL:-}" in
    file://*) ;;
    *) command -v curl >/dev/null 2>&1 \
      || { printf 'bootstrap-runtime: curl is required\n' >&2; exit 2; } ;;
  esac
fi

latest_release_tag() (
  local headers response response_headers result status reset retry_after reason metadata_url tag path repository
  local redirects location expected_repository="$REPOSITORY"
  headers="$(mktemp /tmp/subyard-latest.XXXXXX)" || return 1
  trap 'rm -f -- "$headers"' EXIT
  # Like Codex provisioning, use the public redirect instead of the shared REST
  # quota. Follow redirects ourselves so every hop is checked before contact.
  metadata_url="https://github.com/$REPOSITORY/releases/latest"
  for ((redirects=0; redirects<10; redirects++)); do
    if response="$(curl --disable --fail --silent --show-error --head \
      --proto '=https' --tlsv1.2 --retry 0 --max-redirs 0 \
      --connect-timeout 20 --max-time 60 --dump-header "$headers" \
      --write-out '%{http_code}' --output /dev/null "$metadata_url")"; then
      status="$response"
    else
      result=$?
      status=''
      # Only curl's HTTP refusal exit identifies a failing HTTP response.
      [ "$result" != 22 ] || status="$response"
    fi
    # A redirect chain can carry unrelated quota headers; classify the final response.
    response_headers="$(awk '/^HTTP\// {block=""} {block=block $0 ORS} END {printf "%s", block}' "$headers")"
    case "$status" in
      200) break ;;
      301|302|303|307|308)
        location="$(awk 'tolower($1) == "location:" {
          sub(/^[^:]*:[ \t]*/, ""); sub(/\r$/, ""); value=$0; count++
        } END {if (count != 1) exit 1; print value}' <<< "$response_headers")" \
          || { printf 'bootstrap-runtime: latest release redirect needs one Location header\n' >&2; return 1; }
        case "$location" in
          /*) [[ "$location" == //* ]] || location="https://github.com$location" ;;
        esac
        if [[ "$location" != https://github.com/* || "$location" == *[$'\r\n\t ']* ]]; then
          printf 'bootstrap-runtime: latest release redirects must use https and stay on github.com\n' >&2
          return 1
        fi
        # GitHub canonicalizes renamed/transferred repositories via this endpoint.
        path="${location#https://github.com/}"
        if [[ "$path" =~ ^([^/?#]+)/([^/?#]+)/releases/latest$ ]]; then
          expected_repository="${BASH_REMATCH[1]}/${BASH_REMATCH[2]}"
        fi
        metadata_url="$location"
        continue ;;
    esac
    reason="could not resolve the stable release${status:+ (HTTP $status)}"
    if [ "$status" = 429 ] || { [ "$status" = 403 ] && \
      grep -Eiq $'^x-ratelimit-remaining: *0\r?$|^retry-after:' <<< "$response_headers"; }; then
      reset="$(awk 'tolower($1) == "x-ratelimit-reset:" {gsub(/\r/, "", $2); print $2}' <<< "$response_headers")"
      retry_after="$(awk 'tolower($1) == "retry-after:" {sub(/^[^:]*:[ \t]*/, ""); sub(/\r$/, ""); print}' <<< "$response_headers")"
      if [[ "$retry_after" =~ ^[0-9]+$ ]]; then retry_after="$retry_after seconds"; fi
      reason="GitHub rate limit refused latest release request (HTTP $status)"
      if [ -n "$reset" ]; then reason+=" (reset at Unix time $reset)"
      else reason+=" (reset time unavailable)"; fi
      reason+="${retry_after:+ (retry after $retry_after)}"
    fi
    printf 'bootstrap-runtime: %s\n' "$reason" >&2
    return 1
  done
  if [ "$redirects" = 10 ]; then
    printf 'bootstrap-runtime: too many latest release redirects\n' >&2
    return 1
  fi
  path="${metadata_url#https://github.com/}"
  repository="${path%/releases/tag/*}"
  tag="${path#"$repository/releases/tag/"}"
  if [[ "${repository,,}" != "${expected_repository,,}" || "$path" != "$repository/releases/tag/$tag" ]] || \
    ! [[ "$tag" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
    printf 'bootstrap-runtime: latest release redirect does not name a valid stable release tag\n' >&2
    return 1
  fi
  printf '%s\n' "$tag"
)

tag=""
if [ -z "$VERSION" ]; then
  [ "$OFFLINE" = 0 ] || { printf 'bootstrap-runtime: offline mode requires --version\n' >&2; exit 2; }
  tag="$(latest_release_tag)" || exit 1
  VERSION="${tag#v}"
else
  tag="${YARD_RELEASE_TAG:-v$VERSION}"
fi
case "$VERSION" in ''|*[!A-Za-z0-9._+-]*) printf 'bootstrap-runtime: unsafe version: %s\n' "$VERSION" >&2; exit 2 ;; esac

if [ "$PUBLISH_ONLY" = 0 ]; then
# Pick the files that new interactive and login shells actually read. The stable `current` link
# keeps completion valid across upgrades and rollback.
RC="${YARD_SHELL_RC:-}"
if [ -z "$RC" ]; then
  case "${SHELL:-}" in
    *zsh) RC="${ZDOTDIR:-$HOME}/.zshrc" ;;
    *) RC="$HOME/.bashrc" ;;
  esac
fi
LOGIN_RC="${YARD_LOGIN_RC:-}"
if [ -z "$LOGIN_RC" ]; then
  case "${SHELL:-}" in
    *zsh) LOGIN_RC="${ZDOTDIR:-$HOME}/.zprofile" ;;
    *)
      if [ -f "$HOME/.bash_profile" ]; then LOGIN_RC="$HOME/.bash_profile"
      elif [ -f "$HOME/.bash_login" ]; then LOGIN_RC="$HOME/.bash_login"
      else LOGIN_RC="$HOME/.profile"
      fi
      ;;
  esac
fi
case "${RC##*/}" in
  *zsh*) primary_shell=zsh ;;
  *bash*) primary_shell=bash ;;
  *) case "${SHELL:-}" in *zsh) primary_shell=zsh ;; *) primary_shell=bash ;; esac ;;
esac
completion="$RUNTIME_ROOT/current/completions/yard.$primary_shell"

# Configure both supported shells by default. Explicit startup-file overrides remain scoped to
# the selected files, including the bounded source-install migration entrypoints below.
shell_rcs=("$RC")
shell_completions=("$completion")
login_rcs=("$LOGIN_RC")
login_shells=("$primary_shell")
if [ -z "${YARD_SHELL_RC:-}" ]; then
  case "${SHELL:-}" in
    *zsh) shell_rcs+=("$HOME/.bashrc"); shell_completions+=("$RUNTIME_ROOT/current/completions/yard.bash") ;;
    *) shell_rcs+=("${ZDOTDIR:-$HOME}/.zshrc"); shell_completions+=("$RUNTIME_ROOT/current/completions/yard.zsh") ;;
  esac
fi
if [ -z "${YARD_LOGIN_RC:-}" ]; then
  case "${SHELL:-}" in
    *zsh)
      if [ -f "$HOME/.bash_profile" ]; then login_rcs+=("$HOME/.bash_profile")
      elif [ -f "$HOME/.bash_login" ]; then login_rcs+=("$HOME/.bash_login")
      else login_rcs+=("$HOME/.profile")
      fi
      login_shells+=(bash)
      ;;
    *) login_rcs+=("${ZDOTDIR:-$HOME}/.zprofile"); login_shells+=(zsh) ;;
  esac
fi

# Recovery is an explicit legacy signal; launcher markers alone may be ambiguous.
SOURCE_INGRESS_ROOT=''
SOURCE_INGRESS_CLASS=none
physical_runtime_root="$(readlink -f -- "$RUNTIME_ROOT")" || physical_runtime_root="$RUNTIME_ROOT"
# Runtime ownership takes precedence over checkout markers and recovery metadata:
# another runtime must be selected explicitly before assessing its source state.
yard_link="$BIN_DIR/yard"
sy_link="$BIN_DIR/sy"
yard_target=''
candidate_source=''
if [ -L "$yard_link" ] && [ -L "$sy_link" ]; then
  yard_target="$(readlink -f -- "$yard_link")" || yard_target=''
  sy_target="$(readlink -f -- "$sy_link")" || sy_target=''
  if [ -n "$yard_target" ] && [ "$yard_target" = "$sy_target" ]; then
    case "$yard_target" in
      */bin/yard)
        candidate_source="$(cd "$(dirname "$yard_target")/.." && pwd -P)"
        candidate_parent="$(dirname "$candidate_source")"
        source_runtime="$(dirname "$candidate_parent")"
        if [ "$(basename "$candidate_parent")" = releases ] &&
           [ -f "$candidate_source/bin/yard-engine" ] && [ -L "$source_runtime/current" ] &&
           [ "$source_runtime" != "$physical_runtime_root" ]; then
          SOURCE_INGRESS_CLASS=runtime
          printf 'bootstrap-runtime: entrypoints belong to runtime %s; re-run with --runtime-root %q\n' \
            "$source_runtime" "$source_runtime" >&2
          exit 1
        fi
        ;;
    esac
  fi
fi
# A crash while switching the two launchers can leave a recognized recovery
# journal but no longer leave two equal source links. Recover the descriptor
# from its operator-owned manifest; the verified candidate revalidates every
# path and resource before it mutates anything.
source_recovery="$DATA_HOME/recovery/pre-go-source"
source_manifest="$source_recovery/source-install-manifest.json"
source_transaction="$source_recovery/transaction"
if [ -z "$SOURCE_INGRESS_ROOT" ] &&
   { [ -e "$source_recovery" ] || [ -L "$source_recovery" ]; }; then
  if [ ! -d "$source_recovery" ] || [ -L "$source_recovery" ] || [ ! -O "$source_recovery" ] ||
     [ ! -f "$source_manifest" ] || [ -L "$source_manifest" ] || [ ! -O "$source_manifest" ] ||
     [ ! -f "$source_transaction" ] || [ -L "$source_transaction" ] ||
     [ ! -O "$source_transaction" ]; then
    printf 'bootstrap-runtime: source recovery metadata is unsafe or invalid\n' >&2
    exit 1
  fi
  source_recovery_state="$(<"$source_transaction")"
  case "$source_recovery_state" in
    $'schema=1\nphase=complete\nstep=complete') ;;
    $'schema=1\nphase=prepared\nstep=none'|\
    $'schema=1\nphase=applying\nstep=config-import'|\
    $'schema=1\nphase=applying\nstep=legacy-archive'|\
    $'schema=1\nphase=applying\nstep=state-migration'|\
    $'schema=1\nphase=applying\nstep=source-import-ready'|\
    $'schema=1\nphase=applying\nstep=shell-integration'|\
    $'schema=1\nphase=applying\nstep=entrypoint-switch')
      recovered_source="$(jq -er --arg data "$DATA_HOME" --arg config "$CONFIG_HOME" '
        select(.schemaVersion == 2 and .dataHome == $data and .configHome == $config) |
        .sourceRoot | select(type == "string" and startswith("/"))
      ' "$source_manifest" 2>/dev/null)" || recovered_source=''
      case "$recovered_source" in
        *$'\n'*|*$'\t'*|'')
          printf 'bootstrap-runtime: source recovery metadata is unsafe or invalid\n' >&2
          exit 1
          ;;
      esac
      physical_home="$(readlink -f -- "$HOME")" || physical_home=''
      physical_source="$(readlink -f -- "$recovered_source")" || physical_source=''
      if [ -n "$physical_home" ] && [ -n "$physical_source" ] &&
         [[ "$physical_source" == "$physical_home"/* ]]; then
        SOURCE_INGRESS_ROOT="$recovered_source"
        SOURCE_INGRESS_CLASS=legacy
      else
        printf 'bootstrap-runtime: source recovery metadata is unsafe or invalid\n' >&2
        exit 1
      fi
      ;;
    *)
      printf 'bootstrap-runtime: source recovery metadata is unsafe or invalid\n' >&2
      exit 1
      ;;
  esac
fi
# A pending recovery keeps its exact source descriptor and owner. Without it,
# only the dispatcher proves a legacy source; a native marker stays ambiguous.
if [ -z "$SOURCE_INGRESS_ROOT" ] && [ -n "$candidate_source" ]; then
  case "$yard_target" in
    "$physical_runtime_root"/*) ;;
    *)
      if [ -f "$candidate_source/config/commands.registry" ] &&
         [ -f "$candidate_source/completions/yard.bash" ]; then
        if grep -Fq 'thin dispatcher over scripts/' "$yard_target"; then
          SOURCE_INGRESS_ROOT="$candidate_source"
          SOURCE_INGRESS_CLASS=legacy
        elif grep -Fq 'Stable launcher for a release-installed native Go control-plane engine.' "$yard_target"; then
          SOURCE_INGRESS_CLASS=checkout
          printf 'bootstrap-runtime: entrypoints point at the checkout %s; if this is an installation older than v0.11.0, see docs/reference/legacy-migration-support.md\n' "$candidate_source" >&2
          exit 1
        fi
      fi
      ;;
  esac
fi
# Source-only installations cannot enter the supported update boundary. Refuse
# before publishing a runtime or changing their entrypoints. Existing protected
# journals still route through their exact sealed release owner.
if [ -n "$SOURCE_INGRESS_ROOT" ] && [ "$existing_runtime" = 0 ]; then
  printf '%s\n' "bootstrap-runtime: Sources before v0.11.0 are unsupported. Stage verified current tooling with subyard-install.sh --version <version> --publish-only, then run the printed candidate's bin/yard uninstall --full. Full uninstall deletes this installation's owned data; review and confirm its removal plan. After verified cleanup, install a new version separately." >&2
  exit 1
fi

need_path_line=1
case ":$PATH:" in *":$BIN_DIR:"*) need_path_line=0 ;; esac
if [ -f "$RC" ] && grep -qF "export PATH=\"$BIN_DIR:" "$RC"; then
  need_path_line=0
fi

printf 'Install the yard CLI\nThis will:\n'
printf '  - download and verify Subyard %s for %s/%s;\n' "$VERSION" "$os" "$arch"
printf '  - install the immutable runtime under %s;\n' "$RUNTIME_ROOT"
printf '  - complete the installed release transition before publishing CLI entrypoints;\n'
printf '  - link yard and sy under %s;\n' "$BIN_DIR"
printf '  - configure login PATH and shell completion.\n'
if [ -n "$SOURCE_INGRESS_ROOT" ]; then
  printf '  - assess the recognized source state with the selected release before continuing.\n'
fi
if [ "$ASSUME_YES" != 1 ] && [ "$existing_runtime" = 0 ]; then
  if [ ! -t 1 ] || [ ! -r /dev/tty ]; then
    printf 'bootstrap-runtime: confirmation requires a terminal; rerun with --yes for automation\n' >&2
    exit 1
  fi
  printf 'Proceed? [y/N] ' > /dev/tty
  IFS= read -r reply < /dev/tty || reply=''
  case "$reply" in y|Y|yes|YES|Yes) ;; *)
    printf 'bootstrap-runtime: cancelled\n' >&2
    exit 1 ;;
  esac
fi

else
  printf 'Stage verified Subyard %s for %s/%s under %s; activation requires a separate plan and consent.\n' "$VERSION" "$os" "$arch" "$RUNTIME_ROOT"
fi

name="subyard-$VERSION-$os-$arch.tar.gz"
release_dir="$CACHE_ROOT/$VERSION"
bundle="$release_dir/$name"
checksum="$bundle.sha256"
manifest="$bundle.manifest.json"
provenance="$bundle.provenance.json"
install -d -m 0700 "$release_dir"

fetch() { # <name> <destination>
  local asset="$1" destination="$2" temporary
  [ "$OFFLINE" = 0 ] || { [ -f "$destination" ] && [ ! -L "$destination" ]; return; }
  temporary="$(mktemp "$release_dir/.$asset.download.XXXXXX")"
  trap 'rm -f "$temporary"' RETURN
  if [ -n "${YARD_RELEASE_BASE_URL:-}" ]; then
    case "$YARD_RELEASE_BASE_URL" in
      file://*) cp -- "${YARD_RELEASE_BASE_URL#file://}/$asset" "$temporary" ;;
      https://*) curl -fsSL --proto '=https' --tlsv1.2 "$YARD_RELEASE_BASE_URL/$asset" -o "$temporary" ;;
      *) printf 'bootstrap-runtime: release base URL must use https:// or file://\n' >&2; return 2 ;;
    esac
  else
    curl -fsSL --proto '=https' --tlsv1.2 \
      "https://github.com/$REPOSITORY/releases/download/$tag/$asset" -o "$temporary"
  fi
  chmod 0600 "$temporary"
  mv -f "$temporary" "$destination"
  trap - RETURN
}

for suffix in '' .sha256 .manifest.json .provenance.json; do
  fetch "$name$suffix" "$bundle$suffix" \
    || { printf 'bootstrap-runtime: release download failed; current runtime was not changed\n' >&2; exit 1; }
done

installer="$release_dir/subyard-install-runtime-release.sh"
installer_checksum="$installer.sha256"
for suffix in '' .sha256; do
  fetch "subyard-install-runtime-release.sh$suffix" "$installer$suffix" \
    || { printf 'bootstrap-runtime: installer download failed; current runtime was not changed\n' >&2; exit 1; }
done
read -r installer_expected _ < "$installer_checksum" || true
installer_actual="$(sha256sum "$installer" | cut -d' ' -f1)"
[ "${installer_actual,,}" = "${installer_expected,,}" ] && [ "${#installer_actual}" = 64 ] \
  || { printf 'bootstrap-runtime: installer checksum mismatch\n' >&2; exit 1; }
[ "$(stat -c '%a' "$installer")" = 700 ] || chmod 0700 "$installer"

# The verified candidate validates both installation scopes before activation.
# The publisher passes this bounded request to that exact engine after verification.
unset SUBYARD_INSTALL_ENTRYPOINTS SUBYARD_INSTALL_VERIFIED_HELPER
if [ "$PUBLISH_ONLY" = 0 ]; then
  inventory_shell_files="$(printf '%s\n' "${login_rcs[@]}" "${shell_rcs[@]}" | jq -Rsc 'split("\n") | map(select(length > 0)) | unique')"
  SUBYARD_INSTALL_ENTRYPOINTS="$(jq -n --arg bin "$BIN_DIR" --argjson shellFiles "$inventory_shell_files" '{BinDir:$bin,ShellFiles:$shellFiles}')"
  export SUBYARD_INSTALL_ENTRYPOINTS
  publisher="$installer"
  publisher_source=''
  if [[ "$publisher_sha" = @* ]]; then
    publisher_source="$(dirname "${BASH_SOURCE[0]}")/../scripts/install-runtime-release.sh"
    [ -f "$publisher_source" ] && [ ! -L "$publisher_source" ] \
      || { printf 'bootstrap-runtime: current publisher is unavailable\n' >&2; exit 1; }
    publisher_sha="$(sha256sum "$publisher_source" | cut -d' ' -f1)"
  fi
  if [ "${installer_actual,,}" != "$publisher_sha" ]; then
    publisher_dir="$(mktemp -d /tmp/subyard-bootstrap-publisher.XXXXXX)"
    publisher="$publisher_dir/installer"
    trap 'rm -f -- "$publisher"; rmdir -- "$publisher_dir"' EXIT
    if [ -n "${publisher_source:-}" ]; then
      cp -- "$publisher_source" "$publisher"
    else
      publisher_payload > "$publisher"
    fi
    [ "$(sha256sum "$publisher" | cut -d' ' -f1)" = "$publisher_sha" ] \
      || { printf 'bootstrap-runtime: current publisher integrity check failed\n' >&2; exit 1; }
    chmod 0700 "$publisher"
  fi
  SUBYARD_INSTALL_VERIFIED_HELPER="$installer"
  export SUBYARD_INSTALL_VERIFIED_HELPER
fi

printf 'channel=%s available=%s platform=%s/%s\n' "$CHANNEL" "$VERSION" "$os" "$arch"
if [ "$PUBLISH_ONLY" = 1 ]; then
  published_release="$("$installer" --runtime-root "$RUNTIME_ROOT" --publish-only \
    --bundle "$bundle" --checksum "$checksum" --manifest "$manifest" --provenance "$provenance")"
  case "$published_release" in
    releases/"$VERSION"-*) ;;
    *) printf 'bootstrap-runtime: installer returned an invalid published release identity\n' >&2; exit 1 ;;
  esac
  candidate="$RUNTIME_ROOT/$published_release"
  [ -x "$candidate/bin/yard" ] \
    || { printf 'bootstrap-runtime: published candidate launcher is unavailable\n' >&2; exit 1; }
  printf 'staged release=%s candidate=%s\n' "$published_release" "$candidate"
  exit 0
fi
run_transition=0
if [ "$existing_runtime" = 0 ]; then
  "$publisher" --runtime-root "$RUNTIME_ROOT" \
    --bundle "$bundle" --checksum "$checksum" --manifest "$manifest" --provenance "$provenance"
  candidate="$(readlink -f -- "$RUNTIME_ROOT/current")"
else
  published_release="$("$publisher" --runtime-root "$RUNTIME_ROOT" --publish-only \
    --bundle "$bundle" --checksum "$checksum" --manifest "$manifest" --provenance "$provenance")"
  case "$published_release" in
    releases/"$VERSION"-*) ;;
    *) printf 'bootstrap-runtime: installer returned an invalid published release identity\n' >&2; exit 1 ;;
  esac
  candidate="$RUNTIME_ROOT/$published_release"
  [ -x "$candidate/bin/yard" ] \
    || { printf 'bootstrap-runtime: published candidate launcher is unavailable\n' >&2; exit 1; }
  run_transition=1
fi

# A crash after initial publication can leave current without configuration.
# Retry creates only a missing root; existing-runtime directories keep their mode.
if [ "$existing_runtime" = 0 ] || [ ! -d "$CONFIG_HOME" ]; then
  install -d -m 0700 "$CONFIG_HOME"
fi
unset SUBYARD_INSTALL_ENTRYPOINTS SUBYARD_INSTALL_VERIFIED_HELPER
# A first install interrupted before its authority was published leaves current
# without ledger and checkpoint. A retry with another version first runs that
# supported release's own migrate: it resumes its unfinished initial journal and
# refuses a journal whose history was lost. With no journal, ledger, checkpoint
# or previous link at all, the installation is fresh and is initialized.
uninitialized_current=0
if [ "$run_transition" = 1 ] && [ -z "$SOURCE_INGRESS_ROOT" ] &&
   [ ! -e "$RUNTIME_ROOT/previous" ] && [ ! -L "$RUNTIME_ROOT/previous" ]; then
  current_release="$(readlink -- "$RUNTIME_ROOT/current")" || current_release=''
  # The publisher names each release <version>-<first 12 hex digits of its digest>.
  # A prerelease identifier is numeric without leading zeros or contains a non-digit.
  # Numbers are limited to nine digits so shell arithmetic cannot overflow; longer
  # versions skip this step and keep the engine's own refusal.
  number='(0|[1-9][0-9]{0,8})'
  prerelease_identifier="($number|[0-9]*[A-Za-z-][0-9A-Za-z-]*)"
  release_pattern="^releases/($number\\.$number\\.$number(-$prerelease_identifier(\\.$prerelease_identifier)*)?)-[0-9a-f]{12}\$"
  if [[ "$current_release" =~ $release_pattern ]]; then
    current_version="${BASH_REMATCH[1]}" current_major="${BASH_REMATCH[2]}"
    current_minor="${BASH_REMATCH[3]}" current_patch="${BASH_REMATCH[4]}" current_prerelease="${BASH_REMATCH[5]}"
    # Supported installations start at v0.28.0; its prereleases precede it.
    if [ "$current_version" != "$VERSION" ] &&
       { [ "$current_major" -gt 0 ] || [ "$current_minor" -gt 28 ] ||
         { [ "$current_minor" -eq 28 ] && { [ "$current_patch" -gt 0 ] || [ -z "$current_prerelease" ]; }; }; }; then
      uninitialized_current=1
      for protected_record in ledger.json history-checkpoint.json; do
        if [ -e "$CONFIG_HOME/release-transition/v2/$protected_record" ] ||
           [ -L "$CONFIG_HOME/release-transition/v2/$protected_record" ]; then
          uninitialized_current=0
        fi
      done
    fi
  fi
fi
if [ "$uninitialized_current" = 1 ]; then
  initial_arguments=(migrate)
  if [ "$ASSUME_YES" = 1 ]; then
    initial_arguments+=(--yes)
  fi
  env "YARD_RUNTIME_ROOT=$RUNTIME_ROOT" \
    "$RUNTIME_ROOT/current/bin/yard" "${initial_arguments[@]}"
fi
if [ "$run_transition" = 1 ]; then
  transition_arguments=(update --runtime-root "$RUNTIME_ROOT" --version "$VERSION" --offline)
  transition_environment=(
    "YARD_RELEASE_CACHE=$CACHE_ROOT"
    "YARD_RUNTIME_ROOT=$RUNTIME_ROOT"
  )
  if [ "$ASSUME_YES" = 1 ]; then
    transition_arguments+=(--yes)
  fi
  if [ -n "$SOURCE_INGRESS_ROOT" ]; then
    transition_environment+=(
      # Diagnostic class travels with the descriptor through CommandInputs to
      # the candidate factory. It grants no migration or deletion authority.
      "SUBYARD_SOURCE_INGRESS_V1_CLASS=$SOURCE_INGRESS_CLASS"
      "SUBYARD_SOURCE_INGRESS_V1_ROOT=$SOURCE_INGRESS_ROOT"
      "SUBYARD_SOURCE_INGRESS_V1_DATA=$DATA_HOME"
      "SUBYARD_SOURCE_INGRESS_V1_BIN=$BIN_DIR"
      "SUBYARD_SOURCE_INGRESS_V1_RC=$RC"
      "SUBYARD_SOURCE_INGRESS_V1_LOGIN_RC=$LOGIN_RC"
    )
  fi
  env "${transition_environment[@]}" \
    "$candidate/bin/yard" "${transition_arguments[@]}"
else
  # The initial link has no predecessor, but its persisted-state capabilities
  # still need the supported protected transition before init can use them.
  # The bootstrap's installation consent covers this first-install boundary.
  env "YARD_RUNTIME_ROOT=$RUNTIME_ROOT" \
    "$candidate/bin/yard" migrate --yes
fi

ensure_directory "$DATA_HOME"
validate_data_directory \
  || { printf 'bootstrap-runtime: unsafe installation data directory\n' >&2; exit 1; }
[ -f "$candidate/config/installation-capabilities.json" ] && [ ! -L "$candidate/config/installation-capabilities.json" ] \
  && jq -e '. == {schemaVersion:1,contract:"installation-inventory-v1"}' "$candidate/config/installation-capabilities.json" >/dev/null \
  || { printf 'bootstrap-runtime: unsupported installation capability\n' >&2; exit 1; }
jq -n --arg bin "$BIN_DIR" --argjson shellFiles "$inventory_shell_files" \
  --arg installer "$installer" --arg checksum "$installer_checksum" \
  '{BinDir:$bin,ShellFiles:$shellFiles,CacheInputs:[$installer,$checksum]}' \
  | env YARD_RUNTIME_ROOT="$RUNTIME_ROOT" YARD_RELEASE_CACHE="$CACHE_ROOT" \
    SUBYARD_REPOSITORY_ROOT="$candidate" \
    "$candidate/bin/yard-engine" _installation full

if [ -z "$SOURCE_INGRESS_ROOT" ]; then
  ensure_directory "$BIN_DIR" 0755
  ln -sfn "$RUNTIME_ROOT/current/bin/yard" "$BIN_DIR/yard"
  ln -sfn "$RUNTIME_ROOT/current/bin/yard" "$BIN_DIR/sy"
fi

for index in "${!login_rcs[@]}"; do
  # Source ingress owns its primary startup files and their sealed recovery digests.
  if [ -n "$SOURCE_INGRESS_ROOT" ] && [ "$index" = 0 ]; then continue; fi
  login_rc="${login_rcs[index]}"
  ensure_directory "$(dirname "$login_rc")"
  if [ -f "$login_rc" ] && grep -qF 'Subyard CLI login PATH' "$login_rc"; then
    :
  else
    printf '\n# Subyard CLI login PATH\nexport PATH="%s:$PATH"\n' "$BIN_DIR" >> "$login_rc"
  fi
  # Bash login shells do not necessarily read .bashrc. Keep .profile POSIX-compatible and
  # restrict Readline setup to interactive Bash even when another shell reads this file.
  if [ "${login_shells[index]}" = bash ] &&
     ! grep -qF 'Subyard CLI login completion' "$login_rc"; then
    printf '\n# Subyard CLI login completion\nif [ -n "${BASH_VERSION:-}" ]; then\n  case $- in\n    *i*) [ -f "%s" ] && . "%s" ;;\n  esac\nfi\n' \
      "$RUNTIME_ROOT/current/completions/yard.bash" \
      "$RUNTIME_ROOT/current/completions/yard.bash" >> "$login_rc"
  fi
done
for index in "${!shell_rcs[@]}"; do
  if [ -n "$SOURCE_INGRESS_ROOT" ] && [ "$index" = 0 ]; then continue; fi
  shell_rc="${shell_rcs[index]}"
  completion="${shell_completions[index]}"
  ensure_directory "$(dirname "$shell_rc")"
  if [ -f "$shell_rc" ] && grep -qF "export PATH=\"$BIN_DIR:" "$shell_rc"; then
    :
  else
    printf '\n# Subyard CLI interactive PATH\nexport PATH="%s:$PATH"\n' "$BIN_DIR" >> "$shell_rc"
  fi
  if [ -f "$shell_rc" ] && grep -qF 'Subyard CLI completion' "$shell_rc"; then
    :
  else
    printf '\n# Subyard CLI completion\n[ -f "%s" ] && source "%s"\n' \
      "$completion" "$completion" >> "$shell_rc"
  fi
done

printf 'yard installed: %s/yard\n' "$BIN_DIR"
if [ "$need_path_line" = 1 ]; then
  printf 'activate it in this shell with: export PATH="%s:$PATH"\n' "$BIN_DIR"
fi
printf 'new shells load yard and completion automatically\n'
