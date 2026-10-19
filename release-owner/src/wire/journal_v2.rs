//! Frozen wire DTOs only; deserialization does not grant authority.
use serde::{Deserialize, Serialize};
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
pub struct ReleasePair {
    #[serde(rename = "from")]
    pub from: String,
    #[serde(rename = "previous", skip_serializing_if = "Option::is_none")]
    pub previous: Option<String>,
    #[serde(rename = "target")]
    pub target: String,
}
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct SourceIngress {
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
pub struct Record {
    #[serde(rename = "schemaVersion")]
    pub schema_version: i64,
    #[serde(rename = "transaction")]
    pub transaction: String,
    #[serde(rename = "goal")]
    pub goal: Goal,
    #[serde(rename = "releases")]
    pub releases: ReleasePair,
    #[serde(rename = "authorizationPlan")]
    pub authorization_plan: String,
    #[serde(rename = "resumePlan")]
    pub resume_plan: String,
    #[serde(rename = "artifactDigest")]
    pub artifact_digest: String,
    #[serde(rename = "registryDigest")]
    pub registry_digest: String,
    #[serde(rename = "catalogDigest")]
    pub catalog_digest: String,
    #[serde(rename = "observationScope")]
    pub observation_scope: String,
    #[serde(rename = "authorizationDigest")]
    pub authorization_digest: String,
    #[serde(rename = "intentDigest")]
    pub intent_digest: String,
    #[serde(rename = "sourceIngress", skip_serializing_if = "Option::is_none")]
    pub source_ingress: Option<SourceIngress>,
    #[serde(rename = "checkpoint")]
    pub checkpoint: String,
    #[serde(rename = "steps")]
    pub steps: Option<Vec<Step>>,
}
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct Step {
    #[serde(rename = "id")]
    pub id: String,
    #[serde(rename = "migration")]
    pub migration: String,
    #[serde(rename = "resource")]
    pub resource: String,
    #[serde(rename = "decision")]
    pub decision: String,
    #[serde(rename = "expectedFingerprint")]
    pub expected: String,
    #[serde(rename = "desiredFingerprint")]
    pub desired: String,
    #[serde(rename = "checkpoint")]
    pub checkpoint: String,
    #[serde(rename = "evidence", skip_serializing_if = "Option::is_none")]
    pub evidence: Option<Evidence>,
}
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct Evidence {
    #[serde(rename = "schemaVersion")]
    pub schema_version: i64,
    #[serde(rename = "transaction")]
    pub transaction: String,
    #[serde(rename = "releases")]
    pub releases: ReleasePair,
    #[serde(rename = "step")]
    pub step: String,
    #[serde(rename = "expectedFingerprint")]
    pub expected: String,
    #[serde(rename = "desiredFingerprint")]
    pub desired: String,
    #[serde(rename = "observedFingerprint")]
    pub observed: String,
    #[serde(
        rename = "recoveryFingerprint",
        skip_serializing_if = "String::is_empty"
    )]
    pub recovery: String,
    #[serde(rename = "checkpoint")]
    pub checkpoint: String,
}
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct Archive {
    #[serde(rename = "schemaVersion")]
    pub schema_version: i64,
    #[serde(rename = "authorizationPlan")]
    pub authorization_plan: String,
    #[serde(rename = "replacement")]
    pub replacement: Replacement,
    #[serde(rename = "journal")]
    pub journal: Record,
}
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct Replacement {
    #[serde(rename = "transaction")]
    pub transaction: String,
    #[serde(rename = "fingerprint")]
    pub fingerprint: String,
    #[serde(rename = "reason")]
    pub reason: String,
    #[serde(rename = "sourceVersion", skip_serializing_if = "String::is_empty")]
    pub source_version: String,
}
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct CompatibilityEvidence {
    #[serde(rename = "schemaVersion")]
    pub schema_version: i64,
    #[serde(rename = "kind")]
    pub kind: String,
    #[serde(rename = "identity")]
    pub identity: String,
    #[serde(rename = "from")]
    pub from: String,
    #[serde(rename = "previous")]
    pub previous: String,
}
