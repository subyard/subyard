//! Linux private publication for grant-bearing migration-owner receipts/completion.
use crate::{
    codec::MAX_BYTES,
    inventory::Namespace,
    lease::UpdateLease,
    record::safe_id,
    store::{self, Address},
    Error, Result,
};
use rustix::fs::{self, AtFlags, FileType, Mode, OFlags};
use std::{
    fs::File,
    io::Write,
    os::{fd::AsRawFd, unix::fs::FileExt},
};
impl UpdateLease<'_> {
    pub(crate) fn create_owner_immutable(&self, address: &Address, payload: &[u8]) -> Result<()> {
        if payload.is_empty() || payload.len() > MAX_BYTES {
            return Err(Error::Bound);
        }
        if ![
            Namespace::MigrationOwner.parts(),
            Namespace::MigrationCompleted.parts(),
        ]
        .contains(&address.parents)
            || !address.name.strip_suffix(".json").is_some_and(safe_id)
        {
            return Err(Error::Invalid);
        }
        let root = self.store.root.as_ref().ok_or(Error::Io)?;
        store::checked(root, true, false, false)?;
        if let Some(parent) = store::directory(root, &address.parents, false)? {
            if let Some(current) = store::read_at(&parent, &address.name, false)?.bytes() {
                if current != payload {
                    return Err(Error::Exists);
                }
                return store::io(fs::fsync(&parent));
            }
            let pending = store::read_at(&parent, &format!(".{}.pending", address.name), false)?;
            if let Some(bytes) = pending.bytes() {
                if bytes != payload {
                    return Err(Error::Exists);
                }
                return self.create_immutable(address, payload);
            }
        }
        // Admit O_TMPFILE on the nearest existing ancestor before creating descendants.
        let mut anchor = None;
        for length in (0..=address.parents.len()).rev() {
            if let Some(fd) = store::directory(root, &address.parents[..length], false)? {
                anchor = Some(fd);
                break;
            }
        }
        let anchor = anchor.ok_or(Error::Io)?;
        self.store.fault("before-private-create");
        let private = store::io(fs::openat(
            &anchor,
            ".",
            OFlags::TMPFILE | OFlags::RDWR | OFlags::CLOEXEC,
            Mode::from_raw_mode(0o600),
        ))?;
        store::io(fs::fchmod(&private, Mode::from_raw_mode(0o600)))?;
        let initial = store::io(fs::fstat(&private))?;
        if FileType::from_raw_mode(initial.st_mode) != FileType::RegularFile
            || initial.st_mode & 0o777 != 0o600
            || initial.st_uid != rustix::process::getuid().as_raw()
            || initial.st_nlink != 0
            || initial.st_size != 0
        {
            return Err(Error::Protected);
        }
        let procfd = format!("/proc/self/fd/{}", private.as_raw_fd());
        let proc = store::io(fs::stat(&procfd))?;
        if proc.st_dev != initial.st_dev || proc.st_ino != initial.st_ino {
            return Err(Error::Protected);
        }
        self.store.fault("after-private-create");
        let mut file = File::from(private);
        file.write_all(payload).map_err(|_| Error::Io)?;
        file.sync_all().map_err(|_| Error::Io)?;
        let mut readback = vec![0; payload.len()];
        file.read_exact_at(&mut readback, 0)
            .map_err(|_| Error::Io)?;
        let complete = store::io(fs::fstat(&file))?;
        if complete.st_dev != initial.st_dev
            || complete.st_ino != initial.st_ino
            || complete.st_mode != initial.st_mode
            || complete.st_uid != initial.st_uid
            || complete.st_nlink != 0
            || complete.st_size != payload.len() as i64
            || readback != payload
        {
            return Err(Error::Protected);
        }
        self.store.fault("after-pending-fsync");
        let parent = store::directory(root, &address.parents, true)?.ok_or(Error::Io)?;
        if store::io(fs::fstat(&parent))?.st_dev != initial.st_dev {
            return Err(Error::Protected);
        }
        if store::read_at(&parent, &address.name, false)?
            .bytes()
            .is_some()
        {
            return Err(Error::Exists);
        }
        self.store.fault("before-publish");
        match fs::linkat(
            fs::CWD,
            &procfd,
            &parent,
            &address.name,
            AtFlags::SYMLINK_FOLLOW,
        ) {
            Ok(()) => {}
            Err(rustix::io::Errno::EXIST) => return Err(Error::Exists),
            Err(_) => return Err(Error::Io),
        }
        let linked = store::io(fs::statat(
            &parent,
            &address.name,
            AtFlags::SYMLINK_NOFOLLOW,
        ))?;
        if linked.st_dev != initial.st_dev
            || linked.st_ino != initial.st_ino
            || linked.st_nlink != 1
            || store::read_at(&parent, &address.name, false)?.bytes() != Some(payload)
        {
            return Err(Error::Protected);
        }
        self.store.fault("after-publish-before-dir-fsync");
        store::io(fs::fsync(&parent))?;
        self.store.fault("after-dir-fsync");
        Ok(())
    }
}
