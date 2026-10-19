//! Lossless packing of the fixed native installed recipe. Rust binds bytes and
//! headers; only the owning native component interprets paths and effects.
use crate::{
    codec, digest,
    lease::UpdateLease,
    native,
    owner::Owner,
    record::{self, require},
    store::Store,
    Error, Result,
};
use base64::{engine::general_purpose::STANDARD, Engine};
use serde::{Deserialize, Serialize};
use std::collections::{BTreeMap, BTreeSet};
const MAX: usize = 96 << 20;
#[derive(Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct Recipe {
    version: u8,
    owner: String,
    source: String,
    runtime: String,
    resources: String,
    launcher: String,
    hook_before: String,
    leaves: Option<Vec<String>>,
    files: Option<Vec<File>>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    profile: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    handler: String,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    native: Vec<NativeInput>,
}
#[derive(Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct File {
    path: String,
    content: Option<String>,
}
#[derive(Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub(crate) struct PackedRecipe {
    version: u8,
    owner: String,
    source: String,
    runtime: String,
    resources: String,
    launcher: String,
    hook_before: String,
    leaves: Option<Vec<String>>,
    pub files: Option<Vec<PackedFile>>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    profile: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    handler: String,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    native: Vec<NativeInput>,
}
#[derive(Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct PackedFile {
    path: String,
    pub content: Option<String>,
}
#[derive(Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct NativeInput {
    path: String,
    before: String,
}
pub(crate) type Recipes = BTreeMap<String, PackedRecipe>;
impl Recipe {
    fn validate_shape(&self) -> Result<()> {
        PackedRecipe {
            version: self.version,
            owner: self.owner.clone(),
            source: self.source.clone(),
            runtime: self.runtime.clone(),
            resources: self.resources.clone(),
            launcher: self.launcher.clone(),
            hook_before: self.hook_before.clone(),
            leaves: self.leaves.clone(),
            files: None,
            profile: self.profile.clone(),
            handler: self.handler.clone(),
            native: self.native.clone(),
        }
        .validate()?;
        require(self.files.as_ref().is_none_or(|files| {
            files.len() <= 8192
                && files.windows(2).all(|p| p[0].path < p[1].path)
                && files.iter().all(|f| record::text(&f.path, 512, true))
        }))?;
        let mut total = 0usize;
        for file in self.files.iter().flatten() {
            if let Some(content) = &file.content {
                require(content.len() <= (32usize << 20).div_ceil(3) * 4)?;
                let raw = STANDARD.decode(content).map_err(|_| Error::Codec)?;
                require(raw.len() <= 32 << 20 && STANDARD.encode(&raw) == *content)?;
                total = total.checked_add(raw.len()).ok_or(Error::Bound)?;
                require(total <= 64 << 20)?;
            }
        }
        Ok(())
    }
}
impl PackedRecipe {
    fn pack(lease: &UpdateLease<'_>, wire: Recipe) -> Result<Self> {
        wire.validate_shape()?;
        let mut total = 0usize;
        let files = wire
            .files
            .map(|files| {
                files
                    .into_iter()
                    .map(|f| {
                        let content = f
                            .content
                            .map(|value| {
                                let bytes = STANDARD.decode(&value).map_err(|_| Error::Codec)?;
                                require(STANDARD.encode(&bytes) == value)?;
                                total = total.checked_add(bytes.len()).ok_or(Error::Bound)?;
                                require(total <= 64 << 20)?;
                                lease.publish_prepared_payload(&bytes)
                            })
                            .transpose()?;
                        Ok(PackedFile {
                            path: f.path,
                            content,
                        })
                    })
                    .collect::<Result<Vec<_>>>()
            })
            .transpose()?;
        let packed = Self {
            version: wire.version,
            owner: wire.owner,
            source: wire.source,
            runtime: wire.runtime,
            resources: wire.resources,
            launcher: wire.launcher,
            hook_before: wire.hook_before,
            leaves: wire.leaves,
            files,
            profile: wire.profile,
            handler: wire.handler,
            native: wire.native,
        };
        packed.validate()?;
        Ok(packed)
    }
    fn validate(&self) -> Result<()> {
        require(
            self.version == 1
                && record::safe_id(&self.owner)
                && [&self.source, &self.runtime, &self.resources]
                    .iter()
                    .all(|h| record::fingerprint(h))
                && record::text(&self.launcher, 4096, false)
                && record::text(&self.profile, 512, false)
                && record::text(&self.handler, 512, false)
                && self.native.len() <= 512
                && self
                    .native
                    .iter()
                    .all(|n| record::text(&n.path, 4096, true) && record::fingerprint(&n.before))
                && (self.hook_before.is_empty() || record::fingerprint(&self.hook_before))
                && self
                    .leaves
                    .as_ref()
                    .is_none_or(|v| v.len() <= 512 && v.iter().all(|s| record::text(s, 512, true)))
                && self.files.as_ref().is_none_or(|v| {
                    v.len() <= 8192
                        && v.windows(2).all(|p| p[0].path < p[1].path)
                        && v.iter().all(|f| {
                            record::text(&f.path, 512, true)
                                && f.content.as_ref().is_none_or(|h| record::fingerprint(h))
                        })
                }),
        )
    }
    fn raw(&self, store: &Store) -> Result<Vec<u8>> {
        self.validate()?;
        let mut total = 0usize;
        let files = self
            .files
            .as_ref()
            .map(|files| {
                files
                    .iter()
                    .map(|f| {
                        let content = f
                            .content
                            .as_ref()
                            .map(|hash| {
                                let bytes = store.prepared_payload(hash)?;
                                total = total.checked_add(bytes.len()).ok_or(Error::Bound)?;
                                require(total <= 64 << 20)?;
                                Ok(STANDARD.encode(bytes))
                            })
                            .transpose()?;
                        Ok(File {
                            path: f.path.clone(),
                            content,
                        })
                    })
                    .collect::<Result<Vec<_>>>()
            })
            .transpose()?;
        let raw = codec::encode_native_recipe(&Recipe {
            version: self.version,
            owner: self.owner.clone(),
            source: self.source.clone(),
            runtime: self.runtime.clone(),
            resources: self.resources.clone(),
            launcher: self.launcher.clone(),
            hook_before: self.hook_before.clone(),
            leaves: self.leaves.clone(),
            files,
            profile: self.profile.clone(),
            handler: self.handler.clone(),
            native: self.native.clone(),
        })?;
        require(raw.len() <= MAX)?;
        Ok(raw)
    }
}
pub(crate) fn validate<'a>(
    store: &Store,
    recipes: &Recipes,
    targets: &'a [native::Target],
    owner_hashes: impl Iterator<Item = &'a str>,
) -> Result<()> {
    let expected: BTreeSet<_> = targets
        .iter()
        .filter(|t| !t.prepared_recipe.is_empty())
        .map(|t| t.prepared_recipe.as_str())
        .chain(owner_hashes.filter(|hash| !hash.is_empty()))
        .collect();
    require(
        recipes
            .keys()
            .map(String::as_str)
            .eq(expected.iter().copied()),
    )?;
    for (hash, recipe) in recipes {
        recipe.validate()?;
        require(digest(&recipe.raw(store)?) == *hash)?;
    }
    Ok(())
}
fn accepted<T>(owner: &Owner, value: Result<T>) -> Result<T> {
    if value.is_err() {
        if let crate::legacy::Reader::Frozen(reader) = &owner.store.reader {
            reader.reject_native_projection();
        }
    }
    value
}
impl Owner {
    pub(crate) fn capture_installed_recipes(
        &self,
        lease: &UpdateLease<'_>,
        targets: &[native::Target],
        work: &[crate::work::Work],
    ) -> Result<Recipes> {
        lease.cleanup_native_transactions()?;
        let mut recipes = Recipes::new();
        let hashes = targets
            .iter()
            .map(|t| t.prepared_recipe.as_str())
            .chain(crate::work::owner_payload_hashes(work))
            .filter(|hash| !hash.is_empty())
            .collect::<BTreeSet<_>>();
        for hash in hashes {
            if recipes.contains_key(hash) {
                continue;
            }
            let mut raw = vec![];
            let mut total = None;
            loop {
                let reply = self.call(native::Request::ReadInstalledRecipe {
                    session: self.session.clone(),
                    recipe_hash: hash.to_owned(),
                    offset: raw.len() as u64,
                })?;
                let chunk = reply.installed_chunk.ok_or(Error::Invalid)?;
                let bytes = STANDARD.decode(&chunk.content).map_err(|_| Error::Codec)?;
                accepted(
                    self,
                    (|| {
                        require(
                            chunk.recipe_hash == hash
                                && chunk.offset == raw.len() as u64
                                && chunk.total > 0
                                && chunk.total <= MAX as u64
                                && total.is_none_or(|t| t == chunk.total)
                                && bytes.len()
                                    == crate::prepared_payload::CHUNK
                                        .min(chunk.total as usize - raw.len())
                                && STANDARD.encode(&bytes) == chunk.content
                                && chunk.complete
                                    == (raw.len() + bytes.len() == chunk.total as usize),
                        )?;
                        Ok(())
                    })(),
                )?;
                total = Some(chunk.total);
                raw.extend(bytes);
                if chunk.complete {
                    break;
                }
            }
            let wire: Recipe = accepted(
                self,
                (|| {
                    require(digest(&raw) == hash)?;
                    let wire: Recipe = serde_json::from_slice(&raw).map_err(|_| Error::Codec)?;
                    wire.validate_shape()?;
                    require(codec::encode_native_recipe(&wire)? == raw)?;
                    Ok(wire)
                })(),
            )?;
            drop(raw);
            let packed = PackedRecipe::pack(lease, wire)?;
            packed.validate()?;
            recipes.insert(hash.to_owned(), packed);
        }
        validate(
            &self.store,
            &recipes,
            targets,
            crate::work::owner_payload_hashes(work),
        )?;
        Ok(recipes)
    }
    pub(crate) fn bind_installed_recipe(&self, hash: &str, recipes: &Recipes) -> Result<()> {
        if hash.is_empty() {
            return Ok(());
        }
        let packed = recipes.get(hash).ok_or(Error::Invalid)?;
        let raw = packed.raw(&self.store)?;
        require(digest(&raw) == hash)?;
        for (offset, part) in raw.chunks(crate::prepared_payload::CHUNK).enumerate() {
            let offset = offset * crate::prepared_payload::CHUNK;
            let reply = self.call(native::Request::BindInstalledRecipe {
                session: self.session.clone(),
                recipe_hash: hash.to_owned(),
                offset: offset as u64,
                total: raw.len() as u64,
                content: STANDARD.encode(part),
            })?;
            let chunk = reply.installed_chunk.ok_or(Error::Invalid)?;
            require(
                chunk.recipe_hash == hash
                    && chunk.offset == offset as u64
                    && chunk.total == raw.len() as u64
                    && chunk.content.is_empty()
                    && chunk.complete == (offset + part.len() == raw.len()),
            )?;
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        custody::tests::fixture,
        wire::{plan_v1 as f, process_v1 as p, recovery_v1 as r},
    };
    fn wire() -> Recipe {
        Recipe {
            version: 1,
            owner: "sample-runtime".into(),
            source: "a".repeat(64),
            runtime: "b".repeat(64),
            resources: "c".repeat(64),
            launcher: "".into(),
            hook_before: "".into(),
            leaves: Some(vec!["owner.sh".into()]),
            files: Some(vec![
                File {
                    path: "allowed-a".into(),
                    content: None,
                },
                File {
                    path: "allowed-b".into(),
                    content: Some(String::new()),
                },
                File {
                    path: "allowed-c".into(),
                    content: Some(STANDARD.encode([0, 255, 13, 10])),
                },
            ]),
            profile: "sample".into(),
            handler: "owner.sh".into(),
            native: vec![NativeInput {
                path: "opaque-native-path".into(),
                before: "d".repeat(64),
            }],
        }
    }
    fn target(hash: &str) -> native::Target {
        native::Target {
            prepared_recipe: hash.into(),
            scope: "".into(),
            reported_actual: "a".repeat(64),
            reported_desired: "b".repeat(64),
            before: "".into(),
            class: "profile-runtime".into(),
            scope_state: "".into(),
            stage: "".into(),
            owner: "sample-runtime".into(),
            yard: "default".into(),
            project: "default".into(),
            instance: "default".into(),
            instance_uuid: "absent".into(),
            state: "stale".into(),
            actual: "a".repeat(64),
            desired: "b".repeat(64),
            source: "a".repeat(64),
        }
    }
    // The owning Go parent holds the real component fixture and its local Incus
    // fact server alive. This test exercises only preparation/cache transport;
    // no native effect request or release authority is issued.
    #[test]
    fn actual_large_native_recipe_roundtrip() {
        let Ok(path) = std::env::var("SUBYARD_RUST_LARGE_RECIPE_FIXTURE") else {
            return;
        };
        #[derive(Deserialize)]
        #[serde(rename_all = "camelCase", deny_unknown_fields)]
        struct Case {
            context: native::Context,
            target: native::Target,
            expected_length: usize,
            expected_hash: String,
        }
        fn peer(context: &native::Context, helper: &str) -> crate::legacy::FrozenReader {
            let root = std::fs::File::open(&context.config_home).unwrap();
            let repository = std::fs::File::open(&context.repository_root).unwrap();
            assert!(native::Identity::capture(&root).unwrap() == context.root);
            assert!(native::Identity::capture(&repository).unwrap() == context.repository);
            let reader = crate::legacy::FrozenReader::capture_native_helper(
                std::fs::File::open(helper).unwrap(),
            )
            .unwrap();
            reader
                .native(
                    &native::Request::Initialize {
                        context: context.clone(),
                    },
                    vec![root.into(), repository.into()],
                )
                .unwrap()
                .checked()
                .unwrap();
            reader
                .native(
                    &native::Request::CaptureInventory {
                        session: context.session.clone(),
                    },
                    vec![],
                )
                .unwrap()
                .checked()
                .unwrap();
            reader
        }
        fn captured(reader: &crate::legacy::FrozenReader, case: &Case) {
            let reply = reader
                .native(
                    &native::Request::CaptureActivation {
                        session: case.context.session.clone(),
                        owner: case.target.owner.clone(),
                        fresh: false,
                    },
                    vec![],
                )
                .unwrap()
                .checked()
                .unwrap();
            if !reply.targets.iter().any(|target| target == &case.target) {
                let expected = serde_json::to_value(&case.target).unwrap();
                for target in reply.targets.iter().filter(|target| {
                    target.owner == case.target.owner
                        && target.class == case.target.class
                        && target.yard == case.target.yard
                }) {
                    let actual = serde_json::to_value(target).unwrap();
                    for (field, value) in expected.as_object().unwrap() {
                        if actual.get(field) != Some(value) {
                            let original = serde_json::to_vec(value).unwrap();
                            let observed = serde_json::to_vec(&actual.get(field)).unwrap();
                            eprintln!("target-field-drift {field}: expected {} bytes {} observed {} bytes {}", original.len(), digest(&original), observed.len(), digest(&observed));
                        }
                    }
                }
                panic!("native target differs from original fixture facts");
            }
        }
        let case: Case = serde_json::from_slice(&std::fs::read(path).unwrap()).unwrap();
        assert!(case.expected_length > 48 << 20 && case.expected_length <= MAX);
        assert_eq!(case.expected_hash, case.target.prepared_recipe);
        case.target.validate().unwrap();
        let helper = std::env::var("SUBYARD_RUST_NATIVE_HELPER").unwrap();
        let reader = peer(&case.context, &helper);
        captured(&reader, &case);
        let mut raw = Vec::new();
        loop {
            let reply = reader
                .native(
                    &native::Request::ReadInstalledRecipe {
                        session: case.context.session.clone(),
                        recipe_hash: case.expected_hash.clone(),
                        offset: raw.len() as u64,
                    },
                    vec![],
                )
                .unwrap()
                .checked()
                .unwrap();
            let chunk = reply.installed_chunk.unwrap();
            assert_eq!(chunk.total, case.expected_length as u64);
            raw.extend(STANDARD.decode(chunk.content).unwrap());
            if chunk.complete {
                break;
            }
        }
        assert_eq!(raw.len(), case.expected_length);
        assert_eq!(digest(&raw), case.expected_hash);
        let wire: Recipe = serde_json::from_slice(&raw).unwrap();
        assert!(
            codec::encode_native_recipe(&wire).unwrap() == raw,
            "canonical recipe bytes differ"
        );
        let store =
            Store::open_canonical_fixture(std::path::Path::new(&case.context.config_home)).unwrap();
        let packed = PackedRecipe::pack(&store.lock().unwrap(), wire).unwrap();
        let targets = vec![case.target.clone()];
        let recipes = BTreeMap::from([(case.expected_hash.clone(), packed)]);
        validate(&store, &recipes, &targets, std::iter::empty()).unwrap();
        let catalog = crate::migration_owner::builtin_catalog(true, true).unwrap();
        let root = std::fs::File::open(&case.context.repository_root).unwrap();
        let registry_raw = crate::assets::read(
            &root.into(),
            "config/release-transition.json",
            codec::MAX_BYTES,
        )
        .unwrap();
        let (registry, registry_digest) =
            record::Registry::from_frozen(&reader, &registry_raw, &catalog).unwrap();
        let facts = f::PlanFacts {
            goal: p::Goal {
                target: case.context.target.clone(),
                direction: case.context.direction.clone(),
            },
            releases: case.context.releases.clone(),
            links: r::ReleaseLinks {
                active: case.context.releases.from.clone(),
                previous: case.context.releases.previous.clone(),
            },
            artifact_digest: case.context.artifact.clone(),
            registry_digest,
            catalog_digest: catalog.digest().into(),
            observation_scope: "a".repeat(64),
            assessment: crate::plan::assess(false, false, &[]).unwrap(),
            observations: Some(vec![f::ResourceObservation {
                resource: "native.prepared-recipes".into(),
                class: "release-native-v1".into(),
                fingerprint: crate::bundle::binding(&[], &targets).unwrap(),
            }]),
            ..Default::default()
        };
        let plan = crate::plan::Plan::new(facts).unwrap();
        let journal = plan
            .journal(
                "large-transport-fixture".into(),
                "fixture-only-no-effect",
                None,
            )
            .unwrap();
        store
            .lock()
            .unwrap()
            .publish_bundle(
                &journal,
                plan.wire(),
                &reader.image_digest(),
                &[],
                &targets,
                &case.context.session,
                &recipes,
            )
            .unwrap();
        drop(raw);
        drop(recipes);
        drop(reader);
        drop(store);
        let store =
            Store::open_canonical_fixture(std::path::Path::new(&case.context.config_home)).unwrap();
        let bundle = store.bundle(&journal, &registry).unwrap();
        let raw = bundle
            .installed
            .get(&case.expected_hash)
            .unwrap()
            .raw(&store)
            .unwrap();
        assert_eq!(digest(&raw), case.expected_hash);
        assert_eq!(raw.len(), case.expected_length);
        let reader = peer(&case.context, &helper);
        captured(&reader, &case);
        for (index, chunk) in raw.chunks(crate::prepared_payload::CHUNK).enumerate() {
            let reply = reader
                .native(
                    &native::Request::BindInstalledRecipe {
                        session: case.context.session.clone(),
                        recipe_hash: case.expected_hash.clone(),
                        offset: (index * crate::prepared_payload::CHUNK) as u64,
                        total: raw.len() as u64,
                        content: STANDARD.encode(chunk),
                    },
                    vec![],
                )
                .unwrap()
                .checked()
                .unwrap();
            assert_eq!(
                reply.installed_chunk.unwrap().complete,
                (index + 1) * crate::prepared_payload::CHUNK >= raw.len()
            );
        }
        reader.health().unwrap();
    }
    #[test]
    fn owner_payload_uses_the_exact_shared_recipe_set() {
        let (_f, store, _, _) = fixture();
        let raw = codec::encode_native_recipe(&wire()).unwrap();
        let hash = digest(&raw);
        let packed = PackedRecipe::pack(&store.lock().unwrap(), wire()).unwrap();
        let recipes = BTreeMap::from([(hash.clone(), packed)]);
        validate(&store, &recipes, &[], std::iter::once(hash.as_str())).unwrap();
        assert!(validate(&store, &recipes, &[], std::iter::empty()).is_err());
        let wrong = "e".repeat(64);
        assert!(validate(&store, &recipes, &[], std::iter::once(wrong.as_str())).is_err());
        let before = crate::work::OwnerBefore {
            state: "current".into(),
            registration: "a".repeat(64),
            overrides: "b".repeat(64),
            controller: "c".repeat(64),
            shared_images: true,
            prepared_recipe: hash,
        };
        let semantic = before.semantic();
        assert_eq!(
            before.fingerprint().unwrap(),
            semantic.fingerprint().unwrap()
        );
        assert_ne!(
            codec::encode(&before, false).unwrap(),
            codec::encode(&semantic, false).unwrap()
        );
        assert!(!String::from_utf8(codec::encode(&semantic, false).unwrap())
            .unwrap()
            .contains("preparedRecipe"));
    }
    #[test]
    fn recipe_file_capacity_is_checked_before_payload_publication() {
        let (_f, store, _, _) = fixture();
        let mut recipe = wire();
        recipe.files = Some(
            (0..8193)
                .map(|n| File {
                    path: format!("payload/{n:05}"),
                    content: Some(STANDARD.encode(b"same approved bytes")),
                })
                .collect(),
        );
        assert!(PackedRecipe::pack(&store.lock().unwrap(), recipe).is_err());
        assert_eq!(store.payload_usage().unwrap().roots, 0);
        let mut recipe = wire();
        recipe.files = Some(
            (0..8192)
                .map(|n| File {
                    path: format!("payload/{n:05}"),
                    content: None,
                })
                .collect(),
        );
        let raw = codec::encode_native_recipe(&recipe).unwrap();
        let packed = PackedRecipe::pack(&store.lock().unwrap(), recipe).unwrap();
        assert_eq!(packed.raw(&store).unwrap(), raw);
        assert_eq!(store.payload_usage().unwrap().roots, 0);
    }
    #[test]
    fn lossless_null_empty_binary_and_referenced_bundle_keep_payloads() {
        let (f, s, _, _) = fixture();
        let raw = codec::encode_native_recipe(&wire()).unwrap();
        let hash = digest(&raw);
        let packed = PackedRecipe::pack(&s.lock().unwrap(), wire()).unwrap();
        assert_eq!(packed.raw(&s).unwrap(), raw);
        assert_eq!(s.payload_usage().unwrap().roots, 2);
        let again = PackedRecipe::pack(&s.lock().unwrap(), wire()).unwrap();
        assert_eq!(again.raw(&s).unwrap(), raw);
        assert_eq!(s.payload_usage().unwrap().roots, 2);
        let targets = vec![target(&hash)];
        let recipes = BTreeMap::from([(hash.clone(), packed)]);
        let binding = crate::bundle::binding(&[], &targets).unwrap();
        let h = "a".repeat(64);
        let facts = f::PlanFacts {
            goal: p::Goal {
                target: "new".into(),
                direction: "activate-target".into(),
            },
            releases: crate::wire::journal_v2::ReleasePair {
                from: "old".into(),
                target: "new".into(),
                previous: None,
            },
            links: r::ReleaseLinks {
                active: "old".into(),
                previous: None,
            },
            artifact_digest: h.clone(),
            registry_digest: h.clone(),
            catalog_digest: h.clone(),
            observation_scope: h.clone(),
            assessment: crate::plan::assess(true, false, &[]).unwrap(),
            observations: Some(vec![f::ResourceObservation {
                resource: "native.prepared-recipes".into(),
                class: "release-native-v1".into(),
                fingerprint: binding,
            }]),
            ..Default::default()
        };
        let plan = crate::plan::Plan::new(facts).unwrap();
        let journal = plan
            .journal("payload-reference".into(), "private-consent", None)
            .unwrap();
        s.lock()
            .unwrap()
            .publish_bundle(&journal, plan.wire(), &h, &[], &targets, &h, &recipes)
            .unwrap();
        s.lock().unwrap().cleanup_prepared_payloads().unwrap();
        assert_eq!(s.payload_usage().unwrap().roots, 2);
        let address = crate::store::Address::journal();
        s.lock()
            .unwrap()
            .compare_and_swap(
                &address,
                &s.read(&address).unwrap(),
                &codec::encode(journal.wire(), true).unwrap(),
            )
            .unwrap();
        s.lock().unwrap().cleanup_native_transactions().unwrap();
        assert_eq!(s.payload_usage().unwrap().roots, 2);
        std::fs::remove_file(f.0.join("release-transition/v2/journal.json")).unwrap();
        s.lock().unwrap().cleanup_native_transactions().unwrap();
        assert_eq!(s.payload_usage().unwrap().roots, 0);
    }
    #[test]
    fn actual_go_installed_recipe_codec() {
        let Ok(path) = std::env::var("SUBYARD_RUST_INSTALLED_RECIPE_CORPUS") else {
            return;
        };
        #[derive(Deserialize)]
        struct Case {
            name: String,
            raw: String,
            sha256: String,
            #[serde(default)]
            target: Option<native::Target>,
        }
        let cases: Vec<Case> = serde_json::from_slice(&std::fs::read(path).unwrap()).unwrap();
        assert!(!cases.is_empty());
        for case in cases {
            let (_f, s, _, _) = fixture();
            let raw = STANDARD.decode(case.raw).unwrap();
            assert_eq!(digest(&raw), case.sha256, "{}", case.name);
            let wire: Recipe = serde_json::from_slice(&raw).unwrap();
            assert_eq!(
                codec::encode_native_recipe(&wire).unwrap(),
                raw,
                "{} Go canonical",
                case.name
            );
            let packed = PackedRecipe::pack(&s.lock().unwrap(), wire).unwrap();
            assert_eq!(
                packed.raw(&s).unwrap(),
                raw,
                "{} restored original",
                case.name
            );
            if let Some(target) = case.target {
                assert_eq!(target.prepared_recipe, case.sha256);
                let recipes = BTreeMap::from([(case.sha256.clone(), packed)]);
                validate(
                    &s,
                    &recipes,
                    std::slice::from_ref(&target),
                    std::iter::empty(),
                )
                .unwrap();
                let mut wrong = target.clone();
                wrong.prepared_recipe = "e".repeat(64);
                assert!(validate(
                    &s,
                    &recipes,
                    std::slice::from_ref(&wrong),
                    std::iter::empty()
                )
                .is_err());
                let mut wrong_raw = recipes;
                let packed = wrong_raw.remove(&case.sha256).unwrap();
                wrong_raw.insert(wrong.prepared_recipe.clone(), packed);
                assert!(validate(
                    &s,
                    &wrong_raw,
                    std::slice::from_ref(&wrong),
                    std::iter::empty()
                )
                .is_err());
            }
        }
    }
}
