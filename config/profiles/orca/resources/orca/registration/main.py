#!/usr/bin/env python3
"""One bounded, explicit registration pass; status is read-only."""

import argparse
import json
from pathlib import Path
import signal
import time

from discovery import discover
from reconcile import reconcile
from transport import RuntimeRPC


class DeadlineExceeded(Exception):
    pass


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("sync", "status"))
    parser.add_argument("--workspaces", default="/srv/workspaces")
    parser.add_argument("--state", default="/srv/agents/orca")
    args = parser.parse_args()
    report = {"ready": False, "registered": 0, "total": 0,
              "errors": [], "warnings": [], "projects": []}
    deadline = time.monotonic() + 85

    def expired(*_):
        raise DeadlineExceeded()

    signal.signal(signal.SIGALRM, expired)
    signal.setitimer(signal.ITIMER_REAL, 85)
    try:
        scan = discover(args.workspaces, deadline=min(deadline, time.monotonic() + 20))
        report["total"] = sum(len(project.roots) for project in scan.projects)
        rpc = RuntimeRPC(Path(args.state) / "config/orca/orca-runtime.json", deadline=deadline)
        report = reconcile(scan, rpc, args.state, apply=args.command == "sync", deadline=deadline)
    except DeadlineExceeded:
        report["errors"].append("Orca registration time budget exhausted; result is incomplete")
    except Exception:
        # Exceptions may include runtime credentials or private configuration.
        report["errors"].append("Orca registration failed unexpectedly; result is incomplete")
    finally:
        signal.setitimer(signal.ITIMER_REAL, 0)
    print(json.dumps(report, separators=(",", ":")))
    return 0 if report["ready"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
