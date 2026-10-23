#!/usr/bin/env bash
# Native clients are test fixtures; capability and skill delivery use the installed runtime.
set -euo pipefail
root="$1" runtime="$2" project="$3" instance="$4"
[ "${SUBYARD_E2E_VM:-}" = 1 ] && [ -r /run/subyard-e2e-lease.json ] || exit 2
guest() { sudo -n /usr/bin/incus exec "$instance" --project "$project" -- "$@"; }

# Exercise late client installation: provisioned links must already be present.
for skill in subyard-preview subyard-android; do
  for adapter in .agents/skills .claude/skills .config/opencode/skills; do
    guest test -L "/home/dev/$adapter/$skill"
    guest test -r "/home/dev/$adapter/$skill/SKILL.md"
  done
done
for client in codex opencode; do
  guest timeout --kill-after=5 300 bash -s < "$runtime/config/agents/$client/provision.sh"
done

# Reuse the checksum-pinned native Claude fixture version from OpenLIT acceptance.
# This installs only in this disposable yard and uses no account or provider key.
guest timeout --kill-after=5 240 bash -se <<'CLAUDE'
set -euo pipefail
case "$(dpkg --print-architecture)" in
  amd64) platform=linux-x64; checksum=3920489a5109cff5786a1a392c25277408ff22bc796d5edb9c16a60e5a1718f0 ;;
  arm64) platform=linux-arm64; checksum=e4daf793d1e74fb0d9874dd09e98690bbfd7be515f78a87fd05b9e2b4bb33b03 ;;
  *) exit 2 ;;
esac
test ! -e /usr/local/bin/claude
stage=$(mktemp -d /tmp/subyard-claude-probe.XXXXXX)
trap 'rm -f -- "$stage/claude.tgz" "$stage/claude"; rmdir "$stage"' EXIT
curl --proto '=https' --proto-redir '=https' --tlsv1.2 --fail --silent --show-error --location \
  --connect-timeout 10 --max-time 180 \
  "https://registry.npmjs.org/@anthropic-ai/claude-code-$platform/-/claude-code-$platform-2.1.287.tgz" -o "$stage/claude.tgz"
python3 - "$stage/claude.tgz" "$stage/claude" <<'PY'
import pathlib, sys, tarfile
with tarfile.open(sys.argv[1], 'r:gz') as archive:
    members = [member for member in archive if member.name == 'package/claude' and member.isfile()]
    assert len(members) == 1
    pathlib.Path(sys.argv[2]).write_bytes(archive.extractfile(members[0]).read())
PY
printf '%s  %s\n' "$checksum" "$stage/claude" | sha256sum --check --status
install -m 0755 "$stage/claude" /usr/local/bin/claude
CLAUDE

app="$(guest runuser -u dev -- mktemp -d /tmp/subyard-skills-app.XXXXXX)"
guest runuser -u dev -- git init --quiet "$app"
readarray -t metadata < <(python3 - "$runtime" <<'PY'
from pathlib import Path
import sys
root = Path(sys.argv[1])
for source in (root/'config/skills/core/subyard-preview/SKILL.md',
               root/'config/profiles/android/skills/subyard-android/SKILL.md'):
    fields = dict(line.split(': ', 1) for line in source.read_text().split('---', 2)[1].splitlines() if ': ' in line)
    print(fields['name'] + '=' + fields['description'])
PY
)
[ "${#metadata[@]}" = 2 ]
guest runuser -u dev -- python3 - --cwd "$app" \
  --skill "${metadata[0]}" --skill "${metadata[1]}" < "$root/dev/e2e/agent-skills-discovery.py"
printf 'android_skill_discovery=PASS checkout_dependency=absent late_clients=PASS\n'
