#!/usr/bin/env python3
"""Bounded owner adapter for the AI Observer package and dashboard receipts."""
import base64
import json
import os
from pathlib import Path
import subprocess
import sys
import time


ROOT = Path(__file__).absolute().parent
ENV = os.environ
_producer_run = subprocess.run
PROJECT = ENV["INCUS_PROJECT"]
INSTANCE = ENV["YARD_INSTANCE_NAME"]
SELECTED = ENV["SUBYARD_INTEGRATION_SELECTED"] == "1"
PROVISION = "user.subyard.ai_observer_provision"
PROXY = "user.subyard.ai_observer_proxy"
DIAGNOSTIC = "subyard-lifecycle-diagnostic-v1 "
READINESS_DIAGNOSTIC = "subyard-readiness-diagnostic-v1 "
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


def probe(stage, budget, function, *arguments, **options):
    started = time.monotonic()
    try:
        return function(*arguments, **options)
    except (KeyError, ValueError, RuntimeError, OSError, subprocess.SubprocessError) as error:
        if not hasattr(error, "readiness_diagnostic"):
            category = ("transport-timeout" if isinstance(error, subprocess.TimeoutExpired) else
                        "os-error" if isinstance(error, OSError) else
                        getattr(error, "readiness_category", "native-exit") if isinstance(error, Failure) else "invalid-schema")
            error.readiness_diagnostic = (stage, category, min(1320000, max(0, int((time.monotonic() - started) * 1000))),
                                          min(1320000, max(0, int(budget * 1000))))
        raise


def readiness_failure(stage, category, started, budget):
    error = Failure("native-unavailable")
    error.readiness_diagnostic = (stage, category, min(1320000, max(0, int((time.monotonic() - started) * 1000))), int(budget * 1000))
    return error


def run(*arguments, input=None, check=True, timeout=20, failure="native-unavailable"):
    result = subprocess.run(arguments, input=input, stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE, timeout=timeout)
    if check and result.returncode:
        error = Failure(failure)
        error.readiness_category = "native-timeout" if result.returncode == 124 else "native-exit"
        raise error
    return result


def guest(*arguments, **options):
    return run("incus", "exec", INSTANCE, "--project", PROJECT, "--", *arguments, **options)


def wait_for_docker_startup():
    # Recover only when systemd explicitly reports startup or its completion.
    # Keep one deadline; failed/inactive or unknown state is not absence.
    started = time.monotonic()
    deadline = started + 60
    while True:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise readiness_failure("service-window", "startup-deadline", started, 60)
        state = probe("service-state", min(20, remaining), guest, "timeout", str(min(10, remaining)), "systemctl", "show", "docker.service",
                      "--property=ActiveState", "--value", timeout=min(20, remaining)).stdout.decode(errors="replace").strip()
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise readiness_failure("service-window", "startup-deadline", started, 60)
        if state == "active":
            return deadline
        if state != "activating":
            category = "service-" + state if state in ("inactive", "failed", "deactivating") else "service-unknown"
            raise readiness_failure("service-window", category, started, 60)
        time.sleep(min(1, remaining))


def native_container_json():
    # Enumeration distinguishes absence from daemon/inspection failure. Ownership
    # is checked even when no managed service files or receipt exist.
    arguments = ("timeout", "10", "docker", "container", "ls", "-a", "--format", "{{.Names}}")
    result = probe("enumeration", 20, guest, *arguments, check=False)
    if result.returncode:
        deadline = wait_for_docker_startup()
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise readiness_failure("service-window", "startup-deadline", deadline - 60, 60)
        result = probe("recovery-enumeration", min(20, remaining), guest, "timeout", str(min(10, remaining)), *arguments[2:], timeout=min(20, remaining))
        if time.monotonic() >= deadline:
            raise readiness_failure("service-window", "startup-deadline", deadline - 60, 60)
    names = result.stdout.decode().splitlines()
    if "subyard-ai-observer" not in names:
        return "null"
    return probe("inspection", 20, guest, "timeout", "10", "docker", "container", "inspect", "subyard-ai-observer").stdout.decode()


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
        error = Failure("native-unavailable")
        error.readiness_category = "dependency-missing"
        raise error
    encoded = payload.encode() if isinstance(payload, str) else json.dumps(payload).encode()
    result = _producer_run([str(helper), action], input=encoded, stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE, timeout=10)
    if result.returncode:
        record = result.stderr.decode(errors="replace").strip()
        reason = record.removeprefix(DIAGNOSTIC) if record.startswith(DIAGNOSTIC) else "invalid-schema"
        error = Failure(reason if reason in REASONS else "invalid-schema")
        error.readiness_category = "native-rejected"
        raise error
    return json.loads(result.stdout)


def native_container():
    return probe("native-producer", 10, produce, "container", native_container_json())


def observe():
    snapshot = probe("instance-query", 20, run, "incus", "query", f"/1.0/instances/{INSTANCE}?project={PROJECT}").stdout.decode()
    owner_host = "127.0.0.1"
    if SELECTED and ENV.get("YARD_KIND", "container") != "vm":
        owner_host = probe("owner-address", 20, run, "bash", "-c", '. "$SUBYARD_REPOSITORY_ROOT/scripts/lib/owner-address.sh"; subyard_owner_host').stdout.decode().strip()
    container = native_container_json()
    # Ownership assessment precedes every retirement and also protects selected recovery.
    service = probe("ownership", 20, guest, "sh", "-eu", "-c", CONTROL_ASSESSMENT, failure="ambiguous-ownership").stdout.decode()
    installation = probe("installation", 95, guest, "/usr/local/bin/ai-observer-installed", check=False, timeout=95).returncode == 0 if SELECTED else False
    keys = ("DEV_USER", "DEV_UID", "YARD_KIND", "CODING_TOOL_INTEGRATIONS", "AI_OBSERVER_HOST_PORT", "HOST_BASE", "HOST_LINKS", "HOST_MOUNTS", "SUBYARD_INTEGRATION_SOURCE_BINDING")
    result = probe("native-producer", 10, produce, "observe", {"snapshot": snapshot, "container": container, "service": service,
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
        if hasattr(error, "readiness_diagnostic"):
            print(READINESS_DIAGNOSTIC + PHASE + " " + " ".join(map(str, error.readiness_diagnostic)), file=sys.stderr)
        sys.exit(1)
