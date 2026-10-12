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
def fixture(name):
    with open(os.path.join(fixtures, name + '.frame'), 'rb') as source:
        return json.loads(source.read()[4:])['result']
def reply(request, result):
    value = {'version':1, 'type':'response', 'id':request['id'], 'result':result}
    if request['method'] != 'rpc.negotiate':
        value['operationId'] = request.get('operationId') or request['id']
    body = json.dumps(value).encode('utf8')
    sys.stdout.buffer.write(struct.pack('>I', len(body)) + body)
    sys.stdout.buffer.flush()
while True:
    prefix = sys.stdin.buffer.read(4)
    if not prefix:
        break
    request = json.loads(sys.stdin.buffer.read(struct.unpack('>I', prefix)[0]))
    method = request['method']
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
        names = {'rpc.negotiate':'negotiate', 'owner.inventory':'owner-inventory', 'context.get':'context'}
        result = fixture(names[method]) if method in names else {'pong':True}
    reply(request, result)
"#;

#[test]
fn admitted_eof_stays_unknown_after_worker_reopen() {
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
        .args([
            "--exact",
            "worker::reopen_tests::reopen_actor",
            "--ignored",
            "--nocapture",
        ])
        .process_group(0)
        .env("PATH", path)
        .env("VERANDA_REOPEN_FIXTURE", &root)
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
                "worker reopen fixture exceeded its bounded deadline; {}",
                fs::read_to_string(root.join("actor.log")).unwrap()
            );
        }
        std::thread::sleep(Duration::from_millis(10));
    };
    assert!(
        status.success(),
        "{}",
        fs::read_to_string(root.join("actor.log")).unwrap()
    );
    assert_eq!(
        fs::read_to_string(root.join("execute-observed")).unwrap(),
        "1"
    );
    fs::remove_dir_all(root).unwrap();
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
