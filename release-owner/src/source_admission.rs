//! Complete read-only protected source admission. No runtime readiness or grant.
use crate::{
    digest, graph,
    inventory::Namespace,
    migration_owner,
    record::{self, require},
    source,
    store::{Address, Snapshot, Store},
    wire::journal_v2 as j,
    Error, Result,
};
use std::collections::{BTreeMap, BTreeSet};
pub struct LedgerState {
    ledger: record::Ledger,
    projection: Snapshot,
    authority: Snapshot,
    checkpoint: Option<record::Checkpoint>,
    historical: Option<record::Ledger>,
}
impl LedgerState {
    pub fn ledger(&self) -> &record::Ledger {
        &self.ledger
    }
    pub fn projection(&self) -> &Snapshot {
        &self.projection
    }
    pub fn authority(&self) -> &Snapshot {
        &self.authority
    }
    pub fn checkpoint(&self) -> Option<&record::Checkpoint> {
        self.checkpoint.as_ref()
    }
}
pub struct SourceMetadata {
    journal: Option<record::Journal>,
    ledger: LedgerState,
    binding: String,
}
impl SourceMetadata {
    pub fn journal(&self) -> Option<&record::Journal> {
        self.journal.as_ref()
    }
    pub fn ledger(&self) -> &LedgerState {
        &self.ledger
    }
    pub fn binding(&self) -> &str {
        &self.binding
    }
}
impl Store {
    pub fn migration_ledger(
        &self,
        registry: &record::Registry,
        registry_digest: &str,
    ) -> Result<LedgerState> {
        let projection = self.read(&Address::ledger())?;
        let authority = self.read(&Address::checkpoint())?;
        if let Some(bytes) = authority.bytes() {
            let view = if let Some(p) = projection.bytes() {
                record::Projection::from_reader(p.to_vec(), &self.reader, false)?
            } else {
                record::Projection::absent()
            };
            let checkpoint = record::Checkpoint::new(
                self.decode(bytes, true, false)?,
                registry,
                registry_digest,
                &view,
            )?;
            let ledger = record::Ledger::new(checkpoint.ledger_view().wire().clone(), registry)?;
            // Only the checked checkpoint's exact original projection can be an older
            // historical anchor. A journal's claimed fingerprint is never an anchor.
            let historical = projection.bytes().and_then(|p| {
                self.decode(p, true, false)
                    .and_then(|v| record::Ledger::new(v, registry))
                    .ok()
            });
            Ok(LedgerState {
                ledger,
                projection,
                authority,
                checkpoint: Some(checkpoint),
                historical,
            })
        } else {
            let ledger = if let Some(p) = projection.bytes() {
                record::Ledger::new(self.decode(p, true, false)?, registry)?
            } else {
                record::Ledger::new(
                    self.decode(
                        &migration_owner::ledger_bytes(&registry.history().baseline())?,
                        true,
                        false,
                    )?,
                    registry,
                )?
            };
            Ok(LedgerState {
                ledger,
                authority: projection.clone(),
                projection,
                checkpoint: None,
                historical: None,
            })
        }
    }
    pub fn inspect_source(
        &self,
        registry: &record::Registry,
        registry_digest: &str,
    ) -> Result<SourceMetadata> {
        let before = self.source_metadata()?;
        let snapshot = self.read(&Address::journal())?;
        let current = snapshot
            .bytes()
            .map(|p| record::Journal::new(self.decode(p, true, true)?))
            .transpose()?;
        if let Some(current) = &current {
            let j = current.wire();
            let mut original = None;
            for (ssh, publication) in [(false, false), (true, false), (false, true), (true, true)] {
                let catalog = migration_owner::builtin_catalog(ssh, publication)?;
                if catalog.digest() == j.catalog_digest {
                    original = Some(catalog);
                    break;
                }
            }
            let original = original.ok_or(Error::Invalid)?;
            require(
                j.registry_digest == registry_digest
                    && j.source_ingress.is_none()
                    && registry
                        .wire()
                        .migrations
                        .iter()
                        .flatten()
                        .all(|m| original.supports(&m.kind, &m.domain)),
            )?;
            require(
                j.checkpoint == "complete"
                    || (j.checkpoint == "reconciling"
                        && j.goal.direction == "activate-target"
                        && crate::wire::empty_vec(&j.steps)
                        && !crate::recovery::recovery_transaction(&j.transaction)),
            )?;
            self.migration_owner_for(current)?;
            self.recovery_for(current)?;
        }
        let ledger = self.migration_ledger(registry, registry_digest)?;
        if let Some(current) = &current {
            self.check_artifacts(current.wire(), registry)?;
            registry
                .history()
                .complete(ledger.ledger.authority())
                .map_err(|_| Error::Invalid)?;
            require(ledger.authority.bytes().is_some())?;
            if current.wire().checkpoint == "complete" {
                self.source_bound(
                    current.wire(),
                    registry,
                    &ledger,
                    Some(if ledger.checkpoint.is_some() {
                        digest(&migration_owner::ledger_bytes(ledger.ledger.authority())?)
                    } else {
                        ledger.projection.fingerprint()
                    }),
                )?;
            }
        }
        let owners = self.migration_owner_receipts()?;
        let mut owner_predecessors = BTreeSet::new();
        let mut owner_receipts = Vec::new();
        for receipt in owners.into_values() {
            self.owner_completion(&receipt, current.as_ref())?;
            let catalog = migration_owner::builtin_catalog(true, true)?;
            let original = record::Registry::new(
                self.decode(
                    receipt.wire().registry.as_deref().ok_or(Error::Invalid)?,
                    false,
                    false,
                )?,
                &catalog,
            )?;
            self.owner_predecessor_artifacts(
                &record::Journal::new(receipt.wire().predecessor.clone())?,
                &original,
            )?;
            owner_predecessors.insert(receipt.wire().predecessor.transaction.clone());
            owner_receipts.push(receipt);
        }
        let ordinary = graph::OrdinaryGraph::read(self)?;
        let (directories, archives) = ordinary.source_parts()?;
        let state = graph::State::read(self)?;
        let mut journals = BTreeMap::new();
        if let Some(current) = &current {
            journals.insert(current.wire().transaction.clone(), current.wire().clone());
        }
        for archive in archives.values() {
            journals.insert(
                archive.wire().journal.transaction.clone(),
                archive.wire().journal.clone(),
            );
        }
        for (id, terminal) in state.source_terminals() {
            let t = terminal.wire();
            journals.insert(
                t.receipt.predecessor.transaction.clone(),
                t.receipt.predecessor.clone(),
            );
            journals.insert(
                t.receipt.successor.transaction.clone(),
                t.receipt.successor.clone(),
            );
            if let Some(c) = &t.completed {
                journals.insert(id.clone(), c.clone());
            }
            if let Some(c) = &t.predecessor_completed {
                journals.insert(c.transaction.clone(), c.clone());
            }
        }
        if current.is_none() {
            require(directories.iter().all(|id| {
                owner_predecessors.contains(id)
                    || journals.get(id).is_some_and(|j| j.checkpoint == "complete")
            }))?;
        }
        for id in directories {
            if owner_predecessors.contains(id) {
                continue;
            }
            let bound = if let Some(j) = journals.get(id) {
                Some(j.clone())
            } else {
                self.reconstruct_terminal(id, registry)
                    .ok()
                    .map(|r| r.journal)
            };
            if let Some(bound) = bound {
                if self.source_bound(&bound, registry, &ledger, None).is_ok() {
                    continue;
                }
            }
            let mut proved = false;
            for receipt in &owner_receipts {
                let catalog = migration_owner::builtin_catalog(true, true)?;
                let source_registry = record::Registry::new(
                    self.decode(
                        receipt
                            .wire()
                            .source_registry
                            .as_deref()
                            .ok_or(Error::Invalid)?,
                        false,
                        false,
                    )?,
                    &catalog,
                )?;
                if let Ok(retained) = self.retained_terminal(id, &source_registry, receipt.source())
                {
                    let target = &retained.releases().target;
                    let allowed = target == &receipt.wire().predecessor.releases.from
                        || receipt
                            .wire()
                            .previous_source
                            .as_ref()
                            .is_some_and(|p| &p.release == target);
                    let terminal = retained
                        .steps()
                        .iter()
                        .rfind(|s| s.id == format!("{}.ledger", s.migration));
                    if allowed && terminal.is_none_or(|s| s.desired == receipt.wire().projection) {
                        proved = true;
                        break;
                    }
                }
            }
            require(proved)?;
        }
        let next = archives
            .iter()
            .map(|(successor, a)| (a.wire().journal.transaction.clone(), successor.clone()))
            .collect::<BTreeMap<_, _>>();
        for (successor, archive) in archives {
            let mut selected = journals.get(successor).ok_or(Error::Invalid)?;
            require(selected.authorization_plan == archive.wire().authorization_plan)?;
            let mut remaining = archives.len();
            while selected.checkpoint != "complete" {
                remaining = remaining.checked_sub(1).ok_or(Error::Invalid)?;
                selected = journals
                    .get(next.get(&selected.transaction).ok_or(Error::Invalid)?)
                    .ok_or(Error::Invalid)?;
            }
        }
        state.source_closed()?;
        require(before.binding() == self.source_metadata()?.binding())?;
        Ok(SourceMetadata {
            journal: current,
            ledger,
            binding: before.binding().into(),
        })
    }
    fn source_bound(
        &self,
        journal: &j::Record,
        registry: &record::Registry,
        state: &LedgerState,
        terminal: Option<String>,
    ) -> Result<()> {
        self.check_artifacts(journal, registry)?;
        source::terminal_bindings_at(journal, registry, &state.ledger, state.historical.as_ref())?;
        if let Some(terminal) = terminal {
            require(
                journal
                    .steps
                    .iter()
                    .flatten()
                    .rfind(|s| s.id == format!("{}.ledger", s.migration))
                    .is_none_or(|s| s.desired == terminal),
            )?;
        }
        Ok(())
    }
    pub(crate) fn owner_completion(
        &self,
        receipt: &migration_owner::Receipt,
        current: Option<&record::Journal>,
    ) -> Result<()> {
        let owners = self.migration_owner_receipts()?;
        require(
            owners
                .get(&receipt.wire().successor.transaction)
                .is_some_and(|durable| durable.wire() == receipt.wire()),
        )?;
        let terminal = !self
            .inventory(Namespace::MigrationCompleted)?
            .records()
            .is_empty();
        require(
            terminal
                || current.is_some_and(|j| {
                    j.wire().checkpoint == "complete" && receipt.matches_successor(j.wire()).is_ok()
                }),
        )
    }
}
