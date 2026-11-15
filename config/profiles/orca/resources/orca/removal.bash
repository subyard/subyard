# shellcheck shell=bash
# Observe or remove the Orca wiring of a yard that does not select Orca, by the
# fixed paths this profile and its older releases installed. It never runs an
# installed Orca helper. State under /srv/agents/orca and the orca-ide package
# stay. Arguments: observe <guest-root> <desired> | apply <guest-root>.
set -uo pipefail
mode="${1:-}"; root="${2:-}"; root="${root%/}"
units=(subyard-orca-discovery.timer subyard-orca-discovery.service subyard-orca.service)
files=(
  /usr/local/libexec/subyard/projects-changed.d/orca
  /usr/local/libexec/subyard/orca-registration.sha256
  /usr/local/libexec/subyard/orca-contract.sha256
  /usr/local/libexec/subyard/orca-registration.lock
  /usr/local/libexec/subyard/orca-ingress
  /usr/local/libexec/subyard/orca-capture-ready
  /etc/profile.d/subyard-orca-codex.sh
  /etc/systemd/system/subyard-orca.service
  /etc/systemd/system/subyard-orca-discovery.service
  /etc/systemd/system/subyard-orca-discovery.timer
)
directories=(/usr/local/libexec/subyard/orca-registration /usr/local/libexec/subyard/orca-codex)
table=subyard_orca
marker=subyard-orca-managed

installed() {
  local unit path
  for unit in "${units[@]}"; do
    if systemctl is-active --quiet "$unit"; then printf 'active %s\n' "$unit"; fi
    if systemctl is-enabled --quiet "$unit" 2>/dev/null; then printf 'enabled %s\n' "$unit"; fi
  done
  for path in "${files[@]}" "${directories[@]}"; do
    if [ -e "$root$path" ] || [ -L "$root$path" ]; then printf 'present %s\n' "$path"; fi
  done
  if nft list table inet "$table" >/dev/null 2>&1; then printf 'present nft inet %s\n' "$table"; fi
}

case "$mode" in
  observe)
    desired="${3:-}"
    [[ "$desired" =~ ^[0-9a-f]{64}$ ]] || { echo 'invalid Orca removal digest' >&2; exit 2; }
    found="$(installed)" || exit 1
    if [ -z "$found" ]; then
      # Converged removal: current at the removal digest, proving no ownership.
      printf '{"state":"current","actual":"%s","desired":"%s","ownership_absent":true}\n' "$desired" "$desired"
    else
      printf '{"state":"stale","actual":"%s","desired":"%s"}\n' \
        "$(printf '%s\n' "$found" | sha256sum | cut -d ' ' -f 1)" "$desired"
    fi
    ;;
  apply)
    # Every independent step runs; each refusal is reported in stable order.
    refusals=()
    for unit in "${units[@]}"; do
      if systemctl is-active --quiet "$unit" || systemctl is-enabled --quiet "$unit" 2>/dev/null; then
        systemctl disable --now "$unit" >/dev/null 2>&1 || refusals+=("could not stop and disable $unit")
      fi
    done
    if nft list table inet "$table" >/dev/null 2>&1; then
      if nft list chain inet "$table" input 2>/dev/null | grep -F "comment \"$marker\"" >/dev/null; then
        nft delete table inet "$table" || refusals+=("could not remove the Orca nft table inet $table")
      else
        refusals+=("refusing to remove the unowned nft table inet $table")
      fi
    fi
    for path in "${files[@]}"; do
      rm -f -- "$root$path" || refusals+=("could not remove $path")
    done
    for path in "${directories[@]}"; do
      rm -rf -- "$root$path" || refusals+=("could not remove $path")
    done
    systemctl daemon-reload || refusals+=("could not reload systemd units")
    for refusal in "${refusals[@]}"; do printf 'Orca removal: %s\n' "$refusal" >&2; done
    [ "${#refusals[@]}" -eq 0 ]
    ;;
  *) echo 'usage: removal.bash observe <guest-root> <desired> | apply <guest-root>' >&2; exit 2 ;;
esac
