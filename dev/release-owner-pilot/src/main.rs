#![forbid(unsafe_code)]
mod store;
use release_owner_pilot::{
    canonical, digest,
    domain::{Phase, Prepared, Record},
    Failure, Result,
};
use rustix::{
    fs::{self, FlockOperation, Mode, OFlags},
    io::{fcntl_setfd, FdFlags},
};
use serde::Deserialize;
use serde_json::{json, Value};
use std::{
    fs::File,
    io::{BufRead, Read, Write},
    os::fd::{AsRawFd, OwnedFd},
    path::Path,
    process::{Command, Stdio},
    thread,
    time::{Duration, Instant, SystemTime, UNIX_EPOCH},
};
fn now() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs()
}
fn event(value: Value) {
    println!("{value}");
    let _ = std::io::stdout().flush();
}
fn cut(selected: &str, point: &str) {
    if selected == point {
        event(json!({"event":"cut","point":point}));
        thread::sleep(Duration::from_secs(60));
    }
}
struct Native {
    root: OwnedFd,
    artifact: OwnedFd,
    hash: String,
    pause: String,
}
impl Native {
    fn call(&self, op: &str, yard: &str, recipe: Option<&Value>) -> Result<Value> {
        // Only the root OFD and exact native artifact are inherited; store descriptors
        // remain CLOEXEC. The native component never explicitly unlocks the root.
        let request = json!({"version":1,"op":op,"yard":yard,"recipe":recipe,"pause":if op=="apply"{self.pause.as_str()}else{""}});
        let payload = canonical(&request)?;
        if payload.len() > 1_048_576 {
            return Err(Failure("request-bound"));
        }
        let path = format!("/proc/self/fd/{}", self.artifact.as_raw_fd());
        let control = if op == "apply" && !self.pause.is_empty() {
            let fd = rustix::io::dup(std::io::stdin()).map_err(|_| Failure("control-dup"))?;
            fcntl_setfd(&fd, FdFlags::empty()).map_err(|_| Failure("control-inherit"))?;
            Some(fd)
        } else {
            None
        };
        let control_arg = control
            .as_ref()
            .map(|fd| fd.as_raw_fd().to_string())
            .unwrap_or_else(|| "-1".into());
        let mut child = Command::new(path)
            .arg(self.root.as_raw_fd().to_string())
            .arg(control_arg)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()
            .map_err(|_| Failure("native-spawn"))?;
        event(json!({"event":"native","pid":child.id(),"op":op}));
        let mut stdin = child.stdin.take().ok_or(Failure("native-stdin"))?;
        if stdin.write_all(&payload).is_err() {
            let _ = child.kill();
            let _ = child.wait();
            return Err(Failure("native-write"));
        };
        drop(stdin);
        // Native stdout has a fixed 1MiB contract. Read concurrently so a bounded
        // response cannot fill the pipe and deadlock the custodian's reap loop.
        let stdout = child.stdout.take().ok_or(Failure("native-stdout"))?;
        let reader = thread::spawn(move || {
            let mut bytes = vec![];
            let read = stdout.take(1_048_577).read_to_end(&mut bytes);
            (bytes, read)
        });
        let stderr = child.stderr.take().ok_or(Failure("native-stderr"))?;
        let pid = child.id();
        let expected_pause = self.pause.clone();
        let errors = thread::spawn(move || {
            let mut bounded = String::new();
            let mut reader = std::io::BufReader::new(stderr.take(256));
            while reader.read_line(&mut bounded).unwrap_or(0) > 0 {
                if !expected_pause.is_empty()
                    && bounded.trim_end() == format!("native-cut:{expected_pause}")
                {
                    event(json!({"event":"native_cut","point":expected_pause,"pid":pid}));
                }
                bounded.clear();
            }
        });
        let started = Instant::now();
        let status = loop {
            match child.try_wait() {
                Ok(Some(status)) => break status,
                Ok(None) if started.elapsed() < Duration::from_secs(5) => {
                    thread::sleep(Duration::from_millis(10))
                }
                _ => {
                    let _ = child.kill();
                    let _ = child.wait();
                    let _ = reader.join();
                    let _ = errors.join();
                    return Err(Failure("native-drained"));
                }
            }
        };
        let _ = errors.join();
        let (bytes, read) = reader.join().map_err(|_| Failure("native-reader"))?;
        if !status.success() || read.is_err() || bytes.len() > 1_048_576 {
            return Err(Failure("native-refused"));
        }
        let typed: release_owner_pilot::protocol::Response =
            serde_json::from_slice(&bytes).map_err(|_| Failure("native-codec"))?;
        if canonical(&typed)? != bytes {
            return Err(Failure("native-canonical"));
        }
        let response = serde_json::to_value(typed).map_err(|_| Failure("native-codec"))?;
        if response.get("version") != Some(&json!(1)) {
            return Err(Failure("native-version"));
        };
        Ok(response)
    }
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Consent {
    version: u32,
    consent: String,
}
fn run() -> Result<()> {
    let args: Vec<_> = std::env::args().collect();
    if args.len() != 6 {
        return Err(Failure("arguments"));
    }
    let (root_path, helper, yard, mode, selected) =
        (&args[1], &args[2], &args[3], &args[4], &args[5]);
    let exact_root = Path::new(root_path)
        .file_name()
        .and_then(|name| name.to_str())
        .is_some_and(|name| root_path == &format!("/tmp/{name}"));
    if !exact_root
        || !["new", "resume"].contains(&mode.as_str())
        || !Path::new(root_path)
            .parent()
            .is_some_and(|p| p == Path::new("/tmp"))
        || !Path::new(root_path)
            .file_name()
            .is_some_and(|n| n.to_string_lossy().starts_with("subyard-owner-"))
    {
        return Err(Failure("fixture-root"));
    }
    if ![
        "none",
        "before_intent",
        "after_intent",
        "after_verify",
        "before_cas",
        "after_cas",
    ]
    .contains(&selected.as_str())
    {
        return Err(Failure("cut"));
    }
    let root = fs::open(
        root_path,
        OFlags::RDONLY | OFlags::DIRECTORY | OFlags::NOFOLLOW | OFlags::CLOEXEC,
        Mode::empty(),
    )
    .map_err(|_| Failure("root"))?;
    store::checked(&root, true, Some(0o700))?;
    let marker = store::open(&root, "owner.marker", OFlags::RDONLY | OFlags::NONBLOCK)?;
    store::checked(&marker, false, Some(0o600))?;
    let mut marker_bytes = Vec::new();
    File::from(marker)
        .take(128)
        .read_to_end(&mut marker_bytes)
        .map_err(|_| Failure("marker"))?;
    if marker_bytes != b"subyard-release-owner-pilot-v1\n" {
        return Err(Failure("marker"));
    }
    let artifact = fs::open(
        helper,
        OFlags::RDONLY | OFlags::NONBLOCK | OFlags::NOFOLLOW | OFlags::CLOEXEC,
        Mode::empty(),
    )
    .map_err(|_| Failure("artifact"))?;
    store::checked(&artifact, false, None)?;
    let mut bytes = vec![];
    File::from(rustix::io::dup(&artifact).map_err(|_| Failure("artifact-dup"))?)
        .take((64 << 20) + 1)
        .read_to_end(&mut bytes)
        .map_err(|_| Failure("artifact-read"))?;
    if bytes.len() > 64 << 20 {
        return Err(Failure("artifact-bound"));
    }
    let hash = digest(&bytes);
    fcntl_setfd(&root, FdFlags::empty()).map_err(|_| Failure("inherit"))?;
    fcntl_setfd(&artifact, FdFlags::empty()).map_err(|_| Failure("inherit"))?;
    let native = Native {
        root,
        artifact,
        hash,
        pause: if ["before_cas", "after_cas"].contains(&selected.as_str()) {
            selected.clone()
        } else {
            String::new()
        },
    };
    let owner = digest(&std::fs::read("/proc/self/exe").map_err(|_| Failure("owner-artifact"))?);
    fs::flock(&native.root, FlockOperation::LockExclusive).map_err(|_| Failure("lock"))?;
    let store = store::Store::new(&native.root)?;
    let mut record: Record;
    if mode == "new" {
        if store.read()?.is_some() {
            return Err(Failure("replay"));
        }
        let response = native.call("prepare", yard, None)?;
        let recipe = response.get("recipe").ok_or(Failure("recipe"))?.clone();
        let prepared = Prepared::new(
            owner.clone(),
            native.hash.clone(),
            yard.clone(),
            recipe,
            now(),
        )?;
        fs::flock(&native.root, FlockOperation::Unlock).map_err(|_| Failure("unlock"))?;
        event(json!({"event":"prepared","plan":prepared.plan()}));
        let mut line = String::new();
        std::io::stdin()
            .lock()
            .take(4097)
            .read_line(&mut line)
            .map_err(|_| Failure("consent-read"))?;
        if line.len() > 4096 {
            return Err(Failure("consent-bound"));
        }
        let consent: Consent = serde_json::from_str(&line).map_err(|_| Failure("consent-codec"))?;
        if consent.version != 1 {
            return Err(Failure("consent-version"));
        }
        record = prepared.consent(&consent.consent, now())?.consume();
        fs::flock(&native.root, FlockOperation::LockExclusive).map_err(|_| Failure("lock"))?;
        if store.read()?.is_some() {
            return Err(Failure("replay"));
        }
        if native.call("observe", yard, Some(&record.recipe))?["status"] != "expected" {
            return Err(Failure("stale"));
        }
        record.check_fresh(now())?;
        cut(selected, "before_intent");
        record.check_fresh(now())?;
        store.write(&canonical(&record)?)?;
        cut(selected, "after_intent");
    } else {
        let bytes = store.read()?.ok_or(Failure("no-intent"))?;
        record = serde_json::from_slice(&bytes).map_err(|_| Failure("record-codec"))?;
        if canonical(&record)? != bytes {
            return Err(Failure("record-canonical"));
        }
    }
    record.check(&owner, &native.hash, yard)?;
    let observed = native.call("observe", yard, Some(&record.recipe))?;
    match observed["status"].as_str() {
        Some("desired") => {}
        Some("expected") if record.phase == Phase::Intent => {
            if native.call("apply", yard, Some(&record.recipe))?["status"] != "desired" {
                return Err(Failure("effect-unverified"));
            }
        }
        _ => return Err(Failure("conflict")),
    }
    if native.call("observe", yard, Some(&record.recipe))?["status"] != "desired" {
        return Err(Failure("verify"));
    }
    if record.phase != Phase::Verified {
        record.phase = Phase::Verified;
        store.write(&canonical(&record)?)?
    }
    cut(selected, "after_verify");
    event(json!({"event":"verified","transaction":record.transaction}));
    // Every call has reaped its native process before this final release.
    fs::flock(&native.root, FlockOperation::Unlock).map_err(|_| Failure("unlock"))?;
    Ok(())
}
fn main() {
    if let Err(error) = run() {
        eprintln!("release-owner-pilot: {}", error.0);
        std::process::exit(1)
    }
}
