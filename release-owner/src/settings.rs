//! Generic captured settings inputs. Conversion rules belong to the owning profile.
#[cfg(test)]
pub(crate) use crate::test_vm_settings::conversion as test_vm_conversion;
use crate::{
    codec, digest,
    record::{self, require},
    work::{BeforeValue, Edit, SettingsRecipe},
    Error, Result,
};
use serde::{Deserialize, Serialize};
use std::collections::{BTreeMap, BTreeSet};
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

// Frozen recovery-value shape; decoding defaults remain historical codec behavior.
#[derive(Serialize, Deserialize, Default)]
#[serde(default, deny_unknown_fields, rename_all = "camelCase")]
pub(crate) struct RecoveryValue {
    pub setting: String,
    pub present: bool,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub value: String,
}

/// A settings migration supplies only conversion and its retained-work validator.
/// The engine owns consent, guarded native CAS, evidence, ledger and continuation.
pub(crate) struct Migration {
    pub prepare: fn(Capture<'_>) -> Result<(YardSettings, String)>,
    pub kind: &'static str,
    pub from_epoch: i64,
    pub to_epoch: i64,
    pub decisions: &'static [&'static str],
    pub validate_recovery: fn(&[RecoveryValue]) -> Result<()>,
    pub resource_class: &'static str,
    // Frozen Settings bundle/recovery encoding, never a dispatch choice.
    pub wire_local: bool,
    pub rollback_compatibility: &'static str,
    pub legacy_minimum_version: &'static str,
    pub convert: fn(&YardSettings, &[YardSettings]) -> Result<Option<Conversion>>,
    pub validate: fn(&SettingsRecipe, &str) -> Result<()>,
}
pub(crate) const LOCAL_SSH: Migration = Migration {
    prepare: prepare_local_ssh,
    kind: "local-ssh-port-settings-v2-to-v3",
    from_epoch: 2,
    to_epoch: 3,
    decisions: &["canonicalize"],
    validate_recovery: validate_local_ssh_recovery,
    resource_class: "local-ssh-port-settings-v1",
    wire_local: true,
    rollback_compatibility: "",
    legacy_minimum_version: "",
    convert: convert_local_ssh,
    validate: validate_local_ssh_recipe,
};
pub(crate) fn migrations() -> [&'static Migration; 2] {
    [&crate::test_vm_settings::MIGRATION, &LOCAL_SSH]
}
pub(crate) fn migration(kind: &str) -> Option<&'static Migration> {
    migrations()
        .into_iter()
        .find(|definition| definition.kind == kind)
}
impl Migration {
    pub(crate) fn descriptor(&self) -> crate::wire::registry_v2::CapabilityDescriptor {
        crate::wire::registry_v2::CapabilityDescriptor {
            kind: self.kind.into(),
            domain: "settings".into(),
            version: 1,
            rollback_compatibility: self.rollback_compatibility.into(),
            legacy_minimum_version: self.legacy_minimum_version.into(),
        }
    }
}

fn convert_local_ssh(
    input: &YardSettings,
    inventory: &[YardSettings],
) -> Result<Option<Conversion>> {
    local_ssh_conversion(input, legacy_ssh(inventory))
}

fn legacy_ssh(inventory: &[YardSettings]) -> bool {
    inventory
        .iter()
        .flat_map(|yard| &yard.sources)
        .any(|source| {
            source.label.contains(".sync/settings/")
                && source
                    .assignments
                    .iter()
                    .any(|assignment| assignment.name == "SSH_PORT")
        })
}

fn validate_local_ssh_recovery(values: &[RecoveryValue]) -> Result<()> {
    require(
        values.len() == 1
            && values[0].setting == "SSH_PORT"
            && (!values[0].present || values[0].value.parse::<u16>().is_ok_and(|p| p > 0)),
    )
}

fn validate_local_ssh_recipe(recipe: &SettingsRecipe, _decision: &str) -> Result<()> {
    require(
        recipe.edits.len() == 1
            && recipe.edits[0].setting == "SSH_PORT"
            && recipe.edits[0]
                .value
                .as_ref()
                .is_some_and(|v| v.parse::<u16>().is_ok_and(|n| n > 0 && n.to_string() == *v))
            && recipe.before.len() == 1
            && recipe.before[0].setting == "SSH_PORT"
            && !recipe.before[0].present
            && recipe.before[0].value.is_empty(),
    )?;
    Ok(())
}

fn ssh_inheritance(yard: &YardSettings) -> Result<String> {
    let mut inputs = BTreeMap::new();
    for source in &yard.sources {
        if source.role.contains("scalar settings") {
            inputs.insert(
                source.label.clone(),
                snapshot_fingerprint(source.exists, &source.sha256)?,
            );
        }
    }
    Ok(digest(&codec::encode(&inputs, false)?))
}
// Migration preparation can request only read-only facts for this exact yard.
// It has no native mutation, consent, journal or filesystem writer capability.
pub(crate) struct Capture<'a> {
    owner: &'a crate::owner::Owner,
    pub original: &'a YardSettings,
    inventory: &'a [YardSettings],
    pub overlay_prepared: bool,
}
impl<'a> Capture<'a> {
    pub(crate) fn new(
        owner: &'a crate::owner::Owner,
        original: &'a YardSettings,
        overlay_prepared: bool,
        inventory: &'a [YardSettings],
    ) -> Self {
        Self {
            owner,
            original,
            inventory,
            overlay_prepared,
        }
    }
    pub(crate) fn current(&self, resolved: bool) -> Result<YardSettings> {
        self.owner
            .call(crate::native::Request::CaptureYard {
                session: self.owner.session.clone(),
                yard: self.original.yard.clone(),
                resolved,
            })?
            .settings
            .ok_or(Error::Invalid)
    }
    pub(crate) fn command_ids(&self) -> &[String] {
        &self.owner.options.inherited_setting_ids
    }
}
fn validate_ssh_inventory(inventory: &[YardSettings]) -> Result<()> {
    if legacy_ssh(inventory) {
        let mut ports = BTreeSet::new();
        for yard in inventory {
            if yard.access == "local" && yard.ssh_port != 0 {
                require(ports.insert(yard.ssh_port))?;
            }
        }
    }
    Ok(())
}
fn prepare_local_ssh(input: Capture<'_>) -> Result<(YardSettings, String)> {
    validate_ssh_inventory(input.inventory)?;
    let current = input.current(true)?;
    let binding = ssh_inheritance(&current)?;
    Ok((current, binding))
}

#[cfg(test)]
mod authoring_tests {
    use super::*;
    #[test]
    fn ssh_collision_precondition_belongs_to_pending_ssh_work() {
        let yard = YardSettings {
            yard: "a".into(),
            canonical_exists: false,
            flat_exists: false,
            assignments: vec![],
            inherited: BTreeSet::new(),
            template_supported: false,
            access: "local".into(),
            ssh_port: 2222,
            ssh_group: "Git settings".into(),
            sources: vec![Source {
                role: "git-shared".into(),
                label: ".sync/settings/config.env".into(),
                exists: true,
                sha256: digest(b"SSH_PORT=2222\n"),
                assignments: vec![Assignment {
                    name: "SSH_PORT".into(),
                    value: "2222".into(),
                    line: 1,
                    direct: true,
                    dynamic: false,
                }],
            }],
        };
        let mut other = yard.clone();
        other.yard = "b".into();
        let inventory = [yard.clone(), other];
        assert!(validate_ssh_inventory(&inventory).is_err());
        // An unrelated pending conversion must not apply the SSH precondition.
        assert!(
            (crate::test_vm_settings::MIGRATION.convert)(&yard, &inventory)
                .unwrap()
                .is_none()
        );
        let mut nonlegacy = inventory;
        for yard in &mut nonlegacy {
            yard.sources.clear();
        }
        assert!(validate_ssh_inventory(&nonlegacy).is_ok());
    }
    #[test]
    fn frozen_recovery_keeps_historical_value_policy() {
        let profile = &crate::test_vm_settings::MIGRATION;
        // Schema1 did not require the complete pair in current prepared recipes.
        assert!((profile.validate_recovery)(&[]).is_ok());
        let value = |setting: &str, present: bool, value: &str| RecoveryValue {
            setting: setting.into(),
            present,
            value: value.into(),
        };
        assert!((profile.validate_recovery)(&[value("YARD_TEMPLATE", true, "e2e-vms")]).is_ok());
        assert!((profile.validate_recovery)(&[value("SSH_PORT", true, "2222")]).is_err());
        // The historical SSH reader accepts a numeric port spelling; its
        // current recipe validator still requires canonical newly written bytes.
        assert!((LOCAL_SSH.validate_recovery)(&[value("SSH_PORT", true, "02222")]).is_ok());
        assert!((LOCAL_SSH.validate_recovery)(&[value("SSH_PORT", true, "0")]).is_err());
        assert!((LOCAL_SSH.validate_recovery)(&[]).is_err());
    }
    // Nonshipping author example: a new kind uses the existing settings work,
    // not either historical migration's conversion or validation policy.
    const NEXT: Migration = Migration {
        kind: "author-settings-proof",
        from_epoch: 3,
        to_epoch: 4,
        decisions: &["canonicalize"],
        validate_recovery: |values| require(values.len() == 1 && values[0].setting == "SSH_PORT"),
        resource_class: "author-settings-proof-v1",
        wire_local: false,
        rollback_compatibility: "",
        legacy_minimum_version: "",
        prepare: |_| Err(Error::Invalid),
        convert: |_, _| Err(Error::Invalid),
        validate: |recipe, decision| {
            require(
                decision == "canonicalize"
                    && recipe.edits
                        == [Edit {
                            setting: "SSH_PORT".into(),
                            value: Some("2222".into()),
                        }]
                    && recipe.before
                        == [BeforeValue {
                            setting: "SSH_PORT".into(),
                            present: true,
                            value: "2221".into(),
                        }],
            )
        },
    };
    #[test]
    fn new_definition_uses_generic_settings_work_not_historical_wire_policy() {
        let hash = "a".repeat(64);
        let mut recipe = SettingsRecipe {
            yard: "fixture".into(),
            target: "canonical".into(),
            inheritance: hash.clone(),
            expected: resource_fingerprint(true, &hash, &hash).unwrap(),
            desired: resource_fingerprint(true, &"b".repeat(64), &hash).unwrap(),
            expected_exists: true,
            expected_file: hash.clone(),
            desired_file: "b".repeat(64),
            edits: vec![Edit {
                setting: "SSH_PORT".into(),
                value: Some("2222".into()),
            }],
            before: vec![BeforeValue {
                setting: "SSH_PORT".into(),
                present: true,
                value: "2221".into(),
            }],
            source: hash,
            inputs: vec![],
        };
        let mut migration = crate::wire::registry_v2::Migration {
            id: "author-proof".into(),
            kind: NEXT.kind.into(),
            domain: "settings".into(),
            from_epoch: 3,
            to_epoch: 4,
            ..Default::default()
        };
        let work =
            crate::work::Work::settings(&migration, &NEXT, recipe.clone(), "canonicalize").unwrap();
        assert_eq!(work.intent.id, "author-proof.fixture");
        assert_eq!(work.intent.expected, recipe.expected);
        assert_eq!(NEXT.descriptor().kind, migration.kind);
        migration.kind = LOCAL_SSH.kind.into();
        assert!(
            crate::work::Work::settings(&migration, &NEXT, recipe.clone(), "canonicalize").is_err()
        );
        migration.kind = NEXT.kind.into();
        recipe.edits[0].value = Some("2223".into());
        assert!(crate::work::Work::settings(&migration, &NEXT, recipe, "canonicalize").is_err());
    }
}
