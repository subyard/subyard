#!/usr/bin/env python3
"""Exercise an installed Orca desktop through its loopback DevTools endpoint."""

from __future__ import annotations

import argparse
import json
import os
import re
import stat
import sys
import time
import urllib.request
from pathlib import Path
from typing import Any

import websocket


MAX_PROTECTED_FILE_BYTES = 1024 * 1024
DEVTOOLS_PATTERN = re.compile(r"DevTools listening on ws://127\.0\.0\.1:(\d+)/")


class StageFailure(Exception):
    def __init__(self, stage: str, summary: str = "") -> None:
        super().__init__(stage)
        self.stage = stage
        self.summary = summary


class CDP:
    def __init__(self, url: str, deadline: float) -> None:
        self.deadline = deadline
        self.next_id = 1
        self.socket = websocket.create_connection(url, timeout=self._remaining(), suppress_origin=True)

    def _remaining(self) -> float:
        remaining = self.deadline - time.monotonic()
        if remaining <= 0:
            raise StageFailure("deadline")
        return remaining

    def call(self, method: str, params: dict[str, Any] | None = None) -> dict[str, Any]:
        request_id = self.next_id
        self.next_id += 1
        payload: dict[str, Any] = {"id": request_id, "method": method}
        if params is not None:
            payload["params"] = params
        self.socket.settimeout(self._remaining())
        self.socket.send(json.dumps(payload, separators=(",", ":")))
        while True:
            self.socket.settimeout(self._remaining())
            response = json.loads(self.socket.recv())
            if response.get("id") != request_id:
                continue
            if "error" in response:
                raise StageFailure("devtools-command")
            result = response.get("result")
            if not isinstance(result, dict):
                raise StageFailure("devtools-response")
            return result

    def close(self) -> None:
        self.socket.close()


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--log", required=True, type=Path)
    parser.add_argument("--pairing-file", required=True, type=Path)
    parser.add_argument("--expected-repo-label", required=True)
    parser.add_argument("--expected-repo-path", required=True)
    parser.add_argument("--timeout", type=float, default=45.0)
    return parser.parse_args()


def read_protected(path: Path, limit: int) -> str:
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or stat.S_ISLNK(info.st_mode):
        raise StageFailure("protected-input")
    if info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o600:
        raise StageFailure("protected-input")
    if info.st_size <= 0 or info.st_size > limit:
        raise StageFailure("protected-input")
    return path.read_text(encoding="utf-8")


def discover_page(log_path: Path, deadline: float) -> str:
    port: int | None = None
    while time.monotonic() < deadline:
        try:
            text = read_protected(log_path, MAX_PROTECTED_FILE_BYTES)
        except (FileNotFoundError, StageFailure):
            time.sleep(0.1)
            continue
        match = DEVTOOLS_PATTERN.search(text)
        if match:
            port = int(match.group(1))
            break
        time.sleep(0.1)
    if port is None:
        raise StageFailure("devtools-discovery")

    endpoint = f"http://127.0.0.1:{port}/json/list"
    while time.monotonic() < deadline:
        try:
            with urllib.request.urlopen(endpoint, timeout=2) as response:
                targets = json.load(response)
            for target in targets:
                url = target.get("webSocketDebuggerUrl")
                if target.get("type") == "page" and isinstance(url, str):
                    if url.startswith(f"ws://127.0.0.1:{port}/devtools/page/"):
                        return url
        except (OSError, ValueError, TypeError):
            pass
        time.sleep(0.1)
    raise StageFailure("page-discovery")


def wait_for_api(cdp: CDP, deadline: float, require_reloaded: bool = False) -> str:
    while time.monotonic() < deadline:
        try:
            result = cdp.call(
                "Runtime.evaluate",
                {
                    "expression": "globalThis",
                    "returnByValue": False,
                },
            )
            remote = result.get("result", {})
            object_id = remote.get("objectId")
            if not isinstance(object_id, str):
                time.sleep(0.1)
                continue
            ready = cdp.call(
                "Runtime.callFunctionOn",
                {
                    "objectId": object_id,
                    "functionDeclaration": (
                        "function(){return typeof this.api === 'object' && "
                        "this.__subyardDesktopBeforeReload !== true}"
                        if require_reloaded else
                        "function(){return typeof this.api === 'object'}"
                    ),
                    "returnByValue": True,
                },
            )
            if ready.get("result", {}).get("value") is True:
                return object_id
        except StageFailure:
            pass
        time.sleep(0.1)
    raise StageFailure("renderer-ready")


def call_function(
    cdp: CDP,
    object_id: str,
    declaration: str,
    arguments: list[Any],
) -> Any:
    result = cdp.call(
        "Runtime.callFunctionOn",
        {
            "objectId": object_id,
            "functionDeclaration": declaration,
            "arguments": [{"value": value} for value in arguments],
            "awaitPromise": True,
            "returnByValue": True,
        },
    )
    if "exceptionDetails" in result:
        raise StageFailure("renderer-call")
    return result.get("result", {}).get("value")


def pair_and_verify(
    cdp: CDP,
    object_id: str,
    pairing_code: str,
    expected_repo_path: str,
) -> None:
    value = call_function(
        cdp,
        object_id,
        """async function(pairingCode, expectedRepoPath) {
          await this.api.onboarding.update({
            flowVersion: 4,
            closedAt: 1,
            outcome: 'completed',
            lastCompletedStep: 5
          });
          const added = await this.api.runtimeEnvironments.addFromPairingCode({
            name: 'subyard-vm',
            pairingCode
          });
          const environmentId = added?.environment?.id;
          const status = environmentId
            ? await this.api.runtimeEnvironments.getStatus({selector: environmentId, timeoutMs: 10000})
            : null;
          const repos = environmentId
            ? await this.api.runtimeEnvironments.call({
                selector: environmentId,
                method: 'repo.list',
                timeoutMs: 10000
              })
            : null;
          const repoRows = Array.isArray(repos?.result?.repos) ? repos.result.repos : [];
          if (environmentId) {
            await this.api.settings.setActiveRuntimeEnvironmentPreference({environmentId});
          }
          const settings = await this.api.settings.get();
          return {
            added: typeof environmentId === 'string' && environmentId.length > 0,
            statusOk: status?.ok === true,
            reposOk: repos?.ok === true,
            repoPresent: repoRows.filter((repo) => repo?.path === expectedRepoPath).length === 1,
            activePersisted: settings?.activeRuntimeEnvironmentId === environmentId
          };
        }""",
        [pairing_code, expected_repo_path],
    )
    keys = ("added", "statusOk", "reposOk", "repoPresent", "activePersisted")
    if not isinstance(value, dict) or not all(value.get(key) is True for key in keys):
        summary = " ".join(f"{key}={isinstance(value, dict) and value.get(key) is True}" for key in keys)
        raise StageFailure("pairing", summary)


def reload_and_verify_sidebar(
    cdp: CDP,
    previous_object_id: str,
    expected_repo_label: str,
    deadline: float,
) -> str:
    # Remote object handles can change without a navigation. Mark the old
    # global object so only a newly loaded renderer can satisfy the assertion.
    call_function(cdp, previous_object_id,
                  "function(){this.__subyardDesktopBeforeReload = true; return true}", [])
    cdp.call("Page.enable")
    cdp.call("Page.reload", {"ignoreCache": True})
    object_id = wait_for_api(cdp, deadline, require_reloaded=True)
    value: Any = None
    while time.monotonic() < deadline:
        try:
            value = call_function(
                cdp,
                object_id,
                """function(expectedRepoLabel) {
                  const sidebar = document.querySelector('[data-worktree-sidebar]');
                  return {
                    sidebarVisible: Boolean(sidebar?.getClientRects().length &&
                      getComputedStyle(sidebar).visibility !== 'hidden'),
                    repoRendered: Boolean(sidebar?.textContent?.includes(expectedRepoLabel))
                  };
                }""",
                [expected_repo_label],
            )
        except StageFailure:
            object_id = wait_for_api(cdp, deadline, require_reloaded=True)
            continue
        if isinstance(value, dict) and value.get("sidebarVisible") is True and value.get("repoRendered") is True:
            return object_id
        time.sleep(0.1)
    summary = "sidebarVisible=false repoRendered=false"
    if isinstance(value, dict):
        summary = (
            f"sidebarVisible={value.get('sidebarVisible') is True} "
            f"repoRendered={value.get('repoRendered') is True}"
        )
    raise StageFailure("sidebar", summary)


def request_close(cdp: CDP, object_id: str) -> None:
    value = call_function(
        cdp,
        object_id,
        "function(){setTimeout(() => this.api.ui.requestClose(), 100); return true}",
        [],
    )
    if value is not True:
        raise StageFailure("close-request")


def main() -> int:
    args = parse_args()
    deadline = time.monotonic() + args.timeout
    stage = "startup"
    cdp: CDP | None = None
    try:
        pairing_code = read_protected(args.pairing_file, 16 * 1024).strip()
        if not pairing_code.startswith("orca://pair?code="):
            raise StageFailure("protected-input")
        stage = "devtools"
        cdp = CDP(discover_page(args.log, deadline), deadline)
        object_id = wait_for_api(cdp, deadline)
        stage = "pairing"
        pair_and_verify(
            cdp,
            object_id,
            pairing_code,
            args.expected_repo_path,
        )
        pairing_code = ""
        stage = "sidebar"
        object_id = reload_and_verify_sidebar(
            cdp,
            object_id,
            args.expected_repo_label,
            deadline,
        )
        stage = "close-request"
        request_close(cdp, object_id)
        return 0
    except StageFailure as error:
        summary = f" {error.summary}" if error.summary else ""
        print(f"orca-desktop: {error.stage} failed{summary}", file=sys.stderr)
        return 1
    except Exception:
        print(f"orca-desktop: {stage} failed", file=sys.stderr)
        return 1
    finally:
        if cdp is not None:
            try:
                cdp.close()
            except Exception:
                pass


if __name__ == "__main__":
    raise SystemExit(main())
