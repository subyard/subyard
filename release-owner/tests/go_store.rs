use serde::Deserialize;
use std::{
    collections::BTreeMap,
    fs,
    os::unix::fs::PermissionsExt,
    path::{Path, PathBuf},
};
use subyard_release_owner::{
    codec, record, recovery,
    store::{Address, Store},
    Error, Result,
};
#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Case {
    name: String,
    operation: String,
    arguments: serde_json::Value,
    before: BTreeMap<String, String>,
    before_dirs: Vec<String>,
    after: BTreeMap<String, String>,
    after_dirs: Vec<String>,
    accepted: bool,
    result: String,
    #[serde(default)]
    equivalent_groups: Vec<Vec<String>>,
}
fn dirs(path: &Path) {
    if path.exists() {
        return;
    }
    dirs(path.parent().unwrap());
    fs::create_dir(path).unwrap();
    fs::set_permissions(path, fs::Permissions::from_mode(0o700)).unwrap()
}
fn snapshot(
    root: &Path,
    path: &Path,
    files: &mut BTreeMap<String, String>,
    directories: &mut Vec<String>,
) {
    let mut entries = fs::read_dir(path)
        .unwrap()
        .map(|e| e.unwrap())
        .collect::<Vec<_>>();
    entries.sort_by_key(|e| e.file_name());
    for entry in entries {
        let p = entry.path();
        let name = p.strip_prefix(root).unwrap().to_str().unwrap().to_owned();
        if name == "migrations" {
            continue;
        }
        if entry.file_type().unwrap().is_dir() {
            directories.push(name);
            snapshot(root, &p, files, directories)
        } else {
            files.insert(name, fs::read_to_string(p).unwrap());
        }
    }
}
fn run(store: &Store, case: &Case) -> Result<String> {
    let lease = store.lock()?;
    match case.operation.as_str() {
        "frontier" => {
            lease.publish_frontier(&recovery::Frontier::new(
                serde_json::from_value(case.arguments.clone()).map_err(|_| Error::Codec)?,
            )?)?;
            Ok(String::new())
        }
        "replace" | "replace-cleanup" => {
            let value = case.arguments.get("next").unwrap_or(&case.arguments);
            let next = record::Journal::new(
                serde_json::from_value(value.clone()).map_err(|_| Error::Codec)?,
            )?;
            lease.replace_journal(&store.read(&Address::journal())?, &next)?;
            if case.operation == "replace-cleanup" {
                lease.cleanup_recovery(&next.wire().transaction)?;
            }
            Ok(String::new())
        }
        "resolve-superseded" => {
            let a = &case.arguments;
            let replacement =
                serde_json::from_value(a["replacement"].clone()).map_err(|_| Error::Codec)?;
            lease.resolve_superseded(
                a["proposed"].as_str().ok_or(Error::Codec)?,
                &replacement,
                a["plan"].as_str().ok_or(Error::Codec)?,
            )
        }
        "resolve-v1" | "resolve-v2" => {
            let a = &case.arguments;
            let request = serde_json::from_value(a["request"].clone()).map_err(|_| Error::Codec)?;
            let s = |key: &str| a[key].as_str().ok_or(Error::Codec);
            if case.operation == "resolve-v1" {
                lease.resolve_recovery(s("proposed")?, &request, s("base")?)
            } else {
                lease.resolve_lifecycle(
                    s("proposed")?,
                    &request,
                    s("base")?,
                    s("plan")?,
                    s("grant")?,
                )
            }
        }
        "compact" => {
            let request =
                serde_json::from_value(case.arguments.clone()).map_err(|_| Error::Codec)?;
            lease.compact_cancellations(&request)?;
            Ok(String::new())
        }
        "cleanup-recovery" => {
            lease.cleanup_recovery(case.arguments["current"].as_str().ok_or(Error::Codec)?)?;
            Ok(String::new())
        }
        "cleanup-transactions" => {
            lease.cleanup_transactions(case.arguments.as_str().ok_or(Error::Codec)?)?;
            Ok(String::new())
        }
        "source" => {
            let catalog = subyard_release_owner::migration_owner::builtin_catalog(true, true)?;
            let registry = record::Registry::new(
                serde_json::from_value(case.arguments["registry"].clone())
                    .map_err(|_| Error::Codec)?,
                &catalog,
            )?;
            let proof = store.inspect_source(
                &registry,
                case.arguments["digest"].as_str().ok_or(Error::Codec)?,
            )?;
            Ok(String::from_utf8(codec::encode(proof.ledger().ledger().wire(), true)?).unwrap())
        }
        _ => panic!("closed owning mutation case"),
    }
}
#[test]
fn actual_go_store_orchestration() {
    let Ok(base) = std::env::var("SUBYARD_RUST_OWNER_CORPUS") else {
        return;
    };
    let base = format!("{base}.recovery.json");
    let names: Vec<String> =
        serde_json::from_slice(&fs::read(format!("{base}.store.json")).unwrap()).unwrap();
    for (i, name) in names.iter().enumerate() {
        assert!(!name.contains('/'));
        let payload = fs::read(Path::new(&base).parent().unwrap().join(name)).unwrap();
        assert!(payload.len() <= 64 << 20);
        let case: Case = serde_json::from_slice(&payload).unwrap();
        let root = PathBuf::from(format!(
            "/tmp/subyard-owner-orchestration-{}-{i}",
            std::process::id()
        ));
        assert!(!root.exists());
        struct Cleanup(PathBuf);
        impl Drop for Cleanup {
            fn drop(&mut self) {
                let _ = fs::remove_dir_all(&self.0);
            }
        }
        let _cleanup = Cleanup(root.clone());
        dirs(&root);
        for name in &case.before_dirs {
            dirs(&root.join(name))
        }
        for (name, payload) in &case.before {
            assert!(!name.starts_with('/') && !name.split('/').any(|p| p == ".."));
            let p = root.join(name);
            dirs(p.parent().unwrap());
            fs::write(&p, payload).unwrap();
            fs::set_permissions(p, fs::Permissions::from_mode(0o600)).unwrap()
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
        let result = run(&store, &case);
        assert_eq!(result.is_ok(), case.accepted, "{} {:?}", case.name, result);
        if let Ok(value) = result {
            assert_eq!(value, case.result, "{} result", case.name)
        }
        let (mut actual, mut actual_dirs) = (BTreeMap::new(), Vec::new());
        snapshot(&root, &root, &mut actual, &mut actual_dirs);
        let mut expected = case.after.clone();
        let mut actual_groups = 0;
        let mut expected_groups = 0;
        for group in &case.equivalent_groups {
            assert!(group.iter().all(|p| case.before.contains_key(p)));
            let kept = group.iter().filter(|p| actual.contains_key(*p)).count();
            assert!(
                kept == 0 || kept == group.len(),
                "{} partial group deletion",
                case.name
            );
            if kept > 0 {
                actual_groups += 1
            }
            if group.iter().all(|p| expected.contains_key(p)) {
                expected_groups += 1
            }
            for name in group {
                if let Some(value) = actual.remove(name) {
                    assert_eq!(value, case.before[name], "{} retained bytes", case.name)
                }
                expected.remove(name);
            }
        }
        assert_eq!(
            actual_groups, expected_groups,
            "{} Go-derived eligible group retention count",
            case.name
        );
        assert_eq!(
            actual.keys().collect::<Vec<_>>(),
            expected.keys().collect::<Vec<_>>(),
            "{} retained/deleted addresses",
            case.name
        );
        for (name, value) in &actual {
            assert!(
                Some(value) == expected.get(name),
                "{} exact bytes at {}",
                case.name,
                name
            )
        }
        assert_eq!(actual_dirs, case.after_dirs, "{} directories", case.name);
        drop(store);
        fs::remove_dir_all(root).unwrap()
    }
    println!("{} actual Go mutation/refusal cases", names.len());
}
