use async_channel::{Receiver, Sender};
use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::{Arc, Condvar, Mutex};
use std::thread::{self, JoinHandle};
use veranda_lib::client::Client;
use veranda_lib::connections::{ConnectionSummary, NavigationSelection};
use veranda_lib::local_fleet::{LocalFleetSnapshot, NativeError};

pub enum Update {
    Initialized {
        connections: Vec<ConnectionSummary>,
        selection: Option<NavigationSelection>,
        local: Result<LocalFleetSnapshot, NativeError>,
        remote: Option<(Option<String>, Result<LocalFleetSnapshot, NativeError>)>,
        preference_error: Option<NativeError>,
    },
    Fleet {
        generation: u64,
        connection: Option<String>,
        invalidation: u64,
        result: Result<LocalFleetSnapshot, NativeError>,
    },
    Saved {
        selection: NavigationSelection,
        result: Result<(), NativeError>,
    },
    Events,
    Closed,
}

/// Callbacks are hints, never ordered inventory. A hint invalidates every read
/// started before it, even when callback delivery lags a newer native snapshot.
#[derive(Default)]
pub(crate) struct Invalidations(HashMap<Option<String>, Hint>);

#[derive(Default)]
struct Hint {
    version: u64,
    dirty: bool,
    available: bool,
    last_available: u64,
    failed_at: Option<u64>,
}

impl Invalidations {
    pub(crate) fn hint(&mut self, connection: Option<String>, available: bool) {
        let entry = self.0.entry(connection).or_default();
        entry.version = entry
            .version
            .checked_add(1)
            .expect("invalidation counter exhausted");
        entry.dirty = true;
        entry.available |= available;
        if available {
            entry.last_available = entry.version;
        }
    }

    pub(crate) fn version(&self, connection: &Option<String>) -> u64 {
        self.0.get(connection).map_or(0, |entry| entry.version)
    }

    pub(crate) fn finished(&mut self, connection: &Option<String>, failed: bool) {
        let entry = self.0.entry(connection.clone()).or_default();
        entry.failed_at = failed.then_some(entry.version);
    }

    pub(crate) fn permits_read(&self, connection: &Option<String>, explicit: bool) -> bool {
        explicit
            || self.0.get(connection).is_none_or(|entry| {
                entry
                    .failed_at
                    .is_none_or(|failed| entry.last_available > failed)
            })
    }

    fn take(&mut self) -> Vec<(Option<String>, bool)> {
        self.0
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
    read: Option<(u64, Option<String>, bool)>,
    save: Option<NavigationSelection>,
    stopped: bool,
}

impl Pending {
    fn read(&mut self, generation: u64, connection: Option<String>, explicit: bool) {
        if self
            .read
            .as_ref()
            .is_some_and(|(old_generation, old_connection, old_explicit)| {
                *old_generation == generation
                    && *old_connection == connection
                    && *old_explicit
                    && !explicit
            })
        {
            return;
        }
        self.read = Some((generation, connection, explicit));
    }
}

/// One current read and preference write, independent of click/event frequency.
/// The worker sleeps on the condition variable; no idle timer or polling loop.
pub struct Worker {
    pending: Arc<(Mutex<Pending>, Condvar)>,
    events: Arc<Mutex<Invalidations>>,
    client: Arc<Client>,
    updates: Sender<Update>,
    thread: Option<JoinHandle<()>>,
}

impl Worker {
    pub fn start(root: PathBuf) -> (Self, Receiver<Update>) {
        let (updates, receiver) = async_channel::bounded(8);
        let events = Arc::new(Mutex::new(Invalidations::default()));
        let event_map = events.clone();
        let notifications = updates.clone();
        let client = Client::new(
            root,
            Arc::new(move |event| {
                // Owner inventory is the source of this slice. Detail/operation
                // events belong to later surfaces and cannot overwrite owner state.
                if event.yard.is_some() || event.operation_id.is_some() {
                    return;
                }
                if let Ok(mut pending) = event_map.lock() {
                    // The client store permits at most128 saved owners plus local.
                    // Snapshot presence permits recovery after a failed read;
                    // its contents and callback order never become displayed state.
                    pending.hint(event.connection_id, event.snapshot.is_some());
                }
                let _ = notifications.try_send(Update::Events);
            }),
        );
        let pending = Arc::new((Mutex::new(Pending::default()), Condvar::new()));
        let requests = pending.clone();
        let native = client.clone();
        let replies = updates.clone();
        let invalidations = events.clone();
        let thread = thread::spawn(move || {
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
                })
                .is_err()
            {
                return;
            }
            loop {
                let (read, save) = {
                    let (lock, wake) = &*requests;
                    let mut pending = lock.lock().unwrap();
                    while !pending.stopped && pending.read.is_none() && pending.save.is_none() {
                        pending = wake.wait(pending).unwrap();
                    }
                    if pending.stopped {
                        break;
                    }
                    (pending.read.take(), pending.save.take())
                };
                let read = read.filter(|(_, connection, explicit)| {
                    invalidations
                        .lock()
                        .unwrap()
                        .permits_read(connection, *explicit)
                });
                if let Some((generation, connection, _)) = read {
                    let invalidation = invalidations.lock().unwrap().version(&connection);
                    let result = native
                        .retain_read_scope(connection.clone(), None)
                        .and_then(|()| native.fleet(connection.clone()));
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
        });
        (
            Self {
                pending,
                events,
                client,
                updates,
                thread: Some(thread),
            },
            receiver,
        )
    }

    pub fn read(&self, generation: u64, connection: Option<String>, explicit: bool) -> bool {
        if !self
            .events
            .lock()
            .unwrap()
            .permits_read(&connection, explicit)
        {
            return false;
        }
        let (lock, wake) = &*self.pending;
        let mut pending = lock.lock().unwrap();
        if !pending.stopped {
            pending.read(generation, connection, explicit);
            wake.notify_one();
            return true;
        }
        false
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
            pending.save.take()
        };
        wake.notify_one();
        let native = self.client.clone();
        let replies = self.updates.clone();
        thread::spawn(move || {
            native.shutdown();
            let _ = worker.join();
            if let Some(selection) = final_selection {
                let result = native.save_navigation_selection(&selection);
                let _ = replies.send_blocking(Update::Saved { selection, result });
            }
            let _ = replies.send_blocking(Update::Closed);
        });
    }
}

impl Drop for Worker {
    fn drop(&mut self) {
        self.close();
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn failed_open_discards_automatic_retry_from_its_own_hints() {
        let mut hints = Invalidations::default();
        let mut pending = Pending::default();
        hints.hint(None, true); // Initial session snapshot during opening.
        hints.hint(None, false); // Same opening then loses transport.
        pending.read(0, None, false); // UI scheduled before receiving its error.
        hints.finished(&None, true);
        let (_, connection, explicit) = pending.read.take().unwrap();
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
    fn coalesced_hints_preserve_explicit_refresh_and_new_selection() {
        let mut pending = Pending::default();
        pending.read(1, None, true);
        for _ in 0..100 {
            pending.read(1, None, false);
        }
        assert_eq!(pending.read, Some((1, None, true)));
        pending.read(2, Some("remote".into()), false);
        assert_eq!(pending.read, Some((2, Some("remote".into()), false)));
    }
}
