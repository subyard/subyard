//! Consented activation-only successor orchestration. Ordinary frozen recovery
//! receipts remain distinct from native invocation continuation evidence.
use crate::{
    codec, digest,
    graph::State,
    inventory::Namespace,
    owner::{Grant, Observation, Owner},
    plan::Plan,
    record::{self, require},
    recovery::{self, Receipt},
    wire::{process_v1 as p, recovery_v1 as r},
    Error, Result,
};
use std::time::{Duration, Instant};
struct Review {
    observation: Observation,
    facts: crate::wire::plan_v1::PlanFacts,
    base: String,
    token: String,
    reservation: String,
    plans: Vec<r::RecoveryNativePlan>,
    ledger: String,
    request: r::ActivationOnlyRecoveryRequest,
    contract: String,
}
pub struct PreparedRecovery<'a> {
    owner: &'a Owner,
    review: Review,
    grant: Grant,
    created: Instant,
}
pub struct ConsentedRecovery<'a> {
    owner: &'a Owner,
    review: Review,
    grant: Grant,
}
impl Owner {
    fn recovery_review(
        &self,
        request: r::ActivationOnlyRecoveryRequest,
        contract: &str,
    ) -> Result<Review> {
        require(matches!(
            contract,
            recovery::ACTIVATION_V1 | recovery::ACTIVATION_V2
        ))?;
        recovery::selection(&request)?;
        let mut observation = self.observe_recovery(p::Goal {
            target: self.options.releases.target.clone(),
            direction: "activate-target".into(),
        })?;
        let before = observation.journal.as_ref().ok_or(Error::Invalid)?.wire();
        require(
            before.transaction == request.transaction
                && observation.journal_snapshot.fingerprint() == request.fingerprint
                && before.checkpoint == "reconciling"
                && before.goal.direction == "activate-target"
                && before.source_ingress.is_none()
                && before.steps.iter().flatten().count() == 0
                && !recovery::recovery_transaction(&before.transaction)
                && before.artifact_digest == self.options.artifact_digest
                && before.registry_digest == self.registry_digest
                && before.catalog_digest == self.catalog.digest()
                && observation.plan.wire().links.active == before.goal.target
                && observation.plan.wire().observation_scope != before.observation_scope
                && observation.work.is_empty()
                && observation.plan.wire().blockers.iter().flatten().count() == 0,
        )?;
        self.native_quiescent(Some(before))?;
        require(!self.store.native_cleanup_pending()?)?;
        let ledger = self
            .store
            .migration_ledger(&self.registry, &self.registry_digest)?;
        self.registry
            .history()
            .complete(ledger.ledger().authority())
            .map_err(|_| Error::Invalid)?;
        let ledger = crate::ledger_execution::semantic_snapshot(&ledger)?;
        require(ledger.bytes().is_some())?;
        let mut plans = vec![];
        for source in observation
            .plan
            .wire()
            .observations
            .iter()
            .flatten()
            .filter(|o| o.class == "activation-reconciler-v1")
        {
            let id = source
                .resource
                .strip_prefix("activation.")
                .ok_or(Error::Invalid)?;
            let targets = observation
                .targets
                .iter()
                .filter(|t| t.owner == id)
                .cloned()
                .collect::<Vec<_>>();
            let facts = crate::activation::assess(
                id,
                &self.options.releases.target,
                &self.options.artifact_digest,
                &observation.plan.wire().links.active,
                &targets,
                true,
            )?;
            plans.push(r::RecoveryNativePlan {
                id: id.into(),
                binding: crate::bundle::binding(&[], &targets)?,
                actual: facts.actual,
                desired: facts.desired,
                converged: facts.converged,
            });
        }
        plans.sort_by(|a, b| a.id.cmp(&b.id));
        require(!plans.is_empty())?;
        let mut execution = observation.plan.wire().clone();
        let yard_set = execution
            .observations
            .iter()
            .flatten()
            .filter(|o| o.resource == "native.yard-set")
            .cloned()
            .collect::<Vec<_>>();
        execution.observations = Some(vec![
            crate::wire::plan_v1::ResourceObservation {
                resource: "recovery.predecessor".into(),
                class: contract.into(),
                fingerprint: request.fingerprint.clone(),
            },
            crate::wire::plan_v1::ResourceObservation {
                resource: "recovery.ledger".into(),
                class: "completed-ledger-v2".into(),
                fingerprint: ledger.fingerprint(),
            },
        ]);
        execution.observations.as_mut().unwrap().extend(yard_set);
        for plan in &plans {
            execution.observations.as_mut().unwrap().push(
                crate::wire::plan_v1::ResourceObservation {
                    resource: format!("activation.{}", plan.id),
                    class: "native-recovery-plan-v1".into(),
                    fingerprint: digest(&codec::encode(plan, false)?),
                },
            );
        }
        // The private bundle binding is an additional exact native read-set input.
        execution
            .observations
            .as_mut()
            .unwrap()
            .push(crate::wire::plan_v1::ResourceObservation {
                resource: "native.prepared-recipes".into(),
                class: "release-native-v1".into(),
                fingerprint: crate::bundle::binding(&[], &observation.targets)?,
            });
        let consequences = execution
            .assessment
            .consequences
            .clone()
            .unwrap_or_default();
        execution.assessment = crate::plan::assess(true, false, &consequences)?;
        observation.plan = Plan::new(execution.clone())?;
        let mut facts = execution;
        let mut reviewed=vec!["preserve the interrupted journal as immutable evidence and authorize a new activation-only transaction; keep migration ledger and runtime links".into()];
        if contract == recovery::ACTIVATION_V2 {
            reviewed.push("retire only an exact unselected recovery reservation and preserve its authorized cancellation evidence".into());
        }
        reviewed.extend(consequences);
        facts.assessment = crate::plan::assess(true, false, &reviewed)?;
        let base = Plan::new(facts.clone())?.token().to_owned();
        let reservation = if contract == recovery::ACTIVATION_V2 {
            State::read(&self.store)?.reservation(&request, &base)?
        } else {
            String::new()
        };
        let token = recovery::bind_lifecycle_plan(&base, &reservation)?;
        self.store.reader.health()?;
        Ok(Review {
            observation,
            facts,
            base,
            token,
            reservation,
            plans,
            ledger: ledger.fingerprint(),
            request,
            contract: contract.into(),
        })
    }
    pub fn prepare_recovery(
        &self,
        request: r::ActivationOnlyRecoveryRequest,
        contract: &str,
        grant: Grant,
    ) -> Result<PreparedRecovery<'_>> {
        Ok(PreparedRecovery {
            owner: self,
            review: self.recovery_review(request, contract)?,
            grant,
            created: Instant::now(),
        })
    }
}
impl<'a> PreparedRecovery<'a> {
    pub fn inspection(&self) -> p::Inspection {
        p::Inspection {
            plan: self.review.token.clone(),
            assessment: self.review.facts.assessment.clone(),
            decisions: self.review.facts.decisions.clone(),
            blockers: Some(vec![]),
            resume: None,
            outcome: Some(crate::reducer::recovering(
                &self.review.observation.plan.wire().links,
                &self.review.observation.plan.wire().goal.target,
                &self.review.request.transaction,
                "changed activation inputs require a fresh recovery plan and new authorization",
            )),
        }
    }
    pub fn confirm(self, plan: &str, authorization: &str) -> Result<ConsentedRecovery<'a>> {
        require(
            self.created.elapsed() <= Duration::from_secs(300)
                && plan == self.review.token
                && authorization == self.grant.secret,
        )?;
        self.owner.store.reader.health()?;
        Ok(ConsentedRecovery {
            owner: self.owner,
            review: self.review,
            grant: self.grant,
        })
    }
}
impl ConsentedRecovery<'_> {
    pub fn execute(self) -> Result<p::Outcome> {
        let owner = self.owner;
        owner.outer.admit(&owner.session, &self.review.token, "")?;
        let lease = owner.store.lock()?;
        let review = owner.recovery_review(self.review.request.clone(), &self.review.contract)?;
        require(
            review.token == self.review.token
                && review.observation.journal_snapshot == self.review.observation.journal_snapshot,
        )?;
        owner.verify_prepared_sources(review.observation.plan.wire())?;
        let proposed = format!(
            "recovery-v1-{}",
            &digest(format!("{}{}", owner.session, self.grant.secret).as_bytes())[..32]
        );
        let tx = if review.contract == recovery::ACTIVATION_V2 {
            lease.resolve_lifecycle(
                "recovery-v2-proposed",
                &review.request,
                &review.base,
                &review.token,
                &digest(self.grant.secret.as_bytes()),
            )?
        } else {
            lease.resolve_recovery(&proposed, &review.request, &review.token)?
        };
        let mut journal = review.observation.plan.journal_authorized(
            tx.clone(),
            &self.grant.secret,
            None,
            &review.token,
        )?;
        let namespace = if review.contract == recovery::ACTIVATION_V2 {
            Namespace::LiveRecovery
        } else {
            Namespace::LegacyRecovery
        };
        let inventory = owner.store.inventory(namespace)?;
        if let Some(prior) = inventory
            .records()
            .get(&tx)
            .map(|p| p.exact())
            .transpose()?
            .flatten()
        {
            let prior = Receipt::new(owner.store.decode(prior, true, true)?)?;
            require(
                prior.wire().replacement == review.request
                    && prior.wire().successor.authorization_plan == review.token
                    && prior.wire().ledger == review.ledger
                    && prior.wire().native_plans.as_deref() == Some(review.plans.as_slice()),
            )?;
            journal = record::Journal::new(prior.wire().successor.clone())?;
        }
        let generation = if review.contract == recovery::ACTIVATION_V2 {
            let inv = owner.store.inventory(Namespace::Frontiers)?;
            let raw = inv
                .records()
                .get(&review.request.transaction)
                .ok_or(Error::Invalid)?
                .exact()?
                .ok_or(Error::Invalid)?;
            crate::recovery::Frontier::new(owner.store.decode(raw, true, true)?)?
                .wire()
                .generation
        } else {
            0
        };
        let receipt = Receipt::new(r::RecoveryReceiptV1 {
            schema_version: if generation == 0 { 1 } else { 2 },
            contract: review.contract.clone(),
            replacement: review.request.clone(),
            predecessor: review
                .observation
                .journal
                .as_ref()
                .ok_or(Error::Invalid)?
                .wire()
                .clone(),
            successor: journal.wire().clone(),
            ledger: review.ledger.clone(),
            links: review.observation.plan.wire().links.clone(),
            owner: r::RecoveryOwner {
                release: journal.wire().goal.target.clone(),
                artifact: journal.wire().artifact_digest.clone(),
                registry: journal.wire().registry_digest.clone(),
                catalog: journal.wire().catalog_digest.clone(),
            },
            native_plans: Some(review.plans),
            base_plan: if generation == 0 {
                String::new()
            } else {
                review.base
            },
            reservation: review.reservation,
            generation,
        })?;
        let installed = owner.capture_installed_recipes(
            &lease,
            &review.observation.targets,
            &review.observation.work,
        )?;
        lease.publish_review_bundle(
            &journal,
            review.observation.plan.wire(),
            &owner.helper_digest(),
            &[],
            &review.observation.targets,
            &owner.session,
            &installed,
            Some((&review.facts, &receipt)),
        )?;
        crate::fixture::point("before-recovery-receipt")?;
        lease.publish_recovery_receipt(&receipt)?;
        crate::fixture::point("after-recovery-receipt")?;
        let fresh = owner.recovery_review(review.request, &review.contract)?;
        require(fresh.token == review.token)?;
        owner.store.reader.health()?;
        lease.replace_journal(&fresh.observation.journal_snapshot, &journal)?;
        crate::fixture::point("after-recovery-journal-cas")?;
        owner.run(&lease, journal, vec![], review.observation.targets)
    }
}
