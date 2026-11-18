#!/usr/bin/env bash
# Install the latest native Claude CLI and mandatory rules without touching authorization.
set -euo pipefail

INSTALL_ROOT="${CLAUDE_INSTALL_ROOT:-/opt/subyard/claude}"
RUNTIME_PATH="$INSTALL_ROOT/claude"
BIN_LINK="${CLAUDE_BIN_LINK:-/usr/local/bin/claude}"
RELEASE_BASE_URL=https://downloads.claude.ai/claude-code-releases
ARCH="${CLAUDE_ARCH:-}"
POLICY_DIR="${CLAUDE_POLICY_DIR:-/etc/claude-code/managed-settings.d}"
CHECK_PATH="${CLAUDE_POLICY_CHECK_PATH:-/usr/local/bin/claude-policy-check}"
MARKER='Subyard Claude permission policy'
DEV_USER="${DEV_USER:-dev}"
DEV_HOME="${CLAUDE_DEV_HOME:-}"
# Hermetic non-root callers name their fixture home; only root provisioning resolves the account.
if [ -z "$DEV_HOME" ] && [ "$(id -u)" = 0 ]; then
  DEV_HOME="$(getent passwd "$DEV_USER" | cut -d: -f6 || true)"
fi
expected_owner="$(id -u):$(id -g)"
die() { printf 'Claude policy: %s\n' "$*" >&2; exit 1; }
GIT_APPROVAL="${CODING_TOOL_GIT_APPROVAL:-off}"

# BEGIN generated Git approval hook
git_approval_hook() {
  cat <<'GIT_APPROVAL_HOOK'
#!/usr/bin/python3 -I
# Managed by Subyard Git approval provision.
"""Recognize literal Git command words without executing or expanding shell input."""

import json
import posixpath
import re
import shlex
import sys

# Keep shell separators distinct from quoted words. Comments are recognized only
# at token boundaries. A full shell evaluator is outside this hook's scope.
TOKEN = re.compile(r"""[^\S\n]+|\#[^\n]*|[;&|\n]+|[()]|<{1,3}|>+|(?:'[^']*'|"(?:\\.|[^"\\])*"|\\.|[^\s'";&|()<>\\])+""", re.S)
ASSIGNMENT = re.compile(r"^[A-Za-z_][A-Za-z_0-9]*\+?=")
GIT_VALUE_OPTIONS = {"-C", "-c", "--git-dir", "--work-tree",
                     "--namespace", "--config-env", "--super-prefix"}
GIT_FLAGS = {"-p", "--paginate", "-P", "--no-pager", "--no-replace-objects",
             "--no-optional-locks", "--no-lazy-fetch", "--literal-pathspecs",
             "--glob-pathspecs", "--noglob-pathspecs", "--icase-pathspecs",
             "--bare", "--no-advice"}


def git_action(words):
    args = iter(words)
    for arg in args:
        if arg in GIT_VALUE_OPTIONS:
            next(args, None)
        elif arg in GIT_FLAGS or (
            "=" in arg and arg.partition("=")[0] in GIT_VALUE_OPTIONS | {"--exec-path"}
        ):
            continue
        else:
            return arg if arg in {"commit", "push"} else None
    return None


def simple_commands(command):
    words = []
    offset = 0
    array_depth = 0
    while offset < len(command):
        token = TOKEN.match(command, offset)
        if token is None:
            return
        offset = token.end()
        raw = token[0]
        if array_depth:
            array_depth += (raw == "(") - (raw == ")")
            continue
        if (raw == "(" and words and words[-1][2] == token.start()
                and ASSIGNMENT.fullmatch(words[-1][0])):
            array_depth = 1
            continue
        if raw == "<<":
            # A heredoc body contains data, not a list of shell commands.
            if words:
                yield words
            return
        if raw.startswith("#"):
            continue
        if raw.startswith("\n") or raw[0] in ";&|()":
            if words:
                yield words
                words = []
        elif not raw.isspace():
            words.append((raw, token.start(), token.end()))
    if words:
        yield words


def shell_command(executable, args):
    options = iter(args)
    command_mode = noexec = False
    # sh can be Bash under its POSIX invocation name. POSIX sh and dash
    # use -o; Bash also takes operands for -O, --init-file and --rcfile.
    bash_options = executable in {"bash", "sh"}
    for arg in options:
        if arg == "--":
            arg = next(options, None)
            break
        if bash_options and arg in {"--init-file", "--rcfile"}:
            next(options, None)
            continue
        if re.fullmatch(r"[-+][a-zA-Z]+", arg):
            enabled = arg[0] == "-"
            for flag in arg[1:]:
                if flag == "c" and enabled:
                    command_mode = True
                elif flag == "n":
                    noexec = enabled
                elif flag == "o" or (flag == "O" and bash_options):
                    option = next(options, None)
                    if flag == "o" and option == "noexec":
                        noexec = enabled
        elif not arg.startswith("--"):
            break
    else:
        return None
    return arg if command_mode and not noexec else None


def denied_action(command, nested=False, depth=0):
    if depth == 8:
        return None
    command = command.replace("\\\n", "")
    for tokens in simple_commands(command):
        try:
            words = [shlex.split(raw, comments=False)[0] for raw, _, _ in tokens]
        except (IndexError, ValueError):
            continue
        start = 0
        while start < len(words) and ASSIGNMENT.match(tokens[start][0]):
            start += 1
        if start == len(words):
            continue
        executable = posixpath.basename(words[start])
        args = words[start + 1:]
        if executable == "git":
            action = git_action(args)
            if action and (nested or start or
                           command[tokens[0][1]:tokens[1][2]] != f"git {action}"):
                return action
        elif executable in {"sh", "bash", "dash", "zsh", "ksh"}:
            script = shell_command(executable, args)
            if script is not None:
                action = denied_action(script, nested=True, depth=depth + 1)
                if action:
                    return action
    return None


def main():
    payload = json.load(sys.stdin)
    action = denied_action(payload["tool_input"]["command"])
    if action:
        print(f"Git approval requires a direct `git {action}` command. "
              f"For another directory, use `cd <directory> && git {action}`.",
              file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main())
GIT_APPROVAL_HOOK
}
# Publish the helper before the policy that references it.
install_git_approval_hook() {
  local hook_stage
  [ "$GIT_APPROVAL" = prompt ] || return 0
  hook_stage="$(mktemp "$(dirname "$git_hook")/.git-approval.XXXXXX")"
  git_approval_hook > "$hook_stage"
  chmod 0755 "$hook_stage"
  mv -fT -- "$hook_stage" "$git_hook"
}

check_git_approval_hook() {
  local git_hook="$1" git_hook_sha="$2" expected_owner="$3"
  [ -x /usr/bin/python3 ] || refuse 'managed Git approval hook interpreter is missing; run yard init'
  if [ -f "$git_hook" ] && [ ! -L "$git_hook" ]; then
    [ "$(stat -c '%a:%u:%g' "$git_hook" 2>/dev/null)" = "755:$expected_owner" ] \
      || refuse 'managed Git approval hook permissions drifted; run yard init'
    [ "$(sha256sum < "$git_hook" | cut -d' ' -f1)" = "$git_hook_sha" ] \
      || refuse 'managed Git approval hook content drifted; run yard init'
  else
    refuse 'managed Git approval hook is missing; run yard init'
  fi
}
# END generated Git approval hook

case "$GIT_APPROVAL" in
  prompt) POLICY_JSON='{"$comment":"Subyard Claude permission policy","env":{"DISABLE_UPDATES":"1"},"permissions":{"ask":["Bash(git commit)","Bash(git commit:*)","Bash(git push)","Bash(git push:*)"]}}' ;;
  off) POLICY_JSON='{"$comment":"Subyard Claude permission policy","env":{"DISABLE_UPDATES":"1"}}' ;;
  *) die 'CODING_TOOL_GIT_APPROVAL must be prompt or off' ;;
esac
[ "$(id -u)" = 0 ] || [ "${CLAUDE_POLICY_TEST_ALLOW_NON_ROOT:-0}" = 1 ] || die 'must run as root'
ETC_DIR="${CLAUDE_ETC_DIR:-/etc}"
case "$ETC_DIR" in /*) ;; *) die 'environment directory must be absolute' ;; esac
case "${1:-}" in
  --remove)
    rm -f -- "$ETC_DIR/profile.d/subyard-claude.sh"
    if [ -f "$ETC_DIR/environment" ]; then
      sed -i '/^# >>> subyard-claude >>>$/,/^# <<< subyard-claude <<<$/d' "$ETC_DIR/environment"
    fi
    exit 0
    ;;
  '') ;;
  *) die "unknown argument: $1" ;;
esac
for path in "$INSTALL_ROOT" "$BIN_LINK"; do
  case "$path" in /*) ;; *) die 'install paths must be absolute' ;; esac
done
case "$POLICY_DIR:$CHECK_PATH" in /*:/*) ;; *) die 'policy paths must be absolute' ;; esac
for path in "$INSTALL_ROOT" "$(dirname "$BIN_LINK")" "$(dirname "$CHECK_PATH")" "$(dirname "$POLICY_DIR")" "$POLICY_DIR"; do
  if [ -L "$path" ] || { [ -e "$path" ] && [ ! -d "$path" ]; }; then
    die 'managed directory is not a regular directory'
  fi
  if [ -d "$path" ]; then
    [ "$(stat -c '%u:%g' "$path")" = "$expected_owner" ] || die 'managed directory has a foreign owner'
    permissions="$(stat -c '%a' "$path")"
    (( (8#$permissions & 0022) == 0 )) || die 'managed directory is writable by another user'
  fi
done
policy="$POLICY_DIR/50-subyard-git-approval.json"
git_hook="$(dirname "$POLICY_DIR")/subyard-git-approval-hook"
if [ "$GIT_APPROVAL" = prompt ]; then
  [ -x /usr/bin/python3 ] || die '/usr/bin/python3 is required for Git approval'
  if [ -L "$git_hook" ] || { [ -e "$git_hook" ] && [ ! -f "$git_hook" ]; }; then
    die 'managed Git approval hook is not a regular file'
  fi
  if [ -f "$git_hook" ] && ! grep -Fxq '# Managed by Subyard Git approval provision.' "$git_hook"; then
    die 'existing Git approval hook belongs to another policy'
  fi
fi
for path in "$policy" "$CHECK_PATH"; do
  if [ -L "$path" ] || { [ -e "$path" ] && [ ! -f "$path" ]; }; then
    die 'managed path is not a regular file'
  fi
  if [ -f "$path" ]; then
    grep -Fq "$MARKER" "$path" || die 'existing file belongs to another policy'
  fi
done
if [ -L "$BIN_LINK" ]; then
  [ "$(readlink "$BIN_LINK")" = "$RUNTIME_PATH" ] || die 'canonical command belongs to another installation'
elif [ -e "$BIN_LINK" ]; then
  die 'canonical command belongs to another installation'
fi
if [ -L "$RUNTIME_PATH" ] || { [ -e "$RUNTIME_PATH" ] && [ ! -f "$RUNTIME_PATH" ]; }; then
  die 'managed runtime is not a regular file'
fi
for tool in curl jq sha256sum sort od timeout; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool is required"
done
if [ -z "$ARCH" ]; then ARCH="$(dpkg --print-architecture)"; fi
case "$ARCH" in
  amd64) platform=linux-x64 ;;
  arm64) platform=linux-arm64 ;;
  *) die "unsupported architecture '$ARCH' (supported: amd64, arm64)" ;;
esac

is_working_binary() {
  local path="$1" output magic directory
  directory="$(dirname "$path")"
  [ -d "$directory" ] && [ ! -L "$directory" ] || return 1
  [ "$(stat -c '%a:%u:%g' "$directory")" = "755:$expected_owner" ] || return 1
  [ -f "$path" ] && [ ! -L "$path" ] && [ -x "$path" ] || return 1
  [ "$(stat -c '%a:%u:%g' "$path")" = "755:$expected_owner" ] || return 1
  magic="$(LC_ALL=C od -An -tx1 -N4 "$path" | tr -d '[:space:]')"
  [ "$magic" = 7f454c46 ] || return 1
  output="$(timeout 30 "$path" --version 2>/dev/null)" || return 1
  [[ "$output" =~ ^([0-9]+\.[0-9]+\.[0-9]+)\ \(Claude\ Code\)$ ]] || return 1
  BINARY_VERSION="${BASH_REMATCH[1]}"
}

# Developer-tree repairs apply only where the developer can make them; anything else is a conflict.
dev_owned_directory() {
  [ -d "$1" ] && [ ! -L "$1" ] && [ "$(stat -c '%U' "$1")" = "$DEV_USER" ] \
    && (( (8#$(stat -c '%a' "$1") & 0200) != 0 ))
}

# Claude's native installer publishes <home>/.local/bin/claude -> <data home>/claude/versions/X.Y.Z
# on its first update. Only that exact shape can be released: it shadows the managed runtime in
# login shells, while its version files stay in place for any session still running them.
native_shadow_state() {
  local link="$DEV_HOME/.local/bin/claude" target
  [ -n "$DEV_HOME" ] || { printf 'absent'; return; }
  if [ ! -L "$link" ]; then
    if [ -e "$link" ]; then printf 'foreign'; else printf 'absent'; fi
    return
  fi
  target="$(readlink "$link")"
  if [[ "$target" =~ ^/([^/]+/)*claude/versions/[0-9]+\.[0-9]+\.[0-9]+$ ]] \
    && [[ "$target" != */../* ]] && [ "$(stat -c '%U' "$link")" = "$DEV_USER" ] \
    && dev_owned_directory "$(dirname "$link")" \
    && { [ ! -e "$target" ] || { [ -f "$target" ] && [ ! -L "$target" ] && [ "$(stat -c '%U' "$target")" = "$DEV_USER" ]; }; }; then
    printf 'native'
  else
    printf 'foreign'
  fi
}
release_native_shadow() {
  [ "$(native_shadow_state)" = native ] || return 0
  local -a remove=(rm -f -- "$DEV_HOME/.local/bin/claude")
  [ "$(id -u)" != 0 ] || remove=(runuser -u "$DEV_USER" -- "${remove[@]}")
  if "${remove[@]}"; then
    printf 'Released the native self-installed claude link from %s/.local/bin\n' "$DEV_HOME"
  else
    printf 'Conflict: could not remove %s/.local/bin/claude.\n' "$DEV_HOME"
  fi
}

tmp="$(mktemp -d)"
stage=""
runtime_stage=""
cleanup() {
  rm -rf -- "$tmp"
  [ -z "$stage" ] || rm -f -- "$stage"
  [ -z "$runtime_stage" ] || rm -f -- "$runtime_stage"
}
trap cleanup EXIT
download() {
  curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
    --retry 3 --connect-timeout 20 --max-time 300 --output "$2" "$1"
}
# Keep this function local: provision hooks are delivered on stdin, without repo helpers.
download_artifact() {
  local attempt
  for attempt in 1 2 3 4; do
    # Allow slow links indefinitely; below 1 KiB/s for two minutes indicates a stall.
    if curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
      --connect-timeout 20 --speed-limit 1024 --speed-time 120 --continue-at - \
      --output "$2" "$1"; then return 0; fi
    [ "$attempt" = 4 ] || sleep 2
  done
  return 1
}
# The approval policy precedes every download, the binary it governs and every developer-tree
# repair, so a failed or slow download never leaves the CLI without it.
install -d -m 0755 "$(dirname "$POLICY_DIR")" "$POLICY_DIR" "$(dirname "$CHECK_PATH")"
stage="$(mktemp "$POLICY_DIR/.subyard.XXXXXX")"
install_git_approval_hook
if [ "$GIT_APPROVAL" = prompt ]; then
  POLICY_JSON="$(jq -c --arg path "$git_hook" '
    .hooks.PreToolUse = [{matcher:"Bash", hooks:[{
      type:"command", command:($path | @sh), timeout:3, onFailure:"block"
    }]}]' <<<"$POLICY_JSON")"
fi
jq . <<<"$POLICY_JSON" > "$stage"
chmod 0644 "$stage"
mv -fT -- "$stage" "$policy"
stage=""
# /etc/profile.d covers login shells; PAM reads /etc/environment for yard shell -- cmd.
# These Claude safety switches are ignored in settings env: they must precede CLI startup.
install -d -m 0755 "$ETC_DIR/profile.d"
cat > "$ETC_DIR/profile.d/subyard-claude.sh" <<'ENV'
# Subyard Claude process environment — generated by provision.sh.
export CLAUDE_CODE_DISABLE_INLINE_SHELL_RM_PROMPT=1
export CLAUDE_CODE_DISABLE_SUBSTITUTION_RM_PROMPT=1
ENV
chmod 0644 "$ETC_DIR/profile.d/subyard-claude.sh"
if [ -f "$ETC_DIR/environment" ]; then
  sed -i '/^# >>> subyard-claude >>>$/,/^# <<< subyard-claude <<<$/d' "$ETC_DIR/environment"
else
  install -m 0644 /dev/null "$ETC_DIR/environment"
fi
cat >> "$ETC_DIR/environment" <<'ENV'
# >>> subyard-claude >>>
CLAUDE_CODE_DISABLE_INLINE_SHELL_RM_PROMPT=1
CLAUDE_CODE_DISABLE_SUBSTITUTION_RM_PROMPT=1
# <<< subyard-claude <<<
ENV
# Unlike su/sshd, Debian's runuser PAM stack does not load /etc/environment by default.
# yard shell -- cmd uses runuser without a login shell, so enable its system environment read.
pam_runuser="$ETC_DIR/pam.d/runuser"
[ -f "$pam_runuser" ] || die 'runuser PAM configuration is missing'
pam_environment='session required pam_env.so readenv=1'
if ! grep -Fxq "$pam_environment" "$pam_runuser"; then
  printf '\n%s\n' "$pam_environment" >> "$pam_runuser"
fi
download "$RELEASE_BASE_URL/latest" "$tmp/latest" || die 'latest version download failed'
VERSION="$(cat "$tmp/latest")"
[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die 'invalid latest release version'
download "$RELEASE_BASE_URL/$VERSION/manifest.json" "$tmp/manifest.json" || die 'release manifest download failed'
EXPECTED_SHA256="$(jq -ers --arg version "$VERSION" --arg platform "$platform" '
  if length == 1 then .[0] else error("expected one manifest") end |
  select(.version == $version) | .platforms[$platform] |
  select(.binary == "claude") | .checksum | select(test("^[0-9a-f]{64}$"))
' "$tmp/manifest.json" 2>/dev/null)" || die "manifest lacks a verified artifact for $ARCH"

install -d -m 0755 "$INSTALL_ROOT" "$(dirname "$BIN_LINK")"
current=0
if is_working_binary "$RUNTIME_PATH"; then
  # A stale latest pointer must not downgrade a newer working installation.
  newest="$(printf '%s\n' "$BINARY_VERSION" "$VERSION" | sort -V | tail -n 1)"
  [ "$newest" != "$BINARY_VERSION" ] || current=1
fi
if [ "$current" = 0 ]; then
  download_artifact "$RELEASE_BASE_URL/$VERSION/$platform/claude" "$tmp/claude" || die 'release artifact download failed'
  # Fail closed on a corrupt partial; cleanup discards it before the next invocation.
  [ "$(sha256sum "$tmp/claude" | cut -d' ' -f1)" = "$EXPECTED_SHA256" ] || die "checksum mismatch for Claude $VERSION ($ARCH)"
  runtime_stage="$(mktemp "$INSTALL_ROOT/.claude.XXXXXX")"
  install -m 0755 "$tmp/claude" "$runtime_stage"
  is_working_binary "$runtime_stage" && [ "$BINARY_VERSION" = "$VERSION" ] || die 'staged CLI failed version validation'
fi
if [ -n "$runtime_stage" ]; then
  mv -fT -- "$runtime_stage" "$RUNTIME_PATH"
  runtime_stage=""
fi
if [ ! -L "$BIN_LINK" ]; then ln -s "$RUNTIME_PATH" "$BIN_LINK"; fi
stage="$(mktemp "$(dirname "$CHECK_PATH")/.claude-policy-check.XXXXXX")"
{
  printf '#!/usr/bin/env bash\n# %s\nset -euo pipefail\n' "$MARKER"
  printf 'policy=%q\npolicy_dir=%q\nexpected_owner=%q\n' "$policy" "$POLICY_DIR" "$(id -u):$(id -g)"
  printf 'runtime=%q\nlink=%q\n' "$RUNTIME_PATH" "$BIN_LINK"
  printf 'DEV_USER=%q\nDEV_HOME=%q\n' "$DEV_USER" "$DEV_HOME"
  printf 'git_approval=%q\npolicy_json=%q\n' "$GIT_APPROVAL" "$POLICY_JSON"
  printf 'git_hook=%q\ngit_hook_sha=%q\n' "$git_hook" "$(git_approval_hook | sha256sum | cut -d' ' -f1)"
  declare -f check_git_approval_hook is_working_binary dev_owned_directory native_shadow_state
  cat <<'CHECK'
# Report every refusal that does not depend on a failed read, in this order.
refusals=0
refuse() { printf 'claude-policy-check: %s\n' "$*" >&2; refusals=$((refusals + 1)); }
[ ! -L "$policy_dir" ] && [ "$(stat -c '%a:%u:%g' "$policy_dir" 2>/dev/null)" = "755:$expected_owner" ] \
  || refuse 'managed policy directory drifted; run yard init'
if [ -f "$policy" ] && [ ! -L "$policy" ]; then
  [ "$(stat -c '%a:%u:%g' "$policy")" = "644:$expected_owner" ] \
    || refuse 'managed policy permissions drifted; run yard init'
  jq -e --argjson expected "$policy_json" '. == $expected' "$policy" >/dev/null 2>&1 \
    || refuse 'managed policy drifted or could not be read; run yard init'
else
  refuse 'managed policy is missing; run yard init'
fi
if [ "$git_approval" = prompt ]; then
  hook_dir="$(dirname "$git_hook")"
  [ ! -L "$hook_dir" ] && [ "$(stat -c '%a:%u:%g' "$hook_dir" 2>/dev/null)" = "755:$expected_owner" ] \
    || refuse 'managed Git approval hook directory drifted; run yard init'
  check_git_approval_hook "$git_hook" "$git_hook_sha" "$expected_owner"
fi
[ -L "$link" ] && [ "$(readlink "$link")" = "$runtime" ] \
  || refuse 'canonical command does not point to the managed runtime; run yard init'
is_working_binary "$runtime" || refuse 'managed runtime failed validation; run yard init'
shadow="$(native_shadow_state)" || shadow=foreign
[ "$shadow" != native ] || refuse "a native self-installed link shadows $link; run yard init"
if [ "$refusals" = 0 ]; then
  printf 'Claude %s ready. ' "$BINARY_VERSION"
  if [ "$git_approval" = off ]; then
    printf 'Managed Claude update lock installed; managed Git prompts off (CODING_TOOL_GIT_APPROVAL=off).\n'
  else
    printf 'Managed Claude Git ask rules, Git hook and update lock installed; inspect /status and /permissions in the active client.\n'
  fi
fi
if [ "$shadow" = foreign ]; then
  printf 'Conflict: %s/.local/bin/claude is not Subyard-owned and was left unchanged; login shells can run it instead of %s.\n' "$DEV_HOME" "$link"
fi
[ "$refusals" = 0 ]
CHECK
} > "$stage"
chmod 0755 "$stage"
mv -fT -- "$stage" "$CHECK_PATH"
stage=""
release_native_shadow
# The integration adapter reruns the check and reports readiness once; keep only its conflicts here.
check_status=0
check_output="$("$CHECK_PATH")" || check_status=$?
printf '%s\n' "$check_output" | grep '^Conflict:' || true
[ "$check_status" = 0 ]

