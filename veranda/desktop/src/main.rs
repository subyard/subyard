use gtk::glib;
use gtk::prelude::*;
use gtk::{Application, ApplicationWindow, Orientation};
use std::cell::{Cell, RefCell};
use std::path::PathBuf;
use std::rc::Rc;
use subyard_veranda::model::{Model, ReadAction};
use subyard_veranda::worker::{Update, Worker};
use veranda_lib::client::product_version;

struct View {
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
    rows: RefCell<Vec<(Option<String>, Option<String>)>>,
    model: RefCell<Model>,
    worker: RefCell<Worker>,
    rendering: Cell<bool>,
    closing: Cell<bool>,
    closed: Cell<bool>,
}

impl View {
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
        self.rendering.set(false);
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
            explicit,
        ) {
            self.status.set_text("Refreshing…");
        }
    }

    fn events(&self) {
        if !self.model.borrow().initialized {
            return;
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
    bar.append(&refresh);
    shell.append(&bar);
    let body = gtk::Paned::new(Orientation::Horizontal);
    let navigation = gtk::ListBox::new();
    navigation.set_selection_mode(gtk::SelectionMode::Single);
    navigation.set_sensitive(false);
    navigation.update_property(&[gtk::accessible::Property::Label("Owner hosts and yards")]);
    let scroll = gtk::ScrolledWindow::builder()
        .child(&navigation)
        .min_content_width(240)
        .build();
    body.set_start_child(Some(&scroll));
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
    stack.add_titled(&detail, Some("Overview"), "Overview");
    let list = gtk::Box::new(Orientation::Vertical, 12);
    // A scrollable workspace keeps arbitrarily long typed names visible.
    let list_scroll = gtk::ScrolledWindow::builder().child(&list).build();
    let list_page = stack.add_titled(&list_scroll, Some("Yards"), "Yards");
    workspace.append(&heading);
    workspace.append(&status);
    workspace.append(&switcher);
    workspace.append(&stack);
    body.set_end_child(Some(&workspace));
    body.set_vexpand(true);
    shell.append(&body);
    window.set_child(Some(&shell));
    let view = Rc::new(View {
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
        rows: RefCell::new(vec![]),
        model: RefCell::new(Model::new(vec![])),
        worker: RefCell::new(worker),
        rendering: Cell::new(false),
        closing: Cell::new(false),
        closed: Cell::new(false),
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
                } => {
                    if view.model.borrow().initialized {
                        continue;
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
                    view.render();
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
                        ReadAction::Applied => view.render(),
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
                Update::Events => {}
                Update::Closed => unreachable!(),
            }
        }
    });
    window.present();
}

fn main() -> glib::ExitCode {
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
