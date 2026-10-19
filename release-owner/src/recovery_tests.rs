//! Real-process publication cuts; test-only driver, no runtime control protocol.
use crate::{
    codec, graph,
    inventory::Namespace,
    record, recovery,
    store::{Address, Store},
};
use std::{
    collections::BTreeMap,
    fs,
    io::{BufRead, BufReader},
    os::unix::fs::PermissionsExt,
    path::{Path, PathBuf},
    process::{Command, Stdio},
    sync::mpsc,
    time::{Duration, SystemTime, UNIX_EPOCH},
};
struct Fixture(PathBuf);
impl Fixture {
    fn new() -> Self {
        let p = PathBuf::from(format!(
            "/tmp/subyard-owner-recovery-{}-{}",
            std::process::id(),
            SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        fs::create_dir(&p).unwrap();
        fs::set_permissions(&p, fs::Permissions::from_mode(0o700)).unwrap();
        Self(p)
    }
    fn write(&self, name: &str, payload: &[u8]) {
        assert!(!name.starts_with('/') && !name.split('/').any(|p| p == ".."));
        let path = self.0.join(name);
        let mut missing = Vec::new();
        let mut parent = path.parent().unwrap();
        while !parent.exists() {
            missing.push(parent.to_owned());
            parent = parent.parent().unwrap()
        }
        for p in missing.iter().rev() {
            fs::create_dir(p).unwrap();
            fs::set_permissions(p, fs::Permissions::from_mode(0o700)).unwrap()
        }
        fs::write(&path, payload).unwrap();
        fs::set_permissions(path, fs::Permissions::from_mode(0o600)).unwrap()
    }
}
impl Drop for Fixture {
    fn drop(&mut self) {
        fs::remove_dir_all(&self.0).unwrap()
    }
}
fn corpus() -> Option<serde_json::Value> {
    let path = std::env::var("SUBYARD_RUST_OWNER_CORPUS").ok()?;
    Some(serde_json::from_slice(&fs::read(format!("{path}.recovery.json")).unwrap()).unwrap())
}
fn receipt(c: &serde_json::Value) -> recovery::Receipt {
    let raw = c["vectors"]
        .as_array()
        .unwrap()
        .iter()
        .find(|v| v["name"] == recovery::ACTIVATION_V2)
        .unwrap()["canonical"]
        .as_str()
        .unwrap();
    recovery::Receipt::new(serde_json::from_str(raw).unwrap()).unwrap()
}
fn child_command(path: &Path, operation: &str) -> Command {
    let mut c = Command::new(std::env::current_exe().unwrap());
    c.args(["--exact", "recovery_tests::child", "--nocapture"])
        .env("SUBYARD_RECOVERY_ROOT", path)
        .env("SUBYARD_RECOVERY_OPERATION", operation)
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    c
}
#[test]
fn child() {
    let Ok(root) = std::env::var("SUBYARD_RECOVERY_ROOT") else {
        return;
    };
    assert!(root.starts_with("/tmp/subyard-owner-recovery-"));
    let c = corpus().expect("owning corpus required by child");
    let mut store = Store::open_canonical_fixture(Path::new(&root)).unwrap();
    store.test_cut(match std::env::var("SUBYARD_RECOVERY_CUT").as_deref() {
        Ok("after-pending-fsync") => Some("after-pending-fsync"),
        Ok("before-publish") => Some("before-publish"),
        Ok("after-publish-before-dir-fsync") => Some("after-publish-before-dir-fsync"),
        Ok("after-dir-fsync") => Some("after-dir-fsync"),
        Ok("after-private-create") => Some("after-private-create"),
        Ok("before-private-create") => Some("before-private-create"),
        Ok("after-recovery-completion") => Some("after-recovery-completion"),
        Ok("after-ordinary-unlink") => Some("after-ordinary-unlink"),
        Ok("after-recovery-cancellation") => Some("after-recovery-cancellation"),
        Ok("after-recovery-lifecycle-unlink") => Some("after-recovery-lifecycle-unlink"),
        _ => None,
    });
    let lease = store.lock().unwrap();
    let receipt = receipt(&c);
    match std::env::var("SUBYARD_RECOVERY_OPERATION")
        .unwrap()
        .as_str()
    {
        "receipt" => lease.publish_recovery_receipt(&receipt).unwrap(),
        "ordinary" => lease.cleanup_transactions("tx-current").unwrap(),
        "frontier" => lease
            .publish_frontier(
                &recovery::Frontier::new(crate::wire::lifecycle_v1::RecoveryFrontier {
                    schema_version: 1,
                    replacement: receipt.wire().replacement.clone(),
                    generation: 1,
                    cancellation: None,
                })
                .unwrap(),
            )
            .unwrap(),
        "cancel" => {
            let base = format!("plan-v1-{}", "b".repeat(64));
            let plan = recovery::bind_lifecycle_plan(
                &base,
                &crate::digest(&codec::encode(receipt.wire(), true).unwrap()),
            )
            .unwrap();
            lease
                .cancel_recovery(&receipt, &base, &plan, &"b".repeat(64))
                .unwrap();
        }
        "completion" => {
            let current = record::Journal::new(
                codec::decode_canonical(
                    store.read(&Address::journal()).unwrap().bytes().unwrap(),
                    true,
                )
                .unwrap(),
            )
            .unwrap();
            lease.capture_completion(&current).unwrap()
        }
        "private" => {
            let raw = c["vectors"]
                .as_array()
                .unwrap()
                .iter()
                .find(|v| v["name"] == "migration-owner")
                .unwrap()["canonical"]
                .as_str()
                .unwrap();
            let r = crate::migration_owner::Receipt::from_canonical_fixture(
                serde_json::from_str(raw).unwrap(),
            )
            .unwrap();
            lease
                .create_owner_immutable(
                    &Namespace::MigrationOwner
                        .address(&r.wire().successor.transaction)
                        .unwrap(),
                    raw.as_bytes(),
                )
                .unwrap()
        }
        "retire" => {
            let state = graph::State::read(&store).unwrap();
            lease.retire(state.retirement().unwrap()).unwrap()
        }
        _ => panic!("closed fixture operation"),
    }
}
#[test]
fn native_crash_resume() {
    let Some(c) = corpus() else { return };
    let receipt = receipt(&c);
    let rw = receipt.wire();
    let frontier = crate::wire::lifecycle_v1::RecoveryFrontier {
        schema_version: 1,
        replacement: rw.replacement.clone(),
        generation: 1,
        cancellation: None,
    };
    let mut count = 0;
    for (operation, cuts) in [
        (
            "receipt",
            vec![
                "after-pending-fsync",
                "before-publish",
                "after-publish-before-dir-fsync",
            ],
        ),
        (
            "frontier",
            vec!["after-pending-fsync", "after-publish-before-dir-fsync"],
        ),
        (
            "completion",
            vec!["after-pending-fsync", "after-recovery-completion"],
        ),
        (
            "cancel",
            vec![
                "after-pending-fsync",
                "after-publish-before-dir-fsync",
                "after-recovery-lifecycle-unlink",
            ],
        ),
        (
            "private",
            vec![
                "before-private-create",
                "after-private-create",
                "after-pending-fsync",
                "before-publish",
                "after-publish-before-dir-fsync",
            ],
        ),
        ("retire", vec!["after-recovery-lifecycle-unlink"]),
    ] {
        for cut in cuts {
            let f = Fixture::new();
            match operation {
                "receipt" | "frontier" => {
                    f.write(
                        "release-transition/v2/journal.json",
                        &codec::encode(&rw.predecessor, true).unwrap(),
                    );
                    if operation == "receipt" {
                        f.write(
                            &format!(
                                "release-transition/recovery/v2/frontiers/{}.json",
                                rw.replacement.transaction
                            ),
                            &codec::encode(&frontier, true).unwrap(),
                        );
                    }
                }
                "cancel" => {
                    f.write(
                        "release-transition/v2/journal.json",
                        &codec::encode(&rw.predecessor, true).unwrap(),
                    );
                    f.write(
                        &format!(
                            "release-transition/recovery/v2/transactions/{}.json",
                            rw.successor.transaction
                        ),
                        &codec::encode(rw, true).unwrap(),
                    );
                    f.write(
                        &format!(
                            "release-transition/recovery/v2/frontiers/{}.json",
                            rw.replacement.transaction
                        ),
                        &codec::encode(&frontier, true).unwrap(),
                    );
                }
                "completion" => {
                    let mut completed = rw.successor.clone();
                    completed.checkpoint = "complete".into();
                    f.write(
                        "release-transition/v2/journal.json",
                        &codec::encode(&completed, true).unwrap(),
                    );
                    f.write(
                        &format!(
                            "release-transition/recovery/v2/transactions/{}.json",
                            rw.successor.transaction
                        ),
                        &codec::encode(rw, true).unwrap(),
                    );
                    f.write(
                        &format!(
                            "release-transition/recovery/v2/frontiers/{}.json",
                            rw.replacement.transaction
                        ),
                        &codec::encode(&frontier, true).unwrap(),
                    )
                }
                "retire" => {
                    let policy = c["policies"]
                        .as_array()
                        .unwrap()
                        .iter()
                        .find(|p| p["name"] == "pending-terminal")
                        .unwrap();
                    let files: BTreeMap<String, String> =
                        serde_json::from_value(policy["before"].clone()).unwrap();
                    for (name, value) in files {
                        f.write(&name, value.as_bytes())
                    }
                }
                _ => {}
            }
            let mut child = child_command(&f.0, operation)
                .env("SUBYARD_RECOVERY_CUT", cut)
                .spawn()
                .unwrap();
            let stdout = child.stdout.take().unwrap();
            let (tx, rx) = mpsc::channel();
            let reader = std::thread::spawn(move || {
                for line in BufReader::new(stdout).lines() {
                    let line = line.unwrap();
                    if line.starts_with("publication-cut:") {
                        tx.send(line).unwrap();
                        break;
                    }
                }
            });
            let reached = rx.recv_timeout(Duration::from_secs(5));
            if reached.is_err() {
                let _ = child.kill();
                let output = child.wait_with_output().unwrap();
                panic!(
                    "{operation}/{cut} did not reach cut: {}",
                    String::from_utf8_lossy(&output.stderr)
                )
            };
            assert_eq!(reached.unwrap(), format!("publication-cut:{cut}"));
            child.kill().unwrap();
            assert!(!child.wait().unwrap().success());
            reader.join().unwrap();
            if operation == "private" {
                let store = Store::open_canonical_fixture(&f.0).unwrap();
                let inventory = store.inventory(Namespace::MigrationOwner).unwrap();
                for pair in inventory.records().values() {
                    assert!(pair.pending().bytes().is_none());
                    assert_eq!(
                        pair.published().bytes().unwrap(),
                        c["vectors"]
                            .as_array()
                            .unwrap()
                            .iter()
                            .find(|v| v["name"] == "migration-owner")
                            .unwrap()["canonical"]
                            .as_str()
                            .unwrap()
                            .as_bytes()
                    );
                }
            }
            let mut resumed = child_command(&f.0, operation).spawn().unwrap();
            let until = std::time::Instant::now() + Duration::from_secs(5);
            loop {
                if let Some(status) = resumed.try_wait().unwrap() {
                    if !status.success() {
                        let output = resumed.wait_with_output().unwrap();
                        panic!(
                            "resume {operation}/{cut}: {}",
                            String::from_utf8_lossy(&output.stderr)
                        )
                    }
                    break;
                }
                if std::time::Instant::now() > until {
                    resumed.kill().unwrap();
                    resumed.wait().unwrap();
                    panic!("resume exceeded fixture deadline")
                };
                std::thread::sleep(Duration::from_millis(5))
            }
            count += 1;
        }
    }
    println!("{count} real-process receipt/frontier/completion/private-publication/partial-cleanup cuts resumed");
}

#[test]
fn native_ordinary_partial_cleanup() {
    let Ok(base) = std::env::var("SUBYARD_RUST_OWNER_CORPUS") else {
        return;
    };
    let base = format!("{base}.recovery.json");
    let manifest = fs::read(format!("{base}.store.json")).unwrap();
    let names: Vec<String> = serde_json::from_slice(&manifest).unwrap();
    let mut value = None;
    for name in names {
        let bytes = fs::read(Path::new(&base).parent().unwrap().join(name)).unwrap();
        let case: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
        if case["name"] == "ordinary-chain" {
            value = Some(case);
            break;
        }
    }
    let case = value.expect("actual Go ordinary fixture");
    let f = Fixture::new();
    for name in case["beforeDirs"].as_array().unwrap() {
        let path = f.0.join(name.as_str().unwrap());
        fs::create_dir_all(&path).unwrap();
        fs::set_permissions(path, fs::Permissions::from_mode(0o700)).unwrap();
    }
    for (name, value) in case["before"].as_object().unwrap() {
        f.write(name, value.as_str().unwrap().as_bytes());
    }
    let mut child = child_command(&f.0, "ordinary")
        .env("SUBYARD_RECOVERY_CUT", "after-ordinary-unlink")
        .spawn()
        .unwrap();
    let stdout = child.stdout.take().unwrap();
    let (tx, rx) = mpsc::channel();
    let reader = std::thread::spawn(move || {
        for line in BufReader::new(stdout).lines() {
            let line = line.unwrap();
            if line.starts_with("publication-cut:") {
                tx.send(line).unwrap();
                break;
            }
        }
    });
    if rx.recv_timeout(Duration::from_secs(5)).is_err() {
        let _ = child.kill();
        let output = child.wait_with_output().unwrap();
        panic!(
            "ordinary cleanup did not reach cut: {}",
            String::from_utf8_lossy(&output.stderr)
        )
    }
    child.kill().unwrap();
    child.wait().unwrap();
    reader.join().unwrap();
    let mut resumed = child_command(&f.0, "ordinary").spawn().unwrap();
    let deadline = std::time::Instant::now() + Duration::from_secs(5);
    loop {
        if resumed.try_wait().unwrap().is_some() {
            break;
        }
        if std::time::Instant::now() > deadline {
            resumed.kill().unwrap();
            resumed.wait().unwrap();
            panic!("ordinary continuation exceeded fixture deadline")
        };
        std::thread::sleep(Duration::from_millis(5));
    }
    let output = resumed.wait_with_output().unwrap();
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    let parent = f.0.join("release-transition/v2/transactions");
    assert!(fs::read_dir(parent).unwrap().next().is_none());
    println!("ordinary successor-first partial deletion resumed in a fresh process");
}
