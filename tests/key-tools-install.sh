#!/usr/bin/env bash
# Host-free pinned age/SOPS installer: artifact selection, checksums, atomic replacement, idempotence.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

mkdir -p "$TMP/fixture/age" "$TMP/bin" "$TMP/home"
cat > "$TMP/fixture/age/age" <<'SH'
#!/usr/bin/env bash
echo 'age 9.9.1'
SH
cat > "$TMP/fixture/age/age-keygen" <<'SH'
#!/usr/bin/env bash
echo 'age-keygen 9.9.1'
SH
cat > "$TMP/fixture/sops" <<'SH'
#!/usr/bin/env bash
echo 'sops 8.8.2'
SH
chmod +x "$TMP/fixture/age/age" "$TMP/fixture/age/age-keygen" "$TMP/fixture/sops"
tar -czf "$TMP/fixture/age.tar.gz" -C "$TMP/fixture" age
age_sha="$(sha256sum "$TMP/fixture/age.tar.gz" | cut -d' ' -f1)"
sops_sha="$(sha256sum "$TMP/fixture/sops" | cut -d' ' -f1)"

cat > "$TMP/bin/curl" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
out=''; url=''
while [ $# -gt 0 ]; do
  case "$1" in -o|--output) out="$2"; shift 2 ;; -*) shift ;; *) url="$1"; shift ;; esac
done
[ -n "$out" ] && [ -n "$url" ]
printf '%s\n' "$url" >> "$KEY_TOOL_CURL_LOG"
case "$url" in
  *age-v9.9.1-linux-amd64.tar.gz) cp "$KEY_TOOL_AGE_FIXTURE" "$out" ;;
  *sops-v8.8.2.linux.amd64) cp "$KEY_TOOL_SOPS_FIXTURE" "$out" ;;
  *) exit 22 ;;
esac
SH
chmod +x "$TMP/bin/curl"

# shellcheck source=tests/helpers/test-context.sh
. "$ROOT/tests/helpers/test-context.sh"
setup_test_context "$TMP"
export HOME="$TMP/home"
export SUBYARD_OPERATOR_HOME="$HOME"
export SUBYARD_CONFIG_HOME="$TMP/config"
export SUBYARD_HOME="$TMP/data"
export SUBYARD_KEYS_TOOLS_DIR="$TMP/tools"
export SUBYARD_AGE_VERSION=9.9.1
export SUBYARD_SOPS_VERSION=8.8.2
export SUBYARD_AGE_SHA256_AMD64="$age_sha"
export SUBYARD_SOPS_SHA256_AMD64="$sops_sha"
export KEY_TOOL_CURL_LOG="$TMP/curl.log"
export KEY_TOOL_AGE_FIXTURE="$TMP/fixture/age.tar.gz"
export KEY_TOOL_SOPS_FIXTURE="$TMP/fixture/sops"

PATH="$TMP/bin:$PATH" bash "$ROOT/scripts/install-key-tools.sh" --yes >/dev/null
[ "$("$SUBYARD_KEYS_TOOLS_DIR/bin/age" --version)" = 'age 9.9.1' ] || fail 'age version mismatch'
[ "$("$SUBYARD_KEYS_TOOLS_DIR/bin/sops" --version)" = 'sops 8.8.2' ] || fail 'SOPS version mismatch'
[ "$(stat -c '%a' "$SUBYARD_KEYS_TOOLS_DIR/bin/sops")" = 755 ] || fail 'SOPS mode mismatch'
[ "$(wc -l < "$KEY_TOOL_CURL_LOG")" = 2 ] || fail 'installer fetched unexpected artifact count'

PATH="$TMP/bin:$PATH" bash "$ROOT/scripts/install-key-tools.sh" --yes >/dev/null
[ "$(wc -l < "$KEY_TOOL_CURL_LOG")" = 2 ] || fail 'converged installer downloaded again'

receipt="$SUBYARD_KEYS_TOOLS_DIR/bin/.subyard-native-tools.json"
[ "$(stat -c '%a' "$receipt")" = 600 ] || fail 'native tool receipt is not protected'
# The same operator may invoke yard through sg with a different primary group.
KEY_TOOL_REAL_ID="$(command -v id)"
export KEY_TOOL_REAL_ID
cat > "$TMP/bin/id" <<'SH'
#!/usr/bin/env bash
if [ "$*" = '-g' ]; then printf '4294967294\n'; else exec "$KEY_TOOL_REAL_ID" "$@"; fi
SH
chmod 0755 "$TMP/bin/id"
receipt_inode="$(stat -c '%i' "$receipt")"
PATH="$TMP/bin:$PATH" bash "$ROOT/scripts/install-key-tools.sh" --check
PATH="$TMP/bin:$PATH" bash "$ROOT/scripts/install-key-tools.sh" --yes >/dev/null
[ "$(wc -l < "$KEY_TOOL_CURL_LOG")" = 2 ] || fail 'changed primary group downloaded trusted tools again'
[ "$(stat -c '%i' "$receipt")" = "$receipt_inode" ] || fail 'changed primary group rewrote trusted receipt'
printf '\n# replaced bytes, same reported version\n' >> "$SUBYARD_KEYS_TOOLS_DIR/bin/age"
if PATH="$TMP/bin:$PATH" bash "$ROOT/scripts/install-key-tools.sh" --check; then fail 'replaced native bytes accepted by version only'; fi
PATH="$TMP/bin:$PATH" bash "$ROOT/scripts/install-key-tools.sh" --yes >/dev/null
[ "$(wc -l < "$KEY_TOOL_CURL_LOG")" = 4 ] || fail 'replaced tool did not use verified archive repair'
receipt_before="$(sha256sum "$receipt" | cut -d' ' -f1)"
wrong_pin="$(printf '0%.0s' {1..64})"
if SUBYARD_AGE_SHA256_AMD64="$wrong_pin" PATH="$TMP/bin:$PATH" bash "$ROOT/scripts/install-key-tools.sh" --check; then fail 'same-version changed pin accepted'; fi
if SUBYARD_AGE_SHA256_AMD64="$wrong_pin" PATH="$TMP/bin:$PATH" bash "$ROOT/scripts/install-key-tools.sh" --yes > "$TMP/same-version-pin.out" 2>&1; then fail 'unchecked same-version replacement installed'; fi
[ "$(sha256sum "$receipt" | cut -d' ' -f1)" = "$receipt_before" ] || fail 'failed pin verification replaced trusted receipt'
PATH="$TMP/bin:$PATH" bash "$ROOT/scripts/install-key-tools.sh" --check

export SUBYARD_KEYS_TOOLS_DIR="$TMP/bad-tools"
SUBYARD_AGE_SHA256_AMD64="$(printf '0%.0s' {1..64})"
export SUBYARD_AGE_SHA256_AMD64
if PATH="$TMP/bin:$PATH" bash "$ROOT/scripts/install-key-tools.sh" --yes >"$TMP/bad.out" 2>&1; then
  fail 'checksum mismatch unexpectedly succeeded'
fi
[ ! -e "$SUBYARD_KEYS_TOOLS_DIR/bin/age" ] || fail 'checksum failure published a binary'
grep -Fq 'checksum mismatch' "$TMP/bad.out" || fail 'checksum failure diagnostic missing'

printf 'ok: pinned age/SOPS installer is checksum-verified, atomic and idempotent\n'
