#![forbid(unsafe_code)]
//! The independent Go bitset oracle supplies expected results. This consumer
//! calls production history directly; no development binary or framing adapter.
use serde::Deserialize;
use serde_json::{json, Value};
use std::{collections::BTreeMap, env, fs};
use subyard_release_owner::history::{
    CompatibilityProjection, Registry, RegistryInput, StateInput,
};

#[derive(Deserialize)]
#[serde(deny_unknown_fields, rename_all = "camelCase")]
struct Corpus {
    schema_version: u32,
    cases: Vec<Row>,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Row {
    input: Case,
    expected: Value,
}
#[derive(Deserialize)]
#[serde(tag = "op", rename_all = "snake_case", deny_unknown_fields)]
enum Case {
    History {
        registry: RegistryInput,
        states: BTreeMap<String, StateInput>,
        traces: Vec<Vec<String>>,
    },
    Registry {
        registry: RegistryInput,
    },
    Binding {
        registry: RegistryInput,
        other: RegistryInput,
    },
    Projection {
        epochs: BTreeMap<String, i64>,
        #[serde(default)]
        pin: String,
    },
}
fn evaluate(case: &Case) -> Value {
    match case {
        Case::Registry { registry } => json!({"accepted": Registry::new(registry.clone()).is_ok()}),
        Case::Projection { epochs, pin } => {
            json!({"accepted": CompatibilityProjection::new(epochs.clone(), pin.clone()).is_ok()})
        }
        Case::Binding { registry, other } => {
            let (Ok(r), Ok(other)) = (
                Registry::new(registry.clone()),
                Registry::new(other.clone()),
            ) else {
                return json!({"accepted":false});
            };
            let Ok(edge) = r.next(&r.baseline()) else {
                return json!({"accepted":false});
            };
            let base = r.baseline();
            let ids = r
                .pending(&base)
                .unwrap()
                .iter()
                .map(|m| m.id.clone())
                .collect::<Vec<_>>();
            let trace = r.trace_from(&base, &ids).unwrap();
            let completion = r.complete(trace.terminal()).unwrap();
            json!({"accepted":other.check_edge_binding(&edge).is_ok(), "identity":other.check_edge(&edge).is_ok(), "completionIdentity":other.check_completion(&completion).is_ok()})
        }
        Case::History {
            registry,
            states,
            traces,
        } => {
            let Ok(r) = Registry::new(registry.clone()) else {
                return json!({"accepted":false});
            };
            let Ok(a) = r.history(states.clone()) else {
                return json!({"accepted":false});
            };
            let pending: Vec<_> = r.pending(&a).unwrap().iter().map(|m| &m.id).collect();
            let next = match r.next(&a) {
                Ok(e) => json!({"id":e.migration().id,"after":e.after().domains()}),
                Err(_) => Value::Null,
            };
            let traces: Vec<_> = traces.iter().map(|ids| match r.trace_from(&a, ids) {
                Ok(t) => json!({"accepted":true, "initial":t.initial().domains(), "terminal":t.terminal().domains(), "edges":t.edges().iter().map(|e| &e.migration().id).collect::<Vec<_>>()}),
                Err(_) => json!({"accepted":false}),
            }).collect();
            json!({"accepted":true,"pending":pending,"next":next,"complete":r.complete(&a).is_ok(),"traces":traces})
        }
    }
}

#[test]
#[ignore = "requires generated Go corpus; owning scripts run this"]
fn production_history_matches_independent_go_oracle() {
    let path = env::var("SUBYARD_RUST_HISTORY_CORPUS")
  .expect("required history corpus: run Go TestRustHistoryOracleCorpus with SUBYARD_RUST_HISTORY_CORPUS first");
    let raw = fs::read(path).expect("read required independent Go history corpus");
    let corpus: Corpus = serde_json::from_slice(&raw).expect("decode Go history corpus");
    assert_eq!(corpus.schema_version, 1);
    assert_eq!(
        corpus.cases.len(),
        70,
        "incomplete independent history corpus"
    );
    for (index, row) in corpus.cases.iter().enumerate() {
        assert_eq!(
            evaluate(&row.input),
            row.expected,
            "independent history case {index}"
        );
    }
}
