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


# Native requirements apply across projects, CODEX_HOME and client launch modes.
# Keep this in the self-contained provision hook copied into the yard.
requirements() {
  cat <<'REQUIREMENTS'
# Managed by Subyard Codex provision.
allowed_approval_policies = ["on-request"]
allowed_approvals_reviewers = ["user"]
check_for_update_on_startup = false
REQUIREMENTS
  [ "$GIT_APPROVAL" = prompt ] || return 0
  cat <<'REQUIREMENTS'

[rules]
prefix_rules = [
  { pattern = [{ token = "git" }, { any_of = ["commit", "push"] }], decision = "prompt", justification = "Creating or amending commits and pushing require explicit operator approval." },
]
REQUIREMENTS
  cat <<REQUIREMENTS

[features]
hooks = true

[hooks]
managed_dir = $(jq -n --arg path "$requirements_dir" '$path')

[[hooks.PreToolUse]]
matcher = "Bash"

[[hooks.PreToolUse.hooks]]
type = "command"
command = $(jq -n --arg path "$git_hook" '$path | @sh')
timeout = 3
REQUIREMENTS
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
if [ -L "$REQUIREMENTS_PATH" ] || { [ -e "$REQUIREMENTS_PATH" ] && [ ! -f "$REQUIREMENTS_PATH" ]; }; then
  refuse "$REQUIREMENTS_PATH is not a managed regular file — leaving it unchanged"
elif [ -f "$REQUIREMENTS_PATH" ] && ! grep -Fxq "$MANAGED_CHECK_MARKER" "$REQUIREMENTS_PATH"; then
  refuse "$REQUIREMENTS_PATH is managed by another policy — leaving it unchanged"
fi
git_hook="$requirements_dir/subyard-git-approval-hook"
if [ "$GIT_APPROVAL" = prompt ]; then
  [ -x /usr/bin/python3 ] || refuse '/usr/bin/python3 is required for Git approval'
  if [ -L "$git_hook" ] || { [ -e "$git_hook" ] && [ ! -f "$git_hook" ]; }; then
    refuse 'managed Git approval hook is not a regular file'
  elif [ -f "$git_hook" ] && ! grep -Fxq '# Managed by Subyard Git approval provision.' "$git_hook"; then
    refuse 'existing Git approval hook belongs to another policy'
  fi
fi
[ "$refusals" = 0 ] || exit 1

install_requirements() {
  local requirements_stage
  install -d -m 0755 "$requirements_dir"
  requirements_stage="$(mktemp "$requirements_dir/.requirements.XXXXXX")"
  requirements > "$requirements_stage"
  chmod 0644 "$requirements_stage"
  if [ "$(id -u)" -eq 0 ]; then
    chown 0:0 "$requirements_dir" "$requirements_stage"
  fi
  install_git_approval_hook
  mv -fT -- "$requirements_stage" "$REQUIREMENTS_PATH"
}

install_check() {
  local q_hook hook_sha check_dir check_stage q_root q_link q_user q_home q_owner q_requirements requirements_sha
  check_dir="$(dirname "$CHECK_PATH")"
  mkdir -p "$check_dir"
  check_stage="$(mktemp "$check_dir/.codex-policy-check.XXXXXX")"
  printf -v q_root '%q' "$INSTALL_ROOT"
  printf -v q_link '%q' "$BIN_LINK"
  printf -v q_user '%q' "$DEV_USER"
  printf -v q_home '%q' "$DEV_HOME"
  printf -v q_owner '%q' "$expected_owner"
  printf -v q_requirements '%q' "$REQUIREMENTS_PATH"
  printf -v q_hook '%q' "$git_hook"
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
git_approval=$GIT_APPROVAL
git_hook=$q_hook
git_hook_sha=$hook_sha
# Report every refusal that does not depend on a failed read, in this order.
refusals=0
refuse() { printf 'codex-policy-check: %s\\n' "\$*" >&2; refusals=\$((refusals + 1)); }
$(declare -f check_git_approval_hook dev_owned_directory standalone_shadow_state is_release_target)
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
  [ "\$(stat -c '%a:%u:%g' "\$requirements")" = "644:\$expected_owner" ] \
    || refuse "managed approval requirements permissions drifted"
  [ "\$(sha256sum "\$requirements" | cut -d' ' -f1)" = '$requirements_sha' ] \
    || refuse "managed approval requirements drifted; run yard init"
fi
[ "\$git_approval" != prompt ] || check_git_approval_hook "\$git_hook" "\$git_hook_sha" "\$expected_owner"
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
  run_as_dev bwrap --version >/dev/null 2>&1 \
    || refuse "bubblewrap sandbox runtime is unavailable; run yard init"
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
