//! Bounded exact-tree cleanup of closed ordinary transaction addresses.
use crate::{
    graph::State,
    inventory,
    record::{self, require},
    store::{self, Store},
    Error, Result,
};
use rustix::fs::{self, AtFlags, FileType};
use std::{collections::BTreeMap, os::fd::AsFd};
#[derive(serde::Serialize, PartialEq, Eq)]
struct Identity {
    device: u64,
    inode: u64,
    mode: u32,
    uid: u32,
    gid: u32,
}
fn identity(fd: &impl AsFd) -> Result<Identity> {
    let s = store::io(fs::fstat(fd))?;
    Ok(Identity {
        device: s.st_dev,
        inode: s.st_ino,
        mode: s.st_mode,
        uid: s.st_uid,
        gid: s.st_gid,
    })
}
enum Child {
    File(store::Snapshot),
    Directory(Tree),
}
struct Tree {
    identity: Identity,
    children: BTreeMap<String, Child>,
}
fn snapshot(parent: &impl AsFd, count: &mut usize, bytes: &mut u64, depth: usize) -> Result<Tree> {
    if depth > 64 {
        return Err(Error::Bound);
    }
    store::checked(parent, true, true, false)?;
    let id = identity(parent)?;
    let names = inventory::names(parent, *count)?;
    *count = count.checked_sub(names.len()).ok_or(Error::Bound)?;
    let mut children = BTreeMap::new();
    for name in names {
        let stat = store::io(fs::statat(parent, &name, AtFlags::SYMLINK_NOFOLLOW))?;
        let child = if FileType::from_raw_mode(stat.st_mode) == FileType::Directory {
            let fd = store::directory(parent, std::slice::from_ref(&name), false)?
                .ok_or(Error::Stale)?;
            Child::Directory(snapshot(&fd, count, bytes, depth + 1)?)
        } else {
            let value = store::read_at(parent, &name, true)?;
            *bytes = bytes
                .checked_sub(value.bytes().ok_or(Error::Stale)?.len() as u64)
                .ok_or(Error::Bound)?;
            Child::File(value)
        };
        children.insert(name, child);
    }
    require(identity(parent)? == id)?;
    Ok(Tree {
        identity: id,
        children,
    })
}
fn check_link(parent: &impl AsFd, name: &str, fd: &impl AsFd, id: &Identity) -> Result<()> {
    let stat = store::io(fs::statat(parent, name, AtFlags::SYMLINK_NOFOLLOW))?;
    require(
        identity(fd)? == *id
            && stat.st_dev == id.device
            && stat.st_ino == id.inode
            && stat.st_mode == id.mode
            && stat.st_uid == id.uid
            && stat.st_gid == id.gid,
    )
}
fn remove(store: &Store, parent: &impl AsFd, name: &str, tree: &Tree) -> Result<()> {
    let fd = store::directory(parent, &[name.into()], false)?.ok_or(Error::Stale)?;
    check_link(parent, name, &fd, &tree.identity)?;
    require(
        inventory::names(&fd, tree.children.len())?
            == tree.children.keys().cloned().collect::<Vec<_>>(),
    )?;
    for (child, value) in &tree.children {
        check_link(parent, name, &fd, &tree.identity)?;
        match value {
            Child::File(expected) => {
                require(store::read_at(&fd, child, true)? == *expected)?;
                store::io(fs::unlinkat(&fd, child, AtFlags::empty()))?;
                store.fault("after-ordinary-unlink");
                store::io(fs::fsync(&fd))?
            }
            Child::Directory(nested) => remove(store, &fd, child, nested)?,
        }
    }
    check_link(parent, name, &fd, &tree.identity)?;
    require(inventory::names(&fd, 0)?.is_empty())?;
    store::io(fs::unlinkat(parent, name, AtFlags::REMOVEDIR))?;
    store.fault("after-ordinary-unlink");
    store::io(fs::fsync(parent))
}
impl crate::lease::UpdateLease<'_> {
    pub fn cleanup_transactions(&self, current: &str) -> Result<()> {
        self.store.native_inventory(None, false)?;
        require(record::safe_id(current))?;
        let selected = self.store.read(&store::Address::journal())?;
        if let Some(bytes) = selected.bytes() {
            let journal = record::Journal::new(self.store.decode(bytes, true, false)?)?;
            require(
                journal.wire().transaction == current && journal.wire().checkpoint == "complete",
            )?;
        }
        let state = State::read(self.store)?;
        let selected = state.ordinary_cleanup_for(current, 32)?;
        let Some(root) = &self.store.root else {
            return Ok(());
        };
        let Some(parent) = store::directory(
            root,
            &[
                "release-transition".into(),
                "v2".into(),
                "transactions".into(),
            ],
            false,
        )?
        else {
            return Ok(());
        };
        let mut count = 8192;
        let mut bytes = 32u64 << 20;
        let mut trees = Vec::new();
        for id in &selected {
            let fd =
                store::directory(&parent, std::slice::from_ref(id), false)?.ok_or(Error::Stale)?;
            trees.push((id, snapshot(&fd, &mut count, &mut bytes, 0)?));
        }
        require(state.binding() == State::read(self.store)?.binding())?;
        for (id, tree) in trees {
            remove(self.store, &parent, id, &tree)?;
        }
        Ok(())
    }
}
