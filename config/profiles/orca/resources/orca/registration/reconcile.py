"""Additive reconciliation against stock Orca RPC; never edit its database.

Catalog readback proves current runtime state only. Orca owns its debounced disk
persistence, which is verified separately by service restart acceptance tests.
"""

import math
import os
import time

from discovery import verify_root
from state import State, StateError
from transport import RpcError


def _records(rpc, method, key):
    result = rpc.call(method)
    records = result.get(key)
    if not isinstance(records, list):
        raise RpcError("Orca returned an invalid " + key + " catalog")
    ids = set()
    for record in records:
        if (not isinstance(record, dict) or not isinstance(record.get("id"), str)
                or not record["id"] or record["id"] in ids
                or (key == "repos" and not isinstance(record.get("path"), str))):
            raise RpcError("Orca returned an invalid " + key + " catalog")
        ids.add(record["id"])
    return records


def _repo_at(repos, path):
    all_matches = [repo for repo in repos if repo["path"] == path]
    matches = [repo for repo in all_matches if _local(repo)]
    if len(matches) > 1:
        raise RpcError("Ambiguous Orca repository path: " + path)
    if all_matches and not matches:
        # The pinned repo.add RPC is host-unaware and would return the remote
        # same-path record. It cannot safely create a local record in this case.
        raise RpcError("Orca path belongs to another execution host: " + path)
    return matches[0] if matches else None


def _local(record):
    return record.get("executionHostId") in (None, "local") and record.get("connectionId") is None


def _group(state, entry, project, rpc):
    groups = _records(rpc, "projectGroup.list", "groups")
    group_id = entry.get("group_id")
    if group_id:
        mapped = next((group for group in groups if group["id"] == group_id), None)
        if mapped:
            if not _local(mapped):
                raise RpcError("Mapped Orca group belongs to another execution host")
            return group_id
    pending = entry.get("pending_group")
    if pending:
        # parentPath plus the before-ID set gives identity evidence independent
        # of the editable group name. A name match by itself is never adopted.
        matches = [group for group in groups
                   if group["id"] not in pending["before_ids"]
                   and group.get("parentPath") == project.root
                   and group.get("createdFrom") == "migration"
                   and _local(group)
                   and group.get("parentGroupId") is None]
        if len(matches) > 1:
            raise RpcError("Ambiguous pending Orca group identity")
        if matches:
            entry["group_id"] = matches[0]["id"]
            del entry["pending_group"]
            state.save()
            return entry["group_id"]
        if pending["runtime_id"] == rpc.runtime_id:
            raise RpcError("Pending Orca group creation has no confirmed result; retry after runtime recovery")
        # A different runtime cannot later finish a request from the old process.
        del entry["pending_group"]
    entry.pop("group_id", None)
    entry["pending_group"] = {"before_ids": [group["id"] for group in groups],
                              "runtime_id": rpc.runtime_id, "name": project.name}
    state.save()

    def before_send(runtime_id):
        # Metadata may rotate between the catalog read and this connection.
        entry["pending_group"]["runtime_id"] = runtime_id
        state.save()

    try:
        result = rpc.call("projectGroup.create", {"name": project.name,
                          "parentPath": project.root, "createdFrom": "migration"}, before_send=before_send)
        created = result.get("group")
        if (not isinstance(created, dict) or not isinstance(created.get("id"), str)
                or not created["id"] or created.get("parentPath") != project.root
                or created["id"] in entry["pending_group"]["before_ids"]):
            raise RpcError("Orca returned an invalid created group", unknown=True)
        entry["group_id"] = created["id"]
        del entry["pending_group"]
        state.save()
        return created["id"]
    except RpcError as error:
        if not error.unknown:
            del entry["pending_group"]
            state.save()
            raise
        # Never resend create merely because the response was lost.
        return _group(state, entry, project, rpc)


def _write_and_read(rpc, method, params, path):
    try:
        rpc.call(method, params)
    except RpcError as error:
        if not error.unknown:
            raise
    return _repo_at(_records(rpc, "repo.list", "repos"), path)


def _apply_repo(state, entry, root, group_id, rpc):
    if root.kind not in ("git", "folder"):
        raise RpcError("Git kind is unverified: " + root.path)
    # Reject a directory replaced after discovery, including symlink ancestors.
    if not verify_root(root, state.deadline):
        raise RpcError("Workspace Git fact changed or became unavailable after discovery: " + root.path)
    repos = _records(rpc, "repo.list", "repos")
    repo = _repo_at(repos, root.path)
    pending_repos = entry.setdefault("pending_repos", {})
    pending = pending_repos.get(root.path)
    if repo is None:
        if pending is None:
            pending = {"before_ids": [record["id"] for record in repos], "name": root.name}
            pending_repos[root.path] = pending
            state.save()
        if pending.get("runtime_id") == rpc.runtime_id:
            raise RpcError("Pending Orca repository creation has no confirmed result; retry after runtime recovery: " + root.path)

        def before_send(runtime_id):
            # addRepo checks duplicates before awaiting Git/icon discovery, so
            # a timed-out request can still insert after the next catalog read.
            # Journal the connected runtime, including metadata rotation.
            if pending.get("runtime_id") == runtime_id:
                raise RpcError("Orca repository creation is still pending: " + root.path, unknown=True)
            pending["runtime_id"] = runtime_id
            state.save()

        try:
            rpc.call("repo.add", {"path": root.path, "kind": root.kind}, before_send=before_send)
        except RpcError as error:
            if not error.unknown:
                pending.pop("runtime_id", None)
                state.save()
                raise
        # Keep intent on failed/absent readback too: the add may still complete.
        repo = _repo_at(_records(rpc, "repo.list", "repos"), root.path)
        if repo is None:
            raise RpcError("Orca repository creation has no confirmed result: " + root.path)
    if pending:
        # Initial naming remains pending across interruption, but a user's
        # non-default name always wins.
        newly_created = repo["id"] not in pending["before_ids"]
        old_name = repo.get("displayName")
        if newly_created and old_name in (None, "", os.path.basename(root.path)):
            repo = _write_and_read(rpc, "repo.update", {
                "repo": "id:" + repo["id"], "updates": {"displayName": pending["name"]}}, root.path)
            if repo is None or repo.get("displayName") != pending["name"]:
                raise RpcError("Initial Orca repository name is unconfirmed: " + root.path)
        del pending_repos[root.path]
        state.save()
    updates = {}
    # Legacy registrations can have no explicit label. Fill that gap while
    # preserving every existing nonempty name, including a literal 'src'.
    if repo.get("displayName") in (None, ""):
        updates["displayName"] = root.name
    if repo.get("kind", "git") != root.kind:
        updates["kind"] = root.kind
    if root.kind == "git" and repo.get("externalWorktreeVisibility") != "hide":
        updates["externalWorktreeVisibility"] = "hide"
    if updates:
        if not verify_root(root, state.deadline):
            raise RpcError("Workspace Git fact changed before update: " + root.path)
        repo = _write_and_read(rpc, "repo.update", {"repo": "id:" + repo["id"], "updates": updates}, root.path)
        if (repo is None or repo.get("kind", "git") != root.kind
                or (root.kind == "git" and repo.get("externalWorktreeVisibility") != "hide")
                or repo.get("displayName") in (None, "")):
            raise RpcError("Orca repository kind is unconfirmed: " + root.path)
    if repo.get("projectGroupId") != group_id:
        params = {"repo": "id:" + repo["id"], "groupId": group_id}
        order = repo.get("projectGroupOrder")
        if type(order) in (int, float) and math.isfinite(order):
            params["order"] = order
        repo = _write_and_read(rpc, "projectGroup.moveProject", params, root.path)
        if repo is None or repo.get("projectGroupId") != group_id:
            raise RpcError("Orca repository membership is unconfirmed: " + root.path)


def _report_catalog(report, scan, state, rpc):
    groups = _records(rpc, "projectGroup.list", "groups")
    repos = _records(rpc, "repo.list", "repos")
    group_ids = {group["id"] for group in groups if _local(group)}
    desired = {root.path for project in scan.projects for root in project.roots}
    project_roots = {project.root for project in scan.projects}
    project_roots.update(entry["root"] for entry in state.data["projects"].values())
    for repo in repos:
        if _local(repo) and repo["path"] not in desired and any(
                repo["path"] == root or repo["path"].startswith(root + "/") for root in project_roots):
            report["warnings"].append("Existing Orca record is outside the current discovered set and is retained: " + repo["path"])
    for project in scan.projects:
        entry = state.data["projects"].get(project.project_id, {})
        group_id = entry.get("group_id")
        detail = {"projectId": project.project_id, "name": project.name,
                  "groupId": group_id, "repos": []}
        report["projects"].append(detail)
        for root in project.roots:
            reasons = []
            if entry and entry.get("root") != project.root:
                reasons.append("project identity root differs")
            try:
                repo = _repo_at(repos, root.path)
            except RpcError as error:
                reasons.append(str(error))
                repo = None
            if root.kind not in ("git", "folder"):
                reasons.append("Git kind is unverified")
            if not repo:
                reasons.append("repository is missing")
            else:
                if repo.get("displayName") in (None, ""):
                    reasons.append("initial repository name is missing")
                if repo.get("kind", "git") != root.kind:
                    reasons.append("repository kind differs")
                if root.kind == "git" and repo.get("externalWorktreeVisibility") != "hide":
                    reasons.append("selected checkout visibility differs")
                if not group_id or group_id not in group_ids:
                    reasons.append("project group identity is missing")
                elif repo.get("projectGroupId") != group_id:
                    reasons.append("project group membership differs")
                if root.path in entry.get("pending_repos", {}):
                    reasons.append("initial repository registration is pending")
            ready = not reasons
            report["registered"] += int(ready)
            detail["repos"].append({"path": root.path, "kind": root.kind, "ready": ready,
                                    "repoId": repo.get("id") if repo else None})
            if reasons:
                report["errors"].append(root.path + ": " + "; ".join(reasons))


def reconcile(scan, rpc, state_dir, apply=True, deadline=None):
    deadline = deadline if deadline is not None else time.monotonic() + 65
    report = {"ready": False, "registered": 0,
              "total": sum(len(project.roots) for project in scan.projects),
              "errors": list(scan.errors), "warnings": list(scan.warnings), "projects": []}
    try:
        with State(state_dir, apply, deadline) as state:
            if apply:
                for project in scan.projects:
                    if not project.roots:
                        continue
                    if time.monotonic() >= deadline:
                        report["errors"].append("Orca registration time budget exhausted")
                        break
                    entry = state.data["projects"].setdefault(project.project_id,
                            {"root": project.root, "pending_repos": {}})
                    if entry["root"] != project.root:
                        report["errors"].append("Project identity root differs: " + project.project_id)
                        continue
                    try:
                        group_id = _group(state, entry, project, rpc)
                    except RpcError as error:
                        report["errors"].append(project.project_id + ": " + str(error))
                        continue
                    for root in project.roots:
                        if time.monotonic() >= deadline:
                            report["errors"].append("Orca registration time budget exhausted")
                            break
                        try:
                            _apply_repo(state, entry, root, group_id, rpc)
                        except RpcError as error:
                            report["errors"].append(root.path + ": " + str(error))
            _report_catalog(report, scan, state, rpc)
    except (RpcError, StateError) as error:
        report["errors"].append(str(error))
    report["errors"] = list(dict.fromkeys(report["errors"]))
    report["warnings"] = list(dict.fromkeys(report["warnings"]))
    report["ready"] = not report["errors"] and report["registered"] == report["total"]
    return report
