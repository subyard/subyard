//! Private native capacity and closure proofs. Frozen journals are unchanged.
use crate::{
    codec,
    custody::Command,
    digest, inventory,
    lease::UpdateLease,
    native,
    record::{self, require},
    store::{self, Address},
    Error, Result,
};
use rustix::fs::{self, AtFlags};
use serde::{Deserialize, Serialize};
use std::collections::{BTreeMap, BTreeSet};

pub(crate) const RESERVED_FILES: usize = 2570;
pub(crate) const RESERVED_BYTES: usize = 16 << 20;
const MAX_FILES: usize = 8192;
const MAX_BYTES: usize = 128 << 20;

#[derive(Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct Reservation {
    version: u8,
    pub(crate) binding: native::Binding,
    files: usize,
    bytes: usize,
}
impl Reservation {
    pub(crate) fn new(binding: &native::Binding) -> Self {
        Self {
            version: 1,
            binding: binding.clone(),
            files: RESERVED_FILES,
            bytes: RESERVED_BYTES,
        }
    }
    pub(crate) fn checked(raw: &[u8], binding: &native::Binding) -> Result<Self> {
        let value: Self = codec::decode_canonical(raw, true)?;
        require(
            value.version == 1
                && value.binding == *binding
                && value.files == RESERVED_FILES
                && value.bytes == RESERVED_BYTES,
        )?;
        binding.validate()?;
        Ok(value)
    }
}
#[derive(Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct Fact {
    name: String,
    size: usize,
    sha256: String,
}
#[derive(Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub(crate) struct Closure {
    version: u8,
    binding: native::Binding,
    intent: String,
    accepted: String,
    terminal: String,
    pub(crate) commands: Vec<Command>,
    facts: Vec<Fact>,
}
fn suffix(ordinal: u64, kind: &str) -> String {
    format!("{ordinal:03}.{kind}.json")
}
impl Closure {
    pub(crate) fn covers(&self, name: &str, raw: &[u8]) -> bool {
        self.facts
            .binary_search_by(|f| f.name.as_str().cmp(name))
            .ok()
            .is_some_and(|i| self.facts[i].size == raw.len() && self.facts[i].sha256 == digest(raw))
    }
    pub(crate) fn checked(
        raw: &[u8],
        binding: &native::Binding,
        intent: &[u8],
        accepted: &[u8],
        terminal_raw: &[u8],
        terminal: &native::Terminal,
    ) -> Result<Self> {
        Self::checked_hashes(
            raw,
            binding,
            &digest(intent),
            &digest(accepted),
            &digest(terminal_raw),
            terminal,
        )
    }
    pub(crate) fn checked_hashes(
        raw: &[u8],
        binding: &native::Binding,
        intent: &str,
        accepted: &str,
        terminal_raw: &str,
        terminal: &native::Terminal,
    ) -> Result<Self> {
        let value: Self = codec::decode_canonical(raw, true)?;
        require(
            value.version == 1
                && value.binding == *binding
                && value.intent == intent
                && value.accepted == accepted
                && value.terminal == terminal_raw
                && terminal.binding == *binding
                && matches!(terminal.state.as_str(), "complete" | "failed")
                && value.commands.len() <= 256
                && terminal.operations.len() <= 256
                && value.facts.len() == terminal.operations.len() * 3 + value.commands.len() * 2
                && value.facts.windows(2).all(|p| p[0].name < p[1].name),
        )?;
        for (n, operation) in terminal.operations.iter().enumerate() {
            let mut before = operation.clone();
            before.status = 0;
            let mut intent = before.clone();
            intent.id.clear();
            for (phase, operation) in [
                ("intent", &intent),
                ("accepted", &before),
                ("terminal", operation),
            ] {
                require(value.covers(
                    &suffix(binding.ordinal, &format!("operation-{:03}-{phase}", n + 1)),
                    &codec::encode(operation, true)?,
                ))?;
            }
        }
        for (n, command) in value.commands.iter().enumerate() {
            require(record::safe_id(&command.kind) && record::fingerprint(&command.source))?;
            for phase in ["intent", "terminal"] {
                require(value.covers(
                    &suffix(binding.ordinal, &format!("command-{:03}-{phase}", n + 1)),
                    &codec::encode(command, true)?,
                ))?;
            }
        }
        Ok(value)
    }
}

impl store::Store {
    fn checked_native_closure(&self, tx: &str, ordinal: u64) -> Result<Closure> {
        let read = |kind| self.read(&Address::native(tx, ordinal, kind)?);
        let intent = read("intent")?;
        let accepted = read("accepted")?;
        let terminal_raw = read("terminal")?;
        let closure = read("closure")?;
        let terminal: native::Terminal =
            codec::decode_canonical(terminal_raw.bytes().ok_or(Error::Unknown)?, true)?;
        require(terminal.binding.transaction == tx && terminal.binding.ordinal == ordinal)?;
        Closure::checked(
            closure.bytes().ok_or(Error::Unknown)?,
            &terminal.binding,
            intent.bytes().ok_or(Error::Unknown)?,
            accepted.bytes().ok_or(Error::Unknown)?,
            terminal_raw.bytes().ok_or(Error::Unknown)?,
            &terminal,
        )
    }
}

impl store::Store {
    pub(crate) fn native_usage(
        &self,
        binding: Option<&native::Binding>,
    ) -> Result<(usize, usize, BTreeSet<String>)> {
        require(!self.has_native_retirement()?)?;
        let root = self.root.as_ref().ok_or(Error::Protected)?;
        let parent = store::directory(
            root,
            &["migrations".into(), "native-custody".into(), "v1".into()],
            false,
        )?;
        let (mut total_files, mut total_bytes) = (0usize, self.payload_usage()?.metadata);
        let mut transactions = BTreeSet::new();
        let recovery_closed = self.native_closed_capacity(binding);
        let mut reserved = false;
        if let Some(parent) = parent {
            for tx in inventory::names(&parent, 256)? {
                require(record::safe_id(&tx))?;
                transactions.insert(tx.clone());
                let dir = store::directory(&parent, std::slice::from_ref(&tx), false)?
                    .ok_or(Error::Stale)?;
                let mut by_invocation: BTreeMap<u64, (usize, usize, Option<Reservation>, bool)> =
                    BTreeMap::new();
                for name in inventory::names(&dir, MAX_FILES)? {
                    let snapshot = store::read_at(&dir, &name, true)?;
                    let bytes = snapshot.bytes().ok_or(Error::Stale)?;
                    let logical = name
                        .strip_prefix('.')
                        .and_then(|n| n.strip_suffix(".pending"))
                        .unwrap_or(&name);
                    let invocation = logical
                        .split_once('.')
                        .and_then(|(n, _)| n.parse::<u64>().ok());
                    if let Some(ordinal) = invocation {
                        let group = by_invocation.entry(ordinal).or_default();
                        group.0 = group.0.checked_add(1).ok_or(Error::Bound)?;
                        group.1 = group.1.checked_add(bytes.len()).ok_or(Error::Bound)?;
                        if logical == suffix(ordinal, "reservation") && name == logical {
                            let value: Reservation = codec::decode_canonical(bytes, true)?;
                            Reservation::checked(bytes, &value.binding)?;
                            require(
                                value.binding.transaction == tx && value.binding.ordinal == ordinal,
                            )?;
                            group.2 = Some(value);
                        }
                        if (logical == suffix(ordinal, "closure")
                            || recovery_closed && logical == suffix(ordinal, "recovery"))
                            && name == logical
                        {
                            // A closure releases capacity only after full semantic validation.
                            group.3 = true;
                        }
                    } else {
                        total_files = total_files.checked_add(1).ok_or(Error::Bound)?;
                        total_bytes = total_bytes.checked_add(bytes.len()).ok_or(Error::Bound)?;
                    }
                }
                for (ordinal, (files, bytes, reservation, closed)) in by_invocation {
                    if closed
                        && self
                            .read(&Address::native(&tx, ordinal, "closure")?)?
                            .bytes()
                            .is_some()
                    {
                        self.checked_native_closure(&tx, ordinal)?;
                    }
                    let same = binding.is_some_and(|b| tx == b.transaction && ordinal == b.ordinal);
                    reserved |= same && reservation.is_some();
                    let active = reservation.is_some() && !closed || same && !closed;
                    total_files = total_files
                        .checked_add(if active {
                            files.max(RESERVED_FILES)
                        } else {
                            files
                        })
                        .ok_or(Error::Bound)?;
                    total_bytes = total_bytes
                        .checked_add(if active {
                            bytes.max(RESERVED_BYTES)
                        } else {
                            bytes
                        })
                        .ok_or(Error::Bound)?;
                    if same {
                        reserved = true;
                    }
                }
            }
        }
        if binding.is_some() && !reserved {
            total_files = total_files
                .checked_add(RESERVED_FILES)
                .ok_or(Error::Bound)?;
            total_bytes = total_bytes
                .checked_add(RESERVED_BYTES)
                .ok_or(Error::Bound)?;
        }
        Ok((total_files, total_bytes, transactions))
    }
}

impl UpdateLease<'_> {
    /// Reserve worst-case published + pending helper facts before custody transfer.
    /// Completed closures release the unused reservation without changing its bytes.
    pub(crate) fn reserve_native(&self, binding: &native::Binding) -> Result<()> {
        binding.validate()?;
        let address = Address::native(&binding.transaction, binding.ordinal, "reservation")?;
        let raw = codec::encode(&Reservation::new(binding), true)?;
        if let Some(prior) = self.store.read(&address)?.bytes() {
            Reservation::checked(prior, binding)?;
        }
        let (files, bytes, mut transactions) = self.store.native_usage(Some(binding))?;
        transactions.insert(binding.transaction.clone());
        require(transactions.len() <= 256)?;
        if files > MAX_FILES || bytes > MAX_BYTES {
            return Err(Error::Bound);
        }
        self.create_immutable(&address, &raw)
    }

    pub(crate) fn native_bundle_capacity(
        &self,
        transaction: &str,
        lengths: &[usize],
    ) -> Result<()> {
        require(record::safe_id(transaction) && !lengths.is_empty() && lengths.len() <= 129)?;
        let (mut files, mut bytes, mut transactions) = self.store.native_usage(None)?;
        transactions.insert(transaction.into());
        require(transactions.len() <= 256)?;
        let mut pending = 0;
        for (n, length) in lengths.iter().enumerate() {
            let name = if n + 1 == lengths.len() {
                "bundle.json".into()
            } else {
                format!("bundle.{n:03}.json")
            };
            let address = Address::at(
                vec![
                    "migrations".into(),
                    "native-custody".into(),
                    "v1".into(),
                    transaction.into(),
                ],
                name,
                true,
            );
            if self.store.read(&address)?.bytes().is_none() {
                files = files.checked_add(1).ok_or(Error::Bound)?;
                bytes = bytes.checked_add(*length).ok_or(Error::Bound)?;
                pending = pending.max(*length);
            }
        }
        // Publication is sequential: at most one additional piece is staged.
        if pending != 0 {
            files = files.checked_add(1).ok_or(Error::Bound)?;
        }
        bytes = bytes.checked_add(pending).ok_or(Error::Bound)?;
        if files > MAX_FILES || bytes > MAX_BYTES {
            return Err(Error::Bound);
        }
        Ok(())
    }

    /// Called only after the retained helper acknowledged close-only Release.
    /// Publish the checked closure first; deletion is exact and restartable.
    pub(crate) fn close_native(&self, binding: &native::Binding) -> Result<()> {
        self.store
            .validate_native_transaction(&binding.transaction)?;
        let top = |kind| {
            self.store.read(&Address::native(
                &binding.transaction,
                binding.ordinal,
                kind,
            )?)
        };
        let intent = top("intent")?;
        let accepted = top("accepted")?;
        let terminal_raw = top("terminal")?;
        let terminal: native::Terminal =
            codec::decode_canonical(terminal_raw.bytes().ok_or(Error::Unknown)?, true)?;
        let root = self.store.root.as_ref().ok_or(Error::Protected)?;
        let directory = store::directory(
            root,
            &[
                "migrations".into(),
                "native-custody".into(),
                "v1".into(),
                binding.transaction.clone(),
            ],
            false,
        )?
        .ok_or(Error::Stale)?;
        let address = Address::native(&binding.transaction, binding.ordinal, "closure")?;
        let existing = self.store.read(&address)?;
        let closure = if let Some(raw) = existing.bytes() {
            Closure::checked(
                raw,
                binding,
                intent.bytes().ok_or(Error::Unknown)?,
                accepted.bytes().ok_or(Error::Unknown)?,
                terminal_raw.bytes().ok_or(Error::Unknown)?,
                &terminal,
            )?
        } else {
            let mut facts = vec![];
            let mut commands = vec![];
            let prefix = format!("{:03}.", binding.ordinal);
            for name in inventory::names(&directory, MAX_FILES)? {
                let nested = name
                    .strip_prefix(&prefix)
                    .is_some_and(|n| n.starts_with("operation-") || n.starts_with("command-"));
                if !nested {
                    continue;
                }
                let snapshot = store::read_at(&directory, &name, false)?;
                let raw = snapshot.bytes().ok_or(Error::Stale)?;
                if name.contains(".command-") && name.ends_with("-intent.json") {
                    commands.push(codec::decode_canonical(raw, true)?);
                }
                facts.push(Fact {
                    name,
                    size: raw.len(),
                    sha256: digest(raw),
                });
            }
            let closure = Closure {
                version: 1,
                binding: binding.clone(),
                intent: intent.fingerprint(),
                accepted: accepted.fingerprint(),
                terminal: terminal_raw.fingerprint(),
                commands,
                facts,
            };
            let raw = codec::encode(&closure, true)?;
            let checked = Closure::checked(
                &raw,
                binding,
                intent.bytes().ok_or(Error::Unknown)?,
                accepted.bytes().ok_or(Error::Unknown)?,
                terminal_raw.bytes().ok_or(Error::Unknown)?,
                &terminal,
            )?;
            self.create_immutable(&address, &raw)?;
            self.store.fault("after-native-closure");
            checked
        };
        for fact in &closure.facts {
            let current = store::read_at(&directory, &fact.name, false)?;
            if let Some(raw) = current.bytes() {
                require(raw.len() == fact.size && digest(raw) == fact.sha256)?;
                store::io(fs::unlinkat(&directory, &fact.name, AtFlags::empty()))?;
                store::io(fs::fsync(&directory))?;
                self.store.fault("after-native-fact-unlink");
            }
        }
        self.store.validate_native_transaction(&binding.transaction)
    }
}
