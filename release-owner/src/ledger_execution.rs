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
