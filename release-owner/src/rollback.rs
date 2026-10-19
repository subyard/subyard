//! Release compatibility is reduced in Rust from owning component leaf facts.
use crate::{
    native,
    record::{self, require},
    Result,
};
use std::collections::BTreeSet;
fn same(value: &str) -> Result<bool> {
    require(matches!(value, "same" | "different"))?;
    Ok(value == "same")
}
pub(crate) fn compatible(support: &native::RollbackSupport, owners: &[String]) -> Result<bool> {
    require(
        record::fingerprint(&support.binding)
            && owners.len() <= 128
            && owners.iter().all(|s| record::safe_id(s))
            && owners.iter().collect::<BTreeSet<_>>().len() == owners.len(),
    )?;
    require(support.consumers.len() <= 8192)?;
    let mut seen = BTreeSet::new();
    for c in &support.consumers {
        require(
            record::safe_name(&c.yard)
                && record::safe_id(&c.owner)
                && matches!(c.kind.as_str(), "profile" | "core")
                && seen.insert((&c.kind, &c.yard, &c.owner))
                && c.managed_paths >= 0
                && c.managed_paths <= 8192
                && matches!(
                    c.retained_contract.as_str(),
                    "same" | "absent" | "different"
                ),
        )?;
        let full_payload = same(&c.retained_payload)?;
        let full_settings = same(&c.retained_settings)?;
        let owner_present = owners.contains(&c.owner);
        if c.kind == "profile" {
            let loses = c.retained_contract != "same" || !owner_present;
            if loses && (c.managed_paths_present || c.managed_paths == 0 && c.selected) {
                return Ok(false);
            }
            if loses {
                let Some(observed) = &c.observation else {
                    return Ok(false);
                };
                require(matches!(
                    observed.state.as_str(),
                    "absent" | "current" | "stale" | "stopped" | "deferred"
                ))?;
                if observed.state != "absent"
                    && !(observed.state == "current" && observed.ownership_absent)
                {
                    return Ok(false);
                }
            }
        } else if !(full_payload && full_settings) {
            let Some(parts) = &c.installed else {
                return Ok(false);
            };
            require(parts.len() <= 64)?;
            let mut kinds = BTreeSet::new();
            for part in parts {
                require(record::safe_id(&part.kind) && kinds.insert(&part.kind))?;
                let unchanged = same(&part.payload)? && same(&part.settings)?;
                if part.present && !unchanged && !owner_present {
                    return Ok(false);
                }
            }
        }
    }
    Ok(true)
}
impl crate::owner::Owner {
    pub(crate) fn rollback_support(&self) -> Result<(bool, String)> {
        let source = self
            .options
            .source_artifact
            .as_ref()
            .ok_or(crate::Error::Invalid)?;
        let target = self
            .options
            .target_artifact
            .as_ref()
            .ok_or(crate::Error::Invalid)?;
        require(
            source.release == self.options.releases.from
                && target.release == self.options.releases.target,
        )?;
        let source_fd = self.links.artifact_root(&source.release)?;
        let target_fd = if self.pre_v2_owner() {
            self.links.artifact_root(&target.release)?
        } else {
            crate::store::io(rustix::io::dup(&self.repository))?
        };
        require(
            native::Identity::capture(&source_fd)? == source.root
                && native::Identity::capture(&target_fd)? == target.root,
        )?;
        let owners = self
            .process_capabilities("runtime-activation-capabilities")?
            .activation_owners
            .unwrap_or_default();
        let support = self
            .native(
                &native::Request::CaptureRollbackSupport {
                    session: self.session.clone(),
                    repositories: native::RollbackRepositories {
                        source: native::Repository {
                            release: source.release.clone(),
                            artifact_digest: source.artifact_digest.clone(),
                            root: source.root.clone(),
                        },
                        target: native::Repository {
                            release: target.release.clone(),
                            artifact_digest: target.artifact_digest.clone(),
                            root: target.root.clone(),
                        },
                    },
                    target_owners: owners.clone(),
                },
                vec![source_fd, target_fd],
            )?
            .rollback_support
            .ok_or(crate::Error::Invalid)?;
        Ok((compatible(&support, &owners)?, support.binding))
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    use serde::Deserialize;
    #[derive(Deserialize)]
    #[serde(rename_all = "camelCase")]
    struct Vector {
        support: native::RollbackSupport,
        target_owners: Vec<String>,
        accepted: bool,
    }
    #[test]
    fn actual_go_retained_runtime_policy() {
        let Ok(path) = std::env::var("SUBYARD_RUST_ROLLBACK_CORPUS") else {
            return;
        };
        let raw = std::fs::read(path).unwrap();
        assert!(raw.len() <= 8 << 20);
        let cases: Vec<Vector> = serde_json::from_slice(&raw).unwrap();
        assert!(!cases.is_empty() && cases.len() <= 4096);
        for (n, c) in cases.iter().enumerate() {
            assert_eq!(
                compatible(&c.support, &c.target_owners)
                    .unwrap_or_else(|e| panic!("rollback {n}: {e:?}")),
                c.accepted,
                "rollback {n}"
            );
        }
    }
}
