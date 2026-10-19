//! Journal and native-custody reduction. Effects require the consumed consent,
//! held update lease, original prepared bundle, and a healthy retained peer.
use crate::{
    codec, digest,
    lease::UpdateLease,
    ledger_execution,
    native::{self, Request as N},
    owner::{Consented, Owner},
    record::{self, require},
    store::{Address, EvidenceCheckpoint, Snapshot},
    wire::{journal_v2 as j, process_v1 as p},
    work::{Recipe, Work},
    Error, Result,
};
use serde::Serialize;
#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct Intent<'a> {
    version: i64,
    binding: &'a native::Binding,
    owner: &'a str,
    resource: &'a str,
    expected: &'a str,
    desired: &'a str,
    source: &'a str,
    recipe: &'a str,
}
pub(crate) struct Effect<'a> {
    pub(crate) owner: &'a str,
    pub(crate) resource: &'a str,
    pub(crate) expected: &'a str,
    pub(crate) desired: &'a str,
    pub(crate) source: &'a str,
}
impl Consented<'_> {
    pub fn execute(self) -> Result<p::Outcome> {
        let owner = self.owner;
        owner.store.reader.health()?;
        if self
            .observation
            .journal
            .as_ref()
            .is_some_and(|j| j.wire().checkpoint != "complete")
            && self.observation.plan.wire().replacement.is_none()
        {
            return owner.resume();
        }
        // A fixed-point read with no changed assessment grants no mutation
        // authority and performs no protected cleanup/publication.
        if !self.observation.plan.wire().assessment.changed {
            let fresh = owner.observe(self.observation.plan.wire().goal.clone())?;
            require(
                fresh.plan.token() == self.observation.plan.token()
                    && fresh.journal_snapshot == self.observation.journal_snapshot,
            )?;
            owner.verify_prepared_sources(fresh.plan.wire())?;
            owner.verify_targets(&fresh.targets)?;
            let outcome = owner
                .projected_inspection(&fresh)
                .outcome
                .ok_or(Error::Invalid)?;
            require(outcome.status == "ready" && outcome.reached_goal)?;
            owner.store.reader.health()?;
            return Ok(outcome);
        }
        owner
            .outer
            .admit(&owner.session, self.observation.plan.token(), "")?;
        let lease = owner.store.lock()?;
        let fresh = if let Some(selection) = &self.observation.plan.wire().replacement {
            owner.replacement_observation(self.observation.plan.wire().goal.clone(), selection)?
        } else {
            owner.observe(self.observation.plan.wire().goal.clone())?
        };
        if fresh.plan.token() != self.observation.plan.token()
            || fresh.journal_snapshot != self.observation.journal_snapshot
        {
            return Err(Error::Stale);
        }
        owner.store.reader.health()?;
        if fresh
            .journal
            .as_ref()
            .is_some_and(|j| j.wire().checkpoint != "complete")
            && fresh.plan.wire().replacement.is_none()
        {
            return Err(Error::Stale);
        }
        owner.native_quiescent(None)?;
        owner.verify_prepared_sources(fresh.plan.wire())?;
        if !fresh.plan.wire().assessment.changed {
            owner.verify_prepared_sources(fresh.plan.wire())?;
            owner.verify_targets(&fresh.targets)?;
            let outcome = owner
                .projected_inspection(&fresh)
                .outcome
                .ok_or(Error::Invalid)?;
            require(outcome.status == "ready" && outcome.reached_goal)?;
            owner.store.reader.health()?;
            return Ok(outcome);
        }
        let mut nonce = [0u8; 32];
        {
            use std::io::Read;
            std::fs::File::open("/dev/urandom")
                .and_then(|mut f| f.read_exact(&mut nonce))
                .map_err(|_| Error::Io)?;
        }
        let mut tx = format!("tx-{}", &digest(&nonce)[..32]);
        if let Some(selection) = fresh
            .plan
            .wire()
            .replacement
            .as_ref()
            .filter(|r| r.reason == "post-activation-scope-v0.11.1")
        {
            tx = lease.resolve_superseded(&tx, selection, fresh.plan.token())?;
        }
        let journal = self.observation.plan.journal(
            tx,
            &self.grant.secret,
            owner.options.source_ingress.clone(),
        )?;
        let installed = owner.capture_installed_recipes(
            &lease,
            &self.observation.targets,
            &self.observation.work,
        )?;
        lease.publish_bundle(
            &journal,
            self.observation.plan.wire(),
            &owner.helper_digest(),
            &self.observation.work,
            &self.observation.targets,
            &owner.session,
            &installed,
        )?;
        if let Some(selection) = fresh
            .plan
            .wire()
            .replacement
            .as_ref()
            .filter(|r| r.reason == "post-activation-scope-v0.11.1")
        {
            let archive = record::Archive::new(j::Archive {
                schema_version: 1,
                authorization_plan: journal.wire().authorization_plan.clone(),
                replacement: serde_json::from_slice(&codec::encode(selection, false)?)
                    .map_err(|_| Error::Codec)?,
                journal: fresh.journal.as_ref().ok_or(Error::Invalid)?.wire().clone(),
            })?;
            lease.create_immutable(
                &Address::superseded(&journal.wire().transaction)?,
                &codec::encode(archive.wire(), true)?,
            )?;
            crate::fixture::point("after-superseded-journal")?;
        }
        owner.store.reader.health()?;
        crate::fixture::point("after-prepared-bundle")?;
        lease.replace_journal(&fresh.journal_snapshot, &journal)?;
        crate::fixture::point("after-authorized-journal")?;
        owner.run(
            &lease,
            journal,
            self.observation.work,
            self.observation.targets,
        )
    }
}
impl Owner {
    pub(crate) fn project_owner_activation(&self, work: &[Work]) -> Result<()> {
        let mut selected = None;
        for work in work {
            if let Recipe::TestYardOwnerRegistration(before) = &work.recipe {
                require(selected.is_none())?;
                selected = Some(before);
            }
        }
        if let Some(before) = selected {
            self.call(N::ProjectActivation {
                session: self.session.clone(),
                before: before.clone(),
            })?;
        }
        Ok(())
    }
    fn activation_capture(&self, owner: &str) -> Result<native::Reply> {
        let result = self.call(N::CaptureActivation {
            session: self.session.clone(),
            owner: owner.into(),
            fresh: true,
        });
        if result.is_err() && self.first_cause.borrow().is_none() {
            self.first_cause.replace(Some(native::Diagnostic {
                kind: "component-failure".into(),
                message: format!("activation reconciler {owner:?} cannot be observed safely"),
                retry: "run yard update --check".into(),
            }));
        }
        result
    }
    pub(crate) fn helper_digest(&self) -> String {
        match &self.store.reader {
            crate::legacy::Reader::Frozen(p) => p.image_digest(),
            _ => String::new(),
        }
    }
    fn persist(
        &self,
        lease: &UpdateLease<'_>,
        wire: &j::Record,
        snapshot: &mut Snapshot,
    ) -> Result<record::Journal> {
        self.store.reader.health()?;
        let checked = record::Journal::new(wire.clone())?;
        let raw = codec::encode(checked.wire(), true)?;
        lease.compare_and_swap(&Address::journal(), snapshot, &raw)?;
        *snapshot = Snapshot::captured(Some(raw));
        Ok(checked)
    }
    pub(crate) fn binding(&self, journal: &j::Record, ordinal: u64) -> native::Binding {
        native::Binding {
            session: self.session.clone(),
            transaction: journal.transaction.clone(),
            plan: journal.authorization_plan.clone(),
            journal: journal.intent_digest.clone(),
            artifact: journal.artifact_digest.clone(),
            helper: self.helper_digest(),
            ordinal,
        }
    }
    pub(crate) fn dispatch(
        &self,
        lease: &UpdateLease<'_>,
        journal: &j::Record,
        ordinal: u64,
        effect: Effect<'_>,
        request: impl FnOnce(native::Binding) -> N,
    ) -> Result<()> {
        let Effect {
            owner,
            resource,
            expected,
            desired,
            source,
        } = effect;
        self.store.reader.health()?;
        let mut binding = self.binding(journal, ordinal);
        let mut request = request(binding.clone());
        loop {
            let previous = binding.ordinal;
            (binding, request) = self.continued_native_request(lease, journal, binding, request)?;
            if binding.ordinal == previous {
                break;
            }
            require(binding.ordinal > previous && binding.ordinal <= 4096)?;
        }
        binding.validate()?;
        // Original local cleanup continuation must retain its proof until the
        // retry closes. It may reserve capacity without retiring other roots.
        match self.store.native_cleanup_pending() {
            Ok(false) => lease.cleanup_native_transactions()?,
            // An exact continuation may still be constructing its fence or
            // retry chain. Defer retirement; the existing admitting-binding
            // validator below must independently authorize this dispatch.
            Ok(true) | Err(Error::Unknown) => {}
            Err(error) => return Err(error),
        }
        lease.reserve_native(&binding)?;
        let recipe = request.recipe_fingerprint()?;
        let address = Address::native(&journal.transaction, binding.ordinal, "intent")?;
        let raw = codec::encode(
            &Intent {
                version: 1,
                binding: &binding,
                owner,
                resource,
                expected,
                desired,
                source,
                recipe: &recipe,
            },
            true,
        )?;
        let prior = self.store.read(&address)?;
        if prior.bytes().is_some() {
            require(prior.bytes() == Some(raw.as_slice()))?;
            if self
                .store
                .read(&Address::native(
                    &journal.transaction,
                    binding.ordinal,
                    "accepted",
                )?)?
                .bytes()
                .is_some()
            {
                self.require_terminal(&binding, owner, resource)?;
                lease.close_native(&binding)?;
                return Ok(());
            }
            require(
                self.store
                    .read(&Address::native(&journal.transaction, ordinal, "terminal")?)?
                    .bytes()
                    .is_none(),
            )?;
        } else {
            lease.create_immutable(&address, &raw)?;
        }
        crate::fixture::point("after-native-intent")?;
        self.store.native_before_dispatch(&binding)?;
        let update = native::Identity::capture(&lease._lock)?;
        let (owner_identity, owner_fd) = self.outer.borrowed()?;
        self.native(
            &N::ClaimLease {
                binding: binding.clone(),
                update,
                owner: Some(owner_identity),
            },
            vec![crate::store::io(rustix::io::dup(&lease._lock))?, owner_fd],
        )?;
        crate::fixture::point("after-native-custody")?;
        let result = self.call(request);
        // The reply is sent only after the helper has written its terminal fact.
        // EOF/timeouts never manufacture a terminal witness or permit replay.
        if result.is_err() {
            let snapshot = self.store.read(&Address::native(
                &binding.transaction,
                binding.ordinal,
                "terminal",
            )?)?;
            if let Some(raw) = snapshot.bytes() {
                let terminal: native::Terminal = codec::decode_canonical(raw, true)?;
                if terminal.version == 1
                    && terminal.binding == binding
                    && terminal.owner == owner
                    && terminal.resource == resource
                    && terminal.state == "failed"
                    && self.native_quiescent(Some(journal)).is_ok()
                {
                    self.call(N::ReleaseLease {
                        session: self.session.clone(),
                    })?;
                    lease.close_native(&binding)?;
                    return Err(Error::Io);
                }
            }
            return Err(Error::Unknown);
        }
        self.require_terminal(&binding, owner, resource)?;
        crate::fixture::point("after-native-terminal")?;
        self.call(N::ReleaseLease {
            session: self.session.clone(),
        })?;
        lease.close_native(&binding)?;
        self.store.reader.health()
    }
    fn require_terminal(
        &self,
        binding: &native::Binding,
        owner: &str,
        resource: &str,
    ) -> Result<()> {
        let snapshot = self.store.read(&Address::native(
            &binding.transaction,
            binding.ordinal,
            "terminal",
        )?)?;
        let t: native::Terminal =
            codec::decode_canonical(snapshot.bytes().ok_or(Error::Unknown)?, true)?;
        require(
            t.version == 1
                && t.binding == *binding
                && t.owner == owner
                && t.resource == resource
                && t.state == "complete",
        )?;
        // A top-level success cannot hide an accepted nested operation or
        // command without its own exact terminal witness. Check before the
        // next evidence checkpoint or native dispatch, not only at Ready.
        self.native_quiescent(None)
    }
    pub(crate) fn fingerprint(&self, work: &Work, journal: &j::Record) -> Result<String> {
        match &work.recipe {
            Recipe::LocalSSHPortPin(r) | Recipe::TestVMSettingsMigration(r) => {
                let raw = self
                    .call(N::ObserveSettings {
                        session: self.session.clone(),
                        recipe: r.clone(),
                    })?
                    .actual;
                match raw.as_str() {
                    "expected" => Ok(r.expected.clone()),
                    "desired" => Ok(r.desired.clone()),
                    _ => Err(Error::Stale),
                }
            }
            Recipe::TestYardOwnerRegistration(before) => {
                let step = journal
                    .steps
                    .iter()
                    .flatten()
                    .find(|step| step.id == work.intent.id)
                    .ok_or(Error::Invalid)?;
                let token = self.store.owner_progress_token(journal, step, before)?;
                let actual = self
                    .call(N::ObserveOwner {
                        session: self.session.clone(),
                        before: before.clone(),
                        token,
                    })?
                    .actual;
                match actual.as_str() {
                    "expected" | "in-progress" => Ok(work.intent.expected.clone()),
                    "desired" => Ok(work.intent.desired.clone()),
                    _ => Err(Error::Invalid),
                }
            }
            Recipe::LedgerAdvance { .. } => Ok(ledger_execution::semantic_snapshot(
                &self
                    .store
                    .migration_ledger(&self.registry, &self.registry_digest)?,
            )?
            .fingerprint()),
            Recipe::LegacyV1Import { .. } | Recipe::SourceImport | Recipe::SourceEntrypoints => {
                Ok(self
                    .call(N::ObserveIngress {
                        session: self.session.clone(),
                        step: self.ingress_step(journal, work)?,
                    })?
                    .actual)
            }
        }
    }
    fn recovery(
        &self,
        lease: &UpdateLease<'_>,
        journal: &j::Record,
        index: usize,
        work: &Work,
    ) -> Result<(String, String)> {
        let step = &journal.steps.as_ref().ok_or(Error::Invalid)?[index];
        let address = Address::at(
            vec![
                "release-transition".into(),
                "v2".into(),
                "transactions".into(),
                journal.transaction.clone(),
                "recovery".into(),
            ],
            format!("{}.json", step.id),
            true,
        );
        match &work.recipe {
            Recipe::LocalSSHPortPin(recipe) | Recipe::TestVMSettingsMigration(recipe) => {
                #[derive(Serialize)]
                #[serde(rename_all = "camelCase")]
                struct Recovery<'a> {
                    schema_version: i64,
                    #[serde(skip_serializing_if = "str::is_empty")]
                    kind: &'a str,
                    transaction: &'a str,
                    step: &'a str,
                    migration: &'a str,
                    yard: &'a str,
                    expected_fingerprint: &'a str,
                    before: &'a [crate::work::BeforeValue],
                }
                let local = matches!(work.recipe, Recipe::LocalSSHPortPin(_));
                let raw = codec::encode(
                    &Recovery {
                        schema_version: if local { 2 } else { 1 },
                        kind: if local {
                            "local-ssh-port-settings-v2-to-v3"
                        } else {
                            ""
                        },
                        transaction: &journal.transaction,
                        step: &step.id,
                        migration: &step.migration,
                        yard: &step.resource,
                        expected_fingerprint: &step.expected,
                        before: &recipe.before,
                    },
                    true,
                )?;
                require(raw.len() <= 4096)?;
                lease.create_immutable(&address, &raw)?;
                Ok((digest(&raw), String::new()))
            }
            Recipe::TestYardOwnerRegistration(before) => {
                #[derive(Serialize)]
                #[serde(rename_all = "camelCase")]
                struct Recovery<'a> {
                    schema_version: i64,
                    transaction: &'a str,
                    step: &'a str,
                    migration: &'a str,
                    resource: &'a str,
                    expected_fingerprint: &'a str,
                    token: &'a str,
                    before: &'a crate::work::OwnerBefore,
                }
                let token = digest(
                    format!("{}:{}:{}", self.session, journal.transaction, step.id).as_bytes(),
                )[..32]
                    .to_string();
                let semantic_before = before.semantic();
                let raw = codec::encode(
                    &Recovery {
                        schema_version: 1,
                        transaction: &journal.transaction,
                        step: &step.id,
                        migration: &step.migration,
                        resource: &step.resource,
                        expected_fingerprint: &step.expected,
                        token: &token,
                        before: &semantic_before,
                    },
                    true,
                )?;
                require(raw.len() <= 4096)?;
                lease.create_immutable(&address, &raw)?;
                Ok((digest(&raw), token))
            }
            Recipe::LegacyV1Import { static_pin } => Ok((static_pin.clone(), String::new())),
            _ => Ok((String::new(), String::new())),
        }
    }
    fn evidence(
        &self,
        lease: &UpdateLease<'_>,
        journal: &mut j::Record,
        index: usize,
        checkpoint: &str,
        recovery: &str,
        snapshot: &mut Snapshot,
    ) -> Result<()> {
        let step = &journal.steps.as_ref().ok_or(Error::Invalid)?[index];
        let e = j::Evidence {
            schema_version: 2,
            transaction: journal.transaction.clone(),
            releases: journal.releases.clone(),
            step: step.id.clone(),
            expected: step.expected.clone(),
            desired: step.desired.clone(),
            observed: if checkpoint == "captured" {
                step.expected.clone()
            } else {
                step.desired.clone()
            },
            recovery: recovery.into(),
            checkpoint: checkpoint.into(),
        };
        record::Evidence::new(e.clone())?;
        let kind = match checkpoint {
            "captured" => EvidenceCheckpoint::Captured,
            "applied" => EvidenceCheckpoint::Applied,
            "verified" => EvidenceCheckpoint::Verified,
            _ => return Err(Error::Invalid),
        };
        lease.create_immutable(
            &Address::evidence(&journal.transaction, &step.id, kind)?,
            &codec::encode(&e, true)?,
        )?;
        let step = &mut journal.steps.as_mut().ok_or(Error::Invalid)?[index];
        step.checkpoint = if checkpoint == "captured" {
            "evidence"
        } else {
            checkpoint
        }
        .into();
        step.evidence = Some(e);
        if journal.checkpoint == "authorized" {
            journal.checkpoint = "migrating".into()
        };
        self.persist(lease, journal, snapshot)?;
        Ok(())
    }
    pub(crate) fn run(
        &self,
        lease: &UpdateLease<'_>,
        checked: record::Journal,
        work: Vec<Work>,
        targets: Vec<native::Target>,
    ) -> Result<p::Outcome> {
        let transaction = checked.wire().transaction.clone();
        match self.run_inner(lease, checked, work, targets) {
            Ok(outcome) => Ok(outcome),
            Err(cause) => self.reduce_failure(lease, &transaction, cause),
        }
    }
    // A post-publication failure is reduced while the same update lease is held.
    // Unknown native work always wins over a desired-state observation.
    fn reduce_failure(
        &self,
        lease: &UpdateLease<'_>,
        transaction: &str,
        cause: Error,
    ) -> Result<p::Outcome> {
        let current = self.store.read_checked_journal();
        let links = self.links.observe().ok();
        let base = |links: Option<&crate::wire::recovery_v1::ReleaseLinks>| p::Outcome {
            target: self.options.releases.target.clone(),
            transaction: Some(transaction.into()),
            active: links.map(|l| l.active.clone()).unwrap_or_default(),
            previous: links.and_then(|l| l.previous.clone()),
            ..Default::default()
        };
        let mut outcome = if links.is_none() {
            crate::reducer::operator(
                base(None),
                "recovery-ambiguous",
                "release facts cannot be observed after an interrupted transition",
            )
        } else if current.is_err() {
            crate::reducer::operator(
                base(links.as_ref()),
                "recovery-ambiguous",
                "durable transition intent cannot be observed after a possible mutation",
            )
        } else if let Some((journal, _)) = current.as_ref().ok().and_then(|v| v.as_ref()) {
            if journal.wire().transaction != transaction
                || self.store.reader.health().is_err()
                || self.native_quiescent(None).is_err()
            {
                crate::reducer::operator(
                    base(links.as_ref()),
                    "recovery-ambiguous",
                    "release outcome cannot be proved safely",
                )
            } else if journal.wire().checkpoint == "complete" {
                let checked = (|| {
                    let bundle = self.store.bundle(journal, &self.registry)?;
                    self.verify_targets(&bundle.targets)?;
                    self.ready(lease, journal)
                })();
                checked.unwrap_or_else(|_| {
                    crate::reducer::operator(
                        base(links.as_ref()),
                        "recovery-ambiguous",
                        "terminal release facts cannot be verified after an interrupted transition",
                    )
                })
            } else {
                match self.observe_inner(
                    p::Goal {
                        target: journal.wire().goal.target.clone(),
                        direction: journal.wire().goal.direction.clone(),
                    },
                    false,
                    true,
                ) {
                    Err(_) => crate::reducer::operator(
                        base(links.as_ref()),
                        "recovery-ambiguous",
                        "release facts cannot be reduced to a safe recovery checkpoint",
                    ),
                    Ok(observation) => {
                        let facts = observation.plan.wire();
                        let base = base(Some(&facts.links));
                        if let Some(blocker) = facts.blockers.iter().flatten().next() {
                            let mut out =
                                crate::reducer::operator(base, &blocker.code, &blocker.message);
                            out.retry = blocker.retry.clone();
                            out
                        } else if observation
                            .journal
                            .as_ref()
                            .is_none_or(|j| j.wire().transaction != transaction)
                            || !crate::links::allowed(
                                &journal.wire().checkpoint,
                                &facts.links,
                                &journal.wire().releases,
                            )
                        {
                            crate::reducer::operator(
                                base,
                                "recovery-ambiguous",
                                "release facts do not match the protected recovery transaction",
                            )
                        } else if journal.wire().observation_scope != facts.observation_scope {
                            crate::reducer::operator(base,"plan-stale","the authorized transition observation scope differs from this engine")
                        } else if cause == Error::Stale {
                            crate::reducer::operator(base,"plan-stale","the approved component source or target differs from the protected recovery recipe")
                        } else if matches!(
                            cause,
                            Error::Invalid
                                | Error::Protected
                                | Error::Codec
                                | Error::Noncanonical
                                | Error::Unknown
                        ) {
                            crate::reducer::operator(
                                base,
                                "recovery-ambiguous",
                                "protected recovery evidence is invalid or inconsistent",
                            )
                        } else {
                            p::Outcome{status:"recovering".into(),code:"verification-failed".into(),message:"the release transition was interrupted after a durable mutation checkpoint".into(),retry:"run yard update".into(),..base}
                        }
                    }
                }
            }
        } else {
            return Err(cause);
        };
        if let Some(public) = self.first_cause.borrow().as_ref() {
            crate::reducer::failure_diagnostic(&mut outcome, &public.message, &public.retry);
        }
        Ok(outcome)
    }
    fn run_inner(
        &self,
        lease: &UpdateLease<'_>,
        checked: record::Journal,
        work: Vec<Work>,
        targets: Vec<native::Target>,
    ) -> Result<p::Outcome> {
        let mut journal = checked.wire().clone();
        self.call(N::CaptureInventory {
            session: self.session.clone(),
        })?;
        let ingress_binding = self.ingress_binding(&journal, &work);
        let ingress = self
            .call(N::CaptureIngress {
                session: self.session.clone(),
                binding: Some(ingress_binding),
            })?
            .ingress
            .ok_or(Error::Invalid)?;
        let bundle = self.store.bundle(&checked, &self.registry)?;
        self.verify_prepared_sources(&bundle.facts)?;
        let inventory = digest(&codec::encode(&ingress.yards, false)?);
        require(
            bundle
                .facts
                .observations
                .iter()
                .flatten()
                .any(|o| o.resource == "native.yard-set" && o.fingerprint == inventory),
        )?;
        let mut snapshot = self.store.read(&Address::journal())?;
        require(
            snapshot.bytes() == Some(codec::encode(&journal, true)?.as_slice())
                && crate::links::allowed(
                    &journal.checkpoint,
                    &self.links.observe()?,
                    &journal.releases,
                ),
        )?;
        require(
            journal.registry_digest == self.registry_digest
                && journal.catalog_digest == self.catalog.digest()
                && journal.artifact_digest == self.options.artifact_digest
                && work.len() == journal.steps.iter().flatten().count(),
        )?;
        for step in journal.steps.iter().flatten() {
            self.verify_recorded_step(&journal, step)?;
        }
        self.reobserve_prepared_settings(&work)?;
        self.project_owner_activation(&work)?;
        for (index, w) in work.iter().enumerate() {
            if journal.steps.as_ref().ok_or(Error::Invalid)?[index].checkpoint == "verified" {
                if let Recipe::LedgerAdvance { before, after } = &w.recipe {
                    self.validate_ledger_edge(w, before, after)?;
                } else if matches!(w.recipe, Recipe::TestVMSettingsMigration(_)) {
                    self.verify_settings_predecessor(w, index, &work, &journal)?;
                } else {
                    require(self.fingerprint(w, &journal)? == w.intent.desired)?;
                }
                continue;
            }
            let (recovery, token) = self.recovery(lease, &journal, index, w)?;
            if journal.steps.as_ref().ok_or(Error::Invalid)?[index].checkpoint == "intent" {
                self.evidence(
                    lease,
                    &mut journal,
                    index,
                    "captured",
                    &recovery,
                    &mut snapshot,
                )?;
            }
            if journal.steps.as_ref().ok_or(Error::Invalid)?[index].checkpoint == "evidence" {
                let actual = self.fingerprint(w, &journal)?;
                require(actual == w.intent.expected || actual == w.intent.desired)?;
                if actual == w.intent.expected {
                    match &w.recipe {
                        Recipe::LocalSSHPortPin(r) | Recipe::TestVMSettingsMigration(r) => self
                            .dispatch(
                                lease,
                                &journal,
                                index as u64 + 1,
                                Effect {
                                    owner: "settings",
                                    resource: &r.yard,
                                    expected: &r.expected,
                                    desired: &r.desired,
                                    source: &r.source,
                                },
                                |binding| N::ApplySettings {
                                    binding,
                                    recipe: r.clone(),
                                },
                            )?,
                        Recipe::TestYardOwnerRegistration(before) => {
                            self.bind_installed_recipe(&before.prepared_recipe, &bundle.installed)?;
                            self.dispatch(
                                lease,
                                &journal,
                                index as u64 + 1,
                                Effect {
                                    owner: "owner-registration",
                                    resource: "test-yard",
                                    expected: &w.intent.expected,
                                    desired: &w.intent.desired,
                                    source: &before.controller,
                                },
                                |binding| N::ApplyOwner {
                                    binding,
                                    before: before.clone(),
                                    token,
                                },
                            )?
                        }
                        Recipe::LedgerAdvance { before, after } => {
                            let current = self
                                .store
                                .migration_ledger(&self.registry, &self.registry_digest)?;
                            if ledger_execution::semantic_snapshot(&current)? != *before {
                                return Err(Error::Stale);
                            };
                            let wire = codec::decode_canonical(after, true)?;
                            let next = record::Ledger::new(wire, &self.registry)?;
                            self.verify_prepared_sources(&bundle.facts)?;
                            lease.advance_ledger(
                                &current,
                                &next,
                                &self.registry,
                                &self.registry_digest,
                            )?;
                        }
                        Recipe::LegacyV1Import { .. }
                        | Recipe::SourceImport
                        | Recipe::SourceEntrypoints => {
                            let step = self.ingress_step(&journal, w)?;
                            let kind = step.kind.clone();
                            let source = if step.static_pin.is_empty() {
                                step.expected.clone()
                            } else {
                                step.static_pin.clone()
                            };
                            self.dispatch(
                                lease,
                                &journal,
                                index as u64 + 1,
                                Effect {
                                    owner: "source-ingress",
                                    resource: &kind,
                                    expected: &step.expected,
                                    desired: &step.desired,
                                    source: &source,
                                },
                                |binding| N::ApplyIngress {
                                    binding,
                                    step: step.clone(),
                                },
                            )?;
                        }
                    }
                }
                require(self.fingerprint(w, &journal)? == w.intent.desired)?;
                self.evidence(
                    lease,
                    &mut journal,
                    index,
                    "applied",
                    &recovery,
                    &mut snapshot,
                )?;
            }
            if matches!(
                w.recipe,
                Recipe::LegacyV1Import { .. } | Recipe::SourceImport | Recipe::SourceEntrypoints
            ) {
                self.call(N::VerifyIngress {
                    session: self.session.clone(),
                    step: self.ingress_step(&journal, w)?,
                })?;
            }
            require(self.fingerprint(w, &journal)? == w.intent.desired)?;
            self.evidence(
                lease,
                &mut journal,
                index,
                "verified",
                &recovery,
                &mut snapshot,
            )?;
        }
        if matches!(journal.checkpoint.as_str(), "authorized" | "migrating") {
            journal.checkpoint = if journal.releases.from == journal.releases.target {
                "target-active"
            } else {
                "activation-intent"
            }
            .into();
            self.persist(lease, &journal, &mut snapshot)?;
        }
        if journal.checkpoint == "activation-intent" {
            self.store.reader.health()?;
            self.links.activate(lease, &journal.releases)?;
            journal.checkpoint = "target-active".into();
            self.persist(lease, &journal, &mut snapshot)?;
        }
        if journal.checkpoint == "target-active" {
            journal.checkpoint = "reconciling".into();
            self.persist(lease, &journal, &mut snapshot)?;
        }
        for (index, target) in targets.iter().enumerate() {
            let fresh = self.activation_capture(&target.owner)?.targets;
            let current = fresh
                .iter()
                .find(|v| {
                    v.yard == target.yard && v.class == target.class && v.scope == target.scope
                })
                .ok_or(Error::Stale)?;
            require(
                current.source == target.source
                    && current.project == target.project
                    && current.instance == target.instance
                    && current.instance_uuid == target.instance_uuid
                    && current.desired == target.desired,
            )?;
            let ordinal = work.len() as u64 + index as u64 + 1;
            if current.state == "stale" || self.needs_native_continuation(&journal, ordinal)? {
                if current.state == "stale" && current.actual != target.actual {
                    return Err(Error::Unknown);
                }
                self.bind_installed_recipe(&target.prepared_recipe, &bundle.installed)?;
                let applied = self.dispatch(
                    lease,
                    &journal,
                    ordinal,
                    Effect {
                        owner: &target.owner,
                        resource: &target.yard,
                        expected: &target.actual,
                        desired: &target.desired,
                        source: &target.source,
                    },
                    |binding| N::ApplyActivation {
                        binding,
                        target: target.clone(),
                    },
                );
                if let Err(cause) = applied {
                    let (after, observed, guarded) = crate::reducer::reobserve_activation_failure(
                        || {
                            self.activation_capture(&target.owner).ok().and_then(|a| {
                                a.targets.into_iter().find(|v| {
                                    v.yard == target.yard
                                        && v.class == target.class
                                        && v.scope == target.scope
                                        && v.validate().is_ok()
                                })
                            })
                        },
                        || {
                            self.observe_inner(
                                p::Goal {
                                    target: journal.goal.target.clone(),
                                    direction: journal.goal.direction.clone(),
                                },
                                false,
                                true,
                            )
                            .ok()
                        },
                        || self.links.observe().ok(),
                    );
                    let safe = observed.as_ref().map(|o| o.plan.wire());
                    let facts = crate::reducer::ActivationFailure {
                        journal: &journal,
                        owner: &target.owner,
                        phase: "reconcile",
                        before: &current.actual,
                        after: after
                            .as_ref()
                            .filter(|v| v.validate().is_ok())
                            .map(|v| (v.actual.as_str(), v.state != "stale")),
                        links: safe.map(|f| &f.links).or(guarded.as_ref()),
                        blockers: safe.and_then(|f| f.blockers.as_deref()).unwrap_or_default(),
                        current: observed
                            .as_ref()
                            .and_then(|o| o.journal.as_ref())
                            .is_some_and(|j| j.wire().transaction == journal.transaction),
                        warnings: &[],
                    };
                    let mut outcome = crate::reducer::activation_failure(&facts);
                    if !self.native_quiescent(Some(&journal)).is_ok()
                        || self.store.reader.health().is_err()
                    {
                        outcome = crate::reducer::operator(
                            outcome,
                            "recovery-ambiguous",
                            "native invocation closure cannot be proved safely",
                        );
                    }
                    if let Some(public) = self.first_cause.borrow().as_ref() {
                        crate::reducer::failure_diagnostic(
                            &mut outcome,
                            &public.message,
                            &public.retry,
                        );
                    } else if outcome.status == "operator-action-required" {
                        crate::reducer::failure_diagnostic(
                            &mut outcome,
                            &format!(
                                "activation reconciler {:?} failed during reconcile",
                                target.owner
                            ),
                            "",
                        );
                    }
                    let _ = cause;
                    return Ok(outcome);
                }
            }
            let verified = self.activation_capture(&target.owner)?.targets;
            require(
                verified
                    .iter()
                    .find(|v| {
                        v.yard == target.yard && v.class == target.class && v.scope == target.scope
                    })
                    .is_some_and(|v| v.state != "stale"),
            )?;
        }
        self.verify_targets(&targets)?;
        self.native_ready(&journal)?;
        self.store.reader.health()?;
        self.registry
            .history()
            .complete(
                self.store
                    .migration_ledger(&self.registry, &self.registry_digest)?
                    .ledger()
                    .authority(),
            )
            .map_err(|_| Error::Invalid)?;
        if journal.checkpoint != "complete" {
            journal.checkpoint = "complete".into();
            self.persist(lease, &journal, &mut snapshot)?;
        }
        crate::fixture::point("after-complete-journal")?;
        self.ready(lease, &record::Journal::new(journal)?)
    }
    pub(crate) fn cleanup_owner_ordinal(&self, journal: &j::Record) -> Result<Option<u64>> {
        let checked = record::Journal::new(journal.clone())?;
        let bundle = self.store.bundle(&checked, &self.registry)?;
        if !bundle
            .work
            .iter()
            .any(|w| matches!(w.recipe, Recipe::TestYardOwnerRegistration(_)))
        {
            return Ok(None);
        }
        let ordinal = bundle
            .work
            .len()
            .checked_add(bundle.targets.len())
            .and_then(|n| n.checked_add(1))
            .ok_or(Error::Bound)?;
        require(ordinal <= 2049)?;
        Ok(Some(ordinal as u64))
    }
    fn cleanup_owner(&self, lease: &UpdateLease<'_>, journal: &record::Journal) -> Result<()> {
        let Some(recipe) = self.pending_cleanup_recipe(journal)? else {
            return Ok(());
        };
        let crate::owner_cleanup::CleanupRecipe {
            before,
            token,
            source,
            desired,
            ordinal,
        } = recipe;
        self.dispatch(
            lease,
            journal.wire(),
            ordinal,
            Effect {
                owner: "owner-registration-cleanup",
                resource: "test-yard",
                expected: &desired,
                desired: &desired,
                source: &source,
            },
            |binding| N::CleanupOwner {
                binding,
                before,
                token,
            },
        )
    }
    pub(crate) fn ready(
        &self,
        lease: &UpdateLease<'_>,
        journal: &record::Journal,
    ) -> Result<p::Outcome> {
        self.store.reader.health()?;
        self.terminal_native_ready(lease, journal)?;
        // The fresh evaluator below owns the structured link-drift verdict.
        // Returning an untyped error here would lose its activation diagnostic
        // when the post-effect failure reducer reobserves this complete journal.
        self.links.observe()?;
        let ledger = self
            .store
            .migration_ledger(&self.registry, &self.registry_digest)?;
        self.registry
            .history()
            .complete(ledger.ledger().authority())
            .map_err(|_| Error::Invalid)?;
        let bundle = self.store.bundle(journal, &self.registry)?;
        // Complete is durable history, not a cached Ready proof. Reobserve the
        // full current release and native component facts after publication,
        // before projection conversion, cleanup or success.
        crate::fixture::point("before-terminal-observation")?;
        let terminal = self.observe_inner(
            p::Goal {
                target: journal.wire().goal.target.clone(),
                direction: journal.wire().goal.direction.clone(),
            },
            false,
            true,
        )?;
        require(
            terminal
                .journal
                .as_ref()
                .is_some_and(|j| j.wire().transaction == journal.wire().transaction),
        )?;
        let facts = terminal.plan.wire();
        let mut outcome = crate::reducer::evaluate(&crate::reducer::Facts {
            goal: p::Goal {
                target: journal.wire().goal.target.clone(),
                direction: journal.wire().goal.direction.clone(),
            },
            releases: journal.wire().releases.clone(),
            links: facts.links.clone(),
            journal: Some(journal.wire().clone()),
            steps: None,
            current_plan: journal.wire().resume_plan.clone(),
            current_intents: bundle.facts.intents.clone(),
            verified_authorization_plan: journal.wire().authorization_plan.clone(),
            current_artifact_digest: facts.artifact_digest.clone(),
            current_registry_digest: facts.registry_digest.clone(),
            current_catalog_digest: facts.catalog_digest.clone(),
            current_authorization_digest: journal.wire().authorization_digest.clone(),
            fixed_point_verified: terminal.work.is_empty()
                && terminal.targets.iter().all(|t| t.state != "stale")
                && self.terminal_native_ready(lease, journal).is_ok(),
        });
        if let Some(blocker) = facts.blockers.iter().flatten().next() {
            outcome = crate::reducer::operator(outcome, &blocker.code, &blocker.message);
            outcome.retry = blocker.retry.clone();
        }
        if outcome.status != "ready" || !outcome.reached_goal {
            return Ok(outcome);
        }
        // Projection conversion consumes the fresh terminal proof; the
        // completed journal or an old inspection alone cannot authorize it.
        if bundle.facts.decisions.iter().flatten().any(|d| {
            d.resource == "migration-history.checkpoint"
                && d.decision == "transform"
                && d.result == "compact-migration-history"
        }) {
            lease.compact_ledger(&ledger, &self.registry, &self.registry_digest)?;
        }
        // Completion and cleanup cannot substitute for native terminal evidence.
        let mut warnings = outcome.warnings.take().unwrap_or_default();
        if self.cleanup_owner(lease, journal).is_err() {
            self.store.reader.health()?;
            // A local cleanup error is a warning only after its exact native
            // contract proves no outstanding mutating work. Lost replies from
            // an unclosed invocation retain the pending journal and refuse Ready.
            self.native_quiescent(Some(journal.wire()))?;
            warnings.insert(0, "recovery cleanup is pending".into());
            outcome.warnings = Some(warnings);
            return Ok(outcome);
        }
        if crate::fixture::point("before-ready-cleanup")
            .and_then(|()| lease.cleanup_recovery(&journal.wire().transaction))
            .is_err()
        {
            warnings.push("recovery cleanup is pending".into());
        }
        if (lease
            .cleanup_transactions(&journal.wire().transaction)
            .is_err()
            || lease.cleanup_native_transactions().is_err())
            && warnings.is_empty()
        {
            warnings.push("recovery cleanup is pending".into());
        }
        self.store.reader.health()?;
        outcome.warnings = Some(warnings);
        Ok(outcome)
    }
    /// Resume consumes no new grant. It uses only the checked durable original
    /// intent and its exact immutable prepared bundle under a fresh update lease.
    pub fn resume(&self) -> Result<p::Outcome> {
        self.store.reader.health()?;
        if let Some(outcome) = self.resume_owner_cleanup()? {
            return Ok(outcome);
        }
        if let Some(outcome) = self.resume_migration_receipt()? {
            return Ok(outcome);
        }
        let (journal, snapshot) = self.store.read_checked_journal()?.ok_or(Error::Invalid)?;
        let bundle = self.store.bundle(&journal, &self.registry)?;
        require(
            bundle.session == self.session
                && bundle.helper == self.helper_digest()
                && (bundle.facts.releases == self.options.releases
                    || self.store.recovery_for(&journal)?.is_some()
                    || (bundle.facts.releases.from == bundle.facts.releases.target
                        && bundle.facts.releases.target == self.options.releases.target
                        && bundle.facts.releases.previous == self.links.observe()?.previous))
                && bundle.facts.releases == journal.wire().releases
                && bundle.facts.artifact_digest == self.options.artifact_digest,
        )?;
        self.outer.admit(
            &self.session,
            &journal.wire().resume_plan,
            &journal.wire().transaction,
        )?;
        let lease = self.store.lock()?;
        require(self.store.read(&Address::journal())? == snapshot)?;
        self.store.reader.health()?;
        self.run(&lease, journal, bundle.work, bundle.targets)
    }
    fn ingress_binding(&self, journal: &j::Record, work: &[Work]) -> native::IngressBinding {
        native::IngressBinding {
            transaction: journal.transaction.clone(),
            plan: journal.authorization_plan.clone(),
            releases: journal.releases.clone(),
            steps: journal
                .steps
                .iter()
                .flatten()
                .filter_map(|step| {
                    let kind = match step.id.as_str() {
                        "legacy-v1.import" => "legacy-v1-import",
                        "source-install.import" => "import",
                        "source-install.entrypoints" => "entrypoints",
                        _ => return None,
                    };
                    let static_pin = if kind == "legacy-v1-import" {
                        step.evidence
                            .as_ref()
                            .map(|e| e.recovery.clone())
                            .or_else(|| {
                                work.iter().find_map(|w| {
                                    if let Recipe::LegacyV1Import { static_pin } = &w.recipe {
                                        Some(static_pin.clone())
                                    } else {
                                        None
                                    }
                                })
                            })
                            .unwrap_or_default()
                    } else {
                        String::new()
                    };
                    Some(native::IngressBoundStep {
                        kind: kind.into(),
                        checkpoint: step.checkpoint.clone(),
                        expected: step.expected.clone(),
                        desired: step.desired.clone(),
                        static_pin,
                    })
                })
                .collect(),
        }
    }
    fn ingress_step(&self, journal: &j::Record, work: &Work) -> Result<native::IngressStep> {
        let kind = match work.recipe {
            Recipe::LegacyV1Import { .. } => "legacy-v1-import",
            Recipe::SourceImport => "import",
            Recipe::SourceEntrypoints => "entrypoints",
            _ => return Err(Error::Invalid),
        };
        let static_pin = if let Recipe::LegacyV1Import { static_pin } = &work.recipe {
            static_pin.clone()
        } else {
            String::new()
        };
        Ok(native::IngressStep {
            binding: self.ingress_binding(journal, std::slice::from_ref(work)),
            kind: kind.into(),
            expected: work.intent.expected.clone(),
            desired: work.intent.desired.clone(),
            static_pin,
        })
    }
    fn validate_ledger_edge(&self, w: &Work, before: &Snapshot, after: &[u8]) -> Result<()> {
        require(before.fingerprint() == w.intent.expected && digest(after) == w.intent.desired)?;
        let current = self
            .store
            .migration_ledger(&self.registry, &self.registry_digest)?;
        let from = if let Some(raw) = before.bytes() {
            record::Ledger::new(codec::decode_canonical(raw, true)?, &self.registry)?
                .authority()
                .clone()
        } else {
            self.registry.history().baseline()
        };
        let edge = self
            .registry
            .history()
            .next(&from)
            .map_err(|_| Error::Invalid)?;
        require(edge.migration().id == w.intent.migration)?;
        let next = record::Ledger::new(codec::decode_canonical(after, true)?, &self.registry)?;
        require(next.authority().domains() == edge.after().domains())?;
        for (domain, state) in next.authority().domains() {
            let observed = current
                .ledger()
                .authority()
                .domains()
                .get(domain)
                .ok_or(Error::Invalid)?;
            require(observed.applied().starts_with(state.applied()))?;
        }
        Ok(())
    }

    // Reobserve every approved settings source and the full same-file chain
    // before advancing any journal step. A future recipe never grants permission
    // to recapture a changed inherited source under the old consent.
    fn reobserve_prepared_settings(&self, work: &[Work]) -> Result<()> {
        let mut groups =
            std::collections::BTreeMap::<&str, Vec<&crate::work::SettingsRecipe>>::new();
        for w in work {
            if let Recipe::LocalSSHPortPin(r) | Recipe::TestVMSettingsMigration(r) = &w.recipe {
                groups.entry(&r.yard).or_default().push(r);
            }
        }
        for recipes in groups.values() {
            let mut physical = None;
            let mut states = vec![];
            for (n, r) in recipes.iter().enumerate() {
                if n > 0 {
                    require(
                        recipes[n - 1].target == r.target
                            && recipes[n - 1].desired_file == r.expected_file
                            && r.expected_exists,
                    )?;
                }
                let observed = self
                    .call(N::CaptureSettings {
                        session: self.session.clone(),
                        recipe: (*r).clone(),
                    })?
                    .actual;
                if let Some(prior) = &physical {
                    require(prior == &observed)?;
                } else {
                    physical = Some(observed)
                }
                if n == 0 {
                    states.push(crate::settings::snapshot_fingerprint(
                        r.expected_exists,
                        &r.expected_file,
                    )?);
                }
                states.push(crate::settings::snapshot_fingerprint(
                    true,
                    &r.desired_file,
                )?);
            }
            let observed = physical.ok_or(Error::Invalid)?;
            let cut = states
                .iter()
                .position(|s| s == &observed)
                .ok_or(Error::Stale)?;
            for r in recipes.iter().skip(cut) {
                let prepared = self
                    .call(N::PrepareSettings {
                        session: self.session.clone(),
                        yard: r.yard.clone(),
                        edits: r.edits.clone(),
                    })?
                    .recipe
                    .ok_or(Error::Invalid)?;
                require(
                    prepared.target == r.target
                        && prepared.expected_exists == r.expected_exists
                        && prepared.expected_file == r.expected_file
                        && prepared.desired_file == r.desired_file,
                )?;
            }
        }
        self.store.reader.health()
    }

    pub(crate) fn verify_recorded_step(&self, journal: &j::Record, step: &j::Step) -> Result<()> {
        if step.checkpoint == "intent" {
            return Ok(());
        }
        let embedded = step.evidence.as_ref().ok_or(Error::Invalid)?;
        if let Some(migration) = self
            .registry
            .wire()
            .migrations
            .iter()
            .flatten()
            .find(|m| m.id == step.migration)
        {
            if !step.id.ends_with(".ledger") {
                self.store.typed_recovery(journal, step, &migration.kind)?;
            }
        }
        for (name, kind) in [
            ("captured", EvidenceCheckpoint::Captured),
            ("applied", EvidenceCheckpoint::Applied),
            ("verified", EvidenceCheckpoint::Verified),
        ] {
            let raw = self
                .store
                .read(&Address::evidence(&journal.transaction, &step.id, kind)?)?;
            let value = record::Evidence::new(self.store.decode(
                raw.bytes().ok_or(Error::Invalid)?,
                true,
                true,
            )?)?;
            let v = value.wire();
            require(
                v.transaction == journal.transaction
                    && v.releases == journal.releases
                    && v.step == step.id
                    && v.expected == step.expected
                    && v.desired == step.desired
                    && v.checkpoint == name
                    && v.recovery == embedded.recovery
                    && v.observed
                        == if name == "captured" {
                            step.expected.clone()
                        } else {
                            step.desired.clone()
                        },
            )?;
            if step.checkpoint == "evidence" && name == "captured" || step.checkpoint == name {
                require(v == embedded)?;
                break;
            }
        }
        Ok(())
    }
    fn verify_settings_predecessor(
        &self,
        w: &Work,
        index: usize,
        work: &[Work],
        journal: &j::Record,
    ) -> Result<()> {
        match self.fingerprint(w, journal) {
            Ok(actual) if actual == w.intent.desired => return Ok(()),
            Err(Error::Stale) | Ok(_) => (),
            Err(e) => return Err(e),
        }
        let Recipe::TestVMSettingsMigration(before) = &w.recipe else {
            return Err(Error::Invalid);
        };
        for (n, next) in work.iter().enumerate().skip(index + 1) {
            let Recipe::LocalSSHPortPin(after) = &next.recipe else {
                continue;
            };
            if before.yard != after.yard {
                continue;
            }
            let step = &journal.steps.as_ref().ok_or(Error::Invalid)?[n];
            require(
                step.checkpoint != "intent"
                    && next.intent.decision == "canonicalize"
                    && before.target == after.target
                    && before.desired_file == after.expected_file
                    && after.before.len() == 1
                    && after.before[0].setting == "SSH_PORT"
                    && !after.before[0].present
                    && after.before[0].value.is_empty(),
            )?;
            self.verify_recorded_step(journal, step)?;
            require(self.fingerprint(next, journal)? == next.intent.desired)?;
            let projected = self
                .call(N::ProjectSettings {
                    session: self.session.clone(),
                    recipe: after.clone(),
                    edits: vec![crate::work::Edit {
                        setting: "SSH_PORT".into(),
                        value: None,
                    }],
                })?
                .actual;
            require(projected == after.expected_file && projected == before.desired_file)?;
            return Ok(());
        }
        Err(Error::Stale)
    }

    fn verify_targets(&self, targets: &[native::Target]) -> Result<()> {
        let mut owners = std::collections::BTreeSet::new();
        for target in targets {
            owners.insert(&target.owner);
        }
        for owner in owners {
            let fresh = self.activation_capture(owner)?.targets;
            let expected = targets
                .iter()
                .filter(|t| t.owner == *owner)
                .collect::<Vec<_>>();
            require(
                fresh
                    .iter()
                    .filter(|v| {
                        expected
                            .iter()
                            .any(|t| t.yard == v.yard && t.class == v.class && t.scope == v.scope)
                    })
                    .count()
                    == expected.len(),
            )?;
            for t in expected {
                let v = fresh
                    .iter()
                    .find(|v| v.yard == t.yard && v.class == t.class && v.scope == t.scope)
                    .ok_or(Error::Stale)?;
                require(
                    v.project == t.project
                        && v.instance == t.instance
                        && v.instance_uuid == t.instance_uuid
                        && v.source == t.source
                        && v.desired == t.desired
                        && v.state != "stale",
                )?;
            }
        }
        self.store.reader.health()
    }
}
