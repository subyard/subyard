//! Rooted runtime-link publication. The owner supplies the checked pair and update lease.
//! Forward staging and exchange are distinct durable checkpoints, not a pair-wide CAS.
use crate::{
    lease::UpdateLease,
    record, store,
    wire::{journal_v2::ReleasePair, recovery_v1::ReleaseLinks},
    Error, Result,
};
use rustix::fs::{self, AtFlags, FileType, Mode, OFlags, RenameFlags};
use std::{
    os::fd::OwnedFd,
    path::{Path, PathBuf},
};

pub struct RuntimeLinks {
    root: OwnedFd,
    path: PathBuf,
}
impl RuntimeLinks {
    pub fn open(path: &Path) -> Result<Self> {
        record::require(path.is_absolute() && path != Path::new("/"))?;
        let root = store::io(fs::open(
            path,
            OFlags::RDONLY | OFlags::DIRECTORY | store::flags(),
            Mode::empty(),
        ))?;
        store::checked(&root, true, false, false)?;
        Ok(Self {
            root,
            path: path.to_path_buf(),
        })
    }
    fn current_root(&self) -> Result<()> {
        let pinned = store::io(fs::fstat(&self.root))?;
        let current =
            fs::statat(fs::CWD, &self.path, AtFlags::SYMLINK_NOFOLLOW).map_err(|_| Error::Stale)?;
        if current.st_dev != pinned.st_dev || current.st_ino != pinned.st_ino {
            return Err(Error::Stale);
        }
        record::require(
            FileType::from_raw_mode(current.st_mode) == FileType::Directory
                && current.st_uid == rustix::process::getuid().as_raw()
                && current.st_mode & 0o022 == 0,
        )
    }
    fn release(&self, id: &str) -> Result<()> {
        self.current_root()?;
        record::require(record::safe_id(id))?;
        let releases = store::io(fs::openat(
            &self.root,
            "releases",
            OFlags::RDONLY | OFlags::DIRECTORY | store::flags(),
            Mode::empty(),
        ))?;
        store::checked(&releases, true, false, false)?;
        let release = store::io(fs::openat(
            &releases,
            id,
            OFlags::RDONLY | OFlags::DIRECTORY | store::flags(),
            Mode::empty(),
        ))?;
        store::checked(&release, true, false, false)
    }
    pub(crate) fn artifact_root(&self, id: &str) -> Result<OwnedFd> {
        self.current_root()?;
        record::require(record::safe_id(id))?;
        let parent = store::io(fs::openat(
            &self.root,
            "releases",
            OFlags::RDONLY | OFlags::DIRECTORY | store::flags(),
            Mode::empty(),
        ))?;
        store::checked(&parent, true, false, false)?;
        store::io(fs::openat(
            &parent,
            id,
            OFlags::RDONLY | OFlags::DIRECTORY | store::flags(),
            Mode::empty(),
        ))
    }
    pub(crate) fn release_identity(
        &self,
        id: &str,
        identity: &crate::native::Identity,
    ) -> Result<()> {
        let fd = self.artifact_root(id)?;
        record::require(crate::native::Identity::capture(&fd)? == *identity)
    }
    pub(crate) fn source_artifact(&self, proof: &crate::owner::SourceArtifact) -> Result<()> {
        self.source_artifact_in(proof, &mut crate::assets::VerificationPhase::new())
    }
    pub(crate) fn source_artifact_in(
        &self,
        proof: &crate::owner::SourceArtifact,
        phase: &mut crate::assets::VerificationPhase,
    ) -> Result<()> {
        record::require(
            record::semver(&proof.version)
                && proof.release.starts_with(&format!("{}-", proof.version)),
        )?;
        let root = self.artifact_root(&proof.release)?;
        record::require(crate::native::Identity::capture(&root)? == proof.root)?;
        let entries = phase.verify(&proof.release, &root, &proof.artifact_digest)?;
        record::require(entries.get("bin/yard-engine") == Some(&proof.engine_digest))
    }
    #[cfg(test)]
    pub(crate) fn source_checkpoint(
        &self,
        proof: &crate::owner::SourceCheckpoint,
        reader: &crate::legacy::Reader,
    ) -> Result<()> {
        self.source_checkpoint_in(proof, reader, &mut crate::assets::VerificationPhase::new())
    }
    pub(crate) fn source_checkpoint_in(
        &self,
        proof: &crate::owner::SourceCheckpoint,
        reader: &crate::legacy::Reader,
        phase: &mut crate::assets::VerificationPhase,
    ) -> Result<()> {
        let raw = proof.capability_raw.as_deref().ok_or(Error::Invalid)?;
        record::require(
            record::safe_id(&proof.source_release)
                && record::fingerprint(&proof.artifact_digest)
                && raw.len() <= 1024
                && crate::digest(raw) == proof.capability_digest,
        )?;
        let root = self.artifact_root(&proof.source_release)?;
        record::require(crate::native::Identity::capture(&root)? == proof.root)?;
        let entries = phase.verify(&proof.source_release, &root, &proof.artifact_digest)?;
        record::require(
            entries.get("config/release-checkpoint.json") == Some(&proof.capability_digest),
        )?;
        record::require(
            crate::assets::read(&root, "config/release-checkpoint.json", 1024)? == raw,
        )?;
        let c: crate::owner::CheckpointCapability = reader.decode(raw, false, false)?;
        record::require(c.schema_version == 1 && c.contract == "migration-history-checkpoint-v1")
    }
    fn read_link(&self, name: &str) -> Result<Option<String>> {
        let stat = match fs::statat(&self.root, name, AtFlags::SYMLINK_NOFOLLOW) {
            Ok(s) => s,
            Err(rustix::io::Errno::NOENT) => return Ok(None),
            Err(_) => return Err(Error::Io),
        };
        record::require(
            FileType::from_raw_mode(stat.st_mode) == FileType::Symlink
                && stat.st_uid == rustix::process::getuid().as_raw(),
        )?;
        let link = store::io(fs::readlinkat(&self.root, name, Vec::new()))?;
        let raw = link.to_str().map_err(|_| Error::Invalid)?;
        let id = raw.strip_prefix("releases/").ok_or(Error::Invalid)?;
        self.release(id)?;
        Ok(Some(id.into()))
    }
    pub fn observe(&self) -> Result<ReleaseLinks> {
        self.current_root()?;
        Ok(ReleaseLinks {
            active: self.read_link("current")?.ok_or(Error::Invalid)?,
            previous: self.read_link("previous")?,
        })
    }
    fn stage(&self, target: &str, replace: bool) -> Result<()> {
        self.release(target)?;
        let pending = ".previous.release-transition.pending";
        let value = format!("releases/{target}");
        match fs::symlinkat(&value, &self.root, pending) {
            Ok(()) => (),
            Err(rustix::io::Errno::EXIST) => {
                if store::io(fs::readlinkat(&self.root, pending, Vec::new()))?.to_bytes()
                    != value.as_bytes()
                {
                    return Err(Error::Stale);
                }
            }
            Err(_) => return Err(Error::Io),
        }
        if replace {
            store::io(fs::renameat(&self.root, pending, &self.root, "previous"))?
        } else {
            store::io(fs::renameat_with(
                &self.root,
                pending,
                &self.root,
                "previous",
                RenameFlags::NOREPLACE,
            ))?
        }
        store::io(fs::fsync(&self.root))
    }
    pub fn activate(&self, _lease: &UpdateLease<'_>, pair: &ReleasePair) -> Result<ReleaseLinks> {
        self.activate_at(pair, crate::fixture::point)
    }
    pub(crate) fn activate_at(
        &self,
        pair: &ReleasePair,
        mut cut: impl FnMut(&str) -> Result<()>,
    ) -> Result<ReleaseLinks> {
        record::pair(pair)?;
        self.release(&pair.target)?;
        let mut actual = self.observe()?;
        if activated(&actual, pair) {
            return Ok(actual);
        }
        if pair.previous.as_ref() == Some(&pair.target) {
            if !initial(&actual, pair) || actual.previous.is_none() {
                return Err(Error::Stale);
            }
        } else {
            if initial(&actual, pair) {
                self.stage(&pair.target, actual.previous.is_some())?;
                cut("after-staged-link")?;
                actual = self.observe()?;
            }
            if !staged(&actual, pair) && !activated(&actual, pair) {
                return Err(Error::Stale);
            }
        }
        if !activated(&actual, pair) {
            self.current_root()?;
            store::io(fs::renameat_with(
                &self.root,
                "current",
                &self.root,
                "previous",
                RenameFlags::EXCHANGE,
            ))?;
            store::io(fs::fsync(&self.root))?;
            cut("after-link-exchange")?;
        }
        let actual = self.observe()?;
        if !activated(&actual, pair) {
            return Err(Error::Stale);
        }
        Ok(actual)
    }
}
pub fn initial(links: &ReleaseLinks, pair: &ReleasePair) -> bool {
    links.active == pair.from && links.previous == pair.previous
}
pub fn staged(links: &ReleaseLinks, pair: &ReleasePair) -> bool {
    links.active == pair.from && links.previous.as_ref() == Some(&pair.target)
}
pub fn activated(links: &ReleaseLinks, pair: &ReleasePair) -> bool {
    links.active == pair.target && links.previous.as_ref() == Some(&pair.from)
}
pub fn allowed(checkpoint: &str, links: &ReleaseLinks, pair: &ReleasePair) -> bool {
    if pair.from == pair.target {
        return links.active == pair.target && links.previous == pair.previous;
    }
    match checkpoint {
        "authorized" | "migrating" => initial(links, pair),
        "activation-intent" => {
            initial(links, pair) || staged(links, pair) || activated(links, pair)
        }
        "target-active" | "reconciling" | "complete" => activated(links, pair),
        _ => false,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde::Deserialize;
    use std::{
        fs as stdfs,
        os::unix::fs::{symlink, PermissionsExt},
        process::Command,
    };
    #[derive(Deserialize)]
    struct Case {
        initial: ReleaseLinks,
        pair: ReleasePair,
        cut: String,
        accepted: bool,
        after: ReleaseLinks,
        resumed: ReleaseLinks,
    }
    fn same(a: &ReleaseLinks, b: &ReleaseLinks) {
        assert_eq!(a.active, b.active);
        assert_eq!(a.previous, b.previous)
    }
    #[test]
    fn checkpoint_reader_accepts_released_asset_modes_and_refuses_special_leaves() {
        let (fixture, _, _, _) = crate::custody::tests::fixture();
        let runtime = fixture.0.join("runtime");
        let source = runtime.join("releases/source");
        for path in [
            &runtime,
            &runtime.join("releases"),
            &source,
            &source.join("config"),
        ] {
            stdfs::create_dir(path).unwrap();
            stdfs::set_permissions(path, stdfs::Permissions::from_mode(0o700)).unwrap();
        }
        let raw = b"{\"schemaVersion\":1,\"contract\":\"migration-history-checkpoint-v1\"}\n";
        let capability = source.join("config/release-checkpoint.json");
        stdfs::write(&capability, raw).unwrap();
        stdfs::set_permissions(&capability, stdfs::Permissions::from_mode(0o644)).unwrap();
        let manifest = format!("{}  ./config/release-checkpoint.json\n", crate::digest(raw));
        stdfs::write(source.join("runtime-files.sha256"), &manifest).unwrap();
        stdfs::set_permissions(
            source.join("runtime-files.sha256"),
            stdfs::Permissions::from_mode(0o644),
        )
        .unwrap();
        let links = RuntimeLinks::open(&runtime).unwrap();
        let fd = links.artifact_root("source").unwrap();
        let mut proof = crate::owner::SourceCheckpoint {
            source_release: "source".into(),
            artifact_digest: crate::digest(manifest.as_bytes()),
            root: crate::native::Identity::capture(&fd).unwrap(),
            capability_raw: Some(raw.to_vec()),
            capability_digest: crate::digest(raw),
        };
        assert!(links
            .source_checkpoint(&proof, &crate::legacy::Reader::CanonicalFixture)
            .is_ok());
        let mut phase = crate::assets::VerificationPhase::new();
        phase
            .verify(&proof.source_release, &fd, &proof.artifact_digest)
            .unwrap();
        assert!(links
            .source_checkpoint_in(&proof, &crate::legacy::Reader::CanonicalFixture, &mut phase)
            .is_ok());
        let original_manifest = proof.artifact_digest.clone();
        proof.artifact_digest = crate::digest(b"different manifest");
        assert!(links
            .source_checkpoint_in(&proof, &crate::legacy::Reader::CanonicalFixture, &mut phase)
            .is_err());
        proof.artifact_digest = original_manifest;
        let original_capability = proof.capability_digest.clone();
        proof.capability_digest = crate::digest(b"different capability");
        assert!(links
            .source_checkpoint_in(&proof, &crate::legacy::Reader::CanonicalFixture, &mut phase)
            .is_err());
        proof.capability_digest = original_capability;
        stdfs::write(&capability, b"changed bytes").unwrap();
        assert!(links
            .source_checkpoint_in(&proof, &crate::legacy::Reader::CanonicalFixture, &mut phase)
            .is_err());
        stdfs::write(&capability, raw).unwrap();
        proof.root.inode += 1;
        assert!(links
            .source_checkpoint(&proof, &crate::legacy::Reader::CanonicalFixture)
            .is_err());
        proof.root.inode -= 1;
        stdfs::remove_file(&capability).unwrap();
        symlink("../runtime-files.sha256", &capability).unwrap();
        assert!(links
            .source_checkpoint(&proof, &crate::legacy::Reader::CanonicalFixture)
            .is_err());
        stdfs::remove_file(&capability).unwrap();
        fs::mknodat(
            &fd,
            "config/release-checkpoint.json",
            FileType::Fifo,
            Mode::from_raw_mode(0o600),
            0,
        )
        .unwrap();
        let started = std::time::Instant::now();
        assert!(links
            .source_checkpoint(&proof, &crate::legacy::Reader::CanonicalFixture)
            .is_err());
        assert!(started.elapsed() < std::time::Duration::from_secs(1));
    }
    #[test]
    fn owning_go_links_and_process_cuts() {
        if let Ok(root) = std::env::var("SUBYARD_LINKS_CHILD_ROOT") {
            let pair: ReleasePair =
                serde_json::from_str(&std::env::var("SUBYARD_LINKS_CHILD_PAIR").unwrap()).unwrap();
            let cut = std::env::var("SUBYARD_LINKS_CHILD_CUT").unwrap();
            let links = RuntimeLinks::open(Path::new(&root)).unwrap();
            let result = links.activate_at(&pair, |phase| {
                if phase == cut {
                    std::process::exit(73)
                };
                Ok(())
            });
            std::process::exit(if result.is_ok() { 0 } else { 74 });
        }
        let Ok(path) = std::env::var("SUBYARD_RUST_LINKS_CORPUS") else {
            return;
        };
        let cases: Vec<Case> = serde_json::from_slice(&stdfs::read(path).unwrap()).unwrap();
        assert_eq!(cases.len(), 15);
        for (n, c) in cases.into_iter().enumerate() {
            let root =
                std::env::temp_dir().join(format!("subyard-links-{}-{n}", std::process::id()));
            stdfs::create_dir(&root).unwrap();
            stdfs::set_permissions(&root, stdfs::Permissions::from_mode(0o700)).unwrap();
            stdfs::create_dir(root.join("releases")).unwrap();
            stdfs::set_permissions(root.join("releases"), stdfs::Permissions::from_mode(0o700))
                .unwrap();
            for id in ["release-a", "release-b", "release-old", "foreign"] {
                stdfs::create_dir(root.join("releases").join(id)).unwrap();
                stdfs::set_permissions(
                    root.join("releases").join(id),
                    stdfs::Permissions::from_mode(0o700),
                )
                .unwrap()
            }
            symlink(
                format!("releases/{}", c.initial.active),
                root.join("current"),
            )
            .unwrap();
            if let Some(previous) = &c.initial.previous {
                symlink(format!("releases/{previous}"), root.join("previous")).unwrap()
            };
            if n == 0 {
                let pinned = RuntimeLinks::open(&root).unwrap();
                let held = root.with_extension("held");
                stdfs::rename(&root, &held).unwrap();
                stdfs::create_dir(&root).unwrap();
                stdfs::set_permissions(&root, stdfs::Permissions::from_mode(0o700)).unwrap();
                assert!(pinned.observe().is_err());
                assert!(pinned.activate_at(&c.pair, |_| Ok(())).is_err());
                assert_eq!(
                    stdfs::read_link(held.join("current")).unwrap(),
                    Path::new("releases/release-a")
                );
                stdfs::remove_dir(&root).unwrap();
                stdfs::rename(&held, &root).unwrap();
                drop(pinned);
            }
            let status = Command::new(std::env::current_exe().unwrap())
                .args(["--exact", "links::tests::owning_go_links_and_process_cuts"])
                .env("SUBYARD_LINKS_CHILD_ROOT", &root)
                .env(
                    "SUBYARD_LINKS_CHILD_PAIR",
                    serde_json::to_string(&c.pair).unwrap(),
                )
                .env("SUBYARD_LINKS_CHILD_CUT", &c.cut)
                .stdout(std::process::Stdio::null())
                .status()
                .unwrap();
            assert_eq!(status.success(), c.accepted, "case {n}");
            assert!(matches!(status.code(), Some(0 | 73 | 74)));
            let fresh = RuntimeLinks::open(&root).unwrap();
            same(&fresh.observe().unwrap(), &c.after);
            let _ = fresh.activate_at(&c.pair, |_| Ok(()));
            same(&fresh.observe().unwrap(), &c.resumed);
            drop(fresh);
            stdfs::remove_dir_all(root).unwrap();
        }
    }
}
