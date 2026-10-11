#!/usr/bin/env python3
"""Bounded owner adapter for the AI Observer package and dashboard receipts."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys


ROOT = Path(__file__).absolute().parent
ENV = os.environ
PROJECT = ENV["INCUS_PROJECT"]
INSTANCE = ENV["YARD_INSTANCE_NAME"]
SELECTED = ENV["SUBYARD_INTEGRATION_SELECTED"] == "1"
PROVISION = "user.subyard.ai_observer_provision"
PROXY = "user.subyard.ai_observer_proxy"

# Observe controls directly: published wrappers predate assess-disable. Keep
# their ownership proof without relying on installed wrapper capabilities.
CONTROL_ASSESSMENT = r'''
wrapper=/usr/local/bin/ai-observer
marker=/etc/subyard/ai-observer/managed
unit=/etc/systemd/system/subyard-ai-observer.service
check=/usr/local/bin/ai-observer-check
installed=/usr/local/bin/ai-observer-installed
health=/usr/local/bin/ai-observer-health
managed='# Managed by Subyard AI Observer provision.'
present() { [ -e "$1" ] || [ -L "$1" ]; }
verify() {
  [ -f "$1" ] && [ ! -L "$1" ]
  [ "$(stat -c '%u:%g' "$1")" = '0:0' ]
  [ "$(stat -c '%a' "$1")" = "$2" ]
  grep -Fxq "$3" "$1"
  sha256sum "$1"
}
if present "$wrapper" || present "$marker" || present "$unit" || present "$check" || present "$installed" || present "$health"; then
  verify "$wrapper" 755 "$managed"
  verify "$marker" 644 subyard-ai-observer-v1
  verify "$unit" 644 "$managed"
  verify "$check" 755 "$managed"
  # Current wrappers declare both additional controls. A partial current set
  # must not be mistaken for a valid legacy installation.
  if present "$installed" || present "$health" || grep -q '^installed_path=' "$wrapper" || grep -q '^health_path=' "$wrapper"; then
    verify "$installed" 755 "$managed"
    verify "$health" 755 "$managed"
  fi
  if systemctl is-active --quiet subyard-ai-observer.service; then echo active; fi
  if systemctl is-enabled --quiet subyard-ai-observer.service; then echo enabled; fi
fi
'''


def digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


def run(*arguments, input=None, check=True, timeout=20):
    result = subprocess.run(arguments, input=input, stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE, timeout=timeout)
    if check and result.returncode:
        raise RuntimeError("AI Observer native ownership or operation failed")
    return result


def guest(*arguments, **options):
    return run("incus", "exec", INSTANCE, "--project", PROJECT, "--", *arguments, **options)


def native_container():
    # Enumeration distinguishes absence from daemon/inspection failure. Ownership
    # is checked even when no managed service files or receipt exist.
    names = guest("timeout", "10", "docker", "container", "ls", "-a", "--format", "{{.Names}}").stdout.decode().splitlines()
    if "subyard-ai-observer" not in names:
        return None
    containers = json.loads(guest("timeout", "10", "docker", "container", "inspect", "subyard-ai-observer").stdout)
    if not isinstance(containers, list) or len(containers) != 1 or not isinstance(containers[0], dict):
        raise RuntimeError("AI Observer container assessment is ambiguous; container preserved")
    container = containers[0]
    config, host_config, state = container.get("Config"), container.get("HostConfig"), container.get("State")
    if container.get("Name") != "/subyard-ai-observer" or not isinstance(config, dict) or not isinstance(host_config, dict) or not isinstance(state, dict) or not isinstance(state.get("Running"), bool):
        raise RuntimeError("AI Observer container assessment is invalid; container preserved")
    if any(not isinstance(container.get(key), str) or not container[key] for key in ("Id", "Image")) or not isinstance(container.get("Mounts"), list):
        raise RuntimeError("AI Observer container identity is unavailable; container preserved")
    labels = config.get("Labels")
    if not isinstance(labels, dict) or labels.get("org.subyard.managed") != "ai-observer-v1":
        raise RuntimeError("AI Observer same-name container is foreign; container preserved")
    return {"id": container.get("Id"), "image": container.get("Image"),
            "config": {key: config.get(key) for key in ("Image", "User", "Cmd", "Env", "Labels", "Entrypoint", "WorkingDir", "Volumes", "ExposedPorts")},
            "host_config": host_config, "mounts": container.get("Mounts"),
            "running": state["Running"], "status": state.get("Status")}


def observe():
    snapshot = json.loads(run("incus", "query", f"/1.0/instances/{INSTANCE}?project={PROJECT}").stdout)
    local_config, local_devices = snapshot.get("config", {}), snapshot.get("devices", {})
    config = snapshot.get("expanded_config", local_config)
    devices = snapshot.get("expanded_devices", local_devices)
    if config.get(PROVISION, "") != local_config.get(PROVISION, ""):
        raise RuntimeError("AI Observer inherited provision ownership is unknown; receipt preserved")
    proxy, marker = devices.get("ai-observer"), config.get(PROXY, "")
    if proxy != local_devices.get("ai-observer") or marker != local_config.get(PROXY, ""):
        raise RuntimeError("AI Observer inherited route ownership is unknown; route preserved")
    host, port, ready = "", "", False
    if proxy is not None:
        version, separator, value = marker.partition(":")
        pending = value.startswith("pending:")
        value = value.removeprefix("pending:")
        if version == "v1":
            host, port = "127.0.0.1", value
        elif version == "v2" and ":" in value:
            host, port = value.split(":", 1)
            octets = host.split(".")
            if len(octets) != 4 or any(not x.isdigit() or str(int(x)) != x or int(x) > 255 for x in octets) or octets[0] != "100" or not 64 <= int(octets[1]) <= 127:
                raise RuntimeError("AI Observer route receipt is invalid; route preserved")
        else:
            raise RuntimeError("AI Observer route ownership is unknown; route preserved")
        if not separator or not port.isdigit() or str(int(port)) != port or not 1024 <= int(port) <= 65535 or proxy != {"type": "proxy", "bind": "host", "listen": f"tcp:{host}:{port}", "connect": "tcp:127.0.0.1:8080"}:
            raise RuntimeError("AI Observer route ownership is changed; route preserved")
        ready = not pending
    frontend_host = "127.0.0.1"
    if SELECTED and ENV.get("YARD_KIND", "container") != "vm":
        frontend_host = run("bash", "-c", '. "$SUBYARD_REPOSITORY_ROOT/scripts/lib/owner-address.sh"; subyard_owner_host').stdout.decode().strip()
    frontend = f"http://{frontend_host}:{ENV['AI_OBSERVER_HOST_PORT']}" if SELECTED else ""
    integrations = ENV.get("CODING_TOOL_INTEGRATIONS", "").split()
    fields = ["v1", hashlib.sha256(Path(ENV.get("AGENT_aiobserver_PROVISION", str(ROOT / "provision.sh"))).read_bytes()).hexdigest(), ENV.get("DEV_USER", "dev"), ENV.get("DEV_UID", "1000"), frontend]
    fields += [str(source in integrations).lower() for source in ("claude", "codex")]
    fields += [ENV.get(name, "") for name in ("HOST_BASE", "HOST_LINKS", "HOST_MOUNTS")]
    fields += [ENV["SUBYARD_INTEGRATION_SOURCE_BINDING"]]
    context = hashlib.sha256(("\0".join(fields) + "\0").encode()).hexdigest() if SELECTED else ""
    container = native_container()
    # Ownership assessment precedes every retirement and also protects selected recovery.
    service = guest("sh", "-eu", "-c", CONTROL_ASSESSMENT).stdout.decode()
    if container is not None and not service:
        raise RuntimeError("AI Observer same-name container has no managed controls; container preserved")
    installation = guest("/usr/local/bin/ai-observer-installed", check=False, timeout=95).returncode == 0 if SELECTED else False
    desired = digest({"selected": SELECTED, "context": context, "frontend": frontend, "source": ENV["SUBYARD_INTEGRATION_SOURCE_BINDING"]})
    if SELECTED:
        route_current = not marker and proxy is None if ENV.get("YARD_KIND") == "vm" else ready and frontend == f"http://{host}:{port}"
        current = installation and config.get(PROVISION, "") == context and route_current
    else:
        current = not marker and proxy is None and not config.get(PROVISION, "") and "\nactive\n" not in "\n" + service and "\nenabled\n" not in "\n" + service and (container is None or not container["running"])
        if current and not service:
            return {"state": "absent", "actual": "", "desired": ""}, context, frontend, container
    actual = desired if current else digest({"provision": config.get(PROVISION, ""), "proxy": marker, "device": proxy, "service": service, "installation": installation, "container": container})
    return {"state": "current" if current else "stale", "actual": actual, "desired": desired}, context, frontend, container


def main():
    observation, context, frontend, assessed_container = observe()
    if sys.argv[1:] == ["observe"]:
        print(json.dumps(observation)); return
    if len(sys.argv) != 5 or sys.argv[1] != "apply":
        raise RuntimeError("invalid lifecycle arguments")
    if observation["state"] != "stale" or observation["actual"] != sys.argv[3] or observation["desired"] != sys.argv[4]:
        raise RuntimeError("AI Observer lifecycle assessment changed before apply")
    environment = dict(ENV, AI_OBSERVER_CONTEXT=context, AI_OBSERVER_FRONTEND_URL=frontend)
    if SELECTED:
        run("incus", "config", "set", INSTANCE, PROVISION, "pending", "--project", PROJECT)
        arguments = ["incus", "exec", INSTANCE, "--project", PROJECT]
        for name in ("DEV_USER", "CODING_TOOL_INTEGRATIONS", "AI_OBSERVER_CONTEXT", "AI_OBSERVER_FRONTEND_URL", "AI_OBSERVER_IMAGE", "AI_OBSERVER_VERSION", "YARD_VERSION"):
            arguments += ["--env", f"{name}={environment.get(name, '')}"]
        result = run(*arguments, "--", "bash", "-euo", "pipefail", "-s", input=Path(ENV.get("AGENT_aiobserver_PROVISION", str(ROOT / "provision.sh"))).read_bytes(), timeout=1200)
        sys.stderr.buffer.write(result.stdout); sys.stderr.buffer.write(result.stderr)
        guest("timeout", "90", "ai-observer-installed", timeout=95)
    else:
        guest("sh", "-eu", "-c", 'if [ -e /etc/subyard/ai-observer/managed ] || [ -e /etc/systemd/system/subyard-ai-observer.service ]; then /usr/local/bin/ai-observer disable; fi')
        # Older owned wrappers only disable systemd. An independently started
        # container must also retire, without upgrading the deselected package.
        container = native_container()
        if container is not None:
            if assessed_container is None or container["id"] != assessed_container["id"]:
                raise RuntimeError("AI Observer container identity changed during retirement; container preserved")
            if container["running"]:
                guest("timeout", "20", "docker", "stop", "--time", "10", container["id"], timeout=25)
    result = subprocess.run(["bash", "-euo", "pipefail", "-c", '. "$1/proxy.sh"; PROJ=(--project "$INCUS_PROJECT"); subyard_ai_observer_proxy "$SUBYARD_INTEGRATION_SELECTED"; subyard_ai_observer_provision_marker "$SUBYARD_INTEGRATION_SELECTED" "$AI_OBSERVER_CONTEXT"', "subyard", str(ROOT)], env=environment, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=60)
    sys.stderr.buffer.write(result.stdout); sys.stderr.buffer.write(result.stderr)
    if result.returncode:
        raise RuntimeError("AI Observer publication failed")
    verified, _, _, _ = observe()
    if verified["state"] not in ("current", "absent") or verified["state"] == "current" and verified["desired"] != sys.argv[4]:
        raise RuntimeError("AI Observer lifecycle did not converge")
    print(json.dumps(verified))


if __name__ == "__main__":
    try:
        main()
    except (KeyError, ValueError, RuntimeError, subprocess.SubprocessError) as error:
        print(str(error), file=sys.stderr)
        sys.exit(1)
