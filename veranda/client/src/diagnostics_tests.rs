use super::*;
use std::fs;

fn root() -> PathBuf {
    let mut random = [0u8; 16];
    getrandom::fill(&mut random).unwrap();
    let id: String = random.iter().map(|byte| format!("{byte:02x}")).collect();
    fs::canonicalize(std::env::temp_dir())
        .unwrap()
        .join(format!("veranda-diagnostics-test-{id}"))
        .join("connections")
}

#[test]
fn records_keep_only_validated_names_and_closed_classes() {
    let log = DiagnosticLog::new(root());
    log.record(
        Class::OwnerError,
        "owner.inventory",
        "plan_failed",
        "veranda-7",
    );
    log.record(
        Class::Disconnected,
        "a b\nSECRET",
        "/home/operator",
        "id with space",
    );
    let records = log.records();
    assert_eq!(records.len(), 2);
    assert_eq!(
        (
            records[0].method.as_str(),
            records[0].code.as_str(),
            records[0].request.as_str()
        ),
        ("owner.inventory", "plan_failed", "veranda-7")
    );
    assert_eq!(
        (
            records[1].method.as_str(),
            records[1].code.as_str(),
            records[1].request.as_str()
        ),
        ("unknown", "unknown", "")
    );
    let raw = fs::read_to_string(log.root.join(FILE)).unwrap();
    assert!(!raw.contains("SECRET") && !raw.contains("/home"), "{raw}");
}

#[test]
fn decode_failures_name_the_type_and_field_but_never_the_value() {
    #[derive(Debug, serde::Deserialize)]
    #[allow(dead_code)]
    struct Inventory {
        projects: Vec<String>,
    }
    let log = DiagnosticLog::new(root());
    for value in [
        serde_json::json!({"projects": "SECRET-VALUE"}),
        serde_json::json!({}),
    ] {
        let error = serde_json::from_value::<Inventory>(value).unwrap_err();
        log.decode_failed::<Inventory>("owner.inventory", &error);
    }
    let facts: Vec<_> = log
        .records()
        .into_iter()
        .map(|record| (record.structure, record.path))
        .collect();
    assert_eq!(
        facts,
        vec![
            (Some(Structure::InvalidType), "Inventory".to_string()),
            (
                Some(Structure::MissingField),
                "Inventory.projects".to_string()
            ),
        ]
    );
    let raw = fs::read_to_string(log.root.join(FILE)).unwrap();
    assert!(!raw.contains("SECRET"), "{raw}");
}

#[test]
fn the_log_and_its_previous_generation_stay_bounded() {
    let log = DiagnosticLog::new(root());
    for index in 0..2000 {
        log.record(
            Class::RpcTimeout,
            "owner.inventory",
            "rpc_timeout",
            &format!("veranda-{index}"),
        );
    }
    for name in [FILE.to_string(), format!("{FILE}.1")] {
        let size = fs::metadata(log.root.join(name)).unwrap().len();
        assert!(size <= MAX_FILE, "{size}");
    }
    let records = log.records();
    assert_eq!(records.last().unwrap().request, "veranda-1999");
    assert!(records.len() < 2000 && records.len() > 100);
    assert!(records.windows(2).all(|pair| {
        let number = |record: &Record| record.request["veranda-".len()..].parse::<u32>().unwrap();
        number(&pair[0]) + 1 == number(&pair[1])
    }));
}
