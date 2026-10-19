//! A conditional activation keeps its original source capsule separate from the
//! one physical resolution published after the original owner work completes.
use crate::{
    codec, digest,
    installed_recipe::{PackedRecipe, Recipes},
    lease::UpdateLease,
    native::{self, Request as N},
    owner::Owner,
    record::{self, require},
    store::{Address, Store},
    wire::journal_v2 as j,
    work::Recipe,
    Error, Result,
};
use base64::{engine::general_purpose::STANDARD, Engine};
use serde::{Deserialize, Serialize};
use std::collections::BTreeSet;

const DELTA_MAX: usize = crate::prepared_payload::FILE_MAX;
pub(crate) fn owner_noop(
    before: &crate::work::OwnerBefore,
    expected: &str,
    desired: &str,
) -> Result<bool> {
    require(before.fingerprint()? == expected && before.desired().fingerprint()? == desired)?;
    Ok(expected == desired)
}

#[derive(Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub(crate) struct Capacity {
    version: u8,
    phase: String,
    metadata_bytes: usize,
    descriptor_bytes: usize,
    #[serde(skip)]
    pub sources: std::collections::BTreeMap<String, usize>,
}

impl Capacity {
    pub(crate) fn metadata_bytes(&self) -> usize {
        self.metadata_bytes
    }
    pub(crate) fn checked(raw: &[u8]) -> Result<Self> {
        let value: Self = codec::decode_canonical(raw, false)?;
        require(
            value.version == 1
                && value.phase == "template"
                && (raw.len()..=DELTA_MAX).contains(&value.metadata_bytes)
                && (1..=DELTA_MAX).contains(&value.descriptor_bytes),
        )?;
        Ok(value)
    }
}
#[derive(Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct PhysicalDelta {
    instance_uuid: String,
    state: String,
    actual: String,
    desired: String,
    reported_actual: String,
    reported_desired: String,
    before: String,
    scope_state: String,
}
impl PhysicalDelta {
    #[cfg(test)]
    fn from_target(t: &native::Target) -> Self {
        Self {
            instance_uuid: t.instance_uuid.clone(),
            state: t.state.clone(),
            actual: t.actual.clone(),
            desired: t.desired.clone(),
            reported_actual: t.reported_actual.clone(),
            reported_desired: t.reported_desired.clone(),
            before: t.before.clone(),
            scope_state: t.scope_state.clone(),
        }
    }
    fn apply(&self, original: &native::Target, recipe: &str) -> native::Target {
        let mut target = original.clone();
        target.instance_uuid = self.instance_uuid.clone();
        target.state = self.state.clone();
        target.actual = self.actual.clone();
        target.desired = self.desired.clone();
        target.reported_actual = self.reported_actual.clone();
        target.reported_desired = self.reported_desired.clone();
        target.before = self.before.clone();
        target.scope_state = self.scope_state.clone();
        target.prepared_recipe = recipe.into();
        target
    }
}
#[derive(Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct PhysicalMetadata {
    version: u8,
    phase: String,
    original: String,
    producer: native::Binding,
    producer_terminal: String,
    delta: PhysicalDelta,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    physical: String,
}

impl PhysicalMetadata {
    fn checked(raw: &[u8]) -> Result<Self> {
        require(!raw.is_empty() && raw.len() <= crate::prepared_payload::FILE_MAX)?;
        let value: Self = serde_json::from_slice(raw).map_err(|_| Error::Codec)?;
        require(codec::encode_native_recipe(&value)? == raw)?;
        Ok(value)
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum AfterOwnerActivation {
    Runtime,
    MaterializedFiles,
    MaterializedIntegrations,
    Broker,
    Routes,
}
impl AfterOwnerActivation {
    pub(crate) fn parse(value: &str) -> Result<(Self, &str)> {
        let (kind, hash) = value
            .strip_prefix("v1:")
            .ok_or(Error::Invalid)?
            .split_once(':')
            .ok_or(Error::Invalid)?;
        require(record::fingerprint(hash))?;
        let kind = match kind {
            "runtime" => Self::Runtime,
            "materialized-files" => Self::MaterializedFiles,
            "materialized-integrations" => Self::MaterializedIntegrations,
            "broker" => Self::Broker,
            "routes" => Self::Routes,
            _ => return Err(Error::Invalid),
        };
        Ok((kind, hash))
    }
}

/// Source and logical scope remain the approved contract. Only the explicitly
/// conditional physical observation can be filled by its owning component.
pub(crate) fn physical_target(original: &native::Target, resolved: &native::Target) -> Result<()> {
    original.validate()?;
    resolved.validate()?;
    let (_, conditional) = AfterOwnerActivation::parse(&original.after_owner)?;
    require(conditional == original.prepared_recipe)?;
    require(record::fingerprint(&resolved.prepared_recipe))?;
    let mut allowed = original.clone();
    if original.instance_uuid == "absent" {
        require(resolved.instance_uuid != "absent" && record::safe_id(&resolved.instance_uuid))?;
        allowed.instance_uuid = resolved.instance_uuid.clone();
    }
    if original.state == "stopped" {
        require(matches!(resolved.state.as_str(), "stopped" | "deferred"))?;
    }
    allowed.prepared_recipe = resolved.prepared_recipe.clone();
    allowed.actual = resolved.actual.clone();
    allowed.desired = resolved.desired.clone();
    allowed.before = resolved.before.clone();
    allowed.reported_actual = resolved.reported_actual.clone();
    allowed.reported_desired = resolved.reported_desired.clone();
    allowed.scope_state = resolved.scope_state.clone();
    allowed.state = resolved.state.clone();
    require(allowed == *resolved)
}

/// This is resource/observation provenance, never a native terminal witness.
#[derive(Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub(crate) struct Resolution {
    version: u8,
    pub binding: native::Binding,
    pub producer: native::Binding,
    pub owner_ordinal: u64,
    pub producer_terminal: String,
    original: String,
    conditional: String,
    descriptor: String,
    resolved: String,
    recipe: String,
    pub owner: String,
    pub resource: String,
    pub expected: String,
    pub desired: String,
    pub source: String,
    pub request: String,
}
pub(crate) fn address(binding: &native::Binding) -> Result<Address> {
    binding.validate()?;
    Ok(Address::at(
        vec![
            "migrations".into(),
            "native-custody".into(),
            "v1".into(),
            binding.transaction.clone(),
        ],
        format!("{:03}.after-owner.json", binding.ordinal),
        true,
    ))
}
impl Resolution {
    pub(crate) fn checked(raw: &[u8]) -> Result<Self> {
        require(raw.len() <= 4096)?;
        let value: Self = codec::decode_canonical(raw, true)?;
        value.binding.validate()?;
        value.producer.validate()?;
        let mut same = value.producer.clone();
        same.ordinal = value.binding.ordinal;
        require(
            value.version == 1
                && same == value.binding
                && value.binding.ordinal <= 2048
                && value.owner_ordinal > 0
                && value.owner_ordinal < value.binding.ordinal
                && value.binding.ordinal != value.producer.ordinal
                && record::safe_id(&value.owner)
                && record::safe_name(&value.resource)
                && [
                    &value.producer_terminal,
                    &value.original,
                    &value.conditional,
                    &value.descriptor,
                    &value.resolved,
                    &value.expected,
                    &value.desired,
                    &value.source,
                    &value.request,
                ]
                .iter()
                .all(|h| record::fingerprint(h))
                && record::fingerprint(&value.recipe),
        )?;
        Ok(value)
    }
    fn load(
        &self,
        store: &Store,
        original: &native::Target,
        recipe: &PackedRecipe,
    ) -> Result<(native::Target, Recipes)> {
        let (_, conditional) = AfterOwnerActivation::parse(&original.after_owner)?;
        require(
            self.original == digest(&codec::encode(original, false)?)
                && self.conditional == conditional
                && original.prepared_recipe == conditional,
        )?;
        let raw = store.prepared_payload(&self.descriptor)?;
        let capacity = recipe.after_owner_capacity(store)?;
        require(raw.len() <= capacity.descriptor_bytes)?;
        let metadata = PhysicalMetadata::checked(&raw)?;
        if !metadata.physical.is_empty() {
            let bytes = STANDARD
                .decode(&metadata.physical)
                .map_err(|_| Error::Codec)?;
            require(!bytes.is_empty() && STANDARD.encode(bytes) == metadata.physical)?;
        }
        require(
            raw.len() <= capacity.metadata_bytes
                && metadata.version == 1
                && metadata.phase == "resolved"
                && metadata.original == self.original
                && metadata.producer == self.producer
                && metadata.producer_terminal == self.producer_terminal,
        )?;
        let target = metadata.delta.apply(original, &self.recipe);
        physical_target(original, &target)?;
        require(
            self.resolved == digest(&codec::encode(&target, false)?)
                && self.owner == target.owner
                && self.resource == target.yard
                && self.expected == target.actual
                && self.desired == target.desired
                && self.source == target.source
                && self.request
                    == N::ApplyActivation {
                        binding: self.binding.clone(),
                        target: target.clone(),
                    }
                    .recipe_fingerprint()?,
        )?;
        let packed = recipe.with_after_owner_metadata(&self.descriptor)?;
        require(packed.fingerprint(store)? == self.recipe)?;
        let recipes = Recipes::from([(self.recipe.clone(), packed)]);
        Ok((target, recipes))
    }
}
impl Store {
    pub(crate) fn no_owner_invocation(&self, binding: &native::Binding) -> Result<bool> {
        binding.validate()?;
        for kind in ["intent", "accepted", "terminal"] {
            let a = Address::native(&binding.transaction, binding.ordinal, kind)?;
            if self.read(&a)?.bytes().is_some()
                || self
                    .read(&Address::at(
                        a.parents.clone(),
                        format!(".{}.pending", a.name),
                        true,
                    ))?
                    .bytes()
                    .is_some()
            {
                return Ok(false);
            }
        }
        self.validate_owner_producer(binding)?;
        Ok(true)
    }
    pub(crate) fn after_owner_capacity(&self, extra: Vec<Capacity>) -> Result<()> {
        let mut values = self.pending_after_owner_capacity()?;
        values.extend(extra);
        if values.is_empty() {
            return Ok(());
        }
        let usage = self.payload_usage()?;
        let (files, bytes, _) = self.native_usage(None)?;
        let mut raw = usage.raw;
        let mut pieces = usage.pieces;
        let mut metadata = usage.metadata;
        let mut roots = usage.roots;
        let count = values.len();
        let mut sources = std::collections::BTreeMap::new();
        for value in values {
            sources.extend(value.sources);
            raw = raw
                .checked_add(value.metadata_bytes.min(value.descriptor_bytes))
                .ok_or(Error::Bound)?;
            let p = value
                .metadata_bytes
                .min(value.descriptor_bytes)
                .div_ceil(crate::prepared_payload::CHUNK);
            pieces = pieces.checked_add(p).ok_or(Error::Bound)?;
            metadata = metadata.checked_add(256 + 70 * p).ok_or(Error::Bound)?;
            roots += 1;
        }
        for (hash, length) in sources {
            if self.has_prepared_payload(&hash)? {
                continue;
            }
            let p = length.div_ceil(crate::prepared_payload::CHUNK);
            raw = raw.checked_add(length).ok_or(Error::Bound)?;
            pieces += p;
            roots += 1;
            metadata = metadata.checked_add(256 + 70 * p).ok_or(Error::Bound)?;
        }
        require(
            raw <= 128 << 20
                && pieces <= 16384
                && roots <= 8192
                && metadata <= 8 << 20
                && files.checked_add(count).is_some_and(|n| n <= 8192)
                && bytes
                    .checked_add(metadata - usage.metadata)
                    .and_then(|n| n.checked_add(count * 4096))
                    .is_some_and(|n| n <= 128 << 20),
        )
    }
    pub(crate) fn after_owner_references(
        &self,
        tx: &str,
        names: &[String],
    ) -> Result<BTreeSet<String>> {
        let mut refs = BTreeSet::new();
        for name in names.iter().filter(|n| n.ends_with(".after-owner.json")) {
            let ordinal = name
                .split_once('.')
                .ok_or(Error::Protected)?
                .0
                .parse::<u64>()
                .map_err(|_| Error::Protected)?;
            let a = Address::at(
                vec![
                    "migrations".into(),
                    "native-custody".into(),
                    "v1".into(),
                    tx.into(),
                ],
                name.clone(),
                true,
            );
            let snap = self.read(&a)?;
            let fact = Resolution::checked(snap.bytes().ok_or(Error::Unknown)?)?;
            require(
                fact.binding.transaction == tx
                    && fact.binding.ordinal == ordinal
                    && a.name == address(&fact.binding)?.name,
            )?;
            let (target, recipe) = self.after_owner_original(&fact.binding, fact.owner_ordinal)?;
            let (_, recipes) = fact.load(self, &target, &recipe)?;
            refs.insert(fact.descriptor);
            refs.extend(crate::prepared_payload::references(recipes.values()));
        }
        Ok(refs)
    }
}

impl Owner {
    pub(crate) fn after_owner_noop(
        &self,
        journal: &j::Record,
        bundle: &crate::bundle::Bundle,
    ) -> Result<bool> {
        let owners = bundle
            .work
            .iter()
            .enumerate()
            .filter_map(|(n, w)| {
                if let Recipe::TestYardOwnerRegistration(before) = &w.recipe {
                    Some((n, w, before))
                } else {
                    None
                }
            })
            .collect::<Vec<_>>();
        require(owners.len() == 1)?;
        let (n, work, before) = owners[0];
        Ok(
            owner_noop(before, &work.intent.expected, &work.intent.desired)?
                && self
                    .store
                    .no_owner_invocation(&self.binding(journal, n as u64 + 1))?,
        )
    }
    pub(crate) fn after_owner_preflight(
        &self,
        targets: &[native::Target],
        work: &[crate::work::Work],
    ) -> Result<()> {
        if !targets.iter().any(|t| !t.after_owner.is_empty()) || work.is_empty() {
            return Ok(());
        }
        let owners = work
            .iter()
            .filter_map(|w| {
                if let Recipe::TestYardOwnerRegistration(before) = &w.recipe {
                    Some((w, before))
                } else {
                    None
                }
            })
            .collect::<Vec<_>>();
        require(owners.len() == 1)?;
        if owner_noop(
            owners[0].1,
            &owners[0].0.intent.expected,
            &owners[0].0.intent.desired,
        )? {
            for target in targets.iter().filter(|t| !t.after_owner.is_empty()) {
                require(
                    target.state != "stale"
                        && AfterOwnerActivation::parse(&target.after_owner)?.1
                            == target.prepared_recipe,
                )?;
            }
            return Ok(());
        }
        require(!owners[0].1.prepared_recipe.is_empty())?;
        let mut capacities = vec![];
        for target in targets.iter().filter(|t| !t.after_owner.is_empty()) {
            let (_, hash) = AfterOwnerActivation::parse(&target.after_owner)?;
            require(hash == target.prepared_recipe)?;
            capacities.push(self.after_owner_recipe_capacity(hash)?);
        }
        self.store.after_owner_capacity(capacities)
    }
    /// Read-only reconstruction installs the recorded physical observation view;
    /// it never invokes the resolver or publishes a replacement baseline.
    pub(crate) fn restore_after_owner(
        &self,
        journal: &j::Record,
        bundle: &crate::bundle::Bundle,
    ) -> Result<()> {
        for (index, original) in bundle
            .targets
            .iter()
            .enumerate()
            .filter(|(_, t)| !t.after_owner.is_empty())
        {
            let binding = self.binding(journal, bundle.work.len() as u64 + index as u64 + 1);
            let snap = self.store.read(&address(&binding)?)?;
            if let Some(raw) = snap.bytes() {
                let fact = Resolution::checked(raw)?;
                let (owner_ordinal, producer) = self.completed_owner_producer(journal, bundle)?;
                let terminal = self.store.read(&Address::native(
                    &journal.transaction,
                    producer.ordinal,
                    "terminal",
                )?)?;
                require(
                    fact.binding == binding
                        && fact.producer == producer
                        && fact.owner_ordinal == owner_ordinal
                        && fact.producer_terminal
                            == digest(terminal.bytes().ok_or(Error::Unknown)?),
                )?;
                let capsule = bundle
                    .installed
                    .get(&original.prepared_recipe)
                    .ok_or(Error::Protected)?;
                let (target, recipes) = fact.load(&self.store, original, capsule)?;
                self.bind_installed_recipe_target(
                    &original.prepared_recipe,
                    &bundle.installed,
                    Some(original),
                )?;
                self.bind_installed_recipe_target(
                    &target.prepared_recipe,
                    &recipes,
                    Some(&target),
                )?;
            } else {
                require(
                    (journal.checkpoint != "complete" || self.after_owner_noop(journal, bundle)?)
                        && self
                            .store
                            .read(&Address::native(
                                &journal.transaction,
                                binding.ordinal,
                                "intent",
                            )?)?
                            .bytes()
                            .is_none(),
                )?;
            }
        }
        Ok(())
    }
    pub(crate) fn after_owner_targets(
        &self,
        lease: &UpdateLease<'_>,
        journal: &j::Record,
        bundle: &crate::bundle::Bundle,
    ) -> Result<Vec<native::Target>> {
        let mut targets = bundle.targets.clone();
        if targets.iter().any(|t| !t.after_owner.is_empty()) {
            if self.after_owner_noop(journal, bundle)? {
                return Ok(targets);
            }
            require(self.restore_owner_results(journal, bundle, None)?)?;
            for (index, target) in bundle
                .targets
                .iter()
                .enumerate()
                .filter(|(_, t)| !t.after_owner.is_empty())
            {
                targets[index] = self
                    .resolve_after_owner(
                        lease,
                        journal,
                        bundle,
                        bundle.work.len() as u64 + index as u64 + 1,
                        target,
                    )?
                    .0;
            }
        }
        Ok(targets)
    }
    fn completed_owner_producer(
        &self,
        journal: &j::Record,
        bundle: &crate::bundle::Bundle,
    ) -> Result<(u64, native::Binding)> {
        let owners = bundle
            .work
            .iter()
            .enumerate()
            .filter_map(|(n, work)| {
                if let Recipe::TestYardOwnerRegistration(before) = &work.recipe {
                    Some((n, work, before))
                } else {
                    None
                }
            })
            .collect::<Vec<_>>();
        require(owners.len() == 1)?;
        let (index, work, before) = owners[0];
        require(!before.prepared_recipe.is_empty() && work.intent.expected != work.intent.desired)?;
        let step = journal
            .steps
            .as_ref()
            .and_then(|s| s.get(index))
            .ok_or(Error::Unknown)?;
        require(step.checkpoint == "verified")?;
        let token = self.store.owner_progress_token(journal, step, before)?;
        let base = self.binding(journal, index as u64 + 1);
        let request = N::ApplyOwner {
            binding: base.clone(),
            before: before.clone(),
            token,
        };
        let producers = self.store.owner_result_producers(
            &base,
            &request,
            &work.intent.expected,
            &work.intent.desired,
            &before.controller,
        )?;
        let producer = producers.last().ok_or(Error::Unknown)?.clone();
        let terminal = self.store.read(&Address::native(
            &journal.transaction,
            producer.ordinal,
            "terminal",
        )?)?;
        let t: native::Terminal =
            codec::decode_canonical(terminal.bytes().ok_or(Error::Unknown)?, true)?;
        require(
            t.binding == producer
                && t.state == "complete"
                && t.owner == "owner-registration"
                && t.resource == "test-yard",
        )?;
        Ok((base.ordinal, producer))
    }
    pub(crate) fn resolve_after_owner(
        &self,
        lease: &UpdateLease<'_>,
        journal: &j::Record,
        bundle: &crate::bundle::Bundle,
        ordinal: u64,
        original: &native::Target,
    ) -> Result<(native::Target, Recipes)> {
        let (_, conditional) = AfterOwnerActivation::parse(&original.after_owner)?;
        require(conditional == original.prepared_recipe)?;
        let binding = self.binding(journal, ordinal);
        let (owner_ordinal, producer) = self.completed_owner_producer(journal, bundle)?;
        let producer_snap = self.store.read(&Address::native(
            &journal.transaction,
            producer.ordinal,
            "terminal",
        )?)?;
        let producer_hash = digest(producer_snap.bytes().ok_or(Error::Unknown)?);
        let snapshot = self.store.read(&address(&binding)?)?;
        let capsule = bundle.installed.get(conditional).ok_or(Error::Unknown)?;
        self.bind_installed_recipe_target(conditional, &bundle.installed, Some(original))?;
        let fact = if let Some(raw) = snapshot.bytes() {
            let fact = Resolution::checked(raw)?;
            require(
                fact.binding == binding
                    && fact.producer == producer
                    && fact.owner_ordinal == owner_ordinal
                    && fact.producer_terminal == producer_hash,
            )?;
            fact
        } else {
            // Never choose a new baseline after an intent, even if acceptance
            // or the reply was lost. The original immutable resolution is required.
            require(
                journal.checkpoint != "complete"
                    && self
                        .store
                        .read(&Address::native(&journal.transaction, ordinal, "intent")?)?
                        .bytes()
                        .is_none(),
            )?;
            self.native_quiescent(Some(journal))?;
            let a = address(&binding)?;
            require(
                self.store
                    .read(&Address::at(
                        a.parents.clone(),
                        format!(".{}.pending", a.name),
                        true,
                    ))?
                    .bytes()
                    .is_none(),
            )?;
            lease.cleanup_prepared_payloads()?;
            self.store.after_owner_capacity(vec![])?;
            let (files, bytes, _) = self.store.native_usage(None)?;
            require(files < 8192 && bytes.checked_add(4096).is_some_and(|n| n <= 128 << 20))?;
            let reply = self.call(N::ResolveAfterOwner {
                session: self.session.clone(),
                binding: None,
                target: original.clone(),
            })?;
            let target = reply.targets.into_iter().next().ok_or(Error::Invalid)?;
            physical_target(original, &target)?;
            let packed =
                self.capture_after_owner_recipe(lease, &target.prepared_recipe, capsule)?;
            let descriptor = packed.after_owner_metadata(capsule, &self.store)?;
            let raw = self.store.prepared_payload(&descriptor)?;
            let capacity = capsule.after_owner_capacity(&self.store)?;
            require(
                raw.len() <= capacity.descriptor_bytes && raw.len() <= capacity.metadata_bytes,
            )?;
            let fact = Resolution {
                version: 1,
                binding: binding.clone(),
                producer,
                owner_ordinal,
                producer_terminal: producer_hash,
                original: digest(&codec::encode(original, false)?),
                conditional: conditional.into(),
                descriptor,
                resolved: digest(&codec::encode(&target, false)?),
                recipe: target.prepared_recipe.clone(),
                owner: target.owner.clone(),
                resource: target.yard.clone(),
                expected: target.actual.clone(),
                desired: target.desired.clone(),
                source: target.source.clone(),
                request: N::ApplyActivation {
                    binding: binding.clone(),
                    target,
                }
                .recipe_fingerprint()?,
            };
            let raw = codec::encode(&fact, true)?;
            Resolution::checked(&raw)?;
            fact.load(&self.store, original, capsule)?;
            crate::fixture::point("before-after-owner-resolution")?;
            lease.create_immutable(&address(&binding)?, &raw)?;
            crate::fixture::point("after-after-owner-resolution")?;
            fact
        };
        let (target, recipes) = fact.load(&self.store, original, capsule)?;
        self.bind_installed_recipe_target(&target.prepared_recipe, &recipes, Some(&target))?;
        Ok((target, recipes))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::custody::{
        self,
        tests::{created_owner, fixture, write},
    };
    use std::os::unix::fs::PermissionsExt;

    fn target() -> native::Target {
        native::Target {
            prepared_recipe: "a".repeat(64),
            after_owner: format!("v1:runtime:{}", "a".repeat(64)),
            scope: "scope".into(),
            reported_actual: String::new(),
            reported_desired: String::new(),
            before: String::new(),
            class: "profile-runtime".into(),
            scope_state: String::new(),
            stage: String::new(),
            owner: "synthetic-runtime".into(),
            yard: "yard".into(),
            project: "project".into(),
            instance: "instance".into(),
            instance_uuid: "absent".into(),
            state: "deferred".into(),
            actual: "b".repeat(64),
            desired: "c".repeat(64),
            source: "d".repeat(64),
        }
    }
    fn capacity() -> Capacity {
        Capacity {
            version: 1,
            phase: "template".into(),
            metadata_bytes: 4096,
            descriptor_bytes: 4096,
            sources: Default::default(),
        }
    }
    fn capsule(store: &Store) -> (native::Target, PackedRecipe) {
        let lease = store.lock().unwrap();
        let metadata = lease
            .publish_prepared_payload(&codec::encode(&capacity(), false).unwrap())
            .unwrap();
        let source = lease
            .publish_prepared_payload(b"original executable bytes\n")
            .unwrap();
        let packed: PackedRecipe = serde_json::from_value(serde_json::json!({
            "version":1,"owner":"synthetic-runtime","source":"a".repeat(64),"runtime":"b".repeat(64),
            "resources":"c".repeat(64),"launcher":"owner.sh","hookBefore":"","leaves":["owner.sh"],
            "files":[{"path":"owner.sh","content":source}],"afterOwner":metadata
        })).unwrap();
        let hash = packed.fingerprint(store).unwrap();
        let mut original = target();
        original.after_owner = format!("v1:runtime:{hash}");
        original.prepared_recipe = hash;
        (original, packed)
    }
    fn resolution(
        store: &Store,
        producer: &native::Binding,
        original: &native::Target,
        capsule: &PackedRecipe,
    ) -> Resolution {
        let mut binding = producer.clone();
        binding.ordinal = 2;
        let mut target = original.clone();
        target.instance_uuid = "original-created-uuid".into();
        target.state = "stale".into();
        target.actual = "e".repeat(64);
        target.desired = "f".repeat(64);
        target.before = "original-native-baseline".into();
        let terminal = store
            .read(&Address::native(&producer.transaction, producer.ordinal, "terminal").unwrap())
            .unwrap();
        let producer_terminal = digest(terminal.bytes().unwrap());
        let raw = codec::encode(
            &PhysicalMetadata {
                version: 1,
                phase: "resolved".into(),
                original: digest(&codec::encode(original, false).unwrap()),
                producer: producer.clone(),
                producer_terminal: producer_terminal.clone(),
                delta: PhysicalDelta::from_target(&target),
                physical: String::new(),
            },
            false,
        )
        .unwrap();
        let descriptor = store
            .lock()
            .unwrap()
            .publish_prepared_payload(&raw)
            .unwrap();
        target.prepared_recipe = capsule
            .with_after_owner_metadata(&descriptor)
            .unwrap()
            .fingerprint(store)
            .unwrap();
        Resolution {
            version: 1,
            binding: binding.clone(),
            producer: producer.clone(),
            owner_ordinal: 1,
            producer_terminal,
            original: digest(&codec::encode(original, false).unwrap()),
            conditional: original.prepared_recipe.clone(),
            descriptor,
            resolved: digest(&codec::encode(&target, false).unwrap()),
            recipe: target.prepared_recipe.clone(),
            owner: target.owner.clone(),
            resource: target.yard.clone(),
            expected: target.actual.clone(),
            desired: target.desired.clone(),
            source: target.source.clone(),
            request: N::ApplyActivation { binding, target }
                .recipe_fingerprint()
                .unwrap(),
        }
    }
    fn files(root: &std::path::Path, tx: &str) -> Vec<Result<(String, Vec<u8>)>> {
        std::fs::read_dir(root.join("migrations/native-custody/v1").join(tx))
            .unwrap()
            .map(|entry| {
                let entry = entry.unwrap();
                Ok((
                    entry.file_name().into_string().unwrap(),
                    std::fs::read(entry.path()).unwrap(),
                ))
            })
            .collect()
    }
    #[test]
    fn closed_kinds_and_physical_projection_preserve_original_authority() {
        for kind in [
            "runtime",
            "materialized-files",
            "materialized-integrations",
            "broker",
            "routes",
        ] {
            assert!(AfterOwnerActivation::parse(&format!("v1:{kind}:{}", "a".repeat(64))).is_ok());
        }
        for value in [
            "",
            "v2:runtime:a",
            "v1:runtime:ABC",
            "v1:host-power:a",
            "v1:runtime:a:extra",
        ] {
            assert!(AfterOwnerActivation::parse(value).is_err());
        }
        let original = target();
        let mut physical = original.clone();
        physical.instance_uuid = "original-created-uuid".into();
        physical.desired = "e".repeat(64);
        physical.state = "stale".into();
        assert!(physical_target(&original, &physical).is_ok());
        for field in [
            "source",
            "scope",
            "owner",
            "project",
            "instance",
            "class",
            "afterOwner",
        ] {
            let mut raw = serde_json::to_value(&physical).unwrap();
            raw[field] = serde_json::Value::String(if field == "source" {
                "f".repeat(64)
            } else {
                "different".into()
            });
            let changed: native::Target = serde_json::from_value(raw).unwrap();
            assert!(physical_target(&original, &changed).is_err(), "{field}");
        }
        for uuid in [
            String::new(),
            "absent".into(),
            "-invalid".into(),
            "a".repeat(129),
        ] {
            physical.instance_uuid = uuid;
            assert!(physical_target(&original, &physical).is_err());
        }
        let mut existing = original.clone();
        existing.instance_uuid = "original-real-id".into();
        physical.instance_uuid = "replacement-id".into();
        assert!(physical_target(&existing, &physical).is_err());
        existing.state = "stopped".into();
        physical.instance_uuid = existing.instance_uuid.clone();
        assert!(physical_target(&existing, &physical).is_err());
        physical.state = "deferred".into();
        assert!(physical_target(&existing, &physical).is_ok());
        let mut ordinary = target();
        ordinary.after_owner.clear();
        assert!(!String::from_utf8(codec::encode(&ordinary, false).unwrap())
            .unwrap()
            .contains("afterOwner"));
    }
    #[test]
    fn immutable_delta_reopens_with_original_files_and_one_measured_desired() {
        let (f, store, _, producer) = created_owner();
        let (original, capsule) = capsule(&store);
        let fact = resolution(&store, &producer, &original, &capsule);
        let raw = codec::encode(&fact, true).unwrap();
        let lease = store.lock().unwrap();
        lease
            .create_immutable(&address(&fact.binding).unwrap(), &raw)
            .unwrap();
        lease
            .create_immutable(&address(&fact.binding).unwrap(), &raw)
            .unwrap();
        let mut changed: serde_json::Value = serde_json::from_slice(&raw).unwrap();
        changed["resolved"] = serde_json::Value::String("b".repeat(64));
        assert!(lease
            .create_immutable(
                &address(&fact.binding).unwrap(),
                &codec::encode(&changed, true).unwrap()
            )
            .is_err());
        drop(lease);
        let fresh = Store::open_canonical_fixture(&f.0).unwrap();
        let persisted = fresh.read(&address(&fact.binding).unwrap()).unwrap();
        let checked = Resolution::checked(persisted.bytes().unwrap()).unwrap();
        let (physical, recipes) = checked.load(&fresh, &original, &capsule).unwrap();
        assert_eq!(physical.desired, "f".repeat(64));
        assert_eq!(physical.source, original.source);
        assert_eq!(
            recipes[&physical.prepared_recipe]
                .after_owner_metadata(&capsule, &fresh)
                .unwrap(),
            fact.descriptor
        );
        assert_eq!(
            capsule.fingerprint(&fresh).unwrap(),
            original.prepared_recipe
        );
        let mut foreign = original.clone();
        foreign.source = "e".repeat(64);
        assert!(checked.load(&fresh, &foreign, &capsule).is_err());
        for field in [
            "producerTerminal",
            "original",
            "conditional",
            "descriptor",
            "resolved",
            "recipe",
            "request",
        ] {
            let mut value = serde_json::to_value(&checked).unwrap();
            value[field] = serde_json::Value::String("a".repeat(64));
            let bad: Resolution = serde_json::from_value(value).unwrap();
            // Producer proof is additionally checked by custody; all other
            // fields are checked against the retained descriptor/capsule here.
            assert!(bad.load(&fresh, &original, &capsule).is_err(), "{field}");
        }
    }
    #[test]
    fn metadata_only_replacement_refuses_source_header_and_scope_injection() {
        let (_f, store, _, producer) = created_owner();
        let (original, capsule) = capsule(&store);
        let fact = resolution(&store, &producer, &original, &capsule);
        let packed = capsule.with_after_owner_metadata(&fact.descriptor).unwrap();
        for field in [
            "owner",
            "source",
            "runtime",
            "resources",
            "launcher",
            "handler",
        ] {
            let mut value = serde_json::to_value(&packed).unwrap();
            value[field] = serde_json::Value::String("e".repeat(64));
            let changed: PackedRecipe = serde_json::from_value(value).unwrap();
            assert!(
                changed.after_owner_metadata(&capsule, &store).is_err(),
                "{field}"
            );
        }
        let mut value = serde_json::to_value(&packed).unwrap();
        value["files"][0]["content"] = serde_json::Value::String(fact.descriptor.clone());
        let changed: PackedRecipe = serde_json::from_value(value).unwrap();
        assert!(changed.after_owner_metadata(&capsule, &store).is_err());
        let mut value = serde_json::to_value(&packed).unwrap();
        value["files"]
            .as_array_mut()
            .unwrap()
            .push(serde_json::json!({"path":"new-hook.sh","content":fact.descriptor}));
        let changed: PackedRecipe = serde_json::from_value(value).unwrap();
        assert!(changed.after_owner_metadata(&capsule, &store).is_err());
    }
    #[test]
    fn resolution_is_not_terminal_and_keeps_causal_compacted_owner_proof() {
        let (f, store, journal, producer) = created_owner();
        // The genuine owner nested fact proof compacts independently; the new
        // resolution still has to name its exact retained complete terminal.
        store.lock().unwrap().close_native(&producer).unwrap();
        let (original, capsule) = capsule(&store);
        let fact = resolution(&store, &producer, &original, &capsule);
        write(&f, &fact.binding, "002.after-owner.json", &fact);
        let validate = || {
            custody::validate_files(
                &journal.transaction,
                files(&f.0, &journal.transaction),
                None,
                false,
                None,
                false,
                None,
            )
        };
        assert!(validate().is_ok());
        let intent = custody::Intent {
            version: 1,
            binding: fact.binding.clone(),
            owner: fact.owner.clone(),
            resource: fact.resource.clone(),
            expected: fact.expected.clone(),
            desired: fact.desired.clone(),
            source: fact.source.clone(),
            recipe: fact.request.clone(),
        };
        write(
            &f,
            &fact.binding,
            "002.reservation.json",
            &crate::native_retention::Reservation::new(&fact.binding),
        );
        write(&f, &fact.binding, "002.intent.json", &intent);
        assert!(validate().is_err());
        let mut terminal = crate::custody::tests::terminal(&fact.binding, "accepted", vec![]);
        terminal.owner = fact.owner.clone();
        terminal.resource = fact.resource.clone();
        write(&f, &fact.binding, "002.accepted.json", &terminal);
        assert!(validate().is_err());
        terminal.state = "complete".into();
        terminal.observed = fact.desired.clone();
        write(&f, &fact.binding, "002.terminal.json", &terminal);
        assert!(validate().is_ok());
        let mut bad: Resolution =
            serde_json::from_slice(&codec::encode(&fact, true).unwrap()).unwrap();
        bad.producer_terminal = "e".repeat(64);
        write(&f, &fact.binding, "002.after-owner.json", &bad);
        assert!(validate().is_err());
        write(&f, &fact.binding, "002.after-owner.json", &fact);
        std::fs::remove_file(
            f.0.join("migrations/native-custody/v1")
                .join(&journal.transaction)
                .join("001.terminal.json"),
        )
        .unwrap();
        assert!(validate().is_err());
    }
    #[test]
    fn resolution_selects_only_the_explicit_completed_owner_retry_chain() {
        let (f, store, journal, base) = created_owner();
        let snap = store
            .read(&Address::native(&base.transaction, 1, "intent").unwrap())
            .unwrap();
        let intent_hash = digest(snap.bytes().unwrap());
        let mut intent: custody::Intent =
            codec::decode_canonical(snap.bytes().unwrap(), true).unwrap();
        let snap = store
            .read(&Address::native(&base.transaction, 1, "terminal").unwrap())
            .unwrap();
        let mut terminal: native::Terminal =
            codec::decode_canonical(snap.bytes().unwrap(), true).unwrap();
        terminal.state = "failed".into();
        write(&f, &base, "001.terminal.json", &terminal);
        let mut producer = base.clone();
        producer.ordinal = 2050;
        let edge: crate::native_recovery::Resolution = serde_json::from_value(serde_json::json!({
            "version":1,"binding":base,"intent":intent_hash,"retry":producer,"fence":null
        }))
        .unwrap();
        write(&f, &base, "001.recovery.json", &edge);
        intent.binding = producer.clone();
        write(
            &f,
            &producer,
            "2050.reservation.json",
            &crate::native_retention::Reservation::new(&producer),
        );
        write(&f, &producer, "2050.intent.json", &intent);
        let mut accepted = crate::custody::tests::terminal(&producer, "accepted", vec![]);
        accepted.owner = "owner-registration".into();
        accepted.resource = "test-yard".into();
        write(&f, &producer, "2050.accepted.json", &accepted);
        accepted.state = "complete".into();
        accepted.observed = intent.desired.clone();
        write(&f, &producer, "2050.terminal.json", &accepted);
        let (original, capsule) = capsule(&store);
        let fact = resolution(&store, &producer, &original, &capsule);
        write(&f, &fact.binding, "002.after-owner.json", &fact);
        let validate = || {
            custody::validate_files(
                &journal.transaction,
                files(&f.0, &journal.transaction),
                None,
                false,
                None,
                false,
                None,
            )
        };
        assert!(validate().is_ok());
        let mut foreign = fact;
        foreign.owner_ordinal = 3;
        write(&f, &foreign.binding, "002.after-owner.json", &foreign);
        assert!(validate().is_err());
        foreign.owner_ordinal = 1;
        foreign.producer = base.clone();
        foreign.producer_terminal = digest(
            store
                .read(&Address::native(&base.transaction, 1, "terminal").unwrap())
                .unwrap()
                .bytes()
                .unwrap(),
        );
        write(&f, &foreign.binding, "002.after-owner.json", &foreign);
        assert!(validate().is_err());
    }
    #[test]
    fn declared_tight_capacity_allows_many_small_scopes_and_refuses_overflow() {
        let (_f, store, _, _) = fixture();
        assert!(store
            .after_owner_capacity((0..100).map(|_| capacity()).collect())
            .is_ok());
        assert!(store
            .after_owner_capacity(
                (0..129)
                    .map(|_| Capacity {
                        metadata_bytes: DELTA_MAX,
                        descriptor_bytes: DELTA_MAX,
                        ..capacity()
                    })
                    .collect()
            )
            .is_err());
        for raw in [br#"{"version":1,"phase":"resolved","metadataBytes":128,"descriptorBytes":128}"#.as_slice(),
            br#"{"version":1,"phase":"template","metadataBytes":0,"descriptorBytes":128}"#,
            br#"{"version":1,"phase":"template","metadataBytes":128,"descriptorBytes":128,"extra":1}"#] {
            assert!(Capacity::checked(raw).is_err());
        }
        let (original, capsule) = capsule(&store);
        assert_eq!(
            capsule.after_owner_capacity(&store).unwrap().metadata_bytes,
            4096
        );
        assert!(original.validate().is_ok());
    }
    #[test]
    fn protected_resolution_publication_rejects_aliases_and_pending_facts() {
        let (f, store, journal, producer) = created_owner();
        let (original, capsule) = capsule(&store);
        let fact = resolution(&store, &producer, &original, &capsule);
        write(&f, &fact.binding, "002.after-owner.json", &fact);
        let path =
            f.0.join("migrations/native-custody/v1")
                .join(&journal.transaction)
                .join("002.after-owner.json");
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o644)).unwrap();
        assert!(store.read(&address(&fact.binding).unwrap()).is_err());
        std::fs::remove_file(&path).unwrap();
        std::os::unix::fs::symlink("001.terminal.json", &path).unwrap();
        assert!(store.read(&address(&fact.binding).unwrap()).is_err());
        std::fs::remove_file(&path).unwrap();
        write(&f, &fact.binding, ".002.after-owner.json.pending", &fact);
        assert!(custody::validate_files(
            &journal.transaction,
            files(&f.0, &journal.transaction),
            None,
            false,
            None,
            false,
            None
        )
        .is_err());
    }
    #[test]
    fn conditional_bundle_requires_resolution_before_intent_or_complete() {
        let (_f, store, journal, binding) = fixture();
        let (original, capsule) = capsule(&store);
        let lease = store.lock().unwrap();
        // Inert prepared-bundle expectation only, not a fabricated completed
        // migration or a SourceQuery acceptance fixture.
        lease
            .publish_bundle(
                &record::Journal::new(journal.clone()).unwrap(),
                &crate::wire::plan_v1::PlanFacts::default(),
                &binding.helper,
                &[],
                std::slice::from_ref(&original),
                &binding.session,
                &Recipes::from([(original.prepared_recipe.clone(), capsule)]),
            )
            .unwrap();
        drop(lease);
        assert!(store
            .require_after_owner_resolutions(&journal.transaction, &[], false)
            .is_ok());
        assert!(store
            .require_after_owner_resolutions(&journal.transaction, &[], true)
            .is_err());
        assert!(store
            .require_after_owner_resolutions(
                &journal.transaction,
                &["001.intent.json".into()],
                false
            )
            .is_err());
        assert!(store
            .native_inventory(
                Some(&j::Record {
                    checkpoint: "complete".into(),
                    ..journal.clone()
                }),
                false
            )
            .is_err());
        assert!(store.native_inventory(Some(&journal), false).is_ok());
    }
    #[test]
    fn completed_owner_noop_needs_no_fictitious_invocation_or_resolution() {
        let (_f, store, journal, binding) = fixture();
        let (mut original, capsule) = capsule(&store);
        original.state = "stopped".into();
        original.instance_uuid = "original-real-id".into();
        let before = crate::work::OwnerBefore {
            state: "current".into(),
            registration: "a".repeat(64),
            overrides: "b".repeat(64),
            controller: "c".repeat(64),
            shared_images: true,
            prepared_recipe: String::new(),
        };
        let hash = before.fingerprint().unwrap();
        assert!(owner_noop(&before, &hash, &hash).unwrap());
        let work = crate::work::Work {
            intent: crate::wire::plan_v1::PlannerStepIntent {
                expected: hash.clone(),
                desired: hash,
                ..Default::default()
            },
            recipe: Recipe::TestYardOwnerRegistration(before),
        };
        let facts = crate::wire::plan_v1::PlanFacts {
            artifact_digest: binding.artifact.clone(),
            ..Default::default()
        };
        store
            .lock()
            .unwrap()
            .publish_bundle(
                &record::Journal::new(journal.clone()).unwrap(),
                &facts,
                &binding.helper,
                &[work],
                std::slice::from_ref(&original),
                &binding.session,
                &Recipes::from([(original.prepared_recipe.clone(), capsule)]),
            )
            .unwrap();
        assert!(store
            .require_after_owner_resolutions(&journal.transaction, &[], true)
            .is_ok());
        assert!(store.pending_after_owner_capacity().unwrap().is_empty());
        assert!(store
            .native_inventory(
                Some(&j::Record {
                    checkpoint: "complete".into(),
                    ..journal
                }),
                false
            )
            .is_ok());
        // Any future activation intent would contradict this no-effect path.
        assert!(store
            .require_after_owner_resolutions(
                &binding.transaction,
                &["002.intent.json".into()],
                true
            )
            .is_err());
    }
    #[test]
    fn physical_body_is_lossless_and_canonical_without_becoming_source_payload() {
        let (_f, store, _, producer) = created_owner();
        let (original, capsule) = capsule(&store);
        let mut fact = resolution(&store, &producer, &original, &capsule);
        let raw = store.prepared_payload(&fact.descriptor).unwrap();
        let mut metadata = PhysicalMetadata::checked(&raw).unwrap();
        // Generic carrier test; the owning Go variant validates its record's
        // fields and original plan. Rust does not interpret component policy.
        for (physical, accepted) in [
            (STANDARD.encode([0, 255, 13, 10]), true),
            ("AP8NCg".into(), false),
        ] {
            metadata.physical = physical;
            fact.descriptor = store
                .lock()
                .unwrap()
                .publish_prepared_payload(&codec::encode(&metadata, false).unwrap())
                .unwrap();
            fact.recipe = capsule
                .with_after_owner_metadata(&fact.descriptor)
                .unwrap()
                .fingerprint(&store)
                .unwrap();
            let target = metadata.delta.apply(&original, &fact.recipe);
            fact.resolved = digest(&codec::encode(&target, false).unwrap());
            fact.request = N::ApplyActivation {
                binding: fact.binding.clone(),
                target,
            }
            .recipe_fingerprint()
            .unwrap();
            assert_eq!(fact.load(&store, &original, &capsule).is_ok(), accepted);
        }
    }
    #[test]
    fn physical_metadata_blob_above_record_limit_reopens_without_raising_records() {
        let (f, store, _, producer) = created_owner();
        let (mut original, capsule) = capsule(&store);
        let lease = store.lock().unwrap();
        let template = lease
            .publish_prepared_payload(
                &codec::encode(
                    &Capacity {
                        metadata_bytes: 3 << 20,
                        descriptor_bytes: 3 << 20,
                        ..capacity()
                    },
                    false,
                )
                .unwrap(),
            )
            .unwrap();
        drop(lease);
        let capsule = capsule.with_after_owner_metadata(&template).unwrap();
        original.prepared_recipe = capsule.fingerprint(&store).unwrap();
        original.after_owner = format!("v1:runtime:{}", original.prepared_recipe);
        let mut fact = resolution(&store, &producer, &original, &capsule);
        let mut metadata =
            PhysicalMetadata::checked(&store.prepared_payload(&fact.descriptor).unwrap()).unwrap();
        metadata.physical = STANDARD.encode(vec![255; (1 << 20) + 128]);
        let raw = codec::encode_native_recipe(&metadata).unwrap();
        assert!(raw.len() > codec::MAX_BYTES);
        assert!(codec::decode_canonical::<serde_json::Value>(&raw, false).is_err());
        PhysicalMetadata::checked(&raw).unwrap();
        assert!(
            PhysicalMetadata::checked(&vec![b'x'; crate::prepared_payload::FILE_MAX + 1]).is_err()
        );
        let lease = store.lock().unwrap();
        fact.descriptor = lease.publish_prepared_payload(&raw).unwrap();
        fact.recipe = capsule
            .with_after_owner_metadata(&fact.descriptor)
            .unwrap()
            .fingerprint(&store)
            .unwrap();
        let target = metadata.delta.apply(&original, &fact.recipe);
        fact.resolved = digest(&codec::encode(&target, false).unwrap());
        fact.request = N::ApplyActivation {
            binding: fact.binding.clone(),
            target,
        }
        .recipe_fingerprint()
        .unwrap();
        let record = codec::encode(&fact, true).unwrap();
        Resolution::checked(&record).unwrap();
        lease
            .create_immutable(&address(&fact.binding).unwrap(), &record)
            .unwrap();
        drop(lease);
        let fresh = Store::open_canonical_fixture(&f.0).unwrap();
        let snap = fresh.read(&address(&fact.binding).unwrap()).unwrap();
        let fact = Resolution::checked(snap.bytes().unwrap()).unwrap();
        let (_, recipes) = fact.load(&fresh, &original, &capsule).unwrap();
        assert!(crate::prepared_payload::references(recipes.values()).contains(&fact.descriptor));
        assert_eq!(fresh.prepared_payload(&fact.descriptor).unwrap(), raw);
    }
    #[test]
    fn actual_go_conditional_recipes_roundtrip() {
        let Some(path) = std::env::var_os("SUBYARD_RUST_AFTER_OWNER_CORPUS") else {
            return;
        };
        #[derive(Deserialize)]
        #[serde(deny_unknown_fields)]
        struct Item {
            raw: String,
            sha256: String,
            target: native::Target,
        }
        #[derive(Deserialize)]
        #[serde(rename_all = "camelCase", deny_unknown_fields)]
        struct Pair {
            name: String,
            original: Item,
            resolved: Item,
            producer: native::Binding,
            producer_terminal: String,
            owner_ordinal: u64,
        }
        let rows: Vec<Pair> = serde_json::from_slice(&std::fs::read(path).unwrap()).unwrap();
        assert!(!rows.is_empty());
        for row in rows {
            let (f, store, _, _) = fixture();
            let lease = store.lock().unwrap();
            let read = |item: &Item| {
                let raw = STANDARD.decode(&item.raw).unwrap();
                assert_eq!(STANDARD.encode(&raw), item.raw);
                assert_eq!(digest(&raw), item.sha256);
                assert_eq!(item.sha256, item.target.prepared_recipe);
                PackedRecipe::pack_fixture(&lease, &raw).unwrap()
            };
            let capsule = read(&row.original);
            let packed = read(&row.resolved);
            physical_target(&row.original.target, &row.resolved.target).unwrap();
            let descriptor = packed.after_owner_metadata(&capsule, &store).unwrap();
            let mut binding = row.producer.clone();
            binding.ordinal = row.owner_ordinal + 1;
            let fact = Resolution {
                version: 1,
                binding: binding.clone(),
                producer: row.producer,
                owner_ordinal: row.owner_ordinal,
                producer_terminal: row.producer_terminal,
                original: digest(&codec::encode(&row.original.target, false).unwrap()),
                conditional: row.original.sha256,
                descriptor,
                resolved: digest(&codec::encode(&row.resolved.target, false).unwrap()),
                recipe: row.resolved.sha256,
                owner: row.resolved.target.owner.clone(),
                resource: row.resolved.target.yard.clone(),
                expected: row.resolved.target.actual.clone(),
                desired: row.resolved.target.desired.clone(),
                source: row.resolved.target.source.clone(),
                request: N::ApplyActivation {
                    binding,
                    target: row.resolved.target.clone(),
                }
                .recipe_fingerprint()
                .unwrap(),
            };
            let raw = codec::encode(&fact, true).unwrap();
            Resolution::checked(&raw).unwrap();
            fact.load(&store, &row.original.target, &capsule).unwrap();
            lease
                .create_immutable(&address(&fact.binding).unwrap(), &raw)
                .unwrap();
            drop(lease);
            let fresh = Store::open_canonical_fixture(&f.0).unwrap();
            let raw = fresh.read(&address(&fact.binding).unwrap()).unwrap();
            let fact = Resolution::checked(raw.bytes().unwrap()).unwrap();
            let (target, recipes) = fact.load(&fresh, &row.original.target, &capsule).unwrap();
            assert!(target == row.resolved.target, "{}", row.name);
            assert_eq!(
                recipes[&target.prepared_recipe]
                    .fingerprint(&fresh)
                    .unwrap(),
                target.prepared_recipe
            );
        }
    }
}
