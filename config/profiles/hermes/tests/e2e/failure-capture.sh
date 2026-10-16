#!/usr/bin/env bash
# Private, bounded diagnostics; never send their contents to the terminal.
hermes_capture_failure() {
  local directory="$1" original="$2" unit="subyard-github-$YARD.service" uid capture_rc=0
  [ ! -e "$directory" ] && [ ! -L "$directory" ] || return 2
  install -d -m 0700 "$directory" || return 2
  local name
  for name in unit journal client probes result; do
    install -m 0600 /dev/null "$directory/$name" || return 2
  done
  uid="$(id -u)"
  hermes_capture_probe "$directory" unit env XDG_RUNTIME_DIR="/run/user/$uid" \
    DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$uid/bus" systemctl --user show "$unit" \
    -p LoadState -p ActiveState -p SubState -p Result -p ExecMainStatus -p NRestarts \
    || capture_rc=1
  hermes_capture_probe "$directory" journal journalctl --user -u "$unit" \
    --no-pager -n 80 -o short-precise || capture_rc=1
  # Resolve these before teardown, including failures before normal fixture discovery.
  local instance project
  instance="$(timeout --signal=TERM --kill-after=2 10 "$YARD_BIN" -Y "$YARD" \
    config show YARD_INSTANCE_NAME 2>/dev/null | sed -n 's/^effective: //p')" || capture_rc=1
  project="$(timeout --signal=TERM --kill-after=2 10 "$YARD_BIN" -Y "$YARD" \
    config show INCUS_PROJECT 2>/dev/null | sed -n 's/^effective: //p')" || capture_rc=1
  hermes_capture_probe "$directory" client incus exec "$instance" --project "$project" -- \
    runuser -u dev -- /usr/local/bin/subyard-github status --runtime || capture_rc=1
  printf 'original_exit_code=%s\ncapture_exit_code=%s\n' "$original" "$capture_rc" >"$directory/result"
  return "$capture_rc"
}

hermes_capture_probe() {
  local directory="$1" name="$2"
  shift 2
  local size
  local -a codes
  timeout --signal=TERM --kill-after=2 10 "$@" 2>&1 | head -c 65536 >"$directory/$name"
  codes=("${PIPESTATUS[@]}")
  size="$(stat -c %s "$directory/$name")"
  printf '%s\t%s\t%s\t%s\n' "$name" "${codes[0]}" "${codes[1]}" "$size" >>"$directory/probes"
  [ "${codes[0]}" = 0 ] && [ "${codes[1]}" = 0 ] && [ "$size" -lt 65536 ]
}

hermes_retain_failure() {
  local source="$1" output="$2" rc=0 command
  local -a codes
  install -m 0600 /dev/null "$output/failure.tar" || return 2
  install -m 0600 /dev/null "$output/capture-transport.log" || return 2
  # The fixed allowlist excludes config, environment, credentials and arbitrary paths.
  command="$(quote_ssh_command timeout --signal=TERM --kill-after=2 15 bash -euc '
    test -d "$1" && test ! -L "$1"
    test "$(stat -c %a "$1")" = 700
    for name in unit journal client probes result; do
      path="$1/$name"
      test -f "$path" && test ! -L "$path"
      test "$(stat -c %a "$path")" = 600
      test "$(stat -c %s "$path")" -le 65536
    done
    exec tar -C "$1" -cf - unit journal client probes result
  ' _ "$source")" || return 2
  # Bound the native local transport too: remote timeout cannot stop stalled SSH.
  timeout --signal=TERM --kill-after=2 20 ssh -F "$CLIENT_CONFIG" -T e2e-vm-1 -- "$command" \
    2>"$output/capture-transport.log" | head -c 220000 >"$output/failure.tar"
  codes=("${PIPESTATUS[@]}")
  [ "${codes[0]}" = 0 ] && [ "${codes[1]}" = 0 ] || rc=1
  # Validate the archive without extracting or printing private original bytes.
  python3 - "$output/failure.tar" <<'PY' || rc=1
import sys
import tarfile
try:
    with tarfile.open(sys.argv[1]) as archive:
        members = archive.getmembers()
        assert sorted(item.name for item in members) == ['client', 'journal', 'probes', 'result', 'unit']
        assert all(item.isfile() and item.mode == 0o600 and item.size <= 65536 for item in members)
        result = archive.extractfile('result').read().decode('ascii')
        assert 'capture_exit_code=0\n' in result
except (OSError, tarfile.TarError, AssertionError, UnicodeError):
    sys.exit(1)
PY
  return "$rc"
}
