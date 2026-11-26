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
    #[serde(deny_unknown_fields)]
    #[allow(dead_code)]
    struct Inventory {
        projects: Vec<String>,
    }
    let log = DiagnosticLog::new(root());
    for value in [
        serde_json::json!({"projects": "SECRET-VALUE"}),
        serde_json::json!({}),
        serde_json::json!({"projects": [], "SECRET_KEY": 1}),
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
            (Some(Structure::UnknownField), "Inventory".to_string()),
        ]
    );
    let raw = fs::read_to_string(log.root.join(FILE)).unwrap();
    assert!(!raw.contains("SECRET"), "{raw}");
}

#[test]
fn the_log_and_its_previous_generation_stay_bounded() {
    let log = DiagnosticLog::new(root());
    for index in 0..2000 {
        // Alternating methods, so no record repeats the previous one.
        log.record(
            Class::RpcTimeout,
            ["owner.inventory", "owner.status"][index % 2],
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

#[test]
fn a_repeated_failure_updates_its_first_record_instead_of_filling_the_log() {
    let log = DiagnosticLog::new(root());
    log.record(
        Class::OwnerError,
        "owner.inventory",
        "plan_failed",
        "veranda-1",
    );
    for index in 2..5000 {
        log.record(
            Class::Disconnected,
            "rpc.negotiate",
            "disconnected",
            &format!("veranda-{index}"),
        );
    }
    log.record(
        Class::Disconnected,
        "owner.inventory",
        "disconnected",
        "veranda-5000",
    );
    let records = log.records();
    let facts: Vec<_> = records
        .iter()
        .map(|record| {
            (
                record.method.as_str(),
                record.request.as_str(),
                record.count,
            )
        })
        .collect();
    assert_eq!(
        facts,
        vec![
            ("owner.inventory", "veranda-1", 1),
            ("rpc.negotiate", "veranda-2", 4998),
            ("owner.inventory", "veranda-5000", 1),
        ]
    );
    assert!(records[1].last >= records[1].at && records[1].at > 0);
    assert_eq!(records[0].last, 0);
    assert!(!log.root.join(format!("{FILE}.1")).exists());
}
