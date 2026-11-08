#!/usr/bin/env bash
# `incus list NAME` also lists every instance whose name starts with NAME. Product
# scripts read an instance's status and IPv4 through one exact-name helper.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d /tmp/subyard-exact-instance.XXXXXX)"
trap 'rm -rf "$TMP"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

install -d -m 0700 "$TMP/bin"
export PATH="$TMP/bin:$PATH" MOCK_ROWS="$TMP/rows"
# The prefix-matched neighbour is listed before the exact instance.
cat > "$TMP/bin/incus" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
[ "$1 $3 $4 $5 $6 $7" = 'list --project subyard -f csv -c' ] || { printf 'unexpected incus call: %s\n' "$*" >&2; exit 90; }
[ -s "$MOCK_ROWS.fail" ] && exit 1
cat "$MOCK_ROWS.$8"
MOCK
chmod 0700 "$TMP/bin/incus"
printf 'yard-hermes,RUNNING\n' > "$MOCK_ROWS.ns"
printf 'yard-hermes,"10.0.0.9 (eth0)\n172.17.0.1 (docker0)"\nyard,\n' > "$MOCK_ROWS.n4"

# shellcheck source=scripts/lib-power.sh
. "$ROOT/scripts/lib-power.sh"

[ -z "$(power_state subyard yard)" ] || fail 'a prefix-matched neighbour stood in for an absent yard'
printf 'yard,STOPPED\n' >> "$MOCK_ROWS.ns"
[ "$(power_state subyard yard)" = STOPPED ] || fail "exact status = $(power_state subyard yard)"
[ "$(power_state subyard yard-hermes)" = RUNNING ] || fail 'the neighbour lost its own status'
[ "$(incus_instance_columns subyard yard s 5s)" = STOPPED ] || fail 'bounded status read differs'
[ -z "$(incus_instance_columns subyard yard 4)" ] || fail "the neighbour's address stood in for the yard's"
[ "$(incus_instance_columns subyard yard-hermes 4)" = '"10.0.0.9 (eth0)' ] || fail 'the first address line is missing'
printf x > "$MOCK_ROWS.fail"
rc=0
power_state subyard yard >/dev/null || rc=$?
[ "$rc" != 0 ] || fail 'a failed Incus read became an absent instance'
rm -f "$MOCK_ROWS.fail"

# Product scripts do not read one instance's status or address by prefix.
if grep -rnE 'incus list "\$(YARD_INSTANCE_NAME|instance|INSTANCE)"' \
  "$ROOT/scripts" "$ROOT/config/profiles"/*/owner.sh "$ROOT/config/profiles"/*/resources; then
  fail 'a product script reads an instance by prefix; use power_state or incus_instance_columns'
fi
# Python and Go read one named instance only together with its name column.
python3 -I - "$ROOT" <<'PY' || fail 'a Python or Go call site reads an instance by prefix; list it with -c n... and match the name'
import pathlib, re, sys
root = pathlib.Path(sys.argv[1])
call = re.compile(r'''["']incus["'],\s*["']list["'],\s*[A-Za-z_][^,)]*,(?P<rest>[^)\]]*)''')
found = []
for path in sorted([*root.glob('config/**/*.py'), *root.glob('internal/**/*.go'), *root.glob('cmd/**/*.go')]):
    if path.name.endswith('_test.go') or '/tests/' in str(path):
        continue
    for match in call.finditer(path.read_text(errors='replace')):
        if not re.search(r'''["']-c["'],\s*["']n''', match.group('rest')):
            found.append(f"{path.relative_to(root)}: {match.group(0).splitlines()[0]}")
if found:
    print('\n'.join(found), file=sys.stderr)
sys.exit(1 if found else 0)
PY
printf 'ok: exact instance status and address\n'
