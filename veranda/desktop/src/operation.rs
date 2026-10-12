//! An exact owner plan is transient and consumed once. Outcomes survive navigation.
use crate::state::Scope;
use veranda_lib::client::OperationPlan;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Phase {
    Planning,
    Review,
    Starting,
    Running,
    Cancelling,
    Discarding,
    Finished,
}

pub struct Operation {
    pub request: u64,
    pub scope: Scope,
    pub command: String,
    pub phase: Phase,
    pub plan: Option<OperationPlan>,
    pub operation_id: Option<String>,
    pub text: String,
    pub discard_requested: bool,
}

impl Operation {
    pub fn planning(request: u64, scope: Scope, command: String) -> Self {
        Self {
            request,
            scope,
            command,
            phase: Phase::Planning,
            plan: None,
            operation_id: None,
            text: "Preparing the exact owner plan…".into(),
            discard_requested: false,
        }
    }
    pub fn reviewed(&mut self, request: u64, plan: OperationPlan) -> bool {
        if self.request != request || self.phase != Phase::Planning {
            return false;
        }
        self.operation_id = Some(plan.operation_id.clone());
        self.text = format!(
            "{}\nConfirmation: {}\nExpires: {}\n{}",
            plan.summary(),
            plan.confirmation(),
            plan.expires_at(),
            plan.consequences().join("\n")
        );
        for step in plan.steps() {
            self.text.push_str(&format!("\n\n{} · {} · {}\nObserved: {}\nDesired: {}\nPreconditions: {}\nDepends on: {}\nVerify: {}\n{}", step.id(), step.target(), step.decision(), step.observed(), step.desired(), step.preconditions().join("; "), step.depends_on().join(", "), step.verify(), step.consequence().unwrap_or("")));
        }
        self.phase = Phase::Review;
        self.plan = Some(plan);
        true
    }
    pub fn take_review(&mut self) -> Option<OperationPlan> {
        if self.phase != Phase::Review || self.discard_requested {
            return None;
        }
        let plan = self.plan.take()?;
        self.phase = Phase::Starting;
        Some(plan)
    }
    pub fn busy(&self) -> bool {
        self.phase != Phase::Finished
    }
    pub fn terminal(&mut self, id: &str, outcome: &str, message: &str) -> bool {
        if self.operation_id.as_deref() != Some(id)
            || !matches!(
                self.phase,
                Phase::Starting | Phase::Running | Phase::Cancelling
            )
        {
            return false;
        }
        if matches!(outcome, "completed" | "cancelled" | "failed" | "unknown") {
            self.phase = Phase::Finished;
            self.text = format!("{outcome}: {message}");
            true
        } else {
            false
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn late_results_and_interrupted_operations_never_become_success() {
        let mut op = Operation::planning(
            1,
            Scope {
                connection: None,
                yard: None,
            },
            "init".into(),
        );
        op.operation_id = Some("operation-1".into());
        op.phase = Phase::Running;
        assert!(!op.terminal("operation-2", "completed", "wrong owner"));
        assert!(!op.terminal("operation-1", "running", "progress"));
        assert!(op.terminal("operation-1", "unknown", "Connection lost."));
        assert_eq!(op.phase, Phase::Finished);
        assert!(op.text.starts_with("unknown:"));
        assert!(!op.terminal("operation-1", "completed", "late hint"));
        assert!(op.take_review().is_none());
    }
}
