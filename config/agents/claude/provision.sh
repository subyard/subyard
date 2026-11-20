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
DEV_SUDO="${DEV_SUDO:-yes}"

# BEGIN generated Git approval hook
git_approval_hook() {
  cat <<'GIT_APPROVAL_HOOK'
#!/usr/bin/python3 -I
# Managed by Subyard Git approval provision.
"""Recognize literal gated command words without executing or expanding shell input.

Arguments name the gates: git (commit and push) and sudo (privilege escalation).
"""

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
ESCALATION = {"sudo", "su", "doas", "pkexec", "sudoedit", "run0"}
SHELLS = {"sh", "bash", "dash", "zsh", "ksh"}
# Transparent wrappers: short option letters and long options that take an operand.
WRAPPERS = {
    "env": ("uCP", {"--unset", "--chdir"}),
    "exec": ("a", set()),
    "nice": ("n", {"--adjustment"}),
    "time": ("fo", {"--format", "--output"}),
    "nohup": ("", set()),
    "command": ("", set()),
    "timeout": ("sk", {"--signal", "--kill-after"}),
    "setsid": ("", set()),
    "stdbuf": ("ioe", {"--input", "--output", "--error"}),
    "xargs": ("adEILnPs", {"--arg-file", "--delimiter", "--max-args", "--max-procs",
                          "--max-chars", "--process-slot-var"}),
}
# Words that start a compound command; a command after them is not direct.
KEYWORDS = {"{", "if", "then", "elif", "else", "do", "while", "until", "!"}
FIND_EXEC = {"-exec", "-execdir", "-ok", "-okdir"}
SUBSTITUTION = "_"


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


def matching_paren(text, index):
    """Return the index after the parenthesis that closes a substitution body at index."""
    depth, quote = 1, None
    while index < len(text):
        char = text[index]
        if char == "\\" and quote != "'":
            index += 2
            continue
        if quote:
            if char == quote:
                quote = None
        elif char in "'\"":
            quote = char
        elif char == "(":
            depth += 1
        elif char == ")":
            depth -= 1
            if depth == 0:
                return index + 1
        index += 1
    return len(text)


def body_substitutions(text):
    """Return the substitutions an unquoted heredoc body expands."""
    bodies, index = [], 0
    while index < len(text):
        if text[index] == "\\":
            index += 2
        elif text.startswith("$(", index):
            end = matching_paren(text, index + 2)
            bodies.append(text[index + 2:end - 1])
            index = end
        elif text[index] == "`":
            end = text.find("`", index + 1)
            end = len(text) if end < 0 else end
            bodies.append(text[index + 1:end])
            index = end + 1
        else:
            index += 1
    return bodies


def split_substitutions(command):
    """Separate command and backtick substitutions and heredoc bodies from the shell text.

    Returns the text with each substitution replaced by a placeholder word and each
    heredoc body removed, and the substitution bodies, which run as nested shell text.
    Single-quoted text, comments and quoted heredoc bodies are data, not commands.
    """
    out, bodies, heredocs = [], [], []
    index, quote = 0, None
    while index < len(command):
        char = command[index]
        if char == "\\" and quote != "'":
            out.append(command[index:index + 2])
            index += 2
            continue
        if quote == "'":
            out.append(char)
            quote = None if char == "'" else quote
            index += 1
            continue
        if char == "`":
            end = command.find("`", index + 1)
            end = len(command) if end < 0 else end
            bodies.append(command[index + 1:end])
            out.append(SUBSTITUTION)
            index = end + 1
            continue
        if command.startswith("$(", index):
            end = matching_paren(command, index + 2)
            bodies.append(command[index + 2:end - 1])
            out.append(SUBSTITUTION)
            index = end
            continue
        if quote == '"':
            out.append(char)
            quote = None if char == '"' else quote
            index += 1
            continue
        if char in "'\"":
            quote = char
        elif char == "#" and (index == 0 or command[index - 1] in " \t\n;&|()"):
            end = command.find("\n", index)
            index = len(command) if end < 0 else end
            continue
        elif command.startswith("<<", index) and not command.startswith("<<<", index):
            match = re.compile(r"<<(-?)[^\S\n]*((?:'[^']*'|\"[^\"]*\"|\\.|[^\s;&|()<>])+)").match(command, index)
            if match:
                word = match[2]
                heredocs.append((re.sub(r"""['"\\]""", "", word), match[1] == "-",
                                 any(c in word for c in "'\"\\")))
                out.append(match[0])
                index = match.end()
                continue
        elif char == "\n" and heredocs:
            out.append(char)
            index += 1
            for delimiter, tabs, literal in heredocs:
                lines = []
                while index < len(command):
                    end = command.find("\n", index)
                    end = len(command) if end < 0 else end
                    line = command[index:end]
                    index = end + 1
                    if (line.lstrip("\t") if tabs else line) == delimiter:
                        break
                    lines.append(line)
                if not literal:
                    bodies.extend(body_substitutions("\n".join(lines)))
            heredocs = []
            continue
        out.append(char)
        index += 1
    return "".join(out), bodies


def simple_commands(command):
    """Yield each simple command's words and whether a group or subshell starts it."""
    words = []
    grouped = False
    offset = 0
    array_depth = 0
    heredoc_word = False
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
            # The delimiter names the removed heredoc body; it is not a command word.
            heredoc_word = True
            continue
        if raw.startswith("#"):
            continue
        if raw.startswith("\n") or raw[0] in ";&|()":
            if words:
                yield words, grouped
                words = []
            grouped = raw == "("
        elif not raw.isspace():
            if heredoc_word:
                heredoc_word = False
                continue
            words.append((raw, token.start(), token.end()))
    if words:
        yield words, grouped


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


def unwrap(executable, args):
    """Return the offset of the command a transparent wrapper runs.

    None means that the wrapper runs no command; a list is the words of an env -S command.
    """
    index = 0
    letters, long_values = WRAPPERS[executable]
    while index < len(args):
        arg = args[index]
        if arg == "--":
            index += 1
            break
        if executable == "env" and ASSIGNMENT.match(arg):
            index += 1
        elif executable == "env" and arg.startswith("--split-string"):
            script = arg.partition("=")[2] if "=" in arg else (args[index + 1] if index + 1 < len(args) else "")
            rest = args[index + (1 if "=" in arg else 2):]
            return split_words(script) + rest
        elif arg.startswith("--"):
            index += 2 if arg in long_values else 1
        elif arg.startswith("-") and arg != "-":
            # command -v and -V only describe the named command.
            if executable == "command" and ("v" in arg or "V" in arg):
                return None
            index += 1
            for position, letter in enumerate(arg[1:], 1):
                if executable == "env" and letter == "S":
                    script = arg[position + 1:]
                    if not script:
                        script, index = (args[index] if index < len(args) else ""), index + 1
                    return split_words(script) + args[index:]
                if letter in letters:
                    # The rest of the word is the operand, or the next argument is.
                    index += 0 if arg[position + 1:] else 1
                    break
        else:
            break
    if executable == "timeout":
        index += 1
    return index if index < len(args) else None


def split_words(text):
    """Split an env -S string as env does: quotes group words; shell operators are text."""
    try:
        return shlex.split(text, comments=False)
    except ValueError:
        return []


def gated(executable, args, gates):
    """Return the gated command name, if this simple command runs one."""
    if "git" in gates and executable == "git":
        action = git_action(args)
        return action and f"git {action}"
    if "sudo" in gates and executable in ESCALATION:
        return executable
    return None


def nested_words(words, gates, depth):
    """Check words that another program runs as a command; none of them is direct."""
    return denied_action(shlex.join(words), gates, nested=True, depth=depth + 1) if words else None


def denied_action(command, gates=("git",), nested=False, depth=0):
    if depth == 8:
        return None
    command, substitutions = split_substitutions(command.replace("\\\n", ""))
    for body in substitutions:
        action = denied_action(body, gates, nested=True, depth=depth + 1)
        if action:
            return action
    for tokens, grouped in simple_commands(command):
        try:
            words = [shlex.split(raw, comments=False)[0] for raw, _, _ in tokens]
        except (IndexError, ValueError):
            continue
        start = 0
        while start < len(words) and ASSIGNMENT.match(tokens[start][0]):
            start += 1
        position, wrapped = start, grouped
        while position < len(words) and words[position] in KEYWORDS | {"function"}:
            position += 2 if words[position] == "function" else 1
            wrapped = True
        while position < len(words):
            executable = posixpath.basename(words[position])
            args = words[position + 1:]
            if executable not in WRAPPERS:
                break
            offset = unwrap(executable, args)
            if isinstance(offset, list):
                action = nested_words(offset, gates, depth)
                if action:
                    return action
            if not isinstance(offset, int):
                position = len(words)
            else:
                position, wrapped = position + 1 + offset, True
        if position >= len(words):
            continue
        action = gated(executable, args, gates)
        if action:
            raw = tokens[position][0] if executable in ESCALATION else (
                command[tokens[position][1]:tokens[position + 1][2]])
            if nested or wrapped or start or raw != action:
                return action
        elif executable in SHELLS:
            script = shell_command(executable, args)
            if script is not None:
                action = denied_action(script, gates, nested=True, depth=depth + 1)
                if action:
                    return action
        elif executable == "eval":
            action = denied_action(" ".join(args), gates, nested=True, depth=depth + 1)
            if action:
                return action
        elif executable == "find":
            for index, arg in enumerate(args):
                if arg in FIND_EXEC:
                    end = next((i for i in range(index + 1, len(args)) if args[i] in {";", "+"}), len(args))
                    action = nested_words(args[index + 1:end], gates, depth)
                    if action:
                        return action
    return None


def main():
    gates = set(sys.argv[1:]) or {"git"}
    payload = json.load(sys.stdin)
    action = denied_action(payload["tool_input"]["command"], gates)
    if action and action.startswith("git "):
        print(f"Git approval requires a direct `{action}` command. "
              f"For another directory, use `cd <directory> && {action}`.",
              file=sys.stderr)
        return 2
    if action:
        print(f"Sudo approval requires a direct `{action}` command at the start of a command, "
              f"without a path, wrapper, keyword, substitution or nested shell.",
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
  [ -n "$APPROVAL_GATES" ] || return 0
  hook_stage="$(mktemp "$(dirname "$git_hook")/.git-approval.XXXXXX")"
  git_approval_hook > "$hook_stage"
  chmod 0755 "$hook_stage"
  mv -fT -- "$hook_stage" "$git_hook"
}

check_git_approval_hook() {
  local git_hook="$1" git_hook_sha="$2" expected_owner="$3"
  [ -x /usr/bin/python3 ] || refuse 'managed approval hook interpreter is missing; run yard init'
  if [ -f "$git_hook" ] && [ ! -L "$git_hook" ]; then
    [ "$(stat -c '%a:%u:%g' "$git_hook" 2>/dev/null)" = "755:$expected_owner" ] \
      || refuse 'managed approval hook permissions drifted; run yard init'
    [ "$(sha256sum < "$git_hook" | cut -d' ' -f1)" = "$git_hook_sha" ] \
      || refuse 'managed approval hook content drifted; run yard init'
  else
    refuse 'managed approval hook is missing; run yard init'
  fi
}
# END generated Git approval hook

modes_valid=1
case "$GIT_APPROVAL" in prompt|off) ;; *) printf 'Claude policy: CODING_TOOL_GIT_APPROVAL must be prompt or off\n' >&2; modes_valid=0 ;; esac
case "$DEV_SUDO" in yes|no|prompt) ;; *) printf 'Claude policy: DEV_SUDO must be yes, no or prompt\n' >&2; modes_valid=0 ;; esac
[ "$modes_valid" = 1 ] || exit 1
# The hook receives the gates whose direct commands the ask rules prompt for.
APPROVAL_GATES=''
[ "$GIT_APPROVAL" != prompt ] || APPROVAL_GATES=git
[ "$DEV_SUDO" != prompt ] || APPROVAL_GATES="${APPROVAL_GATES:+$APPROVAL_GATES }sudo"
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
if [ -n "$APPROVAL_GATES" ]; then
  [ -x /usr/bin/python3 ] || die '/usr/bin/python3 is required for managed approval'
  if [ -L "$git_hook" ] || { [ -e "$git_hook" ] && [ ! -f "$git_hook" ]; }; then
    die 'managed approval hook is not a regular file'
  fi
  if [ -f "$git_hook" ] && ! grep -Fxq '# Managed by Subyard Git approval provision.' "$git_hook"; then
    die 'existing approval hook belongs to another policy'
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
POLICY_JSON="$(jq -cn --arg git "$GIT_APPROVAL" --arg sudo "$DEV_SUDO" '
  [if $git == "prompt" then "git commit", "git push" else empty end,
   if $sudo == "prompt" then "sudo", "su", "doas", "pkexec", "sudoedit", "run0" else empty end]
  | map("Bash(\(.))", "Bash(\(.):*)") as $ask
  | {"$comment": "Subyard Claude permission policy", env: {DISABLE_UPDATES: "1"}}
  + if $ask == [] then {} else {permissions: {ask: $ask}} end')"
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
if [ -n "$APPROVAL_GATES" ]; then
  # Keep the historical Git-only command for unchanged installed policies.
  POLICY_JSON="$(jq -c --arg path "$git_hook" --arg gates "$APPROVAL_GATES" '
    .hooks.PreToolUse = [{matcher:"Bash", hooks:[{
      type:"command", command:(if $gates == "git" then $path | @sh else ($path | @sh) + " " + $gates end),
      timeout:3, onFailure:"block"
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
  printf 'git_approval=%q\ndev_sudo=%q\npolicy_json=%q\n' "$GIT_APPROVAL" "$DEV_SUDO" "$POLICY_JSON"
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
if [ "$git_approval" = prompt ] || [ "$dev_sudo" = prompt ]; then
  hook_dir="$(dirname "$git_hook")"
  [ ! -L "$hook_dir" ] && [ "$(stat -c '%a:%u:%g' "$hook_dir" 2>/dev/null)" = "755:$expected_owner" ] \
    || refuse 'managed approval hook directory drifted; run yard init'
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
    printf 'Managed Claude update lock installed; managed Git prompts off (CODING_TOOL_GIT_APPROVAL=off).'
  else
    printf 'Managed Claude Git ask rules, Git hook and update lock installed; inspect /status and /permissions in the active client.'
  fi
  [ "$dev_sudo" != prompt ] || printf ' Managed sudo ask rules installed (DEV_SUDO=prompt).'
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

