"""Synthetic stock admission and measurement checks: no Git fixtures, sockets, processes or VMs."""
import importlib.util
from pathlib import Path
import unittest


spec = importlib.util.spec_from_file_location(
    "orca_load_probe", Path(__file__).resolve().parents[1] / "e2e/load-probe.py")
probe = importlib.util.module_from_spec(spec)
spec.loader.exec_module(probe)


class LoadProbeTests(unittest.TestCase):
    def test_cold_response_precedes_settlement_and_startup_cpu_is_separate(self):
        clock = [0.0, 0.0]
        events, timings, failures = [], [], []
        ticks = probe.os.sysconf("SC_CLK_TCK")

        def advance(seconds, cpu):
            clock[0] += seconds
            clock[1] += seconds * cpu / 100 * ticks

        def sleep(seconds):
            events.append(("sleep", seconds))
            advance(seconds, 120 if seconds == 25 else 2)

        def round_trip():
            events.append(("rpc", clock[0]))
            first = not timings
            timings.append(6000 if first else 200)
            failures.append(1 if first else 0)
            advance(1, 120 if first else 130)
            return {"worktree.list": {"max_ms": timings[-1], "failures": sum(failures)}}

        result = probe.measure_phase(lambda: (1, clock[1], clock[0]), round_trip,
                                     cold=True, sleep=sleep, monotonic=lambda: clock[0])
        self.assertEqual([("rpc", 0), ("sleep", 25), ("sleep", 10), ("rpc", 36)], events)
        self.assertEqual(120, result["startup_cpu_percent"])
        self.assertEqual(25, result["settle_seconds"])
        self.assertEqual(2, result["idle_cpu_percent"])
        self.assertLess(result["cpu_percent"], 80)
        self.assertEqual(130, result["demand_cpu_percent"])
        self.assertEqual(6000, result["first_max_rpc_ms"])
        self.assertEqual({"worktree.list": {"max_ms": 6000, "failures": 1}}, result["first_rpc"])
        self.assertEqual([6000, 200], timings)
        self.assertEqual(1, sum(failures), "a failed first response must remain in the phase's failure count")

    def test_stock_list_and_ps_report_missing_baseline_and_fixture_paths(self):
        root = Path("/fixture/load")
        repos = [{"id": "baseline", "path": "/fixture/main"},
                 {"id": "seed", "path": str(root / "seed")},
                 {"id": "loaded", "path": str(root / "checkout-0")}]
        for id_key in ("id", "worktreeId"):
            rows = [{id_key: repo["id"] + "::main", "repoId": repo["id"],
                     "path": repo["path"]} for repo in repos]
            self.assertEqual(0, probe.missing_worktree_paths(rows, repos, {"baseline"}, root))
            for incomplete in ([], rows[:1], rows[1:], rows[:-1],
                               [dict(row, repoId="wrong") for row in rows],
                               [dict(row, path=row["path"] + "/linked") for row in rows]):
                with self.subTest(id_key=id_key, rows=incomplete):
                    self.assertGreater(probe.missing_worktree_paths(incomplete, repos, {"baseline"}, root), 0)

    def test_cleaned_catalog_preserves_baseline_without_old_load_rows(self):
        repos = [{"id": "baseline", "path": "/fixture/main"}]
        self.assertEqual(0, probe.missing_worktree_paths(
            [{"repoId": "baseline", "path": "/fixture/main"}],
            repos, {"baseline"}, Path("/fixture/load")))
        self.assertEqual(1, probe.missing_worktree_paths([], repos, {"baseline"}, Path("/fixture/load")))



class NativeTelemetryTests(unittest.TestCase):
    def test_native_numeric_whitelist_and_queue_inclusive_spans(self):
        import json
        journal = '[main-thread] ' + json.dumps({
            't': 5000, 'maxGapMs': 75, 'gapsOver50Ms': 2, 'gapsOver250Ms': 0,
            'spawnCount': 3, 'spawns': {'secret command': {'count': 3,
            'blockMsTotal': 12.5, 'blockMsMax': 6}}, 'private': 'token'})
        trace = json.dumps({'name': 'git.exec', 'durationMs': 110,
                            'attributes': {'git.queue_wait_ms': 90, 'cwd': '/private', 'token': 'secret'}})
        result = probe.aggregate_native_telemetry(journal, trace)
        self.assertEqual(75, result['max_gap_ms'])
        self.assertEqual(3, result['spawn_count'])
        self.assertEqual(12.5, result['spawn_block_ms_total'])
        self.assertEqual(110, result['sampled_git_queue_inclusive_duration_p95_ms'])
        self.assertEqual(90, result['sampled_git_queue_wait_p95_ms'])
        self.assertTrue(all(isinstance(value, (int, float)) for value in result.values()))
        self.assertNotIn('secret', json.dumps(result))
        self.assertNotIn('/private', json.dumps(result))

    def test_markers_invalid_numbers_and_other_spans_are_ignored(self):
        import json
        result = probe.aggregate_native_telemetry(
            '[main-thread] {"marker":"startup","t":1}\n[main-thread] bad-json',
            '\n'.join(json.dumps(value) for value in (
                {'name': 'other', 'durationMs': 500},
                {'name': 'git.exec', 'durationMs': True, 'attributes': {'git.queue_wait_ms': -1}},
                {'name': 'git.exec', 'durationMs': float('nan')}))
        )
        self.assertTrue(all(value == 0 for value in result.values()))


class PruneProgressTests(unittest.TestCase):
    def test_cap_counts_all_native_rows_and_seed_is_optional(self):
        root = Path(probe.CLONE_ROOT) / ".build/load-diagnostic"
        baseline = [{"id": f"baseline{index}", "path": f"/fixture/baseline{index}"} for index in range(3)]
        state = {"repo_ids": [repo["id"] for repo in baseline], "tab_ids": ["saved"], "repo_count": 3}
        tabs = [{"id": "saved"}]
        for generated in (1000, 1400):
            for seed_admitted in (False, True):
                fixtures = [{"id": f"load{index}", "path": str(root / f"checkout-{index}")}
                            for index in range(997)]
                if seed_admitted:
                    fixtures[-1] = {"id": "seed", "path": str(root / "seed")}
                result = probe.admission_counts(baseline + fixtures, tabs, state, root, generated, True)
                self.assertEqual(997, result["admitted_fixture_repos"])
                self.assertEqual(generated + 1 - 997, result["deferred_fixture_roots"])
                self.assertEqual(1, result["admission_complete"])
                self.assertEqual(0, probe.admission_counts(baseline + fixtures[:-1], tabs, state, root,
                                                         generated, True)["admission_complete"])
                with self.assertRaises(probe.ProbeError):
                    probe.admission_counts(baseline + fixtures + [{"id": "extra", "path": str(root / "extra")}],
                                           tabs, state, root, generated, True)
        self.assertEqual(1, probe.admission_counts(baseline, tabs, state, root, 1400, False)["admission_complete"])
        self.assertEqual(0, probe.admission_counts(baseline + fixtures[:1], tabs, state, root,
                                                 1400, False)["admission_complete"])
        for repos, saved_tabs in ((baseline[1:], tabs), (baseline, [])):
            with self.assertRaises(probe.ProbeError):
                probe.admission_counts(repos, saved_tabs, state, root, 1000, True)

    def test_exact_root_counts_seed_and_checkout_without_sibling_prefix(self):
        root = Path(probe.CLONE_ROOT) / ".build/load-diagnostic"
        baseline = {"repo_ids": ["kept"], "tab_ids": ["tab"], "repo_count": 1}
        repos = [{"id": "kept", "path": probe.CLONE_ROOT},
                 {"id": "seed", "path": str(root / "seed")},
                 {"id": "load", "path": str(root / "checkout-0")},
                 {"id": "sibling", "path": str(root) + "-other/checkout-0"}]
        tabs = [{"id": "tab"}]
        self.assertEqual(2, probe.remaining_fixture_repos(repos, tabs, baseline, root))
        self.assertEqual(0, probe.remaining_fixture_repos(repos[:1], tabs, baseline, root))
        for missing_repos, missing_tabs in ((repos[1:], tabs), (repos, [])):
            with self.assertRaises(probe.ProbeError):
                probe.remaining_fixture_repos(missing_repos, missing_tabs, baseline, root)
        with self.assertRaises(probe.ProbeError):
            probe.remaining_fixture_repos(repos, tabs, baseline, root.parent)

    def test_invalid_baseline_and_ambiguous_repo_paths_fail_closed(self):
        root = Path(probe.CLONE_ROOT) / ".build/load-diagnostic"
        valid = {"repo_ids": ["repo"], "tab_ids": ["tab"], "repo_count": 1}
        for state in ({"repo_ids": [], "tab_ids": ["tab"]},
                      {"repo_ids": ["repo"], "tab_ids": []},
                      {"repo_ids": ["repo", "repo"], "tab_ids": ["tab"]},
                      {"repo_ids": ["repo"], "tab_ids": [None]}):
            with self.assertRaises(probe.ProbeError):
                probe.validate_baseline(dict(state, repo_count=1))
        for count in (0, True, 2, 1001):
            with self.assertRaises(probe.ProbeError):
                probe.validate_baseline(dict(valid, repo_count=count))
        with self.assertRaises(probe.ProbeError):
            probe.validate_baseline({"repo_ids": ["repo"], "tab_ids": ["tab"]})
        for path in ("relative", str(root / "../other"), None):
            with self.assertRaises(probe.ProbeError):
                probe.remaining_fixture_repos([{"id": "repo", "path": path}], [{"id": "tab"}], valid, root)


class SyncFailureTests(unittest.TestCase):
    def test_only_allowlisted_budget_and_lock_errors_are_retryable(self):
        prefix = "Orca registration error: "
        budget = prefix + "Orca registration time budget exhausted"
        lock = prefix + "Another Orca registration invocation holds the lock"
        self.assertEqual("budget", probe.classify_sync_failure(budget + "\n" + budget + "; result is incomplete"))
        self.assertEqual("lock", probe.classify_sync_failure(lock + "\n" + lock))
        self.assertEqual("mixed", probe.classify_sync_failure(budget + "\n" + lock))
        self.assertEqual("mixed", probe.classify_sync_failure(budget + "\n" + prefix + "private unknown failure"))
        self.assertEqual("unknown", probe.classify_sync_failure(prefix + "private: Orca registration time budget exhausted"))
        self.assertEqual("unknown", probe.classify_sync_failure("private unknown output"))
        self.assertEqual("unknown", probe.classify_sync_failure(budget + "x" * 65536))

    def test_timeout_metadata_has_closed_method_and_numeric_elapsed(self):
        prefix = "Orca registration error: /private/repo: Orca runtime request timed out: "
        self.assertEqual("rpc-timeout:repo.rm:5.0", probe.classify_sync_failure(prefix + "repo.rm (5.0s)"))
        for value in ("secret.method (5.0s)", "repo.rm (secret)", "repo.rm (NaNs)", "repo.rm (5.0s) private"):
            self.assertEqual("unknown", probe.classify_sync_failure(prefix + value))


if __name__ == "__main__":
    unittest.main()
