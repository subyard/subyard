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


def identity(pid):
    try:
        fields = Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()
        return {"pid": int(pid), "start": fields[19], "parent": int(fields[1]),
                "cgroup": Path(f"/proc/{pid}/cgroup").read_text().strip()}
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


def check_boot_placement(uid):
    """The service must start after the user manager, or the daemon stays in the service."""
    deadline = time.monotonic() + 60
    while True:
        for path in Path("/proc").glob("[0-9]*/cmdline"):
            try:
                command = path.read_bytes()
            except OSError:
                continue
            if b"daemon-entry" in command and b"/srv/agents/orca/" in command:
                daemon = identity(path.parent.name)
                assert daemon and re.search(r"/user@" + str(uid) + r"\.service/(?:[^/]+/)*orca-daemon-[^/]+\.scope$",
                                            daemon["cgroup"]), f"daemon placed outside its scope after boot: {daemon}"
                print(f"orca-resource: lifetime boot: daemon={daemon['pid']} in its scope")
                return
        assert time.monotonic() < deadline, "Orca daemon did not start after boot"
        time.sleep(0.5)


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
    main()
