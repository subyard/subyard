//! Finite prepared operations. Ordering and compensation belong to Rust;
//! native components interpret only an operation and its original captured slot.
use crate::{codec, digest, native::Binding, record::require, Result};
use serde::{Deserialize, Serialize};

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "kebab-case")]
pub(crate) enum Operation {
    ValidateOriginal,
    Project,
    Init,
    Teardown,
    Power,
    Start,
    Check,
    CheckLease,
    RestoreStopped,
    PublishRegistration,
    RemoveEmptyFlat,
    MoveOverrides,
    ArchiveRegistration,
    ArchiveController,
    RestoreRegistration,
    RestoreOverrides,
    ParkCurrentRegistration,
    VerifyCompensated,
    VerifyDesired,
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        custody::{
            self,
            tests::{fixture, terminal, write, Fixture},
        },
        native,
        native_recovery::Resolution,
        work::OwnerBefore,
    };

    fn closed(f: &Fixture, proof: &CompensatedAttempt, index: usize, retry: bool, state: &str) {
        let binding = if retry {
            proof.retry(index)
        } else {
            proof.binding(index)
        };
        let request = proof
            .request(index)
            .unwrap()
            .rebound(binding.clone())
            .unwrap();
        let expected = proof.before.fingerprint().unwrap();
        let intent = custody::Intent {
            version: 1,
            binding: binding.clone(),
            owner: "owner-registration".into(),
            resource: "test-yard".into(),
            expected: expected.clone(),
            desired: if index < proof.program.forward.len() {
                proof.before.desired().fingerprint().unwrap()
            } else {
                expected
            },
            source: proof.before.controller.clone(),
            recipe: request.recipe_fingerprint().unwrap(),
        };
        write(
            f,
            &binding,
            &format!("{:03}.reservation.json", binding.ordinal),
            &crate::native_retention::Reservation::new(&binding),
        );
        write(
            f,
            &binding,
            &format!("{:03}.intent.json", binding.ordinal),
            &intent,
        );
        for status in ["accepted", state] {
            let mut fact = terminal(&binding, status, vec![]);
            fact.owner = intent.owner.clone();
            fact.resource = intent.resource.clone();
            write(
                f,
                &binding,
                &format!(
                    "{:03}.{}.json",
                    binding.ordinal,
                    if status == "accepted" {
                        "accepted"
                    } else {
                        "terminal"
                    }
                ),
                &fact,
            );
        }
    }
    fn edge(f: &Fixture, store: &crate::store::Store, proof: &CompensatedAttempt, index: usize) {
        let binding = proof.binding(index);
        let raw = store
            .read(
                &crate::store::Address::native(&binding.transaction, binding.ordinal, "intent")
                    .unwrap(),
            )
            .unwrap();
        let intent = codec::decode_canonical(raw.bytes().unwrap(), true).unwrap();
        let raw =
            Resolution::compensated_edge(&intent, raw.bytes().unwrap(), proof, proof.retry(index))
                .unwrap();
        let value: Resolution = codec::decode_canonical(&raw, true).unwrap();
        write(
            f,
            &binding,
            &format!("{:03}.recovery.json", binding.ordinal),
            &value,
        );
    }
    fn attempt(group: native::Binding) -> CompensatedAttempt {
        use Operation::*;
        let program = Program::new(
            vec![
                Step {
                    action: Action::new(ValidateOriginal, 0),
                    compensation: Some(0),
                },
                Step {
                    action: Action::new(Check, 0),
                    compensation: Some(0),
                },
                Step {
                    action: Action::new(VerifyDesired, 0),
                    compensation: None,
                },
            ],
            vec![vec![
                Action::new(RestoreRegistration, 0),
                Action::new(VerifyCompensated, 1),
            ]],
        )
        .unwrap();
        CompensatedAttempt {
            program,
            before: OwnerBefore {
                state: "legacy-flat".into(),
                registration: "a".repeat(64),
                overrides: "b".repeat(64),
                controller: "c".repeat(64),
                ..Default::default()
            },
            token: "d".repeat(32),
            group,
            failed: 1,
            bindings: vec![1, 2, 3, 4, 5],
            retries: vec![2049, 2050, 3, 2051, 2052],
        }
    }
    #[test]
    fn compensated_prefix_needs_every_retry_even_after_partial_edge_publication() {
        let (f, store, journal, group) = fixture();
        let proof = attempt(group);
        for index in [0, 1, 3, 4] {
            closed(
                &f,
                &proof,
                index,
                false,
                if index == 1 { "failed" } else { "complete" },
            );
        }
        assert!(store.native_inventory(Some(&journal), false).is_ok());
        edge(&f, &store, &proof, 0);
        assert_eq!(
            store
                .native_ordinal_high_water(&journal.transaction)
                .unwrap(),
            2052,
            "one edge reserves all unstarted destinations, including compensation"
        );
        closed(&f, &proof, 0, true, "complete");
        assert!(
            store.native_inventory(Some(&journal), true).is_err(),
            "one replay cannot hide undone sibling effects"
        );
        assert!(
            store.native_before_dispatch(&proof.retry(1)).is_err(),
            "finish sibling edge publication before dispatch"
        );
        for index in [1, 3, 4] {
            edge(&f, &store, &proof, index);
        }
        closed(&f, &proof, 1, true, "complete");
        closed(&f, &proof, 2, false, "complete");
        store.native_inventory(Some(&journal), true).unwrap();
        // Optional compensation retry slots have no intent until another failure.
        for index in [3, 4] {
            assert!(store
                .read(
                    &crate::store::Address::native(
                        &journal.transaction,
                        proof.retries[index],
                        "intent"
                    )
                    .unwrap()
                )
                .unwrap()
                .bytes()
                .is_none());
        }
        store.validate_owner_producer(&proof.binding(0)).unwrap();
    }
    #[test]
    fn compensation_requires_exact_whole_failure_and_closed_verifier() {
        let (f, store, journal, group) = fixture();
        let proof = attempt(group);
        for index in [0, 1, 3] {
            closed(
                &f,
                &proof,
                index,
                false,
                if index == 1 { "failed" } else { "complete" },
            );
        }
        edge(&f, &store, &proof, 0);
        assert!(store.native_inventory(Some(&journal), false).is_err());
        closed(&f, &proof, 4, false, "complete");
        store.native_inventory(Some(&journal), false).unwrap();
        closed(&f, &proof, 1, false, "complete");
        assert!(
            store.native_inventory(Some(&journal), false).is_err(),
            "success cannot be reinterpreted as the failed action"
        );
    }
    #[test]
    fn repeated_failure_needs_new_compensation_and_then_new_forward_effects() {
        let (f, store, journal, group) = fixture();
        let first = attempt(group);
        for index in [0, 1, 3, 4] {
            closed(
                &f,
                &first,
                index,
                false,
                if index == 1 { "failed" } else { "complete" },
            );
        }
        for index in [0, 1, 3, 4] {
            edge(&f, &store, &first, index);
        }
        closed(&f, &first, 0, true, "complete");
        closed(&f, &first, 1, true, "failed");
        let second = CompensatedAttempt {
            bindings: first.retries.clone(),
            retries: vec![2053, 2054, 3, 2055, 2056],
            ..first.clone()
        };
        // The earlier compensation cannot prove the second failed attempt.
        edge(&f, &store, &second, 0);
        assert!(store.native_inventory(Some(&journal), false).is_err());
        for index in [3, 4] {
            closed(&f, &first, index, true, "complete");
        }
        for index in [1, 3, 4] {
            edge(&f, &store, &second, index);
        }
        closed(&f, &second, 0, true, "complete");
        closed(&f, &second, 1, true, "complete");
        closed(&f, &second, 2, false, "complete");
        store.native_inventory(Some(&journal), true).unwrap();
    }
    #[test]
    fn program_slots_leave_activation_and_cleanup_outside_action_ordinals() {
        let (_, _, _, group) = fixture();
        let proof = attempt(group);
        let mut before = proof.before;
        before.program = Some(proof.program);
        let mut work = vec![crate::work::Work {
            intent: Default::default(),
            recipe: crate::work::Recipe::TestYardOwnerRegistration(before),
        }];
        assert_eq!(ordinal(&work, 0).unwrap(), 1);
        assert_eq!(completion_ordinal(&work, 0).unwrap(), 3);
        assert_eq!(ordinal(&work, work.len()).unwrap(), 6);
        let empty = || crate::work::Work {
            intent: Default::default(),
            recipe: crate::work::Recipe::LegacyV1Import {
                static_pin: String::new(),
            },
        };
        work.extend(std::iter::repeat_with(empty).take(2043));
        assert_eq!(ordinal(&work, work.len()).unwrap(), 2049);
        work.push(empty());
        assert!(ordinal(&work, work.len()).is_err());
        assert!(completion_ordinal(&work, work.len()).is_err());
    }
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct Action {
    pub operation: Operation,
    pub slot: usize,
}
impl Action {
    pub(crate) fn new(operation: Operation, slot: usize) -> Self {
        Self { operation, slot }
    }
    fn validate(&self) -> Result<()> {
        use Operation::*;
        require(match self.operation {
            Project | Init => self.slot < 3,
            Teardown | Check => self.slot < 2,
            Power | Start | CheckLease => self.slot == 1,
            RestoreStopped => self.slot == 1 || self.slot == 2,
            VerifyCompensated => self.slot < 3,
            _ => self.slot == 0,
        })
    }
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct Step {
    pub action: Action,
    pub compensation: Option<usize>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct Program {
    version: u8,
    pub forward: Vec<Step>,
    pub compensations: Vec<Vec<Action>>,
}
impl Program {
    pub(crate) fn new(forward: Vec<Step>, compensations: Vec<Vec<Action>>) -> Result<Self> {
        let value = Self {
            version: 1,
            forward,
            compensations,
        };
        value.validate()?;
        Ok(value)
    }
    pub(crate) fn validate(&self) -> Result<()> {
        require(self.version == 1 && !self.forward.is_empty() && self.slots() <= 64)?;
        require(self.forward.last().is_some_and(|s| {
            s.action.operation == Operation::VerifyDesired && s.compensation.is_none()
        }))?;
        for step in &self.forward {
            step.action.validate()?;
            require(
                step.compensation
                    .is_none_or(|n| n < self.compensations.len()),
            )?;
        }
        for actions in &self.compensations {
            require(
                actions
                    .last()
                    .is_some_and(|a| a.operation == Operation::VerifyCompensated),
            )?;
            for action in actions {
                action.validate()?;
            }
        }
        Ok(())
    }
    pub(crate) fn slots(&self) -> usize {
        self.forward.len() + self.compensations.iter().map(Vec::len).sum::<usize>()
    }
    pub(crate) fn action(&self, index: usize) -> Option<&Action> {
        if let Some(step) = self.forward.get(index) {
            return Some(&step.action);
        }
        let mut remaining = index.checked_sub(self.forward.len())?;
        for actions in &self.compensations {
            if let Some(action) = actions.get(remaining) {
                return Some(action);
            }
            remaining = remaining.checked_sub(actions.len())?;
        }
        None
    }
    pub(crate) fn compensation_range(&self, failed: usize) -> Option<std::ops::Range<usize>> {
        let group = self.forward.get(failed)?.compensation?;
        let start = self.forward.len()
            + self.compensations[..group]
                .iter()
                .map(Vec::len)
                .sum::<usize>();
        Some(start..start + self.compensations[group].len())
    }
    pub(crate) fn fingerprint(&self) -> Result<String> {
        self.validate()?;
        Ok(digest(&codec::encode(self, false)?))
    }
    pub(crate) fn reference(&self, group: &Binding, index: usize) -> Result<ActionReference> {
        let action = self.action(index).ok_or(crate::Error::Invalid)?.clone();
        Ok(ActionReference {
            group: group.clone(),
            program: self.fingerprint()?,
            index,
            action,
        })
    }
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct ActionReference {
    pub group: Binding,
    pub program: String,
    pub index: usize,
    pub action: Action,
}
impl ActionReference {
    pub(crate) fn validate(&self, binding: &Binding) -> Result<()> {
        self.group.validate()?;
        binding.validate()?;
        let mut scope = binding.clone();
        scope.ordinal = self.group.ordinal;
        require(
            scope == self.group && self.index < 64 && crate::record::fingerprint(&self.program),
        )?;
        self.action.validate()
    }
}

pub(crate) fn work_slots(work: &crate::work::Work) -> usize {
    match &work.recipe {
        crate::work::Recipe::TestYardOwnerRegistration(before) => {
            before.program.as_ref().map_or(1, Program::slots)
        }
        _ => 1,
    }
}
pub(crate) fn ordinal(work: &[crate::work::Work], index: usize) -> Result<u64> {
    require(index <= work.len())?;
    let ordinal = 1 + work[..index].iter().map(work_slots).sum::<usize>();
    require(ordinal <= 2048 || index == work.len() && ordinal == 2049)?;
    Ok(ordinal as u64)
}

pub(crate) fn completion_ordinal(work: &[crate::work::Work], index: usize) -> Result<u64> {
    require(index < work.len())?;
    let base = ordinal(work, index)?;
    let extra = match &work[index].recipe {
        crate::work::Recipe::TestYardOwnerRegistration(before) => {
            before.program.as_ref().map_or(0, |p| p.forward.len() - 1)
        }
        _ => 0,
    };
    Ok(base + extra as u64)
}

/// This is the typed group variant of an existing native retry edge, not a
/// second journal. It proves the closed attempt that undid earlier effects.
#[derive(Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct CompensatedAttempt {
    pub program: Program,
    pub before: crate::work::OwnerBefore,
    pub token: String,
    pub group: Binding,
    pub failed: usize,
    pub bindings: Vec<u64>,
    pub retries: Vec<u64>,
}
impl CompensatedAttempt {
    pub(crate) fn binding(&self, index: usize) -> Binding {
        let mut binding = self.group.clone();
        binding.ordinal = self.bindings[index];
        binding
    }
    pub(crate) fn retry(&self, index: usize) -> Binding {
        let mut binding = self.group.clone();
        binding.ordinal = self.retries[index];
        binding
    }
    pub(crate) fn replayed(&self, index: usize) -> bool {
        index <= self.failed
            || self
                .program
                .compensation_range(self.failed)
                .is_some_and(|r| r.contains(&index))
    }
    pub(crate) fn request(&self, index: usize) -> Result<crate::native::Request> {
        Ok(crate::native::Request::ApplyOwnerAction {
            binding: self.binding(index),
            before: self.before.clone(),
            token: self.token.clone(),
            action: self.program.reference(&self.group, index)?,
        })
    }
    fn validate(&self) -> Result<()> {
        self.program.validate()?;
        self.before.validate()?;
        self.group.validate()?;
        require(
            self.group.ordinal <= 2048
                && self.bindings.len() == self.program.slots()
                && self.retries.len() == self.bindings.len()
                && self.program.compensation_range(self.failed).is_some()
                && self.token.len() == 32
                && self
                    .token
                    .bytes()
                    .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b)),
        )?;
        let mut originals = std::collections::BTreeSet::new();
        let mut retries = std::collections::BTreeSet::new();
        let last = self
            .bindings
            .iter()
            .copied()
            .max()
            .ok_or(crate::Error::Invalid)?;
        for index in 0..self.bindings.len() {
            let old = self.bindings[index];
            let retry = self.retries[index];
            require((1..=4096).contains(&old) && originals.insert(old) && retries.insert(retry))?;
            require(if self.replayed(index) {
                retry > last && retry > 2048 && retry <= 4096
            } else {
                retry == old
            })?;
        }
        Ok(())
    }
    pub(crate) fn matches(
        &self,
        original: &crate::custody::Intent,
        retry: &Binding,
    ) -> Result<usize> {
        self.validate()?;
        let index = self
            .bindings
            .iter()
            .position(|o| *o == original.binding.ordinal)
            .ok_or(crate::Error::Invalid)?;
        require(
            self.replayed(index)
                && self.binding(index) == original.binding
                && self.retry(index) == *retry,
        )?;
        self.intent(index, original)?;
        Ok(index)
    }
    fn intent(&self, index: usize, actual: &crate::custody::Intent) -> Result<()> {
        let expected = self.before.fingerprint()?;
        let desired = if index < self.program.forward.len() {
            self.before.desired().fingerprint()?
        } else {
            expected.clone()
        };
        require(
            actual.version == 1
                && actual.binding == self.binding(index)
                && actual.owner == "owner-registration"
                && actual.resource == "test-yard"
                && actual.source == self.before.controller
                && actual.expected == expected
                && actual.desired == desired
                && actual.recipe == self.request(index)?.recipe_fingerprint()?,
        )
    }
    pub(crate) fn proof(
        &self,
        intents: &std::collections::BTreeMap<u64, crate::custody::Intent>,
        terminals: &std::collections::BTreeMap<u64, crate::native::Terminal>,
    ) -> Result<()> {
        self.validate()?;
        let compensation = self
            .program
            .compensation_range(self.failed)
            .ok_or(crate::Error::Invalid)?;
        for index in (0..=self.failed).chain(compensation) {
            let ordinal = self.bindings[index];
            self.intent(index, intents.get(&ordinal).ok_or(crate::Error::Unknown)?)?;
            let terminal = terminals.get(&ordinal).ok_or(crate::Error::Unknown)?;
            require(
                terminal.version == 1
                    && terminal.binding == self.binding(index)
                    && terminal.owner == "owner-registration"
                    && terminal.resource == "test-yard"
                    && terminal.state
                        == if index == self.failed {
                            "failed"
                        } else {
                            "complete"
                        },
            )?;
        }
        Ok(())
    }
}
