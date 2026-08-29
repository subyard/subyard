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
ASSUME_YES="${ASSUME_YES:-0}"

while [ $# -gt 0 ]; do
  case "$1" in
    --channel) [ $# -ge 2 ] || { printf 'bootstrap-runtime: --channel needs stable\n' >&2; exit 2; }; CHANNEL="$2"; shift 2 ;;
    --version) [ $# -ge 2 ] || { printf 'bootstrap-runtime: --version needs a value\n' >&2; exit 2; }; VERSION="$2"; shift 2 ;;
    --runtime-root) [ $# -ge 2 ] || { printf 'bootstrap-runtime: --runtime-root needs a path\n' >&2; exit 2; }; RUNTIME_ROOT="$2"; shift 2 ;;
    --offline) OFFLINE=1; shift ;;
    -y|--yes) ASSUME_YES=1; shift ;;
    -h|--help)
      printf 'Usage: bootstrap-runtime.sh [--version VERSION] [--offline] [--runtime-root PATH] [--yes]\n'
      exit 0 ;;
    *) printf 'bootstrap-runtime: unknown option: %s\n' "$1" >&2; exit 2 ;;
  esac
done

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

tag=""
if [ -z "$VERSION" ]; then
  [ "$OFFLINE" = 0 ] || { printf 'bootstrap-runtime: offline mode requires --version\n' >&2; exit 2; }
  release_json="$(curl -fsSL --proto '=https' --tlsv1.2 \
    -H 'Accept: application/vnd.github+json' -H 'X-GitHub-Api-Version: 2022-11-28' \
    "https://api.github.com/repos/$REPOSITORY/releases/latest")" \
    || { printf 'bootstrap-runtime: could not resolve the stable release\n' >&2; exit 1; }
  tag="$(jq -er '.tag_name | select(type == "string" and length > 0)' <<<"$release_json")" \
    || { printf 'bootstrap-runtime: latest release has no valid tag\n' >&2; exit 1; }
  VERSION="${tag#v}"
else
  tag="${YARD_RELEASE_TAG:-v$VERSION}"
fi
case "$VERSION" in ''|*[!A-Za-z0-9._+-]*) printf 'bootstrap-runtime: unsafe version: %s\n' "$VERSION" >&2; exit 2 ;; esac

# Pick the files that new interactive and login shells actually read. The stable `current` link
# keeps completion valid across upgrades and rollback.
RC="${YARD_SHELL_RC:-}"
if [ -z "$RC" ]; then
  case "${SHELL:-}" in
    *zsh) RC="$HOME/.zshrc" ;;
    *) RC="$HOME/.bashrc" ;;
  esac
fi
LOGIN_RC="${YARD_LOGIN_RC:-}"
if [ -z "$LOGIN_RC" ]; then
  case "${SHELL:-}" in
    *zsh) LOGIN_RC="$HOME/.zprofile" ;;
    *)
      if [ -f "$HOME/.bash_profile" ]; then LOGIN_RC="$HOME/.bash_profile"
      elif [ -f "$HOME/.bash_login" ]; then LOGIN_RC="$HOME/.bash_login"
      else LOGIN_RC="$HOME/.profile"
      fi
      ;;
  esac
fi
case "$RC" in
  *zsh*) completion="$RUNTIME_ROOT/current/completions/yard.zsh" ;;
  *) completion="$RUNTIME_ROOT/current/completions/yard.bash" ;;
esac

# Detect the bounded pre-Go source ingress before any installation mutation.
# The verified candidate repeats ownership, containment and manifest checks.
SOURCE_INGRESS_ROOT=''
yard_link="$BIN_DIR/yard"
sy_link="$BIN_DIR/sy"
if [ -L "$yard_link" ] && [ -L "$sy_link" ]; then
  yard_target="$(readlink -f -- "$yard_link")" || yard_target=''
  sy_target="$(readlink -f -- "$sy_link")" || sy_target=''
  if [ -n "$yard_target" ] && [ "$yard_target" = "$sy_target" ]; then
    case "$yard_target" in
      "$RUNTIME_ROOT"/*) ;;
      */bin/yard)
        candidate_source="$(cd "$(dirname "$yard_target")/.." && pwd -P)"
        if [ "$yard_target" = "$candidate_source/bin/yard" ] &&
           [ -f "$candidate_source/config/commands.registry" ] &&
           [ -f "$candidate_source/completions/yard.bash" ] &&
           { grep -Fq 'thin dispatcher over scripts/' "$yard_target" ||
             grep -Fq 'Stable launcher for a release-installed native Go control-plane engine.' \
               "$yard_target"; }; then
          SOURCE_INGRESS_ROOT="$candidate_source"
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
        "$HOME"/*) SOURCE_INGRESS_ROOT="$recovered_source" ;;
        *)
          printf 'bootstrap-runtime: source recovery metadata is unsafe or invalid\n' >&2
          exit 1
          ;;
      esac
      ;;
    *)
      printf 'bootstrap-runtime: source recovery metadata is unsafe or invalid\n' >&2
      exit 1
      ;;
  esac
fi
need_path_line=1
case ":$PATH:" in *":$BIN_DIR:"*) need_path_line=0 ;; esac
if [ -f "$RC" ] && grep -qF "export PATH=\"$BIN_DIR:" "$RC"; then
  need_path_line=0
fi

printf 'Install the yard CLI\nThis will:\n'
printf '  - download and verify Subyard %s for %s/%s;\n' "$VERSION" "$os" "$arch"
printf '  - install the immutable runtime under %s;\n' "$RUNTIME_ROOT"
printf '  - link yard and sy under %s;\n' "$BIN_DIR"
printf '  - configure login PATH and shell completion.\n'
if [ -n "$SOURCE_INGRESS_ROOT" ]; then
  printf '  - migrate the recognized source install through the release transition.\n'
fi
if [ "$ASSUME_YES" != 1 ] && [ "$existing_runtime" = 0 ] &&
   [ -z "$SOURCE_INGRESS_ROOT" ]; then
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
chmod 0700 "$installer"

printf 'channel=%s available=%s platform=%s/%s\n' "$CHANNEL" "$VERSION" "$os" "$arch"
run_transition=0
if [ "$existing_runtime" = 0 ]; then
  install -d -m 0700 "$CONFIG_HOME"
  "$installer" --runtime-root "$RUNTIME_ROOT" \
    --bundle "$bundle" --checksum "$checksum" --manifest "$manifest" --provenance "$provenance"
  candidate="$(readlink -f -- "$RUNTIME_ROOT/current")"
  [ -z "$SOURCE_INGRESS_ROOT" ] || run_transition=1
else
  published_release="$("$installer" --runtime-root "$RUNTIME_ROOT" --publish-only \
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
      "SUBYARD_SOURCE_INGRESS_V1_ROOT=$SOURCE_INGRESS_ROOT"
      "SUBYARD_SOURCE_INGRESS_V1_DATA=$DATA_HOME"
      "SUBYARD_SOURCE_INGRESS_V1_BIN=$BIN_DIR"
      "SUBYARD_SOURCE_INGRESS_V1_RC=$RC"
      "SUBYARD_SOURCE_INGRESS_V1_LOGIN_RC=$LOGIN_RC"
    )
  fi
  env "${transition_environment[@]}" \
    "$candidate/bin/yard" "${transition_arguments[@]}"
fi

if [ -z "$SOURCE_INGRESS_ROOT" ]; then
  install -d "$BIN_DIR"
  ln -sfn "$RUNTIME_ROOT/current/bin/yard" "$BIN_DIR/yard"
  ln -sfn "$RUNTIME_ROOT/current/bin/yard" "$BIN_DIR/sy"

  if [ -f "$LOGIN_RC" ] && grep -qF 'Subyard CLI login PATH' "$LOGIN_RC"; then
    :
  else
    printf '\n# Subyard CLI login PATH\nexport PATH="%s:$PATH"\n' "$BIN_DIR" >> "$LOGIN_RC"
  fi
  if [ "$need_path_line" = 1 ]; then
    if [ -f "$RC" ] && grep -qF 'Subyard CLI interactive PATH' "$RC"; then
      :
    else
      printf '\n# Subyard CLI interactive PATH\nexport PATH="%s:$PATH"\n' "$BIN_DIR" >> "$RC"
    fi
  fi
  if [ -f "$RC" ] && grep -qF 'Subyard CLI completion' "$RC"; then
    :
  else
    printf '\n# Subyard CLI completion\n[ -f "%s" ] && source "%s"\n' \
      "$completion" "$completion" >> "$RC"
  fi
fi

printf 'yard installed: %s/yard\n' "$BIN_DIR"
if [ "$need_path_line" = 1 ]; then
  printf 'activate it in this shell with: export PATH="%s:$PATH"\n' "$BIN_DIR"
fi
printf 'new shells load yard and completion automatically\n'
