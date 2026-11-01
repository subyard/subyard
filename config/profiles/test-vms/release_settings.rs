//! Frozen test-VM settings conversion, compiled into the release owner.
use crate::{
    codec, digest,
    record::{require, safe_name},
    settings::{decision, exact, Conversion, YardSettings},
    work::{BeforeValue, Edit, SettingsRecipe},
    Error, Result,
};
use serde::Serialize;
use std::collections::BTreeSet;
pub(crate) fn conversion(input: &YardSettings) -> Result<Option<Conversion>> {
    require(safe_name(&input.yard) && !(input.canonical_exists && input.flat_exists))?;
    if !input.canonical_exists && !input.flat_exists {
        return Ok(None);
    }
    let (template, present) = exact(input, "YARD_TEMPLATE")?;
    if !present {
        require(!input.inherited.contains("YARD_TEMPLATE"))?;
        return Ok(None);
    }
    if template.is_empty() {
        return Ok(Some(Conversion {
            edits: vec![],
            before: vec![],
            decisions: vec![decision(
                &input.yard,
                "YARD_TEMPLATE",
                "preserve",
                "preserved",
            )],
            decision: "canonicalize".into(),
        }));
    }
    if template != "e2e-vms" && template != "test-vms" {
        require(input.template_supported)?;
        return Ok(Some(Conversion {
            edits: vec![],
            before: vec![],
            decisions: vec![decision(
                &input.yard,
                "YARD_TEMPLATE",
                "preserve",
                "preserved",
            )],
            decision: "canonicalize".into(),
        }));
    }
    require(
        !input.inherited.contains("YARD_TEMPLATE") && !input.inherited.contains("NESTED_E2E_VMS"),
    )?;
    let (nested, nested_present) = exact(input, "NESTED_E2E_VMS")?;
    require(!nested_present || nested == "0" || nested == "1")?;
    let mut edits = vec![];
    let mut decisions = vec![];
    if template == "e2e-vms" {
        edits.push(Edit {
            setting: "YARD_TEMPLATE".into(),
            value: Some("test-vms".into()),
        });
        decisions.push(decision(
            &input.yard,
            "YARD_TEMPLATE",
            "canonicalize",
            "test-vms",
        ))
    } else {
        decisions.push(decision(
            &input.yard,
            "YARD_TEMPLATE",
            "preserve",
            "preserved",
        ))
    }
    let mut file_decision = "canonicalize";
    if nested_present && nested == "0" {
        edits.push(Edit {
            setting: "NESTED_E2E_VMS".into(),
            value: None,
        });
        file_decision = "reset";
        decisions.push(decision(&input.yard, "NESTED_E2E_VMS", "reset", "unset"))
    } else {
        decisions.push(decision(
            &input.yard,
            "NESTED_E2E_VMS",
            "preserve",
            "preserved",
        ))
    }
    Ok(Some(Conversion {
        edits,
        before: vec![
            BeforeValue {
                setting: "YARD_TEMPLATE".into(),
                present,
                value: template,
            },
            BeforeValue {
                setting: "NESTED_E2E_VMS".into(),
                present: nested_present,
                value: nested,
            },
        ],
        decisions,
        decision: file_decision.into(),
    }))
}

// One definition supplies authoring policy, current capability metadata and
// validation of retained settings work. Native settings operations are generic.
pub(crate) const MIGRATION: crate::settings::Migration = crate::settings::Migration {
    prepare: prepare_input,
    kind: "test-vms-settings-v1-to-v2",
    from_epoch: 1,
    to_epoch: 2,
    decisions: &["canonicalize", "reset"],
    validate_recovery,
    resource_class: "yard-settings-file-v1",
    wire_local: false,
    rollback_compatibility: "backward-compatible-no-op",
    legacy_minimum_version: "0.8.0",
    convert: |input, _| conversion(input),
    validate: validate_recipe,
};
fn validate_recovery(values: &[crate::settings::RecoveryValue]) -> Result<()> {
    // Historical schema1 accepts a partial before-value list; current prepared
    // recipes are stricter. Do not tighten admission of frozen source evidence.
    require(values.len() <= 2)?;
    for value in values {
        require(
            ["YARD_TEMPLATE", "NESTED_E2E_VMS"].contains(&value.setting.as_str())
                && (!value.present
                    || match value.setting.as_str() {
                        "YARD_TEMPLATE" => ["e2e-vms", "test-vms"].contains(&value.value.as_str()),
                        "NESTED_E2E_VMS" => ["0", "1"].contains(&value.value.as_str()),
                        _ => false,
                    }),
        )?;
    }
    Ok(())
}

fn validate_recipe(recipe: &SettingsRecipe, decision: &str) -> Result<()> {
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
    Ok(())
}

fn prepare_input(capture: crate::settings::Capture<'_>) -> Result<(YardSettings, String)> {
    let mut input = if capture.overlay_prepared {
        capture.current(false)?
    } else {
        capture.original.clone()
    };
    let (inherited, binding) = template_inheritance(&input, capture.command_ids())?;
    input.inherited = inherited;
    Ok((input, binding))
}
fn template_inheritance(
    yard: &YardSettings,
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
