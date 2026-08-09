#!/usr/bin/env bash
# Create a stopped-service full backup in one local yard and commit it to restic.
set -euo pipefail

die() { printf 'hermes restic backup: %s\n' "$*" >&2; exit 1; }
if [ "$(id -u)" -ne 0 ] && [ "${HERMES_BACKUP_TEST_ALLOW_NON_ROOT:-0}" != 1 ]; then
  die "must run as root"
fi

yard=""
restic_env=""
backup_type=scheduled
while [ "$#" -gt 0 ]; do
  case "$1" in
    --yard) [ "$#" -ge 2 ] || die "--yard needs a value"; yard="$2"; shift 2 ;;
    --restic-env) [ "$#" -ge 2 ] || die "--restic-env needs a value"; restic_env="$2"; shift 2 ;;
    --type) [ "$#" -ge 2 ] || die "--type needs a value"; backup_type="$2"; shift 2 ;;
    -h|--help)
      printf 'Usage: sudo backup-to-restic.sh --yard NAME --restic-env ROOT_FILE [--type scheduled|pre-update|pre-teardown]\n'
      exit 0
      ;;
    *) die "unknown argument $1" ;;
  esac
done
[[ "$yard" =~ ^[a-z0-9][a-z0-9-]*$ ]] || die "invalid yard name"
case "$backup_type" in scheduled|pre-update|pre-teardown) ;; *) die "invalid backup type" ;; esac
[ -n "$restic_env" ] && [ -f "$restic_env" ] && [ ! -L "$restic_env" ] \
  || die "root-owned restic env file is required"

expected_uid=0
[ "$(id -u)" -eq 0 ] || expected_uid="$(id -u)"
[ "$(stat -c %u "$restic_env")" -eq "$expected_uid" ] \
  || die "restic env file has the wrong owner"
mode="$(stat -c %a "$restic_env")"
(( (8#$mode & 077) == 0 )) || die "restic env file must not be group/world accessible"

profile_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$profile_dir/../../.." && pwd)"
yard_bin="${SUBYARD_YARD_BIN:-$root/bin/yard}"
[ -x "$yard_bin" ] || die "yard executable not found"

operator="${SUDO_USER:-$(id -un)}"
operator_home="$(getent passwd "$operator" | cut -d: -f6)"
run_yard() {
  if [ "$(id -un)" = "$operator" ]; then
    HOME="$operator_home" "$yard_bin" "$@"
  else
    runuser -u "$operator" -- env HOME="$operator_home" "$yard_bin" "$@"
  fi
}
effective_setting() {
  output="$(run_yard -Y "$yard" config show "$1")"
  value="$(printf '%s\n' "$output" | sed -n 's/^effective: //p')"
  [ "$(printf '%s\n' "$value" | wc -l)" -eq 1 ] && [[ "$value" =~ ^[a-zA-Z0-9_-]+$ ]] \
    || die "could not resolve $1"
  printf '%s\n' "$value"
}

instance="$(effective_setting INSTANCE_NAME)"
project="$(effective_setting INCUS_PROJECT)"
incus info "$instance" --project "$project" >/dev/null \
  || die "yard instance is unavailable"

local_stage="$(mktemp -d /var/tmp/subyard-hermes-restic.XXXXXX)"
chmod 0700 "$local_stage"
pending=0
remote_stage=""
cleanup() {
  status=$?
  trap - EXIT HUP INT TERM
  if [ "$pending" = 1 ]; then
    incus exec "$instance" --project "$project" -- \
      /usr/local/sbin/hermes-backup-finalize "$remote_stage" failure >/dev/null 2>&1 || true
  fi
  rm -rf -- "$local_stage"
  exit "$status"
}
trap cleanup EXIT HUP INT TERM

create_output="$(incus exec "$instance" --project "$project" -- \
  /usr/local/sbin/hermes-backup-create "$backup_type")"
remote_stage="$(printf '%s\n' "$create_output" | sed -n 's/^BACKUP_DIR=//p')"
remote_zip="$(printf '%s\n' "$create_output" | sed -n 's/^BACKUP_ZIP=//p')"
remote_metadata="$(printf '%s\n' "$create_output" | sed -n 's/^BACKUP_METADATA=//p')"
remote_sha="$(printf '%s\n' "$create_output" | sed -n 's/^BACKUP_SHA256=//p')"
case "$remote_stage" in /var/tmp/subyard-hermes-backup.*) ;; *) die "invalid remote staging path" ;; esac
[ "$remote_zip" = "$remote_stage/hermes-backup.zip" ] \
  && [ "$remote_metadata" = "$remote_stage/metadata.env" ] \
  && [[ "$remote_sha" =~ ^[0-9a-f]{64}$ ]] \
  || die "invalid remote backup result"
pending=1

local_zip="$local_stage/hermes-backup.zip"
local_metadata="$local_stage/metadata.env"
incus file pull "$instance$remote_zip" "$local_zip" --project "$project" --quiet
incus file pull "$instance$remote_metadata" "$local_metadata" --project "$project" --quiet
chmod 0600 "$local_zip" "$local_metadata"

"$profile_dir/verify-backup.py" "$local_zip" "$remote_sha" >/dev/null
grep -Fxq "sha256=$remote_sha" "$local_metadata" \
  || die "metadata SHA-256 does not match the archive"
grep -Fxq "hermes_commit=$(sed -n 's/^HERMES_COMMIT=//p' "$profile_dir/profile.conf")" \
  "$local_metadata" || die "metadata commit does not match this profile"

# The file is root-owned, non-writable by other users and explicitly selected
# by the operator. It uses restic's standard RESTIC_* variables.
set -a
# shellcheck disable=SC1090
. "$restic_env"
set +a
: "${RESTIC_REPOSITORY:?restic env must set RESTIC_REPOSITORY}"
if [ -z "${RESTIC_PASSWORD:-}" ] && [ -z "${RESTIC_PASSWORD_FILE:-}" ] \
  && [ -z "${RESTIC_PASSWORD_COMMAND:-}" ]; then
  die "restic env must configure a password source"
fi
restic snapshots --json >/dev/null

restic_json="$local_stage/restic.jsonl"
(
  cd "$local_stage"
  restic backup --json \
    --tag subyard-hermes --tag "yard-$yard" --tag "type-$backup_type" \
    "$(basename "$local_zip")" "$(basename "$local_metadata")"
) > "$restic_json"
snapshot_id="$(python3 - "$restic_json" <<'PY'
import json
import sys

snapshot = ""
with open(sys.argv[1], encoding="utf-8") as stream:
    for line in stream:
        event = json.loads(line)
        if event.get("message_type") == "summary":
            snapshot = event.get("snapshot_id") or ""
if not snapshot:
    raise SystemExit("restic summary omitted snapshot_id")
print(snapshot)
PY
)"
restic_listing="$(mktemp "$local_stage/restic-ls.XXXXXX")"
chmod 0600 "$restic_listing"
restic ls "$snapshot_id" > "$restic_listing"
grep -Fxq "/$(basename "$local_zip")" "$restic_listing" \
  || die "archive is absent from the restic snapshot"
grep -Fxq "/$(basename "$local_metadata")" "$restic_listing" \
  || die "metadata is absent from the restic snapshot"

{
  printf 'commit=%s\n' "$(sed -n 's/^HERMES_COMMIT=//p' "$profile_dir/profile.conf")"
  printf 'snapshot=%s\n' "$snapshot_id"
  printf 'sha256=%s\n' "$remote_sha"
} | incus exec "$instance" --project "$project" -- sh -euc '
  umask 077
  marker="$(mktemp /srv/hermes/.last-verified-backup.XXXXXX)"
  cat > "$marker"
  chown root:root "$marker"
  chmod 0600 "$marker"
  mv -f "$marker" /srv/hermes/.last-verified-backup
'

incus exec "$instance" --project "$project" -- \
  /usr/local/sbin/hermes-backup-finalize "$remote_stage" success
pending=0

restic forget --tag subyard-hermes --tag "yard-$yard" --group-by tags \
  --keep-daily 7 --keep-weekly 4 --keep-monthly 6 --prune
printf 'hermes restic backup OK: yard=%s snapshot=%s sha256=%s\n' \
  "$yard" "$snapshot_id" "$remote_sha"
