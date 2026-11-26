//! Bounded local diagnostics in the private store directory. A record holds a
//! fixed kind, a method name, a public error code and scrubbed detail lines;
//! owner stderr is scrubbed before it is kept, so setting values, quoted text,
//! paths and token-like words never reach memory or disk.
use crate::local_fleet::NativeError;
use serde::{Deserialize, Serialize};
use std::collections::VecDeque;
use std::io::Read;
use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

pub const FILE: &str = "diagnostics.log";
const MAX_RECORDS: usize = 64;
const MAX_FILE: usize = 64 * 1024;
const MAX_LINE: usize = 240;
const MAX_RAW_LINE: usize = 4096;
const TAIL_LINES: usize = 16;

/// One diagnostic record, stored as one JSON line.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct Record {
    pub at: u64,
    pub kind: String,
    pub method: String,
    pub code: String,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub detail: Vec<String>,
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
    pub fn path(&self) -> PathBuf {
        self.root.join(FILE)
    }
    /// Best effort: a diagnostic failure never changes the operation result.
    pub fn record(&self, kind: &str, method: &str, code: &str, detail: Vec<String>) {
        let Ok(_guard) = self.lock.lock() else {
            return;
        };
        let mut records = self.read();
        records.push(Record {
            at: SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .map_or(0, |elapsed| elapsed.as_secs()),
            kind: name_or(kind, "unknown"),
            method: name_or(method, "unknown"),
            code: name_or(code, "unknown"),
            detail: detail
                .iter()
                .filter_map(|line| scrub(line))
                .take(TAIL_LINES)
                .collect(),
        });
        let mut lines: VecDeque<String> = records
            .iter()
            .filter_map(|record| serde_json::to_string(record).ok())
            .collect();
        while lines.len() > MAX_RECORDS
            || lines.iter().map(|line| line.len() + 1).sum::<usize>() > MAX_FILE
        {
            lines.pop_front();
        }
        let mut bytes = Vec::new();
        for line in lines {
            bytes.extend_from_slice(line.as_bytes());
            bytes.push(b'\n');
        }
        let _ = crate::connections::replace_private_store_file(&self.root, FILE, &bytes);
    }
    /// The retained records, oldest first; an unreadable log reads as empty.
    pub fn records(&self) -> Vec<Record> {
        let Ok(_guard) = self.lock.lock() else {
            return Vec::new();
        };
        self.read()
    }
    fn read(&self) -> Vec<Record> {
        crate::connections::read_private_store_file(&self.root, FILE, MAX_FILE as u64)
            .map(|bytes| {
                bytes
                    .split(|byte| *byte == b'\n')
                    .filter_map(|line| serde_json::from_slice(line).ok())
                    .collect()
            })
            .unwrap_or_default()
    }
    /// Records why a result could not be decoded: the target type and serde's
    /// category, plus a field name only from messages that cannot carry values.
    pub(crate) fn decode_failed<T>(&self, method: &str, error: &serde_json::Error) {
        let mut detail = vec![format!(
            "type {}",
            std::any::type_name::<T>()
                .rsplit("::")
                .next()
                .unwrap_or("value")
        )];
        detail.push(format!("category {:?}", error.classify()).to_lowercase());
        let message = error.to_string();
        for prefix in ["missing field `", "unknown field `", "duplicate field `"] {
            if let Some(field) = message
                .strip_prefix(prefix)
                .and_then(|rest| rest.split('`').next())
                .filter(|field| {
                    field
                        .bytes()
                        .all(|b| b.is_ascii_alphanumeric() || b == b'_')
                })
            {
                detail.push(format!("{}{field}`", prefix));
            }
        }
        self.record("decode", method, "invalid_response", detail);
    }
}

fn name_or(value: &str, fallback: &str) -> String {
    if crate::transport::identity(value) {
        value.into()
    } else {
        fallback.into()
    }
}

/// Owner stderr, kept as at most the last 16 scrubbed lines.
pub(crate) struct StderrTail {
    lines: Mutex<VecDeque<String>>,
    done: AtomicBool,
}

impl StderrTail {
    pub(crate) fn drain(mut reader: impl Read + Send + 'static) -> Arc<Self> {
        let tail = Arc::new(Self {
            lines: Mutex::new(VecDeque::new()),
            done: AtomicBool::new(false),
        });
        let writer = tail.clone();
        std::thread::spawn(move || {
            let mut line = Vec::new();
            let mut chunk = [0u8; 1024];
            while let Ok(read) = reader.read(&mut chunk) {
                if read == 0 {
                    break;
                }
                for byte in &chunk[..read] {
                    if *byte == b'\n' {
                        writer.push(&line);
                        line.clear();
                    } else if line.len() < MAX_RAW_LINE {
                        line.push(*byte);
                    }
                }
            }
            writer.push(&line);
            writer.done.store(true, Ordering::Release);
        });
        tail
    }
    fn push(&self, raw: &[u8]) {
        if let Some(line) = scrub(&String::from_utf8_lossy(raw)) {
            if let Ok(mut lines) = self.lines.lock() {
                if lines.len() == TAIL_LINES {
                    lines.pop_front();
                }
                lines.push_back(line);
            }
        }
    }
    /// The tail, after waiting briefly for an exited owner's last output.
    pub(crate) fn lines(&self, settle: Duration) -> Vec<String> {
        let deadline = Instant::now() + settle;
        while !self.done.load(Ordering::Acquire) && Instant::now() < deadline {
            std::thread::sleep(Duration::from_millis(10));
        }
        self.lines
            .lock()
            .map(|lines| lines.iter().cloned().collect())
            .unwrap_or_default()
    }
}

/// Keeps a line's words only when they cannot carry a value: quoted spans,
/// assignments, paths, addresses and token-like words are replaced.
pub(crate) fn scrub(line: &str) -> Option<String> {
    let mut unquoted = String::with_capacity(line.len());
    let mut quote = None;
    for char in line.chars() {
        match quote {
            Some(open) if char == open => {
                quote = None;
                unquoted.push_str("<redacted>");
            }
            Some(_) => {}
            None if matches!(char, '"' | '\'' | '`') => quote = Some(char),
            None if char.is_control() => unquoted.push(' '),
            None => unquoted.push(char),
        }
    }
    if quote.is_some() {
        unquoted.push_str("<redacted>");
    }
    let words: Vec<String> = unquoted.split_whitespace().map(scrub_word).collect();
    let mut text = words.join(" ");
    if text.is_empty() {
        return None;
    }
    if text.len() > MAX_LINE {
        let mut cut = MAX_LINE - "…".len();
        while !text.is_char_boundary(cut) {
            cut -= 1;
        }
        text.truncate(cut);
        text.push('…');
    }
    Some(text)
}

fn scrub_word(word: &str) -> String {
    if let Some((name, _)) = word.split_once('=') {
        let name = name.trim_start_matches('-');
        return if !name.is_empty() && name.bytes().all(|b| b.is_ascii_alphanumeric() || b == b'_') {
            format!("{name}=<redacted>")
        } else {
            "<redacted>".into()
        };
    }
    if word.contains(['/', '\\', '@', '~']) {
        return "<path>".into();
    }
    let core = word.trim_end_matches([',', '.', ':', ';', ')', ']']);
    let token = core.len() >= 16
        && core
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"+-_".contains(&b))
        && core.bytes().any(|b| b.is_ascii_digit())
        && core.bytes().any(|b| b.is_ascii_alphabetic());
    if token {
        "<redacted>".into()
    } else {
        word.into()
    }
}

/// Codes whose cause is not shown to the operator and is worth a local record.
pub(crate) fn recorded(error: &NativeError) -> bool {
    matches!(
        error.code.as_str(),
        "disconnected" | "invalid_response" | "rpc_timeout" | "incompatible_engine"
    )
}

#[cfg(test)]
#[path = "diagnostics_tests.rs"]
mod tests;
