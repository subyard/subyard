//! Frozen wire DTOs only; deserialization does not grant authority.
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct RegistryV2 {
    #[serde(rename = "schemaVersion")]
    pub schema_version: i64,
    #[serde(rename = "minimumEpochs")]
    pub minimum_epochs: Option<BTreeMap<String, i64>>,
    #[serde(rename = "currentEpochs")]
    pub current_epochs: Option<BTreeMap<String, i64>>,
    #[serde(rename = "migrations")]
    pub migrations: Option<Vec<Migration>>,
    #[serde(
        rename = "retiredPrefixes",
        skip_serializing_if = "crate::wire::empty_map"
    )]
    pub retired_prefixes: Option<BTreeMap<String, RetiredMigrationPrefixV2>>,
}
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct RetiredMigrationPrefixV2 {
    #[serde(rename = "throughEpoch")]
    pub through_epoch: i64,
    #[serde(rename = "digest")]
    pub digest: String,
}
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct Migration {
    #[serde(rename = "id")]
    pub id: String,
    #[serde(rename = "domain")]
    pub domain: String,
    #[serde(rename = "fromEpoch")]
    pub from_epoch: i64,
    #[serde(rename = "toEpoch")]
    pub to_epoch: i64,
    #[serde(rename = "kind")]
    pub kind: String,
    #[serde(rename = "dependsOn", skip_serializing_if = "crate::wire::empty_vec")]
    pub depends_on: Option<Vec<String>>,
}
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct CapabilityDescriptor {
    #[serde(rename = "kind")]
    pub kind: String,
    #[serde(rename = "domain")]
    pub domain: String,
    #[serde(rename = "version")]
    pub version: i64,
    #[serde(
        rename = "rollbackCompatibility",
        skip_serializing_if = "String::is_empty"
    )]
    pub rollback_compatibility: String,
    #[serde(
        rename = "legacyMinimumVersion",
        skip_serializing_if = "String::is_empty"
    )]
    pub legacy_minimum_version: String,
}
