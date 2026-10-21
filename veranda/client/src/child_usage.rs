//! Optional owning-child kernel accounting. Not continuous or family RSS.
//!
//! Linux x86_64 uses the native libc ABI and raw waitid fifth-argument rusage.
//! Accounting can include already-reaped descendants; it is not own-only memory.
//! No argv, environment, executable identity, error text or payload is retained.
//! Constructor errors may hide an internally born/reaped child and stay UNKNOWN.
//! Seal freezes observations only; it neither stops children nor proves quiescence.
use crate::observation::SourceKind;
use serde::Serialize;
use std::io;
use std::process::{Child, ChildStdin, ChildStdout, Command, ExitStatus};
use std::sync::{Arc, Mutex};
use std::time::Instant;

pub const MAX_RECORDS: usize = 8192;
#[derive(Clone, Copy, Debug, Serialize, PartialEq, Eq)]
pub enum UnknownCause {
    SpawnFailed,
    CaptureFailed,
    WaitFailed,
    Unsupported,
    Unfinished,
    MissingCapture,
    Overflow,
}
#[derive(Clone, Copy, Debug, Serialize, PartialEq, Eq)]
pub enum State {
    Pending,
    Captured,
    Unknown,
}
#[derive(Clone, Copy, Debug, Serialize, PartialEq, Eq)]
pub enum ChildKind {
    RpcSession,
    ProvisionalRpc,
    HostKeyScan,
    HostKeyFingerprint,
    AgentReadiness,
    EditorExtensionProbe,
    ShortTool,
}
#[derive(Clone, Debug, Serialize)]
pub struct Usage {
    pub utime_sec: i64,
    pub utime_usec: i64,
    pub stime_sec: i64,
    pub stime_usec: i64,
    pub maxrss_kib: i64,
    pub ixrss: i64,
    pub idrss: i64,
    pub isrss: i64,
    pub minflt: i64,
    pub majflt: i64,
    pub nswap: i64,
    pub inblock: i64,
    pub oublock: i64,
    pub msgsnd: i64,
    pub msgrcv: i64,
    pub nsignals: i64,
    pub nvcsw: i64,
    pub nivcsw: i64,
}
#[derive(Clone, Debug, Serialize)]
pub struct Record {
    #[serde(skip)]
    pre_spawn: Instant,
    #[serde(skip)]
    post_owning_wait: Option<Instant>,
    pub context: u64,
    pub source: Option<u64>,
    pub source_kind: Option<SourceKind>,
    pub provenance: u64,
    pub kind: ChildKind,
    pub attempt: u64,
    pub pre_spawn_micros: Option<u64>,
    pub post_owning_wait_micros: Option<u64>,
    pub interval_start_micros: Option<u64>,
    pub interval_end_micros: Option<u64>,
    pub owned_pid: Option<u32>,
    pub terminal_pid: Option<u32>,
    pub terminal_code: Option<i32>,
    pub terminal_status: Option<i32>,
    pub usage: Option<Usage>,
    pub raw_wait_status: Option<i32>,
    pub returncode: Option<i32>,
    pub signal: Option<i32>,
    pub captures: u64,
    pub reaps: u64,
    pub state: State,
    pub unknown: Option<UnknownCause>,
    pub error_number: Option<i32>,
}
impl Record {
    pub fn pre_spawn(&self) -> Instant {
        self.pre_spawn
    }
    pub fn post_owning_wait(&self) -> Option<Instant> {
        self.post_owning_wait
    }
}
#[derive(Clone, Debug, Default, Serialize)]
pub struct Snapshot {
    pub attempts: u64,
    pub dropped: u64,
    pub overflow: Option<UnknownCause>,
    pub records: Vec<Record>,
}
struct Ledger {
    snapshot: Snapshot,
    capacity: usize,
    sealed: bool,
    origin: Instant,
    next_provenance: u64,
}
#[derive(Clone)]
pub(crate) struct Collector(Arc<Mutex<Ledger>>);
/// Usage-only provenance: no RPC source/ticket/admission is created.
#[derive(Clone)]
pub(crate) struct UsageContext {
    collector: Collector,
    context: u64,
    source: Option<(u64, SourceKind)>,
    ordinal: u64,
    #[cfg(test)]
    hooks: TestHooks,
}
impl UsageContext {
    fn begin(&self, kind: ChildKind) -> Option<Attempt> {
        self.collector.begin(
            self.context,
            self.ordinal,
            self.source,
            kind,
            Instant::now(),
        )
    }
    #[cfg(test)]
    pub(crate) fn with_hooks(mut self, hooks: TestHooks) -> Self {
        self.hooks = hooks;
        self
    }
}
#[cfg(test)]
#[derive(Clone, Default)]
pub(crate) struct TestHooks {
    pub wait_error_once: Option<Arc<std::sync::atomic::AtomicBool>>,
    pub spawn: Option<Arc<dyn Fn(&mut Command) -> io::Result<Child> + Send + Sync>>,
    pub capture_pid: Option<u32>,
    pub before_wait: Option<Arc<dyn Fn(u32) + Send + Sync>>,
    pub after_live: Option<Arc<dyn Fn(u32) + Send + Sync>>,
    pub spawned: Option<std::sync::mpsc::Sender<u32>>,
}
pub(crate) struct Attempt {
    collector: Collector,
    index: usize,
    terminal: bool,
    failed: bool,
    reaped: bool,
}
impl Collector {
    pub(crate) fn new(capacity: usize, origin: Instant) -> Self {
        Self(Arc::new(Mutex::new(Ledger {
            snapshot: Snapshot::default(),
            capacity: capacity.min(MAX_RECORDS),
            sealed: false,
            origin,
            next_provenance: 0,
        })))
    }
    pub(crate) fn provenance(
        &self,
        context: u64,
        source: Option<(u64, SourceKind)>,
    ) -> Option<UsageContext> {
        let mut ledger = self.0.lock().unwrap();
        if ledger.sealed {
            return None;
        }
        let Some(ordinal) = ledger.next_provenance.checked_add(1) else {
            ledger.snapshot.overflow = Some(UnknownCause::Overflow);
            ledger.snapshot.dropped = ledger.snapshot.dropped.saturating_add(1);
            return None;
        };
        ledger.next_provenance = ordinal;
        Some(UsageContext {
            collector: self.clone(),
            context,
            source,
            ordinal,
            #[cfg(test)]
            hooks: TestHooks::default(),
        })
    }
    pub(crate) fn begin(
        &self,
        context: u64,
        provenance: u64,
        source: Option<(u64, SourceKind)>,
        kind: ChildKind,
        started: Instant,
    ) -> Option<Attempt> {
        let mut ledger = self.0.lock().unwrap();
        if ledger.sealed {
            return None;
        }
        ledger.snapshot.attempts = ledger.snapshot.attempts.saturating_add(1);
        if ledger.snapshot.records.len() >= ledger.capacity {
            ledger.snapshot.dropped = ledger.snapshot.dropped.saturating_add(1);
            ledger.snapshot.overflow = Some(UnknownCause::Overflow);
            return None;
        }
        let index = ledger.snapshot.records.len();
        let attempt = ledger.snapshot.attempts;
        let pre_spawn_micros = micros(started, ledger.origin);
        let interval_start_micros = outward_micros(started, ledger.origin, false);
        ledger.snapshot.records.push(Record {
            pre_spawn: started,
            post_owning_wait: None,
            context,
            source: source.map(|s| s.0),
            source_kind: source.map(|s| s.1),
            provenance,
            kind,
            attempt,
            pre_spawn_micros,
            post_owning_wait_micros: None,
            interval_start_micros,
            interval_end_micros: None,
            owned_pid: None,
            terminal_pid: None,
            terminal_code: None,
            terminal_status: None,
            usage: None,
            raw_wait_status: None,
            returncode: None,
            signal: None,
            captures: 0,
            reaps: 0,
            state: State::Pending,
            unknown: None,
            error_number: None,
        });
        Some(Attempt {
            collector: self.clone(),
            index,
            terminal: false,
            failed: false,
            reaped: false,
        })
    }
    pub(crate) fn seal(&self) -> Snapshot {
        let mut ledger = self.0.lock().unwrap();
        if !ledger.sealed {
            ledger.sealed = true;
            for record in &mut ledger.snapshot.records {
                if record.state == State::Pending {
                    record.state = State::Unknown;
                    record.unknown = Some(UnknownCause::Unfinished);
                }
            }
        }
        ledger.snapshot.clone()
    }
}
fn micros(at: Instant, origin: Instant) -> Option<u64> {
    at.checked_duration_since(origin)
        .map(|d| d.as_micros().min(u64::MAX as u128) as u64)
}
fn outward_micros(at: Instant, origin: Instant, end: bool) -> Option<u64> {
    duration_micros(at.checked_duration_since(origin)?, end)
}
fn duration_micros(duration: std::time::Duration, end: bool) -> Option<u64> {
    let nanos = duration.as_nanos();
    let rounded = if end { nanos.checked_add(999)? } else { nanos };
    u64::try_from(rounded / 1000).ok()
}
#[cfg(test)]
mod interval_tests {
    use super::*;
    #[test]
    fn interval_bounds_round_outward_without_clock_or_integer_clamping() {
        use std::time::Duration;
        let origin = Instant::now();
        for (nanos, floor, ceil) in [
            (0, 0, 0),
            (1, 0, 1),
            (999, 0, 1),
            (1000, 1, 1),
            (1001, 1, 2),
        ] {
            let at = origin + Duration::from_nanos(nanos);
            assert_eq!(outward_micros(at, origin, false), Some(floor));
            assert_eq!(outward_micros(at, origin, true), Some(ceil));
        }
        assert_eq!(
            outward_micros(origin, origin + Duration::from_nanos(1), false),
            None
        );
        assert_eq!(duration_micros(Duration::from_secs(u64::MAX), true), None);
    }
}
impl Attempt {
    pub(crate) fn active(&self) -> bool {
        !self.collector.0.lock().unwrap().sealed
    }
    pub(crate) fn supported(&self) -> bool {
        cfg!(all(target_os = "linux", target_arch = "x86_64"))
    }
    fn update(&self, update: impl FnOnce(&mut Record)) {
        let mut ledger = self.collector.0.lock().unwrap();
        if !ledger.sealed {
            update(&mut ledger.snapshot.records[self.index]);
        }
    }
    pub(crate) fn spawned(&self, pid: u32) {
        self.update(|r| r.owned_pid = Some(pid));
    }
    pub(crate) fn fail(&mut self, cause: UnknownCause, error: Option<i32>) {
        self.failed = true;
        self.update(|r| {
            r.state = State::Unknown;
            r.unknown = Some(cause);
            r.error_number = error;
        });
    }
    pub(crate) fn capture(&mut self, pid: u32, nohang: bool) -> io::Result<bool> {
        if self.terminal {
            return Ok(true);
        }
        if self.failed || !self.active() {
            return Ok(false);
        }
        if !self.supported() {
            self.fail(UnknownCause::Unsupported, None);
            return Ok(false);
        }
        match raw_terminal(pid, nohang) {
            Ok(None) => Ok(false),
            Ok(Some((code, status, usage))) => {
                self.terminal = true;
                self.update(|r| {
                    r.terminal_pid = Some(pid);
                    r.terminal_code = Some(code);
                    r.terminal_status = Some(status);
                    r.usage = Some(usage);
                    r.captures = 1;
                });
                Ok(true)
            }
            Err(error) => {
                self.fail(UnknownCause::CaptureFailed, error.raw_os_error());
                Err(error)
            }
        }
    }
    pub(crate) fn finish(&mut self, result: &io::Result<ExitStatus>, finished: Instant) {
        if self.reaped {
            return;
        }
        match result {
            Ok(status) => {
                self.reaped = true;
                let origin = self.collector.0.lock().unwrap().origin;
                let qualified = self.terminal && !self.failed;
                self.update(|r| {
                    r.post_owning_wait = Some(finished);
                    r.post_owning_wait_micros = micros(finished, origin);
                    r.interval_end_micros = outward_micros(finished, origin, true);
                    r.returncode = status.code();
                    r.reaps = 1;
                    #[cfg(unix)]
                    {
                        use std::os::unix::process::ExitStatusExt;
                        r.signal = status.signal();
                        r.raw_wait_status = Some(status.into_raw());
                    }
                    if qualified {
                        r.state = State::Captured;
                    } else if !self.failed {
                        r.state = State::Unknown;
                        r.unknown = Some(UnknownCause::MissingCapture);
                    }
                });
            }
            Err(error) => self.fail(UnknownCause::WaitFailed, error.raw_os_error()),
        }
    }
}
/// The sole consuming wait surface shared by RPC leaders and short helpers.
/// No production API exposes the inner Child or a second reaper.
pub(crate) struct OwnedChild {
    child: Child,
    usage: Option<Attempt>,
    #[cfg(test)]
    hooks: TestHooks,
}
impl OwnedChild {
    pub(crate) fn spawn(
        command: &mut Command,
        context: Option<&UsageContext>,
        kind: ChildKind,
    ) -> io::Result<Self> {
        let mut usage = context.and_then(|context| context.begin(kind));
        #[cfg(test)]
        let spawned = match context.and_then(|context| context.hooks.spawn.as_ref()) {
            Some(spawn) => spawn(command),
            None => command.spawn(),
        };
        #[cfg(not(test))]
        let spawned = command.spawn();
        let child = match spawned {
            Ok(child) => child,
            Err(error) => {
                if let Some(usage) = usage.as_mut() {
                    usage.fail(UnknownCause::SpawnFailed, None);
                }
                return Err(error);
            }
        };
        if let Some(usage) = usage.as_ref() {
            usage.spawned(child.id());
        }
        #[cfg(test)]
        let hooks = context
            .map(|context| context.hooks.clone())
            .unwrap_or_default();
        #[cfg(test)]
        if let Some(sender) = hooks.spawned.as_ref() {
            let _ = sender.send(child.id());
        }
        Ok(Self {
            child,
            usage,
            #[cfg(test)]
            hooks,
        })
    }
    pub(crate) fn id(&self) -> u32 {
        self.child.id()
    }
    pub(crate) fn take_stdin(&mut self) -> Option<ChildStdin> {
        self.child.stdin.take()
    }
    pub(crate) fn take_stdout(&mut self) -> Option<ChildStdout> {
        self.child.stdout.take()
    }
    pub(crate) fn kill(&mut self) -> io::Result<()> {
        self.child.kill()
    }
    fn capture(&mut self, nohang: bool) -> io::Result<bool> {
        let pid = self.id();
        #[cfg(test)]
        let pid = self.hooks.capture_pid.unwrap_or(pid);
        let Some(usage) = self.usage.as_mut() else {
            return Ok(false);
        };
        let result = usage.capture(pid, nohang);
        #[cfg(test)]
        if result.as_ref().is_ok_and(|exited| !exited) {
            if let Some(hook) = self.hooks.after_live.as_ref() {
                hook(self.id());
            }
        }
        result
    }
    pub(crate) fn wait(&mut self) -> io::Result<ExitStatus> {
        if self.usage.is_none() {
            return self.child.wait();
        }
        if self.usage.as_ref().is_some_and(Attempt::active) {
            let _ = self.capture(false);
        }
        #[cfg(test)]
        if let Some(hook) = self.hooks.before_wait.as_ref() {
            hook(self.id());
        }
        #[cfg(test)]
        let result = if self
            .hooks
            .wait_error_once
            .as_ref()
            .is_some_and(|once| !once.swap(true, std::sync::atomic::Ordering::SeqCst))
        {
            Err(io::Error::from_raw_os_error(libc::EIO))
        } else {
            self.child.wait()
        };
        #[cfg(not(test))]
        let result = self.child.wait();
        let finished = Instant::now();
        self.usage.as_mut().unwrap().finish(&result, finished);
        result
    }
    pub(crate) fn exited(&mut self) -> bool {
        #[cfg(all(target_os = "linux", target_arch = "x86_64"))]
        if self
            .usage
            .as_ref()
            .is_some_and(|usage| usage.active() && usage.supported() && !usage.failed)
        {
            if let Ok(exited) = self.capture(true) {
                return exited;
            }
            // Capture faults must not alter the ordinary nonreaping detector.
        }
        #[cfg(unix)]
        {
            let mut info: libc::siginfo_t = unsafe { std::mem::zeroed() };
            let result = unsafe {
                libc::waitid(
                    libc::P_PID,
                    self.id(),
                    &mut info,
                    libc::WEXITED | libc::WNOHANG | libc::WNOWAIT,
                )
            };
            result != 0 || unsafe { info.si_pid() } != 0
        }
        #[cfg(not(unix))]
        {
            matches!(self.child.try_wait(), Ok(Some(_)))
        }
    }
    #[cfg(test)]
    pub(crate) fn has_usage(&self) -> bool {
        self.usage.is_some()
    }
    #[cfg(test)]
    pub(crate) fn capture_for_test(&mut self, pid: u32, nohang: bool) -> io::Result<bool> {
        self.usage.as_mut().unwrap().capture(pid, nohang)
    }
}

#[cfg(all(target_os = "linux", target_arch = "x86_64"))]
fn raw_terminal(pid: u32, nohang: bool) -> io::Result<Option<(i32, i32, Usage)>> {
    let mut info: libc::siginfo_t = unsafe { std::mem::zeroed() };
    let mut usage: libc::rusage = unsafe { std::mem::zeroed() };
    let flags = libc::WEXITED | libc::WNOWAIT | if nohang { libc::WNOHANG } else { 0 };
    let result = unsafe {
        libc::syscall(
            libc::SYS_waitid,
            libc::P_PID,
            pid,
            &mut info as *mut libc::siginfo_t,
            flags,
            &mut usage as *mut libc::rusage,
        )
    };
    if result != 0 {
        return Err(io::Error::last_os_error());
    }
    let terminal_pid = unsafe { info.si_pid() };
    if nohang && terminal_pid == 0 {
        return Ok(None);
    }
    if terminal_pid != pid as i32
        || info.si_signo != libc::SIGCHLD
        || info.si_errno != 0
        || ![libc::CLD_EXITED, libc::CLD_KILLED, libc::CLD_DUMPED].contains(&info.si_code)
    {
        return Err(io::Error::from_raw_os_error(libc::EPROTO));
    }
    Ok(Some((
        info.si_code,
        unsafe { info.si_status() },
        Usage {
            utime_sec: usage.ru_utime.tv_sec,
            utime_usec: usage.ru_utime.tv_usec,
            stime_sec: usage.ru_stime.tv_sec,
            stime_usec: usage.ru_stime.tv_usec,
            maxrss_kib: usage.ru_maxrss,
            ixrss: usage.ru_ixrss,
            idrss: usage.ru_idrss,
            isrss: usage.ru_isrss,
            minflt: usage.ru_minflt,
            majflt: usage.ru_majflt,
            nswap: usage.ru_nswap,
            inblock: usage.ru_inblock,
            oublock: usage.ru_oublock,
            msgsnd: usage.ru_msgsnd,
            msgrcv: usage.ru_msgrcv,
            nsignals: usage.ru_nsignals,
            nvcsw: usage.ru_nvcsw,
            nivcsw: usage.ru_nivcsw,
        },
    )))
}
#[cfg(not(all(target_os = "linux", target_arch = "x86_64")))]
fn raw_terminal(_: u32, _: bool) -> io::Result<Option<(i32, i32, Usage)>> {
    Err(io::Error::from(io::ErrorKind::Unsupported))
}
