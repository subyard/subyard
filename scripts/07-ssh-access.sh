#!/usr/bin/env bash
# 07-ssh-access.sh — give the operator SSH into the yard for VS Code and data transfer
# Remote-SSH (`yard code`) work. Three idempotent steps:
#   1. a loopback-only host endpoint at 127.0.0.1:$SSH_PORT -> yard:22,
#   2. the operator's public key in the yard user's authorized_keys,
#   3. a 'Host $SSH_HOST' entry in ~/.ssh (via an Include — your config is not clobbered).
# Every local yard on this owner host uses the same Subyard-managed transport identity
# under $SUBYARD_HOME/ssh. Personal keys and ssh-agent state are intentionally ignored.
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
# shellcheck source=scripts/lib/ssh-config.sh
. "$SCRIPT_DIR/lib/ssh-config.sh"

INCUS_PROJECT="${INCUS_PROJECT:-subyard}"
YARD_INSTANCE_NAME="${YARD_INSTANCE_NAME:-yard}"
DEV_USER="${DEV_USER:-dev}"
SSH_HOST="${SSH_HOST:-yard}"
SSH_PORT="${SSH_PORT:-2222}"
# Opt-in (default off): forward the host ssh-agent so in-yard git over SSH uses your
# host keys without copying any private key into the yard.
FORWARD_SSH_AGENT="${FORWARD_SSH_AGENT:-0}"
PROJ=(--project "$INCUS_PROJECT")
device_exists() { incus config device list "$YARD_INSTANCE_NAME" "${PROJ[@]}" 2>/dev/null | grep -qx "$1"; }
dev_get() { incus config device get "$YARD_INSTANCE_NAME" "$1" "$2" "${PROJ[@]}" 2>/dev/null || true; }

# --- preconditions -----------------------------------------------------------
incus_preflight
incus info "$YARD_INSTANCE_NAME" "${PROJ[@]}" >/dev/null 2>&1 \
  || die "instance '$YARD_INSTANCE_NAME' missing — run '$(yard_cmd_hint) init' first"
[ "$(incus list "$YARD_INSTANCE_NAME" "${PROJ[@]}" -f csv -c s 2>/dev/null)" = RUNNING ] \
  || die "yard is not running — start it: $(yard_cmd_hint) start"

fwd_note=()
[ "$FORWARD_SSH_AGENT" = 1 ] && fwd_note=("Enable ssh-agent forwarding for '$SSH_HOST' (no private key enters the yard).")
boundary_note=()
[ "${NESTED_E2E_VMS:-0}" != 1 ] || boundary_note=(
  "Replace '$DEV_USER' authorized_keys with this operator key restricted to the L0 loopback proxy."
)
announce "yard SSH access ($SSH_HOST)" \
  "Add a host loopback endpoint: 127.0.0.1:$SSH_PORT -> yard:22." \
  "Authorize the host-scoped Subyard transport key for '$DEV_USER' in the yard." \
  "Pin the yard host key through Incus and add a strict 'Host $SSH_HOST' entry via an Include." \
  ${boundary_note[@]+"${boundary_note[@]}"} \
  ${fwd_note[@]+"${fwd_note[@]}"}
proceed_or_die

# --- 1. prepare the owner-host transport identity ----------------------------
IDENTITY="$(ssh_transport_identity_prepare "$SUBYARD_HOME")" \
  || die "could not prepare the host-scoped SSH transport identity"
PUBKEY_FILE="$IDENTITY.pub"
PUBKEY="$(awk 'NF >= 2 { print $1 " " $2; exit }' "$PUBKEY_FILE")"
ok "host-scoped SSH transport identity ready"

# --- 2. loopback transport (idempotent) --------------------------------------
echo "SSH proxy:"
proxy_connect=tcp:127.0.0.1:22
restore_previous_vm_transport() { return 0; }
if [ "${YARD_KIND:-container}" = vm ]; then
  # Incus VM proxy devices require kernel NAT. Host-originated loopback traffic is not
  # a valid DNAT path, so VMs use a root-owned userspace relay bound only to loopback.
  vm_ipv4="$(incus_instance_primary_ipv4 "$INCUS_PROJECT" "$YARD_INSTANCE_NAME")"
  [ -n "$vm_ipv4" ] || die "VM '$YARD_INSTANCE_NAME' has no IPv4 address for its SSH proxy"
  if device_exists eth0; then
    incus config device set "$YARD_INSTANCE_NAME" eth0 ipv4.address="$vm_ipv4" "${PROJ[@]}"
  else
    incus config device override "$YARD_INSTANCE_NAME" eth0 ipv4.address="$vm_ipv4" "${PROJ[@]}"
  fi
  previous_relay_target=""
  relay_service="/etc/systemd/system/subyard-ssh-relay-$SSH_PORT.service"
  if [ -f "$relay_service" ] && [ ! -L "$relay_service" ] \
    && [ "$(stat -c %u "$relay_service" 2>/dev/null)" = 0 ] \
    && [ "$(stat -c %a "$relay_service" 2>/dev/null)" = 644 ]; then
    previous_relay_exec="$(sed -n 's/^ExecStart=//p' "$relay_service")"
    case "$previous_relay_exec" in
      /usr/lib/systemd/systemd-socket-proxyd\ *:22 | /lib/systemd/systemd-socket-proxyd\ *:22)
        previous_relay_target="${previous_relay_exec##* }"
        previous_relay_target="${previous_relay_target%:22}"
        ;;
    esac
  fi
  previous_proxy=0
  previous_proxy_listen=""
  previous_proxy_connect=""
  previous_proxy_bind=""
  previous_proxy_nat=""
  if device_exists ssh && [ "$(dev_get ssh type)" = proxy ]; then
    previous_proxy=1
    previous_proxy_listen="$(dev_get ssh listen)"
    previous_proxy_connect="$(dev_get ssh connect)"
    previous_proxy_bind="$(dev_get ssh bind)"
    previous_proxy_nat="$(dev_get ssh nat)"
    incus config device remove "$YARD_INSTANCE_NAME" ssh "${PROJ[@]}" >/dev/null
  fi
  "$SCRIPT_DIR/install-ssh-relay.sh" --ensure "$SSH_PORT" "$vm_ipv4"
  restore_previous_vm_transport() {
    local -a old_proxy_args=()
    if [ -n "$previous_relay_target" ]; then
      "$SCRIPT_DIR/install-ssh-relay.sh" --ensure "$SSH_PORT" "$previous_relay_target"
    else
      "$SCRIPT_DIR/install-ssh-relay.sh" --remove "$SSH_PORT"
    fi
    if [ "$previous_proxy" = 1 ] && ! device_exists ssh; then
      [ -z "$previous_proxy_bind" ] || old_proxy_args+=("bind=$previous_proxy_bind")
      [ -z "$previous_proxy_nat" ] || old_proxy_args+=("nat=$previous_proxy_nat")
      incus config device add "$YARD_INSTANCE_NAME" ssh proxy "${PROJ[@]}" \
        "listen=$previous_proxy_listen" "connect=$previous_proxy_connect" \
        "${old_proxy_args[@]}" >/dev/null
    fi
  }
  ok "added loopback relay 127.0.0.1:$SSH_PORT -> $vm_ipv4:22"
else
  proxy_args=(bind=host)
  if device_exists ssh; then
    if [ "$(dev_get ssh type)" = proxy ] \
      && [ "$(dev_get ssh listen)" = "tcp:127.0.0.1:$SSH_PORT" ] \
      && [ "$(dev_get ssh connect)" = "$proxy_connect" ] \
      && { [ "${YARD_KIND:-container}" != vm ] || [ "$(dev_get ssh nat)" = true ]; }; then
      ok "proxy device 'ssh' already attached"
    else
      warn "proxy device 'ssh' drifted — re-attaching on 127.0.0.1:$SSH_PORT"
      incus config device remove "$YARD_INSTANCE_NAME" ssh "${PROJ[@]}" >/dev/null
    fi
  fi
  if ! device_exists ssh; then
    incus config device add "$YARD_INSTANCE_NAME" ssh proxy "${PROJ[@]}" \
      listen="tcp:127.0.0.1:$SSH_PORT" connect="$proxy_connect" "${proxy_args[@]}" >/dev/null
    ok "added proxy 127.0.0.1:$SSH_PORT -> yard:22"
  fi
fi

# --- 3. authorize the key for dev in the yard (idempotent) -------------------
echo "Authorized key:"
incus exec "$YARD_INSTANCE_NAME" "${PROJ[@]}" --env PUBKEY="$PUBKEY" --env DEV_USER="$DEV_USER" \
  --env RESTRICT_TO_PROXY="${NESTED_E2E_VMS:-0}" -- sh -eu -c '
  home="$(getent passwd "$DEV_USER" | cut -d: -f6)"
  install -d -m 700 -o "$DEV_USER" -g "$DEV_USER" "$home/.ssh"
  ak="$home/.ssh/authorized_keys"
  candidate="$PUBKEY"
  [ "$RESTRICT_TO_PROXY" != 1 ] \
    || candidate="from=\"127.0.0.1,::1\" $PUBKEY"
  touch "$ak"
  grep -qxF "$candidate" "$ak" || printf "%s\n" "$candidate" >> "$ak"
  chmod 600 "$ak"; chown "$DEV_USER":"$DEV_USER" "$ak"
' || die "could not authorize the key in the yard"
ok "$DEV_USER@$SSH_HOST authorized for your key"

# --- 4. ~/.ssh Host entry via an Include (does not rewrite your config) -------
echo "SSH client config:"
sshdir="$HOME/.ssh"; install -d -m 700 "$sshdir"
# Per-yard snippet: the default yard keeps ~/.ssh/subyard.config (byte-identical); a named
# yard gets its own ~/.ssh/subyard-<name>.config so several yards' Host blocks never collide
# and teardown of one removes only that file. Its Include line is added per file (below).
snip_name="$(ssh_yard_snippet_name "${YARD_NAME:-}")"
snip="$sshdir/$snip_name"
snip_name="$(basename "$snip")"
# Shared across yards, but host-key entries are keyed by [127.0.0.1]:<port> and each yard
# has a unique port, so entries never collide — one known_hosts is correct and intended.
known="$SUBYARD_HOME/ssh/known_hosts"
install -d -m 700 "$SUBYARD_HOME/ssh"
yard_host_key="$(incus exec "$YARD_INSTANCE_NAME" "${PROJ[@]}" -- \
  awk '$1 == "ssh-ed25519" && NF >= 2 { print $1 " " $2; exit }' \
  /etc/ssh/ssh_host_ed25519_key.pub)" \
  || die "could not read the yard's SSH host key through Incus"
proof_known="$(mktemp "$SUBYARD_HOME/ssh/.known-hosts-proof.XXXXXX")" \
  || die "could not stage the SSH host-key proof"
if [ -f "$known" ]; then
  cp -- "$known" "$proof_known" \
    || { rm -f -- "$proof_known"; die "could not stage existing SSH host-key pins"; }
fi
chmod 0600 "$proof_known"
ssh_known_host_replace "$proof_known" "[127.0.0.1]:$SSH_PORT" "$yard_host_key" \
  || { rm -f -- "$proof_known"; die "could not stage the yard's SSH host key"; }

# Prove the new credential through the loopback transport before switching an existing
# snippet. An empty agent socket plus IdentitiesOnly makes this independent of personal keys.
login_verified=0
probe_dedicated_login() {
  SSH_AUTH_SOCK='' ssh -F /dev/null \
    -o BatchMode=yes -o IdentitiesOnly=yes -o ConnectTimeout=5 \
    -o StrictHostKeyChecking=yes -o UserKnownHostsFile="$proof_known" \
    -o GlobalKnownHostsFile=/dev/null -o IdentityFile="$IDENTITY" \
    -p "$SSH_PORT" "$DEV_USER@127.0.0.1" true
}
for attempt in {1..12}; do
  probe_status=1
  if [ "$attempt" -lt 12 ]; then
    probe_dedicated_login 2>/dev/null && probe_status=0
  else
    probe_dedicated_login && probe_status=0
  fi
  if [ "$probe_status" = 0 ]; then
    login_verified=1
    break
  fi
  [ "$attempt" -eq 12 ] || sleep 5
done
[ "$login_verified" = 1 ] \
  || {
    rm -f -- "$proof_known"
    restore_previous_vm_transport \
      || die "dedicated SSH login failed and the previous VM transport could not be restored"
    die "dedicated SSH identity could not log in; existing client state was preserved"
  }
ssh_known_host_replace "$known" "[127.0.0.1]:$SSH_PORT" "$yard_host_key" \
  || {
    rm -f -- "$proof_known"
    restore_previous_vm_transport \
      || die "host-key publication failed and the previous VM transport could not be restored"
    die "could not publish the yard's SSH host key; existing client state was preserved"
  }
rm -f -- "$proof_known"
ok "dedicated SSH login verified without ssh-agent"

# Opt-in agent forwarding: lets in-yard `git pull/push` over SSH use the host keys
# held by your ssh-agent, without any private key ever entering the yard.
fwd="    ForwardAgent no"
[ "$FORWARD_SSH_AGENT" = 1 ] && fwd="    ForwardAgent yes"
identity_config="$(ssh_config_quote "$IDENTITY")" \
  || die "canonical SSH identity path cannot be represented safely"
known_config="$(ssh_config_quote "$known")" \
  || die "known-hosts path cannot be represented safely"
snip_backup=""
if [ -e "$snip" ] || [ -L "$snip" ]; then
  [ -f "$snip" ] && [ ! -L "$snip" ] \
    || die "existing SSH client snippet is not a regular non-symlink file"
  snip_backup="$(mktemp "$sshdir/.subyard-snippet-backup.XXXXXX")" \
    || die "could not stage the existing SSH client snippet for rollback"
  cp -- "$snip" "$snip_backup" && chmod 600 "$snip_backup" \
    || { rm -f -- "$snip_backup"; die "could not preserve the existing SSH client snippet"; }
fi
snip_temp="$(mktemp "$sshdir/.subyard-snippet.XXXXXX")" \
  || { rm -f -- "$snip_backup"; die "could not stage SSH client config"; }
if ! cat > "$snip_temp" <<EOF
# Managed by Subyard (scripts/07-ssh-access.sh) — regenerated on setup; do not edit.
Host $SSH_HOST
    HostName 127.0.0.1
    Port $SSH_PORT
    User $DEV_USER
    IdentityFile $identity_config
    IdentitiesOnly yes
    StrictHostKeyChecking yes
    UserKnownHostsFile $known_config
$fwd
EOF
then
  rm -f -- "$snip_temp" "$snip_backup"
  die "could not write staged SSH client config"
fi
cfg="$sshdir/config"; touch "$cfg"; chmod 600 "$cfg"
# Prepend this yard's Include once (must precede Host blocks to apply globally). One line per
# snippet file, idempotent: each yard has its own `Include subyard[-<name>].config`.
include_was_present=0
grep -qxF "Include $snip_name" "$cfg" && include_was_present=1
ssh_config_prepend_once "$cfg" "Include $snip_name" \
  || { rm -f -- "$snip_temp" "$snip_backup"; die "could not update SSH config: $cfg"; }
chmod 600 "$snip_temp" && mv -f -- "$snip_temp" "$snip" \
  || {
    rm -f -- "$snip_temp" "$snip_backup"
    [ "$include_was_present" = 1 ] \
      || ssh_config_remove_exact "$cfg" "Include $snip_name" \
      || die "SSH snippet publication failed and its new Include could not be removed"
    die "could not publish SSH client config"
  }

# The nested E2E topology has a stronger admission boundary: after the new snippet is
# published, retire every previous line and retain only the loopback-restricted key. Until
# this point both old and new credentials remain authorized, so a failed proof cannot lock
# the operator out through an existing snippet.
if [ "${NESTED_E2E_VMS:-0}" = 1 ] && ! incus exec \
  "$YARD_INSTANCE_NAME" "${PROJ[@]}" --env PUBKEY="$PUBKEY" --env DEV_USER="$DEV_USER" \
    -- sh -eu -c '
    home="$(getent passwd "$DEV_USER" | cut -d: -f6)"
    ak="$home/.ssh/authorized_keys"
    temp="$(mktemp "$home/.ssh/.authorized-keys.XXXXXX")"
    printf "from=\"127.0.0.1,::1\" %s\n" "$PUBKEY" > "$temp"
    chmod 600 "$temp"; chown "$DEV_USER":"$DEV_USER" "$temp"
    mv -f "$temp" "$ak"
  '; then
  if [ -n "$snip_backup" ]; then
    mv -f -- "$snip_backup" "$snip" \
      || die "nested admission failed and the previous SSH client snippet could not be restored"
  else
    rm -f -- "$snip"
  fi
  [ "$include_was_present" = 1 ] \
    || ssh_config_remove_exact "$cfg" "Include $snip_name" \
    || die "nested admission failed and the new SSH Include could not be removed"
  die "could not finalize the nested-yard SSH admission boundary; previous snippet restored"
fi
rm -f -- "$snip_backup"
ok "ssh Host '$SSH_HOST' ready (~/.ssh/$snip_name)"

echo
ok "SSH access ready."
cat <<MSG

Verify:
  ssh $SSH_HOST -- hostname        # logs into the yard as $DEV_USER
  yard code .                      # VS Code Remote-SSH into the yard
MSG
