#!/usr/bin/env python3
"""Pin rollback inputs from the latest published baseline's local release tag.

Run after updating published-upgrade-baselines.tsv. No network or installation.
"""
import hashlib
import json
import pathlib
import subprocess

ROOT = pathlib.Path(__file__).resolve().parent.parent


def git(*args):
    return subprocess.check_output(["git", "-C", str(ROOT), *args])


def main():
    rows = [line.split() for line in
            (ROOT / "dev/e2e/published-upgrade-baselines.tsv").read_text().splitlines()]
    version, _, bundle, _ = max(rows, key=lambda row: tuple(map(int, row[0].split("."))))
    tag = f"v{version}"
    digest = hashlib.sha256()
    profiles = {}
    for record in git("ls-tree", "-r", tag, "config/agents", "config/profiles").decode().splitlines():
        metadata, path = record.split("\t")
        mode, kind, _ = metadata.split()
        if path.startswith("config/agents/") and "tests" not in path.split("/"):
            if kind != "blob" or mode not in ("100644", "100755"):
                raise ValueError(f"agent source must be a regular file: {path}")
            data = git("show", f"{tag}:{path}")
            relative = path.removeprefix("config/agents/")
            digest.update(f"{relative} {int(mode, 8) & 0o111:03o} {hashlib.sha256(data).hexdigest()}\n".encode())
        if path.startswith("config/profiles/") and path.endswith("/profile.json"):
            runtime = json.loads(git("show", f"{tag}:{path}")).get("runtime")
            if runtime:
                profiles[runtime["activation_id"]] = runtime
    declaration = (json.loads(git("show", f"{tag}:config/release-rollback.json"))
                   if git("ls-tree", tag, "config/release-rollback.json").strip() else {})
    snapshot = {"version": version, "bundleSHA256": bundle,
                "agentSourcesDigest": digest.hexdigest(), "profiles": profiles,
                "registryFeatures": declaration.get("registryFeatures", [])}
    fixtures = ROOT / "internal/releasetransition/testdata"
    (fixtures / "published-rollback-baseline.json").write_text(json.dumps(snapshot, indent=2, sort_keys=True) + "\n")
    (fixtures / f"{tag}-release-transition.json").write_bytes(git("show", f"{tag}:config/release-transition.json"))


if __name__ == "__main__":
    main()
