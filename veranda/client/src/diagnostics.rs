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
    /// SSH itself failed before the owner answered.
    SshFailed,
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

/// One failure, stored as one JSON line.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct Record {
    pub at: u64,
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

pub struct DiagnosticLog {
    root: PathBuf,
    lock: Mutex<()>,
}

impl DiagnosticLog {
    pub fn new(root: PathBuf) -> Arc<Self> {
        Arc::new(Self {
            root,
            lock: Mutex::new(()),
        })
    }
    /// Best effort: a log failure never changes the request result.
    pub fn record(&self, class: Class, method: &str, code: &str, request: &str) {
        self.write(Record {
            at: 0,
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
        // Only field names: serde quotes them between backticks after these
        // prefixes, while values appear only in other messages.
        if let Some((
            prefix,
            Structure::MissingField | Structure::UnknownField | Structure::DuplicateField,
        )) = structure
        {
            if let Some(field) = message[prefix.len()..].split('`').next() {
                path = format!("{target}.{field}");
            }
        }
        self.write(Record {
            at: 0,
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
        record.at = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map_or(0, |elapsed| elapsed.as_secs());
        record.method = name_or(&record.method, "unknown");
        record.code = name_or(&record.code, "unknown");
        record.request = name_or(&record.request, "");
        if record.path.is_empty()
            || record.path.len() > MAX_PATH
            || !record
                .path
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || b"_.".contains(&byte))
        {
            record.path.clear();
        }
        let Ok(mut line) = serde_json::to_vec(&record) else {
            return;
        };
        line.push(b'\n');
        let Ok(_guard) = self.lock.lock() else {
            return;
        };
        let _ = crate::connections::append_private_store_line(&self.root, FILE, &line, MAX_FILE);
    }
}

fn name_or(value: &str, fallback: &str) -> String {
    if crate::transport::identity(value) {
        value.into()
    } else {
        fallback.into()
    }
}

#[cfg(test)]
#[path = "diagnostics_tests.rs"]
mod tests;
