---
title: Legacy migration support and reinstall policy
status: accepted policy
updated: 2026-11-15
---

# Legacy migration support and reinstall policy

## Approved support boundary

Direct update requires installed **v0.28.0 or newer**. Sources from v0.11.0 to below v0.28.0
refuse with `source-anchor-required` before changing active runtime links, journals, settings or data.
Run the published hop release, then retry the latest release:

```sh
yard update --version 0.28.1
yard update
```

The **anchor** (v0.28.0, `UpdateAnchorVersion`) is the oldest accepted update source;
installs that completed on v0.28.0 update directly. The **hop** (v0.28.1, `UpdateHopVersion`) is the
release the refusal names. It is a fix release of the anchor series, because published v0.28.0 can
stop an update from an older release at its first running VM yard with a pending disk-backed `/tmp`
migration. Such a stopped install keeps an unfinished v0.28.0 journal that later releases cannot
replace: re-run the [installer](../getting-started.md) for v0.28.1, then update normally. Launchers
before v0.26.0 never reach the later release's refusal; see
[Release migrations](../control-plane.md#release-migrations).

The same boundary applies to the installer over an existing runtime and offline update. Immutable
staging remains available for cleanup tooling. Rollback is limited by migration epochs: the
NetworkManager device guard (`power-metadata` epoch 4, see
[Release migrations](../control-plane.md#release-migrations)) refuses rollback to every published
release through v0.29.0 as `rollback-incompatible`; rollback stays supported to retained releases
at the same epochs. Incomplete or contradictory state cannot be repaired by comparing
versions: exact unfinished transactions retain their sealed owner, artifacts and evidence.
Any other unfinished transition while the installed release is below v0.28.0 refuses once with
`next: yard update && yard update --version 0.28.1 && yard update`, or `yard update && yard update`
when it already targets v0.28.0 or newer: the installed release first finishes its own journal. If
it cannot, for example when `yard update` reports that the journal-selected release cannot provide
a valid transition inspection (a v0.28.0 update stopped at a restarted VM yard), run the v0.28.1
installer or use a later v0.28.x release that owns that recovery; do not uninstall.
The anchor does not change `minimumEpochs`, persistent formats or retained history readers.

Sources before **v0.11.0** retain full uninstall followed by the user's separate clean installation.
This removal route deletes owned data only after its existing explicit confirmation; an update
refusal never authorizes deletion. There is no automatic update chain or new bridge release.

Retirement removes fresh V1 upgrades and journal imports, including unfinished source V1
recovery. Source-version admission must refuse before changing the active installation and explain the full
data removal and clean-install path. New rollback targets below the current migration epochs
(v0.29.0 and older since `power-metadata` epoch 4) also refuse, but keep
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

Release acceptance runs the direct update from the anchor (v0.28.0, with agents) and from the
previous published release if it is neither the anchor nor the hop. The hop row keeps the
supported direct path covered by an agent-free `published:0.28.1` upgrade in Periodic E2E;
the refusal checks below assert the hop command. The immutable installer and archive digests live in
[`published-upgrade-baselines.tsv`](../../dev/e2e/published-upgrade-baselines.tsv); its fourth column
labels `anchor` and `hop` (both also direct, exactly one each), `direct` and `refusal` expectations;
`pin` rows supply immutable assets for fixture lookups only and never create matrix or periodic
checks. Release preparation refuses if the previous published release is not a direct, anchor or hop
row or local release tags are unavailable. Candidate versions must sort above their direct baselines.

Required host-free released-caller refusal coverage includes v0.11.0, v0.17.3 and v0.27.0,
each asserting the hop command. Their VM refusal checks and the v0.11.0 → hop → candidate
source-upgrade lane are optional Periodic E2E coverage, outside release acceptance and full P0;
their failure never blocks a release or PR. See [Periodic E2E](../testing.md#periodic-e2e-on-hosted-runners).
Pre-v0.11.0 removal retains its own pinned harness and full-uninstall lane. The one-time hop chains belong
to the first boundary release, v0.29.0, whose acceptance runs from its release branch; master no
longer includes them. Fixtures that step an older install through the boundary use the hop;
fixtures whose subject starts at an accepted source use the anchor. The complete old-source
matrix remains frozen in the v0.28.0 tag; later candidate acceptance does not repeat it.

## Moving the update boundary

Move the boundary when the supported matrix's cost or age justifies retiring a source path.

1. Publish the new anchor first. Keep the complete published asset sets (installer, archives,
   checksums, manifests and provenance) of the anchor, the hop and every release a documented
   recovery route names available permanently: an install whose only supported path crosses a
   missing release is stranded.
2. Change `UpdateAnchorVersion` and mark its verified TSV row `anchor` (exactly one).
   The anchor also supplies the direct baseline; retain a `direct` row for the previous release
   when it is neither the anchor nor the hop, and choose representative below-anchor `refusal` rows.
   Set `UpdateHopVersion` to the published fix release of the anchor series that completes the
   hop from older sources, and mark its own verified row `hop` (exactly one, at or above the anchor).
   Refusals print the hop; admission and rollback use the anchor.
3. Add one-time chain catalog entries through the hop for the first boundary release, then
   remove them after that release. Each release refuses with its own hop: successive boundaries
   require operator-directed hops, one by one.
4. Review each fixture: is its subject reachable from an install at the new anchor? Rebase
   reachable subjects onto the anchor, retain older creation pins only for retained-state
   subjects, and remove frozen paths. Run the owning host-free and physical checks.
5. Remove obsolete product code later as a separate measured change; changing the source
   boundary alone neither deletes history readers nor raises epoch floors.

The TSV/constant contract (`tests/release-acceptance.sh`: one anchor and one hop row, each equal to
its constant, hop at or above the anchor) and the shared fixture readers `published_anchor` and
`published_hop` keep both selections synchronized.
Post-activation takeover of an anchor-series journal relies on the candidate still reconciling the
anchor's activated state and on intact migration authority; removing that code makes takeover a
refusal again. A defective hop is repaired by an anchor-series fix release, not a bridge release.

### Input for removing pre-anchor code

The boundary itself deletes no product code. A later removal (proved unreachable by build before
deletion) starts from these facts of the current candidate:

- **Unfinished journals it no longer continues:** every unfinished transition while the installed
  release is below v0.28.0, whoever started it: the candidate refuses before the engine, including
  the historical owner fixes of v0.11.1, v0.17.3 and v0.21.0, so their replacement and recovery paths
  are not reached from admission. It continues only its own exact transaction (same target,
  direction and artifact digest); at or above the anchor the engine keeps ordinary resume,
  newer-release replacement and the post-activation takeover of an anchor-series journal.
- **Readers an install completed on the anchor still needs:** the V2 journal and runtime links, the
  migration ledger and its history checkpoint and projection at the unchanged `minimumEpochs`, and
  the recovery readers for anchor-series journals. Ordinary admission assumes supported installations
  have no V1 files and does not inspect them. Completed V2 evidence and recovery bindings with
  legacy provenance remain readable. An unfinished `legacy-v1-import` journal requires its
  original V1 files and is outside this assumption; current tooling refuses it without writes.
- **Lowest rollback target:** the first release with `power-metadata` epoch 4 (the NetworkManager
  device guard). Rollback to v0.29.0 or older refuses before activation as `rollback-incompatible`;
  rollback between releases at the same epochs stays supported.

## Existing compatibility inventory

Retired migration V1 used a global layout, schema 1 registry, applied state at
`migrations/state.json` and journals under `migrations/transactions/`. The live
[compatibility header](../../config/migrations.json) retains schema/layout fields solely so old
released callers can reach an actionable refusal; it contains no migration definitions.
Supported installations are assumed to have no V1 journals. Ordinary commands and source-repair
inventory do not read, classify or refuse on `migrations/state.json` or
`migrations/transactions/`; stray files are ignored and left untouched. If such files need repair,
the operator handles them manually. There is no V1 mutation gate. The shared migration lock is
still coordination state, not a V1 history read.

Power/layout mutation and verification code without accepted-history consumers is removed.
The V1 registry/journal reader and its read-only owner/route/broker adapters are removed.
An unfinished V2 journal containing `legacy-v1-import`, even with a verified import step, still
requires original V1 files. Current tooling refuses to open it with
`compatibility ingress operation is unavailable`, before invoking a retained owner or taking
locks, writes, resume or replacement. Metadata fallback and automatic replacement do not bypass
this refusal.
The operator repairs such an installation by hand. Completed V2 provenance, compatibility
receipts, recovery bindings, history checkpoints and Ledger V2 projections keep their existing
readers and validation. Current release transitions use domain epochs and Ledger V2. Superseded `_migrate`
mutation endpoints refuse and direct old updaters to staged tooling and full uninstall.

The historical released producers and their retained contracts are:

| Baseline | Supported role |
| --- | --- |
| v0.4.1 | Retired V1 owner checkpoint/terminal: refusal/uninstall; completed V2 evidence reader |
| v0.4.2 | Retired V1 broker/route/owner checkpoints: refusal/uninstall; completed V2 evidence reader |
| v0.9.1 | Retired V1 standalone updater: refusal/uninstall |
| v0.10.0 | Last V1 release: refusal/uninstall at the upper unsupported edge |
| v0.11.0 | First V2 release: anchor refusal; historical V2 reader |
| v0.11.2 | Frozen process V1/Ledger V2 producer; current anchor refusal, historical accepted-state readers |
| v0.17.3 | Historical activation-only Ledger V2 owner; current anchor refusal |
| v0.18.6 | Historical completed/interrupted source-repair producer; original-owner recovery stays in the anchor branch |
| v0.18.1 | Historical process V1/Ledger V2 and checkpoint-reader bridge producer; current anchor refusal |
| v0.19.0 | Historical independent completed domain history producer; current anchor refusal |
| v0.21.0 Linux amd64 | Exact published migration-ledger owner-defect proof and separately consented standalone-candidate supersession through v0.25.1, whose registry matches, then the v0.28.1 hop; later candidates refuse the replacement and unknown artifacts fail closed |

Completed V2 journals and evidence retain their sealed runtime, registry and recovery bindings;
they do not require reading original V1 files. The [released compatibility check](../../dev/verify-release-upgrades.py) pins official
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
manual editing of V2 journals is unsupported. Stray V1 files are outside automatic recovery and
may be repaired by hand. Ordinary ready operation emits no deprecation warning.

### Retained committed V1 files with v0.28.x updaters

An installation created before v0.11.0 may have completed V2 import and still retain committed
V1 files. The installed v0.28.x updater includes those files in its completed-metadata fallback
binding; the current candidate always binds their absence without reading them. That fallback
can therefore refuse with:

`candidate does not bind supported completed history to a new ordinary plan; stage a compatible candidate supporting protected metadata admission`

If the V2 import is complete but an original V1 journal is unfinished, v0.28.x can refuse
earlier, before that binding comparison. Its source-history reader reports
`legacy transaction is not closed canonical history`
(`v0.28.1:internal/migration/source_history.go:93`). The v0.28.x source-repair commands render this as
`release source admission: protected metadata is unsupported or unresolved (legacy history); finish pending publications or recovery intent with the exact sealed original owner before source repair`;
`yard update` of v0.28.x does not show it, because its fallback reports the installed owner's
original refusal instead. The same manual step below applies to those retained V1 files.

Do not move these files during a running update. Once no update is running, move aside only
these two paths from the configuration home
to a private backup outside it, then retry the same update:

- `migrations/state.json`;
- the entire `migrations/transactions/` directory, containing the retained V1 journals and
  their retained recovery files.

Both paths must be absent; leaving an empty `migrations/transactions/` directory still changes
the old updater's binding. Keep `migrations/update.lock`, all `release-transition/` metadata
and all other installation data in place. No other cleanup, migration, uninstall or reinstall
is needed. This manual workaround does not restore a V1 reader and does not apply to an
unfinished V2 `legacy-v1-import` journal.

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
encountered through source-version admission; ordinary commands do not inspect V1 files and
ready operation needs no deprecation warning. Supported V2 unfinished
transactions retain their exact sealed owners, artifacts and evidence until closure. The separately negotiated `migration-ledger-owner-replacement-v1` contract may retain an exactly proved
defective owner as typed superseded evidence and select a freshly authorized forward candidate;
it never manufactures old activation readiness. Compatible retained rollback within the support
boundary remains protected. Refusing an old
upgrade never authorizes deleting its data. Only an explicitly confirmed full uninstall may
remove its owned data without migration; ordinary evidence cleanup still requires proven closure
and absence of required references. See
[Supported compatibility and recovery](resumable-release-transition-design.md#supported-compatibility-and-recovery).
