//! Exact defective-owner receipt proof. Asset bindings do not replace physical asset verification.
//!
//! ```compile_fail,E0596
//! use subyard_release_owner::migration_owner::Receipt;
//! fn repin(receipt:&Receipt) { receipt.wire().protected_inputs.clear(); }
//! ```
//! ```compile_fail,E0308
//! use subyard_release_owner::{source::Retained,record::Journal};
//! fn substitute(closed:&Retained) { let _: &Journal = closed; }
//! ```
use crate::{
    codec, digest, history,
    record::{self, fingerprint, require, safe_id},
    recovery,
    wire::{ledger_v2 as l, migration_owner_v1 as m, registry_v2 as r},
    Error, Result,
};
use serde::Serialize;
pub const AFFECTED_MANIFEST: &str =
    "1db8e08c693bc9e507988576ebef7e3fb958c7c03a667e9f5a68719a49aa5ac7";
pub const AFFECTED_REGISTRY: &str =
    "e4d70df389a0eaac35bd309f864f69c5609a9e3b395e69334441187a96d49d20";
pub(crate) fn selection(request: &m::MigrationOwnerReplacementRequest) -> Result<()> {
    require(
        safe_id(&request.transaction)
            && !request.transaction.starts_with("migration-owner-v1-")
            && !recovery::recovery_transaction(&request.transaction)
            && fingerprint(&request.fingerprint),
    )
}
pub fn builtin_catalog(include_ssh: bool, include_publication: bool) -> Result<record::Catalog> {
    let mut descriptors = vec![
        r::CapabilityDescriptor {
            kind: "test-vms-settings-v1-to-v2".into(),
            domain: "settings".into(),
            version: 1,
            rollback_compatibility: "backward-compatible-no-op".into(),
            legacy_minimum_version: "0.8.0".into(),
        },
        r::CapabilityDescriptor {
            kind: "test-yard-owner-v1-to-v2".into(),
            domain: "owner-registration".into(),
            version: 1,
            rollback_compatibility: "backward-compatible-no-op".into(),
            legacy_minimum_version: "0.8.0".into(),
        },
    ];
    if include_ssh {
        descriptors.push(r::CapabilityDescriptor {
            kind: "local-ssh-port-settings-v2-to-v3".into(),
            domain: "settings".into(),
            version: 1,
            ..Default::default()
        });
    }
    if include_publication {
        descriptors.push(r::CapabilityDescriptor {
            kind: "owner-publication-state-v1".into(),
            domain: "power-metadata".into(),
            version: 1,
            ..Default::default()
        });
    }
    record::Catalog::new(Some(descriptors))
}
pub(crate) fn ledger_bytes(authority: &history::Authority) -> Result<Vec<u8>> {
    #[derive(Serialize)]
    #[serde(rename_all = "camelCase")]
    struct View<'a> {
        schema_version: i64,
        domains: &'a std::collections::BTreeMap<String, history::DomainState>,
    }
    codec::encode(
        &View {
            schema_version: 2,
            domains: authority.domains(),
        },
        true,
    )
}
fn history_proof(
    wire: &m::MigrationOwnerReceiptV1,
    b: &crate::wire::journal_v2::Record,
    reader: &crate::legacy::Reader,
) -> Result<(record::Checkpoint, record::Ledger)> {
    let raw_registry = wire.registry.as_deref().ok_or(Error::Invalid)?;
    require(digest(raw_registry) == AFFECTED_REGISTRY)?;
    let catalog = builtin_catalog(true, true)?;
    let registry = record::Registry::new(reader.decode(raw_registry, false, false)?, &catalog)?;
    let original = [(false, false), (true, false), (false, true), (true, true)]
        .into_iter()
        .map(|(s, p)| builtin_catalog(s, p))
        .collect::<Result<Vec<_>>>()?
        .into_iter()
        .find(|c| c.digest() == b.catalog_digest)
        .ok_or(Error::Invalid)?;
    for migration in registry.wire().migrations.iter().flatten() {
        require(original.supports(&migration.kind, &migration.domain))?;
    }
    let raw_projection = wire.projection_payload.as_deref().ok_or(Error::Invalid)?;
    let projection = record::Projection::from_reader(raw_projection.to_vec(), reader, true)?;
    let raw_checkpoint = wire.checkpoint_payload.as_deref().ok_or(Error::Invalid)?;
    let authority = record::Checkpoint::new(
        reader.decode(raw_checkpoint, true, true)?,
        &registry,
        AFFECTED_REGISTRY,
        &projection,
    )?;
    let projected = record::Ledger::new(reader.decode(raw_projection, true, true)?, &registry)?;
    let epochs = [
        ("owner-registration", 2),
        ("power-metadata", 1),
        ("project-state", 1),
        ("settings", 2),
    ];
    let states = projected.wire().domains.as_ref().ok_or(Error::Invalid)?;
    let compacted = authority.wire().domains.as_ref().ok_or(Error::Invalid)?;
    require(states.len() == epochs.len())?;
    for (d, e) in epochs {
        require(
            states.get(d).is_some_and(|s| s.epoch == e)
                && compacted.get(d).is_some_and(|s| s.compacted_through == e),
        )?;
    }
    registry
        .history()
        .complete(authority.ledger_view().authority())
        .map_err(|_| Error::Invalid)?;
    let steps = b.steps.as_deref().ok_or(Error::Invalid)?;
    let ids: Vec<_> = steps.iter().map(|s| s.migration.clone()).collect();
    let trace = registry
        .history()
        .trace_from(authority.ledger_view().authority(), &ids)
        .map_err(|_| Error::Invalid)?;
    require(ledger_bytes(trace.initial())? == raw_projection)?;
    let mut reconstructed = raw_projection.to_vec();
    for (index, id) in [
        "introduce-owner-publication-state-v1",
        "localize-ssh-port-settings-v3",
    ]
    .into_iter()
    .enumerate()
    {
        let step = &steps[index];
        let edge = &trace.edges()[index];
        let next = ledger_bytes(edge.after())?;
        require(
            step.migration == id
                && step.id == format!("{id}.ledger")
                && step.resource == format!("ledger.{}", edge.migration().domain)
                && step.decision == "transform"
                && step.checkpoint == "verified"
                && step.expected == digest(&reconstructed)
                && step.desired == digest(&next)
                && step
                    .evidence
                    .as_ref()
                    .is_some_and(|e| e.recovery.is_empty()),
        )?;
        reconstructed = next;
    }
    require(
        reconstructed == codec::encode(authority.ledger_view().wire(), true)?
            && wire.projection == digest(raw_projection)
            && wire.authority == digest(raw_checkpoint),
    )?;
    let raw_source = wire.source_registry.as_deref().ok_or(Error::Invalid)?;
    let source_registry =
        record::Registry::new(reader.decode(raw_source, false, false)?, &catalog)?;
    let source = record::Ledger::new(
        reader.decode::<l::LedgerV2>(raw_projection, true, true)?,
        &source_registry,
    )?;
    source_registry
        .history()
        .complete(source.authority())
        .map_err(|_| Error::Invalid)?;
    match (&b.releases.previous, &wire.previous_source) {
        (None, None) => {}
        (Some(id), Some(p)) => require(
            p.release == *id
                && p.registry == digest(raw_source)
                && [&p.artifact, &p.registry, &p.inputs]
                    .into_iter()
                    .all(|s| fingerprint(s)),
        )?,
        _ => return Err(Error::Invalid),
    };
    Ok((authority, source))
}
fn source_inputs_proof(
    store: &crate::store::Store,
    r: &m::MigrationOwnerReceiptV1,
    source_ledger: &record::Ledger,
) -> Result<()> {
    let current = store.read(&crate::store::Address::journal())?;
    require(current.bytes() == Some(codec::encode(&r.predecessor, true)?.as_slice()))?;
    require(
        store.read(&crate::store::Address::ledger())?.bytes() == r.projection_payload.as_deref(),
    )?;
    require(
        store.read(&crate::store::Address::checkpoint())?.bytes()
            == r.checkpoint_payload.as_deref(),
    )?;
    let catalog = builtin_catalog(true, true)?;
    let affected = record::Registry::new(
        store.decode(r.registry.as_deref().ok_or(Error::Invalid)?, false, false)?,
        &catalog,
    )?;
    let source = record::Registry::new(
        store.decode(
            r.source_registry.as_deref().ok_or(Error::Invalid)?,
            false,
            false,
        )?,
        &catalog,
    )?;
    let predecessor = record::Journal::new(r.predecessor.clone())?;
    store.owner_predecessor_artifacts(&predecessor, &affected)?;
    let graph = crate::graph::OrdinaryGraph::read(store)?;
    graph.owner_source(
        &r.predecessor.transaction,
        r.retained_transactions.as_deref().unwrap_or_default(),
    )?;
    for id in r.retained_transactions.iter().flatten() {
        let retained = store.retained_terminal(id, &source, source_ledger)?;
        require(
            retained.releases().target == r.predecessor.releases.from
                || r.previous_source
                    .as_ref()
                    .is_some_and(|p| p.release == retained.releases().target),
        )?;
        let last = retained
            .steps()
            .iter()
            .rfind(|s| s.id == format!("{}.ledger", s.migration));
        require(last.is_none_or(|s| s.desired == r.projection))?;
    }
    for namespace in [
        crate::inventory::Namespace::LegacyRecovery,
        crate::inventory::Namespace::LiveRecovery,
        crate::inventory::Namespace::Archive,
        crate::inventory::Namespace::Frontiers,
    ] {
        require(store.inventory(namespace)?.records().is_empty())?;
    }
    let compatibility = crate::store::Address::compatibility(&"0".repeat(64))?;
    if let Some(root) = &store.root {
        if let Some(parent) = crate::store::directory(root, &compatibility.parents, false)? {
            require(crate::inventory::names(&parent, 0)?.is_empty())?;
        }
    }
    Ok(())
}
/// Pre-consent proposal contains no successor journal and conveys no grant.
/// Only the checked physical/history closure can finish it after consent.
pub(crate) struct Proposal {
    pub(crate) wire: m::MigrationOwnerReceiptV1,
}
impl Proposal {
    pub(crate) fn new(
        mut wire: m::MigrationOwnerReceiptV1,
        store: &crate::store::Store,
    ) -> Result<Self> {
        require(wire.schema_version == 1 && wire.contract == recovery::MIGRATION_OWNER)?;
        selection(&wire.replacement)?;
        let before = record::Journal::new(wire.predecessor.clone())?;
        let b = before.wire();
        require(
            b.transaction == wire.replacement.transaction
                && digest(&codec::encode(b, true)?) == wire.replacement.fingerprint
                && b.artifact_digest == AFFECTED_MANIFEST
                && b.registry_digest == AFFECTED_REGISTRY
                && b.checkpoint == "reconciling"
                && b.goal.direction == "activate-target"
                && b.source_ingress.is_none()
                && b.steps.as_ref().is_some_and(|s| s.len() == 2)
                && wire.links.active == b.releases.target
                && wire.links.previous.as_deref() == Some(&b.releases.from),
        )?;
        crate::fixture::point("migration-owner-before-history")?;
        let (_, source) = history_proof(&wire, b, &store.reader)?;
        crate::fixture::point("migration-owner-history-shape")?;
        let graph = crate::graph::OrdinaryGraph::read(store)?;
        let (dirs, archives) = graph.source_parts()?;
        require(archives.is_empty() && dirs.contains(&b.transaction))?;
        let retained = dirs
            .iter()
            .filter(|id| **id != b.transaction)
            .cloned()
            .collect::<Vec<_>>();
        wire.retained_transactions = if retained.is_empty() {
            None
        } else {
            Some(retained)
        };
        let receipts = store.migration_owner_receipts()?;
        let binding = if let Some(prior) = receipts.values().next() {
            store.source_without_owner(prior)?.binding().to_owned()
        } else {
            store.source_before_owner()?.binding().to_owned()
        };
        wire.protected_inputs = binding.clone();
        crate::fixture::point("migration-owner-before-source-inputs")?;
        source_inputs_proof(store, &wire, &source)?;
        crate::fixture::point("migration-owner-source-inputs")?;
        let after = if let Some(prior) = receipts.values().next() {
            store.source_without_owner(prior)?
        } else {
            store.source_before_owner()?
        };
        require(after.binding() == binding)?;
        Ok(Self { wire })
    }
    pub(crate) fn finish(
        self,
        successor: crate::wire::journal_v2::Record,
        store: &crate::store::Store,
    ) -> Result<Receipt> {
        let mut wire = self.wire;
        wire.successor = successor;
        let receipt = Receipt::from_reader(wire, &store.reader)?;
        receipt.source_evidence(store)?;
        Ok(receipt)
    }
}
pub struct Receipt {
    wire: m::MigrationOwnerReceiptV1,
    authority: record::Checkpoint,
    source: record::Ledger,
}
impl Receipt {
    pub fn from_canonical_fixture(wire: m::MigrationOwnerReceiptV1) -> Result<Self> {
        Self::from_reader(wire, &crate::legacy::Reader::CanonicalFixture)
    }
    pub(crate) fn from_reader(
        wire: m::MigrationOwnerReceiptV1,
        reader: &crate::legacy::Reader,
    ) -> Result<Self> {
        require(wire.schema_version == 1 && wire.contract == recovery::MIGRATION_OWNER)?;
        selection(&wire.replacement)?;
        let before = record::Journal::new(wire.predecessor.clone())?;
        let after = record::Journal::new(wire.successor.clone())?;
        let b = before.wire();
        let a = after.wire();
        require(
            b.transaction == wire.replacement.transaction
                && digest(&codec::encode(b, true)?) == wire.replacement.fingerprint
                && b.artifact_digest == AFFECTED_MANIFEST
                && b.registry_digest == AFFECTED_REGISTRY
                && b.checkpoint == "reconciling"
                && b.goal.direction == "activate-target"
                && b.source_ingress.is_none()
                && b.steps.as_ref().is_some_and(|s| s.len() == 2),
        )?;
        recovery::links(&wire.links)?;
        require(
            wire.links.active == b.releases.target
                && wire.links.previous.as_deref() == Some(&b.releases.from)
                && a.releases.from == wire.links.active
                && a.releases.previous == wire.links.previous
                && a.checkpoint == "authorized"
                && a.source_ingress.is_none()
                && crate::wire::empty_vec(&a.steps)
                && a.goal.direction == "activate-target"
                && a.goal.target != b.goal.target
                && a.registry_digest == b.registry_digest
                && a.transaction.starts_with("migration-owner-v1-")
                && a.authorization_plan != b.authorization_plan
                && a.resume_plan != b.resume_plan
                && a.authorization_digest != b.authorization_digest,
        )?;
        require(
            [
                &wire.projection,
                &wire.authority,
                &wire.protected_inputs,
                &wire.assets,
            ]
            .into_iter()
            .all(|s| fingerprint(s)),
        )?;
        let (authority, source) = history_proof(&wire, b, reader)?;
        let retained = wire.retained_transactions.as_deref().unwrap_or_default();
        require(
            retained.len() <= 256
                && retained
                    .iter()
                    .all(|id| safe_id(id) && id != &b.transaction && id != &a.transaction)
                && retained.windows(2).all(|p| p[0] < p[1]),
        )?;
        codec::encode(&wire, true)?;
        Ok(Self {
            wire,
            authority,
            source,
        })
    }
    pub fn wire(&self) -> &m::MigrationOwnerReceiptV1 {
        &self.wire
    }
    pub fn authority(&self) -> &record::Ledger {
        self.authority.ledger_view()
    }
    pub fn source(&self) -> &record::Ledger {
        &self.source
    }
    /// Read-only local source closure. Exact sealed release assets and the
    /// prospective candidate native facts must be verified separately.
    pub fn source_evidence(&self, store: &crate::store::Store) -> Result<SourceEvidence> {
        let before = store.source_without_owner(self)?;
        require(before.binding() == self.wire.protected_inputs)?;
        source_inputs_proof(store, &self.wire, &self.source)?;
        require(before.binding() == store.source_without_owner(self)?.binding())?;
        Ok(SourceEvidence {
            binding: before.binding().into(),
            receipt: digest(&codec::encode(&self.wire, true)?),
        })
    }
    pub(crate) fn retained_evidence(&self, store: &crate::store::Store) -> Result<()> {
        let catalog = builtin_catalog(true, true)?;
        let registry = record::Registry::new(
            store.decode(
                self.wire.source_registry.as_deref().ok_or(Error::Invalid)?,
                false,
                false,
            )?,
            &catalog,
        )?;
        for id in self.wire.retained_transactions.iter().flatten() {
            let retained = store.retained_terminal(id, &registry, &self.source)?;
            require(
                retained.releases().target == self.wire.predecessor.releases.from
                    || self
                        .wire
                        .previous_source
                        .as_ref()
                        .is_some_and(|p| p.release == retained.releases().target),
            )?;
            require(
                retained
                    .steps()
                    .iter()
                    .rfind(|s| s.id == format!("{}.ledger", s.migration))
                    .is_none_or(|s| s.desired == self.wire.projection),
            )?;
        }
        Ok(())
    }
    pub fn matches_successor(&self, current: &crate::wire::journal_v2::Record) -> Result<()> {
        record::Journal::new(current.clone())?;
        let mut bound = current.clone();
        bound.checkpoint = "authorized".into();
        require(codec::encode(&bound, true)? == codec::encode(&self.wire.successor, true)?)
    }
}

/// Local source bytes/history only; this cannot be used as sealed-asset or execution authority.
pub struct SourceEvidence {
    binding: String,
    receipt: String,
}
impl SourceEvidence {
    pub fn binding(&self) -> &str {
        &self.binding
    }
    pub fn receipt_fingerprint(&self) -> &str {
        &self.receipt
    }
}

impl crate::store::Store {
    pub fn owner_receipt(&self, raw: &[u8]) -> Result<Receipt> {
        Receipt::from_reader(self.decode(raw, true, true)?, &self.reader)
    }
    // The affected owner predecessor has a narrower closed tree than ordinary
    // retained source history: no archive sidecar and no recovery payloads.
    pub(crate) fn owner_predecessor_artifacts(
        &self,
        predecessor: &record::Journal,
        registry: &record::Registry,
    ) -> Result<()> {
        let root = self.root.as_ref().ok_or(Error::Invalid)?;
        let parts = vec![
            "release-transition".into(),
            "v2".into(),
            "transactions".into(),
            predecessor.wire().transaction.clone(),
        ];
        let parent = crate::store::directory(root, &parts, false)?.ok_or(Error::Invalid)?;
        let names = crate::inventory::names(&parent, 2)?;
        require(!names.is_empty() && names.iter().all(|n| n == "evidence" || n == "recovery"))?;
        require(predecessor.wire().steps.iter().flatten().all(|step| {
            step.evidence
                .as_ref()
                .is_some_and(|e| e.recovery.is_empty())
        }))?;
        // Reuse exact canonical triple validation, after narrowing the tree.
        self.terminal_artifacts(predecessor, registry)?;
        let mut evidence = parts;
        evidence.push("evidence".into());
        require(crate::store::directory(root, &evidence, false)?.is_some())?;
        Ok(())
    }
    /// Complete durable owner namespace, shared before exclusion or continuation.
    /// Empty owner and completion namespaces are valid before initial publication.
    pub(crate) fn migration_owner_receipts(
        &self,
    ) -> Result<std::collections::BTreeMap<String, Receipt>> {
        use crate::inventory::Namespace;
        self.validate_owner_namespace()?;
        let inventory = self.inventory(Namespace::MigrationOwner)?;
        require(inventory.records().len() <= 1)?;
        let completed = self.inventory(Namespace::MigrationCompleted)?;
        require(completed.records().is_empty() || !inventory.records().is_empty())?;
        let mut receipts = std::collections::BTreeMap::new();
        for (id, pair) in inventory.records() {
            let receipt = Receipt::from_reader(
                self.decode(pair.exact()?.ok_or(Error::Invalid)?, true, true)?,
                &self.reader,
            )?;
            require(receipt.wire().successor.transaction == *id)?;
            for (complete, pair) in completed.records() {
                require(complete == id)?;
                let terminal = record::Journal::new(self.decode(
                    pair.exact()?.ok_or(Error::Invalid)?,
                    true,
                    true,
                )?)?;
                require(terminal.wire().checkpoint == "complete")?;
                receipt.matches_successor(terminal.wire())?;
            }
            receipts.insert(id.clone(), receipt);
        }
        Ok(receipts)
    }
    /// Local protected owner continuation proof; sealed runtime assets are separate.
    pub fn migration_owner_for(&self, current: &record::Journal) -> Result<Option<Receipt>> {
        let mut receipts = self.migration_owner_receipts()?;
        if !current
            .wire()
            .transaction
            .starts_with("migration-owner-v1-")
        {
            return Ok(None);
        }
        let receipt = receipts
            .remove(&current.wire().transaction)
            .ok_or(Error::Invalid)?;
        receipt.matches_successor(current.wire())?;
        let projection = self.read(&crate::store::Address::ledger())?;
        require(
            projection.bytes().is_some() && projection.fingerprint() == receipt.wire().projection,
        )?;
        let catalog = builtin_catalog(true, true)?;
        let registry = record::Registry::new(
            self.decode(
                receipt.wire().registry.as_deref().ok_or(Error::Invalid)?,
                false,
                false,
            )?,
            &catalog,
        )?;
        self.owner_predecessor_artifacts(
            &record::Journal::new(receipt.wire().predecessor.clone())?,
            &registry,
        )?;
        receipt.retained_evidence(self)?;
        Ok(Some(receipt))
    }
}
