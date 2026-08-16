#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RESOLVER="$ROOT/config/profiles/hermes/hermes-release-resolve.py"
TMP="$(mktemp -d)"
trap 'rm -rf -- "$TMP"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

[ -x "$RESOLVER" ] || fail 'stable-release resolver is missing or not executable'

write_fixture() {
  local name="$1" body="$2"
  printf '%s\n' "$body" >"$TMP/$name.json"
}
resolve_fixture() {
  HERMES_TEST_ROOT="$TMP" \
    HERMES_RELEASE_API_FILE="$TMP/$1.json" \
    "$RESOLVER"
}

write_fixture valid '{"tag_name":"v2026.8.13","html_url":"https://github.com/NousResearch/hermes-agent/releases/tag/v2026.8.13","draft":false,"prerelease":false,"published_at":"2026-08-13T10:00:00Z"}'
[ "$(resolve_fixture valid)" = $'v2026.8.13\t2026-08-13T10:00:00Z\thttps://github.com/NousResearch/hermes-agent/releases/tag/v2026.8.13' ] \
  || fail 'valid stable release was not parsed deterministically'

for pair in \
  'draft {"tag_name":"v2026.8.13","html_url":"https://github.com/NousResearch/hermes-agent/releases/tag/v2026.8.13","draft":true,"prerelease":false,"published_at":"2026-08-13T10:00:00Z"}' \
  'prerelease {"tag_name":"v2026.8.13-rc1","html_url":"https://github.com/NousResearch/hermes-agent/releases/tag/v2026.8.13-rc1","draft":false,"prerelease":true,"published_at":"2026-08-13T10:00:00Z"}' \
  'foreign {"tag_name":"v2026.8.13","html_url":"https://github.com/attacker/hermes-agent/releases/tag/v2026.8.13","draft":false,"prerelease":false,"published_at":"2026-08-13T10:00:00Z"}' \
  'option {"tag_name":"--upload-pack=x","html_url":"https://github.com/NousResearch/hermes-agent/releases/tag/--upload-pack=x","draft":false,"prerelease":false,"published_at":"2026-08-13T10:00:00Z"}' \
  'slash {"tag_name":"refs/heads/main","html_url":"https://github.com/NousResearch/hermes-agent/releases/tag/refs/heads/main","draft":false,"prerelease":false,"published_at":"2026-08-13T10:00:00Z"}' \
  'badtime {"tag_name":"v2026.8.13","html_url":"https://github.com/NousResearch/hermes-agent/releases/tag/v2026.8.13","draft":false,"prerelease":false,"published_at":"today"}' \
  'malformed {'; do
  name="${pair%% *}"
  write_fixture "$name" "${pair#* }"
  if resolve_fixture "$name" >"$TMP/$name.out" 2>&1; then
    fail "unsafe release fixture $name was accepted"
  fi
  if grep -Fq 'tag_name' "$TMP/$name.out"; then
    fail "unsafe fixture $name was dumped to diagnostics"
  fi
done

if HERMES_RELEASE_API_FILE="$TMP/valid.json" "$RESOLVER" >"$TMP/injection.out" 2>&1; then
  fail 'fixture injection was accepted outside explicit test mode'
fi

PYTHONDONTWRITEBYTECODE=1 RESOLVER_PATH="$RESOLVER" python3 - <<'PY'
from __future__ import annotations

import importlib.util
import os
import sys
import threading
import time
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

path = os.environ["RESOLVER_PATH"]
spec = importlib.util.spec_from_file_location("hermes_release_resolver", path)
assert spec is not None and spec.loader is not None
resolver = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = resolver
spec.loader.exec_module(resolver)


class Handler(BaseHTTPRequestHandler):
    counts: dict[str, int] = {}
    authorization: list[str | None] = []

    def log_message(self, _format: str, *args: object) -> None:
        return

    def do_GET(self) -> None:
        self.counts[self.path] = self.counts.get(self.path, 0) + 1
        self.authorization.append(self.headers.get("Authorization"))
        count = self.counts[self.path]
        if self.path == "/sequence" and count == 1:
            self.send_response(429)
            self.send_header("Retry-After", "0")
        elif self.path == "/sequence" and count == 2:
            self.send_response(503)
        elif self.path == "/rate403" and count == 1:
            self.send_response(403)
            self.send_header("Retry-After", "0")
        elif self.path == "/reset" and count == 1:
            self.send_response(403)
            self.send_header("X-RateLimit-Remaining", "0")
            self.send_header("X-RateLimit-Reset", str(int(time.time()) + 1))
        elif self.path == "/exhaust":
            self.send_response(500)
        elif self.path == "/long":
            self.send_response(429)
            self.send_header("Retry-After", str(resolver.MAX_RETRY_DELAY + 1))
        elif self.path == "/reset-long":
            self.send_response(403)
            self.send_header("X-RateLimit-Remaining", "0")
            self.send_header(
                "X-RateLimit-Reset", str(int(time.time()) + resolver.MAX_RETRY_DELAY + 1)
            )
        elif self.path == "/bare429":
            self.send_response(429)
        elif self.path == "/redirect":
            self.send_response(302)
            self.send_header("Location", "/target")
        else:
            self.send_response(200)
        self.end_headers()
        if self.path not in {"/sequence", "/rate403", "/reset", "/exhaust", "/long", "/reset-long", "/bare429", "/redirect"} \
          or (self.path == "/sequence" and count >= 3) \
          or (self.path in {"/rate403", "/reset"} and count >= 2):
            self.wfile.write(b"{}")


server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
thread = threading.Thread(target=server.serve_forever, daemon=True)
thread.start()
base = f"http://127.0.0.1:{server.server_port}"
resolver.API_OPENER = urllib.request.build_opener(
    urllib.request.ProxyHandler({}), resolver._RejectRedirects()
)
resolver.time.sleep = lambda _seconds: None
for name in ("HERMES_TEST_ROOT", "HERMES_RELEASE_API_FILE"):
    os.environ.pop(name, None)
os.environ["GITHUB_TOKEN"] = "token-sentinel-must-not-leak"

try:
    assert resolver._read_api(base + "/sequence", "UNSET_FIXTURE", "sequence") == b"{}"
    assert Handler.counts["/sequence"] == 3
    assert resolver._read_api(base + "/rate403", "UNSET_FIXTURE", "rate403") == b"{}"
    assert Handler.counts["/rate403"] == 2
    assert resolver._read_api(base + "/reset", "UNSET_FIXTURE", "reset") == b"{}"
    assert Handler.counts["/reset"] == 2

    try:
        resolver._read_api(base + "/exhaust", "UNSET_FIXTURE", "exhaust")
    except ValueError:
        pass
    else:
        raise AssertionError("retry exhaustion was accepted")
    assert Handler.counts["/exhaust"] == resolver.MAX_ATTEMPTS

    try:
        resolver._read_api(base + "/long", "UNSET_FIXTURE", "long")
    except ValueError:
        pass
    else:
        raise AssertionError("long Retry-After was retried early")
    assert Handler.counts["/long"] == 1

    for endpoint in ("/reset-long", "/bare429"):
        try:
            resolver._read_api(base + endpoint, "UNSET_FIXTURE", endpoint.removeprefix("/"))
        except ValueError:
            pass
        else:
            raise AssertionError(f"{endpoint} was retried before its safe retry window")
        assert Handler.counts[endpoint] == 1

    try:
        resolver._read_api(base + "/redirect", "UNSET_FIXTURE", "redirect")
    except ValueError as exc:
        assert "token-sentinel-must-not-leak" not in str(exc)
    else:
        raise AssertionError("redirect was accepted")
    assert Handler.counts["/redirect"] == 1
    assert Handler.counts.get("/target", 0) == 0
    assert all(value is None for value in Handler.authorization)
finally:
    server.shutdown()
    server.server_close()
    thread.join(timeout=5)
PY

printf 'ok: Hermes stable-release resolver rejects draft, prerelease and foreign metadata\n'
