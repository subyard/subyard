#!/usr/bin/env python3
"""Exercise supported update contracts with unmodified released updaters."""

import argparse
import ctypes
import hashlib
import json
import os
from pathlib import Path
import platform
import select
import signal
import shutil
import subprocess
import sys
import tempfile
import time
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
ACTIVATION_BASELINE = "0.17.3"
ACTIVATION_BASELINE_SHA256 = {
    "amd64": "3ea73ca51dae023600997a07bbfaa5df8be1f4c1c4f5c9ead1b261b5aec4363b",
    "arm64": "dcc1ae42dc25760b9f4fd290c8aec23c09b729d4a04f780107022710f1247e79",
}
PUBLISHED_BASELINE = "0.18.1"
PUBLISHED_BASELINE_SHA256 = {
    "amd64": "a2e4a18ac806079b70b35a5b679ef6c616bc22137453d6f3f0e0e34ef491ef74",
    "arm64": "2172f375871fe280297f69e9ae88a7d1ca1df5678c02d6189551cfa98aa07d9a",
}


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


def run_process(command, env, timeout, input_text=None, interrupt_marker=None,
                interrupt_replacement=None):
    watch = None
    if interrupt_replacement is not None:
        journal, _, _ = interrupt_replacement
        libc = ctypes.CDLL(None, use_errno=True)
        watch = libc.inotify_init1(os.O_CLOEXEC | os.O_NONBLOCK)
        require(watch >= 0, "cannot observe the owned recovery journal")
        require(libc.inotify_add_watch(watch, os.fsencode(journal.parent), 0x00000080) >= 0,
                "cannot watch the owned recovery journal directory")
    child = subprocess.Popen(command, env=env,
                             stdin=subprocess.DEVNULL if input_text is None else subprocess.PIPE,
                             stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                             text=True, start_new_session=True)
    try:
        if interrupt_replacement is not None:
            journal, predecessor, marker = interrupt_replacement
            deadline = time.monotonic() + timeout
            observed = False
            while child.poll() is None and time.monotonic() < deadline:
                readable, _, _ = select.select([watch], [], [], 0.02)
                if not readable:
                    continue
                os.read(watch, 65536)
                # Pause this owned session before reading the durable state;
                # no journal phase edits or runtime-link changes are fixtures.
                os.killpg(child.pid, signal.SIGSTOP)
                current = json.loads(journal.read_text())
                if current["transaction"] != predecessor:
                    require(current["transaction"].startswith(("recovery-v1-", "recovery-v2-"))
                            and current["checkpoint"] != "complete",
                            "fresh recovery passed its observed publication boundary")
                    marker.write_text(json.dumps({"transaction": current["transaction"],
                                                  "checkpoint": current["checkpoint"]}) + "\n")
                    marker.chmod(0o600)
                    os.killpg(child.pid, signal.SIGKILL)
                    observed = True
                    break
                os.killpg(child.pid, signal.SIGCONT)
            require(observed, "fresh recovery did not publish an observable successor")
        if interrupt_marker is not None:
            deadline = time.monotonic() + timeout
            while not interrupt_marker.is_file() and child.poll() is None:
                if time.monotonic() >= deadline:
                    raise subprocess.TimeoutExpired(command, timeout)
                time.sleep(0.02)
            if interrupt_marker.is_file():
                os.killpg(child.pid, signal.SIGKILL)
        stdout, stderr = child.communicate(input=input_text, timeout=timeout)
        return subprocess.CompletedProcess(command, child.returncode, stdout, stderr)
    finally:
        if watch is not None:
            os.close(watch)
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
        self.baseline_version = baseline_version
        self.config.mkdir(parents=True)
        self.env = {
            "PATH": "/usr/bin:/bin", "HOME": str(self.home), "SHELL": "/bin/bash",
            "SUBYARD_OPERATOR_HOME": str(self.home), "SUBYARD_HOME": str(self.data),
            "SUBYARD_CONFIG_HOME": str(self.config), "YARD_RUNTIME_ROOT": str(self.runtime),
            "YARD_RELEASE_CACHE": str(self.data / "releases"),
            "YARD_RELEASE_BASE_URL": release.as_uri(), "SUBYARD_NO_AUDIT": "1",
            "SUBYARD_INCUS_SOCKET": str(root.parent / "incus.socket"),
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

    def complete(self, old=True, expected_previous=None):
        journal = json.loads(self.journal.read_text())
        require(journal["schemaVersion"] == 2 and journal["checkpoint"] == "complete",
                "transition did not complete in the supported journal format")
        require(self.run([str(self.runtime / "current/bin/yard"), "--version"]).strip()
                == f"yard {self.version}", "candidate is not active")
        require(os.readlink(self.runtime / "previous") == (expected_previous or self.initial),
                "the exact previous runtime was not retained")
        require(digest(self.retained) == self.retained_hash, "operator settings changed")
        require(self.project_marker.read_text() == "project data\n", "project data changed")
        require(self.check(old=old)["outcome"]["status"] == "ready", "updater cannot inspect completion")
        self.compact_history()

    def compact_history(self):
        capability = self.runtime / "current/config/release-checkpoint.json"
        if not capability.exists():
            return
        checkpoint_path = self.config / "release-transition/v2/history-checkpoint.json"
        require(checkpoint_path.is_file(), "capable candidate did not convert its completed migration history")
        checkpoint = json.loads(checkpoint_path.read_text())
        projection = self.config / "release-transition/v2/ledger.json"
        binding = checkpoint["legacyProjection"]
        require(checkpoint["schemaVersion"] == 1 and checkpoint["domains"]
                and binding["exists"] == projection.exists()
                and (not projection.exists() or binding["fingerprint"] == digest(projection)),
                "compact history lost its immutable legacy projection binding")
        require(all(state["compactedThrough"] == state["epoch"] and not state["appliedSuffix"]
                    for state in checkpoint["domains"].values()),
                "completed migration IDs remain in the authoritative history")
        registry = json.loads((self.runtime / "current/config/release-transition.json").read_text())
        require({domain: state["epoch"] for domain, state in checkpoint["domains"].items()}
                == registry["currentEpochs"], "compact history does not match the active registry epochs")


def verify_checkpoint_refusal(fixture):
    checkpoint = fixture.config / "release-transition/v2/history-checkpoint.json"
    if not checkpoint.exists():
        return
    projection = fixture.config / "release-transition/v2/ledger.json"
    retained = projection.read_bytes()
    # The only authority switch is protected checkpoint publication. Neither
    # reader may silently select a legacy projection changed by an old writer.
    projection.write_bytes(retained + b"\n")
    try:
        before = snapshot(fixture.config)
        for old in (False, True):
            launcher = fixture.old_launcher if old else fixture.runtime / "current/bin/yard"
            result = run_process([str(launcher), "update", "--check", "--version", fixture.version],
                                 fixture.env, timeout=180)
            ready = result.returncode == 0 and json.loads(result.stdout)["outcome"]["status"] == "ready"
            require(not ready and snapshot(fixture.config) == before,
                    "reader trusted or changed a divergent compatibility projection")
    finally:
        projection.write_bytes(retained)
    fixture.complete()
    print(f"PASS: candidate and released {fixture.baseline_version} caller refuse a changed pinned ledger projection without mutation",
          flush=True)


def verify_checkpoint_source(release, version, baseline, arch, root, baseline_version=BASELINE):
    fixture = Fixture(root / f"checkpoint-source-{baseline_version}", release, version, baseline, arch,
                      baseline_version)
    fixture.update("--version", version, "--yes")
    fixture.complete()
    bridge = os.readlink(fixture.runtime / "current")
    require((fixture.runtime / bridge / "config/release-checkpoint.json").is_file(),
            "checkpoint-source acceptance requires a sealed checkpoint-reader candidate")
    checkpoint = fixture.config / "release-transition/v2/history-checkpoint.json"
    projection = fixture.config / "release-transition/v2/ledger.json"
    retained_projection = projection.read_bytes()
    projection_epochs = {domain: state["epoch"] for domain, state in
                         json.loads(retained_projection)["domains"].items()}
    require(projection_epochs["settings"] == 2 and
            {domain: state["epoch"] for domain, state in
             json.loads(checkpoint.read_text())["domains"].items()} == projection_epochs,
            "initial checkpoint does not agree with the released caller's epoch-2 projection")

    # This rollback uses the actual released caller and target. It is safe only
    # while the authoritative checkpoint and its frozen projection still agree.
    fixture.update("--rollback", "--yes")
    require(os.readlink(fixture.runtime / "current") == fixture.initial,
            "released caller could not roll back at matching checkpoint/projection epochs")
    require(projection.read_bytes() == retained_projection,
            "matching-epoch rollback changed the pinned legacy projection")
    before = snapshot(fixture.config)
    rollback_links = tuple(os.readlink(fixture.runtime / name) for name in ("current", "previous"))
    bridge_inspection = json.loads(fixture.run([
        str(fixture.runtime / bridge / "bin/yard"), "update", "--check", "--version", version,
    ]))
    bridge_outcome = bridge_inspection["outcome"]
    require(bridge_outcome["status"] in {"ready", "migration-required", "recovering"} and
            bridge_outcome["active"] == fixture.initial.removeprefix("releases/") and
            bridge_outcome["target"] == bridge.removeprefix("releases/") and
            bridge_inspection.get("plan", "").startswith("plan-v1-") and
            bridge_inspection.get("resume") is None,
            f"retained bridge cannot assess a fresh forward plan after released-caller rollback: {bridge_outcome}")
    require(snapshot(fixture.config) == before and
            tuple(os.readlink(fixture.runtime / name) for name in ("current", "previous")) == rollback_links,
            "retained bridge inspection changed the frozen rollback journal or runtime links")

    # Build a future owner from exactly the invoking (normally frozen) public
    # source. Only this private copy gains a synthetic settings migration.
    source = Path(__file__).resolve().parent.parent
    future_source = root / f"checkpoint-future-source-{baseline_version}"
    selected = run_process(["bash", str(source / "tests/helpers/source-files.sh"), str(source)],
                           env=None, timeout=30)
    require(selected.returncode == 0, "cannot select public inputs for the synthetic future owner")
    for relative in filter(None, selected.stdout.split("\0")):
        path = source / relative
        require(path.is_file() and not path.is_symlink(),
                "synthetic future source must contain only regular public files")
        destination = future_source / relative
        destination.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(path, destination)
    registry_path = future_source / "config/release-transition.json"
    registry = json.loads(registry_path.read_text())
    require(registry["currentEpochs"] == projection_epochs,
            "synthetic future source does not match the supplied bridge registry")
    registry["currentEpochs"]["settings"] = 3
    registry["migrations"].append({
        "id": "canonicalize-test-vms-settings-v3", "domain": "settings",
        "fromEpoch": 2, "toEpoch": 3, "kind": "test-vms-settings-v1-to-v2",
    })
    registry_path.write_text(json.dumps(registry, indent=2) + "\n")
    major, minor, patch = version.split("+", 1)[0].split("-", 1)[0].split(".")
    future_version = f"{major}.{minor}.{int(patch) + 1}-checkpoint-future"
    future_release = root / f"checkpoint-future-release-{baseline_version}"
    future_release.mkdir()
    for path in release.iterdir():
        if path.is_file():
            shutil.copy2(path, future_release / path.name)
    built = run_process(["bash", str(future_source / "dev/package-engine.sh"),
                         "--version", future_version, "--arch", arch,
                         "--output-dir", str(future_release)], env=None, timeout=180)
    require(built.returncode == 0,
            f"cannot package the synthetic sealed future owner: {built.stdout}{built.stderr}")
    fixture.env["YARD_RELEASE_BASE_URL"] = future_release.as_uri()

    def links():
        return tuple(os.readlink(fixture.runtime / name) for name in ("current", "previous"))

    before, prior_links = snapshot(fixture.config), links()
    inspection = json.loads(fixture.update("--check", "--version", future_version))
    outcome = inspection["outcome"]
    # The released caller projects the public outcome, not the newer owner's
    # complete blocker inventory. Assert its supported diagnostic contract.
    require(outcome["status"] == "operator-action-required" and
            outcome["code"] == "rollback-incompatible" and
            "verified checkpoint-reader bridge" in outcome["retry"],
            "released caller omitted the checkpoint-reader bridge prerequisite")
    require(snapshot(fixture.config) == before and links() == prior_links,
            "blocked future inspection changed protected state or runtime links")
    refused = run_process([str(fixture.old_launcher), "update", "--version", future_version, "--yes"],
                          fixture.env, timeout=180)
    require(refused.returncode != 0 and
            "verified checkpoint-reader bridge" in refused.stdout + refused.stderr and
            snapshot(fixture.config) == before and links() == prior_links,
            "released caller advanced checkpoint history from an unaware retained source")

    # No new intermediate published baseline is invented: the exact supplied
    # epoch-2 candidate is the reader bridge before checkpoint history advances.
    fixture.update("--version", version, "--yes")
    fixture.complete()
    require(os.readlink(fixture.runtime / "current") == bridge,
            "released caller did not install the exact assessed reader bridge")
    fixture.update("--version", future_version, "--yes")
    fixture.version = future_version
    fixture.complete(expected_previous=bridge)
    require(projection.read_bytes() == retained_projection and
            json.loads(checkpoint.read_text())["domains"]["settings"]["epoch"] == 3,
            "future owner did not preserve projection epoch 2 while advancing compact history to epoch 3")

    before, prior_links = snapshot(fixture.config), links()
    for old in (False, True):
        launcher = fixture.old_launcher if old else fixture.runtime / "current/bin/yard"
        checked = run_process([str(launcher), "update", "--rollback", "--check"],
                              fixture.env, timeout=180)
        if checked.returncode == 0:
            outcome = json.loads(checked.stdout)["outcome"]
            require(outcome["status"] == "operator-action-required" and
                    outcome["code"] == "rollback-incompatible",
                    "caller reported epoch-2 rollback eligible after authoritative epoch 3")
        else:
            require(any(word in (checked.stdout + checked.stderr).lower()
                        for word in ("rollback", "checkpoint")),
                    "rollback inspection failed without its compatibility diagnostic")
        refused = run_process([str(launcher), "update", "--rollback", "--yes"],
                              fixture.env, timeout=180)
        require(refused.returncode != 0 and
                any(word in (refused.stdout + refused.stderr).lower()
                    for word in ("rollback", "checkpoint")) and
                snapshot(fixture.config) == before and links() == prior_links,
                "caller rolled back or changed protected state using a stale epoch-2 projection")
    print(f"PASS: released {baseline_version} caller requires the sealed reader bridge before synthetic "
          "epoch 3; both callers refuse rollback to epoch 2 and preserve the pinned projection", flush=True)


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


def verify_completed_activation_drift(fixture):
    transaction = json.loads(fixture.journal.read_text())["transaction"]
    power = Path(fixture.env["SUBYARD_POWER_RECONCILER_PATH"])
    require(not power.exists(), "power drift fixture already exists")
    # An installed non-executable helper requires activation repair. Inspection
    # does not invoke systemd or privilege authorization; never apply this fixture.
    power.write_text("")
    try:
        retained = fixture.check()
        current = fixture.check(old=False)
        if fixture.baseline_version == BASELINE:
            require(retained["outcome"]["status"] == "recovering"
                    and retained["outcome"]["code"] == "recovery-pending"
                    and retained["outcome"].get("transaction") == transaction,
                    "frozen released updater rejected completed activation drift")
        else:
            require(fixture.baseline_version == PUBLISHED_BASELINE
                    and retained["outcome"]["status"] == "migration-required"
                    and retained["outcome"]["code"] == "transition-required"
                    and retained["outcome"].get("transaction") is None,
                    "published updater did not preserve fresh activation-plan semantics")
        require(current["outcome"]["status"] == "migration-required"
                and current["outcome"].get("transaction") is None,
                "current updater did not preserve fresh activation-plan semantics")
        require(retained["plan"] == current["plan"]
                and retained["assessment"]["changed"] and current["assessment"]["changed"]
                and retained.get("resume") is None and current.get("resume") is None,
                "activation repair changed the plan or reused historical authorization")
        before = snapshot(fixture.config)
        declined = run_process([str(fixture.old_launcher), "update", "--offline", "--version", fixture.version],
                               fixture.env, timeout=180)
        require(declined.returncode != 0
                and "confirmation" in (declined.stdout + declined.stderr).lower(),
                "released updater reused historical consent for an activation repair")
        require(snapshot(fixture.config) == before and power.read_bytes() == b"",
                "unconfirmed activation repair changed protected state")
    finally:
        power.unlink()
    fixture.complete()
    print(f"PASS: released {fixture.baseline_version} updater inspects completed activation drift with a fresh plan",
          flush=True)


def verify(release, version, baseline, arch, root, baseline_version=BASELINE):
    normal = Fixture(root / f"normal-{baseline_version}", release, version, baseline, arch, baseline_version)
    require(normal.check()["outcome"]["status"] == "migration-required",
            "old updater could not inspect candidate activation")
    normal.update("--version", version, "--yes")
    normal.complete()
    verify_checkpoint_refusal(normal)
    verify_completed_activation_drift(normal)
    normal.update("--rollback", "--yes", old=False)
    require(os.readlink(normal.runtime / "current") == normal.initial, "rollback lost the old runtime")
    normal.update("--offline", "--version", version, "--yes")
    normal.complete()
    print(f"PASS: released {baseline_version} updater -> {version}, fixed point, rollback and forward retry", flush=True)

    interrupted = Fixture(root / f"interrupted-{baseline_version}", release, version, baseline, arch,
                          baseline_version)
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
    print(f"PASS: released {baseline_version} updater resumes the candidate's journal after SIGKILL", flush=True)


def verify_activation_only_recovery(release, version, baseline, arch, root):
    started = time.monotonic()
    fixture = Fixture(root / "activation-only", release, version, baseline, arch, ACTIVATION_BASELINE)
    host_settings = fixture.config / "config.env"
    host_settings.write_text("CODING_TOOL_INTEGRATIONS=codex\n")
    host_settings.chmod(0o600)
    fixture.run([str(fixture.old_launcher), "migrate", "--yes"])
    completed_source_transaction = json.loads(fixture.journal.read_text())["transaction"]
    ledger = fixture.config / "release-transition/v2/ledger.json"
    ledger_before = ledger.read_bytes()
    registration = fixture.config / "yards/unrelated/config.env"
    registration.parent.mkdir(parents=True, mode=0o700)
    registration.write_text("YARD_TEMPLATE=''\nSSH_PORT=2226\n")
    registration.chmod(0o600)
    registration_before = registration.read_bytes()

    control = root / "incus-observation.json"

    def observe(status, hold):
        payload = json.dumps({"instances": [{"project": "subyard", "name": "yard",
                                             "info": {"name": "yard", "status": status,
                                                      "type": "container", "config": {}, "devices": {}}}],
                              "holdJournal": str(fixture.journal) if hold else ""})
        control.write_text(payload)
        control.chmod(0o600)
        deadline = time.monotonic() + 10
        applied = Path(str(control) + ".applied")
        while time.monotonic() < deadline:
            if applied.exists() and applied.read_text() == payload:
                return
            time.sleep(0.02)
        require(False, "Incus fixture did not apply the requested observation")

    observe("Running", True)
    shims = fixture.root / "commands"
    shims.mkdir()
    unexpected = fixture.root / "unexpected-host-command"
    interrupted = Path(str(control) + ".held")
    # Each phase needs its own observed interruption. A prior fixture's marker
    # must never kill this session before it has published its own journal.
    interrupted.unlink(missing_ok=True)
    systemctl = shims / "systemctl"
    systemctl.write_text("#!/usr/bin/python3\nimport pathlib, sys\n"
                        f"pathlib.Path({str(unexpected)!r}).write_text('systemctl operation rejected')\n"
                        "sys.exit(99)\n")
    systemctl.chmod(0o700)
    sudo = shims / "sudo"
    sudo.write_text("#!/usr/bin/python3\nimport pathlib, sys\n"
                    "if sys.argv[1:] == ['-n', 'true']:\n    sys.exit(0)\n"
                    f"pathlib.Path({str(unexpected)!r}).write_text('sudo operation rejected')\n"
                    "sys.exit(99)\n")
    sudo.chmod(0o700)
    incus = shims / "incus"
    incus.write_text("#!/usr/bin/python3\nimport pathlib, sys\n"
                     "arguments = sys.argv[1:]\n"
                     "if arguments and arguments[0] == 'exec' and '--' in arguments:\n"
                     "    command = arguments[arguments.index('--') + 1:]\n"
                     "    if command[:3] == ['bash', '-se', '--'] and '\"state\":\"absent\"' in sys.stdin.read():\n"
                     "        print('{\"state\":\"absent\",\"actual\":\"\",\"desired\":\"\"}')\n"
                     "        sys.exit(0)\n"
                     f"pathlib.Path({str(unexpected)!r}).write_text('incus operation rejected')\n"
                     "sys.exit(99)\n")
    incus.chmod(0o700)
    fixture.env["PATH"] = str(shims) + ":/usr/bin:/bin"
    observed_config = run_process([str(fixture.old_launcher), "config", "status"], fixture.env, timeout=30)
    require(observed_config.returncode == 1 and "drift" in observed_config.stdout + observed_config.stderr,
            "synthetic config drift unavailable: " + observed_config.stdout + observed_config.stderr)

    # The fixture backend holds observation after the real engine persists its
    # journal. Interrupt only this owned session as soon as the marker appears.
    result = run_process([str(fixture.old_launcher), "migrate", "--yes"], fixture.env,
                         timeout=60, interrupt_marker=interrupted)
    rejected = unexpected.read_text() if unexpected.exists() else "none"
    require(interrupted.is_file() and result.returncode == -signal.SIGKILL,
            "published baseline did not reach the interrupted observation "
            f"(rejected: {rejected}): {result.stdout}{result.stderr}")
    journal = json.loads(fixture.journal.read_text())
    require(journal["checkpoint"] == "reconciling" and journal["steps"] == []
            and journal["releases"]["from"] == journal["releases"]["target"],
            "published baseline did not create the activation-only journal: checkpoint="
            + journal["checkpoint"] + "; steps=" + str(len(journal["steps"]))
            + "; same_release=" + str(journal["releases"]["from"] == journal["releases"]["target"]))
    require(not unexpected.exists(), "activation fixture attempted a privileged or mutating host command")
    # A stopped synthetic instance has no live config consumers. Its desired
    # settings and asset inventory remain identical to the running observation.
    observe("Stopped", False)
    blocked = json.loads(fixture.run([str(fixture.old_launcher), "migrate", "--check", "--json"]))
    require(len(blocked.get("blockers", [])) == 1
            and blocked["blockers"][0]["resource"] == "yard.unrelated"
            and blocked["blockers"][0]["message"] == "the yard template is not supported by this migration",
            "published baseline did not reproduce the sole unrelated-template blocker")
    journal_before = fixture.journal.read_bytes()
    config_before = snapshot(fixture.config)
    bundle = release / f"subyard-{version}-linux-{arch}.tar.gz"
    published = fixture.run(["bash", str(release / "subyard-install-runtime-release.sh"),
                             "--publish-only", "--runtime-root", str(fixture.runtime),
                             "--bundle", str(bundle), "--checksum", str(bundle) + ".sha256",
                             "--manifest", str(bundle) + ".manifest.json",
                             "--provenance", str(bundle) + ".provenance.json"]).strip()
    require(published.startswith(f"releases/{version}-") and "/" not in published[len("releases/"):],
            "installer returned an invalid recovery candidate identity")
    candidate = fixture.runtime / published / "bin/yard"
    # A newer caller may resume the exact old scope, but the released owner has
    # never advertised the fresh-replacement contract. Changed inputs remain
    # excluded until their exactly known original bytes are restored.
    original_host_settings = host_settings.read_bytes()
    host_settings.write_text("CODING_TOOL_INTEGRATIONS=claude\n")
    host_settings.chmod(0o600)
    excluded_before = snapshot(fixture.config)
    excluded = json.loads(fixture.run([str(candidate), "migrate", "--check", "--json"]))
    require(excluded["outcome"]["status"] == "operator-action-required"
            and snapshot(fixture.config) == excluded_before,
            "legacy owner admitted changed-scope fresh recovery")
    excluded_apply = run_process([str(candidate), "migrate", "--yes"], fixture.env, timeout=60)
    require(excluded_apply.returncode != 0 and snapshot(fixture.config) == excluded_before,
            "legacy owner replaced the original journal under new consent")
    host_settings.write_bytes(original_host_settings)
    host_settings.chmod(0o600)
    inspected = json.loads(fixture.run([str(candidate), "migrate", "--check", "--json"]))
    require(inspected["outcome"]["status"] == "recovering" and not inspected.get("blockers")
            and inspected["current"] == journal["goal"]["target"]
            and inspected["outcome"].get("transaction") == journal["transaction"],
            "standalone candidate did not inspect the original authorized resume: "
            + json.dumps({"status": inspected["outcome"]["status"],
                          "code": inspected["outcome"].get("code"),
                          "blockers": inspected.get("blockers"),
                          "current_matches": inspected["current"] == journal["goal"]["target"],
                          "transaction_matches": inspected["outcome"].get("transaction") == journal["transaction"]}))
    require(snapshot(fixture.config) == config_before and fixture.journal.read_bytes() == journal_before,
            "delegated inspection changed protected metadata or settings")
    fixture.run([str(candidate), "migrate"])
    completed = json.loads(fixture.journal.read_text())
    require(completed["checkpoint"] == "complete"
            and {key: value for key, value in completed.items() if key != "checkpoint"}
            == {key: value for key, value in journal.items() if key != "checkpoint"},
            "delegation replaced or changed the original authorized journal bindings")
    require(ledger.read_bytes() == ledger_before and registration.read_bytes() == registration_before
            and digest(fixture.retained) == fixture.retained_hash
            and fixture.project_marker.read_text() == "project data\n",
            "delegation changed retained settings, migration ledger or project data")
    completed_config = snapshot(fixture.config)
    journal_relative = str(fixture.journal.relative_to(fixture.config))
    config_changes = sorted(key for key in completed_config.keys() | config_before.keys()
                            if key != journal_relative and completed_config.get(key) != config_before.get(key))
    # Normal terminal cleanup may remove only historical migration evidence.
    historical_evidence = f"release-transition/v2/transactions/{completed_source_transaction}/evidence/"
    require(all(key.startswith(historical_evidence) and key in config_before and key not in completed_config
                for key in config_changes), "delegation changed persistent configuration beyond terminal cleanup: "
            + ", ".join(config_changes))
    require(os.readlink(fixture.runtime / "current") == fixture.initial
            and not (fixture.runtime / "previous").exists() and not unexpected.exists(),
            "delegation changed runtime links or invoked a host mutation")
    ready = json.loads(fixture.run([str(fixture.old_launcher), "migrate", "--check", "--json"]))
    require(ready["outcome"]["status"] == "ready", "original published owner cannot verify completed recovery")
    print(f"PASS: unmodified released {ACTIVATION_BASELINE} activation-only journal resumes with {version} code and original assets",
          flush=True)
    fixture.update("--version", version, "--yes")
    fixture.complete(old=False)
    require(os.readlink(fixture.runtime / "previous") == fixture.initial
            and ledger.read_bytes() == ledger_before and registration.read_bytes() == registration_before
            and not unexpected.exists(), "ordinary update changed retained state or invoked a host mutation")
    print(f"PASS: released {ACTIVATION_BASELINE} ordinary updater reaches {version} after delegated recovery "
          f"({time.monotonic() - started:.1f}s)", flush=True)


def verify_fresh_activation_recovery(release, version, baseline, arch, root):
    fixture = Fixture(root / "fresh-activation", release, version, baseline, arch)
    host_settings = fixture.config / "config.env"
    host_settings.write_text("CODING_TOOL_INTEGRATIONS=codex\n")
    host_settings.chmod(0o600)
    fixture.update("--version", version, "--yes")
    fixture.complete()
    launcher = fixture.runtime / "current/bin/yard"
    ledger = fixture.config / "release-transition/v2/ledger.json"
    ledger_before = ledger.read_bytes()
    links_before = tuple(os.readlink(fixture.runtime / name) for name in ("current", "previous"))
    control = root / "incus-observation.json"

    def observe(status, hold):
        payload = json.dumps({"instances": [{"project": "subyard", "name": "yard",
                                             "info": {"name": "yard", "status": status,
                                                      "type": "container", "config": {}, "devices": {}}}],
                              "holdJournal": str(fixture.journal) if hold else ""})
        control.write_text(payload)
        control.chmod(0o600)
        deadline = time.monotonic() + 10
        applied = Path(str(control) + ".applied")
        while time.monotonic() < deadline:
            if applied.exists() and applied.read_text() == payload:
                return
            time.sleep(0.02)
        require(False, "fresh recovery Incus observation was not applied")

    # Native observations are synthetic; any privileged or mutating host command
    # is rejected. This exercises the actual candidate engine and wire contract.
    shims = fixture.root / "commands"
    shims.mkdir()
    unexpected = fixture.root / "unexpected-host-command"
    for name in ("sudo", "systemctl"):
        shim = shims / name
        shim.write_text("#!/usr/bin/python3\nimport pathlib, sys\n"
                        "if sys.argv[0].endswith('/sudo') and sys.argv[1:] == ['-n', 'true']:\n"
                        "    sys.exit(0)\n"
                        f"pathlib.Path({str(unexpected)!r}).write_text('host mutation rejected')\n"
                        "sys.exit(99)\n")
        shim.chmod(0o700)
    incus = shims / "incus"
    incus.write_text("#!/usr/bin/python3\nimport pathlib, sys\n"
                     "arguments = sys.argv[1:]\n"
                     "if arguments and arguments[0] == 'exec' and '--' in arguments:\n"
                     "    command = arguments[arguments.index('--') + 1:]\n"
                     "    if command[:3] == ['bash', '-se', '--'] and '\"state\":\"absent\"' in sys.stdin.read():\n"
                     "        print('{\"state\":\"absent\",\"actual\":\"\",\"desired\":\"\"}')\n"
                     "        sys.exit(0)\n"
                     f"pathlib.Path({str(unexpected)!r}).write_text('incus mutation rejected')\n"
                     "sys.exit(99)\n")
    incus.chmod(0o700)
    fixture.env["PATH"] = str(shims) + ":/usr/bin:/bin"
    interrupted = Path(str(control) + ".held")
    if interrupted.exists():
        interrupted.unlink()
    observe("Running", True)
    result = run_process([str(launcher), "migrate", "--yes"], fixture.env,
                         timeout=60, interrupt_marker=interrupted)
    require(result.returncode == -signal.SIGKILL and interrupted.is_file(),
            "candidate did not interrupt activation-only reconciliation: " + result.stdout + result.stderr)
    original_bytes = fixture.journal.read_bytes()
    original = json.loads(original_bytes)
    require(original["checkpoint"] == "reconciling" and original["steps"] == []
            and not original.get("sourceIngress")
            and original["releases"]["from"] == original["releases"]["target"],
            "candidate did not publish a supported activation-only predecessor")
    observe("Stopped", False)
    unchanged = json.loads(fixture.run([str(launcher), "migrate", "--check", "--json"]))
    require(unchanged["outcome"]["status"] == "recovering" and not unchanged.get("blockers"),
            "unchanged activation inputs did not retain ordinary resume")
    host_settings.write_text("CODING_TOOL_INTEGRATIONS=claude\n")
    host_settings.chmod(0o600)
    changed_bytes = host_settings.read_bytes()
    before = snapshot(fixture.config)
    fresh = json.loads(fixture.run([str(launcher), "migrate", "--check", "--json"]))
    require(fresh["outcome"]["status"] == "recovering" and not fresh.get("blockers")
            and fresh["outcome"].get("transaction") == original["transaction"],
            "capable owner did not offer an authorized fresh assessment")
    require(snapshot(fixture.config) == before, "fresh recovery inspection mutated protected state")
    declined = run_process([str(launcher), "migrate"], fixture.env, timeout=60)
    require(declined.returncode != 0 and "confirmation" in (declined.stdout + declined.stderr).lower()
            and snapshot(fixture.config) == before,
            "fresh recovery reused old consent or changed state without consent")
    request = {"schemaVersion": 2, "mode": "inspect", "runtimeRoot": str(fixture.runtime),
               "configHome": str(fixture.config), "yard": "default", "target": original["goal"]["target"],
               "direction": "activate-target", "artifactDigest": original["artifactDigest"],
               "registryDigest": original["registryDigest"],
               "recovery": {"transaction": original["transaction"],
                            "fingerprint": hashlib.sha256(original_bytes).hexdigest()}}

    def protocol(value):
        result = run_process([str(launcher), "_release-transition"], fixture.env, timeout=60,
                             input_text=json.dumps(value))
        require(result.returncode == 0, "candidate recovery protocol failed: " + result.stdout + result.stderr)
        return json.loads(result.stdout)

    for mode, contract, schema in (("capabilities", "activation-only-replacement-v1", 1),
                                   ("lifecycle-capabilities", "activation-only-replacement-v2", 2)):
        capabilities = protocol({"schemaVersion": 2, "mode": mode})["capabilities"]
        require(capabilities == {"contract": contract, "processSchema": 2,
                                 "journalSchema": 2, "receiptSchema": schema},
                "recovery capability negotiation changed its closed contract")
    legacy_inspection = protocol(request)["inspection"]
    require(not legacy_inspection.get("blockers") and snapshot(fixture.config) == before,
            "original recovery V1 inspection semantics changed")
    request["contract"] = "activation-only-replacement-v2"
    inspection = protocol(request)["inspection"]
    require(inspection.get("resume") is None and inspection["assessment"]["changed"]
            and inspection["plan"] not in (original["authorizationPlan"], original["resumePlan"])
            and snapshot(fixture.config) == before,
            "fresh recovery did not bind a new plan without mutation")
    apply = dict(request, mode="converge", execution={"plan": original["authorizationPlan"], "authorization": ""})
    refused = protocol(apply)["outcome"]
    require(refused["code"] == "plan-stale" and snapshot(fixture.config) == before,
            "old activation authorization admitted replacement")
    apply["execution"] = {"plan": inspection["plan"], "authorization": ""}
    refused = protocol(apply)["outcome"]
    after_refusal = snapshot(fixture.config)
    changed_paths = sorted(path for path in before.keys() | after_refusal.keys()
                           if before.get(path) != after_refusal.get(path))
    require(refused["code"] == "confirmation-required" and after_refusal == before,
            "fresh recovery missing-grant guard disagreed: code=" + refused["code"]
            + "; changed_paths=" + ",".join(changed_paths))
    host_settings.write_text("CODING_TOOL_INTEGRATIONS=\n")
    host_settings.chmod(0o600)
    stale_before = snapshot(fixture.config)
    refused = protocol(apply)["outcome"]
    require(refused["code"] == "plan-stale" and snapshot(fixture.config) == stale_before,
            "stale fresh recovery assessment permitted mutation")
    host_settings.write_bytes(changed_bytes)
    host_settings.chmod(0o600)
    marker = fixture.root / "successor-observed.json"
    result = run_process([str(launcher), "migrate", "--yes"], fixture.env, timeout=60,
                         interrupt_replacement=(fixture.journal, original["transaction"], marker))
    require(result.returncode == -signal.SIGKILL and marker.is_file(),
            "fresh authorized recovery did not reach durable successor publication")
    successor = json.loads(fixture.journal.read_text())
    transaction = successor["transaction"]
    require(transaction.startswith("recovery-v2-") and transaction != original["transaction"]
            and successor["steps"] == [] and successor["checkpoint"] != "complete",
            "fresh replacement did not publish a distinct ordinary V2 successor")
    receipt_path = fixture.config / "release-transition/recovery/v2/transactions" / (transaction + ".json")
    receipt_bytes = receipt_path.read_bytes()
    receipt = json.loads(receipt_bytes)
    require(receipt["schemaVersion"] == 2 and receipt["contract"] == "activation-only-replacement-v2"
            and receipt["basePlan"]
            and (json.dumps(receipt["predecessor"], separators=(",", ":")) + "\n").encode() == original_bytes
            and receipt["replacement"] == request["recovery"]
            and receipt["ledgerFingerprint"] == hashlib.sha256(ledger_before).hexdigest(),
            "fresh recovery lost canonical predecessor or completed ledger bindings")
    require(fixture.check()["outcome"]["status"] == "recovering",
            "released V1 caller cannot inspect the ordinary V2 successor")
    fixture.update("--offline", "--version", version)
    fixture.complete()
    require(json.loads(fixture.journal.read_text())["transaction"] == transaction
            and receipt_path.read_bytes() == receipt_bytes and ledger.read_bytes() == ledger_before
            and host_settings.read_bytes() == changed_bytes
            and tuple(os.readlink(fixture.runtime / name) for name in ("current", "previous")) == links_before
            and not unexpected.exists(),
            "released-caller resume changed recovery evidence, selections, ledger or runtime links")
    fixture.update("--rollback", "--yes", old=False)
    require(os.readlink(fixture.runtime / "current") == fixture.initial,
            "fresh recovery made the retained runtime unavailable for explicit rollback")
    fixture.update("--offline", "--version", version, "--yes")
    fixture.complete()
    terminal_path = fixture.config / "release-transition/recovery/v2/archive" / (transaction + ".json")
    retained_receipt = receipt_path.read_bytes() if receipt_path.exists() else (
        json.dumps(json.loads(terminal_path.read_text())["receipt"], separators=(",", ":")) + "\n").encode()
    require(retained_receipt == receipt_bytes and ledger.read_bytes() == ledger_before
            and host_settings.read_bytes() == changed_bytes and not unexpected.exists(),
            "rollback and forward update changed protected recovery history")
    print(f"PASS: capable {version} fresh activation recovery preserves ledger and links; released {BASELINE} "
          "caller inspects and resumes V2 successor, rollback and forward", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--release-dir", type=Path, required=True)
    parser.add_argument("--version", required=True)
    parser.add_argument("--baseline-dir", type=Path,
                        help="optional directory containing the pinned official baseline assets")
    parser.add_argument("--legacy-baseline-dir", type=Path,
                        help="optional directory containing the pinned official legacy baseline assets")
    parser.add_argument("--activation-baseline-dir", type=Path,
                        help="optional directory containing the pinned activation-recovery baseline assets")
    parser.add_argument("--published-baseline-dir", type=Path,
                        help="optional directory containing the pinned published-caller baseline assets")
    parser.add_argument("--only-activation-recovery", action="store_true",
                        help="run only the published activation-only recovery regression")
    parser.add_argument("--only-checkpoint-source", action="store_true",
                        help="run only released-caller checkpoint source and future rollback compatibility")
    args = parser.parse_args()
    require(not (args.only_activation_recovery and args.only_checkpoint_source),
            "choose only one focused compatibility regression")
    arch = {"x86_64": "amd64", "aarch64": "arm64"}.get(platform.machine())
    require(platform.system() == "Linux" and arch in BASELINE_SHA256, "unsupported platform")
    release = args.release_dir.resolve()
    require(args.version not in (BASELINE, LEGACY_BASELINE, ACTIVATION_BASELINE, PUBLISHED_BASELINE),
            "candidate must differ from the released baselines")
    os.umask(0o077)
    # Protected runtime roots reject writable workspace ancestors. All mutable
    # state and deliberate process interruption are confined to this owned root.
    with tempfile.TemporaryDirectory(prefix="subyard-release-compat-", dir="/tmp") as directory:
        root = Path(directory)
        # A retained yard registration needs a reachable API proving absence;
        # a missing transport cannot establish the profile activation state.
        source = Path(__file__).resolve().parent.parent
        server_binary = root / "empty-incus"
        built = run_process(["go", "-C", str(source), "build", "-buildvcs=false",
                             "-o", str(server_binary), "./internal/testkit/cmd/empty-incus"],
                            env=None, timeout=180)
        require(built.returncode == 0, f"cannot build Incus fixture: {built.stdout}{built.stderr}")
        observation = root / "incus-observation.json"
        observation.write_text("{}")
        with subprocess.Popen([str(server_binary), str(root), str(observation)], stdout=subprocess.PIPE,
                              stderr=subprocess.STDOUT, text=True) as server:
            try:
                readable, _, _ = select.select([server.stdout], [], [], 10)
                require(readable and server.stdout.readline().strip() == "ready",
                        "Incus fixture did not become ready")
                if not args.only_activation_recovery and not args.only_checkpoint_source:
                    legacy = baseline_assets(root, args.legacy_baseline_dir, LEGACY_BASELINE,
                                             LEGACY_BASELINE_SHA256, arch)
                    verify_legacy(release, args.version, legacy, arch, root)
                if not args.only_activation_recovery:
                    for baseline_version, hashes, directory in (
                            (BASELINE, BASELINE_SHA256, args.baseline_dir),
                            (PUBLISHED_BASELINE, PUBLISHED_BASELINE_SHA256, args.published_baseline_dir)):
                        baseline = baseline_assets(root, directory, baseline_version, hashes, arch)
                        if not args.only_checkpoint_source:
                            verify(release, args.version, baseline, arch, root, baseline_version)
                        verify_checkpoint_source(release, args.version, baseline, arch, root, baseline_version)
                        if baseline_version == BASELINE and not args.only_checkpoint_source:
                            verify_fresh_activation_recovery(release, args.version, baseline, arch, root)
                    if args.only_checkpoint_source:
                        return
                activation = baseline_assets(root, args.activation_baseline_dir, ACTIVATION_BASELINE,
                                             ACTIVATION_BASELINE_SHA256, arch)
                verify_activation_only_recovery(release, args.version, activation, arch, root)
            finally:
                server.terminate()
                try:
                    server.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    server.kill()
                    server.wait()


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
