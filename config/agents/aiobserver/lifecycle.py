#!/usr/bin/env python3
"""Bounded owner adapter for the AI Observer package and dashboard receipts."""
import base64
import json
import os
from pathlib import Path
import subprocess
import sys


ROOT = Path(__file__).absolute().parent
ENV = os.environ
_producer_run = subprocess.run
PROJECT = ENV["INCUS_PROJECT"]
INSTANCE = ENV["YARD_INSTANCE_NAME"]
SELECTED = ENV["SUBYARD_INTEGRATION_SELECTED"] == "1"
PROVISION = "user.subyard.ai_observer_provision"
PROXY = "user.subyard.ai_observer_proxy"
DIAGNOSTIC = "subyard-lifecycle-diagnostic-v1 "
REASONS = {"native-unavailable", "invalid-schema", "ambiguous-ownership", "foreign-ownership", "observation-changed", "convergence-failed", "operation-failed"}
PHASE = "inspect"


class Failure(RuntimeError):
    def __init__(self, reason):
        self.reason = reason
        super().__init__(reason)

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


def run(*arguments, input=None, check=True, timeout=20, failure="native-unavailable"):
    result = subprocess.run(arguments, input=input, stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE, timeout=timeout)
    if check and result.returncode:
        raise Failure(failure)
    return result


def guest(*arguments, **options):
    return run("incus", "exec", INSTANCE, "--project", PROJECT, "--", *arguments, **options)


def native_container_json():
    # Enumeration distinguishes absence from daemon/inspection failure. Ownership
    # is checked even when no managed service files or receipt exist.
    names = guest("timeout", "10", "docker", "container", "ls", "-a", "--format", "{{.Names}}").stdout.decode().splitlines()
    if "subyard-ai-observer" not in names:
        return "null"
    return guest("timeout", "10", "docker", "container", "inspect", "subyard-ai-observer").stdout.decode()


def produce(action, payload):
    # Core supplies the exact validated, byte-bound helper root. Direct package
    # tests and plain development copies use the same namespace without Git.
    native_root = ENV.get("SUBYARD_INTEGRATION_NATIVE_ROOT")
    if native_root:
        helper = Path(native_root) / "bin/observation"
    else:
        helper = ROOT / "bin/observation"
        if not helper.exists() and not helper.is_symlink() and (ROOT.parents[2] / "go.mod").is_file():
            helper = ROOT.parents[2] / ".build/agents/aiobserver/bin/observation"
    if not helper.is_file() or helper.is_symlink():
        raise Failure("native-unavailable")
    encoded = payload.encode() if isinstance(payload, str) else json.dumps(payload).encode()
    result = _producer_run([str(helper), action], input=encoded, stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE, timeout=10)
    if result.returncode:
        record = result.stderr.decode(errors="replace").strip()
        reason = record.removeprefix(DIAGNOSTIC) if record.startswith(DIAGNOSTIC) else "invalid-schema"
        raise Failure(reason if reason in REASONS else "invalid-schema")
    return json.loads(result.stdout)


def native_container():
    return produce("container", native_container_json())


def observe():
    snapshot = run("incus", "query", f"/1.0/instances/{INSTANCE}?project={PROJECT}").stdout.decode()
    owner_host = "127.0.0.1"
    if SELECTED and ENV.get("YARD_KIND", "container") != "vm":
        owner_host = run("bash", "-c", '. "$SUBYARD_REPOSITORY_ROOT/scripts/lib/owner-address.sh"; subyard_owner_host').stdout.decode().strip()
    container = native_container_json()
    # Ownership assessment precedes every retirement and also protects selected recovery.
    service = guest("sh", "-eu", "-c", CONTROL_ASSESSMENT, failure="ambiguous-ownership").stdout.decode()
    installation = guest("/usr/local/bin/ai-observer-installed", check=False, timeout=95).returncode == 0 if SELECTED else False
    keys = ("DEV_USER", "DEV_UID", "YARD_KIND", "CODING_TOOL_INTEGRATIONS", "AI_OBSERVER_HOST_PORT", "HOST_BASE", "HOST_LINKS", "HOST_MOUNTS", "SUBYARD_INTEGRATION_SOURCE_BINDING")
    result = produce("observe", {"snapshot": snapshot, "container": container, "service": service,
                                "installed": installation, "selected": SELECTED, "owner_host": owner_host,
                                "environment": {key: ENV[key] for key in keys if key in ENV},
                                "provision": base64.b64encode(Path(ENV.get("AGENT_aiobserver_PROVISION", str(ROOT / "provision.sh"))).read_bytes()).decode()})
    return result["observation"], result["context"], result["frontend"], result["container"]


def main():
    global PHASE
    PHASE = "apply" if sys.argv[1:2] == ["apply"] else "inspect"
    observation, context, frontend, assessed_container = observe()
    if sys.argv[1:] == ["observe"]:
        print(json.dumps(observation)); return
    if len(sys.argv) != 5 or sys.argv[1] != "apply":
        raise RuntimeError("invalid lifecycle arguments")
    if observation["state"] != "stale" or observation["actual"] != sys.argv[3] or observation["desired"] != sys.argv[4]:
        raise Failure("observation-changed")
    environment = dict(ENV, AI_OBSERVER_CONTEXT=context, AI_OBSERVER_FRONTEND_URL=frontend)
    if SELECTED:
        run("incus", "config", "set", INSTANCE, PROVISION, "pending", "--project", PROJECT, failure="operation-failed")
        arguments = ["incus", "exec", INSTANCE, "--project", PROJECT]
        for name in ("DEV_USER", "CODING_TOOL_INTEGRATIONS", "AI_OBSERVER_CONTEXT", "AI_OBSERVER_FRONTEND_URL", "AI_OBSERVER_IMAGE", "AI_OBSERVER_VERSION", "YARD_VERSION"):
            arguments += ["--env", f"{name}={environment.get(name, '')}"]
        run(*arguments, "--", "bash", "-euo", "pipefail", "-s", input=Path(ENV.get("AGENT_aiobserver_PROVISION", str(ROOT / "provision.sh"))).read_bytes(), timeout=1200, failure="operation-failed")
        guest("timeout", "90", "ai-observer-installed", timeout=95, failure="convergence-failed")
    else:
        guest("sh", "-eu", "-c", 'if [ -e /etc/subyard/ai-observer/managed ] || [ -e /etc/systemd/system/subyard-ai-observer.service ]; then /usr/local/bin/ai-observer disable; fi', failure="operation-failed")
        # Older owned wrappers only disable systemd. An independently started
        # container must also retire, without upgrading the deselected package.
        container = native_container()
        if container is not None:
            if assessed_container is None or container["id"] != assessed_container["id"]:
                raise Failure("observation-changed")
            if container["running"]:
                guest("timeout", "20", "docker", "stop", "--time", "10", container["id"], timeout=25, failure="operation-failed")
    result = subprocess.run(["bash", "-euo", "pipefail", "-c", '. "$1/proxy.sh"; PROJ=(--project "$INCUS_PROJECT"); subyard_ai_observer_proxy "$SUBYARD_INTEGRATION_SELECTED"; subyard_ai_observer_provision_marker "$SUBYARD_INTEGRATION_SELECTED" "$AI_OBSERVER_CONTEXT"', "subyard", str(ROOT)], env=environment, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=60)
    if result.returncode:
        raise Failure("operation-failed")
    PHASE = "verify"
    verified, _, _, _ = observe()
    if verified["state"] not in ("current", "absent") or verified["state"] == "current" and verified["desired"] != sys.argv[4]:
        raise Failure("convergence-failed")
    print(json.dumps(verified))


if __name__ == "__main__":
    try:
        main()
    except (KeyError, ValueError, RuntimeError, OSError, subprocess.SubprocessError) as error:
        reason = error.reason if isinstance(error, Failure) else "native-unavailable" if isinstance(error, (OSError, subprocess.SubprocessError)) else "invalid-schema"
        print(DIAGNOSTIC + PHASE + " " + reason, file=sys.stderr)
        sys.exit(1)
