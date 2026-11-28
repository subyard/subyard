#!/usr/bin/env bash
# The artifact installer must never own rollback or migration choreography.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/subyard-install-runtime-release-rollback.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT
# The installer runs against a fixture home and a private lock root, so neither
# a direct nor a suite run coordinates the operator's real data home.
real_home_lock="subyard-owner-locks-$(id -u)/$(printf '%s' "$(realpath -m -s -- "${SUBYARD_HOME:-$HOME/.subyard}")" | sha256sum | cut -d ' ' -f 1).lock"
export HOME="$TMP/operator" SUBYARD_HOME="$TMP/operator/.subyard" SUBYARD_CONFIG_HOME="$TMP/operator/.config/subyard"
unset XDG_CONFIG_HOME
export SUBYARD_OWNER_LOCK_ROOT="$TMP/owner-locks"
install -d -m 0700 "$HOME" "$SUBYARD_OWNER_LOCK_ROOT"
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

# The owner barrier restores its own lock directory, including one its owner
# cannot read, through a pinned inode; the installer requires exactly 0700.
# A private lock root keeps the shared directory under /tmp unchanged.
shared_locks="/tmp/subyard-owner-locks-$(id -u)"
shared_before="$(stat -c %a "$shared_locks" 2>/dev/null || echo absent)"
install -d -m 0700 "$TMP/lock-root"
installer="$ROOT/scripts/install-runtime-release.sh"
locks="$TMP/lock-root/subyard-owner-locks-$(id -u)"
for mode in 0300 1755; do
  install -d -m 0700 "$locks"
  chmod "$mode" "$locks"
  set +e
  SUBYARD_OWNER_LOCK_ROOT="$TMP/lock-root" SUBYARD_HOME="$TMP/home" ENGINE_CAPTURE="$capture" "$installer" \
    --runtime-root "$runtime_root" --rollback >"$TMP/stdout" 2>"$TMP/stderr"
  status=$?
  set -e
  observed="$(stat -c %a "$locks")"
  [ "$status" -eq 2 ] && [ "$observed" = 700 ] \
    || { printf 'install runtime rollback: owner lock directory %s was not repaired (status=%s mode=%s)\n' "$mode" "$status" "$observed" >&2; exit 1; }
done
[ "$(stat -c %a "$shared_locks" 2>/dev/null || echo absent)" = "$shared_before" ] \
  || { printf 'install runtime rollback: shared owner lock directory changed\n' >&2; exit 1; }
printf 'ok: owner barrier repairs its lock directory mode\n'

# The installer reads the lock root as the engine does: a relative, unclean or
# double-slash root is refused before anything is created.
for root in relative/locks "$TMP/lock-root/../lock-root" "/$TMP/lock-root"; do
  set +e
  SUBYARD_OWNER_LOCK_ROOT="$root" SUBYARD_HOME="$TMP/home" ENGINE_CAPTURE="$capture" "$installer" \
    --runtime-root "$runtime_root" --rollback >"$TMP/stdout" 2>"$TMP/stderr"
  status=$?
  set -e
  [ "$status" -eq 2 ] && grep -Fq 'SUBYARD_OWNER_LOCK_ROOT must be a clean absolute path' "$TMP/stderr" \
    || { printf 'install runtime rollback: lock root %s accepted (status=%s)\n' "$root" "$status" >&2; exit 1; }
done
printf 'ok: owner barrier refuses an unsafe lock root\n'
# The native helpers read the same root behind the privilege boundary.
for root in relative/locks "$TMP/lock-root/../lock-root" "/$TMP/lock-root"; do
  SUBYARD_OWNER_LOCK_ROOT="$root" SUBYARD_HOME="$TMP/home" SUBYARD_USER="$(id -un)" python3 -B - "$ROOT/scripts/lib" <<'PY' \
    || { printf 'install runtime rollback: native helper accepted lock root %s\n' "$root" >&2; exit 1; }
import importlib.util, os, sys
def load(name):
    spec = importlib.util.spec_from_file_location(name.replace('-', '_'), os.path.join(sys.argv[1], name + '.py'))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module
def refused(call):
    try:
        call()
    except ValueError as error:
        return str(error) == 'invalid owner lock root'
    return False
barrier = load('native-owner-barrier')
os.environ['SUBYARD_NATIVE_OWNER_FD3'] = '1'
custody = load('native-install-custody')
os.environ['SUBYARD_NATIVE_OWNER_STDIN'] = '1'
sys.argv[1:] = ['owner-barrier', 'host-power', '/nonexistent']
sys.exit(0 if refused(barrier.inherited_owner_descriptors) and refused(custody.main) else 1)
PY
done
printf 'ok: native helpers refuse an unsafe lock root\n'

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
[ ! -e "$SUBYARD_OWNER_LOCK_ROOT/$real_home_lock" ] \
  || { printf 'install runtime rollback: the installer coordinated the real data home\n' >&2; exit 1; }
