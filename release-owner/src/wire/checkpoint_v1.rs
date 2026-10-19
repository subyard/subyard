//! Frozen wire DTOs only; deserialization does not grant authority.
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct LedgerCheckpointV1 {
    #[serde(rename = "schemaVersion")]
    pub schema_version: i64,
    #[serde(rename = "registryDigest")]
    pub registry_digest: String,
    #[serde(rename = "legacyProjection")]
    pub legacy_projection: LedgerProjectionBindingV1,
    #[serde(rename = "domains")]
    pub domains: Option<BTreeMap<String, DomainCheckpointV1>>,
}
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct LedgerProjectionBindingV1 {
    #[serde(rename = "exists")]
    pub exists: bool,
    #[serde(rename = "fingerprint", skip_serializing_if = "String::is_empty")]
    pub fingerprint: String,
    #[serde(rename = "epochs")]
    pub epochs: Option<BTreeMap<String, i64>>,
}
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct DomainCheckpointV1 {
    #[serde(rename = "epoch")]
    pub epoch: i64,
    #[serde(rename = "compactedThrough")]
    pub compacted_through: i64,
    #[serde(rename = "prefixDigest")]
    pub prefix_digest: String,
    #[serde(rename = "appliedSuffix")]
    pub applied_suffix: Option<Vec<String>>,
}
