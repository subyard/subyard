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

# Both supported architecture names resolve their own artifact; execution remains host-native.
run_hook "$tmp/arm" arm64 >/dev/null
grep -Fq '/2.2.0/linux-arm64/claude' "$tmp/fetch.log" || fail 'arm64 artifact was not selected'
if run_hook "$tmp/unsupported" ppc64el >/dev/null 2>&1; then fail 'unsupported architecture accepted'; fi
foreign="$tmp/foreign"
mkdir -p "$foreign/usr/local/bin"
printf 'foreign install\n' > "$foreign/usr/local/bin/claude"
if run_hook "$foreign" >/dev/null 2>&1; then fail 'foreign command was overwritten'; fi
[ "$(cat "$foreign/usr/local/bin/claude")" = 'foreign install' ] || fail 'foreign command changed'
printf 'ok: Claude latest CLI install, repeat, upgrade, checksum, readiness, ownership and managed policy\n'
