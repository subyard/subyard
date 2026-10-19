//! Checked ordinary/recovery graph and exact closure-based retirement decisions.
use crate::{
    codec, digest,
    inventory::{Inventory, Namespace},
    record::{self, require},
    recovery::{self, Frontier, Receipt, Terminal},
    store::{self, Address, Snapshot, Store},
    wire::recovery_v1 as r,
    Error, Result,
};
use std::collections::{BTreeMap, BTreeSet};
pub struct OrdinaryGraph {
    directories: BTreeSet<String>,
    archives: BTreeMap<String, record::Archive>,
    pending: BTreeSet<String>,
    edges: BTreeMap<String, String>,
    successors: BTreeMap<String, String>,
}
impl OrdinaryGraph {
    pub fn read(store: &Store) -> Result<Self> {
        let pairs = store.transaction_pairs()?;
        let directories = pairs.keys().cloned().collect::<BTreeSet<_>>();
        let mut archives = BTreeMap::new();
        let mut pending = BTreeSet::new();
        let mut edges = BTreeMap::new();
        let mut successors = BTreeMap::new();
        for (id, pair) in pairs {
            let Some(bytes) = pair.exact()? else {
                continue;
            };
            let value = record::Archive::new(store.decode(bytes, true, true)?)?;
            let predecessor = value.wire().replacement.transaction.clone();
            require(!successors.contains_key(&predecessor))?;
            if pair.pending().bytes().is_some() {
                pending.insert(id.clone());
            }
            edges.insert(id.clone(), predecessor.clone());
            successors.insert(predecessor, id.clone());
            archives.insert(id, value);
        }
        for id in edges.keys() {
            let mut visited = BTreeSet::new();
            let mut cursor = id;
            while let Some(previous) = edges.get(cursor) {
                require(visited.insert(cursor))?;
                cursor = previous;
            }
            require(!edges.contains_key(&edges[id]))?;
        }
        Ok(Self {
            directories,
            archives,
            pending,
            edges,
            successors,
        })
    }
    pub(crate) fn source_parts(
        &self,
    ) -> Result<(&BTreeSet<String>, &BTreeMap<String, record::Archive>)> {
        self.validate_sources()?;
        require(self.pending.is_empty())?;
        Ok((&self.directories, &self.archives))
    }
    pub(crate) fn owner_source(&self, predecessor: &str, retained: &[String]) -> Result<()> {
        self.validate_sources()?;
        require(
            self.pending.is_empty()
                && self.archives.is_empty()
                && self.directories.contains(predecessor),
        )?;
        let mut expected = retained.iter().cloned().collect::<BTreeSet<_>>();
        expected.insert(predecessor.into());
        require(self.directories == expected)
    }
    fn validate_sources(&self) -> Result<()> {
        require(self.edges.values().all(|p| self.directories.contains(p)))
    }
    pub fn cleanup_order(
        &self,
        current: Option<&record::Journal>,
        prepared: Option<&record::Journal>,
        protected: &BTreeSet<String>,
        budget: usize,
    ) -> Result<Vec<String>> {
        self.validate_sources()?;
        let mut roots = protected.clone();
        for journal in [current, prepared].into_iter().flatten() {
            let j = journal.wire();
            roots.insert(j.transaction.clone());
            if let Some(p) = self.edges.get(&j.transaction) {
                require(
                    self.archives[&j.transaction].wire().authorization_plan == j.authorization_plan,
                )?;
                roots.insert(p.clone());
            }
        }
        for id in &self.pending {
            roots.insert(id.clone());
            roots.insert(self.edges[id].clone());
        }
        let mut visited = BTreeSet::new();
        let mut output = Vec::new();
        for id in &self.directories {
            if visited.contains(id) {
                continue;
            }
            let component = if let Some(p) = self.edges.get(id) {
                vec![id.clone(), p.clone()]
            } else if let Some(s) = self.successors.get(id) {
                vec![s.clone(), id.clone()]
            } else {
                vec![id.clone()]
            };
            for id in &component {
                visited.insert(id.clone());
            }
            if component.iter().any(|id| roots.contains(id))
                || output
                    .len()
                    .checked_add(component.len())
                    .is_none_or(|n| n > budget)
            {
                continue;
            }
            output.extend(component);
        }
        Ok(output)
    }
}
fn frontier_pair(store: &Store, id: &str, pair: &crate::inventory::Pair) -> Result<Frontier> {
    let decode = |bytes: &[u8]| Frontier::new(store.decode(bytes, true, true)?);
    let before = pair.published().bytes().map(decode).transpose()?;
    let after = pair.pending().bytes().map(decode).transpose()?;
    if let Some(a) = &after {
        if let Some(b) = &before {
            if pair.published().bytes() != pair.pending().bytes() {
                require(
                    a.wire().replacement == b.wire().replacement
                        && b.wire().generation.checked_add(1) == Some(a.wire().generation)
                        && a.wire().cancellation.as_ref().is_some_and(|t| {
                            t.receipt.reservation == b.reservation().unwrap_or_default()
                        }),
                )?;
            }
        } else {
            require(a.wire().generation == 1)?;
        }
    }
    let value = after.or(before).ok_or(Error::Invalid)?;
    require(value.wire().replacement.transaction == id)?;
    Ok(value)
}
impl Store {
    fn scoped_pair(&self, namespace: Namespace, id: &str) -> Result<crate::inventory::Pair> {
        let address = namespace.address(id)?;
        let published = self.read(&address)?;
        let mut pending = namespace.address(id)?;
        pending.name = format!(".{}.pending", pending.name);
        Ok(crate::inventory::Pair {
            published,
            pending: self.read(&pending)?,
        })
    }
    /// Validate the selected successor and its family, without making unrelated
    /// historical archive corruption a new prerequisite for exact continuation.
    pub fn recovery_for(&self, current: &record::Journal) -> Result<Option<Receipt>> {
        let id = &current.wire().transaction;
        if !recovery::recovery_transaction(id) {
            return Ok(None);
        }
        let namespace = if id.starts_with("recovery-v2-") {
            Namespace::LiveRecovery
        } else {
            Namespace::LegacyRecovery
        };
        let inventory = self.inventory(namespace)?;
        let mut selected = None;
        let mut predecessors = BTreeSet::new();
        for (key, pair) in inventory.records() {
            let receipt =
                Receipt::new(self.decode(pair.exact()?.ok_or(Error::Invalid)?, true, true)?)?;
            let r = receipt.wire();
            require(
                r.successor.transaction == *key
                    && predecessors.insert(r.replacement.transaction.clone())
                    && (namespace == Namespace::LiveRecovery)
                        == (r.contract == recovery::ACTIVATION_V2),
            )?;
            if key == id {
                selected = Some(receipt)
            }
        }
        let receipt = selected.ok_or(Error::Invalid)?;
        receipt.matches_successor(current.wire())?;
        let pair = self.scoped_pair(Namespace::Archive, id)?;
        if let Some(bytes) = pair.exact()? {
            let terminal = Terminal::new(self.decode(bytes, true, true)?)?;
            require(
                terminal.wire().cancellation.is_none()
                    && terminal.wire().predecessor_completed.is_none()
                    && same_receipt(&terminal.wire().receipt, receipt.wire())?
                    && (terminal.wire().completed.is_none()
                        || current.wire().checkpoint == "complete"),
            )?;
        }
        if namespace == Namespace::LiveRecovery {
            let frontier = frontier_pair(
                self,
                &receipt.wire().replacement.transaction,
                &self.scoped_pair(
                    Namespace::Frontiers,
                    &receipt.wire().replacement.transaction,
                )?,
            )?;
            require(
                frontier.wire().replacement == receipt.wire().replacement
                    && frontier.wire().generation == receipt.wire().generation
                    && frontier.reservation()? == receipt.wire().reservation,
            )?;
        }
        Ok(Some(receipt))
    }
}
pub struct State {
    inventories: BTreeMap<Namespace, Inventory>,
    ordinary: OrdinaryGraph,
    current: Option<record::Journal>,
    prepared: Option<record::Journal>,
    live: BTreeMap<String, (Namespace, Receipt)>,
    archive: BTreeMap<String, Terminal>,
    frontiers: BTreeMap<String, Frontier>,
    roots: BTreeSet<String>,
    owner_roots: BTreeSet<String>,
    binding: String,
}
fn journal(store: &Store, snapshot: &Snapshot) -> Result<Option<record::Journal>> {
    snapshot
        .bytes()
        .map(|bytes| record::Journal::new(store.decode(bytes, true, true)?))
        .transpose()
}
fn same_receipt(a: &r::RecoveryReceiptV1, b: &r::RecoveryReceiptV1) -> Result<bool> {
    Ok(codec::encode(a, true)? == codec::encode(b, true)?)
}
impl State {
    pub fn completion_witnesses(&self, current: &record::Journal) -> Result<Vec<Terminal>> {
        require(current.wire().checkpoint == "complete")?;
        let mut output = Vec::new();
        for (_, receipt) in self.live.values() {
            let r = receipt.wire();
            let mut terminal = crate::wire::lifecycle_v1::RecoveryLifecycleRecord {
                schema_version: 1,
                receipt: r.clone(),
                ..Default::default()
            };
            if receipt.matches_successor(current.wire()).is_ok() {
                terminal.completed = Some(current.wire().clone());
            } else if current.wire().transaction == r.predecessor.transaction {
                // A durable cancellation remains the closure witness even if the
                // original owner subsequently completed its own transaction.
                if let Some(old) = self.archive.get(&r.successor.transaction) {
                    if old.wire().cancellation.is_some() {
                        require(same_receipt(&old.wire().receipt, r)?)?;
                        continue;
                    }
                }
                require(output.is_empty())?;
                terminal.predecessor_completed = Some(current.wire().clone());
            } else {
                continue;
            }
            let terminal = Terminal::new(terminal)?;
            if let Some(old) = self.archive.get(&r.successor.transaction) {
                require(codec::encode(old.wire(), true)? == codec::encode(terminal.wire(), true)?)?;
            }
            output.push(terminal);
        }
        Ok(output)
    }
    pub fn read(store: &Store) -> Result<Self> {
        Self::read_inner(store, None)
    }
    fn read_inner(store: &Store, cancellation: Option<&Receipt>) -> Result<Self> {
        let owner_receipts = store.migration_owner_receipts()?;
        let ordinary = OrdinaryGraph::read(store)?;
        let current = journal(store, &store.read(&Address::journal())?)?;
        let mut prepared_address = Address::journal();
        prepared_address.name = ".journal.json.pending".into();
        let prepared_snapshot = if let Some(root) = &store.root {
            if let Some(parent) = store::directory(root, &prepared_address.parents, false)? {
                store::read_at(&parent, &prepared_address.name, true)?
            } else {
                store.read(&prepared_address)?
            }
        } else {
            store.read(&prepared_address)?
        };
        let prepared = if let Some(receipt) = cancellation {
            if let Some(bytes) = prepared_snapshot.bytes() {
                require(codec::encode(&receipt.wire().successor, true)? == bytes)?;
                require(current.as_ref().is_some_and(|j| {
                    codec::encode(j.wire(), true).ok()
                        == codec::encode(&receipt.wire().predecessor, true).ok()
                }))?;
            }
            None
        } else {
            journal(store, &prepared_snapshot)?
        };
        let mut roots = ordinary.directories.clone();
        for id in ordinary.edges.values() {
            roots.insert(id.clone());
        }
        for j in [&current, &prepared].into_iter().flatten() {
            roots.insert(j.wire().transaction.clone());
        }
        let mut inventories = BTreeMap::new();
        let mut live = BTreeMap::new();
        let mut archive = BTreeMap::new();
        let mut frontiers = BTreeMap::new();
        for namespace in [
            Namespace::LegacyRecovery,
            Namespace::LiveRecovery,
            Namespace::Archive,
            Namespace::Frontiers,
            Namespace::MigrationOwner,
            Namespace::MigrationCompleted,
        ] {
            let inventory = store.inventory(namespace)?;
            match namespace {
                Namespace::LegacyRecovery | Namespace::LiveRecovery => {
                    let mut predecessors = BTreeSet::new();
                    for (id, pair) in inventory.records() {
                        let bytes = pair.exact()?.ok_or(Error::Invalid)?;
                        let receipt = Receipt::new(store.decode(bytes, true, true)?)?;
                        let r = receipt.wire();
                        require(
                            r.successor.transaction == *id
                                && predecessors.insert(r.replacement.transaction.clone())
                                && if namespace == Namespace::LiveRecovery {
                                    r.contract == recovery::ACTIVATION_V2
                                        && id.starts_with("recovery-v2-")
                                } else {
                                    r.contract == recovery::ACTIVATION_V1
                                        && !id.starts_with("recovery-v2-")
                                },
                        )?;
                        require(!live.contains_key(id))?;
                        live.insert(id.clone(), (namespace, receipt));
                    }
                }
                Namespace::Archive => {
                    for (id, pair) in inventory.records() {
                        let terminal = Terminal::new(store.decode(
                            pair.exact()?.ok_or(Error::Invalid)?,
                            true,
                            true,
                        )?)?;
                        require(terminal.wire().receipt.successor.transaction == *id)?;
                        archive.insert(id.clone(), terminal);
                    }
                }
                Namespace::Frontiers => {
                    for (id, pair) in inventory.records() {
                        let value = frontier_pair(store, id, pair)?;
                        frontiers.insert(id.clone(), value);
                    }
                }
                _ => {}
            }
            inventories.insert(namespace, inventory);
        }
        let mut owner_roots = BTreeSet::new();
        for (id, receipt) in &owner_receipts {
            let r = receipt.wire();
            let catalog = crate::migration_owner::builtin_catalog(true, true)?;
            let registry = record::Registry::new(
                store.decode(
                    r.source_registry.as_deref().ok_or(Error::Invalid)?,
                    false,
                    false,
                )?,
                &catalog,
            )?;
            for retained in r.retained_transactions.iter().flatten() {
                let source = store.retained_terminal(retained, &registry, receipt.source())?;
                require(
                    source.releases().target == r.predecessor.releases.from
                        || r.previous_source
                            .as_ref()
                            .is_some_and(|p| p.release == source.releases().target),
                )?;
                require(
                    source
                        .steps()
                        .iter()
                        .rfind(|s| s.id == format!("{}.ledger", s.migration))
                        .is_none_or(|s| s.desired == r.projection),
                )?;
            }
            require(
                ordinary.directories.contains(&r.predecessor.transaction)
                    && !ordinary.successors.contains_key(&r.predecessor.transaction)
                    && !ordinary.edges.contains_key(id)
                    && !ordinary.edges.contains_key(&r.predecessor.transaction),
            )?;
            owner_roots.insert(id.clone());
            owner_roots.insert(r.predecessor.transaction.clone());
            owner_roots.extend(r.retained_transactions.iter().flatten().cloned());
            roots.extend(owner_roots.iter().cloned());
        }
        let mut bindings: BTreeMap<String, String> = inventories
            .iter()
            .map(|(n, i)| Ok((format!("{n:?}"), i.fingerprint()?)))
            .collect::<Result<_>>()?;
        for (kind, journal) in [("current", &current), ("prepared", &prepared)] {
            bindings.insert(
                kind.into(),
                journal
                    .as_ref()
                    .map(|j| codec::encode(j.wire(), true).map(|b| digest(&b)))
                    .transpose()?
                    .unwrap_or_default(),
            );
        }
        bindings.insert(
            "ordinary".into(),
            digest(&codec::encode(&ordinary.edges, false)?),
        );
        bindings.insert(
            "ordinary-directories".into(),
            digest(&codec::encode(&ordinary.directories, false)?),
        );
        let binding = digest(&codec::encode(&bindings, false)?);
        Ok(Self {
            inventories,
            ordinary,
            current,
            prepared,
            live,
            archive,
            frontiers,
            roots,
            owner_roots,
            binding,
        })
    }
    pub(crate) fn native_retained_transactions(&self) -> BTreeSet<String> {
        let mut roots = self.owner_roots.clone();
        for j in [&self.current, &self.prepared].into_iter().flatten() {
            roots.insert(j.wire().transaction.clone());
            if let Some(p) = self.ordinary.edges.get(&j.wire().transaction) {
                roots.insert(p.clone());
            }
        }
        for id in &self.ordinary.pending {
            roots.insert(id.clone());
            roots.insert(self.ordinary.edges[id].clone());
        }
        for (_, receipt) in self.live.values() {
            roots.insert(receipt.wire().successor.transaction.clone());
            roots.insert(receipt.wire().predecessor.transaction.clone());
            roots.insert(receipt.wire().replacement.transaction.clone());
        }
        for frontier in self.frontiers.values() {
            roots.insert(frontier.wire().replacement.transaction.clone());
            if let Some(t) = &frontier.wire().cancellation {
                roots.insert(t.receipt.successor.transaction.clone());
                roots.insert(t.receipt.predecessor.transaction.clone());
            }
        }
        // A terminal archive is historical evidence. Only an active reference
        // can make its original native invocation relevant to continuation.
        loop {
            let before = roots.len();
            for (id, terminal) in &self.archive {
                if roots.contains(id) {
                    roots.insert(terminal.wire().receipt.predecessor.transaction.clone());
                    roots.insert(terminal.wire().receipt.replacement.transaction.clone());
                }
            }
            if roots.len() == before {
                break;
            }
        }
        roots
    }
    pub(crate) fn source_terminals(&self) -> &BTreeMap<String, Terminal> {
        &self.archive
    }
    pub fn binding(&self) -> &str {
        &self.binding
    }
    pub(crate) fn ordinary_cleanup_for(&self, current: &str, budget: usize) -> Result<Vec<String>> {
        if self.current.is_none() {
            require(!self.ordinary.edges.contains_key(current))?;
        }
        let mut roots = self.owner_roots.clone();
        roots.insert(current.into());
        self.ordinary.cleanup_order(
            self.current.as_ref(),
            self.prepared.as_ref(),
            &roots,
            budget,
        )
    }
    pub fn ordinary_cleanup(&self, budget: usize) -> Result<Vec<String>> {
        self.ordinary.cleanup_order(
            self.current.as_ref(),
            self.prepared.as_ref(),
            &self.owner_roots,
            budget,
        )
    }
    pub fn reservation(
        &self,
        request: &r::ActivationOnlyRecoveryRequest,
        base: &str,
    ) -> Result<String> {
        recovery::selection(request)?;
        require(
            record::token(base, "plan-v1-")
                && !recovery::recovery_transaction(&request.transaction),
        )?;
        for (namespace, receipt) in self.live.values() {
            let r = receipt.wire();
            if r.replacement.transaction != request.transaction {
                continue;
            }
            require(*namespace == Namespace::LiveRecovery && r.replacement == *request)?;
            let frontier = self
                .frontiers
                .get(&request.transaction)
                .ok_or(Error::Invalid)?;
            let terminal = self.archive.get(&r.successor.transaction);
            if let Some(t) = terminal {
                require(
                    same_receipt(&t.wire().receipt, r)?
                        && t.wire().completed.is_none()
                        && t.wire().predecessor_completed.is_none(),
                )?;
            }
            if r.generation != frontier.wire().generation {
                require(
                    r.generation.checked_add(1) == Some(frontier.wire().generation)
                        && frontier
                            .wire()
                            .cancellation
                            .as_ref()
                            .is_some_and(|t| same_receipt(&t.receipt, r).unwrap_or(false)),
                )?;
                return frontier.reservation();
            }
            require(r.reservation == frontier.reservation()?)?;
            let cancelled = terminal.is_some_and(|t| t.wire().cancellation.is_some());
            require(r.generation != u64::MAX || (!cancelled && r.base_plan == base))?;
            return if cancelled || r.base_plan != base {
                Ok(digest(&codec::encode(r, true)?))
            } else {
                Ok(r.reservation.clone())
            };
        }
        if let Some(f) = self.frontiers.get(&request.transaction) {
            require(f.wire().replacement == *request)?;
            return f.reservation();
        }
        require(
            !self
                .archive
                .values()
                .any(|t| t.wire().receipt.replacement.transaction == request.transaction),
        )?;
        Ok(String::new())
    }
    pub fn source_closed(&self) -> Result<()> {
        for (id, (_, receipt)) in &self.live {
            let terminal = self.archive.get(id).ok_or(Error::Invalid)?;
            require(same_receipt(&terminal.wire().receipt, receipt.wire())?)?;
        }
        for frontier in self.frontiers.values() {
            let f = frontier.wire();
            let id = recovery::lifecycle_transaction(&f.replacement, f.generation)?;
            if let Some(t) = self.archive.get(&id) {
                require(
                    t.wire().receipt.replacement == f.replacement
                        && t.wire().receipt.generation == f.generation
                        && (t.wire().completed.is_some()
                            || t.wire().predecessor_completed.is_some()),
                )?;
            } else {
                let cancellation = f.cancellation.as_ref().ok_or(Error::Invalid)?;
                let archived = self
                    .archive
                    .get(&cancellation.receipt.successor.transaction)
                    .ok_or(Error::Invalid)?;
                require(
                    codec::encode(cancellation, true)? == codec::encode(archived.wire(), true)?,
                )?;
            }
        }
        Ok(())
    }
    pub fn retirement(&self) -> Result<Retirement> {
        let mut deletes = Vec::new();
        let mut live: BTreeMap<_, _> = self
            .live
            .iter()
            .map(|(id, (namespace, receipt))| (id.clone(), (*namespace, receipt.wire())))
            .collect();
        let mut remaining = 32;
        for (id, (namespace, receipt)) in &self.live {
            let r = receipt.wire();
            let Some(terminal) = self.archive.get(id) else {
                continue;
            };
            if self.roots.contains(id)
                || self.roots.contains(&r.replacement.transaction)
                || remaining == 0
            {
                continue;
            }
            require(same_receipt(&terminal.wire().receipt, r)?)?;
            if terminal.wire().cancellation.is_some() {
                let frontier = self
                    .frontiers
                    .get(&r.replacement.transaction)
                    .ok_or(Error::Invalid)?;
                require(r.generation < frontier.wire().generation)?;
            }
            let pin = digest(&codec::encode(r, true)?);
            if live.iter().any(|(other, (_, d))| {
                other != id && (d.reservation == pin || d.replacement.transaction == *id)
            }) {
                continue;
            }
            deletes.push((*namespace, id.clone()));
            live.remove(id);
            remaining -= 1;
        }
        remaining = 32;
        for (id, frontier) in &self.frontiers {
            if remaining == 0 {
                break;
            }
            let selected = recovery::lifecycle_transaction(
                &frontier.wire().replacement,
                frontier.wire().generation,
            )?;
            if self.roots.contains(&selected)
                || self.roots.contains(id)
                || live.values().any(|(_, r)| r.replacement.transaction == *id)
            {
                continue;
            }
            deletes.push((Namespace::Frontiers, id.clone()));
            remaining -= 1;
        }
        Ok(Retirement {
            binding: self.binding.clone(),
            deletes,
        })
    }
    pub fn archive_pruning(&self, budget: usize, retention: usize) -> Result<Retirement> {
        let mut roots = self.roots.clone();
        for f in self.frontiers.values() {
            if let Some(t) = &f.wire().cancellation {
                roots.insert(t.receipt.successor.transaction.clone());
            }
        }
        let mut deletes = Vec::new();
        let mut remaining = self.archive.len();
        for (id, t) in &self.archive {
            if deletes.len() >= budget || remaining <= retention {
                break;
            }
            if roots.contains(id)
                || roots.contains(&t.wire().receipt.replacement.transaction)
                || self.live.contains_key(id)
            {
                continue;
            }
            let pin = digest(&codec::encode(&t.wire().receipt, true)?);
            if self.live.values().any(|(_, r)| r.wire().reservation == pin) {
                continue;
            }
            deletes.push((Namespace::Archive, id.clone()));
            remaining -= 1;
        }
        Ok(Retirement {
            binding: self.binding.clone(),
            deletes,
        })
    }
}
#[derive(Debug, PartialEq, Eq)]
pub enum Retention {
    Captured,
    Deferred(Error),
}
pub struct Retirement {
    binding: String,
    deletes: Vec<(Namespace, String)>,
}
impl Retirement {
    pub fn selected(&self) -> &[(Namespace, String)] {
        &self.deletes
    }
}
impl crate::lease::UpdateLease<'_> {
    pub fn resolve_superseded(
        &self,
        proposed: &str,
        replacement: &crate::wire::process_v1::JournalReplacement,
        plan: &str,
    ) -> Result<String> {
        require(record::safe_id(proposed) && record::token(plan, "plan-v1-"))?;
        record::replacement(replacement)?;
        require(replacement.reason == "post-activation-scope-v0.11.1")?;
        let graph = OrdinaryGraph::read(self.store)?;
        graph.validate_sources()?;
        if let Some(id) = graph.successors.get(&replacement.transaction) {
            let archive = graph.archives[id].wire();
            require(
                archive.replacement.transaction == replacement.transaction
                    && archive.replacement.fingerprint == replacement.fingerprint
                    && archive.replacement.reason == replacement.reason
                    && archive.replacement.source_version == replacement.source_version
                    && archive.authorization_plan == plan,
            )?;
            return Ok(id.clone());
        }
        require(
            !graph.edges.contains_key(&replacement.transaction)
                && proposed != replacement.transaction
                && !graph.directories.contains(proposed)
                && graph.directories.len() < 256,
        )?;
        Ok(proposed.into())
    }
    /// Resolve data-only transaction identities after the operation boundary has
    /// established its grant. This function does not create or validate consent.
    pub fn resolve_recovery(
        &self,
        proposed: &str,
        request: &r::ActivationOnlyRecoveryRequest,
        plan: &str,
    ) -> Result<String> {
        record::require(
            record::safe_id(proposed)
                && proposed.starts_with("recovery-v1-")
                && proposed != request.transaction
                && record::token(plan, "plan-v1-"),
        )?;
        recovery::selection(request)?;
        require(!recovery::recovery_transaction(&request.transaction))?;
        let state = State::read(self.store)?;
        require(
            !state.frontiers.contains_key(&request.transaction)
                && !state.live.values().any(|(n, r)| {
                    *n == Namespace::LiveRecovery
                        && r.wire().replacement.transaction == request.transaction
                })
                && !state.archive.values().any(|t| {
                    t.wire().cancellation.is_some()
                        && t.wire().receipt.replacement.transaction == request.transaction
                }),
        )?;
        for (id, (namespace, r)) in &state.live {
            if *namespace != Namespace::LegacyRecovery {
                continue;
            }
            if r.wire().replacement.transaction == request.transaction {
                require(
                    r.wire().replacement == *request
                        && r.wire().successor.authorization_plan == plan,
                )?;
                return Ok(id.clone());
            }
            require(id != proposed)?;
        }
        self.retire(state.retirement()?)?;
        require(
            self.store
                .inventory(Namespace::LegacyRecovery)?
                .records()
                .len()
                < 256,
        )?;
        Ok(proposed.into())
    }
    pub fn resolve_lifecycle(
        &self,
        proposed: &str,
        request: &r::ActivationOnlyRecoveryRequest,
        base: &str,
        plan: &str,
        grant_digest: &str,
    ) -> Result<String> {
        require(
            record::safe_id(proposed)
                && proposed.starts_with("recovery-v2-")
                && proposed != request.transaction
                && record::fingerprint(grant_digest),
        )?;
        let current = self.store.read(&Address::journal())?;
        require(current.bytes().is_some() && current.fingerprint() == request.fingerprint)?;
        let journal = journal(self.store, &current)?.ok_or(Error::Invalid)?;
        require(journal.wire().transaction == request.transaction)?;
        let reservation = State::read(self.store)?.reservation(request, base)?;
        require(recovery::bind_lifecycle_plan(base, &reservation)? == plan)?;
        self.retire(State::read(self.store)?.retirement()?)?;
        let state = State::read(self.store)?;
        if let Some((_, receipt)) = state
            .live
            .values()
            .find(|(_, r)| r.wire().replacement.transaction == request.transaction)
        {
            let terminal = state.archive.get(&receipt.wire().successor.transaction);
            if terminal.is_none() && receipt.wire().base_plan == base {
                require(receipt.wire().successor.authorization_plan == plan)?;
                return Ok(receipt.wire().successor.transaction.clone());
            }
            require(terminal.is_none_or(|t| t.wire().cancellation.is_some()))?;
            self.cancel_recovery(receipt, base, plan, grant_digest)?;
        }
        let state = State::read(self.store)?;
        let initial = Frontier::new(crate::wire::lifecycle_v1::RecoveryFrontier {
            schema_version: 1,
            replacement: request.clone(),
            generation: 1,
            cancellation: None,
        })?;
        let frontier = state
            .frontiers
            .get(&request.transaction)
            .unwrap_or(&initial);
        require(frontier.wire().replacement == *request)?;
        self.publish_frontier(frontier)?;
        self.compact_cancellations(request)?;
        self.retire(State::read(self.store)?.archive_pruning(32, 32)?)?;
        let id = recovery::lifecycle_transaction(request, frontier.wire().generation)?;
        let state = State::read(self.store)?;
        require(
            !state.archive.contains_key(&id)
                && !state.live.contains_key(&id)
                && self
                    .store
                    .inventory(Namespace::LiveRecovery)?
                    .records()
                    .len()
                    < 512,
        )?;
        Ok(id)
    }
    pub fn compact_cancellations(&self, request: &r::ActivationOnlyRecoveryRequest) -> Result<()> {
        let state = State::read(self.store)?;
        let Some(frontier) = state.frontiers.get(&request.transaction) else {
            return Ok(());
        };
        require(frontier.wire().replacement == *request)?;
        let mut deletes = Vec::new();
        for (id, t) in &state.archive {
            let r = &t.wire().receipt;
            if deletes.len() == 32 {
                break;
            }
            if t.wire().cancellation.is_none()
                || r.replacement != *request
                || r.generation >= frontier.wire().generation.saturating_sub(1)
                || state.roots.contains(id)
                || state.live.contains_key(id)
            {
                continue;
            }
            let pin = digest(&codec::encode(r, true)?);
            if state
                .live
                .values()
                .any(|(_, r)| r.wire().reservation == pin)
            {
                continue;
            }
            deletes.push((Namespace::Archive, id.clone()));
        }
        if !deletes.is_empty() {
            self.store.fault("before-recovery-cancellation-compaction");
            self.retire(Retirement {
                binding: state.binding,
                deletes,
            })?
        }
        Ok(())
    }
    pub fn cleanup_recovery(&self, transaction: &str) -> Result<()> {
        self.store.native_inventory(None, false)?;
        require(!self.store.retirement_capture_failed.get())?;
        let current =
            journal(self.store, &self.store.read(&Address::journal())?)?.ok_or(Error::Invalid)?;
        require(
            current.wire().transaction == transaction && current.wire().checkpoint == "complete",
        )?;
        self.retire(State::read(self.store)?.retirement()?)?;
        let state = State::read(self.store)?;
        let reserve_slot = !recovery::recovery_transaction(transaction)
            || !state.archive.contains_key(transaction);
        self.retire(state.archive_pruning(32, if reserve_slot { 31 } else { 32 })?)?;
        self.capture_completion(&current)
    }
    pub fn publish_frontier(&self, frontier: &Frontier) -> Result<()> {
        // Validate the complete published/pending frontier relationship before
        // replaying a durable next generation. Never overwrite that witness.
        let inventory = self.store.inventory(Namespace::Frontiers)?;
        let id = &frontier.wire().replacement.transaction;
        let address = Namespace::Frontiers.address(id)?;
        let mut expected = self.store.read(&address)?;
        if let Some(pair) = inventory.records().get(id) {
            let before = frontier_pair(self.store, id, pair)?;
            if let Some(pending) = pair.pending().bytes() {
                self.compare_and_swap(&address, &expected, pending)?;
                expected = self.store.read(&address)?;
            }
            if codec::encode(before.wire(), true)? != codec::encode(frontier.wire(), true)? {
                require(
                    before.wire().replacement == frontier.wire().replacement
                        && before.wire().generation.checked_add(1)
                            == Some(frontier.wire().generation)
                        && frontier.wire().cancellation.as_ref().is_some_and(|t| {
                            t.receipt.reservation == before.reservation().unwrap_or_default()
                        }),
                )?;
            }
        } else {
            require(frontier.wire().generation == 1)?;
        }
        let payload = codec::encode(frontier.wire(), true)?;
        let fresh = self.store.inventory(Namespace::Frontiers)?;
        reserve(&fresh, id, &payload)?;
        if expected.bytes() == Some(payload.as_slice()) {
            let parent = store::directory(
                self.store.root.as_ref().ok_or(Error::Io)?,
                &address.parents,
                false,
            )?
            .ok_or(Error::Io)?;
            return store::io(rustix::fs::fsync(&parent));
        }
        self.store.fault("before-recovery-frontier");
        self.compare_and_swap(&address, &expected, &payload)?;
        self.store.fault("after-recovery-frontier");
        Ok(())
    }
    pub fn publish_recovery_receipt(&self, receipt: &Receipt) -> Result<()> {
        let r = receipt.wire();
        let state = State::read(self.store)?;
        let namespace = if r.contract == recovery::ACTIVATION_V2 {
            Namespace::LiveRecovery
        } else {
            Namespace::LegacyRecovery
        };
        let current = self.store.read(&Address::journal())?;
        require(
            current.bytes() == Some(codec::encode(&r.predecessor, true)?.as_slice())
                && current.fingerprint() == r.replacement.fingerprint,
        )?;
        if namespace == Namespace::LegacyRecovery {
            require(
                self.resolve_recovery(
                    &r.successor.transaction,
                    &r.replacement,
                    &r.successor.authorization_plan,
                )? == r.successor.transaction,
            )?;
        } else if !state.frontiers.contains_key(&r.replacement.transaction) {
            self.retire(state.retirement()?)?;
        }
        let state = State::read(self.store)?;
        if namespace == Namespace::LiveRecovery {
            require(state.reservation(&r.replacement, &r.base_plan)? == r.reservation)?;
            let initial = Frontier::new(crate::wire::lifecycle_v1::RecoveryFrontier {
                schema_version: 1,
                replacement: r.replacement.clone(),
                generation: 1,
                cancellation: None,
            })?;
            let frontier = state
                .frontiers
                .get(&r.replacement.transaction)
                .unwrap_or(&initial);
            require(
                frontier.wire().generation == r.generation
                    && frontier.reservation()? == r.reservation
                    && !state.archive.contains_key(&r.successor.transaction),
            )?;
            self.publish_frontier(frontier)?;
        } else {
            require(
                !state.frontiers.contains_key(&r.replacement.transaction)
                    && !state.archive.values().any(|t| {
                        t.wire().receipt.replacement.transaction == r.replacement.transaction
                            && t.wire().cancellation.is_some()
                    }),
            )?;
        }
        for (other_namespace, existing) in state.live.values() {
            if existing.wire().replacement.transaction == r.replacement.transaction {
                require(
                    *other_namespace == namespace
                        && existing.wire().successor.transaction == r.successor.transaction
                        && same_receipt(existing.wire(), r)?,
                )?;
            }
        }
        let payload = codec::encode(r, true)?;
        let inventory = self.store.inventory(namespace)?;
        reserve(&inventory, &r.successor.transaction, &payload)?;
        self.create_immutable(&namespace.address(&r.successor.transaction)?, &payload)
    }
    /// Cancel a still-unselected receipt using an already checked fresh-plan
    /// binding. This records evidence; it never issues a fresh authorization.
    pub fn cancel_recovery(
        &self,
        receipt: &Receipt,
        base: &str,
        plan: &str,
        grant_digest: &str,
    ) -> Result<()> {
        let r = receipt.wire();
        require(r.contract == recovery::ACTIVATION_V2)?;
        let state = State::read_inner(self.store, Some(receipt))?;
        require(!state.roots.contains(&r.successor.transaction))?;
        let current = self.store.read(&Address::journal())?;
        require(current.bytes() == Some(codec::encode(&r.predecessor, true)?.as_slice()))?;
        let generation = r.generation.checked_add(1).ok_or(Error::Bound)?;
        let proposed = Terminal::new(crate::wire::lifecycle_v1::RecoveryLifecycleRecord {
            schema_version: 1,
            receipt: r.clone(),
            cancellation: Some(crate::wire::lifecycle_v1::RecoveryCancellation {
                base_plan: base.into(),
                plan: plan.into(),
                authorization_digest: grant_digest.into(),
            }),
            ..Default::default()
        })?;
        let terminal = if let Some(old) = state.archive.get(&r.successor.transaction) {
            require(old.wire().cancellation.is_some() && same_receipt(&old.wire().receipt, r)?)?;
            old
        } else {
            &proposed
        };
        // Only the complete canonical unselected intended journal may be discarded;
        // no unrelated pending bytes or current revision are removed.
        let address = Address::journal();
        if let Some(parent) = store::directory(
            self.store.root.as_ref().ok_or(Error::Io)?,
            &address.parents,
            false,
        )? {
            let name = ".journal.json.pending";
            let pending = store::read_at(&parent, name, true)?;
            if let Some(bytes) = pending.bytes() {
                require(codec::encode(&r.successor, true)? == bytes)?;
                require(self.store.read(&address)? == current)?;
                store::io(rustix::fs::unlinkat(
                    &parent,
                    name,
                    rustix::fs::AtFlags::empty(),
                ))?;
                store::io(rustix::fs::fsync(&parent))?;
            }
        }
        let payload = codec::encode(terminal.wire(), true)?;
        self.store.fault("before-recovery-cancellation");
        self.compact_cancellations(&r.replacement)?;
        self.retire(State::read(self.store)?.archive_pruning(32, 32)?)?;
        reserve(
            &self.store.inventory(Namespace::Archive)?,
            &r.successor.transaction,
            &payload,
        )?;
        self.create_immutable(
            &Namespace::Archive.address(&r.successor.transaction)?,
            &payload,
        )?;
        self.store.fault("after-recovery-cancellation");
        self.publish_frontier(&Frontier::new(
            crate::wire::lifecycle_v1::RecoveryFrontier {
                schema_version: 1,
                replacement: r.replacement.clone(),
                generation,
                cancellation: Some(terminal.wire().clone()),
            },
        )?)?;
        let fresh = State::read(self.store)?;
        let pair = fresh.inventories[&Namespace::LiveRecovery]
            .records()
            .get(&r.successor.transaction);
        if let Some(pair) = pair {
            require(pair.exact()? == Some(codec::encode(r, true)?.as_slice()))?;
            self.retire(Retirement {
                binding: fresh.binding.clone(),
                deletes: vec![(Namespace::LiveRecovery, r.successor.transaction.clone())],
            })?;
        }
        Ok(())
    }
    fn capture_owner_completion(&self, current: &record::Journal) -> Result<()> {
        if current
            .wire()
            .transaction
            .starts_with("migration-owner-v1-")
        {
            self.store
                .migration_owner_for(current)?
                .ok_or(Error::Invalid)?;
            require(current.wire().checkpoint == "complete")?;
            self.create_owner_immutable(
                &Namespace::MigrationCompleted.address(&current.wire().transaction)?,
                &codec::encode(current.wire(), true)?,
            )?;
        }
        Ok(())
    }
    /// Preserve required owner completion before replacing a complete journal.
    /// Recovery retention failure is explicit, matching Go's soft retention
    /// boundary; callers must defer cleanup after that result.
    pub fn replace_journal(
        &self,
        expected: &Snapshot,
        next: &record::Journal,
    ) -> Result<Retention> {
        let current = journal(self.store, expected)?;
        require(self.store.read(&Address::journal())? == *expected)?;
        let mut retention = Retention::Captured;
        if let Some(current) = current {
            if current.wire().checkpoint == "complete"
                && current.wire().transaction != next.wire().transaction
            {
                self.capture_owner_completion(&current)?;
                if let Err(error) = self.capture_completion(&current) {
                    self.store.retirement_capture_failed.set(true);
                    retention = Retention::Deferred(error);
                }
            }
        }
        self.compare_and_swap(
            &Address::journal(),
            expected,
            &codec::encode(next.wire(), true)?,
        )?;
        Ok(retention)
    }
    pub fn capture_completion(&self, current: &record::Journal) -> Result<()> {
        require(current.wire().checkpoint == "complete")?;
        let selected = self.store.read(&Address::journal())?;
        require(selected.bytes() == Some(codec::encode(current.wire(), true)?.as_slice()))?;
        self.capture_owner_completion(current)?;
        // Completion publication inspects the complete archive even though an
        // exact selected resume intentionally ignores unrelated archive damage.
        for (id, pair) in self.store.inventory(Namespace::Archive)?.records() {
            let terminal = Terminal::new(self.store.decode(
                pair.exact()?.ok_or(Error::Invalid)?,
                true,
                true,
            )?)?;
            require(terminal.wire().receipt.successor.transaction == *id)?;
        }
        let witnesses = if let Some(receipt) = self.store.recovery_for(current)? {
            vec![Terminal::new(
                crate::wire::lifecycle_v1::RecoveryLifecycleRecord {
                    schema_version: 1,
                    receipt: receipt.wire().clone(),
                    completed: Some(current.wire().clone()),
                    ..Default::default()
                },
            )?]
        } else {
            State::read(self.store)?.completion_witnesses(current)?
        };
        for witness in witnesses {
            let namespace = Namespace::Archive;
            let id = &witness.wire().receipt.successor.transaction;
            let bytes = codec::encode(witness.wire(), true)?;
            let inventory = self.store.inventory(namespace)?;
            if reserve(&inventory, id, &bytes).is_err() {
                self.retire(State::read(self.store)?.retirement()?)?;
                self.retire(State::read(self.store)?.archive_pruning(32, 31)?)?;
            }
            reserve(&self.store.inventory(namespace)?, id, &bytes)?;
            self.store.fault("before-recovery-completion");
            self.create_immutable(&namespace.address(id)?, &bytes)?;
            self.store.fault("after-recovery-completion");
        }
        Ok(())
    }
    pub fn retire(&self, plan: Retirement) -> Result<()> {
        let state = State::read(self.store)?;
        require(state.binding == plan.binding)?;
        for (namespace, id) in plan.deletes {
            let pair = state.inventories[&namespace]
                .records()
                .get(&id)
                .ok_or(Error::Stale)?;
            if namespace != Namespace::Frontiers {
                pair.exact()?;
            }
            let parent = store::directory(
                self.store.root.as_ref().ok_or(Error::Io)?,
                &namespace.parts(),
                false,
            )?
            .ok_or(Error::Io)?;
            for (name, snapshot) in [
                (format!(".{id}.json.pending"), pair.pending()),
                (format!("{id}.json"), pair.published()),
            ] {
                let actual = store::read_at(&parent, &name, false)?;
                if actual != *snapshot {
                    return Err(Error::Stale);
                }
                if actual.bytes().is_some() {
                    store::io(rustix::fs::unlinkat(
                        &parent,
                        &name,
                        rustix::fs::AtFlags::empty(),
                    ))?;
                    self.store.fault("after-recovery-lifecycle-unlink");
                    store::io(rustix::fs::fsync(&parent))?;
                }
            }
        }
        Ok(())
    }
}
fn reserve(inventory: &Inventory, id: &str, payload: &[u8]) -> Result<()> {
    let (entries, bytes) = inventory.namespace().limits();
    if let Some(pair) = inventory.records().get(id) {
        if [pair.published().bytes(), pair.pending().bytes()].contains(&Some(payload)) {
            return Ok(());
        }
    } else {
        require(inventory.records().len() < entries)?;
    }
    if inventory
        .bytes()
        .checked_add(payload.len() as u64)
        .is_none_or(|n| n > bytes)
    {
        return Err(Error::Bound);
    }
    Ok(())
}
