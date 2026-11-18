#!/usr/bin/env bash
# The artifact installer must never own rollback or migration choreography.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
runtime_root="$TMP/runtime"
engine="$TMP/yard-engine"
capture="$TMP/engine-called"

cat > "$engine" <<'ENGINE'
#!/usr/bin/env bash
touch "$ENGINE_CAPTURE"
exit 99
ENGINE
chmod 0700 "$engine"
for release in old new; do
  install -d -m 0700 "$runtime_root/releases/$release/bin"
  install -m 0700 "$engine" "$runtime_root/releases/$release/bin/yard-engine"
done
ln -s releases/new "$runtime_root/current"
ln -s releases/old "$runtime_root/previous"

set +e
ENGINE_CAPTURE="$capture" "$ROOT/scripts/install-runtime-release.sh" \
  --runtime-root "$runtime_root" --rollback >"$TMP/stdout" 2>"$TMP/stderr"
status=$?
set -e
[ "$status" -eq 2 ] \
  || { printf 'install runtime rollback: installer accepted rollback (status=%s)\n' "$status" >&2; exit 1; }
grep -Fxq 'install-runtime-release: rollback is owned by yard update --rollback' "$TMP/stderr" \
  || { printf 'install runtime rollback: missing module-ownership error\n' >&2; exit 1; }
[ "$(readlink "$runtime_root/current")" = releases/new ] \
  && [ "$(readlink "$runtime_root/previous")" = releases/old ] \
  && [ ! -e "$capture" ] \
  || { printf 'install runtime rollback: rejected request changed state or executed a runtime\n' >&2; exit 1; }

printf 'ok: runtime installer delegates rollback to ReleaseTransition\n'

# Put a harmless absolute member first, then enough names to exceed the pipe buffer.
member="$TMP/$(printf '%0100d' 0)"
touch "$member"
archive="$TMP/long.tar.gz"
awk -v path="${member##*/}" 'BEGIN { for (i=0; i<16384; i++) print path }' |
  tar --hard-dereference -C "$TMP" -czPf "$archive" "$member" -T -
sha256sum "$archive" > "$TMP/checksum"
read -r checksum _ < "$TMP/checksum"
case "$(uname -m)" in
  x86_64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) printf 'unsupported test architecture\n' >&2; exit 1 ;;
esac
jq -n --arg arch "$arch" '{schemaVersion:1,kind:"runtime",os:"linux",arch:$arch,
  version:"test",rpc:{min:1,max:1},projectStateSchema:1,credentialSchema:1}' > "$TMP/manifest"
jq -n --arg sha "$checksum" '{schemaVersion:1,artifact:"long.tar.gz",sha256:$sha,
  version:"test",sourceRepository:"github.com/Dmitry-Borodin/Subyard",sourceRevision:"test"}' > "$TMP/provenance"
if "$ROOT/scripts/install-runtime-release.sh" --check --runtime-root "$runtime_root" \
  --bundle "$archive" --checksum "$TMP/checksum" --manifest "$TMP/manifest" \
  --provenance "$TMP/provenance" >"$TMP/stdout" 2>"$TMP/stderr"; then
  printf 'runtime installer accepted an unsafe archive path\n' >&2
  exit 1
fi
grep -Fxq 'install-runtime-release: bundle contains an unsafe path' "$TMP/stderr" \
  || { cat "$TMP/stderr" >&2; exit 1; }
printf 'ok: runtime installer rejects an early unsafe path in a long archive\n'

mkdir "$TMP/fake-bin"
cat > "$TMP/fake-bin/tar" <<'TAR'
#!/usr/bin/env bash
case "$1" in
  -tzf) printf 'file\n' ;;
  -tvzf) printf '%s\n' '-rw-r--r-- user/group 0 2026-01-01 00:00 file'; exit 2 ;;
  -xzf) touch "$EXTRACTION_CAPTURE"; exit 99 ;;
  *) exit 98 ;;
esac
TAR
chmod +x "$TMP/fake-bin/tar"
if PATH="$TMP/fake-bin:$PATH" EXTRACTION_CAPTURE="$TMP/extracted" \
  "$ROOT/scripts/install-runtime-release.sh" --check --runtime-root "$runtime_root" \
  --bundle "$archive" --checksum "$TMP/checksum" --manifest "$TMP/manifest" \
  --provenance "$TMP/provenance" >"$TMP/stdout" 2>"$TMP/stderr"; then
  printf 'runtime installer accepted a failed verbose listing\n' >&2
  exit 1
fi
grep -Fxq 'install-runtime-release: could not list bundle member types' "$TMP/stderr" \
  || { cat "$TMP/stderr" >&2; exit 1; }
[ ! -e "$TMP/extracted" ]
printf 'ok: runtime installer rejects a failed verbose listing before extraction\n'
