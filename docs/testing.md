# Testing changes

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
| `fallback` | 0 | Analysis or bootstrap was unsafe or unavailable. Run the expanded `host-free:all` recommendation and a fresh full P0. Inspect `errors` for the sanitized cause. |
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
| T4 | A fresh full P0: `dev/e2e/p0-acceptance.sh --slot N` with no lane and without `--resume`. |

Run the applicable T0 check while developing, then use the selector to identify the T1 and T3
lower bound. Run T2 when the merge workflow requires it. If `full_p0.required` is true, run T4 in
addition to every selected check.

Targeted evidence shows that the selected contracts and physical boundaries passed for the analyzed
change. It is not full release evidence. The continuous full P0 run remains the external
release gate, even when every targeted recommendation passes. A release candidate or tag, an
operator request or override, or runtime coupling discovered by a targeted E2E check can require a
fresh full P0 independently of the selector's static result.
