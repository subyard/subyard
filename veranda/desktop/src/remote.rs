//! Presentation state for one bounded native trust flow. IDs fence late replies.
use std::sync::Arc;
use veranda_lib::client::{Assessment, ConnectionAttempt};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Phase {
    Entry,
    Assessing,
    Review,
    Connecting,
    Cancelling,
}

pub struct Flow {
    pub id: u64,
    pub phase: Phase,
    pub repair: Option<String>,
    pub review: Option<Assessment>,
    pub attempt: Arc<ConnectionAttempt>,
}

impl Flow {
    pub fn new(id: u64, repair: Option<String>) -> Self {
        Self {
            id,
            phase: Phase::Entry,
            repair,
            review: None,
            attempt: Arc::new(ConnectionAttempt::default()),
        }
    }
    pub fn accepts(&self, id: u64, phase: Phase) -> bool {
        self.id == id && self.phase == phase
    }
    pub fn finish_on_connected(&self, id: u64) -> bool {
        self.accepts(id, Phase::Connecting)
    }
    pub fn reviewed(&mut self, id: u64, review: Assessment) -> bool {
        if !self.accepts(id, Phase::Assessing) {
            return false;
        }
        if !matches!(review.confirmation(), "never" | "prompt-default-yes") {
            return false;
        }
        self.review = Some(review);
        self.phase = Phase::Review;
        true
    }
    pub fn accept(&mut self) -> Option<Assessment> {
        if self.phase != Phase::Review {
            return None;
        }
        let review = self.review.take()?;
        self.phase = Phase::Connecting;
        Some(review)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn committed_connection_cannot_release_flow_before_its_cancel_finishes() {
        let mut flow = Flow::new(1, None);
        flow.phase = Phase::Connecting;
        assert!(flow.finish_on_connected(1));
        flow.phase = Phase::Cancelling;
        assert!(
            !flow.finish_on_connected(1),
            "committed connection released an outstanding cancellation"
        );
        assert!(flow.accepts(1, Phase::Cancelling));
        assert!(!flow.accepts(2, Phase::Cancelling));
    }

    #[test]
    fn late_replies_and_duplicate_accept_are_fenced() {
        let mut flow = Flow::new(2, None);
        flow.phase = Phase::Assessing;
        assert!(!flow.accepts(1, Phase::Assessing));
        assert!(flow.accept().is_none());
        flow.phase = Phase::Cancelling;
        assert!(!flow.accepts(2, Phase::Assessing));
        assert!(!flow.accepts(2, Phase::Connecting));
        assert!(flow.accept().is_none());
    }
}
