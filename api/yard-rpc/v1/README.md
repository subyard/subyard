# Yard RPC v1

This directory publishes the language-neutral contract of `yard rpc --stdio`.
JSON Schemas describe JSON payloads; they do not create a shared runtime or a
second domain implementation. Go remains the authority for validation, routing,
assessment, confirmation, execution and verification.

## Transport and negotiation

A frame is a four-byte unsigned big-endian length followed by exactly that many
bytes of UTF-8 JSON. The JSON payload must be at most 1,048,576 bytes. Reject an
oversized length before allocating or reading its body. Partial reads/writes are
normal. Stdout contains frames only; stderr is not a machine result channel.

[`frame.schema.json`](frame.schema.json) describes request, cancel, response and
event envelopes. Request IDs and operation IDs are printable ASCII, 1–128 bytes.
Protocol `version` is `1`. An absent operation ID defaults to the request ID.
Responses may arrive out of request order: correlate by ID. A typed error has
`code` and `message`; clients must not derive state by parsing that message.

Call `rpc.negotiate` before other methods, with `{}` params. The result follows
[`negotiate.schema.json`](negotiate.schema.json): `version`, `protocolMin`,
`protocolMax`, `engineVersion`, and `capabilities`. Product version compatibility
and wire compatibility are separate checks. A client requiring a capability must
reject its absence; it must not infer support from a version or attempt a human
CLI fallback. Veranda's release policy additionally requires the matched product
version. Unsupported profile descriptors fail through the existing catalog
loader, rather than being translated into another profile schema.

Requests may include an RFC 3339 `deadline`. Cancellation sends a `type:"cancel"`
frame with the active operation ID; it carries no deadline. Explicit cancellation
and deadline expiry produce distinct typed errors. Disconnect cancels active work
and releases unused plans and retained operation-owned drafts. The server has a
bounded writer queue; an unresponsive client is disconnected.

## Ordered events and snapshots

Events have `sequence` and `revision`, both monotonically increasing in one
session-wide stream. They are not per-operation or per-adapter counters. A native
adapter revision, when present inside `data`, cannot replace the outer revision.
`system.snapshot` and `system.resync` emit `snapshot.ready` and return that event's
revision with the complete core snapshot. Replies themselves need not carry
sequence/revision. Detect gaps or reordering, mark affected observations stale,
and obtain a fresh snapshot. A new session starts its own ordered stream.

## Current methods

[`methods.schema.json`](methods.schema.json) describes method-specific params.
[`core-results.schema.json`](core-results.schema.json) holds core result
definitions. The exact mutation schemas intentionally describe the complete
step contract required by new clients; older non-exact execution remains an
engine compatibility surface, not a client fallback.

| Method | Params | Result schema or definition |
| --- | --- | --- |
| `rpc.negotiate` | `{}` | `negotiate.schema.json` |
| `command.list` | `{}` | `core-results.schema.json#/$defs/commands` |
| `context.get` | `{}` | `context.schema.json` |
| `operation.route` | `command` | `core-results.schema.json#/$defs/route` |
| `operation.plan` | `command`, `arguments`, `exact:true`, `stepSchema:1` | `operation-exact.schema.json` |
| `operation.execute` | `confirmed:true`, exact `digest` | `core-results.schema.json#/$defs/execute` |
| `operation.discard` | `{}` | `core-results.schema.json#/$defs/discard` |
| `project.copy.finalize` | exact `digest` | `core-results.schema.json#/$defs/execute` |
| `project.copy.abort` | `{}` | `core-results.schema.json#/$defs/discard` |
| `keys.prepare` | `arguments` | `core-results.schema.json#/$defs/plan` |
| `integration.status` | `id`, optional `ownerOnly` | `core-results.schema.json#/$defs/integration` |
| `project.list` | optional `live` | `core-results.schema.json#/$defs/projects` |
| `owner.inventory` | `{}` | `owner-inventory.schema.json` |
| `yard.status` | `{}` | `core-results.schema.json#/$defs/status` |
| `credential.list` | `{}` | `core-results.schema.json#/$defs/credentials` |
| `credential.status` | `{}` | `core-results.schema.json#/$defs/credentialStatus` |
| `incus.events` | optional `types` (at most 8) | typed event stream, then `eventsClosed` |
| `system.snapshot`, `system.resync` | `{}` | `core-results.schema.json#/$defs/snapshot` |
| `system.ping` | `{}` | `core-results.schema.json#/$defs/ping` |
| `profile.list` | `{}` | `profile-list.schema.json`, capability `profile-list-v1` |
| `settings.list` | `{}` | `settings-list.schema.json`, capability `settings-list-v1` |
| `host.sync.status` | `{}` | `host-sync-status.schema.json`, capability `host-sync-status-v1` |
| `session.prepare` | `kind`, optional `scope`, optional `projectId` | `session-prepare.schema.json`, capability `session-prepare-v1` |

Core context/status/project DTOs predate the narrow GUI projections and contain
owner-native paths. A native client must project their safe facts before frontend
IPC. Terminal sessions, protected credential payloads, passphrases and interactive
editors keep their dedicated transports. Parameters reject secret-bearing fields
recursively. Incus event metadata is allowlisted by the engine.

## Exact operations

Require both `operation-exact-plan-v1` and `operation-steps-v1`. Send planning and
execution in the same owner session, using one operation ID. The owner returns
`schema:1`, `stepSchema:1`, its public plan, a lowercase SHA-256 `digest`, and
`expiresAt`. The digest binds the context, arguments, public plan and hidden
assessment inputs. The plan expires after five minutes; a request deadline does
not extend its lifetime. Ordered native steps must be nonempty and valid.

Show the owner's assessment, confirmation policy, consequences and exact steps.
Plans with `confirmation:"never"`, including no-ops, carry `confirmed:true`; this
records the owner's confirmation policy and does not execute the plan.
Execution requires `confirmed:true` and the returned digest. The owner consumes a
confirmed attempt once, including failed attempts; replay, changed binding and
expired plans fail closed. `confirmed:false` does not consume the plan.
`operation.discard` releases an unused plan. Plans and retained transfers share a
64-operation session bound. Preconditions and postconditions remain native checks
at the existing write boundary. Clients never synthesize broader desired state or
translate a failed exact plan into another mutation pathway.

`yard-bootstrap-v1` additionally permits `operation.plan` to specify `targetYard`
only for exact `init` with `--profile` and a safe non-default yard name. Send it
through an existing owner session. The native preset loader and init preparation
retain the target context and bootstrap; planning and discard create no yard
registration. Confirmation, digest, expiry, single-use execution and native
input guards remain the same. Queries keep the original session context.

Settings edits use existing `config set`/`unset` arguments through this exact
contract. Profile selection is the existing `ENVIRONMENT_PROFILES` setting;
deselection preserves installed state. Explicit `provision`/`init` is a separate
operation. Host sync actions retain the existing configuration and credential
operation families. The query projections add no mutation methods.

## Narrow owner queries

The catalog, sync and session queries require a local owner-bound session. Calling them in a
controller context for a remote yard returns `remote_owner_required`. Each result
has `schemaVersion:1`. Strings are bounded to 8,192 bytes, arrays to 1,024 entries,
and the encoded result reserves 4,096 bytes for its envelope within the frame
limit; excess produces `query_too_large`.

Profile and setting queries resolve persisted configuration on each call, so an
edit followed by a query in the same session observes the saved value. They never
change the session environment or switch owner identity. A changed routing or
instance identity returns `owner_context_changed` and requires reconnecting.

`profile.list` includes current shipped profile directories, optional v1
descriptors, legacy provision hooks and resource-only profiles. Selection comes
from the owner's existing effective configuration and descriptor defaults;
provenance preserves scope/role/status without source paths.
`hasYardPreset` reports a protected regular non-symlink shipped `yard.env`;
it exposes no preset content or path. The native loader validates the selected
preset when planning a named yard. Dedicated and
exclusive roles control eligibility. Convergence uses only the existing
`profile-check` contract for selected eligible hooks in an already running yard.
`current` means the native check returned converged; `changes-required` means
drift. Stopped, missing, failed or invalid checks remain `unknown` with a safe
diagnostic code. A profile without a provision hook is `not-applicable`. Queries
do not start yards, provision tools or invoke resource lifecycle handlers.

`settings.list` projects the operation-local typed catalog and effective dynamic
settings. Sensitive settings are omitted. File settings and scalar executable,
absolute-path and regular-file values/defaults are unavailable; contents and
package/source paths are not returned. Safe scalar values, defaults, validation
bounds, allowed scopes, application mode and layer provenance support native
authoring. `editable` reports whether persistent scalar authoring is declared;
the exact plan still validates scope, role, current state and action guards.
Values, defaults and provenance values preserve native typed scalar syntax:
multiline values may contain LF, and list values may use whitespace separators.
Clients must retain those representations; native authoring validates their types.

`host.sync.status` is always offline and never fetches, repairs permissions or
creates missing configuration roots. It projects native source registration,
cached Git relation/dirty counts, sanitized remote, applied commit, generation
and recovery state. A broken registration or unavailable checkout remains typed
state. Credential summaries contain only record/conflict counts and safe peer
trust/success/failure status; private payloads and raw errors never enter results.
Cached Git state does not establish the current remote HEAD.

`session.prepare` is read-only session preparation, with `kind` equal to `shell`,
`resources` or `vscode`. Scope defaults to `yard`; explicit `host` is required for
an owner-host shell or `htop`. Host sessions carry no project, and VS Code requires
a project in yard scope. Native project lookup uses only the selected owner's
read-only project store and existing role validation. A stopped yard is refused,
rather than started. The immutable RPC yard context cannot be switched by params.

Shell/resource results contain fixed `ownerArguments`: a host login shell,
host `htop`, or the existing `yard -Y <yard> shell [project] [-- htop]` invocation.
Host resources require an available owner `htop`; absence returns the static
`session_tool_unavailable` diagnostic before launch.
They contain no project source paths. VS Code results instead contain
`localArguments` for the owner's existing local `yard code` launch and a native
`vscode` descriptor: the guest folder, dedicated alias, developer, loopback relay
address/port, existing preview port and protected owner-pinned public Ed25519 host
key with its SHA-256 fingerprint. The existing native VS Code readiness check is
shared with CLI code; preparation neither writes a workspace/SSH config nor reads
a private identity or runs guest actions.

For a remote owner, the native desktop may forward the loopback relay through its
already pinned owner connection and create an app-private SSH alias pinned to the
returned guest public key. Its desktop agent key must already be authorized in
the guest. The `remoteAuthentication` field states that requirement explicitly;
preparation does not authorize a key, transfer owner-private credentials or launch
VS Code on the remote host. Guest authorization remains a separate explicit
owner action. Native `folderPath` and launch details stay out of frontend IPC.

## Golden fixtures

`fixtures/*.json` are human-readable illustrative response/event payloads;
matching `.frame` files contain their production-encoded frames. Frames use
compact JSON with sorted object keys, without a trailing newline. The illustrative
exact digest is not a usable owner plan capability. Go's focused
`TestYardRPCGoldenFrames` checks byte equality against the production codec. Rust
client tests consume these same files. Fixtures use only synthetic identities and
paths. Schema/profile changes require an explicit compatible contract change or
typed incompatibility; clients must not migrate unknown schemas themselves.
