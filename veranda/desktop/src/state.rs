//! Nonsecret presentation drafts. No trust or operation-plan capabilities persist.
use serde::{Deserialize, Serialize};
use veranda_lib::local_fleet::NativeError;

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Scope {
    pub connection: Option<String>,
    pub yard: Option<String>,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Draft {
    pub scope: Scope,
    pub field: String,
    pub value: String,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct OperationNote {
    pub scope: Scope,
    pub owner_id: String,
    pub operation_id: String,
    pub command: String,
    /// Saved before execute is sent; a restart cannot imply a confirmed outcome.
    pub outcome: String,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AppState {
    schema: u32,
    pub drafts: Vec<Draft>,
    pub operations: Vec<OperationNote>,
}

impl Default for AppState {
    fn default() -> Self {
        Self {
            schema: 1,
            drafts: vec![],
            operations: vec![],
        }
    }
}

fn identity(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 128
        && value
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"._-".contains(&b))
}

fn text(value: &str) -> bool {
    value.len() <= 8192
        && nonsecret(value)
        && !value
            .chars()
            .any(|c| c.is_control() && !matches!(c, '\n' | '\r' | '\t'))
}

// A presentation storage boundary, not the owner setting validator. Values
// containing obvious credential material are never written to app-local drafts.
fn nonsecret(value: &str) -> bool {
    let lower = value.to_ascii_lowercase();
    ![
        "-----begin private key-----",
        "-----begin openssh private key-----",
        "password=",
        "\"password\":",
        "passwd=",
        "access_token=",
        "\"access_token\":",
        "api_key=",
        "\"api_key\":",
        "\"apikey\":",
        "client_secret=",
        "\"client_secret\":",
    ]
    .iter()
    .any(|marker| lower.contains(marker))
}
pub const PROFILE_SELECTION: &str = "profiles:selection";
fn profile_list_value(value: &str) -> bool {
    value.split_whitespace().all(identity)
}
impl Scope {
    fn valid(&self) -> bool {
        self.connection.as_deref().is_none_or(identity) && self.yard.as_deref().is_none_or(identity)
    }
}

/// Credential-free Git locations only. Authentication belongs to the owner.
pub fn repository_url(value: &str) -> bool {
    if value.is_empty()
        || value.len() > 1024
        || value.chars().any(|c| c.is_whitespace() || c.is_control())
        || value.contains(['?', '#', '%'])
    {
        return false;
    }
    if let Some(rest) = value.strip_prefix("https://") {
        let authority = rest.split('/').next().unwrap_or("");
        return !authority.is_empty() && !authority.contains('@') && rest.contains('/');
    }
    if let Some(rest) = value.strip_prefix("ssh://") {
        let authority = rest.split('/').next().unwrap_or("");
        return !authority.is_empty()
            && rest.contains('/')
            && authority
                .split('@')
                .next()
                .is_none_or(|user| !user.contains(':'));
    }
    value
        .split_once(':')
        .is_some_and(|(host, path)| host.contains('@') && !host.contains('/') && !path.is_empty())
}

impl AppState {
    pub fn decode(bytes: &[u8]) -> Result<Self, NativeError> {
        if bytes.is_empty() {
            return Ok(Self::default());
        }
        if bytes.len() > 128 * 1024 {
            return Err(invalid());
        }
        let state: Self = serde_json::from_slice(bytes).map_err(|_| invalid())?;
        state.validate()?;
        Ok(state)
    }
    fn validate(&self) -> Result<(), NativeError> {
        if self.schema != 1 || self.drafts.len() > 128 || self.operations.len() > 32 {
            return Err(invalid());
        }
        for (index, draft) in self.drafts.iter().enumerate() {
            let allowed = matches!(
                draft.field.as_str(),
                "create:name"
                    | "create:preset"
                    | "sync:repository"
                    | "remote:destination"
                    | "profiles:selection"
            ) || draft.field.strip_prefix("setting:").is_some_and(|name| {
                identity(name)
                    && ![
                        "PASSWORD",
                        "PASSPHRASE",
                        "PRIVATE_KEY",
                        "ACCESS_TOKEN",
                        "API_KEY",
                        "SECRET",
                    ]
                    .iter()
                    .any(|marker| name.to_ascii_uppercase().contains(marker))
            }) || draft.field.strip_prefix("profile:").is_some_and(identity);
            if !draft.scope.valid()
                || !allowed
                || !text(&draft.value)
                || draft.field == "profiles:selection" && !profile_list_value(&draft.value)
                || draft.field == "sync:repository"
                    && !draft.value.is_empty()
                    && !repository_url(&draft.value)
                || self.drafts[..index]
                    .iter()
                    .any(|other| other.scope == draft.scope && other.field == draft.field)
            {
                return Err(invalid());
            }
        }
        if self.operations.iter().any(|note| {
            !note.scope.valid()
                || !identity(&note.owner_id)
                || !identity(&note.operation_id)
                || !identity(&note.command)
                || !matches!(
                    note.outcome.as_str(),
                    "unknown" | "completed" | "failed" | "cancelled"
                )
        }) {
            return Err(invalid());
        }
        Ok(())
    }
    pub fn encode(&self) -> Result<Vec<u8>, NativeError> {
        self.validate()?;
        let bytes = serde_json::to_vec(self).map_err(|_| invalid())?;
        if bytes.len() > 128 * 1024 {
            return Err(invalid());
        }
        Ok(bytes)
    }
    pub fn value<'a>(&'a self, scope: &Scope, field: &str) -> Option<&'a str> {
        self.drafts
            .iter()
            .find(|draft| &draft.scope == scope && draft.field == field)
            .map(|draft| draft.value.as_str())
    }
    pub fn remember(
        &mut self,
        scope: Scope,
        field: String,
        value: String,
    ) -> Result<(), NativeError> {
        let mut next = self.clone();
        if field == "profiles:selection" {
            // Consolidate only after validating the replacement, and commit atomically.
            if !profile_list_value(&value) {
                return Err(invalid());
            }
            next.drafts
                .retain(|draft| draft.scope != scope || !draft.field.starts_with("profile:"));
        }
        if let Some(draft) = next
            .drafts
            .iter_mut()
            .find(|draft| draft.scope == scope && draft.field == field)
        {
            draft.value = value;
        } else {
            next.drafts.push(Draft {
                scope,
                field,
                value,
            });
        }
        next.encode()?;
        *self = next;
        Ok(())
    }
    pub fn intent(&mut self, note: OperationNote) -> Result<(), NativeError> {
        let mut next = self.clone();
        if next
            .operations
            .iter()
            .any(|old| old.operation_id == note.operation_id)
        {
            return Err(invalid());
        }
        if next.operations.len() == 32 {
            let Some(index) = next
                .operations
                .iter()
                .position(|old| old.outcome != "unknown")
            else {
                return Err(invalid());
            };
            next.operations.remove(index);
        }
        next.operations.push(note);
        next.encode()?;
        *self = next;
        Ok(())
    }
    /// Release only a definitively unsubmitted attempt. Unknown admitted work
    /// remains in history until an authoritative owner outcome is received.
    pub fn not_submitted(&mut self, id: &str, _proof: &crate::worker::NotSubmitted) {
        self.operations.retain(|note| note.operation_id != id);
    }
    pub fn outcome(&mut self, id: &str, outcome: &str) {
        if !matches!(outcome, "completed" | "failed" | "cancelled" | "unknown") {
            return;
        }
        if let Some(note) = self
            .operations
            .iter_mut()
            .find(|note| note.operation_id == id)
        {
            note.outcome = outcome.into();
        }
    }
}

fn invalid() -> NativeError {
    NativeError::new("invalid_desktop_state", "Veranda could not save or restore these nonsecret drafts. Review their values or reopen the application.")
}

#[cfg(test)]
mod tests {
    use super::*;
    fn scope(yard: Option<&str>) -> Scope {
        Scope {
            connection: Some("remote".into()),
            yard: yard.map(str::to_owned),
        }
    }
    #[test]
    fn unicode_multiline_drafts_survive_scope_switch_and_restart() {
        let mut state = AppState::default();
        state
            .remember(
                scope(Some("default")),
                "setting:LABEL".into(),
                "مرحبا\nProject α".into(),
            )
            .unwrap();
        state
            .remember(scope(None), "create:name".into(), "named".into())
            .unwrap();
        let restored = AppState::decode(&state.encode().unwrap()).unwrap();
        assert_eq!(
            restored.value(&scope(Some("default")), "setting:LABEL"),
            Some("مرحبا\nProject α")
        );
        assert_eq!(restored.value(&scope(None), "create:name"), Some("named"));
    }
    #[test]
    fn restart_preserves_unknown_without_any_executable_capability() {
        let mut state = AppState::default();
        state
            .intent(OperationNote {
                scope: scope(None),
                owner_id: "owner".into(),
                operation_id: "operation-1".into(),
                command: "init".into(),
                outcome: "unknown".into(),
            })
            .unwrap();
        assert_eq!(
            AppState::decode(&state.encode().unwrap())
                .unwrap()
                .operations[0]
                .outcome,
            "unknown"
        );
        assert!(AppState::decode(
            br#"{"schema":1,"drafts":[],"operations":[],"plan_id":"capability"}"#
        )
        .is_err());
        state.outcome("operation-1", "running");
        assert_eq!(state.operations[0].outcome, "unknown");
    }

    #[test]
    fn definite_nonstart_releases_history_but_admitted_unknown_survives_restart() {
        let mut state = AppState::default();
        let scope = scope(None);
        let proof = crate::worker::NotSubmitted(NativeError::new(
            "arbitrary_static_error",
            "Not admitted.",
        ));
        for index in 0..40 {
            let id = format!("operation-{index}");
            state
                .intent(OperationNote {
                    scope: scope.clone(),
                    owner_id: "owner".into(),
                    operation_id: id.clone(),
                    command: "init".into(),
                    outcome: "unknown".into(),
                })
                .unwrap();
            state.not_submitted(&id, &proof);
            state = AppState::decode(&state.encode().unwrap()).unwrap();
            assert!(state.operations.is_empty());
        }
        state
            .intent(OperationNote {
                scope,
                owner_id: "owner".into(),
                operation_id: "admitted".into(),
                command: "init".into(),
                outcome: "unknown".into(),
            })
            .unwrap();
        state.outcome("admitted", "unknown");
        let restored = AppState::decode(&state.encode().unwrap()).unwrap();
        assert_eq!(restored.operations.len(), 1);
        assert_eq!(restored.operations[0].outcome, "unknown");
    }
    #[test]
    fn full_unknown_history_and_invalid_intent_are_atomic() {
        let mut state = AppState::default();
        let note = |index| OperationNote {
            scope: scope(None),
            owner_id: "owner".into(),
            operation_id: format!("operation-{index}"),
            command: "init".into(),
            outcome: "unknown".into(),
        };
        for index in 0..32 {
            state.intent(note(index)).unwrap();
        }
        let before = state.encode().unwrap();
        assert!(state.intent(note(32)).is_err());
        assert_eq!(state.encode().unwrap(), before);
        state.outcome("operation-0", "completed");
        let before = state.encode().unwrap();
        let mut invalid = note(32);
        invalid.owner_id = "bad/owner".into();
        assert!(state.intent(invalid).is_err());
        assert_eq!(state.encode().unwrap(), before);
        state.intent(note(32)).unwrap();
        assert_eq!(state.operations.len(), 32);
        assert!(state
            .operations
            .iter()
            .any(|op| op.operation_id == "operation-32" && op.outcome == "unknown"));
    }
    #[test]
    fn credentials_and_invalid_fields_never_enter_persisted_drafts() {
        for url in [
            "https://user:password@example/repo",
            "https://example/repo?token=secret",
            "ssh://user:secret@example/repo",
            "https://user%3Asecret@example/repo",
        ] {
            assert!(!repository_url(url));
        }
        for url in [
            "https://example/repo.git",
            "ssh://git@example/repo.git",
            "git@example:repo.git",
        ] {
            assert!(repository_url(url));
        }
        let mut state = AppState::default();
        assert!(state
            .remember(scope(None), "privateKey".into(), "secret".into())
            .is_err());
        assert!(state
            .remember(
                scope(None),
                "sync:repository".into(),
                "https://example/repo?token=secret".into()
            )
            .is_err());
        assert!(state
            .remember(
                scope(None),
                "setting:LABEL".into(),
                "password=secret".into()
            )
            .is_err());
        assert!(state
            .remember(scope(None), "setting:ACCESS_TOKEN".into(), "opaque".into())
            .is_err());
        assert!(state.drafts.is_empty());
    }
}
