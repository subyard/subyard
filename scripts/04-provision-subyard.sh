#!/usr/bin/env bash
# 04-provision-subyard.sh — Phase 3: provision the yard via `incus exec` (core pkgs,
# Docker Stage 1, user 'dev', /srv layout, ssh/docker) + host-side kvm-gid fix. Idempotent.
# Core provisioning + agent bootstrap hooks; project toolchains stay in Phase 4.
# Config: config/incus.project.env + config/subyard.env + config/host.env + config/agents.env.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/runtime.sh
. "$SCRIPT_DIR/lib/runtime.sh"
# shellcheck source=scripts/lib/engine-context.sh
. "$SCRIPT_DIR/lib/engine-context.sh"
subyard_require_engine_context
# shellcheck source=scripts/lib/ui.sh
. "$SCRIPT_DIR/lib/ui.sh"
# shellcheck source=scripts/lib/host.sh
. "$SCRIPT_DIR/lib/host.sh"

INCUS_PROJECT="${INCUS_PROJECT:-subyard}"
YARD_INSTANCE_NAME="${YARD_INSTANCE_NAME:-yard}"
DEV_USER="${DEV_USER:-dev}"
DEV_UID="${DEV_UID:-1000}"

PROJ=(--project "$INCUS_PROJECT")

# --- preconditions -----------------------------------------------------------
# incus_preflight distinguishes incus-absent / stale-group-session / unreachable, so a
# shell that predates the incus-admin group gets the right hint, not a false "missing".
incus_preflight "init"
incus info "$YARD_INSTANCE_NAME" "${PROJ[@]}" >/dev/null 2>&1 \
  || die "instance '$YARD_INSTANCE_NAME' missing — run scripts/03-create-subyard.sh first"

announce_confirm "Subyard Phase 3 — provision the yard ($YARD_INSTANCE_NAME)" \
  "Inside the yard: apt-get install core packages (ssh, git, build tools, python…; Node is per-profile)." \
  "Inside the yard: install Docker Engine + Compose via the get.docker.com script (downloads & runs it)." \
  "Inside the yard: create user '$DEV_USER' + groups (yard/kvm/docker), lay out /srv, enable ssh & docker." \
  "Inside the yard: install the pinned native ccusage reporter." \
  "Inside the yard: bootstrap enabled agent CLIs when missing." \
  "On the host: set the /dev/kvm device GID to the in-yard 'kvm' group." \
  "On the host: copy instructions for enabled agents into the yard, if present." \
  "This pulls packages from the network and changes the yard's userspace (not the host system)."

# RUNNING precedes guest-agent readiness for VMs. Wait before the first mutating exec.
info "waiting for $YARD_INSTANCE_NAME agent"
incus_wait_instance_agent "$INCUS_PROJECT" "$YARD_INSTANCE_NAME" \
  || die "instance '$YARD_INSTANCE_NAME' agent did not become ready"

incus config set "$YARD_INSTANCE_NAME" user.subyard.ccusage_version pending "${PROJ[@]}" \
  || die "could not invalidate ccusage convergence"
_codex_selected=0
for _agent in ${CODING_TOOL_INTEGRATIONS:-}; do
  [ "$_agent" != codex ] || _codex_selected=1
done
if [ "$_codex_selected" = 1 ]; then
  incus config set "$YARD_INSTANCE_NAME" user.subyard.codex_version pending "${PROJ[@]}" \
    || die "could not invalidate Codex convergence"
fi

# --- 1. provision inside the yard --------------------------------------------
# Quoted heredoc: nothing expands on the host; vars arrive via --env.
info "provisioning inside $YARD_INSTANCE_NAME (packages, Docker, user, /srv, services)"
incus exec "$YARD_INSTANCE_NAME" "${PROJ[@]}" --env DEV_USER="$DEV_USER" --env DEV_UID="$DEV_UID" --env DEV_SUDO="${DEV_SUDO:-0}" --env CODING_TOOL_INTEGRATIONS="${CODING_TOOL_INTEGRATIONS:-}" --env HOST_LINKS="${HOST_LINKS:-}" -- bash -euo pipefail -s <<'EOS'
export DEBIAN_FRONTEND=noninteractive
# The yard bridge is IPv4-only (ipv6.address=none), so steer apt to IPv4 — mirrors that
# resolve to AAAA records would otherwise be tried over an unreachable IPv6 path first.
printf 'Acquire::ForceIPv4 "true";\n' > /etc/apt/apt.conf.d/99force-ipv4
apt-get update -qq

# Core packages; project toolchains come from profiles.
apt-get install -y -qq \
  ca-certificates curl gnupg lsb-release sudo \
  openssh-server git git-lfs jq ripgrep rsync make build-essential zip unzip uidmap \
  python3 python3-venv pipx
git lfs install --system >/dev/null 2>&1 || true

# yq (mikefarah single binary; not reliably packaged).
if ! command -v yq >/dev/null 2>&1; then
  arch="$(dpkg --print-architecture)"
  curl -fsSL "https://github.com/mikefarah/yq/releases/latest/download/yq_linux_${arch}" \
    -o /usr/local/bin/yq && chmod +x /usr/local/bin/yq
fi

# A yard may itself host a nested Incus bridge. Keep Docker from changing the yard's global
# FORWARD policy to DROP; Docker's own bridge isolation rules remain in place. Write this before
# first install/start so nested networking is also correct on fresh yards and after reboot.
docker_forwarding_converge() {
  local config_changed="${1:?docker_forwarding_converge needs config state}"
  [ "$config_changed" != 1 ] || systemctl restart docker
  command -v iptables >/dev/null 2>&1 \
    && iptables -S FORWARD | grep -qx -- '-P FORWARD DROP' \
    && iptables -P FORWARD ACCEPT
  return 0
}

install -d -m 0755 /etc/docker
docker_config=/etc/docker/daemon.json
docker_config_changed=0
if [ -e "$docker_config" ]; then
  jq -e 'type == "object"' "$docker_config" >/dev/null \
    || { echo "$docker_config must contain a JSON object" >&2; exit 1; }
  if ! jq -e '."ip-forward-no-drop" == true' "$docker_config" >/dev/null; then
    docker_config_tmp="$(mktemp /etc/docker/.daemon.json.XXXXXX)"
    jq '. + {"ip-forward-no-drop": true}' "$docker_config" > "$docker_config_tmp"
    install -m 0644 "$docker_config_tmp" "$docker_config"
    rm -f "$docker_config_tmp"
    docker_config_changed=1
  fi
else
  docker_config_tmp="$(mktemp /etc/docker/.daemon.json.XXXXXX)"
  printf '%s\n' '{"ip-forward-no-drop":true}' > "$docker_config_tmp"
  install -m 0644 "$docker_config_tmp" "$docker_config"
  rm -f "$docker_config_tmp"
  docker_config_changed=1
fi

# Docker Engine + Compose plugin (official convenience script; Debian/Ubuntu aware).
if ! command -v docker >/dev/null 2>&1; then
  curl -fsSL https://get.docker.com | sh
else
  docker_forwarding_converge "$docker_config_changed"
fi

# Groups + unprivileged dev user (Stage 1: dev is in docker group; agents are not).
# dev's uid is pinned to DEV_UID so it lines up with the shift-mapped host-mount owner.
groupadd -f yard
groupadd -f kvm
if ! id -u "$DEV_USER" >/dev/null 2>&1; then
  useradd -u "$DEV_UID" -m -s /bin/bash "$DEV_USER" \
    || { echo "uid $DEV_UID is taken in the yard — set a free DEV_UID in config/subyard.env" >&2; exit 1; }
elif [ "$(id -u "$DEV_USER")" != "$DEV_UID" ]; then
  echo "WARNING: $DEV_USER uid $(id -u "$DEV_USER") != DEV_UID $DEV_UID — host mounts won't map to $DEV_USER" >&2
fi
usermod -aG yard,kvm,docker "$DEV_USER"

# dev sudo (DEV_SUDO; public default 0, enable in private/config.env): passwordless sudo so
# the agent — and you, via `yard shell` — can make root changes in the yard. dev is already
# ~root-in-yard via the docker group and the yard is unprivileged, so this doesn't widen the
# real boundary. Reconciled: DEV_SUDO=0 removes the grant on the next provision.
sudoers="/etc/sudoers.d/90-subyard-$DEV_USER"
if [ "${DEV_SUDO:-0}" = 1 ]; then
  printf '%s ALL=(ALL) NOPASSWD:ALL\n' "$DEV_USER" > "$sudoers.tmp"
  chmod 0440 "$sudoers.tmp"
  visudo -cf "$sudoers.tmp" >/dev/null && mv -f "$sudoers.tmp" "$sudoers" || { rm -f "$sudoers.tmp"; echo "sudoers validation failed — left dev without sudo" >&2; }
else
  rm -f "$sudoers"
fi

# Agent credentials stay in the yard rootfs; only declared session/state paths use HOST_LINKS.
# CLAUDE.md is copied in host-side (section 3), not symlinked.
dev_home="$(getent passwd "$DEV_USER" | cut -d: -f6)"

# Selected credential homes are real rootfs dirs. Drop only their stale legacy symlinks;
# reconciliation never creates or deletes homes for unselected agents.
for agent in ${CODING_TOOL_INTEGRATIONS:-}; do
  case "$agent" in
    claude) d=.claude ;;
    codex) d=.codex ;;
    *) continue ;;
  esac
  p="$dev_home/$d"
  [ -L "$p" ] && rm -f "$p"
  runuser -u "$DEV_USER" -- mkdir -p "$p"
done
unset agent d p

# Symlink dev's session paths to the host-agent-sessions mount (HOST_LINKS, config/host.env).
# Entry "<name>:<target>[:file]"; ":file" makes the target's parent, not the path. Idempotent;
# skips an unattached mount; never clobbers a real directory that already holds data.
if [ -n "${HOST_LINKS:-}" ]; then
  printf '%s\n' "$HOST_LINKS" | sed 's/[[:space:]]//g' | while IFS=: read -r name target kind; do
    [ -n "$name" ] && [ -n "$target" ] || continue
    mroot="/$(printf '%s' "$target" | cut -d/ -f2-4)"
    [ -d "$mroot" ] || { echo "skip $name -> $target (host mount $mroot not attached)" >&2; continue; }
    if [ "$kind" = file ]; then
      runuser -u "$DEV_USER" -- mkdir -p "$(dirname "$target")" 2>/dev/null || true
    else
      runuser -u "$DEV_USER" -- mkdir -p "$target" 2>/dev/null || true
    fi
    link="$dev_home/$name"
    runuser -u "$DEV_USER" -- mkdir -p "$(dirname "$link")" 2>/dev/null || true
    if [ -L "$link" ] || [ ! -e "$link" ]; then
      ln -sfn "$target" "$link"; chown -h "$DEV_USER:$DEV_USER" "$link"
    else
      echo "WARNING: $link exists and is not a symlink — leaving it (move it aside to share)" >&2
    fi
  done
fi

# Detach the exact legacy OpenCode storage symlink; leave real directories untouched.
legacy_opencode_storage="$dev_home/.local/share/opencode/storage"
if [ -L "$legacy_opencode_storage" ] \
  && [ "$(readlink "$legacy_opencode_storage")" = /mnt/host/agent-sessions/opencode/storage ]; then
  rm -f "$legacy_opencode_storage"
fi

# /srv skeleton (generic core; profile caches like android-sdk come in Phase 4).
# Own + group-share ONLY the skeleton dirs this script creates, NON-recursively. A recursive
# chown/chmod over /srv is destructive here: /srv/workspaces/<id>/src holds shift-mapped bind
# projects (real host files — host uid == container uid under shift), and /srv/env-secrets/<id>
# holds 0600 per-project secrets — a `chown -R root:yard` + `chmod -R g+rwX` would rewrite host
# file ownership and make those secrets group-readable/writable to the yard group. So touch only
# the dirs we own and never descend into per-project data (created later by project-* tooling).
srv_skel=(/srv /srv/cache /srv/workspaces /srv/agents /srv/stacks /srv/images /srv/bin)
mkdir -p "${srv_skel[@]}"
chown root:yard "${srv_skel[@]}"
chmod g+rwXs "${srv_skel[@]}"   # g+rwX + setgid (dirs) so new subdirs inherit the yard group

# Services live in the container's own systemd (does not touch host systemd).
systemctl enable --now ssh docker
EOS
ok "in-yard provisioning complete"

# --- 2. provision the core usage reporter -----------------------------------
# Unconditional because usage is a core command rather than an CODING_TOOL_INTEGRATIONS entry.
[ -n "${CCUSAGE_PROVISION:-}" ] || die "ccusage provision hook is not configured"
[ -r "$CCUSAGE_PROVISION" ] || die "ccusage provision hook missing: $CCUSAGE_PROVISION"
info "provisioning native ccusage $CCUSAGE_VERSION in $YARD_INSTANCE_NAME"
incus exec "$YARD_INSTANCE_NAME" "${PROJ[@]}" \
  --env CCUSAGE_VERSION="$CCUSAGE_VERSION" \
  --env CCUSAGE_SHA256_AMD64="$CCUSAGE_SHA256_AMD64" \
  --env CCUSAGE_SHA256_ARM64="$CCUSAGE_SHA256_ARM64" \
  -- bash -euo pipefail -s < "$CCUSAGE_PROVISION" \
  || die "ccusage provisioning failed"
ok "ccusage $CCUSAGE_VERSION ready"

# --- 3. provision enabled agent CLIs ----------------------------------------
# Hooks live under config/agents/<name> and run as root in the yard.
echo "Agent CLIs:"
for _agent in ${CODING_TOOL_INTEGRATIONS:-}; do
  _provision_var="AGENT_${_agent}_PROVISION"
  _provision="${!_provision_var:-}"
  [ -n "$_provision" ] || continue
  [ -r "$_provision" ] || die "$_agent provision hook missing: $_provision"
  info "provisioning $_agent CLI in $YARD_INSTANCE_NAME"
  _agent_env=(
    --env DEV_USER="$DEV_USER"
    --env YARD_VERSION="${YARD_VERSION:-}"
    --env CODEX_VERSION="$CODEX_VERSION"
    --env CODEX_SHA256_AMD64="$CODEX_SHA256_AMD64"
    --env CODEX_SHA256_ARM64="$CODEX_SHA256_ARM64"
  )
  incus exec "$YARD_INSTANCE_NAME" "${PROJ[@]}" "${_agent_env[@]}" \
    -- bash -euo pipefail -s < "$_provision" \
    || die "$_agent CLI provisioning failed"
  _check_var="AGENT_${_agent}_CHECK"
  _check="${!_check_var:-}"
  if [ -n "$_check" ]; then
    case "$_check" in *[!A-Za-z0-9._/-]*|'') die "$_agent check command is invalid" ;; esac
    incus exec "$YARD_INSTANCE_NAME" "${PROJ[@]}" -- timeout 90 "$_check" \
      || die "$_agent package check failed"
  fi
  ok "$_agent CLI ready"
done
unset _agent _agent_env _check_var _check _provision_var _provision

# Install one generic guest-side project lifecycle dispatcher. Enabled agents stay in the
# generated list; opt-in shared resources own executable hooks in projects-changed.d.
_project_hooks=()
for _agent in ${CODING_TOOL_INTEGRATIONS:-}; do
  _hook_var="AGENT_${_agent}_PROJECTS_CHANGED"
  _hook="${!_hook_var:-}"
  [ -n "$_hook" ] || continue
  case "$_hook" in *[!A-Za-z0-9._/-]*|'') die "$_agent project hook command is invalid" ;; esac
  _project_hooks+=("$_hook")
done
incus exec "$YARD_INSTANCE_NAME" "${PROJ[@]}" -- bash -euo pipefail -s <<'EOS'
install -d -m 0755 /etc/subyard /usr/local/libexec/subyard/projects-changed.d
cat > /usr/local/libexec/subyard/projects-changed <<'DISPATCH'
#!/usr/bin/env bash
set -euo pipefail
status=0
for hook in /usr/local/libexec/subyard/projects-changed.d/*; do
  [ -x "$hook" ] || continue
  "$hook" || status=1
done
while IFS= read -r hook; do
  [ -n "$hook" ] || continue
  "$hook" || status=1
done < /etc/subyard/agent-project-hooks
exit "$status"
DISPATCH
chmod 0755 /usr/local/libexec/subyard/projects-changed
chown root:root /usr/local/libexec/subyard/projects-changed
EOS
printf '%s\n' "${_project_hooks[@]}" \
  | incus exec "$YARD_INSTANCE_NAME" "${PROJ[@]}" -- sh -euc '
      temporary=$(mktemp /etc/subyard/.agent-project-hooks.XXXXXX)
      cleanup_project_hooks() {
        rm -f -- "$temporary"
      }
      trap cleanup_project_hooks EXIT HUP INT TERM
      cat >"$temporary"
      chmod 0644 "$temporary"
      chown root:root "$temporary"
      mv -f -- "$temporary" /etc/subyard/agent-project-hooks
      trap - EXIT HUP INT TERM
    ' || die "could not install agent project lifecycle hooks"
unset _agent _hook_var _hook _project_hooks

# --- 4. fix /dev/kvm device GID to the in-yard 'kvm' group --------------------
echo "KVM gid:"
if incus config device list "$YARD_INSTANCE_NAME" "${PROJ[@]}" 2>/dev/null | grep -qx kvm; then
  KVM_GID="$(incus exec "$YARD_INSTANCE_NAME" "${PROJ[@]}" -- getent group kvm | cut -d: -f3)"
  if [ -n "${KVM_GID:-}" ]; then
    # Nested hosts reject the gid property on unix-char devices — degrade with a warn
    # (KVM stays root-owned in the yard; the emulator then needs sudo/acl there).
    if err="$(incus config device set "$YARD_INSTANCE_NAME" kvm gid "$KVM_GID" "${PROJ[@]}" 2>&1)"; then
      ok "set kvm device gid=$KVM_GID (matches in-yard 'kvm' group)"
    elif printf '%s' "$err" | grep -q "nested container"; then
      warn "nested host: kvm device gid not settable — /dev/kvm stays root-owned in the yard"
    else
      printf '%s\n' "$err" >&2
      die "could not set the kvm device gid"
    fi
  else
    warn "could not resolve in-yard 'kvm' GID — skipping gid fix"
  fi
else
  ok "no kvm device attached (vm mode or /dev/kvm absent) — nothing to fix"
fi

# --- summary -----------------------------------------------------------------
incus config set "$YARD_INSTANCE_NAME" user.subyard.ccusage_version "$CCUSAGE_VERSION" "${PROJ[@]}" \
  || die "could not record ccusage convergence"
if [ "$_codex_selected" = 1 ]; then
  incus config set "$YARD_INSTANCE_NAME" user.subyard.codex_version "$CODEX_VERSION" "${PROJ[@]}" \
    || die "could not record Codex convergence"
fi
unset _codex_selected
echo
ok "Phase 3 done."
cat <<MSG

Verify:
  incus exec $YARD_INSTANCE_NAME "${PROJ[@]}" -- systemctl --no-pager status ssh docker
  incus exec $YARD_INSTANCE_NAME "${PROJ[@]}" -- docker compose version
  incus exec $YARD_INSTANCE_NAME "${PROJ[@]}" -- id $DEV_USER          # groups: yard kvm docker
  incus exec $YARD_INSTANCE_NAME "${PROJ[@]}" -- /usr/local/bin/ccusage --version
  incus exec $YARD_INSTANCE_NAME "${PROJ[@]}" -- su - $DEV_USER -c '/usr/local/bin/ccusage --version'
  incus exec $YARD_INSTANCE_NAME "${PROJ[@]}" -- opencode --version
  incus exec $YARD_INSTANCE_NAME "${PROJ[@]}" -- ls -la /srv

Next:
  - Phase 4: dependency profile (e.g. android) — caches into /srv/cache (per profile).
  - Phase 7: VS Code / SSH proxy ports.
MSG
