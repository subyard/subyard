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

/// Shown when the single owner-command slot refuses a request.
pub const OWNER_BUSY_EXECUTE: &str =
    "Another owner request is queued. Review remains available; try again after it finishes.";
pub const OWNER_BUSY_DISCARD: &str = "Another owner request is queued. The plan was not discarded and remains under review; discard it again after that request finishes.";
pub const OWNER_BUSY_CANCEL: &str = "Another owner request is queued. Cancellation was not sent; try again after that request finishes.";

/// The plan type is generic only so the state transitions can be unit tested
/// without an owner; the desktop always uses the default.
pub struct Operation<P = OperationPlan> {
    pub request: u64,
    pub scope: Scope,
    pub command: String,
    pub phase: Phase,
    pub plan: Option<P>,
    pub operation_id: Option<String>,
    pub text: String,
    pub discard_requested: bool,
}

impl<P> Operation<P> {
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
    /// The reviewed plan Execute may submit. Decide this before admitting the
    /// execute request, so an admitted request always consumes the review.
    pub fn executable_review(&self) -> Option<&P> {
        if self.phase != Phase::Review || self.discard_requested {
            return None;
        }
        self.plan.as_ref()
    }
    pub fn take_review(&mut self) -> Option<P> {
        self.executable_review()?;
        let plan = self.plan.take()?;
        self.phase = Phase::Starting;
        Some(plan)
    }
    /// Returns the plan a Discard request must name now. While planning, the
    /// discard is deferred until the plan arrives.
    pub fn request_discard(&mut self) -> Option<&P> {
        match self.phase {
            Phase::Planning => {
                self.discard_requested = true;
                self.text = "Waiting for the owner plan before discarding it…".into();
                None
            }
            Phase::Review => self.plan.as_ref(),
            _ => None,
        }
    }
    /// A deferred discard that must be sent now that the plan is under review.
    pub fn pending_discard(&self) -> bool {
        self.discard_requested && self.phase == Phase::Review
    }
    pub fn discard_admitted(&mut self) {
        self.plan = None;
        self.phase = Phase::Discarding;
        self.discard_requested = false;
    }
    /// A refused discard leaves an ordinary review the user can act on again.
    pub fn discard_refused(&mut self) -> &'static str {
        self.discard_requested = false;
        OWNER_BUSY_DISCARD
    }
    pub fn cancel_target(&self) -> Option<&str> {
        if self.phase != Phase::Running {
            return None;
        }
        self.operation_id.as_deref()
    }
    pub fn cancel_admitted(&mut self) {
        self.phase = Phase::Cancelling;
        self.text = "Requesting owner cancellation…".into();
    }
    pub fn cancel_refused(&self) -> &'static str {
        OWNER_BUSY_CANCEL
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

impl Operation<OperationPlan> {
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
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn late_results_and_interrupted_operations_never_become_success() {
        let mut op: Operation = Operation::planning(
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

    fn reviewing() -> Operation<&'static str> {
        let mut op = Operation::planning(
            1,
            Scope {
                connection: None,
                yard: None,
            },
            "init".into(),
        );
        op.phase = Phase::Review;
        op.plan = Some("plan-1");
        op.operation_id = Some("operation-1".into());
        op
    }

    #[test]
    fn execute_is_decided_before_admission_and_consumes_the_review() {
        let mut op = reviewing();
        assert_eq!(op.executable_review(), Some(&"plan-1"));
        assert_eq!(op.take_review(), Some("plan-1"));
        assert_eq!(op.phase, Phase::Starting);
        assert!(op.executable_review().is_none());

        let mut op = reviewing();
        op.phase = Phase::Planning;
        assert!(op.request_discard().is_none());
        op.phase = Phase::Review;
        assert!(op.pending_discard());
        assert!(op.executable_review().is_none());
        assert!(op.take_review().is_none());
        assert_eq!(op.phase, Phase::Review);
    }

    #[test]
    fn refused_deferred_discard_returns_to_an_ordinary_review() {
        let mut op = reviewing();
        op.phase = Phase::Planning;
        assert!(op.request_discard().is_none());
        op.phase = Phase::Review;
        assert!(op.pending_discard());
        assert_eq!(op.request_discard(), Some(&"plan-1"));
        assert_eq!(op.discard_refused(), OWNER_BUSY_DISCARD);
        assert!(!op.pending_discard());
        assert_eq!(op.phase, Phase::Review);
        assert_eq!(op.executable_review(), Some(&"plan-1"));
        assert_eq!(op.request_discard(), Some(&"plan-1"));
        op.discard_admitted();
        assert_eq!(op.phase, Phase::Discarding);
        assert!(op.plan.is_none() && !op.discard_requested);
        assert!(op.executable_review().is_none());
    }

    #[test]
    fn cancellation_targets_only_a_running_operation_and_reports_refusal() {
        let mut op = reviewing();
        assert!(op.cancel_target().is_none());
        assert!(op.take_review().is_some());
        assert!(op.cancel_target().is_none());
        op.phase = Phase::Running;
        assert_eq!(op.cancel_target(), Some("operation-1"));
        assert_eq!(op.cancel_refused(), OWNER_BUSY_CANCEL);
        assert_eq!(op.phase, Phase::Running);
        op.cancel_admitted();
        assert_eq!(op.phase, Phase::Cancelling);
        assert!(op.cancel_target().is_none());
    }
}
