//! The native boundary keeps session handles, exact plans, and trust capabilities out of JS.
use crate::connections::{
    ConnectionStore, ConnectionSummary, ConsentedConnection, NavigationSelection, RemoveAssessment,
    TrustAssessment,
};
use crate::local_fleet::{LocalFleetSnapshot, NativeError, OwnerInventory, safe_id, safe_name};
use crate::observation::{
    Effect, Observation, Observed, Retirement, Source, SourceKind, UpdateObservation,
};
use crate::sessions::{self, Descriptor, Launch};
use crate::transport::{self, Processes, RpcClient};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use std::collections::{HashMap, HashSet};
use std::path::PathBuf;
use std::process::Command;
use std::sync::{
    Arc, Mutex, Weak,
    atomic::{AtomicBool, AtomicU64, AtomicUsize, Ordering},
    mpsc,
};
use std::thread;
use std::time::{Duration, Instant};

const QUERY_TIMEOUT: Duration = Duration::from_secs(5);
const MAX_SESSIONS: usize = 8;
const MAX_PLANS: usize = 16;

pub fn product_version() -> &'static str {
    option_env!("VERANDA_PRODUCT_VERSION").unwrap_or(if cfg!(debug_assertions) {
        concat!(env!("CARGO_PKG_VERSION"), "-dev")
    } else {
        env!("CARGO_PKG_VERSION")
    })
}

#[derive(Clone, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct VerandaEvent {
    pub connection_id: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub yard: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub operation_id: Option<String>,
    pub state: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub message: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub snapshot: Option<LocalFleetSnapshot>,
    #[serde(skip)]
    pub observation: Option<UpdateObservation>,
}
pub type Emit = Arc<dyn Fn(VerandaEvent) + Send + Sync>;

#[derive(Clone, Hash, PartialEq, Eq)]
struct SessionKey {
    connection: Option<String>,
    yard: Option<String>,
}
struct Session {
    rpc: RpcClient,
    observation: Option<Source>,
    _pin: Option<ConsentedConnection>,
    key: SessionKey,
    host_id: String,
    current_yard: String,
    initial_snapshot: Mutex<Option<(u64, LocalFleetSnapshot)>>,
    dirty: Arc<AtomicBool>,
    epoch: Arc<AtomicU64>,
    retired: Arc<AtomicBool>,
    last_used: Mutex<Instant>,
    refreshing: AtomicBool,
    retry: Mutex<Option<Instant>>,
}
impl Drop for Session {
    fn drop(&mut self) {
        // Dropping a cached session closes its RPC child deliberately. The
        // transport's cleanup notification is not a loss of a live session.
        self.retired.store(true, Ordering::Release);
        if let Some(source) = &self.observation {
            source.retire_all(Retirement::SessionClosed);
        }
    }
}
struct HeldPlan {
    session: Arc<Session>,
    plan: OperationPlan,
    expires: Instant,
    epoch: u64,
}
struct Running {
    session: Arc<Session>,
}

pub struct Client {
    store: Mutex<ConnectionStore>,
    sessions: Mutex<HashMap<SessionKey, Arc<Session>>>,
    read_scope: Mutex<Option<SessionKey>>,
    opening: Mutex<HashSet<SessionKey>>,
    trust_epoch: AtomicU64,
    plans: Mutex<HashMap<String, HeldPlan>>,
    running: Mutex<HashMap<String, Running>>,
    emit: Emit,
    wake: mpsc::SyncSender<()>,
    stopped: AtomicBool,
    launch_count: Arc<AtomicUsize>,
    processes: Processes,
    observation: Option<Observation>,
}

/// A single provisional trust connection. Cancellation and registration share
/// one publication lock: once cancellation wins, no metadata can be registered.
/// Call cancel off the UI thread because it reaps the provisional SSH child.
#[derive(Default)]
pub struct ConnectionAttempt {
    state: Mutex<AttemptState>,
    processes: Processes,
}

#[derive(Default)]
enum AttemptState {
    #[default]
    Active,
    Cancelled,
    Finished,
}

impl ConnectionAttempt {
    pub fn cancel(&self) {
        if let Ok(mut state) = self.state.lock() {
            if matches!(*state, AttemptState::Active) {
                *state = AttemptState::Cancelled;
                self.processes.stop_accepting();
            }
        }
        self.processes.shutdown();
    }

    pub fn is_cancelled(&self) -> bool {
        self.state
            .lock()
            .map_or(true, |state| matches!(*state, AttemptState::Cancelled))
    }

    fn publish<T>(
        &self,
        change: impl FnOnce() -> Result<T, NativeError>,
    ) -> Result<T, NativeError> {
        let mut state = self.state.lock().map_err(|_| unavailable())?;
        if !matches!(*state, AttemptState::Active) {
            return Err(connection_cancelled());
        }
        let result = change();
        *state = AttemptState::Finished;
        result
    }
}

fn connection_cancelled() -> NativeError {
    NativeError::new(
        "cancelled",
        "The connection was cancelled. No host was registered.",
    )
}

impl Client {
    pub fn new(root: PathBuf, emit: Emit) -> Arc<Self> {
        Self::with_observation(root, emit, None)
    }
    pub fn observed(root: PathBuf, emit: Emit, observation: Observation) -> Arc<Self> {
        Self::with_observation(root, emit, Some(observation))
    }
    fn with_observation(root: PathBuf, emit: Emit, observation: Option<Observation>) -> Arc<Self> {
        let (wake, receiver) = mpsc::sync_channel(1);
        let client = Arc::new(Self {
            store: Mutex::new(ConnectionStore::new(root)),
            sessions: Mutex::new(HashMap::new()),
            read_scope: Mutex::new(None),
            opening: Mutex::new(HashSet::new()),
            trust_epoch: AtomicU64::new(0),
            plans: Mutex::new(HashMap::new()),
            running: Mutex::new(HashMap::new()),
            emit,
            wake,
            stopped: AtomicBool::new(false),
            launch_count: Arc::new(AtomicUsize::new(0)),
            processes: Processes::default(),
            observation,
        });
        let weak = Arc::downgrade(&client);
        thread::spawn(move || monitor(weak, receiver));
        client
    }

    pub fn connections(&self) -> Result<Vec<ConnectionSummary>, NativeError> {
        self.store.lock().map_err(|_| unavailable())?.list()
    }
    pub fn desktop_document(&self) -> Result<Vec<u8>, NativeError> {
        self.store
            .lock()
            .map_err(|_| unavailable())?
            .desktop_document()
    }
    pub fn save_desktop_document(&self, baseline: &[u8], bytes: &[u8]) -> Result<(), NativeError> {
        self.store
            .lock()
            .map_err(|_| unavailable())?
            .save_desktop_document(baseline, bytes)
    }
    pub fn navigation_selection(&self) -> Result<Option<NavigationSelection>, NativeError> {
        self.store
            .lock()
            .map_err(|_| unavailable())?
            .navigation_selection()
    }
    pub fn save_navigation_selection(
        &self,
        selection: &NavigationSelection,
    ) -> Result<(), NativeError> {
        self.store
            .lock()
            .map_err(|_| unavailable())?
            .save_navigation_selection(selection)
    }
    pub fn assess(
        &self,
        destination: &str,
        repair: Option<&str>,
    ) -> Result<Assessment, NativeError> {
        self.store
            .lock()
            .map_err(|_| unavailable())?
            .prepare_trust_with_usage(
                destination,
                repair,
                self.observation
                    .as_ref()
                    .and_then(Observation::child_usage_context)
                    .as_ref(),
            )
            .map(Assessment::from)
    }
    pub fn repair(&self, id: &str) -> Result<Assessment, NativeError> {
        let destination = self
            .connections()?
            .into_iter()
            .find(|record| record.id == id)
            .ok_or_else(unavailable)?
            .destination;
        self.assess(&destination, Some(id))
    }
    pub fn connect(
        &self,
        token: &str,
        confirmed: bool,
        fingerprint: &str,
    ) -> Result<ConnectionSummary, NativeError> {
        self.connect_attempt(token, confirmed, fingerprint, &ConnectionAttempt::default())
    }

    pub fn connect_attempt(
        &self,
        token: &str,
        confirmed: bool,
        fingerprint: &str,
        attempt: &ConnectionAttempt,
    ) -> Result<ConnectionSummary, NativeError> {
        if attempt.is_cancelled() {
            self.cancel_assessment(token)?;
            return Err(connection_cancelled());
        }
        if self.stopped.load(Ordering::Acquire) {
            return Err(unavailable());
        }
        let pin = self
            .store
            .lock()
            .map_err(|_| unavailable())?
            .consented_ssh_command(token, confirmed, fingerprint)?;
        let usage = self
            .observation
            .as_ref()
            .and_then(Observation::child_usage_context);
        crate::ssh::require_agent(usage.as_ref())?;
        let rpc = RpcClient::open_tracked_attempt_usage(
            pin.command(None)?,
            product_version(),
            Arc::new(|_| {}),
            Some(&self.processes),
            Some(&attempt.processes),
            usage,
        )?;
        let inventory: OwnerInventory =
            decode(rpc.call("owner.inventory", "", json!({}), QUERY_TIMEOUT)?)?;
        let context: CurrentContext =
            decode(rpc.call("context.get", "", json!({}), QUERY_TIMEOUT)?)?;
        context.validate(None)?;
        let snapshot = inventory.into_snapshot(rpc.engine_version.clone(), context.yard_name)?;
        rpc.close();
        if self.stopped.load(Ordering::Acquire) {
            return Err(unavailable());
        }
        attempt.publish(|| {
            self.commit_trust(move |store| {
                let record = store.finalize_registration(pin, &snapshot.owner.id)?;
                Ok((record.clone(), record.id))
            })
        })
    }
    pub fn assess_removal(&self, id: &str) -> Result<Removal, NativeError> {
        self.store
            .lock()
            .map_err(|_| unavailable())?
            .prepare_remove(id)
            .map(Removal::from)
    }
    pub fn cancel_assessment(&self, token: &str) -> Result<(), NativeError> {
        if !transport::identity(token) {
            return Err(transport::invalid());
        }
        self.store.lock().map_err(|_| unavailable())?.cancel(token);
        Ok(())
    }
    pub fn remove(&self, token: &str, confirmed: bool) -> Result<(), NativeError> {
        self.commit_trust(|store| {
            let removed = store.remove(token, confirmed)?;
            Ok(((), removed.id))
        })
    }

    fn commit_trust<T>(
        &self,
        change: impl FnOnce(&mut ConnectionStore) -> Result<(T, String), NativeError>,
    ) -> Result<T, NativeError> {
        // Lock order is plans -> sessions -> store. Holding the publication lock
        // across commit and invalidation prevents an old opening from entering
        // the cache between a trust change and its generation increment.
        let mut plans = self.plans.lock().map_err(|_| unavailable())?;
        let mut sessions = self.sessions.lock().map_err(|_| unavailable())?;
        if self.stopped.load(Ordering::Acquire) {
            return Err(unavailable());
        }
        let mut store = self.store.lock().map_err(|_| unavailable())?;
        let change = change(&mut store);
        self.trust_epoch.fetch_add(1, Ordering::AcqRel);
        // A rename can commit before a durability or verification check fails.
        // An ambiguous failed commit therefore invalidates remote trust too.
        let connection = change.as_ref().ok().map(|(_, id)| id.as_str());
        let affected = |id: Option<&str>| {
            id.is_some() && connection.is_none_or(|connection| id == Some(connection))
        };
        plans.retain(|_, held| !affected(held.session.key.connection.as_deref()));
        sessions.retain(|key, session| {
            if affected(key.connection.as_deref()) {
                session.rpc.close();
                false
            } else {
                true
            }
        });
        change.map(|(result, _)| result)
    }

    fn open_session(&self, key: &SessionKey) -> Result<Arc<Session>, NativeError> {
        validate_key(key)?;
        let pin = match key.connection.as_deref() {
            Some(id) => Some(
                self.store
                    .lock()
                    .map_err(|_| unavailable())?
                    .registered_ssh_command(id)?,
            ),
            None if cfg!(target_os = "linux") => None,
            None => {
                return Err(NativeError::new(
                    "remote_only",
                    "This platform supports remote owner hosts. Add an SSH connection.",
                ));
            }
        };
        let command = if let Some(pin) = &pin {
            pin.command(key.yard.as_deref())?
        } else {
            let mut command = Command::new("yard");
            if let Some(yard) = &key.yard {
                command.args(["-Y", yard]);
            }
            command.args(["rpc", "--stdio"]);
            command
        };
        if pin.is_some() {
            crate::ssh::require_agent(
                self.observation
                    .as_ref()
                    .and_then(Observation::child_usage_context)
                    .as_ref(),
            )?;
        }
        self.launch_session(key, pin, command)
    }

    fn launch_session(
        &self,
        key: &SessionKey,
        pin: Option<ConsentedConnection>,
        command: Command,
    ) -> Result<Arc<Session>, NativeError> {
        let observation = self.observation.as_ref().map(|observer| {
            observer.source(if key.connection.is_some() {
                SourceKind::Remote
            } else {
                SourceKind::Local
            })
        });
        let callback_observation = observation.clone();
        let dirty = Arc::new(AtomicBool::new(false));
        let epoch = Arc::new(AtomicU64::new(0));
        let retired = Arc::new(AtomicBool::new(false));
        let callback_retired = retired.clone();
        let event_epoch = epoch.clone();
        let callback_dirty = dirty.clone();
        let emit = self.emit.clone();
        let event_key = key.clone();
        let wake = self.wake.clone();
        let last = Arc::new(Mutex::new(Instant::now() - Duration::from_secs(1)));
        let rpc = RpcClient::open_observed(
            command,
            product_version(),
            Arc::new(move |event| {
                if callback_retired.load(Ordering::Acquire) {
                    if let (Some(source), Some(ticket)) = (&callback_observation, event.receipt) {
                        source.ignore(ticket);
                    }
                    return;
                }
                let disconnected = event.event == "transport.disconnected";
                if disconnected || event.stale {
                    event_epoch.fetch_add(1, Ordering::AcqRel);
                }
                // RPC request lifecycle events also accompany read queries. They must
                // not trigger another read, otherwise inventory refresh polls itself.
                let relevant = disconnected
                    || event.stale
                    || event.operation_id.starts_with("operation-")
                    || event.event.starts_with("incus.");
                if !relevant {
                    if let (Some(source), Some(ticket)) = (&callback_observation, event.receipt) {
                        source.ignore(ticket);
                    }
                    return;
                }
                if let (Some(source), Some(ticket)) = (&callback_observation, &event.receipt) {
                    // A gap invalidates product state; it does not change the
                    // measured effect of this particular receipt.
                    let effect = if event.event.starts_with("incus.") {
                        Some(Effect::Inventory)
                    } else if event.operation_id.starts_with("operation-") {
                        Some(Effect::Operation)
                    } else {
                        None
                    };
                    if let Some(effect) = effect {
                        source.eligible(ticket, effect);
                    } else {
                        source.ignore(ticket.clone());
                    }
                }
                callback_dirty.store(true, Ordering::Release);
                let _ = wake.try_send(());
                let now = Instant::now();
                if let Ok(mut previous) = last.lock() {
                    if disconnected || now.duration_since(*previous) >= Duration::from_millis(100) {
                        *previous = now;
                        emit(VerandaEvent {
                            connection_id: event_key.connection.clone(),
                            yard: event_key.yard.clone(),
                            operation_id: (!event.operation_id.is_empty())
                                .then_some(event.operation_id),
                            state: if disconnected {
                                "disconnected"
                            } else if event.stale {
                                "stale"
                            } else {
                                "running"
                            }
                            .into(),
                            message: Some(if disconnected {
                                "Connection lost; an interrupted operation has no confirmed final result.".into()
                            } else {
                                event.event
                            }),
                            snapshot: None,
                            observation: callback_observation
                                .as_ref()
                                .map(|source| source.update(None)),
                        });
                    }
                }
            }),
            Some(&self.processes),
            observation.clone(),
        )?;
        let context: CurrentContext =
            decode(rpc.call("context.get", "", json!({}), QUERY_TIMEOUT)?)?;
        context.validate(key.yard.as_deref())?;
        let inventory_epoch = epoch.load(Ordering::Acquire);
        let inventory: OwnerInventory =
            decode(rpc.call("owner.inventory", "", json!({}), QUERY_TIMEOUT)?)?;
        let snapshot =
            inventory.into_snapshot(rpc.engine_version.clone(), context.yard_name.clone())?;
        if let Some(id) = &key.connection {
            let expected = self
                .connections()?
                .into_iter()
                .find(|record| &record.id == id)
                .ok_or_else(unavailable)?
                .host_id;
            if snapshot.owner.id != expected {
                return Err(NativeError::new(
                    "host_identity_changed",
                    "The owner HostID changed. Review and repair this connection before continuing.",
                ));
            }
        }
        let session = Arc::new(Session {
            rpc,
            observation,
            _pin: pin,
            key: key.clone(),
            host_id: snapshot.owner.id.clone(),
            initial_snapshot: Mutex::new(Some((inventory_epoch, snapshot))),
            current_yard: context.yard_name,
            dirty,
            epoch,
            retired,
            last_used: Mutex::new(Instant::now()),
            refreshing: AtomicBool::new(false),
            retry: Mutex::new(None),
        });
        session.rpc.subscribe(&format!("events-{}", random_id()?))?;
        Ok(session)
    }

    fn session(&self, key: SessionKey) -> Result<Arc<Session>, NativeError> {
        self.session_with_open(key, |key| self.open_session(key))
    }

    // Keep sessions -> store ordering. A different app process can change saved
    // trust without advancing this client's in-memory trust epoch.
    fn validate_cached_session(
        &self,
        sessions: &mut HashMap<SessionKey, Arc<Session>>,
        session: &Arc<Session>,
    ) -> Result<(), NativeError> {
        let result = match &session._pin {
            Some(pin) => self
                .store
                .lock()
                .map_err(|_| unavailable())?
                .validate_registered_session(pin),
            None if session.key.connection.is_none() => Ok(()),
            None => Err(unavailable()),
        };
        if result.is_err() {
            session.epoch.fetch_add(1, Ordering::AcqRel);
            session.rpc.close();
            sessions.retain(|_, cached| !Arc::ptr_eq(cached, session));
        }
        result
    }

    fn validate_session(&self, session: &Arc<Session>) -> Result<(), NativeError> {
        let mut sessions = self.sessions.lock().map_err(|_| unavailable())?;
        self.validate_cached_session(&mut sessions, session)
    }

    fn invalidate_session(&self, session: &Arc<Session>) {
        session.epoch.fetch_add(1, Ordering::AcqRel);
        session.rpc.close();
        if let Ok(mut sessions) = self.sessions.lock() {
            sessions.retain(|_, cached| !Arc::ptr_eq(cached, session));
        }
    }

    fn snapshot(&self, session: &Arc<Session>) -> Result<LocalFleetSnapshot, NativeError> {
        Ok(self.snapshot_observed(session)?.value)
    }
    fn snapshot_observed(
        &self,
        session: &Arc<Session>,
    ) -> Result<Observed<LocalFleetSnapshot>, NativeError> {
        self.validate_session(session)?;
        let result = snapshot_observed(session)?;
        self.validate_session(session)?;
        Ok(result)
    }

    fn initial_snapshot(&self, session: &Arc<Session>) -> Option<LocalFleetSnapshot> {
        self.validate_session(session).ok()?;
        let epoch = session.epoch.load(Ordering::Acquire);
        let snapshot = session
            .initial_snapshot
            .lock()
            .ok()?
            .as_ref()
            .filter(|(captured, _)| *captured == epoch)?
            .1
            .clone();
        self.validate_session(session).ok()?;
        if session.rpc.is_closed()
            || session.retired.load(Ordering::Acquire)
            || session.epoch.load(Ordering::Acquire) != epoch
            || snapshot.owner.id != session.host_id
        {
            return None;
        }
        Some(decorate_snapshot(session, snapshot))
    }

    fn session_with_open(
        &self,
        key: SessionKey,
        open: impl FnOnce(&SessionKey) -> Result<Arc<Session>, NativeError>,
    ) -> Result<Arc<Session>, NativeError> {
        if self.stopped.load(Ordering::Acquire) {
            return Err(unavailable());
        }
        validate_key(&key)?;
        let trust_epoch = {
            let mut sessions = self.sessions.lock().map_err(|_| unavailable())?;
            // Fleet and current-yard details share one immutable owner context.
            if let Some(yard) = &key.yard {
                let owner_key = SessionKey {
                    connection: key.connection.clone(),
                    yard: None,
                };
                if let Some(session) = sessions
                    .get(&owner_key)
                    .filter(|session| !session.rpc.is_closed() && session.current_yard == *yard)
                    .cloned()
                {
                    self.validate_cached_session(&mut sessions, &session)?;
                    *session.last_used.lock().map_err(|_| unavailable())? = Instant::now();
                    return Ok(session.clone());
                }
            }
            if let Some(session) = sessions
                .get(&key)
                .filter(|session| !session.rpc.is_closed())
                .cloned()
            {
                self.validate_cached_session(&mut sessions, &session)?;
                *session.last_used.lock().map_err(|_| unavailable())? = Instant::now();
                return Ok(session.clone());
            }
            if sessions.len() >= MAX_SESSIONS && !sessions.contains_key(&key) {
                let oldest = sessions
                    .iter()
                    .filter(|(_, session)| Arc::strong_count(session) == 1)
                    .min_by_key(|(_, session)| {
                        session
                            .last_used
                            .lock()
                            .map(|time| *time)
                            .unwrap_or_else(|_| Instant::now())
                    })
                    .map(|(key, _)| key.clone())
                    .ok_or_else(capacity)?;
                sessions.remove(&oldest);
            }
            self.trust_epoch.load(Ordering::Acquire)
        };
        {
            let mut opening = self.opening.lock().map_err(|_| unavailable())?;
            if opening.len() >= MAX_SESSIONS || !opening.insert(key.clone()) {
                return Err(capacity());
            }
        }
        let result = open(&key);
        self.opening.lock().map_err(|_| unavailable())?.remove(&key);
        let session = result?;
        if self.stopped.load(Ordering::Acquire) {
            session.rpc.close();
            return Err(unavailable());
        }
        let mut sessions = self.sessions.lock().map_err(|_| unavailable())?;
        if self.stopped.load(Ordering::Acquire)
            || trust_epoch != self.trust_epoch.load(Ordering::Acquire)
        {
            session.rpc.close();
            return Err(unavailable());
        }
        if sessions.len() >= MAX_SESSIONS && !sessions.contains_key(&key) {
            session.rpc.close();
            return Err(capacity());
        }
        self.validate_cached_session(&mut sessions, &session)?;
        sessions.insert(key, session.clone());
        drop(sessions);
        // Reopening an evicted scope restores its health even without a later
        // Incus event. Preserve the initial read for the requesting caller.
        if let Some(snapshot) = self.initial_snapshot(&session) {
            (self.emit)(state_event(&session, "connected", Some(snapshot)));
        }
        Ok(session)
    }

    /// Opt into retaining only the selected read scope and owner inventory sessions.
    /// Plans, running operations and in-flight reads retain their own session handles.
    pub fn retain_read_scope(
        &self,
        connection: Option<String>,
        yard: Option<String>,
    ) -> Result<(), NativeError> {
        if self.stopped.load(Ordering::Acquire) {
            return Err(unavailable());
        }
        let key = SessionKey { connection, yard };
        validate_key(&key)?;
        {
            // Use the cache lock for both publication and retention decisions.
            let _sessions = self.sessions.lock().map_err(|_| unavailable())?;
            *self.read_scope.lock().map_err(|_| unavailable())? = Some(key);
        }
        self.trim_read_sessions()?;
        let _ = self.wake.try_send(());
        Ok(())
    }

    pub(crate) fn trim_read_sessions(&self) -> Result<(), NativeError> {
        let retired = {
            let mut sessions = self.sessions.lock().map_err(|_| unavailable())?;
            let scope = self.read_scope.lock().map_err(|_| unavailable())?;
            let Some(scope) = scope.as_ref() else {
                return Ok(());
            };
            let keys: Vec<_> = sessions
                .iter()
                .filter(|(key, session)| {
                    key.yard.is_some() && *key != scope && Arc::strong_count(session) == 1
                })
                .map(|(key, _)| key.clone())
                .collect();
            keys.into_iter()
                .filter_map(|key| sessions.remove(&key))
                .collect::<Vec<_>>()
        };
        // Session drop deliberately retires and reaps the child outside the cache lock.
        drop(retired);
        Ok(())
    }

    pub fn fleet(&self, connection: Option<String>) -> Result<LocalFleetSnapshot, NativeError> {
        Ok(self.fleet_observed(connection)?.value)
    }
    pub fn fleet_observed(
        &self,
        connection: Option<String>,
    ) -> Result<Observed<LocalFleetSnapshot>, NativeError> {
        let session = self.session(SessionKey {
            connection,
            yard: None,
        })?;
        self.snapshot_observed(&session)
    }
    pub fn yard(
        &self,
        connection: Option<String>,
        yard: String,
    ) -> Result<YardDetails, NativeError> {
        Ok(self.yard_observed(connection, yard)?.value)
    }
    pub fn yard_observed(
        &self,
        connection: Option<String>,
        yard: String,
    ) -> Result<Observed<YardDetails>, NativeError> {
        let session = self.session(SessionKey {
            connection,
            yard: Some(yard),
        })?;
        session.rpc.require("profile-list-v1")?;
        session.rpc.require("settings-list-v1")?;
        let first: Observed<ProfileList> = query_observed(&session, "profile.list")?;
        let profiles = first.value;
        let settings: SettingsList = query(&session, "settings.list")?;
        profiles.validate(&session.current_yard)?;
        settings.validate(&session.current_yard)?;
        let (diagnostics, diagnostics_ready) = match query::<YardStatus>(&session, "yard.status") {
            Ok(status) => (status.facts()?, true),
            Err(error) => (
                vec![Fact {
                    label: "Status".into(),
                    value: error.message,
                }],
                false,
            ),
        };
        Ok(Observed {
            proof: first.proof,
            value: YardDetails {
                profiles: profiles.profiles,
                selection: profiles.selection,
                settings: settings.settings,
                diagnostics,
                capabilities: session.rpc.capabilities.clone(),
                observed_diagnostics_ready: session.observation.as_ref().map(|_| diagnostics_ready),
            },
        })
    }
    pub fn host(&self, connection: Option<String>) -> Result<HostDetails, NativeError> {
        Ok(self.host_observed(connection)?.value)
    }
    pub fn host_observed(
        &self,
        connection: Option<String>,
    ) -> Result<Observed<HostDetails>, NativeError> {
        let session = self.session(SessionKey {
            connection,
            yard: None,
        })?;
        session.rpc.require("settings-list-v1")?;
        session.rpc.require("host-sync-status-v1")?;
        let first: Observed<SettingsList> = query_observed(&session, "settings.list")?;
        let settings = first.value;
        settings.validate(&session.current_yard)?;
        let sync: HostSyncStatus = query(&session, "host.sync.status")?;
        sync.validate(&session.host_id)?;
        let profiles = if session
            .rpc
            .capabilities
            .iter()
            .any(|capability| capability == "profile-list-v1")
        {
            let profiles: ProfileList = query(&session, "profile.list")?;
            profiles.validate(&session.current_yard)?;
            profiles.profiles
        } else {
            Vec::new()
        };
        Ok(Observed {
            proof: first.proof,
            value: HostDetails {
                profiles,
                settings: settings
                    .settings
                    .into_iter()
                    .filter(|setting| setting.scopes.iter().any(|scope| scope == "host"))
                    .collect(),
                diagnostics: vec![
                    Fact {
                        label: "Owner".into(),
                        value: session.host_id.clone(),
                    },
                    Fact {
                        label: "Subyard".into(),
                        value: session.rpc.engine_version.clone(),
                    },
                ],
                sync,
                capabilities: session.rpc.capabilities.clone(),
            },
        })
    }

    pub fn plan(
        &self,
        connection: Option<String>,
        yard: Option<String>,
        command: String,
        arguments: Vec<String>,
    ) -> Result<OperationPlan, NativeError> {
        // Command authorization and target assessment remain authoritative on the owner.
        if !transport::identity(&command)
            || arguments.len() > 64
            || arguments
                .iter()
                .any(|argument| !value_text(argument, 8192, true))
        {
            return Err(transport::invalid());
        }
        let bootstrap = command == "init"
            && yard.is_some()
            && arguments.len() == 2
            && arguments[0] == "--profile";
        let target_yard = bootstrap.then(|| yard.clone()).flatten();
        if target_yard
            .as_deref()
            .is_some_and(|yard| yard == "default" || !safe_name(yard))
        {
            return Err(transport::invalid());
        }
        let session = self.session(SessionKey {
            connection,
            yard: if bootstrap { None } else { yard },
        })?;
        if bootstrap {
            session.rpc.require("yard-bootstrap-v1")?;
        }
        session.rpc.require("operation-exact-plan-v1")?;
        session.rpc.require("operation-steps-v1")?;
        if session.rpc.take_stale() {
            let _ = self.snapshot(&session)?;
        }
        let mut plans = self.plans.lock().map_err(|_| unavailable())?;
        self.expire_plans(&mut plans);
        if plans.len() >= MAX_PLANS {
            return Err(capacity());
        }
        let operation_id = format!("operation-{}", random_id()?);
        let epoch = session.epoch.load(Ordering::Acquire);
        let mut parameters =
            json!({"command":command,"arguments":arguments,"exact":true,"stepSchema":1});
        if let Some(yard) = target_yard {
            parameters["targetYard"] = json!(yard);
        }
        let raw = session.rpc.call(
            "operation.plan",
            &operation_id,
            parameters,
            Duration::from_secs(30),
        )?;
        let exact: ExactPlan = match decode(raw).and_then(|exact: ExactPlan| {
            exact.validate(&operation_id, &command)?;
            Ok(exact)
        }) {
            Ok(exact) => exact,
            Err(error) => {
                let _ = session.rpc.discard(&operation_id);
                return Err(error);
            }
        };
        if epoch != session.epoch.load(Ordering::Acquire) {
            let _ = session.rpc.discard(&operation_id);
            return Err(expired());
        }
        let owner_expiry = time::OffsetDateTime::parse(
            &exact.expires_at,
            &time::format_description::well_known::Rfc3339,
        )
        .map_err(|_| transport::invalid())?;
        let remaining = owner_expiry - time::OffsetDateTime::now_utc();
        if !remaining.is_positive() {
            let _ = session.rpc.discard(&operation_id);
            return Err(expired());
        }
        let expires = Instant::now()
            + Duration::try_from(remaining)
                .map_err(|_| transport::invalid())?
                .min(Duration::from_secs(300));
        let plan = OperationPlan {
            plan_id: random_id()?,
            digest: exact.digest,
            operation_id,
            summary: exact.plan.command,
            confirmation: exact.plan.confirmation,
            consequences: exact.plan.consequences,
            expires_at: exact.expires_at,
            steps: exact.plan.steps,
        };
        plans.insert(
            plan.plan_id.clone(),
            HeldPlan {
                session,
                plan: plan.clone(),
                expires,
                epoch,
            },
        );
        let _ = self.wake.try_send(());
        Ok(plan)
    }
    fn expire_plans(&self, plans: &mut HashMap<String, HeldPlan>) {
        plans.retain(|_, held| {
            if Instant::now() < held.expires && !held.session.rpc.is_closed() {
                true
            } else {
                let _ = held.session.rpc.discard(&held.plan.operation_id);
                false
            }
        });
    }
    pub fn execute(
        self: &Arc<Self>,
        id: &str,
        digest: &str,
        confirmed: bool,
    ) -> Result<Started, NativeError> {
        self.execute_with_submission(id, digest, confirmed)
            .map_err(ExecutionNotSubmitted::into_error)
    }
    pub fn execute_with_submission(
        self: &Arc<Self>,
        id: &str,
        digest: &str,
        confirmed: bool,
    ) -> Result<Started, ExecutionNotSubmitted> {
        // Every error in execute_reviewed precedes successful RpcClient::begin
        // admission. RpcClient::send returns Err only if no request was queued;
        // all subsequent failures are reported by the execution waiter as events.
        let result = self
            .execute_reviewed(id, digest, confirmed)
            .map_err(|error| ExecutionNotSubmitted { error });
        let _ = self.wake.try_send(());
        result
    }
    fn execute_reviewed(
        self: &Arc<Self>,
        id: &str,
        digest: &str,
        confirmed: bool,
    ) -> Result<Started, NativeError> {
        if !confirmed {
            return Err(NativeError::new(
                "confirmation_required",
                "Review and confirm the exact owner plan before execution.",
            ));
        }
        let held = self
            .plans
            .lock()
            .map_err(|_| unavailable())?
            .remove(id)
            .ok_or_else(expired)?;
        self.validate_session(&held.session)?;
        if held.plan.digest != digest
            || Instant::now() >= held.expires
            || held.session.rpc.is_closed()
            || held.session.rpc.take_stale()
            || held.epoch != held.session.epoch.load(Ordering::Acquire)
        {
            let _ = held.session.rpc.discard(&held.plan.operation_id);
            return Err(expired());
        }
        let operation_id = held.plan.operation_id.clone();
        let call = {
            let mut running = self.running.lock().map_err(|_| unavailable())?;
            if running.len() >= MAX_PLANS {
                let _ = held.session.rpc.discard(&operation_id);
                return Err(capacity());
            }
            // Queue execute while cancellation is locked out, so cancel cannot
            // overtake a just-started operation on the ordered writer channel.
            let call = held.session.rpc.begin(
                "operation.execute",
                &operation_id,
                json!({"digest":held.plan.digest,"confirmed":true}),
            )?;
            running.insert(
                operation_id.clone(),
                Running {
                    session: held.session.clone(),
                },
            );
            call
        };
        let client = self.clone();
        thread::spawn(move || {
            let result = held.session.rpc.wait(call, Duration::from_secs(3600));
            let (state, message) = match result {
                Ok(value) => match validate_execution(&value, &held.plan.operation_id) {
                    Ok(()) => ("completed", "The owner verified the operation.".to_string()),
                    Err(error) => ("unknown", error.message),
                },
                Err(error) if error.code == "cancelled" => ("cancelled", error.message),
                Err(error)
                    if matches!(
                        error.code.as_str(),
                        "disconnected" | "rpc_timeout" | "invalid_response"
                    ) =>
                {
                    ("unknown", error.message)
                }
                Err(error) => ("failed", error.message),
            };
            if let Ok(mut running) = client.running.lock() {
                running.remove(&held.plan.operation_id);
            }
            (client.emit)(VerandaEvent {
                connection_id: held.session.key.connection.clone(),
                yard: held.session.key.yard.clone(),
                operation_id: Some(held.plan.operation_id),
                state: state.into(),
                message: Some(message),
                snapshot: None,
                observation: held
                    .session
                    .observation
                    .as_ref()
                    .map(|source| source.update(None)),
            });
            held.session.dirty.store(true, Ordering::Release);
            drop(held.session);
            let _ = client.wake.try_send(());
        });
        Ok(Started { operation_id })
    }
    pub fn discard(&self, id: &str) -> Result<(), NativeError> {
        let held = self
            .plans
            .lock()
            .map_err(|_| unavailable())?
            .remove(id)
            .ok_or_else(expired)?;
        let result = held.session.rpc.call(
            "operation.discard",
            &held.plan.operation_id,
            json!({}),
            QUERY_TIMEOUT,
        );
        drop(held);
        let _ = self.wake.try_send(());
        result.map(|_| ())
    }
    pub fn cancel(&self, operation_id: &str) -> Result<(), NativeError> {
        let running = self.running.lock().map_err(|_| unavailable())?;
        let operation = running.get(operation_id).ok_or_else(expired)?;
        operation.session.rpc.cancel(operation_id)?;
        Ok(())
    }
    pub fn launch(
        &self,
        connection: Option<String>,
        yard: Option<String>,
        project: Option<String>,
        kind: String,
    ) -> Result<Launch, NativeError> {
        let (session, descriptor) =
            self.session_descriptor(connection.clone(), yard, project, &kind)?;
        let mut store = self.store.lock().map_err(|_| unavailable())?;
        if session.rpc.is_closed() {
            return Err(unavailable());
        }
        let pin = session
            ._pin
            .as_ref()
            .map(|bound| store.registered_ssh_command_bound(bound))
            .transpose();
        let pin = match pin {
            Ok(pin) => pin,
            Err(error) => {
                drop(store);
                self.invalidate_session(&session);
                return Err(error);
            }
        };
        let editor_id = format!(
            "{}-{}",
            connection.as_deref().unwrap_or("local"),
            session.current_yard
        );
        let usage = session
            .observation
            .as_ref()
            .and_then(Source::child_usage_context)
            .or_else(|| {
                self.observation
                    .as_ref()
                    .and_then(Observation::child_usage_context)
            });
        sessions::launch_with_usage(
            descriptor,
            pin,
            &mut store,
            self.launch_count.clone(),
            &editor_id,
            usage.as_ref(),
        )
    }
    pub fn shell_command(
        &self,
        connection: Option<String>,
        yard: Option<String>,
        project: Option<String>,
    ) -> Result<Launch, NativeError> {
        let (session, descriptor) =
            self.session_descriptor(connection.clone(), yard, project, "shell")?;
        let store = self.store.lock().map_err(|_| unavailable())?;
        if session.rpc.is_closed() {
            return Err(unavailable());
        }
        let pin = session
            ._pin
            .as_ref()
            .map(|bound| store.registered_ssh_command_bound(bound))
            .transpose();
        let pin = match pin {
            Ok(pin) => pin,
            Err(error) => {
                drop(store);
                self.invalidate_session(&session);
                return Err(error);
            }
        };
        sessions::shell_command(descriptor, pin)
    }
    fn session_descriptor(
        &self,
        connection: Option<String>,
        yard: Option<String>,
        project: Option<String>,
        kind: &str,
    ) -> Result<(Arc<Session>, Descriptor), NativeError> {
        if !matches!(kind, "shell" | "vscode" | "resources")
            || project
                .as_ref()
                .is_some_and(|project| !safe_id(project, 128))
            || (yard.is_none() && project.is_some())
        {
            return Err(transport::invalid());
        }
        let host_scope = yard.is_none();
        let session = self.session(SessionKey {
            connection: connection.clone(),
            yard,
        })?;
        session.rpc.require("session-prepare-v1")?;
        let descriptor: Descriptor = decode(session.rpc.call("session.prepare", "", json!({"kind":kind,"scope":if host_scope { "host" } else { "yard" },"projectId":project.as_deref().unwrap_or("")}), QUERY_TIMEOUT)?)?;
        descriptor.validate(kind, &session.current_yard, host_scope, project.as_deref())?;
        Ok((session, descriptor))
    }
    #[cfg(test)]
    fn disconnect(&self, connection: Option<&str>) {
        if let Ok(mut plans) = self.plans.lock() {
            plans.retain(|_, held| held.session.key.connection.as_deref() != connection);
        }
        if let Ok(mut sessions) = self.sessions.lock() {
            sessions.retain(|key, session| {
                if key.connection.as_deref() == connection {
                    session.rpc.close();
                    false
                } else {
                    true
                }
            });
        }
    }
    pub fn shutdown(&self) {
        self.stopped.store(true, Ordering::Release);
        let _ = self.wake.try_send(());
        self.processes.stop_accepting();
        if let Ok(mut sessions) = self.sessions.lock() {
            for session in sessions.values() {
                session.rpc.close();
            }
            sessions.clear();
        }
        self.processes.shutdown();
        if let Ok(mut plans) = self.plans.lock() {
            plans.clear();
        }
    }
}
impl Drop for Client {
    fn drop(&mut self) {
        self.shutdown();
    }
}

fn monitor(weak: Weak<Client>, receiver: mpsc::Receiver<()>) {
    loop {
        let Some(client) = weak.upgrade() else { break };
        if client.stopped.load(Ordering::Acquire) {
            break;
        }
        let mut deadline: Option<Instant> = None;
        if let Ok(mut plans) = client.plans.lock() {
            client.expire_plans(&mut plans);
            deadline = plans.values().map(|held| held.expires).min();
        }
        if client.trim_read_sessions().is_err() {
            break;
        }
        let sessions: Vec<_> = match client.sessions.lock() {
            Ok(sessions) => sessions.values().cloned().collect(),
            Err(_) => break,
        };
        let mut started = false;
        for session in sessions {
            if session.refreshing.load(Ordering::Acquire) {
                continue;
            }
            let closed = session.rpc.is_closed();
            if closed {
                if let Ok(retry) = session.retry.lock() {
                    if let Some(next) = *retry {
                        deadline = Some(deadline.map_or(next, |value| value.min(next)));
                        if Instant::now() < next {
                            continue;
                        }
                    }
                }
            }
            if session.refreshing.load(Ordering::Acquire)
                || (!closed && !session.dirty.load(Ordering::Acquire))
            {
                continue;
            }
            if session.refreshing.swap(true, Ordering::AcqRel) {
                continue;
            }
            session.dirty.store(false, Ordering::Release);
            started = true;
            let client = client.clone();
            thread::spawn(move || {
                if closed {
                    (client.emit)(state_event(&session, "reconnecting", None));
                    match client
                        .session(session.key.clone())
                        .and_then(|next| client.snapshot_observed(&next).map(|read| (next, read)))
                    {
                        Ok((next, read)) => (client.emit)(read_event(&next, read)),
                        Err(error) => {
                            let incompatible = matches!(
                                error.code.as_str(),
                                "incompatible_engine"
                                    | "host_identity_changed"
                                    | "capability_missing"
                            );
                            if let Ok(mut retry) = session.retry.lock() {
                                *retry = Some(
                                    Instant::now()
                                        + Duration::from_secs(if incompatible { 3600 } else { 2 }),
                                );
                            }
                            let mut event = state_event(
                                &session,
                                if incompatible {
                                    "incompatible"
                                } else {
                                    "disconnected"
                                },
                                None,
                            );
                            event.message = Some(error.message);
                            (client.emit)(event);
                        }
                    }
                } else {
                    // No owner polling when idle. Coalesce events into one authoritative read.
                    match client.snapshot_observed(&session) {
                        Ok(read) => (client.emit)(read_event(&session, read)),
                        Err(_) => (client.emit)(state_event(&session, "stale", None)),
                    }
                }
                session.refreshing.store(false, Ordering::Release);
                drop(session);
                let _ = client.wake.try_send(());
            });
        }
        drop(client);
        if started {
            thread::sleep(Duration::from_millis(100));
        }
        let result = match deadline {
            Some(deadline) => receiver
                .recv_timeout(deadline.saturating_duration_since(Instant::now()))
                .map_err(|error| matches!(error, mpsc::RecvTimeoutError::Disconnected)),
            None => receiver.recv().map_err(|_| true),
        };
        if matches!(result, Err(true)) {
            break;
        }
    }
}
fn state_event(
    session: &Session,
    state: &str,
    snapshot: Option<LocalFleetSnapshot>,
) -> VerandaEvent {
    VerandaEvent {
        connection_id: session.key.connection.clone(),
        yard: session.key.yard.clone(),
        operation_id: None,
        state: state.into(),
        message: None,
        snapshot,
        observation: session
            .observation
            .as_ref()
            .map(|source| source.update(None)),
    }
}
fn read_event(session: &Session, read: Observed<LocalFleetSnapshot>) -> VerandaEvent {
    let mut event = state_event(session, "connected", Some(read.value));
    event.observation = session
        .observation
        .as_ref()
        .map(|source| source.update(read.proof));
    event
}
#[cfg(test)]
fn snapshot(session: &Session) -> Result<LocalFleetSnapshot, NativeError> {
    Ok(snapshot_observed(session)?.value)
}
fn snapshot_observed(session: &Session) -> Result<Observed<LocalFleetSnapshot>, NativeError> {
    // Inventory is a complete authoritative snapshot; event payloads never become owner state.
    let epoch = session.epoch.load(Ordering::Acquire);
    let initial = session
        .initial_snapshot
        .lock()
        .map_err(|_| unavailable())?
        .take();
    let (snapshot, proof) = match initial.filter(|(captured, _)| *captured == epoch) {
        Some((_, snapshot)) => (snapshot, session.observation.as_ref().map(Source::cached)),
        None => {
            let inventory: Observed<OwnerInventory> = query_observed(session, "owner.inventory")?;
            (
                inventory.value.into_snapshot(
                    session.rpc.engine_version.clone(),
                    session.current_yard.clone(),
                )?,
                inventory.proof,
            )
        }
    };
    if snapshot.owner.id != session.host_id {
        return Err(transport::invalid());
    }
    if epoch != session.epoch.load(Ordering::Acquire) {
        session.dirty.store(true, Ordering::Release);
        return Err(expired());
    }
    session.rpc.take_stale();
    Ok(Observed {
        value: decorate_snapshot(session, snapshot),
        proof,
    })
}
fn decorate_snapshot(session: &Session, mut snapshot: LocalFleetSnapshot) -> LocalFleetSnapshot {
    snapshot.connection_id = session.key.connection.clone();
    snapshot.capabilities = session.rpc.capabilities.clone();
    snapshot.veranda_version = Some(product_version().into());
    snapshot
}
fn query_observed<T: for<'de> Deserialize<'de>>(
    session: &Session,
    method: &str,
) -> Result<Observed<T>, NativeError> {
    let (value, proof) = session.rpc.call_observed(method, QUERY_TIMEOUT)?;
    Ok(Observed {
        value: decode(value)?,
        proof,
    })
}
fn query<T: for<'de> Deserialize<'de>>(session: &Session, method: &str) -> Result<T, NativeError> {
    decode(session.rpc.call(method, "", json!({}), QUERY_TIMEOUT)?)
}
fn decode<T: for<'de> Deserialize<'de>>(value: Value) -> Result<T, NativeError> {
    serde_json::from_value(value).map_err(|_| transport::invalid())
}
fn validate_key(key: &SessionKey) -> Result<(), NativeError> {
    if key
        .connection
        .as_ref()
        .is_some_and(|id| !transport::identity(id))
        || key.yard.as_ref().is_some_and(|yard| !safe_name(yard))
    {
        Err(transport::invalid())
    } else {
        Ok(())
    }
}
fn random_id() -> Result<String, NativeError> {
    let mut bytes = [0u8; 16];
    getrandom::fill(&mut bytes).map_err(|_| unavailable())?;
    Ok(bytes.iter().map(|byte| format!("{byte:02x}")).collect())
}
fn text(value: &str, limit: usize) -> bool {
    value.len() <= limit && !value.chars().any(|character| character.is_control())
}
fn value_text(value: &str, limit: usize, multiline: bool) -> bool {
    value.len() <= limit
        && !value
            .chars()
            .any(|character| character.is_control() && !(multiline && character == '\n'))
}
fn list_text(value: &str) -> bool {
    value.len() <= 8192
        && !value
            .chars()
            .any(|character| character.is_control() && !character.is_whitespace())
}
fn required(value: &str, limit: usize) -> bool {
    !value.trim().is_empty() && text(value, limit)
}
fn unavailable() -> NativeError {
    NativeError::new(
        "native_unavailable",
        "Veranda could not use its native connection state. Reopen the application and try again.",
    )
}
fn capacity() -> NativeError {
    NativeError::new(
        "request_capacity",
        "Finish or discard an existing plan before opening more connections or operations.",
    )
}
fn expired() -> NativeError {
    NativeError::new(
        "plan_stale",
        "The reviewed plan is no longer valid. Refresh the owner and prepare a new plan.",
    )
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
pub struct Assessment {
    assessment_id: String,
    destination: String,
    fingerprint: String,
    previous_fingerprint: Option<String>,
    confirmation: String,
    consequences: Vec<String>,
}
impl From<TrustAssessment> for Assessment {
    fn from(value: TrustAssessment) -> Self {
        Self {
            assessment_id: value.token,
            destination: value.destination,
            fingerprint: value.fingerprint,
            previous_fingerprint: value.previous_fingerprint,
            confirmation: value.confirmation_policy,
            consequences: value.consequences,
        }
    }
}
impl Assessment {
    pub fn id(&self) -> &str {
        &self.assessment_id
    }
    pub fn destination(&self) -> &str {
        &self.destination
    }
    pub fn fingerprint(&self) -> &str {
        &self.fingerprint
    }
    pub fn previous_fingerprint(&self) -> Option<&str> {
        self.previous_fingerprint.as_deref()
    }
    pub fn confirmation(&self) -> &str {
        &self.confirmation
    }
    pub fn consequences(&self) -> &[String] {
        &self.consequences
    }
}
#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
pub struct Removal {
    assessment_id: String,
    connection: ConnectionSummary,
    confirmation: String,
    consequences: Vec<String>,
}
impl From<RemoveAssessment> for Removal {
    fn from(value: RemoveAssessment) -> Self {
        Self {
            assessment_id: value.token,
            connection: value.connection,
            confirmation: value.confirmation_policy,
            consequences: value.consequences,
        }
    }
}
#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct CurrentContext {
    yard_name: String,
}
impl CurrentContext {
    fn validate(&self, requested: Option<&str>) -> Result<(), NativeError> {
        if !safe_name(&self.yard_name) || requested.is_some_and(|yard| yard != self.yard_name) {
            return Err(NativeError::new(
                "invalid_response",
                "The owner returned a context for a different or invalid yard.",
            ));
        }
        Ok(())
    }
}
#[derive(Clone, Deserialize, Serialize)]
pub struct Provenance {
    scope: String,
    role: String,
    status: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    value: Option<String>,
}
// Go nil slices encode as null. A custom deserializer permits null, while
// deliberately omitting serde(default) keeps absent wire fields an error.
fn nullable_vec<'de, D, T>(deserializer: D) -> Result<Vec<T>, D::Error>
where
    D: serde::Deserializer<'de>,
    T: Deserialize<'de>,
{
    Ok(Option::<Vec<T>>::deserialize(deserializer)?.unwrap_or_default())
}
#[derive(Deserialize, Serialize)]
pub struct Selection {
    value: String,
    #[serde(deserialize_with = "nullable_vec")]
    requested: Vec<String>,
    #[serde(deserialize_with = "nullable_vec")]
    effective: Vec<String>,
    present: bool,
    #[serde(deserialize_with = "nullable_vec")]
    provenance: Vec<Provenance>,
}
#[derive(Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct Profile {
    name: String,
    #[serde(default)]
    has_yard_preset: bool,
    descriptor_version: Option<u32>,
    selected: bool,
    requested: bool,
    effective: bool,
    reason: String,
    source_group: String,
    provisionable: bool,
    provision_scope: String,
    eligible: bool,
    eligibility: String,
    convergence: String,
    diagnostic: Option<String>,
    #[serde(deserialize_with = "nullable_vec")]
    resources: Vec<String>,
}
#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct ProfileList {
    schema_version: u32,
    yard_name: String,
    selection: Selection,
    #[serde(deserialize_with = "nullable_vec")]
    profiles: Vec<Profile>,
}
impl ProfileList {
    fn validate(&self, yard: &str) -> Result<(), NativeError> {
        if self.schema_version != 1
            || self.yard_name != yard
            || self.profiles.len() > 256
            || !list_text(&self.selection.value)
            || [&self.selection.requested, &self.selection.effective]
                .iter()
                .any(|names| {
                    names.join(" ").len() > 8192 || names.iter().any(|name| !safe_name(name))
                })
            || !profile_provenance_valid(&self.selection.provenance)
            || self.profiles.iter().any(|profile| {
                !safe_name(&profile.name)
                    || !list_text(&profile.reason)
                    || !list_text(&profile.source_group)
                    || profile
                        .descriptor_version
                        .is_some_and(|version| version != 1)
                    || profile
                        .diagnostic
                        .as_ref()
                        .is_some_and(|value| !transport::identity(value))
                    || !matches!(profile.provision_scope.as_str(), "shared" | "dedicated")
                    || !matches!(
                        profile.eligibility.as_str(),
                        "allowed"
                            | "dedicated-role-required"
                            | "exclusive-role"
                            | "lifecycle-only"
                            | "policy-disabled"
                    )
                    || !matches!(
                        profile.convergence.as_str(),
                        "unknown" | "not-applicable" | "current" | "changes-required"
                    )
                    || profile.resources.len() > 128
                    || profile
                        .resources
                        .iter()
                        .any(|resource| !transport::identity(resource))
            })
        {
            Err(transport::invalid())
        } else {
            Ok(())
        }
    }
}
#[derive(Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct Setting {
    name: String,
    kind: String,
    #[serde(rename = "type")]
    value_type: String,
    default: Option<String>,
    value: Option<String>,
    value_available: bool,
    aliases: Vec<String>,
    scopes: Vec<String>,
    syncable: bool,
    merge: String,
    application: String,
    owner: String,
    #[serde(rename = "enum")]
    choices: Vec<String>,
    minimum: i64,
    maximum: i64,
    optional: bool,
    editable: bool,
    provenance: Vec<Provenance>,
}
#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct SettingsList {
    schema_version: u32,
    yard_name: String,
    settings: Vec<Setting>,
}
impl SettingsList {
    fn validate(&self, yard: &str) -> Result<(), NativeError> {
        if self.schema_version != 1
            || self.yard_name != yard
            || self.settings.len() > 512
            || self.settings.iter().any(|setting| {
                !transport::identity(&setting.name)
                    || !required(&setting.kind, 64)
                    || !required(&setting.value_type, 64)
                    || setting
                        .value
                        .as_ref()
                        .is_some_and(|value| !setting.text_valid(value))
                    || setting
                        .default
                        .as_ref()
                        .is_some_and(|value| !setting.text_valid(value))
                    || setting.provenance.len() > 64
                    || setting.provenance.iter().any(|value| {
                        !provenance_metadata_valid(value)
                            || value
                                .value
                                .as_ref()
                                .is_some_and(|value| !setting.text_valid(value))
                    })
                    || setting.scopes.len() > 8
                    || setting
                        .scopes
                        .iter()
                        .any(|value| !transport::identity(value))
                    || setting.aliases.len() > 128
                    || setting
                        .aliases
                        .iter()
                        .any(|value| !transport::identity(value))
                    || [&setting.merge, &setting.application, &setting.owner]
                        .iter()
                        .any(|value| !text(value, 512))
                    || (!setting.value_available
                        && (setting.value.is_some()
                            || setting.provenance.iter().any(|value| value.value.is_some())))
                    || setting.choices.len() > 128
                    || setting.choices.iter().any(|value| !text(value, 8192))
            })
        {
            Err(transport::invalid())
        } else {
            Ok(())
        }
    }
}
impl Setting {
    fn text_valid(&self, value: &str) -> bool {
        match self.value_type.as_str() {
            "multiline" => value_text(value, 8192, true),
            // Native list types accept whitespace between entries. The owner
            // validates their structure before applying any edited value.
            "link-list" | "mount-list" | "name-list" => list_text(value),
            _ => text(value, 8192),
        }
    }
}
fn provenance_metadata_valid(value: &Provenance) -> bool {
    required(&value.scope, 64) && required(&value.role, 64) && required(&value.status, 64)
}
fn profile_provenance_valid(values: &[Provenance]) -> bool {
    values.len() <= 64
        && values.iter().all(|value| {
            provenance_metadata_valid(value)
                && value.value.as_ref().is_none_or(|value| list_text(value))
        })
}
#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct YardStatus {
    state: String,
    #[serde(default)]
    start_state: String,
    #[serde(default)]
    waiting_for_addresses: Vec<String>,
    initialized: String,
    incus_autostart: String,
    ssh_configured: bool,
    project_count: u64,
    desired: String,
    context: StatusContext,
    #[serde(default)]
    ip: String,
    #[serde(default)]
    services: String,
    #[serde(default)]
    vscode: String,
    mounts: Option<Vec<String>>,
    facts: StatusFacts,
}
#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct StatusContext {
    dev_user: String,
}
#[derive(Deserialize)]
struct StatusFacts {
    security: String,
    space: String,
}
impl YardStatus {
    fn facts(&self) -> Result<Vec<Fact>, NativeError> {
        if !required(&self.state, 128)
            || !text(&self.start_state, 128)
            || !text(&self.initialized, 128)
            || !text(&self.incus_autostart, 128)
            || self.waiting_for_addresses.len() > 32
            || self
                .waiting_for_addresses
                .iter()
                .any(|value| !transport::identity(value))
            || [
                &self.desired,
                &self.context.dev_user,
                &self.ip,
                &self.services,
                &self.vscode,
                &self.facts.security,
                &self.facts.space,
            ]
            .iter()
            .any(|value| !text(value, 1024))
            || self.mounts.as_ref().is_some_and(|values| {
                values.len() > 128 || values.iter().any(|value| !transport::identity(value))
            })
        {
            return Err(transport::invalid());
        }
        Ok(vec![
            Fact {
                label: "State".into(),
                value: self.state.clone(),
            },
            Fact {
                label: "Startup".into(),
                value: self.start_state.clone(),
            },
            Fact {
                label: "Initialized".into(),
                value: self.initialized.to_string(),
            },
            Fact {
                label: "Waiting for addresses".into(),
                value: self.waiting_for_addresses.join(", "),
            },
            Fact {
                label: "Incus autostart".into(),
                value: self.incus_autostart.clone(),
            },
            Fact {
                label: "SSH configured".into(),
                value: self.ssh_configured.to_string(),
            },
            Fact {
                label: "Projects".into(),
                value: self.project_count.to_string(),
            },
            Fact {
                label: "Desired power".into(),
                value: self.desired.clone(),
            },
            Fact {
                label: "Dev user".into(),
                value: self.context.dev_user.clone(),
            },
            Fact {
                label: "IP".into(),
                value: self.ip.clone(),
            },
            Fact {
                label: "Services".into(),
                value: self.services.clone(),
            },
            Fact {
                label: "VS Code".into(),
                value: self.vscode.clone(),
            },
            Fact {
                label: "Mounts".into(),
                value: self.mounts.as_deref().unwrap_or_default().join(", "),
            },
            Fact {
                label: "Security".into(),
                value: self.facts.security.clone(),
            },
            Fact {
                label: "Storage".into(),
                value: self.facts.space.clone(),
            },
        ])
    }
}
#[derive(Serialize)]
pub struct Fact {
    label: String,
    value: String,
}
#[derive(Serialize)]
pub struct YardDetails {
    profiles: Vec<Profile>,
    selection: Selection,
    settings: Vec<Setting>,
    diagnostics: Vec<Fact>,
    capabilities: Vec<String>,
    #[serde(skip)]
    observed_diagnostics_ready: Option<bool>,
}
#[derive(Serialize)]
pub struct HostDetails {
    profiles: Vec<Profile>,
    settings: Vec<Setting>,
    diagnostics: Vec<Fact>,
    sync: HostSyncStatus,
    capabilities: Vec<String>,
}

#[derive(Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct SyncGit {
    branch: String,
    upstream: String,
    head: String,
    relation: String,
    worktree: String,
    remote: String,
    staged: u64,
    unstaged: u64,
    untracked: u64,
    conflicts: u64,
    ahead: u64,
    behind: u64,
    last_fetch: String,
    available: bool,
}
#[derive(Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct SyncPeer {
    name: String,
    role: String,
    manual_only: bool,
    trusted: bool,
    last_attempt: i64,
    last_success: i64,
    consecutive_failures: u64,
    next_retry: i64,
    failed: bool,
}
#[derive(Deserialize, Serialize)]
pub struct SyncCredentials {
    state: String,
    records: u64,
    conflicts: u64,
    peers: Vec<SyncPeer>,
}
#[derive(Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct HostSyncStatus {
    schema_version: u32,
    host_id: String,
    host_id_pending: bool,
    automation: String,
    offline: bool,
    registration: String,
    recovery_required: bool,
    generation: u64,
    applied_commit: String,
    git: Option<SyncGit>,
    credentials: SyncCredentials,
}
impl HostSyncStatus {
    fn validate(&self, host: &str) -> Result<(), NativeError> {
        if self.schema_version != 1
            || self.host_id != host
            || !transport::identity(&self.host_id)
            || self.automation != "manual"
            || !self.offline
            || !matches!(
                self.registration.as_str(),
                "configured" | "not-configured" | "broken"
            )
            || !text(&self.applied_commit, 128)
            || self.credentials.peers.len() > 128
            || !transport::identity(&self.credentials.state)
            || self
                .credentials
                .peers
                .iter()
                .any(|peer| !required(&peer.name, 128) || !required(&peer.role, 128))
            || self.git.as_ref().is_some_and(|git| {
                [
                    &git.branch,
                    &git.upstream,
                    &git.head,
                    &git.relation,
                    &git.worktree,
                    &git.remote,
                    &git.last_fetch,
                ]
                .iter()
                .any(|value| !text(value, 1024))
            })
        {
            Err(transport::invalid())
        } else {
            Ok(())
        }
    }
}

#[derive(Clone, Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct Step {
    id: String,
    target: String,
    observed: String,
    desired: String,
    decision: String,
    #[serde(default)]
    preconditions: Vec<String>,
    #[serde(default)]
    depends_on: Vec<String>,
    verify: String,
    consequence: Option<String>,
}
#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct OwnerPlan {
    operation_id: String,
    command: String,
    effect: String,
    confirmation: String,
    target: String,
    #[serde(default)]
    consequences: Vec<String>,
    steps: Vec<Step>,
    confirmed: bool,
}
#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct ExactPlan {
    schema: u32,
    step_schema: u32,
    plan: OwnerPlan,
    digest: String,
    expires_at: String,
}
impl ExactPlan {
    fn validate(&self, operation: &str, command: &str) -> Result<(), NativeError> {
        if self.schema != 1
            || self.step_schema != 1
            || self.plan.operation_id != operation
            || self.plan.command != command
            || self.plan.effect != "mutate"
            || (self.plan.confirmed && self.plan.confirmation != "never")
            || !matches!(
                self.plan.confirmation.as_str(),
                "never" | "prompt-default-yes" | "prompt-default-no"
            )
            || !matches!(
                self.plan.target.as_str(),
                "local-owner" | "local-controller"
            )
            || self.digest.len() != 64
            || !self
                .digest
                .bytes()
                .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
            || time::OffsetDateTime::parse(
                &self.expires_at,
                &time::format_description::well_known::Rfc3339,
            )
            .is_err()
            || self.plan.steps.is_empty()
            || self.plan.steps.len() > 256
            || self.plan.consequences.len() > 128
            || self
                .plan
                .consequences
                .iter()
                .any(|value| !required(value, 8192))
        {
            return Err(transport::invalid());
        }
        let mut seen = HashSet::new();
        for step in &self.plan.steps {
            if [
                &step.id,
                &step.target,
                &step.observed,
                &step.desired,
                &step.verify,
            ]
            .iter()
            .any(|value| !required(value, 512))
                || !matches!(step.decision.as_str(), "apply" | "skip" | "conditional")
                || step.preconditions.len() > 64
                || step.preconditions.iter().any(|value| !required(value, 512))
                || step.depends_on.len() > 64
                || step.depends_on.iter().any(|value| !seen.contains(value))
                || step
                    .consequence
                    .as_ref()
                    .is_some_and(|value| !required(value, 512))
                || !seen.insert(step.id.clone())
            {
                return Err(transport::invalid());
            }
        }
        Ok(())
    }
}
#[derive(Clone, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct OperationPlan {
    pub plan_id: String,
    pub digest: String,
    pub operation_id: String,
    summary: String,
    confirmation: String,
    consequences: Vec<String>,
    expires_at: String,
    steps: Vec<Step>,
}
/// Returned only before an execute request is admitted to the ordered RPC writer.
/// Once admitted, an uncertain owner/transport outcome arrives as an Unknown event.
#[derive(Debug)]
pub struct ExecutionNotSubmitted {
    error: NativeError,
}
impl ExecutionNotSubmitted {
    pub fn error(&self) -> &NativeError {
        &self.error
    }
    pub fn into_error(self) -> NativeError {
        self.error
    }
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
pub struct Started {
    operation_id: String,
}
fn validate_execution(value: &Value, operation: &str) -> Result<(), NativeError> {
    if value.pointer("/plan/operationId").and_then(Value::as_str) != Some(operation)
        || value.pointer("/plan/confirmed").and_then(Value::as_bool) != Some(true)
        || value.pointer("/result/status").and_then(Value::as_str) != Some("ok")
    {
        Err(transport::invalid())
    } else {
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    fn fixture(name: &str) -> Value {
        let file = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("../../api/yard-rpc/v1/fixtures")
            .join(format!("{name}.json"));
        serde_json::from_slice::<Value>(&std::fs::read(file).unwrap()).unwrap()["result"].clone()
    }
    #[test]
    fn shared_query_results_match_native_boundary() {
        decode::<ProfileList>(fixture("profile-list"))
            .unwrap()
            .validate("default")
            .unwrap();
        decode::<SettingsList>(fixture("settings-list"))
            .unwrap()
            .validate("default")
            .unwrap();
        decode::<HostSyncStatus>(fixture("host-sync-status"))
            .unwrap()
            .validate("example-owner")
            .unwrap();
    }
    #[test]
    fn profile_selection_preserves_owner_list_whitespace() {
        let mut value = fixture("profile-list");
        for selection in [
            "core\ntools",
            "core\r\n\ttools",
            "core\u{000b}\u{000c}\u{0085}tools",
        ] {
            value["selection"]["value"] = json!(selection);
            value["selection"]["provenance"][0]["value"] = json!(selection);
            decode::<ProfileList>(value.clone())
                .unwrap()
                .validate("default")
                .unwrap();
        }
        for selection in ["core\0tools", "core\u{001b}tools"] {
            value["selection"]["value"] = json!(selection);
            assert!(
                decode::<ProfileList>(value.clone())
                    .unwrap()
                    .validate("default")
                    .is_err()
            );
            value["selection"]["value"] = json!("core tools");
            value["selection"]["provenance"][0]["value"] = json!(selection);
            assert!(
                decode::<ProfileList>(value.clone())
                    .unwrap()
                    .validate("default")
                    .is_err()
            );
        }
    }
    #[test]
    fn exact_plan_requires_complete_bound_acyclic_steps() {
        let mut value = fixture("operation-exact");
        decode::<ExactPlan>(value.clone())
            .unwrap()
            .validate("selection-1", "config")
            .unwrap();
        assert!(
            decode::<ExactPlan>(value.clone())
                .unwrap()
                .validate("another-operation", "config")
                .is_err()
        );
        value["plan"]["steps"][0]["dependsOn"] = json!(["config.write"]);
        assert!(
            decode::<ExactPlan>(value)
                .unwrap()
                .validate("selection-1", "config")
                .is_err()
        );
    }
    #[test]
    fn mismatched_schemas_and_owner_identity_fail_closed() {
        let mut value = fixture("profile-list");
        value["schemaVersion"] = json!(2);
        assert!(
            decode::<ProfileList>(value)
                .unwrap()
                .validate("default")
                .is_err()
        );
        assert!(
            decode::<HostSyncStatus>(fixture("host-sync-status"))
                .unwrap()
                .validate("different-owner")
                .is_err()
        );
    }
    #[test]
    fn owner_execution_must_be_confirmed_and_verified() {
        assert!(
            validate_execution(
                &json!({"plan":{"operationId":"one","confirmed":true},"result":{"status":"ok"}}),
                "one"
            )
            .is_ok()
        );
        assert!(validate_execution(
            &json!({"plan":{"operationId":"one","confirmed":true},"result":{"status":"running"}}),
            "one"
        )
        .is_err());
    }
}

#[cfg(test)]
#[path = "client_tests.rs"]
mod client_tests;

// Read-only presentation access to native validated DTOs.
impl Provenance {
    pub fn scope(&self) -> &str {
        &self.scope
    }
    pub fn role(&self) -> &str {
        &self.role
    }
    pub fn status(&self) -> &str {
        &self.status
    }
    pub fn value(&self) -> Option<&str> {
        self.value.as_deref()
    }
}
impl Selection {
    pub fn requested(&self) -> &[String] {
        &self.requested
    }
    pub fn effective(&self) -> &[String] {
        &self.effective
    }
    pub fn present(&self) -> bool {
        self.present
    }
    pub fn value(&self) -> &str {
        &self.value
    }
    pub fn provenance(&self) -> &[Provenance] {
        &self.provenance
    }
}
impl Profile {
    pub fn requested(&self) -> bool {
        self.requested
    }
    pub fn effective(&self) -> bool {
        self.effective
    }
    pub fn reason(&self) -> &str {
        &self.reason
    }
    pub fn source_group(&self) -> &str {
        &self.source_group
    }
    pub fn name(&self) -> &str {
        &self.name
    }
    pub fn has_yard_preset(&self) -> bool {
        self.has_yard_preset
    }
    pub fn selected(&self) -> bool {
        self.selected
    }
    pub fn provisionable(&self) -> bool {
        self.provisionable
    }
    pub fn provision_scope(&self) -> &str {
        &self.provision_scope
    }
    pub fn eligible(&self) -> bool {
        self.eligible
    }
    pub fn eligibility(&self) -> &str {
        &self.eligibility
    }
    pub fn convergence(&self) -> &str {
        &self.convergence
    }
    pub fn diagnostic(&self) -> Option<&str> {
        self.diagnostic.as_deref()
    }
    pub fn resources(&self) -> &[String] {
        &self.resources
    }
}
impl Setting {
    pub fn name(&self) -> &str {
        &self.name
    }
    pub fn kind(&self) -> &str {
        &self.kind
    }
    pub fn value_type(&self) -> &str {
        &self.value_type
    }
    pub fn default(&self) -> Option<&str> {
        self.default.as_deref()
    }
    pub fn value(&self) -> Option<&str> {
        self.value.as_deref()
    }
    pub fn value_available(&self) -> bool {
        self.value_available
    }
    pub fn scopes(&self) -> &[String] {
        &self.scopes
    }
    pub fn application(&self) -> &str {
        &self.application
    }
    pub fn owner(&self) -> &str {
        &self.owner
    }
    pub fn choices(&self) -> &[String] {
        &self.choices
    }
    pub fn minimum(&self) -> i64 {
        self.minimum
    }
    pub fn maximum(&self) -> i64 {
        self.maximum
    }
    pub fn optional(&self) -> bool {
        self.optional
    }
    pub fn editable(&self) -> bool {
        self.editable
    }
    pub fn provenance(&self) -> &[Provenance] {
        &self.provenance
    }
}
impl Fact {
    pub fn label(&self) -> &str {
        &self.label
    }
    pub fn value(&self) -> &str {
        &self.value
    }
}
impl YardDetails {
    pub fn observed_diagnostics_ready(&self) -> Option<bool> {
        self.observed_diagnostics_ready
    }
    pub fn profiles(&self) -> &[Profile] {
        &self.profiles
    }
    pub fn selection(&self) -> &Selection {
        &self.selection
    }
    pub fn settings(&self) -> &[Setting] {
        &self.settings
    }
    pub fn diagnostics(&self) -> &[Fact] {
        &self.diagnostics
    }
    pub fn capabilities(&self) -> &[String] {
        &self.capabilities
    }
}
impl HostDetails {
    pub fn profiles(&self) -> &[Profile] {
        &self.profiles
    }
    pub fn settings(&self) -> &[Setting] {
        &self.settings
    }
    pub fn diagnostics(&self) -> &[Fact] {
        &self.diagnostics
    }
    pub fn sync(&self) -> &HostSyncStatus {
        &self.sync
    }
    pub fn capabilities(&self) -> &[String] {
        &self.capabilities
    }
}
impl HostSyncStatus {
    pub fn host_id(&self) -> &str {
        &self.host_id
    }
    pub fn registration(&self) -> &str {
        &self.registration
    }
    pub fn host_id_pending(&self) -> bool {
        self.host_id_pending
    }
    pub fn recovery_required(&self) -> bool {
        self.recovery_required
    }
    pub fn generation(&self) -> u64 {
        self.generation
    }
    pub fn applied_commit(&self) -> &str {
        &self.applied_commit
    }
    pub fn git(&self) -> Option<&SyncGit> {
        self.git.as_ref()
    }
    pub fn credentials(&self) -> &SyncCredentials {
        &self.credentials
    }
}
impl SyncGit {
    pub fn branch(&self) -> &str {
        &self.branch
    }
    pub fn upstream(&self) -> &str {
        &self.upstream
    }
    pub fn head(&self) -> &str {
        &self.head
    }
    pub fn relation(&self) -> &str {
        &self.relation
    }
    pub fn worktree(&self) -> &str {
        &self.worktree
    }
    pub fn remote(&self) -> &str {
        &self.remote
    }
    pub fn available(&self) -> bool {
        self.available
    }
    pub fn staged(&self) -> u64 {
        self.staged
    }
    pub fn unstaged(&self) -> u64 {
        self.unstaged
    }
    pub fn untracked(&self) -> u64 {
        self.untracked
    }
    pub fn conflicts(&self) -> u64 {
        self.conflicts
    }
    pub fn ahead(&self) -> u64 {
        self.ahead
    }
    pub fn behind(&self) -> u64 {
        self.behind
    }
    pub fn last_fetch(&self) -> &str {
        &self.last_fetch
    }
}
impl SyncCredentials {
    pub fn state(&self) -> &str {
        &self.state
    }
    pub fn records(&self) -> u64 {
        self.records
    }
    pub fn conflicts(&self) -> u64 {
        self.conflicts
    }
    pub fn peers(&self) -> &[SyncPeer] {
        &self.peers
    }
}
impl SyncPeer {
    pub fn name(&self) -> &str {
        &self.name
    }
    pub fn role(&self) -> &str {
        &self.role
    }
    pub fn manual_only(&self) -> bool {
        self.manual_only
    }
    pub fn trusted(&self) -> bool {
        self.trusted
    }
    pub fn last_attempt(&self) -> i64 {
        self.last_attempt
    }
    pub fn last_success(&self) -> i64 {
        self.last_success
    }
    pub fn consecutive_failures(&self) -> u64 {
        self.consecutive_failures
    }
    pub fn next_retry(&self) -> i64 {
        self.next_retry
    }
    pub fn failed(&self) -> bool {
        self.failed
    }
}
impl Step {
    pub fn id(&self) -> &str {
        &self.id
    }
    pub fn target(&self) -> &str {
        &self.target
    }
    pub fn observed(&self) -> &str {
        &self.observed
    }
    pub fn desired(&self) -> &str {
        &self.desired
    }
    pub fn decision(&self) -> &str {
        &self.decision
    }
    pub fn preconditions(&self) -> &[String] {
        &self.preconditions
    }
    pub fn depends_on(&self) -> &[String] {
        &self.depends_on
    }
    pub fn verify(&self) -> &str {
        &self.verify
    }
    pub fn consequence(&self) -> Option<&str> {
        self.consequence.as_deref()
    }
}
impl OperationPlan {
    pub fn summary(&self) -> &str {
        &self.summary
    }
    pub fn confirmation(&self) -> &str {
        &self.confirmation
    }
    pub fn consequences(&self) -> &[String] {
        &self.consequences
    }
    pub fn expires_at(&self) -> &str {
        &self.expires_at
    }
    pub fn steps(&self) -> &[Step] {
        &self.steps
    }
}
impl Started {
    pub fn operation_id(&self) -> &str {
        &self.operation_id
    }
}
