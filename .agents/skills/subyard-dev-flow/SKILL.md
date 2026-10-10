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
| Every development task | [AGENTS.md](../../../AGENTS.md): repository rules. Also read [private/AGENTS.md](../../../private/AGENTS.md) if present: local workflow, task plans, and operator-specific requirements. |
| Product context or component docs | [README.md](../../../README.md): product overview and documentation index. |
| Code, build, or packaging work | [Development](../../../docs/development.md): toolchain and build/release workflow; use `go.mod` for versions. |
| Commands, RPC, reconciliation, or adapters | [Control plane](../../../docs/control-plane.md): implementation map, ownership boundaries, and extension contracts. |
| Design rationale or difficult recovery bugs | [Decisions and glossary](../../../docs/reference/decisions-and-glossary.md), [Architecture](../../../docs/reference/control-plane-architecture.md), and [Release transitions](../../../docs/reference/resumable-release-transition-design.md): current decisions, architectural rationale, and recovery invariants; use the control-plane guide for the component implementation map. |
| Before building, running, or changing tests | [Testing](../../../docs/testing.md) and [Test VMs](../../../docs/test-vms.md): commands, evidence tiers, and VM access. |
| Live host or release behavior | [Real-host acceptance](../../../docs/real-host-acceptance.md): physical-boundary and release checks. |
| Configuration or credentials | [Configuration](../../../docs/configuration.md) and [Keys](../../../docs/keys.md), respectively. |

An absent private overlay does not block public-only work. Ask for missing
requirements when the task depends on them.

## Work

The coordinator owns this section's task planning and acceptance. Assigned workers follow
the engineering and testing rules within their assignment, report remaining work to the
coordinator, and do not create or close shared task files or start another full role cycle.

1. Establish the requested outcome, scope, plan steps, and acceptance checks before
   editing. Unless other instructions specify a location, keep task plans in
   `private/tasks/`, creating it as needed; it is gitignored and needs no separate
   private repository. Follow the overlay's task-file workflow when present. Keep
   remaining steps current; change the agreed scope only with the user's agreement.
2. Preserve unrelated edits and follow the affected component's architecture.
   Make host fixes reproducible through product setup, including repeat runs.
   Choose tests by the policy below and existing coverage.
   Simplify unnecessary implementation scope before expanding its test machinery.
3. Run the applicable checks against the final changes using the policy below.
   Planned checks on available allocated VMs are agent work: complete them before
   reporting readiness.
4. Batch independent reads/searches and short checks without shared mutable resources;
   use `Promise.allSettled` where available and inspect every result/exit code.
   Keep dependencies, edits, approvals and decisions sequential.
5. Before handoff or compaction, update the concise task plan: remaining work, decisions,
   evidence and candidate identities. Preserve required rules and relevant architecture,
   without a chat journal or full history; leave compaction defaults unchanged.

## Choose checks by risk

- Normal checks and acceptance test the current public worktree, including uncommitted and
  untracked public inputs. Installed-runtime and reboot checks package that worktree and install
  the resulting candidate through the supported installer. Use a published release only as an
  explicit problem-reproduction baseline; a pass on it does not establish current-worktree acceptance.
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
  [Development](../../../docs/development.md), plus the declared acceptance checks of all shipped
  profiles against the same candidate. A core-only pass is not release acceptance; missing required
  profile results leave it incomplete. This publication gate does not apply to every
  development edit. Existing CI/merge checks remain unchanged; full P0 is a separate risk-based
  or explicitly requested check, not a synonym for release smoke.
- Once relevant checks pass, broaden or repeat them only for a new change, failure or unresolved
  risk. Report the actual evidence and its limits; do not turn a focused pass into a claim that
  the full lifecycle or release gate passed.

## Test progress across leases

- For acceptance, check prerequisites and capacity first; ARM runs in GitHub CI only.
  Repair with targeted checks, then freeze the final candidate. Run the original regression
  before the remaining required gates; a blocked check is incomplete, never passed.
- Keep passed, failed and pending segments in the current task plan with the tested source hash,
  environment/base fingerprint and controller evidence paths. Reassess affected results when those
  identities change; copy evidence needed beyond the runner's retention window.
- Every lease gets disposable VMs. Never infer progress or reuse fixtures from a released VM.
  Run remaining independent `--lane` segments with their own fresh setup. P0 rejects cross-lease
  `--resume`; reboot continuation is valid only within the same active lease.
- Required full P0 gates remain one fresh run; targeted passes cannot substitute for them.

## Test execution and delegation

- Use `model="gpt-6.1-sol"`, `reasoning_effort="high"` for main development tasks,
  developer and independent reviewer.
- Run short checks directly. For long runs, use one mechanical observer with
  `model="gpt-6-luna"`, `reasoning_effort="medium"`, `fork_turns="none"`;
  run locally if unavailable.
- Give each bounded phase a self-contained assignment: working directory, exact commands,
  source state, known facts, relevant references, stop conditions and evidence paths.
  The observer reads mandatory repository/overlay and applicable testing/VM rules,
  without copied chat history, whole documents or repeated project discovery.
- The observer runs prescribed checks only; the main developer owns failure analysis,
  repairs and rerun decisions.
- Follow the guide's [execution and reporting protocol](../../../docs/testing.md#execution-and-reporting-principles),
  including waits, cleanup and evidence. Promptly report guide deviations to the operator
  under its [deviation contract](../../../docs/testing.md#report-deviations-from-the-guide).

## Coordinator role pilot

The coordinator → developer → independent reviewer workflow is an explicitly selected,
bounded pilot, not yet the default for all development. During the pilot, delegate substantial
implementation, behavioral fixes and changes spanning related components as complete,
reviewable blocks. Keep typos, formatting, simple mechanical edits and short read-only answers
direct; add review only for a concrete risk. Preserve every existing acceptance requirement
and the risk-based check policy above. Do not add test runs just to exercise the workflow.

Role contracts belong to this skill: [developer](agents/subyard_developer.toml),
[reviewer](agents/subyard_reviewer.toml) and [observer](agents/subyard_observer.toml).
Read the selected contract when assigning that role. For tool-based orchestration, use its
instructions in a self-contained assignment and only supported model/effort/fork fields.
These files are not automatically discovered as native custom agents from the skill directory;
CLI/IDE integration requires a separate supported project configuration, not a global change.

The coordinator owns original requirements, editable scope, one shared task and final acceptance.
Do only the discovery needed to assign work or resolve a finding; do not duplicate implementation,
observer polling or the whole review. Use this cycle:

1. Assign one sole writer a complete block. Start developer and fresh reviewer with
   `model="gpt-6.1-sol"`, `reasoning_effort="high"`, `fork_turns="none"`. Record a comparable
   available alternative if needed; do not downgrade independent review to an observer.
2. Wait for the developer to stop writing and finish relevant checks, then assign independent
   review of the exact dirty/untracked candidate. Preliminary review with incomplete checks must
   state that limit and cannot establish readiness. Read-only is a contract unless effective
   isolation is verified; live parent overrides can defeat native sandbox defaults.
3. Resolve unsupported findings with a reason; return confirmed repairs via `followup_task` to
   the same developer while its context is relevant. Recheck affected claims after writing stops,
   broadening only when the delta warrants it. Unresolved correctness/coverage blockers remain.
4. Accept only when original requirements, findings and required evidence match the final
   candidate. Read native receipts and concrete risky areas as needed. No findings alone is
   not acceptance; only the coordinator closes the task and responds to the operator.

Keep assignments short: role, canonical coordinator/developer targets, working directory,
original requirements, editable scope/exclusions, known facts and source links, acceptance,
candidate/evidence paths, a bounded checkpoint and stop conditions, and the expected result.
Do not copy the full chat or guides. Only a necessary long check adds one observer, assigned by
the developer with both canonical targets, an exact runner command and existing deadlines.

Before handoff/compaction preserve remaining work, decisions, active writer/runner ownership,
candidate identities and evidence in the shared task; workers send updates instead of editing it.
Loss of context or an unavailable agent is not success. Before takeover confirm the former
writer stopped and establish runner/cleanup status; stopping an agent does not drain its lease.
Use supported event waits within runtime limits, without duplicate raw polling or periodic
status requests. A missed checkpoint or concrete stall permits one targeted ownership/blocker
check, then a reasoned continuation with a new bounded deadline, owned interruption or takeover.
Do not restart blindly or extend indefinitely. A substantial block still needs an independent
reviewer; if none is available, leave review pending and finish other executable work. Self-review
does not satisfy that requirement. Workers do not recursively orchestrate this cycle.

Set the pilot's eligible blocks, baseline, model settings, deadline and promotion/rollback
criteria in the governing task before using it. Keep it bounded to three suitable ordinary
blocks or fourteen calendar days, whichever comes first; retain the direct path for a needed
small edit. At the boundary, keep, narrow or roll back only for classes supported by evidence.
Missing comparable baseline or usage leaves savings unknown. Missed requirements/checks,
false readiness, lost changes/evidence or unsafe authority expansion stop the affected class
until repaired; do not trade quality for lower usage. Unexplained repeated growth above 20%
in two comparable blocks calls for narrowing or rollback, not a causal claim from mixed data.
Unobserved failure, compaction or long-run cases remain unverified. Leave compaction defaults,
global settings and existing runner infrastructure unchanged.

## Boundaries

- Keep optional-profile implementation and tests in the owning profile. Core provides generic
  extension contracts and synthetic fixtures, without profile-specific branches or assertions.
- Keep public files generic and in English. Keep private material in the overlay;
  never print or store secrets in task notes or reports.
- Use allocated test targets within the VM guide's ownership boundary. The outer
  yard's lifecycle belongs to the operator. Ask the user to run exact commands
  when operator access or `sudo` is required.
- Commit, push, or rewrite Git history only on explicit user instruction.

## Completion

**Done means every agreed requirement and plan step is satisfied, and every
required check has passed for the final changes.** Only then may the coordinator close or
delete the task plan according to the governing task workflow.

If steps remain, report **partial** and continue executable work. If no remaining
step can proceed without missing input or access, report **blocked** and name what
is needed. Keep unfinished steps in the plan; operator-only checks remain pending
until their results arrive. Moving a step to another file or ending a session
does not complete it.

Report what changed, what was actually checked, and what remains. A component or
host-free pass proves only that scope; it does not prove the planned yard lifecycle
or full release acceptance. End file-changing work with a short imperative
suggested commit message.
