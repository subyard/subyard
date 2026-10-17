# Stateful resource leases

`resource-lease/v1` is the observable contract for resource-owned lease adapters.
It does not introduce a shared broker or move profile lifecycle enforcement into
core. The Android emulator pool and OpenClaw staging bot expose the same `lease`
projection alongside their existing fields and protocols.

Each projection contains these fields:

| Field | Meaning |
| --- | --- |
| `contract` | `resource-lease/v1` |
| `resource_type` | Adapter-owned resource kind |
| `resource_id` | Stable resource identity within its owning yard |
| `slot_id` | Stable pool slot, or `null` for a singleton |
| `generation` | Retained monotonic allocation fence for this identity/slot |
| `state` | One of the states below |
| `owner` | Public attribution object, or `null` when unallocated |
| `expires_at` | UTC RFC 3339 lease deadline, or `null` when unallocated |
| `failure` | Optional safe diagnostic code |

Owner attribution is descriptive, never a credential. Status must whitelist
public fields and omit lease tokens, token hashes, provider credentials, private
configuration and raw failure text. Reading status must not create directories or
locks, renew, reap, stop, repair or otherwise change resource state. A projection
is an observation, not proof that a later mutation will be admitted.

| State | Admission and lifecycle meaning |
| --- | --- |
| `available` | No worker retains authority; a new allocation may be admitted after resource checks. |
| `provisioning` | An exclusive allocation is preparing its worker. |
| `held` | The resource is exclusively leased; resource-specific readiness remains separately observable. |
| `draining` | Authority expired or cleanup is in progress; do not allocate a successor yet. |
| `quarantined` | State or cleanup cannot be trusted; repair is required before reuse. |

Adapters need only expose states their lifecycle uses. Expiry invalidates rights;
it does not establish that a worker has stopped. An expired active allocation
projects `draining` even before a background reaper updates persisted state.
Reuse requires the owning adapter to confirm the old worker stopped. Failed or
unconfirmed stop retains the resource outside admission.

Every successful new allocation advances the retained fence, including after
release and process restart. An idempotent acquire of the same active staging
reservation retains its fence. One monotonic generation is sufficient; adapters
may use generation plus epoch when their domain requires both. Clients must
capture the allocation's authority and fence. A stale renew, release, stop or
delayed launch must never change a successor. A matching release may be
idempotent; expired or unknown credentials never grant authority over a new
allocation. TTL and heartbeat frequency remain resource policy, not shared
constants. Renew before expiry; an expired lease cannot be revived by heartbeat.

Errors distinguish confirmed contention (`busy`) from obsolete authority
(`stale`), failed initialization/cleanup (`failed`, with adapter diagnostic codes)
and unusable service/capacity (`unavailable`). Invalid state, failed transport or
unknown outcomes must not be treated as busy. A client may retry only confirmed
busy admission, with a finite caller-selected deadline and cancellation. Timeout
and cancellation do not start a worker, release another holder, or continue
retrying in the background. Wait is bounded polling, without fairness or FIFO
guarantees. Resource-specific capacity and validation errors remain immediate.

## Adapter projections

Android's `status.slots[].lease` and allocation `lease` use
`resource_type=android-emulator`, `resource_id=emulator`, the existing `slot_id`
and `generation`, and owner attribution `{yard, project, run, purpose}`. Existing
states, timestamps, requests, image details and graphics/memory diagnostics remain
available in their original fields. The private token identifies its allocation;
the broker checks it and captures the generation before stopping or reaping.
A retained generation floor preserves removed idle slots' fences through pool
shrink, growth and restart.
All-quarantined capacity reports `unavailable`; occupied recoverable capacity
reports `busy`. The existing client `--wait` retries only `busy`, and requester
loss cancels admission.

OpenClaw's staging payload `status` and both staging CLI status views use
`resource_type=staging-bot`, the configured bot lease key as `resource_id`,
`slot_id=null`, the retained `epoch` as `generation`, and owner attribution
`{zone, kind}`. Existing `holder`, `kind`, `epoch` and Unix `expires` fields remain
alongside the projection for allocated leases. Missing state is `available`,
unexpired state is `held`, expired state is `draining`, and malformed retained
state is `quarantined`. Gateway readiness is reported separately by the CLI.
Changing expiry is intentionally excluded from exact operation-plan binding;
holder, kind and epoch still bind the plan.

The staging wire results preserve `OK`, `BUSY` and `PREEMPT`; `PREEMPT` includes
the captured holder and epoch and authorizes no takeover by itself. `STALE`
identifies retained expired authority. An ephemeral client stops the captured
canonical worker and confirms its process group stopped under the existing shared inode lock
before releasing that generation and attempting normal admission. Unconditional
force replacement is refused. Launch also checks its captured epoch and TTL
under that lock and records private process identity for stop verification. Legacy
workers without that identity require container stop/removal. An expired lease requires explicit confirmed stop before reuse.
Heartbeat renew and cleanup compare both holder and epoch. Release of a live
gateway is refused by the in-yard client; use stop instead. Container down or
removal checks the captured holder and epoch and retains the same lease lock
through the container operation, staged-data cleanup and owned lease release.
A stale host operation cannot stop a successor that acquired in the meantime.

`sy-stage reserve` and `restart` accept `--wait SECONDS` in the range 0..3600,
defaulting to immediate admission. They wait only for another active ephemeral
holder (`BUSY`); expired state, malformed state, failed preemption and transport
errors fail immediately. SIGINT/SIGTERM cancel waiting. The host staging CLI
remains fail-fast and performs no implicit retry.

Owning regression coverage lives in Android's `android-pool-test.py` and
OpenClaw's `staging-lease.sh`, `openclaw-resource-protocol.sh` and
`staging-exact.sh`. Physical worker lifecycle remains an owning profile VM gate.
