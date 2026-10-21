//! Opt-in GTK frame correlation. The caller, not this module, owns UI authority.
use gtk::{gdk, glib, prelude::*, subclass::prelude::*};
use std::cell::{Cell, RefCell};
use std::time::Instant;
use veranda_lib::observation::{Batch, Observation, Retirement};

#[derive(Clone)]
struct Stamp {
    clock: gdk::FrameClock,
    counter: i64,
    revision: u64,
}

mod imp {
    use super::*;
    #[derive(Default)]
    pub struct PaintBox {
        pub(super) observation: RefCell<Option<Observation>>,
        pub(super) revision: Cell<u64>,
        pub(super) pending: RefCell<Batch>,
        pub(super) stamp: RefCell<Option<Stamp>>,
        pub(super) connection: RefCell<Option<(gdk::FrameClock, glib::SignalHandlerId)>>,
        pub(super) clock_ordinal: Cell<u64>,
        pub(super) ready: RefCell<Option<std::rc::Rc<dyn Fn() -> bool>>>,
        #[cfg(test)]
        pub(super) snapshots: Cell<u64>,
        #[cfg(test)]
        pub(super) parents: Cell<u64>,
        #[cfg(test)]
        pub(super) changed_during_parent: Cell<u64>,
    }
    #[glib::object_subclass]
    impl ObjectSubclass for PaintBox {
        const NAME: &'static str = "VerandaPaintBox";
        type Type = super::TrackedBox;
        type ParentType = gtk::Box;
    }
    impl ObjectImpl for PaintBox {
        fn dispose(&self) {
            self.disconnect();
        }
    }
    impl WidgetImpl for PaintBox {
        fn map(&self) {
            self.parent_map();
            self.bind();
        }
        fn unmap(&self) {
            self.disconnect();
            self.parent_unmap();
        }
        fn unrealize(&self) {
            self.disconnect();
            self.parent_unrealize();
        }
        fn snapshot(&self, snapshot: &gtk::Snapshot) {
            let revision = self.revision.get();
            let clock = self.obj().frame_clock();
            let ready = self.ready.borrow().as_ref().is_none_or(|ready| ready());
            #[cfg(test)]
            self.snapshots.set(self.snapshots.get() + 1);
            self.parent_snapshot(snapshot);
            #[cfg(test)]
            self.parents.set(self.parents.get() + 1);
            // Never reuse a cached prior stamp for a child invalidation or a
            // revision that changed while inherited children were snapshotted.
            self.stamp.borrow_mut().take();
            if revision != self.revision.get() {
                #[cfg(test)]
                self.changed_during_parent
                    .set(self.changed_during_parent.get() + 1);
                return;
            }
            if !ready || !self.ready.borrow().as_ref().is_none_or(|ready| ready()) {
                return;
            }
            if self.pending.borrow().tickets().is_empty() {
                return;
            }
            if let Some(clock) = clock {
                let counter = clock.frame_counter();
                *self.stamp.borrow_mut() = Some(Stamp {
                    clock,
                    counter,
                    revision,
                });
            }
        }
    }
    impl BoxImpl for PaintBox {}
    impl PaintBox {
        fn bind(&self) {
            if self.connection.borrow().is_some() {
                return;
            }
            if let Some(clock) = self.obj().frame_clock() {
                let weak = self.obj().downgrade();
                let id = clock.connect_after_paint(move |clock| {
                    if let Some(object) = weak.upgrade() {
                        object.imp().finish(clock, clock.frame_counter());
                    }
                });
                if let Some(observation) = self.observation.borrow().as_ref() {
                    self.clock_ordinal.set(observation.clock_binding());
                }
                *self.connection.borrow_mut() = Some((clock, id));
            }
        }
        pub(super) fn disconnect(&self) {
            if let Some((clock, id)) = self.connection.borrow_mut().take() {
                clock.disconnect(id);
            }
            self.stamp.borrow_mut().take();
            let batch = self.pending.replace(Batch::default());
            if let Some(observation) = self.observation.borrow().as_ref() {
                observation.retire(&observation.live(&batch), Retirement::Unknown);
            }
        }
        pub(super) fn finish(&self, clock: &gdk::FrameClock, counter: i64) {
            let stamp = self.stamp.borrow().clone();
            let Some(stamp) = stamp else {
                return;
            };
            if !self
                .connection
                .borrow()
                .as_ref()
                .is_some_and(|(bound, _)| bound == clock)
                || stamp.clock != *clock
                || stamp.counter != counter
                || stamp.revision != self.revision.get()
            {
                return;
            }
            self.stamp.borrow_mut().take();
            let batch = self.pending.replace(Batch::default());
            if let Some(observation) = self.observation.borrow().as_ref() {
                observation.complete(
                    &batch,
                    stamp.revision,
                    self.clock_ordinal.get(),
                    counter,
                    Instant::now(),
                );
            }
        }
    }
}

glib::wrapper! {
    pub struct TrackedBox(ObjectSubclass<imp::PaintBox>)
        @extends gtk::Widget, gtk::Box,
        @implements gtk::Accessible, gtk::Buildable, gtk::ConstraintTarget, gtk::Orientable;
}

/// Weak handle: holding it cannot keep a closed widget or clock callback alive.
#[derive(Clone)]
pub struct PaintHandle(glib::WeakRef<TrackedBox>);
impl PaintHandle {
    /// Snapshot-time readiness only. False waits for a naturally generated frame.
    pub fn snapshot_ready(&self, ready: std::rc::Rc<dyn Fn() -> bool>) {
        if let Some(object) = self.0.upgrade() {
            *object.imp().ready.borrow_mut() = Some(ready);
        }
    }
    /// Supply the COMPLETE represented live batch for this advancing revision.
    /// Same-scope callers must merge still-covered originals before calling.
    /// Missing old members retire as Superseded; no cross-scope coverage is guessed.
    /// This never invalidates GTK or forces a frame. Call after actual widget apply.
    pub fn applied(&self, revision: u64, represented: &Batch) -> bool {
        let Some(object) = self.0.upgrade() else {
            return false;
        };
        let imp = object.imp();
        let observation = imp.observation.borrow();
        let Some(observation) = observation.as_ref() else {
            return false;
        };
        if revision <= imp.revision.get() {
            return false;
        }
        let represented = observation.live(represented);
        let previous = imp.pending.replace(represented.clone());
        observation.retire_missing(&previous, &represented, Retirement::Superseded);
        imp.revision.set(revision);
        imp.stamp.borrow_mut().take();
        true
    }
}

/// Same inherited Box hierarchy in both modes; no extra accessible/layout ancestor.
pub fn shell(
    orientation: gtk::Orientation,
    spacing: i32,
    observation: Option<Observation>,
) -> (gtk::Box, Option<PaintHandle>) {
    let Some(observation) = observation else {
        return (gtk::Box::new(orientation, spacing), None);
    };
    let object: TrackedBox = glib::Object::builder()
        .property("orientation", orientation)
        .property("spacing", spacing)
        .build();
    *object.imp().observation.borrow_mut() = Some(observation);
    let handle = PaintHandle(object.downgrade());
    (object.upcast(), Some(handle))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::time::Duration;
    use veranda_lib::observation::{SourceKind, TerminalOutcome};

    fn wait_until(mut ready: impl FnMut() -> bool) {
        let end = Instant::now() + Duration::from_secs(5);
        while !ready() {
            assert!(Instant::now() < end, "finite GTK fixture deadline");
            glib::MainContext::default().iteration(false);
            std::thread::sleep(Duration::from_millis(1));
        }
    }
    fn quiet_interval() {
        let end = Instant::now() + Duration::from_millis(160);
        while Instant::now() < end {
            glib::MainContext::default().iteration(false);
            std::thread::sleep(Duration::from_millis(1));
        }
    }
    fn entry_focused(window: &gtk::Window, entry: &gtk::Entry) -> bool {
        gtk::prelude::GtkWindowExt::focus(window).is_some_and(|focus| {
            focus == *entry.upcast_ref::<gtk::Widget>() || focus.is_ancestor(entry)
        })
    }

    #[test]
    #[ignore = "requires isolated GTK display; run exact owning test single-threaded with Cairo"]
    fn adapter_tracks_real_child_frames_and_cancels_owned_lifetimes() {
        gtk::init().unwrap();
        gtk::Settings::default()
            .unwrap()
            .set_property("gtk-cursor-blink", false);
        let observation = Observation::new();
        let source = observation.source(SourceKind::Local);
        let (plain, disabled) = shell(gtk::Orientation::Vertical, 6, None);
        let (tracked, handle) = shell(gtk::Orientation::Vertical, 6, Some(observation.clone()));
        let handle = handle.unwrap();
        assert!(disabled.is_none() && !plain.is::<TrackedBox>() && tracked.is::<TrackedBox>());
        let object = tracked.clone().downcast::<TrackedBox>().unwrap();
        let label = gtk::Label::new(Some("0"));
        let plain_label = gtk::Label::new(Some("0"));
        let entry = gtk::Entry::new();
        let plain_entry = gtk::Entry::new();
        tracked.append(&label);
        tracked.append(&entry);
        plain.append(&plain_label);
        plain.append(&plain_entry);
        let row = gtk::Box::new(gtk::Orientation::Horizontal, 12);
        row.set_homogeneous(true);
        row.append(&plain);
        row.append(&tracked);
        let window = gtk::Window::builder()
            .default_width(640)
            .default_height(180)
            .child(&row)
            .build();
        window.present();
        wait_until(|| object.imp().snapshots.get() > 0);
        assert_eq!(
            plain.measure(gtk::Orientation::Horizontal, -1),
            tracked.measure(gtk::Orientation::Horizontal, -1)
        );
        assert_eq!(
            (plain.allocated_width(), plain.allocated_height()),
            (tracked.allocated_width(), tracked.allocated_height())
        );
        assert_eq!(plain.accessible_role(), tracked.accessible_role());
        assert_eq!(label.parent().as_ref(), Some(tracked.upcast_ref()));
        assert_eq!(plain_label.parent().as_ref(), Some(plain.upcast_ref()));
        assert_eq!(plain.parent(), tracked.parent());
        for (shell, input) in [(&plain, &plain_entry), (&tracked, &entry)] {
            gtk::prelude::GtkWindowExt::set_focus(&window, None::<&gtk::Widget>);
            assert!(shell.child_focus(gtk::DirectionType::TabForward));
            assert!(entry_focused(&window, input));
        }
        let first = source.request().unwrap();
        label.set_label("1");
        assert!(handle.applied(1, &source.batch()));
        wait_until(|| observation.counts().completed == 1);
        let sample = observation.terminals()[0].clone();
        assert_eq!(sample.ticket, first.ordinal());
        assert!(matches!(
            sample.outcome,
            TerminalOutcome::Paint { revision: 1, .. }
        ));
        assert!(entry_focused(&window, &entry));

        let a = source.request().unwrap();
        assert!(handle.applied(2, &source.batch()));
        let b = source.selection().unwrap();
        // Caller explicitly includes still-covered A with B before a frame.
        let merged = source.batch();
        assert!(handle.applied(3, &merged));
        label.set_label("2");
        wait_until(|| observation.counts().completed == 3);
        let rows = observation.terminals();
        assert_eq!((rows[1].ticket, rows[2].ticket), (a.ordinal(), b.ordinal()));
        assert_eq!(rows[1].outcome, rows[2].outcome);
        assert!(rows[1].elapsed_micros.unwrap() >= rows[2].elapsed_micros.unwrap());
        assert!(!handle.applied(3, &merged));

        let abandoned = source.request().unwrap();
        assert!(handle.applied(4, &source.batch()));
        let another = observation.source(SourceKind::Remote);
        let replacement = another.request().unwrap();
        assert!(handle.applied(5, &another.batch()));
        label.set_label("3");
        wait_until(|| observation.counts().completed == 4);
        let rows = observation.terminals();
        assert!(rows.iter().any(|row| row.ticket == abandoned.ordinal()
            && row.outcome == TerminalOutcome::Retired(Retirement::Superseded)));
        assert!(rows.iter().any(|row| row.ticket == replacement.ordinal()
            && matches!(row.outcome, TerminalOutcome::Paint { revision: 5, .. })));

        // Wrong frame, clock and revision exercise the real completion gate.
        let mutation_ticket = source.request().unwrap();
        assert!(handle.applied(6, &source.batch()));
        let clock = object.frame_clock().unwrap();
        let frame = clock.frame_counter();
        let observed_frames = std::rc::Rc::new(RefCell::new(Vec::new()));
        let observed_after = observed_frames.clone();
        let after_id = clock.connect_after_paint(move |clock| {
            let mut frames = observed_after.borrow_mut();
            assert!(frames.len() < 32, "finite frame observer bound");
            frames.push(clock.frame_counter());
        });
        let other_window = gtk::Window::builder()
            .default_width(80)
            .default_height(60)
            .build();
        other_window.present();
        wait_until(|| other_window.frame_clock().is_some());
        let other_clock = other_window.frame_clock().unwrap();
        assert_ne!(clock, other_clock);
        *object.imp().stamp.borrow_mut() = Some(Stamp {
            clock: clock.clone(),
            counter: frame,
            revision: 6,
        });
        let before = observation.counts().completed;
        object.imp().finish(&clock, frame + 1);
        object.imp().finish(&other_clock, frame);
        object.imp().revision.set(7);
        object.imp().finish(&clock, frame);
        object.imp().revision.set(6);
        assert_eq!(observation.counts().completed, before);
        object.imp().stamp.borrow_mut().take();
        other_window.close();

        // A real child's draw callback changes accepted revision DURING parent_snapshot.
        let drawing = gtk::DrawingArea::new();
        drawing.set_content_height(8);
        drawing.set_content_width(8);
        let changed = std::rc::Rc::new(Cell::new(false));
        let changed_in_draw = changed.clone();
        let during = handle.clone();
        let represented = source.batch();
        drawing.set_draw_func(move |_, _, _, _| {
            if !changed_in_draw.replace(true) {
                assert!(during.applied(7, &represented));
            }
        });
        tracked.append(&drawing);
        wait_until(|| changed.get());
        quiet_interval();
        assert_eq!(object.imp().changed_during_parent.get(), 1);
        assert!(
            !observation
                .terminals()
                .iter()
                .any(|row| matches!(row.outcome, TerminalOutcome::Paint { revision: 6, .. })),
            "mutated snapshot must never complete revision6"
        );
        label.set_label("4");
        wait_until(|| observation.counts().completed == before + 1);
        let samples = observation.terminals();
        let completed: Vec<_> = samples
            .iter()
            .filter(|row| row.ticket == mutation_ticket.ordinal())
            .collect();
        assert_eq!(completed.len(), 1);
        assert_eq!(completed[0].source, mutation_ticket.source());
        assert!(
            matches!(completed[0].outcome, TerminalOutcome::Paint { revision:7, clock, frame } if clock==object.imp().clock_ordinal.get() && frame>=0)
        );
        if let TerminalOutcome::Paint { frame, .. } = completed[0].outcome {
            assert!(
                observed_frames.borrow().contains(&frame),
                "terminal must match this actual clock after-paint callback"
            );
        }
        clock.disconnect(after_id);
        quiet_interval();
        let idle = object.imp().snapshots.get();
        quiet_interval();
        assert_eq!(
            object.imp().snapshots.get(),
            idle,
            "adapter creates no idle frames"
        );

        source.request().unwrap();
        assert!(handle.applied(8, &source.batch()));
        window.set_visible(false);
        assert!(object.imp().connection.borrow().is_none());
        assert_eq!(observation.counts().pending, 0);
        assert_eq!(
            observation.terminals().last().unwrap().outcome,
            TerminalOutcome::Retired(Retirement::Unknown)
        );
        object.imp().disconnect();
        object.imp().disconnect();
        source.request().unwrap();
        assert!(handle.applied(9, &source.batch()));
        window.present();
        label.set_label("5");
        wait_until(|| observation.counts().completed == before + 2);
        assert!(object.imp().connection.borrow().is_some());
        assert_eq!(object.imp().parents.get(), object.imp().snapshots.get());
        source.request().unwrap();
        assert!(handle.applied(10, &source.batch()));
        window.close();
        assert!(object.imp().connection.borrow().is_none());
        assert_eq!(observation.counts().pending, 0);
        // Pending work on an unrealized widget also retires on actual dispose.
        let (unmapped, unmapped_handle) =
            shell(gtk::Orientation::Vertical, 0, Some(observation.clone()));
        let unmapped_handle = unmapped_handle.unwrap();
        source.request().unwrap();
        assert!(unmapped_handle.applied(1, &source.batch()));
        drop(unmapped);
        assert_eq!(observation.counts().pending, 0);
        assert!(!unmapped_handle.applied(2, &Batch::default()));
        window.set_child(None::<&gtk::Widget>);
        drop(window);
        drop(row);
        drop(tracked);
        drop(object);
        drop(plain);
        drop(drawing);
        drop(label);
        drop(entry);
        drop(plain_label);
        drop(plain_entry);
        assert!(!handle.applied(11, &Batch::default()));
        println!(
            "paint_adapter_checks=passed natural_close=true pending=0 terminal_samples={} compact_overflow={}",
            observation.terminals().len(),
            observation.counts().dropped_terminals
        );
    }
}
