//! Bounded rooted physical snapshots. Pending copies count before payloads are retained.
use crate::{
    codec, digest,
    record::safe_id,
    store::{self, Address, Snapshot, Store},
    Error, Result,
};
use rustix::fs;
use std::collections::BTreeMap;
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord)]
pub enum Namespace {
    LegacyRecovery,
    LiveRecovery,
    Archive,
    Frontiers,
    MigrationOwner,
    MigrationCompleted,
}
impl Namespace {
    pub(crate) fn parts(self) -> Vec<String> {
        let parts: &[&str] = match self {
            Self::LegacyRecovery => &["release-transition", "recovery", "v1", "transactions"],
            Self::LiveRecovery => &["release-transition", "recovery", "v2", "transactions"],
            Self::Archive => &["release-transition", "recovery", "v2", "archive"],
            Self::Frontiers => &["release-transition", "recovery", "v2", "frontiers"],
            Self::MigrationOwner => &[
                "release-transition",
                "migration-owner",
                "v1",
                "transactions",
            ],
            Self::MigrationCompleted => {
                &["release-transition", "migration-owner", "v1", "completed"]
            }
        };
        parts.iter().map(|s| (*s).into()).collect()
    }
    pub(crate) fn limits(self) -> (usize, u64) {
        if self == Self::LegacyRecovery {
            (256, 512 << 20)
        } else {
            (512, 32 << 20)
        }
    }
    pub fn address(self, transaction: &str) -> Result<Address> {
        if !safe_id(transaction) {
            return Err(Error::Invalid);
        }
        Ok(Address::at(
            self.parts(),
            format!("{transaction}.json"),
            self != Self::Frontiers,
        ))
    }
}
#[derive(Clone)]
pub struct Pair {
    pub(crate) published: Snapshot,
    pub(crate) pending: Snapshot,
}
impl Pair {
    pub fn published(&self) -> &Snapshot {
        &self.published
    }
    pub fn pending(&self) -> &Snapshot {
        &self.pending
    }
    pub fn exact(&self) -> Result<Option<&[u8]>> {
        match (self.published.bytes(), self.pending.bytes()) {
            (Some(a), Some(b)) if a != b => Err(Error::Invalid),
            (Some(a), _) | (_, Some(a)) => Ok(Some(a)),
            _ => Ok(None),
        }
    }
}
pub struct Inventory {
    namespace: Namespace,
    records: BTreeMap<String, Pair>,
    bytes: u64,
}
impl Inventory {
    pub fn records(&self) -> &BTreeMap<String, Pair> {
        &self.records
    }
    pub fn bytes(&self) -> u64 {
        self.bytes
    }
    pub fn namespace(&self) -> Namespace {
        self.namespace
    }
    pub fn fingerprint(&self) -> Result<String> {
        let entries: BTreeMap<_, _> = self
            .records
            .iter()
            .map(|(id, p)| (id, (p.published.fingerprint(), p.pending.fingerprint())))
            .collect();
        Ok(digest(&codec::encode(&entries, false)?))
    }
}
pub(crate) fn names(parent: &impl std::os::fd::AsFd, maximum: usize) -> Result<Vec<String>> {
    byte_names(parent, maximum)?
        .into_iter()
        .map(|name| String::from_utf8(name).map_err(|_| Error::Protected))
        .collect()
}
pub(crate) fn byte_names(parent: &impl std::os::fd::AsFd, maximum: usize) -> Result<Vec<Vec<u8>>> {
    let mut result = Vec::new();
    for entry in fs::Dir::read_from(parent).map_err(|_| Error::Io)? {
        let entry = entry.map_err(|_| Error::Io)?;
        let name = entry.file_name().to_bytes();
        if matches!(name, b"." | b"..") {
            continue;
        }
        if result.len() >= maximum {
            return Err(Error::Bound);
        }
        result.push(name.to_vec());
    }
    result.sort();
    Ok(result)
}
impl Store {
    pub fn inventory(&self, namespace: Namespace) -> Result<Inventory> {
        let (count, limit) = namespace.limits();
        let mut records = BTreeMap::new();
        let mut bytes = 0u64;
        let Some(root) = &self.root else {
            return Ok(Inventory {
                namespace,
                records,
                bytes,
            });
        };
        store::checked(root, true, false, false)?;
        let Some(parent) = store::directory(root, &namespace.parts(), false)? else {
            return Ok(Inventory {
                namespace,
                records,
                bytes,
            });
        };
        let entries = names(&parent, count * 2)?;
        let mut logical = BTreeMap::<String, ()>::new();
        for name in &entries {
            let id = if name.starts_with('.') && name.ends_with(".json.pending") {
                name.strip_prefix('.')
                    .and_then(|rest| rest.strip_suffix(".json.pending"))
            } else {
                name.strip_suffix(".json")
            }
            .ok_or(Error::Invalid)?;
            if !safe_id(id) {
                return Err(Error::Invalid);
            }
            logical.insert(id.into(), ());
            if logical.len() > count {
                return Err(Error::Bound);
            }
            let s = store::io(fs::statat(&parent, name, fs::AtFlags::SYMLINK_NOFOLLOW))?;
            if fs::FileType::from_raw_mode(s.st_mode) != fs::FileType::RegularFile
                || s.st_size <= 0
                || s.st_size > codec::MAX_BYTES as i64
                || s.st_mode & 0o777 != 0o600
                || s.st_uid != rustix::process::getuid().as_raw()
                || s.st_nlink != 1
            {
                return Err(Error::Protected);
            }
            bytes = bytes.checked_add(s.st_size as u64).ok_or(Error::Bound)?;
            if bytes > limit {
                return Err(Error::Bound);
            }
        }
        let mut retained = 0u64;
        for id in logical.keys() {
            let published = store::read_at(&parent, &format!("{id}.json"), false)?;
            let pending = store::read_at(&parent, &format!(".{id}.json.pending"), false)?;
            if published.bytes().is_none() && pending.bytes().is_none() {
                return Err(Error::Stale);
            }
            retained = retained
                .checked_add(published.bytes().map_or(0, |b| b.len()) as u64)
                .and_then(|n| n.checked_add(pending.bytes().map_or(0, |b| b.len()) as u64))
                .ok_or(Error::Bound)?;
            if retained > limit {
                return Err(Error::Bound);
            }
            records.insert(id.clone(), Pair { published, pending });
        }
        bytes = retained;
        Ok(Inventory {
            namespace,
            records,
            bytes,
        })
    }
    pub(crate) fn transaction_pairs(&self) -> Result<BTreeMap<String, Pair>> {
        let mut result = BTreeMap::new();
        let Some(root) = &self.root else {
            return Ok(result);
        };
        store::checked(root, true, false, false)?;
        let parts = vec![
            "release-transition".into(),
            "v2".into(),
            "transactions".into(),
        ];
        let Some(parent) = store::directory(root, &parts, false)? else {
            return Ok(result);
        };
        for id in names(&parent, 256)? {
            if !safe_id(&id) {
                return Err(Error::Invalid);
            }
            let child =
                store::directory(&parent, std::slice::from_ref(&id), false)?.ok_or(Error::Io)?;
            result.insert(
                id,
                Pair {
                    published: store::read_at(&child, "superseded-journal.json", false)?,
                    pending: store::read_at(&child, ".superseded-journal.json.pending", false)?,
                },
            );
        }
        Ok(result)
    }
}
