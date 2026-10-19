#![forbid(unsafe_code)]
//! Pure release history. No authorization, persistence or runtime readiness.
//!
//! Positive control: borrowing a snapshot and consuming a semantic edge.
//! ```
//! use subyard_release_owner::history::{Registry, RegistryInput};
//! let input: RegistryInput = serde_json::from_str(r#"{"minimum":{"a":1},"current":{"a":2},"migrations":[{"id":"a2","domain":"a","fromEpoch":1,"toEpoch":2,"kind":"kind"}]}"#).unwrap();
//! let registry = Registry::new(input).unwrap();
//! let base = registry.baseline();
//! assert_eq!(base.domains()["a"].epoch(), 1);
//! let edge = registry.next(&base).unwrap();
//! assert_eq!(edge.migration().id, "a2");
//! let after = edge.into_after();
//! assert!(registry.complete(&after).is_ok());
//! ```
//! A borrowed view cannot mutate the checked frontier (E0596).
//! ```compile_fail,E0596
//! # use subyard_release_owner::history::Authority;
//! fn corrupt(a: &Authority) { a.domains().clear(); }
//! ```
//! A consumed semantic step cannot be reused (E0382).
//! ```compile_fail,E0382
//! # use subyard_release_owner::history::Edge;
//! fn twice(edge: Edge) { let _ = edge.into_after(); let _ = edge.into_after(); }
//! ```
//! Projection cannot serve as planning authority (E0308).
//! ```compile_fail,E0308
//! # use subyard_release_owner::history::{Registry, CompatibilityProjection};
//! fn plan(r: &Registry, p: &CompatibilityProjection) { let _ = r.next(p); }
//! ```
//! Checked values cannot be forged by their fields (E0451).
//! ```compile_fail,E0451
//! # use subyard_release_owner::history::CompatibilityProjection;
//! let forged = CompatibilityProjection { epochs: Default::default(), pin: String::new() };
//! ```
//! DTO deserialization does not create a checked authority (E0277).
//! ```compile_fail,E0277
//! # use subyard_release_owner::history::Authority;
//! let _: Authority = serde_json::from_str("{}").unwrap();
//! ```

use serde::{Deserialize, Serialize};
use std::{
    collections::{BTreeMap, BTreeSet},
    rc::Rc,
};

#[derive(Debug, Clone, PartialEq, Eq, Deserialize)]
#[serde(deny_unknown_fields, rename_all = "camelCase")]
pub struct MigrationInput {
    pub id: String,
    pub domain: String,
    pub from_epoch: i64,
    pub to_epoch: i64,
    pub kind: String,
    #[serde(default)]
    pub depends_on: Vec<String>,
}
#[derive(Debug, Clone, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RegistryInput {
    pub minimum: BTreeMap<String, i64>,
    pub current: BTreeMap<String, i64>,
    pub migrations: Vec<MigrationInput>,
    #[serde(default)]
    pub retired: BTreeMap<String, String>,
}
#[derive(Debug, Clone, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct StateInput {
    pub epoch: i64,
    pub applied: Vec<String>,
}
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct DomainState {
    epoch: i64,
    applied: Vec<String>,
}
impl DomainState {
    pub fn epoch(&self) -> i64 {
        self.epoch
    }
    pub fn applied(&self) -> &[String] {
        &self.applied
    }
}
#[derive(Debug, PartialEq, Eq)]
struct RegistryData {
    minimum: BTreeMap<String, i64>,
    current: BTreeMap<String, i64>,
    migrations: Vec<MigrationInput>,
    retired: BTreeMap<String, String>,
    ids: BTreeMap<String, usize>,
}
pub struct Registry {
    data: Rc<RegistryData>,
}
/// Immutable semantic snapshots may branch; they are not authorization tokens.
#[derive(Clone)]
pub struct Authority {
    registry: Rc<RegistryData>,
    domains: BTreeMap<String, DomainState>,
}
pub struct CompatibilityProjection {
    epochs: BTreeMap<String, i64>,
    pin: String,
}
/// A local semantic transition, deliberately not Clone. Consuming it proves no external effect.
pub struct Edge {
    before: Authority,
    after: Authority,
    migration: MigrationInput,
}
pub struct Trace {
    initial: Authority,
    terminal: Authority,
    edges: Vec<Edge>,
}
pub struct Completion {
    terminal: Authority,
}
#[derive(Debug, PartialEq, Eq)]
pub enum Error {
    Registry,
    Frontier,
    Foreign,
    Complete,
    Trace,
    Projection,
}
type Result<T> = std::result::Result<T, Error>;
fn safe_id(s: &str) -> bool {
    !s.is_empty()
        && s.len() <= 128
        && s != "."
        && s != ".."
        && !s.starts_with('-')
        && s.bytes()
            .all(|c| c.is_ascii_alphanumeric() || b"._-".contains(&c))
}
fn valid_pin(s: &str) -> bool {
    s.len() == 64
        && s.bytes()
            .all(|c| c.is_ascii_digit() || (b'a'..=b'f').contains(&c))
}
impl Registry {
    pub fn new(input: RegistryInput) -> Result<Self> {
        let RegistryInput {
            minimum,
            current,
            migrations,
            retired,
        } = input;
        if minimum.is_empty()
            || minimum.len() != current.len()
            || minimum
                .iter()
                .any(|(d, e)| !safe_id(d) || *e < 1 || current.get(d).is_none_or(|end| end < e))
            || retired
                .iter()
                .any(|(d, p)| !minimum.contains_key(d) || !valid_pin(p))
        {
            return Err(Error::Registry);
        }
        let mut expected = minimum.clone();
        let mut ids = BTreeMap::new();
        for (index, m) in migrations.iter().enumerate() {
            let mut seen = BTreeSet::new();
            if !safe_id(&m.id)
                || !safe_id(&m.kind)
                || ids.contains_key(&m.id)
                || expected.get(&m.domain) != Some(&m.from_epoch)
                || m.from_epoch.checked_add(1) != Some(m.to_epoch)
                || m.depends_on
                    .iter()
                    .any(|d| !safe_id(d) || !seen.insert(d) || !ids.contains_key(d))
            {
                return Err(Error::Registry);
            }
            expected.insert(m.domain.clone(), m.to_epoch);
            ids.insert(m.id.clone(), index);
        }
        if expected != current {
            return Err(Error::Registry);
        }
        Ok(Self {
            data: Rc::new(RegistryData {
                minimum,
                current,
                migrations,
                retired,
                ids,
            }),
        })
    }
    pub fn history(&self, input: BTreeMap<String, StateInput>) -> Result<Authority> {
        self.check_domains(
            input
                .into_iter()
                .map(|(d, s)| {
                    (
                        d,
                        DomainState {
                            epoch: s.epoch,
                            applied: s.applied,
                        },
                    )
                })
                .collect(),
        )
    }
    fn check_domains(&self, domains: BTreeMap<String, DomainState>) -> Result<Authority> {
        if domains.len() != self.data.minimum.len() {
            return Err(Error::Frontier);
        }
        let mut completed = BTreeSet::new();
        for (d, s) in &domains {
            let Some(floor) = self.data.minimum.get(d) else {
                return Err(Error::Frontier);
            };
            if s.epoch < *floor || s.epoch > self.data.current[d] {
                return Err(Error::Frontier);
            }
            let expected: Vec<_> = self
                .data
                .migrations
                .iter()
                .filter(|m| &m.domain == d && m.to_epoch <= s.epoch)
                .map(|m| m.id.clone())
                .collect();
            if s.applied != expected {
                return Err(Error::Frontier);
            }
            completed.extend(s.applied.iter());
        }
        if self.data.migrations.iter().any(|m| {
            completed.contains(&m.id) && m.depends_on.iter().any(|d| !completed.contains(d))
        }) {
            return Err(Error::Frontier);
        }
        Ok(Authority {
            registry: self.data.clone(),
            domains,
        })
    }
    pub fn baseline(&self) -> Authority {
        Authority {
            registry: self.data.clone(),
            domains: self
                .data
                .minimum
                .iter()
                .map(|(d, e)| {
                    (
                        d.clone(),
                        DomainState {
                            epoch: *e,
                            applied: vec![],
                        },
                    )
                })
                .collect(),
        }
    }
    fn owns(&self, a: &Authority) -> bool {
        Rc::ptr_eq(&self.data, &a.registry)
    }
    pub fn pending<'a>(&'a self, a: &Authority) -> Result<Vec<&'a MigrationInput>> {
        if !self.owns(a) {
            return Err(Error::Foreign);
        }
        Ok(self
            .data
            .migrations
            .iter()
            .filter(|m| a.domains[&m.domain].epoch < m.to_epoch)
            .collect())
    }
    pub fn next(&self, a: &Authority) -> Result<Edge> {
        self.advance(a, self.pending(a)?.first().ok_or(Error::Complete)?)
    }
    fn advance(&self, a: &Authority, m: &MigrationInput) -> Result<Edge> {
        if !self.owns(a) || a.domains[&m.domain].epoch != m.from_epoch {
            return Err(Error::Frontier);
        }
        let mut states = a.domains.clone();
        let state = states.get_mut(&m.domain).ok_or(Error::Frontier)?;
        state.epoch = m.to_epoch;
        state.applied.push(m.id.clone());
        Ok(Edge {
            before: a.clone(),
            after: self.check_domains(states)?,
            migration: m.clone(),
        })
    }
    pub fn complete(&self, a: &Authority) -> Result<Completion> {
        if !self.pending(a)?.is_empty() {
            return Err(Error::Frontier);
        }
        Ok(Completion {
            terminal: a.clone(),
        })
    }
    pub fn check_completion(&self, c: &Completion) -> Result<()> {
        self.complete(&c.terminal).map(|_| ())
    }
    pub fn check_edge(&self, e: &Edge) -> Result<()> {
        if !self.owns(&e.before) || !self.owns(&e.after) {
            return Err(Error::Foreign);
        }
        Ok(())
    }
    pub fn check_edge_binding(&self, e: &Edge) -> Result<()> {
        if self.data != e.before.registry {
            return Err(Error::Foreign);
        }
        Ok(())
    }
    pub fn trace_from(&self, observed: &Authority, ids: &[String]) -> Result<Trace> {
        if !self.owns(observed) {
            return Err(Error::Foreign);
        }
        let mut seen = BTreeSet::new();
        let mut cut = ids.len();
        for (i, id) in ids.iter().enumerate() {
            let m = self
                .data
                .ids
                .get(id)
                .and_then(|i| self.data.migrations.get(*i))
                .ok_or(Error::Trace)?;
            if !seen.insert(id) {
                return Err(Error::Trace);
            }
            if observed.domains[&m.domain].epoch < m.to_epoch {
                if cut == ids.len() {
                    cut = i;
                }
            } else if cut != ids.len() {
                return Err(Error::Trace);
            }
        }
        let mut initial = observed.clone();
        for id in ids[..cut].iter().rev() {
            let m = &self.data.migrations[self.data.ids[id]];
            let mut states = initial.domains.clone();
            let s = states.get_mut(&m.domain).ok_or(Error::Trace)?;
            if s.epoch != m.to_epoch || s.applied.last() != Some(&m.id) {
                return Err(Error::Trace);
            }
            s.epoch = m.from_epoch;
            s.applied.pop();
            initial = self.check_domains(states)?;
        }
        let mut cursor = initial.clone();
        let mut edges = vec![];
        for (i, id) in ids.iter().enumerate() {
            let e = if i < cut {
                self.advance(&cursor, &self.data.migrations[self.data.ids[id]])?
            } else {
                self.next(&cursor)?
            };
            if &e.migration.id != id {
                return Err(Error::Trace);
            }
            cursor = e.after.clone();
            edges.push(e);
        }
        Ok(Trace {
            initial,
            terminal: cursor,
            edges,
        })
    }
}
impl Authority {
    pub fn domains(&self) -> &BTreeMap<String, DomainState> {
        &self.domains
    }
}
impl Edge {
    pub fn before(&self) -> &Authority {
        &self.before
    }
    pub fn migration(&self) -> &MigrationInput {
        &self.migration
    }
    pub fn after(&self) -> &Authority {
        &self.after
    }
    pub fn into_after(self) -> Authority {
        self.after
    }
}
impl Trace {
    pub fn initial(&self) -> &Authority {
        &self.initial
    }
    pub fn terminal(&self) -> &Authority {
        &self.terminal
    }
    pub fn edges(&self) -> &[Edge] {
        &self.edges
    }
}
impl CompatibilityProjection {
    pub fn new(epochs: BTreeMap<String, i64>, pin: String) -> Result<Self> {
        if !valid_pin(&pin) || epochs.iter().any(|(d, e)| !safe_id(d) || *e < 1) {
            return Err(Error::Projection);
        }
        Ok(Self { epochs, pin })
    }
    pub fn epochs(&self) -> &BTreeMap<String, i64> {
        &self.epochs
    }
    pub fn pin(&self) -> &str {
        &self.pin
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn positive_type_controls() {
        let input = || RegistryInput {
            minimum: BTreeMap::from([("a".into(), 1)]),
            current: BTreeMap::from([("a".into(), 2)]),
            migrations: vec![MigrationInput {
                id: "a2".into(),
                domain: "a".into(),
                from_epoch: 1,
                to_epoch: 2,
                kind: "kind".into(),
                depends_on: vec![],
            }],
            retired: BTreeMap::new(),
        };
        let registry = Registry::new(input()).unwrap();
        let foreign = Registry::new(input()).unwrap();
        let base = registry.baseline();
        assert_eq!(base.domains()["a"].epoch(), 1);
        let edge = registry.next(&base).unwrap();
        assert!(registry.check_edge(&edge).is_ok());
        assert!(foreign.check_edge(&edge).is_err());
        assert!(foreign.check_edge_binding(&edge).is_ok());
        let after = edge.into_after();
        assert!(registry.complete(&after).is_ok());
        // Re-evaluation of immutable snapshots is legitimate, not authorization replay.
        assert!(registry.next(&base).is_ok());
        let projection =
            CompatibilityProjection::new(BTreeMap::from([("a".into(), 1)]), "a".repeat(64))
                .unwrap();
        assert_eq!(projection.epochs()["a"], 1);
        assert_eq!(projection.pin(), "a".repeat(64));
    }
}
