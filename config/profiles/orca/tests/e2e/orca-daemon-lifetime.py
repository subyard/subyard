#!/usr/bin/env python3
"""Process and cgroup evidence for the disposable Orca resource fixture."""
import json
from pathlib import Path
import pwd
import re
import subprocess
import sys
import time


def output(*command):
    return subprocess.check_output(command, text=True).strip()


def identity(pid, proc=Path("/proc")):
    process = proc / str(pid)
    try:
        # Read start ticks last so a reused PID cannot validate a replacement's cgroup.
        cgroup = (process / "cgroup").read_text().strip()
        fields = (process / "stat").read_text().rsplit(")", 1)[1].split()
        return {"pid": int(pid), "start": fields[19], "parent": int(fields[1]),
                "cgroup": cgroup}
    except FileNotFoundError:
        return None


def user_systemctl(*arguments):
    uid = pwd.getpwnam("dev").pw_uid
    return output("runuser", "-u", "dev", "--", "env", f"XDG_RUNTIME_DIR=/run/user/{uid}",
                  f"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/{uid}/bus",
                  "systemctl", "--user", *arguments)


def placement_diagnostics(uid, pty):
    """Print why Orca's daemon did not select the user scope; Orca logs none of this."""
    print(f"orca-resource: PTY cgroup: {pty['cgroup']}", file=sys.stderr)
    login_env = ["runuser", "-u", "dev", "--", "env", f"XDG_RUNTIME_DIR=/run/user/{uid}"]
    for command in (
        ["systemctl", "show", f"user@{uid}.service", "--property=ActiveState,SubState,Result,ActiveEnterTimestamp"],
        ["systemctl", "show", "subyard-orca.service", "--property=ActiveEnterTimestamp"],
        ["ls", "-l", f"/run/user/{uid}"],
        ["bash", "-c", "for pid in $(pgrep -f daemon-entry); do printf '%s ' \"$pid\"; cat /proc/$pid/cgroup; done"],
        login_env + ["systemctl", "--user", "list-units", "--no-pager", "--plain", "--no-legend", "--type=socket"],
        login_env + ["systemctl", "--user", "list-units", "--no-pager", "--plain", "--no-legend", "--type=scope"],
        login_env + ["systemd-run", "--user", "--scope", "--quiet", "--collect", "--", "cat", "/proc/self/cgroup"],
        ["journalctl", "--no-pager", "-q", "-n", "20", "-u", f"user@{uid}.service"],
    ):
        result = subprocess.run(command, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=30)
        print(f"orca-resource: $ {' '.join(command)} (exit {result.returncode})\n{result.stdout}", file=sys.stderr)


def check_boot_placement(uid, proc=Path("/proc"), runtime=Path("/srv/agents/orca/config/orca/daemon")):
    """The service must start after the user manager, or the daemon stays in the service."""
    deadline = time.monotonic() + 60
    boot_id = (proc / "sys/kernel/random/boot_id").read_text().strip()
    while time.monotonic() < deadline:
        for path in sorted(runtime.glob("daemon-v[0-9]*.pid")):
            try:
                record = json.loads(path.read_text())
            except (FileNotFoundError, json.JSONDecodeError):
                continue
            if not isinstance(record, dict) or record.get("bootId") != boot_id:
                continue
            pid = record.get("pid")
            if type(pid) is not int or pid <= 0:
                continue
            daemon = identity(pid, proc)
            if daemon is None or daemon["start"] != record.get("linuxStartTicks"):
                continue
            if time.monotonic() >= deadline:
                break
            assert re.search(r"/user@" + str(uid) + r"\.service/(?:[^/]+/)*orca-daemon-[^/]+\.scope$",
                             daemon["cgroup"]), f"daemon placed outside its scope after boot: {daemon}"
            print(f"orca-resource: lifetime boot: daemon={pid} in its scope")
            return
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            break
        time.sleep(min(0.5, remaining))
    raise AssertionError("Orca daemon did not publish a live PID record after boot")


def test_boot_placement():
    from tempfile import TemporaryDirectory
    from unittest import TestCase
    from unittest.mock import patch

    with TemporaryDirectory(dir="/tmp") as tmp:
        proc = Path(tmp) / "proc"
        boot_id = proc / "sys/kernel/random/boot_id"
        boot_id.parent.mkdir(parents=True)
        boot_id.write_text("current-boot\n")
        process = proc / "42"
        process.mkdir()
        stat = process / "stat"
        stat_prefix = "42 (systemd-run) S " + "1 " * 18
        stat.write_text(stat_prefix + "123\n")
        (process / "cmdline").write_bytes(b"systemd-run\0--scope\0/srv/agents/orca/daemon-entry\0")
        cgroup = process / "cgroup"
        service = "0::/system.slice/subyard-orca.service"
        scope = "0::/user.slice/user-1000.slice/user@1000.service/orca-daemon-test.scope"
        cgroup.write_text(service)
        pid_file = Path(tmp) / "daemon-v56.pid"

        def publish(_delay):
            cgroup.write_text(scope)
            pid_file.write_text(json.dumps({"pid": 42, "bootId": "current-boot", "linuxStartTicks": "123"}))

        with patch.object(time, "sleep", side_effect=publish) as sleep:
            check_boot_placement(1000, proc, Path(tmp))
            sleep.assert_called_once()
        cgroup.write_text(service)
        with patch.object(time, "sleep", side_effect=AssertionError("ready daemon was retried")):
            with TestCase().assertRaisesRegex(AssertionError, "daemon placed outside its scope.*system.slice"):
                check_boot_placement(1000, proc, Path(tmp))

        read_text = Path.read_text
        expect_error = TestCase().assertRaisesRegex
        timeout = "Orca daemon did not publish a live PID record"

        def reuse_pid(path):
            if path == cgroup:
                stat.write_text(stat_prefix + "124\n")
                cgroup.write_text(scope)
            return read_text(path)

        with patch.object(Path, "read_text", reuse_pid), \
                patch.object(time, "monotonic", side_effect=[0, 59.75, 59.75, 60]), \
                patch.object(time, "sleep") as sleep:
            with expect_error(AssertionError, timeout):
                check_boot_placement(1000, proc, Path(tmp))
            sleep.assert_called_once_with(0.25)

        stat.write_text(stat_prefix + "123\n")
        now = 0

        def slow_stat(path):
            nonlocal now
            if path == stat:
                now = 60.1
            return read_text(path)

        with patch.object(Path, "read_text", slow_stat), \
                patch.object(time, "monotonic", side_effect=lambda: now), \
                patch.object(time, "sleep") as sleep:
            with expect_error(AssertionError, timeout):
                check_boot_placement(1000, proc, Path(tmp))
            sleep.assert_not_called()


def main():
    phase = sys.argv[1]
    state = Path("/tmp/orca-lifetime-processes.json")
    uid = pwd.getpwnam("dev").pw_uid
    manager = f"user@{uid}.service"
    assert output("loginctl", "show-user", "dev", "--property=Linger", "--value") == "yes", "lingering disabled"
    if phase == "boot":
        check_boot_placement(uid)
        return
    if phase == "capture":
        deadline = time.monotonic() + 30
        pid_file = Path("/tmp/orca-lifetime-pty.pid")
        while not pid_file.exists() and time.monotonic() < deadline:
            time.sleep(0.1)
        pty = identity(int(pid_file.read_text()))
        assert pty is not None, "native PTY did not start"
        manager_path = r"/user@" + str(uid) + r"\.service/(?:[^/]+/)*"
        pty_scope = re.search(manager_path + r"(orca_pty_orca_daemon_[^/]+\.scope)$", pty["cgroup"])
        if not pty_scope:
            placement_diagnostics(uid, pty)
        assert pty_scope, "native PTY is outside the development user's Orca terminal scope"
        daemon = pty
        daemon_scope = None
        while daemon is not None and not daemon_scope:
            daemon = identity(daemon["parent"])
            daemon_scope = daemon and re.search(manager_path + r"(orca-daemon-[^/]+\.scope)$", daemon["cgroup"])
        if not daemon_scope:
            placement_diagnostics(uid, pty)
        assert daemon_scope, "native PTY has no ancestor in the development user's Orca daemon scope"
        manager_process = identity(int(output("systemctl", "show", manager, "--property=MainPID", "--value")))
        assert manager_process is not None, "user manager process unavailable"
        state.write_text(json.dumps({"daemon": daemon, "pty": pty, "manager": manager_process,
                                     "scopes": [daemon_scope[1], pty_scope[1]]}))
        state.chmod(0o600)
    saved = json.loads(state.read_text())
    if phase == "stopped":
        for name in ("daemon", "pty"):
            current = identity(saved[name]["pid"])
            assert current is None or current["start"] != saved[name]["start"], f"{name} survived down"
        assert not user_systemctl("list-units", "--no-legend", "--plain", "--type=scope,slice",
                                  "--state=active,activating,deactivating", "orca-daemon-*.scope",
                                  "orca_pty_orca_daemon_*.scope", "orca_ptys_orca_daemon_*.slice"), "unit survived down"
    else:
        for name in ("daemon", "pty", "manager"):
            current = identity(saved[name]["pid"])
            assert current is not None and all(current[key] == saved[name][key]
                                               for key in ("pid", "start", "cgroup")), f"{name} identity or cgroup changed"
        assert output("systemctl", "is-active", manager) == "active", "user manager stopped"
        for scope in saved["scopes"]:
            assert user_systemctl("is-active", scope) == "active", f"{scope} stopped"
    print(f"orca-resource: lifetime {phase}: daemon={saved['daemon']['pid']} "
          f"pty={saved['pty']['pid']} manager={saved['manager']['pid']} scopes={','.join(saved['scopes'])} linger=yes")


if __name__ == "__main__":
    if sys.argv[1:] == ["self-test"]:
        test_boot_placement()
    else:
        main()
