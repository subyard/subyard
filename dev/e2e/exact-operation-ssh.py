#!/usr/bin/env python3
"""Physical exact-operation cases on an already marker-owned SSH owner.

Usage: exact-operation-ssh.py fixture.json
Fixture: ssh (argv), command, arguments, probe (argv returning bounded native
state), optional drift (argv changing exactly the prepared target). No endpoint,
command payload, plan, native state or credential is printed as evidence.
The caller owns provisioning/cleanup and the agent-e2e source-hash envelope.
"""
import datetime
import json
import os
import select
import subprocess
import sys
import uuid

from contextlib import contextmanager

MAX_FRAME = 1024 * 1024


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def read_exact(stream, length):
    data = bytearray()
    while len(data) < length:
        require(select.select([stream], [], [], 60)[0], "owner frame timed out")
        part = os.read(stream.fileno(), length - len(data))
        require(part, "owner disconnected before complete frame")
        data.extend(part)
    return bytes(data)


def call(process, method, operation="", params=None):
    import struct
    request_id = uuid.uuid4().hex
    request = {"version": 1, "type": "request", "id": request_id,
               "method": method, "operationId": operation, "params": params or {},
               "deadline": (datetime.datetime.now(datetime.timezone.utc) +
                            datetime.timedelta(seconds=60)).isoformat()}
    data = json.dumps(request).encode()
    process.stdin.write(struct.pack(">I", len(data)) + data)
    process.stdin.flush()
    while True:
        size = struct.unpack(">I", read_exact(process.stdout, 4))[0]
        require(0 < size <= MAX_FRAME, "invalid owner frame length")
        response = json.loads(read_exact(process.stdout, size))
        require(response.get("version") == 1 and
                response.get("operationId", "") == operation,
                "uncorrelated owner response")
        if response.get("type") == "event":
            continue
        require(response.get("type") == "response" and response.get("id") == request_id,
                "uncorrelated owner response identifier")
        return response


@contextmanager
def session(argv):
    process = subprocess.Popen(argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                               stderr=subprocess.DEVNULL)
    try:
        response = call(process, "rpc.negotiate")
        capabilities = response.get("result", {}).get("capabilities", [])
        require(all(cap in capabilities for cap in
                    ("operation-exact-plan-v1", "operation-steps-v1")),
                "owner lacks exact executable-step capability")
        yield process
    finally:
        process.stdin.close()
        try:
            process.wait(timeout=10)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=5)
            raise RuntimeError("owner did not close its SSH session")


def error(response, code):
    require(response.get("error", {}).get("code") == code,
            "owner returned unexpected rejection category")


def probe(argv):
    result = subprocess.run(argv, capture_output=True, timeout=60)
    require(result.returncode == 0, "native state probe failed")
    require(len(result.stdout) <= MAX_FRAME, "native state probe exceeded bound")
    return result.stdout


def run(fixture):
    params = {"command": fixture["command"], "arguments": fixture["arguments"],
              "exact": True, "stepSchema": 1}
    if "source" in fixture:
        params["source"] = fixture["source"]
    if "export" in fixture:
        params["export"] = fixture["export"]

    def plan(process):
        operation = "physical-exact-" + uuid.uuid4().hex
        response = call(process, "operation.plan", operation, params)
        exact = response.get("result", {})
        require(exact.get("schema") == 1 and exact.get("stepSchema") == 1 and
                len(exact.get("digest", "")) == 64 and exact.get("expiresAt") and
                exact.get("plan", {}).get("steps"), "invalid executable owner plan")
        require(exact["plan"].get("operationId") == operation,
                "owner plan changed operation identity")
        if fixture.get("exportAdmission"):
            require(exact["plan"].get("confirmation") == "never",
                    "bounded owner export acquired a confirmation prompt")
            require(exact["plan"]["steps"][-1].get("target") == "controller:" + fixture["export"]["destination"],
                    "owner export plan changed the controller destination scope")
            require(subprocess.run(fixture["temporaryProbe"] + ["absent", operation],
                                   capture_output=True, timeout=60).returncode == 0,
                    "owner export preparation created a native temporary")
        return operation, exact["digest"]

    initial = probe(fixture["probe"])
    with session(fixture["ssh"]) as process:
        operation, digest = plan(process)
        require(probe(fixture["probe"]) == initial, "preparation mutated target state")
        error(call(process, "operation.execute", operation,
                   {"confirmed": False, "digest": digest}), "confirmation_required")
        require(probe(fixture["probe"]) == initial, "decline mutated target state")
        require("result" in call(process, "operation.discard", operation),
                "discard failed")
        error(call(process, "operation.execute", operation,
                   {"confirmed": True, "digest": digest}), "plan_not_found")
        operation, digest = plan(process)
        error(call(process, "operation.execute", operation,
                   {"confirmed": True, "digest": "0" * 64}), "plan_binding_invalid")
        error(call(process, "operation.execute", operation,
                   {"confirmed": True, "digest": digest}), "plan_not_found")
        disconnected_operation, disconnected_digest = plan(process)
    require(probe(fixture["probe"]) == initial, "rejected or disconnected plan mutated state")
    with session(fixture["ssh"]) as process:
        error(call(process, "operation.execute", disconnected_operation,
                   {"confirmed": True, "digest": disconnected_digest}), "plan_not_found")
        if "drift" in fixture:
            operation, digest = plan(process)
            require(subprocess.run(fixture["drift"], capture_output=True,
                                   timeout=60).returncode == 0, "native drift injection failed")
            drifted = probe(fixture["probe"])
            require(drifted != initial, "drift injection did not alter native target")
            error(call(process, "operation.execute", operation,
                       {"confirmed": True, "digest": digest}), "plan_stale")
            require(probe(fixture["probe"]) == drifted, "stale execution mutated target")
            if fixture.get("exportAdmission"):
                require(subprocess.run(fixture["temporaryProbe"] + ["absent", operation],
                                       capture_output=True, timeout=60).returncode == 0,
                        "stale export execution created a native temporary")
            error(call(process, "operation.execute", operation,
                       {"confirmed": True, "digest": digest}), "plan_not_found")
    if fixture.get("pinnedClone"):
        clone = fixture["pinnedClone"]
        with session(fixture["ssh"]) as process:
            operation, digest = plan(process)
            require(subprocess.run(clone["advance"], capture_output=True,
                                   timeout=60).returncode == 0, "native clone origin advance failed")
            response = call(process, "operation.execute", operation,
                            {"confirmed": True, "digest": digest})
            require(response.get("result", {}).get("result", {}).get("status") == "ok",
                    "approved pinned clone failed")
            require(subprocess.run(clone["verify"], capture_output=True,
                                   timeout=60).returncode == 0, "native cloned HEAD differs from approved revision")
            error(call(process, "operation.execute", operation,
                       {"confirmed": True, "digest": digest}), "plan_not_found")
        print("ok: physical SSH owner clone retains approved revision after native origin advances")
    if fixture.get("copyAdmission") or fixture.get("exportAdmission"):
        for ending in ("abort", "incomplete-finalize", "disconnect"):
            before = probe(fixture["probe"])
            with session(fixture["ssh"]) as process:
                operation, digest = plan(process)
                admitted = call(process, "operation.execute", operation,
                                {"confirmed": True, "digest": digest})
                require(admitted.get("result", {}).get("result", {}).get("status") == "ok",
                        "owner copy admission failed")
                if fixture.get("exportAdmission"):
                    temporary_identity = admitted["result"]["result"].get("output", {}).get("temporaryIdentity", "")
                    require(temporary_identity, "owner export admission lacks native temporary identity")
                    require(subprocess.run(fixture["temporaryProbe"] + ["present", operation, temporary_identity],
                                           capture_output=True, timeout=60).returncode == 0,
                            "owner export temporary native scope differs from admission")
                error(call(process, "operation.execute", operation,
                           {"confirmed": True, "digest": digest}), "plan_not_found")
                if ending == "abort":
                    require("result" in call(process, "project.copy.abort", operation),
                            "owner copy abort failed")
                elif ending == "incomplete-finalize":
                    error(call(process, "project.copy.finalize", operation,
                               {"digest": digest}), "project_copy_failed")
                if ending != "disconnect":
                    error(call(process, "project.copy.finalize", operation,
                               {"digest": digest}), "plan_not_found")
            if fixture.get("exportAdmission"):
                require(subprocess.run(fixture["temporaryProbe"] + ["absent", operation],
                                       capture_output=True, timeout=60).returncode == 0,
                        "owner export temporary survived abort, failed verification or disconnect")
            require(probe(fixture["probe"]) == before,
                    "untransferred work changed owner registry, workspace or controller source")
        print("ok: physical owner " + ("export" if fixture.get("exportAdmission") else "copy") +
              " single-use admission, abort, incomplete verification, disconnect cleanup")
    print("ok: physical SSH exact steps, read-only prepare, decline, tamper, replay, disconnect" +
          (", native target drift" if "drift" in fixture else ""))


if __name__ == "__main__":
    try:
        with open(sys.argv[1], encoding="utf-8") as source:
            run(json.load(source))
    except (RuntimeError, OSError, ValueError, subprocess.TimeoutExpired) as failure:
        # Do not include native subprocess output, endpoint or JSON payload in evidence.
        print("FAIL: physical SSH exact-operation contract: " +
              (str(failure) if isinstance(failure, RuntimeError) else type(failure).__name__),
              file=sys.stderr)
        sys.exit(1)
