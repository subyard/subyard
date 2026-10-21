//! Explicit run-owned observation and post-app-return private scalar output.
use serde::Serialize;
use std::{
    ffi::OsString,
    io::{self, Write},
    path::{Component, Path, PathBuf},
};
use veranda_lib::observation::{CleanupObservation, Observation, Snapshot};

pub const ENV_NAME: &str = "SUBYARD_VERANDA_TRACE";
pub const CORRELATION_ENV: &str = "SUBYARD_VERANDA_CORRELATION_CONFIG";
pub const OBSERVATION_ENV: &str = "SUBYARD_VERANDA_OBSERVATION_CONFIG";
const MAX_BYTES: usize = 8 * 1024 * 1024;
#[derive(Clone, Copy, Debug, Serialize, PartialEq, Eq)]
pub enum RendererKind {
    Cairo,
    GL,
    Vulkan,
    Other,
    Unknown,
}
#[derive(Clone, Copy, Debug, Serialize, PartialEq, Eq)]
pub enum SurfaceBackend {
    Wayland,
    X11,
    Unknown,
}
#[derive(Clone, Debug, Serialize)]
pub struct RendererRecord {
    ordinal: u64,
    renderer: RendererKind,
    backend: SurfaceBackend,
}
#[derive(Clone, Debug, Default, Serialize)]
pub struct RendererSnapshot {
    records: Vec<RendererRecord>,
    dropped: u64,
}
#[derive(Default)]
struct RendererState {
    snapshot: RendererSnapshot,
    sealed: bool,
}
#[derive(Clone, Default)]
struct Renderers(std::sync::Arc<std::sync::Mutex<RendererState>>);
impl Renderers {
    fn begin(&self) -> Option<usize> {
        let mut state = self.0.lock().unwrap();
        if state.sealed {
            return None;
        }
        if state.snapshot.records.len() >= 64 {
            state.snapshot.dropped += 1;
            return None;
        }
        let index = state.snapshot.records.len();
        state.snapshot.records.push(RendererRecord {
            ordinal: index as u64 + 1,
            renderer: RendererKind::Unknown,
            backend: SurfaceBackend::Unknown,
        });
        Some(index)
    }
    fn observed(&self, index: usize, renderer: Option<&str>, surface: Option<&str>) {
        let mut state = self.0.lock().unwrap();
        if state.sealed {
            return;
        }
        if let Some(row) = state.snapshot.records.get_mut(index) {
            row.renderer = match renderer {
                Some("GskCairoRenderer") => RendererKind::Cairo,
                Some("GskGLRenderer" | "GskNglRenderer") => RendererKind::GL,
                Some("GskVulkanRenderer") => RendererKind::Vulkan,
                Some(_) => RendererKind::Other,
                None => RendererKind::Unknown,
            };
            row.backend = match surface {
                Some("GdkWaylandToplevel") => SurfaceBackend::Wayland,
                Some("GdkX11Toplevel") => SurfaceBackend::X11,
                _ => SurfaceBackend::Unknown,
            };
        }
    }
    fn seal(&self) -> RendererSnapshot {
        let mut state = self.0.lock().unwrap();
        state.sealed = true;
        state.snapshot.clone()
    }
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum SinkError {
    UnsafePath,
    Io,
    Bound,
    Unsupported,
}
pub struct TraceRun {
    path: PathBuf,
    observation: Observation,
    usage: bool,
    renderers: Option<Renderers>,
    #[cfg(target_os = "linux")]
    correlation: Option<crate::event_correlation::RuntimeRun>,
}
#[derive(Clone)]
pub struct TraceView {
    pub observation: Observation,
    renderers: Option<Renderers>,
    #[cfg(target_os = "linux")]
    correlation: Option<crate::event_correlation::RuntimeHandle>,
}
impl TraceView {
    /// Only requested v3 installs a callback; no realization or redraw is forced.
    #[cfg(feature = "desktop")]
    pub fn observe_native(&self, window: &gtk::ApplicationWindow) {
        use gtk::prelude::*;
        let Some(registry) = self.renderers.clone() else {
            return;
        };
        let Some(index) = registry.begin() else {
            return;
        };
        let sampled = std::cell::Cell::new(false);
        window.connect_map(move |window| {
            if sampled.replace(true) {
                return;
            }
            let renderer = window.renderer();
            let surface = window.surface();
            registry.observed(
                index,
                renderer.as_ref().map(|r| r.type_().name()),
                surface.as_ref().map(|s| s.type_().name()),
            );
        });
    }
    pub fn coordinator(self) -> crate::view_observation::ViewObservation {
        #[cfg(target_os = "linux")]
        if let Some(runtime) = self.correlation {
            return crate::view_observation::ViewObservation::with_runtime(
                self.observation,
                runtime,
            );
        }
        crate::view_observation::ViewObservation::new(self.observation)
    }
}
impl TraceRun {
    /// None performs no filesystem work and constructs no observation ledger.
    pub fn configured(output: Option<OsString>) -> Option<Self> {
        output.map(|path| Self {
            path: path.into(),
            observation: Observation::new(),
            usage: false,
            renderers: None,
            #[cfg(target_os = "linux")]
            correlation: None,
        })
    }
    pub fn configured_runtime(output: OsString, config: OsString) -> Result<Self, SinkError> {
        #[cfg(target_os = "linux")]
        {
            let path = PathBuf::from(output);
            let plan = crate::event_correlation::RuntimePlan::load(Path::new(&config), &path)
                .map_err(|_| SinkError::UnsafePath)?;
            let (observation, correlation) = plan.start().map_err(|_| SinkError::Io)?;
            Ok(Self {
                path,
                observation,
                usage: false,
                renderers: None,
                correlation: Some(correlation),
            })
        }
        #[cfg(not(target_os = "linux"))]
        {
            let _ = (output, config);
            Err(SinkError::Unsupported)
        }
    }
    pub fn view(&self) -> TraceView {
        TraceView {
            observation: self.observation.clone(),
            renderers: self.renderers.clone(),
            #[cfg(target_os = "linux")]
            correlation: self.correlation.as_ref().map(|runtime| runtime.handle()),
        }
    }
    pub fn configured_observation(output: OsString, config: OsString) -> Result<Self, SinkError> {
        #[cfg(target_os = "linux")]
        {
            let path = PathBuf::from(output);
            let plan = crate::event_correlation::ObservationPlan::load(Path::new(&config), &path)
                .map_err(|_| SinkError::UnsafePath)?;
            let (observation, correlation) = plan.start().map_err(|_| SinkError::Io)?;
            Ok(Self {
                path,
                observation,
                usage: true,
                renderers: Some(Renderers::default()),
                correlation,
            })
        }
        #[cfg(not(target_os = "linux"))]
        {
            let _ = (output, config);
            Err(SinkError::Unsupported)
        }
    }
    pub fn observation(&self) -> Observation {
        self.observation.clone()
    }
    /// Call only after app.run returns. This seals the window, not producers.
    pub fn finish(mut self, exit_success: bool) -> Result<(), SinkError> {
        #[cfg(target_os = "linux")]
        let correlation = self.correlation.take().map(|runtime| runtime.finish());
        let snapshot = self.observation.seal();
        let renderer = self.renderers.take().map(|value| value.seal());
        #[cfg(target_os = "linux")]
        if self.usage {
            return write_new(
                &self.path,
                &encode_usage(
                    &snapshot,
                    exit_success,
                    correlation.as_ref(),
                    renderer.as_ref(),
                )?,
            );
        }
        #[cfg(target_os = "linux")]
        if let Some(correlation) = correlation {
            return write_new(
                &self.path,
                &encode_correlated(&snapshot, exit_success, &correlation)?,
            );
        }
        let bytes = encode(&snapshot, exit_success, MAX_BYTES)?;
        write_new(&self.path, &bytes)
    }
}
#[cfg(target_os = "linux")]
fn encode_usage(
    snapshot: &Snapshot,
    exit_success: bool,
    correlation: Option<&crate::event_correlation::RuntimeEnvelope>,
    renderer: Option<&RendererSnapshot>,
) -> Result<Vec<u8>, SinkError> {
    #[derive(Serialize)]
    struct UsageOutput<'a> {
        schema_version: u8,
        app_run_returned: bool,
        exit_success: bool,
        run_lifecycle: CleanupObservation,
        snapshot: &'a Snapshot,
        correlation_run: Option<&'a crate::event_correlation::RuntimeEnvelope>,
        renderer_observation: Option<&'a RendererSnapshot>,
    }
    let output = UsageOutput {
        schema_version: 3,
        app_run_returned: true,
        exit_success,
        run_lifecycle: if exit_success {
            snapshot.worker_cleanup()
        } else {
            CleanupObservation::Unknown
        },
        snapshot,
        correlation_run: correlation,
        renderer_observation: renderer,
    };
    let mut bounded = Bounded {
        bytes: Vec::new(),
        limit: MAX_BYTES,
    };
    serde_json::to_writer(&mut bounded, &output).map_err(|_| SinkError::Bound)?;
    bounded.write_all(b"\n").map_err(|_| SinkError::Bound)?;
    Ok(bounded.bytes)
}
#[cfg(target_os = "linux")]
fn encode_correlated(
    snapshot: &Snapshot,
    exit_success: bool,
    correlation: &crate::event_correlation::RuntimeEnvelope,
) -> Result<Vec<u8>, SinkError> {
    #[derive(Serialize)]
    struct Correlated<'a> {
        schema_version: u8,
        app_run_returned: bool,
        exit_success: bool,
        run_lifecycle: CleanupObservation,
        snapshot: &'a Snapshot,
        correlation_run: &'a crate::event_correlation::RuntimeEnvelope,
    }
    let output = Correlated {
        schema_version: 2,
        app_run_returned: true,
        exit_success,
        run_lifecycle: if exit_success {
            snapshot.worker_cleanup()
        } else {
            CleanupObservation::Unknown
        },
        snapshot,
        correlation_run: correlation,
    };
    let mut bounded = Bounded {
        bytes: Vec::new(),
        limit: MAX_BYTES,
    };
    serde_json::to_writer(&mut bounded, &output).map_err(|_| SinkError::Bound)?;
    bounded.write_all(b"\n").map_err(|_| SinkError::Bound)?;
    Ok(bounded.bytes)
}
#[derive(Serialize)]
struct Output<'a> {
    schema_version: u8,
    app_run_returned: bool,
    exit_success: bool,
    run_lifecycle: CleanupObservation,
    snapshot: &'a Snapshot,
}
struct Bounded {
    bytes: Vec<u8>,
    limit: usize,
}
impl Write for Bounded {
    fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
        if bytes.len() > self.limit.saturating_sub(self.bytes.len()) {
            return Err(io::ErrorKind::FileTooLarge.into());
        }
        self.bytes.extend_from_slice(bytes);
        Ok(bytes.len())
    }
    fn flush(&mut self) -> io::Result<()> {
        Ok(())
    }
}
fn encode(snapshot: &Snapshot, exit_success: bool, limit: usize) -> Result<Vec<u8>, SinkError> {
    let output = Output {
        schema_version: 1,
        app_run_returned: true,
        exit_success,
        run_lifecycle: if exit_success {
            snapshot.worker_cleanup()
        } else {
            CleanupObservation::Unknown
        },
        snapshot,
    };
    let mut bounded = Bounded {
        bytes: Vec::new(),
        limit,
    };
    serde_json::to_writer(&mut bounded, &output).map_err(|_| SinkError::Bound)?;
    bounded.write_all(b"\n").map_err(|_| SinkError::Bound)?;
    Ok(bounded.bytes)
}
fn persist(file: &mut std::fs::File, bytes: &[u8]) -> Result<(), SinkError> {
    file.write_all(bytes)
        .and_then(|()| file.sync_all())
        .map_err(|_| SinkError::Io)
}
#[cfg(target_os = "linux")]
fn write_new(path: &Path, bytes: &[u8]) -> Result<(), SinkError> {
    use std::{
        ffi::CString,
        fs::File,
        os::{
            fd::{AsRawFd, FromRawFd},
            unix::{
                ffi::OsStrExt,
                fs::{MetadataExt, PermissionsExt},
            },
        },
    };
    if bytes.len() > MAX_BYTES
        || !path.is_absolute()
        || path
            .components()
            .any(|part| matches!(part, Component::ParentDir))
    {
        return Err(SinkError::UnsafePath);
    }
    let mut names = Vec::new();
    for part in path.components() {
        match part {
            Component::RootDir => (),
            Component::Normal(name) => {
                names.push(CString::new(name.as_bytes()).map_err(|_| SinkError::UnsafePath)?)
            }
            _ => return Err(SinkError::UnsafePath),
        }
    }
    let leaf = names.pop().ok_or(SinkError::UnsafePath)?;
    let open_dir = |parent: i32, name: &std::ffi::CStr| -> Result<File, SinkError> {
        let fd = unsafe {
            libc::openat(
                parent,
                name.as_ptr(),
                libc::O_RDONLY | libc::O_DIRECTORY | libc::O_NOFOLLOW | libc::O_CLOEXEC,
            )
        };
        if fd < 0 {
            Err(SinkError::UnsafePath)
        } else {
            Ok(unsafe { File::from_raw_fd(fd) })
        }
    };
    let mut parent = open_dir(libc::AT_FDCWD, c"/")?;
    for name in names {
        parent = open_dir(parent.as_raw_fd(), &name)?;
    }
    let metadata = parent.metadata().map_err(|_| SinkError::Io)?;
    if !metadata.is_dir()
        || metadata.uid() != unsafe { libc::geteuid() }
        || metadata.mode() & 0o7777 != 0o700
    {
        return Err(SinkError::UnsafePath);
    }
    let fd = unsafe {
        libc::openat(
            parent.as_raw_fd(),
            leaf.as_ptr(),
            libc::O_WRONLY | libc::O_CREAT | libc::O_EXCL | libc::O_NOFOLLOW | libc::O_CLOEXEC,
            0o600,
        )
    };
    if fd < 0 {
        return Err(SinkError::UnsafePath);
    }
    let mut file = unsafe { File::from_raw_fd(fd) };
    file.set_permissions(std::fs::Permissions::from_mode(0o600))
        .map_err(|_| SinkError::Io)?;
    let private = |file: &File| -> Result<(), SinkError> {
        let actual = file.metadata().map_err(|_| SinkError::Io)?;
        if !actual.is_file()
            || actual.uid() != unsafe { libc::geteuid() }
            || actual.mode() & 0o7777 != 0o600
            || actual.nlink() != 1
        {
            Err(SinkError::UnsafePath)
        } else {
            Ok(())
        }
    };
    let result = private(&file)
        .and_then(|()| persist(&mut file, bytes))
        .and_then(|()| private(&file));
    if result.is_err() {
        // Invalidate only our newly created open inode, never unlink a replaced
        // name or touch a pre-existing operator file. Failure still means UNKNOWN.
        let _ = file.set_len(0);
    }
    result
}
#[cfg(not(target_os = "linux"))]
fn write_new(_path: &Path, _bytes: &[u8]) -> Result<(), SinkError> {
    Err(SinkError::Unsupported)
}

#[cfg(target_os = "linux")]
pub(crate) fn publish_ready(
    path: &Path,
    parent_identity: (u64, u64),
    bytes: &[u8],
    stop: &std::sync::atomic::AtomicBool,
) -> Result<(), SinkError> {
    publish_ready_checked(path, parent_identity, bytes, stop, || Ok(()), || Ok(()))
}
#[cfg(target_os = "linux")]
fn publish_ready_checked(
    path: &Path,
    parent_identity: (u64, u64),
    bytes: &[u8],
    stop: &std::sync::atomic::AtomicBool,
    before_publish: impl FnOnce() -> Result<(), SinkError>,
    after_publish: impl FnOnce() -> Result<(), SinkError>,
) -> Result<(), SinkError> {
    use std::{
        ffi::CString,
        fs::File,
        os::{
            fd::{AsRawFd, FromRawFd},
            unix::fs::{MetadataExt, PermissionsExt},
        },
        sync::atomic::Ordering,
    };
    if bytes.len() > 65536 {
        return Err(SinkError::Bound);
    }
    let (parent, leaf) =
        crate::event_correlation::private_parent(path).map_err(|_| SinkError::UnsafePath)?;
    let identity = |file: &File| -> Result<(u64, u64), SinkError> {
        let m = file.metadata().map_err(|_| SinkError::Io)?;
        Ok((m.dev(), m.ino()))
    };
    if identity(&parent)? != parent_identity {
        return Err(SinkError::UnsafePath);
    }
    let mut name = vec![b'.'];
    name.extend_from_slice(leaf.to_bytes());
    name.extend_from_slice(b".pending");
    let stage = CString::new(name).map_err(|_| SinkError::UnsafePath)?;
    let fd = unsafe {
        libc::openat(
            parent.as_raw_fd(),
            stage.as_ptr(),
            libc::O_WRONLY | libc::O_CREAT | libc::O_EXCL | libc::O_NOFOLLOW | libc::O_CLOEXEC,
            0o600,
        )
    };
    if fd < 0 {
        return Err(SinkError::UnsafePath);
    }
    let mut file = unsafe { File::from_raw_fd(fd) };
    let private = |f: &File| -> Result<(u64, u64), SinkError> {
        let m = f.metadata().map_err(|_| SinkError::Io)?;
        if !m.is_file()
            || m.uid() != unsafe { libc::geteuid() }
            || m.mode() & 0o7777 != 0o600
            || m.nlink() != 1
        {
            return Err(SinkError::UnsafePath);
        }
        Ok((m.dev(), m.ino()))
    };
    let named = |name: &std::ffi::CStr| -> Result<File, SinkError> {
        let fd = unsafe {
            libc::openat(
                parent.as_raw_fd(),
                name.as_ptr(),
                libc::O_RDONLY | libc::O_NOFOLLOW | libc::O_NONBLOCK | libc::O_CLOEXEC,
            )
        };
        if fd < 0 {
            Err(SinkError::Io)
        } else {
            Ok(unsafe { File::from_raw_fd(fd) })
        }
    };
    let result = (|| {
        file.set_permissions(std::fs::Permissions::from_mode(0o600))
            .map_err(|_| SinkError::Io)?;
        let created = private(&file)?;
        persist(&mut file, bytes)?;
        before_publish()?;
        if stop.load(Ordering::Acquire)
            || private(&file)? != created
            || private(&named(&stage)?)? != created
        {
            return Err(SinkError::Io);
        }
        let (again, _) =
            crate::event_correlation::private_parent(path).map_err(|_| SinkError::UnsafePath)?;
        if identity(&again)? != parent_identity {
            return Err(SinkError::UnsafePath);
        }
        if unsafe {
            libc::renameat2(
                parent.as_raw_fd(),
                stage.as_ptr(),
                parent.as_raw_fd(),
                leaf.as_ptr(),
                libc::RENAME_NOREPLACE,
            )
        } != 0
        {
            return Err(SinkError::Io);
        }
        parent.sync_all().map_err(|_| SinkError::Io)?;
        after_publish()?;
        let (published_parent, _) =
            crate::event_correlation::private_parent(path).map_err(|_| SinkError::UnsafePath)?;
        if identity(&published_parent)? != parent_identity {
            return Err(SinkError::UnsafePath);
        }
        if stop.load(Ordering::Acquire)
            || private(&named(&leaf)?)? != created
            || file.metadata().map_err(|_| SinkError::Io)?.len() != bytes.len() as u64
        {
            return Err(SinkError::Io);
        }
        Ok(())
    })();
    if result.is_err() {
        // Never remove the published leaf, a foreign staging entry, or a stale destination.
        if let (Ok(ours), Ok(entry)) = (private(&file), named(&stage)) {
            if private(&entry) == Ok(ours) {
                let _ = unsafe { libc::unlinkat(parent.as_raw_fd(), stage.as_ptr(), 0) };
            }
        }
    }
    result
}

#[cfg(all(test, target_os = "linux"))]
mod tests {
    use super::*;
    use std::{
        fs,
        os::unix::fs::{MetadataExt, PermissionsExt, symlink},
        sync::atomic::{AtomicU64, Ordering},
    };
    static NEXT: AtomicU64 = AtomicU64::new(0);
    #[test]
    fn renderer_observations_are_bounded_scalar_only_and_sealed() {
        // Only v3 owns this optional registry, including in headless tests.
        assert!(
            TraceRun::configured(Some("unused".into()))
                .unwrap()
                .view()
                .renderers
                .is_none()
        );
        let renderers = Renderers::default();
        let first = renderers.begin().unwrap();
        renderers.observed(first, Some("GskGLRenderer"), Some("GdkWaylandToplevel"));
        let second = renderers.begin().unwrap();
        renderers.observed(second, Some("PRIVATE_TYPE"), None);
        for _ in 2..64 {
            renderers.begin().unwrap();
        }
        assert!(renderers.begin().is_none());
        let value = renderers.seal();
        assert_eq!(value.records[0].renderer, RendererKind::GL);
        assert_eq!(value.records[0].backend, SurfaceBackend::Wayland);
        assert_eq!(value.records[1].renderer, RendererKind::Other);
        assert_eq!(value.records[1].backend, SurfaceBackend::Unknown);
        assert_eq!(value.dropped, 1);
        let raw = serde_json::to_vec(&value).unwrap();
        assert!(!String::from_utf8(raw.clone()).unwrap().contains("PRIVATE"));
        renderers.observed(first, None, None);
        assert!(renderers.begin().is_none());
        assert_eq!(serde_json::to_vec(&renderers.seal()).unwrap(), raw);
    }
    struct Fixture(PathBuf);
    impl Fixture {
        fn new() -> Self {
            let p = PathBuf::from("/tmp").join(format!(
                "veranda-trace-sink-{}-{}",
                std::process::id(),
                NEXT.fetch_add(1, Ordering::Relaxed)
            ));
            fs::create_dir(&p).unwrap();
            fs::set_permissions(&p, fs::Permissions::from_mode(0o700)).unwrap();
            Self(p)
        }
    }
    impl Drop for Fixture {
        fn drop(&mut self) {
            fs::remove_dir_all(&self.0).unwrap();
        }
    }
    #[test]
    fn explicit_run_writes_only_sealed_scalar_output_after_finish() {
        let root = Fixture::new();
        let path = root.0.join("trace");
        assert!(TraceRun::configured(None).is_none());
        assert!(!path.exists());
        let run = TraceRun::configured(Some(path.clone().into_os_string())).unwrap();
        let observer = run.observation();
        observer.activation();
        let worker = observer.worker_started().unwrap();
        observer.worker_closed(worker);
        let view = observer.view();
        observer.view_start(view, true, true);
        assert!(!path.exists());
        run.finish(true).unwrap();
        let metadata = fs::metadata(&path).unwrap();
        assert_eq!(metadata.mode() & 0o7777, 0o600);
        assert_eq!(metadata.nlink(), 1);
        assert_eq!(metadata.uid(), unsafe { libc::geteuid() });
        let value: serde_json::Value = serde_json::from_slice(&fs::read(&path).unwrap()).unwrap();
        assert_eq!(value["run_lifecycle"], "ObservedClosed");
        assert_eq!(value["snapshot"]["producer_quiescence"], "Unknown");
        assert_eq!(value["snapshot"]["counts"]["pending"], 0);
        assert_eq!(value["snapshot"]["counts"]["unknown"], 1);
        let before = fs::read(&path).unwrap();
        let held = observer.source(veranda_lib::observation::SourceKind::Local);
        assert!(held.request().is_none());
        drop(held);
        assert_eq!(encode(&observer.seal(), true, MAX_BYTES).unwrap(), before);
        let output = value.as_object().unwrap();
        assert_eq!(output.len(), 5);
        assert!(
            !String::from_utf8(before)
                .unwrap()
                .contains(path.to_str().unwrap())
        );
    }
    #[test]
    fn usage_config_is_validated_before_sources_and_exports_only_requested_v3() {
        let root = Fixture::new();
        for correlated in [false, true] {
            let path = root.0.join(if correlated {
                "combined-trace"
            } else {
                "usage-trace"
            });
            let config = root.0.join(if correlated {
                "combined-config"
            } else {
                "usage-config"
            });
            let correlation = correlated.then(|| {
                serde_json::json!({
                    "offers":[{"offer":1,"action":"synthetic","source":"/synthetic"}],
                    "cohorts":[{"ordinal":1,"owner":"private-owner","connection":null,"yard":null}],
                    "ready":root.0.join("ready").to_str().unwrap()
                })
            });
            let value = serde_json::json!({"schema":3,"child_usage":{"capacity":4},"correlation":correlation});
            fs::write(&config, serde_json::to_vec(&value).unwrap()).unwrap();
            fs::set_permissions(&config, fs::Permissions::from_mode(0o600)).unwrap();
            let run = TraceRun::configured_observation(
                path.clone().into_os_string(),
                config.clone().into_os_string(),
            )
            .unwrap();
            let observation = run.observation();
            assert!(run.view().renderers.is_some());
            assert_eq!(observation.counts().session_sources, 0);
            let worker = observation.worker_started().unwrap();
            observation.activation();
            observation.worker_closed(worker);
            run.finish(true).unwrap();
            let output: serde_json::Value =
                serde_json::from_slice(&fs::read(&path).unwrap()).unwrap();
            assert_eq!(output["schema_version"], 3);
            assert_eq!(output["snapshot"]["child_usage"]["attempts"], 0);
            assert_eq!(output["snapshot"]["counts"]["session_sources"], 0);
            assert_eq!(output["correlation_run"].is_null(), !correlated);
            if correlated {
                assert_eq!(output["correlation_run"]["writer"], "Stopped");
            }
            let frozen = serde_json::to_vec(&observation.seal()).unwrap();
            drop(observation.source(veranda_lib::observation::SourceKind::Local));
            assert_eq!(serde_json::to_vec(&observation.seal()).unwrap(), frozen);
            for capacity in [0, 8193] {
                let mut bad = value.clone();
                bad["child_usage"]["capacity"] = capacity.into();
                fs::write(&config, serde_json::to_vec(&bad).unwrap()).unwrap();
                assert!(
                    TraceRun::configured_observation(
                        root.0.join("absent").into_os_string(),
                        config.clone().into_os_string()
                    )
                    .is_err()
                );
            }
            let mut missing = value.clone();
            missing.as_object_mut().unwrap().remove("correlation");
            fs::write(&config, serde_json::to_vec(&missing).unwrap()).unwrap();
            assert!(
                TraceRun::configured_observation(
                    root.0.join("missing-field").into_os_string(),
                    config.clone().into_os_string()
                )
                .is_err()
            );
        }
    }
    #[test]
    fn readiness_publication_is_atomic_private_and_no_clobber() {
        use std::sync::atomic::AtomicBool;
        let root = Fixture::new();
        let path = root.0.join("ready");
        let metadata = fs::metadata(&root.0).unwrap();
        let identity = (metadata.dev(), metadata.ino());
        publish_ready_checked(
            &path,
            identity,
            b"complete",
            &AtomicBool::new(false),
            || {
                assert!(!path.exists());
                assert_eq!(
                    fs::read(root.0.join(".ready.pending")).unwrap(),
                    b"complete"
                );
                Ok(())
            },
            || {
                assert_eq!(fs::read(&path).unwrap(), b"complete");
                Ok(())
            },
        )
        .unwrap();
        assert_eq!(fs::metadata(&path).unwrap().mode() & 0o7777, 0o600);
        assert!(publish_ready(&path, identity, b"replacement", &AtomicBool::new(false)).is_err());
        assert_eq!(fs::read(&path).unwrap(), b"complete");
        assert!(!root.0.join(".ready.pending").exists());
    }
    #[test]
    fn runtime_trace_finishes_owned_writer_before_sealing_scalar_envelope() {
        let root = Fixture::new();
        let path = root.0.join("trace");
        let config = root.0.join("config");
        let value = serde_json::json!({"schema":2,"offers":[{"offer":1,"action":"private-action","source":"/private-uri"}],"cohorts":[{"ordinal":1,"owner":"private-owner","connection":null,"yard":null}],"ready":root.0.join("ready")});
        fs::write(&config, serde_json::to_vec(&value).unwrap()).unwrap();
        fs::set_permissions(&config, fs::Permissions::from_mode(0o600)).unwrap();
        let run =
            TraceRun::configured_runtime(path.clone().into_os_string(), config.into_os_string())
                .unwrap();
        let observation = run.observation();
        assert_eq!(observation.counts().session_sources, 0);
        assert!(!path.exists());
        run.finish(true).unwrap();
        let raw = fs::read(&path).unwrap();
        let result: serde_json::Value = serde_json::from_slice(&raw).unwrap();
        assert_eq!(result["schema_version"], 2);
        assert_eq!(result["snapshot"]["trace_window"], "Sealed");
        assert_eq!(result["snapshot"]["producer_quiescence"], "Unknown");
        assert_eq!(result["correlation_run"]["writer"], "Stopped");
        assert_eq!(result["correlation_run"]["phase"], "Cold");
        assert!(result["correlation_run"]["ready"].is_null());
        assert!(!root.0.join("ready").exists());
        let text = String::from_utf8(raw.clone()).unwrap();
        for private in [
            "private-owner",
            "private-action",
            "/private-uri",
            root.0.to_str().unwrap(),
        ] {
            assert!(!text.contains(private));
        }
        let snapshot = serde_json::to_value(observation.seal()).unwrap();
        let late = observation.source(veranda_lib::observation::SourceKind::Local);
        assert_eq!(late.ordinal(), 0);
        drop(late);
        assert_eq!(serde_json::to_value(observation.seal()).unwrap(), snapshot);
        assert_eq!(fs::read(path).unwrap(), raw);
    }
    #[test]
    fn readiness_fault_stop_and_unsafe_paths_never_report_success() {
        use std::sync::atomic::AtomicBool;
        for fault in [
            "stop",
            "before",
            "after",
            "post-parent",
            "stage",
            "symlink",
            "parent",
            "identity",
            "size",
        ] {
            let root = Fixture::new();
            let path = root.0.join("ready");
            let metadata = fs::metadata(&root.0).unwrap();
            let mut identity = (metadata.dev(), metadata.ino());
            if fault == "stage" {
                fs::write(root.0.join(".ready.pending"), b"foreign").unwrap();
            }
            if fault == "symlink" {
                std::os::unix::fs::symlink(root.0.join("unrelated"), &path).unwrap();
            }
            if fault == "parent" {
                fs::set_permissions(&root.0, fs::Permissions::from_mode(0o755)).unwrap();
            }
            if fault == "identity" {
                identity.1 += 1;
            }
            let bytes = if fault == "size" {
                vec![0; 65537]
            } else {
                b"complete".to_vec()
            };
            let result = publish_ready_checked(
                &path,
                identity,
                &bytes,
                &AtomicBool::new(fault == "stop"),
                || {
                    if fault == "before" {
                        Err(SinkError::Io)
                    } else {
                        Ok(())
                    }
                },
                || {
                    if fault == "post-parent" {
                        fs::set_permissions(&root.0, fs::Permissions::from_mode(0o755)).unwrap();
                        Ok(())
                    } else if fault == "after" {
                        Err(SinkError::Io)
                    } else {
                        Ok(())
                    }
                },
            );
            assert!(result.is_err(), "{fault}");
            if fault == "after" || fault == "post-parent" {
                assert_eq!(fs::read(&path).unwrap(), b"complete");
            } else if fault != "symlink" {
                assert!(!path.exists());
            }
            if fault == "stage" {
                assert_eq!(fs::read(root.0.join(".ready.pending")).unwrap(), b"foreign");
            } else {
                assert!(!root.0.join(".ready.pending").exists());
            }
        }
    }
    #[test]
    fn unsafe_paths_and_existing_operator_bytes_are_never_written() {
        let root = Fixture::new();
        let private = root.0.join("private");
        fs::create_dir(&private).unwrap();
        fs::set_permissions(&private, fs::Permissions::from_mode(0o700)).unwrap();
        let existing = private.join("existing");
        fs::write(&existing, b"operator").unwrap();
        fs::set_permissions(&existing, fs::Permissions::from_mode(0o600)).unwrap();
        assert!(write_new(&existing, b"new").is_err());
        assert_eq!(fs::read(&existing).unwrap(), b"operator");
        let link = root.0.join("link");
        symlink(&private, &link).unwrap();
        assert!(write_new(&link.join("new"), b"new").is_err());
        assert!(!private.join("new").exists());
        let leaf = private.join("leaf");
        symlink(&existing, &leaf).unwrap();
        assert!(write_new(&leaf, b"new").is_err());
        assert_eq!(fs::read(&existing).unwrap(), b"operator");
        assert!(write_new(&private.join("../escaped"), b"new").is_err());
        assert!(!root.0.join("escaped").exists());
        assert!(write_new(Path::new("relative"), b"new").is_err());
        assert!(write_new(&existing.join("not-a-directory"), b"new").is_err());
        assert!(write_new(&root.0.join("missing/new"), b"new").is_err());
        fs::set_permissions(&private, fs::Permissions::from_mode(0o755)).unwrap();
        assert!(write_new(&private.join("new"), b"new").is_err());
    }
    #[test]
    fn bounded_encoding_write_errors_and_incomplete_lifecycle_fail_closed() {
        let root = Fixture::new();
        let observer = Observation::new();
        let snapshot = observer.seal();
        assert!(matches!(encode(&snapshot, true, 1), Err(SinkError::Bound)));
        assert_eq!(
            serde_json::from_slice::<serde_json::Value>(
                &encode(&snapshot, true, MAX_BYTES).unwrap()
            )
            .unwrap()["run_lifecycle"],
            "Unknown"
        );
        let observer = Observation::new();
        observer.activation();
        let worker = observer.worker_started().unwrap();
        observer.worker_closed(worker);
        assert_eq!(
            serde_json::from_slice::<serde_json::Value>(
                &encode(&observer.seal(), false, MAX_BYTES).unwrap()
            )
            .unwrap()["run_lifecycle"],
            "Unknown"
        );
        let path = root.0.join("read-only");
        fs::write(&path, b"original").unwrap();
        let mut file = fs::File::open(&path).unwrap();
        assert_eq!(persist(&mut file, b"new"), Err(SinkError::Io));
        assert_eq!(fs::read(&path).unwrap(), b"original");
        assert!(write_new(&root.0.join("too-large"), &vec![0; MAX_BYTES + 1]).is_err());
        assert!(!root.0.join("too-large").exists());
    }
}
