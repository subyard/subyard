//! Original V1 source history. Only syntax crosses the frozen Go codec; history
//! eligibility and the exact original raw/physical binding remain Rust-owned.
use crate::{codec, digest, inventory, record::require, store, Error, Result};
use rustix::fs::{self, AtFlags, Mode, OFlags};
use serde::{Deserialize, Serialize};
use std::{
    collections::BTreeMap,
    io::Read,
    os::fd::{AsFd, OwnedFd},
};

#[derive(Default, Serialize, Deserialize)]
#[serde(default, rename_all = "camelCase", deny_unknown_fields)]
pub(crate) struct Transaction {
    pub(crate) schema_version: i64,
    pub(crate) from_layout: i64,
    pub(crate) to_layout: i64,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub(crate) from_runtime: String,
    pub(crate) to_release: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub(crate) to_runtime: String,
    pub(crate) phase: String,
    pub(crate) migrations: Option<Vec<String>>,
    #[serde(skip_serializing_if = "crate::wire::empty_vec")]
    pub(crate) entries: Option<Vec<Entry>>,
    #[serde(skip_serializing_if = "crate::wire::empty_vec")]
    pub(crate) operations: Option<Vec<Operation>>,
    #[serde(
        rename = "rollbackOperations",
        skip_serializing_if = "crate::wire::zero"
    )]
    pub(crate) rollback_ops: bool,
}
impl crate::legacy::Historical for Transaction {
    const KIND: crate::legacy::Kind = crate::legacy::Kind::LegacyV1Transaction;
}
#[derive(Default, Serialize, Deserialize)]
#[serde(default, rename_all = "camelCase", deny_unknown_fields)]
pub(crate) struct Entry {
    pub(crate) migration_id: String,
    pub(crate) scope: String,
    pub(crate) source: String,
    pub(crate) destination: String,
    pub(crate) consumer: String,
    pub(crate) recovery: String,
    #[serde(rename = "sha256", skip_serializing_if = "String::is_empty")]
    pub(crate) digest: String,
    #[serde(skip_serializing_if = "crate::wire::zero")]
    pub(crate) mode: u32,
}
#[derive(Default, Serialize, Deserialize)]
#[serde(default, rename_all = "camelCase", deny_unknown_fields)]
pub(crate) struct Operation {
    pub(crate) migration_id: String,
    pub(crate) operation_id: String,
    pub(crate) kind: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub(crate) before: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub(crate) phase: String,
}
#[derive(Default, PartialEq, Eq, Serialize)]
#[serde(rename_all = "PascalCase")]
struct Input {
    present: bool,
    digest: String,
    device: u64,
    inode: u64,
    links: u64,
    mode: u32,
    #[serde(rename = "UID")]
    uid: u32,
    #[serde(rename = "GID")]
    gid: u32,
}
#[allow(clippy::unnecessary_cast)]
fn metadata(s: fs::Stat, digest: String) -> Input {
    Input {
        present: true,
        digest,
        device: s.st_dev,
        inode: s.st_ino,
        links: s.st_nlink as u64,
        mode: s.st_mode,
        uid: s.st_uid,
        gid: s.st_gid,
    }
}
fn read(parent: &impl AsFd, name: &str) -> Result<(Option<Vec<u8>>, Input)> {
    let fd = match fs::openat(parent, name, OFlags::RDONLY | store::flags(), Mode::empty()) {
        Ok(fd) => fd,
        Err(rustix::io::Errno::NOENT) => return Ok((None, Input::default())),
        Err(_) => return Err(Error::Io),
    };
    store::checked(&fd, false, true, true)?;
    let before = store::io(fs::fstat(&fd))?;
    let mut file = std::fs::File::from(fd);
    let mut bytes = Vec::new();
    file.by_ref()
        .take((1 << 20) + 1)
        .read_to_end(&mut bytes)
        .map_err(|_| Error::Io)?;
    require(!bytes.is_empty() && bytes.len() <= 1 << 20)?;
    let after = store::io(fs::fstat(&file))?;
    let named = store::io(fs::statat(parent, name, AtFlags::SYMLINK_NOFOLLOW))?;
    require(
        metadata(before, String::new()) == metadata(after, String::new())
            && metadata(before, String::new()) == metadata(named, String::new())
            && before.st_size == after.st_size
            && after.st_size == bytes.len() as i64
            && before.st_mtime == after.st_mtime
            && before.st_mtime_nsec == after.st_mtime_nsec
            && before.st_ctime == after.st_ctime
            && before.st_ctime_nsec == after.st_ctime_nsec,
    )?;
    Ok((Some(bytes.clone()), metadata(before, digest(&bytes))))
}
// Same lexical acceptance/cleaning as the existing Linux filepath contract;
// the actual traversal still uses rooted no-follow descriptors.
fn relative(raw: &str) -> Result<Vec<String>> {
    require(!raw.is_empty() && !raw.starts_with('/') && !raw.contains(['\0', '\n', '\r', '\t']))?;
    let mut parts = Vec::new();
    for part in raw.split('/') {
        match part {
            "" | "." => (),
            ".." => {
                require(parts.pop().is_some())?;
            }
            _ => parts.push(part.to_owned()),
        }
    }
    require(!parts.is_empty())?;
    Ok(parts)
}
fn recovery_parent(root: &OwnedFd, parts: &[String]) -> Result<Option<OwnedFd>> {
    // V1 bound the recovery LEAF's ownership/mode, not new metadata for every
    // nested recovery directory. Preserve that distinction while refusing all
    // symlink traversal beneath the already protected transaction root.
    let mut fd = store::io(rustix::io::dup(root))?;
    for part in parts {
        fd = match fs::openat(
            &fd,
            part,
            OFlags::RDONLY | OFlags::DIRECTORY | store::flags(),
            Mode::empty(),
        ) {
            Ok(fd) => fd,
            Err(rustix::io::Errno::NOENT) => return Ok(None),
            Err(_) => return Err(Error::Io),
        };
    }
    Ok(Some(fd))
}
pub(crate) fn binding(store: &store::Store, logical: &[u8]) -> Result<String> {
    let root = store.root.as_ref().ok_or(Error::Protected)?;
    let mut values = BTreeMap::new();
    let migrations = store::directory(root, &["migrations".into()], false)?;
    let transactions = migrations
        .as_ref()
        .map(|m| store::directory(m, &["transactions".into()], false))
        .transpose()?
        .flatten();
    let root_path = [logical, b"/migrations"].concat();
    let join = |root: &[u8], tail: &str| [root, b"/", tail.as_bytes()].concat();
    let mut add_dir = |name: Vec<u8>, fd: Option<&OwnedFd>| -> Result<()> {
        values.insert(
            name,
            fd.map(|f| store::io(fs::fstat(f)).map(|s| metadata(s, String::new())))
                .transpose()?
                .unwrap_or_default(),
        );
        Ok(())
    };
    add_dir(root_path.clone(), migrations.as_ref())?;
    add_dir(join(&root_path, "transactions"), transactions.as_ref())?;
    let state = if let Some(m) = &migrations {
        read(m, "state.json")?.1
    } else {
        Input::default()
    };
    if !state.present && transactions.is_none() {
        values.remove(&root_path);
    }
    values.insert(join(&root_path, "state.json"), state);
    if let Some(parent) = &transactions {
        for name in inventory::names(parent, 64)? {
            let parts = relative(&name)?;
            require(parts.len() == 1 && parts[0] == name)?;
            let dir = store::directory(parent, std::slice::from_ref(&name), false)?
                .ok_or(Error::Stale)?;
            store::linked_directory(parent, &name, &dir)?;
            let path = join(&root_path, &format!("transactions/{name}"));
            values.insert(
                path.clone(),
                metadata(store::io(fs::fstat(&dir))?, String::new()),
            );
            let (raw, input) = read(&dir, "transaction.json")?;
            let tx: Transaction =
                store.decode(raw.as_deref().ok_or(Error::Invalid)?, false, false)?;
            require(
                tx.schema_version == 1
                    && tx.phase == "committed"
                    && name == digest(tx.to_release.as_bytes())[..32],
            )?;
            values.insert(join(&path, "transaction.json"), input);
            for entry in tx.entries.iter().flatten() {
                let mut parts = relative(&entry.recovery)?;
                let leaf = parts.pop().ok_or(Error::Invalid)?;
                let suffix = parts
                    .iter()
                    .map(String::as_str)
                    .chain(std::iter::once(leaf.as_str()))
                    .collect::<Vec<_>>()
                    .join("/");
                require(
                    path.len()
                        .checked_add(1 + suffix.len())
                        .ok_or(Error::Bound)?
                        < 4096,
                )?;
                let parent = recovery_parent(&dir, &parts)?;
                let input = if let Some(parent) = parent {
                    read(&parent, &leaf)?.1
                } else {
                    Input::default()
                };
                values.insert(join(&path, &suffix), input);
            }
            store::linked_directory(parent, &name, &dir)?;
        }
    }
    codec::source_hash_with(|out| codec::write_source_map(out, &values))
}
