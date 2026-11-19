# Local agent and resource telemetry

The optional `telemetry` profile collects native OTLP logs, metrics and traces,
managed process measurements and broker resource records into local SQLite.
It does not require ClickHouse, Redis, a provider account or a database
server. The [data contracts](reference/agent-telemetry.md) explain identities,
durability and attribution. This is a collection and export layer; it does not
estimate provider compute, score agents or calculate billing.

## Enable and migrate

On the owner host, select an existing running yard:

```sh
yard -Y default profile enable telemetry --yes
yard -Y default profile status telemetry --json
```

These are operator actions; building and testing the profile does not enable it
on an existing deployment. The profile installs separate collector and delegated
run-manager services. Data lives in `/var/lib/subyard/telemetry`, owned by the
yard's developer account with directory mode `0700`, active files `0600` and
archive files `0400`. The service listens on `127.0.0.1:14318`, authenticates
native exporters and provides a private Unix control socket. The receiver's
bearer file lives at `/var/lib/subyard/telemetry-private/auth.token`, outside
the telemetry data directory. SQLite and the endpoint are not published on
the owner host.

Incus images can apply a systemd policy to every service. The profile validates
the effective command, identity, stop policy and resource limits, and uses
`setpriv --no-new-privs` to preserve process isolation under that policy. Readiness
checks the running processes as well as unit files. Foreign unit overrides are
refused; interrupted startup changes can be retried without terminating agent
workloads.

Telemetry writes whole policy files for selected coding-tool integrations. Both are
`0640`, owned by root and readable by the developer's primary group.

- Claude reads an `env` block from `/etc/claude-code/managed-settings.d/60-subyard-telemetry.json`.
- Codex reads the HTTP endpoint, protocol, Authorization header and an empty TLS table for
  each of logs, metrics and traces from `/etc/codex/managed_config.toml`. Codex merges this
  layer recursively. Lower layers can still supply extra headers and TLS fields. A competing
  gRPC exporter can make the merged definition invalid, and `otel.headers_helper` can replace
  static Authorization. Codex system requirements can replace the entire exporter definition.
  The Codex provisioner owns and verifies the requirements file, while telemetry has its
  own enable and disable lifecycle.

The policy overrides user values in the fields it defines while telemetry is enabled.
Telemetry refuses a foreign file at either managed path on enable and reports fields where a
configured Subyard template disagrees with the policy. It compares the two inputs
without reading the home document.

The one-time upgrade from v0.30 restores recorded prior values where telemetry settings
are unchanged, preserves later edits and removes leaves containing the exact collector token.
Rewritten home documents retain at most owner read and write permissions. A home file the
receipt never covered is only scanned for the token bytes. The upgrade keeps the receipt if
any resulting home file still contains them. The collector token stays the same; a missing
token file is restored from the token every recorded header names. Subsequent telemetry
updates and disable operations leave home documents alone.

Rolling back to v0.28–v0.30 leaves both system policy files behind if they were installed.
Those releases can also reintroduce the collector token into home documents, where an
older template writer can publish it with mode `0644`. If an upgrade stopped while it was
replacing collector assets, finish it before rolling back: those releases refuse a collector
asset that the inventory does not record. Process environment settings such
as `OTEL_EXPORTER_OTLP_*` and `HTTP_PROXY` can bypass the managed policy. Claude ignores
local drop-ins when server-managed settings apply.

Data, services and routes from removed integrations remain untouched. Already-running
agents continue with their existing configuration; restart them normally when collection
should begin. No running agent is killed to reload exporter settings.

Disable collection through the same profile family:

```sh
yard -Y default profile disable telemetry --yes
```

Disabling removes the owned policy files and retains the database.
The run manager drains existing observations without terminating agent workloads.

## Managed runs

Inside the selected yard, launch the native command explicitly:

```sh
subyard-telemetry-run --agent codex --project-id example --task-id change-42 -- codex
subyard-telemetry-run --agent claude --project-id example --task-id review-42 -- claude
```

The native command retains its terminal and exit status. Each invocation has a new
`agent_run_id`; it is not the native conversation/session ID. A managed Codex run stays out of
the [shared daemon](configuration.md#where-codex-runs), so its tools carry its own identity. The launcher passes
that context to descendants and optional broker correlation hooks. Codex receives
an explicit OTLP header; Claude receives an OTLP resource attribute. Native session
attributes bind sessions to the registered run when received. The receiver accepts
registered resource run attributes as well as the explicit HTTP header, including
when native settings override the launcher's inherited environment. No matching by PID,
cwd or nearby timestamps is used to invent that relationship.

Android's resource definition explicitly declares the run ID and data-directory
environment names through the generic `CONTEXT_ENV` contract. The dispatcher
preserves only that declared context for the handler, so ordinary `yard emu run`
can register the same managed run without changing native lease requests.
Direct Android broker invocations inherit the same context from the launcher.

On GNU/Linux, the shipped Codex provisioning hook also reconciles the system
`bubblewrap` dependency used by Codex's tool sandbox. Codex readiness runs
`bwrap --version` as the developer account before accepting the installation. On
affected hosts, yard provisioning also probes actual user and network namespace creation.

`--agent-version` can record a known version. Task, project, worktree, yard and host
identifiers are optional explicit metadata. Nested managed launches inherit their
parent run context. Ordinary direct `codex` and `claude` invocation remains
available; native telemetry without a verified run binding stays unattributed.

The default scope mode is `auto`. Full subtree accounting requires a verified
cgroup v2 delegation that preserves the caller's inherited limits. If unavailable,
the command runs with explicit partial `/proc` coverage. `--scope-mode require`
refuses before starting the command when safe placement or bounded durable run
tracking cannot be established; `--scope-mode partial` requests the fallback.
Default launches preserve the native command on telemetry-capacity failure and
report partial or unavailable coverage. Sampling defaults to five seconds. The collector and
run manager stay outside measured workload groups. The run manager observes
both fully managed scopes and partial runs.

Full accounting uses an available caller-owned delegated ancestor. Its finite
CPU, memory, I/O and process ceilings still apply to the whole subtree. Permission
to write a separate observer service's cgroup alone does not permit migration
from an unrelated SSH/session group. Such a caller retains partial coverage;
`require` rejects placement before the native command starts.

Where an existing systemd user manager supplies the required controllers, start
a new service boundary before the workload exists:

```sh
systemd-run --user --service-type=exec --collect --wait --pty \
  --property='Delegate=cpu memory io pids' --property=DelegateSubgroup=caller \
  --property=ExitType=cgroup \
  bash
# Inside that delegated shell:
subyard-telemetry-run --agent codex --scope-mode require --project-id example -- codex
```

`DelegateSubgroup` requires systemd 254 or newer. Controller availability and
delegation permissions are checked at runtime. Keep the delegated shell alive
until the manager records resource completion, including background descendants.
Forced removal of that outer boundary can lose a final counter tail and produces
a gap. This service does not move an existing limited SSH workload into a sibling
group. See the
[systemd delegation contract](https://systemd.io/CGROUP_DELEGATION/) and
[kernel migration rules](https://docs.kernel.org/admin-guide/cgroup-v2.html#delegation-containment).

A cgroup counter includes short-lived children between sampling ticks. Process
rows describe only observed members and use boot/PID-namespace/PID/start-time
identities. When `/proc` namespace access is denied, individual process detail
is unavailable; accessible cumulative cgroup counters still cover that subtree.
Observed process counts do not claim an exact count of every process created.
The manager continues after the native parent exits, including
background descendants, and retains the group until a durable final observation.
A real empty-group counter snapshot is taken before the startup gate opens;
the initial interval is not discarded just because the command finishes before
the first periodic tick. This baseline survives producer retry and manager restart.
A missing final boundary is a gap. Partial discovery cannot recover children that
exited before observation or reconstruct past cgroup membership.

Use `--shared-backend` for a managed server serving several native sessions.
Its usage remains shared; sessions are not assigned equal CPU or RAM shares.
Unrelated Gradle daemons, builders and broker overhead are not silently charged
to the foreground agent.

## Read-only inspection

Run as the data-owning developer account inside the yard:

```sh
collector=/usr/local/lib/subyard-telemetry/subyard-telemetry
data=/var/lib/subyard/telemetry
"$collector" stats --data-dir "$data"
"$collector" show-run --data-dir "$data" --run RUN_ID
"$collector" show-run --data-dir "$data" --run RUN_ID --include-children
"$collector" query --data-dir "$data" --sql \
  'SELECT object_id,source_time,cpu_ns,memory_bytes,rss_bytes,io_read_bytes,io_write_bytes FROM sample_values ORDER BY source_time LIMIT 100'
```

The `stats`, `query`, `show-run` and `export` readers open SQLite with
read-only/query-only restrictions and select the reader for the stored
`PRAGMA user_version`. `stats` reports `schema_version` and
`store_generation_id`. Queries have finite row, response-size and time
limits. The installed `subyard-telemetry` skill provides these commands
for agents. A native SQLite limit rejects scalar values and encoded rows above
16 MiB before result materialization; this is not a total SQLite working-memory
budget for arbitrary SQL. Large histories must be inspected in bounded windows rather than
held in an indefinite WAL reader transaction.

### Pre-upgrade archive

On the first upgrade to spool delivery, the profile checks for archive
collisions, gates legacy launch and broker entry, then stops the collector,
run manager and identified detached legacy observers. Agent workloads keep
running. A released launcher already in memory may miss a launch or final
sample as it crosses this cutover.

A private receipt lets the installer resume after interruption. The profile
fences the old outbox, checkpoints the schema 1–4 database and publishes it
read-only at `/var/lib/subyard/telemetry/archive/telemetry.sqlite`. An occupied
archive is preserved and the next archive uses `archive-2`, `archive-3` and so
on. The active schema 5 store starts empty with its own generation ID. Pending
legacy outbox records and health markers stay in retired storage. A disabled
collector finishes the store transition when enabled. Fresh installs and
compatible spool upgrades skip legacy quiesce.

Each identifiable unfinished historical run gets an ordinary
`interrupted_by_upgrade` gap with `missing=final_tail` in the archive. The gap
appears in `show-run`, export and replay. The archive retains historical rows,
their identifiers and their sequences. Each store has its own export cursor.

```sh
"$collector" stats --data-dir "$data/archive"
"$collector" query --data-dir "$data/archive" --sql \
  'SELECT agent_run_id,source_time FROM runs ORDER BY source_time DESC LIMIT 20'
"$collector" show-run --data-dir "$data/archive" --run STORED_RUN_ID
"$collector" export --data-dir "$data/archive" --after 0 --limit 1000
```

Use the store's own identifiers and export cursor for each database. Keep
archive exports private under the previous release's capture rules. Schema 5
stores the schema version and generation ID in the initialization transaction.
The first spool cutover is the implemented archive transition. General rotation
is deferred. A future transition must checkpoint WAL and drain
or transfer committed spool offsets and loss revisions before a new store
consumes surviving segments. The logical JSON/protobuf export stays independent
of the physical storage codec, with bounded decoded sizes and SQL access to
fields used for projections and retention.

`show-run` returns sessions, child runs, processes, temporal object links, gaps and
separate `local_direct`, `local_inclusive`, `broker_actual`, `shared` and `unknown`
totals. Each available counter sum includes observed/missing object counts. CPU and
block-I/O use compatible counter deltas. Peaks remain per object; multiple
noncoincident peaks do not become a fictional simultaneous peak. Current gauges
without a common fresh snapshot remain unavailable as an aggregate. Inspect
`last_seen`, stale state and coverage alongside any number.

The default selects the named run's own links. `--include-children` follows only
explicit `run.parent_run_id` relationships and selects each measured object once
across that tree, including descendants' broker allocations. `included_runs` and
`hierarchy_source` identify the selection. Parent cgroup counters already cover
their nested scopes; those child measurements remain explanatory detail. Shared
objects stay shared. Compare these two views rather than adding their totals.
Cycles or a hierarchy exceeding 1000 runs are reported as errors.
In the active store, `show-run` prefers the local run or a unique imported run.
Use `--origin ORIGIN_ID` to select an imported run explicitly.

The canonical source is `records`. Its provenance columns include
`origin_id`, `event_id`, `sequence`, `source_id`, `source_version`, `source_time`,
`received_time`, `kind`, `encoding`, `policy_version`, `import_policy_version`
and `redactions`.
`protobuf` stores sanitized supported OTLP ExportRequest data as one zstd frame
per record; rows written before compression stay plain protobuf. `export`
returns plain protobuf for both, while `query` returns the stored bytes. `body`
stores versioned internal resource/run JSON. Views and projections are conveniences:

| Table/view | Meaning |
| --- | --- |
| `runs` | Explicit execution metadata and latest lifecycle state |
| `relations` | Native sessions and other evidenced relationships |
| `objects` | Scope/process/resource identities, generation and containment |
| `associations` | Time-bounded run/object attribution with quality and shared flag |
| `samples`, `sample_values` | One measurement per object/source event |
| `allocations` | Generation-specific latest allocation state |
| `gaps` | Missing boundaries, stale/offline sources and coverage failures |
| `model_requests`, `tool_outputs` | Usage records per model request and tool output |
| `model_turn_triggers` | Model requests with the preceding usage event of the same agent |

Each displayed object's `sample_refs` points to canonical `(origin_id,event_id)`
records. Export or join those identities to inspect source provenance independently
of the summary.
Session entries likewise retain relation references, source/quality and first/last
recorded observation times. Those evidence points are not a claim about the
session's full lifetime. Owner-level offline gaps accompany linked allocations
even when no new object sample could be obtained.

Use the record origin when querying a run in the active store.

```sql
SELECT sequence, origin_id, event_id, source_id, source_time, received_time, body
FROM records
WHERE encoding = 'json'
  AND origin_id = 'ORIGIN_ID'
  AND json_extract(body, '$.agent_run_id') = 'RUN_ID'
ORDER BY sequence
LIMIT 1000;
```

Unlinked resource records remain visible at yard level. They are not inferred to
belong to the run being inspected:

```sql
SELECT r.origin_id, r.object_id, r.source_time, r.body
FROM allocations AS r
WHERE NOT EXISTS (
  SELECT 1 FROM associations AS a
  WHERE a.origin_id = r.origin_id AND a.object_id = r.object_id
)
ORDER BY r.source_time
LIMIT 1000;
```

Inspect the linked allocations and their original records without adding guest
details to owner-side measurements:

```sql
SELECT a.object_id, a.start_time, a.end_time, a.quality,
       json_extract(r.body, '$.state') AS state,
       json_extract(r.body, '$.allocated_cpu') AS allocated_vcpu,
       json_extract(r.body, '$.allocated_memory_bytes') AS allocated_memory_bytes,
       json_extract(r.body, '$.stopped_at') AS stopped_at
FROM associations AS a
JOIN allocations AS r USING (origin_id, object_id)
WHERE a.origin_id = 'ORIGIN_ID' AND a.agent_run_id = 'RUN_ID'
ORDER BY a.start_time;

SELECT s.origin_id, s.event_id, r.sequence, s.object_id, s.source_time, s.body
FROM samples AS s
JOIN records AS r USING (origin_id, event_id)
WHERE EXISTS (
  SELECT 1 FROM associations AS a
  WHERE a.origin_id = 'ORIGIN_ID' AND a.origin_id = s.origin_id
    AND a.object_id = s.object_id AND a.agent_run_id = 'RUN_ID'
    AND s.source_time >= a.start_time
    AND (a.end_time = '' OR s.source_time <= a.end_time)
)
ORDER BY s.source_time
LIMIT 1000;
```

Do not write derived calculations back into canonical records.

### Usage and attribution

The collector derives one `usage` record per native model request, tool output
and user prompt from the sanitized log it stores, and a durable `relation` for
each session link it sees. Both carry `source_id=collector:usage`. They hold only
IDs, categories and counters; native counter names are kept, so incompatible
counters are never combined. Metrics and spans repeat the same usage and are not
counted. Usage records are not rebuilt from older raw OTLP.

| Client | Model request | Tool output | Request identity | Links |
| --- | --- | --- | --- | --- |
| Claude Code | `api_request`: `input_tokens`, `output_tokens`, `cache_read_tokens`, `cache_creation_tokens` (with 5m/1h), `cost_usd`, `query_source`, `request_id`, `prompt.id`, `event.sequence` | `tool_result`: `tool_use_id`, `tool_result_size_bytes`, `tool_input_size_bytes` | provider `request_id`, else log time and sequence | `session.id`; subagents by `agent_id`/`parent_agent_id` in the same session (2.1.327; earlier releases count subagents as the session) |
| Codex | `sse_event` with `event.kind=response.completed`: `input_token_count` (includes `cached_token_count`), `output_token_count`, `reasoning_token_count`, `cache_write_token_count`; `tool_token_count` is their total | `tool_result`: `call_id`, `output_length`, `output_line_count` | log time (no response ID) | `conversation.id`; a child thread only when a log carries `parent.conversation.id` (`codex.agent_response`, off by default) |

Claude reports absent cache counters as zero, so a zero cache value does not
prove the provider reported one. Codex fields that are absent stay `NULL`. A
Codex WebSocket prewarm request can report its own completed response.

Usage record IDs are derived from the client, session and request identity, so a
re-delivered export, a resumed sender or a repeated replay adds no usage.
`model_turn_triggers.cause` is `new_prompt` after a user prompt, `tool_result`
after a tool output, otherwise `unknown`.

A coordinator is attributed by its own session. Native subagents appear under
the coordinator's session with their `native_agent_id`. A worker started from a
coordinator's shell is linked only when its exporter reports the coordinator's
session as the resource attribute `subyard.parent_session.id`; the collector then
records a `parent_session` relation. Claude exposes its session to tools as
`CLAUDE_CODE_SESSION_ID`, Codex as `CODEX_THREAD_ID`, and both clients read
`OTEL_RESOURCE_ATTRIBUTES`:

```sh
OTEL_RESOURCE_ATTRIBUTES="subyard.parent_session.id=$CLAUDE_CODE_SESSION_ID${OTEL_RESOURCE_ATTRIBUTES:+,$OTEL_RESOURCE_ATTRIBUTES}" codex exec ...
```

Without it the worker's usage is still counted under its own session and the
link stays unknown. One query reports a task from its coordinator session:

```sql
SELECT CASE WHEN m.session_id = :session AND m.native_agent_id IS NULL THEN 'coordinator'
            WHEN m.session_id = :session THEN 'subagent' ELSE 'worker' END AS party,
       m.client, count(*) AS requests,
       sum(coalesce(m.input_tokens, 0) + coalesce(m.input_token_count, 0)) AS input_tokens,
       sum(coalesce(m.output_tokens, 0) + coalesce(m.output_token_count, 0)) AS output_tokens
FROM model_requests AS m
WHERE m.session_id = :session
   OR m.session_id IN (SELECT session_id FROM relations
                       WHERE json_extract(body, '$.relation') = 'parent_session'
                         AND json_extract(body, '$.parent_session_id') = :session)
GROUP BY 1, 2 ORDER BY 1;
```

Codex `input_token_count` includes its cached tokens while Claude
`input_tokens` excludes `cache_read_tokens`; report cache columns separately.

## Resource attribution and measurement semantics

Broker `run` remains an acquire correlation ID. Optional local hooks bind it to
the inherited agent run without changing old strict acquire request envelopes.
Held-acquire hooks persist a bounded private registration intent without waiting
for the observer's remote read lock. The observer merges it and replays retained
owner events when adding a new mapping. That also links a short allocation which
ended before the mapping was applied. Stable event identities prevent duplicate
measurements during replay; generation checks prevent stale ownership changes.
Private operational storage outside the data directory holds transport
credentials, Android observe capabilities and sensitive route paths.

A returned VM allocation is identified by owner, slot, resource generation and
lease epoch; Android uses its native owner/slot/generation contract. Observe-only
capabilities are negotiated separately. Source cursors advance only after durable
admission and never acknowledge/delete the existing broker sink's journal.

Resource observations require an updated owner advertising `resource-observe-v1`.
An older owner continues ordinary acquire/renew/release operations; unavailable
observation is reported as missing coverage. Enabling the collector does not
silently update or grant new host access to that owner.

Actual measurements come from the owner through bounded read-only operations.
The yard does not receive an Incus/Docker socket or root access to the owner.
Owner unavailability means last-seen/stale/gap, not zero usage. TTL expiry is not
proof that a VM stopped. Draining resources remain visible after agent exit.

Counters and gauges retain their source semantics:

| Measurement | Interpretation |
| --- | --- |
| Cumulative CPU | Delta within the same generation, boot and monotonic domain; CPU% of one core differs from quota utilization |
| `memory.current` | Cgroup charged memory gauge, distinct from process RSS/PSS and limits |
| Kernel memory peak | Kernel high-water mark for that object/generation, not a sum of individual peaks |
| Sampled memory maximum | Largest observed gauge; short peaks can be missed |
| Process RSS/PSS | Distinct process measurements; summed RSS can count shared pages repeatedly |
| Block I/O bytes | Actual source block-I/O counters; not logical `rchar`/`wchar` or syscall counts |
| Requested/reserved resources | Allocation capacity and holding intervals, distinct from actual CPU/RAM |

For an observed compatible interval, one-core CPU percent is
`100 * cpu_ns_delta / observed_elapsed_ns`; it can exceed 100 for parallel work.
Quota utilization would require the effective quota over that same interval and
is a separate quantity. A missing quota is not inferred from the number of CPUs.
Live current gauges require a running object and an available source; final or
stopped objects retain their last measurement as historical evidence. Missing live
values remain null. Logical release or TTL expiry alone does not prove that a
resource stopped. Per-object peaks retain their source semantics. An actual-memory
byte-second integral is not calculated by this version.

For a granted VM, reserved capacity means its assigned virtual CPU/RAM/disk held
by that allocation. It does not promise exclusive physical host cores or resident
RAM. `allocated_vcpu_seconds` and `reserved_vcpu_seconds` describe capacity-time;
`reserved_memory_byte_seconds` describes the granted memory ceiling over its
holding interval. Android uses only capacities actually reported by its owner;
its memory ceiling includes the documented runtime overhead. Unavailable capacity
stays null. These fields are separate views and must not be added to actual usage.
Native journals provide UTC holding boundaries, so holding duration and associated
capacity-time are source-wall-clock estimates; CPU/I/O deltas use compatible
owner counter/monotonic domains. Clock drift and missing boundaries limit coverage.

Incus guest-agent state is guest detail, not host physical VM memory. When only a
verified QEMU process is available, measurements explicitly describe that process
and its threads; RSS/PSS are not relabeled as cgroup memory. Helper/kernel-thread
coverage remains unavailable unless a broader owned boundary is proven.

Reused resources require allocation-boundary counter baselines. Missing or reset
boundaries leave an approximate/unattributed interval. An old-generation sample
cannot update the new owner's resource state. Guest detail, host QEMU detail and
the corresponding broker object are not independent additive resource charges.
Likewise direct workload and inclusive parent scopes must not be added together.

## Capture policy and delivery

The collector binary owns the allowlist sanitizer used by native OTLP, ingest
and replay. Local producers and state writers call it over stdin, including
while the service is stopped. Sanitization happens before a frame, manifest or
temporary file is written. Producer configuration, observer snapshots,
reservations, delegation, broker intents, cursors, loss markers and cutover
receipts use closed schemas. The producer, readiness check and repair
verification share a read-only loader for producer identity and capture policy.

New telemetry preserves raw session, conversation, run, trace and span IDs,
host, yard, project, worktree and task labels, model and tool names, token
counts, cost and durations. OTLP retains supported resource and scope fields,
timestamps, trace links, severity and status numbers, known signal names,
metric points, exemplars, histogram bounds and counts, and units. Bounded
decimal strings are accepted for documented numeric fields. Zero is
distinguished from a missing value.

- Claude retains `event.name`, `session.id`, `ccr.session.id`, `app.version`,
  `model`, `tool_name`, `prompt.id`, `request_id`, `agent_id`,
  `parent_agent_id`, `tool_use_id`, `query_source`, `event.sequence`,
  `tool_result_size_bytes`, `tool_input_size_bytes`, `prompt_length`, `input_tokens`, `output_tokens`,
  `cache_read_tokens`, `cache_creation_tokens`, `cache_creation_5m_tokens`,
  `cache_creation_1h_tokens`, `cost_usd`, `cost_usd_micros`, `duration_ms`,
  `ttft_ms` and `success`, plus supported token, cost and time metric points
  with their type and unit. Claude sends `event.name` without the
  `claude_code.` prefix, which appears only in the dropped log body; the
  stored value keeps the client's form and is checked as
  `claude_code.<name>`. Prompt, tool-detail and tool-content logging stay off.
- Codex retains `event.name`, `conversation.id` and supported session aliases,
  `parent.conversation.id`, `parent.turn.id`, `agent.type`, `event.kind`,
  `call_id`, `output_length`, `output_line_count`,
  `app.version`, `model`, `slug`, `tool_name`, `tool_namespace`,
  `prompt_length`, `duration_ms`, `ttft_ms`, `success`, `input_token_count`,
  `output_token_count`, `cached_token_count`, `cache_write_token_count`,
  `reasoning_token_count` and `tool_token_count`, plus supported usage and
  duration metric points. Codex sends prefixed event names such as
  `codex.tool_result`, `codex.tool_decision` and `codex.sse_event`. The Codex
  emitter uses `tool_token_count` for total tokens. `log_user_prompt` stays
  false.

Both clients keep the resource `service.name`, which for Codex app-server is
the connecting client's name; it passes the same value check as other labels.
Both clients send `success` and several counts and durations as strings.
`success` is accepted as a boolean or the string `true` or `false` and is
stored as a boolean; counters are stored as integers and durations and cost
as doubles. Sensitive values found in a log record, span or metric are
removed only from that record; values found in a resource or scope apply to
every record below it.

The sanitizer drops log bodies, prompts, tool inputs, arguments, parameters
and output previews, response text, error messages, severity text, headers,
allocation purpose and arbitrary nested fields. It drops operator PII fields
including email, user and account IDs, account UUIDs and organization IDs.
Every retained string is checked for recognizable email addresses, bearer and
basic credentials, password assignments, API and private keys, token formats
and exact sensitive values identified in the input. Unsafe optional strings
are omitted. An unsafe required identity is refused with a fixed diagnostic.

An optional `--policy-file` supplies `subyard.telemetry.capture/v1` with
`attributes`, `span_names` and `event_names` allowlists that narrow the built-in
schema. The selected producer policy is published as a private snapshot.
Missing or invalid producer state makes profile readiness stale.

The serialized writer batches with SQLite WAL and `synchronous=FULL`. Defaults
bound OTLP payloads to 2 MiB, allow 16 concurrent OTLP requests before decode,
and limit queued sanitized data to 16 MiB/128 requests. The profile setting
`TELEMETRY_DISK_BUDGET` (default `30GiB`) sets the collector's `--disk-bytes`.
Raw OTLP is accepted while allocated database pages and the WAL stay below half
of it and the data partition keeps 1 GiB free, or a twentieth of a smaller
partition. Beyond that the collector stores the request's usage records and
bindings, refuses the raw request with a retryable status, and `stats` warns and
names the cleanup command. The database itself may grow to three quarters of
the budget. Each legacy archive uses additional disk space outside it.
Nothing is pruned automatically. Remove old raw OTLP, per-request session
correlations and samples manually:

```sh
"$collector" prune --data-dir "$data" --older-than 60d
```

The command asks the running collector over its control socket, or opens the
store when the collector is stopped. Usage records and their bindings stay
unless `--include-usage` is added; run lifecycle, resource and attribution
records always stay, so an old run's outstanding resource responsibility does
not disappear. Freed pages are reused; the database file does not shrink.
The producer spool is bounded to 4096 files and 64 MiB using fixed allocations
and 64 KiB frames. It keeps records ordered within each generated run stream and
within the broker stream. SQLite commits the records, projections and byte
position together. Producers acknowledge after frame and directory durability.
The collector retires sealed segments after the committed end. A malformed
frame or a sealed torn tail produces a content-free gap.
Run snapshots have separate limits of 4096 files, 16 MiB total and 16 KiB per snapshot, including reserved space, with a
100 ms admission deadline. Older overflowing directories are consumed in bounded
batches; capacity and interrupted reservations produce content-free coverage gaps.
Private observer registration intents have a
separate 128-file/1 MiB bound and a 100 ms admission-lock deadline; observer
configurations are capped at 32 transports and 128 correlations per transport.
Capacity failures produce content-free diagnostics and remain nonfatal to
agent commands and broker operations. Producers retain a small content-free overflow
marker outside that quota. The collector consumes it even if the producer never
writes again. A filesystem that rejects even the marker cannot guarantee a durable loss count; producer
failure remains nonfatal to the native workload. Inspect `serve --help` and the owning producer constants before
changing bounds. Export data before pruning it when a longer history is required.

An OTLP success acknowledges a completed SQLite commit. A storage/capacity failure
is retryable and never acknowledges an unsaved request. Admission is atomic per
request; there is no misleading partial-success count. Only a raw OTLP refusal
for disk budget or partition reserve keeps the request's usage records and
bindings. Collector failure does not
make agent commands or broker lifecycle operations wait indefinitely. If the
bounded local spool fills, collection records a capacity gap where the reserved
marker can be written. Native bytes the receiver never saw cannot be recovered
by the spool, and receiver downtime is not recorded as a gap.

## Export and replay

Export is JSON Lines with schema `subyard.telemetry.export/v1`. OTLP protobuf is
base64 in the `protobuf` field, with kind/encoding `otlp/logs`, `otlp/metrics` or
`otlp/traces`. An empty OTLP message omits `protobuf`. Internal records use JSON
`body`. Source and receive timestamps, policy identity, record identities and
provenance are retained.

```sh
"$collector" export --data-dir "$data" --after 0 --limit 1000 > telemetry-0001.jsonl
"$collector" replay --data-dir /path/to/private/replay-store --input telemetry-0001.jsonl
```

Continue export with `--after` equal to the last exported `sequence`. Each
command reads a bounded snapshot. Continue a pre-upgrade export against the
archive and start a new cursor at zero for the active store.

The producer assigns `(origin_id, event_id)` once and reuses it on retry. Export
and replay preserve that pair across stores, including exports from a
database-only backup. The first accepted row wins even if a later delivery
changes the payload or capture policy. A duplicate advances the spool offset
without changing the row or its projections. Identical payloads with distinct
identities remain distinct. Run and resource joins use the origin to keep raw
IDs from different stores separate.

Replay sanitizes new imports with the destination's `--policy-file` or default
policy and applies queue and disk limits. `policy_version` preserves the original
validated capture-policy claim and `import_policy_version` records the local
policy applied by replay. Legacy exports without that optional field remain
readable. Pruning a sample or OTLP row also removes its projections and
deduplication claim. A pruned sample can be restored once and rejoin its
retained object and run. Run lifecycle and coverage records remain available.

OTLP has no universal delivery identity. If a sender retries after a successful
commit whose acknowledgement was lost, the native delivery can appear twice.
Stable source event IDs support idempotent retries for internal producers. Raw
native OTLP observations can repeat; the derived usage records do not.

## Standalone and nested containers

Build the same native collector from the repository:

```sh
go build -o subyard-telemetry ./config/profiles/telemetry/cmd/telemetry
install -d -m 0700 /path/to/private/telemetry /path/to/private/telemetry-credentials
install -m 0600 /dev/null /path/to/private/telemetry-credentials/auth.token
openssl rand -hex 32 > /path/to/private/telemetry-credentials/auth.token
./subyard-telemetry serve --data-dir /path/to/private/telemetry \
  --listen 127.0.0.1:14318 --auth-file /path/to/private/telemetry-credentials/auth.token
```

Configure trusted senders for HTTP/protobuf `/v1/logs`, `/v1/metrics` and
`/v1/traces` with their private Bearer credential. A standalone receiver can retain
native events without Subyard; full process/resource attribution still needs the
explicit local producers and available delegation/owner capabilities.
Partial launches with a separate `--data-dir` start a local observer when no
manager holds that directory's lock. The observer exits after pending manifests drain.

A nested devcontainer has a different loopback network. Explicitly mount only
the collector's private Unix socket at an agreed container path and run:

```sh
subyard-telemetry-forward --socket /run/subyard-telemetry/control.sock \
  --listen 127.0.0.1:14318
```

The restricted forwarder supports only the three OTLP paths and uses the mounted
socket's ownership as its trust boundary. It does not expose arbitrary control
requests, publish a host port or require a privileged container, host PID namespace
or writable host cgroup mount. Preserve the data-owning UID and private socket
permissions; do not broaden them to make a mount work.

## Verification

Owning local and disposable acceptance entrypoints are under
`config/profiles/telemetry/tests/`. The reproducible comparison driver lives in
`dev/telemetry-compare`; `tests/e2e/measure.py` measures two actual binaries sharing
the same capture policy and SQLite sink. Source/version identities, trial
conditions and accepted versus persisted counts belong to the measurement
evidence. Product limits and binary size are not runtime overhead measurements.

Release acceptance splits the disposable fixture into parallel `local` (`--local`)
and `resources` (`--resources`) lanes. Together they retain the local storage-failure,
OTLP, overhead, process-scope and native-signal checks, plus VM/Android native allocation,
profile lifecycle, held-resource and storage-failure observations, final counters,
resource examples and export/replay. The resource lane uses Android's short
`memory-budget` exercise and the broker's `--telemetry-resources` mode.
Long renewal, requester-death natural expiry and reuse assertions run in the owning
Android `memory-lease` and test-vms `memory-boundary` lanes. Telemetry's host-free
`resources-test.py` replays synthetic expiry and actual-stop events for both VM guests,
checking draining → available, pre-stop samples and explicit missing final tails.

### Verified workload and broker example

A disposable acceptance run launched an explicitly managed fixture workload,
acquired two VM allocations, observed owner counters and an offline/recovery
interval, and retained the resulting records in SQLite. The final read-only
report selected the local cgroup and two VM objects: observed local CPU deltas
were 212.149 CPU-seconds, while observed broker CPU deltas were 0.450 CPU-seconds.
The allocations reserved 855.415 vCPU-seconds across their holding intervals;
that capacity is separate from actual CPU use. Final-tail gaps remain explicit,
and completed resources have unavailable current gauges rather than historical
memory presented as current use. Inclusive local accounting does not add nested
process detail a second time.

The exported snapshot contained 351 source records. Replaying it twice into a
fresh standalone store preserved record counts, kinds and sample SQL results;
the final report's source references matched those canonical records. This fixture
used agent type `other`, not a fabricated native session. Separate disposable
checks used actual Codex and Claude clients with local fixture providers to bind
native sessions and tool-triggered VM/Android allocations to the same managed
run. These checks do not claim verification of paid provider accounts.

### Receiver packaging comparison

On 2026-10-24, two actual CGO-disabled Go binaries were compared on one disposable
Debian 13 amd64 VM (Linux 6.12.113, 4 vCPU, 4 GiB RAM). Both used the same official
receiver, capture policy, SQLite sink, durability settings and load driver.
SQLite reported 3.54.1. Three fresh-database trials per variant alternated order;
each had 15 seconds of warmup, 60 seconds idle and 90 seconds at 30 mixed OTLP
requests/second, 32 records per request. The driver ran outside the measured
process. RSS was sampled every 0.5 seconds; CPU used process counter deltas and
monotonic elapsed time. The underlying host was shared, not an isolated laboratory.

| Measured median | Embedded receiver service | Minimal Collector service |
| --- | ---: | ---: |
| Idle CPU, percent of one core | 0.05% | 0.10% |
| Load CPU, percent of one core | 10.15% | 19.40% |
| Mean sampled RSS under load | 35.63 MiB | 38.73 MiB |
| Process block writes under load | 1.175 MiB/s | 1.175 MiB/s |
| SQLite/WAL/SHM apparent growth | 0.127 MiB/s | 0.126 MiB/s |

All offered requests were accepted and matched committed database rows. One
90-second trial offered 2699 requests; the other trials offered 2700. There were
no rejected requests or driver queue drops. Load CPU ranged from 9.50–13.10% for
the embedded service and 10.11–27.64% for the minimal Collector; these few trials
do not establish a general CPU speedup. WAL checkpoints can produce negative
idle file-size growth. Process block writes and SQLite size growth are separate
measurements, not interchangeable disk-usage figures.

The embedded receiver was selected because it keeps the existing protocol
implementation while avoiding the additional Collector service/factory/config
assembly around this single local sink. Its observed RSS was also lower here.
The minimal Collector remains a reproducible comparison under
`dev/telemetry-compare/collector`; it is not an installed runtime dependency.

This comparison binds to product SHA-256
`f6ad4f0d2561ebfa2ebbea24b8fb74f4eead4f0f2175a3c1ddabda4c8efb747c`
and minimal Collector SHA-256
`1483d3cf16c5ed45d97aa137d5f7e238fa89a5f8802ebe750493b1d55bdf34a4`.
Later correctness repairs cover nonfinite metric values, resource run binding,
summary semantics and retention. The finite synthetic input did not exercise those
repaired cases. Installed collector/observer overhead is measured separately
below; these figures are evidence for the packaging comparison,
not a claim that a newer binary has the same exact runtime consumption.

### Installed collector and manager

On 2026-10-25, the installed services were measured separately on a disposable
4 GiB Linux stage using test base
`bd6294b5361ed22271665d9db3428602a8029dc2479ab63d0bcadae5ba46eec0`.
The collector used SQLite 3.54.1 and product SHA-256
`cf16ded93c82fcd65b4d6ff87f539c3bcd34132dd08989dac3f4c4319300ce73`.
The measured launcher had SHA-256
`461cae184f9e270669f0dc1bfbf2cfcd39a0a4d829d53f967466c357660a53c1`.
The manager's own leaf and collector service were measured independently of the
managed workload. The observer verified service PID start times and cgroup
generations. Measurements used cumulative cgroup CPU deltas, monotonic elapsed
time, process RSS and block-write counters, sampled once per second.

| Measured service | Idle CPU, one core | Load CPU, one core | Idle sampled max RSS | Load sampled max RSS |
| --- | ---: | ---: | ---: | ---: |
| Collector | 0.075% | 5.993% | 26.42 MiB | 36.38 MiB |
| Manager | 0.053% | 0.183% | 25.31 MiB | 25.38 MiB |

Idle lasted 60 seconds; load lasted 90.46 seconds. Load combined 30 mixed OTLP
requests/second with one managed scope holding 16 MiB and starting a short CPU
child each second. Agent sampling remained five seconds. All 2700 requests
(86,400 records) were accepted, with no driver drops or rejected requests; SQLite
contained 900 requests for each signal plus one preceding unit probe. Collector process block writes averaged
1.178 MiB/s under load; manager writes averaged 2.70 KiB/s. Database/WAL/SHM
apparent growth averaged 0.127 MiB/s. Idle size decreased during checkpointing.
The installed producer `records.py` had SHA-256
`94d0e091a4e241460263934a7f2d2473353f910b7b6fee5bf0d3f2db7bc5ce16`.

The windows bind the collector and launcher versions above and their service
limits after readiness. They exclude startup and restart costs; the `setpriv` startup wrapper
execs into the measured program rather than remaining as another process.
Subsequent repairs changed read-only report projection and overflow-recovery
fairness without changing this trial's normal serving and sampling paths. The
measurements remain scoped to the recorded versions, not byte-identical runtime
measurements of a later build.

These are one installed-stack trial on a shared host, not capacity guarantees or
an estimate of remote-owner polling cost at arbitrary fleet size. RSS includes
resident shared pages; cgroup charged memory has different accounting and is not
substituted for it. Service limits are safety ceilings, not measured consumption.
The local lane also passed storage/crash recovery, short-lived and background
descendants, real native Codex/Claude session binding and disable/retain-history.
Resource acceptance runs in its separate fresh lane; this measurement alone does
not prove broker acceptance.
