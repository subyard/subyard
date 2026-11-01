//! Closed component facts/recipes and late descriptor custody transfer.
//! This transport never asks Go to inspect, plan or reduce a release transaction.
use crate::{
    record::{self, require},
    settings::YardSettings,
    work::{Edit, OwnerBefore, SettingsRecipe},
    Error, Result,
};
use serde::{Deserialize, Serialize};
use std::os::fd::AsFd;
#[derive(Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct Identity {
    pub device: u64,
    pub inode: u64,
    pub mode: u32,
    pub uid: u32,
    pub links: u64,
}
#[derive(Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "PascalCase", deny_unknown_fields)]
pub(crate) struct FileIdentity {
    pub device: u64,
    pub inode: u64,
    pub mode: u32,
    #[serde(rename = "UID")]
    pub uid: u32,
    #[serde(rename = "GID")]
    pub gid: u32,
    pub links: u64,
}
#[derive(Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub(crate) struct InputBinding {
    pub relative: String,
    pub exists: bool,
    pub digest: String,
    pub identity: FileIdentity,
}
impl Identity {
    pub(crate) fn capture(fd: &impl AsFd) -> Result<Self> {
        let s = crate::store::io(rustix::fs::fstat(fd))?;
        // Linux libc layouts expose nlink_t at different widths on our two
        // supported architectures; the wire identity always uses u64.
        #[allow(clippy::useless_conversion)]
        let links = u64::from(s.st_nlink);
        Ok(Self {
            device: s.st_dev,
            inode: s.st_ino,
            mode: s.st_mode,
            uid: s.st_uid,
            links,
        })
    }
}
#[derive(Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub(crate) struct Context {
    pub session: String,
    pub data_home: String,
    pub releases: crate::wire::journal_v2::ReleasePair,
    pub direction: String,
    pub source_ingress: Option<crate::wire::journal_v2::SourceIngress>,
    pub repository_root: String,
    pub config_home: String,
    pub runtime_root: String,
    pub yard: String,
    pub target: String,
    pub artifact: String,
    pub root: Identity,
    pub repository: Identity,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub inherited_setting_ids: Vec<String>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub command_overrides: Vec<CommandOverride>,
}
#[derive(Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct CommandOverride {
    pub name: String,
    pub present: bool,
    #[serde(with = "crate::wire::go_bytes")]
    pub value: Option<Vec<u8>>,
}
#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub(crate) struct Binding {
    pub session: String,
    pub transaction: String,
    pub plan: String,
    pub journal: String,
    pub artifact: String,
    pub helper: String,
    pub ordinal: u64,
}
impl Binding {
    pub(crate) fn validate(&self) -> Result<()> {
        require(
            record::fingerprint(&self.session)
                && record::safe_id(&self.transaction)
                && record::token(&self.plan, "plan-v1-")
                && [&self.journal, &self.artifact, &self.helper]
                    .into_iter()
                    .all(|s| record::fingerprint(s))
                && self.ordinal > 0
                && self.ordinal <= 4096,
        )
    }
}
#[derive(Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub(crate) struct Target {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub prepared_recipe: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub after_owner: String,
    pub scope: String,
    pub reported_actual: String,
    pub reported_desired: String,
    pub before: String,
    pub class: String,
    pub scope_state: String,
    pub stage: String,
    pub owner: String,
    pub yard: String,
    pub project: String,
    pub instance: String,
    pub instance_uuid: String,
    pub state: String,
    pub actual: String,
    pub desired: String,
    pub source: String,
}
impl Target {
    pub(crate) fn materialized_order(&self, other: &Self) -> std::cmp::Ordering {
        self.yard.cmp(&other.yard).then_with(|| {
            (self.class != "materialized-files").cmp(&(other.class != "materialized-files"))
        })
    }
    // Only after the exact original recipe binder has checked captured facts.
    pub(crate) fn original_integration_observation(&self, original: &Self) -> bool {
        if self.validate().is_err()
            || original.validate().is_err()
            || original.owner != "materialized-config"
            || original.class != "materialized-integrations"
            || !original.after_owner.is_empty()
            || original.state != "stale"
            || original.instance_uuid == "absent"
            || original.prepared_recipe.is_empty()
            || !matches!(self.state.as_str(), "current" | "stale")
        {
            return false;
        }
        let mut observed = self.clone();
        observed.state = original.state.clone();
        observed.actual = original.actual.clone();
        observed.reported_actual = original.reported_actual.clone();
        observed.before = original.before.clone();
        observed.prepared_recipe = original.prepared_recipe.clone();
        observed == *original
    }
    pub(crate) fn materialized_current(&self) -> bool {
        self.validate().is_ok()
            && self.owner == "materialized-config"
            && self.class == "materialized-files"
            && self.after_owner.is_empty()
            && !self.prepared_recipe.is_empty()
            && self.state != "deferred"
            && self.instance_uuid != "absent"
    }
    // Used only after binding the retained ORIGINAL recipe/target. The native
    // binder requires original physical custody for a mixed current state.
    pub(crate) fn materialized_current_observation(&self, original: &Self) -> bool {
        if !self.materialized_current() || !original.materialized_current() {
            return false;
        }
        let mut comparison = self.clone();
        comparison.state = original.state.clone();
        comparison.actual = original.actual.clone();
        comparison.reported_actual = original.reported_actual.clone();
        comparison.prepared_recipe = original.prepared_recipe.clone();
        comparison == *original
    }
    pub(crate) fn materialized_after_owner(&self) -> bool {
        self.validate().is_ok()
            && self.owner == "materialized-config"
            && self.class == "materialized-files"
            && self.state != "deferred"
            && self.instance_uuid != "absent"
            && matches!(
                crate::after_owner::AfterOwnerActivation::parse(&self.after_owner),
                Ok((
                    crate::after_owner::AfterOwnerActivation::MaterializedFiles,
                    _
                ))
            )
    }
    pub(crate) fn materialized_after_owner_observation(&self, original: &Self) -> bool {
        let eligible = |target: &Self| {
            target.materialized_after_owner()
                || (target.validate().is_ok()
                    && target.owner == "materialized-config"
                    && target.class == "materialized-integrations"
                    && target.state != "deferred"
                    && target.instance_uuid != "absent"
                    && matches!(
                        crate::after_owner::AfterOwnerActivation::parse(&target.after_owner),
                        Ok((
                            crate::after_owner::AfterOwnerActivation::MaterializedIntegrations,
                            _
                        ))
                    ))
        };
        if !eligible(self) || !eligible(original) {
            return false;
        }
        let mut comparison = self.clone();
        comparison.state = original.state.clone();
        comparison.actual = original.actual.clone();
        comparison.reported_actual = original.reported_actual.clone();
        comparison == *original
    }
    pub(crate) fn resolved(&self, targets: &[Self]) -> Result<Self> {
        require(targets.len() == 1)?;
        let resolved = &targets[0];
        resolved.validate()?;
        let mut expected = self.clone();
        if resolved.instance_uuid != self.instance_uuid {
            require(
                self.instance_uuid == "absent"
                    && resolved.instance_uuid != "absent"
                    && record::safe_id(&resolved.instance_uuid),
            )?;
            expected.instance_uuid = resolved.instance_uuid.clone();
        }
        require(*resolved == expected)?;
        Ok(resolved.clone())
    }
    pub(crate) fn validate(&self) -> Result<()> {
        if !self.after_owner.is_empty() {
            crate::after_owner::AfterOwnerActivation::parse(&self.after_owner)?;
        }
        require(
            (self.prepared_recipe.is_empty() || record::fingerprint(&self.prepared_recipe))
                && record::safe_id(&self.owner)
                && record::safe_name(&self.yard)
                && record::safe_name(&self.project)
                && record::safe_name(&self.instance)
                && record::text(&self.instance_uuid, 128, true)
                && matches!(
                    self.state.as_str(),
                    "absent" | "stopped" | "current" | "stale" | "deferred"
                )
                && [&self.actual, &self.desired, &self.source]
                    .into_iter()
                    .all(|s| record::fingerprint(s)),
        )
    }
}
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub(crate) struct CompleteSourceReference {
    pub transaction: String,
    pub journal_digest: String,
    pub metadata_binding: String,
}
#[derive(Clone, Serialize)]
#[serde(tag = "op", rename_all = "kebab-case")]
pub(crate) enum Request {
    Initialize {
        context: Context,
    },
    CaptureInventory {
        session: String,
    },
    CaptureConfigLayout {
        session: String,
    },
    CaptureRollbackSupport {
        session: String,
        #[serde(rename = "rollbackRepositories")]
        repositories: RollbackRepositories,
        #[serde(rename = "targetOwners")]
        target_owners: Vec<String>,
    },
    CaptureIngress {
        session: String,
        #[serde(rename = "legacyRust")]
        legacy_rust: bool,
        #[serde(rename = "bindingIngress")]
        binding: Option<IngressBinding>,
    },
    ObserveLegacyCheckpoint {
        session: String,
        #[serde(rename = "originalV1")]
        original_v1: OriginalV1,
    },
    ObserveIngress {
        session: String,
        step: IngressStep,
    },
    ApplyIngress {
        binding: Binding,
        step: IngressStep,
    },
    VerifyIngress {
        session: String,
        step: IngressStep,
    },
    CaptureYard {
        session: String,
        yard: String,
        resolved: bool,
    },
    PrepareSettings {
        session: String,
        yard: String,
        edits: Vec<Edit>,
    },
    CaptureOwnerObservation {
        session: String,
    },
    CaptureOwner {
        session: String,
        #[serde(rename = "completeSource", skip_serializing_if = "Option::is_none")]
        complete_source: Option<CompleteSourceReference>,
    },
    ObserveOwner {
        session: String,
        before: OwnerBefore,
        token: String,
    },
    ProjectActivation {
        session: String,
        before: OwnerBefore,
    },
    BindOwnerResults {
        session: String,
        #[serde(skip_serializing_if = "Option::is_none")]
        binding: Option<Binding>,
        producer: Binding,
        before: Box<OwnerBefore>,
        token: String,
        #[serde(skip_serializing_if = "Option::is_none")]
        action: Option<crate::program::ActionReference>,
        #[serde(rename = "producerAction", skip_serializing_if = "Option::is_none")]
        producer_action: Option<crate::program::ActionReference>,
        #[serde(
            rename = "retainedOnly",
            default,
            skip_serializing_if = "std::ops::Not::not"
        )]
        retained_only: bool,
    },
    ResolveOwnerTarget {
        session: String,
        #[serde(skip_serializing_if = "Option::is_none")]
        binding: Option<Binding>,
        target: Target,
    },
    ResolveAfterOwner {
        session: String,
        #[serde(skip_serializing_if = "Option::is_none")]
        binding: Option<Binding>,
        target: Target,
    },
    ReadInstalledRecipe {
        session: String,
        #[serde(rename = "recipeHash")]
        recipe_hash: String,
        offset: u64,
    },
    BindInstalledRecipe {
        session: String,
        #[serde(rename = "recipeHash")]
        recipe_hash: String,
        offset: u64,
        total: u64,
        content: String,
        #[serde(skip_serializing_if = "Option::is_none")]
        target: Option<Target>,
    },
    BindSealedInstalledRecipe {
        session: String,
        #[serde(rename = "recipeHash")]
        recipe_hash: String,
        total: u64,
        #[serde(skip_serializing_if = "Option::is_none")]
        target: Option<Target>,
    },
    CaptureActivation {
        session: String,
        owner: String,
        fresh: bool,
    },
    ClaimLease {
        binding: Binding,
        update: Identity,
        owner: Option<Identity>,
    },
    CaptureSettings {
        session: String,
        recipe: SettingsRecipe,
    },
    ObserveSettings {
        session: String,
        recipe: SettingsRecipe,
    },
    ApplySettings {
        binding: Binding,
        recipe: SettingsRecipe,
    },
    ApplyOwner {
        binding: Binding,
        before: OwnerBefore,
        token: String,
    },
    ApplyOwnerAction {
        binding: Binding,
        before: OwnerBefore,
        token: String,
        action: crate::program::ActionReference,
    },
    ObserveOwnerAction {
        session: String,
        before: OwnerBefore,
        token: String,
        action: crate::program::ActionReference,
    },
    CleanupOwner {
        binding: Binding,
        before: OwnerBefore,
        token: String,
    },
    QueryOwnerCleanup {
        binding: Binding,
        before: OwnerBefore,
        token: String,
    },
    ApplyActivation {
        binding: Binding,
        target: Target,
    },
    FenceActivation {
        binding: Binding,
        target: Target,
        successor: String,
    },
    QueryInvocation {
        binding: Binding,
        #[serde(skip_serializing_if = "Option::is_none")]
        target: Option<Target>,
    },
    ReleaseLease {
        session: String,
    },
}
impl Request {
    pub(crate) fn rebound(&self, binding: Binding) -> Result<Self> {
        let mut value = self.clone();
        match &mut value {
            Self::ApplySettings { binding: b, .. }
            | Self::ApplyOwner { binding: b, .. }
            | Self::ApplyOwnerAction { binding: b, .. }
            | Self::CleanupOwner { binding: b, .. }
            | Self::ApplyActivation { binding: b, .. }
            | Self::ApplyIngress { binding: b, .. }
            | Self::FenceActivation { binding: b, .. } => *b = binding,
            _ => return Err(Error::Invalid),
        }
        Ok(value)
    }
    pub(crate) fn recipe_fingerprint(&self) -> Result<String> {
        #[derive(Serialize)]
        struct Owner<'a> {
            before: &'a OwnerBefore,
            token: &'a str,
        }
        #[derive(Serialize)]
        struct Action<'a> {
            before: &'a OwnerBefore,
            token: &'a str,
            action: &'a crate::program::ActionReference,
        }
        #[derive(Serialize)]
        struct Cleanup<'a> {
            kind: &'a str,
            before: &'a OwnerBefore,
            token: &'a str,
        }
        #[derive(Serialize)]
        struct Fence<'a> {
            target: &'a Target,
            successor: &'a str,
        }
        let raw = match self {
            Self::ApplySettings { recipe, .. } => crate::codec::encode(recipe, false)?,
            Self::ApplyOwner { before, token, .. } => {
                crate::codec::encode(&Owner { before, token }, false)?
            }
            Self::ApplyOwnerAction {
                binding,
                before,
                token,
                action,
            } => {
                action.validate(binding)?;
                crate::codec::encode(
                    &Action {
                        before,
                        token,
                        action,
                    },
                    false,
                )?
            }
            Self::CleanupOwner { before, token, .. } => crate::codec::encode(
                &Cleanup {
                    kind: "cleanup-owner",
                    before,
                    token,
                },
                false,
            )?,
            Self::ApplyActivation { target, .. } => crate::codec::encode(target, false)?,
            Self::FenceActivation {
                target, successor, ..
            } => crate::codec::encode(&Fence { target, successor }, false)?,
            Self::ApplyIngress { step, .. } => crate::codec::encode(step, false)?,
            _ => return Err(Error::Invalid),
        };
        Ok(crate::digest(&raw))
    }
}
#[derive(Deserialize, Serialize, Default)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub(crate) struct Reply {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub resolved_recipe: String,
    #[serde(default)]
    pub installed_chunk: Option<InstalledChunk>,
    #[serde(default)]
    pub rollback_support: Option<RollbackSupport>,
    #[serde(default)]
    pub layout: Option<Layout>,
    #[serde(default)]
    pub diagnostic: Option<Diagnostic>,
    pub code: String,
    #[serde(default)]
    pub yards: Vec<String>,
    #[serde(default)]
    pub owners: Vec<String>,
    pub ingress: Option<IngressFacts>,
    pub settings: Option<YardSettings>,
    pub recipe: Option<PreparedSettings>,
    pub owner: Option<OwnerBefore>,
    #[serde(default)]
    pub targets: Vec<Target>,
    #[serde(default)]
    pub actual: String,
    pub terminal: Option<Terminal>,
}
impl Reply {
    pub(crate) fn protocol(&self, request: &Request) -> Result<()> {
        use base64::{engine::general_purpose::STANDARD, Engine};
        if !self.code.is_empty() {
            return require(self.installed_chunk.is_none());
        }
        if !matches!(request, Request::ResolveAfterOwner { .. }) {
            require(self.resolved_recipe.is_empty())?;
        }
        match request {
            Request::ResolveAfterOwner { target, .. } => {
                require(self.installed_chunk.is_none() && self.terminal.is_none())?;
                require(
                    self.targets.len() == 1
                        && self.targets[0].prepared_recipe == self.resolved_recipe,
                )?;
                crate::after_owner::physical_target(target, &self.targets[0])
            }
            Request::BindOwnerResults { .. } => require(
                self.installed_chunk.is_none()
                    && self.targets.is_empty()
                    && self.terminal.is_none(),
            ),
            Request::ResolveOwnerTarget { target, .. } => {
                require(self.installed_chunk.is_none() && self.terminal.is_none())?;
                target.resolved(&self.targets).map(|_| ())
            }
            Request::ProjectActivation { .. } => require(
                self.installed_chunk.is_none()
                    && !self.yards.is_empty()
                    && self.yards.len() <= 128
                    && self.yards.windows(2).all(|pair| pair[0] < pair[1])
                    && self.yards.iter().all(|yard| record::safe_name(yard)),
            ),
            Request::CaptureOwnerObservation { .. } => {
                require(
                    self.installed_chunk.is_none()
                        && self.terminal.is_none()
                        && self.targets.is_empty(),
                )?;
                let owner = self.owner.as_ref().ok_or(Error::Invalid)?;
                owner.validate()?;
                require(owner.prepared_recipe.is_empty())
            }
            Request::ObserveOwner { .. } => require(
                self.installed_chunk.is_none()
                    && matches!(self.actual.as_str(), "expected" | "in-progress" | "desired"),
            ),
            Request::ObserveOwnerAction { .. } => require(
                self.installed_chunk.is_none()
                    && self.targets.is_empty()
                    && self.terminal.is_none()
                    && self.actual == "desired",
            ),
            Request::ObserveLegacyCheckpoint { .. } => {
                require(self.installed_chunk.is_none() && record::fingerprint(&self.actual))
            }
            Request::ReadInstalledRecipe {
                recipe_hash,
                offset,
                ..
            } => {
                let c = self.installed_chunk.as_ref().ok_or(Error::Invalid)?;
                require(
                    c.recipe_hash == *recipe_hash
                        && c.offset == *offset
                        && c.total > *offset
                        && c.total <= 96 << 20,
                )?;
                let raw = STANDARD.decode(&c.content).map_err(|_| Error::Codec)?;
                require(
                    raw.len() == crate::prepared_payload::CHUNK.min((c.total - c.offset) as usize)
                        && STANDARD.encode(&raw) == c.content
                        && c.complete == (c.offset + raw.len() as u64 == c.total),
                )
            }
            Request::BindInstalledRecipe {
                recipe_hash,
                offset,
                total,
                content,
                target,
                ..
            } => {
                let c = self.installed_chunk.as_ref().ok_or(Error::Invalid)?;
                let size = STANDARD.decode(content).map_err(|_| Error::Codec)?.len() as u64;
                require(
                    target.as_ref().is_none_or(|t| {
                        t.prepared_recipe == *recipe_hash
                            && offset.checked_add(size) == Some(*total)
                    }) && c.recipe_hash == *recipe_hash
                        && c.offset == *offset
                        && c.total == *total
                        && c.content.is_empty()
                        && c.complete == (offset.checked_add(size) == Some(*total)),
                )
            }
            Request::BindSealedInstalledRecipe {
                recipe_hash,
                total,
                target,
                ..
            } => {
                let c = self.installed_chunk.as_ref().ok_or(Error::Invalid)?;
                require(
                    *total > 0
                        && *total <= 96 << 20
                        && target
                            .as_ref()
                            .is_none_or(|t| t.prepared_recipe == *recipe_hash)
                        && c.recipe_hash == *recipe_hash
                        && c.offset == 0
                        && c.total == *total
                        && c.content.is_empty()
                        && c.complete,
                )
            }
            _ => require(self.installed_chunk.is_none()),
        }
    }
}
#[derive(Deserialize, Serialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub(crate) struct InstalledChunk {
    pub recipe_hash: String,
    pub offset: u64,
    pub total: u64,
    pub content: String,
    pub complete: bool,
}
#[derive(Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub(crate) struct Repository {
    pub release: String,
    pub artifact_digest: String,
    pub root: Identity,
}
#[derive(Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct RollbackRepositories {
    pub source: Repository,
    pub target: Repository,
}
#[derive(Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct RollbackSupport {
    pub consumers: Vec<RollbackConsumer>,
    pub binding: String,
}
#[derive(Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub(crate) struct RollbackConsumer {
    pub kind: String,
    pub yard: String,
    pub owner: String,
    pub selected: bool,
    pub managed_paths: i64,
    pub managed_paths_present: bool,
    pub retained_contract: String,
    pub observation: Option<RuntimeObservation>,
    pub retained_payload: String,
    pub retained_settings: String,
    pub installed: Option<Vec<RollbackPart>>,
}
#[derive(Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct RuntimeObservation {
    pub state: String,
    pub actual: String,
    pub desired: String,
    #[serde(default)]
    pub hook_binding: String,
    #[serde(default)]
    pub ownership_absent: bool,
}
#[derive(Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct RollbackPart {
    pub kind: String,
    pub present: bool,
    pub payload: String,
    pub settings: String,
}
#[derive(Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct Layout {
    pub exists: bool,
    pub raw: String,
    pub digest: String,
}
#[derive(Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct Diagnostic {
    pub kind: String,
    pub message: String,
    pub retry: String,
}
impl Diagnostic {
    pub(crate) fn validate(&self) -> Result<()> {
        require(
            matches!(
                self.kind.as_str(),
                "component-failure" | "component-conflict"
            ) && !self.message.is_empty()
                && self.message.len() <= 512
                && self.retry.len() <= 512
                && !self
                    .message
                    .chars()
                    .chain(self.retry.chars())
                    .any(char::is_control),
        )
    }
}
#[derive(Deserialize, Serialize, Default)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub(crate) struct PreparedSettings {
    pub yard: String,
    pub target: String,
    pub expected_exists: bool,
    pub expected_file: String,
    pub desired_file: String,
    pub source: String,
    pub inputs: Vec<InputBinding>,
}
impl crate::legacy::Historical for Reply {
    const KIND: crate::legacy::Kind = crate::legacy::Kind::Native;
}
impl Reply {
    pub(crate) fn checked(self) -> Result<Self> {
        if self.code.is_empty() {
            Ok(self)
        } else {
            Err(match self.code.as_str() {
                "stale" => Error::Stale,
                "unknown" => Error::Unknown,
                _ => Error::Io,
            })
        }
    }
}
#[derive(Deserialize, Serialize, Clone, PartialEq, Eq)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub(crate) struct Terminal {
    pub version: i64,
    pub binding: Binding,
    pub owner: String,
    pub resource: String,
    pub state: String,
    pub server: String,
    pub project: String,
    pub instance: String,
    pub instance_uuid: String,
    pub operation: String,
    pub final_status: i64,
    pub operations: Vec<Operation>,
    pub observed: String,
}

#[derive(Deserialize, Serialize, Clone, PartialEq, Eq)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub(crate) struct Operation {
    pub kind: String,
    pub server: String,
    pub project: String,
    pub resource: String,
    pub id: String,
    pub status: i64,
}

#[derive(Deserialize, Serialize, Clone)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub(crate) struct IngressBinding {
    pub transaction: String,
    pub plan: String,
    pub releases: crate::wire::journal_v2::ReleasePair,
    pub steps: Vec<IngressBoundStep>,
}
#[derive(Clone, PartialEq, Eq, Serialize)]
#[serde(rename_all = "camelCase")]
pub(crate) struct OriginalV1Record {
    pub identity: FileIdentity,
    pub digest: String,
}
#[derive(Clone, Serialize)]
#[serde(rename_all = "camelCase")]
pub(crate) struct OriginalV1 {
    pub expectation: crate::legacy_import::Expectation,
    pub journal: OriginalV1Record,
    pub registry: OriginalV1Record,
}
#[derive(Deserialize, Serialize, Clone)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub(crate) struct IngressBoundStep {
    pub kind: String,
    pub checkpoint: String,
    pub expected: String,
    pub desired: String,
    pub static_pin: String,
}
#[derive(Deserialize, Serialize, Clone)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub(crate) struct IngressStep {
    pub binding: IngressBinding,
    pub kind: String,
    pub expected: String,
    pub desired: String,
    pub static_pin: String,
}
#[derive(Deserialize, Serialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub(crate) struct IngressFacts {
    pub yards: Vec<String>,
    pub operations: Vec<IngressOperation>,
    pub decisions: Vec<crate::wire::process_v1::RedactedDecision>,
    pub blockers: Vec<crate::wire::process_v1::Blocker>,
}
#[derive(Deserialize, Serialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub(crate) struct IngressOperation {
    pub kind: String,
    pub decision: String,
    pub expected: String,
    pub desired: String,
    pub static_pin: String,
}

#[cfg(test)]
mod tests {
    use super::*;
    use base64::{engine::general_purpose::STANDARD, Engine};
    #[test]
    fn command_override_wire_preserves_absence_and_empty_and_rejects_malformed() {
        for (raw, present, bytes) in [
            (
                r#"{"name":"SSH_PORT","present":false,"value":null}"#,
                false,
                None,
            ),
            (
                r#"{"name":"ENVIRONMENT_PROFILES","present":true,"value":""}"#,
                true,
                Some(Vec::new()),
            ),
            (
                r#"{"name":"SSH_PORT","present":true,"value":"MjI0MQ=="}"#,
                true,
                Some(b"2241".to_vec()),
            ),
        ] {
            let value: CommandOverride = serde_json::from_str(raw).unwrap();
            assert_eq!(value.present, present);
            assert_eq!(value.value, bytes);
            let round: CommandOverride =
                serde_json::from_slice(&serde_json::to_vec(&value).unwrap()).unwrap();
            assert_eq!(round.value, value.value);
        }
        for raw in [
            r#"{"name":"SSH_PORT","present":true,"value":"%%%"}"#,
            r#"{"name":"SSH_PORT","present":true,"value":[50]}"#,
            r#"{"name":"SSH_PORT","present":false,"value":null,"unknown":1}"#,
        ] {
            assert!(serde_json::from_str::<CommandOverride>(raw).is_err());
        }
    }

    #[test]
    fn materialized_integration_partial_observation_keeps_original_authority() {
        let original: Target = serde_json::from_value(serde_json::json!({
            "owner":"materialized-config", "class":"materialized-integrations", "yard":"yard",
            "scope":"scope", "reportedActual":"", "reportedDesired":"", "before":"", "scopeState":"", "stage":"",
            "project":"project", "instance":"instance", "instanceUuid":"original-instance",
            "state":"stale", "actual":"a".repeat(64), "desired":"b".repeat(64), "source":"c".repeat(64),
            "preparedRecipe":"d".repeat(64), "afterOwner":format!("v1:materialized-integrations:{}","e".repeat(64))
        })).unwrap();
        let before = crate::codec::encode(&original, false).unwrap();
        let mut current = original.clone();
        current.state = "current".into();
        current.actual = "f".repeat(64);
        current.reported_actual = "1".repeat(64);
        assert!(current.materialized_after_owner_observation(&original));
        assert!(!current.materialized_after_owner()); // Query recovery remains files-only.
        for field in [
            "owner",
            "class",
            "source",
            "desired",
            "before",
            "scope",
            "scopeState",
            "stage",
            "preparedRecipe",
            "afterOwner",
            "instanceUuid",
            "project",
            "instance",
            "yard",
            "reportedDesired",
        ] {
            let mut value = serde_json::to_value(&current).unwrap();
            value[field] = "foreign".into();
            assert!(
                !serde_json::from_value::<Target>(value)
                    .unwrap()
                    .materialized_after_owner_observation(&original),
                "{field}"
            );
        }
        current.state = "deferred".into();
        assert!(!current.materialized_after_owner_observation(&original));
        assert_eq!(before, crate::codec::encode(&original, false).unwrap());
    }

    #[test]
    fn original_integration_progress_and_new_order_keep_persisted_targets() {
        let original: Target = serde_json::from_value(serde_json::json!({
            "owner":"materialized-config", "class":"materialized-integrations", "yard":"yard",
            "scope":"scope", "reportedActual":"", "reportedDesired":"", "before":"before", "scopeState":"", "stage":"",
            "project":"project", "instance":"instance", "instanceUuid":"original-instance",
            "state":"stale", "actual":"a".repeat(64), "desired":"b".repeat(64), "source":"c".repeat(64),
            "preparedRecipe":"d".repeat(64)
        })).unwrap();
        let mut progressed = original.clone();
        progressed.actual = "e".repeat(64);
        progressed.reported_actual = "progress".into();
        progressed.before = "fresh-before".into();
        progressed.prepared_recipe = "f".repeat(64);
        assert!(progressed.original_integration_observation(&original));
        assert!(!progressed.materialized_current());
        for field in [
            "owner",
            "class",
            "source",
            "desired",
            "scope",
            "scopeState",
            "stage",
            "afterOwner",
            "instanceUuid",
            "project",
            "instance",
            "yard",
            "reportedDesired",
        ] {
            let mut foreign = serde_json::to_value(&progressed).unwrap();
            foreign[field] = "foreign".into();
            assert!(
                !serde_json::from_value::<Target>(foreign)
                    .unwrap()
                    .original_integration_observation(&original),
                "{field}"
            );
        }
        let mut files = original.clone();
        files.class = "materialized-files".into();
        let persisted = vec![original.clone(), files];
        let bytes = crate::codec::encode(&persisted, false).unwrap();
        let binding = crate::bundle::binding(&[], &persisted).unwrap();
        let mut fresh = persisted.clone();
        fresh.sort_by(Target::materialized_order);
        assert_eq!(fresh[0].class, "materialized-files");
        assert_eq!(persisted[0].class, "materialized-integrations");
        assert_eq!(binding, crate::bundle::binding(&[], &persisted).unwrap());
        assert_ne!(binding, crate::bundle::binding(&[], &fresh).unwrap());
        assert_eq!(bytes, crate::codec::encode(&persisted, false).unwrap());
        assert!(progressed.original_integration_observation(&persisted[0]));
    }

    #[test]
    fn capture_owner_complete_reference_is_only_readonly_request_input() {
        let original = CompleteSourceReference {
            transaction: "original-source".into(),
            journal_digest: "a".repeat(64),
            metadata_binding: "b".repeat(64),
        };
        let request = Request::CaptureOwner {
            session: "c".repeat(64),
            complete_source: Some(original.clone()),
        };
        let wire = serde_json::to_value(&request).unwrap();
        assert_eq!(
            wire["completeSource"],
            serde_json::to_value(&original).unwrap()
        );
        assert!(wire.get("complete_source").is_none());
        let omitted = serde_json::to_value(Request::CaptureOwner {
            session: "c".repeat(64),
            complete_source: None,
        })
        .unwrap();
        assert!(omitted.get("completeSource").is_none());
        let mut expanded = serde_json::to_value(original).unwrap();
        expanded["authority"] = true.into();
        assert!(serde_json::from_value::<CompleteSourceReference>(expanded).is_err());
    }

    #[test]
    fn owner_result_projection_only_resolves_the_original_absent_uuid() {
        let original = Target {
            prepared_recipe: "a".repeat(64),
            after_owner: String::new(),
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
            state: "stale".into(),
            actual: "b".repeat(64),
            desired: "c".repeat(64),
            source: "d".repeat(64),
        };
        let approved = crate::codec::encode(&original, false).unwrap();
        let bind = |target| Request::BindInstalledRecipe {
            session: "a".repeat(64),
            recipe_hash: original.prepared_recipe.clone(),
            offset: 0,
            total: 1,
            content: STANDARD.encode([0]),
            target,
        };
        assert!(serde_json::to_value(bind(None))
            .unwrap()
            .get("target")
            .is_none());
        let reply = Reply {
            installed_chunk: Some(InstalledChunk {
                recipe_hash: original.prepared_recipe.clone(),
                offset: 0,
                total: 1,
                content: String::new(),
                complete: true,
            }),
            ..Default::default()
        };
        reply.protocol(&bind(Some(original.clone()))).unwrap();
        let sealed = Request::BindSealedInstalledRecipe {
            session: "a".repeat(64),
            recipe_hash: original.prepared_recipe.clone(),
            total: 1,
            target: Some(original.clone()),
        };
        reply.protocol(&sealed).unwrap();
        let foreign = Request::BindSealedInstalledRecipe {
            session: "a".repeat(64),
            recipe_hash: "f".repeat(64),
            total: 1,
            target: Some(original.clone()),
        };
        assert!(reply.protocol(&foreign).is_err());
        let mut nonfinal = bind(Some(original.clone()));
        if let Request::BindInstalledRecipe { total, .. } = &mut nonfinal {
            *total = 2;
        }
        let nonfinal_reply = Reply {
            installed_chunk: Some(InstalledChunk {
                recipe_hash: original.prepared_recipe.clone(),
                offset: 0,
                total: 2,
                content: String::new(),
                complete: false,
            }),
            ..Default::default()
        };
        assert!(nonfinal_reply.protocol(&nonfinal).is_err());
        if let Request::BindInstalledRecipe { target, .. } = &mut nonfinal {
            *target = None;
        }
        nonfinal_reply.protocol(&nonfinal).unwrap();
        assert!(original.resolved(std::slice::from_ref(&original)).is_ok());
        let mut resolved = original.clone();
        resolved.instance_uuid = "01234567-89ab-cdef-0123-456789abcdef".into();
        assert!(original.resolved(std::slice::from_ref(&resolved)).is_ok());
        let mut existing_grammar = resolved.clone();
        existing_grammar.instance_uuid = "original-created-uuid".into();
        assert!(original.resolved(&[existing_grammar]).is_ok());
        assert_eq!(crate::codec::encode(&original, false).unwrap(), approved);
        assert!(original.resolved(&[]).is_err());
        assert!(original
            .resolved(&[resolved.clone(), resolved.clone()])
            .is_err());
        let mut changed = resolved.clone();
        changed.instance_uuid = "11234567-89ab-cdef-0123-456789abcdef".into();
        assert!(resolved.resolved(&[changed]).is_err());
        for field in [
            "preparedRecipe",
            "scope",
            "class",
            "owner",
            "yard",
            "project",
            "instance",
            "actual",
            "desired",
            "source",
            "before",
            "state",
        ] {
            let mut value = serde_json::to_value(&resolved).unwrap();
            value[field] = serde_json::Value::String(
                if matches!(field, "preparedRecipe" | "actual" | "desired" | "source") {
                    "f".repeat(64)
                } else if field == "state" {
                    "current".into()
                } else {
                    "other".into()
                },
            );
            let changed: Target = serde_json::from_value(value).unwrap();
            assert!(original.resolved(&[changed]).is_err(), "{field}");
        }
        assert!(resolved.resolved(std::slice::from_ref(&original)).is_err());
        for invalid in [String::new(), "a".repeat(129), "../invalid".into()] {
            resolved.instance_uuid = invalid;
            assert!(original.resolved(std::slice::from_ref(&resolved)).is_err());
        }
    }
    #[test]
    fn owner_observation_cannot_return_preparation_authority() {
        let request = Request::CaptureOwnerObservation {
            session: "a".repeat(64),
        };
        let wire = serde_json::to_value(&request).unwrap();
        assert_eq!(wire["op"], "capture-owner-observation");
        assert_eq!(wire.as_object().unwrap().len(), 2);
        let mut reply = Reply {
            owner: Some(OwnerBefore {
                state: "absent".into(),
                ..Default::default()
            }),
            ..Default::default()
        };
        reply.protocol(&request).unwrap();
        reply.owner.as_mut().unwrap().prepared_recipe = "b".repeat(64);
        assert!(reply.protocol(&request).is_err());
        reply.owner.as_mut().unwrap().prepared_recipe.clear();
        reply.owner.as_mut().unwrap().state = "unknown".into();
        assert!(reply.protocol(&request).is_err());
        reply.owner = None;
        assert!(reply.protocol(&request).is_err());
    }

    #[test]
    fn owner_progress_is_a_closed_success_projection() {
        let request = Request::ObserveOwner {
            session: "a".repeat(64),
            before: OwnerBefore::default(),
            token: "b".repeat(32),
        };
        for progress in ["expected", "in-progress", "desired"] {
            let reply = Reply {
                actual: progress.into(),
                ..Default::default()
            };
            reply.protocol(&request).unwrap();
        }
        for progress in ["", "current", "complete", "unknown"] {
            let reply = Reply {
                actual: progress.into(),
                ..Default::default()
            };
            assert!(reply.protocol(&request).is_err());
        }
    }
    #[test]
    fn recipe_transport_admits_the_full_declared_canonical_capacity() {
        let total = 96u64 << 20;
        let offset = total - crate::prepared_payload::CHUNK as u64;
        let hash = "a".repeat(64);
        let request = Request::ReadInstalledRecipe {
            session: hash.clone(),
            recipe_hash: hash.clone(),
            offset,
        };
        let mut reply = Reply {
            installed_chunk: Some(InstalledChunk {
                recipe_hash: hash,
                offset,
                total,
                content: STANDARD.encode(vec![0; crate::prepared_payload::CHUNK]),
                complete: true,
            }),
            ..Default::default()
        };
        reply.protocol(&request).unwrap();
        reply.installed_chunk.as_mut().unwrap().total += 1;
        assert!(reply.protocol(&request).is_err());
    }
    #[test]
    fn activation_projection_inventory_is_bounded_and_unambiguous() {
        let request = Request::ProjectActivation {
            session: "a".repeat(64),
            before: OwnerBefore::default(),
        };
        let reply = Reply {
            yards: vec!["first".into(), "second".into()],
            ..Default::default()
        };
        reply.protocol(&request).unwrap();
        for yards in [
            vec![],
            vec!["same".into(), "same".into()],
            vec!["z".into(), "a".into()],
            vec!["../escape".into()],
            (0..129).map(|n| format!("yard-{n:03}")).collect(),
        ] {
            assert!(Reply {
                yards,
                ..Default::default()
            }
            .protocol(&request)
            .is_err());
        }
    }
}
