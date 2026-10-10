---
title: Legacy migration support and deprecation policy
status: accepted policy
updated: 2026-10-07
---

# Legacy migration support and deprecation policy

Legacy migration V1 uses a global layout, a schema 1 registry, applied state at
`migrations/state.json` and journals under `migrations/transactions/`. The retained
[registry](../../config/migrations.json) describes layouts 1–5: owner and route migration,
broker refresh, power runtime refresh and systemd compatibility repair. Current release
transitions use domain epochs and Ledger V2. Superseded `_migrate` mutation endpoints refuse
execution and direct legacy updaters to the supported standalone installer.

The supported released baselines cover distinct contracts:

| Baseline | Supported role |
| --- | --- |
| v0.4.1 | Closed import of the exact published owner rollback checkpoints and terminal journal; migration V1 layout 1→2 |
| v0.4.2 | Closed import of the exact published broker, route and owner rollback checkpoints and terminal journal; migration V1 layout 1→3 |
| v0.9.1 | Legacy migration V1 layout 1→5; actual updater verifies refusal of the superseded endpoint and upgrade through the standalone bridge |
| v0.11.2 | Actual caller for frozen process V1 and Ledger V2; update, interrupted resume, compatible rollback and forward retry |
| v0.17.3 | Actual activation-only journal producer and recovery baseline using Ledger V2 |
| v0.18.1 | Published caller using frozen process V1 and Ledger V2; update, interrupted resume, rollback, forward retry and checkpoint-reader bridge ordering |

The [closed importer](../../internal/migration/v1_import.go) binds the exact retained runtime
pair, registry, journal and observed postconditions. It does not admit arbitrary historical
journals. The [released compatibility check](../../dev/verify-release-upgrades.py) pins official
v0.9.1, v0.11.2, v0.17.3 and v0.18.1 runtime checksums and the legacy runtime installer. These binaries
remain acceptance inputs; rebuilding old versions from current source does not replace them.

Frozen process V1, recovery receipt V1 and legacy migration V1 have separate support policies.
Retiring the legacy migration engine does not retire the caller protocol or recovery receipts.
The current domain upgrade floor is epoch 1 for each domain in the
[release registry](../../config/release-transition.json). The legacy standalone-upgrade boundary,
domain floor and proven retained-runtime rollback eligibility are assessed separately.

Use [`yard update --check`](../cli-reference.md#update) for read-only assessment and follow the
reported action. Older updaters use the exact candidate version in both the installer URL and
arguments, as documented in [Legacy upgrades](../control-plane.md#legacy-upgrades). Unknown,
ambiguous or foreign state blocks before mutation; manual journal editing is unsupported.

Legacy direct-upgrade support is planned for retirement **no earlier than 2027-04-07**. The
notice window lasts at least six months after both an actual release announcement and a working,
published, checksum-pinned bridge are available. Later publication moves the retirement date.
This policy does not start that window. The candidate checkpoint bridge is prepared for acceptance
but remains unpublished; its release identity, official assets and checksums must be recorded
when publication occurs.

The bridge must read the supported legacy sources, reach verified canonical state and resume
after interruption. Preserve its runtime, installer, checksums, manifest and provenance after
legacy code leaves the latest runtime. The checkpoint writer uses the separately sealed optional
owner marker `config/release-checkpoint.json` for `migration-history-checkpoint-v1`, while retaining
the frozen Ledger V2 projection for older readers. New engines always honor an existing compact
checkpoint; retained owner assets without the marker do not enable initial conversion. Initial
conversion needs assessed authorization after verified readiness and preserves unfinished bindings.
The main registry and its two migration IDs remain unchanged. The compatible current-owner path
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
Published v0.18.1 has no sealed checkpoint-reader marker and does not start the bridge publication window.

Before retirement, publish the exact affected source formats, new readable floor, bridge path
and rollback limits. Warn when assessment actually encounters deprecated legacy state; ordinary
ready operation needs no deprecation warning. Every unfinished transaction retains its exact
sealed owner, artifacts and evidence until closure. Compatible retained rollback remains
protected. Calendar dates and release counts never authorize deletion of those pins or evidence;
cleanup requires proven closure and absence of required references. See
[Supported compatibility and recovery](resumable-release-transition-design.md#supported-compatibility-and-recovery).
