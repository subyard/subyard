//! Physical facts for closed retained releases. Release policy stays in Owner.
use crate::{
    codec, digest, inventory,
    record::{self, require},
    store, Error, Result,
};
use rustix::fs::{self, AtFlags, FileType, Mode, OFlags};
use std::{
    collections::{BTreeMap, BTreeSet},
    io::Read,
    os::{
        fd::{AsFd, OwnedFd},
        unix::ffi::OsStrExt,
    },
};
fn regular(fd: &impl AsFd) -> Result<()> {
    let s = store::io(fs::fstat(fd))?;
    require(
        FileType::from_raw_mode(s.st_mode) == FileType::RegularFile
            && (s.st_uid == 0 || s.st_uid == rustix::process::getuid().as_raw())
            && s.st_nlink == 1,
    )
}
fn directory(fd: &impl AsFd) -> Result<()> {
    let s = store::io(fs::fstat(fd))?;
    require(
        FileType::from_raw_mode(s.st_mode) == FileType::Directory
            && (s.st_uid == 0 || s.st_uid == rustix::process::getuid().as_raw())
            && s.st_mode & 0o022 == 0,
    )
}
fn open(parent: &impl AsFd, path: &str) -> Result<OwnedFd> {
    open_bytes(parent, path.as_bytes())
}
fn open_bytes(parent: &impl AsFd, path: &[u8]) -> Result<OwnedFd> {
    require(
        !path.is_empty()
            && !path.starts_with(b"/")
            && !path.contains(&b'\\')
            && !path.contains(&0)
            && path.len() <= 4096,
    )?;
    let mut fd = store::io(rustix::io::dup(parent))?;
    let parts = path.split(|b| *b == b'/').collect::<Vec<_>>();
    for (i, p) in parts.iter().enumerate() {
        require(!p.is_empty() && !matches!(*p, b"." | b".."))?;
        fd = store::io(fs::openat(
            &fd,
            std::ffi::OsStr::from_bytes(p),
            OFlags::RDONLY
                | store::flags()
                | if i + 1 < parts.len() {
                    OFlags::DIRECTORY
                } else {
                    OFlags::empty()
                },
            Mode::empty(),
        ))?;
        if i + 1 < parts.len() {
            directory(&fd)?;
        }
    }
    regular(&fd)?;
    Ok(fd)
}
// A phase ends before any native capture or authority advance. Callers create
// another phase for the independent full verification after that boundary.
// This proof cannot be decoded, supplied by a caller, or retained in a bundle.
pub(crate) struct VerificationPhase {
    inventories: Vec<VerifiedInventory>,
}
struct VerifiedInventory {
    release: String,
    identity: crate::native::Identity,
    manifest: String,
    _root: OwnedFd,
    entries: Entries,
}
impl VerificationPhase {
    pub(crate) fn new() -> Self {
        Self {
            inventories: Vec::new(),
        }
    }
    pub(crate) fn verify(
        &mut self,
        release: &str,
        root: &OwnedFd,
        manifest: &str,
    ) -> Result<&Entries> {
        require(record::safe_id(release) && record::fingerprint(manifest))?;
        let identity = crate::native::Identity::capture(root)?;
        let index = if let Some(index) = self.inventories.iter().position(|proof| {
            proof.release == release && proof.identity == identity && proof.manifest == manifest
        }) {
            index
        } else {
            // The closed operation has only source, target and previous roots.
            require(self.inventories.len() < 3)?;
            let entries = verify(root, manifest)?;
            require(crate::native::Identity::capture(root)? == identity)?;
            self.inventories.push(VerifiedInventory {
                release: release.into(),
                identity,
                manifest: manifest.into(),
                _root: store::io(rustix::io::dup(root))?,
                entries,
            });
            self.inventories.len() - 1
        };
        Ok(&self.inventories[index].entries)
    }
}
pub(crate) struct Entries(BTreeMap<Vec<u8>, String>);
impl Entries {
    pub(crate) fn get(&self, fixed: &str) -> Option<&String> {
        self.0.get(fixed.as_bytes())
    }
    pub(crate) fn contains_key(&self, fixed: &str) -> bool {
        self.0.contains_key(fixed.as_bytes())
    }
    #[cfg(test)]
    fn len(&self) -> usize {
        self.0.len()
    }
}
pub(crate) fn verify(root: &OwnedFd, expected: &str) -> Result<Entries> {
    Ok(Entries(verify_bytes(root, expected)?))
}
pub(crate) fn verify_bytes(root: &OwnedFd, expected: &str) -> Result<BTreeMap<Vec<u8>, String>> {
    require(record::fingerprint(expected))?;
    directory(root)?;
    let manifest = open(root, "runtime-files.sha256")?;
    let mut raw = vec![];
    std::fs::File::from(manifest)
        .take((1 << 20) + 1)
        .read_to_end(&mut raw)
        .map_err(|_| Error::Io)?;
    require(
        !raw.is_empty()
            && raw.len() <= 1 << 20
            && raw.last() == Some(&b'\n')
            && digest(&raw) == expected,
    )?;
    let mut entries = BTreeMap::new();
    for line in raw[..raw.len() - 1].split(|b| *b == b'\n') {
        require(line.len() >= 68 && line.get(64..68) == Some(b"  ./"))?;
        let hash = std::str::from_utf8(&line[..64]).map_err(|_| Error::Invalid)?;
        require(hash.bytes().all(|b| b.is_ascii_hexdigit()))?;
        let name = &line[68..];
        let fd = open_bytes(root, name)?;
        if matches!(name, b"bin/yard-engine" | b"bin/subyard-release-owner") {
            require(store::io(fs::fstat(&fd))?.st_mode & 0o111 != 0)?;
        }
        require(
            entries
                .insert(name.to_vec(), hash.to_ascii_lowercase())
                .is_none(),
        )?;
        use sha2::{Digest, Sha256};
        let mut sha = Sha256::new();
        let before = store::io(fs::fstat(&fd))?;
        require(before.st_size >= 0)?;
        let mut file = std::fs::File::from(fd)
            .take((before.st_size as u64).checked_add(1).ok_or(Error::Bound)?);
        let mut buffer = [0; 65536];
        let mut length = 0u64;
        loop {
            let n = file.read(&mut buffer).map_err(|_| Error::Io)?;
            if n == 0 {
                break;
            }
            length = length.checked_add(n as u64).ok_or(Error::Bound)?;
            sha.update(&buffer[..n]);
        }
        let after = store::io(fs::fstat(file.get_ref()))?;
        require(
            length == before.st_size as u64
                && after.st_size == before.st_size
                && after.st_mtime == before.st_mtime
                && after.st_mtime_nsec == before.st_mtime_nsec
                && after.st_ctime == before.st_ctime
                && after.st_ctime_nsec == before.st_ctime_nsec
                && sha
                    .finalize()
                    .iter()
                    .map(|b| format!("{b:02x}"))
                    .collect::<String>()
                    == hash.to_ascii_lowercase(),
        )?;
    }
    fn walk(
        root: &OwnedFd,
        prefix: &[u8],
        remaining: &mut BTreeSet<Vec<u8>>,
        count: &mut usize,
        depth: usize,
    ) -> Result<()> {
        require(depth <= 2048)?;
        let names = inventory::byte_names(root, *count)?;
        *count = count.checked_sub(names.len()).ok_or(Error::Bound)?;
        for name in names {
            let path = if prefix.is_empty() {
                name.clone()
            } else {
                [prefix, b"/", name.as_slice()].concat()
            };
            let s = store::io(fs::statat(
                root,
                std::ffi::OsStr::from_bytes(&name),
                AtFlags::SYMLINK_NOFOLLOW,
            ))?;
            if FileType::from_raw_mode(s.st_mode) == FileType::Directory {
                let fd = store::io(fs::openat(
                    root,
                    std::ffi::OsStr::from_bytes(&name),
                    OFlags::RDONLY | OFlags::DIRECTORY | store::flags(),
                    Mode::empty(),
                ))?;
                directory(&fd)?;
                walk(&fd, &path, remaining, count, depth + 1)?;
            } else {
                require(
                    FileType::from_raw_mode(s.st_mode) == FileType::RegularFile
                        && (path == b"runtime-files.sha256" || remaining.remove(&path)),
                )?;
            }
        }
        Ok(())
    }
    let mut remaining = entries.keys().cloned().collect();
    walk(root, b"", &mut remaining, &mut 32768, 0)?;
    require(remaining.is_empty())?;
    Ok(entries)
}

#[derive(Clone, Default, PartialEq, Eq, serde::Serialize)]
#[serde(rename_all = "PascalCase")]
pub(crate) struct Input {
    pub(crate) present: bool,
    pub(crate) device: u64,
    pub(crate) inode: u64,
    pub(crate) links: u64,
    pub(crate) mode: u32,
    #[serde(rename = "UID")]
    pub(crate) uid: u32,
    #[serde(rename = "GID")]
    pub(crate) gid: u32,
}
pub(crate) fn metadata(fd: &impl AsFd) -> Result<Input> {
    Ok(input(store::io(fs::fstat(fd))?))
}
#[allow(clippy::unnecessary_cast)]
pub(crate) fn input(s: fs::Stat) -> Input {
    Input {
        present: true,
        device: s.st_dev,
        inode: s.st_ino,
        links: s.st_nlink as u64,
        mode: s.st_mode,
        uid: s.st_uid,
        gid: s.st_gid,
    }
}
#[derive(Clone, Default, PartialEq, Eq)]
pub(crate) struct Inputs(pub(crate) BTreeMap<Vec<u8>, Input>);
impl Inputs {
    pub(crate) fn extend(&mut self, other: Self) {
        self.0.extend(other.0);
    }
    pub(crate) fn fingerprint(&self) -> Result<String> {
        codec::source_hash_with(|out| codec::write_source_map(out, &self.0))
    }
}
pub(crate) fn inputs(root: &OwnedFd, logical: &str, manifest: &str) -> Result<Inputs> {
    inputs_bytes(root, logical.as_bytes(), manifest)
}
pub(crate) fn inputs_bytes(root: &OwnedFd, logical: &[u8], manifest: &str) -> Result<Inputs> {
    require(logical.starts_with(b"/") && !logical.ends_with(b"/"))?;
    let entries = verify_bytes(root, manifest)?;
    let capture = || -> Result<Inputs> {
        let mut values = BTreeMap::new();
        values.insert(logical.to_vec(), metadata(root)?);
        for name in std::iter::once(b"runtime-files.sha256".as_slice())
            .chain(entries.keys().map(Vec::as_slice))
        {
            values.insert(
                [logical, b"/", name].concat(),
                metadata(&open_bytes(root, name)?)?,
            );
        }
        Ok(Inputs(values))
    };
    let before = capture()?;
    verify_bytes(root, manifest)?;
    require(before == capture()?)?;
    Ok(before)
}
pub(crate) fn read(root: &OwnedFd, path: &str, limit: usize) -> Result<Vec<u8>> {
    directory(root)?;
    let fd = open(root, path)?;
    let stat = store::io(fs::fstat(&fd))?;
    require(stat.st_size > 0 && stat.st_size as u64 <= limit as u64)?;
    let mut raw = vec![];
    std::fs::File::from(fd)
        .take((limit as u64).checked_add(1).ok_or(Error::Bound)?)
        .read_to_end(&mut raw)
        .map_err(|_| Error::Io)?;
    require(!raw.is_empty() && raw.len() <= limit)?;
    Ok(raw)
}
pub(crate) fn registry(root: &OwnedFd) -> Result<Vec<u8>> {
    read(root, "config/release-transition.json", 1 << 20)
}
pub(crate) fn source_engine(root: &OwnedFd) -> Result<std::fs::File> {
    Ok(std::fs::File::from(open(root, "bin/yard-engine")?))
}
pub(crate) fn original_v1_registry(root: &OwnedFd) -> Result<OwnedFd> {
    open(root, "config/migrations.json")
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::PermissionsExt;
    #[test]
    fn hashes_actual_bytes_and_refuses_extra_symlink_fifo() {
        let root =
            std::path::PathBuf::from(format!("/tmp/subyard-owner-assets-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&root);
        std::fs::create_dir(&root).unwrap();
        std::fs::set_permissions(&root, std::fs::Permissions::from_mode(0o700)).unwrap();
        struct Cleanup(std::path::PathBuf);
        impl Drop for Cleanup {
            fn drop(&mut self) {
                let _ = std::fs::remove_dir_all(&self.0);
            }
        }
        let _cleanup = Cleanup(root.clone());
        std::fs::create_dir(root.join("bin")).unwrap();
        std::fs::set_permissions(root.join("bin"), std::fs::Permissions::from_mode(0o700)).unwrap();
        std::fs::write(root.join("bin/yard-engine"), b"sealed fixture image").unwrap();
        std::fs::set_permissions(
            root.join("bin/yard-engine"),
            std::fs::Permissions::from_mode(0o700),
        )
        .unwrap();
        let manifest = format!("{}  ./bin/yard-engine\n", digest(b"sealed fixture image"));
        std::fs::write(root.join("runtime-files.sha256"), &manifest).unwrap();
        let fd = store::io(fs::open(
            &root,
            OFlags::RDONLY | OFlags::DIRECTORY | store::flags(),
            Mode::empty(),
        ))
        .unwrap();
        assert_eq!(verify(&fd, &digest(manifest.as_bytes())).unwrap().len(), 1);
        let manifest_digest = digest(manifest.as_bytes());
        let mut phase = VerificationPhase::new();
        assert_eq!(
            phase
                .verify("source", &fd, &manifest_digest)
                .unwrap()
                .get("bin/yard-engine"),
            Some(&digest(b"sealed fixture image"))
        );
        // Source and target roles may consume the same physical proof inside
        // one phase. A new phase must hash actual bytes again.
        assert!(phase.verify("source", &fd, &manifest_digest).is_ok());
        assert_eq!(phase.inventories.len(), 1);
        let other = root.join("other-root");
        std::fs::create_dir(&other).unwrap();
        std::fs::set_permissions(&other, std::fs::Permissions::from_mode(0o700)).unwrap();
        let other_fd = store::io(fs::open(
            &other,
            OFlags::RDONLY | OFlags::DIRECTORY | store::flags(),
            Mode::empty(),
        ))
        .unwrap();
        assert!(phase.verify("source", &other_fd, &manifest_digest).is_err());
        drop(other_fd);
        std::fs::remove_dir(other).unwrap();
        std::fs::write(root.join("bin/yard-engine"), b"changed image").unwrap();
        assert!(VerificationPhase::new()
            .verify("source", &fd, &manifest_digest)
            .is_err());
        // Neither another logical release nor another manifest can consume
        // the original proof, even with the identical root descriptor.
        assert!(phase.verify("target", &fd, &manifest_digest).is_err());
        assert!(phase
            .verify("source", &fd, &digest(b"other manifest"))
            .is_err());
        std::fs::remove_file(root.join("bin/yard-engine")).unwrap();
        std::fs::write(root.join("bin/yard-engine"), b"sealed fixture image").unwrap();
        std::fs::set_permissions(
            root.join("bin/yard-engine"),
            std::fs::Permissions::from_mode(0o700),
        )
        .unwrap();
        // An identical byte replacement of a regular leaf remains valid;
        // incidental leaf inode changes do not enter the source authority.
        assert!(VerificationPhase::new()
            .verify("source", &fd, &manifest_digest)
            .is_ok());
        std::fs::set_permissions(
            root.join("bin/yard-engine"),
            std::fs::Permissions::from_mode(0o600),
        )
        .unwrap();
        assert!(verify(&fd, &digest(manifest.as_bytes())).is_err());
        std::fs::set_permissions(
            root.join("bin/yard-engine"),
            std::fs::Permissions::from_mode(0o700),
        )
        .unwrap();
        std::fs::write(root.join("bin/yard-engine"), b"changed image").unwrap();
        assert!(verify(&fd, &digest(manifest.as_bytes())).is_err());
        std::fs::write(root.join("bin/yard-engine"), b"sealed fixture image").unwrap();
        std::fs::set_permissions(
            root.join("bin/yard-engine"),
            std::fs::Permissions::from_mode(0o700),
        )
        .unwrap();
        std::fs::write(root.join("unlisted"), b"extra").unwrap();
        assert!(verify(&fd, &digest(manifest.as_bytes())).is_err());
        std::fs::remove_file(root.join("unlisted")).unwrap();
        std::fs::remove_file(root.join("bin/yard-engine")).unwrap();
        std::os::unix::fs::symlink("../runtime-files.sha256", root.join("bin/yard-engine"))
            .unwrap();
        assert!(verify(&fd, &digest(manifest.as_bytes())).is_err());
        std::fs::remove_file(root.join("bin/yard-engine")).unwrap();
        fs::mknodat(
            &fd,
            "bin/yard-engine",
            FileType::Fifo,
            Mode::from_raw_mode(0o600),
            0,
        )
        .unwrap();
        let start = std::time::Instant::now();
        assert!(verify(&fd, &digest(manifest.as_bytes())).is_err());
        assert!(start.elapsed() < std::time::Duration::from_secs(1));
    }
}
