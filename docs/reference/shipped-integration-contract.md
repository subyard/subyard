# Shipped integration contract

Shipped coding integrations extend the existing package loader, operation-local
configuration catalog and `IntegrationPlan` / `ApplyIntegrations`. They do not
introduce another selection resolver, planner, registry of operator executable
code or desired-state store. Environment profiles retain their own selection and
`profile.Load` remains profile-only.

An optional `config/agents/<id>/integration.json` uses schema version `1`, or
version `2` for declared artifacts. Version 1 cannot declare artifacts. The
common shipped loader rejects unsupported versions, unknown fields, duplicate
names, unsafe or symbolic-link source paths and setting collisions. Integration
IDs retain the directory name and existing `AGENT_<id>_*` metadata contracts.
Package declarations own immutable typed inputs, scalar settings, existing agent
metadata, lifecycle sources and dashboard metadata. Immutable input values cannot
be replaced by persistent settings, command overrides or ambient environment.
Their bytes and values bind package identity and operation scope.

Schema 2 `artifacts` declare config and instruction delivery. The same resolver
serves CLI config assets, runtime, prepared execution and release activation.
Config sources retain file-setting import/edit and `AGENT_<id>_CONFIG[_DEST]`
overrides; instructions reference an optional integration-owned `absolute-path`
scalar. Existing host-file path settings may serve as compatibility references
without being redeclared. Config-status scope excludes instructions. Raw JSONC
stays raw at its default `.jsonc` destination even when an imported source has a
different filename. The `destination` format policy retains legacy JSON/TOML
ownership when CONFIG_DEST changes. See the
[artifact decision and safety contract](integration-artifacts.md) for schema,
precedence, capture, permissions, preview, adoption and rollback rules. Remaining
recommendation, credential and persistence boundaries are documented in
[consumer compatibility](integration-consumer-compatibility.md).

An optional `native` array reuses the shipped profile package/path declaration
for package-owned Go executables. Development uses `.build/agents/<id>/`;
runtime bundles contain them in their owning integration directory. Actual
executable bytes enter the source binding. Plain source copies need no Git
metadata; missing helpers never cause an implicit build or source-hash fallback.
See the [typed observation pilot](typed-semantic-observations.md).

Settings reuse the shipped profile setting contract. A port may declare
`default_from` with a source port setting and integer offset. References must
exist, have port type and be acyclic. For bounds `minimum..maximum`, the default
is `minimum + (source + offset - minimum) % (maximum - minimum + 1)` with a
nonnegative remainder. Explicit values win. Selected integration listeners
participate in the existing owner collision and stopped-peer reservation rules.

A lifecycle declares an executable package-relative handler, its source files,
and native instance configuration keys and device names that it owns. Each
excluded native key/device must enter the handler's actual observation; unrelated
native scope remains bound by the core planner. Sources, inputs, selection and
desired fingerprints must stay unchanged before effects.

The handler accepts `observe`, or
`apply OPERATION_ID ACTUAL_SHA256 DESIRED_SHA256`, with the prepared engine
context and `SUBYARD_INTEGRATION_SELECTED=0|1`. Observation is read-only and
returns one bounded, strictly decoded JSON object containing `state`, `actual`
and `desired`. `current` carries equal lowercase SHA-256 values; `stale` carries
the exact native actual fingerprint and a different desired fingerprint.
`absent` is permitted only for a deselected integration with no owned native
state and carries no fingerprints. A selected missing installation is `stale`.
An installed deselected integration is `current` only when its owned startup and
publication are retired; binaries and user data may remain. Foreign or ambiguous
ownership is an error, never an absent state. Stopped yards are deferred by core
and are never started by observation.

Core reuses the existing bounded shipped-hook execution and cancellation
boundary, adds each observation to the existing exact integration facts, checks
the captured assessment before apply, and independently observes the result.
Apply must itself reject changed actual or desired input before effects. A
successful apply report alone does not establish convergence. Generic legacy
`PROVISION`/`CHECK` hooks continue to work; a lifecycle package owns its complete
provision/retire sequence. `HEALTH` remains advisory and cannot enter activation
fingerprints or prevent activation while a service is starting.

Failing handlers may emit the optional closed lifecycle diagnostic record
documented in the typed observation contract. Core renders fixed validated
yard/component/phase context through ActivationDiagnostic; arbitrary handler
stderr remains private. Historical handlers retain a fixed generic failure.
Success observation JSON remains unchanged.

A failed handler may also emit exactly one optional terminal probe detail:
`subyard-readiness-diagnostic-v1 PHASE PROBE CATEGORY ELAPSED_MS BUDGET_MS`.
It must accompany one valid lifecycle diagnostic with the same phase. Generic
probe labels are instance-query, owner-address, enumeration, service-state,
service-window, recovery-enumeration, inspection, ownership, installation and
native-producer. Categories are transport-timeout, os-error, native-exit,
native-timeout, invalid-schema, dependency-missing, native-rejected,
startup-deadline, service-inactive, service-failed, service-deactivating and
service-unknown. Decimal millisecond values are canonical nonnegative integers
bounded by 1320000; the record is at most 160 bytes. Core renders validated
terminal detail through the existing operation diagnostic. Missing, malformed,
duplicate or inconsistent detail retains the original fixed lifecycle reason.
No native output, arguments, environment or configuration values are public.
At the execution boundary, handler-deadline, caller-deadline and caller-cancelled
identify the aggregate budget or caller context, with bounded handler duration.
These details preserve the original error classification and retry command.

Integration observation has a 120-second aggregate handler limit and a 64-KiB
limit on each stdout and stderr stream. The aggregate includes bounded startup
readiness waits, native probes, transport and projection; individual limits
cannot all consume their full allowance. Profile observation retains its
30-second limit. Caller deadlines and cancellation take precedence. Cancellation
terminates the handler's process group, including native child commands.
Integration apply has a fixed 22-minute limit covering the package's 600-second
image pull, guest installation, bounded recovery and route verification. A
deselected apply may verify `absent`; its plan fact retains desired absence through
retirement while privately binding the captured native actual and desired fingerprints.

Dashboard metadata supplies scheme, path, configured owner port, guest port and
optional container route receipt/device names. Core renders direct owner routes
and local/remote SSH tunnels from that metadata. Existing route receipt versions,
pending recovery, foreign-device guards and persisted state remain compatible.
Concrete package tests and physical acceptance belong to the integration;
compatibility entrypoints preserve the existing release coverage aggregation.

## Retired package compatibility

Retirement-only declarations live under `config/retired-integrations/<id>/integration.json`.
Their `retired` flag and `retired_settings` names normalize historical configuration inputs
without mutating authored files or enabling another integration. A package may list its own
`AGENT_<id>_*` settings there. Unknown unrelated IDs still
fail validation. These declarations have only a bounded cleanup lifecycle; provisioning,
image/version inputs, settings, dashboard services and native executables are prohibited.
The existing assessed integration lifecycle binds cleanup ownership, source and actual state.
Protected predecessor transitions retain their sealed source authority; cleanup does not
rewrite an old in-flight plan.

The Observer compatibility package verifies exact predecessor routes, local ownership,
managed control hashes and the labelled native container before disabling or removing them.
A root-private control-hash receipt permits interrupted deletion to resume. Databases,
session roots and credentials are outside its deletion inventory.

A retired package's lifecycle observation may carry an optional public `notice`, bounded to 440
characters without control characters. Active packages may not report one, and it stays outside
the plan binding. On a `stale` observation it states the removal, which appears as a plan
consequence and as one warning after a successful apply, because an unattended update does not
print the preview. On `absent` it reports retained state as a persistent warning.

A stopped guest cannot report its state, so a retired package may also declare a static
`retirement_notice` with the same limits, and it is the retirement text of a stopped yard's plan.

### Retirement proof

`user.subyard.retired_integration.<id>=true` is the host-readable proof that a yard selecting this
retired package has been observed at its desired state. Core publishes it at the end of integration
apply, after that apply's own observation found every lifecycle at its desired state: `current` or
`absent`, including a kept foreign unit and a guest that was already clean. A failed or interrupted
apply publishes nothing. The key is reserved from profile publication and excluded from the
integration plan binding and the legacy-continuation binding, because it is written after approval.

Without proof, a selected retired package with a lifecycle changes two init stages:

- On an intentionally stopped yard with disk-backed `/tmp` already complete, the instance stage
  is pending. Its existing guarded temporary start runs after confirmation, the plan shows that start and the retirement, the provision stage
  retires the package and publishes the proof, and finalization restores stopped. A repeat init
  with the proof does not boot.
- On a running yard the provision stage is pending once, including for an already clean guest, so
  that a later stop needs no boot.

If the boot or the retirement fails, the yard has no proof and stays running with its fenced
`desired=stopped` until a retry of `yard init` finishes and stops it.

For pending stopped-yard work, run `yard -Y NAME start`, then `yard migrate`. Start completes any
pending disk-backed `/tmp` policy; migrate retires the package and repairs the running guest.
When disk-backed `/tmp` is already complete, `yard -Y NAME init` is also supported: it temporarily
starts the yard, retires the package and restores stopped power. Until proof exists, host-only
update warnings and stopped integration status name the applicable sequence or init; after a
successful start the reminder names `yard migrate`. These reminders read no guest state.

Known limits:

- `ALLOWS_CODING_TOOLS=false` has no integration reconcile, so it neither boots nor warns.
- Selection decides the warning and the boot. A stopped yard whose settings no longer select the
  package gets neither, even with an enabled unit; a later init on the running yard still retires it.
- Proof published before a rollback survives it. After rolling forward, a stopped yard reinstalled
  by the old release stays silent and is not booted. A running yard still observes its guest.
- A materialized activation whose runtime lacks the selection publishes no proof. The next init on
  that yard does, with one extra temporary boot if it is stopped by then.

The Paseo compatibility package removes its service from every declared unit location, including
foreign units and drop-ins, the helper commands, `/opt/subyard/paseo`, the receipt and
`/srv/agents/paseo`. It requires no ownership proof. Only a non-empty real `worktrees/` directory
inside that state directory survives, with one warning naming `/srv/agents/paseo/worktrees`.
All other state, including chats, schedules, loops and uploads, is removed. A second observation
with only those worktrees left changes nothing.

Removal never follows a leaf or ancestor symlink outside the fixed paths and refuses mounted
removal targets. Genuine removal failures remain errors and partial removal is safe to retry.
A stopped yard stays unchanged during an update. Run `yard -Y NAME start`, then `yard migrate`
to finish pending disk-backed `/tmp`, remove Paseo and repair runtimes. If disk-backed `/tmp` is
already complete, `yard -Y NAME init` can instead temporarily start it for retirement and restore
stopped power. Start alone does not retire Paseo. See [Retirement proof](#retirement-proof).
To use Paseo, install an older release.
