//! Frozen outcome reduction over one immutable facts set. These facts are
//! crate-private diagnostics, never deserializable mutation authorization.
use crate::{
    record,
    wire::{journal_v2 as j, plan_v1 as f, process_v1 as p, recovery_v1::ReleaseLinks},
};
use serde::Deserialize;
#[derive(Deserialize)]
#[serde(rename_all = "PascalCase")]
pub(crate) struct Facts {
    pub goal: p::Goal,
    pub releases: j::ReleasePair,
    pub links: ReleaseLinks,
    pub journal: Option<j::Record>,
    pub steps: Option<Vec<Step>>,
    pub current_plan: String,
    pub current_intents: Option<Vec<f::PlannerStepIntent>>,
    pub verified_authorization_plan: String,
    pub current_artifact_digest: String,
    pub current_registry_digest: String,
    pub current_catalog_digest: String,
    pub current_authorization_digest: String,
    pub fixed_point_verified: bool,
}
#[derive(Deserialize)]
pub(crate) struct Step {
    pub step: String,
    pub fingerprint: String,
}
pub(crate) fn operator(mut base: p::Outcome, code: &str, message: &str) -> p::Outcome {
    base.status = "operator-action-required".into();
    base.code = code.into();
    base.message = message.into();
    base.retry = "run yard update --check".into();
    base
}
pub(crate) fn ready(mut base: p::Outcome) -> p::Outcome {
    base.status = "ready".into();
    base.reached_goal = true;
    base.code = "ready".into();
    base.message = "the target release is active and the transition is verified".into();
    base.retry.clear();
    base
}
pub(crate) fn evaluate(f: &Facts) -> p::Outcome {
    if !record::safe_id(&f.goal.target) || !record::direction(&f.goal.direction) {
        return operator(
            p::Outcome::default(),
            "journal-invalid",
            "release goal is invalid",
        );
    }
    if record::pair(&f.releases).is_err() || f.goal.target != f.releases.target {
        return operator(
            p::Outcome::default(),
            "journal-invalid",
            "release pair is invalid",
        );
    }
    if crate::recovery::links(&f.links).is_err() {
        return operator(
            p::Outcome::default(),
            "activation-ambiguous",
            "release links are ambiguous",
        );
    }
    let mut base = p::Outcome {
        active: f.links.active.clone(),
        previous: f.links.previous.clone(),
        target: f.goal.target.clone(),
        ..Default::default()
    };
    let initial = f.links.active == f.releases.from && f.links.previous == f.releases.previous;
    let activated = f.links.active == f.releases.target
        && f.links.previous.as_deref() == Some(&f.releases.from);
    let Some(journal) = &f.journal else {
        if f.fixed_point_verified && (f.releases.from == f.releases.target && initial || activated)
        {
            return ready(base);
        }
        if initial {
            base.status = "migration-required".into();
            base.code = "transition-required".into();
            base.message = "the inspected release transition has not started".into();
            base.retry = "run yard update".into();
            return base;
        }
        if activated {
            return operator(
                base,
                "recovery-ambiguous",
                "the active target has no trusted completion evidence",
            );
        }
        return operator(
            base,
            "activation-ambiguous",
            "release links do not match a known transition checkpoint",
        );
    };
    if record::Journal::new(journal.clone()).is_err()
        || journal.goal.target != f.goal.target
        || journal.goal.direction != f.goal.direction
        || journal.releases != f.releases
    {
        return operator(
            base,
            "journal-invalid",
            "release transition journal is invalid or foreign",
        );
    }
    base.transaction = Some(journal.transaction.clone());
    if !record::token(&f.current_plan, "resume-v1-")
        || !record::token(&f.verified_authorization_plan, "plan-v1-")
        || [
            &f.current_artifact_digest,
            &f.current_registry_digest,
            &f.current_catalog_digest,
            &f.current_authorization_digest,
        ]
        .iter()
        .any(|v| !record::fingerprint(v))
    {
        return operator(
            base,
            "journal-invalid",
            "current release transition bindings are invalid",
        );
    }
    if f.current_plan != journal.resume_plan
        || f.verified_authorization_plan != journal.authorization_plan
        || f.current_artifact_digest != journal.artifact_digest
        || f.current_registry_digest != journal.registry_digest
        || f.current_catalog_digest != journal.catalog_digest
        || f.current_authorization_digest != journal.authorization_digest
    {
        return operator(
            base,
            "plan-stale",
            "the authorized release transition plan is stale",
        );
    }
    let intents = f.current_intents.as_deref().unwrap_or_default();
    let mut ids = std::collections::BTreeSet::new();
    if intents.len() > 256
        || intents.iter().any(|i| {
            !record::safe_id(&i.id)
                || !ids.insert(&i.id)
                || !record::safe_id(&i.migration)
                || !record::safe_id(&i.resource)
                || !matches!(
                    i.decision.as_str(),
                    "preserve" | "transform" | "canonicalize" | "reset" | "quarantine"
                )
                || !record::fingerprint(&i.expected)
                || !record::fingerprint(&i.desired)
        })
    {
        return operator(
            base,
            "journal-invalid",
            "current planner step intents are invalid",
        );
    }
    let steps = journal.steps.as_deref().unwrap_or_default();
    if intents.len() != steps.len()
        || intents.iter().zip(steps).any(|(i, s)| {
            i.id != s.id
                || i.migration != s.migration
                || i.resource != s.resource
                || i.decision != s.decision
                || i.expected != s.expected
                || i.desired != s.desired
        })
    {
        return operator(
            base,
            "plan-stale",
            "current planner step intents do not match the authorized journal",
        );
    }
    if !crate::links::allowed(&journal.checkpoint, &f.links, &f.releases) {
        return operator(
            base,
            "activation-ambiguous",
            "release links do not match the journaled activation checkpoint",
        );
    }
    let mut observed = std::collections::BTreeMap::new();
    for observation in f.steps.iter().flatten() {
        if !record::safe_id(&observation.step)
            || !record::fingerprint(&observation.fingerprint)
            || !steps.iter().any(|s| s.id == observation.step)
            || observed
                .insert(&observation.step, &observation.fingerprint)
                .is_some()
        {
            return operator(
                base,
                "recovery-ambiguous",
                "observed migration resources are incomplete or foreign",
            );
        }
    }
    for step in steps {
        if step.checkpoint == "verified" {
            continue;
        }
        let Some(actual) = observed.get(&step.id) else {
            return operator(
                base,
                "recovery-ambiguous",
                "observed migration resources are incomplete or foreign",
            );
        };
        if **actual != step.expected && **actual != step.desired {
            return operator(
                base,
                "migration-stale",
                "an observed migration resource does not match a journaled checkpoint",
            );
        }
    }
    if journal.checkpoint == "complete" && f.fixed_point_verified {
        return ready(base);
    }
    base.status = "recovering".into();
    base.code = "recovery-pending".into();
    base.message = "the authorized release transition can resume from observed facts".into();
    base.retry = "run yard update".into();
    base
}
pub(crate) fn inspection(
    facts: &f::PlanFacts,
    journal: Option<&j::Record>,
    completed: bool,
    fixed: bool,
    conversion: bool,
    consent: bool,
    warnings: &[String],
) -> crate::Result<p::Inspection> {
    let current = journal.filter(|j| {
        j.checkpoint != "complete"
            || j.goal.target == facts.goal.target && j.goal.direction == facts.goal.direction
    });
    let base = p::Outcome {
        active: facts.links.active.clone(),
        previous: facts.links.previous.clone(),
        target: facts.goal.target.clone(),
        transaction: current.map(|j| j.transaction.clone()),
        ..Default::default()
    };
    let mut outcome = if let Some(b) = facts.blockers.iter().flatten().next() {
        let mut out = operator(base, &b.code, &b.message);
        out.retry = b.retry.clone();
        out
    } else if completed && !conversion {
        if fixed {
            ready(base)
        } else {
            p::Outcome{status:"recovering".into(),code:"recovery-pending".into(),message:"activation repair requires a new plan and authorization; the completed transaction is history".into(),retry:"run yard update".into(),..base}
        }
    } else if current.is_some_and(|j| j.checkpoint != "complete") {
        p::Outcome {
            status: "recovering".into(),
            code: "recovery-pending".into(),
            message: "the authorized release transition can resume from observed facts".into(),
            retry: "run yard update".into(),
            ..base
        }
    } else if conversion {
        p::Outcome {
            status: "migration-required".into(),
            code: "transition-required".into(),
            message: "compact migration history checkpoint publication requires authorization"
                .into(),
            retry: "run yard update".into(),
            transaction: None,
            ..base
        }
    } else {
        evaluate(&Facts {
            goal: facts.goal.clone(),
            releases: facts.releases.clone(),
            links: facts.links.clone(),
            journal: current.cloned(),
            steps: None,
            current_plan: current.map(|j| j.resume_plan.clone()).unwrap_or_default(),
            current_intents: facts.intents.clone(),
            verified_authorization_plan: current
                .map(|j| j.authorization_plan.clone())
                .unwrap_or_default(),
            current_artifact_digest: facts.artifact_digest.clone(),
            current_registry_digest: facts.registry_digest.clone(),
            current_catalog_digest: facts.catalog_digest.clone(),
            current_authorization_digest: current
                .map(|j| j.authorization_digest.clone())
                .unwrap_or_default(),
            fixed_point_verified: fixed,
        })
    };
    if outcome.status == "migration-required" && completed {
        outcome.status = "recovering".into();
        outcome.code = "recovery-pending".into();
        outcome.transaction = current.map(|j| j.transaction.clone());
        outcome.message="activation repair requires a new plan and authorization; the completed transaction is history".into();
    }
    let mut warnings = warnings.to_vec();
    warnings.sort();
    warnings.dedup();
    outcome.warnings = if warnings.is_empty() {
        None
    } else {
        Some(warnings)
    };
    let resumable =
        current.filter(|j| j.checkpoint != "complete" || conversion && consent && fixed);
    if resumable.is_some_and(|j| j.checkpoint == "complete") {
        outcome = p::Outcome {
            status: "recovering".into(),
            code: "recovery-pending".into(),
            message: "the authorized migration checkpoint publication can resume".into(),
            retry: "run yard update".into(),
            warnings: None,
            ..outcome
        };
    }
    Ok(p::Inspection {
        plan: if let Some(j) = resumable {
            j.resume_plan.clone()
        } else {
            crate::plan::Plan::new(facts.clone())?.token().into()
        },
        assessment: facts.assessment.clone(),
        decisions: facts.decisions.clone(),
        blockers: facts.blockers.clone(),
        resume: resumable.map(|j| j.transaction.clone()),
        outcome: Some(outcome),
    })
}
impl crate::owner::Owner {
    pub(crate) fn projected_inspection(&self, o: &crate::owner::Observation) -> p::Inspection {
        let f = o.plan.wire();
        let journal = o
            .journal
            .as_ref()
            .filter(|_| f.replacement.is_none())
            .map(|j| j.wire());
        let quiet = self.native_quiescent(journal).is_ok();
        let complete = self
            .store
            .migration_ledger(&self.registry, &self.registry_digest)
            .is_ok_and(|l| {
                self.registry
                    .history()
                    .complete(l.ledger().authority())
                    .is_ok()
            });
        let initial = f.links.active == f.releases.from && f.links.previous == f.releases.previous;
        let activated = f.links.active == f.releases.target
            && f.links.previous.as_deref() == Some(f.releases.from.as_str());
        let fixed = quiet
            && complete
            && o.work.is_empty()
            && o.targets.iter().all(|t| t.state != "stale")
            && if f.releases.from == f.releases.target {
                initial
            } else {
                activated
            };
        let completed = journal.is_some_and(|j| {
            j.checkpoint == "complete"
                && j.goal.target == f.goal.target
                && j.goal.direction == f.goal.direction
                && j.artifact_digest == f.artifact_digest
                && j.registry_digest == f.registry_digest
                && j.catalog_digest == f.catalog_digest
                && (j.releases == f.releases
                    && crate::links::allowed("complete", &f.links, &j.releases)
                    || f.releases.from == j.releases.target
                        && f.releases.target == j.releases.target
                        && initial
                        && crate::links::allowed("complete", &f.links, &j.releases))
        });
        let conversion = f
            .decisions
            .iter()
            .flatten()
            .any(|d| d.resource == "migration-history.checkpoint" && d.decision == "transform");
        let mut result = inspection(f, journal, completed, fixed, conversion, false, &[])
            .unwrap_or_else(|_| p::Inspection {
                plan: o.plan.token().into(),
                assessment: f.assessment.clone(),
                outcome: Some(operator(
                    p::Outcome {
                        active: f.links.active.clone(),
                        previous: f.links.previous.clone(),
                        target: f.goal.target.clone(),
                        ..Default::default()
                    },
                    "recovery-ambiguous",
                    "release facts cannot be observed safely",
                )),
                ..Default::default()
            });
        if self.store.reader.health().is_err() || !quiet {
            if let Some(outcome) = result.outcome.take() {
                result.outcome = Some(operator(
                    outcome,
                    "recovery-ambiguous",
                    "release outcome cannot be proved safely",
                ));
            }
        }
        result
    }
}
pub(crate) fn recovering(
    links: &crate::wire::recovery_v1::ReleaseLinks,
    target: &str,
    transaction: &str,
    message: &str,
) -> p::Outcome {
    p::Outcome {
        status: "recovering".into(),
        active: links.active.clone(),
        previous: links.previous.clone(),
        target: target.into(),
        transaction: Some(transaction.into()),
        code: "recovery-pending".into(),
        message: message.into(),
        retry: "review and confirm the fresh plan with yard migrate or yard update".into(),
        ..Default::default()
    }
}

// Preserve the owning failure protocol: an invalid/unobservable component
// result permits only a link guard, never another whole release observation.
// These closures perform reads; the result conveys no mutation authority.
pub(crate) fn reobserve_activation_failure<A, F>(
    capture: impl FnOnce() -> Option<A>,
    observe: impl FnOnce() -> Option<F>,
    guard: impl FnOnce() -> Option<ReleaseLinks>,
) -> (Option<A>, Option<F>, Option<ReleaseLinks>) {
    match capture() {
        Some(after) => (Some(after), observe(), None),
        None => (None, None, guard()),
    }
}

pub(crate) struct ActivationFailure<'a> {
    pub journal: &'a j::Record,
    pub owner: &'a str,
    pub phase: &'a str,
    pub before: &'a str,
    pub after: Option<(&'a str, bool)>,
    pub links: Option<&'a ReleaseLinks>,
    pub blockers: &'a [p::Blocker],
    pub current: bool,
    pub warnings: &'a [String],
}
pub(crate) fn activation_failure(f: &ActivationFailure<'_>) -> p::Outcome {
    let mut out = p::Outcome {
        target: f.journal.goal.target.clone(),
        transaction: Some(f.journal.transaction.clone()),
        warnings: if f.warnings.is_empty() {
            None
        } else {
            Some(f.warnings.to_vec())
        },
        ..Default::default()
    };
    if let Some(l) = f.links {
        out.active = l.active.clone();
        out.previous = l.previous.clone();
    }
    let Some((actual, converged)) = f.after else {
        return operator(
            out,
            "recovery-ambiguous",
            &format!(
                "activation reconciler {:?} result cannot be observed safely",
                f.owner
            ),
        );
    };
    if f.links.is_none() {
        return operator(
            out,
            "recovery-ambiguous",
            "release facts cannot be observed after activation reconciliation",
        );
    }
    if let Some(b) = f.blockers.first() {
        out = operator(out, &b.code, &b.message);
        out.retry = b.retry.clone();
        return out;
    }
    if !f.current {
        return operator(
            out,
            "recovery-ambiguous",
            "protected transition facts changed during activation reconciliation",
        );
    }
    if actual != f.before && !converged {
        return operator(
            out,
            "recovery-ambiguous",
            &format!(
                "activation reconciler {:?} reached an unknown state during {}",
                f.owner, f.phase
            ),
        );
    }
    out.status = "recovering".into();
    out.code = "dependency-unavailable".into();
    out.message = format!(
        "activation reconciler {:?} failed during {}",
        f.owner, f.phase
    );
    out.retry = "run yard update".into();
    out
}
pub(crate) fn failure_diagnostic(out: &mut p::Outcome, message: &str, retry: &str) {
    if out.status == "recovering" {
        if !message.is_empty() {
            out.message = message.into()
        }
        if !retry.is_empty() {
            out.retry = retry.into()
        }
        return;
    }
    failure_cause(out, message)
}
pub(crate) fn failure_cause(out: &mut p::Outcome, message: &str) {
    if message.is_empty() || message == out.message {
        return;
    }
    fn bounded(s: &str, max: usize) -> String {
        if s.len() <= max {
            return s.into();
        }
        let mut end = max - 3;
        while !s.is_char_boundary(end) {
            end -= 1
        }
        s[..end].trim_end().to_owned() + "..."
    }
    let mut recovery = out.message.clone();
    let mut public = message.to_owned();
    if public.len() + recovery.len() + 2 > 512 {
        recovery = bounded(&recovery, 256);
        public = bounded(&public, 512 - recovery.len() - 2)
    }
    out.message = public + "; " + &recovery;
}
#[cfg(test)]
mod activation_failure_tests {
    use super::*;
    #[derive(Deserialize)]
    struct After {
        actual: String,
        converged: bool,
    }
    #[derive(Deserialize)]
    #[serde(rename_all = "camelCase")]
    struct Vector {
        journal: j::Record,
        before: String,
        after: Option<After>,
        links: ReleaseLinks,
        outcome: p::Outcome,
        observe_calls: usize,
    }
    #[test]
    fn actual_go_failure_reducer() {
        let Ok(path) = std::env::var("SUBYARD_RUST_ACTIVATION_FAILURE_CORPUS") else {
            return;
        };
        let raw = std::fs::read(path).unwrap();
        let rows: Vec<Vector> = serde_json::from_slice(&raw).unwrap();
        assert_eq!(rows.len(), 5);
        for (n, c) in rows.iter().enumerate() {
            let calls = std::cell::RefCell::new(Vec::new());
            let (after, observed, guarded) = reobserve_activation_failure(
                || {
                    calls.borrow_mut().push("component-observe");
                    c.after.as_ref()
                },
                || {
                    calls.borrow_mut().push("release-observe");
                    calls.borrow_mut().push("component-observe");
                    Some(&c.links)
                },
                || {
                    calls.borrow_mut().push("link-guard");
                    Some(c.links.clone())
                },
            );
            let f = ActivationFailure {
                journal: &c.journal,
                owner: "post-error-runtime",
                phase: "reconcile",
                before: &c.before,
                after: after.map(|a| (a.actual.as_str(), a.converged)),
                links: observed.or(guarded.as_ref()),
                blockers: &[],
                current: true,
                warnings: &[],
            };
            let mut actual = activation_failure(&f);
            if actual.status == "operator-action-required" {
                failure_diagnostic(
                    &mut actual,
                    "activation reconciler \"post-error-runtime\" failed during reconcile",
                    "",
                )
            };
            assert_eq!(
                crate::codec::encode(&actual, false).unwrap(),
                crate::codec::encode(&c.outcome, false).unwrap(),
                "failure {n}"
            );
            assert_eq!(
                calls
                    .borrow()
                    .iter()
                    .filter(|v| **v == "component-observe")
                    .count(),
                c.observe_calls
            );
            assert_eq!(
                calls.borrow().last().copied(),
                Some(if c.after.is_some() {
                    "component-observe"
                } else {
                    "link-guard"
                })
            );
        }
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    #[derive(Deserialize)]
    struct Vector {
        facts: Facts,
        outcome: p::Outcome,
    }
    #[derive(Deserialize)]
    struct InspectionInput {
        facts: f::PlanFacts,
        journal: Option<j::Record>,
        completed: bool,
        fixed: bool,
        conversion: bool,
        consent: bool,
        warnings: Vec<String>,
    }
    #[derive(Deserialize)]
    struct InspectionVector {
        input: InspectionInput,
        inspection: p::Inspection,
    }
    #[test]
    fn actual_go_same_observation_inspection() {
        let Ok(path) = std::env::var("SUBYARD_RUST_INSPECTION_CORPUS") else {
            return;
        };
        let raw = std::fs::read(path).unwrap();
        assert!(raw.len() <= 8 << 20);
        let vectors: Vec<InspectionVector> = serde_json::from_slice(&raw).unwrap();
        assert!(!vectors.is_empty() && vectors.len() <= 4096);
        for (n, v) in vectors.into_iter().enumerate() {
            let f = v.input;
            let actual = inspection(
                &f.facts,
                f.journal.as_ref(),
                f.completed,
                f.fixed,
                f.conversion,
                f.consent,
                &f.warnings,
            )
            .unwrap();
            assert_eq!(
                crate::codec::encode(&actual, false).unwrap(),
                crate::codec::encode(&v.inspection, false).unwrap(),
                "inspection {n}"
            );
        }
    }
    #[test]
    fn actual_go_same_facts_whole_outcomes() {
        let Ok(path) = std::env::var("SUBYARD_RUST_REDUCER_CORPUS") else {
            return;
        };
        let raw = std::fs::read(path).unwrap();
        assert!(raw.len() <= 16 << 20);
        let vectors: Vec<Vector> = serde_json::from_slice(&raw).unwrap();
        assert!(!vectors.is_empty() && vectors.len() <= 4096);
        for (n, v) in vectors.into_iter().enumerate() {
            assert_eq!(
                crate::codec::encode(&evaluate(&v.facts), false).unwrap(),
                crate::codec::encode(&v.outcome, false).unwrap(),
                "vector{n}"
            )
        }
    }
}
