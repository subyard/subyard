use veranda_lib::connections::{ConnectionSummary, NavigationSelection};
use veranda_lib::local_fleet::{LocalFleetSnapshot, LocalYard, NativeError};

pub struct Host {
    pub connection: Option<String>,
    pub label: String,
    pub snapshot: Option<LocalFleetSnapshot>,
    pub status: String,
    pub fresh: bool,
    failed: bool,
}

pub struct Model {
    pub hosts: Vec<Host>,
    pub selection: NavigationSelection,
    /// A reply for an older user selection must never replace the workspace.
    pub generation: u64,
    pub initialized: bool,
}

#[derive(Debug, PartialEq)]
pub enum ReadAction {
    Ignored,
    Applied,
    Retry,
}

impl Model {
    pub fn new(connections: Vec<ConnectionSummary>) -> Self {
        let mut hosts = vec![Host {
            connection: None,
            label: "Local owner".into(),
            snapshot: None,
            status: "Connecting…".into(),
            fresh: false,
            failed: false,
        }];
        hosts.extend(connections.into_iter().map(|connection| Host {
            connection: Some(connection.id),
            label: connection.destination,
            snapshot: None,
            status: "Not connected".into(),
            fresh: false,
            failed: false,
        }));
        Self {
            hosts,
            selection: NavigationSelection {
                schema_version: 1,
                connection_id: None,
                yard_name: None,
                tab: "Overview".into(),
            },
            generation: 0,
            initialized: false,
        }
    }

    pub fn host(&self) -> &Host {
        self.hosts
            .iter()
            .find(|host| host.connection == self.selection.connection_id)
            .unwrap_or(&self.hosts[0])
    }

    pub fn yard(&self) -> Option<&LocalYard> {
        let name = self.selection.yard_name.as_ref()?;
        self.host()
            .snapshot
            .as_ref()?
            .owner
            .yards
            .iter()
            .find(|yard| &yard.name == name)
    }

    pub fn update(
        &mut self,
        connection: &Option<String>,
        result: Result<LocalFleetSnapshot, NativeError>,
    ) {
        let Some(host) = self
            .hosts
            .iter_mut()
            .find(|host| &host.connection == connection)
        else {
            return;
        };
        match result {
            Ok(snapshot) => {
                host.status = "Connected".into();
                host.snapshot = Some(snapshot);
                host.fresh = true;
                host.failed = false;
            }
            Err(error) => {
                // Retain the last authoritative view, visibly marked unavailable.
                host.status = format!("{}: {}", error.code, error.message);
                host.fresh = false;
                host.failed = true;
            }
        }
        if self.selection.connection_id == *connection {
            self.normalize();
        }
    }

    pub fn initialize(&mut self, saved: Option<NavigationSelection>) {
        if !self.initialized {
            self.restore(saved);
            self.initialized = true;
        }
    }

    pub fn select_tab(&mut self, tab: String) -> bool {
        if !self.initialized {
            return false;
        }
        self.selection.tab = tab;
        self.normalize();
        true
    }

    /// Preserve inactive inventory as explicitly stale. A failed read remains
    /// visible until a successful native hint or an explicit user read, so an
    /// opening failure's disconnect hints cannot create an immediate retry loop.
    pub fn invalidate(&mut self, connection: &Option<String>, available: bool) -> bool {
        let Some(host) = self
            .hosts
            .iter_mut()
            .find(|host| &host.connection == connection)
        else {
            return false;
        };
        host.fresh = false;
        if !host.failed {
            host.status = "Inventory changed; refreshing…".into();
        }
        self.initialized
            && self.selection.connection_id == *connection
            && (!host.failed || available)
    }

    pub fn read_complete(
        &mut self,
        generation: u64,
        connection: &Option<String>,
        current: bool,
        result: Result<LocalFleetSnapshot, NativeError>,
    ) -> ReadAction {
        if !self.accepts_reply(generation, connection) {
            return ReadAction::Ignored;
        }
        // Errors are retained instead of endlessly retrying an opening failure
        // whose own transport callback invalidated the read. No callback payload
        // can supersede a successful read; only a subsequent explicit read can.
        if result.is_err() || current {
            self.update(connection, result);
            ReadAction::Applied
        } else {
            ReadAction::Retry
        }
    }

    pub fn restore(&mut self, saved: Option<NavigationSelection>) {
        if let Some(saved) = saved.filter(|saved| {
            self.hosts.iter().any(|host| {
                host.connection == saved.connection_id
                    && host.snapshot.as_ref().is_some_and(|snapshot| {
                        saved.yard_name.as_ref().is_none_or(|name| {
                            snapshot.owner.yards.iter().any(|yard| &yard.name == name)
                        })
                    })
            })
        }) {
            self.selection = saved;
        } else {
            self.selection.connection_id = None;
            self.selection.yard_name = self.hosts[0].snapshot.as_ref().and_then(|snapshot| {
                snapshot
                    .owner
                    .yards
                    .iter()
                    .find(|yard| yard.name == snapshot.current_yard_name)
                    .map(|yard| yard.name.clone())
            });
            self.selection.tab = "Overview".into();
        }
        self.normalize();
    }

    pub fn select(&mut self, connection: Option<String>, yard: Option<String>) -> bool {
        if !self.initialized || !self.hosts.iter().any(|host| host.connection == connection) {
            return false;
        }
        let changed_owner = self.selection.connection_id != connection;
        self.selection.connection_id = connection;
        self.selection.yard_name = yard;
        self.selection.tab = "Overview".into();
        self.generation += 1;
        self.normalize();
        if changed_owner {
            let host = self
                .hosts
                .iter_mut()
                .find(|host| host.connection == self.selection.connection_id)
                .unwrap();
            host.fresh = false;
            host.status = "Refreshing…".into();
        }
        changed_owner || !self.host().fresh
    }

    fn normalize(&mut self) {
        if self.selection.yard_name.is_some() && self.yard().is_none() {
            self.selection.yard_name = self.host().snapshot.as_ref().and_then(|snapshot| {
                snapshot
                    .owner
                    .yards
                    .iter()
                    .find(|yard| yard.name == snapshot.current_yard_name)
                    .map(|yard| yard.name.clone())
            });
        }
        let list_tab = if self.selection.yard_name.is_some() {
            "Projects"
        } else {
            "Yards"
        };
        if self.selection.tab != "Overview" && self.selection.tab != list_tab {
            self.selection.tab = "Overview".into();
        }
    }

    pub fn accepts_reply(&self, generation: u64, connection: &Option<String>) -> bool {
        self.generation == generation && self.selection.connection_id == *connection
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use veranda_lib::local_fleet::{LocalOwnerHost, LocalProject};
    fn fleet(connection: Option<String>) -> LocalFleetSnapshot {
        LocalFleetSnapshot {
            engine_version: "test".into(),
            observed_at: "now".into(),
            current_yard_name: "default".into(),
            owner: LocalOwnerHost {
                id: "host".into(),
                yards: vec![LocalYard {
                    name: "default".into(),
                    kind: "local".into(),
                    state: "RUNNING".into(),
                    projects: vec![LocalProject {
                        id: "p".into(),
                        name: "Project α".into(),
                    }],
                }],
            },
            connection_id: connection,
            capabilities: vec![],
            veranda_version: None,
        }
    }
    fn remote() -> ConnectionSummary {
        ConnectionSummary {
            id: "remote".into(),
            host_id: "host".into(),
            destination: "dev@example".into(),
            auth_label: "SSH agent".into(),
        }
    }
    #[test]
    fn restores_available_remote_and_projects_tab() {
        let mut model = Model::new(vec![remote()]);
        model.update(&None, Ok(fleet(None)));
        model.update(&Some("remote".into()), Ok(fleet(Some("remote".into()))));
        let saved = NavigationSelection {
            schema_version: 1,
            connection_id: Some("remote".into()),
            yard_name: Some("default".into()),
            tab: "Projects".into(),
        };
        model.restore(Some(saved.clone()));
        assert_eq!(model.selection, saved);
        assert_eq!(model.yard().unwrap().projects[0].name, "Project α");
    }
    #[test]
    fn missing_remote_or_yard_falls_back_to_local_current_yard() {
        for name in [None, Some("missing".into())] {
            let mut model = Model::new(vec![remote()]);
            model.update(&None, Ok(fleet(None)));
            let saved = NavigationSelection {
                schema_version: 1,
                connection_id: Some("remote".into()),
                yard_name: name,
                tab: "Projects".into(),
            };
            model.restore(Some(saved));
            assert_eq!(model.selection.connection_id, None);
            assert_eq!(model.selection.yard_name.as_deref(), Some("default"));
        }
    }
    #[test]
    fn stale_scope_reply_cannot_replace_new_selection() {
        let mut model = Model::new(vec![remote()]);
        model.initialize(None);
        model.select(None, None);
        let old = model.generation;
        model.select(Some("remote".into()), None);
        assert!(!model.accepts_reply(old, &None));
        assert!(model.accepts_reply(model.generation, &Some("remote".into())));
    }
    #[test]
    fn failure_keeps_names_and_explicit_error() {
        let mut model = Model::new(vec![]);
        model.update(&None, Ok(fleet(None)));
        model.update(
            &None,
            Err(NativeError::new(
                "incompatible_engine",
                "Install matching releases.",
            )),
        );
        assert_eq!(model.host().snapshot.as_ref().unwrap().owner.yards.len(), 1);
        assert!(model.host().status.contains("incompatible_engine"));
    }

    #[test]
    fn review_regression_hint_before_queued_read_cannot_restore_old_inventory() {
        let mut model = Model::new(vec![]);
        model.initialize(None);
        let mut hints = crate::worker::Invalidations::default();
        let started = hints.version(&None);
        // A newer native state or disconnect invalidates the in-flight read.
        hints.hint(None, false);
        assert!(model.invalidate(&None, false));
        let action =
            model.read_complete(0, &None, started == hints.version(&None), Ok(fleet(None)));
        assert_eq!(action, ReadAction::Retry);
        assert!(model.host().snapshot.is_none());
        let mut newer = fleet(None);
        newer.owner.yards[0].projects[0].name = "newer".into();
        let quiet = hints.version(&None);
        assert_eq!(
            model.read_complete(0, &None, quiet == hints.version(&None), Ok(newer)),
            ReadAction::Applied
        );
        assert_eq!(
            model.host().snapshot.as_ref().unwrap().owner.yards[0].projects[0].name,
            "newer"
        );
        assert!(model.host().fresh);
    }

    #[test]
    fn review_regression_delayed_hint_cannot_replace_newer_read() {
        let mut model = Model::new(vec![]);
        model.initialize(None);
        let mut hints = crate::worker::Invalidations::default();
        let mut newer = fleet(None);
        newer.owner.yards[0].projects[0].name = "newer".into();
        assert_eq!(
            model.read_complete(0, &None, true, Ok(newer.clone())),
            ReadAction::Applied
        );
        // An older callback payload arriving after the read is only a hint.
        // The UI never consumes the callback's snapshot or observed_at.
        hints.hint(None, true);
        assert!(model.invalidate(&None, true));
        assert_eq!(
            model.host().snapshot.as_ref().unwrap().owner.yards[0].projects[0].name,
            "newer"
        );
        assert!(!model.host().fresh);
        let quiet = hints.version(&None);
        assert_eq!(
            model.read_complete(0, &None, quiet == hints.version(&None), Ok(newer)),
            ReadAction::Applied
        );
        assert!(model.host().fresh);
    }

    #[test]
    fn review_regression_owner_reselection_must_not_reuse_unvalidated_cache() {
        let mut model = Model::new(vec![remote()]);
        model.update(&None, Ok(fleet(None)));
        model.update(&Some("remote".into()), Ok(fleet(Some("remote".into()))));
        model.initialize(None);
        assert!(model.select(Some("remote".into()), None));
        assert!(!model.invalidate(&None, true)); // Inactive owner retained stale.
        assert!(!model.hosts[0].fresh);
        assert!(
            model.select(None, None),
            "returning owner needs authoritative read"
        );
        assert!(!model.host().fresh);
        let mut newer = fleet(None);
        newer.owner.yards[0].projects[0].name = "newer".into();
        assert_eq!(
            model.read_complete(model.generation, &None, true, Ok(newer)),
            ReadAction::Applied
        );
        assert_eq!(
            model.host().snapshot.as_ref().unwrap().owner.yards[0].projects[0].name,
            "newer"
        );
    }

    #[test]
    fn review_regression_late_startup_cannot_replace_user_selection() {
        let mut model = Model::new(vec![remote()]);
        model.update(&None, Ok(fleet(None))); // Local finishes before slow remote.
        assert!(!model.select(None, Some("default".into())));
        assert!(model.selection.yard_name.is_none()); // Navigation is gated.
        model.initialize(None);
        model.select(None, Some("default".into()));
        model.selection.tab = "Projects".into();
        let user = model.selection.clone();
        let generation = model.generation;
        model.initialize(None); // Even a duplicate initialization is harmless.
        assert_eq!(model.selection, user);
        assert_eq!(model.generation, generation);
    }

    #[test]
    fn startup_tab_choice_cannot_save_before_selection_restoration() {
        let mut model = Model::new(vec![]);
        let initial = model.selection.clone();
        assert!(!model.select_tab("Yards".into()));
        assert_eq!(model.selection, initial);
        model.update(&None, Ok(fleet(None)));
        model.initialize(None);
        assert!(model.select_tab("Projects".into()));
        assert_eq!(model.selection.tab, "Projects");
    }

    #[test]
    fn failed_opening_with_hints_stops_until_recovery_or_explicit_refresh() {
        let mut model = Model::new(vec![]);
        model.initialize(None);
        let mut hints = crate::worker::Invalidations::default();
        let started = hints.version(&None);
        hints.hint(None, false); // Transport error from the opening read.
        assert_eq!(
            model.read_complete(
                0,
                &None,
                started == hints.version(&None),
                Err(NativeError::new("disconnected", "Unavailable"))
            ),
            ReadAction::Applied
        );
        assert!(model.host().status.contains("disconnected"));
        for _ in 0..3 {
            hints.hint(None, false);
            assert!(!model.invalidate(&None, false));
        }
        assert!(model.invalidate(&None, true)); // Native monitor recovered.
        let quiet = hints.version(&None);
        assert_eq!(
            model.read_complete(0, &None, quiet == hints.version(&None), Ok(fleet(None))),
            ReadAction::Applied
        );
        assert!(model.host().fresh);
    }
}
