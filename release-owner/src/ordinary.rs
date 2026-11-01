//! Bounded exact-tree cleanup of closed ordinary transaction addresses.
use crate::{
    graph::State,
    inventory,
    record::{self, require},
    store::{self, Store},
    Error, Result,
};
use rustix::fs::{self, AtFlags, FileType};
use std::{
    collections::{BTreeMap, BTreeSet},
    os::fd::AsFd,
};
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
        self.cleanup_transactions_with_history(current, None)
    }
    pub(crate) fn cleanup_migration_transactions(
        &self,
        current: &str,
        registry: &record::Registry,
        registry_digest: &str,
        endpoints: &crate::wire::recovery_v1::ReleaseLinks,
    ) -> Result<()> {
        crate::recovery::links(endpoints)?;
        self.cleanup_transactions_with_history(
            current,
            Some((registry, registry_digest, endpoints)),
        )
    }
    fn cleanup_transactions_with_history(
        &self,
        current: &str,
        history: Option<(
            &record::Registry,
            &str,
            &crate::wire::recovery_v1::ReleaseLinks,
        )>,
    ) -> Result<()> {
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
        let mut retained = BTreeSet::new();
        if let Some((registry, registry_digest, endpoints)) = history {
            let ledger = self.store.migration_ledger(registry, registry_digest)?;
            // Closed historical records prove retention, never an authorization.
            for transaction in state.ordinary_cleanup_for(current, 8192)? {
                let terminal = self.store.reconstruct_terminal(&transaction, registry)?;
                let advanced = terminal
                    .steps()
                    .iter()
                    .filter(|s| s.id == format!("{}.ledger", s.migration))
                    .map(|s| &s.migration)
                    .collect::<BTreeSet<_>>();
                require(
                    terminal
                        .steps()
                        .iter()
                        .all(|s| advanced.contains(&s.migration)),
                )?;
                let endpoint = terminal.releases().target == endpoints.active
                    || endpoints.previous.as_ref() == Some(&terminal.releases().target);
                let projection = ledger
                    .checkpoint()
                    .is_some_and(|c| c.wire().legacy_projection.exists)
                    && terminal.steps().iter().any(|s| {
                        s.id == format!("{}.ledger", s.migration)
                            && s.desired == ledger.projection().fingerprint()
                    })
                    && !terminal.steps().iter().any(|s| {
                        s.id == format!("{}.ledger", s.migration)
                            && s.expected == ledger.projection().fingerprint()
                    });
                if endpoint || projection {
                    retained.insert(transaction);
                }
            }
        }
        let selected = state.ordinary_cleanup_retaining(current, &retained, 32)?;
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

#[cfg(test)]
mod migration_retention_tests {
    use crate::{
        codec, digest, migration_owner, record,
        wire::{journal_v2 as j, recovery_v1::ReleaseLinks},
    };
    use std::{
        collections::BTreeMap,
        fs,
        os::unix::fs::{MetadataExt, PermissionsExt},
        path::Path,
    };
    fn write(root: &Path, name: &str, raw: &[u8]) {
        let path = root.join(name);
        fs::create_dir_all(path.parent().unwrap()).unwrap();
        for parent in path
            .parent()
            .unwrap()
            .ancestors()
            .take_while(|p| p.starts_with(root))
        {
            fs::set_permissions(parent, fs::Permissions::from_mode(0o700)).unwrap();
        }
        fs::write(&path, raw).unwrap();
        fs::set_permissions(&path, fs::Permissions::from_mode(0o600)).unwrap();
    }
    fn snapshot(root: &Path) -> BTreeMap<String, (u64, u32, i64, i64, String)> {
        let mut paths = vec![root.to_owned()];
        let mut files = BTreeMap::new();
        while let Some(path) = paths.pop() {
            for entry in fs::read_dir(path).unwrap() {
                let path = entry.unwrap().path();
                if path.is_dir() {
                    paths.push(path);
                } else {
                    let metadata = path.metadata().unwrap();
                    files.insert(
                        path.strip_prefix(root)
                            .unwrap()
                            .to_str()
                            .unwrap()
                            .to_owned(),
                        (
                            metadata.ino(),
                            metadata.mode(),
                            metadata.mtime(),
                            metadata.mtime_nsec(),
                            digest(&fs::read(&path).unwrap()),
                        ),
                    );
                }
            }
        }
        files
    }
    #[test]
    fn migration_cleanup_retains_endpoint_and_projection_history() {
        for case in [
            "retained",
            "unknown",
            "foreign",
            "missing",
            "without-projection",
            "projection-advanced",
        ] {
            let (fixture, store, mut journal, _) = crate::custody::tests::fixture();
            let catalog = migration_owner::current_catalog().unwrap();
            let registry = record::Registry::new(
                serde_json::from_slice(include_bytes!("../../config/release-transition.json"))
                    .unwrap(),
                &catalog,
            )
            .unwrap();
            let registry_digest = digest(&codec::encode(registry.wire(), false).unwrap());
            let history = registry.history();
            let mut authority = history.baseline();
            let mut edges = vec![];
            let mut before = digest(b"protected-record-absent-v1");
            let mut projection_bytes = vec![];
            for index in 0..3 {
                let edge = history.next(&authority).unwrap();
                let after = record::Ledger::from_authority(edge.after(), &registry).unwrap();
                let raw = codec::encode(after.wire(), true).unwrap();
                edges.push((edge.migration().clone(), before.clone(), digest(&raw)));
                before = digest(&raw);
                authority = edge.into_after();
                if index == 1 {
                    projection_bytes = codec::encode(
                        record::Ledger::from_authority(&authority, &registry)
                            .unwrap()
                            .wire(),
                        true,
                    )
                    .unwrap();
                }
            }
            let all = registry
                .wire()
                .migrations
                .iter()
                .flatten()
                .map(|m| m.id.clone())
                .collect::<Vec<_>>();
            let trace = history.trace_from(&history.baseline(), &all).unwrap();
            let terminal = record::Ledger::from_authority(trace.terminal(), &registry).unwrap();
            let projection = record::Projection::from_reader(
                projection_bytes.clone(),
                &crate::legacy::Reader::CanonicalFixture,
                false,
            )
            .unwrap();
            let checkpoint = record::Checkpoint::build(
                &terminal,
                &registry,
                &registry_digest,
                &projection,
                None,
                false,
            )
            .unwrap();
            write(
                &fixture.0,
                "release-transition/v2/ledger.json",
                &projection_bytes,
            );
            write(
                &fixture.0,
                "release-transition/v2/history-checkpoint.json",
                &codec::encode(checkpoint.wire(), true).unwrap(),
            );
            journal.checkpoint = "complete".into();
            write(
                &fixture.0,
                "release-transition/v2/journal.json",
                &codec::encode(&journal, true).unwrap(),
            );
            let endpoints = ReleaseLinks {
                active: "new".into(),
                previous: Some("previous".into()),
            };
            let mut protected = BTreeMap::new();
            for (tx, target, count) in [
                ("tx-previous", "previous", 2),
                (
                    "tx-anchor",
                    "ancient",
                    if case == "projection-advanced" { 3 } else { 2 },
                ),
                ("tx-active", "new", 1),
                ("tx-unused", "unused", 1),
            ] {
                for (migration, before, after) in edges.iter().take(count) {
                    for phase in ["captured", "applied", "verified"] {
                        let evidence = j::Evidence {
                            schema_version: 2,
                            transaction: tx.into(),
                            releases: j::ReleasePair {
                                from: "source".into(),
                                target: target.into(),
                                previous: None,
                            },
                            step: format!("{}.ledger", migration.id),
                            expected: before.clone(),
                            desired: after.clone(),
                            observed: if phase == "captured" {
                                before.clone()
                            } else {
                                after.clone()
                            },
                            checkpoint: phase.into(),
                            ..Default::default()
                        };
                        let name = format!("release-transition/v2/transactions/{tx}/evidence/{}.ledger.{phase}.json", migration.id);
                        let raw = codec::encode(&evidence, true).unwrap();
                        write(&fixture.0, &name, &raw);
                        if tx != "tx-unused" {
                            protected.insert(name, raw);
                        }
                    }
                }
            }
            let transactions = fixture.0.join("release-transition/v2/transactions");
            let first = &edges[0].0.id;
            let captured = format!("release-transition/v2/transactions/tx-unused/evidence/{first}.ledger.captured.json");
            if case == "unknown" {
                let verified = fixture.0.join(format!("release-transition/v2/transactions/tx-unused/evidence/{first}.ledger.verified.json"));
                write(
                    &fixture.0,
                    "release-transition/v2/transactions/tx-unused/evidence/unknown.verified.json",
                    &fs::read(verified).unwrap(),
                );
            } else if case == "foreign" {
                let mut value: j::Evidence =
                    serde_json::from_slice(&fs::read(fixture.0.join(&captured)).unwrap()).unwrap();
                value.transaction = "foreign".into();
                write(&fixture.0, &captured, &codec::encode(&value, true).unwrap());
            } else if case == "missing" {
                fs::remove_file(fixture.0.join(&captured)).unwrap();
            } else if case == "without-projection" {
                fs::remove_file(
                    fixture
                        .0
                        .join("release-transition/v2/history-checkpoint.json"),
                )
                .unwrap();
                write(
                    &fixture.0,
                    "release-transition/v2/ledger.json",
                    &codec::encode(terminal.wire(), true).unwrap(),
                );
            }
            let before = snapshot(&transactions);
            let lease = store.lock().unwrap();
            let result = lease.cleanup_migration_transactions(
                &journal.transaction,
                &registry,
                &registry_digest,
                &endpoints,
            );
            if ["unknown", "foreign", "missing"].contains(&case) {
                assert!(result.is_err(), "{case} evidence accepted");
                assert_eq!(
                    snapshot(&transactions),
                    before,
                    "{case} refusal changed artifacts"
                );
            } else {
                result.unwrap();
                let after = snapshot(&transactions);
                for (name, raw) in protected {
                    if name.contains("/tx-anchor/") && case != "retained" {
                        continue;
                    }
                    let key = name
                        .strip_prefix("release-transition/v2/transactions/")
                        .unwrap();
                    assert_eq!(
                        after.get(key),
                        before.get(key),
                        "{case} changed retained metadata"
                    );
                    assert_eq!(fs::read(fixture.0.join(name)).unwrap(), raw);
                }
                assert!(!transactions.join("tx-unused").exists());
                assert_eq!(transactions.join("tx-anchor").exists(), case == "retained");
            }
            eprintln!("migration retention case {case} passed");
        }
    }
}
