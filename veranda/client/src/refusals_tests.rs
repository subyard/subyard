use super::*;
use serde_json::json;

fn fixture(name: &str) -> Value {
    let path = format!(
        "{}/../contract/yard-rpc/v1/fixtures/{name}.json",
        env!("CARGO_MANIFEST_DIR")
    );
    serde_json::from_str(&std::fs::read_to_string(path).unwrap()).unwrap()
}

#[test]
fn golden_gate_refusals_render_every_cause_and_typed_next_step() {
    let frame = fixture("error-gate-refusal");
    let refusals = parse(&frame["error"]["data"]).unwrap();
    assert_eq!(refusals.items.len(), 2);
    assert_eq!(refusals.items[0].code, "precondition-blocked");
    assert_eq!(
        refusals.items[0].command.as_deref(),
        Some("yard -Y default start")
    );
    assert_eq!(
        refusals.lines(),
        vec![
            "• yard default has a pending configuration change",
            "  Next: start the yard, then run yard -Y default start",
            "• profile route-client runtime is unavailable",
            "  Next: run yard migrate",
        ]
    );
}

#[test]
fn golden_config_refusals_never_show_the_owner_message() {
    let frame = fixture("error-config-refusal");
    let mut error = crate::local_fleet::NativeError::new("owner_request_failed", "Fixed text.");
    error.refusals = Some(parse(&frame["error"]["data"]).unwrap());
    let summary = error.summary();
    assert!(summary.starts_with("owner_request_failed: Fixed text.\n• profile \"route-client\""));
    assert!(summary.contains("• selected yard role forbids non-empty HOST_LINKS\n  Next: "));
    assert!(!summary.contains(frame["error"]["message"].as_str().unwrap()));
}

fn valid() -> Value {
    json!({"schemaVersion": 1, "future": {"ignored": true}, "refusals": [
        {"source": "config", "code": "role-forbids-setting", "text": "t", "future": 1}
    ]})
}

#[test]
fn unknown_fields_are_ignored_and_markers_are_rendered() {
    let mut data = valid();
    data["omitted"] = json!(3);
    data["incomplete"] = json!(true);
    let refusals = parse(&data).unwrap();
    assert_eq!(refusals.items.len(), 1);
    let lines = refusals.lines();
    assert_eq!(lines[1], "3 more refusals; review the owner's diagnostics.");
    assert_eq!(
        lines[2],
        "Other causes are reported only in the owner's diagnostics."
    );
}

#[test]
fn data_outside_the_contract_is_rejected_whole() {
    type Mutation = Box<dyn Fn(&mut Value)>;
    let cases: Vec<(&str, Mutation, Rejected)> = vec![
        (
            "not an object",
            Box::new(|v| *v = json!("text")),
            Rejected::Shape,
        ),
        (
            "schema",
            Box::new(|v| v["schemaVersion"] = json!(2)),
            Rejected::Schema,
        ),
        (
            "empty",
            Box::new(|v| v["refusals"] = json!([])),
            Rejected::Bounds,
        ),
        (
            "too many",
            Box::new(|v| {
                let one = v["refusals"][0].clone();
                v["refusals"] = Value::Array(vec![one; MAX_REFUSALS + 1]);
            }),
            Rejected::Bounds,
        ),
        (
            "source",
            Box::new(|v| v["refusals"][0]["source"] = json!("shell")),
            Rejected::Text,
        ),
        (
            "code",
            Box::new(|v| v["refusals"][0]["code"] = json!("../x")),
            Rejected::Text,
        ),
        (
            "control",
            Box::new(|v| v["refusals"][0]["text"] = json!("a\u{1b}[31m")),
            Rejected::Text,
        ),
        (
            "blank",
            Box::new(|v| v["refusals"][0]["text"] = json!("")),
            Rejected::Text,
        ),
        (
            "long",
            Box::new(|v| v["refusals"][0]["next"] = json!("x".repeat(MAX_TEXT + 1))),
            Rejected::Text,
        ),
        (
            "command",
            Box::new(|v| {
                v["refusals"][0]["action"] = json!({"verb": "start", "command": "rm -rf /"})
            }),
            Rejected::Text,
        ),
        (
            "type",
            Box::new(|v| v["refusals"][0]["text"] = json!(7)),
            Rejected::Shape,
        ),
    ];
    for (name, mutate, want) in cases {
        let mut data = valid();
        mutate(&mut data);
        assert_eq!(parse(&data), Err(want), "{name}");
    }
}
