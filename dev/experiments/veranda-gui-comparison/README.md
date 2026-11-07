# Synthetic Veranda GUI comparison

This standalone experiment compares egui/eframe with a small GTK4 C control. It
does not depend on Veranda, contact an owner, or change the selected GTK toolkit.
Both windows contain 10 synthetic yards and 100 projects, selection, scrolling,
an editable project-name filter, and a Refresh button that advances an in-memory
snapshot counter. Refresh deliberately performs no I/O.

The [comparison report](../../../docs/veranda/native-framework-comparison.md)
contains real screenshots, measurements and their limits.

## Build

Use a leased disposable Linux VM for the graphical workload, following
[the VM guide](../../../docs/test-vms.md). On Debian 13 the required packages are
`build-essential pkg-config libgtk-4-dev libxkbcommon-dev libegl1-mesa-dev
libgl1-mesa-dev libx11-dev libxi-dev libxrandr-dev libxcursor-dev xvfb openbox
xdotool x11-utils imagemagick mesa-utils dbus`. Install Rust through the existing
`subyard-dev` profile's `--rust` provisioning option if unavailable.

From the repository root, with Rust 1.95 or newer and Cargo on PATH:

```sh
export demo=dev/experiments/veranda-gui-comparison
cargo_environment="$(python3 dev/cargo_cache.py --manifest-path "$demo/Cargo.toml" --target-dir "$PWD/.build/gui-comparison/target" --snapshot --run-target)"
eval "$cargo_environment"
cargo fmt --manifest-path "$demo/Cargo.toml" --check
cargo build --release --locked --manifest-path "$demo/Cargo.toml"
cc -O2 -Wall -Wextra -Werror "$demo/gtk-control.c" -o "$CARGO_TARGET_DIR/gtk-control" $(pkg-config --cflags --libs gtk4)
```

`Cargo.toml` pins eframe 0.37.0 and `Cargo.lock` pins transitive dependencies.
Only `accesskit`, `default_fonts`, `glow` and `x11` features are enabled. The
`wgpu`, `wayland` and `persistence` features are disabled; the app never requests
continuous repaint. Glow uses OpenGL; in this VM
Mesa llvmpipe provides software rendering. GTK uses its system libraries and requests Cairo.
This is a release build; it is not a renderer-independent toolkit memory floor.

## Run and measure

Use the same X server, window manager, font/DPI settings and application size for
both executables. Inside the disposable VM, start a session bus with
`dbus-run-session -- bash`, then prepare the display:

```sh
export DISPLAY=:99 LIBGL_ALWAYS_SOFTWARE=1 GDK_BACKEND=x11 GSK_RENDERER=cairo
Xvfb :99 -screen 0 1920x1080x24 -nolisten tcp >xvfb.log 2>&1 &
sleep 2
openbox >openbox.log 2>&1 &
"$CARGO_TARGET_DIR/release/veranda-egui-comparison" >egui.log 2>&1 &
pid=$!
```

Run the GTK control separately, after closing and waiting for egui, by replacing
the executable with `"$CARGO_TARGET_DIR/gtk-control"`. Both request 1120×768 windows.

First exercise yard navigation, project selection, scrolling, filter editing and
Refresh. The application logs these changes on stdout. Restore the empty filter,
select All projects, unfocus the text entry and move the pointer outside the
window before measuring. Capture screenshots before measurement, for example
`import -window "$window" screenshot.png`. Obtain the exact PID from the launcher
and the window ID from `xdotool search --onlyvisible --all --pid "$pid" --name 'Veranda'`.

```sh
python3 "$demo/measure.py" --pid "$pid" --window "$window" --state visible --output visible.json
xdotool windowminimize "$window"
python3 "$demo/measure.py" --pid "$pid" --window "$window" --state minimized --output minimized.json
```

The probe checks mapped/hidden window state before and after each phase, settles
for 30 seconds, then reads 120 one-second samples plus a CPU baseline. CPU is the
process's user+system tick delta divided by elapsed monotonic time, normalized to
one logical CPU. RSS/PSS come from `smaps_rollup`; p95 uses nearest rank. PID start
time must stay constant and any child process causes refusal, since these demos
have single-process scope. A failed/missing read aborts, rather than fabricating a
sample. The JSON preserves every raw sample and summary. The X server, window
manager, accessibility/session services and probe are excluded.
Guest CPU idle/steal counters and CPU pressure samples are also retained; they do
not establish absence of external load on the physical host.

Close each window normally and wait for its process to exit. Export raw samples,
logs, versions, source/binary hashes and screenshots before releasing the VM.
One run cannot establish across-machine variance, production accessibility,
real-backend cost or full Veranda acceptance.

The report binds measured sources by hashing this manifest (from the repository root):

```sh
(
    cd "$demo"
    sha256sum Cargo.lock Cargo.toml gtk-control.c measure.py src/main.rs
) | sha256sum
```
