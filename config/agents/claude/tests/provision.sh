#!/usr/bin/env bash
# Hermetic native CLI installation and managed-policy checks; no account or model API.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
hook="$ROOT/config/agents/claude/provision.sh"
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
tmp="$(mktemp -d /tmp/subyard-claude-provision.XXXXXX)"
trap 'rm -rf -- "$tmp"' EXIT
mkdir -p "$tmp/bin" "$tmp/releases" "$tmp/home/.claude"
printf '{"credential":"fixture-only"}\n' > "$tmp/home/.claude/.credentials.json"
auth_before="$(sha256sum "$tmp/home/.claude/.credentials.json")"

make_release() { # version [reported-version]
  local version="$1" reported="${2:-$1}" platform
  mkdir -p "$tmp/releases/$version"
  cat > "$tmp/main.c" <<EOF
#include <stdio.h>
#include <string.h>
int main(int argc, char **argv) {
  if (argc == 2 && strcmp(argv[1], "--version") == 0) {
    puts("$reported (Claude Code)");
    return 0;
  }
  return 64;
}
EOF
  cc -O2 -o "$tmp/native" "$tmp/main.c"
  for platform in linux-x64 linux-arm64; do
    mkdir -p "$tmp/releases/$version/$platform"
    install -m 0644 "$tmp/native" "$tmp/releases/$version/$platform/claude"
  done
  local sha
  sha="$(sha256sum "$tmp/native" | cut -d' ' -f1)"
  jq -n --arg version "$version" --arg sha "$sha" \
    '{version:$version, platforms:{"linux-x64":{binary:"claude",checksum:$sha},"linux-arm64":{binary:"claude",checksum:$sha}}}' \
    > "$tmp/releases/$version/manifest.json"
}
make_release 2.1.0
make_release 2.2.0
make_release 2.3.0
make_release 2.4.0 2.3.9
printf '2.1.0\n' > "$tmp/releases/latest"
cat > "$tmp/bin/curl" <<'CURL'
#!/usr/bin/env bash
set -euo pipefail
out='' url=''
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output) out="$2"; shift 2 ;;
    --proto|--proto-redir|--retry|--connect-timeout|--max-time) shift 2 ;;
    --*) shift ;;
    *) url="$1"; shift ;;
  esac
done
case "$url" in
  https://downloads.claude.ai/claude-code-releases/*)
    printf '%s\n' "$url" >> "$CLAUDE_TEST_ROOT/fetch.log"
    cp -- "$CLAUDE_TEST_ROOT/releases/${url#https://downloads.claude.ai/claude-code-releases/}" "$out" ;;
  *) exit 90 ;;
esac
CURL
chmod 0755 "$tmp/bin/curl"
export CLAUDE_TEST_ROOT="$tmp"
run_hook() { # installation root [architecture]
  local root="$1" arch="${2:-amd64}"
  env PATH="$tmp/bin:$PATH" HOME="$tmp/home" TMPDIR=/tmp \
    CLAUDE_POLICY_TEST_ALLOW_NON_ROOT=1 CLAUDE_ARCH="$arch" \
    CLAUDE_INSTALL_ROOT="$root/opt/subyard/claude" \
    CLAUDE_BIN_LINK="$root/usr/local/bin/claude" \
    CLAUDE_POLICY_DIR="$root/etc/claude-code/managed-settings.d" \
    CLAUDE_POLICY_CHECK_PATH="$root/usr/local/bin/claude-policy-check" \
    bash "$hook"
}
root="$tmp/install"
runtime="$root/opt/subyard/claude/claude"
link="$root/usr/local/bin/claude"
check="$root/usr/local/bin/claude-policy-check"
policy_dir="$root/etc/claude-code/managed-settings.d"
policy="$policy_dir/50-subyard-git-approval.json"
install -d -m 0755 "$policy_dir"
printf '{"permissions":{"deny":["Bash(secret *)"]}}\n' > "$policy_dir/10-foreign.json"
before="$(sha256sum "$policy_dir/10-foreign.json")"
run_hook "$root" >/dev/null
[ -x "$runtime" ] && [ ! -L "$runtime" ] || fail 'native CLI was not installed'
[ "$(stat -c '%a' "$runtime")" = 755 ] || fail 'incorrect runtime mode'
[ "$(stat -c '%u:%g' "$runtime")" = "$(id -u):$(id -g)" ] || fail 'incorrect runtime owner'
[ -L "$link" ] && [ "$(readlink "$link")" = "$runtime" ] || fail 'canonical command is not managed'
[ "$("$link" --version)" = '2.1.0 (Claude Code)' ] || fail 'wrong installed version'
"$check" >/dev/null
run_hook "$root" >/dev/null
[ "$(wc -l < "$tmp/fetch.log")" -eq 5 ] || fail 'repeat provision re-downloaded the binary'
printf '2.2.0\n' > "$tmp/releases/latest"
run_hook "$root" >/dev/null
[ "$("$link" --version)" = '2.2.0 (Claude Code)' ] || fail 'new release was not installed'
printf '2.1.0\n' > "$tmp/releases/latest"
run_hook "$root" >/dev/null
[ "$(wc -l < "$tmp/fetch.log")" -eq 10 ] || fail 'older metadata downloaded a binary'
[ "$("$link" --version)" = '2.2.0 (Claude Code)' ] || fail 'working newer runtime was downgraded'

# Metadata and artifact failures must preserve the working runtime.
runtime_before="$(sha256sum "$runtime")"
printf '2.3.0\n' > "$tmp/releases/latest"
cp "$tmp/releases/2.3.0/manifest.json" "$tmp/valid.json"
for mutation in \
  '.version = "2.3.1"' \
  'del(.platforms["linux-x64"])' \
  '.platforms["linux-x64"].checksum = "invalid"' \
  '.platforms["linux-x64"].binary = "unexpected"'; do
  jq "$mutation" "$tmp/valid.json" > "$tmp/releases/2.3.0/manifest.json"
  count="$(wc -l < "$tmp/fetch.log")"
  if run_hook "$root" >/dev/null 2>"$tmp/error"; then fail 'invalid manifest accepted'; fi
  [ "$(wc -l < "$tmp/fetch.log")" -eq "$((count + 2))" ] || fail 'invalid manifest fetched a binary'
  [ "$(sha256sum "$runtime")" = "$runtime_before" ] || fail 'invalid manifest replaced the runtime'
done
cp "$tmp/valid.json" "$tmp/releases/2.3.0/manifest.json"
printf 'corrupted\n' > "$tmp/releases/2.3.0/linux-x64/claude"
if run_hook "$root" >/dev/null 2>"$tmp/error"; then fail 'checksum mismatch accepted'; fi
[ "$(sha256sum "$runtime")" = "$runtime_before" ] || fail 'bad checksum replaced the runtime'
printf '2.4.0\n' > "$tmp/releases/latest"
if run_hook "$root" >/dev/null 2>"$tmp/error"; then fail 'incorrect executable version accepted'; fi
[ "$(sha256sum "$runtime")" = "$runtime_before" ] || fail 'bad executable replaced the runtime'
printf '../../bad\n' > "$tmp/releases/latest"
count="$(wc -l < "$tmp/fetch.log")"
if run_hook "$root" >/dev/null 2>"$tmp/error"; then fail 'invalid latest version accepted'; fi
[ "$(wc -l < "$tmp/fetch.log")" -eq "$((count + 1))" ] || fail 'invalid latest version reached manifest download'
printf '2.2.0\n' > "$tmp/releases/latest"

# Readiness includes the CLI, and provisioning repairs owned binary/policy drift.
chmod 0777 "$(dirname "$runtime")"
if "$check" >/dev/null 2>&1; then fail 'writable runtime directory passed readiness'; fi
if run_hook "$root" >/dev/null 2>&1; then fail 'writable runtime directory accepted by provision'; fi
chmod 0755 "$(dirname "$runtime")"
chmod 0644 "$runtime"
if "$check" >/dev/null 2>&1; then fail 'runtime mode drift passed readiness'; fi
run_hook "$root" >/dev/null
jq '. + {allowManagedPermissionRulesOnly:true}' "$policy" > "$tmp/drift.json"
cp "$tmp/drift.json" "$policy"
if "$check" >/dev/null 2>&1; then fail 'policy drift passed readiness'; fi
run_hook "$root" >/dev/null
chmod 0666 "$policy"
if "$check" >/dev/null 2>&1; then fail 'policy mode drift passed readiness'; fi
run_hook "$root" >/dev/null
[ "$before" = "$(sha256sum "$policy_dir/10-foreign.json")" ] || fail 'foreign policy was modified'
[ "$auth_before" = "$(sha256sum "$tmp/home/.claude/.credentials.json")" ] || fail 'authorization was modified'
printf '{}\n' > "$policy"
if run_hook "$root" >/dev/null 2>&1; then fail 'foreign policy was overwritten'; fi
rm "$policy"
ln -s "$policy_dir/10-foreign.json" "$policy"
if run_hook "$root" >/dev/null 2>&1; then fail 'policy symlink accepted'; fi

# The managed drop-in locks every native update path and old policy files converge.
rm -f "$policy"
run_hook "$root" >/dev/null
jq -e '.env == {DISABLE_UPDATES: "1"}' "$policy" >/dev/null || fail 'update lock missing from the managed policy'
jq 'del(.env)' "$policy" > "$tmp/legacy-policy.json"
cp "$tmp/legacy-policy.json" "$policy"
if "$check" >/dev/null 2>&1; then fail 'policy without the update lock passed readiness'; fi
run_hook "$root" >/dev/null
jq -e '.env == {DISABLE_UPDATES: "1"}' "$policy" >/dev/null || fail 'legacy policy did not converge'

# Only the native installer's exact link is released, and never the files it points to.
dev_home="$tmp/dev-home"
shadow="$dev_home/.local/bin/claude"
run_shadow_hook() {
  env PATH="$tmp/bin:$PATH" HOME="$tmp/home" TMPDIR=/tmp DEV_USER="$(id -un)" CLAUDE_DEV_HOME="$dev_home" \
    CLAUDE_POLICY_TEST_ALLOW_NON_ROOT=1 CLAUDE_ARCH=amd64 \
    CLAUDE_INSTALL_ROOT="$root/opt/subyard/claude" CLAUDE_BIN_LINK="$link" \
    CLAUDE_POLICY_DIR="$policy_dir" CLAUDE_POLICY_CHECK_PATH="$check" bash "$hook"
}
mkdir -p "$dev_home/.local/bin" "$dev_home/data/claude/versions"
printf 'old\n' > "$dev_home/data/claude/versions/2.0.1"
printf '{}\n' > "$dev_home/data/claude/.last-update-result.json"
printf 'keep\n' > "$dev_home/data/unknown"
versions_before="$(sha256sum "$dev_home/data/claude/versions/2.0.1" "$dev_home/data/unknown")"
count="$(wc -l < "$tmp/fetch.log")"
run_shadow_hook >/dev/null
"$check" | grep -Fq 'Conflict:' && fail 'absent shadow reported a conflict'
ln -s "$dev_home/data/claude/versions/2.0.1" "$shadow"
if "$check" >"$tmp/shadow.out" 2>&1; then fail 'native shadow passed readiness'; fi
grep -Fq 'run yard init' "$tmp/shadow.out" || fail 'native shadow readiness lacks its repair'
run_shadow_hook >/dev/null
[ ! -e "$shadow" ] && [ ! -L "$shadow" ] || fail 'current provisioning kept the native shadow'
[ "$versions_before" = "$(sha256sum "$dev_home/data/claude/versions/2.0.1" "$dev_home/data/unknown")" ] \
  || fail 'shadow repair changed installer files'
[ -f "$dev_home/data/claude/.last-update-result.json" ] || fail 'shadow repair removed installer state'
[ "$(( $(wc -l < "$tmp/fetch.log") - count ))" -eq 4 ] || fail 'current provisioning downloaded a binary'
"$check" >/dev/null || fail 'released shadow did not restore readiness'
ln -s "$dev_home/data/claude/versions/9.9.9" "$shadow"
run_shadow_hook >/dev/null
[ ! -L "$shadow" ] || fail 'dangling native shadow was kept'
assert_foreign_shadow() {
  run_shadow_hook >/dev/null
  { [ -e "$shadow" ] || [ -L "$shadow" ]; } || fail "foreign shadow removed: $1"
  "$check" >"$tmp/foreign.out" 2>&1 || fail "foreign shadow failed readiness: $1"
  grep -Fq 'Conflict:' "$tmp/foreign.out" || fail "foreign shadow not reported: $1"
  rm -f -- "$shadow"
}
# The approval policy never depends on the developer tree.
if [ "$(id -u)" != 0 ]; then
  ln -s "$dev_home/data/claude/versions/2.0.1" "$shadow"
  chmod 0500 "$(dirname "$shadow")"
  rm -f "$policy"
  run_shadow_hook >"$tmp/unremovable.out" || fail 'unremovable native link blocked provisioning'
  [ -f "$policy" ] || fail 'unremovable native link prevented the policy'
  grep -Fq 'Conflict:' "$tmp/unremovable.out" || fail 'unremovable native link was not reported'
  chmod 0700 "$(dirname "$shadow")"
  rm -f -- "$shadow"
fi
printf '#!/bin/sh\n' > "$shadow"; assert_foreign_shadow 'regular file'
ln -s /usr/bin/true "$shadow"; assert_foreign_shadow 'other program'
ln -s "$dev_home/data/claude/versions/latest" "$shadow"; assert_foreign_shadow 'unversioned target'
ln -s "data/claude/versions/2.0.1" "$shadow"; assert_foreign_shadow 'relative target'
ln -s "$dev_home/data/claude/versions/../versions/2.0.1" "$shadow"; assert_foreign_shadow 'traversal target'
mkdir "$dev_home/data/claude/versions/3.0.0"
ln -s "$dev_home/data/claude/versions/3.0.0" "$shadow"; assert_foreign_shadow 'directory target'
# Independent refusals are all reported, in order. Findings about a missing file are skipped.
mv "$policy" "$policy.saved"
rm -f "$link"
ln -s "$dev_home/data/claude/versions/2.0.1" "$shadow"
if "$check" >"$tmp/refusals.out" 2>"$tmp/refusals.err"; then fail 'readiness passed with refusals'; fi
grep -Fq ' ready' "$tmp/refusals.out" && fail 'refused readiness still reported ready'
sed -e 's/^claude-policy-check: //' -e 's/;.*//' "$tmp/refusals.err" > "$tmp/refusals.actual"
cat > "$tmp/refusals.expected" <<REFUSALS
managed policy is missing
canonical command does not point to the managed runtime
a native self-installed link shadows $link
REFUSALS
diff "$tmp/refusals.expected" "$tmp/refusals.actual" >&2 || fail 'refusals are incomplete or out of order'
rm -f -- "$shadow"
ln -s "$root/opt/subyard/claude/claude" "$link"
# A missing policy alone still fails, and a conflict stays visible beside a refusal.
if "$check" >/dev/null 2>"$tmp/refusals.err"; then fail 'a missing policy passed readiness'; fi
[ "$(cat "$tmp/refusals.err")" = 'claude-policy-check: managed policy is missing; run yard init' ] \
  || fail 'a missing policy reported something else'
ln -s /usr/bin/true "$shadow"
if "$check" >"$tmp/refusals.out" 2>/dev/null; then fail 'a missing policy passed beside a conflict'; fi
grep -Fq 'Conflict:' "$tmp/refusals.out" || fail 'a refusal hid a conflict'
rm -f -- "$shadow"
mv "$policy.saved" "$policy"
# Permission and content drift of one present file are separate findings.
chmod 0600 "$policy"
cp "$policy" "$policy.saved"
printf '{}\n' > "$policy"
if "$check" >/dev/null 2>"$tmp/refusals.err"; then fail 'policy drift passed readiness'; fi
grep -Fq 'permissions drifted' "$tmp/refusals.err" || fail 'policy permission drift was not reported'
grep -Fq 'managed policy drifted' "$tmp/refusals.err" || fail 'policy content drift was not reported'
mv "$policy.saved" "$policy"
chmod 0644 "$policy"
"$check" >/dev/null || fail 'restored policy did not restore readiness'
# Hermetic callers without an explicit fixture home must never reach the caller's own home.
mkdir -p "$tmp/home/.local/bin"
ln -s "$dev_home/data/claude/versions/2.0.1" "$tmp/home/.local/bin/claude"
run_hook "$root" >/dev/null
[ -L "$tmp/home/.local/bin/claude" ] || fail 'hook released a link without an explicit fixture home'

# Both supported architecture names resolve their own artifact; execution remains host-native.
run_hook "$tmp/arm" arm64 >/dev/null
grep -Fq '/2.2.0/linux-arm64/claude' "$tmp/fetch.log" || fail 'arm64 artifact was not selected'
if run_hook "$tmp/unsupported" ppc64el >/dev/null 2>&1; then fail 'unsupported architecture accepted'; fi
foreign="$tmp/foreign"
mkdir -p "$foreign/usr/local/bin"
printf 'foreign install\n' > "$foreign/usr/local/bin/claude"
if run_hook "$foreign" >/dev/null 2>&1; then fail 'foreign command was overwritten'; fi
[ "$(cat "$foreign/usr/local/bin/claude")" = 'foreign install' ] || fail 'foreign command changed'
printf 'ok: Claude latest CLI install, repeat, upgrade, checksum, readiness, ownership, managed policy, update lock and native shadow release\n'
