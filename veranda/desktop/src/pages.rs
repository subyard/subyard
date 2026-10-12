//! GTK keeps draft widgets alive while authoritative facts and action gates change.
use gtk::prelude::*;
use gtk::Orientation;
use std::cell::{Cell, RefCell};
use std::collections::{BTreeSet, HashMap};
use std::rc::Rc;
use subyard_veranda::presentation::FormLifetime;
use subyard_veranda::state::{repository_url, AppState, Scope};
use subyard_veranda::worker::Details;
use veranda_lib::client::{Fact, HostSyncStatus, Profile, Provenance, Setting};

#[derive(Clone)]
pub enum Action {
    Draft(String, String),
    Plan(Scope, String, Vec<String>),
    Session(Option<String>, String, bool),
    Error(String),
}
pub type Callback = Rc<dyn Fn(Action)>;

struct SettingRow {
    root: gtk::Box,
    fact: gtk::Label,
    editor: gtk::TextView,
    set: gtk::Button,
    unset: gtk::Button,
}
struct ProfileRow {
    root: gtk::Box,
    fact: gtk::Label,
    check: gtk::CheckButton,
    provision: gtk::Button,
}
struct Creation {
    entry: gtk::Entry,
    preset: gtk::DropDown,
    options: Rc<RefCell<Vec<String>>>,
    create: gtk::Button,
    admitted: Rc<Cell<bool>>,
}
struct Synchronization {
    fact: gtk::Label,
    git: gtk::Label,
    credentials: gtk::Label,
    repository: gtk::Entry,
    actions: Vec<gtk::Button>,
}
#[derive(Default)]
struct Forms {
    lifetime: FormLifetime,
    settings: HashMap<String, SettingRow>,
    profiles: HashMap<String, ProfileRow>,
    selected: Rc<RefCell<BTreeSet<String>>>,
    selection: Option<gtk::Label>,
    apply_profiles: Option<gtk::Button>,
    creation: Option<Creation>,
    synchronization: Option<Synchronization>,
}

pub struct Pages {
    pub actions: gtk::Box,
    pub creation: gtk::Box,
    pub profiles: gtk::Box,
    pub settings: gtk::Box,
    pub sync: gtk::Box,
    pub operation: gtk::Box,
    pub operation_text: gtk::Label,
    pub execute: gtk::Button,
    pub discard: gtk::Button,
    pub cancel: gtk::Button,
    forms: RefCell<Forms>,
    updating: Rc<Cell<bool>>,
    diagnostic_facts: gtk::Label,
}
fn column() -> gtk::Box {
    gtk::Box::new(Orientation::Vertical, 12)
}
fn clear(column: &gtk::Box) {
    while let Some(child) = column.first_child() {
        column.remove(&child);
    }
}
fn label(column: &gtk::Box, text: &str) -> gtk::Label {
    let label = gtk::Label::new(Some(text));
    label.set_xalign(0.0);
    label.set_wrap(true);
    label.set_selectable(true);
    column.append(&label);
    label
}
fn button(column: &gtk::Box, title: &str, callback: &Callback, action: Action) -> gtk::Button {
    let button = gtk::Button::with_mnemonic(title);
    button.set_halign(gtk::Align::Start);
    let callback = callback.clone();
    button.connect_clicked(move |_| callback(action.clone()));
    column.append(&button);
    button
}
fn plan(scope: &Scope, command: &str, args: &[&str]) -> Action {
    Action::Plan(
        scope.clone(),
        command.into(),
        args.iter().map(|s| (*s).into()).collect(),
    )
}
fn provenance(values: &[Provenance]) -> String {
    values
        .iter()
        .map(|p| {
            format!(
                "{} · {} · {}: {}",
                p.scope(),
                p.role(),
                p.status(),
                p.value().unwrap_or("Unavailable")
            )
        })
        .collect::<Vec<_>>()
        .join("\n")
}
fn entry(
    column: &gtk::Box,
    title: &str,
    value: &str,
    field: &str,
    callback: &Callback,
) -> gtk::Entry {
    let entry = gtk::Entry::builder().text(value).max_length(1024).build();
    entry.update_property(&[gtk::accessible::Property::Label(title)]);
    let caption = gtk::Label::new(Some(title));
    caption.set_xalign(0.0);
    caption.set_mnemonic_widget(Some(&entry));
    column.append(&caption);
    column.append(&entry);
    let callback = callback.clone();
    let field = field.to_owned();
    entry.connect_changed(move |entry| {
        callback(Action::Draft(field.clone(), entry.text().to_string()))
    });
    entry
}
fn supports(caps: &[String], cap: &str) -> bool {
    caps.iter().any(|value| value == cap)
}
fn editable(setting: &Setting, scope: &Scope) -> bool {
    setting.editable()
        && !matches!(
            setting.value_type(),
            "absolute-path" | "regular-file" | "executable"
        )
        && setting
            .scopes()
            .iter()
            .any(|s| s == if scope.yard.is_some() { "yard" } else { "host" })
}
fn setting_fact(setting: &Setting) -> String {
    format!("{} · {} · {}\nEffective: {}\nDefault: {}\nApplication: {} · Owner: {}\nAvailable scopes: {}\nOptional: {} · Range: {}…{} · Choices: {}\n{}", setting.name(), setting.kind(), setting.value_type(), if setting.value_available() { setting.value().unwrap_or("") } else { "Unavailable" }, setting.default().unwrap_or("Unavailable"), setting.application(), setting.owner(), setting.scopes().join(", "), setting.optional(), setting.minimum(), setting.maximum(), setting.choices().join(", "), provenance(setting.provenance()))
}
fn profile_fact(profile: &Profile) -> String {
    let convergence = match profile.convergence() {
        "current" => "Applied",
        "changes-required" => "Changes required",
        "not-applicable" => "N/A",
        _ => "Unavailable",
    };
    format!(
        "Observed convergence: {convergence}\nEligibility: {}\n{}\nResources: {}",
        profile.eligibility(),
        profile.diagnostic().unwrap_or(""),
        profile.resources().join(", ")
    )
}
fn diagnostics(facts: &[Fact]) -> String {
    facts
        .iter()
        .map(|fact| {
            format!(
                "{}: {}",
                fact.label(),
                if fact.value().is_empty() {
                    "Unavailable"
                } else {
                    fact.value()
                }
            )
        })
        .collect::<Vec<_>>()
        .join("\n")
}

impl Pages {
    pub fn new(stack: &gtk::Stack) -> Self {
        let profiles = column();
        let settings = column();
        let diagnostics = column();
        let sync = column();
        for (name, widget) in [
            ("Profiles", &profiles),
            ("Sync", &sync),
            ("Diagnostics", &diagnostics),
            ("Settings", &settings),
        ] {
            let scroll = gtk::ScrolledWindow::builder().child(widget).build();
            stack.add_titled(&scroll, Some(name), name);
        }
        let diagnostic_facts = label(&diagnostics, "Unavailable");
        let operation = column();
        let operation_text = gtk::Label::new(None);
        operation_text.set_xalign(0.0);
        operation_text.set_wrap(true);
        operation_text.set_selectable(true);
        let scroll = gtk::ScrolledWindow::builder()
            .child(&operation_text)
            .max_content_height(220)
            .propagate_natural_height(true)
            .build();
        operation.append(&scroll);
        let row = gtk::Box::new(Orientation::Horizontal, 12);
        let execute = gtk::Button::with_mnemonic("_Execute reviewed plan");
        let discard = gtk::Button::with_mnemonic("_Discard plan");
        let cancel = gtk::Button::with_mnemonic("Cancel _operation");
        row.append(&execute);
        row.append(&discard);
        row.append(&cancel);
        operation.append(&row);
        operation.set_visible(false);
        Self {
            actions: gtk::Box::new(Orientation::Horizontal, 8),
            creation: column(),
            profiles,
            settings,
            sync,
            operation,
            operation_text,
            execute,
            discard,
            cancel,
            forms: RefCell::new(Forms::default()),
            updating: Rc::new(Cell::new(false)),
            diagnostic_facts,
        }
    }
    fn context(&self, generation: u64, scope: &Scope) {
        let mut forms = self.forms.borrow_mut();
        if forms.lifetime.change(generation, scope) {
            // Deliberate navigation alone replaces form widgets. Same-scope
            // refresh never detaches an editor or writes its text buffer.
            clear(&self.profiles);
            clear(&self.settings);
            clear(&self.sync);
            clear(&self.creation);
            let lifetime = std::mem::take(&mut forms.lifetime);
            *forms = Forms {
                lifetime,
                ..Forms::default()
            };
        }
        self.creation.set_visible(scope.yard.is_none());
    }
    fn visibility(&self, stack: &gtk::Stack, scope: &Scope, caps: Option<&[String]>) {
        let available = |cap| caps.is_none_or(|caps| supports(caps, cap));
        for (name, visible) in [
            (
                "Profiles",
                scope.yard.is_some() && available("profile-list-v1"),
            ),
            (
                "Sync",
                scope.yard.is_none() && available("host-sync-status-v1"),
            ),
            ("Settings", available("settings-list-v1")),
        ] {
            stack
                .page(&stack.child_by_name(name).unwrap())
                .set_visible(visible);
        }
    }
    pub fn unavailable(
        &self,
        stack: &gtk::Stack,
        generation: u64,
        scope: &Scope,
        message: &str,
        mutable: bool,
    ) {
        self.context(generation, scope);
        self.visibility(stack, scope, None);
        self.diagnostic_facts.set_text(message);
        clear(&self.actions);
        let forms = self.forms.borrow();
        if let Some(selection) = &forms.selection {
            selection
                .set_text("Effective profile selection: Unavailable. Draft selection retained.");
        }
        for (name, row) in &forms.settings {
            row.fact
                .set_text(&format!("{name} · Unavailable. Draft retained."));
            row.editor.set_sensitive(mutable);
            row.set.set_sensitive(false);
            row.unset.set_sensitive(false);
        }
        for row in forms.profiles.values() {
            row.fact.set_text("Observed convergence: Unavailable");
            row.check.set_sensitive(mutable);
            row.provision.set_sensitive(false);
        }
        if let Some(button) = &forms.apply_profiles {
            button.set_sensitive(false);
        }
        if let Some(creation) = &forms.creation {
            creation.entry.set_sensitive(mutable);
            creation.admitted.set(false);
            creation.create.set_sensitive(false);
        }
        if let Some(sync) = &forms.synchronization {
            sync.fact.set_text("Configuration Git · Unavailable");
            sync.git.set_text("Cached Git status · Unavailable");
            sync.credentials
                .set_text("Credentials · REDACTED · Unavailable");
            sync.repository.set_sensitive(mutable);
            for button in &sync.actions {
                button.set_sensitive(false);
            }
        }
    }
    pub fn render_actions(
        &self,
        scope: &Scope,
        caps: &[String],
        enabled: bool,
        mutable: bool,
        callback: &Callback,
    ) {
        let plans = enabled
            && mutable
            && supports(caps, "operation-exact-plan-v1")
            && supports(caps, "operation-steps-v1");
        let sessions = enabled && supports(caps, "session-prepare-v1");
        clear(&self.actions);
        button(
            &self.actions,
            "_Shell",
            callback,
            Action::Session(None, "shell".into(), false),
        )
        .set_sensitive(sessions);
        button(
            &self.actions,
            "_Copy shell command",
            callback,
            Action::Session(None, "shell".into(), true),
        )
        .set_sensitive(sessions);
        if scope.yard.is_some() {
            for (title, command) in [
                ("_Start", "start"),
                ("S_top", "stop"),
                ("_Reconcile", "init"),
            ] {
                button(&self.actions, title, callback, plan(scope, command, &[]))
                    .set_sensitive(plans);
            }
        } else {
            button(
                &self.actions,
                "_Resources (htop)",
                callback,
                Action::Session(None, "resources".into(), false),
            )
            .set_sensitive(sessions);
        }
    }
    pub fn render(
        &self,
        stack: &gtk::Stack,
        generation: u64,
        scope: &Scope,
        details: &Details,
        state: &AppState,
        enabled: bool,
        mutable: bool,
        callback: Callback,
    ) {
        self.context(generation, scope);
        self.visibility(stack, scope, Some(details.capabilities()));
        let caps = details.capabilities();
        let plans = enabled
            && mutable
            && supports(caps, "operation-exact-plan-v1")
            && supports(caps, "operation-steps-v1");
        self.render_actions(scope, caps, enabled, mutable, &callback);
        self.updating.set(true);
        let mut forms = self.forms.borrow_mut();
        match details {
            Details::Yard(details) => {
                let selection = forms
                    .selection
                    .get_or_insert_with(|| label(&self.profiles, ""));
                selection.set_text(&format!(
                    "Effective profile selection: {}\n{}",
                    details.selection().value(),
                    provenance(details.selection().provenance())
                ));
                let current: BTreeSet<_> = details
                    .profiles()
                    .iter()
                    .map(|p| p.name().to_owned())
                    .collect();
                forms
                    .selected
                    .borrow_mut()
                    .retain(|name| current.contains(name));
                forms.profiles.retain(|name, row| {
                    if current.contains(name) || row.check.has_focus() {
                        true
                    } else {
                        self.profiles.remove(&row.root);
                        false
                    }
                });
                for (name, row) in &forms.profiles {
                    if !current.contains(name) {
                        row.fact.set_text("Profile unavailable");
                        row.check.set_sensitive(false);
                        row.provision.set_sensitive(false);
                    }
                }
                for profile in details.profiles() {
                    if !forms.profiles.contains_key(profile.name()) {
                        let root = column();
                        let check = gtk::CheckButton::with_label(profile.name());
                        let field = format!("profile:{}", profile.name());
                        let active = state
                            .value(scope, &field)
                            .map_or(profile.selected(), |value| value == "true");
                        check.set_active(active);
                        if active {
                            forms.selected.borrow_mut().insert(profile.name().into());
                        }
                        let chosen = forms.selected.clone();
                        let name = profile.name().to_owned();
                        let cb = callback.clone();
                        let updating = self.updating.clone();
                        check.connect_toggled(move |check| {
                            if updating.get() {
                                return;
                            }
                            if check.is_active() {
                                chosen.borrow_mut().insert(name.clone());
                            } else {
                                chosen.borrow_mut().remove(&name);
                            }
                            cb(Action::Draft(field.clone(), check.is_active().to_string()));
                        });
                        root.append(&check);
                        let fact = label(&root, "");
                        let provision = button(
                            &root,
                            "Review _provision",
                            &callback,
                            plan(scope, "provision", &[profile.name()]),
                        );
                        self.profiles.append(&root);
                        forms.profiles.insert(
                            profile.name().into(),
                            ProfileRow {
                                root,
                                fact,
                                check,
                                provision,
                            },
                        );
                    }
                    let row = &forms.profiles[profile.name()];
                    row.fact.set_text(&profile_fact(profile));
                    row.check.set_sensitive(mutable && profile.eligible());
                    row.provision.set_visible(profile.provisionable());
                    row.provision
                        .set_sensitive(plans && profile.provisionable() && profile.eligible());
                    // No user draft: update the selection fact without disrupting
                    // a focused check. Editor text buffers are never reset here.
                    if state
                        .value(scope, &format!("profile:{}", profile.name()))
                        .is_none()
                        && !row.check.has_focus()
                    {
                        row.check.set_active(profile.selected());
                        if profile.selected() {
                            forms.selected.borrow_mut().insert(profile.name().into());
                        } else {
                            forms.selected.borrow_mut().remove(profile.name());
                        }
                    }
                }
                if forms.apply_profiles.is_none() {
                    let apply = gtk::Button::with_mnemonic("Review profile _selection");
                    let cb = callback.clone();
                    let scope = scope.clone();
                    let chosen = forms.selected.clone();
                    apply.connect_clicked(move |_| {
                        cb(plan(
                            &scope,
                            "config",
                            &[
                                "set",
                                "ENVIRONMENT_PROFILES",
                                &chosen
                                    .borrow()
                                    .iter()
                                    .cloned()
                                    .collect::<Vec<_>>()
                                    .join(" "),
                                "--scope",
                                "yard",
                                "--local",
                            ],
                        ))
                    });
                    self.profiles.append(&apply);
                    forms.apply_profiles = Some(apply);
                }
                forms.apply_profiles.as_ref().unwrap().set_sensitive(plans);
                self.settings(
                    &mut forms,
                    scope,
                    details.settings(),
                    state,
                    plans,
                    mutable,
                    &callback,
                );
                self.diagnostic_facts
                    .set_text(&diagnostics(details.diagnostics()));
            }
            Details::Host(details) => {
                self.settings(
                    &mut forms,
                    scope,
                    details.settings(),
                    state,
                    plans,
                    mutable,
                    &callback,
                );
                self.diagnostic_facts
                    .set_text(&diagnostics(details.diagnostics()));
                self.create(
                    &mut forms,
                    scope,
                    details.profiles(),
                    state,
                    plans
                        && supports(caps, "yard-bootstrap-v1")
                        && supports(caps, "profile-list-v1"),
                    mutable,
                    &callback,
                );
                self.synchronize(
                    &mut forms,
                    scope,
                    details.sync(),
                    state,
                    plans && supports(caps, "host-sync-status-v1"),
                    mutable,
                    &callback,
                );
            }
        }
        self.updating.set(false);
    }
    fn settings(
        &self,
        forms: &mut Forms,
        scope: &Scope,
        settings: &[Setting],
        state: &AppState,
        plans: bool,
        mutable: bool,
        callback: &Callback,
    ) {
        let current: BTreeSet<_> = settings.iter().map(|s| s.name()).collect();
        // A removed focused editor remains a visibly unavailable local draft.
        // Unfocused removed rows can be retired, keeping the cache bounded.
        forms.settings.retain(|name, row| {
            if current.contains(name.as_str()) || row.editor.has_focus() {
                true
            } else {
                self.settings.remove(&row.root);
                false
            }
        });
        for (name, row) in &forms.settings {
            if !current.contains(name.as_str()) {
                row.fact
                    .set_text(&format!("{name} · Unavailable. Draft retained."));
                row.set.set_sensitive(false);
                row.unset.set_sensitive(false);
            }
        }
        for setting in settings {
            if setting.name() == "ENVIRONMENT_PROFILES" {
                continue;
            }
            if !forms.settings.contains_key(setting.name()) {
                let root = column();
                let fact = label(&root, "");
                let editor = gtk::TextView::new();
                editor.set_wrap_mode(gtk::WrapMode::WordChar);
                editor.set_accepts_tab(false);
                editor.set_height_request(72);
                editor.set_top_margin(8);
                editor.set_bottom_margin(8);
                let field = format!("setting:{}", setting.name());
                editor.buffer().set_text(
                    state
                        .value(scope, &field)
                        .unwrap_or_else(|| setting.value().unwrap_or("")),
                );
                editor.update_property(&[gtk::accessible::Property::Label(&format!(
                    "Draft {}",
                    setting.name()
                ))]);
                let cb = callback.clone();
                editor.buffer().connect_changed(move |buffer| {
                    cb(Action::Draft(
                        field.clone(),
                        buffer
                            .text(&buffer.start_iter(), &buffer.end_iter(), true)
                            .to_string(),
                    ))
                });
                root.append(&editor);
                let actions = gtk::Box::new(Orientation::Horizontal, 8);
                let set = gtk::Button::with_mnemonic("Review _set");
                let text = editor.clone();
                let name = setting.name().to_owned();
                let scope2 = scope.clone();
                let cb = callback.clone();
                let level = if scope.yard.is_some() { "yard" } else { "host" };
                set.connect_clicked(move |_| {
                    let buffer = text.buffer();
                    let value = buffer.text(&buffer.start_iter(), &buffer.end_iter(), true);
                    cb(plan(
                        &scope2,
                        "config",
                        &["set", &name, &value, "--scope", level, "--local"],
                    ));
                });
                actions.append(&set);
                let unset = button(
                    &actions,
                    "Review _unset",
                    callback,
                    plan(
                        scope,
                        "config",
                        &["unset", setting.name(), "--scope", level, "--local"],
                    ),
                );
                root.append(&actions);
                self.settings.append(&root);
                forms.settings.insert(
                    setting.name().into(),
                    SettingRow {
                        root,
                        fact,
                        editor,
                        set,
                        unset,
                    },
                );
            }
            let row = &forms.settings[setting.name()];
            row.fact.set_text(&setting_fact(setting));
            let can_edit = editable(setting, scope);
            row.editor.set_visible(can_edit);
            row.editor.set_sensitive(mutable);
            row.set.set_visible(can_edit);
            row.unset.set_visible(can_edit);
            row.set.set_sensitive(plans && can_edit);
            row.unset.set_sensitive(plans && can_edit);
        }
    }
    fn create(
        &self,
        forms: &mut Forms,
        scope: &Scope,
        profiles: &[Profile],
        state: &AppState,
        admitted: bool,
        mutable: bool,
        callback: &Callback,
    ) {
        if forms.creation.is_none() {
            label(&self.creation, "Create named yard · choose a reported preset. Review and execute its init plan explicitly.");
            let entry = entry(
                &self.creation,
                "Yard name",
                state.value(scope, "create:name").unwrap_or(""),
                "create:name",
                callback,
            );
            let options = Rc::new(RefCell::new(Vec::<String>::new()));
            let preset = gtk::DropDown::from_strings(&[]);
            preset.update_property(&[gtk::accessible::Property::Label("Reported yard preset")]);
            self.creation.append(&preset);
            let create = gtk::Button::with_mnemonic("Review _create yard");
            self.creation.append(&create);
            let gate = Rc::new(Cell::new(false));
            let cb = callback.clone();
            let choices = options.clone();
            let updating = self.updating.clone();
            let button = create.clone();
            let gate2 = gate.clone();
            preset.connect_selected_notify(move |preset| {
                if updating.get() {
                    return;
                }
                let choices = choices.borrow();
                button.set_sensitive(
                    gate2.get() && choices.get(preset.selected() as usize).is_some(),
                );
                if let Some(value) = choices.get(preset.selected() as usize) {
                    cb(Action::Draft("create:preset".into(), value.clone()));
                }
            });
            let cb = callback.clone();
            let scope = scope.clone();
            let name = entry.clone();
            let choices = options.clone();
            let dropdown = preset.clone();
            create.connect_clicked(move |_| {
                let yard = name.text().to_string();
                if yard.is_empty() {
                    cb(Action::Error(
                        "Enter a named yard and choose a reported preset.".into(),
                    ));
                    return;
                }
                if let Some(profile) = choices.borrow().get(dropdown.selected() as usize) {
                    cb(plan(
                        &Scope {
                            connection: scope.connection.clone(),
                            yard: Some(yard),
                        },
                        "init",
                        &["--profile", profile],
                    ));
                }
            });
            forms.creation = Some(Creation {
                entry,
                preset,
                options,
                create,
                admitted: gate,
            });
        }
        let creation = forms.creation.as_ref().unwrap();
        creation.entry.set_sensitive(mutable);
        creation.admitted.set(admitted);
        let options: Vec<_> = profiles
            .iter()
            .filter(|p| p.has_yard_preset())
            .map(|p| p.name().to_owned())
            .collect();
        if *creation.options.borrow() != options {
            let previous = creation
                .options
                .borrow()
                .get(creation.preset.selected() as usize)
                .cloned()
                .or_else(|| state.value(scope, "create:preset").map(str::to_owned));
            let strings: Vec<_> = options.iter().map(String::as_str).collect();
            creation
                .preset
                .set_model(Some(&gtk::StringList::new(&strings)));
            creation.preset.set_selected(
                previous
                    .as_ref()
                    .and_then(|old| options.iter().position(|name| name == old))
                    .map_or(
                        if previous.is_some() || options.is_empty() {
                            gtk::INVALID_LIST_POSITION
                        } else {
                            0
                        },
                        |index| index as u32,
                    ),
            );
            *creation.options.borrow_mut() = options;
        }
        creation.create.set_sensitive(
            admitted
                && creation
                    .options
                    .borrow()
                    .get(creation.preset.selected() as usize)
                    .is_some(),
        );
    }
    fn synchronize(
        &self,
        forms: &mut Forms,
        scope: &Scope,
        sync: &HostSyncStatus,
        state: &AppState,
        plans: bool,
        mutable: bool,
        callback: &Callback,
    ) {
        if forms.synchronization.is_none() {
            let fact = label(&self.sync, "");
            let git = label(&self.sync, "");
            let repository = entry(
                &self.sync,
                "Credential-free Git repository (SSH or HTTPS)",
                state.value(scope, "sync:repository").unwrap_or(""),
                "sync:repository",
                callback,
            );
            let connect = gtk::Button::with_mnemonic("Review _Connect Git");
            let cb = callback.clone();
            let scope2 = scope.clone();
            let text = repository.clone();
            connect.connect_clicked(move |_| { let url = text.text(); if repository_url(&url) { cb(plan(&scope2, "config", &["sync", "connect", &url])); } else { cb(Action::Error("Use a credential-free SSH or HTTPS repository URL. Authentication is managed by the owner.".into())); } });
            self.sync.append(&connect);
            let pull = button(
                &self.sync,
                "Review _Pull",
                callback,
                plan(scope, "config", &["sync", "pull"]),
            );
            let push = button(
                &self.sync,
                "Review P_ush",
                callback,
                plan(scope, "config", &["sync", "push"]),
            );
            self.sync
                .append(&gtk::Separator::new(Orientation::Horizontal));
            let credentials = label(&self.sync, "");
            let keys = button(
                &self.sync,
                "Review _Sync now",
                callback,
                plan(scope, "keys", &["sync", "--all", "--now"]),
            );
            forms.synchronization = Some(Synchronization {
                fact,
                git,
                credentials,
                repository,
                actions: vec![connect, pull, push, keys],
            });
        }
        let form = forms.synchronization.as_ref().unwrap();
        form.repository.set_sensitive(mutable);
        for button in &form.actions {
            button.set_sensitive(plans);
        }
        form.fact.set_text(&format!("Configuration Git\nRegistration: {}\nHost ID: {}\nPending ID: {} · Recovery required: {}\nGeneration: {}\nApplied commit: {}", sync.registration(), sync.host_id(), sync.host_id_pending(), sync.recovery_required(), sync.generation(), sync.applied_commit()));
        form.git.set_text(&sync.git().map_or_else(|| "Cached Git status · Unavailable".into(), |git| format!("Cached Git status · Available: {}\nRemote: {}\nBranch: {}\nUpstream: {}\nHEAD: {}\nRelation: {} · Worktree: {}\nAhead: {} · Behind: {} · Conflicts: {}\nStaged: {} · Unstaged: {} · Untracked: {}\nLast fetch: {}", git.available(), git.remote(), git.branch(), git.upstream(), git.head(), git.relation(), git.worktree(), git.ahead(), git.behind(), git.conflicts(), git.staged(), git.unstaged(), git.untracked(), git.last_fetch())));
        let credentials = sync.credentials();
        let mut text = format!(
            "Credentials · REDACTED\nState: {} · Records: {} · Conflicts: {}",
            credentials.state(),
            credentials.records(),
            credentials.conflicts()
        );
        for peer in credentials.peers() {
            text.push_str(&format!("\n\n{} · {}\nTrusted: {} · Manual only: {}\nLast attempt: {} · Last success: {}\nFailures: {} · Failed: {} · Next retry: {}", peer.name(), peer.role(), peer.trusted(), peer.manual_only(), peer.last_attempt(), peer.last_success(), peer.consecutive_failures(), peer.failed(), peer.next_retry()));
        }
        form.credentials.set_text(&text);
    }
}
