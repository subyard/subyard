//! Closed release work. Recorded history cannot be dispatched as native work.
use crate::{
    codec, digest,
    record::{self, require},
    wire::{plan_v1::PlannerStepIntent, registry_v2::Migration},
    Result,
};
use serde::{Deserialize, Serialize};
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "kebab-case")]
pub(crate) enum IngressKind {
    LegacyV1Import,
    Import,
    Entrypoints,
}
impl IngressKind {
    pub(crate) fn labels(
        self,
    ) -> (
        &'static str,
        &'static str,
        &'static str,
        &'static str,
        &'static str,
    ) {
        match self {
            Self::LegacyV1Import => (
                "legacy-v1-import",
                "legacy-v1.import",
                "legacy-v1.journal",
                "legacy-v1",
                "legacy-v1-journal",
            ),
            Self::Import => (
                "source-install-v1",
                "source-install.import",
                "source-install.config",
                "source-install",
                "source-install-v1",
            ),
            Self::Entrypoints => (
                "source-install-v1",
                "source-install.entrypoints",
                "source-install.entrypoints",
                "source-install",
                "source-install-v1",
            ),
        }
    }
}
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub(crate) struct Edit {
    pub setting: String,
    pub value: Option<String>,
}
#[derive(Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub(crate) struct SettingsRecipe {
    pub yard: String,
    pub target: String,
    pub inheritance: String,
    pub expected: String,
    pub desired: String,
    pub expected_exists: bool,
    pub expected_file: String,
    pub desired_file: String,
    pub edits: Vec<Edit>,
    pub before: Vec<BeforeValue>,
    pub source: String,
    pub inputs: Vec<crate::native::InputBinding>,
}
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub(crate) struct BeforeValue {
    pub setting: String,
    pub present: bool,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub value: String,
}
#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub(crate) struct OwnerBefore {
    pub state: String,
    pub registration: String,
    pub overrides: String,
    pub controller: String,
    pub shared_images: bool,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub prepared_recipe: String,
}
impl OwnerBefore {
    pub(crate) fn validate(&self) -> Result<()> {
        require(self.prepared_recipe.is_empty() || record::fingerprint(&self.prepared_recipe))?;
        require(matches!(
            self.state.as_str(),
            "absent"
                | "legacy-directory"
                | "legacy-directory+projects"
                | "legacy-directory+overrides"
                | "legacy-directory+projects+overrides"
                | "legacy-flat"
                | "legacy-directory-adopt-current"
                | "legacy-flat-adopt-current"
                | "current"
        ))?;
        if self.state == "absent" {
            require(
                self.registration.is_empty()
                    && self.overrides.is_empty()
                    && self.controller.is_empty()
                    && !self.shared_images,
            )
        } else {
            require(
                [&self.registration, &self.overrides, &self.controller]
                    .into_iter()
                    .all(|s| record::fingerprint(s)),
            )
        }
    }
    pub(crate) fn fingerprint(&self) -> Result<String> {
        self.validate()?;
        Ok(digest(&codec::encode(&self.semantic(), false)?))
    }
    pub(crate) fn semantic(&self) -> Self {
        let mut value = self.clone();
        value.prepared_recipe.clear();
        value
    }
    pub(crate) fn desired(&self) -> Self {
        let mut next = self.clone();
        next.state = "current".into();
        next
    }
}
pub(crate) enum Recipe {
    LocalSSHPortPin(SettingsRecipe),
    TestVMSettingsMigration(SettingsRecipe),
    TestYardOwnerRegistration(OwnerBefore),
    LegacyV1Import {
        static_pin: String,
    },
    SourceImport,
    SourceEntrypoints,
    LedgerAdvance {
        before: crate::store::Snapshot,
        after: Vec<u8>,
    },
}
pub(crate) struct Work {
    pub intent: PlannerStepIntent,
    pub recipe: Recipe,
}

pub(crate) fn owner_payload_hashes(work: &[Work]) -> impl Iterator<Item = &str> {
    work.iter().filter_map(|work| match &work.recipe {
        Recipe::TestYardOwnerRegistration(before) => Some(before.prepared_recipe.as_str()),
        _ => None,
    })
}

impl Work {
    pub(crate) fn settings(
        m: &Migration,
        recipe: SettingsRecipe,
        local: bool,
        decision: &str,
    ) -> Result<Self> {
        require(
            record::safe_name(&recipe.yard)
                && matches!(recipe.target.as_str(), "canonical" | "flat")
                && record::fingerprint(&recipe.inheritance)
                && record::fingerprint(&recipe.expected)
                && record::fingerprint(&recipe.desired)
                && record::fingerprint(&recipe.source)
                && recipe.expected != recipe.desired
                && !recipe.edits.is_empty()
                && recipe.edits.len() <= 256,
        )?;
        require(
            m.domain == "settings"
                && m.kind
                    == if local {
                        "local-ssh-port-settings-v2-to-v3"
                    } else {
                        "test-vms-settings-v1-to-v2"
                    }
                && m.from_epoch == if local { 2 } else { 1 }
                && m.to_epoch == if local { 3 } else { 2 },
        )?;
        require(
            record::fingerprint(&recipe.expected_file)
                && record::fingerprint(&recipe.desired_file)
                && recipe.inputs.len() <= 2048
                && (!recipe.expected_exists
                    || recipe.target == "canonical"
                    || recipe.target == "flat"),
        )?;
        require(
            recipe.expected
                == crate::settings::resource_fingerprint(
                    recipe.expected_exists,
                    &recipe.expected_file,
                    &recipe.inheritance,
                )?
                && recipe.desired
                    == crate::settings::resource_fingerprint(
                        true,
                        &recipe.desired_file,
                        &recipe.inheritance,
                    )?,
        )?;
        if !recipe.expected_exists {
            require(recipe.expected_file == digest(b"") && recipe.target == "canonical")?;
        }
        if local {
            require(
                recipe.edits.len() == 1
                    && recipe.edits[0].setting == "SSH_PORT"
                    && recipe.edits[0].value.as_ref().is_some_and(|v| {
                        v.parse::<u16>().is_ok_and(|n| n > 0 && n.to_string() == *v)
                    })
                    && recipe.before.len() == 1
                    && recipe.before[0].setting == "SSH_PORT"
                    && !recipe.before[0].present
                    && recipe.before[0].value.is_empty(),
            )?;
        } else {
            require(
                recipe.before.len() == 2
                    && recipe.before[0].setting == "YARD_TEMPLATE"
                    && recipe.before[0].present
                    && ["e2e-vms", "test-vms"].contains(&recipe.before[0].value.as_str())
                    && recipe.before[1].setting == "NESTED_E2E_VMS"
                    && if recipe.before[1].present {
                        ["0", "1"].contains(&recipe.before[1].value.as_str())
                    } else {
                        recipe.before[1].value.is_empty()
                    },
            )?;
            let mut expected = vec![];
            if recipe.before[0].value == "e2e-vms" {
                expected.push(Edit {
                    setting: "YARD_TEMPLATE".into(),
                    value: Some("test-vms".into()),
                })
            };
            if recipe.before[1].present && recipe.before[1].value == "0" {
                expected.push(Edit {
                    setting: "NESTED_E2E_VMS".into(),
                    value: None,
                })
            };
            require(
                recipe.edits == expected
                    && decision
                        == if recipe.before[1].present && recipe.before[1].value == "0" {
                            "reset"
                        } else {
                            "canonicalize"
                        },
            )?;
        }
        let intent = PlannerStepIntent {
            id: format!("{}.{}", m.id, recipe.yard),
            migration: m.id.clone(),
            resource: recipe.yard.clone(),
            decision: decision.into(),
            expected: recipe.expected.clone(),
            desired: recipe.desired.clone(),
        };
        Ok(Self {
            intent,
            recipe: if local {
                Recipe::LocalSSHPortPin(recipe)
            } else {
                Recipe::TestVMSettingsMigration(recipe)
            },
        })
    }
    pub(crate) fn ingress(
        kind: IngressKind,
        decision: String,
        expected: String,
        desired: String,
        static_pin: String,
    ) -> Result<Self> {
        let (migration, id, resource, _, _) = kind.labels();
        require(
            record::fingerprint(&expected)
                && record::fingerprint(&desired)
                && matches!(
                    decision.as_str(),
                    "transform" | "canonicalize" | "reset" | "quarantine"
                )
                && (expected != desired
                    || kind == IngressKind::LegacyV1Import && decision == "canonicalize"),
        )?;
        let recipe = match kind {
            IngressKind::LegacyV1Import => {
                require(record::fingerprint(&static_pin))?;
                Recipe::LegacyV1Import { static_pin }
            }
            IngressKind::Import => {
                require(static_pin.is_empty())?;
                Recipe::SourceImport
            }
            IngressKind::Entrypoints => {
                require(static_pin.is_empty())?;
                Recipe::SourceEntrypoints
            }
        };
        Ok(Self {
            intent: PlannerStepIntent {
                id: id.into(),
                migration: migration.into(),
                resource: resource.into(),
                decision,
                expected,
                desired,
            },
            recipe,
        })
    }
}
