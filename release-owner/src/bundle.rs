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
            Recipe::LocalSSHPortPin(v) | Recipe::TestVMSettingsMigration(v) => {
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
                    local: matches!(w.recipe, Recipe::LocalSSHPortPin(_)),
                    value,
                    input_group: group,
                }
            }
            Recipe::TestYardOwnerRegistration(v) => StoredRecipe::Owner { value: v.clone() },
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
    ) -> Result<()> {
        self.publish_review_bundle(
            journal, facts, helper, work, targets, session, installed, None,
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
        review: Option<(&f::PlanFacts, &crate::recovery::Receipt)>,
    ) -> Result<()> {
        let (groups, stored) = stored(work)?;
        let wire = Wire {
            version: 1,
            session: session.into(),
            transaction: journal.wire().transaction.clone(),
            plan: facts.clone(),
            helper: helper.into(),
            review: review.map(|(facts, receipt)| Review {
                facts: facts.clone(),
                receipt: receipt.wire().clone(),
            }),
            work: stored,
            inputs: groups,
            targets: targets.to_vec(),
            installed: installed.clone(),
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
            version: 1,
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
    pub(crate) fn has_bundle(&self, tx: &str) -> Result<bool> {
        Ok(self.read(&address(tx, None)?)?.bytes().is_some())
    }

    fn read_bundle_wire(&self, tx: &str) -> Result<(Index, Wire)> {
        let snapshot = self.read(&address(tx, None)?)?;
        let index: Index = codec::decode_canonical(snapshot.bytes().ok_or(Error::Invalid)?, true)?;
        require(
            index.version == 1
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
                && wire.version == 1
                && record::fingerprint(&wire.session)
                && wire.transaction == *tx
                && record::fingerprint(&wire.helper)
                && wire.inputs.len() <= 256
                && wire.targets.len() <= 2048,
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
            let has = names
                .iter()
                .any(|n| n.starts_with("bundle.") || n.starts_with(".bundle."));
            if !has {
                continue;
            }
            if !self.has_bundle(&tx)? {
                return Ok(None);
            }
            let (_, wire) = self.read_bundle_wire(&tx)?;
            crate::installed_recipe::validate(
                self,
                &wire.installed,
                &wire.targets,
                wire.work.iter().filter_map(|work| match &work.recipe {
                    StoredRecipe::Owner { value } => Some(value.prepared_recipe.as_str()),
                    _ => None,
                }),
            )?;
            refs.extend(crate::prepared_payload::references(wire.installed.values()));
        }
        Ok(Some(refs))
    }
    pub(crate) fn bundle(
        &self,
        journal: &record::Journal,
        registry: &record::Registry,
    ) -> Result<Bundle> {
        let tx = &journal.wire().transaction;
        let (index, wire) = self.read_bundle_wire(tx)?;
        require(
            index.plan == journal.wire().authorization_plan
                && index.journal == journal.wire().intent_digest,
        )?;
        let plan = crate::plan::Plan::new(wire.plan)?;
        require(wire.work.len() == journal.wire().steps.iter().flatten().count())?;
        if let Some(review) = wire.review {
            let receipt = crate::recovery::Receipt::new(review.receipt)?;
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
            let mut equivalent = review.facts;
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
        let mut work = vec![];
        for (n, w) in wire.work.into_iter().enumerate() {
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
                    let checked = Work::settings(migration, value, local, &w.intent.decision)?;
                    require(checked.intent == w.intent)?;
                    checked.recipe
                }
                StoredRecipe::Owner { value } => {
                    value.validate()?;
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
        Ok(Bundle {
            session: wire.session,
            facts: plan.wire().clone(),
            helper: wire.helper,
            work,
            targets: wire.targets,
            installed: wire.installed,
        })
    }
}
