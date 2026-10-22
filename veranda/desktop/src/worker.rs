use crate::state::{AppState, Scope};
use async_channel::{Receiver, Sender};
use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::{Arc, Condvar, Mutex};
use std::thread::{self, JoinHandle};
use veranda_lib::client::{
    Assessment, Client, ConnectionAttempt, HostDetails, OperationPlan, VerandaEvent, YardDetails,
};
use veranda_lib::connections::{ConnectionSummary, NavigationSelection};
use veranda_lib::local_fleet::{LocalFleetSnapshot, NativeError};
use veranda_lib::observation::{Batch, Observation, Observed, ReadProof, Retirement};

pub enum Update {
    Initialized {
        connections: Vec<ConnectionSummary>,
        selection: Option<NavigationSelection>,
        local: Result<LocalFleetSnapshot, NativeError>,
        remote: Option<(Option<String>, Result<LocalFleetSnapshot, NativeError>)>,
        preference_error: Option<NativeError>,
        state: Result<AppState, NativeError>,
    },
    Fleet {
        generation: u64,
        connection: Option<String>,
        invalidation: u64,
        result: Result<LocalFleetSnapshot, NativeError>,
        observation: Option<ReadObservation>,
    },
    Saved {
        selection: NavigationSelection,
        result: Result<(), NativeError>,
    },
    Reviewed {
        id: u64,
        result: Result<Assessment, NativeError>,
    },
    Connected {
        id: u64,
        result: Result<ConnectionSummary, NativeError>,
    },
    Cancelled {
        id: u64,
    },
    Details {
        generation: u64,
        scope: Scope,
        invalidation: u64,
        result: Result<Details, NativeError>,
        observation: Option<ReadObservation>,
    },
    StateSaved {
        revision: u64,
        result: Result<(), NativeError>,
    },
    Planned {
        request: u64,
        result: Result<OperationPlan, NativeError>,
    },
    Executed {
        request: u64,
        result: Result<String, NotSubmitted>,
    },
    Discarded {
        request: u64,
        result: Result<(), NativeError>,
    },
    OperationCancelled {
        request: u64,
        result: Result<(), NativeError>,
    },
    Launched {
        generation: u64,
        scope: Scope,
        copy: bool,
        result: Result<String, NativeError>,
    },
    Events,
    Closed,
}

/// Original hints and actual native read proof, paired with the outer update's
/// generation/invalidation. Neither hints nor a monitor snapshot prove UI apply.
#[derive(Clone)]
pub struct ReadObservation {
    /// Requested scope; the outer Update owns generation and invalidation.
    pub scope: Scope,
    /// Actual callback scope, only when it exactly matches the requested scope.
    /// Owner-wide hints are retained separately, never lent to a selected yard.
    pub hint_scope: Option<Scope>,
    pub batch: Batch,
    pub proof: Option<ReadProof>,
}
impl ReadObservation {
    fn retire(&self, observer: &Observation, reason: Retirement) {
        observer.retire(&observer.live(&self.batch), reason);
    }
}

/// Concrete pre-admission failure, including failed local intent persistence.
pub struct NotSubmitted(pub NativeError);

pub enum Details {
    Host(HostDetails),
    Yard(YardDetails),
}
impl Details {
    pub fn capabilities(&self) -> &[String] {
        match self {
            Self::Host(details) => details.capabilities(),
            Self::Yard(details) => details.capabilities(),
        }
    }
}

pub enum OwnerCommand {
    Plan {
        request: u64,
        scope: Scope,
        command: String,
        arguments: Vec<String>,
    },
    Execute {
        request: u64,
        plan: OperationPlan,
        state: AppState,
    },
    Discard {
        request: u64,
        plan_id: String,
    },
    Cancel {
        request: u64,
        operation_id: String,
    },
    Launch {
        generation: u64,
        scope: Scope,
        project: Option<String>,
        kind: String,
        copy: bool,
    },
}

fn record_operation(events: &mut HashMap<String, VerandaEvent>, id: String, event: VerandaEvent) {
    let terminal = |state: &str| matches!(state, "completed" | "failed" | "cancelled" | "unknown");
    if events.get(&id).is_some_and(|old| terminal(&old.state)) {
        return;
    }
    if events.len() == 32 && !events.contains_key(&id) {
        if !terminal(&event.state) {
            return;
        }
        // Authoritative completion must survive a storm of progress hints.
        if let Some(old) = events
            .iter()
            .find(|(_, old)| !terminal(&old.state))
            .map(|(id, _)| id.clone())
        {
            events.remove(&old);
        } else {
            return;
        }
    }
    events.insert(id, event);
}

fn save_state(
    native: &Client,
    baseline: &mut Result<Vec<u8>, NativeError>,
    state: &AppState,
) -> Result<(), NativeError> {
    let expected = baseline.as_ref().map_err(Clone::clone)?;
    let bytes = state.encode()?;
    match native.save_desktop_document(expected, &bytes) {
        Ok(()) => {
            *baseline = Ok(bytes);
            Ok(())
        }
        Err(error) => {
            *baseline = Err(error.clone());
            Err(error)
        }
    }
}

/// Callbacks are hints, never ordered inventory. A hint invalidates every read
/// started before it, even when callback delivery lags a newer native snapshot.
#[derive(Default)]
pub(crate) struct Invalidations {
    hints: HashMap<Option<String>, Hint>,
    observation: Option<Observation>,
}

#[derive(Default)]
struct Hint {
    version: u64,
    dirty: bool,
    available: bool,
    last_available: u64,
    failed_at: Option<u64>,
    observations: Option<Vec<ReadObservation>>,
}

impl Invalidations {
    pub(crate) fn hint(&mut self, connection: Option<String>, available: bool) {
        self.hint_observed(connection, None, available, None);
    }
    fn hint_observed(
        &mut self,
        connection: Option<String>,
        yard: Option<String>,
        available: bool,
        batch: Option<Batch>,
    ) {
        // Closed/retired sources must not leave an accumulating side queue in
        // inactive owners. This only removes metadata with a recorded terminal
        // reason; ordinary dirty/version/availability state is unchanged.
        if let Some(observer) = &self.observation {
            for hint in self.hints.values_mut() {
                if let Some(items) = &mut hint.observations {
                    for old in items.iter_mut() {
                        old.batch = observer.live(&old.batch);
                    }
                    items.retain(|old| !old.batch.tickets().is_empty());
                }
            }
        }
        let scope = self.observation.as_ref().map(|_| Scope {
            connection: connection.clone(),
            yard,
        });
        let entry = self.hints.entry(connection).or_default();
        entry.version = entry
            .version
            .checked_add(1)
            .expect("invalidation counter exhausted");
        entry.dirty = true;
        entry.available |= available;
        if available {
            entry.last_available = entry.version;
        }
        if let (Some(observer), Some(scope), Some(batch)) = (&self.observation, scope, batch) {
            let batch = observer.inventory(&batch);
            if !batch.tickets().is_empty() {
                let observations = entry.observations.get_or_insert_with(Vec::new);
                if let Some(old) = observations.iter_mut().find(|old| old.scope == scope) {
                    old.batch = observer.merge(&old.batch, &batch);
                } else {
                    observations.push(ReadObservation {
                        hint_scope: Some(scope.clone()),
                        scope,
                        batch,
                        proof: None,
                    });
                }
            }
        }
    }
    fn carry(&self, connection: &Option<String>, yard: &Option<String>) -> Option<ReadObservation> {
        let observer = self.observation.as_ref()?;
        let scope = Scope {
            connection: connection.clone(),
            yard: yard.clone(),
        };
        let batch = self
            .hints
            .get(connection)
            .and_then(|hint| hint.observations.as_ref())
            .and_then(|items| items.iter().find(|item| item.scope == scope))
            .map(|item| observer.live(&item.batch))
            .unwrap_or_default();
        let hint_scope = (!batch.tickets().is_empty()).then(|| scope.clone());
        Some(ReadObservation {
            scope,
            hint_scope,
            batch,
            proof: None,
        })
    }
    fn close_observations(&mut self) {
        if let Some(observer) = &self.observation {
            for hint in self.hints.values_mut() {
                for item in hint.observations.take().unwrap_or_default() {
                    item.retire(observer, Retirement::Unknown);
                }
            }
        }
    }

    pub(crate) fn version(&self, connection: &Option<String>) -> u64 {
        self.hints.get(connection).map_or(0, |entry| entry.version)
    }

    pub(crate) fn finished(&mut self, connection: &Option<String>, failed: bool) {
        let entry = self.hints.entry(connection.clone()).or_default();
        entry.failed_at = failed.then_some(entry.version);
    }

    pub(crate) fn permits_read(&self, connection: &Option<String>, explicit: bool) -> bool {
        explicit
            || self.hints.get(connection).is_none_or(|entry| {
                entry
                    .failed_at
                    .is_none_or(|failed| entry.last_available > failed)
            })
    }

    fn take(&mut self) -> Vec<(Option<String>, bool)> {
        self.hints
            .iter_mut()
            .filter_map(|(connection, entry)| {
                if std::mem::take(&mut entry.dirty) {
                    Some((connection.clone(), std::mem::take(&mut entry.available)))
                } else {
                    None
                }
            })
            .collect()
    }
}

#[derive(Default)]
struct Pending {
    observation: Option<Observation>,
    read_observation: Option<ReadObservation>,
    details_observation: Option<ReadObservation>,
    read: Option<(u64, Option<String>, Option<String>, bool)>,
    details: Option<(u64, Scope)>,
    state: Option<(u64, AppState)>,
    owner: Option<OwnerCommand>,
    save: Option<NavigationSelection>,
    trust: Option<TrustCommand>,
    cancellation: Option<u64>,
    stopped: bool,
}

enum TrustCommand {
    Assess {
        id: u64,
        destination: String,
        repair: Option<String>,
        attempt: Arc<ConnectionAttempt>,
    },
    Connect {
        id: u64,
        review: Assessment,
        attempt: Arc<ConnectionAttempt>,
    },
}

impl Pending {
    fn queue_cancel(&mut self, id: u64) {
        // A late old reaper is a separate control message, never a replacement
        // for a newer assessment/connection. Only one GUI flow can be active.
        self.cancellation = Some(self.cancellation.map_or(id, |queued| queued.max(id)));
    }
    fn read(
        &mut self,
        generation: u64,
        connection: Option<String>,
        yard: Option<String>,
        explicit: bool,
    ) {
        self.read_observed(generation, connection, yard, explicit, None);
    }
    fn read_observed(
        &mut self,
        generation: u64,
        connection: Option<String>,
        yard: Option<String>,
        explicit: bool,
        carry: Option<ReadObservation>,
    ) {
        Self::merge_carry(
            &self.observation,
            &mut self.read_observation,
            carry,
            &connection,
            &yard,
        );
        if self.read.as_ref().is_some_and(
            |(old_generation, old_connection, old_yard, old_explicit)| {
                *old_generation == generation
                    && *old_connection == connection
                    && *old_yard == yard
                    && *old_explicit
                    && !explicit
            },
        ) {
            return;
        }
        self.read = Some((generation, connection, yard, explicit));
    }
    fn merge_carry(
        observer: &Option<Observation>,
        old: &mut Option<ReadObservation>,
        next: Option<ReadObservation>,
        connection: &Option<String>,
        yard: &Option<String>,
    ) {
        let Some(observer) = observer else {
            return;
        };
        if old
            .as_ref()
            .is_some_and(|item| item.scope.connection != *connection || item.scope.yard != *yard)
        {
            old.take().unwrap().retire(observer, Retirement::Superseded);
        }
        if let Some(mut next) = next {
            if next.scope.connection != *connection || next.scope.yard != *yard {
                next.retire(observer, Retirement::Unknown);
                return;
            }
            next.batch = observer.live(&next.batch);
            if next.batch.tickets().is_empty() {
                next.hint_scope = None;
            }
            if let Some(old) = old {
                old.batch = observer.merge(&old.batch, &next.batch);
                if !next.batch.tickets().is_empty() {
                    old.hint_scope = next.hint_scope;
                }
            } else {
                *old = Some(next);
            }
        }
    }
    fn details_observed(&mut self, generation: u64, scope: Scope, carry: Option<ReadObservation>) {
        Self::merge_carry(
            &self.observation,
            &mut self.details_observation,
            carry,
            &scope.connection,
            &scope.yard,
        );
        self.details = Some((generation, scope));
    }
    fn close_observations(&mut self) {
        if let Some(observer) = &self.observation {
            for item in [
                self.read_observation.take(),
                self.details_observation.take(),
            ]
            .into_iter()
            .flatten()
            {
                item.retire(observer, Retirement::Unknown);
            }
        }
    }
}

#[derive(Default)]
struct ReviewedToken(Option<(u64, String)>);

impl ReviewedToken {
    fn replace(&mut self, id: u64, token: Option<String>) -> Option<String> {
        let previous = self.0.take().map(|(_, token)| token);
        self.0 = token.map(|token| (id, token));
        previous
    }

    fn take_for(&mut self, id: u64) -> Option<String> {
        if self.0.as_ref().is_some_and(|(owner, _)| *owner == id) {
            self.0.take().map(|(_, token)| token)
        } else {
            None
        }
    }

    fn consume_review(&mut self, id: u64, token: &str) -> bool {
        if self
            .0
            .as_ref()
            .is_some_and(|(owner, reviewed)| *owner == id && reviewed == token)
        {
            self.0 = None;
            true
        } else {
            false
        }
    }
}

fn receive_event(
    events: &Mutex<Invalidations>,
    operations: &Mutex<HashMap<String, VerandaEvent>>,
    event: VerandaEvent,
) {
    if let Some(id) = &event.operation_id {
        if let Ok(mut operations) = operations.lock() {
            record_operation(&mut operations, id.clone(), event);
        }
    } else if let Ok(mut pending) = events.lock() {
        // Monitor read proofs and snapshot values are never authoritative Worker results.
        let batch = event.observation.map(|metadata| metadata.batch);
        pending.hint_observed(
            event.connection_id,
            event.yard,
            event.snapshot.is_some(),
            batch,
        );
    }
}
fn finish_read<T>(
    result: Result<Observed<T>, NativeError>,
    mut observation: Option<ReadObservation>,
) -> (Result<T, NativeError>, Option<ReadObservation>) {
    match result {
        Ok(read) => {
            if let Some(metadata) = &mut observation {
                metadata.proof = read.proof;
            }
            (Ok(read.value), observation)
        }
        Err(error) => (Err(error), observation),
    }
}

/// One current read, preference write and trust command, independent of click/event frequency.
/// The worker sleeps on the condition variable; no idle timer or polling loop.
pub struct Worker {
    pending: Arc<(Mutex<Pending>, Condvar)>,
    events: Arc<Mutex<Invalidations>>,
    client: Arc<Client>,
    updates: Sender<Update>,
    thread: Option<JoinHandle<()>>,
    cancellation: Option<JoinHandle<()>>,
    operation_events: Arc<Mutex<HashMap<String, VerandaEvent>>>,
}

impl Worker {
    pub fn start(root: PathBuf) -> (Self, Receiver<Update>) {
        Self::start_with_observation(root, None)
    }
    pub fn observed(root: PathBuf, observation: Observation) -> (Self, Receiver<Update>) {
        Self::start_with_observation(root, Some(observation))
    }
    fn start_with_observation(
        root: PathBuf,
        observation: Option<Observation>,
    ) -> (Self, Receiver<Update>) {
        let (updates, receiver) = async_channel::bounded(8);
        let events = Arc::new(Mutex::new(Invalidations {
            observation: observation.clone(),
            ..Invalidations::default()
        }));
        let event_map = events.clone();
        let notifications = updates.clone();
        let operation_events = Arc::new(Mutex::new(HashMap::<String, VerandaEvent>::new()));
        let operations = operation_events.clone();
        let emit = Arc::new(move |event| {
            receive_event(&event_map, &operations, event);
            let _ = notifications.try_send(Update::Events);
        });
        let client = match &observation {
            Some(observer) => Client::observed(root, emit, observer.clone()),
            None => Client::new(root, emit),
        };
        let pending = Arc::new((
            Mutex::new(Pending {
                observation: observation.clone(),
                ..Pending::default()
            }),
            Condvar::new(),
        ));
        let requests = pending.clone();
        let native = client.clone();
        let replies = updates.clone();
        let invalidations = events.clone();
        let thread = thread::spawn(move || {
            let mut document = native.desktop_document();
            let state = document
                .as_ref()
                .map_err(Clone::clone)
                .and_then(|bytes| AppState::decode(bytes));
            if let Err(error) = &state {
                document = Err(error.clone());
            }
            let connections = native.connections();
            let selection = native.navigation_selection();
            let preference_error = connections
                .as_ref()
                .err()
                .cloned()
                .or_else(|| selection.as_ref().err().cloned());
            let connections = connections.unwrap_or_default();
            let selection = selection.unwrap_or(None);
            let local = native.fleet(None);
            invalidations
                .lock()
                .unwrap()
                .finished(&None, local.is_err());
            let remote = selection
                .as_ref()
                .and_then(|selection| selection.connection_id.as_ref())
                .filter(|id| connections.iter().any(|connection| &connection.id == *id))
                .map(|id| {
                    let connection = Some(id.clone());
                    let result = native.fleet(connection.clone());
                    invalidations
                        .lock()
                        .unwrap()
                        .finished(&connection, result.is_err());
                    (connection, result)
                });
            if replies
                .send_blocking(Update::Initialized {
                    connections,
                    selection,
                    local,
                    remote,
                    preference_error,
                    state,
                })
                .is_err()
            {
                return;
            }
            let mut review_token = ReviewedToken::default();
            loop {
                let (read, save, trust, cancellation, details, state, owner) = {
                    let (lock, wake) = &*requests;
                    let mut pending = lock.lock().unwrap();
                    while !pending.stopped
                        && pending.read.is_none()
                        && pending.save.is_none()
                        && pending.trust.is_none()
                        && pending.cancellation.is_none()
                        && pending.details.is_none()
                        && pending.state.is_none()
                        && pending.owner.is_none()
                    {
                        pending = wake.wait(pending).unwrap();
                    }
                    if pending.stopped {
                        if let Some((revision, state)) = pending.state.take() {
                            drop(pending);
                            let result = save_state(&native, &mut document, &state);
                            let _ = replies.send_blocking(Update::StateSaved { revision, result });
                        }
                        break;
                    }
                    (
                        pending
                            .read
                            .take()
                            .map(|read| (read, pending.read_observation.take())),
                        pending.save.take(),
                        pending.trust.take(),
                        pending.cancellation.take(),
                        pending
                            .details
                            .take()
                            .map(|details| (details, pending.details_observation.take())),
                        pending.state.take(),
                        pending.owner.take(),
                    )
                };
                if let Some((revision, state)) = state {
                    let result = save_state(&native, &mut document, &state);
                    if replies
                        .send_blocking(Update::StateSaved { revision, result })
                        .is_err()
                    {
                        break;
                    }
                }
                if let Some(owner) = owner {
                    let update = match owner {
                        OwnerCommand::Plan {
                            request,
                            scope,
                            command,
                            arguments,
                        } => Update::Planned {
                            request,
                            result: native.plan(scope.connection, scope.yard, command, arguments),
                        },
                        OwnerCommand::Execute {
                            request,
                            plan,
                            state,
                        } => {
                            let saved = save_state(&native, &mut document, &state);
                            if let Err(error) = &saved {
                                let _ = replies.send_blocking(Update::StateSaved {
                                    revision: 0,
                                    result: Err(error.clone()),
                                });
                            }
                            let result = saved.map_err(NotSubmitted).and_then(|()| {
                                native
                                    .execute_with_submission(&plan.plan_id, &plan.digest, true)
                                    .map(|started| started.operation_id().to_owned())
                                    .map_err(|error| NotSubmitted(error.into_error()))
                            });
                            if result.is_err() {
                                let _ = native.discard(&plan.plan_id);
                            }
                            Update::Executed { request, result }
                        }
                        OwnerCommand::Discard { request, plan_id } => Update::Discarded {
                            request,
                            result: native.discard(&plan_id),
                        },
                        OwnerCommand::Cancel {
                            request,
                            operation_id,
                        } => Update::OperationCancelled {
                            request,
                            result: native.cancel(&operation_id),
                        },
                        OwnerCommand::Launch {
                            generation,
                            scope,
                            project,
                            kind,
                            copy,
                        } => {
                            let result = if copy {
                                native.shell_command(
                                    scope.connection.clone(),
                                    scope.yard.clone(),
                                    project,
                                )
                            } else {
                                native.launch(
                                    scope.connection.clone(),
                                    scope.yard.clone(),
                                    project,
                                    kind,
                                )
                            };
                            Update::Launched {
                                generation,
                                scope,
                                copy,
                                result: result.map(|launch| launch.command().to_owned()),
                            }
                        }
                    };
                    if replies.send_blocking(update).is_err() {
                        break;
                    }
                }
                if let Some(id) = cancellation {
                    if let Some(token) = review_token.take_for(id) {
                        let _ = native.cancel_assessment(&token);
                    }
                    if replies.send_blocking(Update::Cancelled { id }).is_err() {
                        break;
                    }
                }
                if let Some(trust) = trust {
                    let update = match trust {
                        TrustCommand::Assess {
                            id,
                            destination,
                            repair,
                            attempt,
                        } => {
                            if let Some(token) = review_token.replace(id, None) {
                                let _ = native.cancel_assessment(&token);
                            }
                            let result = if attempt.is_cancelled() {
                                Err(cancelled())
                            } else {
                                native.assess(&destination, repair.as_deref())
                            };
                            let result = if attempt.is_cancelled() {
                                if let Ok(review) = &result {
                                    let _ = native.cancel_assessment(review.id());
                                }
                                Err(cancelled())
                            } else {
                                result
                            };
                            review_token.replace(
                                id,
                                result.as_ref().ok().map(|review| review.id().to_owned()),
                            );
                            Update::Reviewed { id, result }
                        }
                        TrustCommand::Connect {
                            id,
                            review,
                            attempt,
                        } => {
                            let result = if review_token.consume_review(id, review.id()) {
                                native.connect_attempt(
                                    review.id(),
                                    true,
                                    review.fingerprint(),
                                    &attempt,
                                )
                            } else {
                                Err(NativeError::new("stale_assessment", "The connection review is no longer current. Review the host key again."))
                            };
                            let result = if result.is_err() && attempt.is_cancelled() {
                                Err(cancelled())
                            } else {
                                result
                            };
                            Update::Connected { id, result }
                        }
                    };
                    if replies.send_blocking(update).is_err() {
                        break;
                    }
                }
                let read = read.filter(|((_, connection, _, explicit), _)| {
                    invalidations
                        .lock()
                        .unwrap()
                        .permits_read(connection, *explicit)
                });
                if let Some(((generation, connection, yard, _), observation)) = read {
                    let invalidation = invalidations.lock().unwrap().version(&connection);
                    let result = native
                        .retain_read_scope(connection.clone(), yard)
                        .and_then(|()| native.fleet_observed(connection.clone()));
                    let (result, observation) = finish_read(result, observation);
                    invalidations
                        .lock()
                        .unwrap()
                        .finished(&connection, result.is_err());
                    if replies
                        .send_blocking(Update::Fleet {
                            generation,
                            connection,
                            invalidation,
                            result,
                            observation,
                        })
                        .is_err()
                    {
                        break;
                    }
                }
                if let Some(((generation, scope), observation)) = details {
                    let invalidation = invalidations.lock().unwrap().version(&scope.connection);
                    let result = native
                        .retain_read_scope(scope.connection.clone(), scope.yard.clone())
                        .and_then(|()| match &scope.yard {
                            Some(yard) => native
                                .yard_observed(scope.connection.clone(), yard.clone())
                                .map(|read| Observed {
                                    value: Details::Yard(read.value),
                                    proof: read.proof,
                                }),
                            None => native.host_observed(scope.connection.clone()).map(|read| {
                                Observed {
                                    value: Details::Host(read.value),
                                    proof: read.proof,
                                }
                            }),
                        });
                    let (result, observation) = finish_read(result, observation);
                    invalidations
                        .lock()
                        .unwrap()
                        .finished(&scope.connection, result.is_err());
                    if replies
                        .send_blocking(Update::Details {
                            generation,
                            scope,
                            invalidation,
                            result,
                            observation,
                        })
                        .is_err()
                    {
                        break;
                    }
                }
                if let Some(selection) = save {
                    let result = native.save_navigation_selection(&selection);
                    if replies
                        .send_blocking(Update::Saved { selection, result })
                        .is_err()
                    {
                        break;
                    }
                }
            }
            if let Some((_, token)) = review_token.0 {
                let _ = native.cancel_assessment(&token);
            }
        });
        (
            Self {
                pending,
                events,
                client,
                updates,
                thread: Some(thread),
                cancellation: None,
                operation_events,
            },
            receiver,
        )
    }

    pub fn assess(
        &self,
        id: u64,
        destination: String,
        repair: Option<String>,
        attempt: Arc<ConnectionAttempt>,
    ) {
        self.trust(TrustCommand::Assess {
            id,
            destination,
            repair,
            attempt,
        });
    }

    pub fn connect(&self, id: u64, review: Assessment, attempt: Arc<ConnectionAttempt>) {
        self.trust(TrustCommand::Connect {
            id,
            review,
            attempt,
        });
    }

    pub fn cancel(&mut self, id: u64, attempt: Arc<ConnectionAttempt>) {
        // At most one cancellation can be live: GTK keeps the flow busy until
        // its Cancelled reply. Reaping never runs on the GTK thread.
        // The previous thread has already queued its final reply before GTK
        // can start another flow. Drop its handle without a main-thread wait.
        self.cancellation.take();
        let pending = self.pending.clone();
        self.cancellation = Some(thread::spawn(move || {
            attempt.cancel();
            let (lock, wake) = &*pending;
            let mut pending = lock.lock().unwrap();
            if !pending.stopped {
                pending.queue_cancel(id);
                wake.notify_one();
            }
        }));
    }

    fn trust(&self, command: TrustCommand) {
        let (lock, wake) = &*self.pending;
        let mut pending = lock.lock().unwrap();
        if !pending.stopped {
            pending.trust = Some(command);
            wake.notify_one();
        }
    }

    pub fn read(
        &self,
        generation: u64,
        connection: Option<String>,
        yard: Option<String>,
        explicit: bool,
    ) -> bool {
        let observation = {
            let events = self.events.lock().unwrap();
            if !events.permits_read(&connection, explicit) {
                return false;
            }
            events.carry(&connection, &yard)
        };
        let (lock, wake) = &*self.pending;
        let mut pending = lock.lock().unwrap();
        if !pending.stopped {
            pending.read_observed(generation, connection, yard, explicit, observation);
            wake.notify_one();
            return true;
        }
        false
    }

    pub fn details(&self, generation: u64, scope: Scope) {
        let observation = self
            .events
            .lock()
            .unwrap()
            .carry(&scope.connection, &scope.yard);
        let (lock, wake) = &*self.pending;
        let mut pending = lock.lock().unwrap();
        if !pending.stopped {
            pending.details_observed(generation, scope, observation);
            wake.notify_one();
        }
    }
    pub fn save_state(&self, revision: u64, state: AppState) {
        let (lock, wake) = &*self.pending;
        let mut pending = lock.lock().unwrap();
        if !pending.stopped {
            pending.state = Some((revision, state));
            wake.notify_one();
        }
    }
    pub fn owner(&self, command: OwnerCommand) -> bool {
        let (lock, wake) = &*self.pending;
        let mut pending = lock.lock().unwrap();
        if pending.stopped || pending.owner.is_some() {
            return false;
        }
        pending.owner = Some(command);
        wake.notify_one();
        true
    }
    pub fn take_operations(&self) -> Vec<VerandaEvent> {
        self.operation_events
            .lock()
            .unwrap()
            .drain()
            .map(|(_, event)| event)
            .collect()
    }
    pub fn save(&self, selection: NavigationSelection) {
        let (lock, wake) = &*self.pending;
        let mut pending = lock.lock().unwrap();
        if !pending.stopped {
            pending.save = Some(selection);
            wake.notify_one();
        }
    }

    pub fn take_events(&self) -> Vec<(Option<String>, bool)> {
        self.events
            .lock()
            .map(|mut events| events.take())
            .unwrap_or_default()
    }

    pub fn read_is_current(&self, connection: &Option<String>, invalidation: u64) -> bool {
        self.events.lock().unwrap().version(connection) == invalidation
    }

    /// Interrupt native I/O and reap tracked engine/SSH children off the GTK
    /// thread. Closed is emitted only after both shutdown and the worker join.
    pub fn close(&mut self) {
        let Some(worker) = self.thread.take() else {
            return;
        };
        let (lock, wake) = &*self.pending;
        let final_selection = {
            let mut pending = lock.lock().unwrap();
            pending.stopped = true;
            pending.close_observations();
            pending.save.take()
        };
        self.events.lock().unwrap().close_observations();
        wake.notify_one();
        let native = self.client.clone();
        let replies = self.updates.clone();
        let cancellation = self.cancellation.take();
        thread::spawn(move || {
            native.shutdown();
            if let Some(cancellation) = cancellation {
                let _ = cancellation.join();
            }
            let _ = worker.join();
            if let Some(selection) = final_selection {
                let result = native.save_navigation_selection(&selection);
                let _ = replies.send_blocking(Update::Saved { selection, result });
            }
            let _ = replies.send_blocking(Update::Closed);
        });
    }
}

fn cancelled() -> NativeError {
    NativeError::new(
        "cancelled",
        "The connection was cancelled. No host was registered.",
    )
}

impl Drop for Worker {
    fn drop(&mut self) {
        self.close();
    }
}

#[cfg(test)]
mod tests {
    #[test]
    fn authoritative_terminal_survives_progress_storm_and_late_hint() {
        let mut events = std::collections::HashMap::new();
        let event = |id: &str, state: &str| veranda_lib::client::VerandaEvent {
            connection_id: None,
            yard: None,
            operation_id: Some(id.into()),
            state: state.into(),
            message: None,
            snapshot: None,
            observation: None,
        };
        for index in 0..32 {
            let id = format!("operation-{index}");
            super::record_operation(&mut events, id.clone(), event(&id, "running"));
        }
        super::record_operation(&mut events, "active".into(), event("active", "unknown"));
        assert_eq!(events.len(), 32);
        super::record_operation(&mut events, "active".into(), event("active", "running"));
        assert_eq!(events["active"].state, "unknown");
    }

    use super::*;

    #[test]
    fn old_cancel_preserves_new_assessment_and_its_review() {
        let mut pending = Pending::default();
        pending.trust = Some(TrustCommand::Assess {
            id: 2,
            destination: "user@example.test".into(),
            repair: None,
            attempt: Arc::new(ConnectionAttempt::default()),
        });
        // Old reaper finishes after the registration result allowed a new flow.
        pending.queue_cancel(1);
        assert!(
            matches!(
                pending.trust.as_ref(),
                Some(TrustCommand::Assess { id: 2, .. })
            ),
            "old cancellation overwrote a new assessment"
        );
        assert_eq!(pending.cancellation.take(), Some(1));
        let mut reviewed = ReviewedToken::default();
        reviewed.replace(2, Some("new-assessment".into()));
        assert!(
            reviewed.take_for(1).is_none(),
            "old cancellation consumed a new review"
        );
        assert_eq!(reviewed.take_for(2).as_deref(), Some("new-assessment"));
    }

    #[test]
    fn old_cancel_after_new_review_does_not_revoke_it() {
        let mut reviewed = ReviewedToken::default();
        reviewed.replace(2, Some("new-assessment".into()));
        assert!(
            reviewed.take_for(1).is_none(),
            "old cancellation consumed a new review"
        );
        assert_eq!(reviewed.take_for(2).as_deref(), Some("new-assessment"));
        assert!(reviewed.take_for(2).is_none());
    }

    #[test]
    fn superseded_connect_cannot_consume_a_newer_or_different_review() {
        let mut reviewed = ReviewedToken::default();
        reviewed.replace(2, Some("new-assessment".into()));
        assert!(!reviewed.consume_review(1, "old-assessment"));
        assert!(!reviewed.consume_review(2, "old-assessment"));
        assert!(reviewed.consume_review(2, "new-assessment"));
        assert!(!reviewed.consume_review(2, "new-assessment"));
    }

    #[test]
    fn failed_open_discards_automatic_retry_from_its_own_hints() {
        let mut hints = Invalidations::default();
        let mut pending = Pending::default();
        hints.hint(None, true); // Initial session snapshot during opening.
        hints.hint(None, false); // Same opening then loses transport.
        pending.read(0, None, None, false); // UI scheduled before receiving its error.
        hints.finished(&None, true);
        let (_, connection, _, explicit) = pending.read.take().unwrap();
        assert!(!hints.permits_read(&connection, explicit));
        assert!(hints.permits_read(&None, true)); // Explicit Refresh is allowed.
        assert_eq!(hints.take(), vec![(None, true)]);
        assert!(hints.take().is_empty()); // No idle notification/read feedback.
        hints.hint(None, false);
        assert!(!hints.permits_read(&None, false));
        hints.hint(None, true); // Recovery hint after failed read completion.
        assert!(hints.permits_read(&None, false));
        hints.finished(&None, false);
        let quiet = hints.version(&None);
        assert_eq!(quiet, hints.version(&None));
        hints.take();
        assert!(hints.take().is_empty());
        assert!(pending.read.is_none());
    }

    #[test]
    fn cached_details_stay_invalid_after_inventory_refresh_but_ignore_inactive_owner_hints() {
        let mut hints = Invalidations::default();
        let cached_details = hints.version(&None);
        hints.hint(Some("inactive".into()), true);
        assert_eq!(hints.version(&None), cached_details);
        hints.hint(None, true);
        assert_ne!(hints.version(&None), cached_details);
        hints.finished(&None, false); // Inventory became available; old details did not.
        hints.take();
        assert_ne!(hints.version(&None), cached_details);
        let refreshed_details = hints.version(&None);
        hints.hint(None, false);
        assert_ne!(hints.version(&None), refreshed_details);
    }

    #[test]
    fn coalesced_hints_preserve_explicit_refresh_and_new_selection() {
        let mut pending = Pending::default();
        pending.read(1, None, None, true);
        for _ in 0..100 {
            pending.read(1, None, None, false);
        }
        assert_eq!(pending.read, Some((1, None, None, true)));
        pending.read(2, Some("remote".into()), None, false);
        assert_eq!(pending.read, Some((2, Some("remote".into()), None, false)));
    }
    fn observed_carry(scope: Scope, batch: Batch) -> ReadObservation {
        ReadObservation {
            hint_scope: Some(scope.clone()),
            scope,
            batch,
            proof: None,
        }
    }
    #[test]
    fn observation_pending_preserves_starts_refresh_generation_and_supersedes_only_old_scope() {
        use veranda_lib::observation::SourceKind;
        let observation = Observation::new();
        let local = observation.source(SourceKind::Local);
        let first = local.request().unwrap();
        let scope = Scope {
            connection: None,
            yard: None,
        };
        let mut pending = Pending {
            observation: Some(observation.clone()),
            ..Pending::default()
        };
        pending.read_observed(
            1,
            None,
            None,
            true,
            Some(observed_carry(scope.clone(), local.batch())),
        );
        local.request().unwrap();
        pending.read_observed(
            1,
            None,
            None,
            false,
            Some(observed_carry(scope.clone(), local.batch())),
        );
        assert_eq!(pending.read, Some((1, None, None, true)));
        assert_eq!(
            pending
                .read_observation
                .as_ref()
                .unwrap()
                .batch
                .tickets()
                .len(),
            2
        );
        assert_eq!(
            pending.read_observation.as_ref().unwrap().batch.tickets()[0].started(),
            first.started()
        );
        pending.read_observed(
            2,
            None,
            None,
            false,
            Some(observed_carry(scope, local.batch())),
        );
        assert_eq!(pending.read.as_ref().unwrap().0, 2);
        assert_eq!(
            pending.read_observation.as_ref().unwrap().batch.tickets()[0].started(),
            first.started()
        );
        let remote = observation.source(SourceKind::Remote);
        let remote_ticket = remote.request().unwrap();
        let next_scope = Scope {
            connection: Some("synthetic-other".into()),
            yard: None,
        };
        pending.read_observed(
            3,
            next_scope.connection.clone(),
            None,
            false,
            Some(observed_carry(next_scope.clone(), remote.batch())),
        );
        assert!(local.batch().tickets().is_empty());
        assert_eq!(
            pending.read_observation.as_ref().unwrap().batch.tickets()[0].ordinal(),
            remote_ticket.ordinal()
        );
        local.request().unwrap();
        pending.read_observed(
            3,
            next_scope.connection.clone(),
            None,
            false,
            Some(observed_carry(
                Scope {
                    connection: None,
                    yard: None,
                },
                local.batch(),
            )),
        );
        assert!(
            local.batch().tickets().is_empty(),
            "wrong requested scope is explicitly unknown, never lent"
        );
        assert_eq!(pending.read_observation.as_ref().unwrap().scope, next_scope);
        assert_eq!(observation.counts().pending, 1);
        pending.close_observations();
        assert_eq!(observation.counts().pending, 0);
    }
    #[test]
    fn observation_callback_filters_foreign_context_without_changing_owner_invalidation_or_operations(
    ) {
        use veranda_lib::observation::{SourceKind, UpdateObservation};
        let observation = Observation::new();
        let foreign = Observation::new();
        let source = foreign.source(SourceKind::Remote);
        source.request().unwrap();
        let hints = Mutex::new(Invalidations {
            observation: Some(observation.clone()),
            ..Invalidations::default()
        });
        let operations = Mutex::new(HashMap::new());
        let event = |operation_id| VerandaEvent {
            connection_id: None,
            yard: Some("synthetic-yard".into()),
            operation_id,
            state: "running".into(),
            message: None,
            snapshot: None,
            observation: Some(UpdateObservation {
                batch: source.batch(),
                read: None,
            }),
        };
        receive_event(&hints, &operations, event(None));
        assert_eq!(hints.lock().unwrap().version(&None), 1);
        assert_eq!(hints.lock().unwrap().take(), vec![(None, false)]);
        let carry = hints
            .lock()
            .unwrap()
            .carry(&None, &Some("synthetic-yard".into()))
            .unwrap();
        assert!(
            carry.batch.tickets().is_empty() && carry.hint_scope.is_none() && carry.proof.is_none()
        );
        assert_eq!(observation.counts().rejected_batch_tickets, 1);
        receive_event(
            &hints,
            &operations,
            event(Some("operation-synthetic".into())),
        );
        assert_eq!(operations.lock().unwrap().len(), 1);
        assert_eq!(
            hints.lock().unwrap().version(&None),
            1,
            "operation dispatch route unchanged"
        );
    }
}

#[cfg(all(test, unix))]
#[path = "worker_tests.rs"]
mod reopen_tests;
