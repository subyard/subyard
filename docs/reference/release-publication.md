# Release preparation, acceptance and publication

A stable version is reserved only by final publication. Preparation or acceptance failures
create no `vMAJOR.MINOR.PATCH` ref, so a corrected commit can use the same version.

## Operator commands and retained state

Run from the managed development checkout with Python 3, authenticated GitHub CLI and the
[acceptance prerequisites](../testing.md#one-release-candidate). The dispatch workflows must exist
on the default branch and selected workflow ref; the source commit must contain the release tooling.
Choose worktree checks using the
[validation policy](../../.agents/skills/subyard-dev-flow/SKILL.md#choose-checks-by-risk).

```sh
python3 dev/release-acceptance.py preflight --slots 1 2 3
python3 dev/release-acceptance.py prepare --version X.Y.Z --output .build/release-local
dev/release.sh preflight --output .build/release-local
```

The publication preflight reads a frozen local collector, including pending checks, and measures
its full source manifest and existing public evidence. Repeat `--manifest FILE` for real v1
manifests of planned DIRECT source snapshots. It uploads and reads back a deterministic bounded
Git blob through the normal encoder/decoder, checking the selected repository's Contents write/read
access even for small projections. It creates no acceptance, tag, release or workflow dispatch and
does not alter the collector. Dispatch authorization and future evidence growth are not certified;
final publication still validates the complete actual attestation. Only sanitized public evidence
is transported. A failed preflight stops expensive dependent work.

Record the frozen source/build, full gate commands, slots and owners. Start ready local/VM gates,
including complete release smoke, in one terminal while preparation CI runs in another:

```sh
python3 dev/release-acceptance.py run --output .build/release-local --slots 1 2 3
# In a second terminal, against the same frozen public source:
dev/release.sh prepare --version X.Y.Z --commit FULL_SHA
```

After CI, compare actual binaries and consumed runtime inputs/modes and use
[reviewed evidence transfers](../testing.md#reviewed-evidence-transfers) for equivalent complete
original PASS results. Rerun affected or missing checks; `accept` reuses applicable imported results:

```sh
dev/release.sh accept --candidate rRUN_ID-aATTEMPT --slots 1 2 3
dev/release.sh publish --candidate rRUN_ID-aATTEMPT
```

`prepare` pins a full lowercase 40-character commit SHA and stable version. Release preparation
runs native ARM64, Paseo builds and host-free verification in parallel after input validation.
Host-free verification retains core/profile checks, CLI freshness and profile inventory. Final preparation waits
for all these same-attempt jobs before downloading Paseo assets, running
local adapters and the complete released-caller compatibility matrix. It builds final-version assets
once and uploads their source archive, exact inventory and SHA-256 identity manifest for 90 days.
`CANDIDATE` prints the attempt-specific ID and local directory. Publication never rebuilds these assets.

Recovery checks the exact same-attempt job inventory against the executed workflow file at the
native run's immutable `head_sha`. Only explicitly recorded workflow Git-blob identities are accepted:
the retained five-job policy and the current six-job policy. Unknown policies fail closed. Workflow
edits must register the new reviewed blob identity and retain historical policies needed for recovery;
the owning workflow contract checks this binding. Job count alone never selects a legacy policy.

The default `.build/release-publication/rRUN_ID-aATTEMPT/` directory holds:

- `prepared/{source.tar.gz,identity.json,assets/}` and a copied `identity.json`;
- `acceptance/{source/,candidate-bundle.tar.gz,receipt.json,events.jsonl,*.log}`;
- `attestation.json` and `publication.json` when generated.

Dispatch requests persist under `.build/release-publication/requests/`. Global `--state-dir DIR`
selects another root; `--repository OWNER/REPO` selects a fork or fixture. `--workflow-ref BRANCH`
selects the dispatch workflow independently of the source SHA. Preserve the whole candidate
directory when moving evidence between operator machines.

Dispatch state records UTC `observed_at` at the first terminal observation for an attempt,
`github_updated_at` from the API, and nullable `completed_at` only when the API supplies an actual
completion field. An update timestamp is not an exact completion time. `next_action_at` records
the first actual local step after successful dispatch: candidate download/validation or publication
verification. These are CLI boundaries, not coordinator action times. A new attempt clears prior
completion/action fields; resuming the same attempt preserves its first observation and handoff.
The CLI polls every 20 seconds while active. Sparse agent status reports do not delay its native
completion handling, but scheduling, network and unavailable completion metadata limit precision.

`accept` imports the successful preparation into the collector without repackaging. Local gates
precede fresh release smoke and every shipped-profile lane on leased disposable VMs. A repeat
runs only pending checks; SIGINT/SIGTERM drain workers and retain interrupted evidence.
Unavailable environments and missing required results stay incomplete.

Before allocating, use the [collector preflight](../testing.md#one-release-candidate) and native
VM status. Collector selectors can target the downloaded candidate's acceptance directory:

```sh
python3 dev/release-acceptance.py preflight \
  --output .build/release-publication/ID/acceptance --slots 1 2 3
```

Source, asset or tooling changes require a new candidate. Never amend a frozen tree, its policy
or its assets; the pinned candidate determines available tooling and validation rules.

## External checks

The frozen profile-owned `tests/e2e/external-obligations.json` manifests determine external gates.
Boolean `required` defaults to `true` when absent. The shipped GitHub App live API, Hermes provider,
OpenClaw staging gateway and QA broker checks declare `required: false`: they may remain `not-run`
without real credentials or external staging. No deferral is needed; `accept` exits zero once its
required gates pass. Acceptance and publication report skipped IDs as `external_optional_not_run`.

Local, core and all shipped-profile VM gates remain required, including OpenClaw's synthetic staging
safety/lifecycle fixture. Synthetic checks do not prove live API/provider behavior or deployed Convex
allocation, concurrency and exhaustion. Failed, unknown or malformed external results block
publication; skipped entries cannot carry execution evidence. After an actual same-candidate PASS,
record `status: passed` and its `evidence_sha256` in the receipt. Keep secrets out of receipts.

Missing required external results remain incomplete. For an explicitly authorized deferral:

```sh
python3 dev/release-acceptance.py defer-external \
  --output .build/release-publication/ID/acceptance \
  --obligation PROFILE:ID \
  --reason 'Public reason for postponing this external check' \
  --authorization-reference 'Public reference to the operator authorization'
```

Repeat `--obligation` for checks sharing a public reason and authorization. This candidate-bound
declaration preserves `not-run`; collector acceptance stays incomplete and exits nonzero.
Publication permits a valid deferral when all other requirements are met. Unknown IDs, blank
authorization, failed checks or changed bindings are refused. This cannot waive local or VM gates.

## Acceptance handoff and cross-machine serialization

`publish` verifies the frozen receipt, source, transport, complete inventories, hashes, commands,
logs and execution environments. It sends a sanitized attestation containing candidate/receipt
identities, check statuses and evidence hashes, public VM identities and any explicit decisions.
Raw receipts, logs, credentials and VM payloads stay local.

GitHub validates this statement by the trusted operator; it does not rerun local physical or external
checks. It separately verifies the successful preparation run/attempt, checks out the pinned commit,
verifies its source fingerprint and publishes its exact assets. Branch movement cannot change the
candidate. [Reviewed evidence transfers](../testing.md#reviewed-evidence-transfers) retain the actual tested
candidate and distinguish inherited coverage from current execution; affected scenarios need fresh checks.

Attestation v5 carries original completed-attempt evidence without repeating its full candidate
manifest for each check. It has exactly the v4 top-level fields plus `manifests`; the transfer,
operator acceptance and profile deferral inventories are always present (including empty lists).
`manifests` is keyed by SHA-256 of each canonical v1 manifest's exact bytes. Direct checks add
`original_attempt` (the complete v1 record) and `attempt_sha256`; their record references a manifest
by hash. Reviewed transfers v2 instead reference `tested_manifest_sha256` and
`target_manifest_sha256`, and retain the original record in `original_attempt`. Their other exact
fields are `schema_version`, `check`, `command`, `actual_command`, `origin` (`attempt`), `artifacts`,
`review`, `preparation_environment`, `execution_environment`, `environments`, `duration_seconds`
and `launched_at_utc`. `artifacts` has exactly `attempt`, `manifest`, `target_manifest`, `review`
and `log` digests. These refer to immutable attempt/manifest evidence rather than a mutable
source receipt. The v2 declaration and v1 sidecar fields are defined in the
[acceptance contract](../testing.md#reviewed-evidence-transfers).

All table entries must be referenced, all references must resolve, and every hash and exact-key
object must validate. A direct record must match the published check's candidate, command,
status, log and environment facts; a transferred record must match the reviewed original facts.
Extra, dangling or conflicting references fail before remote mutation. This is deduplication
inside one attestation, without an external artifact store. Legacy v1 declarations and attestation
versions 1–4 remain readable; mixed legacy evidence and v2 transfers can coexist in v5.
Package preparation and explicit publication exceptions retain their existing provenance and gates.
Batch preparation and atomic application use the same
[transfer command](../testing.md#batch-transfer-preparation-and-application). A batch contributes the
existing per-check v2 references and shared v5 manifest table; it introduces no additional wire
schema or publication exception. Legacy declarations and standalone evidence may share that batch.

Attestation encoding and schema selection are automatic. Canonical documents are bounded to
4 MiB and compressed blobs to 2 MiB. Small attestations use inline compressed envelopes; larger
ones use a same-repository Git-blob pointer with exact size, Git object ID and SHA-256 validation.
Identical bytes are restored on dispatch/resume because dangling blob retention is not guaranteed;
the canonical local attestation and durable request remain the evidence. Legacy plain inputs remain
readable. Complete dispatch inputs must fit GitHub's
[65,535-character limit](https://docs.github.com/en/actions/reference/workflows-and-actions/workflow-syntax#onworkflow_dispatchinputs).
Malformed, oversized or hash-mismatched inputs are rejected before remote mutation.

All publication runs share one repository-wide concurrency group with `cancel-in-progress: false`
and `queue: max`, preserving queued retries across operator machines and versions. Local locks
protect local state only. Neither workflow reacts to tag pushes.

## Explicit publication exceptions

An operator may accept a known terminal failure of one exact runnable profile check for one
candidate. This publication-only decision preserves the failed collector result and requires
a public reason, authorization and repair reference:

```sh
dev/release.sh publish --candidate rRUN_ID-aATTEMPT \
  --operator-acceptance /path/to/exact-candidate-decision.json
```

The declaration has exactly these fields:

| Field | Required value |
| --- | --- |
| `schema_version` | Integer `1` |
| `check` | Exact runnable `profile:NAME` or `profile:NAME/LANE` from the frozen inventory |
| `target` | Complete target preparation `identity.json` |
| `tested` | Original receipt's complete identity from `release_acceptance.transfer_candidate` |
| `command` | Exact normalized declared command from `release_acceptance.command_identity` |
| `decision` | Exactly `reason`, `authorization_reference`, `repair_reference`; nonblank public English strings without credentials or private paths |
| `source` | Exactly local frozen acceptance `output` path and original `receipt_sha256` |
| `artifacts` | Exactly `native`, `launch`, `timing`, `log`; each with local `path` and original `sha256` |

Paths resolve relative to the declaration. Repeat `--operator-acceptance` for separately authorized
checks. Keep the original frozen source, runtime, bundle and full asset inventory. Original and target
commands and versions must match. Evidence must prove terminal nonzero exit, complete launch/timing,
VM identities, successful cleanup of every guest and lease release. Interrupted, incomplete, changed
or ambiguous evidence is refused. Only pending/uncollected or matching failed entries qualify;
passing or inherited entries cannot use this path. Publication reports `native_status=failed` and
the original exit and log hash without claiming a PASS.

For an explicitly authorized runnable profile check that has never run:

```sh
dev/release.sh publish --candidate rRUN_ID-aATTEMPT \
  --defer-profile profile:NAME \
  --deferral-reason 'Known profile issue; postpone this check for this release' \
  --deferral-authorization-reference 'Public reference to operator authorization'
```

Repeat `--defer-profile` for exact IDs; lane profiles require `profile:NAME/LANE`. Only pristine
pending entries without logs or attempt events qualify. Receipts stay unchanged and collector
acceptance remains incomplete; publication reports `not-run` and `operator-deferred`.
Both exception paths bind the decision to the exact candidate, command and original receipt.
Other gates, including CI, packaging, core smoke and other profiles, retain their requirements.
Retry with the same decision and receipt.

## Publication and recovery

Publication validates evidence before creating an annotated stable tag bound to the candidate ID,
identity manifest hash and source commit. It creates an owned draft, uploads missing assets, verifies
the complete remote inventory and downloaded bytes, publishes, then verifies the immutable release,
ref and assets again. Installer/updater discovery sees only the completed published release.

Repeat the same command after interruption or a lost response. Active workflows are watched;
failed or cancelled ones rerun with their original inputs and workflow SHA. A matching owned ref
or draft resumes publication. Uploaded bytes must match. A zero-byte `starter` asset from an HTTP 502
upload failure may be deleted and retried only on that owned unpublished draft. Foreign state,
unexpected assets and conflicting bytes are never overwritten. A matching published immutable
release is verified and returned as success.

If dispatch/run discovery was lost, the persisted request UUID locates the original run. Discovery
timeout leaves outcome unknown without a second dispatch. Retry once the run becomes visible.
If it never reached GitHub, inspect the Actions inventory before explicitly retiring the local
request and issuing another; unknown outcome is never PASS.

Once a stable ref exists, its version belongs to that candidate even after failure: retry that
candidate. The tooling never moves/deletes stable refs or replaces foreign/published releases.
Occupied legacy tags need a separate operator decision; otherwise use a free version.

Immutable releases must already be enabled. A mutable published release or expired preparation
artifact blocks success; preserve state for investigation. Never roll back a published release,
rebuild accepted bytes or substitute another preparation run.

A candidate whose workflows differ from the default branch may hit GitHub's `Workflows:write`
restriction. `GITHUB_TOKEN` cannot grant it. Resolve the permission failure through separately
authorized credentials/workflow policy while retaining the pinned SHA and unfinished state;
never substitute current branch HEAD.

## Development verification

`TMPDIR=/tmp bash tests/release-acceptance.sh` and
`TMPDIR=/tmp bash tests/release-publication.sh` check candidate/evidence binding, optional external
results, explicit decisions, transport validation and publication recovery with synthetic fixtures.
They do not dispatch workflows, publish releases or establish physical/external acceptance.

GitHub references: [concurrency](https://docs.github.com/en/actions/concepts/workflows-and-actions/concurrency),
[release API](https://docs.github.com/en/rest/releases/releases),
[asset upload/recovery](https://docs.github.com/en/rest/releases/assets#upload-a-release-asset),
and [immutable release preparation](https://docs.github.com/en/repositories/releasing-projects-on-github/managing-releases-in-a-repository).
