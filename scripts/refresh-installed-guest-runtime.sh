#!/usr/bin/env bash
# Refresh only the release-owned guest helpers, without base/profile provisioning.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/engine-context.sh
. "$SCRIPT_DIR/lib/engine-context.sh"
subyard_require_engine_context
# shellcheck source=scripts/lib/native-installed-stage.sh
. "$SCRIPT_DIR/lib/native-installed-stage.sh"
# shellcheck source=scripts/lib/ui.sh
. "$SCRIPT_DIR/lib/ui.sh"
PROJ=(--project "$INCUS_PROJECT")
PREVIEW_SOURCE="$SCRIPT_DIR/../config/preview/subyard-preview"
PREVIEW_ENDPOINT="${SUBYARD_PREVIEW_ENDPOINT:?prepared preview endpoint required}"
PREVIEW_SHA256="$(sha256sum "$PREVIEW_SOURCE" | cut -d ' ' -f1)"
# The helper owns no service or persistent preview state.
incus exec "$YARD_INSTANCE_NAME" "${PROJ[@]}" -- sh -eu -c '
  install -d -m 0755 -o root -g root /usr/local/bin
  temporary=$(mktemp /usr/local/bin/.subyard-preview.XXXXXX)
  trap '\''rm -f -- "$temporary"'\'' EXIT HUP INT TERM
  cat > "$temporary"
  chown root:root "$temporary"
  chmod 0755 "$temporary"
  if [ ! -L /usr/local/bin/subyard-preview ] && \
      [ "$(stat -c "%F|%a|%u:%g" /usr/local/bin/subyard-preview 2>/dev/null || true)" = "regular file|755|0:0" ] && \
      cmp -s "$temporary" /usr/local/bin/subyard-preview; then
    exit 0
  fi
  mv -fT -- "$temporary" /usr/local/bin/subyard-preview
' < "$PREVIEW_SOURCE" || die "static preview helper installation failed"
ok "static preview helper ready"
PREVIEW_ENDPOINT_SHA256="$(printf '%s' "$PREVIEW_ENDPOINT" | sha256sum | cut -d ' ' -f1)"
printf '%s' "$PREVIEW_ENDPOINT" | incus exec "$YARD_INSTANCE_NAME" "${PROJ[@]}" -- sh -eu -c '
  [ ! -L /etc/subyard ]
  install -d -m 0755 -o root -g root /etc/subyard
  temporary=$(mktemp /etc/subyard/.preview.XXXXXX)
  trap '\''rm -f -- "$temporary"'\'' EXIT HUP INT TERM
  cat > "$temporary"
  chown root:root "$temporary"
  chmod 0644 "$temporary"
  if [ ! -L /etc/subyard/preview.json ] &&
      [ "$(stat -c "%F|%a|%u:%g" /etc/subyard/preview.json 2>/dev/null || true)" = "regular file|644|0:0" ] &&
      cmp -s "$temporary" /etc/subyard/preview.json; then
    exit 0
  fi
  mv -fT -- "$temporary" /etc/subyard/preview.json
' || die "static preview endpoint metadata installation failed"
ok "static preview endpoint ready"

if [ "${ALLOWS_CODING_TOOLS:-true}" != false ]; then
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
incus exec "$YARD_INSTANCE_NAME" "${PROJ[@]}" -- sh -eu -c '
  install -d -m 0755 /var/lib/subyard
  receipt=$(mktemp /var/lib/subyard/.ccusage-ownership.XXXXXX)
  printf "%s\n" "# subyard-ccusage-ownership-v1" >"$receipt"
  sha256sum /usr/local/bin/ccusage >>"$receipt"
  chmod 0600 "$receipt"
  chown root:root "$receipt"
  mv -f -- "$receipt" /var/lib/subyard/ccusage-ownership
' || die "could not record ccusage ownership"

fi

incus config set "$YARD_INSTANCE_NAME" user.subyard.preview_sha256 "$PREVIEW_SHA256" "${PROJ[@]}" \
  || die "could not record preview convergence"
incus config set "$YARD_INSTANCE_NAME" user.subyard.preview_endpoint_sha256 "$PREVIEW_ENDPOINT_SHA256" "${PROJ[@]}" \
  || die "could not record preview endpoint convergence"
if [ "${ALLOWS_CODING_TOOLS:-true}" != false ]; then
incus config set "$YARD_INSTANCE_NAME" user.subyard.ccusage_version "$CCUSAGE_VERSION" "${PROJ[@]}" \
  || die "could not record ccusage convergence"
fi
