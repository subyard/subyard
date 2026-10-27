# Local agent and resource collection

The optional `telemetry` profile owns a local, independently runnable collector.
OpenLIT remains disabled. Collection does not require an analytics UI, ClickHouse,
Redis, a provider account, or a SQL server. The control plane owns only generic
resource observation contracts; agent interpretation and SQLite writes belong to
this profile.

## Collection and durability

The collector embeds the official OpenTelemetry OTLP receiver for HTTP/protobuf
logs, metrics and traces. A minimal Collector comparison uses the same policy and
SQLite sink. The embedded receiver avoids the extra service/factory/configuration
assembly for this single sink and used less RSS in the frozen comparison.
Measured conditions, binary identities and limits are recorded in the
[owning guide](../telemetry.md#receiver-packaging-comparison); binary size and
memory limits are not runtime overhead.

Capture policy runs before any durable write. The default excludes log bodies,
prompt/response/tool content, arbitrary status text, command lines, environment
capture and authentication headers. Attribute policy applies to resource, scope,
records, points, exemplars, events and links. Configurable safe metadata is kept
within that policy; credentials and lease capabilities are never telemetry data.
Sanitized complete OTLP ExportRequest protobuf retains supported structure and
optional-field semantics, rather than flattening usage into model/token/cost rows.
Source and receive times, policy and source versions remain distinct.
Export/replay preserves the original capture-policy fingerprint separately from
the trusted local import-policy fingerprint. Imported policy claims cannot relax
sanitization. SQLite schema 2 transactionally migrates version 1 stores while
read-only legacy export leaves them unchanged.

One bounded writer batches requests into SQLite WAL transactions with
`synchronous=FULL`. OTLP acknowledges only a completed commit. Capacity or storage
failure is retryable; no memory-only admission is success. Requests are atomic:
the official receiver's consumer interface has no downstream partial-success
counts, so the collector never commits part of a request and rejects the whole.
Field redactions are counted as policy actions, not undisclosed record loss.

Raw rows have stable origin/store and event identities. Export/replay uses those
identities, never payload hashes. Identical legitimate events remain separate.
Native OTLP has no universal delivery ID: an acknowledgement lost after commit
can produce another delivery. Native source IDs are retained where available;
ambiguous retry duplicates are not advertised as exactly-once usage totals.

## Internal record contract

Profile-owned local producers write versioned sanitized records through the
collector's private control endpoint or its bounded durable outbox. The collector
owns only its outbox; it never acknowledges or deletes broker/owner journals.
Run snapshots reserve bounded count, total bytes and per-file space before a
new execution scope is created. Capacity failure keeps default native execution
with partial/unavailable coverage; explicit strict tracking refuses before exec.
Observer restart processes legacy overflow in bounded batches and reports
interrupted reservations as gaps rather than permanently refusing the directory.

```json
{
  "schema": "subyard.telemetry.record/v1",
  "source_id": "stable-producer-identity",
  "event_id": "unique-source-event-id",
  "source_version": "producer-version",
  "source_time": "2026-10-24T00:00:00Z",
  "kind": "run",
  "body": {}
}
```

Kinds cover run lifecycle, session/run relations, execution-scope and process
identity, samples, allocation lifecycle, attribution and gaps. The owning code
defines each body and rejects unsupported versions. Producer event IDs survive
retry/replay. Receive time belongs to the collector. Monotonic values identify
their host/boot clock domain and are never compared across owners.

Run records distinguish `agent_run_id`, agent type/version, native session ID,
parent run, task, project/worktree, yard and owner host. Samples belong to measured
objects once; temporal attribution edges carry source, quality and interval.
Unknown is not zero, and a shared backend is not independently charged to every
session. Raw OTLP and resource records remain the reconstruction source.
Read-only run inspection offers both the run's own links and an explicit
`parent_run_id` descendant view. The latter selects objects once across the tree,
including broker allocations; parent-inclusive counters suppress overlapping child
detail. It does not infer children from native sessions, PIDs or project names.

## Process and resource boundaries

Managed runs create their measurement boundary before agent exec. Full accounting
requires safe cgroup v2 delegation within inherited resource ceilings. A manager
outside the measured subtree holds the group until descendants finish and final
counters are durable. Agent exit and resource completion are separate events.
The launcher prefers an existing caller-owned delegated ancestor, so finite
common-root limits continue to bound the combined workload. It verifies write
permission at the migration common ancestor and refuses to escape narrower
caller-leaf limits. A separate observer service's delegation alone does not grant
permission to move unrelated session processes. The owner of an external delegated
boundary must keep it alive through final sampling; forced removal is a gap.
Restricted managed launches use explicitly partial read-only discovery. Existing
unregistered processes are not adopted by PID/name heuristics; their native
events can be retained without a verified run association.
Process identities include host/yard, PID namespace, boot ID, PID and start time;
cgroups and resource instances have separate generation identities.

CPU is a cumulative counter, memory a gauge, and kernel peaks differ from sampled
maxima. Block I/O differs from logical character I/O. CPU deltas, coverage gaps,
counter resets and memory-byte-second estimates retain their semantics. Parent
inclusive groups, child groups, QEMU, owner VM usage and guest detail are not added
as independent expenditure.

Broker acquire correlation IDs retain their existing meaning. Local caller hooks
associate them with inherited `agent_run_id` outside strict legacy requests.
Android opts into the generic resource `CONTEXT_ENV` declaration for its run ID
and data-directory metadata. The dispatcher forwards those bounded values only
to the handler process; it does not add agent semantics to the control plane or
change lease envelopes.
Held-acquire registration is a bounded, private durable intent before observer
lock acquisition. The observer installs generation-validated mappings and replays
retained events for new mappings; canonical event identities prevent replay from
adding another measurement. Transport credentials stay in private operational
configuration, outside telemetry records and export.
Owners expose separately advertised bounded read-only lifecycle/counter reads;
they derive targets from validated allocation ownership, never caller paths or
arbitrary Incus/Docker commands. VM identity includes owner, slot,
`resource_generation` and `lease_epoch`; Android identity includes owner, slot and
generation. Sampling has an independent persisted cursor and never consumes the
host sink's acknowledgement state.

Boundary observations precede destructive cleanup. Lost final tails remain gaps.
Expiry ends authority, not evidence that a worker stopped. Draining/orphaned
resources remain visible after agent exit; stale owners do not become zero usage.
Reservations and actual measurements are separate: reserved vCPU-seconds and
memory-byte-seconds are not actual CPU or resident memory.

## Lifecycle and migration

The profile owns its data directory, private endpoints, bounded service and native
exporter leaves. Collector processes stay outside measured agent scopes. Optional
collection failure cannot fail an agent command or resource lifecycle operation.
Native exporter ownership is acquired only after the disabled OpenLIT owner
relinquishes its managed leaves. Foreign edits are preserved/refused. Existing
agents are not killed to reload settings; OpenLIT/ClickHouse history is retained.
Enabling and deployment are operator actions after development acceptance.

Nested devcontainers use an explicit private mounted socket/limited loopback
forwarder or their own standalone collector; no automatic public listener,
privileged container, host PID namespace or writable host cgroup mount is needed.

## Sources

- [Usage Visibility requirements](https://github.com/subyard/usage-visibility/blob/master/docs/decisions.md).
- [OTLP specification](https://opentelemetry.io/docs/specs/otlp/).
- [SQLite WAL](https://sqlite.org/wal.html) and [synchronous modes](https://sqlite.org/pragma.html#pragma_synchronous).
- [cgroup v2](https://docs.kernel.org/admin-guide/cgroup-v2.html) and [proc](https://docs.kernel.org/filesystems/proc.html).
- [systemd resource control](https://www.freedesktop.org/software/systemd/man/latest/systemd.resource-control.html).

This reference describes implementation contracts, not a claim that every
physical acceptance case has passed. The active task owns remaining work.
