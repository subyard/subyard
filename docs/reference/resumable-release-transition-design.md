---
title: Resumable release transitions
status: accepted
date: 2026-08-23
accepted: 2026-08-23
---

# Resumable release transitions

This reference describes the current release-transition architecture, its rationale and safety
constraints. The accepted design dates from 2026-08-23. The implementation map and operational
recovery instructions are maintained in [Release migrations](../control-plane.md#release-migrations).
Related references are [Decisions and glossary](decisions-and-glossary.md) and
[Control-plane architecture](control-plane-architecture.md).

## Context

A release update combines three distinct responsibilities: one-time transformation of persisted
state, activation of an immutable runtime, and repeatable convergence of release-owned runtime
resources. They share one transition owner, while retaining separate completion semantics.

[`internal/releasetransition`](../../internal/releasetransition) owns the assessed transition and
recovery. The [runtime installer](../../scripts/install-runtime-release.sh) verifies, unpacks and
publishes an immutable candidate. It can create the first `current` link during a clean bootstrap;
update activation and rollback belong to the transition owner.

## Goal

An update converges to a verified release/state fixed point through a resumable, authorized plan.
After interruption, durable intent and fresh observations determine the safe next action.

- A completed one-time migration never reopens because of later drift.
- Release activation and recovery have one owner.
- A planned reset is a successful migration outcome.
- Public outcomes report active/target/next action independently of internal checkpoints.
- Read-only inspection remains available during an unfinished transition.
- Manual journal editing is not a supported recovery path.

## Constraints and limitations

- No ACID transaction across the filesystem, Incus and systemd is promised. The contract is durable
  convergence with verifiable recovery; these systems do not commit simultaneously.
- The registry cannot supply arbitrary scripts, shell, argv, paths, templates or payloads.
- One-time migration does not fetch/sync Git, ask for a URL, or depend on a TTY or network input.
  Physical activation adapters can inspect dependencies and perform separately authorized privileged
  work; that does not make migration a general command runner.
- Generic migration does not reset secrets, inherited/profile-owned values or unknown data.
- Ordinary application readers do not retain support for every historical schema. Compatibility
  readers are bounded by supported upgrade and unfinished-transaction recovery horizons.
- Rollback requires an intact retained release and proven state compatibility. Some declared resets
  can remove rollback eligibility; their recovery class must be visible before authorization.
- Recovery evidence is protected and bounded by the retained runtime horizon. Cleanup is separate
  from transition correctness.

## Canonical terms

- **Release transition** — an authorized transition from the observed runtime/state pair to the target
  release and its verified fixed point.
- **One-time migration** — an append-only transformation of persisted state from one domain epoch
  to the next. Durable completion is permanent history.
- **Activation reconciler** — repeatable convergence of a derived release-owned resource to the exact
  target release, without changing the migration ledger.
- **Decision** — the planner's typed decision for one resource: `preserve`, `transform`,
  `canonicalize`, `reset`, `quarantine` or `block`.
- **Evidence** — protected durable expected-before/desired-after data and, where required, a recovery
  copy. It is not a handler's free-form status string.
- **Public outcome** — `ready`, `migration-required`, `recovering` or `operator-action-required`,
  with a stable code and safe next action.
- **Fact-based recovery** — choosing the next step from journaled intent and freshly observed actual
  state, rather than only from a recorded phase.

## Transition ownership

The `ReleaseTransition` Module owns planning, authorization binding, the journal, runtime activation,
recovery, post-activation reconciliation and the terminal outcome. Its two operations are:

```go
type ReleaseTransition interface {
    Inspect(context.Context, Goal) (Inspection, error)
    Converge(context.Context, Execution) (Outcome, error)
}
```

This excerpt shows the architectural seam. The complete current Go types, JSON tags, optional
inspection outcome and diagnostic warnings are defined in
[`contract.go`](../../internal/releasetransition/contract.go); this reference is not a generated API
specification. Registry examples below illustrate schema shape, rather than the complete shipped
migration list or an operator-supplied policy.

`Inspect` is strictly read-only: it does not create a journal, request sudo, acquire a lease,
transfer secrets or mutate a target. It returns an opaque plan token, typed action assessment,
redacted decisions, blockers, resumable transaction identity and structured outcome as applicable.
`Converge` runs or resumes the exact authorized plan to a durable public outcome.

The caller supplies the goal and authorization, rather than arbitrary registry paths, config/data
roots, executables, Incus binaries or raw environment. Trusted construction supplies physical
adapters. `yard update --rollback` uses this same seam with the retained previous release as target.
`yard migrate` uses the same owner to reconcile the exact installed release without selecting a
version or rotating links to another release; see the [current command contract](../control-plane.md#release-migrations).

## Migration state and activation state

The ledger records each domain's epoch and exact applied migration IDs. The shipped domains are
`settings`, `project-state`, `owner-registration` and `power-metadata`; their current epochs and
supported minima come from [`config/release-transition.json`](../../config/release-transition.json).
Registry v2 defines a stable order of one-time transitions, for example:

```json
{
  "schemaVersion": 2,
  "minimumEpochs": {"settings": 1},
  "currentEpochs": {"settings": 2},
  "migrations": [
    {
      "id": "canonicalize-test-vms-settings-v2",
      "domain": "settings",
      "fromEpoch": 1,
      "toEpoch": 2,
      "kind": "test-vms-settings-v1-to-v2"
    }
  ]
}
```

Each migration advances exactly one domain by one epoch. Cross-domain dependencies name stable
migration IDs and must precede their dependents in registry order. Validation rejects cycles, gaps,
unknown domains/kinds and mismatched applied history before mutation.

Derived runtime artifacts are outside migration epochs. Owner/registration schema changes are
one-time migrations; broker engine/facade and installed power executable/unit convergence are
activation responsibilities, including systemd reload and verification. A resource's persisted
schema and its executable generation are separate contracts.

Activation observes exact `current`/`previous` release-store links and resource generations/digests.
The ledger does not serve as a desired-state marker for reconcilers. Completed migration history
stays complete; later activation drift requires a fresh repair plan and authorization.

## Typed registry capabilities

The registry selects stable IDs, closed kinds and explicit dependencies from the candidate's
compiled capability catalog. The compiled implementation owns resource/setting allowlists,
classification, typed recipes, impact/recovery policy, compatibility, verification, evidence
schemas and size/count limits. Catalog identity is bound to the plan and journal.

Registry data cannot weaken effect/recovery/confirmation policy or supply executables, argv, shell,
URLs, raw environment names, arbitrary filesystem paths, regex/templates or arbitrary payloads.
Unknown state or ownership blocks the affected operation rather than selecting a fallback mutation.
See [`registry_v2.go`](../../internal/releasetransition/registry_v2.go) and
[`catalog.go`](../../internal/releasetransition/catalog.go).

## Decision matrix and reset

The planner classifies affected resources before confirmation. The matrix states the safety meaning
of the closed decisions; a compiled capability must actually support the selected recipe and its
recovery class. It does not grant every resource every decision.

| Decision | Semantics | Recovery constraint |
|---|---|---|
| `preserve` | Value/state is already valid; no mutation | Verify only |
| `transform` | Lossless typed conversion | Typed inverse or protected before-image when recovery needs it |
| `canonicalize` | Meaning is preserved; legacy representation is removed | A compatible previous target must read the canonical form; obsolete bytes need not be restored |
| `reset` | Incompatible value is replaced with the declared default/absence | Protected evidence or explicitly irreversible recovery class |
| `quarantine` | Data leaves the active namespace without loss | Restoration requires an empty target and a matching fingerprint |
| `block` | Intent/ownership is ambiguous or policy is absent | Mutation does not begin |

A planned reset needs no separate `--reset` flag. It is visible in the exact update plan and uses
the same single top-level confirmation:

- A reversible/recreatable reset uses `prompt-default-yes`.
- Irreversible loss of meaningful state uses `prompt-default-no`.
- Automation `--yes` answers the consent gate and bypasses neither blockers nor stale checks.
- A successful planned reset ends with `ready` and exit `0`.
- Reports identify redacted setting IDs/scopes and safe canonical results without assignment values.

Generic reset is allowed only for a catalog-known non-secret setting owned by the selected scope.
Secrets, inherited/profile-owned values, unknown keys, ambiguous local/effective ownership and
conflicting concurrent drift block before mutation. Complete settings/config dumps are neither
stored as generic evidence nor printed.

The journal records expected and desired fingerprints and the capability's protected recovery
evidence before mutation. Compare-and-set is followed by observation and verification. After a crash:

- Exact expected-before permits repeating apply.
- Exact desired-after permits recording progress and continuing verification.
- A third value yields `operator-action-required/migration-stale` without overwrite.

A completed reset never reopens in the next update. A key forbidden by the current schema is a
current writer/loader validation error; its historical migration does not become a permanent reconciler.

## Execution and durable checkpoints

`Inspect` completes before confirmation. It validates candidate identity, artifact/registry/catalog
bindings and supported epochs; observes runtime links, ledger and affected resources; classifies
decisions/blockers; and builds the redacted plan and canonical action assessment. The opaque plan
token binds material observations, resource scope and impacts.

After authorization, `Converge` serializes work with the stable update lock, reobserves material
facts and rejects stale plans before mutation. It resumes an exact unfinished authorized journal,
or durably publishes a new goal/plan/grant. Its ordered work includes:

1. Pre-activation one-time migration: intent → evidence → CAS apply → observe → verify.
2. Compatibility and retained-target checks before activation.
3. Durable activation intent and compare-and-swap of `previous/current` links.
4. Stable-order activation reconciliation against the target release.
5. Reobservation of ledger, links and resource generations before publishing `ready`.
6. Separate bounded recovery cleanup.

The journal's phase/checkpoint optimizes resume; intent, evidence and observed facts determine
correctness. Unknown adapter results require reobservation. A cleanup error after a verified fixed
point retains evidence and produces a warning while leaving the update `ready`.

Readiness observers return state and diagnostics. Expected drift is `Converged: false`; persistent
notices are warnings; failures are errors. The operation boundary renders validated diagnostics
and current warnings once, sorted and deduplicated. See the
[activation diagnostic contract](../control-plane.md#release-migrations).

## Forward recovery and rollback

Before activation, errors leave the previous runtime active. A published candidate alone does not
change stable links. An unstarted valid plan reports `migration-required`; a valid authorized
unfinished transaction reports `recovering`; ambiguous actual state requires operator action.

Activation recognizes only the exact enumerated link checkpoints. Unknown/foreign link pairs yield
`operator-action-required/activation-ambiguous` without heuristic switching. Once the candidate is
active, recovery proceeds forward to its verified fixed point; the owner does not blindly run a
previous runtime as error compensation.

Explicit rollback is a new assessed `activate-previous` goal with its own plan and confirmation.
Compatibility must prove that the retained target is available and verified, can read current
canonical state, and supports the already completed capability results under its registry/catalog
or the compiled backward-compatible-no-op policy and legacy minimum. Actual links/resources must
match allowed checkpoints. Retention alone does not prove compatibility.

Exact continuation of an unfinished authorized transition needs no new prompt when the journaled
grant, release pair, artifact/registry/catalog bindings and observation scope match. New impacts or
resets require a new plan and authorization. Completed history cannot reuse a grant to authorize
new drift repair.

Changed scope in an activation-only forward `reconciling` journal may receive a bounded replacement
after fresh native ownership-aware planning and new authorization. The target and exact links,
verified owner/artifact/registry/catalog, completed ledger and canonical predecessor evidence remain
bound. A separate negotiated process contract and immutable receipt precede current-journal CAS;
the ordinary V2 successor resumes without rotating links or replaying migrations. Source/settings
steps, replacement chains and unknown partial application are excluded. See the implemented
[recovery contract](../control-plane.md#release-migrations).

At each process boundary the owner revalidates the protected sealed target against `ArtifactDigest`
bound to the plan and journal, deriving version/registry facts from that artifact. Owner registry
and target registry are checked separately. An altered or unavailable sealed target blocks before
mutation. Protected recovery additionally checks exact journal goal/transaction and actual links;
a standalone ready outcome does not bypass those checks.

## Public status and command gating

| Status | Meaning | Mutation behavior |
|---|---|---|
| `ready` | Target active; migration and activation fixed point verified | Ordinary commands allowed |
| `migration-required` | A safe assessed transition or repair is needed | No implicit mutation; authorize the reported plan |
| `recovering` | A valid unfinished journal can continue from observed facts | Exact transition resume allowed; unrelated mutations gated |
| `operator-action-required` | Automatic mutation is unsafe | Read/status and the exact safe recovery action |

An unsuccessful mutating update reports status, active/previous/target identity, stable code,
transaction identity where available and a safe next action. Internal checkpoints are not public
status values. Inspection can expose structured checkpoints without conflating them with outcomes.

Read-only status/RPC do not implicitly finalize a transaction. Ordinary CLI startup does not perform
recovery mutation. Inspection exits `0` when inspection itself succeeds, including when work remains;
readiness comes from the structured outcome. Apply succeeds only after verified readiness.
Current command-specific guidance is in [Release migrations](../control-plane.md#release-migrations).

## Internal seams and adapters

The external seam stays `ReleaseTransition.Inspect/Converge`. Internal physical responsibilities
include verified artifact resolution and stable-link CAS; protected journal/evidence writes,
locking, fsync and bounded cleanup; typed scope-local settings CAS; protected file ingress;
Incus inventory/effective configuration and owned metadata; exact installed runtime/systemd
reconciliation; broker activity/lease observation; and bounded privileged leaves after consent.
These are architectural roles, not a second generated list of production interface names.

Production uses filesystem, Incus, systemd and child-process adapters. Tests substitute temporary,
in-memory and fault-injection implementations at those boundaries. A registry-selected generic
`RunMigration(kind, payload)` or arbitrary `CommandRunner` would violate the typed capability boundary.

Source-install and legacy ingress live below the same transition authorization and journal. They
can inventory prospective imported state without writing it and perform closed typed ingress
steps; they do not gain independent runtime activation or a second rollback state machine.

## Supported compatibility and recovery

The frozen [process protocol v1](../../internal/releasetransition/protocol/v1) and
[journal v2](../../internal/releasetransition/journal/v2) preserve the supported published-updater,
retained-runtime rollback and unfinished-transaction recovery contracts. Internal types do not
silently extend their fields or outcomes. Strict decoding, enum/bounds validation, canonical
journal bytes and fingerprint bindings remain part of compatibility.

A blocked rollback projects the existing `operator-action-required/rollback-incompatible` outcome
without an executable plan. The V1 adapter's special presentation for completed-history activation
drift does not authorize or resume completed migration history; apply still requires a fresh plan
and creates a new repair transaction. The current caller validates the identity and actual links.
The [implemented compatibility contract](../control-plane.md#release-migrations) describes this projection.

Recognized legacy journals are imported through bounded compatibility ingress with the original
protected evidence retained and actual postconditions verified. There is no supported manual phase
edit. Exact supported older-engine failures retain dedicated recovery rules rather than a general
permission to replace journals or links. Use the maintained [legacy upgrade](../control-plane.md#legacy-upgrades)
and [interrupted recovery](../control-plane.md#interrupted-release-recovery) guidance.

Protocol evolution first ships readers/writers alongside the old version while continuing to send
that version; only a later release replaces the default protocol with supporting owners. Optional
same-owner recovery is capability-negotiated separately and must prove retained-caller compatibility
before writing its unchanged V2 successor. Retain old semantics
for the supported upgrade/rollback/recovery horizon. Rebuilding both ends from current source does
not prove published-binary compatibility.

## Public error taxonomy

- `registry-invalid`, `unsupported-epoch`, `unsupported-kind`: release/state compatibility defect before mutation.
- `confirmation-required`, `operation-declined`: authorization gate.
- `plan-stale`, `migration-stale`, `resource-conflict`: concurrent or drifted observation.
- `precondition-blocked`: lease, ownership or topology prevents mutation.
- `dependency-unavailable`: retryable dependency failure.
- `verification-failed`: mutation may have occurred; protected recovery state must be preserved.
- `activation-ambiguous`, `recovery-ambiguous`, `journal-invalid`: automatic recovery is unsafe.
- `rollback-incompatible`, `rollback-expired`: retained target is outside the proven horizon.

Diagnostics redact secrets, assignment values, complete configs, raw environment and host-specific
payloads. They can name validated canonical IDs, scopes, yard/project/instance references and safe
inspection paths where the diagnostic contract permits them. Raw hook/guest error text stays private.

## Validation requirements

These are evidence requirements, not a claim that this documentation change ran acceptance:

1. State-machine and fault-injection coverage traverses durable journal writes, adapter mutations,
   verification and link switches, including allowed crash-state combinations and unknown results.
2. Every outcome remains structured; ambiguous/foreign journals, links and resources fail closed
   with a safe next action, without manual journal edits.
3. Completed migrations remain complete under arbitrary later drift; activation repair changes
   resource generations without rewriting epoch history.
4. Planned reset/canonicalization survives interruption around CAS, uses one confirmation and reaches
   `ready`; a repeated converged update is a no-op. Unknown/secret/inherited/ambiguous values block.
5. Read-only operations remain reachable during recovery while unrelated lifecycle/boot mutations
   are gated.
6. Ordinary update, same-version retry, source ingress, installed-release repair and explicit rollback
   use the same transition owner and protected binding rules.
7. Host-free contracts cover artifact validation, epoch history, plan binding, activation CAS,
   cleanup warnings and statuses. Published-updater checks preserve the frozen wire/storage contract.
8. Disposable VM acceptance covers predecessor → candidate, reset/canonicalization, durable-checkpoint
   interruption/reboot, forward resume, explicit compatible rollback and repeated no-op convergence.

## Decision rationale

One owner and a narrow inspection/convergence seam keep planning, authorization and recovery bound
to the same exact facts. Splitting these responsibilities between independent state machines would
multiply mixed states and make compensation depend on unverified previous-runtime behavior.

Compiled typed capabilities provide bounded policy flexibility without a declarative interpreter
that can expand blast radius or weaken confirmation from release data. One-time history and current
activation reconciliation remain separate so runtime repair does not require reopening old migrations.

A privileged recovery daemon or separate rescue runtime requires a demonstrated need. The current
in-process owner, versioned protected journal and bounded compatibility adapters do not add a second
background lifecycle. Candidate self-check, retained-target compatibility and fail-closed outcomes
remain the recovery boundary.
