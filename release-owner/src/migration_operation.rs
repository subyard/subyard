//! Closed defective-owner replacement. Planning contains no invented grant or
//! executable successor; the exact receipt is completed only after consent.
use crate::{
    codec, digest,
    migration_owner::{self, Proposal},
    owner::{Grant, Observation, Owner},
    plan::Plan,
    record::{self, require},
    store::Address,
    wire::{migration_owner_v1 as m, plan_v1 as f, process_v1 as p},
    Error, Result,
};
use std::time::{Duration, Instant};
struct Review {
    observation: Observation,
    proposal: Proposal,
    request: m::MigrationOwnerReplacementRequest,
}
pub struct PreparedMigrationOwner<'a> {
    owner: &'a Owner,
    review: Review,
    durable: Option<record::Journal>,
    grant: Grant,
    created: Instant,
}
pub struct ConsentedMigrationOwner<'a> {
    owner: &'a Owner,
    review: Review,
    grant: Grant,
}
impl Owner {
    pub(crate) fn migration_assets(
        &self,
        b: &crate::wire::journal_v2::Record,
    ) -> Result<(String, Vec<u8>, Option<m::MigrationOwnerPreviousSource>)> {
        let sources = self
            .options
            .migration_sources
            .as_ref()
            .ok_or(Error::Invalid)?;
        let old = self
            .options
            .source_artifact
            .as_ref()
            .ok_or(Error::Invalid)?;
        let candidate = self
            .options
            .target_artifact
            .as_ref()
            .ok_or(Error::Invalid)?;
        require(
            old.release == b.goal.target
                && old.version == "0.21.0"
                && old.artifact_digest == migration_owner::AFFECTED_MANIFEST
                && sources.source.release == b.releases.from
                && sources.previous.as_ref().map(|p| &p.release) == b.releases.previous.as_ref(),
        )?;
        let source_fd = self.links.artifact_root(&sources.source.release)?;
        self.links.source_artifact(&sources.source)?;
        let raw_source = crate::assets::registry(&source_fd)?;
        let input = |proof: &crate::owner::SourceArtifact| -> Result<_> {
            self.links.source_artifact(proof)?;
            let fd = self.links.artifact_root(&proof.release)?;
            crate::assets::inputs(
                &fd,
                &format!("{}/releases/{}", self.options.runtime_root, proof.release),
                &proof.artifact_digest,
            )
        };
        let old_inputs = input(old)?;
        let source_inputs = input(&sources.source)?;
        let candidate_inputs = crate::assets::inputs(
            &crate::store::io(rustix::io::dup(&self.repository))?,
            &self.options.repository_root,
            &candidate.artifact_digest,
        )?;
        let mut previous_source = None;
        let previous_inputs = if let Some(previous) = &sources.previous {
            let inputs = input(previous)?;
            let fd = self.links.artifact_root(&previous.release)?;
            require(crate::assets::registry(&fd)? == raw_source)?;
            previous_source = Some(m::MigrationOwnerPreviousSource {
                release: previous.release.clone(),
                artifact: previous.artifact_digest.clone(),
                registry: digest(&raw_source),
                inputs: digest(&codec::encode_inventory(&inputs)?),
            });
            Some(inputs)
        } else {
            None
        };
        #[derive(serde::Serialize)]
        struct Assets<'a> {
            #[serde(rename = "Old")]
            old: &'a std::collections::BTreeMap<String, crate::assets::Input>,
            #[serde(rename = "Candidate")]
            candidate: &'a std::collections::BTreeMap<String, crate::assets::Input>,
            #[serde(rename = "Source")]
            source: &'a std::collections::BTreeMap<String, crate::assets::Input>,
            #[serde(rename = "Previous")]
            previous: &'a Option<std::collections::BTreeMap<String, crate::assets::Input>>,
        }
        let assets = digest(&codec::encode_inventory(&Assets {
            old: &old_inputs,
            candidate: &candidate_inputs,
            source: &source_inputs,
            previous: &previous_inputs,
        })?);
        Ok((assets, raw_source, previous_source))
    }
    fn migration_review(&self, request: m::MigrationOwnerReplacementRequest) -> Result<Review> {
        require(supported_platform())?;
        migration_owner::selection(&request)?;
        let (before, snapshot) = self.store.read_checked_journal()?.ok_or(Error::Invalid)?;
        let b = before.wire();
        require(
            b.transaction == request.transaction
                && snapshot.fingerprint() == request.fingerprint
                && snapshot.bytes() == Some(codec::encode(b, true)?.as_slice())
                && b.artifact_digest == migration_owner::AFFECTED_MANIFEST
                && b.registry_digest == migration_owner::AFFECTED_REGISTRY
                && self.registry_digest == b.registry_digest
                && self.options.releases.from == b.goal.target
                && self.options.releases.target != b.goal.target
                && self.options.source_ingress.is_none()
                && self.options.replacement.is_none()
                && self.options.rollback_target.is_none()
                && self.options.migration_checkpoint
                && semver::Version::parse(&self.options.candidate_version)
                    .is_ok_and(|v| v > semver::Version::new(0, 21, 0)),
        )?;
        self.native_quiescent(Some(b))?;
        require(!self.store.native_cleanup_pending()?)?;
        crate::fixture::point("migration-owner-before-assets")?;
        let (assets, raw_source, previous_source) = self.migration_assets(b)?;
        crate::fixture::point("migration-owner-assets-verified")?;
        let projection = self.store.read(&Address::ledger())?;
        let checkpoint = self.store.read(&Address::checkpoint())?;
        let links = self.links.observe()?;
        require(
            links.active == self.options.releases.from
                && links.previous == self.options.releases.previous,
        )?;
        let proposal = Proposal::new(
            m::MigrationOwnerReceiptV1 {
                schema_version: 1,
                contract: crate::recovery::MIGRATION_OWNER.into(),
                registry: self.options.registry_raw.clone(),
                source_registry: Some(raw_source),
                projection_payload: projection.bytes().map(<[u8]>::to_vec),
                checkpoint_payload: checkpoint.bytes().map(<[u8]>::to_vec),
                previous_source,
                replacement: request.clone(),
                predecessor: b.clone(),
                projection: projection.fingerprint(),
                authority: checkpoint.fingerprint(),
                assets,
                links: links.clone(),
                ..Default::default()
            },
            &self.store,
        )?;
        crate::fixture::point("migration-owner-history-verified")?;
        let mut observation = self.observe_inner(
            p::Goal {
                target: self.options.releases.target.clone(),
                direction: "activate-target".into(),
            },
            false,
            true,
        )?;
        crate::fixture::point("migration-owner-observation-captured")?;
        for work in &observation.work {
            crate::fixture::point(&format!("migration-owner-work/{}", work.intent.migration))?;
        }
        require(observation.work.is_empty())?;
        crate::fixture::point("migration-owner-work-empty")?;
        let mut facts = observation.plan.wire().clone();
        for (resource, fingerprint) in [
            ("predecessor", request.fingerprint.clone()),
            ("authority", proposal.wire.authority.clone()),
            ("projection", proposal.wire.projection.clone()),
            ("protected-inputs", proposal.wire.protected_inputs.clone()),
            ("assets", proposal.wire.assets.clone()),
        ] {
            facts
                .observations
                .get_or_insert_default()
                .push(f::ResourceObservation {
                    resource: format!("migration-owner.{resource}"),
                    class: crate::recovery::MIGRATION_OWNER.into(),
                    fingerprint,
                });
        }
        let mut consequences = facts.assessment.consequences.clone().unwrap_or_default();
        if consequences
            .first()
            .is_some_and(|s| s == "apply the exact typed migration and release activation plan")
        {
            consequences.remove(0);
        }
        consequences.push("preserve the proved defective owner's unfinished activation as superseded evidence and activate the standalone candidate".into());
        facts.assessment = crate::plan::assess(true, false, &consequences)?;
        observation.plan = Plan::new(facts)?;
        self.store.reader.health()?;
        Ok(Review {
            observation,
            proposal,
            request,
        })
    }
    pub(crate) fn resume_migration_receipt(&self) -> Result<Option<p::Outcome>> {
        let Some((current, _)) = self.store.read_checked_journal()? else {
            return Ok(None);
        };
        let receipts = self.store.migration_owner_receipts()?;
        let Some(receipt) = receipts
            .values()
            .find(|r| r.wire().predecessor.transaction == current.wire().transaction)
        else {
            return Ok(None);
        };
        let successor = record::Journal::new(receipt.wire().successor.clone())?;
        let bundle = self.store.bundle(&successor, &self.registry)?;
        require(
            bundle.session == self.session
                && bundle.helper == self.helper_digest()
                && bundle.work.is_empty()
                && bundle.facts.artifact_digest == self.options.artifact_digest,
        )?;
        self.outer.admit(
            &self.session,
            &successor.wire().resume_plan,
            &successor.wire().transaction,
        )?;
        let lease = self.store.lock()?;
        let review = self.migration_review(receipt.wire().replacement.clone())?;
        require(
            review.observation.plan.token() == successor.wire().authorization_plan
                && review.observation.plan.wire() == &bundle.facts
                && review.observation.targets == bundle.targets,
        )?;
        self.verify_prepared_sources(&bundle.facts)?;
        let proved = review
            .proposal
            .finish(successor.wire().clone(), &self.store)?;
        require(proved.wire() == receipt.wire())?;
        proved.source_evidence(&self.store)?;
        lease.replace_journal(&review.observation.journal_snapshot, &successor)?;
        crate::fixture::point("after-migration-owner-journal")?;
        self.run(&lease, successor, bundle.work, bundle.targets)
            .map(Some)
    }
    fn migration_resume_journal(&self, review: &Review) -> Result<Option<record::Journal>> {
        let receipts = self.store.migration_owner_receipts()?;
        let Some(receipt) = receipts
            .values()
            .find(|r| r.wire().replacement == review.request)
        else {
            return Ok(None);
        };
        let successor = record::Journal::new(receipt.wire().successor.clone())?;
        let bundle = self.store.bundle(&successor, &self.registry)?;
        if bundle.facts != *review.observation.plan.wire() {
            crate::fixture::point("migration-owner-resume-facts-changed")?;
            for observation in bundle.facts.observations.iter().flatten() {
                if review
                    .observation
                    .plan
                    .wire()
                    .observations
                    .iter()
                    .flatten()
                    .find(|o| o.resource == observation.resource)
                    != Some(observation)
                {
                    crate::fixture::point(&format!(
                        "migration-owner-resume-observation/{}",
                        observation.resource
                    ))?;
                }
            }
        }
        if bundle.targets != review.observation.targets {
            crate::fixture::point("migration-owner-resume-targets-changed")?;
        }
        crate::fixture::point("migration-owner-resume-before-binding")?;
        require(
            bundle.session == self.session
                && bundle.helper == self.helper_digest()
                && bundle.facts == *review.observation.plan.wire()
                && bundle.targets == review.observation.targets
                && successor.wire().authorization_plan == review.observation.plan.token(),
        )?;
        crate::fixture::point("migration-owner-resume-binding-verified")?;
        let checked = Proposal {
            wire: review.proposal.wire.clone(),
        }
        .finish(successor.wire().clone(), &self.store)?;
        require(checked.wire() == receipt.wire())?;
        Ok(Some(successor))
    }
    pub fn prepare_migration_owner(
        &self,
        request: m::MigrationOwnerReplacementRequest,
        grant: Grant,
    ) -> Result<PreparedMigrationOwner<'_>> {
        let review = self.migration_review(request)?;
        let durable = self.migration_resume_journal(&review)?;
        Ok(PreparedMigrationOwner {
            owner: self,
            review,
            durable,
            grant,
            created: Instant::now(),
        })
    }
}
#[derive(serde::Serialize)]
pub struct ResumeAdmission {
    plan: String,
    transaction: String,
}
impl ResumeAdmission {
    pub(crate) fn checked_journal(journal: &record::Journal) -> Self {
        Self {
            plan: journal.wire().resume_plan.clone(),
            transaction: journal.wire().transaction.clone(),
        }
    }
}
impl<'a> PreparedMigrationOwner<'a> {
    /// Correlation for the checked original durable receipt; not a new grant.
    pub fn resume_admission(&self) -> Option<ResumeAdmission> {
        self.durable.as_ref().map(|j| ResumeAdmission {
            plan: j.wire().resume_plan.clone(),
            transaction: j.wire().transaction.clone(),
        })
    }
    pub fn inspection(&self) -> p::Inspection {
        let facts = self.review.observation.plan.wire();
        if let Some(journal) = &self.durable {
            return p::Inspection {
                plan: journal.wire().resume_plan.clone(),
                assessment: facts.assessment.clone(),
                decisions: facts.decisions.clone(),
                blockers: facts.blockers.clone(),
                resume: Some(journal.wire().transaction.clone()),
                outcome: Some(crate::reducer::recovering(
                    &facts.links,
                    &facts.goal.target,
                    &journal.wire().transaction,
                    "the authorized release transition can resume from observed facts",
                )),
            };
        }
        let mut outcome=crate::reducer::recovering(&facts.links,&self.owner.options.releases.target,&self.review.request.transaction,"the proved defective owner requires a fresh standalone replacement plan and authorization");
        if let Some(blocker) = facts.blockers.iter().flatten().next() {
            outcome = crate::reducer::operator(outcome, &blocker.code, &blocker.message);
            outcome.retry = blocker.retry.clone();
        }
        p::Inspection {
            plan: self.review.observation.plan.token().into(),
            assessment: facts.assessment.clone(),
            decisions: facts.decisions.clone(),
            blockers: facts.blockers.clone(),
            resume: None,
            outcome: Some(outcome),
        }
    }
    pub fn confirm(self, plan: &str, authorization: &str) -> Result<ConsentedMigrationOwner<'a>> {
        require(
            self.durable.is_none()
                && self.created.elapsed() <= Duration::from_secs(300)
                && plan == self.review.observation.plan.token()
                && authorization == self.grant.secret
                && self
                    .review
                    .observation
                    .plan
                    .wire()
                    .blockers
                    .iter()
                    .flatten()
                    .next()
                    .is_none(),
        )?;
        self.owner.store.reader.health()?;
        Ok(ConsentedMigrationOwner {
            owner: self.owner,
            review: self.review,
            grant: self.grant,
        })
    }
}
impl ConsentedMigrationOwner<'_> {
    pub fn execute(self) -> Result<p::Outcome> {
        let owner = self.owner;
        let token = self.review.observation.plan.token();
        owner.outer.admit(&owner.session, token, "")?;
        let lease = owner.store.lock()?;
        let review = owner.migration_review(self.review.request.clone())?;
        require(
            review.observation.plan.token() == token
                && review.observation.journal_snapshot == self.review.observation.journal_snapshot,
        )?;
        crate::fixture::point("migration-owner-plan-revalidated")?;
        owner.verify_prepared_sources(review.observation.plan.wire())?;
        crate::fixture::point("migration-owner-sources-revalidated")?;
        let prior = owner.store.migration_owner_receipts()?;
        let journal = if let Some(receipt) = prior.values().next() {
            require(receipt.wire().successor.authorization_plan == token)?;
            record::Journal::new(receipt.wire().successor.clone())?
        } else {
            review.observation.plan.journal(
                format!(
                    "migration-owner-v1-{}",
                    &digest(format!("{}{}", owner.session, self.grant.secret).as_bytes())[..32]
                ),
                &self.grant.secret,
                None,
            )?
        };
        crate::fixture::point("migration-owner-successor-formed")?;
        let receipt = review
            .proposal
            .finish(journal.wire().clone(), &owner.store)?;
        crate::fixture::point("migration-owner-receipt-checked")?;
        if let Some(prior) = prior.values().next() {
            require(prior.wire() == receipt.wire())?;
        }
        let installed = owner.capture_installed_recipes(
            &lease,
            &review.observation.targets,
            &review.observation.work,
        )?;
        lease.publish_bundle(
            &journal,
            review.observation.plan.wire(),
            &owner.helper_digest(),
            &[],
            &review.observation.targets,
            &owner.session,
            &installed,
        )?;
        crate::fixture::point("before-migration-owner-receipt")?;
        lease.create_owner_immutable(
            &crate::inventory::Namespace::MigrationOwner.address(&journal.wire().transaction)?,
            &codec::encode(receipt.wire(), true)?,
        )?;
        crate::fixture::point("after-migration-owner-receipt")?;
        let fresh = owner.migration_review(review.request)?;
        if fresh.observation.plan.token() != token {
            for observation in fresh.observation.plan.wire().observations.iter().flatten() {
                if review
                    .observation
                    .plan
                    .wire()
                    .observations
                    .iter()
                    .flatten()
                    .find(|o| o.resource == observation.resource)
                    != Some(observation)
                {
                    crate::fixture::point(&format!(
                        "migration-owner-plan-changed/{}",
                        observation.resource
                    ))?;
                }
            }
            if fresh.observation.plan.wire().blockers != review.observation.plan.wire().blockers {
                crate::fixture::point("migration-owner-blockers-changed")?;
                for blocker in fresh.observation.plan.wire().blockers.iter().flatten() {
                    crate::fixture::point(&format!(
                        "migration-owner-blocker/{}/{}",
                        blocker.resource, blocker.code
                    ))?;
                }
            }
            return Err(Error::Stale);
        }
        crate::fixture::point("migration-owner-final-plan-revalidated")?;
        receipt.source_evidence(&owner.store)?;
        crate::fixture::point("migration-owner-final-source-revalidated")?;
        lease.replace_journal(&fresh.observation.journal_snapshot, &journal)?;
        crate::fixture::point("after-migration-owner-journal")?;
        owner.run(&lease, journal, vec![], review.observation.targets)
    }
}

fn supported_platform() -> bool {
    cfg!(all(target_os = "linux", target_arch = "x86_64"))
}
#[cfg(test)]
#[test]
fn frozen_migration_owner_contract_platform() {
    assert_eq!(
        supported_platform(),
        cfg!(all(target_os = "linux", target_arch = "x86_64"))
    );
}
