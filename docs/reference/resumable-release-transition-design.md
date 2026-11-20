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
  Releases that read headless-plus-reporting VM configuration must retain `_vm-start-policy`
  in the bound command manifest with effect `mutate`: rollback uses this reader-support probe.
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
Registry v2 defines a stable order of one-time transitions. This historical example is retained
reader vocabulary; the live registry keeps only the ledger-only `introduce-nm-device-guard-v4`:

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
compatibility projection, validated on every checkpoint read. Historical migrations advanced authoritative
history once; existing journal/evidence bindings keep their meaning. See [Migration retirement](#migration-retirement).

`internal/migrationhistory` owns the validated registry, per-domain frontiers,
canonical next edges and historical journal traces. Completed domains need not form a
prefix of the registry's total order: each domain must have its exact prefix and all
completed migrations must satisfy their dependencies. The semantic registry retains canonical edge ordering for history validation. Readers reconstruct
the journal's exact edges from a validated observed frontier at a durable cut, preserving completed
independent domains. Current admission refuses below-floor authority or unfinished unverified
historical steps instead of planning those edges. A fresh current baseline has no pending path.

The release-transition adapter retains the V1/V2 codecs, byte fingerprints, protected
storage, evidence grammar, capability/support policy and CAS. Interrupted private staging files
may be completed only when their bytes are an exact prefix of the independently reconstructed
intended record. A journal CAS may discard stale staging only while the selected record still
matches its expected snapshot. Published records retain strict nonempty size, ownership, mode
and link validation; staging supplies no authority. Unsafe or conflicting immutable staging
refuses without overwriting it. It checks journal pins
against encoded edge endpoints when reading historical evidence. Opaque authority and compatibility
projection values have different types; a hash alone cannot supply authority. Historical validation
binds the complete semantic registry, including ordered definitions, epoch floors, current epochs
and retired-prefix pins. Matching one selected migration is insufficient.

Only generic source ingress carries an executable work recipe. Constructors own nested inputs;
reconstruction and per-step guards reject missing, foreign and typed-nil recipes before effects.
The effect boundary consumes a private bound work step tying its recipe to the exact transaction,
intent and evidence phase. Frozen historical migrations have no recipe constructor or effect
consumer. Their journal/evidence DTOs retain codec, transaction, phase and physical freshness checks.

A private transition readiness proof combines validated migration completion with exact
active links and activation fixed-point observation. Journaled completion also binds
validated completed journal facts. Successful V2 convergence results, including no-op
results, and checkpoint publication consume that proof. Migration
completion alone cannot call either consumer. The proof describes observed readiness;
it does not replace the existing lock, reobservation, authorization or CAS guards, and
it cannot guarantee that external resources remain unchanged afterward. Closed
historical v0.11.1 records retain their validation grammar, without a replacement writer.

Derived runtime artifacts are outside migration epochs. Owner/registration schema changes are
one-time migrations; broker engine/facade and installed power executable/unit convergence are
activation responsibilities, including systemd reload and verification. A resource's persisted
schema and its executable generation are separate contracts.

Activation observes exact `current`/`previous` release-store links and resource generations/digests.
The ledger does not serve as a desired-state marker for reconcilers. Completed migration history
stays complete; later activation drift requires a fresh repair plan and authorization.

## Typed registry capabilities

The catalog freezes stable IDs, closed kinds, compatibility rules and evidence grammars understood
by historical readers. Its exact identity remains bound to old plans and journals. The live registry
selects no migration kind, and these descriptors supply no executable implementation. Generic source
ingress and activation keep their own typed recipes, impact/recovery policy and size/count limits.

Registry data cannot weaken effect/recovery/confirmation policy or supply executables, argv, shell,
URLs, raw environment names, arbitrary filesystem paths, regex/templates or arbitrary payloads.
Unknown state or ownership blocks the affected operation rather than selecting a fallback mutation.
See [`registry_v2.go`](../../internal/releasetransition/registry_v2.go) and
[`catalog.go`](../../internal/releasetransition/catalog.go).

## Decision matrix and reset

The matrix states the retained safety meaning of closed decisions in historical records and current
source ingress. An executable owner must support its selected recipe and recovery class before
confirmation; a historical descriptor alone grants no mutation authority.

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

An explicitly selected release newer than the pinned target may replace an unfinished forward journal
at any checkpoint, with a fresh plan and fresh consent. This is forward recovery: nothing is undone, and
no previous runtime runs. The plan binds the exact journal fingerprint. Its bytes and, once the successor closes, an
unclosed transaction directory leave the transaction graph, so replacements may repeat and older engines read a clean graph. See
[Interrupted release recovery](../control-plane.md#interrupted-release-recovery).

This also admits a validated `recovery-v2-` successor only at `reconciling`, with zero migration
steps, forward activation, no source ingress, an activated target and no live reservation.
The fresh plan and consent bind its exact journal fingerprint. Before publishing the successor,
the owner preserves the superseded journal and its existing recovery receipt, cancellation and
frontier records outside the transaction graph. Only after verified successor completion do
the exact copied live records leave the recovery namespaces. Archive-only crashes retain the
original authority; retries observe the selected journal and reuse exact archived bytes.

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

Fresh rollback with declared mutating source work requires the exact optional process-schema-2
`rollback-source-activation-v1` capability from its verified retained owner before consent. Typed
settings, owner-registration and source-install decisions require it; unknown mutating scopes also
require it. Known activation, power-metadata, migration-history and legacy-V1 identity scopes do
not establish source work. Preserve decisions and ledger-only plans keep the ordinary compatibility
path. An absent or unsupported probe refuses with `rollback-incompatible` before mutations. A
version number alone cannot establish the capability. Registryless targets keep their verified
active-owner path, and an already-authorized transaction keeps its original pinned owner.

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
readable, and supported V2 recovery retains its exact bindings. Supported installations are
assumed to have no V1 journals: ordinary commands ignore V1 files without reading or deleting them.
Stray V1 files require manual repair if needed. An unfinished V2 `legacy-v1-import` journal
requires those files and current tooling refuses it without writes; completed V2 legacy
provenance and recovery evidence remain readable. The source-version boundary is
separate from domain epochs and V2 checkpoint-reader compatibility.
When an installed v0.28.x updater's completed-metadata fallback refuses a completed V2 import
with committed V1 remnants, wait until no update is running, then move aside only
`migrations/state.json` and the whole
`migrations/transactions/` directory to a private backup outside the configuration home.
Nothing else needs cleanup or reinstallation; keep `migrations/update.lock`, V2 metadata and
all other data in place. The [retained committed V1 workaround](legacy-migration-support.md#retained-committed-v1-files-with-v028x-updaters)
explains the binding limitation and the earlier v0.28.x source-history refusal for an
unfinished original V1 journal under a completed V2 import; the same manual step applies.

Three independent policies determine support:

- The **upgrade floor** is the minimum readable domain epoch declared by the registry, not a release
  age or release count.
- **Rollback eligibility** requires an intact, verified retained `current`/`previous` runtime and
  compatibility with actual persisted state and completed capabilities. Passing the upgrade floor
  does not itself prove rollback eligibility.
- A **supported in-flight transaction** pins its sealed owner, artifacts and protected evidence.
  With the six historical handlers retired, an unfinished v0.28.x transaction remains supported
  when all migration steps are verified and the authoritative ledger is at the live floor.
  Earlier frontiers require manual repair. Refusal never permits deletion of their inputs.

Historical `migration-ledger-owner-replacement-v1` is a retained evidence contract. Its frozen
process-schema-2 capability probe remains available, while the current engine no longer selects
or writes replacements. Historical writers through v0.25.1 could supersede the exact supported
Linux amd64 v0.21.0 manifest and registry after independent proof and fresh authorization;
version comparison alone never established eligibility. The current registry differs, and
below-anchor source admission also prevents a new replacement by the current candidate.

Readers continue to validate immutable receipts in
`release-transition/migration-owner/v1/transactions`, their canonical predecessor and initial
successor, frozen projection/checkpoint, completed evidence triples, exact source/previous-release
bindings and retained transactions. Cleanup keeps those artifacts and captures successor completion
before a later journal replaces it. An unfinished predecessor is superseded evidence, never
activation readiness. Canonical owner-completion records retain private immutable publication;
partial legacy receipt staging, unsafe files and conflicting records remain untouched and refuse.
Source admission refuses pending journal publications; the retired receipt-bound prepared-journal
continuation is no longer implemented.

The current engine also no longer writes v0.11.1 post-activation superseded archives or delegates
v0.17.3 activation repair. Archive codecs and transaction graph readers remain necessary for
completed installations. These retirements change no frozen process/journal/activation-recovery
meaning, retained rollback checks or generic activation-only recovery. The separate registry
retirement below raises the domain floor while preserving its frozen history readers.
Explicit `--version` selection with an unfinished journal and an installed release from v0.11.0
to below v0.28.0 returns the existing `source-anchor-required` guidance. Sources before v0.11.0
retain the full-uninstall and separate clean-install policy. Ordinary installed-owner resume and historical retry hints
remain available without `--version`; historical recovery still belongs to its compatible release.

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

Fresh V1 imports and unfinished `legacy-v1-import` resume are retired. Completed V2 history
retains its protected evidence and recovery bindings without reading original V1 journals.
Stray V1 files require manual repair; manual editing of supported V2 phases remains unsupported.
Exact supported older-engine failures retain
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

The live registry keeps only the ledger-only `introduce-nm-device-guard-v4` (power-metadata 3 to
4, depending on the retired `complete-registered-guest-disk-tmp-v3`) and floors
owner-registration 3, power-metadata 3, project-state 1 and settings 3. A live dependency on a
retired ID is satisfied by the domain floor. Ledger-only kinds keep the generic ledger work path;
a retired kind refuses with `migration-manual-repair-required`. The six v0.28.x definitions and original registry bytes are frozen
in `history_v028.json`; their exact registry digest is checked before use. Catalog identities,
per-domain prefix proofs, typed recovery and step/evidence validators remain data-reader
contracts. There is no executable work recipe or effect dispatch for these historical kinds.

Live admission enforces the raised floor. `FreshBootstrapLedgerV2` starts at the floor with empty
`Applied`, and the live ledger-only steps follow the baseline step; existing historical `Applied` prefixes retain their original interpretation. Readers
select the declared baseline for a pinned fresh projection, and the frozen vocabulary for old
projections and journal traces, without rewriting either projection.

The initial bootstrap is self-activation with no previous release, journal, ledger or checkpoint.
It records one `initialize-ledger-v2.initialize` step, with resource `ledger.bootstrap`, whose
canonical desired bytes are the floor baseline. The existing V2 Intent/Captured/Applied/Verified
machinery publishes it by CAS from the absent snapshot before native activation. A restart
reconstructs that exact authorized intent and accepts only absent or exact desired bytes; it
never reclassifies a prior transaction or predecessor as fresh. Publication of the schema-1
checkpoint still requires Ready and keeps this ledger as its pinned projection. No persistent
field or version changes; the additional step kind fits the frozen V2 journal/evidence grammar.
An unpublished baseline keeps its original journal when a plan becomes stale. A newer-release
replacement must wait for the installed owner to publish that first ledger; recovery uses the
original bootstrap transaction and grant.
A fresh install has no v0.28.x previous runtime to select for rollback. Completed v0.28.x updates
keep their original history and eligible retained rollback targets.

Completed v0.28.x history remains readable both with and without a checkpoint. Historical trace
before-images retain the original epoch-1 baseline and exact absent-ledger pins. Checkpoints may
retain an earlier compaction frontier and a completed suffix; the current owner can compact that
suffix with consent. A v0.28.x journal interrupted after all six steps are verified, with a top-epoch
ledger but no checkpoint yet, remains eligible for newer-release replacement. The successor keeps
its bytes and completed effects, records no historical migration steps, and preserves
tag-reader rollback.
A changed live registry digest is not substituted for the original journal's sealed owner binding.

A below-floor ledger or an unfinished historical step other than `StepVerified` refuses before
writes with `migration-manual-repair-required`. Missing protected history is a typed read error
(`ErrMigrationLedgerAbsent`), never a completed baseline value. Only observation of initial
bootstrap, an unfinished baseline publication step, or an already determined rollback refusal
can admit that absence. Other transitions report: `protected migration history is absent:
ledger.json and history-checkpoint.json are missing`. Corrupt files remain read errors. A pinned
projection without a checkpoint remains valid plain history; a checkpoint without its required
projection is invalid. `FrozenHistoryBaselineV2` supplies the historical initial state only for
retained traces, independently of the fresh constructor.
Protected data and foreign resources remain untouched; full uninstall is a separate confirmed
operation for every source version.

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

The frozen v0.28.0 released-caller matrix established the matching-epoch bridge, synthetic
future-epoch advance and epoch-2 rollback refusal. Later candidates start direct admission at
v0.28.0; current Go contracts retain checkpoint/projection and unfinished-owner reader coverage.
The first boundary release additionally checks the published old-source → v0.28.1 hop → candidate
chains on disposable VMs. A source-version boundary does not retire accepted history readers.

The main registry records the retired-prefix floor and exact retired-prefix digests. The original six
migration definitions remain frozen reader data, outside that live registry. Any future prefix
retirement must likewise preserve completed history, readable rollback targets and supported
recovery proofs; raising the live floor alone does not establish that compatibility. The
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
