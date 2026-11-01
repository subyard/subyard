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
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub capture: Option<OwnerCapture>,
    #[serde(skip)]
    pub program: Option<crate::program::Program>,
}
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct OwnerCapture {
    pub instance: String,
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
        value.capture = None;
        value.program = None;
        value
    }
    pub(crate) fn desired(&self) -> Self {
        let mut next = self.clone();
        next.state = "current".into();
        next
    }
}
pub(crate) enum Recipe {
    Settings {
        definition: &'static crate::settings::Migration,
        recipe: SettingsRecipe,
    },
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
        definition: &'static crate::settings::Migration,
        recipe: SettingsRecipe,
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
            m.kind == definition.kind
                && m.domain == "settings"
                && m.from_epoch == definition.from_epoch
                && m.to_epoch == definition.to_epoch
                && definition.decisions.contains(&decision),
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
        (definition.validate)(&recipe, decision)?;
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
            recipe: Recipe::Settings { definition, recipe },
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
