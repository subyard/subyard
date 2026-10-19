//! Execution-only account barrier ingress. Inspection retains only a socket and
//! pinned read-only DataHome FD; the actual borrowed OFD arrives after consent.
use crate::{codec, digest, native, record::require, store, Error, Result};
use rustix::{
    fs::{self, AtFlags, FileType, FlockOperation, Mode, OFlags},
    net::{RecvAncillaryBuffer, RecvAncillaryMessage, RecvFlags, ReturnFlags},
};
use serde::{Deserialize, Serialize};
use std::{
    cell::RefCell,
    fs::File,
    io::{Read, Write},
    os::fd::OwnedFd,
    os::unix::net::UnixStream,
    path::{Path, PathBuf},
    time::{Duration, Instant},
};
#[derive(Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct Admit {
    version: u8,
    session: String,
    plan: String,
    transaction: String,
    data_root: native::Identity,
    lock: native::Identity,
}
#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct Ack<'a> {
    version: u8,
    session: &'a str,
    plan: &'a str,
    transaction: &'a str,
    accepted: bool,
}
struct Held {
    file: OwnedFd,
    session: String,
    plan: String,
    transaction: String,
}
pub(crate) struct OuterCustody {
    ingress: RefCell<Option<UnixStream>>,
    held: RefCell<Option<Held>>,
    data: OwnedFd,
    path: PathBuf,
    identity: native::Identity,
}
impl OuterCustody {
    pub(crate) fn identity(&self) -> native::Identity {
        self.identity.clone()
    }
    pub(crate) fn capture(path: &Path, ingress: File) -> Result<Self> {
        let stat = store::io(fs::fstat(&ingress))?;
        require(
            FileType::from_raw_mode(stat.st_mode) == FileType::Socket
                && stat.st_uid == rustix::process::getuid().as_raw(),
        )?;
        let data = store::io(fs::openat(
            fs::CWD,
            path,
            OFlags::RDONLY | OFlags::DIRECTORY | store::flags(),
            Mode::empty(),
        ))?;
        store::checked(&data, true, false, false)?;
        let identity = native::Identity::capture(&data)?;
        let ingress = UnixStream::from(OwnedFd::from(ingress));
        ingress.peer_addr().map_err(|_| Error::Invalid)?;
        Ok(Self {
            ingress: RefCell::new(Some(ingress)),
            held: RefCell::new(None),
            data,
            path: path.into(),
            identity,
        })
    }
    pub(crate) fn current(&self) -> Result<()> {
        let current = store::io(fs::statat(fs::CWD, &self.path, AtFlags::SYMLINK_NOFOLLOW))?;
        require(
            current.st_dev == self.identity.device
                && current.st_ino == self.identity.inode
                && current.st_mode == self.identity.mode
                && current.st_uid == self.identity.uid
                && {
                    let pinned = native::Identity::capture(&self.data)?;
                    pinned.device == self.identity.device
                        && pinned.inode == self.identity.inode
                        && pinned.uid == self.identity.uid
                        && pinned.mode == self.identity.mode
                },
        )
    }
    pub(crate) fn admit(&self, session: &str, plan: &str, transaction: &str) -> Result<()> {
        self.current()?;
        if let Some(held) = self.held.borrow().as_ref() {
            return require(
                held.session == session && held.plan == plan && held.transaction == transaction,
            );
        }
        let mut ingress = self.ingress.borrow_mut().take().ok_or(Error::Invalid)?;
        ingress
            .set_read_timeout(Some(Duration::from_secs(5)))
            .map_err(|_| Error::Io)?;
        let deadline = Instant::now() + Duration::from_secs(5);
        let mut byte = [0];
        let mut space = [std::mem::MaybeUninit::uninit(); rustix::cmsg_space!(ScmRights(1))];
        let mut ancillary = RecvAncillaryBuffer::new(&mut space);
        let message = store::io(rustix::net::recvmsg(
            &ingress,
            &mut [std::io::IoSliceMut::new(&mut byte)],
            &mut ancillary,
            RecvFlags::CMSG_CLOEXEC,
        ))?;
        let mut files = vec![];
        let mut unexpected = false;
        for message in ancillary.drain() {
            match message {
                RecvAncillaryMessage::ScmRights(fds) => files.extend(fds),
                _ => unexpected = true,
            }
        }
        require(
            !unexpected
                && message.bytes == 1
                && byte == *b"A"
                && files.len() == 1
                && !message
                    .flags
                    .intersects(ReturnFlags::CTRUNC | ReturnFlags::TRUNC),
        )?;
        let mut read = |bytes: &mut [u8]| -> Result<()> {
            let mut offset = 0;
            while offset < bytes.len() {
                ingress
                    .set_read_timeout(Some(
                        deadline
                            .checked_duration_since(Instant::now())
                            .ok_or(Error::Io)?,
                    ))
                    .map_err(|_| Error::Io)?;
                let n = ingress.read(&mut bytes[offset..]).map_err(|_| Error::Io)?;
                if n == 0 {
                    return Err(Error::Io);
                }
                offset += n;
            }
            Ok(())
        };
        let mut length = [0; 4];
        read(&mut length)?;
        let length = u32::from_be_bytes(length) as usize;
        require(length > 0 && length <= 4096)?;
        let mut raw = vec![0; length];
        read(&mut raw)?;
        let admit: Admit = codec::decode_canonical(&raw, false)?;
        require(
            admit.version == 1
                && admit.session == session
                && admit.plan == plan
                && admit.transaction == transaction
                && admit.data_root == self.identity,
        )?;
        let file = files.pop().ok_or(Error::Invalid)?;
        store::checked(&file, false, true, false)?;
        let identity = native::Identity::capture(&file)?;
        require(identity == admit.lock)?;
        // The coordination address is closed and derived locally; no caller path
        // or procfd reopen can substitute another lock or another OFD.
        let directory = format!(
            "/tmp/subyard-owner-locks-{}",
            rustix::process::getuid().as_raw()
        );
        let parent = store::io(fs::openat(
            fs::CWD,
            directory,
            OFlags::RDONLY | OFlags::DIRECTORY | store::flags(),
            Mode::empty(),
        ))?;
        store::checked(&parent, true, true, false)?;
        let name = format!(
            "{}.lock",
            digest(self.path.to_str().ok_or(Error::Invalid)?.as_bytes())
        );
        let linked = store::io(fs::statat(&parent, name, AtFlags::SYMLINK_NOFOLLOW))?;
        require(
            linked.st_dev == identity.device
                && linked.st_ino == identity.inode
                && linked.st_mode == identity.mode
                && linked.st_uid == identity.uid
                && linked.st_nlink == 1,
        )?;
        store::io(fs::flock(&file, FlockOperation::NonBlockingLockExclusive))?;
        self.current()?;
        ingress
            .set_write_timeout(Some(Duration::from_secs(5)))
            .map_err(|_| Error::Io)?;
        ingress
            .write_all(&codec::encode(
                &Ack {
                    version: 1,
                    session,
                    plan,
                    transaction,
                    accepted: true,
                },
                true,
            )?)
            .map_err(|_| Error::Io)?;
        self.held.replace(Some(Held {
            file,
            session: session.into(),
            plan: plan.into(),
            transaction: transaction.into(),
        }));
        Ok(())
    }
    pub(crate) fn borrowed(&self) -> Result<(native::Identity, OwnedFd)> {
        self.current()?;
        let held = self.held.borrow();
        let held = held.as_ref().ok_or(Error::Invalid)?;
        Ok((
            native::Identity::capture(&held.file)?,
            store::io(rustix::io::dup(&held.file))?,
        ))
    }
}
