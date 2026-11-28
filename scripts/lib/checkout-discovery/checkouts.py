#!/usr/bin/env python3
"""JSON CLI over checkout discovery for a profile that keeps its own state.

advance runs one bounded, resumable discovery portion; a profile timer calls it.
roots reports each project's Git roots under src, revalidates the roots that
discovery has cached without traversal, and reports which of the caller's known
paths are proven absent. With --siblings it also reports Git roots directly in
the project directory beside src, such as linked worktrees, without looking
inside them.
"""

import argparse
import json
import os
import sys
import time

from discovery import Root, ScanLimit, _git_probe, _open_dir, _same_directory, absence, discover, include_known
from inventory import Inventory, InventoryError

ADVANCE_BUDGET = 5
SCAN_BUDGET = 20


def add_siblings(scan, deadline):
    """Add Git roots in each project directory beside its present src root."""
    for project in scan.projects:
        if not project.roots:
            continue
        directory = os.path.dirname(project.root)
        fd = None
        try:
            fd = _open_dir(directory)
            if not _same_directory(directory, fd):
                raise OSError("directory changed")
            with os.scandir(fd) as iterator:
                names = sorted(item.name for item in iterator
                               if item.name not in ("src", ".git") and item.is_dir(follow_symlinks=False))
            for name in names:
                path = os.path.join(directory, name)
                try:
                    child = _open_dir(name, fd)
                except OSError:
                    scan.errors.append("Directory scan failed: " + path)
                    continue
                try:
                    try:
                        os.stat(".git", dir_fd=child, follow_symlinks=False)
                    except FileNotFoundError:
                        continue
                    kind, git_dir, common_dir = _git_probe(path, child, True, deadline)
                    if kind in ("git", "unknown"):
                        project.roots.append(Root(path, name, kind, git_dir, common_dir))
                    if kind == "unknown":
                        scan.errors.append("Git root could not be verified: " + path)
                finally:
                    os.close(child)
        except ScanLimit:
            scan.errors.append("Workspace discovery time limit reached; expected repository set is incomplete")
        except OSError:
            scan.errors.append("Project directory scan failed: " + directory)
        finally:
            if fd is not None:
                os.close(fd)


def roots(arguments, known):
    deadline = time.monotonic() + SCAN_BUDGET
    scan = discover(arguments.workspaces, deadline=deadline, recursive=False)
    with Inventory(arguments.state) as inventory:
        cached = inventory.paths()
        progress = inventory.progress()
    include_known(scan, cached + known, deadline)
    if arguments.siblings:
        add_siblings(scan, deadline)
    present = {root.path for project in scan.projects for root in project.roots}
    return {
        "projects": [{"projectId": project.project_id, "name": project.name, "root": project.root,
                      "roots": [{"path": root.path, "kind": root.kind}
                                for root in sorted(project.roots, key=lambda item: item.path)]}
                     for project in sorted(scan.projects, key=lambda item: item.project_id)],
        "absent": sorted(path for path in set(known) if path not in present and absence(scan, path) is True),
        "errors": scan.errors, "warnings": scan.warnings, "discovery": progress,
    }


def advance(arguments):
    deadline = time.monotonic() + SCAN_BUDGET
    scan = discover(arguments.workspaces, deadline=deadline, recursive=False)
    with Inventory(arguments.state, apply=True) as inventory:
        inventory.advance(scan, min(deadline, time.monotonic() + ADVANCE_BUDGET))
        progress = inventory.progress()
    return {"errors": scan.errors, "warnings": scan.warnings, "discovery": progress}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("roots", "advance"))
    parser.add_argument("--workspaces", required=True)
    parser.add_argument("--state", required=True)
    parser.add_argument("--siblings", action="store_true")
    arguments = parser.parse_args()
    try:
        if arguments.command == "advance":
            report = advance(arguments)
        else:
            # Known paths arrive on stdin as a JSON list of absolute paths.
            known = json.loads(sys.stdin.read(1 << 20) or "[]")
            if not isinstance(known, list) or not all(isinstance(path, str) and path.startswith("/") for path in known):
                raise ValueError("known paths must be a JSON list of absolute paths")
            report = roots(arguments, known)
    except (InventoryError, ValueError) as error:
        report = {"errors": [str(error)]}
    # Errors travel in the report; a caller treats a report without projects as unknown.
    print(json.dumps(report, separators=(",", ":")))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
