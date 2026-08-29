#!/usr/bin/env bash
# Restore entrypoints and shell files captured by migrate-source-install.sh.
set -euo pipefail

RECOVERY_ROOT="${SUBYARD_SOURCE_RECOVERY_ROOT:-${SUBYARD_HOME:-$HOME/.subyard}/recovery/pre-go-source}"
INCOMPLETE=0
while [ $# -gt 0 ]; do
  case "$1" in
    --recovery-root) [ $# -ge 2 ] || exit 2; RECOVERY_ROOT="$2"; shift 2 ;;
    --incomplete) INCOMPLETE=1; shift ;;
    *) printf 'restore-source-install: unknown option: %s\n' "$1" >&2; exit 2 ;;
  esac
done

fail() { printf 'restore-source-install: %s\n' "$*" >&2; exit 1; }
uid="$(id -u)"
owned_regular() {
  [ -f "$1" ] && [ ! -L "$1" ] && [ "$(stat -c '%u' -- "$1")" = "$uid" ]
}
owned_directory() {
  [ -d "$1" ] && [ ! -L "$1" ] && [ "$(stat -c '%u' -- "$1")" = "$uid" ]
}
owned_symlink() {
  [ -L "$1" ] && [ "$(stat -c '%u' -- "$1")" = "$uid" ]
}
protected_regular() {
  local mode
  owned_regular "$1" || return 1
  mode="$(stat -c '%a' -- "$1")"
  (( (8#$mode & 8#022) == 0 ))
}
digest_matches() {
  [ -f "$1" ] && [ ! -L "$1" ] \
    && [ "$(sha256sum "$1" | cut -d' ' -f1)" = "$2" ]
}
valid_digest() {
  [[ "$1" =~ ^[0-9a-f]{64}$ ]]
}
command -v sync >/dev/null 2>&1 || fail "sync is required for durable source recovery"
persist() {
  sync -f -- "$1" || fail "could not persist source recovery state: $1"
}
persist_nearest() {
  local path="$1" parent
  while [ ! -e "$path" ] && [ ! -L "$path" ]; do
    parent="$(dirname "$path")"
    [ "$parent" != "$path" ] || fail "could not find a recovery persistence boundary"
    path="$parent"
  done
  persist "$path"
}

case "$RECOVERY_ROOT" in "$HOME"/*) ;; *) fail "recovery root must be inside the operator home" ;; esac
owned_directory "$RECOVERY_ROOT" \
  || fail "recovery root is missing or not operator-owned"
root_mode="$(stat -c '%a' -- "$RECOVERY_ROOT")"
(( (8#$root_mode & 8#022) == 0 )) \
  || fail "recovery root is group/world writable"

read_value() {
  protected_regular "$RECOVERY_ROOT/$1" \
    || fail "recovery metadata is incomplete or unsafe: $1"
  IFS= read -r REPLY < "$RECOVERY_ROOT/$1"
  [ -n "$REPLY" ] || fail "recovery metadata is empty: $1"
}

transaction_schema=''; transaction_phase=''; transaction_step=''
if [ -e "$RECOVERY_ROOT/transaction" ] || [ -L "$RECOVERY_ROOT/transaction" ]; then
  protected_regular "$RECOVERY_ROOT/transaction" \
    || fail "recovery transaction is missing or unsafe"
  while IFS='=' read -r key value; do
    case "$key" in
      schema) [ -z "$transaction_schema" ] || fail "duplicate recovery transaction schema"
        transaction_schema="$value" ;;
      phase) [ -z "$transaction_phase" ] || fail "duplicate recovery transaction phase"
        transaction_phase="$value" ;;
      step) [ -z "$transaction_step" ] || fail "duplicate recovery transaction step"
        transaction_step="$value" ;;
      *) fail "unknown recovery transaction field" ;;
    esac
  done < "$RECOVERY_ROOT/transaction"
  [ "$transaction_schema" = 1 ] && [ -n "$transaction_phase" ] && [ -n "$transaction_step" ] \
    || fail "recovery transaction is incomplete"
  case "$transaction_phase:$transaction_step" in
    prepared:none|applying:config-import|applying:legacy-archive|applying:state-migration|\
applying:source-import-ready|\
applying:shell-integration|applying:entrypoint-switch|complete:complete) ;;
    *) fail "invalid recovery transaction phase/step" ;;
  esac
else
  transaction_phase=legacy-complete
fi
if [ -e "$RECOVERY_ROOT/source-import.state" ] ||
  [ -L "$RECOVERY_ROOT/source-import.state" ]; then
  protected_regular "$RECOVERY_ROOT/source-import.state" \
    || fail "source import seal is unsafe"
  IFS= read -r import_state < "$RECOVERY_ROOT/source-import.state"
  [ "$import_state" = committed ] || fail "source import seal is invalid"
  fail "source import is committed; restore would invalidate release migration history"
fi
if [ "$INCOMPLETE" = 1 ]; then
  case "$transaction_phase" in prepared|applying) ;; *)
    fail "incomplete recovery requires a prepared or applying transaction" ;;
  esac
else
  case "$transaction_phase" in complete|legacy-complete) ;; *)
    fail "source migration is incomplete; rerun the installer before one-time recovery" ;;
  esac
fi

read_value bin-dir; bin_dir="$REPLY"
read_value runtime-launcher; runtime_launcher="$REPLY"
read_value yard.target; yard_target="$REPLY"
read_value sy.target; sy_target="$REPLY"
read_value yard.temp; yard_temp="$REPLY"
read_value sy.temp; sy_temp="$REPLY"
read_value rc.path; rc="$REPLY"
read_value login-rc.path; login_rc="$REPLY"
read_value rc.temp; rc_temp="$REPLY"
read_value login-rc.temp; login_rc_temp="$REPLY"
read_value data-home; data_home="$REPLY"
read_value config-home; config_home="$REPLY"
read_value source-root; source_root="$REPLY"
for path in \
  "$bin_dir" "$runtime_launcher" "$yard_target" "$sy_target" "$yard_temp" "$sy_temp" \
  "$rc" "$login_rc" "$rc_temp" "$login_rc_temp" "$data_home" "$config_home" "$source_root"; do
  case "$path" in *$'\n'*|*$'\t'*) fail "invalid recovery path" ;; esac
done
for path in \
  "$bin_dir" "$yard_temp" "$sy_temp" "$rc" "$login_rc" "$rc_temp" "$login_rc_temp" \
  "$data_home" "$config_home" "$source_root"; do
  case "$path" in "$HOME"/*) ;; *) fail "recovery path escapes the operator home: $path" ;; esac
done
case "$rc_temp" in "$rc.subyard-migrate."*) ;; *) fail "invalid interactive shell temporary path" ;; esac
case "$login_rc_temp" in "$login_rc.subyard-migrate."*) ;; *)
  fail "invalid login shell temporary path" ;;
esac
case "$yard_temp" in "$bin_dir/.yard.subyard-migrate."*) ;; *)
  fail "invalid yard temporary link path" ;;
esac
case "$sy_temp" in "$bin_dir/.sy.subyard-migrate."*) ;; *)
  fail "invalid sy temporary link path" ;;
esac
owned_directory "$source_root" && owned_regular "$source_root/bin/yard" \
  || fail "retained source checkout is missing or unsafe"
owned_regular "$runtime_launcher" && [ -x "$runtime_launcher" ] \
  || fail "verified runtime launcher is missing or unsafe"
resolve_link_target() {
  case "$1" in
    /*) readlink -f -- "$1" ;;
    *) readlink -f -- "$bin_dir/$1" ;;
  esac
}
resolved_yard_target="$(resolve_link_target "$yard_target")" \
  || fail "recorded yard source entrypoint cannot be resolved"
resolved_sy_target="$(resolve_link_target "$sy_target")" \
  || fail "recorded sy source entrypoint cannot be resolved"
[ "$resolved_yard_target" = "$source_root/bin/yard" ] \
  && [ "$resolved_sy_target" = "$source_root/bin/yard" ] \
  || fail "recorded source entrypoints do not belong to the retained checkout"

read_value rc.after.sha256; rc_after="$REPLY"
read_value login-rc.after.sha256; login_after="$REPLY"
valid_digest "$rc_after" && valid_digest "$login_after" \
  || fail "shell recovery digest is invalid"
if [ "$transaction_phase" != legacy-complete ]; then
  for metadata in created.tsv created-directories.list temporary-files.tsv; do
    protected_regular "$RECOVERY_ROOT/$metadata" \
      && [ "$(stat -c '%a' -- "$RECOVERY_ROOT/$metadata")" = 600 ] \
      || fail "recovery metadata is incomplete or unsafe: $metadata"
  done
fi

validate_shell_file() {
  local label="$1" path="$2" after="$3" state before=''
  read_value "$label.state"; state="$REPLY"
  case "$state" in
    present)
      protected_regular "$RECOVERY_ROOT/$label.before" \
        || fail "shell recovery payload is unsafe: $label"
      before="$(sha256sum "$RECOVERY_ROOT/$label.before" | cut -d' ' -f1)"
      protected_regular "$path" \
        || fail "shell file changed during migration: $path"
      if [ "$INCOMPLETE" = 1 ]; then
        digest_matches "$path" "$before" || digest_matches "$path" "$after" \
          || fail "shell file changed during incomplete migration: $path"
      else
        digest_matches "$path" "$after" \
          || fail "shell file changed after migration: $path"
      fi
      ;;
    absent)
      if [ "$INCOMPLETE" = 1 ] && [ ! -e "$path" ] && [ ! -L "$path" ]; then
        :
      else
        protected_regular "$path" && digest_matches "$path" "$after" \
          || fail "shell file changed during migration: $path"
      fi
      ;;
    same) ;;
    *) fail "invalid shell recovery state for $label" ;;
  esac
}
validate_shell_file rc "$rc" "$rc_after"
read_value login-rc.state; login_state="$REPLY"
if [ "$login_state" != same ]; then
  validate_shell_file login-rc "$login_rc" "$login_after"
fi

validate_temporary_file() {
  local path="$1"
  if [ -e "$path" ] || [ -L "$path" ]; then
    [ "$INCOMPLETE" = 1 ] \
      || fail "completed migration retained a temporary file: $path"
    protected_regular "$path" \
      || fail "migration temporary file changed: $path"
  fi
}
validate_temporary_file "$rc_temp"
if [ "$login_state" != same ]; then
  validate_temporary_file "$login_rc_temp"
fi

validate_link() {
  local path="$1" before="$2" current
  owned_symlink "$path" || fail "entrypoint is not an operator-owned symbolic link: $path"
  current="$(readlink "$path")"
  if [ "$INCOMPLETE" = 1 ]; then
    [ "$current" = "$before" ] || [ "$current" = "$runtime_launcher" ] \
      || fail "entrypoint changed during incomplete migration: $path"
  else
    [ "$current" = "$runtime_launcher" ] \
      || fail "entrypoint changed after migration: $path"
  fi
}
validate_link "$bin_dir/yard" "$yard_target"
validate_link "$bin_dir/sy" "$sy_target"

validate_link_temporary() {
  local path="$1" before="$2" current
  if [ -e "$path" ] || [ -L "$path" ]; then
    [ "$INCOMPLETE" = 1 ] \
      || fail "completed migration retained a temporary link: $path"
    owned_symlink "$path" || fail "migration temporary link changed: $path"
    current="$(readlink "$path")"
    [ "$current" = "$before" ] || [ "$current" = "$runtime_launcher" ] \
      || fail "migration temporary link changed: $path"
  fi
}
validate_link_temporary "$yard_temp" "$yard_target"
validate_link_temporary "$sy_temp" "$sy_target"

canonical_test_yard_path() {
  case "$1" in
    "$config_home/yards/e2e-yard/config.env")
      REPLY="$config_home/yards/test-yard/config.env"
      ;;
    "$config_home/yards/e2e-yard")
      REPLY="$config_home/yards/test-yard"
      ;;
    "$config_home/yards/e2e-yard.env")
      REPLY="$config_home/yards/test-yard.env"
      ;;
    *)
      REPLY="$1"
      ;;
  esac
}

if [ -f "$RECOVERY_ROOT/created.tsv" ]; then
  while IFS=$'\t' read -r digest path; do
    [ -n "$path" ] || continue
    valid_digest "$digest" || fail "invalid created-file digest"
    case "$path" in *$'\n'*|*$'\t'*) fail "invalid created-file path" ;; esac
    case "$path" in "$config_home"/*) ;; *)
      fail "created-file record escapes the configuration root" ;;
    esac
    if [ ! -e "$path" ] && [ ! -L "$path" ]; then
      canonical_test_yard_path "$path"
      if [ "$REPLY" != "$path" ] && { [ -e "$REPLY" ] || [ -L "$REPLY" ]; }; then
        path="$REPLY"
      fi
    fi
    if [ -e "$path" ] || [ -L "$path" ]; then
      owned_regular "$path" && [ "$(stat -c '%a' -- "$path")" = 600 ] \
        && [ "$(stat -c '%h' -- "$path")" = 1 ] && digest_matches "$path" "$digest" \
        || fail "migrated file changed after installation: $path"
    elif [ "$INCOMPLETE" != 1 ]; then
      fail "migrated file disappeared after installation: $path"
    fi
  done < "$RECOVERY_ROOT/created.tsv"
fi

if [ -f "$RECOVERY_ROOT/temporary-files.tsv" ]; then
  while IFS=$'\t' read -r digest path; do
    [ -n "$path" ] || continue
    valid_digest "$digest" || fail "invalid temporary-file digest"
    case "$path" in *$'\n'*|*$'\t'*) fail "invalid temporary-file path" ;; esac
    case "$path" in "$config_home"/*.subyard-migrate.*) ;; *)
      fail "temporary-file record escapes the configuration root" ;;
    esac
    if [ -e "$path" ] || [ -L "$path" ]; then
      [ "$INCOMPLETE" = 1 ] \
        || fail "completed migration retained a temporary file: $path"
      owned_regular "$path" && [ "$(stat -c '%a' -- "$path")" = 600 ] \
        || fail "migration temporary file changed: $path"
    fi
  done < "$RECOVERY_ROOT/temporary-files.tsv"
fi

if [ -f "$RECOVERY_ROOT/created-directories.list" ]; then
  while IFS= read -r path; do
    [ -n "$path" ] || continue
    case "$path" in *$'\n'*|*$'\t'*) fail "invalid created-directory path" ;; esac
    case "$config_home/" in "$path/"*) ;; *)
      case "$path" in "$config_home"/*) ;; *)
        fail "created-directory record is outside the configuration topology" ;;
      esac ;;
    esac
    if [ ! -e "$path" ] && [ ! -L "$path" ]; then
      canonical_test_yard_path "$path"
      if [ "$REPLY" != "$path" ] && { [ -e "$REPLY" ] || [ -L "$REPLY" ]; }; then
        path="$REPLY"
      fi
    fi
    if [ -e "$path" ] || [ -L "$path" ]; then
      owned_directory "$path" || fail "created migration directory became unsafe: $path"
    fi
  done < "$RECOVERY_ROOT/created-directories.list"
fi

validate_legacy_data() {
  local label="$1" expected="$2" state path original=0 backup=0
  read_value "$label.state"; state="$REPLY"
  read_value "$label.path"; path="$REPLY"
  [ "$path" = "$expected" ] || fail "legacy data path does not match its recovery role"
  [ ! -e "$path" ] && [ ! -L "$path" ] || original=1
  [ ! -e "$RECOVERY_ROOT/$label.before" ] && [ ! -L "$RECOVERY_ROOT/$label.before" ] || backup=1
  case "$state" in
    absent)
      [ "$original" = 0 ] && [ "$backup" = 0 ] \
        || fail "absent legacy data was created during migration: $path"
      ;;
    present)
      if [ "$INCOMPLETE" = 1 ]; then
        [ $((original + backup)) -eq 1 ] \
          || fail "legacy data recovery state is ambiguous: $path"
      else
        [ "$original" = 0 ] && [ "$backup" = 1 ] \
          || fail "legacy data changed after migration: $path"
      fi
      if [ "$original" = 1 ]; then
        owned_directory "$path" || owned_regular "$path" \
          || fail "legacy data path became unsafe: $path"
      fi
      if [ "$backup" = 1 ]; then
        owned_directory "$RECOVERY_ROOT/$label.before" \
          || owned_regular "$RECOVERY_ROOT/$label.before" \
          || fail "legacy data recovery payload became unsafe: $label"
      fi
      ;;
    *) fail "invalid legacy data recovery state for $label" ;;
  esac
}
validate_legacy_data legacy-data-config "$data_home/config.env"
validate_legacy_data legacy-operator-overlay "$data_home/operator-overlay"

restore_legacy_data() {
  local label="$1" state path
  read_value "$label.state"; state="$REPLY"
  read_value "$label.path"; path="$REPLY"
  if [ "$state" = present ] && [ ! -e "$path" ] && [ ! -L "$path" ]; then
    mv -- "$RECOVERY_ROOT/$label.before" "$path"
  fi
}
restore_legacy_data legacy-data-config
restore_legacy_data legacy-operator-overlay

restore_shell_file() {
  local label="$1" path="$2" temporary="$3" state before_digest current_digest
  read_value "$label.state"; state="$REPLY"
  case "$state" in
    present)
      before_digest="$(sha256sum "$RECOVERY_ROOT/$label.before" | cut -d' ' -f1)"
      current_digest="$(sha256sum "$path" | cut -d' ' -f1)"
      if [ "$current_digest" != "$before_digest" ]; then
        rm -f -- "$temporary"
        install -m "$(stat -c '%a' "$RECOVERY_ROOT/$label.before")" \
          "$RECOVERY_ROOT/$label.before" "$temporary"
        mv -fT -- "$temporary" "$path"
      else
        rm -f -- "$temporary"
      fi
      ;;
    absent)
      rm -f -- "$path" "$temporary"
      ;;
    same) ;;
  esac
}
restore_shell_file rc "$rc" "$rc_temp"
if [ "$login_state" != same ]; then
  restore_shell_file login-rc "$login_rc" "$login_rc_temp"
fi

if [ -f "$RECOVERY_ROOT/created.tsv" ]; then
  while IFS=$'\t' read -r _ path; do
    if [ -n "$path" ]; then
      if [ ! -e "$path" ] && [ ! -L "$path" ]; then
        canonical_test_yard_path "$path"
        if [ "$REPLY" != "$path" ] && { [ -e "$REPLY" ] || [ -L "$REPLY" ]; }; then
          path="$REPLY"
        fi
      fi
      rm -f -- "$path"
    fi
  done < "$RECOVERY_ROOT/created.tsv"
fi
if [ -f "$RECOVERY_ROOT/temporary-files.tsv" ]; then
  while IFS=$'\t' read -r _ path; do
    if [ -n "$path" ]; then
      rm -f -- "$path"
    fi
  done < "$RECOVERY_ROOT/temporary-files.tsv"
fi
if [ -f "$RECOVERY_ROOT/created-directories.list" ]; then
  while IFS= read -r path; do
    if [ -n "$path" ]; then
      if [ ! -e "$path" ] && [ ! -L "$path" ]; then
        canonical_test_yard_path "$path"
        if [ "$REPLY" != "$path" ] && { [ -e "$REPLY" ] || [ -L "$REPLY" ]; }; then
          path="$REPLY"
        fi
      fi
      rmdir -- "$path" 2>/dev/null || true
    fi
  done < <(tac "$RECOVERY_ROOT/created-directories.list")
fi

restore_link() {
  local path="$1" before="$2" temporary="$3"
  if [ "$(readlink "$path")" != "$before" ]; then
    rm -f -- "$temporary"
    ln -s -- "$before" "$temporary"
    mv -fT -- "$temporary" "$path"
  else
    rm -f -- "$temporary"
  fi
}
restore_link "$bin_dir/sy" "$sy_target" "$sy_temp"
restore_link "$bin_dir/yard" "$yard_target" "$yard_temp"
persist "$data_home"
persist_nearest "$config_home"
persist "$bin_dir"
persist "$(dirname "$rc")"
if [ "$login_rc" != "$rc" ]; then
  persist "$(dirname "$login_rc")"
fi

if [ "$INCOMPLETE" = 1 ]; then
  suffix=interrupted
else
  suffix=restored
fi
consumed="$RECOVERY_ROOT.$suffix.$(date -u +%Y%m%dT%H%M%SZ).$$"
mv -- "$RECOVERY_ROOT" "$consumed"
persist "$(dirname "$RECOVERY_ROOT")"
if [ "$INCOMPLETE" = 1 ]; then
  printf 'recovered incomplete source migration from %s\n' "$source_root"
else
  printf 'restored source-linked yard entrypoints from %s\n' "$source_root"
fi
printf 'consumed recovery record retained at %s\n' "$consumed"
