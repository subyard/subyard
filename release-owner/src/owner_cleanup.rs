//! Continuation of the original synchronous owner archive cleanup. These values
//! are reconstructed from the checked journal, prepared bundle and recovery token.
use crate::{
    codec, digest,
    native::Request,
    owner::{Observation, Owner, SourceArtifact},
    record::{self, require},
    store::Address,
    wire::{journal_v2 as j, plan_v1 as f, process_v1 as p},
    work::{OwnerBefore, Recipe},
    Error, Result,
};

pub(crate) struct Continuation {
    pub(crate) journal: record::Journal,
    pub(crate) inspection: p::Inspection,
}
pub(crate) struct CleanupRecipe {
    pub(crate) before: OwnerBefore,
    pub(crate) token: String,
    pub(crate) source: String,
    pub(crate) desired: String,
    pub(crate) ordinal: u64,
}
impl CleanupRecipe {
    fn request(&self, owner: &Owner, journal: &j::Record) -> Request {
        Request::CleanupOwner {
            binding: owner.binding(journal, self.ordinal),
            before: self.before.clone(),
            token: self.token.clone(),
        }
    }
    fn fingerprint(&self, owner: &Owner, journal: &j::Record) -> Result<String> {
        self.request(owner, journal).recipe_fingerprint()
    }
}
impl Owner {
    fn original_bundle_present(&self, journal: &record::Journal) -> Result<bool> {
        if self.store.has_bundle(&journal.wire().transaction)? {
            return Ok(true);
        };
        // Frozen historical Go records have no Rust bundle. For an exact
        // authenticated current/source/previous artifact declaring this owner,
        // missing bundle bytes are corruption, not legacy-format admission.
        for proof in [
            self.options.target_artifact.as_ref(),
            self.options.source_artifact.as_ref(),
            self.options.previous_artifact.as_ref(),
        ]
        .into_iter()
        .flatten()
        {
            if proof.release == journal.wire().goal.target
                && proof.artifact_digest == journal.wire().artifact_digest
            {
                let root = self.links.artifact_root(&proof.release)?;
                let entries = crate::assets::verify(&root, &proof.artifact_digest)?;
                require(!entries.contains_key("config/release-owner.json"))?;
            }
        }
        Ok(false)
    }
    pub(crate) fn pending_cleanup_recipe(
        &self,
        journal: &record::Journal,
    ) -> Result<Option<CleanupRecipe>> {
        let Some(recipe) = self.cleanup_recipe(journal)? else {
            return Ok(None);
        };
        let pending = self.store.required_cleanup_pending(
            journal.wire(),
            &recipe.fingerprint(self, journal.wire())?,
            recipe.ordinal,
        )?;
        // The checked original owner step/token requires this closed slot.
        // Missing completion before first dispatch stays pending; absence
        // never proves native success or quiescence.
        if !pending {
            return Ok(None);
        };
        Ok(Some(recipe))
    }
    pub(crate) fn cleanup_recipe(
        &self,
        journal: &record::Journal,
    ) -> Result<Option<CleanupRecipe>> {
        let bundle = self.store.bundle(journal, &self.registry)?;
        let Some(ordinal) = self.cleanup_owner_ordinal(journal.wire())? else {
            return Ok(None);
        };
        let mut selected = None;
        for (index, work) in bundle.work.iter().enumerate() {
            if let Recipe::TestYardOwnerRegistration(before) = &work.recipe {
                require(selected.is_none())?;
                let step = journal
                    .wire()
                    .steps
                    .as_ref()
                    .and_then(|steps| steps.get(index))
                    .ok_or(Error::Invalid)?;
                if let Some((token, source)) =
                    self.store
                        .owner_cleanup_recipe(journal.wire(), step, before)?
                {
                    selected = Some(CleanupRecipe {
                        before: before.clone(),
                        token,
                        source,
                        desired: step.desired.clone(),
                        ordinal,
                    });
                } else {
                    self.store
                        .completed_cleanup_without_recovery(journal.wire(), ordinal)?;
                }
            }
        }
        Ok(selected)
    }
    fn cleanup_sources(&self, facts: &f::PlanFacts) -> Result<()> {
        let original = self.options.original_sources.as_ref().ok_or(Error::Stale)?;
        let verify = |resource: &str, proof: &SourceArtifact, release: &str| -> Result<()> {
            require(proof.release == release)?;
            let expected = facts
                .observations
                .iter()
                .flatten()
                .find(|o| o.resource == resource)
                .ok_or(Error::Stale)?;
            require(expected.fingerprint == digest(&codec::encode(proof, false)?))?;
            self.links.source_artifact(proof)
        };
        verify(
            "release.source-artifact",
            &original.source,
            &facts.releases.from,
        )?;
        require(
            original.previous.as_ref().map(|p| &p.release) == facts.releases.previous.as_ref(),
        )?;
        if let Some(previous) = &original.previous {
            verify("release.previous-artifact", previous, &previous.release)?;
        }
        Ok(())
    }
    pub(crate) fn prepare_owner_cleanup(
        &self,
        goal: &p::Goal,
    ) -> Result<Option<(Observation, Continuation)>> {
        let Some((journal, snapshot)) = self.store.read_checked_journal()? else {
            return Ok(None);
        };
        if journal.wire().checkpoint != "complete" || !self.original_bundle_present(&journal)? {
            return Ok(None);
        }
        let Some(_recipe) = self.pending_cleanup_recipe(&journal)? else {
            return Ok(None);
        };
        let j = journal.wire();
        require(
            j.goal.target == goal.target
                && goal.target == self.options.releases.target
                && j.goal.direction == goal.direction
                && j.artifact_digest == self.options.artifact_digest
                && j.registry_digest == self.registry_digest
                && j.catalog_digest == self.catalog.digest()
                && self.options.replacement.is_none(),
        )?;
        let bundle = self.store.bundle(&journal, &self.registry)?;
        require(bundle.session == self.session && bundle.helper == self.helper_digest())?;
        self.cleanup_sources(&bundle.facts)?;
        let links = self.links.observe()?;
        require(crate::links::allowed("complete", &links, &j.releases))?;
        let mut assessment = bundle.facts.assessment.clone();
        assessment.changed = true;
        let inspection = p::Inspection {
            plan: j.resume_plan.clone(),
            assessment,
            decisions: bundle.facts.decisions.clone(),
            blockers: None,
            resume: Some(j.transaction.clone()),
            outcome: Some(crate::reducer::recovering(
                &links,
                &goal.target,
                &j.transaction,
                "the authorized release transition has pending owner archive cleanup",
            )),
        };
        require(self.store.read(&Address::journal())? == snapshot)?;
        let observation = Observation {
            plan: crate::plan::Plan::new(bundle.facts)?,
            work: bundle.work,
            journal: Some(record::Journal::new(j.clone())?),
            journal_snapshot: snapshot,
            targets: bundle.targets,
        };
        Ok(Some((
            observation,
            Continuation {
                journal,
                inspection,
            },
        )))
    }
    pub(crate) fn resume_owner_cleanup(&self) -> Result<Option<p::Outcome>> {
        let Some((selected, _)) = self.store.read_checked_journal()? else {
            return Ok(None);
        };
        let goal = p::Goal {
            target: selected.wire().goal.target.clone(),
            direction: selected.wire().goal.direction.clone(),
        };
        let Some((observation, continuation)) = self.prepare_owner_cleanup(&goal)? else {
            return Ok(None);
        };
        let j = continuation.journal.wire();
        self.outer
            .admit(&self.session, &j.resume_plan, &j.transaction)?;
        let lease = self.store.lock()?;
        require(self.store.read(&Address::journal())? == observation.journal_snapshot)?;
        self.verify_cleanup_current_sources()?;
        self.cleanup_sources(observation.plan.wire())?;
        let recipe = self
            .cleanup_recipe(&continuation.journal)?
            .ok_or(Error::Invalid)?;
        // This continuation can publish query/closed retry evidence before the
        // fresh terminal evaluation; it does not dispatch archive deletion here.
        self.continued_native_request(
            &lease,
            j,
            self.binding(j, recipe.ordinal),
            recipe.request(self, j),
        )?;
        self.ready(&lease, &continuation.journal).map(Some)
    }
    pub(crate) fn terminal_native_ready(
        &self,
        lease: &crate::lease::UpdateLease<'_>,
        journal: &record::Journal,
    ) -> Result<()> {
        if let Some(recipe) = self.pending_cleanup_recipe(journal)? {
            lease.cleanup_native_quiescent(
                journal.wire(),
                &recipe.fingerprint(self, journal.wire())?,
            )
        } else {
            self.native_ready(journal.wire())
        }
    }
    pub fn retained_releases(&self) -> Result<Vec<String>> {
        let Some((journal, _)) = self.store.read_checked_journal()? else {
            return Ok(vec![]);
        };
        if journal.wire().checkpoint != "complete" || !self.original_bundle_present(&journal)? {
            return Ok(vec![]);
        }
        let Some(_recipe) = self.pending_cleanup_recipe(&journal)? else {
            return Ok(vec![]);
        };
        let pair = &journal.wire().releases;
        let mut releases = vec![pair.from.clone(), pair.target.clone()];
        releases.extend(pair.previous.iter().cloned());
        releases.sort();
        releases.dedup();
        Ok(releases)
    }
    fn verify_cleanup_current_sources(&self) -> Result<()> {
        self.verify_all_artifacts(true)
    }
}
