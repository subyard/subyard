#!/usr/bin/env bash
# Additional released-source cases in the owning held-lease full-uninstall lane.
# Globals and helpers are supplied by the owning full-uninstall fixture.
# shellcheck disable=SC2154

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
