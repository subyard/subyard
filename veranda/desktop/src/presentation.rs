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
/// Presentation preserves owner ordering and invisible names; it does not decide policy.
pub fn profile_selection(
    state: &crate::state::AppState,
    scope: &Scope,
    present: bool,
    requested: &[String],
    effective: &[String],
) -> Vec<String> {
    if let Some(value) = state.value(scope, crate::state::PROFILE_SELECTION) {
        // AppState validates the list before admitting it.
        return value.split_whitespace().map(str::to_owned).collect();
    }
    let mut chosen = if present { requested } else { effective }.to_vec();
    for draft in &state.drafts {
        if &draft.scope == scope {
            if let Some(name) = draft.field.strip_prefix("profile:") {
                match draft.value.as_str() {
                    "true" => toggle_profile(&mut chosen, name, true),
                    "false" => toggle_profile(&mut chosen, name, false),
                    _ => (),
                }
            }
        }
    }
    chosen
}
pub fn toggle_profile(chosen: &mut Vec<String>, name: &str, active: bool) {
    if !active {
        chosen.retain(|value| value != name);
    } else if !chosen.iter().any(|value| value == name) {
        chosen.push(name.into());
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

#[cfg(test)]
mod profile_tests {
    use super::*;
    use crate::state::{AppState, PROFILE_SELECTION};
    fn names(values: &[&str]) -> Vec<String> {
        values.iter().map(|v| (*v).into()).collect()
    }
    #[test]
    fn ordered_intent_survives_hidden_catalog_entries_refresh_and_restart() {
        let scope = Scope {
            connection: None,
            yard: Some("default".into()),
        };
        let mut state = AppState::default();
        let requested = names(&["hidden", "a", "a", "filtered"]);
        assert_eq!(
            profile_selection(&state, &scope, true, &requested, &names(&["a"])),
            requested
        );
        assert!(profile_selection(&state, &scope, true, &[], &names(&["default"])).is_empty());
        assert_eq!(
            profile_selection(&state, &scope, false, &[], &names(&["default"])),
            names(&["default"])
        );
        state
            .remember(scope.clone(), "profile:orphan".into(), "true".into())
            .unwrap();
        state
            .remember(scope.clone(), "profile:filtered".into(), "false".into())
            .unwrap();
        let mut chosen = profile_selection(&state, &scope, true, &requested, &[]);
        assert_eq!(chosen, names(&["hidden", "a", "a", "orphan"]));
        toggle_profile(&mut chosen, "a", true); // Existing duplicates survive checking.
        assert_eq!(chosen, names(&["hidden", "a", "a", "orphan"]));
        toggle_profile(&mut chosen, "a", false);
        toggle_profile(&mut chosen, "new", true);
        assert_eq!(chosen, names(&["hidden", "orphan", "new"]));
        state
            .remember(scope.clone(), PROFILE_SELECTION.into(), chosen.join(" "))
            .unwrap();
        let restored = AppState::decode(&state.encode().unwrap()).unwrap();
        assert_eq!(
            profile_selection(&restored, &scope, false, &[], &names(&["changed-default"])),
            chosen
        );
        state
            .remember(scope.clone(), PROFILE_SELECTION.into(), "".into())
            .unwrap();
        assert!(profile_selection(&state, &scope, false, &[], &names(&["default"])).is_empty());
    }
    #[test]
    fn legacy_consolidation_is_atomic_at_storage_limit_and_preserves_other_scopes() {
        let scope = Scope {
            connection: None,
            yard: Some("default".into()),
        };
        let other = Scope {
            connection: None,
            yard: Some("other".into()),
        };
        let mut state = AppState::default();
        state
            .remember(other.clone(), "profile:orphan".into(), "true".into())
            .unwrap();
        state
            .remember(scope.clone(), "setting:LABEL".into(), "keep".into())
            .unwrap();
        for i in 0..126 {
            state
                .remember(scope.clone(), format!("profile:p{i}"), "true".into())
                .unwrap();
        }
        let before = state.encode().unwrap();
        assert!(state
            .remember(scope.clone(), PROFILE_SELECTION.into(), "bad/name".into())
            .is_err());
        assert_eq!(state.encode().unwrap(), before);
        let chosen = profile_selection(&state, &scope, true, &[], &[]);
        state
            .remember(scope.clone(), PROFILE_SELECTION.into(), chosen.join(" "))
            .unwrap();
        assert_eq!(state.drafts.len(), 3);
        assert_eq!(state.value(&other, "profile:orphan"), Some("true"));
        assert_eq!(state.value(&scope, "setting:LABEL"), Some("keep"));
        let before = state.encode().unwrap();
        for value in ["password=secret".to_string(), vec!["a"; 4097].join(" ")] {
            assert!(state
                .remember(scope.clone(), PROFILE_SELECTION.into(), value)
                .is_err());
            assert_eq!(state.encode().unwrap(), before);
        }
    }
    #[test]
    fn long_ordered_drafts_fit_the_original_text_budget_without_json_overhead() {
        let scope = Scope {
            connection: None,
            yard: Some("default".into()),
        };
        for chosen in [vec!["a".to_owned(); 1025], vec!["legacy".to_owned(); 1000]] {
            let mut state = AppState::default();
            state
                .remember(scope.clone(), PROFILE_SELECTION.into(), chosen.join(" "))
                .unwrap();
            let restored = AppState::decode(&state.encode().unwrap()).unwrap();
            let mut edited = profile_selection(&restored, &scope, true, &[], &[]);
            assert_eq!(edited, chosen);
            toggle_profile(&mut edited, &chosen[0], true);
            assert_eq!(edited, chosen);
            toggle_profile(&mut edited, "new", true);
            assert_eq!(&edited[..chosen.len()], &chosen);
            state
                .remember(scope.clone(), PROFILE_SELECTION.into(), edited.join(" "))
                .unwrap();
            let restored = AppState::decode(&state.encode().unwrap()).unwrap();
            assert_eq!(
                profile_selection(&restored, &scope, false, &[], &names(&["changed"])),
                edited
            );
        }
        let mut state = AppState::default();
        let boundary = vec!["a"; 4096].join(" ") + " "; // Exactly8192 bytes, all valid names.
        assert_eq!(boundary.len(), 8192);
        state
            .remember(scope.clone(), PROFILE_SELECTION.into(), boundary.clone())
            .unwrap();
        let before = state.encode().unwrap();
        assert!(state
            .remember(scope.clone(), PROFILE_SELECTION.into(), boundary + "a")
            .is_err());
        assert_eq!(state.encode().unwrap(), before);
    }
}
