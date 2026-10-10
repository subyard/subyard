//! Synthetic owner subprocesses exercise the actual framed transport, without
//! personal SSH files, owner state, network access, or runtime dependencies.
use super::*;
use std::fs;
use std::io::Cursor;
use std::path::PathBuf;

const PRODUCT_VERSION: &str = "0.1.0-dev";
const FIXTURE_DIR: &str = concat!(
    env!("CARGO_MANIFEST_DIR"),
    "/../../api/yard-rpc/v1/fixtures"
);
const OWNER: &str = r#"
import datetime, json, os, struct, subprocess, sys, time
mode, pid_file, fixtures = sys.argv[1:]
with open(pid_file, 'w', encoding='ascii') as output:
    output.write(str(os.getpid()))

def read():
    prefix = sys.stdin.buffer.read(4)
    if not prefix:
        return None
    if len(prefix) != 4:
        raise RuntimeError('partial header')
    length = struct.unpack('>I', prefix)[0]
    if length > 1048576:
        raise RuntimeError('oversized request')
    body = sys.stdin.buffer.read(length)
    if len(body) != length:
        raise RuntimeError('partial body')
    return json.loads(body)

def send(value):
    body = json.dumps(value, separators=(',', ':'), sort_keys=True).encode('utf8')
    sys.stdout.buffer.write(struct.pack('>I', len(body)) + body)
    sys.stdout.buffer.flush()

def fixture(name):
    with open(os.path.join(fixtures, name + '.frame'), 'rb') as source:
        data = source.read()
    return json.loads(data[4:])

def reply(request, result=None, error=None):
    response = {'version':1, 'type':'response', 'id':request['id'],
                'operationId':request.get('operationId') or request['id']}
    if error is not None:
        response['error'] = {'code':error, 'message':'synthetic private owner text'}
    else:
        response['result'] = result
    send(response)

def event(sequence, name='snapshot.ready', operation='op-stream'):
    value = fixture('snapshot-ready')
    value.update(sequence=sequence, revision=sequence, event=name, operationId=operation)
    send(value)

negotiate = read()
assert negotiate['method'] == 'rpc.negotiate'
if mode == 'negotiation-blocked':
    while True:
        time.sleep(60)
result = fixture('negotiate')['result']
if mode == 'version-mismatch':
    result['engineVersion'] = '9.9.9'
elif mode == 'wire-mismatch':
    result['protocolMin'] = 2
    result['protocolMax'] = 2
elif mode == 'capability-missing':
    result['capabilities'].remove('owner-inventory-v1')
# Go negotiation deliberately omits operationId; ordinary method responses
# include the explicit operation binding or default to their request ID.
negotiated = fixture('negotiate')
negotiated['id'] = negotiate['id']
negotiated['result'] = result
send(negotiated)
pending = None
held_sequence = 0
held_requests = {}
plans = {}
execute_count = 0
discard_count = 0
burst_sequence = 0
while True:
    request = read()
    if request is None:
        if mode == 'unresponsive':
            while True:
                time.sleep(60)
        break
    operation = request.get('operationId') or request['id']
    if request['type'] == 'cancel':
        if operation in held_requests:
            reply(held_requests.pop(operation), error='cancelled')
            reply(request, {'cancelled':True})
            continue
        if pending is not None and operation == (pending.get('operationId') or pending['id']):
            if mode == 'cancel':
                reply(pending, error='cancelled')
            else:
                event(2, 'operation.cancelled', operation)
            reply(request, {'cancelled':True})
            if mode == 'timeout':
                reply(pending, {'late':True})
            pending = None
        else:
            reply(request, {'cancelled':True})
        continue
    method = request['method']
    if mode == 'exited-parent-inherited-pipes':
        subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(60)'],
                         stdin=sys.stdin, stdout=sys.stdout, stderr=sys.stderr)
        reply(request, {'descendantStarted':True})
        sys.exit(0)
    if mode.startswith('manager'):
        if method == 'operation.plan':
            exact = fixture('operation-exact')['result']
            exact['plan']['operationId'] = operation
            exact['plan']['command'] = request['params']['command']
            expiry = datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(seconds=300)
            exact['expiresAt'] = expiry.isoformat(timespec='seconds').replace('+00:00', 'Z')
            if mode == 'manager-expired':
                exact['expiresAt'] = '2000-01-01T00:00:00Z'
            elif mode == 'manager-bad-expiry':
                exact['expiresAt'] = 'not-a-date'
            elif mode == 'manager-long-expiry':
                exact['expiresAt'] = (expiry + datetime.timedelta(days=1)).isoformat(timespec='seconds')
            plans[operation] = exact
            if mode == 'manager-gap-plan':
                event(1)
                event(3)
            reply(request, exact)
            continue
        if method == 'operation.discard':
            discard_count += 1
            plans.pop(operation, None)
            reply(request, {'operationId':operation, 'discarded':True})
            continue
        if method == 'operation.execute':
            execute_count += 1
            if mode == 'manager-cancel':
                held_requests[operation] = request
                continue
            exact = plans.pop(operation)
            exact['plan']['confirmed'] = True
            reply(request, {'plan':exact['plan'], 'result':{'status':'ok'}})
            continue
        if method == 'synthetic.stats':
            reply(request, {'executes':execute_count, 'discards':discard_count, 'retained':len(plans)})
            continue
        if method == 'owner.inventory' and mode == 'manager-gap-snapshot':
            event(1)
            event(3)
    if mode == 'hold' and method == 'incus.events':
        held_sequence += 1
        held_requests[operation] = request
        event(held_sequence, 'subscription.accepted', operation)
        continue
    if method == 'synthetic.first' and mode == 'responses-reordered':
        pending = request
        event(1, 'request.accepted', operation)
        continue
    if method == 'synthetic.second' and mode == 'responses-reordered':
        reply(request, {'which':'second'})
        reply(pending, {'which':'first'})
        pending = None
        continue
    if method == 'synthetic.slow':
        pending = request
        event(1, 'operation.started', operation)
        continue
    if mode == 'malformed':
        send({'version':1, 'type':'response', 'id':request['id'], 'operationId':operation,
              'result':{}, 'error':{'code':'synthetic_failure'}})
        continue
    if mode == 'binding-mismatch':
        send({'version':1, 'type':'response', 'id':request['id'], 'operationId':'different-operation', 'result':{}})
        continue
    if mode == 'oversized':
        sys.stdout.buffer.write(struct.pack('>I', 1048577))
        sys.stdout.buffer.flush()
        time.sleep(60)
        continue
    if mode in ['ordered', 'gap', 'reordered', 'duplicate']:
        for sequence in {'ordered':[1,2], 'gap':[1,3], 'reordered':[1,2,1], 'duplicate':[1,1]}[mode]:
            event(sequence)
    if mode == 'burst':
        for _ in range(1000):
            burst_sequence += 1
            event(burst_sequence, 'incus.lifecycle')
    names = {'owner.inventory':'owner-inventory', 'context.get':'context',
             'profile.list':'profile-list', 'settings.list':'settings-list',
             'host.sync.status':'host-sync-status', 'operation.plan':'operation-exact'}
    reply(request, fixture(names[method])['result'] if method in names else {'pong':True})
"#;

pub(crate) struct OwnerFixture {
    root: PathBuf,
}
impl OwnerFixture {
    pub(crate) fn new() -> Self {
        let mut random = [0u8; 16];
        getrandom::fill(&mut random).unwrap();
        let id: String = random.iter().map(|byte| format!("{byte:02x}")).collect();
        let root = fs::canonicalize(std::env::temp_dir())
            .unwrap()
            .join(format!("veranda-transport-test-{id}"));
        #[cfg(unix)]
        {
            use std::os::unix::fs::{DirBuilderExt, PermissionsExt};
            fs::DirBuilder::new().mode(0o700).create(&root).unwrap();
            fs::set_permissions(&root, fs::Permissions::from_mode(0o700)).unwrap();
        }
        #[cfg(not(unix))]
        fs::create_dir(&root).unwrap();
        Self { root }
    }
    pub(crate) fn command(&self, mode: &str) -> Command {
        let mut command = Command::new(
            std::env::var_os("VERANDA_TEST_PYTHON").unwrap_or_else(|| "python3".into()),
        );
        command.args(["-u", "-c", OWNER, mode]);
        command.arg(self.root.join("child.pid")).arg(FIXTURE_DIR);
        command
    }
    pub(crate) fn store_root(&self) -> PathBuf {
        self.root.join("connections")
    }
    fn open(&self, mode: &str) -> (RpcClient, mpsc::Receiver<Event>) {
        let (sender, receiver) = mpsc::channel();
        let notify: Notify = Arc::new(move |event| {
            let _ = sender.send(event);
        });
        let client = RpcClient::open(self.command(mode), PRODUCT_VERSION, notify).unwrap();
        (client, receiver)
    }
    #[cfg(unix)]
    fn assert_child_reaped(&self) {
        let pid: i32 = fs::read_to_string(self.root.join("child.pid"))
            .unwrap()
            .parse()
            .unwrap();
        // The PID belongs only to our fixture process. ESRCH proves that an
        // unsuccessful open did not leave a live child or a zombie behind.
        assert_eq!(unsafe { libc::kill(pid, 0) }, -1);
        assert_eq!(
            std::io::Error::last_os_error().raw_os_error(),
            Some(libc::ESRCH)
        );
    }
}
impl Drop for OwnerFixture {
    fn drop(&mut self) {
        let _ = fs::remove_dir_all(&self.root);
    }
}
fn receive(receiver: &mpsc::Receiver<Event>) -> Event {
    receiver.recv_timeout(Duration::from_secs(2)).unwrap()
}
fn await_no_pending(client: &RpcClient) {
    let deadline = Instant::now() + Duration::from_secs(1);
    loop {
        if client.shared.pending.lock().unwrap().is_empty() {
            return;
        }
        assert!(
            Instant::now() < deadline,
            "response waiters were not released"
        );
        thread::sleep(Duration::from_millis(5));
    }
}

#[test]
fn shutdown_reaps_a_process_that_has_not_completed_negotiation() {
    let fixture = OwnerFixture::new();
    let processes = Arc::new(Processes::default());
    let registry = processes.clone();
    let command = fixture.command("negotiation-blocked");
    let started = Instant::now();
    let opening = thread::spawn(move || {
        RpcClient::open_tracked(command, PRODUCT_VERSION, Arc::new(|_| {}), Some(&registry))
    });
    let pid_file = fixture.root.join("child.pid");
    while !pid_file.exists() {
        assert!(started.elapsed() < Duration::from_secs(2));
        thread::sleep(Duration::from_millis(5));
    }
    processes.shutdown();
    assert!(opening.join().unwrap().is_err());
    assert!(started.elapsed() < Duration::from_secs(2));
    #[cfg(unix)]
    fixture.assert_child_reaped();
}
#[test]
fn event_bursts_keep_order_and_do_not_starve_the_method_response() {
    let fixture = OwnerFixture::new();
    let (client, notifications) = fixture.open("burst");
    assert_eq!(
        client
            .call("system.ping", "", json!({}), Duration::from_secs(2))
            .unwrap()["pong"],
        true
    );
    for expected in 1..=1000 {
        let event = receive(&notifications);
        assert_eq!(event.sequence, expected);
        assert_eq!(event.revision, expected);
        assert!(!event.stale);
    }
    client.close();
    #[cfg(unix)]
    fixture.assert_child_reaped();
}
#[test]
fn shared_golden_frames_decode_and_reencode_with_the_production_codec() {
    for name in [
        "negotiate",
        "owner-inventory",
        "context",
        "profile-list",
        "settings-list",
        "host-sync-status",
        "operation-exact",
        "snapshot-ready",
        "session-shell",
        "session-vscode",
    ] {
        let bytes = fs::read(format!("{FIXTURE_DIR}/{name}.frame")).unwrap();
        let expected: Value =
            serde_json::from_slice(&fs::read(format!("{FIXTURE_DIR}/{name}.json")).unwrap())
                .unwrap();
        let mut input = Cursor::new(&bytes);
        let decoded: Value = read_frame(&mut input).unwrap();
        assert_eq!(decoded, expected, "golden {name}");
        assert_eq!(input.position(), bytes.len() as u64);
        let typed: Frame = serde_json::from_value(decoded.clone()).unwrap();
        assert_eq!(typed.version, 1);
        let mut encoded = Vec::new();
        write_frame(&mut encoded, &decoded).unwrap();
        assert_eq!(encoded, bytes, "golden {name}");
    }
}

#[test]
fn subprocess_negotiation_and_queries_use_shared_golden_results_and_operation_binding() {
    let fixture = OwnerFixture::new();
    let (client, _) = fixture.open("golden");
    assert_eq!(client.engine_version, PRODUCT_VERSION);
    assert!(client
        .capabilities
        .iter()
        .any(|capability| capability == "operation-exact-plan-v1"));
    for (method, name) in [
        ("owner.inventory", "owner-inventory"),
        ("context.get", "context"),
        ("profile.list", "profile-list"),
        ("settings.list", "settings-list"),
        ("host.sync.status", "host-sync-status"),
        ("operation.plan", "operation-exact"),
    ] {
        let expected: Value =
            serde_json::from_slice(&fs::read(format!("{FIXTURE_DIR}/{name}.json")).unwrap())
                .unwrap();
        let result = client
            .call(method, "", json!({}), Duration::from_secs(2))
            .unwrap();
        assert_eq!(result, expected["result"]);
    }
    client.close();
    assert!(client.child.lock().unwrap().is_none());
}

#[test]
fn responses_arriving_out_of_order_are_correlated_by_request_and_operation_id() {
    let fixture = OwnerFixture::new();
    let (client, notifications) = fixture.open("responses-reordered");
    thread::scope(|scope| {
        let first = scope.spawn(|| {
            client.call(
                "synthetic.first",
                "op-first",
                json!({}),
                Duration::from_secs(2),
            )
        });
        assert_eq!(receive(&notifications).event, "request.accepted");
        let second = client
            .call(
                "synthetic.second",
                "op-second",
                json!({}),
                Duration::from_secs(2),
            )
            .unwrap();
        assert_eq!(second, json!({"which":"second"}));
        assert_eq!(first.join().unwrap().unwrap(), json!({"which":"first"}));
    });
    await_no_pending(&client);
    client.close();
}

#[test]
fn incompatible_negotiation_fails_closed_and_reaps_its_subprocess() {
    for (mode, code) in [
        ("version-mismatch", "incompatible_engine"),
        ("wire-mismatch", "invalid_response"),
        ("capability-missing", "capability_missing"),
    ] {
        let fixture = OwnerFixture::new();
        let error = RpcClient::open(fixture.command(mode), PRODUCT_VERSION, Arc::new(|_| {}))
            .err()
            .unwrap();
        assert_eq!(error.code, code, "{mode}");
        #[cfg(unix)]
        fixture.assert_child_reaped();
    }
}

#[test]
fn malformed_envelopes_and_wrong_operation_binding_close_the_transport() {
    for (mode, code) in [
        ("malformed", "disconnected"),
        ("binding-mismatch", "invalid_response"),
        ("oversized", "disconnected"),
    ] {
        let fixture = OwnerFixture::new();
        let (client, notifications) = fixture.open(mode);
        let error = client
            .call(
                "system.ping",
                "operation-test",
                json!({}),
                Duration::from_secs(2),
            )
            .unwrap_err();
        assert_eq!(error.code, code, "{mode}");
        let notification = receive(&notifications);
        assert_eq!(notification.event, "transport.disconnected");
        assert!(notification.stale);
        assert!(client.is_closed());
        client.close();
        assert!(client.child.lock().unwrap().is_none());
    }
}

#[test]
fn ordered_events_are_fresh_but_gaps_and_reordering_require_resync() {
    for (mode, expected) in [
        ("ordered", vec![(1, false), (2, false)]),
        ("gap", vec![(1, false), (3, true)]),
        ("reordered", vec![(1, false), (2, false), (1, true)]),
        ("duplicate", vec![(1, false), (1, true)]),
    ] {
        let fixture = OwnerFixture::new();
        let (client, notifications) = fixture.open(mode);
        client
            .call(
                "system.ping",
                "op-stream",
                json!({}),
                Duration::from_secs(2),
            )
            .unwrap();
        for (sequence, stale) in expected {
            let event = receive(&notifications);
            assert_eq!(
                (event.sequence, event.revision, event.stale),
                (sequence, sequence, stale),
                "{mode}"
            );
        }
        assert_eq!(client.take_stale(), mode != "ordered");
        assert!(!client.take_stale());
        client.close();
    }
}

#[test]
fn explicit_cancellation_targets_the_active_operation_and_returns_typed_final_result() {
    let fixture = OwnerFixture::new();
    let (client, notifications) = fixture.open("cancel");
    thread::scope(|scope| {
        let operation = scope.spawn(|| {
            client.call(
                "synthetic.slow",
                "op-cancel",
                json!({}),
                Duration::from_secs(2),
            )
        });
        let event = receive(&notifications);
        assert_eq!(
            (event.event.as_str(), event.operation_id.as_str()),
            ("operation.started", "op-cancel")
        );
        client.cancel("op-cancel").unwrap();
        let error = operation.join().unwrap().unwrap_err();
        assert_eq!(error.code, "cancelled");
        assert!(!error.message.contains("private owner text"));
    });
    await_no_pending(&client);
    assert!(!client.is_closed());
    client.close();
}

#[test]
fn timeout_cancels_the_operation_and_late_reply_cannot_complete_another_request() {
    let fixture = OwnerFixture::new();
    let (client, notifications) = fixture.open("timeout");
    let error = client
        .call(
            "synthetic.slow",
            "op-timeout",
            json!({}),
            Duration::from_millis(30),
        )
        .unwrap_err();
    assert_eq!(error.code, "rpc_timeout");
    assert_eq!(receive(&notifications).event, "operation.started");
    let cancelled = receive(&notifications);
    assert_eq!(
        (cancelled.event.as_str(), cancelled.operation_id.as_str()),
        ("operation.cancelled", "op-timeout")
    );
    await_no_pending(&client);
    let result = client
        .call("system.ping", "op-next", json!({}), Duration::from_secs(2))
        .unwrap();
    assert_eq!(result, json!({"pong":true}));
    client.close();
}

#[cfg(target_os = "linux")]
#[test]
fn suspend_clock_expiry_is_checked_after_a_bounded_receive_slice() {
    let (_sender, receiver) = mpsc::channel::<Value>();
    let mut readings = [0, 0, 12].into_iter();
    let started = Instant::now();
    assert_eq!(
        receive_with_deadline(&receiver, Duration::from_secs(5), || {
            Some(Duration::from_secs(readings.next().unwrap()))
        }),
        Err(mpsc::RecvTimeoutError::Timeout)
    );
    assert!(started.elapsed() < Duration::from_secs(1));
}

#[cfg(target_os = "linux")]
#[test]
fn a_queued_response_cannot_succeed_after_the_suspend_clock_deadline() {
    let (sender, receiver) = mpsc::channel();
    sender.send(json!({"late":true})).unwrap();
    let mut readings = [0, 4, 12].into_iter();
    assert_eq!(
        receive_with_deadline(&receiver, Duration::from_secs(5), || {
            Some(Duration::from_secs(readings.next().unwrap()))
        }),
        Err(mpsc::RecvTimeoutError::Timeout)
    );
}

#[cfg(target_os = "linux")]
#[test]
fn unavailable_or_invalid_suspend_clock_deadlines_fail_closed() {
    for readings in [
        vec![None],
        vec![Some(Duration::ZERO), None],
        vec![Some(Duration::ZERO), Some(Duration::ZERO), None],
        vec![Some(Duration::MAX)],
        vec![Some(Duration::from_secs(8)), Some(Duration::from_secs(7))],
        vec![
            Some(Duration::from_secs(8)),
            Some(Duration::from_secs(9)),
            Some(Duration::from_secs(8)),
        ],
    ] {
        let (sender, receiver) = mpsc::channel();
        sender.send(json!({"pong":true})).unwrap();
        let mut readings = readings.into_iter();
        assert_eq!(
            receive_with_deadline(&receiver, Duration::from_secs(5), || {
                readings.next().unwrap()
            }),
            Err(mpsc::RecvTimeoutError::Timeout)
        );
    }
}

#[test]
fn bounded_pending_capacity_still_allows_cancellation() {
    let fixture = OwnerFixture::new();
    let (client, notifications) = fixture.open("hold");
    for index in 0..MAX_PENDING {
        client.subscribe(&format!("held-{index}")).unwrap();
        // An event proves the fixture consumed this request, isolating pending
        // capacity from the separate bounded writer queue without timing sleeps.
        let accepted = receive(&notifications);
        assert_eq!(accepted.event, "subscription.accepted");
        assert_eq!(accepted.operation_id, format!("held-{index}"));
    }
    assert_eq!(
        client.subscribe("one-too-many").unwrap_err().code,
        "request_capacity"
    );
    client
        .cancel("held-0")
        .expect("cancellation must remain available at request capacity");
    let deadline = Instant::now() + Duration::from_secs(1);
    while client.shared.pending.lock().unwrap().len() != MAX_PENDING - 1 {
        assert!(
            Instant::now() < deadline,
            "cancelled subscription did not release its response slot"
        );
        thread::sleep(Duration::from_millis(5));
    }
    client.subscribe("replacement").unwrap();
    assert_eq!(receive(&notifications).operation_id, "replacement");
    assert_eq!(client.shared.pending.lock().unwrap().len(), MAX_PENDING);
    client.close();
}

#[test]
#[cfg(unix)]
fn close_remains_bounded_after_parent_exit_with_descendant_inherited_pipes() {
    let fixture = OwnerFixture::new();
    let (client, _) = fixture.open("exited-parent-inherited-pipes");
    client
        .call("synthetic.inherited", "", json!({}), Duration::from_secs(2))
        .unwrap();
    let pid = client.child.lock().unwrap().as_ref().unwrap().id() as i32;
    let deadline = Instant::now() + Duration::from_secs(2);
    while !child_exited(client.child.lock().unwrap().as_mut().unwrap()) {
        assert!(Instant::now() < deadline);
        thread::sleep(Duration::from_millis(5));
    }
    let client = Arc::new(client);
    let closing = client.clone();
    let (sender, complete) = mpsc::channel();
    let worker = thread::spawn(move || {
        closing.close();
        let _ = sender.send(());
    });
    let bounded = complete.recv_timeout(Duration::from_secs(2)).is_ok();
    // This PID/group came only from our fixture. Clean it even on regression,
    // otherwise a failing assertion would leave the inherited pipe alive.
    if !bounded {
        unsafe {
            libc::kill(-pid, libc::SIGKILL);
        }
    }
    worker.join().unwrap();
    assert!(
        bounded,
        "closing waited indefinitely for an exited parent's descendant"
    );
}
#[test]
fn close_kills_unresponsive_child_reaps_it_and_is_idempotent() {
    let fixture = OwnerFixture::new();
    let (client, notifications) = fixture.open("unresponsive");
    let started = Instant::now();
    client.close();
    assert!(started.elapsed() < Duration::from_secs(2));
    assert!(client.child.lock().unwrap().is_none());
    assert!(client.reader.lock().unwrap().is_none());
    assert!(client.writer.lock().unwrap().is_none());
    assert!(client.is_closed());
    assert_eq!(receive(&notifications).event, "transport.disconnected");
    client.close();
    #[cfg(unix)]
    fixture.assert_child_reaped();
}
