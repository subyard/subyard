---
title: Legacy migration support and reinstall policy
status: accepted policy
updated: 2026-11-10
---

# Legacy migration support and reinstall policy

## Approved support boundary

Direct update requires installed **v0.28.0 or newer**. Sources from v0.11.0 to below v0.28.0
refuse with `source-anchor-required` before changing active runtime links, journals, settings or data.
Run the exact published anchor, then retry the latest release:

```sh
yard update --version 0.28.0
yard update
```

The same boundary applies to the installer over an existing runtime and offline update. Immutable
staging remains available for cleanup tooling. Rollback below v0.28.0 refuses; compatible v0.28.x
rollback remains supported. Incomplete or contradictory state cannot be repaired by comparing
versions: exact unfinished transactions retain their sealed owner, artifacts and evidence.
Any other unfinished transition while the installed release is below v0.28.0 refuses once with
`next: yard update && yard update --version 0.28.0 && yard update`, or `yard update && yard update`
when it already targets v0.28.0 or newer: the installed release first finishes its own journal. If
it cannot, the repair path is a v0.28.x release that owns that recovery; do not uninstall.
The anchor does not change `minimumEpochs`, persistent formats or retained history readers.

Sources before **v0.11.0** retain full uninstall followed by the user's separate clean installation.
This removal route deletes owned data only after its existing explicit confirmation; an update
refusal never authorizes deletion. There is no automatic update chain or new bridge release.

Retirement removes fresh V1 upgrades and journal imports, including unfinished source V1
recovery. Detection must refuse before changing the active installation and explain the full
data removal and clean-install path. New rollback targets below v0.28.0 also refuse, but keep
the current release active and offer no uninstall path. Refusal never deletes evidence. An installation that already completed its transition to supported V2 state
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


The cutoff concerns legacy migration V1. Fresh v0.28.0+ updates, process V1, Ledger V2,
recovery receipts and checkpoint-reader contracts retain their own compatibility requirements.
The source-version cutoff does not raise the domain epoch floor or retire V2 checkpoint history.

Supported updates run versioned compatibility migrations for registered local yards through
the release journal. Plain `yard init` finishes missing compatibility work before its strict
ordinary initialization; `--adopt` remains a compatible explicit entry. Bounded registration
and native identity checks precede project and selected-resource receipt publication. This
adoption path cannot be rolled back, so it takes existing local devices under management as they
are, including unknown names, memory devices and unregistered or changed `ws-*` devices, and its
consent says so. Ordinary reconciliation owns any later convergence. Device refusals remain
for a missing or different `srv` device, because reattaching it would change persistent data, the
`ALLOWS_HOST_ACCESS=false` boundary, a selected loopback or Tailscale resource device that fails its
recipe, a foreign or inherited resource receipt, a project root or NIC with a different type, path,
pool or network, and local and expanded devices that disagree. Selected owner-IPv4 routes, their
pending receipts and startup intent, and other project root and NIC properties enter as found. Project
ownership metadata contains no process-local path. Metadata stamped by an earlier release is kept as found
when its context differs only in the repository root and configuration directory; any other difference
refuses. A revision-only marker that an old disabled backend left on the instance also enters as found;
an enabled backend, a schema key or a conflict with the protected mode still refuses, and strict
continuation stays closed to it. Working
resource endpoints are retained: only a shipped default or unset setting imports the native value,
and an operator-set or saved value is left untouched, while unproven shared and host creation
ownership is preserved. Refusals are reported together, per yard, before any journal exists.
Guest temporary-storage policy installation never implies restart consent: a required restart
or temporary boot asks separately after migration begins, even with `--yes`. Decline or EOF
leaves the journal incomplete and resumable without restarting the yard. A guest that fails its
read-only safety assessment reports every reason it can determine; the refusal names the yard and those
reasons, and nothing is authorized or changed. Activation keeps its approved integration plan across
exactly what these verified steps publish and their restart outputs; any other change still refuses as
stale. Persistent data,
instance UUID and desired power remain protected. Completed migrations are not reopened for drift.
Resume pins native instance identity and configuration while the journal is migrating.
After all native rows are verified and the journal leaves migration, activation legitimately
changes those observations. As in the published hotfix, continuation finishes that transaction
without reopening its completed native rows, including after a post-activation replacement.
Explicit `yard init --continue-legacy` can establish
a separately confirmed, protected grant for bounded normal reconciliation of a supported,
exactly assessed legacy resource. It keeps its closed device list while reporting every refusal
together, does not relabel the project and does not authorize full removal.
The power acceptance fixture covers registered ownership through repeated plain init,
two reboots and compatible rollback/forward; it requires the continuation grant to be absent.
The separate `legacy-continuation` acceptance lane exercises the opt-in grant through
repeated plain init, two reboots and compatible rollback/forward. It explicitly seeds an
unmarked native project after a verified installed upgrade, preserving the native UUID and
data; this synthetic boundary does not claim an authenticated historical grant. The lane
also verifies that continuation alone never authorizes full removal. Current-owned
projects retain their ordinary repeatable initialization contract.

Published direct baselines are v0.28.0, the latest v0.28.x patch if different, and the previous
published release if distinct. The immutable installer and archive digests live in
[`published-upgrade-baselines.tsv`](../../dev/e2e/published-upgrade-baselines.tsv); its fourth column
labels `anchor` (also direct), `direct` and `refusal` expectations; `pin` rows supply immutable assets for fixture lookups
only and never create matrix or periodic checks. Release preparation refuses if the previous published
release is not a direct or anchor row or local release tags are unavailable. Candidate versions must sort
above their direct baselines.

Refusal coverage includes v0.11.0, v0.17.3 and v0.27.0; pre-v0.11.0 removal retains its own pinned
harness and full-uninstall lane. The first boundary release additionally runs
v0.11.0 → v0.28.0 → candidate and v0.27.0 → v0.28.0 → candidate. The complete old-source matrix
remains frozen in the v0.28.0 tag; later candidate acceptance does not repeat it.

## Moving the update boundary

Move the boundary when the supported matrix's cost or age justifies retiring a source path.

1. Publish the new anchor first and keep its installer/archive assets available permanently.
2. Change `UpdateAnchorVersion` and mark its verified TSV row `anchor` (exactly one).
   The anchor also supplies the direct baseline; retain a `direct` row for the previous release
   when distinct, and choose representative below-anchor `refusal` rows.
3. Add one-time chain catalog entries for the first boundary release, then remove them after
   that release. Each release refuses with its own anchor: successive boundaries require
   operator-directed hops through those anchors, one by one.
4. Review each fixture: is its subject reachable from an install at the new anchor? Rebase
   reachable subjects onto the anchor, retain older creation pins only for retained-state
   subjects, and remove frozen paths. Run the owning host-free and physical checks.
5. Remove obsolete product code later as a separate measured change; changing the source
   boundary alone neither deletes history readers nor raises epoch floors.

The TSV/constant contract and shared fixture readers keep the anchor selection synchronized.
Post-activation takeover of an anchor journal relies on the candidate still reconciling the
anchor's activated state and on intact migration authority; removing that code makes takeover a
refusal again. A defective hop is repaired by an anchor-series fix release, not a bridge release.

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

The historical released producers and their retained contracts are:

| Baseline | Supported role |
| --- | --- |
| v0.4.1 | Retired V1 owner checkpoint/terminal: refusal/uninstall; accepted V2 historical reader |
| v0.4.2 | Retired V1 broker/route/owner checkpoints: refusal/uninstall; accepted V2 historical reader |
| v0.9.1 | Retired V1 standalone updater: refusal/uninstall |
| v0.10.0 | Last V1 release: refusal/uninstall at the upper unsupported edge |
| v0.11.0 | First V2 release: anchor refusal; historical V2 reader |
| v0.11.2 | Frozen process V1/Ledger V2 producer; current anchor refusal, historical accepted-state readers |
| v0.17.3 | Historical activation-only Ledger V2 owner; current anchor refusal |
| v0.18.6 | Historical completed/interrupted source-repair producer; original-owner recovery stays in the anchor branch |
| v0.18.1 | Historical process V1/Ledger V2 and checkpoint-reader bridge producer; current anchor refusal |
| v0.19.0 | Historical independent completed domain history producer; current anchor refusal |
| v0.21.0 Linux amd64 | Exact published migration-ledger owner-defect proof and separately consented standalone-candidate supersession through v0.25.1, whose registry matches, then the v0.28.0 anchor hop; later candidates refuse the replacement and unknown artifacts fail closed |

The [historical reader](../../internal/migration/v1_import.go) binds the exact retained runtime
pair, registry, journal and observed postconditions. It does not admit arbitrary historical
journals. The [released compatibility check](../../dev/verify-release-upgrades.py) pins official
v0.4.1, v0.4.2, v0.9.1, v0.10.0, v0.11.0 and v0.17.3 runtime checksums, the amd64 v0.27.0 checksum
and the exact legacy runtime installers. These binaries remain current refusal/uninstall inputs. Their successful migration and original-owner recovery
corpora are frozen in the v0.28.0 tag; current Go readers retain accepted history coverage. Rebuilding
old versions from current source does not replace authentic released inputs.

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

The frozen v0.28.0 released-caller acceptance exercises the actual v0.11.2 and v0.18.1 binaries against matching epoch-2
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
