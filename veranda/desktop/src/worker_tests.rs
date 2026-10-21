//! A subprocess isolates PATH while exercising the actual worker, native RPC,
//! private atomic store, shutdown and reopen boundary. No GTK or live owner.
use super::*;
use crate::state::OperationNote;
use std::fs;
use std::os::unix::{fs::PermissionsExt, process::CommandExt};
use std::process::{Command, Stdio};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

const OWNER: &str = r#"#!/usr/bin/env python3
import datetime, json, os, struct, sys
root = os.environ['VERANDA_REOPEN_FIXTURE']
fixtures = os.environ['VERANDA_REOPEN_FRAMES']
observing = os.environ.get('VERANDA_WORKER_OBSERVE') == '1'
status_failure = os.environ.get('VERANDA_WORKER_STATUS_FAILURE') == '1'
profile_na = os.environ.get('VERANDA_WORKER_PROFILE_NA') == '1'
details_only = os.environ.get('VERANDA_WORKER_DETAILS_ONLY') == '1'
details_event_sent = False
owner_event_sent = False
inventory_reads = 0
request_ordinal = 0
def scalar(record):
    if details_only and request_ordinal <= 64:
        fd = os.open(os.path.join(root, 'owner-scalars'), os.O_WRONLY | os.O_APPEND | os.O_CREAT, 0o600)
        with os.fdopen(fd, 'w') as output:
            output.write(json.dumps(record) + '\n')
def fixture(name):
    with open(os.path.join(fixtures, name + '.frame'), 'rb') as source:
        return json.loads(source.read()[4:])['result']
def reply(request, result=None, error=None):
    value = {'version':1, 'type':'response', 'id':request['id'], 'result':result}
    if error is not None:
        del value['result']
        value['error'] = {'code':'synthetic_status_failure', 'message':'synthetic status unavailable'}
    if request['method'] != 'rpc.negotiate':
        value['operationId'] = request.get('operationId') or request['id']
    body = json.dumps(value).encode('utf8')
    sys.stdout.buffer.write(struct.pack('>I', len(body)) + body)
    sys.stdout.buffer.flush()
def event(sequence):
    with open(os.path.join(fixtures, 'snapshot-ready.frame'), 'rb') as source:
        value = json.loads(source.read()[4:])
    value.update(sequence=sequence, revision=sequence, event='incus.lifecycle', operationId='')
    body = json.dumps(value).encode('utf8')
    sys.stdout.buffer.write(struct.pack('>I', len(body)) + body)
    sys.stdout.buffer.flush()
while True:
    prefix = sys.stdin.buffer.read(4)
    if not prefix:
        break
    request = json.loads(sys.stdin.buffer.read(struct.unpack('>I', prefix)[0]))
    method = request['method']
    request_ordinal += 1
    scalar(dict(request=request_ordinal, yard_arg=('-Y' in sys.argv), profile=(method == 'profile.list'), inventory=(method == 'owner.inventory'), negotiate=(method == 'rpc.negotiate')))
    if status_failure and method == 'yard.status':
        reply(request, error=True)
        continue
    if method == 'owner.inventory':
        inventory_reads += 1
        if observing and not details_only and inventory_reads == 2:
            event(1)
            event(2)
    if details_only and '-Y' in sys.argv and method == 'profile.list' and not details_event_sent:
        details_event_sent = True
        event(1)
        scalar(dict(request=request_ordinal, emitted=True, yard_arg=True))
    if details_only and '-Y' not in sys.argv and method == 'context.get' and not owner_event_sent:
        owner_event_sent = True
        event(1)
        scalar(dict(request=request_ordinal, emitted=True, yard_arg=False))
    if method == 'operation.execute':
        fd = os.open(os.path.join(root, 'execute-observed'), os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, 'w') as output:
            output.write('1')
        os._exit(0)
    if method == 'operation.plan':
        result = fixture('operation-exact')
        result['plan']['operationId'] = request['operationId']
        result['plan']['command'] = request['params']['command']
        result['plan']['arguments'] = request['params']['arguments']
        result['expiresAt'] = (datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(seconds=60)).isoformat(timespec='seconds').replace('+00:00', 'Z')
    else:
        names = {'rpc.negotiate':'negotiate', 'owner.inventory':'owner-inventory', 'context.get':'context', 'profile.list':'profile-list', 'settings.list':'settings-list', 'host.sync.status':'host-sync-status', 'yard.status':'yard-status'}
        result = fixture(names[method]) if method in names else {'pong':True}
    if details_only and method == 'owner.inventory':
        result['yards'].append(dict(result['yards'][0], name='secondary'))
    if details_only and '-Y' in sys.argv and method in ('context.get', 'profile.list', 'settings.list'):
        result['yardName'] = sys.argv[sys.argv.index('-Y') + 1]
    if profile_na and method == 'rpc.negotiate':
        result['capabilities'].remove('profile-list-v1')
    reply(request, result)
"#;

#[test]
fn admitted_eof_stays_unknown_after_worker_reopen() {
    run_actor("worker::reopen_tests::reopen_actor", false);
}

#[test]
fn observed_worker_keeps_callback_hints_separate_from_authoritative_reads() {
    run_actor("worker::reopen_tests::observation_actor", true);
}

#[test]
fn view_observation_uses_actual_worker_proofs_and_compound_failures() {
    run_actor("worker::reopen_tests::view_observation_actor", true);
}

#[test]
fn view_observation_requires_fleet_membership_for_inventory_events() {
    run_actor("worker::reopen_tests::fleet_membership_actor", true);
}

fn run_actor(actor: &str, observing: bool) {
    let root = std::path::Path::new("/tmp").join(format!(
        "veranda-worker-reopen-{}-{}",
        std::process::id(),
        SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_nanos()
    ));
    fs::create_dir(&root).unwrap();
    fs::set_permissions(&root, fs::Permissions::from_mode(0o700)).unwrap();
    let script = root.join("yard");
    fs::write(&script, OWNER).unwrap();
    fs::set_permissions(&script, fs::Permissions::from_mode(0o700)).unwrap();
    let path = std::env::join_paths(
        std::iter::once(root.clone())
            .chain(std::env::split_paths(&std::env::var_os("PATH").unwrap())),
    )
    .unwrap();
    let output = fs::File::create(root.join("actor.log")).unwrap();
    output
        .set_permissions(fs::Permissions::from_mode(0o600))
        .unwrap();
    let mut child = Command::new(std::env::current_exe().unwrap())
        .args(["--exact", actor, "--ignored", "--nocapture"])
        .process_group(0)
        .env("PATH", path)
        .env("VERANDA_REOPEN_FIXTURE", &root)
        .env("VERANDA_WORKER_OBSERVE", if observing { "1" } else { "0" })
        .env(
            "VERANDA_WORKER_DETAILS_ONLY",
            if actor == "worker::reopen_tests::fleet_membership_actor" {
                "1"
            } else {
                "0"
            },
        )
        .env(
            "VERANDA_WORKER_STATUS_FAILURE",
            if actor == "worker::reopen_tests::view_observation_actor" {
                "1"
            } else {
                "0"
            },
        )
        .env(
            "VERANDA_REOPEN_FRAMES",
            concat!(
                env!("CARGO_MANIFEST_DIR"),
                "/../../api/yard-rpc/v1/fixtures"
            ),
        )
        .stdout(Stdio::from(output.try_clone().unwrap()))
        .stderr(Stdio::from(output))
        .spawn()
        .unwrap();
    let deadline = Instant::now() + Duration::from_secs(15);
    let status = loop {
        if let Some(status) = child.try_wait().unwrap() {
            break status;
        }
        if Instant::now() >= deadline {
            // The isolated actor and every owned engine share this fresh group.
            let _ = Command::new("/bin/kill")
                .args(["-KILL", "--", &format!("-{}", child.id())])
                .status();
            let _ = child.kill();
            let _ = child.wait();
            panic!(
                "worker reopen fixture exceeded its bounded deadline; {}\nscalar fixture journal: {}",
                fs::read_to_string(root.join("actor.log")).unwrap(),
                fs::read_to_string(root.join("owner-scalars")).unwrap_or_default()
            );
        }
        std::thread::sleep(Duration::from_millis(10));
    };
    assert!(
        status.success(),
        "{}",
        fs::read_to_string(root.join("actor.log")).unwrap()
    );
    if !observing {
        assert_eq!(
            fs::read_to_string(root.join("execute-observed")).unwrap(),
            "1"
        );
    }
    fs::remove_dir_all(root).unwrap();
}

// Real selected-yard and owner sessions share an owner but have distinct RPC
// sources. A later owner read cannot borrow a Details-only source's frontier.
#[test]
#[ignore]
fn fleet_membership_actor() {
    use crate::{
        model::{Model, ReadAction},
        view_observation::{Parts, ViewObservation},
    };
    use veranda_lib::observation::{Retirement, TerminalOutcome};
    let root = PathBuf::from(std::env::var_os("VERANDA_REOPEN_FIXTURE").unwrap());
    let observer = Observation::new();
    let (mut worker, updates) = Worker::observed(root.join("private"), observer.clone());
    println!("fleet_membership_stage=initialized-wait");
    let local = loop {
        if let Update::Initialized { local, .. } = updates.recv_blocking().unwrap() {
            break local.unwrap();
        }
    };
    let mut model = Model::new(vec![]);
    model.update(&None, Ok(local));
    model.initialize(None);
    model.select(None, Some("secondary".into()));
    model.select_tab("Projects".into());
    let scope = Scope {
        connection: None,
        yard: Some("secondary".into()),
    };
    let mut view = ViewObservation::new(observer.clone());
    let gui = view.request(&model, false);
    let gui_original = gui.tickets()[0].clone();

    // First profile query emits one real validated frame after admission.
    // The second actual Details call is admitted after that receipt.
    worker.details(model.generation, scope.clone());
    println!("fleet_membership_stage=first-details-wait");
    loop {
        if let Update::Details {
            observation,
            result,
            ..
        } = updates.recv_blocking().unwrap()
        {
            result.unwrap();
            view.deferred(observation);
            break;
        }
    }
    println!(
        "fleet_membership_stage=hint-wait received={} pending={}",
        observer.counts().received,
        observer.counts().pending
    );
    loop {
        if worker
            .events
            .lock()
            .unwrap()
            .carry(&None, &scope.yard)
            .is_some_and(|hint| hint.batch.tickets().len() == 1)
        {
            break;
        }
        updates.recv_blocking().unwrap();
    }
    worker.details(model.generation, scope.clone());
    println!("fleet_membership_stage=second-details-wait");
    let (details_ticket, details_admitted) = loop {
        if let Update::Details {
            generation,
            scope,
            invalidation,
            result,
            observation,
        } = updates.recv_blocking().unwrap()
        {
            let value = result.unwrap();
            let metadata = observation.unwrap();
            let proof = metadata.proof.as_ref().unwrap();
            assert_eq!(proof.batch().tickets().len(), 1);
            let original = proof.batch().tickets()[0].clone();
            let admitted = proof.started();
            assert_eq!(original.source(), proof.source());
            view.details(
                &model,
                generation,
                &scope,
                invalidation,
                Parts::details(&value),
                Some(metadata),
            );
            break (original, admitted);
        }
    };
    assert!(worker.read(model.generation, None, scope.yard.clone(), true));
    println!("fleet_membership_stage=fleet-wait");
    let (fleet_source, fleet_started, fleet_contains_details, fleet_original) = loop {
        if let Update::Fleet {
            generation,
            connection,
            invalidation,
            result,
            observation,
        } = updates.recv_blocking().unwrap()
        {
            let current = worker.read_is_current(&connection, invalidation);
            assert!(current);
            let action = model.read_complete(generation, &connection, current, result);
            assert_eq!(action, ReadAction::Applied);
            let metadata = observation.unwrap();
            let proof = metadata.proof.as_ref().unwrap();
            let contains = proof
                .batch()
                .tickets()
                .iter()
                .any(|ticket| ticket.ordinal() == details_ticket.ordinal());
            let source = proof.source();
            let started = proof.started();
            assert_eq!(proof.batch().tickets().len(), 1);
            let original = proof.batch().tickets()[0].clone();
            view.fleet(
                &model,
                generation,
                &connection,
                invalidation,
                current,
                true,
                &action,
                Some(metadata),
            );
            break (source, started, contains, original);
        }
    };
    let applied = view.rendered(&model, true).unwrap();
    let represented_event = applied
        .batch
        .tickets()
        .iter()
        .any(|ticket| ticket.ordinal() == details_ticket.ordinal());
    let represented_gui = applied
        .batch
        .tickets()
        .iter()
        .find(|ticket| ticket.ordinal() == gui_original.ordinal())
        .unwrap();
    assert_eq!(represented_gui.started(), gui_original.started());
    let represented_fleet = applied
        .batch
        .tickets()
        .iter()
        .find(|ticket| ticket.ordinal() == fleet_original.ordinal())
        .unwrap();
    assert_eq!(represented_fleet.started(), fleet_original.started());
    assert_eq!(fleet_original.source(), fleet_source);
    assert!(details_admitted >= fleet_original.started());
    println!(
        "fleet_membership_check details_source={} fleet_source={} details_in_fleet={} later_fleet={} represented_event={} represented_count={}",
        details_ticket.source(),
        fleet_source,
        fleet_contains_details,
        fleet_started >= details_ticket.started(),
        represented_event,
        applied.batch.tickets().len()
    );
    let represented_count = applied.batch.tickets().len();
    view.close();
    worker.close();
    while !matches!(updates.recv_blocking().unwrap(), Update::Closed) {}
    assert_eq!(observer.counts().pending, 0);
    assert!(
        observer
            .terminals()
            .iter()
            .all(|row| !matches!(row.outcome, TerminalOutcome::Paint { .. }))
    );
    assert_ne!(fleet_source, details_ticket.source());
    assert!(fleet_started >= details_ticket.started());
    assert!(!fleet_contains_details);
    assert!(
        !represented_event,
        "Details-only foreign-source receipt must not receive Fleet coverage from a later admission"
    );
    assert_eq!(
        represented_count, 2,
        "Fleet receipt and explicit GUI request remain eligible with required Details"
    );
    let fleet_terminal = observer
        .terminals()
        .into_iter()
        .find(|row| row.ticket == fleet_original.ordinal())
        .unwrap();
    let coverage = fleet_terminal.coverage.unwrap();
    assert!(coverage.whole && !coverage.cached);
    assert_eq!(
        coverage.parts,
        crate::view_observation::FLEET | crate::view_observation::SESSIONS
    );
    let event_terminal = observer
        .terminals()
        .into_iter()
        .find(|row| row.ticket == details_ticket.ordinal())
        .unwrap();
    assert_eq!(
        event_terminal.outcome,
        TerminalOutcome::Retired(Retirement::Unknown)
    );
    assert!(event_terminal.coverage.is_none());
}

// Invoked only by the bounded parent with a disposable PATH and private root.
#[test]
#[ignore]
fn reopen_actor() {
    let root = PathBuf::from(std::env::var_os("VERANDA_REOPEN_FIXTURE").unwrap());
    let (mut worker, updates) = Worker::start(root.join("private"));
    let mut state = loop {
        if let Update::Initialized { state, local, .. } = updates.recv_blocking().unwrap() {
            assert!(local.is_ok(), "fixture negotiation/inventory failed");
            break state.unwrap();
        }
    };
    // A definite native rejection must release the persisted intent, even after
    // more than the 32-slot history bound, using the same UI save transition.
    let scope = Scope {
        connection: None,
        yard: None,
    };
    for index in 0..40 {
        assert!(worker.owner(OwnerCommand::Plan {
            request: index,
            scope: scope.clone(),
            command: "config".into(),
            arguments: vec!["set".into(), "SSH_PORT".into(), "2223".into()],
        }));
        let mut plan = loop {
            if let Update::Planned { result, .. } = updates.recv_blocking().unwrap() {
                break result.unwrap();
            }
        };
        let id = plan.operation_id.clone();
        plan.digest = "b".repeat(64);
        state
            .intent(OperationNote {
                scope: scope.clone(),
                owner_id: "example-owner".into(),
                operation_id: id.clone(),
                command: "config".into(),
                outcome: "unknown".into(),
            })
            .unwrap();
        assert!(worker.owner(OwnerCommand::Execute {
            request: index,
            plan,
            state: state.clone()
        }));
        loop {
            if let Update::Executed { result, .. } = updates.recv_blocking().unwrap() {
                let proof = result.err().expect("wrong digest cannot start execute");
                state.not_submitted(&id, &proof);
                break;
            }
        }
        worker.save_state(1, state.clone());
        loop {
            if let Update::StateSaved {
                revision: 1,
                result,
            } = updates.recv_blocking().unwrap()
            {
                result.unwrap();
                break;
            }
        }
    }
    assert!(
        !root.join("execute-observed").exists(),
        "definite rejections executed an owner request"
    );
    worker.close();
    while !matches!(updates.recv_blocking().unwrap(), Update::Closed) {}
    let (next, next_updates) = Worker::start(root.join("private"));
    worker = next;
    let updates = next_updates;
    state = loop {
        if let Update::Initialized { state, .. } = updates.recv_blocking().unwrap() {
            let state = state.unwrap();
            assert!(
                state.operations.is_empty(),
                "definite rejection consumed unknown history after reopen"
            );
            break state;
        }
    };
    assert!(worker.owner(OwnerCommand::Plan {
        request: 1,
        scope: scope.clone(),
        command: "config".into(),
        arguments: vec!["set".into(), "SSH_PORT".into(), "2223".into()]
    }));
    let plan = loop {
        if let Update::Planned { result, .. } = updates.recv_blocking().unwrap() {
            break result.unwrap();
        }
    };
    let operation_id = plan.operation_id.clone();
    state
        .intent(OperationNote {
            scope,
            owner_id: "example-owner".into(),
            operation_id: operation_id.clone(),
            command: "config".into(),
            outcome: "unknown".into(),
        })
        .unwrap();
    assert!(worker.owner(OwnerCommand::Execute {
        request: 2,
        plan,
        state: state.clone()
    }));
    let mut started = false;
    let mut unknown = false;
    while !started || !unknown {
        if let Update::Executed { result, .. } = updates.recv_blocking().unwrap() {
            assert_eq!(result.map_err(|failure| failure.0).unwrap(), operation_id);
            started = true;
        }
        for event in worker.take_operations() {
            if event.operation_id.as_deref() == Some(operation_id.as_str()) {
                assert_eq!(event.state, "unknown");
                state.outcome(&operation_id, &event.state);
                unknown = true;
            }
        }
    }
    assert_eq!(
        fs::read_to_string(root.join("execute-observed")).unwrap(),
        "1"
    );
    worker.save_state(1, state);
    loop {
        if let Update::StateSaved {
            revision: 1,
            result,
        } = updates.recv_blocking().unwrap()
        {
            result.unwrap();
            break;
        }
    }
    worker.close();
    while !matches!(updates.recv_blocking().unwrap(), Update::Closed) {}
    let (mut reopened, updates) = Worker::start(root.join("private"));
    loop {
        if let Update::Initialized { state, .. } = updates.recv_blocking().unwrap() {
            let state = state.unwrap();
            assert_eq!(state.operations.len(), 1);
            assert_eq!(state.operations[0].operation_id, operation_id);
            assert_eq!(state.operations[0].outcome, "unknown");
            break;
        }
    }
    reopened.close();
    while !matches!(updates.recv_blocking().unwrap(), Update::Closed) {}
}

// The parent runs this actor once with the existing framed OWNER and a private
// /tmp root. Two real receipts arrive during the first authoritative read.
#[test]
#[ignore]
fn observation_actor() {
    use veranda_lib::observation::{Effect, ReadKind};
    let root = PathBuf::from(std::env::var_os("VERANDA_REOPEN_FIXTURE").unwrap());
    let observer = Observation::new();
    let (mut worker, updates) = Worker::observed(root.join("private"), observer.clone());
    loop {
        if let Update::Initialized { local, .. } = updates.recv_blocking().unwrap() {
            assert!(local.is_ok());
            break;
        }
    }
    assert!(worker.read(11, None, None, true));
    let first_invalidation = loop {
        if let Update::Fleet {
            generation,
            invalidation,
            result,
            observation,
            ..
        } = updates.recv_blocking().unwrap()
        {
            assert_eq!(generation, 11);
            result.unwrap();
            let metadata = observation.unwrap();
            let proof = metadata.proof.unwrap();
            assert_eq!(proof.kind(), ReadKind::Rpc);
            assert!(
                proof.batch().tickets().is_empty(),
                "events during read must not enter its admission frontier"
            );
            break invalidation;
        }
    };
    let hints = loop {
        let metadata = worker.events.lock().unwrap().carry(&None, &None).unwrap();
        if metadata.batch.tickets().len() == 2 {
            break metadata;
        }
        updates.recv_blocking().unwrap();
    };
    assert!(
        hints.proof.is_none(),
        "monitor snapshots are hints, never Worker read proof"
    );
    assert!(!worker.read_is_current(&None, first_invalidation));
    assert_eq!(observer.counts().received, 2);
    assert!(
        hints
            .batch
            .tickets()
            .iter()
            .all(|ticket| ticket.effect() == Some(Effect::Inventory))
    );
    let same_originals = |batch: &Batch| {
        assert_eq!(batch.tickets().len(), 2);
        for (actual, original) in batch.tickets().iter().zip(hints.batch.tickets()) {
            assert_eq!(actual.ordinal(), original.ordinal());
            assert_eq!(actual.source(), original.source());
            assert_eq!(actual.started(), original.started());
        }
    };
    assert!(worker.read(11, None, None, false));
    let scope = Scope {
        connection: None,
        yard: None,
    };
    worker.details(11, scope.clone());
    let mut fleet = false;
    let mut details = false;
    while !fleet || !details {
        let metadata = match updates.recv_blocking().unwrap() {
            Update::Fleet {
                generation,
                result,
                observation,
                ..
            } => {
                assert_eq!(generation, 11);
                result.unwrap();
                fleet = true;
                observation.unwrap()
            }
            Update::Details {
                generation,
                scope: actual,
                result,
                observation,
                ..
            } => {
                assert_eq!(generation, 11);
                assert_eq!(actual, scope);
                result.unwrap();
                details = true;
                observation.unwrap()
            }
            _ => continue,
        };
        assert_eq!(metadata.scope, scope);
        assert_eq!(metadata.hint_scope, Some(scope.clone()));
        same_originals(&metadata.batch);
        let proof = metadata.proof.unwrap();
        assert_eq!(proof.kind(), ReadKind::Rpc);
        same_originals(proof.batch());
    }
    // An owner-wide callback is not silently lent to the selected yard scope.
    let yard = Scope {
        connection: None,
        yard: Some("default".into()),
    };
    worker.details(12, yard.clone());
    loop {
        if let Update::Details {
            scope,
            result,
            observation,
            ..
        } = updates.recv_blocking().unwrap()
        {
            assert_eq!(scope, yard);
            result.unwrap();
            let metadata = observation.unwrap();
            assert_eq!(metadata.scope, yard);
            assert!(metadata.batch.tickets().is_empty());
            assert!(metadata.hint_scope.is_none());
            assert!(metadata.proof.is_some());
            break;
        }
    }
    assert!(worker.read(13, Some("missing".into()), None, true));
    loop {
        if let Update::Fleet {
            generation: 13,
            result,
            observation,
            ..
        } = updates.recv_blocking().unwrap()
        {
            assert!(result.is_err());
            let metadata = observation.unwrap();
            assert_eq!(metadata.scope.connection.as_deref(), Some("missing"));
            assert!(metadata.proof.is_none());
            break;
        }
    }
    assert!(!worker.read(13, Some("missing".into()), None, false));
    worker.close();
    while !matches!(updates.recv_blocking().unwrap(), Update::Closed) {}
    assert_eq!(
        observer.counts().pending,
        0,
        "close retires unmatched original hints as unknown"
    );
}

#[test]
#[ignore]
fn view_observation_actor() {
    use crate::{
        model::{Model, ReadAction},
        view_observation::{DIAGNOSTICS, FLEET, Parts, ViewObservation},
    };
    use veranda_lib::observation::{Origin, Retirement, StartKind, TerminalOutcome};
    let root = PathBuf::from(std::env::var_os("VERANDA_REOPEN_FIXTURE").unwrap());
    let observer = Observation::new();
    let (mut worker, updates) = Worker::observed(root.join("private"), observer.clone());
    let local = loop {
        if let Update::Initialized { local, .. } = updates.recv_blocking().unwrap() {
            break local.unwrap();
        }
    };
    #[cfg(target_os = "linux")]
    let typed_owner = local.owner.id.clone();
    #[cfg(target_os = "linux")]
    let expected = || {
        vec![crate::event_correlation::ExpectedCohort {
            ordinal: 1,
            owner: typed_owner.clone(),
            connection: None,
            yard: Some("default".into()),
        }]
    };
    #[cfg(target_os = "linux")]
    let mut cohorts = crate::event_correlation::Cohorts::new(observer.clone(), expected()).unwrap();
    #[cfg(target_os = "linux")]
    let mut foreign =
        crate::event_correlation::Cohorts::new(Observation::new(), expected()).unwrap();
    let mut model = Model::new(vec![]);
    model.update(&None, Ok(local));
    model.initialize(None);
    let mut view = ViewObservation::new(observer.clone());
    // A genuinely loaded cached body qualifies without a new RPC or Details.
    let selected = view.selection_start(
        &model,
        Scope {
            connection: None,
            yard: Some("default".into()),
        },
    );
    let selected_start = selected.tickets()[0].started();
    model.select(None, Some("default".into()));
    view.selected(&model, selected);
    let cached = view.rendered(&model, false).unwrap();
    assert_eq!(cached.batch.tickets()[0].started(), selected_start);
    observer.retire(&cached.batch, Retirement::Unknown); // Headless test NEVER simulates paint.
    let cached_row = observer.terminals().last().unwrap().clone();
    assert_eq!(cached_row.origin, Origin::Gui);
    assert_eq!(cached_row.source, 0);
    assert!(cached_row.source_kind.is_none());
    assert!(cached_row.coverage.unwrap().cached && !cached_row.coverage.unwrap().whole);
    assert_eq!(cached_row.kind, StartKind::LoadedSelection);
    let start = view.request(&model, false);
    let original = start.tickets()[0].clone();
    assert!(worker.read(model.generation, None, Some("default".into()), true));
    let first = loop {
        if let Update::Fleet {
            generation,
            connection,
            invalidation,
            result,
            observation,
        } = updates.recv_blocking().unwrap()
        {
            break (generation, connection, invalidation, result, observation);
        }
    };
    loop {
        if worker
            .events
            .lock()
            .unwrap()
            .carry(&None, &None)
            .unwrap()
            .batch
            .tickets()
            .len()
            == 2
        {
            break;
        }
        updates.recv_blocking().unwrap();
    }
    let current = worker.read_is_current(&first.1, first.2);
    assert!(!current);
    #[cfg(target_os = "linux")]
    let first_owner = first.3.as_ref().ok().map(|v| v.owner.id.clone());
    let success = first.3.is_ok();
    let action = model.read_complete(first.0, &first.1, current, first.3);
    assert_eq!(action, ReadAction::Retry);
    #[cfg(target_os = "linux")]
    {
        cohorts.fleet(
            &model,
            first.0,
            &first.1,
            current,
            &action,
            first_owner.as_deref(),
            first.4.as_ref(),
        );
        assert!(
            cohorts.bindings().is_empty(),
            "stale actual Fleet cannot qualify"
        );
    }
    view.fleet(
        &model, first.0, &first.1, first.2, current, success, &action, first.4,
    );
    assert!(
        view.request(&model, true).tickets().is_empty(),
        "retry keeps original admission start"
    );
    assert!(worker.read(model.generation, None, Some("default".into()), false));
    let next = loop {
        if let Update::Fleet {
            generation,
            connection,
            invalidation,
            result,
            observation,
        } = updates.recv_blocking().unwrap()
        {
            break (generation, connection, invalidation, result, observation);
        }
    };
    let current = worker.read_is_current(&next.1, next.2);
    assert!(current);
    #[cfg(target_os = "linux")]
    let returned_owner = next.3.as_ref().ok().map(|v| v.owner.id.clone());
    let action = model.read_complete(next.0, &next.1, current, next.3);
    #[cfg(target_os = "linux")]
    {
        cohorts.fleet(
            &model,
            next.0,
            &next.1,
            current,
            &action,
            Some("wrong-owner"),
            next.4.as_ref(),
        );
        assert!(cohorts.bindings().is_empty());
        foreign.fleet(
            &model,
            next.0,
            &next.1,
            current,
            &action,
            returned_owner.as_deref(),
            next.4.as_ref(),
        );
        assert!(
            foreign.bindings().is_empty(),
            "colliding foreign context rejected"
        );
        cohorts.fleet(
            &model,
            next.0,
            &next.1,
            current,
            &action,
            returned_owner.as_deref(),
            next.4.as_ref(),
        );
        assert_eq!(cohorts.bindings().len(), 1);
        assert!(!cohorts.complete());
    }
    view.fleet(
        &model, next.0, &next.1, next.2, current, true, &action, next.4,
    );
    let origin = view.fleet_provenance().unwrap();
    assert_eq!(origin.requested.yard.as_deref(), Some("default"));
    assert_eq!(origin.actual.yard.as_deref(), Some("default"));
    assert!(origin.hint_scope.is_none() && origin.hints.tickets().is_empty());
    assert_eq!(
        origin.proof.batch().tickets().len(),
        2,
        "actual owner proof covers selected-yard Fleet despite unmatched raw scope hint"
    );
    let applied = view.rendered(&model, false).unwrap();
    assert_eq!(
        applied.batch.tickets().len(),
        3,
        "2 actual receipts plus original GUI request"
    );
    assert_eq!(
        applied
            .batch
            .tickets()
            .iter()
            .find(|ticket| ticket.ordinal() == original.ordinal())
            .unwrap()
            .started(),
        original.started()
    );
    // Later invalidation/status-only rebuild still contains accepted A's body.
    model.invalidate(&None, false);
    let later = view.rendered(&model, false).unwrap();
    assert_eq!(
        later.batch.tickets().len(),
        3,
        "no always-latest-at-paint starvation"
    );
    assert!(view.snapshot_ready());
    observer.retire(&later.batch, Retirement::Unknown);
    assert!(
        observer
            .terminals()
            .iter()
            .filter_map(|row| row.coverage)
            .filter(|coverage| !coverage.cached)
            .all(|coverage| coverage.whole && coverage.parts == FLEET)
    );

    // Two ordinary admissions coalesce, while a later start cannot borrow an
    // earlier Fleet frontier even if Details completes after that later start.
    model.select(None, Some("default".into()));
    model.select_tab("Projects".into());
    view.changed(&model);
    let a = view.request(&model, false);
    let b = view.request(&model, false);
    assert!(worker.read(model.generation, None, Some("default".into()), true));
    let read = loop {
        if let Update::Fleet {
            generation,
            connection,
            invalidation,
            result,
            observation,
        } = updates.recv_blocking().unwrap()
        {
            break (generation, connection, invalidation, result, observation);
        }
    };
    let current = worker.read_is_current(&read.1, read.2);
    assert!(current);
    let action = model.read_complete(read.0, &read.1, current, read.3);
    view.fleet(
        &model, read.0, &read.1, read.2, current, true, &action, read.4,
    );
    let late = view.request(&model, false);
    assert!(
        view.rendered(&model, false).is_none(),
        "Projects needs actual rendered session controls"
    );
    let scope = Scope {
        connection: None,
        yard: Some("default".into()),
    };
    worker.details(model.generation, scope.clone());
    loop {
        if let Update::Details {
            generation,
            scope,
            invalidation,
            result,
            observation,
        } = updates.recv_blocking().unwrap()
        {
            let details = result.unwrap();
            #[cfg(target_os = "linux")]
            {
                let parts = Parts::details(&details);
                cohorts.details(
                    &model,
                    generation,
                    &scope,
                    false,
                    true,
                    observation.as_ref(),
                );
                cohorts.details(
                    &model,
                    generation,
                    &scope,
                    true,
                    false,
                    observation.as_ref(),
                );
                assert_eq!(
                    cohorts.bindings().len(),
                    1,
                    "stale or failed parts cannot bind"
                );
                cohorts.details(
                    &model,
                    generation,
                    &scope,
                    worker.read_is_current(&scope.connection, invalidation),
                    parts.failed == 0,
                    observation.as_ref(),
                );
                assert_ne!(
                    parts.failed & DIAGNOSTICS,
                    0,
                    "existing actual status failure must remain failed"
                );
                assert!(!cohorts.complete());
                assert_eq!(
                    cohorts.bindings().len(),
                    1,
                    "fallback Details cannot qualify required parts"
                );
            }
            view.details(
                &model,
                generation,
                &scope,
                invalidation,
                Parts::details(&details),
                observation,
            );
            break;
        }
    }
    let joined = view.rendered(&model, true).unwrap();
    assert_eq!(joined.batch.tickets().len(), 2);
    for original in [a.tickets()[0].clone(), b.tickets()[0].clone()] {
        assert_eq!(
            joined
                .batch
                .tickets()
                .iter()
                .find(|ticket| ticket.ordinal() == original.ordinal())
                .unwrap()
                .started(),
            original.started()
        );
    }
    assert!(
        !joined
            .batch
            .tickets()
            .iter()
            .any(|ticket| ticket.ordinal() == late.tickets()[0].ordinal())
    );
    observer.retire(&joined.batch, Retirement::Unknown);
    assert!(worker.read(model.generation, None, Some("default".into()), true));
    let read = loop {
        if let Update::Fleet {
            generation,
            connection,
            invalidation,
            result,
            observation,
        } = updates.recv_blocking().unwrap()
        {
            break (generation, connection, invalidation, result, observation);
        }
    };
    let current = worker.read_is_current(&read.1, read.2);
    assert!(current);
    let action = model.read_complete(read.0, &read.1, current, read.3);
    view.fleet(
        &model, read.0, &read.1, read.2, current, true, &action, read.4,
    );
    let final_late = view.rendered(&model, true).unwrap();
    assert_eq!(final_late.batch.tickets().len(), 1);
    assert_eq!(
        final_late.batch.tickets()[0].ordinal(),
        late.tickets()[0].ordinal()
    );
    observer.retire(&final_late.batch, Retirement::Unknown);

    // A successful Fleet normalization binds the actual fallback scope without
    // resetting the original request or pretending old requested scope survived.
    model.select_tab("Overview".into());
    model.selection.yard_name = Some("gone".into());
    view.changed(&model);
    let normalized_start = view.request(&model, false);
    assert!(worker.read(model.generation, None, Some("gone".into()), true));
    let read = loop {
        if let Update::Fleet {
            generation,
            connection,
            invalidation,
            result,
            observation,
        } = updates.recv_blocking().unwrap()
        {
            break (generation, connection, invalidation, result, observation);
        }
    };
    let current = worker.read_is_current(&read.1, read.2);
    assert!(current);
    let action = model.read_complete(read.0, &read.1, current, read.3);
    view.fleet(
        &model, read.0, &read.1, read.2, current, true, &action, read.4,
    );
    let provenance = view.fleet_provenance().unwrap();
    assert_eq!(provenance.requested.yard.as_deref(), Some("gone"));
    assert_eq!(provenance.actual.yard.as_deref(), Some("default"));
    let normalized = view.rendered(&model, false).unwrap();
    assert_eq!(
        normalized.batch.tickets()[0].started(),
        normalized_start.tickets()[0].started()
    );
    observer.retire(&normalized.batch, Retirement::Unknown);

    // Wrong measurement scope and a genuinely ignored generation cannot lend
    // a newer actual proof to the current request, even though native I/O worked.
    let guarded = view.request(&model, false);
    assert!(worker.read(model.generation, None, Some("default".into()), true));
    let read = loop {
        if let Update::Fleet {
            generation,
            connection,
            invalidation,
            result,
            observation,
        } = updates.recv_blocking().unwrap()
        {
            break (generation, connection, invalidation, result, observation);
        }
    };
    let current = worker.read_is_current(&read.1, read.2);
    assert!(current);
    let action = model.read_complete(read.0, &read.1, current, read.3);
    let mut wrong = read.4.unwrap();
    wrong.scope.connection = Some("synthetic-other-owner".into());
    view.fleet(
        &model,
        read.0,
        &read.1,
        read.2,
        current,
        true,
        &action,
        Some(wrong),
    );
    assert!(
        view.rendered(&model, false).is_none(),
        "wrong requested owner cannot borrow the proof"
    );
    assert!(worker.read(model.generation, None, Some("default".into()), true));
    let read = loop {
        if let Update::Fleet {
            generation,
            connection,
            invalidation,
            result,
            observation,
        } = updates.recv_blocking().unwrap()
        {
            break (generation, connection, invalidation, result, observation);
        }
    };
    let action = model.read_complete(read.0 + 1, &read.1, true, read.3);
    assert_eq!(action, ReadAction::Ignored);
    view.fleet(
        &model,
        read.0 + 1,
        &read.1,
        read.2,
        true,
        true,
        &action,
        read.4,
    );
    assert!(
        view.rendered(&model, false).is_none(),
        "ignored generation cannot become accepted content"
    );
    view.close();
    assert!(
        observer
            .terminals()
            .iter()
            .any(|row| row.ticket == guarded.tickets()[0].ordinal()
                && row.outcome == TerminalOutcome::Retired(Retirement::Unknown))
    );

    // Real typed YardDetails stays Ok on yard.status failure; diagnostics is
    // failed measurement content rather than a fabricated successful part.
    model.select(None, Some("default".into()));
    model.select_tab("Diagnostics".into());
    view.changed(&model);
    let failed_start = view.request(&model, false);
    assert!(worker.read(model.generation, None, Some("default".into()), true));
    let read = loop {
        if let Update::Fleet {
            generation,
            connection,
            invalidation,
            result,
            observation,
        } = updates.recv_blocking().unwrap()
        {
            break (generation, connection, invalidation, result, observation);
        }
    };
    let current = worker.read_is_current(&read.1, read.2);
    assert!(current);
    let action = model.read_complete(read.0, &read.1, current, read.3);
    view.fleet(
        &model, read.0, &read.1, read.2, current, true, &action, read.4,
    );
    let scope = Scope {
        connection: None,
        yard: Some("default".into()),
    };
    worker.details(model.generation, scope.clone());
    loop {
        if let Update::Details {
            generation,
            scope,
            invalidation,
            result,
            observation,
        } = updates.recv_blocking().unwrap()
        {
            let details = result.unwrap();
            let parts = Parts::details(&details);
            assert_ne!(parts.failed & DIAGNOSTICS, 0);
            view.details(&model, generation, &scope, invalidation, parts, observation);
            break;
        }
    }
    assert!(view.rendered(&model, true).is_none());
    assert!(
        observer
            .terminals()
            .iter()
            .any(|row| row.ticket == failed_start.tickets()[0].ordinal()
                && row.outcome == TerminalOutcome::Retired(Retirement::Unknown))
    );
    assert!(
        observer
            .terminals()
            .iter()
            .all(|row| !matches!(row.outcome, TerminalOutcome::Paint { .. })),
        "headless acceptance tests do not claim GTK paint"
    );
    view.close();
    worker.close();
    while !matches!(updates.recv_blocking().unwrap(), Update::Closed) {}
    assert_eq!(observer.counts().pending, 0);
    // A separate real negotiated host session without profile-list capability
    // produces explicit N/A profile/preset parts, not a fictitious query success.
    std::env::set_var("VERANDA_WORKER_PROFILE_NA", "1");
    let (mut no_profiles, events) = Worker::observed(root.join("private-na"), observer.clone());
    loop {
        if let Update::Initialized { local, .. } = events.recv_blocking().unwrap() {
            local.unwrap();
            break;
        }
    }
    no_profiles.details(
        0,
        Scope {
            connection: None,
            yard: None,
        },
    );
    loop {
        if let Update::Details { result, .. } = events.recv_blocking().unwrap() {
            let details = result.unwrap();
            let parts = Parts::details(&details);
            assert_ne!(parts.not_applicable & crate::view_observation::PRESETS, 0);
            assert_ne!(parts.not_applicable & crate::view_observation::PROFILES, 0);
            break;
        }
    }
    no_profiles.close();
    while !matches!(events.recv_blocking().unwrap(), Update::Closed) {}
    std::env::remove_var("VERANDA_WORKER_PROFILE_NA");
    assert_eq!(observer.counts().pending, 0);
}

#[cfg(target_os = "linux")]
#[test]
fn correlation_cohorts_use_actual_successful_worker_proofs() {
    run_actor("worker::reopen_tests::correlation_cohorts_actor", true);
}
#[cfg(target_os = "linux")]
#[test]
#[ignore]
fn correlation_cohorts_actor() {
    correlation_cohorts_case(false, false, 0);
}
#[cfg(target_os = "linux")]
#[test]
fn correlation_normalized_owner_replacement_invalidates_join() {
    run_actor(
        "worker::reopen_tests::correlation_normalized_owner_actor",
        true,
    );
}
#[cfg(target_os = "linux")]
#[test]
#[ignore]
fn correlation_normalized_owner_actor() {
    correlation_cohorts_case(true, false, 0);
}
#[cfg(target_os = "linux")]
#[test]
fn correlation_owner_replacement_without_new_proof_invalidates_join() {
    run_actor(
        "worker::reopen_tests::correlation_unproven_owner_actor",
        true,
    );
}
#[cfg(target_os = "linux")]
#[test]
#[ignore]
fn correlation_unproven_owner_actor() {
    correlation_cohorts_case(false, true, 0);
}
#[cfg(target_os = "linux")]
#[test]
fn correlation_runtime_freezes_actual_guarded_proofs_and_publishes_ready() {
    run_actor("worker::reopen_tests::correlation_runtime_actor", true);
}
#[cfg(target_os = "linux")]
#[test]
#[ignore]
fn correlation_runtime_actor() {
    correlation_cohorts_case(false, false, 1);
}
#[cfg(target_os = "linux")]
#[test]
fn correlation_runtime_after_ready_owner_change_is_unknown() {
    run_actor(
        "worker::reopen_tests::correlation_runtime_changed_owner_actor",
        true,
    );
}
#[cfg(target_os = "linux")]
#[test]
#[ignore]
fn correlation_runtime_changed_owner_actor() {
    correlation_cohorts_case(true, false, 2);
}
#[cfg(target_os = "linux")]
#[test]
fn correlation_runtime_after_ready_fresh_worker_is_unqualified() {
    run_actor(
        "worker::reopen_tests::correlation_runtime_fresh_worker_actor",
        true,
    );
}
#[cfg(target_os = "linux")]
#[test]
#[ignore]
fn correlation_runtime_fresh_worker_actor() {
    correlation_cohorts_case(false, false, 3);
}
#[cfg(target_os = "linux")]
fn correlation_cohorts_case(normalized: bool, missing_new_proof: bool, runtime: u8) {
    use crate::{
        event_correlation::{Cohorts, ExpectedCohort, Role},
        model::{Model, ReadAction},
        view_observation::Parts,
    };
    let root = PathBuf::from(std::env::var_os("VERANDA_REOPEN_FIXTURE").unwrap());
    let (observer, run) = if runtime != 0 {
        let config = root.join("runtime.json");
        let raw=serde_json::to_vec(&serde_json::json!({"schema":2,"offers":[{"offer":1,"action":"synthetic","source":"/synthetic"}],"cohorts":[{"ordinal":1,"owner":"example-owner","connection":null,"yard":"default"}],"ready":root.join("ready.json")})).unwrap();
        fs::write(&config, raw).unwrap();
        fs::set_permissions(&config, fs::Permissions::from_mode(0o600)).unwrap();
        let plan =
            crate::event_correlation::RuntimePlan::load(&config, &root.join("trace.json")).unwrap();
        let (observation, run) = plan.start().unwrap();
        assert_eq!(observation.counts().session_sources, 0);
        (observation, Some(run))
    } else {
        (Observation::new(), None)
    };
    let (mut worker, updates) = Worker::observed(root.join("private"), observer.clone());
    let local = loop {
        if let Update::Initialized { local, .. } = updates.recv_blocking().unwrap() {
            break local.unwrap();
        }
    };
    let owner = local.owner.id.clone();
    let cohorts = run
        .as_ref()
        .map(|run| run.handle().cohorts.clone())
        .unwrap_or_else(|| {
            std::sync::Arc::new(std::sync::Mutex::new(
                Cohorts::new(
                    observer.clone(),
                    vec![ExpectedCohort {
                        ordinal: 1,
                        owner: owner.clone(),
                        connection: None,
                        yard: Some("default".into()),
                    }],
                )
                .unwrap(),
            ))
        });
    let mut view = if let Some(run) = &run {
        crate::view_observation::ViewObservation::with_runtime(observer.clone(), run.handle())
    } else {
        crate::view_observation::ViewObservation::with_cohorts(observer.clone(), cohorts.clone())
    };
    let mut model = Model::new(vec![]);
    model.update(&None, Ok(local));
    model.initialize(None);
    model.select(None, Some("default".into()));
    // The first ordinary read triggers the existing manager's two invalidations.
    // A second read is admitted explicitly by this test only after receiving them.
    assert!(worker.read(model.generation, None, Some("default".into()), true));
    let first = loop {
        if let Update::Fleet {
            generation,
            connection,
            invalidation,
            result,
            observation,
        } = updates.recv_blocking().unwrap()
        {
            break (generation, connection, invalidation, result, observation);
        }
    };
    while worker
        .events
        .lock()
        .unwrap()
        .carry(&None, &None)
        .unwrap()
        .batch
        .tickets()
        .len()
        != 2
    {
        updates.recv_blocking().unwrap();
    }
    let current = worker.read_is_current(&first.1, first.2);
    assert!(!current);
    let typed_owner = first.3.as_ref().ok().map(|v| v.owner.id.clone());
    let action = model.read_complete(first.0, &first.1, current, first.3);
    view.fleet_identified(
        &model,
        first.0,
        &first.1,
        first.2,
        current,
        true,
        &action,
        typed_owner.as_deref(),
        first.4,
    );
    assert!(cohorts.lock().unwrap().bindings().is_empty());
    assert!(worker.read(model.generation, None, Some("default".into()), false));
    let next = loop {
        if let Update::Fleet {
            generation,
            connection,
            invalidation,
            result,
            observation,
        } = updates.recv_blocking().unwrap()
        {
            break (generation, connection, invalidation, result, observation);
        }
    };
    let current = worker.read_is_current(&next.1, next.2);
    assert!(current);
    let actual_metadata = next.4.clone();
    let typed_owner = next.3.as_ref().ok().map(|v| v.owner.id.clone());
    let action = model.read_complete(next.0, &next.1, current, next.3);
    assert_eq!(action, ReadAction::Applied);
    view.fleet_identified(
        &model,
        next.0,
        &next.1,
        next.2,
        current,
        true,
        &action,
        typed_owner.as_deref(),
        next.4,
    );
    assert_eq!(cohorts.lock().unwrap().bindings().len(), 1);
    let scope = Scope {
        connection: None,
        yard: Some("default".into()),
    };
    worker.details(model.generation, scope.clone());
    loop {
        if let Update::Details {
            generation,
            scope,
            invalidation,
            result,
            observation,
        } = updates.recv_blocking().unwrap()
        {
            let details = result.unwrap();
            let parts = Parts::details(&details);
            assert_eq!(parts.failed, 0);
            assert!(worker.read_is_current(&scope.connection, invalidation));
            view.details(&model, generation, &scope, invalidation, parts, observation);
            break;
        }
    }
    let guard = cohorts.lock().unwrap();
    assert!(guard.complete());
    assert_eq!(guard.bindings().len(), 2);
    assert_eq!(guard.bindings()[0].role, Role::Fleet);
    assert_eq!(guard.bindings()[1].role, Role::Details);
    assert_eq!(
        guard.bindings()[0].source,
        guard.bindings()[1].source,
        "actual current-yard session alias"
    );
    assert!(!guard.replacement_unknown());
    drop(guard);
    if let Some(run) = run {
        let deadline = Instant::now() + Duration::from_secs(2);
        while !root.join("ready.json").exists() && Instant::now() < deadline {
            std::thread::sleep(Duration::from_millis(2));
        }
        let ready: serde_json::Value =
            serde_json::from_slice(&fs::read(root.join("ready.json")).unwrap()).unwrap();
        assert_eq!(ready["bindings"].as_array().unwrap().len(), 2);
        if runtime == 2 {
            // Same advertised collector boundary as the accepted replacement
            // regression; the actual Client independently refuses host-ID drift.
            let mut changed = model.host().snapshot.as_ref().unwrap().clone();
            changed.owner.id = "synthetic-replacement-owner".into();
            changed.owner.yards.clear();
            let owner = changed.owner.id.clone();
            let generation = model.generation;
            let action = model.read_complete(generation, &None, true, Ok(changed));
            view.fleet_identified(
                &model,
                generation,
                &None,
                next.2,
                true,
                true,
                &action,
                Some(&owner),
                None,
            );
        }
        if runtime == 3 {
            // A real second framed Worker allocates fresh validated RPC sources
            // in the same ledger. They cannot extend the frozen role table.
            let (mut second, updates) = Worker::observed(root.join("private"), observer.clone());
            loop {
                if let Update::Initialized { local, .. } = updates.recv_blocking().unwrap() {
                    local.unwrap();
                    break;
                }
            }
            second.close();
            loop {
                if let Update::Closed { .. } = updates.recv_blocking().unwrap() {
                    break;
                }
            }
        }
        view.close();
        worker.close();
        loop {
            if let Update::Closed { .. } = updates.recv_blocking().unwrap() {
                break;
            }
        }
        let final_run = run.finish();
        assert_eq!(
            final_run.writer,
            crate::event_correlation::WriterOutcome::Written
        );
        if runtime == 2 {
            assert!(!final_run.cohorts_complete && final_run.replacement_unknown);
        } else {
            assert!(final_run.cohorts_complete && !final_run.replacement_unknown);
        }
        if runtime == 3 {
            assert!(observer.counts().session_sources > ready["source_frontier"].as_u64().unwrap());
        }
        assert_eq!(serde_json::to_value(final_run.ready).unwrap(), ready);
        let before = {
            let state = cohorts.lock().unwrap();
            (
                state.bindings().to_vec(),
                state.complete(),
                state.replacement_unknown(),
            )
        };
        let sealed = serde_json::to_value(observer.seal()).unwrap();
        view.fleet_identified(
            &model,
            model.generation,
            &None,
            next.2,
            true,
            true,
            &ReadAction::Applied,
            Some("synthetic-late-owner"),
            None,
        );
        let after = {
            let state = cohorts.lock().unwrap();
            (
                state.bindings().to_vec(),
                state.complete(),
                state.replacement_unknown(),
            )
        };
        assert_eq!(
            before, after,
            "finished runtime cohort state is immutable to held View callbacks"
        );
        assert_eq!(serde_json::to_value(observer.seal()).unwrap(), sealed);
        assert!(
            observer
                .correlation()
                .unwrap()
                .index
                .iter()
                .all(|row| row.tag == veranda_lib::observation::EventTag::Unclassifiable)
        );
        return;
    }
    let mapping = root.join("correlation.json");
    let mut mapping_file = std::fs::OpenOptions::new()
        .create_new(true)
        .write(true)
        .open(&mapping)
        .unwrap();
    std::fs::set_permissions(&mapping, std::fs::Permissions::from_mode(0o600)).unwrap();
    std::io::Write::write_all(
        &mut mapping_file,
        br#"{"schema":1,"offers":[{"offer":1,"action":"synthetic","source":"/synthetic"}]}"#,
    )
    .unwrap();
    let classifier = crate::event_correlation::Classifier::load(&mapping).unwrap();
    cohorts.lock().unwrap().freeze(&classifier).unwrap();
    // A different connection cannot contradict this accepted Owner binding.
    // Keep the actual proof; the original Model connection guard must refuse.
    cohorts.lock().unwrap().fleet(
        &model,
        model.generation,
        &Some("synthetic-unselected-connection".into()),
        true,
        &ReadAction::Applied,
        Some("synthetic-replacement-owner"),
        actual_metadata.as_ref(),
    );
    assert!(cohorts.lock().unwrap().complete());
    // The Client already refuses a changed session Owner ID. This intentionally
    // isolates the collector's advertised accepted-result boundary with a typed
    // Model result and the actual framed proof; it is NOT a native drift claim.
    let mut replacement = model.host().snapshot.as_ref().unwrap().clone();
    replacement.owner.id = "synthetic-replacement-owner".into();
    if normalized {
        replacement.owner.yards.clear();
    }
    let replacement_owner = replacement.owner.id.clone();
    let generation = model.generation;
    let action = model.read_complete(generation, &None, true, Ok(replacement));
    assert_eq!(action, ReadAction::Applied);
    assert_eq!(model.generation, generation);
    if normalized {
        assert!(model.selection.yard_name.is_none());
    } else {
        assert_eq!(model.selection.yard_name.as_deref(), Some("default"));
    }
    view.fleet_identified(
        &model,
        generation,
        &None,
        next.2,
        true,
        true,
        &action,
        Some(&replacement_owner),
        if missing_new_proof {
            None
        } else {
            actual_metadata
        },
    );
    let refused = {
        let state = cohorts.lock().unwrap();
        state.replacement_unknown() && !state.complete()
    };
    view.close();
    worker.close();
    loop {
        if let Update::Closed { .. } = updates.recv_blocking().unwrap() {
            break;
        }
    }
    assert!(
        refused,
        "accepted Owner identity replacement must invalidate the frozen cohort join"
    );
}
