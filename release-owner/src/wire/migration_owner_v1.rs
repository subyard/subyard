//! Frozen untrusted DTOs; these do not convey consent or effects.
use serde::{Deserialize, Serialize};
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct MigrationOwnerPreviousSource {
    #[serde(rename = "release")]
    pub release: String,
    #[serde(rename = "artifact")]
    pub artifact: String,
    #[serde(rename = "registry")]
    pub registry: String,
    #[serde(rename = "inputs")]
    pub inputs: String,
}
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct MigrationOwnerReplacementRequest {
    #[serde(rename = "transaction")]
    pub transaction: String,
    #[serde(rename = "fingerprint")]
    pub fingerprint: String,
}
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct MigrationOwnerReceiptV1 {
    #[serde(rename = "registry", with = "crate::wire::go_bytes")]
    pub registry: Option<Vec<u8>>,
    #[serde(rename = "sourceRegistry", with = "crate::wire::go_bytes")]
    pub source_registry: Option<Vec<u8>>,
    #[serde(rename = "previousSource", skip_serializing_if = "Option::is_none")]
    pub previous_source: Option<MigrationOwnerPreviousSource>,
    #[serde(
        rename = "retainedTransactions",
        skip_serializing_if = "crate::wire::empty_vec"
    )]
    pub retained_transactions: Option<Vec<String>>,
    #[serde(rename = "projection", with = "crate::wire::go_bytes")]
    pub projection_payload: Option<Vec<u8>>,
    #[serde(rename = "checkpoint", with = "crate::wire::go_bytes")]
    pub checkpoint_payload: Option<Vec<u8>>,
    #[serde(rename = "schemaVersion")]
    pub schema_version: i64,
    #[serde(rename = "contract")]
    pub contract: String,
    #[serde(rename = "replacement")]
    pub replacement: MigrationOwnerReplacementRequest,
    #[serde(rename = "predecessor")]
    pub predecessor: crate::wire::journal_v2::Record,
    #[serde(rename = "successor")]
    pub successor: crate::wire::journal_v2::Record,
    #[serde(rename = "projectionFingerprint")]
    pub projection: String,
    #[serde(rename = "authorityFingerprint")]
    pub authority: String,
    #[serde(rename = "protectedInputs")]
    pub protected_inputs: String,
    #[serde(rename = "assets")]
    pub assets: String,
    #[serde(rename = "links")]
    pub links: crate::wire::recovery_v1::ReleaseLinks,
}
