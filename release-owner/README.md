# Release owner

This is the production release-owner crate. Its private operation entrypoint is not
selected by the product facade and advertises no writer capability. Existing Go owners
remain the default production readers, writers and recovery owners. The native [pilot](../dev/release-owner-pilot/README.md)
remains a separate disposable fixture; its record is never imported here.

Run `bash release-owner/check.sh` (or `make verify-release-owner-foundation`). It exports
the existing Go constructors/codecs into ignored `.build/release-owner-foundation`,
checks canonical bytes and semantic acceptance, runs rooted native store process cuts,
compiler controls and the unchanged exhaustive history comparison, and cross-compiles
the library for both Linux musl architectures. It requires Rust stable with those std
targets. It does not install a toolchain. ARM execution remains a CI responsibility.

`history` is the single pure kernel previously exercised by the history comparison;
the development crate now re-exports it. Borrowed checked contents cannot be mutated;
consuming semantic edges express local advancement. Re-evaluating an immutable history
is legitimate and grants no authorization. Private checked record constructors validate
the actual registry/catalog, migration history, ledger/checkpoint prefix and projection
pins, journal intent/evidence/archive bindings and ProcessV1 wire values. Checked values
are neither deserializable consent nor mutation authority. The operation reducer is under development; the checked foundation alone does not
prove release convergence or readiness.

Canonical encoding uses typed struct field order, sorted maps, explicit nil/empty
collections and `omitempty`, Go HTML and U+2028/U+2029 escaping. ProcessV1, standalone
evidence, compatibility evidence, ledger/checkpoint and durable journal/archive append
LF; low-level journal/archive encoding omits it. Registry and catalog encoding omit LF.
The journal artifact digest continues to mean the runtime manifest digest. Intent and
prefix digests use their separate frozen structures. Raw registry/projection/store
fingerprints always hash the original bytes, never a canonicalized substitute.

Historical input now passes through the retained frozen syntax bridge described below.
`Store::open_with_reader` requires its complete `FrozenReader`; the separate
`open_canonical_fixture`, `Projection::from_canonical` and owner receipt fixture
constructor remain explicit test/pure-constructor boundaries. Raw wire deserialization
alone never creates a checked record. The legacy ProcessV1 facade header grammar stays
with its existing Go decoder; the bridge does not redefine that protocol.

The protected store uses closed current production record addresses and a private
exclusive lease on `migrations/update.lock`. Read-only opening never creates an absent
root. Descendants use pinned-directory `openat`, nofollow, nonblocking leaf opens,
regular-file/UID/mode/single-link checks, bounded reads and exact created modes. Roots
retain Go's non-writable-by-others permission rule; owned descendants require 0700,
files 0600. Snapshot revision is existence plus bytes/digest, so a same-byte inode
replacement remains valid. Publication uses exact observation, exclusive pending file,
prefix retry, file/directory fsync, reobservation and atomic rename (noreplace for absent
records). Immutable same-byte retries succeed; different bytes collide. Closing a lease
never explicitly unlocks an OFD that a supervised child might still hold. This library
does not yet expose child delegation or prove async Incus quiescence.

Native `/tmp` tests cover byte-stale CAS, immutable collisions, pending prefixes and
orphan CAS staging, permission/symlink/hardlink/FIFO/size refusal, absent-root reads,
independent lock contention and kill/fresh-process continuation at four publication cuts.
Foreign-UID refusal runs when the fixture user can chown; other users report that physical
case unavailable. Process death on a running kernel is not a power-loss durability test.
The physical store remains separate from the checked graph/retention decisions.
The release reducer must still own when those decisions are authorized.

| Contract | Current reader/writer and continuation |
| --- | --- |
| Existing ProcessV1, JournalV2/ArchiveV1, compatibility evidence, RegistryV2/catalog, LedgerV2/checkpointV1 | Exact Go owners remain active; Rust library admits frozen syntax through the bounded bridge and checks policy; no runtime route |
| Recovery schema 2, activation receipts V1/V2, lifecycle/frontier V1, migration-owner receipt V1 | Checked Rust values, frozen record admission and protected local policy; existing Go owners remain active, no Rust capabilities advertised |
| Private pilot record v1 | Pilot Rust owner only; never imported or promoted |

Next work is the complete release observation/reducer and
closed component-native recipes for every shipped activation owner, then verified
manifest-bound facade/delivery/custody and same-candidate compatibility/VM acceptance.
This library does not establish full migration equivalence or production readiness.

The recovery tranche is checked with `make verify-release-owner-recovery`. It extends
that same Go exporter with real schema/plan constructors, before/after store retirement,
cancellation and pruning outcomes, ordinary graph selections/refusals, retained typed
sidecars and actual `newJournal` results. A read-only test probe compares inventory hashes
on the original Go fixture inodes; cloned fixtures cannot establish metadata hash parity.
It neither runs a product helper nor calls Go policy on behalf of a Rust owner.

Recovery contracts remain distinct: activation-only receipt V1, generation-bound V2
with cancellation/frontier/completion witnesses, frozen 0.11.1 post-activation archive,
and the exact affected 0.21.0 migration-owner replacement. Go byte slices use padded
base64, preserving null versus an empty string. Schema 2 has no LF, including its
explicit empty runtime probe contract. Plan/resume and lifecycle bindings use their
frozen separate digest structures. Semver parsing uses `semver`, additionally enforcing
Go's u64 bound for numeric prerelease identifiers. Dependencies `base64` 0.22.1 and
`semver` 1.0.28 are locked; their upstream contracts are documented at
[base64](https://docs.rs/base64/0.22.1/base64/) and
[semver](https://docs.rs/semver/1.0.28/semver/struct.Version.html).

Protected recovery namespaces count both pending and published bytes before retaining
payloads: legacy V1 allows 256 logical records and 512 MiB, while V2 and migration-owner
namespaces allow 512 logical records and 32 MiB. Every record remains at most 1 MiB,
including base64 and LF. A migration-owner horizon admits at most one receipt. Source
inventory is capped at 8192 entries and 32 MiB of record bytes; its internal metadata
encoding is capped separately at 8 MiB. Source inspection refuses unsupported grammar,
unsafe files and pending publications. Its narrowly typed migration-owner source check
validates and excludes its own publication branch and intended pending journal, binding
all other original bytes and metadata. It checks the completed historical ledger steps,
retained triples and typed before-images, not the state of historical runtime resources.
`SourceEvidence` proves local protected source closure only; exact official asset FDs,
platform/version facts and prospective native component facts remain separate work.

Retirement uses checked closure/reference decisions and exact byte snapshots under the
update lease. Pending frontier generations are replayed durably before further advance.
Cancellation accepts only an exact canonical unselected prepared journal; it preserves
conflicting or partial bytes. Required migration-owner completion is captured before a
journal overwrite, after revalidating the frozen projection, predecessor evidence triples,
retained history and exact successor. This proof validates the whole owner completion
namespace before publication and requires the predecessor's narrower evidence/recovery
tree; ordinary retained source history keeps its separate grammar. Recovery retention
failure is returned explicitly and remains sticky for `CleanupRecovery` on that store
instance. Selected recovery continuation validates its family and selected terminal and
frontier without demanding that unrelated historical archives parse successfully.
One complete owner/completion namespace validator serves continuation, graph and source
inventory admission, including before the owner branch is excluded from source bindings.
A completion requires its actual durable receipt; a supplied checked receipt cannot
replace it. Both namespaces may be empty before initial receipt publication.
Native tests kill and restart independent processes around receipt/frontier/cancellation,
completion, private publication and partial cleanup. Full grant/session/release-reducer
admission and all component-native activation effects remain to be integrated before a
production writer is enabled.

The eventual owner route must preserve exact supported historical targets for fresh
update/rollback as well as pending continuation. Target declaration selects the exact
owner before inspection/consent; a Rust-declared target must never fall back after an
error. This crate currently implements no owner dispatch route.

The store orchestration resolves exact V1/V2 recovery and frozen ordinary supersession
identities, reuses matching reservations, cancels only unselected generations, compacts
unreferenced older cancellation witnesses and refreshes inventories before publication.
Completion capture can free eligible archive capacity; cleanup reserves its completion
slot before pruning. It follows the current Go ordering, including its instance-local
sticky capture-failure rule. A fresh process still reconstructs durable evidence rather
than treating that flag as a persistent record or a new authorization.

Ordinary cleanup protects selected/prepared and owner-retained roots and deletes whole
unreferenced components successor first, at most 32 transactions per call. It snapshots
and reobserves closed transaction trees under the update lease, using pinned directory
FDs and exact byte revisions before unlink/fsync. The finite physical snapshot bounds
are 8192 descendants, 32 MiB of file contents, 1 MiB per file and depth 64; unsupported
file types, modes or ownership refuse before deletion. Bounded empty regular staging
files are valid cleanup inputs and are reobserved with their exact empty bytes. A separate process test kills
cleanup after unlink and continues from the remaining ordinary graph.

`inspect_source` assembles checked current-journal eligibility, ledger/checkpoint
provenance, retained typed evidence, archive authorization chains reaching terminal
closure, owner completion and recovery closure between unchanged inventories. Historical
ledger evidence may use the exact original projection bound by a checked checkpoint;
an arbitrary journal fingerprint cannot supply that anchor. Go-owned cases cover current
and retained historical cuts, closed and unproved source trees without a current journal,
changed anchors, and unchanged bytes on refusal. Historical reads use the frozen reader; original-byte canonicality remains required
where the owning store requires canonical protected evidence.

Store mutation fixtures come from actual Go calls and are emitted as separate bounded
case documents. Capacity tests query Go's eligible deletion groups and compare safe
retention counts plus exact protected/new bytes; incidental map iteration order does not
become a Rust policy table. Each case document is bounded at 64 MiB, separately from the
unchanged 1 MiB production record limit. Earlier review candidates and failed terminal
receipts remain under `.build/release-owner-recovery`; the continuation gate writes its
corpus under `orchestration/`.


## Frozen syntax bridge

`make verify-release-owner-legacy` builds the current Go executable and exercises its
early read-only `_release-owner-codec-v1` mode, before CLI/configuration initialization.
One retained peer serves repeated fixed-kind Normalize requests for each Rust operation.
There is no daemon, network listener, path/argv executor or effect verb. It decodes only
RegistryV2, catalog descriptors, LedgerV2, checkpointV1, JournalV2, evidence,
ArchiveV1, compatibility evidence, recovery receipts V1/V2, lifecycle/frontier,
migration-owner receipts and the existing settings/owner recovery sidecars.

The Go side performs typed `encoding/json` decoding and marshaling, preserving duplicate,
case-folding, null, invalid UTF-8, integer and byte-slice rules. It does not call registry,
ledger, recovery or graph Parse/Validate policy. The existing JournalRecord and
SupersededJournalRecord custom wire hooks remain intentional exceptions: their frozen
per-occurrence decoding/validation and replacement behavior must remain intact, including
intermediate rejection in duplicate embedded journal objects. Rust owns checked history,
recovery, graph and store decisions. Root recovery receipt decoding reports exact raw
presence of `basePlan`, `reservation` and `generation`; Rust applies the V1 refusal only
to that root, preserving the different embedded-receipt contract.

A private, non-deserializable/non-cloneable normalization proof binds record kind,
sequence, original bytes/SHA256, normalized DTO and presence bits to the actual sealed
helper image. Store, graph, source, checkpoint/projection and nested migration-owner
registry reads all use that operation's reader. Original snapshots and fingerprints
remain the CAS/source authority; canonical-required paths compare the original bytes,
not the normalized projection. The owning oracle includes valid alternate registry,
ledger and journal ingress, valid normalization expansion beyond 1 MiB, and original
canonicality refusals for source journals and ordinary archive graphs.

The reader captures an already-open regular, owner-held, single-link helper FD,
rejects writable-by-others modes, copies at most 128 MiB into a sealed memfd and computes
the actual sealed image SHA256. It executes that FD with one fixed mode. This proves
physical image identity/immutability, **not authenticated manifest provenance**. The
future delivery factory must supply the exact manifest-authenticated helper FD before
an owner is enabled; no caller-provided hash or boolean substitutes for that work.

Requests/replies use a fixed 52-byte binary header with magic/version, closed kind,
status/presence bits, monotonic u64 sequence, original SHA256 and payload length.
Original records are nonempty and at most 1 MiB. Projections have a separate 8 MiB bound.
The peer permits one in-flight request, bounds allocation before reading a reply payload,
and checks kind/sequence/hash/status/flags exactly. A private full-duplex socket worker
has a five-second whole-frame deadline; transport failure poisons the peer, closes it,
kills/reaps the read-only child and joins the worker. Syntax refusal is a typed empty
reply with no presence flags and may be followed by another request. Malformed success
JSON, a wrong typed projection or invalid refusal flags also poison and reap the peer
before an optional historical read can discard its error. Input canonicality and root
receipt presence refusals leave a valid peer reusable. Drop also drains/reaps this read-only
peer. These tests do not prove component-native or Incus quiescence.

Before `json.Marshal` can buffer expanded output, Go charges a conservative typed-value
JSON upper bound: string bytes at six times their length, full numeric widths, padded
base64, map keys, field labels and all default/omitted fields. Nil containers are charged
as `null`. The admitted frozen shapes fit 8 MiB: repeated catalog/domain/migration,
checkpoint, plan and before-image entries require validated labels/nonempty IDs, epochs
or fingerprint payloads; their smallest valid encodings cover their fixed default-field
cost, while additional string bytes cost at most six times source bytes. The remaining
fixed wrappers are bounded by the frozen 256 journal-step/plan-item and retained-history
limits. Fields admitting non-ASCII text are also byte-bounded by JournalV2 (including
4096-byte paths and the finite step/resource fields); invalid UTF-8 replacement cannot
create an unbounded valid text collection. This bound requires re-audit when a frozen
DTO/validator changes. Compact invalid
empty-object arrays can be conservatively refused before Marshal even when their eventual
output would be smaller. The fixture also tests an invalid array whose default expansion
would exceed the projection cap.

This pre-Marshal check does **not** bound allocations already made by typed Decode.
Decode retains the existing Go source-driven behavior: at most 1 MiB of original JSON can
materialize many empty typed elements before domain rejection. Its amplification is
finite for these non-recursive closed DTOs, but the 8 MiB projection cap is not a Go heap
or RSS limit. Publication remains separately limited to 1 MiB; normalization and canonical
hash computation do not silently grant permission to publish a larger record. No runtime
speed, CPU or RSS improvement is claimed.

The gate checks all 14 kinds against actual owning Go parsers/constructors, all prior
35 store cases and retained typed sidecars through `FrozenReader`, original raw pins,
repeated-peer reuse, malformed/late/poisoned replies, EOF/nonzero/timeout and compiler
controls. Accepted raw vectors obtain expected projections independently from owning
Go Parse/Validate paths followed by Marshal, rather than from the bridge under test.
Artifacts and native receipts stay in `.build/release-owner-legacy`. Existing
ProcessV1/JournalV2/recovery formats and all production owner routes remain unchanged.


## Inert native operation

`make verify-release-owner-runtime` checks the current operation slice. One retained
Go helper shares the frozen syntax channel and closed component verbs; it never calls
Go release inspection, plan binding, convergence, graph policy or whole activation
reconciliation. Rust prepares immutable semantic recipes before consent, consumes the
prepared handle, acquires the update lease, reobserves the original source/target set,
publishes an immutable prepared bundle before the journal, and advances frozen evidence,
ledger and runtime links. The prepared config component retains its selected payloads
and refuses changed sources before guest writes. Existing ordinary `RefreshConfigs`
callers retain their capture-and-apply behavior.

The native channel receives exact descriptor roles with `SCM_RIGHTS` and
`MSG_CMSG_CLOEXEC`. Inspection receives the pinned config root without acquiring a
mutation lease. Dispatch receives the actual update-lock open-file description after
consent. The helper closes borrowed descriptions without `LOCK_UN`; it runs in a
separate scoped process group. Read-only codec peers may be killed on poison; an armed
custodian instead closes admission and drains accepted work. Helper loss or an
unproved remote result leaves the frozen pending journal and an UNKNOWN invocation.
A desired observation alone cannot clear that state. A checked failed terminal can
prove quiescence but cannot satisfy successful completion or Ready. Official SDK operations record
intent before submission, accepted identities and exact final witnesses; nil `Wait` is insufficient.
Every started fixed command requires its separate matching terminal fact. The armed
Rust peer allows 75 minutes 40 seconds for an armed helper to exit after admission
closes, preserving the existing maximum component refresh allowance plus drain reserve.
An emergency supervisory kill remains UNKNOWN and never counts as drain. Closed
generation fences exist for capable profile/stage components; other opaque command
losses remain UNKNOWN. The shipped guest fence and Rust owner recovery need their
combined physical acceptance; a component-only result does not establish owner Ready.

`migrations/native-custody/v1/<transaction>/` is separate from the frozen journal
namespace. It contains the prepared bundle, per-invocation intent/acceptance/terminal
facts, nested SDK intent/accepted/terminal triples, and fixed-command intent/terminal pairs. The owner writes recipes and intent; the helper
writes only its native facts. Each publication remains at most 1 MiB. A bundle is at
most 64 MiB before base64 framing, split into at most 128 immutable 512 KiB pieces plus
one index. Current checked inventory limits are 256 transactions, 8192 physical files
and 128 MiB. Before dispatch, the owner reserves 2570 physical files and 16 MiB
for the invocation's published and pending facts. Exact SDK triples and command pairs
must form a checked immutable closure before nested facts can be retired. Fresh-process
partial retirement tests retain that closure. Whole-transaction retirement derives protection from checked current/prepared,
ordinary and recovery edges, live frontier reservations/cancellations, and retained
migration-owner closure. An unreferenced historical archive is still readable frozen
history; it does not permanently retain unrelated native payload. Only exact checked
quiescent or finitely resolved invocations, and bundles without native intent, can be
retired. UNKNOWN invocations remain protected. Cleanup processes at most 32 roots,
with one retirement proof stage at a time. That stage retains the original semantic
facts and byte identities before deletion, allowing bounded fresh-process partial
cleanup while refusing foreign residuals. It uses at most 128 MiB and 256 proof pieces;
low space before complete proof publication cannot delete original payload.
An orphan bundle does not authorize mutation.

Fixed native installed recipes retain their exact original Go canonical bytes and
SHA through the existing bounded chunk transport. Rust losslessly packs file contents
by SHA into `migrations/native-prepared/v1/`; native components still interpret all
paths and executable choices. Source payload is deduplicated across yards and
transactions, with no mutable reference-count ledger. The global table admits at most
8192 hash roots, 16384 physical raw pieces including pending copies, 32 MiB per payload
and 128 MiB raw bytes. Its canonical indices consume at most 8 MiB, charged inside the
existing native 128 MiB byte ceiling. Recipe metadata remains in the existing 64 MiB
bundle. One recipe admits at most 8192 files, 64 MiB of raw contents and 96 MiB of
canonical bytes; each individual payload remains bounded at 32 MiB. These are
explicit prepublication capacity limits, not assumptions about how
many graph scopes can be retained; referenced data is never pruned to fit. Together,
the witness namespace, raw table and one retirement proof stage have a transient
384 MiB ceiling. Payload-only orphans carry no authority and are removed only under
the update lease, after checked bundle-reference reconstruction. Original recipe SHA
must match after reconstruction and before native bind/apply.
The private owner-registration recipe can reference that same exact payload set;
its payload pin is excluded from frozen semantic owner fingerprints and recovery
records. The native owner callback extraction remains a separate acceptance step.

The runtime check includes 224 settings vectors generated by the actual owning Go
assignment policy, 33 owning Go activation metadata cases, 15 Go runtime-link topology/cut results checked with separate Rust
processes, and real operation processes against a disposable local config root and an
absent-instance Incus fixture. Exact owner-kill cuts cover bundle-before-journal,
authorized journal, native intent, late custody, terminal-before-release, staged and
exchanged links, and complete-before-Ready. Drift after durable authorization must
leave journal, ledger and target bytes unchanged. A fresh owner resumes the same durable
transaction without replaying an already desired SSH pin. The optional `fixture-cuts`
Cargo feature adds one inherited control socket for these tests; normal builds contain
no pause role or environment switch. These local cases do not prove remote guest,
service, historical-writer or all shipped-profile behavior.

The private entry also contains ordinary replacement, frozen v0.11.1 replacement,
activation-only/lifecycle recovery, and defective-owner replacement orchestration.
The latter prepares a proposal without a successor or grant, binds actual retained
asset inventories and protected history, and constructs its frozen receipt only after
consent. These branches still require their complete process/physical acceptance.
Inspection includes the frozen Go-shaped outcome and deterministic plan across
sessions; 64 whole-inspection, five activation-failure and 15 retained-runtime policy
vectors come from the owning Go code over the same fixed facts. They do not substitute
for whole-operation call/durable-outcome parity.

Normal operation ingress requires authenticated source, target and any previous
artifact tuples. Rust rehashes every listed file under the update lease; original raw
registry and capability carriers use padded base64, preserving original-byte pins.
The existing pre-v2 rollback convention has a closed source-owner/registry-absent-target
exception, with normal rollback eligibility still checked separately. The four initial
descriptor roles are helper image, grant pipe, late-custody ingress and repository root;
only fixture builds accept a fifth control descriptor. The actual owner barrier arrives
once after consent. An unchanged fixed-point read acquires neither mutation lease nor
cleanup authority.

Terminal owner-registration cleanup is a separate closed native operation over the
original verified step and recovery token. It runs after fresh terminal proof and
authorized checkpoint conversion, before recovery garbage collection. Its audited
component branch performs synchronous local archive deletion and fixed read-only
Incus queries; it submits no mutating child or remote operation. A bound failed
terminal can retain Ready with a cleanup warning, while missing terminal evidence
refuses Ready. Pending cleanup protects its native and recovery proofs from
retirement and replacement. Fresh-process cleanup admission reconstructs the original
verified step, token and bundle, and requires the original authenticated source and
previous artifacts only while cleanup remains pending. A required slot without an
invocation remains pending; only its checked completed or resolved witness removes
the continuation and retained release references. A clean fixed point remains a
read-only operation. The complete normal-factory cleanup retry process matrix is
still pending acceptance.

Fixture builds expose `after-journal-publication` on the existing control socket
after each successful current-journal CAS and directory fsync, including initial
authorization and recovery successors. This permits exact durable trace replay;
normal builds expose no additional control API.

This slice remains incomplete: full replacement/recovery and rollback acceptance, all activation recipe/resource-set proofs, combined custody
fault cases, complete operation parity, final facade/delivery and required physical
acceptance are pending. No default routing, whole-release readiness or performance
improvement is claimed by these checks.
