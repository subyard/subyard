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


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("gui", type=Path)
    parser.add_argument("engine", type=Path)
    parser.add_argument("--remote", action="store_true", help="include real loopback SSH onboarding, cancellation and saved selection lifetimes")
    parser.add_argument("--ui", action="store_true", help="include focused typed GTK pages, plan/discard and Unicode draft restoration")
    args = parser.parse_args()
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
              "scope": "GTK Linux Wayland local/SSH connection SHORT; no resource acceptance" if args.remote else "GTK Linux Wayland read-only shell SHORT; no resource acceptance",
              "gui_sha256": hashlib.sha256(args.gui.read_bytes()).hexdigest(),
              "engine_sha256": hashlib.sha256(args.engine.read_bytes()).hexdigest()}
    try:
        with fixture.recorded_state() as state, contextlib.ExitStack() as stack:
            def retain_evidence():
                result['private_logs'] = {}
                for name in ('app.log', 'input.log', 'app-remote-restore.log', 'input-remote-restore.log', 'app-local-restore.log', 'input-local-restore.log', 'ssh-preflight.log', 'listener.log', 'platform-prepare.log', 'platform-cleanup.log'):
                    path = state / name
                    if path.exists():
                        assert path.stat().st_nlink == 1 and path.stat().st_size <= 2 * 1024 * 1024
                        result['private_logs'][name] = base64.b64encode(path.read_bytes()).decode('ascii')
                result['screenshots'] = {}
                for name in ('useful.png', 'trust.png', 'remote-restore.png', 'local-restore.png'):
                    capture = state / name
                    if capture.exists():
                        assert capture.stat().st_size <= 2 * 1024 * 1024
                        result['screenshots'][name] = base64.b64encode(capture.read_bytes()).decode('ascii')
            stack.callback(retain_evidence)
            env = dict(os.environ, DEBIAN_FRONTEND= "noninteractive")
            run(["sudo", "-n", "apt-get", "update", "-qq"], env, state / "apt-update.log", 180)
            run(["sudo", "-n", "apt-get", "install", "-y", "--no-install-recommends",
                 "libgtk-4-1", "labwc", "wlrctl", "wlr-randr", "wayland-utils", "wtype", "grim",
                 "dbus", "at-spi2-core", "python3-gi", "gir1.2-atspi-2.0", "openssh-server"], env, state / "apt-install.log", 300)
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
                forced = '#!/usr/bin/python3\nimport os,pathlib,shlex,time\n'
                forced += 'root=pathlib.Path(' + repr(str(state)) + ')\n'
                forced += 'args=shlex.split(os.environ.get("SSH_ORIGINAL_COMMAND",""))\n'
                forced += 'assert args in [["yard","rpc","--stdio"],["yard","-Y","default","rpc","--stdio"]]\n'
                forced += 'if (root/"block").exists():\n (root/"blocked-start").touch(mode=0o600)\n time.sleep(60)\n'
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
                ssh = ['ssh', '-F', 'none', '-T', '-o', 'BatchMode=yes', '-o', 'StrictHostKeyChecking=yes',
                       '-o', 'UserKnownHostsFile=' + str(state / 'known_hosts'), '-o', 'GlobalKnownHostsFile=none',
                       '-p', port, '--', account + '@127.0.0.1', 'yard rpc --stdio']
                with fixture.private_log(state / 'ssh-preflight.log') as output, contextlib.redirect_stdout(output):
                    owner = owner_module.Owner(ssh, env=env, timeout=10)
                    try:
                        assert owner.result('rpc.negotiate')['engineVersion'] == result['fleet']['engine_version']
                        assert len(owner.result('owner.inventory')['yards']) == 20
                    finally: owner.close()
                result['authenticated_ssh_preflight'] = True

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
            result['lifetimes'] = []
            scenarios = ['connect', 'remote-restore', 'local-restore'] if args.remote else ['local']
            for scenario in scenarios:
                result["stage"] = "launch-" + scenario
                suffix = '' if scenario in ('local', 'connect') else '-' + scenario
                app_log = stack.enter_context(fixture.private_log(state / ('app' + suffix + '.log')))
                app = subprocess.Popen([str(args.gui.resolve())], env=env, stdout=app_log, stderr=subprocess.STDOUT, start_new_session=True)
                tree = probe.Tree(app.pid, engine=args.engine.resolve())
                stack.enter_context(fixture.cleanup_on_exit(lambda app=app, tree=tree: tree.stop(app), "app-" + scenario))
                driver = ROOT / "veranda/desktop/wayland-input.py"
                capture = state / ('useful.png' if not suffix else scenario + '.png')
                driver_env = dict(env, VERANDA_GTK_PID=str(app.pid), VERANDA_GTK_CAPTURE=str(capture), VERANDA_GTK_SCENARIO=scenario, VERANDA_GTK_UI="1" if args.ui else "0")
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
                frames = [line[len("VERANDA_GTK_INPUT "):] for line in (state / ('input' + suffix + '.log')).read_text().splitlines() if line.startswith("VERANDA_GTK_INPUT ")]
                assert len(frames) == 1
                lifetime = {'scenario': scenario, 'input_exit_code': action.returncode, 'input': json.loads(frames[0])}
                result['lifetimes'].append(lifetime)
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
                    assert selected['tab'] == 'Projects' if scenario != 'local-restore' else selected['tab'] == 'Overview'
                    assert selected['connectionId'] == record['connections'][0]['id'] if scenario != 'remote-restore' else selected['connectionId'] is None
                    lifetime['private_metadata_and_selection_verified'] = True
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
