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
}
#[derive(Clone, Serialize, Deserialize, PartialEq, Eq)]
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
    pub(crate) fn validate(&self) -> Result<()> {
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
        #[serde(rename = "bindingIngress")]
        binding: Option<IngressBinding>,
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
    CaptureOwner {
        session: String,
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
    ProjectSettings {
        session: String,
        recipe: SettingsRecipe,
        edits: Vec<Edit>,
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
        match request {
            Request::ProjectActivation { .. } => require(
                self.installed_chunk.is_none()
                    && !self.yards.is_empty()
                    && self.yards.len() <= 128
                    && self.yards.windows(2).all(|pair| pair[0] < pair[1])
                    && self.yards.iter().all(|yard| record::safe_name(yard)),
            ),
            Request::ObserveOwner { .. } => require(
                self.installed_chunk.is_none()
                    && matches!(self.actual.as_str(), "expected" | "in-progress" | "desired"),
            ),
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
                ..
            } => {
                let c = self.installed_chunk.as_ref().ok_or(Error::Invalid)?;
                let size = STANDARD.decode(content).map_err(|_| Error::Codec)?.len() as u64;
                require(
                    c.recipe_hash == *recipe_hash
                        && c.offset == *offset
                        && c.total == *total
                        && c.content.is_empty()
                        && c.complete == (offset.checked_add(size) == Some(*total)),
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
