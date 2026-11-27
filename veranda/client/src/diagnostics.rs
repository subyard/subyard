//! Bounded local log of Yard RPC failures whose cause the operator does not
//! see. A record holds only facts from closed sets or validated names: no owner
//! text, stderr, value or path ever reaches it.
use serde::{Deserialize, Serialize};
use std::path::PathBuf;
use std::sync::{Arc, Mutex};
use std::time::{SystemTime, UNIX_EPOCH};

pub const FILE: &str = "rpc-errors.log";
/// Each of the log and its one previous generation stays below this size.
const MAX_FILE: u64 = 32 * 1024;
const MAX_PATH: usize = 128;

/// Why a request failed, from a closed list.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Class {
    /// The connection closed without Veranda closing it.
    Disconnected,
    /// Yard or SSH could not be started.
    TransportUnavailable,
    /// A frame or result did not match the contract.
    InvalidResponse,
    RpcTimeout,
    /// The owner release or capabilities do not match this Veranda.
    IncompatibleEngine,
    /// The owner returned a code this Veranda shows only as a generic failure.
    OwnerError,
    /// The owner's error data was outside the contract and was ignored.
    ErrorDataRejected,
}

/// The serde category of a structural decode failure, from a closed list.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Structure {
    MissingField,
    UnknownField,
    DuplicateField,
    InvalidType,
    InvalidValue,
    InvalidLength,
    UnknownVariant,
    Other,
}

/// One failure, stored as one JSON line. Repeats of the record before it are
/// counted in memory and appended as one summary record when another failure
/// arrives, hourly during a long outage and when the log is dropped, so an
/// outage keeps its first record.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct Record {
    pub at: u64,
    /// In a summary, how many repeats it counts, the first at `at`.
    #[serde(default = "one", skip_serializing_if = "is_one")]
    pub count: u64,
    /// In a summary, when the last repeat happened; zero otherwise.
    #[serde(default, skip_serializing_if = "is_zero")]
    pub last: u64,
    pub class: Class,
    pub method: String,
    pub code: String,
    /// The frame id, which the owner's own RPC error log also records.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub request: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub structure: Option<Structure>,
    /// The decoded type and, when serde names one, the field: never a value.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub path: String,
}

fn one() -> u64 {
    1
}
fn is_one(count: &u64) -> bool {
    *count == 1
}
fn is_zero(value: &u64) -> bool {
    *value == 0
}

/// A summary of repeats is appended at least this often, in seconds.
const REPEAT_INTERVAL: u64 = 3600;
/// Codes this client sets itself; owner codes come from the vocabulary.
const LOCAL_CODES: &[&str] = &[
    "disconnected",
    "rpc_timeout",
    "invalid_response",
    "incompatible_engine",
    "transport_unavailable",
];

#[derive(Default)]
struct State {
    /// The last record appended by this process.
    last: Option<Record>,
    /// Repeats of `last` not yet appended.
    repeats: Option<Record>,
}

pub struct DiagnosticLog {
    root: PathBuf,
    lock: Mutex<State>,
}

impl DiagnosticLog {
    pub fn new(root: PathBuf) -> Arc<Self> {
        Arc::new(Self {
            root,
            lock: Mutex::new(State::default()),
        })
    }
    /// Best effort: a log failure never changes the request result.
    pub fn record(&self, class: Class, method: &str, code: &str, request: &str) {
        self.write(Record {
            at: 0,
            count: 1,
            last: 0,
            class,
            method: method.into(),
            code: code.into(),
            request: request.into(),
            structure: None,
            path: String::new(),
        });
    }
    /// Records why a result could not be decoded into `T`, without its value.
    pub(crate) fn decode_failed<T>(&self, method: &str, error: &serde_json::Error) {
        let message = error.to_string();
        let structure = [
            ("missing field `", Structure::MissingField),
            ("unknown field `", Structure::UnknownField),
            ("duplicate field `", Structure::DuplicateField),
            ("invalid type", Structure::InvalidType),
            ("invalid value", Structure::InvalidValue),
            ("invalid length", Structure::InvalidLength),
            ("unknown variant", Structure::UnknownVariant),
        ]
        .into_iter()
        .find(|(prefix, _)| message.starts_with(prefix));
        let target = std::any::type_name::<T>()
            .split('<')
            .next()
            .and_then(|name| name.rsplit("::").next())
            .unwrap_or("value");
        let mut path = target.to_string();
        // Only names of fields that `T` declares: serde quotes them between
        // backticks after these prefixes. An unknown field is named by the
        // owner, so only its type is kept.
        if let Some((prefix, Structure::MissingField | Structure::DuplicateField)) = structure {
            if let Some(field) = message[prefix.len()..].split('`').next() {
                path = format!("{target}.{field}");
            }
        }
        self.write(Record {
            at: 0,
            count: 1,
            last: 0,
            class: Class::InvalidResponse,
            method: method.into(),
            code: "invalid_response".into(),
            request: String::new(),
            structure: Some(structure.map_or(Structure::Other, |(_, structure)| structure)),
            path,
        });
    }
    /// The retained records, oldest first; unreadable lines are skipped.
    pub fn records(&self) -> Vec<Record> {
        let Ok(_guard) = self.lock.lock() else {
            return Vec::new();
        };
        [format!("{FILE}.1"), FILE.to_string()]
            .iter()
            .filter_map(|name| {
                crate::connections::read_private_store_file(&self.root, name, MAX_FILE).ok()
            })
            .flat_map(|bytes| {
                bytes
                    .split(|byte| *byte == b'\n')
                    .filter_map(|line| serde_json::from_slice(line).ok())
                    .collect::<Vec<Record>>()
            })
            .collect()
    }
    fn write(&self, mut record: Record) {
        let now = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map_or(0, |elapsed| elapsed.as_secs());
        record.at = now;
        // Only names from closed sets: an owner-chosen code or method could
        // carry a value.
        record.method = crate::vocabulary::Method::parse(&record.method)
            .map_or("unknown", |method| method.as_str())
            .into();
        record.code = crate::vocabulary::ErrorCode::parse(&record.code)
            .map(|code| code.as_str())
            .or_else(|| {
                LOCAL_CODES
                    .iter()
                    .copied()
                    .find(|code| *code == record.code)
            })
            .unwrap_or("unknown")
            .into();
        if !own_request(&record.request) {
            record.request.clear();
        }
        if record.path.len() > MAX_PATH
            || !record
                .path
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || b"_.".contains(&byte))
        {
            record.path.clear();
        }
        let Ok(mut state) = self.lock.lock() else {
            return;
        };
        if state.last.as_ref().is_some_and(|last| same(last, &record)) {
            let repeats = state.repeats.get_or_insert(Record {
                count: 0,
                ..record.clone()
            });
            repeats.count = repeats.count.saturating_add(1);
            repeats.last = now;
            repeats.request = record.request;
            if now.saturating_sub(repeats.at) >= REPEAT_INTERVAL {
                self.flush(&mut state);
            }
            return;
        }
        self.flush(&mut state);
        self.append(&record);
        state.last = Some(record);
    }
    fn flush(&self, state: &mut State) {
        if let Some(repeats) = state.repeats.take() {
            self.append(&repeats);
        }
    }
    fn append(&self, record: &Record) {
        let Ok(mut line) = serde_json::to_vec(record) else {
            return;
        };
        line.push(b'\n');
        let _ = crate::connections::append_private_store_line(&self.root, FILE, &line, MAX_FILE);
    }
}

impl Drop for DiagnosticLog {
    fn drop(&mut self) {
        if let Ok(mut state) = self.lock.lock() {
            self.flush(&mut state);
        }
    }
}

fn same(left: &Record, right: &Record) -> bool {
    (left.class, left.structure) == (right.class, right.structure)
        && (&left.method, &left.code, &left.path) == (&right.method, &right.code, &right.path)
}

/// Request IDs this client generates.
fn own_request(request: &str) -> bool {
    request.strip_prefix("veranda-").is_some_and(|number| {
        !number.is_empty() && number.bytes().all(|byte| byte.is_ascii_digit())
    })
}

#[cfg(test)]
#[path = "diagnostics_tests.rs"]
mod tests;
