//! Authoring proof only. An isolated source copy registers this definition;
//! ordinary owner builds and their frozen catalogs never include it.
use crate::{
    record::require,
    settings::{self, Capture, Conversion, RecoveryValue, YardSettings},
    work::{BeforeValue, Edit, SettingsRecipe},
    Result,
};

pub(crate) const MIGRATION: settings::Migration = settings::Migration {
    kind: "author-default-user-setting-v3-to-v4",
    from_epoch: 3,
    to_epoch: 4,
    resource_class: "author-default-user-setting-v1",
    decisions: &["reset"],
    wire_local: false,
    rollback_compatibility: "",
    legacy_minimum_version: "",
    prepare: |capture: Capture<'_>| {
        Ok((
            capture.current(false)?,
            crate::digest(b"direct-default-user-setting-v1"),
        ))
    },
    convert,
    validate,
    validate_recovery,
};

fn convert(input: &YardSettings, _: &[YardSettings]) -> Result<Option<Conversion>> {
    let (value, present) = settings::exact(input, "DEV_USER")?;
    if !present {
        return Ok(None);
    }
    require(value == "dev")?;
    Ok(Some(Conversion {
        edits: vec![Edit {
            setting: "DEV_USER".into(),
            value: None,
        }],
        before: vec![BeforeValue {
            setting: "DEV_USER".into(),
            present: true,
            value,
        }],
        decisions: vec![settings::decision(
            &input.yard,
            "DEV_USER",
            "reset",
            "unset",
        )],
        decision: "reset".into(),
    }))
}

fn validate(recipe: &SettingsRecipe, decision: &str) -> Result<()> {
    require(
        decision == "reset"
            && recipe.edits
                == [Edit {
                    setting: "DEV_USER".into(),
                    value: None,
                }]
            && recipe.before
                == [BeforeValue {
                    setting: "DEV_USER".into(),
                    present: true,
                    value: "dev".into(),
                }],
    )
}

fn validate_recovery(values: &[RecoveryValue]) -> Result<()> {
    require(
        values.len() == 1
            && values[0].setting == "DEV_USER"
            && values[0].present
            && values[0].value == "dev",
    )
}
