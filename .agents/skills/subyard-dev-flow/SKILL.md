---
name: subyard-dev-flow
description: "Always use with dev-flow when developing Subyard itself: its parameters, checks and tools."
---

# Subyard dev-flow

Subyard parameters for the terms of the [dev-flow](https://github.com/Dmitry-Borodin/dev-flow) skill.

## Read the relevant sources

Read by task; links are relative to this skill.

| When | Source |
| --- | --- |
| Every development task | [AGENTS.md](../../../AGENTS.md): repository rules. Also read [private/AGENTS.md](../../../private/AGENTS.md) if present: local workflow, task plans, and operator-specific requirements. |
| Product context or component docs | [README.md](../../../README.md): product overview and documentation index. |
| Code, build, or packaging work | [Development](../../../docs/development.md): toolchain and build/release workflow; use `go.mod` for versions. |
| Commands, RPC, reconciliation, or adapters | [Control plane](../../../docs/control-plane.md): implementation map, ownership boundaries, and extension contracts. |
| Design rationale or difficult recovery bugs | [Decisions and glossary](../../../docs/reference/decisions-and-glossary.md), [Architecture](../../../docs/reference/control-plane-architecture.md), and [Release transitions](../../../docs/reference/resumable-release-transition-design.md): current decisions, architectural rationale, and recovery invariants. |
| Testing guidance by task type | [Testing reading map](../../../docs/testing.md#reading-map-by-task-type): choose the relevant sections for the task. |
| Live host or release behavior | [Real-host acceptance](../../../docs/real-host-acceptance.md): physical-boundary and release checks. |
| Configuration or credentials | [Configuration](../../../docs/configuration.md) and [Keys](../../../docs/keys.md), respectively. |

## Project parameters

- **Tasks and instructions:** the optional private overlay `private/`, a separate gitignored
  repository; rules in `private/AGENTS.md`. Keep the main checkout's `private/` current and never
  commit it; the operator syncs it with [subyard-private-sync](../subyard-private-sync/SKILL.md).
  Without an overlay or other instructions, ask the operator.
- **Commits:** the coordinator commits and pushes the accepted candidate without asking.
- **Checks:** [Choose checks by risk](#choose-checks-by-risk). **PR format:** [Pull requests](#pull-requests).
- **Code areas:** `product`, `tests`, `fixtures and tools`, `documentation`.

## Engineering

- Make host fixes reproducible through product setup, including repeat runs.
- Name the shared primitive for each invariant (writes, private directories, refusal collection,
  resume); a hand-written substitute is a first-round review finding. The interruption harness
  and the refusal table test run before review.
- For ordinary checks, inherit shared Go caches and developer Cargo/Rustup homes; use the
  [development cache workflow](../../../docs/development.md#toolchain) for shared Cargo
  intermediates and separate final outputs. Do not copy build caches into evidence directories.
  Preserve deliberate cold-cache and UID-isolation fixtures.
- Changes to profile runtime formats, migration registries, or agent integration sources must
  pass `TestPublishedRollbackGate` against the latest published baseline. Provide the existing
  rollback hooks/down path, or seal a rollback boundary in the introducing release's
  `config/release-rollback.json`. Runtime owners absent from either release are skipped. Agent
  inventory changes (tracked package/materialization inputs and project dispatcher) require a
  non-blocking declaration of surviving yard files and manual cleanup, shown in plan/result
  output and release notes; accepted agent cleanup limitations never refuse rollback.
  See [rollback boundaries](../../../docs/control-plane.md#declared-rollback-boundaries).

## Choose checks by risk

| dev-flow kind | Subyard | Guide |
| --- | --- | --- |
| Local | focused `go test`, shell/Python contracts, profile `tests/run.sh`, `./tests/run.sh`, `make verify`, `make cli-docs-check` | [core checks](../../../docs/testing.md#run-the-core-checks) |
| VM pipelines | `dev/agent-e2e.sh` leases: P0 `vm:p0-full` or `--lane`, profile `tests/e2e/acceptance.sh --slot N`, fixture preflight | [VM guide](../../../docs/test-vms.md) |
| Base CI | `verify` and CodeQL on every push | [required CI checks](../../../docs/testing.md#required-ci-checks) |
| Extended CI | Periodic E2E dispatch on the PR branch, including ARM and Intel retry runs | [Periodic E2E](../../../docs/testing.md#periodic-e2e-on-hosted-runners) |
| Stand | leased disposable VM (root), Android emulator, web preview | VM guide, `subyard-android`, `subyard-preview` |

- Checks and acceptance test the current public worktree, including uncommitted and untracked
  inputs. Installed-runtime and reboot checks package that worktree and install it through the
  supported installer. A published release is only a problem-reproduction baseline.
- Source-text assertions belong only to explicit source-level contracts.
- Before an expensive VM check, run changed-component unit/contract checks, then the smallest
  [fixture preflight](../../../docs/test-vms.md#fixture-preflight), then the affected lane. Reuse a
  current applicable PASS. If no local check can prove the physical contract, record that limit
  in one line and run the smallest owning VM diagnostic.
- Run `./tests/run.sh` for shared behavior or actual coupling, an explicit request, or a
  merge/release gate, not merely because a CLI or shell file changed.
- Run full P0 only on explicit request or for cross-domain lifecycle/recovery coupling that
  targeted lanes cannot cover; state the reason first. Free VMs alone do not justify it.
- Select owning lanes, including profile-owned lanes and transitive validators, from
  [coverage ownership](../../../docs/testing.md#coverage-ownership). Mocks cannot close a
  contract the map assigns to a physical lane.
- Also run the existing tests and lanes the change can break outside its own area: shared code,
  callers, affected profiles. A whole Periodic run is not required. Merge only when they show
  nothing existing breaks.
- A test that fails on the default branch or its scheduled runs, even once on a loaded runner, is
  a defect, not a tolerated flake. Plan its fix at once and queue it first; tell the operator
  which merged change let it in and which check missed it. Make timing-dependent tests wait for
  completed work, not deadlines.
- Planned checks on available allocated VMs are agent work: complete them before readiness.
- Release preparation, acceptance and publication follow the
  [agent release workflow](../../../docs/reference/release-publication.md#agent-release-workflow).

## Test progress across leases

- For acceptance, check prerequisites and capacity first; ARM runs in GitHub CI only.
  Repair with targeted checks, then freeze the final candidate. Run the original regression
  before the remaining required gates; a blocked check is incomplete, never passed.
- Release acceptance has priority on local slots and CI: while a release `accept` or Release VM
  checks run is active, start no new non-release physical lanes or long hosted dispatches such as
  Periodic E2E, and stop non-release lanes on slots it needs at their next safe point.
- Keep passed, failed and pending segments in the current task plan with the tested source hash,
  environment/base fingerprint and controller evidence paths. Reassess affected results when those
  identities change; use the testing guide's [evidence handoff](../../../docs/testing.md#evidence-handoff-and-cache-cleanup)
  before removal of completed task-owned cache. Keep logs and receipts while the task is active;
  after verification and cleanup, retain only key results and limitations in the private reference.
- Every lease gets disposable VMs. Never infer progress or reuse fixtures from a released VM.
  Run remaining independent `--lane` segments with their own fresh setup. P0 rejects cross-lease
  `--resume`; reboot continuation is valid only within the same active lease.
- A required full P0 gate is one fresh `vm:p0-full` run plus its split checks on the same candidate;
  targeted passes cannot substitute for them.

## Pull requests

- By default a change ships as one PR: the first commit adds the mechanism without changing
  behavior, the second changes behavior, and the first review round goes commit by commit.
  Moving existing code onto shared primitives goes in separate PRs.
- Run GitHub commands as `subyard-github run -- gh …`. The body is the describer's output plus
  `python3 dev/pr-stats.py --base origin/BASE --review-cycles N --purpose AREA=TEXT ... [--check NAME=URL ...]`;
  `--review-cycles` counts dev-flow rounds.
- Create with `gh pr create --base BASE --title TITLE --body-file FILE`, adding `--label bug` for a
  defect fix and `--draft` while owed checks must still pass; then `gh pr ready NUMBER`. Update the
  body with `gh pr edit NUMBER --body-file FILE`.
- Owed before merge, all green on the current head:
  - base CI (`verify`, CodeQL);
  - a [Periodic E2E dispatch](../../../docs/testing.md#periodic-e2e-on-hosted-runners) on the PR
    branch of the standard VM checks plus every profile and lane check that
    [coverage ownership](../../../docs/testing.md#coverage-ownership) assigns to the change, Intel
    retry runs included, with run URLs passed as `--check`;
  - planned leased-VM checks: owned lanes outside the Periodic matrix
    (`python3 dev/ci/plan-periodic-checks.py` lists it), run after any release acceptance holding
    the local VMs ends. Release acceptance runs a lane on hosted runners only when
    `CI_HOSTED_CHECKS` in `dev/release-acceptance.py` names it; otherwise it is leased-VM there;
  - required review.
- A member's approving review of the current head shows in
  `gh pr view NUMBER --json headRefOid,latestReviews`. After consent, update the statistics, then
  run `gh pr merge NUMBER --auto --squash`; hold it with `gh pr merge NUMBER --disable-auto`.
- All agent PRs share one GitHub identity; leave other tasks' PRs to their owners.

## Agents

| Role | Claude Code | Codex | Cross-vendor |
| --- | --- | --- | --- |
| Developer, reviewer | [native project agents](../../../.claude/agents/) `developer`, `reviewer` | `spawn_agent` with `fork_turns="none"` | [subyard-agent-bridge](../../../config/skills/core/subyard-agent-bridge/SKILL.md) `--role developer\|reviewer` |
| Observer, describer, design consult | `general-purpose` agent with the role brief | `spawn_agent` with the role brief | `--role observer`, or the role brief as the task |

- A brief is the loaded dev-flow skill's `roles/<role>.md` plus the role additions below.
- Use the installed bridge skill.
  `--approve-for-me` forces a sandbox in which protected-runtime tests and the GitHub broker fail.
- Scratch files are per session and disposable. Never rely on another session's scratch paths,
  and never store shared material there. Remove your own scratch files when your task ends.
  Hand facts on in your report, where the coordinator puts them into the task file, and keep
  artifacts with the PR or task output.

### Role additions

- **Developer, observer:** follow the testing guide's
  [execution principles](../../../docs/testing.md#execution-and-reporting-principles),
  [long checks](../../../docs/testing.md#launch-and-observe-long-checks),
  [repair after a failure](../../../docs/testing.md#repair-after-a-failure) and
  [evidence handoff](../../../docs/testing.md#evidence-handoff-and-cache-cleanup). Keep global
  settings and existing runner infrastructure unchanged.
- **Reviewer:** assess execution evidence by the same execution principles.
- **Describer:** English Markdown; omit statistics.

## Environment

- Use allocated test targets within the VM guide's ownership boundary. Ask the operator to run
  exact commands that need operator access or `sudo`.
- Exclude production updates and post-deployment checks from development plans and follow-up tasks.
