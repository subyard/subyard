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
pub fn inspect(options: Options, helper: File, config: File, runtime: File) -> Result<Proof> {
    let config_path = root_path(options.config_home.as_deref().ok_or(Error::Invalid)?)?;
    let runtime_path = root_path(options.runtime_root.as_deref().ok_or(Error::Invalid)?)?;
    named_root(&config_path, &config, &options.config_identity)?;
    named_root(&runtime_path, &runtime, &options.runtime_identity)?;
    let store = Store::pinned(config.into(), legacy::FrozenReader::capture_helper(helper)?)?;
    let links = RuntimeLinks::pinned(&runtime_path, runtime.into())?;
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
        candidate(&links, &options.active, &runtime_path)?;
    let selected = match &current {
        Some((journal, _))
            if active_entries.get("config/release-transition.json")
                != Some(&journal.wire().registry_digest) =>
        {
            require(journal.wire().goal.direction == "activate-previous")?;
            let original = options.original_owner.as_ref().ok_or(Error::Invalid)?;
            require(original.release == journal.wire().releases.from)?;
            original
        }
        _ => {
            require(options.original_owner.is_none())?;
            &options.active
        }
    };
    let (owner_root, owner_inputs, entries) = if selected.release == options.active.release {
        (
            store::io(rustix::io::dup(&active_root))?,
            files.clone(),
            active_entries,
        )
    } else {
        candidate(&links, selected, &runtime_path)?
    };
    files.extend(owner_inputs);
    let registry_raw = assets::registry(&owner_root)?;
    let registry_digest = digest(&registry_raw);
    require(entries.get("config/release-transition.json") == Some(&registry_digest))?;
    let catalog = migration_owner::builtin_catalog(true, true)?;
    let registry = record::Registry::new(store.decode(&registry_raw, false, false)?, &catalog)?;
    let metadata = store.inspect_source(&registry, &registry_digest)?;
    if let Some((journal, snapshot)) = &current {
        require(
            metadata
                .journal()
                .is_some_and(|j| j.wire() == journal.wire())
                && snapshot.bytes() == Some(codec::encode(journal.wire(), true)?.as_slice()),
        )?;
        if journal.wire().checkpoint != "complete" {
            capability(&owner_root, &selected.engine_digest, &store.reader)?;
        }
    } else {
        require(metadata.journal().is_none())?;
    }
    let native_before = native_snapshot(&store)?;
    let retained = cleanup(
        &store,
        metadata.journal(),
        &registry,
        &links,
        options.original_sources.as_ref(),
        entries.contains_key("config/release-owner.json"),
    )?;
    let legacy = source_v1::binding(&store, config_path.as_os_str().as_bytes())?;
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
            && legacy == source_v1::binding(&store, config_path.as_os_str().as_bytes())?
            && metadata.binding() == store.source_metadata()?.binding()
            && native_before == native_snapshot(&store)?,
    )?;
    let (_, mut after, _) = candidate(&links, &options.active, &runtime_path)?;
    if selected.release != options.active.release {
        after.extend(candidate(&links, selected, &runtime_path)?.1);
    }
    after.extend(links.source_inputs()?);
    require(files == after)?;
    require(
        retained
            == cleanup(
                &store,
                metadata.journal(),
                &registry,
                &links,
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
