//! Opt-in local observations. These receipts are evidence, never owner state or paint.
use serde::Serialize;
use std::collections::{BTreeMap, BTreeSet};
use std::sync::{
    Arc, Mutex,
    atomic::{AtomicU64, Ordering},
};
use std::time::Instant;

static NEXT_CONTEXT: AtomicU64 = AtomicU64::new(1);

pub const MAX_PENDING: usize = 2048;
pub const MAX_RECORDS: usize = 8192;
/// Compact terminal samples are independent of optional, higher-volume phases.
/// Exceeding either bound remains explicit; neither is a workload capacity claim.
pub const MAX_TERMINALS: usize = 8192;
/// Independent of optional phases and terminals. Any drop leaves correlation incomplete.
pub const MAX_CORRELATIONS: usize = 8192;

/// Borrowed only during an explicit classifier call, after ordinary event validation.
/// The original receipt precedes any classifier parse/work. No frame bytes enter the ledger.
pub struct ValidatedEvent<'a> {
    pub event: &'a str,
    pub operation_id: &'a str,
    pub sequence: u64,
    pub revision: u64,
    pub received: Instant,
    pub frame: &'a [u8],
}
#[derive(Clone, Copy, Debug, Serialize, PartialEq, Eq)]
pub enum EventTag {
    Matched(u64),
    Other,
    Unclassifiable,
}
/// Classifiers own their immutable opaque mapping and bounded parsing policy.
/// Errors/panics become Unclassifiable; they cannot reject or rewrite a product event.
pub type EventTagger =
    Arc<dyn for<'a> Fn(ValidatedEvent<'a>) -> Result<EventTag, ()> + Send + Sync>;
#[derive(Clone, Copy, Debug, Serialize, PartialEq, Eq)]
pub enum CorrelationAdmission {
    Admitted,
    Duplicate,
    Overflow,
}
#[derive(Clone, Copy, Debug, Serialize, PartialEq, Eq)]
pub enum CorrelationClassification {
    Pending,
    Complete,
    Unknown,
}
#[derive(Clone, Debug, Serialize)]
pub struct Correlation {
    pub context: u64,
    pub source: u64,
    pub source_kind: SourceKind,
    pub sequence: u64,
    pub started_micros: Option<u64>,
    pub ticket: Option<u64>,
    pub tag: EventTag,
    pub admission: CorrelationAdmission,
    pub classification: CorrelationClassification,
}
#[derive(Clone, Debug, Default, Serialize)]
pub struct CorrelationCounts {
    pub valid_raw: u64,
    pub tagged: u64,
    pub other: u64,
    pub unclassifiable: u64,
    pub duplicate: u64,
    pub admitted: u64,
    pub admission_drops: u64,
    pub index_drops: u64,
    pub classification_pending: u64,
    pub classification_unknown: u64,
}
#[derive(Clone, Debug, Default, Serialize)]
pub struct CorrelationSnapshot {
    pub counts: CorrelationCounts,
    pub index: Vec<Correlation>,
}
/// A one-use local reservation. It carries no payload, callback or completion authority.
#[derive(Default)]
pub(crate) struct EventReservation {
    ticket: Option<Ticket>,
    classification: Option<ClassificationReservation>,
}
struct ClassificationReservation {
    context: u64,
    source: u64,
    index: Option<usize>,
}

#[derive(Clone, Copy, Debug, Serialize, PartialEq, Eq)]
pub enum Origin {
    RpcSession,
    Gui,
}
#[derive(Clone, Copy, Debug, Serialize, PartialEq, Eq)]
pub struct ViewCoverage {
    pub view: u64,
    pub scope: u64,
    pub generation: u64,
    pub invalidation: u64,
    pub parts: u16,
    pub whole: bool,
    pub cached: bool,
    pub remote: bool,
}

#[derive(Clone, Copy, Debug, Serialize, PartialEq, Eq)]
pub enum TerminalOutcome {
    Paint {
        revision: u64,
        clock: u64,
        frame: i64,
    },
    Retired(Retirement),
}
#[derive(Clone, Debug, Serialize)]
pub struct Terminal {
    pub context: u64,
    pub source: u64,
    pub source_kind: Option<SourceKind>,
    pub origin: Origin,
    pub gui_view: Option<u64>,
    pub gui_remote: Option<bool>,
    pub coverage: Option<ViewCoverage>,
    pub ticket: u64,
    pub kind: StartKind,
    pub effect: Option<Effect>,
    pub elapsed_micros: Option<u64>,
    pub started_micros: Option<u64>,
    pub outcome: TerminalOutcome,
}

#[derive(Clone, Copy, Debug, Serialize, PartialEq, Eq)]
pub enum SourceKind {
    Local,
    Remote,
}
#[derive(Clone, Copy, Debug, Serialize, PartialEq, Eq)]
pub enum StartKind {
    RpcEvent,
    LocalRequest,
    LoadedSelection,
}
#[derive(Clone, Copy, Debug, Serialize, PartialEq, Eq)]
pub enum Effect {
    Inventory,
    Operation,
    View,
}
#[derive(Clone, Copy, Debug, Serialize, PartialEq, Eq)]
pub enum ReadKind {
    Rpc,
    InitialCache,
    Sealed,
}
#[derive(Clone, Copy, Debug, Serialize, PartialEq, Eq)]
pub enum Retirement {
    Ignored,
    Unknown,
    SessionClosed,
    Superseded,
}
#[derive(Clone, Copy, Debug, Serialize)]
pub enum Phase {
    Source(SourceKind),
    Received(StartKind),
    Eligible(Effect),
    Read(ReadKind),
    Retired(Retirement),
    Overflow,
    Duplicate,
}

#[derive(Clone, Debug)]
pub struct Ticket {
    context: u64,
    ordinal: u64,
    source: u64,
    source_kind: Option<SourceKind>,
    origin: Origin,
    gui_view: Option<u64>,
    gui_remote: Option<bool>,
    kind: StartKind,
    effect: Option<Effect>,
    started: Instant,
}
impl Ticket {
    pub fn ordinal(&self) -> u64 {
        self.ordinal
    }
    pub fn source(&self) -> u64 {
        self.source
    }
    pub fn kind(&self) -> StartKind {
        self.kind
    }
    pub fn started(&self) -> Instant {
        self.started
    }
    pub fn effect(&self) -> Option<Effect> {
        self.effect
    }
}
#[derive(Clone, Debug, Default)]
pub struct Batch {
    tickets: Vec<Ticket>,
}
impl Batch {
    pub fn tickets(&self) -> &[Ticket] {
        &self.tickets
    }
}
#[derive(Clone, Debug)]
/// A conservative request frontier, not evidence of UI acceptance or paint.
/// Initial cache reads never cover pending event receipts or claim an RPC round trip.
pub struct ReadProof {
    context: u64,
    source: u64,
    request: Option<u64>,
    started: Instant,
    kind: ReadKind,
    frontier: u64,
    batch: Batch,
}
impl ReadProof {
    pub fn source(&self) -> u64 {
        self.source
    }
    pub fn request(&self) -> Option<u64> {
        self.request
    }
    pub fn started(&self) -> Instant {
        self.started
    }
    pub fn kind(&self) -> ReadKind {
        self.kind
    }
    pub fn frontier(&self) -> u64 {
        self.frontier
    }
    pub fn batch(&self) -> &Batch {
        &self.batch
    }
}
#[derive(Clone, Debug)]
pub struct UpdateObservation {
    pub batch: Batch,
    pub read: Option<ReadProof>,
}
pub struct Observed<T> {
    pub value: T,
    pub proof: Option<ReadProof>,
}
#[derive(Clone, Debug, Serialize)]
pub struct ScalarRecord {
    pub source: u64,
    pub ticket: u64,
    pub request: u64,
    pub elapsed_micros: u64,
    pub phase: Phase,
}
#[derive(Clone, Debug, Default, Serialize)]
pub struct Counts {
    pub session_sources: u64,
    pub received: u64,
    pub eligible: u64,
    pub inventory_eligible: u64,
    pub operation_eligible: u64,
    pub view_starts: u64,
    pub gui_start_attempts: u64,
    pub ignored: u64,
    pub retired: u64,
    pub unknown: u64,
    pub rpc_admission_attempts: u64,
    pub cache_reads: u64,
    pub overflow: u64,
    pub duplicate: u64,
    pub unknown_retirement: u64,
    pub rejected_batch_tickets: u64,
    pub dropped_records: u64,
    pub completed: u64,
    pub duplicate_completion: u64,
    pub dropped_terminals: u64,
    pub pending: usize,
}
#[derive(Clone, Debug, Default, Serialize)]
pub struct Lifecycle {
    pub activations: u64,
    pub workers_started: u64,
    pub workers_closed: u64,
    pub unmatched_closed: u64,
    pub untracked_workers: u64,
}
#[derive(Clone, Copy, Debug, Serialize, PartialEq, Eq)]
pub enum CleanupObservation {
    ObservedClosed,
    Unknown,
}
#[derive(Clone, Copy, Debug)]
pub struct WorkerToken {
    context: u64,
    ordinal: u64,
}
#[derive(Clone, Copy, Debug, Serialize)]
enum TraceWindow {
    Sealed,
}
/// Immutable local trace window, not a drained-producer or compositor receipt.
/// Fields are private so callers cannot relabel lifecycle or coverage facts.
#[derive(Clone, Debug, Serialize)]
pub struct Snapshot {
    schema_version: u8,
    context: u64,
    trace_window: TraceWindow,
    lifecycle: Lifecycle,
    worker_cleanup: CleanupObservation,
    producer_quiescence: CleanupObservation,
    counts: Counts,
    records: Vec<ScalarRecord>,
    terminals: Vec<Terminal>,
    #[serde(skip_serializing_if = "Option::is_none")]
    correlation: Option<CorrelationSnapshot>,
    #[serde(skip_serializing_if = "Option::is_none")]
    child_usage: Option<crate::child_usage::Snapshot>,
}
impl Snapshot {
    pub fn child_usage(&self) -> Option<&crate::child_usage::Snapshot> {
        self.child_usage.as_ref()
    }
    pub fn worker_cleanup(&self) -> CleanupObservation {
        self.worker_cleanup
    }
}
struct Entry {
    ticket: Ticket,
    eligible: bool,
    coverage: Option<ViewCoverage>,
}
struct Ledger {
    sealed: Option<Snapshot>,
    lifecycle: Lifecycle,
    workers: BTreeSet<u64>,
    context: u64,
    origin: Instant,
    next: u64,
    source: u64,
    request: u64,
    clock_binding: u64,
    view: u64,
    pending: BTreeMap<u64, Entry>,
    records: Vec<ScalarRecord>,
    terminals: Vec<Terminal>,
    counts: Counts,
    event_tagger: Option<EventTagger>,
    correlation: Option<CorrelationSnapshot>,
    child_usage: Option<crate::child_usage::Collector>,
}
impl Ledger {
    fn retire(&mut self, source: u64, ticket: u64, reason: Retirement) {
        if self.sealed.is_some() {
            return;
        }
        if let Some(entry) = self.pending.remove(&ticket) {
            self.terminal(
                &entry.ticket,
                entry.coverage,
                TerminalOutcome::Retired(reason),
                None,
            );
        }
        self.counts.retired += 1;
        if reason == Retirement::Ignored {
            self.counts.ignored += 1;
        }
        if matches!(reason, Retirement::Unknown | Retirement::SessionClosed) {
            self.counts.unknown += 1;
        }
        self.record(source, ticket, 0, Phase::Retired(reason));
    }
    fn terminal(
        &mut self,
        ticket: &Ticket,
        coverage: Option<ViewCoverage>,
        outcome: TerminalOutcome,
        elapsed_micros: Option<u64>,
    ) {
        if self.sealed.is_some() {
            return;
        }
        if self.terminals.len() == MAX_TERMINALS {
            self.counts.dropped_terminals += 1;
            return;
        }
        self.terminals.push(Terminal {
            context: ticket.context,
            source: ticket.source,
            source_kind: ticket.source_kind,
            origin: ticket.origin,
            gui_view: ticket.gui_view,
            gui_remote: ticket.gui_remote,
            coverage,
            ticket: ticket.ordinal,
            kind: ticket.kind,
            effect: ticket.effect,
            elapsed_micros,
            started_micros: ticket
                .started
                .checked_duration_since(self.origin)
                .map(|elapsed| elapsed.as_micros().min(u64::MAX as u128) as u64),
            outcome,
        });
    }
    fn record(&mut self, source: u64, ticket: u64, request: u64, phase: Phase) {
        self.record_at(source, ticket, request, phase, Instant::now());
    }
    fn record_at(&mut self, source: u64, ticket: u64, request: u64, phase: Phase, at: Instant) {
        if self.sealed.is_some() {
            return;
        }
        if self.records.len() == MAX_RECORDS {
            self.counts.dropped_records += 1;
            return;
        }
        self.records.push(ScalarRecord {
            source,
            ticket,
            request,
            phase,
            elapsed_micros: at
                .saturating_duration_since(self.origin)
                .as_micros()
                .min(u64::MAX as u128) as u64,
        });
    }
}
/// Construct explicitly for a measurement run. Ordinary clients hold None and allocate no ledger.
#[derive(Clone)]
pub struct Observation(Arc<Mutex<Ledger>>);
impl Observation {
    pub fn new() -> Self {
        Self::with_tagger(None)
    }
    /// Explicit local-only opt-in. Ordinary observations retain no event payload/index.
    pub fn with_event_tagger(tagger: EventTagger) -> Self {
        Self::with_tagger(Some(tagger))
    }
    fn with_tagger(tagger: Option<EventTagger>) -> Self {
        Self::with_observers(tagger, None)
    }
    /// Configure before creating any Source; child metadata is independent of tickets.
    pub fn with_child_usage(capacity: usize) -> Self {
        Self::with_observers(None, Some(capacity))
    }
    pub fn with_observers(tagger: Option<EventTagger>, child_capacity: Option<usize>) -> Self {
        let correlation = tagger.as_ref().map(|_| CorrelationSnapshot::default());
        let origin = Instant::now();
        let child_usage =
            child_capacity.map(|capacity| crate::child_usage::Collector::new(capacity, origin));
        Self(Arc::new(Mutex::new(Ledger {
            sealed: None,
            lifecycle: Lifecycle::default(),
            workers: BTreeSet::new(),
            context: NEXT_CONTEXT.fetch_add(1, Ordering::Relaxed),
            origin,
            next: 0,
            source: 0,
            request: 0,
            clock_binding: 0,
            view: 0,
            pending: BTreeMap::new(),
            records: Vec::new(),
            terminals: Vec::new(),
            counts: Counts::default(),
            event_tagger: tagger,
            correlation,
            child_usage,
        })))
    }
    pub fn source(&self, kind: SourceKind) -> Source {
        let mut ledger = self.0.lock().unwrap();
        let ordinal = if ledger.sealed.is_none() {
            ledger.source += 1;
            let ordinal = ledger.source;
            ledger.record(ordinal, 0, 0, Phase::Source(kind));
            ordinal
        } else {
            0
        }; // inert handle; never an admitted RPC source
        Source(Arc::new(SourceInner {
            observation: self.clone(),
            ordinal,
            kind,
            sequence: Mutex::new(0),
        }))
    }
    pub(crate) fn child_usage_context(&self) -> Option<crate::child_usage::UsageContext> {
        let ledger = self.0.lock().unwrap();
        if ledger.sealed.is_some() {
            return None;
        }
        ledger
            .child_usage
            .as_ref()?
            .provenance(ledger.context, None)
    }
    pub fn counts(&self) -> Counts {
        let ledger = self.0.lock().unwrap();
        Counts {
            pending: ledger.pending.len(),
            session_sources: ledger.source,
            ..ledger.counts.clone()
        }
    }
    /// Verify only context-local allocated RPC source provenance, never owner
    /// identity, liveness, cache event coverage or UI authority.
    pub fn read_source(&self, proof: &ReadProof) -> Option<(u64, u64)> {
        let ledger = self.0.lock().unwrap();
        (ledger.sealed.is_none()
            && proof.kind != ReadKind::Sealed
            && proof.context == ledger.context
            && proof.source > 0
            && proof.source <= ledger.source)
            .then_some((ledger.context, proof.source))
    }
    pub fn records(&self) -> Vec<ScalarRecord> {
        self.0.lock().unwrap().records.clone()
    }
    /// Read-only conversion in the ledger's original monotonic clock domain.
    /// This grants no source, event, coverage, or completion authority.
    pub fn relative_micros(&self, at: Instant) -> Option<u64> {
        at.checked_duration_since(self.0.lock().unwrap().origin)
            .map(|elapsed| elapsed.as_micros().min(u64::MAX as u128) as u64)
    }
    pub fn terminals(&self) -> Vec<Terminal> {
        self.0.lock().unwrap().terminals.clone()
    }
    /// Scalar index/counters do not depend on optional phase or terminal survival.
    pub fn correlation(&self) -> Option<CorrelationSnapshot> {
        self.0.lock().unwrap().correlation.clone()
    }
    /// Opaque context-local binding ordinal; not a compositor presentation ID.
    pub fn clock_binding(&self) -> u64 {
        let mut ledger = self.0.lock().unwrap();
        if ledger.sealed.is_some() {
            return 0;
        }
        ledger.clock_binding += 1;
        ledger.clock_binding
    }
    pub fn view(&self) -> u64 {
        let mut ledger = self.0.lock().unwrap();
        if ledger.sealed.is_some() {
            return 0;
        }
        ledger.view += 1;
        ledger.view
    }
    /// Source zero is explicitly GUI origin, never an RPC-session ordinal.
    pub fn view_start(&self, view: u64, selection: bool, remote: bool) -> Batch {
        let mut ledger = self.0.lock().unwrap();
        if ledger.sealed.is_some() {
            return Batch::default();
        }
        ledger.counts.gui_start_attempts += 1;
        if ledger.pending.len() == MAX_PENDING {
            ledger.counts.overflow += 1;
            ledger.record(0, 0, 0, Phase::Overflow);
            return Batch::default();
        }
        ledger.next += 1;
        let ticket = Ticket {
            context: ledger.context,
            ordinal: ledger.next,
            source: 0,
            source_kind: None,
            origin: Origin::Gui,
            gui_view: Some(view),
            gui_remote: Some(remote),
            kind: if selection {
                StartKind::LoadedSelection
            } else {
                StartKind::LocalRequest
            },
            effect: Some(Effect::View),
            started: Instant::now(),
        };
        ledger.counts.view_starts += 1;
        ledger.record(0, ticket.ordinal, 0, Phase::Received(ticket.kind));
        ledger.pending.insert(
            ticket.ordinal,
            Entry {
                ticket: ticket.clone(),
                eligible: true,
                coverage: None,
            },
        );
        Batch {
            tickets: vec![ticket],
        }
    }
    pub fn represent(&self, batch: &Batch, coverage: ViewCoverage) {
        let live = self.live(batch);
        let mut ledger = self.0.lock().unwrap();
        if ledger.sealed.is_some() {
            return;
        }
        for ticket in live.tickets() {
            if let Some(entry) = ledger.pending.get_mut(&ticket.ordinal) {
                entry.coverage = Some(coverage);
            }
        }
    }
    pub fn ticket_batch(&self, ticket: &Ticket) -> Batch {
        self.live(&Batch {
            tickets: vec![ticket.clone()],
        })
    }
    /// The trusted adapter calls this only after matching actual snapshot and
    /// after-paint. This core records supplied evidence, never infers UI authority.
    pub fn complete(&self, batch: &Batch, revision: u64, clock: u64, frame: i64, at: Instant) {
        let mut ledger = self.0.lock().unwrap();
        if ledger.sealed.is_some() {
            return;
        }
        for ticket in batch.tickets() {
            if ticket.context != ledger.context {
                ledger.counts.rejected_batch_tickets += 1;
                continue;
            }
            let Some(entry) = ledger.pending.get(&ticket.ordinal) else {
                ledger.counts.duplicate_completion += 1;
                continue;
            };
            if entry.ticket.source != ticket.source || !entry.eligible {
                ledger.counts.rejected_batch_tickets += 1;
                continue;
            }
            let Some(elapsed) = at.checked_duration_since(entry.ticket.started) else {
                ledger.retire(ticket.source, ticket.ordinal, Retirement::Unknown);
                continue;
            };
            let entry = ledger.pending.remove(&ticket.ordinal).unwrap();
            ledger.counts.completed += 1;
            ledger.terminal(
                &entry.ticket,
                entry.coverage,
                TerminalOutcome::Paint {
                    revision,
                    clock,
                    frame,
                },
                Some(elapsed.as_micros().min(u64::MAX as u128) as u64),
            );
        }
    }
    /// Deduplicate only live, eligible originals from this context. Retired tickets
    /// already have an explicit terminal reason; foreign contexts are rejected.
    pub fn merge(&self, left: &Batch, right: &Batch) -> Batch {
        let mut ledger = self.0.lock().unwrap();
        if ledger.sealed.is_some() {
            return Batch::default();
        }
        let mut tickets = BTreeMap::new();
        for ticket in left.tickets().iter().chain(right.tickets()) {
            if ticket.context != ledger.context {
                ledger.counts.rejected_batch_tickets += 1;
                continue;
            }
            if let Some(entry) = ledger.pending.get(&ticket.ordinal) {
                if entry.ticket.source != ticket.source {
                    ledger.counts.rejected_batch_tickets += 1;
                } else if entry.eligible {
                    tickets.insert(ticket.ordinal, entry.ticket.clone());
                }
            }
        }
        debug_assert!(tickets.len() <= MAX_PENDING);
        Batch {
            tickets: tickets.into_values().collect(),
        }
    }
    pub fn live(&self, batch: &Batch) -> Batch {
        self.merge(batch, &Batch::default())
    }
    pub fn inventory(&self, batch: &Batch) -> Batch {
        let mut batch = self.live(batch);
        batch
            .tickets
            .retain(|ticket| ticket.effect == Some(Effect::Inventory));
        batch
    }
    /// Retire live originals omitted by an explicitly complete replacement batch.
    pub fn retire_missing(&self, previous: &Batch, represented: &Batch, reason: Retirement) {
        let mut missing = self.live(previous);
        let represented = self.live(represented);
        missing.tickets.retain(|old| {
            !represented
                .tickets
                .iter()
                .any(|new| new.ordinal == old.ordinal)
        });
        self.retire(&missing, reason);
    }
    /// Explicit unknown/retirement only. No core operation claims widget paint completion.
    pub fn retire(&self, batch: &Batch, reason: Retirement) {
        let mut ledger = self.0.lock().unwrap();
        if ledger.sealed.is_some() {
            return;
        }
        for ticket in batch.tickets() {
            if ledger.pending.get(&ticket.ordinal).is_some_and(|entry| {
                entry.ticket.source == ticket.source && ticket.context == ledger.context
            }) {
                ledger.retire(ticket.source, ticket.ordinal, reason);
            } else {
                ledger.counts.unknown_retirement += 1;
            }
        }
    }
    pub fn activation(&self) {
        let mut ledger = self.0.lock().unwrap();
        if ledger.sealed.is_none() {
            ledger.lifecycle.activations += 1;
        }
    }
    /// Token belongs to this actual worker/receiver, not a logical owner/source.
    pub fn worker_started(&self) -> Option<WorkerToken> {
        let mut ledger = self.0.lock().unwrap();
        if ledger.sealed.is_some() {
            return None;
        }
        ledger.lifecycle.workers_started += 1;
        let id = ledger.lifecycle.workers_started;
        if ledger.workers.len() == MAX_PENDING {
            ledger.lifecycle.untracked_workers += 1;
            None
        } else {
            ledger.workers.insert(id);
            Some(WorkerToken {
                context: ledger.context,
                ordinal: id,
            })
        }
    }
    pub fn worker_closed(&self, token: WorkerToken) {
        let mut ledger = self.0.lock().unwrap();
        if ledger.sealed.is_some() {
            return;
        }
        if token.context == ledger.context && ledger.workers.remove(&token.ordinal) {
            ledger.lifecycle.workers_closed += 1;
        } else {
            ledger.lifecycle.unmatched_closed += 1;
        }
    }
    /// One lock excludes every admission/classification/completion/drop mutation.
    /// It does not wait for, or prove quiescence of, detached native producers.
    pub fn seal(&self) -> Snapshot {
        let mut ledger = self.0.lock().unwrap();
        if let Some(snapshot) = &ledger.sealed {
            return snapshot.clone();
        }
        if let Some(correlation) = &mut ledger.correlation {
            correlation.counts.classification_unknown += correlation.counts.classification_pending;
            correlation.counts.classification_pending = 0;
            for row in &mut correlation.index {
                if row.classification == CorrelationClassification::Pending {
                    row.classification = CorrelationClassification::Unknown;
                }
            }
        }
        let pending: Vec<_> = ledger
            .pending
            .values()
            .map(|entry| (entry.ticket.source, entry.ticket.ordinal))
            .collect();
        for (source, ticket) in pending {
            ledger.retire(source, ticket, Retirement::Unknown);
        }
        let snapshot = Snapshot {
            schema_version: 1,
            context: ledger.context,
            trace_window: TraceWindow::Sealed,
            lifecycle: ledger.lifecycle.clone(),
            worker_cleanup: if ledger.lifecycle.activations > 0
                && ledger.lifecycle.workers_started > 0
                && ledger.lifecycle.workers_started == ledger.lifecycle.workers_closed
                && ledger.lifecycle.untracked_workers == 0
                && ledger.lifecycle.unmatched_closed == 0
            {
                CleanupObservation::ObservedClosed
            } else {
                CleanupObservation::Unknown
            },
            producer_quiescence: CleanupObservation::Unknown,
            counts: Counts {
                pending: 0,
                session_sources: ledger.source,
                ..ledger.counts.clone()
            },
            records: ledger.records.clone(),
            terminals: ledger.terminals.clone(),
            correlation: ledger.correlation.clone(),
            child_usage: ledger
                .child_usage
                .as_ref()
                .map(crate::child_usage::Collector::seal),
        };
        ledger.sealed = Some(snapshot.clone());
        snapshot
    }
}
impl Default for Observation {
    fn default() -> Self {
        Self::new()
    }
}
struct SourceInner {
    observation: Observation,
    ordinal: u64,
    kind: SourceKind,
    sequence: Mutex<u64>,
}
/// One fresh RPC-session provenance ordinal, not a persistent owner identity.
/// Local/Remote cohorts are scalar; later Worker binding owns logical owner/scope cohorts.
#[derive(Clone)]
pub struct Source(Arc<SourceInner>);
impl Source {
    pub(crate) fn child_usage_context(&self) -> Option<crate::child_usage::UsageContext> {
        let ledger = self.0.observation.0.lock().unwrap();
        if ledger.sealed.is_some() || self.0.ordinal == 0 {
            return None;
        }
        ledger
            .child_usage
            .as_ref()?
            .provenance(ledger.context, Some((self.0.ordinal, self.0.kind)))
    }
    pub fn ordinal(&self) -> u64 {
        self.0.ordinal
    }
    pub fn kind(&self) -> SourceKind {
        self.0.kind
    }
    pub fn request(&self) -> Option<Ticket> {
        self.insert(StartKind::LocalRequest, true, Instant::now())
    }
    pub fn selection(&self) -> Option<Ticket> {
        self.insert(StartKind::LoadedSelection, true, Instant::now())
    }
    fn insert(&self, kind: StartKind, eligible: bool, started: Instant) -> Option<Ticket> {
        let mut ledger = self.0.observation.0.lock().unwrap();
        self.insert_locked(&mut ledger, kind, eligible, started)
    }
    fn insert_locked(
        &self,
        ledger: &mut Ledger,
        kind: StartKind,
        eligible: bool,
        started: Instant,
    ) -> Option<Ticket> {
        if ledger.sealed.is_some() {
            return None;
        }
        ledger.next += 1;
        let ordinal = ledger.next;
        if ledger.pending.len() == MAX_PENDING {
            ledger.counts.overflow += 1;
            ledger.record(self.ordinal(), ordinal, 0, Phase::Overflow);
            return None;
        }
        let ticket = Ticket {
            context: ledger.context,
            ordinal,
            source: self.ordinal(),
            source_kind: Some(self.kind()),
            origin: Origin::RpcSession,
            gui_view: None,
            gui_remote: None,
            kind,
            effect: eligible.then_some(Effect::View),
            started,
        };
        ledger.pending.insert(
            ordinal,
            Entry {
                ticket: ticket.clone(),
                eligible,
                coverage: None,
            },
        );
        if eligible {
            ledger.counts.eligible += 1;
            ledger.counts.view_starts += 1;
        }
        ledger.record_at(self.ordinal(), ordinal, 0, Phase::Received(kind), started);
        Some(ticket)
    }
    pub(crate) fn event_tagger(&self) -> Option<EventTagger> {
        let ledger = self.0.observation.0.lock().unwrap();
        if ledger.sealed.is_some() {
            None
        } else {
            ledger.event_tagger.clone()
        }
    }
    pub(crate) fn classify_event(&self, event: ValidatedEvent<'_>) -> Option<EventTag> {
        let tagger = self.event_tagger()?;
        Some(
            std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| tagger(event)))
                .ok()
                .and_then(Result::ok)
                .unwrap_or(EventTag::Unclassifiable),
        )
    }
    #[cfg(test)]
    pub(crate) fn received(&self, sequence: u64, started: Instant) -> Option<Ticket> {
        self.reserve_event(sequence, started).ticket
    }
    #[cfg(test)]
    pub(crate) fn received_classified(
        &self,
        sequence: u64,
        started: Instant,
        tag: Option<EventTag>,
    ) -> Option<Ticket> {
        let reserved = self.reserve_event(sequence, started);
        self.finish_event(reserved, tag)
    }
    /// Reserve the validated raw receipt, original ticket and explicit unfinished
    /// classification atomically, BEFORE any optional callback can run or seal.
    pub(crate) fn reserve_event(&self, sequence: u64, started: Instant) -> EventReservation {
        let mut previous = self.0.sequence.lock().unwrap();
        let mut ledger = self.0.observation.0.lock().unwrap();
        if ledger.sealed.is_some() {
            return EventReservation::default();
        }
        ledger.counts.received += 1;
        let duplicate = sequence <= *previous;
        let ticket = if duplicate {
            ledger.counts.duplicate += 1;
            ledger.record(self.ordinal(), 0, 0, Phase::Duplicate);
            None
        } else {
            *previous = sequence;
            self.insert_locked(&mut ledger, StartKind::RpcEvent, false, started)
        };
        let context = ledger.context;
        let started_micros = started
            .checked_duration_since(ledger.origin)
            .map(|elapsed| elapsed.as_micros().min(u64::MAX as u128) as u64);
        let classification = if let Some(correlation) = &mut ledger.correlation {
            correlation.counts.valid_raw += 1;
            correlation.counts.unclassifiable += 1;
            correlation.counts.classification_pending += 1;
            let admission = if duplicate {
                correlation.counts.duplicate += 1;
                CorrelationAdmission::Duplicate
            } else if ticket.is_some() {
                correlation.counts.admitted += 1;
                CorrelationAdmission::Admitted
            } else {
                correlation.counts.admission_drops += 1;
                CorrelationAdmission::Overflow
            };
            let index = if correlation.index.len() == MAX_CORRELATIONS {
                correlation.counts.index_drops += 1;
                None
            } else {
                let index = correlation.index.len();
                correlation.index.push(Correlation {
                    context,
                    source: self.ordinal(),
                    source_kind: self.kind(),
                    sequence,
                    started_micros,
                    ticket: ticket.as_ref().map(Ticket::ordinal),
                    tag: EventTag::Unclassifiable,
                    admission,
                    classification: CorrelationClassification::Pending,
                });
                Some(index)
            };
            Some(ClassificationReservation {
                context,
                source: self.ordinal(),
                index,
            })
        } else {
            None
        };
        EventReservation {
            ticket,
            classification,
        }
    }
    /// Resolving a callback cannot add a raw receipt or rewrite a sealed window.
    /// The move-only reservation prevents double classification, including index drops.
    pub(crate) fn finish_event(
        &self,
        reserved: EventReservation,
        tag: Option<EventTag>,
    ) -> Option<Ticket> {
        if let (Some(reservation), Some(tag)) = (reserved.classification, tag) {
            let mut ledger = self.0.observation.0.lock().unwrap();
            if ledger.sealed.is_none()
                && reservation.context == ledger.context
                && reservation.source == self.ordinal()
            {
                if let Some(correlation) = &mut ledger.correlation {
                    correlation.counts.classification_pending -= 1;
                    correlation.counts.unclassifiable -= 1;
                    match tag {
                        EventTag::Matched(_) => correlation.counts.tagged += 1,
                        EventTag::Other => correlation.counts.other += 1,
                        EventTag::Unclassifiable => correlation.counts.unclassifiable += 1,
                    }
                    if let Some(index) = reservation.index {
                        let row = &mut correlation.index[index];
                        row.tag = tag;
                        row.classification = CorrelationClassification::Complete;
                    }
                }
            }
        }
        reserved.ticket
    }
    pub(crate) fn eligible(&self, ticket: &Ticket, effect: Effect) {
        let mut ledger = self.0.observation.0.lock().unwrap();
        if ledger.sealed.is_some() {
            return;
        }
        if ticket.context != ledger.context {
            return;
        }
        if let Some(entry) = ledger.pending.get_mut(&ticket.ordinal) {
            if entry.ticket.source == self.ordinal() && !entry.eligible {
                entry.eligible = true;
                entry.ticket.effect = Some(effect);
                ledger.counts.eligible += 1;
                match effect {
                    Effect::Inventory => ledger.counts.inventory_eligible += 1,
                    Effect::Operation => ledger.counts.operation_eligible += 1,
                    Effect::View => ledger.counts.view_starts += 1,
                }
                ledger.record(self.ordinal(), ticket.ordinal, 0, Phase::Eligible(effect));
            }
        }
    }
    pub(crate) fn ignore(&self, ticket: Ticket) {
        self.0.observation.retire(
            &Batch {
                tickets: vec![ticket],
            },
            Retirement::Ignored,
        );
    }
    pub(crate) fn retire_all(&self, reason: Retirement) {
        let mut ledger = self.0.observation.0.lock().unwrap();
        if ledger.sealed.is_some() {
            return;
        }
        let ids: Vec<_> = ledger
            .pending
            .values()
            .filter(|entry| entry.ticket.source == self.ordinal())
            .map(|entry| entry.ticket.ordinal)
            .collect();
        for id in ids {
            ledger.retire(self.ordinal(), id, reason);
        }
    }
    pub fn batch(&self) -> Batch {
        let ledger = self.0.observation.0.lock().unwrap();
        Batch {
            tickets: ledger
                .pending
                .values()
                .filter(|entry| entry.eligible && entry.ticket.source == self.ordinal())
                .map(|entry| entry.ticket.clone())
                .collect(),
        }
    }
    pub(crate) fn update(&self, read: Option<ReadProof>) -> UpdateObservation {
        UpdateObservation {
            batch: self.batch(),
            read,
        }
    }
    pub(crate) fn admit(&self) -> ReadProof {
        let mut ledger = self.0.observation.0.lock().unwrap();
        if ledger.sealed.is_some() {
            return self.sealed_proof(ledger.context);
        }
        ledger.request += 1;
        let request = ledger.request;
        let frontier = ledger.next;
        let started = Instant::now();
        let batch = Batch {
            tickets: ledger
                .pending
                .values()
                .filter(|entry| {
                    entry.eligible
                        && entry.ticket.effect == Some(Effect::Inventory)
                        && entry.ticket.source == self.ordinal()
                        && entry.ticket.ordinal <= frontier
                })
                .map(|entry| entry.ticket.clone())
                .collect(),
        };
        ledger.counts.rpc_admission_attempts += 1;
        ledger.record_at(
            self.ordinal(),
            0,
            request,
            Phase::Read(ReadKind::Rpc),
            started,
        );
        ReadProof {
            context: ledger.context,
            source: self.ordinal(),
            request: Some(request),
            started,
            kind: ReadKind::Rpc,
            frontier,
            batch,
        }
    }
    pub(crate) fn cached(&self) -> ReadProof {
        let mut ledger = self.0.observation.0.lock().unwrap();
        if ledger.sealed.is_some() {
            return self.sealed_proof(ledger.context);
        }
        ledger.counts.cache_reads += 1;
        ledger.record(self.ordinal(), 0, 0, Phase::Read(ReadKind::InitialCache));
        ReadProof {
            context: ledger.context,
            source: self.ordinal(),
            request: None,
            started: Instant::now(),
            kind: ReadKind::InitialCache,
            frontier: 0,
            batch: Batch::default(),
        }
    }
    fn sealed_proof(&self, context: u64) -> ReadProof {
        ReadProof {
            context,
            source: self.ordinal(),
            request: None,
            started: Instant::now(),
            kind: ReadKind::Sealed,
            frontier: 0,
            batch: Batch::default(),
        }
    }
}
impl Drop for SourceInner {
    fn drop(&mut self) {
        let mut ledger = self.observation.0.lock().unwrap();
        if ledger.sealed.is_some() {
            return;
        }
        let ids: Vec<_> = ledger
            .pending
            .values()
            .filter(|entry| entry.ticket.source == self.ordinal)
            .map(|entry| entry.ticket.ordinal)
            .collect();
        for id in ids {
            ledger.retire(self.ordinal, id, Retirement::SessionClosed);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    fn ledger_fingerprint(observer: &Observation) -> serde_json::Value {
        let ledger = observer.0.lock().unwrap();
        serde_json::json!({"next":ledger.next,"source":ledger.source,"request":ledger.request,
            "clock_binding":ledger.clock_binding,"view":ledger.view,"workers":ledger.workers,
            "lifecycle":ledger.lifecycle,"counts":ledger.counts,"pending":ledger.pending.len(),
            "records":ledger.records,"terminals":ledger.terminals,"sealed":ledger.sealed.is_some()})
    }
    #[test]
    fn seal_retires_once_and_freezes_every_late_mutation_and_drop() {
        let observer = Observation::new();
        observer.activation();
        let worker = observer.worker_started().unwrap();
        observer.worker_closed(worker);
        let source = observer.source(SourceKind::Local);
        let ticket = source.received(1, Instant::now()).unwrap();
        source.eligible(&ticket, Effect::Inventory);
        let batch = source.batch();
        let view = observer.view();
        let gui = observer.view_start(view, false, true);
        let old_clock = observer.clock_binding();
        let sealed = observer.seal();
        assert_eq!(sealed.worker_cleanup(), CleanupObservation::ObservedClosed);
        assert_eq!(sealed.counts.pending, 0);
        assert_eq!(sealed.counts.unknown, 2);
        assert_eq!(sealed.terminals.len(), 2);
        let expected = serde_json::to_value(&sealed).unwrap();
        let raw_ledger = ledger_fingerprint(&observer);
        let old_counts = serde_json::to_value(observer.counts()).unwrap();
        observer.activation();
        observer.worker_closed(worker);
        assert!(observer.worker_started().is_none());
        assert_eq!(observer.clock_binding(), 0);
        assert_eq!(observer.view(), 0);
        assert!(observer.view_start(view, true, false).tickets().is_empty());
        observer.represent(
            &gui,
            ViewCoverage {
                view,
                scope: 1,
                generation: 1,
                invalidation: 1,
                parts: 1,
                whole: true,
                cached: false,
                remote: false,
            },
        );
        observer.complete(&batch, 1, old_clock, 1, Instant::now());
        assert!(observer.merge(&batch, &gui).tickets().is_empty());
        observer.retire_missing(&batch, &gui, Retirement::Ignored);
        observer.retire(&batch, Retirement::Ignored);
        assert!(source.request().is_none() && source.selection().is_none());
        assert!(source.received(0, Instant::now()).is_none());
        source.eligible(&ticket, Effect::Operation);
        source.ignore(ticket);
        source.retire_all(Retirement::SessionClosed);
        assert_eq!(source.admit().kind(), ReadKind::Sealed);
        assert_eq!(source.cached().kind(), ReadKind::Sealed);
        let late = observer.source(SourceKind::Remote);
        assert_eq!(late.ordinal(), 0);
        assert!(late.request().is_none() && late.batch().tickets().is_empty());
        assert_eq!(*source.0.sequence.lock().unwrap(), 1);
        drop(late);
        drop(source);
        assert_eq!(serde_json::to_value(observer.counts()).unwrap(), old_counts);
        assert_eq!(serde_json::to_value(observer.seal()).unwrap(), expected);
        assert_eq!(ledger_fingerprint(&observer), raw_ledger);
        assert_eq!(
            serde_json::to_value(observer.records()).unwrap(),
            serde_json::to_value(&sealed.records).unwrap()
        );
        assert_eq!(
            serde_json::to_value(observer.terminals()).unwrap(),
            serde_json::to_value(&sealed.terminals).unwrap()
        );
        assert!(matches!(
            sealed.producer_quiescence,
            CleanupObservation::Unknown
        ));
    }
    #[test]
    fn seal_is_atomic_against_held_concurrent_producers_and_lifecycle_is_honest() {
        let observer = Observation::new();
        observer.activation();
        let worker = observer.worker_started().unwrap();
        let barrier = Arc::new(std::sync::Barrier::new(5));
        let mut threads = Vec::new();
        for _ in 0..4 {
            let source = observer.source(SourceKind::Local);
            let gate = barrier.clone();
            let context = observer.clone();
            threads.push(std::thread::spawn(move || {
                let first = source.received(1, Instant::now()).unwrap();
                source.eligible(&first, Effect::Inventory);
                gate.wait();
                for n in 2..=100 {
                    if let Some(ticket) = source.received(n, Instant::now()) {
                        source.eligible(&ticket, Effect::Inventory);
                    }
                    let proof = source.admit();
                    context.complete(proof.batch(), 1, 1, 1, Instant::now());
                    source.cached();
                }
            }));
        }
        barrier.wait();
        let sealed = observer.seal();
        let expected = serde_json::to_value(&sealed).unwrap();
        for thread in threads {
            thread.join().unwrap();
        }
        observer.worker_closed(worker);
        assert_eq!(serde_json::to_value(observer.seal()).unwrap(), expected);
        assert_eq!(sealed.worker_cleanup(), CleanupObservation::Unknown);
        assert_eq!(sealed.counts.pending, 0);
        assert_eq!(
            Observation::new().seal().worker_cleanup(),
            CleanupObservation::Unknown
        );
        let bounded = Observation::new();
        bounded.activation();
        let other = Observation::new();
        let foreign = other.worker_started().unwrap();
        let real = bounded.worker_started().unwrap();
        bounded.worker_closed(foreign);
        bounded.worker_closed(real);
        bounded.worker_closed(real);
        for _ in 0..=MAX_PENDING {
            bounded.worker_started();
        }
        let snapshot = bounded.seal();
        assert_eq!(snapshot.lifecycle.unmatched_closed, 2);
        assert_eq!(snapshot.lifecycle.untracked_workers, 1);
        assert_eq!(snapshot.worker_cleanup(), CleanupObservation::Unknown);
    }
    #[test]
    fn observation_frontier_preserves_original_starts_and_excludes_late_or_other_sessions() {
        let observation = Observation::new();
        let source = observation.source(SourceKind::Local);
        let started = Instant::now();
        let first = source.received(1, started).unwrap();
        source.eligible(&first, Effect::Inventory);
        let proof = source.admit();
        let late = source.received(2, Instant::now()).unwrap();
        source.eligible(&late, Effect::Inventory);
        let other = observation.source(SourceKind::Remote);
        let foreign = other.received(1, Instant::now()).unwrap();
        other.eligible(&foreign, Effect::Inventory);
        assert_eq!(proof.kind, ReadKind::Rpc);
        assert_eq!(proof.batch.tickets().len(), 1);
        assert_eq!(proof.batch.tickets()[0].started(), started);
        assert_eq!(proof.batch.tickets()[0].ordinal(), first.ordinal());
        assert!(late.ordinal() > proof.frontier);
        let progress = source.received(3, Instant::now()).unwrap();
        source.eligible(&progress, Effect::Operation);
        assert_eq!(source.batch().tickets().len(), 3);
        assert_eq!(
            source.admit().batch().tickets().len(),
            2,
            "inventory proof excludes operation progress"
        );
        let cache = source.cached();
        assert_eq!(cache.kind, ReadKind::InitialCache);
        assert!(cache.batch.tickets().is_empty() && cache.request.is_none());
        assert!(source.received(1, Instant::now()).is_none());
        assert_eq!(observation.counts().duplicate, 1);
        observation.retire(&proof.batch, Retirement::Unknown);
        observation.retire(&proof.batch, Retirement::Unknown);
        assert_eq!(observation.counts().unknown_retirement, 1);
        let second_context = Observation::new();
        let second_source = second_context.source(SourceKind::Local);
        let second_ticket = second_source.received(1, Instant::now()).unwrap();
        second_source.eligible(&first, Effect::Inventory);
        assert!(
            second_source.batch().tickets().is_empty(),
            "foreign context cannot classify equal ordinals"
        );
        second_source.eligible(&second_ticket, Effect::Inventory);
        second_context.retire(&proof.batch, Retirement::Superseded);
        assert_eq!(
            second_context.counts().pending,
            1,
            "foreign context must not retire equal ordinals"
        );
        assert_eq!(second_context.counts().unknown_retirement, 1);
    }
    #[test]
    fn observation_pending_records_overflow_and_retirement_are_bounded_and_scalar() {
        let observation = Observation::new();
        let source = observation.source(SourceKind::Local);
        for _ in 0..MAX_PENDING {
            source.selection().unwrap();
        }
        assert!(source.request().is_none());
        assert_eq!(observation.counts().pending, MAX_PENDING);
        assert_eq!(observation.counts().overflow, 1);
        for _ in 0..3 {
            observation.retire(&source.batch(), Retirement::Superseded);
            for _ in 0..MAX_PENDING {
                source.request().unwrap();
            }
        }
        assert_eq!(observation.records().len(), MAX_RECORDS);
        assert!(observation.counts().dropped_records > 0);
        source.retire_all(Retirement::SessionClosed);
        assert_eq!(observation.counts().pending, 0);
        let records = serde_json::to_value(observation.records()).unwrap();
        let allowed = ["source", "ticket", "request", "elapsed_micros", "phase"];
        for row in records.as_array().unwrap() {
            assert!(
                row.as_object()
                    .unwrap()
                    .keys()
                    .all(|key| allowed.contains(&key.as_str()))
            );
        }
    }
    #[test]
    fn observation_live_merge_keeps_unique_sources_starts_and_rejects_foreign_context() {
        let observation = Observation::new();
        let source = observation.source(SourceKind::Local);
        let first = source.received(1, Instant::now()).unwrap();
        source.eligible(&first, Effect::Inventory);
        let progress = source.received(2, Instant::now()).unwrap();
        source.eligible(&progress, Effect::Operation);
        let another = observation.source(SourceKind::Remote);
        let second = another.received(1, Instant::now()).unwrap();
        another.eligible(&second, Effect::Inventory);
        let combined = observation.merge(&source.batch(), &another.batch());
        assert_eq!(combined.tickets().len(), 3);
        assert_eq!(observation.merge(&combined, &combined).tickets().len(), 3);
        assert_eq!(observation.inventory(&combined).tickets().len(), 2);
        assert_eq!(combined.tickets()[0].started(), first.started());
        assert_ne!(
            combined.tickets()[0].source(),
            combined.tickets()[2].source()
        );
        assert_eq!(
            source.admit().batch().tickets().len(),
            1,
            "merge never lends another source to a read proof"
        );
        let foreign = Observation::new();
        let foreign_source = foreign.source(SourceKind::Local);
        foreign_source.request().unwrap();
        assert!(
            observation
                .live(&foreign_source.batch())
                .tickets()
                .is_empty()
        );
        assert_eq!(observation.counts().rejected_batch_tickets, 1);
        observation.retire(&source.batch(), Retirement::Superseded);
        let live = observation.live(&combined);
        assert_eq!(live.tickets().len(), 1);
        assert_eq!(live.tickets()[0].ordinal(), second.ordinal());
        assert_eq!(live.tickets()[0].started(), second.started());
        for _ in 1..MAX_PENDING {
            another.request().unwrap();
        }
        let full = another.batch();
        assert_eq!(observation.merge(&full, &full).tickets().len(), MAX_PENDING);
    }
    #[test]
    fn terminal_samples_keep_originals_once_independent_of_phase_capacity() {
        let observer = Observation::new();
        let source = observer.source(SourceKind::Local);
        let first = source.request().unwrap();
        let second = source.selection().unwrap();
        let batch = source.batch();
        let finish = Instant::now();
        let clock = observer.clock_binding();
        observer.complete(&batch, 7, clock, 13, finish);
        assert_eq!(observer.counts().completed, 2);
        for (row, ticket) in observer.terminals().iter().zip([first, second]) {
            assert_eq!(row.context, ticket.context);
            assert_eq!(row.source, ticket.source());
            assert_eq!(row.source_kind, Some(SourceKind::Local));
            let origin = observer.0.lock().unwrap().origin;
            assert_eq!(
                row.started_micros,
                Some(ticket.started().duration_since(origin).as_micros() as u64)
            );
            assert_eq!(row.ticket, ticket.ordinal());
            assert_eq!(row.kind, ticket.kind());
            assert_eq!(
                row.elapsed_micros,
                Some(finish.duration_since(ticket.started()).as_micros() as u64)
            );
            assert_eq!(
                row.outcome,
                TerminalOutcome::Paint {
                    revision: 7,
                    clock,
                    frame: 13
                }
            );
        }
        observer.complete(&batch, 8, clock, 14, Instant::now());
        assert_eq!(observer.terminals().len(), 2);
        assert_eq!(observer.counts().duplicate_completion, 2);
        let foreign = Observation::new();
        let other = foreign.source(SourceKind::Remote);
        other.request().unwrap();
        observer.complete(&other.batch(), 8, clock, 14, Instant::now());
        assert_eq!(observer.counts().rejected_batch_tickets, 1);
        let premature = source.request().unwrap();
        observer.complete(
            &source.batch(),
            9,
            clock,
            15,
            premature.started() - std::time::Duration::from_secs(1),
        );
        assert_eq!(observer.counts().unknown, 1);
        assert_eq!(
            observer.terminals()[2].outcome,
            TerminalOutcome::Retired(Retirement::Unknown)
        );
        // 5000 received samples remain complete even when optional phase rows fill.
        for sequence in 1..=5000 {
            let receipt = source.received(sequence, Instant::now()).unwrap();
            source.eligible(&receipt, Effect::Inventory);
            observer.complete(&source.batch(), 10, clock, 16, Instant::now());
        }
        assert_eq!(observer.counts().completed, 5002);
        assert_eq!(observer.terminals().len(), 5003);
        assert_eq!(observer.records().len(), MAX_RECORDS);
        assert!(observer.counts().dropped_records > 0);
        assert_eq!(observer.counts().dropped_terminals, 0);
        let late_source = observer.source(SourceKind::Remote);
        late_source.request().unwrap();
        observer.complete(&late_source.batch(), 11, clock, 17, Instant::now());
        assert_eq!(
            observer.terminals().last().unwrap().source_kind,
            Some(SourceKind::Remote),
            "cohort must survive even when this source's phase row was dropped"
        );
        assert!(
            !observer
                .records()
                .iter()
                .any(|row| row.source == late_source.ordinal())
        );
        for _ in 5004..MAX_TERMINALS + 3 {
            source.request().unwrap();
            observer.complete(&source.batch(), 11, clock, 17, Instant::now());
        }
        assert_eq!(observer.terminals().len(), MAX_TERMINALS);
        assert_eq!(observer.counts().dropped_terminals, 3);
        assert_eq!(observer.counts().pending, 0);
        let rows = serde_json::to_value(observer.terminals()).unwrap();
        let allowed = [
            "context",
            "source",
            "source_kind",
            "origin",
            "gui_view",
            "gui_remote",
            "coverage",
            "ticket",
            "kind",
            "effect",
            "elapsed_micros",
            "started_micros",
            "outcome",
        ];
        assert!(rows.as_array().unwrap().iter().all(|row| {
            row.as_object()
                .unwrap()
                .keys()
                .all(|key| allowed.contains(&key.as_str()))
        }));
    }
    #[test]
    fn gui_starts_have_truthful_non_rpc_origin_remote_cohort_and_bounds() {
        let observer = Observation::new();
        let view = observer.view();
        let request = observer.view_start(view, false, true);
        let selected = observer.view_start(view, true, false);
        assert_eq!(observer.counts().session_sources, 0);
        let merged = observer.merge(&request, &selected);
        observer.retire(&merged, Retirement::Unknown);
        let rows = observer.terminals();
        assert_eq!(rows[0].origin, Origin::Gui);
        assert_eq!(rows[0].source, 0);
        assert_eq!(rows[0].source_kind, None);
        assert_eq!(rows[0].gui_view, Some(view));
        assert_eq!(rows[0].gui_remote, Some(true));
        assert_eq!(rows[1].gui_remote, Some(false));
        assert_eq!(rows[0].kind, StartKind::LocalRequest);
        assert_eq!(rows[1].kind, StartKind::LoadedSelection);
        assert!(request.tickets()[0].started() <= selected.tickets()[0].started());
        for _ in 0..MAX_PENDING {
            observer.view_start(view, false, false);
        }
        assert!(observer.view_start(view, false, false).tickets().is_empty());
        assert_eq!(observer.counts().pending, MAX_PENDING);
        assert_eq!(observer.counts().gui_start_attempts, MAX_PENDING as u64 + 3);
        assert_eq!(observer.counts().overflow, 1);
        assert_eq!(observer.counts().session_sources, 0);
    }
}

#[cfg(test)]
mod correlation_tests {
    use super::*;
    #[test]
    fn correlation_default_has_no_classifier_index_or_snapshot_field() {
        let observer = Observation::new();
        let source = observer.source(SourceKind::Local);
        assert!(source.event_tagger().is_none());
        source.received(1, Instant::now()).unwrap();
        assert!(observer.correlation().is_none());
        assert!(
            serde_json::to_value(observer.seal())
                .unwrap()
                .get("correlation")
                .is_none()
        );
    }
    #[test]
    fn correlation_retains_failed_admissions_and_index_counts_after_phase_overflow() {
        let observer = Observation::with_event_tagger(Arc::new(|_| Ok(EventTag::Other)));
        let source = observer.source(SourceKind::Remote);
        for _ in 0..MAX_PENDING {
            source.request().unwrap();
        }
        for sequence in 1..=MAX_CORRELATIONS as u64 + 2 {
            assert!(
                source
                    .received_classified(
                        sequence,
                        Instant::now(),
                        Some(EventTag::Matched(sequence))
                    )
                    .is_none()
            );
        }
        let correlation = observer.correlation().unwrap();
        assert_eq!(correlation.counts.valid_raw, MAX_CORRELATIONS as u64 + 2);
        assert_eq!(correlation.counts.tagged, correlation.counts.valid_raw);
        assert_eq!(
            correlation.counts.admission_drops,
            correlation.counts.valid_raw
        );
        assert_eq!(correlation.counts.admitted, 0);
        assert_eq!(correlation.counts.index_drops, 2);
        assert_eq!(correlation.index.len(), MAX_CORRELATIONS);
        assert!(correlation.index.iter().all(|row| row.ticket.is_none()
            && row.admission == CorrelationAdmission::Overflow
            && row.source == source.ordinal()
            && row.source_kind == SourceKind::Remote));
        assert!(observer.counts().dropped_records > 0);
        let sealed = observer.seal();
        assert_eq!(
            serde_json::to_value(&sealed).unwrap()["counts"]["unknown"],
            MAX_PENDING
        );
    }
    #[test]
    fn correlation_terminal_loss_is_independent_and_join_keeps_original_start() {
        let observer = Observation::with_event_tagger(Arc::new(|_| Ok(EventTag::Other)));
        let source = observer.source(SourceKind::Local);
        let clock = observer.clock_binding();
        for sequence in 1..=MAX_TERMINALS as u64 + 2 {
            let started = Instant::now();
            let ticket = source
                .received_classified(sequence, started, Some(EventTag::Matched(sequence)))
                .unwrap();
            source.eligible(&ticket, Effect::Inventory);
            observer.complete(&source.batch(), 1, clock, 1, Instant::now());
            assert_eq!(ticket.started(), started);
        }
        let correlation = observer.correlation().unwrap();
        let terminals = observer.terminals();
        assert_eq!(correlation.counts.admitted, MAX_TERMINALS as u64 + 2);
        assert_eq!(correlation.counts.index_drops, 2);
        assert_eq!(observer.counts().dropped_terminals, 2);
        assert!(observer.counts().dropped_records > 0);
        for (row, terminal) in correlation.index.iter().zip(&terminals) {
            assert_eq!(
                (row.context, row.source, row.ticket, row.started_micros),
                (
                    terminal.context,
                    terminal.source,
                    Some(terminal.ticket),
                    terminal.started_micros
                )
            );
        }
        let remote = observer.source(SourceKind::Remote);
        let ticket = remote
            .received_classified(1, Instant::now(), Some(EventTag::Other))
            .unwrap();
        assert_ne!(ticket.source(), source.ordinal());
        assert_eq!(observer.correlation().unwrap().counts.other, 1);
    }
    #[test]
    fn correlation_seal_is_immutable_with_held_classifier_and_late_sources() {
        use std::sync::Barrier;
        let entered = Arc::new(Barrier::new(2));
        let release = Arc::new(Barrier::new(2));
        let observer = Observation::with_event_tagger(Arc::new({
            let entered = entered.clone();
            let release = release.clone();
            move |_| {
                entered.wait();
                release.wait();
                Ok(EventTag::Matched(9))
            }
        }));
        let source = observer.source(SourceKind::Remote);
        let late = source.clone();
        let worker = std::thread::spawn(move || {
            let started = Instant::now();
            let reserved = late.reserve_event(1, started);
            let tag = late.classify_event(ValidatedEvent {
                event: "synthetic",
                operation_id: "",
                sequence: 1,
                revision: 1,
                received: started,
                frame: b"{}",
            });
            assert_eq!(late.finish_event(reserved, tag).unwrap().started(), started);
        });
        entered.wait();
        let sealed = serde_json::to_value(observer.seal()).unwrap();
        release.wait();
        worker.join().unwrap();
        assert!(
            source
                .classify_event(ValidatedEvent {
                    event: "synthetic",
                    operation_id: "",
                    sequence: 2,
                    revision: 2,
                    received: Instant::now(),
                    frame: b"{}"
                })
                .is_none()
        );
        let later = observer.source(SourceKind::Local);
        assert!(
            later
                .received_classified(1, Instant::now(), Some(EventTag::Other))
                .is_none()
        );
        drop(later);
        drop(source);
        assert_eq!(serde_json::to_value(observer.seal()).unwrap(), sealed);
        assert_eq!(observer.correlation().unwrap().counts.valid_raw, 1);
        assert_eq!(
            observer
                .correlation()
                .unwrap()
                .counts
                .classification_unknown,
            1
        );
        assert_eq!(observer.counts().unknown, 1);
    }
}

#[cfg(test)]
mod unresolved_classification_tests {
    use super::*;
    #[test]
    fn correlation_seal_counts_unfinished_reservations_even_after_index_and_admission_drop() {
        let observer = Observation::with_event_tagger(Arc::new(|_| Ok(EventTag::Other)));
        let source = observer.source(SourceKind::Remote);
        for sequence in 1..=MAX_CORRELATIONS as u64 + 1 {
            let _unfinished = source.reserve_event(sequence, Instant::now());
        }
        let correlation = observer.correlation().unwrap();
        assert_eq!(correlation.counts.valid_raw, MAX_CORRELATIONS as u64 + 1);
        assert_eq!(
            correlation.counts.classification_pending,
            correlation.counts.valid_raw
        );
        assert_eq!(
            correlation.counts.unclassifiable,
            correlation.counts.valid_raw
        );
        assert_eq!(correlation.counts.index_drops, 1);
        assert_eq!(
            correlation.counts.admission_drops,
            (MAX_CORRELATIONS + 1 - MAX_PENDING) as u64
        );
        assert!(
            correlation
                .index
                .iter()
                .all(|row| row.classification == CorrelationClassification::Pending)
        );
        let sealed = serde_json::to_value(observer.seal()).unwrap();
        let correlation = observer.correlation().unwrap();
        assert_eq!(correlation.counts.classification_pending, 0);
        assert_eq!(
            correlation.counts.classification_unknown,
            MAX_CORRELATIONS as u64 + 1
        );
        assert_eq!(
            correlation.counts.unclassifiable,
            correlation.counts.classification_unknown
        );
        assert_eq!(observer.counts().unknown, MAX_PENDING as u64);
        assert!(
            correlation
                .index
                .iter()
                .all(|row| row.classification == CorrelationClassification::Unknown)
        );
        drop(source);
        assert_eq!(serde_json::to_value(observer.seal()).unwrap(), sealed);
    }
}

#[cfg(test)]
mod read_source_tests {
    use super::*;
    #[test]
    fn relative_micros_is_read_only_and_shares_original_receipt_origin() {
        let before = Instant::now();
        let observation = Observation::new();
        assert!(observation.relative_micros(before).is_none());
        let source = observation.source(SourceKind::Local);
        let at = Instant::now();
        let ticket = source.received(1, at).unwrap();
        let expected = observation.relative_micros(at).unwrap();
        observation.retire(
            &Batch {
                tickets: vec![ticket],
            },
            Retirement::Unknown,
        );
        assert_eq!(observation.terminals()[0].started_micros, Some(expected));
        let first = serde_json::to_value(observation.seal()).unwrap();
        assert_eq!(observation.relative_micros(at), Some(expected));
        assert_eq!(serde_json::to_value(observation.seal()).unwrap(), first);
    }
    #[test]
    fn read_source_rejects_colliding_foreign_contexts_for_rpc_and_empty_cache() {
        let first = Observation::new();
        let second = Observation::new();
        let a = first.source(SourceKind::Local);
        let b = second.source(SourceKind::Remote);
        assert_eq!(a.ordinal(), b.ordinal());
        for proof in [a.admit(), a.cached()] {
            assert_eq!(first.read_source(&proof).unwrap().1, a.ordinal());
            assert!(second.read_source(&proof).is_none());
        }
        let cached = b.cached();
        assert!(cached.batch().tickets().is_empty());
        assert_eq!(cached.frontier(), 0);
        assert!(first.read_source(&cached).is_none());
        assert!(second.read_source(&cached).is_some());
    }
    #[test]
    fn read_source_rejects_zero_unallocated_sealed_and_postseal_without_changing_frontier() {
        let observer = Observation::new();
        let source = observer.source(SourceKind::Local);
        let start = Instant::now();
        let ticket = source.received(1, start).unwrap();
        source.eligible(&ticket, Effect::Inventory);
        let proof = source.admit();
        assert_eq!(proof.batch().tickets()[0].started(), start);
        let cached = source.cached();
        assert!(cached.batch().tickets().is_empty());
        let mut bad = proof.clone();
        bad.source = 0;
        assert!(observer.read_source(&bad).is_none());
        bad.source = source.ordinal() + 1;
        assert!(observer.read_source(&bad).is_none());
        bad = proof.clone();
        bad.kind = ReadKind::Sealed;
        assert!(observer.read_source(&bad).is_none());
        observer.seal();
        assert!(observer.read_source(&proof).is_none());
        assert!(observer.read_source(&cached).is_none());
        assert_eq!(source.admit().kind(), ReadKind::Sealed);
        assert_eq!(source.cached().kind(), ReadKind::Sealed);
        let inert = observer.source(SourceKind::Local);
        assert_eq!(inert.ordinal(), 0);
        assert!(observer.read_source(&inert.cached()).is_none());
    }
}
