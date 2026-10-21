//! Sideband acceptance coordinator. The existing Model and main guards remain
//! authoritative; typed read outcomes justify parts, never callback snapshots.
use crate::{
    model::{Model, ReadAction},
    state::Scope,
    worker::{Details, ReadObservation},
};
use veranda_lib::observation::{Batch, Observation, ReadKind, ReadProof, Retirement, ViewCoverage};

pub const FLEET: u16 = 1;
pub const ACTIONS: u16 = 2;
pub const PRESETS: u16 = 4;
pub const SESSIONS: u16 = 8;
pub const PROFILES: u16 = 16;
pub const SETTINGS: u16 = 32;
pub const DIAGNOSTICS: u16 = 64;
pub const SYNC: u16 = 128;

#[derive(Clone, Debug, PartialEq)]
struct Key {
    scope: Scope,
    tab: String,
    generation: u64,
}
impl Key {
    fn model(model: &Model) -> Self {
        Self {
            scope: Scope {
                connection: model.selection.connection_id.clone(),
                yard: model.selection.yard_name.clone(),
            },
            tab: model.selection.tab.clone(),
            generation: model.generation,
        }
    }
}
#[derive(Clone, Copy, Debug, Default, PartialEq)]
pub struct Parts {
    pub complete: u16,
    pub failed: u16,
    pub not_applicable: u16,
}
impl Parts {
    pub fn details(details: &Details) -> Self {
        let caps = details.capabilities();
        let profiles = caps.iter().any(|cap| cap == "profile-list-v1");
        let mut parts = Self {
            complete: ACTIONS | SESSIONS | SETTINGS,
            ..Self::default()
        };
        if profiles {
            parts.complete |= PROFILES | PRESETS;
        } else {
            parts.not_applicable |= PROFILES | PRESETS;
        }
        match details {
            Details::Host(_) => parts.complete |= DIAGNOSTICS | SYNC,
            Details::Yard(yard) => match yard.observed_diagnostics_ready() {
                Some(true) => parts.complete |= DIAGNOSTICS,
                _ => parts.failed |= DIAGNOSTICS,
            },
        }
        parts
    }
    fn includes(self, mask: u16) -> bool {
        (self.complete | self.not_applicable) & mask == mask && self.failed & mask == 0
    }
}
struct Read {
    proof: ReadProof,
    key: Key,
    invalidation: u64,
    parts: Parts,
    requested: Scope,
    hint_scope: Option<Scope>,
    hints: Batch,
}
/// Raw hints, requested scope and typed accepted scope stay distinguishable.
/// This borrowed diagnostic interface does not serialize domain identifiers.
pub struct PartProvenance<'a> {
    pub requested: &'a Scope,
    pub hint_scope: Option<&'a Scope>,
    pub hints: &'a Batch,
    pub actual: &'a Scope,
    pub proof: &'a ReadProof,
    pub generation: u64,
    pub invalidation: u64,
    pub parts: Parts,
}
impl Read {
    fn provenance(&self) -> PartProvenance<'_> {
        PartProvenance {
            requested: &self.requested,
            hint_scope: self.hint_scope.as_ref(),
            hints: &self.hints,
            actual: &self.key.scope,
            proof: &self.proof,
            generation: self.key.generation,
            invalidation: self.invalidation,
            parts: self.parts,
        }
    }
}
pub struct Application {
    pub revision: u64,
    pub batch: Batch,
}

pub struct ViewObservation {
    observation: Observation,
    #[cfg(target_os = "linux")]
    correlation: Option<std::sync::Arc<std::sync::Mutex<crate::event_correlation::Cohorts>>>,
    #[cfg(target_os = "linux")]
    readiness: Option<crate::event_correlation::RuntimeHandle>,
    view: u64,
    scope: u64,
    key: Option<Key>,
    revision: u64,
    request: Batch,
    selection: Batch,
    selection_attempt: Option<(Scope, u64)>,
    represented: Batch,
    // Bounded by the observation's live ledger, including hints which cannot
    // be promoted to typed coverage. Replacement must not silently lose them.
    outstanding: Batch,
    fleet: Option<Read>,
    details: Option<Read>,
    ready: bool,
}
impl ViewObservation {
    pub fn fleet_provenance(&self) -> Option<PartProvenance<'_>> {
        self.fleet.as_ref().map(Read::provenance)
    }
    pub fn details_provenance(&self) -> Option<PartProvenance<'_>> {
        self.details.as_ref().map(Read::provenance)
    }
    pub fn new(observation: Observation) -> Self {
        let view = observation.view();
        Self {
            observation,
            #[cfg(target_os = "linux")]
            correlation: None,
            #[cfg(target_os = "linux")]
            readiness: None,
            view,
            scope: 0,
            key: None,
            revision: 0,
            request: Batch::default(),
            selection: Batch::default(),
            selection_attempt: None,
            represented: Batch::default(),
            outstanding: Batch::default(),
            fleet: None,
            details: None,
            ready: false,
        }
    }
    #[cfg(target_os = "linux")]
    pub fn with_cohorts(
        observation: Observation,
        correlation: std::sync::Arc<std::sync::Mutex<crate::event_correlation::Cohorts>>,
    ) -> Self {
        let mut view = Self::new(observation);
        view.correlation = Some(correlation);
        view
    }
    #[cfg(target_os = "linux")]
    pub fn with_runtime(
        observation: Observation,
        runtime: crate::event_correlation::RuntimeHandle,
    ) -> Self {
        let mut view = Self::with_cohorts(observation, runtime.cohorts.clone());
        view.readiness = Some(runtime);
        view
    }
    pub fn collects_sources(&self) -> bool {
        #[cfg(target_os = "linux")]
        {
            self.correlation.is_some()
        }
        #[cfg(not(target_os = "linux"))]
        {
            false
        }
    }
    /// Typed Owner identity is captured from this result before the Model consumes
    /// it. This optional sideband never changes the ordinary acceptance path.
    pub fn fleet_identified(
        &mut self,
        model: &Model,
        generation: u64,
        connection: &Option<String>,
        invalidation: u64,
        current: bool,
        success: bool,
        action: &ReadAction,
        returned_owner: Option<&str>,
        metadata: Option<ReadObservation>,
    ) {
        #[cfg(target_os = "linux")]
        if let Some(correlation) = &self.correlation {
            correlation.lock().unwrap().fleet(
                model,
                generation,
                connection,
                current,
                action,
                returned_owner,
                metadata.as_ref(),
            );
            if let Some(readiness) = &self.readiness {
                readiness.changed();
            }
        }
        #[cfg(not(target_os = "linux"))]
        let _ = returned_owner;
        self.fleet(
            model,
            generation,
            connection,
            invalidation,
            current,
            success,
            action,
            metadata,
        );
    }
    fn retire(&self, batch: &Batch) {
        self.observation
            .retire(&self.observation.live(batch), Retirement::Unknown);
    }
    fn reset(&mut self, key: Option<Key>) {
        for batch in [
            &self.request,
            &self.selection,
            &self.represented,
            &self.outstanding,
        ] {
            self.retire(batch);
        }
        for read in [&self.fleet, &self.details].into_iter().flatten() {
            self.retire(read.proof.batch());
        }
        self.request = Batch::default();
        self.selection = Batch::default();
        self.selection_attempt = None;
        self.represented = Batch::default();
        self.outstanding = Batch::default();
        self.fleet = None;
        self.details = None;
        self.ready = false;
        self.key = key;
        self.scope += 1;
    }
    fn sync(&mut self, model: &Model) {
        let key = Key::model(model);
        if self.key.as_ref() != Some(&key) {
            self.reset(Some(key));
        }
    }
    pub fn request(&mut self, model: &Model, retry: bool) -> Batch {
        self.sync(model);
        if retry && !self.observation.live(&self.request).tickets().is_empty() {
            return Batch::default();
        }
        let batch =
            self.observation
                .view_start(self.view, false, model.selection.connection_id.is_some());
        self.request = self.observation.merge(&self.request, &batch);
        batch
    }
    pub fn refused(&mut self, batch: &Batch) {
        self.retire(batch);
    }
    /// Called immediately before the existing Model::select, without RPC sources.
    pub fn selection_start(&mut self, model: &Model, requested: Scope) -> Batch {
        let remote = requested.connection.is_some();
        self.selection_attempt = Some((requested, model.generation));
        self.observation.view_start(self.view, true, remote)
    }
    pub fn selected(&mut self, model: &Model, start: Batch) {
        let accepted = self
            .selection_attempt
            .take()
            .is_some_and(|(requested, generation)| {
                generation.checked_add(1) == Some(model.generation)
                    && requested == Key::model(model).scope
            });
        self.reset(Some(Key::model(model)));
        if accepted && model.initialized && model.host().snapshot.is_some() {
            self.selection = start;
        } else {
            self.retire(&start);
        }
    }
    pub fn changed(&mut self, model: &Model) {
        self.sync(model);
    }
    pub fn close(&mut self) {
        self.reset(None);
    }
    fn reject_metadata(&self, metadata: Option<ReadObservation>) {
        if let Some(metadata) = metadata {
            self.retire(&metadata.batch);
            if let Some(proof) = metadata.proof {
                self.retire(proof.batch());
            }
        }
    }
    fn retain(&mut self, metadata: &ReadObservation) {
        self.outstanding = self.observation.merge(&self.outstanding, &metadata.batch);
        if let Some(proof) = &metadata.proof {
            self.outstanding = self.observation.merge(&self.outstanding, proof.batch());
        }
    }
    pub fn fleet(
        &mut self,
        model: &Model,
        generation: u64,
        connection: &Option<String>,
        invalidation: u64,
        current: bool,
        success: bool,
        action: &ReadAction,
        metadata: Option<ReadObservation>,
    ) {
        if *action == ReadAction::Retry {
            if let Some(metadata) = &metadata {
                self.retain(metadata);
            }
            return;
        }
        if *action != ReadAction::Applied || !model.accepts_reply(generation, connection) {
            self.reject_metadata(metadata);
            return;
        }
        if !success || !current {
            self.reject_metadata(metadata);
            self.reset(Some(Key::model(model)));
            return;
        }
        let key = Key::model(model);
        // Successful typed Fleet may normalize a disappeared yard within the
        // same owner/generation. Preserve starts; bind to ACTUAL resulting scope.
        if self.key.as_ref().is_some_and(|old| {
            old.scope.connection == key.scope.connection && old.generation == key.generation
        }) {
            self.key = Some(key.clone());
            if self.details.as_ref().is_some_and(|read| read.key != key) {
                if let Some(read) = self.details.take() {
                    self.retire(read.proof.batch());
                }
            }
        } else {
            self.sync(model);
        }
        if let Some(metadata) = metadata {
            if metadata.scope.connection != *connection {
                self.reject_metadata(Some(metadata));
                return;
            }
            self.retain(&metadata);
            self.fleet = None;
            if let Some(proof) = metadata.proof.filter(|proof| proof.kind() == ReadKind::Rpc) {
                self.fleet = Some(Read {
                    proof,
                    key,
                    invalidation,
                    parts: Parts {
                        complete: FLEET,
                        ..Parts::default()
                    },
                    requested: metadata.scope,
                    hint_scope: metadata.hint_scope,
                    hints: metadata.batch,
                });
            }
        }
    }
    pub fn details(
        &mut self,
        model: &Model,
        generation: u64,
        scope: &Scope,
        invalidation: u64,
        parts: Parts,
        metadata: Option<ReadObservation>,
    ) {
        if generation != model.generation || Key::model(model).scope != *scope {
            self.reject_metadata(metadata);
            return;
        }
        #[cfg(target_os = "linux")]
        if let Some(correlation) = &self.correlation {
            correlation.lock().unwrap().details(
                model,
                generation,
                scope,
                true,
                parts.failed == 0,
                metadata.as_ref(),
            );
            if let Some(readiness) = &self.readiness {
                readiness.changed();
            }
        }
        self.sync(model);
        if let Some(metadata) = metadata {
            if metadata.scope != *scope {
                self.reject_metadata(Some(metadata));
                return;
            }
            self.retain(&metadata);
            if let Some(proof) = metadata.proof.filter(|proof| proof.kind() == ReadKind::Rpc) {
                self.details = Some(Read {
                    proof,
                    key: Key::model(model),
                    invalidation,
                    parts,
                    requested: metadata.scope,
                    hint_scope: metadata.hint_scope,
                    hints: metadata.batch,
                });
            }
        }
    }
    pub fn details_failed(&mut self, model: &Model, metadata: Option<ReadObservation>) {
        self.reject_metadata(metadata);
        self.details = None;
        if Self::required(&model.selection.tab, false) != FLEET {
            self.retire(&self.request);
            self.retire(&self.represented);
            self.ready = false;
        }
    }
    pub fn discard(&self, metadata: Option<ReadObservation>) {
        self.reject_metadata(metadata);
    }
    /// A product retry is not an accepted part and must not lose its originals.
    pub fn deferred(&mut self, metadata: Option<ReadObservation>) {
        if let Some(metadata) = &metadata {
            self.retain(metadata);
        }
    }
    pub fn required(tab: &str, details_rendered: bool) -> u16 {
        FLEET
            | match tab {
                "Overview" => {
                    if details_rendered {
                        ACTIONS
                    } else {
                        0
                    }
                }
                "Yards" => PRESETS,
                "Projects" => SESSIONS,
                "Profiles" => PROFILES,
                "Settings" => SETTINGS,
                "Diagnostics" => DIAGNOSTICS,
                "Sync" => SYNC,
                _ => 0,
            }
    }
    fn covers(read: &Read, ticket: &veranda_lib::observation::Ticket) -> bool {
        if read.proof.source() == ticket.source() {
            read.proof
                .batch()
                .tickets()
                .iter()
                .any(|covered| covered.ordinal() == ticket.ordinal())
        } else {
            read.proof.started() >= ticket.started()
        }
    }
    /// Called only after real widgets have been rendered. Old accepted A can
    /// survive a later invalidation B while its actual body remains represented;
    /// no always-latest-at-paint barrier is imposed.
    pub fn rendered(&mut self, model: &Model, details_rendered: bool) -> Option<Application> {
        self.sync(model);
        self.ready = false;
        if !model.initialized || model.host().snapshot.is_none() {
            return None;
        }
        let key = Key::model(model);
        let required = Self::required(&key.tab, details_rendered);
        let mut represented = Batch::default();
        let selection = self.observation.live(&self.selection);
        if !selection.tickets().is_empty() {
            self.observation.represent(
                &selection,
                ViewCoverage {
                    view: self.view,
                    scope: self.scope,
                    generation: key.generation,
                    invalidation: 0,
                    parts: FLEET,
                    whole: false,
                    cached: true,
                    remote: key.scope.connection.is_some(),
                },
            );
            represented = selection;
        }
        if let Some(fleet) = self.fleet.as_ref().filter(|read| read.key == key) {
            let details = self
                .details
                .as_ref()
                .filter(|read| read.key == key && details_rendered);
            // Only the accepted Fleet proof can introduce inventory receipts.
            // Details can supplement their visible parts, never lend Fleet
            // authority to another session's receipt by admission time alone.
            let candidates = self.observation.inventory(fleet.proof.batch());
            let candidates = self.observation.merge(&candidates, &self.request);
            for ticket in candidates.tickets() {
                // Candidate membership originates in the ACTUAL Fleet proof (or
                // explicit GUI request). Cross-part joins use trusted same-key
                // typed calls, never another session's frontier or owner ordinal.
                let fleet_ok = Self::covers(fleet, ticket);
                let detail_mask = required & !FLEET;
                let details_ok = detail_mask == 0
                    || details.is_some_and(|read| {
                        read.parts.includes(detail_mask) && Self::covers(read, ticket)
                    });
                if fleet_ok && details_ok {
                    let one = self.observation.ticket_batch(ticket);
                    self.observation.represent(
                        &one,
                        ViewCoverage {
                            view: self.view,
                            scope: self.scope,
                            generation: key.generation,
                            invalidation: fleet.invalidation,
                            parts: required,
                            whole: true,
                            cached: false,
                            remote: key.scope.connection.is_some(),
                        },
                    );
                    represented = self.observation.merge(&represented, &one);
                } else if details.is_some_and(|read| read.parts.failed & detail_mask != 0) {
                    self.retire(&self.observation.ticket_batch(ticket));
                }
            }
        }
        if represented.tickets().is_empty() {
            return None;
        }
        self.represented = self.observation.live(&represented);
        self.revision += 1;
        self.ready = true;
        Some(Application {
            revision: self.revision,
            batch: represented,
        })
    }
    pub fn snapshot_ready(&self) -> bool {
        self.ready
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use veranda_lib::{
        local_fleet::{LocalFleetSnapshot, LocalOwnerHost, LocalYard, NativeError},
        observation::{Origin, TerminalOutcome},
    };
    fn loaded() -> Model {
        let mut model = Model::new(vec![]);
        model.update(
            &None,
            Ok(LocalFleetSnapshot {
                engine_version: "test".into(),
                observed_at: "test".into(),
                current_yard_name: "default".into(),
                owner: LocalOwnerHost {
                    id: "synthetic".into(),
                    yards: vec![LocalYard {
                        name: "default".into(),
                        kind: "local".into(),
                        state: "RUNNING".into(),
                        projects: vec![],
                    }],
                },
                connection_id: None,
                capabilities: vec![],
                veranda_version: None,
            }),
        );
        model.initialize(None);
        model
    }
    #[test]
    fn loaded_body_is_distinct_from_placeholder_rpc_and_status_only_render() {
        let observer = Observation::new();
        let mut view = ViewObservation::new(observer.clone());
        let mut empty = Model::new(vec![]);
        empty.initialize(None);
        let start = view.selection_start(
            &empty,
            Scope {
                connection: None,
                yard: None,
            },
        );
        empty.select(None, None);
        view.selected(&empty, start);
        assert!(view.rendered(&empty, false).is_none());
        assert_eq!(
            observer.terminals()[0].outcome,
            TerminalOutcome::Retired(Retirement::Unknown)
        );
        let mut model = loaded();
        let start = view.selection_start(
            &model,
            Scope {
                connection: None,
                yard: None,
            },
        );
        model.select(None, None);
        view.selected(&model, start);
        model.invalidate(&None, false); // Cached body still exists; Refreshing alone is not the body.
        let applied = view.rendered(&model, false).unwrap();
        observer.retire(&applied.batch, Retirement::Unknown);
        let sample = observer.terminals().last().unwrap().clone();
        assert_eq!(sample.origin, Origin::Gui);
        assert!(sample.coverage.unwrap().cached);
        assert!(!sample.coverage.unwrap().whole);
        let starts = observer.counts().gui_start_attempts;
        assert!(view.rendered(&model, false).is_none());
        assert_eq!(
            observer.counts().gui_start_attempts,
            starts,
            "ordinary status render creates no measurement start"
        );
        view.request(&model, false);
        assert!(
            view.rendered(&model, false).is_none(),
            "cached model is not an authoritative RPC request proof"
        );
        view.close();
        assert_eq!(observer.counts().pending, 0);
    }
    #[test]
    fn unrepresented_retry_and_replaced_hints_retire_explicitly_on_close() {
        let observer = Observation::new();
        let mut view = ViewObservation::new(observer.clone());
        let model = loaded();
        view.changed(&model);
        let scope = Key::model(&model).scope;
        // Opaque live batches exercise carry accounting only; these synthetic
        // GUI starts are deliberately NOT represented as RPC inventory proofs.
        for action in [ReadAction::Retry, ReadAction::Applied, ReadAction::Applied] {
            let batch = observer.view_start(observer.view(), false, false);
            view.fleet(
                &model,
                model.generation,
                &None,
                0,
                true,
                true,
                &action,
                Some(ReadObservation {
                    scope: scope.clone(),
                    hint_scope: Some(scope.clone()),
                    batch,
                    proof: None,
                }),
            );
            assert!(view.rendered(&model, false).is_none());
        }
        let batch = observer.view_start(observer.view(), false, false);
        view.deferred(Some(ReadObservation {
            scope: scope.clone(),
            hint_scope: Some(scope),
            batch,
            proof: None,
        }));
        assert_eq!(observer.counts().pending, 4);
        view.close();
        assert_eq!(observer.counts().pending, 0);
        assert_eq!(observer.terminals().len(), 4);
        assert!(
            observer
                .terminals()
                .iter()
                .all(|row| row.outcome == TerminalOutcome::Retired(Retirement::Unknown))
        );
    }
    #[test]
    fn rejected_selection_does_not_measure_the_previous_loaded_body() {
        let observer = Observation::new();
        let mut view = ViewObservation::new(observer.clone());
        let mut model = loaded();
        let start = view.selection_start(
            &model,
            Scope {
                connection: Some("missing".into()),
                yard: None,
            },
        );
        let generation = model.generation;
        assert!(!model.select(Some("missing".into()), None));
        assert_eq!(model.generation, generation);
        view.selected(&model, start);
        assert!(view.rendered(&model, false).is_none());
        assert_eq!(
            observer.terminals()[0].outcome,
            TerminalOutcome::Retired(Retirement::Unknown)
        );
        assert_eq!(observer.counts().pending, 0);
    }
    #[test]
    fn real_model_error_applied_and_ignored_cannot_masquerade_as_success() {
        let observer = Observation::new();
        let mut view = ViewObservation::new(observer.clone());
        let mut model = loaded();
        let start = view.request(&model, false);
        let generation = model.generation;
        let action = model.read_complete(
            generation,
            &None,
            true,
            Err(NativeError {
                code: "synthetic".into(),
                message: "unavailable".into(),
            }),
        );
        assert_eq!(action, ReadAction::Applied);
        view.fleet(&model, generation, &None, 0, true, false, &action, None);
        assert!(view.rendered(&model, false).is_none());
        assert!(
            observer
                .terminals()
                .iter()
                .any(|row| row.ticket == start.tickets()[0].ordinal()
                    && row.outcome == TerminalOutcome::Retired(Retirement::Unknown))
        );
        let start = view.request(&model, false);
        let action = model.read_complete(
            generation + 1,
            &None,
            true,
            Err(NativeError {
                code: "synthetic".into(),
                message: "unavailable".into(),
            }),
        );
        assert_eq!(action, ReadAction::Ignored);
        view.fleet(&model, generation + 1, &None, 0, true, false, &action, None);
        assert!(view.rendered(&model, false).is_none());
        model.select_tab("Settings".into());
        view.changed(&model);
        assert!(
            observer
                .terminals()
                .iter()
                .any(|row| row.ticket == start.tickets()[0].ordinal()
                    && row.outcome == TerminalOutcome::Retired(Retirement::Unknown))
        );
        assert!(!view.snapshot_ready());
    }
}
