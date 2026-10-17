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
  readers have an explicit supported upgrade floor; unfinished transactions retain their exact
  recovery owner and evidence until terminal completion.
- Rollback requires an intact retained release and proven state compatibility. Some declared resets
  can remove rollback eligibility; their recovery class must be visible before authorization.
- Recovery evidence is protected. Retirement requires proof that terminal evidence is closed and
  unreferenced; age or release count does not authorize cleanup.

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

Completed history can use a separate compact checkpoint: per-domain prefix proof and applied suffix
replace the growing applied list in the authoritative record. Frozen Ledger V2 remains an immutable
compatibility projection, validated on every checkpoint read. Native migrations advance authoritative
history once; existing journal/evidence bindings keep their meaning. See [Migration retirement](#migration-retirement).

`internal/migrationhistory` owns the validated registry, per-domain frontiers,
canonical next edges and historical journal traces. Completed domains need not form a
prefix of the registry's total order: each domain must have its exact prefix and all
completed migrations must satisfy their dependencies. Fresh planning selects the first
pending migration in registry order. Recovery reconstructs the journal's exact edges
from a validated observed frontier at a durable cut, preserving completed independent
domains when a pending migration was inserted earlier in that order. The not-yet-applied
suffix still follows the current registry's canonical next order.

The release-transition adapter retains the V1/V2 codecs, byte fingerprints, protected
storage, evidence grammar, capability/support policy and CAS. Interrupted private staging files
may be completed only when their bytes are an exact prefix of the independently reconstructed
intended record. A journal CAS may discard stale staging only while the selected record still
matches its expected snapshot. Published records retain strict nonempty size, ownership, mode
and link validation; staging supplies no authority. Unsafe or conflicting immutable staging
refuses without overwriting it. It checks journal pins
against encoded edge endpoints before effects. Opaque authority and compatibility
projection values have different types; a hash alone cannot supply authority. Ledger
work binds the complete semantic registry, including ordered definitions, epoch floors,
current epochs and retired-prefix pins. Matching one selected migration is insufficient.
The adapter's authoritative ledger revision and historical compatibility revision are
distinct inputs to its work and source-history consumers.

Work carries one settings, recorded-settings, owner, ingress or ledger recipe variant.
Constructors own nested inputs; reconstruction and per-step guards reject missing,
foreign and typed-nil recipes before effects. The effect boundary consumes a private
bound work step that ties its recipe to the exact transaction, intent and evidence
phase. Recorded settings results can be observed and verified without supplying a
mutation recipe. Frozen journal/evidence DTOs still pass their existing codec,
transaction, phase and physical freshness checks.

A private transition readiness proof combines validated migration completion with exact
active links and activation fixed-point observation. Journaled completion also binds
validated completed journal facts. Successful V2 convergence results, including no-op
results, and checkpoint publication consume that proof. Migration
completion alone cannot call either consumer. The proof describes observed readiness;
it does not replace the existing lock, reobservation, authorization or CAS guards, and
it cannot guarantee that external resources remain unchanged afterward. The closed
historical v0.11.1 recovery policy keeps its existing eligibility checks.

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
steps and unknown partial application are excluded. `activation-only-replacement-v1` also excludes
replacement chains and automatic cancellation of a legacy reservation. Cancellation and subsequent
reassessment require the separately negotiated `activation-only-replacement-v2` lifecycle contract;
they do not extend frozen process V1 or canonical journal V2. See the implemented
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

The approved [legacy support boundary](legacy-migration-support.md) is v0.11.0, inclusive.
Earlier source installations use full uninstall and a separate
clean installation instead of V1 upgrade/import or unfinished source V1 recovery. No V1 upgrade
bridge or notice window is required. Already accepted V2 history with legacy provenance remains
readable, and supported V2 recovery retains its exact bindings. The source-version boundary is
separate from domain epochs and V2 checkpoint-reader compatibility.

Three independent policies determine support:

- The **upgrade floor** is the minimum readable domain epoch declared by the registry, not a release
  age or release count.
- **Rollback eligibility** requires an intact, verified retained `current`/`previous` runtime and
  compatibility with actual persisted state and completed capabilities. Passing the upgrade floor
  does not itself prove rollback eligibility.
- A **supported in-flight transaction** pins its exact sealed owner, artifacts and protected evidence
  until it reaches a terminal state, regardless of age or intervening releases. Raising the ordinary
  upgrade floor cannot revoke that recovery contract or permit deletion of its inputs.

A proved defective migration-ledger owner has one typed exception to exact-owner continuation:
`migration-ledger-owner-replacement-v1`. This separately negotiated process-schema-2 contract
initially recognizes only the exact supported official Linux amd64 v0.21.0 manifest and registry.
Version comparison alone never establishes eligibility. An explicitly selected newer standalone
candidate with checkpoint support independently proves the canonical reconciling predecessor,
sealed owner/source/candidate assets, actual links, projection/checkpoint prefixes and dependencies,
and the captured/applied/verified evidence for the two completed ledger-only power and SSH steps.
Retained terminal migration artifacts may belong only to the exact source or previous release
named by that predecessor journal. Admission independently seals the previous release and requires
its registry to equal the verified source registry, then validates every retained evidence triple
against the frozen projection. The receipt records these asset identities and transaction references;
cleanup preserves the referenced artifacts. An ordinary zero-step source update needs no fabricated
transaction directory. Other earlier releases and arbitrary same-registry targets remain unsupported.
Unrecognized state, conflicting publication, ingress and recovery chains refuse supersession.

A fresh candidate assessment and grant bind these facts and current activation consequences.
Under the update lock, the candidate revalidates the proof, publishes an immutable receipt in
`release-transition/migration-owner/v1/transactions`, then CASes the protected journal to an ordinary
V2 successor. Receipt-only crashes reuse the exact reserved successor after fresh consent; selected
successors resume normally. The receipt preserves the unfinished predecessor as superseded evidence,
without declaring its activation ready or replaying completed migrations. Cleanup retains its
predecessor artifacts and captures successor completion before later journals replace it. This
exception changes no frozen process/journal/activation-recovery/v0.11.1 archive meaning and confers
no rollback promise to the defective owner; existing rollback eligibility checks still apply.

The shared history trace validates the completed ledger edges; exact artifact eligibility and
canonical evidence policy remain in the replacement adapter. A canonical immutable receipt can
also prove the exact intended successor bytes when its journal staging file is empty or truncated,
provided the selected predecessor is unchanged. Staging supplies no authority. New owner receipts
and owner-completion records are written into an unnamed inode on the target Linux filesystem.
The writer sets and checks exact private permissions, ownership and link count, verifies the complete
canonical bytes, and fsyncs the file before a no-replace link through its held procfd makes it visible.
The parent directory is then fsynced. A process killed during any private prefix write leaves no
named partial receipt; a process killed after publication leaves the complete canonical record.
Filesystems or procfd environments without this mechanism refuse the owner-replacement feature
before publishing a receipt or successor journal; ordinary migration publication is unchanged.
Full canonical pending records from the previous writer remain resumable. Partial legacy receipt
staging, unsafe files and conflicting records remain untouched and refuse admission; the writer
never appends a fresh grant to visible legacy staging or treats staging as authority.

Released-binary acceptance retains a supported baseline for each distinct caller, producer or
compatibility-bridge contract. Checks use the actual published binaries; rebuilding current source
does not prove their compatibility. This is a contract matrix, not every pair of released versions.
Removing a baseline requires an explicit support decision and replacement evidence where needed;
neither a fixed time window nor a release-count limit silently removes coverage.

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

Fresh V1 imports are retired. Accepted bound V2 history retains read-only compatibility ingress
with its original protected evidence and required postcondition observations. There is no supported manual phase edit. Exact supported older-engine failures retain
dedicated recovery rules rather than a general permission to replace journals or links. Use the maintained
[legacy upgrade](../control-plane.md#legacy-upgrades)
and [interrupted recovery](../control-plane.md#interrupted-release-recovery) guidance.

Protocol evolution first ships readers/writers alongside the old version while continuing to send
that version; only a later release replaces the default protocol with supporting owners. Optional
same-owner recovery is capability-negotiated separately and must prove retained-caller compatibility
before writing its unchanged V2 successor. The lifecycle extension negotiates
`activation-only-replacement-v2` through process schema 2 `lifecycle-capabilities`, with
`recovery-v2-` identities and receipt schema 2. Its live and archived evidence belongs to that
separate contract; older recovery receipts retain their existing semantics. Terminal receipt
retirement requires proof of closure and absence of current, pending or retained references;
unknown or corrupt evidence blocks retirement. Age and release count supply no authority.
A protected predecessor frontier binds a monotonic generation and the latest authorized cancellation
witness. Pre-CAS input cycles select fresh generations; crash retry finishes durable cancellation
intent while requiring a new grant for changed inputs. Compaction preserves that proof and cannot
resurrect cancelled successors. V1 receipts retire only with exact terminal closure and reference
checks; V1 cancellation and replacement chains remain excluded. Modern live/archive/frontier
namespaces have independent 512-record and 32 MiB budgets; the frozen ordinary graph and V1 live
namespace retain their 256-record limits.
Successful update cleanup validates supported closed migration artifact sets from ordinary
predecessors. Exact canonical evidence triples and typed recovery bytes prove closure only;
they do not supply an execution grant, ledger authority, or runtime readiness. Source admission
still requires its historical ledger or checkpoint anchor. Unknown, incomplete, or conflicting
artifacts leave cleanup pending without deletion. A closed component remains retained when its
release is a verified current or previous runtime endpoint, its terminal ledger pin matches the
validated frozen compatibility projection, or selected/prepared journal, pending archive or live
recovery grant references it. Closed components without these references can be collected.
Older archive-only components without a supported terminal closure witness remain cleanup-pending;
archive presence alone does not establish completion. Retention protects whole graph components;
the existing successor-first deletion order, 32-transaction batch and 256-entry graph ceiling
remain in force. A new grant binds the current source frontier, while an interrupted grant
keeps its original source pin across its own ledger progress.
Detailed cancellation, publication and storage rules belong to the
[recovery contract](../control-plane.md#release-migrations).

### Migration retirement

Ledger V2 validates `Applied` as the exact completed registry prefix. Raising `minimumEpochs` or
deleting old migration IDs alone cannot retire that history: it can make an otherwise valid ledger
unreadable. The separate schema 1 `release-transition/v2/history-checkpoint.json` records per-domain
epoch, compaction boundary, prefix digest and applied suffix, registry provenance and the immutable
legacy projection's existence, fingerprint and epochs. Publication atomically switches authority;
suffix advances update only the checkpoint. The assessed transition compacts the completed suffix
after verified readiness, so later migrations do not accumulate completed IDs in the authoritative
history. A changed or missing bound projection blocks reading.

The verified owner's separate `config/release-checkpoint.json` marker selects the
`migration-history-checkpoint-v1` writer. New readers honor existing checkpoints regardless of that
marker. Initial conversion must be assessed, authorized and performed after verified readiness,
preserving unfinished authorization and journal bindings. The checkpoint capability and
stable source-ledger fingerprint bind that consent into existing `ObservationScope`. Exact marked
scope resumes conversion under the original grant; exact legacy scope resumes without conversion and
needs fresh consent afterward. A normal marked update includes conversion in one consent. Publication
runs under the shared lock after verified readiness, with atomic authority and retry across interruption.
Terminal publication retains the selected activation scope authorized before source mutations;
completed readiness can widen after the checkpoint is compact. A post-activation journal replacement
binds its fresh checkpoint consent to the actual ledger authority, after the legacy eligibility proof.
Retained assets without the marker do not enable initial conversion. Once a checkpoint exists,
advancing migration history requires the exact current/from sealed release to support its reader
contract. An unaware source can activate a reader bridge while checkpoint and projection epochs
agree; it cannot skip that bridge by selecting a checkpoint-capable future target. Initial conversion
can retain the full result in the legacy projection because no prior checkpoint authority exists.
After checkpoint history advances, the frozen projection is compatibility evidence, not rollback
permission. Before dispatching rollback to an unaware retained target, the checkpoint-aware runtime
boundary validates the checkpoint and pinned projection against that target's sealed registry,
requires matching authoritative/projection epochs and rechecks both captured records at execution.
It preserves the original selected transition owner, catalog and journal bindings. A marked target
reads the checkpoint itself; a target without a transition registry keeps the existing active-owner
route. An incompatible target blocks before mutation.

Actual released-caller acceptance must prove the compatible owner path and frozen projection.
The supplied checkpoint-capable candidate can serve as the matching-epoch bridge; this does not
announce or publish it. The compatibility harness also seals a synthetic future epoch-3 owner from
the same bound source to test bridge-first refusal, successful advance and refusal of epoch-2 rollback.
That synthetic owner is a contract fixture, not a released baseline.

The main registry retains its live migration definitions and the current epoch floor. Future V2 prefix
removal needs an explicit retired-prefix digest matching completed history, a declared new readable floor,
and release of recovery/rollback compatibility pins that still need the full projection or old handlers. The
[legacy support policy](legacy-migration-support.md) separately governs the pre-v0.11.0 uninstall
boundary. Its removal of the V1 upgrade bridge does not retire V2 reader-bridge requirements.

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
