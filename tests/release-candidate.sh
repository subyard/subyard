#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
. "$ROOT/tests/helpers/release-candidate.sh"
root="$TMP/root"
mkdir -p "$root/.subyard-acceptance/release"
! release_candidate_prepare "$root" >/dev/null
archive="$root/.subyard-acceptance/release/runtime.tar.gz"
python3 - "$archive" <<'PY'
import hashlib, io, tarfile, sys
files={"bin/yard":b"#!/bin/sh\n", "bin/yard-engine":b"#!/bin/sh\n"}
files["runtime-files.sha256"]=''.join(hashlib.sha256(v).hexdigest()+"  "+k+"\n" for k,v in files.items()).encode()
with tarfile.open(sys.argv[1], "w:gz") as archive:
    for name, value in files.items():
        member=tarfile.TarInfo(name); member.size=len(value); member.mode=0o755 if name.startswith("bin/") else 0o644
        archive.addfile(member, io.BytesIO(value))
PY
hash="$(sha256sum "$archive" | awk '{print $1}')"
printf '{"version":"test","runtime_file":"runtime.tar.gz","runtime_sha256":"%s"}\n' "$hash" > "$root/.subyard-acceptance/candidate.json"
[ "$(release_candidate_prepare "$root")" = "$root/.subyard-acceptance/runtime/bin/yard-engine" ]
# Fresh profile fixtures must use the standalone installer, isolated entrypoints
# and an unselected readiness query instead of executing the extracted engine.
case "$(uname -m)" in x86_64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) exit 1 ;; esac
runtime_file="subyard-1.2.3-linux-$arch.tar.gz"
cp "$archive" "$root/.subyard-acceptance/release/$runtime_file"
printf '{"version":"1.2.3","runtime_file":"%s","runtime_sha256":"%s"}\n' \
  "$runtime_file" "$hash" > "$root/.subyard-acceptance/candidate.json"
export SUBYARD_CONFIG_HOME="$TMP/config" SUBYARD_HOME="$TMP/data"
export TEST_CANDIDATE_STATE="$TMP/state" TEST_CANDIDATE_READY=ready
cat > "$root/.subyard-acceptance/release/subyard-install.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[ "$*" = '--version 1.2.3 --yes' ]
[ "$YARD_RUNTIME_ROOT" = "$TEST_CANDIDATE_STATE/runtime" ]
[ "$YARD_RELEASE_CACHE" = "$TEST_CANDIDATE_STATE/releases" ]
[ "$YARD_BIN_DIR" = "$TEST_CANDIDATE_STATE/bin" ]
[ "$YARD_SHELL_RC" = "$TEST_CANDIDATE_STATE/bashrc" ]
[ "$YARD_LOGIN_RC" = "$TEST_CANDIDATE_STATE/profile" ]
[ "$SUBYARD_CONFIG_HOME" = "${TEST_CANDIDATE_STATE%/*}/config" ]
[ "$SUBYARD_HOME" = "${TEST_CANDIDATE_STATE%/*}/data" ]
install -d "$YARD_BIN_DIR"
cat > "$YARD_BIN_DIR/yard" <<'YARD'
#!/usr/bin/env bash
set -euo pipefail
[ "$*" = 'migrate --check --json' ]
[ "$YARD_RUNTIME_ROOT" = "$TEST_CANDIDATE_STATE/runtime" ]
printf '{"outcome":{"status":"%s"}}\n' "$TEST_CANDIDATE_READY"
YARD
chmod 0755 "$YARD_BIN_DIR/yard"
SH
[ "$(release_candidate_install "$root" "$TEST_CANDIDATE_STATE")" = "$TEST_CANDIDATE_STATE/bin/yard" ]
TEST_CANDIDATE_READY=operator-action-required
export TEST_CANDIDATE_READY
if release_candidate_install "$root" "$TEST_CANDIDATE_STATE" > "$TMP/not-ready-launcher"; then
  printf 'FAIL: release fixture continued without migration readiness\n' >&2; exit 1
fi
[ ! -s "$TMP/not-ready-launcher" ]
printf '{"version":"test","runtime_file":"runtime.tar.gz","runtime_sha256":"%064d"}\n' 0 > "$root/.subyard-acceptance/candidate.json"
! release_candidate_prepare "$root" >/dev/null
python3 - "$archive" <<'PY'
import io, tarfile, sys
with tarfile.open(sys.argv[1], "w:gz") as archive:
    member=tarfile.TarInfo("../escape"); member.size=1; archive.addfile(member, io.BytesIO(b"x"))
PY
hash="$(sha256sum "$archive" | awk '{print $1}')"
printf '{"version":"test","runtime_file":"runtime.tar.gz","runtime_sha256":"%s"}\n' "$hash" > "$root/.subyard-acceptance/candidate.json"
! release_candidate_prepare "$root" >/dev/null
printf 'ok: release candidate helper validates packaged runtime\n'
printf 'ok: fresh release fixture installs isolated entrypoints and requires unselected migration readiness\n'
