#!/usr/bin/env bash
# Static contract for the real-VM Hermes substrate lane.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PROFILE="$ROOT/config/profiles/hermes"
E2E="$ROOT/dev/e2e/hermes-profile.sh"
PRESET="$PROFILE/yard.env"
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

for setting in \
  'ENVIRONMENT_PROFILES=hermes' \
  'CODING_TOOL_INTEGRATIONS=' \
  'HOST_CLAUDE_MD=' \
  'HOST_CODEX_AGENTS_MD=' \
  'HOST_OPENCODE_AGENTS_MD=' \
  'HOST_MOUNTS=' \
  'HOST_LINKS=' \
  'YARD_CAPABILITIES=' \
  'YARD_CAPS=' \
  'YARD_DEVICES=' \
  'YARD_MOUNTS=' \
  'FORWARD_SSH_AGENT=0' \
  'DEV_SUDO=0' \
  'NESTED_E2E_VMS=0'; do
  grep -Fxq "$setting" "$PRESET" || fail "Hermes preset omits $setting"
done

for required in \
  'yard "$YARD" init --profile hermes --yes' \
  'cmp "$ROOT/config/profiles/hermes/yard.env" "$definition"' \
  'yard "$YARD" provision --yes' \
  'dev_uid="$(incus exec' \
  '--user "$dev_uid" --group "$dev_gid"' \
  '$HOME/.hermes' \
  '$HOME/.local/bin/hermes' \
  'operator-opaque/state.bin' \
  'tar --sort=name --numeric-owner --one-file-system' \
  'repeat provision changed the installation or opaque state' \
  'yard "$YARD" stop --yes' \
  'yard "$YARD" start --yes' \
  'incus restart "$instance"' \
  'setting "$YARD" NESTED_E2E_VMS' \
  'security.nesting: "true"' \
  'restricted.containers.interception: block' \
  'security.syscalls.intercept.bpf' \
  'e2e-vhost-vsock' \
  'security --require-live --quiet' \
  '! command -v tailscale' \
  'test ! -S /run/host-services/ssh-auth.sock' \
  'python3 -m http.server 9119 --bind 127.0.0.1' \
  'config set HERMES_DASHBOARD_ADVERTISE_HOST' \
  'config set HERMES_DASHBOARD_HOST_PORT' \
  'dashboard up --yes' \
  'injected owner-endpoint readiness failure was accepted' \
  'readiness failure left the owned browser proxy attached' \
  'readiness failure left owner-side route metadata' \
  'listen: tcp:$TAILSCALE_IP:$dashboard_port' \
  'connect: tcp:127.0.0.1:9119' \
  'bind: host' \
  'replacement_dashboard_port' \
  'owned browser proxy was not replaced after its port setting changed' \
  'config unset HERMES_DASHBOARD_ADVERTISE_HOST' \
  'config unset HERMES_DASHBOARD_HOST_PORT' \
  'user.subyard.resource.hermes-dashboard' \
  'http://$guest_ip:9119/' \
  'dashboard down --yes' \
  'definition_before_teardown="$(sha256sum "$definition")"' \
  'state_dir="$SUBYARD_CONFIG_HOME/yards/$YARD/projects"' \
  'Hermes project state survived teardown' \
  'yard "$YARD" teardown --yes'; do
  grep -Fq -- "$required" "$E2E" || fail "real lane omits: $required"
done

if grep -Eq -- '--user 1000|--group 1000|1000:1000' "$E2E"; then
  fail 'real lane hard-codes the guest primary identity'
fi
if grep -Fq 'Hermes yard enables nesting' "$E2E"; then
  fail 'real lane confuses core Docker container nesting with the nested-VM backend'
fi
if grep -Fq 'yard definition survived teardown' "$E2E"; then
  fail 'real lane expects teardown to remove the reusable named-yard definition'
fi

grep -Fq "'ENVIRONMENT_PROFILES=openclaw'" "$E2E" \
  || fail 'real lane does not seed a hostile host profile selection'
grep -Fq "'CODING_TOOL_INTEGRATIONS=claude'" "$E2E" \
  || fail 'real lane does not seed a hostile host agent selection'
grep -Fq 'HOST_MOUNTS=host-fixture:/mnt/host-fixture:ro:0755' "$E2E" \
  || fail 'real lane does not seed a hostile host mount'
if grep -Eq 'hermes (doctor|setup|model|gateway|cron|update)|faster[_-]whisper|telegram|config\.yaml|/srv/hermes|HERMES_DASHBOARD_BASIC|HERMES_PROVIDER' \
  "$E2E"; then
  fail 'real lane validates or configures Hermes application behavior'
fi
if grep -Eq 'write_yard_config|cat >.*yards/.*/config\.env' "$E2E"; then
  fail 'Hermes E2E writes a named-yard definition directly'
fi
if grep -Eq '^export (ENVIRONMENT_PROFILES|CODING_TOOL_INTEGRATIONS|DEV_SUDO|FORWARD_SSH_AGENT|NESTED_E2E_VMS)=' \
  "$E2E"; then
  fail 'Hermes E2E masks the shipped preset with command environment values'
fi

for forbidden in \
  backup-to-restic.sh hermes-backup-create hermes-backup-finalize hermes-provider-ready \
  hermes-release-update hermes-restore hermes-runtime-check hermes-serve.service \
  hermes-dashboard.service hermes-dashboard-ingress; do
  [ ! -e "$PROFILE/$forbidden" ] || fail "application-aware profile helper remains: $forbidden"
done

printf 'ok: Hermes VM lane stays inside the substrate boundary\n'
