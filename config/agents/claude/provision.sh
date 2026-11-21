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
GIT_APPROVAL="${CODING_TOOL_GIT_APPROVAL:-prompt}"
DEV_SUDO="${DEV_SUDO:-prompt}"
# 0 and 1 are deprecated aliases of no and yes.
case "$DEV_SUDO" in 0) DEV_SUDO=no ;; 1) DEV_SUDO=yes ;; esac

# BEGIN generated Git approval hook
git_approval_hook() {
  cat <<'GIT_APPROVAL_HOOK'
#!/usr/bin/python3 -I
# Managed by Subyard Git approval provision.
"""Refuse a literal mention of a gated command unless the client's own prompt covers it.

Arguments name the gates: git (commit and push) and sudo (privilege escalation).
--plain selects the Codex rules, which require the whole command to be plain; without
it, the Claude rules check the plain prefix up to the first direct gated command.
"""

import collections
import json
import os
import posixpath
import re
import shlex
import sys

# A flat lexer: shell separators, comments and quoted words, without evaluating anything.
TOKEN = re.compile(r"""[^\S\n]+|\#[^\n]*|[;&|\n]+|[()`]|<{1,3}|>+|(?:'[^']*'|"(?:\\.|[^"\\])*"|\\.|[^\s'"`;&|()<>\\])+""", re.S)
ASSIGNMENT = re.compile(r"^[A-Za-z_][A-Za-z_0-9]*\+?=")
GIT_VALUE_OPTIONS = {"-C", "-c", "--git-dir", "--work-tree",
                     "--namespace", "--config-env", "--super-prefix"}
GIT_FLAGS = {"-p", "--paginate", "-P", "--no-pager", "--no-replace-objects",
             "--no-optional-locks", "--no-lazy-fetch", "--literal-pathspecs",
             "--glob-pathspecs", "--noglob-pathspecs", "--icase-pathspecs",
             "--bare", "--no-advice"}
ESCALATION = ("sudo", "su", "doas", "pkexec", "sudoedit", "run0")
GIT_ACTIONS = ("git commit", "git push")
RESERVED = {"!", "{", "}", "[[", "]]", "case", "coproc", "do", "done", "elif", "else", "esac",
            "fi", "for", "function", "if", "in", "select", "then", "time", "until", "while"}
# Commands that only print, search or list their arguments, so a mention there is text.
INERT = {"echo", "printf", "grep", "rg", "which", "type", "ls", "cat", "head", "tail", "wc",
         "cut", "uniq", "cd", "true"}
PLAIN_SEPARATORS = {";", "|", "&&", "||"}
# Codex runs a command with a glob, tilde, brace or escape without asking, so its plain words
# use only these characters outside quotes, and quotes without expansions or escapes.
CODEX_WORD = re.compile(r"""(?:[A-Za-z0-9_@%+=:,./-]|'[^'\n]*'|"[^"\\$`\n]*")+""")
RESCAN = re.compile(r"""[\s;&|()<>`$#'"\\]""")
# Larger commands are refused: scanning them could outlast the client's hook timeout.
MAX_COMMAND_BYTES = 256 * 1024
ANSI_C = re.compile(r"""(?<!\\)\$'((?:\\.|[^'\\])*)'""", re.S)
ANSI_C_ESCAPE = re.compile(r"\\(x[0-9A-Fa-f]{1,2}|u[0-9A-Fa-f]{1,4}|U[0-9A-Fa-f]{1,8}|[0-7]{1,3}|c.|.)", re.S)
ANSI_C_NAMED = {"a": "\a", "b": "\b", "e": "\x1b", "E": "\x1b", "f": "\f", "n": "\n",
                "r": "\r", "t": "\t", "v": "\v"}
# Brace expansion, option values and history expansion glue names to other text.
NAME_PARTS = re.compile(r"[{},=!]")


def git_action(words, start=0):
    """Return the subcommand git runs from words[start:]; ... marks one hidden by an expansion."""
    index = start
    while index < len(words):
        arg = words[index]
        index += 1
        if arg is None or "$" in arg or "`" in arg:
            return "..."
        if arg in GIT_VALUE_OPTIONS:
            index += 1
        elif not (arg in GIT_FLAGS or (
            "=" in arg and arg.partition("=")[0] in GIT_VALUE_OPTIONS | {"--exec-path"}
        )):
            return arg
    return None


def ansi_c(match):
    """Return a $'…' body with its escapes decoded, quoted again for shlex."""
    def decode(escape):
        code = escape[1]
        if code[0] in "xuU":
            value = chr(min(int(code[1:], 16), 0x10FFFF))
        elif code[0] in "01234567":
            value = chr(int(code, 8) & 0xFF)
        elif code[0] == "c" and len(code) == 2:
            value = chr(ord(code[1]) & 0x1F)
        else:
            value = ANSI_C_NAMED.get(code, code)
        return value
    # Bash ends the string at the first NUL, from \0, \x0, \u0000 or \c@ alike.
    return shlex.quote(ANSI_C_ESCAPE.sub(decode, match[1]).partition("\0")[0])


def unquote(raw):
    """Return the value of one shell word; $'…' is decoded and $"…" loses its prefix."""
    raw = ANSI_C.sub(ansi_c, raw)
    raw = re.sub(r"""(?<!\\)\$(?=["])""", "", raw)
    try:
        return "".join(shlex.split(raw, comments=False))
    except ValueError:
        return re.sub(r"""[\\'"]""", "", raw)


def names(value):
    """Return the command names a word spells, also inside braces, after = or !, or after -S."""
    parts = [value, *NAME_PARTS.split(value)]
    if value.startswith("-S"):
        # env -S takes its command in the same word.
        parts.append(value[2:])
    return {posixpath.basename(part) for part in parts}


def git_mention(run):
    """Return the git action that a git word in one command's words runs."""
    later, action = [None] * (len(run) + 1), None
    for index in range(len(run) - 1, -1, -1):
        action = f"git {run[index]}" if run[index] in ("commit", "push") else action
        later[index] = action
    for index, word in enumerate(run):
        if word is not None and "git" in names(word):
            action = git_action(run, index + 1)
            if action in ("commit", "push"):
                return f"git {action}"
            if action == "...":
                # An expansion hides the subcommand; a later literal commit or push counts.
                return later[index + 1]
    return None


def mention(text, gates):
    """Return the first gated command the text names anywhere, in any position."""
    pending = collections.deque([text])
    while pending:
        current = pending.popleft()
        run = []
        offset = 0
        while offset <= len(current):
            token = TOKEN.match(current, offset) if offset < len(current) else None
            raw = token[0] if token else ""
            if token is None or raw.isspace() and "\n" in raw or raw[0] in ";&|\n#":
                # The end of a command's words: name a git subcommand in them.
                found = "git" in gates and git_mention(run)
                if found:
                    return found
                run = []
                if token is None:
                    offset += 1
                    continue
                if raw[0] == "#":
                    pending.append(raw[1:])
            elif raw.isspace():
                pass
            elif raw[0] in "()`<>":
                run.append(None)
            else:
                value = unquote(raw)
                spelled = names(value)
                found = "sudo" in gates and next((n for n in ESCALATION if n in spelled), None)
                if found:
                    return found
                # Git runs git-commit and git-push from its exec path.
                found = "git" in gates and next((f"git {n[4:]}" for n in ("git-commit", "git-push")
                                                 if n in spelled), None)
                if found:
                    return found
                if len(value) < len(raw) and RESCAN.search(value):
                    pending.append(value)
                run.append(value)
            offset = token.end()
    return None


def native(text, gates, start=0):
    """Return the gated command text[start:] starts with literally, as the client's rules match it."""
    for name in (ESCALATION if "sudo" in gates else ()) + (GIT_ACTIONS if "git" in gates else ()):
        end = start + len(name)
        if text.startswith(name, start) and (end == len(text) or text[end] in " ;&|\n"):
            return name
    return None


def inert(segment):
    return segment.split(None, 1)[0] in INERT or segment == "git commit" or segment.startswith("git commit ")


def pipelines(command, gates, claude):
    """Split a plain command into pipelines of (text, direct) segments, or return None.

    Plain means simple commands of words joined by &&, ||, ; or |, with no expansion,
    redirect, grouping, newline or comment, and no reserved word or assignment first.
    Claude prompts once for a whole call, so its check ends at the first direct segment.
    """
    result, pipeline, offset = [], [], 0
    while True:
        while command.startswith((" ", "\t"), offset):
            offset += 1
        start = offset
        if claude and native(command, gates, start):
            return result + [pipeline + [(command[start:], True)]]
        words, separator = [], None
        while offset < len(command):
            token = TOKEN.match(command, offset)
            if token is None:
                return None
            raw = token[0]
            if raw in PLAIN_SEPARATORS:
                separator = raw
                break
            if raw.isspace():
                if raw.strip(" \t"):
                    return None
                offset = token.end()
                continue
            if raw[0] in "#;&|\n()`<>" or any(char in raw for char in "$`\n"):
                return None
            if not claude and not CODEX_WORD.fullmatch(raw):
                return None
            words.append(raw)
            offset = token.end()
        if not words or words[0] in RESERVED or ASSIGNMENT.match(words[0]):
            return None
        segment = command[start:offset].rstrip(" \t")
        pipeline.append((segment, bool(native(segment, gates))))
        if separator is None:
            return result + [pipeline]
        offset += len(separator)
        if separator != "|":
            result.append(pipeline)
            pipeline = []


def refused(command, gates, claude):
    """Return the mention that the client's own prompt does not cover, if any."""
    first = mention(command, gates)
    if not first:
        return None, True
    split = pipelines(command, gates, claude)
    if split is None:
        return first, False
    for pipeline in split:
        # A mention is text only when no command of its pipeline can run it.
        inert_pipeline = all(d or inert(s) for s, d in pipeline)
        for segment, direct in pipeline:
            found = not direct and mention(segment, gates)
            if found and not (inert_pipeline and inert(segment)):
                return found, True
    return None, True


def main():
    args = sys.argv[1:]
    if "--policies" in args:
        index = args.index("--policies")
        # Provision supplies the two paths under the client-owned managed directory.
        gates = {gate for gate, path in zip(("git", "sudo"), args[index + 1:], strict=True)
                 if os.path.isfile(path)}
        if not gates:
            return 0
        args = args[:index]
    else:
        # Existing sessions can still invoke the original registration.
        gates = {arg for arg in args if not arg.startswith("--")} or {"git"}
    codex = "--plain" in args
    command = json.load(sys.stdin)["tool_input"]["command"]
    if len(command.encode(errors="surrogatepass")) > MAX_COMMAND_BYTES:
        print(f"Approval refuses a command longer than {MAX_COMMAND_BYTES // 1024} KiB; "
              f"write long scripts to a file first.", file=sys.stderr)
        return 2
    action, plain = refused(command, gates, claude=not codex)
    if not action:
        return 0
    if codex and not plain:
        print(f"Approval requires `{action}` in a plain command: words joined only by "
              f"&&, ||, ; or |, without redirects, variables, substitutions or grouping.",
              file=sys.stderr)
    elif action.startswith("git "):
        print(f"Git approval requires a direct `{action}` command. "
              f"For another directory, use `cd <directory> && {action}`.",
              file=sys.stderr)
    else:
        print(f"Sudo approval requires a direct `{action}` command at the start of a command, "
              f"without a path, wrapper, keyword, substitution or nested shell.",
              file=sys.stderr)
    if not codex or plain:
        print("Another command may name it only in a plain command that starts with "
              + ", ".join(sorted(INERT)) + ".", file=sys.stderr)
    return 2


if __name__ == "__main__":
    sys.exit(main())
GIT_APPROVAL_HOOK
}
# Publish each file atomically and preserve an already converged inode.
publish_approval_file() {
  local path="$1" mode="$2" stage
  stage="$(mktemp "$(dirname "$path")/.approval.XXXXXX")"
  cat > "$stage"
  chmod "$mode" "$stage"
  if [ "$(id -u)" = 0 ]; then chown "$expected_owner" "$stage"; fi
  if [ -f "$path" ] && [ ! -L "$path" ] \
    && [ "$(stat -c '%a:%u:%g' "$path")" = "$mode:$expected_owner" ] && cmp -s "$stage" "$path"; then
    rm -f -- "$stage"
  else
    mv -fT -- "$stage" "$path"
  fi
}

reconcile_approval_file() {
  local path="$1" content="$2"
  if [ -n "$content" ]; then
    printf '%s\n' "$content" | publish_approval_file "$path" 644
  else
    rm -f -- "$path"
  fi
}

approval_command() {
  jq -n --args '$ARGS.positional | map(@sh) | join(" ")' -- \
    "$git_hook" "$@" --policies "$git_policy" "$sudo_policy"
}

# A content check can still run when permissions drifted.
approval_file_present() {
  local path="$1" expected="$2" mode="$3" label="$4"
  if [ -z "$expected" ]; then
    [ ! -e "$path" ] && [ ! -L "$path" ] || refuse "$label should be absent; run yard init"
  elif [ -f "$path" ] && [ ! -L "$path" ]; then
    [ "$(stat -c '%a:%u:%g' "$path" 2>/dev/null)" = "$mode:$expected_owner" ] \
      || refuse "$label permissions drifted; run yard init"
    return 0
  else
    refuse "$label is missing; run yard init"
  fi
  return 1
}

check_approval_hash() {
  if approval_file_present "$1" "$2" "$3" "$4"; then
    [ "$(sha256sum < "$1" | cut -d' ' -f1)" = "$2" ] \
      || refuse "$4 content drifted; run yard init"
  fi
}

check_git_approval_hook() {
  [ -x /usr/bin/python3 ] || refuse 'managed approval hook interpreter is missing; run yard init'
  check_approval_hash "$git_hook" "$git_hook_sha" 755 'managed approval hook'
}
# END generated Git approval hook

modes_valid=1
case "$GIT_APPROVAL" in prompt|off) ;; *) printf 'Claude policy: CODING_TOOL_GIT_APPROVAL must be prompt or off\n' >&2; modes_valid=0 ;; esac
case "$DEV_SUDO" in yes|no|prompt) ;; *) printf 'Claude policy: DEV_SUDO must be yes, no or prompt\n' >&2; modes_valid=0 ;; esac
[ "$modes_valid" = 1 ] || exit 1
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
update_policy="$POLICY_DIR/48-subyard-update-lock.json"
hook_policy="$POLICY_DIR/49-subyard-approval-hook.json"
git_policy="$POLICY_DIR/50-subyard-git-approval.json"
sudo_policy="$POLICY_DIR/51-subyard-sudo-approval.json"
git_hook="$(dirname "$POLICY_DIR")/subyard-git-approval-hook"
if [ "$GIT_APPROVAL" = prompt ] || [ "$DEV_SUDO" = prompt ]; then
  [ -x /usr/bin/python3 ] || die '/usr/bin/python3 is required for managed approval'
  if [ -L "$git_hook" ] || { [ -e "$git_hook" ] && [ ! -f "$git_hook" ]; }; then
    die 'managed approval hook is not a regular file'
  fi
  if [ -f "$git_hook" ] && ! grep -Fxq '# Managed by Subyard Git approval provision.' "$git_hook"; then
    die 'existing approval hook belongs to another policy'
  fi
fi
for path in "$update_policy" "$hook_policy" "$git_policy" "$sudo_policy" "$CHECK_PATH"; do
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
approval_rules() {
  jq -n --arg marker "$MARKER" --args '
    {"$comment": $marker, permissions: {ask: ($ARGS.positional | map("Bash(\(.))", "Bash(\(.):*)"))}}
  ' -- "$@"
}
git_approval_policy() {
  [ "$GIT_APPROVAL" != prompt ] || approval_rules 'git commit' 'git push'
}
sudo_approval_policy() {
  [ "$DEV_SUDO" != prompt ] || approval_rules sudo su doas pkexec sudoedit run0
}
approval_hook_policy() {
  [ -n "$git_policy_json" ] || [ -n "$sudo_policy_json" ] || return 0
  jq -n --arg marker "$MARKER" --argjson command "$(approval_command)" '
    {"$comment": $marker, hooks: {PreToolUse: [{matcher: "Bash", hooks: [{
      type: "command", command: $command, timeout: 3, onFailure: "block"
    }]}]}}'
}
check_policy_json() {
  if approval_file_present "$1" "$2" 644 "$3"; then
    jq -e --argjson expected "$2" '. == $expected' "$1" >/dev/null 2>&1 \
      || refuse "$3 drifted or could not be read; run yard init"
  fi
}
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
update_policy_json='{"$comment":"Subyard Claude permission policy","env":{"DISABLE_UPDATES":"1"}}'
git_policy_json="$(git_approval_policy)"
sudo_policy_json="$(sudo_approval_policy)"
hook_policy_json="$(approval_hook_policy)"
reconcile_approval_file "$update_policy" "$update_policy_json"
if [ -n "$hook_policy_json" ]; then
  git_approval_hook | publish_approval_file "$git_hook" 755
  reconcile_approval_file "$hook_policy" "$hook_policy_json"
fi
# Supersede the old combined policy only after the replacement sudo rules and hook exist.
reconcile_approval_file "$sudo_policy" "$sudo_policy_json"
reconcile_approval_file "$git_policy" "$git_policy_json"
[ -n "$hook_policy_json" ] || reconcile_approval_file "$hook_policy" ''
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
  printf 'policy_dir=%q\nexpected_owner=%q\n' "$POLICY_DIR" "$expected_owner"
  printf 'update_policy=%q\nhook_policy=%q\ngit_policy=%q\nsudo_policy=%q\n' \
    "$update_policy" "$hook_policy" "$git_policy" "$sudo_policy"
  printf 'update_policy_json=%q\nhook_policy_json=%q\ngit_policy_json=%q\nsudo_policy_json=%q\n' \
    "$update_policy_json" "$hook_policy_json" "$git_policy_json" "$sudo_policy_json"
  printf 'runtime=%q\nlink=%q\n' "$RUNTIME_PATH" "$BIN_LINK"
  printf 'DEV_USER=%q\nDEV_HOME=%q\n' "$DEV_USER" "$DEV_HOME"
  printf 'git_hook=%q\ngit_hook_sha=%q\n' "$git_hook" "$(git_approval_hook | sha256sum | cut -d' ' -f1)"
  declare -f approval_file_present check_approval_hash check_git_approval_hook check_policy_json is_working_binary dev_owned_directory native_shadow_state
  cat <<'CHECK'
# Report every refusal that does not depend on a failed read, in this order.
refusals=0
refuse() { printf 'claude-policy-check: %s\n' "$*" >&2; refusals=$((refusals + 1)); }
[ ! -L "$policy_dir" ] && [ "$(stat -c '%a:%u:%g' "$policy_dir" 2>/dev/null)" = "755:$expected_owner" ] \
  || refuse 'managed policy directory drifted; run yard init'
check_policy_json "$update_policy" "$update_policy_json" 'managed Claude update lock'
check_policy_json "$git_policy" "$git_policy_json" 'managed Git approval policy'
check_policy_json "$sudo_policy" "$sudo_policy_json" 'managed sudo approval policy'
check_policy_json "$hook_policy" "$hook_policy_json" 'managed approval hook registration'
if [ -n "$hook_policy_json" ]; then
  hook_dir="$(dirname "$git_hook")"
  [ ! -L "$hook_dir" ] && [ "$(stat -c '%a:%u:%g' "$hook_dir" 2>/dev/null)" = "755:$expected_owner" ] \
    || refuse 'managed approval hook directory drifted; run yard init'
  check_git_approval_hook
fi
[ -L "$link" ] && [ "$(readlink "$link")" = "$runtime" ] \
  || refuse 'canonical command does not point to the managed runtime; run yard init'
is_working_binary "$runtime" || refuse 'managed runtime failed validation; run yard init'
shadow="$(native_shadow_state)" || shadow=foreign
[ "$shadow" != native ] || refuse "a native self-installed link shadows $link; run yard init"
if [ "$refusals" = 0 ]; then
  printf 'Claude %s ready. ' "$BINARY_VERSION"
  if [ -z "$git_policy_json" ]; then
    printf 'Managed Claude update lock installed; managed Git prompts off (CODING_TOOL_GIT_APPROVAL=off).'
  else
    printf 'Managed Claude Git ask rules, Git hook and update lock installed; inspect /status and /permissions in the active client.'
  fi
  [ -z "$sudo_policy_json" ] || printf ' Managed sudo ask rules installed (DEV_SUDO=prompt).'
  printf '\n'
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

