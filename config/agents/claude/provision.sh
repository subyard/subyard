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
"""Refuse a literal mention of a gated command unless the client's own prompt covers it.

Arguments name the gates: git (commit and push) and sudo (privilege escalation).
--plain selects the Codex rules, which require the whole command to be plain; without
it, the Claude rules check the plain prefix up to the first direct gated command.
"""

import json
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


def git_action(words):
    """Return the subcommand a git argument list runs; ... marks one hidden by an expansion."""
    args = iter(words)
    for arg in args:
        if arg is None or "$" in arg or "`" in arg:
            return "..."
        if arg in GIT_VALUE_OPTIONS:
            next(args, None)
        elif arg in GIT_FLAGS or (
            "=" in arg and arg.partition("=")[0] in GIT_VALUE_OPTIONS | {"--exec-path"}
        ):
            continue
        else:
            return arg
    return None


def unquote(raw):
    """Return the value of one shell word; $'…' and $"…" lose their quoting prefix."""
    raw = re.sub(r"""(?<!\\)\$(?=['"])""", "", raw)
    try:
        return "".join(shlex.split(raw, comments=False))
    except ValueError:
        return re.sub(r"""[\\'"]""", "", raw)


def name(value):
    """Return the command name a word spells, also after an option's or assignment's =."""
    names = {posixpath.basename(value), posixpath.basename(value.rpartition("=")[2])}
    return next((n for n in ESCALATION + ("git",) if n in names), None)


def git_mention(rest):
    action = git_action(rest)
    if action in ("commit", "push"):
        return f"git {action}"
    if action == "...":
        # An expansion hides the subcommand; a later literal commit or push counts.
        for word in rest:
            if word in ("commit", "push"):
                return f"git {word}"
    return None


def mention(text, gates):
    """Return the first gated command the text names anywhere, in any position."""
    pending = [text]
    while pending:
        current = pending.pop(0)
        run = []
        offset = 0
        while offset <= len(current):
            token = TOKEN.match(current, offset) if offset < len(current) else None
            raw = token[0] if token else ""
            if token is None or raw.isspace() and "\n" in raw or raw[0] in ";&|\n#":
                # The end of a command's words: name a git subcommand in them.
                for index, word in enumerate(run):
                    if "git" in gates and word is not None and name(word) == "git":
                        found = git_mention(run[index + 1:])
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
                if "sudo" in gates and name(value) in ESCALATION:
                    return name(value)
                if len(value) < len(raw) and RESCAN.search(value):
                    pending.append(value)
                run.append(value)
            offset = token.end()
    return None


def native(segment, gates):
    """Return the gated command a segment starts with literally, as the client's rules match it."""
    names = (ESCALATION if "sudo" in gates else ()) + (GIT_ACTIONS if "git" in gates else ())
    for name in names:
        if segment == name or segment.startswith(name) and segment[len(name)] in " ;&|\n":
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
        if claude and native(command[start:], gates):
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
        if claude and pipeline[-1][1]:
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
        for segment, direct in pipeline:
            found = not direct and mention(segment, gates)
            if found and not (inert(segment) and all(d or inert(s) for s, d in pipeline)):
                return found, True
    return None, True


def main():
    gates = {arg for arg in sys.argv[1:] if not arg.startswith("--")} or {"git"}
    codex = "--plain" in sys.argv[1:]
    command = json.load(sys.stdin)["tool_input"]["command"]
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

