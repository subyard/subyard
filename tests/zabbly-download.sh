#!/usr/bin/env bash
# Host-free regression coverage for the shared Zabbly signing-key downloader.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=tests/helpers/test-lib.sh
. "$ROOT/tests/helpers/test-lib.sh"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/subyard-zabbly-download.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

mkdir -p "$TMP/bin" "$TMP/keyrings"
cat > "$TMP/bin/curl" <<'SH'
#!/usr/bin/env bash
set -euo pipefail

output=
arguments="$*"
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output) output="$2"; shift 2 ;;
    --write-out) shift 2 ;;
    *) shift ;;
  esac
done
[ -n "$output" ] || exit 2
printf '%s\n' "$arguments" >> "$ZABBLY_TEST_CURL_LOG"
count=0
[ ! -s "$ZABBLY_TEST_COUNT" ] || count="$(cat "$ZABBLY_TEST_COUNT")"
count=$((count + 1))
printf '%s\n' "$count" > "$ZABBLY_TEST_COUNT"
case "$ZABBLY_TEST_SCENARIO" in
  transient)
    if [ "$count" -lt 3 ]; then printf '000'; exit 7; fi
    printf 'new-key\n' > "$output"
    printf '200'
    ;;
  exhausted)
    printf '000'
    exit 28
    ;;
  permanent-http)
    printf 'not found\n' > "$output"
    printf '404'
    exit 22
    ;;
  permanent-tls)
    printf '000'
    exit 60
    ;;
  empty)
    : > "$output"
    printf '200'
    ;;
  partial)
    printf 'partial' > "$output"
    printf '200'
    exit 18
    ;;
  success)
    printf 'new-key\n' > "$output"
    printf '200'
    ;;
  *) exit 2 ;;
esac
SH
chmod 0755 "$TMP/bin/curl"
export PATH="$TMP/bin:$PATH"

# shellcheck source=scripts/lib/download.sh
. "$ROOT/scripts/lib/download.sh"
sleep() { :; }

run_scenario() {
  local scenario="$1" destination="$2"
  export ZABBLY_TEST_SCENARIO="$scenario"
  export ZABBLY_TEST_COUNT="$TMP/$scenario.count"
  export ZABBLY_TEST_CURL_LOG="$TMP/$scenario.curl.log"
  rm -f "$ZABBLY_TEST_COUNT"
  rm -f "$ZABBLY_TEST_CURL_LOG"
  subyard_download_https_atomic \
    https://pkgs.zabbly.com/key.asc "$destination" 0640 "$(id -u)" "$(id -g)"
}

key="$TMP/keyrings/zabbly.asc"
run_scenario transient "$key"
[ "$(cat "$key")" = new-key ] || fail "transient success did not publish the payload"
[ "$(cat "$ZABBLY_TEST_COUNT")" -eq 3 ] || fail "transient failure did not retry to success"
[ "$(stat -c '%a' "$key")" = 640 ] || fail "published key has the wrong mode"
[ "$(stat -c '%u:%g' "$key")" = "$(id -u):$(id -g)" ] \
  || fail "published key has the wrong owner"
grep -Fq -- \
  '--fail --silent --show-error --location --max-redirs 5 --proto =https --proto-redir =https --tlsv1.2 --connect-timeout 5 --max-time 10' \
  "$ZABBLY_TEST_CURL_LOG" \
  || fail "curl invocation drifted from the HTTPS and per-attempt timeout contract"

for scenario in exhausted permanent-http permanent-tls empty partial; do
  printf 'known-good\n' > "$key"
  chmod 0644 "$key"
  if run_scenario "$scenario" "$key"; then
    fail "$scenario failure was reported as success"
  fi
  [ "$(cat "$key")" = known-good ] || fail "$scenario failure replaced the existing key"
  case "$scenario" in
    permanent-http | permanent-tls)
      [ "$(cat "$ZABBLY_TEST_COUNT")" -eq 1 ] \
        || fail "$scenario failure was retried"
      ;;
    *)
      [ "$(cat "$ZABBLY_TEST_COUNT")" -eq 4 ] \
        || fail "$scenario failure did not exhaust the bounded retry budget"
      ;;
  esac
done

run_scenario success "$key"
[ "$(cat "$key")" = new-key ] || fail "successful refresh did not replace the existing key"
[ -z "$(find "$TMP/keyrings" -maxdepth 1 -name '.zabbly.asc.tmp.*' -print -quit)" ] \
  || fail "download left a temporary key behind"

if subyard_download_https_atomic \
  http://pkgs.zabbly.com/key.asc "$key" 0644 "$(id -u)" "$(id -g)"; then
  fail "non-HTTPS URL was accepted"
fi

grep -Fq 'subyard_download_https_atomic https://pkgs.zabbly.com/key.asc "$key" 0644 root root' \
  "$ROOT/scripts/lib/host.sh" \
  || fail "host Incus installer does not use the shared downloader"
grep -Fq 'subyard_download_https_atomic' "$ROOT/scripts/e2e-lab/provision.sh" \
  && ! grep -Fq 'curl -fsSL https://pkgs.zabbly.com/key.asc' \
    "$ROOT/scripts/e2e-lab/provision.sh" \
  || fail "test-vms provisioner bypasses the shared downloader"

printf 'PASS: bounded atomic Zabbly signing-key download\n'
