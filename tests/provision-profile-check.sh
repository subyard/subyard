#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp="$(mktemp -d /tmp/subyard-profile-check.XXXXXX)"
trap 'rm -rf -- "$tmp"' EXIT
mkdir -p "$tmp/runtime/scripts/lib" "$tmp/runtime/config/profiles/synthetic" "$tmp/bin" "$tmp/home" "$tmp/config-home" "$tmp/storage" "$tmp/host"

cp "$ROOT/scripts/provision-profile.sh" "$tmp/runtime/scripts/"
cp "$ROOT/scripts/lib/engine-context.sh" "$ROOT/scripts/lib/producer-run.py" "$tmp/runtime/scripts/lib/"
: > "$tmp/runtime/config/profiles/synthetic/profile.conf"
printf '#!/bin/sh\n# subyard-provision-check-v1\nprintf "hook output\\n"\nprintf "hook error\\n" >&2\nexit "${PROVISION_CHECK_STATUS:?}"\n' > "$tmp/runtime/config/profiles/synthetic/provision.sh"

# Fake the external guest boundary here; PID/mount/remote cancellation is a VM gate.
cat > "$tmp/bin/incus" <<'INCUS'
#!/usr/bin/env python3
import os, subprocess, sys, tarfile, tempfile
with open(os.environ["PROFILE_EFFECT_MARKER"], "a") as marker:
    marker.write("effect\n")
with tempfile.TemporaryDirectory() as target:
    with tarfile.open(fileobj=sys.stdin.buffer, mode="r|") as archive:
        archive.extractall(target, filter="data")
    mode = sys.argv[-1]
    result = subprocess.run(["bash", target + "/provision.sh", *(["--check"] if mode == "check" else [])],
                            stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL if mode == "check" else None)
    sys.exit(result.returncode)
INCUS
chmod +x "$tmp/bin/incus"

engine_env=(
  PATH="$tmp/bin:$PATH"
  PROFILE_EFFECT_MARKER="$tmp/effects"
  SUBYARD_METADATA_FD=3
  SUBYARD_ENGINE_CONTEXT=1
  SUBYARD_ENGINE_CONTEXT_SCHEMA=1
  SUBYARD_OPERATOR_HOME="$tmp/home"
  SUBYARD_CONFIG_DIR="$ROOT/config"
  SUBYARD_CONFIG_HOME="$tmp/config-home"
  SUBYARD_HOME="$ROOT"
  STORAGE_PATH="$tmp/storage"
  HOST_BASE="$tmp/host"
  RESTRICTED_DISK_PATHS=""
  ACCESS_KIND=local
  YARD_KIND=container
  YARD_INSTANCE_NAME=yard-check
  INCUS_PROJECT=subyard-check
  INCUS_BRIDGE=incusbr0
  SSH_HOST=yard-check
  DEV_USER=dev
  DEV_UID=1000
  DEV_SUDO=1
  FORWARD_SSH_AGENT=0
  NESTED_E2E_VMS=0
)

bound_provision() {
  python3 - "$ROOT/scripts/lib/producer-run.py" "$tmp/runtime/config/profiles/synthetic" "$tmp/request.json" <<'BINDING'
import hashlib, importlib.util, json, os, pathlib, sys
spec = importlib.util.spec_from_file_location("producer", sys.argv[1])
producer = importlib.util.module_from_spec(spec)
spec.loader.exec_module(producer)
root = pathlib.Path(sys.argv[2])
entries = {}
for path in [root, *sorted(root.rglob("*"))]:
    info = path.lstat()
    digest = hashlib.sha256(path.read_bytes()).hexdigest() if path.is_file() else None
    entries[str(path.relative_to(root))] = producer.source_identity(info, digest)
pathlib.Path(sys.argv[3]).write_text(json.dumps({"input": {"profileBinding": {"root": str(root), "entries": entries, "environment": {}, "parents": {}}}}))
BINDING
  env "${engine_env[@]}" "$@" 3<"$tmp/request.json"
}

output="$(bound_provision PROVISION_CHECK_STATUS=0 \
  bash "$tmp/runtime/scripts/provision-profile.sh" --check synthetic)"
[ "$output" = converged ] || { printf 'FAIL: converged output=%q\n' "$output" >&2; exit 1; }

output="$(bound_provision PROVISION_CHECK_STATUS=10 \
  bash "$tmp/runtime/scripts/provision-profile.sh" --check synthetic)"
[ "$output" = changed ] || { printf 'FAIL: changed output=%q\n' "$output" >&2; exit 1; }

status=0
bound_provision PROVISION_CHECK_STATUS=7 \
  bash "$tmp/runtime/scripts/provision-profile.sh" --check synthetic >"$tmp/out" 2>"$tmp/err" || status=$?
[ "$status" = 7 ] && [ ! -s "$tmp/out" ] && [ "$(cat "$tmp/err")" = 'hook error' ] \
  || { printf 'FAIL: failed check status/output was not preserved\n' >&2; exit 1; }
for expected in 0 7; do
  status=0
  bound_provision PROVISION_CHECK_STATUS="$expected" \
    bash "$tmp/runtime/scripts/provision-profile.sh" synthetic >"$tmp/out" 2>"$tmp/err" || status=$?
  [ "$status" = "$expected" ] && [ "$(cat "$tmp/out")" = 'hook output' ] && [ "$(cat "$tmp/err")" = 'hook error' ] \
    || { printf 'FAIL: apply status/output was not preserved\n' >&2; exit 1; }
done

# Large stderr must be drained, capped, and retain the native failure status.
printf '#!/bin/bash\n# subyard-provision-check-v1\nhead -c 200000 /dev/zero | tr "\\0" x >&2\nexit 7\n' > "$tmp/runtime/config/profiles/synthetic/provision.sh"
status=0
bound_provision bash "$tmp/runtime/scripts/provision-profile.sh" synthetic >"$tmp/out" 2>"$tmp/err" || status=$?
[ "$status" = 7 ] && [ "$(wc -c <"$tmp/err")" -lt 65700 ] && grep -q 'stderr truncated' "$tmp/err"
# Replacing a bound source must refuse before the external guest command starts.
before_effects="$(wc -c <"$tmp/effects")"
printf changed >> "$tmp/runtime/config/profiles/synthetic/profile.conf"
status=0
env "${engine_env[@]}" bash "$tmp/runtime/scripts/provision-profile.sh" synthetic 3<"$tmp/request.json" >"$tmp/out" 2>"$tmp/err" || status=$?
[ "$status" = 125 ] && [ "$(wc -c <"$tmp/effects")" = "$before_effects" ] \
  || { printf 'FAIL: stale bundle reached guest effects\n' >&2; exit 1; }
printf 'ok: provision profile check protocol and bounded capture\n'

python3 "$ROOT/tests/producer-run.py"
