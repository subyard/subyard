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
