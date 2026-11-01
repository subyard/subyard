#!/usr/bin/env bash
# Source engine freshness follows production inputs, not test-only Go files.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fixture="$TMP/repository"
no_go_path="$TMP/no-go-bin"

install -d "$fixture/scripts" "$fixture/cmd/yard" "$fixture/internal/sample" "$fixture/.build" "$no_go_path"
for command in bash basename dirname find grep readlink uname; do
  ln -s "$(command -v "$command")" "$no_go_path/$command"
done
install -m 0755 "$ROOT/dev/build-engine.sh" "$fixture/scripts/build-engine.sh"
printf 'module example.invalid/fixture\n\ngo 1.26.0\n' > "$fixture/go.mod"
: > "$fixture/go.sum"
printf 'package main\nfunc main() {}\n' > "$fixture/cmd/yard/main.go"
printf 'package sample\n' > "$fixture/internal/sample/sample_test.go"
cat > "$fixture/.build/yard" <<'SH'
#!/usr/bin/env bash
case "$1" in
  --version) printf 'yard 0.1.0-dev\n' ;;
  _release-owner-variant-v1) printf 'go\n' ;;
  *) exit 2 ;;
esac
SH
chmod +x "$fixture/.build/yard"

touch -r "$fixture/.build/yard" -d '+1 second' "$fixture/internal/sample/sample_test.go"
PATH="$no_go_path" /bin/bash "$fixture/scripts/build-engine.sh"

if PATH="$no_go_path" /bin/bash "$fixture/scripts/build-engine.sh" --experimental-release-owner >"$TMP/stdout" 2>"$TMP/stderr"; then
  printf 'build-engine: changed variant did not require a rebuild\n' >&2; exit 1
fi
grep -Fq 'Go is required' "$TMP/stderr"
sed -i "s/printf 'go/printf 'invalid/" "$fixture/.build/yard"
if PATH="$no_go_path" /bin/bash "$fixture/scripts/build-engine.sh" >"$TMP/stdout" 2>"$TMP/stderr"; then
  printf 'build-engine: invalid compiled variant did not require a rebuild\n' >&2; exit 1
fi
grep -Fq 'Go is required' "$TMP/stderr"
sed -i "s/printf 'invalid/printf 'go/" "$fixture/.build/yard"

touch -r "$fixture/.build/yard" -d '+1 second' "$fixture/cmd/yard/main.go"
if PATH="$no_go_path" /bin/bash "$fixture/scripts/build-engine.sh" >"$TMP/stdout" 2>"$TMP/stderr"; then
  printf 'build-engine: production source change did not require a rebuild\n' >&2
  exit 1
fi
grep -Fq 'Go is required' "$TMP/stderr" \
  || { printf 'build-engine: missing-Go diagnostic regressed\n' >&2; exit 1; }

mkdir -p "$TMP/fake-go-bin"
cat > "$TMP/fake-go-bin/go" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[ "$PWD" = "$EXPECTED_BUILD_CWD" ] || exit 71
while [ $# -gt 0 ]; do
  if [ "$1" = -o ]; then : > "$2"; exit 0; fi
  shift
done
exit 72
SH
chmod +x "$TMP/fake-go-bin/go"
(cd "$TMP" && PATH="$TMP/fake-go-bin:$PATH" EXPECTED_BUILD_CWD="$ROOT" \
  "$ROOT/dev/build-engine.sh" --force --output "$TMP/yard")
[ -x "$TMP/yard" ] \
  || { printf 'build-engine: invocation outside the repository did not produce an engine\n' >&2; exit 1; }
[ "$(stat -c '%a' "$TMP")" = 700 ] \
  || { printf 'build-engine: output parent permissions changed\n' >&2; exit 1; }

install -m 0755 "$ROOT/.build/yard" "$TMP/release-engine"
expected_version="$("$ROOT/.build/yard" --version)"
[ "$(PATH="$no_go_path" YARD_ENGINE_PATH="$TMP/release-engine" "$ROOT/bin/yard" --version)" = "$expected_version" ] \
  || { printf 'build-engine: explicit release engine did not run without Go\n' >&2; exit 1; }

[ "$(PATH="$no_go_path" "$ROOT/.build/yard" _release-owner-variant-v1)" = go ]
"$ROOT/dev/package-engine.sh" --runtime-dir "$TMP/runtime-go" --engine "$ROOT/.build/yard" --version "${expected_version#* }" >"$TMP/stage-go.stdout" 2>"$TMP/stage-go.stderr"
[ -x "$TMP/runtime-go/current/bin/yard-engine" ]

if "$ROOT/dev/package-engine.sh" --experimental-release-owner --runtime-dir "$TMP/runtime" --engine "$ROOT/.build/yard" --version "${expected_version#* }" >"$TMP/stdout" 2>"$TMP/stderr"; then
  printf 'build-engine: Go engine admitted as experimental Rust tooling\n' >&2; exit 1
fi
grep -Fq 'prebuilt engine compiled release owner differs' "$TMP/stderr" \
  || { cat "$TMP/stderr" >&2; exit 1; }
[ ! -e "$TMP/runtime" ] || { printf 'build-engine: mismatched engine published a tooling stage\n' >&2; exit 1; }

printf 'ok: engine freshness is source-aware and the release launcher runs without Go\n'
