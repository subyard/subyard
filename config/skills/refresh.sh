# shellcheck shell=bash
  work=$(mktemp -d /tmp/subyard-skills.XXXXXX)
  trap 'rm -rf -- "$work"' EXIT HUP INT TERM
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
