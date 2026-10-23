#!/usr/bin/env bash
# shellcheck disable=SC2016
# Static skill delivery only: no service, toolchain or emulator operations.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/engine-context.sh
. "$SCRIPT_DIR/lib/engine-context.sh"
subyard_require_engine_context
mode="${1:-apply}"
case "$mode" in apply|check) ;; *) exit 2 ;; esac
root="$(cd "$SCRIPT_DIR/.." && pwd)"
# Ship only package-local skill sources. The archive is ephemeral; installed skills
# and their receipts have no dependency on the host checkout or temporary paths.
python3 - "$root" <<'PY' | incus exec "$YARD_INSTANCE_NAME" --project "$INCUS_PROJECT" \
  --env DEV_USER="${DEV_USER:-dev}" --env ENVIRONMENT_PROFILES="${ENVIRONMENT_PROFILES:-}" \
  --env ALLOWS_CODING_TOOLS="${ALLOWS_CODING_TOOLS:-true}" -- bash -eu -c '
  work=$(mktemp -d /tmp/subyard-skills.XXXXXX)
  trap '\''rm -rf -- "$work"'\'' EXIT HUP INT TERM
  tar -xf - -C "$work"
  home=$(getent passwd "$DEV_USER" | cut -d: -f6)
  test -n "$home"
  uid=$(id -u "$DEV_USER"); gid=$(id -g "$DEV_USER")
  helper="$work/config/skills/deliver.py"
  installed=/usr/local/libexec/subyard/agent-skills
  if [ "$1" = apply ]; then
    for directory in /usr/local/libexec /usr/local/libexec/subyard; do test ! -L "$directory"; done
    install -d -m 0755 /usr/local/libexec/subyard
    test ! -L "$installed"
    if [ -e "$installed" ]; then grep -Fxq "# subyard-agent-skills-v1" "$installed"; fi
    install -m 0755 "$helper" "$installed"
  else
    test ! -L "$installed" && test -f "$installed" && cmp -s "$helper" "$installed" || exit 1
    test "$(stat -c "%a:%u:%g" "$installed")" = 755:0:0 || exit 1
  fi
  operation() {
    local result=0
    python3 "$helper" "$1" --provider "$2" --source "$3" --home "$home" --uid "$uid" --gid "$gid" "${@:4}" || result=$?
    [ "$result" -ne 10 ] || return 1
    return "$result"
  }
  core_mode="$1"
  if [ "$ALLOWS_CODING_TOOLS" = false ]; then
    if [ "$1" = check ]; then core_mode=check-removed; else core_mode=remove; fi
  fi
  if [ "$1" = apply ]; then python3 "$work/config/skills/migrate-preview.py" "$home" "$uid"; fi
  operation "$core_mode" core "$work/config/skills/core"
  for receipt in /var/lib/subyard/agent-skills/profile-*.json; do
    [ -f "$receipt" ] || continue
    provider=$(basename "$receipt" .json)
    profile=${provider#profile-}
    source="$work/config/profiles/$profile/skills"
    [ ! -f "$work/config/profiles/$profile/skills.json" ] || source="$work/config/profiles/$profile/skills.json"
    selected=false
    for name in $ENVIRONMENT_PROFILES; do [ "$name" != "$profile" ] || selected=true; done
    profile_mode="$1"
    if [ "$selected" = false ]; then
      if [ "$1" = check ]; then profile_mode=check-removed; else profile_mode=remove; fi
    fi
    operation "$profile_mode" "$provider" "$source" --registered
  done
' -- "$mode"
import json, pathlib, sys, tarfile
root = pathlib.Path(sys.argv[1])
with tarfile.open(fileobj=sys.stdout.buffer, mode='w|') as archive:
    archive.add(root/'config/skills', arcname='config/skills', filter=lambda m: None if '__pycache__' in m.name or '/tests' in m.name else m)
    for package in sorted((root/'config/profiles').iterdir()):
        source = package/'skills'
        if source.is_dir():
            archive.add(source, arcname=str(source.relative_to(root)))
        manifest = package/'skills.json'
        if manifest.is_file():
            archive.add(manifest, arcname=str(manifest.relative_to(root)))
            for files in json.loads(manifest.read_text()).values():
                for relative in files.values():
                    source = package/relative
                    archive.add(source, arcname=str(source.relative_to(root)))
PY
