use serde::{de::DeserializeOwned, Deserialize, Serialize};
use serde_json::Value;
use std::collections::HashSet;
use std::io::{self, Read, Write};
use std::process::{Child, Command, Stdio};
use std::sync::mpsc;
use std::thread;
use std::time::{Duration, Instant};

const PROTOCOL_VERSION: u32 = 1;
const MAX_FRAME_SIZE: usize = 1024 * 1024;
const RESPONSE_TIMEOUT: Duration = Duration::from_secs(5);
const EXIT_GRACE: Duration = Duration::from_millis(500);
const OWNER_INVENTORY_CAPABILITY: &str = "owner-inventory-v1";

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct LocalProject {
    id: String,
    name: String,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct LocalYard {
    name: String,
    kind: String,
    state: String,
    projects: Vec<LocalProject>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct LocalOwnerHost {
    id: String,
    yards: Vec<LocalYard>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct LocalFleetSnapshot {
    engine_version: String,
    observed_at: String,
    current_yard_name: String,
    owner: LocalOwnerHost,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct NativeError {
    pub code: String,
    pub message: String,
}

impl NativeError {
    pub fn new(code: impl Into<String>, message: impl Into<String>) -> Self {
        Self {
            code: code.into(),
            message: message.into(),
        }
    }

    fn io(context: &'static str, error: io::Error) -> Self {
        let code = if error.kind() == io::ErrorKind::NotFound {
            "yard_not_found"
        } else {
            "yard_io_failed"
        };
        Self::new(
            code,
            format!("{context}. Check that Yard is installed and try again."),
        )
    }
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
struct RpcRequest<'a> {
    version: u32,
    #[serde(rename = "type")]
    kind: &'static str,
    id: &'a str,
    method: &'a str,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct RpcResponse {
    version: u32,
    #[serde(rename = "type")]
    kind: String,
    #[serde(default)]
    id: String,
    #[serde(default)]
    operation_id: String,
    #[serde(default)]
    result: Option<Value>,
    #[serde(default)]
    error: Option<RpcFault>,
}

#[derive(Debug, Deserialize)]
struct RpcFault {
    code: String,
    #[allow(dead_code)]
    message: String,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct Negotiation {
    version: u32,
    protocol_min: u32,
    protocol_max: u32,
    engine_version: String,
    capabilities: Vec<String>,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct CurrentContext {
    yard_name: String,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct OwnerInventory {
    schema: u32,
    host_id: String,
    observed_at: String,
    yards: Vec<OwnerYard>,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct OwnerYard {
    name: String,
    kind: String,
    state: String,
    projects: Vec<OwnerProject>,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct OwnerProject {
    project_id: String,
    name: String,
}

pub fn load_from_yard() -> Result<LocalFleetSnapshot, NativeError> {
    let mut child = Command::new("yard")
        .args(["rpc", "--stdio"])
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::null())
        .spawn()
        .map_err(|error| NativeError::io("Veranda could not start `yard rpc --stdio`", error))?;

    let mut input = child.stdin.take().ok_or_else(|| {
        NativeError::new(
            "yard_io_failed",
            "Veranda could not open the Yard request stream. Try again.",
        )
    })?;
    let mut output = child.stdout.take().ok_or_else(|| {
        NativeError::new(
            "yard_io_failed",
            "Veranda could not open the Yard response stream. Try again.",
        )
    })?;

    let (responses, receiver) = mpsc::sync_channel(1);
    let reader = thread::spawn(move || loop {
        let response = read_frame::<RpcResponse>(&mut output);
        let terminal = response.is_err();
        if responses.send(response).is_err() || terminal {
            break;
        }
    });

    let result = exchange(
        |request| write_frame(&mut input, request),
        || {
            receiver
                .recv_timeout(RESPONSE_TIMEOUT)
                .map_err(|error| match error {
                    mpsc::RecvTimeoutError::Timeout => NativeError::new(
                        "yard_timeout",
                        "Yard did not return the local fleet in time. Try again.",
                    ),
                    mpsc::RecvTimeoutError::Disconnected => NativeError::new(
                        "yard_disconnected",
                        "Yard closed the local RPC session. Run `yard status` for details.",
                    ),
                })?
        },
    );

    drop(input);
    finish_child(&mut child);
    drop(receiver);
    let _ = reader.join();
    result
}

fn finish_child(child: &mut Child) {
    let deadline = Instant::now() + EXIT_GRACE;
    while Instant::now() < deadline {
        match child.try_wait() {
            Ok(Some(_)) => return,
            Ok(None) => thread::sleep(Duration::from_millis(10)),
            Err(_) => break,
        }
    }
    let _ = child.kill();
    let _ = child.wait();
}

fn exchange(
    mut send: impl FnMut(&RpcRequest<'_>) -> Result<(), NativeError>,
    mut receive: impl FnMut() -> Result<RpcResponse, NativeError>,
) -> Result<LocalFleetSnapshot, NativeError> {
    send(&request("negotiate", "rpc.negotiate"))?;
    let negotiation: Negotiation = response_result(receive()?, "negotiate")?;
    if negotiation.version != PROTOCOL_VERSION
        || negotiation.protocol_min > PROTOCOL_VERSION
        || negotiation.protocol_max < PROTOCOL_VERSION
    {
        return Err(NativeError::new(
            "incompatible_engine",
            "This Veranda build requires Yard RPC v1. Install the matching Yard release.",
        ));
    }
    if !negotiation
        .capabilities
        .iter()
        .any(|capability| capability == OWNER_INVENTORY_CAPABILITY)
    {
        return Err(NativeError::new(
            "capability_missing",
            "This Yard build cannot provide a local fleet. Install the matching Yard release.",
        ));
    }
    if negotiation.engine_version.is_empty() || negotiation.engine_version.len() > 128 {
        return Err(invalid_response("Yard returned an invalid engine version."));
    }

    send(&request("context", "context.get"))?;
    let context: CurrentContext = response_result(receive()?, "context")?;
    if !safe_name(&context.yard_name) {
        return Err(invalid_response("Yard returned an invalid current yard."));
    }

    send(&request("inventory", "owner.inventory"))?;
    let inventory: OwnerInventory = response_result(receive()?, "inventory")?;
    inventory.into_snapshot(negotiation.engine_version, context.yard_name)
}

fn request<'a>(id: &'a str, method: &'a str) -> RpcRequest<'a> {
    RpcRequest {
        version: PROTOCOL_VERSION,
        kind: "request",
        id,
        method,
    }
}

fn response_result<T: DeserializeOwned>(
    response: RpcResponse,
    expected_id: &str,
) -> Result<T, NativeError> {
    if response.version != PROTOCOL_VERSION {
        return Err(NativeError::new(
            "incompatible_engine",
            "Yard returned an incompatible RPC response. Install the matching Yard release.",
        ));
    }
    if response.kind != "response" || response.id != expected_id {
        return Err(invalid_response(
            "Yard returned an unexpected RPC response.",
        ));
    }
    if !response.operation_id.is_empty() && response.operation_id != expected_id {
        return Err(invalid_response(
            "Yard returned an uncorrelated RPC response.",
        ));
    }
    if let Some(fault) = response.error {
        let code = if safe_error_code(&fault.code) {
            fault.code
        } else {
            "yard_rpc_failed".to_string()
        };
        let message = match expected_id {
            "negotiate" => "Yard RPC negotiation failed. Install the matching Yard release.",
            "context" => "Yard could not report the current local yard. Run `yard status` for details.",
            "inventory" => {
                "Yard could not provide the local fleet. Run `yard status` for details and try again."
            }
            _ => "Yard could not complete the local request. Try again.",
        };
        return Err(NativeError::new(code, message));
    }
    let result = response
        .result
        .ok_or_else(|| invalid_response("Yard returned an empty RPC result."))?;
    serde_json::from_value(result)
        .map_err(|_| invalid_response("Yard returned an invalid typed RPC result."))
}

impl OwnerInventory {
    fn into_snapshot(
        self,
        engine_version: String,
        current_yard_name: String,
    ) -> Result<LocalFleetSnapshot, NativeError> {
        if self.schema != 1
            || !safe_id(&self.host_id, 128)
            || self.host_id.contains('/')
            || self.host_id.contains('\\')
        {
            return Err(invalid_response("Yard returned an invalid owner identity."));
        }
        if self.observed_at.is_empty()
            || self.observed_at.len() > 64
            || self.observed_at.chars().any(char::is_control)
        {
            return Err(invalid_response(
                "Yard returned an invalid observation time.",
            ));
        }
        if self.yards.len() > 1024 {
            return Err(invalid_response("Yard returned too many local yards."));
        }

        let mut yard_names = HashSet::with_capacity(self.yards.len());
        let mut yards = Vec::with_capacity(self.yards.len());
        for yard in self.yards {
            if !safe_name(&yard.name) || !yard_names.insert(yard.name.clone()) {
                return Err(invalid_response("Yard returned an invalid local yard."));
            }
            if yard.kind != "container" && yard.kind != "vm" {
                return Err(invalid_response("Yard returned an invalid yard kind."));
            }
            if yard.state.is_empty()
                || yard.state.len() > 64
                || yard.state.chars().any(char::is_control)
            {
                return Err(invalid_response("Yard returned an invalid yard state."));
            }
            if yard.projects.len() > 100_000 {
                return Err(invalid_response("Yard returned too many projects."));
            }

            let mut project_ids = HashSet::with_capacity(yard.projects.len());
            let mut projects = Vec::with_capacity(yard.projects.len());
            for project in yard.projects {
                if !safe_id(&project.project_id, 128)
                    || !safe_id(&project.name, 50)
                    || !project_ids.insert(project.project_id.clone())
                {
                    return Err(invalid_response(
                        "Yard returned an invalid project identity.",
                    ));
                }
                projects.push(LocalProject {
                    id: project.project_id,
                    name: project.name,
                });
            }
            yards.push(LocalYard {
                name: yard.name,
                kind: yard.kind,
                state: yard.state,
                projects,
            });
        }

        Ok(LocalFleetSnapshot {
            engine_version,
            observed_at: self.observed_at,
            current_yard_name,
            owner: LocalOwnerHost {
                id: self.host_id,
                yards,
            },
        })
    }
}

fn write_frame<T: Serialize>(writer: &mut impl Write, value: &T) -> Result<(), NativeError> {
    let payload = serde_json::to_vec(value).map_err(|_| {
        NativeError::new(
            "native_encode_failed",
            "Veranda could not encode a Yard request.",
        )
    })?;
    if payload.is_empty() || payload.len() > MAX_FRAME_SIZE {
        return Err(NativeError::new(
            "invalid_frame",
            "Veranda refused an invalid Yard RPC frame.",
        ));
    }
    writer
        .write_all(&(payload.len() as u32).to_be_bytes())
        .and_then(|_| writer.write_all(&payload))
        .and_then(|_| writer.flush())
        .map_err(|error| NativeError::io("Veranda could not write to Yard", error))
}

fn read_frame<T: DeserializeOwned>(reader: &mut impl Read) -> Result<T, NativeError> {
    let mut header = [0_u8; 4];
    reader
        .read_exact(&mut header)
        .map_err(|error| NativeError::io("Veranda could not read from Yard", error))?;
    let size = u32::from_be_bytes(header) as usize;
    if size == 0 || size > MAX_FRAME_SIZE {
        return Err(NativeError::new(
            "invalid_frame",
            "Yard returned an RPC frame outside the allowed size.",
        ));
    }
    let mut payload = vec![0; size];
    reader.read_exact(&mut payload).map_err(|error| {
        NativeError::io("Veranda could not read a complete Yard response", error)
    })?;
    serde_json::from_slice(&payload)
        .map_err(|_| NativeError::new("invalid_frame", "Yard returned a malformed RPC response."))
}

fn invalid_response(message: &'static str) -> NativeError {
    NativeError::new("invalid_response", message)
}

fn safe_id(value: &str, max_len: usize) -> bool {
    !value.is_empty()
        && value.len() <= max_len
        && value != "."
        && value != ".."
        && !value.starts_with('-')
        && value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'.' | b'_' | b'-'))
}

fn safe_name(value: &str) -> bool {
    value.len() <= 128
        && value
            .as_bytes()
            .first()
            .is_some_and(|byte| byte.is_ascii_lowercase() || byte.is_ascii_digit())
        && value.bytes().all(|byte| {
            byte.is_ascii_lowercase() || byte.is_ascii_digit() || matches!(byte, b'_' | b'-')
        })
}

fn safe_error_code(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 64
        && value
            .bytes()
            .all(|byte| byte.is_ascii_lowercase() || byte.is_ascii_digit() || byte == b'_')
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;
    use std::collections::VecDeque;
    use std::io::Cursor;

    fn response(id: &str, result: Value) -> Value {
        json!({
            "version": 1,
            "type": "response",
            "id": id,
            "operationId": if id == "negotiate" { "" } else { id },
            "result": result
        })
    }

    fn success_frames(capabilities: Value, inventory: Value) -> Vec<u8> {
        let values = [
            response(
                "negotiate",
                json!({
                    "version": 1,
                    "protocolMin": 1,
                    "protocolMax": 1,
                    "engineVersion": "0.4.0",
                    "capabilities": capabilities
                }),
            ),
            response(
                "context",
                json!({ "yardName": "default", "paths": { "hostBase": "/private" } }),
            ),
            response("inventory", inventory),
        ];
        let mut frames = Vec::new();
        for value in values {
            write_frame(&mut frames, &value).expect("fixture frame");
        }
        frames
    }

    fn valid_inventory() -> Value {
        json!({
            "schema": 1,
            "hostId": "owner-a",
            "observedAt": "2026-08-12T10:00:00Z",
            "yards": [{
                "name": "default",
                "kind": "container",
                "instance": "subyard",
                "state": "RUNNING",
                "sshPort": 2222,
                "devUser": "dev",
                "projects": [{
                    "projectId": "demo",
                    "name": "Demo",
                    "mode": "sync",
                    "target": "yard",
                    "sourceKey": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
                }]
            }]
        })
    }

    fn run_exchange(frames: Vec<u8>) -> (Result<LocalFleetSnapshot, NativeError>, Vec<u8>) {
        let mut reader = Cursor::new(frames);
        let mut requests = Vec::new();
        let result = exchange(
            |request| write_frame(&mut requests, request),
            || read_frame(&mut reader),
        );
        (result, requests)
    }

    #[test]
    fn golden_exchange_returns_only_the_narrow_local_fleet() {
        let (snapshot, requests) = run_exchange(success_frames(
            json!(["snapshot", "owner-inventory-v1"]),
            valid_inventory(),
        ));
        let snapshot = snapshot.expect("valid fleet");
        assert_eq!(snapshot.engine_version, "0.4.0");
        assert_eq!(snapshot.current_yard_name, "default");
        assert_eq!(snapshot.owner.id, "owner-a");
        assert_eq!(snapshot.owner.yards[0].projects[0].name, "Demo");

        let encoded = serde_json::to_string(&snapshot).expect("serialize fleet");
        for forbidden in [
            "hostBase",
            "sourceKey",
            "sshPort",
            "devUser",
            "mode",
            "target",
        ] {
            assert!(
                !encoded.contains(forbidden),
                "leaked {forbidden}: {encoded}"
            );
        }

        let mut request_reader = Cursor::new(requests);
        let mut methods = VecDeque::new();
        for _ in 0..3 {
            let request: Value = read_frame(&mut request_reader).expect("request frame");
            methods.push_back(request["method"].as_str().unwrap().to_string());
        }
        assert_eq!(
            methods,
            VecDeque::from([
                "rpc.negotiate".to_string(),
                "context.get".to_string(),
                "owner.inventory".to_string()
            ])
        );
    }

    #[test]
    fn missing_owner_inventory_capability_fails_closed() {
        let (result, _) = run_exchange(success_frames(json!(["snapshot"]), valid_inventory()));
        assert_eq!(result.unwrap_err().code, "capability_missing");
    }

    #[test]
    fn incompatible_protocol_fails_closed() {
        let frames = {
            let mut frames = Vec::new();
            write_frame(
                &mut frames,
                &response(
                    "negotiate",
                    json!({
                        "version": 2,
                        "protocolMin": 2,
                        "protocolMax": 2,
                        "engineVersion": "1.0.0",
                        "capabilities": ["owner-inventory-v1"]
                    }),
                ),
            )
            .unwrap();
            frames
        };
        let (result, _) = run_exchange(frames);
        assert_eq!(result.unwrap_err().code, "incompatible_engine");
    }

    #[test]
    fn malformed_and_oversized_frames_are_rejected() {
        let mut malformed = Cursor::new([0, 0, 0, 1, b'{']);
        assert_eq!(
            read_frame::<RpcResponse>(&mut malformed).unwrap_err().code,
            "invalid_frame"
        );

        let oversized = (MAX_FRAME_SIZE as u32 + 1).to_be_bytes();
        assert_eq!(
            read_frame::<RpcResponse>(&mut Cursor::new(oversized))
                .unwrap_err()
                .code,
            "invalid_frame"
        );
    }

    #[test]
    fn typed_owner_error_is_redacted_for_the_frontend() {
        let mut frames = success_frames(json!(["owner-inventory-v1"]), valid_inventory());
        let mut reader = Cursor::new(&frames);
        let _: RpcResponse = read_frame(&mut reader).unwrap();
        let _: RpcResponse = read_frame(&mut reader).unwrap();
        let inventory_offset = reader.position() as usize;
        frames.truncate(inventory_offset);
        write_frame(
            &mut frames,
            &json!({
                "version": 1,
                "type": "response",
                "id": "inventory",
                "operationId": "inventory",
                "error": {
                    "code": "owner_inventory_failed",
                    "message": "private path must never cross IPC"
                }
            }),
        )
        .unwrap();

        let (result, _) = run_exchange(frames);
        let error = result.unwrap_err();
        assert_eq!(error.code, "owner_inventory_failed");
        assert!(!error.message.contains("private path"));
    }

    #[test]
    fn invalid_inventory_identity_is_rejected() {
        let mut inventory = valid_inventory();
        inventory["yards"][0]["projects"][0]["projectId"] = json!("../escape");
        let (result, _) = run_exchange(success_frames(json!(["owner-inventory-v1"]), inventory));
        assert_eq!(result.unwrap_err().code, "invalid_response");
    }

    #[test]
    #[ignore = "requires a built `yard` on PATH and a disposable initialized owner host"]
    fn real_yard_process_returns_the_local_owner_inventory() {
        let snapshot = load_from_yard().expect("real local Yard RPC exchange");
        assert!(!snapshot.engine_version.is_empty());
        assert!(!snapshot.observed_at.is_empty());
        assert!(!snapshot.current_yard_name.is_empty());
        assert!(!snapshot.owner.id.is_empty());
        assert!(
            snapshot
                .owner
                .yards
                .iter()
                .any(|yard| yard.name == snapshot.current_yard_name),
            "current yard must be present in the owner inventory"
        );
    }
}
