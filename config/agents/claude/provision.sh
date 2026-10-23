#!/usr/bin/env bash
# Add native mandatory rules without replacing user/project permission restrictions.
set -euo pipefail

POLICY_DIR="${CLAUDE_POLICY_DIR:-/etc/claude-code/managed-settings.d}"
CHECK_PATH="${CLAUDE_POLICY_CHECK_PATH:-/usr/local/bin/claude-policy-check}"
MARKER='Subyard Claude permission policy'
expected_owner="$(id -u):$(id -g)"
die() { printf 'Claude policy: %s\n' "$*" >&2; exit 1; }
[ "$(id -u)" = 0 ] || [ "${CLAUDE_POLICY_TEST_ALLOW_NON_ROOT:-0}" = 1 ] || die 'must run as root'
case "$POLICY_DIR:$CHECK_PATH" in /*:/*) ;; *) die 'policy paths must be absolute' ;; esac
for path in "$(dirname "$POLICY_DIR")" "$POLICY_DIR"; do
  if [ -L "$path" ] || { [ -e "$path" ] && [ ! -d "$path" ]; }; then
    die 'policy directory is not a regular directory'
  fi
  if [ -d "$path" ]; then
    [ "$(stat -c '%u:%g' "$path")" = "$expected_owner" ] || die 'policy directory has a foreign owner'
    permissions="$(stat -c '%a' "$path")"
    (( (8#$permissions & 0022) == 0 )) || die 'policy directory is writable by another user'
  fi
done
policy="$POLICY_DIR/50-subyard-git-approval.json"
for path in "$policy" "$CHECK_PATH"; do
  if [ -L "$path" ] || { [ -e "$path" ] && [ ! -f "$path" ]; }; then
    die 'managed path is not a regular file'
  fi
  if [ -f "$path" ]; then
    grep -Fq "$MARKER" "$path" || die 'existing file belongs to another policy'
  fi
done
install -d -m 0755 "$(dirname "$POLICY_DIR")" "$POLICY_DIR" "$(dirname "$CHECK_PATH")"
stage="$(mktemp "$POLICY_DIR/.subyard.XXXXXX")"
trap 'rm -f -- "$stage"' EXIT
cat > "$stage" <<'JSON'
{
  "$comment": "Subyard Claude permission policy",
  "permissions": {
    "ask": ["Bash(git commit)", "Bash(git commit:*)", "Bash(git push)", "Bash(git push:*)"]
  }
}
JSON
chmod 0644 "$stage"
mv -fT -- "$stage" "$policy"
stage="$(mktemp "$(dirname "$CHECK_PATH")/.claude-policy-check.XXXXXX")"
{
  printf '#!/usr/bin/env bash\n# %s\nset -euo pipefail\n' "$MARKER"
  printf 'policy=%q\npolicy_dir=%q\nexpected_owner=%q\n' "$policy" "$POLICY_DIR" "$(id -u):$(id -g)"
  cat <<'CHECK'
for path in "$policy_dir" "$policy"; do
  [ ! -L "$path" ] && [ "$(stat -c '%u:%g' "$path")" = "$expected_owner" ] || exit 1
done
[ "$(stat -c '%a' "$policy_dir")" = 755 ] && [ "$(stat -c '%a' "$policy")" = 644 ] || exit 1
jq -e '. == {"$comment":"Subyard Claude permission policy","permissions":{"ask":["Bash(git commit)","Bash(git commit:*)","Bash(git push)","Bash(git push:*)"]}}' "$policy" >/dev/null
printf 'Managed Claude Git ask rules installed; inspect /status and /permissions in the active client.\n'
CHECK
} > "$stage"
chmod 0755 "$stage"
mv -fT -- "$stage" "$CHECK_PATH"
"$CHECK_PATH"

