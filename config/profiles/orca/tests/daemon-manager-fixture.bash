# shellcheck shell=bash
# Execute the real guest scope script against a small user-manager model.
daemon_manager_fixture() {
  export ORCA_MANAGER_FIXTURE_ROOT="$1"
  id() { [ "$*" = '-u dev' ] && printf '1234\n'; }
  systemctl() {
    local root="$ORCA_MANAGER_FIXTURE_ROOT"
    printf 'manager %s\n' "$*" >>"$root/commands.log"
    case "$*" in
      'show user@1234.service --property=ActiveState --value')
        [ ! -e "$root/fail-manager-state" ] || return 1
        if [ -e "$root/no-user-manager" ]; then printf 'inactive\n'; else printf 'active\n'; fi ;;
      '--user list-units --all --plain --no-legend --type=scope,slice --state=active,activating,reloading,deactivating orca-daemon-*.scope orca_pty_orca_daemon_*.scope orca_ptys_orca_daemon_*.slice')
        [ ! -e "$root/fail-user-bus" ] || return 1
        if [ -e "$root/daemon-scope" ]; then
          printf 'orca-daemon-fixture.scope loaded active running fixture\n'
          printf 'orca_pty_orca_daemon_fixture_1.scope loaded active running fixture\n'
          printf 'orca_ptys_orca_daemon_fixture.slice loaded active active fixture\n'
        fi ;;
      '--user stop -- orca-daemon-fixture.scope orca_pty_orca_daemon_fixture_1.scope orca_ptys_orca_daemon_fixture.slice')
        [ ! -e "$root/fail-scope-stop" ] || return 1
        [ -e "$root/ignore-scope-stop" ] || rm -f "$root/daemon-scope"
        [ ! -e "$root/fail-scope-verify" ] || touch "$root/fail-user-bus"
        [ ! -e "$root/collected-scope-stop" ] || return 1 ;;
      *) printf 'unexpected manager command: %s\n' "$*" >&2; return 1 ;;
    esac
  }
  runuser() {
    [ "$1 $2 $3 $4 $5 $6" = '-u dev -- env XDG_RUNTIME_DIR=/run/user/1234 DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1234/bus' ] || return 1
    shift 6
    "$@"
  }
  export -f id systemctl runuser
  /bin/bash -se -- dev "$2"
}

# Execute the real guest service scan over chosen live processes.
service_scan_fixture() {
  export ORCA_MANAGER_FIXTURE_ROOT="$1"
  systemctl() { printf '/fixture\n'; }
  find() {
    [ ! -e "$ORCA_MANAGER_FIXTURE_ROOT/fail-service-read" ] || return 1
    command cat "$ORCA_MANAGER_FIXTURE_ROOT/service-pids"
  }
  export -f systemctl find
  /bin/bash -se -- subyard-orca.service
}
