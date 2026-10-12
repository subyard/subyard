# Release preparation, acceptance and publication

A stable version is reserved only by the final publication operation. Preparation and
acceptance failures create no `vMAJOR.MINOR.PATCH` ref and permit a corrected commit to use
the same version. Existing releases, installer names, manifests, provenance and updater
verification remain unchanged.

## Operator commands and retained state

Run from the managed development checkout with Python 3, GitHub CLI authentication and
the ordinary acceptance prerequisites. The dispatch workflows must already be on the
repository's default branch. The source commit must contain the release tooling.

```sh
dev/release.sh prepare --version X.Y.Z --commit FULL_SHA
dev/release.sh accept --candidate rRUN_ID-aATTEMPT --slots 1 2 3
dev/release.sh publish --candidate rRUN_ID-aATTEMPT
```

`prepare` requires a full lowercase 40-character commit SHA and a stable version. It
starts Release preparation through `workflow_dispatch`, with that SHA pinned in every
checkout. It keeps native ARM64, both native Paseo builds, full core/profile host-free
verification, CLI freshness, complete profile acceptance inventory, local adapters and
the complete released-caller compatibility matrix. Final-version runtime files are built
once per preparation attempt; publication never renames or rebuilds them. A successful
run uploads an immutable attempt-specific artifact containing the public source archive,
complete exact release inventory and SHA-256 identity manifest. The artifact is retained
for 90 days. `CANDIDATE` prints its ID and local directory; no stable tag is created.

The default directory `.build/release-publication/rRUN_ID-aATTEMPT/` contains
`prepared/{source.tar.gz,identity.json,assets/}`, a copied `identity.json`,
`acceptance/{source/,candidate-bundle.tar.gz,receipt.json,events.jsonl,*.log}` and, once
complete, `attestation.json` and `publication.json`. Dispatch requests are durable under
`.build/release-publication/requests/`. `--state-dir DIR` before the subcommand selects a
separate local root; `--workflow-ref BRANCH` selects the branch containing the workflow,
without changing the source SHA. `--repository OWNER/REPO` supports repository fixtures
or a fork containing the same workflows. Preserve the whole candidate directory when
moving accepted evidence to another operator machine.

`accept` downloads the exact successful run/attempt artifact if it is absent locally,
then imports its frozen sources and assets into the existing acceptance collector without
packaging again. It retains the original CI provenance SHA, complete release hashes,
source fingerprint, runtime identity, command/log bindings, execution environment and
all profile inventories. The local gates precede fresh release smoke and every shipped
profile lane on leased disposable VMs. The managed invoking checkout still supplies lease
attribution. A repeat runs only pending checks; workers and transport use the existing
collector contracts. SIGINT/SIGTERM drain owned workers and retain interrupted evidence.
An unavailable environment or missing result stays incomplete.

Before allocating, use the [collector preflight](../testing.md#one-release-candidate) and
native VM status to choose capacity. For a downloaded candidate, use
`python3 dev/release-acceptance.py preflight --output .build/release-publication/ID/acceptance
--slots 1 2 3`. The ordinary collector `run --only CHECK`, `--first CHECK` and other
selectors can target this same acceptance directory. Source/artifact changes require a
new preparation candidate. Do not amend a frozen tree or alter its assets.

External-service obligations remain required. As in the existing collector, after an
actual authorized external check passes on this same candidate, record `status: passed`
and `evidence_sha256` in that obligation's receipt entry. This hash identifies retained
actual external evidence; synthetic VM credentials do not establish external acceptance.
Keep credentials and private payloads out of receipts. An all-green reproducible subset
with external results missing remains incomplete and cannot publish.

When the operator explicitly authorizes deferring named external obligations for this release,
record each exact declared ID against the already-frozen candidate:

```sh
python3 dev/release-acceptance.py defer-external \
  --output .build/release-publication/ID/acceptance \
  --obligation PROFILE:ID \
  --reason 'Public reason for postponing this external check' \
  --authorization-reference 'Public reference to the operator authorization'
```

Repeat `--obligation` for checks covered by the same reason and authorization. Keep both strings
public and free of credentials or private details. The declaration is bound to the exact preparation
identity (including candidate ID, pinned commit, source and all asset hashes). Its receipt entry stays
`status: not-run`, with a separate `deferral` containing the reason, authorization reference and
candidate identity hash. No actual evidence hash or PASS is invented. The complete `result` remains
`incomplete`; the collector and `accept` keep their nonzero exit status. A valid deferral permits
`publish` only when `reproducible_result` is `passed` and every other external obligation has actual
passing evidence or its own valid authorization. Publication prints the deferred IDs and `execution=not-run`.

There are no default deferrals. Unknown IDs, missing/blank reason or authorization, failed actual
checks, changed declarations and candidate bindings reject this path. Local, physical, core and
shipped-profile checks remain required with unchanged evidence. Existing candidates prepared before
this tooling change cannot use the exception: publication checks out the pinned candidate code,
so commit the change and prepare a new candidate before using it. Never edit an older frozen source.

## Acceptance handoff and cross-machine serialization

`publish` loads and verifies the frozen collector receipt, source, transport, asset
inventory, hashes and passing logs before dispatching Release publication. It requires
all declared checks and external obligations (actual passes or authorized deferrals), matching command provenance and local
execution environments. It sends only a sanitized attestation: candidate and identity
hash, receipt hash, complete expected check IDs/statuses/log hashes, public execution and
VM base identities, and external evidence hashes or the bound public deferral declarations.
The receipt hash includes those declarations. No raw acceptance receipt, controller
logs, credentials or VM payloads are uploaded to GitHub.

This attestation is a statement by the authenticated, trusted release operator. GitHub
validates its complete inventory, statuses and candidate binding; GitHub does not
independently execute or inspect local physical/external checks. Native CI evidence is
verified separately using the exact successful preparation run and attempt. The worker
checks out the artifact's commit, verifies its public source fingerprint and publishes
its exact assets. Branch movement cannot change the candidate.

Explicit [reviewed evidence transfers](../testing.md#one-release-candidate) retain the
candidate that was actually tested and distinguish inherited coverage from current execution.
An attestation containing transfers uses schema version 2 with an exact `reviewed_transfers`
check-ID list and complete sanitized per-check transfer provenance. Remote validation checks
that inventory, the target preparation identity, original hashes, declared/actual command,
environment provenance and nonblank scope/reason/authorization. Ordinary attestations retain
strict schema version 1; transfer fields cannot be silently downgraded to that schema.
The trusted operator owns the reviewed unchanged-scope decision. A transfer does not waive
new or affected scenarios, invent an old collector PASS or replace current preparation.
The pinned publication candidate must contain this tooling before using schema version 2;
never amend an older frozen snapshot to add support.

All publication workflow runs use the same repository-wide stable concurrency group,
`cancel-in-progress: false` and `queue: max`. This serializes different versions as well
as same-version retries across operator machines, and preserves queued requests. Local
locks protect local state only. Neither workflow reacts to tag pushes, so creating a tag
with a GitHub token, App token or PAT cannot start a second release pipeline.

## Publication and recovery

Publication checks evidence before creating an annotated stable tag whose message binds
the candidate ID, identity manifest hash and exact source commit. It creates an owned
draft, uploads missing assets, verifies the complete remote inventory and downloaded
bytes, publishes, then confirms the actual release is immutable and verifies its ref and
assets again. Installer/updater discovery sees the completed published release.

Repeat the same command after a lost response or interruption. An unfinished dispatched
workflow is watched; a failed or cancelled one is rerun with its original inputs and
workflow SHA. A matching annotated ref left before draft creation is adopted. An owned
matching draft resumes missing uploads; already uploaded bytes must match. A zero-byte
`starter` asset left by GitHub's documented HTTP 502 upload failure may be deleted and
retried only on that owned unpublished draft. Conflicting uploaded bytes, unexpected
assets or foreign state stop publication and are never overwritten. A published matching
immutable release is verified and returned as success after response loss.

If dispatch succeeded but its response/run discovery was lost, the stored request UUID
is used to find the original run. Discovery timeout leaves outcome unknown, with no
second dispatch. Repeat after GitHub makes the run visible. If the original request
never reached GitHub, inspect the Actions run inventory before explicitly retiring that
local request file and issuing a new request; unknown outcome is never reported as PASS.

After a stable ref exists, its version is bound to this candidate even if later steps
fail. Retry that candidate. The tooling never moves or deletes stable refs and never
replaces foreign or published releases. Occupied legacy tags, including failed old
publication runs, refuse adoption even if they point at the same commit. Their resolution
requires a separate explicit operator decision; use a free version in this workflow.

GitHub immutable releases must already be enabled. The tooling does not change that
setting and refuses success if the actual published release is mutable. Preserve its
state for investigation; never roll back a published release. Artifact expiration also
blocks publication: do not rebuild accepted bytes or substitute another run.

A historical candidate whose `.github/workflows` differs from the default branch can
hit GitHub's additional `Workflows:write` restriction when creating a release/ref.
`GITHUB_TOKEN` cannot grant that permission. The operation fails safely and keeps the
exact pinned SHA and unfinished state. Inspect the permission failure and resolve it
through separately authorized repository credentials/workflow policy; never change the
candidate to current branch HEAD as a workaround.

## Development verification

`TMPDIR=/tmp bash tests/release-publication.sh` exercises exact imports, absent/changed
acceptance, candidate/provenance/inventory binding, foreign refs/drafts, partial upload,
HTTP 502 starter recovery, response loss at ref/draft/upload/publication, idempotent
retry and the native workflow serialization contract using an in-memory GitHub fixture.
The existing collector, workflow gates and packaging contracts cover their own preserved
behavior. These tests perform no real workflow dispatch, release publication or VM work;
actual GitHub runner behavior, immutable publication and full runtime/profile/external
acceptance remain checks of the next separately authorized release.

GitHub references: [concurrency](https://docs.github.com/en/actions/concepts/workflows-and-actions/concurrency),
[release API](https://docs.github.com/en/rest/releases/releases),
[asset upload/recovery](https://docs.github.com/en/rest/releases/assets#upload-a-release-asset),
and [immutable release preparation](https://docs.github.com/en/repositories/releasing-projects-on-github/managing-releases-in-a-repository).
