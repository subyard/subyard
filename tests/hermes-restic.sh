#!/usr/bin/env bash
# Host-free L0 orchestration checks with fake yard, Incus and restic.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PROFILE_DIR="$ROOT/config/profiles/hermes"
BACKUP="$PROFILE_DIR/backup-to-restic.sh"
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

tmp="$(mktemp -d)"
trap 'rm -rf -- "$tmp"' EXIT
mkdir -p "$tmp/bin" "$tmp/remote"

python3 - "$tmp/remote/hermes-backup.zip" <<'PY'
import sys
import zipfile

with zipfile.ZipFile(sys.argv[1], "w") as output:
    output.writestr("config.yaml", "model:\n  provider: fixture\n")
    output.writestr(".serve.env", "HERMES_DASHBOARD_SESSION_TOKEN=" + "0" * 64 + "\n")
PY
sha="$(sha256sum "$tmp/remote/hermes-backup.zip" | awk '{print $1}')"
size="$(stat -c %s "$tmp/remote/hermes-backup.zip")"
cat > "$tmp/remote/metadata.env" <<EOF
yard=yard-hermes
created_utc=2026-07-29T00:00:00Z
hermes_version=0.19.0
hermes_tag=v2026.7.20
hermes_commit=3ef6bbd201263d354fd83ec55b3c306ded2eb72a
hermes_home=/srv/hermes
size=$size
sha256=$sha
service_was_active=1
backup_type=scheduled
EOF

cat > "$tmp/bin/yard" <<'YARD'
#!/usr/bin/env bash
set -euo pipefail
setting="${*: -1}"
case "$setting" in
  YARD_INSTANCE_NAME) value=yard-hermes ;;
  INCUS_PROJECT) value=subyard-hermes ;;
  *) exit 90 ;;
esac
printf 'setting: %s\neffective: %s\n' "$setting" "$value"
YARD
cat > "$tmp/bin/incus" <<'INCUS'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  info) exit 0 ;;
  exec)
    args="$*"
    case "$args" in
      *hermes-backup-create*)
        printf '%s\n' \
          'BACKUP_DIR=/var/tmp/subyard-hermes-backup.fixture' \
          'BACKUP_ZIP=/var/tmp/subyard-hermes-backup.fixture/hermes-backup.zip' \
          'BACKUP_METADATA=/var/tmp/subyard-hermes-backup.fixture/metadata.env' \
          "BACKUP_SHA256=$HERMES_TEST_REMOTE_SHA"
        ;;
      *hermes-backup-finalize*)
        printf '%s\n' "$args" >> "$HERMES_TEST_FINALIZE_LOG"
        ;;
      *'sh -euc'*)
        cat > "$HERMES_TEST_MARKER_LOG"
        ;;
      *) exit 91 ;;
    esac
    ;;
  file)
    [ "${2:-}" = pull ]
    remote="$3"
    destination="$4"
    case "$remote" in
      */hermes-backup.zip) cp "$HERMES_TEST_REMOTE_DIR/hermes-backup.zip" "$destination" ;;
      */metadata.env) cp "$HERMES_TEST_REMOTE_DIR/metadata.env" "$destination" ;;
      *) exit 92 ;;
    esac
    ;;
  *) exit 93 ;;
esac
INCUS
cat > "$tmp/bin/restic" <<'RESTIC'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$HERMES_TEST_RESTIC_LOG"
case "${1:-}" in
  snapshots) printf '[]\n' ;;
  backup)
    [ "${HERMES_TEST_RESTIC_FAIL:-0}" != 1 ] || exit 94
    printf '%s\n' '{"message_type":"summary","snapshot_id":"0123456789abcdef"}'
    ;;
  ls)
    printf '%s\n' '/hermes-backup.zip' '/metadata.env'
    if [ "${HERMES_TEST_RESTIC_LARGE_LS:-0}" = 1 ]; then
      for index in $(seq 1 20000); do
        printf '/filler-%05d\n' "$index"
      done
    fi
    ;;
  forget) ;;
  *) exit 95 ;;
esac
RESTIC
chmod +x "$tmp/bin/yard" "$tmp/bin/incus" "$tmp/bin/restic"

restic_env="$tmp/restic.env"
cat > "$restic_env" <<'EOF'
RESTIC_REPOSITORY=/tmp/fake-hermes-restic
RESTIC_PASSWORD=fake-test-password
EOF
chmod 0600 "$restic_env"

common_env=(
  PATH="$tmp/bin:$PATH"
  SUBYARD_YARD_BIN="$tmp/bin/yard"
  HERMES_BACKUP_TEST_ALLOW_NON_ROOT=1
  HERMES_TEST_REMOTE_DIR="$tmp/remote"
  HERMES_TEST_REMOTE_SHA="$sha"
  HERMES_TEST_FINALIZE_LOG="$tmp/finalize.log"
  HERMES_TEST_MARKER_LOG="$tmp/marker.log"
  HERMES_TEST_RESTIC_LOG="$tmp/restic.log"
  HERMES_TEST_RESTIC_LARGE_LS=1
)

env "${common_env[@]}" "$BACKUP" \
  --yard hermes --restic-env "$restic_env" --type scheduled > "$tmp/output"
grep -Fq 'snapshot=0123456789abcdef' "$tmp/output" \
  || fail "snapshot ID was not reported"
grep -Fq 'hermes-backup-finalize /var/tmp/subyard-hermes-backup.fixture success' \
  "$tmp/finalize.log" || fail "remote staging was not finalized after confirmation"
grep -Fxq 'commit=3ef6bbd201263d354fd83ec55b3c306ded2eb72a' \
  "$tmp/marker.log" || fail "verified-backup marker omitted the commit"
grep -Fxq 'snapshot=0123456789abcdef' "$tmp/marker.log" \
  || fail "verified-backup marker omitted the snapshot"
grep -Fq 'forget --tag subyard-hermes --tag yard-hermes' "$tmp/restic.log" \
  || fail "retention policy is not scoped to this Hermes yard"

: > "$tmp/finalize.log"
if env "${common_env[@]}" HERMES_TEST_RESTIC_FAIL=1 "$BACKUP" \
  --yard hermes --restic-env "$restic_env" --type scheduled \
  >"$tmp/failure.out" 2>&1; then
  fail "restic backup failure was accepted"
fi
grep -Fq 'hermes-backup-finalize /var/tmp/subyard-hermes-backup.fixture failure' \
  "$tmp/finalize.log" || fail "service state was not recovered after restic failure"

printf 'ok: Hermes L0 restic orchestration and failure recovery\n'
