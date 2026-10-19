//! Immutable published V1 import selection and fingerprints. Native supplies
//! only the physical checkpoint observations selected by this checked record.
use crate::{
    codec, digest,
    legacy::{Historical, Kind, Reader},
    record::require,
    source_v1::Transaction,
    Error, Result,
};
use serde::{Deserialize, Serialize};
#[derive(Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "PascalCase", deny_unknown_fields)]
pub(crate) struct RuntimePair {
    pub current: String,
    pub previous: String,
}
#[derive(Clone, Serialize, Deserialize)]
#[serde(rename_all = "PascalCase", deny_unknown_fields)]
pub(crate) struct Expectation {
    pub release: String,
    pub checkpoint: String,
    pub transaction: String,
    pub runtime_pair: RuntimePair,
}
#[derive(Default, Serialize, Deserialize)]
#[serde(default, rename_all = "camelCase", deny_unknown_fields)]
struct Registry {
    schema_version: i64,
    minimum_layout: i64,
    current_layout: i64,
    migrations: Option<Vec<Definition>>,
}
impl Historical for Registry {
    const KIND: Kind = Kind::LegacyV1Registry;
}
#[derive(Default, Serialize, Deserialize)]
#[serde(default, rename_all = "camelCase", deny_unknown_fields)]
struct Definition {
    id: String,
    from_layout: i64,
    to_layout: i64,
    resources: Option<Vec<String>>,
    finalize_policy: String,
    rollback_policy: String,
    #[serde(skip_serializing_if = "crate::wire::empty_vec")]
    moves: Option<Vec<Move>>,
    #[serde(skip_serializing_if = "crate::wire::empty_vec")]
    operations: Option<Vec<Operation>>,
}
#[derive(Default, Serialize, Deserialize)]
#[serde(default, rename_all = "camelCase", deny_unknown_fields)]
struct Move {
    scope: String,
    source: String,
    destination: String,
    consumer: String,
}
#[derive(Default, Serialize, Deserialize)]
#[serde(default, rename_all = "camelCase", deny_unknown_fields)]
struct Operation {
    id: String,
    kind: String,
}
#[derive(Default, Serialize, Deserialize)]
#[serde(default, rename_all = "camelCase", deny_unknown_fields)]
struct Routes {
    schema_version: i64,
    active: bool,
    consumers: Option<Vec<Consumer>>,
}
impl Historical for Routes {
    const KIND: Kind = Kind::LegacyV1RouteConsumers;
}
#[derive(Default, Serialize, Deserialize)]
#[serde(default, rename_all = "camelCase", deny_unknown_fields)]
struct Consumer {
    project: String,
    instance: String,
    yard: String,
    mounted: bool,
}
#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct OperationIdentity<'a> {
    migration_id: &'a str,
    operation_id: &'a str,
    kind: &'a str,
    before: &'a str,
}
#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct TransactionIdentity<'a> {
    from_layout: i64,
    to_layout: i64,
    from_runtime: &'a str,
    to_release: &'a str,
    to_runtime: &'a str,
    migrations: &'a Option<Vec<String>>,
    operations: Vec<OperationIdentity<'a>>,
}
pub(crate) struct Checked {
    pub expectation: Expectation,
    pub static_digest: String,
}
fn sha(value: &impl Serialize) -> Result<String> {
    Ok(digest(&codec::encode(value, false)?))
}
pub(crate) fn check(
    reader: &Reader,
    journal: &[u8],
    registry: &[u8],
    pair: RuntimePair,
) -> Result<Checked> {
    let tx: Transaction = reader.decode(journal, false, false)?;
    let registry_digest = digest(registry);
    let registry: Registry = reader.decode(registry, false, false)?;
    require(
        tx.schema_version == 1
            && tx.from_layout == 1
            && tx.from_runtime == "releases/0.4.0-68b9925f6880"
            && tx.rollback_ops
            && tx.entries.iter().flatten().count() == 0
            && matches!(tx.phase.as_str(), "rolling-back" | "rolled-back")
            && pair.current == tx.to_runtime
            && pair.previous == tx.from_runtime,
    )?;
    let owner = "migrate-test-yard-owner";
    let broker = "refresh-test-vm-broker";
    let (current, layout, migrations) = match tx.to_release.as_str() {
        "0.4.1" => ("releases/0.4.1-fc5b03078508", 2, vec![owner]),
        "0.4.2" => ("releases/0.4.2-17608894ab09", 3, vec![owner, broker]),
        _ => return Err(Error::Invalid),
    };
    require(
        tx.to_runtime == current
            && tx.to_layout == layout
            && tx
                .migrations
                .as_deref()
                .unwrap_or_default()
                .iter()
                .map(String::as_str)
                .eq(migrations.iter().copied()),
    )?;
    let operations = tx.operations.as_deref().unwrap_or_default();
    require(operations.len() == layout as usize)?;
    let expected = [
        (owner, "test-yard-owner", "test-yard-owner-v1"),
        (
            owner,
            "test-yard-route-consumers",
            "test-yard-route-consumers-v1",
        ),
        (
            broker,
            "test-vm-broker-runtime",
            "test-vm-broker-runtime-v1",
        ),
    ];
    for (op, (migration, id, kind)) in operations.iter().zip(expected) {
        require(op.migration_id == migration && op.operation_id == id && op.kind == kind)?;
    }
    require(operations[0].before == "current")?;
    let routes: Routes = reader.decode(operations[1].before.as_bytes(), false, false)?;
    let consumers = routes.consumers.as_deref().unwrap_or_default();
    require(
        routes.schema_version == 1
            && routes.active
            && consumers.len() == 1
            && consumers[0].project == "subyard"
            && consumers[0].instance == "yard"
            && consumers[0].yard == "default"
            && !consumers[0].mounted,
    )?;
    if layout == 3 {
        require(operations[2].before == "active")?;
    }
    let phases = operations
        .iter()
        .map(|o| o.phase.as_str())
        .collect::<Vec<_>>();
    let suffix = if tx.phase == "rolled-back" {
        require(phases.iter().all(|p| *p == "rolled-back"))?;
        "terminal"
    } else if layout == 2 {
        require(phases[1] == "rolled-back")?;
        match phases[0] {
            "rolling-back" => "owner-rolling-back",
            "rolled-back" => "owner-rolled-back",
            _ => return Err(Error::Invalid),
        }
    } else {
        match phases.as_slice() {
            ["committed", "committed", "rolling-back"] => "broker-rolling-back",
            ["committed", "committed", "rolled-back"] => "broker-rolled-back",
            ["committed", "rolling-back", "rolled-back"] => "routes-rolling-back",
            ["committed", "rolled-back", "rolled-back"] => "routes-rolled-back",
            ["rolling-back", "rolled-back", "rolled-back"] => "owner-rolling-back",
            ["rolled-back", "rolled-back", "rolled-back"] => "owner-rolled-back",
            _ => return Err(Error::Invalid),
        }
    };
    require(
        registry.schema_version == 1
            && registry.minimum_layout == 1
            && registry.current_layout == layout,
    )?;
    let definitions = registry.migrations.as_deref().unwrap_or_default();
    require(definitions.len() == migrations.len())?;
    for (index, d) in definitions.iter().enumerate() {
        let start = if index == 0 { 0 } else { 2 };
        let end = if index == 0 { 2 } else { 3 };
        require(
            d.id == migrations[index]
                && d.from_layout == index as i64 + 1
                && d.to_layout == index as i64 + 2
                && d.finalize_policy == "ordered-after-active-verify"
                && d.rollback_policy == "reverse-before-runtime-swap"
                && d.moves.iter().flatten().count() == 0
                && d.resources
                    .as_deref()
                    .unwrap_or_default()
                    .iter()
                    .map(String::as_str)
                    .eq(expected[start..end].iter().map(|v| v.1))
                && d.operations
                    .iter()
                    .flatten()
                    .map(|o| (o.id.as_str(), o.kind.as_str()))
                    .eq(expected[start..end].iter().map(|v| (v.1, v.2))),
        )?;
    }
    let identity = TransactionIdentity {
        from_layout: tx.from_layout,
        to_layout: tx.to_layout,
        from_runtime: &tx.from_runtime,
        to_release: &tx.to_release,
        to_runtime: &tx.to_runtime,
        migrations: &tx.migrations,
        operations: operations
            .iter()
            .map(|o| OperationIdentity {
                migration_id: &o.migration_id,
                operation_id: &o.operation_id,
                kind: &o.kind,
                before: &o.before,
            })
            .collect(),
    };
    let transaction = format!("v1-{}", &sha(&identity)?[..32]);
    let checkpoint = format!("{}-{suffix}", if layout == 2 { "041" } else { "042" });
    #[derive(Serialize)]
    #[serde(rename_all = "camelCase")]
    struct Static<'a> {
        schema_version: u32,
        transaction: &'a str,
        runtime_pair: &'a RuntimePair,
        checkpoint: &'a str,
        journal_digest: String,
        registry_digest: String,
    }
    let static_digest = sha(&Static {
        schema_version: 1,
        transaction: &transaction,
        runtime_pair: &pair,
        checkpoint: &checkpoint,
        journal_digest: digest(journal),
        registry_digest,
    })?;
    Ok(Checked {
        expectation: Expectation {
            release: tx.to_release,
            checkpoint,
            transaction,
            runtime_pair: pair,
        },
        static_digest,
    })
}

impl Checked {
    pub(crate) fn factual_digest(&self, facts: &str) -> Result<String> {
        require(crate::record::fingerprint(facts))?;
        #[derive(Serialize)]
        #[serde(rename_all = "camelCase")]
        struct Facts<'a> {
            schema_version: u32,
            static_digest: &'a str,
            facts: &'a str,
        }
        sha(&Facts {
            schema_version: 1,
            static_digest: &self.static_digest,
            facts,
        })
    }
}

use std::os::fd::OwnedFd;
fn record(fd: OwnedFd, mode: u32) -> Result<(OwnedFd, Vec<u8>, crate::native::OriginalV1Record)> {
    use rustix::fs::{self, FileType};
    use std::os::unix::fs::FileExt;
    let stat = crate::store::io(fs::fstat(&fd))?;
    require(
        FileType::from_raw_mode(stat.st_mode) == FileType::RegularFile
            && stat.st_mode & 0o7777 == mode
            && stat.st_uid == rustix::process::getuid().as_raw()
            && stat.st_nlink == 1
            && stat.st_size > 0
            && stat.st_size <= 1 << 20,
    )?;
    let identity = crate::native::FileIdentity {
        device: stat.st_dev,
        inode: stat.st_ino,
        mode: stat.st_mode,
        uid: stat.st_uid,
        gid: stat.st_gid,
        links: stat.st_nlink as u64,
    };
    let file = std::fs::File::from(fd);
    let mut raw = vec![0; stat.st_size as usize];
    file.read_exact_at(&mut raw, 0).map_err(|_| Error::Io)?;
    let after = crate::store::io(fs::fstat(&file))?;
    require(
        stat.st_dev == after.st_dev
            && stat.st_ino == after.st_ino
            && stat.st_mode == after.st_mode
            && stat.st_uid == after.st_uid
            && stat.st_gid == after.st_gid
            && stat.st_nlink == after.st_nlink
            && stat.st_size == after.st_size
            && stat.st_mtime == after.st_mtime
            && stat.st_mtime_nsec == after.st_mtime_nsec
            && stat.st_ctime == after.st_ctime
            && stat.st_ctime_nsec == after.st_ctime_nsec,
    )?;
    let witness = crate::native::OriginalV1Record {
        identity,
        digest: digest(&raw),
    };
    Ok((file.into(), raw, witness))
}
struct Selected {
    checked: Checked,
    files: Vec<OwnedFd>,
    request: crate::native::OriginalV1,
}
fn select(owner: &crate::owner::Owner, pair: RuntimePair) -> Result<Selected> {
    use rustix::fs::{self, Mode, OFlags};
    let root = owner.store.root.as_ref().ok_or(Error::Protected)?;
    let parent =
        crate::store::directory(root, &["migrations".into(), "transactions".into()], false)?
            .ok_or(Error::Invalid)?;
    let mut candidate = None;
    for name in crate::inventory::names(&parent, 64)? {
        let dir = crate::store::directory(&parent, std::slice::from_ref(&name), false)?
            .ok_or(Error::Invalid)?;
        crate::store::linked_directory(&parent, &name, &dir)?;
        let fd = crate::store::io(fs::openat(
            &dir,
            "transaction.json",
            OFlags::RDONLY | crate::store::flags(),
            Mode::empty(),
        ))?;
        let (fd, raw, witness) = record(fd, 0o600)?;
        let tx: Transaction = owner.store.decode(&raw, false, false)?;
        if tx.phase == "committed" {
            continue;
        }
        require(candidate.is_none() && name == digest(tx.to_release.as_bytes())[..32])?;
        let released = pair
            .current
            .strip_prefix("releases/")
            .ok_or(Error::Invalid)?;
        let release_root = owner.links.artifact_root(released)?;
        let (registry_fd, registry_raw, registry) =
            record(crate::assets::original_v1_registry(&release_root)?, 0o644)?;
        let checked = check(&owner.store.reader, &raw, &registry_raw, pair.clone())?;
        crate::store::linked_directory(&parent, &name, &dir)?;
        let named = crate::store::io(fs::statat(
            &dir,
            "transaction.json",
            fs::AtFlags::SYMLINK_NOFOLLOW,
        ))?;
        require(named.st_dev == witness.identity.device && named.st_ino == witness.identity.inode)?;
        let request = crate::native::OriginalV1 {
            expectation: checked.expectation.clone(),
            journal: witness,
            registry,
        };
        candidate = Some(Selected {
            checked,
            files: vec![fd, registry_fd],
            request,
        });
    }
    candidate.ok_or(Error::Invalid)
}
impl crate::owner::Owner {
    pub(crate) fn observe_legacy_import(
        &self,
        journal: &crate::wire::journal_v2::Record,
        work: &crate::work::Work,
        static_pin: &str,
    ) -> Result<String> {
        require(
            work.intent.expected == work.intent.desired && crate::record::fingerprint(static_pin),
        )?;
        let pair = RuntimePair {
            current: format!("releases/{}", journal.releases.from),
            previous: format!(
                "releases/{}",
                journal.releases.previous.as_ref().ok_or(Error::Invalid)?
            ),
        };
        let selected = select(self, pair.clone())?;
        require(selected.checked.static_digest == static_pin)?;
        let step = journal
            .steps
            .iter()
            .flatten()
            .find(|s| s.id == work.intent.id)
            .ok_or(Error::Invalid)?;
        if step.checkpoint == "verified" {
            require(
                step.evidence
                    .as_ref()
                    .is_some_and(|e| e.recovery == static_pin),
            )?;
            return Ok(work.intent.expected.clone());
        }
        let original_journal = selected.request.journal.clone();
        let original_registry = selected.request.registry.clone();
        let actual = self
            .native(
                &crate::native::Request::ObserveLegacyCheckpoint {
                    session: self.session.clone(),
                    original_v1: selected.request,
                },
                selected.files,
            )?
            .actual;
        let next = select(self, pair)?;
        require(
            next.checked.static_digest == static_pin
                && next.request.journal == original_journal
                && next.request.registry == original_registry,
        )?;
        let actual = next.checked.factual_digest(&actual)?;
        require(actual == work.intent.expected)?;
        Ok(actual)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[derive(Deserialize)]
    #[serde(rename_all = "PascalCase")]
    struct Expected {
        release: String,
        checkpoint: String,
        transaction: String,
        runtime_pair: RuntimePair,
        journal_digest: String,
        registry_digest: String,
        static_digest: String,
        fact_digest: String,
    }
    #[derive(Deserialize)]
    struct Row {
        name: String,
        #[serde(with = "crate::wire::go_bytes")]
        journal: Option<Vec<u8>>,
        #[serde(with = "crate::wire::go_bytes")]
        registry: Option<Vec<u8>>,
        result: Expected,
    }
    #[test]
    fn actual_go_published_v1_static_and_fact_fingerprints() {
        let Ok(path) = std::env::var("SUBYARD_RUST_V1_IMPORT_CORPUS") else {
            return;
        };
        let rows: Vec<Row> = serde_json::from_slice(&std::fs::read(path).unwrap()).unwrap();
        assert_eq!(rows.len(), 10);
        let reader = Reader::Frozen(
            crate::legacy::FrozenReader::capture_helper(
                std::fs::File::open(std::env::var("SUBYARD_RUST_CODEC_HELPER").unwrap()).unwrap(),
            )
            .unwrap(),
        );
        for row in rows {
            let journal = row.journal.unwrap();
            let registry = row.registry.unwrap();
            let expected = row.result;
            let checked = check(&reader, &journal, &registry, expected.runtime_pair).unwrap();
            assert_eq!(
                checked.expectation.release, expected.release,
                "{}",
                row.name
            );
            assert_eq!(
                checked.expectation.checkpoint, expected.checkpoint,
                "{}",
                row.name
            );
            assert_eq!(
                checked.expectation.transaction, expected.transaction,
                "{}",
                row.name
            );
            assert_eq!(
                checked.static_digest, expected.static_digest,
                "{}",
                row.name
            );
            assert_eq!(digest(&journal), expected.journal_digest);
            assert_eq!(digest(&registry), expected.registry_digest);
            #[derive(Serialize)]
            #[serde(rename_all = "camelCase")]
            struct Physical<'a> {
                schema_version: u32,
                expectation: &'a Expectation,
                class: &'a str,
            }
            let physical = sha(&Physical {
                schema_version: 1,
                expectation: &checked.expectation,
                class: "supported-published-checkpoint",
            })
            .unwrap();
            assert_eq!(
                checked.factual_digest(&physical).unwrap(),
                expected.fact_digest,
                "{}",
                row.name
            );
            let mut wrong: serde_json::Value = serde_json::from_slice(&registry).unwrap();
            wrong["minimumLayout"] = 2.into();
            assert!(check(
                &reader,
                &journal,
                &serde_json::to_vec(&wrong).unwrap(),
                checked.expectation.runtime_pair.clone()
            )
            .is_err());
            let mut tx: serde_json::Value = serde_json::from_slice(&journal).unwrap();
            tx["operations"][0]["before"] = "foreign".into();
            assert!(check(
                &reader,
                &serde_json::to_vec(&tx).unwrap(),
                &registry,
                checked.expectation.runtime_pair
            )
            .is_err());
        }
    }

    #[test]
    fn actual_go_native_checkpoint_roundtrip() {
        let Ok(path) = std::env::var("SUBYARD_RUST_V1_NATIVE_FIXTURE") else {
            return;
        };
        #[derive(Deserialize)]
        struct Fixture {
            context: crate::native::Context,
            result: Expected,
        }
        let fixture: Fixture = serde_json::from_slice(&std::fs::read(path).unwrap()).unwrap();
        let config = std::fs::File::open(&fixture.context.config_home).unwrap();
        let repository = std::fs::File::open(&fixture.context.repository_root).unwrap();
        assert!(crate::native::Identity::capture(&config).unwrap() == fixture.context.root);
        assert!(
            crate::native::Identity::capture(&repository).unwrap() == fixture.context.repository
        );
        let reader = crate::legacy::FrozenReader::capture_native_helper(
            std::fs::File::open(std::env::var("SUBYARD_RUST_NATIVE_HELPER").unwrap()).unwrap(),
        )
        .unwrap();
        reader
            .native(
                &crate::native::Request::Initialize {
                    context: fixture.context.clone(),
                },
                vec![rustix::io::dup(&config).unwrap(), repository.into()],
            )
            .unwrap()
            .checked()
            .unwrap();
        reader
            .native(
                &crate::native::Request::CaptureInventory {
                    session: fixture.context.session.clone(),
                },
                vec![],
            )
            .unwrap()
            .checked()
            .unwrap();
        let ingress = reader
            .native(
                &crate::native::Request::CaptureIngress {
                    session: fixture.context.session.clone(),
                    legacy_rust: true,
                    binding: None,
                },
                vec![],
            )
            .unwrap()
            .checked()
            .unwrap()
            .ingress
            .unwrap();
        assert!(ingress.operations.is_empty() && ingress.decisions.is_empty());
        let store = crate::store::Store::pinned(config.into(), reader).unwrap();
        let name = digest(fixture.result.release.as_bytes())[..32].to_owned();
        let dir = crate::store::directory(
            store.root.as_ref().unwrap(),
            &["migrations".into(), "transactions".into(), name],
            false,
        )
        .unwrap()
        .unwrap();
        let fd = rustix::fs::openat(
            &dir,
            "transaction.json",
            rustix::fs::OFlags::RDONLY | crate::store::flags(),
            rustix::fs::Mode::empty(),
        )
        .unwrap();
        let (journal_fd, journal, journal_witness) = record(fd, 0o600).unwrap();
        let links =
            crate::links::RuntimeLinks::open(std::path::Path::new(&fixture.context.runtime_root))
                .unwrap();
        let released = fixture
            .result
            .runtime_pair
            .current
            .strip_prefix("releases/")
            .unwrap();
        let root = links.artifact_root(released).unwrap();
        let (registry_fd, registry, registry_witness) =
            record(crate::assets::original_v1_registry(&root).unwrap(), 0o644).unwrap();
        let checked = check(
            &store.reader,
            &journal,
            &registry,
            fixture.result.runtime_pair,
        )
        .unwrap();
        assert_eq!(checked.static_digest, fixture.result.static_digest);
        let crate::legacy::Reader::Frozen(reader) = &store.reader else {
            panic!("actual helper required")
        };
        let reply = reader
            .native(
                &crate::native::Request::ObserveLegacyCheckpoint {
                    session: fixture.context.session,
                    original_v1: crate::native::OriginalV1 {
                        expectation: checked.expectation.clone(),
                        journal: journal_witness,
                        registry: registry_witness,
                    },
                },
                vec![journal_fd, registry_fd],
            )
            .unwrap()
            .checked()
            .unwrap();
        assert_eq!(
            checked.factual_digest(&reply.actual).unwrap(),
            fixture.result.fact_digest
        );
        assert!(!std::path::Path::new(&fixture.context.config_home)
            .join("migrations/update.lock")
            .exists());
    }
}
