//! Owner-local immutable prepared recipes. A bundle without the matching durable
//! journal is inert; it is never a grant and cannot select replacement targets.
use crate::{
    codec, digest,
    lease::UpdateLease,
    native,
    record::{self, require},
    store::{Address, Snapshot, Store},
    wire::plan_v1 as f,
    work::{Recipe, Work},
    Error, Result,
};
use base64::{engine::general_purpose::STANDARD, Engine};
use serde::{Deserialize, Serialize};
const CHUNK: usize = 512 << 10;
const MAX: usize = 64 << 20;
fn check_original_checkpoint(facts: &f::PlanFacts, snapshot: &Snapshot) -> Result<()> {
    let observations = facts
        .observations
        .iter()
        .flatten()
        .filter(|o| o.resource == "migration-history.checkpoint")
        .collect::<Vec<_>>();
    require(match observations.as_slice() {
        [] => snapshot.bytes().is_none(),
        [observation] => {
            observation.class == "migration-history-checkpoint-v1"
                && observation.fingerprint == snapshot.fingerprint()
        }
        _ => false,
    })
}

#[cfg(test)]
mod catalog_tests {
    use super::*;
    #[test]
    fn historical_catalog_is_bound_data_not_current_executable_policy() {
        use std::os::unix::fs::PermissionsExt;
        let (_fixture, store, _, old) = crate::custody::tests::fixture();
        std::fs::remove_dir_all(
            _fixture
                .0
                .join("migrations/native-custody/v1")
                .join(old.transaction),
        )
        .unwrap();
        let mut descriptors = crate::migration_owner::builtin_catalog(true, true)
            .unwrap()
            .descriptors()
            .clone()
            .unwrap();
        descriptors.push(crate::wire::registry_v2::CapabilityDescriptor {
            kind: "original-author-settings".into(),
            domain: "settings".into(),
            version: 1,
            ..Default::default()
        });
        let original = record::Catalog::new(Some(descriptors.clone())).unwrap();
        descriptors.push(crate::wire::registry_v2::CapabilityDescriptor {
            kind: "later-author-settings".into(),
            domain: "settings".into(),
            version: 1,
            ..Default::default()
        });
        let later = record::Catalog::new(Some(descriptors)).unwrap();
        assert_ne!(original.digest(), later.digest());
        let registry = record::Registry::new(
            crate::wire::registry_v2::RegistryV2 {
                schema_version: 2,
                minimum_epochs: Some(std::collections::BTreeMap::from([("settings".into(), 1)])),
                current_epochs: Some(std::collections::BTreeMap::from([("settings".into(), 1)])),
                migrations: Some(vec![]),
                ..Default::default()
            },
            &original,
        )
        .unwrap();
        let registry_hash = digest(&codec::encode(registry.wire(), false).unwrap());
        let hash = "a".repeat(64);
        let facts = f::PlanFacts {
            goal: crate::wire::process_v1::Goal {
                target: "new".into(),
                direction: "activate-target".into(),
            },
            releases: crate::wire::journal_v2::ReleasePair {
                from: "old".into(),
                target: "new".into(),
                previous: None,
            },
            links: crate::wire::recovery_v1::ReleaseLinks {
                active: "old".into(),
                previous: None,
            },
            artifact_digest: hash.clone(),
            registry_digest: registry_hash.clone(),
            catalog_digest: original.digest().into(),
            observation_scope: hash.clone(),
            assessment: crate::plan::assess(true, false, &[]).unwrap(),
            observations: Some(vec![f::ResourceObservation {
                resource: "native.prepared-recipes".into(),
                class: "release-native-v1".into(),
                fingerprint: binding(&[], &[]).unwrap(),
            }]),
            ..Default::default()
        };
        let plan = crate::plan::Plan::new(facts).unwrap();
        let journal = plan
            .journal("catalog-history".into(), "fixture-consent", None)
            .unwrap();
        let mut completed = journal.wire().clone();
        completed.checkpoint = "complete".into();
        let journal = record::Journal::new(completed).unwrap();
        store
            .lock()
            .unwrap()
            .publish_bundle(
                &journal,
                plan.wire(),
                &hash,
                &[],
                &[],
                &hash,
                &crate::installed_recipe::Recipes::new(),
                Some(&original),
            )
            .unwrap();
        for dir in ["release-transition", "release-transition/v2"] {
            let path = _fixture.0.join(dir);
            std::fs::create_dir_all(&path).unwrap();
            std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o700)).unwrap();
        }
        for (name, raw) in [
            ("journal.json", codec::encode(journal.wire(), true).unwrap()),
            (
                "ledger.json",
                crate::migration_owner::ledger_bytes(&registry.history().baseline()).unwrap(),
            ),
        ] {
            let path = _fixture.0.join("release-transition/v2").join(name);
            std::fs::write(&path, raw).unwrap();
            std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o600)).unwrap();
        }
        assert!(store.inspect_source(&registry, &registry_hash).is_err());
        assert!(store.inspect_rust_source(&registry, &registry_hash).is_ok());
        assert_eq!(
            store.source_catalog(&journal).unwrap().unwrap().digest(),
            original.digest()
        );
        // The later executable catalog cannot replace the original snapshot.
        let mut changed = journal.wire().clone();
        changed.catalog_digest = later.digest().into();
        changed.intent_digest = record::Journal::intent_digest(&changed).unwrap();
        assert!(store
            .source_catalog(&record::Journal::new(changed).unwrap())
            .is_err());
        let mut changed = journal.wire().clone();
        changed.transaction = "foreign-catalog".into();
        assert!(store
            .source_catalog(&record::Journal::new(changed).unwrap())
            .is_err());
        let mut changed = journal.wire().clone();
        changed.authorization_plan = format!("plan-v1-{}", "b".repeat(64));
        changed.intent_digest = record::Journal::intent_digest(&changed).unwrap();
        assert!(store
            .source_catalog(&record::Journal::new(changed).unwrap())
            .is_err());
        assert!(store
            .lock()
            .unwrap()
            .publish_bundle(
                &journal,
                plan.wire(),
                &hash,
                &[],
                &[],
                &hash,
                &crate::installed_recipe::Recipes::new(),
                Some(&later)
            )
            .is_err());
        let (index, wire) = store.read_bundle_wire(&journal.wire().transaction).unwrap();
        let valid = codec::encode_bundle(&wire).unwrap();
        for fault in ["mutated", "malformed", "missing"] {
            let mut wire: Wire = serde_json::from_slice(&valid).unwrap();
            match fault {
                "missing" => wire.catalog = None,
                "malformed" => wire.catalog.as_mut().unwrap()[0].kind = "invalid/kind".into(),
                _ => wire.catalog.as_mut().unwrap()[0].kind = "different-author".into(),
            }
            // Rebind the mechanical container so refusal must come from the
            // snapshot/plan contract, rather than a stale transport checksum.
            let raw = codec::encode_bundle(&wire).unwrap();
            let piece = codec::encode(
                &Piece {
                    data: STANDARD.encode(&raw),
                },
                true,
            )
            .unwrap();
            let changed = Index {
                version: index.version,
                transaction: index.transaction.clone(),
                plan: index.plan.clone(),
                journal: index.journal.clone(),
                length: raw.len(),
                digest: digest(&raw),
                pieces: vec![digest(&piece)],
            };
            for (part, bytes) in [
                (Some(0), piece),
                (None, codec::encode(&changed, true).unwrap()),
            ] {
                let address = address(&journal.wire().transaction, part).unwrap();
                let path = _fixture
                    .0
                    .join(address.parents.join("/"))
                    .join(address.name);
                std::fs::write(path, bytes).unwrap();
            }
            let result = store.source_catalog(&journal);
            if fault == "missing" {
                assert!(result.unwrap().is_none());
            } else {
                assert!(result.is_err(), "{fault} snapshot accepted");
            }
            assert!(
                store
                    .inspect_rust_source(&registry, &registry_hash)
                    .is_err(),
                "{fault} source accepted"
            );
        }
    }
}
#[derive(Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "kebab-case", deny_unknown_fields)]
enum StoredRecipe {
    Settings {
        local: bool,
        value: crate::work::SettingsRecipe,
        input_group: usize,
    },
    Owner {
        value: crate::work::OwnerBefore,
        #[serde(default, skip_serializing_if = "Option::is_none")]
        program: Option<crate::program::Program>,
    },
    Legacy {
        pin: String,
    },
    Import,
    Entrypoints,
    Ledger {
        before: Option<Vec<u8>>,
        after: Vec<u8>,
    },
}
fn stored_slots(recipe: &StoredRecipe) -> Result<usize> {
    if let StoredRecipe::Owner {
        program: Some(program),
        ..
    } = recipe
    {
        program.validate()?;
        Ok(program.slots())
    } else {
        Ok(1)
    }
}
fn stored_ordinal(work: &[StoredWork], index: usize) -> Result<u64> {
    require(index <= work.len())?;
    let mut ordinal = 1usize;
    for item in &work[..index] {
        ordinal = ordinal
            .checked_add(stored_slots(&item.recipe)?)
            .ok_or(Error::Bound)?;
    }
    require(ordinal <= 2048 || index == work.len() && ordinal == 2049)?;
    Ok(ordinal as u64)
}
fn stored_completion_ordinal(work: &[StoredWork], index: usize) -> Result<u64> {
    let item = work.get(index).ok_or(Error::Invalid)?;
    let base = stored_ordinal(work, index)?;
    let extra = if let StoredRecipe::Owner {
        program: Some(program),
        ..
    } = &item.recipe
    {
        program.validate()?;
        program.forward.len() - 1
    } else {
        0
    };
    let ordinal = base.checked_add(extra as u64).ok_or(Error::Bound)?;
    require(ordinal <= 2048)?;
    Ok(ordinal)
}
#[derive(Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct StoredWork {
    intent: f::PlannerStepIntent,
    recipe: StoredRecipe,
}
#[derive(Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct Wire {
    version: i64,
    session: String,
    transaction: String,
    plan: f::PlanFacts,
    helper: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    review: Option<Review>,
    work: Vec<StoredWork>,
    inputs: Vec<Vec<native::InputBinding>>,
    targets: Vec<native::Target>,
    #[serde(default, skip_serializing_if = "std::collections::BTreeMap::is_empty")]
    installed: crate::installed_recipe::Recipes,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    original_checkpoint: Option<OriginalCheckpoint>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    catalog: Option<Vec<crate::wire::registry_v2::CapabilityDescriptor>>,
}
#[derive(Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct OriginalCheckpoint {
    bytes: Option<Vec<u8>>,
}
#[derive(Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct Review {
    facts: f::PlanFacts,
    receipt: crate::wire::recovery_v1::RecoveryReceiptV1,
}
#[derive(Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct Index {
    version: i64,
    transaction: String,
    plan: String,
    journal: String,
    length: usize,
    digest: String,
    pieces: Vec<String>,
}
#[derive(Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct Piece {
    data: String,
}
pub(crate) struct Bundle {
    pub session: String,
    pub facts: f::PlanFacts,
    pub helper: String,
    pub work: Vec<Work>,
    pub targets: Vec<native::Target>,
    pub installed: crate::installed_recipe::Recipes,
    pub original_checkpoint: Option<Snapshot>,
}
fn address(tx: &str, part: Option<usize>) -> Result<Address> {
    require(record::safe_id(tx) && part.is_none_or(|p| p < 128))?;
    Ok(Address::at(
        vec![
            "migrations".into(),
            "native-custody".into(),
            "v1".into(),
            tx.into(),
        ],
        part.map_or_else(|| "bundle.json".into(), |p| format!("bundle.{p:03}.json")),
        true,
    ))
}
fn stored(work: &[Work]) -> Result<(Vec<Vec<native::InputBinding>>, Vec<StoredWork>)> {
    let mut groups = Vec::<Vec<native::InputBinding>>::new();
    let mut stored = vec![];
    for w in work {
        let recipe = match &w.recipe {
            Recipe::Settings {
                definition,
                recipe: v,
            } => {
                let mut value = v.clone();
                let group = groups
                    .iter()
                    .position(|g| g == &value.inputs)
                    .unwrap_or_else(|| {
                        groups.push(value.inputs.clone());
                        groups.len() - 1
                    });
                value.inputs.clear();
                StoredRecipe::Settings {
                    local: definition.wire_local,
                    value,
                    input_group: group,
                }
            }
            Recipe::TestYardOwnerRegistration(v) => {
                if let Some(program) = &v.program {
                    program.validate()?;
                }
                StoredRecipe::Owner {
                    value: v.clone(),
                    program: v.program.clone(),
                }
            }
            Recipe::LegacyV1Import { static_pin } => StoredRecipe::Legacy {
                pin: static_pin.clone(),
            },
            Recipe::SourceImport => StoredRecipe::Import,
            Recipe::SourceEntrypoints => StoredRecipe::Entrypoints,
            Recipe::LedgerAdvance { before, after } => StoredRecipe::Ledger {
                before: before.bytes().map(Vec::from),
                after: after.clone(),
            },
        };
        stored.push(StoredWork {
            intent: w.intent.clone(),
            recipe,
        });
    }
    Ok((groups, stored))
}
pub(crate) fn binding(work: &[Work], targets: &[native::Target]) -> Result<String> {
    let (groups, work) = stored(work)?;
    #[derive(Serialize)]
    struct Value<'a> {
        work: Vec<StoredWork>,
        inputs: Vec<Vec<native::InputBinding>>,
        targets: &'a [native::Target],
    }
    Ok(digest(&codec::encode_bundle(&Value {
        work,
        inputs: groups,
        targets,
    })?))
}
impl UpdateLease<'_> {
    #[allow(clippy::too_many_arguments)]
    pub(crate) fn publish_bundle(
        &self,
        journal: &record::Journal,
        facts: &f::PlanFacts,
        helper: &str,
        work: &[Work],
        targets: &[native::Target],
        session: &str,
        installed: &crate::installed_recipe::Recipes,
        catalog: Option<&record::Catalog>,
    ) -> Result<()> {
        self.publish_review_bundle(
            journal, facts, helper, work, targets, session, installed, catalog, None,
        )
    }
    #[allow(clippy::too_many_arguments)]
    pub(crate) fn publish_review_bundle(
        &self,
        journal: &record::Journal,
        facts: &f::PlanFacts,
        helper: &str,
        work: &[Work],
        targets: &[native::Target],
        session: &str,
        installed: &crate::installed_recipe::Recipes,
        catalog: Option<&record::Catalog>,
        review: Option<(&f::PlanFacts, &crate::recovery::Receipt)>,
    ) -> Result<()> {
        let catalog = catalog
            .map(|catalog| {
                require(
                    catalog.digest() == facts.catalog_digest
                        && catalog.digest() == journal.wire().catalog_digest,
                )?;
                catalog.descriptors().clone().ok_or(Error::Invalid)
            })
            .transpose()?;
        let (groups, stored) = stored(work)?;
        let checkpoint = self.store.read(&Address::checkpoint())?;
        check_original_checkpoint(facts, &checkpoint)?;
        let wire = Wire {
            version: 2,
            session: session.into(),
            transaction: journal.wire().transaction.clone(),
            plan: facts.clone(),
            helper: helper.into(),
            review: review.map(|(facts, receipt)| Review {
                facts: facts.clone(),
                receipt: receipt.wire().clone(),
            }),
            catalog,
            work: stored,
            inputs: groups,
            targets: targets.to_vec(),
            installed: installed.clone(),
            original_checkpoint: Some(OriginalCheckpoint {
                bytes: checkpoint.bytes().map(Vec::from),
            }),
        };
        let raw = codec::encode_bundle(&wire)?;
        require(raw.len() <= MAX)?;
        let mut pieces = vec![];
        let mut lengths = vec![];
        for part in raw.chunks(CHUNK) {
            let bytes = codec::encode(
                &Piece {
                    data: STANDARD.encode(part),
                },
                true,
            )?;
            lengths.push(bytes.len());
            pieces.push(digest(&bytes));
        }
        let index = Index {
            version: 2,
            transaction: wire.transaction.clone(),
            plan: journal.wire().authorization_plan.clone(),
            journal: journal.wire().intent_digest.clone(),
            length: raw.len(),
            digest: digest(&raw),
            pieces,
        };
        let index = codec::encode(&index, true)?;
        lengths.push(index.len());
        self.native_bundle_capacity(&wire.transaction, &lengths)?;
        // Retain lengths/digests rather than another whole base64 bundle.
        for (n, part) in raw.chunks(CHUNK).enumerate() {
            self.create_immutable(
                &address(&wire.transaction, Some(n))?,
                &codec::encode(
                    &Piece {
                        data: STANDARD.encode(part),
                    },
                    true,
                )?,
            )?;
        }
        self.create_immutable(&address(&wire.transaction, None)?, &index)
    }
}
impl Store {
    fn wire_owner_noop(&self, index: &Index, wire: &Wire) -> Result<bool> {
        let owners = wire
            .work
            .iter()
            .enumerate()
            .filter_map(|(n, w)| {
                if let StoredRecipe::Owner { value, .. } = &w.recipe {
                    Some((n, w, value))
                } else {
                    None
                }
            })
            .collect::<Vec<_>>();
        if owners.len() != 1 {
            return Ok(false);
        }
        let (n, work, before) = owners[0];
        if !crate::after_owner::owner_noop(before, &work.intent.expected, &work.intent.desired)? {
            return Ok(false);
        }
        for target in wire.targets.iter().filter(|t| !t.after_owner.is_empty()) {
            target.validate()?;
            require(
                crate::after_owner::AfterOwnerActivation::parse(&target.after_owner)?.1
                    == target.prepared_recipe
                    && target.state != "stale",
            )?;
        }
        let mut binding = native::Binding {
            session: wire.session.clone(),
            transaction: wire.transaction.clone(),
            plan: index.plan.clone(),
            journal: index.journal.clone(),
            artifact: wire.plan.artifact_digest.clone(),
            helper: wire.helper.clone(),
            ordinal: stored_ordinal(&wire.work, n)?,
        };
        for _ in 0..stored_slots(&work.recipe)? {
            if !self.no_owner_invocation(&binding)? {
                return Ok(false);
            }
            binding.ordinal += 1;
        }
        Ok(true)
    }
    pub(crate) fn require_after_owner_resolutions(
        &self,
        tx: &str,
        names: &[String],
        complete: bool,
    ) -> Result<()> {
        if !self.has_bundle(tx)? {
            return Ok(());
        }
        let (index, wire) = self.read_bundle_wire(tx)?;
        if !wire.targets.iter().any(|t| !t.after_owner.is_empty()) {
            return Ok(());
        }
        let no_op = self.wire_owner_noop(&index, &wire)?;
        let target_base = stored_ordinal(&wire.work, wire.work.len())?;
        for (n, _) in wire
            .targets
            .iter()
            .enumerate()
            .filter(|(_, t)| !t.after_owner.is_empty())
        {
            let ordinal = target_base + n as u64;
            require(ordinal <= 2048)?;
            let required = complete && !no_op
                || names.iter().any(|name| {
                    *name == format!("{ordinal:03}.intent.json")
                        || *name == format!(".{ordinal:03}.intent.json.pending")
                });
            if required
                && !names
                    .iter()
                    .any(|name| *name == format!("{ordinal:03}.after-owner.json"))
            {
                return Err(Error::Unknown);
            }
        }
        Ok(())
    }
    pub(crate) fn pending_after_owner_capacity(&self) -> Result<Vec<crate::after_owner::Capacity>> {
        let Some(root) = &self.root else {
            return Ok(vec![]);
        };
        let Some(parent) = crate::store::directory(
            root,
            &["migrations".into(), "native-custody".into(), "v1".into()],
            false,
        )?
        else {
            return Ok(vec![]);
        };
        let retiring = self.native_retirement()?;
        let mut values = vec![];
        for tx in crate::inventory::names(&parent, 256)? {
            if retiring.as_deref() == Some(&tx) || !self.has_bundle(&tx)? {
                continue;
            }
            let (index, wire) = self.read_bundle_wire(&tx)?;
            if !wire.targets.iter().any(|t| !t.after_owner.is_empty()) {
                continue;
            }
            if self.wire_owner_noop(&index, &wire)? {
                continue;
            }
            let target_base = stored_ordinal(&wire.work, wire.work.len())?;
            for (n, target) in wire
                .targets
                .iter()
                .enumerate()
                .filter(|(_, t)| !t.after_owner.is_empty())
            {
                let ordinal = target_base + n as u64;
                require(ordinal <= 2048)?;
                let a = Address::at(
                    vec![
                        "migrations".into(),
                        "native-custody".into(),
                        "v1".into(),
                        tx.clone(),
                    ],
                    format!("{ordinal:03}.after-owner.json"),
                    true,
                );
                if self.read(&a)?.bytes().is_none() {
                    let recipe = wire
                        .installed
                        .get(&target.prepared_recipe)
                        .ok_or(Error::Protected)?;
                    values.push(recipe.after_owner_capacity(self)?);
                }
            }
        }
        Ok(values)
    }
    pub(crate) fn after_owner_original(
        &self,
        binding: &native::Binding,
        owner_ordinal: u64,
    ) -> Result<(native::Target, crate::installed_recipe::PackedRecipe)> {
        let (index, wire) = self.read_bundle_wire(&binding.transaction)?;
        require(
            index.plan == binding.plan
                && index.journal == binding.journal
                && wire.session == binding.session
                && wire.helper == binding.helper
                && wire.plan.artifact_digest == binding.artifact,
        )?;
        let target_base = stored_ordinal(&wire.work, wire.work.len())?;
        let n = usize::try_from(
            binding
                .ordinal
                .checked_sub(target_base)
                .ok_or(Error::Invalid)?,
        )
        .map_err(|_| Error::Invalid)?;
        let target = wire.targets.get(n).ok_or(Error::Invalid)?.clone();
        let (_, hash) = crate::after_owner::AfterOwnerActivation::parse(&target.after_owner)?;
        require(target.prepared_recipe == hash)?;
        let recipe = wire.installed.get(hash).ok_or(Error::Protected)?.clone();
        require(recipe.fingerprint(self)? == hash)?;
        let owners = wire
            .work
            .iter()
            .enumerate()
            .filter(|(_, w)| matches!(w.recipe, StoredRecipe::Owner { .. }))
            .collect::<Vec<_>>();
        require(owners.len() == 1)?;
        let (owner_index, owner) = owners[0];
        require(stored_completion_ordinal(&wire.work, owner_index)? == owner_ordinal)?;
        let StoredRecipe::Owner { value: before, .. } = &owner.recipe else {
            return Err(Error::Protected);
        };
        let mut base = binding.clone();
        base.ordinal = owner_ordinal;
        let snap = self.read(&Address::native(
            &binding.transaction,
            owner_ordinal,
            "intent",
        )?)?;
        let intent: crate::custody::Intent =
            codec::decode_canonical(snap.bytes().ok_or(Error::Unknown)?, true)?;
        require(
            intent.binding == base
                && intent.owner == "owner-registration"
                && intent.resource == "test-yard"
                && intent.expected == owner.intent.expected
                && intent.desired == owner.intent.desired
                && intent.source == before.controller
                && before.fingerprint()? == owner.intent.expected
                && before.desired().fingerprint()? == owner.intent.desired,
        )?;
        require(owner.intent.expected != owner.intent.desired)?;
        Ok((target, recipe))
    }
    pub(crate) fn has_bundle(&self, tx: &str) -> Result<bool> {
        Ok(self.read(&address(tx, None)?)?.bytes().is_some())
    }

    fn read_bundle_wire(&self, tx: &str) -> Result<(Index, Wire)> {
        let snapshot = self.read(&address(tx, None)?)?;
        let index: Index = codec::decode_canonical(snapshot.bytes().ok_or(Error::Invalid)?, true)?;
        require(
            matches!(index.version, 1 | 2)
                && index.transaction == *tx
                && record::token(&index.plan, "plan-v1-")
                && record::fingerprint(&index.journal)
                && index.length > 0
                && index.length <= MAX
                && index.pieces.len() == index.length.div_ceil(CHUNK),
        )?;
        let mut raw = Vec::with_capacity(index.length);
        for (n, hash) in index.pieces.iter().enumerate() {
            require(record::fingerprint(hash))?;
            let s = self.read(&address(tx, Some(n))?)?;
            let bytes = s.bytes().ok_or(Error::Invalid)?;
            require(digest(bytes) == *hash)?;
            let piece: Piece = codec::decode_canonical(bytes, true)?;
            let data = STANDARD.decode(&piece.data).map_err(|_| Error::Codec)?;
            require(
                STANDARD.encode(&data) == piece.data
                    && data.len() == CHUNK.min(index.length - raw.len()),
            )?;
            raw.extend(data);
        }
        require(raw.len() == index.length && digest(&raw) == index.digest)?;
        let wire: Wire = serde_json::from_slice(&raw).map_err(|_| Error::Codec)?;
        require(
            codec::encode_bundle(&wire)? == raw
                && wire.version == index.version
                && (wire.version == 2) == wire.original_checkpoint.is_some()
                && record::fingerprint(&wire.session)
                && wire.transaction == *tx
                && record::fingerprint(&wire.helper)
                && wire.inputs.len() <= 256
                && wire.targets.len() <= 2048,
        )?;
        for work in &wire.work {
            stored_slots(&work.recipe)?;
        }
        require(
            stored_ordinal(&wire.work, wire.work.len())?
                .checked_add(wire.targets.len() as u64)
                .is_some_and(|n| n <= 2049),
        )?;
        Ok((index, wire))
    }
    pub(crate) fn bundle_payload_references(
        &self,
    ) -> Result<Option<std::collections::BTreeSet<String>>> {
        let mut refs = std::collections::BTreeSet::new();
        let Some(root) = &self.root else {
            return Ok(Some(refs));
        };
        let Some(parent) = crate::store::directory(
            root,
            &["migrations".into(), "native-custody".into(), "v1".into()],
            false,
        )?
        else {
            return Ok(Some(refs));
        };
        for tx in crate::inventory::names(&parent, 256)? {
            require(record::safe_id(&tx))?;
            let dir = crate::store::directory(&parent, std::slice::from_ref(&tx), false)?
                .ok_or(Error::Stale)?;
            let names = crate::inventory::names(&dir, 8192)?;
            self.require_after_owner_resolutions(&tx, &names, false)?;
            if names
                .iter()
                .any(|n| n.starts_with('.') && n.ends_with(".after-owner.json.pending"))
            {
                return Ok(None);
            }
            let has = names
                .iter()
                .any(|n| n.starts_with("bundle.") || n.starts_with(".bundle."));
            if !has {
                require(!names.iter().any(|n| n.ends_with(".after-owner.json")))?;
                continue;
            }
            if !self.has_bundle(&tx)? {
                return Ok(None);
            }
            let (_, wire) = self.read_bundle_wire(&tx)?;
            refs.extend(self.after_owner_references(&tx, &names)?);
            crate::installed_recipe::validate(
                self,
                &wire.installed,
                &wire.targets,
                wire.work.iter().filter_map(|work| match &work.recipe {
                    StoredRecipe::Owner { value, .. } => Some(value.prepared_recipe.as_str()),
                    _ => None,
                }),
            )?;
            refs.extend(crate::prepared_payload::references(wire.installed.values()));
        }
        Ok(Some(refs))
    }
    // Bound retained data only. Reading source identity does not reconstruct
    // executable recipes or admit this snapshot as the current owner's policy.
    fn bound_bundle_wire(
        &self,
        journal: &record::Journal,
    ) -> Result<(Wire, crate::plan::Plan, Option<record::Catalog>)> {
        let tx = &journal.wire().transaction;
        let (index, wire) = self.read_bundle_wire(tx)?;
        require(
            index.plan == journal.wire().authorization_plan
                && index.journal == journal.wire().intent_digest,
        )?;
        let plan = crate::plan::Plan::new(wire.plan.clone())?;
        require(wire.work.len() == journal.wire().steps.iter().flatten().count())?;
        if let Some(review) = &wire.review {
            let receipt = crate::recovery::Receipt::new(review.receipt.clone())?;
            receipt.matches_successor(journal.wire())?;
            let durable = self.recovery_for(journal)?.ok_or(Error::Invalid)?;
            require(durable.wire() == receipt.wire())?;
            let checked = crate::plan::Plan::new(review.facts.clone())?;
            require(
                checked.token()
                    == if receipt.wire().schema_version == 2 {
                        &receipt.wire().base_plan
                    } else {
                        &journal.wire().authorization_plan
                    },
            )?;
            let mut equivalent = review.facts.clone();
            equivalent.assessment = plan.wire().assessment.clone();
            require(equivalent == *plan.wire())?;
        } else {
            require(plan.token() == journal.wire().authorization_plan)?;
        }
        let reconstructed = plan.journal_authorized(
            journal.wire().transaction.clone(),
            "reconstruction",
            journal.wire().source_ingress.clone(),
            &journal.wire().authorization_plan,
        )?;
        require(reconstructed.wire().resume_plan == journal.wire().resume_plan)?;
        let expected = plan.wire().intents.as_deref().unwrap_or_default();
        for (n, w) in wire.work.iter().enumerate() {
            require(expected.get(n) == Some(&w.intent))?;
            let step = &journal.wire().steps.as_ref().ok_or(Error::Invalid)?[n];
            require(
                step.id == w.intent.id
                    && step.migration == w.intent.migration
                    && step.resource == w.intent.resource
                    && step.expected == w.intent.expected
                    && step.desired == w.intent.desired
                    && step.decision == w.intent.decision,
            )?;
        }
        let catalog = wire
            .catalog
            .clone()
            .map(|descriptors| record::Catalog::new(Some(descriptors)))
            .transpose()?;
        if let Some(catalog) = &catalog {
            require(
                catalog.digest() == plan.wire().catalog_digest
                    && catalog.digest() == journal.wire().catalog_digest,
            )?;
        }
        Ok((wire, plan, catalog))
    }
    pub(crate) fn source_catalog(
        &self,
        journal: &record::Journal,
    ) -> Result<Option<record::Catalog>> {
        Ok(self.bound_bundle_wire(journal)?.2)
    }
    pub(crate) fn bundle(
        &self,
        journal: &record::Journal,
        registry: &record::Registry,
    ) -> Result<Bundle> {
        let (wire, plan, _catalog) = self.bound_bundle_wire(journal)?;
        let mut work = vec![];
        for w in wire.work {
            let recipe = match w.recipe {
                StoredRecipe::Settings {
                    local,
                    mut value,
                    input_group,
                } => {
                    require(value.inputs.is_empty())?;
                    value.inputs = wire.inputs.get(input_group).ok_or(Error::Invalid)?.clone();
                    let migration = registry
                        .wire()
                        .migrations
                        .iter()
                        .flatten()
                        .find(|m| m.id == w.intent.migration)
                        .ok_or(Error::Invalid)?;
                    let definition =
                        crate::settings::migration(&migration.kind).ok_or(Error::Invalid)?;
                    require(local == definition.wire_local)?;
                    let checked = Work::settings(migration, definition, value, &w.intent.decision)?;
                    require(checked.intent == w.intent)?;
                    checked.recipe
                }
                StoredRecipe::Owner { mut value, program } => {
                    value.validate()?;
                    if let Some(program) = &program {
                        program.validate()?;
                    }
                    value.program = program;
                    require(
                        value.fingerprint()? == w.intent.expected
                            && value.desired().fingerprint()? == w.intent.desired,
                    )?;
                    Recipe::TestYardOwnerRegistration(value)
                }
                StoredRecipe::Legacy { pin } => {
                    require(record::fingerprint(&pin))?;
                    Recipe::LegacyV1Import { static_pin: pin }
                }
                StoredRecipe::Import => Recipe::SourceImport,
                StoredRecipe::Entrypoints => Recipe::SourceEntrypoints,
                StoredRecipe::Ledger { before, after } => {
                    let snap = Snapshot::captured(before);
                    require(
                        snap.fingerprint() == w.intent.expected
                            && digest(&after) == w.intent.desired,
                    )?;
                    let ledger: crate::wire::ledger_v2::LedgerV2 =
                        codec::decode_canonical(&after, true)?;
                    record::Ledger::new(ledger, registry)?;
                    Recipe::LedgerAdvance {
                        before: snap,
                        after,
                    }
                }
            };
            work.push(Work {
                intent: w.intent,
                recipe,
            });
        }
        for target in &wire.targets {
            target.validate()?;
        }
        require(plan.wire().observations.iter().flatten().any(|o| {
            o.resource == "native.prepared-recipes"
                && o.class == "release-native-v1"
                && binding(&work, &wire.targets).is_ok_and(|b| b == o.fingerprint)
        }))?;
        crate::installed_recipe::validate(
            self,
            &wire.installed,
            &wire.targets,
            crate::work::owner_payload_hashes(&work),
        )?;
        let original_checkpoint = wire
            .original_checkpoint
            .map(|v| Snapshot::captured(v.bytes));
        if let Some(snapshot) = &original_checkpoint {
            check_original_checkpoint(plan.wire(), snapshot)?;
        }
        Ok(Bundle {
            session: wire.session,
            facts: plan.wire().clone(),
            helper: wire.helper,
            work,
            targets: wire.targets,
            installed: wire.installed,
            original_checkpoint,
        })
    }
}
