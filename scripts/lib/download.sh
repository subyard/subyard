#!/usr/bin/env bash
# download.sh — bounded, HTTPS-only atomic downloads for host provisioning.

[ -n "${SUBYARD_DOWNLOAD_SOURCED:-}" ] && return 0
SUBYARD_DOWNLOAD_SOURCED=1

_subyard_download_retryable() {
  local curl_status="${1:?curl status is required}" http_status="${2:-000}"
  if [ "$curl_status" -eq 22 ]; then
    case "$http_status" in
      408 | 429 | 500 | 502 | 503 | 504) return 0 ;;
      *) return 1 ;;
    esac
  fi
  case "$curl_status" in
    # Proxy/host lookup, connection, partial transfer, timeout, empty reply,
    # send/receive and HTTP/2 stream failures are safe to retry.
    5 | 6 | 7 | 18 | 28 | 52 | 55 | 56 | 92 | 124) return 0 ;;
    *) return 1 ;;
  esac
}

subyard_download_https_atomic() {
  local url="${1:-}" destination="${2:-}" mode="${3:-0644}"
  local owner="${4:-root}" group="${5:-root}"
  local attempts=4 connect_timeout=5 transfer_timeout=10 total_timeout=45
  local attempt=1 delay=1 started=$SECONDS elapsed remaining attempt_timeout
  local directory filename temporary="" http_status curl_status=1 retryable=0

  case "$url" in https://*) ;; *) return 2 ;; esac
  case "${url#https://}" in *@*) return 2 ;; esac
  case "$destination" in /*) ;; *) return 2 ;; esac
  case "$mode" in
    [0-7][0-7][0-7] | [0-7][0-7][0-7][0-7]) ;;
    *) return 2 ;;
  esac
  command -v curl >/dev/null 2>&1 || return 2
  command -v timeout >/dev/null 2>&1 || return 2

  directory="$(dirname -- "$destination")"
  filename="$(basename -- "$destination")"
  [ -d "$directory" ] || return 2
  temporary="$(mktemp "$directory/.${filename}.tmp.XXXXXX")" || return 1
  chmod 0600 "$temporary" || { rm -f -- "$temporary"; return 1; }

  while [ "$attempt" -le "$attempts" ]; do
    elapsed=$((SECONDS - started))
    remaining=$((total_timeout - elapsed))
    [ "$remaining" -gt 0 ] || { curl_status=124; break; }
    attempt_timeout=$transfer_timeout
    [ "$attempt_timeout" -le "$remaining" ] || attempt_timeout=$remaining
    : > "$temporary" || { rm -f -- "$temporary"; return 1; }
    if http_status="$(
      timeout --signal=TERM --kill-after=1s "${remaining}s" \
        curl --fail --silent --show-error --location --max-redirs 5 \
          --proto '=https' --proto-redir '=https' --tlsv1.2 \
          --connect-timeout "$connect_timeout" --max-time "$attempt_timeout" \
          --output "$temporary" --write-out '%{http_code}' "$url"
    )"; then
      curl_status=0
    else
      curl_status=$?
    fi
    case "$http_status" in
      [0-9][0-9][0-9]) ;;
      *) http_status=000 ;;
    esac

    if [ "$curl_status" -eq 0 ] && [ -s "$temporary" ]; then
      chmod "$mode" "$temporary" \
        && chown "$owner:$group" "$temporary" \
        && mv -fT -- "$temporary" "$destination" \
        && return 0
      rm -f -- "$temporary"
      return 1
    fi

    retryable=0
    if [ "$curl_status" -eq 0 ]; then
      # A successful but empty response is an incomplete download.
      retryable=1
    elif _subyard_download_retryable "$curl_status" "$http_status"; then
      retryable=1
    fi
    if [ "$retryable" -ne 1 ] || [ "$attempt" -eq "$attempts" ]; then
      break
    fi

    printf 'Subyard: HTTPS download failed (attempt %s/%s); retrying in %ss\n' \
      "$attempt" "$attempts" "$delay" >&2
    elapsed=$((SECONDS - started))
    remaining=$((total_timeout - elapsed))
    [ "$remaining" -gt 0 ] || { curl_status=124; break; }
    [ "$delay" -le "$remaining" ] || delay=$remaining
    sleep "$delay" || { rm -f -- "$temporary"; return 1; }
    delay=$((delay * 2))
    attempt=$((attempt + 1))
  done

  rm -f -- "$temporary"
  printf 'Subyard: HTTPS download failed after %s attempt(s)\n' "$attempt" >&2
  return 1
}
