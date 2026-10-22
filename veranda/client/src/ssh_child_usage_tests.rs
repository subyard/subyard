//! Real children through the shared production capture port; no SSH/Code/network.
use super::*;
use crate::child_usage::TestHooks;
use crate::observation::{Observation, SourceKind};
use std::os::unix::process::CommandExt;
use std::sync::{Arc, Mutex};
struct Canary(std::process::Child);
impl Drop for Canary {
    fn drop(&mut self) {
        let _ = self.0.kill();
        let _ = self.0.wait();
    }
}

fn command(code: &str) -> Command {
    let program = std::env::var_os("VERANDA_TEST_PYTHON").unwrap_or_else(|| "python3".into());
    let mut command = Command::new(program);
    command.args(["-c", code]);
    command
}
fn value(observation: &Observation) -> serde_json::Value {
    serde_json::to_value(observation.seal()).unwrap()
}
fn run(
    code: &str,
    usage: Option<&UsageContext>,
    timeout: Duration,
) -> Result<Vec<u8>, NativeError> {
    capture_owned(
        &mut command(code),
        None,
        timeout,
        usage,
        ChildKind::ShortTool,
    )
}
fn reaped(pid: u32) {
    let mut status = 0;
    assert_eq!(
        unsafe { libc::waitpid(pid as i32, &mut status, libc::WNOHANG) },
        -1
    );
    assert_eq!(
        std::io::Error::last_os_error().raw_os_error(),
        Some(libc::ECHILD)
    );
}

#[test]
fn child_usage_actual_keyscan_fingerprint_agent_and_code_paths_no_event_source() {
    let observation = Observation::with_child_usage(8);
    let offered = Arc::new(Mutex::new(Vec::new()));
    let calls = offered.clone();
    let context = observation.child_usage_context().unwrap().with_hooks(TestHooks {
        spawn: Some(Arc::new(move |original| {
            let tool = original.get_program().to_str().unwrap();
            calls.lock().unwrap().push(tool.to_owned());
            let code = match tool {
                "ssh-keyscan" => "print('synthetic-host ssh-ed25519 AAAA')",
                "ssh-keygen" => "import sys; sys.stdin.buffer.read(); print('256 SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA synthetic')",
                "ssh-add" => "print('256 synthetic-public-agent-listing')",
                "code" | "code.cmd" => "print('ms-vscode-remote.remote-ssh')",
                _ => panic!("unexpected test tool"),
            };
            command(code).process_group(0)
                .stdin(if tool == "ssh-keygen" { Stdio::piped() } else { Stdio::null() })
                .stdout(Stdio::piped()).stderr(Stdio::null()).spawn()
        })), ..TestHooks::default()
    });
    let endpoint = Endpoint::parse("synthetic-host").unwrap();
    let key = assess_host_key(&endpoint, Some(&context)).unwrap();
    assert_eq!(
        key.fingerprint,
        "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
    );
    require_agent(Some(&context)).unwrap();
    crate::sessions::extension_probe_for_test(Some(&context)).unwrap();
    let snapshot = value(&observation);
    assert_eq!(observation.counts().session_sources, 0);
    assert_eq!(observation.counts().received, 0);
    assert!(observation.records().is_empty());
    let rows = snapshot["child_usage"]["records"].as_array().unwrap();
    assert_eq!(rows.len(), 4);
    let expected = [
        "HostKeyScan",
        "HostKeyFingerprint",
        "AgentReadiness",
        "EditorExtensionProbe",
    ];
    for (row, kind) in rows.iter().zip(expected) {
        assert_eq!(row["kind"], kind);
        assert_eq!(row["state"], "Captured");
        assert!(row["source"].is_null());
        assert!(row["source_kind"].is_null());
        assert_eq!(row["context"], snapshot["context"]);
        assert_eq!(row["provenance"], rows[0]["provenance"]);
        assert_eq!(row["captures"], 1);
        assert_eq!(row["reaps"], 1);
        reaped(row["owned_pid"].as_u64().unwrap() as u32);
    }
    assert_eq!(
        &offered.lock().unwrap()[..3],
        ["ssh-keyscan", "ssh-keygen", "ssh-add"]
    );
    let text = snapshot.to_string();
    assert!(!text.contains("synthetic-host"));
    assert!(!text.contains("AAAA"));
    assert!(!text.contains("agent-listing"));
    assert!(!text.contains("remote-ssh"));
}

#[test]
fn child_usage_capture_memory_status_exact_times_and_actual_optional_source() {
    let observation = Observation::with_child_usage(2);
    let source = observation.source(SourceKind::Remote);
    let context = source.child_usage_context().unwrap();
    let before = Instant::now();
    assert_eq!(
        run(
            "a=bytearray(24*1024*1024); print('ready')",
            Some(&context),
            Duration::from_secs(2)
        )
        .unwrap(),
        b"ready\n"
    );
    let after = Instant::now();
    let sealed = observation.seal();
    let exact = &sealed.child_usage().unwrap().records[0];
    assert!(exact.pre_spawn() >= before && exact.pre_spawn() <= after);
    assert!(exact.post_owning_wait().unwrap() >= exact.pre_spawn());
    assert!(exact.post_owning_wait().unwrap() <= after);
    let snapshot = serde_json::to_value(sealed).unwrap();
    let row = &snapshot["child_usage"]["records"][0];
    assert_eq!(row["source"], source.ordinal());
    assert_eq!(row["source_kind"], "Remote");
    assert_eq!(row["terminal_pid"], row["owned_pid"]);
    assert_eq!(row["terminal_code"], libc::CLD_EXITED);
    assert_eq!(row["terminal_status"], 0);
    assert_eq!(row["raw_wait_status"], 0);
    assert_eq!(row["returncode"], 0);
    assert!(row["usage"]["maxrss_kib"].as_i64().unwrap() >= 24 * 1024);
    reaped(row["owned_pid"].as_u64().unwrap() as u32);
}

#[test]
fn child_usage_nonzero_signal_and_live_terminal_race_preserve_capture_results() {
    let observation = Observation::with_child_usage(3);
    let context = observation.child_usage_context().unwrap();
    assert_eq!(
        run(
            "print('retained'); raise SystemExit(7)",
            Some(&context),
            Duration::from_secs(2)
        )
        .unwrap_err()
        .code,
        "ssh_probe_failed"
    );
    assert_eq!(
        run(
            "import os,signal; os.kill(os.getpid(),signal.SIGTERM)",
            Some(&context),
            Duration::from_secs(2)
        )
        .unwrap_err()
        .code,
        "ssh_probe_failed"
    );
    let once = Arc::new(std::sync::atomic::AtomicBool::new(false));
    let first = once.clone();
    let raced = context.clone().with_hooks(TestHooks {
        after_live: Some(Arc::new(move |pid| {
            if !first.swap(true, std::sync::atomic::Ordering::SeqCst) {
                unsafe {
                    libc::kill(pid as i32, libc::SIGTERM);
                }
            }
        })),
        ..TestHooks::default()
    });
    assert_eq!(
        run(
            "import signal; signal.pause()",
            Some(&raced),
            Duration::from_secs(2)
        )
        .unwrap_err()
        .code,
        "ssh_probe_failed"
    );
    assert!(once.load(std::sync::atomic::Ordering::SeqCst));
    let snapshot = value(&observation);
    let rows = snapshot["child_usage"]["records"].as_array().unwrap();
    assert_eq!(rows[0]["terminal_status"], 7);
    assert_eq!(rows[1]["terminal_code"], libc::CLD_KILLED);
    assert_eq!(rows[2]["terminal_status"], libc::SIGTERM);
    for row in rows {
        assert_eq!(row["state"], "Captured");
        assert_eq!(row["captures"], 1);
        assert_eq!(row["reaps"], 1);
        reaped(row["owned_pid"].as_u64().unwrap() as u32);
    }
}

#[test]
fn child_usage_timeout_output_limit_and_inherited_pipe_revoke_keep_canary_alive() {
    let observation = Observation::with_child_usage(3);
    let context = observation.child_usage_context().unwrap();
    let mut canary = Canary(
        command("import signal; signal.pause()")
            .process_group(0)
            .spawn()
            .unwrap(),
    );
    {
        assert_eq!(
            run(
                "import time; time.sleep(60)",
                Some(&context),
                Duration::from_millis(30)
            )
            .unwrap_err()
            .code,
            "ssh_timeout"
        );
        assert_eq!(
            run(
                "import sys; sys.stdout.write('x'*70000)",
                Some(&context),
                Duration::from_secs(2)
            )
            .unwrap_err()
            .code,
            "ssh_output_limit"
        );
        assert_eq!(run("import subprocess,sys; subprocess.Popen([sys.executable,'-c','import time;time.sleep(60)']);print('complete')", Some(&context), Duration::from_secs(2)).unwrap(), b"complete\n");
        assert!(canary.0.try_wait().unwrap().is_none());
    }
    drop(canary);
    let snapshot = value(&observation);
    for row in snapshot["child_usage"]["records"].as_array().unwrap() {
        assert_eq!(row["state"], "Captured");
        assert_eq!(row["reaps"], 1);
        reaped(row["owned_pid"].as_u64().unwrap() as u32);
    }
}

#[test]
fn child_usage_capture_fault_does_not_change_success_and_postcapture_echild_is_unknown() {
    let observation = Observation::with_child_usage(2);
    let context = observation.child_usage_context().unwrap();
    let failed = context.clone().with_hooks(TestHooks {
        capture_pid: Some(u32::MAX),
        ..TestHooks::default()
    });
    assert_eq!(
        run(
            "print('normal result')",
            Some(&failed),
            Duration::from_secs(2)
        )
        .unwrap(),
        b"normal result\n"
    );
    let consumed = context.with_hooks(TestHooks {
        before_wait: Some(Arc::new(|pid| {
            let mut status = 0;
            assert_eq!(
                unsafe { libc::waitpid(pid as i32, &mut status, 0) },
                pid as i32
            );
        })),
        ..TestHooks::default()
    });
    assert_eq!(
        run("print('complete')", Some(&consumed), Duration::from_secs(2))
            .unwrap_err()
            .code,
        "ssh_probe_failed"
    );
    let snapshot = value(&observation);
    let rows = snapshot["child_usage"]["records"].as_array().unwrap();
    assert_eq!(rows[0]["state"], "Unknown");
    assert_eq!(rows[0]["unknown"], "CaptureFailed");
    assert_eq!(rows[0]["returncode"], 0);
    assert_eq!(rows[0]["reaps"], 1);
    assert!(rows[0]["usage"].is_null());
    assert_eq!(rows[1]["state"], "Unknown");
    assert_eq!(rows[1]["unknown"], "WaitFailed");
    assert_eq!(rows[1]["captures"], 1);
    assert_eq!(rows[1]["reaps"], 0);
    assert!(rows[1]["raw_wait_status"].is_null());
    for row in rows {
        reaped(row["owned_pid"].as_u64().unwrap() as u32);
    }
}

#[test]
fn child_usage_failed_spawn_none_bounds_and_sealed_alive_late_completion() {
    let plain = Observation::new();
    assert!(plain.child_usage_context().is_none());
    assert_eq!(
        run("print('plain')", None, Duration::from_secs(2)).unwrap(),
        b"plain\n"
    );
    assert!(
        serde_json::to_value(plain.seal())
            .unwrap()
            .get("child_usage")
            .is_none()
    );
    let observation = Observation::with_child_usage(1);
    let context = observation.child_usage_context().unwrap();
    assert_eq!(
        capture_owned(
            &mut Command::new("/nonexistent-r91-child"),
            None,
            Duration::from_secs(1),
            Some(&context),
            ChildKind::ShortTool
        )
        .unwrap_err()
        .code,
        "ssh_tool_unavailable"
    );
    assert_eq!(
        run("print('overflow')", Some(&context), Duration::from_secs(2)).unwrap(),
        b"overflow\n"
    );
    let snapshot = value(&observation);
    assert_eq!(snapshot["child_usage"]["dropped"], 1);
    assert_eq!(snapshot["child_usage"]["overflow"], "Overflow");
    assert_eq!(
        snapshot["child_usage"]["records"][0]["unknown"],
        "SpawnFailed"
    );
    assert!(snapshot["child_usage"]["records"][0]["usage"].is_null());
    let live = Observation::with_child_usage(1);
    let (sender, receiver) = mpsc::channel();
    let context = live.child_usage_context().unwrap().with_hooks(TestHooks {
        spawned: Some(sender),
        ..TestHooks::default()
    });
    let worker = thread::spawn(move || {
        run(
            "import signal; signal.pause()",
            Some(&context),
            Duration::from_secs(2),
        )
    });
    let pid = receiver.recv_timeout(Duration::from_secs(1)).unwrap();
    let sealed = value(&live);
    assert_eq!(sealed["child_usage"]["records"][0]["unknown"], "Unfinished");
    unsafe {
        libc::kill(pid as i32, libc::SIGTERM);
    }
    assert_eq!(worker.join().unwrap().unwrap_err().code, "ssh_probe_failed");
    assert_eq!(value(&live), sealed);
    reaped(pid);
}
