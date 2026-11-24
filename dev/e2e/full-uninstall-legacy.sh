#!/usr/bin/env bash
# Additional released-source cases in the owning held-lease full-uninstall lane.
# Globals and helpers are supplied by the owning full-uninstall fixture.
# shellcheck disable=SC2154

legacy_env() {
  operator_env env SUBYARD_HOME="$state/legacy-data" SUBYARD_CONFIG_HOME="$state/legacy-config" \
    YARD_RUNTIME_ROOT="$state/legacy-runtime" YARD_RELEASE_CACHE="$state/legacy-cache" \
    YARD_BIN_DIR="$state/legacy-bin" YARD_SHELL_RC="$state/legacy.rc" YARD_LOGIN_RC="$state/legacy.login" \
    SUBYARD_POWER_UNIT_PATH="/etc/systemd/system/subyard-legacy-$token.service" \
    SUBYARD_POWER_RECONCILER_PATH="$state/host-helper/legacy-power" "$@"
}

legacy_full_uninstall() {
  local baseline=0.10.0 assets="$state/legacy-release" name old_target candidate before field
  name="subyard-$baseline-linux-amd64.tar.gz"
  operator_env mkdir -p "$assets"
  for suffix in '' .sha256 .manifest.json .provenance.json; do
    operator_env curl -fsSL --proto '=https' --tlsv1.2 \
      "https://github.com/Subyard/Subyard/releases/download/v$baseline/$name$suffix" -o "$assets/$name$suffix"
  done
  [ "$(sha256sum "$assets/$name" | cut -d' ' -f1)" = e61405d6539ccc3b953be1dd24b9d7101476a4380225b866fe8bd9896ae7c659 ] || fail 'released legacy bundle checksum changed'
  operator_env curl -fsSL --proto '=https' --tlsv1.2 \
    "https://github.com/Subyard/Subyard/releases/download/v$baseline/subyard-install-runtime-release.sh" -o "$assets/installer.sh"
  [ "$(sha256sum "$assets/installer.sh" | cut -d' ' -f1)" = 7932034b1c4f42c7fd66975ff1291bec5ea83b687988d634d67d46ff93ce6d06 ] || fail 'released legacy installer checksum changed'
  legacy_env bash "$assets/installer.sh" --runtime-root "$state/legacy-runtime" \
    --bundle "$assets/$name" --checksum "$assets/$name.sha256" --manifest "$assets/$name.manifest.json" \
    --provenance "$assets/$name.provenance.json" > "$state/legacy-install.log" 2>&1
  old_target="$(legacy_env readlink "$state/legacy-runtime/current")"
  [ "$(legacy_env "$state/legacy-runtime/current/bin/yard" --version)" = 'yard 0.10.0' ] || fail 'legacy producer is not the actual release'
  legacy_env mkdir -p "$state/legacy-config"
  legacy_env bash -c 'cat > "$1/legacy-config/config.env" <<CONFIG
INCUS_PROJECT=subyard-legacy-$2
YARD_INSTANCE_NAME=yard-legacy
HOST_BASE=$1/legacy-host
RESTRICTED_DISK_PATHS=$1/legacy-host
SRV_POOL=default
INCUS_BRIDGE=incusbr0
SSH_HOST=legacy-$2
SSH_PORT=24230
CODING_TOOL_INTEGRATIONS=
ENVIRONMENT_PROFILES=
FORWARD_SSH_AGENT=0
SHIFT_MODE=acl
DEV_UID=1001
BASE_IMAGE=subyard-e2e-debian-13-container
BASE_IMAGE_FALLBACK=subyard-e2e-debian-13-container
CONFIG
chmod 600 "$1/legacy-config/config.env"' _ "$state" "$token"
  # Use the original release's read-only interface before native setup.
  for field in INCUS_PROJECT YARD_INSTANCE_NAME HOST_BASE CODING_TOOL_INTEGRATIONS; do
    legacy_env "$state/legacy-runtime/current/bin/yard" config show "$field" >> "$state/legacy-context.log" 2>&1
  done
  grep -Fxq "effective: subyard-legacy-$token" "$state/legacy-context.log" || fail 'old default project configuration was not loaded'
  grep -Fxq 'effective: yard-legacy' "$state/legacy-context.log" || fail 'old default instance configuration was not loaded'
  grep -Fxq "effective: $state/legacy-host" "$state/legacy-context.log" || fail 'old default host configuration was not loaded'
  # The platform owns the cached image; the actual old product configures this
  # dedicated project and creates its own instance, custom volume and mounts.
  incus project create "subyard-legacy-$token" -c features.images=false -c "user.legacy-fixture=$marker" >/dev/null
  legacy_env "$state/legacy-runtime/current/bin/yard" init --yes > "$state/legacy-init.log" 2>&1
  [ "$(incus config device get yard-legacy subyard-e2e-routes source --project "subyard-legacy-$token")" = "$state/legacy-data/e2e/routes" ] || fail 'old producer did not create native installation association'
  sudo -n test -d "$state/legacy-host/backups"
  sudo -n install -o "$operator" -g "$operator" -m 0600 /dev/null "$state/legacy-host/foreign.control"
  legacy_env bash -c 'printf "preserved old host content\n" > "$1/legacy-host/foreign.control"' _ "$state"
  before="$(legacy_env find "$state/legacy-config" -type f -exec sha256sum '{}' + | sort | sha256sum | cut -d' ' -f1)"
  if legacy_env env YARD_RELEASE_BASE_URL="file://$state/release" bash "$state/bootstrap.sh" --version "$version" --yes > "$state/legacy-refusal.log" 2>&1; then fail 'old initialized source upgraded'; fi
  grep -Fq 'v0.11.0' "$state/legacy-refusal.log" && grep -Fq 'uninstall --full' "$state/legacy-refusal.log" || fail 'old-source refusal lacks removal guidance'
  [ "$(legacy_env readlink "$state/legacy-runtime/current")" = "$old_target" ] || fail 'old-source refusal activated candidate'
  [ "$(legacy_env find "$state/legacy-config" -type f -exec sha256sum '{}' + | sort | sha256sum | cut -d' ' -f1)" = "$before" ] || fail 'old-source refusal mutated protected configuration'
  legacy_env env YARD_RELEASE_BASE_URL="file://$state/release" bash "$state/bootstrap.sh" --version "$version" --publish-only > "$state/legacy-stage.log" 2>&1
  candidate="$(legacy_env find "$state/legacy-runtime/releases" -maxdepth 1 -type d -name "$version-*")/bin/yard"
  legacy_env test -x "$candidate"
  # A foreign resource in the proven project must refuse the WHOLE deletion.
  incus profile create foreign-control --project "subyard-legacy-$token" >/dev/null
  if legacy_env "$candidate" uninstall --full --yes > "$state/legacy-foreign-refusal.log" 2>&1; then fail 'foreign project resource was deleted'; fi
  incus profile show foreign-control --project "subyard-legacy-$token" >/dev/null
  incus profile delete foreign-control --project "subyard-legacy-$token" >/dev/null
  interrupt_full_removal legacy_env "$state/legacy-data" "$state/legacy-config" "$candidate" legacy-
  [ ! -e "$state/legacy-runtime" ] && [ ! -e "$state/legacy-config" ] && [ ! -e "$state/legacy-data.uninstall" ] || fail 'old uninstall retained runtime/config/recovery'
  ! incus project show "subyard-legacy-$token" >/dev/null 2>&1 || fail 'old native project remains'
  sudo -n test -d "$state/legacy-host/backups"
  grep -Fq "Preserved: legacy host namespace $state/legacy-host " "$state/legacy-retry.log" || fail 'legacy preservation boundary was not reported'
  legacy_env grep -Fxq 'preserved old host content' "$state/legacy-host/foreign.control"
  # This is a separate user installation after cleanup; no initializer is run.
  legacy_env env YARD_RELEASE_BASE_URL="file://$state/release" bash "$state/bootstrap.sh" --version "$version" --yes > "$state/legacy-clean-install.log" 2>&1
  legacy_env test ! -e "$state/legacy-config/config.env"
  legacy_env "$state/legacy-runtime/current/bin/yard" uninstall --full --yes > "$state/legacy-clean-remove.log" 2>&1
  printf 'verified actual v0.10.0 refusal, staged tooling, foreign collision, interrupted removal/resume and separate clean install\n' > "$state/legacy-result.log"
}

# A checkpoint that the actual v0.30.0 release wrote, with its retained runtime,
# resumes through the candidate CLI. Release-native facts and digests come from
# the released teardown helper, not from candidate code.
released_checkpoint_resume() {
  local baseline=0.30.0 assets="$state/released-release" name suffix
  name="subyard-$baseline-linux-amd64.tar.gz"
  released_env() {
    operator_env env SUBYARD_HOME="$state/released-data" SUBYARD_CONFIG_HOME="$state/released-config" \
      YARD_RUNTIME_ROOT="$state/released-runtime" YARD_RELEASE_CACHE="$state/released-cache" "$@"
  }
  operator_env mkdir -p "$assets"
  for suffix in '' .sha256 .manifest.json .provenance.json; do
    operator_env curl -fsSL --proto '=https' --tlsv1.2 \
      "https://github.com/Subyard/Subyard/releases/download/v$baseline/$name$suffix" -o "$assets/$name$suffix"
  done
  [ "$(sha256sum "$assets/$name" | cut -d' ' -f1)" = 85adf086cd630d61ba8ce3f4c0ea6e450bb898964e2135fbddaddf4104d75164 ] || fail 'released v0.30.0 bundle checksum changed'
  operator_env curl -fsSL --proto '=https' --tlsv1.2 \
    "https://github.com/Subyard/Subyard/releases/download/v$baseline/subyard-install-runtime-release.sh" -o "$assets/installer.sh"
  [ "$(sha256sum "$assets/installer.sh" | cut -d' ' -f1)" = aa842448af3056f58b565b75bc887ac1cebd3dc04cc07a2c3bf3326955858c30 ] || fail 'released v0.30.0 installer checksum changed'
  released_env bash "$assets/installer.sh" --runtime-root "$state/released-runtime" \
    --bundle "$assets/$name" --checksum "$assets/$name.sha256" --manifest "$assets/$name.manifest.json" \
    --provenance "$assets/$name.provenance.json" > "$state/released-install.log" 2>&1
  [ "$(released_env "$state/released-runtime/current/bin/yard" --version)" = "yard $baseline" ] || fail 'released checkpoint writer is not the actual release'
  released_env bash -c 'install -d -m 0700 "$1/released-config" "$1/released-data"
printf "INCUS_PROJECT=subyard-released-%s\nSSH_PORT=24231\n" "$2" > "$1/released-config/config.env"; chmod 600 "$1/released-config/config.env"
printf "released data\n" > "$1/released-data/state"; chmod 600 "$1/released-data/state"' _ "$state" "$token"
  interrupt_full_removal released_env "$state/released-data" "$state/released-config" \
    "$state/released-runtime/current/bin/yard" released- "$state/runtime/current/bin/yard"
  grep -Fq 'Fully removed this Subyard installation' "$state/released-retry.log" || fail 'candidate did not complete the released checkpoint'
  for suffix in runtime config data data.uninstall; do
    [ ! -e "$state/released-$suffix" ] || fail "released checkpoint resume left $suffix"
  done
  printf 'ok: candidate resumed an actual v0.30.0 checkpoint with its retained runtime\n'
}
