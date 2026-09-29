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

# Execute the client's install section in a temporary profile-owned directory.
# Repeated installation retires old aliases without shadowing the owner's CLI.
tmp="$(mktemp -d)"
trap 'rm -rf -- "$tmp"' EXIT
mkdir -p "$tmp/public/bin" "$tmp/owner"
touch "$tmp/public/bin/yard"
ln -s missing-client "$tmp/public/bin/yard-emu"
sed -n '/^install -m 0755 .*bin\/android-broker/,/^# Retire the old staged/p' \
  "$ROOT/config/profiles/android/pool-install.sh" | sed '$d' > "$tmp/install-client.sh"
for _attempt in 1 2; do
  PUBLIC_ROOT="$tmp/public" bash "$tmp/install-client.sh"
  [ -x "$tmp/public/bin/android-broker" ]
  for retired in yard yard-emu; do
    [ ! -e "$tmp/public/bin/$retired" ] && [ ! -L "$tmp/public/bin/$retired" ]
  done
done
cat > "$tmp/owner/yard" <<'EOF'
#!/usr/bin/env bash
printf 'installed CLI\n'
EOF
chmod +x "$tmp/owner/yard"
PATH="$tmp/public/bin:$tmp/owner:$PATH" yard > "$tmp/output"
grep -Fxq 'installed CLI' "$tmp/output"

# Redirect only the installed Python payload path to a fixture.
cat > "$tmp/client.py" <<'EOF'
import sys
assert sys.argv[1:] == ['run', '--', 'argument with spaces']
print('broker client')
sys.exit(23)
EOF
sed -i "s@/usr/local/lib/subyard-android/client.py@$tmp/client.py@" "$tmp/public/bin/android-broker"
status=0
"$tmp/public/bin/android-broker" run -- 'argument with spaces' > "$tmp/output" || status=$?
[ "$status" = 23 ] && grep -Fxq 'broker client' "$tmp/output" \
  || { printf 'FAIL: broker wrapper lost arguments or exit code\n' >&2; exit 1; }
printf 'ok: broker client installation and retired manual entrypoints\n'
