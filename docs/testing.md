# Testing changes

## Keep tests proportional

Choose additional tests by concrete failure risk and existing coverage.

- Add a case for a distinct regression or meaningful observable contract that is not
  adequately covered. Extend an existing test when it fits.
- Use the narrowest boundary that proves the behavior. Another layer or combination
  of inputs must catch a different failure. Assert outcomes and stable interfaces;
  reserve source-text assertions for explicit source-level contracts.
- For prose, formatting and mechanical edits with unchanged behavior, use existing
  checks. New tests need a concrete behavior risk, not merely a changed file.
- Preserve targeted checks for permissions, data loss, migrations, atomic updates
  and recovery. A small code change can still carry a large risk.
- If a small feature needs extensive fixtures, a parser or a process harness,
  reconsider the implementation and scope first. Remove obsolete behavior and its
  tests together; keep protections for behavior that remains.
- Mocks prove our adapter's behavior. Claims about an external CLI or protocol need
  evidence from the real consumer; otherwise state that compatibility is unverified.

These rules govern adding tests. Run the existing required gates below; during
development use focused checks and repeat broader checks after relevant changes or failures.

## Run the core checks

Run `./tests/run.sh` against the current source files, including uncommitted edits. No Git history,
clean checkout, base commit or `.git` directory is required. Install the tools listed in
[the test guide](test-vms.md), including Git and ripgrep: tests of Git behavior create their own
temporary repositories. The release packaging test also creates its own index of the current public
files; it does not change the source checkout's index.

The runner prints one start/result line per check and a final `SUMMARY` with its
status, check count, elapsed seconds and original exit code. Successful check output
stays in separate logs under a unique `.build/test-runs/run.*/` directory. On the
first failure it stops, prints the last 40 log lines and points to the complete log.
`RESULTS` and `SUMMARY` identify the run's `summary.tsv`, which has these columns:
`kind`, `suite`, `check`, `status`, `exit_code`, `duration_seconds`, `log`.
Log names are relative to that summary's directory. Each completed check has a
`check` row; the final `run` row reports the whole invocation. A missing final row
means the run did not finish reporting; absent checks have not passed. Durations
use Bash's elapsed seconds and may be zero for short checks. Separate invocations
never overwrite each other's results. Logs stay local until explicitly removed;
they are not uploaded by CI. Agents should read the summary first and open only
the relevant log when a check fails.

GitHub CI runs the full core gate, warning-level ShellCheck and
`bash tests/real-host/adapter-contracts.sh` in one `verify` job on every branch push and pull request;
tag pushes are reserved for the independent Release workflow. Native Paseo uses the same branch/PR
trigger boundary, while Release builds its native artifacts independently.
The separate nightly/manual Deep CI runs repeated Go race tests and one minute of parser fuzzing.
Both workflows use standard Ubuntu runners, read-only repository permissions and no artifact uploads.
Veranda, native Paseo and live P0 acceptance remain separate checks.

Agent compatibility regressions run locally with
`go test ./internal/adapters/statusruntime` and `bash tests/codex-agent-provision.sh`.
They use temporary rules, fake CLI programs and release metadata; they do not start VMs,
contact model APIs or publish Git history. They cover the matcher invocation and failure
handling, latest-release installation and preservation of a working binary after a bad download.
Mocks do not establish compatibility with a real CLI release. Check its native `execpolicy check`
against the shipped rules for that evidence; real client approve/deny needs separate acceptance.

## Select additional checks

Subyard's change-impact selector recommends a conservative set of checks for a repository diff. It
is advisory: it prints recommendations and never executes a check. A caller may add checks, but a
selector result does not waive required host-free, release, operator-requested, or runtime-derived
testing.

## Select checks

Run exactly one of these forms from a non-bare repository checkout:

```sh
dev/test-impact.sh --current-base REF [--format human|json]
dev/test-impact.sh --base REF --head REF [--format human|json]
dev/test-impact.sh --changes-from FILE|- [--format human|json]
```

`--current-base REF` is the normal local workflow. It compares the checkout with the base tree and
includes tracked, staged, unstaged, and non-ignored untracked changes. It works in an ordinary Git
checkout or Git worktree, including a detached or dirty checkout. It cannot inspect a bare
repository because there is no working tree.

`--base REF --head REF` compares two commits. The canonical head commit must be the current `HEAD`,
and selector-owned paths must be clean: `internal/testimpact/**`, `cmd/test-impact/**`,
`dev/test-impact.sh`, and `tests/impact-map.json`. These guards ensure that the checked-out selector
and policy describe the analyzed head.

`--changes-from FILE|-` reads a strict, versioned JSON change set from a file, or from standard input
when the value is `-`. This form is intended for fixtures and callers that already have normalized
Git changes. For example:

```json
{
  "schema_version": 1,
  "changes": [
    {
      "status": "M",
      "similarity": null,
      "old_path": "internal/example/example.go",
      "new_path": "internal/example/example.go",
      "old_mode": "100644",
      "new_mode": "100644"
    }
  ]
}
```

Human-readable output is the default. Add `--format json` for one machine-readable JSON document;
`--format human` is accepted explicitly. Flag order does not matter.

## Interpret the result

Treat the result as fail-closed:

| Status | Exit | Meaning and required response |
| --- | ---: | --- |
| `selected` | 0 | Normal analysis. Run the recommended checks and apply the external gates below. An empty diff can produce an empty recommendation. |
| `fallback` | 0 | Analysis or bootstrap was unsafe or unavailable. Run the expanded `host-free:all` recommendation and a fresh `dev/e2e/p0-acceptance.sh --slot N --lane full`. Inspect `errors` for the sanitized cause. |
| `error` | 2 | Command-line misuse. No recommendations are available; correct the invocation and rerun it. |

Automation must inspect `status` and `full_p0.required`; exit 0 alone does not mean targeted testing
is sufficient. JSON results separate `host_free_checks` from `e2e_checks` and include stable check
IDs, tiers, budgets, rationales, selection reasons, and any static requirement for full P0. They do
not contain executable command lines and do not run them.

## Evidence tiers

| Tier | Evidence |
| --- | --- |
| T0 | An exact regression test for the defect or failure mode. The engineer or agent defines it; the selector cannot derive it from paths. Target: at most 60 seconds. |
| T1 | Affected host-free package, race, shell, CLI, frontend, or Rust checks. Typical target: at most 3 minutes; registry metadata identifies larger explicit budgets. |
| T2 | The core host-free gate, `./tests/run.sh`. It remains required by the merge workflow and is not narrowed by the selector. The `host-free:all` fallback composite also includes Veranda checks. |
| T3 | Existing targeted E2E lanes or real-host checks for affected physical boundaries. |
| T4 | A fresh full P0: `dev/e2e/p0-acceptance.sh --slot N --lane full` without `--resume`. |

Run the applicable T0 check while developing, then use the selector to identify the T1 and T3
lower bound. Run T2 when the merge workflow requires it. If `full_p0.required` is true, run T4.
A fresh full pass also satisfies its contained T3 lanes: smoke, boundary, transport, nested teardown,
real Incus, release, source upgrade, power/systemd (including reboot verification), peer and cleanup.
Do not rerun those same lanes solely because the selector also lists them. Other selected checks
remain required; for example, cold dependencies, profile-resource's bind fixture and Orca acceptance
are not covered by the full matrix.

Targeted evidence shows that the selected contracts and physical boundaries passed for the analyzed
change. It does not replace the fresh release smoke required before publication:
`dev/e2e/p0-acceptance.sh --slot N` without `--resume`. This VM gate is run externally and manually;
GitHub workflows do not receive VM access or enforce it automatically. The exhaustive `--lane full`
matrix is periodic manual evidence and is also required when `full_p0.required` is true, an operator
requests it, or targeted runtime evidence exposes broader coupling.
