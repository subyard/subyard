//! Read-only protected source facts. These never authorize mutation or prove runtime readiness.
use crate::{
    codec, digest, inventory,
    record::{self, fingerprint, require, safe_id},
    store::{self, Address, EvidenceCheckpoint, Store},
    wire::journal_v2 as j,
    Error, Result,
};
use rustix::fs;
use serde::Serialize;
use std::collections::{BTreeMap, BTreeSet};
#[derive(Serialize, Default)]
#[allow(non_snake_case)]
struct Input {
    Present: bool,
    Fingerprint: String,
    Device: u64,
    Inode: u64,
    Links: u64,
    Mode: u32,
    UID: u32,
    GID: u32,
}
// Linux nlink_t is u64 on amd64 and u32 on aarch64. Frozen JSON uses u64.
#[allow(clippy::unnecessary_cast)]
fn input(s: fs::Stat, pin: String) -> Input {
    Input {
        Present: true,
        Fingerprint: pin,
        Device: s.st_dev,
        Inode: s.st_ino,
        Links: s.st_nlink as u64,
        Mode: s.st_mode,
        UID: s.st_uid,
        GID: s.st_gid,
    }
}
fn directory(parts: &[String]) -> bool {
    let path = parts.join("/");
    if [
        "release-transition/migration-owner",
        "release-transition/migration-owner/v1",
        "release-transition/migration-owner/v1/transactions",
        "release-transition/migration-owner/v1/completed",
        "release-transition/v2",
        "release-transition/v2/compatibility",
        "release-transition/v2/transactions",
        "release-transition/recovery",
        "release-transition/recovery/v1",
        "release-transition/recovery/v1/transactions",
        "release-transition/recovery/v2",
        "release-transition/recovery/v2/transactions",
        "release-transition/recovery/v2/archive",
        "release-transition/recovery/v2/frontiers",
    ]
    .contains(&path.as_str())
    {
        return true;
    }
    (4..=5).contains(&parts.len())
        && parts[..3] == ["release-transition", "v2", "transactions"]
        && safe_id(&parts[3])
        && (parts.len() == 4 || ["evidence", "recovery"].contains(&parts[4].as_str()))
}
fn file(parts: &[String]) -> bool {
    if parts.len() == 3 && parts[1] == "v2" {
        return ["journal.json", "ledger.json", "history-checkpoint.json"]
            .contains(&parts[2].as_str());
    }
    if parts.len() == 5 && parts[1] == "v2" && parts[2] == "transactions" {
        return parts[4] == "superseded-journal.json";
    }
    if parts.len() == 6 && parts[1] == "v2" && parts[2] == "transactions" {
        return match parts[4].as_str() {
            "evidence" => parts[5].ends_with(".json"),
            "recovery" => parts[5].strip_suffix(".json").is_some_and(safe_id),
            _ => false,
        };
    }
    if parts.len() == 4 && parts[1] == "v2" && parts[2] == "compatibility" {
        return parts[3].strip_suffix(".json").is_some_and(fingerprint);
    }
    if parts.len() == 5
        && ((parts[1] == "migration-owner"
            && parts[2] == "v1"
            && ["transactions", "completed"].contains(&parts[3].as_str()))
            || (parts[1] == "recovery"
                && ((parts[2] == "v1" && parts[3] == "transactions")
                    || (parts[2] == "v2"
                        && ["transactions", "archive", "frontiers"].contains(&parts[3].as_str())))))
    {
        return parts[4].strip_suffix(".json").is_some_and(safe_id);
    }
    false
}
/// Exact raw bytes plus inode/owner/mode inventory; no configurable exclusions.
pub struct Metadata {
    binding: String,
}
impl Metadata {
    pub fn binding(&self) -> &str {
        &self.binding
    }
}
impl Store {
    pub(crate) fn validate_owner_namespace(&self) -> Result<()> {
        if let Some(root) = &self.root {
            for (path, allowed) in [
                ("release-transition/migration-owner", vec!["v1"]),
                (
                    "release-transition/migration-owner/v1",
                    vec!["transactions", "completed"],
                ),
            ] {
                let parts = path.split('/').map(String::from).collect::<Vec<_>>();
                if let Some(parent) = store::directory(root, &parts, false)? {
                    for name in inventory::names(&parent, allowed.len())? {
                        require(allowed.contains(&name.as_str()))?;
                        let mut child = parts.clone();
                        child.push(name);
                        require(store::directory(root, &child, false)?.is_some())?;
                    }
                }
            }
        }
        Ok(())
    }
    pub fn source_metadata(&self) -> Result<Metadata> {
        self.migration_owner_receipts()?;
        self.source_metadata_inner(None, false)
    }
    pub(crate) fn source_without_owner(
        &self,
        receipt: &crate::migration_owner::Receipt,
    ) -> Result<Metadata> {
        for existing in self.migration_owner_receipts()?.values() {
            require(codec::encode(existing.wire(), true)? == codec::encode(receipt.wire(), true)?)?;
        }
        self.source_metadata_inner(Some(receipt), true)
    }
    // Initial replacement preparation may normalize only an EMPTY checked
    // owner namespace. A supplied receipt never substitutes for durable proof.
    pub(crate) fn source_before_owner(&self) -> Result<Metadata> {
        require(self.migration_owner_receipts()?.is_empty())?;
        self.source_metadata_inner(None, true)
    }
    fn source_metadata_inner(
        &self,
        exclude: Option<&crate::migration_owner::Receipt>,
        exclude_owner: bool,
    ) -> Result<Metadata> {
        let mut inputs = BTreeMap::new();
        let mut count = 8192usize;
        let mut bytes = 32u64 << 20;
        self.visit_source(
            &["release-transition".into()],
            &mut inputs,
            &mut count,
            &mut bytes,
            exclude,
            exclude_owner,
        )?;
        for path in [
            "release-transition/v2",
            "release-transition/recovery/v1/transactions",
            "release-transition/recovery/v2/transactions",
            "release-transition/recovery/v2/archive",
            "release-transition/recovery/v2/frontiers",
        ] {
            if !inputs.contains_key(path) {
                self.visit_source(
                    &path.split('/').map(String::from).collect::<Vec<_>>(),
                    &mut inputs,
                    &mut count,
                    &mut bytes,
                    exclude,
                    exclude_owner,
                )?
            }
        }
        Ok(Metadata {
            binding: digest(&codec::encode_inventory(&inputs)?),
        })
    }
    fn visit_source(
        &self,
        parts: &[String],
        inputs: &mut BTreeMap<String, Input>,
        count: &mut usize,
        bytes: &mut u64,
        exclude: Option<&crate::migration_owner::Receipt>,
        exclude_owner: bool,
    ) -> Result<()> {
        let key = parts.join("/");
        let Some(root) = &self.root else {
            inputs.insert(key, Input::default());
            return Ok(());
        };
        let Some(parent) = store::directory(root, parts, false)? else {
            inputs.insert(key, Input::default());
            return Ok(());
        };
        let mut identity = input(store::io(fs::fstat(&parent))?, String::new());
        if exclude_owner && parts.len() == 1 {
            identity.Links = 0;
        }
        inputs.insert(key, identity);
        let names = inventory::names(&parent, *count)?;
        *count = count.checked_sub(names.len()).ok_or(Error::Bound)?;
        for name in names {
            let mut child = parts.to_vec();
            child.push(name.clone());
            if exclude_owner && child == ["release-transition", "migration-owner"] {
                continue;
            }
            if let Some(receipt) = exclude {
                if child == ["release-transition", "v2", ".journal.json.pending"] {
                    let staged = store::read_at(&parent, &name, true)?;
                    require(staged.bytes().is_some_and(|p| {
                        codec::encode(&receipt.wire().successor, true)
                            .is_ok_and(|desired| desired.starts_with(p))
                    }))?;
                    require(
                        self.read(&Address::journal())?.bytes()
                            == Some(codec::encode(&receipt.wire().predecessor, true)?.as_slice()),
                    )?;
                    continue;
                }
            }
            require(!name.ends_with(".pending"))?;
            let before = store::io(fs::statat(&parent, &name, fs::AtFlags::SYMLINK_NOFOLLOW))?;
            if fs::FileType::from_raw_mode(before.st_mode) == fs::FileType::Directory {
                require(directory(&child))?;
                self.visit_source(&child, inputs, count, bytes, exclude, exclude_owner)?;
                continue;
            }
            require(file(&child))?;
            if before.st_size <= 0 || before.st_size > codec::MAX_BYTES as i64 {
                return Err(Error::Bound);
            };
            *bytes = bytes
                .checked_sub(before.st_size as u64)
                .ok_or(Error::Bound)?;
            let snapshot = store::read_at(&parent, &name, false)?;
            let payload = snapshot.bytes().ok_or(Error::Stale)?;
            if child.len() == 4 && child[1] == "v2" && child[2] == "compatibility" {
                let value = record::CompatibilityEvidence::new(self.decode(payload, true, true)?)?;
                require(format!("{}.json", value.wire().identity) == name)?
            }
            if child.len() == 6 && child[1] == "v2" && child[4] == "evidence" {
                let value = record::Evidence::new(self.decode(payload, true, true)?)?;
                let e = value.wire();
                require(
                    e.transaction == child[3]
                        && format!("{}.{}.json", e.step, e.checkpoint) == name,
                )?
            }
            let after = store::io(fs::statat(&parent, &name, fs::AtFlags::SYMLINK_NOFOLLOW))?;
            require(
                before.st_dev == after.st_dev
                    && before.st_ino == after.st_ino
                    && before.st_size == after.st_size
                    && before.st_mtime == after.st_mtime
                    && before.st_mtime_nsec == after.st_mtime_nsec
                    && before.st_mode == after.st_mode
                    && before.st_uid == after.st_uid
                    && before.st_gid == after.st_gid
                    && before.st_nlink == after.st_nlink,
            )?;
            inputs.insert(child.join("/"), input(after, snapshot.fingerprint()));
        }
        Ok(())
    }
    /// Check the exact finite transaction namespace, including every sidecar.
    pub fn terminal_artifacts(
        &self,
        journal: &record::Journal,
        registry: &record::Registry,
    ) -> Result<Artifacts> {
        self.check_artifacts(journal.wire(), registry)
    }
    pub(crate) fn check_artifacts(
        &self,
        journal: &j::Record,
        registry: &record::Registry,
    ) -> Result<Artifacts> {
        require(safe_id(&journal.transaction))?;
        let parts = vec![
            "release-transition".into(),
            "v2".into(),
            "transactions".into(),
            journal.transaction.clone(),
        ];
        let steps = journal.steps.as_deref().unwrap_or_default();
        let Some(root) = &self.root else {
            require(steps.is_empty())?;
            return Ok(Artifacts {
                transaction: journal.transaction.clone(),
            });
        };
        let Some(parent) = store::directory(root, &parts, false)? else {
            require(steps.is_empty())?;
            return Ok(Artifacts {
                transaction: journal.transaction.clone(),
            });
        };
        let names = inventory::names(&parent, 3)?;
        require(
            names
                .iter()
                .all(|n| ["evidence", "recovery", "superseded-journal.json"].contains(&n.as_str())),
        )?;
        let mut evidence = BTreeSet::new();
        let mut recovery = BTreeSet::new();
        for step in steps {
            let embedded = step.evidence.as_ref().ok_or(Error::Invalid)?;
            require(step.checkpoint == "verified")?;
            let migration = registry
                .wire()
                .migrations
                .as_ref()
                .and_then(|m| m.iter().find(|m| m.id == step.migration))
                .ok_or(Error::Invalid)?;
            if step.id == format!("{}.ledger", migration.id) {
                require(
                    step.resource == format!("ledger.{}", migration.domain)
                        && step.decision == "transform"
                        && embedded.recovery.is_empty(),
                )?
            } else {
                let supported = (migration.kind == "test-vms-settings-v1-to-v2"
                    && safe_yard(&step.resource)
                    && ["canonicalize", "reset"].contains(&step.decision.as_str()))
                    || (migration.kind == "local-ssh-port-settings-v2-to-v3"
                        && safe_yard(&step.resource)
                        && step.decision == "canonicalize")
                    || (migration.kind == "test-yard-owner-v1-to-v2"
                        && step.resource == "test-yard-owner"
                        && step.decision == "canonicalize");
                require(supported && step.id == format!("{}.{}", migration.id, step.resource))?;
                self.typed_recovery(journal, step, &migration.kind)?;
            }
            for (checkpoint, kind) in [
                ("captured", EvidenceCheckpoint::Captured),
                ("applied", EvidenceCheckpoint::Applied),
                ("verified", EvidenceCheckpoint::Verified),
            ] {
                let snapshot =
                    self.read(&Address::evidence(&journal.transaction, &step.id, kind)?)?;
                let value = record::Evidence::new(self.decode(
                    snapshot.bytes().ok_or(Error::Invalid)?,
                    true,
                    true,
                )?)?;
                let e = value.wire();
                require(
                    e.transaction == journal.transaction
                        && e.releases == journal.releases
                        && e.step == step.id
                        && e.expected == step.expected
                        && e.desired == step.desired
                        && e.checkpoint == checkpoint
                        && e.recovery == embedded.recovery
                        && &e.observed
                            == if checkpoint == "captured" {
                                &step.expected
                            } else {
                                &step.desired
                            },
                )?;
                if checkpoint == "verified" {
                    require(e == embedded)?
                }
                evidence.insert(format!("{}.{}.json", step.id, checkpoint));
            }
            if !embedded.recovery.is_empty() {
                recovery.insert(format!("{}.json", step.id));
            }
        }
        for (name, wanted) in [("evidence", evidence), ("recovery", recovery)] {
            let mut child = parts.clone();
            child.push(name.into());
            let actual = store::directory(root, &child, false)?
                .map(|fd| inventory::names(&fd, wanted.len()))
                .transpose()?
                .unwrap_or_default()
                .into_iter()
                .collect::<BTreeSet<_>>();
            require(actual == wanted)?
        }
        Ok(Artifacts {
            transaction: journal.transaction.clone(),
        })
    }
    /// Reconstruct old closed evidence without inventing an authorization record.
    pub fn retained_terminal(
        &self,
        transaction: &str,
        registry: &record::Registry,
        observed: &record::Ledger,
    ) -> Result<Retained> {
        let value = self.reconstruct_terminal(transaction, registry)?;
        terminal_bindings(&value.journal, registry, observed)?;
        Ok(value)
    }
    pub(crate) fn reconstruct_terminal(
        &self,
        transaction: &str,
        registry: &record::Registry,
    ) -> Result<Retained> {
        require(safe_id(transaction))?;
        let parts = vec![
            "release-transition".into(),
            "v2".into(),
            "transactions".into(),
            transaction.into(),
            "evidence".into(),
        ];
        let root = self.root.as_ref().ok_or(Error::Invalid)?;
        let parent = store::directory(root, &parts, false)?.ok_or(Error::Invalid)?;
        let mut verified = BTreeMap::new();
        for name in inventory::names(&parent, 8192)? {
            if let Some(id) = name.strip_suffix(".verified.json") {
                let snapshot = self.read(&Address::evidence(
                    transaction,
                    id,
                    EvidenceCheckpoint::Verified,
                )?)?;
                let value = record::Evidence::new(self.decode(
                    snapshot.bytes().ok_or(Error::Invalid)?,
                    true,
                    true,
                )?)?;
                verified.insert(id.to_string(), value);
            }
        }
        let mut journal = j::Record {
            transaction: transaction.into(),
            ..Default::default()
        };
        let mut steps = Vec::new();
        let baseline = registry.history().baseline();
        for migration in registry
            .history()
            .pending(&baseline)
            .map_err(|_| Error::Invalid)?
        {
            let ids = verified
                .keys()
                .filter(|id| id.starts_with(&format!("{}.", migration.id)))
                .cloned()
                .collect::<Vec<_>>();
            for id in ids {
                let evidence = verified.remove(&id).ok_or(Error::Invalid)?;
                let evidence = evidence.wire();
                if steps.is_empty() {
                    journal.releases = evidence.releases.clone();
                }
                let suffix = id
                    .strip_prefix(&format!("{}.", migration.id))
                    .ok_or(Error::Invalid)?;
                let (resource, decision) = if suffix == "ledger" {
                    (format!("ledger.{}", migration.domain), "transform")
                } else {
                    (suffix.into(), "canonicalize")
                };
                steps.push(j::Step {
                    id,
                    migration: migration.id.clone(),
                    resource,
                    decision: decision.into(),
                    expected: evidence.expected.clone(),
                    desired: evidence.desired.clone(),
                    checkpoint: "verified".into(),
                    evidence: Some(evidence.clone()),
                });
            }
        }
        require(!steps.is_empty() && verified.is_empty())?;
        journal.steps = Some(steps);
        self.check_artifacts(&journal, registry)?;
        Ok(Retained { journal })
    }
    pub(crate) fn owner_cleanup_recipe(
        &self,
        journal: &j::Record,
        step: &j::Step,
        before: &crate::work::OwnerBefore,
    ) -> Result<Option<(String, String)>> {
        let value = self.owner_recovery_recipe(journal, step, before)?;
        if value.is_some() {
            require(step.checkpoint == "verified" && journal.checkpoint == "complete")?;
        }
        Ok(value)
    }
    pub(crate) fn owner_progress_token(
        &self,
        journal: &j::Record,
        step: &j::Step,
        before: &crate::work::OwnerBefore,
    ) -> Result<String> {
        self.owner_recovery_recipe(journal, step, before)?
            .map(|value| value.0)
            .ok_or(Error::Invalid)
    }
    fn owner_recovery_recipe(
        &self,
        journal: &j::Record,
        step: &j::Step,
        before: &crate::work::OwnerBefore,
    ) -> Result<Option<(String, String)>> {
        let mut address = Address::superseded(&journal.transaction)?;
        address.parents.push("recovery".into());
        address.name = format!("{}.json", step.id);
        let snapshot = self.read(&address)?;
        let Some(raw) = snapshot.bytes() else {
            return Ok(None);
        };
        self.typed_recovery(journal, step, "test-yard-owner-v1-to-v2")?;
        let value: OwnerRecovery = self.decode(raw, true, true)?;
        let observed = crate::work::OwnerBefore {
            state: value.before.state,
            registration: value.before.registration,
            overrides: value.before.overrides,
            controller: value.before.controller,
            shared_images: value.before.shared_images,
            // This private payload pin belongs to the checked prepared bundle,
            // not the frozen semantic owner recovery record.
            prepared_recipe: before.prepared_recipe.clone(),
        };
        require(
            &observed == before
                && before.fingerprint()? == step.expected
                && before.desired().fingerprint()? == step.desired,
        )?;
        Ok(Some((value.token, snapshot.fingerprint())))
    }
    pub(crate) fn typed_recovery(
        &self,
        journal: &j::Record,
        step: &j::Step,
        kind: &str,
    ) -> Result<()> {
        let mut address = Address::superseded(&journal.transaction)?;
        address.parents.push("recovery".into());
        address.name = format!("{}.json", step.id);
        let snapshot = self.read(&address)?;
        let bytes = snapshot.bytes().ok_or(Error::Invalid)?;
        require(
            bytes.len() <= 4096
                && snapshot.fingerprint() == step.evidence.as_ref().ok_or(Error::Invalid)?.recovery,
        )?;
        if [
            "test-vms-settings-v1-to-v2",
            "local-ssh-port-settings-v2-to-v3",
        ]
        .contains(&kind)
        {
            let v: SettingsRecovery = self.decode(bytes, true, true)?;
            let local = v.schema_version == 2 && v.kind == "local-ssh-port-settings-v2-to-v3";
            require(
                (local || (v.schema_version == 1 && v.kind.is_empty()))
                    && local == (kind == "local-ssh-port-settings-v2-to-v3")
                    && v.transaction == journal.transaction
                    && v.step == step.id
                    && v.migration == step.migration
                    && v.yard == step.resource
                    && safe_yard(&v.yard)
                    && v.expected_fingerprint == step.expected,
            )?;
            let values = v.before.as_deref().unwrap_or_default();
            require(values.len() <= 2 && (!local || values.len() == 1))?;
            let mut seen = BTreeSet::new();
            for value in values {
                require(
                    seen.insert(&value.setting)
                        && if local {
                            value.setting == "SSH_PORT"
                        } else {
                            ["YARD_TEMPLATE", "NESTED_E2E_VMS"].contains(&value.setting.as_str())
                        },
                )?;
                require(if !value.present {
                    value.value.is_empty()
                } else {
                    match value.setting.as_str() {
                        "SSH_PORT" => value.value.parse::<u16>().is_ok_and(|p| p > 0),
                        "YARD_TEMPLATE" => ["e2e-vms", "test-vms"].contains(&value.value.as_str()),
                        "NESTED_E2E_VMS" => ["0", "1"].contains(&value.value.as_str()),
                        _ => false,
                    }
                })?
            }
        } else {
            let v: OwnerRecovery = self.decode(bytes, true, true)?;
            require(
                kind == "test-yard-owner-v1-to-v2"
                    && v.schema_version == 1
                    && v.transaction == journal.transaction
                    && v.step == step.id
                    && v.migration == step.migration
                    && v.resource == "test-yard-owner"
                    && v.expected_fingerprint == step.expected
                    && v.token.len() == 32
                    && v.token
                        .bytes()
                        .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b)),
            )?;
            require(
                [
                    "legacy-directory",
                    "legacy-directory+projects",
                    "legacy-directory+overrides",
                    "legacy-directory+projects+overrides",
                    "legacy-flat",
                    "legacy-directory-adopt-current",
                    "legacy-flat-adopt-current",
                ]
                .contains(&v.before.state.as_str())
                    && [
                        &v.before.registration,
                        &v.before.overrides,
                        &v.before.controller,
                    ]
                    .into_iter()
                    .all(|s| fingerprint(s))
                    && digest(&codec::encode(&v.before, false)?) == step.expected,
            )?
        }
        Ok(())
    }
}
/// Closed sidecar proof only; ledger authority and protected inventory are separate.
pub struct Artifacts {
    transaction: String,
}
impl Artifacts {
    pub fn transaction(&self) -> &str {
        &self.transaction
    }
}
fn safe_yard(s: &str) -> bool {
    !s.is_empty()
        && s.bytes()
            .next()
            .is_some_and(|b| b.is_ascii_lowercase() || b.is_ascii_digit())
        && s.bytes()
            .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'-' || b == b'_')
}
#[derive(serde::Serialize, serde::Deserialize, Default)]
#[serde(default, deny_unknown_fields, rename_all = "camelCase")]
struct Before {
    setting: String,
    present: bool,
    #[serde(skip_serializing_if = "String::is_empty")]
    value: String,
}
#[derive(serde::Serialize, serde::Deserialize, Default)]
#[serde(default, deny_unknown_fields, rename_all = "camelCase")]
pub(crate) struct SettingsRecovery {
    schema_version: i64,
    #[serde(skip_serializing_if = "String::is_empty")]
    kind: String,
    transaction: String,
    step: String,
    migration: String,
    yard: String,
    expected_fingerprint: String,
    before: Option<Vec<Before>>,
}
#[derive(serde::Serialize, serde::Deserialize, Default)]
#[serde(default, deny_unknown_fields, rename_all = "camelCase")]
struct OwnerBefore {
    state: String,
    registration: String,
    overrides: String,
    controller: String,
    shared_images: bool,
}
#[derive(serde::Serialize, serde::Deserialize, Default)]
#[serde(default, deny_unknown_fields, rename_all = "camelCase")]
pub(crate) struct OwnerRecovery {
    schema_version: i64,
    transaction: String,
    step: String,
    migration: String,
    resource: String,
    expected_fingerprint: String,
    token: String,
    before: OwnerBefore,
}

/// Closed evidence and historical edge bindings, never a Journal authorization.
pub struct Retained {
    pub(crate) journal: j::Record,
}
impl Retained {
    pub fn releases(&self) -> &j::ReleasePair {
        &self.journal.releases
    }
    pub fn steps(&self) -> &[j::Step] {
        self.journal.steps.as_deref().unwrap_or_default()
    }
}
fn terminal_bindings(
    journal: &j::Record,
    registry: &record::Registry,
    observed: &record::Ledger,
) -> Result<()> {
    terminal_bindings_at(journal, registry, observed, None)
}
pub(crate) fn terminal_bindings_at(
    journal: &j::Record,
    registry: &record::Registry,
    observed: &record::Ledger,
    historical: Option<&record::Ledger>,
) -> Result<()> {
    let steps = journal.steps.as_deref().unwrap_or_default();
    let h = registry.history();
    let all = registry
        .wire()
        .migrations
        .iter()
        .flatten()
        .map(|m| m.id.clone())
        .collect::<Vec<_>>();
    let selected = steps
        .iter()
        .filter(|s| s.id == format!("{}.ledger", s.migration))
        .map(|s| s.migration.clone())
        .collect::<Vec<_>>();
    let canonical = h
        .trace_from(&h.baseline(), &all)
        .map_err(|_| Error::Invalid)?;
    let encode =
        |trace: &crate::history::Trace| -> Result<BTreeMap<String, (String, String, bool)>> {
            let mut edges = BTreeMap::new();
            let mut before = trace.initial();
            for edge in trace.edges() {
                let initial = before.domains() == h.baseline().domains();
                let before_pin = digest(&crate::migration_owner::ledger_bytes(before)?);
                let after_pin = digest(&crate::migration_owner::ledger_bytes(edge.after())?);
                edges.insert(
                    edge.migration().id.clone(),
                    (before_pin, after_pin, initial),
                );
                before = edge.after();
            }
            for step in steps
                .iter()
                .filter(|s| s.id == format!("{}.ledger", s.migration))
            {
                let (before, after, initial) = edges.get(&step.migration).ok_or(Error::Invalid)?;
                require(
                    step.desired == *after
                        && (step.expected == *before
                            || (*initial
                                && step.expected == digest(b"protected-record-absent-v1"))),
                )?;
            }
            Ok(edges)
        };
    let edges = match encode(&canonical) {
        Ok(edges) => edges,
        Err(_) => {
            let last = steps
                .iter()
                .rfind(|s| s.id == format!("{}.ledger", s.migration));
            let anchor = if let Some(last) = last {
                if last.desired
                    == digest(&crate::migration_owner::ledger_bytes(observed.authority())?)
                {
                    observed
                } else {
                    let historical = historical.ok_or(Error::Invalid)?;
                    require(
                        last.desired
                            == digest(&crate::migration_owner::ledger_bytes(
                                historical.authority(),
                            )?),
                    )?;
                    historical
                }
            } else {
                observed
            };
            let trace = h
                .trace_from(anchor.authority(), &selected)
                .map_err(|_| Error::Invalid)?;
            encode(&trace)?
        }
    };
    let mut previous = None;
    for step in steps {
        let (_, after, _) = edges.get(&step.migration).ok_or(Error::Invalid)?;
        require(
            selected.contains(&step.migration)
                && step.checkpoint == "verified"
                && step.evidence.is_some(),
        )?;
        if step.id == format!("{}.ledger", step.migration) {
            require(
                step.desired == *after && previous.as_ref().is_none_or(|p| *p == step.expected),
            )?;
            previous = Some(step.desired.clone());
        }
    }
    Ok(())
}

impl crate::legacy::Historical for SettingsRecovery {
    const KIND: crate::legacy::Kind = crate::legacy::Kind::Settings;
}
impl crate::legacy::Historical for OwnerRecovery {
    const KIND: crate::legacy::Kind = crate::legacy::Kind::Owner;
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::{fs, os::unix::fs::PermissionsExt};
    #[test]
    fn owner_progress_uses_the_original_bound_recovery_before_completion() {
        let (fixture, store, journal, _) = crate::custody::tests::fixture();
        let before = crate::work::OwnerBefore {
            state: "legacy-flat".into(),
            registration: "a".repeat(64),
            overrides: "b".repeat(64),
            controller: "c".repeat(64),
            shared_images: false,
            prepared_recipe: "d".repeat(64),
        };
        let mut step = j::Step {
            id: "canonicalize-test-yard-owner-v2.test-yard-owner".into(),
            migration: "canonicalize-test-yard-owner-v2".into(),
            resource: "test-yard-owner".into(),
            expected: before.fingerprint().unwrap(),
            desired: before.desired().fingerprint().unwrap(),
            checkpoint: "evidence".into(),
            ..Default::default()
        };
        let token = "e".repeat(32);
        let recovery = OwnerRecovery {
            schema_version: 1,
            transaction: journal.transaction.clone(),
            step: step.id.clone(),
            migration: step.migration.clone(),
            resource: step.resource.clone(),
            expected_fingerprint: step.expected.clone(),
            token: token.clone(),
            before: OwnerBefore {
                state: before.state.clone(),
                registration: before.registration.clone(),
                overrides: before.overrides.clone(),
                controller: before.controller.clone(),
                shared_images: false,
            },
        };
        let raw = codec::encode(&recovery, true).unwrap();
        step.evidence = Some(j::Evidence {
            recovery: digest(&raw),
            ..Default::default()
        });
        let mut address = Address::superseded(&journal.transaction).unwrap();
        address.parents.push("recovery".into());
        address.name = format!("{}.json", step.id);
        let path = address
            .parents
            .iter()
            .fold(fixture.0.clone(), |path, part| path.join(part))
            .join(&address.name);
        fs::create_dir_all(path.parent().unwrap()).unwrap();
        let mut parent = path.parent().unwrap();
        while parent != fixture.0 {
            fs::set_permissions(parent, fs::Permissions::from_mode(0o700)).unwrap();
            parent = parent.parent().unwrap();
        }
        fs::write(&path, &raw).unwrap();
        fs::set_permissions(&path, fs::Permissions::from_mode(0o600)).unwrap();
        assert_eq!(
            store
                .owner_progress_token(&journal, &step, &before)
                .unwrap(),
            token
        );
        assert!(store
            .owner_cleanup_recipe(&journal, &step, &before)
            .is_err());
        let mut foreign = before.clone();
        foreign.controller = "f".repeat(64);
        assert!(store
            .owner_progress_token(&journal, &step, &foreign)
            .is_err());
        let mut foreign_journal = journal.clone();
        foreign_journal.transaction = "another-transaction".into();
        assert!(store
            .owner_progress_token(&foreign_journal, &step, &before)
            .is_err());
        fs::write(
            &path,
            raw.iter()
                .copied()
                .chain(std::iter::once(b' '))
                .collect::<Vec<_>>(),
        )
        .unwrap();
        assert!(store
            .owner_progress_token(&journal, &step, &before)
            .is_err());
    }
}
