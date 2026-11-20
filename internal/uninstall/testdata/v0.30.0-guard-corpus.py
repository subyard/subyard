#!/usr/bin/env python3
"""Regenerate v0.30.0-guard-corpus.jsonl from the released helper.

Usage: git show v0.30.0:scripts/lib/teardown-plan.py > helper.py
       python3 -I v0.30.0-guard-corpus.py helper.py WORKDIR > v0.30.0-guard-corpus.jsonl
Read-only wire modes only. "engine" marks inputs a typed engine marshal can emit.
"""
import json, os, re, subprocess, sys

helper, work = sys.argv[1:3]
H, ABSENT = "a" * 64, object()
tools = os.path.join(work, "tools")
os.makedirs(tools, exist_ok=True)
py = os.path.join(tools, "python3")
if not os.path.lexists(py):
    os.symlink(sys.executable, py)
missing = os.path.join(work, "absent", "leaf")


def variants(base, field, values):
    for value in values:
        item = dict(base)
        item.pop(field, None) if value is ABSENT else item.__setitem__(field, value)
        yield item


cases = []
r = {"kind": "instance", "name": "yard", "binding": H}
v = {"kind": "volume", "name": "vol", "pool": "pool0", "binding": H}
hp = {"kind": "host-profile", "name": "default", "binding": H}
res = []
res += variants(r, "kind", ["instance", "profile", "image", "volume", "network", "pool", "project", "host-profile", "bucket", "", None, 7, ABSENT, ["instance"]])
res += variants(r, "name", [ABSENT, None, "", 7, 7.5, True, ["x"], {"a": 1}, "bad name", "-x", "a" * 300, "ok.name_1-2"])
res += variants(v, "pool", [ABSENT, None, "", 7, False, [], {}, "bad pool", "-p", "ok"])
res += variants(r, "pool", [ABSENT, None, "", 7, "ok"])
res += variants(r, "binding", [ABSENT, None, "", 7, "abc", "A" * 64, H + "0", [H]])
res += variants(r, "foo", [1])
res += variants(r, "path", [None, "", "/x", 7, []])
res += variants(r, "usedBy", [None, [], ["a"], "a", 7, [7]])
res += variants(r, "owner", [None, {}, {"UID": "1"}, 7, "x", []])
res += variants(hp, "bindings", [ABSENT, None, [], [H], [H] * 3, [H] * 4, ["x"], "x", 7, [7], {}])
inventories = [json.dumps([x]) for x in res] + [json.dumps(x) for x in (
    None, {}, 7, "x", [], [7], [None], ["x"], [[]], [{}], [r, r], [v, dict(v)], [dict(r, pool=None), r],
    [dict(r, pool=""), r], [r, v])] + ["", "{", "[] x", " [] ", '[{"kind":"instance","name":"a","name":"b","binding":"%s"}]' % H,
    '[{"kind":"instance","name":"a","binding":"%s","path":NaN}]' % H, '[{"kind":"instance","name":"\\u00e9","binding":"%s"}]' % H,
    '[{"kind":"instance","name":"a\\n","binding":"%s"}]' % H]
for value in inventories + [None]:
    for mode in (["validate"], ["volumes"], ["list", "instance"], ["list", "volume"], ["guard-all", "subyard"], ["guard-all", "default"],
                 ["guard", "instance", "yard", "subyard"], ["guard", "volume", "vol", "subyard", "pool0"], ["remove-nothing-such-mode"]):
        cases.append((mode, {} if value is None else {"SUBYARD_TEARDOWN_INVENTORY": value}))

a = {"Path": missing, "Binding": ""}
items = []
items += variants(a, "Path", [ABSENT, None, 7, "", "relative", "/", "/a/../b", "/a/", "//a", "/a/./b", ["/a"]])
items += variants(a, "Binding", [ABSENT, None, 7, "abc", "A" * 64, H, [H]])
items += variants(a, "Entries", [ABSENT, None, [], "x", 7, {}, [7], [{}]])
items += variants(a, "Scoped", [True, False])
items += variants(a, "foo", [1])
artifacts = [json.dumps([x]) for x in items] + [json.dumps(x) for x in (None, {}, 7, "x", [], [7], [None], ["x"], [[]], [{}], [a, a])] + [
    "", "{", '[{"Path":"/x/a","Path":"%s","Binding":""}]' % missing]
for value in artifacts + [None]:
    for mode in (["guard-artifacts"], ["guard-artifact", missing], ["guard-artifact", "/not/approved"], ["service-fragment", missing]):
        cases.append((mode, {} if value is None else {"SUBYARD_TEARDOWN_ARTIFACTS": value}))

e = dict(a, Entries=None)
s = {"scope": "system", "units": ["subyard-a.service"], "Artifacts": [e]}
m = {"scope": "system", "units": [], "Artifacts": None}
svcs = []
svcs += variants(s, "scope", [ABSENT, None, 7, "user", "bad", ""])
svcs += variants(s, "units", [ABSENT, None, 7, "subyard-a.service", [], ["a.service"] * 4, ["a.service"] * 5, ["bad"], [7], [None], ["a.timer", "b.socket"], ["-a.service"]])
svcs += variants(s, "Artifacts", [ABSENT, None, [], 7, "x", {}, [a], [e, e], [e, dict(e, Binding=H)], [dict(e, Scoped=True)], [7], [{}], [dict(e, Path=7)]])
svcs += variants(s, "foo", [1])
services = [json.dumps([x]) for x in svcs] + [json.dumps(x) for x in (None, {}, 7, [], [7], [None], [s, dict(s, Artifacts=[dict(e, Binding=H)])], [s, s], [m] * 129, [m] * 128)] + ["", "{"]
for value in services + [None]:
    for mode in (["service-inventory"], ["producer-artifacts"], ["producer-artifacts-json"]):
        cases.append((mode, {} if value is None else {"SUBYARD_TEARDOWN_SERVICES": value}))

HEX, IDENT = re.compile(r"[a-f0-9]{64}$"), re.compile(r"[A-Za-z0-9][A-Za-z0-9_.-]*$")


def resource(x):
    if not isinstance(x, dict) or set(x) - {"kind", "name", "pool", "binding", "path", "usedBy", "bindings", "owner"}:
        return False
    if x.get("kind") not in ("instance", "profile", "image", "volume", "network", "pool", "project", "host-profile"):
        return False
    if not isinstance(x.get("name"), str) or not IDENT.match(x["name"]) or not isinstance(x.get("binding"), str) or not HEX.match(x["binding"]):
        return False
    if "pool" in x and not (isinstance(x["pool"], str) and IDENT.match(x["pool"])) or x["kind"] == "volume" and "pool" not in x:
        return False
    if "path" in x and not (isinstance(x["path"], str) and x["path"]) or "owner" in x and not isinstance(x["owner"], dict):
        return False
    lists = {"usedBy": lambda y: isinstance(y, str), "bindings": lambda y: isinstance(y, str) and HEX.match(y)}
    return all(k not in x or isinstance(x[k], list) and x[k] and all(f(y) for y in x[k]) for k, f in lists.items())


def artifact(x, entries=False):
    if not isinstance(x, dict) or set(x) not in ({"Path", "Binding", "Entries"},) + (() if entries else ({"Path", "Binding"},)):
        return False
    p, b = x["Path"], x["Binding"]
    return (isinstance(p, str) and p.startswith("/") and p != "/" and os.path.normpath(p) == p and not p.startswith("//")
            and isinstance(b, str) and (not b or HEX.match(b)) and x.get("Entries") is None)


def service(x):
    if not isinstance(x, dict) or set(x) != {"scope", "units", "Artifacts"} or x["scope"] not in ("system", "user"):
        return False
    if not isinstance(x["units"], list) or not all(isinstance(u, str) for u in x["units"]):
        return False
    return x["Artifacts"] is None or isinstance(x["Artifacts"], list) and all(artifact(y, True) for y in x["Artifacts"])


def engine(env):
    for name, check in (("SUBYARD_TEARDOWN_INVENTORY", resource), ("SUBYARD_TEARDOWN_ARTIFACTS", artifact), ("SUBYARD_TEARDOWN_SERVICES", service)):
        if name in env:
            try:
                value = json.loads(env[name])
            except ValueError:
                return False
            if env[name] != json.dumps(value) or not isinstance(value, list) or not all(check(x) for x in value):
                return False
            keys = [json.dumps(x, sort_keys=True) for x in value]
            return len(keys) == len(set(keys))
    return False


rows = {}
for mode, env in cases:
    full = {"PATH": tools, "HOME": work, "LC_ALL": "C.UTF-8"}
    full.update(env)
    done = subprocess.run([py, "-I", "-B", helper] + mode, env=full, stdin=subprocess.DEVNULL, capture_output=True, timeout=30)
    stdout, stderr = done.stdout.decode(), done.stderr.decode()
    if "Traceback (most recent call last)" in stderr:
        continue  # an uncaught usage error carries no contract
    key = json.dumps(env, sort_keys=True)
    row = rows.setdefault(key, {"env": env, "engine": engine(env), "results": []})
    row["results"].append({"mode": mode, "exit": done.returncode, "stdout": stdout, "stderr": stderr})
for row in rows.values():
    print(json.dumps(row, ensure_ascii=False).replace(work, "$W"))
