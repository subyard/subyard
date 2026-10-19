//! Frozen test-VM settings conversion, compiled into the release owner.
use crate::{
    record::{require, safe_name},
    settings::{decision, exact, Conversion, YardSettings},
    work::{BeforeValue, Edit},
    Result,
};
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
