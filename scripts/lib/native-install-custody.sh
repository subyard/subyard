#!/usr/bin/env bash
# Retain the actual installer flock across its entire effect and verification body.
subyard_install_custody() { # <fixed owner> <installed libexec directory>
  local owner=$1 primary=$3 helper="$SCRIPT_DIR/lib/native-install-custody.py" resource directory fd
  python3 -B "$helper" owner-barrier "$owner" "$primary"
  SUBYARD_INSTALL_LOCK_FILES=()
  SUBYARD_INSTALL_LOCK_FDS=()
  local -A resource_fds=()
  local -a resources
  mapfile -t resources < <(printf '%s\n' "${@:3}" | LC_ALL=C sort -u)
  for resource in "${resources[@]}"; do
    directory=$(dirname "$resource")
    if [ ! -e "$directory" ]; then install -d -o root -g root -m 0755 "$directory"; fi
    python3 -B "$helper" prepare-lock "$owner" "$resource"
    exec {fd}>>"$directory/.$(basename "$resource").native-install.lock"
    flock -n "$fd" || die 'native installer is busy; retry after the active installation drains'
    python3 -B "$helper" validate "$owner" "$resource" "$fd"
    SUBYARD_INSTALL_LOCK_FILES+=("$resource")
    SUBYARD_INSTALL_LOCK_FDS+=("$fd")
    resource_fds["$resource"]=$fd
    if [ "$resource" = "$primary" ]; then SUBYARD_INSTALL_LOCK_FD=$fd; fi
  done
  if [ -n "${SUBYARD_NATIVE_INSTALLED_BEFORE:-}" ]; then
    python3 -B "$helper" before "$owner" "$primary" "$SUBYARD_INSTALL_LOCK_FD" "$SUBYARD_NATIVE_INSTALLED_BEFORE" "${@:3}"
  fi
  local -a vector=()
  for resource in "${@:3}"; do vector+=("$resource" "${resource_fds[$resource]}"); done
  if [ -n "${SUBYARD_NATIVE_GENERATION:-}${SUBYARD_NATIVE_EXPECTED:-}${SUBYARD_NATIVE_EXPECTED_VECTOR:-}${SUBYARD_NATIVE_SUCCESSOR:-}" ]; then
    [ -n "${SUBYARD_NATIVE_GENERATION:-}" ] && [ -n "${SUBYARD_NATIVE_EXPECTED_VECTOR:-}" ] \
      || die 'incomplete native installer generation'
    if [ -n "${SUBYARD_NATIVE_SUCCESSOR:-}" ]; then
      python3 -B "$helper" vector "$owner" "$primary" "$SUBYARD_INSTALL_LOCK_FD" fence "$SUBYARD_NATIVE_EXPECTED_VECTOR" "$SUBYARD_NATIVE_GENERATION" "$SUBYARD_NATIVE_SUCCESSOR" "${vector[@]}"
    else
      python3 -B "$helper" vector "$owner" "$primary" "$SUBYARD_INSTALL_LOCK_FD" claim "$SUBYARD_NATIVE_EXPECTED_VECTOR" "$SUBYARD_NATIVE_GENERATION" '' "${vector[@]}"
    fi
  else
    # Direct ordinary install/removal also invalidates any delayed native
    # claim before changing the same resources, under all their actual locks.
    python3 -B "$helper" vector "$owner" "$primary" "$SUBYARD_INSTALL_LOCK_FD" invalidate '' '' '' "${vector[@]}"
  fi
}

subyard_install_custody_verify() {
  local index
  for index in "${!SUBYARD_INSTALL_LOCK_FILES[@]}"; do
    python3 -B "$SCRIPT_DIR/lib/native-install-custody.py" validate "$1" "${SUBYARD_INSTALL_LOCK_FILES[$index]}" "${SUBYARD_INSTALL_LOCK_FDS[$index]}"
  done
}
