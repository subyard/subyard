"""Reconciliation against stock Orca RPC; never edit its database.

Catalog readback proves current runtime state only. Orca owns its debounced disk
persistence, which is verified separately by service restart acceptance tests.
"""

import hashlib
import json
import math
import os
import stat
import time

from discovery import absence, include_known, verify_missing, verify_root
from state import State, StateError
from transport import RpcError


REPOSITORY_LIMIT = 1000

# Retention classes of a cleanup record. Only "protected" records can settle a pass:
# "pending" is cleanup work still to perform and "undetermined" lacks safe evidence.
PROTECTED, PENDING, UNDETERMINED = "protected", "pending", "undetermined"
OWNERSHIP_REFUSALS = frozenset((
    "repository identity changed", "repository belongs to another execution host",
    "repository belongs to another project group", "legacy repository identity is unconfirmed",
    "repository path changed"))
SESSION_RETENTION = frozenset(("session tabs are retained", "live terminals are retained"))
PENDING_WORK = frozenset(("repository removal is pending", "project group removal is pending"))
ACTIVE_ROOT_MISSING = "project root is missing; repository is retained"
CLEANUP_INCOMPLETE = "cleanup safety checks are incomplete; repository is retained"
READBACK_INTERVAL, READBACK_MAX_INTERVAL = 0.1, 1.0


class AdmissionLimit(RpcError):
    pass


def _identity(path):
    """Stable identity only: size and times change with ordinary work and are not bound."""
    observed = os.lstat(path)
    target = os.readlink(path) if stat.S_ISLNK(observed.st_mode) else None
    return [[observed.st_mode, observed.st_uid, observed.st_gid,
             observed.st_dev, observed.st_ino], target]


def _root_metadata(root, deadline):
    """Bind a known root to where Git resolved its repository, without reading contents.

    Discovery's single Git probe resolves core.worktree, core.bare, includes,
    config.worktree, gitfiles and symlinks, so binding the resolved locations and
    their identities catches redirection while commits, fetches, branch and config
    writes, sparse checkout and partial clone leave the binding unchanged. Only
    paths and inode facts are bound; config values, which may carry credentials,
    never enter. Accepted gap: an in-place re-init, or a deletion and re-creation
    at the same path, is undetected when the filesystem reuses the freed inode
    numbers (common on ext4); registration is keyed by path and kind and acts
    identically on such a root.
    """
    if time.monotonic() >= deadline:
        raise RpcError("Orca registration time budget exhausted")
    try:
        metadata = [_identity(root.path)]
    except FileNotFoundError:
        raise RpcError("Known repository root disappeared during observation: " + root.path) from None
    except OSError:
        raise RpcError("Known repository metadata could not be observed: " + root.path) from None
    for directory in dict.fromkeys(path for path in (root.git_dir, root.common_dir) if path):
        try:
            identity = _identity(directory)
        except OSError:
            # Reported per root by the apply-time probe; never abort the catalog.
            identity = None
        metadata.append([directory, identity])
    return metadata


class RemovalEvidence:
    """Filesystem facts of one pass that can prove a project's removal from Subyard."""
    def __init__(self, scan, errors):
        self.scan = scan
        self.errors = errors


class CleanupRefusal(RpcError):
    """A pre-send identity check refused a destructive request; nothing was sent."""


AMBIGUOUS_SESSION = "session identity is ambiguous; repository is retained"


class AmbiguousSession(CleanupRefusal):
    """A PTY is attributed to this record and to another; the next close is not sent."""


class CleanupPass:
    """Typed per-repository outcomes of one pruning pass, keyed by native repository ID."""
    def __init__(self):
        self.outcomes = {}
        self.complete = False


def removal_authorized(record, evidence):
    """Whether one exact owned cleanup record may be destroyed: True, False or None (undetermined).

    Session-free records rely on the caller's verified absence of the checkout below the
    same workspace boundary after complete registration. Sessions of an active project are
    protected. A project whose whole workspace directory (its address) is absent below the
    scan's boundary is removed, so its sessions may be closed: with no project files at the
    address it may be removed, including from Orca. An existing directory there, such as
    a remaining bind mountpoint, keeps them. An incomplete scan, another or changed
    workspace boundary or an unreadable path is undetermined.
    """
    if not record["sessions"]:
        return True
    if not record["removedProject"]:
        return False
    if evidence.errors:
        return None
    address = os.path.dirname(record["root"])
    if os.path.basename(record["root"]) != "src" or os.path.dirname(address) != evidence.scan.workspaces:
        return None
    return absence(evidence.scan, address)


def retention(project_id, root, removed, reason, evidence):
    # A PTY shared with another record leaves ownership unproven, like the refusals above.
    if reason in OWNERSHIP_REFUSALS or reason == AMBIGUOUS_SESSION or reason == ACTIVE_ROOT_MISSING and not removed:
        return PROTECTED
    if reason == REFUSED_CLOSE:
        return REFUSED_CLOSE_CLASS
    if reason in SESSION_RETENTION:
        authorized = removal_authorized({"projectId": project_id, "root": root, "removedProject": removed,
                                         "sessions": True}, evidence)
        return PROTECTED if authorized is False else PENDING if authorized else UNDETERMINED
    return PENDING if reason in PENDING_WORK else UNDETERMINED


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
        try:
            return self.rpc.call(method, params, before_send=before_send)
        finally:
            # before_send fences may themselves refresh the read catalogs.
            self.refresh()


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


def _pending_groups(groups, pending, root):
    return [group for group in groups
            if group["id"] not in pending["before_ids"]
            and group.get("parentPath") == root
            and group.get("createdFrom") == "migration" and _local(group)
            and group.get("parentGroupId") is None]


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
        matches = _pending_groups(groups, pending, project.root)
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
    repo = _repo_at(_records(rpc, "repo.list", "repos"), path)
    if repo is not None and params.get("repo") != "id:" + repo["id"]:
        raise RpcError("Orca repository identity changed during update: " + path)
    return repo


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
    # The verified local catalog identity must survive every later write failure,
    # including initial naming and membership of an interrupted registration.
    confirmed = entry.setdefault("confirmed_repos", {})
    if confirmed.get(root.path) != repo["id"]:
        confirmed[root.path] = repo["id"]
        state.save()
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


def _report_catalog(report, scan, state, rpc, host_name, cleanup_pass, evidence):
    groups = _records(rpc, "projectGroup.list", "groups")
    repos = _records(rpc, "repo.list", "repos")
    admission_full = _admission_full(state, rpc, repos)
    report["deferred"] = 0
    group_ids = {group["id"] for group in groups if _local(group)}
    desired = {root.path for project in scan.projects for root in project.roots}
    project_roots = {project.root for project in scan.projects}
    project_roots.update(entry["root"] for entry in state.data["projects"].values())
    for repo in repos:
        if _local(repo) and repo["path"] not in desired and not verify_missing(scan, repo["path"]) and any(
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
    _report_cleanup(report, scan, state, rpc, groups, repos, cleanup_pass, evidence)


def _cleanup_item(report, evidence, project_id, root, removed, reason, **fields):
    item = {"projectId": project_id, **fields, "removedProject": removed, "reason": reason,
            "retention": retention(project_id, root, removed, reason, evidence)}
    if reason == REFUSED_CLOSE:
        # Protected, yet it needs the operator: status shows it as a warning.
        item["attention"] = True
    report["cleanup"].append(item)


def _report_cleanup(report, scan, state, rpc, groups, repos, cleanup_pass, evidence):
    active = {project.project_id for project in scan.projects}
    rootless = {project.project_id for project in scan.projects if not project.roots}
    read_set = None
    folders = None
    for project_id, entry, repo, reason in _cleanup_records(scan, state, groups, repos):
        if not verify_missing(scan, repo["path"]):
            # Confirmed identities of removed projects stay visible when a mount
            # or the original workspace boundary cannot prove absence.
            if project_id in active:
                continue
            reason = reason or "workspace absence is unverified"
        if reason is None and project_id in rootless:
            # The registered project's mount may be temporarily unavailable.
            reason = ACTIVE_ROOT_MISSING
        if reason is None:
            # Outcomes are keyed by exact repository ID, never inferred from path text.
            reason = cleanup_pass.outcomes.get(repo["id"])
        if reason is None and (report["errors"] or state.apply and not cleanup_pass.complete):
            reason = CLEANUP_INCOMPLETE
        if reason is None:
            # The same ownership rule as the apply pass classifies the record.
            try:
                if read_set is None:
                    read_set = (repos, _session_snapshots(rpc), _terminal_census(rpc))
                closable, tabs, rows = _ownership(repo, *read_set)
                reason = (AMBIGUOUS_SESSION if not closable else "session tabs are retained" if tabs
                          else "live terminals are retained" if rows else "repository removal is pending")
            except RpcError:
                reason = "session inventory is unavailable; cleanup is retained"
        if reason in SESSION_RETENTION and removal_authorized(
                {"projectId": project_id, "root": entry["root"], "removedProject": project_id not in active,
                 "sessions": True}, evidence) is True:
            # Sessions of a removed project are cleanup work, not a retention reason.
            reason = "repository removal is pending"
        if (reason in SESSION_RETENTION or reason == "repository removal is pending") \
                and entry.get("refused_closes", {}).get(repo["id"]):
            # A close Orca refused in an earlier pass needs the operator while its target remains.
            try:
                if read_set is None:
                    read_set = (repos, _session_snapshots(rpc), _terminal_census(rpc))
                if _refused_targets(entry, repo, read_set):
                    reason = REFUSED_CLOSE
            except RpcError:
                reason = "session inventory is unavailable; cleanup is retained"
        if reason == "legacy repository identity is unconfirmed":
            action = "Inspect sessions and remove the exact repository in Orca"
        elif reason == REFUSED_CLOSE:
            action = ("Close this repository's remaining tab or terminal in Orca by hand; "
                      "Orca refused Subyard's close and the record is retained")
        elif reason == AMBIGUOUS_SESSION:
            action = ("Close the terminal session it shares with another repository in Orca; "
                      "Subyard retains this record")
        elif retention(project_id, entry["root"], project_id not in active, reason, evidence) == PROTECTED:
            action = "Inspect the exact repository in Orca; Subyard retains it"
        else:
            action = "Inspect retained sessions or ownership, then retry Orca sync"
        _cleanup_item(report, evidence, project_id, entry["root"], project_id not in active, reason,
                      path=repo["path"], repoId=repo["id"], nextAction=action)
    for project_id, entry in state.data["projects"].items():
        if project_id in active:
            continue
        reported = {item["path"] for item in report["cleanup"] if item["projectId"] == project_id}
        for path in entry.get("pending_repos", {}):
            if path not in reported:
                _cleanup_item(report, evidence, project_id, entry["root"], True,
                              "repository creation identity is pending",
                              path=path, repoId=None,
                              nextAction="Inspect the Orca catalog after runtime recovery, then retry sync")
        if entry.get("pending_group"):
            _cleanup_item(report, evidence, project_id, entry["root"], True,
                          "project group creation identity is pending",
                          path=entry["root"], repoId=None,
                          nextAction="Inspect the Orca catalog after runtime recovery, then retry sync")
        group_id = entry.get("group_id")
        group = next((group for group in groups if group["id"] == group_id), None)
        if (group is None or not _local(group)
                or any(item["projectId"] == project_id for item in report["cleanup"])
                or any(repo.get("projectGroupId") == group_id for repo in repos)
                or any(group.get("parentGroupId") == group_id for group in groups)):
            continue
        # Native folders and child groups deliberately retain their parent. Only
        # an otherwise empty mapped group represents pending project cleanup.
        reason = ("project group removal is pending" if verify_missing(scan, entry["root"])
                  else "workspace absence is unverified")
        try:
            if folders is None:
                folders = _records(rpc, "folderWorkspace.list", "folderWorkspaces")
            if any(folder.get("projectGroupId") == group_id for folder in folders):
                continue
        except RpcError:
            reason = "project group inventory is unavailable; cleanup is retained"
        _cleanup_item(report, evidence, project_id, entry["root"], True, reason, path=entry["root"],
                      groupId=group_id, repoId=None, nextAction="Retry Orca sync")
    report["cleanupReady"] = not any(item["removedProject"] for item in report["cleanup"])


def _session_snapshots(rpc):
    result = rpc.call("session.tabs.listAll")
    snapshots = result.get("snapshots") if isinstance(result, dict) else None
    if (not isinstance(snapshots, list) or any(
            not isinstance(item, dict) or not isinstance(item.get("worktree"), str)
            or not isinstance(item.get("tabs"), list) for item in snapshots)):
        raise RpcError("Orca returned an invalid session tab catalog; cleanup skipped")
    return snapshots


def _only_stale_terminal_layouts(snapshots, repo):
    found = False
    prefix = repo["id"] + "::"
    for item in snapshots:
        if not item["worktree"].startswith(prefix) or not item["tabs"]:
            continue
        # A saved linked checkout may still be usable outside this missing root.
        if item["worktree"] != prefix + repo["path"]:
            return False
        for tab in item["tabs"]:
            if (not isinstance(tab, dict) or tab.get("type") != "terminal"
                    or tab.get("status") != "pending-handle" or "terminal" not in tab
                    or tab["terminal"] is not None or not isinstance(tab.get("id"), str)
                    or not tab["id"] or not isinstance(tab.get("ptyId"), str) or not tab["ptyId"]
                    or (tab.get("parentTabId") is not None and (
                        not isinstance(tab["parentTabId"], str) or not tab["parentTabId"]))
                    or tab.get("sessionId") or tab.get("agentSessionId")
                    or tab.get("agentStatus") or tab.get("launchDraft")):
                return False
            layout = tab.get("parentLayout")
            if layout is not None and (not isinstance(layout, dict) or any(
                    layout.get(key) for key in ("buffersByLeafId", "scrollbackRefsByLeafId", "chatLeafId"))):
                return False
        found = True
    return found


def _terminal_census(rpc):
    # A pending layout can fail to bind a live orphan PTY. Request the native
    # controller's fresh census without worktree/handle filters before pruning.
    result = rpc.call("terminal.list", {"limit": 1000, "requireFreshPtyLiveness": True,
                                        "includeVisualLayouts": False})
    terminals = result.get("terminals")
    scope = result.get("hostScope")
    if (not isinstance(terminals, list) or result.get("truncated") is not False
            or type(result.get("totalCount")) is not int or result["totalCount"] != len(terminals)
            or not isinstance(scope, dict) or not isinstance(scope.get("hostIds"), list)
            or not isinstance(scope.get("omittedHostIds"), list)
            or "local" not in scope["hostIds"] or "local" in scope["omittedHostIds"]):
        raise RpcError("Orca terminal inventory is incomplete or invalid; cleanup skipped")
    for terminal in terminals:
        if (not isinstance(terminal, dict) or not isinstance(terminal.get("ptyId"), str)
                or not terminal["ptyId"] or not isinstance(terminal.get("worktreeId"), str)
                or "::" not in terminal["worktreeId"]
                or not isinstance(terminal.get("executionHostId"), str) or not terminal["executionHostId"]
                or type(terminal.get("connected")) is not bool):
            raise RpcError("Orca terminal inventory has an unknown identity; cleanup skipped")
    return terminals


def _ownership(repo, catalog, snapshots, terminals):
    """The one ownership rule for record R, over one read set.

    Every identity key - a PTY ID or terminal handle, and a tab's close identity - is owned
    by every tab and terminal row that mentions it: the local catalog repository its
    worktree names, or unknown for another execution host or an unknown repository. The
    edges are snapshot.worktree, tab.ptyId, tab.terminal, tab.parentTabId (or its own ID),
    terminal.worktreeId, executionHostId, ptyId and handle. R is closable only when every
    key R mentions is owned by R alone. Returns (closable, R's tabs, R's terminal rows).
    """
    local = {item["id"] for item in catalog if _local(item)}
    owner = lambda worktree, host="local": (
        worktree.split("::", 1)[0] if host == "local" and "::" in worktree
        and worktree.split("::", 1)[0] in local else None)
    owners, tabs, rows = {}, [], []

    def claim(kind, value, who, keys):
        if isinstance(value, str) and value:
            owners.setdefault((kind, value), set()).add(who)
            keys.add((kind, value))

    mentioned = set()
    for item in snapshots:
        who = owner(item["worktree"])
        for tab in item["tabs"]:
            keys = mentioned if who == repo["id"] else set()
            if isinstance(tab, dict):
                # Claimed even for a tab without identity: its PTY and handle are still shared.
                claim("pty", tab.get("ptyId"), who, keys)
                claim("handle", tab.get("terminal"), who, keys)
            parent = (tab.get("parentTabId") or tab.get("id")) if isinstance(tab, dict) else None
            if not isinstance(parent, str) or not parent:
                # A tab without identity cannot be addressed exactly: its record stays protected.
                if who == repo["id"]:
                    key = ("unidentified", item["worktree"], len(tabs))
                    owners[key] = {None}
                    mentioned.add(key)
                    tabs.append((item["worktree"], None))
                continue
            claim("tab", parent, who, keys)
            if who == repo["id"]:
                tabs.append((item["worktree"], parent))
    for terminal in terminals:
        who = owner(terminal["worktreeId"], terminal["executionHostId"])
        keys = mentioned if who == repo["id"] else set()
        claim("pty", terminal["ptyId"], who, keys)
        claim("handle", terminal.get("handle"), who, keys)
        if who == repo["id"]:
            rows.append(terminal)
    closable = all(owners[key] == {repo["id"]} for key in mentioned)
    return closable, sorted(set(tabs), key=lambda tab: (tab[0], tab[1] or "")), rows


def _cleanup_plan(repo, read_set, refused_stops=frozenset()):
    """Pure plan over one read set: None when R is protected, else the ordered actions that
    remove R's session state. A PTY whose fenced exact stop was refused is closed by handle
    first, so that close directly follows the refused stop; the remaining live PTYs are then
    stopped through the fenced exact stop, tabs close next, then the remaining terminal handles."""
    catalog, snapshots, terminals = read_set
    closable, tabs, rows = _ownership(repo, catalog, snapshots, terminals)
    if not closable:
        return None
    rows = sorted(rows, key=lambda row: (row["ptyId"], str(row.get("handle"))))
    close = lambda row: ("close", row.get("handle"), row["ptyId"])
    fallback = [row for row in rows if row["ptyId"] in refused_stops]
    return ([close(row) for row in fallback]
            + [("stop", row["worktreeId"], row["ptyId"]) for row in rows
               if row["connected"] and row["ptyId"] not in refused_stops]
            + [("tab", worktree, parent) for worktree, parent in tabs]
            + [close(row) for row in rows if row not in fallback])


def _refused_targets(entry, repo, read_set):
    """Persisted refused closes of this record whose targets are still planned. A refused
    close stays protected across passes, and is never re-sent, until its target is gone."""
    remembered = entry.get("refused_closes", {}).get(repo["id"], [])
    plan = _cleanup_plan(repo, read_set)
    if plan is None:
        return remembered
    return [target for target in remembered if tuple(target) in plan]


def _remember_refusals(state, entry, repo, targets):
    refused = entry.setdefault("refused_closes", {})
    if targets:
        if refused.get(repo["id"]) == targets:
            return
        refused[repo["id"]] = targets
    elif repo["id"] in refused:
        del refused[repo["id"]]
    else:
        return
    if not refused:
        del entry["refused_closes"]
    state.save()


def _has_session_state(repo, read_set):
    _, tabs, rows = _ownership(repo, *read_set)
    return bool(tabs or rows)


class _Fence:
    """The pre-send check of one cleanup candidate.

    read() takes the catalog first and the terminal census last. A caller decides on that
    read set only, then authorize() checks the catalog record, the filesystem removal
    authority and the runtime without any further read, RPC or sleep before the send.
    Stock Orca offers no atomic fence for tab or terminal-handle close, so the remaining
    window is the request's own transit.
    """
    def __init__(self, rpc, scan, repo, record, evidence, runtime_id):
        self.rpc, self.scan, self.repo, self.record, self.evidence = rpc, scan, repo, record, evidence
        self.runtime_id = runtime_id

    def same_runtime(self, runtime_id):
        if runtime_id != self.runtime_id:
            raise CleanupRefusal("Orca cleanup runtime changed; retry sync")

    def read(self):
        self.rpc.refresh()
        catalog = _records(self.rpc, "repo.list", "repos")
        return catalog, _session_snapshots(self.rpc), _terminal_census(self.rpc)

    def authorize(self, read_set):
        if (_repo_at(read_set[0], self.repo["path"]) != self.repo
                or not verify_missing(self.scan, self.repo["path"])
                or self.record["sessions"] and removal_authorized(self.record, self.evidence) is not True):
            raise CleanupRefusal("Orca cleanup candidate changed; retry sync: " + self.repo["path"])
        self.same_runtime(self.rpc.runtime_id)


class _Stale(Exception):
    """The target left the recomputed plan before sending; nothing was sent."""


class _PreSendFailure(RpcError):
    """A read or check inside a pre-send callback failed; nothing was sent."""


class CleanupRefused(RpcError):
    """Orca definitively refused a close and readback still shows the target."""
    def __init__(self, message, target):
        super().__init__(message)
        self.target = target


REFUSED_CLOSE = "Orca refused a session close; repository is retained"
# Operator decision: a definitively refused close keeps the record protected; nothing more is
# closed for it, the pass can settle, and status warns that it needs a manual close in Orca.
REFUSED_CLOSE_CLASS = PROTECTED


def _request(action):
    kind = action[0]
    if kind == "stop":
        return "terminal.stopExact", {"worktree": "id:" + action[1], "expectedPtyIds": [action[2]],
                                      "targetOnly": True}
    if kind == "tab":
        return "session.tabs.close", {"worktree": "id:" + action[1], "tabId": action[2], "reason": "user"}
    if not isinstance(action[1], str) or not action[1]:
        raise RpcError("Orca cleanup terminal handle is unavailable; retry sync")
    return "terminal.close", {"terminal": action[1]}


def _send_and_confirm(rpc, repo, fence, action, deadline, refused_stops, allow_live):
    """One destructive request and its outcome.

    Pre-send: re-read all three inventories, recompute the plan, require the target still
    in it, then authorize; nothing is read between that decision and the send. A
    pre-send refusal sends nothing. A definitive refusal (an RPC rejection, or a tab close
    answered {closed: true, refused: true}) gets one readback: if the target is still
    planned the record is retained. An accepted or unknown outcome is polled until the
    target leaves the plan or the cleanup deadline passes.
    """
    method, params = _request(action)

    def before_send(runtime_id):
        fence.same_runtime(runtime_id)
        read_set = fence.read()
        plan = _cleanup_plan(repo, read_set, refused_stops)
        # Decision on this read set only; no reads follow.
        if plan is None:
            raise AmbiguousSession(AMBIGUOUS_SESSION)
        if not allow_live and (any(item[0] != "tab" for item in plan)
                               or not _only_stale_terminal_layouts(read_set[1], repo)):
            raise CleanupRefusal("Orca cleanup session state changed; retry sync")
        if action not in plan:
            raise _Stale()
        fence.authorize(read_set)

    def guarded(runtime_id):
        try:
            before_send(runtime_id)
        except (CleanupRefusal, _Stale):
            raise
        except RpcError as error:
            # Even a rejected nested read is only a pre-send failure of this request.
            raise _PreSendFailure(str(error)) from None

    try:
        result = rpc.call(method, params, before_send=guarded)
        refused = method == "session.tabs.close" and result.get("refused") is True
        unknown = False
    except (CleanupRefusal, _Stale, _PreSendFailure):
        raise
    except RpcError as error:
        if not (error.rejected or error.unknown):
            # Nothing was sent: a pre-send read or transport failure leaves this pass
            # undetermined, never a refusal and never a fallback.
            raise
        refused, unknown = error.rejected, error.unknown
    def planned():
        # A record that became protected stops here: nothing more of it is sent.
        plan = _cleanup_plan(repo, fence.read(), refused_stops)
        if plan is None:
            raise AmbiguousSession(AMBIGUOUS_SESSION)
        return action in plan
    if refused and not unknown:
        if action[0] == "stop":
            # A refused fenced stop falls back to the checked handle close.
            refused_stops.add(action[2])
            return
        if planned():
            raise CleanupRefused(REFUSED_CLOSE, list(action))
        return
    interval = READBACK_INTERVAL
    while planned():
        if time.monotonic() + interval >= deadline:
            raise RpcError("Orca session cleanup is unconfirmed; retry sync: " + repo["path"])
        time.sleep(interval)
        interval = min(interval * 2, READBACK_MAX_INTERVAL)


def _remove_sessions(rpc, repo, fence, deadline, allow_live=True):
    """Remove every saved tab and live terminal of one exact record by replanning after each
    confirmed step; nothing outside the plan of a closable record is ever addressed."""
    refused_stops = set()
    while True:
        if time.monotonic() >= deadline:
            raise RpcError("Orca registration time budget exhausted")
        plan = _cleanup_plan(repo, fence.read(), refused_stops)
        if plan is None:
            raise AmbiguousSession(AMBIGUOUS_SESSION)
        if not plan:
            return
        try:
            _send_and_confirm(rpc, repo, fence, plan[0], deadline, refused_stops, allow_live)
        except _Stale:
            continue


def _cleanup_records(scan, state, groups, repos):
    """Share exact ownership evidence between cleanup and read-only reporting."""
    desired = {root.path for project in scan.projects for root in project.roots}
    active = {project.project_id for project in scan.projects}
    local_groups = {group["id"] for group in groups if _local(group)}
    records = []
    for project_id, entry in state.data["projects"].items():
        root = entry["root"]
        confirmed = entry.get("confirmed_repos", {})
        legacy_paths = {root, *entry.get("known_roots", []), *entry.get("pending_repos", {})}
        legacy_removed = project_id not in active and verify_missing(scan, root)
        for repo in repos:
            path = repo["path"]
            if (path in desired or not (path == root or path.startswith(root + "/"))
                    or not (path == root or path in confirmed or repo.get("kind", "git") == "git")):
                continue
            exact = confirmed.get(path) == repo["id"]
            grouped = (entry.get("group_id") in local_groups
                       and repo.get("projectGroupId") == entry.get("group_id"))
            # A replacement at a remembered path is never adopted during cleanup.
            if path in confirmed and not exact:
                records.append((project_id, entry, repo, "repository identity changed"))
            elif exact or grouped or (legacy_removed and path in legacy_paths):
                if not _local(repo):
                    reason = "repository belongs to another execution host"
                elif repo.get("projectGroupId") not in (None, entry.get("group_id")):
                    reason = "repository belongs to another project group"
                else:
                    reason = None
                records.append((project_id, entry, repo, reason))
            elif _local(repo) and repo.get("projectGroupId") is None:
                records.append((project_id, entry, repo, "legacy repository identity is unconfirmed"))
        # A remembered ID moved elsewhere must also remain visible and retained.
        for path, repo_id in confirmed.items():
            repo = next((repo for repo in repos if repo["id"] == repo_id), None)
            if repo is not None and repo["path"] != path:
                records.append((project_id, entry, dict(repo, path=path), "repository path changed"))
    return records


def _prune_missing(report, scan, state, rpc, cleanup_pass, evidence, approved_ids=None):
    # Missing project roots may be temporarily unmounted. A known checkout
    # losing Git metadata is retained without blocking independent missing paths.
    if report["errors"]:
        return
    active = {project.project_id for project in scan.projects}
    # A registered project whose root is missing may be temporarily unmounted. Only its own
    # records are inhibited; other projects, including removed ones, are still cleaned.
    rootless = {project.project_id for project in scan.projects if not project.roots}
    # Removed projects cannot pass through registration again. Recover only
    # submitted identities, without creating or renaming native resources.
    if any(project_id not in active and (entry.get("pending_group") or entry.get("pending_repos"))
           for project_id, entry in state.data["projects"].items()):
        rpc.refresh()
    groups = _records(rpc, "projectGroup.list", "groups")
    intent_runtime = rpc.runtime_id
    repos = _records(rpc, "repo.list", "repos")
    if rpc.runtime_id != intent_runtime:
        raise RpcError("Orca cleanup runtime changed; retry sync")
    for project_id, entry in state.data["projects"].items():
        if project_id in active or not verify_missing(scan, entry["root"]):
            continue
        pending = entry.get("pending_group")
        if pending:
            matches = _pending_groups(groups, pending, entry["root"])
            if len(matches) > 1:
                raise RpcError("Ambiguous pending Orca group identity; cleanup retained")
            if matches:
                entry["group_id"] = matches[0]["id"]
                entry["group_name"] = pending["name"]
                del entry["pending_group"]
                state.save()
            elif pending["runtime_id"] != rpc.runtime_id:
                del entry["pending_group"]
                state.save()
        for path, pending in list(entry.get("pending_repos", {}).items()):
            if (pending.get("runtime_id") != rpc.runtime_id
                    and not any(repo["path"] == path for repo in repos)
                    and verify_missing(scan, path)):
                del entry["pending_repos"][path]
                state.save()
    candidates = _cleanup_records(scan, state, groups, repos)
    for project_id, entry, repo, reason in candidates:
        if reason or project_id in rootless or not verify_missing(scan, repo["path"]):
            continue
        if approved_ids is not None and repo["id"] not in approved_ids[0]:
            raise RpcError("plan_stale: unapproved Orca cleanup repository")
        if time.monotonic() >= state.deadline:
            raise RpcError("Orca registration time budget exhausted")
        # Legacy ownership comes from the persisted project/path relation, not
        # from absence alone. Pin the actual catalog ID before any native write.
        confirmed = entry.setdefault("confirmed_repos", {})
        if confirmed.get(repo["path"]) != repo["id"]:
            confirmed[repo["path"]] = repo["id"]
            state.save()
        # This API also covers saved tabs of now-missing linked worktrees. The
        # read only probes the inventory; the fence below reads it again.
        try:
            _session_snapshots(rpc)
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
            if not _has_session_state(repo, (repos, retry_snapshots, [])):
                raise error from None
            report["warnings"].append(
                "Missing Orca checkout has session tabs and is retained: " + repo["path"]
            )
            # Later candidates stay unvisited, so the pass remains incomplete.
            cleanup_pass.outcomes[repo["id"]] = "session tabs are retained"
            return
        proof_runtime = rpc.runtime_id
        record = {"projectId": project_id, "root": entry["root"],
                  "removedProject": project_id not in active, "sessions": False}
        fence = _Fence(rpc, scan, repo, record, evidence, proof_runtime)
        read_set = fence.read()
        closable, tabs, rows = _ownership(repo, *read_set)
        if not closable:
            cleanup_pass.outcomes[repo["id"]] = AMBIGUOUS_SESSION
            report["warnings"].append("Missing Orca checkout has session state it does not own alone "
                                      "and is retained: " + repo["path"])
            continue
        # A close Orca refused earlier stays protected and is not re-sent while its target remains.
        if entry.get("refused_closes", {}).get(repo["id"]):
            still = _refused_targets(entry, repo, read_set)
            _remember_refusals(state, entry, repo, still)
            if still:
                cleanup_pass.outcomes[repo["id"]] = REFUSED_CLOSE
                continue
        # Live session content: any terminal row, or a tab other than this checkout's stale layouts.
        record["sessions"] = bool(rows) or bool(tabs) and not _only_stale_terminal_layouts(read_set[1], repo)
        if removal_authorized(record, evidence) is not True:
            retained = "session tabs are retained" if tabs else "live terminals are retained"
            cleanup_pass.outcomes[repo["id"]] = retained
            report["warnings"].append("Missing Orca checkout has " + retained.split(" are ")[0]
                                      + " and is retained: " + repo["path"])
            continue
        fence.authorize(read_set)
        try:
            _remove_sessions(rpc, repo, fence, state.deadline, allow_live=record["sessions"])
        except AmbiguousSession:
            cleanup_pass.outcomes[repo["id"]] = AMBIGUOUS_SESSION
            continue
        except CleanupRefused as refusal:
            remembered = entry.get("refused_closes", {}).get(repo["id"], [])
            if refusal.target not in remembered:
                remembered = remembered + [refusal.target]
            _remember_refusals(state, entry, repo, remembered)
            cleanup_pass.outcomes[repo["id"]] = REFUSED_CLOSE
            continue

        def before_remove(runtime_id):
            fence.same_runtime(runtime_id)
            read_set = fence.read()
            # The repository is removed only while its plan is empty.
            plan = _cleanup_plan(repo, read_set)
            if plan is None:
                raise AmbiguousSession(AMBIGUOUS_SESSION)
            if plan:
                raise RpcError("Orca cleanup session state changed; retry sync")
            fence.authorize(read_set)

        try:
            rpc.call("repo.rm", {"repo": "id:" + repo["id"]}, before_send=before_remove)
        except AmbiguousSession:
            # The record became protected just before removal: keep it and continue the pass.
            cleanup_pass.outcomes[repo["id"]] = AMBIGUOUS_SESSION
            continue
        except RpcError as error:
            if not error.unknown:
                raise
        remaining = _records(rpc, "repo.list", "repos")
        if any(item["id"] == repo["id"] for item in remaining):
            raise RpcError("Orca repository removal is unconfirmed: " + repo["path"])
        _remember_refusals(state, entry, repo, [])
        # Keep the original identity if a replacement appeared during removal;
        # retiring it would let a later legacy-path retry adopt the new record.
        if not any(item["path"] == repo["path"] for item in remaining):
            if project_id in active:
                entry.get("confirmed_repos", {}).pop(repo["path"], None)
            entry.get("pending_repos", {}).pop(repo["path"], None)
            state.save()
    for project_id, entry in list(state.data["projects"].items()):
        group_id = entry.get("group_id")
        if project_id in active or not verify_missing(scan, entry["root"]):
            continue
        rpc.refresh()
        groups = _records(rpc, "projectGroup.list", "groups")
        repos = _records(rpc, "repo.list", "repos")
        # Keep prior IDs until the whole removed project retires. A replacement
        # arriving after this read must never fall back to legacy path ownership.
        live_ids = {repo["id"] for repo in repos}
        retained_confirmed = any(repo_id in live_ids or any(repo["path"] == path for repo in repos)
                                 for path, repo_id in entry.get("confirmed_repos", {}).items())
        folders = _records(rpc, "folderWorkspace.list", "folderWorkspaces")
        if group_id is not None and (any(repo.get("projectGroupId") == group_id for repo in repos + folders)
                or any(group.get("parentGroupId") == group_id for group in groups)):
            continue
        group = next((item for item in groups if item["id"] == group_id), None)
        if group is not None and not _local(group):
            continue
        if group is not None:
            if approved_ids is not None and group_id not in approved_ids[1]:
                raise RpcError("plan_stale: unapproved Orca cleanup group")
            proof_runtime = rpc.runtime_id

            def before_group_remove(runtime_id):
                if runtime_id != proof_runtime:
                    raise RpcError("Orca cleanup runtime changed; retry sync")
                rpc.refresh()
                current_groups = _records(rpc, "projectGroup.list", "groups")
                current_repos = _records(rpc, "repo.list", "repos")
                current_folders = _records(rpc, "folderWorkspace.list", "folderWorkspaces")
                if (next((item for item in current_groups if item["id"] == group_id), None) != group
                        or any(item.get("projectGroupId") == group_id for item in current_repos + current_folders)
                        or any(item.get("parentGroupId") == group_id for item in current_groups)
                        or not verify_missing(scan, entry["root"]) or rpc.runtime_id != proof_runtime):
                    raise RpcError("Orca cleanup group changed; retry sync")

            try:
                rpc.call("projectGroup.delete", {"groupId": group_id}, before_send=before_group_remove)
            except RpcError as error:
                if not error.unknown:
                    raise
            if any(group["id"] == group_id for group in _records(rpc, "projectGroup.list", "groups")):
                raise RpcError("Orca project group removal is unconfirmed")
        # An empty or already absent group says nothing about ungrouped records.
        if (retained_confirmed or entry.get("pending_repos") or entry.get("pending_group")
                or any(owner == project_id for owner, _, _, _ in _cleanup_records(scan, state, groups, repos))):
            state.save()
            continue
        del state.data["projects"][project_id]
        state.save()
    cleanup_pass.complete = True


def reconcile(scan, rpc, state_dir, apply=True, deadline=None, host_name="", known_paths=None):
    deadline = deadline if deadline is not None else time.monotonic() + 65
    report = {"ready": False, "registered": 0,
              "total": sum(len(project.roots) for project in scan.projects),
              "errors": list(scan.errors), "warnings": list(scan.warnings), "projects": [],
              "cleanup": [], "cleanupReady": True, "settled": False}
    cleanup_pass, evidence = CleanupPass(), RemovalEvidence(scan, report["errors"])
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
            # Only registration metadata and known-root Git-resolved identities are
            # fingerprinted; saved tabs and credentials never enter a fact or digest.
            keys = ("id", "path", "displayName", "kind", "projectGroupId", "parentPath",
                    "name", "createdFrom", "connectionId", "executionHostId", "parentGroupId",
                    "externalWorktreeVisibility")
            repos = _records(rpc, "repo.list", "repos")
            groups = _records(rpc, "projectGroup.list", "groups")
            safe_records = lambda records: sorted(
                [{key: record[key] for key in keys if key in record} for record in records],
                key=lambda record: record["id"])
            catalog = {"scope": scope, "repos": safe_records(repos),
                       "groups": safe_records(groups), "registration": state.data,
                       "rootMetadata": [{"path": root.path, "metadata": _root_metadata(root, deadline)}
                                        for project in sorted(scan.projects, key=lambda project: project.project_id)
                                        for root in sorted(project.roots, key=lambda root: root.path)]}
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
                try:
                    _prune_missing(report, scan, state, rpc, cleanup_pass, evidence, approved_ids)
                except RpcError as error:
                    report["errors"].append(str(error))
            rpc.refresh()
            _report_catalog(report, scan, state, rpc, host_name, cleanup_pass, evidence)
    except (RpcError, StateError) as error:
        report["errors"].append(str(error))
    report["errors"] = list(dict.fromkeys(report["errors"]))
    report["warnings"] = list(dict.fromkeys(report["warnings"]))
    report["ready"] = (not report["errors"] and report["registered"] == report["total"]
                       and report["cleanupReady"])
    # Like ready, but a removed project's remaining record may be protected. Pending work of
    # removed projects and any undetermined record never settle, including after partial progress.
    report["settled"] = (not report["errors"] and report["registered"] == report["total"]
                         and all(item["retention"] == PROTECTED if item["removedProject"]
                                 else item["retention"] != UNDETERMINED for item in report["cleanup"]))
    return report
