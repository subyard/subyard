---
name: subyard-dev-flow
description: Use when developing Subyard in this repository, including features, bug fixes, refactoring, tests, and developer documentation.
---

# Subyard dev-flow

Follow the linked sources; keep detailed rules in their authoritative files.

## Read the relevant sources

Read by task; links are relative to this skill.

| When | Source |
| --- | --- |
| Every development task | [AGENTS.md](../../../AGENTS.md): repository rules. Also read [private/AGENTS.md](../../../private/AGENTS.md) if present: task plans, requirements, decisions, and open questions. |
| Product context or component docs | [README.md](../../../README.md): product overview and documentation index. |
| Code, build, or packaging work | [Development](../../../docs/development.md): toolchain and build/release workflow; use `go.mod` for versions. |
| Commands, RPC, reconciliation, or adapters | [Control plane](../../../docs/control-plane.md): implementation map, ownership boundaries, and extension contracts. |
| Before building, running, or changing tests | [Testing](../../../docs/testing.md) and [Test VMs](../../../docs/test-vms.md): required checks, evidence tiers, and VM access. |
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
   Choose new tests by [failure risk and existing coverage](../../../docs/testing.md#keep-tests-proportional).
   Simplify unnecessary implementation scope before expanding its test machinery.
3. Run the applicable checks against the final changes. The test selector is
   advisory; follow the testing guides for required core and release gates.
   Planned checks on available allocated VMs are agent work: complete them before
   reporting readiness.

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
