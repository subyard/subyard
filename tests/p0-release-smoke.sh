#!/usr/bin/env bash
# Exercise archive selection without sourcing the disposable-VM setup.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=tests/helpers/test-lib.sh
. "$ROOT/tests/helpers/test-lib.sh"
eval "$(sed -n '/^write_retired_release_inputs() {/,/^}/p' "$ROOT/dev/e2e/p0-release-smoke.sh")"

# Retain the small archives and diagnostics as test evidence.
mkdir -p "$ROOT/.build"
STATE_ROOT="$(mktemp -d "$ROOT/.build/p0-release-smoke.XXXXXX")"
RELEASE_ROOT="$STATE_ROOT/release assets"
CANDIDATE_VERSION=0.31.0-test
RETIRED_REGISTRY="$STATE_ROOT/retired.json"
RETIRED_DECLARATION="$STATE_ROOT/declaration.json"
mkdir -p "$RELEASE_ROOT" "$STATE_ROOT/runtime/config"
for arch in amd64 arm64; do
  printf '{"architecture":"%s"}\n' "$arch" > "$STATE_ROOT/runtime/config/release-transition.json"
  tar -czf "$RELEASE_ROOT/subyard-$CANDIDATE_VERSION-linux-$arch.tar.gz" -C "$STATE_ROOT/runtime" config
done

# Registry transformations are covered separately; preserve the selected input here.
cat > "$STATE_ROOT/registry-fixture" <<'EOF'
#!/usr/bin/env bash
cat "$2"
EOF
chmod 0700 "$STATE_ROOT/registry-fixture"
go() { :; }
uname() { printf '%s\n' "$machine"; }
for machine in x86_64 aarch64 arm64; do
  expected=arm64
  [ "$machine" != x86_64 ] || expected=amd64
  write_retired_release_inputs
  [ "$(cat "$RETIRED_REGISTRY")" = "{\"architecture\":\"$expected\"}" ] \
    || fail "selected the wrong archive for $machine"
  cmp "$RETIRED_REGISTRY" "$RETIRED_DECLARATION" || fail 'declaration used a different archive'
done

assert_refusal() {
  local expected="$1" rc=0
  (write_retired_release_inputs) > "$STATE_ROOT/refusal.log" 2>&1 || rc=$?
  [ "$rc" = 2 ] || fail "expected archive refusal, got exit $rc"
  grep -Fq "$expected" "$STATE_ROOT/refusal.log" || fail 'missing archive diagnostic'
}
machine=x86_64
bundle="$RELEASE_ROOT/subyard-$CANDIDATE_VERSION-linux-amd64.tar.gz"
mv "$bundle" "$bundle.saved"
assert_refusal "release smoke cannot find a regular candidate bundle for amd64: $bundle"
mkdir "$bundle"
assert_refusal "release smoke cannot find a regular candidate bundle for amd64: $bundle"
mv "$bundle" "$bundle.directory"
ln -s "$bundle.saved" "$bundle"
assert_refusal "release smoke cannot find a regular candidate bundle for amd64: $bundle"
machine=unsupported
assert_refusal 'release smoke candidate bundle architecture is unsupported'
printf 'PASS: release smoke archive selection (both architectures, missing, directory, symlink, unsupported)\n'
