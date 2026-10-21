//! Optional fixture correlation policy. Tags are not source trust or paint authority.
use serde::Deserialize;
use std::collections::{BTreeMap, BTreeSet};
use std::ffi::CString;
use std::fs::{File, Metadata};
use std::io::{Read, Seek, SeekFrom};
use std::os::fd::{AsRawFd, FromRawFd};
use std::os::unix::ffi::OsStrExt;
use std::os::unix::fs::MetadataExt;
use std::path::{Component, Path};
use std::sync::{Arc, OnceLock};
use std::time::Instant;
use veranda_lib::observation::{EventTag, EventTagger, ValidatedEvent};

/// Private expected identities. These are fixture configuration, never trace fields.
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ExpectedCohort {
    pub ordinal: u64,
    pub owner: String,
    pub connection: Option<String>,
    pub yard: Option<String>,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq, serde::Serialize)]
pub enum Role {
    Fleet,
    Details,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq, serde::Serialize)]
pub struct SourceBinding {
    pub cohort: u64,
    pub context: u64,
    pub source: u64,
    pub role: Role,
}

/// Collects provenance only. It never changes event membership, scheduling, or paint coverage.
/// No ready file or qualifying envelope exists until the caller implements owned readiness.
pub struct Cohorts {
    observation: veranda_lib::observation::Observation,
    expected: Vec<ExpectedCohort>,
    bindings: Vec<SourceBinding>,
    fleet: BTreeMap<u64, (u64, crate::state::Scope)>,
    frozen: bool,
    replacement_unknown: bool,
    sealed: bool,
}
impl Cohorts {
    pub fn new(
        observation: veranda_lib::observation::Observation,
        expected: Vec<ExpectedCohort>,
    ) -> Result<Self, Fault> {
        if expected.is_empty() || expected.len() > 64 {
            return Err(Fault::Bounds);
        }
        let mut ordinals = BTreeSet::new();
        let mut targets = BTreeSet::new();
        for row in &expected {
            if row.ordinal == 0
                || !ordinals.insert(row.ordinal)
                || !bounded_ascii(&row.owner, 128)
                || row
                    .connection
                    .as_ref()
                    .is_some_and(|v| !bounded_ascii(v, 128))
                || row.yard.as_ref().is_some_and(|v| !bounded_ascii(v, 128))
                || !targets.insert((row.connection.clone(), row.yard.clone()))
            {
                return Err(Fault::Config);
            }
        }
        Ok(Self {
            observation,
            expected,
            bindings: Vec::new(),
            fleet: BTreeMap::new(),
            frozen: false,
            replacement_unknown: false,
            sealed: false,
        })
    }
    pub fn bindings(&self) -> &[SourceBinding] {
        &self.bindings
    }
    pub fn replacement_unknown(&self) -> bool {
        self.replacement_unknown
    }
    pub fn complete(&self) -> bool {
        !self.replacement_unknown
            && self.expected.iter().all(|row| {
                self.bindings
                    .iter()
                    .any(|b| b.cohort == row.ordinal && b.role == Role::Fleet)
                    && (row.yard.is_none()
                        || self
                            .bindings
                            .iter()
                            .any(|b| b.cohort == row.ordinal && b.role == Role::Details))
            })
    }
    /// Call only with the typed result retained before Model consumes it. All original
    /// acceptance facts are checked here; cached model Owner IDs are not an input.
    pub fn fleet(
        &mut self,
        model: &crate::model::Model,
        generation: u64,
        connection: &Option<String>,
        current: bool,
        action: &crate::model::ReadAction,
        returned_owner: Option<&str>,
        metadata: Option<&crate::worker::ReadObservation>,
    ) {
        if self.sealed
            || !current
            || *action != crate::model::ReadAction::Applied
            || !model.accepts_reply(generation, connection)
        {
            return;
        }
        let Some(owner) = returned_owner else {
            return;
        };
        let scope = crate::state::Scope {
            connection: model.selection.connection_id.clone(),
            yard: model.selection.yard_name.clone(),
        };
        // A newly accepted identity cannot keep an earlier expected Owner's
        // Fleet-to-Details join alive, even if a future caller bypasses the
        // Client's existing session identity refusal. Historical scalar roles
        // remain inspectable, but the cohort is explicitly incomplete. This
        // contradiction concerns the whole connection, independent of yard
        // normalization or optional new measurement proof.
        let replaced: Vec<_> = self
            .expected
            .iter()
            .filter(|row| {
                row.connection == *connection
                    && row.owner != owner
                    && self.fleet.contains_key(&row.ordinal)
            })
            .map(|row| row.ordinal)
            .collect();
        for ordinal in replaced {
            self.fleet.remove(&ordinal);
            self.replacement_unknown = true;
        }
        // Proof is required to grant a NEW source binding, never to revoke a
        // prior identity join contradicted by an accepted typed result.
        let Some(metadata) = metadata.filter(|m| m.scope.connection == *connection) else {
            return;
        };
        let Some((context, source)) = metadata
            .proof
            .as_ref()
            .and_then(|p| self.observation.read_source(p))
        else {
            return;
        };
        let matches: Vec<_> = self
            .expected
            .iter()
            .filter(|r| {
                r.owner == owner && r.connection == scope.connection && r.yard == scope.yard
            })
            .map(|r| r.ordinal)
            .collect();
        for ordinal in matches {
            self.bind(ordinal, context, source, Role::Fleet);
            if !self.frozen {
                self.fleet.insert(ordinal, (generation, scope.clone()));
            }
        }
    }
    /// The caller must be inside the same original successful/current Details branch.
    /// Failed/fallback required parts are excluded even though their product value is Ok.
    pub fn details(
        &mut self,
        model: &crate::model::Model,
        generation: u64,
        scope: &crate::state::Scope,
        current: bool,
        required_parts_success: bool,
        metadata: Option<&crate::worker::ReadObservation>,
    ) {
        let actual = crate::state::Scope {
            connection: model.selection.connection_id.clone(),
            yard: model.selection.yard_name.clone(),
        };
        if self.sealed
            || !current
            || !required_parts_success
            || !model.host().fresh
            || model.generation != generation
            || actual != *scope
        {
            return;
        }
        let Some(metadata) = metadata.filter(|m| m.scope == *scope) else {
            return;
        };
        let Some((context, source)) = metadata
            .proof
            .as_ref()
            .and_then(|p| self.observation.read_source(p))
        else {
            return;
        };
        let matches: Vec<_> = self
            .fleet
            .iter()
            .filter(|(_, v)| v.0 == generation && v.1 == *scope)
            .map(|(k, _)| *k)
            .collect();
        for ordinal in matches {
            self.bind(ordinal, context, source, Role::Details);
        }
    }
    fn bind(&mut self, cohort: u64, context: u64, source: u64, role: Role) {
        if self.sealed {
            return;
        }
        if let Some(old) = self
            .bindings
            .iter()
            .find(|b| b.cohort == cohort && b.role == role)
        {
            if old.context != context || old.source != source {
                self.replacement_unknown = true;
            }
            return;
        }
        if self.frozen || self.sealed {
            self.replacement_unknown = true;
            return;
        }
        self.bindings.push(SourceBinding {
            cohort,
            context,
            source,
            role,
        });
    }
    /// Freezes known source roles once. This alone is not readiness or source liveness.
    pub fn freeze(&mut self, classifier: &Classifier) -> Result<Instant, Fault> {
        if self.frozen || self.sealed {
            return Err(Fault::AlreadyFrozen);
        }
        if !self.complete() || self.replacement_unknown {
            return Err(Fault::Config);
        }
        let at = classifier.freeze()?;
        self.frozen = true;
        Ok(at)
    }
}

pub const MAX_CONFIG_BYTES: usize = 512 * 1024;
pub const MAX_OFFERS: usize = 2048;
pub const MAX_EVENT_BYTES: usize = 1024 * 1024;
const EVENT_KIND: &str = "incus.lifecycle";

/// Fixed scalar failures only. Never include an OS/serde error or private path/payload.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Fault {
    Path,
    Parent,
    File,
    Read,
    Drift,
    Config,
    Bounds,
    AlreadyFrozen,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Config {
    schema: u8,
    offers: Vec<Offer>,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Offer {
    offer: u64,
    action: String,
    source: String,
}

/// Private immutable exact mapping. No URI normalization, generation, or polling.
pub struct Classifier {
    offers: BTreeMap<(String, String), u64>,
    frozen: OnceLock<Instant>,
}
impl Classifier {
    pub fn load(path: &Path) -> Result<Self, Fault> {
        Self::load_checked(path, || {})
    }
    fn load_checked(path: &Path, after_read: impl FnOnce()) -> Result<Self, Fault> {
        let first = private_bytes_checked(path, after_read)?;
        let config: Config = serde_json::from_slice(&first).map_err(|_| Fault::Config)?;
        if config.schema != 1 {
            return Err(Fault::Config);
        }
        Self::from_offers(config.offers)
    }
    fn from_offers(rows: Vec<Offer>) -> Result<Self, Fault> {
        if rows.is_empty() || rows.len() > MAX_OFFERS {
            return Err(Fault::Bounds);
        }
        let mut offers = BTreeMap::new();
        let mut ordinals = BTreeSet::new();
        for row in rows {
            if row.offer == 0
                || !bounded_ascii(&row.action, 128)
                || !bounded_ascii(&row.source, 2048)
                || !row.source.starts_with('/')
            {
                return Err(Fault::Config);
            }
            if !ordinals.insert(row.offer)
                || offers.insert((row.action, row.source), row.offer).is_some()
            {
                return Err(Fault::Config);
            }
        }
        Ok(Self {
            offers,
            frozen: OnceLock::new(),
        })
    }
    /// This is only an original-Instant gate. It establishes no trusted Source cohort.
    pub fn freeze(&self) -> Result<Instant, Fault> {
        let at = Instant::now();
        self.frozen.set(at).map_err(|_| Fault::AlreadyFrozen)?;
        Ok(at)
    }
    pub fn tagger(self: Arc<Self>) -> EventTagger {
        Arc::new(move |event| Ok(self.classify(event)))
    }
    pub fn classify(&self, event: ValidatedEvent<'_>) -> EventTag {
        if event.event != EVENT_KIND {
            return EventTag::Other;
        }
        let Some(frozen) = self.frozen.get() else {
            return EventTag::Unclassifiable;
        };
        // Equality is conservatively unqualified; finishing after freeze cannot promote old receipts.
        if event.received <= *frozen || event.frame.len() > MAX_EVENT_BYTES {
            return EventTag::Unclassifiable;
        }
        let Ok(frame) = serde_json::from_slice::<Envelope>(event.frame) else {
            return EventTag::Unclassifiable;
        };
        if frame.version != 1
            || frame.kind != "event"
            || frame.event != event.event
            || frame.operation_id != event.operation_id
            || frame.sequence == 0
            || frame.sequence != event.sequence
            || frame.revision != event.revision
            || frame.revision != frame.sequence
            || !bounded_ascii(&frame.data.data.action, 128)
            || !bounded_ascii(&frame.data.data.source, 2048)
        {
            return EventTag::Unclassifiable;
        }
        self.offers
            .get(&(frame.data.data.action, frame.data.data.source))
            .copied()
            .map(EventTag::Matched)
            .unwrap_or(EventTag::Other)
    }
}

fn bounded_ascii(value: &str, max: usize) -> bool {
    !value.is_empty() && value.len() <= max && value.bytes().all(|byte| byte.is_ascii_graphic())
}

pub const CONFIG_ENV: &str = "SUBYARD_VERANDA_CORRELATION_CONFIG";
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct UsageConfig {
    capacity: usize,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct CorrelationConfig {
    offers: Vec<Offer>,
    cohorts: Vec<ExpectedCohort>,
    ready: String,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct ObservationConfig {
    schema: u8,
    child_usage: UsageConfig,
    #[serde(deserialize_with = "required_correlation")]
    correlation: Option<CorrelationConfig>,
}
fn required_correlation<'de, D: serde::Deserializer<'de>>(
    deserializer: D,
) -> Result<Option<CorrelationConfig>, D::Error> {
    Option::<CorrelationConfig>::deserialize(deserializer)
}
pub struct ObservationPlan {
    capacity: usize,
    correlation: Option<RuntimePlan>,
}
impl ObservationPlan {
    pub fn load(path: &Path, trace: &Path) -> Result<Self, Fault> {
        private_parent(trace)?;
        let bytes = private_bytes_checked(path, || {})?;
        let config: ObservationConfig =
            serde_json::from_slice(&bytes).map_err(|_| Fault::Config)?;
        if config.schema != 3
            || config.child_usage.capacity == 0
            || config.child_usage.capacity > veranda_lib::child_usage::MAX_RECORDS
        {
            return Err(Fault::Bounds);
        }
        let correlation = config
            .correlation
            .map(|c| {
                RuntimePlan::from_config(
                    RuntimeConfig {
                        schema: 2,
                        offers: c.offers,
                        cohorts: c.cohorts,
                        ready: c.ready,
                    },
                    trace,
                )
            })
            .transpose()?;
        Ok(Self {
            capacity: config.child_usage.capacity,
            correlation,
        })
    }
    pub fn start(
        self,
    ) -> Result<(veranda_lib::observation::Observation, Option<RuntimeRun>), Fault> {
        if let Some(plan) = self.correlation {
            let (observation, runtime) = plan.start_with_usage(Some(self.capacity))?;
            Ok((observation, Some(runtime)))
        } else {
            Ok((
                veranda_lib::observation::Observation::with_child_usage(self.capacity),
                None,
            ))
        }
    }
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct RuntimeConfig {
    schema: u8,
    offers: Vec<Offer>,
    cohorts: Vec<ExpectedCohort>,
    ready: String,
}
pub struct RuntimePlan {
    classifier: Arc<Classifier>,
    expected: Vec<ExpectedCohort>,
    ready: std::path::PathBuf,
    parent_identity: (u64, u64),
}
impl RuntimePlan {
    pub fn load(path: &Path, trace: &Path) -> Result<Self, Fault> {
        let bytes = private_bytes_checked(path, || {})?;
        let config: RuntimeConfig = serde_json::from_slice(&bytes).map_err(|_| Fault::Config)?;
        Self::from_config(config, trace)
    }
    fn from_config(config: RuntimeConfig, trace: &Path) -> Result<Self, Fault> {
        private_parent(trace)?;
        if config.schema != 2 || Path::new(&config.ready) == trace {
            return Err(Fault::Config);
        }
        let ready = std::path::PathBuf::from(config.ready);
        let (parent, _) = private_parent(&ready)?;
        let meta = parent.metadata().map_err(|_| Fault::Parent)?;
        // Validate private expected targets before Observation or thread birth.
        validate_targets(&config.cohorts)?;
        Ok(Self {
            classifier: Arc::new(Classifier::from_offers(config.offers)?),
            expected: config.cohorts,
            ready,
            parent_identity: (meta.dev(), meta.ino()),
        })
    }
    pub fn start(self) -> Result<(veranda_lib::observation::Observation, RuntimeRun), Fault> {
        self.start_with_usage(None)
    }
    fn start_with_usage(
        self,
        capacity: Option<usize>,
    ) -> Result<(veranda_lib::observation::Observation, RuntimeRun), Fault> {
        let observation = veranda_lib::observation::Observation::with_observers(
            Some(self.classifier.clone().tagger()),
            capacity,
        );
        let cohorts = Arc::new(std::sync::Mutex::new(Cohorts::new(
            observation.clone(),
            self.expected,
        )?));
        let (sender, receiver) = std::sync::mpsc::sync_channel(1);
        let stopped = Arc::new(std::sync::atomic::AtomicBool::new(false));
        let state = Arc::new(std::sync::Mutex::new(WriterResult {
            writer: WriterOutcome::Pending,
            ready: None,
        }));
        let handle = RuntimeHandle {
            cohorts: cohorts.clone(),
            sender,
            stopped: stopped.clone(),
        };
        let output = state.clone();
        let thread_observation = observation.clone();
        let stop = stopped.clone();
        let thread = std::thread::Builder::new()
            .name("veranda-readiness".into())
            .spawn(move || {
                use std::sync::atomic::Ordering;
                while receiver.recv().is_ok() {
                    if stop.load(Ordering::Acquire) {
                        output.lock().unwrap().writer = WriterOutcome::Stopped;
                        return;
                    }
                    let payload = {
                        let mut bound = cohorts.lock().unwrap();
                        if !bound.complete() {
                            continue;
                        }
                        // Capture first: concurrent births are conservatively late,
                        // never silently admitted by a post-freeze counter sample.
                        let source_frontier = thread_observation.counts().session_sources;
                        let Ok(at) = bound.freeze(&self.classifier) else {
                            output.lock().unwrap().writer = WriterOutcome::Failed;
                            return;
                        };
                        let Some(freeze_micros) = thread_observation.relative_micros(at) else {
                            output.lock().unwrap().writer = WriterOutcome::Failed;
                            return;
                        };
                        ReadyPayload {
                            schema_version: 1,
                            context: bound.bindings[0].context,
                            freeze_micros,
                            source_frontier,
                            bindings: bound.bindings.clone(),
                        }
                    };
                    let result = serde_json::to_vec(&payload)
                        .map_err(|_| ())
                        .and_then(|bytes| {
                            crate::trace_sink::publish_ready(
                                &self.ready,
                                self.parent_identity,
                                &bytes,
                                &stop,
                            )
                            .map_err(|_| ())
                        });
                    let mut result_state = output.lock().unwrap();
                    result_state.ready = Some(payload);
                    result_state.writer = if result.is_ok() {
                        WriterOutcome::Written
                    } else {
                        WriterOutcome::Failed
                    };
                    return;
                }
                output.lock().unwrap().writer = WriterOutcome::Stopped;
            })
            .map_err(|_| Fault::Read)?;
        Ok((
            observation,
            RuntimeRun {
                handle,
                state,
                thread: Some(thread),
            },
        ))
    }
}
fn validate_targets(expected: &[ExpectedCohort]) -> Result<(), Fault> {
    if expected.is_empty() || expected.len() > 64 {
        return Err(Fault::Bounds);
    }
    let mut ordinals = BTreeSet::new();
    let mut targets = BTreeSet::new();
    for row in expected {
        if row.ordinal == 0
            || !ordinals.insert(row.ordinal)
            || !bounded_ascii(&row.owner, 128)
            || row
                .connection
                .as_ref()
                .is_some_and(|v| !bounded_ascii(v, 128))
            || row.yard.as_ref().is_some_and(|v| !bounded_ascii(v, 128))
            || !targets.insert((row.connection.clone(), row.yard.clone()))
        {
            return Err(Fault::Config);
        }
    }
    Ok(())
}
#[derive(Clone)]
pub struct RuntimeHandle {
    pub(crate) cohorts: Arc<std::sync::Mutex<Cohorts>>,
    sender: std::sync::mpsc::SyncSender<()>,
    stopped: Arc<std::sync::atomic::AtomicBool>,
}
impl RuntimeHandle {
    pub fn changed(&self) {
        if !self.stopped.load(std::sync::atomic::Ordering::Acquire) {
            let _ = self.sender.try_send(());
        }
    }
}
#[derive(Clone, Copy, Debug, PartialEq, Eq, serde::Serialize)]
pub enum WriterOutcome {
    Pending,
    Written,
    Failed,
    Stopped,
}
#[derive(Clone, Debug, PartialEq, Eq, serde::Serialize)]
pub struct ReadyPayload {
    pub schema_version: u8,
    pub context: u64,
    pub freeze_micros: u64,
    pub source_frontier: u64,
    pub bindings: Vec<SourceBinding>,
}
struct WriterResult {
    writer: WriterOutcome,
    ready: Option<ReadyPayload>,
}
pub struct RuntimeRun {
    handle: RuntimeHandle,
    state: Arc<std::sync::Mutex<WriterResult>>,
    thread: Option<std::thread::JoinHandle<()>>,
}
#[derive(serde::Serialize)]
pub struct RuntimeEnvelope {
    pub schema_version: u8,
    pub phase: RuntimePhase,
    pub bindings: Vec<SourceBinding>,
    pub writer: WriterOutcome,
    pub ready: Option<ReadyPayload>,
    pub cohorts_complete: bool,
    pub replacement_unknown: bool,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq, serde::Serialize)]
pub enum RuntimePhase {
    Cold,
    Warming,
    Frozen,
}
impl RuntimeRun {
    pub fn handle(&self) -> RuntimeHandle {
        self.handle.clone()
    }
    pub fn finish(mut self) -> RuntimeEnvelope {
        self.stop();
        let state = self.state.lock().unwrap();
        let mut cohorts = self.handle.cohorts.lock().unwrap();
        cohorts.sealed = true;
        RuntimeEnvelope {
            schema_version: 1,
            phase: if cohorts.frozen {
                RuntimePhase::Frozen
            } else if cohorts.bindings.is_empty() {
                RuntimePhase::Cold
            } else {
                RuntimePhase::Warming
            },
            bindings: cohorts.bindings.clone(),
            writer: state.writer,
            ready: state.ready.clone(),
            cohorts_complete: cohorts.complete(),
            replacement_unknown: cohorts.replacement_unknown(),
        }
    }
    fn stop(&mut self) {
        self.handle
            .stopped
            .store(true, std::sync::atomic::Ordering::Release);
        let _ = self.handle.sender.try_send(());
        if let Some(thread) = self.thread.take() {
            if thread.join().is_err() {
                self.state.lock().unwrap().writer = WriterOutcome::Failed;
            }
        }
    }
}
impl Drop for RuntimeRun {
    fn drop(&mut self) {
        self.stop();
    }
}
#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Envelope {
    version: u32,
    #[serde(rename = "type")]
    kind: String,
    event: String,
    #[serde(default)]
    operation_id: String,
    sequence: u64,
    revision: u64,
    data: EventData,
}
#[derive(Deserialize)]
struct EventData {
    data: Lifecycle,
}
#[derive(Deserialize)]
struct Lifecycle {
    action: String,
    source: String,
}

fn private_bytes_checked(path: &Path, after_read: impl FnOnce()) -> Result<Vec<u8>, Fault> {
    let (parent, leaf) = private_parent(path)?;
    let parent_identity = identity(&parent.metadata().map_err(|_| Fault::Parent)?);
    let fd = unsafe {
        libc::openat(
            parent.as_raw_fd(),
            leaf.as_ptr(),
            libc::O_RDONLY | libc::O_CLOEXEC | libc::O_NOFOLLOW | libc::O_NONBLOCK,
        )
    };
    if fd < 0 {
        return Err(Fault::File);
    }
    let mut file = unsafe { File::from_raw_fd(fd) };
    let metadata = file.metadata().map_err(|_| Fault::File)?;
    let uid = unsafe { libc::getuid() };
    if !metadata.is_file()
        || metadata.uid() != uid
        || metadata.nlink() != 1
        || metadata.mode() & 0o7777 != 0o600
    {
        return Err(Fault::File);
    }
    if metadata.len() == 0 || metadata.len() > MAX_CONFIG_BYTES as u64 {
        return Err(Fault::Bounds);
    }
    let initial = identity(&metadata);
    let first = read_bounded(&mut file)?;
    after_read();
    file.seek(SeekFrom::Start(0)).map_err(|_| Fault::Read)?;
    let second = read_bounded(&mut file)?;
    if first != second || identity(&file.metadata().map_err(|_| Fault::Drift)?) != initial {
        return Err(Fault::Drift);
    }
    // Re-open the exact anchored entry and parent to reject same-byte inode/path replacement.
    let (again, again_leaf) = private_parent(path)?;
    if identity(&again.metadata().map_err(|_| Fault::Drift)?) != parent_identity {
        return Err(Fault::Drift);
    }
    let named_fd = unsafe {
        libc::openat(
            again.as_raw_fd(),
            again_leaf.as_ptr(),
            libc::O_RDONLY | libc::O_CLOEXEC | libc::O_NOFOLLOW | libc::O_NONBLOCK,
        )
    };
    if named_fd < 0 {
        return Err(Fault::Drift);
    }
    let named = unsafe { File::from_raw_fd(named_fd) };
    if identity(&named.metadata().map_err(|_| Fault::Drift)?) != initial {
        return Err(Fault::Drift);
    }
    Ok(first)
}
fn read_bounded(file: &mut File) -> Result<Vec<u8>, Fault> {
    let mut bytes = Vec::new();
    file.take(MAX_CONFIG_BYTES as u64 + 1)
        .read_to_end(&mut bytes)
        .map_err(|_| Fault::Read)?;
    if bytes.len() > MAX_CONFIG_BYTES {
        return Err(Fault::Bounds);
    }
    Ok(bytes)
}
fn identity(m: &Metadata) -> (u64, u64, u32, u32, u32, u64, u64, i64, i64, i64, i64) {
    (
        m.dev(),
        m.ino(),
        m.mode(),
        m.uid(),
        m.gid(),
        m.nlink(),
        m.len(),
        m.mtime(),
        m.mtime_nsec(),
        m.ctime(),
        m.ctime_nsec(),
    )
}
pub(crate) fn private_parent(path: &Path) -> Result<(File, CString), Fault> {
    let raw = path.as_os_str().as_bytes();
    if !path.is_absolute() || raw.len() > 4096 {
        return Err(Fault::Path);
    }
    let mut canonical = std::path::PathBuf::from("/");
    let mut names = Vec::new();
    for component in path.components() {
        match component {
            Component::RootDir => {}
            Component::Normal(name) => {
                canonical.push(name);
                names.push(CString::new(name.as_bytes()).map_err(|_| Fault::Path)?);
            }
            _ => return Err(Fault::Path),
        }
    }
    if canonical.as_os_str().as_bytes() != raw || names.len() < 2 || names.len() > 64 {
        return Err(Fault::Path);
    }
    let leaf = names.pop().ok_or(Fault::Path)?;
    let root = unsafe {
        libc::open(
            c"/".as_ptr(),
            libc::O_RDONLY | libc::O_DIRECTORY | libc::O_CLOEXEC | libc::O_NOFOLLOW,
        )
    };
    if root < 0 {
        return Err(Fault::Parent);
    }
    let mut directory = unsafe { File::from_raw_fd(root) };
    for name in names {
        let fd = unsafe {
            libc::openat(
                directory.as_raw_fd(),
                name.as_ptr(),
                libc::O_RDONLY | libc::O_DIRECTORY | libc::O_CLOEXEC | libc::O_NOFOLLOW,
            )
        };
        if fd < 0 {
            return Err(Fault::Parent);
        }
        directory = unsafe { File::from_raw_fd(fd) };
    }
    let metadata = directory.metadata().map_err(|_| Fault::Parent)?;
    if !metadata.is_dir()
        || metadata.uid() != unsafe { libc::getuid() }
        || metadata.mode() & 0o7777 != 0o700
    {
        return Err(Fault::Parent);
    }
    Ok((directory, leaf))
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;
    use std::fs::{self, OpenOptions};
    use std::io::Write;
    use std::os::unix::fs::{OpenOptionsExt, PermissionsExt, symlink};
    use std::path::PathBuf;
    use std::sync::atomic::{AtomicU64, Ordering};
    use std::time::Duration;
    static NEXT: AtomicU64 = AtomicU64::new(0);
    // Pinned6.0.6 source-derived ASCII example; NOT a native-captured or served event.
    const ACTION: &str = "instance-log-retrieved";
    const SOURCE: &str =
        "/1.0/instances/veranda-probe/backups/migration_veranda-0001.log?project=veranda-owner-a";
    struct Fixture {
        root: PathBuf,
        path: PathBuf,
    }
    impl Fixture {
        fn new(raw: &[u8]) -> Self {
            let root = PathBuf::from(format!(
                "/tmp/veranda-classifier-test-{}-{}",
                std::process::id(),
                NEXT.fetch_add(1, Ordering::Relaxed)
            ));
            fs::create_dir(&root).unwrap();
            fs::set_permissions(&root, fs::Permissions::from_mode(0o700)).unwrap();
            let path = root.join("mapping.json");
            write_private(&path, raw);
            Self { root, path }
        }
    }
    impl Drop for Fixture {
        fn drop(&mut self) {
            fs::remove_dir_all(&self.root).unwrap();
        }
    }
    fn write_private(path: &Path, raw: &[u8]) {
        let mut out = OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o600)
            .open(path)
            .unwrap();
        out.set_permissions(fs::Permissions::from_mode(0o600))
            .unwrap();
        out.write_all(raw).unwrap();
    }
    fn config() -> Vec<u8> {
        serde_json::to_vec(
            &json!({"schema":1,"offers":[{"offer":17,"action":ACTION,"source":SOURCE}]}),
        )
        .unwrap()
    }
    #[test]
    fn cohort_freeze_requires_every_role_and_never_extends_sources() {
        let fixture = Fixture::new(&config());
        let classifier = Classifier::load(&fixture.path).unwrap();
        let expected = vec![
            ExpectedCohort {
                ordinal: 1,
                owner: "private-owner".into(),
                connection: None,
                yard: Some("private-yard".into()),
            },
            ExpectedCohort {
                ordinal: 2,
                owner: "private-remote".into(),
                connection: Some("private-connection".into()),
                yard: None,
            },
        ];
        let mut cohorts =
            Cohorts::new(veranda_lib::observation::Observation::new(), expected).unwrap();
        assert_eq!(cohorts.freeze(&classifier), Err(Fault::Config));
        // Scalar state-machine test only: actual accepted proof admission is
        // separately exercised by the framed Worker/View actor.
        cohorts.bind(1, 7, 1, Role::Fleet);
        cohorts.bind(1, 7, 1, Role::Details);
        assert_eq!(cohorts.freeze(&classifier), Err(Fault::Config));
        cohorts.bind(2, 7, 2, Role::Fleet);
        assert!(cohorts.freeze(&classifier).is_ok());
        assert_eq!(cohorts.bindings().len(), 3);
        let before = cohorts.bindings().to_vec();
        cohorts.bind(1, 7, 1, Role::Fleet);
        assert!(!cohorts.replacement_unknown());
        cohorts.bind(1, 7, 3, Role::Fleet);
        assert!(cohorts.replacement_unknown());
        assert!(!cohorts.complete());
        assert_eq!(cohorts.bindings(), before);
        assert_eq!(cohorts.freeze(&classifier), Err(Fault::AlreadyFrozen));
        let scalar = serde_json::to_string(cohorts.bindings()).unwrap();
        for private in [
            "private-owner",
            "private-yard",
            "private-remote",
            "private-connection",
        ] {
            assert!(!scalar.contains(private));
        }
    }
    #[test]
    fn cohort_targets_are_bounded_private_and_unambiguous() {
        let observer = veranda_lib::observation::Observation::new();
        assert!(matches!(
            Cohorts::new(observer.clone(), vec![]),
            Err(Fault::Bounds)
        ));
        let row = || ExpectedCohort {
            ordinal: 1,
            owner: "owner".into(),
            connection: None,
            yard: None,
        };
        assert!(matches!(
            Cohorts::new(observer.clone(), vec![row(), row()]),
            Err(Fault::Config)
        ));
        let mut other = row();
        other.ordinal = 2;
        assert!(matches!(
            Cohorts::new(observer.clone(), vec![row(), other]),
            Err(Fault::Config)
        ));
        let mut invalid = row();
        invalid.owner = "private value".into();
        assert!(matches!(
            Cohorts::new(observer, vec![invalid]),
            Err(Fault::Config)
        ));
    }
    #[test]
    fn runtime_config_is_strict_and_stopped_owner_cannot_publish() {
        let fixture = Fixture::new(b"{}");
        let trace = fixture.root.join("trace");
        let ready = fixture.root.join("ready");
        let valid = serde_json::json!({"schema":2,"offers":[{"offer":1,"action":ACTION,"source":SOURCE}],"cohorts":[{"ordinal":1,"owner":"fixture-owner","connection":null,"yard":"default"}],"ready":ready});
        for fault in [
            "none",
            "extra",
            "duplicate-target",
            "same-path",
            "bad-schema",
            "empty-target",
        ] {
            let mut value = valid.clone();
            if fault == "extra" {
                value["private_extra"] = serde_json::json!("secret");
            }
            if fault == "duplicate-target" {
                value["cohorts"]
                    .as_array_mut()
                    .unwrap()
                    .push(valid["cohorts"][0].clone());
            }
            if fault == "same-path" {
                value["ready"] = serde_json::json!(trace);
            }
            if fault == "bad-schema" {
                value["schema"] = serde_json::json!(1);
            }
            if fault == "empty-target" {
                value["cohorts"] = serde_json::json!([]);
            }
            fs::write(&fixture.path, serde_json::to_vec(&value).unwrap()).unwrap();
            let result = RuntimePlan::load(&fixture.path, &trace);
            if fault == "none" {
                let (observation, run) = result.unwrap().start().unwrap();
                assert_eq!(observation.counts().session_sources, 0);
                let result = run.finish();
                assert_eq!(result.writer, WriterOutcome::Stopped);
                assert!(!result.cohorts_complete);
                assert!(!ready.exists());
            } else {
                assert!(result.is_err(), "{fault}");
            }
        }
    }
    fn frame(action: &str, source: &str) -> Vec<u8> {
        serde_json::to_vec(&json!({"version":1,"type":"event","event":"incus.lifecycle","sequence":3,"revision":3,"operationId":"","data":{"data":{"action":action,"source":source,"context":{"private":"never-output"}},"ignored":"extra"}})).unwrap()
    }
    fn event<'a>(raw: &'a [u8], at: Instant) -> ValidatedEvent<'a> {
        ValidatedEvent {
            event: EVENT_KIND,
            operation_id: "",
            sequence: 3,
            revision: 3,
            received: at,
            frame: raw,
        }
    }
    fn loaded() -> (Fixture, Classifier, Instant) {
        let fixture = Fixture::new(&config());
        let classifier = Classifier::load(&fixture.path).unwrap();
        let at = classifier.freeze().unwrap();
        (fixture, classifier, at)
    }
    #[test]
    fn exact_source_derived_nested_shape_and_default_project_are_exact_only() {
        let (_fixture, classifier, at) = loaded();
        let at = at + Duration::from_nanos(1);
        assert_eq!(
            classifier.classify(event(&frame(ACTION, SOURCE), at)),
            EventTag::Matched(17)
        );
        for (action, source) in [
            ("instance-log-retrieved-extra", SOURCE),
            ("instance-started", SOURCE),
            (
                ACTION,
                "/1.0/instances/veranda-probe/logs/migration_veranda-0001.log?project=veranda-owner-a",
            ),
            (
                ACTION,
                "/1.0/instances/veranda-probe/backups/migration_veranda-0001.log",
            ),
            (
                ACTION,
                "/1.0/instances/veranda-probe/backups/migration_veranda-0001.log?project=veranda-owner-A",
            ),
            (
                ACTION,
                "/1.0/instances/veranda-probe/backups/migration_veranda%2D0001.log?project=veranda-owner-a",
            ),
        ] {
            assert_eq!(
                classifier.classify(event(&frame(action, source), at)),
                EventTag::Other
            );
        }
        let default = SOURCE.split('?').next().unwrap();
        let cfg = serde_json::to_vec(
            &json!({"schema":1,"offers":[{"offer":18,"action":ACTION,"source":default}]}),
        )
        .unwrap();
        let fixture = Fixture::new(&cfg);
        let classifier = Classifier::load(&fixture.path).unwrap();
        let at = classifier.freeze().unwrap() + Duration::from_nanos(1);
        assert_eq!(
            classifier.classify(event(&frame(ACTION, default), at)),
            EventTag::Matched(18)
        );
        assert_eq!(
            classifier.classify(event(
                &frame(ACTION, &format!("{default}?project=default")),
                at
            )),
            EventTag::Other
        );
    }
    #[test]
    fn unrelated_has_no_parse_and_expected_bad_shapes_are_unclassifiable() {
        let (_fixture, classifier, at) = loaded();
        let at = at + Duration::from_nanos(1);
        let mut unrelated = event(b"\xffnot-json", at);
        unrelated.event = "operation.progress";
        assert_eq!(classifier.classify(unrelated), EventTag::Other);
        let good = frame(ACTION, SOURCE);
        let value: serde_json::Value = serde_json::from_slice(&good).unwrap();
        let mut cases = vec![b"not-json".to_vec(), vec![b'x'; MAX_EVENT_BYTES + 1]];
        for pointer in [
            "/data",
            "/data/data",
            "/data/data/action",
            "/data/data/source",
        ] {
            for bad in [serde_json::Value::Null, json!(17), json!([]), json!({})] {
                let mut wire = value.clone();
                *wire.pointer_mut(pointer).unwrap() = bad;
                cases.push(serde_json::to_vec(&wire).unwrap());
            }
        }
        for key in ["action", "source"] {
            let mut wire = value.clone();
            wire["data"]["data"].as_object_mut().unwrap().remove(key);
            cases.push(serde_json::to_vec(&wire).unwrap());
        }
        for pointer in [
            "/version",
            "/type",
            "/event",
            "/sequence",
            "/revision",
            "/operationId",
        ] {
            let mut wire = value.clone();
            *wire.pointer_mut(pointer).unwrap() = json!("wrong");
            cases.push(serde_json::to_vec(&wire).unwrap());
        }
        for wire in cases {
            assert_eq!(
                classifier.classify(event(&wire, at)),
                EventTag::Unclassifiable
            );
        }
    }
    #[test]
    fn duplicate_relevant_keys_are_rejected_at_every_nested_level() {
        let (_fixture, classifier, at) = loaded();
        let at = at + Duration::from_nanos(1);
        let good = String::from_utf8(frame(ACTION, SOURCE)).unwrap();
        for (needle, replacement) in [
            ("\"action\":", "\"action\":\"other\",\"action\":"),
            ("\"source\":", "\"source\":\"/other\",\"source\":"),
            ("\"data\":{", "\"data\":{},\"data\":{"),
        ] {
            let raw = good.replacen(needle, replacement, 1);
            assert_eq!(
                classifier.classify(event(raw.as_bytes(), at)),
                EventTag::Unclassifiable
            );
        }
        let raw = good.replace("\"data\":{\"action\"", "\"data\":{},\"data\":{\"action\"");
        assert_ne!(raw, good);
        assert_eq!(
            classifier.classify(event(raw.as_bytes(), at)),
            EventTag::Unclassifiable
        );
    }
    #[test]
    fn strict_config_duplicates_shape_bounds_and_scalar_error_privacy() {
        let good = String::from_utf8(config()).unwrap();
        let row = json!({"offer":17,"action":ACTION,"source":SOURCE});
        let mut cases = vec![
            "not-json".as_bytes().to_vec(),
            good.replacen("\"schema\":1", "\"schema\":1,\"schema\":1", 1)
                .into_bytes(),
            good.replacen("\"offer\":17", "\"offer\":17,\"offer\":18", 1)
                .into_bytes(),
            serde_json::to_vec(&json!({"schema":1,"offers":[row.clone(),row]})).unwrap(),
            serde_json::to_vec(&json!({"schema":1,"offers":[],"unknown":"PRIVATE"})).unwrap(),
            vec![b'x'; MAX_CONFIG_BYTES + 1],
        ];
        for value in [
            json!({"schema":2,"offers":[]}),
            json!({"schema":1,"offers":[]}),
            json!({"schema":1,"offers":[{"offer":0,"action":ACTION,"source":SOURCE}]}),
            json!({"schema":1,"offers":[{"offer":17,"action":ACTION,"source":SOURCE},{"offer":18,"action":ACTION,"source":SOURCE}]}),
            json!({"schema":1,"offers":[{"offer":17,"action":17,"source":SOURCE}]}),
            json!({"schema":1,"offers":[{"offer":17,"action":ACTION,"source":"/private\nvalue"}]}),
            json!({"schema":1,"offers":[{"offer":17,"action":ACTION,"source":"relative"}]}),
        ] {
            cases.push(serde_json::to_vec(&value).unwrap());
        }
        let rows: Vec<_> = (1..=MAX_OFFERS + 1)
            .map(|id| json!({"offer":id,"action":ACTION,"source":format!("/bounded/{id}")}))
            .collect();
        cases.push(serde_json::to_vec(&json!({"schema":1,"offers":rows})).unwrap());
        for raw in cases {
            let fixture = Fixture::new(&raw);
            let error = Classifier::load(&fixture.path).err().unwrap();
            let scalar = format!("{error:?}");
            assert!(
                !scalar.contains("PRIVATE") && !scalar.contains("private") && !scalar.contains("/")
            );
        }
    }
    #[test]
    fn private_fd_modes_links_canonical_paths_and_parent_are_guarded() {
        let fixture = Fixture::new(&config());
        fs::set_permissions(&fixture.path, fs::Permissions::from_mode(0o644)).unwrap();
        assert!(matches!(Classifier::load(&fixture.path), Err(Fault::File)));
        fs::set_permissions(&fixture.path, fs::Permissions::from_mode(0o600)).unwrap();
        let link = fixture.root.join("linked");
        fs::hard_link(&fixture.path, &link).unwrap();
        assert!(matches!(Classifier::load(&fixture.path), Err(Fault::File)));
        fs::remove_file(link).unwrap();
        let link = fixture.root.join("symbolic");
        symlink(&fixture.path, &link).unwrap();
        assert!(Classifier::load(&link).is_err());
        let directory_link = fixture.root.join("parent-link");
        symlink(&fixture.root, &directory_link).unwrap();
        assert!(Classifier::load(&directory_link.join("mapping.json")).is_err());
        for path in [
            PathBuf::from("mapping.json"),
            PathBuf::from(format!("{}/./mapping.json", fixture.root.display())),
            fixture.root.join("../mapping.json"),
        ] {
            assert!(matches!(Classifier::load(&path), Err(Fault::Path)));
        }
        fs::set_permissions(&fixture.root, fs::Permissions::from_mode(0o755)).unwrap();
        assert!(matches!(
            Classifier::load(&fixture.path),
            Err(Fault::Parent)
        ));
        fs::set_permissions(&fixture.root, fs::Permissions::from_mode(0o700)).unwrap();
    }
    #[test]
    fn stable_fd_rejects_same_byte_inode_mode_or_content_drift() {
        for kind in ["inode", "mode", "bytes"] {
            let fixture = Fixture::new(&config());
            let result = Classifier::load_checked(&fixture.path, || match kind {
                "inode" => {
                    let replacement = fixture.root.join("new");
                    write_private(&replacement, &config());
                    fs::rename(replacement, &fixture.path).unwrap();
                }
                "mode" => {
                    fs::set_permissions(&fixture.path, fs::Permissions::from_mode(0o644)).unwrap()
                }
                _ => {
                    let mut file = OpenOptions::new()
                        .write(true)
                        .truncate(true)
                        .open(&fixture.path)
                        .unwrap();
                    file.write_all(b"PRIVATE changed bytes").unwrap();
                }
            });
            assert!(matches!(result, Err(Fault::Drift)));
        }
    }
    #[test]
    fn original_pre_freeze_receipt_cannot_be_promoted_by_delayed_callback() {
        let fixture = Fixture::new(&config());
        let classifier = Arc::new(Classifier::load(&fixture.path).unwrap());
        let before = Instant::now();
        assert_eq!(
            classifier.classify(event(&frame(ACTION, SOURCE), before)),
            EventTag::Unclassifiable
        );
        let (entered, entry) = std::sync::mpsc::channel();
        let (release, released) = std::sync::mpsc::channel();
        let worker = std::thread::spawn({
            let classifier = classifier.clone();
            move || {
                let original = Instant::now();
                entered.send(original).unwrap();
                released.recv_timeout(Duration::from_secs(3)).unwrap();
                classifier.classify(event(&frame(ACTION, SOURCE), original))
            }
        });
        let original = entry.recv_timeout(Duration::from_secs(3));
        let frozen = classifier.freeze();
        let _ = release.send(());
        let tag = worker.join();
        let frozen = frozen.unwrap();
        assert!(original.unwrap() <= frozen);
        assert_eq!(tag.unwrap(), EventTag::Unclassifiable);
        assert_eq!(
            classifier.classify(event(&frame(ACTION, SOURCE), frozen)),
            EventTag::Unclassifiable
        );
        assert_eq!(
            classifier.classify(event(
                &frame(ACTION, SOURCE),
                frozen + Duration::from_nanos(1)
            )),
            EventTag::Matched(17)
        );
        assert_eq!(classifier.freeze(), Err(Fault::AlreadyFrozen));
        let tagger = classifier.clone().tagger();
        assert_eq!(
            tagger(event(
                &frame(ACTION, SOURCE),
                frozen + Duration::from_nanos(1)
            ))
            .unwrap(),
            EventTag::Matched(17)
        );
        // The immutable loaded map is not silently replaced by later file changes.
        fs::remove_file(&fixture.path).unwrap();
        assert_eq!(
            classifier.classify(event(
                &frame(ACTION, SOURCE),
                frozen + Duration::from_nanos(1)
            )),
            EventTag::Matched(17)
        );
    }
}
