//! Frozen untrusted DTOs; these do not convey consent or effects.
use crate::wire::recovery_v1::*;
use serde::{Deserialize, Serialize};
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct RecoveryFrontier {
    #[serde(rename = "schemaVersion")]
    pub schema_version: i64,
    #[serde(rename = "replacement")]
    pub replacement: ActivationOnlyRecoveryRequest,
    #[serde(rename = "generation")]
    pub generation: u64,
    #[serde(rename = "cancellation", skip_serializing_if = "Option::is_none")]
    pub cancellation: Option<RecoveryLifecycleRecord>,
}
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct RecoveryCancellation {
    #[serde(rename = "basePlan")]
    pub base_plan: String,
    #[serde(rename = "plan")]
    pub plan: String,
    #[serde(rename = "authorizationDigest")]
    pub authorization_digest: String,
}
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct RecoveryLifecycleRecord {
    #[serde(rename = "schemaVersion")]
    pub schema_version: i64,
    #[serde(rename = "receipt")]
    pub receipt: RecoveryReceiptV1,
    #[serde(rename = "completed", skip_serializing_if = "Option::is_none")]
    pub completed: Option<crate::wire::journal_v2::Record>,
    #[serde(rename = "cancellation", skip_serializing_if = "Option::is_none")]
    pub cancellation: Option<RecoveryCancellation>,
    #[serde(
        rename = "predecessorCompleted",
        skip_serializing_if = "Option::is_none"
    )]
    pub predecessor_completed: Option<crate::wire::journal_v2::Record>,
}
