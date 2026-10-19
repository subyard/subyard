//! Frozen untrusted DTOs; these do not convey consent or effects.
use serde::{Deserialize, Serialize};
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct ResourceObservation {
    #[serde(rename = "resource")]
    pub resource: String,
    #[serde(rename = "class")]
    pub class: String,
    #[serde(rename = "fingerprint")]
    pub fingerprint: String,
}
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct PlannerStepIntent {
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
}
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct PlanFacts {
    #[serde(rename = "goal")]
    pub goal: crate::wire::process_v1::Goal,
    #[serde(rename = "releases")]
    pub releases: crate::wire::journal_v2::ReleasePair,
    #[serde(rename = "links")]
    pub links: crate::wire::recovery_v1::ReleaseLinks,
    #[serde(rename = "artifactDigest")]
    pub artifact_digest: String,
    #[serde(rename = "registryDigest")]
    pub registry_digest: String,
    #[serde(rename = "catalogDigest")]
    pub catalog_digest: String,
    #[serde(rename = "rollbackTarget", skip_serializing_if = "Option::is_none")]
    pub rollback_target: Option<RollbackTarget>,
    #[serde(rename = "observationScope")]
    pub observation_scope: String,
    #[serde(rename = "assessment")]
    pub assessment: crate::wire::process_v1::ActionAssessment,
    #[serde(rename = "decisions")]
    pub decisions: Option<Vec<crate::wire::process_v1::RedactedDecision>>,
    #[serde(rename = "observations")]
    pub observations: Option<Vec<ResourceObservation>>,
    #[serde(rename = "intents")]
    pub intents: Option<Vec<PlannerStepIntent>>,
    #[serde(rename = "blockers")]
    pub blockers: Option<Vec<crate::wire::process_v1::Blocker>>,
    #[serde(rename = "replacement", skip_serializing_if = "Option::is_none")]
    pub replacement: Option<crate::wire::process_v1::JournalReplacement>,
}
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct ResumePlanFacts {
    #[serde(rename = "goal")]
    pub goal: crate::wire::process_v1::Goal,
    #[serde(rename = "releases")]
    pub releases: crate::wire::journal_v2::ReleasePair,
    #[serde(rename = "artifactDigest")]
    pub artifact_digest: String,
    #[serde(rename = "registryDigest")]
    pub registry_digest: String,
    #[serde(rename = "catalogDigest")]
    pub catalog_digest: String,
    #[serde(rename = "rollbackTarget", skip_serializing_if = "Option::is_none")]
    pub rollback_target: Option<RollbackTarget>,
    #[serde(rename = "observationScope")]
    pub observation_scope: String,
    #[serde(rename = "assessment")]
    pub assessment: crate::wire::process_v1::ActionAssessment,
    #[serde(rename = "decisions")]
    pub decisions: Option<Vec<crate::wire::process_v1::RedactedDecision>>,
    #[serde(rename = "intents")]
    pub intents: Option<Vec<PlannerStepIntent>>,
    #[serde(rename = "blockers")]
    pub blockers: Option<Vec<crate::wire::process_v1::Blocker>>,
    #[serde(rename = "transaction")]
    pub transaction: String,
    #[serde(rename = "authorizationPlan")]
    pub authorization_plan: String,
}
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct RollbackTarget {
    #[serde(rename = "version")]
    pub version: String,
    #[serde(rename = "registryDigest", skip_serializing_if = "String::is_empty")]
    pub registry_digest: String,
}
