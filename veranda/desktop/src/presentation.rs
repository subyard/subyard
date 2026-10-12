//! Editor lifetime follows deliberate selection, never background inventory freshness.
use crate::state::Scope;
#[derive(Default)]
pub struct FormLifetime {
    key: Option<(u64, Scope)>,
}
impl FormLifetime {
    pub fn change(&mut self, generation: u64, scope: &Scope) -> bool {
        if self
            .key
            .as_ref()
            .is_some_and(|(old_generation, old_scope)| {
                *old_generation == generation && old_scope == scope
            })
        {
            return false;
        }
        self.key = Some((generation, scope.clone()));
        true
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn reads_and_inactive_hints_retain_editors_until_deliberate_navigation() {
        let host = Scope {
            connection: Some("owner".into()),
            yard: None,
        };
        let mut lifetime = FormLifetime::default();
        assert!(lifetime.change(1, &host));
        for _ in 0..100 {
            assert!(!lifetime.change(1, &host));
        }
        assert!(lifetime.change(2, &host)); // Explicit reselection gets new callbacks.
        let yard = Scope {
            connection: host.connection,
            yard: Some("default".into()),
        };
        assert!(lifetime.change(3, &yard));
        assert!(!lifetime.change(3, &yard));
    }
}
