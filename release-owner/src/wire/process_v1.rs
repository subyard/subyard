//! Frozen wire DTOs only; deserialization does not grant authority.
use serde::{Deserialize, Serialize};
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct SourceIngressRequest {
    #[serde(rename = "schemaVersion")]
    pub schema_version: i64,
    #[serde(rename = "kind")]
    pub kind: String,
    #[serde(rename = "sourceRoot")]
    pub source_root: String,
    #[serde(rename = "dataHome")]
    pub data_home: String,
    #[serde(rename = "binDir")]
    pub bin_dir: String,
    #[serde(rename = "rc")]
    pub rc: String,
    #[serde(rename = "loginRC")]
    pub login_rc: String,
}
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct Request {
    #[serde(rename = "schemaVersion")]
    pub schema_version: i64,
    #[serde(rename = "mode")]
    pub mode: String,
    #[serde(rename = "runtimeRoot")]
    pub runtime_root: String,
    #[serde(rename = "configHome")]
    pub config_home: String,
    #[serde(rename = "yard")]
    pub yard: String,
    #[serde(rename = "target")]
    pub target: String,
    #[serde(rename = "direction")]
    pub direction: String,
    #[serde(rename = "artifactDigest")]
    pub artifact_digest: String,
    #[serde(rename = "registryDigest", skip_serializing_if = "String::is_empty")]
    pub registry_digest: String,
    #[serde(
        rename = "inheritedSettingIds",
        skip_serializing_if = "crate::wire::empty_vec"
    )]
    pub inherited_setting_ids: Option<Vec<String>>,
    #[serde(rename = "sourceIngress", skip_serializing_if = "Option::is_none")]
    pub source_ingress: Option<SourceIngressRequest>,
    #[serde(rename = "replacement", skip_serializing_if = "Option::is_none")]
    pub replacement: Option<JournalReplacement>,
    #[serde(rename = "execution", skip_serializing_if = "Option::is_none")]
    pub execution: Option<Execution>,
}
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct Response {
    #[serde(rename = "schemaVersion")]
    pub schema_version: i64,
    #[serde(rename = "activationReconciliationOwned")]
    pub activation_reconciliation_owned: bool,
    #[serde(rename = "inspection", skip_serializing_if = "Option::is_none")]
    pub inspection: Option<Inspection>,
    #[serde(rename = "outcome", skip_serializing_if = "Option::is_none")]
    pub outcome: Option<Outcome>,
}
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct Goal {
    #[serde(rename = "target")]
    pub target: String,
    #[serde(rename = "direction")]
    pub direction: String,
}
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct ActionAssessment {
    #[serde(rename = "action")]
    pub action: String,
    #[serde(rename = "effect")]
    pub effect: String,
    #[serde(rename = "changed")]
    pub changed: bool,
    #[serde(rename = "impacts", skip_serializing_if = "crate::wire::empty_vec")]
    pub impacts: Option<Vec<String>>,
    #[serde(rename = "recovery")]
    pub recovery: String,
    #[serde(
        rename = "consequences",
        skip_serializing_if = "crate::wire::empty_vec"
    )]
    pub consequences: Option<Vec<String>>,
}
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct RedactedDecision {
    #[serde(rename = "resource")]
    pub resource: String,
    #[serde(rename = "scope", skip_serializing_if = "String::is_empty")]
    pub scope: String,
    #[serde(rename = "decision")]
    pub decision: String,
    #[serde(rename = "result", skip_serializing_if = "String::is_empty")]
    pub result: String,
}
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct Blocker {
    #[serde(rename = "code")]
    pub code: String,
    #[serde(rename = "resource", skip_serializing_if = "String::is_empty")]
    pub resource: String,
    #[serde(rename = "message")]
    pub message: String,
    #[serde(rename = "retry")]
    pub retry: String,
}
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct Inspection {
    #[serde(rename = "plan")]
    pub plan: String,
    #[serde(rename = "assessment")]
    pub assessment: ActionAssessment,
    #[serde(rename = "decisions", skip_serializing_if = "crate::wire::empty_vec")]
    pub decisions: Option<Vec<RedactedDecision>>,
    #[serde(rename = "blockers", skip_serializing_if = "crate::wire::empty_vec")]
    pub blockers: Option<Vec<Blocker>>,
    #[serde(rename = "resume", skip_serializing_if = "Option::is_none")]
    pub resume: Option<String>,
    #[serde(rename = "outcome", skip_serializing_if = "Option::is_none")]
    pub outcome: Option<Outcome>,
}
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct Execution {
    #[serde(rename = "plan")]
    pub plan: String,
    #[serde(rename = "authorization")]
    pub authorization: String,
}
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct Outcome {
    #[serde(rename = "status")]
    pub status: String,
    #[serde(rename = "reachedGoal")]
    pub reached_goal: bool,
    #[serde(rename = "active")]
    pub active: String,
    #[serde(rename = "previous", skip_serializing_if = "Option::is_none")]
    pub previous: Option<String>,
    #[serde(rename = "target")]
    pub target: String,
    #[serde(rename = "code")]
    pub code: String,
    #[serde(rename = "message")]
    pub message: String,
    #[serde(rename = "retry", skip_serializing_if = "String::is_empty")]
    pub retry: String,
    #[serde(rename = "transaction", skip_serializing_if = "Option::is_none")]
    pub transaction: Option<String>,
    #[serde(rename = "warnings", skip_serializing_if = "crate::wire::empty_vec")]
    pub warnings: Option<Vec<String>>,
}
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct JournalReplacement {
    #[serde(rename = "transaction")]
    pub transaction: String,
    #[serde(rename = "fingerprint")]
    pub fingerprint: String,
    #[serde(rename = "reason")]
    pub reason: String,
    #[serde(rename = "sourceVersion", skip_serializing_if = "String::is_empty")]
    pub source_version: String,
}
