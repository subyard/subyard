"""Keep stock Orca launch defaults from overriding the yard's Codex and Claude config."""

import argparse
from pathlib import Path
import sys

from transport import RpcError, RuntimeRPC


YOLO = "--dangerously-bypass-approvals-and-sandbox"
# Orca's stock arguments per agent. A stock Claude flag also makes Orca's native chat
# answer every permission prompt itself, bypassing the yard's managed approval rules.
STOCK = {"codex": YOLO, "claude": "--dangerously-skip-permissions"}


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


def _stock(arguments):
    return [agent for agent, stock in STOCK.items()
            if agent not in arguments or arguments[agent].strip() == stock]


def launch_defaults(rpc, apply=False):
    arguments = _arguments(rpc)
    # An explicit empty entry overrides Orca's built-in stock default. Preserve custom
    # arguments and every other agent's settings; the agents own their configuration.
    stock = _stock(arguments)
    if not stock:
        return True
    if not apply:
        return False
    rpc.call("settings.update", {"agentDefaultArgs": {**arguments, **dict.fromkeys(stock, "")}})
    if _stock(_arguments(rpc)):
        raise RpcError("Orca agent launch defaults did not converge")
    return True


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true")
    parser.add_argument("--state", default="/srv/agents/orca")
    args = parser.parse_args()
    rpc = RuntimeRPC(Path(args.state) / "config/orca/orca-runtime.json")
    try:
        return 0 if launch_defaults(rpc, apply=not args.check) else 1
    except Exception:
        # Neither settings nor upstream errors may enter logs: they can contain secrets.
        print("Orca agent launch settings unavailable; run yard orca up", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
