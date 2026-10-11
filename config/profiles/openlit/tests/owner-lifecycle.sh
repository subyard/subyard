#!/usr/bin/env bash
# Verify bounded owner transport and refusal before guest effects.
set -euo pipefail
export TMPDIR=/tmp
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
state="$(mktemp -d /tmp/subyard-openlit-owner.XXXXXX)"
trap 'rm -rf -- "$state"' EXIT
mkdir "$state/bin"
cat > "$state/bin/incus" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
if [ "$1" = list ]; then printf '%s\n' "${OPENLIT_OWNER_FIXTURE_STATE:-RUNNING}"; exit; fi
[ "$1" = exec ] || exit 1
# Exactly these profile-owned sources and the shared TOML writer are transported.
tar -tf - | sort > "$OPENLIT_OWNER_FIXTURE_FILES"
printf '%s\n' "${@: -4}" >> "$OPENLIT_OWNER_FIXTURE_CALLS"
[ "${@: -4:1}" != pause ] || printf 'paused\n'
MOCK
chmod 755 "$state/bin/incus"
export PATH="$state/bin:$PATH"
export OPENLIT_OWNER_FIXTURE_FILES="$state/files" OPENLIT_OWNER_FIXTURE_CALLS="$state/calls"
export SUBYARD_ENGINE_CONTEXT=1 SUBYARD_ENGINE_CONTEXT_SCHEMA=1
export SUBYARD_OPERATOR_HOME="$state" SUBYARD_CONFIG_DIR="$root/config" SUBYARD_CONFIG_HOME="$state/config" SUBYARD_HOME="$state/data"
export STORAGE_PATH="$state/storage" HOST_BASE="$state/host" RESTRICTED_DISK_PATHS="$state/host"
export ACCESS_KIND=local YARD_KIND=container YARD_INSTANCE_NAME=yard-fixture INCUS_PROJECT=subyard-fixture INCUS_BRIDGE=fixture SSH_HOST=127.0.0.1
export DEV_USER=dev DEV_UID=1000 DEV_SUDO=0 FORWARD_SSH_AGENT=0 NESTED_E2E_VMS=0
export CODING_TOOL_INTEGRATIONS='claude codex'
export AGENT_claude_CONFIG="$root/config/agents/claude/settings.json" AGENT_claude_CONFIG_DEST=.claude/settings.json
export AGENT_codex_CONFIG="$root/config/agents/codex/config.toml" AGENT_codex_CONFIG_DEST=.codex/config.toml
export SUBYARD_PROFILE_SELECTED=0
bash "$root/config/profiles/openlit/owner.sh" --check
printf 'check\n0\ndev\nclaude codex\n' > "$state/expected"
cmp "$state/expected" "$state/calls"
export SUBYARD_PROFILE_SELECTED=1
: > "$state/calls"
bash "$root/config/profiles/openlit/owner.sh" --yes
printf 'apply\n1\ndev\nclaude codex\n' > "$state/expected"
cmp "$state/expected" "$state/calls"
printf 'collector.yaml\ncompose.yaml\nguest.sh\nnative.py\nruntime.py\nsubyard-openlit.service\ntomli_w.py\n' > "$state/expected"
cmp "$state/expected" "$state/files"
: > "$state/calls"
bash "$root/config/profiles/openlit/owner.sh" --remove
printf 'apply\n0\ndev\nclaude codex\n' > "$state/expected"
cmp "$state/expected" "$state/calls"
[ "$(bash "$root/config/profiles/openlit/owner.sh" --pause)" = paused ]
: > "$state/calls"
OPENLIT_OWNER_FIXTURE_STATE=STOPPED bash "$root/config/profiles/openlit/owner.sh" --yes
[ ! -s "$state/calls" ]
printf '[otel]\nexporter="none"\n' > "$state/conflict.toml"
if AGENT_codex_CONFIG="$state/conflict.toml" bash "$root/config/profiles/openlit/owner.sh" --yes >/dev/null 2>&1; then
  echo 'FAIL: template conflict accepted' >&2; exit 1
fi
[ ! -s "$state/calls" ]
printf 'ok: OpenLIT lifecycle transport, stopped preservation and template-conflict refusal\n'
