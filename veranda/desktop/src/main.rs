mod pages;
use gtk::glib;
use gtk::prelude::*;
use gtk::{Application, ApplicationWindow, Orientation};
use std::cell::{Cell, RefCell};
use std::path::PathBuf;
use std::rc::{Rc, Weak};
use subyard_veranda::model::{Model, ReadAction};
use subyard_veranda::operation::{Operation, Phase as OperationPhase};
use subyard_veranda::remote::{Flow, Phase};
use subyard_veranda::state::{AppState, OperationNote, Scope};
use subyard_veranda::worker::{Details, OwnerCommand};
use subyard_veranda::worker::{Update, Worker};
use veranda_lib::client::product_version;

struct View {
    pages: pages::Pages,
    weak: RefCell<Weak<View>>,
    details: RefCell<Option<(u64, Scope, u64, Details)>>,
    state: RefCell<AppState>,
    state_writable: Cell<bool>,
    state_revision: Cell<u64>,
    operation: RefCell<Option<Operation>>,
    next_request: Cell<u64>,
    window: ApplicationWindow,
    navigation: gtk::ListBox,
    heading: gtk::Label,
    status: gtk::Label,
    detail: gtk::Label,
    list: gtk::Box,
    list_page: gtk::StackPage,
    stack: gtk::Stack,
    switcher: gtk::StackSwitcher,
    refresh: gtk::Button,
    add_remote: gtk::Button,
    repair: gtk::Button,
    remote_panel: gtk::Box,
    workspace_content: gtk::Box,
    destination: gtk::Entry,
    remote_title: gtk::Label,
    remote_detail: gtk::Label,
    assess: gtk::Button,
    accept: gtk::Button,
    cancel: gtk::Button,
    flow: RefCell<Option<Flow>>,
    next_flow: Cell<u64>,
    rows: RefCell<Vec<(Option<String>, Option<String>)>>,
    model: RefCell<Model>,
    worker: RefCell<Worker>,
    rendering: Cell<bool>,
    closing: Cell<bool>,
    closed: Cell<bool>,
}

impl View {
    fn scope(&self) -> Scope {
        let model = self.model.borrow();
        Scope {
            connection: model.selection.connection_id.clone(),
            yard: model.selection.yard_name.clone(),
        }
    }
    fn current(&self, generation: u64, scope: &Scope) -> bool {
        !self.closing.get()
            && self.model.borrow().generation == generation
            && self.scope() == *scope
    }
    fn request_details(&self) {
        if self.model.borrow().initialized && self.model.borrow().host().fresh {
            self.worker
                .borrow()
                .details(self.model.borrow().generation, self.scope());
        }
    }
    fn save_state(&self) {
        if !self.state_writable.get() {
            return;
        }
        let revision = self
            .state_revision
            .get()
            .checked_add(1)
            .expect("state revision exhausted");
        self.state_revision.set(revision);
        self.worker
            .borrow()
            .save_state(revision, self.state.borrow().clone());
    }
    fn callback(&self) -> pages::Callback {
        let weak = self.weak.borrow().clone();
        let scope = self.scope();
        let generation = self.model.borrow().generation;
        Rc::new(move |action| {
            if let Some(view) = weak
                .upgrade()
                .filter(|view| view.current(generation, &scope))
            {
                view.action(action);
            }
        })
    }
    fn render_details(&self) {
        let scope = self.scope();
        let generation = self.model.borrow().generation;
        let details = self.details.borrow();
        if let Some((_, _, _, details)) = details.as_ref().filter(|(g, s, invalidation, _)| {
            *g == generation
                && *s == scope
                && self.model.borrow().host().fresh
                && self
                    .worker
                    .borrow()
                    .read_is_current(&scope.connection, *invalidation)
        }) {
            let enabled = self.model.borrow().host().fresh && self.flow.borrow().is_none();
            self.pages.render(
                &self.stack,
                generation,
                &scope,
                details,
                &self.state.borrow(),
                enabled,
                self.state_writable.get(),
                self.callback(),
            );
            // Project rows contain names only. Sessions use owner descriptors.
            if let Some(yard) = self.model.borrow().yard() {
                if details
                    .capabilities()
                    .iter()
                    .any(|cap| cap == "session-prepare-v1")
                {
                    while let Some(child) = self.list.first_child() {
                        self.list.remove(&child);
                    }
                    let callback = self.callback();
                    for project in &yard.projects {
                        let row = gtk::Box::new(Orientation::Horizontal, 8);
                        let name = gtk::Label::new(Some(&project.name));
                        name.set_xalign(0.0);
                        name.set_hexpand(true);
                        row.append(&name);
                        for (title, kind) in [("_Code", "vscode"), ("_Shell", "shell")] {
                            let button = gtk::Button::with_mnemonic(title);
                            button.set_sensitive(enabled);
                            button.update_property(&[gtk::accessible::Property::Label(&format!(
                                "{} {}",
                                title.trim_start_matches('_'),
                                project.name
                            ))]);
                            let cb = callback.clone();
                            let project = subyard_veranda::model::session_project(project);
                            button.connect_clicked(move |_| {
                                cb(pages::Action::Session(
                                    Some(project.clone()),
                                    kind.into(),
                                    false,
                                ))
                            });
                            row.append(&button);
                        }
                        self.list.append(&row);
                    }
                }
            }
        } else {
            self.pages.unavailable(
                &self.stack,
                generation,
                &scope,
                "Details unavailable. Select an available owner or Refresh.",
                self.state_writable.get(),
            );
            if let Some(snapshot) = &self.model.borrow().host().snapshot {
                self.pages.render_actions(
                    &scope,
                    &snapshot.capabilities,
                    self.model.borrow().host().fresh,
                    self.state_writable.get(),
                    &self.callback(),
                );
            }
        }
    }
    fn action(&self, action: pages::Action) {
        if self.closing.get() {
            return;
        }
        match action {
            pages::Action::Draft(field, value) => {
                if !self.state_writable.get() {
                    return;
                }
                match self.state.borrow_mut().remember(self.scope(), field, value) {
                    Ok(()) => {}
                    Err(error) => {
                        self.status
                            .set_text(&format!("{}: {}", error.code, error.message));
                        return;
                    }
                }
                self.save_state();
            }
            pages::Action::Error(message) => self.status.set_text(&message),
            pages::Action::Session(project, kind, copy) => {
                if !self.model.borrow().host().fresh {
                    return;
                }
                if !self.worker.borrow().owner(OwnerCommand::Launch {
                    generation: self.model.borrow().generation,
                    scope: self.scope(),
                    project,
                    kind,
                    copy,
                }) {
                    self.status.set_text(
                        "Another owner request is being prepared. Try again after it finishes.",
                    );
                }
            }
            pages::Action::Plan(scope, command, arguments) => {
                if !self.state_writable.get()
                    || !self.model.borrow().host().fresh
                    || self
                        .operation
                        .borrow()
                        .as_ref()
                        .is_some_and(Operation::busy)
                {
                    self.status.set_text("Finish or discard the current owner operation before reviewing another plan.");
                    return;
                }
                let request = self
                    .next_request
                    .get()
                    .checked_add(1)
                    .expect("operation request exhausted");
                self.next_request.set(request);
                if self.worker.borrow().owner(OwnerCommand::Plan {
                    request,
                    scope: scope.clone(),
                    command: command.clone(),
                    arguments,
                }) {
                    *self.operation.borrow_mut() =
                        Some(Operation::planning(request, scope, command));
                    self.render_operation();
                } else {
                    self.status.set_text(
                        "Another owner request is being prepared. Try again after it finishes.",
                    );
                }
            }
        }
    }
    fn render_operation(&self) {
        let operations = self.operation.borrow();
        if let Some(operation) = operations.as_ref() {
            self.pages.operation.set_visible(true);
            self.pages.operation_text.set_text(&format!(
                "Owner operation · {} / {}\n{}",
                operation
                    .scope
                    .connection
                    .as_deref()
                    .unwrap_or("Local owner"),
                operation.scope.yard.as_deref().unwrap_or("Host"),
                operation.text
            ));
            let unknown = self.state.borrow().operations.iter().filter(|note| note.outcome == "unknown" && Some(note.operation_id.as_str()) != operation.operation_id.as_deref()).map(|note| format!("Outcome unknown · Owner {} · {} · {}. Refresh and prepare a fresh plan before retrying.", note.owner_id, note.command, note.operation_id)).collect::<Vec<_>>().join("\n");
            if !unknown.is_empty() {
                self.pages
                    .operation_text
                    .set_text(&format!("{}\n\n{}", operation.text, unknown));
            }
            self.pages
                .execute
                .set_visible(operation.phase == OperationPhase::Review);
            self.pages.execute.set_sensitive(
                self.state_writable.get()
                    && operation.scope.connection == self.scope().connection
                    && self.model.borrow().host().fresh,
            );
            self.pages.discard.set_visible(matches!(
                operation.phase,
                OperationPhase::Planning | OperationPhase::Review | OperationPhase::Discarding
            ));
            self.pages.discard.set_sensitive(matches!(
                operation.phase,
                OperationPhase::Planning | OperationPhase::Review
            ));
            self.pages.cancel.set_visible(matches!(
                operation.phase,
                OperationPhase::Starting | OperationPhase::Running | OperationPhase::Cancelling
            ));
            self.pages
                .cancel
                .set_sensitive(operation.phase == OperationPhase::Running);
        } else {
            let state = self.state.borrow();
            let unknown: Vec<_> = state
                .operations
                .iter()
                .filter(|note| note.outcome == "unknown")
                .collect();
            self.pages
                .operation
                .set_visible(!unknown.is_empty() || !self.state_writable.get());
            self.pages.operation_text.set_text(&unknown.iter().map(|note| format!("Outcome unknown after interruption · Owner {} · {} · {}. Refresh owner state and prepare a fresh plan before retrying.", note.owner_id, note.command, note.operation_id)).collect::<Vec<_>>().join("\n"));
            if !self.state_writable.get() {
                self.pages.operation_text.set_text("Draft storage is unavailable or changed in another process. Reopen Veranda before editing drafts or executing owner operations.");
            }
            self.pages.execute.set_visible(false);
            self.pages.discard.set_visible(false);
            self.pages.cancel.set_visible(false);
        }
    }
    fn execute_operation(&self) {
        if !self.state_writable.get() || self.closing.get() {
            return;
        }
        let mut operations = self.operation.borrow_mut();
        let Some(operation) = operations.as_mut().filter(|op| {
            op.scope.connection == self.scope().connection && self.model.borrow().host().fresh
        }) else {
            return;
        };
        let Some(plan) = operation.plan.as_ref() else {
            return;
        };
        let owner_id = self
            .model
            .borrow()
            .host()
            .snapshot
            .as_ref()
            .map(|snapshot| snapshot.owner.id.clone())
            .unwrap_or_default();
        let note = OperationNote {
            scope: operation.scope.clone(),
            owner_id,
            operation_id: plan.operation_id.clone(),
            command: operation.command.clone(),
            outcome: "unknown".into(),
        };
        let mut state = self.state.borrow().clone();
        if let Err(error) = state.intent(note) {
            self.status
                .set_text(&format!("{}: {}", error.code, error.message));
            return;
        }
        // Queue admission precedes consuming the review; main-thread callbacks
        // cannot interleave with this synchronous bounded queue operation.
        let plan = plan.clone();
        if self.worker.borrow().owner(OwnerCommand::Execute {
            request: operation.request,
            plan,
            state: state.clone(),
        }) {
            operation.take_review();
            *self.state.borrow_mut() = state;
        } else {
            self.status.set_text("Another owner request is queued. Review remains available; try again after it finishes.");
        }
        drop(operations);
        self.render_operation();
    }
    fn discard_operation(&self) {
        let mut operations = self.operation.borrow_mut();
        let Some(operation) = operations.as_mut() else {
            return;
        };
        if operation.phase == OperationPhase::Planning {
            operation.discard_requested = true;
            operation.text = "Waiting for the owner plan before discarding it…".into();
        } else if operation.phase == OperationPhase::Review {
            let Some(plan) = operation.plan.as_ref() else {
                return;
            };
            if self.worker.borrow().owner(OwnerCommand::Discard {
                request: operation.request,
                plan_id: plan.plan_id.clone(),
            }) {
                operation.plan = None;
                operation.phase = OperationPhase::Discarding;
            }
        }
        drop(operations);
        self.render_operation();
    }
    fn cancel_operation(&self) {
        let mut operations = self.operation.borrow_mut();
        let Some(operation) = operations
            .as_mut()
            .filter(|op| op.phase == OperationPhase::Running)
        else {
            return;
        };
        if let Some(id) = &operation.operation_id {
            if self.worker.borrow().owner(OwnerCommand::Cancel {
                request: operation.request,
                operation_id: id.clone(),
            }) {
                operation.phase = OperationPhase::Cancelling;
                operation.text = "Requesting owner cancellation…".into();
            }
        }
        drop(operations);
        self.render_operation();
    }

    fn render(&self) {
        self.rendering.set(true);
        let focused = self.navigation.has_focus() || self.navigation.focus_child().is_some();
        while let Some(child) = self.navigation.first_child() {
            self.navigation.remove(&child);
        }
        let model = self.model.borrow();
        let mut rows = self.rows.borrow_mut();
        rows.clear();
        let mut selected = None;
        for host in &model.hosts {
            let label = gtk::Label::new(Some(&host.label));
            label.set_xalign(0.0);
            label.set_margin_top(10);
            label.set_margin_bottom(10);
            label.set_margin_start(12);
            label.set_margin_end(12);
            let row = gtk::ListBoxRow::new();
            row.set_child(Some(&label));
            self.navigation.append(&row);
            if host.connection == model.selection.connection_id
                && model.selection.yard_name.is_none()
            {
                selected = Some(row);
            }
            rows.push((host.connection.clone(), None));
            if let Some(snapshot) = &host.snapshot {
                for yard in &snapshot.owner.yards {
                    let label = gtk::Label::new(Some(&yard.name));
                    label.set_xalign(0.0);
                    label.set_margin_start(28);
                    label.set_margin_end(12);
                    label.set_margin_top(8);
                    label.set_margin_bottom(8);
                    let row = gtk::ListBoxRow::new();
                    row.set_child(Some(&label));
                    row.update_property(&[gtk::accessible::Property::Label(&format!(
                        "{} / {}",
                        host.label, yard.name
                    ))]);
                    self.navigation.append(&row);
                    if host.connection == model.selection.connection_id
                        && model.selection.yard_name.as_ref() == Some(&yard.name)
                    {
                        selected = Some(row);
                    }
                    rows.push((host.connection.clone(), Some(yard.name.clone())));
                }
            }
        }
        if let Some(row) = selected {
            self.navigation.select_row(Some(&row));
            if focused {
                row.grab_focus();
            }
        }
        self.status.set_text(&model.host().status);
        self.repair
            .set_sensitive(model.selection.connection_id.is_some() && self.flow.borrow().is_none());
        while let Some(child) = self.list.first_child() {
            self.list.remove(&child);
        }
        let list_name;
        if let Some(yard) = model.yard() {
            self.heading
                .set_text(&format!("{} / {}", model.host().label, yard.name));
            self.detail.set_text(&format!(
                "Yard state: {}\n{} projects\nObserved: {}",
                yard.state,
                yard.projects.len(),
                model.host().snapshot.as_ref().unwrap().observed_at
            ));
            list_name = "Projects";
            for project in &yard.projects {
                self.add_name(&project.name);
            }
            if yard.projects.is_empty() {
                self.add_name("No projects");
            }
        } else {
            self.heading.set_text(&model.host().label);
            list_name = "Yards";
            if let Some(snapshot) = &model.host().snapshot {
                self.detail.set_text(&format!(
                    "Owner: {}\nYard {} · Veranda {}\n{} yards\nObserved: {}",
                    snapshot.owner.id,
                    snapshot.engine_version,
                    product_version(),
                    snapshot.owner.yards.len(),
                    snapshot.observed_at
                ));
                for yard in &snapshot.owner.yards {
                    self.add_name(&format!("{} — {}", yard.name, yard.state));
                }
                if snapshot.owner.yards.is_empty() {
                    self.add_name("No yards");
                }
            } else {
                self.detail
                    .set_text("Select this owner or Refresh to load its fleet.");
            }
        }
        self.list_page.set_title(list_name);
        self.list_page.set_name(list_name);
        self.stack.set_visible_child_name(&model.selection.tab);
        drop(rows);
        drop(model);
        self.render_details();
        self.render_operation();
        self.rendering.set(false);
    }

    fn open_remote(&self, repair: bool) {
        if self.flow.borrow().is_some() || !self.model.borrow().initialized {
            return;
        }
        let (id, destination) = if repair {
            let model = self.model.borrow();
            (
                model.selection.connection_id.clone(),
                model.host().label.clone(),
            )
        } else {
            (None, String::new())
        };
        if repair && id.is_none() {
            return;
        }
        let next = self
            .next_flow
            .get()
            .checked_add(1)
            .expect("flow counter exhausted");
        self.next_flow.set(next);
        *self.flow.borrow_mut() = Some(Flow::new(next, id));
        self.remote_title.set_text(if repair {
            "Repair host key"
        } else {
            "Connect remote host"
        });
        self.destination.set_text(&destination);
        self.destination.set_editable(!repair);
        self.destination.set_sensitive(true);
        self.remote_detail.set_text("Use the system SSH agent. Enter host or user@host, optionally :port. Verify the public fingerprint independently before accepting.");
        self.assess.set_sensitive(true);
        self.accept.set_visible(false);
        self.cancel.set_sensitive(true);
        self.remote_panel.set_visible(true);
        self.workspace_content.set_visible(false);
        self.navigation.set_sensitive(false);
        self.refresh.set_sensitive(false);
        self.add_remote.set_sensitive(false);
        self.repair.set_sensitive(false);
        self.destination.grab_focus();
    }

    fn assess_remote(&self) {
        let mut flow = self.flow.borrow_mut();
        let Some(flow) = flow.as_mut().filter(|flow| flow.phase == Phase::Entry) else {
            return;
        };
        flow.attempt = std::sync::Arc::new(veranda_lib::client::ConnectionAttempt::default());
        flow.review = None;
        flow.phase = Phase::Assessing;
        self.assess.set_sensitive(false);
        self.destination.set_sensitive(false);
        self.remote_detail
            .set_text("Assessing the public SSH host key…");
        self.worker.borrow().assess(
            flow.id,
            self.destination.text().to_string(),
            flow.repair.clone(),
            flow.attempt.clone(),
        );
    }

    fn accept_remote(&self) {
        let mut flow = self.flow.borrow_mut();
        let Some(flow) = flow.as_mut() else {
            return;
        };
        let Some(review) = flow.accept() else {
            return;
        };
        self.accept.set_sensitive(false);
        self.remote_detail.set_text("Connecting with the system SSH agent; verifying versions, capabilities and owner identity…");
        self.worker
            .borrow()
            .connect(flow.id, review, flow.attempt.clone());
    }

    fn cancel_remote(&self) {
        let mut flow = self.flow.borrow_mut();
        let Some(flow) = flow.as_mut().filter(|flow| flow.phase != Phase::Cancelling) else {
            return;
        };
        flow.phase = Phase::Cancelling;
        self.assess.set_sensitive(false);
        self.accept.set_sensitive(false);
        self.cancel.set_sensitive(false);
        self.remote_detail.set_text("Cancelling connection…");
        self.worker
            .borrow_mut()
            .cancel(flow.id, flow.attempt.clone());
    }

    fn finish_remote(&self) {
        self.flow.borrow_mut().take();
        self.remote_panel.set_visible(false);
        self.workspace_content.set_visible(true);
        self.navigation.set_sensitive(true);
        self.refresh.set_sensitive(true);
        self.add_remote.set_sensitive(true);
        self.repair
            .set_sensitive(self.model.borrow().selection.connection_id.is_some());
    }

    fn add_name(&self, name: &str) {
        let label = gtk::Label::new(Some(name));
        label.set_xalign(0.0);
        label.set_selectable(true);
        label.set_wrap(true);
        self.list.append(&label);
    }

    fn read(&self, explicit: bool) {
        let model = self.model.borrow();
        if self.worker.borrow().read(
            model.generation,
            model.selection.connection_id.clone(),
            model.selection.yard_name.clone(),
            explicit,
        ) {
            self.status.set_text("Refreshing…");
            self.worker.borrow().details(
                model.generation,
                Scope {
                    connection: model.selection.connection_id.clone(),
                    yard: model.selection.yard_name.clone(),
                },
            );
        }
    }

    fn events(&self) {
        if !self.model.borrow().initialized {
            return;
        }
        for event in self.worker.borrow().take_operations() {
            if let Some(id) = &event.operation_id {
                let changed = self
                    .operation
                    .borrow_mut()
                    .as_mut()
                    .is_some_and(|operation| {
                        operation.terminal(id, &event.state, event.message.as_deref().unwrap_or(""))
                    });
                if changed {
                    self.state.borrow_mut().outcome(id, &event.state);
                    self.save_state();
                    self.render_operation();
                } else if self.operation.borrow().as_ref().is_some_and(|operation| {
                    operation.operation_id.as_ref() == Some(id)
                        && matches!(
                            operation.phase,
                            OperationPhase::Running | OperationPhase::Cancelling
                        )
                }) {
                    self.pages.operation_text.set_text(&format!(
                        "{}: {}",
                        event.state,
                        event.message.as_deref().unwrap_or("")
                    ));
                }
            }
        }
        let mut changed = false;
        let mut read = false;
        for (connection, available) in self.worker.borrow().take_events() {
            changed = true;
            read |= self.model.borrow_mut().invalidate(&connection, available);
        }
        if changed {
            self.render();
        }
        if read {
            self.read(false);
        }
    }
}

fn store_root() -> Result<PathBuf, &'static str> {
    let root = std::env::var_os("XDG_DATA_HOME")
        .filter(|value| !value.is_empty())
        .map(PathBuf::from)
        .or_else(|| std::env::var_os("HOME").map(|home| PathBuf::from(home).join(".local/share")))
        .ok_or("A user data directory is required.")?;
    if !root.is_absolute() {
        return Err("The user data directory must be absolute.");
    }
    Ok(root.join("subyard/veranda"))
}

fn activate(app: &Application, root: PathBuf) {
    if let Some(window) = app.active_window() {
        window.present();
        return;
    }
    let (worker, updates) = Worker::start(root);
    let window = ApplicationWindow::builder()
        .application(app)
        .title("Subyard Veranda")
        .default_width(1000)
        .default_height(680)
        .build();
    let shell = gtk::Box::new(Orientation::Vertical, 0);
    let bar = gtk::Box::new(Orientation::Horizontal, 12);
    bar.set_margin_start(12);
    bar.set_margin_end(12);
    bar.set_margin_top(12);
    bar.set_margin_bottom(12);
    let title = gtk::Label::new(Some("Subyard Veranda"));
    title.set_hexpand(true);
    title.set_xalign(0.0);
    let refresh = gtk::Button::with_mnemonic("_Refresh");
    refresh.set_sensitive(false);
    bar.append(&title);
    let add_remote = gtk::Button::with_mnemonic("_Connect remote host");
    add_remote.set_sensitive(false);
    let repair = gtk::Button::with_mnemonic("Repair host _key");
    repair.set_sensitive(false);
    bar.append(&repair);
    bar.append(&refresh);
    shell.append(&bar);
    let remote_panel = gtk::Box::new(Orientation::Vertical, 8);
    remote_panel.set_margin_start(24);
    remote_panel.set_margin_end(24);
    remote_panel.set_margin_bottom(16);
    let remote_title = gtk::Label::new(None);
    remote_title.set_xalign(0.0);
    remote_title.add_css_class("title-2");
    let destination = gtk::Entry::builder()
        .placeholder_text("user@host:port")
        .max_length(320)
        .build();
    destination.update_property(&[gtk::accessible::Property::Label("Remote SSH destination")]);
    let remote_detail = gtk::Label::new(None);
    remote_detail.set_xalign(0.0);
    remote_detail.set_wrap(true);
    remote_detail.set_selectable(true);
    let actions = gtk::Box::new(Orientation::Horizontal, 12);
    let assess = gtk::Button::with_mnemonic("_Review host key");
    let accept = gtk::Button::with_mnemonic("_Accept and connect");
    let cancel = gtk::Button::with_mnemonic("Ca_ncel");
    actions.append(&assess);
    actions.append(&accept);
    actions.append(&cancel);
    remote_panel.append(&remote_title);
    remote_panel.append(&destination);
    remote_panel.append(&remote_detail);
    remote_panel.append(&actions);
    remote_panel.set_visible(false);
    let body = gtk::Paned::new(Orientation::Horizontal);
    let navigation = gtk::ListBox::new();
    navigation.set_selection_mode(gtk::SelectionMode::Single);
    navigation.set_sensitive(false);
    navigation.update_property(&[gtk::accessible::Property::Label("Owner hosts and yards")]);
    let scroll = gtk::ScrolledWindow::builder()
        .child(&navigation)
        .min_content_width(240)
        .build();
    let navigator = gtk::Box::new(Orientation::Vertical, 12);
    scroll.set_vexpand(true);
    navigator.append(&scroll);
    navigator.append(&add_remote);
    body.set_start_child(Some(&navigator));
    body.set_resize_start_child(false);
    let workspace = gtk::Box::new(Orientation::Vertical, 16);
    workspace.set_margin_start(24);
    workspace.set_margin_end(24);
    workspace.set_margin_top(16);
    workspace.set_margin_bottom(16);
    let heading = gtk::Label::new(Some("Local owner"));
    heading.set_xalign(0.0);
    heading.add_css_class("title-1");
    let status = gtk::Label::new(Some("Connecting…"));
    status.set_xalign(0.0);
    status.set_wrap(true);
    status.set_selectable(true);
    let stack = gtk::Stack::new();
    stack.set_sensitive(false);
    stack.set_vexpand(true);
    let switcher = gtk::StackSwitcher::builder()
        .stack(&stack)
        .halign(gtk::Align::Start)
        .build();
    switcher.set_sensitive(false);
    let detail = gtk::Label::new(None);
    detail.set_xalign(0.0);
    detail.set_yalign(0.0);
    detail.set_wrap(true);
    detail.set_selectable(true);
    let overview = gtk::Box::new(Orientation::Vertical, 12);
    overview.append(&detail);
    stack.add_titled(&overview, Some("Overview"), "Overview");
    let list = gtk::Box::new(Orientation::Vertical, 12);
    // A scrollable workspace keeps arbitrarily long typed names visible.
    let list_content = gtk::Box::new(Orientation::Vertical, 12);
    list_content.append(&list);
    let list_scroll = gtk::ScrolledWindow::builder().child(&list_content).build();
    let list_page = stack.add_titled(&list_scroll, Some("Yards"), "Yards");
    let pages = pages::Pages::new(&stack);
    list_content.append(&pages.creation);
    overview.append(&pages.actions);
    let workspace_content = gtk::Box::new(Orientation::Vertical, 16);
    workspace_content.set_vexpand(true);
    workspace_content.append(&heading);
    workspace_content.append(&status);
    workspace_content.append(&switcher);
    workspace_content.append(&pages.operation);
    workspace_content.append(&stack);
    workspace.append(&workspace_content);
    workspace.append(&remote_panel);
    body.set_end_child(Some(&workspace));
    body.set_vexpand(true);
    shell.append(&body);
    window.set_child(Some(&shell));
    let view = Rc::new(View {
        pages,
        weak: RefCell::new(Weak::new()),
        details: RefCell::new(None),
        state: RefCell::new(AppState::default()),
        state_writable: Cell::new(false),
        state_revision: Cell::new(0),
        operation: RefCell::new(None),
        next_request: Cell::new(0),
        window: window.clone(),
        navigation,
        heading,
        status,
        detail,
        list,
        list_page,
        stack,
        switcher,
        refresh,
        add_remote,
        repair,
        remote_panel,
        workspace_content,
        destination,
        remote_title,
        remote_detail,
        assess,
        accept,
        cancel,
        flow: RefCell::new(None),
        next_flow: Cell::new(0),
        rows: RefCell::new(vec![]),
        model: RefCell::new(Model::new(vec![])),
        worker: RefCell::new(worker),
        rendering: Cell::new(false),
        closing: Cell::new(false),
        closed: Cell::new(false),
    });
    *view.weak.borrow_mut() = Rc::downgrade(&view);
    let weak = Rc::downgrade(&view);
    view.pages.execute.connect_clicked(move |_| {
        if let Some(view) = weak.upgrade() {
            view.execute_operation();
        }
    });
    let weak = Rc::downgrade(&view);
    view.pages.discard.connect_clicked(move |_| {
        if let Some(view) = weak.upgrade() {
            view.discard_operation();
        }
    });
    let weak = Rc::downgrade(&view);
    view.pages.cancel.connect_clicked(move |_| {
        if let Some(view) = weak.upgrade() {
            view.cancel_operation();
        }
    });
    let weak = Rc::downgrade(&view);
    view.add_remote.connect_clicked(move |_| {
        if let Some(view) = weak.upgrade().filter(|view| !view.closing.get()) {
            view.open_remote(false);
        }
    });
    let weak = Rc::downgrade(&view);
    view.repair.connect_clicked(move |_| {
        if let Some(view) = weak.upgrade().filter(|view| !view.closing.get()) {
            view.open_remote(true);
        }
    });
    let weak = Rc::downgrade(&view);
    view.assess.connect_clicked(move |_| {
        if let Some(view) = weak.upgrade().filter(|view| !view.closing.get()) {
            view.assess_remote();
        }
    });
    let weak = Rc::downgrade(&view);
    view.destination.connect_activate(move |_| {
        if let Some(view) = weak.upgrade().filter(|view| !view.closing.get()) {
            view.assess_remote();
        }
    });
    let weak = Rc::downgrade(&view);
    view.accept.connect_clicked(move |_| {
        if let Some(view) = weak.upgrade().filter(|view| !view.closing.get()) {
            view.accept_remote();
        }
    });
    let weak = Rc::downgrade(&view);
    view.cancel.connect_clicked(move |_| {
        if let Some(view) = weak.upgrade().filter(|view| !view.closing.get()) {
            view.cancel_remote();
        }
    });
    let weak = Rc::downgrade(&view);
    view.navigation.connect_row_selected(move |_, row| {
        let Some(view) = weak
            .upgrade()
            .filter(|view| !view.rendering.get() && !view.closing.get())
        else {
            return;
        };
        let Some((connection, yard)) =
            row.and_then(|row| view.rows.borrow().get(row.index() as usize).cloned())
        else {
            return;
        };
        let needs_read = view.model.borrow_mut().select(connection, yard);
        view.worker
            .borrow()
            .save(view.model.borrow().selection.clone());
        view.render();
        if needs_read {
            view.read(true);
        } else {
            view.request_details();
        }
    });
    let weak = Rc::downgrade(&view);
    view.refresh.connect_clicked(move |_| {
        if let Some(view) = weak.upgrade().filter(|view| !view.closing.get()) {
            view.read(true);
        }
    });
    let weak = Rc::downgrade(&view);
    view.stack.connect_visible_child_name_notify(move |stack| {
        let Some(view) = weak.upgrade().filter(|view| {
            !view.rendering.get() && !view.closing.get() && view.model.borrow().initialized
        }) else {
            return;
        };
        if let Some(tab) = stack.visible_child_name() {
            if view.model.borrow_mut().select_tab(tab.to_string()) {
                view.worker
                    .borrow()
                    .save(view.model.borrow().selection.clone());
            }
        }
    });
    let weak = Rc::downgrade(&view);
    window.connect_close_request(move |_| {
        let Some(view) = weak.upgrade() else {
            return glib::Propagation::Proceed;
        };
        if view.closed.get() {
            return glib::Propagation::Proceed;
        }
        if !view.closing.replace(true) {
            view.refresh.set_sensitive(false);
            view.add_remote.set_sensitive(false);
            view.repair.set_sensitive(false);
            view.remote_panel.set_sensitive(false);
            if view.flow.borrow().is_some() {
                view.cancel_remote();
            }
            view.navigation.set_sensitive(false);
            view.status.set_text("Closing connections…");
            view.worker.borrow_mut().close();
        }
        glib::Propagation::Stop
    });
    glib::spawn_future_local(async move {
        while let Ok(update) = updates.recv().await {
            if matches!(update, Update::Closed) {
                view.closed.set(true);
                view.window.close();
                break;
            }
            if view.closing.get() {
                continue;
            }
            // A full notification channel can contain command replies. Drain
            // the latest event state on every wake so the final state survives.
            view.events();
            match update {
                Update::Initialized {
                    connections,
                    selection,
                    local,
                    remote,
                    preference_error,
                    state,
                } => {
                    if view.model.borrow().initialized {
                        continue;
                    }
                    match state {
                        Ok(state) => {
                            *view.state.borrow_mut() = state;
                            view.state_writable.set(true);
                        }
                        Err(error) => {
                            view.pages.operation.set_visible(true);
                            view.pages
                                .operation_text
                                .set_text(&format!("{}: {}", error.code, error.message));
                        }
                    }
                    let mut model = Model::new(connections);
                    model.update(&None, local);
                    if let Some((connection, result)) = remote {
                        model.update(&connection, result);
                    }
                    model.initialize(selection);
                    *view.model.borrow_mut() = model;
                    view.events();
                    view.navigation.set_sensitive(true);
                    view.stack.set_sensitive(true);
                    view.switcher.set_sensitive(true);
                    view.refresh.set_sensitive(true);
                    view.add_remote.set_sensitive(true);
                    view.render();
                    view.request_details();
                    if let Some(error) = preference_error {
                        view.status
                            .set_text(&format!("{}: {}", error.code, error.message));
                    }
                }
                Update::Fleet {
                    generation,
                    connection,
                    invalidation,
                    result,
                } => {
                    let current = view
                        .worker
                        .borrow()
                        .read_is_current(&connection, invalidation);
                    let action = view.model.borrow_mut().read_complete(
                        generation,
                        &connection,
                        current,
                        result,
                    );
                    match action {
                        ReadAction::Applied => {
                            view.render();
                            view.request_details();
                        }
                        ReadAction::Retry => view.read(false),
                        ReadAction::Ignored => {}
                    }
                }
                Update::Saved { selection, result } => {
                    if selection == view.model.borrow().selection {
                        if let Err(error) = result {
                            view.status
                                .set_text(&format!("{}: {}", error.code, error.message));
                        }
                    }
                }
                Update::Reviewed { id, result } => {
                    let current = view
                        .flow
                        .borrow()
                        .as_ref()
                        .is_some_and(|flow| flow.accepts(id, Phase::Assessing));
                    if !current {
                        continue;
                    }
                    match result {
                        Ok(review) => {
                            let previous = review
                                .previous_fingerprint()
                                .map(|fp| format!("Previous fingerprint: {fp}\n"))
                                .unwrap_or_default();
                            view.remote_detail.set_text(&format!(
                                "{}\n{}Fingerprint: {}\n{}",
                                review.destination(),
                                previous,
                                review.fingerprint(),
                                review.consequences().join("\n")
                            ));
                            view.accept.set_label(if review.confirmation() == "never" {
                                "_Connect"
                            } else {
                                "_Accept and connect"
                            });
                            if view
                                .flow
                                .borrow_mut()
                                .as_mut()
                                .unwrap()
                                .reviewed(id, review)
                            {
                                view.accept.set_visible(true);
                                view.accept.set_sensitive(true);
                                view.accept.grab_focus();
                            } else {
                                view.remote_detail.set_text("The trust assessment has unsupported confirmation metadata. Cancel and try again.");
                            }
                        }
                        Err(error) => {
                            view.remote_detail
                                .set_text(&format!("{}: {}", error.code, error.message));
                            view.flow.borrow_mut().as_mut().unwrap().phase = Phase::Entry;
                            view.destination.set_sensitive(true);
                            view.assess.set_sensitive(true);
                        }
                    }
                }
                Update::Connected { id, result } => {
                    let current = view.flow.borrow().as_ref().is_some_and(|flow| {
                        flow.id == id && matches!(flow.phase, Phase::Connecting | Phase::Cancelling)
                    });
                    if !current {
                        continue;
                    }
                    match result {
                        Ok(connection) => {
                            view.model.borrow_mut().connected(connection);
                            view.worker
                                .borrow()
                                .save(view.model.borrow().selection.clone());
                            let finish =
                                view.flow.borrow().as_ref().unwrap().finish_on_connected(id);
                            if finish {
                                view.finish_remote();
                            } else {
                                view.remote_detail.set_text(
                                    "The connection was registered. Finishing connection cleanup…",
                                );
                            }
                            view.render();
                            view.read(true);
                        }
                        Err(error) => {
                            if view.flow.borrow().as_ref().unwrap().phase == Phase::Cancelling {
                                continue;
                            }
                            view.remote_detail
                                .set_text(&format!("{}: {}", error.code, error.message));
                            view.flow.borrow_mut().as_mut().unwrap().phase = Phase::Entry;
                            view.destination.set_sensitive(true);
                            view.assess.set_sensitive(true);
                            view.accept.set_visible(false);
                        }
                    }
                }
                Update::Cancelled { id } => {
                    if view
                        .flow
                        .borrow()
                        .as_ref()
                        .is_some_and(|flow| flow.accepts(id, Phase::Cancelling))
                    {
                        view.finish_remote();
                    }
                }
                Update::Details {
                    generation,
                    scope,
                    invalidation,
                    result,
                } => {
                    if view.current(generation, &scope) {
                        match result {
                            Ok(details)
                                if view
                                    .worker
                                    .borrow()
                                    .read_is_current(&scope.connection, invalidation) =>
                            {
                                *view.details.borrow_mut() =
                                    Some((generation, scope, invalidation, details));
                                view.render();
                            }
                            Ok(_) => view.request_details(),
                            Err(error) => {
                                *view.details.borrow_mut() = None;
                                view.render_details();
                                view.status
                                    .set_text(&format!("{}: {}", error.code, error.message));
                            }
                        }
                    }
                }
                Update::StateSaved {
                    revision: _,
                    result,
                } => {
                    if let Err(error) = result {
                        view.state_writable.set(false);
                        view.status
                            .set_text(&format!("{}: {}", error.code, error.message));
                        view.render_operation();
                    }
                }
                Update::Planned { request, result } => {
                    let mut operations = view.operation.borrow_mut();
                    if let Some(operation) = operations.as_mut().filter(|operation| {
                        operation.request == request && operation.phase == OperationPhase::Planning
                    }) {
                        match result {
                            Ok(plan) => {
                                operation.reviewed(request, plan);
                            }
                            Err(error) => {
                                operation.phase = OperationPhase::Finished;
                                operation.text = format!("{}: {}", error.code, error.message);
                            }
                        }
                    }
                    let discard = operations.as_ref().is_some_and(|op| {
                        op.discard_requested && op.phase == OperationPhase::Review
                    });
                    drop(operations);
                    if discard {
                        view.discard_operation();
                    }
                    view.render_operation();
                    if let Some(operation) = view
                        .operation
                        .borrow()
                        .as_ref()
                        .filter(|op| op.phase == OperationPhase::Review)
                    {
                        if operation
                            .plan
                            .as_ref()
                            .is_some_and(|plan| plan.confirmation() == "prompt-default-no")
                        {
                            view.pages.discard.grab_focus();
                        } else {
                            view.pages.execute.grab_focus();
                        }
                    }
                }
                Update::Executed { request, result } => {
                    let mut operations = view.operation.borrow_mut();
                    if let Some(operation) = operations
                        .as_mut()
                        .filter(|op| op.request == request && op.phase == OperationPhase::Starting)
                    {
                        match result {
                            Ok(id) => {
                                operation.operation_id = Some(id);
                                operation.phase = OperationPhase::Running;
                                operation.text = "The owner is executing the reviewed plan…".into();
                            }
                            Err(proof) => {
                                if let Some(id) = &operation.operation_id {
                                    view.state.borrow_mut().not_submitted(id, &proof);
                                    view.save_state();
                                }
                                operation.phase = OperationPhase::Finished;
                                operation.text = format!(
                                    "Not submitted · {}: {}",
                                    proof.0.code, proof.0.message
                                );
                            }
                        }
                    }
                    drop(operations);
                    view.render_operation();
                }
                Update::Discarded { request, result } => {
                    if let Some(operation) = view.operation.borrow_mut().as_mut().filter(|op| {
                        op.request == request && op.phase == OperationPhase::Discarding
                    }) {
                        operation.phase = OperationPhase::Finished;
                        operation.text = match result {
                            Ok(()) => "Plan discarded. Prepare a fresh plan to continue.".into(),
                            Err(error) => format!("{}: {}", error.code, error.message),
                        };
                    }
                    view.render_operation();
                }
                Update::OperationCancelled { request, result } => {
                    if let Some(operation) = view.operation.borrow_mut().as_mut().filter(|op| {
                        op.request == request && op.phase == OperationPhase::Cancelling
                    }) {
                        operation.text = match result {
                            Ok(()) => {
                                "Cancellation requested. Waiting for the owner outcome…".into()
                            }
                            Err(error) => format!(
                                "{}: {} · Waiting for the owner outcome.",
                                error.code, error.message
                            ),
                        };
                    }
                    view.render_operation();
                }
                Update::Launched {
                    generation,
                    scope,
                    copy,
                    result,
                } => {
                    if view.current(generation, &scope) {
                        match result {
                            Ok(command) if copy => {
                                view.window.clipboard().set_text(&command);
                                view.status.set_text("Shell command copied.");
                            }
                            Ok(_) => view.status.set_text("Session opened."),
                            Err(error) => view
                                .status
                                .set_text(&format!("{}: {}", error.code, error.message)),
                        }
                    }
                }
                Update::Events => {}
                Update::Closed => unreachable!(),
            }
        }
    });
    window.present();
}

fn main() -> glib::ExitCode {
    // Package verification must not initialize GTK, owner I/O or app-local state.
    if std::env::args_os()
        .skip(1)
        .eq([std::ffi::OsString::from("--version")])
    {
        println!("Subyard Veranda {}", product_version());
        return glib::ExitCode::SUCCESS;
    }
    let root = match store_root() {
        Ok(root) => root,
        Err(message) => {
            eprintln!("{message}");
            return glib::ExitCode::FAILURE;
        }
    };
    let app = Application::builder()
        .application_id("org.subyard.Veranda")
        .build();
    app.connect_activate(move |app| activate(app, root.clone()));
    app.run()
}
