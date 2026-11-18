#!/usr/bin/env bash
set -euo pipefail

mode=$1
setting=${2:-}
dev_user=${3:-}
need_socat=${4:-0}
kind=${5:-}
policy=/opt/subyard/bwrap-apparmor
marker=/var/lib/subyard/bwrap-apparmor-ownership
unit=/etc/systemd/system/subyard-bwrap-apparmor.service
link=/etc/systemd/system/multi-user.target.wants/${unit##*/}
aa=/sys/kernel/security/apparmor
legacy_unit=/etc/systemd/system/subyard-codex-bwrap.service
names=(unpriv_bwrap bwrap)

present() { [ -e "$1" ] || [ -L "$1" ]; }
root_path() { [ ! -L "$1" ] && [ "$(stat -c '%u:%g:%a' "$1" 2>/dev/null)" = "0:0:$2" ]; }
problem() {
  printf 'Bubblewrap AppArmor: %s. Inspect the yard policy and restore the original Subyard files from backup or resolve the administrator policy before rerunning yard setup.\n' "$*" >&2
  refused=1
}
refused=0
check_refusals() { [ "$refused" = 0 ] || exit 1; }

unit_content() {
  cat <<UNIT
[Unit]
Description=Load Subyard Bubblewrap AppArmor policy
After=apparmor.service
Before=systemd-user-sessions.service

[Service]
Type=oneshot
ExecStart=$policy/load boot

[Install]
WantedBy=multi-user.target
UNIT
}

check_unit() {
  if present "$unit"; then
    if ! { [ -f "$unit" ] && root_path "$unit" 644 && cmp -s "$unit" <(unit_content); }; then
      problem "ownership of $unit cannot be verified"
    fi
  fi
  if present "$link"; then
    [ "$(readlink "$link" 2>/dev/null)" = "$unit" ] || problem "ownership of $link cannot be verified"
  fi
  local override
  for override in /{etc,run}/systemd/system/"${unit##*/}".d /run/systemd/system/"${unit##*/}"; do
    if present "$override"; then problem "administrator systemd override at $override"; fi
  done
}

check_admin_policy() {
  local path
  for path in /etc/apparmor.d/{bwrap,bwrap-userns-restrict,unpriv_bwrap} \
      /etc/apparmor.d/{local,disable,force-complain}/{bwrap,bwrap-userns-restrict,unpriv_bwrap}; do
    if present "$path"; then problem "administrator AppArmor policy at $path"; fi
  done
}

check_namespace() {
  local level
  if ! read -r level < "$aa/.ns_level" || ! [[ "$level" =~ ^[1-9][0-9]*$ ]]; then
    problem 'AppArmor is not in a yard policy namespace'
  fi
  [ -d "$aa/policy/profiles" ] || problem 'the yard cannot inspect its AppArmor profiles'
}

files_verified=0
check_files() {
  local name checked=0
  if [ -d "$policy" ] && root_path "$policy" 755; then
    checked=1
    for name in "${names[@]}"; do
      if ! { [ -f "$policy/$name" ] && root_path "$policy/$name" 644; }; then
        problem "the owned file $policy/$name is missing or changed"
        checked=0
      fi
    done
    if ! { [ -f "$policy/load" ] && root_path "$policy/load" 755; }; then
      problem "the owned boot loader $policy/load is missing or changed"
    elif [ "$mode" != boot ] \
        && ! cmp -s "$policy/load" <(printf '%s' "$BASH_EXECUTION_STRING"); then
      problem "the boot loader $policy/load does not match the Subyard release"
    fi
    if [ -n "$(find "$policy" -mindepth 1 -maxdepth 1 ! -name bwrap ! -name unpriv_bwrap ! -name load -print -quit)" ]; then
      problem "unexpected entry in the owned policy directory $policy"
    fi
  else
    problem "the owned policy directory $policy is missing or changed"
  fi
  if ! { [ -f "$marker" ] && root_path "$marker" 644; }; then
    problem "the ownership record $marker is missing or invalid"
  elif [ "$checked" = 1 ]; then
    if cmp -s "$marker" <(cd "$policy" && sha256sum "${names[@]}"); then
      files_verified=1
    else
      problem "the two profile hashes in $marker do not match the files"
    fi
  fi
}

missing=()
check_live() {
  local name entry
  local loaded
  missing=()
  for name in "${names[@]}"; do
    loaded=0
    for entry in "$aa/policy/profiles/$name".*; do
      [ -d "$entry" ] || continue
      loaded=1
      if [ "$1" != owned ] || ! cmp -s "$policy/$name" "$entry/raw_data"; then
        problem "the loaded AppArmor profile $name cannot be verified as Subyard-owned"
      fi
    done
    [ "$loaded" = 1 ] || missing+=("$name")
  done
}

probe() {
  runuser -u "$dev_user" -- /usr/bin/bwrap --unshare-user --unshare-net \
    --ro-bind / / --chdir / -- /bin/true
}

restricted() {
  local enabled
  [ "$kind" != vm ] && read -r enabled 2>/dev/null < /proc/sys/kernel/apparmor_restrict_unprivileged_userns \
    && [ "$enabled" = 1 ]
}

load_missing() {
  local name added entry index
  local added_names=()
  for name in "${missing[@]}"; do
    if ! cat "$policy/$name" > "$aa/.load"; then
      problem "could not load AppArmor profile $name"
      for ((index=${#added_names[@]}-1; index>=0; index--)); do
        added=${added_names[index]}
        for entry in "$aa/policy/profiles/$added".*; do
          if [ -d "$entry" ] && cmp -s "$policy/$added" "$entry/raw_data"; then
            printf '%s' "$added" > "$aa/.remove" || problem "could not undo loading AppArmor profile $added"
          fi
        done
      done
      check_refusals
    fi
    added_names+=("$name")
  done
  check_live owned
  check_refusals
  [ "${#missing[@]}" = 0 ]
}

package_member() {
  dpkg-deb --fsys-tarfile "$1" | tar -xOf - ".$2"
}

compile_policy() (
  cd "$1"
  apt-get update -qq >&2
  apt-get download apparmor >&2
  if ! package_member ./apparmor_*.deb /etc/apparmor.d/bwrap-userns-restrict > source 2>/dev/null; then
    apt-get download apparmor-profiles >&2
    package_member ./apparmor-profiles_*.deb \
      /usr/share/apparmor/extra-profiles/bwrap-userns-restrict > source
  fi
  abi="$(sed -nE 's/^abi <abi\/([0-9]+\.[0-9]+)>,$/\1/p' source)"
  [[ "$abi" =~ ^[0-9]+\.[0-9]+$ ]]
  mkdir abi
  package_member ./apparmor_*.deb "/etc/apparmor.d/abi/$abi" > "abi/$abi"
  package_member ./apparmor_*.deb /usr/sbin/apparmor_parser > parser
  chmod 0755 parser
  parser=(./parser --config-file /dev/null --base "$PWD" --skip-cache)
  for name in "${names[@]}"; do
    # Both administrator includes are checked before extracting the stock definitions.
    awk -v name="$name" '
      /^abi / { print }
      $1 == "profile" && $2 == name { selected = 1 }
      selected && !/^[[:space:]]*include if exists <local\/(bwrap-userns-restrict|unpriv_bwrap)>/ { print }
      /^}/ { selected = 0 }
    ' source > "$name.source"
    [ "$("${parser[@]}" --names "$name.source")" = "$name" ]
    "${parser[@]}" --stdout "$name.source" > "$name"
  done
)

register_boot() {
  if ! present "$unit"; then
    unit_content | install -m 0644 /dev/stdin "$unit"
  fi
  systemctl enable "${unit##*/}" >&2
  check_unit
  check_refusals
  [ "$(readlink "$link")" = "$unit" ]
}

if [ "$mode" != observe ]; then
  exec 9>/run/subyard-bwrap-apparmor.lock
  flock 9
fi

for old in /opt/subyard/codex/apparmor "$legacy_unit" \
    /etc/systemd/system/multi-user.target.wants/"${legacy_unit##*/}"; do
  if present "$old"; then problem "legacy policy at $old has no ownership record"; fi
done

owned=0
if present "$marker"; then
  owned=1
  check_files
  check_namespace
fi
if [ "$owned" = 1 ] || present "$policy" || present "$unit" || present "$link"; then
  check_unit
  [ "$owned" = 1 ] || problem "existing policy or boot registration has no ownership record at $marker"
fi

if [ "$owned" = 1 ]; then
  # Never change either profile until both live names have been inspected.
  if [ "$files_verified" = 1 ] && [ -d "$aa/policy/profiles" ]; then check_live owned; fi
  if [ "$files_verified" = 1 ] && [ "$mode" = observe ]; then sha256sum -- "$marker"; fi
  if [ "$setting" != off ]; then check_admin_policy; fi
fi
check_refusals

if [ "$mode" = boot ]; then
  if [ "$owned" != 1 ] || [ "$(readlink "$link")" != "$unit" ]; then
    problem "the boot loader has no verified ownership or is disabled"
    check_refusals
  fi
  load_missing
  exit
fi

if [ "$setting" = off ] && [ "$owned" = 1 ]; then
  if [ "$mode" = observe ]; then
    printf 'bwrap=retire\n'
    exit
  fi
  # Disable startup before removing the attachment, then the payload policy.
  if present "$unit" || present "$link"; then systemctl disable --now "${unit##*/}" >&2; fi
  if present "$link"; then
    problem "could not disable $unit"
    check_refusals
  fi
  for name in bwrap unpriv_bwrap; do
    if [[ " ${missing[*]} " != *" $name "* ]]; then printf '%s' "$name" > "$aa/.remove"; fi
  done
  check_live absent
  check_refusals
  rm -f -- "$unit" "$policy/load" "$policy/bwrap" "$policy/unpriv_bwrap"
  rmdir -- "$policy"
  rm -- "$marker"
  systemctl daemon-reload
  owned=0
fi

packages=()
if { [ "$setting" != off ] && restricted; } || [ "$need_socat" = 1 ] || [ "$owned" = 1 ]; then
  [ -x /usr/bin/bwrap ] || packages+=(bubblewrap)
  if [ "$need_socat" = 1 ] && [ ! -x /usr/bin/socat ]; then packages+=(socat); fi
fi
if [ "${#packages[@]}" != 0 ]; then
  if [ "$mode" = observe ]; then
    printf 'bwrap=install\n'
    exit
  fi
  [ "$mode" = apply ] || exit 0
  apt-get update -qq >&2
  DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=a apt-get install -y -qq --no-install-recommends "${packages[@]}" >&2
fi
[ "$setting" != off ] || exit 0

if [ "$owned" = 1 ]; then
  if [ "$mode" = observe ]; then
    if [ "${#missing[@]}" = 0 ] && [ -f "$unit" ] && [ "$(readlink "$link")" = "$unit" ] && probe >/dev/null 2>&1; then
      printf 'bwrap=owned\n'
    else
      printf 'bwrap=repair\n'
    fi
  elif [ "$mode" = apply ]; then
    load_missing
    probe
    register_boot
  fi
  exit 0
fi

if ! restricted || probe >/dev/null 2>&1; then exit 0; fi
check_namespace
check_admin_policy
check_unit
check_live absent
[ -w "$aa/.load" ] || problem 'the yard cannot load AppArmor profiles'
check_refusals
if [ "$mode" = observe ]; then
  printf 'bwrap=enable\n'
  exit 0
fi

# Only compiled policy survives installation. The distro parser runs on staging inputs.
stage="$(mktemp -d)"
trap 'rm -rf -- "$stage"' EXIT
trap 'problem "could not compile the distribution Bubblewrap profiles"' ERR
compile_policy "$stage"
trap - ERR
check_admin_policy
check_unit
check_live absent
check_refusals
if probe >/dev/null 2>&1; then exit 0; fi
mkdir -p -- "$(dirname "$marker")"
mkdir -m 0755 -- "$policy"
for name in "${names[@]}"; do install -m 0644 "$stage/$name" "$policy/$name"; done
printf '%s' "$BASH_EXECUTION_STRING" | install -m 0755 /dev/stdin "$policy/load"
(cd "$policy" && sha256sum "${names[@]}") | install -m 0644 /dev/stdin "$marker"
check_files
check_refusals
load_missing
probe
register_boot
