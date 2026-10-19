//! One bounded, restartable native transaction retirement. The proof retains the
//! original semantic facts; a digest or a directory name is never quiescence.
use crate::{
    codec, custody, digest,
    graph::State,
    inventory,
    lease::UpdateLease,
    native,
    record::{self, require},
    store::{self, Address, Store},
    Error, Result,
};
use rustix::fs::{self, AtFlags};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::{collections::BTreeMap, os::fd::OwnedFd};
const CHUNK: usize = 512 << 10;
const STAGE_MAX: usize = 128 << 20;
const LIVE_MAX: usize = 128 << 20;
const MAX_FILES: usize = 8192;
fn parts() -> Vec<String> {
    ["migrations", "native-custody", "retirement-v1"]
        .map(str::to_string)
        .to_vec()
}
fn live_parts(tx: &str) -> Vec<String> {
    ["migrations", "native-custody", "v1", tx]
        .map(str::to_string)
        .to_vec()
}
fn address(tx: &str, n: Option<usize>) -> Address {
    let mut p = parts();
    p.push(tx.into());
    Address::at(
        p,
        n.map_or_else(|| "index.json".into(), |n| format!("piece-{n:03}.bin")),
        true,
    )
}
#[derive(Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct Index {
    version: u8,
    transaction: String,
    directory: native::Identity,
    graph: String,
    length: usize,
    sha256: String,
    pieces: Vec<String>,
}
#[derive(Clone)]
struct Fact {
    size: usize,
    sha256: String,
}
fn logical(name: &str) -> &str {
    name.strip_prefix('.')
        .and_then(|s| s.strip_suffix(".pending"))
        .unwrap_or(name)
}
fn bundle(name: &str) -> bool {
    let n = logical(name);
    n == "bundle.json"
        || n.strip_prefix("bundle.")
            .and_then(|s| s.strip_suffix(".json"))
            .is_some_and(|s| s.len() == 3 && s.bytes().all(|c| c.is_ascii_digit()))
}
fn retained_proof(name: &str) -> bool {
    !bundle(name)
}
fn stage(store: &Store) -> Result<Option<(String, OwnedFd)>> {
    let Some(root) = &store.root else {
        return Ok(None);
    };
    let Some(parent) = store::directory(root, &parts(), false)? else {
        return Ok(None);
    };
    let names = inventory::names(&parent, 1)?;
    let Some(tx) = names.first() else {
        return Ok(None);
    };
    require(record::safe_id(tx))?;
    let dir = store::directory(&parent, std::slice::from_ref(tx), false)?.ok_or(Error::Stale)?;
    let mut bytes = 0usize;
    for name in inventory::names(&dir, 258)? {
        let l = logical(&name);
        require(
            l == "index.json"
                || l.strip_prefix("piece-")
                    .and_then(|s| s.strip_suffix(".bin"))
                    .is_some_and(|s| {
                        s.len() == 3
                            && s.bytes().all(|c| c.is_ascii_digit())
                            && s.parse::<usize>().is_ok_and(|n| n < 256)
                    }),
        )?;
        let snapshot = store::read_at(&dir, &name, true)?;
        bytes = bytes
            .checked_add(snapshot.bytes().ok_or(Error::Stale)?.len())
            .ok_or(Error::Bound)?;
        require(bytes <= STAGE_MAX)?;
    }
    Ok(Some((tx.clone(), dir)))
}
fn live(store: &Store, tx: &str) -> Result<Option<OwnedFd>> {
    match &store.root {
        Some(root) => store::directory(root, &live_parts(tx), false),
        None => Ok(None),
    }
}
fn metadata(dir: &OwnedFd) -> Result<BTreeMap<String, Fact>> {
    let mut out = BTreeMap::new();
    let mut bytes = 0usize;
    for name in inventory::names(dir, MAX_FILES)? {
        let snapshot = store::read_at(dir, &name, true)?;
        let raw = snapshot.bytes().ok_or(Error::Stale)?;
        bytes = bytes.checked_add(raw.len()).ok_or(Error::Bound)?;
        require(bytes <= LIVE_MAX)?;
        out.insert(
            name,
            Fact {
                size: raw.len(),
                sha256: digest(raw),
            },
        );
    }
    Ok(out)
}
fn check_residual(dir: &OwnedFd, expected: &BTreeMap<String, Fact>) -> Result<()> {
    for name in inventory::names(dir, MAX_FILES)? {
        let fact = expected.get(&name).ok_or(Error::Protected)?;
        let snapshot = store::read_at(dir, &name, true)?;
        let raw = snapshot.bytes().ok_or(Error::Stale)?;
        require(raw.len() == fact.size && digest(raw) == fact.sha256)?;
    }
    Ok(())
}
struct Writer<'a, 'b> {
    lease: &'a UpdateLease<'b>,
    tx: &'a str,
    buffer: Vec<u8>,
    pieces: Vec<String>,
    length: usize,
    hash: Sha256,
}
impl Writer<'_, '_> {
    fn write(&mut self, mut raw: &[u8]) -> Result<()> {
        self.length = self.length.checked_add(raw.len()).ok_or(Error::Bound)?;
        // Leave room for the index and one simultaneously staged immutable piece.
        require(self.length <= STAGE_MAX - 2 * CHUNK)?;
        self.hash.update(raw);
        while !raw.is_empty() {
            let n = raw.len().min(CHUNK - self.buffer.len());
            self.buffer.extend_from_slice(&raw[..n]);
            raw = &raw[n..];
            if self.buffer.len() == CHUNK {
                self.flush()?;
            }
        }
        Ok(())
    }
    fn flush(&mut self) -> Result<()> {
        if self.buffer.is_empty() {
            return Ok(());
        }
        require(self.pieces.len() < 256)?;
        self.lease
            .create_immutable(&address(self.tx, Some(self.pieces.len())), &self.buffer)?;
        self.pieces.push(digest(&self.buffer));
        self.buffer.clear();
        self.lease.store.fault("after-native-retirement-piece");
        Ok(())
    }
    fn number(&mut self, n: usize) -> Result<()> {
        self.write(&u32::try_from(n).map_err(|_| Error::Bound)?.to_be_bytes())
    }
    fn name(&mut self, name: &str) -> Result<()> {
        require(name.len() <= 256)?;
        self.write(
            &u16::try_from(name.len())
                .map_err(|_| Error::Bound)?
                .to_be_bytes(),
        )?;
        self.write(name.as_bytes())
    }
}
struct Reader<'a> {
    store: &'a Store,
    tx: &'a str,
    index: &'a Index,
    piece: usize,
    buffer: Vec<u8>,
    offset: usize,
    read: usize,
    hash: Sha256,
}
impl Reader<'_> {
    fn take(&mut self, n: usize) -> Result<Vec<u8>> {
        require(
            n <= 1 << 20
                && self
                    .read
                    .checked_add(n)
                    .is_some_and(|s| s <= self.index.length),
        )?;
        let mut out = Vec::with_capacity(n);
        while out.len() < n {
            if self.offset == self.buffer.len() {
                let hash = self.index.pieces.get(self.piece).ok_or(Error::Protected)?;
                let snapshot = self.store.read(&address(self.tx, Some(self.piece)))?;
                let raw = snapshot.bytes().ok_or(Error::Protected)?;
                require(
                    digest(raw) == *hash && raw.len() == CHUNK.min(self.index.length - self.read),
                )?;
                self.buffer = raw.to_vec();
                self.offset = 0;
                self.piece += 1;
            }
            let size = (n - out.len()).min(self.buffer.len() - self.offset);
            let raw = &self.buffer[self.offset..self.offset + size];
            self.hash.update(raw);
            out.extend_from_slice(raw);
            self.offset += size;
            self.read += size;
        }
        Ok(out)
    }
    fn number(&mut self) -> Result<usize> {
        Ok(u32::from_be_bytes(self.take(4)?.try_into().map_err(|_| Error::Protected)?) as usize)
    }
    fn name(&mut self) -> Result<String> {
        let len =
            u16::from_be_bytes(self.take(2)?.try_into().map_err(|_| Error::Protected)?) as usize;
        require(len > 0 && len <= 256)?;
        let name = String::from_utf8(self.take(len)?).map_err(|_| Error::Protected)?;
        require(!name.contains('/') && name != "." && name != "..")?;
        Ok(name)
    }
    fn finish(self) -> Result<()> {
        require(
            self.read == self.index.length
                && self.offset == self.buffer.len()
                && self.piece == self.index.pieces.len()
                && self
                    .hash
                    .finalize()
                    .iter()
                    .map(|b| format!("{b:02x}"))
                    .collect::<String>()
                    == self.index.sha256,
        )
    }
}
fn read_index(store: &Store, tx: &str) -> Result<Option<Index>> {
    let snapshot = store.read(&address(tx, None))?;
    let Some(raw) = snapshot.bytes() else {
        return Ok(None);
    };
    let v: Index = codec::decode_canonical(raw, true)?;
    require(
        v.version == 1
            && v.transaction == tx
            && record::fingerprint(&v.graph)
            && record::fingerprint(&v.sha256)
            && v.length > 0
            && v.length <= STAGE_MAX - 2 * CHUNK
            && v.pieces.len() == v.length.div_ceil(CHUNK)
            && v.pieces.len() <= 256
            && v.pieces.iter().all(|h| record::fingerprint(h)),
    )?;
    Ok(Some(v))
}
fn prove(store: &Store, tx: &str, index: &Index) -> Result<BTreeMap<String, Fact>> {
    let mut reader = Reader {
        store,
        tx,
        index,
        piece: 0,
        buffer: vec![],
        offset: 0,
        read: 0,
        hash: Sha256::new(),
    };
    require(reader.take(8)? == b"NRETIRE1")?;
    let count = reader.number()?;
    require(count <= MAX_FILES)?;
    let mut facts = BTreeMap::new();
    let mut prior = String::new();
    for _ in 0..count {
        let name = reader.name()?;
        require(name > prior)?;
        prior = name.clone();
        let size = reader.number()?;
        require(size <= 1 << 20)?;
        let hash = String::from_utf8(reader.take(64)?).map_err(|_| Error::Protected)?;
        require(record::fingerprint(&hash))?;
        facts.insert(name, Fact { size, sha256: hash });
    }
    let count = reader.number()?;
    require(count <= facts.len())?;
    let mut remaining = count;
    let mut prior = String::new();
    let iter = std::iter::from_fn(|| {
        if remaining == 0 {
            return None;
        }
        remaining -= 1;
        Some((|| {
            let name = reader.name()?;
            require(name > prior && retained_proof(&name))?;
            prior = name.clone();
            let fact = facts.get(&name).ok_or(Error::Protected)?;
            let size = reader.number()?;
            require(size == fact.size)?;
            let raw = reader.take(size)?;
            require(digest(&raw) == fact.sha256)?;
            Ok((name, raw))
        })())
    });
    require(!custody::validate_files(
        tx, iter, None, false, None, false, None,
    )?)?;
    require(facts.keys().filter(|n| retained_proof(n)).count() == count)?;
    reader.finish()?;
    Ok(facts)
}
impl Store {
    pub(crate) fn has_native_retirement(&self) -> Result<bool> {
        Ok(stage(self)?.is_some())
    }
    /// A complete stage can substitute only its fully reconstructed original
    /// quiescence proof, and only while the transaction remains unreferenced.
    pub(crate) fn native_retirement(&self) -> Result<Option<String>> {
        let Some((tx, dir)) = stage(self)? else {
            return Ok(None);
        };
        require(
            !State::read(self)?
                .native_retained_transactions()
                .contains(&tx),
        )?;
        let Some(index) = read_index(self, &tx)? else {
            if live(self, &tx)?.is_none() {
                require(inventory::names(&dir, 0)?.is_empty())?;
                return Ok(Some(tx));
            }
            return Ok(None);
        };
        if let Some(original) = live(self, &tx)? {
            require(native::Identity::capture(&original)? == index.directory)?;
            let facts = prove(self, &tx, &index)?;
            check_residual(&original, &facts)?;
        } else {
            // Original payload is gone. Only exact remaining proof pieces may be
            // discarded; missing pieces here do not grant any native authority.
            for name in inventory::names(&dir, 258)? {
                if name == "index.json" {
                    continue;
                }
                let n = name
                    .strip_prefix("piece-")
                    .and_then(|s| s.strip_suffix(".bin"))
                    .and_then(|s| s.parse::<usize>().ok())
                    .ok_or(Error::Protected)?;
                let expected = index.pieces.get(n).ok_or(Error::Protected)?;
                require(
                    digest(
                        store::read_at(&dir, &name, false)?
                            .bytes()
                            .ok_or(Error::Protected)?,
                    ) == *expected,
                )?;
            }
        }
        Ok(Some(tx))
    }
}
impl UpdateLease<'_> {
    pub(crate) fn cleanup_native_transactions(&self) -> Result<()> {
        self.store.reader.health()?;
        require(!self.store.native_cleanup_pending()?)?;
        if let Some((tx, _)) = stage(self.store)? {
            self.retire_native_transaction(&tx)?;
        }
        let Some(root) = &self.store.root else {
            return Ok(());
        };
        let Some(parent) = store::directory(
            root,
            &["migrations".into(), "native-custody".into(), "v1".into()],
            false,
        )?
        else {
            return self.cleanup_prepared_payloads();
        };
        let retained = State::read(self.store)?.native_retained_transactions();
        let mut budget = 32;
        for tx in inventory::names(&parent, 256)? {
            require(record::safe_id(&tx))?;
            if retained.contains(&tx) || budget == 0 {
                continue;
            }
            self.retire_native_transaction(&tx)?;
            budget -= 1;
        }
        self.cleanup_prepared_payloads()
    }
    fn retire_native_transaction(&self, tx: &str) -> Result<()> {
        require(record::safe_id(tx))?;
        let graph = State::read(self.store)?;
        require(!graph.native_retained_transactions().contains(tx))?;
        if let Some((active, _)) = stage(self.store)? {
            require(active == tx)?;
        }
        let mut index = read_index(self.store, tx)?;
        if index.is_none() && live(self.store, tx)?.is_none() {
            let (_, dir) = stage(self.store)?.ok_or(Error::Stale)?;
            require(inventory::names(&dir, 0)?.is_empty())?;
            let parent = store::directory(
                self.store.root.as_ref().ok_or(Error::Protected)?,
                &parts(),
                false,
            )?
            .ok_or(Error::Stale)?;
            store::linked_directory(&parent, tx, &dir)?;
            store::io(fs::unlinkat(&parent, tx, AtFlags::REMOVEDIR))?;
            store::io(fs::fsync(&parent))?;
            return Ok(());
        }
        if index.is_none() {
            let dir = live(self.store, tx)?.ok_or(Error::Protected)?;
            // This is the same complete validator as admission, not a supplied
            // boolean. No pending native intent or unknown effect is retired.
            self.store.native_retireable_transaction(tx)?;
            let identity = native::Identity::capture(&dir)?;
            let facts = metadata(&dir)?;
            let mut length = 8usize + 4 + 4;
            for (name, f) in &facts {
                length = length
                    .checked_add(2 + name.len() + 4 + 64)
                    .ok_or(Error::Bound)?;
                if retained_proof(name) {
                    length = length
                        .checked_add(2 + name.len() + 4 + f.size)
                        .ok_or(Error::Bound)?;
                }
            }
            require(length <= STAGE_MAX - 2 * CHUNK)?;
            let mut writer = Writer {
                lease: self,
                tx,
                buffer: Vec::with_capacity(CHUNK),
                pieces: vec![],
                length: 0,
                hash: Sha256::new(),
            };
            writer.write(b"NRETIRE1")?;
            writer.number(facts.len())?;
            for (name, f) in &facts {
                writer.name(name)?;
                writer.number(f.size)?;
                writer.write(f.sha256.as_bytes())?;
            }
            writer.number(facts.keys().filter(|n| retained_proof(n)).count())?;
            for (name, f) in facts.iter().filter(|(n, _)| retained_proof(n)) {
                let snapshot = store::read_at(&dir, name, true)?;
                let raw = snapshot.bytes().ok_or(Error::Stale)?;
                require(raw.len() == f.size && digest(raw) == f.sha256)?;
                writer.name(name)?;
                writer.number(raw.len())?;
                writer.write(raw)?;
            }
            writer.flush()?;
            require(writer.length == length)?;
            let value = Index {
                version: 1,
                transaction: tx.into(),
                directory: identity,
                graph: graph.binding().into(),
                length,
                sha256: writer
                    .hash
                    .finalize()
                    .iter()
                    .map(|b| format!("{b:02x}"))
                    .collect::<String>(),
                pieces: writer.pieces,
            };
            require(graph.binding() == State::read(self.store)?.binding())?;
            check_residual(&dir, &facts)?;
            require(metadata(&dir)?.keys().eq(facts.keys()))?;
            self.create_immutable(&address(tx, None), &codec::encode(&value, true)?)?;
            self.store.fault("after-native-retirement-index");
            index = Some(value);
        }
        let index = index.ok_or(Error::Protected)?;
        let checked = self.store.native_retirement()?;
        require(checked.as_deref() == Some(tx))?;
        if let Some(dir) = live(self.store, tx)? {
            let facts = prove(self.store, tx, &index)?;
            for name in inventory::names(&dir, MAX_FILES)? {
                require(
                    !State::read(self.store)?
                        .native_retained_transactions()
                        .contains(tx),
                )?;
                require(native::Identity::capture(&dir)? == index.directory)?;
                require(
                    native::Identity::capture(&live(self.store, tx)?.ok_or(Error::Stale)?)?
                        == index.directory,
                )?;
                let fact = facts.get(&name).ok_or(Error::Protected)?;
                let snapshot = store::read_at(&dir, &name, true)?;
                let raw = snapshot.bytes().ok_or(Error::Stale)?;
                require(raw.len() == fact.size && digest(raw) == fact.sha256)?;
                store::io(fs::unlinkat(&dir, &name, AtFlags::empty()))?;
                store::io(fs::fsync(&dir))?;
                self.store.fault("after-native-transaction-unlink");
            }
            require(inventory::names(&dir, 0)?.is_empty())?;
            let root = self.store.root.as_ref().ok_or(Error::Protected)?;
            let parent = store::directory(
                root,
                &["migrations".into(), "native-custody".into(), "v1".into()],
                false,
            )?
            .ok_or(Error::Stale)?;
            let linked = store::directory(&parent, &[tx.into()], false)?.ok_or(Error::Stale)?;
            require(native::Identity::capture(&linked)? == index.directory)?;
            store::io(fs::unlinkat(&parent, tx, AtFlags::REMOVEDIR))?;
            store::io(fs::fsync(&parent))?;
            self.store.fault("after-native-transaction-directory");
        }
        let (active, dir) = stage(self.store)?.ok_or(Error::Stale)?;
        require(active == tx)?;
        let parent = store::directory(
            self.store.root.as_ref().ok_or(Error::Protected)?,
            &parts(),
            false,
        )?
        .ok_or(Error::Stale)?;
        store::linked_directory(&parent, tx, &dir)?;
        for (n, hash) in index.pieces.iter().enumerate() {
            let name = format!("piece-{n:03}.bin");
            let snapshot = store::read_at(&dir, &name, false)?;
            if let Some(raw) = snapshot.bytes() {
                require(digest(raw) == *hash)?;
                store::linked_directory(&parent, tx, &dir)?;
                store::io(fs::unlinkat(&dir, &name, AtFlags::empty()))?;
                store::io(fs::fsync(&dir))?;
                self.store.fault("after-native-retirement-unlink");
            }
        }
        require(inventory::names(&dir, 1)? == vec!["index.json".to_string()])?;
        store::linked_directory(&parent, tx, &dir)?;
        store::io(fs::unlinkat(&dir, "index.json", AtFlags::empty()))?;
        store::io(fs::fsync(&dir))?;
        self.store.fault("after-native-retirement-index-unlink");
        let parent = store::directory(
            self.store.root.as_ref().ok_or(Error::Protected)?,
            &parts(),
            false,
        )?
        .ok_or(Error::Stale)?;
        store::linked_directory(&parent, tx, &dir)?;
        store::io(fs::unlinkat(&parent, tx, AtFlags::REMOVEDIR))?;
        store::io(fs::fsync(&parent))?;
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::custody::tests::{fixture, terminal, write, Fixture};
    use std::{
        fs as stdfs,
        io::{BufRead, BufReader},
        process::{Command, Stdio},
        time::Duration,
    };
    fn completed() -> (
        Fixture,
        Store,
        crate::wire::journal_v2::Record,
        native::Binding,
    ) {
        let (f, s, j, b) = fixture();
        let hash = "a".repeat(64);
        write(
            &f,
            &b,
            "001.intent.json",
            &custody::Intent {
                version: 1,
                binding: b.clone(),
                owner: "host-power".into(),
                resource: "default".into(),
                expected: hash.clone(),
                desired: hash.clone(),
                source: hash.clone(),
                recipe: hash,
            },
        );
        write(
            &f,
            &b,
            "001.accepted.json",
            &terminal(&b, "accepted", vec![]),
        );
        write(
            &f,
            &b,
            "001.terminal.json",
            &terminal(&b, "complete", vec![]),
        );
        s.lock().unwrap().close_native(&b).unwrap();
        (f, s, j, b)
    }
    fn child(f: &Fixture, cut: &str) {
        let mut child = Command::new(std::env::current_exe().unwrap())
            .args([
                "--exact",
                "native_transaction_retention::tests::cut_child",
                "--nocapture",
            ])
            .env("SUBYARD_NATIVE_TRANSACTION_ROOT", &f.0)
            .env("SUBYARD_NATIVE_TRANSACTION_CUT", cut)
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
    }
    #[test]
    fn transaction_retirement_is_restartable_and_reconstructs_original_proof() {
        for cut in [
            "after-native-retirement-piece",
            "after-native-retirement-index",
            "after-native-transaction-unlink",
            "after-native-transaction-directory",
            "after-native-retirement-unlink",
            "after-native-retirement-index-unlink",
        ] {
            let (f, _, _, b) = completed();
            child(&f, cut);
            let fresh = Store::open_canonical_fixture(&f.0).unwrap();
            fresh.native_inventory(None, false).unwrap();
            fresh.lock().unwrap().cleanup_native_transactions().unwrap();
            assert!(
                !f.0.join("migrations/native-custody/v1")
                    .join(b.transaction)
                    .exists(),
                "{cut}"
            );
            assert!(stage(&fresh).unwrap().is_none(), "{cut}");
        }
    }
    #[test]
    fn owner_create_retirement_reconstructs_causality_and_refuses_missing_result() {
        for fixture in [
            crate::custody::tests::created_owner,
            crate::custody::tests::created_owner_with_fingerprint,
            crate::custody::tests::created_owner_with_image_token,
        ] {
            let (f, s, _, b) = fixture();
            s.lock().unwrap().close_native(&b).unwrap();
            let result =
                f.0.join("migrations/native-custody/v1")
                    .join(&b.transaction)
                    .join("001.command-001-create-result.json");
            let raw = stdfs::read(&result).unwrap();
            stdfs::remove_file(&result).unwrap();
            assert_eq!(
                s.native_retireable_transaction(&b.transaction),
                Err(Error::Unknown)
            );
            assert!(s.lock().unwrap().cleanup_native_transactions().is_err());
            stdfs::write(&result, raw).unwrap();
            use std::os::unix::fs::PermissionsExt;
            stdfs::set_permissions(&result, stdfs::Permissions::from_mode(0o600)).unwrap();
            child(&f, "after-native-transaction-unlink");
            let fresh = Store::open_canonical_fixture(&f.0).unwrap();
            fresh.native_inventory(None, false).unwrap();
            fresh.lock().unwrap().cleanup_native_transactions().unwrap();
            assert!(!f
                .0
                .join("migrations/native-custody/v1")
                .join(b.transaction)
                .exists());
            assert!(stage(&fresh).unwrap().is_none());
        }
    }
    #[test]
    fn active_reference_unknown_and_foreign_residual_never_authorize_deletion() {
        let (f, s, j, b) = completed();
        let path =
            f.0.join("migrations/native-custody/v1")
                .join(&b.transaction);
        let original = metadata(&live(&s, &b.transaction).unwrap().unwrap()).unwrap();
        let checked = record::Journal::new(j.clone()).unwrap();
        s.lock()
            .unwrap()
            .compare_and_swap(
                &Address::journal(),
                &s.read(&Address::journal()).unwrap(),
                &codec::encode(checked.wire(), true).unwrap(),
            )
            .unwrap();
        s.lock().unwrap().cleanup_native_transactions().unwrap();
        assert!(path.exists());
        assert_eq!(
            metadata(&live(&s, &b.transaction).unwrap().unwrap())
                .unwrap()
                .len(),
            original.len()
        );
        stdfs::remove_file(f.0.join("release-transition/v2/journal.json")).unwrap();
        child(&f, "after-native-retirement-index");
        write(&f, &b, "foreign.json", &serde_json::json!({"not":"owned"}));
        let before = stdfs::read_dir(&path).unwrap().count();
        assert!(s.native_inventory(None, false).is_err());
        assert!(s.lock().unwrap().cleanup_native_transactions().is_err());
        assert_eq!(stdfs::read_dir(&path).unwrap().count(), before);
        stdfs::remove_file(path.join("foreign.json")).unwrap();
        s.lock().unwrap().cleanup_native_transactions().unwrap();
        let (f, s, _, b) = completed();
        stdfs::remove_file(
            f.0.join("migrations/native-custody/v1")
                .join(&b.transaction)
                .join("001.terminal.json"),
        )
        .unwrap();
        assert!(s.lock().unwrap().cleanup_native_transactions().is_err());
        assert!(stage(&s).unwrap().is_none());
    }
    #[test]
    fn bundle_only_orphan_cannot_authorize_and_can_be_retired() {
        let (f, s, _, b) = fixture();
        let dir =
            f.0.join("migrations/native-custody/v1")
                .join(&b.transaction);
        stdfs::remove_file(dir.join("001.reservation.json")).unwrap();
        write(
            &f,
            &b,
            "bundle.000.json",
            &serde_json::json!({"data":"aGVsbG8="}),
        );
        // Even an empty, interrupted regular staging leaf has no authority.
        let pending = dir.join(".bundle.json.pending");
        stdfs::write(&pending, []).unwrap();
        use std::os::unix::fs::PermissionsExt;
        stdfs::set_permissions(pending, stdfs::Permissions::from_mode(0o600)).unwrap();
        s.lock().unwrap().cleanup_native_transactions().unwrap();
        assert!(!dir.exists());
    }
    #[test]
    fn actual_go_archive_selection_pins_only_reachable_native_history() {
        let Ok(base) = std::env::var("SUBYARD_RUST_OWNER_CORPUS") else {
            return;
        };
        let corpus: serde_json::Value =
            serde_json::from_slice(&stdfs::read(format!("{base}.recovery.json")).unwrap()).unwrap();
        for name in ["prune-unreferenced", "prune-current"] {
            let case = corpus["policies"]
                .as_array()
                .unwrap()
                .iter()
                .find(|v| v["name"] == name)
                .unwrap();
            assert_eq!(case["accepted"], true);
            let (f, s, _, template) = fixture();
            stdfs::remove_dir_all(
                f.0.join("migrations/native-custody/v1")
                    .join(&template.transaction),
            )
            .unwrap();
            let mut ids = vec![];
            for (path, value) in case["before"].as_object().unwrap() {
                let full = f.0.join(path);
                stdfs::create_dir_all(full.parent().unwrap()).unwrap();
                use std::os::unix::fs::PermissionsExt;
                let mut parent = full.parent().unwrap();
                while parent != f.0 {
                    stdfs::set_permissions(parent, stdfs::Permissions::from_mode(0o700)).unwrap();
                    parent = parent.parent().unwrap();
                }
                stdfs::write(&full, value.as_str().unwrap()).unwrap();
                stdfs::set_permissions(&full, stdfs::Permissions::from_mode(0o600)).unwrap();
                if path.contains("/archive/") {
                    ids.push(
                        path.strip_suffix(".json")
                            .unwrap()
                            .rsplit('/')
                            .next()
                            .unwrap()
                            .to_string(),
                    );
                }
            }
            for id in &ids {
                let mut b = template.clone();
                b.transaction = id.clone();
                let hash = "a".repeat(64);
                write(
                    &f,
                    &b,
                    "001.reservation.json",
                    &crate::native_retention::Reservation::new(&b),
                );
                write(
                    &f,
                    &b,
                    "001.intent.json",
                    &custody::Intent {
                        version: 1,
                        binding: b.clone(),
                        owner: "host-power".into(),
                        resource: "default".into(),
                        expected: hash.clone(),
                        desired: hash.clone(),
                        source: hash.clone(),
                        recipe: hash,
                    },
                );
                write(
                    &f,
                    &b,
                    "001.accepted.json",
                    &terminal(&b, "accepted", vec![]),
                );
                write(
                    &f,
                    &b,
                    "001.terminal.json",
                    &terminal(&b, "complete", vec![]),
                );
                s.lock().unwrap().close_native(&b).unwrap();
            }
            let before = State::read(&s).unwrap().binding().to_string();
            s.lock().unwrap().cleanup_native_transactions().unwrap();
            for id in ids {
                let archived = format!("release-transition/recovery/v2/archive/{id}.json");
                let go_kept = case["after"].as_object().unwrap().contains_key(&archived);
                assert_eq!(
                    f.0.join("migrations/native-custody/v1").join(id).exists(),
                    go_kept,
                    "{name}"
                );
            }
            assert_eq!(State::read(&s).unwrap().binding(), before);
            for (path, value) in case["before"].as_object().unwrap() {
                assert_eq!(
                    stdfs::read(f.0.join(path)).unwrap(),
                    value.as_str().unwrap().as_bytes()
                );
            }
        }
    }
    #[test]
    fn cut_child() {
        let Ok(root) = std::env::var("SUBYARD_NATIVE_TRANSACTION_ROOT") else {
            return;
        };
        let mut s = Store::open_canonical_fixture(std::path::Path::new(&root)).unwrap();
        s.test_cut(
            match std::env::var("SUBYARD_NATIVE_TRANSACTION_CUT")
                .unwrap()
                .as_str()
            {
                "after-native-retirement-piece" => Some("after-native-retirement-piece"),
                "after-native-retirement-index" => Some("after-native-retirement-index"),
                "after-native-transaction-unlink" => Some("after-native-transaction-unlink"),
                "after-native-transaction-directory" => Some("after-native-transaction-directory"),
                "after-native-retirement-unlink" => Some("after-native-retirement-unlink"),
                "after-native-retirement-index-unlink" => {
                    Some("after-native-retirement-index-unlink")
                }
                _ => panic!("closed cut"),
            },
        );
        s.lock().unwrap().cleanup_native_transactions().unwrap();
    }
}
