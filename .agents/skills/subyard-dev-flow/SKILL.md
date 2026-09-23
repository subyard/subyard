---
name: subyard-dev-flow
description: Use when developing Subyard in this repository, including features, bug fixes, refactoring, tests, and developer documentation.
---

# Subyard dev-flow

This skill is the authoritative source for development workflow and test-selection policy.
The linked guides describe commands, evidence, tools and environment constraints; they do not
add automatic test gates. Explicit user instructions take precedence over this skill.

## Read the relevant sources

Read by task; links are relative to this skill.

| When | Source |
| --- | --- |
| Every development task | [AGENTS.md](../../../AGENTS.md): repository rules. Also read [private/AGENTS.md](../../../private/AGENTS.md) if present: task plans, requirements, decisions, and open questions. |
| Product context or component docs | [README.md](../../../README.md): product overview and documentation index. |
| Code, build, or packaging work | [Development](../../../docs/development.md): toolchain and build/release workflow; use `go.mod` for versions. |
| Commands, RPC, reconciliation, or adapters | [Control plane](../../../docs/control-plane.md): implementation map, ownership boundaries, and extension contracts. |
| Before building, running, or changing tests | [Testing](../../../docs/testing.md) and [Test VMs](../../../docs/test-vms.md): commands, evidence tiers, and VM access. |
| Live host or release behavior | [Real-host acceptance](../../../docs/real-host-acceptance.md): physical-boundary and release checks. |
| Configuration or credentials | [Configuration](../../../docs/configuration.md) and [Keys](../../../docs/keys.md), respectively. |

An absent private overlay does not block public-only work. Ask for missing
requirements when the task depends on them.

## Work

1. Establish the requested outcome, scope, plan steps, and acceptance checks before
   editing. Follow the overlay's task-file workflow when present. Keep remaining
   steps current; change the agreed scope only with the user's agreement.
2. Preserve unrelated edits and follow the affected component's architecture.
   Make host fixes reproducible through product setup, including repeat runs.
   Choose tests by the policy below and existing coverage.
   Simplify unnecessary implementation scope before expanding its test machinery.
3. Run the applicable checks against the final changes using the policy below.
   Planned checks on available allocated VMs are agent work: complete them before
   reporting readiness.

## Choose checks by risk

- Start with the smallest existing check that proves the changed behavior. Add a regression
  case only for a distinct failure or observable contract not already covered. Assert outcomes;
  source-text assertions belong only to explicit source-level contracts. If a small change needs
  extensive fixtures, reconsider its implementation before adding test machinery.
- For documentation, formatting and mechanical changes with unchanged behavior, inspect the diff
  and use relevant syntax, link or format validation. Do not automatically build, run the core
  suite or acquire VMs. Small behavioral changes normally need focused package or contract tests.
- Preserve checks for permissions, data loss, migrations, atomic updates and recovery. Choose
  broader checks when shared behavior or actual coupling makes focused coverage insufficient.
  Run the full host-free suite (`./tests/run.sh`) for that reason, an explicit user request, or
  an applicable merge/release gate, not merely because a CLI or shell file changed.
- Use the change-impact selector when it helps identify affected checks. Its entire output is
  advisory, including `full_p0.required=true` and `fallback`. Inspect the reasons against the
  actual diff; neither a flag nor the number of touched files/risk domains is a sufficient reason
  for a full run. If selection fails, assess impact manually rather than treating it as no risk.
  Briefly record the chosen checks and the reason for accepting or narrowing broad recommendations.
- Use targeted VM/real-host checks when correctness depends on physical behavior that local tests
  cannot establish. Mocks do not prove external CLI/protocol or host compatibility. Run full P0
  only for an explicit request or concrete cross-domain lifecycle/recovery coupling that targeted
  lanes cannot adequately cover. Explain that reason before the expensive run; availability of
  VMs alone does not justify using them.
- Before publishing a runtime release, run the fresh release smoke
  (`dev/e2e/p0-acceptance.sh --slot N`) and release compatibility checks documented in
  [Development](../../../docs/development.md). This publication gate does not apply to every
  development edit. Existing CI/merge checks remain unchanged; full P0 is a separate risk-based
  or explicitly requested check, not a synonym for release smoke.
- Once relevant checks pass, broaden or repeat them only for a new change, failure or unresolved
  risk. Report the actual evidence and its limits; do not turn a focused pass into a claim that
  the full lifecycle or release gate passed.

## Test progress across leases

- Keep passed, failed and pending segments in the current task plan with the tested source hash,
  environment/base fingerprint and controller evidence paths. Reassess affected results when those
  identities change; copy evidence needed beyond the runner's retention window.
- Every lease gets disposable VMs. Never infer progress or reuse fixtures from a released VM.
  Run remaining independent `--lane` segments with their own fresh setup. P0 rejects cross-lease
  `--resume`; reboot continuation is valid only within the same active lease.
- Required full P0 gates remain one fresh run; targeted passes cannot substitute for them.

## Test execution and delegation

- Run short checks directly. For long runs, use one worker with
  `model="gpt-5.6-terra"`, `reasoning_effort="medium"`, `fork_turns="none"`;
  run locally if unavailable.
- Pass the working directory, exact commands, source state, known facts, relevant
  file/section references and result paths. Do not copy chat history or whole
  documents, or ask the worker to repeat project discovery.
- Only the worker monitors the run; the parent does not duplicate polls or log
  reads. Prefer completion events; otherwise poll about every 60 seconds within
  client limits. Leave lease heartbeats and cleanup to the runner.
- Return the exit code, duration, summary/log paths and a short failure excerpt.
  Read `summary.tsv` first. Do not edit source or rerun the full suite without
  the parent's instruction.

## Boundaries

- Keep public files generic and in English. Keep private material in the overlay;
  never print or store secrets in task notes or reports.
- Use allocated test targets within the VM guide's ownership boundary. The outer
  yard's lifecycle belongs to the operator. Ask the user to run exact commands
  when operator access or `sudo` is required.
- Commit, push, or rewrite Git history only on explicit user instruction.

## Completion

**Done means every agreed requirement and plan step is satisfied, and every
required check has passed for the final changes.** Only then close or delete the
task plan according to the governing task workflow.

If steps remain, report **partial** and continue executable work. If no remaining
step can proceed without missing input or access, report **blocked** and name what
is needed. Keep unfinished steps in the plan; operator-only checks remain pending
until their results arrive. Moving a step to another file or ending a session
does not complete it.

Report what changed, what was actually checked, and what remains. A component or
host-free pass proves only that scope; it does not prove the planned yard lifecycle
or full release acceptance. End file-changing work with a short imperative
suggested commit message.
