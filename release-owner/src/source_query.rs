//! Finite read-only source admission. No grant, lease, component factory or
//! operational inspection is available at this boundary.
use crate::{
    assets, codec, digest, inventory, legacy,
    links::RuntimeLinks,
    migration_owner,
    native::Identity,
    owner::{MigrationSources, SourceArtifact},
    record::{self, require},
    source_v1,
    store::{self, Store},
    work::Recipe,
    Error, Result,
};
use rustix::fs::{self, AtFlags, FileType};
use serde::{Deserialize, Serialize};
use std::{
    collections::BTreeMap,
    fs::File,
    io::{Read, Write},
    os::fd::{AsFd, AsRawFd, OwnedFd},
    os::unix::{ffi::OsStrExt, net::UnixStream},
    path::{Path, PathBuf},
    process::{Command, Stdio},
    time::{Duration, Instant},
};

#[derive(Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct Options {
    #[serde(with = "crate::wire::go_bytes")]
    pub config_home: Option<Vec<u8>>,
    #[serde(with = "crate::wire::go_bytes")]
    pub runtime_root: Option<Vec<u8>>,
    pub config_identity: Identity,
    pub runtime_identity: Identity,
    pub active: SourceArtifact,
    pub original_owner: Option<SourceArtifact>,
    pub original_sources: Option<MigrationSources>,
}
#[derive(Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct Proof {
    #[serde(with = "crate::wire::go_bytes")]
    pub journal_raw: Option<Vec<u8>>,
    pub metadata_binding: String,
    pub binding: String,
    pub retained_releases: Vec<String>,
}
#[derive(Default, Serialize, Deserialize)]
#[serde(default, rename_all = "camelCase", deny_unknown_fields)]
struct CapabilityResponse {
    schema_version: i64,
    activation_reconciliation_owned: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    capabilities: Option<crate::wire::process_v2::RecoveryCapabilities>,
    #[serde(skip_serializing_if = "crate::wire::empty_vec")]
    activation_owners: Option<Vec<String>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    inspection: Option<serde_json::Value>,
    #[serde(skip_serializing_if = "Option::is_none")]
    outcome: Option<serde_json::Value>,
}
impl legacy::Historical for CapabilityResponse {
    const KIND: legacy::Kind = legacy::Kind::SourceCapability;
}
fn root_path(raw: &[u8]) -> Result<PathBuf> {
    require(raw.starts_with(b"/") && !raw.contains(&0))?;
    let mut path = PathBuf::from("/");
    for part in raw.split(|b| *b == b'/') {
        match part {
            b"" | b"." => (),
            b".." => {
                path.pop();
            }
            _ => path.push(std::ffi::OsStr::from_bytes(part)),
        }
    }
    require(path != Path::new("/"))?;
    Ok(path)
}
fn named_root(path: &Path, fd: &impl AsFd, expected: &Identity) -> Result<()> {
    require(Identity::capture(fd)? == *expected)?;
    let named = store::io(fs::statat(fs::CWD, path, AtFlags::SYMLINK_NOFOLLOW))?;
    let actual = store::io(fs::fstat(fd))?;
    require(
        named.st_dev == actual.st_dev
            && named.st_ino == actual.st_ino
            && FileType::from_raw_mode(named.st_mode) == FileType::Directory,
    )?;
    store::checked(fd, true, false, false)
}
fn candidate(
    links: &RuntimeLinks,
    proof: &SourceArtifact,
    logical: &Path,
) -> Result<(OwnedFd, assets::Inputs, assets::Entries)> {
    links.source_artifact(proof)?;
    let root = links.artifact_root(&proof.release)?;
    let entries = assets::verify(&root, &proof.artifact_digest)?;
    let inputs = assets::inputs_bytes(
        &root,
        logical
            .join("releases")
            .join(&proof.release)
            .as_os_str()
            .as_bytes(),
        &proof.artifact_digest,
    )?;
    Ok((root, inputs, entries))
}
// This fixed mode cannot carry roots, execution, authorization or an owner
// callback. The image is the actual manifest-bound sealed original engine.
fn capability(root: &OwnedFd, expected: &str, reader: &legacy::Reader) -> Result<()> {
    let (image, hash) = legacy::sealed_source_image(assets::source_engine(root)?)?;
    require(hash == expected)?;
    for mode in ["lifecycle-capabilities", "capabilities"] {
        if probe(&image, mode)
            .and_then(|raw| {
                let response: CapabilityResponse = reader.decode(&raw, false, false)?;
                require(
                    response.schema_version == 2
                        && !response.activation_reconciliation_owned
                        && response.inspection.is_none()
                        && response.outcome.is_none()
                        && crate::wire::empty_vec(&response.activation_owners),
                )?;
                let caps = response.capabilities.ok_or(Error::Invalid)?;
                require(
                    caps.process_schema == 2
                        && caps.journal_schema == 2
                        && matches!(
                            (caps.contract.as_str(), caps.receipt_schema),
                            ("activation-only-replacement-v1", 1)
                                | ("activation-only-replacement-v2", 2)
                                | ("migration-ledger-owner-replacement-v1", 1)
                        ),
                )
            })
            .is_ok()
        {
            return Ok(());
        }
        reader.health()?;
    }
    Err(Error::Invalid)
}
fn probe(image: &File, mode: &str) -> Result<Vec<u8>> {
    let (mut socket, child_socket) = UnixStream::pair().map_err(|_| Error::Io)?;
    socket
        .set_read_timeout(Some(Duration::from_secs(10)))
        .map_err(|_| Error::Io)?;
    socket
        .set_write_timeout(Some(Duration::from_secs(10)))
        .map_err(|_| Error::Io)?;
    let mut child = Command::new(format!("/proc/self/fd/{}", image.as_raw_fd()))
        .arg("_release-transition")
        .stdin(Stdio::from(OwnedFd::from(
            child_socket.try_clone().map_err(|_| Error::Io)?,
        )))
        .stdout(Stdio::from(OwnedFd::from(child_socket)))
        .stderr(Stdio::null())
        .spawn()
        .map_err(|_| Error::Io)?;
    let result = (|| {
        let end = Instant::now() + Duration::from_secs(10);
        #[derive(Serialize)]
        #[serde(rename_all = "camelCase")]
        struct Request<'a> {
            schema_version: u8,
            mode: &'a str,
        }
        socket
            .write_all(&codec::encode(
                &Request {
                    schema_version: 2,
                    mode,
                },
                true,
            )?)
            .map_err(|_| Error::Io)?;
        socket
            .shutdown(std::net::Shutdown::Write)
            .map_err(|_| Error::Io)?;
        let mut raw = Vec::new();
        let mut block = [0u8; 65536];
        loop {
            socket
                .set_read_timeout(Some(
                    end.checked_duration_since(Instant::now())
                        .ok_or(Error::Io)?,
                ))
                .map_err(|_| Error::Io)?;
            let n = socket.read(&mut block).map_err(|_| Error::Io)?;
            if n == 0 {
                break;
            }
            require(raw.len().checked_add(n).ok_or(Error::Bound)? <= 1 << 20)?;
            raw.extend_from_slice(&block[..n]);
        }
        require(!raw.is_empty() && raw.len() <= 1 << 20)?;
        loop {
            if let Some(status) = child.try_wait().map_err(|_| Error::Io)? {
                require(status.success())?;
                return Ok(raw);
            }
            require(Instant::now() < end)?;
            std::thread::sleep(Duration::from_millis(5));
        }
    })();
    if result.is_err() {
        let _ = child.kill();
    }
    let waited = child.wait().map_err(|_| Error::Io);
    result.and_then(|raw| {
        waited?;
        Ok(raw)
    })
}

// Bind the sibling namespaces independently of the frozen source grammar.
// Existing custody/payload validators supply semantics; this is only a physical
// before/after snapshot, including names and original fstat identities.
fn native_snapshot(store: &Store) -> Result<String> {
    #[derive(Serialize)]
    struct Entry {
        identity: assets::Input,
        sha256: String,
    }
    let mut values = BTreeMap::new();
    let root = store.root.as_ref().ok_or(Error::Protected)?;
    let mut count = 0usize;
    let mut bytes = 0usize;
    fn walk(
        fd: &OwnedFd,
        path: &str,
        depth: usize,
        values: &mut BTreeMap<String, Entry>,
        count: &mut usize,
        bytes: &mut usize,
    ) -> Result<()> {
        require(depth <= 4)?;
        values.insert(
            path.into(),
            Entry {
                identity: assets::metadata(fd)?,
                sha256: String::new(),
            },
        );
        // Live8192 +256transaction dirs +8192payload roots +16384pieces
        // +pending/published indexes +one258-file retirement stage fit65536.
        for name in inventory::names(fd, 65536usize.checked_sub(*count).ok_or(Error::Bound)?)? {
            *count += 1;
            let path = format!("{path}/{name}");
            let stat = store::io(fs::statat(fd, &name, AtFlags::SYMLINK_NOFOLLOW))?;
            if FileType::from_raw_mode(stat.st_mode) == FileType::Directory {
                let child = store::directory(fd, std::slice::from_ref(&name), false)?
                    .ok_or(Error::Stale)?;
                walk(&child, &path, depth + 1, values, count, bytes)?;
                store::linked_directory(fd, &name, &child)?;
            } else {
                let raw = store::read_at(fd, &name, true)?;
                let raw = raw.bytes().ok_or(Error::Stale)?;
                *bytes = bytes.checked_add(raw.len()).ok_or(Error::Bound)?;
                require(*bytes <= 384 << 20)?;
                let after = store::io(fs::statat(fd, &name, AtFlags::SYMLINK_NOFOLLOW))?;
                require(assets::input(stat) == assets::input(after))?;
                values.insert(
                    path,
                    Entry {
                        identity: assets::input(after),
                        sha256: digest(raw),
                    },
                );
            }
        }
        Ok(())
    }
    for name in ["native-custody", "native-prepared"] {
        if let Some(dir) = store::directory(root, &["migrations".into(), name.into()], false)? {
            walk(&dir, name, 0, &mut values, &mut count, &mut bytes)?;
        }
    }
    codec::source_fingerprint(&values)
}
fn cleanup(
    store: &Store,
    journal: Option<&record::Journal>,
    registry: &record::Registry,
    links: &RuntimeLinks,
    originals: Option<&MigrationSources>,
    declared: bool,
) -> Result<Vec<String>> {
    // Without original OFD/update lease, an accepted local cleanup with no
    // terminal cannot be proved drained. This intentionally uses generic exact
    // closure, not the more permissive cleanup continuation inspection mode.
    store.native_inventory(journal.map(|j| j.wire()), false)?;
    let Some(journal) = journal else {
        require(!store.native_cleanup_pending()?)?;
        return Ok(Vec::new());
    };
    let j = journal.wire();
    if !store.has_bundle(&j.transaction)? {
        require(!declared && !store.native_cleanup_pending()?)?;
        return Ok(Vec::new());
    }
    let bundle = store.bundle(journal, registry)?;
    if j.checkpoint != "complete" {
        return Ok(Vec::new());
    }
    let ordinal = bundle
        .work
        .len()
        .checked_add(bundle.targets.len())
        .and_then(|n| n.checked_add(1))
        .ok_or(Error::Bound)? as u64;
    let mut pending = false;
    for (index, work) in bundle.work.iter().enumerate() {
        if let Recipe::TestYardOwnerRegistration(before) = &work.recipe {
            let step = j
                .steps
                .as_ref()
                .and_then(|s| s.get(index))
                .ok_or(Error::Invalid)?;
            if let Some((token, _)) = store.owner_cleanup_recipe(j, step, before)? {
                #[derive(Serialize)]
                struct Recipe<'a> {
                    kind: &'a str,
                    before: &'a crate::work::OwnerBefore,
                    token: &'a str,
                }
                let hash = digest(&codec::encode(
                    &Recipe {
                        kind: "cleanup-owner",
                        before,
                        token: &token,
                    },
                    false,
                )?);
                pending |= store.required_cleanup_pending(j, &hash, ordinal)?;
            } else {
                store.completed_cleanup_without_recovery(j, ordinal)?;
            }
        }
    }
    if !pending {
        require(!store.native_cleanup_pending()?)?;
        return Ok(Vec::new());
    }
    let originals = originals.ok_or(Error::Stale)?;
    let verify = |resource: &str, proof: &SourceArtifact, release: &str| -> Result<()> {
        require(proof.release == release)?;
        let observed = bundle
            .facts
            .observations
            .iter()
            .flatten()
            .find(|o| o.resource == resource)
            .ok_or(Error::Stale)?;
        require(observed.fingerprint == digest(&codec::encode(proof, false)?))?;
        links.source_artifact(proof)
    };
    verify(
        "release.source-artifact",
        &originals.source,
        &bundle.facts.releases.from,
    )?;
    require(
        originals.previous.as_ref().map(|p| &p.release) == bundle.facts.releases.previous.as_ref(),
    )?;
    if let Some(previous) = &originals.previous {
        verify("release.previous-artifact", previous, &previous.release)?;
    }
    let mut retained = vec![j.releases.from.clone(), j.releases.target.clone()];
    retained.extend(j.releases.previous.iter().cloned());
    retained.sort();
    retained.dedup();
    Ok(retained)
}
// Shared registry admission; selection and source history remain owned by Rust.
fn selected_registry(
    store: &Store,
    root: &OwnedFd,
    entries: &assets::Entries,
) -> Result<(record::Registry, String)> {
    let raw = assets::registry(root)?;
    let hash = digest(&raw);
    require(entries.get("config/release-transition.json") == Some(&hash))?;
    let catalog = if entries.contains_key("config/release-owner.json") {
        migration_owner::current_catalog()?
    } else {
        migration_owner::builtin_catalog(true, true)?
    };
    Ok((
        record::Registry::new(store.decode(&raw, false, false)?, &catalog)?,
        hash,
    ))
}

fn select_registry_owner<'a>(
    current: Option<&record::Journal>,
    active: &'a SourceArtifact,
    active_registry: Option<&String>,
    original: Option<&'a SourceArtifact>,
) -> Result<&'a SourceArtifact> {
    if let Some(journal) = current.filter(|j| active_registry != Some(&j.wire().registry_digest)) {
        require(journal.wire().goal.direction == "activate-previous")?;
        let original = original.ok_or(Error::Invalid)?;
        require(original.release == journal.wire().releases.from)?;
        Ok(original)
    } else {
        require(original.is_none())?;
        Ok(active)
    }
}
fn complete_snapshot_reference(
    journal: &record::Journal,
    snapshot: &store::Snapshot,
    metadata: &str,
) -> Result<crate::native::CompleteSourceReference> {
    let raw = codec::encode(journal.wire(), true)?;
    require(
        journal.wire().checkpoint == "complete"
            && snapshot.bytes() == Some(raw.as_slice())
            && record::fingerprint(metadata),
    )?;
    Ok(crate::native::CompleteSourceReference {
        transaction: journal.wire().transaction.clone(),
        journal_digest: digest(&raw),
        metadata_binding: metadata.into(),
    })
}

pub(crate) fn complete_reference(
    owner: &crate::owner::Owner,
) -> Result<Option<crate::native::CompleteSourceReference>> {
    let Some((journal, snapshot)) = owner.store.read_checked_journal()? else {
        return Ok(None);
    };
    require(journal.wire().checkpoint == "complete")?;
    let links = owner.links.observe()?;
    let candidates = [
        owner.options.source_artifact.as_ref(),
        owner.options.target_artifact.as_ref(),
        owner.options.previous_artifact.as_ref(),
    ];
    let active = candidates
        .iter()
        .flatten()
        .find(|p| p.release == links.active)
        .ok_or(Error::Invalid)?;
    let previous = if journal.wire().releases.from != journal.wire().releases.target {
        Some(&journal.wire().releases.from)
    } else {
        journal.wire().releases.previous.as_ref()
    };
    require(
        journal.wire().goal.target == links.active
            && previous == links.previous.as_ref()
            && journal.wire().artifact_digest == active.artifact_digest,
    )?;
    let (mut root, _, mut entries) =
        candidate(&owner.links, active, Path::new(&owner.options.runtime_root))?;
    let original =
        if entries.get("config/release-transition.json") != Some(&journal.wire().registry_digest) {
            candidates
                .iter()
                .flatten()
                .find(|p| p.release == journal.wire().releases.from)
                .copied()
        } else {
            None
        };
    let selected = select_registry_owner(
        Some(&journal),
        active,
        entries.get("config/release-transition.json"),
        original,
    )?;
    if selected.release != active.release {
        let chosen = candidate(
            &owner.links,
            selected,
            Path::new(&owner.options.runtime_root),
        )?;
        root = chosen.0;
        entries = chosen.2;
    }
    let (registry, hash) = selected_registry(&owner.store, &root, &entries)?;
    let metadata = if entries.contains_key("config/release-owner.json") {
        owner.store.inspect_rust_source(&registry, &hash)?
    } else {
        owner.store.inspect_source(&registry, &hash)?
    };
    require(
        metadata
            .journal()
            .is_some_and(|j| j.wire() == journal.wire())
            && owner.links.observe()? == links,
    )?;
    Ok(Some(complete_snapshot_reference(
        &journal,
        &snapshot,
        metadata.binding(),
    )?))
}

pub fn inspect(options: Options, helper: File, config: File, runtime: File) -> Result<Proof> {
    let config_path = root_path(options.config_home.as_deref().ok_or(Error::Invalid)?)?;
    let runtime_path = root_path(options.runtime_root.as_deref().ok_or(Error::Invalid)?)?;
    named_root(&config_path, &config, &options.config_identity)?;
    named_root(&runtime_path, &runtime, &options.runtime_identity)?;
    let store = Store::pinned(config.into(), legacy::FrozenReader::capture_helper(helper)?)?;
    let links = RuntimeLinks::pinned(&runtime_path, runtime.into())?;
    inspect_pinned(&options, &store, &links)
}

fn inspect_pinned(options: &Options, store: &Store, links: &RuntimeLinks) -> Result<Proof> {
    let config_path = root_path(options.config_home.as_deref().ok_or(Error::Invalid)?)?;
    let runtime_path = root_path(options.runtime_root.as_deref().ok_or(Error::Invalid)?)?;
    named_root(
        &config_path,
        store.root.as_ref().ok_or(Error::Protected)?,
        &options.config_identity,
    )?;
    let before = links.observe()?;
    require(before.active == options.active.release)?;
    let current = store.read_checked_journal()?;
    if let Some((journal, _)) = &current {
        let j = journal.wire();
        let previous = if j.releases.from != j.releases.target {
            Some(&j.releases.from)
        } else {
            j.releases.previous.as_ref()
        };
        require(
            j.goal.target == before.active
                && previous == before.previous.as_ref()
                && j.artifact_digest == options.active.artifact_digest,
        )?;
    }
    let (active_root, mut files, active_entries) =
        candidate(links, &options.active, &runtime_path)?;
    let selected = select_registry_owner(
        current.as_ref().map(|(j, _)| j),
        &options.active,
        active_entries.get("config/release-transition.json"),
        options.original_owner.as_ref(),
    )?;
    let (owner_root, owner_inputs, entries) = if selected.release == options.active.release {
        (
            store::io(rustix::io::dup(&active_root))?,
            files.clone(),
            active_entries,
        )
    } else {
        candidate(links, selected, &runtime_path)?
    };
    files.extend(owner_inputs);
    let (registry, registry_digest) = selected_registry(store, &owner_root, &entries)?;
    let metadata = if entries.contains_key("config/release-owner.json") {
        store.inspect_rust_source(&registry, &registry_digest)?
    } else {
        store.inspect_source(&registry, &registry_digest)?
    };
    if let Some((journal, snapshot)) = &current {
        require(
            metadata
                .journal()
                .is_some_and(|j| j.wire() == journal.wire())
                && snapshot.bytes() == Some(codec::encode(journal.wire(), true)?.as_slice()),
        )?;
        if journal.wire().checkpoint != "complete" {
            require(entries.contains_key("config/release-owner.json"))?;
            capability(&owner_root, &selected.engine_digest, &store.reader)?;
        }
    } else {
        require(metadata.journal().is_none())?;
    }
    let native_before = native_snapshot(store)?;
    let retained = cleanup(
        store,
        metadata.journal(),
        &registry,
        links,
        options.original_sources.as_ref(),
        entries.contains_key("config/release-owner.json"),
    )?;
    let legacy = source_v1::binding(store, config_path.as_os_str().as_bytes())?;
    files.extend(links.source_inputs()?);
    let binding = codec::source_hash_with(|out| {
        out.write_all(b"{\"Metadata\":").map_err(|_| Error::Io)?;
        codec::write_source_json(out, &metadata.binding())?;
        out.write_all(b",\"Legacy\":").map_err(|_| Error::Io)?;
        codec::write_source_json(out, &legacy)?;
        out.write_all(b",\"Links\":").map_err(|_| Error::Io)?;
        codec::write_source_json(out, &before)?;
        out.write_all(b",\"Files\":").map_err(|_| Error::Io)?;
        codec::write_source_map(out, &files.0)?;
        out.write_all(b"}").map_err(|_| Error::Io)
    })?;
    require(
        before == links.observe()?
            && legacy == source_v1::binding(store, config_path.as_os_str().as_bytes())?
            && metadata.binding() == store.source_metadata()?.binding()
            && native_before == native_snapshot(store)?,
    )?;
    let (_, mut after, _) = candidate(links, &options.active, &runtime_path)?;
    if selected.release != options.active.release {
        after.extend(candidate(links, selected, &runtime_path)?.1);
    }
    after.extend(links.source_inputs()?);
    require(files == after)?;
    require(
        retained
            == cleanup(
                store,
                metadata.journal(),
                &registry,
                links,
                options.original_sources.as_ref(),
                entries.contains_key("config/release-owner.json"),
            )?,
    )?;
    named_root(
        &config_path,
        store.root.as_ref().ok_or(Error::Protected)?,
        &options.config_identity,
    )?;
    store.reader.health()?;
    Ok(Proof {
        journal_raw: current.and_then(|(_, s)| s.bytes().map(Vec::from)),
        metadata_binding: metadata.binding().into(),
        binding,
        retained_releases: retained,
    })
}

fn completed_options(owner: &crate::owner::Owner) -> Result<Options> {
    let current = owner.store.read_checked_journal()?.ok_or(Error::Invalid)?;
    let journal = current.0.wire();
    require(journal.checkpoint == "complete")?;
    let mut facts = BTreeMap::<&str, &SourceArtifact>::new();
    for fact in owner
        .options
        .source_artifact
        .iter()
        .chain(owner.options.target_artifact.iter())
        .chain(owner.options.previous_artifact.iter())
        .chain(owner.options.original_sources.iter().map(|s| &s.source))
        .chain(
            owner
                .options
                .original_sources
                .iter()
                .flat_map(|s| s.previous.iter()),
        )
    {
        if let Some(previous) = facts.insert(&fact.release, fact) {
            require(codec::encode(previous, false)? == codec::encode(fact, false)?)?;
        }
    }
    let links = owner.links.observe()?;
    let active = facts.get(links.active.as_str()).ok_or(Error::Invalid)?;
    let active_registry = digest(&assets::registry(
        &owner.links.artifact_root(&active.release)?,
    )?);
    let original_owner = if active_registry != journal.registry_digest {
        facts
            .get(journal.releases.from.as_str())
            .map(|f| (*f).clone())
    } else {
        None
    };
    let original_sources =
        facts
            .get(journal.releases.from.as_str())
            .map(|source| MigrationSources {
                source: (*source).clone(),
                previous: journal
                    .releases
                    .previous
                    .as_ref()
                    .and_then(|id| facts.get(id.as_str()))
                    .map(|f| (*f).clone()),
            });
    Ok(Options {
        config_home: Some(owner.options.config_home.as_bytes().to_vec()),
        runtime_root: Some(owner.options.runtime_root.as_bytes().to_vec()),
        config_identity: Identity::capture(owner.store.root.as_ref().ok_or(Error::Protected)?)?,
        runtime_identity: owner.links.source_identity()?,
        active: (*active).clone(),
        original_owner,
        original_sources,
    })
}

pub(crate) fn completed_proof(owner: &crate::owner::Owner) -> Result<Proof> {
    inspect_pinned(&completed_options(owner)?, &owner.store, &owner.links)
}

// The source's own head is authoritative. A future target may introduce another
// Rust migration, but cannot reinterpret an unsettled predecessor as complete.
pub(crate) fn fresh_source_supported(owner: &crate::owner::Owner) -> Result<bool> {
    let before = owner.links.observe()?;
    let proof = owner
        .options
        .source_artifact
        .iter()
        .chain(owner.options.target_artifact.iter())
        .chain(owner.options.previous_artifact.iter())
        .find(|proof| proof.release == before.active)
        .ok_or(Error::Invalid)?;
    let (root, _, entries) =
        candidate(&owner.links, proof, Path::new(&owner.options.runtime_root))?;
    let (registry, hash) = selected_registry(&owner.store, &root, &entries)?;
    let metadata = if entries.contains_key("config/release-owner.json") {
        owner.store.inspect_rust_source(&registry, &hash)?
    } else {
        owner.store.inspect_source(&registry, &hash)?
    };
    let version = semver::Version::parse(&proof.version).map_err(|_| Error::Invalid)?;
    let bridge = [
        ("owner-registration", 2),
        ("power-metadata", 2),
        ("project-state", 1),
        ("settings", 3),
    ];
    let head = registry
        .wire()
        .current_epochs
        .as_ref()
        .ok_or(Error::Invalid)?;
    let supported = version >= semver::Version::new(0, 22, 0)
        && bridge
            .iter()
            .all(|(domain, epoch)| head.get(*domain).is_some_and(|v| v >= epoch));
    // Preserve the existing first activation of this exact current image. Old
    // raw ingress and accepted metadata cannot be relabelled as bootstrap.
    let bootstrap = metadata.journal().is_none()
        && metadata.ledger().authority().bytes().is_none()
        && owner.options.source_ingress.is_none()
        && before.previous.is_none()
        && proof.release == owner.options.releases.target
        && proof.artifact_digest == owner.options.artifact_digest
        && proof.engine_digest == owner.helper_digest()
        && owner
            .store
            .read(&store::Address::at(
                vec!["migrations".into()],
                "state.json".into(),
                false,
            ))?
            .bytes()
            .is_none()
        && store::directory(
            owner.store.root.as_ref().ok_or(Error::Protected)?,
            &["migrations".into(), "transactions".into()],
            false,
        )?
        .is_none();
    if bootstrap {
        return Ok(supported);
    }
    let complete = metadata
        .journal()
        .is_some_and(|j| j.wire().checkpoint == "complete")
        && registry
            .history()
            .complete(metadata.ledger().ledger().authority())
            .is_ok()
        && bridge.iter().all(|(domain, epoch)| {
            metadata
                .ledger()
                .ledger()
                .authority()
                .domains()
                .get(*domain)
                .is_some_and(|state| state.epoch() >= *epoch)
        });
    require(before == owner.links.observe()?)?;
    Ok(supported && complete)
}

/// Dedicated one-shot ingress. Ordinary stream reads start only AFTER the
/// single ancillary transfer, so no buffered reader can consume its sentinel.
pub fn run() -> Result<()> {
    use rustix::net::{RecvAncillaryBuffer, RecvAncillaryMessage, RecvFlags, ReturnFlags};
    let fd = store::io(rustix::io::dup(std::io::stdin()))?;
    let mut socket = UnixStream::from(fd);
    socket
        .set_read_timeout(Some(Duration::from_secs(5)))
        .map_err(|_| Error::Io)?;
    socket
        .set_write_timeout(Some(Duration::from_secs(5)))
        .map_err(|_| Error::Io)?;
    let mut byte = [0];
    let mut space = [std::mem::MaybeUninit::uninit(); rustix::cmsg_space!(ScmRights(3))];
    let mut ancillary = RecvAncillaryBuffer::new(&mut space);
    let message = rustix::net::recvmsg(
        &socket,
        &mut [std::io::IoSliceMut::new(&mut byte)],
        &mut ancillary,
        RecvFlags::CMSG_CLOEXEC,
    )
    .map_err(|_| Error::Io)?;
    let mut files = Vec::new();
    let mut unexpected = false;
    for message in ancillary.drain() {
        match message {
            RecvAncillaryMessage::ScmRights(fds) => files.extend(fds),
            _ => unexpected = true,
        }
    }
    require(
        !unexpected
            && message.bytes == 1
            && byte == *b"S"
            && !message
                .flags
                .intersects(ReturnFlags::CTRUNC | ReturnFlags::TRUNC)
            && files.len() == 3,
    )?;
    let runtime = File::from(files.pop().ok_or(Error::Invalid)?);
    let config = File::from(files.pop().ok_or(Error::Invalid)?);
    let helper = File::from(files.pop().ok_or(Error::Invalid)?);
    let mut raw = Vec::new();
    let end = Instant::now() + Duration::from_secs(5);
    let mut block = [0u8; 4096];
    loop {
        socket
            .set_read_timeout(Some(
                end.checked_duration_since(Instant::now())
                    .ok_or(Error::Io)?,
            ))
            .map_err(|_| Error::Io)?;
        let n = socket.read(&mut block).map_err(|_| Error::Io)?;
        require(n != 0 && raw.len().checked_add(n).ok_or(Error::Bound)? <= 64 << 10)?;
        let newline = block[..n].iter().position(|b| *b == b'\n');
        if let Some(at) = newline {
            require(at + 1 == n)?;
            raw.extend_from_slice(&block[..n]);
            break;
        }
        raw.extend_from_slice(&block[..n]);
    }
    require(!raw.is_empty() && raw.len() <= 64 << 10 && raw.last() == Some(&b'\n'))?;
    let options: Options = serde_json::from_slice(&raw).map_err(|_| Error::Codec)?;
    let proof = inspect(options, helper, config, runtime)?;
    socket
        .write_all(&codec::encode_source_proof(&proof)?)
        .map_err(|_| Error::Io)
}

#[cfg(test)]
mod continuation_tests {
    use super::*;
    fn journal(checkpoint: &str) -> record::Journal {
        let a = "a".repeat(64);
        let mut j = crate::wire::journal_v2::Record {
            schema_version: 2,
            transaction: "original-source".into(),
            goal: crate::wire::journal_v2::Goal {
                target: "source".into(),
                direction: "activate-target".into(),
            },
            releases: crate::wire::journal_v2::ReleasePair {
                from: "source".into(),
                target: "source".into(),
                previous: None,
            },
            artifact_digest: a.clone(),
            registry_digest: a.clone(),
            catalog_digest: a.clone(),
            observation_scope: a.clone(),
            authorization_plan: format!("plan-v1-{a}"),
            resume_plan: format!("resume-v1-{a}"),
            authorization_digest: a,
            checkpoint: checkpoint.into(),
            ..Default::default()
        };
        j.intent_digest = record::Journal::intent_digest(&j).unwrap();
        record::Journal::new(j).unwrap()
    }

    struct SourceFixture(std::path::PathBuf);
    impl SourceFixture {
        fn new() -> Self {
            use std::os::unix::fs::PermissionsExt;
            let path = std::path::PathBuf::from(format!(
                "/tmp/subyard-continuation-source-{}-{}",
                std::process::id(),
                std::time::SystemTime::now()
                    .duration_since(std::time::UNIX_EPOCH)
                    .unwrap()
                    .as_nanos()
            ));
            std::fs::create_dir(&path).unwrap();
            std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o700)).unwrap();
            Self(path)
        }
        fn write(&self, relative: &str, raw: &[u8]) {
            use std::os::unix::fs::PermissionsExt;
            let path = self.0.join(relative);
            let mut missing = vec![];
            let mut parent = path.parent().unwrap();
            while !parent.exists() {
                missing.push(parent.to_owned());
                parent = parent.parent().unwrap();
            }
            for dir in missing.iter().rev() {
                std::fs::create_dir(dir).unwrap();
                std::fs::set_permissions(dir, std::fs::Permissions::from_mode(0o700)).unwrap();
            }
            std::fs::write(&path, raw).unwrap();
            std::fs::set_permissions(
                path,
                std::fs::Permissions::from_mode(if relative.ends_with("/yard-engine") {
                    0o700
                } else {
                    0o600
                }),
            )
            .unwrap();
        }
        fn owner(&self) -> crate::owner::Owner {
            self.owner_with_head(BTreeMap::from([("synthetic".into(), 1)]))
        }
        fn owner_with_head(&self, head: BTreeMap<String, i64>) -> crate::owner::Owner {
            use crate::owner::{Options, Owner};
            use std::{cell::RefCell, os::unix::net::UnixStream};
            let catalog = migration_owner::builtin_catalog(true, true).unwrap();
            let registry = record::Registry::new(
                crate::wire::registry_v2::RegistryV2 {
                    schema_version: 2,
                    minimum_epochs: Some(head.clone()),
                    current_epochs: Some(head),
                    migrations: Some(vec![]),
                    ..Default::default()
                },
                &catalog,
            )
            .unwrap();
            let raw = codec::encode(registry.wire(), false).unwrap();
            let registry_hash = digest(&raw);
            let engine = b"original immutable source image";
            self.write(
                "runtime/releases/1.0.0-source/config/release-transition.json",
                &raw,
            );
            self.write("runtime/releases/1.0.0-source/bin/yard-engine", engine);
            let manifest = format!(
                "{}  ./bin/yard-engine\n{}  ./config/release-transition.json\n",
                digest(engine),
                registry_hash
            );
            self.write(
                "runtime/releases/1.0.0-source/runtime-files.sha256",
                manifest.as_bytes(),
            );
            std::os::unix::fs::symlink("releases/1.0.0-source", self.0.join("runtime/current"))
                .unwrap();
            let mut j = journal("complete").wire().clone();
            j.releases.from = "1.0.0-source".into();
            j.releases.target = "1.0.0-source".into();
            j.goal.target = "1.0.0-source".into();
            j.artifact_digest = digest(manifest.as_bytes());
            j.registry_digest = registry_hash.clone();
            j.catalog_digest = catalog.digest().into();
            self.write(
                "config/release-transition/v2/journal.json",
                &codec::encode(&j, true).unwrap(),
            );
            self.write(
                "config/release-transition/v2/ledger.json",
                &migration_owner::ledger_bytes(&registry.history().baseline()).unwrap(),
            );
            self.write("data/present", b"fixture");
            let root = self.0.join("runtime/releases/1.0.0-source");
            let identity = Identity::capture(&File::open(&root).unwrap()).unwrap();
            let artifact = SourceArtifact {
                release: "1.0.0-source".into(),
                version: "1.0.0".into(),
                artifact_digest: j.artifact_digest.clone(),
                engine_digest: digest(engine),
                root: identity,
            };
            let (socket, _peer) = UnixStream::pair().unwrap();
            let options = Options {
                command_overrides: vec![],
                data_home: self.0.join("data").to_str().unwrap().into(),
                config_home: self.0.join("config").to_str().unwrap().into(),
                runtime_root: self.0.join("runtime").to_str().unwrap().into(),
                repository_root: root.to_str().unwrap().into(),
                yard: "default".into(),
                releases: j.releases.clone(),
                artifact_digest: j.artifact_digest.clone(),
                registry_raw: Some(raw),
                catalog: catalog.descriptors().clone().unwrap(),
                inherited_setting_ids: vec![],
                rollback_target: None,
                migration_checkpoint: true,
                source_ingress: None,
                source_checkpoint: None,
                source_artifact: Some(artifact),
                target_artifact: None,
                previous_artifact: None,
                candidate_version: "1.0.0".into(),
                config_layout: None,
                replacement: None,
                migration_sources: None,
                original_sources: None,
            };
            Owner {
                repository: File::open(root).unwrap(),
                first_cause: RefCell::new(None),
                last_cause: RefCell::new(None),
                outer: crate::outer_custody::OuterCustody::capture(
                    &self.0.join("data"),
                    File::from(OwnedFd::from(socket)),
                )
                .unwrap(),
                store: Store::open_canonical_fixture(&self.0.join("config")).unwrap(),
                links: RuntimeLinks::open(&self.0.join("runtime")).unwrap(),
                options,
                registry,
                catalog,
                registry_digest: registry_hash,
                session: "c".repeat(64),
            }
        }
    }
    impl Drop for SourceFixture {
        fn drop(&mut self) {
            std::fs::remove_dir_all(&self.0).unwrap();
        }
    }
    #[test]
    fn fresh_source_requires_its_own_complete_bridge_frontier() {
        for case in [
            "source-head",
            "target-future-head",
            "old-source-head",
            "pending-go",
            "unknown-evidence",
        ] {
            let fixture = SourceFixture::new();
            let mut head = BTreeMap::from([
                ("owner-registration".into(), 2),
                ("power-metadata".into(), 2),
                ("project-state".into(), 1),
                ("settings".into(), 3),
            ]);
            if case == "old-source-head" {
                head.insert("settings".into(), 2);
            }
            let mut owner = fixture.owner_with_head(head.clone());
            if case == "target-future-head" {
                head.insert("settings".into(), 4);
                owner.registry = record::Registry::new(
                    crate::wire::registry_v2::RegistryV2 {
                        schema_version: 2,
                        minimum_epochs: Some(head.clone()),
                        current_epochs: Some(head),
                        migrations: Some(vec![]),
                        ..Default::default()
                    },
                    &owner.catalog,
                )
                .unwrap();
            }
            if case == "pending-go" {
                let mut journal = owner
                    .store
                    .read_checked_journal()
                    .unwrap()
                    .unwrap()
                    .0
                    .wire()
                    .clone();
                journal.checkpoint = "reconciling".into();
                fixture.write(
                    "config/release-transition/v2/journal.json",
                    &codec::encode(&journal, true).unwrap(),
                );
            }
            if case == "unknown-evidence" {
                fixture.write("config/release-transition/v2/transactions/original-source/evidence/unlisted.json", b"{}\n");
            }
            let journal_path = fixture.0.join("config/release-transition/v2/journal.json");
            let ledger_path = fixture.0.join("config/release-transition/v2/ledger.json");
            let before = (
                std::fs::read(&journal_path).unwrap(),
                std::fs::read(&ledger_path).unwrap(),
            );
            let result = fresh_source_supported(&owner);
            if case == "unknown-evidence" {
                assert!(result.is_err());
            } else {
                assert_eq!(
                    result.unwrap(),
                    matches!(case, "source-head" | "target-future-head"),
                    "{case}"
                );
            }
            assert_eq!(
                (
                    std::fs::read(journal_path).unwrap(),
                    std::fs::read(ledger_path).unwrap()
                ),
                before
            );
        }
    }
    #[test]
    fn matching_registry_retains_independent_original_sources() {
        let fixture = SourceFixture::new();
        let mut owner = fixture.owner();
        let source = owner.options.source_artifact.clone().unwrap();
        let mut previous = source.clone();
        previous.release = "1.0.0-previous".into();
        owner.options.previous_artifact = Some(previous.clone());
        let mut journal = owner
            .store
            .read_checked_journal()
            .unwrap()
            .unwrap()
            .0
            .wire()
            .clone();
        journal.releases.previous = Some(previous.release.clone());
        journal.intent_digest = record::Journal::intent_digest(&journal).unwrap();
        fixture.write(
            "config/release-transition/v2/journal.json",
            &codec::encode(&journal, true).unwrap(),
        );
        let selected = completed_options(&owner).unwrap();
        assert!(selected.original_owner.is_none());
        let originals = selected.original_sources.unwrap();
        assert_eq!(
            codec::encode(&originals.source, false).unwrap(),
            codec::encode(&source, false).unwrap()
        );
        assert_eq!(
            codec::encode(&originals.previous.unwrap(), false).unwrap(),
            codec::encode(&previous, false).unwrap()
        );
        let mut conflicting = source.clone();
        conflicting.engine_digest = "f".repeat(64);
        owner.options.target_artifact = Some(conflicting);
        assert!(completed_options(&owner).is_err());
    }

    #[test]
    fn completed_metadata_uses_exact_full_source_proof() {
        for fault in ["supported", "missing", "tampered", "foreign", "conflict"] {
            let fixture = SourceFixture::new();
            let mut owner = fixture.owner();
            let proof = completed_proof(&owner).unwrap();
            assert_ne!(proof.binding, proof.metadata_binding);
            match fault {
                "supported" => (),
                "missing" => {
                    std::fs::remove_file(fixture.0.join("config/release-transition/v2/ledger.json"))
                        .unwrap()
                }
                "tampered" => {
                    fixture.write("config/release-transition/v2/journal.json", b"invalid")
                }
                "foreign" => owner.options.source_artifact.as_mut().unwrap().root.inode += 1,
                "conflict" => {
                    let mut other = owner.options.source_artifact.clone().unwrap();
                    other.engine_digest = "f".repeat(64);
                    owner.options.target_artifact = Some(other);
                }
                _ => unreachable!(),
            }
            let mut observations = vec![];
            let mut decisions = vec![];
            owner.observe_completed_metadata(&mut observations, &mut decisions);
            if fault == "supported" {
                assert_eq!(observations.len(), 1);
                assert_eq!(observations[0].resource, "transition.source-metadata");
                assert_eq!(observations[0].class, "completed-metadata-v1");
                assert_eq!(observations[0].fingerprint, proof.binding);
                assert_eq!(decisions.len(), 1);
                assert_eq!(decisions[0].resource, observations[0].resource);
                assert_eq!(decisions[0].decision, "preserve");
                assert_eq!(decisions[0].result, proof.binding);
            } else {
                assert!(
                    completed_proof(&owner).is_err(),
                    "invalid full proof accepted: {fault}"
                );
                assert!(
                    observations.is_empty() && decisions.is_empty(),
                    "unproved annotation emitted: {fault}"
                );
            }
        }
    }

    #[test]
    fn pending_inspection_retains_plan_and_reports_checked_current_links() {
        let fixture = SourceFixture::new();
        let owner = fixture.owner();
        let current = owner.links.observe().unwrap();
        let mut journal = journal("reconciling").wire().clone();
        journal.goal.target = current.active.clone();
        journal.releases.from = "prior-release".into();
        journal.releases.target = current.active.clone();
        journal.releases.previous = Some("older-release".into());
        journal.intent_digest = record::Journal::intent_digest(&journal).unwrap();
        let journal = record::Journal::new(journal).unwrap();
        let facts = crate::wire::plan_v1::PlanFacts {
            goal: crate::wire::process_v1::Goal {
                target: current.active.clone(),
                direction: "activate-target".into(),
            },
            releases: journal.wire().releases.clone(),
            links: crate::wire::recovery_v1::ReleaseLinks {
                active: "prior-release".into(),
                previous: Some("older-release".into()),
            },
            artifact_digest: owner.options.artifact_digest.clone(),
            registry_digest: owner.registry_digest.clone(),
            catalog_digest: owner.catalog.digest().into(),
            observation_scope: digest(b"original scope"),
            assessment: crate::plan::assess(true, false, &[]).unwrap(),
            ..Default::default()
        };
        let plan = crate::plan::Plan::new(facts).unwrap();
        let bytes = codec::encode(plan.wire(), false).unwrap();
        let token = plan.token().to_owned();
        let observation = crate::owner::Observation {
            observed_links: current.clone(),
            plan,
            work: vec![],
            journal: Some(journal),
            journal_snapshot: crate::store::Snapshot::captured(None),
            targets: vec![],
        };
        let inspection = owner.projected_inspection(&observation);
        let outcome = inspection.outcome.unwrap();
        assert_eq!(outcome.active, current.active);
        assert_eq!(outcome.previous, current.previous);
        assert_eq!(outcome.status, "recovering");
        assert_eq!(
            inspection.plan,
            observation.journal.as_ref().unwrap().wire().resume_plan
        );
        assert_eq!(observation.plan.token(), token);
        assert_eq!(
            codec::encode(observation.plan.wire(), false).unwrap(),
            bytes
        );
    }

    #[test]
    fn retained_authority_pending_artifacts_reject_unlisted_evidence() {
        let fixture = SourceFixture::new();
        let owner = fixture.owner();
        let mut journal = owner
            .store
            .read_checked_journal()
            .unwrap()
            .unwrap()
            .0
            .wire()
            .clone();
        journal.checkpoint = "authorized".into();
        let before = crate::store::Snapshot::captured(None);
        let after = codec::encode(
            crate::record::Ledger::from_authority(
                &owner.registry.history().baseline(),
                &owner.registry,
            )
            .unwrap()
            .wire(),
            true,
        )
        .unwrap();
        let mut work = vec![crate::work::Work {
            intent: crate::wire::plan_v1::PlannerStepIntent {
                id: "synthetic.ledger".into(),
                migration: "synthetic".into(),
                resource: "ledger.synthetic".into(),
                decision: "transform".into(),
                expected: before.fingerprint(),
                desired: digest(&after),
            },
            recipe: crate::work::Recipe::LedgerAdvance { before, after },
        }];
        let intent = &work[0].intent;
        journal.steps = Some(vec![crate::wire::journal_v2::Step {
            id: intent.id.clone(),
            migration: intent.migration.clone(),
            resource: intent.resource.clone(),
            decision: intent.decision.clone(),
            expected: intent.expected.clone(),
            desired: intent.desired.clone(),
            checkpoint: "intent".into(),
            ..Default::default()
        }]);
        assert!(owner.verify_pending_artifacts(&journal, &work).is_ok());
        let evidence = crate::wire::journal_v2::Evidence {
            schema_version: 2,
            transaction: journal.transaction.clone(),
            releases: journal.releases.clone(),
            step: "unlisted.ledger".into(),
            expected: intent.expected.clone(),
            desired: intent.desired.clone(),
            observed: intent.expected.clone(),
            checkpoint: "captured".into(),
            ..Default::default()
        };
        fixture.write(&format!("config/release-transition/v2/transactions/{}/evidence/unlisted.ledger.captured.json", journal.transaction), &codec::encode(&evidence, true).unwrap());
        let ledger = owner.store.read(&crate::store::Address::ledger()).unwrap();
        let durable = owner.store.read(&crate::store::Address::journal()).unwrap();
        assert!(
            owner.verify_pending_artifacts(&journal, &work).is_err(),
            "unlisted evidence accepted by actual owner"
        );
        assert_eq!(
            owner.store.read(&crate::store::Address::ledger()).unwrap(),
            ledger
        );
        assert_eq!(
            owner.store.read(&crate::store::Address::journal()).unwrap(),
            durable
        );
        let folder = format!(
            "config/release-transition/v2/transactions/{}/evidence",
            journal.transaction
        );
        std::fs::remove_file(
            fixture
                .0
                .join(&folder)
                .join("unlisted.ledger.captured.json"),
        )
        .unwrap();
        let mut evidence = evidence;
        evidence.step = intent.id.clone();
        let write = |value: &crate::wire::journal_v2::Evidence| {
            fixture.write(
                &format!("{folder}/{}.{}.json", value.step, value.checkpoint),
                &codec::encode(value, true).unwrap(),
            );
        };
        write(&evidence);
        assert!(
            owner.verify_pending_artifacts(&journal, &work).is_ok(),
            "captured-sidecar-before-journal cut refused"
        );
        evidence.transaction = "foreign".into();
        write(&evidence);
        assert!(
            owner.verify_pending_artifacts(&journal, &work).is_err(),
            "foreign transaction accepted"
        );
        evidence.transaction = journal.transaction.clone();
        write(&evidence);
        journal.steps.as_mut().unwrap()[0].checkpoint = "evidence".into();
        journal.steps.as_mut().unwrap()[0].evidence = Some(evidence.clone());
        assert!(owner.verify_pending_artifacts(&journal, &work).is_ok());
        evidence.checkpoint = "applied".into();
        evidence.observed = intent.desired.clone();
        write(&evidence);
        assert!(
            owner.verify_pending_artifacts(&journal, &work).is_ok(),
            "applied-sidecar-before-journal cut refused"
        );
        evidence.checkpoint = "verified".into();
        write(&evidence);
        assert!(
            owner.verify_pending_artifacts(&journal, &work).is_err(),
            "two-ahead evidence accepted"
        );
        std::fs::remove_file(
            fixture
                .0
                .join(&folder)
                .join(format!("{}.verified.json", intent.id)),
        )
        .unwrap();
        evidence.checkpoint = "applied".into();
        journal.steps.as_mut().unwrap()[0].checkpoint = "applied".into();
        journal.steps.as_mut().unwrap()[0].evidence = Some(evidence.clone());
        evidence.checkpoint = "verified".into();
        write(&evidence);
        assert!(
            owner.verify_pending_artifacts(&journal, &work).is_ok(),
            "verified-sidecar-before-journal cut refused"
        );
        std::fs::remove_file(
            fixture
                .0
                .join(&folder)
                .join(format!("{}.captured.json", intent.id)),
        )
        .unwrap();
        assert!(
            owner.verify_pending_artifacts(&journal, &work).is_err(),
            "missing required sidecar accepted"
        );
        evidence.checkpoint = "captured".into();
        evidence.observed = intent.expected.clone();
        write(&evidence);
        assert!(owner.verify_pending_artifacts(&journal, &work).is_ok());
        let mut future = journal.steps.as_ref().unwrap()[0].clone();
        future.id = "future.ledger".into();
        future.migration = "future".into();
        future.checkpoint = "intent".into();
        future.evidence = None;
        work.push(crate::work::Work {
            intent: crate::wire::plan_v1::PlannerStepIntent {
                id: future.id.clone(),
                migration: future.migration.clone(),
                resource: future.resource.clone(),
                decision: future.decision.clone(),
                expected: future.expected.clone(),
                desired: future.desired.clone(),
            },
            recipe: crate::work::Recipe::LedgerAdvance {
                before: crate::store::Snapshot::captured(None),
                after: vec![],
            },
        });
        journal.steps.as_mut().unwrap().push(future.clone());
        assert!(owner.verify_pending_artifacts(&journal, &work).is_ok());
        evidence.step = future.id.clone();
        write(&evidence);
        assert!(
            owner.verify_pending_artifacts(&journal, &work).is_err(),
            "future prepared step artifact accepted before its turn"
        );
    }
    #[test]
    fn retained_authority_pending_recovery_publication_window() {
        let fixture = SourceFixture::new();
        let mut owner = fixture.owner();
        let catalog = migration_owner::current_catalog().unwrap();
        owner.registry = record::Registry::new(
            serde_json::from_slice(include_bytes!("../../config/release-transition.json")).unwrap(),
            &catalog,
        )
        .unwrap();
        let mut journal = owner
            .store
            .read_checked_journal()
            .unwrap()
            .unwrap()
            .0
            .wire()
            .clone();
        journal.checkpoint = "authorized".into();
        let hash = "a".repeat(64);
        let desired = "b".repeat(64);
        let work = vec![crate::work::Work {
            intent: crate::wire::plan_v1::PlannerStepIntent {
                id: "canonicalize-test-vms-settings-v2.hermes".into(),
                migration: "canonicalize-test-vms-settings-v2".into(),
                resource: "hermes".into(),
                decision: "canonicalize".into(),
                expected: hash.clone(),
                desired: desired.clone(),
            },
            recipe: Recipe::Settings {
                definition: crate::settings::migration("test-vms-settings-v1-to-v2").unwrap(),
                recipe: crate::work::SettingsRecipe {
                    yard: "hermes".into(),
                    target: "canonical".into(),
                    inheritance: hash.clone(),
                    expected: hash.clone(),
                    desired: desired.clone(),
                    expected_exists: true,
                    expected_file: hash.clone(),
                    desired_file: desired.clone(),
                    edits: vec![],
                    before: vec![crate::work::BeforeValue {
                        setting: "YARD_TEMPLATE".into(),
                        present: true,
                        value: "e2e-vms".into(),
                    }],
                    source: hash.clone(),
                    inputs: vec![],
                },
            },
        }];
        let w = &work[0].intent;
        journal.steps = Some(vec![crate::wire::journal_v2::Step {
            id: w.id.clone(),
            migration: w.migration.clone(),
            resource: w.resource.clone(),
            decision: w.decision.clone(),
            expected: w.expected.clone(),
            desired: w.desired.clone(),
            checkpoint: "intent".into(),
            ..Default::default()
        }]);
        assert!(owner.verify_pending_artifacts(&journal, &work).is_ok());
        let (recovery, _) = {
            let lease = owner.store.lock().unwrap();
            owner.recovery(&lease, &journal, 0, &work[0]).unwrap()
        };
        assert!(
            owner.verify_pending_artifacts(&journal, &work).is_ok(),
            "recovery-before-captured crash window refused"
        );
        let mut evidence = crate::wire::journal_v2::Evidence {
            schema_version: 2,
            transaction: journal.transaction.clone(),
            releases: journal.releases.clone(),
            step: w.id.clone(),
            expected: w.expected.clone(),
            desired: w.desired.clone(),
            observed: w.expected.clone(),
            checkpoint: "captured".into(),
            recovery,
        };
        let name = format!(
            "config/release-transition/v2/transactions/{}/evidence/{}.captured.json",
            journal.transaction, w.id
        );
        fixture.write(&name, &codec::encode(&evidence, true).unwrap());
        assert!(
            owner.verify_pending_artifacts(&journal, &work).is_ok(),
            "capture-before-journal with typed recovery refused"
        );
        journal.steps.as_mut().unwrap()[0].checkpoint = "evidence".into();
        journal.steps.as_mut().unwrap()[0].evidence = Some(evidence.clone());
        assert!(owner.verify_pending_artifacts(&journal, &work).is_ok());
        evidence.recovery = "c".repeat(64);
        fixture.write(&name, &codec::encode(&evidence, true).unwrap());
        assert!(
            owner.verify_pending_artifacts(&journal, &work).is_err(),
            "changed recovery binding accepted"
        );
        let path = fixture.0.join(format!(
            "config/release-transition/v2/transactions/{}/recovery/{}.json",
            journal.transaction, w.id
        ));
        std::fs::remove_file(path).unwrap();
        assert!(
            owner.verify_pending_artifacts(&journal, &work).is_err(),
            "missing typed recovery accepted"
        );
    }
    #[test]
    fn retained_authority_zero_step_requires_current_new_authority() {
        let fixture = SourceFixture::new();
        let owner = fixture.owner();
        let mut journal = owner
            .store
            .read_checked_journal()
            .unwrap()
            .unwrap()
            .0
            .wire()
            .clone();
        journal.checkpoint = "authorized".into();
        journal.steps = Some(vec![]);
        let current = owner
            .store
            .migration_ledger(&owner.registry, &owner.registry_digest)
            .unwrap();
        let facts = crate::wire::plan_v1::PlanFacts {
            observations: Some(vec![crate::wire::plan_v1::ResourceObservation {
                resource: "migration-history.authority".into(),
                class: "validated-migration-authority-v1".into(),
                fingerprint: current.authority().fingerprint(),
            }]),
            ..Default::default()
        };
        let mut bundle = crate::bundle::Bundle {
            session: owner.session.clone(),
            facts,
            helper: String::new(),
            work: vec![],
            targets: vec![],
            installed: BTreeMap::new(),
            original_checkpoint: Some(crate::store::Snapshot::captured(None)),
        };
        assert!(owner
            .bundle_scope_source(&journal, &bundle, &current)
            .is_ok());
        let approved_observations = bundle.facts.observations.clone();
        for fault in ["missing", "duplicate", "class", "hash"] {
            bundle.facts.observations = approved_observations.clone();
            let observations = bundle.facts.observations.as_mut().unwrap();
            match fault {
                "missing" => observations.clear(),
                "duplicate" => observations.push(observations[0].clone()),
                "class" => observations[0].class = "completed-ledger-v2".into(),
                "hash" => observations[0].fingerprint = "f".repeat(64),
                _ => unreachable!(),
            }
            assert!(
                owner
                    .bundle_scope_source(&journal, &bundle, &current)
                    .is_err(),
                "invalid authority proof accepted: {fault}"
            );
        }
        bundle.facts.observations = approved_observations;
        let projection =
            record::Projection::from_canonical(current.projection().bytes().unwrap().to_vec())
                .unwrap();
        let checkpoint = record::Checkpoint::build(
            current.ledger(),
            &owner.registry,
            &owner.registry_digest,
            &projection,
            None,
            true,
        )
        .unwrap();
        fixture.write(
            "config/release-transition/v2/history-checkpoint.json",
            &codec::encode(checkpoint.wire(), true).unwrap(),
        );
        let fresh = owner
            .store
            .migration_ledger(&owner.registry, &owner.registry_digest)
            .unwrap();
        assert!(
            owner
                .bundle_scope_source(&journal, &bundle, &fresh)
                .is_err(),
            "old zero-step authority adopted today's checkpoint"
        );
        bundle.original_checkpoint = Some(fresh.authority().clone());
        bundle.facts.observations.as_mut().unwrap()[0].fingerprint =
            fresh.authority().fingerprint();
        assert!(
            owner.bundle_scope_source(&journal, &bundle, &fresh).is_ok(),
            "new zero-step proof did not bind today's exact authority"
        );
        std::fs::remove_file(
            fixture
                .0
                .join("config/release-transition/v2/history-checkpoint.json"),
        )
        .unwrap();
        let removed = owner
            .store
            .migration_ledger(&owner.registry, &owner.registry_digest)
            .unwrap();
        assert!(
            owner
                .bundle_scope_source(&journal, &bundle, &removed)
                .is_err(),
            "checkpoint removal accepted"
        );
    }
    #[test]
    fn continuation_source_recheck_rejects_capture_window_drift() {
        for fault in [
            "original",
            "registry",
            "pending",
            "missing",
            "metadata",
            "foreign-root",
        ] {
            let fixture = SourceFixture::new();
            let mut owner = fixture.owner();
            let before = complete_reference(&owner).unwrap().unwrap();
            match fault {
                "registry" => fixture.write(
                    "runtime/releases/1.0.0-source/config/release-transition.json",
                    b"{}",
                ),
                "pending" => {
                    let mut j = owner
                        .store
                        .read_checked_journal()
                        .unwrap()
                        .unwrap()
                        .0
                        .wire()
                        .clone();
                    j.checkpoint = "authorized".into();
                    fixture.write(
                        "config/release-transition/v2/journal.json",
                        &codec::encode(&j, true).unwrap(),
                    );
                }
                "missing" => std::fs::remove_file(
                    fixture.0.join("config/release-transition/v2/journal.json"),
                )
                .unwrap(),
                "metadata" => fixture.write("config/release-transition/v2/ledger.json", b"{}\n"),
                "foreign-root" => owner.options.source_artifact.as_mut().unwrap().root.inode += 1,
                _ => (),
            }
            let after = complete_reference(&owner);
            if fault == "original" {
                assert_eq!(Some(before), after.unwrap());
            } else {
                assert!(after.is_err() || after.unwrap() != Some(before), "{fault}");
            }
        }
    }
    #[test]
    fn continuation_complete_reference_requires_original_canonical_terminal() {
        let original = journal("complete");
        let bytes = codec::encode(original.wire(), true).unwrap();
        let snapshot = store::Snapshot::captured(Some(bytes.clone()));
        let reference = complete_snapshot_reference(&original, &snapshot, &"b".repeat(64)).unwrap();
        assert_eq!(reference.transaction, "original-source");
        assert_eq!(reference.journal_digest, digest(&bytes));
        assert!(complete_snapshot_reference(
            &original,
            &store::Snapshot::captured(None),
            &reference.metadata_binding
        )
        .is_err());
        let mut expanded = bytes.clone();
        expanded.push(b'\n');
        assert!(complete_snapshot_reference(
            &original,
            &store::Snapshot::captured(Some(expanded)),
            &reference.metadata_binding
        )
        .is_err());
        assert!(complete_snapshot_reference(
            &journal("authorized"),
            &snapshot,
            &reference.metadata_binding
        )
        .is_err());
        assert!(complete_snapshot_reference(&original, &snapshot, "malformed").is_err());
        let mut changed = original.wire().clone();
        changed.transaction = "foreign-source".into();
        assert!(complete_snapshot_reference(
            &record::Journal::new(changed).unwrap(),
            &snapshot,
            &reference.metadata_binding
        )
        .is_err());
    }
}
