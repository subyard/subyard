//! Typed owner refusals from Yard RPC `error.data`. The owner sends only
//! value-free texts; this client still accepts the data only within the
//! contract bounds and otherwise keeps the fixed text for the error code.
use serde::{Deserialize, Serialize};
use serde_json::Value;

const MAX_REFUSALS: usize = 32;
const MAX_TEXT: usize = 512;
const MAX_NAME: usize = 128;

/// One independent owner refusal and its next step.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct Refusal {
    pub source: String,
    pub code: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub resource: Option<String>,
    pub text: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub next: Option<String>,
    /// The rendered recovery command, such as `yard migrate`.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub command: Option<String>,
}

/// Every refusal the owner determined, in the owner's order.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct Refusals {
    pub items: Vec<Refusal>,
    /// Refusals beyond the contract bound.
    pub omitted: u32,
    /// The owner had causes that are not typed refusals.
    pub incomplete: bool,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct WireData {
    schema_version: u32,
    refusals: Vec<Value>,
    #[serde(default)]
    omitted: u32,
    #[serde(default)]
    incomplete: bool,
}

#[derive(Deserialize)]
struct WireRefusal {
    source: String,
    code: String,
    resource: Option<String>,
    text: String,
    next: Option<String>,
    action: Option<WireAction>,
}

#[derive(Deserialize)]
struct WireAction {
    verb: String,
    command: String,
}

/// Why `error.data` was not accepted; names no owner text.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Rejected {
    Shape,
    Schema,
    Bounds,
    Text,
}

impl Rejected {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Shape => "shape",
            Self::Schema => "schema-version",
            Self::Bounds => "bounds",
            Self::Text => "text",
        }
    }
}

const SOURCES: [&str; 2] = ["config", "release-gate"];

/// Accepts the whole data object or nothing: a partially valid list could
/// hide the refusal that matters. Only a refusal from a source this client
/// does not know is skipped, and the result is then marked incomplete.
pub fn parse(data: &Value) -> Result<Refusals, Rejected> {
    let wire = WireData::deserialize(data).map_err(|_| Rejected::Shape)?;
    if wire.schema_version != 1 {
        return Err(Rejected::Schema);
    }
    if wire.refusals.is_empty() || wire.refusals.len() > MAX_REFUSALS {
        return Err(Rejected::Bounds);
    }
    let mut items = Vec::with_capacity(wire.refusals.len());
    let mut incomplete = wire.incomplete;
    for refusal in wire.refusals {
        match refusal.get("source").and_then(Value::as_str) {
            Some(source) if SOURCES.contains(&source) => {}
            Some(_) => {
                incomplete = true;
                continue;
            }
            None => return Err(Rejected::Shape),
        }
        let refusal = WireRefusal::deserialize(refusal).map_err(|_| Rejected::Shape)?;
        if !name(&refusal.code)
            || !text(&refusal.text, MAX_TEXT)
            || !optional(&refusal.resource, MAX_NAME)
            || !optional(&refusal.next, MAX_TEXT)
        {
            return Err(Rejected::Text);
        }
        let command = match refusal.action {
            Some(action) => {
                if !name(&action.verb)
                    || !text(&action.command, MAX_TEXT)
                    || !action.command.starts_with("yard ")
                {
                    return Err(Rejected::Text);
                }
                Some(action.command)
            }
            None => None,
        };
        items.push(Refusal {
            source: refusal.source,
            code: refusal.code,
            resource: refusal.resource,
            text: refusal.text,
            next: refusal.next,
            command,
        });
    }
    Ok(Refusals {
        items,
        omitted: wire.omitted,
        incomplete,
    })
}

fn name(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= MAX_NAME
        && value
            .bytes()
            .all(|byte| byte.is_ascii_lowercase() || byte.is_ascii_digit() || b"-_".contains(&byte))
}
fn text(value: &str, limit: usize) -> bool {
    !value.is_empty()
        && value.len() <= limit
        && !value.chars().any(|c| c.is_control() || is_format(c))
}

/// Unicode format characters (general category Cf), such as bidirectional
/// overrides and zero-width characters, which reorder or hide displayed text.
fn is_format(c: char) -> bool {
    matches!(c,
        '\u{ad}'
        | '\u{600}'..='\u{605}'
        | '\u{61c}'
        | '\u{6dd}'
        | '\u{70f}'
        | '\u{890}'..='\u{891}'
        | '\u{8e2}'
        | '\u{180e}'
        | '\u{200b}'..='\u{200f}'
        | '\u{202a}'..='\u{202e}'
        | '\u{2060}'..='\u{2064}'
        | '\u{2066}'..='\u{206f}'
        | '\u{feff}'
        | '\u{fff9}'..='\u{fffb}'
        | '\u{110bd}'
        | '\u{110cd}'
        | '\u{13430}'..='\u{1343f}'
        | '\u{1bca0}'..='\u{1bca3}'
        | '\u{1d173}'..='\u{1d17a}'
        | '\u{e0001}'
        | '\u{e0020}'..='\u{e007f}')
}
fn optional(value: &Option<String>, limit: usize) -> bool {
    value.as_deref().is_none_or(|value| text(value, limit))
}

impl Refusals {
    /// One line per refusal with its next step, then what was left out.
    pub fn lines(&self) -> Vec<String> {
        let mut lines = Vec::new();
        for refusal in &self.items {
            lines.push(format!("• {}", refusal.text));
            let next = match (&refusal.next, &refusal.command) {
                (Some(next), Some(command)) => Some(format!("{next}, then run {command}")),
                (None, Some(command)) => Some(format!("run {command}")),
                (Some(next), None) => Some(next.clone()),
                (None, None) => None,
            };
            if let Some(next) = next {
                lines.push(format!("  Next: {next}"));
            }
        }
        if self.omitted != 0 {
            lines.push(format!(
                "{} more refusals; review the owner's diagnostics.",
                self.omitted
            ));
        }
        if self.incomplete {
            lines.push("Other causes are reported only in the owner's diagnostics.".into());
        }
        lines
    }
}

#[cfg(test)]
#[path = "refusals_tests.rs"]
mod tests;
