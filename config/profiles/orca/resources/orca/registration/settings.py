"""Keep stock Orca launch defaults from overriding the yard's Codex config."""

import argparse
from pathlib import Path
import sys

from transport import RpcError, RuntimeRPC


YOLO = "--dangerously-bypass-approvals-and-sandbox"


def _arguments(rpc):
    result = rpc.call("settings.get")
    settings = result.get("settings")
    if not isinstance(settings, dict):
        raise RpcError("Orca returned invalid agent settings")
    arguments = settings.get("agentDefaultArgs", {})
    if not isinstance(arguments, dict) or not all(
        isinstance(key, str) and isinstance(value, str) for key, value in arguments.items()
    ):
        raise RpcError("Orca returned invalid agent arguments")
    return arguments


def codex_defaults(rpc, apply=False):
    arguments = _arguments(rpc)
    # An explicit empty entry overrides Orca's built-in YOLO default. Preserve
    # custom arguments and every other agent's settings; Codex owns config.toml.
    if "codex" in arguments and arguments["codex"].strip() != YOLO:
        return True
    if not apply:
        return False
    rpc.call("settings.update", {"agentDefaultArgs": {**arguments, "codex": ""}})
    if _arguments(rpc).get("codex") != "":
        raise RpcError("Orca Codex launch defaults did not converge")
    return True


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true")
    parser.add_argument("--state", default="/srv/agents/orca")
    args = parser.parse_args()
    rpc = RuntimeRPC(Path(args.state) / "config/orca/orca-runtime.json")
    try:
        return 0 if codex_defaults(rpc, apply=not args.check) else 1
    except Exception:
        # Neither settings nor upstream errors may enter logs: they can contain secrets.
        print("Orca Codex launch settings unavailable; run yard orca up", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
