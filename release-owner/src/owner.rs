//! Release operation ownership. No production facade routes here yet.
//! Prepared inspection borrows the owner without taking its mutation lease.
use crate::{
    codec, digest, ledger_execution,
    legacy::{FrozenReader, Reader},
    links::RuntimeLinks,
    native::{self, Request as N},
    plan,
    record::{self, require},
    settings,
    store::{Address, Snapshot, Store},
    wire::{journal_v2 as j, plan_v1 as f, process_v1 as p, registry_v2 as r},
    work::{Recipe, Work},
    Error, Result,
};
use serde::{Deserialize, Serialize};
use std::{
    collections::{BTreeMap, BTreeSet},
    fs::File,
    io::Read,
    path::Path,
    time::{Duration, Instant},
};

/// Operation configuration, not a grant or manifest-authenticated delivery proof.
/// The eventual verified facade supplies these exact retained owner inputs.
#[derive(Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct Options {
    pub data_home: String,
    pub config_home: String,
    pub runtime_root: String,
    pub repository_root: String,
    pub yard: String,
    pub releases: j::ReleasePair,
    pub artifact_digest: String,
    #[serde(with = "crate::wire::go_bytes")]
    pub registry_raw: Option<Vec<u8>>,
    pub catalog: Vec<r::CapabilityDescriptor>,
    pub inherited_setting_ids: Vec<String>,
    pub rollback_target: Option<f::RollbackTarget>,
    pub migration_checkpoint: bool,
    pub source_ingress: Option<j::SourceIngress>,
    #[serde(default)]
    pub source_checkpoint: Option<SourceCheckpoint>,
    #[serde(default)]
    pub source_artifact: Option<SourceArtifact>,
    #[serde(default)]
    pub target_artifact: Option<SourceArtifact>,
    #[serde(default)]
    pub previous_artifact: Option<SourceArtifact>,
    #[serde(default)]
    pub candidate_version: String,
    #[serde(default)]
    pub config_layout: Option<ConfigLayout>,
    #[serde(default)]
    pub replacement: Option<p::JournalReplacement>,
    #[serde(default)]
    pub migration_sources: Option<MigrationSources>,
    #[serde(default)]
    pub original_sources: Option<MigrationSources>,
}
#[derive(Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct MigrationSources {
    pub source: SourceArtifact,
    pub previous: Option<SourceArtifact>,
}
#[derive(Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct ConfigLayout {
    #[serde(with = "crate::wire::go_bytes")]
    pub manifest_raw: Option<Vec<u8>>,
    pub manifest_digest: Option<String>,
    pub command_manifest_digest: Option<String>,
    #[serde(with = "crate::wire::go_bytes")]
    pub command_options_raw: Option<Vec<u8>>,
    pub command_options_status: String,
}
#[derive(Serialize, Deserialize, Default)]
#[serde(default, rename_all = "camelCase", deny_unknown_fields)]
pub(crate) struct LayoutManifest {
    pub schema_version: i64,
    pub files: Option<Vec<LayoutFile>>,
}
#[derive(Serialize, Deserialize, Default)]
#[serde(default, deny_unknown_fields)]
pub(crate) struct LayoutFile {
    pub path: String,
}
impl crate::legacy::Historical for LayoutManifest {
    const KIND: crate::legacy::Kind = crate::legacy::Kind::ConfigLayout;
}
#[derive(Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct SourceArtifact {
    pub release: String,
    pub version: String,
    pub artifact_digest: String,
    pub engine_digest: String,
    pub root: crate::native::Identity,
}
/// Exact source facts authenticated by the verified facade. The owner additionally
/// reopens the closed retained release under its pinned runtime root and checks
/// root identity, manifest digest and the manifest-bound capability bytes.
#[derive(Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct SourceCheckpoint {
    pub source_release: String,
    pub artifact_digest: String,
    pub root: crate::native::Identity,
    #[serde(with = "crate::wire::go_bytes")]
    pub capability_raw: Option<Vec<u8>>,
    pub capability_digest: String,
}
#[derive(Serialize, Deserialize, Default)]
#[serde(default, rename_all = "camelCase", deny_unknown_fields)]
pub(crate) struct CheckpointCapability {
    pub schema_version: i64,
    pub contract: String,
}
impl crate::legacy::Historical for CheckpointCapability {
    const KIND: crate::legacy::Kind = crate::legacy::Kind::CheckpointCapability;
}
pub struct Owner {
    pub(crate) repository: File,
    pub(crate) first_cause: std::cell::RefCell<Option<native::Diagnostic>>,
    pub(crate) last_cause: std::cell::RefCell<Option<native::Diagnostic>>,
    pub(crate) outer: crate::outer_custody::OuterCustody,
    pub(crate) options: Options,
    pub(crate) store: Store,
    pub(crate) links: RuntimeLinks,
    pub(crate) registry: record::Registry,
    pub(crate) catalog: record::Catalog,
    pub(crate) registry_digest: String,
    pub(crate) session: String,
}
pub struct Grant {
    pub(crate) secret: String,
}
impl Grant {
    /// The frozen facade supplies a private inherited pipe, not an environment
    /// variable or public request field. The helper never receives its contents.
    pub fn capture(mut input: File) -> Result<Self> {
        use std::os::unix::fs::FileTypeExt;
        let meta = input.metadata().map_err(|_| Error::Io)?;
        require(meta.file_type().is_fifo() || meta.file_type().is_socket())?;
        let mut raw = Vec::new();
        let end = Instant::now() + Duration::from_secs(5);
        loop {
            let remaining = end
                .checked_duration_since(Instant::now())
                .ok_or(Error::Io)?;
            let timeout = rustix::event::Timespec {
                tv_sec: remaining.as_secs() as i64,
                tv_nsec: remaining.subsec_nanos() as i64,
            };
            let mut polls = [rustix::event::PollFd::new(
                &input,
                rustix::event::PollFlags::IN,
            )];
            crate::store::io(rustix::event::poll(&mut polls, Some(&timeout)))?;
            if polls[0].revents().is_empty() {
                return Err(Error::Io);
            };
            let mut buffer = [0; 513];
            let n = input
                .read(&mut buffer[..(513 - raw.len()).min(513)])
                .map_err(|_| Error::Io)?;
            if n == 0 {
                break;
            };
            raw.extend_from_slice(&buffer[..n]);
            if raw.len() > 512 {
                return Err(Error::Bound);
            }
        }
        require(!raw.is_empty() && raw.len() <= 512)?;
        let secret = String::from_utf8(raw).map_err(|_| Error::Invalid)?;
        require(record::text(&secret, 512, true))?;
        Ok(Self { secret })
    }
}
pub(crate) struct Observation {
    pub(crate) plan: plan::Plan,
    pub(crate) work: Vec<Work>,
    pub(crate) journal: Option<record::Journal>,
    pub(crate) journal_snapshot: Snapshot,
    pub(crate) targets: Vec<native::Target>,
}
/// Inspection has no mutation lease. Consuming confirmation prevents reusing
/// this in-process prepared handle; durable replay refusal still belongs to the
/// journal/session checks, not to Rust moves.
/// ```compile_fail,E0382
/// use subyard_release_owner::owner::Prepared;
/// fn duplicate(p:Prepared<'_>){let _=p.confirm("plan", "grant");let _=p.confirm("plan", "grant");}
/// ```
/// ```
/// use subyard_release_owner::owner::Prepared;
/// fn inspect_twice(p:&Prepared<'_>){let _=p.inspection();let _=p.inspection();}
/// ```
pub struct Prepared<'a> {
    owner: &'a Owner,
    observation: Observation,
    grant: Grant,
    created: Instant,
    cleanup: Option<crate::owner_cleanup::Continuation>,
}
pub struct Consented<'a> {
    pub(crate) owner: &'a Owner,
    pub(crate) observation: Observation,
    pub(crate) grant: Grant,
}
impl<'a> Prepared<'a> {
    pub fn inspection(&self) -> p::Inspection {
        if let Some(cleanup) = &self.cleanup {
            return cleanup.inspection.clone();
        }
        self.owner.projected_inspection(&self.observation)
    }
    pub fn resume_admission(&self) -> Option<crate::migration_operation::ResumeAdmission> {
        self.cleanup
            .as_ref()
            .map(|c| crate::migration_operation::ResumeAdmission::checked_journal(&c.journal))
    }
    pub fn confirm(self, plan: &str, authorization: &str) -> Result<Consented<'a>> {
        require(self.cleanup.is_none())?;
        self.owner.store.reader.health()?;
        require(
            self.created.elapsed() <= Duration::from_secs(300)
                && (if let Some(j) = self.observation.journal.as_ref().filter(|j| {
                    self.observation.plan.wire().replacement.is_none()
                        && j.wire().checkpoint != "complete"
                }) {
                    plan == j.wire().resume_plan
                } else {
                    plan == self.observation.plan.token()
                        && (!self.observation.plan.wire().assessment.changed
                            || authorization == self.grant.secret)
                })
                && self
                    .observation
                    .plan
                    .wire()
                    .blockers
                    .as_ref()
                    .is_none_or(|b| b.is_empty()),
        )?;
        Ok(Consented {
            owner: self.owner,
            observation: self.observation,
            grant: self.grant,
        })
    }
}
impl Owner {
    pub fn open(
        options: Options,
        helper: File,
        custody_ingress: File,
        repository: File,
    ) -> Result<Self> {
        record::pair(&options.releases)?;
        // Only the compile-time disposable process fixture may omit physical
        // artifact tuples. The packaged entry has no environment bypass.
        if !cfg!(feature = "fixture-cuts") {
            require(
                options.source_artifact.is_some()
                    && options.target_artifact.is_some()
                    && options.previous_artifact.is_some() == options.releases.previous.is_some(),
            )?;
        }
        require(
            options
                .registry_raw
                .as_ref()
                .is_some_and(|raw| !raw.is_empty() && raw.len() <= codec::MAX_BYTES)
                && options.catalog.len() <= 256,
        )?;
        if let Some(target) = &options.target_artifact {
            require(options.candidate_version == target.version)?;
        }

        require(
            record::path(&options.data_home)
                && record::path(&options.config_home)
                && record::path(&options.runtime_root)
                && record::path(&options.repository_root)
                && record::safe_name(&options.yard)
                && record::fingerprint(&options.artifact_digest)
                && options.inherited_setting_ids.len() <= 256,
        )?;
        crate::store::checked(&repository, true, false, false)?;
        let reader = FrozenReader::capture_native_helper(helper)?;
        let catalog = record::Catalog::new(Some(options.catalog.clone()))?;
        require(catalog.digest() == crate::migration_owner::builtin_catalog(true, true)?.digest())?;
        let (registry, registry_digest) = record::Registry::from_frozen(
            &reader,
            options.registry_raw.as_deref().ok_or(Error::Invalid)?,
            &catalog,
        )?;
        let store = Store::open_with_reader(Path::new(&options.config_home), reader)?;
        let links = RuntimeLinks::open(Path::new(&options.runtime_root))?;
        let outer = crate::outer_custody::OuterCustody::capture(
            Path::new(&options.data_home),
            custody_ingress,
        )?;
        // Correlation identity is deterministic across frozen PV1 inspect and
        // converge processes. It is neither consent nor a replay capability.
        #[derive(Serialize)]
        struct Session<'a> {
            version: u8,
            artifact: &'a str,
            registry: &'a str,
            catalog: &'a str,
            helper: String,
            config: native::Identity,
            data: native::Identity,
            releases: &'a j::ReleasePair,
            yard: &'a str,
        }
        let image = match &store.reader {
            Reader::Frozen(r) => r.image_digest(),
            _ => return Err(Error::Invalid),
        };
        let mut session = digest(&codec::encode(
            &Session {
                version: 1,
                artifact: &options.artifact_digest,
                registry: &registry_digest,
                catalog: catalog.digest(),
                helper: image,
                config: native::Identity::capture(store.root.as_ref().ok_or(Error::Io)?)?,
                data: outer.identity(),
                releases: &options.releases,
                yard: &options.yard,
            },
            false,
        )?);
        if let Some((journal, _)) = store.read_checked_journal()? {
            if journal.wire().artifact_digest == options.artifact_digest
                && journal.wire().registry_digest == registry_digest
                && journal.wire().catalog_digest == catalog.digest()
                && store.has_bundle(&journal.wire().transaction)?
            {
                session = store.bundle(&journal, &registry)?.session;
            }
        }
        // Receipt-only restart retains the original correlation identity. This
        // does not grant execution: proposal/source/assets are checked again.
        if options.migration_sources.is_some() {
            for receipt in store.migration_owner_receipts()?.values() {
                let successor = record::Journal::new(receipt.wire().successor.clone())?;
                if successor.wire().artifact_digest == options.artifact_digest
                    && successor.wire().registry_digest == registry_digest
                    && successor.wire().catalog_digest == catalog.digest()
                    && store.has_bundle(&successor.wire().transaction)?
                    && store.read_checked_journal()?.is_some_and(|(j, _)| {
                        j.wire().transaction == receipt.wire().predecessor.transaction
                    })
                {
                    receipt.source_evidence(&store)?;
                    let bundle = store.bundle(&successor, &registry)?;
                    require(
                        bundle.helper
                            == match &store.reader {
                                Reader::Frozen(r) => r.image_digest(),
                                _ => return Err(Error::Invalid),
                            },
                    )?;
                    session = bundle.session;
                }
            }
        }
        let owner = Self {
            repository,
            first_cause: std::cell::RefCell::new(None),
            last_cause: std::cell::RefCell::new(None),
            outer,
            options,
            store,
            links,
            registry,
            catalog,
            registry_digest,
            session,
        };
        owner.verify_all_artifacts(false)?;
        let root = owner.store.root.as_ref().ok_or(Error::Io)?;
        let context = native::Context {
            session: owner.session.clone(),
            data_home: owner.options.data_home.clone(),
            releases: owner.options.releases.clone(),
            direction: if owner.options.rollback_target.is_some() {
                "activate-previous"
            } else {
                "activate-target"
            }
            .into(),
            source_ingress: owner.options.source_ingress.clone(),
            repository_root: owner.options.repository_root.clone(),
            config_home: owner.options.config_home.clone(),
            runtime_root: owner.options.runtime_root.clone(),
            yard: owner.options.yard.clone(),
            target: owner.options.releases.target.clone(),
            artifact: owner.options.artifact_digest.clone(),
            root: native::Identity::capture(root)?,
            repository: native::Identity::capture(&owner.repository)?,
        };
        owner.native(
            &N::Initialize { context },
            vec![
                crate::store::io(rustix::io::dup(root))?,
                crate::store::io(rustix::io::dup(&owner.repository))?,
            ],
        )?;
        Ok(owner)
    }
    pub fn process_capabilities(
        &self,
        kind: &str,
    ) -> Result<crate::wire::process_v2::RecoveryProcessResponse> {
        use crate::wire::process_v2::{RecoveryCapabilities, RecoveryProcessResponse};
        self.store.reader.health()?;
        let mut response = RecoveryProcessResponse {
            schema_version: 2,
            activation_reconciliation_owned: false,
            ..Default::default()
        };
        match kind {
            "capabilities" => {
                response.capabilities = Some(RecoveryCapabilities {
                    contract: crate::recovery::ACTIVATION_V1.into(),
                    process_schema: 2,
                    journal_schema: 2,
                    receipt_schema: 1,
                })
            }
            "lifecycle-capabilities" => {
                response.capabilities = Some(RecoveryCapabilities {
                    contract: crate::recovery::ACTIVATION_V2.into(),
                    process_schema: 2,
                    journal_schema: 2,
                    receipt_schema: 2,
                })
            }
            "runtime-activation-capabilities" => {
                let mut owners = self
                    .call(N::CaptureInventory {
                        session: self.session.clone(),
                    })?
                    .owners;
                owners.extend(
                    [
                        "materialized-config",
                        "core-guest-runtime",
                        "key-tools-runtime",
                        "test-vm-broker",
                        "test-yard-route-consumers",
                        "host-power",
                    ]
                    .map(str::to_owned),
                );
                owners.sort();
                require(
                    owners.len() <= 128
                        && owners.iter().all(|s| record::safe_id(s))
                        && owners.windows(2).all(|p| p[0] != p[1]),
                )?;
                response.activation_owners = Some(owners);
            }
            _ => return Err(Error::Invalid),
        }
        Ok(response)
    }
    pub fn custody_context(&self) -> CustodyContext {
        CustodyContext {
            session: self.session.clone(),
            data_root: self.outer.identity(),
        }
    }
    pub(crate) fn native(
        &self,
        request: &N,
        descriptors: Vec<std::os::fd::OwnedFd>,
    ) -> Result<native::Reply> {
        match &self.store.reader {
            Reader::Frozen(reader) => {
                let reply = reader.native(request, descriptors)?;
                self.last_cause.replace(reply.diagnostic.clone());
                if let Some(diagnostic) = &reply.diagnostic {
                    diagnostic.validate()?;
                    if !reply.code.is_empty() && self.first_cause.borrow().is_none() {
                        self.first_cause.replace(Some(diagnostic.clone()));
                    }
                }
                reply.checked()
            }
            Reader::CanonicalFixture => Err(Error::Invalid),
        }
    }
    pub(crate) fn call(&self, request: N) -> Result<native::Reply> {
        self.native(&request, vec![])
    }
    pub fn prepare(&self, goal: p::Goal, grant: Grant) -> Result<Prepared<'_>> {
        self.store.reader.health()?;
        require(self.options.migration_sources.is_none())?;
        let cleanup = self.prepare_owner_cleanup(&goal)?;
        let (observation, cleanup) = if let Some((observation, cleanup)) = cleanup {
            (observation, Some(cleanup))
        } else {
            (
                if let Some(selection) = &self.options.replacement {
                    self.replacement_observation(goal, selection)?
                } else {
                    self.observe(goal)?
                },
                None,
            )
        };
        self.store.reader.health()?;
        Ok(Prepared {
            owner: self,
            observation,
            grant,
            created: Instant::now(),
            cleanup,
        })
    }
    pub(crate) fn pre_v2_owner(&self) -> bool {
        self.options.rollback_target.as_ref().is_some_and(|t| {
            t.registry_digest.is_empty()
                && semver::Version::parse(&t.version)
                    .is_ok_and(|v| v < semver::Version::new(0, 11, 0))
        })
    }
    fn verify_artifact(
        &self,
        resource: &str,
        proof: &SourceArtifact,
        published: bool,
        phase: &mut crate::assets::VerificationPhase,
    ) -> Result<()> {
        if resource == "release.target-artifact" && self.pre_v2_owner() {
            let source = self
                .options
                .source_artifact
                .as_ref()
                .ok_or(Error::Invalid)?;
            require(
                proof.release == self.options.releases.target
                    && proof.artifact_digest == self.options.artifact_digest
                    && self
                        .options
                        .rollback_target
                        .as_ref()
                        .is_some_and(|t| t.version == proof.version)
                    && source.release == self.options.releases.from
                    && native::Identity::capture(&self.repository)? == source.root
                    && self.options.repository_root
                        == format!("{}/releases/{}", self.options.runtime_root, source.release),
            )?;
            let owner = phase.verify(
                &source.release,
                &crate::store::io(rustix::io::dup(&self.repository))?,
                &source.artifact_digest,
            )?;
            require(
                owner.get("bin/yard-engine") == Some(&source.engine_digest)
                    && source.engine_digest == self.helper_digest()
                    && owner.get("config/release-transition.json") == Some(&self.registry_digest),
            )?;
            self.links.source_artifact_in(proof, phase)?;
            let target = self.links.artifact_root(&proof.release)?;
            let entries = phase.verify(&proof.release, &target, &proof.artifact_digest)?;
            require(
                !entries.contains_key("config/release-transition.json")
                    && !entries.contains_key("config/release-owner.json"),
            )?;
            return Ok(());
        }
        if resource == "release.target-artifact" {
            require(
                proof.release == self.options.releases.target
                    && proof.artifact_digest == self.options.artifact_digest
                    && native::Identity::capture(&self.repository)? == proof.root
                    && record::semver(&proof.version),
            )?;
            let entries = phase.verify(
                &proof.release,
                &crate::store::io(rustix::io::dup(&self.repository))?,
                &proof.artifact_digest,
            )?;
            require(
                entries.get("bin/yard-engine") == Some(&proof.engine_digest)
                    && proof.engine_digest == self.helper_digest()
                    && entries.get("config/release-transition.json") == Some(&self.registry_digest)
                    && self.options.repository_root
                        == format!(
                            "{}/releases/{}",
                            self.options.runtime_root, self.options.releases.target
                        ),
            )?;
            if published {
                self.links.release_identity(&proof.release, &proof.root)?;
            }
            Ok(())
        } else {
            self.links.source_artifact_in(proof, phase)
        }
    }
    pub(crate) fn verify_all_artifacts(&self, published: bool) -> Result<()> {
        self.verify_artifacts_in(published, &mut crate::assets::VerificationPhase::new())
    }
    fn verify_artifacts_in(
        &self,
        published: bool,
        phase: &mut crate::assets::VerificationPhase,
    ) -> Result<()> {
        for (resource, proof, release) in [
            (
                "release.source-artifact",
                self.options.source_artifact.as_ref(),
                Some(self.options.releases.from.as_str()),
            ),
            (
                "release.target-artifact",
                self.options.target_artifact.as_ref(),
                Some(self.options.releases.target.as_str()),
            ),
            (
                "release.previous-artifact",
                self.options.previous_artifact.as_ref(),
                self.options.releases.previous.as_deref(),
            ),
        ] {
            if let Some(proof) = proof {
                require(Some(proof.release.as_str()) == release)?;
                self.verify_artifact(resource, proof, published, phase)?;
            }
        }
        Ok(())
    }
    fn config_layout_matches(&self, proof: &ConfigLayout) -> Result<bool> {
        use base64::Engine;
        require(
            matches!(proof.command_options_status.as_str(), "ok" | "unavailable")
                && proof
                    .command_options_raw
                    .as_ref()
                    .is_none_or(|r| r.len() <= 1 << 20)
                && proof
                    .command_manifest_digest
                    .as_ref()
                    .is_none_or(|s| record::fingerprint(s)),
        )?;
        let actual = self
            .call(N::CaptureConfigLayout {
                session: self.session.clone(),
            })?
            .layout
            .ok_or(Error::Invalid)?;
        let bytes = base64::engine::general_purpose::STANDARD
            .decode(&actual.raw)
            .map_err(|_| Error::Invalid)?;
        require(
            bytes.len() <= 1 << 20
                && base64::engine::general_purpose::STANDARD.encode(&bytes) == actual.raw
                && digest(&bytes) == actual.digest
                && (actual.exists || bytes.is_empty()),
        )?;
        match (&proof.manifest_raw, &proof.manifest_digest) {
            (None, None) => require(!actual.exists)?,
            (Some(raw), Some(hash)) => {
                require(actual.exists && bytes == *raw && digest(raw) == *hash)?
            }
            _ => return Err(Error::Invalid),
        }
        let Some(raw) = &proof.manifest_raw else {
            return Ok(true);
        };
        let manifest: LayoutManifest = self.store.decode(raw, false, false)?;
        let requires_cache = manifest.schema_version >= 2
            && manifest
                .files
                .iter()
                .flatten()
                .any(|f| f.path.starts_with(".sync/settings/"));
        let options =
            String::from_utf8_lossy(proof.command_options_raw.as_deref().unwrap_or_default());
        let local = options.split_whitespace().any(|s| s == "--local");
        let git = options.split_whitespace().any(|s| s == "--git");
        Ok(!requires_cache
            || proof.command_options_status == "ok"
                && proof.command_manifest_digest.is_some()
                && local
                && git)
    }
    pub(crate) fn verify_prepared_sources(&self, facts: &f::PlanFacts) -> Result<()> {
        // Every authority check starts another full physical verification;
        // only duplicate roles inside this check share its checked inventory.
        let mut phase = crate::assets::VerificationPhase::new();
        self.verify_source_reader(facts, &mut phase)?;
        if let Some(expected) = facts
            .observations
            .iter()
            .flatten()
            .find(|o| o.resource == "migration-owner.assets")
        {
            let (journal, _) = self.store.read_checked_journal()?.ok_or(Error::Invalid)?;
            let predecessor = if journal
                .wire()
                .transaction
                .starts_with("migration-owner-v1-")
            {
                self.store
                    .migration_owner_for(&journal)?
                    .ok_or(Error::Invalid)?
                    .wire()
                    .predecessor
                    .clone()
            } else {
                journal.wire().clone()
            };
            require(self.migration_assets(&predecessor)?.0 == expected.fingerprint)?;
        }

        for (resource, proof, release) in [
            (
                "release.source-artifact",
                self.options.source_artifact.as_ref(),
                Some(self.options.releases.from.as_str()),
            ),
            (
                "release.target-artifact",
                self.options.target_artifact.as_ref(),
                Some(self.options.releases.target.as_str()),
            ),
            (
                "release.previous-artifact",
                self.options.previous_artifact.as_ref(),
                self.options.releases.previous.as_deref(),
            ),
        ] {
            if let Some(expected) = facts
                .observations
                .iter()
                .flatten()
                .find(|o| o.resource == resource)
            {
                let proof = proof.ok_or(Error::Stale)?;
                require(
                    Some(proof.release.as_str()) == release
                        && expected.fingerprint == digest(&codec::encode(proof, false)?),
                )?;
                if resource == "release.target-artifact" {
                    require(proof.artifact_digest == self.options.artifact_digest)?;
                }
                self.verify_artifact(resource, proof, true, &mut phase)?;
            }
        }
        drop(phase);
        if let Some(expected) = facts
            .observations
            .iter()
            .flatten()
            .find(|o| o.resource == "release.rollback-runtime")
        {
            let (compatible, binding) = self.rollback_support()?;
            require(compatible && binding == expected.fingerprint)?;
        }
        if let Some(expected) = facts
            .observations
            .iter()
            .flatten()
            .find(|o| o.resource == "release.config-layout")
        {
            let proof = self.options.config_layout.as_ref().ok_or(Error::Stale)?;
            require(
                expected.fingerprint == digest(&codec::encode(proof, false)?)
                    && self.config_layout_matches(proof)?,
            )?;
        }
        Ok(())
    }
    fn verify_source_reader(
        &self,
        facts: &f::PlanFacts,
        phase: &mut crate::assets::VerificationPhase,
    ) -> Result<()> {
        if let Some(expected) = facts
            .observations
            .iter()
            .flatten()
            .find(|o| o.resource == "migration-history.source-reader")
        {
            let proof = self
                .options
                .source_checkpoint
                .as_ref()
                .ok_or(Error::Stale)?;
            require(
                proof.source_release == self.options.releases.from
                    && expected.fingerprint == digest(&codec::encode(proof, false)?),
            )?;
            self.links
                .source_checkpoint_in(proof, &self.store.reader, phase)?;
        }
        Ok(())
    }
    pub(crate) fn replacement_observation(
        &self,
        goal: p::Goal,
        request: &p::JournalReplacement,
    ) -> Result<Observation> {
        record::replacement(request)?;
        let mut observation = self.observe_inner(goal, false, true)?;
        let before = observation.journal.as_ref().ok_or(Error::Invalid)?;
        let j = before.wire();
        require(
            j.transaction == request.transaction
                && observation.journal_snapshot.fingerprint() == request.fingerprint,
        )?;
        self.native_quiescent(Some(j))?;
        require(!self.store.native_cleanup_pending()?)?;
        match request.reason.as_str() {
            "pre-activation-plan-stale" => {
                require(
                    matches!(j.checkpoint.as_str(), "authorized" | "migrating")
                        && observation.plan.wire().links.active == j.releases.from
                        && observation.plan.wire().links.previous == j.releases.previous,
                )?;
                for step in j.steps.iter().flatten() {
                    self.verify_recorded_step(j, step)?;
                }
                // A replacement needs an actual changed frozen binding, never a
                // second grant for the identical unfinished plan.
                require(
                    j.goal.target != observation.plan.wire().goal.target
                        || j.goal.direction != observation.plan.wire().goal.direction
                        || j.releases != observation.plan.wire().releases
                        || j.artifact_digest != self.options.artifact_digest
                        || j.registry_digest != self.registry_digest
                        || j.catalog_digest != self.catalog.digest()
                        || j.observation_scope != observation.plan.wire().observation_scope
                        || !same_intents(j, observation.plan.wire()),
                )?;
            }
            "post-activation-scope-v0.11.1" => {
                require(
                    request.source_version == "0.11.1"
                        && self.options.source_ingress.is_none()
                        && j.source_ingress.is_none()
                        && j.checkpoint == "reconciling"
                        && j.goal.direction == "activate-target"
                        && observation.plan.wire().goal.direction == "activate-target"
                        && j.registry_digest == self.registry_digest
                        && self.options.releases.from == j.releases.target
                        && self.options.releases.target != j.releases.target
                        && self.options.releases.previous.as_deref()
                            == Some(j.releases.from.as_str())
                        && crate::links::allowed(
                            "reconciling",
                            &observation.plan.wire().links,
                            &j.releases,
                        )
                        && observation.plan.wire().observation_scope != j.observation_scope
                        && observation.journal_snapshot.bytes()
                            == Some(codec::encode(j, true)?.as_slice()),
                )?;
                let source = self
                    .options
                    .source_artifact
                    .as_ref()
                    .ok_or(Error::Invalid)?;
                let target = self
                    .options
                    .target_artifact
                    .as_ref()
                    .ok_or(Error::Invalid)?;
                require(
                    source.version == "0.11.1"
                        && source.release == j.releases.target
                        && source.artifact_digest == j.artifact_digest
                        && semver::Version::parse(&target.version).map_err(|_| Error::Invalid)?
                            > semver::Version::new(0, 11, 1),
                )?;
                require(
                    j.catalog_digest == self.catalog.digest()
                        || j.catalog_digest
                            == "49e4c86efea0f1be557a43569728d9eb7f9df519379b0bdf45edd91ac5c93cf2"
                            && self
                                .catalog
                                .descriptors()
                                .iter()
                                .flatten()
                                .filter(|d| {
                                    matches!(
                                        d.kind.as_str(),
                                        "test-vms-settings-v1-to-v2" | "test-yard-owner-v1-to-v2"
                                    )
                                })
                                .count()
                                == 2,
                )?;
                self.store
                    .owner_predecessor_artifacts(before, &self.registry)?;
                require(j.steps.iter().flatten().next().is_some())?;
                let mut authority = self.registry.history().baseline();
                let mut expected = Snapshot::captured(None);
                for step in j.steps.iter().flatten() {
                    let edge = self
                        .registry
                        .history()
                        .next(&authority)
                        .map_err(|_| Error::Invalid)?;
                    let m = edge.migration();
                    require(
                        step.id == format!("{}.ledger", m.id)
                            && step.migration == m.id
                            && step.resource == format!("ledger.{}", m.domain)
                            && step.decision == "transform"
                            && step.checkpoint == "verified"
                            && step.expected == expected.fingerprint(),
                    )?;
                    let after = record::Ledger::from_authority(edge.after(), &self.registry)?;
                    let raw = codec::encode(after.wire(), true)?;
                    require(step.desired == digest(&raw))?;
                    self.verify_recorded_step(j, step)?;
                    expected = Snapshot::captured(Some(raw));
                    authority = edge.into_after();
                }
                self.registry
                    .history()
                    .complete(&authority)
                    .map_err(|_| Error::Invalid)?;
                require(
                    expected
                        == crate::ledger_execution::semantic_snapshot(
                            &self
                                .store
                                .migration_ledger(&self.registry, &self.registry_digest)?,
                        )?,
                )?;
            }
            _ => return Err(Error::Invalid),
        }
        require(
            observation
                .plan
                .wire()
                .blockers
                .iter()
                .flatten()
                .next()
                .is_none(),
        )?;
        let mut facts = observation.plan.wire().clone();
        facts.replacement = Some(request.clone());
        facts
            .observations
            .get_or_insert_with(Vec::new)
            .push(f::ResourceObservation {
                resource: "release.replacement-source".into(),
                class: "protected-source-v1".into(),
                fingerprint: self.store.source_metadata()?.binding().into(),
            });
        observation.plan = plan::Plan::new(facts)?;
        self.store.reader.health()?;
        Ok(observation)
    }
    pub(crate) fn observe(&self, goal: p::Goal) -> Result<Observation> {
        self.observe_inner(goal, false, false)
    }
    pub(crate) fn observe_recovery(&self, goal: p::Goal) -> Result<Observation> {
        self.observe_inner(goal, true, true)
    }
    pub(crate) fn observe_inner(
        &self,
        goal: p::Goal,
        fresh_activation: bool,
        ignore_pending: bool,
    ) -> Result<Observation> {
        require(goal.target == self.options.releases.target && record::direction(&goal.direction))?;
        require(!self.pre_v2_owner() || goal.direction == "activate-previous")?;
        let links = self.links.observe()?;
        let mut releases = if fresh_activation {
            j::ReleasePair {
                from: links.active.clone(),
                target: links.active.clone(),
                previous: links.previous.clone(),
            }
        } else {
            self.options.releases.clone()
        };
        let ledger = self
            .store
            .migration_ledger(&self.registry, &self.registry_digest)?;
        let journal_snapshot = self.store.read(&Address::journal())?;
        let journal = journal_snapshot
            .bytes()
            .map(|raw| {
                self.store
                    .decode(raw, true, false)
                    .and_then(record::Journal::new)
            })
            .transpose()?;
        if let Some(current) = journal.as_ref().filter(|j| {
            j.wire().checkpoint == "complete"
                && j.wire().goal.target == goal.target
                && j.wire().goal.direction == goal.direction
                && j.wire().artifact_digest == self.options.artifact_digest
        }) {
            let j = current.wire();
            releases = j.releases.clone();
            if releases.from != releases.target {
                releases.previous = Some(releases.from.clone());
                releases.from = releases.target.clone();
            }
        }
        if let Some(journal) = &journal {
            crate::graph::State::read(&self.store)?;
            self.store.recovery_for(journal)?;
        }
        if let Some(current) = journal
            .as_ref()
            .filter(|j| !fresh_activation && !ignore_pending && j.wire().checkpoint != "complete")
        {
            require(
                current.wire().goal.target == goal.target
                    && current.wire().goal.direction == goal.direction
                    && current.wire().releases == self.options.releases
                    && current.wire().registry_digest == self.registry_digest
                    && current.wire().catalog_digest == self.catalog.digest()
                    && current.wire().artifact_digest == self.options.artifact_digest
                    && crate::links::allowed(
                        &current.wire().checkpoint,
                        &links,
                        &current.wire().releases,
                    ),
            )?;
            let bundle = self.store.bundle(current, &self.registry)?;
            require(bundle.helper == self.helper_digest() && bundle.session == self.session)?;
            let plan = plan::Plan::new(bundle.facts)?;
            self.store.reader.health()?;
            return Ok(Observation {
                plan,
                work: bundle.work,
                journal,
                journal_snapshot,
                targets: bundle.targets,
            });
        }

        let mut initial_artifacts = crate::assets::VerificationPhase::new();
        self.verify_artifacts_in(false, &mut initial_artifacts)?;
        let source_reader = if let Some(proof) = &self.options.source_checkpoint {
            require(proof.source_release == self.options.releases.from)?;
            self.links
                .source_checkpoint_in(proof, &self.store.reader, &mut initial_artifacts)?;
            Some(digest(&codec::encode(proof, false)?))
        } else {
            None
        };
        if !fresh_activation
            && self.options.migration_sources.is_none()
            && ledger.checkpoint().is_some()
            && goal.direction == "activate-target"
            && source_reader.is_none()
        {
            let projection = ledger.projection().bytes().and_then(|raw| {
                self.store
                    .decode(raw, false, false)
                    .and_then(|v| record::Ledger::new(v, &self.registry))
                    .ok()
            });
            let same = projection
                .as_ref()
                .is_some_and(|p| p.authority().domains() == ledger.ledger().authority().domains());
            if !same
                || self
                    .registry
                    .history()
                    .complete(ledger.ledger().authority())
                    .is_err()
            {
                let facts=f::PlanFacts{goal,releases,links,artifact_digest:self.options.artifact_digest.clone(),registry_digest:self.registry_digest.clone(),catalog_digest:self.catalog.digest().into(),observation_scope:digest(b"blocked-source-checkpoint-reader"),assessment:plan::assess(false,false,&[])?,blockers:Some(vec![p::Blocker{code:"rollback-incompatible".into(),resource:"transition.source-checkpoint-compatibility".into(),message:"the current source release cannot verify the authoritative migration checkpoint".into(),retry:"install a verified checkpoint-reader bridge before advancing migrations, then run yard update --check".into()}]),..Default::default()};
                return Ok(Observation {
                    plan: plan::Plan::new(facts)?,
                    work: vec![],
                    journal,
                    journal_snapshot,
                    targets: vec![],
                });
            }
        }
        let mut source_blocker = None;
        let rollback_support = if goal.direction == "activate-previous" {
            // The native rollback observation is an independent capture.
            initial_artifacts = crate::assets::VerificationPhase::new();
            Some(self.rollback_support()?)
        } else {
            None
        };
        if rollback_support
            .as_ref()
            .is_some_and(|(compatible, _)| !*compatible)
        {
            source_blocker=Some(p::Blocker{code:"rollback-incompatible".into(),resource:"transition.rollback-runtime".into(),message:"the retained release cannot activate installed runtime contracts".into(),retry:"retain a release supporting the installed runtime owners, then run yard update --rollback".into()});
        }
        if let Some(proof) = &self.options.source_artifact {
            require(proof.release == self.options.releases.from)?;
            self.links
                .source_artifact_in(proof, &mut initial_artifacts)?;
            let version = semver::Version::parse(&proof.version).map_err(|_| Error::Invalid)?;
            if version < semver::Version::new(0, 11, 0)
                && journal
                    .as_ref()
                    .is_none_or(|j| j.wire().checkpoint == "complete")
            {
                source_blocker = Some(legacy_source_blocker());
            }
        }
        drop(initial_artifacts);
        if goal.direction == "activate-previous" {
            let target = self
                .options
                .rollback_target
                .as_ref()
                .ok_or(Error::Invalid)?;
            let version = semver::Version::parse(&target.version).map_err(|_| Error::Invalid)?;
            let fresh = journal.as_ref().is_none_or(|j| {
                j.wire().goal.target != goal.target || j.wire().goal.direction != goal.direction
            });
            let compatible = (!fresh || version >= semver::Version::new(0, 11, 0))
                && (!target.registry_digest.is_empty()
                    || self.registry.wire().migrations.iter().flatten().all(|m| {
                        self.catalog
                            .descriptors()
                            .iter()
                            .flatten()
                            .find(|d| d.kind == m.kind)
                            .is_some_and(|d| {
                                d.rollback_compatibility == "backward-compatible-no-op"
                                    && semver::Version::parse(&d.legacy_minimum_version)
                                        .is_ok_and(|minimum| version >= minimum)
                            })
                    }));
            if !compatible {
                source_blocker = Some(p::Blocker {
                    code: "rollback-incompatible".into(),
                    resource: "transition.rollback-compatibility".into(),
                    message:
                        "the retained release cannot safely accept the current migration ledger"
                            .into(),
                    retry: "install a compatible retained release, then run yard update --check"
                        .into(),
                });
            }
            if let Some(layout) = &self.options.config_layout {
                if !self.config_layout_matches(layout)? {
                    source_blocker=Some(p::Blocker{code:"rollback-incompatible".into(),resource:"transition.config-layout".into(),message:"the retained release cannot read the installed local-first Git configuration cache".into(),retry:"retain a release supporting local and Git configuration settings, then run yard update --rollback".into()});
                }
            }
        }
        if let Some(blocker) = source_blocker {
            let facts = f::PlanFacts {
                goal,
                releases,
                links,
                artifact_digest: self.options.artifact_digest.clone(),
                registry_digest: self.registry_digest.clone(),
                catalog_digest: self.catalog.digest().into(),
                rollback_target: self.options.rollback_target.clone(),
                observation_scope: digest(b"blocked-release-source"),
                assessment: plan::assess(false, false, &[])?,
                blockers: Some(vec![blocker]),
                ..Default::default()
            };
            return Ok(Observation {
                plan: plan::Plan::new(facts)?,
                work: vec![],
                journal,
                journal_snapshot,
                targets: vec![],
            });
        }
        let mut inventory = self.call(N::CaptureInventory {
            session: self.session.clone(),
        })?;
        let ingress = self
            .call(N::CaptureIngress {
                session: self.session.clone(),
                legacy_rust: true,
                binding: None,
            })?
            .ingress
            .ok_or(Error::Invalid)?;
        require(
            ingress
                .operations
                .iter()
                .all(|o| o.kind != "legacy-v1-import"),
        )?;
        inventory.yards = ingress.yards.clone();
        require(
            inventory.yards.len() <= 128
                && inventory.yards.windows(2).all(|a| a[0] < a[1])
                && inventory.yards.iter().all(|s| record::safe_name(s)),
        )?;
        let mut yards = Vec::new();
        for yard in &inventory.yards {
            yards.push(
                self.call(N::CaptureYard {
                    session: self.session.clone(),
                    yard: yard.clone(),
                    resolved: false,
                })?
                .settings
                .ok_or(Error::Invalid)?,
            )
        }
        let mut decisions = ingress.decisions;
        let mut observations = vec![f::ResourceObservation {
            resource: "native.yard-set".into(),
            class: "release-native-v1".into(),
            fingerprint: digest(&codec::encode(&inventory.yards, false)?),
        }];
        let mut blockers = ingress.blockers;
        let mut work = vec![];
        let mut ingress_rank = 0;
        for operation in ingress.operations {
            let (kind, rank) = match operation.kind.as_str() {
                "legacy-v1-import" => (crate::work::IngressKind::LegacyV1Import, 1),
                "import" => (crate::work::IngressKind::Import, 2),
                "entrypoints" => (crate::work::IngressKind::Entrypoints, 3),
                _ => return Err(Error::Invalid),
            };
            require(rank > ingress_rank)?;
            ingress_rank = rank;
            let w = Work::ingress(
                kind,
                operation.decision,
                operation.expected,
                operation.desired,
                operation.static_pin,
            )?;
            let (_, _, _, _, class) = kind.labels();
            observations.push(f::ResourceObservation {
                resource: w.intent.resource.clone(),
                class: class.into(),
                fingerprint: w.intent.expected.clone(),
            });
            work.push(w);
        }
        let legacy_git = yards.iter().flat_map(|y| &y.sources).any(|s| {
            s.label.contains(".sync/settings/")
                && s.assignments.iter().any(|a| a.name == "SSH_PORT")
        });
        if legacy_git {
            let mut ports = BTreeSet::new();
            for y in &yards {
                if y.access == "local" && y.ssh_port != 0 {
                    require(ports.insert(y.ssh_port))?;
                }
            }
        }
        let mut authority = ledger.ledger().authority().clone();
        let mut before = ledger_execution::semantic_snapshot(&ledger)?;
        let pending = self
            .registry
            .history()
            .pending(&authority)
            .map_err(|_| Error::Invalid)?
            .into_iter()
            .map(|m| m.id.clone())
            .collect::<Vec<_>>();
        for id in pending {
            let migration = self
                .registry
                .wire()
                .migrations
                .iter()
                .flatten()
                .find(|m| m.id == id)
                .ok_or(Error::Invalid)?;
            match migration.kind.as_str() {
                "local-ssh-port-settings-v2-to-v3" | "test-vms-settings-v1-to-v2" => {
                    let mut migration_decisions = Vec::new();
                    for original in &yards {
                        let mut input = self
                            .call(N::CaptureYard {
                                session: self.session.clone(),
                                yard: original.yard.clone(),
                                resolved: migration.kind == "local-ssh-port-settings-v2-to-v3",
                            })?
                            .settings
                            .ok_or(Error::Invalid)?;
                        let local = migration.kind == "local-ssh-port-settings-v2-to-v3";
                        let inheritance = if local {
                            ssh_inheritance(&input)?
                        } else {
                            let (inherited, pin) =
                                template_inheritance(&input, &self.options.inherited_setting_ids)?;
                            input.inherited = inherited;
                            pin
                        };
                        let conversion = if local {
                            settings::local_ssh_conversion(&input, legacy_git)
                        } else {
                            settings::test_vm_conversion(&input)
                        };
                        let conversion = match conversion {
                            Ok(v) => v,
                            Err(_) => {
                                blockers.push(blocker(
                                    "precondition-blocked",
                                    &format!("yard.{}", input.yard),
                                    "persistent settings cannot be migrated safely",
                                ));
                                continue;
                            }
                        };
                        if let Some(conversion) = conversion {
                            migration_decisions.extend(conversion.decisions);
                            if conversion.edits.is_empty() {
                                continue;
                            }
                            let prepared = self
                                .call(N::PrepareSettings {
                                    session: self.session.clone(),
                                    yard: input.yard.clone(),
                                    edits: conversion.edits.clone(),
                                })?
                                .recipe
                                .ok_or(Error::Invalid)?;
                            require(prepared.yard == input.yard)?;
                            let expected = settings::resource_fingerprint(
                                prepared.expected_exists,
                                &prepared.expected_file,
                                &inheritance,
                            )?;
                            let desired = settings::resource_fingerprint(
                                true,
                                &prepared.desired_file,
                                &inheritance,
                            )?;
                            let recipe = crate::work::SettingsRecipe {
                                yard: prepared.yard,
                                target: prepared.target,
                                inheritance,
                                expected: expected.clone(),
                                desired,
                                expected_exists: prepared.expected_exists,
                                expected_file: prepared.expected_file,
                                desired_file: prepared.desired_file,
                                edits: conversion.edits,
                                before: conversion.before,
                                source: prepared.source,
                                inputs: prepared.inputs,
                            };
                            observations.push(f::ResourceObservation {
                                resource: format!("yard.{}", input.yard),
                                class: if local {
                                    "local-ssh-port-settings-v1"
                                } else {
                                    "yard-settings-file-v1"
                                }
                                .into(),
                                fingerprint: expected,
                            });
                            work.push(Work::settings(
                                migration,
                                recipe,
                                local,
                                &conversion.decision,
                            )?);
                        }
                    }
                    migration_decisions
                        .sort_by(|a, b| (&a.scope, &a.resource).cmp(&(&b.scope, &b.resource)));
                    decisions.extend(migration_decisions);
                }
                "test-yard-owner-v1-to-v2" => {
                    require(
                        migration.domain == "owner-registration"
                            && migration.from_epoch == 1
                            && migration.to_epoch == 2,
                    )?;
                    let owner = self
                        .call(N::CaptureOwner {
                            session: self.session.clone(),
                        })?
                        .owner
                        .ok_or(Error::Invalid)?;
                    owner.validate()?;
                    if !matches!(owner.state.as_str(), "absent" | "current") {
                        let expected = owner.fingerprint()?;
                        let desired = owner.desired().fingerprint()?;
                        let intent = f::PlannerStepIntent {
                            id: format!("{}.test-yard-owner", migration.id),
                            migration: migration.id.clone(),
                            resource: "test-yard-owner".into(),
                            decision: "canonicalize".into(),
                            expected: expected.clone(),
                            desired,
                        };
                        decisions.push(p::RedactedDecision {
                            resource: "test-yard-owner".into(),
                            scope: "owner-registration".into(),
                            decision: "canonicalize".into(),
                            result: "test-yard".into(),
                        });
                        observations.push(f::ResourceObservation {
                            resource: "owner-registration.test-yard-owner".into(),
                            class: "test-yard-owner-v1".into(),
                            fingerprint: expected,
                        });
                        work.push(Work {
                            intent,
                            recipe: Recipe::TestYardOwnerRegistration(owner),
                        });
                    }
                }
                "owner-publication-state-v1" => {
                    require(
                        migration.domain == "power-metadata"
                            && migration.from_epoch == 1
                            && migration.to_epoch == 2,
                    )?;
                    decisions.push(p::RedactedDecision {
                        resource: "power-metadata".into(),
                        scope: "power-metadata".into(),
                        decision: "transform".into(),
                        result: "optional-publication-reader-v1".into(),
                    });
                }
                _ => {
                    blockers.push(blocker(
                        "unsupported-kind",
                        &format!("migration.{}", migration.id),
                        "the pending migration kind is not compiled into this engine",
                    ));
                    continue;
                }
            }
            let edge = self
                .registry
                .history()
                .next(&authority)
                .map_err(|_| Error::Invalid)?;
            require(edge.migration().id == migration.id)?;
            let after = record::Ledger::from_authority(edge.after(), &self.registry)?;
            let bytes = codec::encode(after.wire(), true)?;
            let intent = f::PlannerStepIntent {
                id: format!("{}.ledger", migration.id),
                migration: migration.id.clone(),
                resource: format!("ledger.{}", migration.domain),
                decision: "transform".into(),
                expected: before.fingerprint(),
                desired: digest(&bytes),
            };
            observations.push(f::ResourceObservation {
                resource: intent.resource.clone(),
                class: "domain-ledger-v2".into(),
                fingerprint: intent.expected.clone(),
            });
            work.push(Work {
                intent,
                recipe: Recipe::LedgerAdvance {
                    before: before.clone(),
                    after: bytes.clone(),
                },
            });
            before = Snapshot::captured(Some(bytes));
            authority = edge.into_after();
        }
        if let Some(current) = journal.as_ref().filter(|j| {
            j.wire().goal.target == goal.target
                && j.wire().goal.direction == goal.direction
                && j.wire().artifact_digest == self.options.artifact_digest
                && j.wire().registry_digest == self.registry_digest
                && j.wire().catalog_digest == self.catalog.digest()
        }) {
            if self.store.has_bundle(&current.wire().transaction)? {
                let original = self.store.bundle(current, &self.registry)?;
                require(
                    original.session == self.session && original.helper == self.helper_digest(),
                )?;
                if self.restore_owner_results(current.wire(), &original, None)? {
                    self.project_owner_activation(&original.work)?;
                    self.restore_after_owner(current.wire(), &original)?;
                }
            }
        }
        self.project_owner_activation(&work)?;
        let mut targets = vec![];
        let mut activation_scope = vec![];
        let mut consequences = vec![];
        let mut owners = inventory.owners;
        owners.extend(
            [
                "materialized-config",
                "core-guest-runtime",
                "key-tools-runtime",
                "test-vm-broker",
                "test-yard-route-consumers",
                "host-power",
            ]
            .map(str::to_string),
        );
        require(owners.len() <= 256 && owners.iter().all(|id| record::safe_id(id)))?;
        let mut seen = BTreeSet::new();
        require(owners.iter().all(|id| seen.insert(id.clone())))?;
        for owner in owners.drain(..) {
            let mut current = match self.call(N::CaptureActivation {
                session: self.session.clone(),
                owner: owner.clone(),
                fresh: false,
            }) {
                Ok(reply) => reply.targets,
                Err(error) => {
                    self.store.reader.health()?;
                    if !matches!(error, Error::Io | Error::Stale | Error::Unknown) {
                        return Err(error);
                    }
                    let cause = self.last_cause.borrow();
                    let cause = cause.as_ref();
                    let conflict = cause.is_some_and(|d| d.kind == "component-conflict");
                    let mut item = blocker(
                        if conflict {
                            "activation-ambiguous"
                        } else {
                            "dependency-unavailable"
                        },
                        &format!("activation.{owner}"),
                        &format!("activation reconciler \"{owner}\" cannot be observed safely"),
                    );
                    if let Some(cause) = cause.filter(|d| record::text(&d.message, 512, true)) {
                        item.message = cause.message.clone();
                        if record::action(&cause.retry) {
                            item.retry = cause.retry.clone();
                        }
                    }
                    blockers.push(item);
                    continue;
                }
            };
            if owner == "materialized-config" {
                let all_local = if self.options.source_ingress.is_some() {
                    false
                } else if releases.from != releases.target {
                    self.registry
                        .history()
                        .complete(ledger.ledger().authority())
                        .is_ok()
                } else {
                    journal.as_ref().is_none_or(|j| {
                        j.wire().goal.target == goal.target
                            && j.wire().goal.direction == goal.direction
                            && j.wire().artifact_digest == self.options.artifact_digest
                    })
                };
                if !all_local {
                    current.retain(|t| t.yard == self.options.yard);
                }
                current.sort_by(|a, b| {
                    a.yard.cmp(&b.yard).then_with(|| {
                        (a.class != "materialized-integrations")
                            .cmp(&(b.class != "materialized-integrations"))
                    })
                });
            }
            for target in &current {
                target.validate()?;
                require(target.owner == owner)?;
            }
            let assessed = crate::activation::assess(
                &owner,
                &self.options.releases.target,
                &self.options.artifact_digest,
                &links.active,
                &current,
                self.registry
                    .history()
                    .complete(ledger.ledger().authority())
                    .is_ok(),
            )?;
            let desired = assessed.desired;
            let converged = assessed.converged;
            if !converged {
                decisions.push(p::RedactedDecision {
                    resource: format!("activation.{owner}"),
                    scope: "activation".into(),
                    decision: "canonicalize".into(),
                    result: "converged".into(),
                });
                consequences.push(format!("refresh {owner}"));
            }
            let actual = assessed.actual;
            observations.push(activation_observation(&owner, &actual, &desired)?);
            activation_scope.push(ActivationScope { id: owner, desired });
            targets.extend(current);
        }
        require(
            work.len()
                .checked_add(targets.len())
                .is_some_and(|n| n <= 2048),
        )?;
        self.after_owner_preflight(&targets, &work)?;
        // Rehash all actual release bytes after native capture. Never reuse
        // the initial inventories across that capture boundary.
        let mut final_artifacts = crate::assets::VerificationPhase::new();
        for (resource, proof, release) in [
            (
                "release.source-artifact",
                self.options.source_artifact.as_ref(),
                Some(self.options.releases.from.as_str()),
            ),
            (
                "release.target-artifact",
                self.options.target_artifact.as_ref(),
                Some(self.options.releases.target.as_str()),
            ),
            (
                "release.previous-artifact",
                self.options.previous_artifact.as_ref(),
                self.options.releases.previous.as_deref(),
            ),
        ] {
            if let Some(proof) = proof {
                require(Some(proof.release.as_str()) == release)?;
                if resource == "release.target-artifact" {
                    require(proof.artifact_digest == self.options.artifact_digest)?;
                }
                self.verify_artifact(resource, proof, false, &mut final_artifacts)?;
                observations.push(f::ResourceObservation {
                    resource: resource.into(),
                    class: "verified-release-v1".into(),
                    fingerprint: digest(&codec::encode(proof, false)?),
                });
            }
        }
        if let Some(proof) = &self.options.config_layout {
            observations.push(f::ResourceObservation {
                resource: "release.config-layout".into(),
                class: "rollback-config-layout-v1".into(),
                fingerprint: digest(&codec::encode(proof, false)?),
            });
        }
        if let Some((_, fingerprint)) = rollback_support {
            observations.push(f::ResourceObservation {
                resource: "release.rollback-runtime".into(),
                class: "retained-runtime-leaves-v1".into(),
                fingerprint,
            });
        }
        if let Some(fingerprint) = source_reader {
            observations.push(f::ResourceObservation {
                resource: "migration-history.source-reader".into(),
                class: "verified-source-checkpoint-v1".into(),
                fingerprint,
            });
        }
        observations.push(f::ResourceObservation {
            resource: "native.prepared-recipes".into(),
            class: "release-native-v1".into(),
            fingerprint: crate::bundle::binding(&work, &targets)?,
        });
        let (scope, checkpoint_consent) = current_observation_scope(
            &activation_scope,
            &self.options.inherited_setting_ids,
            journal
                .as_ref()
                .filter(|_| !fresh_activation)
                .map(|j| j.wire()),
            self.options.migration_checkpoint,
            &ledger.authority().fingerprint(),
        )?;
        let checkpoint_work = self.options.migration_checkpoint
            && (journal
                .as_ref()
                .is_none_or(|j| j.wire().checkpoint == "complete")
                || checkpoint_consent
                || fresh_activation)
            && (ledger.checkpoint().is_none()
                || ledger.checkpoint().is_some_and(|c| {
                    c.wire()
                        .domains
                        .iter()
                        .flat_map(|d| d.values())
                        .any(|d| d.applied_suffix.iter().flatten().next().is_some())
                })
                || !work.is_empty());
        if checkpoint_work {
            decisions.push(p::RedactedDecision {
                resource: "migration-history.checkpoint".into(),
                scope: "migration-history".into(),
                decision: "transform".into(),
                result: "compact-migration-history".into(),
            });
            consequences.push("compact completed migration history checkpoint".into());
        }
        if self.options.migration_checkpoint || ledger.checkpoint().is_some() {
            observations.push(f::ResourceObservation {
                resource: "migration-history.checkpoint".into(),
                class: "migration-history-checkpoint-v1".into(),
                fingerprint: self.store.read(&Address::checkpoint())?.fingerprint(),
            });
        }
        let changed = !work.is_empty()
            || links.active != goal.target
            || targets.iter().any(|t| t.state == "stale")
            || checkpoint_work;
        let assessment = plan::assess(
            changed,
            self.options
                .rollback_target
                .as_ref()
                .is_some_and(|r| r.registry_digest.is_empty()),
            &consequences,
        )?;
        let intents = work.iter().map(|w| w.intent.clone()).collect();
        let facts = f::PlanFacts {
            goal,
            releases,
            links,
            artifact_digest: self.options.artifact_digest.clone(),
            registry_digest: self.registry_digest.clone(),
            catalog_digest: self.catalog.digest().into(),
            rollback_target: self.options.rollback_target.clone(),
            observation_scope: scope,
            assessment,
            decisions: Some(decisions),
            observations: Some(observations),
            intents: Some(intents),
            blockers: Some(blockers),
            replacement: None,
        };
        self.store.reader.health()?;
        Ok(Observation {
            plan: plan::Plan::new(facts)?,
            work,
            journal,
            journal_snapshot,
            targets,
        })
    }
}
#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
pub struct CustodyContext {
    session: String,
    data_root: native::Identity,
}
#[derive(Clone, Serialize)]
struct ActivationScope {
    id: String,
    desired: String,
}
fn observation_scope(
    activation: &[ActivationScope],
    inherited: &[String],
    checkpoint: bool,
) -> Result<String> {
    #[derive(Serialize)]
    #[serde(rename_all = "camelCase")]
    struct Scope<'a> {
        schema_version: i64,
        activation_reconcilers: &'a [ActivationScope],
        inherited_setting_ids: Vec<String>,
        #[serde(skip_serializing_if = "std::ops::Not::not")]
        migration_checkpoint: bool,
    }
    let mut inherited = inherited.to_vec();
    inherited.sort();
    inherited.dedup();
    require(inherited.iter().all(|s| record::safe_id(s)))?;
    Ok(digest(&codec::encode(
        &Scope {
            schema_version: 1,
            activation_reconcilers: activation,
            inherited_setting_ids: inherited,
            migration_checkpoint: checkpoint,
        },
        false,
    )?))
}
fn current_observation_scope(
    activation: &[ActivationScope],
    inherited: &[String],
    journal: Option<&j::Record>,
    prefer: bool,
    source: &str,
) -> Result<(String, bool)> {
    let base = observation_scope(activation, inherited, false)?;
    let mut marked = activation.to_vec();
    if !source.is_empty() {
        require(record::fingerprint(source))?;
        marked.push(ActivationScope {
            id: "migration-history-source".into(),
            desired: source.into(),
        });
    }
    let marked = observation_scope(&marked, inherited, true)?;
    Ok(match journal {
        Some(j) if j.observation_scope == marked => (marked, true),
        Some(j) if j.observation_scope == base && (j.checkpoint != "complete" || !prefer) => {
            (base, false)
        }
        Some(_) => (if prefer { marked } else { base }, false),
        None => (if prefer { marked } else { base }, prefer),
    })
}
fn activation_observation(id: &str, actual: &str, desired: &str) -> Result<f::ResourceObservation> {
    #[derive(Serialize)]
    struct Value<'a> {
        id: &'a str,
        actual: &'a str,
        desired: &'a str,
    }
    Ok(f::ResourceObservation {
        resource: format!("activation.{id}"),
        class: "activation-reconciler-v1".into(),
        fingerprint: digest(&codec::encode(
            &Value {
                id,
                actual,
                desired,
            },
            false,
        )?),
    })
}
fn blocker(code: &str, resource: &str, message: &str) -> p::Blocker {
    p::Blocker {
        code: code.into(),
        resource: resource.into(),
        message: message.into(),
        retry: "run yard update --check".into(),
    }
}
fn legacy_source_blocker() -> p::Blocker {
    p::Blocker {
        code: "precondition-blocked".into(),
        resource: "transition.source".into(),
        message: "Sources before v0.11.0 are unsupported.".into(),
        retry: "Stage verified current tooling with subyard-install.sh --version <version> --publish-only, then run the printed candidate's bin/yard uninstall --full. Full uninstall deletes this installation's owned data. Review and confirm its removal plan. After verified cleanup, install a new version separately.".into(),
    }
}

fn ssh_inheritance(yard: &settings::YardSettings) -> Result<String> {
    let mut inputs = BTreeMap::new();
    for source in &yard.sources {
        if source.role.contains("scalar settings") {
            inputs.insert(
                source.label.clone(),
                settings::snapshot_fingerprint(source.exists, &source.sha256)?,
            );
        }
    }
    Ok(digest(&codec::encode(&inputs, false)?))
}
fn template_inheritance(
    yard: &settings::YardSettings,
    command: &[String],
) -> Result<(BTreeSet<String>, String)> {
    #[derive(Serialize)]
    struct Resource<'a> {
        role: &'a str,
        settings: Vec<String>,
    }
    #[derive(Serialize)]
    #[serde(rename_all = "camelCase")]
    struct Value<'a> {
        schema_version: i64,
        resources: Vec<Resource<'a>>,
        command_setting_ids: Vec<String>,
    }
    let affected = |name: &str| matches!(name, "YARD_TEMPLATE" | "NESTED_E2E_VMS");
    let mut inherited = BTreeSet::new();
    let mut resources = vec![];
    for role in ["shared", "host"] {
        let source = yard
            .sources
            .iter()
            .find(|s| s.role == role)
            .ok_or(Error::Invalid)?;
        let names = source
            .assignments
            .iter()
            .filter(|a| affected(&a.name))
            .map(|a| a.name.clone())
            .collect::<BTreeSet<_>>();
        inherited.extend(names.iter().cloned());
        resources.push(Resource {
            role,
            settings: names.into_iter().collect(),
        });
    }
    let ids = command
        .iter()
        .filter(|s| affected(s))
        .cloned()
        .collect::<BTreeSet<_>>();
    inherited.extend(ids.iter().cloned());
    Ok((
        inherited,
        digest(&codec::encode(
            &Value {
                schema_version: 1,
                resources,
                command_setting_ids: ids.into_iter().collect(),
            },
            false,
        )?),
    ))
}

fn same_intents(j: &j::Record, f: &f::PlanFacts) -> bool {
    let steps = j.steps.as_deref().unwrap_or_default();
    let intents = f.intents.as_deref().unwrap_or_default();
    steps.len() == intents.len()
        && steps.iter().zip(intents).all(|(s, i)| {
            s.id == i.id
                && s.migration == i.migration
                && s.resource == i.resource
                && s.decision == i.decision
                && s.expected == i.expected
                && s.desired == i.desired
        })
}

#[cfg(test)]
mod source_guidance_tests {
    #[test]
    fn legacy_source_guidance_keeps_full_uninstall_and_clean_install_direction() {
        let blocker = super::legacy_source_blocker();
        assert_eq!(blocker.code, "precondition-blocked");
        assert_eq!(blocker.resource, "transition.source");
        assert!(crate::record::action(&blocker.retry));
        for text in [
            "--publish-only",
            "bin/yard uninstall --full",
            "owned data",
            "confirm",
            "install a new version separately",
        ] {
            assert!(blocker.retry.contains(text));
        }
    }
}
