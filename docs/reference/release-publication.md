# Release preparation, acceptance and publication

A stable version is reserved only by final publication. Preparation or acceptance failures
create no `vMAJOR.MINOR.PATCH` ref, so a corrected commit can use the same version.

## Operator commands and retained state

Run from the managed development checkout with Python 3, authenticated GitHub CLI and the
[acceptance prerequisites](#one-release-candidate). The dispatch workflows must exist
on the default branch; the source commit must contain the release tooling. A
[compatible workflow ref](#workflow-ref-selection) must have the candidate as its head and run its
own release workflows. For a candidate behind the default branch, push a release branch at that
commit before preparation.
Choose worktree checks using the
[validation policy](../../.agents/skills/subyard-dev-flow/SKILL.md#choose-checks-by-risk).

```sh
python3 dev/release-acceptance.py preflight
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

Record the frozen source/build, check commands and owners. Start ready local checks in one
terminal while preparation CI runs in another:

```sh
python3 dev/release-acceptance.py run --output .build/release-local
# In a second terminal, against the same frozen public source:
dev/release.sh prepare --version X.Y.Z --commit FULL_SHA
```

After CI, compare actual binaries and consumed runtime inputs/modes and use
[reviewed evidence transfers](#reviewed-evidence-transfers) for equivalent complete
original PASS results. Rerun affected or missing checks; `accept` reuses applicable imported results:

```sh
dev/release.sh accept --candidate rRUN_ID-aATTEMPT
dev/release.sh publish --candidate rRUN_ID-aATTEMPT
```

For a CI-prepared candidate, `verify`, ShellCheck, local adapters and released-caller compatibility
are counted from the successful preparation jobs and not repeated; `release-acceptance.py run` with
`--only NAME --rerun` replaces a counted result with a local run. A locally prepared candidate runs them.

After downloading the preparation artifact, `prepare` also dispatches `release-vm.yml` asynchronously
for the 25 exact IDs in `CI_HOSTED_CHECKS`. The workflow needs a branch whose head is the candidate
commit and a reviewed workflow blob in `CI_HOSTED_POLICIES`; otherwise it reports the refusal and
leaves those checks open. It downloads the exact preparation run/attempt artifact, uses the shared
hosted pool setup, and runs one fresh collector selection per job (12 jobs at once, each bounded to
350 minutes). Repeating preparation discovers the durable request instead of dispatching another.
`accept` waits up to six hours for that request, then collects only completed successful jobs before
running the remaining requested checks. `--ci-wait-seconds 0` collects the current snapshot immediately;
a nonzero override is bounded to 86400 seconds. Failed, running, missing or refused results stay open
for local execution when requested. After a local check fails, `accept` neither waits nor runs hosted
checks locally: it counts finished hosted results and leaves the rest pending for a later `accept`. Collection prints `CI-HOSTED CHECK run=RUN job=JOB` or
`CI-HOSTED-SKIP CHECK: REASON`.

Hosted evidence is `execution: ci-hosted`, with the original runner environment, manifest, complete
PASS attempt and recorded cleanup. Candidate ID, commit, source fingerprint, version, runtime and
all release assets must match exactly. Independently generated transport gzip checksums may differ;
the original checksum remains in its manifest. Collection imports bounded evidence files, never source
or release assets. Publication validates that evidence again and rechecks the GitHub run/attempt,
job success, exact head SHA and reviewed workflow blob, and prints `PUBLICATION ci_hosted=...`.
Periodic E2E results remain background evidence and cannot count through this path.

Failed, timed-out or cancelled hosted checks have a separate seven-day
`hosted-diagnostics-JOB_INDEX-ATTEMPT` artifact on the Release VM run page, with bounded sanitized
check logs, events, receipt/attempt/invocation records, cleanup output and environment summary.
The job log also prints a bounded sanitized failure tail. These snapshots never become `ci-hosted`
evidence; only a fully validated PASS with recorded cleanup produces that artifact.
See [hosted check diagnostics](#exact-candidate-hosted-release-checks) for limits,
Periodic E2E artifact names and interruption caveats.

### Release levels

| Level | When | Checks | Typical time |
| --- | --- | --- | --- |
| Continuous | Every push and on a schedule, outside a release | `verify` and the GitHub-hosted matrix and lanes | Background |
| Ordinary release or hotfix | No product migration or update-contract change since the previous release | CI preparation counted by `accept`; `p0-release-smoke` on the final bytes; the released-caller matrix; eligible hosted VM results counted; remaining VM checks stay `not-run` unless requested | About 2 hours |
| Full release | Product migration or update-contract changes, or an operator request | Ordinary checks plus every VM profile check and every core VM check (`vm:<id>` from `dev/e2e/release-vm-checks.json`) | Up to 8 hours |

`accept --level ordinary|full [--level-reason TEXT]` records the level, its reason, the baseline tag and
the changed migration paths as `release_level` in the receipt; later runs keep the recorded level unless
`--level` or `--level-reason` is given, and `publish` refuses a candidate without one and prints it.
Candidates whose pinned code predates release levels publish without one.
The baseline is the highest local `vX.Y.Z` tag below the candidate version that the candidate commit
contains. Changed product paths use the transfer rule in
[reviewed evidence transfers](#reviewed-evidence-transfers). Ordinary is refused while
such paths exist; when the baseline or diff is unavailable locally it needs an explicit
`--level-reason`, and a recorded change list still applies. Full requires a reason unless migration
changes supply it, runs every VM check, and its attestation refuses an untouched `not-run` VM profile
or core VM check; only an explicit `--defer-profile` operator deferral can leave a profile check unrun, and publication prints
it. The level is a local receipt record: the publication workflow does not recheck it. Before a full release, fix a red continuous
run before freezing the candidate. Optional unsupported-source VM checks listed in
[Periodic E2E](../testing.md#periodic-e2e-on-hosted-runners) are excluded from this requirement
and never gate a release or PR.
Local long VM smoke and shipped-profile VM checks run on explicit request:

```sh
dev/release.sh accept --candidate rRUN_ID-aATTEMPT --with-vms --slots 1 2 3
# Or select one check from an existing frozen collector:
python3 dev/release-acceptance.py run --output .build/release-local \
  --only p0-release-smoke --slots 1
```

`--slots` alone does not request local VM execution. Checks without counted hosted evidence or an
explicit local request remain `not-run` in acceptance and publication; they are not passing physical evidence. Once requested, incomplete or failed
checks block the candidate, including after interruption or a later default invocation.
A catalog check's `requires` (the split lanes of `vm:p0-full`) must also pass once that check is requested
or recorded, imported or transferred; acceptance stays incomplete and publication refuses otherwise,
including under failure acceptance or profile deferral. `run --only vm:p0-full` runs the full P0:
`vm:p0-full --split-covered` plus its split checks that have not passed; `accept --only` counts their
hosted results first.

`prepare` pins a full lowercase 40-character commit SHA and stable version. Release preparation
runs native ARM64 and host-free verification in parallel after input validation.
Host-free verification retains core/profile checks, CLI freshness and profile inventory. Final preparation waits
for these same-attempt jobs before running
local adapters and the complete released-caller compatibility matrix. It builds final-version assets
once and uploads their source archive, exact inventory and SHA-256 identity manifest for 90 days.
While waiting under `subyard-github run`, the CLI refreshes an expired broker token after HTTP 401 and retries the failed API call or download once.
`CANDIDATE` prints the attempt-specific ID and local directory. Before printing it, `prepare` also creates
that directory's `acceptance/` output from the downloaded assets: it imports the `package` result,
records `shellcheck` and `verify` as passed by `host-free-verify` and `local-adapters` and `compatibility`
as passed by `prepare` (`execution: ci-preparation`), leaves every other check pending or optional
`not-run`, and dispatches the eligible hosted VM checks described above. It counts them only when the candidate's own pinned code declares
`CI_PREPARED_CHECKS` and its `release.yml` blob is listed in `CI_COUNTED_POLICIES`, which every reviewed
`release.yml` change must update; older candidates run them locally, so their own publication
validator still accepts the attestation. Publication rechecks the run's policy and prints counted
results as `ci_counted`. Counted results are never a transfer source for another candidate. Evidence can be transferred into it
before the first `accept`. Before running checks, `accept` reuses profile PASS results from earlier
local candidates of the same version when the runtime and release assets are identical and the source
differs only in other profiles' unshared test files; provenance may differ in its timestamp and, between
two concrete commits, in `sourceRevision`. Publication shows them as inherited evidence transfers. A
`REUSE-SKIP` line names why an earlier PASS was not reused. Publication never rebuilds these assets.
The separate `policy:harness-only-v1` also permits profile/VM reuse for release orchestration,
CI and documentation changes, with identical runtime/assets and the existing migration guards;
the collector `dev/release-acceptance.py`, local gates and continuous release smoke are excluded.
For reviewed changes, `prepare-reviews --output NEW --from OLD --request REQUEST.json` prepares
unsigned batch drafts without transferring evidence; edit the drafts and bind their paths and
edited hashes as described in [batch transfer preparation](#batch-transfer-preparation-and-application).

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
selects another root; `--repository OWNER/REPO` selects a fork or fixture. Preserve the whole
candidate directory when moving evidence between operator machines.

### Workflow ref selection

The dispatched workflow ref supplies the workflow definitions, preparation source and publication
fetch tooling. `prepare` and `publish` accept only branches whose head is the exact candidate commit.
The default branch is preferred when its head equals the candidate; otherwise candidate-headed
branches are tried in name order. Identical workflow files on a different commit do not qualify:
candidate code must run in its own branch's cache scope. Preparation checks out the workflow SHA
and verifies that both `github.sha` and checkout HEAD equal `inputs.commit` before executing source
code; Go caching remains disabled. A ref is compatible only when its
`.github/workflows/release.yml`, `.github/workflows/release-publish.yml`
and `dev/release-assets.sh` blobs equal the candidate's and the candidate's `release.yml` blob is a
registered preparation policy. An explicit `--workflow-ref BRANCH` must name a branch and gets the same check.
`prepare` also refuses a new request when tag `vX.Y.Z` already exists, naming the owning candidate
when the tag belongs to this commit. Refusals list every examined ref and happen before dispatch.

When no candidate-headed ref exists, including a release of a non-head commit whose workflow files
still match the default branch, push a release branch first. The refusal names the exact release
branch and the commands that create it at the candidate and publish it. The tool never creates or
pushes a branch itself. Then repeat the
same `prepare` or `publish` command (with `--workflow-ref` of the new branch if a ref was given):

```sh
git branch release/X.Y.Z-SHORT12 FULL_SHA
git push origin release/X.Y.Z-SHORT12
dev/release.sh prepare --version X.Y.Z --commit FULL_SHA
```

Before its first dispatch, a retained request rechecks its recorded ref. If that branch moved, push
the suggested release branch and repeat with `--workflow-ref BRANCH`. The new candidate-headed ref
is validated and recorded under the same request identity; no request file needs to be deleted.
This also recovers publication requests retained after attestation encoding fails before dispatch.
Once dispatch intent has been recorded, the request stays pinned: repeating the same command
discovers or reruns the original workflow attempt at its original SHA even after branches move.
A different explicit `--workflow-ref` is then refused with the recorded ref's name and dispatches nothing.

Under `subyard-github run`, the broker's token belongs to the App installation of the repository it
selected, which may come from the working directory's `origin`. Before any GitHub call, local
`preflight`, `prepare`, `accept`, and `publish` refuse unless the broker selected the release
repository, and name the exact `subyard-github run --repo OWNER/REPO -- dev/release.sh ...` rerun.

Dispatch state records UTC `observed_at` at the first terminal observation for an attempt,
`github_updated_at` from the API, and nullable `completed_at` only when the API supplies an actual
completion field. An update timestamp is not an exact completion time. `next_action_at` records
the first actual local step after successful dispatch: candidate download/validation or publication
verification. These are CLI boundaries, not coordinator action times. A new attempt clears prior
completion/action fields; resuming the same attempt preserves its first observation and handoff.
The CLI polls every 20 seconds while active. Sparse agent status reports do not delay its native
completion handling, but scheduling, network and unavailable completion metadata limit precision.

`accept` imports the successful preparation into the collector without repackaging. Local gates
precede any explicitly requested checks on leased disposable VMs. A repeat
runs only pending checks; SIGINT/SIGTERM drain workers and retain interrupted evidence.
Unavailable environments and missing required results stay incomplete.

Before allocating, use the [collector preflight](#one-release-candidate) and native
VM status. Collector selectors can target the downloaded candidate's acceptance directory:

```sh
python3 dev/release-acceptance.py preflight \
  --output .build/release-publication/ID/acceptance --with-vms --slots 1 2 3
```

Source, asset or tooling changes require a new candidate. Never amend a frozen tree, its policy
or its assets; the pinned candidate determines available tooling and validation rules.

## External checks

The frozen profile-owned `tests/e2e/external-obligations.json` manifests determine external gates.
Boolean `required` defaults to `true` when absent. The shipped GitHub App live API, Hermes provider,
OpenClaw staging gateway and QA broker checks declare `required: false`: they may remain `not-run`
without real credentials or external staging. No deferral is needed; `accept` exits zero once its
required gates pass. Acceptance and publication report skipped IDs as `external_optional_not_run`.

Local/core and shipped-profile host-free checks remain required. VM checks, including OpenClaw's
synthetic staging safety/lifecycle fixture, run only when requested. Synthetic checks do not prove
live API/provider behavior or deployed Convex
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
candidate. [Reviewed evidence transfers](#reviewed-evidence-transfers) retain the actual tested
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
[acceptance contract](#reviewed-evidence-transfers).

All table entries must be referenced, all references must resolve, and every hash and exact-key
object must validate. A direct record must match the published check's candidate, command,
status, log and environment facts; a transferred record must match the reviewed original facts.
Extra, dangling or conflicting references fail before remote mutation. This is deduplication
inside one attestation, without an external artifact store. Legacy v1 declarations and attestation
versions 1–4 remain readable; mixed legacy evidence and v2 transfers can coexist in v5.

Attestation v6 is v5 with each distinct manifest `source_entries` stored once. It adds
`manifest_sources`, keyed by SHA-256 of the canonical inventory; each `manifests` entry has exactly
`schema_version`, `candidate`, `environment` and `source_entries_sha256`. The validator rebuilds each
v1 manifest, rechecks its table hash and applies the v5 rules; every source must be referenced, and
the rebuilt manifests are bounded to 128 MiB. Remote publication validates with the candidate
commit's own publisher, so the schema follows the candidate source, not the local checkout or the
size: v6 for every manifest-carrying attestation of a candidate whose publisher supports it, v5 for
earlier candidates. An earlier candidate whose v5 attestation exceeds the limits, such as one with
many hosted checks, cannot be published; prepare a new candidate that contains v6 support.
A publication state recorded for the equivalent v5 form keeps that form once dispatched; before
dispatch it is rebound to v6 and the replaced hash is printed.
Package preparation and explicit publication exceptions retain their existing provenance and gates.
Batch preparation and atomic application use the same
[transfer command](#batch-transfer-preparation-and-application). A batch contributes the
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
timeout leaves outcome unknown without a second dispatch; repeat the same command once the run
becomes visible. Unknown outcome is never PASS. There is no retirement command: only after the
Actions inventory shows no run titled `Subyard REQUEST_UUID` may the operator remove that exact
request file (`requests/X.Y.Z-FULL_SHA.json` or the candidate's `publication.json`) and repeat the command.

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

## Agent release workflow

Agents preparing a release apply the
[development workflow](../../.agents/skills/subyard-dev-flow/SKILL.md) together with these rules.

- Runtime releases default to the existing host-free/CI checks, local adapters and full
  released-caller compatibility check in [Development](../development.md).
  Long VM smoke and shipped-profile VM checks run only on explicit operator request:
  `accept --with-vms --slots N` or collector `run --only CHECK --slots N`.
  Available slots alone do not request a VM run. Core `p0-release-smoke` is mandatory:
  local-only acceptance remains incomplete until it passes, and it cannot be excluded
  or waived as optional. Unrequested shipped-profile VM checks remain `not-run`,
  never passed; requested incomplete or failed checks block acceptance/publication.
  Optional external checks follow [external checks](#external-checks).
  Existing CI/merge checks and risk-based development validation are unchanged.
- Release repairs land as reviewed PRs to the release base. Before preparation
  CI, run the [publication preflight](#operator-commands-and-retained-state)
  and assign every required local check and explicitly requested VM gate in the existing task plan:
  frozen local candidate, full command, slot budget and runner owner. Build locally and start ready
  requested gates alongside CI; a targeted lane does not complete full release smoke. Do not
  allocate VMs without an explicit request or wait for CI artifacts to begin
  checks that can use the local build. After CI, compare binaries and consumed runtime inputs/modes,
  then transfer equivalent complete original PASS evidence through the reviewed workflow.
  Rerun only affected or missing gates.
- A release at the default branch head needs no extra branch. Every non-head candidate, including a
  hotfix whose workflow files still match the default branch, needs a candidate-headed branch pushed
  before preparation. A hotfix gets a release branch that the agent creates and pushes from
  the hotfix base; release-task PRs target it. At the candidate commit the agent creates and
  pushes the branch named by the refusal (`git branch release/X.Y.Z-SHORT12 FULL_SHA`, then
  `git push origin release/X.Y.Z-SHORT12`), then repeats `prepare` without `--workflow-ref`. Any
  pushed branch whose head is the candidate is selected automatically, as described in
  [workflow ref selection](#workflow-ref-selection). As soon as the release branch has the fixes,
  the agent opens a PR returning them to the default branch and asks for the operator's OK once
  its CI checks pass; the release is complete only after it merges. Publication, attestation and
  their confirmations stay with the operator.

- After publication, open a PR to the default branch pinning the new release's verified installer
  and archive SHA-256 values as a `direct` row in
  `dev/e2e/published-upgrade-baselines.tsv`. Add its all-agent published-upgrade check to
  `dev/e2e/release-vm-checks.json` and `CI_HOSTED_CHECKS` in `dev/release-acceptance.py`, so
  the next release accepts a direct upgrade from the previous published release.

### One release candidate

The same profile inventory is used by local acceptance and the branch CI inventory step
(`dev/test-profiles.sh --e2e --list`).
Check prerequisites before packaging or acquiring a VM, then freeze the final candidate:

```sh
python3 dev/release-acceptance.py preflight
python3 dev/release-acceptance.py prepare --version X.Y.Z --output .build/release-acceptance
python3 dev/release-acceptance.py run --output .build/release-acceptance
```

Add `--with-vms --slots 1 2 3` to `preflight`/`run` to request all VM smoke, core and profile checks,
or use `run --only p0-release-smoke --slots 1` for one check. `--slots` alone does not
request VM execution. Core VM checks are the lanes in `dev/e2e/release-vm-checks.json`
(full P0 and its split lanes, uninstall, published-baseline upgrades and other release lanes), recorded as `vm:<id>`
and run by `dev/e2e/release-vm-check.sh ID --slot N` with the frozen candidate; `--only vm:published-upgrade`
selects one family. A catalog entry's optional `requires` lists sibling checks that must also pass
whenever that check is requested or recorded; preparation records them in the receipt and the
candidate's catalog must still match. Selecting a check also runs its required checks that have not
passed for the candidate, so `run --only vm:p0-full --slots 1 2 3` runs the full P0 outside a release:
`vm:p0-full --split-covered` plus its split checks; `--exclude` keeps a required check not run and the
result incomplete. A candidate whose pinned source has no catalog has no `vm:` checks. One worker
runs per listed slot. After a check fails inside a recorded lease whose cleanup passed, that slot takes
the next check; a refusal, interruption, timeout, cleanup failure or attempt without a lease stops the slot,
so an unusable slot cannot turn later checks into failures. One collector owns a candidate's receipt
at a time; run every check through it rather than separately, so the result is recorded. Once explicitly requested, a pending VM check remains required on
continuation; interruption or exclusion cannot turn it back into optional passing acceptance.

Preflight reads the worktree or `--output` frozen candidate. It never authenticates or allocates;
`--broker-status FILE` accepts an already-collected public status, and `--external-readiness FILE`
accepts obligation IDs mapped to `available`, `unavailable` or `unknown`. Readiness declarations
are planning inputs, not passing evidence. `--slots` and `--types` identify the contemplated requests;
they do not reserve capacity or change controller dispatch. A compact `CAPACITY` line on stderr
summarizes these selections, numeric headroom, observation time/age and prerequisites; stdout remains
the JSON report. Missing observation time stays unknown, independent of file read time or mtime.
Missing telemetry and legacy reservations leave headroom unknown. Observed unavailable selected
slots or exhausted headroom report planning `insufficient` and exit 1 before packaging or local checks.
Positive headroom does not prove the selected types fit: compare it with their actual resource contract
and preserve the reported reserves. Run this worktree preflight before `prepare` and the local gates
to avoid an obviously unsuitable attempt. The supplied snapshot is advisory; current native broker
admission remains authoritative. Use `run --first CHECK` to prioritize the original
regression after required local gates. Remaining live checks run longest-first using successful
durations in the current receipt, then earlier candidate receipts already found by release publication.
Unknown durations and ties retain declaration order. Explicit `--exclude CHECK` leaves the required gate incomplete.

Select available slots using `dev/agent-e2e.sh --status`; each slot runs one controller at a time.
Source fingerprints use Git executable-bit semantics; snapshots normalize public file modes explicitly,
so the checkout umask cannot change the candidate. One plain source copy is retained for local
checks and controllers, with the runtime assets under its `.subyard-acceptance/release/` directory.
The output also retains the transport archive, logs and `receipt.json`; the intermediate source
archive is removed after extraction. Neither the source copy nor the VM payload needs a Git
repository or index. Every controller receives the same checksum-verified transport archive; profile fixtures
execute the packaged engine and runtime assets. The core smoke installs those assets through the
public updater, including reboot, rollback and roll-forward. Local verification, ShellCheck, real local adapters and updater compatibility precede physical checks. A CI-prepared candidate counts these four from its successful preparation jobs; see [release publication](release-publication.md).
Process coverage (`dev/process-coverage.sh`) is optional; ordinary process contracts remain in
`make verify`. Frozen receipts retain their recorded coverage requirement and command binding.
Run the aggregator from the managed workspace checkout: it supplies that workspace for lease
attribution while executing controllers and guest payloads from the frozen candidate.

A repeated `run` executes only checks without a passing result. Use `--only profile:NAME` or
`--only p0-release-smoke` for a remaining independent check; `--rerun` also repeats passed checks.
After capacity changes, inspect a new supplied snapshot with `preflight --output DIR`, then continue
the same frozen candidate with `run --output DIR --slots N --only CHECK`. Passing local checks remain
bound to that identity and are retained; capacity alone does not require another `prepare`.
This is a separate continuation run: retry the blocked check after conditions change, retaining
its original attempt logs/events and applicable PASS results from the same candidate.
Planning insufficiency is distinct from an actual typed broker refusal recorded as `blocked` with
`capacity_memory` or `capacity_disk`. Both leave required physical acceptance incomplete.
Profiles may declare independent checks in `tests/e2e/acceptance-lanes.json` with schema version 1
and ordered `lanes` entries containing `id` and runner `arguments`. A lane marked
`"periodic_only": true` runs only in Periodic E2E: it is not a release check, so release
acceptance neither runs nor requires it. A schema-version-1
`acceptance-lanes.json` may declare nonempty `lanes`, a host-free `preflight` command array, or both.
A preflight-only declaration preserves the unsplit profile acceptance check. Full collector runs
execute declared preflights after successful local gates and collect results in profile-name order
before live dispatch. Recorded failed, blocked, interrupted, incomplete, or unfinished `running`
preflights prevent live dispatch, including targeted continuation. Untouched pending preflights
remain exempt for fresh targeted jobs, whose explicit selections remain unchanged. Explicit `--with-vms` requests
every runnable release lane; `--only profile:NAME/ID` requests one release lane and refuses a
periodic-only lane as an unknown check. Available approved slots take the
next queued check after their controller exits. A typed `blocked/capacity_memory` or
`blocked/capacity_disk` refusal before any lease or guest start lets that slot worker take the
next independent check. After all first-pass controllers finish, capacity-blocked checks are
retried once on slots whose workers drained the queue. Both attempts retain their logs and
immutable records; a second capacity block leaves the candidate incomplete.
Failures, interruptions, unknown refusals and attempts that
reached a lease or guest start stop that worker's queue. Exit 4 alone does not permit continuation.
A physical retry always allocates fresh VMs. It never resumes a fixture across leases. Source or
artifact changes reject reuse; prepare a new candidate in a new directory. Logs are retained per
attempt and include the controllers' source and environment evidence. The run checks its platform
and Go toolchain against preparation; receipts retain measured execution and available VM base identities.

`receipt.json` records terminal results, including `blocked` capacity refusal and `interrupted`
execution. Before dispatch, the collector durably replaces the selected check with `running`,
removing its former PASS reference. A crash cannot restore that historical PASS. After terminal
execution, log fsync and environment capture, the collector creates an immutable completed-attempt
record before replacing the aggregate entry. An interrupted or failed attempt remains evidence,
while a record orphaned by a failed aggregate commit is never discovered or indexed automatically.

New direct checks (except package preparation) have `original_attempt: {id, sha256}` references.
`attempts/ID.json` and shared `manifests/SHA256.json` use exact-key schema version 1 and canonical
JSON bytes (sorted keys, compact separators, no trailing newline). Creation is exclusive with exact
mode 0600, followed by file and directory fsync. Their hashes bind the exact saved bytes; immutability
is a protocol, not a claim that local files cannot be edited. Validation rejects changed records/logs,
foreign candidates and differences from the current authoritative receipt result. Historical receipts
without these references remain readable. Package retains its final bundle or CI preparation binding.

A manifest contains exactly `schema_version`, `candidate`, `source_entries` and `environment`.
`candidate` is the full `transfer_candidate(receipt)` identity; `source_entries` maps every public
source path to `[regular|executable, SHA256|link:TARGET]`. Validation rebuilds the source fingerprint
and verifies runtime membership in the complete release asset map. `environment` is the measured
preparation system/machine/Go identity. An attempt contains exactly `schema_version`, `id`,
`manifest_sha256`, `check`, normalized `command` and `result`. The result has exactly `status`,
`exit_code`, `termination`, `signal`, `duration_seconds`, `log_sha256`, `execution_environment`,
`environments`, `environment_evidence`, `slot`, `cleanup`, `cleanup_status`, `capture_status`,
`last_completed_phase`, `phase_budgets`, `failure_class` and `reason`. Absent measurements are null
(`environments` is an empty list); capture, timing or environment evidence is never reconstructed.
An incomplete capture cannot certify PASS. `events.jsonl` retains starts, results, minute heartbeats and bounded controller phases;
phase durations use the controller clock. An unfinished phase has no successful end marker.
Each validated `run` also prints a unique invocation ID and `runs/ID.json` pointer. This versioned
sidecar records that selection's attempted, reused and not-started checks with reasons, outcome/log
pointers and the final receipt digest. `selected_status=passed` means every selected check passed
(or its existing inventory permits N/A); `overall_status=incomplete` still prevents publication and
retains the legacy nonzero aggregate exit. Exclusions leave required acceptance incomplete. A rerun
uses its new attempts; undispatched checks cannot inherit an earlier PASS. `running`, `interrupted`
or `error` sidecars cannot certify selection PASS. Capture failure is separate from the native workload
exit and known cleanup evidence; absent native facts remain unknown. Sidecars never alter the receipt
format or digest. `run-start` and `run-result` carry the same ID; read the terminal sidecar after its
final saved receipt, rather than treating an early `check-result` as the final summary.
Fixtures may emit `E2E_PHASE phase=fixture/NAME state=start|end duration_seconds=N exit_code=N`
with a lowercase name of up to 60 letters, digits or hyphens. Start markers use zero duration and
exit code; end markers retain the measured duration and original exit code (0–255). The collector
projects only this bounded grammar into phase events, without copying fixture output. A successful
cleanup phase does not change a preceding failure into a pass.
Terminal results retain the native exit, classified termination (`exit`, `signal`, `timeout` or
`unknown`), last successful operation phase and separate cleanup phase exits/durations.
Exit 124 is a timeout; known signal exits retain the signal number. Any lease or guest-start
marker rules out capacity classification for the entire attempt, even if a typed refusal follows it.
Exit 4 then remains a failed check with its native exit. A typed refusal before any such marker is
environment failure; otherwise cause stays unknown unless the fixture emits the bounded
`E2E_FAILURE class=product|fixture|environment|timeout|unknown` marker. Do not infer a product
cause from an unexplained nonzero result. Guest worktree deletion has a 120-second transport
budget, lease release 180 seconds, each with a five-second TERM-to-KILL allowance. The GitHub
connection fixture limits ordinary phases to 1800 seconds and each cleanup to 180 seconds,
with 60 seconds for guest termination. Its fixture cleanup, guest cleanup and lease release are
separate evidence. A cleanup error changes a successful invocation to failure but preserves an
already nonzero workload/signal/timeout exit. Capture failure and original work remain distinct.
Bounded `E2E_BUDGET` records populate `phase_budgets`; `E2E_RELEASE` start/end records preserve
the lease release budget, elapsed time and native status separately from aggregate cleanup.
Use `supersede --output DIR` to retire an idle candidate while preserving its original evidence.
Only one mutating collector may use an output directory at a time.

Explicit `import --output NEW --from OLD --check CHECK` copies passing command-bound evidence
between bound candidates with matching source and release assets, retaining inherited provenance.
Only provenance generation timestamps may differ; artifact identities and original metadata hashes remain bound.
Local evidence also requires its recorded execution environment to match.
The current run prints `REUSE` with direct/inherited origin and its validation reason for applicable
PASS results. A new or changed input requires the affected fresh check; failure/incomplete results
remain recorded and cannot be promoted by a passing subset or an equal executable.
`--allow-other-profile-tests` permits a profile check to survive changes confined to other shipped
profiles' unshared test directories. Its runtime, assets, inventory, own profile and shared inputs must
remain identical. A profile's tests are shared when any non-Markdown file outside that profile names
`profiles/NAME/tests`; a change there invalidates every profile check. This exception never applies
to local gates or continuous release smoke.

`reuse --output NEW --from OLD [--from OLDER ...]` applies the same rule automatically to every open
profile check and records each match as an original-attempt evidence transfer under
`policy:isolated-other-profile-tests-v1`, taking the first listed candidate that qualifies. The
original PASS must be a complete direct attempt; physical attempts also need recorded environment
and cleanup. The review text is generated from the changed paths, and validators, including
publication, recompute the scope from both manifests and the target source. Busy, superseded or
different candidates are skipped and each skip of an existing PASS prints a `REUSE-SKIP` reason.
Shared-test detection matches literal `profiles/NAME/tests` references; a reference built from a
variable or glob must also name the path literally. The release `accept` command runs it before any check against
earlier local candidates of the same repository and version, newest attempt first.
The separate `policy:harness-only-v1` also reuses profile and `vm:*` checks when changes are confined
to `dev/release-publication.py`, `dev/release.sh`, `dev/ci/`, `.github/`, `docs/` or top-level Markdown
files (plus other profiles' unshared tests for profile checks); local gates, final-byte checks and
`dev/release-acceptance.py` are excluded, and migration/update-contract refusals still apply.

#### Reviewed evidence transfers

After reviewing changed inputs, transfer per-check evidence as inherited coverage:

```sh
python3 dev/release-acceptance.py transfer --output NEW --from OLD --check CHECK --review REVIEW.json
```

Both directories must be valid frozen candidates. The target check must still be pending, or an
optional VM profile check with the exact untouched default `not-run` result; no `--exclude` is needed
first. Failed, blocked, interrupted, already passed, otherwise not-run, or previously requested target
checks cannot be overwritten.
A transfer may cover a reviewed product change only when no product migration or update-contract
path changed; otherwise rerun the check. Such paths name a migration or transition (`migrat` or
`transition`) or belong to the update contract: `scripts/install-runtime-release.sh`,
`dev/bootstrap-runtime.sh`, `dev/release-assets.sh`, `internal/cli/update*.go`,
`internal/cli/release_*.go` and `internal/adapters/releaseruntime/`. Markdown, `tests/`, `testkit/`,
`testdata/`, `e2e/`, `*_test.go` and `*-test.py`/`.sh` files never count. Validation of legacy
schema-1 transfers has no source inventories, so this rule applies to them only when they are made. `verify` and
`p0-release-smoke` transfer only between identical sources, so they always describe the final bytes.
Every refusal that applies is reported together. Publication prints each inherited check with its
authorization and reason.
For a new original attempt, `REVIEW.json` has exactly `schema_version: 2`, `check`,
`tested_manifest_sha256`, `target_manifest_sha256`, `original_attempt`, `review` and `standalone`.
`original_attempt` is the source check's current `{id, sha256}` reference; `standalone` must be null.
The tested manifest is the one referenced by that record. Obtain the target manifest hash with
`evidence_hash(candidate_manifest(target_root, target_receipt))`; these helpers do not authorize
reuse. `review` retains the scope/reason/authorization fields below. The source must be active and
its current direct attempt must still be a complete PASS. A later FAIL, interruption or running
rerun prevents selecting an older attempt. An unrelated source receipt update does not invalidate
this declaration. The transfer copies and rehashes the original record, both manifests, review and
log, with no dependency on a digest of the mutable carrier receipt. Physical attempt transfer also
requires recorded environment and successful cleanup evidence.

Legacy receipt and standalone declarations remain supported unchanged.
Their `REVIEW.json` has exactly `schema_version: 1`, `check`, `tested`, `target`,
`source_receipt_sha256`, `review` and `standalone`. Each candidate identity contains
`version`, `source_fingerprint`, `runtime_file`, `runtime_sha256`,
`candidate_bundle_sha256`, the complete `release_files` hash map and `candidate_sha256`.
The latter is the preparation identity hash for CI candidates, otherwise the collector's
version/source/assets/transport identity hash. Generate these identities with the tooling's
`transfer_candidate(receipt)` helper; no field may be omitted. `review` contains public,
nonblank `unchanged_scope`, `reason` and `authorization_reference` strings identifying the
review and release task. The scope must explain why this exact check remains valid despite
the recorded source/runtime/asset differences. New or affected scenarios require fresh execution.
The tool checks bindings and evidence; the review establishes the unchanged scope and
records its reference.

For an original collector PASS, `standalone` is `null`. The original exit must be zero,
the exact command must match, and the original receipt/log and execution environment are
retained. Already inherited results cannot be transferred again. `package` and external
obligations cannot use this path; current preparation and external rules remain authoritative.

An uncollected physical controller PASS may instead supply `standalone` with exactly
`native`, `launch` and `log`, each containing `path` and `sha256` for ORIGINAL evidence.
Relative paths resolve beside the review declaration. The original collector entry stays
pending. The native terminal object must have a final integer exit zero, and the complete
controller log must retain the VM base, successful cleanup of every VM and lease release.
The frozen source, release assets and exact launcher bind the checksum-verified tested bundle;
native exit zero proves controller completion. Transport and guest phase timing may be absent
for direct controllers and stays unknown; any observed failed phase rejects the transfer.
Rich launch records contain `cwd`, `argv`, `source_fingerprint`, `runtime_sha256`, `bundle_sha256`,
`acceptance_script_sha256` and optional `launched_at_utc`. Original raw records with `cwd`, `argv`
and timezone-aware `started_at` are also supported. Their checksum-bound bundle must exactly match
the frozen source, executable modes, candidate metadata and complete release inventory; do not
add hashes to original records. Contradictory bindings or malformed timestamps/clocks are refused.
The command must be the exact check plus `--slot N`, in the supported
`timeout --signal=TERM --kill-after=300s 90m env` wrapper with only `TMPDIR` and the three
controller/transport assignments. Unmeasured duration and execution environment stay unknown;
preparation environment and final-poll duration cannot substitute for them.

Single and batch transfers stage and validate evidence before replacing the receipt. A copy failure
leaves the receipt unchanged; a later capture failure reports the commit state and preserves evidence.
Transfer retains and revalidates the review, original evidence and exact target. Receipts distinguish
`coverage.executed` from `coverage.inherited`; pending gates stay pending. Publication sends only
sanitized identities, hashes, command/environment provenance and authorization.

#### Batch transfer preparation and application

Use the same transfer command for a batch of independently reviewed checks:

```sh
python3 dev/release-acceptance.py prepare-reviews --output NEW --from OLD --request REQUEST.json
python3 dev/release-acceptance.py transfer --output NEW --batch REQUEST.json --plan > PLAN.json
python3 dev/release-acceptance.py transfer --output NEW --batch REQUEST.json
```

`prepare-reviews` takes repeatable `--from` sources, selects the first qualifying complete direct PASS
for each open executable check, prints skipped checks with reasons, and writes `REQUEST.json` plus
unsigned drafts in `REQUEST-reviews/` with source and asset changes; it applies no transfers.
Edit each draft's scope, reason and authorization reference, then replace that check's `review: null`
with `{ "path": "REQUEST-reviews/0001.json", "sha256": "<SHA-256 of the edited file>" }` before applying;
paths must be new, and no request is written when no check qualifies.

`--batch` excludes `--from`, `--check` and `--review`; `--plan` requires `--batch`.
The exact-key request schema is `schema_version: 1`, `target` and `checks`. `target` is the
complete `transfer_candidate(target_receipt)` identity. Each check has exactly `check`, `source`,
`attempt` and `review`. `source` names its original candidate output directory. `attempt` is its
current `{id, sha256}` original-attempt reference, or null for legacy receipt/standalone evidence.
`review` is `{path, sha256}` for the exact declaration bytes, or null during planning. Paths resolve
relative to the request file. Duplicate checks and source paths resolving to the target are refused.

Planning returns JSON with `schema_version: 1`, `target_manifest_sha256`, a shared hash-keyed
`manifests` table, one `pairs` source/target diff per unique source and per-check `checks` decisions.
Diffs retain changed source paths with their actual normalized modes and hashes, changed release
assets, runtime equality and the validated `identical`, `generated-at-only` or `review-required`
release scope. Each decision refers to the shared manifest and original attempt and includes an
unsigned `review_draft`. Fill its public scope, reason and authorization reference in a separate
review declaration, then bind its path and exact hash in the request. Planning never supplies
an authorization. Legacy draft declarations retain their required v1 candidate identity fields;
new v2 drafts use manifest references. To plan standalone evidence, supply a bound v1 review
containing its original artifact descriptors; its decision strings may still be blank.
Planning preserves receipts and evidence; existing collector-lock bookkeeping still applies.
Request and preparation JSON are bounded to 1 MiB.

Apply is a separate invocation and revalidates current identities, inventories, commands,
environments, original attempts and reviews. Equal runtime bytes alone never establish unchanged
behavior. The ordinary generated-at-only provenance rule remains available in the shared diff;
semantic differences still require explicit review. New physical attempts require recorded base
and successful cleanup evidence. Standalone evidence retains the original launcher, bundle,
terminal-result and cleanup checks; unknown timings remain unknown. Inherited evidence cannot
be chained, active sources must remain current, and only pending or untouched optional VM target
checks can be replaced.

Canonical paths are deduplicated and existing nonblocking locks are acquired in sorted order:
exclusive for the target, shared for every source. Each unique snapshot builds its public source
manifest once, and each source/target pair computes one diff. Mandatory artifact copying still
rehashes the bytes. All checks are validated before copying into an invocation-owned staging
directory; shared artifacts are copied once by role/hash. The collector validates every staged
result, publishes its artifacts and performs one atomic receipt replacement for the whole batch.
Any precommit failure preserves the receipt and removes only that invocation's staging/artifacts.
Unrelated or orphan staging is never applied or removed.

Committed checks retain `transfer_batch_sha256`, derived from the target and canonical
source/check/attempt/review references. Retrying those exact references returns `state=committed`
without copying or rewriting the receipt. New-attempt retry identity excludes the mutable source
receipt digest. A later source rerun cannot make a completed target transfer appear untouched;
conflicting references are rejected. A postcommit event/capture or durability-confirmation error
reports the reached commit state and preserves referenced artifacts. If replacement cannot be
observed, the state is `unknown` and potentially referenced artifacts are preserved for a safe retry.
There is no separate transaction database. Publication uses the existing v2 transfer/v5 attestation
schemas, including mixed legacy receipt/standalone evidence and explicit publication exceptions.

#### External checks and publication

The [external-check contract](release-publication.md#external-checks) defines profile-owned
requirements, optional `not-run` results, actual PASS evidence and explicit required-check deferrals.
These do not replace required local or VM gates.

Keep receipts/logs local and follow the [cleanup policy](../testing.md#evidence-handoff-and-cache-cleanup) before
removing them. For publication, use the [release flow](release-publication.md), which
imports exact CI assets and submits bound acceptance without repackaging.

GitHub CI runs the full core gate, profile host-free checks, warning-level ShellCheck and
`bash tests/real-host/adapter-contracts.sh` in one `verify` job on every branch push and pull request;
release preparation and publication use explicit workflow dispatches on a branch whose head is the
candidate commit, with no tag-triggered publication. A non-head release needs a release branch
pushed at that commit first, even when its workflow files match the default branch. Preparation
checks out the workflow SHA and requires it and HEAD to equal the input commit before source
execution. Basic native ARM64 engine build,
execution and Go tests run on GitHub's ARM runner in CI and gate Release publication. ARM requires
no local agent access or workflow authorization.
The separate nightly/manual Deep CI adds only what that gate cannot find: it builds the runtime,
runs every Go package twice with the race detector in shuffled order to expose order-dependent
and intermittent tests, and fuzzes each fuzz target for one minute.
Both workflows use standard Ubuntu runners, read-only repository permissions and no artifact uploads.
Veranda and live P0 acceptance remain separate checks.

#### Exact-candidate hosted release checks

`release-vm.yml` runs the explicit `CI_HOSTED_CHECKS` inventory against a downloaded preparation
artifact, one fresh collector per job. It shares the Periodic E2E pool setup. Job success requires
a terminal recorded PASS for that exact selection and recorded cleanup; the collector's aggregate
receipt stays incomplete while other checks are unrequested. `accept` waits up to six hours (override
with `--ci-wait-seconds`, including `0`), counts verified successful jobs as `ci-hosted`, and runs only
the remaining requested checks. Wrong SHA, failed jobs, unknown workflow policies, foreign candidates,
stale attempts and missing cleanup refuse import. Publication rechecks GitHub execution facts.
Nested-VM stages keep `vm:p0-full` local; its non-nested power-systemd and
legacy-continuation lanes are the hosted `vm:p0-power-systemd` and
`vm:p0-legacy-continuation` checks, which its `--split-covered` matrix does not repeat.
See [release publication](release-publication.md) for identity and policy rules. The local
contracts are `tests/release-acceptance.sh`, `tests/release-publication.sh` and
`tests/workflow-real-adapter-gate.sh`; they do not establish native hosted VM acceptance.

Every Release VM job attempts to upload `hosted-diagnostics-JOB_INDEX-ATTEMPT` from its
`always()` steps, including after failure, check timeout or cancellation. Find artifacts on the
workflow run page. Contents differ by workflow:

| Artifact | Contents, when produced by the check |
| --- | --- |
| Release VM `hosted-diagnostics-JOB_INDEX-ATTEMPT` | Sanitized `acceptance/<check>-*.log`, other collector logs, `acceptance/receipt.json`, `acceptance/events.jsonl`, `acceptance/attempts/*.json` and `acceptance/runs/*.json`; `runner/` probe logs/results and broker events; `p0/` failure snapshots; `cleanup.log` and `summary.json` |
| Periodic `pool-diagnostics-ATTEMPT`, `diagnostics-JOB_INDEX-ATTEMPT`, `nested-diagnostics-JOB_INDEX-ATTEMPT` | Sanitized `runner/check.log`, probe logs, `runner/results.tsv` and service/yard broker events; available `p0/` failure snapshots; `cleanup.log` and `summary.json`. These jobs have no collector output under `$RUNNER_TEMP/acceptance`, so no `acceptance/`, receipt, original attempt or invocation records |
| Release VM `ci-hosted-JOB_INDEX` | Strict validated native PASS evidence: `ci-execution.json`, receipt, events, logs, original attempts/manifests and invocation records. This is separate from diagnostic snapshots |

`p0/` preserves the route-relative paths of `p0-*.failure.log` (keeper, redacted broker status,
capacity, reachability and OOM snapshots). `cleanup.log` contains captured cleanup/release lines;
absent cleanup stays unconfirmed. `summary.json` records execution IDs, check/job outcome,
environment facts, truncation and omitted-file counts. Early setup failure can leave only a subset
of files. Intel-skipped nested jobs do not upload a snapshot.

Diagnostic redaction is a convenience for disposable test values, not a security boundary.
The workflows refuse repository `secrets` references and restrict `github.token` to the artifact
download and control steps; the workflow contract checks those sites. Capture applies exactly four
rules, in one line-oriented pass shared by all files and the printed tail:

1. Mask exact values of at least eight characters from environment variables whose names contain
   a whole word `TOKEN`, `SECRET`, `PASSWORD`, `PASSWD`, `PASSPHRASE`, `CREDENTIAL`, `CREDENTIALS`,
   `COOKIE`, `API_KEY`, `PRIVATE_KEY`, `ACCESS_KEY` or `SIGNING_KEY` (case-insensitive; underscores
   separate words). Names ending in `_FILE`, `_PATH`, `_DIR` or `_SOCK` are excluded. Values are
   never learned from log text or JSON fields.
2. Replace paired `-----BEGIN … PRIVATE KEY-----` through the matching END with `[redacted PEM]`.
   If no matching END exists, suppress the BEGIN line and at most 128 following lines, retaining
   later diagnostic text. Certificates and orphaned base64 bodies have no masking rule.
3. Mask `gh[pousr]_[A-Za-z0-9]{36,}`, `github_pat_[A-Za-z0-9_]{20,}` and JWTs matching
   `eyJ[A-Za-z0-9_-]+\.eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`.
4. Replace hex runs of at least 64 characters, including runs glued to identifiers, with
   `[hexN:TAG]`, where TAG is the first eight hex characters of SHA-256 of the matched bytes.
   Equal values have equal tags; different values remain distinguishable. Shorter hex runs,
   including 40-character git SHAs and 32-character IDs, remain unchanged.

There are no label, flag, URL, entropy or control-normalization rules. Unknown passwords,
Authorization/Cookie values, URL userinfo, `sk-`/base62/base64url strings, SSH public keys and
fingerprints remain visible unless they match one of the four rules. Prose, instance/test names,
paths and file locations are preserved outside those matches. Native `ci-hosted` evidence retains
its original bytes, and Periodic check output is also present in the job log.

Capture streams the check log first (`acceptance/<check>-*.log` or `runner/check.log`; otherwise
failed probes selected from `runner/results.tsv`), writes it and prints its tail before opening
other records. Remaining order is runs, receipt, attempts, events, P0 failure logs, broker events
and other logs. Each file is independent; `summary.json` is written in `finally` and records skip
reasons. There is no whole-input byte budget. A private temporary disk spool preserves PEM state
across arbitrarily long keys; only the last 2 MiB of sanitized output per file is retained in memory
for the artifact. Capture retains at most 128 files and 16 MiB of file content, plus summary/cleanup
metadata. Snapshots contain no source tree or release assets and expire after seven days.
Failed jobs print at most 80 lines / 16 KiB. Diagnostic steps are best effort: their failure cannot
change a passing job into a failure or prevent counting otherwise valid hosted evidence.
The lightweight redaction/capture tests can run independently with
`python3 -B tests/helpers/hosted-diagnostics-test.py`; the publication owner also runs them.

Check steps have a 340-minute limit within the 350-minute job budget (110/120 for the Periodic E2E
pool), reserving time for diagnostic collection/upload. Runner loss or a forced termination that
prevents final steps can still prevent upload. Diagnostic snapshots never count as evidence. The
separate `ci-hosted-JOB_INDEX` artifact is uploaded only after strict validation of the complete
selected PASS and cleanup; its retention remains 90 days.

## Development verification

`TMPDIR=/tmp bash tests/release-acceptance.sh` and
`TMPDIR=/tmp bash tests/release-publication.sh` check candidate/evidence binding, optional external
results, explicit decisions, transport validation and publication recovery with synthetic fixtures.
They do not dispatch workflows, publish releases or establish physical/external acceptance.

GitHub references: [concurrency](https://docs.github.com/en/actions/concepts/workflows-and-actions/concurrency),
[release API](https://docs.github.com/en/rest/releases/releases),
[asset upload/recovery](https://docs.github.com/en/rest/releases/assets#upload-a-release-asset),
and [immutable release preparation](https://docs.github.com/en/repositories/releasing-projects-on-github/managing-releases-in-a-repository).
