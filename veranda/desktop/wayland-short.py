#!/usr/bin/env python3
"""One same-build GTK read/Refresh/close on owned Wayland or X11 in a disposable VM.

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
import re
import selectors
import shlex
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


def wait_x11_ready(display, manager, env, deadline, log):
    command = ['xdotool', 'getdisplaygeometry' if manager is None else 'get_num_desktops']
    with fixture.private_log(log) as output:
        while True:
            assert display.poll() is None, 'owned_x11_display_died'
            assert manager is None or manager.poll() is None, 'owned_x11_window_manager_died'
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise TimeoutError('x11_startup_readiness_timeout')
            try:
                result = subprocess.run(command, env=env, capture_output=True, text=True, timeout=min(1, remaining))
                output.write(result.stdout[:1024] + result.stderr[:1024])
                value = result.stdout.strip()
                ready = (re.fullmatch(r'[1-9][0-9]* [1-9][0-9]*', value) if manager is None
                         else re.fullmatch(r'[1-9][0-9]*', value))
                assert display.poll() is None, 'owned_x11_display_died'
                assert manager is None or manager.poll() is None, 'owned_x11_window_manager_died'
                if result.returncode == 0 and ready and time.monotonic() < deadline:
                    return
            except subprocess.TimeoutExpired:
                output.write('xdotool readiness probe timed out\n')
            time.sleep(min(.05, max(0, deadline - time.monotonic())))


def ssh_owner_argv(state, port, account):
    # Owner appends its RPC subcommand after this transport/engine prefix.
    return ['ssh', '-F', 'none', '-T', '-o', 'BatchMode=yes', '-o', 'StrictHostKeyChecking=yes',
            '-o', 'UserKnownHostsFile=' + str(state / 'known_hosts'), '-o', 'GlobalKnownHostsFile=none',
            '-p', port, '--', account + '@127.0.0.1', 'yard']


def retain_lifetime_snapshot(lifetime, tree, app):
    # Runs before ExitStack cleanup on every success/failure path. A diagnostic
    # failure cannot replace the original action/GUI failure.
    try:
        lifetime['process_snapshot_before_cleanup'] = tree.sample()
        lifetime['observed_process_history'] = tree.process_history()
        lifetime['gui_exit_at_snapshot'] = app.poll()
    except BaseException as error:
        lifetime['process_snapshot_failure_type'] = type(error).__name__


def short_scenarios(remote, owner_focus):
    if remote and owner_focus:
        raise ValueError('owner focus diagnostic is local only')
    return ['owner-focus'] if owner_focus else ['connect', 'remote-restore', 'local-restore'] if remote else ['local']


def verify_persisted_selection(selected, connection_id, scenario):
    assert scenario in ('connect', 'remote-restore', 'local-restore')
    assert selected['tab'] == ('Overview' if scenario == 'local-restore' else 'Projects')
    assert selected['connectionId'] == (None if scenario == 'remote-restore' else connection_id)
    assert selected['yardName'] == (None if scenario == 'local-restore' else 'default')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("gui", type=Path)
    parser.add_argument("engine", type=Path)
    parser.add_argument("--x11", action="store_true", help="run the SHORT on a private Xvfb display instead of Wayland")
    parser.add_argument("--remote", action="store_true", help="include real loopback SSH onboarding, cancellation and saved selection lifetimes")
    parser.add_argument("--ui", action="store_true", help="include focused typed GTK pages, plan/discard and Unicode draft restoration")
    parser.add_argument("--owner-focus", action="store_true", help="diagnose one local owner-row physical focus lifetime")
    parser.add_argument("--prepared-assets", type=Path, help="validated staged runtime and exact Debian container image assets")
    args = parser.parse_args()
    if args.owner_focus and (args.remote or args.ui):
        parser.error("--owner-focus requires a local lifetime without --remote or --ui")
    if args.ui and not args.remote:
        parser.error("--ui currently requires the three --remote lifetimes")
    if os.environ.get("SUBYARD_E2E_VM") != "1" or os.getuid() == 0:
        parser.error("requires an allocated unprivileged disposable VM")
    for path in (args.gui, args.engine):
        info = path.lstat()
        if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or info.st_uid != os.getuid():
            parser.error("inputs must be owned plain independent ELFs")
    started = time.monotonic()
    result = {"schema_version": 1, "original_exit_code": 1, "stage": "packages",
              "run": os.environ.get("SUBYARD_E2E_RUN_ID"), "slot": os.environ.get("SUBYARD_E2E_SLOT"),
              "backend": "x11" if args.x11 else "wayland",
              "scope": "GTK Linux local/SSH connection SHORT; no resource acceptance" if args.remote else "GTK Linux read-only shell SHORT; no resource acceptance",
              "gui_sha256": hashlib.sha256(args.gui.read_bytes()).hexdigest(),
              "engine_sha256": hashlib.sha256(args.engine.read_bytes()).hexdigest()}
    try:
        with fixture.recorded_state() as state, contextlib.ExitStack() as stack:
            state_info = state.stat()
            result['fixture_state_identity'] = {'name': state.name, 'device': state_info.st_dev,
                                                'inode': state_info.st_ino}
            def retain_evidence():
                result['private_logs'] = {}
                for name in ('app.log', 'input.log', 'app-remote-restore.log', 'input-remote-restore.log', 'app-local-restore.log', 'input-local-restore.log', 'ssh-preflight.log', 'sshd.log', 'listener.log', 'platform-prepare.log', 'platform-cleanup.log', 'xvfb.log', 'window-manager.log', 'x11-display-ready.log', 'x11-wm-ready.log', 'session-bus.log', 'accessibility.log', 'fleet.log'):
                    path = state / name
                    if path.exists():
                        assert path.stat().st_nlink == 1 and path.stat().st_size <= 2 * 1024 * 1024
                        result['private_logs'][name] = base64.b64encode(path.read_bytes()).decode('ascii')
                result['screenshots'] = {}
                for name in ('useful.png', 'failure.png', 'trust.png', 'remote-restore.png', 'local-restore.png'):
                    capture = state / name
                    if capture.exists():
                        assert capture.stat().st_size <= 2 * 1024 * 1024
                        result['screenshots'][name] = base64.b64encode(capture.read_bytes()).decode('ascii')
            stack.callback(retain_evidence)
            env = dict(os.environ, DEBIAN_FRONTEND= "noninteractive")
            run(["sudo", "-n", "apt-get", "update", "-qq"], env, state / "apt-update.log", 180)
            display_packages = ["xvfb", "openbox", "xdotool", "libx11-6"] if args.x11 else ["labwc", "wlrctl", "wlr-randr", "wayland-utils", "wtype", "grim"]
            run(["sudo", "-n", "apt-get", "install", "-y", "--no-install-recommends", *display_packages,
                 "libgtk-4-1",
                 "dbus", "at-spi2-core", "python3-gi", "gir1.2-atspi-2.0", "openssh-server"], env, state / "apt-install.log", 300)
            result["stage"] = "platform"
            stack.enter_context(fixture.prepared_platform(state, args.engine, prepared_assets=args.prepared_assets)
                                if args.prepared_assets is not None else fixture.prepared_platform(state, args.engine))
            for name in ("home", "runtime", "xdg-config", "xdg-cache", "xdg-data", "owner-config", "owner-data", "fleet", "compositor", "bin"):
                probe.private_dir(state / name)
            (state / "bin/yard").symlink_to(args.engine.resolve())
            operator = Path(pwd.getpwuid(os.getuid()).pw_dir)
            env = {"PATH": str(state / "bin") + ":/usr/local/bin:/usr/bin:/bin", "LANG": "C.UTF-8",
                   "HOME": str(state / "home"), "XDG_RUNTIME_DIR": str(state / "runtime"),
                   "XDG_CONFIG_HOME": str(state / "xdg-config"), "XDG_CACHE_HOME": str(state / "xdg-cache"),
                   "XDG_DATA_HOME": str(state / "xdg-data"), "XDG_SESSION_TYPE": "x11" if args.x11 else "wayland",
                   "SUBYARD_OPERATOR_HOME": str(operator), "SUBYARD_CONFIG_HOME": str(state / "owner-config"),
                   "SUBYARD_HOME": str(state / "owner-data"), "SUBYARD_REPOSITORY_ROOT": str(ROOT),
                   "YARD_ENGINE_PATH": str(args.engine.resolve()), "SUBYARD_NO_AUDIT": "1",
                   "SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE": "1", "MIN_DISK_GIB": "1",
                   "STORAGE_PATH": str(operator / ".cache/subyard-e2e-platform/incus/incus/storage")}
            result["stage"] = "fleet"
            with fixture.private_log(state / "fleet.log") as output, contextlib.redirect_stdout(output):
                result["fleet"] = probe.configured_fleet(state / "fleet", env)
            probe.private_file(state / "compositor/rc.xml", "<labwc_config><core><xwayland>no</xwayland></core></labwc_config>\n")
            env.update(GDK_BACKEND="x11" if args.x11 else "wayland", GSK_RENDERER="cairo",
                       GTK_A11Y="atspi", DBUS_SESSION_BUS_ADDRESS="unix:path=" + str(state / "session-bus"))
            if not args.x11:
                env.update(WLR_BACKENDS="headless", WLR_HEADLESS_OUTPUTS="1", WLR_RENDERER="pixman",
                           WLR_LIBINPUT_NO_DEVICES="1", LABWC_UPDATE_ACTIVATION_ENV="0",
                           XDG_CONFIG_DIRS=str(state / "compositor"))

            def service(name, command, **options):
                log = stack.enter_context(fixture.private_log(state / (name + ".log")))
                process = subprocess.Popen(command, env=env, stdout=log, stderr=subprocess.STDOUT, start_new_session=True, **options)
                tree = probe.Tree(process.pid)
                stack.enter_context(fixture.cleanup_on_exit(lambda: tree.stop(process), name))
                return process, tree

            if args.remote:
                result['stage'] = 'ssh-fixture'
                for name in ('remote-config', 'remote-data', 'remote-fleet'):
                    probe.private_dir(state / name)
                remote_env = dict(env, SUBYARD_CONFIG_HOME=str(state / 'remote-config'), SUBYARD_HOME=str(state / 'remote-data'))
                with fixture.private_log(state / 'remote-fleet.log') as output, contextlib.redirect_stdout(output):
                    result['remote_fleet'] = probe.configured_fleet(state / 'remote-fleet', remote_env)
                for key in ('host-key', 'host-key-next', 'agent-key'):
                    run(['ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-f', str(state / key)], env, state / (key + '.log'), 5)
                    (state / key).chmod(0o600)
                    (state / (key + '.pub')).chmod(0o600)
                probe.private_file(state / 'authorized_keys', (state / 'agent-key.pub').read_text())
                fingerprints = [subprocess.check_output(['ssh-keygen', '-lf', str(state / (key + '.pub')), '-E', 'sha256'], stderr=subprocess.DEVNULL, timeout=5).decode().split()[1] for key in ('host-key', 'host-key-next')]
                env['SSH_AUTH_SOCK'] = str(state / 'agent.sock')
                service('agent', ['ssh-agent', '-D', '-a', env['SSH_AUTH_SOCK']])
                deadline = time.monotonic() + 5
                while not (state / 'agent.sock').is_socket():
                    assert time.monotonic() < deadline
                    time.sleep(.05)
                run(['ssh-add', str(state / 'agent-key')], env, state / 'ssh-add.log', 5)
                remote_env['SSH_AUTH_SOCK'] = env['SSH_AUTH_SOCK']
                captured_yards = sorted(path.name for path in (state / 'remote-config/yards').iterdir() if path.is_dir() and not path.is_symlink())
                assert len(captured_yards) == 20 and all(re.fullmatch(r'[a-z0-9][a-z0-9-]{0,62}', name) for name in captured_yards)
                result['captured_rpc_yards'] = captured_yards
                forced = '#!/usr/bin/python3\nimport importlib.util,os,pathlib,shlex,time\n'
                forced += 'root=pathlib.Path(' + repr(str(state)) + ')\n'
                forced += 'args=shlex.split(os.environ.get("SSH_ORIGINAL_COMMAND",""))\n'
                forced += 'spec=importlib.util.spec_from_file_location("ssh_fixture",' + repr(str(ROOT / 'veranda/desktop/ssh-fixture.py')) + ')\nhelper=importlib.util.module_from_spec(spec);spec.loader.exec_module(helper)\n'
                forced += 'assert helper.allowed_rpc(args,' + repr(captured_yards) + ')\n'
                forced += 'if helper.maybe_block(root,args,os.environ.get("SSH_CONNECTION","")):\n time.sleep(60)\n'
                forced += 'engine=' + repr(str(args.engine.resolve())) + '\nos.execve(engine,[engine]+args[1:],' + repr(remote_env) + ')\n'
                probe.private_file(state / 'bin/owner-rpc', forced)
                (state / 'bin/owner-rpc').chmod(0o700)
                account = pwd.getpwuid(os.getuid()).pw_name
                config = ['ListenAddress 127.0.0.1', 'HostKey ' + str(state / 'host-key'),
                          'PidFile ' + str(state / 'sshd.pid'), 'AuthorizedKeysFile ' + str(state / 'authorized_keys'),
                          'AllowUsers ' + account, 'AuthenticationMethods publickey', 'PasswordAuthentication no',
                          'KbdInteractiveAuthentication no', 'UsePAM no', 'StrictModes no', 'AllowTcpForwarding no',
                          'AllowAgentForwarding no', 'X11Forwarding no', 'PermitTTY no', 'PermitUserRC no',
                          'PerSourcePenalties no', 'LogLevel QUIET', 'ForceCommand ' + str(state / 'bin/owner-rpc')]
                probe.private_file(state / 'sshd_config', '\n'.join(config) + '\n')
                for name in ('sshd.log', 'known_hosts', 'port'):
                    probe.private_file(state / name, '')
                helper = 'source ' + shlex.quote(str(ROOT / 'tests/helpers/loopback-sshd.sh')) + '\n'
                helper += 'start_loopback_sshd ' + shlex.quote(str(state)) + ' /usr/sbin/sshd 43771 || exit 1\n'
                helper += 'printf "%s" "$port" > ' + shlex.quote(str(state / 'port')) + '\n'
                helper += 'while [ ! -f ' + shlex.quote(str(state / 'rotate')) + ' ]; do kill -0 "$sshd_pid" || exit 1; sleep 0.05; done\n'
                helper += 'kill "$sshd_pid"\nwait "$sshd_pid" || true\n'
                helper += 'cp ' + shlex.quote(str(state / 'host-key-next')) + ' ' + shlex.quote(str(state / 'host-key')) + '\n'
                helper += 'cp ' + shlex.quote(str(state / 'host-key-next.pub')) + ' ' + shlex.quote(str(state / 'host-key.pub')) + '\n'
                helper += 'start_loopback_sshd ' + shlex.quote(str(state)) + ' /usr/sbin/sshd "$port" || exit 1\n'
                helper += 'touch ' + shlex.quote(str(state / 'rotated')) + '\nwait "$sshd_pid"\n'
                listener, _ = service('listener', ['/bin/bash', '-c', helper])
                deadline = time.monotonic() + 15
                while not (state / 'port').read_text():
                    assert listener.poll() is None and time.monotonic() < deadline
                    time.sleep(.05)
                port = (state / 'port').read_text()
                assert port.isdecimal()
                destination = account + '@127.0.0.1:' + port
                # Minimum prerequisite: actual authenticated RPC inventory before
                # GTK actions; no mocked frames or compatibility rewrites.
                spec = importlib.util.spec_from_file_location('remote_owner', ROOT / 'dev/e2e/veranda-owner.py')
                owner_module = importlib.util.module_from_spec(spec); spec.loader.exec_module(owner_module)
                ssh = ssh_owner_argv(state, port, account)
                result['stage'] = 'authenticated-ssh-preflight'
                with fixture.private_log(state / 'ssh-preflight.log') as output, contextlib.redirect_stdout(output):
                    owner = owner_module.Owner(ssh, env=env, timeout=10)
                    try:
                        assert owner.result('rpc.negotiate')['engineVersion'] == result['fleet']['engine_version']
                        inventory = owner.result('owner.inventory')
                        assert sorted(yard['name'] for yard in inventory['yards']) == captured_yards
                    finally: owner.close()
                result['authenticated_ssh_preflight'] = True

            result["stage"] = "display"
            if args.x11:
                x11_deadline = time.monotonic() + 10
                read_fd, write_fd = os.pipe()
                try:
                    try:
                        # Keep our private display alive across short readiness clients.
                        display, _ = service("xvfb", ["Xvfb", "-noreset", "-displayfd", str(write_fd), "-screen", "0", "1280x800x24", "-nolisten", "tcp"], pass_fds=(write_fd,))
                    finally:
                        os.close(write_fd)
                    with selectors.DefaultSelector() as selector:
                        selector.register(read_fd, selectors.EVENT_READ)
                        assert selector.select(min(5, max(0, x11_deadline - time.monotonic()))), 'x11_display_readiness_timeout'
                    number = os.read(read_fd, 32).decode().strip()
                    assert re.fullmatch(r'[0-9]{1,5}', number) and display.poll() is None
                    env['DISPLAY'] = ':' + number
                finally:
                    os.close(read_fd)
                wait_x11_ready(display, None, env, x11_deadline, state / 'x11-display-ready.log')
                result['display'] = {'server': 'Xvfb', 'screen': '1280x800x24', 'rendering': 'cairo'}
            else:
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
            if args.x11:
                manager, _ = service("window-manager", ["openbox"])
                result['stage'] = 'x11-window-manager'
                wait_x11_ready(display, manager, env, x11_deadline, state / 'x11-wm-ready.log')
            else:
                service("keyboard", ["wtype", "-s", "120000"])
            result['lifetimes'] = []
            scenarios = short_scenarios(args.remote, args.owner_focus)
            for scenario in scenarios:
                result["stage"] = "launch-" + scenario
                suffix = '' if scenario in ('local', 'connect', 'owner-focus') else '-' + scenario
                app_log = stack.enter_context(fixture.private_log(state / ('app' + suffix + '.log')))
                app = subprocess.Popen([str(args.gui.resolve())], env=env, stdout=app_log, stderr=subprocess.STDOUT, start_new_session=True)
                tree = probe.Tree(app.pid, engine=args.engine.resolve())
                stack.enter_context(fixture.cleanup_on_exit(lambda app=app, tree=tree: tree.stop(app), "app-" + scenario))
                lifetime = {'scenario': scenario}
                result['lifetimes'].append(lifetime)
                try:
                    driver = ROOT / "veranda/desktop/wayland-input.py"
                    capture = state / ('useful.png' if not suffix else scenario + '.png')
                    driver_env = dict(env, VERANDA_GTK_PID=str(app.pid), VERANDA_GTK_CAPTURE=str(capture), VERANDA_GTK_SCENARIO=scenario, VERANDA_GTK_UI="1" if args.ui else "0", VERANDA_GTK_BACKEND=result['backend'])
                    if args.remote:
                        driver_env.update(VERANDA_GTK_DESTINATION=destination, VERANDA_GTK_FINGERPRINT=fingerprints[0],
                                          VERANDA_GTK_REPLACEMENT=fingerprints[1], VERANDA_GTK_BLOCK=str(state / 'block'))
                    result["stage"] = "physical-action-" + scenario
                    with fixture.private_log(state / ('input' + suffix + '.log')) as output:
                        action = subprocess.Popen([sys.executable, str(driver)], env=driver_env, stdout=output, stderr=subprocess.STDOUT, start_new_session=True)
                        action_tree = probe.Tree(action.pid)
                        stack.enter_context(fixture.cleanup_on_exit(lambda action=action, tree=action_tree: tree.stop(action), "input-" + scenario))
                        deadline = time.monotonic() + (180 if args.remote else 90)
                        while action.poll() is None:
                            tree.sample()
                            assert time.monotonic() < deadline
                            time.sleep(.05)
                    lifetime['input_exit_code'] = action.returncode
                    frames = [line[len("VERANDA_GTK_INPUT "):] for line in (state / ('input' + suffix + '.log')).read_text().splitlines() if line.startswith("VERANDA_GTK_INPUT ")]
                    assert len(frames) == 1
                    lifetime.update(input_exit_code=action.returncode, input=json.loads(frames[0]))
                    assert action.returncode == 0
                    result["stage"] = "natural-close-" + scenario
                    lifetime['gui_original_exit_code'] = app.wait(timeout=15)
                    assert app.returncode == 0
                    history = tree.process_history()
                    assert any("local-core" in row["observed_roles"] for row in history)
                    assert all(row["alive_at_end"] is False for row in history)
                    lifetime['observed_process_history'] = history
                    lifetime['all_observed_owned_processes_naturally_dead'] = True
                    if args.remote:
                        assert any('app-ssh-helper' in row['observed_roles'] for row in history)
                        store = state / 'xdg-data/subyard/veranda'
                        record = json.loads((store / 'connections.json').read_bytes())
                        assert len(record['connections']) == 1
                        for path in store.iterdir():
                            info = path.lstat()
                            assert info.st_uid == os.getuid() and info.st_nlink == 1 and stat.S_IMODE(info.st_mode) == 0o600
                        selected = json.loads((store / 'navigation.json').read_bytes())
                        verify_persisted_selection(selected, record['connections'][0]['id'], scenario)
                        lifetime['private_metadata_and_selection_verified'] = True
                finally:
                    retain_lifetime_snapshot(lifetime, tree, app)
            result['shutdown_scope'] = 'observed GUI/local engine/owned SSH PID+start identities; child wait status is not retained by client'
            result["stage"] = "cleanup"
        result.update(original_exit_code=0, fixture_cleanup="passed", stage="completed")
    except BaseException as error:
        result["failure_type"] = type(error).__name__
    result["duration_seconds"] = time.monotonic() - started
    print("VERANDA_GTK_SHORT " + json.dumps(result), flush=True)
    return result["original_exit_code"]


if __name__ == "__main__":
    sys.exit(main())
