//! Frozen untrusted DTOs; these do not convey consent or effects.
use serde::{Deserialize, Serialize};
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct ActivationOnlyRecoveryRequest {
    #[serde(rename = "transaction")]
    pub transaction: String,
    #[serde(rename = "fingerprint")]
    pub fingerprint: String,
}
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct RecoveryOwner {
    #[serde(rename = "release")]
    pub release: String,
    #[serde(rename = "artifactDigest")]
    pub artifact: String,
    #[serde(rename = "registryDigest")]
    pub registry: String,
    #[serde(rename = "catalogDigest")]
    pub catalog: String,
}
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct RecoveryNativePlan {
    #[serde(rename = "id")]
    pub id: String,
    #[serde(rename = "binding")]
    pub binding: String,
    #[serde(rename = "actual")]
    pub actual: String,
    #[serde(rename = "desired")]
    pub desired: String,
    #[serde(rename = "converged")]
    pub converged: bool,
}
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct RecoveryReceiptV1 {
    #[serde(rename = "schemaVersion")]
    pub schema_version: i64,
    #[serde(rename = "contract")]
    pub contract: String,
    #[serde(rename = "replacement")]
    pub replacement: ActivationOnlyRecoveryRequest,
    #[serde(rename = "predecessor")]
    pub predecessor: crate::wire::journal_v2::Record,
    #[serde(rename = "successor")]
    pub successor: crate::wire::journal_v2::Record,
    #[serde(rename = "ledgerFingerprint")]
    pub ledger: String,
    #[serde(rename = "links")]
    pub links: crate::wire::recovery_v1::ReleaseLinks,
    #[serde(rename = "owner")]
    pub owner: RecoveryOwner,
    #[serde(rename = "nativePlans")]
    pub native_plans: Option<Vec<RecoveryNativePlan>>,
    #[serde(rename = "basePlan", skip_serializing_if = "String::is_empty")]
    pub base_plan: String,
    #[serde(rename = "reservation", skip_serializing_if = "String::is_empty")]
    pub reservation: String,
    #[serde(rename = "generation", skip_serializing_if = "crate::wire::zero")]
    pub generation: u64,
}
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct ReleaseLinks {
    #[serde(rename = "active")]
    pub active: String,
    #[serde(rename = "previous", skip_serializing_if = "Option::is_none")]
    pub previous: Option<String>,
}
