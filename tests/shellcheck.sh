#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fixture="$(mktemp -d /tmp/subyard-shellcheck.XXXXXX)"
trap 'rm -rf -- "$fixture"' EXIT
cd "$fixture"
mkdir -p bin completions scripts dev config/profiles config/agents tests
touch bin/yard completions/yard.bash
cat > bin/shellcheck <<'EOF'
#!/usr/bin/env bash
set -eu
[[ $# == 4 && $1 == -x && $2 == -S && $3 == warning && -f $4 ]] || exit 2
case "$4" in
  scripts/*)
    printf 'finding in %s\n' "$4"
    printf 'detail for %s\n' "$4" >&2
    exit 1
    ;;
esac
EOF
chmod 0700 bin/shellcheck
export PATH="$fixture/bin:$PATH"

output="$(bash "$ROOT/dev/shellcheck.sh")"
[[ -z "$output" ]] || exit 1
touch 'scripts/z warning.sh' scripts/a.sh
if output="$(bash "$ROOT/dev/shellcheck.sh" 2>&1)"; then
  printf 'expected ShellCheck findings to fail the check\n' >&2
  exit 1
fi
diff -u - <(printf '%s\n' "$output") <<'EOF'
finding in scripts/a.sh
detail for scripts/a.sh
finding in scripts/z warning.sh
detail for scripts/z warning.sh
EOF
