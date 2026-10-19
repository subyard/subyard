//! Generic captured settings inputs. Conversion rules belong to the owning profile.
use crate::{
    codec, digest,
    record::{self, require},
    work::{BeforeValue, Edit},
    Result,
};
use serde::{Deserialize, Serialize};
use std::collections::BTreeSet;
#[path = "../../config/profiles/test-vms/release_settings.rs"]
mod test_vm;
pub(crate) use test_vm::conversion as test_vm_conversion;
#[derive(Clone, Deserialize, Serialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub(crate) struct Assignment {
    pub name: String,
    pub value: String,
    pub line: i64,
    pub direct: bool,
    pub dynamic: bool,
}
#[derive(Clone, Deserialize, Serialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub(crate) struct YardSettings {
    pub yard: String,
    pub canonical_exists: bool,
    pub flat_exists: bool,
    pub assignments: Vec<Assignment>,
    pub inherited: BTreeSet<String>,
    pub template_supported: bool,
    pub access: String,
    pub ssh_port: i64,
    pub ssh_group: String,
    pub sources: Vec<Source>,
}
#[derive(Clone, Deserialize, Serialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub(crate) struct Source {
    pub role: String,
    pub label: String,
    pub exists: bool,
    pub sha256: String,
    pub assignments: Vec<Assignment>,
}
pub(crate) struct Conversion {
    pub edits: Vec<Edit>,
    pub before: Vec<BeforeValue>,
    pub decisions: Vec<crate::wire::process_v1::RedactedDecision>,
    pub decision: String,
}
pub(crate) fn exact(input: &YardSettings, name: &str) -> Result<(String, bool)> {
    let matches: Vec<_> = input
        .assignments
        .iter()
        .filter(|a| a.name == name)
        .collect();
    require(matches.len() <= 1 && matches.iter().all(|a| a.direct && !a.dynamic))?;
    Ok(matches
        .first()
        .map(|a| (a.value.clone(), true))
        .unwrap_or_default())
}
pub(crate) fn decision(
    yard: &str,
    name: &str,
    decision: &str,
    result: &str,
) -> crate::wire::process_v1::RedactedDecision {
    crate::wire::process_v1::RedactedDecision {
        resource: format!("setting.{name}"),
        scope: format!("yard.{yard}"),
        decision: decision.into(),
        result: result.into(),
    }
}
pub(crate) fn local_ssh_conversion(
    input: &YardSettings,
    legacy_git_present: bool,
) -> Result<Option<Conversion>> {
    require(record::safe_name(&input.yard) && !(input.canonical_exists && input.flat_exists))?;
    if !legacy_git_present || input.access != "local" || input.ssh_group != "Git settings" {
        return Ok(None);
    }
    require(input.ssh_port > 0 && input.ssh_port <= 65535)?;
    let (_, present) = exact(input, "SSH_PORT")?;
    require(!present)?;
    Ok(Some(Conversion{edits:vec![Edit{setting:"SSH_PORT".into(),value:Some(input.ssh_port.to_string())}],before:vec![BeforeValue{setting:"SSH_PORT".into(),present:false,value:String::new()}],decisions:vec![crate::wire::process_v1::RedactedDecision{resource:format!("yard.{}",input.yard),scope:"yard".into(),decision:"canonicalize".into(),result:"preserve existing effective SSH port in local yard settings; source and cache remain protected".into()}],decision:"canonicalize".into()}))
}
pub(crate) fn snapshot_fingerprint(exists: bool, sha256: &str) -> Result<String> {
    #[derive(Serialize)]
    struct Snapshot<'a> {
        exists: bool,
        sha256: &'a str,
    }
    require(record::fingerprint(sha256))?;
    Ok(digest(&codec::encode(&Snapshot { exists, sha256 }, false)?))
}
pub(crate) fn resource_fingerprint(
    exists: bool,
    sha256: &str,
    inheritance: &str,
) -> Result<String> {
    #[derive(Serialize)]
    struct Resource<'a> {
        file: String,
        inheritance: &'a str,
    }
    require(record::fingerprint(inheritance))?;
    Ok(digest(&codec::encode(
        &Resource {
            file: snapshot_fingerprint(exists, sha256)?,
            inheritance,
        },
        false,
    )?))
}

#[cfg(test)]
mod tests {
    use super::*;
    #[derive(Deserialize)]
    struct Case {
        input: YardSettings,
        blocked: bool,
        edits: Vec<Edit>,
        decisions: Option<Vec<crate::wire::process_v1::RedactedDecision>>,
        decision: String,
    }
    #[test]
    fn owning_go_same_assignment_facts() {
        let Ok(path) = std::env::var("SUBYARD_RUST_SETTINGS_CORPUS") else {
            return;
        };
        let raw = std::fs::read(path).unwrap();
        let cases: Vec<Case> = serde_json::from_slice(&raw).unwrap();
        assert_eq!(cases.len(), 224);
        for (n, c) in cases.into_iter().enumerate() {
            let result = test_vm_conversion(&c.input);
            assert_eq!(result.is_err(), c.blocked, "case {n} admission");
            if c.blocked {
                continue;
            };
            let (edits, decisions, decision) = match result.unwrap() {
                Some(v) => (v.edits, v.decisions, v.decision),
                None => (vec![], vec![], String::new()),
            };
            assert_eq!(
                codec::encode(&edits, false).unwrap(),
                codec::encode(&c.edits, false).unwrap(),
                "case {n} edits"
            );
            assert_eq!(
                decisions,
                c.decisions.unwrap_or_default(),
                "case {n} decisions"
            );
            if !edits.is_empty() {
                assert_eq!(decision, c.decision, "case {n} decision")
            }
        }
    }
}
