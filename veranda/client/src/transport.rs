//! Bounded Yard RPC sessions. Only the native shell owns process handles and frames.
use crate::child_usage::{ChildKind, OwnedChild, UsageContext};
use crate::diagnostics::{Class, DiagnosticLog};
use crate::local_fleet::{read_frame_captured, write_frame, NativeError};
use crate::observation::{ReadProof, Source, Ticket, ValidatedEvent};
use crate::vocabulary::{ErrorCode, PROTOCOL_VERSION};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use std::collections::HashMap;
use std::io::Write;
use std::process::{Command, Stdio};
use std::sync::{
    atomic::{AtomicBool, AtomicU64, Ordering},
    mpsc, Arc, Mutex, Weak,
};
use std::thread::{self, JoinHandle};
use std::time::{Duration, Instant};

const MAX_PENDING: usize = 64;
pub type Notify = Arc<dyn Fn(Event) + Send + Sync>;
type SentRequest = (
    String,
    mpsc::Receiver<Result<Value, NativeError>>,
    Option<ReadProof>,
);

struct ChildObservation {
    source: Option<Source>,
    usage: Option<UsageContext>,
    kind: ChildKind,
    diagnostics: Option<Arc<DiagnosticLog>>,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct Event {
    pub operation_id: String,
    pub event: String,
    pub sequence: u64,
    pub revision: u64,
    pub stale: bool,
    #[serde(skip)]
    pub receipt: Option<Ticket>,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Frame {
    version: u32,
    #[serde(rename = "type")]
    kind: String,
    #[serde(default)]
    id: String,
    #[serde(default)]
    operation_id: String,
    #[serde(default)]
    event: String,
    #[serde(default)]
    sequence: u64,
    #[serde(default)]
    revision: u64,
    result: Option<Value>,
    error: Option<Fault>,
}

#[derive(Deserialize)]
struct Fault {
    code: String,
    #[serde(default)]
    data: Option<Value>,
}

struct Shared {
    pending: Mutex<HashMap<String, Pending>>,
    closed: AtomicBool,
    /// Set only when this client closes the session, so the failures that
    /// closing causes are not logged.
    closing: AtomicBool,
    stale: AtomicBool,
    sequence: AtomicU64,
    diagnostics: Option<Arc<DiagnosticLog>>,
}

impl Shared {
    fn record(&self, class: Class, method: &str, code: &str, request: &str) {
        if let Some(log) = &self.diagnostics {
            if !self.closing.load(Ordering::Acquire) {
                log.record(class, method, code, request);
            }
        }
    }
    /// Logs a local failure whose cause the operator does not see.
    fn record_failure(&self, method: &str, request: &str, error: &NativeError) {
        let class = match error.code.as_str() {
            "disconnected" => Class::Disconnected,
            "rpc_timeout" => Class::RpcTimeout,
            "invalid_response" => Class::InvalidResponse,
            _ => return,
        };
        self.record(class, method, &error.code, request);
    }
}

struct Pending {
    operation_id: String,
    method: String,
    sender: mpsc::SyncSender<Result<Value, NativeError>>,
}
pub struct Call {
    id: String,
    operation_id: String,
    method: String,
    receiver: mpsc::Receiver<Result<Value, NativeError>>,
}

pub struct RpcClient {
    child: Arc<Mutex<Option<OwnedChild>>>,
    outgoing: Mutex<Option<mpsc::SyncSender<Value>>>,
    shared: Arc<Shared>,
    next_id: AtomicU64,
    reader: Mutex<Option<JoinHandle<()>>>,
    writer: Mutex<Option<JoinHandle<()>>>,
    pub engine_version: String,
    pub capabilities: Vec<String>,
    observation: Option<Source>,
}

/// Includes children that are still negotiating and therefore not in a session cache yet.
#[derive(Default)]
pub struct Processes {
    children: Mutex<Vec<Weak<Mutex<Option<OwnedChild>>>>>,
    stopped: AtomicBool,
}
impl Processes {
    #[cfg(test)]
    pub(crate) fn tracked_count_for_test(&self) -> usize {
        self.children
            .lock()
            .unwrap()
            .iter()
            .filter_map(Weak::upgrade)
            .filter(|child| child.lock().unwrap().is_some())
            .count()
    }
    pub fn stop_accepting(&self) {
        self.stopped.store(true, Ordering::Release);
    }
    fn track(&self, child: &Arc<Mutex<Option<OwnedChild>>>) -> Result<(), NativeError> {
        let mut children = self.children.lock().map_err(|_| disconnected())?;
        children.retain(|child| child.strong_count() != 0);
        if self.stopped.load(Ordering::Acquire) || children.len() >= 32 {
            if let Ok(mut child) = child.lock() {
                if let Some(mut child) = child.take() {
                    stop_child(&mut child);
                }
            }
            return Err(disconnected());
        }
        children.push(Arc::downgrade(child));
        Ok(())
    }
    pub fn shutdown(&self) {
        self.stopped.store(true, Ordering::Release);
        if let Ok(mut children) = self.children.lock() {
            for child in children.drain(..).filter_map(|child| child.upgrade()) {
                if let Ok(mut child) = child.lock() {
                    if let Some(mut child) = child.take() {
                        stop_child(&mut child);
                    }
                }
            }
        }
    }
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Negotiation {
    version: u32,
    protocol_min: u32,
    protocol_max: u32,
    engine_version: String,
    capabilities: Vec<String>,
}

impl RpcClient {
    #[cfg(test)]
    pub fn open(
        command: Command,
        expected_version: &str,
        notify: Notify,
    ) -> Result<Self, NativeError> {
        Self::open_tracked(command, expected_version, notify, None)
    }
    pub fn open_tracked(
        command: Command,
        expected_version: &str,
        notify: Notify,
        processes: Option<&Processes>,
    ) -> Result<Self, NativeError> {
        Self::open_tracked_attempt(command, expected_version, notify, processes, None)
    }

    pub(crate) fn open_tracked_attempt(
        command: Command,
        expected_version: &str,
        notify: Notify,
        processes: Option<&Processes>,
        attempt: Option<&Processes>,
    ) -> Result<Self, NativeError> {
        Self::open_tracked_attempt_usage(
            command,
            expected_version,
            notify,
            processes,
            attempt,
            None,
            None,
        )
    }
    pub(crate) fn open_tracked_attempt_usage(
        command: Command,
        expected_version: &str,
        notify: Notify,
        processes: Option<&Processes>,
        attempt: Option<&Processes>,
        usage: Option<UsageContext>,
        diagnostics: Option<Arc<DiagnosticLog>>,
    ) -> Result<Self, NativeError> {
        Self::open_with_observation(
            command,
            expected_version,
            notify,
            processes,
            attempt,
            ChildObservation {
                source: None,
                usage,
                kind: ChildKind::ProvisionalRpc,
                diagnostics,
            },
        )
    }
    pub(crate) fn open_observed(
        command: Command,
        expected_version: &str,
        notify: Notify,
        processes: Option<&Processes>,
        observation: Option<Source>,
        diagnostics: Option<Arc<DiagnosticLog>>,
    ) -> Result<Self, NativeError> {
        let usage = observation.as_ref().and_then(Source::child_usage_context);
        Self::open_with_observation(
            command,
            expected_version,
            notify,
            processes,
            None,
            ChildObservation {
                source: observation,
                usage,
                kind: ChildKind::RpcSession,
                diagnostics,
            },
        )
    }
    fn open_with_observation(
        mut command: Command,
        expected_version: &str,
        notify: Notify,
        processes: Option<&Processes>,
        attempt: Option<&Processes>,
        child_observation: ChildObservation,
    ) -> Result<Self, NativeError> {
        let ChildObservation {
            source: observation,
            usage,
            kind: child_kind,
            diagnostics,
        } = child_observation;
        #[cfg(unix)]
        {
            use std::os::unix::process::CommandExt;
            command.process_group(0);
        }
        command
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::null());
        let mut child = OwnedChild::spawn(&mut command, usage.as_ref(), child_kind).map_err(|_| {
            if let Some(log) = &diagnostics {
                log.record(Class::TransportUnavailable, "rpc.negotiate", "transport_unavailable", "");
            }
            NativeError::new("transport_unavailable", "Veranda could not start Yard or SSH. Check the installed tools and authentication reference.")
        })?;
        let mut input = child.take_stdin().ok_or_else(disconnected)?;
        let mut output = child.take_stdout().ok_or_else(disconnected)?;
        let shared = Arc::new(Shared {
            pending: Mutex::new(HashMap::new()),
            closed: AtomicBool::new(false),
            closing: AtomicBool::new(false),
            stale: AtomicBool::new(false),
            sequence: AtomicU64::new(0),
            diagnostics,
        });
        let reader_observation = observation.clone();
        let reader_shared = shared.clone();
        let reader = thread::spawn(move || {
            loop {
                let capture = reader_observation
                    .as_ref()
                    .is_some_and(|source| source.event_tagger().is_some());
                let (frame, payload): (Frame, _) = match read_frame_captured(&mut output, capture) {
                    Ok(frame) => frame,
                    Err(_) => break,
                };
                if frame.version != PROTOCOL_VERSION {
                    break;
                }
                match frame.kind.as_str() {
                    "response" => {
                        if !identity(&frame.id) || frame.result.is_some() == frame.error.is_some() {
                            break;
                        }
                        let Ok(target) = reader_shared
                            .pending
                            .lock()
                            .map(|mut pending| pending.remove(&frame.id))
                        else {
                            break;
                        };
                        if let Some(target) = target {
                            if frame.operation_id != target.operation_id {
                                // The waiter logs the invalid response.
                                let _ = target.sender.try_send(Err(invalid()));
                                break;
                            }
                            let response = match frame.error {
                                Some(error) => Err(owner_fault(
                                    &error,
                                    &reader_shared,
                                    &target.method,
                                    &frame.id,
                                )),
                                None => Ok(frame.result.unwrap_or(Value::Null)),
                            };
                            let _ = target.sender.try_send(response);
                        }
                    }
                    "event" => {
                        if !identity(&frame.event)
                            || (!frame.operation_id.is_empty() && !identity(&frame.operation_id))
                            || frame.sequence == 0
                            || frame.revision != frame.sequence
                        {
                            break;
                        }
                        let started = Instant::now();
                        let receipt = reader_observation.as_ref().and_then(|source| {
                            let reserved = source.reserve_event(frame.sequence, started);
                            let tag = payload.as_deref().and_then(|raw| {
                                source.classify_event(ValidatedEvent {
                                    event: &frame.event,
                                    operation_id: &frame.operation_id,
                                    sequence: frame.sequence,
                                    revision: frame.revision,
                                    received: started,
                                    frame: raw,
                                })
                            });
                            source.finish_event(reserved, tag)
                        });
                        let previous = reader_shared
                            .sequence
                            .swap(frame.sequence, Ordering::AcqRel);
                        let gap = frame.sequence != previous.saturating_add(1);
                        if gap {
                            reader_shared.stale.store(true, Ordering::Release);
                        }
                        notify(Event {
                            operation_id: frame.operation_id,
                            event: frame.event,
                            sequence: frame.sequence,
                            revision: frame.revision,
                            stale: gap,
                            receipt,
                        });
                    }
                    _ => break,
                }
            }
            reader_shared.closed.store(true, Ordering::Release);
            if let Ok(mut pending) = reader_shared.pending.lock() {
                for (_, waiter) in pending.drain() {
                    let _ = waiter.sender.try_send(Err(disconnected()));
                }
            }
            notify(Event {
                operation_id: String::new(),
                event: "transport.disconnected".into(),
                sequence: 0,
                revision: 0,
                stale: true,
                receipt: None,
            });
        });
        let (outgoing, writes) = mpsc::sync_channel::<Value>(16);
        let writer_shared = shared.clone();
        let writer = thread::spawn(move || {
            while let Ok(request) = writes.recv() {
                if write_frame(&mut input, &request).is_err() || input.flush().is_err() {
                    writer_shared.closed.store(true, Ordering::Release);
                    if let Ok(mut pending) = writer_shared.pending.lock() {
                        for (_, waiter) in pending.drain() {
                            let _ = waiter.sender.try_send(Err(disconnected()));
                        }
                    }
                    break;
                }
            }
        });
        let mut client = Self {
            child: Arc::new(Mutex::new(Some(child))),
            outgoing: Mutex::new(Some(outgoing)),
            shared,
            next_id: AtomicU64::new(1),
            reader: Mutex::new(Some(reader)),
            writer: Mutex::new(Some(writer)),
            engine_version: String::new(),
            capabilities: Vec::new(),
            observation,
        };
        if let Some(processes) = processes {
            processes.track(&client.child)?;
        }
        if let Some(attempt) = attempt {
            attempt.track(&client.child)?;
        }
        let call = client.begin("rpc.negotiate", "", json!({}))?;
        let request = call.id.clone();
        let raw = client
            .wait_unrecorded(call, Duration::from_secs(8))
            .inspect_err(|error| {
                client
                    .shared
                    .record_failure("rpc.negotiate", &request, error)
            })?;
        let negotiation: Negotiation = client.decode("rpc.negotiate", raw)?;
        if negotiation.version != PROTOCOL_VERSION
            || negotiation.protocol_min > PROTOCOL_VERSION
            || negotiation.protocol_max < PROTOCOL_VERSION
            || !version(&negotiation.engine_version)
            || negotiation.capabilities.len() > 128
            || negotiation
                .capabilities
                .iter()
                .any(|capability| !identity(capability))
        {
            client.shared.record(
                Class::InvalidResponse,
                "rpc.negotiate",
                "invalid_response",
                &request,
            );
            return Err(invalid());
        }
        if negotiation.engine_version != expected_version {
            client.shared.record(
                Class::IncompatibleEngine,
                "rpc.negotiate",
                "incompatible_engine",
                &request,
            );
            return Err(NativeError::new(
                "incompatible_engine",
                format!(
                    "Veranda {expected_version} and Subyard {} are incompatible. Install both from the same release.",
                    negotiation.engine_version
                ),
            ));
        }
        client.engine_version = negotiation.engine_version;
        client.capabilities = negotiation.capabilities;
        client.require("owner-inventory-v1").inspect_err(|_| {
            client.shared.record(
                Class::IncompatibleEngine,
                "rpc.negotiate",
                "capability_missing",
                &request,
            );
        })?;
        Ok(client)
    }

    pub fn require(&self, capability: &str) -> Result<(), NativeError> {
        if self.capabilities.iter().any(|value| value == capability) {
            Ok(())
        } else {
            Err(NativeError::new(
                "capability_missing",
                format!(
                    "Subyard {} does not support {capability}. Install Veranda and Subyard from the same release.",
                    self.engine_version
                ),
            ))
        }
    }

    pub fn call(
        &self,
        method: &str,
        operation_id: &str,
        params: Value,
        timeout: Duration,
    ) -> Result<Value, NativeError> {
        self.wait(self.begin(method, operation_id, params)?, timeout)
    }
    pub fn begin(
        &self,
        method: &str,
        operation_id: &str,
        params: Value,
    ) -> Result<Call, NativeError> {
        let (id, receiver, _) = self.send("request", method, operation_id, params, false)?;
        Ok(Call {
            id,
            operation_id: operation_id.into(),
            method: method.into(),
            receiver,
        })
    }
    pub(crate) fn call_observed(
        &self,
        method: &str,
        timeout: Duration,
    ) -> Result<(Value, Option<ReadProof>), NativeError> {
        let (id, receiver, proof) = self.send("request", method, "", json!({}), true)?;
        let value = self.wait(
            Call {
                id,
                receiver,
                operation_id: String::new(),
                method: method.into(),
            },
            timeout,
        )?;
        Ok((value, proof))
    }
    pub fn wait(&self, call: Call, timeout: Duration) -> Result<Value, NativeError> {
        let (method, request) = (call.method.clone(), call.id.clone());
        self.wait_unrecorded(call, timeout)
            .inspect_err(|error| self.shared.record_failure(&method, &request, error))
    }
    /// Decodes a result, logging a failure without the offending value.
    pub(crate) fn decode<T: for<'de> Deserialize<'de>>(
        &self,
        method: &str,
        value: Value,
    ) -> Result<T, NativeError> {
        serde_json::from_value(value).map_err(|error| {
            if let Some(log) = &self.shared.diagnostics {
                if !self.shared.closing.load(Ordering::Acquire) {
                    log.decode_failed::<T>(method, &error);
                }
            }
            invalid()
        })
    }
    fn wait_unrecorded(&self, call: Call, timeout: Duration) -> Result<Value, NativeError> {
        #[cfg(target_os = "linux")]
        let received = receive_with_deadline(&call.receiver, timeout, boottime);
        #[cfg(not(target_os = "linux"))]
        let received = call.receiver.recv_timeout(timeout);
        match received {
            Ok(result) => result,
            Err(_) => {
                if let Ok(mut pending) = self.shared.pending.lock() {
                    pending.remove(&call.id);
                }
                let operation_id = if call.operation_id.is_empty() {
                    &call.id
                } else {
                    &call.operation_id
                };
                let _ = self.cancel(operation_id);
                Err(NativeError::new(
                    "rpc_timeout",
                    "The Yard request timed out. Its final result is unavailable; refresh the owner before trying again.",
                ))
            }
        }
    }

    pub fn subscribe(&self, operation_id: &str) -> Result<(), NativeError> {
        self.require("ordered-events")?;
        let _ = self.send(
            "request",
            "incus.events",
            operation_id,
            json!({"types":["lifecycle","operation"]}),
            false,
        )?;
        Ok(())
    }

    pub fn discard(&self, operation_id: &str) -> Result<(), NativeError> {
        let _ = self.send(
            "request",
            "operation.discard",
            operation_id,
            json!({}),
            false,
        )?;
        Ok(())
    }

    pub fn cancel(&self, operation_id: &str) -> Result<(), NativeError> {
        if !identity(operation_id) {
            return Err(invalid());
        }
        let id = format!("veranda-{}", self.next_id.fetch_add(1, Ordering::Relaxed));
        self.outgoing.lock().map_err(|_| disconnected())?.as_ref().ok_or_else(disconnected)?
            .try_send(json!({"version":PROTOCOL_VERSION,"type":"cancel","id":id,"operationId":operation_id}))
            .map_err(|_| NativeError::new("request_capacity", "The Yard cancellation queue is busy. Retry cancellation or disconnect the owner."))
    }

    fn send(
        &self,
        kind: &str,
        method: &str,
        operation_id: &str,
        params: Value,
        observed: bool,
    ) -> Result<SentRequest, NativeError> {
        if self.shared.closed.load(Ordering::Acquire) {
            return Err(disconnected());
        }
        if (!method.is_empty() && !identity(method))
            || (!operation_id.is_empty() && !identity(operation_id))
        {
            return Err(invalid());
        }
        let id = format!("veranda-{}", self.next_id.fetch_add(1, Ordering::Relaxed));
        let operation_id = if operation_id.is_empty() {
            id.as_str()
        } else {
            operation_id
        };
        let (sender, receiver) = mpsc::sync_channel(1);
        {
            let mut pending = self.shared.pending.lock().map_err(|_| disconnected())?;
            if pending.len() >= MAX_PENDING {
                return Err(NativeError::new(
                    "request_capacity",
                    "Finish or cancel an existing operation before starting another.",
                ));
            }
            pending.insert(
                id.clone(),
                Pending {
                    operation_id: if method == "rpc.negotiate" {
                        String::new()
                    } else {
                        operation_id.into()
                    },
                    method: method.into(),
                    sender,
                },
            );
        }
        let request = json!({"version":PROTOCOL_VERSION,"type":kind,"id":id,"method":method,"operationId":operation_id,"params":params});
        let result = (|| {
            let outgoing = self.outgoing.lock().map_err(|_| disconnected())?;
            let outgoing = outgoing.as_ref().ok_or_else(disconnected)?;
            // Conservative frontier immediately before this authoritative request is enqueued.
            let proof = if observed {
                self.observation.as_ref().map(Source::admit)
            } else {
                None
            };
            outgoing.try_send(request).map_err(|_| NativeError::new(
                "request_capacity", "The bounded Yard request queue is busy. Finish an existing request and try again."))?;
            Ok(proof)
        })();
        match result {
            Ok(proof) => Ok((id, receiver, proof)),
            Err(error) => {
                if let Ok(mut pending) = self.shared.pending.lock() {
                    pending.remove(&id);
                }
                Err(error)
            }
        }
    }

    pub fn is_closed(&self) -> bool {
        self.shared.closed.load(Ordering::Acquire)
    }
    pub fn take_stale(&self) -> bool {
        self.shared.stale.swap(false, Ordering::AcqRel)
    }

    #[cfg(all(test, target_os = "linux"))]
    pub(crate) fn kill_owned_child_for_test(&self) -> Result<(), NativeError> {
        self.child
            .lock()
            .map_err(|_| disconnected())?
            .as_mut()
            .ok_or_else(disconnected)?
            .kill()
            .map_err(|_| disconnected())
    }

    pub fn close(&self) {
        self.shared.closing.store(true, Ordering::Release);
        if let (Ok(pending), Ok(outgoing)) = (self.shared.pending.lock(), self.outgoing.lock()) {
            if let Some(outgoing) = outgoing.as_ref() {
                for request in pending
                    .values()
                    .filter(|request| !request.operation_id.is_empty())
                {
                    let id = format!("veranda-{}", self.next_id.fetch_add(1, Ordering::Relaxed));
                    let _ = outgoing.try_send(json!({"version":PROTOCOL_VERSION,"type":"cancel","id":id,"operationId":request.operation_id}));
                }
            }
        }
        self.shared.closed.store(true, Ordering::Release);
        if let Ok(mut outgoing) = self.outgoing.lock() {
            outgoing.take();
        }
        if let Ok(mut owned) = self.child.lock() {
            if let Some(mut child) = owned.take() {
                let deadline = Instant::now() + Duration::from_millis(500);
                while Instant::now() < deadline {
                    if child_exited(&mut child) {
                        break;
                    }
                    thread::sleep(Duration::from_millis(10));
                }
                stop_child(&mut child);
            }
        }
        if let Ok(mut reader) = self.reader.lock() {
            if let Some(reader) = reader.take() {
                let _ = reader.join();
            }
        }
        if let Ok(mut writer) = self.writer.lock() {
            if let Some(writer) = writer.take() {
                let _ = writer.join();
            }
        }
    }
}

#[cfg(target_os = "linux")]
fn boottime() -> Option<Duration> {
    let mut value = libc::timespec {
        tv_sec: 0,
        tv_nsec: 0,
    };
    if unsafe { libc::clock_gettime(libc::CLOCK_BOOTTIME, &mut value) } != 0
        || !(0..1_000_000_000).contains(&value.tv_nsec)
    {
        return None;
    }
    Some(Duration::new(
        value.tv_sec.try_into().ok()?,
        value.tv_nsec.try_into().ok()?,
    ))
}

#[cfg(target_os = "linux")]
fn receive_with_deadline<T>(
    receiver: &mpsc::Receiver<T>,
    timeout: Duration,
    mut clock: impl FnMut() -> Option<Duration>,
) -> Result<T, mpsc::RecvTimeoutError> {
    use mpsc::RecvTimeoutError::Timeout;
    let mut previous = clock().ok_or(Timeout)?;
    let deadline = previous.checked_add(timeout).ok_or(Timeout)?;
    loop {
        let now = clock().ok_or(Timeout)?;
        if now < previous || now >= deadline {
            return Err(Timeout);
        }
        previous = now;
        // Linux channel waits exclude suspend. Short slices bound the wake-up
        // delay while CLOCK_BOOTTIME includes time spent asleep.
        match receiver.recv_timeout((deadline - now).min(Duration::from_millis(100))) {
            Ok(result) => {
                let now = clock().ok_or(Timeout)?;
                if now < previous || now >= deadline {
                    return Err(Timeout);
                }
                return Ok(result);
            }
            Err(Timeout) => {}
            Err(error) => return Err(error),
        }
    }
}

fn stop_child(child: &mut OwnedChild) {
    // Kill the dedicated group before reaping its leader: descendants may keep
    // the pipes open after the leader exits. Ownership is consumed exactly once.
    #[cfg(unix)]
    unsafe {
        libc::kill(-(child.id() as i32), libc::SIGKILL);
    }
    let _ = child.kill();
    let _ = child.wait();
}
fn child_exited(child: &mut OwnedChild) -> bool {
    child.exited()
}

impl Drop for RpcClient {
    fn drop(&mut self) {
        self.close();
    }
}

pub(crate) fn identity(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 128
        && value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || b"_.-".contains(&byte))
}
fn version(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 128
        && value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || b".+-".contains(&byte))
}
pub(crate) fn invalid() -> NativeError {
    NativeError::new(
        "invalid_response",
        "Subyard returned an unsupported response. Install Veranda and Subyard from the same release.",
    )
}
fn disconnected() -> NativeError {
    NativeError::new(
        "disconnected",
        "The Yard connection closed. Reconnect and refresh; an interrupted operation has no confirmed final result.",
    )
}

/// The fixed text for the code, plus the owner's typed refusals when its
/// data is within the contract; other data is ignored. A code this client
/// shows only as a generic failure, and ignored data, are logged.
fn owner_fault(error: &Fault, shared: &Shared, method: &str, request: &str) -> NativeError {
    let mut fault = public_fault(&error.code);
    if let Some(data) = &error.data {
        fault.refusals = crate::refusals::parse(data).ok();
        if fault.refusals.is_none() {
            shared.record(Class::ErrorDataRejected, method, &error.code, request);
        }
    }
    if fault.code == "owner_request_failed" {
        shared.record(Class::OwnerError, method, &error.code, request);
    }
    fault
}

fn public_fault(code: &str) -> NativeError {
    let (code, message) = match ErrorCode::parse(code) {
        Some(ErrorCode::PlanStale) | Some(ErrorCode::PlanBindingInvalid) => (
            "plan_stale",
            "The owner state or plan changed. Refresh and request a new plan before applying.",
        ),
        Some(ErrorCode::PlanNotFound) => (
            "plan_not_found",
            "The plan expired or was already used. Request a new plan.",
        ),
        Some(ErrorCode::Cancelled) => (
            "cancelled",
            "The operation was cancelled. Refresh to observe the owner state.",
        ),
        Some(ErrorCode::ConfirmationRequired) => (
            "confirmation_required",
            "Review and confirm the exact owner plan before applying.",
        ),
        Some(ErrorCode::OperationStepsUnsupported) | Some(ErrorCode::MethodNotFound) | Some(ErrorCode::IncompatibleVersion) => (
            "incompatible_engine",
            "This Subyard release does not support the requested schema or operation. Install Veranda and Subyard from the same release.",
        ),
        Some(ErrorCode::SecretForbidden) | Some(ErrorCode::InteractiveOrPayloadCommand) => (
            "protected_operation",
            "This operation requires the owner's protected terminal transport.",
        ),
        Some(ErrorCode::SessionToolUnavailable) => (
            "session_tool_unavailable",
            "Install htop on the selected owner for CPU / RAM, or open its host shell.",
        ),
        Some(ErrorCode::SessionYardUnavailable) => (
            "session_yard_unavailable",
            "This yard is stopped or unavailable. Start it explicitly before opening a session.",
        ),
        Some(ErrorCode::SessionSshUnavailable) | Some(ErrorCode::SessionHostKeyUnavailable) => (
            "session_ssh_unavailable",
            "Reconcile the selected yard explicitly to prepare its verified VS Code SSH access.",
        ),
        Some(ErrorCode::SessionProjectUnavailable) => (
            "session_project_unavailable",
            "The selected project is no longer registered. Refresh the yard and select it again.",
        ),
        Some(ErrorCode::SessionProjectForbidden) => (
            "session_project_forbidden",
            "This yard role does not permit project sessions.",
        ),
        Some(ErrorCode::ProfileCatalogInvalid) => (
            "incompatible_engine",
            "The owner profile catalog is incompatible or invalid. Install Veranda and Subyard from the same release.",
        ),
        Some(ErrorCode::OwnerConfigurationInvalid) => (
            "owner_configuration_invalid",
            "The owner configuration or profile catalog is invalid. Review owner diagnostics and install Veranda and Subyard from the same release.",
        ),
        Some(ErrorCode::OwnerContextChanged) => (
            "owner_context_changed",
            "The owner context changed. Reconnect and refresh before continuing.",
        ),
        Some(code) if code.is_release_gate() => (
            "owner_repair_required",
            "The owner requires release or configuration repair before this operation.",
        ),
        _ => (
            "owner_request_failed",
            "Subyard could not complete the request. Review owner diagnostics and refresh its state.",
        ),
    };
    NativeError::new(code, message)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn faults_never_forward_owner_text_or_unknown_error_codes() {
        let error = public_fault("private/path/token-secret");
        assert_eq!(error.code, "owner_request_failed");
        assert!(!error.message.contains("token-secret"));
        assert_eq!(public_fault("plan_stale").code, "plan_stale");
    }

    #[test]
    fn every_shared_gate_error_code_maps_to_owner_repair() {
        let fixture: Value =
            serde_json::from_str(include_str!("../../contract/yard-rpc/v1/vocabulary.json"))
                .unwrap();
        for (code, entry) in fixture["errorCodes"].as_object().unwrap() {
            if entry["group"] == "release-gate" {
                assert_eq!(public_fault(code).code, "owner_repair_required", "{code}");
            }
        }
        assert_eq!(
            public_fault("migration_required").code,
            "owner_request_failed"
        );
    }

    #[test]
    fn identities_and_versions_are_bounded_before_ipc_or_process_use() {
        assert!(identity("operation-23.test"));
        assert!(!identity("../../bad"));
        assert!(!identity("a\nsecret"));
        assert!(!identity(&"a".repeat(129)));
        assert!(version("0.1.0-dev+build.3"));
        assert!(!version("0.1.0\nprivate"));
    }
}

#[cfg(test)]
#[path = "transport_tests.rs"]
pub(crate) mod transport_tests;

#[cfg(all(test, target_os = "linux", target_arch = "x86_64"))]
#[path = "child_usage_tests.rs"]
mod child_usage_tests;
