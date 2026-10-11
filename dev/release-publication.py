#!/usr/bin/env python3
"""Prepare exact CI assets, collect local acceptance, publish through serialized Actions."""
import argparse
import base64
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import uuid
import zipfile

ROOT = Path(__file__).resolve().parent.parent
spec = importlib.util.spec_from_file_location("release_acceptance", ROOT / "dev/release-acceptance.py")
a = importlib.util.module_from_spec(spec)
spec.loader.exec_module(a)
REPOSITORY = os.environ.get("GITHUB_REPOSITORY", "Subyard/Subyard")


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"))


def identity_hash(identity):
    return hashlib.sha256(canonical(identity).encode()).hexdigest()


def write_json(path, value):
    with a.private_file(path.with_suffix(path.suffix + ".next")) as stream:
        stream.write(canonical(value) + "\n")
        stream.flush()
        os.fsync(stream.fileno())
    path.with_suffix(path.suffix + ".next").replace(path)
    a.sync_directory(path.parent)


class GitHub:
    def __init__(self, repository=REPOSITORY):
        if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repository):
            raise ValueError("invalid repository")
        self.repository = repository
        self.prefix = "repos/" + repository + "/"

    def api(self, path, method="GET", payload=None, missing=False):
        command = ["gh", "api", self.prefix + path, "--method", method]
        if payload is not None:
            command += ["--input", "-"]
        result = subprocess.run(command, input=canonical(payload) if payload is not None else None,
                                text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        if result.returncode:
            if missing and "(HTTP 404)" in result.stderr:
                return None
            status = re.search(r"HTTP ([0-9]{3})", result.stderr)
            diagnostic = " HTTP " + status[1] if status else ""
            raise ValueError("GitHub API failed: " + method + " " + path + diagnostic +
                             "; inspect workflow permissions (historical workflow changes may require Workflows:write), then retry; never change the pinned SHA")
        return json.loads(result.stdout) if result.stdout.strip() else None

    def pages(self, path, key=None):
        rows = []
        for page in range(1, 1001):
            value = self.api(path + ("&" if "?" in path else "?") + f"per_page=100&page={page}")
            items = value[key] if key else value
            rows.extend(items)
            if len(items) < 100:
                return rows
        raise ValueError("GitHub inventory exceeded pagination limit")

    def download(self, path, destination):
        with destination.open("wb") as stream:
            result = subprocess.run(["gh", "api", self.prefix + path, "-H", "Accept: application/octet-stream"],
                                    stdout=stream, stderr=subprocess.PIPE)
        if result.returncode:
            raise ValueError("GitHub download failed; retry with retained candidate")

    def upload(self, release_id, path):
        # gh api supports streaming the exact file body to the uploads endpoint.
        result = subprocess.run(["gh", "api", "https://uploads.github.com/" + self.prefix +
                                 f"releases/{release_id}/assets?name={path.name}", "--method", "POST",
                                 "-H", "Content-Type: application/octet-stream", "--input", str(path)],
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        if result.returncode:
            raise ValueError("asset upload unfinished; retry reconciles remote assets")


def candidate_parts(candidate):
    match = re.fullmatch(r"r([1-9][0-9]*)-a([1-9][0-9]*)", candidate)
    if not match:
        raise ValueError("candidate must be rRUN_ID-aATTEMPT")
    return tuple(map(int, match.groups()))


def checked_identity(prepared):
    for name in ("identity.json", "source.tar.gz"):
        if not (prepared / name).is_file() or (prepared / name).is_symlink():
            raise ValueError("preparation metadata must be regular files")
    identity = json.loads((prepared / "identity.json").read_text())
    expected = {"schema_version", "candidate_id", "repository", "version", "commit", "source_fingerprint",
                "source_archive_sha256", "release_files"}
    if (set(identity) != expected or type(identity["schema_version"]) is not int or identity["schema_version"] != 1
        or not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", identity["version"])
        or not re.fullmatch(r"[0-9a-f]{40}", identity["commit"])
        or any(not re.fullmatch(r"[0-9a-f]{64}", identity[key]) for key in
               ("source_fingerprint", "source_archive_sha256"))):
        raise ValueError("invalid preparation identity")
    candidate_parts(identity["candidate_id"])
    assets = prepared / "assets"
    names = subprocess.check_output(["bash", str(ROOT / "dev/release-assets.sh"), "--release-dir", str(assets),
                                     "--version", identity["version"]], text=True).splitlines()
    expected_names = {Path(name).name for name in names}
    if (set(identity["release_files"]) != expected_names
        or {path.name for path in assets.iterdir()} != expected_names
        or any(path.is_symlink() or not path.is_file() for path in assets.iterdir())
        or {path.name: a.digest(path) for path in assets.iterdir()} != identity["release_files"]
        or a.digest(prepared / "source.tar.gz") != identity["source_archive_sha256"]):
        raise ValueError("prepared inventory or hashes mismatch")
    for name in expected_names:
        if name.endswith(".provenance.json"):
            value = json.loads((assets / name).read_text())
            artifact = name.removesuffix(".provenance.json")
            if (value.get("sourceRevision") != identity["commit"] or value.get("version") != identity["version"]
                or value.get("sha256") != identity["release_files"][artifact] or value.get("artifact") != artifact):
                raise ValueError("prepared provenance mismatch")
        if name.endswith(".sha256"):
            artifact = name.removesuffix(".sha256")
            if (assets / name).read_text().strip() != identity["release_files"][artifact] + "  " + artifact:
                raise ValueError("prepared checksum mismatch")
    return identity


def verify_run(github, candidate):
    run_id, attempt = candidate_parts(candidate)
    run = github.api(f"actions/runs/{run_id}/attempts/{attempt}")
    workflow = github.api("actions/workflows/release.yml")
    if (run["workflow_id"] != workflow["id"] or run["event"] != "workflow_dispatch"
        or run["status"] != "completed" or run["conclusion"] != "success"
        or run["run_attempt"] != attempt or run["repository"]["full_name"] != github.repository):
        raise ValueError("preparation run/attempt did not pass")
    jobs = github.pages(f"actions/runs/{run_id}/attempts/{attempt}/jobs", "jobs")
    required = {"validate", "native-engine-arm64", "paseo-headless-amd64", "paseo-headless-arm64", "prepare"}
    if (len(jobs) != len(required) or {job["name"] for job in jobs} != required
        or any(job["status"] != "completed" or job["conclusion"] != "success" for job in jobs)):
        raise ValueError("preparation job inventory did not completely pass in this attempt")
    return run


def fetch(github, candidate, destination):
    verify_run(github, candidate)
    run_id, attempt = candidate_parts(candidate)
    artifacts = [item for item in github.pages(f"actions/runs/{run_id}/artifacts", "artifacts")
                 if item["name"] == f"release-candidate-{attempt}" and not item["expired"]]
    if len(artifacts) != 1:
        raise ValueError("exact preparation artifact unavailable")
    destination.mkdir(parents=True, exist_ok=False)
    destination.chmod(0o700)
    archive = destination / "download.zip"
    github.download(f"actions/artifacts/{artifacts[0]['id']}/zip", archive)
    with zipfile.ZipFile(archive) as zipped:
        for item in zipped.infolist():
            if item.filename.startswith("/") or ".." in Path(item.filename).parts:
                raise ValueError("unsafe candidate artifact")
        zipped.extractall(destination)
    archive.unlink()
    identity = checked_identity(destination)
    if identity["candidate_id"] != candidate or identity["repository"] != github.repository:
        raise ValueError("preparation artifact belongs to another candidate")
    return identity


def export_candidate(output, version, commit, candidate, repository):
    if subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip() != commit:
        raise ValueError("CI checkout is not the exact requested commit")
    if subprocess.check_output(["git", "status", "--porcelain", "--untracked-files=all"], cwd=ROOT):
        raise ValueError("CI public checkout changed after the pinned checkout")
    if not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", version) or not re.fullmatch(r"[0-9a-f]{40}", commit):
        raise ValueError("stable semver and full commit SHA required")
    candidate_parts(candidate)
    output.mkdir(parents=True, exist_ok=False)
    assets = output / "assets"
    assets.mkdir()
    names = subprocess.check_output(["bash", "dev/release-assets.sh", "--release-dir", ".build/release",
                                     "--version", version], cwd=ROOT, text=True).splitlines()
    for name in names:
        shutil.copyfile(ROOT / name, assets / Path(name).name)
    fingerprint = a.source_fingerprint(ROOT)
    a.bundle(ROOT, output / "source.tar.gz")
    if a.source_fingerprint(ROOT) != fingerprint:
        raise ValueError("source changed during preparation")
    identity = {"schema_version": 1, "candidate_id": candidate, "repository": repository,
                "version": version, "commit": commit, "source_fingerprint": fingerprint,
                "source_archive_sha256": a.digest(output / "source.tar.gz"),
                "release_files": {path.name: a.digest(path) for path in sorted(assets.iterdir())}}
    write_json(output / "identity.json", identity)
    checked_identity(output)


def attestation(output, identity):
    root, receipt = a.load_bound(output)
    a.update_result(receipt)
    if (receipt["result"] != "passed" or receipt.get("preparation") != identity
        or receipt["source_fingerprint"] != identity["source_fingerprint"]
        or receipt["release_files"] != identity["release_files"]):
        raise ValueError("complete passing acceptance of this exact candidate is required")
    checks = {}
    for name, result in receipt["checks"].items():
        if result["status"] == "passed":
            if result.get("exit_code") != 0 or not re.fullmatch(r"[0-9a-f]{64}", result.get("log_sha256", "")):
                raise ValueError("passing check lacks evidence")
            if not a.is_live(name) and result.get("execution_environment") != receipt["environment"]:
                raise ValueError("passing local check execution environment differs")
            if name != "package" and result.get("command") != a.command_identity(a.commands(root, receipt["version"], receipt["profiles"])[name], root):
                raise ValueError("passing check lacks command binding")
        checks[name] = {"status": result["status"], **({"log_sha256": result["log_sha256"], "exit_code": 0}
                                                     if result["status"] == "passed" else {})}
        if a.is_live(name) and result["status"] == "passed":
            checks[name]["environments"] = result.get("environments", [])
    external = {}
    for name, result in receipt["external_obligations"].items():
        if not re.fullmatch(r"[0-9a-f]{64}", result.get("evidence_sha256", "")):
            raise ValueError("external obligation lacks actual evidence SHA-256")
        external[name] = {"status": result["status"], "evidence_sha256": result["evidence_sha256"]}
    return {"schema_version": 1, "candidate_id": identity["candidate_id"], "identity_sha256": identity_hash(identity),
            "receipt_sha256": a.digest(output / "receipt.json"), "environment": receipt["environment"],
            "checks": checks, "external_obligations": external}


def validate_attestation(document, identity, root):
    if (set(document) != {"schema_version", "candidate_id", "identity_sha256", "receipt_sha256", "environment", "checks", "external_obligations"}
        or type(document["schema_version"]) is not int or document["schema_version"] != 1 or document["candidate_id"] != identity["candidate_id"]
        or document["identity_sha256"] != identity_hash(identity)
        or not re.fullmatch(r"[0-9a-f]{64}", document["receipt_sha256"])):
        raise ValueError("acceptance attestation identity mismatch")
    environment = document["environment"]
    if (not isinstance(environment, dict) or set(environment) != {"system", "machine", "go"}
        or any(not isinstance(value, str) or not value or len(value) > 256 or any(ord(c) < 32 for c in value)
               for value in environment.values())):
        raise ValueError("acceptance execution environment missing")
    inventory = a.profile_inventory(root)
    required = a.commands(root, identity["version"], inventory)
    required["package"] = ["ci-prepared"]
    if set(document["checks"]) != set(required):
        raise ValueError("acceptance attestation check inventory mismatch")
    for name, command in required.items():
        result = document["checks"][name]
        if command is None:
            if result != {"status": "not-applicable"}:
                raise ValueError("invalid not-applicable attestation")
        else:
            keys = {"status", "log_sha256", "exit_code"} | ({"environments"} if a.is_live(name) else set())
            if (set(result) != keys or result["status"] != "passed" or type(result["exit_code"]) is not int or result["exit_code"] != 0
                or not re.fullmatch(r"[0-9a-f]{64}", result["log_sha256"])):
                raise ValueError("missing passing acceptance evidence")
            if a.is_live(name):
                environments = result["environments"]
                if not isinstance(environments, list) or not environments:
                    raise ValueError("physical acceptance environment missing")
                for item in environments:
                    if (not isinstance(item, dict) or set(item) != {"type", "vm_count", "base_fingerprint"}
                        or not re.fullmatch(r"[a-z][a-z0-9-]{0,79}", item["type"])
                        or type(item["vm_count"]) is not int or not 1 <= item["vm_count"] <= 100
                        or not re.fullmatch(r"[0-9a-f]{64}", item["base_fingerprint"])):
                        raise ValueError("invalid physical acceptance environment")
    if set(document["external_obligations"]) != set(a.external_inventory(root)):
        raise ValueError("external obligation attestation inventory mismatch")
    for result in document["external_obligations"].values():
        if (set(result) != {"status", "evidence_sha256"} or result["status"] != "passed"
            or not re.fullmatch(r"[0-9a-f]{64}", result["evidence_sha256"])):
            raise ValueError("external obligation evidence missing")


def verify_owned_ref(github, tag, marker, commit, ref=None):
    ref = ref if ref is not None else github.api("git/ref/tags/" + tag)
    if ref["object"]["type"] != "tag":
        raise ValueError("stable tag is occupied by foreign/legacy state")
    obj = github.api("git/tags/" + ref["object"]["sha"])
    if (obj["message"].strip() != marker or obj["object"].get("type") != "commit"
        or obj["object"].get("sha") != commit):
        raise ValueError("stable tag belongs to another candidate")
    return ref


def publish_remote(github, prepared, document, root):
    identity = checked_identity(prepared)
    if identity["repository"] != github.repository or a.source_fingerprint(root) != identity["source_fingerprint"]:
        raise ValueError("publication source/repository mismatch")
    verify_run(github, identity["candidate_id"])
    validate_attestation(document, identity, root)
    marker = "Subyard candidate " + identity["candidate_id"] + " identity " + identity_hash(identity)
    tag = "v" + identity["version"]
    releases = [item for item in github.pages("releases") if item["tag_name"] == tag]
    if len(releases) > 1:
        raise ValueError("ambiguous release state")
    if releases and (releases[0].get("body", "").strip() != marker or releases[0].get("prerelease")):
        raise ValueError("release is foreign or contradictory")
    ref = github.api("git/ref/tags/" + tag, missing=True)
    if ref is None:
        obj = github.api("git/tags", "POST", {"tag": tag, "message": marker, "object": identity["commit"], "type": "commit"})
        github.api("git/refs", "POST", {"ref": "refs/tags/" + tag, "sha": obj["sha"]})
        ref = github.api("git/ref/tags/" + tag)
    ref = verify_owned_ref(github, tag, marker, identity["commit"], ref)
    release = releases[0] if releases else github.api("releases", "POST", {
        "tag_name": tag, "target_commitish": identity["commit"], "name": "Subyard " + identity["version"],
        "body": marker, "draft": True, "prerelease": False})
    if release.get("body", "").strip() != marker or release.get("prerelease"):
        raise ValueError("release is foreign or contradictory")
    expected = identity["release_files"]
    assets = github.pages(f"releases/{release['id']}/assets")
    if len({item["name"] for item in assets}) != len(assets) or any(item["name"] not in expected for item in assets):
        raise ValueError("release has foreign or duplicate assets")
    present = {item["name"]: item for item in assets}
    for name, sha in expected.items():
        if name in present and present[name].get("state") == "starter":
            if not release["draft"] or present[name].get("size") != 0:
                raise ValueError("contradictory unfinished upload")
            # GitHub documents starter assets after HTTP 502 as safe to delete.
            github.api(f"releases/assets/{present[name]['id']}", "DELETE")
            del present[name]
        if name not in present:
            if not release["draft"]:
                raise ValueError("published release inventory incomplete")
            github.upload(release["id"], prepared / "assets" / name)
    if release["draft"]:
        verify_release_assets(github, release["id"], expected)
        github.api(f"releases/{release['id']}", "PATCH", {"draft": False, "make_latest": "true"})
    actual = github.api(f"releases/{release['id']}")
    if (actual["draft"] or actual.get("immutable") is not True or actual["tag_name"] != tag
        or actual.get("body", "").strip() != marker or actual.get("prerelease")):
        raise ValueError("actual release is not immutable and fully published; never roll back")
    final_ref = github.api("git/ref/tags/" + tag)
    if final_ref["object"] != ref["object"]:
        raise ValueError("stable ref changed during publication")
    verify_release_assets(github, release["id"], expected)
    return actual


def verify_release_assets(github, release_id, expected):
    assets = github.pages(f"releases/{release_id}/assets")
    if len(assets) != len(expected) or {item["name"] for item in assets} != set(expected):
        raise ValueError("remote asset inventory incomplete")
    with tempfile.TemporaryDirectory(prefix="subyard-release-assets-", dir="/tmp") as temporary:
        for item in assets:
            if item.get("state") != "uploaded":
                raise ValueError("unfinished asset upload; preserve draft for operator inspection")
            downloaded = Path(temporary) / "asset"
            github.download(f"releases/assets/{item['id']}", downloaded)
            if a.digest(downloaded) != expected[item["name"]] or downloaded.stat().st_size != item["size"]:
                raise ValueError("remote asset hash mismatch; never overwrite")


def discover(github, workflow, request):
    matches = [run for run in github.pages(f"actions/workflows/{workflow}/runs?event=workflow_dispatch", "workflow_runs")
               if run["display_title"] == "Subyard " + request]
    if len(matches) > 1:
        raise ValueError("ambiguous dispatch identity")
    return matches[0] if matches else None


def dispatch(github, workflow, inputs, state, path):
    request = state["request"]
    if not state.get("dispatch_started"):
        state["dispatch_started"] = True
        state["status"] = "dispatch-outcome-unknown"
        write_json(path, state)  # Persist intent before a possibly lost response.
        github.api(f"actions/workflows/{workflow}/dispatches", "POST", {"ref": state["workflow_ref"], "inputs": {**inputs, "request": request}})
    deadline = time.monotonic() + 120
    while time.monotonic() < deadline:
        run = discover(github, workflow, request)
        if run:
            state["run_id"] = run["id"]
            if run["status"] == "completed" and run["conclusion"] != "success":
                # Persist rerun intent too: response loss must not launch another attempt.
                target = run["run_attempt"] + 1
                if state.get("rerun_attempt", 0) < target:
                    state["rerun_attempt"] = target
                    state["status"] = "rerun-outcome-unknown"
                    write_json(path, state)
                    github.api(f"actions/runs/{run['id']}/rerun", "POST")
                while time.monotonic() < deadline:
                    run = github.api(f"actions/runs/{run['id']}")
                    if run["run_attempt"] >= target:
                        break
                    time.sleep(2)
                else:
                    raise ValueError("rerun outcome unknown; repeat command to discover retained attempt")
            state["status"] = "running"
            write_json(path, state)
            print("RUN " + run["html_url"], flush=True)
            try:
                last_report = 0
                while True:
                    run = github.api(f"actions/runs/{run['id']}")
                    state["run_attempt"] = run["run_attempt"]
                    state["status"] = run["status"]
                    write_json(path, state)
                    if run["status"] == "completed":
                        state["status"] = "passed" if run["conclusion"] == "success" else run["conclusion"]
                        write_json(path, state)
                        if run["conclusion"] != "success":
                            raise ValueError("workflow failed/cancelled; repeat command resumes the same request")
                        return run
                    if time.monotonic() - last_report >= 60:
                        print(f"WAIT run={run['id']} attempt={run['run_attempt']} status={run['status']}", flush=True)
                        last_report = time.monotonic()
                    time.sleep(20)
            except KeyboardInterrupt:
                state["status"] = "interrupted"
                write_json(path, state)
                raise
    raise ValueError("dispatch outcome unknown; repeat command to discover retained request, do not duplicate dispatch")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--state-dir", type=Path, default=ROOT / ".build/release-publication")
    parser.add_argument("--repository", default=REPOSITORY)
    sub = parser.add_subparsers(dest="action", required=True)
    prepare = sub.add_parser("prepare", help="dispatch all preparation gates on a pinned commit; create no stable tag")
    prepare.add_argument("--version", required=True)
    prepare.add_argument("--commit", required=True)
    prepare.add_argument("--workflow-ref", default="master", help="branch containing the dispatch workflow")
    for action in ("accept", "publish"):
        command = sub.add_parser(action)
        command.add_argument("--candidate", required=True)
        if action == "accept":
            command.add_argument("--slots", nargs="+", type=int, required=True)
        else:
            command.add_argument("--workflow-ref", default="master")
    export = sub.add_parser("export-ci", help="internal CI asset freezing entrypoint")
    for name in ("version", "commit", "candidate"):
        export.add_argument("--" + name, required=True)
    export.add_argument("--output", type=Path, required=True)
    remote = sub.add_parser("publish-ci", help="internal serialized workflow publication entrypoint")
    remote.add_argument("--prepared", type=Path, required=True)
    remote.add_argument("--attestation", type=Path, required=True)
    args = parser.parse_args()
    def interrupted(signum, frame):
        raise KeyboardInterrupt
    signal.signal(signal.SIGTERM, interrupted)
    github = GitHub(args.repository)
    if args.action == "export-ci":
        export_candidate(args.output.resolve(), args.version, args.commit, args.candidate, args.repository)
        return 0
    if args.action == "publish-ci":
        # Mutation entrypoint belongs only to the single native concurrency workflow.
        if os.environ.get("GITHUB_ACTIONS") != "true" or os.environ.get("GITHUB_WORKFLOW") != "Release publication":
            raise ValueError("publication must run in the serialized Release publication workflow")
        actual = publish_remote(github, args.prepared.resolve(), json.loads(args.attestation.read_text()), ROOT)
        print("PUBLISHED " + actual["html_url"])
        return 0
    state_dir = args.state_dir.resolve()
    if state_dir == ROOT or (ROOT in state_dir.parents and ROOT / ".build" not in state_dir.parents):
        raise ValueError("state directory must be under .build or outside the source checkout")
    state_dir.mkdir(parents=True, exist_ok=True)
    state_dir.chmod(0o700)
    if args.action == "prepare":
        if not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", args.version) or not re.fullmatch(r"[0-9a-f]{40}", args.commit):
            raise ValueError("stable X.Y.Z and full lowercase 40-character commit SHA required")
        requests = state_dir / "requests"
        requests.mkdir(exist_ok=True)
        path = requests / (args.version + "-" + args.commit + ".json")
        with a.acceptance_lock(requests):
            state = json.loads(path.read_text()) if path.exists() else {
                "request": str(uuid.uuid4()), "version": args.version, "commit": args.commit,
                "repository": args.repository, "workflow_ref": args.workflow_ref}
            if state["repository"] != args.repository:
                raise ValueError("request repository mismatch")
            write_json(path, state)
            run = dispatch(github, "release.yml", {"version": args.version, "commit": args.commit}, state, path)
            candidate = f"r{run['id']}-a{run['run_attempt']}"
            directory = state_dir / candidate
            directory.mkdir(exist_ok=True)
            prepared = directory / "prepared"
            if not prepared.exists():
                with tempfile.TemporaryDirectory(prefix=".download-", dir=directory) as temporary:
                    staging = Path(temporary) / "prepared"
                    identity = fetch(github, candidate, staging)
                    if identity["version"] != args.version or identity["commit"] != args.commit:
                        raise ValueError("preparation inputs mismatch")
                    staging.rename(prepared)
            identity = checked_identity(prepared)
            if (identity["candidate_id"] != candidate or identity["repository"] != github.repository
                or identity["version"] != args.version or identity["commit"] != args.commit):
                raise ValueError("local preparation state mismatch")
            write_json(directory / "identity.json", identity)
            print("CANDIDATE " + candidate + " " + str(directory))
            return 0
    candidate_parts(args.candidate)
    directory = state_dir / args.candidate
    directory.mkdir(exist_ok=True)
    directory.chmod(0o700)
    with a.acceptance_lock(directory):
        prepared = directory / "prepared"
        if not prepared.exists():
            with tempfile.TemporaryDirectory(prefix=".download-", dir=directory) as temporary:
                staging = Path(temporary) / "prepared"
                fetch(github, args.candidate, staging)
                staging.rename(prepared)
        identity = checked_identity(prepared)
        if identity["candidate_id"] != args.candidate or identity["repository"] != github.repository:
            raise ValueError("local state belongs to another candidate/repository")
        write_json(directory / "identity.json", identity)
        output = directory / "acceptance"
        if args.action == "accept":
            for signum in (signal.SIGINT, signal.SIGTERM):
                signal.signal(signum, lambda signum, frame: a.STOP.set())
            if len(set(args.slots)) != len(args.slots) or any(slot < 1 or slot > 999 for slot in args.slots):
                raise ValueError("slots must be unique integers from 1 to 999")
            if not output.exists():
                with tempfile.TemporaryDirectory(prefix=".acceptance-", dir=directory) as temporary:
                    staging = Path(temporary) / "acceptance"
                    a.prepare(staging, identity["version"], prepared)
                    staging.rename(output)
            with a.acceptance_lock(output):
                result = a.run_checks(output, args.slots, [], False)
            if result == 0:
                write_json(directory / "attestation.json", attestation(output, identity))
            return result
        with a.acceptance_lock(output, shared=True):
            document = attestation(output, identity)
            validate_attestation(document, identity, output / "source")
            write_json(directory / "attestation.json", document)
        path = directory / "publication.json"
        state = json.loads(path.read_text()) if path.exists() else {
            "request": str(uuid.uuid4()), "workflow_ref": args.workflow_ref, "attestation_sha256": identity_hash(document)}
        if state["attestation_sha256"] != identity_hash(document):
            raise ValueError("publication attestation changed; preserve original unfinished state")
        write_json(path, state)
        encoded = base64.b64encode(canonical(document).encode()).decode()
        run = dispatch(github, "release-publish.yml", {"candidate": args.candidate, "attestation": encoded}, state, path)
        # A failed/cancelled run can be rerun natively; no second publication request is needed.
        release = github.api("releases/tags/v" + identity["version"])
        marker = "Subyard candidate " + args.candidate + " identity " + identity_hash(identity)
        if (release.get("draft") or release.get("immutable") is not True or release.get("body", "").strip() != marker):
            raise ValueError("actual immutable publication not confirmed")
        verify_owned_ref(github, "v" + identity["version"], marker, identity["commit"])
        verify_release_assets(github, release["id"], identity["release_files"])
        state.update({"status": "published", "release_id": release["id"], "run_id": run["id"]})
        write_json(path, state)
        print("PUBLISHED " + release["html_url"])
        return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except KeyboardInterrupt:
        print("release: interrupted; retained durable state permits retry", file=sys.stderr)
        sys.exit(130)
    except (ValueError, KeyError, TypeError, AttributeError, OSError, subprocess.SubprocessError) as error:
        message = str(error) if isinstance(error, (ValueError, KeyError)) else type(error).__name__
        print("release: " + message, file=sys.stderr)
        sys.exit(2)
