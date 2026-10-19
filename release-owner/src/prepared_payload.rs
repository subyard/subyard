//! Immutable raw source bytes used only by the closed installed-component recipe.
//! Addresses are SHA-256 values, never executable paths; refs come from bundles.
use crate::{
    codec, digest, inventory,
    lease::UpdateLease,
    record::{self, require},
    store::{self, Address, Store},
    Error, Result,
};
use rustix::fs::{self, AtFlags};
use serde::{Deserialize, Serialize};
use std::{collections::BTreeSet, os::fd::OwnedFd};
pub(crate) const CHUNK: usize = 512 << 10;
const MAX: usize = 128 << 20;
pub(crate) const FILE_MAX: usize = 32 << 20;
#[derive(Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct Index {
    version: u8,
    sha256: String,
    length: usize,
    pieces: Vec<String>,
}
fn parts() -> Vec<String> {
    ["migrations", "native-prepared", "v1"]
        .map(str::to_string)
        .to_vec()
}
fn address(hash: &str, n: Option<usize>) -> Result<Address> {
    require(record::fingerprint(hash) && n.is_none_or(|n| n < 64))?;
    let mut p = parts();
    p.push(hash.into());
    Ok(Address::at(
        p,
        n.map_or_else(|| "index.json".into(), |n| format!("{n:03}.bin")),
        true,
    ))
}
fn logical(n: &str) -> &str {
    n.strip_prefix('.')
        .and_then(|s| s.strip_suffix(".pending"))
        .unwrap_or(n)
}
fn piece(n: &str) -> Result<Option<usize>> {
    let l = logical(n);
    if l == "index.json" {
        return Ok(None);
    }
    let n = l.strip_suffix(".bin").ok_or(Error::Protected)?;
    require(n.len() == 3 && n.bytes().all(|c| c.is_ascii_digit()))?;
    let v = n.parse::<usize>().map_err(|_| Error::Protected)?;
    require(v < 64)?;
    Ok(Some(v))
}
fn parent(store: &Store) -> Result<Option<OwnedFd>> {
    match &store.root {
        Some(root) => store::directory(root, &parts(), false),
        None => Ok(None),
    }
}
fn read_index(store: &Store, hash: &str) -> Result<Option<Index>> {
    let snapshot = store.read(&address(hash, None)?)?;
    let Some(raw) = snapshot.bytes() else {
        return Ok(None);
    };
    let v: Index = codec::decode_canonical(raw, true)?;
    require(
        v.version == 1
            && v.sha256 == hash
            && v.length <= FILE_MAX
            && v.pieces.len() == v.length.div_ceil(CHUNK)
            && v.pieces.iter().all(|h| record::fingerprint(h)),
    )?;
    Ok(Some(v))
}
pub(crate) struct Usage {
    pub raw: usize,
    pub metadata: usize,
    pub roots: usize,
    pub pieces: usize,
}
impl Store {
    pub(crate) fn has_prepared_payload(&self, hash: &str) -> Result<bool> {
        Ok(read_index(self, hash)?.is_some())
    }
    pub(crate) fn payload_usage(&self) -> Result<Usage> {
        let mut usage = Usage {
            raw: 0,
            metadata: 0,
            roots: 0,
            pieces: 0,
        };
        let Some(parent) = parent(self)? else {
            return Ok(usage);
        };
        let roots = inventory::names(&parent, 8192)?;
        usage.roots = roots.len();
        for hash in roots {
            require(record::fingerprint(&hash))?;
            let dir = store::directory(&parent, std::slice::from_ref(&hash), false)?
                .ok_or(Error::Stale)?;
            let index = read_index(self, &hash)?;
            for name in inventory::names(&dir, 130)? {
                let n = piece(&name)?;
                let snapshot = store::read_at(&dir, &name, true)?;
                let raw = snapshot.bytes().ok_or(Error::Stale)?;
                if let Some(n) = n {
                    require(raw.len() <= CHUNK)?;
                    usage.pieces = usage.pieces.checked_add(1).ok_or(Error::Bound)?;
                    usage.raw = usage.raw.checked_add(raw.len()).ok_or(Error::Bound)?;
                    if let Some(index) = &index {
                        require(
                            name == logical(&name) && index.pieces.get(n) == Some(&digest(raw)),
                        )?;
                    }
                } else {
                    usage.metadata = usage.metadata.checked_add(raw.len()).ok_or(Error::Bound)?;
                }
                require(usage.raw <= MAX && usage.metadata <= 8 << 20 && usage.pieces <= 16384)?;
            }
        }
        Ok(usage)
    }
    pub(crate) fn prepared_payload(&self, hash: &str) -> Result<Vec<u8>> {
        let index = read_index(self, hash)?.ok_or(Error::Protected)?;
        let mut out = Vec::with_capacity(index.length);
        for (n, hash) in index.pieces.iter().enumerate() {
            let snapshot = self.read(&address(&index.sha256, Some(n))?)?;
            let raw = snapshot.bytes().ok_or(Error::Protected)?;
            require(raw.len() == CHUNK.min(index.length - out.len()) && digest(raw) == *hash)?;
            out.extend_from_slice(raw);
        }
        require(out.len() == index.length && digest(&out) == index.sha256)?;
        Ok(out)
    }
}
impl UpdateLease<'_> {
    pub(crate) fn publish_prepared_payload(&self, raw: &[u8]) -> Result<String> {
        require(raw.len() <= FILE_MAX)?;
        let hash = digest(raw);
        if read_index(self.store, &hash)?.is_some() {
            require(self.store.prepared_payload(&hash)? == raw)?;
            return Ok(hash);
        }
        let usage = self.store.payload_usage()?;
        let dir = parent(self.store)?
            .map(|p| store::directory(&p, std::slice::from_ref(&hash), false))
            .transpose()?
            .flatten();
        let mut additional_raw = 0usize;
        let mut additional_pieces = 0usize;
        let mut pieces = vec![];
        for (n, part) in raw.chunks(CHUNK).enumerate() {
            let existing = self.store.read(&address(&hash, Some(n))?)?;
            if let Some(bytes) = existing.bytes() {
                require(bytes == part)?;
            } else {
                let pending = if let Some(dir) = &dir {
                    store::read_at(dir, &format!(".{n:03}.bin.pending"), true)?
                } else {
                    store::Snapshot::captured(None)
                };
                let prefix = pending.bytes().unwrap_or_default();
                require(part.starts_with(prefix))?;
                additional_raw = additional_raw
                    .checked_add(part.len() - prefix.len())
                    .ok_or(Error::Bound)?;
                additional_pieces += usize::from(pending.bytes().is_none());
            }
            pieces.push(digest(part));
        }
        let index = codec::encode(
            &Index {
                version: 1,
                sha256: hash.clone(),
                length: raw.len(),
                pieces,
            },
            true,
        )?;
        let pending = if let Some(dir) = &dir {
            store::read_at(dir, ".index.json.pending", true)?
        } else {
            store::Snapshot::captured(None)
        };
        require(index.starts_with(pending.bytes().unwrap_or_default()))?;
        let (files, bytes, _) = self.store.native_usage(None)?;
        require(
            usage
                .raw
                .checked_add(additional_raw)
                .is_some_and(|n| n <= MAX)
                && usage
                    .pieces
                    .checked_add(additional_pieces)
                    .is_some_and(|n| n <= 16384)
                && usage.roots + usize::from(dir.is_none()) <= 8192
                && usage.metadata + index.len() - pending.bytes().map_or(0, <[u8]>::len) <= 8 << 20
                && bytes
                    .checked_add(index.len() - pending.bytes().map_or(0, <[u8]>::len))
                    .is_some_and(|n| n <= 128 << 20)
                && files <= 8192,
        )?;
        for (n, part) in raw.chunks(CHUNK).enumerate() {
            self.create_immutable(&address(&hash, Some(n))?, part)?;
            self.store.fault("after-prepared-payload-piece");
        }
        self.create_immutable(&address(&hash, None)?, &index)?;
        self.store.fault("after-prepared-payload-index");
        Ok(hash)
    }
    pub(crate) fn cleanup_prepared_payloads(&self) -> Result<()> {
        let Some(parent) = parent(self.store)? else {
            return Ok(());
        };
        let Some(refs) = self.store.bundle_payload_references()? else {
            return Ok(());
        };
        let mut budget = 32;
        for hash in inventory::names(&parent, 8192)? {
            require(record::fingerprint(&hash))?;
            if refs.contains(&hash) || budget == 0 {
                continue;
            }
            let dir = store::directory(&parent, std::slice::from_ref(&hash), false)?
                .ok_or(Error::Stale)?;
            let index = read_index(self.store, &hash)?;
            let names = inventory::names(&dir, 130)?;
            let mut snapshots = vec![];
            for name in names {
                let n = piece(&name)?;
                let snapshot = store::read_at(&dir, &name, true)?;
                if let (Some(index), Some(n)) = (&index, n) {
                    require(
                        name == logical(&name)
                            && index.pieces.get(n)
                                == Some(&digest(snapshot.bytes().ok_or(Error::Protected)?)),
                    )?;
                }
                snapshots.push((name, snapshot));
            }
            // Index last permits bounded retry after an interrupted raw unlink.
            snapshots.sort_by_key(|(name, _)| logical(name) == "index.json");
            for (name, snapshot) in snapshots {
                let Some(current_refs) = self.store.bundle_payload_references()? else {
                    return Err(Error::Stale);
                };
                store::linked_directory(&parent, &hash, &dir)?;
                require(
                    !current_refs.contains(&hash) && store::read_at(&dir, &name, true)? == snapshot,
                )?;
                store::io(fs::unlinkat(&dir, &name, AtFlags::empty()))?;
                store::io(fs::fsync(&dir))?;
                self.store.fault("after-prepared-payload-unlink");
            }
            require(inventory::names(&dir, 0)?.is_empty())?;
            store::linked_directory(&parent, &hash, &dir)?;
            store::io(fs::unlinkat(&parent, &hash, AtFlags::REMOVEDIR))?;
            store::io(fs::fsync(&parent))?;
            budget -= 1;
        }
        self.store.payload_usage()?;
        Ok(())
    }
}
pub(crate) fn references<'a>(
    values: impl Iterator<Item = &'a super::installed_recipe::PackedRecipe>,
) -> BTreeSet<String> {
    values
        .flat_map(|v| {
            v.files
                .iter()
                .flatten()
                .filter_map(|f| f.content.clone())
                .chain((!v.after_owner.is_empty()).then(|| v.after_owner.clone()))
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::custody::tests::fixture;
    use std::{
        fs as stdfs,
        io::{BufRead, BufReader},
        process::{Command, Stdio},
        time::Duration,
    };
    fn data() -> Vec<u8> {
        (0..CHUNK + 17).map(|n| (n % 251) as u8).collect()
    }
    #[test]
    fn immutable_binary_payload_deduplicates_and_detects_corruption() {
        let (f, s, _, _) = fixture();
        let bytes = data();
        let hash = s.lock().unwrap().publish_prepared_payload(&bytes).unwrap();
        assert_eq!(
            s.lock().unwrap().publish_prepared_payload(&bytes).unwrap(),
            hash
        );
        assert_eq!(s.payload_usage().unwrap().raw, bytes.len());
        assert_eq!(s.prepared_payload(&hash).unwrap(), bytes);
        let path =
            f.0.join("migrations/native-prepared/v1")
                .join(&hash)
                .join("000.bin");
        stdfs::write(&path, b"changed").unwrap();
        assert!(s.prepared_payload(&hash).is_err());
        assert!(s.lock().unwrap().cleanup_prepared_payloads().is_err());
        assert!(path.exists());
    }
    #[test]
    fn raw_piece_publication_and_orphan_cleanup_resume_after_process_loss() {
        for cut in [
            "after-prepared-payload-piece",
            "after-prepared-payload-index",
            "after-prepared-payload-unlink",
        ] {
            let (f, _, _, _) = fixture();
            let mut child = Command::new(std::env::current_exe().unwrap())
                .args([
                    "--exact",
                    "prepared_payload::tests::cut_child",
                    "--nocapture",
                ])
                .env("SUBYARD_PAYLOAD_ROOT", &f.0)
                .env("SUBYARD_PAYLOAD_CUT", cut)
                .stdout(Stdio::piped())
                .spawn()
                .unwrap();
            let stdout = child.stdout.take().unwrap();
            let (tx, rx) = std::sync::mpsc::channel();
            let reader = std::thread::spawn(move || {
                for line in BufReader::new(stdout).lines() {
                    if line.unwrap().starts_with("publication-cut:") {
                        let _ = tx.send(());
                        break;
                    }
                }
            });
            let ready = rx.recv_timeout(Duration::from_secs(10));
            child.kill().unwrap();
            child.wait().unwrap();
            reader.join().unwrap();
            ready.unwrap();
            let s = Store::open_canonical_fixture(&f.0).unwrap();
            s.payload_usage().unwrap();
            if cut != "after-prepared-payload-unlink" {
                let hash = s.lock().unwrap().publish_prepared_payload(&data()).unwrap();
                assert_eq!(s.prepared_payload(&hash).unwrap(), data());
            }
            s.lock().unwrap().cleanup_prepared_payloads().unwrap();
            assert_eq!(s.payload_usage().unwrap().roots, 0);
        }
    }
    #[test]
    fn cut_child() {
        let Ok(root) = std::env::var("SUBYARD_PAYLOAD_ROOT") else {
            return;
        };
        let mut s = Store::open_canonical_fixture(std::path::Path::new(&root)).unwrap();
        let cut = std::env::var("SUBYARD_PAYLOAD_CUT").unwrap();
        s.test_cut(match cut.as_str() {
            "after-prepared-payload-piece" => Some("after-prepared-payload-piece"),
            "after-prepared-payload-index" => Some("after-prepared-payload-index"),
            "after-prepared-payload-unlink" => None,
            _ => panic!("closed cut"),
        });
        s.lock().unwrap().publish_prepared_payload(&data()).unwrap();
        if cut == "after-prepared-payload-unlink" {
            s.test_cut(Some("after-prepared-payload-unlink"));
            s.lock().unwrap().cleanup_prepared_payloads().unwrap();
        }
    }
}
