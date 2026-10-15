#!/usr/bin/env bash
# One standard disposable owner VM; the guest owns bounded native cleanup.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd -P)"
usage() { printf 'Usage: release-activation.sh --slot N [--preflight]\n'; }
slot=''
preflight=0
STARTED="$(date +%s)"
while [ "$#" -gt 0 ]; do
  case "$1" in
    --slot) [ "$#" -ge 2 ] && [ -z "$slot" ] || { usage >&2; exit 2; }; slot="$2"; shift 2 ;;
    --preflight) preflight=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done
[[ "$slot" =~ ^[0-9]+$ ]] || { usage >&2; exit 2; }
for command in go jq python3 shellcheck tar timeout sha256sum; do
  command -v "$command" >/dev/null || { printf 'release-activation: missing %s\n' "$command" >&2; exit 2; }
done
bash -n "$ROOT/config/profiles/github/tests/e2e/release-activation.sh" "$ROOT/config/profiles/github/tests/e2e/release-activation-guest.sh"
shellcheck -S warning "$ROOT/config/profiles/github/tests/e2e/release-activation.sh" "$ROOT/config/profiles/github/tests/e2e/release-activation-guest.sh"
if [ "$preflight" = 1 ]; then
  bash "$ROOT/config/profiles/github/tests/e2e/release-activation-guest.sh" --local-probe
  printf 'ok: release activation local fixture prerequisites; no lease acquired\n'
  exit 0
fi
# Copy the exact coordinator-frozen public bundle; source and candidate A stay intact.
[[ "${SUBYARD_E2E_CANDIDATE_SHA256:-}" =~ ^[0-9a-f]{64}$ ]] \
  && [ -f "${SUBYARD_E2E_CANDIDATE_BUNDLE:-}" ] && [ ! -L "$SUBYARD_E2E_CANDIDATE_BUNDLE" ] \
  || { printf 'release-activation: frozen candidate bundle and SHA256 required\n' >&2; exit 2; }
[ "$(sha256sum "$SUBYARD_E2E_CANDIDATE_BUNDLE" | cut -d' ' -f1)" = "$SUBYARD_E2E_CANDIDATE_SHA256" ]
WORK="$(mktemp -d /tmp/subyard-activation-controller.XXXXXX)"
printf '%s\n' subyard-activation-controller-v1 > "$WORK/.marker"
cleanup() {
  local original=$?
  trap - EXIT INT TERM
  [ -f "$WORK/.marker" ] && [ "$(cat "$WORK/.marker")" = subyard-activation-controller-v1 ] \
    && [[ "$WORK" = /tmp/subyard-activation-controller.* ]] || exit 3
  find "$WORK" -depth -delete || exit 3
  exit "$original"
}
trap cleanup EXIT
trap 'exit 143' TERM
trap 'exit 130' INT
python3 - "$SUBYARD_E2E_CANDIDATE_BUNDLE" "$WORK/source-a" <<'PY'
import pathlib,sys,tarfile
target=pathlib.Path(sys.argv[2]); target.mkdir()
with tarfile.open(sys.argv[1],'r:gz') as bundle:
    for member in bundle.getmembers():
        if member.name.startswith('/') or '..' in member.name.split('/') or not (member.isfile() or member.isdir()):
            raise SystemExit('unsafe frozen public bundle')
    bundle.extractall(target,filter='data')
PY
jq -e '.version == "0.19.1-activation-e2e"' "$WORK/source-a/.subyard-acceptance/candidate.json" >/dev/null
# shellcheck source=tests/helpers/release-candidate.sh
. "$ROOT/tests/helpers/release-candidate.sh"
release_candidate_prepare "$WORK/source-a" >/dev/null
mkdir "$WORK/source-b"
bash "$ROOT/tests/helpers/source-files.sh" "$WORK/source-a" \
  | tar -C "$WORK/source-a" --null -T - -cf - | tar -C "$WORK/source-b" -xf -
printf '\nSynthetic release activation fixture B.\n' >> "$WORK/source-b/config/profiles/github/SKILL.md"
printf '\n# Synthetic release activation fixture B.\n' >> "$WORK/source-b/config/preview/subyard-preview"
printf 'release_activation_frozen_a_bundle_sha256=%s synthetic_b=0.19.2-activation-fixture\n' "$SUBYARD_E2E_CANDIDATE_SHA256"
sha256sum "$WORK/source-a/config/profiles/github/SKILL.md" "$WORK/source-b/config/profiles/github/SKILL.md" \
  "$WORK/source-a/config/preview/subyard-preview" "$WORK/source-b/config/preview/subyard-preview"
# Standard packager regenerates manifests/provenance; no hand-edited runtime receipts.
timeout --signal=TERM --kill-after=30 600 "$WORK/source-b/dev/package-engine.sh" \
  --version 0.19.2-activation-fixture --output-dir "$WORK/source-a/.subyard-acceptance/fixture-b"
sha256sum "$WORK/source-a/.subyard-acceptance/fixture-b/"subyard-0.19.2-activation-fixture-linux-*.tar.gz
# Revalidate the original A archive/metadata after adding the transport-only sidecar.
release_candidate_prepare "$WORK/source-a" >/dev/null
jq -c '{version,runtime_file,runtime_sha256}' "$WORK/source-a/.subyard-acceptance/candidate.json"
tar -C "$WORK/source-a" -czf "$WORK/fixture-bundle.tar.gz" .
SUBYARD_E2E_CANDIDATE_BUNDLE="$WORK/fixture-bundle.tar.gz"
SUBYARD_E2E_CANDIDATE_SHA256="$(sha256sum "$SUBYARD_E2E_CANDIDATE_BUNDLE" | cut -d' ' -f1)"
export SUBYARD_E2E_CANDIDATE_BUNDLE SUBYARD_E2E_CANDIDATE_SHA256
printf 'release_activation_composed_transport_sha256=%s\n' "$SUBYARD_E2E_CANDIDATE_SHA256"
remaining=$((2700 - $(date +%s) + STARTED))
[ "$remaining" -ge 300 ] || { printf 'release-activation: work deadline exhausted before VM allocation\n' >&2; exit 2; }
bash "$ROOT/dev/agent-e2e.sh" --slot "$slot" --vm-count 1 --vm 1 \
  --purpose github-release-activation -- timeout --signal=TERM --kill-after=360 "$remaining" \
  bash config/profiles/github/tests/e2e/release-activation-guest.sh
