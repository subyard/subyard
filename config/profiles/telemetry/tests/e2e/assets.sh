#!/usr/bin/env bash
# Product bytes come from the verified runtime; only the test load driver is built.
telemetry_prepare_assets() {
  local root="$1" destination="$2" engine binary manifest
  # shellcheck source=tests/helpers/release-candidate.sh
  . "$root/tests/helpers/release-candidate.sh"
  engine="$(release_candidate_prepare "$root")" || {
    printf 'telemetry fixture requires a verified frozen runtime\n' >&2; return 2;
  }
  binary="${engine%/bin/yard-engine}/config/profiles/telemetry/bin/subyard-telemetry"
  manifest="${engine%/bin/yard-engine}/runtime-files.sha256"
  [ -f "$binary" ] && [ ! -L "$binary" ] && [ -x "$binary" ] \
    && awk '$2 == "./config/profiles/telemetry/bin/subyard-telemetry" && length($1) == 64 && $1 !~ /[^0-9a-f]/ { found++ } END { exit(found != 1) }' "$manifest" \
    || { printf 'frozen telemetry executable is missing or not manifest-bound\n' >&2; return 2; }
  install -d -m 0700 "$destination"
  install -m 0755 "$binary" "$destination/subyard-telemetry"
  timeout --signal=TERM --kill-after=5 1200 env CGO_ENABLED=0 go -C "$root" build \
    -buildvcs=false -mod=readonly -trimpath -ldflags '-s -w' \
    -o "$destination/otlp-load" ./dev/telemetry-compare
}
