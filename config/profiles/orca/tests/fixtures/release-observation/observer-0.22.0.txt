set -euo pipefail
desired="$1"; hook="$2"; directory="$3"; marker="$4"; lock="$5"; executable="$6"; unit="$7"
discovery_service="$8"; discovery_timer="$9"; shift 9
codex_profile="$1"; codex_bin="$2"; shift 2
present=0
# The package launcher may be a symlink; profile-owned contract paths may not.
[ ! -e "$executable" ] && [ ! -L "$executable" ] || present=1
for artifact in "$unit" "$hook" "$directory" "$marker" "$lock" \
  "$discovery_service" "$discovery_timer" "$codex_profile" "$codex_bin" "$directory"/*.py; do
  [ ! -L "$artifact" ] || { echo 'Orca runtime contract contains an unsafe symbolic link' >&2; exit 1; }
  [ ! -e "$artifact" ] || present=1
done
if [ "$present" = 0 ]; then
  printf '{"state":"absent","actual":"","desired":""}\n'
  exit 0
fi
actual="$({
  if [ -f "$hook" ]; then
    printf '%s %s:%s ' "$(stat -c %a "$hook")" "$(stat -c %u "$hook")" "$(stat -c %g "$hook")"
    sha256sum "$hook" | awk '{print $1}'
  else printf 'missing\n'; fi
  for name in "$@"; do
    if [ -f "$directory/$name" ]; then
      printf '%s %s:%s %s ' "$(stat -c %a "$directory/$name")" \
        "$(stat -c %u "$directory/$name")" "$(stat -c %g "$directory/$name")" "$name"
    else
      printf '%s ' "$name"
    fi
    if [ -f "$directory/$name" ]; then
      sha256sum "$directory/$name" | awk '{print $1}'
    else
      printf 'missing\n'
    fi
  done
  for installed in "$directory"/*.py; do
    [ -e "$installed" ] || continue
    case " $* " in
      *" ${installed##*/} "*) ;;
      *) printf 'unexpected %s\n' "${installed##*/}" ;;
    esac
  done
  for artifact in "$directory/discover" "$discovery_service" "$discovery_timer" "$codex_profile" "$codex_bin"; do
    case "$artifact" in
      "$directory/discover") label=discover ;;
      "$discovery_service") label=discovery-service ;;
      "$discovery_timer") label=discovery-timer ;;
      "$codex_profile") label=codex-profile ;;
      *) label=codex-bin ;;
    esac
    if [ -f "$artifact" ]; then
      printf '%s %s:%s %s ' "$(stat -c %a "$artifact")" "$(stat -c %u "$artifact")" "$(stat -c %g "$artifact")" "$label"
      sha256sum "$artifact" | awk '{print $1}'
    else printf 'missing %s\n' "$label"; fi
  done
  if systemctl is-enabled --quiet "${discovery_timer##*/}"; then
    printf 'enabled discovery-timer\n'
  else printf 'disabled discovery-timer\n'; fi
  if [ -f "$lock" ]; then
    printf '%s %s:%s lock\n' "$(stat -c %a "$lock")" "$(stat -c %u "$lock")" "$(stat -c %g "$lock")"
  else
    printf 'missing lock\n'
  fi
  if [ -f "$marker" ]; then
    printf '%s %s:%s marker ' "$(stat -c %a "$marker")" \
      "$(stat -c %u "$marker")" "$(stat -c %g "$marker")"
    cat "$marker"
  else
    printf 'missing marker\n'
  fi
} | sha256sum | awk '{print $1}')"
state=stale
if [ "$actual" = "$desired" ]; then
  state=current
fi
printf '{"state":"%s","actual":"%s","desired":"%s"}\n' "$state" "$actual" "$desired"
