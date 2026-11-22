#!/usr/bin/env bash
set -euo pipefail
test_root=$(mktemp -d /tmp/subyard-herdr-install.XXXXXX)
trap 'rm -rf -- "$test_root"' EXIT
mkdir "$test_root/home" "$test_root/bin" "$test_root/archive"
export PATH="$test_root/bin:$PATH"
export HERDR_FIXTURE_ARCHIVE="$test_root/release.tar.gz"
export HERDR_FIXTURE_CALLS="$test_root/calls"
cat > "$test_root/bin/curl" <<'EOF'
#!/usr/bin/env bash
set -eu
printf 'download\n' >> "$HERDR_FIXTURE_CALLS"
cp "$HERDR_FIXTURE_ARCHIVE" "${@: -1}"
EOF
cat > "$test_root/archive/herdr-core" <<'EOF'
#!/usr/bin/env bash
set -eu
[ "$1" = --version ]
printf 'hc 0.1.21\n'
EOF
chmod 0755 "$test_root/bin/curl" "$test_root/archive/herdr-core"
tar -czf "$HERDR_FIXTURE_ARCHIVE" -C "$test_root/archive" herdr-core
digest=$(sha256sum "$HERDR_FIXTURE_ARCHIVE" | cut -d ' ' -f1)
payload=config/profiles/herdr/resources/herdr/install-core.sh
login="$test_root/bin/login"
cat > "$login" <<'EOF'
#!/usr/bin/env bash
set -eu
[ "$1" = update ] && [ "$2" = --from ] && [ "$#" -eq 3 ]
printf 'swap\n' >> "$HERDR_FIXTURE_CALLS"
mv -T "$3" "$HOME/.local/bin/herdr-core"
EOF
chmod 0755 "$login"
HOME="$test_root/home" bash "$payload" 0.1.21 1 "$digest" minimum "$login"
HOME="$test_root/home" bash "$payload" 0.1.21 1 "$digest" check "$login"
HOME="$test_root/home" bash "$payload" 0.1.20 1 "$digest" minimum "$login"
[ "$(readlink "$test_root/home/.local/bin/hc")" = herdr-core ]
[ "$(wc -l < "$HERDR_FIXTURE_CALLS")" -eq 1 ]
mkdir "$test_root/bad"
if HOME="$test_root/bad" bash "$payload" 0.1.21 1 "$(printf '%064d' 0)" minimum "$login"; then
  printf 'Herdr installer accepted a bad release checksum\n' >&2
  exit 1
fi
[ ! -e "$test_root/bad/.local/bin/herdr-core" ]
sed -i 's/hc 0.1.21/hc 0.1.20/' "$test_root/home/.local/bin/herdr-core"
HOME="$test_root/home" bash "$payload" 0.1.21 1 "$digest" minimum "$login"
HOME="$test_root/home" bash "$payload" 0.1.21 1 "$digest" check "$login"
grep -qx swap "$HERDR_FIXTURE_CALLS"
