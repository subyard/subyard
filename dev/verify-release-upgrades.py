#!/usr/bin/env python3
"""Exercise the frozen update contract with an unmodified released updater."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import signal
import subprocess
import sys
import tempfile
import urllib.request


BASELINE = "0.11.2"
BASELINE_SHA256 = {
    "amd64": "9cadb47ba14bf9407f30eeeeccbbe737fa23bcf50c5b3e6589d55722f39bb818",
    "arm64": "868e6a64acd30f4091d78a5bfa5ee348e8eb96b44acb749258965c1469f28295",
}
LEGACY_BASELINE = "0.9.1"
LEGACY_BASELINE_SHA256 = {
    "amd64": "a5256e985fa5dd0d392df75515f9dca7e239888c3e101867bbd0285035644a90",
    "arm64": "4c349fe6316f90242c09398261e7f76f9bf07c94fa50862e8e707989cd76eea5",
}
LEGACY_INSTALLER_SHA256 = "7932034b1c4f42c7fd66975ff1291bec5ea83b687988d634d67d46ff93ce6d06"


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def snapshot(root):
    return {
        str(path.relative_to(root)): (path.stat().st_mode, digest(path))
        for path in root.rglob("*") if path.is_file()
    }


def run_process(command, env, timeout, input_text=None):
    child = subprocess.Popen(command, env=env,
                             stdin=subprocess.DEVNULL if input_text is None else subprocess.PIPE,
                             stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                             text=True, start_new_session=True)
    try:
        stdout, stderr = child.communicate(input=input_text, timeout=timeout)
        return subprocess.CompletedProcess(command, child.returncode, stdout, stderr)
    finally:
        # A timed-out updater may still have a candidate holding the fixture's
        # locks. Stop every process in this owned session before removing state.
        try:
            os.killpg(child.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        child.communicate()


class Fixture:
    def __init__(self, root, release, version, baseline, arch, baseline_version=BASELINE):
        self.root = root
        self.home = root / "home"
        self.data = self.home / ".subyard"
        self.config = self.home / ".config/subyard"
        self.runtime = self.data / "runtime"
        self.journal = self.config / "release-transition/v2/journal.json"
        self.version = version
        self.config.mkdir(parents=True)
        self.env = {
            "PATH": "/usr/bin:/bin", "HOME": str(self.home), "SHELL": "/bin/bash",
            "SUBYARD_OPERATOR_HOME": str(self.home), "SUBYARD_HOME": str(self.data),
            "SUBYARD_CONFIG_HOME": str(self.config), "YARD_RUNTIME_ROOT": str(self.runtime),
            "YARD_RELEASE_CACHE": str(self.data / "releases"),
            "YARD_RELEASE_BASE_URL": release.as_uri(), "SUBYARD_NO_AUDIT": "1",
            "SUBYARD_INCUS_SOCKET": str(root / "missing-incus.socket"),
            "SUBYARD_POWER_RECONCILER_PATH": str(root / "missing-power-reconciler"),
            "SUBYARD_POWER_UNIT_PATH": str(root / "missing-power-unit"),
        }
        bundle = baseline / f"subyard-{baseline_version}-linux-{arch}.tar.gz"
        installer_root = baseline if baseline_version == LEGACY_BASELINE else release
        self.run([
            "bash", str(installer_root / "subyard-install-runtime-release.sh"),
            "--runtime-root", str(self.runtime), "--bundle", str(bundle),
            "--checksum", str(bundle) + ".sha256", "--manifest", str(bundle) + ".manifest.json",
            "--provenance", str(bundle) + ".provenance.json",
        ])
        self.initial = os.readlink(self.runtime / "current")
        self.old_launcher = self.runtime / self.initial / "bin/yard"
        require(self.run([str(self.old_launcher), "--version"]).strip() == f"yard {baseline_version}",
                "baseline is not the released updater")
        self.retained = self.config / "settings.env"
        self.retained.write_text("# Operator settings must survive the release transition.\n")
        self.retained_hash = digest(self.retained)
        self.project_marker = self.data / "retained-project-data"
        self.project_marker.write_text("project data\n")

    def run(self, command):
        result = run_process(command, self.env, timeout=180)
        require(result.returncode == 0,
                f"command failed ({result.returncode}): {result.stdout}{result.stderr}")
        return result.stdout

    def update(self, *arguments, old=True):
        launcher = self.old_launcher if old else self.runtime / "current/bin/yard"
        return self.run([str(launcher), "update", *arguments])

    def check(self, old=True):
        before = snapshot(self.config)
        current = os.readlink(self.runtime / "current")
        previous = self.runtime / "previous"
        old_previous = os.readlink(previous) if previous.is_symlink() else None
        inspection = json.loads(self.update("--check", "--version", self.version, old=old))
        require(snapshot(self.config) == before, "inspection changed protected config")
        require(os.readlink(self.runtime / "current") == current, "inspection activated a release")
        require((os.readlink(previous) if previous.is_symlink() else None) == old_previous,
                "inspection changed the retained runtime")
        return inspection

    def complete(self, old=True):
        journal = json.loads(self.journal.read_text())
        require(journal["schemaVersion"] == 2 and journal["checkpoint"] == "complete",
                "transition did not complete in the supported journal format")
        require(self.run([str(self.runtime / "current/bin/yard"), "--version"]).strip()
                == f"yard {self.version}", "candidate is not active")
        require(os.readlink(self.runtime / "previous") == self.initial, "baseline was not retained")
        require(digest(self.retained) == self.retained_hash, "operator settings changed")
        require(self.project_marker.read_text() == "project data\n", "project data changed")
        require(self.check(old=old)["outcome"]["status"] == "ready", "updater cannot inspect completion")


def verify_legacy(release, version, baseline, arch, root):
    fixture = Fixture(root / "legacy", release, version, baseline, arch, LEGACY_BASELINE)

    def links():
        return tuple(os.readlink(path) if path.is_symlink() else None
                     for path in (fixture.runtime / "current", fixture.runtime / "previous"))

    def assert_unstarted(before):
        require(snapshot(fixture.config) == before, "blocked legacy upgrade changed protected config")
        require(links() == initial_links, "blocked legacy upgrade changed runtime links")

    initial_links = links()
    before = snapshot(fixture.config)
    result = run_process([str(fixture.old_launcher), "update", "--version", version, "--yes"],
                         fixture.env, timeout=180)
    require(result.returncode != 0, "legacy updater unexpectedly activated the candidate")
    assert_unstarted(before)
    instruction = ("curl -fsSL https://github.com/Subyard/Subyard/releases/download/"
                   f"v{version}/subyard-install.sh | bash -s -- --version {version} --yes")
    require(instruction in result.stdout + result.stderr,
            f"legacy updater omitted the exact pinned bridge instruction: {result.stdout}{result.stderr}")

    installer = (release / "subyard-install.sh").read_text()

    def bridge(*arguments):
        return run_process(["bash", "-s", "--", "--version", version, *arguments],
                           fixture.env, timeout=180, input_text=installer)

    result = bridge()
    require(result.returncode != 0 and "confirmation" in (result.stdout + result.stderr).lower(),
            f"piped legacy bridge did not refuse missing consent: {result.stdout}{result.stderr}")
    assert_unstarted(before)

    nested = fixture.config / "yards/named/config.env"
    flat = fixture.config / "yards/named.env"
    nested.parent.mkdir(parents=True)
    nested.write_text("# Retained nested registration.\n")
    flat.write_text("# Shadowed operator registration.\n")
    nested_hash, flat_hash = digest(nested), digest(flat)
    before = snapshot(fixture.config)
    result = bridge("--yes")
    require(result.returncode != 0, "legacy bridge silently selected a duplicate registration")
    assert_unstarted(before)
    diagnostic = result.stdout + result.stderr
    require(all(fragment in diagnostic for fragment in ("named", "yards/named.env", "yards/named/config.env")),
            f"duplicate diagnostic omitted the exact repair scope: {diagnostic}")

    candidates = list((fixture.runtime / "releases").glob(f"{version}-*"))
    require(len(candidates) == 1, "legacy bridge did not publish one exact verified candidate")
    candidate = candidates[0] / "bin/yard"
    require(fixture.run([str(candidate), "--version"]).strip() == f"yard {version}",
            "repair launcher is not the published candidate")
    fixture.run([str(candidate), "config", "repair-registration", "named", "--yes"])
    archive = fixture.config / "recovery/yard-registrations/named.env"
    require(not flat.exists() and archive.is_file() and digest(archive) == flat_hash,
            "registration repair did not preserve the shadowed config")
    require(digest(nested) == nested_hash, "registration repair changed the retained config")
    require(links() == initial_links and not fixture.journal.exists(),
            "registration repair started the release transition")

    result = bridge("--yes")
    require(result.returncode == 0,
            f"explicitly authorized piped bridge failed: {result.stdout}{result.stderr}")
    fixture.complete(old=False)
    require(digest(archive) == flat_hash and digest(nested) == nested_hash,
            "completed bridge changed the preserved registration configs")
    print(f"PASS: released {LEGACY_BASELINE} refusal, pinned piped bridge, consent and duplicate repair -> {version}",
          flush=True)


def verify(release, version, baseline, arch, root):
    normal = Fixture(root / "normal", release, version, baseline, arch)
    require(normal.check()["outcome"]["status"] == "migration-required",
            "old updater could not inspect candidate activation")
    normal.update("--version", version, "--yes")
    normal.complete()
    normal.update("--rollback", "--yes", old=False)
    require(os.readlink(normal.runtime / "current") == normal.initial, "rollback lost the old runtime")
    normal.update("--offline", "--version", version, "--yes")
    normal.complete()
    print(f"PASS: released {BASELINE} updater -> {version}, fixed point, rollback and forward retry", flush=True)

    interrupted = Fixture(root / "interrupted", release, version, baseline, arch)
    inspection = interrupted.check()
    target = inspection["outcome"]["target"]
    marker = interrupted.root / "activation-observed.json"
    observer = Path(__file__).resolve().parent / "e2e/release-transition-post-cas-observer.py"
    command = [sys.executable, str(observer), "--runtime-root", str(interrupted.runtime),
               "--journal", str(interrupted.journal), "--source-transaction", "none",
               "--candidate-target", target, "--marker", str(marker), "--timeout", "120", "--",
               str(interrupted.old_launcher), "update", "--offline", "--version", version, "--yes"]
    try:
        result = run_process(command, interrupted.env, timeout=150)
    except subprocess.TimeoutExpired:
        raise RuntimeError("interrupted update exceeded its deadline") from None
    require(result.returncode == -signal.SIGKILL and marker.is_file(),
            f"did not interrupt the real update at activation: {result.stdout}{result.stderr}")
    journal = json.loads(interrupted.journal.read_text())
    require(journal["checkpoint"] != "complete", "update completed before interruption")
    transaction = journal["transaction"]
    require(interrupted.check()["outcome"]["status"] == "recovering",
            "old updater cannot route the candidate journal")
    interrupted.update("--offline", "--version", version)
    interrupted.complete()
    require(json.loads(interrupted.journal.read_text())["transaction"] == transaction,
            "resume replaced the authorized transaction")
    print(f"PASS: released {BASELINE} updater resumes the candidate's journal after SIGKILL", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--release-dir", type=Path, required=True)
    parser.add_argument("--version", required=True)
    parser.add_argument("--baseline-dir", type=Path,
                        help="optional directory containing the pinned official baseline assets")
    parser.add_argument("--legacy-baseline-dir", type=Path,
                        help="optional directory containing the pinned official legacy baseline assets")
    args = parser.parse_args()
    arch = {"x86_64": "amd64", "aarch64": "arm64"}.get(platform.machine())
    require(platform.system() == "Linux" and arch in BASELINE_SHA256, "unsupported platform")
    release = args.release_dir.resolve()
    require(args.version not in (BASELINE, LEGACY_BASELINE),
            "candidate must differ from the released baselines")
    os.umask(0o077)
    # Protected runtime roots reject writable workspace ancestors. All mutable
    # state and deliberate process interruption are confined to this owned root.
    with tempfile.TemporaryDirectory(prefix="subyard-release-compat-", dir="/tmp") as directory:
        root = Path(directory)
        legacy = baseline_assets(root, args.legacy_baseline_dir, LEGACY_BASELINE,
                                 LEGACY_BASELINE_SHA256, arch)
        verify_legacy(release, args.version, legacy, arch, root)
        baseline = baseline_assets(root, args.baseline_dir, BASELINE, BASELINE_SHA256, arch)
        verify(release, args.version, baseline, arch, root)


def baseline_assets(root, directory, version, hashes, arch):
    baseline = directory.resolve() if directory else root / f"baseline-{version}"
    bundle_name = f"subyard-{version}-linux-{arch}.tar.gz"
    if directory is None:
        baseline.mkdir()
        print(f"Downloading pinned official {version} baseline for linux/{arch}", flush=True)
        names = [bundle_name + suffix for suffix in ("", ".sha256", ".manifest.json", ".provenance.json")]
        if version == LEGACY_BASELINE:
            names.append("subyard-install-runtime-release.sh")
        for name in names:
            url = f"https://github.com/Subyard/Subyard/releases/download/v{version}/{name}"
            with urllib.request.urlopen(url, timeout=60) as response:
                (baseline / name).write_bytes(response.read())
    require(digest(baseline / bundle_name) == hashes[arch],
            "official baseline checksum does not match the pinned release")
    if version == LEGACY_BASELINE:
        require(digest(baseline / "subyard-install-runtime-release.sh") == LEGACY_INSTALLER_SHA256,
                "official legacy installer does not match the pinned release")
    return baseline


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, KeyError, RuntimeError, subprocess.TimeoutExpired) as error:
        print(f"release upgrade compatibility: {error}", file=sys.stderr)
        sys.exit(1)
