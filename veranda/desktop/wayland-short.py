#!/usr/bin/env python3
"""One same-build GTK read/Refresh/close on a leased disposable Linux VM.

Inputs are independently staged GUI/engine ELFs. This is not a resource benchmark.
The existing lease runner owns allocation, renewal, source transfer and release.
"""
import argparse
import base64
import contextlib
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import pwd
import stat
import subprocess
import sys
import time

ROOT = Path(__file__).resolve().parents[2]
sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location("gtk_short_fixture", ROOT / "dev/e2e/veranda-wayland-resources.py")
fixture = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fixture)
probe = fixture.probe


def run(command, env, log, timeout=30):
    with fixture.private_log(log) as output:
        result = subprocess.run(command, env=env, stdout=output, stderr=subprocess.STDOUT, timeout=timeout)
    if result.returncode:
        raise RuntimeError("owned_command_failed")
    return result.returncode


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("gui", type=Path)
    parser.add_argument("engine", type=Path)
    args = parser.parse_args()
    if os.environ.get("SUBYARD_E2E_VM") != "1" or os.getuid() == 0:
        parser.error("requires an allocated unprivileged disposable VM")
    for path in (args.gui, args.engine):
        info = path.lstat()
        if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or info.st_uid != os.getuid():
            parser.error("inputs must be owned plain independent ELFs")
    started = time.monotonic()
    result = {"schema_version": 1, "original_exit_code": 1, "stage": "packages",
              "run": os.environ.get("SUBYARD_E2E_RUN_ID"), "slot": os.environ.get("SUBYARD_E2E_SLOT"),
              "scope": "GTK Linux Wayland read-only shell SHORT; no resource acceptance",
              "gui_sha256": hashlib.sha256(args.gui.read_bytes()).hexdigest(),
              "engine_sha256": hashlib.sha256(args.engine.read_bytes()).hexdigest()}
    try:
        with fixture.recorded_state() as state, contextlib.ExitStack() as stack:
            def retain_evidence():
                result['private_logs'] = {}
                for name in ('app.log', 'input.log', 'platform-prepare.log', 'platform-cleanup.log'):
                    path = state / name
                    if path.exists():
                        assert path.stat().st_nlink == 1 and path.stat().st_size <= 2 * 1024 * 1024
                        result['private_logs'][name] = base64.b64encode(path.read_bytes()).decode('ascii')
                capture = state / 'useful.png'
                if capture.exists():
                    result['useful_png'] = base64.b64encode(capture.read_bytes()).decode('ascii')
            stack.callback(retain_evidence)
            env = dict(os.environ, DEBIAN_FRONTEND= "noninteractive")
            run(["sudo", "-n", "apt-get", "update", "-qq"], env, state / "apt-update.log", 180)
            run(["sudo", "-n", "apt-get", "install", "-y", "--no-install-recommends",
                 "libgtk-4-1", "labwc", "wlrctl", "wlr-randr", "wayland-utils", "wtype", "grim",
                 "dbus", "at-spi2-core", "python3-gi", "gir1.2-atspi-2.0"], env, state / "apt-install.log", 300)
            result["stage"] = "platform"
            stack.enter_context(fixture.prepared_platform(state, args.engine))
            for name in ("home", "runtime", "xdg-config", "xdg-cache", "xdg-data", "owner-config", "owner-data", "fleet", "compositor", "bin"):
                probe.private_dir(state / name)
            (state / "bin/yard").symlink_to(args.engine.resolve())
            operator = Path(pwd.getpwuid(os.getuid()).pw_dir)
            env = {"PATH": str(state / "bin") + ":/usr/local/bin:/usr/bin:/bin", "LANG": "C.UTF-8",
                   "HOME": str(state / "home"), "XDG_RUNTIME_DIR": str(state / "runtime"),
                   "XDG_CONFIG_HOME": str(state / "xdg-config"), "XDG_CACHE_HOME": str(state / "xdg-cache"),
                   "XDG_DATA_HOME": str(state / "xdg-data"), "XDG_SESSION_TYPE": "wayland",
                   "SUBYARD_OPERATOR_HOME": str(operator), "SUBYARD_CONFIG_HOME": str(state / "owner-config"),
                   "SUBYARD_HOME": str(state / "owner-data"), "SUBYARD_REPOSITORY_ROOT": str(ROOT),
                   "YARD_ENGINE_PATH": str(args.engine.resolve()), "SUBYARD_NO_AUDIT": "1",
                   "SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE": "1", "MIN_DISK_GIB": "1",
                   "STORAGE_PATH": str(operator / ".cache/subyard-e2e-platform/incus/incus/storage")}
            result["stage"] = "fleet"
            with fixture.private_log(state / "fleet.log") as output, contextlib.redirect_stdout(output):
                result["fleet"] = probe.configured_fleet(state / "fleet", env)
            probe.private_file(state / "compositor/rc.xml", "<labwc_config><core><xwayland>no</xwayland></core></labwc_config>\n")
            env.update(WLR_BACKENDS="headless", WLR_HEADLESS_OUTPUTS="1", WLR_RENDERER="pixman",
                       WLR_LIBINPUT_NO_DEVICES="1", LABWC_UPDATE_ACTIVATION_ENV="0",
                       XDG_CONFIG_DIRS=str(state / "compositor"), GDK_BACKEND="wayland", GSK_RENDERER="cairo",
                       GTK_A11Y="atspi", DBUS_SESSION_BUS_ADDRESS="unix:path=" + str(state / "session-bus"))

            def service(name, command):
                log = stack.enter_context(fixture.private_log(state / (name + ".log")))
                process = subprocess.Popen(command, env=env, stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
                tree = probe.Tree(process.pid)
                stack.enter_context(fixture.cleanup_on_exit(lambda: tree.stop(process), name))
                return process, tree

            result["stage"] = "compositor"
            compositor, _ = service("compositor", ["labwc", "-V", "-C", str(state / "compositor")])
            deadline = time.monotonic() + 15
            while True:
                assert compositor.poll() is None and time.monotonic() < deadline
                sockets = [path for path in (state / "runtime").glob("wayland-*") if stat.S_ISSOCK(path.lstat().st_mode)]
                if len(sockets) == 1:
                    env["WAYLAND_DISPLAY"] = str(sockets[0])
                    break
                time.sleep(.05)
            assert "pixman" in (state / "compositor.log").read_text().lower()
            run(["wlr-randr", "--output", "HEADLESS-1", "--custom-mode", "1920x1080@60Hz", "--scale", "1"], env, state / "geometry.log", 5)
            result["display"] = fixture.output_geometry(subprocess.check_output(["wayland-info", "--interface", "wl_output"], env=env, text=True, stderr=subprocess.DEVNULL, timeout=5))
            service("session-bus", ["dbus-daemon", "--session", "--nofork", "--nopidfile", "--address=" + env["DBUS_SESSION_BUS_ADDRESS"]])
            deadline = time.monotonic() + 10
            while not (state / "session-bus").exists():
                assert time.monotonic() < deadline
                time.sleep(.05)
            launchers = [Path(path) for path in subprocess.check_output(["dpkg-query", "-L", "at-spi2-core"], text=True).splitlines() if path.endswith("/at-spi-bus-launcher")]
            assert len(launchers) == 1
            service("accessibility", [str(launchers[0]), "--launch-immediately"])
            service("keyboard", ["wtype", "-s", "120000"])
            result["stage"] = "launch"
            app_log = stack.enter_context(fixture.private_log(state / "app.log"))
            app = subprocess.Popen([str(args.gui.resolve())], env=env, stdout=app_log, stderr=subprocess.STDOUT, start_new_session=True)
            tree = probe.Tree(app.pid, engine=args.engine.resolve())
            stack.enter_context(fixture.cleanup_on_exit(lambda: tree.stop(app), "app"))
            # The driver uses AT-SPI only for reads. Every action is a real
            # virtual-keyboard key delivered through the owned Wayland seat.
            driver = ROOT / "veranda/desktop/wayland-input.py"
            driver_env = dict(env, VERANDA_GTK_PID=str(app.pid), VERANDA_GTK_CAPTURE=str(state / 'useful.png'))
            result["stage"] = "physical-action"
            with fixture.private_log(state / "input.log") as output:
                action = subprocess.Popen([sys.executable, str(driver)], env=driver_env, stdout=output, stderr=subprocess.STDOUT, start_new_session=True)
                action_tree = probe.Tree(action.pid)
                stack.enter_context(fixture.cleanup_on_exit(lambda: action_tree.stop(action), "input"))
                deadline = time.monotonic() + 90
                while action.poll() is None:
                    tree.sample()
                    assert time.monotonic() < deadline
                    time.sleep(.05)
            result["input_exit_code"] = action.returncode
            frames = [line[len("VERANDA_GTK_INPUT "):] for line in (state / "input.log").read_text().splitlines() if line.startswith("VERANDA_GTK_INPUT ")]
            assert len(frames) == 1
            result["input"] = json.loads(frames[0])
            assert action.returncode == 0
            result["stage"] = "natural-close"
            result["gui_original_exit_code"] = app.wait(timeout=15)
            assert app.returncode == 0
            history = tree.process_history()
            assert any("local-core" in row["observed_roles"] for row in history)
            assert all(row["alive_at_end"] is False for row in history)
            result["observed_owned_engine_count"] = sum("local-core" in row["observed_roles"] for row in history)
            result["all_observed_owned_processes_naturally_dead"] = True
            result["shutdown_scope"] = "observed GUI/engine identities; no remote SSH used by this local SHORT"
            result["stage"] = "cleanup"
        result.update(original_exit_code=0, fixture_cleanup="passed", stage="completed")
    except BaseException as error:
        result["failure_type"] = type(error).__name__
    result["duration_seconds"] = time.monotonic() - started
    print("VERANDA_GTK_SHORT " + json.dumps(result), flush=True)
    return result["original_exit_code"]


if __name__ == "__main__":
    sys.exit(main())
