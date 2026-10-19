//! Frozen syntax bridge. Original bytes remain the protected revision/identity.
//! A normalized projection is private and cannot be supplied by a caller.
//! ```compile_fail,E0603
//! use subyard_release_owner::legacy::NormalizedRecord;
//! ```
//! ```
//! use subyard_release_owner::{legacy::FrozenReader,store::Store,Result};
//! fn operation(path:&std::path::Path,reader:FrozenReader)->Result<Store>{
//!     Store::open_with_reader(path,reader)
//! }
//! ```
//! ```compile_fail,E0451
//! use subyard_release_owner::legacy::FrozenReader;
//! fn dismantle(reader:FrozenReader){let FrozenReader {peer:_}=reader;}
//! ```
use crate::{codec, digest, Error, Result};
use serde::{de::DeserializeOwned, Serialize};
use std::{
    cell::RefCell,
    fs::File,
    io::{Read, Seek, SeekFrom, Write},
    os::{
        fd::{AsFd, AsRawFd, OwnedFd},
        unix::net::UnixStream,
    },
    process::{Child, Command, Stdio},
    sync::mpsc,
    thread,
    time::{Duration, Instant},
};
const MAX_PROJECTION: usize = 8 << 20;
const HEADER: usize = 52;
const DEADLINE: Duration = Duration::from_secs(5);
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
#[repr(u8)]
pub(crate) enum Kind {
    Registry = 1,
    Catalog,
    Ledger,
    Checkpoint,
    Journal,
    Evidence,
    Archive,
    Compatibility,
    Receipt,
    Lifecycle,
    Frontier,
    MigrationOwner,
    Settings,
    Owner,
    Native,
    CheckpointCapability,
    ConfigLayout,
}
pub(crate) trait Historical: DeserializeOwned + Serialize {
    const KIND: Kind;
}
macro_rules! kinds {($($t:ty=>$k:ident),* $(,)?)=>{$(impl Historical for $t{const KIND:Kind=Kind::$k;})*};}
kinds!(crate::wire::registry_v2::RegistryV2=>Registry,Option<Vec<crate::wire::registry_v2::CapabilityDescriptor>> =>Catalog,crate::wire::ledger_v2::LedgerV2=>Ledger,crate::wire::checkpoint_v1::LedgerCheckpointV1=>Checkpoint,crate::wire::journal_v2::Record=>Journal,crate::wire::journal_v2::Evidence=>Evidence,crate::wire::journal_v2::Archive=>Archive,crate::wire::journal_v2::CompatibilityEvidence=>Compatibility,crate::wire::recovery_v1::RecoveryReceiptV1=>Receipt,crate::wire::lifecycle_v1::RecoveryLifecycleRecord=>Lifecycle,crate::wire::lifecycle_v1::RecoveryFrontier=>Frontier,crate::wire::migration_owner_v1::MigrationOwnerReceiptV1=>MigrationOwner);
struct NormalizedRecord {
    kind: Kind,
    sequence: u64,
    original: Vec<u8>,
    hash: String,
    image_digest: String,
    projection: Vec<u8>,
    flags: u8,
}
impl NormalizedRecord {
    fn project<T: Historical>(&self, kind: Kind, sequence: u64, original: &[u8]) -> Result<T> {
        if self.kind != kind
            || T::KIND != kind
            || self.sequence != sequence
            || self.original != original
            || self.hash != digest(original)
        {
            return Err(Error::Invalid);
        }
        // Go marshals every closed record as an object; only the catalog is a
        // nullable array. Serde's positional struct sequence support is not a
        // normalized record representation from this peer.
        if self.kind != Kind::Catalog
            && self.projection.iter().find(|b| !b.is_ascii_whitespace()) != Some(&b'{')
        {
            return Err(Error::Codec);
        }
        serde_json::from_slice(&self.projection).map_err(|_| Error::Codec)
    }
    fn admit<T: Historical>(self, value: T, canonical: bool, newline: bool) -> Result<T> {
        // Only root receipt V1 applies exact raw lifecycle-key presence rejection.
        if self.kind == Kind::Receipt && self.flags != 0 {
            let schema: serde_json::Value =
                serde_json::from_slice(&self.projection).map_err(|_| Error::Codec)?;
            if schema["schemaVersion"] == 1 {
                return Err(Error::Invalid);
            }
        }
        if canonical && codec::encode(&value, newline)? != self.original {
            return Err(Error::Noncanonical);
        }
        Ok(value)
    }
    #[cfg(test)]
    fn into_dto<T: Historical>(
        self,
        kind: Kind,
        sequence: u64,
        original: &[u8],
        canonical: bool,
        newline: bool,
    ) -> Result<T> {
        let value = self.project(kind, sequence, original)?;
        self.admit(value, canonical, newline)
    }
}
struct Message {
    header: [u8; HEADER],
    raw: Vec<u8>,
    descriptors: Vec<OwnedFd>,
    deadline: Duration,
}
struct Reply {
    header: [u8; HEADER],
    projection: Vec<u8>,
}
struct Peer {
    child: Child,
    tx: Option<mpsc::Sender<Message>>,
    rx: mpsc::Receiver<Result<Reply>>,
    worker: Option<thread::JoinHandle<()>>,
    sequence: u64,
    poisoned: bool,
    _image: File,
    image_digest: String,
    native: bool,
    armed: bool,
    admission: UnixStream,
}
fn transfer(stream: &mut UnixStream, bytes: &mut [u8], write: bool, end: Instant) -> Result<()> {
    let mut offset = 0;
    while offset < bytes.len() {
        let left = end
            .checked_duration_since(Instant::now())
            .ok_or(Error::Io)?;
        let n = if write {
            stream
                .set_write_timeout(Some(left))
                .map_err(|_| Error::Io)?;
            stream.write(&bytes[offset..])
        } else {
            stream.set_read_timeout(Some(left)).map_err(|_| Error::Io)?;
            stream.read(&mut bytes[offset..])
        }
        .map_err(|_| Error::Io)?;
        if n == 0 {
            return Err(Error::Io);
        };
        offset += n;
    }
    Ok(())
}
impl Peer {
    fn spawn(input: File) -> Result<Self> {
        Self::spawn_mode(input, "_release-owner-codec-v1")
    }
    fn spawn_mode(mut input: File, mode: &'static str) -> Result<Self> {
        use rustix::fs::{self, FileType, MemfdFlags, Mode, SealFlags};
        let stat = fs::fstat(&input).map_err(|_| Error::Io)?;
        if FileType::from_raw_mode(stat.st_mode) != FileType::RegularFile
            || stat.st_uid != rustix::process::getuid().as_raw()
            || !(stat.st_nlink == 1
                || stat.st_nlink == 0
                    && stat.st_mode & 0o777 == 0o500
                    && fs::fcntl_get_seals(&input).is_ok_and(|seals| {
                        seals.contains(
                            SealFlags::WRITE
                                | SealFlags::GROW
                                | SealFlags::SHRINK
                                | SealFlags::SEAL,
                        )
                    }))
            || stat.st_mode & 0o022 != 0
            || stat.st_size <= 0
            || stat.st_size > 128 << 20
        {
            return Err(Error::Protected);
        }
        input.seek(SeekFrom::Start(0)).map_err(|_| Error::Io)?;
        let mut image = if stat.st_nlink == 0 {
            input
        } else {
            let fd = fs::memfd_create("subyard-frozen-codec", MemfdFlags::ALLOW_SEALING)
                .map_err(|_| Error::Io)?;
            let mut image = File::from(fd);
            let copied = std::io::copy(&mut input.take((128 << 20) + 1), &mut image)
                .map_err(|_| Error::Io)?;
            if copied != stat.st_size as u64 {
                return Err(Error::Stale);
            }
            fs::fchmod(&image, Mode::from_raw_mode(0o500)).map_err(|_| Error::Io)?;
            fs::fcntl_add_seals(
                &image,
                SealFlags::WRITE | SealFlags::GROW | SealFlags::SHRINK | SealFlags::SEAL,
            )
            .map_err(|_| Error::Io)?;
            image
        };
        image.seek(SeekFrom::Start(0)).map_err(|_| Error::Io)?;
        use sha2::{Digest, Sha256};
        let mut hasher = Sha256::new();
        let mut block = [0u8; 65536];
        loop {
            let n = image.read(&mut block).map_err(|_| Error::Io)?;
            if n == 0 {
                break;
            };
            hasher.update(&block[..n]);
        }
        let image_digest = hasher
            .finalize()
            .iter()
            .map(|b| format!("{b:02x}"))
            .collect();
        // The executable path refers to this actual sealed FD, never a reopened path.
        let (mut stream, native) = UnixStream::pair().map_err(|_| Error::Io)?;
        let admission = stream.try_clone().map_err(|_| Error::Io)?;
        let stdin = Stdio::from(std::os::fd::OwnedFd::from(
            native.try_clone().map_err(|_| Error::Io)?,
        ));
        let stdout = Stdio::from(std::os::fd::OwnedFd::from(native));
        let mut command = Command::new(format!("/proc/self/fd/{}", image.as_raw_fd()));
        if mode == "_release-owner-native-v1" {
            use std::os::unix::process::CommandExt;
            command.process_group(0);
        }
        let child = command
            .arg(mode)
            .stdin(stdin)
            .stdout(stdout)
            .stderr(Stdio::null())
            .spawn()
            .map_err(|_| Error::Io)?;
        let (tx, requests) = mpsc::channel::<Message>();
        let (responses, rx) = mpsc::channel();
        let worker = thread::spawn(move || {
            while let Ok(mut request) = requests.recv() {
                let result = (|| {
                    let end = Instant::now() + request.deadline;
                    if request.descriptors.is_empty() {
                        transfer(&mut stream, &mut request.header, true, end)?;
                    } else {
                        use rustix::net::{SendAncillaryBuffer, SendAncillaryMessage, SendFlags};
                        let mut space =
                            [std::mem::MaybeUninit::uninit(); rustix::cmsg_space!(ScmRights(3))];
                        let mut ancillary = SendAncillaryBuffer::new(&mut space);
                        let borrowed: Vec<_> =
                            request.descriptors.iter().map(|fd| fd.as_fd()).collect();
                        if !ancillary.push(SendAncillaryMessage::ScmRights(&borrowed)) {
                            return Err(Error::Bound);
                        }
                        let remaining = end
                            .checked_duration_since(Instant::now())
                            .ok_or(Error::Io)?;
                        stream
                            .set_write_timeout(Some(remaining))
                            .map_err(|_| Error::Io)?;
                        let sent = rustix::net::sendmsg(
                            &stream,
                            &[std::io::IoSlice::new(&request.header[..1])],
                            &mut ancillary,
                            SendFlags::NOSIGNAL,
                        )
                        .map_err(|_| Error::Io)?;
                        if sent != 1 {
                            return Err(Error::Io);
                        }
                        transfer(&mut stream, &mut request.header[1..], true, end)?;
                    }
                    transfer(&mut stream, &mut request.raw, true, end)?;
                    let mut header = [0; HEADER];
                    transfer(&mut stream, &mut header, false, end)?;
                    let size = u32::from_be_bytes(header[48..].try_into().unwrap()) as usize;
                    if size > MAX_PROJECTION {
                        return Err(Error::Bound);
                    };
                    let mut projection = vec![0; size];
                    transfer(&mut stream, &mut projection, false, end)?;
                    Ok(Reply { header, projection })
                })();
                let stop = result.is_err();
                if responses.send(result).is_err() || stop {
                    break;
                }
            }
        });
        Ok(Self {
            child,
            tx: Some(tx),
            rx,
            worker: Some(worker),
            sequence: 0,
            poisoned: false,
            _image: image,
            image_digest,
            native: mode == "_release-owner-native-v1",
            armed: false,
            admission,
        })
    }
    fn normalize(&mut self, kind: Kind, raw: &[u8]) -> Result<NormalizedRecord> {
        self.normalize_with_descriptors(kind, raw, Vec::new())
    }
    fn normalize_with_descriptors(
        &mut self,
        kind: Kind,
        raw: &[u8],
        descriptors: Vec<OwnedFd>,
    ) -> Result<NormalizedRecord> {
        self.normalize_with_budget(
            kind,
            raw,
            descriptors,
            if kind == Kind::Native {
                Duration::from_secs(600)
            } else {
                DEADLINE
            },
        )
    }
    fn normalize_with_budget(
        &mut self,
        kind: Kind,
        raw: &[u8],
        descriptors: Vec<OwnedFd>,
        deadline: Duration,
    ) -> Result<NormalizedRecord> {
        if descriptors.len() > 3 || kind != Kind::Native && !descriptors.is_empty() {
            return Err(Error::Invalid);
        }
        if self.poisoned || raw.is_empty() || raw.len() > codec::MAX_BYTES {
            return Err(Error::Bound);
        }
        self.sequence = self.sequence.checked_add(1).ok_or(Error::Bound)?;
        let mut header = [0; HEADER];
        header[..4].copy_from_slice(b"SYC1");
        header[4] = 1;
        header[5] = kind as u8;
        header[8..16].copy_from_slice(&self.sequence.to_be_bytes());
        use sha2::{Digest, Sha256};
        header[16..48].copy_from_slice(&Sha256::digest(raw));
        header[48..].copy_from_slice(&(raw.len() as u32).to_be_bytes());
        let result = (|| {
            self.tx
                .as_ref()
                .ok_or(Error::Io)?
                .send(Message {
                    header,
                    raw: raw.to_vec(),
                    descriptors,
                    deadline,
                })
                .map_err(|_| Error::Io)?;
            let reply = self
                .rx
                .recv_timeout(deadline + Duration::from_millis(100))
                .map_err(|_| Error::Io)??;
            if reply.header[..6] != header[..6]
                || reply.header[8..48] != header[8..48]
                || reply.header[6] > 1
                || reply.header[7] & !7 != 0
            {
                return Err(Error::Invalid);
            }
            if reply.header[6] != 0 {
                if !reply.projection.is_empty() || reply.header[7] != 0 {
                    return Err(Error::Invalid);
                };
                return Err(Error::Codec);
            }
            if reply.projection.is_empty() || kind != Kind::Receipt && reply.header[7] != 0 {
                return Err(Error::Invalid);
            }
            Ok(NormalizedRecord {
                kind,
                sequence: self.sequence,
                original: raw.to_vec(),
                hash: digest(raw),
                image_digest: self.image_digest.clone(),
                projection: reply.projection,
                flags: reply.header[7],
            })
        })();
        if result.is_err() && !matches!(&result, Err(Error::Codec)) {
            self.poison();
        }
        result
    }
    fn poison(&mut self) {
        self.poisoned = true;
        self.stop();
    }
    fn stop(&mut self) {
        self.stop_with_budget(Duration::from_secs(75 * 60 + 40));
    }
    fn stop_with_budget(&mut self, budget: Duration) {
        self.tx.take();
        // Read-only codec peers can be killed. A native custodian must first
        // see admission EOF and drain accepted work while retaining its OFDs.
        let _ = self.admission.shutdown(std::net::Shutdown::Write);
        if !self.native || !self.armed {
            let _ = self.child.kill();
        }
        if let Some(worker) = self.worker.take() {
            let _ = worker.join();
        }
        if self.native && self.armed {
            let end = Instant::now() + budget;
            loop {
                match self.child.try_wait() {
                    Ok(Some(_)) => return,
                    Ok(None) if Instant::now() < end => thread::sleep(Duration::from_millis(10)),
                    _ => break,
                }
            }
            // The helper's accepted mutation budget preserves the 75-minute component allowance. A
            // supervisory kill after the drain allowance is not terminal evidence: poison and
            // the existing intent/pending journal continue to forbid Ready.
            self.poisoned = true;
            let _ = self.child.kill();
        }
        let _ = self.child.wait();
    }
}
impl Drop for Peer {
    fn drop(&mut self) {
        self.stop()
    }
}
/// One retained syntax peer per operation. Physical image sealing is not manifest trust.
/// Delivery must supply the exact authenticated helper FD before enabling an owner.
pub struct FrozenReader {
    peer: RefCell<Peer>,
}
impl FrozenReader {
    /// Capture and seal the actual executable FD. No caller hash or path is trusted.
    pub fn capture_helper(input: File) -> Result<Self> {
        Ok(Self {
            peer: RefCell::new(Peer::spawn(input)?),
        })
    }
    pub(crate) fn capture_native_helper(input: File) -> Result<Self> {
        Ok(Self {
            peer: RefCell::new(Peer::spawn_mode(input, "_release-owner-native-v1")?),
        })
    }
    pub(crate) fn native(
        &self,
        request: &crate::native::Request,
        descriptors: Vec<OwnedFd>,
    ) -> Result<crate::native::Reply> {
        let raw = codec::encode(request, false)?;
        let mut peer = self.peer.borrow_mut();
        if matches!(request, crate::native::Request::ClaimLease { .. }) {
            peer.armed = true;
        }
        let effect = matches!(
            request,
            crate::native::Request::ApplySettings { .. }
                | crate::native::Request::ApplyOwner { .. }
                | crate::native::Request::CleanupOwner { .. }
                | crate::native::Request::ApplyIngress { .. }
                | crate::native::Request::ApplyActivation { .. }
                | crate::native::Request::FenceActivation { .. }
        );
        let proof = peer.normalize_with_budget(
            Kind::Native,
            &raw,
            descriptors,
            Duration::from_secs(if effect { 76 * 60 } else { 600 }),
        )?;
        let reply = proof
            .project::<crate::native::Reply>(Kind::Native, peer.sequence, &raw)
            .and_then(|reply| {
                crate::record::require(matches!(
                    reply.code.as_str(),
                    "" | "stale" | "unknown" | "failed" | "unavailable"
                ))?;
                if let Some(diagnostic) = &reply.diagnostic {
                    diagnostic.validate()?;
                }
                reply.protocol(request)?;
                Ok(reply)
            });
        if reply.is_err() {
            peer.poison()
        }
        if matches!(request, crate::native::Request::ReleaseLease { .. })
            && reply.as_ref().is_ok_and(|r| r.code.is_empty())
        {
            peer.armed = false;
        }
        reply
    }
    pub(crate) fn reject_native_projection(&self) {
        self.peer.borrow_mut().poison();
    }
    pub fn image_digest(&self) -> String {
        self.peer.borrow().image_digest.clone()
    }
    pub(crate) fn health(&self) -> Result<()> {
        let mut peer = self.peer.borrow_mut();
        if peer.poisoned {
            return Err(Error::Codec);
        }
        if peer.child.try_wait().map_err(|_| Error::Io)?.is_some() {
            peer.poison();
            return Err(Error::Codec);
        }
        Ok(())
    }
    pub(crate) fn decode<T: Historical>(
        &self,
        raw: &[u8],
        canonical: bool,
        newline: bool,
    ) -> Result<T> {
        let mut peer = self.peer.borrow_mut();
        let proof = peer.normalize(T::KIND, raw)?;
        if proof.image_digest != peer.image_digest {
            peer.poison();
            return Err(Error::Invalid);
        }
        let value = match proof.project::<T>(T::KIND, peer.sequence, raw) {
            Ok(value) => value,
            Err(error) => {
                // A success frame must carry the promised typed projection. Poison
                // here before optional historical reads can discard the error.
                peer.poison();
                return Err(error);
            }
        };
        proof.admit(value, canonical, newline)
    }
}
pub(crate) enum Reader {
    CanonicalFixture,
    Frozen(FrozenReader),
}
impl Reader {
    pub(crate) fn health(&self) -> Result<()> {
        match self {
            Self::CanonicalFixture => Ok(()),
            Self::Frozen(reader) => reader.health(),
        }
    }
    pub(crate) fn decode<T: Historical>(
        &self,
        raw: &[u8],
        canonical: bool,
        newline: bool,
    ) -> Result<T> {
        match self {
            Self::CanonicalFixture => {
                if canonical {
                    codec::decode_canonical(raw, newline)
                } else if raw.is_empty() || raw.len() > codec::MAX_BYTES {
                    Err(Error::Bound)
                } else {
                    serde_json::from_slice(raw).map_err(|_| Error::Codec)
                }
            }
            Self::Frozen(peer) => peer.decode(raw, canonical, newline),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use base64::Engine;
    use std::{collections::BTreeMap, fs, os::unix::fs::PermissionsExt, path::PathBuf};
    #[derive(serde::Deserialize)]
    struct Vector {
        name: String,
        kind: u8,
        raw: String,
        projection: String,
        flags: u8,
        syntax: bool,
        accepted: bool,
    }
    #[derive(serde::Deserialize)]
    #[serde(rename_all = "PascalCase")]
    struct StoreCase {
        name: String,
        operation: String,
        registry: String,
        registry_digest: String,
        files: BTreeMap<String, String>,
        accepted: bool,
    }
    #[derive(serde::Deserialize)]
    #[serde(rename_all = "camelCase")]
    struct Corpus {
        catalog: Vec<crate::wire::registry_v2::CapabilityDescriptor>,
        registry: String,
        registry_digest: String,
        projection: String,
        expansion_registry: String,
        vectors: Vec<Vector>,
        stores: Vec<StoreCase>,
    }
    fn raw(s: &str) -> Vec<u8> {
        base64::engine::general_purpose::STANDARD.decode(s).unwrap()
    }
    #[test]
    fn held_custodian() {
        if std::env::var("SUBYARD_HELD_CUSTODIAN").as_deref() != Ok("1") {
            return;
        };
        println!("CUSTODIAN_READY");
        std::io::stdout().flush().unwrap();
        thread::sleep(Duration::from_secs(30));
    }
    #[test]
    fn unlinked_image_requires_every_seal_and_exact_executable_mode() {
        use rustix::fs::{self, MemfdFlags, Mode, SealFlags};
        for omitted in [
            SealFlags::WRITE,
            SealFlags::GROW,
            SealFlags::SHRINK,
            SealFlags::SEAL,
        ] {
            let mut file = File::from(
                fs::memfd_create("untrusted-helper", MemfdFlags::ALLOW_SEALING).unwrap(),
            );
            file.write_all(b"untrusted image").unwrap();
            fs::fchmod(&file, Mode::from_raw_mode(0o500)).unwrap();
            let all = SealFlags::WRITE | SealFlags::GROW | SealFlags::SHRINK | SealFlags::SEAL;
            fs::fcntl_add_seals(&file, all & !omitted).unwrap();
            assert!(matches!(Peer::spawn(file), Err(Error::Protected)));
        }
        let mut file =
            File::from(fs::memfd_create("unsealed-helper", MemfdFlags::ALLOW_SEALING).unwrap());
        file.write_all(b"untrusted image").unwrap();
        fs::fchmod(&file, Mode::from_raw_mode(0o500)).unwrap();
        assert!(matches!(Peer::spawn(file), Err(Error::Protected)));
    }
    #[test]
    fn armed_supervisory_timeout_stays_poisoned() {
        use std::io::BufRead;
        let executable = std::env::current_exe().unwrap();
        let mut child = Command::new(&executable)
            .args(["--exact", "legacy::tests::held_custodian", "--nocapture"])
            .env("SUBYARD_HELD_CUSTODIAN", "1")
            .stdout(Stdio::piped())
            .spawn()
            .unwrap();
        let mut output = std::io::BufReader::new(child.stdout.take().unwrap());
        let mut line = String::new();
        loop {
            assert!(output.read_line(&mut line).unwrap() > 0);
            if line.contains("CUSTODIAN_READY") {
                break;
            };
            line.clear()
        }
        let (admission, _remote) = UnixStream::pair().unwrap();
        let (_tx, rx) = mpsc::channel();
        let mut peer = Peer {
            child,
            tx: None,
            rx,
            worker: None,
            sequence: 0,
            poisoned: false,
            _image: File::open(executable).unwrap(),
            image_digest: "a".repeat(64),
            native: true,
            armed: true,
            admission,
        };
        let start = Instant::now();
        peer.stop_with_budget(Duration::from_millis(20));
        assert!(start.elapsed() < Duration::from_secs(2));
        assert!(peer.child.try_wait().unwrap().is_some());
        assert!(peer.poisoned);
        let reader = FrozenReader {
            peer: RefCell::new(peer),
        };
        assert_eq!(reader.health(), Err(Error::Codec));
    }
    fn peer() -> FrozenReader {
        FrozenReader::capture_helper(
            File::open(std::env::var("SUBYARD_RUST_CODEC_HELPER").unwrap()).unwrap(),
        )
        .unwrap()
    }
    fn kind(n: u8) -> Kind {
        match n {
            1 => Kind::Registry,
            2 => Kind::Catalog,
            3 => Kind::Ledger,
            4 => Kind::Checkpoint,
            5 => Kind::Journal,
            6 => Kind::Evidence,
            7 => Kind::Archive,
            8 => Kind::Compatibility,
            9 => Kind::Receipt,
            10 => Kind::Lifecycle,
            11 => Kind::Frontier,
            12 => Kind::MigrationOwner,
            13 => Kind::Settings,
            14 => Kind::Owner,
            _ => panic!("closed corpus kind"),
        }
    }
    fn dirs(path: &std::path::Path) {
        if path.exists() {
            return;
        };
        dirs(path.parent().unwrap());
        fs::create_dir(path).unwrap();
        fs::set_permissions(path, fs::Permissions::from_mode(0o700)).unwrap()
    }
    #[test]
    fn owning_raw_and_store_paths() {
        let Ok(path) = std::env::var("SUBYARD_RUST_LEGACY_CORPUS") else {
            return;
        };
        let corpus: Corpus = serde_json::from_slice(&fs::read(path).unwrap()).unwrap();
        let catalog = crate::record::Catalog::new(Some(corpus.catalog)).unwrap();
        let reader = Reader::Frozen(peer());
        let base = crate::record::Registry::new(
            reader.decode(&raw(&corpus.registry), false, false).unwrap(),
            &catalog,
        )
        .unwrap();
        let projection =
            crate::record::Projection::from_reader(raw(&corpus.projection), &reader, false)
                .unwrap();
        let expansion: crate::wire::registry_v2::RegistryV2 = reader
            .decode(&raw(&corpus.expansion_registry), false, false)
            .unwrap();
        let expansion = crate::record::Registry::new(expansion, &catalog).unwrap();
        let Reader::Frozen(frozen) = &reader else {
            unreachable!()
        };
        let pid = frozen.peer.borrow().child.id();
        assert_eq!(
            frozen.image_digest(),
            digest(&fs::read(std::env::var("SUBYARD_RUST_CODEC_HELPER").unwrap()).unwrap())
        );
        for case in &corpus.vectors {
            let bytes = raw(&case.raw);
            let k = kind(case.kind);
            let result = frozen.peer.borrow_mut().normalize(k, &bytes);
            assert_eq!(result.is_ok(), case.syntax, "{} syntax", case.name);
            if let Ok(proof) = result {
                assert_eq!(
                    proof.projection,
                    case.projection.as_bytes(),
                    "{} projection",
                    case.name
                );
                assert_eq!(proof.flags, case.flags, "{} flags", case.name);
                let sequence = proof.sequence;
                let accepted = match k {
                    Kind::Registry => proof
                        .into_dto(k, sequence, &bytes, false, false)
                        .and_then(|v| crate::record::Registry::new(v, &catalog))
                        .is_ok(),
                    Kind::Ledger => proof
                        .into_dto(k, sequence, &bytes, false, false)
                        .and_then(|v| crate::record::Ledger::new(v, &expansion))
                        .is_ok(),
                    Kind::Journal => proof
                        .into_dto(k, sequence, &bytes, false, false)
                        .and_then(crate::record::Journal::new)
                        .is_ok(),
                    Kind::Receipt => proof
                        .into_dto(k, sequence, &bytes, false, false)
                        .and_then(crate::recovery::Receipt::new)
                        .is_ok(),
                    Kind::MigrationOwner => proof
                        .into_dto(k, sequence, &bytes, false, false)
                        .and_then(|v| crate::migration_owner::Receipt::from_reader(v, &reader))
                        .is_ok(),
                    Kind::Catalog => proof
                        .into_dto(k, sequence, &bytes, false, false)
                        .and_then(crate::record::Catalog::new)
                        .is_ok(),
                    Kind::Checkpoint => proof
                        .into_dto(k, sequence, &bytes, false, false)
                        .and_then(|v| {
                            crate::record::Checkpoint::new(
                                v,
                                &base,
                                &corpus.registry_digest,
                                &projection,
                            )
                        })
                        .is_ok(),
                    Kind::Evidence => proof
                        .into_dto(k, sequence, &bytes, false, false)
                        .and_then(crate::record::Evidence::new)
                        .is_ok(),
                    Kind::Archive => proof
                        .into_dto(k, sequence, &bytes, false, false)
                        .and_then(crate::record::Archive::new)
                        .is_ok(),
                    Kind::Compatibility => proof
                        .into_dto(k, sequence, &bytes, false, false)
                        .and_then(crate::record::CompatibilityEvidence::new)
                        .is_ok(),
                    Kind::Lifecycle => proof
                        .into_dto(k, sequence, &bytes, false, false)
                        .and_then(crate::recovery::Terminal::new)
                        .is_ok(),
                    Kind::Frontier => proof
                        .into_dto(k, sequence, &bytes, false, false)
                        .and_then(crate::recovery::Frontier::new)
                        .is_ok(),
                    Kind::Settings => proof
                        .into_dto::<crate::source::SettingsRecovery>(
                            k, sequence, &bytes, false, false,
                        )
                        .is_ok(),
                    Kind::Native | Kind::CheckpointCapability | Kind::ConfigLayout => {
                        panic!("operation ingress kinds are not frozen historical corpus kinds")
                    }
                    Kind::Owner => proof
                        .into_dto::<crate::source::OwnerRecovery>(k, sequence, &bytes, false, false)
                        .is_ok(),
                };
                assert_eq!(accepted, case.accepted, "{} domain", case.name);
            } else {
                assert!(
                    !case.accepted,
                    "{} owning admission requires syntax",
                    case.name
                )
            }
            assert_eq!(frozen.peer.borrow().child.id(), pid, "one retained peer");
        }
        for (index, case) in corpus.stores.iter().enumerate() {
            let root = PathBuf::from(format!(
                "/tmp/subyard-legacy-store-{}-{index}",
                std::process::id()
            ));
            assert!(!root.exists());
            dirs(&root);
            struct Cleanup(PathBuf);
            impl Drop for Cleanup {
                fn drop(&mut self) {
                    let _ = fs::remove_dir_all(&self.0);
                }
            }
            let _cleanup = Cleanup(root.clone());
            for (name, value) in &case.files {
                assert!(!name.starts_with('/') && !name.split('/').any(|c| c == ".."));
                let p = root.join(name);
                dirs(p.parent().unwrap());
                fs::write(&p, raw(value)).unwrap();
                fs::set_permissions(p, fs::Permissions::from_mode(0o600)).unwrap()
            }
            let frozen = peer();
            let registry = if case.registry.is_empty() {
                None
            } else {
                Some(
                    crate::record::Registry::from_frozen(
                        &frozen,
                        &raw(&case.registry),
                        &crate::migration_owner::builtin_catalog(true, true).unwrap(),
                    )
                    .unwrap(),
                )
            };
            let store = crate::store::Store::open_with_reader(&root, frozen).unwrap();
            let result = if case.operation == "journal" {
                store.read_checked_journal().map(|value| {
                    let (_, snapshot) = value.unwrap();
                    let bytes = raw(&case.files["release-transition/v2/journal.json"]);
                    assert_eq!(snapshot.bytes(), Some(bytes.as_slice()));
                    assert_eq!(snapshot.fingerprint(), digest(&bytes));
                })
            } else if case.operation == "graph" {
                crate::graph::State::read(&store).map(|_| ())
            } else {
                let (registry, pin) = registry.as_ref().unwrap();
                assert_eq!(pin, &case.registry_digest);
                store.inspect_source(registry, pin).map(|proof| {
                    let snapshot = proof.ledger().projection();
                    let bytes = raw(&case.files["release-transition/v2/ledger.json"]);
                    assert_eq!(snapshot.bytes(), Some(bytes.as_slice()));
                    assert_eq!(snapshot.fingerprint(), digest(&bytes));
                })
            };
            assert_eq!(result.is_ok(), case.accepted, "{} {:?}", case.name, result);
            for (name, value) in &case.files {
                assert_eq!(
                    fs::read(root.join(name)).unwrap(),
                    raw(value),
                    "{} original bytes",
                    case.name
                )
            }
        }
        eprintln!(
            "{} actual Go raw probes; {} protected store/source/graph outcomes",
            corpus.vectors.len(),
            corpus.stores.len()
        );
    }
    #[test]
    fn native_payload_correlation_poison_is_shared_and_domain_refusal_reuses_peer() {
        let Ok(path) = std::env::var("SUBYARD_RUST_CODEC_FAULT_PEER") else {
            return;
        };
        for offset in 1..=6 {
            let reader = FrozenReader::capture_native_helper(File::open(&path).unwrap()).unwrap();
            let request = crate::native::Request::ReadInstalledRecipe {
                session: "a".repeat(64),
                recipe_hash: "b".repeat(64),
                offset,
            };
            assert!(reader.native(&request, vec![]).is_err(), "offset={offset}");
            assert!(reader.peer.borrow().poisoned);
            assert!(reader.peer.borrow_mut().child.try_wait().unwrap().is_some());
            assert!(reader.peer.borrow().worker.is_none());
        }
        let reader = FrozenReader::capture_native_helper(File::open(&path).unwrap()).unwrap();
        let request = |offset| crate::native::Request::ReadInstalledRecipe {
            session: "a".repeat(64),
            recipe_hash: "b".repeat(64),
            offset,
        };
        assert_eq!(reader.native(&request(7), vec![]).unwrap().code, "stale");
        reader.health().unwrap();
        reader.native(&request(0), vec![]).unwrap();
        reader.health().unwrap();
        let request = crate::native::Request::BindInstalledRecipe {
            session: "a".repeat(64),
            recipe_hash: "b".repeat(64),
            offset: 8,
            total: 13,
            content: "aGVsbG8=".into(),
        };
        assert!(reader.native(&request, vec![]).is_err());
        assert!(reader.peer.borrow().poisoned);
    }
    #[test]
    fn malformed_projection_and_refusal_poison() {
        let Ok(path) = std::env::var("SUBYARD_RUST_CODEC_FAULT_PEER") else {
            return;
        };
        for receipt in [false, true] {
            for raw in [
                b"malformed".as_slice(),
                b"wrong-dto",
                b"array-dto",
                b"refusal-flags",
            ] {
                let reader = FrozenReader::capture_helper(File::open(&path).unwrap()).unwrap();
                let result = if receipt {
                    reader
                        .decode::<crate::wire::recovery_v1::RecoveryReceiptV1>(raw, false, false)
                        .map(|_| ())
                } else {
                    reader
                        .decode::<crate::wire::registry_v2::RegistryV2>(raw, false, false)
                        .map(|_| ())
                };
                assert!(result.is_err());
                assert!(
                    reader.peer.borrow().poisoned,
                    "receipt={receipt}, raw={raw:?}"
                );
                assert!(reader.peer.borrow_mut().child.try_wait().unwrap().is_some());
                assert!(reader.peer.borrow().worker.is_none());
                assert!(reader
                    .decode::<crate::wire::registry_v2::RegistryV2>(b"valid", false, false)
                    .is_err());
            }
        }
        // A normal syntax refusal, root receipt presence rejection and original-byte
        // canonicality refusal describe input, not a malformed peer response.
        let reader = FrozenReader::capture_helper(File::open(&path).unwrap()).unwrap();
        assert!(matches!(
            reader.decode::<crate::wire::registry_v2::RegistryV2>(b"Refusal", false, false),
            Err(Error::Codec)
        ));
        assert!(matches!(
            reader.decode::<crate::wire::recovery_v1::RecoveryReceiptV1>(b"presence", false, false),
            Err(Error::Invalid)
        ));
        assert!(matches!(
            reader.decode::<crate::wire::registry_v2::RegistryV2>(b"canonical", true, false),
            Err(Error::Noncanonical)
        ));
        assert!(!reader.peer.borrow().poisoned);
        assert!(reader.peer.borrow_mut().child.try_wait().unwrap().is_none());
        assert!(reader
            .decode::<crate::wire::registry_v2::RegistryV2>(b"valid", false, false)
            .is_ok());
    }
    #[test]
    fn native_poison_deadline_and_bounds() {
        let Ok(path) = std::env::var("SUBYARD_RUST_CODEC_FAULT_PEER") else {
            return;
        };
        for raw in [
            b"nonce".as_slice(),
            b"kind",
            b"hash",
            b"status",
            b"flags",
            b"bound",
            b"disconnect",
            b"exit",
            b"x-nonzero",
            b"timeout",
        ] {
            let mut peer = Peer::spawn(File::open(&path).unwrap()).unwrap();
            let before = Instant::now();
            assert!(peer.normalize(Kind::Registry, raw).is_err());
            assert!(before.elapsed() < Duration::from_secs(7));
            assert!(peer.child.try_wait().unwrap().is_some());
            assert!(peer.normalize(Kind::Registry, b"valid").is_err());
        }
        let mut peer = Peer::spawn(File::open(path).unwrap()).unwrap();
        assert!(peer
            .normalize(Kind::Registry, &vec![0; codec::MAX_BYTES + 1])
            .is_err());
        let proof = peer.normalize(Kind::Registry, b"valid").unwrap();
        let sequence = proof.sequence;
        assert!(proof
            .into_dto::<crate::wire::registry_v2::RegistryV2>(
                Kind::Registry,
                sequence,
                b"changed",
                false,
                false
            )
            .is_err());
        assert!(peer.normalize(Kind::Registry, b"valid").is_ok());
    }
}
