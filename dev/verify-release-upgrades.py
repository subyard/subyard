#!/usr/bin/env python3
"""Exercise the frozen update contract with an unmodified released updater."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import select
import signal
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


def run_process(command, env, timeout, input_text=None, interrupt_marker=None):
    child = subprocess.Popen(command, env=env,
                             stdin=subprocess.DEVNULL if input_text is None else subprocess.PIPE,
                             stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                             text=True, start_new_session=True)
    try:
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
        require(retained["outcome"]["status"] == "recovering"
                and retained["outcome"]["code"] == "recovery-pending"
                and retained["outcome"].get("transaction") == transaction,
                "released updater rejected completed activation drift")
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
    print(f"PASS: released {BASELINE} updater inspects completed activation drift with a fresh plan",
          flush=True)


def verify(release, version, baseline, arch, root):
    normal = Fixture(root / "normal", release, version, baseline, arch)
    require(normal.check()["outcome"]["status"] == "migration-required",
            "old updater could not inspect candidate activation")
    normal.update("--version", version, "--yes")
    normal.complete()
    verify_completed_activation_drift(normal)
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
            "published baseline did not create the activation-only journal")
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
    inspected = json.loads(fixture.run([str(candidate), "migrate", "--check", "--json"]))
    require(inspected["outcome"]["status"] == "recovering" and not inspected.get("blockers")
            and inspected["current"] == journal["goal"]["target"]
            and inspected["outcome"].get("transaction") == journal["transaction"],
            "standalone candidate did not inspect the original authorized resume")
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
    parser.add_argument("--only-activation-recovery", action="store_true",
                        help="run only the published activation-only recovery regression")
    args = parser.parse_args()
    arch = {"x86_64": "amd64", "aarch64": "arm64"}.get(platform.machine())
    require(platform.system() == "Linux" and arch in BASELINE_SHA256, "unsupported platform")
    release = args.release_dir.resolve()
    require(args.version not in (BASELINE, LEGACY_BASELINE, ACTIVATION_BASELINE),
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
                if not args.only_activation_recovery:
                    legacy = baseline_assets(root, args.legacy_baseline_dir, LEGACY_BASELINE,
                                             LEGACY_BASELINE_SHA256, arch)
                    verify_legacy(release, args.version, legacy, arch, root)
                    baseline = baseline_assets(root, args.baseline_dir, BASELINE, BASELINE_SHA256, arch)
                    verify(release, args.version, baseline, arch, root)
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
