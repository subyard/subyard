---
title: Legacy migration support and reinstall policy
status: accepted policy
updated: 2026-10-16
---

# Legacy migration support and reinstall policy

## Approved support boundary

The approved minimum source version is **v0.11.0, inclusive**. Earlier installations use
full uninstall followed by a clean installation performed by the user. This replaces the
previous six-month notice window and V1 upgrade-bridge plan. The runtime refuses fresh
pre-boundary admission while retaining the evidence readers required
by accepted V2 histories. Physical acceptance is recorded separately against the tested candidate.

Retirement removes fresh V1 upgrades and journal imports, including unfinished source V1
recovery and new rollback targets below v0.11.0. Detection must refuse before changing the
active installation and explain the full data removal and clean-install path. Refusal never
deletes evidence. An installation that already completed its transition to supported V2 state
remains supported, including necessary historical evidence and recovery bindings with legacy
provenance. The source version, authoritative state and retained bindings must be assessed
together; an old directory or version string alone does not establish eligibility.

Reuse the existing [full uninstall](../configuration.md) mechanism and its ownership inventory,
confirmation and interrupted-cleanup recovery. The verified cleanup tooling must run against
the old installation without upgrading, activating or migrating it first, including when an
original V1 journal is unfinished. Migration-independent full uninstall is the general contract
for every source version, including pending or invalid V1/V2 journals. Validate tooling delivery
against old installations before retiring the upgrade code; a new
uninstall implementation is not required. Full removal deletes the selected installation's
owned yard data, settings, credentials, sessions, backups and runtime. Foreign resources remain
protected. Cleanup ends after verification; it does not automatically install a replacement.
Unknown ownership still refuses removal.

Historical installations, including supported V2 releases, may lack project ownership labels or
creation receipts for host directories. A verified historical creator-script contract, protected
registration and exact native association can establish a yard's bounded project/instance/volume
scope without reading migration journals; the migration source floor has no ownership meaning. Unknown
creator contracts, unrelated native
resources or conflicting ownership refuse that scope. Unrecorded legacy host namespaces, including
backups, sessions and host mounts, and shared infrastructure without installation ownership remain
preserved and are listed in the plan and final result. Full removal does not claim those paths
were deleted. Modern receipt-owned namespaces retain their existing deletion and stale-plan guards.


The cutoff concerns legacy migration V1. Supported v0.11.0+ updates, process V1, Ledger V2,
recovery receipts and checkpoint-reader contracts retain their own compatibility requirements.
The source-version cutoff does not raise the domain epoch floor or retire V2 checkpoint history.

Supported historical updates do not invent project ownership labels. Plain `yard init` refuses
an existing unlabelled project without proven authority, including one created by v0.11.0,
even after its supported update succeeds. Explicit `yard init --adopt` can register the existing
yard for normal management while preserving unproven shared and host creation ownership.
This metadata-only lifecycle operation is separate from release migration; run plain init
after adoption to reconcile. Older releases refuse its versioned adopted ownership marker.
Explicit `yard init --continue-legacy` can establish
a separately confirmed, protected grant for bounded normal reconciliation of a supported,
exactly assessed legacy resource. It does not relabel the project or authorize full removal.
The power acceptance fixture exercises this opt-in, repeated plain init, two reboots and
compatible rollback/forward while preserving the grant and native identity. Current-owned
projects retain their ordinary repeatable initialization contract.

## Existing compatibility inventory

Retired migration V1 used a global layout, schema 1 registry, applied state at
`migrations/state.json` and journals under `migrations/transactions/`. The live
[compatibility header](../../config/migrations.json) retains schema/layout fields solely so old
released callers can reach an actionable refusal; it contains no migration definitions.
Power/layout mutation and verification code without accepted-history consumers is removed.
The exact retained v0.4.1/v0.4.2 registry, journal codecs and read-only owner/route/broker observations
remain only for accepted bound V2 recovery and static completion receipts. There is no fresh-import
reader API. Current release transitions use domain epochs and Ledger V2. Superseded `_migrate`
mutation endpoints refuse and direct old updaters to staged tooling and full uninstall.

The existing released baselines cover distinct contracts:

| Baseline | Supported role |
| --- | --- |
| v0.4.1 | Retired V1 owner checkpoint/terminal: refusal/uninstall; accepted V2 historical reader |
| v0.4.2 | Retired V1 broker/route/owner checkpoints: refusal/uninstall; accepted V2 historical reader |
| v0.9.1 | Retired V1 standalone updater: refusal/uninstall |
| v0.10.0 | Last V1 release: refusal/uninstall at the upper unsupported edge |
| v0.11.0 | Actual first V2 release: inclusive source-floor update |
| v0.11.2 | Actual caller for frozen process V1 and Ledger V2; update, interrupted resume, compatible rollback and forward retry |
| v0.17.3 | Actual activation-only journal producer and recovery baseline using Ledger V2 |
| v0.18.6 | Actual completed and interrupted activation-only source-repair producer; original-owner schema-2 recovery and separately consented candidate upgrade |
| v0.18.1 | Published caller using frozen process V1 and Ledger V2; update, interrupted resume, rollback, forward retry and checkpoint-reader bridge ordering |
| v0.19.0 | Actual direct-upgrade caller with independent completed domain history; post-effects activation failure, reread and fresh-process resume on the fixed candidate |
| v0.21.0 Linux amd64 | Exact published migration-ledger owner-defect proof and separately consented standalone-candidate supersession; unknown artifacts fail closed |

The [historical reader](../../internal/migration/v1_import.go) binds the exact retained runtime
pair, registry, journal and observed postconditions. It does not admit arbitrary historical
journals. The [released compatibility check](../../dev/verify-release-upgrades.py) pins official
v0.4.1, v0.4.2, v0.9.1, v0.10.0, v0.11.0, v0.11.2, v0.17.3, v0.18.1 and v0.18.6 runtime checksums
and the exact legacy runtime installers. These binaries
remain acceptance inputs for their declared contracts; retirement replaces obsolete V1 success
cases with refusal/uninstall cases. Rebuilding old versions from current source does not replace them.

Completed-history source admission proves sealed original assets, the supported registry/catalog,
canonical journal, authoritative ledger/checkpoint and terminal typed evidence independently of old
operational readiness. A verified different candidate may prepare a new goal and fresh authorization
when old operational inspection is blocked. Historical journals are neither reopened nor delegated.
Known closed supersession and recovery provenance remain history; unresolved publications, live
reservations, unsupported source ingress or unknown provenance refuse this new fallback without
weakening ordinary/historical paths. Source repair changes only explicit allowed local alias families
and confers no release authorization. A retained unfinished original owner must advertise its own
schema-2 activation capability before source repair; newer staged tooling cannot supply it.

Frozen process V1, recovery receipt V1 and legacy migration V1 have separate support policies.
Retiring the legacy migration engine does not retire the caller protocol or recovery receipts.
The current domain upgrade floor is epoch 1 for each domain in the
[release registry](../../config/release-transition.json). The legacy standalone-upgrade boundary,
domain floor and proven retained-runtime rollback eligibility are assessed separately.

Use [`yard update --check`](../cli-reference.md#update) for read-only assessment. Unsupported
sources require the [staged full-uninstall route](../control-plane.md#legacy-upgrades), followed
by the user's separate clean install. Unknown, ambiguous or foreign state blocks before mutation;
manual journal editing is unsupported. Ordinary ready operation emits no deprecation warning.

## Supported V2 checkpoint compatibility

V2 checkpoint-reader bridges remain a separate contract from the retired V1 upgrade path.
The checkpoint writer uses the separately sealed optional
owner marker `config/release-checkpoint.json` for `migration-history-checkpoint-v1`, while retaining
the frozen Ledger V2 projection for older readers. New engines always honor an existing compact
checkpoint; retained owner assets without the marker do not enable initial conversion. Initial
conversion needs assessed authorization after verified readiness and preserves unfinished bindings.
The live registry retains its migration definitions. The compatible current-owner path
requires acceptance with actual supported released binaries; their ability to read the legacy
projection does not imply support for the new compact checkpoint. Before an existing checkpoint
advances beyond that projection, the exact current/from sealed release must support the checkpoint
reader contract. Install a verified reader bridge at matching epochs first; a capable future target
alone is insufficient. The supplied candidate can serve as that bridge once released and verified.
Keeping the old projection does not authorize rollback after authoritative epochs advance. The
checkpoint-aware runtime boundary validates an unaware target's sealed registry, matching checkpoint
and projection epochs, and pinned records before dispatch; captured records are rechecked at execution.
It preserves frozen transition owners and catalog/journal bindings. Marked targets read checkpoint
authority themselves; incompatible history blocks mutation.

Released-caller acceptance exercises the actual v0.11.2 and v0.18.1 binaries against matching epoch-2
history, then a separately sealed synthetic epoch-3 owner built from the same bound source. It requires
bridge-first refusal, successful advance after the bridge, an unchanged epoch-2 projection and
refusal of epoch-2 rollback through each released caller and the candidate. The synthetic owner is not a
published release.
Published v0.18.1 has no sealed checkpoint-reader marker; it cannot itself serve as that reader bridge.

Retirement guidance must state the v0.11.0 source boundary, affected V1 formats, full-uninstall
data loss and the user's separate clean-install step. Diagnose unsupported legacy state when
encountered; ordinary ready operation needs no deprecation warning. Supported V2 unfinished
transactions retain their exact sealed owners, artifacts and evidence until closure. The separately negotiated `migration-ledger-owner-replacement-v1` contract may retain an exactly proved
defective owner as typed superseded evidence and select a freshly authorized forward candidate;
it never manufactures old activation readiness. Compatible retained rollback within the support
boundary remains protected. Refusing an old
upgrade never authorizes deleting its data. Only an explicitly confirmed full uninstall may
remove its owned data without migration; ordinary evidence cleanup still requires proven closure
and absence of required references. See
[Supported compatibility and recovery](resumable-release-transition-design.md#supported-compatibility-and-recovery).
