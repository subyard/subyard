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

    def run_sync(self, apply=True):
        return self.reconcile(self.discover(self.workspaces), self.rpc, self.state, apply=apply)

    def sidecar(self):
        return json.loads((self.state / "subyard-registration.json").read_text())

    def test_create_exact_names_and_one_group_per_project_id_despite_same_name(self):
        init_git(self.root / "packages/backend")
        other = project(self.workspaces, "other-id")
        report = self.run_sync()
        self.assertTrue(report["ready"], report)
        self.assertEqual((3, 3), (report["registered"], report["total"]))
        repos = {r["path"]: r for r in self.rpc.repos}
        self.assertEqual("Sample", repos[str(self.root)]["displayName"])
        self.assertEqual("packages/backend", repos[str(self.root / "packages/backend")]["displayName"])
        self.assertEqual(repos[str(self.root)]["projectGroupId"], repos[str(self.root / "packages/backend")]["projectGroupId"])
        self.assertNotEqual(repos[str(self.root)]["projectGroupId"], repos[str(other)]["projectGroupId"])
        before = copy.deepcopy((self.rpc.groups, self.rpc.repos))
        self.assertTrue(self.run_sync()["ready"])
        self.assertEqual(before, (self.rpc.groups, self.rpc.repos))
        self.assertEqual(2, len(self.sidecar()["projects"]))

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
        report = self.run_sync()
        self.assertTrue(report["ready"], report)
        self.assertEqual([str(self.root), str(self.root / "former")], [r["path"] for r in self.rpc.repos])
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
