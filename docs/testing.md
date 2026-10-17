# Testing changes

Test-selection policy lives in [Subyard dev-flow](../.agents/skills/subyard-dev-flow/SKILL.md#choose-checks-by-risk).
This guide documents how to run checks and interpret their evidence.

## Keep tests proportional

Use the skill's [risk-based selection policy](../.agents/skills/subyard-dev-flow/SKILL.md#choose-checks-by-risk)
for both adding tests and choosing which existing checks to run.

Use `testkit.WriteFile` for exact file modes and `testkit.TempDir` for private
fixture roots. Keep raw calls for creation-semantics tests. The full core Go race suite runs
uncached once under umask `0022`. The explicit file-creation and permission selection in
[`tests/run.sh`](../tests/run.sh) also runs uncached under `0002` and `0077` in separate
processes. This covers private persistence and public output modes, while fixtures that
explicitly chmod unsafe inputs keep their ordinary full-suite coverage. The shared
[`go-permission-umasks.sh`](../tests/helpers/go-permission-umasks.sh) checks that every selected
test exists before running the additional masks; update the selection when renaming a test
or adding a distinct creation-mode boundary.

Profile settings/descriptor Go checks run once under `0022` through `profile-go.sh`.
The GitHub profile runs its full Go race suite under `0022` and repeats only its explicit
broker protected-file checks under `0002` and `0077`.

## Coverage ownership

The GitHub owning release-activation fixture checks an authentic published predecessor upgrade,
delivery/restart interruption and fresh-consent retry, loaded broker/client identities, core helper
delivery, differing local profile selections, stopped-yard deferred convergence and retained data.
It also checks refusal of an incompatible published rollback and a supported rollback between two
compatible packaged fixture versions. The second version contains explicit skill/preview fixture
deltas; it is synthetic compatibility evidence. Run its local `--preflight` before allocating a VM,
then provide the frozen public candidate bundle and SHA-256 to
`config/profiles/github/tests/e2e/release-activation.sh --slot N` as described in the VM guide.
The candidate identity comes from its verified frozen metadata; select `--baseline 0.20.0`
for the current published predecessor, or retain the original default 0.19.0 regression path.
This focused lane does not replace release publication or external GitHub-account acceptance.

Logical matrices belong to their unit or local integration owner. Physical E2E keeps
installed-runtime, real Incus/systemd, routing, reboot and retained-data evidence.
Choose the owning packages, scripts and lanes below by the changed contract, rather
than repeating their logical matrices in P0.

| Contract | Owning unit checks | Local integration | Physical gate | Trigger |
| --- | --- | --- | --- | --- |
| Legacy config normalization and pure stdin/stdout | `go:migration`: `TestNormalizeLegacyYardConfigContent` | `go:cli`: `TestNormalizeLegacyYardConfigDoesNotWritePathsDuringRecovery` runs CLI stdin/stdout against a real filesystem fixture | `p0:source-upgrade`: authorized import, installed readiness/history and real host-to-guest materialization | Normalizer or source ingress changes |
| Source authorization, journal binding and recovery | `go:releasetransition`: journal codecs and in-memory source inspection normalization | `go:releasetransition`: source order/drift/recovery over filesystem fixtures; `go:cli`: candidate process/source ingress checks; `shell:engine-release`: installer/update/rollback | `p0:source-upgrade`: source bootstrap and pinned predecessor histories | Transition/installer changes |
| Config precedence, safe diagnostics and drift | `go:config`: `TestResolveIntegrationSelection` | `go:config`: precedence over persisted layers; `go:cli`: config paths/show/status/apply tests use persisted config fixtures and native materialization; `shell:engine-release`: installed process checks | `p0:source-upgrade`: config apply/status plus actual guest file hash | Config resolver/materializer changes |
| Status space cache freshness and selectors | `go:adapters/statusruntime`: `TestDescribeSpaceCache` uses fixed time and an injected refresh request | `go:adapters/statusruntime`: cache persistence and synchronous/asynchronous refresh contracts; `go:cli`: `TestStatusRoutesSummaryAndExplicitYardSelectors` and `TestSpace*` | `dev/e2e/status-fast-acceptance.sh`: real container refresh, separate `/srv` mount and verified same-inode storage allocation; `p0:real-incus` retains VM adapter compatibility | Status cache or space measurement changes |
| Generic resource consent, exact plans and retained data | `go:resource`: `TestStructuredPrepareContract`; `go:cli`: stream prompt logic | `go:resource`: registry/assessment over descriptor files; `go:cli`: resource command/prepared tests execute handlers; `TestResourceExactTerminalConsentAndRetainedData` adds real files and PTY; shipped profile host-free checks own handler semantics | `p0:profile-resource`: bound-resource profile; shipped profile E2E gates own external services | Resource contract or profile changes (`profile-generic` and owning profile sets) |
| RPC framing, skew, cancellation, EOF and ordered streams | `go:rpc`: in-memory framing/stream checks in `session_test.go` | `go:cli`: typed owner queries over config fixtures; `shell:engine-release`: packaged version negotiation; `e2e:ssh-rpc`: real loopback SSH and selected-yard native settings | `p0:peer`: deployed selected-yard project query and reconnect snapshot; `p0:transport`: real disconnect | RPC/SSH changes |
| Exact owner plan binding, expiry and replay | `go:cli`: `TestExactOperationPlanExpiry` and `TestExactOperationPlanRejectsChangedBinding` use fixed time and in-memory plans | `go:cli`: `TestExactRPCPlanBindingAndSingleUse` retains handler refusal, no mutation and replay; `TestExactRPCRequiresCapabilityAndDiscardsOnDisconnect` uses `net.Pipe` | `dev/e2e/integration-remote.sh`: successful negotiated plan/execute over real SSH, remote enable/disable/status and retained data | Exact plan or owner session changes |
| Project copies, owner inventory and offline recovery | `go:application`, `go:ownerinventory`: decisions with in-memory ports | `go:state`: file-backed persistence/recovery; `go:cli`: `TestOwnerProjectStateConvergesWithoutLosingLocalSource`, `TestReadOnlyProjectListHonorsFreshCacheAndLiveForce`, copy/admission/consent matrices; `tests/project-registry-convergence.sh` and `tests/remote-projects.sh`: native RPC/transfer and removal boundaries | `p0:peer`: real concurrent copies, remote shells, offline stale inventory and refresh/deletion | Project admission/routing changes |
| Owner inventory read freshness | `go:ownerinventory`: `TestSnapshotFresh` uses fixed time for default/custom TTL boundaries and clock skew | `go:ownerinventory`: service cache/fetch contracts; `go:cli`: trusted owner rename and stale inventory checks | `dev/e2e/entity-naming-lifecycle.sh`: real SSH rename, key rotation, offline refresh and removal; fixture timestamps expire caches without wall-clock sleeps | Owner inventory cache or refresh changes |
| Host registration consent and trust | `go:cli`: `TestSSHTrustUsesTypedConsentAndDoesNotConsumeCommandPayload` covers interactive and automated consent | `go:cli`: `TestHostAddPinsFingerprintAndRegistersInitialSnapshotTogether` checks one confirmation and atomic trust/inventory registration | `dev/e2e/entity-naming-lifecycle.sh`: direct, alias and proxy SSH registration uses explicit `--yes` | Host registration or SSH trust changes |
| Crypto and SSH adapter compatibility | Owning crypto, trust and transport packages | `e2e:adapter-contracts`, `e2e:credential-tools`, `e2e:ssh-credential-peer`: pinned real tools and loopback SSH | `e2e:ssh-unknown-host`, `p0:peer`: separate host identities, routing and failure/reconnect | Adapter/tool/trust changes |

Go packages can contain both unit and local integration checks; the fixture boundary
determines their evidence, rather than the test language or package name.

Local adapter fixtures are T1 checks with local entrypoints; they require their
documented tools and never acquire VM leases.
A local native fixture does not establish systemd/cgroup, physical disk or Incus behavior.
Frozen release smoke and peer fixtures already reuse collector candidate assets when present;
source A/B, tagged owner histories and pinned predecessors retain distinct artifact identities.
Full/release-smoke, reboot observations, nested teardown and all shipped-profile acceptance
requirements remain as documented; local passes do not replace them.

## Execution and reporting principles

Follow [test execution and delegation](../.agents/skills/subyard-dev-flow/SKILL.md#test-execution-and-delegation)
for roles and model settings. Preserve required coverage and candidate identity.

- One assigned polling owner reads active progress/logs; the developer and coordinator use
  its events and evidence instead of parallel polling. Interactive root-console work also has
  one owner. Process control, lease heartbeats and cleanup remain with the existing runner.
  While an observer owns the run, other roles do independent work or use the supported agent
  event wait within client limits. Do not add process/status/log polls or request updates merely
  because a wait returned unchanged. Check ownership once when its agreed checkpoint expires.
- Prefer completion events. For capacity shortages, use long adaptive waits and retry guarded
  acquisition when availability may reasonably have changed. Record the next retry time or
  resource-change trigger in the existing task. Before that time or trigger, an unchanged timed
  wakeup does not justify another capacity probe or allocation attempt. Use supported waits within
  client limits, without intermediate clock/status calls solely to recheck the wait.
  Necessary timed waits may still invoke the model.
  Send routine/material progress to one responsible canonical recipient named in the assignment
  (normally the coordinator while the developer phase has ended). Preserve operator updates.
  Do not send duplicate progress to both roles or acknowledge/follow up merely to say still running.
  Send failures, observation errors, blocked/interrupted execution and cleanup problems immediately
  to both the assigned developer and coordinator (canonical targets in the observer assignment).
  Escalate questions and approvals when observed;
  never answer approvals for the operator.
- Save the FULL native terminal tool object before any final artifact/projection read, even for
  empty stdout with a nonzero exit. Then read native summaries/receipts first. Return one final report:
  run/check identity, check and overall candidate statuses where applicable, original exit code, total duration and
  evidence paths. Verify every field; mark unavailable values explicitly. Use native duration
  or launch-to-completion time, never an individual wait's wall time. Preserve full logs;
  identify shortened output and include a bounded safe failure excerpt with its complete log path.
  Keep the original test exit distinct from capture failure/refusal and cleanup status; none of
  these can overwrite or turn that exit into a pass.
- Preserve failed, blocked, interrupted and incomplete outcomes. Missing expected final
  evidence is incomplete; heartbeats, process disappearance or one passing check do not
  prove complete acceptance.
- Reuse applicable evidence; do not duplicate logs/receipts in separate reports or run extra
  acceptance cycles solely to measure efficiency. Rerun reasons belong in the current task.

### Launch and observe long checks

Start the prescribed runner once; retain its process/session handle and evidence paths.
Set wait intervals explicitly, about once a minute within tool limits and applicable rules:
`exec_command` initial `yield_time_ms=30000`, then `write_stdin` with empty `chars` and
`yield_time_ms=55000`; an enclosing `functions.exec` uses `yield_time_ms=60000` to cover
the inner wait and bounded reads. One enclosing call contains only one long process poll;
do not serialize several long polls in it. Resume a yielded script cell before another
process poll: its cell ID is distinct from the process session ID. On an unchanged timeout, continue
the same wait without rereading plans, receipts or logs or sending an acknowledgement. Short
default waits add model returns; explicit outer timed yields can still cause model returns.
Each completed timed observation step also returns to the model; this protocol does not remove inference.

Copy the prescribed command as an exact code block/argv, with no prose punctuation. Before launch,
verify its arguments, candidate, output directory, expected checks and capture paths. Keep one
observer and one runner owner; never restart a runner to repair observation.

[`dev/project-run-events.py`](../dev/project-run-events.py) reads only new complete JSONL records,
omits heartbeats and returns a bounded batch plus the next cursor. Accept the whole successful
projection before persisting its cursor outside the loop. It binds device/inode, observed size and
a bounded anchor at the processed newline; truncation/replacement or malformed complete records
produce explicit diagnostics, never an automatic offset reset. The journal stays untouched.
An incomplete tail is deferred. Drain `backlog` batches without another long wait so failures and
terminal events beyond the cap are reached. At native completion, an incomplete tail is incomplete
observation evidence. The anchor detects changes near the cursor, not arbitrary historical edits;
the existing journal remains append-only evidence, not a writable observation state.
A single record must fit the byte cap (8 KiB in the template); oversized lines are diagnostics.
If a valid native record needs a larger cap, adjust projection and output budgets explicitly.

For tool-based observation, initialize `store("observer-run", state)` once with the process
`session_id`, absolute `journal` path, unused `terminal_path` under `.build/` without newlines,
`cursor: null`, `backlog: false` and `terminal: false`. Bind that state to the assigned candidate/command and retain
it when resuming a cell or starting the next observation step. This single-run template executes
poll and delta projection in the same cell; use the assigned working directory. After a material
return, route progress to the designated recipient and alerts/terminal evidence to both roles,
then continue the same state through cleanup. If the outer call yields, use `functions.wait` on
that exact cell (up to 60000 ms within client limits); do not start another cell until it finishes.
If launch itself returns a terminal exit, save that full native launch object and its artifact first,
initialize `terminal: true` and drain projection without a process poll. Keep the accepted cursor
and stream identity with handles/evidence in any compaction handoff; do not initialize it again.

```js
// @exec: {"yield_time_ms": 60000, "max_output_tokens": 24000}
// observer-poll-template
const state = load("observer-run");
const quote = value => "'" + String(value).replaceAll("'", "'\\''") + "'";
if (!state.terminal && !state.backlog) {
  const native = await tools.write_stdin({session_id: state.session_id, chars: "", yield_time_ms: 55000});
  if (native.exit_code !== undefined && native.exit_code !== null) {
    state.terminal = true;
    store("observer-native-terminal", native); // Full object, before any artifact read.
    store("observer-run", state);
    const body = JSON.stringify(native, null, 2) + "\n";
    await tools.apply_patch("*** Begin Patch\n*** Add File: " + state.terminal_path + "\n"
      + body.trimEnd().split("\n").map(line => "+" + line).join("\n") + "\n*** End Patch");
  }
}
while (true) {
  let delta = {events: [], backlog: false};
  let observation_error;
  if (!state.projection_disabled) {
    try {
      const command = "python3 dev/project-run-events.py --max-bytes 8192 " + quote(state.journal)
        + (state.cursor === null ? "" : " --cursor " + quote(JSON.stringify(state.cursor)));
      const projection = await tools.exec_command({cmd: command, max_output_tokens: 24000});
      if (projection.exit_code !== 0) throw new Error(projection.output.trim() || "projection did not finish");
      delta = JSON.parse(projection.output);
      state.cursor = delta.cursor; // Advance only after accepting the complete projection.
      state.backlog = delta.backlog;
    } catch (error) {
      observation_error = error.message;
      state.projection_disabled = true; // Continue native polling; repair observation explicitly.
      state.backlog = false;
    }
  }
  store("observer-run", state);
  if (observation_error || delta.events.length || (state.terminal && !state.backlog)) {
    text({events: delta.events, observation_error, terminal: state.terminal,
      backlog: state.backlog, incomplete_bytes: delta.incomplete_bytes,
      native_terminal: state.terminal ? state.terminal_path : undefined});
    break;
  }
  if (!state.backlog) break; // Unchanged: no text/notify or second long poll.
}
```

Keep the full native object in the persistent store if writing its artifact fails; report that
failure to both roles and retrieve it without polling the terminal handle again. Projection errors
after terminal cannot replace its exit code or lose the object. Observation errors leave the journal
cursor unchanged and disable projection until explicitly repaired; continue the same runner's native
completion/cleanup. Full logs and native receipts retain authority. The template is an observation
step, not a runner, receipt writer or replacement controller. Its host-free checks are
`python3 -B dev/project-run-events-test.py` and `node dev/observer-poll-test.mjs`.

For parallel acceptance, read new `check-result` events/current receipt as well as process
completion. Report failed/blocked checks when observed, without waiting for `run-result`;
keep observing the same controller through completion and cleanup unless the assigned developer
directs an owned interruption. Then return the final report and end the bounded assignment.
Make the native final summary/evidence available to both developer and coordinator; they need
not rewrite it. Stopping an observer does not establish that its runner or lease has drained.

### Evidence handoff and cache cleanup

Before an expensive run, identify the smallest useful existing failure artifacts and which
will disappear with fixture/VM cleanup. Preserve their original bytes in the task's existing
private output directory before disposal, with bounded capture and restrictive access; hashes
and paths alone cannot recover deleted content. Keep secrets out of model output and tracked
files; return only safe bounded excerpts. Use the existing runner/fixture capture path rather
than another collector. A missing capture capability belongs to the owning fixture's repair.

On failure, save the native outcome first when available, then the minimal useful failure bytes
and safe excerpt with existing artifact paths, let runner cleanup/release finish, and return one
result. Capture from disposable storage must happen before it is deleted, even when the final
native outcome arrives only after cleanup. Preserve capture failure/refusal separately from the
original test exit; missing evidence stays incomplete and never authorizes a blind rerun.

Logs, receipts, native terminal objects and candidate bundles are temporary task artifacts.
Retain original records, launch bindings and complete logs while the task is active, including
failed, interrupted, blocked and incomplete outcomes and unavailable measurements. Keep sensitive
artifacts private; preserve safe redacted views for handoff. If retention would expire, copy the
needed bytes into the task's existing `.build` output directory and verify their hashes.
Never reconstruct missing artifacts. Copying records does not import a PASS onto another candidate.

After checks, review and runner cleanup, retain only concise key findings in the owning reference
or private overlay: the tested scope, source/candidate and baseline identities, outcomes, remaining
limits and any necessary reproduction steps. Do not commit task logs, receipts or bundles to Git.
Permanent log storage or copies outside the task cache are not required.

Then stop task-owned temporary helpers and remove only explicitly identified, completed task-owned
cache directories without asking for permission again. Confirm that their runners and leases have
finished and that no registered live
worktree or running application uses those paths. Preserve active sibling runs and shared build
outputs; never clear the whole `.build` as a handoff step. Keep original records until their review
and concise findings are complete. Cache cleanup does not require another VM acceptance run.

### Repair after a failure

Start with the observed native outcome and bounded failure excerpt linked to its full log.
Classify the cause as product, fixture or environment, then choose the smallest existing
diagnostic. State the hypothesis, owning files/check, expected difference and stop condition
before expanding reads. Fix the cause and run affected checks; batch independent repairs before
freezing a new candidate when needed. Before an expensive retry, verify the changed prerequisite
with its existing check and consult installed CLI help when invocation/compatibility is uncertain.
Record the concrete rerun reason in the current task, using existing evidence pointers.

Before cancelling an active check or repeating a passed one after a candidate change, identify
its affected runtime, fixture or shared inputs and check supported evidence import/transfer.
A new fingerprint alone is not a reason to cancel or repeat an unaffected check. If a required
gate cannot reuse that evidence, record that specific binding/coverage limit as the rerun reason.

Reassess a rebase or changed candidate by affected contracts. Reuse only supported, still-bound
results under the [candidate import/transfer limits](#one-release-candidate); supporting diagnostics
do not replace required fresh, continuous or full end-to-end gates.

### Report deviations from the guide

If execution or reporting differs from this guide, promptly notify the operator before the
final task report: expected vs observed behavior, safe command/check and run identity,
evidence paths, and impact on coverage, result confidence or cleanup.
After reporting and investigating, correct code or outdated docs within the authorized scope
and verify; affected acceptance stays unconfirmed until valid evidence exists. Do not conceal
the discrepancy or weaken gates. A correctly reported failure/capacity refusal is an outcome,
not itself a guide deviation.

## Run the core checks

`make test` runs the basic Go tests and profile host-free checks. Core Go tests use
`-count=1` so repository-inventory checks execute even when their package code is unchanged;
the Go build cache still applies. To run only the core Go tests, use
`TMPDIR=/tmp go test -count=1 ./cmd/... ./internal/...`.

`./tests/run.sh` checks the current source files, including uncommitted edits. No Git history,
clean checkout, base commit or `.git` directory is required. Install the tools listed in
[the test guide](test-vms.md), including Git and ripgrep: tests of Git behavior create their own
temporary repositories. The release packaging test uses a disposable copy of the current public
files because it adds fixtures; that copy needs no Git metadata.

`make test`, `./tests/run.sh`, the host-free profile runners and `dev/process-coverage.sh`
place disposable fixtures in `/tmp`, independently of a workspace-local `TMPDIR`, so Orca
cannot discover them in the working project. When invoking an individual test script or `go test` directly, use
`TMPDIR=/tmp`. Manual local reproductions must also create disposable Git repositories and
short-lived test worktrees under `/tmp`, outside managed workspace trees. `.gitignore` does
not prevent Orca from discovering nested Git roots, including those under `.build`.
Test worktrees must belong to fixture repositories.
Reserve `.build` for artifacts and logs; persistent development worktrees can remain there.
Representative local discovery tests share one Git directory across their checkout paths;
hook-observation tests use synthetic Git metadata. Ordinary Orca projects acceptance uses
small native fixtures; mass Git-root generation belongs only to the
[extended/manual diagnostic](#extendedmanual-diagnostics) on a leased disposable VM.

The runner prints one start/result line per check and a final `SUMMARY` with its
status, check count, elapsed seconds and original exit code. Successful check output
stays in separate logs under a unique `.build/test-runs/run.*/` directory. On the
first failure it stops, prints the full failed Go check output or the last 40 log lines
for other checks, and points to the complete log.
`RESULTS` and `SUMMARY` identify the run's `summary.tsv`, which has these columns:
`kind`, `suite`, `check`, `status`, `exit_code`, `duration_seconds`, `log`.
SIGINT and SIGTERM exit with 130 and 143 respectively; the active check and run are recorded as
failed with that status, rather than passed with partial output.
Log names are relative to that summary's directory. Each completed check has a
`check` row; the final `run` row reports the whole invocation. A missing final row
means the run did not finish reporting; absent checks have not passed. Durations
use Bash's elapsed seconds and may be zero for short checks. Separate invocations
never overwrite each other's results. Local logs stay until explicitly removed.
On failure, CI and Release upload the core logs and summaries plus the complete
`make verify` console output as `ci-host-free-<attempt>` or `release-host-free-<attempt>`
artifacts, retained for seven days. Read the summary first and open only the relevant
log when a check fails.

Host-free shell fixtures use a 30-second Incus agent deadline with margin for slow
CI scheduling instead of real VM boot deadlines. Tests of the deadline itself explicitly
select their intended timeout.
Keep fake responses current when shared lifecycle adapters gain new probes; include
captured fixture output and command logs when a fixture fails.

Profile-specific assertions live in `config/profiles/<name>/tests/`. Each shipped profile must own a
host-free `tests/run.sh` and, when it has live scenarios, `tests/e2e/acceptance.sh`. The shared
`dev/test-profiles.sh` runner enumerates every profile directory and checks all required entrypoints
before running any of them. A missing entrypoint is reported as incomplete and exits nonzero; a
profile can declare `tests/run.not-applicable` or `tests/e2e/acceptance.not-applicable` with a reason
only when that runner is mutually exclusive with the declaration. The reserved `default` profile is
the only built-in exception. An incomplete live runner keeps profile release acceptance incomplete.

Run all profile host-free checks with `bash dev/test-profiles.sh`, or invoke one profile's runner
directly. Core tests exercise generic APIs with synthetic fixtures; profile tests check their preset
and runtime through those APIs. Profile test directories are excluded from runtime release bundles.
For profile-owned live checks, use `bash dev/test-profiles.sh --e2e --slot N` or a profile's
`tests/e2e/acceptance.sh` directly, following the VM allocation guide below. The shared runner
forwards arguments; each profile owns its fixtures, assertions and lease cleanup. These profile
results are one part of release acceptance alongside core release smoke and compatibility checks;
no green subset implies the combined gate passed.

Moving checks into profiles changes ownership, not release coverage: release acceptance must
aggregate core and all shipped profiles' declared checks against the same candidate. Missing or
failed required profile checks leave acceptance incomplete; optional runtime selection does not
make a shipped profile optional to release verification.

### One release candidate

The same profile inventory is used by local acceptance and branch CI preflight.
Check prerequisites before packaging or acquiring a VM, then freeze the final candidate:

```sh
python3 dev/release-acceptance.py preflight --slots 1 2 3 --types subyard-pair android-test
python3 dev/release-acceptance.py prepare --version X.Y.Z --output .build/release-acceptance
python3 dev/release-acceptance.py run --output .build/release-acceptance --slots 1 2 3
```

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
regression after required local gates. Explicit `--exclude CHECK` leaves the required gate incomplete.

Select available slots using `dev/agent-e2e.sh --status`; each slot runs one controller at a time.
Source fingerprints use Git executable-bit semantics; snapshots normalize public file modes explicitly,
so the checkout umask cannot change the candidate. One plain source copy is retained for local
checks and controllers, with the runtime assets under its `.subyard-acceptance/release/` directory.
The output also retains the transport archive, logs and `receipt.json`; the intermediate source
archive is removed after extraction. Neither the source copy nor the VM payload needs a Git
repository or index. Every controller receives the same checksum-verified transport archive; profile fixtures
execute the packaged engine and runtime assets. The core smoke installs those assets through the
public updater, including reboot, rollback and roll-forward. Local verification, ShellCheck, real local adapters and updater compatibility precede physical checks.
Process coverage (`dev/process-coverage.sh`) is optional; ordinary process contracts remain in
`make verify`. Frozen receipts retain their recorded coverage requirement and command binding.
Run the aggregator from the managed workspace checkout: it supplies that workspace for lease
attribution while executing controllers and guest payloads from the frozen candidate.

A repeated `run` executes only checks without a passing result. Use `--only profile:NAME` or
`--only p0-release-smoke` for a remaining independent check; `--rerun` also repeats passed checks.
After capacity changes, inspect a new supplied snapshot with `preflight --output DIR`, then continue
the same frozen candidate with `run --output DIR --slots N --only CHECK`. Passing local checks remain
bound to that identity and are retained; capacity alone does not require another `prepare`.
Planning insufficiency is distinct from an actual typed broker refusal recorded as `blocked` with
`capacity_memory` or `capacity_disk`. Both leave required physical acceptance incomplete.
Profiles may declare independent checks in `tests/e2e/acceptance-lanes.json` with schema version 1
and ordered `lanes` entries containing `id` and runner `arguments`. The collector requires every
lane to pass and accepts `--only profile:NAME/ID` for one lane. Available approved slots take the
next queued check after their controller exits; a failed or blocked worker stops its queue.
A physical retry always allocates fresh VMs. It never resumes a fixture across leases. Source or
artifact changes reject reuse; prepare a new candidate in a new directory. Logs are retained per
attempt and include the controllers' source and environment evidence. The run checks its platform
and Go toolchain against preparation; receipts retain measured execution and available VM base identities.

`receipt.json` records terminal results, including `blocked` capacity refusal and `interrupted`
execution. `events.jsonl` retains starts, results, minute heartbeats and bounded controller phases;
phase durations use the controller clock. An unfinished phase has no successful end marker.
Fixtures may emit `E2E_PHASE phase=fixture/NAME state=start|end duration_seconds=N exit_code=N`
with a lowercase name of up to 60 letters, digits or hyphens. Start markers use zero duration and
exit code; end markers retain the measured duration and original exit code (0–255). The collector
projects only this bounded grammar into phase events, without copying fixture output. A successful
cleanup phase does not change a preceding failure into a pass.
Use `supersede --output DIR` to retire an idle candidate while preserving its original evidence.
Only one mutating collector may use an output directory at a time.

Explicit `import --output NEW --from OLD --check CHECK` copies passing command-bound evidence
between bound candidates with matching source and release assets, retaining inherited provenance.
Only provenance generation timestamps may differ; artifact identities and original metadata hashes remain bound.
Local evidence also requires its recorded execution environment to match.
`--allow-other-profile-tests` permits a profile check to survive changes confined to other shipped
profiles' test directories. Its runtime, assets, inventory, own profile and shared inputs must
remain identical. This exception never applies to local gates or continuous release smoke.

#### Reviewed evidence transfers

After reviewing changed inputs, transfer per-check evidence as inherited coverage:

```sh
python3 dev/release-acceptance.py transfer --output NEW --from OLD --check CHECK --review REVIEW.json
```

Both directories must be valid frozen candidates. The target check must still be pending;
failed, blocked, interrupted or already passed target attempts cannot be overwritten.
`REVIEW.json` has exactly `schema_version: 1`, `check`, `tested`, `target`,
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

Transfer retains and revalidates the review, original evidence and exact target. Receipts distinguish
`coverage.executed` from `coverage.inherited`; pending gates stay pending. Publication sends only
sanitized identities, hashes, command/environment provenance and authorization.

#### External checks and publication

The [external-check contract](reference/release-publication.md#external-checks) defines profile-owned
requirements, optional `not-run` results, actual PASS evidence and explicit required-check deferrals.
These do not replace required local or VM gates.

Keep receipts/logs local and follow the [cleanup policy](#evidence-handoff-and-cache-cleanup) before
removing them. For publication, use the [release flow](reference/release-publication.md), which
imports exact CI assets and submits bound acceptance without repackaging.

GitHub CI runs the full core gate, profile host-free checks, warning-level ShellCheck and
`bash tests/real-host/adapter-contracts.sh` in one `verify` job on every branch push and pull request;
release preparation and publication use explicit workflow dispatches, with no tag-triggered
publication. Basic native ARM64 engine build,
execution and Go tests run on GitHub's ARM runner in CI and gate Release publication. ARM requires
no local agent access or workflow authorization. Native Paseo uses the same branch/PR trigger
boundary, while Release builds its native artifacts independently.
The separate nightly/manual Deep CI runs repeated Go race tests and one minute of parser fuzzing.
Both workflows use standard Ubuntu runners, read-only repository permissions and no artifact uploads.
Veranda, native Paseo and live P0 acceptance remain separate checks.

Agent compatibility regressions run locally with
`go test ./internal/adapters/statusruntime` and `bash tests/codex-agent-provision.sh`.
They use temporary rules, fake CLI programs and release metadata; they do not start VMs,
contact model APIs or publish Git history. They cover the matcher invocation and failure
handling, latest-release installation and preservation of a working binary after a bad download.
Mocks do not establish compatibility with a real CLI release. Check its native `execpolicy check`
against the shipped rules for that evidence; real client approve/deny needs separate acceptance.

## Run Veranda host-free checks

Install Python 3 and the current stable Rust toolchain selected by
[`rust-toolchain.toml`](../rust-toolchain.toml), then run from the repository root:

```sh
make verify-veranda
```

The target runs `python3 dev/check-veranda.py`; override the interpreter with
`make verify-veranda PYTHON=python` when needed. CI runs the same entrypoint with
`python` and current stable Rust. The native client has no npm/WebKit dependencies.
The entrypoint explicitly selects `+stable`, including when `RUSTUP_TOOLCHAIN` is set.
The target and entrypoint do not install toolchains. `make verify` retains its
separate core and shipped-profile scope.

Checks stop at the first failure: runner temporary-path contract, Linux resource
probe isolation and native Rust client tests with `--locked`.
Python fixtures use the entrypoint's own
interpreter. `python3 dev/check-veranda.py --rust-only` selects only native tests.
On Linux that suite runs once with a child-only `TMPDIR` alias to a private real
directory under an owned `/tmp` fixture. This exercises symlink temporary paths
without mutating the parent environment or parallel Rust tests' global state;
cleanup failure fails the check. Non-Linux runs explicitly skip the Linux probes.

A local Linux pass does not verify native ARM, macOS or Windows; those remain
separate CI matrix results. Desktop launches, real local/SSH owners, package
installation/upgrade/rollback and physical resource acceptance remain separate
[Veranda delivery gates](../veranda/README.md#delivery-gates).

Before a long Veranda VM scenario, use the same build in the target environment for
preparation, launch, one real product action, normal closure and cleanup. Verify actual
API responses and UI states. On the first failure, preserve the result, distinguish fixture
and product causes, repair the cause and repeat the short check before the long scenario.
Check nonzero executed test counts and calibrate the producer's actual offered, dispatched
and completed load. Reuse unaffected passes and retain the original acceptance criteria.

## Extended/manual diagnostics

The Orca load diagnostic is an explicit manual check, separate from `tests/run.sh`,
profile host-free runners, profile acceptance lanes and release acceptance. Ordinary
missing-record regression uses a small representative fixture. The large workload
of 1,000 or 1,400 Git roots and 100,001 flat files belongs only to the diagnostic's
leased disposable VM; never generate it in a live controller checkout or its `.build`.
Local `.build` directories may retain numeric results and sanitized logs.

Run the [dedicated controller](test-vms.md#extendedmanual-orca-load-diagnostic)
explicitly when measuring Orca catalog behavior. Passing ordinary tests does not prove
load capacity, and a diagnostic timeout or interrupted run is incomplete evidence.

## Choose the VM allocation

Android acceptance runs on one `android-test` VM (8 GiB RAM / 40 GiB disk). Ordinary full P0
uses the standard `subyard-pair` (two 4 GiB / 20 GiB guests) for its two-host scenarios. It does
not recursively run the separate physical broker pool diagnostic. Do not request two 16 GiB
guests for Android or ordinary full P0.

`--vm 1` only selects where a command runs; it does not turn a pair allocation into a singleton.
Use `--vm-count 1` for one standard VM; `--type android-test` retains the larger Android
singleton. Commands and exact coverage are documented in
[the VM guide](test-vms.md#agent-workflow).

## Evidence tiers

| Tier | Evidence |
| --- | --- |
| T0 | An exact regression test for the defect or failure mode. The engineer or agent defines it from the observed failure. Target: at most 60 seconds. |
| T1 | Affected host-free package, race, shell, CLI, frontend, or Rust checks. Typical target: at most 3 minutes; broader owning checks may take longer. |
| T2 | The full host-free suite, `./tests/run.sh`. Veranda retains its separate host-free checks. |
| T3 | Existing targeted E2E lanes or real-host checks for affected physical boundaries. |
| T4 | A fresh full P0: `dev/e2e/p0-acceptance.sh --slot N --lane full`. |

A fresh full pass also satisfies its contained T3 lanes: smoke, boundary, transport, nested teardown,
real Incus, release, source upgrade, power/systemd (including reboot verification), peer and cleanup.
Cold dependencies, profile-resource's bind fixture and Orca acceptance are not covered by that matrix.
See the skill for when local, VM and publication checks apply; the tiers describe evidence scope,
not a ladder that every change must climb.

## Continue work across leases

Follow the skill's [lease evidence rules](../.agents/skills/subyard-dev-flow/SKILL.md#test-progress-across-leases)
and the [VM guide](test-vms.md) for disposable targets and lane commands.
