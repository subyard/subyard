//! Bounded Yard RPC sessions. Only the native shell owns process handles and frames.
use crate::local_fleet::{read_frame, write_frame, NativeError};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use std::collections::HashMap;
use std::io::Write;
use std::process::{Child, Command, Stdio};
use std::sync::{
    atomic::{AtomicBool, AtomicU64, Ordering},
    mpsc, Arc, Mutex, Weak,
};
use std::thread::{self, JoinHandle};
use std::time::{Duration, Instant};

const MAX_PENDING: usize = 64;
pub type Notify = Arc<dyn Fn(Event) + Send + Sync>;

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct Event {
    pub operation_id: String,
    pub event: String,
    pub sequence: u64,
    pub revision: u64,
    pub stale: bool,
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
}

struct Shared {
    pending: Mutex<HashMap<String, Pending>>,
    closed: AtomicBool,
    stale: AtomicBool,
    sequence: AtomicU64,
}

struct Pending {
    operation_id: String,
    sender: mpsc::SyncSender<Result<Value, NativeError>>,
}
pub struct Call {
    id: String,
    operation_id: String,
    receiver: mpsc::Receiver<Result<Value, NativeError>>,
}

pub struct RpcClient {
    child: Arc<Mutex<Option<Child>>>,
    outgoing: Mutex<Option<mpsc::SyncSender<Value>>>,
    shared: Arc<Shared>,
    next_id: AtomicU64,
    reader: Mutex<Option<JoinHandle<()>>>,
    writer: Mutex<Option<JoinHandle<()>>>,
    pub engine_version: String,
    pub capabilities: Vec<String>,
}

/// Includes children that are still negotiating and therefore not in a session cache yet.
#[derive(Default)]
pub struct Processes {
    children: Mutex<Vec<Weak<Mutex<Option<Child>>>>>,
    stopped: AtomicBool,
}
impl Processes {
    pub fn stop_accepting(&self) {
        self.stopped.store(true, Ordering::Release);
    }
    fn track(&self, child: &Arc<Mutex<Option<Child>>>) -> Result<(), NativeError> {
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
        mut command: Command,
        expected_version: &str,
        notify: Notify,
        processes: Option<&Processes>,
    ) -> Result<Self, NativeError> {
        #[cfg(unix)]
        {
            use std::os::unix::process::CommandExt;
            command.process_group(0);
        }
        let mut child = command.stdin(Stdio::piped()).stdout(Stdio::piped()).stderr(Stdio::null())
            .spawn().map_err(|_| NativeError::new("transport_unavailable", "Veranda could not start Yard or SSH. Check the installed tools and authentication reference."))?;
        let mut input = child.stdin.take().ok_or_else(disconnected)?;
        let mut output = child.stdout.take().ok_or_else(disconnected)?;
        let shared = Arc::new(Shared {
            pending: Mutex::new(HashMap::new()),
            closed: AtomicBool::new(false),
            stale: AtomicBool::new(false),
            sequence: AtomicU64::new(0),
        });
        let reader_shared = shared.clone();
        let reader = thread::spawn(move || {
            loop {
                let frame: Frame = match read_frame(&mut output) {
                    Ok(frame) => frame,
                    Err(_) => break,
                };
                if frame.version != 1 {
                    break;
                }
                match frame.kind.as_str() {
                    "response" => {
                        if !identity(&frame.id) || frame.result.is_some() == frame.error.is_some() {
                            break;
                        }
                        let response = match frame.error {
                            Some(error) => Err(public_fault(&error.code)),
                            None => Ok(frame.result.unwrap_or(Value::Null)),
                        };
                        let Ok(mut pending) = reader_shared.pending.lock() else {
                            break;
                        };
                        if let Some(target) = pending.remove(&frame.id) {
                            if frame.operation_id != target.operation_id {
                                let _ = target.sender.try_send(Err(invalid()));
                                break;
                            }
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
        };
        if let Some(processes) = processes {
            processes.track(&client.child)?;
        }
        let raw = client.call("rpc.negotiate", "", json!({}), Duration::from_secs(8))?;
        let negotiation: Negotiation = serde_json::from_value(raw).map_err(|_| invalid())?;
        if negotiation.version != 1
            || negotiation.protocol_min > 1
            || negotiation.protocol_max < 1
            || !version(&negotiation.engine_version)
            || negotiation.capabilities.len() > 128
            || negotiation
                .capabilities
                .iter()
                .any(|capability| !identity(capability))
        {
            return Err(invalid());
        }
        if negotiation.engine_version != expected_version {
            return Err(NativeError::new("incompatible_engine", format!(
                "Veranda {expected_version} and Subyard {} are incompatible. Install both from the same release.", negotiation.engine_version)));
        }
        client.engine_version = negotiation.engine_version;
        client.capabilities = negotiation.capabilities;
        client.require("owner-inventory-v1")?;
        Ok(client)
    }

    pub fn require(&self, capability: &str) -> Result<(), NativeError> {
        if self.capabilities.iter().any(|value| value == capability) {
            Ok(())
        } else {
            Err(NativeError::new("capability_missing", format!(
            "Subyard {} does not support {capability}. Install Veranda and Subyard from the same release.", self.engine_version)))
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
        let (id, receiver) = self.send("request", method, operation_id, params)?;
        Ok(Call {
            id,
            operation_id: operation_id.into(),
            receiver,
        })
    }
    pub fn wait(&self, call: Call, timeout: Duration) -> Result<Value, NativeError> {
        match call.receiver.recv_timeout(timeout) {
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
                Err(NativeError::new("rpc_timeout", "The Yard request timed out. Its final result is unavailable; refresh the owner before trying again."))
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
        )?;
        Ok(())
    }

    pub fn discard(&self, operation_id: &str) -> Result<(), NativeError> {
        let _ = self.send("request", "operation.discard", operation_id, json!({}))?;
        Ok(())
    }

    pub fn cancel(&self, operation_id: &str) -> Result<(), NativeError> {
        if !identity(operation_id) {
            return Err(invalid());
        }
        let id = format!("veranda-{}", self.next_id.fetch_add(1, Ordering::Relaxed));
        self.outgoing.lock().map_err(|_| disconnected())?.as_ref().ok_or_else(disconnected)?
            .try_send(json!({"version":1,"type":"cancel","id":id,"operationId":operation_id}))
            .map_err(|_| NativeError::new("request_capacity", "The Yard cancellation queue is busy. Retry cancellation or disconnect the owner."))
    }

    fn send(
        &self,
        kind: &str,
        method: &str,
        operation_id: &str,
        params: Value,
    ) -> Result<(String, mpsc::Receiver<Result<Value, NativeError>>), NativeError> {
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
                    sender,
                },
            );
        }
        let request = json!({"version":1,"type":kind,"id":id,"method":method,"operationId":operation_id,"params":params});
        let result = (|| {
            let outgoing = self.outgoing.lock().map_err(|_| disconnected())?;
            outgoing.as_ref().ok_or_else(disconnected)?.try_send(request).map_err(|_| NativeError::new(
                "request_capacity", "The bounded Yard request queue is busy. Finish an existing request and try again."))
        })();
        if let Err(error) = result {
            if let Ok(mut pending) = self.shared.pending.lock() {
                pending.remove(&id);
            }
            return Err(error);
        }
        Ok((id, receiver))
    }

    pub fn is_closed(&self) -> bool {
        self.shared.closed.load(Ordering::Acquire)
    }
    pub fn take_stale(&self) -> bool {
        self.shared.stale.swap(false, Ordering::AcqRel)
    }

    #[cfg(test)]
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
        if let (Ok(pending), Ok(outgoing)) = (self.shared.pending.lock(), self.outgoing.lock()) {
            if let Some(outgoing) = outgoing.as_ref() {
                for request in pending
                    .values()
                    .filter(|request| !request.operation_id.is_empty())
                {
                    let id = format!("veranda-{}", self.next_id.fetch_add(1, Ordering::Relaxed));
                    let _ = outgoing.try_send(json!({"version":1,"type":"cancel","id":id,"operationId":request.operation_id}));
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

fn stop_child(child: &mut Child) {
    // Kill the dedicated group before reaping its leader: descendants may keep
    // the pipes open after the leader exits. Ownership is consumed exactly once.
    #[cfg(unix)]
    unsafe {
        libc::kill(-(child.id() as i32), libc::SIGKILL);
    }
    let _ = child.kill();
    let _ = child.wait();
}
fn child_exited(child: &mut Child) -> bool {
    #[cfg(unix)]
    {
        // WNOWAIT keeps the PID reserved until stop_child revokes the group.
        let mut status: libc::siginfo_t = unsafe { std::mem::zeroed() };
        let result = unsafe {
            libc::waitid(
                libc::P_PID,
                child.id(),
                &mut status,
                libc::WEXITED | libc::WNOHANG | libc::WNOWAIT,
            )
        };
        result != 0 || unsafe { status.si_pid() } != 0
    }
    #[cfg(not(unix))]
    {
        matches!(child.try_wait(), Ok(Some(_)))
    }
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
    NativeError::new("invalid_response", "Subyard returned an unsupported response. Install Veranda and Subyard from the same release.")
}
fn disconnected() -> NativeError {
    NativeError::new("disconnected", "The Yard connection closed. Reconnect and refresh; an interrupted operation has no confirmed final result.")
}

fn public_fault(code: &str) -> NativeError {
    let (code, message) = match code {
        "plan_stale" | "plan_binding_invalid" => ("plan_stale", "The owner state or plan changed. Refresh and request a new plan before applying."),
        "plan_not_found" => ("plan_not_found", "The plan expired or was already used. Request a new plan."),
        "cancelled" => ("cancelled", "The operation was cancelled. Refresh to observe the owner state."),
        "confirmation_required" => ("confirmation_required", "Review and confirm the exact owner plan before applying."),
        "operation_steps_unsupported" | "method_not_found" | "incompatible_version" => ("incompatible_engine", "This Subyard release does not support the requested schema or operation. Install Veranda and Subyard from the same release."),
        "secret_forbidden" | "interactive_or_payload_command" => ("protected_operation", "This operation requires the owner's protected terminal transport."),
        "session_tool_unavailable" => ("session_tool_unavailable", "Install htop on the selected owner for CPU / RAM, or open its host shell."),
        "session_yard_unavailable" => ("session_yard_unavailable", "This yard is stopped or unavailable. Start it explicitly before opening a session."),
        "session_ssh_unavailable" | "session_host_key_unavailable" => ("session_ssh_unavailable", "Reconcile the selected yard explicitly to prepare its verified VS Code SSH access."),
        "session_project_unavailable" => ("session_project_unavailable", "The selected project is no longer registered. Refresh the yard and select it again."),
        "session_project_forbidden" => ("session_project_forbidden", "This yard role does not permit project sessions."),
        "profile_catalog_invalid" => ("incompatible_engine", "The owner profile catalog is incompatible or invalid. Install Veranda and Subyard from the same release."),
        "owner_configuration_invalid" => ("owner_configuration_invalid", "The owner configuration or profile catalog is invalid. Review owner diagnostics and install Veranda and Subyard from the same release."),
        "owner_context_changed" => ("owner_context_changed", "The owner context changed. Reconnect and refresh before continuing."),
        "mutation_gate_failed" | "migration_required" => ("owner_repair_required", "The owner requires release or configuration repair before this operation."),
        _ => ("owner_request_failed", "Subyard could not complete the request. Review owner diagnostics and refresh its state."),
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
