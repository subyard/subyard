//! ```compile_fail,E0599
//! use subyard_release_owner::store::Store;
//! fn implicit(path:&std::path::Path){let _=Store::open(path);}
//! ```
//! Physical protected-record publication only. Journal replacement/retention policy is not here.
//!
//! ```compile_fail,E0451
//! use subyard_release_owner::store::Snapshot;
//! let forged = Snapshot { bytes: None };
//! ```
use crate::{
    codec::MAX_BYTES,
    digest,
    lease::UpdateLease,
    record::{fingerprint, safe_id},
    Error, Result,
};
use rustix::fs::{self, FileType, FlockOperation, Mode, OFlags, RenameFlags};
use std::{
    fs::File,
    io::{Read, Write},
    os::fd::{AsFd, OwnedFd},
    path::Path,
};
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Snapshot {
    bytes: Option<Vec<u8>>,
}
impl Snapshot {
    pub(crate) fn captured(bytes: Option<Vec<u8>>) -> Self {
        Self { bytes }
    }
    pub fn bytes(&self) -> Option<&[u8]> {
        self.bytes.as_deref()
    }
    pub fn fingerprint(&self) -> String {
        digest(
            self.bytes
                .as_deref()
                .unwrap_or(b"protected-record-absent-v1"),
        )
    }
}
/// Closed production addresses; arbitrary path or filename input is never admitted.
pub struct Address {
    pub(crate) parents: Vec<String>,
    pub(crate) name: String,
    pub(crate) immutable: bool,
}
#[derive(Clone, Copy)]
pub enum EvidenceCheckpoint {
    Captured,
    Applied,
    Verified,
}
impl Address {
    pub(crate) fn native(transaction: &str, ordinal: u64, kind: &str) -> Result<Self> {
        if !safe_id(transaction)
            || ordinal == 0
            || !matches!(
                kind,
                "intent" | "accepted" | "terminal" | "reservation" | "closure" | "recovery"
            )
        {
            return Err(Error::Invalid);
        }
        Ok(Self::at(
            vec![
                "migrations".into(),
                "native-custody".into(),
                "v1".into(),
                transaction.into(),
            ],
            format!("{ordinal:03}.{kind}.json"),
            true,
        ))
    }
    pub(crate) fn at(parents: Vec<String>, name: String, immutable: bool) -> Self {
        Self {
            parents,
            name,
            immutable,
        }
    }
    fn base() -> Vec<String> {
        vec!["release-transition".into(), "v2".into()]
    }
    pub fn ledger() -> Self {
        Self::at(Self::base(), "ledger.json".into(), false)
    }
    pub fn journal() -> Self {
        Self::at(Self::base(), "journal.json".into(), false)
    }
    pub fn checkpoint() -> Self {
        Self::at(Self::base(), "history-checkpoint.json".into(), false)
    }
    pub fn evidence(transaction: &str, step: &str, checkpoint: EvidenceCheckpoint) -> Result<Self> {
        if !safe_id(transaction) || !safe_id(step) {
            return Err(Error::Invalid);
        }
        let suffix = match checkpoint {
            EvidenceCheckpoint::Captured => "captured",
            EvidenceCheckpoint::Applied => "applied",
            EvidenceCheckpoint::Verified => "verified",
        };
        let mut parents = Self::base();
        parents.extend(["transactions".into(), transaction.into(), "evidence".into()]);
        Ok(Self::at(parents, format!("{step}.{suffix}.json"), true))
    }
    pub fn superseded(transaction: &str) -> Result<Self> {
        if !safe_id(transaction) {
            return Err(Error::Invalid);
        }
        let mut parents = Self::base();
        parents.extend(["transactions".into(), transaction.into()]);
        Ok(Self::at(parents, "superseded-journal.json".into(), true))
    }
    pub fn compatibility(identity: &str) -> Result<Self> {
        if !fingerprint(identity) {
            return Err(Error::Invalid);
        }
        let mut parents = Self::base();
        parents.push("compatibility".into());
        Ok(Self::at(parents, format!("{identity}.json"), true))
    }
}
pub struct Store {
    pub(crate) reader: crate::legacy::Reader,
    pub(crate) root: Option<OwnedFd>,
    pub(crate) retirement_capture_failed: std::cell::Cell<bool>,
    #[cfg(test)]
    cut: Option<&'static str>,
}
pub(crate) fn linked_directory(parent: &impl AsFd, name: &str, fd: &impl AsFd) -> Result<()> {
    let actual = io(fs::fstat(fd))?;
    let named = io(fs::statat(parent, name, fs::AtFlags::SYMLINK_NOFOLLOW))?;
    if actual.st_dev != named.st_dev
        || actual.st_ino != named.st_ino
        || FileType::from_raw_mode(named.st_mode) != FileType::Directory
    {
        return Err(Error::Stale);
    }
    checked(fd, true, true, false)
}
pub(crate) fn io<T>(r: rustix::io::Result<T>) -> Result<T> {
    r.map_err(|_| Error::Io)
}
pub(crate) fn checked(fd: &impl AsFd, directory: bool, exact: bool, bounded: bool) -> Result<()> {
    let s = io(fs::fstat(fd))?;
    let kind = if directory {
        FileType::Directory
    } else {
        FileType::RegularFile
    };
    let mode = s.st_mode & 0o777;
    if FileType::from_raw_mode(s.st_mode) != kind
        || s.st_uid != rustix::process::getuid().as_raw()
        || (directory
            && if exact {
                mode != 0o700
            } else {
                mode & 0o022 != 0
            })
        || (!directory && (mode != 0o600 || s.st_nlink != 1))
    {
        return Err(Error::Protected);
    }
    if bounded && (s.st_size <= 0 || s.st_size > MAX_BYTES as i64) {
        return Err(Error::Bound);
    }
    Ok(())
}
pub(crate) fn flags() -> OFlags {
    OFlags::NOFOLLOW | OFlags::CLOEXEC | OFlags::NONBLOCK
}
pub(crate) fn directory(
    root: &impl AsFd,
    parts: &[String],
    create: bool,
) -> Result<Option<OwnedFd>> {
    let mut current = io(rustix::io::dup(root))?;
    for name in parts {
        let open = || {
            fs::openat(
                &current,
                name,
                OFlags::RDONLY | OFlags::DIRECTORY | flags(),
                Mode::empty(),
            )
        };
        let next = match open() {
            Ok(fd) => fd,
            Err(rustix::io::Errno::NOENT) if !create => return Ok(None),
            Err(rustix::io::Errno::NOENT) => {
                let created = match fs::mkdirat(&current, name, Mode::from_raw_mode(0o700)) {
                    Ok(()) => true,
                    Err(rustix::io::Errno::EXIST) => false,
                    Err(_) => return Err(Error::Io),
                };
                let fd = io(open())?;
                if created {
                    io(fs::fchmod(&fd, Mode::from_raw_mode(0o700)))?;
                }
                checked(&fd, true, true, false)?;
                io(fs::fsync(&current))?;
                fd
            }
            Err(_) => return Err(Error::Io),
        };
        checked(&next, true, true, false)?;
        current = next;
    }
    Ok(Some(current))
}
pub(crate) fn read_at(parent: &impl AsFd, name: &str, staging: bool) -> Result<Snapshot> {
    let fd = match fs::openat(parent, name, OFlags::RDONLY | flags(), Mode::empty()) {
        Ok(fd) => fd,
        Err(rustix::io::Errno::NOENT) => return Ok(Snapshot { bytes: None }),
        Err(_) => return Err(Error::Io),
    };
    checked(&fd, false, true, !staging)?;
    let mut bytes = Vec::new();
    File::from(fd)
        .take(MAX_BYTES as u64 + 1)
        .read_to_end(&mut bytes)
        .map_err(|_| Error::Io)?;
    if bytes.len() > MAX_BYTES || (!staging && bytes.is_empty()) {
        return Err(Error::Bound);
    }
    Ok(Snapshot { bytes: Some(bytes) })
}
impl Store {
    pub(crate) fn pinned(root: OwnedFd, reader: crate::legacy::FrozenReader) -> Result<Self> {
        checked(&root, true, false, false)?;
        Ok(Self {
            reader: crate::legacy::Reader::Frozen(reader),
            root: Some(root),
            retirement_capture_failed: std::cell::Cell::new(false),
            #[cfg(test)]
            cut: None,
        })
    }
    /// Complete historical reader is mandatory for the future production owner.
    pub fn open_with_reader(path: &Path, reader: crate::legacy::FrozenReader) -> Result<Self> {
        let mut store = Self::open_canonical_fixture(path)?;
        store.reader = crate::legacy::Reader::Frozen(reader);
        Ok(store)
    }
    /// Frozen journal ingress retains the exact original byte revision.
    pub fn read_checked_journal(&self) -> Result<Option<(crate::record::Journal, Snapshot)>> {
        let snapshot = self.read(&Address::journal())?;
        let Some(raw) = snapshot.bytes() else {
            return Ok(None);
        };
        let journal = crate::record::Journal::new(self.decode(raw, true, false)?)?;
        Ok(Some((journal, snapshot)))
    }
    pub(crate) fn decode<T: crate::legacy::Historical>(
        &self,
        bytes: &[u8],
        newline: bool,
        canonical: bool,
    ) -> Result<T> {
        self.reader.decode(bytes, canonical, newline)
    }
    /// Read-only canonical fixture construction; never the production historical reader.
    /// Read-only construction does not create an absent root or hierarchy.
    pub fn open_canonical_fixture(path: &Path) -> Result<Self> {
        if !path.is_absolute() || path == Path::new("/") {
            return Err(Error::Invalid);
        }
        let root = match fs::open(
            path,
            OFlags::RDONLY | OFlags::DIRECTORY | flags(),
            Mode::empty(),
        ) {
            Ok(fd) => {
                checked(&fd, true, false, false)?;
                Some(fd)
            }
            Err(rustix::io::Errno::NOENT) => None,
            Err(_) => return Err(Error::Io),
        };
        Ok(Self {
            reader: crate::legacy::Reader::CanonicalFixture,
            root,
            retirement_capture_failed: std::cell::Cell::new(false),
            #[cfg(test)]
            cut: None,
        })
    }
    pub fn read(&self, address: &Address) -> Result<Snapshot> {
        let Some(root) = &self.root else {
            return Ok(Snapshot { bytes: None });
        };
        let Some(parent) = directory(root, &address.parents, false)? else {
            return Ok(Snapshot { bytes: None });
        };
        read_at(&parent, &address.name, false)
    }
    pub fn lock(&self) -> Result<UpdateLease<'_>> {
        let root = self.root.as_ref().ok_or(Error::Io)?;
        let parent = directory(root, &["migrations".into()], true)?.ok_or(Error::Io)?;
        let (lock, created) = match fs::openat(
            &parent,
            "update.lock",
            OFlags::RDWR | OFlags::CREATE | OFlags::EXCL | flags(),
            Mode::from_raw_mode(0o600),
        ) {
            Ok(fd) => (fd, true),
            Err(rustix::io::Errno::EXIST) => (
                io(fs::openat(
                    &parent,
                    "update.lock",
                    OFlags::RDWR | flags(),
                    Mode::empty(),
                ))?,
                false,
            ),
            Err(_) => return Err(Error::Io),
        };
        if created {
            io(fs::fchmod(&lock, Mode::from_raw_mode(0o600)))?;
        }
        checked(&lock, false, true, false)?;
        io(fs::fsync(&parent))?;
        io(fs::flock(&lock, FlockOperation::LockExclusive))?;
        Ok(UpdateLease {
            store: self,
            _lock: lock,
        })
    }
    #[cfg(test)]
    pub(crate) fn test_cut(&mut self, cut: Option<&'static str>) {
        self.cut = cut;
    }
    pub(crate) fn fault(&self, point: &str) {
        #[cfg(test)]
        if self.cut == Some(point) {
            println!("publication-cut:{point}");
            std::io::stdout().flush().unwrap();
            loop {
                std::thread::park();
            }
        }
        let _ = point;
    }
}
impl UpdateLease<'_> {
    /// Exact byte revision, deliberately independent of file inode/mtime.
    pub fn compare_and_swap(
        &self,
        address: &Address,
        expected: &Snapshot,
        payload: &[u8],
    ) -> Result<()> {
        if address.immutable {
            return Err(Error::Invalid);
        }
        self.publish(address, expected, payload, false)?;
        // The existing fixture control observes only durable journal revisions,
        // including initial authorization and recovery successor publication.
        // Normal builds compile this point to a no-op.
        if address.parents == Address::base() && address.name == "journal.json" {
            crate::fixture::point("after-journal-publication")?;
        }
        Ok(())
    }
    pub fn create_immutable(&self, address: &Address, payload: &[u8]) -> Result<()> {
        if !address.immutable {
            return Err(Error::Invalid);
        }
        let current = self.store.read(address)?;
        if let Some(bytes) = current.bytes() {
            if bytes != payload {
                return Err(Error::Exists);
            }
            let parent = directory(
                self.store.root.as_ref().ok_or(Error::Io)?,
                &address.parents,
                false,
            )?
            .ok_or(Error::Io)?;
            return io(fs::fsync(&parent));
        }
        self.publish(address, &current, payload, true)
    }
    fn publish(
        &self,
        address: &Address,
        expected: &Snapshot,
        payload: &[u8],
        immutable: bool,
    ) -> Result<()> {
        if payload.is_empty() || payload.len() > MAX_BYTES {
            return Err(Error::Bound);
        }
        let parent = directory(
            self.store.root.as_ref().ok_or(Error::Io)?,
            &address.parents,
            true,
        )?
        .ok_or(Error::Io)?;
        let conflict = if immutable {
            Error::Exists
        } else {
            Error::Stale
        };
        if read_at(&parent, &address.name, false)? != *expected {
            return Err(conflict);
        }
        let pending = format!(".{}.pending", address.name);
        let staged = read_at(&parent, &pending, true)?;
        if let Some(bytes) = staged.bytes() {
            if !payload.starts_with(bytes) {
                if immutable {
                    return Err(conflict);
                }
                // Match Go orphan-CAS staging: discard only after exact expected reobservation.
                if read_at(&parent, &address.name, false)? != *expected {
                    return Err(conflict);
                }
                io(fs::unlinkat(&parent, &pending, fs::AtFlags::empty()))?;
                io(fs::fsync(&parent))?;
            }
        }
        let (fd, existing) = match fs::openat(
            &parent,
            &pending,
            OFlags::RDWR | OFlags::CREATE | OFlags::EXCL | flags(),
            Mode::from_raw_mode(0o600),
        ) {
            Ok(fd) => (fd, false),
            Err(rustix::io::Errno::EXIST) => (
                io(fs::openat(
                    &parent,
                    &pending,
                    OFlags::RDWR | flags(),
                    Mode::empty(),
                ))?,
                true,
            ),
            Err(_) => return Err(Error::Io),
        };
        if !existing {
            io(fs::fchmod(&fd, Mode::from_raw_mode(0o600)))?;
        }
        checked(&fd, false, true, false)?;
        let mut file = File::from(fd);
        let mut prefix = Vec::new();
        if existing {
            (&mut file)
                .take(MAX_BYTES as u64 + 1)
                .read_to_end(&mut prefix)
                .map_err(|_| Error::Io)?;
            if !payload.starts_with(&prefix) {
                return Err(conflict);
            }
        }
        file.write_all(&payload[prefix.len()..])
            .map_err(|_| Error::Io)?;
        file.sync_all().map_err(|_| Error::Io)?;
        io(fs::fsync(&parent))?;
        self.store.fault("after-pending-fsync");
        if read_at(&parent, &address.name, false)? != *expected {
            return Err(conflict);
        }
        self.store.fault("before-publish");
        let published = if expected.bytes.is_some() {
            fs::renameat(&parent, &pending, &parent, &address.name)
        } else {
            fs::renameat_with(
                &parent,
                &pending,
                &parent,
                &address.name,
                RenameFlags::NOREPLACE,
            )
        };
        match published {
            Ok(()) => {}
            Err(rustix::io::Errno::EXIST) => return Err(conflict),
            Err(_) => return Err(Error::Io),
        };
        self.store.fault("after-publish-before-dir-fsync");
        io(fs::fsync(&parent))?;
        self.store.fault("after-dir-fsync");
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::{
        io::{BufRead, BufReader},
        os::unix::fs::{symlink, PermissionsExt},
        path::PathBuf,
        process::{Command, Stdio},
        sync::mpsc,
        time::{Duration, SystemTime, UNIX_EPOCH},
    };
    struct Fixture(PathBuf);
    impl Fixture {
        fn new() -> Self {
            let p = PathBuf::from(format!(
                "/tmp/subyard-owner-store-{}-{}",
                std::process::id(),
                SystemTime::now()
                    .duration_since(UNIX_EPOCH)
                    .unwrap()
                    .as_nanos()
            ));
            std::fs::create_dir(&p).unwrap();
            std::fs::set_permissions(&p, std::fs::Permissions::from_mode(0o700)).unwrap();
            Self(p)
        }
        fn write(&self, name: &str, bytes: &[u8]) {
            let p = self.0.join(name);
            std::fs::write(&p, bytes).unwrap();
            std::fs::set_permissions(p, std::fs::Permissions::from_mode(0o600)).unwrap();
        }
    }
    impl Drop for Fixture {
        fn drop(&mut self) {
            std::fs::remove_dir_all(&self.0).unwrap();
        }
    }
    #[test]
    fn byte_revision_and_immutable_collision() {
        let f = Fixture::new();
        let store = Store::open_canonical_fixture(&f.0).unwrap();
        let address = Address::ledger();
        let before = store.read(&address).unwrap();
        assert_eq!(before.fingerprint(), digest(b"protected-record-absent-v1"));
        let lease = store.lock().unwrap();
        lease.compare_and_swap(&address, &before, b"one\n").unwrap();
        let same = store.read(&address).unwrap();
        f.write("replacement", b"one\n");
        std::fs::rename(
            f.0.join("replacement"),
            f.0.join("release-transition/v2/ledger.json"),
        )
        .unwrap();
        lease.compare_and_swap(&address, &same, b"two\n").unwrap();
        assert_eq!(
            lease.compare_and_swap(&address, &same, b"three\n"),
            Err(Error::Stale)
        );
        let immutable = Address::compatibility(&"a".repeat(64)).unwrap();
        lease.create_immutable(&immutable, b"receipt\n").unwrap();
        lease.create_immutable(&immutable, b"receipt\n").unwrap();
        assert_eq!(
            lease.create_immutable(&immutable, b"changed\n"),
            Err(Error::Exists)
        );
        assert_eq!(
            lease.compare_and_swap(&immutable, &before, b"x"),
            Err(Error::Invalid)
        );
        assert!(Address::superseded("../escape").is_err());
        assert!(Address::evidence("tx", "../escape", EvidenceCheckpoint::Verified).is_err());
    }
    #[test]
    fn absent_read_and_protected_leaves() {
        let f = Fixture::new();
        let absent = f.0.join("absent");
        let s = Store::open_canonical_fixture(&absent).unwrap();
        assert!(s.read(&Address::journal()).unwrap().bytes().is_none());
        assert!(!absent.exists());
        assert!(s.lock().is_err());
        let store = Store::open_canonical_fixture(&f.0).unwrap();
        let address = Address::ledger();
        let lease = store.lock().unwrap();
        let before = store.read(&address).unwrap();
        lease
            .compare_and_swap(&address, &before, b"record\n")
            .unwrap();
        drop(lease);
        let leaf = f.0.join("release-transition/v2/ledger.json");
        std::fs::set_permissions(&leaf, std::fs::Permissions::from_mode(0o644)).unwrap();
        assert_eq!(store.read(&address).unwrap_err(), Error::Protected);
        std::fs::set_permissions(&leaf, std::fs::Permissions::from_mode(0o600)).unwrap();
        std::fs::hard_link(&leaf, f.0.join("hardlink")).unwrap();
        assert_eq!(store.read(&address).unwrap_err(), Error::Protected);
        std::fs::remove_file(f.0.join("hardlink")).unwrap();
        if rustix::process::geteuid().as_raw() == 0 {
            std::os::unix::fs::chown(&leaf, Some(1), None).unwrap();
            assert_eq!(store.read(&address).unwrap_err(), Error::Protected);
            std::os::unix::fs::chown(&leaf, Some(0), None).unwrap();
        } else {
            eprintln!("foreign UID physical fixture unavailable without privilege; ownership guard exercised by metadata branch only on root runs");
        }
        std::fs::remove_file(&leaf).unwrap();
        symlink(f.0.join("owner.marker"), &leaf).unwrap();
        assert!(store.read(&address).is_err());
        std::fs::remove_file(&leaf).unwrap();
        fs::mknodat(
            fs::CWD,
            &leaf,
            FileType::Fifo,
            Mode::from_raw_mode(0o600),
            0,
        )
        .unwrap();
        assert_eq!(store.read(&address).unwrap_err(), Error::Protected);
        std::fs::remove_file(&leaf).unwrap();
        f.write(
            "release-transition/v2/ledger.json",
            &vec![b'x'; MAX_BYTES + 1],
        );
        assert_eq!(store.read(&address).unwrap_err(), Error::Bound);
        let lease = store.lock().unwrap();
        assert_eq!(
            lease.compare_and_swap(&address, &before, b""),
            Err(Error::Bound)
        );
    }
    #[test]
    fn interrupted_pending_prefix_and_orphan_cas() {
        let f = Fixture::new();
        let store = Store::open_canonical_fixture(&f.0).unwrap();
        let address = Address::ledger();
        let lease = store.lock().unwrap();
        let before = store.read(&address).unwrap();
        lease.compare_and_swap(&address, &before, b"old\n").unwrap();
        let before = store.read(&address).unwrap();
        for prefix in [b"".as_slice(), b"ne", b"new\n"] {
            f.write("release-transition/v2/.ledger.json.pending", prefix);
            lease.compare_and_swap(&address, &before, b"new\n").unwrap();
            let current = store.read(&address).unwrap();
            lease
                .compare_and_swap(&address, &current, b"old\n")
                .unwrap();
        }
        f.write("release-transition/v2/.ledger.json.pending", b"orphan\n");
        lease.compare_and_swap(&address, &before, b"new\n").unwrap();
        let immutable = Address::compatibility(&"b".repeat(64)).unwrap();
        lease.create_immutable(&immutable, b"receipt\n").unwrap();
    }
    // Re-exec only this test to expose real process death at durable publication cuts.
    #[test]
    fn child() {
        let Ok(root) = std::env::var("SUBYARD_STORE_FIXTURE") else {
            return;
        };
        assert!(root.starts_with("/tmp/subyard-owner-store-"));
        let mut store = Store::open_canonical_fixture(Path::new(&root)).unwrap();
        store.cut = match std::env::var("SUBYARD_STORE_CUT").as_deref() {
            Ok("after-pending-fsync") => Some("after-pending-fsync"),
            Ok("before-publish") => Some("before-publish"),
            Ok("after-publish-before-dir-fsync") => Some("after-publish-before-dir-fsync"),
            Ok("after-dir-fsync") => Some("after-dir-fsync"),
            _ => None,
        };
        let probe = std::env::var_os("SUBYARD_STORE_LOCK_PROBE").is_some();
        if probe {
            println!("lease-waiting");
            std::io::stdout().flush().unwrap();
        }
        let lease = store.lock().unwrap();
        if probe {
            println!("lease-acquired");
            return;
        }
        let address = Address::journal();
        let before = store.read(&address).unwrap();
        if before.bytes() != Some(b"desired\n".as_slice()) {
            lease
                .compare_and_swap(&address, &before, b"desired\n")
                .unwrap();
        }
    }
    fn command(root: &Path) -> Command {
        let mut command = Command::new(std::env::current_exe().unwrap());
        command
            .args(["--exact", "store::tests::child", "--nocapture"])
            .env("SUBYARD_STORE_FIXTURE", root)
            .stdout(Stdio::piped())
            .stderr(Stdio::piped());
        command
    }
    fn bounded_wait(child: &mut std::process::Child) {
        let until = std::time::Instant::now() + Duration::from_secs(5);
        loop {
            if let Some(status) = child.try_wait().unwrap() {
                assert!(status.success());
                return;
            }
            if std::time::Instant::now() >= until {
                child.kill().unwrap();
                child.wait().unwrap();
                panic!("fixture child exceeded deadline");
            }
            std::thread::sleep(Duration::from_millis(5));
        }
    }
    #[test]
    fn fresh_process_after_publication_cuts() {
        for cut in [
            "after-pending-fsync",
            "before-publish",
            "after-publish-before-dir-fsync",
            "after-dir-fsync",
        ] {
            let f = Fixture::new();
            let mut child = command(&f.0).env("SUBYARD_STORE_CUT", cut).spawn().unwrap();
            let stdout = child.stdout.take().unwrap();
            let (tx, rx) = mpsc::channel();
            std::thread::spawn(move || {
                for line in BufReader::new(stdout)
                    .lines()
                    .map_while(std::result::Result::ok)
                {
                    if line.starts_with("publication-cut:") {
                        let _ = tx.send(line);
                        break;
                    }
                }
            });
            let ready = rx.recv_timeout(Duration::from_secs(5));
            child.kill().unwrap();
            child.wait().unwrap();
            assert_eq!(ready.unwrap(), format!("publication-cut:{cut}"));
            let current = Store::open_canonical_fixture(&f.0)
                .unwrap()
                .read(&Address::journal())
                .unwrap();
            if matches!(cut, "after-pending-fsync" | "before-publish") {
                assert!(current.bytes().is_none());
            } else {
                assert_eq!(current.bytes(), Some(b"desired\n".as_slice()));
            }
            let mut resumed = command(&f.0).spawn().unwrap();
            bounded_wait(&mut resumed);
            assert_eq!(
                Store::open_canonical_fixture(&f.0)
                    .unwrap()
                    .read(&Address::journal())
                    .unwrap()
                    .bytes(),
                Some(b"desired\n".as_slice())
            );
        }
    }
    #[test]
    fn compatibility_update_lock_blocks_other_process() {
        let f = Fixture::new();
        let store = Store::open_canonical_fixture(&f.0).unwrap();
        let lease = store.lock().unwrap();
        let mut child = command(&f.0)
            .env("SUBYARD_STORE_LOCK_PROBE", "1")
            .spawn()
            .unwrap();
        let stdout = child.stdout.take().unwrap();
        let (tx, rx) = mpsc::channel();
        std::thread::spawn(move || {
            for line in BufReader::new(stdout)
                .lines()
                .map_while(std::result::Result::ok)
            {
                if line.starts_with("lease-") {
                    let _ = tx.send(line);
                }
            }
        });
        let waiting = rx.recv_timeout(Duration::from_secs(5));
        if waiting.as_deref() != Ok("lease-waiting") {
            child.kill().unwrap();
            child.wait().unwrap();
            panic!("lock probe did not reach the held lease");
        }
        assert!(rx.recv_timeout(Duration::from_millis(100)).is_err());
        drop(lease);
        bounded_wait(&mut child);
        assert_eq!(
            rx.recv_timeout(Duration::from_secs(5)).unwrap(),
            "lease-acquired"
        );
    }
}
