# Shipped integration contract

Shipped coding integrations extend the existing package loader, operation-local
configuration catalog and `IntegrationPlan` / `ApplyIntegrations`. They do not
introduce another selection resolver, planner, registry of operator executable
code or desired-state store. Environment profiles retain their own selection and
`profile.Load` remains profile-only.

An optional `config/agents/<id>/integration.json` uses schema version `1`. The
common shipped loader rejects unsupported versions, unknown fields, duplicate
names, unsafe or symbolic-link source paths and setting collisions. Integration
IDs retain the directory name and existing `AGENT_<id>_*` metadata contracts.
Package declarations own immutable typed inputs, scalar settings, existing agent
metadata, lifecycle sources and dashboard metadata. Immutable input values cannot
be replaced by persistent settings, command overrides or ambient environment.
Their bytes and values bind package identity and operation scope.

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
without mutating authored files or enabling another integration. Unknown unrelated IDs still
fail validation. These declarations have only a bounded cleanup lifecycle; provisioning,
image/version inputs, settings, dashboard services and native executables are prohibited.
The existing assessed integration lifecycle binds cleanup ownership, source and actual state.
Protected predecessor transitions retain their sealed source authority; cleanup does not
rewrite an old in-flight plan.

The Observer compatibility package verifies exact predecessor routes, local ownership,
managed control hashes and the labelled native container before disabling or removing them.
A root-private control-hash receipt permits interrupted deletion to resume. Databases,
session roots and credentials are outside its deletion inventory.
