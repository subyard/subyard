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
   For ordinary checks, inherit shared Go caches and developer Cargo/Rustup homes;
   use the [development cache workflow](../../../docs/development.md#toolchain) for
   shared Cargo intermediates and separate final outputs. Do not copy build caches
   into each evidence directory. Preserve deliberate cold-cache and UID-isolation fixtures.
   Choose tests by the policy below and existing coverage.
   Simplify unnecessary implementation scope before expanding its test machinery.
3. Run the applicable checks against the final changes using the policy below.
   Planned checks on available allocated VMs are agent work: complete them before
   reporting readiness.
4. Use `rg` and targeted excerpts. Set a total output budget before batching independent
   reads/searches or short checks; use `Promise.allSettled` where available and inspect every
   result/exit. Broaden reads for new evidence or insufficient excerpts, rather than rereading
   truncated batches. Keep dependencies, edits, approvals and decisions sequential.
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
- Before an expensive affected VM check, run changed-component unit/contract checks, then the
  smallest fixture preflight, then the affected E2E lane. Reuse a current applicable PASS;
  a failed prerequisite stops dependent expensive checks. If no local check exists or it cannot
  prove the physical contract, record that limit in one line and run the smallest owning VM
  diagnostic. Use the VM guide's [fixture preflight](../../../docs/test-vms.md#fixture-preflight)
  for commands, execution user, readiness and capacity; this does not require the full host-free suite.
- For documentation, formatting and mechanical changes with unchanged behavior, inspect the diff
  and use relevant syntax, link or format validation. Do not automatically build, run the core
  suite or acquire VMs. Small behavioral changes normally need focused package or contract tests.
- Preserve checks for permissions, data loss, migrations, atomic updates and recovery. Choose
  broader checks when shared behavior or actual coupling makes focused coverage insufficient.
  Run the full host-free suite (`./tests/run.sh`) for that reason, an explicit user request, or
  an applicable merge/release gate, not merely because a CLI or shell file changed.
- Use targeted VM/real-host checks when correctness depends on physical behavior that local tests
  cannot establish. Mocks do not prove external CLI/protocol or host compatibility. Run full P0
  only for an explicit request or concrete cross-domain lifecycle/recovery coupling that targeted
  lanes cannot adequately cover. Explain that reason before the expensive run; availability of
  VMs alone does not justify using them.
- Keys sync unit/sandbox, installer/refresh and shared runtime/credential admission changes require
  `config/profiles/github/tests/e2e/acceptance.sh --installed-worker --slot N`, including changes
  to called validators. This existing owning mode checks a real user manager and installed worker
  with its SSH/SOPS peer; a host-free systemctl stub cannot close that contract. Select other units'
  existing owning systemd lanes separately. Follow the explicit dependency map in
  [coverage ownership](../../../docs/testing.md#coverage-ownership); there is no automatic
  changed-file selector. Run the mode's local prerequisites before its allocation.
- Before publishing a runtime release, run fresh release smoke
  (`dev/e2e/p0-acceptance.sh --slot N`), the full release compatibility check in
  [Development](../../../docs/development.md), and all shipped-profile acceptance on the same candidate.
  Complete local smoke may precede CI completion and transfer after review of equivalent inputs.
  Required local/VM gates remain mandatory; optional live checks may stay `not-run` under the
  [release contract](../../../docs/reference/release-publication.md#external-checks).
  Failed or invalid external results still block publication. These gates do not apply to every edit;
  existing CI/merge checks are unchanged. Full P0 remains separate from release smoke and risk-selected
  or explicitly requested.
- Once relevant checks pass, broaden or repeat them only for a new change, failure or unresolved
  risk. Report the actual evidence and its limits; do not turn a focused pass into a claim that
  the full lifecycle or release gate passed.

## Test progress across leases

- For acceptance, check prerequisites and capacity first; ARM runs in GitHub CI only.
  Repair with targeted checks, then freeze the final candidate. Run the original regression
  before the remaining required gates; a blocked check is incomplete, never passed.
- For release repairs, accumulate fixes and commit them together after verification. Run pending
  local/VM gates, including complete release smoke, on a frozen local build alongside preparation CI.
  After CI, compare binaries and consumed runtime inputs/modes, then transfer equivalent complete
  original PASS evidence through the reviewed workflow. Rerun only affected or missing gates.
- Keep passed, failed and pending segments in the current task plan with the tested source hash,
  environment/base fingerprint and controller evidence paths. Reassess affected results when those
  identities change; use the testing guide's [evidence handoff](../../../docs/testing.md#evidence-handoff-and-cache-cleanup)
  before removal of completed task-owned cache. Keep logs and receipts while the task is active;
  after verification and cleanup, retain only key results and limitations in the private reference.
- Every lease gets disposable VMs. Never infer progress or reuse fixtures from a released VM.
  Run remaining independent `--lane` segments with their own fresh setup. P0 rejects cross-lease
  `--resume`; reboot continuation is valid only within the same active lease.
- Required full P0 gates remain one fresh run; targeted passes cannot substitute for them.

## Test execution and delegation

Keep short mechanical/doc edits and read-only answers direct; add review for a concrete risk.
For substantial changes in an explicitly selected, bounded role pilot, use one sole writer and
one independent reviewer. The governing task owns eligible blocks, deadline and quality/rollback
conditions; this does not claim universal savings or promote the pilot to a global default. Add a
second reviewer only for an explicit two-axis request or a concrete independent risk. Preserve
the risk-based checks above; orchestration does not add acceptance runs.

Role-specific obligations live in the [developer](agents/subyard_developer.toml),
[reviewer](agents/subyard_reviewer.toml) and [observer](agents/subyard_observer.toml) contracts.
Read the selected contract when assigning a role. These files are not automatically loaded as
native custom agents; use their instructions in a self-contained assignment with supported
model/effort/fork fields. Use `model="gpt-6.1-sol"`, `reasoning_effort="high"`,
`fork_turns="none"` for developers and fresh reviewers; record a comparable available alternative
if needed. A necessary long check may use one `gpt-6-luna`/`medium` observer with
`fork_turns="none"`; run it directly if unavailable. Short checks stay with the developer.

The coordinator owns original requirements, editable scope, one shared task and final acceptance:

1. Assign a complete block to one developer. Do only the discovery needed to assign work or
   resolve a finding; delegate implementation and use its evidence pointers.
2. After the writer stops and relevant checks reach native terminal outcomes, assign review of
   that exact dirty/untracked candidate. An observer handoff alone does not meet this condition.
   Preliminary review with incomplete checks cannot establish readiness.
3. Resolve unsupported findings with a reason; return confirmed repairs via `followup_task` to
   the same developer while its context is relevant. Recheck affected claims after writing stops.
4. Accept when original requirements, findings and required evidence match the final candidate.
   Read native receipts and concrete risky areas as needed, using the reviewer's independent
   binding check without repeating the whole review or generating another report. A substantial
   change stays review-pending if an independent reviewer is unavailable; self-review cannot
   replace it. Only the coordinator closes the shared task and responds to the operator.

Keep assignments compact: role and canonical coordinator/developer targets, working directory,
requirements, editable scope/exclusions, known facts and source links, acceptance,
candidate/evidence pointers, checkpoint/stop conditions and expected result. Long-check
assignments also name the exact command and one progress recipient. Reuse existing candidate
bindings and native receipts; one concise result per role is enough. Copy relevant pointers
rather than the chat or whole guides. Workers report remaining work to the coordinator and
follow only their assigned phase.

Before handoff/compaction, the coordinator preserves remaining work, decisions, active
writer/runner ownership, candidate and evidence pointers in the shared task. Workers send
concise state instead of editing it. Before takeover, confirm the former writer stopped and
establish runner/cleanup status; agent termination does not drain a lease. At checkpoint expiry
or a concrete stall, obtain one targeted ownership/blocker result, report partial work and any
blocker, then choose owned interruption/takeover or a reasoned continuation with a new finite
bound. Loss of context and missed deadlines never imply success or a silent extension.

Execution, polling/cursors, native outcomes, repair and evidence handling belong to the testing
guide's [execution protocol](../../../docs/testing.md#execution-and-reporting-principles).
Keep compaction defaults, global settings and existing runner infrastructure unchanged.

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

**Done means the agreed change is verified, required checks and review have passed,
and the final code is ready to commit and hand to the operator.** Close or delete
the development plan according to the governing task workflow.

When ending an implementation session with a development plan still present, link it in
the final response, explain why it is unfinished, and list the concrete remaining work
and any blocking input or access. Updating the plan alone does not satisfy this requirement.

Production updates and post-deployment checks belong to the operator after handoff;
exclude them from development plans and automatic follow-up or `check-*` tasks.
Release publication retains its separate gates.

If implementation or required validation steps remain, report **partial** and continue
executable work. If none can proceed without missing input or access, report **blocked**
and name what is needed. Report pending required checks and optional `not-run` checks accurately.
Moving an in-scope step to another file or ending a session does not complete it.

Report what changed, what was actually checked, and the operator handoff. A component or
host-free pass proves only that scope; it does not prove the planned yard lifecycle
or full release acceptance. End file-changing work with a short imperative
suggested commit message.
