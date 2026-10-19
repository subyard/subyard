use crate::{Failure, Result};
use rustix::fs::{self, Mode, OFlags};
use std::{
    fs::File,
    io::{Read, Write},
    os::fd::{AsFd, OwnedFd},
};
const LIMIT: u64 = 1_048_576;
pub struct Store {
    directory: OwnedFd,
}
pub fn checked(file: &impl AsFd, directory: bool, mode: Option<u32>) -> Result<()> {
    let st = fs::fstat(file).map_err(|_| Failure("metadata"))?;
    let kind = if directory {
        rustix::fs::FileType::Directory
    } else {
        rustix::fs::FileType::RegularFile
    };
    if rustix::fs::FileType::from_raw_mode(st.st_mode) != kind
        || st.st_uid != rustix::process::geteuid().as_raw()
        || st.st_mode & 0o022 != 0
        || (!directory && st.st_nlink != 1)
        || mode.is_some_and(|m| st.st_mode & 0o777 != m)
    {
        return Err(Failure("protected-mode"));
    };
    Ok(())
}
pub fn open(root: &impl AsFd, name: &str, flags: OFlags) -> Result<OwnedFd> {
    fs::openat(
        root,
        name,
        flags | OFlags::NOFOLLOW | OFlags::CLOEXEC,
        Mode::empty(),
    )
    .map_err(|_| Failure("open"))
}
impl Store {
    pub fn new(root: &impl AsFd) -> Result<Self> {
        let created = match fs::mkdirat(root, ".owner", Mode::from_raw_mode(0o700)) {
            Ok(()) => true,
            Err(rustix::io::Errno::EXIST) => false,
            Err(_) => return Err(Failure("store-create")),
        };
        let directory = open(root, ".owner", OFlags::RDONLY | OFlags::DIRECTORY)?;
        if created {
            fs::fchmod(&directory, Mode::from_raw_mode(0o700)).map_err(|_| Failure("store-mode"))?
        }
        checked(&directory, true, Some(0o700))?;
        fs::fsync(root).map_err(|_| Failure("store-sync"))?;
        Ok(Self { directory })
    }
    pub fn read(&self) -> Result<Option<Vec<u8>>> {
        let fd = match fs::openat(
            &self.directory,
            "record.json",
            OFlags::RDONLY | OFlags::NONBLOCK | OFlags::NOFOLLOW | OFlags::CLOEXEC,
            Mode::empty(),
        ) {
            Ok(fd) => fd,
            Err(rustix::io::Errno::NOENT) => return Ok(None),
            Err(_) => return Err(Failure("record-open")),
        };
        checked(&fd, false, Some(0o600))?;
        let mut bytes = vec![];
        File::from(fd)
            .take(LIMIT + 1)
            .read_to_end(&mut bytes)
            .map_err(|_| Failure("record-read"))?;
        if bytes.len() as u64 > LIMIT {
            return Err(Failure("record-bound"));
        };
        Ok(Some(bytes))
    }
    pub fn write(&self, bytes: &[u8]) -> Result<()> {
        if bytes.len() as u64 > LIMIT {
            return Err(Failure("record-bound"));
        }
        let fd = fs::openat(
            &self.directory,
            "record.pending",
            OFlags::WRONLY | OFlags::CREATE | OFlags::EXCL | OFlags::NOFOLLOW | OFlags::CLOEXEC,
            Mode::from_raw_mode(0o600),
        )
        .map_err(|_| Failure("record-pending"))?;
        fs::fchmod(&fd, Mode::from_raw_mode(0o600)).map_err(|_| Failure("record-mode"))?;
        checked(&fd, false, Some(0o600))?;
        let mut file = File::from(fd);
        file.write_all(bytes).map_err(|_| Failure("record-write"))?;
        file.sync_all().map_err(|_| Failure("record-sync"))?;
        fs::renameat(
            &self.directory,
            "record.pending",
            &self.directory,
            "record.json",
        )
        .map_err(|_| Failure("record-publish"))?;
        fs::fsync(&self.directory).map_err(|_| Failure("record-dir-sync"))?;
        Ok(())
    }
}
