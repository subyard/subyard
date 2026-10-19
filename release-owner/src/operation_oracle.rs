//! Test-only owning Go trajectory consumer; no production authority hooks.
use crate::{
    record,
    wire::{journal_v2 as j, plan_v1 as f},
};
use base64::Engine;
use serde::Deserialize;
use std::collections::BTreeSet;
#[test]
fn actual_go_operation_plan_and_durable_journal_bytes() {
    #[derive(Deserialize)]
    struct Operation {
        name: String,
        #[serde(with = "crate::wire::go_bytes")]
        registry: Option<Vec<u8>>,
        catalog: Option<Vec<crate::wire::registry_v2::CapabilityDescriptor>>,
        initial: f::PlanFacts,
        journals: Vec<j::Record>,
    }
    let Ok(path) = std::env::var("SUBYARD_RUST_OPERATION_TRACE_CORPUS") else {
        return;
    };
    let raw = std::fs::read(path).unwrap();
    assert!(raw.len() <= 8 << 20);
    let rows: Vec<Operation> = serde_json::from_slice(&raw).unwrap();
    let names = rows
        .iter()
        .map(|r| r.name.as_str())
        .collect::<BTreeSet<_>>();
    assert_eq!(names, required_names());
    for row in rows {
        let catalog = record::Catalog::new(row.catalog).unwrap();
        let registry = row.registry.unwrap();
        let _checked =
            record::Registry::new(serde_json::from_slice(&registry).unwrap(), &catalog).unwrap();
        assert_eq!(crate::digest(&registry), row.initial.registry_digest);
        assert_eq!(catalog.digest(), row.initial.catalog_digest);
        let plan = crate::plan::Plan::new(row.initial).unwrap();
        let first = row.journals.first().unwrap();
        let journal = plan
            .journal(
                first.transaction.clone(),
                &format!("test-grant.{}", plan.token()),
                None,
            )
            .unwrap();
        assert_eq!(
            crate::codec::encode(journal.wire(), true).unwrap(),
            crate::codec::encode(first, true).unwrap()
        );
        for observed in row.journals {
            let observed = record::Journal::new(observed).unwrap();
            assert_eq!(observed.wire().intent_digest, journal.wire().intent_digest);
            assert_eq!(observed.wire().registry_digest, crate::digest(&registry));
        }
    }
}

fn required_names() -> BTreeSet<&'static str> {
    [
        "success",
        "apply-error-after-effect",
        "failed-reobserve",
        "changed-final-observation",
        "cleanup-failure",
        "owner-cleanup-success",
        "owner-cleanup-failure",
        "owner-original-progress",
    ]
    .into_iter()
    .collect()
}

// The process replay performs the real operation. This independent consumer
// checks its exact durable bytes and native-bound journal constructor; it does
// not replace that operation with a precomputed reducer or trajectory table.
#[test]
fn executed_operation_exact_native_plan_and_journal_bindings() {
    #[derive(Deserialize)]
    #[serde(rename_all = "camelCase")]
    struct Actual {
        plan: f::PlanFacts,
        journals: Vec<j::Record>,
        raw_journals: Vec<String>,
    }
    let Ok(directory) = std::env::var("SUBYARD_RUST_OPERATION_REPLAY_OUTPUT") else {
        return;
    };
    for name in required_names() {
        let raw =
            std::fs::read(std::path::Path::new(&directory).join(format!("{name}-actual.json")))
                .unwrap();
        let actual: Actual = serde_json::from_slice(&raw).unwrap();
        let raw_journals: Vec<Vec<u8>> = actual
            .raw_journals
            .iter()
            .map(|raw| {
                base64::engine::general_purpose::STANDARD
                    .decode(raw)
                    .unwrap()
            })
            .collect();
        let plan = crate::plan::Plan::new(actual.plan).unwrap();
        let first = actual.journals.first().unwrap();
        let expected = plan
            .journal(first.transaction.clone(), "fixture-private-consent", None)
            .unwrap();
        assert_eq!(
            crate::codec::encode(expected.wire(), true).unwrap(),
            raw_journals[0],
            "{name}: exact first durable bytes"
        );
        assert_eq!(actual.journals.len(), raw_journals.len());
        for (journal, raw) in actual.journals.iter().zip(raw_journals) {
            let checked = record::Journal::new(journal.clone()).unwrap();
            assert_eq!(crate::codec::encode(checked.wire(), true).unwrap(), raw);
            let mut immutable = checked.wire().clone();
            immutable.checkpoint = first.checkpoint.clone();
            immutable.steps = first.steps.clone();
            assert_eq!(immutable, *first, "{name}: immutable journal changed");
        }
        for resource in ["native.yard-set", "native.prepared-recipes"] {
            let mut changed = plan.wire().clone();
            let bound = changed
                .observations
                .as_mut()
                .unwrap()
                .iter_mut()
                .find(|o| o.resource == resource)
                .unwrap();
            bound.fingerprint = crate::digest(format!("changed-{resource}").as_bytes());
            assert_ne!(
                crate::plan::Plan::new(changed).unwrap().token(),
                plan.token(),
                "{name}: native source binding escaped consent"
            );
        }
    }
}
