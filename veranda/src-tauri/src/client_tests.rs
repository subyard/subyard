//! Manager regressions use the same synthetic subprocess owner as transport tests.
use super::*;
use crate::transport::transport_tests::OwnerFixture;

#[cfg(target_os = "linux")]
#[derive(Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct StoreActorRequest {
    version: u32,
    action: String,
    store_root: PathBuf,
    record_id: Option<String>,
    destination: Option<String>,
}

#[cfg(target_os = "linux")]
fn actor_private_directory(path: &std::path::Path) -> Result<(), &'static str> {
    use std::os::unix::fs::{MetadataExt, PermissionsExt};
    let metadata = std::fs::symlink_metadata(path).map_err(|_| "invalid_actor_request")?;
    if !path.is_absolute()
        || std::fs::canonicalize(path).map_err(|_| "invalid_actor_request")? != path
        || !metadata.is_dir()
        || metadata.uid() != unsafe { libc::geteuid() }
        || metadata.permissions().mode() & 0o7777 != 0o700
    {
        return Err("invalid_actor_request");
    }
    Ok(())
}

#[cfg(target_os = "linux")]
fn actor_read(path: &std::path::Path) -> Result<Vec<u8>, &'static str> {
    use std::io::Read;
    use std::os::unix::fs::{MetadataExt, OpenOptionsExt, PermissionsExt};
    let file = std::fs::OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NOFOLLOW)
        .open(path)
        .map_err(|_| "invalid_actor_request")?;
    let metadata = file.metadata().map_err(|_| "invalid_actor_request")?;
    if !metadata.is_file()
        || metadata.uid() != unsafe { libc::geteuid() }
        || metadata.permissions().mode() & 0o7777 != 0o600
        || metadata.nlink() != 1
        || metadata.len() > 4096
    {
        return Err("invalid_actor_request");
    }
    let mut bytes = Vec::new();
    file.take(4097)
        .read_to_end(&mut bytes)
        .map_err(|_| "invalid_actor_request")?;
    if bytes.len() > 4096 {
        return Err("invalid_actor_request");
    }
    Ok(bytes)
}

#[cfg(target_os = "linux")]
fn actor_write(path: &std::path::Path, bytes: &[u8]) -> Result<(), &'static str> {
    use std::io::Write;
    use std::os::unix::fs::{OpenOptionsExt, PermissionsExt};
    let mut file = std::fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .custom_flags(libc::O_NOFOLLOW)
        .open(path)
        .map_err(|_| "actor_failed")?;
    file.set_permissions(std::fs::Permissions::from_mode(0o600))
        .map_err(|_| "actor_failed")?;
    file.write_all(bytes).map_err(|_| "actor_failed")?;
    file.sync_all().map_err(|_| "actor_failed")
}

#[cfg(target_os = "linux")]
fn actor_request(path: &std::path::Path) -> Result<StoreActorRequest, &'static str> {
    let root = path.parent().ok_or("invalid_actor_request")?;
    actor_private_directory(root)?;
    let owner = root.parent().ok_or("invalid_actor_request")?;
    actor_private_directory(owner)?;
    let name = owner
        .file_name()
        .and_then(|name| name.to_str())
        .unwrap_or("");
    let suffix = name.strip_prefix("veranda-transport-test-").unwrap_or("");
    if suffix.len() != 32
        || !suffix
            .bytes()
            .all(|byte| byte.is_ascii_hexdigit() && !byte.is_ascii_uppercase())
        || path.file_name().and_then(|name| name.to_str()) != Some("request.json")
        || root
            .file_name()
            .and_then(|name| name.to_str())
            .is_none_or(|name| !name.starts_with("store-actor-") || !transport::identity(name))
        || actor_read(&root.join(".marker"))? != b"veranda-store-actor-v1\n"
    {
        return Err("invalid_actor_request");
    }
    let request: StoreActorRequest =
        serde_json::from_slice(&actor_read(path)?).map_err(|_| "invalid_actor_request")?;
    if request.version != 1 || request.store_root != owner.join("connections") {
        return Err("invalid_actor_request");
    }
    actor_private_directory(&request.store_root)?;
    match request.action.as_str() {
        "lock-probe" if request.record_id.is_none() && request.destination.is_none() => {}
        "remove"
            if request.destination.is_none()
                && request
                    .record_id
                    .as_deref()
                    .is_some_and(transport::identity) => {}
        "repair"
            if request
                .record_id
                .as_deref()
                .is_some_and(transport::identity) =>
        {
            let endpoint = crate::ssh::Endpoint::parse(
                request
                    .destination
                    .as_deref()
                    .ok_or("invalid_actor_request")?,
            )
            .map_err(|_| "invalid_actor_request")?;
            if endpoint.host != "127.0.0.1" || endpoint.user.is_none() || endpoint.port < 1024 {
                return Err("invalid_actor_request");
            }
        }
        _ => return Err("invalid_actor_request"),
    }
    Ok(request)
}

#[cfg(target_os = "linux")]
fn actor_apply(request: StoreActorRequest) -> Result<(), &'static str> {
    if request.action == "lock-probe" {
        return crate::connections::try_store_lock_for_test(&request.store_root)
            .map(|_| ())
            .map_err(|error| {
                if error.code == "connection_store_busy" {
                    "connection_store_busy"
                } else {
                    "actor_failed"
                }
            });
    }
    let client = Client::new(request.store_root, Arc::new(|_| {}));
    let _shutdown = SmokeShutdown(client.clone());
    let id = request.record_id.ok_or("invalid_actor_request")?;
    if request.action == "remove" {
        let review = client.assess_removal(&id).map_err(|_| "actor_failed")?;
        return client
            .remove(&review.assessment_id, true)
            .map_err(|_| "actor_failed");
    }
    let previous = client
        .connections()
        .map_err(|_| "actor_failed")?
        .into_iter()
        .find(|record| record.id == id)
        .ok_or("actor_failed")?;
    if Some(&previous.destination) != request.destination.as_ref() {
        return Err("invalid_actor_request");
    }
    let review = client.repair(&id).map_err(|_| "actor_failed")?;
    if review.previous_fingerprint.as_deref() == Some(&review.fingerprint) {
        return Err("actor_failed");
    }
    let repaired = client
        .connect(&review.assessment_id, true, &review.fingerprint)
        .map_err(|_| "actor_failed")?;
    if repaired != previous {
        return Err("actor_failed");
    }
    Ok(())
}

#[cfg(target_os = "linux")]
#[test]
#[ignore = "private request supplied only by the subprocess fixture"]
fn external_store_actor() {
    let path = PathBuf::from(
        std::env::var_os("VERANDA_TEST_STORE_ACTOR_REQUEST")
            .expect("marked store actor request required"),
    );
    let request = actor_request(&path).expect("invalid marked store actor request");
    let code = actor_apply(request).err().unwrap_or("ok");
    actor_write(&path.with_file_name("result"), code.as_bytes()).expect("private actor result");
}

#[cfg(target_os = "linux")]
fn run_store_actor(
    root: &std::path::Path,
    action: &str,
    record: Option<&ConnectionSummary>,
) -> String {
    use std::os::unix::fs::{DirBuilderExt, PermissionsExt};
    let directory = root
        .parent()
        .unwrap()
        .join(format!("store-actor-{}", random_id().unwrap()));
    std::fs::DirBuilder::new()
        .mode(0o700)
        .create(&directory)
        .unwrap();
    std::fs::set_permissions(&directory, std::fs::Permissions::from_mode(0o700)).unwrap();
    actor_write(&directory.join(".marker"), b"veranda-store-actor-v1\n").unwrap();
    let request = StoreActorRequest {
        version: 1,
        action: action.into(),
        store_root: root.to_path_buf(),
        record_id: record.map(|record| record.id.clone()),
        destination: (action == "repair").then(|| record.unwrap().destination.clone()),
    };
    let path = directory.join("request.json");
    actor_write(&path, &serde_json::to_vec(&request).unwrap()).unwrap();
    let mut child = Command::new(std::env::current_exe().unwrap())
        .args([
            "--exact",
            "client::client_tests::external_store_actor",
            "--ignored",
            "--nocapture",
            "--test-threads=1",
        ])
        .env("VERANDA_TEST_STORE_ACTOR_REQUEST", path)
        .stdin(std::process::Stdio::null())
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null())
        .spawn()
        .unwrap();
    let deadline = Instant::now() + Duration::from_secs(45);
    loop {
        if let Some(status) = child.try_wait().unwrap() {
            assert!(status.success(), "store actor failed");
            break;
        }
        if Instant::now() >= deadline {
            let _ = child.kill();
            let _ = child.wait();
            panic!("store actor timed out");
        }
        thread::sleep(Duration::from_millis(10));
    }
    let result = actor_read(&directory.join("result")).unwrap();
    assert!(
        matches!(
            result.as_slice(),
            b"ok" | b"connection_store_busy" | b"actor_failed" | b"invalid_actor_request"
        ),
        "invalid actor status"
    );
    std::fs::remove_dir_all(directory).unwrap();
    String::from_utf8(result).unwrap()
}

#[cfg(target_os = "linux")]
#[test]
fn separate_process_store_lock_and_removal_reject_stale_cached_trust() {
    let fixture = ManagerFixture::with_registration("manager", true);
    let record = fixture.client.connections().unwrap().remove(0);
    let root = fixture.owner.store_root();
    let held = crate::connections::try_store_lock_for_test(&root).unwrap();
    assert_eq!(
        run_store_actor(&root, "lock-probe", None),
        "connection_store_busy"
    );
    drop(held);
    assert_eq!(run_store_actor(&root, "lock-probe", None), "ok");
    let plan = fixture
        .client
        .plan(
            Some(record.id.clone()),
            None,
            "config".into(),
            vec!["set".into(), "SSH_PORT".into(), "2223".into()],
        )
        .unwrap();
    assert_eq!(run_store_actor(&root, "remove", Some(&record)), "ok");
    assert!(!fixture.session.rpc.is_closed());
    assert_eq!(fixture.stats()["executes"], 0);
    assert!(fixture
        .client
        .execute(&plan.plan_id, &plan.digest, true)
        .is_err());
    assert!(fixture.session.rpc.is_closed());
    assert!(fixture.client.fleet(Some(record.id)).is_err());
}

fn fixture(name: &str) -> Value {
    let path = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("../../api/yard-rpc/v1/fixtures")
        .join(format!("{name}.json"));
    serde_json::from_slice::<Value>(&std::fs::read(path).unwrap()).unwrap()["result"].clone()
}
fn owner_noop_plan() -> Value {
    // Go's remote-operation no-op contract still requires the owner live recheck.
    let mut exact = fixture("operation-exact");
    exact["plan"]["confirmation"] = json!("never");
    exact["plan"]["confirmed"] = json!(true);
    exact["plan"]["steps"][0]["desired"] = exact["plan"]["steps"][0]["observed"].clone();
    exact["plan"]["steps"][0]["decision"] = json!("skip");
    exact
}

#[test]
fn owner_noop_exact_plan_allows_confirmed_never_but_rejects_confirmed_prompts() {
    let mut exact = owner_noop_plan();
    decode::<ExactPlan>(exact.clone())
        .unwrap()
        .validate("selection-1", "config")
        .unwrap();
    for policy in ["prompt-default-yes", "prompt-default-no"] {
        exact["plan"]["confirmation"] = json!(policy);
        assert_eq!(
            decode::<ExactPlan>(exact.clone())
                .unwrap()
                .validate("selection-1", "config")
                .unwrap_err()
                .code,
            "invalid_response"
        );
    }
}

#[cfg(target_os = "linux")]
#[test]
fn production_monitor_resyncs_a_gap_and_resubscribes_after_transport_loss() {
    use std::os::unix::fs::{DirBuilderExt, MetadataExt, PermissionsExt};
    const CHILD_ROOT: &str = "VERANDA_TEST_MONITOR_ROOT";
    if let Some(root) = std::env::var_os(CHILD_ROOT) {
        let root = PathBuf::from(root);
        actor_private_directory(&root).unwrap();
        actor_private_directory(root.parent().unwrap()).unwrap();
        assert_eq!(root.file_name().unwrap(), "monitor");
        assert_eq!(
            actor_read(&root.join(".marker")).unwrap(),
            b"veranda-monitor-v1\n"
        );
        let (sender, events) = mpsc::channel();
        let client = Client::new(
            root.join("connections"),
            Arc::new(move |event| {
                let _ = sender.send(event);
            }),
        );
        let _shutdown = SmokeShutdown(client.clone());
        assert_eq!(client.fleet(None).unwrap().observed_at, "initial-1");
        let key = SessionKey {
            connection: None,
            yard: None,
        };
        let initial = client.sessions.lock().unwrap()[&key].clone();
        let stats = || {
            initial
                .rpc
                .call(
                    "synthetic.monitor.stats",
                    "",
                    json!({}),
                    Duration::from_secs(2),
                )
                .unwrap()
        };
        assert_eq!(stats()["subscriptions"], 1);
        while events.try_recv().is_ok() {}
        initial
            .rpc
            .call(
                "synthetic.monitor.gap",
                "",
                json!({}),
                Duration::from_secs(2),
            )
            .unwrap();
        let deadline = Instant::now() + Duration::from_secs(2);
        loop {
            let event = events
                .recv_timeout(deadline.saturating_duration_since(Instant::now()))
                .expect("production monitor did not autonomously resync the gap");
            if let Some(snapshot) = event.snapshot {
                assert_eq!(event.state, "connected");
                assert_eq!(snapshot.observed_at, "gap-resynced");
                break;
            }
        }
        assert!(Arc::ptr_eq(
            &initial,
            &client.sessions.lock().unwrap()[&key]
        ));
        assert_eq!(stats()["subscriptions"], 1);
        initial.rpc.kill_owned_child_for_test().unwrap();
        let deadline = Instant::now() + Duration::from_secs(3);
        let mut disconnected = false;
        loop {
            let event = events
                .recv_timeout(deadline.saturating_duration_since(Instant::now()))
                .expect("production monitor did not autonomously replace the lost transport");
            disconnected |= event.state == "disconnected";
            if let Some(snapshot) = event.snapshot {
                assert!(disconnected);
                assert_eq!(event.state, "connected");
                assert_eq!(snapshot.observed_at, "initial-2");
                break;
            }
        }
        let replacement = client.sessions.lock().unwrap()[&key].clone();
        assert!(!Arc::ptr_eq(&initial, &replacement));
        assert_eq!(
            replacement
                .rpc
                .call(
                    "synthetic.monitor.stats",
                    "",
                    json!({}),
                    Duration::from_secs(2)
                )
                .unwrap()["subscriptions"],
            1
        );
        // Exercise production callbacks during a real cache eviction. Keep all
        // other sessions in use so the named target is the sole eviction choice.
        let retired_key = SessionKey {
            connection: None,
            yard: Some("retired".into()),
        };
        let retired = client.session(retired_key.clone()).unwrap();
        let retired_weak = Arc::downgrade(&retired);
        drop(retired);
        let mut retained = Vec::new();
        for index in 0..MAX_SESSIONS - 2 {
            retained.push(
                client
                    .session(SessionKey {
                        connection: None,
                        yard: Some(format!("cached-{index}")),
                    })
                    .unwrap(),
            );
        }
        assert_eq!(client.sessions.lock().unwrap().len(), MAX_SESSIONS);
        while events.try_recv().is_ok() {}
        let acquire = |key: SessionKey| {
            let deadline = Instant::now() + Duration::from_secs(2);
            loop {
                match client.session(key.clone()) {
                    Ok(session) => break session,
                    Err(error) if error.code == "request_capacity" && Instant::now() < deadline => {
                        thread::sleep(Duration::from_millis(10));
                    }
                    Err(error) => panic!("cache eviction failed: {}", error.code),
                }
            }
        };
        let next = acquire(SessionKey {
            connection: None,
            yard: Some("next".into()),
        });
        assert!(!client.sessions.lock().unwrap().contains_key(&retired_key));
        assert_eq!(client.sessions.lock().unwrap().len(), MAX_SESSIONS);
        assert!(retired_weak.upgrade().is_none());
        assert!(
            events
                .try_iter()
                .all(|event| event.yard.as_deref() != Some("retired")
                    || event.state != "disconnected")
        );
        let identity = actor_read(&root.join("owner-3.identity")).unwrap();
        let pid: i32 = std::str::from_utf8(&identity)
            .unwrap()
            .split_whitespace()
            .next()
            .unwrap()
            .parse()
            .unwrap();
        assert_eq!(
            unsafe { libc::kill(pid, 0) },
            -1,
            "evicted child was not reaped"
        );
        assert_eq!(
            std::io::Error::last_os_error().raw_os_error(),
            Some(libc::ESRCH)
        );
        retained.pop();
        let reopened = acquire(retired_key.clone());
        let ready = events
            .try_iter()
            .find(|event| event.yard.as_deref() == Some("retired") && event.state == "connected")
            .expect("reopened scope did not publish healthy inventory");
        let ready = ready
            .snapshot
            .expect("connected scope did not include its snapshot");
        assert_eq!(ready.observed_at, "initial-11");
        assert!(ready.owner.yards.iter().any(|yard| yard.name == "retired"));
        let reopened_stats = || {
            reopened
                .rpc
                .call(
                    "synthetic.monitor.stats",
                    "",
                    json!({}),
                    Duration::from_secs(2),
                )
                .unwrap()
        };
        assert_eq!(reopened_stats()["subscriptions"], 1);
        assert_eq!(reopened_stats()["inventoryQueries"], 1);
        assert_eq!(
            client.snapshot(&reopened).unwrap().observed_at,
            "initial-11"
        );
        assert_eq!(
            reopened_stats()["inventoryQueries"],
            1,
            "publication consumed the initial read"
        );
        assert!(Arc::ptr_eq(
            &client.session(retired_key).unwrap(),
            &reopened
        ));
        assert!(
            events
                .try_iter()
                .all(|event| event.yard.as_deref() != Some("retired")),
            "cached hits duplicated publication"
        );
        let context_client = Client::new(root.join("scope-connections"), Arc::new(|_| {}));
        let _context_shutdown = SmokeShutdown(context_client.clone());
        for yard in ["wrong-scope", "unsafe-scope"] {
            assert_eq!(
                context_client.yard(None, yard.into()).err().unwrap().code,
                "invalid_response"
            );
            assert!(context_client.sessions.lock().unwrap().is_empty());
        }
        actor_write(
            &root.join("invalid-default-context"),
            b"invalid-context-v1\n",
        )
        .unwrap();
        assert_eq!(
            context_client.fleet(None).unwrap_err().code,
            "invalid_response"
        );
        assert!(context_client.sessions.lock().unwrap().is_empty());
        assert!(
            !root.join("unexpected-context-query").exists(),
            "invalid context reached inventory, subscription or detail queries"
        );
        let started = Instant::now();
        client.shutdown();
        assert!(started.elapsed() < Duration::from_secs(2));
        assert!(initial.rpc.is_closed() && replacement.rpc.is_closed());
        assert!(next.rpc.is_closed() && retained.iter().all(|session| session.rpc.is_closed()));
        assert!(reopened.rpc.is_closed());
        assert!(client.sessions.lock().unwrap().is_empty());
        for generation in 1..=MAX_SESSIONS + 6 {
            let identity = actor_read(&root.join(format!("owner-{generation}.identity"))).unwrap();
            let identity = std::str::from_utf8(&identity).unwrap();
            let pid: i32 = identity.split_whitespace().next().unwrap().parse().unwrap();
            assert_eq!(unsafe { libc::kill(pid, 0) }, -1);
            assert_eq!(
                std::io::Error::last_os_error().raw_os_error(),
                Some(libc::ESRCH)
            );
        }
        return;
    }

    // Re-exec isolates PATH from parallel tests while reaching the actual open_session handler.
    let owner = OwnerFixture::new();
    let root = owner.store_root().parent().unwrap().join("monitor");
    std::fs::DirBuilder::new()
        .mode(0o700)
        .create(&root)
        .unwrap();
    std::fs::set_permissions(&root, std::fs::Permissions::from_mode(0o700)).unwrap();
    actor_write(&root.join(".marker"), b"veranda-monitor-v1\n").unwrap();
    let command = owner.command("manager-monitor");
    let arguments: Vec<_> = command
        .get_args()
        .map(|value| value.to_str().unwrap())
        .collect();
    let fixture_arguments = serde_json::to_string(&arguments[3..]).unwrap();
    let source = arguments[2].replace(
        "mode, pid_file, fixtures = sys.argv[1:]",
        &format!(
            "mode, pid_file, fixtures = json.loads({})",
            serde_json::to_string(&fixture_arguments).unwrap()
        ),
    );
    let setup = format!(
        r#"
monitor_root = {root}
os.chmod(pid_file, 0o600)
generation_file = os.path.join(monitor_root, 'generation')
generation = int(open(generation_file).read()) + 1 if os.path.exists(generation_file) else 1
assert 1 <= generation <= 14
with open(generation_file, 'w') as output: output.write(str(generation))
os.chmod(generation_file, 0o600)
identity = os.path.join(monitor_root, 'owner-%d.identity' % generation)
with open('/proc/self/stat') as source: start = source.read().rsplit(')', 1)[1].split()[19]
with open(identity, 'w') as output: output.write('%d %s\n' % (os.getpid(), start))
os.chmod(identity, 0o600)
monitor_observed = 'initial-%d' % generation
subscriptions = 0
inventory_queries = 0
selected_yard = sys.argv[sys.argv.index('-Y') + 1] if '-Y' in sys.argv else 'default'
invalid_context = selected_yard in ('wrong-scope', 'unsafe-scope') or os.path.exists(os.path.join(monitor_root, 'invalid-default-context'))
"#,
        root = serde_json::to_string(root.to_str().unwrap()).unwrap()
    );
    let source = source.replace(
        "negotiate = read()",
        &format!("{setup}\nnegotiate = read()"),
    );
    let behavior = r#"    if method == 'context.get':
        context = fixture('context')['result']
        context['yardName'] = ('default' if selected_yard == 'wrong-scope' else '../escape') if invalid_context else selected_yard
        reply(request, context)
        continue
    if invalid_context:
        path = os.path.join(monitor_root, 'unexpected-context-query')
        with open(path, 'w') as output: output.write('unexpected-query\n')
        os.chmod(path, 0o600)
    if method == 'incus.events':
        subscriptions += 1
        reply(request, {'subscribed':True})
        continue
    if method == 'synthetic.monitor.stats':
        reply(request, {'subscriptions':subscriptions, 'inventoryQueries':inventory_queries})
        continue
    if method == 'synthetic.monitor.gap':
        monitor_observed = 'gap-resynced'
        event(1)
        event(3)
        reply(request, {'gapSent':True})
        continue
    if method == 'owner.inventory':
        inventory_queries += 1
        inventory = fixture('owner-inventory')['result']
        inventory['yards'].append({'name':'retired', 'kind':'container', 'state':'NOT_CREATED', 'projects':[]})
        inventory['observedAt'] = monitor_observed
        reply(request, inventory)
        continue
"#;
    let source = source.replace(
        "    method = request['method']\n",
        &format!("    method = request['method']\n{behavior}"),
    );
    let script = root.join("owner.py");
    actor_write(&script, source.as_bytes()).unwrap();
    let quote = |value: &str| format!("'{}'", value.replace('\'', "'\\''"));
    let wrapper = format!(
        "#!/bin/sh\nexec {} -u {} \"$@\"\n",
        quote(command.get_program().to_str().unwrap()),
        quote(script.to_str().unwrap())
    );
    actor_write(&root.join("yard"), wrapper.as_bytes()).unwrap();
    std::fs::set_permissions(root.join("yard"), std::fs::Permissions::from_mode(0o700)).unwrap();
    let path = std::env::join_paths(
        std::iter::once(root.clone())
            .chain(std::env::split_paths(&std::env::var_os("PATH").unwrap())),
    )
    .unwrap();
    let mut child = Command::new(std::env::current_exe().unwrap())
        .args(["--exact", "client::client_tests::production_monitor_resyncs_a_gap_and_resubscribes_after_transport_loss", "--nocapture", "--test-threads=1"])
        .env(CHILD_ROOT, &root).env("PATH", path)
        .stdin(std::process::Stdio::null()).stdout(std::process::Stdio::null())
        .spawn().unwrap();
    let deadline = Instant::now() + Duration::from_secs(30);
    loop {
        if let Some(status) = child.try_wait().unwrap() {
            assert!(
                status.success(),
                "isolated production monitor regression failed"
            );
            break;
        }
        if Instant::now() >= deadline {
            // Revoke only recorded fixture children whose UID, parent, group and start still match.
            for generation in 1..=MAX_SESSIONS + 6 {
                if let Ok(identity) = actor_read(&root.join(format!("owner-{generation}.identity")))
                {
                    let identity = std::str::from_utf8(&identity).unwrap();
                    let fields: Vec<_> = identity.split_whitespace().collect();
                    let pid: i32 = fields[0].parse().unwrap();
                    let entry = PathBuf::from(format!("/proc/{pid}"));
                    if let (Ok(metadata), Ok(current)) = (
                        entry.metadata(),
                        std::fs::read_to_string(entry.join("stat")),
                    ) {
                        let current: Vec<_> = current
                            .rsplit(')')
                            .next()
                            .unwrap()
                            .split_whitespace()
                            .collect();
                        if metadata.uid() == unsafe { libc::geteuid() }
                            && current[1] == child.id().to_string()
                            && current[2] == pid.to_string()
                            && current[19] == fields[1]
                        {
                            unsafe {
                                libc::kill(-pid, libc::SIGKILL);
                            }
                        }
                    }
                }
            }
            // Give the test's shutdown guard time to reap the revoked RPC child.
            let cleanup_deadline = Instant::now() + Duration::from_secs(2);
            while child.try_wait().unwrap().is_none() && Instant::now() < cleanup_deadline {
                thread::sleep(Duration::from_millis(10));
            }
            let _ = child.kill();
            let _ = child.wait();
            panic!("isolated production monitor regression exceeded its deadline");
        }
        thread::sleep(Duration::from_millis(10));
    }
}

struct ManagerFixture {
    owner: OwnerFixture,
    client: Arc<Client>,
    session: Arc<Session>,
    events: mpsc::Receiver<VerandaEvent>,
}
impl ManagerFixture {
    fn new(mode: &str) -> Self {
        Self::with_registration(mode, false)
    }
    fn with_registration(mode: &str, registered: bool) -> Self {
        let owner = OwnerFixture::new();
        let (sender, events) = mpsc::channel();
        let client = Client::new(
            owner.store_root(),
            Arc::new(move |event| {
                let _ = sender.send(event);
            }),
        );
        let dirty = Arc::new(AtomicBool::new(false));
        let epoch = Arc::new(AtomicU64::new(0));
        let callback_dirty = dirty.clone();
        let callback_epoch = epoch.clone();
        let original = owner.command(mode);
        let command = if mode == "manager-noop" {
            // Reuse the real subprocess fixture with a private no-op golden.
            let directory = owner.store_root().with_file_name("noop-fixtures");
            std::fs::create_dir(&directory).unwrap();
            let public =
                PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../../api/yard-rpc/v1/fixtures");
            for name in ["negotiate", "operation-exact", "owner-inventory"] {
                std::fs::copy(
                    public.join(format!("{name}.frame")),
                    directory.join(format!("{name}.frame")),
                )
                .unwrap();
            }
            let body = serde_json::to_vec(&json!({"result": owner_noop_plan()})).unwrap();
            let mut frame = (body.len() as u32).to_be_bytes().to_vec();
            frame.extend(body);
            std::fs::write(directory.join("operation-exact.frame"), frame).unwrap();
            #[cfg(unix)]
            {
                use std::os::unix::fs::PermissionsExt;
                std::fs::set_permissions(&directory, std::fs::Permissions::from_mode(0o700))
                    .unwrap();
                for name in ["negotiate", "operation-exact", "owner-inventory"] {
                    std::fs::set_permissions(
                        directory.join(format!("{name}.frame")),
                        std::fs::Permissions::from_mode(0o600),
                    )
                    .unwrap();
                }
            }
            let arguments: Vec<_> = original.get_args().collect();
            let mut command = std::process::Command::new(original.get_program());
            command
                .args(&arguments[..arguments.len() - 1])
                .arg(directory);
            command
        } else {
            original
        };
        let rpc = RpcClient::open(
            command,
            product_version(),
            Arc::new(move |event| {
                callback_dirty.store(true, Ordering::Release);
                if event.stale || event.event == "transport.disconnected" {
                    callback_epoch.fetch_add(1, Ordering::AcqRel);
                }
            }),
        )
        .unwrap();
        let pin = registered.then(|| {
            let mut store = client.store.lock().unwrap();
            let consent = synthetic_trust(&mut store, None);
            let record = store
                .finalize_registration(consent, "example-owner")
                .unwrap();
            store.registered_ssh_command(&record.id).unwrap()
        });
        let key = SessionKey {
            connection: client
                .connections()
                .unwrap()
                .first()
                .map(|record| record.id.clone()),
            yard: None,
        };
        let session = Arc::new(Session {
            rpc,
            _pin: pin,
            key: key.clone(),
            host_id: "example-owner".into(),
            current_yard: "default".into(),
            initial_snapshot: Mutex::new(None),
            dirty,
            epoch,
            retired: Arc::new(AtomicBool::new(false)),
            last_used: Mutex::new(Instant::now()),
            refreshing: AtomicBool::new(false),
            retry: Mutex::new(None),
        });
        client.sessions.lock().unwrap().insert(key, session.clone());
        Self {
            owner,
            client,
            session,
            events,
        }
    }
    fn plan(&self) -> OperationPlan {
        self.client
            .plan(
                None,
                None,
                "config".into(),
                vec!["set".into(), "SSH_PORT".into(), "2223".into()],
            )
            .unwrap()
    }
    fn stats(&self) -> Value {
        self.session
            .rpc
            .call("synthetic.stats", "", json!({}), Duration::from_secs(2))
            .unwrap()
    }
}

fn external_trust_change(fixture: &ManagerFixture, change: &str) {
    let id = fixture.session.key.connection.as_deref().unwrap();
    external_store_change(fixture.owner.store_root(), id, change);
}
fn external_store_change(root: PathBuf, id: &str, change: &str) {
    let mut other = ConnectionStore::new(root);
    match change {
        "repair" => {
            let pin = synthetic_trust(&mut other, Some(id));
            other.finalize_registration(pin, "example-owner").unwrap();
        }
        "remove" => {
            let assessment = other.prepare_remove(id).unwrap();
            other.remove(&assessment.token, true).unwrap();
        }
        "unrelated" => {
            let assessment = other
                .assess_test_key(
                    "other@unrelated.test:2222",
                    crate::ssh::HostKey {
                        key: "ssh-ed25519 CCCC".into(),
                        fingerprint: format!("SHA256:{}", "C".repeat(43)),
                    },
                    None,
                )
                .unwrap();
            let pin = other
                .consented_ssh_command(&assessment.token, true, &assessment.fingerprint)
                .unwrap();
            let record = other.finalize_registration(pin, "unrelated-owner").unwrap();
            other.select(Some(&record.id)).unwrap();
        }
        _ => unreachable!(),
    }
}

#[test]
fn external_trust_change_rejects_cached_owner_and_current_yard() {
    for change in ["repair", "remove"] {
        for yard in [None, Some("default".to_string())] {
            let fixture = ManagerFixture::with_registration("manager", true);
            external_trust_change(&fixture, change);
            let result = fixture.client.session_with_open(
                SessionKey {
                    connection: fixture.session.key.connection.clone(),
                    yard,
                },
                |_| panic!("stale cached trust must be rejected before reopening"),
            );
            assert!(result.is_err(), "accepted external {change}");
            assert!(fixture.session.rpc.is_closed());
            assert!(fixture.client.sessions.lock().unwrap().is_empty());
        }
    }
}

#[test]
fn external_trust_change_rejects_held_execution() {
    for change in ["repair", "remove"] {
        let fixture = ManagerFixture::with_registration("manager", true);
        let plan = fixture
            .client
            .plan(
                fixture.session.key.connection.clone(),
                None,
                "config".into(),
                vec!["set".into(), "SSH_PORT".into(), "2223".into()],
            )
            .unwrap();
        external_trust_change(&fixture, change);
        assert!(
            fixture
                .client
                .execute(&plan.plan_id, &plan.digest, true)
                .is_err(),
            "accepted external {change}"
        );
        assert!(fixture.session.rpc.is_closed());
    }
}

#[test]
fn unrelated_external_registration_and_selection_preserve_cached_trust() {
    let fixture = ManagerFixture::with_registration("manager", true);
    external_trust_change(&fixture, "unrelated");
    let session = fixture
        .client
        .session_with_open(fixture.session.key.clone(), |_| {
            panic!("unrelated registration reopened session")
        })
        .unwrap();
    assert!(Arc::ptr_eq(&session, &fixture.session));
    assert!(!session.rpc.is_closed());
    let pin = fixture
        .client
        .store
        .lock()
        .unwrap()
        .registered_ssh_command_bound(session._pin.as_ref().unwrap())
        .unwrap();
    assert!(pin.public_pin().contains("AAAA"));
    assert!(fixture.client.snapshot(&session).is_ok());
}

#[test]
fn external_trust_change_rejects_snapshot_and_bound_launch_pin() {
    for change in ["repair", "remove"] {
        let fixture = ManagerFixture::with_registration("manager", true);
        // The bound pin belongs to a live, negotiated subprocess owner. Launch
        // must not recapture another registration after preparing its descriptor.
        let _: OwnerInventory = query_for_test(&fixture.session.rpc, "owner.inventory");
        external_trust_change(&fixture, change);
        assert_eq!(
            fixture
                .client
                .store
                .lock()
                .unwrap()
                .registered_ssh_command_bound(fixture.session._pin.as_ref().unwrap())
                .err()
                .unwrap()
                .code,
            "connection_changed"
        );
        assert!(fixture.client.snapshot(&fixture.session).is_err());
        assert!(fixture.session.rpc.is_closed());
        assert!(fixture.client.sessions.lock().unwrap().is_empty());
    }
}
impl Drop for ManagerFixture {
    fn drop(&mut self) {
        self.client.shutdown();
    }
}

fn synthetic_trust(store: &mut ConnectionStore, repair: Option<&str>) -> ConsentedConnection {
    let marker = if repair.is_some() { 'B' } else { 'A' };
    let assessment = store
        .assess_test_key(
            "user@synthetic.test:2222",
            crate::ssh::HostKey {
                key: format!("ssh-ed25519 {}", marker.to_string().repeat(4)),
                fingerprint: format!("SHA256:{}", marker.to_string().repeat(43)),
            },
            repair,
        )
        .unwrap();
    store
        .consented_ssh_command(&assessment.token, true, &assessment.fingerprint)
        .unwrap()
}

enum TrustChange {
    Repair,
    Remove,
    FailedReadback,
    ExternalRepair,
    ExternalRemove,
    ExternalUnrelated,
}

fn opening_across_trust_change_is_rejected(change: TrustChange) {
    let owner = OwnerFixture::new();
    let client = Client::new(owner.store_root(), Arc::new(|_| {}));
    let record = {
        let mut store = client.store.lock().unwrap();
        let pin = synthetic_trust(&mut store, None);
        store.finalize_registration(pin, "example-owner").unwrap()
    };
    let key = SessionKey {
        connection: Some(record.id.clone()),
        yard: None,
    };
    let (ready, opened) = mpsc::sync_channel(1);
    let (resume, paused) = mpsc::sync_channel(1);
    let opening_client = client.clone();
    let command = owner.command("manager");
    let opening = thread::spawn(move || {
        opening_client.session_with_open(key, |key| {
            // This is an actual negotiated subprocess session holding the old
            // registration's pin; only its cache publication is deliberately paused.
            let pin = opening_client
                .store
                .lock()
                .unwrap()
                .registered_ssh_command(key.connection.as_deref().unwrap())?;
            let rpc = RpcClient::open(command, product_version(), Arc::new(|_| {}))?;
            let inventory: OwnerInventory = query_for_test(&rpc, "owner.inventory");
            let snapshot = inventory.into_snapshot(product_version().into(), "default".into())?;
            let session = Arc::new(Session {
                rpc,
                _pin: Some(pin),
                key: key.clone(),
                host_id: snapshot.owner.id.clone(),
                current_yard: "default".into(),
                initial_snapshot: Mutex::new(Some((0, snapshot))),
                dirty: Arc::new(AtomicBool::new(false)),
                epoch: Arc::new(AtomicU64::new(0)),
                retired: Arc::new(AtomicBool::new(false)),
                last_used: Mutex::new(Instant::now()),
                refreshing: AtomicBool::new(false),
                retry: Mutex::new(None),
            });
            ready.send(session.clone()).unwrap();
            paused.recv_timeout(Duration::from_secs(5)).unwrap();
            Ok(session)
        })
    });
    let old = opened.recv_timeout(Duration::from_secs(5)).unwrap();
    let _: Value = query_for_test(&old.rpc, "synthetic.stats");
    assert_eq!(old.host_id, record.host_id);
    if matches!(
        change,
        TrustChange::ExternalRepair | TrustChange::ExternalRemove | TrustChange::ExternalUnrelated
    ) {
        external_store_change(
            owner.store_root(),
            &record.id,
            match change {
                TrustChange::ExternalRepair => "repair",
                TrustChange::ExternalRemove => "remove",
                _ => "unrelated",
            },
        );
        assert_eq!(client.trust_epoch.load(Ordering::Acquire), 0);
    } else if !matches!(change, TrustChange::Remove) {
        let pin = synthetic_trust(&mut client.store.lock().unwrap(), Some(&record.id));
        let repaired = client.commit_trust(|store| {
            let repaired = store.finalize_registration(pin, "example-owner")?;
            if matches!(change, TrustChange::FailedReadback) {
                // Model a durability/readback error after the atomic rename:
                // persisted new trust must not leave the old opening usable.
                return Err(NativeError::new(
                    "connection_store_io",
                    "Synthetic verification failed after commit.",
                ));
            }
            Ok((repaired.clone(), repaired.id))
        });
        // Repair preserves both identifiers. HostID-only validation would accept
        // this old session even though its public key has explicitly been revoked.
        if matches!(change, TrustChange::FailedReadback) {
            assert!(repaired.is_err());
            assert_eq!(client.connections().unwrap(), vec![record.clone()]);
        } else {
            assert_eq!(repaired.unwrap(), record);
        }
        assert!(client
            .store
            .lock()
            .unwrap()
            .registered_ssh_command(&record.id)
            .unwrap()
            .public_pin()
            .contains("BBBB"));
    } else {
        let removal = client.assess_removal(&record.id).unwrap();
        client.remove(&removal.assessment_id, true).unwrap();
        assert!(client.connections().unwrap().is_empty());
    }
    resume.send(()).unwrap();
    if matches!(change, TrustChange::ExternalUnrelated) {
        let published = opening.join().unwrap().unwrap();
        assert!(Arc::ptr_eq(&published, &old));
        assert!(!old.rpc.is_closed());
        client.shutdown();
        return;
    }
    assert!(opening.join().unwrap().is_err());
    assert!(old.rpc.is_closed());
    assert!(old
        .rpc
        .call("synthetic.stats", "", json!({}), Duration::from_secs(1))
        .is_err());
    assert!(client.sessions.lock().unwrap().is_empty());
    assert!(client.opening.lock().unwrap().is_empty());
    client.shutdown();
}

fn query_for_test<T: for<'de> Deserialize<'de>>(rpc: &RpcClient, method: &str) -> T {
    decode(
        rpc.call(method, "", json!({}), Duration::from_secs(2))
            .unwrap(),
    )
    .unwrap()
}

#[test]
fn opening_with_revoked_key_cannot_publish_after_same_host_id_repair() {
    opening_across_trust_change_is_rejected(TrustChange::Repair);
}

#[test]
fn opening_cannot_publish_after_connection_removal() {
    opening_across_trust_change_is_rejected(TrustChange::Remove);
}

#[test]
fn opening_cannot_publish_after_a_committed_repair_reports_readback_failure() {
    opening_across_trust_change_is_rejected(TrustChange::FailedReadback);
}

#[test]
fn external_trust_change_checks_opening_publication_without_global_revision_invalidation() {
    for change in [
        TrustChange::ExternalRepair,
        TrustChange::ExternalRemove,
        TrustChange::ExternalUnrelated,
    ] {
        opening_across_trust_change_is_rejected(change);
    }
}

#[test]
fn current_yard_details_share_the_fleet_owner_process() {
    let manager = ManagerFixture::new("manager");
    let session = manager
        .client
        .session(SessionKey {
            connection: None,
            yard: Some("default".into()),
        })
        .unwrap();
    assert!(Arc::ptr_eq(&session, &manager.session));
    manager.client.yard(None, "default".into()).unwrap();
    assert_eq!(manager.client.sessions.lock().unwrap().len(), 1);
}

#[test]
fn initial_inventory_is_used_once_and_invalidated_by_an_event_gap() {
    let manager = ManagerFixture::new("manager");
    let mut initial = decode::<OwnerInventory>(fixture("owner-inventory"))
        .unwrap()
        .into_snapshot(product_version().into(), "default".into())
        .unwrap();
    initial.observed_at = "initial-captured-snapshot".into();
    *manager.session.initial_snapshot.lock().unwrap() = Some((0, initial.clone()));
    assert_eq!(
        manager.client.fleet(None).unwrap().observed_at,
        initial.observed_at
    );
    assert_ne!(
        manager.client.fleet(None).unwrap().observed_at,
        initial.observed_at
    );
    *manager.session.initial_snapshot.lock().unwrap() = Some((0, initial.clone()));
    manager.session.epoch.fetch_add(1, Ordering::AcqRel);
    assert_ne!(
        manager.client.fleet(None).unwrap().observed_at,
        initial.observed_at
    );
}

#[test]
fn native_multiline_settings_preserve_values_and_reject_unsafe_controls() {
    let mut settings = fixture("settings-list");
    for kind in ["multiline", "link-list", "mount-list", "name-list"] {
        settings["settings"][0]["type"] = json!(kind);
        settings["settings"][0]["value"] = json!("first\nsecond");
        settings["settings"][0]["default"] = json!("first\nsecond");
        settings["settings"][0]["provenance"][0]["value"] = json!("first\nsecond");
        decode::<SettingsList>(settings.clone())
            .unwrap()
            .validate("default")
            .unwrap();
        settings["settings"][0]["value"] = json!("first\u{0}second");
        assert!(decode::<SettingsList>(settings.clone())
            .unwrap()
            .validate("default")
            .is_err());
    }
    settings["settings"][0]["type"] = json!("string");
    settings["settings"][0]["value"] = json!("first\nsecond");
    assert!(decode::<SettingsList>(settings)
        .unwrap()
        .validate("default")
        .is_err());
}

#[test]
fn omitted_optional_owner_fields_and_empty_arrays_match_real_go_projection() {
    let mut settings = fixture("settings-list");
    let setting = settings["settings"][0].as_object_mut().unwrap();
    setting.remove("value");
    setting.remove("default");
    setting.insert("valueAvailable".into(), json!(false));
    setting.insert("aliases".into(), json!([]));
    setting.insert("enum".into(), json!([]));
    for entry in setting["provenance"].as_array_mut().unwrap() {
        entry.as_object_mut().unwrap().remove("value");
    }
    decode::<SettingsList>(settings)
        .unwrap()
        .validate("default")
        .unwrap();
    let mut profiles = fixture("profile-list");
    for profile in profiles["profiles"].as_array_mut().unwrap() {
        profile.as_object_mut().unwrap().remove("diagnostic");
        profile.as_object_mut().unwrap().remove("descriptorVersion");
        profile["resources"] = json!([]);
    }
    decode::<ProfileList>(profiles)
        .unwrap()
        .validate("default")
        .unwrap();
    let mut sync = fixture("host-sync-status");
    sync.as_object_mut().unwrap().remove("git");
    sync["credentials"]["peers"] = json!([]);
    decode::<HostSyncStatus>(sync)
        .unwrap()
        .validate("example-owner")
        .unwrap();
}

#[test]
fn pending_host_identity_is_still_bound_and_bounded() {
    let mut sync = fixture("host-sync-status");
    sync["hostIdPending"] = json!(true);
    decode::<HostSyncStatus>(sync.clone())
        .unwrap()
        .validate("example-owner")
        .unwrap();
    for host in [
        "different-owner".to_string(),
        "x".repeat(129),
        "private/path".to_string(),
        "".to_string(),
    ] {
        sync["hostId"] = json!(host);
        assert!(decode::<HostSyncStatus>(sync.clone())
            .unwrap()
            .validate("example-owner")
            .is_err());
    }
}

#[test]
fn unavailable_setting_values_cannot_leak_through_direct_or_provenance_fields() {
    let mut settings = fixture("settings-list");
    settings["settings"][0]["valueAvailable"] = json!(false);
    assert!(decode::<SettingsList>(settings.clone())
        .unwrap()
        .validate("default")
        .is_err());
    settings["settings"][0]
        .as_object_mut()
        .unwrap()
        .remove("value");
    assert!(decode::<SettingsList>(settings.clone())
        .unwrap()
        .validate("default")
        .is_err());
    for entry in settings["settings"][0]["provenance"]
        .as_array_mut()
        .unwrap()
    {
        entry.as_object_mut().unwrap().remove("value");
    }
    decode::<SettingsList>(settings)
        .unwrap()
        .validate("default")
        .unwrap();
}

#[test]
fn all_forwarded_setting_and_profile_metadata_remains_bounded() {
    for (field, invalid) in [
        ("aliases", json!(["bad/path"])),
        ("aliases", json!(vec!["alias"; 129])),
        ("scopes", json!(["bad\nvalue"])),
        ("merge", json!("x".repeat(8193))),
        ("application", json!("x".repeat(8193))),
        ("owner", json!("x".repeat(8193))),
    ] {
        let mut settings = fixture("settings-list");
        settings["settings"][0][field] = invalid;
        assert!(
            decode::<SettingsList>(settings)
                .unwrap()
                .validate("default")
                .is_err(),
            "{field}"
        );
    }
    let mut profiles = fixture("profile-list");
    profiles["profiles"][0]["diagnostic"] = json!("x".repeat(129));
    assert!(decode::<ProfileList>(profiles)
        .unwrap()
        .validate("default")
        .is_err());
}

#[test]
fn owner_expiry_format_is_validated_and_held_deadline_never_exceeds_five_minutes() {
    let mut exact = fixture("operation-exact");
    exact["expiresAt"] = json!("not-a-date");
    assert!(decode::<ExactPlan>(exact)
        .unwrap()
        .validate("selection-1", "config")
        .is_err());
    let fixture = ManagerFixture::new("manager-long-expiry");
    let started = Instant::now();
    let plan = fixture.plan();
    let expires = fixture.client.plans.lock().unwrap()[&plan.plan_id].expires;
    assert!(expires <= Instant::now() + Duration::from_secs(300));
    assert!(expires > started + Duration::from_secs(290));
    fixture.client.discard(&plan.plan_id).unwrap();
    assert_eq!(fixture.stats()["retained"], 0);
}

#[test]
fn expired_and_malformed_owner_plans_are_discarded_without_native_capability_or_execution() {
    for (mode, code) in [
        ("manager-expired", "plan_stale"),
        ("manager-bad-expiry", "invalid_response"),
    ] {
        let fixture = ManagerFixture::new(mode);
        let error = fixture
            .client
            .plan(None, None, "config".into(), vec![])
            .err()
            .unwrap();
        assert_eq!(error.code, code);
        assert!(fixture.client.plans.lock().unwrap().is_empty());
        assert_eq!(
            fixture.stats(),
            json!({"executes":0,"discards":1,"retained":0})
        );
    }
}

#[test]
fn declining_execution_preserves_review_but_confirmed_attempt_is_single_use() {
    reviewed_execution_is_explicit_and_single_use("manager", "prompt-default-yes");
}

#[test]
fn owner_noop_execution_still_requires_explicit_confirmation_and_single_use_review() {
    reviewed_execution_is_explicit_and_single_use("manager-noop", "never");
}

fn reviewed_execution_is_explicit_and_single_use(mode: &str, confirmation: &str) {
    let fixture = ManagerFixture::new(mode);
    let plan = fixture.plan();
    assert_eq!(plan.confirmation, confirmation);
    assert_eq!(
        fixture.stats(),
        json!({"executes":0,"discards":0,"retained":1})
    );
    assert_eq!(
        fixture
            .client
            .execute(&plan.plan_id, &plan.digest, false)
            .err()
            .unwrap()
            .code,
        "confirmation_required"
    );
    assert!(fixture
        .client
        .plans
        .lock()
        .unwrap()
        .contains_key(&plan.plan_id));
    assert_eq!(fixture.stats()["executes"], 0);
    let started = fixture
        .client
        .execute(&plan.plan_id, &plan.digest, true)
        .unwrap();
    assert_eq!(started.operation_id, plan.operation_id);
    let deadline = Instant::now() + Duration::from_secs(2);
    loop {
        let event = fixture
            .events
            .recv_timeout(deadline.saturating_duration_since(Instant::now()))
            .unwrap();
        if event.operation_id.as_deref() == Some(&plan.operation_id) {
            assert_eq!(event.state, "completed");
            break;
        }
    }
    assert!(fixture.client.running.lock().unwrap().is_empty());
    assert!(fixture.client.snapshot(&fixture.session).is_ok());
    assert_eq!(
        fixture
            .client
            .execute(&plan.plan_id, &plan.digest, true)
            .err()
            .unwrap()
            .code,
        "plan_stale"
    );
    assert_eq!(fixture.stats()["executes"], 1);
}

#[test]
fn wrong_digest_consumes_review_and_discards_owner_plan_without_execution() {
    let fixture = ManagerFixture::new("manager");
    let plan = fixture.plan();
    let wrong = "b".repeat(64);
    assert_ne!(wrong, plan.digest);
    assert_eq!(
        fixture
            .client
            .execute(&plan.plan_id, &wrong, true)
            .err()
            .unwrap()
            .code,
        "plan_stale"
    );
    assert_eq!(
        fixture
            .client
            .execute(&plan.plan_id, &plan.digest, true)
            .err()
            .unwrap()
            .code,
        "plan_stale"
    );
    assert_eq!(
        fixture.stats(),
        json!({"executes":0,"discards":1,"retained":0})
    );
}

#[test]
fn cleared_stale_flag_cannot_make_an_old_epoch_plan_valid_again() {
    let fixture = ManagerFixture::new("manager");
    let plan = fixture.plan();
    fixture.session.epoch.fetch_add(1, Ordering::AcqRel);
    fixture.session.rpc.take_stale();
    assert_eq!(
        fixture
            .client
            .execute(&plan.plan_id, &plan.digest, true)
            .err()
            .unwrap()
            .code,
        "plan_stale"
    );
    assert_eq!(fixture.stats()["executes"], 0);
    assert_eq!(fixture.stats()["retained"], 0);
}

#[test]
fn gap_during_planning_discards_the_review_before_it_can_be_executed() {
    let fixture = ManagerFixture::new("manager-gap-plan");
    let error = fixture
        .client
        .plan(None, None, "config".into(), vec![])
        .err()
        .unwrap();
    assert_eq!(error.code, "plan_stale");
    assert!(fixture.client.plans.lock().unwrap().is_empty());
    assert_eq!(fixture.stats()["executes"], 0);
    assert_eq!(fixture.stats()["retained"], 0);
}

#[test]
fn gap_during_snapshot_is_not_erased_by_the_query_completion() {
    let fixture = ManagerFixture::new("manager-gap-snapshot");
    let _ = snapshot(&fixture.session);
    assert!(fixture.session.dirty.load(Ordering::Acquire));
    assert!(
        fixture.session.rpc.take_stale(),
        "a gap occurring during a snapshot still needs resync"
    );
}

#[test]
fn immediate_cancellation_cannot_overtake_the_execute_request() {
    for _ in 0..8 {
        let fixture = ManagerFixture::new("manager-cancel");
        let plan = fixture.plan();
        let started = fixture
            .client
            .execute(&plan.plan_id, &plan.digest, true)
            .unwrap();
        fixture.client.cancel(&started.operation_id).unwrap();
        let deadline = Instant::now() + Duration::from_secs(2);
        loop {
            let event = fixture
                .events
                .recv_timeout(deadline.saturating_duration_since(Instant::now()))
                .unwrap();
            if event.operation_id.as_deref() == Some(&started.operation_id) {
                assert_eq!(event.state, "cancelled");
                break;
            }
        }
        assert!(fixture.client.running.lock().unwrap().is_empty());
        assert_eq!(fixture.stats()["executes"], 1);
    }
}
#[test]
fn shutdown_closes_cached_children_and_rejects_new_native_work() {
    let fixture = ManagerFixture::new("manager");
    let _ = fixture.plan();
    fixture.client.shutdown();
    assert!(fixture.client.sessions.lock().unwrap().is_empty());
    assert!(fixture.client.plans.lock().unwrap().is_empty());
    assert!(fixture.session.rpc.is_closed());
    assert_eq!(
        fixture.client.fleet(None).err().unwrap().code,
        "native_unavailable"
    );
    assert_eq!(
        fixture
            .client
            .connect("invalid", true, "invalid")
            .err()
            .unwrap()
            .code,
        "native_unavailable"
    );
    assert!(!fixture.owner.store_root().exists());
}

struct SmokeShutdown(Arc<Client>);
impl Drop for SmokeShutdown {
    fn drop(&mut self) {
        self.0.shutdown();
    }
}

#[cfg(target_os = "linux")]
enum RelayControl {
    Pause,
    Resume,
    Stop,
}
#[cfg(target_os = "linux")]
struct RelayBuffer {
    bytes: [u8; 8192],
    start: usize,
    end: usize,
    eof: bool,
    finished: bool,
}
#[cfg(target_os = "linux")]
impl RelayBuffer {
    fn new() -> Self {
        Self {
            bytes: [0; 8192],
            start: 0,
            end: 0,
            eof: false,
            finished: false,
        }
    }
    fn flush(&mut self, target: &mut impl std::io::Write) -> std::io::Result<()> {
        if self.start < self.end {
            match target.write(&self.bytes[self.start..self.end]) {
                Ok(0) => return Err(std::io::ErrorKind::WriteZero.into()),
                Ok(count) => self.start += count,
                Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {}
                Err(error) => return Err(error),
            }
        }
        if self.start == self.end {
            self.start = 0;
            self.end = 0;
        }
        Ok(())
    }
    fn pump(
        &mut self,
        source: &mut std::net::TcpStream,
        target: &mut std::net::TcpStream,
    ) -> std::io::Result<()> {
        use std::io::Read;
        if self.finished {
            return Ok(());
        }
        self.flush(target)?;
        if self.end == 0 && !self.eof {
            match source.read(&mut self.bytes) {
                Ok(0) => self.eof = true,
                Ok(count) => self.end = count,
                Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {}
                Err(error) => return Err(error),
            }
        }
        if self.eof && self.end == 0 {
            target.shutdown(std::net::Shutdown::Write)?;
            self.finished = true;
        }
        Ok(())
    }
}
#[cfg(target_os = "linux")]
struct OwnedTcpRelay {
    address: std::net::SocketAddr,
    control: mpsc::SyncSender<(RelayControl, mpsc::SyncSender<()>)>,
    worker: Option<thread::JoinHandle<Result<(), &'static str>>>,
}
#[cfg(target_os = "linux")]
impl OwnedTcpRelay {
    fn new(destination: std::net::SocketAddr) -> Self {
        use std::net::{Ipv4Addr, TcpListener, TcpStream};
        assert_eq!(destination.ip(), std::net::IpAddr::V4(Ipv4Addr::LOCALHOST));
        assert_ne!(destination.port(), 0);
        let listener = TcpListener::bind((Ipv4Addr::LOCALHOST, 0)).unwrap();
        let address = listener.local_addr().unwrap();
        listener.set_nonblocking(true).unwrap();
        let (control, receive) = mpsc::sync_channel::<(RelayControl, mpsc::SyncSender<()>)>(1);
        let worker = thread::spawn(move || {
            let mut paused = false;
            let mut pairs: Vec<(TcpStream, TcpStream, RelayBuffer, RelayBuffer)> = Vec::new();
            loop {
                match receive.try_recv() {
                    Ok((command, ack)) => {
                        let stop = matches!(command, RelayControl::Stop);
                        match command {
                            RelayControl::Pause => paused = true,
                            RelayControl::Resume => paused = false,
                            RelayControl::Stop => {}
                        }
                        let _ = ack.send(());
                        if stop {
                            return Ok(());
                        }
                    }
                    Err(mpsc::TryRecvError::Disconnected) => return Ok(()),
                    Err(mpsc::TryRecvError::Empty) => {}
                }
                // One accept per tick keeps control responsive. No more than 16
                // owned pairs, each with two fixed 8 KiB buffers, can be retained.
                if pairs.len() < 16 {
                    match listener.accept() {
                        Ok((front, peer)) => {
                            if peer.ip() != address.ip() {
                                return Err("relay peer was not loopback");
                            }
                            let back = TcpStream::connect_timeout(
                                &destination,
                                Duration::from_millis(200),
                            )
                            .map_err(|_| "relay backend connection failed")?;
                            front
                                .set_nonblocking(true)
                                .map_err(|_| "relay front configuration failed")?;
                            back.set_nonblocking(true)
                                .map_err(|_| "relay backend configuration failed")?;
                            pairs.push((front, back, RelayBuffer::new(), RelayBuffer::new()));
                        }
                        Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {}
                        Err(_) => return Err("relay accept failed"),
                    }
                }
                if !paused {
                    let mut index = 0;
                    while index < pairs.len() {
                        let (front, back, outgoing, incoming) = &mut pairs[index];
                        let result = outgoing
                            .pump(front, back)
                            .and_then(|_| incoming.pump(back, front));
                        let retire = match result {
                            Ok(()) => outgoing.finished && incoming.finished,
                            Err(error)
                                if matches!(
                                    error.kind(),
                                    std::io::ErrorKind::BrokenPipe
                                        | std::io::ErrorKind::ConnectionReset
                                        | std::io::ErrorKind::ConnectionAborted
                                        | std::io::ErrorKind::NotConnected
                                ) =>
                            {
                                true
                            }
                            Err(_) => return Err("relay forwarding failed"),
                        };
                        if retire {
                            pairs.swap_remove(index);
                        } else {
                            index += 1;
                        }
                    }
                }
                thread::sleep(Duration::from_millis(5));
            }
        });
        Self {
            address,
            control,
            worker: Some(worker),
        }
    }
    fn command(&self, command: RelayControl) {
        let (ack, receive) = mpsc::sync_channel(1);
        assert!(
            self.control.send((command, ack)).is_ok(),
            "relay worker ended before control"
        );
        assert!(
            receive.recv_timeout(QUERY_TIMEOUT).is_ok(),
            "relay control deadline"
        );
    }
    fn stop(&mut self) {
        self.command(RelayControl::Stop);
        assert!(
            self.worker.take().unwrap().join().unwrap().is_ok(),
            "relay worker failed"
        );
    }
}
#[cfg(target_os = "linux")]
impl Drop for OwnedTcpRelay {
    fn drop(&mut self) {
        if let Some(worker) = self.worker.take() {
            let (ack, _) = mpsc::sync_channel(1);
            let _ = self.control.send((RelayControl::Stop, ack));
            let _ = worker.join();
        }
    }
}
#[cfg(target_os = "linux")]
#[test]
fn tcp_relay_retains_partial_writes_and_backpressure() {
    use std::io::{self, Write};
    struct ShortWriter {
        output: Vec<u8>,
        blocked: bool,
    }
    impl Write for ShortWriter {
        fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
            if self.blocked {
                return Err(io::ErrorKind::WouldBlock.into());
            }
            let count = bytes.len().min(3);
            self.output.extend_from_slice(&bytes[..count]);
            Ok(count)
        }
        fn flush(&mut self) -> io::Result<()> {
            Ok(())
        }
    }
    let mut pending = RelayBuffer::new();
    pending.bytes[..7].copy_from_slice(b"payload");
    pending.end = 7;
    let mut target = ShortWriter {
        output: Vec::new(),
        blocked: false,
    };
    pending.flush(&mut target).unwrap();
    assert_eq!(pending.start, 3);
    target.blocked = true;
    pending.flush(&mut target).unwrap();
    assert_eq!(pending.start, 3);
    assert_eq!(target.output, b"pay");
    target.blocked = false;
    pending.flush(&mut target).unwrap();
    pending.flush(&mut target).unwrap();
    assert_eq!(target.output, b"payload");
    assert_eq!((pending.start, pending.end), (0, 0));
}
#[cfg(target_os = "linux")]
#[test]
fn tcp_relay_pauses_both_directions_and_resumes_without_losing_bytes() {
    use std::io::{Read, Write};
    use std::net::{TcpListener, TcpStream};
    let listener = TcpListener::bind(("127.0.0.1", 0)).unwrap();
    let mut relay = OwnedTcpRelay::new(listener.local_addr().unwrap());
    let mut front = TcpStream::connect(relay.address).unwrap();
    let (mut back, _) = listener.accept().unwrap();
    front
        .set_read_timeout(Some(Duration::from_millis(100)))
        .unwrap();
    back.set_read_timeout(Some(Duration::from_millis(100)))
        .unwrap();
    relay.command(RelayControl::Pause);
    front.write_all(b"toward owner").unwrap();
    back.write_all(b"toward client").unwrap();
    let mut byte = [0];
    for socket in [&mut front, &mut back] {
        assert!(matches!(
            socket.read(&mut byte).unwrap_err().kind(),
            std::io::ErrorKind::WouldBlock | std::io::ErrorKind::TimedOut
        ));
        socket.set_read_timeout(Some(QUERY_TIMEOUT)).unwrap();
    }
    relay.command(RelayControl::Resume);
    let mut received = [0; 12];
    back.read_exact(&mut received).unwrap();
    assert_eq!(&received, b"toward owner");
    let mut received = [0; 13];
    front.read_exact(&mut received).unwrap();
    assert_eq!(&received, b"toward client");
    // Larger than the fixed buffer: every byte must survive repeated reads/writes.
    let payload = vec![0x5a; 65537];
    front.write_all(&payload).unwrap();
    let mut received = vec![0; payload.len()];
    back.read_exact(&mut received).unwrap();
    assert_eq!(received, payload);
    front.shutdown(std::net::Shutdown::Write).unwrap();
    assert_eq!(back.read(&mut byte).unwrap(), 0);
    relay.stop();
    assert!(TcpStream::connect(relay.address).is_err());
}
#[cfg(target_os = "linux")]
#[test]
fn tcp_relay_rejects_foreign_targets_and_bounds_live_pairs() {
    use std::net::{TcpListener, TcpStream};
    for destination in ["192.0.2.1:22", "127.0.0.1:0"] {
        let address = destination.parse().unwrap();
        assert!(std::panic::catch_unwind(|| OwnedTcpRelay::new(address)).is_err());
    }
    let listener = TcpListener::bind(("127.0.0.1", 0)).unwrap();
    listener.set_nonblocking(true).unwrap();
    let mut relay = OwnedTcpRelay::new(listener.local_addr().unwrap());
    relay.command(RelayControl::Pause);
    let mut fronts: Vec<_> = (0..16)
        .map(|_| TcpStream::connect(relay.address).unwrap())
        .collect();
    let mut backs = Vec::new();
    let deadline = Instant::now() + QUERY_TIMEOUT;
    let accept = || loop {
        match listener.accept() {
            Ok((socket, _)) => break socket,
            Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {
                assert!(Instant::now() < deadline, "relay acceptance deadline");
                thread::sleep(Duration::from_millis(5));
            }
            Err(_) => panic!("owned relay backend accept failed"),
        }
    };
    for _ in 0..16 {
        backs.push(accept());
    }
    let _queued = TcpStream::connect(relay.address).unwrap();
    relay.command(RelayControl::Pause);
    assert_eq!(
        listener.accept().unwrap_err().kind(),
        std::io::ErrorKind::WouldBlock
    );
    drop(fronts.pop());
    drop(backs.pop());
    relay.command(RelayControl::Resume);
    let _replacement = accept();
    relay.stop();
}
#[cfg(target_os = "linux")]
#[test]
fn tcp_relay_drop_joins_and_closes_owned_sockets_during_unwind() {
    use std::io::Read;
    use std::net::{TcpListener, TcpStream};
    let listener = TcpListener::bind(("127.0.0.1", 0)).unwrap();
    let relay = OwnedTcpRelay::new(listener.local_addr().unwrap());
    let address = relay.address;
    let mut front = TcpStream::connect(address).unwrap();
    let (mut back, _) = listener.accept().unwrap();
    relay.command(RelayControl::Pause);
    let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(move || {
        let _relay = relay;
        panic!("synthetic relay unwind");
    }));
    assert!(result.is_err());
    for socket in [&mut front, &mut back] {
        socket.set_read_timeout(Some(QUERY_TIMEOUT)).unwrap();
        assert_eq!(socket.read(&mut [0]).unwrap(), 0);
    }
    assert!(TcpStream::connect(address).is_err());
}

#[cfg(target_os = "linux")]
#[derive(Clone, Copy, PartialEq, Eq)]
enum PhysicalOutage {
    ByteFlow,
    PacketLoss,
    InterfaceReplacement,
}

#[cfg(target_os = "linux")]
fn physical_ssh_byte_flow_stall(
    record: &ConnectionSummary,
    control_root: &std::path::Path,
    outage: PhysicalOutage,
) {
    use std::fs;
    use std::net::{Ipv4Addr, SocketAddr};
    actor_private_directory(control_root).unwrap();
    assert_eq!(
        actor_read(&control_root.join(".marker")).unwrap(),
        b"subyard-veranda-native-v1\n"
    );
    let endpoint = crate::ssh::Endpoint::parse(&record.destination).unwrap();
    assert_eq!(
        endpoint.host,
        if outage == PhysicalOutage::InterfaceReplacement {
            "198.18.0.2"
        } else {
            "127.0.0.1"
        }
    );
    let port: u16 = std::str::from_utf8(&actor_read(&control_root.join("owner.port")).unwrap())
        .unwrap()
        .parse()
        .unwrap();
    assert_eq!(endpoint.port, port);
    let mut relay = (outage == PhysicalOutage::ByteFlow)
        .then(|| OwnedTcpRelay::new(SocketAddr::from((Ipv4Addr::LOCALHOST, port))));
    // Each phase has a distinct store with the same authoritative HostID.
    let fixture = OwnerFixture::new();
    let (sender, events) = mpsc::sync_channel(128);
    let client = Client::new(
        fixture.store_root(),
        Arc::new(move |event| {
            let _ = sender.try_send(event);
        }),
    );
    let _shutdown = SmokeShutdown(client.clone());
    let destination = format!(
        "{}@{}:{}",
        endpoint.user.unwrap(),
        endpoint.host,
        relay.as_ref().map_or(port, |relay| relay.address.port())
    );
    let assessment = client.assess(&destination, None).unwrap();
    let registered = client
        .connect(&assessment.assessment_id, true, &assessment.fingerprint)
        .unwrap();
    assert_eq!(registered.host_id, record.host_id);
    let initial = client.fleet(Some(registered.id.clone())).unwrap();
    let key = SessionKey {
        connection: Some(registered.id.clone()),
        yard: None,
    };
    let established = client.sessions.lock().unwrap()[&key].clone();
    let settings = established
        .rpc
        .call("settings.list", "", json!({}), QUERY_TIMEOUT)
        .unwrap();
    let store_path = fixture.store_root().join("connections.json");
    let pin_path = fixture
        .store_root()
        .join(format!("veranda-stored-{}.known_hosts", registered.id));
    let saved = actor_read(&store_path).unwrap();
    let pin = actor_read(&pin_path).unwrap();
    while events.try_recv().is_ok() {}
    // Five-second keepalives, eight-second negotiation and two-second backoff
    // must complete within each phase's fixed bound.
    let deadline = Instant::now() + Duration::from_secs(45);
    let phase_control = |action: &str| {
        assert!(matches!(action, "drop" | "remove" | "restore"));
        let phase = if outage == PhysicalOutage::InterfaceReplacement {
            "interface"
        } else {
            "packet-loss"
        };
        let request = control_root.join(format!("{phase}.{action}.request"));
        let temporary = control_root.join(format!("{phase}.{action}.request.tmp"));
        let done = control_root.join(format!("{phase}.{action}.done"));
        let payload = format!("{phase}-{action}-v1\n");
        assert!(!request.exists() && !done.exists());
        actor_write(&temporary, payload.as_bytes()).unwrap();
        fs::rename(temporary, request).unwrap();
        loop {
            if done.exists() {
                assert_eq!(actor_read(&done).unwrap(), payload.as_bytes());
                break;
            }
            assert!(Instant::now() < deadline, "network outage control deadline");
            thread::sleep(Duration::from_millis(25));
        }
    };
    if let Some(relay) = &relay {
        relay.command(RelayControl::Pause);
    } else {
        phase_control(if outage == PhysicalOutage::InterfaceReplacement {
            "remove"
        } else {
            "drop"
        });
    }
    let failure = established
        .rpc
        .call("owner.inventory", "", json!({}), QUERY_TIMEOUT)
        .unwrap_err();
    assert!(matches!(
        failure.code.as_str(),
        "rpc_timeout" | "disconnected"
    ));
    let mut lost = false;
    let mut retrying = false;
    loop {
        let event = events
            .recv_timeout(deadline.saturating_duration_since(Instant::now()))
            .expect("stalled SSH transport did not report failed reconnect");
        if event.connection_id.as_deref() != Some(&registered.id) || event.yard.is_some() {
            continue;
        }
        assert!(event.state != "connected" && event.snapshot.is_none());
        if event.state == "reconnecting" {
            assert!(lost);
            retrying = true;
        }
        if event.state == "disconnected" {
            lost = true;
            if retrying {
                assert!(established.rpc.is_closed());
                break;
            }
        }
    }
    assert!(client.running.lock().unwrap().is_empty());
    assert_eq!(actor_read(&store_path).unwrap(), saved);
    assert_eq!(actor_read(&pin_path).unwrap(), pin);
    if let Some(relay) = &relay {
        relay.command(RelayControl::Resume);
    } else {
        // The helper verifies actual kernel mutation before acknowledging it.
        phase_control("restore");
    }
    if outage == PhysicalOutage::PacketLoss {
        let dropped: u64 =
            std::str::from_utf8(&actor_read(&control_root.join("packet-loss.drops")).unwrap())
                .unwrap()
                .parse()
                .unwrap();
        assert!(dropped > 0, "kernel packet loss was not observed");
        println!("veranda-native-packet-loss: dropped={dropped}");
    }
    loop {
        let event = events
            .recv_timeout(deadline.saturating_duration_since(Instant::now()))
            .expect("production monitor did not recover after packet flow restoration");
        if event.connection_id.as_deref() != Some(&registered.id) || event.yard.is_some() {
            continue;
        }
        if event.state == "connected" {
            let snapshot = event
                .snapshot
                .expect("recovered SSH session lacked a fresh snapshot");
            assert_eq!(snapshot.owner.id, record.host_id);
            assert_eq!(snapshot.current_yard_name, initial.current_yard_name);
            break;
        }
    }
    let replacement = client.sessions.lock().unwrap()[&key].clone();
    assert!(!Arc::ptr_eq(&established, &replacement));
    assert!(!replacement.rpc.is_closed());
    assert_eq!(
        replacement
            .rpc
            .call("settings.list", "", json!({}), QUERY_TIMEOUT)
            .unwrap(),
        settings
    );
    assert_eq!(actor_read(&store_path).unwrap(), saved);
    assert_eq!(actor_read(&pin_path).unwrap(), pin);
    assert_eq!(client.connections().unwrap(), vec![registered]);
    assert!(client.running.lock().unwrap().is_empty());
    client.shutdown();
    if let Some(relay) = &mut relay {
        relay.stop();
    }
    assert!(Instant::now() < deadline, "SSH stall phase deadline");
    // No operation was submitted, no trust was repaired, and no SSH child was
    // killed to induce an outage. These phases do not simulate sleep/wake.
    assert!(fs::read(store_path).unwrap() == saved);
    match outage {
        PhysicalOutage::PacketLoss => println!("ok: owned IPv4 packet loss fails bounded reads and autonomously restores pinned subscribed state"),
        PhysicalOutage::ByteFlow => println!("ok: owned SSH byte-flow stall fails bounded reads and autonomously restores pinned subscribed state"),
        PhysicalOutage::InterfaceReplacement => println!("ok: owned interface replacement fails bounded reads and autonomously restores pinned subscribed state"),
    }
}

#[cfg(target_os = "linux")]
fn physical_ssh_recovery_and_churn(
    store_root: &std::path::Path,
    record: &ConnectionSummary,
    control_root: &std::path::Path,
) {
    use std::fs::{self, OpenOptions};
    use std::io::Write;
    use std::os::unix::fs::{MetadataExt, OpenOptionsExt, PermissionsExt};
    let root_metadata = fs::symlink_metadata(control_root).unwrap();
    assert!(root_metadata.is_dir());
    assert_eq!(root_metadata.permissions().mode() & 0o777, 0o700);
    assert_eq!(root_metadata.uid(), unsafe { libc::geteuid() });
    let marker = control_root.join(".marker");
    let marker_metadata = fs::symlink_metadata(&marker).unwrap();
    assert!(marker_metadata.is_file());
    assert_eq!(marker_metadata.permissions().mode() & 0o777, 0o600);
    assert_eq!(marker_metadata.uid(), unsafe { libc::geteuid() });
    assert_eq!(
        fs::read_to_string(marker).unwrap(),
        "subyard-veranda-native-v1\n"
    );
    let new_client = || {
        let (sender, events) = mpsc::sync_channel(128);
        let client = Client::new(
            store_root.to_path_buf(),
            Arc::new(move |event| {
                let _ = sender.try_send(event);
            }),
        );
        (client, events)
    };
    let phase_start = Instant::now();
    let saved = fs::read(store_root.join("connections.json")).unwrap();
    let (loaded, _) = new_client();
    let _loaded_shutdown = SmokeShutdown(loaded.clone());
    assert_eq!(loaded.connections().unwrap(), vec![record.clone()]);
    let fleet = loaded.fleet(Some(record.id.clone())).unwrap();
    assert_eq!(fleet.owner.id, record.host_id);
    let yard = fleet.current_yard_name;
    // Current-yard requests intentionally share a fleet RPC within one Client.
    // Independent caches let each rejection prove its own live-trust boundary.
    let (planner, _) = new_client();
    let _planner_shutdown = SmokeShutdown(planner.clone());
    let held = planner
        .plan(
            Some(record.id.clone()),
            Some(yard.clone()),
            "config".into(),
            vec![
                "set".into(),
                "SSH_PORT".into(),
                "2223".into(),
                "--scope".into(),
                "yard".into(),
                "--local".into(),
            ],
        )
        .unwrap();
    let host_session = loaded.sessions.lock().unwrap()[&SessionKey {
        connection: Some(record.id.clone()),
        yard: None,
    }]
        .clone();
    let plan_session = planner.plans.lock().unwrap()[&held.plan_id].session.clone();
    let (launcher, _) = new_client();
    let _launcher_shutdown = SmokeShutdown(launcher.clone());
    let (launch_session, _descriptor) = launcher
        .session_descriptor(Some(record.id.clone()), Some(yard.clone()), None, "shell")
        .unwrap();

    let request = control_root.join("rotate.request");
    let temporary_request = control_root.join("rotate.request.tmp");
    let mut request_file = OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .custom_flags(libc::O_NOFOLLOW)
        .open(&temporary_request)
        .unwrap();
    request_file
        .set_permissions(fs::Permissions::from_mode(0o600))
        .unwrap();
    request_file.write_all(b"rotate-host-key-v1\n").unwrap();
    request_file.sync_all().unwrap();
    drop(request_file);
    assert!(!request.exists());
    fs::rename(temporary_request, &request).unwrap();
    let deadline = Instant::now() + Duration::from_secs(10);
    let done = control_root.join("rotate.done");
    while !done.exists() {
        assert!(
            Instant::now() < deadline,
            "fixture host-key rotation timed out"
        );
        thread::sleep(Duration::from_millis(25));
    }
    let done_metadata = fs::symlink_metadata(&done).unwrap();
    assert!(done_metadata.is_file());
    assert_eq!(done_metadata.permissions().mode() & 0o777, 0o600);
    assert_eq!(done_metadata.uid(), unsafe { libc::geteuid() });
    assert_eq!(fs::read_to_string(done).unwrap(), "rotate-host-key-v1\n");

    let (refusing, _) = new_client();
    let _refusing_shutdown = SmokeShutdown(refusing.clone());
    assert_eq!(refusing.connections().unwrap(), vec![record.clone()]);
    assert!(refusing.fleet(Some(record.id.clone())).is_err());
    assert_eq!(
        fs::read(store_root.join("connections.json")).unwrap(),
        saved
    );
    assert_eq!(
        refusing
            .assess(&record.destination, None)
            .err()
            .unwrap()
            .code,
        "host_key_changed"
    );
    let declined = refusing.repair(&record.id).unwrap();
    assert_ne!(
        declined.previous_fingerprint.as_deref(),
        Some(declined.fingerprint.as_str())
    );
    assert_eq!(
        refusing
            .connect(&declined.assessment_id, false, &declined.fingerprint)
            .err()
            .unwrap()
            .code,
        "consent_declined"
    );
    assert_eq!(
        fs::read(store_root.join("connections.json")).unwrap(),
        saved
    );
    assert_eq!(run_store_actor(store_root, "repair", Some(record)), "ok");
    assert_ne!(
        fs::read(store_root.join("connections.json")).unwrap(),
        saved
    );
    refusing.shutdown();

    // Listener rotation and another process's consent must not be mistaken for
    // transport loss: every old authenticated RPC still answers directly.
    for session in [&host_session, &plan_session, &launch_session] {
        assert!(!session.rpc.is_closed());
        let inventory: OwnerInventory = decode(
            session
                .rpc
                .call("owner.inventory", "", json!({}), QUERY_TIMEOUT)
                .unwrap(),
        )
        .unwrap();
        assert_eq!(
            inventory
                .into_snapshot(
                    session.rpc.engine_version.clone(),
                    session.current_yard.clone()
                )
                .unwrap()
                .owner
                .id,
            record.host_id
        );
    }
    println!("ok: separate-process repair preserves old authenticated RPC transports");
    assert_eq!(
        launcher
            .store
            .lock()
            .unwrap()
            .registered_ssh_command_bound(launch_session._pin.as_ref().unwrap())
            .err()
            .unwrap()
            .code,
        "connection_changed"
    );
    assert_eq!(
        loaded.fleet(Some(record.id.clone())).err().unwrap().code,
        "connection_changed"
    );
    assert!(host_session.rpc.is_closed());
    assert_eq!(
        planner
            .execute(&held.plan_id, &held.digest, true)
            .err()
            .unwrap()
            .code,
        "connection_changed"
    );
    assert!(plan_session.rpc.is_closed());
    assert!(planner.running.lock().unwrap().is_empty());
    assert_eq!(
        launcher
            .launch(Some(record.id.clone()), Some(yard), None, "shell".into())
            .err()
            .unwrap()
            .code,
        "connection_changed"
    );
    assert!(launch_session.rpc.is_closed());
    assert_eq!(launcher.launch_count.load(Ordering::Acquire), 0);
    println!("ok: separate-process repair rejects stale cached RPC, held execute and bound launch");
    loaded.shutdown();
    planner.shutdown();
    launcher.shutdown();

    let (trusted, events) = new_client();
    let _trusted_shutdown = SmokeShutdown(trusted.clone());
    assert_eq!(trusted.connections().unwrap(), vec![record.clone()]);
    assert_eq!(
        trusted.fleet(Some(record.id.clone())).unwrap().owner.id,
        record.host_id
    );
    let key = SessionKey {
        connection: Some(record.id.clone()),
        yard: None,
    };
    let old = trusted.sessions.lock().unwrap()[&key].clone();
    while events.try_recv().is_ok() {}
    old.rpc.kill_owned_child_for_test().unwrap();
    let deadline = Instant::now() + Duration::from_secs(15);
    let mut disconnected = false;
    loop {
        let event = events
            .recv_timeout(deadline.saturating_duration_since(Instant::now()))
            .expect("the production monitor did not reconnect the SSH owner");
        if event.connection_id.as_deref() != Some(&record.id) {
            continue;
        }
        disconnected |= event.state == "disconnected";
        if event.state == "connected" {
            assert!(
                disconnected,
                "monitor reconnect must follow actual transport loss"
            );
            assert_eq!(event.snapshot.unwrap().owner.id, record.host_id);
            break;
        }
    }
    assert!(!Arc::ptr_eq(&old, &trusted.sessions.lock().unwrap()[&key]));
    assert_eq!(
        trusted.fleet(Some(record.id.clone())).unwrap().owner.id,
        record.host_id
    );
    // Stop only the listener first: an existing authenticated SSH stream is
    // distinct from the endpoint accepting new connections.
    let established = trusted.sessions.lock().unwrap()[&key].clone();
    let settings_before = established
        .rpc
        .call("settings.list", "", json!({}), QUERY_TIMEOUT)
        .unwrap();
    let store_before = fs::read(store_root.join("connections.json")).unwrap();
    let pin_path = store_root.join(format!("veranda-stored-{}.known_hosts", record.id));
    let pin_before = actor_read(&pin_path).unwrap();
    let recovery_deadline = phase_start + Duration::from_secs(45);
    let listener_control = |action: &str| {
        assert!(matches!(action, "stop" | "restore"));
        actor_private_directory(control_root).unwrap();
        assert!(
            actor_read(&control_root.join(".marker")).unwrap() == b"subyard-veranda-native-v1\n"
        );
        let request = control_root.join(format!("listener.{action}.request"));
        let temporary = control_root.join(format!("listener.{action}.request.tmp"));
        let done = control_root.join(format!("listener.{action}.done"));
        assert!(!request.exists() && !done.exists());
        let payload = format!("listener-{action}-v1\n");
        actor_write(&temporary, payload.as_bytes()).unwrap();
        fs::rename(temporary, request).unwrap();
        loop {
            if done.exists() {
                assert!(actor_read(&done).unwrap() == payload.as_bytes());
                break;
            }
            assert!(
                Instant::now() < recovery_deadline,
                "listener control deadline"
            );
            thread::sleep(Duration::from_millis(25));
        }
    };
    listener_control("stop");
    let inventory: OwnerInventory = query(&established, "owner.inventory").unwrap();
    assert_eq!(
        inventory
            .into_snapshot(
                established.rpc.engine_version.clone(),
                established.current_yard.clone()
            )
            .unwrap()
            .owner
            .id,
        record.host_id
    );
    assert!(!established.rpc.is_closed());
    while events.try_recv().is_ok() {}
    established.rpc.kill_owned_child_for_test().unwrap();
    let mut retried = false;
    loop {
        let event = events
            .recv_timeout(recovery_deadline.saturating_duration_since(Instant::now()))
            .expect("listener outage did not report disconnected state");
        if event.connection_id.as_deref() != Some(&record.id) || event.yard.is_some() {
            continue;
        }
        assert!(event.state != "connected" && event.snapshot.is_none());
        retried |= event.state == "reconnecting";
        if event.state == "disconnected" && retried {
            assert!(event
                .message
                .as_ref()
                .is_some_and(|message| !message.is_empty()));
            break;
        }
    }
    assert!(trusted.running.lock().unwrap().is_empty());
    assert!(fs::read(store_root.join("connections.json")).unwrap() == store_before);
    assert!(actor_read(&pin_path).unwrap() == pin_before);
    listener_control("restore");
    loop {
        let event = events
            .recv_timeout(recovery_deadline.saturating_duration_since(Instant::now()))
            .expect("production monitor did not recover after listener restoration");
        if event.connection_id.as_deref() != Some(&record.id) || event.yard.is_some() {
            continue;
        }
        if event.state == "connected" {
            let snapshot = event
                .snapshot
                .expect("restored session has no fresh snapshot");
            assert_eq!(snapshot.owner.id, record.host_id);
            assert_eq!(snapshot.current_yard_name, established.current_yard);
            break;
        }
    }
    let replacement = trusted.sessions.lock().unwrap()[&key].clone();
    assert!(!Arc::ptr_eq(&established, &replacement));
    assert!(!replacement.rpc.is_closed());
    assert!(trusted.running.lock().unwrap().is_empty());
    assert!(fs::read(store_root.join("connections.json")).unwrap() == store_before);
    assert!(actor_read(&pin_path).unwrap() == pin_before);
    assert!(
        replacement
            .rpc
            .call("settings.list", "", json!({}), QUERY_TIMEOUT)
            .unwrap()
            == settings_before,
        "owner settings changed during read-only listener recovery"
    );
    println!("ok: owned listener outage preserves authenticated RPC and autonomously restores subscribed state");
    trusted.shutdown();
    assert!(phase_start.elapsed() < Duration::from_secs(45));
    println!("ok: actual SSH key rotation, explicit repair, saved-store reload and autonomous monitor reconnect");

    physical_ssh_byte_flow_stall(record, control_root, PhysicalOutage::ByteFlow);
    physical_ssh_byte_flow_stall(record, control_root, PhysicalOutage::PacketLoss);
    let mut interface_record = record.clone();
    let endpoint = crate::ssh::Endpoint::parse(&record.destination).unwrap();
    interface_record.destination =
        format!("{}@198.18.0.2:{}", endpoint.user.unwrap(), endpoint.port);
    actor_write(
        &control_root.join("interface.record"),
        &serde_json::to_vec(&interface_record).unwrap(),
    )
    .unwrap();
    actor_write(
        &control_root.join("interface.run.request"),
        b"interface-run-v1\n",
    )
    .unwrap();
    let interface_deadline = Instant::now() + Duration::from_secs(65);
    while !control_root.join("interface.run.done").exists() {
        assert!(
            Instant::now() < interface_deadline,
            "interface segment deadline"
        );
        thread::sleep(Duration::from_millis(25));
    }
    assert_eq!(
        actor_read(&control_root.join("interface.run.done")).unwrap(),
        b"interface-run-v1\n"
    );

    // Each connect performs keyscan, native consent binding, an actual agent-
    // authenticated SSH/RPC handshake, and transport close/reap. This checks the
    // SSH component; it does not claim the whole GUI's resident-memory budget.
    let (churn, _) = new_client();
    let _churn_shutdown = SmokeShutdown(churn.clone());
    let saved = fs::read(store_root.join("connections.json")).unwrap();
    let start = Instant::now();
    let mut completed = 0;
    println!("native-stage: ssh.churn");
    std::io::stdout().flush().unwrap();
    for iteration in 1..=100 {
        assert!(
            start.elapsed() < Duration::from_secs(120),
            "SSH churn timed out"
        );
        println!("native-churn: {iteration} assessment");
        std::io::stdout().flush().unwrap();
        let assessment = churn.assess(&record.destination, None).unwrap();
        println!("native-churn: {iteration} connect");
        std::io::stdout().flush().unwrap();
        assert_eq!(
            churn
                .connect(&assessment.assessment_id, true, &assessment.fingerprint)
                .unwrap(),
            *record
        );
        completed += 1;
        println!("native-churn: {iteration} completed");
        std::io::stdout().flush().unwrap();
    }
    assert_eq!(completed, 100);
    assert!(
        start.elapsed() < Duration::from_secs(120),
        "SSH churn timed out"
    );
    assert_eq!(
        fs::read(store_root.join("connections.json")).unwrap(),
        saved
    );
    let pin_files: Vec<_> = fs::read_dir(store_root)
        .unwrap()
        .map(Result::unwrap)
        .filter(|entry| {
            entry
                .file_name()
                .to_str()
                .is_some_and(|name| name.ends_with(".known_hosts"))
        })
        .map(|entry| entry.file_name())
        .collect();
    assert_eq!(
        pin_files,
        vec![std::ffi::OsString::from(format!(
            "veranda-stored-{}.known_hosts",
            record.id
        ))]
    );
    println!(
        "ok: 100 SSH assess/connect/disconnect handshakes; unchanged store and no provisional pins"
    );
    let removal = churn.assess_removal(&record.id).unwrap();
    churn.remove(&removal.assessment_id, true).unwrap();
    assert!(churn.connections().unwrap().is_empty());
}

#[cfg(target_os = "linux")]
fn physical_terminal_sessions(
    client: &Client,
    connection: Option<String>,
    fleet: &LocalFleetSnapshot,
) {
    use std::io::Write;
    use std::os::unix::fs::{MetadataExt, OpenOptionsExt, PermissionsExt};
    let root = PathBuf::from(
        std::env::var_os("VERANDA_TEST_TERMINAL_CONTROL_ROOT").expect("marked terminal fixture"),
    );
    #[derive(Deserialize)]
    #[serde(deny_unknown_fields)]
    struct XvfbIdentity {
        pid: u32,
        start: u64,
    }
    let verified_display = || -> Result<String, &'static str> {
        actor_private_directory(&root)?;
        if actor_read(&root.join(".marker"))? != b"subyard-veranda-native-v1\n" {
            return Err("invalid terminal fixture marker");
        }
        let display_bytes = actor_read(&root.join("display"))?;
        let number = std::str::from_utf8(&display_bytes)
            .ok()
            .and_then(|value| value.strip_suffix('\n'))
            .filter(|value| !value.is_empty() && value.len() <= 5)
            .filter(|value| value.bytes().all(|byte| byte.is_ascii_digit()))
            .filter(|value| value.parse::<u16>().is_ok())
            .ok_or("invalid terminal fixture display")?;
        let display = format!(":{number}");
        if std::env::var("DISPLAY").ok().as_deref() != Some(display.as_str()) {
            return Err("terminal fixture display changed");
        }
        let identity: XvfbIdentity =
            serde_json::from_slice(&actor_read(&root.join("xvfb.identity"))?)
                .map_err(|_| "invalid terminal fixture identity")?;
        if identity.pid < 2 || identity.start == 0 {
            return Err("invalid terminal fixture identity");
        }
        let process = PathBuf::from(format!("/proc/{}", identity.pid));
        let metadata = process.metadata().map_err(|_| "terminal Xvfb exited")?;
        let stat =
            std::fs::read_to_string(process.join("stat")).map_err(|_| "terminal Xvfb exited")?;
        let fields: Vec<_> = stat
            .rsplit(')')
            .next()
            .unwrap()
            .split_whitespace()
            .collect();
        if metadata.uid() != unsafe { libc::geteuid() }
            || fields.first() == Some(&"Z")
            || fields.get(2).and_then(|value| value.parse::<u32>().ok()) != Some(identity.pid)
            || fields.get(19).and_then(|value| value.parse::<u64>().ok()) != Some(identity.start)
            || std::fs::read_link(process.join("exe")).ok().as_deref()
                != Some(std::path::Path::new("/usr/bin/Xvfb"))
        {
            return Err("terminal Xvfb identity changed");
        }
        Ok(display)
    };
    verified_display().unwrap();
    let yard = &fleet.current_yard_name;
    let project = fleet
        .owner
        .yards
        .iter()
        .find(|entry| &entry.name == yard)
        .unwrap()
        .projects
        .first()
        .expect("registered fixture project")
        .id
        .clone();
    assert!(crate::local_fleet::safe_id(&project, 128));
    let project_file = root.join("terminal-project-id");
    if project_file.exists() {
        let metadata = std::fs::symlink_metadata(&project_file).unwrap();
        assert!(metadata.is_file());
        assert_eq!(metadata.permissions().mode() & 0o777, 0o600);
        assert_eq!(metadata.uid(), unsafe { libc::geteuid() });
        assert_eq!(
            std::fs::read_to_string(&project_file).unwrap().trim(),
            project
        );
    } else {
        let mut file = std::fs::OpenOptions::new()
            .write(true)
            .create_new(true)
            .custom_flags(libc::O_NOFOLLOW)
            .mode(0o600)
            .open(&project_file)
            .unwrap();
        writeln!(file, "{project}").unwrap();
    }
    let run = |arguments: &[String]| -> String {
        let mut command = std::process::Command::new("yard");
        command.args(["-Y", yard]).args(arguments);
        String::from_utf8(crate::ssh::bounded_output(&mut command, None).unwrap())
            .unwrap()
            .trim()
            .to_string()
    };
    for (index, (target_yard, target_project)) in [
        (None, None),
        (Some(yard.clone()), None),
        (Some(yard.clone()), Some(project.clone())),
    ]
    .into_iter()
    .enumerate()
    {
        let mut prefix = vec!["shell".to_string()];
        if let Some(project) = &target_project {
            prefix.push(project.clone());
        }
        prefix.push("--".into());
        let expected = if target_yard.is_some() {
            let mut args = prefix.clone();
            args.push("pwd".into());
            run(&args)
        } else if connection.is_some() {
            std::env::var("HOME").unwrap()
        } else {
            std::env::current_dir()
                .unwrap()
                .to_str()
                .unwrap()
                .to_string()
        };
        let guest_directory = if target_yard.is_some() {
            let mut args = prefix.clone();
            args.extend([
                "mktemp".into(),
                "-d".into(),
                "/tmp/veranda-terminal.XXXXXX".into(),
            ]);
            let directory = run(&args);
            assert!(
                directory.starts_with("/tmp/veranda-terminal.")
                    && directory
                        .bytes()
                        .all(|b| b.is_ascii_alphanumeric() || b"/.-".contains(&b))
            );
            Some(directory)
        } else {
            None
        };
        let marker = guest_directory
            .as_ref()
            .map(|directory| format!("{directory}/pwd"))
            .unwrap_or_else(|| {
                root.join(format!("terminal-host-{}-{index}", std::process::id()))
                    .to_str()
                    .unwrap()
                    .to_string()
            });
        if target_yard.is_some() {
            let mut args = prefix.clone();
            args.extend(["test".into(), "!".into(), "-e".into(), marker.clone()]);
            run(&args);
        } else {
            assert!(std::fs::symlink_metadata(&marker).is_err());
        }
        client
            .launch(
                connection.clone(),
                target_yard.clone(),
                target_project.clone(),
                "shell".into(),
            )
            .unwrap();
        let deadline = Instant::now() + Duration::from_secs(30);
        println!("native-stage: terminal.window-wait");
        std::io::Write::flush(&mut std::io::stdout()).unwrap();
        let window = loop {
            let output = std::process::Command::new("xdotool")
                .env("DISPLAY", verified_display().unwrap())
                .args(["search", "--onlyvisible", "--class", "^XTerm$"])
                .output()
                .unwrap();
            let windows = String::from_utf8(output.stdout).unwrap();
            let rows: Vec<_> = windows.lines().collect();
            if rows.len() == 1 {
                break rows[0].to_string();
            }
            assert!(Instant::now() < deadline, "terminal window deadline");
            std::thread::sleep(Duration::from_millis(50));
        };
        let input = format!("set -C; printf '%s\\n' \"$PWD\" > {marker}; exit");
        for (stage, args) in [
            ("terminal.focus", vec!["windowfocus", &window]),
            (
                "terminal.type",
                vec![
                    "type",
                    "--window",
                    &window,
                    "--clearmodifiers",
                    "--delay",
                    "1",
                    &input,
                ],
            ),
            (
                "terminal.submit",
                vec!["keydown", "--window", &window, "Return"],
            ),
        ] {
            println!("native-stage: {stage}");
            std::io::Write::flush(&mut std::io::stdout()).unwrap();
            let mut command = std::process::Command::new("xdotool");
            command
                .args(args)
                .env("DISPLAY", verified_display().unwrap());
            let result = crate::ssh::bounded_output(&mut command, None);
            if stage == "terminal.submit" {
                // A focused keydown uses XTEST and leaves Return held. Release it
                // on this owned X server without targeting the now-closing window,
                // even when keydown reports an error after delivering the press.
                let released = verified_display().is_ok_and(|display| {
                    let mut release = std::process::Command::new("xdotool");
                    release.args(["keyup", "Return"]).env("DISPLAY", display);
                    crate::ssh::bounded_output(&mut release, None).is_ok()
                });
                assert!(
                    result.is_ok() && released,
                    "terminal Return gesture failed (keydown failed: {}, keyup failed: {})",
                    result.is_err(),
                    !released
                );
            } else {
                result.unwrap();
            }
        }
        while client.launch_count.load(Ordering::Acquire) != 0 {
            assert!(Instant::now() < deadline, "terminal did not exit");
            std::thread::sleep(Duration::from_millis(50));
        }
        let observed = if target_yard.is_some() {
            let mut args = prefix.clone();
            args.extend(["cat".into(), marker.clone()]);
            run(&args)
        } else {
            std::fs::read_to_string(&marker).unwrap().trim().to_string()
        };
        assert_eq!(observed, expected, "terminal opened the wrong context");
        if target_yard.is_some() {
            let mut args = prefix.clone();
            args.extend(["rm".into(), "--".into(), marker]);
            run(&args);
            let mut args = prefix;
            args.extend(["rmdir".into(), "--".into(), guest_directory.unwrap()]);
            run(&args);
        } else {
            std::fs::remove_file(marker).unwrap();
        }
        let windows = std::process::Command::new("xdotool")
            .env("DISPLAY", verified_display().unwrap())
            .args(["search", "--onlyvisible", "--class", "^XTerm$"])
            .output()
            .unwrap();
        assert!(windows.stdout.is_empty(), "orphan terminal window");
        println!("ok: real terminal context {index}");
    }
}

#[cfg(target_os = "linux")]
#[derive(Debug, Deserialize)]
#[serde(tag = "status", rename_all = "snake_case", deny_unknown_fields)]
enum EditorPwd {
    Pending {},
    Ready { pwd: String },
}
#[cfg(target_os = "linux")]
impl EditorPwd {
    fn parse(output: &str) -> Result<Self, NativeError> {
        serde_json::from_str(output).map_err(|_| crate::transport::invalid())
    }
}
#[cfg(target_os = "linux")]
#[test]
fn editor_pwd_reply_distinguishes_absence_from_all_present_content() {
    assert!(matches!(
        EditorPwd::parse(r#"{"status":"pending"}"#).unwrap(),
        EditorPwd::Pending {}
    ));
    for content in [
        "/synthetic/project\n",
        "editor-pwd-pending-v1",
        "\u{2003}editor-pwd-pending-v1\u{2003}",
    ] {
        let reply = serde_json::json!({"status": "ready", "pwd": content});
        let EditorPwd::Ready { pwd } = EditorPwd::parse(&reply.to_string()).unwrap() else {
            panic!("present marker was classified as pending");
        };
        assert_eq!(pwd, content);
        assert_eq!(pwd.trim() == "/synthetic/project", content.starts_with('/'));
    }
}
#[cfg(target_os = "linux")]
#[test]
fn editor_pwd_reply_rejects_malformed_or_ambiguous_responses() {
    for reply in [
        "not JSON",
        r#"{"status":"absent"}"#,
        r#"{"status":"ready"}"#,
        r#"{"status":"ready","pwd":null}"#,
        r#"{"status":"pending","pwd":"/synthetic/project"}"#,
        r#"{"status":"pending","extra":true}"#,
        r#"{"status":"ready","pwd":"/synthetic/project","extra":true}"#,
    ] {
        assert_eq!(
            EditorPwd::parse(reply).unwrap_err().code,
            "invalid_response"
        );
    }
}
#[cfg(target_os = "linux")]
struct EditorGuest {
    yard: String,
    project: String,
    token: String,
    key: String,
    script: String,
    armed: bool,
}
#[cfg(target_os = "linux")]
impl EditorGuest {
    fn run(&self, action: &str) -> Result<String, NativeError> {
        self.run_until(action, None)
    }
    fn run_until(&self, action: &str, deadline: Option<Instant>) -> Result<String, NativeError> {
        let mut command = std::process::Command::new("yard");
        command.args([
            "-Y",
            &self.yard,
            "shell",
            &self.project,
            "--",
            "python3",
            "-c",
            &self.script,
            action,
            &self.token,
            &self.key,
        ]);
        let output = match deadline {
            Some(deadline) => crate::ssh::bounded_output_until(&mut command, None, deadline),
            None => crate::ssh::bounded_output(&mut command, None),
        }?;
        String::from_utf8(output)
            .map(|output| output.trim().to_owned())
            .map_err(|_| crate::transport::invalid())
    }
    fn pwd_before(&self, deadline: Instant) -> Result<EditorPwd, NativeError> {
        EditorPwd::parse(&self.run_until("pwd", Some(deadline))?)
    }
}
#[cfg(target_os = "linux")]
impl Drop for EditorGuest {
    fn drop(&mut self) {
        if self.armed {
            let _ = self.run("cleanup");
        }
    }
}

#[cfg(target_os = "linux")]
fn physical_editor_session(fleet: &LocalFleetSnapshot) {
    use std::io::Write;
    use std::os::unix::fs::{MetadataExt, OpenOptionsExt, PermissionsExt};
    let started = Instant::now();
    let overall = started + Duration::from_secs(300);
    let root = PathBuf::from(std::env::var_os("VERANDA_TEST_EDITOR_CONTROL_ROOT").unwrap());
    let check = |path: &std::path::Path, directory: bool, mode: u32| {
        let metadata = std::fs::symlink_metadata(path).unwrap();
        assert!(
            if directory {
                metadata.is_dir()
            } else {
                metadata.is_file()
            },
            "unsafe editor fixture path"
        );
        assert_eq!(metadata.permissions().mode() & 0o777, mode);
        assert_eq!(metadata.uid(), unsafe { libc::geteuid() });
        if !directory {
            assert_eq!(metadata.nlink(), 1);
        }
    };
    check(&root, true, 0o700);
    check(&root.join(".marker"), false, 0o600);
    assert!(
        std::fs::read_to_string(root.join(".marker")).unwrap() == "subyard-veranda-native-v1\n"
    );
    check(&root.join("editor"), true, 0o700);
    check(&root.join("editor/.marker"), false, 0o600);
    assert!(
        std::fs::read_to_string(root.join("editor/.marker")).unwrap()
            == "subyard-veranda-editor-v1\n"
    );
    let token = root
        .file_name()
        .unwrap()
        .to_str()
        .unwrap()
        .strip_prefix("native.")
        .unwrap()
        .to_owned();
    assert!(
        !token.is_empty()
            && token.len() <= 32
            && token.bytes().all(|byte| byte.is_ascii_alphanumeric())
    );
    let write = |name: &str, data: &[u8]| {
        assert!(std::fs::symlink_metadata(root.join(name)).is_err());
        let temporary = root.join(format!("{name}.tmp"));
        let mut file = std::fs::OpenOptions::new()
            .write(true)
            .create_new(true)
            .custom_flags(libc::O_NOFOLLOW)
            .mode(0o600)
            .open(&temporary)
            .unwrap();
        file.set_permissions(std::fs::Permissions::from_mode(0o600))
            .unwrap();
        file.write_all(data).unwrap();
        file.sync_all().unwrap();
        drop(file);
        std::fs::rename(temporary, root.join(name)).unwrap();
    };
    write("editor.start", b"editor-segment-v1\n");
    let yard = fleet.current_yard_name.clone();
    let project = fleet
        .owner
        .yards
        .iter()
        .find(|entry| entry.name == yard)
        .unwrap()
        .projects
        .first()
        .unwrap()
        .id
        .clone();
    assert!(crate::local_fleet::safe_id(&project, 128));
    check(&root.join("identity.pub"), false, 0o600);
    let public = std::fs::read_to_string(root.join("identity.pub")).unwrap();
    assert!(public.len() <= 1024);
    let parts: Vec<_> = public.split_whitespace().collect();
    assert!(parts.len() >= 2 && parts[0] == "ssh-ed25519");
    let key = parts[..2].join(" ");
    check(&root.join("bin/editor-guest.py"), false, 0o700);
    let script = std::fs::read_to_string(root.join("bin/editor-guest.py")).unwrap();
    assert!(script.len() <= 16 * 1024);
    let mut pwd = std::process::Command::new("yard");
    pwd.args(["-Y", &yard, "shell", &project, "--", "pwd"]);
    let expected = String::from_utf8(crate::ssh::bounded_output(&mut pwd, None).unwrap())
        .unwrap()
        .trim()
        .to_owned();
    assert!(
        expected.starts_with('/')
            && expected.len() <= 4096
            && !expected.chars().any(char::is_control)
    );
    let mut guest = EditorGuest {
        yard: yard.clone(),
        project: project.clone(),
        token: token.clone(),
        key,
        script,
        armed: true,
    };
    assert!(guest.run("setup").unwrap() == "editor-guest-setup-v1");
    let client = Client::new(root.join("editor-store"), Arc::new(|_| {}));
    let _shutdown = SmokeShutdown(client.clone());
    let destination = std::env::var("VERANDA_TEST_SSH_DESTINATION").unwrap();
    let review = client.assess(&destination, None).unwrap();
    let record = client
        .connect(&review.assessment_id, true, &review.fingerprint)
        .unwrap();
    assert!(record.host_id == fleet.owner.id);
    let session = client
        .session(SessionKey {
            connection: Some(record.id.clone()),
            yard: Some(yard.clone()),
        })
        .unwrap();
    session.rpc.require("session-prepare-v1").unwrap();
    let raw = session
        .rpc
        .call(
            "session.prepare",
            "",
            json!({"kind":"vscode","scope":"yard","projectId":project}),
            QUERY_TIMEOUT,
        )
        .unwrap();
    let descriptor: crate::sessions::Descriptor = decode(raw.clone()).unwrap();
    descriptor
        .validate("vscode", &yard, false, Some(&project))
        .unwrap();
    assert!(
        raw["vscode"]["folderPath"].as_str() == Some(expected.as_str()),
        "editor descriptor project mismatch"
    );
    let port = raw["vscode"]["port"].as_u64().unwrap();
    let wait_done = |name: &str| {
        let deadline = (Instant::now() + Duration::from_secs(15)).min(overall);
        while !root.join(name).exists() {
            assert!(
                Instant::now() < deadline,
                "editor forwarding handshake deadline"
            );
            std::thread::sleep(Duration::from_millis(50));
        }
        check(&root.join(name), false, 0o600);
        assert!(std::fs::read_to_string(root.join(name)).unwrap() == "editor-forwarding-v1\n");
    };
    write(
        "editor.prepare.request",
        &serde_json::to_vec(&json!({"port":port})).unwrap(),
    );
    wait_done("editor.prepare.done");
    println!("ok: editor fixture public-key authorization and narrow owner forwarding");
    let editor_id = format!("{}-{yard}", record.id);
    let alias = format!("veranda-{editor_id}");
    let profile = client
        .store
        .lock()
        .unwrap()
        .prepare_editor_profile(&editor_id)
        .unwrap();
    assert!(profile.starts_with(root.join("editor-store")));
    let settings = profile.join("userdata/User/settings.json");
    let mut file = std::fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .custom_flags(libc::O_NOFOLLOW)
        .mode(0o600)
        .open(&settings)
        .unwrap();
    file.set_permissions(std::fs::Permissions::from_mode(0o600))
        .unwrap();
    // Keep trust enabled. The feature dialog trusts only the current folder,
    // without the startup prompt's optional parent-folder checkbox.
    serde_json::to_writer(&mut file, &json!({
        "workbench.startupEditor":"none", "workbench.tips.enabled":false,
        "workbench.welcomePage.experimentalOnboarding":false,
        "update.mode":"none", "extensions.autoUpdate":false,
        "extensions.autoCheckUpdates":false, "telemetry.telemetryLevel":"off",
        "security.workspace.trust.enabled":true, "security.workspace.trust.startupPrompt":"never",
        "remote.SSH.remotePlatform":{(alias.clone()):"linux"},
        "remote.SSH.serverInstallPath":{(alias.clone()):format!("/tmp/veranda-editor.{token}/server")}
    })).unwrap();
    file.sync_all().unwrap();
    drop(file);
    let windows = || -> Vec<String> {
        // A missing X server must not count as successful window cleanup.
        let mut geometry = std::process::Command::new("xdotool");
        geometry.arg("getdisplaygeometry");
        crate::ssh::bounded_output(&mut geometry, None).unwrap();
        let mut command = std::process::Command::new("xdotool");
        command.args(["search", "--onlyvisible", "--class", "^(Code|code)$"]);
        crate::ssh::bounded_output(&mut command, None)
            .ok()
            .and_then(|bytes| String::from_utf8(bytes).ok())
            .unwrap_or_default()
            .lines()
            .map(str::to_owned)
            .collect()
    };
    assert!(windows().is_empty(), "unexpected pre-existing Code window");
    client
        .launch(
            Some(record.id.clone()),
            Some(yard),
            Some(project),
            "vscode".into(),
        )
        .unwrap();
    let connection_deadline = (Instant::now() + Duration::from_secs(180)).min(overall);
    let window = loop {
        let visible = windows();
        assert!(visible.len() <= 1, "unexpected extra Code window");
        if let Some(window) = visible.first() {
            let mut title = std::process::Command::new("xdotool");
            title.args(["getwindowname", window]);
            let title =
                String::from_utf8(crate::ssh::bounded_output(&mut title, None).unwrap()).unwrap();
            if title.contains(&alias) && guest.run("ready").is_ok() {
                break window.clone();
            }
        }
        assert!(Instant::now() < connection_deadline, "editor connection deadline: verify sandbox, guest authorization and server download access");
        std::thread::sleep(Duration::from_millis(200));
    };
    let xdo = |args: &[&str]| {
        let mut command = std::process::Command::new("xdotool");
        command.args(args);
        crate::ssh::bounded_output(&mut command, None).unwrap();
    };
    let terminal_deadline = (Instant::now() + Duration::from_secs(30)).min(overall);
    check(&root.join("bin/editor-ui.py"), false, 0o700);
    assert!(
        std::fs::metadata(root.join("bin/editor-ui.py"))
            .unwrap()
            .len()
            <= 32 * 1024
    );
    let input = format!("umask 077; set -C; printf '%s\\n' \"$PWD\" > /tmp/veranda-editor.{token}/pwd; chmod 0600 /tmp/veranda-editor.{token}/pwd; exit");
    // The fixture observer exposes only fixed stage markers. It makes each
    // XTEST gesture once, after the corresponding semantic UI state is ready.
    println!("native-stage: editor.accessibility");
    std::io::stdout().flush().unwrap();
    let milliseconds = terminal_deadline
        .saturating_duration_since(Instant::now())
        .as_millis();
    assert!(milliseconds > 0 && milliseconds <= 30000);
    let child = std::process::Command::new("/usr/bin/python3")
        .arg(root.join("bin/editor-ui.py"))
        .arg(&root)
        .arg(&window)
        .arg(milliseconds.to_string())
        .stdin(std::process::Stdio::piped())
        .stdout(std::process::Stdio::inherit())
        .stderr(std::process::Stdio::null())
        .spawn()
        .unwrap();
    struct EditorUi(std::process::Child);
    impl Drop for EditorUi {
        fn drop(&mut self) {
            let _ = self.0.kill();
            let _ = self.0.wait();
        }
    }
    let mut ui = EditorUi(child);
    ui.0.stdin
        .take()
        .unwrap()
        .write_all(input.as_bytes())
        .unwrap();
    loop {
        if let Some(status) = ui.0.try_wait().unwrap() {
            assert!(
                status.success(),
                "editor semantic readiness failed; fixed stage emitted"
            );
            break;
        }
        if Instant::now() >= terminal_deadline {
            println!("native-stage: editor.deadline");
            std::io::stdout().flush().unwrap();
            panic!("editor terminal readiness deadline");
        }
        std::thread::park_timeout(Duration::from_millis(20));
    }
    println!("native-stage: editor.marker-wait");
    std::io::stdout().flush().unwrap();
    loop {
        assert!(
            Instant::now() < terminal_deadline,
            "editor terminal marker deadline"
        );
        match guest.pwd_before(terminal_deadline).unwrap() {
            EditorPwd::Ready { pwd } => {
                assert!(
                    pwd.trim() == expected,
                    "Code integrated terminal opened the wrong project"
                );
                break;
            }
            EditorPwd::Pending {} => {}
        }
        std::thread::sleep(Duration::from_millis(100));
    }
    let cleanup_deadline = (Instant::now() + Duration::from_secs(30)).min(overall);
    xdo(&["key", "ctrl+shift+w"]);
    while client.launch_count.load(Ordering::Acquire) != 0 || !windows().is_empty() {
        assert!(
            Instant::now() < cleanup_deadline,
            "editor window cleanup deadline"
        );
        std::thread::sleep(Duration::from_millis(100));
    }
    assert!(guest.run("cleanup").unwrap() == "editor-guest-cleanup-v1");
    guest.armed = false;
    write("editor.cleanup.request", b"editor-cleanup-v1\n");
    wait_done("editor.cleanup.done");
    assert!(
        Instant::now() < cleanup_deadline && Instant::now() < overall,
        "editor overall deadline"
    );
    let settings: Value = serde_json::from_slice(&std::fs::read(settings).unwrap()).unwrap();
    assert!(settings["security.workspace.trust.enabled"] == true);
    write("editor.finish", b"editor-segment-v1\n");
    println!("ok: real VS Code Remote SSH project terminal and window cleanup");
}

/// The launcher must provide a disposable compatible owner and, for remote
/// runs, its isolated system agent. This entry never locates personal SSH files.
#[cfg(target_os = "linux")]
#[test]
#[ignore = "requires the marked isolated network namespace fixture"]
fn native_interface_recovery() {
    assert_eq!(std::env::var("VERANDA_TEST_DISPOSABLE_OWNER").unwrap(), "1");
    let root = PathBuf::from(std::env::var("VERANDA_TEST_NATIVE_CONTROL_ROOT").unwrap());
    actor_private_directory(&root).unwrap();
    let value: serde_json::Value =
        serde_json::from_slice(&actor_read(&root.join("interface.record")).unwrap()).unwrap();
    assert_eq!(value.as_object().unwrap().len(), 4);
    let record = ConnectionSummary {
        id: value["id"].as_str().unwrap().into(),
        host_id: value["hostId"].as_str().unwrap().into(),
        destination: value["destination"].as_str().unwrap().into(),
        auth_label: value["authLabel"].as_str().unwrap().into(),
    };
    physical_ssh_byte_flow_stall(&record, &root, PhysicalOutage::InterfaceReplacement);
    actor_write(
        &root.join("interface.finish.request"),
        b"interface-finish-v1\n",
    )
    .unwrap();
}

/// Two real kernel sleeps are controlled by the owning pair supervisor. This
/// test deliberately exercises the production wait and autonomous monitor.
#[cfg(target_os = "linux")]
#[test]
#[ignore = "requires the marked RTC sleep pair and a real awake owner"]
fn native_sleep_recovery() {
    assert_eq!(std::env::var("VERANDA_TEST_DISPOSABLE_OWNER").unwrap(), "1");
    let root = PathBuf::from(std::env::var("VERANDA_TEST_SLEEP_CONTROL_ROOT").unwrap());
    actor_private_directory(&root).unwrap();
    let run = std::env::var("SUBYARD_E2E_RUN_ID").unwrap();
    let slot = std::env::var("SUBYARD_E2E_SLOT").unwrap();
    let token = std::env::var("SUBYARD_E2E_SLEEP_TOKEN").unwrap();
    assert_eq!(
        actor_read(&root.join(".marker")).unwrap(),
        format!("subyard-veranda-sleep-v1:{run}:{slot}:1:{token}\n").as_bytes()
    );
    let clock = || {
        let mut value = libc::timespec {
            tv_sec: 0,
            tv_nsec: 0,
        };
        assert_eq!(
            unsafe { libc::clock_gettime(libc::CLOCK_BOOTTIME, &mut value) },
            0
        );
        Duration::new(
            value.tv_sec.try_into().unwrap(),
            value.tv_nsec.try_into().unwrap(),
        )
    };
    let deadline = clock() + Duration::from_secs(170);
    let wait_file = |name: &str| {
        while !root.join(name).exists() {
            assert!(clock() < deadline, "sleep control deadline");
            thread::sleep(Duration::from_millis(20));
        }
        assert_eq!(actor_read(&root.join(name)).unwrap(), b"sleep-cycle-v1\n");
    };
    let write = |name: &str| actor_write(&root.join(name), b"sleep-cycle-v1\n").unwrap();
    let fixture = OwnerFixture::new();
    let (sender, events) = mpsc::sync_channel(256);
    let client = Client::new(
        fixture.store_root(),
        Arc::new(move |event| {
            let _ = sender.try_send(event);
        }),
    );
    let _shutdown = SmokeShutdown(client.clone());
    let assessment = client
        .assess(
            &std::env::var("VERANDA_TEST_SSH_DESTINATION").unwrap(),
            None,
        )
        .unwrap();
    let registered = client
        .connect(&assessment.assessment_id, true, &assessment.fingerprint)
        .unwrap();
    let initial = client.fleet(Some(registered.id.clone())).unwrap();
    let yard = std::env::var("VERANDA_TEST_SLEEP_YARD").unwrap();
    assert!(initial
        .owner
        .yards
        .iter()
        .any(|item| item.name == yard && item.state.eq_ignore_ascii_case("running")));
    let key = SessionKey {
        connection: Some(registered.id.clone()),
        yard: None,
    };
    let established = client.sessions.lock().unwrap()[&key].clone();
    let store_path = fixture.store_root().join("connections.json");
    let pin_path = fixture
        .store_root()
        .join(format!("veranda-stored-{}.known_hosts", registered.id));
    let saved = actor_read(&store_path).unwrap();
    let pin = actor_read(&pin_path).unwrap();
    let call = established
        .rpc
        .begin("settings.list", "sleep-query", json!({}))
        .unwrap();
    let (query_sender, query_result) = mpsc::sync_channel(1);
    let query_session = established.clone();
    let query_root = root.clone();
    thread::spawn(move || {
        actor_write(
            &query_root.join("query-clock.json"),
            &serde_json::to_vec(&json!({
                "started_boottime_ms": clock().as_millis() as u64,
                "timeout_ms": QUERY_TIMEOUT.as_millis() as u64
            }))
            .unwrap(),
        )
        .unwrap();
        actor_write(&query_root.join("query-waiting"), b"sleep-cycle-v1\n").unwrap();
        let _ = query_sender.send(query_session.rpc.wait(call, QUERY_TIMEOUT));
    });
    wait_file("query-waiting");
    write("cycle-1.request");
    wait_file("cycle-1.wake");
    let wake = clock();
    let first = query_result.recv_timeout(Duration::from_secs(1));
    let expired_promptly = first.is_ok();
    let result = first.unwrap_or_else(|_| {
        query_result
            .recv_timeout(Duration::from_secs(6))
            .expect("sleep query deadline")
    });
    let query_after_wake_ms = (clock() - wake).as_millis() as u64;
    let expired_failed = result
        .as_ref()
        .is_err_and(|error| error.code == "rpc_timeout");
    write("query-observed");
    wait_file("query-released");
    // The released response belongs to a removed pending request. A separate
    // real read must remain usable; no mutating operation is submitted.
    let no_late_success = expired_failed
        && established
            .rpc
            .call("settings.list", "", json!({}), QUERY_TIMEOUT)
            .is_ok();
    while events.try_recv().is_ok() {}
    write("cycle-2.request");
    wait_file("cycle-2.wake");
    let recovery_deadline = clock() + Duration::from_secs(45);
    let mut reconnected_snapshot = false;
    while clock() < recovery_deadline {
        if let Ok(event) = events.recv_timeout(Duration::from_millis(100)) {
            if event.connection_id.as_deref() != Some(&registered.id) || event.yard.is_some() {
                continue;
            }
            if let Some(snapshot) = event.snapshot {
                if event.state == "connected"
                    && snapshot.owner.id == registered.host_id
                    && snapshot
                        .owner
                        .yards
                        .iter()
                        .any(|item| item.name == yard && item.state.eq_ignore_ascii_case("stopped"))
                {
                    reconnected_snapshot = true;
                    break;
                }
            }
        }
    }
    write("event.request");
    let event_deadline = clock() + Duration::from_secs(45);
    let mut lifecycle = false;
    let mut changed_snapshot = false;
    while clock() < event_deadline && !(lifecycle && changed_snapshot) {
        if let Ok(event) = events.recv_timeout(Duration::from_millis(100)) {
            if event.connection_id.as_deref() != Some(&registered.id) || event.yard.is_some() {
                continue;
            }
            lifecycle |= event.message.as_deref() == Some("incus.lifecycle");
            if let Some(snapshot) = event.snapshot {
                changed_snapshot |= event.state == "connected"
                    && snapshot.owner.id == registered.host_id
                    && snapshot.owner.yards.iter().any(|item| {
                        item.name == yard && item.state.eq_ignore_ascii_case("running")
                    });
            }
        }
    }
    let replacement = client.sessions.lock().unwrap()[&key].clone();
    reconnected_snapshot &=
        !Arc::ptr_eq(&established, &replacement) && !replacement.rpc.is_closed();
    let proof = json!({"expired_promptly": expired_promptly, "expired_failed": expired_failed,
        "no_late_success": no_late_success, "query_after_wake_ms": query_after_wake_ms,
        "reconnected_snapshot": reconnected_snapshot, "owner_event": lifecycle && changed_snapshot,
        "store_unchanged": actor_read(&store_path).unwrap() == saved,
        "pin_unchanged": actor_read(&pin_path).unwrap() == pin,
        "no_mutations": client.running.lock().unwrap().is_empty() && client.connections().unwrap() == vec![registered]});
    actor_write(
        &root.join("native-proof.json"),
        &serde_json::to_vec(&proof).unwrap(),
    )
    .unwrap();
    assert!(
        proof
            .as_object()
            .unwrap()
            .iter()
            .all(|(name, value)| name == "query_after_wake_ms" || value == &json!(true)),
        "sleep acceptance failed"
    );
}

#[test]
#[ignore = "requires an explicitly disposable installed owner and optional isolated SSH fixture"]
fn native_owner_smoke() {
    assert_eq!(
        std::env::var("VERANDA_TEST_DISPOSABLE_OWNER")
            .ok()
            .as_deref(),
        Some("1"),
        "the native smoke requires a disposable owner"
    );
    let stage = |marker: &'static str| {
        println!("{marker}");
        std::io::Write::flush(&mut std::io::stdout()).unwrap();
    };
    let fixture = OwnerFixture::new();
    let (sender, events) = mpsc::channel();
    let client = Client::new(
        fixture.store_root(),
        Arc::new(move |event| {
            let _ = sender.send(event);
        }),
    );
    let _shutdown = SmokeShutdown(client.clone());
    // A synthetic failing SSH/RPC peer may be supplied independently. Failed
    // negotiation must not leave a saved record or temporary accepted pin.
    if let Ok(destination) = std::env::var("VERANDA_TEST_INCOMPATIBLE_DESTINATION") {
        let review = client.assess(&destination, None).unwrap();
        assert!(client
            .connect(&review.assessment_id, true, &review.fingerprint)
            .is_err());
        assert!(client.connections().unwrap().is_empty());
        assert!(!std::fs::read_dir(fixture.store_root())
            .unwrap()
            .any(|entry| entry
                .unwrap()
                .file_name()
                .to_str()
                .is_some_and(|name| name.ends_with(".known_hosts"))));
    }
    let connection = std::env::var("VERANDA_TEST_SSH_DESTINATION")
        .ok()
        .map(|destination| {
            let review = client.assess(&destination, None).unwrap();
            assert!(client.connections().unwrap().is_empty());
            let record = client
                .connect(&review.assessment_id, true, &review.fingerprint)
                .unwrap();
            assert_eq!(client.connections().unwrap().len(), 1);
            record
        });
    let id = connection.as_ref().map(|record| record.id.clone());
    stage("native-stage: fleet");
    let fleet = client.fleet(id.clone()).unwrap();
    assert_eq!(fleet.engine_version, product_version());
    assert!(!fleet.owner.id.is_empty());
    let yard = fleet.current_yard_name.clone();
    stage("native-stage: yard");
    let details = client.yard(id.clone(), yard.clone()).unwrap();
    stage("native-stage: host");
    client.host(id.clone()).unwrap();
    stage("native-stage: session.prepare");
    let command = serde_json::to_value(
        client
            .shell_command(id.clone(), Some(yard.clone()), None)
            .unwrap(),
    )
    .unwrap();
    assert!(command["command"].as_str().unwrap().contains("shell"));
    assert_eq!(client.launch_count.load(Ordering::Acquire), 0);
    if let Some(record) = &connection {
        assert_eq!(record.host_id, fleet.owner.id);
        // Change only our own temporary app-local metadata, then ensure the
        // pinned peer cannot silently replace the stored authoritative HostID.
        client.disconnect(Some(&record.id));
        let path = fixture.store_root().join("connections.json");
        let original = std::fs::read(&path).unwrap();
        let mut changed: Value = serde_json::from_slice(&original).unwrap();
        changed["connections"][0]["hostId"] = json!("synthetic-different-owner");
        std::fs::write(&path, serde_json::to_vec(&changed).unwrap()).unwrap();
        stage("native-stage: requery");
        assert_eq!(
            client.fleet(id.clone()).err().unwrap().code,
            "host_identity_changed"
        );
        std::fs::write(&path, original).unwrap();
        stage("native-stage: requery");
        client.fleet(id.clone()).unwrap();
    }
    let previous = details
        .settings
        .iter()
        .find(|setting| setting.name == "SSH_PORT")
        .and_then(|setting| setting.value.clone());
    let arguments = vec![
        "set".into(),
        "SSH_PORT".into(),
        "2223".into(),
        "--scope".into(),
        "yard".into(),
        "--local".into(),
    ];
    stage("native-stage: plan");
    let discarded = client
        .plan(
            id.clone(),
            Some(yard.clone()),
            "config".into(),
            arguments.clone(),
        )
        .unwrap();
    stage("native-stage: discard");
    client.discard(&discarded.plan_id).unwrap();
    let execute_and_wait = |arguments: Vec<String>, cancel: bool| -> String {
        stage("native-stage: plan");
        let plan = client
            .plan(id.clone(), Some(yard.clone()), "config".into(), arguments)
            .unwrap();
        stage("native-stage: execute");
        let started = client.execute(&plan.plan_id, &plan.digest, true).unwrap();
        if cancel {
            stage("native-stage: cancel");
            let _ = client.cancel(&started.operation_id);
        }
        let deadline = Instant::now() + Duration::from_secs(30);
        loop {
            let event = events
                .recv_timeout(deadline.saturating_duration_since(Instant::now()))
                .unwrap();
            if event.operation_id.as_deref() == Some(&started.operation_id)
                && matches!(
                    event.state.as_str(),
                    "completed" | "cancelled" | "failed" | "unknown"
                )
            {
                return event.state;
            }
        }
    };
    // Cancellation of this quick operation may legitimately race completion;
    // the synthetic Manager regression proves deterministic cancel ordering.
    assert!(matches!(
        execute_and_wait(arguments.clone(), true).as_str(),
        "completed" | "cancelled"
    ));
    assert_eq!(execute_and_wait(arguments, false), "completed");
    stage("native-stage: requery");
    let updated = client.yard(id.clone(), yard.clone()).unwrap();
    assert_eq!(
        updated
            .settings
            .iter()
            .find(|setting| setting.name == "SSH_PORT")
            .unwrap()
            .value
            .as_deref(),
        Some("2223")
    );
    let restore = if let Some(value) = previous {
        vec!["set".into(), "SSH_PORT".into(), value]
    } else {
        vec!["unset".into(), "SSH_PORT".into()]
    };
    let mut restore = restore;
    restore.extend(["--scope".into(), "yard".into(), "--local".into()]);
    stage("native-stage: restore");
    assert_eq!(execute_and_wait(restore, false), "completed");
    #[cfg(target_os = "linux")]
    if std::env::var("VERANDA_TEST_TERMINAL").ok().as_deref() == Some("1") {
        physical_terminal_sessions(&client, id.clone(), &fleet);
    }
    #[cfg(target_os = "linux")]
    if id.is_some() && std::env::var("VERANDA_TEST_EDITOR").ok().as_deref() == Some("1") {
        physical_editor_session(&fleet);
    }
    if let Some(record) = connection {
        #[cfg(target_os = "linux")]
        if let Some(control_root) = std::env::var_os("VERANDA_TEST_NATIVE_CONTROL_ROOT") {
            client.shutdown();
            physical_ssh_recovery_and_churn(
                &fixture.store_root(),
                &record,
                &PathBuf::from(control_root),
            );
            return;
        }
        let removal = client.assess_removal(&record.id).unwrap();
        client.remove(&removal.assessment_id, true).unwrap();
        assert!(client.connections().unwrap().is_empty());
    }
}
