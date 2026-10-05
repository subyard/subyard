#!/usr/bin/env python3
"""Validate retained teardown identities; never print configuration or bindings."""
import datetime
import hashlib
import json
import os
import re
import subprocess
import sys
import urllib.parse


def inventory():
    value = os.environ.get("SUBYARD_TEARDOWN_INVENTORY", "")
    if len(value.encode()) > 65536:
        raise ValueError("oversized teardown inventory")
    resources = json.loads(value)
    if not isinstance(resources, list) or len(resources) > 256:
        raise ValueError("invalid teardown inventory")
    seen = set()
    for resource in resources:
        if set(resource) - {"kind", "name", "pool", "binding"}:
            raise ValueError("invalid teardown resource fields")
        if resource.get("kind") not in ("instance", "profile", "image", "volume", "network", "pool", "project"):
            raise ValueError("invalid teardown resource kind")
        for field in ("name", "pool"):
            value = resource.get(field, "")
            if (field == "name" or value) and not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]*", value):
                raise ValueError("invalid teardown resource identity")
        if not re.fullmatch(r"[a-f0-9]{64}", resource.get("binding", "")):
            raise ValueError("invalid teardown binding")
        key = (resource["kind"], resource.get("pool", ""), resource["name"])
        if key in seen:
            raise ValueError("duplicate teardown resource")
        seen.add(key)
    return resources


def query(path, project):
    return json.loads(subprocess.check_output(
        ["incus", "query", path + "?project=" + urllib.parse.quote(project, safe="")],
        stderr=subprocess.DEVNULL))


def guard(resource, project):
    kind, name = resource["kind"], resource["name"]
    if kind == "instance":
        path = "/1.0/instances/" + name
        fields = ("name", "type", "created_at", "config", "devices", "profiles")
    elif kind == "profile":
        path = "/1.0/profiles/" + name
        fields = ("name", "config", "devices", "description")
    elif kind == "volume":
        path = "/1.0/storage-pools/" + resource["pool"] + "/volumes/custom/" + name
        fields = ("name", "type", "content_type", "config", "description")
    elif kind == "project":
        path = "/1.0/projects/" + name
        fields = ("name", "config", "description")
        project = "default"
    elif kind == "network":
        path = "/1.0/networks/" + name
        fields = ("name", "type", "managed", "config", "description")
        project = "default"
    elif kind == "pool":
        path = "/1.0/storage-pools/" + name
        fields = ("name", "driver", "config", "description")
        project = "default"
    else:
        value = query("/1.0/images/" + name, project)["fingerprint"]
        fields = ()
    if fields:
        observed = query(path, project)
        value = {key: observed[key] for key in fields}
        if kind in ("instance", "volume"):
            value["snapshots"] = sorted(urllib.parse.unquote(urllib.parse.urlsplit(item).path.rsplit("/", 1)[-1])
                                        for item in query(path + "/snapshots", project))
        if kind == "instance":
            timestamp = value["created_at"]
            fraction = re.search(r"\.(\d+)", timestamp)
            stamp = datetime.datetime.fromisoformat(re.sub(r"\.\d+", "", timestamp).replace("Z", "+00:00"))
            suffix = fraction.group(1).rstrip("0") if fraction else ""
            value["created_at"] = stamp.astimezone(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%S") + ("." + suffix if suffix else "") + "Z"

    payload = json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
    payload = payload.replace("<", "\\u003c").replace(">", "\\u003e").replace("&", "\\u0026").replace("\u2028", "\\u2028").replace("\u2029", "\\u2029")
    if hashlib.sha256(payload.encode()).hexdigest() != resource["binding"]:
        raise ValueError("teardown resource changed after confirmation")


def main():
    resources = inventory()
    mode = sys.argv[1]
    if mode == "validate":
        return
    if mode == "guard-all":
        project = sys.argv[2]
        if project in ("", "default"):
            raise ValueError("teardown requires a dedicated project")
        for resource in resources:
            guard(resource, project)
        return
    if mode == "volumes":
        for resource in resources:
            if resource["kind"] == "volume":
                print(resource["pool"] + "\t" + resource["name"])
        return
    kind = sys.argv[2]
    if mode == "list":
        for resource in resources:
            if resource["kind"] == kind:
                print(resource["name"])
        return
    if mode == "guard":
        name, project = sys.argv[3:5]
        pool = sys.argv[5] if len(sys.argv) > 5 else ""
        if project in ("", "default"):
            raise ValueError("teardown requires a dedicated project")
        matches = [item for item in resources if item["kind"] == kind and item["name"] == name and item.get("pool", "") == pool]
        if len(matches) != 1:
            raise ValueError("teardown resource was not approved")
        guard(matches[0], project)
        return
    raise ValueError("invalid teardown guard mode")


if __name__ == "__main__":
    try:
        main()
    except (ValueError, KeyError, TypeError, OSError, subprocess.CalledProcessError):
        sys.exit("plan_stale: teardown inventory or resource binding is invalid")
