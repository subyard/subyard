import copy
import json
import os
from pathlib import Path
import shutil
import tempfile
import threading
import unittest
from unittest import mock

from support import Catalog, init_git, project


class ReconcileTests(unittest.TestCase):
    def setUp(self):
        from discovery import discover
        from reconcile import reconcile
        from transport import RpcError
        self.discover = discover
        self.reconcile = reconcile
        self.error = RpcError
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.workspaces = Path(self.tmp.name) / "workspaces"
        self.state = Path(self.tmp.name) / "state"
        self.root = project(self.workspaces)
        self.rpc = Catalog()
        self.host_name = "owner-host"

    def run_sync(self, apply=True):
        return self.reconcile(self.discover(self.workspaces), self.rpc, self.state, apply=apply,
                              host_name=self.host_name)

    def prepare_missing_repo(self):
        init_git(self.root / "gone")
        self.assertTrue(self.run_sync()["ready"])
        repo = next(repo for repo in self.rpc.repos if repo["path"] == str(self.root / "gone"))
        shutil.rmtree(self.root / "gone")
        return repo

    def sidecar(self):
        return json.loads((self.state / "subyard-registration.json").read_text())

    def test_exact_scope_rejects_new_root_before_registration_writes(self):
        approved = self.run_sync(apply=False)["catalogDigest"]
        init_git(self.root / "new-root")
        self.rpc.calls.clear()
        with mock.patch.dict(os.environ, {"SUBYARD_ORCA_REGISTRATION_SCOPE": approved}):
            report = self.run_sync()
        self.assertFalse(report["ready"])
        self.assertIn("plan_stale", " ".join(report["errors"]))
        self.assertFalse(any(method not in ("repo.list", "projectGroup.list", "folderWorkspace.list")
                             for method, _ in self.rpc.calls))

    def test_exact_catalog_rejects_new_missing_record_before_any_write(self):
        self.assertTrue(self.run_sync()["ready"])
        approved = self.run_sync(apply=False)["catalogDigest"]
        self.rpc.repos.append({"id": "new-missing", "path": str(self.root / "missing"),
                               "kind": "git", "projectGroupId": self.rpc.groups[0]["id"]})
        self.rpc.calls.clear()
        with mock.patch.dict(os.environ, {"SUBYARD_ORCA_REGISTRATION_SCOPE": approved}):
            report = self.run_sync()
        self.assertIn("plan_stale", " ".join(report["errors"]))
        self.assertFalse(any(method not in ("repo.list", "projectGroup.list", "folderWorkspace.list")
                             for method, _ in self.rpc.calls))

    def test_exact_scope_does_not_prune_record_appearing_after_first_write(self):
        self.assertTrue(self.run_sync()["ready"])
        self.host_name = "renamed-host"
        approved = self.run_sync(apply=False)["catalogDigest"]
        new_record = {"id": "newly-appeared", "path": str(self.root / "missing-new"),
                      "kind": "git", "projectGroupId": self.rpc.groups[0]["id"]}
        def appeared(method, _):
            if method == "projectGroup.update":
                self.rpc.repos.append(new_record)
        self.rpc.after = appeared
        with mock.patch.dict(os.environ, {"SUBYARD_ORCA_REGISTRATION_SCOPE": approved}):
            report = self.run_sync()
        self.assertFalse(report["ready"])
        self.assertIn("plan_stale", " ".join(report["errors"]))
        self.assertIn(new_record, self.rpc.repos)
        self.assertFalse(any(method == "repo.rm" and params["repo"] == "id:newly-appeared"
                             for method, params in self.rpc.calls))

    def fill_catalog(self, total):
        # Catalog-only rows: admission regressions create no mass Git fixtures.
        self.rpc.repos.extend({"id": "manual-" + str(index), "path": "/manual/" + str(index),
                               "kind": "folder", "executionHostId": "remote"}
                              for index in range(len(self.rpc.repos), total))

    def test_admission_rechecks_growth_after_the_cached_catalog_read(self):
        self.assertTrue(self.run_sync()["ready"])
        self.fill_catalog(999)
        init_git(self.root / "new-checkout")
        original = self.rpc.call
        reads = 0

        def changed(method, params=None, **kwargs):
            nonlocal reads
            result = original(method, params, **kwargs)
            if method == "repo.list":
                reads += 1
                if reads == 1:
                    self.fill_catalog(1000)
            return result

        self.rpc.call = changed
        self.rpc.calls.clear()
        report = self.run_sync()
        self.assertEqual(1, report["deferred"])
        self.assertEqual(1000, len(self.rpc.repos))
        self.assertFalse(any(method == "repo.add" for method, _ in self.rpc.calls))

    def test_definitely_rejected_add_does_not_reserve_capacity(self):
        self.assertTrue(self.run_sync()["ready"])
        self.fill_catalog(999)
        rejected = self.root / "a-rejected"
        accepted = self.root / "b-accepted"
        init_git(rejected)
        init_git(accepted)
        original = self.rpc.call

        def reject_one(method, params=None, before_send=None):
            if method == "repo.add" and params["path"] == str(rejected):
                before_send(self.rpc.runtime_id)
                raise self.error("request rejected")
            return original(method, params, before_send=before_send)

        self.rpc.call = reject_one
        report = self.run_sync()
        self.assertFalse(report["ready"])
        self.assertEqual(1000, len(self.rpc.repos))
        self.assertIn(str(accepted), [repo["path"] for repo in self.rpc.repos])
        self.assertNotIn(str(rejected), [repo["path"] for repo in self.rpc.repos])
        self.assertEqual(1, report["deferred"])
        self.rpc.call = original
        self.rpc.repos = [repo for repo in self.rpc.repos if repo["path"] != str(accepted)]
        shutil.rmtree(accepted)
        self.assertTrue(self.run_sync()["ready"])
        self.assertIn(str(rejected), [repo["path"] for repo in self.rpc.repos])

    def test_admission_stops_at_1000_and_existing_records_remain_repairable(self):
        self.fill_catalog(999)
        self.assertTrue(self.run_sync()["ready"])
        self.assertEqual(1000, len(self.rpc.repos))
        existing = self.rpc.repos[-1]
        existing.update(displayName="My project", sessionIds=["saved-session"])
        existing_id = existing["id"]
        init_git(self.root)
        init_git(self.root / "new-checkout")
        self.rpc.calls.clear()
        report = self.run_sync()
        self.assertFalse(report["ready"])
        self.assertEqual(1, report["deferred"])
        self.assertIn("admission limit reached", " ".join(report["errors"]))
        self.assertEqual(1000, len(self.rpc.repos))
        self.assertFalse(any(method in ("repo.add", "repo.rm") for method, _ in self.rpc.calls))
        self.assertEqual((existing_id, "My project", ["saved-session"], "git"),
                         tuple(existing[key] for key in ("id", "displayName", "sessionIds", "kind")))
        # A pre-existing over-cap catalog is retained, too.
        self.rpc.repos.append({"id": "outside-limit", "path": "/manual/outside", "kind": "folder"})
        self.rpc.repos[-2]["externalWorktreeVisibility"] = "show"
        self.assertFalse(self.run_sync()["ready"])
        self.assertEqual(1001, len(self.rpc.repos))
        self.assertEqual("hide", existing["externalWorktreeVisibility"])

    def test_at_capacity_no_new_project_group_or_add_intent_is_created(self):
        self.fill_catalog(1000)
        before = copy.deepcopy(self.rpc.repos)
        report = self.run_sync()
        self.assertFalse(report["ready"])
        self.assertEqual(1, report["deferred"])
        self.assertEqual(before, self.rpc.repos)
        self.assertEqual([], self.rpc.groups)
        self.assertEqual({}, self.sidecar()["projects"]["sample-id"]["pending_repos"])
        self.assertTrue(all(method.endswith(".list") for method, _ in self.rpc.calls))
        self.assertEqual(1, self.run_sync(apply=False)["deferred"])

    def test_pending_unknown_add_reserves_the_last_slot_across_projects(self):
        project(self.workspaces, "other-id", "Other")
        self.fill_catalog(999)
        original = self.rpc.call
        requests = []

        def delayed(method, params=None, before_send=None):
            if method == "repo.add":
                before_send(self.rpc.runtime_id)
                requests.append(params)
                raise self.error("lost response", unknown=True)
            return original(method, params, before_send=before_send)

        self.rpc.call = delayed
        report = self.run_sync()
        self.assertFalse(report["ready"])
        self.assertEqual(1, len(requests))
        self.assertEqual(1, report["deferred"])
        self.assertFalse(self.run_sync()["ready"])
        self.assertEqual(1, len(requests))
        # The old service can no longer finish the unknown request.
        self.rpc.runtime_id = "runtime-2"
        self.rpc.call = original
        report = self.run_sync()
        self.assertFalse(report["ready"])
        self.assertEqual(1000, len(self.rpc.repos))
        self.assertEqual(1, report["registered"])
        self.assertEqual(1, report["deferred"])

    def test_cap_deferral_does_not_suppress_safe_pruning_and_next_sync_admits(self):
        gone = self.prepare_missing_repo()
        init_git(self.root / "new-checkout")
        self.fill_catalog(1000)
        report = self.run_sync()
        self.assertFalse(report["ready"])
        self.assertNotIn(gone["id"], [repo["id"] for repo in self.rpc.repos])
        self.assertEqual(999, len(self.rpc.repos))
        self.assertTrue(self.run_sync()["ready"])
        self.assertEqual(1000, len(self.rpc.repos))

    def test_cap_rejections_reuse_catalog_and_skip_per_root_git_probes(self):
        from discovery import Root
        from reconcile import verify_root
        self.assertTrue(self.run_sync()["ready"])
        self.fill_catalog(1000)
        scan = self.discover(self.workspaces)
        scan.projects[0].roots.extend(Root(str(self.root / ("new-" + str(index))), "New", "git")
                                     for index in range(10))
        self.rpc.calls.clear()
        with mock.patch("reconcile.verify_root", wraps=verify_root) as verify:
            report = self.reconcile(scan, self.rpc, self.state, host_name=self.host_name)
        self.assertEqual(10, report["deferred"])
        self.assertFalse(report["ready"])
        self.assertEqual([str(self.root)], [call.args[0].path for call in verify.call_args_list])
        self.assertEqual(2, sum(method == "repo.list" for method, _ in self.rpc.calls))
        self.assertTrue(all(method in ("repo.list", "projectGroup.list") for method, _ in self.rpc.calls))

    def test_create_exact_names_and_one_group_per_project_id_despite_same_name(self):
        init_git(self.root / "packages/backend")
        other = project(self.workspaces, "other-id")
        report = self.run_sync()
        self.assertTrue(report["ready"], report)
        self.assertEqual((3, 3), (report["registered"], report["total"]))
        self.assertEqual(["Sample / owner-host"] * 2, [g["name"] for g in self.rpc.groups])
        repos = {r["path"]: r for r in self.rpc.repos}
        self.assertEqual("Sample", repos[str(self.root)]["displayName"])
        self.assertEqual("packages/backend", repos[str(self.root / "packages/backend")]["displayName"])
        self.assertEqual(repos[str(self.root)]["projectGroupId"], repos[str(self.root / "packages/backend")]["projectGroupId"])
        self.assertNotEqual(repos[str(self.root)]["projectGroupId"], repos[str(other)]["projectGroupId"])
        before = copy.deepcopy((self.rpc.groups, self.rpc.repos))
        self.assertTrue(self.run_sync()["ready"])
        self.assertEqual(before, (self.rpc.groups, self.rpc.repos))
        self.assertEqual(2, len(self.sidecar()["projects"]))

    def test_legacy_group_name_and_host_rename_converge_without_replacing_group(self):
        self.host_name = ""
        self.assertTrue(self.run_sync()["ready"])
        group = self.rpc.groups[0]
        group.update(color="purple", tabOrder=31)
        original = copy.deepcopy(group)
        sidecar = self.sidecar()
        del sidecar["projects"]["sample-id"]["group_name"]
        (self.state / "subyard-registration.json").write_text(json.dumps(sidecar))
        for host in ("owner-host", "renamed-host"):
            self.host_name = host
            self.assertFalse(self.run_sync(apply=False)["ready"])
            self.assertTrue(self.run_sync()["ready"])
            self.assertEqual(dict(original, name="Sample / " + host), group)
            self.rpc.calls.clear()
            self.assertTrue(self.run_sync()["ready"])
            self.assertFalse(any(method == "projectGroup.update" for method, _ in self.rpc.calls))

    def test_group_rename_unknown_response_requires_readback(self):
        self.assertTrue(self.run_sync()["ready"])
        self.host_name = "renamed-host"
        def lost(method, params):
            if method == "projectGroup.update":
                raise self.error("lost response", unknown=True)
        self.rpc.after = lost
        self.assertTrue(self.run_sync()["ready"])
        self.assertEqual("Sample / renamed-host", self.rpc.groups[0]["name"])

        original_call = self.rpc.call
        def not_applied(method, params=None, **kwargs):
            if method == "projectGroup.update":
                raise self.error("lost response", unknown=True)
            return original_call(method, params, **kwargs)
        self.rpc.call = not_applied
        self.host_name = "another-host"
        self.assertFalse(self.run_sync()["ready"])

    def test_existing_manual_group_and_names_preserved_while_only_project_repos_move(self):
        group = self.rpc.call("projectGroup.create", {"name": "Manual", "createdFrom": "manual"})["group"]
        self.rpc.groups[0].update(color="purple", tabOrder=31)
        for path in (str(self.root), "/unrelated/project"):
            repo = self.rpc.call("repo.add", {"path": path, "kind": "folder"})["repo"]
            self.rpc.call("repo.update", {"repo": "id:" + repo["id"], "updates": {"displayName": "Manual name", "badgeColor": "orange"}})
            self.rpc.call("projectGroup.moveProject", {"repo": "id:" + repo["id"], "groupId": group["id"], "order": 42})
        before_group = copy.deepcopy(self.rpc.groups[0])
        before_other = copy.deepcopy(self.rpc.repos[1])
        self.assertTrue(self.run_sync()["ready"])
        self.assertEqual(before_group, self.rpc.groups[0])
        self.assertEqual(before_other, self.rpc.repos[1])
        self.assertEqual("Manual name", self.rpc.repos[0]["displayName"])
        self.assertEqual("orange", self.rpc.repos[0]["badgeColor"])
        self.assertEqual(42, self.rpc.repos[0]["projectGroupOrder"])
        self.assertNotEqual(group["id"], self.rpc.repos[0]["projectGroupId"])
        self.rpc.groups[1].update(name="Renamed group", color="blue", tabOrder=19)
        self.rpc.repos[0].update(displayName="Renamed root", projectGroupOrder=9)
        self.host_name = "renamed-host"
        before = copy.deepcopy((self.rpc.groups, self.rpc.repos))
        self.assertTrue(self.run_sync()["ready"])
        self.assertEqual(before, (self.rpc.groups, self.rpc.repos))

    def test_root_kind_changes_in_place_keep_name_membership_order_sessions(self):
        self.assertTrue(self.run_sync()["ready"])
        original = self.rpc.repos[0]
        original.update(displayName="My root", badgeColor="red", projectGroupOrder=99,
                        sessionIds=["retained-session"], externalWorktreeVisibility="show")
        stable = {k: original[k] for k in ("id", "path", "displayName", "badgeColor", "projectGroupId", "projectGroupOrder", "sessionIds")}
        init_git(self.root)
        self.assertFalse(self.run_sync(apply=False)["ready"])
        self.assertTrue(self.run_sync()["ready"])
        self.assertEqual("git", original["kind"])
        self.assertEqual("hide", original["externalWorktreeVisibility"])
        shutil.rmtree(self.root / ".git")
        self.assertTrue(self.run_sync()["ready"])
        self.assertEqual("folder", original["kind"])
        self.assertEqual(stable, {k: original[k] for k in stable})

    def test_deleted_records_or_group_recreated_for_existing_directories(self):
        self.assertTrue(self.run_sync()["ready"])
        self.rpc.groups.clear()
        self.rpc.repos.clear()
        self.assertFalse(self.run_sync(apply=False)["ready"])
        self.assertTrue(self.run_sync()["ready"])
        self.assertEqual((1, 1), (len(self.rpc.groups), len(self.rpc.repos)))

    def test_stale_paths_and_former_nested_git_remain_with_warning(self):
        init_git(self.root / "nested")
        self.assertTrue(self.run_sync()["ready"])
        before = copy.deepcopy(self.rpc.repos)
        shutil.rmtree(self.root / "nested/.git")
        report = self.run_sync()
        self.assertTrue(report["ready"], report)
        self.assertTrue(report["warnings"])
        self.assertEqual(1, report["total"])
        self.assertEqual(before, self.rpc.repos)
        shutil.rmtree(self.root)
        report = self.run_sync()
        self.assertTrue(report["ready"], report)
        self.assertEqual(0, report["total"])
        self.assertTrue(report["warnings"])
        self.assertEqual(before, self.rpc.repos)
        self.assertFalse(self.root.exists())

    def test_missing_nested_checkout_pruned_but_existing_former_git_retained(self):
        init_git(self.root / "gone")
        init_git(self.root / "former")
        self.assertTrue(self.run_sync()["ready"])
        shutil.rmtree(self.root / "gone")
        shutil.rmtree(self.root / "former/.git")
        before = copy.deepcopy(self.rpc.repos)
        self.assertTrue(self.run_sync(apply=False)["ready"])
        self.assertEqual(before, self.rpc.repos)
        report = self.reconcile(self.discover(self.workspaces, recursive=False),
                                self.rpc, self.state, known_paths=[])
        self.assertTrue(report["ready"], report)
        self.assertEqual([str(self.root), str(self.root / "former")], [r["path"] for r in self.rpc.repos])
        self.assertTrue(report["warnings"])
        self.assertTrue((self.root / "former").is_dir())
        self.assertTrue(self.run_sync()["ready"])

    def test_removed_project_prunes_owned_records_and_empty_group(self):
        init_git(self.root / "nested")
        self.assertTrue(self.run_sync()["ready"])
        shutil.rmtree(self.root.parent)
        report = self.run_sync()
        self.assertTrue(report["ready"], report)
        self.assertEqual([], self.rpc.repos)
        self.assertEqual([], self.rpc.groups)
        self.assertEqual({}, self.sidecar()["projects"])

    def test_missing_ungrouped_git_checkouts_pruned_with_the_same_safety_guards(self):
        self.assertTrue(self.run_sync()["ready"])
        path = self.root / ".build/cleanup-trial/checkout"
        init_git(path)
        repo = self.rpc.call("repo.add", {"path": str(path), "kind": "git"})["repo"]
        # Legacy or interrupted registration has no managed membership.
        # Neither the discovery inventory nor sidecar knows this root.
        retained = []
        for label, changes in (
                ("manual", {"projectGroupId": "manual"}),
                ("unknown-group", {"projectGroupId": "unknown"}),
                ("remote", {"executionHostId": "remote"}),
                ("connected", {"connectionId": "remote"}),
                ("folder", {"kind": "folder"}),
                ("saved", {})):
            record = dict(repo, id=label, path=str(path / label), **changes)
            self.rpc.repos.append(record)
            retained.append(copy.deepcopy(record))
        self.rpc.snapshots = [{"worktree": "saved::/old/worktree", "tabs": [{"id": "saved"}]}]
        shutil.rmtree(path)
        before = copy.deepcopy(self.rpc.repos)
        self.assertTrue(self.run_sync(apply=False)["ready"])
        self.assertEqual(before, self.rpc.repos)
        report = self.reconcile(self.discover(self.workspaces, recursive=False),
                                self.rpc, self.state, known_paths=[])
        self.assertTrue(report["ready"], report)
        self.assertEqual(before[:1] + retained, self.rpc.repos)
        self.assertTrue(any("session tabs" in warning for warning in report["warnings"]))

    def test_cleanup_preserves_session_tabs_manual_and_remote_records(self):
        init_git(self.root / "gone")
        self.assertTrue(self.run_sync()["ready"])
        repo = self.rpc.repos[1]
        # A disconnected tab on another worktree still belongs to this repo.
        self.rpc.snapshots = [{"worktree": repo["id"] + "::/old/worktree", "tabs": [{"id": "saved"}]}]
        for changes in ({"id": "manual", "projectGroupId": "manual", "path": str(self.root / "manual")},
                        {"id": "outside", "path": str(self.root) + "-outside"},
                        {"id": "remote", "executionHostId": "remote"},
                        {"id": "manual-folder", "path": str(self.root / "manual-folder"), "kind": "folder"}):
            self.rpc.repos.append(dict(repo, **changes))
        shutil.rmtree(self.root / "gone")
        before = copy.deepcopy(self.rpc.repos)
        report = self.run_sync()
        self.assertTrue(report["ready"], report)
        self.assertEqual(before, self.rpc.repos)
        self.assertTrue(any("session tabs" in warning for warning in report["warnings"]))
        self.rpc.snapshots.clear()
        self.assertTrue(self.run_sync()["ready"])
        self.assertEqual(before[:1] + before[2:], self.rpc.repos)

    def pending_terminal_snapshot(self, repo):
        return {"worktree": repo["id"] + "::" + repo["path"], "tabs": [{
            "id": "saved-terminal", "type": "terminal", "status": "pending-handle",
            "terminal": None, "ptyId": "old-pty",
        }]}

    def test_missing_checkout_with_stale_terminal_layout_is_pruned(self):
        repo = self.prepare_missing_repo()
        self.rpc.snapshots = [self.pending_terminal_snapshot(repo)]
        self.assertTrue(self.run_sync(apply=False)["ready"])
        self.assertIn(repo, self.rpc.repos)
        self.assertEqual(1, len(self.rpc.snapshots))
        report = self.run_sync()
        self.assertTrue(report["ready"], report)
        self.assertNotIn(repo, self.rpc.repos)
        self.assertEqual([dict(self.pending_terminal_snapshot(repo), tabs=[])], self.rpc.snapshots)
        closes = [params for method, params in self.rpc.calls if method == "session.tabs.close"]
        self.assertEqual([{"worktree": "id:" + repo["id"] + "::" + repo["path"],
                           "tabId": "saved-terminal", "reason": "user"}], closes)
        methods = [method for method, _ in self.rpc.calls]
        self.assertLess(methods.index("session.tabs.close"), methods.index("repo.rm"))
        census = [params for method, params in self.rpc.calls if method == "terminal.list"]
        self.assertTrue(census)
        self.assertTrue(all(params.get("requireFreshPtyLiveness") is True for params in census))
        self.assertFalse(any("worktree" in params or "handles" in params for params in census))

    def test_cleanup_retains_saved_tabs_without_explicit_stale_terminal_proof(self):
        for index, changes in enumerate((
                {"type": "browser"}, {"type": "editor"}, {"status": "ready"},
                {"terminal": "live-handle"}, {"ptyId": None}, {"status": None},
                {"sessionId": "saved-session"}, {"agentSessionId": "agent-session"},
                {"agentStatus": {"providerSession": "resumable-session"}},
                {"launchDraft": {"command": "retained draft"}},
                {"parentLayout": {"buffersByLeafId": {"leaf": "saved buffer"}}},
                {"parentLayout": {"scrollbackRefsByLeafId": {"leaf": "saved scrollback"}}},
                {"parentLayout": {"chatLeafId": "saved-chat"}}, {"parentLayout": "unknown"},
                {"parentTabId": []}, {"parentTabId": ""})):
            with self.subTest(changes=changes):
                self.rpc = Catalog()
                self.state = Path(self.tmp.name) / ("saved-" + str(index))
                repo = self.prepare_missing_repo()
                snapshot = self.pending_terminal_snapshot(repo)
                snapshot["tabs"][0].update(changes)
                self.rpc.snapshots = [snapshot]
                report = self.run_sync()
                self.assertTrue(report["ready"], report)
                self.assertIn(repo, self.rpc.repos)
                self.assertFalse(any(method == "terminal.list" for method, _ in self.rpc.calls))

    def test_cleanup_retains_stale_layout_for_a_different_worktree(self):
        repo = self.prepare_missing_repo()
        snapshot = self.pending_terminal_snapshot(repo)
        snapshot["worktree"] = repo["id"] + "::/another/worktree"
        self.rpc.snapshots = [snapshot]
        report = self.run_sync()
        self.assertTrue(report["ready"], report)
        self.assertIn(repo, self.rpc.repos)

    def test_cleanup_retains_live_orphan_by_worktree_or_saved_pty(self):
        for index, (pty, worktree) in enumerate((
                ("different-pty", "same"), ("different-pty", "related"), ("old-pty", "other::/elsewhere"))):
            with self.subTest(pty=pty, worktree=worktree):
                self.rpc = Catalog()
                self.state = Path(self.tmp.name) / ("live-" + str(index))
                repo = self.prepare_missing_repo()
                self.rpc.snapshots = [self.pending_terminal_snapshot(repo)]
                key = repo["id"] + "::" + repo["path"] if worktree == "same" else (
                    repo["id"] + "::/linked" if worktree == "related" else worktree)
                self.rpc.terminals = [{"ptyId": pty, "worktreeId": key, "executionHostId": "local",
                                       "connected": True, "orphaned": True}]
                report = self.run_sync()
                self.assertTrue(report["ready"], report)
                self.assertIn(repo, self.rpc.repos)
                self.assertEqual(1, len(self.rpc.snapshots))

    def test_stale_terminal_cleanup_refuses_incomplete_or_invalid_live_inventory(self):
        complete = {"terminals": [], "totalCount": 0, "truncated": False,
                    "hostScope": {"hostIds": ["local"], "omittedHostIds": []}}
        variants = (
            {}, dict(complete, truncated=True), dict(complete, totalCount=1),
            dict(complete, hostScope={"hostIds": [], "omittedHostIds": ["local"]}),
            dict(complete, hostScope={"hostIds": ["local"], "omittedHostIds": ["local"]}),
            dict(complete, terminals=[{"ptyId": "pty", "worktreeId": None,
                                      "executionHostId": "local", "connected": True}], totalCount=1),
        )
        for index, inventory in enumerate(variants):
            with self.subTest(inventory=inventory):
                self.rpc = Catalog()
                self.state = Path(self.tmp.name) / ("incomplete-" + str(index))
                repo = self.prepare_missing_repo()
                self.rpc.snapshots = [self.pending_terminal_snapshot(repo)]
                self.rpc.terminal_inventory = inventory
                report = self.run_sync()
                self.assertFalse(report["ready"], report)
                self.assertIn(repo, self.rpc.repos)
                self.assertFalse(any(method == "repo.rm" for method, _ in self.rpc.calls))

    def test_stale_terminal_cleanup_retains_new_saved_tab_before_removal(self):
        repo = self.prepare_missing_repo()
        self.rpc.snapshots = [self.pending_terminal_snapshot(repo)]
        reads = 0

        def new_saved_tab(method, _):
            nonlocal reads
            if method == "session.tabs.listAll":
                reads += 1
                if reads == 2:
                    self.rpc.snapshots[0]["tabs"].append({"id": "new-browser", "type": "browser"})

        self.rpc.after = new_saved_tab
        report = self.run_sync()
        self.assertTrue(report["ready"], report)
        self.assertEqual(2, reads)
        self.assertIn(repo, self.rpc.repos)

    def test_stale_terminal_cleanup_keeps_record_when_fresh_liveness_fails(self):
        repo = self.prepare_missing_repo()
        self.rpc.snapshots = [self.pending_terminal_snapshot(repo)]
        original = self.rpc.call

        def unavailable(method, params=None, **kwargs):
            if method == "terminal.list":
                raise self.error("fresh terminal liveness unavailable", timed_out=True)
            return original(method, params, **kwargs)

        self.rpc.call = unavailable
        report = self.run_sync()
        self.assertFalse(report["ready"], report)
        self.assertIn(repo, self.rpc.repos)
        self.assertFalse(any(method == "repo.rm" for method, _ in self.rpc.calls))

    def test_stale_terminal_cleanup_refuses_runtime_replaced_after_proof(self):
        repo = self.prepare_missing_repo()
        self.rpc.snapshots = [self.pending_terminal_snapshot(repo)]

        def replaced(method, _):
            if method == "terminal.list":
                self.rpc.runtime_id = "replacement-runtime"

        self.rpc.after = replaced
        report = self.run_sync()
        self.assertFalse(report["ready"], report)
        self.assertIn(repo, self.rpc.repos)
        self.assertIn("Orca cleanup runtime changed; retry sync", report["errors"])
        self.assertFalse(any(method == "repo.rm" for method, _ in self.rpc.calls))

    def test_stale_layout_close_rereads_snapshot_before_sending(self):
        repo = self.prepare_missing_repo()
        self.rpc.snapshots = [self.pending_terminal_snapshot(repo)]

        def new_tab(method, _):
            if method == "terminal.list":
                self.rpc.snapshots[0]["tabs"].append({"id": "new-editor", "type": "editor"})

        self.rpc.after = new_tab
        report = self.run_sync()
        self.assertFalse(report["ready"], report)
        self.assertIn(repo, self.rpc.repos)
        self.assertFalse(any(method in ("session.tabs.close", "repo.rm") for method, _ in self.rpc.calls))

    def test_stale_layout_close_refreshes_liveness_before_sending(self):
        repo = self.prepare_missing_repo()
        self.rpc.snapshots = [self.pending_terminal_snapshot(repo)]
        reads = 0

        def revived(method, _):
            nonlocal reads
            if method == "session.tabs.listAll":
                reads += 1
                if reads == 3:
                    self.rpc.terminals = [{"ptyId": "revived-pty",
                        "worktreeId": repo["id"] + "::" + repo["path"],
                        "executionHostId": "local", "connected": True}]

        self.rpc.after = revived
        report = self.run_sync()
        self.assertFalse(report["ready"], report)
        self.assertIn(repo, self.rpc.repos)
        self.assertFalse(any(method in ("session.tabs.close", "repo.rm") for method, _ in self.rpc.calls))

    def test_stale_layout_closes_each_parent_once_and_keeps_unrelated_tabs(self):
        repo = self.prepare_missing_repo()
        snapshot = self.pending_terminal_snapshot(repo)
        snapshot["tabs"][0]["parentTabId"] = "parent-one"
        snapshot["tabs"].extend([
            dict(snapshot["tabs"][0], id="second-leaf", ptyId="second-pty"),
            dict(snapshot["tabs"][0], id="third-leaf", ptyId="third-pty", parentTabId="parent-two"),
        ])
        other = {"worktree": "unrelated::/checkout", "tabs": [{"id": "kept", "type": "browser"}]}
        self.rpc.snapshots = [snapshot, other]
        report = self.run_sync()
        self.assertTrue(report["ready"], report)
        self.assertNotIn(repo, self.rpc.repos)
        self.assertEqual([], self.rpc.snapshots[0]["tabs"])
        self.assertEqual(other, self.rpc.snapshots[1])
        closes = [params["tabId"] for method, params in self.rpc.calls if method == "session.tabs.close"]
        self.assertEqual(["parent-one", "parent-two"], closes)

    def test_stale_layout_close_unknown_reply_uses_readback_without_retry(self):
        repo = self.prepare_missing_repo()
        self.rpc.snapshots = [self.pending_terminal_snapshot(repo)]

        def lost_reply(method, _):
            if method == "session.tabs.close":
                raise self.error("lost close reply", unknown=True)

        self.rpc.after = lost_reply
        report = self.run_sync()
        self.assertTrue(report["ready"], report)
        self.assertNotIn(repo, self.rpc.repos)
        self.assertEqual(1, sum(method == "session.tabs.close" for method, _ in self.rpc.calls))

    def test_stale_layout_close_requires_confirmed_removal_before_repo_rm(self):
        repo = self.prepare_missing_repo()
        self.rpc.snapshots = [self.pending_terminal_snapshot(repo)]
        original = self.rpc.call

        def refused(method, params=None, before_send=None):
            if method == "session.tabs.close":
                if before_send:
                    before_send(self.rpc.runtime_id)
                self.rpc.calls.append((method, params))
                return {"closed": True, "refused": True}
            return original(method, params, before_send=before_send)

        self.rpc.call = refused
        report = self.run_sync()
        self.assertFalse(report["ready"], report)
        self.assertIn(repo, self.rpc.repos)
        self.assertTrue(self.rpc.snapshots[0]["tabs"])
        self.assertFalse(any(method == "repo.rm" for method, _ in self.rpc.calls))

    def test_stale_layout_close_keeps_repo_if_followup_tab_identity_is_invalid(self):
        repo = self.prepare_missing_repo()
        self.rpc.snapshots = [self.pending_terminal_snapshot(repo)]

        def malformed(method, _):
            if method == "session.tabs.close":
                self.rpc.snapshots[0]["tabs"].append("invalid-tab")

        self.rpc.after = malformed
        report = self.run_sync()
        self.assertFalse(report["ready"], report)
        self.assertIn(repo, self.rpc.repos)
        self.assertIn("Orca returned an invalid session tab identity; cleanup skipped", report["errors"])
        self.assertFalse(any(method == "repo.rm" for method, _ in self.rpc.calls))

    def test_timed_out_saved_tab_read_retries_once_and_positive_result_preserves_repo(self):
        repo = self.prepare_missing_repo()
        original_call = self.rpc.call
        deadline = 12345.0
        self.rpc.deadline = deadline
        list_calls = 0
        observed_deadlines = []

        def timeout_then_saved_tab(method, params=None, **kwargs):
            nonlocal list_calls
            if method == "session.tabs.listAll":
                list_calls += 1
                observed_deadlines.append(self.rpc.deadline)
                if list_calls == 1:
                    raise self.error("first read timed out", timed_out=True)
                return {"snapshots": [{"worktree": repo["id"] + "::/old/worktree",
                                       "tabs": [{"id": "saved"}]}]}
            return original_call(method, params, **kwargs)

        self.rpc.call = timeout_then_saved_tab
        report = self.run_sync()
        self.assertTrue(report["ready"], report)
        self.assertEqual(2, list_calls)
        self.assertEqual([deadline, deadline], observed_deadlines)
        self.assertEqual(deadline, self.rpc.deadline)
        self.assertIn(repo, self.rpc.repos)
        self.assertTrue(any("session tabs" in warning for warning in report["warnings"]))
        self.assertFalse(any(method == "repo.rm" for method, _ in self.rpc.calls))

    def test_positive_fallback_stops_all_later_repo_and_empty_group_pruning(self):
        first_path = self.root / "gone-first"
        later_path = self.root / "gone-later"
        retired_root = project(self.workspaces, "zz-retired")
        for path in (first_path, later_path, retired_root):
            init_git(path)
        self.assertTrue(self.run_sync()["ready"])
        first = next(repo for repo in self.rpc.repos if repo["path"] == str(first_path))
        later = next(repo for repo in self.rpc.repos if repo["path"] == str(later_path))
        retired = next(repo for repo in self.rpc.repos if repo["path"] == str(retired_root))
        shutil.rmtree(first_path)
        shutil.rmtree(later_path)
        shutil.rmtree(retired_root.parent)

        # Fix candidate order so the positive fallback occurs before another
        # missing repo and an otherwise deletable project group.
        active = [repo for repo in self.rpc.repos
                  if repo["path"] == str(self.root)]
        self.rpc.repos = active + [first, later]
        self.assertNotIn(retired, self.rpc.repos)
        before_repos = copy.deepcopy(self.rpc.repos)
        before_groups = copy.deepcopy(self.rpc.groups)
        original_call = self.rpc.call
        list_calls = 0

        def timeout_then_saved_tab(method, params=None, **kwargs):
            nonlocal list_calls
            if method == "session.tabs.listAll":
                list_calls += 1
                if list_calls == 1:
                    raise self.error("first read timed out", timed_out=True)
                return {"snapshots": [{"worktree": first["id"] + "::/old/worktree",
                                       "tabs": [{"id": "saved"}]}]}
            return original_call(method, params, **kwargs)

        self.rpc.call = timeout_then_saved_tab
        report = self.run_sync()
        self.assertTrue(report["ready"], report)
        self.assertEqual(before_groups, self.rpc.groups)
        self.assertEqual(before_repos, self.rpc.repos)
        self.assertEqual(2, list_calls)
        self.assertFalse(any(method in ("repo.rm", "projectGroup.delete")
                             for method, _ in self.rpc.calls))
        self.assertTrue(any("session tabs" in warning for warning in report["warnings"]))

    def test_timed_out_saved_tab_read_never_prunes_without_positive_retry_evidence(self):
        outcomes = (
            ("empty", {"snapshots": []}),
            ("unrelated", {"snapshots": [{"worktree": "other::/worktree", "tabs": [{"id": "saved"}]}]}),
            ("invalid", {"snapshots": [{"worktree": "missing-tabs"}]}),
            ("second-timeout", self.error("second read timed out", timed_out=True)),
        )
        for index, (name, retry_result) in enumerate(outcomes):
            with self.subTest(name=name):
                self.rpc = Catalog()
                self.state = Path(self.tmp.name) / ("state-" + str(index))
                repo = self.prepare_missing_repo()
                original_call = self.rpc.call
                list_calls = 0

                def timeout_then_result(method, params=None, **kwargs):
                    nonlocal list_calls
                    if method == "session.tabs.listAll":
                        list_calls += 1
                        if list_calls == 1:
                            raise self.error("original saved-tab read timed out", timed_out=True)
                        if isinstance(retry_result, Exception):
                            raise retry_result
                        return retry_result
                    return original_call(method, params, **kwargs)

                self.rpc.call = timeout_then_result
                report = self.run_sync()
                self.assertFalse(report["ready"], report)
                self.assertEqual(2, list_calls)
                self.assertIn("original saved-tab read timed out", " ".join(report["errors"]))
                self.assertIn(repo, self.rpc.repos)
                self.assertFalse(any(method == "repo.rm" for method, _ in self.rpc.calls))

    def test_saved_tab_read_retries_only_typed_known_timeout(self):
        for index, error in enumerate((self.error("known read failure"),
                                        self.error("ambiguous timeout", unknown=True, timed_out=True))):
            with self.subTest(unknown=error.unknown, timed_out=error.timed_out):
                self.rpc = Catalog()
                self.state = Path(self.tmp.name) / ("state-no-retry-" + str(index))
                self.prepare_missing_repo()
                original_call = self.rpc.call
                list_calls = 0

                def fail_without_retry(method, params=None, **kwargs):
                    nonlocal list_calls
                    if method == "session.tabs.listAll":
                        list_calls += 1
                        raise error
                    return original_call(method, params, **kwargs)

                self.rpc.call = fail_without_retry
                report = self.run_sync()
                self.assertFalse(report["ready"], report)
                self.assertEqual(1, list_calls)
                self.assertFalse(any(method == "repo.rm" for method, _ in self.rpc.calls))

    def test_cleanup_skips_incomplete_scan_and_unavailable_or_changed_workspace(self):
        init_git(self.root / "gone")
        self.assertTrue(self.run_sync()["ready"])
        shutil.rmtree(self.root / "gone")
        before = copy.deepcopy(self.rpc.repos)
        scan = self.discover(self.workspaces)
        scan.errors.append("scan incomplete")
        self.assertFalse(self.reconcile(scan, self.rpc, self.state)["ready"])
        self.assertEqual(before, self.rpc.repos)
        scan = self.discover(self.workspaces)
        self.workspaces.rename(Path(self.tmp.name) / "old")
        self.assertFalse(self.run_sync()["ready"])
        self.assertEqual(before, self.rpc.repos)
        self.workspaces.mkdir()
        self.reconcile(scan, self.rpc, self.state)
        self.assertEqual(before, self.rpc.repos)

    def test_cleanup_rechecks_reappearing_directory_and_rejects_invalid_session_catalog(self):
        init_git(self.root / "gone")
        self.assertTrue(self.run_sync()["ready"])
        shutil.rmtree(self.root / "gone")
        self.rpc.snapshots = [{"worktree": "invalid"}]
        self.assertFalse(self.run_sync()["ready"])
        self.assertEqual(2, len(self.rpc.repos))
        self.rpc.snapshots = []
        def reappear(method, params):
            if method == "session.tabs.listAll":
                (self.root / "gone").mkdir()
        self.rpc.after = reappear
        self.assertFalse(self.run_sync()["ready"])
        self.assertEqual(2, len(self.rpc.repos))

    def test_cleanup_unknown_remove_reply_is_verified_and_failed_removal_retried(self):
        init_git(self.root / "gone")
        self.assertTrue(self.run_sync()["ready"])
        shutil.rmtree(self.root / "gone")
        original = self.rpc.call
        def rejected(method, params=None, **kwargs):
            if method == "repo.rm":
                raise self.error("rejected")
            return original(method, params, **kwargs)
        with mock.patch.object(self.rpc, "call", side_effect=rejected):
            self.assertFalse(self.run_sync()["ready"])
        self.assertEqual(2, len(self.rpc.repos))
        def lost(method, params):
            if method == "repo.rm":
                raise self.error("lost reply", unknown=True)
        self.rpc.after = lost
        self.assertTrue(self.run_sync()["ready"])
        self.assertEqual(1, len(self.rpc.repos))

    def test_removed_project_group_preserves_manual_children_and_native_folders(self):
        self.assertTrue(self.run_sync()["ready"])
        group_id = self.rpc.groups[0]["id"]
        shutil.rmtree(self.root.parent)
        self.rpc.groups.append({"id": "child", "parentGroupId": group_id})
        self.assertTrue(self.run_sync()["ready"])
        self.assertEqual(2, len(self.rpc.groups))
        self.rpc.groups.pop()
        self.rpc.folders.append({"id": "folder", "projectGroupId": group_id})
        self.assertTrue(self.run_sync()["ready"])
        self.assertEqual(1, len(self.rpc.groups))
        self.rpc.folders.clear()
        self.assertTrue(self.run_sync()["ready"])
        self.assertEqual([], self.rpc.groups)

    def test_one_rejected_repo_does_not_skip_other_repos_or_projects(self):
        init_git(self.root / "broken")
        init_git(self.root / "healthy")
        other = project(self.workspaces, "zzz-id", "Second")
        original = self.rpc.call

        def call(method, params=None, **kwargs):
            if method == "repo.add" and params["path"].endswith("/broken"):
                raise self.error("Orca runtime rejected repo.add")
            return original(method, params, **kwargs)

        self.rpc.call = call
        report = self.run_sync()
        self.assertFalse(report["ready"])
        self.assertEqual((3, 4), (report["registered"], report["total"]))
        self.assertTrue(report["errors"])
        self.assertIn(str(other), [r["path"] for r in self.rpc.repos])
        self.rpc.call = original
        self.assertTrue(self.run_sync()["ready"])

    def test_partial_discovery_never_claims_ready_but_applies_known_roots(self):
        scan = self.discover(self.workspaces)
        scan.errors.append("Fixture partial scan")
        report = self.reconcile(scan, self.rpc, self.state)
        self.assertFalse(report["ready"])
        self.assertEqual((1, 1), (report["registered"], report["total"]))

    @unittest.skipIf(os.geteuid() == 0, "root bypasses directory search permissions")
    def test_unsearchable_directory_does_not_skip_healthy_siblings(self):
        blocked = self.root / "packages/aaa-blocked"
        sibling = self.root / "packages/zzz-healthy"
        later = self.root / "zzz-other"
        for checkout in (blocked / "hidden-checkout", sibling, later):
            init_git(checkout)
        self.addCleanup(blocked.chmod, 0o755)
        blocked.chmod(0o444)

        report = self.run_sync()

        self.assertCountEqual([str(self.root), str(sibling), str(later)],
                              [repo["path"] for repo in self.rpc.repos])
        self.assertFalse(report["ready"], report)
        self.assertEqual((3, 3), (report["registered"], report["total"]))
        self.assertTrue(any(str(blocked) in error for error in report["errors"]), report)
        self.assertEqual(0o444, blocked.stat().st_mode & 0o777)
        blocked.chmod(0o755)
        self.assertTrue(self.run_sync()["ready"])
        self.assertEqual(4, len(self.rpc.repos))

    def test_unknown_git_probe_never_mutates_kind(self):
        self.assertTrue(self.run_sync()["ready"])
        (self.root / ".git").write_text("gitdir: /nonexistent/orca-fixture\n")
        report = self.run_sync()
        self.assertFalse(report["ready"])
        self.assertEqual("folder", self.rpc.repos[0]["kind"])

    def test_lost_group_reply_recovers_by_durable_pending_identity_without_duplicate(self):
        def lost(method, params):
            if method == "projectGroup.create":
                self.assertIn("pending_group", self.sidecar()["projects"]["sample-id"])
                self.rpc.after = None
                raise self.error("lost response", unknown=True)
        self.rpc.after = lost
        self.assertTrue(self.run_sync()["ready"])
        self.assertTrue(self.run_sync()["ready"])
        self.assertEqual(1, len(self.rpc.groups))

    def test_interruption_after_group_create_before_mapping_recovers_across_invocations(self):
        def interrupted(method, params):
            if method == "projectGroup.create":
                raise KeyboardInterrupt()
        self.rpc.after = interrupted
        with self.assertRaises(KeyboardInterrupt):
            self.run_sync()
        self.rpc.after = None
        self.rpc.groups[0]["name"] = "Manual rename while interrupted"
        self.assertTrue(self.run_sync()["ready"])
        self.assertEqual(1, len(self.rpc.groups))
        self.assertEqual("Manual rename while interrupted", self.rpc.groups[0]["name"])

    def test_ambiguous_pending_group_refuses_adoption_and_further_create(self):
        def interrupted(method, params):
            if method == "projectGroup.create":
                raise KeyboardInterrupt()
        self.rpc.after = interrupted
        with self.assertRaises(KeyboardInterrupt):
            self.run_sync()
        self.rpc.after = None
        self.rpc.groups.append(dict(self.rpc.groups[0], id="foreign-group"))
        report = self.run_sync()
        self.assertFalse(report["ready"])
        self.assertIn("ambiguous", " ".join(report["errors"]).lower())
        self.assertEqual(2, len(self.rpc.groups))

    def test_unknown_create_absent_is_not_blindly_repeated_in_same_runtime(self):
        original = self.rpc.call

        def lost(method, params=None, **kwargs):
            if method == "projectGroup.create":
                raise self.error("unknown write", unknown=True)
            return original(method, params, **kwargs)
        self.rpc.call = lost
        self.assertFalse(self.run_sync()["ready"])
        self.rpc.call = original
        self.assertFalse(self.run_sync()["ready"])
        self.assertEqual([], self.rpc.groups)
        self.rpc.runtime_id = "runtime-2"
        self.assertTrue(self.run_sync()["ready"])
        self.assertEqual(1, len(self.rpc.groups))

    def test_interrupted_first_repo_add_finishes_initial_name_but_keeps_manual_rename(self):
        def interrupted(method, params):
            if method == "repo.add":
                raise KeyboardInterrupt()
        self.rpc.after = interrupted
        with self.assertRaises(KeyboardInterrupt):
            self.run_sync()
        self.rpc.after = None
        self.assertTrue(self.run_sync()["ready"])
        self.assertEqual("Sample", self.rpc.repos[0]["displayName"])
        self.assertEqual(1, len(self.rpc.repos))

    def test_lost_initial_name_reply_does_not_overwrite_manual_name_on_retry(self):
        def interrupted(method, params):
            if method == "repo.update":
                raise KeyboardInterrupt()
        self.rpc.after = interrupted
        with self.assertRaises(KeyboardInterrupt):
            self.run_sync()
        self.rpc.after = None
        self.rpc.repos[0]["displayName"] = "Changed manually"
        self.assertTrue(self.run_sync()["ready"])
        self.assertEqual("Changed manually", self.rpc.repos[0]["displayName"])

    def test_status_does_not_create_sidecar_or_mutate_existing_catalog(self):
        self.assertFalse(self.run_sync(apply=False)["ready"])
        self.assertFalse(self.state.exists())
        self.assertTrue(self.run_sync()["ready"])
        before = {p.name: (p.read_bytes(), p.stat().st_mtime_ns) for p in self.state.iterdir()}
        self.rpc.calls.clear()
        self.assertTrue(self.run_sync(apply=False)["ready"])
        self.assertTrue(all(method.endswith(".list") for method, _ in self.rpc.calls))
        after = {p.name: (p.read_bytes(), p.stat().st_mtime_ns) for p in self.state.iterdir()}
        self.assertEqual(before, after)

    def test_corrupt_sidecar_fails_without_catalog_writes(self):
        self.state.mkdir()
        (self.state / "subyard-registration.json").write_text("not-json")
        report = self.run_sync()
        self.assertFalse(report["ready"])
        self.assertEqual([], self.rpc.groups)

    def test_two_concurrent_invocations_share_one_group_and_repo(self):
        barrier = threading.Barrier(2)
        results = []
        failures = []

        def run():
            try:
                scan = self.discover(self.workspaces)
                barrier.wait(timeout=2)
                results.append(self.reconcile(scan, self.rpc, self.state))
            except Exception as exc:
                failures.append(exc)
        threads = [threading.Thread(target=run) for _ in range(2)]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join(3)
            self.assertFalse(thread.is_alive())
        self.assertEqual([], failures)
        self.assertEqual(2, len(results))
        self.assertTrue(all(r["ready"] for r in results), results)
        self.assertEqual((1, 1), (len(self.rpc.groups), len(self.rpc.repos)))

    def test_metadata_rotation_before_create_send_does_not_permit_unknown_create_retry(self):
        original = self.rpc.call
        creates = []

        def rotated(method, params=None, before_send=None):
            if method == "projectGroup.create":
                creates.append(params)
                self.rpc.runtime_id = "runtime-rotated"
                if before_send:
                    before_send(self.rpc.runtime_id)
                raise self.error("lost response", unknown=True)
            return original(method, params)
        self.rpc.call = rotated
        self.assertFalse(self.run_sync()["ready"])
        self.assertEqual(1, len(creates))

    def test_git_checkout_visibility_is_verified_and_repaired(self):
        self.assertTrue(self.run_sync()["ready"])
        init_git(self.root)
        original = self.rpc.call

        def bad_visibility(method, params=None, **kwargs):
            result = original(method, params, **kwargs)
            if method == "repo.update" and "kind" in params["updates"]:
                self.rpc.repos[0]["externalWorktreeVisibility"] = "show"
            return result
        self.rpc.call = bad_visibility
        self.assertFalse(self.run_sync()["ready"])
        self.assertFalse(self.run_sync(apply=False)["ready"])
        self.rpc.call = original
        self.assertTrue(self.run_sync()["ready"])
        self.assertEqual("hide", self.rpc.repos[0]["externalWorktreeVisibility"])

    def test_remote_same_path_record_is_not_adopted_or_mutated(self):
        remote = self.rpc.call("repo.add", {"path": str(self.root), "kind": "folder"})["repo"]
        self.rpc.repos[0].update(executionHostId="ssh:foreign", connectionId="foreign",
                                 displayName="Remote checkout", projectGroupId="manual")
        before = copy.deepcopy(self.rpc.repos[0])
        other = project(self.workspaces, "zzz-id", "Other")
        report = self.run_sync()
        self.assertFalse(report["ready"])
        self.assertEqual(before, self.rpc.repos[0])
        self.assertEqual(1, report["registered"])
        self.assertIn(str(other), [r["path"] for r in self.rpc.repos])
        self.assertFalse(self.run_sync(apply=False)["ready"])

    def test_local_record_is_selected_when_foreign_host_has_same_path(self):
        self.assertTrue(self.run_sync()["ready"])
        remote = dict(self.rpc.repos[0], id="remote-id", executionHostId="runtime:foreign",
                      displayName="Remote copy", projectGroupId="foreign-group")
        self.rpc.repos.insert(0, remote)
        before = copy.deepcopy(remote)
        self.assertTrue(self.run_sync()["ready"])
        self.assertEqual(before, self.rpc.repos[0])

    def test_git_changed_after_scan_does_not_apply_stale_kind(self):
        self.assertTrue(self.run_sync()["ready"])
        init_git(self.root)
        scan = self.discover(self.workspaces)
        shutil.rmtree(self.root / ".git")
        report = self.reconcile(scan, self.rpc, self.state)
        self.assertFalse(report["ready"])
        self.assertEqual("folder", self.rpc.repos[0]["kind"])

    def test_adopt_legacy_unnamed_records_once_preserving_id_and_explicit_basename(self):
        init_git(self.root / "nested")
        root = self.rpc.call("repo.add", {"path": str(self.root), "kind": "folder"})["repo"]
        self.rpc.call("repo.add", {"path": str(self.root / "nested"), "kind": "git"})
        self.rpc.repos[0].pop("displayName")
        self.rpc.repos[1]["displayName"] = ""
        self.assertTrue(self.run_sync()["ready"])
        self.assertEqual(root["id"], self.rpc.repos[0]["id"])
        self.assertEqual(["Sample", "nested"], [repo["displayName"] for repo in self.rpc.repos])
        self.rpc.repos[0]["displayName"] = "src"
        self.assertTrue(self.run_sync()["ready"])
        self.assertEqual("src", self.rpc.repos[0]["displayName"])

    def test_existing_group_identity_with_remote_host_fails_without_moving_repos(self):
        self.assertTrue(self.run_sync()["ready"])
        self.rpc.groups[0]["executionHostId"] = "runtime:foreign"
        before = copy.deepcopy(self.rpc.repos)
        self.assertFalse(self.run_sync()["ready"])
        self.assertFalse(self.run_sync(apply=False)["ready"])
        self.assertEqual(before, self.rpc.repos)

    def test_status_rejects_sidecar_project_root_mismatch(self):
        self.assertTrue(self.run_sync()["ready"])
        data = self.sidecar()
        data["projects"]["sample-id"]["root"] = "/different/root"
        (self.state / "subyard-registration.json").write_text(json.dumps(data))
        self.assertFalse(self.run_sync(apply=False)["ready"])

    def test_lost_repo_add_reply_recovers_initial_name_without_duplicate(self):
        def lost(method, params):
            if method == "repo.add":
                self.rpc.after = None
                raise self.error("lost reply", unknown=True)
        self.rpc.after = lost
        self.assertTrue(self.run_sync()["ready"])
        self.assertEqual(1, len(self.rpc.repos))
        self.assertEqual("Sample", self.rpc.repos[0]["displayName"])

    def test_delayed_repo_add_is_not_resent_until_runtime_replacement(self):
        original = self.rpc.call
        requests = []

        def delayed(method, params=None, before_send=None):
            if method == "repo.add":
                if before_send:
                    before_send(self.rpc.runtime_id)
                requests.append(dict(params))
                raise self.error("response timed out before insertion", unknown=True)
            return original(method, params, before_send=before_send)

        self.rpc.call = delayed
        self.assertFalse(self.run_sync()["ready"])
        self.assertFalse(self.run_sync()["ready"])
        self.assertEqual(1, len(requests))
        self.rpc.runtime_id = "runtime-replacement"
        self.assertFalse(self.run_sync()["ready"])
        self.assertEqual(2, len(requests))
        self.assertFalse(self.run_sync()["ready"])
        self.assertEqual(2, len(requests))
        # The outstanding request eventually completes; exact-path recovery
        # finishes naming and membership without submitting another add.
        original("repo.add", requests[-1])
        self.assertTrue(self.run_sync()["ready"])
        self.assertEqual(2, len(requests))
        self.assertEqual(1, len(self.rpc.repos))
        self.assertEqual("Sample", self.rpc.repos[0]["displayName"])

    def test_repo_intent_uses_runtime_at_send_after_metadata_rotation(self):
        original = self.rpc.call
        requests = []

        def rotated(method, params=None, before_send=None):
            if method == "repo.add":
                self.rpc.runtime_id = "rotated-runtime"
                if before_send:
                    before_send(self.rpc.runtime_id)
                requests.append(dict(params))
                raise self.error("lost reply", unknown=True)
            return original(method, params, before_send=before_send)

        self.rpc.call = rotated
        self.assertFalse(self.run_sync()["ready"])
        pending = self.sidecar()["projects"]["sample-id"]["pending_repos"][str(self.root)]
        self.assertEqual("rotated-runtime", pending.get("runtime_id"))
        self.assertFalse(self.run_sync()["ready"])
        self.assertEqual(1, len(requests))

    def test_rejected_repo_add_can_be_retried_in_same_runtime(self):
        original = self.rpc.call

        def rejected(method, params=None, before_send=None):
            if method == "repo.add":
                if before_send:
                    before_send(self.rpc.runtime_id)
                raise self.error("request rejected")
            return original(method, params, before_send=before_send)

        self.rpc.call = rejected
        self.assertFalse(self.run_sync()["ready"])
        self.rpc.call = original
        self.assertTrue(self.run_sync()["ready"])
        self.assertEqual(1, len(self.rpc.repos))

    def test_lost_group_readback_does_not_discard_pending_create_identity(self):
        original = self.rpc.call
        created = False

        def lost(method, params=None, **kwargs):
            nonlocal created
            if method == "projectGroup.list" and created:
                raise self.error("readback unavailable")
            result = original(method, params, **kwargs)
            if method == "projectGroup.create":
                created = True
                raise self.error("lost reply", unknown=True)
            return result
        self.rpc.call = lost
        self.assertFalse(self.run_sync()["ready"])
        self.assertIn("pending_group", self.sidecar()["projects"]["sample-id"])
        self.rpc.call = original
        self.assertTrue(self.run_sync()["ready"])
        self.assertEqual(1, len(self.rpc.groups))

    def test_atomic_sidecar_replace_failure_prevents_create(self):
        from unittest import mock
        with mock.patch("state.os.replace", side_effect=OSError("fixture")):
            report = self.run_sync()
        self.assertFalse(report["ready"])
        self.assertEqual([], self.rpc.groups)
        self.assertFalse((self.state / "subyard-registration.json").exists())

    def test_duplicate_local_catalog_path_is_incomplete(self):
        self.assertTrue(self.run_sync()["ready"])
        self.rpc.repos.append(dict(self.rpc.repos[0], id="duplicate-id"))
        self.assertFalse(self.run_sync()["ready"])

    def test_malformed_catalog_response_is_incomplete(self):
        original = self.rpc.call

        def invalid(method, params=None, **kwargs):
            if method == "repo.list":
                return {"repos": [{"id": "without-path"}]}
            return original(method, params, **kwargs)
        self.rpc.call = invalid
        self.assertFalse(self.run_sync()["ready"])
