use crate::client::{
    Assessment, Client, HostDetails, OperationPlan, Removal, Started, YardDetails,
};
use crate::connections::ConnectionSummary;
use crate::local_fleet::{LocalFleetSnapshot, NativeError};
use crate::sessions::Launch;
use std::sync::atomic::{AtomicBool, AtomicU32, Ordering};
use std::sync::Arc;
use tauri::{Emitter, Manager, State, WebviewWindow};

async fn task<T: Send + 'static>(
    window: WebviewWindow,
    state: State<'_, Arc<Client>>,
    job: impl FnOnce(Arc<Client>) -> Result<T, NativeError> + Send + 'static,
) -> Result<T, NativeError> {
    if window.label() != "main" {
        return Err(NativeError::new(
            "ipc_denied",
            "This window cannot manage Subyard connections.",
        ));
    }
    let client = state.inner().clone();
    tauri::async_runtime::spawn_blocking(move || job(client)).await.map_err(|_| NativeError::new(
        "native_task_failed", "The native request did not finish. Refresh the owner before retrying an operation."))?
}

#[tauri::command]
async fn list_connections(
    window: WebviewWindow,
    state: State<'_, Arc<Client>>,
) -> Result<Vec<ConnectionSummary>, NativeError> {
    task(window, state, |client| client.connections()).await
}
#[tauri::command]
async fn load_local_fleet(
    window: WebviewWindow,
    state: State<'_, Arc<Client>>,
) -> Result<LocalFleetSnapshot, NativeError> {
    task(window, state, |client| client.fleet(None)).await
}
#[tauri::command]
async fn load_fleet(
    window: WebviewWindow,
    state: State<'_, Arc<Client>>,
    connection_id: Option<String>,
) -> Result<LocalFleetSnapshot, NativeError> {
    task(window, state, move |client| client.fleet(connection_id)).await
}
#[tauri::command]
async fn assess_connection(
    window: WebviewWindow,
    state: State<'_, Arc<Client>>,
    destination: String,
) -> Result<Assessment, NativeError> {
    task(window, state, move |client| {
        client.assess(&destination, None)
    })
    .await
}
#[tauri::command]
async fn connect_remote(
    window: WebviewWindow,
    state: State<'_, Arc<Client>>,
    assessment_id: String,
    confirmed: bool,
    fingerprint: String,
) -> Result<ConnectionSummary, NativeError> {
    task(window, state, move |client| {
        client.connect(&assessment_id, confirmed, &fingerprint)
    })
    .await
}
#[tauri::command]
async fn repair_connection(
    window: WebviewWindow,
    state: State<'_, Arc<Client>>,
    connection_id: String,
) -> Result<Assessment, NativeError> {
    task(window, state, move |client| client.repair(&connection_id)).await
}
#[tauri::command]
async fn assess_removal(
    window: WebviewWindow,
    state: State<'_, Arc<Client>>,
    connection_id: String,
) -> Result<Removal, NativeError> {
    task(window, state, move |client| {
        client.assess_removal(&connection_id)
    })
    .await
}
#[tauri::command]
async fn cancel_assessment(
    window: WebviewWindow,
    state: State<'_, Arc<Client>>,
    assessment_id: String,
) -> Result<(), NativeError> {
    task(window, state, move |client| {
        client.cancel_assessment(&assessment_id)
    })
    .await
}
#[tauri::command]
async fn remove_connection(
    window: WebviewWindow,
    state: State<'_, Arc<Client>>,
    assessment_id: String,
    confirmed: bool,
) -> Result<(), NativeError> {
    task(window, state, move |client| {
        client.remove(&assessment_id, confirmed)
    })
    .await
}
#[tauri::command]
async fn load_yard(
    window: WebviewWindow,
    state: State<'_, Arc<Client>>,
    connection_id: Option<String>,
    yard: String,
) -> Result<YardDetails, NativeError> {
    task(window, state, move |client| {
        client.yard(connection_id, yard)
    })
    .await
}
#[tauri::command]
async fn load_host(
    window: WebviewWindow,
    state: State<'_, Arc<Client>>,
    connection_id: Option<String>,
) -> Result<HostDetails, NativeError> {
    task(window, state, move |client| client.host(connection_id)).await
}
#[tauri::command]
async fn plan_operation(
    window: WebviewWindow,
    state: State<'_, Arc<Client>>,
    connection_id: Option<String>,
    yard: Option<String>,
    command: String,
    arguments: Vec<String>,
) -> Result<OperationPlan, NativeError> {
    task(window, state, move |client| {
        client.plan(connection_id, yard, command, arguments)
    })
    .await
}
#[tauri::command]
async fn execute_operation(
    window: WebviewWindow,
    state: State<'_, Arc<Client>>,
    plan_id: String,
    digest: String,
    confirmed: bool,
) -> Result<Started, NativeError> {
    task(window, state, move |client| {
        client.execute(&plan_id, &digest, confirmed)
    })
    .await
}
#[tauri::command]
async fn discard_operation(
    window: WebviewWindow,
    state: State<'_, Arc<Client>>,
    plan_id: String,
) -> Result<(), NativeError> {
    task(window, state, move |client| client.discard(&plan_id)).await
}
#[tauri::command]
async fn cancel_operation(
    window: WebviewWindow,
    state: State<'_, Arc<Client>>,
    operation_id: String,
) -> Result<(), NativeError> {
    task(window, state, move |client| client.cancel(&operation_id)).await
}
#[tauri::command]
async fn launch_session(
    window: WebviewWindow,
    state: State<'_, Arc<Client>>,
    connection_id: Option<String>,
    yard: Option<String>,
    project_id: Option<String>,
    kind: String,
) -> Result<Launch, NativeError> {
    task(window, state, move |client| {
        client.launch(connection_id, yard, project_id, kind)
    })
    .await
}
#[tauri::command]
async fn shell_command(
    window: WebviewWindow,
    state: State<'_, Arc<Client>>,
    connection_id: Option<String>,
    yard: Option<String>,
    project_id: Option<String>,
) -> Result<Launch, NativeError> {
    task(window, state, move |client| {
        client.shell_command(connection_id, yard, project_id)
    })
    .await
}
struct StartupProbe {
    ready: AtomicBool,
    enabled: bool,
    reported: AtomicU32,
}
#[derive(Clone, Copy)]
enum Diagnostic {
    FrontendBoundary,
    FrontendError,
    FrontendUnhandledRejection,
    WebProcessCrashed,
    WebProcessMemoryLimit,
    WebProcessTerminatedByApi,
    WebProcessTerminationUnknown,
    WebProcessResponsive,
    WebProcessUnresponsive,
}
impl Diagnostic {
    fn label(self) -> &'static str {
        match self {
            Self::FrontendBoundary => "frontend_boundary",
            Self::FrontendError => "frontend_error",
            Self::FrontendUnhandledRejection => "frontend_unhandled_rejection",
            Self::WebProcessCrashed => "web_process_crashed",
            Self::WebProcessMemoryLimit => "web_process_memory_limit",
            Self::WebProcessTerminatedByApi => "web_process_terminated_by_api",
            Self::WebProcessTerminationUnknown => "web_process_termination_unknown",
            Self::WebProcessResponsive => "web_process_responsive",
            Self::WebProcessUnresponsive => "web_process_unresponsive",
        }
    }
}
impl StartupProbe {
    fn diagnostic(&self, event: Diagnostic) {
        let bit = 1 << event as u32;
        if self.enabled && self.reported.fetch_or(bit, Ordering::AcqRel) & bit == 0 {
            eprintln!("VERANDA_DIAGNOSTIC {}", event.label());
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn diagnostics_are_opt_in_and_report_each_fixed_event_once() {
        let probe = StartupProbe {
            ready: AtomicBool::new(false),
            enabled: false,
            reported: AtomicU32::new(0),
        };
        probe.diagnostic(Diagnostic::FrontendBoundary);
        assert_eq!(probe.reported.load(Ordering::Acquire), 0);
        let probe = StartupProbe {
            enabled: true,
            ..probe
        };
        for _ in 0..100 {
            probe.diagnostic(Diagnostic::FrontendBoundary);
        }
        assert_eq!(probe.reported.load(Ordering::Acquire), 1);
        probe.diagnostic(Diagnostic::WebProcessUnresponsive);
        assert_eq!(probe.reported.load(Ordering::Acquire), 1 | (1 << 8));
    }

    #[test]
    fn frontend_diagnostics_accept_only_the_three_fixed_codes() {
        for event in ["boundary", "error", "unhandled_rejection"] {
            assert!(serde_json::from_value::<FrontendDiagnostic>(serde_json::json!(event)).is_ok());
        }
        for payload in [
            serde_json::json!("exception text"),
            serde_json::json!({"message": "exception text"}),
            serde_json::json!("web_process_crashed"),
        ] {
            assert!(serde_json::from_value::<FrontendDiagnostic>(payload).is_err());
        }
    }
}
#[derive(serde::Deserialize)]
#[serde(rename_all = "snake_case")]
enum FrontendDiagnostic {
    Boundary,
    Error,
    UnhandledRejection,
}
#[tauri::command]
fn frontend_diagnostic(
    window: WebviewWindow,
    probe: State<'_, Arc<StartupProbe>>,
    event: FrontendDiagnostic,
) {
    if window.label() == "main" {
        probe.diagnostic(match event {
            FrontendDiagnostic::Boundary => Diagnostic::FrontendBoundary,
            FrontendDiagnostic::Error => Diagnostic::FrontendError,
            FrontendDiagnostic::UnhandledRejection => Diagnostic::FrontendUnhandledRejection,
        });
    }
}
#[derive(serde::Deserialize, serde::Serialize)]
#[serde(deny_unknown_fields)]
struct ReadyFleet {
    owners: u32,
    yards: u32,
    projects: u32,
}
#[tauri::command]
fn frontend_ready(
    window: WebviewWindow,
    probe: State<'_, Arc<StartupProbe>>,
    fleet: Option<ReadyFleet>,
) {
    if window.label() == "main" && !probe.ready.swap(true, Ordering::AcqRel) && probe.enabled {
        if let Some(fleet) = fleet {
            println!(
                "VERANDA_FLEET {}",
                serde_json::to_string(&fleet).expect("fleet counts serialize")
            );
        }
        println!("VERANDA_READY");
    }
}

pub fn run() {
    let probe = Arc::new(StartupProbe {
        ready: AtomicBool::new(false),
        enabled: std::env::var_os("VERANDA_RESOURCE_PROBE").is_some_and(|value| value == "1"),
        reported: AtomicU32::new(0),
    });
    let app = tauri::Builder::default()
        .setup(move |app| {
            #[cfg(target_os = "linux")]
            if let Some(window) = app.get_webview_window("main") {
                let webview_probe = probe.clone();
                window.with_webview(move |webview| {
                    use webkit2gtk::{
                        CacheModel, HardwareAccelerationPolicy, SettingsExt, WebContextExt,
                        WebProcessTerminationReason, WebViewExt,
                    };
                    // This local interface has no video, audio or WebGL content.
                    // Avoid allocating a browser's accelerated rendering and history caches.
                    let view = webview.inner();
                    if let Some(settings) = view.settings() {
                        settings
                            .set_hardware_acceleration_policy(HardwareAccelerationPolicy::Never);
                        settings.set_enable_webgl(false);
                        settings.set_enable_webaudio(false);
                        settings.set_enable_page_cache(false);
                    }
                    if let Some(context) = view.context() {
                        context.set_cache_model(CacheModel::DocumentViewer);
                    }
                    if webview_probe.enabled {
                        let termination_probe = webview_probe.clone();
                        view.connect_web_process_terminated(move |_, reason| {
                            termination_probe.diagnostic(match reason {
                                WebProcessTerminationReason::Crashed => {
                                    Diagnostic::WebProcessCrashed
                                }
                                WebProcessTerminationReason::ExceededMemoryLimit => {
                                    Diagnostic::WebProcessMemoryLimit
                                }
                                WebProcessTerminationReason::TerminatedByApi => {
                                    Diagnostic::WebProcessTerminatedByApi
                                }
                                _ => Diagnostic::WebProcessTerminationUnknown,
                            });
                        });
                        view.connect_is_web_process_responsive_notify(move |view| {
                            webview_probe.diagnostic(if view.is_web_process_responsive() {
                                Diagnostic::WebProcessResponsive
                            } else {
                                Diagnostic::WebProcessUnresponsive
                            });
                        });
                    }
                })?;
            }
            let handle = app.handle().clone();
            let client = Client::new(
                app.path().app_data_dir()?.join("connections"),
                Arc::new(move |event| {
                    let _ = handle.emit_to("main", "veranda:event", event);
                }),
            );
            app.manage(client);
            app.manage(probe.clone());
            Ok(())
        })
        .invoke_handler(tauri::generate_handler![
            list_connections,
            load_local_fleet,
            load_fleet,
            assess_connection,
            connect_remote,
            repair_connection,
            assess_removal,
            cancel_assessment,
            remove_connection,
            load_yard,
            load_host,
            plan_operation,
            execute_operation,
            discard_operation,
            cancel_operation,
            launch_session,
            shell_command,
            frontend_diagnostic,
            frontend_ready
        ])
        .build(tauri::generate_context!())
        .expect("failed to build Subyard Veranda");
    app.run(|handle, event| {
        if matches!(
            event,
            tauri::RunEvent::Exit | tauri::RunEvent::ExitRequested { .. }
        ) {
            handle.state::<Arc<Client>>().shutdown();
        }
    });
}
