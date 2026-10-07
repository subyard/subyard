#[cfg_attr(not(feature = "desktop"), allow(dead_code))]
mod local_fleet;

#[cfg_attr(not(feature = "desktop"), allow(dead_code))]
mod client;
#[cfg_attr(not(feature = "desktop"), allow(dead_code))]
mod connections;
#[cfg_attr(not(feature = "desktop"), allow(dead_code))]
mod sessions;
#[cfg_attr(not(feature = "desktop"), allow(dead_code))]
mod ssh;
#[cfg_attr(not(feature = "desktop"), allow(dead_code))]
mod transport;

#[cfg(feature = "desktop")]
mod desktop;

#[cfg(feature = "desktop")]
#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    desktop::run();
}

#[cfg(not(feature = "desktop"))]
pub fn run() {}
