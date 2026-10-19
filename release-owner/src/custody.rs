//! Bounded sibling namespace, separate from all frozen release record grammars.
//! Only a matching checked terminal fact proves an invocation drained; a failed
//! fact proves quiescence without satisfying success or Ready.
use crate::{
    codec, inventory, native,
    owner::Owner,
    record::{self, require},
    store,
    wire::journal_v2 as j,
    Error, Result,
};
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;
#[derive(Deserialize, Serialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub(crate) struct Intent {
    pub(crate) version: i64,
    pub(crate) binding: native::Binding,
    pub(crate) owner: String,
    pub(crate) resource: String,
    pub(crate) expected: String,
    pub(crate) desired: String,
    pub(crate) source: String,
    pub(crate) recipe: String,
}
#[derive(Deserialize, Serialize, PartialEq, Eq)]
#[serde(deny_unknown_fields)]
pub(crate) struct Command {
    pub(crate) kind: String,
    pub(crate) source: String,
}
#[derive(Clone, Copy)]
pub(crate) enum CleanupRead<'a> {
    Inspection(&'a str),
    Quiescent(&'a str),
}
impl<'a> CleanupRead<'a> {
    fn recipe(self) -> &'a str {
        match self {
            Self::Inspection(recipe) | Self::Quiescent(recipe) => recipe,
        }
    }
}
impl crate::lease::UpdateLease<'_> {
    pub(crate) fn cleanup_native_quiescent(&self, current: &j::Record, recipe: &str) -> Result<()> {
        self.store
            .native_inventory_for(
                Some(current),
                true,
                None,
                None,
                false,
                true,
                Some(CleanupRead::Quiescent(recipe)),
            )
            .map(|_| ())
    }
}
impl Owner {
    pub(crate) fn native_quiescent(&self, current: Option<&j::Record>) -> Result<()> {
        self.store.native_inventory(current, false)
    }
    pub(crate) fn native_ready(&self, current: &j::Record) -> Result<()> {
        self.store.native_inventory(Some(current), true)
    }
}
impl store::Store {
    pub(crate) fn validate_native_transaction(&self, transaction: &str) -> Result<()> {
        self.native_inventory_for(None, false, Some(transaction), None, true, true, None)
            .map(|_| ())
    }
    pub(crate) fn native_retireable_transaction(&self, transaction: &str) -> Result<()> {
        self.native_inventory_for(None, false, Some(transaction), None, false, false, None)
            .map(|_| ())
    }
    pub(crate) fn native_before_dispatch(&self, binding: &native::Binding) -> Result<()> {
        self.native_inventory_for(None, false, None, Some(binding), true, true, None)
            .map(|_| ())
    }
    pub(crate) fn native_closed_capacity(&self, binding: Option<&native::Binding>) -> bool {
        self.native_inventory_for(None, false, None, binding, false, true, None)
            .is_ok()
    }
    pub(crate) fn native_inventory(
        &self,
        current: Option<&j::Record>,
        successful: bool,
    ) -> Result<()> {
        self.native_inventory_for(
            current,
            successful,
            None,
            None,
            true,
            current.is_some(),
            None,
        )
        .map(|_| ())
    }
    pub(crate) fn native_cleanup_pending(&self) -> Result<bool> {
        self.native_inventory_for(None, false, None, None, false, true, None)
    }
    pub(crate) fn native_cleanup_admission(
        &self,
        current: &j::Record,
        recipe: &str,
    ) -> Result<bool> {
        self.native_inventory_for(
            Some(current),
            true,
            None,
            None,
            false,
            true,
            Some(CleanupRead::Inspection(recipe)),
        )
    }
    pub(crate) fn required_cleanup_pending(
        &self,
        current: &j::Record,
        recipe: &str,
        ordinal: u64,
    ) -> Result<bool> {
        require(ordinal > 0 && ordinal <= 2049)?;
        let pending = self.native_cleanup_admission(current, recipe)?;
        let slot = self.read(&store::Address::native(
            &current.transaction,
            ordinal,
            "intent",
        )?)?;
        let Some(raw) = slot.bytes() else {
            // The checked original bundle requires the slot; no invocation
            // has completed it yet. This is admission, never quiescence.
            return Ok(true);
        };
        let intent: Intent = codec::decode_canonical(raw, true)?;
        require(
            intent.binding.ordinal == ordinal
                && intent.recipe == recipe
                && local_cleanup(current, &intent),
        )?;
        Ok(pending)
    }
    pub(crate) fn completed_cleanup_without_recovery(
        &self,
        current: &j::Record,
        ordinal: u64,
    ) -> Result<()> {
        let slot = self.read(&store::Address::native(
            &current.transaction,
            ordinal,
            "intent",
        )?)?;
        let intent: Intent = codec::decode_canonical(slot.bytes().ok_or(Error::Invalid)?, true)?;
        // Recovery GC may remove the token only after this exact local slot
        // closed. The original step's recovery fingerprint remains in intent.
        require(!self.required_cleanup_pending(current, &intent.recipe, ordinal)?)
    }
    #[allow(clippy::too_many_arguments)]
    fn native_inventory_for(
        &self,
        current: Option<&j::Record>,
        successful: bool,
        only: Option<&str>,
        admitting: Option<&native::Binding>,
        permit_fence_dispatch: bool,
        permit_cleanup_pending: bool,
        cleanup_admission: Option<CleanupRead<'_>>,
    ) -> Result<bool> {
        let Some(root) = &self.root else {
            return Ok(false);
        };
        let parts = ["migrations".into(), "native-custody".into(), "v1".into()];
        let Some(parent) = store::directory(root, &parts, false)? else {
            return Ok(false);
        };
        let retiring = self.native_retirement()?;
        let transactions = inventory::names(&parent, 256)?;
        let mut count = 0usize;
        let mut bytes = self.payload_usage()?.metadata;
        let mut cleanup_pending = false;
        for tx in transactions {
            require(record::safe_id(&tx))?;
            if retiring.as_deref() == Some(&tx) {
                continue;
            }
            if only.is_some_and(|t| t != tx) {
                continue;
            }
            let dir =
                store::directory(&parent, std::slice::from_ref(&tx), false)?.ok_or(Error::Stale)?;
            let names = inventory::names(&dir, 8192 - count)?;
            count = count.checked_add(names.len()).ok_or(Error::Bound)?;
            require(count <= 8192)?;
            let files = names.into_iter().map(|name| {
                let snapshot = store::read_at(&dir, &name, true)?;
                let raw = snapshot.bytes().ok_or(Error::Stale)?;
                bytes = bytes.checked_add(raw.len()).ok_or(Error::Bound)?;
                require(bytes <= 128 << 20)?;
                Ok((name, raw.to_vec()))
            });
            let pending_cleanup = validate_files(
                &tx,
                files,
                current,
                successful,
                admitting,
                permit_fence_dispatch,
                cleanup_admission,
            )?;
            require(!pending_cleanup || permit_cleanup_pending)?;
            if cleanup_admission.is_some() && pending_cleanup {
                require(current.is_some_and(|j| j.transaction == tx))?;
            }
            cleanup_pending |= pending_cleanup;
        }
        Ok(cleanup_pending)
    }
}

pub(crate) fn validate_files(
    tx: &str,
    files: impl IntoIterator<Item = Result<(String, Vec<u8>)>>,
    current: Option<&j::Record>,
    successful: bool,
    admitting: Option<&native::Binding>,
    permit_fence_dispatch: bool,
    cleanup_admission: Option<CleanupRead<'_>>,
) -> Result<bool> {
    let mut unfinished_cleanup = false;
    let mut operations = BTreeMap::new();
    let mut commands = BTreeMap::new();
    let mut intents = BTreeMap::new();
    let mut terminals = BTreeMap::new();
    let mut accepted = BTreeMap::new();
    let mut reservations = BTreeMap::new();
    let mut closure_bytes = BTreeMap::new();
    let mut recovery_bytes = BTreeMap::new();
    let mut top_bytes = BTreeMap::new();
    for file in files {
        let (name, raw) = file?;
        let raw = raw.as_slice();
        let pending = name.starts_with('.') && name.ends_with(".pending");
        let logical = if pending {
            &name[1..name.len() - 8]
        } else {
            &name
        };
        if logical == "bundle.json"
            || logical
                .strip_prefix("bundle.")
                .and_then(|n| n.strip_suffix(".json"))
                .is_some_and(|n| n.len() == 3 && n.bytes().all(|c| c.is_ascii_digit()))
        {
            continue;
        }
        let (ordinal, kind) = logical.split_once('.').ok_or(Error::Protected)?;
        let ordinal: u64 = ordinal.parse().map_err(|_| Error::Protected)?;
        require(ordinal > 0 && ordinal <= 4096 && logical == format!("{ordinal:03}.{kind}"))?;
        // Incomplete native facts cannot be interpreted as terminal. They may be
        // retried only by the original exact publication; no admission hides them.
        if pending {
            return Err(Error::Unknown);
        }
        if let Some(command) = kind.strip_prefix("command-") {
            let (n, phase) = command.split_once('-').ok_or(Error::Protected)?;
            let n: usize = n.parse().map_err(|_| Error::Protected)?;
            require(
                n > 0
                    && n <= 256
                    && matches!(phase, "intent.json" | "terminal.json")
                    && kind == format!("command-{n:03}-{phase}"),
            )?;
            let value: Command = codec::decode_canonical(raw, true)?;
            require(record::safe_id(&value.kind) && record::fingerprint(&value.source))?;
            require(
                commands
                    .insert((ordinal, n, phase.to_string()), value)
                    .is_none(),
            )?;
            continue;
        }
        if let Some(op) = kind.strip_prefix("operation-") {
            let (n, phase) = op.split_once('-').ok_or(Error::Protected)?;
            let n: usize = n.parse().map_err(|_| Error::Protected)?;
            require(
                n > 0
                    && n <= 256
                    && matches!(phase, "intent.json" | "accepted.json" | "terminal.json")
                    && kind == format!("operation-{n:03}-{phase}"),
            )?;
            let value: native::Operation = codec::decode_canonical(raw, true)?;
            require(
                record::fingerprint(&value.server)
                    && record::safe_name(&value.project)
                    && record::safe_name(&value.resource)
                    && record::safe_id(&value.kind)
                    && if phase == "intent.json" {
                        value.id.is_empty() && value.status == 0
                    } else {
                        record::safe_id(&value.id)
                            && if phase == "accepted.json" {
                                value.status == 0
                            } else {
                                matches!(value.status, 200 | 400 | 401)
                            }
                    },
            )?;
            require(
                operations
                    .insert((ordinal, n, phase.to_string()), value)
                    .is_none(),
            )?;
            continue;
        }
        match kind {
            "reservation.json" => {
                let reservation: crate::native_retention::Reservation =
                    codec::decode_canonical(raw, true)?;
                crate::native_retention::Reservation::checked(raw, &reservation.binding)?;
                require(
                    reservation.binding.transaction == tx && reservation.binding.ordinal == ordinal,
                )?;
                require(reservations.insert(ordinal, raw.to_vec()).is_none())?;
            }
            "closure.json" => {
                require(closure_bytes.insert(ordinal, raw.to_vec()).is_none())?;
            }
            "recovery.json" => {
                require(recovery_bytes.insert(ordinal, raw.to_vec()).is_none())?;
            }
            "intent.json" => {
                top_bytes.insert((ordinal, "intent"), crate::digest(raw));
                let v: Intent = codec::decode_canonical(raw, true)?;
                require(
                    v.version == 1
                        && v.binding.transaction == tx
                        && v.binding.ordinal == ordinal
                        && record::safe_id(&v.owner)
                        && record::safe_name(&v.resource)
                        && [&v.expected, &v.desired, &v.source, &v.recipe]
                            .into_iter()
                            .all(|s| record::fingerprint(s)),
                )?;
                v.binding.validate()?;
                require(intents.insert(ordinal, v).is_none())?;
            }
            "accepted.json" | "terminal.json" => {
                top_bytes.insert(
                    (
                        ordinal,
                        if kind == "accepted.json" {
                            "accepted"
                        } else {
                            "terminal"
                        },
                    ),
                    crate::digest(raw),
                );
                let v: native::Terminal = codec::decode_canonical(raw, true)?;
                require(
                    v.version == 1 && v.binding.transaction == tx && v.binding.ordinal == ordinal,
                )?;
                v.binding.validate()?;
                let map = if kind == "terminal.json" {
                    &mut terminals
                } else {
                    &mut accepted
                };
                require(map.insert(ordinal, v).is_none())?;
            }
            _ => return Err(Error::Protected),
        }
    }
    let mut closures = BTreeMap::new();
    for (ordinal, raw) in closure_bytes {
        let terminal = terminals.get(&ordinal).ok_or(Error::Unknown)?;
        let top = |kind| {
            top_bytes
                .get(&(ordinal, kind))
                .map(String::as_str)
                .ok_or(Error::Unknown)
        };
        closures.insert(
            ordinal,
            crate::native_retention::Closure::checked_hashes(
                &raw,
                &terminal.binding,
                top("intent")?,
                top("accepted")?,
                top("terminal")?,
                terminal,
            )?,
        );
    }
    let mut recoveries = BTreeMap::new();
    for (ordinal, raw) in recovery_bytes {
        let original = intents.get(&ordinal).ok_or(Error::Unknown)?;
        let original_raw = top_bytes.get(&(ordinal, "intent")).ok_or(Error::Unknown)?;
        recoveries.insert(
            ordinal,
            crate::native_recovery::Resolution::checked_hash(&raw, original, original_raw)?,
        );
    }
    let mut fenced = std::collections::BTreeSet::new();
    let mut completed = BTreeMap::new();
    let mut proofs = BTreeMap::new();
    // Recovery edges strictly increase ordinals: walk this finite DAG
    // without recursive parsing or a caller-supplied success flag.
    for (ordinal, intent) in intents.iter().rev() {
        let proof = recoveries
            .get(ordinal)
            .map(|r| {
                r.proof(
                    intent,
                    &intents,
                    &terminals,
                    &completed,
                    if permit_fence_dispatch {
                        admitting
                    } else {
                        None
                    },
                )
            })
            .transpose()?;
        if terminals
            .get(ordinal)
            .is_some_and(|t| t.state == "complete")
        {
            completed.insert(*ordinal, *ordinal);
        } else if proof.is_some_and(|p| p.1) {
            let end = recoveries
                .get(ordinal)
                .and_then(|r| r.completed_retry(&completed))
                .ok_or(Error::Unknown)?;
            completed.insert(*ordinal, end);
        }
        proofs.insert(*ordinal, proof);
    }
    for (ordinal, intent) in &intents {
        crate::native_retention::Reservation::checked(
            reservations.get(ordinal).ok_or(Error::Unknown)?,
            &intent.binding,
        )?;
        if admitting.is_some_and(|b| *b == intent.binding)
            && !accepted.contains_key(ordinal)
            && !terminals.contains_key(ordinal)
        {
            require(
                !operations.keys().any(|(o, _, _)| o == ordinal)
                    && !commands.keys().any(|(o, _, _)| o == ordinal),
            )?;
            continue;
        }
        let resolution = proofs.get(ordinal).copied().flatten();
        if resolution.is_some_and(|r| r.0) {
            let accepted = accepted.get(ordinal).ok_or(Error::Unknown)?;
            require(
                accepted.binding == intent.binding
                    && accepted.owner == intent.owner
                    && accepted.resource == intent.resource
                    && accepted.state == "accepted",
            )?;
            require(!operations.keys().any(|(o, _, _)| o == ordinal))?;
            if let Some(j) = current.filter(|j| j.transaction == tx) {
                require(
                    intent.binding.plan == j.authorization_plan
                        && intent.binding.journal == j.intent_digest
                        && intent.binding.artifact == j.artifact_digest,
                )?;
            }
            if successful && current.is_some_and(|j| j.transaction == tx) {
                require(resolution.is_some_and(|r| r.1))?;
            }
            fenced.insert(*ordinal);
            continue;
        }
        if intent.owner == "owner-registration-cleanup"
            && cleanup_admission.is_some()
            && !completed.contains_key(ordinal)
        {
            require(
                cleanup_admission.is_some_and(|mode| mode.recipe() == intent.recipe)
                    && current.is_some_and(|j| local_cleanup(j, intent)),
            )?;
            if !terminals.contains_key(ordinal) {
                if let Some(accepted) = accepted.get(ordinal) {
                    require(matches!(
                        cleanup_admission,
                        Some(CleanupRead::Inspection(_))
                    ))?;
                    require(
                        accepted.binding == intent.binding
                            && accepted.owner == intent.owner
                            && accepted.resource == intent.resource
                            && accepted.state == "accepted",
                    )?;
                }
                require(
                    !operations.keys().any(|(o, _, _)| o == ordinal)
                        && !commands.keys().any(|(o, _, _)| o == ordinal),
                )?;
                unfinished_cleanup = true;
                continue;
            }
        }
        let terminal = terminals.get(ordinal).ok_or(Error::Unknown)?;
        let accepted = accepted.get(ordinal).ok_or(Error::Unknown)?;
        require(
            terminal.binding == intent.binding
                && accepted.binding == intent.binding
                && terminal.owner == intent.owner
                && accepted.owner == intent.owner
                && terminal.resource == intent.resource
                && accepted.resource == intent.resource
                && matches!(terminal.state.as_str(), "complete" | "failed")
                && accepted.state == "accepted",
        )?;
        if terminal.state == "complete" {
            require(terminal.operations.iter().all(|o| o.status == 200))?;
        }
        let mut ids = std::collections::BTreeSet::new();
        require(
            terminal.operations.len() <= 256
                && terminal.operations.iter().all(|o| {
                    record::fingerprint(&o.server)
                        && record::safe_name(&o.project)
                        && record::safe_name(&o.resource)
                        && record::safe_id(&o.id)
                        && record::safe_id(&o.kind)
                        && matches!(o.status, 200 | 400 | 401)
                        && ids.insert(o.id.clone())
                }),
        )?;
        for (n, op) in terminal.operations.iter().enumerate() {
            let mut projected = op.clone();
            projected.status = 0;
            let before = projected.clone();
            projected.id.clear();
            for (phase, expected) in [
                ("intent", &projected),
                ("accepted", &before),
                ("terminal", op),
            ] {
                if let Some(found) = operations.get(&(*ordinal, n + 1, format!("{phase}.json"))) {
                    require(found == expected)?;
                } else {
                    let closure = closures.get(ordinal).ok_or(Error::Unknown)?;
                    require(closure.covers(
                        &format!("{ordinal:03}.operation-{:03}-{phase}.json", n + 1),
                        &codec::encode(expected, true)?,
                    ))?;
                }
            }
        }
        let command_count = closures
            .get(ordinal)
            .map(|c| c.commands.len())
            .unwrap_or_else(|| {
                commands
                    .keys()
                    .filter(|(o, _, phase)| o == ordinal && phase == "intent.json")
                    .count()
            });
        require(command_count <= 256)?;
        for n in 1..=command_count {
            let expected = closures
                .get(ordinal)
                .and_then(|c| c.commands.get(n - 1))
                .or_else(|| commands.get(&(*ordinal, n, "intent.json".into())))
                .ok_or(Error::Unknown)?;
            for phase in ["intent", "terminal"] {
                if let Some(found) = commands.get(&(*ordinal, n, format!("{phase}.json"))) {
                    require(found == expected)?;
                } else {
                    let closure = closures.get(ordinal).ok_or(Error::Unknown)?;
                    require(closure.covers(
                        &format!("{ordinal:03}.command-{n:03}-{phase}.json"),
                        &codec::encode(expected, true)?,
                    ))?;
                }
            }
        }
        require(
            commands
                .keys()
                .filter(|(o, _, _)| o == ordinal)
                .all(|(_, n, _)| *n <= command_count),
        )?;
        if let Some(j) = current.filter(|j| j.transaction == tx) {
            if successful {
                let local_cleanup = local_cleanup(j, intent)
                    && terminal.state == "failed"
                    && terminal.operations.is_empty()
                    && command_count == 0;
                require(
                    terminal.state == "complete"
                        || resolution.is_some_and(|r| r.1)
                        || local_cleanup,
                )?;
            }
            require(
                intent.binding.plan == j.authorization_plan
                    && intent.binding.journal == j.intent_digest
                    && intent.binding.artifact == j.artifact_digest,
            )?;
        }
    }
    require(
        commands
            .keys()
            .all(|(ordinal, _, _)| terminals.contains_key(ordinal) || fenced.contains(ordinal)),
    )?;
    require(operations.keys().all(|(ordinal, n, _)| {
        fenced.contains(ordinal)
            || terminals
                .get(ordinal)
                .is_some_and(|t| *n <= t.operations.len())
    }))?;
    require(
        terminals.keys().all(|k| intents.contains_key(k))
            && accepted.keys().all(|k| intents.contains_key(k)),
    )?;

    Ok(unfinished_cleanup
        || intents.iter().any(|(ordinal, intent)| {
            intent.owner == "owner-registration-cleanup"
                && intent.resource == "test-yard"
                && !terminals
                    .get(ordinal)
                    .is_some_and(|t| t.state == "complete")
                && !completed.contains_key(ordinal)
        }))
}

fn local_cleanup(j: &j::Record, intent: &Intent) -> bool {
    intent.owner == "owner-registration-cleanup"
        && intent.resource == "test-yard"
        && j.checkpoint == "complete"
        && j.transaction == intent.binding.transaction
        && intent.binding.plan == j.authorization_plan
        && intent.binding.journal == j.intent_digest
        && intent.binding.artifact == j.artifact_digest
        && j.steps.iter().flatten().any(|step| {
            step.resource == "test-yard-owner"
                && step.checkpoint == "verified"
                && step.desired == intent.expected
                && step.desired == intent.desired
                && step
                    .evidence
                    .as_ref()
                    .is_some_and(|e| e.recovery == intent.source)
        })
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    use std::{fs, os::unix::fs::PermissionsExt, path::PathBuf};
    pub(crate) struct Fixture(pub(crate) PathBuf);
    #[test]
    fn local_owner_cleanup_pending_preserves_proofs_without_authorizing_other_failures() {
        let (f, store, mut journal, binding) = fixture();
        let hash = "a".repeat(64);
        journal.checkpoint = "complete".into();
        journal.steps = Some(vec![j::Step {
            resource: "test-yard-owner".into(),
            checkpoint: "verified".into(),
            desired: hash.clone(),
            evidence: Some(j::Evidence {
                recovery: hash.clone(),
                ..Default::default()
            }),
            ..Default::default()
        }]);
        let mut intent = Intent {
            version: 1,
            binding: binding.clone(),
            owner: "owner-registration-cleanup".into(),
            resource: "test-yard".into(),
            expected: hash.clone(),
            desired: hash.clone(),
            source: hash.clone(),
            recipe: hash,
        };
        assert!(store
            .required_cleanup_pending(&journal, &intent.recipe, 1)
            .unwrap());
        assert!(store
            .completed_cleanup_without_recovery(&journal, 1)
            .is_err());
        write(&f, &binding, "001.intent.json", &intent);
        // The exact not-yet-accepted local cleanup can be admitted under the
        // fresh update lease; this does not make generic native work terminal.
        assert!(store
            .lock()
            .unwrap()
            .cleanup_native_quiescent(&journal, &intent.recipe)
            .is_ok());
        let mut accepted = terminal(&binding, "accepted", vec![]);
        accepted.owner = intent.owner.clone();
        accepted.resource = intent.resource.clone();
        write(&f, &binding, "001.accepted.json", &accepted);
        // Missing terminal evidence remains UNKNOWN even for local cleanup.
        assert!(store.native_inventory(Some(&journal), true).is_err());
        assert!(store
            .lock()
            .unwrap()
            .cleanup_native_quiescent(&journal, &intent.recipe)
            .is_err());
        assert!(store
            .native_cleanup_admission(&journal, &intent.recipe)
            .unwrap());
        assert!(store
            .native_cleanup_admission(&journal, &"b".repeat(64))
            .is_err());
        let mut failed = accepted.clone();
        failed.state = "failed".into();
        write(&f, &binding, "001.terminal.json", &failed);
        assert!(store
            .lock()
            .unwrap()
            .cleanup_native_quiescent(&journal, &intent.recipe)
            .is_ok());
        assert!(store.native_inventory(Some(&journal), true).is_ok());
        assert!(store.native_cleanup_pending().unwrap());
        assert!(store
            .required_cleanup_pending(&journal, &intent.recipe, 1)
            .unwrap());
        assert!(store
            .completed_cleanup_without_recovery(&journal, 1)
            .is_err());
        assert!(store.native_inventory(None, false).is_err());
        assert!(store
            .native_retireable_transaction(&binding.transaction)
            .is_err());
        assert!(store.lock().unwrap().cleanup_native_transactions().is_err());
        let mut complete = failed.clone();
        complete.state = "complete".into();
        write(&f, &binding, "001.terminal.json", &complete);
        assert!(!store
            .required_cleanup_pending(&journal, &intent.recipe, 1)
            .unwrap());
        assert!(store
            .completed_cleanup_without_recovery(&journal, 1)
            .is_ok());
        assert!(store
            .required_cleanup_pending(&journal, &"b".repeat(64), 1)
            .is_err());
        write(&f, &binding, "001.terminal.json", &failed);
        journal.steps.as_mut().unwrap()[0]
            .evidence
            .as_mut()
            .unwrap()
            .recovery = "b".repeat(64);
        assert!(store.native_inventory(Some(&journal), true).is_err());
        journal.steps.as_mut().unwrap()[0]
            .evidence
            .as_mut()
            .unwrap()
            .recovery = intent.source.clone();
        intent.owner = "unrelated-component".into();
        accepted.owner = intent.owner.clone();
        failed.owner = intent.owner.clone();
        write(&f, &binding, "001.intent.json", &intent);
        write(&f, &binding, "001.accepted.json", &accepted);
        write(&f, &binding, "001.terminal.json", &failed);
        assert!(store.native_inventory(Some(&journal), true).is_err());
    }
    impl Drop for Fixture {
        fn drop(&mut self) {
            fs::remove_dir_all(&self.0).unwrap()
        }
    }
    pub(crate) fn fixture() -> (Fixture, store::Store, j::Record, native::Binding) {
        let root = PathBuf::from(format!(
            "/tmp/subyard-native-facts-{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        fs::create_dir(&root).unwrap();
        fs::set_permissions(&root, fs::Permissions::from_mode(0o700)).unwrap();
        let fixture = Fixture(root);
        let store = store::Store::open_canonical_fixture(&fixture.0).unwrap();
        let hash = "a".repeat(64);
        let mut journal = j::Record {
            schema_version: 2,
            transaction: "tx-native-facts".into(),
            goal: j::Goal {
                target: "new".into(),
                direction: "activate-target".into(),
            },
            releases: j::ReleasePair {
                from: "old".into(),
                target: "new".into(),
                previous: None,
            },
            authorization_plan: format!("plan-v1-{hash}"),
            resume_plan: format!("resume-v1-{hash}"),
            artifact_digest: hash.clone(),
            registry_digest: hash.clone(),
            catalog_digest: hash.clone(),
            observation_scope: hash.clone(),
            authorization_digest: hash.clone(),
            checkpoint: "reconciling".into(),
            steps: Some(vec![]),
            ..Default::default()
        };
        journal.intent_digest = record::Journal::intent_digest(&journal).unwrap();
        let journal = record::Journal::new(journal).unwrap().wire().clone();
        let binding = native::Binding {
            session: hash.clone(),
            transaction: journal.transaction.clone(),
            plan: journal.authorization_plan.clone(),
            journal: journal.intent_digest.clone(),
            artifact: hash.clone(),
            helper: hash,
            ordinal: 1,
        };
        write(
            &fixture,
            &binding,
            "001.reservation.json",
            &crate::native_retention::Reservation::new(&binding),
        );
        (fixture, store, journal, binding)
    }
    pub(crate) fn write(f: &Fixture, b: &native::Binding, name: &str, value: &impl Serialize) {
        let root =
            f.0.join("migrations/native-custody/v1")
                .join(&b.transaction);
        fs::create_dir_all(&root).unwrap();
        let mut p = root.as_path();
        while p != f.0 {
            fs::set_permissions(p, fs::Permissions::from_mode(0o700)).unwrap();
            p = p.parent().unwrap()
        }
        let p = root.join(name);
        fs::write(&p, codec::encode(value, true).unwrap()).unwrap();
        fs::set_permissions(p, fs::Permissions::from_mode(0o600)).unwrap()
    }
    pub(crate) fn terminal(
        b: &native::Binding,
        state: &str,
        operations: Vec<native::Operation>,
    ) -> native::Terminal {
        native::Terminal {
            version: 1,
            binding: b.clone(),
            owner: "host-power".into(),
            resource: "default".into(),
            state: state.into(),
            server: String::new(),
            project: String::new(),
            instance: String::new(),
            instance_uuid: String::new(),
            operation: String::new(),
            final_status: 0,
            operations,
            observed: String::new(),
        }
    }
    #[test]
    fn terminal_failure_proves_only_closure_and_missing_uuid_stays_unknown() {
        let (f, s, j, b) = fixture();
        let hash = "a".repeat(64);
        write(
            &f,
            &b,
            "001.intent.json",
            &Intent {
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
        assert_eq!(s.native_inventory(Some(&j), false), Err(Error::Unknown));
        write(&f, &b, "001.terminal.json", &terminal(&b, "failed", vec![]));
        assert!(s.native_inventory(Some(&j), false).is_ok());
        assert!(s.native_inventory(Some(&j), true).is_err());
        let intent = native::Operation {
            kind: "instance-state".into(),
            server: "b".repeat(64),
            project: "project".into(),
            resource: "yard".into(),
            id: String::new(),
            status: 0,
        };
        write(&f, &b, "001.operation-001-intent.json", &intent);
        assert!(s.native_inventory(Some(&j), false).is_err());
        let mut accepted = intent.clone();
        accepted.id = "operation-1".into();
        write(&f, &b, "001.operation-001-accepted.json", &accepted);
        let mut failed = accepted.clone();
        failed.status = 400;
        write(&f, &b, "001.operation-001-terminal.json", &failed);
        write(
            &f,
            &b,
            "001.terminal.json",
            &terminal(&b, "failed", vec![failed.clone()]),
        );
        assert!(s.native_inventory(Some(&j), false).is_ok());
        assert!(s.native_inventory(Some(&j), true).is_err());
        write(
            &f,
            &b,
            "001.terminal.json",
            &terminal(&b, "complete", vec![failed]),
        );
        assert!(s.native_inventory(Some(&j), false).is_err());
        let mut success = accepted;
        success.status = 200;
        write(&f, &b, "001.operation-001-terminal.json", &success);
        write(
            &f,
            &b,
            "001.terminal.json",
            &terminal(&b, "complete", vec![success]),
        );
        assert!(s.native_inventory(Some(&j), true).is_ok());
        let command = Command {
            kind: "profile-transport".into(),
            source: "c".repeat(64),
        };
        write(&f, &b, "001.command-001-intent.json", &command);
        assert!(s.native_inventory(Some(&j), true).is_err());
        write(&f, &b, "001.command-001-terminal.json", &command);
        assert!(s.native_inventory(Some(&j), true).is_ok());
        fs::remove_file(
            f.0.join("migrations/native-custody/v1")
                .join(&b.transaction)
                .join("001.operation-001-intent.json"),
        )
        .unwrap();
        assert_eq!(s.native_inventory(Some(&j), false), Err(Error::Unknown));
        write(&f, &b, "001.operation-001-intent.json", &intent);
        for cut in ["after-native-closure", "after-native-fact-unlink"] {
            use std::io::{BufRead, BufReader};
            use std::process::{Command as Process, Stdio};
            let mut child = Process::new(std::env::current_exe().unwrap())
                .args(["--exact", "custody::tests::retirement_child", "--nocapture"])
                .env("SUBYARD_NATIVE_RETIRE_ROOT", &f.0)
                .env("SUBYARD_NATIVE_RETIRE_CUT", cut)
                .stdout(Stdio::piped())
                .spawn()
                .unwrap();
            let output = child.stdout.take().unwrap();
            let (tx, rx) = std::sync::mpsc::channel();
            std::thread::spawn(move || {
                for line in BufReader::new(output).lines() {
                    if line.unwrap().starts_with("publication-cut:") {
                        let _ = tx.send(());
                        break;
                    }
                }
            });
            let ready = rx.recv_timeout(std::time::Duration::from_secs(5));
            child.kill().unwrap();
            child.wait().unwrap();
            ready.unwrap();
            assert!(s.native_inventory(Some(&j), true).is_ok());
        }
        let fresh = store::Store::open_canonical_fixture(&f.0).unwrap();
        fresh.lock().unwrap().close_native(&b).unwrap();
        assert!(fresh.native_inventory(Some(&j), true).is_ok());
        let dir =
            f.0.join("migrations/native-custody/v1")
                .join(&b.transaction);
        assert!(!dir.join("001.operation-001-intent.json").exists());
        assert!(!dir.join("001.command-001-terminal.json").exists());
        // A conflicting residual cannot be hidden by a valid immutable closure.
        write(
            &f,
            &b,
            "001.operation-001-intent.json",
            &terminal(&b, "accepted", vec![]),
        );
        assert!(fresh.native_inventory(Some(&j), true).is_err());
    }
    #[test]
    fn native_capacity_refuses_before_publication_and_closure_frees_reservation() {
        let (f, s, _, mut binding) = fixture();
        let lease = s.lock().unwrap();
        for ordinal in [2, 3] {
            binding.ordinal = ordinal;
            lease.reserve_native(&binding).unwrap();
        }
        let dir =
            f.0.join("migrations/native-custody/v1")
                .join(&binding.transaction);
        let before = fs::read_dir(&dir).unwrap().count();
        binding.ordinal = 4;
        assert_eq!(lease.reserve_native(&binding), Err(Error::Bound));
        assert_eq!(fs::read_dir(&dir).unwrap().count(), before);
        assert!(!dir.join("004.reservation.json").exists());
        binding.ordinal = 1;
        let hash = "a".repeat(64);
        write(
            &f,
            &binding,
            "001.intent.json",
            &Intent {
                version: 1,
                binding: binding.clone(),
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
            &binding,
            "001.accepted.json",
            &terminal(&binding, "accepted", vec![]),
        );
        write(
            &f,
            &binding,
            "001.terminal.json",
            &terminal(&binding, "complete", vec![]),
        );
        lease.close_native(&binding).unwrap();
        binding.ordinal = 4;
        lease.reserve_native(&binding).unwrap();
        assert!(dir.join("004.reservation.json").exists());
    }
    #[test]
    fn retirement_child() {
        let Ok(root) = std::env::var("SUBYARD_NATIVE_RETIRE_ROOT") else {
            return;
        };
        let mut store = store::Store::open_canonical_fixture(std::path::Path::new(&root)).unwrap();
        store.test_cut(
            match std::env::var("SUBYARD_NATIVE_RETIRE_CUT").as_deref() {
                Ok("after-native-closure") => Some("after-native-closure"),
                Ok("after-native-fact-unlink") => Some("after-native-fact-unlink"),
                _ => None,
            },
        );
        let raw = store
            .read(&store::Address::native("tx-native-facts", 1, "terminal").unwrap())
            .unwrap();
        let terminal: native::Terminal =
            codec::decode_canonical(raw.bytes().unwrap(), true).unwrap();
        store
            .lock()
            .unwrap()
            .close_native(&terminal.binding)
            .unwrap();
    }
}
