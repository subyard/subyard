#!/usr/bin/env bash
# Reconcile the current stable official Codex package without touching user authorization.
set -euo pipefail

INSTALL_ROOT="${CODEX_INSTALL_ROOT:-/opt/subyard/codex}"
RELEASES_ROOT="$INSTALL_ROOT/releases"
# The command stays on this path, which every retained release can replace on rollback. It links to
# the current release; earlier provisioning left a bare binary here, which the first swap replaces.
RUNTIME_LINK="$INSTALL_ROOT/codex"
BIN_LINK="${CODEX_BIN_LINK:-/usr/local/bin/codex}"
CHECK_PATH="${CODEX_CHECK_PATH:-/usr/local/bin/codex-policy-check}"
REQUIREMENTS_PATH="${CODEX_REQUIREMENTS_PATH:-/etc/codex/requirements.toml}"
RELEASE_LATEST_URL=https://github.com/openai/codex/releases/latest
RELEASE_BASE_URL=https://github.com/openai/codex/releases/download
ARCH="${CODEX_ARCH:-}"
DEV_USER="${DEV_USER:-dev}"
DEV_HOME="${CODEX_DEV_HOME:-}"
MANAGED_CHECK_MARKER='# Managed by Subyard Codex provision.'
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

# Shared with Codex, which passes --plain; Claude needs no extra argument.
# shellcheck disable=SC2120
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
  # The generated policy check assigns the expected hash before calling this helper.
  # shellcheck disable=SC2154
  check_approval_hash "$git_hook" "$git_hook_sha" 755 'managed approval hook'
}
# END generated Git approval hook

git_approval_rules() {
  [ "$GIT_APPROVAL" = prompt ] || return 0
  printf '%s\n' "$MANAGED_CHECK_MARKER"
  cat <<'RULES'
  { pattern = [{ token = "git" }, { any_of = ["commit", "push"] }], decision = "prompt", justification = "Creating or amending commits and pushing require explicit operator approval." },
RULES
}

sudo_approval_rules() {
  [ "$DEV_SUDO" = prompt ] || return 0
  local command
  printf '%s\n' "$MANAGED_CHECK_MARKER"
  for command in sudo su doas pkexec sudoedit run0; do
    printf '  { pattern = [{ token = "%s" }], decision = "prompt", justification = "Privilege escalation requires explicit operator approval." },\n' "$command"
  done
}

approval_hook_requirements() {
  [ -n "$git_rules" ] || [ -n "$sudo_rules" ] || return 0
  cat <<REQUIREMENTS
$MANAGED_CHECK_MARKER

[features]
hooks = true

[hooks]
managed_dir = $(jq -n --arg path "$requirements_dir" '$path')

[[hooks.PreToolUse]]
matcher = "Bash"

[[hooks.PreToolUse.hooks]]
type = "command"
# Codex matches its prompt rules only against plain command sequences; the hook refuses the rest.
command = $(approval_command --plain)
timeout = 3
REQUIREMENTS
}

# Codex loads one system requirements file. Publish the native projection atomically.
requirements() {
  cat <<'REQUIREMENTS'
# Managed by Subyard Codex provision.
allowed_approval_policies = ["on-request"]
allowed_approvals_reviewers = ["user"]
check_for_update_on_startup = false
REQUIREMENTS
  [ -f "$hook_policy" ] || return 0
  printf '\n[rules]\nprefix_rules = [\n'
  local policy
  for policy in "$git_policy" "$sudo_policy"; do
    [ ! -f "$policy" ] || tail -n +2 -- "$policy"
  done
  printf ']\n'
  tail -n +2 -- "$hook_policy"
}

die() { printf 'Codex provision: %s\n' "$*" >&2; exit 1; }
# The preflight changes nothing, so it reports every refusal in this order before stopping.
refusals=0
refuse() { printf 'Codex provision: %s\n' "$*" >&2; refusals=$((refusals + 1)); }

if [ "$(id -u)" -ne 0 ] && [ "${CODEX_TEST_ALLOW_NON_ROOT:-0}" != 1 ]; then
  refuse "must run as root"
fi

for path in "$INSTALL_ROOT" "$BIN_LINK" "$CHECK_PATH" "$REQUIREMENTS_PATH"; do
  case "$path" in /*) ;; *) refuse "install paths must be absolute" ;; esac
done
case "$DEV_USER" in ''|*[!A-Za-z0-9._-]*) refuse "DEV_USER is invalid" ;; esac
case "$GIT_APPROVAL" in prompt|off) ;; *) refuse "CODING_TOOL_GIT_APPROVAL must be prompt or off" ;; esac
case "$DEV_SUDO" in yes|no|prompt) ;; *) refuse "DEV_SUDO must be yes, no or prompt" ;; esac

if [ -z "$ARCH" ]; then
  if command -v dpkg >/dev/null 2>&1; then
    ARCH="$(dpkg --print-architecture)"
  else
    refuse "dpkg is required to detect the architecture"
  fi
fi
case "$ARCH" in
  amd64)
    TARGET=x86_64-unknown-linux-musl
    ;;
  arm64)
    TARGET=aarch64-unknown-linux-musl
    ;;
  '') ;;
  *) refuse "unsupported architecture '$ARCH' (supported: amd64, arm64)" ;;
esac

for tool in curl jq sort sha256sum tar od stat; do
  command -v "$tool" >/dev/null 2>&1 || refuse "$tool is required"
done

expected_owner="$(id -u):$(id -g)"
if [ "$(id -u)" -eq 0 ]; then expected_owner=0:0; fi
# Hermetic non-root callers name their fixture home; only root provisioning resolves the account.
if [ -z "$DEV_HOME" ] && [ "$(id -u)" -eq 0 ]; then
  DEV_HOME="$(getent passwd "$DEV_USER" | cut -d: -f6 || true)"
fi

# Run writes in the developer's tree with the developer's own authority.
as_dev() {
  if [ "$(id -u)" -eq 0 ]; then runuser -u "$DEV_USER" -- "$@"; else "$@"; fi
}

# Developer-tree repairs apply only where the developer can make them; anything else is a conflict.
dev_owned_directory() {
  [ -d "$1" ] && [ ! -L "$1" ] && [ "$(stat -c '%U' "$1")" = "$DEV_USER" ] \
    && (( (8#$(stat -c '%a' "$1") & 0200) != 0 ))
}

# The standalone installer publishes this exact link. It shadows the managed runtime in login
# shells; its release directories stay in place for any session still running them.
standalone_shadow_state() {
  local link="$DEV_HOME/.local/bin/codex"
  [ -n "$DEV_HOME" ] || { printf 'absent'; return; }
  if [ -L "$link" ]; then
    if [ "$(readlink "$link")" = "$DEV_HOME/.codex/packages/standalone/current/bin/codex" ] \
      && [ "$(stat -c '%U' "$link")" = "$DEV_USER" ] && dev_owned_directory "$(dirname "$link")"; then
      printf 'standalone'
    else
      printf 'foreign'
    fi
  elif [ -e "$link" ]; then
    printf 'foreign'
  else
    printf 'absent'
  fi
}

release_standalone_shadow() {
  [ "$(standalone_shadow_state)" = standalone ] || return 0
  if as_dev rm -f -- "$DEV_HOME/.local/bin/codex"; then
    printf 'Released the standalone self-installed codex link from %s/.local/bin\n' "$DEV_HOME"
  else
    printf 'Conflict: could not remove %s/.local/bin/codex.\n' "$DEV_HOME"
  fi
}

# The runtime link's target, relative to the install root.
is_release_target() {
  [[ "$1" =~ ^releases/[0-9]+\.[0-9]+\.[0-9]+/bin/codex$ ]]
}

is_native_binary() {
  local magic
  magic="$(LC_ALL=C od -An -tx1 -N4 "$1" 2>/dev/null | tr -d '[:space:]')"
  [ "$magic" = 7f454c46 ]
}

is_working_binary() {
  local path="$1" output
  [ -f "$path" ] && [ ! -L "$path" ] && [ -x "$path" ] || return 1
  [ "$(stat -c '%a' "$path" 2>/dev/null)" = 755 ] || return 1
  [ "$(stat -c '%u:%g' "$path" 2>/dev/null)" = "$expected_owner" ] || return 1
  is_native_binary "$path" || return 1
  output="$("$path" --version 2>/dev/null)" || return 1
  [[ "$output" =~ ^codex-cli\ ([0-9]+\.[0-9]+\.[0-9]+([+-][0-9A-Za-z.-]+)?)$ ]] || return 1
  BINARY_VERSION="${BASH_REMATCH[1]}"
  "$path" app-server --help >/dev/null 2>&1
}

# Never replace a command or package check that is not visibly ours.
if [ -L "$BIN_LINK" ]; then
  [ "$(readlink "$BIN_LINK")" = "$RUNTIME_LINK" ] \
    || refuse "$BIN_LINK is managed by another install — leaving it unchanged"
elif [ -e "$BIN_LINK" ]; then
  refuse "$BIN_LINK already exists outside the Subyard-managed runtime — leaving it unchanged"
fi
current_target=''
if [ -L "$RUNTIME_LINK" ]; then
  current_target="$(readlink "$RUNTIME_LINK")"
  is_release_target "$current_target" \
    || refuse "$RUNTIME_LINK is managed by another install — leaving it unchanged"
elif [ -e "$RUNTIME_LINK" ] && [ ! -f "$RUNTIME_LINK" ]; then
  refuse "$RUNTIME_LINK is not a managed runtime — leaving it unchanged"
fi
if [ -L "$CHECK_PATH" ] || { [ -e "$CHECK_PATH" ] && [ ! -f "$CHECK_PATH" ]; }; then
  refuse "$CHECK_PATH is not a managed regular file — leaving it unchanged"
elif [ -f "$CHECK_PATH" ] && ! grep -Fxq "$MANAGED_CHECK_MARKER" "$CHECK_PATH"; then
  refuse "$CHECK_PATH is managed by another install — leaving it unchanged"
fi
for path in "$INSTALL_ROOT" "$RELEASES_ROOT"; do
  if [ -L "$path" ] || { [ -e "$path" ] && [ ! -d "$path" ]; }; then
    refuse "$path is not a managed directory — leaving it unchanged"
  fi
done
requirements_dir="$(dirname "$REQUIREMENTS_PATH")"
if [ -L "$requirements_dir" ] || { [ -e "$requirements_dir" ] && [ ! -d "$requirements_dir" ]; }; then
  refuse "$requirements_dir is not a regular directory — leaving it unchanged"
fi
hook_policy="$requirements_dir/49-subyard-approval-hook.toml"
git_policy="$requirements_dir/50-subyard-git-approval.rules.inc"
sudo_policy="$requirements_dir/51-subyard-sudo-approval.rules.inc"
for path in "$REQUIREMENTS_PATH" "$hook_policy" "$git_policy" "$sudo_policy"; do
  if [ -L "$path" ] || { [ -e "$path" ] && [ ! -f "$path" ]; }; then
    refuse "$path is not a managed regular file — leaving it unchanged"
  elif [ -f "$path" ] && ! grep -Fxq "$MANAGED_CHECK_MARKER" "$path"; then
    refuse "$path is managed by another policy — leaving it unchanged"
  fi
done
git_hook="$requirements_dir/subyard-git-approval-hook"
if [ "$GIT_APPROVAL" = prompt ] || [ "$DEV_SUDO" = prompt ]; then
  [ -x /usr/bin/python3 ] || refuse '/usr/bin/python3 is required for managed approval'
  if [ -L "$git_hook" ] || { [ -e "$git_hook" ] && [ ! -f "$git_hook" ]; }; then
    refuse 'managed approval hook is not a regular file'
  elif [ -f "$git_hook" ] && ! grep -Fxq '# Managed by Subyard Git approval provision.' "$git_hook"; then
    refuse 'existing approval hook belongs to another policy'
  fi
fi
[ "$refusals" = 0 ] || exit 1

git_rules="$(git_approval_rules)"
sudo_rules="$(sudo_approval_rules)"
hook_rules="$(approval_hook_requirements)"

install_requirements() {
  install -d -m 0755 "$requirements_dir"
  if [ "$(id -u)" -eq 0 ]; then chown 0:0 "$requirements_dir"; fi
  if [ -n "$hook_rules" ]; then
    git_approval_hook | publish_approval_file "$git_hook" 755
  fi
  reconcile_approval_file "$git_policy" "$git_rules"
  reconcile_approval_file "$sudo_policy" "$sudo_rules"
  reconcile_approval_file "$hook_policy" "$hook_rules"
  requirements | publish_approval_file "$REQUIREMENTS_PATH" 644
}

approval_hash() {
  [ -z "$1" ] || printf '%s\n' "$1" | sha256sum | cut -d' ' -f1
}

install_check() {
  local hook_sha check_dir check_stage q_root q_link q_user q_home q_owner q_requirements requirements_sha
  check_dir="$(dirname "$CHECK_PATH")"
  mkdir -p "$check_dir"
  check_stage="$(mktemp "$check_dir/.codex-policy-check.XXXXXX")"
  printf -v q_root '%q' "$INSTALL_ROOT"
  printf -v q_link '%q' "$BIN_LINK"
  printf -v q_user '%q' "$DEV_USER"
  printf -v q_home '%q' "$DEV_HOME"
  printf -v q_owner '%q' "$expected_owner"
  printf -v q_requirements '%q' "$REQUIREMENTS_PATH"
  hook_sha="$(git_approval_hook | sha256sum | cut -d' ' -f1)"
  requirements_sha="$(requirements | sha256sum | cut -d' ' -f1)"
  cat > "$check_stage" <<EOF
#!/usr/bin/env bash
$MANAGED_CHECK_MARKER
set -euo pipefail
install_root=$q_root
link=$q_link
dev_user="\${CODEX_CHECK_DEV_USER:-$q_user}"
DEV_USER="\$dev_user"
DEV_HOME=$q_home
expected_owner=$q_owner
requirements=$q_requirements
git_hook=$(printf %q "$git_hook")
hook_policy_sha=$(approval_hash "$hook_rules")
git_hook_sha=$hook_sha
# Report every refusal that does not depend on a failed read, in this order.
refusals=0
refuse() { printf 'codex-policy-check: %s\\n' "\$*" >&2; refusals=\$((refusals + 1)); }
$(declare -f approval_file_present check_approval_hash check_git_approval_hook dev_owned_directory standalone_shadow_state is_release_target)
requirements_present=0
if [ -f "\$requirements" ] && [ ! -L "\$requirements" ]; then
  requirements_present=1
else
  refuse "managed approval requirements are missing"
fi
[ ! -L "\$(dirname "\$requirements")" ] \
  && [ "\$(stat -c '%a:%u:%g' "\$(dirname "\$requirements")" 2>/dev/null)" = "755:\$expected_owner" ] \
  || refuse "managed approval requirements directory drifted"
if [ "\$requirements_present" = 1 ]; then
  check_approval_hash "\$requirements" '$requirements_sha' 644 'managed approval requirements'
fi
check_approval_hash $(printf %q "$git_policy") '$(approval_hash "$git_rules")' 644 'managed Git approval policy'
check_approval_hash $(printf %q "$sudo_policy") '$(approval_hash "$sudo_rules")' 644 'managed sudo approval policy'
check_approval_hash $(printf %q "$hook_policy") "\$hook_policy_sha" 644 'managed approval hook registration'
[ -z "\$hook_policy_sha" ] || check_git_approval_hook
target="\$(readlink "\$install_root/codex" 2>/dev/null)" || target=''
runtime="\$install_root/\$target"
runtime_present=0
if [ "\$(readlink "\$link" 2>/dev/null)" != "\$install_root/codex" ] || ! is_release_target "\$target"; then
  refuse "canonical command does not point to a managed Codex release; run yard init"
elif [ -f "\$runtime" ] && [ ! -L "\$runtime" ] && [ -x "\$runtime" ]; then
  runtime_present=1
  [ "\$(stat -c '%a' "\$runtime" 2>/dev/null)" = 755 ] || refuse "managed runtime mode is not 0755"
  [ "\$(stat -c '%u:%g' "\$runtime" 2>/dev/null)" = "\$expected_owner" ] \
    || refuse "managed runtime owner drifted"
else
  refuse "managed runtime is missing"
fi
dev_home="\$(getent passwd "\$dev_user" | cut -d: -f6)" || dev_home=''
run_as_dev() {
  if [ "\$(id -un)" = "\$dev_user" ]; then
    env HOME="\$dev_home" CODEX_HOME="\$dev_home/.codex" "\$@"
  else
    runuser -u "\$dev_user" -- env HOME="\$dev_home" CODEX_HOME="\$dev_home/.codex" "\$@"
  fi
}
can_run_as_dev=1
if [ -z "\$dev_home" ]; then
  refuse "could not resolve home for \$dev_user"
  can_run_as_dev=0
elif [ "\$(id -un)" != "\$dev_user" ] && ! command -v runuser >/dev/null 2>&1; then
  refuse "runuser is required"
  can_run_as_dev=0
fi
version=''
if [ "\$can_run_as_dev" = 1 ]; then
  run_as_dev bwrap --unshare-user --unshare-net --ro-bind / / --chdir / -- /bin/true >/dev/null 2>&1 \
    || refuse "bubblewrap could not create the developer sandbox; check yard user namespace permissions"
  if [ "\$runtime_present" = 1 ]; then
    if output="\$(run_as_dev "\$runtime" --version 2>/dev/null)"; then
      if [[ "\$output" =~ ^codex-cli\ ([0-9]+\.[0-9]+\.[0-9]+([+-][0-9A-Za-z.-]+)?)\$ ]]; then
        version="\${BASH_REMATCH[1]}"
      else
        refuse "runtime version response is invalid"
      fi
    else
      refuse "runtime version check failed"
    fi
    run_as_dev "\$runtime" app-server --help >/dev/null 2>&1 \
      || refuse "Codex app-server readiness check failed"
  fi
fi
shadow="\$(standalone_shadow_state)" || shadow=foreign
[ "\$shadow" != standalone ] \
  || refuse "a standalone self-installed link shadows the managed runtime; run yard init"
[ "\$refusals" != 0 ] || printf 'codex %s ready\\n' "\$version"
if [ "\$shadow" = foreign ]; then
  printf 'Conflict: %s/.local/bin/codex is not Subyard-owned and was left unchanged; login shells can run it instead of %s.\\n' "\$DEV_HOME" "\$link"
fi
[ "\$refusals" = 0 ]
EOF
  chmod 0755 "$check_stage"
  if [ "$(id -u)" -eq 0 ]; then chown 0:0 "$check_stage"; fi
  mv -fT -- "$check_stage" "$CHECK_PATH"
}

# Conflicts do not fail readiness, so surface them when provisioning runs.
run_check() {
  local output status=0
  output="$("$CHECK_PATH")" || status=$?
  printf '%s\n' "$output" | grep '^Conflict:' || true
  return "$status"
}

# A running client keeps its image; only the command moves. A retained release's hook replaces this link.
publish_command() { # runtime link target
  local link_stage="$RUNTIME_LINK.subyard-$$"
  ln -s "$1" "$link_stage"
  mv -fT -- "$link_stage" "$RUNTIME_LINK"
  mkdir -p "$(dirname "$BIN_LINK")"
  if [ ! -L "$BIN_LINK" ]; then ln -s "$RUNTIME_LINK" "$BIN_LINK"; fi
  [ "$(readlink "$BIN_LINK")" = "$RUNTIME_LINK" ] && [ "$(readlink "$RUNTIME_LINK")" = "$1" ] \
    || die "canonical command verification failed"
}

release_in_use() { # directory
  local exe target
  for exe in /proc/[0-9]*/exe; do
    target="$(readlink "$exe" 2>/dev/null)" || continue
    case "$target" in "$1"/*) return 0 ;; esac
  done
  return 1
}

# Keep the published release, the one it replaced, and any release a live process runs.
prune_releases() { # version previous-target
  local entry previous=''
  if is_release_target "$2"; then previous="$INSTALL_ROOT/${2%/bin/codex}"; fi
  for entry in "$RELEASES_ROOT"/* "$RELEASES_ROOT"/.stage.*; do
    [ -d "$entry" ] && [ ! -L "$entry" ] || continue
    [ "$entry" = "$RELEASES_ROOT/$1" ] || [ "$entry" = "$previous" ] || release_in_use "$entry" || rm -rf -- "$entry"
  done
}

# The approval policy precedes package installation, every download, the binary it governs and
# every developer-tree repair, so a failed or slow step never leaves the CLI without it.
install_requirements

# Reconcile the system sandbox runtime on current-version reruns as well as fresh installs.
if ! command -v bwrap >/dev/null 2>&1 || ! bwrap --version >/dev/null 2>&1; then
  command -v apt-get >/dev/null 2>&1 || die 'apt-get is required to install the bubblewrap sandbox runtime'
  DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=a apt-get update -qq
  DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=a apt-get install -y -qq --no-install-recommends bubblewrap
  command -v bwrap >/dev/null 2>&1 && bwrap --version >/dev/null 2>&1 \
    || die 'bubblewrap sandbox runtime is unavailable after package installation'
fi

install -d -m 0755 "$INSTALL_ROOT" "$RELEASES_ROOT"
if [ "$(id -u)" -eq 0 ]; then chown 0:0 "$INSTALL_ROOT" "$RELEASES_ROOT"; fi

tmp="$(mktemp -d)"
stage=""
cleanup() {
  rm -rf "$tmp"
  [ -z "$stage" ] || rm -rf "$stage"
}
trap cleanup EXIT

artifact="codex-package-${TARGET}.tar.gz"
# Public release URLs avoid GitHub's shared anonymous REST quota. Retry transient
# failures ourselves: curl's normal retries also retry 429 rate-limit refusals.
download_metadata() {
  local attempt result response status reset retry_after reason
  # Four 60-second attempts and three 2-second pauses bound each metadata request.
  for attempt in 1 2 3 4; do
    : > "$tmp/headers"
    if response="$(curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
      --retry 0 --connect-timeout 20 --max-time 60 --dump-header "$tmp/headers" \
      --write-out $'%{http_code}\n%{url_effective}' --output "$1" "${@:2}")"; then
      metadata_url="${response#*$'\n'}"
      return 0
    else result=$?; fi
    status=''
    # Only curl's HTTP refusal exit identifies a failing HTTP response. A transport
    # failure can leave http_code pointing to a successful redirect or partial 200.
    [ "$result" != 22 ] || status="${response%%$'\n'*}"
    reason="release metadata download failed${status:+ (HTTP $status)}"
    if [ "$status" = 429 ] || { [ "$status" = 403 ] && \
      grep -Eiq $'^x-ratelimit-remaining: *0\r?$|^retry-after:' "$tmp/headers"; }; then
      reset="$(awk 'tolower($1) == "x-ratelimit-reset:" {gsub(/\r/, "", $2); print $2}' "$tmp/headers")"
      retry_after="$(awk 'tolower($1) == "retry-after:" {sub(/^[^:]*:[ \t]*/, ""); sub(/\r$/, ""); print}' "$tmp/headers")"
      if [[ "$retry_after" =~ ^[0-9]+$ ]]; then retry_after="$retry_after seconds"; fi
      reason="GitHub rate limit refused release metadata${reset:+ (reset at Unix time $reset)}${retry_after:+ (retry after $retry_after)}"
      break
    fi
    case "$status" in ''|5??) ;; *) break ;; esac
    [ "$attempt" = 4 ] || sleep 2
  done
  if is_release_target "$current_target" && is_working_binary "$INSTALL_ROOT/$current_target"; then
    install_check
    release_standalone_shadow
    run_check
    printf 'Retained working Codex %s at %s; latest release could not be verified: %s\n' "$BINARY_VERSION" "$BIN_LINK" "$reason"
    exit 0
  fi
  die "$reason"
}
download_metadata /dev/null --head "$RELEASE_LATEST_URL"
[[ "$metadata_url" =~ ^https://github\.com/openai/codex/releases/tag/rust-v((0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*))$ ]] \
  || die "latest release redirect does not name an official stable Codex release"
VERSION="${BASH_REMATCH[1]}"

if is_release_target "$current_target" && is_working_binary "$INSTALL_ROOT/$current_target"; then
  # A stale latest-release response must not roll back a newer working install.
  current_base="${BINARY_VERSION%%[-+]*}"
  newest_base="$(printf '%s\n' "$current_base" "$VERSION" | sort -V | tail -n 1)"
  if [ "$BINARY_VERSION" = "$VERSION" ] || \
    { [ "$current_base" != "$VERSION" ] && [ "$newest_base" = "$current_base" ]; } || \
    { [ "$current_base" = "$VERSION" ] && [[ "$BINARY_VERSION" = "$VERSION"+* ]]; }; then
    install_check
    release_standalone_shadow
    run_check
    printf 'Codex %s is already current at %s\n' "$BINARY_VERSION" "$BIN_LINK"
    exit 0
  fi
fi

# OpenAI publishes this SHA-256 manifest with the packages. Pin both the checksum
# source and archive to the resolved stable tag; never download an unchecked binary.
url="$RELEASE_BASE_URL/rust-v$VERSION/$artifact"
download_metadata "$tmp/SHA256SUMS" "$RELEASE_BASE_URL/rust-v$VERSION/codex-package_SHA256SUMS"
EXPECTED_SHA256="$(jq -Rers --arg artifact "$artifact" '
  [split("\n")[] | select(endswith($artifact))] | select(length == 1) |
  .[0] | rtrimstr("  " + $artifact) | select(test("^[0-9a-f]{64}$"))
' "$tmp/SHA256SUMS")" || die "release checksum manifest lacks a unique SHA-256 for $artifact"

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
archive="$tmp/$artifact"
download_artifact "$url" "$archive" || die "release artifact download failed"

# Fail closed on a corrupt partial; cleanup discards it before the next invocation.
actual_sha256="$(sha256sum "$archive" | cut -d' ' -f1)"
[ "$actual_sha256" = "$EXPECTED_SHA256" ] \
  || die "checksum mismatch for Codex $VERSION ($ARCH)"
tar -tvzf "$archive" > "$tmp/members" 2>/dev/null || die "Codex archive is invalid"
tar -tzf "$archive" > "$tmp/names" 2>/dev/null || die "Codex archive is invalid"
reject_archive_match() {
  if grep -Eq "$1" "$2"; then
    die "Codex archive has a member that is not a contained file or directory"
  elif [ "$?" -ne 1 ]; then
    die "Codex archive listing could not be scanned"
  fi
}
reject_archive_match '^[^-d]' "$tmp/members"
reject_archive_match '^/|(^|/)\.\.(/|$)' "$tmp/names"

# Validate fully before the atomic rename so a bad release cannot break the old CLI.
stage="$(mktemp -d "$RELEASES_ROOT/.stage.XXXXXX")"
tar -xzf "$archive" -C "$stage" --no-same-owner 2>/dev/null || die "Codex archive could not be extracted"
chmod -R u+w,go-w,go+rX "$stage"
if [ "$(id -u)" -eq 0 ]; then chown -R 0:0 "$stage"; fi
is_working_binary "$stage/bin/codex" && [ "$BINARY_VERSION" = "$VERSION" ] \
  || die "staged package failed version or app-server validation for '$VERSION'"
# A same-version reinstall moves the old tree aside, so a failed move can put it back.
release_dir="$RELEASES_ROOT/$VERSION"
aside=''
if [ -e "$release_dir" ]; then
  aside="$RELEASES_ROOT/.stage.aside-$$"
  mv -T -- "$release_dir" "$aside"
fi
if ! mv -T -- "$stage" "$release_dir"; then
  [ -z "$aside" ] || mv -T -- "$aside" "$release_dir"
  die "the staged package could not be published"
fi
stage=""

publish_command "releases/$VERSION/bin/codex"
install_check
prune_releases "$VERSION" "$current_target"
release_standalone_shadow
is_working_binary "$release_dir/bin/codex" && [ "$BINARY_VERSION" = "$VERSION" ] \
  || die "installed runtime verification failed"
run_check
printf 'Installed Codex %s at %s\n' "$VERSION" "$BIN_LINK"
