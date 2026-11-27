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
[`vocabulary.json`](vocabulary.json) owns the protocol version, methods, capabilities,
error codes and event names. Errors in its `release-gate` group require owner repair.

## Error refusals

An error may carry `data` (`frame.schema.json#/$defs/errorData`): every
independent typed refusal the owner determined, in the owner's stable order. No
capability announces it; a client detects support by the presence of `data`.
Each refusal has a `source` (`config` or `release-gate`), a `code` of lowercase
letters, digits, `-` and `_`, a non-empty value-free `text`, and optionally the
refused `resource`, a `next` step and a typed `action` (a `verb` in the same
charset and the rendered `command`, which starts with `yard `). Configuration
refusals name only validated profile, setting, integration or yard-kind names,
never a setting value, secret or path. At most 32 refusals are sent; `omitted`
counts the rest. Texts are at most 512 bytes and contain no control or format
characters. The owner leaves out a refusal outside these rules and marks the
data `incomplete`, which also means the error had causes that are not typed
refusals; the owner's own diagnostics (`config validate`, `yard migrate --check`)
report them. A release-gate refusal adds `gate`, the public gate outcome
(status, code, active and target releases, transaction, message and action)
within the same text bounds. Its error `message` stays self-contained for
clients and relays that do not read `data`: one plain-text line of at most 2048
bytes with the outcome message and release state, the next step and every cause
with its resource and retry.
Other errors carry no `data`. A client skips a refusal whose `source` it does
not know and treats the data as `incomplete`. `data` is optional: older clients
ignore it, and older owners omit it, in which case clients keep their fixed text
for the error `code`. A relay built before error data existed drops `data` from
the errors it forwards, so a client behind it also falls back to that fixed text.

The owner keeps each failed request's full local error text with its request
ID, operation ID, method and code; `yard logs --rpc` on the owner shows them.
Veranda records only value-free facts about the failures it shows generically
in `rpc-errors.log` in its private store (0600, two 32 KiB generations): time,
method and code (from this vocabulary or its own transport codes, otherwise
`unknown`), its own request ID, a class (`disconnected`,
`transport_unavailable`, `invalid_response`, `rpc_timeout`,
`incompatible_engine`, `owner_error`, `error_data_rejected`) and, for an
undecodable result, the serde category and the decoded type and the name of a
field it declares. Repeats of the previous failure become one summary record
with a `count` and `last` time, appended when another failure arrives, at
least hourly, and when the client exits.

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
bounded writer queue. Responses are never dropped; they wait for queue room. When
events outrun the client, the server drops the events that do not fit except
the newest one, which it still delivers; the client observes a sequence gap
instead of a disconnect, and the session and its operations stay alive. At most
128 requests are active or awaiting delivery of their response at once; further
requests fail with `operation_capacity`, while cancel frames are still accepted.
If the client accepts no output bytes for the write stall timeout (30 seconds),
the server cancels the session's operations and delivers no further frames. Over
a blocking stdio pipe one more request may still be read and run with a
cancelled context, and the process ends only when the peer closes the stream.

## Ordered events and snapshots

Events have `sequence` and `revision`, both monotonically increasing in one
session-wide stream. They are not per-operation or per-adapter counters. A native
adapter revision, when present inside `data`, cannot replace the outer revision.
`system.snapshot` and `system.resync` emit `snapshot.ready` and return that event's
revision with the complete core snapshot. Replies themselves need not carry
sequence/revision. Detect gaps or reordering, mark affected observations stale,
and obtain a fresh snapshot. A new session starts its own ordered stream.
During a sustained event storm a resync snapshot can itself be followed by a
gap, so observations may stay stale until the storm ends.

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
| `release.readiness` | `{}` | `release-readiness.schema.json`, capability `release-readiness-v1` |
| `yard.status.profile-dashboards` | `{}` | `core-results.schema.json#/$defs/status`, capability `yard-status-profile-dashboards-v1` |
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

`release.readiness` may include `warnings`: at most 33 strings, each bounded to
512 characters. Larger combined lists retain 32 diagnostics plus an overflow
summary. The full warnings remain in
`yard migrate --check`. This optional response member preserves the core readiness
fields for older Go clients, which ignore unknown members and omit the new details.
Clients validating an older closed schema need the updated contract; the matched
Veranda product-version policy still applies.

`profile.list` includes unselected profiles. Its `eligibility` reason can be
`allowed`, `dedicated-role-required`, `exclusive-role`, `lifecycle-only` or
`policy-disabled`; clients retain the owner's `eligible` decision and reason.

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
`vscode` descriptor: the guest folder, ordinary yard alias, developer, loopback relay
address/port and protected owner-pinned public Ed25519 host
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

## Vocabulary evolution

The manifest's `revision` identifies a contract snapshot; clients branch only on
negotiated capabilities, never on `revision`. Changes are additive: retain existing
wire names, meanings and identifiers. `since` is the first manifest revision that
records an entry (revision 1 includes the existing vocabulary). Capabilities declare
`stability`, supported `sessions` (`workspace` or `owner-services`), and optional
`deprecated` metadata. Deprecation does not remove support. Explicit `ident` values
resolve generated identifier collisions and remain stable. New manifest sections
are allowed; DTOs and their schemas remain handwritten.

A new error code that requires new client behavior requires a new capability.
Clients handle unknown codes generically, ignore unknown capabilities and fields,
and handle unknown events without decoding them as a known event payload.
`rpc-v1` names the base handshake support. Negotiation is always admitted;
after it, the server gates each known method on its advertised capability and
returns `capability_missing` before dispatch when absent. Negotiation and the gate
run in input order, so a pipelined negotiate+call uses the same capability set.
Feature capabilities can additionally constrain method parameters (for example,
exact plans and complete operation steps); method gates do not replace those guards.

Run `make rpc-vocabulary` after editing the manifest, then update Veranda's contract
mirror and run `python3 veranda/dev/generate-vocabulary.py`. `make
rpc-vocabulary-check` and Veranda's host-free check reject stale generated files;
contract tests compare schemas, documented methods and the byte-equal mirror.
