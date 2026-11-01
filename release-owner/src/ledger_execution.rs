//! Authoritative ledger publication. Compatibility projection bytes remain pinned.
use crate::{
    codec,
    lease::UpdateLease,
    record::{self, require},
    source_admission::LedgerState,
    store::{Address, Snapshot},
    Error, Result,
};
impl UpdateLease<'_> {
    pub(crate) fn advance_ledger(
        &self,
        expected: &LedgerState,
        next: &record::Ledger,
        registry: &record::Registry,
        registry_digest: &str,
    ) -> Result<()> {
        let current = self.store.migration_ledger(registry, registry_digest)?;
        require(
            current.authority() == expected.authority()
                && current.projection() == expected.projection()
                && current.checkpoint().is_some() == expected.checkpoint().is_some(),
        )?;
        if let Some(previous) = expected.checkpoint() {
            let projection = if let Some(raw) = expected.projection().bytes() {
                record::Projection::from_reader(raw.to_vec(), &self.store.reader, false)?
            } else {
                record::Projection::absent()
            };
            let checkpoint = record::Checkpoint::build(
                next,
                registry,
                registry_digest,
                &projection,
                Some(previous),
                false,
            )?;
            self.compare_and_swap(
                &Address::checkpoint(),
                expected.authority(),
                &codec::encode(checkpoint.wire(), true)?,
            )
        } else {
            self.compare_and_swap(
                &Address::ledger(),
                expected.authority(),
                &codec::encode(next.wire(), true)?,
            )
        }
    }
    pub(crate) fn compact_ledger(
        &self,
        expected: &LedgerState,
        registry: &record::Registry,
        registry_digest: &str,
    ) -> Result<()> {
        let projection = if let Some(raw) = expected.projection().bytes() {
            record::Projection::from_reader(raw.to_vec(), &self.store.reader, false)?
        } else {
            record::Projection::absent()
        };
        let checkpoint = record::Checkpoint::build(
            expected.ledger(),
            registry,
            registry_digest,
            &projection,
            expected.checkpoint(),
            true,
        )?;
        let payload = codec::encode(checkpoint.wire(), true)?;
        let current = self.store.migration_ledger(registry, registry_digest)?;
        if current.projection() != expected.projection() {
            return Err(Error::Stale);
        }
        if current.authority().bytes() == Some(payload.as_slice()) {
            return Ok(());
        }
        if current.authority() != expected.authority() {
            return Err(Error::Stale);
        }
        self.compare_and_swap(
            &Address::checkpoint(),
            &self.store.read(&Address::checkpoint())?,
            &payload,
        )
    }
}
pub(crate) fn semantic_snapshot(state: &LedgerState) -> Result<Snapshot> {
    if state.checkpoint().is_some() {
        Ok(Snapshot::captured(Some(codec::encode(
            state.ledger().wire(),
            true,
        )?)))
    } else {
        Ok(state.projection().clone())
    }
}

// Reconstruct only the exact prepared history. A valid but unrecorded future
// edge is drift, even when it is a prefix of the registry's eventual goal.
#[allow(clippy::too_many_arguments)]
pub(crate) fn retained_scope_source(
    registry: &record::Registry,
    registry_digest: &str,
    current: &LedgerState,
    work: &[crate::work::Work],
    journal: &crate::wire::journal_v2::Record,
    original_checkpoint: Option<&Snapshot>,
    reader: &crate::legacy::Reader,
    compact: bool,
) -> Result<String> {
    use crate::work::Recipe;
    let ledger_work = work
        .iter()
        .filter(|w| matches!(w.recipe, Recipe::LedgerAdvance { .. }))
        .collect::<Vec<_>>();
    if ledger_work.is_empty() {
        if let Some(original) = original_checkpoint {
            if original != current.authority()
                && !(original.bytes().is_none() && current.checkpoint().is_none())
            {
                require(
                    compact
                        && journal.checkpoint == "complete"
                        && registry
                            .history()
                            .complete(current.ledger().authority())
                            .is_ok(),
                )?;
                let projection = if let Some(raw) = current.projection().bytes() {
                    record::Projection::from_reader(raw.to_vec(), reader, false)?
                } else {
                    record::Projection::absent()
                };
                let previous = original
                    .bytes()
                    .map(|raw| {
                        record::Checkpoint::new(
                            codec::decode_canonical(raw, true)?,
                            registry,
                            registry_digest,
                            &projection,
                        )
                    })
                    .transpose()?;
                if let Some(previous) = &previous {
                    require(
                        previous.ledger_view().authority().domains()
                            == current.ledger().authority().domains(),
                    )?;
                }
                let expected = record::Checkpoint::build(
                    current.ledger(),
                    registry,
                    registry_digest,
                    &projection,
                    previous.as_ref(),
                    true,
                )?;
                require(
                    current.authority().bytes()
                        == Some(codec::encode(expected.wire(), true)?.as_slice()),
                )?;
                return Ok(if original.bytes().is_none() {
                    current.projection().fingerprint()
                } else {
                    original.fingerprint()
                });
            }
        }
        return Ok(current.authority().fingerprint());
    }
    let ids = ledger_work
        .iter()
        .map(|w| w.intent.migration.clone())
        .collect::<Vec<_>>();
    let trace = registry
        .history()
        .trace_from(current.ledger().authority(), &ids)
        .map_err(|_| Error::Stale)?;
    let mut states = Vec::new();
    for (n, (w, edge)) in ledger_work.iter().zip(trace.edges()).enumerate() {
        let Recipe::LedgerAdvance { before, after } = &w.recipe else {
            return Err(Error::Invalid);
        };
        let expected = codec::encode(
            record::Ledger::from_authority(edge.before(), registry)?.wire(),
            true,
        )?;
        let desired = codec::encode(
            record::Ledger::from_authority(edge.after(), registry)?.wire(),
            true,
        )?;
        require(
            w.intent.id == format!("{}.ledger", edge.migration().id)
                && w.intent.resource == format!("ledger.{}", edge.migration().domain)
                && w.intent.decision == "transform"
                && w.intent.expected == before.fingerprint()
                && w.intent.desired == crate::digest(after)
                && *after == desired
                && if let Some(bytes) = before.bytes() {
                    bytes == expected
                } else {
                    n == 0 && edge.before().domains() == registry.history().baseline().domains()
                },
        )?;
        if let Some(previous) = states.last() {
            require(previous == before)?;
        } else {
            states.push(before.clone());
        }
        states.push(Snapshot::captured(Some(after.clone())));
    }
    let steps = journal.steps.as_deref().unwrap_or_default();
    require(steps.len() == work.len())?;
    let (mut minimum, mut maximum, mut cursor) = (0, 0, 0);
    let mut open = true;
    for (step, w) in steps.iter().zip(work) {
        require(step.id == w.intent.id)?;
        if !open {
            require(step.checkpoint == "intent")?;
        }
        if matches!(w.recipe, Recipe::LedgerAdvance { .. }) {
            match step.checkpoint.as_str() {
                "intent" => (),
                "evidence" => {
                    minimum = cursor;
                    maximum = cursor + 1;
                }
                "applied" | "verified" => {
                    minimum = cursor + 1;
                    maximum = minimum;
                }
                _ => return Err(Error::Invalid),
            }
            cursor += 1;
        }
        if step.checkpoint != "verified" {
            open = false;
        }
    }
    let semantic = semantic_snapshot(current)?;
    let cut = states
        .iter()
        .position(|state| state == &semantic)
        .ok_or(Error::Stale)?;
    require(cut >= minimum && cut <= maximum)?;
    let original = original_checkpoint.ok_or(Error::Stale)?;
    let projection = if let Some(raw) = current.projection().bytes() {
        record::Projection::from_reader(raw.to_vec(), reader, false)?
    } else {
        record::Projection::absent()
    };
    let mut checkpoint = original
        .bytes()
        .map(|raw| {
            record::Checkpoint::new(
                codec::decode_canonical(raw, true)?,
                registry,
                registry_digest,
                &projection,
            )
        })
        .transpose()?;
    if let Some(initial) = &checkpoint {
        require(initial.ledger_view().authority().domains() == trace.initial().domains())?;
        for edge in trace.edges().iter().take(cut) {
            let next = record::Ledger::from_authority(edge.after(), registry)?;
            checkpoint = Some(record::Checkpoint::build(
                &next,
                registry,
                registry_digest,
                &projection,
                checkpoint.as_ref(),
                false,
            )?);
        }
        let expected = codec::encode(checkpoint.as_ref().ok_or(Error::Invalid)?.wire(), true)?;
        if current.authority().bytes() != Some(expected.as_slice()) {
            require(
                compact
                    && journal.checkpoint == "complete"
                    && steps.iter().all(|s| s.checkpoint == "verified")
                    && registry
                        .history()
                        .complete(current.ledger().authority())
                        .is_ok(),
            )?;
            let terminal = record::Checkpoint::build(
                current.ledger(),
                registry,
                registry_digest,
                &projection,
                checkpoint.as_ref(),
                true,
            )?;
            require(
                current.authority().bytes()
                    == Some(codec::encode(terminal.wire(), true)?.as_slice()),
            )?;
        }
        Ok(original.fingerprint())
    } else if current.checkpoint().is_none() {
        Ok(states[0].fingerprint())
    } else {
        // Terminal compaction is separately authorized only after every
        // recorded step and the native operation have reached completion.
        require(
            compact
                && journal.checkpoint == "complete"
                && steps.iter().all(|s| s.checkpoint == "verified")
                && registry
                    .history()
                    .complete(current.ledger().authority())
                    .is_ok(),
        )?;
        let compact = record::Checkpoint::build(
            current.ledger(),
            registry,
            registry_digest,
            &projection,
            None,
            true,
        )?;
        require(
            current.authority().bytes() == Some(codec::encode(compact.wire(), true)?.as_slice()),
        )?;
        Ok(states[0].fingerprint())
    }
}

impl crate::owner::Owner {
    pub(crate) fn bundle_scope_source(
        &self,
        journal: &crate::wire::journal_v2::Record,
        bundle: &crate::bundle::Bundle,
        ledger: &LedgerState,
    ) -> Result<String> {
        self.verify_pending_artifacts(journal, &bundle.work)?;
        if journal.checkpoint == "complete" {
            self.native_ready(journal)?;
        }
        let compact = journal.checkpoint == "complete"
            && bundle.facts.decisions.iter().flatten().any(|d| {
                d.resource == "migration-history.checkpoint"
                    && d.decision == "transform"
                    && d.result == "compact-migration-history"
            });
        let source = retained_scope_source(
            &self.registry,
            &self.registry_digest,
            ledger,
            &bundle.work,
            journal,
            bundle.original_checkpoint.as_ref(),
            &self.store.reader,
            compact,
        )?;
        let observations = bundle
            .facts
            .observations
            .iter()
            .flatten()
            .filter(|o| o.resource == "migration-history.authority")
            .collect::<Vec<_>>();
        require(
            matches!(observations.as_slice(), [observation] if observation.class == "validated-migration-authority-v1" && observation.fingerprint == source),
        )?;
        Ok(source)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        legacy::Reader,
        wire::{journal_v2 as j, plan_v1 as f},
        work::{Recipe, Work},
    };
    use std::{
        fs,
        os::unix::fs::{DirBuilderExt, PermissionsExt},
    };

    #[test]
    fn zero_step_terminal_compaction_retains_projection_authority() {
        let (fixture, store, mut journal, _) = crate::custody::tests::fixture();
        fs::DirBuilder::new()
            .recursive(true)
            .mode(0o700)
            .create(fixture.0.join("release-transition/v2"))
            .unwrap();
        let catalog = crate::migration_owner::current_catalog().unwrap();
        let raw = include_bytes!("../../config/release-transition.json");
        let registry =
            record::Registry::new(serde_json::from_slice(raw).unwrap(), &catalog).unwrap();
        let registry_digest = crate::digest(raw);
        let mut state = registry.history().baseline();
        while let Ok(next) = registry.history().next(&state) {
            state = next.into_after();
        }
        let ledger = record::Ledger::from_authority(&state, &registry).unwrap();
        let bytes = codec::encode(ledger.wire(), true).unwrap();
        let projection = record::Projection::from_canonical(bytes.clone()).unwrap();
        let checkpoint = record::Checkpoint::build(
            &ledger,
            &registry,
            &registry_digest,
            &projection,
            None,
            true,
        )
        .unwrap();
        for (name, raw) in [
            ("ledger.json", bytes.clone()),
            (
                "history-checkpoint.json",
                codec::encode(checkpoint.wire(), true).unwrap(),
            ),
        ] {
            let path = fixture.0.join("release-transition/v2").join(name);
            fs::write(&path, raw).unwrap();
            fs::set_permissions(path, fs::Permissions::from_mode(0o600)).unwrap();
        }
        journal.checkpoint = "complete".into();
        journal.steps = Some(vec![]);
        let current = store.migration_ledger(&registry, &registry_digest).unwrap();
        let absent = Snapshot::captured(None);
        let check = |j: &j::Record, compact| {
            retained_scope_source(
                &registry,
                &registry_digest,
                &current,
                &[],
                j,
                Some(&absent),
                &Reader::CanonicalFixture,
                compact,
            )
        };
        assert!(
            check(&journal, false).is_err(),
            "unapproved terminal compaction accepted"
        );
        let mut pending = journal.clone();
        pending.checkpoint = "reconciling".into();
        assert!(
            check(&pending, true).is_err(),
            "nonterminal compaction accepted"
        );
        assert_eq!(
            check(&journal, true).unwrap(),
            crate::digest(&bytes),
            "zero-step compaction lost original raw projection authority"
        );
    }

    #[test]
    fn pending_checkpoint_scope_requires_retained_raw_authority_and_projection() {
        let (fixture, store, mut journal, _) = crate::custody::tests::fixture();
        fs::DirBuilder::new()
            .recursive(true)
            .mode(0o700)
            .create(fixture.0.join("release-transition/v2"))
            .unwrap();
        let catalog = crate::migration_owner::current_catalog().unwrap();
        let raw = include_bytes!("../../config/release-transition.json");
        let registry =
            record::Registry::new(serde_json::from_slice(raw).unwrap(), &catalog).unwrap();
        let registry_digest = crate::digest(raw);
        let base =
            record::Ledger::from_authority(&registry.history().baseline(), &registry).unwrap();
        let before = codec::encode(base.wire(), true).unwrap();
        let projection = record::Projection::from_canonical(before.clone()).unwrap();
        let original =
            record::Checkpoint::build(&base, &registry, &registry_digest, &projection, None, true)
                .unwrap();
        let original_raw = codec::encode(original.wire(), true).unwrap();
        let original_snapshot = Snapshot::captured(Some(original_raw));
        let edge = registry.history().next(base.authority()).unwrap();
        let next = record::Ledger::from_authority(edge.after(), &registry).unwrap();
        let after = codec::encode(next.wire(), true).unwrap();
        let work = vec![Work {
            intent: f::PlannerStepIntent {
                id: format!("{}.ledger", edge.migration().id),
                migration: edge.migration().id.clone(),
                resource: format!("ledger.{}", edge.migration().domain),
                decision: "transform".into(),
                expected: crate::digest(&before),
                desired: crate::digest(&after),
            },
            recipe: Recipe::LedgerAdvance {
                before: Snapshot::captured(Some(before.clone())),
                after: after.clone(),
            },
        }];
        journal.checkpoint = "migrating".into();
        journal.steps = Some(vec![j::Step {
            id: work[0].intent.id.clone(),
            checkpoint: "evidence".into(),
            ..Default::default()
        }]);
        let ledger_path = fixture.0.join("release-transition/v2/ledger.json");
        let checkpoint_path = fixture
            .0
            .join("release-transition/v2/history-checkpoint.json");
        let write = |path: &std::path::Path, value: &[u8]| {
            fs::write(path, value).unwrap();
            fs::set_permissions(path, fs::Permissions::from_mode(0o600)).unwrap();
        };
        write(&ledger_path, &before);
        let advanced = record::Checkpoint::build(
            &next,
            &registry,
            &registry_digest,
            &projection,
            Some(&original),
            false,
        )
        .unwrap();
        let advanced_raw = codec::encode(advanced.wire(), true).unwrap();
        write(&checkpoint_path, &advanced_raw);
        let check = |original: Option<&Snapshot>| {
            retained_scope_source(
                &registry,
                &registry_digest,
                &store.migration_ledger(&registry, &registry_digest).unwrap(),
                &work,
                &journal,
                original,
                &Reader::CanonicalFixture,
                false,
            )
        };
        assert_eq!(
            check(Some(&original_snapshot)).unwrap(),
            original_snapshot.fingerprint()
        );
        assert!(
            check(None).is_err(),
            "old bundle acquired raw checkpoint authority"
        );
        assert!(
            check(Some(&Snapshot::captured(None))).is_err(),
            "checkpoint appeared without original authority"
        );
        let replacement = checkpoint_path.with_extension("replacement");
        write(&replacement, &advanced_raw);
        fs::rename(&replacement, &checkpoint_path).unwrap();
        assert!(
            check(Some(&original_snapshot)).is_ok(),
            "same protected bytes acquired an inode requirement"
        );
        let changed = record::Checkpoint::build(
            &next,
            &registry,
            &registry_digest,
            &projection,
            Some(&original),
            true,
        )
        .unwrap();
        write(
            &checkpoint_path,
            &codec::encode(changed.wire(), true).unwrap(),
        );
        assert!(
            check(Some(&original_snapshot)).is_err(),
            "unapproved prefix compaction accepted"
        );
        write(&checkpoint_path, &advanced_raw);
        write(&ledger_path, &after);
        assert!(
            store.migration_ledger(&registry, &registry_digest).is_err(),
            "changed legacy projection accepted"
        );
    }

    #[test]
    fn pending_ledger_scope_requires_exact_recorded_frontier() {
        let (fixture, store, mut journal, _) = crate::custody::tests::fixture();
        fs::DirBuilder::new()
            .recursive(true)
            .mode(0o700)
            .create(fixture.0.join("release-transition/v2"))
            .unwrap();
        let catalog = crate::migration_owner::current_catalog().unwrap();
        let raw = include_bytes!("../../config/release-transition.json");
        let registry =
            record::Registry::new(serde_json::from_slice(raw).unwrap(), &catalog).unwrap();
        let registry_digest = crate::digest(raw);
        let mut authority = registry.history().baseline();
        let mut previous = Snapshot::captured(None);
        let mut work = vec![];
        for _ in 0..2 {
            let edge = registry.history().next(&authority).unwrap();
            let after = codec::encode(
                record::Ledger::from_authority(edge.after(), &registry)
                    .unwrap()
                    .wire(),
                true,
            )
            .unwrap();
            work.push(Work {
                intent: f::PlannerStepIntent {
                    id: format!("{}.ledger", edge.migration().id),
                    migration: edge.migration().id.clone(),
                    resource: format!("ledger.{}", edge.migration().domain),
                    decision: "transform".into(),
                    expected: previous.fingerprint(),
                    desired: crate::digest(&after),
                },
                recipe: Recipe::LedgerAdvance {
                    before: previous.clone(),
                    after: after.clone(),
                },
            });
            previous = Snapshot::captured(Some(after));
            authority = edge.into_after();
        }
        journal.checkpoint = "migrating".into();
        journal.steps = Some(
            work.iter()
                .map(|w| j::Step {
                    id: w.intent.id.clone(),
                    migration: w.intent.migration.clone(),
                    resource: w.intent.resource.clone(),
                    expected: w.intent.expected.clone(),
                    desired: w.intent.desired.clone(),
                    decision: w.intent.decision.clone(),
                    checkpoint: "intent".into(),
                    ..Default::default()
                })
                .collect(),
        );
        let ledger = fixture.0.join("release-transition/v2/ledger.json");
        let absent = Snapshot::captured(None);
        let check = |journal: &j::Record, work: &[Work]| {
            retained_scope_source(
                &registry,
                &registry_digest,
                &store.migration_ledger(&registry, &registry_digest).unwrap(),
                work,
                journal,
                Some(&absent),
                &Reader::CanonicalFixture,
                false,
            )
        };
        assert_eq!(check(&journal, &work).unwrap(), absent.fingerprint());
        let Recipe::LedgerAdvance { after: first, .. } = &work[0].recipe else {
            panic!()
        };
        fs::write(&ledger, first).unwrap();
        fs::set_permissions(&ledger, fs::Permissions::from_mode(0o600)).unwrap();
        assert!(
            check(&journal, &work).is_err(),
            "uncaptured advance accepted"
        );
        journal.steps.as_mut().unwrap()[0].checkpoint = "evidence".into();
        assert_eq!(
            check(&journal, &work).unwrap(),
            absent.fingerprint(),
            "captured CAS cut lost original authority"
        );
        let Recipe::LedgerAdvance { after: second, .. } = &work[1].recipe else {
            panic!()
        };
        fs::write(&ledger, second).unwrap();
        assert!(
            check(&journal, &work).is_err(),
            "valid future edge without capture accepted"
        );
        journal.steps.as_mut().unwrap()[0].checkpoint = "verified".into();
        journal.steps.as_mut().unwrap()[1].checkpoint = "evidence".into();
        assert!(check(&journal, &work).is_ok());
        work.swap(0, 1);
        assert!(
            check(&journal, &work).is_err(),
            "reordered migration scope accepted"
        );
        work.swap(0, 1);
        if let Recipe::LedgerAdvance { after, .. } = &mut work[0].recipe {
            after.push(b' ');
        }
        assert!(
            check(&journal, &work).is_err(),
            "tampered exact recipe accepted"
        );
    }
}
