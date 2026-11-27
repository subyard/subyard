use crate::compatibility::{ENGINE_CURRENT_LINE, ENGINE_PREVIOUS_LINE};
use crate::vocabulary::{
    Capability, ErrorCode, EventName, Method, CONTRACT_REVISION, PROTOCOL_VERSION,
};
use serde_json::Value;

#[test]
fn generated_vocabulary_matches_vendored_contract() {
    let manifest: Value =
        serde_json::from_str(include_str!("../../contract/yard-rpc/v1/vocabulary.json")).unwrap();
    assert_eq!(manifest["revision"], CONTRACT_REVISION);
    assert_eq!(manifest["rpcVersion"], PROTOCOL_VERSION);
    let sections = [
        (
            "errorCodes",
            ErrorCode::ALL
                .iter()
                .map(|v| v.as_str())
                .collect::<Vec<_>>(),
        ),
        (
            "capabilities",
            Capability::ALL.iter().map(|v| v.as_str()).collect(),
        ),
        ("methods", Method::ALL.iter().map(|v| v.as_str()).collect()),
        (
            "events",
            EventName::ALL.iter().map(|v| v.as_str()).collect(),
        ),
    ];
    for (section, mut names) in sections {
        let mut expected: Vec<_> = manifest[section]
            .as_object()
            .unwrap()
            .keys()
            .map(String::as_str)
            .collect();
        names.sort_unstable();
        expected.sort_unstable();
        assert_eq!(names, expected, "{section}");
    }
    for code in ErrorCode::ALL {
        assert_eq!(ErrorCode::parse(code.as_str()), Some(*code));
    }
    for cap in Capability::ALL {
        assert_eq!(Capability::parse(cap.as_str()), Some(*cap));
    }
    for method in Method::ALL {
        assert_eq!(Method::parse(method.as_str()), Some(*method));
    }
    for event in EventName::ALL {
        assert_eq!(EventName::parse(event.as_str()), Some(*event));
    }
    assert_eq!(ErrorCode::parse("future-error"), None);
    assert_eq!(Capability::parse("future-capability"), None);
    assert_eq!(EventName::parse("future.event"), None);
}

#[test]
fn compatibility_names_known_contract_and_capabilities() {
    let policy: Value = serde_json::from_str(include_str!("../../compatibility.json")).unwrap();
    assert_eq!(policy["contractRevision"], CONTRACT_REVISION);
    assert_eq!(policy["rpc"]["minimum"], PROTOCOL_VERSION);
    assert_eq!(policy["rpc"]["maximum"], PROTOCOL_VERSION);
    assert_eq!(policy["productVersionPolicy"], "engine-window");
    assert_eq!(
        policy["engineLines"],
        serde_json::json!({"current": ENGINE_CURRENT_LINE, "previous": ENGINE_PREVIOUS_LINE})
    );
    assert_eq!(policy["productVersion"], env!("CARGO_PKG_VERSION"));
    assert_eq!(
        policy["requiredCapabilities"],
        serde_json::json!(["owner-inventory-v1"])
    );
    let mut sets = vec![policy["requiredCapabilities"].as_array().unwrap()];
    sets.extend(
        policy["featureCapabilities"]
            .as_object()
            .unwrap()
            .values()
            .map(|v| v.as_array().unwrap()),
    );
    for set in sets {
        for cap in set {
            assert!(Capability::parse(cap.as_str().unwrap()).is_some(), "{cap}");
        }
    }
    let expected = serde_json::json!({
        "yardDetails": ["profile-list-v1", "settings-list-v1"],
        "hostSettings": ["settings-list-v1", "host-sync-status-v1"],
        "operations": ["operation-exact-plan-v1", "operation-steps-v1"],
        "sessions": ["session-prepare-v1"],
        "yardCreation": ["yard-bootstrap-v1", "profile-list-v1", "operation-exact-plan-v1", "operation-steps-v1"],
        "orderedEvents": ["ordered-events"]
    });
    assert_eq!(policy["featureCapabilities"], expected);
}

// A new engine line moves the window; veranda/README.md#engine-window has the refresh step.
#[test]
fn previous_engine_snapshot_is_the_oldest_admitted_release() {
    const STEP: &str = "refresh the previous-engine snapshot: veranda/README.md#engine-window";
    let release: Value =
        serde_json::from_str(include_str!("../../contract/previous-engine/release.json")).unwrap();
    assert_eq!(
        release,
        serde_json::json!({"version": format!("{ENGINE_PREVIOUS_LINE}.0")}),
        "{STEP}"
    );
    let negotiate: Value = serde_json::from_str(include_str!(
        "../../contract/previous-engine/fixtures/negotiate.json"
    ))
    .unwrap();
    let advertised = negotiate["result"]["capabilities"].as_array().unwrap();
    let policy: Value = serde_json::from_str(include_str!("../../compatibility.json")).unwrap();
    // A capability new on the current line can only be a feature capability.
    for required in policy["requiredCapabilities"].as_array().unwrap() {
        assert!(advertised.contains(required), "{required}: {STEP}");
    }
}
