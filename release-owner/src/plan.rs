//! Pure frozen assessment/plan binding and journal construction. No grant is issued here.
use crate::{
    codec, digest,
    record::{self, fingerprint, require, safe_id, token},
    wire::{journal_v2 as j, plan_v1 as f, process_v1 as p},
    Result,
};
use std::collections::BTreeSet;
fn assessment(a: &p::ActionAssessment) -> Result<()> {
    let impacts = a.impacts.as_deref().unwrap_or_default();
    let consequences = a.consequences.as_deref().unwrap_or_default();
    require(
        safe_id(&a.action)
            && impacts.windows(2).all(|p| p[0] < p[1])
            && consequences.len() <= 64
            && consequences.iter().all(|s| record::text(s, 512, true)),
    )
}
pub fn assess(
    changed: bool,
    legacy_rollback: bool,
    activation_consequences: &[String],
) -> Result<p::ActionAssessment> {
    let mut consequences = None;
    if changed {
        let mut values = vec!["apply the exact typed migration and release activation plan".into()];
        if legacy_rollback {
            values.push("preserve applied migrations as backward-compatible no-ops".into());
        }
        values.extend_from_slice(activation_consequences);
        consequences = Some(values);
    }
    let a = p::ActionAssessment {
        action: "release.transition.v2".into(),
        effect: "mutation".into(),
        changed,
        impacts: Some(vec![
            "local-metadata".into(),
            "persistent-data".into(),
            "yard-runtime".into(),
        ]),
        recovery: "reversible".into(),
        consequences,
    };
    assessment(&a)?;
    Ok(a)
}
fn facts(f: &f::PlanFacts) -> Result<()> {
    require(
        safe_id(&f.goal.target)
            && record::direction(&f.goal.direction)
            && f.goal.target == f.releases.target,
    )?;
    record::pair(&f.releases)?;
    if f.goal.direction == "activate-previous" {
        let r = f.rollback_target.as_ref().ok_or(crate::Error::Invalid)?;
        require(
            record::semver(&r.version)
                && (r.registry_digest.is_empty() || fingerprint(&r.registry_digest)),
        )?;
    } else {
        require(f.rollback_target.is_none())?;
    }
    require(
        [
            &f.artifact_digest,
            &f.registry_digest,
            &f.catalog_digest,
            &f.observation_scope,
        ]
        .into_iter()
        .all(|s| fingerprint(s)),
    )?;
    assessment(&f.assessment)?;
    let decisions = f.decisions.as_deref().unwrap_or_default();
    let observations = f.observations.as_deref().unwrap_or_default();
    let intents = f.intents.as_deref().unwrap_or_default();
    let blockers = f.blockers.as_deref().unwrap_or_default();
    require(
        [
            decisions.len(),
            observations.len(),
            intents.len(),
            blockers.len(),
        ]
        .into_iter()
        .all(|n| n <= 256),
    )?;
    for d in decisions {
        require(
            safe_id(&d.resource)
                && (d.scope.is_empty() || safe_id(&d.scope))
                && matches!(
                    d.decision.as_str(),
                    "preserve" | "transform" | "canonicalize" | "reset" | "quarantine" | "block"
                )
                && record::text(&d.result, 256, false),
        )?;
    }
    for o in observations {
        require(safe_id(&o.resource) && safe_id(&o.class) && fingerprint(&o.fingerprint))?;
    }
    let mut seen = BTreeSet::new();
    for i in intents {
        require(
            safe_id(&i.id)
                && seen.insert(&i.id)
                && safe_id(&i.migration)
                && safe_id(&i.resource)
                && matches!(
                    i.decision.as_str(),
                    "preserve" | "transform" | "canonicalize" | "reset" | "quarantine"
                )
                && fingerprint(&i.expected)
                && fingerprint(&i.desired),
        )?;
    }
    for b in blockers {
        require(
            record::code(&b.code)
                && b.code != "ready"
                && (b.resource.is_empty() || safe_id(&b.resource))
                && record::text(&b.message, 512, true)
                && record::action(&b.retry),
        )?;
    }
    if let Some(replacement) = &f.replacement {
        record::replacement(replacement)?;
    }
    Ok(())
}
pub struct Plan {
    wire: f::PlanFacts,
    token: String,
}
impl Plan {
    pub fn new(wire: f::PlanFacts) -> Result<Self> {
        facts(&wire)?;
        crate::recovery::links(&wire.links)?;
        let token = format!("plan-v1-{}", digest(&codec::encode(&wire, false)?));
        Ok(Self { wire, token })
    }
    pub fn wire(&self) -> &f::PlanFacts {
        &self.wire
    }
    pub fn token(&self) -> &str {
        &self.token
    }
    /// Constructs a validated record from caller-supplied authorization evidence;
    /// permission to persist or execute still belongs to the release owner.
    pub fn journal(
        &self,
        transaction: String,
        authorization: &str,
        source_ingress: Option<j::SourceIngress>,
    ) -> Result<record::Journal> {
        self.journal_authorized(transaction, authorization, source_ingress, &self.token)
    }
    pub(crate) fn journal_authorized(
        &self,
        transaction: String,
        authorization: &str,
        source_ingress: Option<j::SourceIngress>,
        authorization_plan: &str,
    ) -> Result<record::Journal> {
        require(record::token(authorization_plan, "plan-v1-"))?;
        let f = &self.wire;
        let resume = ResumePlan::new(f::ResumePlanFacts {
            goal: f.goal.clone(),
            releases: f.releases.clone(),
            artifact_digest: f.artifact_digest.clone(),
            registry_digest: f.registry_digest.clone(),
            catalog_digest: f.catalog_digest.clone(),
            rollback_target: f.rollback_target.clone(),
            observation_scope: f.observation_scope.clone(),
            assessment: f.assessment.clone(),
            decisions: f.decisions.clone(),
            intents: f.intents.clone(),
            blockers: Some(vec![]),
            transaction: transaction.clone(),
            authorization_plan: authorization_plan.into(),
        })?;
        let steps = Some(
            f.intents
                .iter()
                .flatten()
                .map(|i| j::Step {
                    id: i.id.clone(),
                    migration: i.migration.clone(),
                    resource: i.resource.clone(),
                    decision: i.decision.clone(),
                    expected: i.expected.clone(),
                    desired: i.desired.clone(),
                    checkpoint: "intent".into(),
                    evidence: None,
                })
                .collect(),
        );
        let mut wire = j::Record {
            schema_version: 2,
            transaction,
            goal: j::Goal {
                target: f.goal.target.clone(),
                direction: f.goal.direction.clone(),
            },
            releases: f.releases.clone(),
            authorization_plan: authorization_plan.into(),
            resume_plan: resume.token,
            artifact_digest: f.artifact_digest.clone(),
            registry_digest: f.registry_digest.clone(),
            catalog_digest: f.catalog_digest.clone(),
            observation_scope: f.observation_scope.clone(),
            authorization_digest: digest(authorization.as_bytes()),
            intent_digest: String::new(),
            source_ingress,
            checkpoint: "authorized".into(),
            steps,
        };
        wire.intent_digest = record::Journal::intent_digest(&wire)?;
        record::Journal::new(wire)
    }
}
pub struct ResumePlan {
    wire: f::ResumePlanFacts,
    token: String,
}
impl ResumePlan {
    pub fn new(wire: f::ResumePlanFacts) -> Result<Self> {
        facts(&f::PlanFacts {
            goal: wire.goal.clone(),
            releases: wire.releases.clone(),
            artifact_digest: wire.artifact_digest.clone(),
            registry_digest: wire.registry_digest.clone(),
            catalog_digest: wire.catalog_digest.clone(),
            rollback_target: wire.rollback_target.clone(),
            observation_scope: wire.observation_scope.clone(),
            assessment: wire.assessment.clone(),
            decisions: wire.decisions.clone(),
            intents: wire.intents.clone(),
            blockers: wire.blockers.clone(),
            ..Default::default()
        })?;
        require(safe_id(&wire.transaction) && token(&wire.authorization_plan, "plan-v1-"))?;
        let token = format!("resume-v1-{}", digest(&codec::encode(&wire, false)?));
        Ok(Self { wire, token })
    }
    pub fn wire(&self) -> &f::ResumePlanFacts {
        &self.wire
    }
    pub fn token(&self) -> &str {
        &self.token
    }
}
