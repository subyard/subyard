//! Execute a consent-bound finite program using the ordinary native journal.
use crate::{
    codec,
    custody::Intent,
    execution::Effect,
    lease::UpdateLease,
    native::{Binding, Request as N, Terminal},
    native_recovery::Resolution,
    owner::Owner,
    program::{CompensatedAttempt, Operation, Program},
    record::require,
    store::Address,
    wire::journal_v2 as j,
    work::OwnerBefore,
    Error, Result,
};
use std::collections::{BTreeMap, BTreeSet};

struct Invocation {
    binding: Binding,
    state: String,
}
struct State {
    bindings: Vec<Binding>,
    chains: Vec<Vec<Invocation>>,
    attempts: Vec<CompensatedAttempt>,
}
#[derive(Clone, Copy)]
struct CapturedProgram<'a> {
    group: &'a Binding,
    before: &'a OwnerBefore,
    token: &'a str,
    program: &'a Program,
}
impl State {
    fn failed(&self, program: &Program) -> Option<usize> {
        (0..program.forward.len()).find(|&i| self.terminal(i) == Some("failed"))
    }
    fn terminal(&self, index: usize) -> Option<&str> {
        self.chains[index]
            .last()
            .filter(|v| v.binding == self.bindings[index])
            .map(|v| v.state.as_str())
    }
    /// Reconstruct causal order from explicit retry edges, never directory order.
    fn events(&self, program: &Program) -> Result<Vec<(usize, &Invocation, bool)>> {
        let mut events = vec![];
        let mut seen = BTreeSet::new();
        let mut append = |index: usize, through: u64| {
            for invocation in &self.chains[index] {
                if invocation.binding.ordinal <= through && seen.insert(invocation.binding.ordinal)
                {
                    events.push((index, invocation, false));
                }
            }
        };
        for attempt in &self.attempts {
            for index in (0..=attempt.failed).chain(
                program
                    .compensation_range(attempt.failed)
                    .ok_or(Error::Invalid)?,
            ) {
                append(index, attempt.bindings[index]);
            }
        }
        for index in 0..program.slots() {
            append(index, u64::MAX);
        }
        // A completed teardown revokes the old create capabilities, while
        // retaining installation, cache and actual restoration evidence.
        for position in 0..events.len() {
            let (index, invocation, _) = events[position];
            let action = program.action(index).ok_or(Error::Invalid)?;
            if action.operation != Operation::Teardown || invocation.state != "complete" {
                continue;
            }
            for (prior, _, retained) in &mut events[..position] {
                let created = program.action(*prior).ok_or(Error::Invalid)?;
                if matches!(created.operation, Operation::Project | Operation::Init)
                    && if action.slot == 0 {
                        created.slot == 0
                    } else {
                        created.slot != 0
                    }
                {
                    *retained = true;
                }
            }
        }
        Ok(events)
    }
}

impl Owner {
    fn program_state(
        &self,
        group: &Binding,
        before: &OwnerBefore,
        token: &str,
        program: &Program,
    ) -> Result<State> {
        program.validate()?;
        require(group.ordinal + program.slots() as u64 <= 2049)?;
        let mut state = State {
            bindings: vec![],
            chains: vec![],
            attempts: vec![],
        };
        let expected = before.fingerprint()?;
        let desired = before.desired().fingerprint()?;
        let mut attempts = BTreeMap::new();
        for index in 0..program.slots() {
            let mut binding = group.clone();
            binding.ordinal += index as u64;
            let reference = program.reference(group, index)?;
            let mut chain = vec![];
            loop {
                binding.validate()?;
                let read = |kind| {
                    self.store.read(&Address::native(
                        &binding.transaction,
                        binding.ordinal,
                        kind,
                    )?)
                };
                let raw = read("intent")?;
                let accepted = read("accepted")?;
                let terminal = read("terminal")?;
                let recovery = read("recovery")?;
                let Some(bytes) = raw.bytes() else {
                    require(
                        accepted.bytes().is_none()
                            && terminal.bytes().is_none()
                            && recovery.bytes().is_none(),
                    )?;
                    break;
                };
                let intent: Intent = codec::decode_canonical(bytes, true)?;
                let request = N::ApplyOwnerAction {
                    binding: binding.clone(),
                    before: before.clone(),
                    token: token.into(),
                    action: reference.clone(),
                };
                require(
                    intent.version == 1
                        && intent.binding == binding
                        && intent.owner == "owner-registration"
                        && intent.resource == "test-yard"
                        && intent.source == before.controller
                        && intent.expected == expected
                        && intent.desired.as_str()
                            == if index < program.forward.len() {
                                desired.as_str()
                            } else {
                                expected.as_str()
                            }
                        && intent.recipe == request.recipe_fingerprint()?,
                )?;
                let Some(accepted) = accepted.bytes() else {
                    require(terminal.bytes().is_none() && recovery.bytes().is_none())?;
                    break;
                };
                let accepted: Terminal = codec::decode_canonical(accepted, true)?;
                let terminal: Terminal =
                    codec::decode_canonical(terminal.bytes().ok_or(Error::Unknown)?, true)?;
                for (fact, allowed) in [
                    (&accepted, &["accepted"][..]),
                    (&terminal, &["complete", "failed"][..]),
                ] {
                    require(
                        fact.version == 1
                            && fact.binding == binding
                            && fact.owner == intent.owner
                            && fact.resource == intent.resource
                            && allowed.contains(&fact.state.as_str()),
                    )?;
                }
                // A typed native capsule uses output handoff receipts. Missing
                // output cannot silently become an old/synthetic producer.
                if !before.prepared_recipe.is_empty() {
                    require(read("physical")?.bytes().is_some())?;
                }
                self.store.validate_owner_producer(&binding)?;
                chain.push(Invocation {
                    binding: binding.clone(),
                    state: terminal.state,
                });
                let Some(raw) = recovery.bytes() else {
                    break;
                };
                let resolution = Resolution::checked(raw, &intent, bytes)?;
                if let Some(attempt) = resolution.compensated() {
                    require(
                        attempt.group == *group
                            && attempt.program == *program
                            && codec::encode(&attempt.before, false)?
                                == codec::encode(before, false)?
                            && attempt.token == token,
                    )?;
                    let rank = *attempt.retries.iter().max().ok_or(Error::Invalid)?;
                    let encoded = codec::encode(attempt, false)?;
                    if let Some((previous, _)) = attempts.get(&rank) {
                        require(*previous == encoded)?;
                    } else {
                        attempts.insert(rank, (encoded, attempt.clone()));
                    }
                }
                binding = resolution.retry_binding().clone();
            }
            state.bindings.push(binding);
            state.chains.push(chain);
        }
        state.attempts = attempts.into_values().map(|(_, attempt)| attempt).collect();
        Ok(state)
    }

    pub(crate) fn restore_program_results(
        &self,
        group: &Binding,
        before: &OwnerBefore,
        token: &str,
        program: &Program,
        consumer: Option<&Binding>,
        action: Option<&crate::program::ActionReference>,
    ) -> Result<bool> {
        // Synthetic/historical component contracts without an installed payload
        // cannot carry typed original-created-resource results.
        if before.prepared_recipe.is_empty() {
            return Ok(false);
        }
        let state = self.program_state(group, before, token, program)?;
        let mut restored = false;
        for (index, invocation, retained_only) in state.events(program)? {
            self.call(N::BindOwnerResults {
                session: self.session.clone(),
                binding: consumer.cloned(),
                producer: invocation.binding.clone(),
                before: Box::new(before.clone()),
                token: token.into(),
                action: action.cloned(),
                producer_action: Some(program.reference(group, index)?),
                retained_only,
            })?;
            restored = true;
        }
        Ok(restored)
    }

    fn program_action(
        &self,
        lease: &UpdateLease<'_>,
        journal: &j::Record,
        captured: CapturedProgram<'_>,
        index: usize,
        failed: &mut bool,
    ) -> Result<()> {
        let CapturedProgram {
            group,
            before,
            token,
            program,
        } = captured;
        let expected = before.fingerprint()?;
        let desired = if index < program.forward.len() {
            before.desired().fingerprint()?
        } else {
            expected.clone()
        };
        let action = program.reference(group, index)?;
        self.dispatch(
            lease,
            journal,
            group.ordinal + index as u64,
            Effect {
                owner: "owner-registration",
                resource: "test-yard",
                expected: &expected,
                desired: &desired,
                source: &before.controller,
            },
            |binding| N::ApplyOwnerAction {
                binding,
                before: before.clone(),
                token: token.into(),
                action,
            },
            Some(failed),
        )
    }

    fn verify_compensated(
        &self,
        group: &Binding,
        before: &OwnerBefore,
        token: &str,
        program: &Program,
        failed: usize,
    ) -> Result<()> {
        let index = program
            .compensation_range(failed)
            .ok_or(Error::Invalid)?
            .last()
            .ok_or(Error::Invalid)?;
        self.restore_program_results(group, before, token, program, None, None)?;
        require(
            self.call(N::ObserveOwnerAction {
                session: self.session.clone(),
                before: before.clone(),
                token: token.into(),
                action: program.reference(group, index)?,
            })?
            .actual
                == "desired",
        )
    }

    fn publish_program_retry(
        &self,
        lease: &UpdateLease<'_>,
        proof: &CompensatedAttempt,
    ) -> Result<()> {
        for index in 0..proof.program.slots() {
            if !proof.replayed(index) {
                continue;
            }
            let binding = proof.binding(index);
            let raw = self.store.read(&Address::native(
                &binding.transaction,
                binding.ordinal,
                "intent",
            )?)?;
            let intent = codec::decode_canonical(raw.bytes().ok_or(Error::Unknown)?, true)?;
            let edge = Resolution::compensated_edge(
                &intent,
                raw.bytes().ok_or(Error::Unknown)?,
                proof,
                proof.retry(index),
            )?;
            lease.create_immutable(
                &Address::native(&binding.transaction, binding.ordinal, "recovery")?,
                &edge,
            )?;
            crate::fixture::point("after-program-retry-edge")?;
        }
        Ok(())
    }

    pub(crate) fn run_program(
        &self,
        lease: &UpdateLease<'_>,
        journal: &j::Record,
        group: Binding,
        before: &OwnerBefore,
        token: &str,
        program: &Program,
    ) -> Result<()> {
        let captured = CapturedProgram {
            group: &group,
            before,
            token,
            program,
        };
        let state = self.program_state(&group, before, token, program)?;
        // A crash may leave only a prefix of the immutable group retry edges.
        // Finish that same mapping before admitting any new invocation.
        if let Some(proof) = state.attempts.last() {
            let mut missing = false;
            let mut started = false;
            for index in 0..program.slots() {
                if !proof.replayed(index) {
                    continue;
                }
                missing |= self
                    .store
                    .read(&Address::native(
                        &group.transaction,
                        proof.bindings[index],
                        "recovery",
                    )?)?
                    .bytes()
                    .is_none();
                started |= self
                    .store
                    .read(&Address::native(
                        &group.transaction,
                        proof.retries[index],
                        "intent",
                    )?)?
                    .bytes()
                    .is_some();
            }
            if missing || !started {
                require(!started)?;
                self.native_quiescent(Some(journal))?;
                self.verify_compensated(&group, before, token, program, proof.failed)?;
                self.publish_program_retry(lease, proof)?;
            }
        }
        let state = self.program_state(&group, before, token, program)?;
        if let Some(failed) = state
            .failed(program)
            .filter(|&i| program.compensation_range(i).is_some())
        {
            return self.compensate_program(lease, journal, captured, failed);
        }
        for index in 0..program.forward.len() {
            let mut failed = false;
            match self.program_action(lease, journal, captured, index, &mut failed) {
                Ok(()) => {}
                Err(_) if failed && program.compensation_range(index).is_some() => {
                    return self.compensate_program(lease, journal, captured, index)
                }
                Err(error) => return Err(error),
            }
        }
        self.verify_program_complete(&group, before, token, program)
    }

    fn compensate_program(
        &self,
        lease: &UpdateLease<'_>,
        journal: &j::Record,
        captured: CapturedProgram<'_>,
        failed: usize,
    ) -> Result<()> {
        let CapturedProgram {
            group,
            before,
            token,
            program,
        } = captured;
        self.native_quiescent(Some(journal))?;
        for index in program.compensation_range(failed).ok_or(Error::Invalid)? {
            self.program_action(lease, journal, captured, index, &mut false)?;
        }
        self.verify_compensated(group, before, token, program, failed)?;
        let state = self.program_state(group, before, token, program)?;
        let bindings = state.bindings.iter().map(|b| b.ordinal).collect::<Vec<_>>();
        let mut last = self
            .store
            .native_ordinal_high_water(&group.transaction)?
            .max(bindings.iter().copied().max().ok_or(Error::Invalid)?);
        let reserved = self.cleanup_owner_ordinal(journal)?;
        let mut proof = CompensatedAttempt {
            program: program.clone(),
            before: before.clone(),
            token: token.into(),
            group: group.clone(),
            failed,
            retries: bindings.clone(),
            bindings,
        };
        for index in 0..program.slots() {
            if proof.replayed(index) {
                last = last.checked_add(1).ok_or(Error::Bound)?;
                if reserved == Some(last) {
                    last = last.checked_add(1).ok_or(Error::Bound)?;
                }
                require(last <= 4096)?;
                proof.retries[index] = last;
            }
        }
        self.publish_program_retry(lease, &proof)?;
        // Compensation closed this attempt, not the requested migration.
        Err(Error::Io)
    }

    pub(crate) fn verify_program_complete(
        &self,
        group: &Binding,
        before: &OwnerBefore,
        token: &str,
        program: &Program,
    ) -> Result<()> {
        let state = self.program_state(group, before, token, program)?;
        require((0..program.forward.len()).all(|i| state.terminal(i) == Some("complete")))
    }
}
