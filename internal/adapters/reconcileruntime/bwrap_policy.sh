#!/usr/bin/env bash
set -euo pipefail

mode=$1
setting=${2:-}
dev_user=${3:-}
need_socat=${4:-0}
kind=${5:-}
need_bwrap=${6:-0}
policy_parent=/opt/subyard
policy=$policy_parent/bwrap-apparmor
record_parent=/var/lib/subyard
marker=$record_parent/bwrap-apparmor-ownership
pending=$marker.pending
record=$marker
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

publish_path() {
  mv -nT -- "$1" "$2"
  if present "$1"; then
    problem "$2 appeared during reconciliation"
    check_refusals
  fi
}

check_parent() {
  if present "$1" && ! { [ -d "$1" ] && root_path "$1" 755; }; then
    problem "ownership of the parent directory $1 cannot be verified"
    return 1
  fi
}

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

stock_container_dropin() {
  [ "$1" = /run/systemd/system/service.d/zzz-lxc-service.conf ] &&
    [ -f "$1" ] && root_path "$1" 644 &&
    awk '
      NR == 1 { if ($0 != "[Service]") exit 1; next }
      !/^(ProcSubset=all|ProtectProc=default|ProtectControlGroups=no|ProtectKernelTunables=no|NoNewPrivileges=no|LoadCredential=|PrivateNetwork=no|ImportCredential=)$/ { exit 1 }
      END { if (!NR) exit 1 }
    ' "$1"
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
  local paths dir override
  local -a unit_paths
  if ! paths="$(systemctl show --property=UnitPath --value)" || [ -z "$paths" ]; then
    problem 'the systemd unit lookup paths could not be verified'
    return
  fi
  read -r -a unit_paths <<< "$paths"
  for dir in "${unit_paths[@]}"; do
    for override in "$dir"/{service,subyard-.service,subyard-bwrap-.service,subyard-bwrap-apparmor.service}.d/*.conf; do
      if present "$override" && ! stock_container_dropin "$override"; then
        problem "administrator systemd override at $override"
      fi
    done
    if [ "$dir/${unit##*/}" != "$unit" ] && present "$dir/${unit##*/}"; then
      problem "administrator systemd override at $dir/${unit##*/}"
    fi
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
record_verified=0
check_record() {
  if ! { [ -f "$record" ] && root_path "$record" 644 &&
    [ "$(wc -l < "$record")" = 2 ] &&
    sed -n '1p' "$record" | grep -E '^[a-f0-9]{64}  unpriv_bwrap$' >/dev/null &&
    sed -n '2p' "$record" | grep -E '^[a-f0-9]{64}  bwrap$' >/dev/null; }; then
    problem "the ownership record $record is missing or invalid"
    return 1
  fi
  record_verified=1
}

check_loader() {
  [ -f "$policy/load" ] && root_path "$policy/load" 755 || return 1
  [ "$mode" = boot ] || cmp -s "$policy/load" <(printf '%s' "$BASH_EXECUTION_STRING") ||
    # Keep previously shipped loaders verifiable for repair and opt-out.
    case "$(sha256sum < "$policy/load")" in
      '67af6296afdba16d3511438b80184e5deefe1dfc9bab2d632def0b268a3b1e30  -' | \
      '3a3bfed285a93bd59669f466e65c4ff61802a519820783a45502be7072d77d72  -' | \
      '958d8e0c45a913ad823e982a4d28a8ca0c4e5c905728dc462c208783650977b5  -' | \
      'ca44357a350eb3d4bb0e9f1746eba3fa2a75918cd26b9986aedd28f359772a0d  -') return 0 ;;
      *) return 1 ;;
    esac
}

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
    if ! check_loader; then
      problem "the owned boot loader $policy/load is missing or changed"
    fi
    if [ -n "$(find "$policy" -mindepth 1 -maxdepth 1 ! -name bwrap ! -name unpriv_bwrap ! -name load -print -quit)" ]; then
      problem "unexpected entry in the owned policy directory $policy"
    fi
  else
    problem "the owned policy directory $policy is missing or changed"
  fi
  if check_record && [ "$checked" = 1 ]; then
    if cmp -s "$record" <(cd "$policy" && sha256sum "${names[@]}"); then
      files_verified=1
    else
      problem "the two profile hashes in $record do not match the files"
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

sandbox_failure() {
  printf 'subyard-integration-failure:bwrap-sandbox\n' >&2
  exit 1
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

extract_parser() {
  local member
  member="$(dpkg-deb --fsys-tarfile "$1" | tar -tf - | awk '
    /^(\.\/)?(usr\/)?sbin\/apparmor_parser$/ { member = $0; count++ }
    END { if (count != 1) exit 1; print member }
  ')" || return 1
  dpkg-deb --fsys-tarfile "$1" | tar -xOf - "$member"
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
  extract_parser ./apparmor_*.deb > parser
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
    (
      unit_stage="$(mktemp "${unit%/*}/.subyard-bwrap-apparmor.XXXXXX")"
      trap 'rm -f -- "$unit_stage"' EXIT
      unit_content > "$unit_stage"
      chmod 0644 "$unit_stage"
      publish_path "$unit_stage" "$unit"
    )
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
parents_verified=1
if present "$pending"; then
  if present "$marker"; then problem "both ownership records $marker and $pending exist"; fi
  record=$pending
fi
if present "$record" || present "$policy" || present "$unit" || present "$link"; then
  check_parent "$policy_parent" || parents_verified=0
  check_parent "$record_parent" || parents_verified=0
  check_unit
fi
if present "$record"; then
  check_namespace
  if [ "$parents_verified" = 1 ]; then
    if [ "$record" = "$pending" ] && ! present "$policy"; then
      check_record || :
      if present "$unit" || present "$link"; then problem "boot registration exists without the policy directory"; fi
      if [ -d "$aa/policy/profiles" ]; then check_live absent; fi
    else
      owned=1
      check_files
      # Never change either profile until both live names have been inspected.
      if [ "$files_verified" = 1 ] && [ -d "$aa/policy/profiles" ]; then check_live owned; fi
    fi
    if [ "$record_verified" = 1 ] && [ "$mode" = observe ]; then sha256sum -- "$record"; fi
  fi
elif present "$policy" || present "$unit" || present "$link"; then
  problem "existing policy or boot registration has no ownership record at $marker"
fi

if [ "$owned" = 1 ] && [ "$setting" != off ]; then check_admin_policy; fi
check_refusals

if [ "$mode" = boot ]; then
  if [ "$owned" != 1 ] || [ "$record" != "$marker" ] || [ "$(readlink "$link")" != "$unit" ]; then
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
  rm -f -- "$unit"
  # The pending record still authenticates the payload until its atomic removal
  # from the active path. A retry can finish from either side of that rename.
  stage="$(mktemp -d "$policy_parent/.bwrap-apparmor.XXXXXX")"
  trap 'rm -rf -- "$stage"' EXIT
  if [ "$record" = "$marker" ]; then publish_path "$marker" "$pending"; fi
  mv -T -- "$policy" "$stage/retired"
  rm -- "$pending"
  systemctl daemon-reload
  owned=0
  record=$marker
fi

if [ "$owned" = 0 ] && [ "$record" = "$pending" ]; then
  if [ "$mode" = observe ]; then
    printf 'bwrap=cleanup\n'
  else
    rm -- "$pending"
    systemctl daemon-reload
  fi
fi

eligible=0
if [ "$setting" != off ] && restricted; then eligible=1; fi
packages=()
if [ "$eligible" = 1 ] || [ "$need_bwrap" = 1 ] || [ "$owned" = 1 ]; then
  [ -x /usr/bin/bwrap ] || packages+=(bubblewrap)
  if [ "$need_socat" = 1 ] && [ ! -x /usr/bin/socat ]; then packages+=(socat); fi
fi
if [ "${#packages[@]}" != 0 ]; then
  if [ "$mode" = observe ]; then
    printf 'bwrap=install\n'
    if [ ! -x /usr/bin/bwrap ]; then
      if [ "$owned" = 1 ]; then
        printf 'bwrap=repair\n'
      elif [ "$eligible" = 1 ]; then
        printf 'bwrap=conditional\n'
      fi
      exit 0
    fi
  elif [ "$mode" = apply ]; then
    apt-get update -qq >&2
    DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=a apt-get install -y -qq --no-install-recommends "${packages[@]}" >&2
  fi
fi
[ "$setting" != off ] || exit 0

if [ "$owned" = 1 ]; then
  if [ "$mode" = observe ]; then
    if [ "$record" = "$marker" ] && [ "${#missing[@]}" = 0 ] && [ -f "$unit" ] && [ "$(readlink "$link")" = "$unit" ] && probe >/dev/null 2>&1; then
      printf 'bwrap=owned\n'
    else
      printf 'bwrap=repair\n'
    fi
  elif [ "$mode" = apply ]; then
    if [ "$record" = "$pending" ]; then publish_path "$pending" "$marker"; fi
    load_missing
    probe >/dev/null 2>&1 || sandbox_failure
    register_boot
  fi
  exit 0
fi

if [ "$eligible" = 0 ] && [ "$need_bwrap" = 0 ]; then exit 0; fi
if probe >/dev/null 2>&1; then exit 0; fi
if [ "$eligible" = 0 ]; then
  sandbox_failure
fi
check_parent "$policy_parent" || :
check_parent "$record_parent" || :
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

# Build and verify staged files before publishing the ownership record or payload.
for parent in "$policy_parent" "$record_parent"; do
  if ! present "$parent"; then mkdir -m 0755 -- "$parent"; fi
  check_parent "$parent" || :
done
check_refusals
stage="$(mktemp -d "$policy_parent/.bwrap-apparmor.XXXXXX")"
receipt_stage=
trap 'rm -rf -- "$stage"; if [ -n "$receipt_stage" ]; then rm -f -- "$receipt_stage"; fi' EXIT
trap 'problem "could not compile the distribution Bubblewrap profiles"' ERR
compile_policy "$stage"
trap - ERR
check_parent "$policy_parent" || :
check_parent "$record_parent" || :
check_admin_policy
check_unit
check_live absent
check_refusals
if probe >/dev/null 2>&1; then exit 0; fi
mkdir -m 0755 -- "$stage/policy"
for name in "${names[@]}"; do install -m 0644 "$stage/$name" "$stage/policy/$name"; done
printf '%s' "$BASH_EXECUTION_STRING" | install -m 0755 /dev/stdin "$stage/policy/load"
receipt_stage="$(mktemp "$record_parent/.bwrap-apparmor-ownership.XXXXXX")"
(cd "$stage/policy" && sha256sum "${names[@]}") > "$receipt_stage"
chmod 0644 "$receipt_stage"
publish_path "$receipt_stage" "$pending"
publish_path "$stage/policy" "$policy"
publish_path "$pending" "$marker"
record=$marker
check_files
check_refusals
load_missing
probe >/dev/null 2>&1 || sandbox_failure
register_boot
