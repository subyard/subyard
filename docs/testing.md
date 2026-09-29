# Testing changes

Test-selection policy lives in [Subyard dev-flow](../.agents/skills/subyard-dev-flow/SKILL.md#choose-checks-by-risk).
This guide documents how to run checks and interpret their evidence.

## Keep tests proportional

Use the skill's [risk-based selection policy](../.agents/skills/subyard-dev-flow/SKILL.md#choose-checks-by-risk)
for both adding tests and choosing which existing checks to run.

Use `testkit.WriteFile` for exact file modes and `testkit.TempDir` for private
fixture roots. Keep raw calls for creation-semantics tests. The Go race gate runs uncached under
umasks `0002`, `0022` and `0077` in separate processes.

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

Profile-specific assertions live in `config/profiles/<name>/tests/`. Each shipped profile must own a
host-free `tests/run.sh` and, when it has live scenarios, `tests/e2e/acceptance.sh`. The shared
`dev/test-profiles.sh` runner enumerates every profile directory and checks all required entrypoints
before running any of them. A missing entrypoint is reported as incomplete and exits nonzero; a
profile can declare `tests/run.not-applicable` or `tests/e2e/acceptance.not-applicable` with a reason
only when that runner is mutually exclusive with the declaration. The reserved `default` profile is
the only built-in exception. An incomplete live runner keeps profile release acceptance incomplete.

Run all profile host-free checks with `bash dev/test-profiles.sh`, or invoke one profile's runner
directly. Core tests exercise generic APIs with synthetic fixtures; profile tests check their preset
and runtime through those APIs. Profile test directories are excluded from runtime release bundles.
For profile-owned live checks, use `bash dev/test-profiles.sh --e2e --slot N` or a profile's
`tests/e2e/acceptance.sh` directly, following the VM allocation guide below. The shared runner
forwards arguments; each profile owns its fixtures, assertions and lease cleanup. These profile
results are one part of release acceptance alongside core release smoke and compatibility checks;
no green subset implies the combined gate passed.

Moving checks into profiles changes ownership, not release coverage: release acceptance must
aggregate core and all shipped profiles' declared checks against the same candidate. Missing or
failed required profile checks leave acceptance incomplete; optional runtime selection does not
make a shipped profile optional to release verification.

### One release candidate

The same profile inventory is used by local acceptance and branch CI preflight.
Create a frozen public source snapshot and one installable runtime, then run its pending checks:

```sh
python3 dev/release-acceptance.py prepare --version 0.14.1 --output .build/release-acceptance
python3 dev/release-acceptance.py run --output .build/release-acceptance --slots 1 2 3
```

Select available slots using `dev/agent-e2e.sh --status`; each slot runs one controller at a time.
Source fingerprints use Git executable-bit semantics; snapshots normalize public file modes explicitly,
so the checkout umask cannot change the candidate. The source snapshot, runtime assets, transport
archive, logs and `receipt.json` remain in the output
directory. Every controller receives the same checksum-verified transport archive; profile fixtures
execute the packaged engine and runtime assets. The core smoke installs those assets through the
public updater, including reboot, rollback and roll-forward. Local verification, ShellCheck,
process coverage, real local adapters and updater compatibility precede physical checks.
Run the aggregator from the managed workspace checkout: it supplies that workspace for lease
attribution while executing controllers and guest payloads from the frozen candidate.

A repeated `run` executes only checks without a passing result. Use `--only profile:NAME` or
`--only p0-release-smoke` for a remaining independent check; `--rerun` also repeats passed checks.
A physical retry always allocates fresh VMs. It never resumes a fixture across leases. Source or
artifact changes reject reuse; prepare a new candidate in a new directory. Logs are retained per
attempt and include the controllers' source and environment evidence.

Profile-owned `tests/e2e/external-obligations.json` declares checks requiring an authorized external
application/account. The receipt distinguishes these from reproducible VM results and initially
marks them `not-run`; synthetic credentials do not prove external API acceptance. Missing external
evidence leaves the overall result incomplete even when `reproducible_result` is `passed`. Record
actual external check evidence and its SHA-256 in the corresponding receipt entry only after that
check passes on the same candidate. Do not put credentials or private payloads in receipts.

Acceptance receipts and logs stay in the local output directory and are not committed to the
repository. The Release workflow gates publication on the checks it runs itself; physical VM and
external-service acceptance are performed separately. Cross-compiled assets retain their separate
build checks.

GitHub CI runs the full core gate, profile host-free checks, warning-level ShellCheck and
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

## Choose the VM allocation

Android acceptance runs on one `android-test` VM (8 GiB RAM / 40 GiB disk). Ordinary full P0
uses the standard `subyard-pair` (two 4 GiB / 20 GiB guests) for its two-host scenarios. It does
not recursively run the separate physical broker pool diagnostic. Do not request two 16 GiB
guests for Android or ordinary full P0.

`--vm 1` only selects where a command runs; it does not turn a pair allocation into a singleton.
Use `--type android-test` when allocating one VM. Commands and exact coverage are documented in
[the VM guide](test-vms.md#agent-workflow).

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
