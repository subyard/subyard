//! Checked semantic values. These prove format/history validity, never consent or effects.
//!
//! ```compile_fail,E0277
//! use subyard_release_owner::record::Journal;
//! let _: Journal = serde_json::from_str("{}").unwrap();
//! ```
//! ```compile_fail,E0596
//! use subyard_release_owner::record::Journal;
//! fn change(j: &Journal) { j.wire().transaction.clear(); }
//! ```
use crate::{
    codec, digest, history,
    wire::{
        checkpoint_v1 as c, journal_v2 as j, ledger_v2 as l, process_v1 as p, registry_v2 as r,
    },
    Error, Result,
};
use serde::Serialize;
use std::collections::{BTreeMap, BTreeSet};
pub(crate) fn require(ok: bool) -> Result<()> {
    if ok {
        Ok(())
    } else {
        Err(Error::Invalid)
    }
}
pub(crate) fn safe_id(s: &str) -> bool {
    !s.is_empty()
        && s.len() <= 128
        && s != "."
        && s != ".."
        && !s.starts_with('-')
        && s.bytes()
            .all(|c| c.is_ascii_alphanumeric() || b"._-".contains(&c))
}
pub(crate) fn safe_name(s: &str) -> bool {
    s.as_bytes()
        .first()
        .is_some_and(|b| b.is_ascii_lowercase() || b.is_ascii_digit())
        && s.bytes()
            .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'_' || b == b'-')
}
pub(crate) fn fingerprint(s: &str) -> bool {
    s.len() == 64
        && s.bytes()
            .all(|c| c.is_ascii_digit() || (b'a'..=b'f').contains(&c))
}
pub(crate) fn token(s: &str, prefix: &str) -> bool {
    s.strip_prefix(prefix).is_some_and(fingerprint)
}
pub(crate) fn direction(s: &str) -> bool {
    matches!(s, "activate-target" | "activate-previous")
}
pub(crate) fn text(s: &str, max: usize, required: bool) -> bool {
    s.len() <= max
        && (!required || !s.is_empty())
        && s.trim() == s
        && !s.chars().any(char::is_control)
}
pub(crate) fn action(s: &str) -> bool {
    text(s, 512, true) && !s.contains([';', '&', '|'])
}
pub(crate) fn path(s: &str) -> bool {
    s.len() <= 4096
        && s.starts_with('/')
        && s != "/"
        && !s.ends_with('/')
        && !s.contains("//")
        && !s.split('/').any(|p| p == "." || p == "..")
        && !s.chars().any(char::is_control)
}
pub(crate) fn semver(s: &str) -> bool {
    semver::Version::parse(s).is_ok_and(|v| {
        v.to_string() == s
            && (v.pre.is_empty()
                || v.pre
                    .as_str()
                    .split('.')
                    .all(|p| !p.bytes().all(|c| c.is_ascii_digit()) || p.parse::<u64>().is_ok()))
    })
}

/// Immutable compiled-descriptor identity; this type alone advertises no implementations.
pub struct Catalog {
    descriptors: Option<Vec<r::CapabilityDescriptor>>,
    digest: String,
}
impl Catalog {
    pub fn from_frozen(reader: &crate::legacy::FrozenReader, raw: &[u8]) -> Result<Self> {
        Self::new(reader.decode(raw, false, false)?)
    }
    pub fn new(mut descriptors: Option<Vec<r::CapabilityDescriptor>>) -> Result<Self> {
        let mut seen = BTreeSet::new();
        if let Some(values) = &mut descriptors {
            values.sort_by(|a, b| a.kind.cmp(&b.kind));
            for d in values {
                require(
                    safe_id(&d.kind)
                        && safe_id(&d.domain)
                        && d.version >= 1
                        && seen.insert(d.kind.clone()),
                )?;
                require(match d.rollback_compatibility.as_str() {
                    "" => d.legacy_minimum_version.is_empty(),
                    "backward-compatible-no-op" => semver(&d.legacy_minimum_version),
                    _ => false,
                })?;
            }
        }
        let digest = digest(&codec::encode(&descriptors, false)?);
        Ok(Self {
            descriptors,
            digest,
        })
    }
    pub fn digest(&self) -> &str {
        &self.digest
    }
    pub fn descriptors(&self) -> &Option<Vec<r::CapabilityDescriptor>> {
        &self.descriptors
    }
    pub fn supports(&self, kind: &str, domain: &str) -> bool {
        self.descriptors
            .iter()
            .flatten()
            .any(|d| d.kind == kind && d.domain == domain)
    }
}
pub struct Registry {
    wire: r::RegistryV2,
    history: history::Registry,
}
impl Registry {
    pub fn from_frozen(
        reader: &crate::legacy::FrozenReader,
        raw: &[u8],
        catalog: &Catalog,
    ) -> Result<(Self, String)> {
        Ok((
            Self::new(reader.decode(raw, false, false)?, catalog)?,
            digest(raw),
        ))
    }
    pub fn new(wire: r::RegistryV2, catalog: &Catalog) -> Result<Self> {
        require(wire.schema_version == 2)?;
        let minimum = wire.minimum_epochs.clone().ok_or(Error::Invalid)?;
        let current = wire.current_epochs.clone().ok_or(Error::Invalid)?;
        let mut retired = BTreeMap::new();
        for (d, p) in wire.retired_prefixes.iter().flatten() {
            require(minimum.get(d) == Some(&p.through_epoch) && fingerprint(&p.digest))?;
            retired.insert(d.clone(), p.digest.clone());
        }
        let mut migrations = Vec::new();
        for m in wire.migrations.iter().flatten() {
            require(catalog.supports(&m.kind, &m.domain))?;
            migrations.push(history::MigrationInput {
                id: m.id.clone(),
                domain: m.domain.clone(),
                from_epoch: m.from_epoch,
                to_epoch: m.to_epoch,
                kind: m.kind.clone(),
                depends_on: m.depends_on.clone().unwrap_or_default(),
            });
        }
        let history = history::Registry::new(history::RegistryInput {
            minimum,
            current,
            migrations,
            retired,
        })
        .map_err(|_| Error::Invalid)?;
        Ok(Self { wire, history })
    }
    pub fn wire(&self) -> &r::RegistryV2 {
        &self.wire
    }
    pub fn history(&self) -> &history::Registry {
        &self.history
    }
    pub fn prefix_digest(&self, domain: &str, through: i64) -> Result<String> {
        let floor = *self
            .wire
            .minimum_epochs
            .as_ref()
            .and_then(|m| m.get(domain))
            .ok_or(Error::Invalid)?;
        let current = self.wire.current_epochs.as_ref().ok_or(Error::Invalid)?[domain];
        require(through >= floor && through <= current)?;
        #[derive(Serialize)]
        struct Seed<'a> {
            domain: &'a str,
            epoch: i64,
        }
        let mut previous = digest(&codec::encode(
            &Seed {
                domain,
                epoch: floor,
            },
            false,
        )?);
        if let Some(p) = self
            .wire
            .retired_prefixes
            .as_ref()
            .and_then(|m| m.get(domain))
        {
            previous = p.digest.clone();
        }
        #[derive(Serialize)]
        #[serde(rename_all = "camelCase")]
        struct Entry<'a> {
            domain: &'a str,
            from_epoch: i64,
            previous: &'a str,
            migration: &'a r::Migration,
        }
        for m in self
            .wire
            .migrations
            .iter()
            .flatten()
            .filter(|m| m.domain == domain && m.to_epoch <= through)
        {
            previous = digest(&codec::encode(
                &Entry {
                    domain,
                    from_epoch: m.from_epoch,
                    previous: &previous,
                    migration: m,
                },
                false,
            )?);
        }
        Ok(previous)
    }
}
pub struct Ledger {
    wire: l::LedgerV2,
    authority: history::Authority,
}
impl Ledger {
    pub(crate) fn from_authority(
        authority: &history::Authority,
        registry: &Registry,
    ) -> Result<Self> {
        let domains = authority
            .domains()
            .iter()
            .map(|(name, state)| {
                (
                    name.clone(),
                    l::DomainState {
                        epoch: state.epoch(),
                        applied: Some(state.applied().to_vec()),
                    },
                )
            })
            .collect();
        Self::new(
            l::LedgerV2 {
                schema_version: 2,
                domains: Some(domains),
            },
            registry,
        )
    }
    pub fn new(wire: l::LedgerV2, registry: &Registry) -> Result<Self> {
        require(wire.schema_version == 2)?;
        let states = wire
            .domains
            .as_ref()
            .ok_or(Error::Invalid)?
            .iter()
            .map(|(d, s)| {
                (
                    d.clone(),
                    history::StateInput {
                        epoch: s.epoch,
                        applied: s.applied.clone().unwrap_or_default(),
                    },
                )
            })
            .collect();
        let authority = registry
            .history
            .history(states)
            .map_err(|_| Error::Invalid)?;
        Ok(Self { wire, authority })
    }
    pub fn wire(&self) -> &l::LedgerV2 {
        &self.wire
    }
    pub fn authority(&self) -> &history::Authority {
        &self.authority
    }
}
pub struct Projection {
    bytes: Option<Vec<u8>>,
    epochs: BTreeMap<String, i64>,
}
impl Projection {
    pub fn absent() -> Self {
        Self {
            bytes: None,
            epochs: BTreeMap::new(),
        }
    }
    pub fn from_canonical(bytes: Vec<u8>) -> Result<Self> {
        let dto: l::LedgerV2 = codec::decode_canonical(&bytes, true)?;
        Self::from_dto(bytes, dto)
    }
    pub(crate) fn from_reader(
        bytes: Vec<u8>,
        reader: &crate::legacy::Reader,
        canonical: bool,
    ) -> Result<Self> {
        let dto = reader.decode(&bytes, canonical, true)?;
        Self::from_dto(bytes, dto)
    }
    fn from_dto(bytes: Vec<u8>, dto: l::LedgerV2) -> Result<Self> {
        require(dto.schema_version == 2)?;
        let mut epochs = BTreeMap::new();
        for (d, s) in dto.domains.ok_or(Error::Invalid)? {
            require(safe_id(&d) && s.epoch >= 1)?;
            epochs.insert(d, s.epoch);
        }
        Ok(Self {
            bytes: Some(bytes),
            epochs,
        })
    }
    pub fn bytes(&self) -> Option<&[u8]> {
        self.bytes.as_deref()
    }
}
/// A checkpoint retains the history checked under its original registry.
/// ```
/// use subyard_release_owner::record::{Checkpoint, Ledger};
/// fn view(checkpoint: &Checkpoint) -> &Ledger { checkpoint.ledger_view() }
/// ```
/// A different registry cannot be substituted when reading that view (E0061).
/// ```compile_fail,E0061
/// use subyard_release_owner::record::{Checkpoint, Registry};
/// fn substitute(checkpoint: &Checkpoint, other: &Registry) { checkpoint.ledger_view(other); }
/// ```
pub struct Checkpoint {
    wire: c::LedgerCheckpointV1,
    ledger: Ledger,
}
impl Checkpoint {
    pub(crate) fn build(
        ledger: &Ledger,
        registry: &Registry,
        registry_digest: &str,
        projection: &Projection,
        previous: Option<&Checkpoint>,
        compact: bool,
    ) -> Result<Self> {
        let mut domains = BTreeMap::new();
        for (name, state) in ledger.wire().domains.iter().flatten() {
            let through = if compact {
                state.epoch
            } else {
                previous
                    .and_then(|p| p.wire.domains.as_ref()?.get(name))
                    .map(|d| d.compacted_through)
                    .unwrap_or(
                        registry
                            .wire
                            .minimum_epochs
                            .as_ref()
                            .ok_or(Error::Invalid)?[name],
                    )
            };
            let suffix = registry
                .wire
                .migrations
                .iter()
                .flatten()
                .filter(|m| {
                    &m.domain == name && m.from_epoch >= through && m.to_epoch <= state.epoch
                })
                .map(|m| m.id.clone())
                .collect();
            domains.insert(
                name.clone(),
                c::DomainCheckpointV1 {
                    epoch: state.epoch,
                    compacted_through: through,
                    prefix_digest: registry.prefix_digest(name, through)?,
                    applied_suffix: Some(suffix),
                },
            );
        }
        let epochs = previous
            .and_then(|p| p.wire.legacy_projection.epochs.clone())
            .unwrap_or_else(|| {
                if projection.bytes.is_some() {
                    projection.epochs.clone()
                } else {
                    ledger
                        .wire
                        .domains
                        .iter()
                        .flatten()
                        .map(|(d, s)| (d.clone(), s.epoch))
                        .collect()
                }
            });
        let wire = c::LedgerCheckpointV1 {
            schema_version: 1,
            registry_digest: registry_digest.into(),
            legacy_projection: c::LedgerProjectionBindingV1 {
                exists: projection.bytes.is_some(),
                fingerprint: projection
                    .bytes
                    .as_ref()
                    .map(|p| digest(p))
                    .unwrap_or_default(),
                epochs: Some(epochs),
            },
            domains: Some(domains),
        };
        Self::new(wire, registry, registry_digest, projection)
    }
    pub fn new(
        wire: c::LedgerCheckpointV1,
        registry: &Registry,
        registry_digest: &str,
        projection: &Projection,
    ) -> Result<Self> {
        require(
            wire.schema_version == 1
                && fingerprint(&wire.registry_digest)
                && fingerprint(registry_digest),
        )?;
        let domains = wire.domains.as_ref().ok_or(Error::Invalid)?;
        let legacy = &wire.legacy_projection;
        let epochs = legacy.epochs.as_ref().ok_or(Error::Invalid)?;
        require(legacy.exists == projection.bytes.is_some())?;
        if let Some(bytes) = &projection.bytes {
            require(
                fingerprint(&legacy.fingerprint)
                    && legacy.fingerprint == digest(bytes)
                    && epochs == &projection.epochs,
            )?;
        } else {
            require(legacy.fingerprint.is_empty())?;
        }
        let minimum = registry
            .wire
            .minimum_epochs
            .as_ref()
            .ok_or(Error::Invalid)?;
        let current = registry
            .wire
            .current_epochs
            .as_ref()
            .ok_or(Error::Invalid)?;
        let retired = |d: &String| {
            registry
                .wire
                .retired_prefixes
                .as_ref()
                .is_some_and(|m| m.contains_key(d))
        };
        for (d, s) in domains {
            let floor = *minimum.get(d).ok_or(Error::Invalid)?;
            require(
                s.epoch >= floor
                    && s.epoch <= current[d]
                    && s.compacted_through >= floor
                    && s.compacted_through <= s.epoch,
            )?;
            if let Some(e) = epochs.get(d) {
                require(*e >= 1 && *e <= s.epoch)?;
            } else {
                require(floor == 1 && !retired(d))?;
            }
            require(registry.prefix_digest(d, s.compacted_through)? == s.prefix_digest)?;
            let suffix: Vec<_> = registry
                .wire
                .migrations
                .iter()
                .flatten()
                .filter(|m| {
                    &m.domain == d && m.from_epoch >= s.compacted_through && m.to_epoch <= s.epoch
                })
                .map(|m| m.id.clone())
                .collect();
            require(s.applied_suffix.as_deref().unwrap_or_default() == suffix)?;
        }
        require(epochs.keys().all(|d| domains.contains_key(d)))?;
        require(
            minimum
                .iter()
                .all(|(d, f)| domains.contains_key(d) || (*f == 1 && !retired(d))),
        )?;
        let ledger = Self::checked_ledger(&wire, registry)?;
        Ok(Self { wire, ledger })
    }
    pub fn wire(&self) -> &c::LedgerCheckpointV1 {
        &self.wire
    }
    pub fn ledger_view(&self) -> &Ledger {
        &self.ledger
    }
    fn checked_ledger(wire: &c::LedgerCheckpointV1, registry: &Registry) -> Result<Ledger> {
        let mut domains = BTreeMap::new();
        let minimum = registry
            .wire
            .minimum_epochs
            .as_ref()
            .ok_or(Error::Invalid)?;
        require(
            wire.domains
                .as_ref()
                .ok_or(Error::Invalid)?
                .keys()
                .all(|d| minimum.contains_key(d)),
        )?;
        for (domain, floor) in registry
            .wire
            .minimum_epochs
            .as_ref()
            .ok_or(Error::Invalid)?
        {
            let epoch = wire
                .domains
                .as_ref()
                .and_then(|m| m.get(domain))
                .map_or(*floor, |s| s.epoch);
            let applied = registry
                .wire
                .migrations
                .iter()
                .flatten()
                .filter(|m| &m.domain == domain && m.to_epoch <= epoch)
                .map(|m| m.id.clone())
                .collect();
            domains.insert(
                domain.clone(),
                l::DomainState {
                    epoch,
                    applied: Some(applied),
                },
            );
        }
        Ledger::new(
            l::LedgerV2 {
                schema_version: 2,
                domains: Some(domains),
            },
            registry,
        )
    }
}
pub(crate) fn pair(pair: &j::ReleasePair) -> Result<()> {
    require(
        safe_id(&pair.from)
            && safe_id(&pair.target)
            && pair
                .previous
                .as_ref()
                .is_none_or(|p| safe_id(p) && p != &pair.from),
    )
}
pub(crate) fn ingress(version: i64, kind: &str, paths: [&str; 5]) -> Result<()> {
    require(version == 1 && kind == "pre-go-source-v1" && paths.into_iter().all(path))
}
fn evidence(e: &j::Evidence) -> Result<()> {
    pair(&e.releases)?;
    require(
        e.schema_version == 2
            && safe_id(&e.transaction)
            && safe_id(&e.step)
            && [&e.expected, &e.desired, &e.observed]
                .into_iter()
                .all(|s| fingerprint(s))
            && (e.recovery.is_empty() || fingerprint(&e.recovery)),
    )?;
    require(match e.checkpoint.as_str() {
        "captured" => e.observed == e.expected,
        "applied" | "verified" => e.observed == e.desired,
        _ => false,
    })
}
pub struct Evidence {
    wire: j::Evidence,
}
impl Evidence {
    pub fn new(wire: j::Evidence) -> Result<Self> {
        evidence(&wire)?;
        Ok(Self { wire })
    }
    pub fn wire(&self) -> &j::Evidence {
        &self.wire
    }
}
pub struct Journal {
    wire: j::Record,
}
impl Journal {
    pub fn new(wire: j::Record) -> Result<Self> {
        require(
            wire.schema_version == 2
                && safe_id(&wire.transaction)
                && safe_id(&wire.goal.target)
                && direction(&wire.goal.direction)
                && wire.goal.target == wire.releases.target,
        )?;
        pair(&wire.releases)?;
        require(
            token(&wire.authorization_plan, "plan-v1-") && token(&wire.resume_plan, "resume-v1-"),
        )?;
        require(
            [
                &wire.artifact_digest,
                &wire.registry_digest,
                &wire.catalog_digest,
                &wire.observation_scope,
                &wire.authorization_digest,
                &wire.intent_digest,
            ]
            .into_iter()
            .all(|s| fingerprint(s)),
        )?;
        if let Some(s) = &wire.source_ingress {
            ingress(
                s.schema_version,
                &s.kind,
                [&s.source_root, &s.data_home, &s.bin_dir, &s.rc, &s.login_rc],
            )?;
        }
        require(matches!(
            wire.checkpoint.as_str(),
            "authorized"
                | "migrating"
                | "activation-intent"
                | "target-active"
                | "reconciling"
                | "complete"
        ))?;
        let steps = wire.steps.as_deref().unwrap_or_default();
        require(steps.len() <= 256)?;
        let mut ids = BTreeSet::new();
        for s in steps {
            require(
                safe_id(&s.id)
                    && ids.insert(&s.id)
                    && safe_id(&s.migration)
                    && safe_id(&s.resource)
                    && matches!(
                        s.decision.as_str(),
                        "preserve" | "transform" | "canonicalize" | "reset" | "quarantine"
                    )
                    && fingerprint(&s.expected)
                    && fingerprint(&s.desired),
            )?;
            if s.checkpoint == "intent" {
                require(s.evidence.is_none())?;
            } else {
                let e = s.evidence.as_ref().ok_or(Error::Invalid)?;
                evidence(e)?;
                require(
                    e.transaction == wire.transaction
                        && e.releases == wire.releases
                        && e.step == s.id
                        && e.expected == s.expected
                        && e.desired == s.desired,
                )?;
                require(matches!(
                    (s.checkpoint.as_str(), e.checkpoint.as_str()),
                    ("evidence", "captured") | ("applied", "applied") | ("verified", "verified")
                ))?;
            }
            if !matches!(wire.checkpoint.as_str(), "authorized" | "migrating") {
                require(s.checkpoint == "verified")?;
            }
        }
        require(wire.intent_digest == Self::intent_digest(&wire)?)?;
        Ok(Self { wire })
    }
    pub fn wire(&self) -> &j::Record {
        &self.wire
    }
    pub fn intent_digest(wire: &j::Record) -> Result<String> {
        #[derive(Serialize)]
        struct Step<'a> {
            id: &'a str,
            migration: &'a str,
            resource: &'a str,
            decision: &'a str,
            expected: &'a str,
            desired: &'a str,
        }
        #[derive(Serialize)]
        #[serde(rename_all = "camelCase")]
        struct Intent<'a> {
            authorization_plan: &'a str,
            resume_plan: &'a str,
            observation_scope: &'a str,
            steps: Vec<Step<'a>>,
        }
        let steps = wire
            .steps
            .iter()
            .flatten()
            .map(|s| Step {
                id: &s.id,
                migration: &s.migration,
                resource: &s.resource,
                decision: &s.decision,
                expected: &s.expected,
                desired: &s.desired,
            })
            .collect();
        Ok(digest(&codec::encode(
            &Intent {
                authorization_plan: &wire.authorization_plan,
                resume_plan: &wire.resume_plan,
                observation_scope: &wire.observation_scope,
                steps,
            },
            false,
        )?))
    }
}
pub struct Archive {
    wire: j::Archive,
}
impl Archive {
    pub fn new(wire: j::Archive) -> Result<Self> {
        require(
            wire.schema_version == 1
                && token(&wire.authorization_plan, "plan-v1-")
                && safe_id(&wire.replacement.transaction)
                && fingerprint(&wire.replacement.fingerprint)
                && wire.replacement.reason == "post-activation-scope-v0.11.1"
                && semver(&wire.replacement.source_version),
        )?;
        let journal = Journal::new(wire.journal.clone())?;
        require(
            wire.replacement.transaction == journal.wire.transaction
                && wire.replacement.fingerprint == digest(&codec::encode(journal.wire(), true)?),
        )?;
        Ok(Self { wire })
    }
    pub fn wire(&self) -> &j::Archive {
        &self.wire
    }
}
pub struct CompatibilityEvidence {
    wire: j::CompatibilityEvidence,
}
impl CompatibilityEvidence {
    pub fn new(wire: j::CompatibilityEvidence) -> Result<Self> {
        require(
            wire.schema_version == 1
                && wire.kind == "legacy-v1-import"
                && fingerprint(&wire.identity)
                && safe_id(&wire.from)
                && safe_id(&wire.previous)
                && wire.from != wire.previous,
        )?;
        Ok(Self { wire })
    }
    pub fn wire(&self) -> &j::CompatibilityEvidence {
        &self.wire
    }
}
pub(crate) fn replacement(r: &p::JournalReplacement) -> Result<()> {
    require(
        safe_id(&r.transaction)
            && fingerprint(&r.fingerprint)
            && match r.reason.as_str() {
                "pre-activation-plan-stale" => r.source_version.is_empty(),
                "post-activation-scope-v0.11.1" => semver(&r.source_version),
                _ => false,
            },
    )
}
pub struct Request {
    wire: p::Request,
}
impl Request {
    pub fn new(wire: p::Request) -> Result<Self> {
        require(
            wire.schema_version == 1
                && matches!(wire.mode.as_str(), "inspect" | "converge")
                && wire.runtime_root.starts_with('/')
                && wire.config_home.starts_with('/')
                && safe_id(&wire.target)
                && direction(&wire.direction)
                && fingerprint(&wire.artifact_digest)
                && (wire.registry_digest.is_empty() || fingerprint(&wire.registry_digest)),
        )?;
        require(
            wire.yard.is_empty()
                || wire.yard.bytes().enumerate().all(|(i, c)| {
                    c.is_ascii_lowercase() || c.is_ascii_digit() || (i > 0 && b"_-".contains(&c))
                }),
        )?;
        let ids = wire.inherited_setting_ids.as_deref().unwrap_or_default();
        require(ids.len() <= 256 && ids.iter().all(|s| safe_id(s)))?;
        require((wire.mode == "converge") == wire.execution.is_some())?;
        if let Some(s) = &wire.source_ingress {
            ingress(
                s.schema_version,
                &s.kind,
                [&s.source_root, &s.data_home, &s.bin_dir, &s.rc, &s.login_rc],
            )?;
        }
        if let Some(r) = &wire.replacement {
            replacement(r)?;
        }
        Ok(Self { wire })
    }
    pub fn wire(&self) -> &p::Request {
        &self.wire
    }
}
pub(crate) fn code(s: &str) -> bool {
    matches!(
        s,
        "ready"
            | "transition-required"
            | "recovery-pending"
            | "registry-invalid"
            | "unsupported-epoch"
            | "unsupported-kind"
            | "confirmation-required"
            | "operation-declined"
            | "plan-stale"
            | "migration-stale"
            | "resource-conflict"
            | "precondition-blocked"
            | "dependency-unavailable"
            | "verification-failed"
            | "activation-ambiguous"
            | "recovery-ambiguous"
            | "journal-invalid"
            | "rollback-incompatible"
            | "rollback-expired"
    )
}
pub(crate) fn outcome(o: &p::Outcome) -> Result<()> {
    require(
        matches!(
            o.status.as_str(),
            "ready" | "migration-required" | "recovering" | "operator-action-required"
        ) && (o.active.is_empty() || safe_id(&o.active))
            && o.previous.as_ref().is_none_or(|s| safe_id(s))
            && safe_id(&o.target)
            && code(&o.code)
            && text(&o.message, 512, true)
            && (o.retry.is_empty() || action(&o.retry))
            && o.transaction.as_ref().is_none_or(|s| safe_id(s)),
    )?;
    let warnings = o.warnings.as_deref().unwrap_or_default();
    require(
        warnings.len() <= 64
            && warnings.iter().all(|s| text(s, 512, true))
            && warnings.windows(2).all(|p| p[0] <= p[1]),
    )
}
pub struct Response {
    wire: p::Response,
}
impl Response {
    /// Frozen wire validation only; goal-dependent convergence validation belongs to the reducer.
    pub fn new(wire: p::Response) -> Result<Self> {
        require(wire.schema_version == 1 && (wire.inspection.is_some() != wire.outcome.is_some()))?;
        if let Some(o) = &wire.outcome {
            outcome(o)?;
        }
        if let Some(i) = &wire.inspection {
            require(if let Some(r) = &i.resume {
                safe_id(r) && token(&i.plan, "resume-v1-")
            } else {
                token(&i.plan, "plan-v1-")
            })?;
            let a = &i.assessment;
            require(
                a.action == "release.transition.v2"
                    && a.effect == "mutation"
                    && a.recovery == "reversible"
                    && a.impacts.as_deref()
                        == Some(&[
                            "local-metadata".into(),
                            "persistent-data".into(),
                            "yard-runtime".into(),
                        ]),
            )?;
            let consequences = a.consequences.as_deref().unwrap_or_default();
            require(
                consequences.len() <= 64
                    && consequences.iter().all(|s| text(s, 512, true))
                    && (!a.changed || !consequences.is_empty()),
            )?;
            let decisions = i.decisions.as_deref().unwrap_or_default();
            let blockers = i.blockers.as_deref().unwrap_or_default();
            require(decisions.len() <= 256 && blockers.len() <= 256)?;
            for d in decisions {
                require(
                    safe_id(&d.resource)
                        && (d.scope.is_empty() || safe_id(&d.scope))
                        && matches!(
                            d.decision.as_str(),
                            "preserve"
                                | "transform"
                                | "canonicalize"
                                | "reset"
                                | "quarantine"
                                | "block"
                        )
                        && text(&d.result, 256, false),
                )?;
            }
            for b in blockers {
                require(
                    code(&b.code)
                        && b.code != "ready"
                        && (b.resource.is_empty() || safe_id(&b.resource))
                        && text(&b.message, 512, true)
                        && action(&b.retry),
                )?;
            }
            outcome(i.outcome.as_ref().ok_or(Error::Invalid)?)?;
        }
        Ok(Self { wire })
    }
    pub fn wire(&self) -> &p::Response {
        &self.wire
    }
}
