#!/usr/bin/env python3
"""Resolve the latest non-draft Hermes GitHub Release without shell JSON parsing."""

from __future__ import annotations

import json
import math
import os
import re
import sys
import time
import urllib.error
import urllib.request
from dataclasses import dataclass
from datetime import datetime
from pathlib import Path

API_URL = "https://api.github.com/repos/NousResearch/hermes-agent/releases/latest"
RELEASE_URL_PREFIX = "https://github.com/NousResearch/hermes-agent/releases/tag/"
SAFE_TAG = re.compile(r"^v[0-9]{4}\.[0-9]{1,2}\.[0-9]{1,2}(?:\.[0-9]+)?$")
MAX_ATTEMPTS = 4
MAX_RETRY_DELAY = 30


class _RejectRedirects(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):  # noqa: ANN001
        return None


API_OPENER = urllib.request.build_opener(_RejectRedirects())


@dataclass(frozen=True)
class Release:
    tag: str
    published_at: str
    html_url: str


def parse_latest_release(payload: bytes) -> Release:
    try:
        value = json.loads(payload)
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ValueError("latest-release response is not valid JSON") from exc
    if not isinstance(value, dict):
        raise ValueError("latest-release response is not an object")
    if value.get("draft") is not False or value.get("prerelease") is not False:
        raise ValueError("latest release is draft or prerelease")
    tag = value.get("tag_name")
    html_url = value.get("html_url")
    published_at = value.get("published_at")
    if not isinstance(tag, str) or not SAFE_TAG.fullmatch(tag):
        raise ValueError("latest release has an unsafe tag")
    if html_url != RELEASE_URL_PREFIX + tag:
        raise ValueError("latest release URL is outside the official repository")
    if not isinstance(published_at, str):
        raise ValueError("latest release has no publication timestamp")
    try:
        parsed_time = datetime.fromisoformat(published_at.replace("Z", "+00:00"))
    except ValueError as exc:
        raise ValueError("latest release has an invalid publication timestamp") from exc
    if parsed_time.tzinfo is None:
        raise ValueError("latest release timestamp has no timezone")
    return Release(tag=tag, published_at=published_at, html_url=html_url)


def _read_fixture(name: str) -> bytes | None:
    fixture = os.environ.get(name)
    if not fixture:
        return None
    test_root = os.environ.get("HERMES_TEST_ROOT")
    if not test_root:
        raise ValueError("release fixture injection is test-only")
    root = Path(test_root).resolve(strict=True)
    path = Path(fixture).resolve(strict=True)
    if path != root and root not in path.parents:
        raise ValueError("release fixture escapes the test root")
    return path.read_bytes()


def _read_api(url: str, fixture_name: str, label: str) -> bytes:
    fixture = _read_fixture(fixture_name)
    if fixture is not None:
        return fixture
    if os.environ.get("HERMES_TEST_ROOT"):
        raise ValueError(f"{label} fixture is missing in test mode")
    headers = {
        "Accept": "application/vnd.github+json",
        "User-Agent": "Subyard-Hermes-bootstrap",
        "X-GitHub-Api-Version": "2022-11-28",
    }
    request = urllib.request.Request(
        url,
        headers=headers,
    )
    last_error: BaseException | None = None
    for attempt in range(MAX_ATTEMPTS):
        try:
            with API_OPENER.open(request, timeout=20) as response:
                if response.geturl() != url:
                    raise ValueError(f"{label} API redirected unexpectedly")
                if response.status != 200:
                    raise ValueError(f"{label} API returned an error")
                return response.read(1024 * 1024 + 1)
        except urllib.error.HTTPError as exc:
            last_error = exc
            delay = _retry_delay(exc, attempt)
            if delay is None or delay > MAX_RETRY_DELAY:
                break
        except (urllib.error.URLError, TimeoutError) as exc:
            last_error = exc
            delay = 2**attempt
        if attempt < MAX_ATTEMPTS - 1:
            time.sleep(delay)
    raise ValueError(f"{label} API is unavailable") from last_error


def _nonnegative_header_int(headers, name: str) -> int | None:  # noqa: ANN001
    raw = headers.get(name)
    if raw is None:
        return None
    try:
        value = int(raw)
    except ValueError:
        return None
    return value if value >= 0 else None


def _retry_delay(exc: urllib.error.HTTPError, attempt: int) -> int | None:
    if 500 <= exc.code <= 599:
        return 2**attempt
    rate_limited = exc.code == 429 or (
        exc.code == 403
        and (
            exc.headers.get("Retry-After") is not None
            or exc.headers.get("X-RateLimit-Remaining") == "0"
        )
    )
    if not rate_limited:
        return None
    retry_after = _nonnegative_header_int(exc.headers, "Retry-After")
    if retry_after is not None:
        return retry_after
    reset_at = _nonnegative_header_int(exc.headers, "X-RateLimit-Reset")
    if reset_at is not None:
        return max(0, math.ceil(reset_at - time.time()))
    return None


def main() -> int:
    try:
        payload = _read_api(API_URL, "HERMES_RELEASE_API_FILE", "latest-release")
        if len(payload) > 1024 * 1024:
            raise ValueError("latest-release response is too large")
        release = parse_latest_release(payload)
    except (OSError, ValueError) as exc:
        print(f"hermes release resolver: {exc}", file=sys.stderr)
        return 1
    print(f"{release.tag}\t{release.published_at}\t{release.html_url}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
