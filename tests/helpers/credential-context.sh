#!/usr/bin/env bash
# Shared local credential fixtures; no core-suite assertions.

setup_credential_context() { # <temp-root>
  local fixture="${1:?}"
  # shellcheck source=tests/helpers/test-context.sh
  . "$ROOT/tests/helpers/test-context.sh"
  setup_test_context "$fixture"
  export HOME="$fixture/home"
  export SUBYARD_NO_AUDIT=1
  SUBYARD_KEYS_ROOT="$SUBYARD_CONFIG_HOME/key-hosts"
  export SUBYARD_KEYS_TOOLS_DIR="$SUBYARD_CONFIG_HOME/tools"
  export SUBYARD_KEYS_SYSTEMD_DIR="$fixture/systemd"
  export SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1
  export TMPDIR="$fixture/tmp"
  mkdir -p "$HOME" "$TMPDIR" "$SUBYARD_CONFIG_HOME/yards" "$SUBYARD_KEYS_TOOLS_DIR/bin"

  # Small deterministic test doubles preserve the CLI contract without CI network. Revision files
  # still contain no plaintext and are signed with real per-host OpenSSH signing identities.
  cat > "$SUBYARD_KEYS_TOOLS_DIR/bin/age" <<'SH'
#!/usr/bin/env bash
[ "${1:-}" = --version ] && { echo 'age 1.3.1'; exit 0; }
exit 2
SH
  cat > "$SUBYARD_KEYS_TOOLS_DIR/bin/age-keygen" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  --version) echo 'age-keygen 1.3.1' ;;
  -o)
    recipient="age1$(printf '%s-%s-%s' "$2" "$$" "$RANDOM" | sha256sum | cut -c1-58)"
    printf 'FAKE:%s\n' "$recipient" > "$2"
    printf 'Public key: %s\n' "$recipient" >&2 ;;
  -y) sed -n 's/^FAKE://p' "$2" ;;
  *) exit 2 ;;
esac
SH
  cat > "$SUBYARD_KEYS_TOOLS_DIR/bin/sops" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in --version) echo 'sops 3.13.2'; exit 0 ;; esac
mode="${1:-}"; shift || true
age_csv=''; input=''
while [ $# -gt 0 ]; do
  case "$1" in
    --age) age_csv="$2"; shift 2 ;;
    --encrypted-regex|--input-type|--output-type) shift 2 ;;
    -*) shift ;;
    *) input="$1"; shift ;;
  esac
done
[ -r "$input" ] || exit 2
case "$mode" in
  encrypt)
    payload="$(jq -r '.payload' "$input")"
    wrapped="$(printf '%s' "$payload" | base64 -w0)"
    jq --arg payload "ENC[$wrapped]" --arg recipients "$age_csv" '
      .payload=$payload |
      .sops={age:($recipients|split(",")|map({recipient:.,enc:"test-envelope"})),mac:"test-mac"}
    ' "$input" ;;
  decrypt)
    wrapped="$(jq -r '.payload' "$input")"
    case "$wrapped" in ENC\[*\]) ;; *) exit 1 ;; esac
    wrapped="${wrapped#ENC[}"; wrapped="${wrapped%]}"
    payload="$(printf '%s' "$wrapped" | base64 -d)"
    jq --arg payload "$payload" '.payload=$payload | del(.sops)' "$input" ;;
  *) exit 2 ;;
esac
SH
  chmod +x "$SUBYARD_KEYS_TOOLS_DIR/bin/age" "$SUBYARD_KEYS_TOOLS_DIR/bin/age-keygen" "$SUBYARD_KEYS_TOOLS_DIR/bin/sops"

  cat > "$SUBYARD_CONFIG_HOME/yards/one.env" <<EOF
SSH_PORT=3221
SUBYARD_KEYS_ROOT=$SUBYARD_KEYS_ROOT/one
SUBYARD_KEYS_CONSUMER_ROOT=$fixture/consumer-one
EOF
  cat > "$SUBYARD_CONFIG_HOME/yards/two.env" <<EOF
SSH_PORT=3222
SUBYARD_KEYS_ROOT=$SUBYARD_KEYS_ROOT/two
SUBYARD_KEYS_CONSUMER_ROOT=$fixture/consumer-two
EOF
  cat > "$SUBYARD_CONFIG_HOME/yards/one_alt.env" <<EOF
SSH_PORT=3226
SUBYARD_KEYS_ROOT=$SUBYARD_KEYS_ROOT/one
SUBYARD_KEYS_CONSUMER_ROOT=$fixture/consumer-one-alt
EOF

  yard_one() { "$ROOT/bin/yard" -Y one "$@"; }
  yard_two() { "$ROOT/bin/yard" -Y two "$@"; }
  yard_one_alt() { "$ROOT/bin/yard" -Y one_alt "$@"; }
  yard_three() { "$ROOT/bin/yard" -Y three "$@"; }
  yard_four() { "$ROOT/bin/yard" -Y four "$@"; }
  bootstrap_keys() {
    "$ROOT/bin/yard" -Y "$1" _keys-init
  }

}

assert_credential_outputs_private() {
  local output_file leaked
  while IFS= read -r output_file; do
    for leaked in "$@"; do
      if grep -Fq -- "$leaked" "$output_file" 2>/dev/null; then
        fail "plaintext appeared in output file $output_file"
      fi
    done
  done < <(find "$TMP" -maxdepth 1 -type f -name "*.out" -print)
  if find "$TMPDIR" -mindepth 1 -print -quit | grep -q .; then
    fail "key operations left files in the private temp root"
  fi
}
