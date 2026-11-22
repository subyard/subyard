#!/usr/bin/env python3
"""Pin rollback inputs from the latest published baseline's local release tag.

Run after updating published-upgrade-baselines.tsv. No network or installation.
"""
import json
import pathlib
import subprocess
import tempfile
import io
import tarfile

ROOT = pathlib.Path(__file__).resolve().parent.parent


def git(*args):
    return subprocess.check_output(["git", "-C", str(ROOT), *args])


def main():
    rows = [line.split() for line in
            (ROOT / "dev/e2e/published-upgrade-baselines.tsv").read_text().splitlines()]
    version, _, bundle, _ = max(rows, key=lambda row: tuple(map(int, row[0].split("."))))
    tag = f"v{version}"
    declaration = (json.loads(git("show", f"{tag}:config/release-rollback.json"))
                   if git("ls-tree", tag, "config/release-rollback.json").strip() else {})
    # The same Go inventory and canonical record ordering serve both readers.
    with tempfile.TemporaryDirectory(prefix="subyard-published-inventory-", dir="/tmp") as temporary:
        source = pathlib.Path(temporary) / "source"
        source.mkdir()
        tracked = git("ls-tree", "-r", "--name-only", tag).decode().splitlines()
        archive = git("archive", tag)
        with tarfile.open(fileobj=io.BytesIO(archive)) as release:
            release.extractall(source, filter="data")
        manifest = pathlib.Path(temporary) / "tracked.json"
        manifest.write_text(json.dumps(sorted(tracked)))
        inventory = json.loads(subprocess.check_output(
            ["go", "run", "./dev/rollback-inventory", str(source), str(manifest)], cwd=ROOT))
    snapshot = {"version": version, "bundleSHA256": bundle,
                "agentSourcesDigest": inventory["digest"], "profiles": inventory["profiles"],
                "registryFeatures": declaration.get("registryFeatures", []),
                "predecessorRegistryDigest": declaration.get("predecessorRegistryDigest", "")}
    fixtures = ROOT / "internal/releasetransition/testdata"
    (fixtures / "published-rollback-baseline.json").write_text(json.dumps(snapshot, indent=2, sort_keys=True) + "\n")
    (fixtures / f"{tag}-release-transition.json").write_bytes(git("show", f"{tag}:config/release-transition.json"))


if __name__ == "__main__":
    main()
