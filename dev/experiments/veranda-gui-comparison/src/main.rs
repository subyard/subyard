//! Synthetic UI comparison only: no RPC, filesystem inventory or production actions.
use eframe::egui;
use eframe::glow::HasContext;

struct Demo {
    yard: usize,
    project: usize,
    filter: String,
    refreshes: usize,
}

impl Default for Demo {
    fn default() -> Self {
        Self {
            yard: 0,
            project: 0,
            filter: String::new(),
            refreshes: 0,
        }
    }
}

impl eframe::App for Demo {
    fn ui(&mut self, ui: &mut egui::Ui, _frame: &mut eframe::Frame) {
        egui::Panel::top("header").exact_size(100.0).show(ui, |ui| {
            ui.add_space(12.0);
            ui.heading("Veranda / egui");
            ui.label("Synthetic comparison · 10 yards · 100 projects · no backend");
            ui.horizontal(|ui| {
                if ui.button("Refresh").clicked() {
                    self.refreshes += 1;
                    println!("refresh {}", self.refreshes);
                }
                ui.label(format!("Snapshot {} · local fixture", self.refreshes));
                ui.label("Filter:");
                if ui
                    .add(egui::TextEdit::singleline(&mut self.filter).desired_width(240.0))
                    .changed()
                {
                    println!("filter {:?}", self.filter);
                }
            });
        });
        egui::Panel::left("yards")
            .exact_size(210.0)
            .resizable(false)
            .show(ui, |ui| {
                ui.add_space(12.0);
                ui.heading("Yards");
                for yard in 0..10 {
                    if ui
                        .add_sized(
                            [190.0, 38.0],
                            egui::Button::selectable(
                                self.yard == yard + 1,
                                format!("yard-{:02}     10 projects", yard + 1),
                            ),
                        )
                        .clicked()
                    {
                        self.yard = yard + 1;
                        self.project = yard * 10;
                        println!("yard {}", self.yard);
                    }
                }
                if ui.button("All projects").clicked() {
                    self.yard = 0;
                    println!("yard 0");
                }
            });
        egui::Panel::right("details").exact_size(250.0).resizable(false).show(ui, |ui| {
            ui.add_space(12.0);
            ui.heading("Project details");
            ui.separator();
            ui.label(format!("project-{:03}", self.project + 1));
            ui.label(format!("yard-{:02}", self.project / 10 + 1));
            ui.label("NOT_CREATED");
            ui.add_space(16.0);
            ui.label("Synthetic data in memory.");
            ui.label("Select a yard or project. Edit the filter. Refresh advances the fixture snapshot.");
        });
        egui::CentralPanel::default().show(ui, |ui| {
            ui.heading(if self.yard == 0 {
                "All projects".to_owned()
            } else {
                format!("yard-{:02}", self.yard)
            });
            ui.label("Project                                      State");
            ui.separator();
            egui::ScrollArea::vertical().show(ui, |ui| {
                for project in 0..100 {
                    let name = format!("project-{:03}", project + 1);
                    if (self.yard == 0 || project / 10 + 1 == self.yard)
                        && name.contains(&self.filter)
                    {
                        if ui
                            .add_sized(
                                [600.0, 38.0],
                                egui::Button::selectable(
                                    self.project == project,
                                    format!("{name}                 NOT_CREATED"),
                                ),
                            )
                            .clicked()
                        {
                            self.project = project;
                            println!("project {}", self.project + 1);
                        }
                    }
                }
            });
        });
    }
}

fn main() -> eframe::Result {
    let options = eframe::NativeOptions {
        viewport: egui::ViewportBuilder::default().with_inner_size([1120.0, 768.0]),
        renderer: eframe::Renderer::Glow,
        ..Default::default()
    };
    eframe::run_native(
        "Veranda egui comparison",
        options,
        Box::new(|cc| {
            if let Some(gl) = &cc.gl {
                // The creation callback owns a current OpenGL context.
                println!("renderer {}", unsafe {
                    gl.get_parameter_string(eframe::glow::RENDERER)
                });
            }
            cc.egui_ctx.set_visuals(egui::Visuals::light());
            Ok(Box::new(Demo::default()))
        }),
    )
}
