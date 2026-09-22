#!/usr/bin/env bash

start_loopback_sshd() { # temp-root sshd initial-port; sets port and sshd_pid
  local tmp="${1:?loopback sshd temp root is required}"
  local sshd="${2:?loopback sshd executable is required}"
  local candidate="${3:?loopback sshd initial port is required}"
  local expected scanned attempt previous
  expected="$(awk 'NF >= 2 { print $1 " " $2; exit }' "$tmp/host-key.pub")"
  [ -n "$expected" ] || return 1
  "$sshd" -t -f "$tmp/sshd_config" || return 1

  for attempt in 1 2 3 4 5; do
    : > "$tmp/sshd.log"
    : > "$tmp/known_hosts"
    "$sshd" -D -e -f "$tmp/sshd_config" -p "$candidate" \
      > "$tmp/sshd.log" 2>&1 &
    sshd_pid=$!
    for _ in $(seq 1 50); do
      kill -0 "$sshd_pid" 2>/dev/null || break
      if ssh-keyscan -T 1 -t ed25519 -p "$candidate" 127.0.0.1 \
        > "$tmp/known_hosts" 2>/dev/null; then
        scanned="$(awk '$2 == "ssh-ed25519" { print $2 " " $3; exit }' \
          "$tmp/known_hosts")"
        if [ "$scanned" = "$expected" ] && kill -0 "$sshd_pid" 2>/dev/null; then
          # The caller consumes this output global after the function returns.
          # shellcheck disable=SC2034
          printf -v port '%s' "$candidate"
          return 0
        fi
      fi
      sleep 0.1
    done

    if kill -0 "$sshd_pid" 2>/dev/null; then
      kill -TERM "$sshd_pid" 2>/dev/null || true
      wait "$sshd_pid" 2>/dev/null || true
      sshd_pid=''
      sed -n '1,80p' "$tmp/sshd.log" >&2
      return 1
    fi
    wait "$sshd_pid" 2>/dev/null || true
    sshd_pid=''
    grep -Fq 'Address already in use' "$tmp/sshd.log" || {
      sed -n '1,80p' "$tmp/sshd.log" >&2
      return 1
    }
    [ "$attempt" -lt 5 ] || break
    previous="$candidate"
    while [ "$candidate" = "$previous" ]; do
      candidate=$((42000 + RANDOM % 10000))
    done
  done
  sed -n '1,80p' "$tmp/sshd.log" >&2
  return 1
}
