//! Frozen wire DTOs only; deserialization does not grant authority.
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct LedgerV2 {
    #[serde(rename = "schemaVersion")]
    pub schema_version: i64,
    #[serde(rename = "domains")]
    pub domains: Option<BTreeMap<String, DomainState>>,
}
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct DomainState {
    #[serde(rename = "epoch")]
    pub epoch: i64,
    #[serde(rename = "applied")]
    pub applied: Option<Vec<String>>,
}
