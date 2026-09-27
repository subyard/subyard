#!/usr/bin/env bash
# Retired manual entrypoints must never launch an emulator outside a lease.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
for command in emulator-run.sh emulator-control.sh; do
  if bash "$ROOT/config/profiles/android/$command" start >/dev/null 2>&1; then
    printf 'FAIL: legacy %s bypasses lease ownership\n' "$command" >&2
    exit 1
  fi
done

# The generated emulator facade must preserve the CLI already installed on PATH.
tmp="$(mktemp -d)"
trap 'rm -rf -- "$tmp"' EXIT
mkdir -p "$tmp/facade" "$tmp/alias" "$tmp/home/.local/bin"
sed -n '/"$PUBLIC_ROOT\/bin\/yard" <<\x27EOF\x27/,/^EOF/p' \
  "$ROOT/config/profiles/android/pool-install.sh" | sed '1d;$d' > "$tmp/facade/yard"
chmod +x "$tmp/facade/yard"
ln -s "$tmp/facade/yard" "$tmp/alias/yard"
cat > "$tmp/home/.local/bin/yard" <<'EOF'
#!/usr/bin/env bash
[ "$#" = 2 ] && [ "$1" = status ] && [ "$2" = 'argument with spaces' ] || exit 99
printf 'installed CLI\n'
exit 23
EOF
chmod +x "$tmp/home/.local/bin/yard"
status=0
HOME="$tmp/home" PATH="$tmp/facade:$tmp/alias:$tmp/home/.local/bin:$PATH" \
  "$tmp/facade/yard" status 'argument with spaces' > "$tmp/output" || status=$?
[ "$status" = 23 ] && grep -Fxq 'installed CLI' "$tmp/output" \
  || { printf 'FAIL: Android facade hid installed yard CLI\n' >&2; exit 1; }
printf 'ok: manual emulator bypasses disabled\n'
