use gtk::glib;
use gtk::prelude::*;
use std::cell::RefCell;
use std::rc::Rc;

struct Pending {
    clock: gtk::gdk::FrameClock,
    handler: glib::SignalHandlerId,
}

#[derive(Default)]
struct State {
    pending: RefCell<Option<Pending>>,
}

impl State {
    fn cancel(&self) {
        let pending = self.pending.borrow_mut().take();
        if let Some(pending) = pending {
            pending.clock.disconnect(pending.handler);
        }
    }
}

/// Reveals the current focused selection after rebuilt navigator rows are allocated.
/// Only one request can survive; it never changes focus or schedules frames.
pub struct Visibility {
    state: Rc<State>,
    navigation: glib::WeakRef<gtk::ListBox>,
    scroll: glib::WeakRef<gtk::ScrolledWindow>,
    unmap: Option<glib::SignalHandlerId>,
}

impl Visibility {
    /// Read actual allocated target bounds at snapshot, not pending-callback state.
    pub fn snapshot_ready(&self) -> bool {
        let (Some(nav), Some(scroll)) = (self.navigation.upgrade(), self.scroll.upgrade()) else {
            return false;
        };
        if !nav.is_mapped() || !scroll.is_mapped() {
            return false;
        }
        let Some(row) = nav.selected_row() else {
            return true;
        };
        let focused = row.root().and_then(|root| root.focus());
        if !focused.is_some_and(|focus| focus == row || focus.is_ancestor(&row)) {
            return true;
        }
        let Some(bounds) = row.compute_bounds(&nav) else {
            return false;
        };
        let adjustment = scroll.vadjustment();
        let top = f64::from(bounds.y());
        let bottom = top + f64::from(bounds.height());
        top.is_finite()
            && bottom.is_finite()
            && bottom > top
            && adjustment.page_size() > 0.0
            && top >= adjustment.value()
            && bottom <= adjustment.value() + adjustment.page_size()
    }
    pub fn new(navigation: &gtk::ListBox, scroll: &gtk::ScrolledWindow) -> Self {
        let state = Rc::new(State::default());
        let weak = Rc::downgrade(&state);
        let unmap = navigation.connect_unmap(move |_| {
            if let Some(state) = weak.upgrade() {
                state.cancel();
            }
        });
        Self {
            state,
            navigation: navigation.downgrade(),
            scroll: scroll.downgrade(),
            unmap: Some(unmap),
        }
    }

    pub fn cancel(&self) {
        self.state.cancel();
    }

    pub fn reveal(&self, row: &gtk::ListBoxRow) {
        self.cancel();
        let Some(navigation) = self.navigation.upgrade().filter(|nav| nav.is_mapped()) else {
            return;
        };
        let Some(clock) = navigation.frame_clock() else {
            return;
        };
        let weak = Rc::downgrade(&self.state);
        let navigation = self.navigation.clone();
        let scroll = self.scroll.clone();
        let row = row.downgrade();
        let handler = clock.connect_after_paint(move |_| {
            let Some(state) = weak.upgrade() else {
                return;
            };
            state.cancel();
            let (Some(navigation), Some(scroll), Some(row)) =
                (navigation.upgrade(), scroll.upgrade(), row.upgrade())
            else {
                return;
            };
            if !navigation.is_mapped()
                || !scroll.is_mapped()
                || row.parent().as_ref() != Some(navigation.upcast_ref())
                || navigation.selected_row().as_ref() != Some(&row)
            {
                return;
            }
            let focused = row.root().and_then(|root| root.focus());
            if !focused.is_some_and(|focus| focus == row || focus.is_ancestor(&row)) {
                return;
            }
            // after-paint follows layout. Coordinates in the list are content
            // coordinates, independent of its viewport's current translation.
            let Some(bounds) = row.compute_bounds(&navigation) else {
                return;
            };
            let adjustment = scroll.vadjustment();
            let top = f64::from(bounds.y());
            let bottom = top + f64::from(bounds.height());
            let page = adjustment.page_size();
            if !top.is_finite() || !bottom.is_finite() || bottom <= top || page <= 0.0 {
                return;
            }
            let mut value = adjustment.value();
            if value + page < bottom {
                value = bottom - page;
            }
            if value > top {
                value = top;
            }
            value = value.clamp(
                adjustment.lower(),
                adjustment.lower().max(adjustment.upper() - page),
            );
            // set_value also cancels GTK's earlier focus-scroll animation.
            // clamp_page leaves that animation alive to overwrite this value.
            adjustment.set_value(value);
        });
        *self.state.pending.borrow_mut() = Some(Pending { clock, handler });
    }
}

impl Drop for Visibility {
    fn drop(&mut self) {
        self.cancel();
        if let (Some(navigation), Some(handler)) = (self.navigation.upgrade(), self.unmap.take()) {
            navigation.disconnect(handler);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::cell::Cell;
    use std::time::{Duration, Instant};

    fn rows(navigation: &gtk::ListBox) -> gtk::ListBoxRow {
        while let Some(child) = navigation.first_child() {
            navigation.remove(&child);
        }
        for index in 0..42 {
            let label = gtk::Label::new(Some(&index.to_string()));
            label.set_margin_top(8);
            label.set_margin_bottom(8);
            let row = gtk::ListBoxRow::new();
            row.set_child(Some(&label));
            navigation.append(&row);
        }
        let row = navigation.row_at_index(21).unwrap();
        navigation.select_row(Some(&row));
        row
    }

    fn wait(predicate: impl Fn() -> bool) {
        let context = glib::MainContext::default();
        let end = Instant::now() + Duration::from_secs(5);
        while Instant::now() < end {
            while context.pending() {
                context.iteration(false);
            }
            if predicate() {
                return;
            }
            std::thread::sleep(Duration::from_millis(1));
        }
        panic!("GTK navigation observation did not complete");
    }

    #[test]
    #[ignore = "requires a private X11 display; run the explicit GTK widget check"]
    fn rebuilt_selection_is_visible_without_stealing_or_stale_callbacks() {
        gtk::init().unwrap();
        let navigation = gtk::ListBox::new();
        let scroll = gtk::ScrolledWindow::builder()
            .child(&navigation)
            .vexpand(true)
            .build();
        let label = gtk::Label::new(Some("control"));
        let entry = gtk::Entry::new();
        let observation = veranda_lib::observation::Observation::new();
        let view_ordinal = observation.view();
        let (content, paint) =
            subyard_veranda::paint::shell(gtk::Orientation::Vertical, 0, Some(observation.clone()));
        let paint = paint.unwrap();
        content.append(&label);
        content.append(&scroll);
        content.append(&entry);
        let window = gtk::Window::builder()
            .child(&content)
            .default_width(400)
            .default_height(500)
            .build();
        rows(&navigation);
        window.present();
        wait(|| navigation.is_mapped() && scroll.vadjustment().upper() > 500.0);
        let clock = navigation.frame_clock().unwrap();
        let paints = Rc::new(Cell::new(0));
        let observed = paints.clone();
        let painted = clock.connect_after_paint(move |_| observed.set(observed.get() + 1));
        let visibility = Rc::new(Visibility::new(&navigation, &scroll));
        let weak_visibility = Rc::downgrade(&visibility);
        let blocked = Rc::new(RefCell::new(Vec::new()));
        let blocked_snapshot = blocked.clone();
        let weak_nav = navigation.downgrade();
        paint.snapshot_ready(Rc::new(move || {
            let ready = weak_visibility
                .upgrade()
                .is_some_and(|visibility| visibility.snapshot_ready());
            if !ready {
                if let Some(clock) = weak_nav.upgrade().and_then(|nav| nav.frame_clock()) {
                    let mut frames = blocked_snapshot.borrow_mut();
                    if frames.last() != Some(&clock.frame_counter()) {
                        assert!(frames.len() < 64);
                        frames.push(clock.frame_counter());
                    }
                }
            }
            ready
        }));
        let row = navigation.row_at_index(21).unwrap();
        row.grab_focus();
        visibility.cancel();
        let row = rows(&navigation);
        row.grab_focus();
        scroll.vadjustment().set_value(0.0);
        visibility.reveal(&row);
        let start = observation.view_start(view_ordinal, false, false);
        assert!(paint.applied(1, &start));
        let before = paints.get();
        wait(|| {
            let bounds = row.compute_bounds(&scroll).unwrap();
            visibility.state.pending.borrow().is_none()
                && observation.counts().completed == 1
                && paints.get() > before
                && bounds.y() >= 0.0
                && bounds.y() + bounds.height() <= scroll.height() as f32
        });
        assert!(
            !blocked.borrow().is_empty(),
            "actual off-viewport snapshot was blocked"
        );
        let terminals = observation.terminals();
        if let veranda_lib::observation::TerminalOutcome::Paint { frame, .. } = terminals[0].outcome
        {
            assert!(
                !blocked.borrow().contains(&frame),
                "later after-paint correction cannot validate an old blocked snapshot"
            );
        } else {
            panic!("actual matching paint required");
        }
        let already_ready = observation.view_start(view_ordinal, false, false);
        assert!(visibility.snapshot_ready());
        assert!(paint.applied(2, &already_ready));
        label.set_text("already ready");
        wait(|| observation.counts().completed == 2);
        assert_eq!(navigation.selected_row().as_ref(), Some(&row));
        assert_eq!(RootExt::focus(&window).as_ref(), Some(row.upcast_ref()));

        // A moved focus and a changed selected identity are both stale work.
        for moved_focus in [true, false] {
            navigation.select_row(Some(&row));
            row.grab_focus();
            scroll.vadjustment().set_value(0.0);
            visibility.reveal(&row);
            if moved_focus {
                entry.grab_focus();
            } else {
                navigation.select_row(navigation.row_at_index(20).as_ref());
            }
            let before = paints.get();
            wait(|| visibility.state.pending.borrow().is_none() && paints.get() > before);
            assert_eq!(scroll.vadjustment().value(), 0.0);
            if moved_focus {
                assert!(RootExt::focus(&window).unwrap().is_ancestor(&entry));
            }
        }

        // Replacement/close cancellation owns no work after the next real paint.
        navigation.select_row(Some(&row));
        row.grab_focus();
        scroll.vadjustment().set_value(0.0);
        visibility.reveal(&row);
        visibility.cancel();
        let replacement = rows(&navigation);
        replacement.grab_focus();
        scroll.vadjustment().set_value(0.0);
        label.set_text("replacement");
        let before = paints.get();
        wait(|| paints.get() > before);
        assert!(visibility.state.pending.borrow().is_none());
        assert_eq!(scroll.vadjustment().value(), 0.0);

        visibility.reveal(&replacement);
        window.set_visible(false);
        assert!(visibility.state.pending.borrow().is_none());
        window.present();
        wait(|| navigation.is_mapped());
        replacement.grab_focus();
        scroll.vadjustment().set_value(0.0);
        visibility.reveal(&replacement);
        let state = visibility.state.clone();
        drop(visibility);
        assert!(state.pending.borrow().is_none());
        label.set_text("disposed");
        let before = paints.get();
        wait(|| paints.get() > before);
        assert_eq!(scroll.vadjustment().value(), 0.0);
        clock.disconnect(painted);
        window.close();
        wait(|| !window.is_mapped());
        println!("navigation_readiness_checks=passed natural_close=true pending=0");
    }
}
