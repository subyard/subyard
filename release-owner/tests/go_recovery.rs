use serde::Deserialize;
use std::{collections::BTreeMap, fs, os::unix::fs::PermissionsExt, path::Path};
use subyard_release_owner::{
    codec, graph, migration_owner, plan, recovery, store::Store, wire, Error, Result,
};
#[derive(Deserialize)]
struct Corpus {
    vectors: Vec<Vector>,
    policies: Vec<Policy>,
    sources: Vec<SourceCase>,
    graphs: Vec<GraphCase>,
    journals: Vec<JournalCase>,
}
#[derive(Deserialize)]
struct Vector {
    name: String,
    kind: String,
    input: String,
    accepted: bool,
    canonical: String,
    token: String,
}
#[derive(Deserialize)]
struct Policy {
    name: String,
    operation: String,
    before: BTreeMap<String, String>,
    after: BTreeMap<String, String>,
    accepted: bool,
}
fn process(v: &Vector) -> Result<(Vec<u8>, String)> {
    macro_rules! checked {
        ($ty:ty,$new:path,$lf:expr) => {{
            let dto: $ty = serde_json::from_str(&v.input).map_err(|_| Error::Codec)?;
            let value = $new(dto)?;
            Ok((codec::encode(value.wire(), $lf)?, String::new()))
        }};
    }
    match v.kind.as_str() {
        "receipt" => checked!(
            wire::recovery_v1::RecoveryReceiptV1,
            recovery::Receipt::new,
            true
        ),
        "terminal" => checked!(
            wire::lifecycle_v1::RecoveryLifecycleRecord,
            recovery::Terminal::new,
            true
        ),
        "frontier" => checked!(
            wire::lifecycle_v1::RecoveryFrontier,
            recovery::Frontier::new,
            true
        ),
        "migrationOwner" => checked!(
            wire::migration_owner_v1::MigrationOwnerReceiptV1,
            migration_owner::Receipt::from_canonical_fixture,
            true
        ),
        "request2" => {
            let value = recovery::ProcessRequest::new(
                serde_json::from_str(&v.input).map_err(|_| Error::Codec)?,
            )?;
            Ok((value.bytes()?, String::new()))
        }
        "response2" => checked!(
            wire::process_v2::RecoveryProcessResponse,
            recovery::ProcessResponse::new,
            false
        ),
        "assessment" => {
            #[derive(Deserialize)]
            struct Facts {
                changed: bool,
                rollback: bool,
                consequences: Vec<String>,
            }
            let f: Facts = serde_json::from_str(&v.input).map_err(|_| Error::Codec)?;
            Ok((
                codec::encode(
                    &plan::assess(f.changed, f.rollback, &f.consequences)?,
                    false,
                )?,
                String::new(),
            ))
        }
        "plan" => {
            let value = plan::Plan::new(serde_json::from_str(&v.input).map_err(|_| Error::Codec)?)?;
            Ok((codec::encode(value.wire(), false)?, value.token().into()))
        }
        "resume" => {
            let value =
                plan::ResumePlan::new(serde_json::from_str(&v.input).map_err(|_| Error::Codec)?)?;
            Ok((codec::encode(value.wire(), false)?, value.token().into()))
        }
        _ => panic!("unknown owning case"),
    }
}
fn corpus() -> Option<Corpus> {
    let p = std::env::var("SUBYARD_RUST_OWNER_CORPUS").ok()?;
    let bytes = fs::read(format!("{p}.recovery.json")).unwrap();
    assert!(bytes.len() < 4 << 20);
    Some(serde_json::from_slice(&bytes).unwrap())
}
#[test]
fn go_owned_recovery_formats() {
    let Some(c) = corpus() else { return };
    for v in &c.vectors {
        let result = process(v);
        assert_eq!(result.is_ok(), v.accepted, "{}: {:?}", v.name, result);
        if let Ok((bytes, token)) = result {
            assert_eq!(bytes, v.canonical.as_bytes(), "{} canonical", v.name);
            assert_eq!(token, v.token, "{} token", v.name)
        }
    }
    println!("{} Go recovery format/plan vectors", c.vectors.len());
}
fn dirs(path: &Path) {
    if path.exists() {
        return;
    }
    dirs(path.parent().unwrap());
    fs::create_dir(path).unwrap();
    fs::set_permissions(path, fs::Permissions::from_mode(0o700)).unwrap()
}
fn files(root: &Path, path: &Path, out: &mut BTreeMap<String, String>) {
    for entry in fs::read_dir(path).unwrap() {
        let e = entry.unwrap();
        let p = e.path();
        if e.file_type().unwrap().is_dir() {
            files(root, &p, out)
        } else {
            let key = p.strip_prefix(root).unwrap().to_str().unwrap();
            if key != "migrations/update.lock" {
                out.insert(key.into(), fs::read_to_string(p).unwrap());
            }
        }
    }
}
#[test]
fn go_owned_retirement_outcomes() {
    let Some(c) = corpus() else { return };
    for (i, p) in c.policies.iter().enumerate() {
        assert!(["retire", "cancel", "prune", "resume"].contains(&p.operation.as_str()));
        let root =
            std::env::temp_dir().join(format!("subyard-rust-recovery-{}-{i}", std::process::id()));
        assert!(!root.exists());
        dirs(&root);
        for (name, value) in &p.before {
            assert!(!name.starts_with('/') && !name.split('/').any(|s| s == ".."));
            let path = root.join(name);
            dirs(path.parent().unwrap());
            fs::write(&path, value).unwrap();
            fs::set_permissions(path, fs::Permissions::from_mode(0o600)).unwrap()
        }
        let store = if let Ok(helper) = std::env::var("SUBYARD_RUST_CODEC_HELPER") {
            Store::open_with_reader(
                &root,
                subyard_release_owner::legacy::FrozenReader::capture_helper(
                    fs::File::open(helper).unwrap(),
                )
                .unwrap(),
            )
            .unwrap()
        } else {
            Store::open_canonical_fixture(&root).unwrap()
        };
        let lease = store.lock().unwrap();
        let result = if p.operation == "resume" {
            let bytes = store
                .read(&subyard_release_owner::store::Address::journal())
                .unwrap();
            let journal = subyard_release_owner::record::Journal::new(
                codec::decode_canonical(bytes.bytes().unwrap(), true).unwrap(),
            )
            .unwrap();
            store.recovery_for(&journal).map(|_| ())
        } else if p.operation == "cancel" {
            let vector = c
                .vectors
                .iter()
                .find(|v| v.name == recovery::ACTIVATION_V2)
                .unwrap();
            let receipt =
                recovery::Receipt::new(serde_json::from_str(&vector.canonical).unwrap()).unwrap();
            let base = format!("plan-v1-{}", "b".repeat(64));
            let pin = subyard_release_owner::digest(&codec::encode(receipt.wire(), true).unwrap());
            let plan = recovery::bind_lifecycle_plan(&base, &pin).unwrap();
            lease.cancel_recovery(&receipt, &base, &plan, &"b".repeat(64))
        } else {
            graph::State::read(&store)
                .and_then(|state| {
                    if p.operation == "prune" {
                        state.archive_pruning(32, 0)
                    } else {
                        state.retirement()
                    }
                })
                .and_then(|plan| lease.retire(plan))
        };
        assert_eq!(result.is_ok(), p.accepted, "{}: {:?}", p.name, result);
        let mut actual = BTreeMap::new();
        files(&root, &root, &mut actual);
        assert_eq!(actual, p.after, "{} retained/deleted records", p.name);
        drop(lease);
        drop(store);
        fs::remove_dir_all(root).unwrap();
    }
    println!(
        "{} Go store before/after retirement cases",
        c.policies.len()
    );
}

#[derive(Deserialize)]
struct SourceCase {
    name: String,
    before: BTreeMap<String, String>,
    registry: String,
    ledger: String,
    transaction: String,
    accepted: bool,
    steps: Vec<wire::journal_v2::Step>,
}
#[test]
fn go_owned_terminal_sources() {
    let Some(c) = corpus() else { return };
    for (i, p) in c.sources.iter().enumerate() {
        let root =
            std::env::temp_dir().join(format!("subyard-rust-source-{}-{i}", std::process::id()));
        assert!(!root.exists());
        dirs(&root);
        for (name, value) in &p.before {
            assert!(!name.starts_with('/') && !name.split('/').any(|s| s == ".."));
            let path = root.join(name);
            dirs(path.parent().unwrap());
            fs::write(&path, value).unwrap();
            fs::set_permissions(path, fs::Permissions::from_mode(0o600)).unwrap()
        }
        let catalog = migration_owner::builtin_catalog(true, true).unwrap();
        let registry = subyard_release_owner::record::Registry::new(
            serde_json::from_str(&p.registry).unwrap(),
            &catalog,
        )
        .unwrap();
        let ledger = subyard_release_owner::record::Ledger::new(
            serde_json::from_str(&p.ledger).unwrap(),
            &registry,
        )
        .unwrap();
        let store = if let Ok(helper) = std::env::var("SUBYARD_RUST_CODEC_HELPER") {
            Store::open_with_reader(
                &root,
                subyard_release_owner::legacy::FrozenReader::capture_helper(
                    fs::File::open(helper).unwrap(),
                )
                .unwrap(),
            )
            .unwrap()
        } else {
            Store::open_canonical_fixture(&root).unwrap()
        };
        let before = store.source_metadata();
        let result = store.retained_terminal(&p.transaction, &registry, &ledger);
        assert_eq!(result.is_ok(), p.accepted, "{}", p.name);
        if let Ok(value) = result {
            assert_eq!(value.steps(), p.steps, "{} reconstructed history", p.name)
        }
        if let Ok(before) = before {
            assert_eq!(
                before.binding(),
                store.source_metadata().unwrap().binding(),
                "read-only source proof"
            )
        };
        drop(store);
        fs::remove_dir_all(root).unwrap();
    }
    println!(
        "{} Go source terminal/typed sidecar proofs",
        c.sources.len()
    );
}

#[derive(Deserialize)]
struct GraphCase {
    name: String,
    before: BTreeMap<String, String>,
    directories: Vec<String>,
    accepted: bool,
    selected: Vec<String>,
}
#[test]
fn go_owned_ordinary_graph() {
    let Some(c) = corpus() else { return };
    for (i, p) in c.graphs.iter().enumerate() {
        let root =
            std::env::temp_dir().join(format!("subyard-rust-graph-{}-{i}", std::process::id()));
        assert!(!root.exists());
        dirs(&root);
        for name in &p.directories {
            dirs(&root.join(name))
        }
        for (name, value) in &p.before {
            let path = root.join(name);
            dirs(path.parent().unwrap());
            fs::write(&path, value).unwrap();
            fs::set_permissions(path, fs::Permissions::from_mode(0o600)).unwrap()
        }
        let store = if let Ok(helper) = std::env::var("SUBYARD_RUST_CODEC_HELPER") {
            Store::open_with_reader(
                &root,
                subyard_release_owner::legacy::FrozenReader::capture_helper(
                    fs::File::open(helper).unwrap(),
                )
                .unwrap(),
            )
            .unwrap()
        } else {
            Store::open_canonical_fixture(&root).unwrap()
        };
        let result = graph::State::read(&store).and_then(|s| s.ordinary_cleanup(32));
        assert_eq!(result.is_ok(), p.accepted, "{} {:?}", p.name, result);
        if let Ok(selected) = result {
            assert_eq!(selected, p.selected, "{} safe keep/delete ordering", p.name)
        }
        drop(store);
        fs::remove_dir_all(root).unwrap()
    }
    println!(
        "{} actual Go ordinary graph selection/refusal cases",
        c.graphs.len()
    );
}

#[derive(Deserialize)]
struct JournalCase {
    facts: wire::plan_v1::PlanFacts,
    transaction: String,
    authorization: String,
    journal: String,
}
#[test]
fn go_owned_checked_journal_construction() {
    let Some(c) = corpus() else { return };
    for case in c.journals {
        let plan = plan::Plan::new(case.facts).unwrap();
        let journal = plan
            .journal(case.transaction, &case.authorization, None)
            .unwrap();
        assert_eq!(
            codec::encode(journal.wire(), true).unwrap(),
            case.journal.as_bytes(),
            "actual Go newJournal"
        );
    }
}
