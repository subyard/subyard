//! Veranda owns this store; it never reads or writes the Yard CLI controller store.
use crate::local_fleet::NativeError;
use crate::ssh::{self, Endpoint, HostKey};
use serde::{Deserialize, Serialize};
use std::collections::{HashMap, HashSet};
use std::fs::{self, File, OpenOptions};
use std::io::{Read, Write};
use std::path::{Path, PathBuf};
use std::process::Command;
use std::time::{Duration, Instant};

const STORE_LIMIT: usize = 1024 * 1024;
const CONNECTION_LIMIT: usize = 128;
const ASSESSMENT_LIMIT: usize = 32;
const ASSESSMENT_TTL: Duration = Duration::from_secs(120);
const STORE_FILE: &str = "connections.json";

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct ConnectionSummary {
    pub id: String,
    pub host_id: String,
    pub destination: String,
    pub auth_label: String,
}

#[derive(Clone, Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct TrustAssessment {
    pub token: String,
    pub destination: String,
    pub fingerprint: String,
    pub previous_fingerprint: Option<String>,
    pub confirmation_policy: String,
    pub consequences: Vec<String>,
}

#[derive(Clone, Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct RemoveAssessment {
    pub token: String,
    pub connection: ConnectionSummary,
    pub confirmation_policy: String,
    pub consequences: Vec<String>,
}

#[derive(Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields, rename_all = "camelCase")]
struct Record {
    id: String,
    host_id: String,
    destination: String,
    auth: AuthReference,
    public_host_key: String,
    fingerprint: String,
}

// Credential references stay native. Agent uses the OS-managed SSH_AUTH_SOCK;
// no private key, passphrase, token, or arbitrary key path crosses IPC.
#[derive(Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "camelCase", deny_unknown_fields)]
enum AuthReference {
    Agent,
}

impl Record {
    fn summary(&self) -> ConnectionSummary {
        ConnectionSummary {
            id: self.id.clone(),
            host_id: self.host_id.clone(),
            destination: self.destination.clone(),
            auth_label: "SSH agent".into(),
        }
    }
}

#[derive(Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields, rename_all = "camelCase")]
struct StoreData {
    schema_version: u32,
    revision: u64,
    connections: Vec<Record>,
    selected_connection_id: Option<String>,
}
impl Default for StoreData {
    fn default() -> Self {
        Self {
            schema_version: 1,
            revision: 0,
            connections: Vec::new(),
            selected_connection_id: None,
        }
    }
}

struct Pending {
    expires: Instant,
    baseline: Vec<u8>,
    action: Action,
}
enum Action {
    Trust {
        endpoint: Endpoint,
        key: HostKey,
        repair: Option<Record>,
        confirmation_required: bool,
    },
    Remove(Record),
}

pub struct ConnectionStore {
    root: PathBuf,
    pending: HashMap<String, Pending>,
}

/// A consumed, native-only consent capability. Dropping it cleans the temporary
/// pin, including after an unsuccessful SSH/RPC negotiation.
pub struct ConsentedConnection {
    root: PathBuf,
    pin_file: PathBuf,
    alias: String,
    endpoint: Endpoint,
    key: HostKey,
    baseline: Vec<u8>,
    repair: Option<Record>,
    expires: Instant,
    registration_allowed: bool,
}
impl ConsentedConnection {
    fn verify_pin(&self, path: &Path) -> Result<(), NativeError> {
        if Instant::now() >= self.expires && self.registration_allowed {
            return Err(expired());
        }
        let parent = path.parent().ok_or_else(unsafe_store)?;
        check_ancestors(parent)?;
        check_private_dir(parent)?;
        let mut pin = open_private_read(path)?;
        let mut bytes = Vec::new();
        Read::by_ref(&mut pin)
            .take(16 * 1024)
            .read_to_end(&mut bytes)
            .map_err(|_| store_io())?;
        if bytes != self.public_pin().as_bytes() {
            return Err(NativeError::new(
                "changed_session_pin",
                "The SSH session pin changed after acceptance. Review the connection again.",
            ));
        }
        Ok(())
    }
    pub fn public_pin(&self) -> String {
        format!("{} {}\n", self.alias, self.key.key)
    }
    pub fn command(&self, yard: Option<&str>) -> Result<Command, NativeError> {
        self.verify_pin(&self.pin_file)?;
        ssh::rpc_command(
            &self.endpoint,
            &self.pin_file,
            &self.alias,
            &self.key.key,
            yard,
        )
    }
    pub fn owner_command(
        &self,
        owner_arguments: &[String],
        tty: bool,
    ) -> Result<Command, NativeError> {
        self.owner_command_with_pin(owner_arguments, tty, &self.pin_file)
    }
    pub fn owner_command_with_pin(
        &self,
        owner_arguments: &[String],
        tty: bool,
        pin_file: &Path,
    ) -> Result<Command, NativeError> {
        self.verify_pin(pin_file)?;
        ssh::owner_command(
            &self.endpoint,
            pin_file,
            &self.alias,
            &self.key.key,
            owner_arguments,
            tty,
        )
    }
    #[cfg(test)]
    pub fn proxy_command(&self, port: u16) -> Result<Command, NativeError> {
        self.proxy_command_with_pin(port, &self.pin_file)
    }
    pub fn proxy_command_with_pin(
        &self,
        port: u16,
        pin_file: &Path,
    ) -> Result<Command, NativeError> {
        self.verify_pin(pin_file)?;
        ssh::proxy_command(&self.endpoint, pin_file, &self.alias, &self.key.key, port)
    }
}
impl Drop for ConsentedConnection {
    fn drop(&mut self) {
        if self.registration_allowed {
            let _ = fs::remove_file(&self.pin_file);
        }
    }
}

impl ConnectionStore {
    pub fn new(root: PathBuf) -> Self {
        Self {
            root,
            pending: HashMap::new(),
        }
    }

    pub fn list(&self) -> Result<Vec<ConnectionSummary>, NativeError> {
        Ok(self
            .read()?
            .1
            .connections
            .iter()
            .map(Record::summary)
            .collect())
    }

    /// Keyscan and assessment are read-only: no store, pin, registration or
    /// authentication attempt is made before explicit acceptance.
    pub fn prepare_trust(
        &mut self,
        destination: &str,
        repair_id: Option<&str>,
    ) -> Result<TrustAssessment, NativeError> {
        let endpoint = Endpoint::parse(destination)?;
        if repair_id.is_some_and(|id| !safe_id(id)) {
            return Err(not_found());
        }
        let (baseline, data) = self.read()?;
        let key = ssh::assess_host_key(&endpoint)?;
        self.prepare_with_key(endpoint, key, repair_id, baseline, data)
    }

    fn prepare_with_key(
        &mut self,
        endpoint: Endpoint,
        key: HostKey,
        repair_id: Option<&str>,
        baseline: Vec<u8>,
        data: StoreData,
    ) -> Result<TrustAssessment, NativeError> {
        let existing = data
            .connections
            .iter()
            .find(|record| record.destination == endpoint.destination);
        let repair = if let Some(id) = repair_id {
            let record = data
                .connections
                .iter()
                .find(|record| record.id == id)
                .ok_or_else(not_found)?;
            if record.destination != endpoint.destination {
                return Err(NativeError::new(
                    "repair_endpoint_mismatch",
                    "Host-key repair must use the registered endpoint.",
                ));
            }
            Some(record.clone())
        } else {
            None
        };
        if existing.is_some_and(|record| record.public_host_key != key.key) && repair.is_none() {
            return Err(NativeError::new("host_key_changed", "The registered SSH host key changed. Verify the replacement independently, then use Repair host key."));
        }
        let previous_fingerprint = repair
            .as_ref()
            .or(existing)
            .map(|record| record.fingerprint.clone());
        let confirmation_required = existing.is_none_or(|record| record.public_host_key != key.key);
        let consequences = if repair.is_some() && confirmation_required {
            vec!["Replace the pinned SSH host key only after a compatible RPC session confirms the same owner host identity.".into(), "Verify this replacement fingerprint independently before accepting.".into()]
        } else if confirmation_required {
            vec!["Trust this public SSH host key for this endpoint.".into(), "Register the owner host only after SSH authentication and compatible Yard RPC negotiation succeed.".into(), "Verify the fingerprint independently before accepting.".into()]
        } else {
            vec!["Connect using the previously accepted SSH host key.".into()]
        };
        let token = self.insert_pending(Pending {
            expires: Instant::now() + ASSESSMENT_TTL,
            baseline,
            action: Action::Trust {
                endpoint: endpoint.clone(),
                key: key.clone(),
                repair,
                confirmation_required,
            },
        })?;
        Ok(TrustAssessment {
            token,
            destination: endpoint.destination,
            fingerprint: key.fingerprint,
            previous_fingerprint,
            confirmation_policy: if confirmation_required {
                "prompt-default-yes"
            } else {
                "never"
            }
            .into(),
            consequences,
        })
    }

    #[cfg(test)]
    pub(crate) fn assess_test_key(
        &mut self,
        destination: &str,
        key: HostKey,
        repair_id: Option<&str>,
    ) -> Result<TrustAssessment, NativeError> {
        let (baseline, data) = self.read()?;
        self.prepare_with_key(
            Endpoint::parse(destination)?,
            key,
            repair_id,
            baseline,
            data,
        )
    }

    pub fn consented_ssh_command(
        &mut self,
        token: &str,
        confirmed: bool,
        fingerprint: &str,
    ) -> Result<ConsentedConnection, NativeError> {
        let pending = self.consume(token)?;
        let Action::Trust {
            endpoint,
            key,
            repair,
            confirmation_required,
        } = pending.action
        else {
            return Err(invalid_assessment());
        };
        if key.fingerprint != fingerprint {
            return Err(invalid_assessment());
        }
        if confirmation_required && !confirmed {
            return Err(NativeError::new(
                "consent_declined",
                "The connection was not accepted. No host was registered.",
            ));
        }
        if self.read()?.0 != pending.baseline {
            return Err(stale());
        }
        self.make_session(
            endpoint,
            key,
            pending.baseline,
            repair,
            pending.expires,
            true,
        )
    }

    /// Reconnect is a bounded session using an already stored pin; it never
    /// scans, accepts or updates host keys.
    pub fn registered_ssh_command(&self, id: &str) -> Result<ConsentedConnection, NativeError> {
        self.registered_ssh_command_checked(id, None)
    }

    /// Bind a launch pin to the registration that authenticated its owner RPC.
    /// Compare and recapture under one store lock, never by ID alone.
    pub fn registered_ssh_command_bound(
        &self,
        bound: &ConsentedConnection,
    ) -> Result<ConsentedConnection, NativeError> {
        let record = self.bound_record(bound)?;
        self.registered_ssh_command_checked(&record.id, Some(record))
    }

    pub fn validate_registered_session(
        &self,
        bound: &ConsentedConnection,
    ) -> Result<(), NativeError> {
        let expected = self.bound_record(bound)?;
        let _lock = StoreLock::acquire(&self.root)?;
        if self
            .read()?
            .1
            .connections
            .iter()
            .any(|record| record == expected)
        {
            Ok(())
        } else {
            Err(changed_registration())
        }
    }

    fn bound_record<'a>(&self, bound: &'a ConsentedConnection) -> Result<&'a Record, NativeError> {
        if bound.root != self.root || bound.registration_allowed {
            return Err(changed_registration());
        }
        bound.repair.as_ref().ok_or_else(changed_registration)
    }

    fn registered_ssh_command_checked(
        &self,
        id: &str,
        expected: Option<&Record>,
    ) -> Result<ConsentedConnection, NativeError> {
        if !safe_id(id) {
            return Err(not_found());
        }
        // Do not create even a lock for an absent registration. Re-read under
        // the lock before deriving a stable pin from currently saved trust.
        if !self
            .read()?
            .1
            .connections
            .iter()
            .any(|record| record.id == id)
        {
            return Err(if expected.is_some() {
                changed_registration()
            } else {
                not_found()
            });
        }
        let _lock = StoreLock::acquire(&self.root)?;
        let (baseline, data) = self.read()?;
        let record = data
            .connections
            .into_iter()
            .find(|record| record.id == id)
            .ok_or_else(|| {
                if expected.is_some() {
                    changed_registration()
                } else {
                    not_found()
                }
            })?;
        if expected.is_some_and(|expected| expected != &record) {
            return Err(changed_registration());
        }
        let alias = format!("veranda-stored-{}", record.id);
        let session = ConsentedConnection {
            root: self.root.clone(),
            pin_file: self.root.join(format!("{alias}.known_hosts")),
            alias,
            endpoint: Endpoint::parse(&record.destination)?,
            key: HostKey {
                key: record.public_host_key.clone(),
                fingerprint: record.fingerprint.clone(),
            },
            baseline,
            repair: Some(record),
            expires: Instant::now() + ASSESSMENT_TTL,
            registration_allowed: false,
        };
        atomic_private_replace(&session.pin_file, session.public_pin().as_bytes())?;
        session.command(None)?;
        Ok(session)
    }

    fn make_session(
        &self,
        endpoint: Endpoint,
        key: HostKey,
        baseline: Vec<u8>,
        repair: Option<Record>,
        expires: Instant,
        registration_allowed: bool,
    ) -> Result<ConsentedConnection, NativeError> {
        ensure_private_root(&self.root)?;
        let alias = format!("veranda-{}", random_id()?);
        let pin_file = self.root.join(format!("{alias}.known_hosts"));
        let session = ConsentedConnection {
            root: self.root.clone(),
            pin_file,
            alias,
            endpoint,
            key,
            baseline,
            repair,
            expires,
            registration_allowed,
        };
        let bytes = format!("{} {}\n", session.alias, session.key.key);
        let mut file = create_private_file(&session.pin_file)?;
        file.write_all(bytes.as_bytes())
            .and_then(|_| file.sync_all())
            .map_err(|_| store_io())?;
        check_private_file(&session.pin_file)?;
        // Check the command's path/arguments while the temporary pin is owned.
        session.command(None)?;
        Ok(session)
    }

    /// Call only after the typed RPC client has negotiated compatibility and
    /// returned the authoritative owner inventory HostID over the pinned SSH.
    pub fn finalize_registration(
        &mut self,
        session: ConsentedConnection,
        authoritative_host_id: &str,
    ) -> Result<ConnectionSummary, NativeError> {
        if session.root != self.root || !session.registration_allowed {
            return Err(invalid_assessment());
        }
        if Instant::now() >= session.expires {
            return Err(expired());
        }
        if !safe_id(authoritative_host_id) {
            return Err(NativeError::new(
                "invalid_owner_identity",
                "Yard returned an invalid owner host identity.",
            ));
        }
        if session
            .repair
            .as_ref()
            .is_some_and(|record| record.host_id != authoritative_host_id)
        {
            return Err(NativeError::new("repair_identity_mismatch", "The replacement SSH key belongs to a different owner host. The registered trust was preserved."));
        }
        let _lock = StoreLock::acquire(&self.root)?;
        let (baseline, mut data) = self.read()?;
        if baseline != session.baseline {
            return Err(stale());
        }
        if let Some(existing) = data.connections.iter().find(|record| {
            record.host_id == authoritative_host_id
                || record.destination == session.endpoint.destination
        }) {
            if session.repair.is_none()
                && existing.host_id == authoritative_host_id
                && existing.destination == session.endpoint.destination
                && existing.public_host_key == session.key.key
            {
                return Ok(existing.summary());
            }
            if session
                .repair
                .as_ref()
                .is_none_or(|repair| repair.id != existing.id)
            {
                return Err(NativeError::new("connection_exists", "This owner host or endpoint is already registered. Use its existing connection or repair it."));
            }
        }
        let record = Record {
            id: session
                .repair
                .as_ref()
                .map(|record| record.id.clone())
                .unwrap_or(random_id()?),
            host_id: authoritative_host_id.into(),
            destination: session.endpoint.destination.clone(),
            auth: AuthReference::Agent,
            public_host_key: session.key.key.clone(),
            fingerprint: session.key.fingerprint.clone(),
        };
        if let Some(existing) = data
            .connections
            .iter_mut()
            .find(|existing| existing.id == record.id)
        {
            *existing = record.clone();
        } else {
            if data.connections.len() >= CONNECTION_LIMIT {
                return Err(NativeError::new(
                    "connection_limit",
                    "Remove an unused connection before registering another host.",
                ));
            }
            data.connections.push(record.clone());
        }
        data.selected_connection_id = Some(record.id.clone());
        if session.repair.is_some() {
            self.revoke_editor_profiles_locked(&record.id)?;
            self.revoke_registered_pin_locked(&record.id)?;
        }
        self.write(&mut data)?;
        let verified = self
            .read()?
            .1
            .connections
            .into_iter()
            .find(|verified| verified.id == record.id && verified == &record)
            .ok_or_else(store_io)?;
        Ok(verified.summary())
    }

    pub fn prepare_remove(&mut self, id: &str) -> Result<RemoveAssessment, NativeError> {
        if !safe_id(id) {
            return Err(not_found());
        }
        let (baseline, data) = self.read()?;
        let record = data
            .connections
            .into_iter()
            .find(|record| record.id == id)
            .ok_or_else(not_found)?;
        let summary = record.summary();
        let token = self.insert_pending(Pending {
            expires: Instant::now() + ASSESSMENT_TTL,
            baseline,
            action: Action::Remove(record),
        })?;
        Ok(RemoveAssessment { token, connection: summary, confirmation_policy: "prompt-default-yes".into(), consequences: vec!["Remove this app's connection and pinned SSH host key. The remote host and its yards are unchanged.".into()] })
    }

    pub fn remove(
        &mut self,
        token: &str,
        confirmed: bool,
    ) -> Result<ConnectionSummary, NativeError> {
        let pending = self.consume(token)?;
        let Action::Remove(record) = pending.action else {
            return Err(invalid_assessment());
        };
        if !confirmed {
            return Err(NativeError::new(
                "consent_declined",
                "The connection was preserved.",
            ));
        }
        let _lock = StoreLock::acquire(&self.root)?;
        let (baseline, mut data) = self.read()?;
        if baseline != pending.baseline {
            return Err(stale());
        }
        data.connections.retain(|existing| existing.id != record.id);
        if data.selected_connection_id.as_deref() == Some(&record.id) {
            data.selected_connection_id = None;
        }
        self.revoke_editor_profiles_locked(&record.id)?;
        self.revoke_registered_pin_locked(&record.id)?;
        self.write(&mut data)?;
        if self
            .read()?
            .1
            .connections
            .iter()
            .any(|existing| existing.id == record.id)
        {
            return Err(store_io());
        }
        Ok(record.summary())
    }

    #[cfg(test)]
    pub fn selected_connection_id(&self) -> Result<Option<String>, NativeError> {
        Ok(self.read()?.1.selected_connection_id)
    }
    #[cfg(test)]
    pub fn select(&mut self, id: Option<&str>) -> Result<(), NativeError> {
        // Selection is reversible UI state, with no trust change or network I/O.
        if id.is_some_and(|id| !safe_id(id)) {
            return Err(not_found());
        }
        ensure_private_root(&self.root)?;
        let _lock = StoreLock::acquire(&self.root)?;
        let mut data = self.read()?.1;
        if id.is_some_and(|id| !data.connections.iter().any(|record| record.id == id)) {
            return Err(not_found());
        }
        if data.selected_connection_id.as_deref() == id {
            return Ok(());
        }
        data.selected_connection_id = id.map(str::to_owned);
        self.write(&mut data)
    }

    /// Stable editor state is separate from bounded active terminal leases.
    pub fn prepare_editor_profile(&self, id: &str) -> Result<PathBuf, NativeError> {
        if !safe_editor_id(id) {
            return Err(invalid_bundle());
        }
        ensure_private_root(&self.root)?;
        let _lock = StoreLock::acquire(&self.root)?;
        let target = self.root.join(format!("editor-{id}"));
        match fs::symlink_metadata(&target) {
            Ok(_) => check_private_dir(&target)?,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                let mut count = 0;
                for entry in fs::read_dir(&self.root).map_err(|_| store_io())? {
                    let entry = entry.map_err(|_| store_io())?;
                    if entry
                        .file_name()
                        .to_str()
                        .is_some_and(|name| name.starts_with("editor-"))
                    {
                        count += 1;
                    }
                }
                if count >= 128 {
                    return Err(NativeError::new("editor_profile_limit", "The native editor profile limit was reached. Reuse an existing owner and yard profile."));
                }
                create_private_dir(&target)?;
            }
            Err(_) => return Err(store_io()),
        }
        ensure_private_root(&target.join("userdata/User"))?;
        Ok(target)
    }

    /// Only generated public trust/config files are replaced. Existing editor
    /// settings are merged in place without exposing their contents through IPC.
    pub fn write_editor_profile(
        &self,
        root: &Path,
        files: &[(&str, &str)],
    ) -> Result<(), NativeError> {
        self.check_editor_profile(root)?;
        let _lock = StoreLock::acquire(&self.root)?;
        if files.len() > 4 {
            return Err(invalid_bundle());
        }
        let mut names = HashSet::new();
        let mut updates = Vec::new();
        let mut total = 0usize;
        for (name, content) in files {
            if !matches!(
                *name,
                "owner-known-hosts"
                    | "guest-known-hosts"
                    | "ssh-config"
                    | "userdata/User/settings.json"
            ) || !names.insert(*name)
                || content.contains('\0')
                || content.contains("PRIVATE KEY-----")
            {
                return Err(invalid_bundle());
            }
            total = total
                .checked_add(content.len())
                .ok_or_else(invalid_bundle)?;
            if total > STORE_LIMIT {
                return Err(invalid_bundle());
            }
            let path = root.join(name);
            let bytes = if *name == "userdata/User/settings.json" {
                let changes: serde_json::Map<String, serde_json::Value> =
                    serde_json::from_str(content).map_err(|_| invalid_bundle())?;
                if changes.keys().any(|key| {
                    !matches!(
                        key.as_str(),
                        "remote.SSH.configFile" | "remote.SSH.showLoginTerminal"
                    )
                }) || changes
                    .get("remote.SSH.configFile")
                    .is_some_and(|value| !value.is_string())
                    || changes
                        .get("remote.SSH.showLoginTerminal")
                        .is_some_and(|value| !value.is_boolean())
                {
                    return Err(invalid_bundle());
                }
                let mut settings = match open_private_read(&path) {
                    Ok(mut file) => {
                        let mut existing = Vec::new();
                        Read::by_ref(&mut file)
                            .take(4 * 1024 * 1024 + 1)
                            .read_to_end(&mut existing)
                            .map_err(|_| store_io())?;
                        if existing.len() > 4 * 1024 * 1024 {
                            return Err(invalid_bundle());
                        }
                        serde_json::from_slice::<serde_json::Map<String, serde_json::Value>>(
                            &existing,
                        )
                        .map_err(|_| invalid_bundle())?
                    }
                    Err(error) if error.code == "store_missing" => serde_json::Map::new(),
                    Err(error) => return Err(error),
                };
                settings.extend(changes);
                let bytes = serde_json::to_vec_pretty(&settings).map_err(|_| invalid_bundle())?;
                if bytes.len() > 4 * 1024 * 1024 {
                    return Err(invalid_bundle());
                }
                bytes
            } else {
                content.as_bytes().to_vec()
            };
            // Validate every destination before replacing any generated file.
            match fs::symlink_metadata(&path) {
                Ok(_) => check_private_file(&path)?,
                Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
                Err(_) => return Err(store_io()),
            }
            updates.push((path, bytes));
        }
        for (path, bytes) in updates {
            atomic_private_replace(&path, &bytes)?;
        }
        Ok(())
    }
    fn check_editor_profile(&self, root: &Path) -> Result<(), NativeError> {
        let id = root
            .file_name()
            .and_then(|name| name.to_str())
            .and_then(|name| name.strip_prefix("editor-"))
            .filter(|id| safe_editor_id(id))
            .ok_or_else(invalid_bundle)?;
        if root.parent() != Some(self.root.as_path())
            || root != self.root.join(format!("editor-{id}"))
        {
            return Err(invalid_bundle());
        }
        check_ancestors(root)?;
        check_private_dir(root)?;
        check_private_dir(&root.join("userdata"))?;
        check_private_dir(&root.join("userdata/User"))
    }
    fn revoke_editor_profiles_locked(&self, connection_id: &str) -> Result<(), NativeError> {
        let prefix = format!("editor-{connection_id}-");
        for entry in fs::read_dir(&self.root).map_err(|_| store_io())? {
            let entry = entry.map_err(|_| store_io())?;
            if !entry
                .file_name()
                .to_str()
                .is_some_and(|name| name.starts_with(&prefix))
            {
                continue;
            }
            self.check_editor_profile(&entry.path())?;
            for name in ["owner-known-hosts", "guest-known-hosts", "ssh-config"] {
                let path = entry.path().join(name);
                match fs::symlink_metadata(&path) {
                    Ok(_) => {
                        check_private_file(&path)?;
                        fs::remove_file(path).map_err(|_| store_io())?;
                    }
                    Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
                    Err(_) => return Err(store_io()),
                }
            }
        }
        Ok(())
    }
    fn revoke_registered_pin_locked(&self, id: &str) -> Result<(), NativeError> {
        let path = self.root.join(format!("veranda-stored-{id}.known_hosts"));
        match fs::symlink_metadata(&path) {
            Ok(_) => {
                check_private_file(&path)?;
                fs::remove_file(path).map_err(|_| store_io())
            }
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(()),
            Err(_) => Err(store_io()),
        }
    }

    fn insert_pending(&mut self, pending: Pending) -> Result<String, NativeError> {
        self.pending
            .retain(|_, pending| pending.expires > Instant::now());
        if self.pending.len() >= ASSESSMENT_LIMIT {
            return Err(NativeError::new(
                "assessment_limit",
                "Too many connection reviews are pending. Finish or cancel an existing review.",
            ));
        }
        let token = random_id()?;
        self.pending.insert(token.clone(), pending);
        Ok(token)
    }
    fn consume(&mut self, token: &str) -> Result<Pending, NativeError> {
        if token.len() != 48 || !token.bytes().all(|byte| byte.is_ascii_hexdigit()) {
            return Err(invalid_assessment());
        }
        let pending = self.pending.remove(token).ok_or_else(invalid_assessment)?;
        if pending.expires <= Instant::now() {
            return Err(expired());
        }
        Ok(pending)
    }
    pub fn cancel(&mut self, token: &str) {
        if token.len() == 48 {
            self.pending.remove(token);
        }
    }

    fn read(&self) -> Result<(Vec<u8>, StoreData), NativeError> {
        check_ancestors(&self.root)?;
        match fs::symlink_metadata(&self.root) {
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                return Ok((Vec::new(), StoreData::default()))
            }
            Err(_) => return Err(store_io()),
            Ok(_) => check_private_dir(&self.root)?,
        }
        let path = self.root.join(STORE_FILE);
        let mut file = match open_private_read(&path) {
            Ok(file) => file,
            Err(error) if error.code == "store_missing" => {
                return Ok((Vec::new(), StoreData::default()))
            }
            Err(error) => return Err(error),
        };
        let mut bytes = Vec::new();
        Read::by_ref(&mut file)
            .take(STORE_LIMIT as u64 + 1)
            .read_to_end(&mut bytes)
            .map_err(|_| store_io())?;
        if bytes.len() > STORE_LIMIT {
            return Err(invalid_store());
        }
        let data: StoreData = serde_json::from_slice(&bytes).map_err(|_| invalid_store())?;
        validate_store(&data)?;
        Ok((bytes, data))
    }
    fn write(&self, data: &mut StoreData) -> Result<(), NativeError> {
        data.revision = data.revision.checked_add(1).ok_or_else(invalid_store)?;
        validate_store(data)?;
        let bytes = serde_json::to_vec_pretty(data).map_err(|_| store_io())?;
        if bytes.len() > STORE_LIMIT {
            return Err(invalid_store());
        }
        let temporary = self.root.join(format!("store-{}.tmp", random_id()?));
        let result = (|| {
            let mut file = create_private_file(&temporary)?;
            file.write_all(&bytes)
                .and_then(|_| file.sync_all())
                .map_err(|_| store_io())?;
            drop(file);
            let target = self.root.join(STORE_FILE);
            if fs::symlink_metadata(&target).is_ok() {
                check_private_file(&target)?;
            }
            fs::rename(&temporary, &target).map_err(|_| store_io())?;
            #[cfg(unix)]
            File::open(&self.root)
                .and_then(|file| file.sync_all())
                .map_err(|_| store_io())?;
            check_private_file(&target)?;
            if self.read()?.0 != bytes {
                return Err(store_io());
            }
            Ok(())
        })();
        let _ = fs::remove_file(&temporary);
        result
    }
}

fn invalid_bundle() -> NativeError {
    NativeError::new(
        "invalid_launch_bundle",
        "Native launch files must have bounded safe names and contain only non-secret metadata.",
    )
}
fn safe_editor_id(id: &str) -> bool {
    !id.is_empty()
        && id.len() <= 192
        && !id.ends_with('.')
        && !windows_device_name(id)
        && id.as_bytes()[0].is_ascii_alphanumeric()
        && id
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || b"._-".contains(&byte))
}
fn atomic_private_replace(path: &Path, bytes: &[u8]) -> Result<(), NativeError> {
    let parent = path.parent().ok_or_else(invalid_bundle)?;
    check_ancestors(parent)?;
    check_private_dir(parent)?;
    let temporary = parent.join(format!("{}.editor.tmp", random_id()?));
    let result = (|| {
        let mut file = create_private_file(&temporary)?;
        file.write_all(bytes)
            .and_then(|_| file.sync_all())
            .map_err(|_| store_io())?;
        drop(file);
        match fs::symlink_metadata(path) {
            Ok(_) => check_private_file(path)?,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
            Err(_) => return Err(store_io()),
        }
        fs::rename(&temporary, path).map_err(|_| store_io())?;
        check_private_file(path)?;
        #[cfg(unix)]
        File::open(parent)
            .and_then(|file| file.sync_all())
            .map_err(|_| store_io())?;
        Ok(())
    })();
    let _ = fs::remove_file(temporary);
    result
}
fn windows_device_name(value: &str) -> bool {
    let stem = value.split('.').next().unwrap_or("").to_ascii_uppercase();
    matches!(stem.as_str(), "CON" | "PRN" | "AUX" | "NUL")
        || stem
            .strip_prefix("COM")
            .or_else(|| stem.strip_prefix("LPT"))
            .is_some_and(|number| {
                matches!(number, "1" | "2" | "3" | "4" | "5" | "6" | "7" | "8" | "9")
            })
}
fn safe_id(id: &str) -> bool {
    !id.is_empty()
        && id.len() <= 128
        && id.as_bytes()[0].is_ascii_alphanumeric()
        && id
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"._-".contains(&b))
}
fn validate_store(data: &StoreData) -> Result<(), NativeError> {
    if data.schema_version != 1 || data.connections.len() > CONNECTION_LIMIT {
        return Err(invalid_store());
    }
    let mut ids = HashSet::new();
    let mut hosts = HashSet::new();
    let mut destinations = HashSet::new();
    for record in &data.connections {
        if !safe_id(&record.id)
            || !safe_id(&record.host_id)
            || !ids.insert(&record.id)
            || !hosts.insert(&record.host_id)
            || !destinations.insert(&record.destination)
            || Endpoint::parse(&record.destination)
                .map(|endpoint| endpoint.destination != record.destination)
                .unwrap_or(true)
            || ssh::validate_key(&record.public_host_key).is_err()
            || !ssh::valid_fingerprint(&record.fingerprint)
        {
            return Err(invalid_store());
        }
    }
    if data
        .selected_connection_id
        .as_ref()
        .is_some_and(|id| !ids.contains(id))
    {
        return Err(invalid_store());
    }
    Ok(())
}
fn random_id() -> Result<String, NativeError> {
    let mut bytes = [0u8; 24];
    getrandom::fill(&mut bytes).map_err(|_| {
        NativeError::new(
            "native_random_failed",
            "Could not prepare a secure connection review. Try again.",
        )
    })?;
    Ok(bytes.iter().map(|byte| format!("{byte:02x}")).collect())
}
fn store_io() -> NativeError {
    NativeError::new("connection_store_io", "Could not safely read or update the Veranda connection store. Check app-data access and try again.")
}
fn invalid_store() -> NativeError {
    NativeError::new(
        "invalid_connection_store",
        "The Veranda connection store is invalid or unsupported. Restore a valid app-data backup.",
    )
}
fn unsafe_store() -> NativeError {
    NativeError::new("unsafe_connection_store", "The Veranda connection store must be private, owned by the current user, and free of symlinks.")
}
fn stale() -> NativeError {
    NativeError::new(
        "stale_assessment",
        "The connection store changed after review. Review the current connection again.",
    )
}
fn invalid_assessment() -> NativeError {
    NativeError::new("invalid_assessment", "This connection review was already used, cancelled, or does not match the requested action.")
}
fn expired() -> NativeError {
    NativeError::new(
        "assessment_expired",
        "The connection review expired. Review the current host key again.",
    )
}
fn changed_registration() -> NativeError {
    NativeError::new(
        "connection_changed",
        "The saved connection changed or was removed. Reopen it before continuing.",
    )
}
fn not_found() -> NativeError {
    NativeError::new(
        "connection_not_found",
        "This connection is no longer registered. Refresh the connection list.",
    )
}

fn check_ancestors(path: &Path) -> Result<(), NativeError> {
    if !path.is_absolute()
        || path
            .components()
            .any(|component| matches!(component, std::path::Component::ParentDir))
    {
        return Err(unsafe_store());
    }
    for ancestor in path.ancestors() {
        match fs::symlink_metadata(ancestor) {
            Ok(metadata) if is_link(&metadata) || !metadata.is_dir() => return Err(unsafe_store()),
            Ok(_) => (),
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => (),
            Err(_) => return Err(store_io()),
        }
    }
    Ok(())
}
fn is_link(metadata: &fs::Metadata) -> bool {
    #[cfg(windows)]
    {
        use std::os::windows::fs::MetadataExt;
        metadata.file_attributes() & 0x400 != 0
    }
    #[cfg(not(windows))]
    {
        metadata.file_type().is_symlink()
    }
}
fn ensure_private_root(path: &Path) -> Result<(), NativeError> {
    check_ancestors(path)?;
    if !path.exists() {
        let parent = path.parent().ok_or_else(unsafe_store)?;
        if !parent.exists() {
            ensure_private_root(parent)?;
        }
        create_private_dir(path)?;
    }
    check_private_dir(path)
}
fn check_private_dir(path: &Path) -> Result<(), NativeError> {
    let metadata = fs::symlink_metadata(path).map_err(|_| store_io())?;
    if is_link(&metadata) || !metadata.is_dir() {
        return Err(unsafe_store());
    }
    check_access(path, &metadata, true)
}
fn check_private_file(path: &Path) -> Result<(), NativeError> {
    open_private_read(path).map(|_| ())
}
fn open_private_read(path: &Path) -> Result<File, NativeError> {
    let metadata = fs::symlink_metadata(path).map_err(|error| {
        if error.kind() == std::io::ErrorKind::NotFound {
            NativeError::new("store_missing", "Connection store is not yet created.")
        } else {
            store_io()
        }
    })?;
    if is_link(&metadata) || !metadata.is_file() {
        return Err(unsafe_store());
    }
    let mut options = OpenOptions::new();
    options.read(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        options.custom_flags(libc::O_NOFOLLOW);
    }
    #[cfg(windows)]
    {
        use std::os::windows::fs::OpenOptionsExt;
        options.custom_flags(windows_sys::Win32::Storage::FileSystem::FILE_FLAG_OPEN_REPARSE_POINT);
    }
    let file = options.open(path).map_err(|_| unsafe_store())?;
    let actual = file.metadata().map_err(|_| store_io())?;
    if is_link(&actual) || !actual.is_file() {
        return Err(unsafe_store());
    }
    check_access(path, &actual, false)?;
    Ok(file)
}

#[cfg(unix)]
fn check_access(_path: &Path, metadata: &fs::Metadata, directory: bool) -> Result<(), NativeError> {
    use std::os::unix::fs::MetadataExt;
    if metadata.uid() != unsafe { libc::geteuid() }
        || metadata.mode() & 0o7777 != if directory { 0o700 } else { 0o600 }
        || (!directory && metadata.nlink() != 1)
    {
        return Err(unsafe_store());
    }
    Ok(())
}
#[cfg(unix)]
fn create_private_dir(path: &Path) -> Result<(), NativeError> {
    use std::os::unix::fs::{DirBuilderExt, PermissionsExt};
    fs::DirBuilder::new()
        .mode(0o700)
        .create(path)
        .map_err(|_| store_io())?;
    fs::set_permissions(path, fs::Permissions::from_mode(0o700)).map_err(|_| store_io())
}
#[cfg(unix)]
fn create_private_file(path: &Path) -> Result<File, NativeError> {
    use std::os::unix::fs::{OpenOptionsExt, PermissionsExt};
    let file = OpenOptions::new()
        .read(true)
        .write(true)
        .create_new(true)
        .mode(0o600)
        .custom_flags(libc::O_NOFOLLOW)
        .open(path)
        .map_err(|_| store_io())?;
    file.set_permissions(fs::Permissions::from_mode(0o600))
        .map_err(|_| store_io())?;
    Ok(file)
}

struct StoreLock {
    file: File,
}
#[cfg(all(test, target_os = "linux"))]
pub(crate) fn try_store_lock_for_test(root: &Path) -> Result<impl Drop, NativeError> {
    StoreLock::acquire(root)
}
impl StoreLock {
    fn acquire(root: &Path) -> Result<Self, NativeError> {
        check_private_dir(root)?;
        let path = root.join("connections.lock");
        let file = if fs::symlink_metadata(&path).is_ok() {
            open_private_lock(&path)?
        } else {
            match create_private_file(&path) {
                Ok(file) => file,
                Err(_) if path.exists() => open_private_lock(&path)?,
                Err(error) => return Err(error),
            }
        };
        lock_file(&file)?;
        Ok(Self { file })
    }
}
fn open_private_lock(path: &Path) -> Result<File, NativeError> {
    let _ = open_private_read(path)?;
    let mut options = OpenOptions::new();
    options.read(true).write(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        options.custom_flags(libc::O_NOFOLLOW);
    }
    #[cfg(windows)]
    {
        use std::os::windows::fs::OpenOptionsExt;
        options.custom_flags(windows_sys::Win32::Storage::FileSystem::FILE_FLAG_OPEN_REPARSE_POINT);
    }
    let file = options.open(path).map_err(|_| unsafe_store())?;
    check_access(path, &file.metadata().map_err(|_| store_io())?, false)?;
    Ok(file)
}
#[cfg(unix)]
fn lock_file(file: &File) -> Result<(), NativeError> {
    use std::os::fd::AsRawFd;
    if unsafe { libc::flock(file.as_raw_fd(), libc::LOCK_EX | libc::LOCK_NB) } != 0 {
        return Err(NativeError::new(
            "connection_store_busy",
            "Another Veranda process is updating connections. Try again.",
        ));
    }
    Ok(())
}
impl Drop for StoreLock {
    fn drop(&mut self) {
        #[cfg(unix)]
        {
            use std::os::fd::AsRawFd;
            unsafe {
                libc::flock(self.file.as_raw_fd(), libc::LOCK_UN);
            }
        }
        #[cfg(windows)]
        {
            windows_access::unlock(&self.file);
        }
    }
}

#[cfg(windows)]
fn check_access(path: &Path, _metadata: &fs::Metadata, directory: bool) -> Result<(), NativeError> {
    windows_access::check(path, directory)
}
#[cfg(windows)]
fn create_private_dir(path: &Path) -> Result<(), NativeError> {
    windows_access::create_dir(path)
}
#[cfg(windows)]
fn create_private_file(path: &Path) -> Result<File, NativeError> {
    windows_access::create_file(path)
}
#[cfg(windows)]
fn lock_file(file: &File) -> Result<(), NativeError> {
    windows_access::lock(file)
}

#[cfg(windows)]
mod windows_access {
    use super::*;
    use std::ffi::c_void;
    use std::os::windows::ffi::OsStrExt;
    use std::os::windows::io::{AsRawHandle, FromRawHandle};
    use std::ptr::{null, null_mut};
    use windows_sys::Win32::Foundation::{
        CloseHandle, LocalFree, GENERIC_READ, GENERIC_WRITE, INVALID_HANDLE_VALUE,
    };
    use windows_sys::Win32::Security::Authorization::*;
    use windows_sys::Win32::Security::*;
    use windows_sys::Win32::Storage::FileSystem::*;
    use windows_sys::Win32::System::Threading::{GetCurrentProcess, OpenProcessToken};
    use windows_sys::Win32::System::IO::OVERLAPPED;

    struct LocalAllocation(*mut c_void);
    impl Drop for LocalAllocation {
        fn drop(&mut self) {
            if !self.0.is_null() {
                unsafe {
                    LocalFree(self.0);
                }
            }
        }
    }
    struct Identity {
        buffer: Vec<usize>,
    }
    impl Identity {
        fn current() -> Result<Self, NativeError> {
            unsafe {
                let mut token = null_mut();
                if OpenProcessToken(GetCurrentProcess(), TOKEN_QUERY, &mut token) == 0 {
                    return Err(unsafe_store());
                }
                let mut needed = 0;
                GetTokenInformation(token, TokenUser, null_mut(), 0, &mut needed);
                if needed == 0 || needed > 64 * 1024 {
                    CloseHandle(token);
                    return Err(unsafe_store());
                }
                // usize preserves TOKEN_USER alignment, unlike a byte buffer.
                let mut buffer =
                    vec![0usize; (needed as usize).div_ceil(std::mem::size_of::<usize>())];
                let success = GetTokenInformation(
                    token,
                    TokenUser,
                    buffer.as_mut_ptr().cast(),
                    needed,
                    &mut needed,
                );
                CloseHandle(token);
                if success == 0 {
                    return Err(unsafe_store());
                }
                Ok(Self { buffer })
            }
        }
        fn sid(&self) -> PSID {
            unsafe { (*(self.buffer.as_ptr().cast::<TOKEN_USER>())).User.Sid }
        }
        fn descriptor(&self, directory: bool) -> Result<LocalAllocation, NativeError> {
            unsafe {
                let mut sid_text = null_mut();
                if ConvertSidToStringSidW(self.sid(), &mut sid_text) == 0 {
                    return Err(unsafe_store());
                }
                let _sid_allocation = LocalAllocation(sid_text.cast());
                let mut length = 0;
                while *sid_text.add(length) != 0 && length < 256 {
                    length += 1;
                }
                if length == 256 {
                    return Err(unsafe_store());
                }
                let sid = String::from_utf16(std::slice::from_raw_parts(sid_text, length))
                    .map_err(|_| unsafe_store())?;
                let flags = if directory { "OICI" } else { "" };
                // Protected DACL: exactly the current user, full control. No
                // inherited Everyone/Users entries or permissions from umask.
                let text: Vec<u16> = format!("O:{sid}D:P(A;{flags};FA;;;{sid})")
                    .encode_utf16()
                    .chain(Some(0))
                    .collect();
                let mut descriptor = null_mut();
                if ConvertStringSecurityDescriptorToSecurityDescriptorW(
                    text.as_ptr(),
                    SDDL_REVISION_1,
                    &mut descriptor,
                    null_mut(),
                ) == 0
                {
                    return Err(unsafe_store());
                }
                Ok(LocalAllocation(descriptor))
            }
        }
    }
    fn wide(path: &Path) -> Result<Vec<u16>, NativeError> {
        let mut text: Vec<_> = path.as_os_str().encode_wide().collect();
        if text.contains(&0) {
            return Err(unsafe_store());
        }
        text.push(0);
        Ok(text)
    }
    pub fn create_dir(path: &Path) -> Result<(), NativeError> {
        let identity = Identity::current()?;
        let descriptor = identity.descriptor(true)?;
        let attributes = SECURITY_ATTRIBUTES {
            nLength: std::mem::size_of::<SECURITY_ATTRIBUTES>() as u32,
            lpSecurityDescriptor: descriptor.0,
            bInheritHandle: 0,
        };
        if unsafe { CreateDirectoryW(wide(path)?.as_ptr(), &attributes) } == 0 {
            return Err(store_io());
        }
        check(path, true)
    }
    pub fn create_file(path: &Path) -> Result<File, NativeError> {
        let identity = Identity::current()?;
        let descriptor = identity.descriptor(false)?;
        let attributes = SECURITY_ATTRIBUTES {
            nLength: std::mem::size_of::<SECURITY_ATTRIBUTES>() as u32,
            lpSecurityDescriptor: descriptor.0,
            bInheritHandle: 0,
        };
        let handle = unsafe {
            CreateFileW(
                wide(path)?.as_ptr(),
                GENERIC_READ | GENERIC_WRITE,
                FILE_SHARE_READ | FILE_SHARE_WRITE | FILE_SHARE_DELETE,
                &attributes,
                CREATE_NEW,
                FILE_ATTRIBUTE_NORMAL | FILE_FLAG_OPEN_REPARSE_POINT,
                null_mut(),
            )
        };
        if handle == INVALID_HANDLE_VALUE {
            return Err(store_io());
        }
        let file = unsafe { File::from_raw_handle(handle) };
        check_handle(&file, false)?;
        Ok(file)
    }
    pub fn check(path: &Path, directory: bool) -> Result<(), NativeError> {
        let handle = unsafe {
            CreateFileW(
                wide(path)?.as_ptr(),
                READ_CONTROL | FILE_READ_ATTRIBUTES,
                FILE_SHARE_READ | FILE_SHARE_WRITE | FILE_SHARE_DELETE,
                null(),
                OPEN_EXISTING,
                FILE_FLAG_BACKUP_SEMANTICS | FILE_FLAG_OPEN_REPARSE_POINT,
                null_mut(),
            )
        };
        if handle == INVALID_HANDLE_VALUE {
            return Err(unsafe_store());
        }
        let file = unsafe { File::from_raw_handle(handle) };
        check_handle(&file, directory)
    }
    fn check_handle(file: &File, directory: bool) -> Result<(), NativeError> {
        let identity = Identity::current()?;
        unsafe {
            let handle = file.as_raw_handle();
            let mut info: BY_HANDLE_FILE_INFORMATION = std::mem::zeroed();
            if GetFileInformationByHandle(handle, &mut info) == 0
                || info.dwFileAttributes & FILE_ATTRIBUTE_REPARSE_POINT != 0
                || (!directory && info.nNumberOfLinks != 1)
            {
                return Err(unsafe_store());
            }
            let mut owner = null_mut();
            let mut acl: *mut ACL = null_mut();
            let mut descriptor = null_mut();
            if GetSecurityInfo(
                handle,
                SE_FILE_OBJECT,
                OWNER_SECURITY_INFORMATION | DACL_SECURITY_INFORMATION,
                &mut owner,
                null_mut(),
                &mut acl,
                null_mut(),
                &mut descriptor,
            ) != 0
            {
                return Err(unsafe_store());
            }
            let _allocation = LocalAllocation(descriptor);
            let mut control = 0;
            let mut revision = 0;
            if owner.is_null()
                || EqualSid(owner, identity.sid()) == 0
                || acl.is_null()
                || (*acl).AceCount != 1
                || GetSecurityDescriptorControl(descriptor, &mut control, &mut revision) == 0
                || control & SE_DACL_PROTECTED == 0
            {
                return Err(unsafe_store());
            }
            let mut ace = null_mut();
            if GetAce(acl, 0, &mut ace) == 0 || ace.is_null() {
                return Err(unsafe_store());
            }
            let allowed = &*(ace.cast::<ACCESS_ALLOWED_ACE>());
            // ACCESS_ALLOWED_ACE_TYPE is 0; reject callback/object/deny ACEs.
            if allowed.Header.AceType != 0
                || u32::from(allowed.Header.AceFlags) & INHERIT_ONLY_ACE != 0
                || allowed.Mask & FILE_ALL_ACCESS != FILE_ALL_ACCESS
                || EqualSid(
                    (&allowed.SidStart as *const u32).cast_mut().cast(),
                    identity.sid(),
                ) == 0
            {
                return Err(unsafe_store());
            }
        }
        Ok(())
    }
    pub fn lock(file: &File) -> Result<(), NativeError> {
        let mut overlapped: OVERLAPPED = unsafe { std::mem::zeroed() };
        if unsafe {
            LockFileEx(
                file.as_raw_handle(),
                LOCKFILE_EXCLUSIVE_LOCK | LOCKFILE_FAIL_IMMEDIATELY,
                0,
                1,
                0,
                &mut overlapped,
            )
        } == 0
        {
            return Err(NativeError::new(
                "connection_store_busy",
                "Another Veranda process is updating connections. Try again.",
            ));
        }
        Ok(())
    }
    pub fn unlock(file: &File) {
        let mut overlapped: OVERLAPPED = unsafe { std::mem::zeroed() };
        unsafe {
            UnlockFileEx(file.as_raw_handle(), 0, 1, 0, &mut overlapped);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    struct Fixture {
        parent: PathBuf,
        store: ConnectionStore,
    }
    impl Fixture {
        fn new() -> Self {
            // macOS may expose its temporary directory through the /var symlink.
            let parent = std::env::temp_dir()
                .canonicalize()
                .unwrap()
                .join(format!("veranda-test-{}", random_id().unwrap()));
            create_private_dir(&parent).unwrap();
            let store = ConnectionStore::new(parent.join("connections"));
            Self { parent, store }
        }
        fn assess(&mut self, repair: Option<&str>, replacement: bool) -> TrustAssessment {
            let (baseline, data) = self.store.read().unwrap();
            let key = key(replacement);
            self.store
                .prepare_with_key(
                    Endpoint::parse("user@synthetic.test:2222").unwrap(),
                    key,
                    repair,
                    baseline,
                    data,
                )
                .unwrap()
        }
        fn register(&mut self) -> ConnectionSummary {
            let assessment = self.assess(None, false);
            let session = self
                .store
                .consented_ssh_command(&assessment.token, true, &assessment.fingerprint)
                .unwrap();
            self.store
                .finalize_registration(session, "synthetic-owner")
                .unwrap()
        }
    }
    impl Drop for Fixture {
        fn drop(&mut self) {
            let _ = fs::remove_dir_all(&self.parent);
        }
    }
    fn key(replacement: bool) -> HostKey {
        HostKey {
            key: format!("ssh-ed25519 {}", if replacement { "BBBB" } else { "AAAA" }),
            fingerprint: format!("SHA256:{}", if replacement { "B" } else { "A" }.repeat(43)),
        }
    }
    #[test]
    fn registered_pins_are_stable_across_launches_and_reload_then_revoked_by_trust_changes() {
        let mut fixture = Fixture::new();
        let record = fixture.register();
        let initial = fixture.store.registered_ssh_command(&record.id).unwrap();
        let path = initial.pin_file.clone();
        let public = initial.public_pin();
        drop(initial);
        assert!(path.exists());
        let reloaded = ConnectionStore::new(fixture.store.root.clone());
        for _ in 0..20 {
            let session = reloaded.registered_ssh_command(&record.id).unwrap();
            assert_eq!(session.pin_file, path);
            assert_eq!(session.public_pin(), public);
            session
                .owner_command(&["bash".into(), "-l".into()], true)
                .unwrap();
        }
        assert_eq!(
            fs::read_dir(&fixture.store.root)
                .unwrap()
                .filter_map(Result::ok)
                .filter(|entry| entry
                    .file_name()
                    .to_str()
                    .is_some_and(|name| name.ends_with(".known_hosts")))
                .count(),
            1
        );
        let repair = fixture.assess(Some(&record.id), true);
        let accepted = fixture
            .store
            .consented_ssh_command(&repair.token, true, &repair.fingerprint)
            .unwrap();
        fixture
            .store
            .finalize_registration(accepted, &record.host_id)
            .unwrap();
        assert!(!path.exists());
        let repaired = fixture.store.registered_ssh_command(&record.id).unwrap();
        assert_eq!(repaired.pin_file, path);
        assert_ne!(repaired.public_pin(), public);
        drop(repaired);
        let removal = fixture.store.prepare_remove(&record.id).unwrap();
        fixture.store.remove(&removal.token, true).unwrap();
        assert!(!path.exists());
    }
    #[test]
    fn editor_profiles_preserve_user_state_and_revoke_only_generated_trust() {
        let mut fixture = Fixture::new();
        let connection = fixture.register();
        let profile_id = format!("{}-default", connection.id);
        let profile = fixture.store.prepare_editor_profile(&profile_id).unwrap();
        let settings_path = profile.join("userdata/User/settings.json");
        let mut settings = create_private_file(&settings_path).unwrap();
        settings.write_all(br#"{"editor.fontSize":17,"remote.SSH.configFile":"old","unrelated":{"keep":true}}"#).unwrap();
        drop(settings);
        let updates = [
            ("owner-known-hosts", "public owner pin\n"),
            ("guest-known-hosts", "public guest pin\n"),
            ("ssh-config", "Host guest\n"),
            (
                "userdata/User/settings.json",
                r#"{"remote.SSH.configFile":"new","remote.SSH.showLoginTerminal":true}"#,
            ),
        ];
        fixture
            .store
            .write_editor_profile(&profile, &updates)
            .unwrap();
        fixture
            .store
            .write_editor_profile(&profile, &[("ssh-config", "Host replacement\n")])
            .unwrap();
        assert_eq!(
            fixture.store.prepare_editor_profile(&profile_id).unwrap(),
            profile
        );
        let preserved: serde_json::Value =
            serde_json::from_slice(&fs::read(&settings_path).unwrap()).unwrap();
        assert_eq!(preserved["editor.fontSize"], 17);
        assert_eq!(preserved["unrelated"]["keep"], true);
        assert_eq!(preserved["remote.SSH.configFile"], "new");
        assert!(fixture
            .store
            .write_editor_profile(
                &profile,
                &[("userdata/User/settings.json", r#"{"unapproved":"change"}"#)]
            )
            .is_err());
        // Persistent profiles do not consume any of the sixteen active leases.
        for index in 0..17 {
            fixture
                .store
                .prepare_editor_profile(&format!("{}-yard{index}", connection.id))
                .unwrap();
        }
        let removal = fixture.store.prepare_remove(&connection.id).unwrap();
        fixture.store.remove(&removal.token, true).unwrap();
        for name in ["owner-known-hosts", "guest-known-hosts", "ssh-config"] {
            assert!(!profile.join(name).exists());
        }
        assert_eq!(
            serde_json::from_slice::<serde_json::Value>(&fs::read(settings_path).unwrap()).unwrap(),
            preserved
        );
    }
    #[cfg(unix)]
    #[test]
    fn editor_profile_refuses_symlink_and_invalid_settings_before_replacing_trust() {
        use std::os::unix::fs::symlink;
        let fixture = Fixture::new();
        let profile = fixture
            .store
            .prepare_editor_profile("synthetic-default")
            .unwrap();
        fixture
            .store
            .write_editor_profile(&profile, &[("owner-known-hosts", "original\n")])
            .unwrap();
        let target = profile.join("userdata/User/settings.json");
        symlink(profile.join("owner-known-hosts"), &target).unwrap();
        assert!(fixture
            .store
            .write_editor_profile(
                &profile,
                &[
                    ("owner-known-hosts", "replacement\n"),
                    ("userdata/User/settings.json", "{}")
                ]
            )
            .is_err());
        assert_eq!(
            fs::read_to_string(profile.join("owner-known-hosts")).unwrap(),
            "original\n"
        );
        fs::remove_file(&target).unwrap();
        let mut file = create_private_file(&target).unwrap();
        file.write_all(b"not-json").unwrap();
        drop(file);
        assert!(fixture
            .store
            .write_editor_profile(
                &profile,
                &[
                    ("owner-known-hosts", "replacement\n"),
                    ("userdata/User/settings.json", "{}")
                ]
            )
            .is_err());
        assert_eq!(
            fs::read_to_string(profile.join("owner-known-hosts")).unwrap(),
            "original\n"
        );
    }
    #[test]
    fn generated_editor_metadata_is_private_and_rejects_paths_secrets_and_oversize() {
        let fixture = Fixture::new();
        let profile = fixture
            .store
            .prepare_editor_profile("synthetic-default")
            .unwrap();
        fixture
            .store
            .write_editor_profile(
                &profile,
                &[
                    ("owner-known-hosts", "public original pin\n"),
                    ("userdata/User/settings.json", "{}"),
                ],
            )
            .unwrap();
        check_private_dir(&profile).unwrap();
        check_private_dir(&profile.join("userdata/User")).unwrap();
        check_private_file(&profile.join("owner-known-hosts")).unwrap();
        check_private_file(&profile.join("userdata/User/settings.json")).unwrap();
        for invalid in [
            "../escape",
            "userdata/../escape",
            "/absolute",
            "bad\\file",
            "NUL",
            "CON.txt",
            "filename.",
        ] {
            assert!(fixture
                .store
                .write_editor_profile(&profile, &[(invalid, "metadata")])
                .is_err());
        }
        for invalid_id in ["../escape", "NUL", "CON.txt", "filename."] {
            assert!(fixture.store.prepare_editor_profile(invalid_id).is_err());
        }
        assert!(fixture
            .store
            .write_editor_profile(
                &profile,
                &[(
                    "owner-known-hosts",
                    "-----BEGIN OPENSSH PRIVATE KEY-----\nsynthetic forbidden payload"
                )]
            )
            .is_err());
        assert!(fixture
            .store
            .write_editor_profile(
                &profile,
                &[
                    ("owner-known-hosts", "duplicate"),
                    ("owner-known-hosts", "duplicate")
                ]
            )
            .is_err());
        assert!(fixture
            .store
            .write_editor_profile(
                &profile,
                &[("owner-known-hosts", &"x".repeat(STORE_LIMIT + 1))]
            )
            .is_err());
        assert_eq!(
            fs::read_to_string(profile.join("owner-known-hosts")).unwrap(),
            "public original pin\n"
        );
    }
    #[test]
    fn copied_public_pin_supports_proxy_but_changed_copy_is_refused() {
        let mut fixture = Fixture::new();
        let registered = fixture.register();
        let session = fixture
            .store
            .registered_ssh_command(&registered.id)
            .unwrap();
        let pin = session.public_pin();
        let profile = fixture
            .store
            .prepare_editor_profile("proxy-editor")
            .unwrap();
        fixture
            .store
            .write_editor_profile(
                &profile,
                &[
                    ("owner-known-hosts", &pin),
                    ("ssh-config", "Host synthetic\n"),
                    ("userdata/User/settings.json", "{}"),
                ],
            )
            .unwrap();
        let path = profile.join("owner-known-hosts");
        assert!(session
            .owner_command_with_pin(&["bash".into(), "-l".into()], true, &path)
            .is_ok());
        assert!(session.proxy_command_with_pin(2222, &path).is_ok());
        assert!(session.proxy_command_with_pin(0, &path).is_err());
        assert!(session
            .owner_command(&["bash".into(), "-l".into()], true)
            .is_ok());
        let mut file = OpenOptions::new()
            .write(true)
            .truncate(true)
            .open(&path)
            .unwrap();
        file.write_all(b"veranda-other ssh-ed25519 BBBB\n").unwrap();
        assert_eq!(
            session
                .proxy_command_with_pin(2222, &path)
                .err()
                .unwrap()
                .code,
            "changed_session_pin"
        );
    }
    #[test]
    fn review_and_decline_never_mutate_store_or_launch_a_session() {
        let mut fixture = Fixture::new();
        let review = fixture.assess(None, false);
        assert_eq!(review.confirmation_policy, "prompt-default-yes");
        assert!(!fixture.store.root.exists());
        let error = fixture
            .store
            .consented_ssh_command(&review.token, false, &review.fingerprint)
            .err()
            .unwrap();
        assert_eq!(error.code, "consent_declined");
        assert!(!fixture.store.root.exists());
        assert_eq!(
            fixture
                .store
                .consented_ssh_command(&review.token, true, &review.fingerprint)
                .err()
                .unwrap()
                .code,
            "invalid_assessment"
        );
    }
    #[test]
    fn accepted_probe_failure_cleans_pin_without_registration() {
        let mut fixture = Fixture::new();
        let review = fixture.assess(None, false);
        let session = fixture
            .store
            .consented_ssh_command(&review.token, true, &review.fingerprint)
            .unwrap();
        let pin = session.pin_file.clone();
        assert!(pin.exists());
        assert!(fixture.store.list().unwrap().is_empty());
        drop(session);
        assert!(!pin.exists());
        assert!(!fixture.store.root.join(STORE_FILE).exists());
    }
    #[test]
    fn stale_before_consent_is_rejected_and_probe_pin_cannot_be_changed() {
        let mut fixture = Fixture::new();
        let review = fixture.assess(None, false);
        fixture.register();
        assert_eq!(
            fixture
                .store
                .consented_ssh_command(&review.token, true, &review.fingerprint)
                .err()
                .unwrap()
                .code,
            "stale_assessment"
        );
        let registered = fixture.store.list().unwrap()[0].clone();
        let session = fixture
            .store
            .registered_ssh_command(&registered.id)
            .unwrap();
        assert!(session.command(Some("synthetic-yard")).is_ok());
        assert!(session.command(Some("yard;bad")).is_err());
        let mut file = OpenOptions::new()
            .write(true)
            .truncate(true)
            .open(&session.pin_file)
            .unwrap();
        file.write_all(b"veranda-unreviewed ssh-ed25519 BBBB\n")
            .unwrap();
        assert_eq!(
            session.command(None).err().unwrap().code,
            "changed_session_pin"
        );
        assert_eq!(fixture.store.list().unwrap(), [registered]);
    }
    #[test]
    fn registration_reload_selection_and_remove_are_verified() {
        let mut fixture = Fixture::new();
        let registered = fixture.register();
        let reloaded = ConnectionStore::new(fixture.store.root.clone());
        assert_eq!(reloaded.list().unwrap(), [registered.clone()]);
        assert_eq!(
            reloaded.selected_connection_id().unwrap(),
            Some(registered.id.clone())
        );
        let review = fixture.store.prepare_remove(&registered.id).unwrap();
        fixture.store.remove(&review.token, true).unwrap();
        assert!(reloaded.list().unwrap().is_empty());
        assert_eq!(reloaded.selected_connection_id().unwrap(), None);
        assert_eq!(
            fixture.store.remove(&review.token, true).unwrap_err().code,
            "invalid_assessment"
        );
    }
    #[test]
    fn assessment_expiry_and_fingerprint_mismatch_are_single_use() {
        let mut fixture = Fixture::new();
        let review = fixture.assess(None, false);
        assert_eq!(
            fixture
                .store
                .consented_ssh_command(&review.token, true, "SHA256:wrong")
                .err()
                .unwrap()
                .code,
            "invalid_assessment"
        );
        assert!(!fixture.store.root.exists());
        let review = fixture.assess(None, false);
        fixture
            .store
            .pending
            .get_mut(&review.token)
            .unwrap()
            .expires = Instant::now() - Duration::from_secs(1);
        assert_eq!(
            fixture
                .store
                .consented_ssh_command(&review.token, true, &review.fingerprint)
                .err()
                .unwrap()
                .code,
            "assessment_expired"
        );
    }
    #[test]
    fn captured_baseline_blocks_late_registration_and_remove() {
        let mut fixture = Fixture::new();
        let review = fixture.assess(None, false);
        let session = fixture
            .store
            .consented_ssh_command(&review.token, true, &review.fingerprint)
            .unwrap();
        fixture.register();
        assert_eq!(
            fixture
                .store
                .finalize_registration(session, "different-owner")
                .unwrap_err()
                .code,
            "stale_assessment"
        );
        let registered = fixture.store.list().unwrap()[0].clone();
        let removal = fixture.store.prepare_remove(&registered.id).unwrap();
        fixture.store.select(None).unwrap();
        assert_eq!(
            fixture.store.remove(&removal.token, true).unwrap_err().code,
            "stale_assessment"
        );
        assert_eq!(fixture.store.list().unwrap(), [registered]);
    }
    #[test]
    fn host_key_repair_requires_same_owner_identity() {
        let mut fixture = Fixture::new();
        let registered = fixture.register();
        let (baseline, data) = fixture.store.read().unwrap();
        assert_eq!(
            fixture
                .store
                .prepare_with_key(
                    Endpoint::parse(&registered.destination).unwrap(),
                    key(true),
                    None,
                    baseline,
                    data
                )
                .unwrap_err()
                .code,
            "host_key_changed"
        );
        let review = fixture.assess(Some(&registered.id), true);
        let session = fixture
            .store
            .consented_ssh_command(&review.token, true, &review.fingerprint)
            .unwrap();
        assert_eq!(
            fixture
                .store
                .finalize_registration(session, "different-owner")
                .unwrap_err()
                .code,
            "repair_identity_mismatch"
        );
        assert_eq!(
            fixture.store.read().unwrap().1.connections[0].public_host_key,
            key(false).key
        );
        let review = fixture.assess(Some(&registered.id), true);
        let session = fixture
            .store
            .consented_ssh_command(&review.token, true, &review.fingerprint)
            .unwrap();
        assert_eq!(
            fixture
                .store
                .finalize_registration(session, &registered.host_id)
                .unwrap(),
            registered
        );
        assert_eq!(
            fixture.store.read().unwrap().1.connections[0].public_host_key,
            key(true).key
        );
    }
    #[test]
    fn duplicate_host_and_invalid_json_are_rejected() {
        let mut fixture = Fixture::new();
        fixture.register();
        let (baseline, data) = fixture.store.read().unwrap();
        let review = fixture
            .store
            .prepare_with_key(
                Endpoint::parse("other.test").unwrap(),
                key(false),
                None,
                baseline,
                data,
            )
            .unwrap();
        let session = fixture
            .store
            .consented_ssh_command(&review.token, true, &review.fingerprint)
            .unwrap();
        assert_eq!(
            fixture
                .store
                .finalize_registration(session, "synthetic-owner")
                .unwrap_err()
                .code,
            "connection_exists"
        );
        let path = fixture.store.root.join(STORE_FILE);
        let mut file = OpenOptions::new()
            .write(true)
            .truncate(true)
            .open(&path)
            .unwrap();
        file.write_all(b"{\"schemaVersion\":1,\"revision\":0,\"connections\":[],\"selectedConnectionId\":null,\"secret\":\"invalid\"}").unwrap();
        assert_eq!(
            fixture.store.list().unwrap_err().code,
            "invalid_connection_store"
        );
    }
    #[test]
    fn concurrent_writer_lock_is_released_on_drop() {
        let fixture = Fixture::new();
        ensure_private_root(&fixture.store.root).unwrap();
        let lock = StoreLock::acquire(&fixture.store.root).unwrap();
        assert_eq!(
            StoreLock::acquire(&fixture.store.root).err().unwrap().code,
            "connection_store_busy"
        );
        drop(lock);
        assert!(StoreLock::acquire(&fixture.store.root).is_ok());
    }
    #[cfg(unix)]
    #[test]
    fn symlinks_hardlinks_and_public_permissions_are_refused() {
        use std::os::unix::fs::{symlink, PermissionsExt};
        let mut fixture = Fixture::new();
        fixture.register();
        let root = fixture.store.root.clone();
        let store_file = root.join(STORE_FILE);
        fs::set_permissions(&store_file, fs::Permissions::from_mode(0o644)).unwrap();
        assert_eq!(
            fixture.store.list().unwrap_err().code,
            "unsafe_connection_store"
        );
        fs::set_permissions(&store_file, fs::Permissions::from_mode(0o600)).unwrap();
        let hardlink = fixture.parent.join("hardlink");
        fs::hard_link(&store_file, &hardlink).unwrap();
        assert_eq!(
            fixture.store.list().unwrap_err().code,
            "unsafe_connection_store"
        );
        fs::remove_file(hardlink).unwrap();
        let target = fixture.parent.join("target");
        fs::rename(&store_file, &target).unwrap();
        symlink(&target, &store_file).unwrap();
        assert_eq!(
            fixture.store.list().unwrap_err().code,
            "unsafe_connection_store"
        );
        fs::remove_file(&store_file).unwrap();
        fs::rename(&target, &store_file).unwrap();
        let root_alias = fixture.parent.join("alias");
        symlink(&root, &root_alias).unwrap();
        assert_eq!(
            ConnectionStore::new(root_alias).list().unwrap_err().code,
            "unsafe_connection_store"
        );
        let parent_alias = fixture.parent.join("parent-alias");
        symlink(&fixture.parent, &parent_alias).unwrap();
        assert_eq!(
            ConnectionStore::new(parent_alias.join("connections"))
                .list()
                .unwrap_err()
                .code,
            "unsafe_connection_store"
        );
        fs::set_permissions(&root, fs::Permissions::from_mode(0o755)).unwrap();
        assert_eq!(
            fixture.store.list().unwrap_err().code,
            "unsafe_connection_store"
        );
    }
}
