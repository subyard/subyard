---
title: Decisions, assumptions, and glossary
status: living
updated: 2026-10-06
note: Current accepted contracts, rationale, and explicit implementation limits.
---

# Decisions, assumptions, and glossary

This reference records current accepted decisions and their rationale. The [CLI
reference](../cli-reference.md), [configuration guide](../configuration.md), [control
plane](../control-plane.md), and owning profile guides describe operational behavior. Proposed
work is identified explicitly; accepted direction does not imply completed implementation or
acceptance. Superseded alternatives and execution reports belong in Git history.

<a id="subyard-model"></a>

## Product model and trust boundary

Subyard provides reusable development isolation, protecting personal data and the host from
mistakes by autonomous coding tools. It is not inherently a production application platform.
Coding tools and environment profiles are independent: Codex, Claude, OpenCode, and pi are
tools; Android and OpenClaw describe codebases and environment requirements. OpenClaw
development does not imply running a live OpenClaw service or calling providers in tests.

The trust boundary is the host. Agents are trusted peers of one operator and may intentionally
use that operator's subscription. Subyard does not promise to hide supplied credentials from all
code inside their receiving environment. System containers protect against accidental host
damage but share the kernel; malicious kernel escape is outside that boundary. Explicit VM yards
provide a separate kernel and stronger isolation, with independently verified virtiofs, network
transport, and nested virtualization.

Autonomous local editing/build/testing is compatible with explicit approval for outward
publication. Commit/push prompts are coding-tool policy, not a hard credential boundary. App
installation tokens can carry write permissions; the yard boundary alone does not prevent push.
Pi has no equivalent native permission mechanism and relies on instructions. Diagnostics must
distinguish verified rules from unverified enforcement.

<a id="entities-and-layers"></a>

## Entities, layers, and identity

The domain map is `OwnerHost → Yard → Project → optional ProjectEnv`. Profiles are
configuration, not virtualization layers. Coding sessions/turns/transcripts/questions/approvals
belong to their upstream client/server and are never aggregated by Yard RPC, status, or Veranda.

| Layer | Boundary and ownership |
| --- | --- |
| L0 | Physical owner host, Incus, host networking and guards |
| L1 | Incus yard; guest userspace, persistence, unioned profile capabilities |
| L2 | Project Docker environment, selected profile/image/workspace; user starts coding tools |
| L3 | Optional runtime created by code under test, such as rootless Docker sandboxes |

Workloads use only the necessary prefix: L1 self-development need not create L2, and ordinary L2
work need not create L3. People enter L1 through SSH or L2 through container attachment; tests
create L3 programmatically. Multiple yards are independent L1 instances on one owner.

`YardRef = HostID + YardName`; input is a `YardSelector`. `AccessKind` is local/remote;
`YardKind` is container/VM. SSH endpoints/aliases, instance names, and images are not yard
identity. `EnvironmentProfile` and `CodingToolIntegration` are distinct many-to-many yard
relationships; projects select environment profiles. Public tool noun is `integration`, key
`CODING_TOOL_INTEGRATIONS`. L1 `YARD_IMAGE`/fallback differs from L2 `PROJECT_ENV_BASE_IMAGE`;
desired `YardImageRef` differs from observed `ResolvedYardImage`.

`default` is an ordinary canonical yard with registration/settings under `yards/default/`, not a
special settings scope, inheritance from default into other yards, or a switchable default role.
Selector defaults are command-specific: unselected status reports host-wide inventory. There is
no global sticky context.

Project selectors are `<project>/<yard>/<HostID>`, shortened to `<project>/<HostID>` for
default. Short names resolve only uniquely; ambiguity fails with canonical suggestions.
Yard-only selection is `<HostID>/<yard>`. Technical transport details belong to explicit
verbose/JSON output, not minimal human identity tables.

<a id="host-identity-and-trust"></a>

## Host identity and controller trust

HostID is operator-controlled; initial hostname is a default snapshot, never a continuing
authority. Explicit owner rename atomically changes persisted HostID without renaming yards,
storage, or Incus resources. On authenticated refresh, registered controllers migrate registry,
cached inventory, ownership/routing references, and transport metadata without another prompt.

Continuity requires the pinned concrete SSH server public key, not endpoint equality or a
caller-set authenticated boolean. Refresh uses managed known_hosts and strict host-key checking.
Collision fails before mutation; no stale HostID alias remains. Changed key produces integrity
failure without migration. Explicit host repair displays old/new fingerprints/IDs and the exact
plan, then may transactionally accept changed key and rename after consent. OpenSSH
aliases/ProxyJump/ProxyCommand work only when a concrete accepted key can be obtained. No extra
Subyard identity/signing key is required for this continuity contract.

Host add commits connection/trust/cache together. Host remove changes controller state only and
rejects live project references; it never removes owner resources. Controllers register owners,
not local alias copies of their actual yards/projects. Remote lifecycle/data-plane operations
run on owner; controller receives neither Incus/root authority nor owner project-state
authority.

<a id="project-identity-and-transport"></a>

## Project identity, transport, and visibility

New records use `ProjectID == Name`: canonical ASCII SafeID, at most 50 bytes, unique
case-insensitively within host/yard. Automatic collision allocation uses numeric suffixes;
explicit invalid/colliding names fail. Workspace is `/srv/workspaces/<ProjectID>/src`, without a
second public hash identity. Source fingerprints remain provenance. Existing legacy hash-bearing
IDs/paths remain stable; owner store outranks workspace metadata and crash-safe normalization
resolves legacy duplicate records.

Admission is a durable owner reservation. It includes records, active reservations, and
physically retained workspace names. Each completed sync invocation creates a fresh independent
snapshot of the same source; same-operation unfinished retries are idempotent. Retained/partial
copies are never overwritten. An approved automatic-name plan may allocate the next available
suffix concurrently, while explicit names remain exact/stale-guarded. Repeat bind retains
identity; duplicate clone URL fails. Multiple sync copies make their common source-path
lifecycle selector ambiguous: choose the copy's name/ID. Exact stored HostPath drives routing;
technical consumers use injective encoding when needed rather than another project ID.

Default sync copies into yard; Git clone is another transport. Explicit host bind independently
authorizes an arbitrary host folder, warns about weakened encapsulation, and appears as a
security warning. Machine-local host paths never enter portable committed project configuration.
Owner-local mappings may differ across machines.

An L2 environment mounts only its own workspace, not neighboring projects. Filesystem visibility
is independent of network egress/remote-yard access. Separate networks and absence of
cross-workspace mounts add defense in depth. Shared dependency caches are deliberate, subject to
each tool's concurrency contract.

Aggregate list/yards/status reports all configured local and registered remote yards, including
not-created runtime. Unreachable owners yield stale snapshots with age or unavailable entries;
expected unavailability does not make an aggregate report fail. Identity/integrity violations
do. Explicit selection scopes detailed status; RPC status remains one context. Size uses
cache-first bounded stale-while-refresh because Incus disk state is not consistently
authoritative for managed volumes.

<a id="host-safety-and-lifecycle"></a>

## Host safety and yard lifecycle

System containers are default; VM is explicit opt-in. Yard OS/toolchain and project images are
independent. CPU/RAM limits are opt-in, not mandatory defaults. Pool/persistent data live on the
home-volume storage root rather than assuming free root-filesystem space. Host Docker is
unnecessary, but existing host Docker must coexist safely. Only the operator's main host account
receives Incus-admin authority, which is effectively host root.

Docker runs inside yard. Neither host nor yard Docker sockets enter L2. Yard scripts
create/manage environments; tests needing Docker get an independent nested rootless runtime with
private data-root. Namespace/UID separation reduces peer access but cannot hide credentials from
code deliberately receiving them.

Idmapped mounts are preferred; emergency ACL fallback is only for unsupported filesystems. Incus
restricted disk-path enforcement conflicts with shifted mounts, so tooling constrains managed
mounts to per-yard host base instead. Explicit bind has separate operator authority. Never infer
that agents can change Incus device configuration.

Host NetworkManager must leave managed bridges/veth/tap devices unmanaged. Match type/driver as
well as interface naming and verify effective configuration; guards must precede instance
creation/start and rerun during setup. Bridge-scoped DHCP/DNS and forwarding rules coexist with
UFW/host Docker, without broad host input openings or hardcoded subnets. Package
installation/upgrade and networking must be reproducible through product reconciliation, not
unexplained manual host repair. Install optional dependencies only at the point of need, through
detect/advise/offer.

Init is idempotent/resumable ordered check/plan/apply/verify. Desired configuration converges
add/update/remove, rather than merely adding. Potentially disruptive yard changes appear in the
assessed plan before consent. Host safety checks remain fail closed. Internal numbered scripts
are not additional public lifecycle commands.

Outer Incus autostart remains false. The host boot reconciler waits for Incus/network, verifies
effective network guards/routes, starts local desired-running yards sequentially, and rechecks
routes after each. Fresh default/test-VM yards initially desire running; other named yards
stopped. Thereafter explicit desired state survives reboot/update for every yard; legacy
unmanaged instances import observed power. Remote owners restore their own yards. Inner broker
VMs never autostart and wait for acquire.

Teardown removes only selected/proven owned resources. Delete instance/veth before bridge, empty
owned project before deleting it, remove network guard only after bridge absence, and backing
data only after pool absence. Authoritative failed observations are not absence. Nested/source
teardown cannot delete outer data or interrupt outer SSH/VS Code: nonempty shared roots survive,
and ambiguous/custom/symlink paths remain with diagnostics. Controller-local workspace
descriptors avoid agent-writable-home lifecycle dependencies.

Generic backup/restore remains an unfinished product capability. Instance backup is not
whole-yard backup: custom persistent volumes need one coordinated restore contract. Host binds,
credentials, and machine-local settings are outside automatic capture. Data-bearing destructive
operations need the owning backup/consent policy; do not promise unimplemented commands.

<a id="settings-and-configuration"></a>

## Settings and effective configuration

Immutable shipped defaults, typed persistent Subyard settings, and temporary command overrides
combine into effective configuration. One catalog owns types, scopes, syncability,
merge/application mode, and domain ownership for scalar/file settings. Shared, host, and yard
scopes are typed; unknown ambient environment is not settings. Secret values are always
redacted.

Local-first behavior is authoritative: writers save locally by default or with `--local`;
`--git` saves only the selected setting and immediately commits/pushes. All local scopes outrank
Git scopes; explicit empty is present, not absence. Offline Git fallback is separate under
`.sync/settings/`. Registration alone cannot prevent local profile enablement. Sync push
transports already committed checkout content and never exports local overrides.

Git checkout is transport/versioning, separate from live configuration. Sync has no background
fetch/pull/push. It does not perform merge/rebase/stash/reset/conflict resolution/force.
Dirty/detached/diverged/no-upstream/changed-origin/pending-transaction states fail closed.
Authentication belongs to owner operator account, never yard/controller. Source record pins
checkout/origin identity and sanitizes diagnostics. Remote sync runs on owner.

Only catalog-known syncable nonsecret shared/selected-host settings are versioned. Host
enrollment is local, not implied by a Git directory. Missing shared/host subtree means empty
overlay; file-only host directories need not have scalar file, while an existing yard
registration does. Removing a subtree removes only prior managed paths after
ownership/digest/drift/in-use checks. Projects, desired power, observations, trust, secrets,
generated consumers, data home, support tools, and recovery/ownership records are never sync
payload.

Configuration root is a storage root, not one undifferentiated settings object: it also holds
secret inputs, consumers, encrypted ledger, project state, and tools. Use precise terms; avoid
ambiguous operator/private/machine config. Show/fields/paths distinguish effective values, typed
contract, and storage roles; status/apply refers to materialized file consumers. See
[Configuration](../configuration.md) for exact current syntax and precedence.

<a id="per-yard-coding-tool-selection-2026-09-20"></a>

## Per-yard coding-tool selection and artifact ownership

Every yard initializes without mandatory tool parameters. Default/named yards inherit active
profile/settings selection; shipped fallback is `claude codex opencode pi aiobserver`. Init does
not materialize inherited values or empty a fresh named yard. Remote access reaches owner
authority; controller discovery never changes selection. Paseo remains optional.

Requested set uses replacement semantics: unset inherits, explicit empty does not; legacy none
normalizes to empty. Dependency closure is separate, with reasons/provenance. Disabling a
required dependency fails rather than silently cascading. Special validated
`allows_coding_tools=false` suppresses inherited tools to empty and rejects forbidden explicit
requests; it is a capability, never a branch on a concrete profile name.

Enable/disable requires an existing running yard with ready prerequisites, rechecked before
apply. Missing/stopped yard fails before consent/writes with nonzero diagnostic, never automatic
startup. One integration-only Go reconcile shares full-init helpers; services/proxies/project
hooks belong to its exact plan. No second orchestrator or offline-maintenance startup. After
consent desired is persisted; failed reconcile retains desired plus
pending/conflict/error/nonzero so repetition resumes. No-op requires both desired equality and
converged runtime; no global rollback of every package/service is promised. Pairing is separate,
never automatic on enable or persisted in audit/config/RPC. Destructive purge is
separate/deferred.

Status uses one domain query for requested/effective sets, dependency reasons, provenance, and
observed readiness, distinguishing unset/empty/unknown. Reads never mutate or disclose auth.
Existing init preserves inheritance and canonicalizes only explicit local legacy selection,
never inferring desired from installed binaries or bulk scanning yards.

Structured-file ownership removes only proven unchanged owned JSON/TOML fields, retaining
foreign fields/document/auth/history. Other artifacts have a small root-owned versioned observed
inventory, not another desired database. Missing/corrupt/drifted evidence preserves data with
conflict, never claims success. Stop proven service before removing wiring; preserve explicit
links, session targets, unmanaged binaries, credentials, and user CLI sessions. Special
test-role proven cleanup is a stated preservation exception, but unknown artifacts block a
clean-state claim.

Known shared dispatcher/hook-list adoption is narrow: exact desired or known published
predecessor bytes, expected root ownership/modes, protected symlink-free ancestors. Legacy
first-init adoption offers exact paths and rechecks bytes/targets/config/metadata after consent;
present equality means management from now, never proof of historical ownership. Persistence
link UID and group-name-resolved GID are checked independently; plain-file numeric ownership
follows prior writer.

JSON adoption can reconstruct old owned template values and protected semantic digest, but
adoption is distinct from convergence to new defaults. TOML schema 1 uses raw SHA and requires
trusted historical shipped template, owned pointer/kind set, and matching current managed
values; comments alone are not owned drift. Owning integrations retain historical assets, core
stays generic. Status/enable/disable do not adopt unknown ownership. Release checks this before
refreshing config, with durable desired scope and same reconciler for recovery. Retained schema
1 baseline remains exact; protected companion retirement proof under the same lock is necessary
for removal. Receipt-only restoration preserves document/baseline bytes, never rewrites
release/journal.

Rationale: [controller desired/observed
separation](https://kubernetes.io/docs/concepts/architecture/controller/), [preserving user
configuration](https://www.debian.org/doc/debian-policy/ch-files.html#configuration-files),
[executing reviewed
plans](https://developer.hashicorp.com/terraform/cli/commands/apply#saved-plan-mode), and [one
declarative authority](https://fluxcd.io/flux/faq/#why-are-kubectl-edits-rolled-back-by-flux).
These are references, not dependencies.

<a id="operation-plans-and-confirmation"></a>

## Operation plans and confirmation

`config/commands.registry` is the single public metadata source for aliases/visibility/remote
policy/effects/help/completion/options/verbs. One Go handler-family resolver and
prepared-command pipeline owns canonical args/context/typed state/plan/execute/cleanup. CLI and
RPC share prepare/execute; direct boundary owns preview/prompt/audit, RPC bounded single-use
storage and wire errors. New public mutating commands enter this pipeline or have an explicit
dedicated non-RPC reason. Sessions, generic profile resources, and protected credential payloads
retain their own workflows. No plugin framework/reflection/generic DAG/second command allowlist
is needed.

Policy follows actual effect, blast radius, and recovery: exactly never, prompt-default-yes, or
prompt-default-no. Reads/navigation/session entry/bounded operational writes/no-op need no
prompt. Important changes with inverse/rollback/simple recreation default yes; irreversible
meaningful-state loss without simple recovery defaults no. Missing/unknown/contradictory
metadata fails closed. Required sequence is read-only assessment/plan → one prompt → apply/sudo
→ verify. No pre-consent target mutation/lease/secret transfer. Explicit automation consent
answers only prompt, never stale/destructive guards or verification. Empty Enter accepts yes
only on interactive TTY; EOF/non-TTY without explicit consent rejects. Root reexec follows
consent and never repeats it; profile handlers do not add child prompts.

Owner-built exact plans bind scope/digest/expiry in one SSH session. Human/RPC share immutable
step projection; owning runners retain check/apply/verify and specialized recovery.
Fresh/stopped targets may require bounded conditional steps: preview distinguishes observed
apply/skip from conditional. Unknown observation never means unknown metadata/target/desired.
After prerequisite, live guard allows only old-scope effect or skip. New work/desired or worse
recovery requires stale/new plan. Partial apply then skip is allowed, but the operation is not a
general transaction. Existing recovery covers only approved partial effects; durable
release/config authorization survives outer plan expiry/disconnect. Protected credential
payload/passphrase never enters RPC.

<a id="release-transition-and-integrity"></a>

## Release transition and integrity

One Go ReleaseTransition owns exact plan/consent binding/migrations/journal/stable-link
activation/recovery/reconcilers. Installer validates/publishes immutable candidate; ordinary CLI
startup does not implicitly finalize. Public states are ready, migration-required, recovering,
operator-action-required. Durable intent plus reobserved facts selects recovery. Domain epochs
prevent reopening completed one-time migrations; repeatable activation reconciles derived
resources. Stable typed registry IDs and compiled catalog define semantics/blast radius.

Known nonsecret settings may canonicalize/reset in the assessed update plan under ordinary
policy, without another reset flag. Unknown/secret/inherited/ambiguous state blocks before
mutation. After activation default recovery goes forward; rollback is a new assessed goal within
proven compatibility horizon. Readiness checks return state/diagnostics; they print nothing.
Outcome warnings render once at operation boundary. Expected drift is not a warning. See
[Release migrations](../control-plane.md#release-migrations) and [transition
design](resumable-release-transition-design.md).

Artifact protection covers corruption/publication errors with existing SHA-256,
manifest/provenance, safe unpack, rename publication, stale guards, pinned root, sealed engine.
Operator/same-UID processes are trusted and release tree is not concurrently edited. Every
regular runtime file occurs exactly once in manifest except root runtime-files manifest; nested
names are ordinary entries. Symlinks/special files reject. Corrupt republication preserves
bytes/stable links while removing temporaries. Process V1/journal V2 meanings remain frozen.
Independent trust anchors/TUF/signatures/root-owned helper/fs-verity/FD-backed trees/coordinated
same-UID replacement defense are outside accepted scope; chmod/rehash wrappers would add no
promised guarantee.

Future publication uses [GitHub immutable
releases](https://docs.github.com/en/code-security/concepts/supply-chain-security/immutable-releases),
with corrections as new patch versions. Existing release creation uploads full assets in draft
then publishes; no wrapper. Enabled setting does not prove the next publication's immutable
behavior; actual publication remains its own gate.

**Important retained compatibility.** Migration kind identities are append-only, chosen from
durable source layout, never capability probing/error fallback. Candidate engine always reads
its compatible manifest; retained payload is explicit, protected, exactly resolved within
release store. Ordinary execution clears stale payload override; only retained-adapter
construction projects known legacy aliases. Child clears inherited config-loaded and re-resolves
explicitly selected yard context, preventing default overrides from replacing identity;
internal-child marker prevents recursion.

Current systemd convergence requires observed loaded/no pending daemon reload plus exact
bytes/enablement. Do not re-enable already enabled units merely to hide missing reload. Exact
rollback to published v0.7.2 permits its known systemd 255 bad-setting only after exact old
bytes and no pending reload; candidate/current verification is not weakened. Legacy layout 1
broker reversal defers to owner reverse because topology predates that typed resource; candidate
tears down its own temporary topology, exact retained executable initializes/checks restored
legacy. Only executable regular files inside release store are eligible; no foreign/symlink
target. Retained failure keeps rolling-back for resumable retry. Journaled before-state, not
live probing, restores legacy desired power between init/check.

**Interrupted activation with changed inputs.** Current journal binds activation scope. Changed
inputs may yield plan-stale; file-bound verified settings receipts can also yield
migration-stale. New consent alone cannot bypass either, and deleting/editing journal is
forbidden. Exactly restored original inputs may resume existing authorization. Current records
contain a hash of observation scope, not a complete frozen copy of original inputs; they cannot
safely support arbitrary replacement or silent settings reset. No generic activation-repair
bypass is promised. Unknown partial mutation, unfinished source/settings work, ownership
conflict, and corrupt/foreign state require supported recovery rather than invented convergence.

<a id="resources-and-test-vm-admission"></a>

## Shared resources and test-VM admission

Pool → slot → lease contracts share status/acquire/renew/release, bounded wait,
owner/purpose/timestamps, revoke, and
provisioning/available/held/draining/quarantined/recovering/unavailable states. Backends remain
resource-owned: Android adapter, root test broker/SSH fencing, credential broker. Shared
semantics/conformance do not require a common daemon/database. Lease timing and resource limits
belong to the owning backend; the test-VM runner uses minute heartbeats and ten-minute expiry.

Test pool defaults to two configurable outer slots shared across environment types. Each acquire
atomically reserves an explicitly chosen slot and creates fresh disposable VM disks from a
versioned immutable base. Standard allocations use one or two guests; Android allocations use
one larger guest. Release/expiry/revoke fences SSH and deletes disposable guest disks. A new
lease never continues the previous guest state. Increase adds empty slots; shrink fails closed
while retiring slots are held, provisioning, draining, or quarantined. Acquire never starts or
stops the outer allocation yard. Broker/facade services follow outer yard power; inner VMs
remain no-autostart. Explicit retained legacy leases are a compatibility exception, not the
normal allocation contract.

Ordinary L1 agents receive only bounded forced-command facade and lease SSH into their own
disposable guests. No privileged outer-yard socket/state/Incus or host endpoint access. Public
egress is allowed, while owner/outer/private/metadata/other-slot networks are denied. Within
leased disposable guests agents have unrestricted root and may create/remove nested
yards/brokers/leases. Inner namespaces do not inherit outer slot numbers.

Exact slot is required for run/SSH/boundary, not prepare/status/help. Busy/invalid/unknown fails
safely with stable redacted reason. Bounded wait retries same slot only after unambiguous
no-allocation; unknown acquire never retries. No neighbor selection or affinity. Verify returned
slot before guest transport. Human attribution is yard/canonical project/run/purpose,
client-reported and never authorization; hidden keys/lease identities remain
authoritative/redacted. Provenance metadata does not authorize resuming a released guest.
Removed slotless grammar/downgrade must not be resurrected as recovery API.

Verified host physical/cgroup MemAvailable accounts for occupied RAM. Reserve separately only
unfinished starts; held/stopped guests and empty slots have no RAM promises. Cancellation/expiry
retains in-flight reserve until verified cleanup. Existing Reserved/ReadyAt, atomic admission,
the default 4 GiB host reserve, both samples, bounded builder peak, and disk accounting suffice;
no per-VM resident/PSS probes/new persisted schema. Unknown download/peak measurements are never
reported as zero/measured limits. Ordinary smoke requests one standard VM; pair only for
both-guest checks. See [Test VMs](../test-vms.md).

Only proven test-owned fixtures are automatically deleted. Unknown unmarked retained
yards/archives require explicit ownership/restore/delete decisions. Existing source/default
profiles/networks/images and operator allocation/transport yard remain outside fixture cleanup.

<a id="credentials-and-memory"></a>

## Credentials, instructions, transcripts, and memory

Subscription identity is server-side account identity, not a shared writable credentials file.
Each host/L1/L2 has its own default mutable auth store and manual login. OAuth rotation/in-place
refresh makes concurrent copied/shared stores unsafe; use private directory storage, not file
binds whose inode can be replaced. Teardown removes rootfs-local coding credentials; login must
be repeated. Subyard neither seeds coding OAuth nor includes it in static encrypted ledger.

Global instructions may be copied at provision; agent settings/rules use yard defaults/typed
overrides. Transcript/session-only mounts are separate from credentials and may survive teardown
for usage reporting. Agent memory is not a Subyard per-agent managed object or command family;
profiles may choose shared/dedicated mounts. Credential separation remains mandatory even when
other session/memory material is shared.

Static staging/QA secrets use one physical-L0 signed SOPS/age ledger under configuration-root
keys, outside repositories/yard host base/L1. Yard is consumer/assignment, not crypto identity.
Normal teardown retains ledger/identities. Shared and physically separate local-only ledgers are
append-only encrypted/signed revision/commit stores. Init idempotently establishes them.
Explicit trust mutually enrolls public recipients/signers; known-route side active with default
auto-sync, inbound no-return route passive/respond-only. Refresh never silently demotes active.
Six-hour timer/24-hour stale warning are default. Code/shell never secretly sync; explicit
catch-up does.

Safe DAG merges include union/descendant/same-value/revoke-wins. Different concurrent values
remain multihead until explicit resolution and block materialization. Exclusive credentials have
authority/assignment epochs and fail-closed supported staging handoff; hostile stale runtime
fencing would require separate proxy enforcement. Mode 0600 files are atomic consumers, not
portable source authority. Runtime secrets are exact-target read-only files, never public
config/argv/Docker environment/cache. Empty generic host-secret mount does not imply coding
credentials belong there. See [Keys](../keys.md).

<a id="coding-tools-and-backend-clients"></a>

## Coding tools and backend clients

Direct normal launches are supported. Subyard does not retain old agent versions, impose a
version allowlist, or require a launcher. Installer selects current version with integrity
checks. New version alone is not incompatibility: capabilities/policy mechanisms determine
readiness and actionable diagnostics. The narrow Codex home-rule matcher verifies rules only; no
effective client-policy reconstruction. Claude/OpenCode/pi approval enforcement remains
explicitly unverified where native probe is absent; lack of probe neither bans launch nor proves
enforcement.

One L1 backend server can serve many projects; same-user per-project servers are not an
isolation boundary. Subyard may supply connection bootstrap, never app-session control plane.
Disconnect does not cancel turns or autoapprove questions; first backend MVP promises no
in-flight survival across server/yard/host restart. Managed restarts must respect live work and
upstream shutdown behavior.

Paseo is an optional installed headless package, absent from shipped defaults: one L1 daemon and
client-side multi-yard connections. It depends on current stable Codex, while its managed
headless runtime is pinned. Hosted relay uses loopback local API/outbound WSS and an explicitly
issued sensitive end-to-end pairing offer through short SSH command. No permanent controller
tunnel or public yard listener is needed. Offer never enters state/logs/RPC. Relay sees
transport metadata and remains availability dependency despite content encryption. Agent
latest-version policy does not authorize hidden restarts during live work. Physical Desktop
acceptance is distinct from package/VM readiness; see [Paseo](../paseo.md).

<a id="github-app-token-broker"></a>

## GitHub App token broker

Long-lived App private keys/PATs/refresh tokens stay on owner in protected encrypted-ledger
consumer. Short-lived installation token may enter L1 command environment without Git/gh
auth-store persistence, remaining valid until GitHub expiry (up to an hour), not revoked at
command completion. Installed App permissions/repositories define scope. Per-call approval/typed
operation allowlists/hard push boundary are outside current credential contract; agent
commit/push prompts remain separate policy. See [GitHub](../github.md).

<a id="orca-registration-and-runtime"></a>

## Orca registration and runtime

One project maps to one group with its existing canonical src root as Git/folder and every
nested checkout, each with independent path/diff. Repo/checkout/project are different; worktrees
each have entries. Initial names derive project/relative paths; preserve manual
names/colors/order. Subyard owns membership through stable IDs and moves only related entries
out of mixed manual groups. Folder/Git type changes in place preserve ID/path/group/session and
never initialize/delete Git.

Known-root maintenance is separate from bounded periodic discovery. Support at least 1,000 roots
without billion-file caches blocking status/hooks/activation. One profile timer persists batched
FIFO/directory-cookie traversal; finished trees rescan while other caches continue.
Ignored/hidden/dependency/fixture directories remain discoverable; ordinary files are not
read/watched individually, directory symlinks and Git internals are not traversed.
Submodule/worktree Git files work. Lifecycle/explicit commands immediately add canonical root
and reconcile known roots without full scan. No automatic sessions/diff aggregator.

Sync removes proven missing managed checkout rows/empty removed-project groups, never disk
files. Preserve live/saved sessions, foreign/remote records, explicit unknown membership, manual
nested folders, existing directories that lost Git. Root errors/missing registered canonical
root disable cleanup; unfinished scan proves no absence. Legacy ungrouped local Git paths
beneath active successfully reconciled canonical root count as managed, including
indistinguishable manual ungrouped entries.

Stale terminal null/pending layouts alone do not retain missing checkout when complete fresh PTY
census lacks it. Live/orphan terminals, other tabs/linked checkouts/available agent
sessions/drafts/buffers/uncertainty survive. Close exact stale native parent tabs before repo
removal to avoid nonempty cached snapshots. Recheck snapshot/liveness/runtime/repo
ownership/path absence before every send; unknown writes require readback, never resend. Native
API lacks atomic fence against concurrent null-tab resurrection between check and close;
auto-cleanup explicitly accepts that narrow race with repeat checks. No whole-server restart or
byte patches for pruning.

Keep stock runtime. Subyard admission caps new repo rows at 1,000 including folder/remote rows,
without performance claim. Existing rows are retained/repaired even above limit; never evict for
room. Unknown adds reserve capacity until resolved/runtime change. Cached-full refusal avoids
one probe per rejected root; actual add uses fresh catalog. No empty group when no root
admitted. Native clients remain independent. Cleanup precedes admission refusal; deferred roots
remain explicit/incomplete, accepted on later freed capacity. Automatic sync reads repo/group
catalogs rather than polling worktree.list; periodic discovery does not run settings
convergence.

Project-action per-repo errors yield warnings without aborting unrelated work; explicit sync
fails on incomplete registration, status explains cause. Restart never emits pairing capability
but stock startup may recreate protected unused offer; existing grants/state/projects survive,
unused copied offer may expire. Stock shutdown can kill PTY/agent process trees, so preserved
layouts/grants do not promise running-process preservation. Restart/update follows completed
work or explicit consent to stop it.

Bulk 1,000/1,400-repo/100,001-file diagnostics belong only to explicit manual controller on
leased disposable VM, not ordinary/profile/release acceptance or live checkout. Matching
lease/deadlines/marker cleanup/baseline IDs and unknown-write guards are mandatory. Numerical
native CPU/RPC/worktree costs remain upstream limitation; admission is no claim to fix them. See
[Orca](../orca.md).

<a id="android-profile"></a>

## Android profile

Android owns SDK/JDK/KVM/emulator devices/cache/graphics, never core branches. L1 agents share
writable Gradle using native coordination; each L2 has its own writable Gradle home. Optional
shared read-only dependencies are allowed; cross-L2 writable Gradle mount is not. SDK
maintenance is serialized. Project sdk.dir may outrank environment; profile must reconcile guest
paths without changing host-bound files. Required AVD presets avoid degenerate geometry; clean
per-allocation Android state prevents stale app/keystore databases.

Default shared on-demand pool has two slots, one emulator/exclusive lease each. No hidden
RAM-based sizing; insufficient resources fail explicitly. Release/expiry/revoke closes
access/stops helpers and publishes availability only after verified stop. Prepare verified
API/images with idle pool before allocations; maintenance lock excludes active
allocations/provision. Single acquire may prepare missing image. Run holds lease for command;
prune touches only reproducible unused artifacts with dry-run/lease/download/retained-AVD
guards. Per-agent modes remain later extensions.

Default software-gles is headless SwiftShader/OpenGL ES with guest Vulkan disabled. Software
retains software Vulkan; host explicitly selects hardware. No hidden fallback; all modes require
KVM for CPU. Hardware headless rendering needs profile-owned display bridge rather than assuming
render-node EGL suffices for emulator GLX; no host GUI/socket access is required by software
default.

Phone 1080×1920/420dpi; tablet 800×1280/160dpi preserves tablet dp layout below automatic
large-screen RAM floor. Each 2560 MiB guest plus 1024 MiB runtime, 512 MiB yard reserve totals
7680 MiB; fixed presets, never silent resizing. One 8 GiB/40 GiB VM is agreed software-GLES
acceptance scope; separate nested-broker pair requirement does not apply. See
[Android](../android.md) and [emulator
source](https://android.googlesource.com/platform/external/qemu/+/refs/heads/emu-main-dev/android/android-emu/android/main-common.c).

<a id="staging-and-qa"></a>

## Staging and QA boundaries

Development tests are distinct from live provider/bot tests. Application-specific staging launch
details/credentials belong to local owning-profile configuration, never public generic profile
values. A staging zone and a coding project are different identities. Existing live-bound
application test workflows may include uncommitted/submodule changes and build writes back to
the source tree; Subyard does not promise frozen source artifacts or independent per-run
application state. Optional canonical application baselines are outside core lifecycle
guarantees.

Production identity/config/state/bot must remain distinct and be validated by the owning
application before live testing. Secrets belong to runtime rather than build, but receiving code
can read its token. Rootful execution must not claim rootless UID credential isolation. A
separate runner can enforce shared-resource arbitration and guard/teardown boundaries; it is not
inherently required for one agent. Simulated after-hours approval tests need restartable
gateway/durable state/clock-TTL simulation, not a long-running service. These are integration
constraints, not a generic shipped staging service or CLI promise.

Gateway lock (one config process), active-runner (one shared-state writer), and scarce bot lease
(one poller identity) are distinct. Multiple agents need bot pool or serialization; TTL alone
cannot fence stale runtime, so the owning lifecycle must stop the holder before next grant.
Subscription model auth needs manual private writable single-writer storage, not a read-only
OAuth copy. Native credential distribution does not hide tokens from receiving workers.

For integrations using the upstream self-hostable Convex QA broker, the native harness's
pool/lease API remains application-owned. Workers share a broker origin independent of checkout
copies; driver and SUT/group resources differ. Acquire distributes full payload, so moving the
database does not make a token hider. L1 root can access yard-owned runtime data, while L2
without yard socket has only its client API. Protected durable static inputs and their
materialized consumers are distinct. Broker availability and application-specific live behavior
remain owning-profile/external acceptance, not core guarantees.

<a id="profile-boundaries"></a>

## Shipped profiles and extension ownership

Core stays generic; profiles own dependency versions/images/cache/env/devices/native
handlers/tests. Project Dockerfile/lockfile is source authority for project toolchains; do not
duplicate project pins or bake private workspace paths into public profile. Nonsecret profile
contract may be committed; secret input is ignored/read-only target material, never injected
through Docker environment. Desired configuration union requests L1 capabilities and separately
supplies L2-only mounts.

Shipped profile.json schema 1 extends existing package contract, not another planner/executable
registry. Core loads only shipped roots, validates unknown versions/fields/unsafe
paths/collisions and common types, owns orchestration/confirmation/protected storage. Profile
owns native hooks/credential mappings/setup/tests. Separate declared binaries are built, not
linked into main engine. Existing persisted keys/service paths/activation IDs remain compatible.
Interactive snapshot is rechecked before apply. Typed settings/runtime/guest hooks use same
operation-local catalog and config loader.

`yard provision <profile>` enables/reconciles that profile on the selected yard under one
confirmation; a new selection is saved locally even with registered Git source, preserving other
profiles. Profiles without install hooks still reconcile prerequisites, subject to
dedicated-role restrictions. Dedicated test yards are separately configured resources. Special
no-coding-tools capability retains profile broker/keys/inner resources. Core tests use synthetic
contracts; profile-specific acceptance belongs to profile and must remain release-covered. See
[Profile extensions](../control-plane.md#profile-extensions) and
[provision](../cli-reference.md#provision).

Hermes profile is substrate only: users/permissions/network/persistence/generic OS
prerequisites. Operator/upstream owns app
installation/version/layout/update/config/auth/components/gateway/voice/cron/dashboard; profile
never reads/modifies/runs its dev-owned tree as root. App state/rootfs survives ordinary
provision/start-stop/reboot until assessed teardown. Tailscale ends on L0; SSH or explicit typed
owner-address proxy reaches loopback, with no direct L1 ingress or app-auth inspection.

Amnezia explicit init installs prerequisites but leaves fresh VPN stopped until first explicit
yard start. After VPN down, provision/init/start/reboot preserve disabled intent until VPN up.
Selecting profile alone never publishes VPN; external exposure follows confirmation policy. See
[Amnezia](../amnezia.md).

AI Observer installation readiness proves exact owned runtime/active service, separate from
advisory health. Initial history scan can delay HTTP while sole watcher works; re-provision does
not restart a valid starting service. Watcher commits file data/offset transactionally, skips
unchanged committed files, may retry interrupted files; progress uses offset/message/checkpoint
aggregates, not record_count/CPU/DB growth alone. Health is ready/starting/failed/unknown
without activation-fingerprint change. Rollback is for proven startup/spec failure, not absent
HTTP. Full first-import budget cannot be inferred from a quick later retry.

<a id="decision-47"></a>

## Provision backend: Go and Bash

Go owns preparation, typed observations, planning, orchestration, and verification. Bash and
profile adapters own the required system operations. Ansible is not an additional in-yard
backend: another runtime, playbook, inventory, and check/apply path would add lifecycle
complexity and risk a second desired-state authority without replacing the existing operation
boundary.

Package, mode, and service drift belong to typed component observations in the same Go
check/plan/apply/verify path. Reconsider another backend only when a substantially larger
recurring declarative slice demonstrates which production paths it removes, preserves one
authority, and supplies a complete release, offline, and uninstall contract. That includes
dependency integrity and licensing. YAML convenience alone is insufficient.

<a id="veranda-direction"></a>

## Veranda: accepted direction

The current Veranda candidate includes local and pinned SSH fleet views, native connection/trust
management and typed owner operations. Its architecture is Tauri 2 with a thin native Rust shell
and Svelte/TypeScript over versioned Yard RPC. Platform, reliability and resource acceptance remain
pending; Windows/macOS are intended to reach Linux owners remotely. Minimize resources according to [architecture
budgets](control-plane-architecture.md#veranda-resource-budgets). Slint is a fallback only after
same-screen startup, idle RAM/CPU, and keyboard comparisons on target OS demonstrate value;
replacing UI does not require Go/RPC rewrite.

The accepted distribution contract keeps Yard/Veranda in one public monorepo/product SemVer
release with separate artifacts. Core bootstrap/update remains CLI/engine-only; GUI installation
is separate. Official pair compatibility requires matching product version; RPC
versions/capabilities negotiate independently, without a promised arbitrary adjacent-version
matrix. Shared behavior must have conformance coverage against the [RPC
contract](../control-plane.md#rpc); GUI source lives in `veranda/`.

Profile editing uses current shipped optional schema 1
and descriptorless/resource-only profiles without depending on a future registry. Owner
projection must separate selection from observed provision; deselection does not delete
artifacts. Product/schema/capability mismatch must give actionable same-release guidance rather
than frontend migration/fallback.

The native connection/trust requirement assigns an app-local store to the Rust shell on every
OS: authoritative HostID, destination, credential references, and pinned public keys. CLI
controller store remains a separate authority. System SSH agent/keychain references
authenticate; private payload must never enter UI/state. Native writes require typed consent,
atomic stale guards, and verification; owner mutations use exact-plan engine. Focused GUI
development can proceed before full P0, while unresolved defects/obligations remain
feature-release prerequisites and required release gates stand.

<a id="evidence-and-acceptance-scope"></a>

## Evidence and acceptance limits

Acceptance proves the specific candidate and boundary exercised. Host-free checks do not prove
installed lifecycle; packaging does not prove actual immutable publication; VM tests do not
prove real Desktop/VPN/external account behavior. Native ARM checks run in CI. Core plus all
shipped-profile same-candidate gates remain required for runtime publication; limited current VM
checks do not replace that gate. See [Testing](../testing.md), [Test VMs](../test-vms.md), and
[Development](../development.md).

Choose the smallest allocation that proves the behavior: one standard VM for independent
single-host checks, a pair for checks that coordinate two hosts, and the dedicated larger guest
for Android acceptance. A passing focused check does not establish full lifecycle acceptance
or external-account behavior. The owning guide and profile declare the required evidence.

Security exemptions must be current graph-specific triage, never copied permanently from
historical counts. Client-only Incus import does not make daemon-side findings runtime client
vulnerabilities; new/changed affected-symbol findings require fresh triage and block release
until resolved. Current toolchain/dependency versions belong to source/lockfiles, not stale
decision snapshots.

<a id="glossary"></a>

## Glossary

| Term | Meaning |
| --- | --- |
| Subyard | Development isolation product; yard CLI, sy alias |
| Veranda | Graphical client with a read-only local fleet; remote and mutating features remain design requirements |
| OwnerHost / HostID | Physical owner authority and its stable operator-controlled identity |
| YardRef / YardSelector | HostID+yard identity / user selection input |
| AccessKind / YardKind | Local-remote transport relationship / container-VM runtime kind |
| Yard (L1) | Owner-scoped Incus environment; runtime instance/image is not identity |
| Incus project | Restricted runtime namespace, distinct from code project |
| Project / workspace | Owner-registered source copy/bind/clone under canonical project identity |
| ProjectID | Canonical name for new records; retained legacy IDs/paths stay compatible |
| ProjectEnv (L2) | Project Docker environment, distinct from the coding agent it runs |
| EnvironmentProfile | Dependency/image/cache/env/device and owning-hook contract |
| CodingToolIntegration | Selected coding-tool integration; requested roots differ from effective dependencies |
| Effective configuration | Typed shipped defaults, persistent settings, and command overrides resolved with provenance |
| Configuration root | Storage root with separate settings/secrets/consumers/state/tool roles |
| Desired / observed | Requested authoritative state / rechecked actual facts |
| Exact operation plan | Owner-built bounded reviewed effects and conditions bound to authorized execute |
| Conditional step | Preauthorized scope whose apply/skip is decided after prerequisite observation |
| Recovery journal | Durable intent/evidence/authorization, not a disposable status file |
| Pool / slot / lease | Shared-resource capacity/unit/exclusive bounded allocation |
| Fencing | Preventing stale holders from using resource after transfer/revocation |
| Shared writable cache | Deliberate trusted-peer cache, subject to tool concurrency rules |
| Gradle home | L1-shared or L2-private writable cache; never cross-L2 writable mount |
| AVD | Android virtual device with clean per-allocation state and retained downloaded bases |
| Idmapped mount / shift | UID/GID-compatible host/guest bind mapping |
| Proxy device | Incus host/guest forwarding; loopback or explicitly assessed exposure |
| scrcpy / ADB proxy | Android viewing/control via ADB and scoped forwarding |
| Host session | Operator-supplied account identity; not shared mutable credential file |
| Static credential ledger | Signed/encrypted host authority, separate from coding OAuth stores |
| Materialized consumer | Protected runtime file derived from authoritative settings/ledger |
| Agent memory | Upstream/profile-selected shared or private data, not Subyard lifecycle entity |
| Staging zone | Shared named live-test configuration/lifecycle, independent of coding project identity |
| Token distributor / hider | Gives worker credential payload / separately retains real token behind proxy |
| Canonical / ephemeral | Optional baseline / current uncommitted live-bound test execution |
| GUI passthrough | Optional disabled host-display debugging, not baseline agent access |
