//! Separate checked recovery contracts. Evidence of an old grant is not a new grant.
use crate::{
    codec, digest,
    record::{self, fingerprint, require, safe_id, token},
    wire::{
        journal_v2 as j, lifecycle_v1 as l, process_v1 as p, process_v2 as q, recovery_v1 as r,
    },
    Error, Result,
};
use serde::Serialize;
pub const ACTIVATION_V1: &str = "activation-only-replacement-v1";
pub const ACTIVATION_V2: &str = "activation-only-replacement-v2";
pub const MIGRATION_OWNER: &str = "migration-ledger-owner-replacement-v1";
pub fn recovery_transaction(s: &str) -> bool {
    s.starts_with("recovery-v1-") || s.starts_with("recovery-v2-")
}
pub(crate) fn selection(s: &r::ActivationOnlyRecoveryRequest) -> Result<()> {
    require(safe_id(&s.transaction) && fingerprint(&s.fingerprint))
}
pub(crate) fn links(s: &r::ReleaseLinks) -> Result<()> {
    require(
        safe_id(&s.active)
            && s.previous
                .as_ref()
                .is_none_or(|p| safe_id(p) && p != &s.active),
    )
}
pub fn lifecycle_transaction(
    s: &r::ActivationOnlyRecoveryRequest,
    generation: u64,
) -> Result<String> {
    selection(s)?;
    require(generation > 0)?;
    Ok(format!("recovery-v2-{}-{generation:x}", s.fingerprint))
}
pub fn bind_lifecycle_plan(base: &str, reservation: &str) -> Result<String> {
    require(token(base, "plan-v1-"))?;
    if reservation.is_empty() {
        return Ok(base.into());
    }
    require(fingerprint(reservation))?;
    #[derive(Serialize)]
    struct Binding<'a> {
        contract: &'a str,
        base: &'a str,
        reservation: &'a str,
    }
    Ok(format!(
        "plan-v1-{}",
        digest(&codec::encode(
            &Binding {
                contract: ACTIVATION_V2,
                base,
                reservation
            },
            false
        )?)
    ))
}
pub struct Receipt {
    wire: r::RecoveryReceiptV1,
}
impl Receipt {
    pub fn new(wire: r::RecoveryReceiptV1) -> Result<Self> {
        let prefix = if wire.contract == ACTIVATION_V2 && wire.schema_version == 2 {
            require(
                lifecycle_transaction(&wire.replacement, wire.generation)?
                    == wire.successor.transaction
                    && bind_lifecycle_plan(&wire.base_plan, &wire.reservation)?
                        == wire.successor.authorization_plan,
            )?;
            "recovery-v2-"
        } else {
            require(
                wire.schema_version == 1
                    && wire.contract == ACTIVATION_V1
                    && wire.base_plan.is_empty()
                    && wire.reservation.is_empty()
                    && wire.generation == 0,
            )?;
            "recovery-v1-"
        };
        selection(&wire.replacement)?;
        let before = record::Journal::new(wire.predecessor.clone())?;
        let after = record::Journal::new(wire.successor.clone())?;
        let b = before.wire();
        let a = after.wire();
        require(
            b.checkpoint == "reconciling"
                && b.goal.direction == "activate-target"
                && b.source_ingress.is_none()
                && crate::wire::empty_vec(&b.steps)
                && wire.replacement.transaction == b.transaction
                && digest(&codec::encode(b, true)?) == wire.replacement.fingerprint,
        )?;
        links(&wire.links)?;
        require(
            wire.links.active == b.releases.target
                && if b.releases.from == b.releases.target {
                    wire.links.previous == b.releases.previous
                } else {
                    wire.links.previous.as_deref() == Some(&b.releases.from)
                },
        )?;
        require(
            a.checkpoint == "authorized"
                && a.source_ingress.is_none()
                && crate::wire::empty_vec(&a.steps)
                && a.goal == b.goal
                && a.releases.from == wire.links.active
                && a.releases.target == wire.links.active
                && a.releases.previous == wire.links.previous
                && a.transaction != b.transaction
                && a.transaction.starts_with(prefix)
                && !recovery_transaction(&b.transaction)
                && a.authorization_plan != b.authorization_plan
                && a.resume_plan != b.resume_plan
                && a.authorization_digest != b.authorization_digest
                && a.observation_scope != b.observation_scope,
        )?;
        require(
            b.artifact_digest == a.artifact_digest
                && b.registry_digest == a.registry_digest
                && b.catalog_digest == a.catalog_digest
                && wire.owner.release == a.goal.target
                && wire.owner.artifact == a.artifact_digest
                && wire.owner.registry == a.registry_digest
                && wire.owner.catalog == a.catalog_digest
                && fingerprint(&wire.ledger),
        )?;
        let native = wire.native_plans.as_deref().unwrap_or_default();
        require(!native.is_empty() && native.len() <= 256)?;
        for (i, n) in native.iter().enumerate() {
            require(
                safe_id(&n.id)
                    && (i == 0 || native[i - 1].id < n.id)
                    && [&n.binding, &n.actual, &n.desired]
                        .into_iter()
                        .all(|s| fingerprint(s))
                    && (!n.converged || n.actual == n.desired),
            )?;
        }
        codec::encode(&wire, true)?;
        Ok(Self { wire })
    }
    pub fn wire(&self) -> &r::RecoveryReceiptV1 {
        &self.wire
    }
    pub fn matches_successor(&self, current: &j::Record) -> Result<()> {
        record::Journal::new(current.clone())?;
        let mut current = current.clone();
        current.checkpoint = self.wire.successor.checkpoint.clone();
        require(codec::encode(&current, true)? == codec::encode(&self.wire.successor, true)?)
    }
}
pub struct Terminal {
    wire: l::RecoveryLifecycleRecord,
    receipt: Receipt,
}
impl Terminal {
    pub fn new(wire: l::RecoveryLifecycleRecord) -> Result<Self> {
        require(
            wire.schema_version == 1
                && usize::from(wire.completed.is_some())
                    + usize::from(wire.cancellation.is_some())
                    + usize::from(wire.predecessor_completed.is_some())
                    == 1,
        )?;
        let receipt = Receipt::new(wire.receipt.clone())?;
        if let Some(journal) = &wire.completed {
            require(journal.checkpoint == "complete")?;
            receipt.matches_successor(journal)?;
        } else if let Some(journal) = &wire.predecessor_completed {
            record::Journal::new(journal.clone())?;
            require(journal.checkpoint == "complete")?;
            let mut bound = journal.clone();
            bound.checkpoint = wire.receipt.predecessor.checkpoint.clone();
            require(
                codec::encode(&bound, true)? == codec::encode(&wire.receipt.predecessor, true)?,
            )?;
        } else {
            let cancellation = wire.cancellation.as_ref().ok_or(Error::Invalid)?;
            require(
                wire.receipt.contract == ACTIVATION_V2
                    && cancellation.base_plan != wire.receipt.base_plan
                    && bind_lifecycle_plan(
                        &cancellation.base_plan,
                        &digest(&codec::encode(&wire.receipt, true)?),
                    )? == cancellation.plan
                    && fingerprint(&cancellation.authorization_digest),
            )?;
        }
        codec::encode(&wire, true)?;
        Ok(Self { wire, receipt })
    }
    pub fn wire(&self) -> &l::RecoveryLifecycleRecord {
        &self.wire
    }
    pub fn receipt(&self) -> &Receipt {
        &self.receipt
    }
}
pub struct Frontier {
    wire: l::RecoveryFrontier,
}
impl Frontier {
    pub fn new(wire: l::RecoveryFrontier) -> Result<Self> {
        require(wire.schema_version == 1 && wire.generation > 0)?;
        selection(&wire.replacement)?;
        if wire.generation == 1 {
            require(wire.cancellation.is_none())?;
        } else {
            let terminal = wire.cancellation.as_ref().ok_or(Error::Invalid)?;
            require(
                terminal.cancellation.is_some()
                    && terminal.receipt.generation == wire.generation - 1
                    && terminal.receipt.replacement == wire.replacement,
            )?;
            Terminal::new(terminal.clone())?;
        }
        codec::encode(&wire, true)?;
        Ok(Self { wire })
    }
    pub fn wire(&self) -> &l::RecoveryFrontier {
        &self.wire
    }
    pub fn reservation(&self) -> Result<String> {
        self.wire
            .cancellation
            .as_ref()
            .map_or(Ok(String::new()), |t| {
                Ok(digest(&codec::encode(&t.receipt, true)?))
            })
    }
    pub fn after_cancellation(self, terminal: Terminal) -> Result<Self> {
        require(
            terminal.wire.cancellation.is_some()
                && terminal.wire.receipt.replacement == self.wire.replacement
                && terminal.wire.receipt.generation == self.wire.generation
                && terminal.wire.receipt.reservation == self.reservation()?,
        )?;
        Self::new(l::RecoveryFrontier {
            schema_version: 1,
            replacement: self.wire.replacement,
            generation: self.wire.generation.checked_add(1).ok_or(Error::Invalid)?,
            cancellation: Some(terminal.wire),
        })
    }
}
pub struct ProcessRequest {
    wire: q::RecoveryProcessRequest,
}
impl ProcessRequest {
    pub fn new(wire: q::RecoveryProcessRequest) -> Result<Self> {
        require(wire.schema_version == 2)?;
        if matches!(
            wire.mode.as_str(),
            "capabilities"
                | "lifecycle-capabilities"
                | "runtime-activation-capabilities"
                | "migration-owner-capabilities"
        ) {
            require(
                wire.runtime_root.is_empty()
                    && wire.config_home.is_empty()
                    && wire.yard.is_empty()
                    && wire.target.is_empty()
                    && wire.direction.is_empty()
                    && wire.artifact_digest.is_empty()
                    && wire.registry_digest.is_empty()
                    && crate::wire::empty_vec(&wire.inherited_setting_ids)
                    && wire.recovery.is_none()
                    && wire.execution.is_none()
                    && wire.contract.is_empty()
                    && wire.migration_owner.is_none(),
            )?;
        } else {
            require(
                matches!(wire.mode.as_str(), "inspect" | "converge")
                    && matches!(
                        wire.contract.as_str(),
                        "" | ACTIVATION_V1 | ACTIVATION_V2 | MIGRATION_OWNER
                    )
                    && wire.direction == "activate-target"
                    && record::path(&wire.runtime_root)
                    && record::path(&wire.config_home)
                    && fingerprint(&wire.registry_digest),
            )?;
            if wire.contract == MIGRATION_OWNER {
                require(wire.recovery.is_none())?;
                crate::migration_owner::selection(
                    wire.migration_owner.as_ref().ok_or(Error::Invalid)?,
                )?;
            } else {
                require(wire.migration_owner.is_none())?;
                selection(wire.recovery.as_ref().ok_or(Error::Invalid)?)?;
            }
            record::Request::new(p::Request {
                schema_version: 1,
                mode: wire.mode.clone(),
                runtime_root: wire.runtime_root.clone(),
                config_home: wire.config_home.clone(),
                yard: wire.yard.clone(),
                target: wire.target.clone(),
                direction: wire.direction.clone(),
                artifact_digest: wire.artifact_digest.clone(),
                registry_digest: wire.registry_digest.clone(),
                inherited_setting_ids: wire.inherited_setting_ids.clone(),
                execution: wire.execution.clone(),
                ..Default::default()
            })?;
            if let Some(e) = &wire.execution {
                require(token(&e.plan, "plan-v1-") && record::text(&e.authorization, 512, false))?;
            }
        }
        let result = Self { wire };
        result.bytes()?;
        Ok(result)
    }
    pub fn wire(&self) -> &q::RecoveryProcessRequest {
        &self.wire
    }
    pub fn bytes(&self) -> Result<Vec<u8>> {
        if self.wire.mode == "runtime-activation-capabilities" {
            #[derive(Serialize)]
            struct Probe<'a> {
                #[serde(flatten)]
                request: &'a q::RecoveryProcessRequest,
                contract: &'a str,
            }
            codec::encode(
                &Probe {
                    request: &self.wire,
                    contract: "",
                },
                false,
            )
        } else {
            codec::encode(&self.wire, false)
        }
    }
}
pub struct ProcessResponse {
    wire: q::RecoveryProcessResponse,
}
impl ProcessResponse {
    pub fn new(wire: q::RecoveryProcessResponse) -> Result<Self> {
        require(wire.schema_version == 2)?;
        let owners = wire.activation_owners.as_deref().unwrap_or_default();
        if !owners.is_empty() {
            require(
                owners.len() <= 128
                    && wire.capabilities.is_none()
                    && wire.inspection.is_none()
                    && wire.outcome.is_none()
                    && !wire.activation_reconciliation_owned
                    && owners.iter().all(|s| safe_id(s))
                    && owners.windows(2).all(|p| p[0] < p[1]),
            )?;
        } else if let Some(c) = &wire.capabilities {
            require(
                wire.inspection.is_none()
                    && wire.outcome.is_none()
                    && !wire.activation_reconciliation_owned
                    && c.process_schema == 2
                    && c.journal_schema == 2
                    && matches!(
                        (c.contract.as_str(), c.receipt_schema),
                        (ACTIVATION_V1, 1) | (ACTIVATION_V2, 2) | (MIGRATION_OWNER, 1)
                    ),
            )?;
        } else {
            record::Response::new(p::Response {
                schema_version: 1,
                activation_reconciliation_owned: wire.activation_reconciliation_owned,
                inspection: wire.inspection.clone(),
                outcome: wire.outcome.clone(),
            })?;
        }
        codec::encode(&wire, false)?;
        Ok(Self { wire })
    }
    pub fn wire(&self) -> &q::RecoveryProcessResponse {
        &self.wire
    }
}
