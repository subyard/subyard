#[cfg(any(feature = "desktop", test))]
#[cfg_attr(all(test, not(feature = "desktop")), allow(dead_code))]
mod local_fleet;

#[cfg(feature = "desktop")]
use local_fleet::{LocalFleetSnapshot, NativeError};

#[cfg(feature = "desktop")]
#[tauri::command]
async fn load_local_fleet() -> Result<LocalFleetSnapshot, NativeError> {
    tauri::async_runtime::spawn_blocking(local_fleet::load_from_yard)
        .await
        .map_err(|_| {
            NativeError::new(
                "native_task_failed",
                "Veranda could not start its local Yard request. Try again.",
            )
        })?
}

#[cfg(feature = "desktop")]
#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    tauri::Builder::default()
        .invoke_handler(tauri::generate_handler![load_local_fleet])
        .run(tauri::generate_context!())
        .expect("failed to run Subyard Veranda");
}

#[cfg(not(feature = "desktop"))]
pub fn run() {}
