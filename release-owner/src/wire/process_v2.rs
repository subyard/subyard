//! Frozen untrusted DTOs; these do not convey consent or effects.
use crate::wire::migration_owner_v1::MigrationOwnerReplacementRequest;
use crate::wire::recovery_v1::*;
use serde::{Deserialize, Serialize};
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct RecoveryCapabilities {
    #[serde(rename = "contract")]
    pub contract: String,
    #[serde(rename = "processSchema")]
    pub process_schema: i64,
    #[serde(rename = "journalSchema")]
    pub journal_schema: i64,
    #[serde(rename = "receiptSchema")]
    pub receipt_schema: i64,
}
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct RecoveryProcessRequest {
    #[serde(rename = "migrationOwner", skip_serializing_if = "Option::is_none")]
    pub migration_owner: Option<MigrationOwnerReplacementRequest>,
    #[serde(rename = "schemaVersion")]
    pub schema_version: i64,
    #[serde(rename = "mode")]
    pub mode: String,
    #[serde(rename = "contract", skip_serializing_if = "String::is_empty")]
    pub contract: String,
    #[serde(rename = "runtimeRoot", skip_serializing_if = "String::is_empty")]
    pub runtime_root: String,
    #[serde(rename = "configHome", skip_serializing_if = "String::is_empty")]
    pub config_home: String,
    #[serde(rename = "yard", skip_serializing_if = "String::is_empty")]
    pub yard: String,
    #[serde(rename = "target", skip_serializing_if = "String::is_empty")]
    pub target: String,
    #[serde(rename = "direction", skip_serializing_if = "String::is_empty")]
    pub direction: String,
    #[serde(rename = "artifactDigest", skip_serializing_if = "String::is_empty")]
    pub artifact_digest: String,
    #[serde(rename = "registryDigest", skip_serializing_if = "String::is_empty")]
    pub registry_digest: String,
    #[serde(
        rename = "inheritedSettingIds",
        skip_serializing_if = "crate::wire::empty_vec"
    )]
    pub inherited_setting_ids: Option<Vec<String>>,
    #[serde(rename = "recovery", skip_serializing_if = "Option::is_none")]
    pub recovery: Option<ActivationOnlyRecoveryRequest>,
    #[serde(rename = "execution", skip_serializing_if = "Option::is_none")]
    pub execution: Option<crate::wire::process_v1::Execution>,
}
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct RecoveryProcessResponse {
    #[serde(rename = "schemaVersion")]
    pub schema_version: i64,
    #[serde(rename = "activationReconciliationOwned")]
    pub activation_reconciliation_owned: bool,
    #[serde(rename = "capabilities", skip_serializing_if = "Option::is_none")]
    pub capabilities: Option<RecoveryCapabilities>,
    #[serde(
        rename = "activationOwners",
        skip_serializing_if = "crate::wire::empty_vec"
    )]
    pub activation_owners: Option<Vec<String>>,
    #[serde(rename = "inspection", skip_serializing_if = "Option::is_none")]
    pub inspection: Option<crate::wire::process_v1::Inspection>,
    #[serde(rename = "outcome", skip_serializing_if = "Option::is_none")]
    pub outcome: Option<crate::wire::process_v1::Outcome>,
}
