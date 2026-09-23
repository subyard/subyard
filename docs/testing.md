# Testing changes

Test-selection policy lives in [Subyard dev-flow](../.agents/skills/subyard-dev-flow/SKILL.md#choose-checks-by-risk).
This guide documents how to run checks and interpret their evidence.

## Keep tests proportional

Use the skill's [risk-based selection policy](../.agents/skills/subyard-dev-flow/SKILL.md#choose-checks-by-risk)
for both adding tests and choosing which existing checks to run.

## Run the core checks

`./tests/run.sh` checks the current source files, including uncommitted edits. No Git history,
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

Subyard's change-impact selector prints a conservative set of recommendations for a repository
diff and never executes checks. Apply the skill's selection policy to these recommendations.

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

| Status | Exit | Meaning |
| --- | ---: | --- |
| `selected` | 0 | Normal analysis. An empty diff can produce an empty recommendation. |
| `fallback` | 0 | Analysis or bootstrap was unsafe or unavailable. The output contains expanded recommendations; `errors` explains the sanitized cause. |
| `error` | 2 | Command-line misuse. No recommendations are available; correct the invocation and rerun it. |

Exit 0 reports selector completion, not a passing test result. JSON results separate
`host_free_checks` from `e2e_checks` and include check IDs, tiers, budgets, rationales and selection
reasons. The existing `full_p0.required` field name represents the selector's conservative full-P0
recommendation; it is not an execution mandate. See the skill for how to assess it.

## Evidence tiers

| Tier | Evidence |
| --- | --- |
| T0 | An exact regression test for the defect or failure mode. The engineer or agent defines it; the selector cannot derive it from paths. Target: at most 60 seconds. |
| T1 | Affected host-free package, race, shell, CLI, frontend, or Rust checks. Typical target: at most 3 minutes; registry metadata identifies larger explicit budgets. |
| T2 | The full host-free suite, `./tests/run.sh`. The `host-free:all` fallback composite also includes Veranda checks. |
| T3 | Existing targeted E2E lanes or real-host checks for affected physical boundaries. |
| T4 | A fresh full P0: `dev/e2e/p0-acceptance.sh --slot N --lane full`. |

A fresh full pass also satisfies its contained T3 lanes: smoke, boundary, transport, nested teardown,
real Incus, release, source upgrade, power/systemd (including reboot verification), peer and cleanup.
Cold dependencies, profile-resource's bind fixture and Orca acceptance are not covered by that matrix.
See the skill for when local, VM and publication checks apply; the tiers describe evidence scope,
not a ladder that every change must climb.

## Continue work across leases

Follow the skill's [lease evidence rules](../.agents/skills/subyard-dev-flow/SKILL.md#test-progress-across-leases)
and the [VM guide](test-vms.md) for disposable targets and lane commands.
