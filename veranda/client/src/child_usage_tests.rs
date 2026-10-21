//! Real child contracts at the sole owning capture/reap seam.
use super::*;
use crate::observation::{Observation, SourceKind};
use std::os::unix::process::{CommandExt, ExitStatusExt};

struct Guard(OwnedChild);
impl Drop for Guard {
    fn drop(&mut self) {
        // Do not signal a numeric PID/group after an earlier successful reap.
        let mut info: libc::siginfo_t = unsafe { std::mem::zeroed() };
        let still_owned = unsafe {
            libc::waitid(
                libc::P_PID,
                self.0.id(),
                &mut info,
                libc::WEXITED | libc::WNOWAIT | libc::WNOHANG,
            )
        } == 0;
        if still_owned {
            stop_child(&mut self.0);
        }
    }
}
fn owned(observation: Option<&Source>, code: &str) -> Guard {
    let usage = observation.and_then(Source::child_usage_context);
    let python = std::env::var_os("VERANDA_TEST_PYTHON").unwrap_or_else(|| "python3".into());
    let mut command = Command::new(python);
    command
        .args(["-c", code])
        .process_group(0)
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null());
    Guard(OwnedChild::spawn(&mut command, usage.as_ref(), ChildKind::RpcSession).unwrap())
}
fn snapshot(observation: &Observation) -> serde_json::Value {
    serde_json::to_value(observation.seal()).unwrap()
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
fn framed(observation: &Observation, mode: &str, registry: Option<&Processes>) -> RpcClient {
    let fixture = transport_tests::OwnerFixture::new();
    RpcClient::open_observed(
        fixture.command(mode),
        "0.1.0-dev",
        Arc::new(|_| {}),
        registry,
        Some(observation.source(SourceKind::Local)),
    )
    .unwrap()
}

#[test]
fn child_usage_native_linux_layout_and_syscall_are_bound() {
    assert_eq!(std::mem::size_of::<usize>(), 8);
    assert_eq!(std::mem::size_of::<libc::siginfo_t>(), 128);
    assert_eq!(std::mem::size_of::<libc::rusage>(), 144);
    assert_eq!(std::mem::offset_of!(libc::rusage, ru_maxrss), 32);
    assert_eq!(libc::SYS_waitid, 247);
    assert!(cfg!(target_endian = "little"));
}

#[test]
fn child_usage_memory_original_times_status_pid_and_cache() {
    let observation = Observation::with_child_usage(8);
    let source = observation.source(SourceKind::Remote);
    let before_spawn = Instant::now();
    let mut child = owned(
        Some(&source),
        "a=bytearray(24*1024*1024); a[::4096]=b'x'*len(a[::4096]); raise SystemExit(7)",
    );
    let pid = child.0.id();
    let after_spawn = Instant::now();
    let before_wait = Instant::now();
    let status = child.0.wait().unwrap();
    let after_wait = Instant::now();
    assert_eq!(status.code(), Some(7));
    assert_eq!(child.0.wait().unwrap(), status);
    reaped(pid);
    let sealed = observation.seal();
    let exact = &sealed.child_usage().unwrap().records[0];
    assert!(exact.pre_spawn() >= before_spawn && exact.pre_spawn() <= after_spawn);
    let finished = exact.post_owning_wait().unwrap();
    assert!(finished >= before_wait && finished <= after_wait);
    assert_eq!(
        exact.interval_start_micros,
        observation.relative_micros(exact.pre_spawn())
    );
    let floor = observation.relative_micros(finished).unwrap();
    assert!(
        exact.interval_end_micros == Some(floor)
            || exact.interval_end_micros == floor.checked_add(1)
    );
    let value = snapshot(&observation);
    let row = &value["child_usage"]["records"][0];
    assert_eq!(row["state"], "Captured");
    assert_eq!(row["source"], source.ordinal());
    assert_eq!(row["source_kind"], "Remote");
    assert_eq!(row["kind"], "RpcSession");
    assert_eq!(row["context"], value["context"]);
    assert_eq!(row["owned_pid"], pid);
    assert_eq!(row["terminal_pid"], pid);
    assert_eq!(row["terminal_code"], libc::CLD_EXITED);
    assert_eq!(row["terminal_status"], 7);
    assert_eq!(row["raw_wait_status"], status.into_raw());
    assert_eq!(row["returncode"], 7);
    assert!(row["usage"]["maxrss_kib"].as_i64().unwrap() >= 24 * 1024);
    assert!(row["usage"]["minflt"].as_i64().unwrap() > 0);
    assert_eq!(row["captures"], 1);
    assert_eq!(row["reaps"], 1);
    assert!(
        row["post_owning_wait_micros"].as_u64().unwrap()
            >= row["pre_spawn_micros"].as_u64().unwrap()
    );
}

#[test]
fn child_usage_live_poll_then_terminal_reserves_pid_until_ordinary_wait() {
    let observation = Observation::with_child_usage(1);
    let source = observation.source(SourceKind::Local);
    let mut child = owned(Some(&source), "import signal; signal.pause()");
    let pid = child.0.id();
    assert!(!child_exited(&mut child.0));
    unsafe {
        libc::kill(pid as i32, libc::SIGTERM);
    }
    let deadline = Instant::now() + Duration::from_secs(2);
    while !child_exited(&mut child.0) {
        assert!(Instant::now() < deadline);
        thread::yield_now();
    }
    let mut info: libc::siginfo_t = unsafe { std::mem::zeroed() };
    assert_eq!(
        unsafe {
            libc::waitid(
                libc::P_PID,
                pid,
                &mut info,
                libc::WEXITED | libc::WNOWAIT | libc::WNOHANG,
            )
        },
        0
    );
    assert_eq!(unsafe { info.si_pid() }, pid as i32);
    assert_eq!(unsafe { libc::kill(pid as i32, 0) }, 0);
    let status = child.0.wait().unwrap();
    assert_eq!(status.signal(), Some(libc::SIGTERM));
    reaped(pid);
    let value = snapshot(&observation);
    let row = &value["child_usage"]["records"][0];
    assert_eq!(row["state"], "Captured");
    assert_eq!(row["terminal_code"], libc::CLD_KILLED);
    assert_eq!(row["terminal_status"], libc::SIGTERM);
    assert_eq!(row["captures"], 1);
    assert_eq!(row["reaps"], 1);
}

#[test]
fn child_usage_concurrent_close_and_two_registries_reap_once_canary_survives() {
    let observation = Observation::with_child_usage(8);
    let registry = Arc::new(Processes::default());
    let attempt = Arc::new(Processes::default());
    let client = Arc::new(framed(&observation, "unresponsive", Some(&registry)));
    attempt.track(&client.child).unwrap();
    let pid = client.child.lock().unwrap().as_ref().unwrap().id();
    let mut canary = owned(None, "import signal; signal.pause()");
    let closing = client.clone();
    let shutdown = registry.clone();
    let second = attempt.clone();
    let a = thread::spawn(move || closing.close());
    let b = thread::spawn(move || shutdown.shutdown());
    let c = thread::spawn(move || second.shutdown());
    a.join().unwrap();
    b.join().unwrap();
    c.join().unwrap();
    client.close();
    assert_eq!(registry.tracked_count_for_test(), 0);
    assert_eq!(attempt.tracked_count_for_test(), 0);
    assert!(!canary.0.exited());
    reaped(pid);
    let value = snapshot(&observation);
    let row = &value["child_usage"]["records"][0];
    assert_eq!(row["state"], "Captured");
    assert_eq!(row["captures"], 1);
    assert_eq!(row["reaps"], 1);
    assert_eq!(row["owned_pid"], pid);
    assert_eq!(row["terminal_code"], libc::CLD_KILLED);
}

#[test]
fn child_usage_framed_normal_rejected_and_failed_spawn_keep_original_errors() {
    let observation = Observation::with_child_usage(8);
    let client = framed(&observation, "normal", None);
    client.close();
    let fixture = transport_tests::OwnerFixture::new();
    let rejected = RpcClient::open_observed(
        fixture.command("version-mismatch"),
        "0.1.0-dev",
        Arc::new(|_| {}),
        None,
        Some(observation.source(SourceKind::Local)),
    );
    assert!(matches!(rejected, Err(NativeError { ref code, .. }) if code == "incompatible_engine"));
    let failed = RpcClient::open_observed(
        Command::new("/nonexistent-veranda-r86-command"),
        "0.1.0-dev",
        Arc::new(|_| {}),
        None,
        Some(observation.source(SourceKind::Local)),
    );
    assert!(matches!(failed, Err(NativeError { ref code, .. }) if code == "transport_unavailable"));
    let stopped = Processes::default();
    stopped.stop_accepting();
    let registration = RpcClient::open_observed(
        fixture.command("normal"),
        "0.1.0-dev",
        Arc::new(|_| {}),
        Some(&stopped),
        Some(observation.source(SourceKind::Local)),
    );
    assert!(registration.is_err());
    let value = snapshot(&observation);
    let rows = value["child_usage"]["records"].as_array().unwrap();
    assert_eq!(rows.len(), 4);
    assert_eq!(rows[0]["state"], "Captured");
    assert_eq!(rows[0]["terminal_code"], libc::CLD_EXITED);
    assert_eq!(rows[0]["terminal_status"], 0);
    assert_eq!(rows[1]["state"], "Captured");
    assert_eq!(rows[2]["state"], "Unknown");
    assert_eq!(rows[2]["unknown"], "SpawnFailed");
    assert!(rows[2]["owned_pid"].is_null());
    assert!(rows[2]["usage"].is_null());
    assert_eq!(rows[3]["state"], "Captured");
    assert_eq!(rows[3]["reaps"], 1);
    let text = value.to_string();
    assert!(!text.contains("nonexistent"));
    assert!(!text.contains("python"));
}

#[test]
fn child_usage_negotiating_registry_shutdown_still_owns_cleanup() {
    let observation = Observation::with_child_usage(1);
    let source = observation.source(SourceKind::Local);
    let fixture = transport_tests::OwnerFixture::new();
    let command = fixture.command("negotiation-blocked");
    let registry = Arc::new(Processes::default());
    let other = registry.clone();
    let opening = thread::spawn(move || {
        RpcClient::open_observed(
            command,
            "0.1.0-dev",
            Arc::new(|_| {}),
            Some(&other),
            Some(source),
        )
    });
    let deadline = Instant::now() + Duration::from_secs(2);
    while registry.tracked_count_for_test() == 0 {
        assert!(Instant::now() < deadline);
        thread::yield_now();
    }
    registry.shutdown();
    assert!(opening.join().unwrap().is_err());
    let value = snapshot(&observation);
    let row = &value["child_usage"]["records"][0];
    assert_eq!(row["state"], "Captured");
    assert_eq!(row["captures"], 1);
    assert_eq!(row["reaps"], 1);
    reaped(row["owned_pid"].as_u64().unwrap() as u32);
}

#[test]
fn child_usage_capture_fault_cleanup_stays_unknown() {
    let observation = Observation::with_child_usage(1);
    let source = observation.source(SourceKind::Local);
    let mut child = owned(Some(&source), "import signal; signal.pause()");
    let pid = child.0.id();
    assert!(child.0.capture_for_test(u32::MAX, true).is_err());
    stop_child(&mut child.0);
    assert_eq!(child.0.wait().unwrap().signal(), Some(libc::SIGKILL));
    reaped(pid);
    let value = snapshot(&observation);
    let row = &value["child_usage"]["records"][0];
    assert_eq!(row["state"], "Unknown");
    assert_eq!(row["unknown"], "CaptureFailed");
    assert_eq!(row["reaps"], 1);
    assert!(row["usage"].is_null());
}

#[test]
fn child_usage_terminal_capture_then_consuming_echild_never_qualifies() {
    let observation = Observation::with_child_usage(1);
    let source = observation.source(SourceKind::Local);
    let mut child = owned(Some(&source), "raise SystemExit(7)");
    let pid = child.0.id();
    assert!(child.0.capture_for_test(pid, false).unwrap());
    let mut status = 0;
    assert_eq!(
        unsafe { libc::waitpid(pid as i32, &mut status, 0) },
        pid as i32
    );
    assert_eq!(status, 7 << 8);
    assert_eq!(
        child.0.wait().unwrap_err().raw_os_error(),
        Some(libc::ECHILD)
    );
    reaped(pid);
    let value = snapshot(&observation);
    let row = &value["child_usage"]["records"][0];
    assert_eq!(row["state"], "Unknown");
    assert_eq!(row["unknown"], "WaitFailed");
    assert_eq!(row["captures"], 1);
    assert_eq!(row["reaps"], 0);
    assert!(row["raw_wait_status"].is_null());
    assert!(row["post_owning_wait_micros"].is_null());
}

#[test]
fn child_usage_owning_wait_fault_latches_through_later_real_cleanup() {
    let observation = Observation::with_child_usage(1);
    let once = Arc::new(std::sync::atomic::AtomicBool::new(false));
    let usage =
        observation
            .child_usage_context()
            .unwrap()
            .with_hooks(crate::child_usage::TestHooks {
                wait_error_once: Some(once),
                ..Default::default()
            });
    let mut command =
        Command::new(std::env::var_os("VERANDA_TEST_PYTHON").unwrap_or_else(|| "python3".into()));
    command.args(["-c", "raise SystemExit(7)"]).process_group(0);
    let mut child =
        Guard(OwnedChild::spawn(&mut command, Some(&usage), ChildKind::ShortTool).unwrap());
    let pid = child.0.id();
    assert_eq!(child.0.wait().unwrap_err().raw_os_error(), Some(libc::EIO));
    assert_eq!(child.0.wait().unwrap().code(), Some(7));
    let snapshot = snapshot(&observation);
    let row = &snapshot["child_usage"]["records"][0];
    assert_eq!(row["state"], "Unknown");
    assert_eq!(row["unknown"], "WaitFailed");
    assert_eq!(row["captures"], 1);
    assert_eq!(row["reaps"], 1);
    assert_eq!(row["returncode"], 7);
    reaped(pid);
}

#[test]
fn child_usage_provisional_rpc_keeps_event_admission_absent() {
    let observation = Observation::with_child_usage(1);
    let fixture = transport_tests::OwnerFixture::new();
    let registry = Processes::default();
    let received = Arc::new(Mutex::new(Vec::new()));
    let events = received.clone();
    let client = RpcClient::open_tracked_attempt_usage(
        fixture.command("manager-observation"),
        "0.1.0-dev",
        Arc::new(move |event| {
            if event.sequence > 0 {
                events.lock().unwrap().push(event.receipt.is_none());
            }
        }),
        Some(&registry),
        None,
        observation.child_usage_context(),
    )
    .unwrap();
    client
        .call(
            "synthetic.observation-burst",
            "",
            json!({}),
            Duration::from_secs(2),
        )
        .unwrap();
    assert_eq!(*received.lock().unwrap(), [true, true, true]);
    client.close();
    registry.shutdown();
    assert_eq!(observation.counts().session_sources, 0);
    assert_eq!(observation.counts().received, 0);
    assert!(observation.records().is_empty());
    let snapshot = snapshot(&observation);
    let row = &snapshot["child_usage"]["records"][0];
    assert_eq!(row["kind"], "ProvisionalRpc");
    assert_eq!(row["state"], "Captured");
    assert!(row["source"].is_null());
    assert!(row["source_kind"].is_null());
    assert_eq!(row["captures"], 1);
    assert_eq!(row["reaps"], 1);
    reaped(row["owned_pid"].as_u64().unwrap() as u32);
}

#[test]
fn child_usage_bounds_none_shape_and_sealed_late_completion_are_inert() {
    let plain = Observation::new();
    let source = plain.source(SourceKind::Local);
    let before = plain.records().len();
    let mut child = owned(Some(&source), "pass");
    assert!(!child.0.has_usage());
    child.0.wait().unwrap();
    assert_eq!(plain.records().len(), before);
    let plain = snapshot(&plain);
    assert!(plain.get("child_usage").is_none());
    assert_eq!(plain["schema_version"], 1);
    assert_eq!(plain.as_object().unwrap().len(), 9);
    let observation = Observation::with_child_usage(1);
    let source = observation.source(SourceKind::Local);
    let mut first = owned(Some(&source), "import signal; signal.pause()");
    let mut second = owned(Some(&source), "pass");
    assert!(!second.0.has_usage());
    second.0.wait().unwrap();
    let sealed = snapshot(&observation);
    assert_eq!(sealed["child_usage"]["attempts"], 2);
    assert_eq!(sealed["child_usage"]["dropped"], 1);
    assert_eq!(sealed["child_usage"]["overflow"], "Overflow");
    assert_eq!(sealed["child_usage"]["records"][0]["state"], "Unknown");
    assert_eq!(sealed["child_usage"]["records"][0]["unknown"], "Unfinished");
    assert_eq!(sealed["producer_quiescence"], "Unknown");
    stop_child(&mut first.0);
    assert_eq!(snapshot(&observation), sealed);
    assert!(
        observation
            .source(SourceKind::Local)
            .child_usage_context()
            .is_none()
    );
}
