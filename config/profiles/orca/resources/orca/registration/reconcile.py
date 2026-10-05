"""Reconciliation against stock Orca RPC; never edit its database.

Catalog readback proves current runtime state only. Orca owns its debounced disk
persistence, which is verified separately by service restart acceptance tests.
"""

import hashlib
import json
import math
import os
import time

from discovery import include_known, verify_missing, verify_root
from state import State, StateError
from transport import RpcError


REPOSITORY_LIMIT = 1000


class AdmissionLimit(RpcError):
    pass


def _admission_full(state, rpc, repos):
    # A lost add response can still insert later. Reserve its slot until the
    # row appears or the runtime that received the request is replaced.
    local_paths = {repo["path"] for repo in repos if _local(repo)}
    pending_paths = {path for entry in state.data["projects"].values()
                     for path, pending in entry.get("pending_repos", {}).items()
                     if pending.get("runtime_id") == rpc.runtime_id and path not in local_paths}
    return len(repos) + len(pending_paths) >= REPOSITORY_LIMIT


class CatalogRPC:
    """Reuse catalogs on no-op roots; invalidate before every possible mutation."""
    def __init__(self, rpc):
        self.rpc = rpc
        self.catalogs = {}

    @property
    def runtime_id(self):
        return self.rpc.runtime_id

    def refresh(self):
        self.catalogs.clear()

    def call(self, method, params=None, before_send=None):
        if method in ("repo.list", "projectGroup.list"):
            if method not in self.catalogs:
                self.catalogs[method] = self.rpc.call(method, params, before_send=before_send)
            return self.catalogs[method]
        self.refresh()
        return self.rpc.call(method, params, before_send=before_send)


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


def _group_name(project, host_name):
    return project.name + " / " + host_name if host_name else project.name


def _group_name_differs(group, entry, project, host_name):
    return (bool(host_name) and group.get("name") in (project.name, entry.get("group_name"))
            and group.get("name") != _group_name(project, host_name))


def _group(state, entry, project, rpc, host_name):
    name = _group_name(project, host_name)
    groups = _records(rpc, "projectGroup.list", "groups")
    group_id = entry.get("group_id")
    if group_id:
        mapped = next((group for group in groups if group["id"] == group_id), None)
        if mapped:
            if not _local(mapped):
                raise RpcError("Mapped Orca group belongs to another execution host")
            if _group_name_differs(mapped, entry, project, host_name):
                try:
                    rpc.call("projectGroup.update", {"groupId": group_id, "updates": {"name": name}})
                except RpcError as error:
                    if not error.unknown:
                        raise
                mapped = next((group for group in _records(rpc, "projectGroup.list", "groups")
                               if group["id"] == group_id), None)
                if mapped is None or mapped.get("name") != name or not _local(mapped):
                    raise RpcError("Orca project group name is unconfirmed")
            if mapped.get("name") == name and entry.get("group_name") != name:
                entry["group_name"] = name
                state.save()
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
            entry["group_name"] = pending["name"]
            del entry["pending_group"]
            state.save()
            return _group(state, entry, project, rpc, host_name)
        if pending["runtime_id"] == rpc.runtime_id:
            raise RpcError("Pending Orca group creation has no confirmed result; retry after runtime recovery")
        # A different runtime cannot later finish a request from the old process.
        del entry["pending_group"]
    entry.pop("group_id", None)
    entry["pending_group"] = {"before_ids": [group["id"] for group in groups],
                              "runtime_id": rpc.runtime_id, "name": name}
    state.save()

    def before_send(runtime_id):
        # Metadata may rotate between the catalog read and this connection.
        entry["pending_group"]["runtime_id"] = runtime_id
        state.save()

    try:
        result = rpc.call("projectGroup.create", {"name": name,
                          "parentPath": project.root, "createdFrom": "migration"}, before_send=before_send)
        created = result.get("group")
        if (not isinstance(created, dict) or not isinstance(created.get("id"), str)
                or not created["id"] or created.get("parentPath") != project.root
                or created["id"] in entry["pending_group"]["before_ids"]):
            raise RpcError("Orca returned an invalid created group", unknown=True)
        entry["group_id"] = created["id"]
        entry["group_name"] = name
        del entry["pending_group"]
        state.save()
        return created["id"]
    except RpcError as error:
        if not error.unknown:
            del entry["pending_group"]
            state.save()
            raise
        # Never resend create merely because the response was lost.
        return _group(state, entry, project, rpc, host_name)


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
    repos = _records(rpc, "repo.list", "repos")
    repo = _repo_at(repos, root.path)
    pending_repos = entry.setdefault("pending_repos", {})
    if repo is None and root.path not in pending_repos and _admission_full(state, rpc, repos):
        # At capacity, cached facts can only defer a new root. Do not repeatedly
        # probe Git or refresh the native catalog for every rejected fixture.
        raise AdmissionLimit("Orca repository admission limit reached")
    # Reject a directory replaced after discovery, including symlink ancestors.
    if not verify_root(root, state.deadline):
        raise RpcError("Workspace Git fact changed or became unavailable after discovery: " + root.path)
    if (repo is None or root.path in pending_repos or repo.get("displayName") in (None, "")
            or repo.get("kind", "git") != root.kind or repo.get("projectGroupId") != group_id
            or root.kind == "git" and repo.get("externalWorktreeVisibility") != "hide"):
        # Recheck before changing a cached record; a client may have edited it
        # while other known roots were being validated.
        rpc.refresh()
        repos = _records(rpc, "repo.list", "repos")
        repo = _repo_at(repos, root.path)
    pending = pending_repos.get(root.path)
    if repo is None:
        if pending and pending.get("runtime_id") == rpc.runtime_id:
            raise RpcError("Pending Orca repository creation has no confirmed result; retry after runtime recovery: " + root.path)
        if _admission_full(state, rpc, repos):
            raise AdmissionLimit("Orca repository admission limit reached")
        if pending is None:
            pending = {"before_ids": [record["id"] for record in repos], "name": root.name}
            pending_repos[root.path] = pending
            state.save()
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


def _report_catalog(report, scan, state, rpc, host_name):
    groups = _records(rpc, "projectGroup.list", "groups")
    repos = _records(rpc, "repo.list", "repos")
    admission_full = _admission_full(state, rpc, repos)
    report["deferred"] = 0
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
        group = next((group for group in groups if group["id"] == group_id), None)
        if group and _local(group) and _group_name_differs(group, entry, project, host_name):
            report["errors"].append(project.project_id + ": project group name differs")
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
                if (not reasons and admission_full
                        and entry.get("pending_repos", {}).get(root.path, {}).get("runtime_id") != rpc.runtime_id
                        and not entry.get("pending_group")):
                    report["deferred"] += 1
                    detail["repos"].append({"path": root.path, "kind": root.kind, "ready": False,
                                            "repoId": None, "deferred": True})
                    continue
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
    if report["deferred"]:
        report["errors"].append(
            f"Orca repository admission limit reached ({REPOSITORY_LIMIT} records including pending additions); "
            f"{report['deferred']} new registrations deferred; existing records are retained")


def _session_snapshots(rpc):
    result = rpc.call("session.tabs.listAll")
    snapshots = result.get("snapshots") if isinstance(result, dict) else None
    if (not isinstance(snapshots, list) or any(
            not isinstance(item, dict) or not isinstance(item.get("worktree"), str)
            or not isinstance(item.get("tabs"), list) for item in snapshots)):
        raise RpcError("Orca returned an invalid session tab catalog; cleanup skipped")
    return snapshots


def _has_saved_tabs(snapshots, repo):
    return any(item["worktree"].startswith(repo["id"] + "::") and item["tabs"]
               for item in snapshots)


def _prune_missing(report, scan, state, rpc, approved_ids=None):
    # Missing project roots may be temporarily unmounted. A known checkout
    # losing Git metadata is retained without blocking independent missing paths.
    if report["errors"] or any(not project.roots for project in scan.projects):
        return
    groups = {group["id"] for group in _records(rpc, "projectGroup.list", "groups") if _local(group)}
    owners = {entry["group_id"]: entry["root"] for entry in state.data["projects"].values()
              if entry.get("group_id") in groups}
    active_roots = {project.root for project in scan.projects
                    if state.data["projects"].get(project.project_id, {}).get("group_id") in owners}
    desired = {root.path for project in scan.projects for root in project.roots}
    candidates = []
    for repo in _records(rpc, "repo.list", "repos"):
        root = owners.get(repo.get("projectGroupId"))
        if repo.get("projectGroupId") is None:
            # Legacy or interrupted registrations may not have membership yet.
            # Only nested Git paths of a reconciled active project are owned here.
            root = next((root for root in active_roots if repo["path"].startswith(root + "/")), None)
        if (root and _local(repo) and repo["path"] not in desired
                and (repo["path"] == root or repo["path"].startswith(root + "/"))
                and (repo["path"] == root or repo.get("kind", "git") == "git")
                and verify_missing(scan, repo["path"])):
            candidates.append(repo)
    for repo in candidates:
        if approved_ids is not None and repo["id"] not in approved_ids[0]:
            raise RpcError("plan_stale: unapproved Orca cleanup repository")
        if time.monotonic() >= state.deadline:
            raise RpcError("Orca registration time budget exhausted")
        # This API also covers saved tabs of now-missing linked worktrees.
        try:
            snapshots = _session_snapshots(rpc)
        except RpcError as error:
            if not error.timed_out or error.unknown:
                raise
            # The pinned Orca session inventory can be incomplete while cold. A
            # second read may preserve a positively observed saved tab, but can
            # never prove absence well enough to authorize deletion.
            try:
                retry_snapshots = _session_snapshots(rpc)
            except RpcError:
                raise error from None
            if not _has_saved_tabs(retry_snapshots, repo):
                raise error from None
            report["warnings"].append(
                "Missing Orca checkout has session tabs and is retained: " + repo["path"]
            )
            return
        if _has_saved_tabs(snapshots, repo):
            report["warnings"].append("Missing Orca checkout has session tabs and is retained: " + repo["path"])
            continue
        rpc.refresh()
        current = _repo_at(_records(rpc, "repo.list", "repos"), repo["path"])
        if current != repo or not verify_missing(scan, repo["path"]):
            raise RpcError("Orca cleanup candidate changed; retry sync: " + repo["path"])
        try:
            rpc.call("repo.rm", {"repo": "id:" + repo["id"]})
        except RpcError as error:
            if not error.unknown:
                raise
        if any(item["id"] == repo["id"] for item in _records(rpc, "repo.list", "repos")):
            raise RpcError("Orca repository removal is unconfirmed: " + repo["path"])
    active = {project.project_id for project in scan.projects}
    for project_id, entry in list(state.data["projects"].items()):
        group_id = entry.get("group_id")
        if project_id in active or group_id not in owners or not verify_missing(scan, entry["root"]):
            continue
        if approved_ids is not None and group_id not in approved_ids[1]:
            raise RpcError("plan_stale: unapproved Orca cleanup group")
        rpc.refresh()
        groups = _records(rpc, "projectGroup.list", "groups")
        repos = _records(rpc, "repo.list", "repos")
        folders = _records(rpc, "folderWorkspace.list", "folderWorkspaces")
        if (any(repo.get("projectGroupId") == group_id for repo in repos + folders)
                or any(group.get("parentGroupId") == group_id for group in groups)):
            continue
        group = next((item for item in groups if item["id"] == group_id), None)
        if group is None or not _local(group) or not verify_missing(scan, entry["root"]):
            continue
        try:
            rpc.call("projectGroup.delete", {"groupId": group_id})
        except RpcError as error:
            if not error.unknown:
                raise
        if any(group["id"] == group_id for group in _records(rpc, "projectGroup.list", "groups")):
            raise RpcError("Orca project group removal is unconfirmed")
        del state.data["projects"][project_id]
        state.save()


def reconcile(scan, rpc, state_dir, apply=True, deadline=None, host_name="", known_paths=None):
    deadline = deadline if deadline is not None else time.monotonic() + 65
    report = {"ready": False, "registered": 0,
              "total": sum(len(project.roots) for project in scan.projects),
              "errors": list(scan.errors), "warnings": list(scan.warnings), "projects": []}
    try:
        with State(state_dir, apply, deadline) as state:
            rpc = CatalogRPC(rpc)
            if known_paths is not None:
                paths = list(known_paths)
                for entry in state.data["projects"].values():
                    paths.extend(entry.get("known_roots", []))
                # Upgrade without a rescan: adopt verified local native Git roots.
                paths.extend(repo["path"] for repo in _records(rpc, "repo.list", "repos")
                             if _local(repo) and repo.get("kind", "git") == "git")
                include_known(scan, paths, deadline)
                report["errors"].extend(scan.errors)
                report["warnings"].extend(scan.warnings)
                report["total"] = sum(len(project.roots) for project in scan.projects)
            scope = [{"projectId": project.project_id, "name": project.name,
                      "repos": [{"path": root.path, "kind": root.kind}
                                for root in sorted(project.roots, key=lambda root: root.path)]}
                     for project in sorted(scan.projects, key=lambda project: project.project_id)]
            scope_digest = hashlib.sha256(json.dumps(scope, ensure_ascii=False, sort_keys=True,
                                           separators=(",", ":")).encode()).hexdigest()
            report["scopeDigest"] = scope_digest
            # Only registration metadata is fingerprinted; saved tab contents and
            # credentials never enter a public fact or digest.
            keys = ("id", "path", "displayName", "kind", "projectGroupId", "parentPath",
                    "name", "createdFrom", "connectionId", "executionHostId", "parentGroupId",
                    "externalWorktreeVisibility")
            repos = _records(rpc, "repo.list", "repos")
            groups = _records(rpc, "projectGroup.list", "groups")
            safe_records = lambda records: sorted(
                [{key: record[key] for key in keys if key in record} for record in records],
                key=lambda record: record["id"])
            catalog = {"scope": scope, "repos": safe_records(repos),
                       "groups": safe_records(groups), "registration": state.data}
            catalog_digest = hashlib.sha256(json.dumps(catalog, ensure_ascii=False, sort_keys=True,
                                             separators=(",", ":")).encode()).hexdigest()
            report["catalogDigest"] = catalog_digest
            expected_scope = os.environ.get("SUBYARD_ORCA_REGISTRATION_SCOPE", "")
            approved_ids = None
            if expected_scope:
                if expected_scope != "conditional" and expected_scope != catalog_digest:
                    raise RpcError("plan_stale: Orca verified project-root scope changed")
                approved_ids = ({record["id"] for record in repos},
                                {record["id"] for record in groups})
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
                    roots = set(entry.get("known_roots", []))
                    roots.update(root.path for root in project.roots
                                 if root.kind == "git" and root.path != project.root)
                    # A failed Git probe must not erase the only durable fact
                    # about a root adopted before background discovery reaches it.
                    roots = sorted(path for path in roots if not verify_missing(scan, path))
                    if entry.get("known_roots") != roots:
                        entry["known_roots"] = roots
                        state.save()
                    repos = _records(rpc, "repo.list", "repos")
                    if (not entry.get("group_id") and not entry.get("pending_group")
                            and not entry.get("pending_repos") and _admission_full(state, rpc, repos)
                            and not any(_repo_at(repos, root.path) for root in project.roots)):
                        # Do not create empty groups for projects whose every
                        # root would be refused by the admission policy.
                        continue
                    try:
                        group_id = _group(state, entry, project, rpc, host_name)
                    except RpcError as error:
                        report["errors"].append(project.project_id + ": " + str(error))
                        continue
                    for root in project.roots:
                        if time.monotonic() >= deadline:
                            report["errors"].append("Orca registration time budget exhausted")
                            break
                        try:
                            _apply_repo(state, entry, root, group_id, rpc)
                        except AdmissionLimit:
                            # Report capacity after pruning: new-admission
                            # refusal must not prevent independently safe cleanup.
                            continue
                        except RpcError as error:
                            report["errors"].append(root.path + ": " + str(error))
                _prune_missing(report, scan, state, rpc, approved_ids)
            rpc.refresh()
            _report_catalog(report, scan, state, rpc, host_name)
    except (RpcError, StateError) as error:
        report["errors"].append(str(error))
    report["errors"] = list(dict.fromkeys(report["errors"]))
    report["warnings"] = list(dict.fromkeys(report["warnings"]))
    report["ready"] = not report["errors"] and report["registered"] == report["total"]
    return report
