#!/usr/bin/env python3
"""Host-free controls; all files/processes belong to synthetic /tmp fixtures."""
import base64
import contextlib
import importlib.util
import io
import json
import os
import stat
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile
import unittest
from unittest import mock

HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("capture", HERE / "publication-capture.py")
capture = importlib.util.module_from_spec(spec)
spec.loader.exec_module(capture)
SOURCE = (HERE / "proxy-address-boot.sh").read_text()

def records(content=b"original protected bytes\n"):
    result = {name: ({"status": "absent"}, None) for name in capture.LABELS}
    marker = b"subyard-p0-power-systemd-1\n"
    result["state-0"] = {"status": "present", "size": len(marker), "sha256": capture.digest(marker)}, marker
    result["state-5"] = ({"status": "present", "size": len(content),
                         "sha256": capture.digest(content)}, content)
    return result

def framed(items):
    data, complete = capture.make_archive(items)
    stream = io.BytesIO()
    capture.emit(data, complete, stream)
    return stream.getvalue()

def function(name, end):
    start = SOURCE.index(name + "() {")
    return SOURCE[start:SOURCE.index(end, start)]

def framed_from(data, complete):
    stream = io.BytesIO()
    capture.emit(data, complete, stream)
    return stream.getvalue()

class CaptureControls(unittest.TestCase):

    @contextlib.contextmanager
    def installed_mode(self):
        with mock.patch.multiple(capture, INSTALLED=capture.INSTALLED, LABELS=capture.LABELS, LOG_LABELS=capture.LOG_LABELS):
            capture.installed_profile()
            yield

    def installed_fixture(self):
        root = self.root / "subyard-rust-owner-1"
        fixture = root / "success"
        for path in (root, fixture, fixture / "config", fixture / "data", fixture / "data/runtime"):
            path.mkdir(mode=0o700)
            path.chmod(0o700)
        (root / ".subyard-p0-marker").write_bytes(b"subyard-rust-owner-1\n")
        (fixture / ".subyard-p0-marker").write_bytes(b"subyard-rust-owner-1:success\n")
        (root / ".subyard-p0-marker").chmod(0o600)
        (fixture / ".subyard-p0-marker").chmod(0o600)
        return root, fixture

    def installed_records(self):
        items = {name: ({"status": "absent"}, None) for name in capture.LABELS}
        for name, raw in (("state-0", b"subyard-rust-owner-1\n"),
                          ("state-1", b"subyard-rust-owner-1:success\n"),
                          ("log-15", b"original\x00graph failure\xff")):
            items[name] = ({"status": "present", "size": len(raw), "sha256": capture.digest(raw)}, raw)
        return items

    def test_installed_bootstrap_failure_preserves_outer_capture_streams(self):
        source = (HERE / "release-owner-installed-graph.sh").read_text()
        start = source.index("    # Bootstrap the real current owner")
        block = source[source.index("\n", start) + 1:
                       source.index('    run_current_partial_graph "$target_id"', start)]
        script = ('set -eu\nFIXTURE_ROOT="$1"\nTARGET_ASSETS=unused\nTARGET_VERSION=1.2.3\n'
                  'install_graph_bundle() { return 37; }\n'
                  'trap \'rc=$?; printf "CAPTURE\n"; printf "OUTCOME=%s\n" "$rc" >&2; exit "$rc"\' EXIT\n'
                  + "run_bootstrap() {\n" + block + "\n}\nrun_bootstrap\n")
        result = subprocess.run(["bash", "-c", script, "bootstrap", str(self.root)],
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, cwd=self.root)
        self.assertEqual(result.returncode, 37)
        self.assertEqual(result.stdout, b"CAPTURE\n")
        self.assertEqual(result.stderr, b"OUTCOME=37\n")
        self.assertNotIn(b"CAPTURE", (self.root / "partial-setup.log").read_bytes())

    def test_installed_prepare_failure_preserves_outer_capture_streams(self):
        source = (HERE / "release-owner-installed-graph.sh").read_text()
        start = source.index("run_current_partial_graph() {")
        run = source[start:source.index("main_installed_graph() {", start)]
        script = ('set -eu\nFIXTURE_ROOT="$1"\n'
                  'fixture_env() { env "$@"; }\n'
                  'prepare_current_partial_yard() { fixture_env sh -c "exit 37" > "$FIXTURE_ROOT/partial-init.log" 2>&1; printf unreachable; }\n'
                  'cleanup() { rc=$?; trap - EXIT; printf "CAPTURE\n"; printf "OUTCOME=%s\n" "$rc" >&2; exit "$rc"; }\n'
                  'trap cleanup EXIT\n'
                  + run + '\nmain() { run_current_partial_graph target; }\nmain\n')
        result = subprocess.run(["bash", "-c", script, "prepare", str(self.root)],
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, cwd=self.root)
        self.assertEqual(result.returncode, 37)
        self.assertEqual(result.stdout, b"CAPTURE\n")
        self.assertEqual(result.stderr, b"OUTCOME=37\n")
        self.assertEqual((self.root / "partial-setup.log").read_bytes(), b"")

    def test_installed_full_cleanup_requires_absent_projects_and_original_unmounted_tmpfs(self):
        source = (HERE / "release-owner-installed-graph.sh").read_text()
        cleanup = source[source.index("cleanup_installed_graph() {"):source.index("run_current_partial_graph() {")]
        recovery = (HERE / "release-transition-v0111-recovery.sh").read_text()
        power = recovery[recovery.index("cleanup_power_runtime() {"):recovery.index("cleanup_test_vms_sink() {")]
        script = r'''set -eu
TEST_ROOT=$1; TEST_MODE=$2; STATE_ROOT=$1/state; FIXTURE_ROOT=$STATE_ROOT/success
EVIDENCE=$1/evidence; RUNTIME_ROOT=$FIXTURE_ROOT/data/runtime; FIXTURE_MARKER=marker
CLEANUP_ONLY=1; CLEANUP_ARMED=1; POWER_RECONCILER=$FIXTURE_ROOT/shared-power
POWER_UNIT=$FIXTURE_ROOT/power.service; POWER_UNIT_ENABLEMENT=$FIXTURE_ROOT/enabled; POWER_UNIT_NAME=fixture-power
TARGET_ASSETS=fixture-assets; TARGET_VERSION=1.2.3; SUBYARD_YARD=e2e-yard
assert_state_root() { [ "$TEST_MODE" != foreign-root ]; }
assert_fixture_root() { return 0; }
graph_project_exists() { case "$TEST_MODE:$1" in unknown-project:*) return 2;; foreign-project:subyard-test-yard) return 0;; *) return 1;; esac; }
mark_product_project() { return 1; }
partial_mount_identity() { printf 'check\n' >> "$TEST_ROOT/events"; if [ "$TEST_MODE" = bad-mount ]; then printf changed; else printf original; fi; }
sudo() {
  [ "$1" = -n ]; shift
  if [ "$1" = umount ]; then printf 'unmount\n' >> "$TEST_ROOT/events"; rm "$TEST_ROOT/mounted"; else "$@"; fi
}
fixture_env() {
  if [ "$4" = "$RUNTIME_ROOT/releases/legacy/bin/yard" ]; then "$@"; return; fi
  [ "$SUBYARD_YARD" = e2e-yard ] && [ "$*" = "timeout --kill-after=15s 180s $RUNTIME_ROOT/releases/1.2.3-target/bin/yard -Y default teardown --full --yes" ] || return 89
  [ "$(readlink "$RUNTIME_ROOT/current")" = releases/legacy ] && [ "$(readlink "$RUNTIME_ROOT/previous")" = releases/previous ] || return 90
  printf 'full\n' >> "$TEST_ROOT/events"
  [ ! -e "$TEST_ROOT/mounted" ] || return 88
  [ "$TEST_MODE" != full-failure ] || return 37
  rm "$POWER_RECONCILER"; rm -rf "$RUNTIME_ROOT"
}
install_graph_bundle() {
  [ "$*" = 'fixture-assets 1.2.3 --publish-only' ] || return 89
  printf 'publish\n' >> "$TEST_ROOT/events"
  [ "$TEST_MODE" != publish-failure ] || return 38
  if [ "$TEST_MODE" = invalid-target ]; then printf 'releases/foreign'; else printf 'releases/1.2.3-target'; fi
}
cleanup_test_vms_sink() { printf 'sink\n' >> "$TEST_ROOT/events"; }
prepare_operator_user_bus() { return 0; }
systemctl() { case "$*" in *LoadState*) printf not-found;; *ActiveState*) printf inactive;; *) printf 0;; esac; }
''' + power + cleanup + '\ncleanup_installed_graph\n'
        for mode in ('complete', 'foreign-root', 'foreign-project', 'unknown-project', 'bad-mount', 'full-failure', 'publish-failure', 'invalid-target'):
            with self.subTest(mode=mode):
                root = self.root/mode
                fixture = root/'state/success'
                binary = fixture/'data/runtime/releases/1.2.3-target/bin/yard'
                binary.parent.mkdir(parents=True, mode=0o700)
                binary.write_text('#!/bin/sh\nexit 0\n'); binary.chmod(0o700)
                legacy = fixture/'data/runtime/releases/legacy/bin/yard'
                legacy.parent.mkdir(parents=True, mode=0o700)
                legacy.write_text('#!/bin/sh\nprintf \'unknown teardown argument "--full"\\n\' >&2\nexit 2\n'); legacy.chmod(0o700)
                (fixture/'data/runtime/current').symlink_to('releases/legacy')
                (fixture/'data/runtime/previous').symlink_to('releases/previous')
                (fixture/'partial-mount.json').write_text('original\n')
                (fixture/'shared-power').write_text('owned shared power')
                (root/'mounted').touch()
                result = subprocess.run(['bash', '-c', script, 'cleanup', str(root), mode],
                                        stdout=subprocess.PIPE, stderr=subprocess.PIPE, cwd=self.root)
                self.assertEqual(result.returncode, 0 if mode == 'complete' else 3)
                events = (root/'events').read_text().splitlines() if (root/'events').exists() else []
                self.assertEqual(events, {'complete': ['check', 'unmount', 'publish', 'full', 'sink'],
                                         'bad-mount': ['check'], 'full-failure': ['check', 'unmount', 'publish', 'full'],
                                         'publish-failure': ['check', 'unmount', 'publish'], 'invalid-target': ['check', 'unmount', 'publish']}.get(mode, []))
                self.assertEqual((root/'state').exists(), mode != 'complete')
                if mode != 'complete':
                    self.assertEqual(os.readlink(fixture/'data/runtime/current'), 'releases/legacy')
                    self.assertEqual(os.readlink(fixture/'data/runtime/previous'), 'releases/previous')
                phase = {'foreign-project': 'project-remains exit=0', 'unknown-project': 'project-presence exit=2',
                         'bad-mount': 'mount-identity-or-prerequisite exit=1', 'full-failure': 'full-teardown exit=37',
                         'publish-failure': 'full-publish exit=38', 'invalid-target': 'full-target exit=1'}.get(mode)
                if phase:
                    self.assertIn(('cleanup phase='+phase).encode(), (root/'evidence/fallback-cleanup.log').read_bytes())

    def test_installed_completed_bridge_requires_exact_ready_and_own_head(self):
        source = (HERE / "release-owner-installed-graph.sh").read_text()
        block = source[source.index("assert_source_ready() {"):source.index("main_installed_graph() {")]
        head = {"owner-registration": 2, "power-metadata": 2, "project-state": 1, "settings": 3}
        registry = json.dumps({"currentEpochs": head}).encode()
        manifest = b"original source manifest\n"
        original = {"transaction": "source-tx", "checkpoint": "complete", "sourceIngress": None,
                    "goal": {"target": "source", "direction": "activate-target"},
                    "releases": {"from": "source", "target": "source"},
                    "artifactDigest": capture.digest(manifest), "registryDigest": capture.digest(registry),
                    "steps": [{"checkpoint": "verified"}]}
        ready = {"current": "source", "owner": "source", "blockers": [],
                 "outcome": {"status": "ready", "reachedGoal": True, "active": "source", "target": "source"},
                 "journal": {"checkpoint": "complete", "transaction": "source-tx"},
                 "domains": [{"domain": d, "epoch": e, "requiredEpoch": e, "pending": []} for d, e in head.items()]}
        script = r"""set -eu
FIXTURE_ROOT=$1; RUNTIME_ROOT=$1/runtime; JOURNAL=$1/journal
fixture_env() { case " $* " in *' --yes '*) touch "$FIXTURE_ROOT/repairs"; return 97;; *) cat "$FIXTURE_ROOT/fixture-ready.json";; esac; }
die() { exit 2; }
""" + block + '\nassert_source_ready releases/source\n'
        for case in ("verified-steps", "zero-steps", "pending-status", "wrong-owner", "wrong-domain", "domain-pending",
                     "pending-journal", "bad-step", "wrong-transaction", "wrong-artifact", "source-ingress", "previous-link"):
            with self.subTest(case=case):
                root = self.root / case
                runtime = root / "runtime/releases/source"
                (runtime / "config").mkdir(parents=True, mode=0o700)
                (root / "runtime/current").symlink_to("releases/source")
                (runtime / "runtime-files.sha256").write_bytes(manifest)
                (runtime / "config/release-transition.json").write_bytes(registry)
                journal, observation = json.loads(json.dumps(original)), json.loads(json.dumps(ready))
                if case == "zero-steps": journal["steps"] = []
                elif case == "pending-status": observation["outcome"]["status"] = "migration-required"
                elif case == "wrong-owner": observation["owner"] = "foreign"
                elif case == "wrong-domain": observation["domains"][0]["epoch"] = 1
                elif case == "domain-pending": observation["domains"][0]["pending"] = ["unrecorded"]
                elif case == "pending-journal": journal["checkpoint"] = "reconciling"
                elif case == "bad-step": journal["steps"][0]["checkpoint"] = "applied"
                elif case == "wrong-transaction": observation["journal"]["transaction"] = "foreign"
                elif case == "wrong-artifact": journal["artifactDigest"] = "f" * 64
                elif case == "source-ingress": journal["sourceIngress"] = {"kind": "unexpected"}
                elif case == "previous-link": (root / "runtime/previous").symlink_to("releases/foreign")
                raw = json.dumps(journal)
                (root / "journal").write_text(raw)
                (root / "fixture-ready.json").write_text(json.dumps(observation))
                result = subprocess.run(["bash", "-c", script, "bridge", str(root)],
                    stdout=subprocess.PIPE, stderr=subprocess.PIPE, cwd=self.root)
                if case in ("verified-steps", "zero-steps"):
                    self.assertEqual(result.returncode, 0, result.stderr)
                else:
                    self.assertNotEqual(result.returncode, 0)
                self.assertEqual((root / "journal").read_text(), raw)
                self.assertFalse((root / "repairs").exists())

    def test_installed_old_first_admission_modes_refuse_before_fixture_setup(self):
        for option in ("--first-admission", "--first-admission-compensation"):
            with self.subTest(option=option):
                result = subprocess.run([str(HERE / "release-owner-installed-graph.sh"), option],
                    stdout=subprocess.PIPE, stderr=subprocess.PIPE, cwd=self.root)
                self.assertEqual(result.returncode, 2)
                self.assertEqual(result.stdout, b"")
                self.assertIn(b"complete the released Go v0.22.0 bridge", result.stderr)

    def test_installed_framing_binds_both_markers_and_retains_original_bytes(self):
        with self.installed_mode():
            items = self.installed_records()
            stream = framed(items)
            output = self.output / "installed"
            self.assertTrue(capture.receive(io.BytesIO(stream), output, io.BytesIO(), token="1"))
            with tarfile.open(output) as archive:
                self.assertEqual(archive.extractfile("log-15").read(), b"original\x00graph failure\xff")
            with self.assertRaises(ValueError):
                capture.receive(io.BytesIO(stream), self.output / "foreign", io.BytesIO(), token="2")
            items["state-1"] = items["state-0"]
            with self.assertRaises(ValueError):
                capture.receive(io.BytesIO(framed(items)), self.output / "bad-marker", io.BytesIO(), token="1")

    def test_installed_collector_closed_scope_marker_and_capsule_refusal(self):
        with self.installed_mode():
            root, fixture = self.installed_fixture()
            real_path = Path
            def paths(value):
                return self.root if str(value) == "/var/tmp" else real_path(value)
            with mock.patch.object(capture, "Path", side_effect=paths):
                data, complete = capture.collect_installed("1")
                self.assertTrue(complete)
                self.assertTrue(capture.validate_archive(data, token="1"))
                link = fixture / "normal-rust-graph.log"
                link.symlink_to(root / ".subyard-p0-marker")
                data, complete = capture.collect_installed("1")
                self.assertFalse(complete)
                output = self.output / "partial-installed"
                with self.assertRaises(ValueError):
                    capture.receive(io.BytesIO(framed_from(data, complete)), output, io.BytesIO(), token="1")
                self.assertTrue(output.exists())
                (fixture / ".subyard-p0-marker").write_bytes(b"foreign\n")
                with self.assertRaises(ValueError):
                    capture.collect_installed("1")

    def test_installed_partial_snapshots_survive_fixture_removal(self):
        with self.installed_mode():
            root, fixture = self.installed_fixture()
            originals = {"partial-pending-journal.json": b"original pending journal\x00\xff",
                         "partial-pending-capsule.tar": b"original selected-TX custody\x00\xff",
                         "partial-pending-facts.json": b"original physical target facts\n"}
            for name, raw in originals.items():
                (fixture / name).write_bytes(raw)
                (fixture / name).chmod(0o600)
            (fixture / "unselected-private-file").write_bytes(b"must not be exported")
            real_path = Path
            def paths(value):
                return self.root if str(value) == "/var/tmp" else real_path(value)
            with mock.patch.object(capture, "Path", side_effect=paths):
                data, complete = capture.collect_installed("1")
                self.assertTrue(complete)
                output = self.output / "original-partial"
                self.assertTrue(capture.receive(io.BytesIO(framed_from(data, complete)), output, io.BytesIO(), token="1"))
                snapshot = fixture / "partial-pending-capsule.tar"
                snapshot.unlink()
                snapshot.symlink_to(root / ".subyard-p0-marker")
                refused, complete = capture.collect_installed("1")
                self.assertFalse(complete)
                with self.assertRaises(ValueError):
                    capture.receive(io.BytesIO(framed_from(refused, complete)), self.output / "refused-partial", io.BytesIO(), token="1")
                snapshot.unlink()
                snapshot.write_bytes(b"x" * (4 * capture.MIB + 1))
                _, complete = capture.collect_installed("1")
                self.assertFalse(complete)
            shutil.rmtree(root)
            with tarfile.open(output) as archive:
                for name, raw in originals.items():
                    label = "log-" + str(capture.INSTALLED_LOGS.index(name))
                    self.assertEqual(archive.extractfile(label).read(), raw)
                self.assertNotIn(b"must not be exported", b"".join(archive.extractfile(m).read() for m in archive if m.isfile()))

    def test_installed_journal_selects_only_original_private_transaction(self):
        with self.installed_mode():
            journal = dict(schemaVersion=2, transaction="tx-original", checkpoint="reconciling",
                goal={"target": "release-target", "direction": "activate-target"}, releases={"from": "release-source", "target": "release-target"},
                authorizationPlan="plan-v1-" + "a"*64, resumePlan="resume-v1-" + "b"*64, steps=[])
            for key in ("artifactDigest", "registryDigest", "catalogDigest", "observationScope", "authorizationDigest", "intentDigest"):
                journal[key] = "c"*64
            raw = json.dumps(journal).encode()
            info = {"status": "present", "mode": 0o600, "uid": os.getuid()}
            selected = capture.installed_journal(info, raw, owner=os.getuid())
            config = self.root / "config"
            path = config / "migrations/native-custody/v1/tx-original"
            path.mkdir(mode=0o700, parents=True)
            path.chmod(0o700)
            (path / "bundle.json").write_bytes(b"original capsule")
            (path / "bundle.json").chmod(0o600)
            # Actual native publishers: nested facts, retry ordinals and staged CAS.
            names = ["bundle.000.json", "bundle.127.json", "001.restoration.json",
                     "2048.after-owner.json", "2049.recovery.json", "4096.reservation.json",
                     "001.host-power-installation.json", "001.image-cache-001.json", "001.image-cache-064.json",
                     "001.physical.json"]
            for domain, phases in (("command", ("intent", "terminal", "create-intent", "create-result")),
                                   ("operation", ("intent", "accepted", "terminal", "create-intent", "create-result"))):
                names.extend(f"001.{domain}-{index:03}-{phase}.json" for index in (1, 256) for phase in phases)
            names += ["." + name + ".pending" for name in names]
            for name in names:
                (path / name).write_bytes(("original:" + name).encode())
                (path / name).chmod(0o600)
            other = path.parent / "tx-foreign"
            other.mkdir(mode=0o700)
            (other / "secret").write_bytes(b"must never be read")
            _, data = capture.installed_capsule(config, selected)
            with tarfile.open(fileobj=io.BytesIO(data)) as archive:
                self.assertEqual(set(archive.getnames()), {"manifest.json", "custody/bundle.json"} | {"custody/" + name for name in names})
                self.assertEqual(archive.extractfile("custody/bundle.json").read(), b"original capsule")
                for name in names:
                    self.assertEqual(archive.extractfile("custody/" + name).read(), ("original:" + name).encode())
            capture.validate_installed_capsule(data, selected, os.getuid())
            for name in ("000.intent.json", "0001.intent.json", "4097.intent.json", "001.unknown.json",
                         "001.command-001-accepted.json", "001.operation-257-intent.json",
                         "001.operation-000-create-result.json", "001.image-cache-065.json", "bundle.128.json",
                         ".001.intent.json", "001.intent.json.pending", ".001.unknown.json.pending"):
                with self.subTest(refused=name):
                    self.assertFalse(capture.installed_tx_name("custody", name, set()))
                    unexpected = path / name
                    unexpected.write_bytes(b"refuse")
                    unexpected.chmod(0o600)
                    with self.assertRaises(ValueError):
                        capture.installed_capsule(config, selected)
                    unexpected.unlink()
            for kind, final in (("evidence", "step.applied.json"), ("recovery", "step.json")):
                self.assertTrue(capture.installed_tx_name(kind, final, {"step"}))
                self.assertTrue(capture.installed_tx_name(kind, "." + final + ".pending", {"step"}))
                self.assertFalse(capture.installed_tx_name(kind, "." + final, {"step"}))
                self.assertFalse(capture.installed_tx_name(kind, final, {"foreign"}))
            items = self.installed_records()
            items["journal"] = (dict(info, size=len(raw), sha256=capture.digest(raw)), raw)
            items["capsule"] = ({"status": "present", "size": len(data), "sha256": capture.digest(data)}, data)
            self.assertTrue(capture.receive(io.BytesIO(framed(items)), self.output / "original-tx", io.BytesIO(), token="1"))
            items["capsule"] = ({"status": "absent"}, None)
            with self.assertRaises(ValueError):
                capture.receive(io.BytesIO(framed(items)), self.output / "missing-tx", io.BytesIO(), token="1")
            with self.assertRaises(ValueError):
                capture.validate_installed_capsule(data, dict(selected, transaction="tx-foreign"), os.getuid())
            (path / "unexpected").write_bytes(b"refuse")
            with self.assertRaises(ValueError):
                capture.installed_capsule(config, selected)
            with self.assertRaises(ValueError):
                capture.installed_journal(dict(info, mode=0o644), raw, owner=os.getuid())
            for malformed in ([], dict(journal, authorizationPlan=42), dict(journal, intentDigest=None)):
                with self.assertRaises(ValueError):
                    capture.installed_journal(info, json.dumps(malformed).encode(), owner=os.getuid())
            journal["transaction"] = "../foreign"
            with self.assertRaises(ValueError):
                capture.installed_journal(info, json.dumps(journal).encode(), owner=os.getuid())

    def test_installed_guest_defers_cleanup_preserves_failure_and_ack_order(self):
        with self.installed_mode():
            source = (HERE / "release-owner-installed-graph.sh").read_text()
            start = source.index("cleanup_installed_graph() {")
            cleanup = source[start:source.index("main_installed_graph() {", start)]
            frame = self.root / "installed-frame"
            frame.write_bytes(framed(self.installed_records()))
            for original, capture_exit in ((7, 0), (7, 3), (0, 3)):
                trace = self.root / ("events-" + str(original) + str(capture_exit))
                script = """
CLEANUP_ONLY=0; TOKEN=1; ROOT=unused; TRACE="$1"; FRAME="$2"
python3() { cat "$FRAME"; return CAPTURE_EXIT; }
operator_yard() { printf native-teardown >> "$TRACE"; }
""".replace("CAPTURE_EXIT", str(capture_exit))
                result = subprocess.run(["bash", "-c", script + cleanup + "\n(exit " + str(original) + "); cleanup_installed_graph", "control", str(trace), str(frame)],
                    stdout=subprocess.PIPE, stderr=subprocess.PIPE, cwd=self.root)
                self.assertEqual(result.returncode, original or 3)
                self.assertIn(("scenario_exit=%s capture_exit=%s fixture_cleanup=deferred" % (original, capture_exit)).encode(), result.stderr)
                self.assertFalse(trace.exists())
            # The actual sink fsyncs file and directory before a controller may
            # invoke a separate cleanup. No byte/status surrogate grants ACK.
            events = []
            fsync = capture.os.fsync
            with mock.patch.object(capture.os, "fsync", side_effect=lambda fd: (events.append("fsync"), fsync(fd))[1]):
                self.assertTrue(capture.receive(io.BytesIO(frame.read_bytes()), self.output / "ack-installed", io.BytesIO(), token="1"))
            self.assertEqual(events, ["fsync", "fsync"])

    def test_installed_existing_fifo_wrapper_requires_complete_ack(self):
        with self.installed_mode():
            wrapper = function("proxy_run_guest", "controller_main() {").replace('publication-capture.py" receive', 'publication-capture.py" installed-receive')
            for bad in (False, True):
                frame = self.root / ("installed-bad" if bad else "installed-good")
                frame.write_bytes(b"SUBYARD_PUBLICATION_CAPTURE_BAD\n" if bad else framed(self.installed_records()))
                out = self.root / ("installed-bad-out" if bad else "installed-good-out")
                out.mkdir(mode=0o700)
                script = '''
ROOT="$1"; LOCAL_TEMP="$2"; FRAME="$3"; PROXY_CAPTURE_DIRECTORY="$4"
PROXY_CAPTURE_ENGINE=''; PROXY_CAPTURE_FAILED=0; PROXY_CAPTURE_ACK=0; PROXY_CONTROLLER_TOKEN=1
run_guest() { cat "$FRAME"; return 7; }
''' + wrapper + '''
proxy_run_guest handoff
rc=$?
printf 'installed-control rc=%s ack=%s failed=%s\n' "$rc" "$PROXY_CAPTURE_ACK" "$PROXY_CAPTURE_FAILED"
exit "$rc"
'''
                result = subprocess.run(["bash", "-c", script, "control", str(HERE.parent.parent), str(self.root), str(frame), str(out)],
                    stdout=subprocess.PIPE, stderr=subprocess.PIPE, cwd=self.root)
                self.assertEqual(result.returncode, 7)
                self.assertIn(b"ack=" + (b"0 failed=1" if bad else b"1 failed=0"), result.stdout)

    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="publication-capture-test-", dir="/tmp")
        self.root = Path(self.temporary.name)
        self.root.chmod(0o700)
        self.output = self.root / "output"
        self.output.mkdir(mode=0o700)
        self.output.chmod(0o700)

    def tearDown(self):
        self.temporary.cleanup()

    def capture_main(self, args, data=b""):
        source = io.BytesIO(data)
        output, diagnostic = io.BytesIO(), io.StringIO()
        with mock.patch.object(capture.sys, "argv", ["publication-capture.py", *args]), \
                mock.patch.object(capture.sys, "stdin", mock.Mock(buffer=source)), \
                mock.patch.object(capture.sys, "stdout", mock.Mock(buffer=output)), \
                contextlib.redirect_stderr(diagnostic):
            result = capture.main()
        return result, source.tell(), output.getvalue(), diagnostic.getvalue()

    def test_refusal_diagnostics_are_fixed_and_receiver_still_drains(self):
        data = b"x" * 65537 + b"\nremaining private payload\n"
        result, consumed, output, diagnostic = self.capture_main(
            ["receive", str(self.output / "oversized"), "", "1"], data)
        self.assertEqual((result, consumed, output), (3, len(data), b""))
        self.assertRegex(diagnostic, r"^publication capture refused or incomplete stage=1 reason=1[0-9]{2}\n$")
        self.assertNotIn("private", diagnostic)
        self.assertFalse((self.output / "oversized").exists())
        result, _, _, diagnostic = self.capture_main(["collect", "unused", "invalid", "unused"])
        self.assertEqual(result, 3)
        self.assertRegex(diagnostic, r"^publication capture refused or incomplete stage=4 reason=1[0-9]{2}\n$")
        self.assertNotIn("invalid", diagnostic)
        for error, code in ((ValueError("private payload"), 1), (OSError("private path"), 2),
                            (KeyError("private key"), 3)):
            with self.subTest(code=code), mock.patch.object(capture, "collect", side_effect=error):
                result, _, _, diagnostic = self.capture_main(["collect", "unused", "1", "unused"])
                self.assertEqual(result, 3)
                self.assertEqual(diagnostic, f"publication capture refused or incomplete stage=4 reason={code}\n")

    def test_diagnostics_preserve_complete_and_partial_frame_outcomes(self):
        items = records()
        data, complete = capture.make_archive(items)
        with mock.patch.object(capture, "collect", return_value=(data, complete)):
            result, _, output, diagnostic = self.capture_main(["collect", "unused", "1", "unused"])
        self.assertEqual((result, output, diagnostic), (0, framed_from(data, complete), ""))
        items["instance"] = ({"status": "refused"}, None)
        data, complete = capture.make_archive(items)
        with mock.patch.object(capture, "collect", return_value=(data, complete)):
            result, _, output, diagnostic = self.capture_main(["collect", "unused", "1", "unused"])
        self.assertEqual((result, output), (3, framed_from(data, complete)))
        self.assertEqual(diagnostic, "publication capture refused or incomplete stage=5 reason=8\n")
        result, consumed, output, diagnostic = self.capture_main(
            ["receive", str(self.output / "partial-diagnostic"), "", "1"], output)
        self.assertEqual(result, 3)
        self.assertTrue((self.output / "partial-diagnostic").exists())
        self.assertRegex(diagnostic, r"^publication capture refused or incomplete stage=1 reason=1[0-9]{2}\n$")

    def test_framed_status_type_refuses_without_traceback_and_drains_producer(self):
        items = records()
        manifest = {name: info for name, (info, _) in items.items()}
        manifest["manager"] = {"status": []}
        raw = io.BytesIO()
        with tarfile.open(fileobj=raw, mode="w:gz") as archive:
            for name, body in [("manifest.json", json.dumps(manifest).encode())] + [
                    (name, body) for name, (_, body) in items.items() if body is not None]:
                entry = tarfile.TarInfo(name)
                entry.size, entry.mode = len(body), 0o600
                archive.addfile(entry, io.BytesIO(body))
        # A full pipe's worth of trailing producer output must reach passthrough;
        # early close/traceback cannot masquerade as a drained refusal.
        tail = b"producer drained\n" * 32768
        output = self.output / "malformed-status-private-path"
        result = subprocess.run(["python3", "-B", str(HERE / "publication-capture.py"),
                                 "receive", str(output), "", "1"],
                                input=framed_from(raw.getvalue(), True) + tail,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=10)
        self.assertEqual(result.returncode, 3)
        self.assertEqual(result.stdout, tail)
        self.assertEqual(result.stderr, b"publication capture refused or incomplete stage=1 reason=4\n")
        self.assertFalse(output.exists())

    def test_decode_refusals_drain_real_producer_preserve_exit_and_never_ack(self):
        def manifest_frame(body):
            raw = io.BytesIO()
            with tarfile.open(fileobj=raw, mode="w:gz") as archive:
                entry = tarfile.TarInfo("manifest.json")
                entry.size, entry.mode = len(body), 0o600
                archive.addfile(entry, io.BytesIO(body))
            return framed_from(raw.getvalue(), True)

        valid = framed_from(b"a", True)
        cases = (
            ("invalid-data", valid.replace(b"DATA YQ==", b"DATA !"), "base64 decode"),
            ("truncated-padding", valid.replace(b"DATA YQ==", b"DATA YQ="), "base64 decode"),
            ("manifest-json", manifest_frame(b'{"private":'), "manifest decode"),
            ("manifest-unicode", manifest_frame(b"\xff"), "manifest decode"),
        )
        wrapper = function("proxy_run_guest", "controller_main() {")
        for name, wire, reason in cases:
            with self.subTest(name=name):
                frame = self.root / (name + "-frame")
                # More than a pipe buffer proves early refusal still drains the
                # producer; successful cat is recorded before its original exit7.
                frame.write_bytes(wire + b"producer drained\n" * 32768)
                out = self.root / (name + "-out")
                out.mkdir(mode=0o700)
                drained = self.root / (name + "-producer-exit")
                script = """
ROOT="$1"; LOCAL_TEMP="$2"; FRAME="$3"; PROXY_CAPTURE_DIRECTORY="$4"; DRAINED="$5"
PROXY_CAPTURE_ENGINE=''; PROXY_CAPTURE_FAILED=0; PROXY_CONTROLLER_TOKEN=1
run_guest() { cat "$FRAME"; printf '%s\\n' "$?" > "$DRAINED"; return 7; }
""" + wrapper + """
proxy_run_guest handoff
rc=$?
printf 'CONTROL rc=%s ack=%s failed=%s\\n' "$rc" "$PROXY_CAPTURE_ACK" "$PROXY_CAPTURE_FAILED"
exit "$rc"
"""
                result = subprocess.run(["bash", "-c", script, "decode-control", str(HERE.parent.parent),
                    str(self.root), str(frame), str(out), str(drained)], stdout=subprocess.PIPE,
                    stderr=subprocess.PIPE, cwd=self.root, timeout=10)
                self.assertEqual(result.returncode, 7)
                self.assertEqual(drained.read_text(), "0\n")
                self.assertIn(b"CONTROL rc=7 ack=0 failed=1", result.stdout)
                code = 100 + capture.REFUSAL_REASONS.index(reason)
                self.assertIn(f"publication capture refused or incomplete stage=1 reason={code}\n".encode(), result.stderr)
                self.assertNotIn(b"Traceback", result.stderr)
                self.assertNotIn(b"private", result.stderr)
                self.assertNotIn(str(frame).encode(), result.stderr)
                self.assertFalse((out / "handoff.capture.tar.gz").exists())
                retained = out / "handoff.capture.wire"
                self.assertEqual(retained.read_bytes(), frame.read_bytes())
                self.assertEqual(stat.S_IMODE(retained.stat().st_mode), 0o600)

    def test_shared_guest_transport_keeps_framed_stdout_separate(self):
        frame = self.root / "transport-frame"
        frame.write_bytes(framed(records(os.urandom(131072))))
        self.assertGreater(frame.stat().st_size, 131072)
        emitter = self.root / "transport-emitter.py"
        emitter.write_text("""import pathlib,sys
raw=pathlib.Path(sys.argv[1]).read_bytes()
split=raw.index(b'DATA ')+len(b'DATA ')+43
sys.stdout.buffer.write(raw[:split]);sys.stdout.buffer.flush()
sys.stderr.buffer.write(b'E2E_PUBLICATION_OUTCOME workload_exit=23 capture_exit=0 fixture_cleanup=deferred\\n');sys.stderr.buffer.flush()
sys.stdout.buffer.write(raw[split:]);sys.stdout.buffer.flush()
raise SystemExit(23)
""")
        wrapper = function("proxy_run_guest", "controller_main() {")
        for raw in (False, True):
            with self.subTest(raw=raw):
                out = self.root / ("transport-raw" if raw else "transport-merged")
                out.mkdir(mode=0o700)
                selected = wrapper if raw else wrapper.replace('run_guest --raw-stdout "$@"', 'run_guest "$@"')
                script = r'''
SUBYARD_E2E_STATE_DIR="$2/client"
. "$1/dev/agent-e2e.sh"
unset E2E_GUEST_TIMEOUT
ROOT="$1"; LOCAL_TEMP="$2"; FRAME="$3"; PROXY_CAPTURE_DIRECTORY="$4"; EMITTER="$5"
PROXY_CAPTURE_ENGINE=''; PROXY_CAPTURE_FAILED=0; PROXY_CONTROLLER_TOKEN=1
PROXY_CAPTURE_RECEIVER_PID=''; PROXY_CAPTURE_WIRE_PID=''
# The actual shared prepare/write/guest/phase helpers run. Only SSH's remote
# filesystem replies and final command are synthetic; no host/VM is contacted.
ssh() {
  local command="${!#}"
  case "$command" in
    findmnt*) printf 'ext4\n' ;;
    mktemp*) printf '/var/tmp/subyard-worktree.capture-control\n' ;;
    dd*) cat >/dev/null ;;
    sha256sum*) printf 'expected  worktree.tar.gz\n' ;;
    mkdir*|tar*|chmod*) return 0 ;;
    /var/tmp/subyard-worktree.capture-control/run.sh) python3 "$EMITTER" "$FRAME" ;;
    *) return 91 ;;
  esac
}
''' + selected + r'''
rc=0
proxy_run_guest handoff 1 "$FRAME" expected true || rc=$?
printf 'CONTROL rc=%s ack=%s failed=%s receiver=%s wire_pid=%s jobs=%s\n' "$rc" "$PROXY_CAPTURE_ACK" "$PROXY_CAPTURE_FAILED" "$PROXY_CAPTURE_RECEIVER_PID" "$PROXY_CAPTURE_WIRE_PID" "$(jobs -pr)"
exit "$rc"
'''
                result = subprocess.run(["bash", "-c", script, "transport-control", str(HERE.parent.parent), str(self.root), str(frame), str(out), str(emitter)], stdout=subprocess.PIPE, stderr=subprocess.PIPE, cwd=self.root, timeout=10)
                self.assertEqual(result.returncode, 23, result.stderr)
                wire = out / "handoff.capture.wire"
                self.assertEqual(stat.S_IMODE(wire.stat().st_mode), 0o600)
                self.assertIn(b"receiver= wire_pid= jobs=\n", result.stdout)
                if raw:
                    self.assertTrue(wire.read_bytes() == frame.read_bytes(), "raw stdout changed")
                    self.assertIn(b"CONTROL rc=23 ack=1 failed=0", result.stdout)
                    self.assertIn(b"phase=handoff operation_exit=23 capture_exit=0 wire_exit=0", result.stderr)
                    self.assertIn(b"E2E_PUBLICATION_OUTCOME workload_exit=23", result.stderr)
                    with tarfile.open(out / "handoff.capture.tar.gz") as archive:
                        self.assertEqual(len(archive.extractfile("state-5").read()), 131072)
                else:
                    self.assertFalse(wire.read_bytes() == frame.read_bytes())
                    self.assertIn(b"CONTROL rc=23 ack=0 failed=1", result.stdout)
                    self.assertIn(b"stage=1 reason=175", result.stderr)
                    self.assertFalse((out / "handoff.capture.tar.gz").exists())

    def test_passive_manager_restore_clears_only_inactive_failure(self):
        owning = (HERE / "power-reconciler-upgrade.sh").read_text()
        start = owning.index("restore_publication_manager() {")
        restore = owning[start:owning.index("restore_host_runtime() {", start)]
        cases = (("inactive", "failed", 0, 0, ["reset-failed"]),
                 ("inactive", "inactive", 0, 0, []),
                 ("inactive", "failed", 17, 1, ["reset-failed"]),
                 ("inactive", "active", 0, 1, []),
                 ("active", "inactive", 0, 0, ["start"]),
                 ("failed", "failed", 0, 0, ["reset-failed", "start"]))
        for index, (original, current, reset_exit, expected, mutations) in enumerate(cases):
            with self.subTest(original=original, current=current, reset_exit=reset_exit):
                root = self.root / ("manager-" + str(index))
                root.mkdir(mode=0o700)
                for name, value in (("publication-enabled", "absent"),
                                    ("publication-active", original), ("current", current)):
                    path = root / name
                    path.write_text(value + "\n")
                    path.chmod(0o600)
                script = r'''
set -euo pipefail
BEFORE_ROOT="$1"; RESET_EXIT="$2"; PUBLICATION_SERVICE=subyard-publication-reconcile.service
sudo() {
  [ "$1" = -n ] && shift
  [ "$1" = systemctl ] && shift
  case "$*" in
    "is-enabled $PUBLICATION_SERVICE") printf 'not-found\n'; return 4 ;;
    "show $PUBLICATION_SERVICE -p ActiveState --value") cat "$BEFORE_ROOT/current" ;;
    "reset-failed $PUBLICATION_SERVICE")
      printf 'reset-failed\n' >> "$BEFORE_ROOT/mutations"
      [ "$RESET_EXIT" = 0 ] || return "$RESET_EXIT"
      printf 'inactive\n' > "$BEFORE_ROOT/current" ;;
    "start $PUBLICATION_SERVICE")
      printf 'start\n' >> "$BEFORE_ROOT/mutations"
      if [ "$(cat "$BEFORE_ROOT/publication-active")" = failed ]; then
        printf 'failed\n' > "$BEFORE_ROOT/current"; return 1
      fi
      printf 'active\n' > "$BEFORE_ROOT/current" ;;
    *) return 91 ;;
  esac
}
''' + restore + "\nrestore_publication_manager\n"
                result = subprocess.run(["bash", "-c", script, "restore", str(root), str(reset_exit)],
                                        stdout=subprocess.PIPE, stderr=subprocess.PIPE, cwd=self.root, timeout=5)
                self.assertEqual(result.returncode, expected, result.stderr)
                calls = root / "mutations"
                self.assertEqual(calls.read_text().splitlines() if calls.exists() else [], mutations)

    def test_project_cleanup_deletes_only_bound_isolated_images(self):
        owning = (HERE / "power-reconciler-upgrade.sh").read_text()
        helpers = owning[owning.index("capture_owned_project_images() {"):owning.index("cleanup() {", owning.index("delete_owned_project() {"))]
        first, second, extra = "a" * 64, "b" * 64, "c" * 64
        for case in ("bound", "two-images", "shared", "unknown", "feature-query-error", "unbound", "extra-image", "foreign-instance", "foreign-marker", "partial", "image-query-error", "extra-after", "feature-changed", "consumer-after"):
            with self.subTest(case=case):
                root = self.root / ("project-" + case)
                root.mkdir(mode=0o700)
                log, phase = root / "calls", root / "after"
                instances = [{"name": name, "config": {"user.subyard.managed": "true", "user.subyard.e2e": "marker", "volatile.base_image": second if case == "two-images" and name != "yard" else first}} for name in ("yard", "ready", "waiting")]
                if case == "unbound":
                    del instances[1]["config"]["volatile.base_image"]
                if case == "partial":
                    instances.pop()
                if case == "foreign-instance":
                    instances.append({"name": "foreign", "config": {}})
                images = [{"fingerprint": first}]
                if case in ("two-images", "extra-image"):
                    images.append({"fingerprint": second})
                script = r'''
set -euo pipefail
PROJECT=fixture MARKER=marker INSTANCE=yard
CASE="$1"; CALLS="$2"; AFTER="$3"; INSTANCES="$4"; IMAGES="$5"; FIRST="$6"; EXTRA="$7"
die() { printf '%s\n' "$*" >&2; exit 2; }
project_presence() { return 0; }
assert_project_owned() { [ "$CASE" != foreign-marker ] || die foreign; }
incus() {
  printf '%s\n' "$*" >> "$CALLS"
  case "$*" in
    "project get fixture features.images")
      [ "$CASE" != feature-query-error ] || return 17
      if [ "$CASE" = shared ] || { [ "$CASE" = feature-changed ] && [ -e "$AFTER" ]; }; then printf 'false\n'
      elif [ "$CASE" = unknown ]; then printf '\n'
      else printf 'true\n'; fi ;;
    "list --project fixture --format json") printf '%s\n' "$INSTANCES" ;;
    "image list --project fixture --format json")
      [ "$CASE" != image-query-error ] || return 17
      if [ "$CASE" = extra-after ] && [ -e "$AFTER" ]; then printf '[{"fingerprint":"%s"},{"fingerprint":"%s"}]\n' "$FIRST" "$EXTRA"
      else printf '%s\n' "$IMAGES"; fi ;;
    "config show yard --project fixture") return 0 ;;
    "config get yard user.subyard.managed --project fixture") printf 'true\n' ;;
    "delete yard --project fixture --force"|"delete ready --project fixture --force"|"delete waiting --project fixture --force") return 0 ;;
    "storage volume list default --project fixture --format csv -c t,n") printf 'custom,fixture-volume\n' ;;
    "storage volume delete default fixture-volume --project fixture") return 0 ;;
    "list --project fixture --format csv -c n") [ "$CASE" != consumer-after ] || printf 'foreign\n' ;;
    "image delete "*" --project fixture") return 0 ;;
    "project delete fixture") return 0 ;;
    *) return 91 ;;
  esac
}
''' + helpers + r'''
capture_owned_project_images yard ready waiting || exit 33
incus delete ready --project fixture --force
incus delete waiting --project fixture --force
: > "$AFTER"
delete_owned_project
'''
                result = subprocess.run(["bash", "-c", script, "cleanup", case, str(log), str(phase), json.dumps(instances), json.dumps(images), first, extra], stdout=subprocess.PIPE, stderr=subprocess.PIPE, cwd=self.root, timeout=5)
                calls = log.read_text().splitlines() if log.exists() else []
                deletes = [line for line in calls if line.startswith("image delete ")]
                if case in ("bound", "two-images", "shared"):
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertIn("project delete fixture", calls)
                    expected = [] if case == "shared" else ["image delete " + fingerprint + " --project fixture" for fingerprint in ([first, second] if case == "two-images" else [first])]
                    self.assertEqual(deletes, expected)
                else:
                    self.assertNotEqual(result.returncode, 0)
                    self.assertFalse(deletes)
                    self.assertNotIn("project delete fixture", calls)
                self.assertFalse(any("profile " in line or "--project default" in line for line in calls))

    def test_original_bytes_survive_fixture_deletion_and_decorations(self):
        fixture = self.root / "fixture"
        fixture.mkdir()
        original = fixture / "protected"
        original.write_bytes(b"exact\x00original\xffbytes")
        info, data = capture.read_file(original, 100)
        items = records()
        items["state-5"] = info, data
        callback = fixture / "callback-continuation.jsonl"
        callback.write_bytes(b"original callback continuation\n")
        self.assertEqual(capture.STATE_FILES[8], "callback-continuation.jsonl")
        self.assertIn("state-8", capture.LOG_LABELS)
        items["state-8"] = capture.read_file(callback, 4 * capture.MIB)
        stream = b"== e2e-vm-1 ==\n  [ .. ] ordinary transport\n" + framed(items) + b"normal tail\n"
        passthrough = io.BytesIO()
        self.assertTrue(capture.receive(io.BytesIO(stream), self.output / "capture.tar.gz", passthrough))
        shutil.rmtree(fixture)
        with tarfile.open(self.output / "capture.tar.gz") as archive:
            self.assertEqual(archive.extractfile("state-5").read(), b"exact\x00original\xffbytes")
            self.assertEqual(archive.extractfile("state-8").read(), b"original callback continuation\n")
        self.assertEqual(passthrough.getvalue(), b"== e2e-vm-1 ==\n  [ .. ] ordinary transport\nnormal tail\n")
        self.assertEqual((self.output / "capture.tar.gz").stat().st_mode & 0o777, 0o600)

    def test_actual_transport_normalizer_preserves_frames(self):
        agent = (HERE.parent / "agent-e2e.sh").read_text()
        start = agent.index("normalize_terminal_progress() {")
        normalizer = agent[start:agent.index("run_direct_ssh() {", start)]
        original = b"== e2e-vm-1 ==\nprogress\\rfinished\\r\n".replace(b"\\r", b"\r")
        original += framed(records()).replace(b"\n", b"\r\n")
        result = subprocess.run(["bash", "-c", normalizer + "\nnormalize_terminal_progress"],
            input=original, stdout=subprocess.PIPE, stderr=subprocess.PIPE, cwd=self.root, check=True)
        ordinary = io.BytesIO()
        self.assertTrue(capture.receive(io.BytesIO(result.stdout), self.output / "normalized", ordinary))
        self.assertEqual(ordinary.getvalue(), b"== e2e-vm-1 ==\nfinished\n")

    def test_symlink_special_oversized_changed_absent(self):
        path = self.root / "file"
        self.assertEqual(capture.read_file(path, 4)[0]["status"], "absent")
        path.write_bytes(b"large")
        self.assertEqual(capture.read_file(path, 4)[0]["status"], "refused")
        link = self.root / "link"
        link.symlink_to(path)
        self.assertEqual(capture.read_file(link, 10)[0]["status"], "refused")
        parent = self.root / "parent"
        parent.symlink_to(self.root, target_is_directory=True)
        self.assertEqual(capture.read_file(parent / "file", 10)[0]["status"], "refused")
        fifo = self.root / "fifo"
        os.mkfifo(fifo)
        self.assertEqual(capture.read_file(fifo, 10)[0]["status"], "refused")
        original_read = capture.os.read
        def changed(fd, size):
            result = original_read(fd, size)
            path.write_bytes(b"other")
            return result
        with mock.patch.object(capture.os, "read", changed):
            self.assertEqual(capture.read_file(path, 10)[0]["status"], "changed")

    def test_malformed_truncated_oversized_frame_and_occupied_output_refuse(self):
        valid = framed(records())
        invalid = [valid[:-20], valid.replace(b"DATA ", b"DATA !", 1),
                   valid.replace(b"DATA ", b"DATA \nSUBYARD_PUBLICATION_CAPTURE_DATA ", 1),
                   b"SUBYARD_PUBLICATION_CAPTURE_BEGIN 999999999 " + b"a" * 64 + b" 1\n",
                   valid + valid, b"x" * 65537]
        for number, data in enumerate(invalid):
            with self.subTest(number=number):
                with self.assertRaises((ValueError, OSError)):
                    capture.receive(io.BytesIO(data), self.output / str(number), io.BytesIO())
                self.assertFalse((self.output / str(number)).exists())
        occupied = self.output / "occupied"
        occupied.write_bytes(b"retained")
        with self.assertRaises(FileExistsError):
            capture.receive(io.BytesIO(valid), occupied, io.BytesIO())
        self.assertEqual(occupied.read_bytes(), b"retained")

    def test_partial_archive_retained_but_never_acknowledged(self):
        items = records()
        items["resource-0"] = {"status": "refused"}, None
        with self.assertRaises(ValueError):
            capture.receive(io.BytesIO(framed(items)), self.output / "partial", io.BytesIO())
        with tarfile.open(self.output / "partial") as archive:
            self.assertEqual(archive.extractfile("state-5").read(), b"original protected bytes\n")

    def test_malformed_present_fact_cannot_claim_complete_without_bytes(self):
        items = records()
        items["manager"] = {"status": "present"}, None
        with self.assertRaises(ValueError):
            capture.receive(io.BytesIO(framed(items)), self.output / "missing-bytes", io.BytesIO())
        self.assertFalse((self.output / "missing-bytes").exists())

    def test_reference_requires_same_retained_engine(self):
        path = self.root / "engine"
        path.write_bytes(b"known engine")
        info, data = capture.read_file(path, 100, capture.digest(b"known engine"))
        self.assertIsNone(data)
        items = records()
        items["resource-0"] = info, data
        with self.assertRaises(ValueError):
            capture.receive(io.BytesIO(framed(items)), self.output / "unbound", io.BytesIO(), "0" * 64)
        self.assertTrue(capture.receive(io.BytesIO(framed(items)), self.output / "bound",
                                        io.BytesIO(), capture.digest(b"known engine")))

    def test_diagnostic_child_output_is_bounded_and_reaped(self):
        info, data = capture.command(["python3", "-c", "import os; os.write(1,b'x'*1000000)"], 100)
        self.assertEqual(info["status"], "oversized")
        self.assertLessEqual(len(data), 101)

    def run_cleanup(self, mode, original, capture_exit=0, preserve=0, fixture=None):
        fixture = fixture or self.root / ("fixture-" + mode + str(original) + str(capture_exit))
        fixture.mkdir(exist_ok=True)
        (fixture / "protected").write_bytes(b"original")
        frame = self.root / "frame"
        frame.write_bytes(framed(records(b"original")))
        script = function("proxy_cleanup", "trap proxy_cleanup EXIT")
        prelude = """
MODE="$1"; FIXTURE="$2"; FRAME="$3"; ROOT="$4"
CLEANUP_ARMED=1; PRESERVE_FIXTURE="$5"; TOKEN=1; ADDRESS_UNIT=unused
assert_state_root() { test -f "$FIXTURE/protected"; }
sudo() { cat "$FRAME"; return "$6"; }
cleanup() {
  if [ "$PRESERVE_FIXTURE" = 1 ]; then exit 0; fi
  rm -f -- "$FIXTURE/protected"; exit 0
}
"""
        # No real sudo/provider command exists in this execution path.
        prelude = prelude.replace('return "$6"', 'return ' + str(capture_exit))
        if mode == "clean":
            prelude += "CLEANUP_ARMED=0\n"
        result = subprocess.run(["bash", "-c", prelude + script + "\n(exit " + str(original)
                + "); proxy_cleanup", "control", mode, str(fixture), str(frame), str(HERE.parent.parent),
                str(preserve)], stdout=subprocess.PIPE, stderr=subprocess.PIPE, cwd=self.root)
        return result, fixture

    def test_guest_failure_preserves_original_rc_and_bytes_before_ack(self):
        for original, capture_exit in [(7, 0), (7, 3), (0, 3)]:
            with self.subTest(original=original, capture_exit=capture_exit):
                result, fixture = self.run_cleanup("handoff", original, capture_exit)
                self.assertEqual(result.returncode, original or 3)
                self.assertEqual((fixture / "protected").read_bytes(), b"original")
                self.assertIn(b"fixture_cleanup=deferred", result.stderr)

    def test_missing_or_changed_marker_keeps_workload_exit_despite_fatal_guard(self):
        power = (HERE / "power-reconciler-upgrade.sh").read_text()
        start = power.index("assert_state_root() {")
        guard = power[start:power.index("assert_project_owned() {", start)]
        for marker_state in ("missing", "changed"):
            state = self.root / marker_state
            state.mkdir()
            if marker_state == "changed":
                (state / ".marker").write_text("different fixture\n")
            (state / "protected").write_bytes(b"uncaptured original")
            prelude = '''
STATE_ROOT="$1"; MARKER=subyard-p0-power-systemd-1
MODE=handoff; CLEANUP_ARMED=1; PRESERVE_FIXTURE=0; ROOT="$2"; TOKEN=1; ADDRESS_UNIT=unused
die() { exit 2; }
sudo() {
  # The capture helper reports a refused marker; it never owns the workload exit.
  test -f "$STATE_ROOT/.marker" && test ! -L "$STATE_ROOT/.marker" &&
    test "$(cat "$STATE_ROOT/.marker")" = "$MARKER" || return 3
}
cleanup() { rm -f -- "$STATE_ROOT/protected"; exit 0; }
'''
            args = ["control", str(state), str(HERE.parent.parent)]
            fatal = subprocess.run(["bash", "-c", prelude + guard + "\nassert_state_root", *args],
                stdout=subprocess.PIPE, stderr=subprocess.PIPE, cwd=self.root)
            self.assertEqual(fatal.returncode, 2)
            result = subprocess.run(["bash", "-c", prelude + guard
                + function("proxy_cleanup", "trap proxy_cleanup EXIT") + "\n(exit 7); proxy_cleanup", *args],
                stdout=subprocess.PIPE, stderr=subprocess.PIPE, cwd=self.root)
            self.assertEqual(result.returncode, 7)
            self.assertIn(b"workload_exit=7 capture_exit=3 fixture_cleanup=deferred", result.stderr)
            self.assertEqual(result.stdout, b"")  # No frame means no cleanup acknowledgement.
            self.assertEqual((state / "protected").read_bytes(), b"uncaptured original")

    def test_default_reboot_prepare_preserves_without_capture(self):
        result, fixture = self.run_cleanup("prepare", 0, preserve=1)
        self.assertEqual(result.returncode, 0)
        self.assertEqual(result.stdout, b"")
        self.assertTrue((fixture / "protected").exists())

    def test_handoff_and_reboot_resume_capture_then_clean_deletes(self):
        for mode in ("handoff", "resume"):
            result, fixture = self.run_cleanup(mode, 0)
            self.assertEqual(result.returncode, 0)
            output = self.output / (mode + ".tar.gz")
            self.assertTrue(capture.receive(io.BytesIO(result.stdout), output, io.BytesIO()))
            self.assertTrue((fixture / "protected").exists())
            # Invoke the actual separate-clean branch only after durable receipt.
            clean, clean_fixture = self.run_cleanup("clean", 0, fixture=fixture)
            self.assertEqual(clean.returncode, 0)
            self.assertFalse((clean_fixture / "protected").exists())

    def test_actual_fifo_wrapper_keeps_original_failure_and_rejects_bad_capture(self):
        wrapper = function("proxy_run_guest", "controller_main() {")
        for bad in (False, True):
            frame = self.root / ("bad-frame" if bad else "good-frame")
            frame.write_bytes(b"SUBYARD_PUBLICATION_CAPTURE_BAD\n" + b"tail\n" * 10000 if bad else framed(records()))
            out = self.root / ("bad-out" if bad else "good-out")
            out.mkdir(mode=0o700)
            script = """
ROOT="$1"; LOCAL_TEMP="$2"; FRAME="$3"; PROXY_CAPTURE_DIRECTORY="$4"
PROXY_CAPTURE_ENGINE=''; PROXY_CAPTURE_FAILED=0
PROXY_CONTROLLER_TOKEN=1
run_guest() { cat "$FRAME"; return 7; }
""" + wrapper + """
proxy_run_guest handoff
rc=$?
printf 'CONTROL rc=%s ack=%s failed=%s\\n' "$rc" "$PROXY_CAPTURE_ACK" "$PROXY_CAPTURE_FAILED"
exit "$rc"
"""
            result = subprocess.run(["bash", "-c", script, "wrapper", str(HERE.parent.parent),
                str(self.root), str(frame), str(out)], stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                cwd=self.root)
            self.assertEqual(result.returncode, 7)
            self.assertIn(b"CONTROL rc=7 ack=" + (b"0 failed=1" if bad else b"1 failed=0"), result.stdout)

    def test_controller_capture_refusal_skips_destructive_clean(self):
        start = SOURCE.index("  controller_cleanup() {")
        code = SOURCE[start:SOURCE.index("  trap controller_cleanup EXIT", start)]
        script = """
PROXY_CONTROLLER_PREPARED=1; PROXY_CAPTURE_ACK=0; PROXY_CAPTURE_FAILED=1
PROXY_WORKLOAD_EXIT=7; PROXY_WORKLOAD_PHASE=handoff; PROXY_FIXTURE_CLEANUP_EXIT=not-run
PROXY_CONTROLLER_BUNDLE=unused; PROXY_CONTROLLER_BUNDLE_HASH=unused; PROXY_CONTROLLER_TOKEN=1
cleanup_guest() { printf 'transport-delete\\n'; }
proxy_run_guest() { printf 'DESTRUCTIVE-CLEAN\\n'; }
cleanup_on_exit() { printf 'LEASE-RELEASE-OWNED\\n'; exit "$2"; }
""" + code + "\n(exit 7); controller_cleanup"
        result = subprocess.run(["bash", "-c", script], stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE, cwd=self.root)
        self.assertEqual(result.returncode, 7)
        self.assertNotIn(b"DESTRUCTIVE-CLEAN", result.stdout)
        self.assertIn(b"fixture_cleanup=skipped", result.stderr)
        self.assertEqual(result.stdout.count(b"LEASE-RELEASE-OWNED\n"), 1)

    def test_missing_final_frame_refuses_but_prepare_preserves(self):
        wrapper = function("proxy_run_guest", "controller_main() {")
        for role in ("prepare", "handoff", "resume", "clean"):
            out = self.root / ("missing-" + role)
            out.mkdir(mode=0o700)
            script = '''
ROOT="$1"; LOCAL_TEMP="$2"; PROXY_CAPTURE_DIRECTORY="$3"
PROXY_CAPTURE_ENGINE=''; PROXY_CAPTURE_FAILED=0; PROXY_CAPTURE_ACK=0; PROXY_CONTROLLER_TOKEN=1
run_guest() { printf 'ordinary transport decoration\\n'; return 0; }
''' + wrapper + '\nproxy_run_guest ' + role
            result = subprocess.run(["bash", "-c", script, "missing", str(HERE.parent.parent),
                str(self.root), str(out)], stdout=subprocess.PIPE, stderr=subprocess.PIPE, cwd=self.root)
            self.assertEqual(result.returncode, 0 if role == "prepare" else 3)
            self.assertFalse((out / (role + ".capture.tar.gz")).exists())

    def test_actual_controller_ack_precedes_separate_clean(self):
        wrapper = function("proxy_run_guest", "controller_main() {")
        for role in ("handoff", "resume"):
            out = self.root / ("ack-" + role)
            out.mkdir(mode=0o700)
            frame = self.root / ("ack-frame-" + role)
            frame.write_bytes(framed(records()))
            script = '''
ROOT="$1"; LOCAL_TEMP="$2"; PROXY_CAPTURE_DIRECTORY="$3"; FRAME="$4"; PHASE="$5"
PROXY_CAPTURE_ENGINE=''; PROXY_CAPTURE_FAILED=0; PROXY_CAPTURE_ACK=0; PROXY_CONTROLLER_TOKEN=1
run_guest() {
  if [ "$PHASE" = clean ]; then
    test -s "$PROXY_CAPTURE_DIRECTORY/$ORIGINAL.capture.tar.gz" || return 99
    printf 'acknowledged-clean\\n'
  else cat "$FRAME"; fi
}
''' + wrapper + '''
ORIGINAL="$PHASE"
proxy_run_guest "$PHASE" || exit $?
PHASE=clean
proxy_run_guest clean
'''
            result = subprocess.run(["bash", "-c", script, "ack", str(HERE.parent.parent),
                str(self.root), str(out), str(frame), role], stdout=subprocess.PIPE,
                stderr=subprocess.PIPE, cwd=self.root)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn(b"acknowledged-clean", result.stdout)

    def test_private_parent_total_limit_and_foreign_marker_refuse(self):
        link = self.root / "output-link"
        link.symlink_to(self.output, target_is_directory=True)
        with self.assertRaises(ValueError):
            capture.receive(io.BytesIO(framed(records())), link / "outside", io.BytesIO())
        self.assertFalse((self.output / "outside").exists())
        with mock.patch.object(capture, "TOTAL", 1):
            with self.assertRaises(ValueError):
                capture.make_archive(records())
        with self.assertRaises(ValueError):
            capture.receive(io.BytesIO(framed(records())), self.output / "foreign", io.BytesIO(), token="2")

    def test_unbundled_default_reboot_has_no_false_engine_reference(self):
        archive = self.root / "ordinary-source.tar.gz"
        with tarfile.open(archive, "w:gz"):
            pass
        self.assertEqual(capture.retained_engine(archive), "")

    def test_interrupted_receiver_is_reaped_without_destructive_clean(self):
        wrapper = function("proxy_run_guest", "controller_main() {")
        start = SOURCE.index("  controller_cleanup() {")
        cleanup = SOURCE[start:SOURCE.index("  trap controller_cleanup EXIT", start)]
        script = '''
ROOT="$1"; LOCAL_TEMP="$2"; PROXY_CAPTURE_DIRECTORY="$3"
PROXY_CAPTURE_ENGINE=''; PROXY_CAPTURE_FAILED=0; PROXY_CAPTURE_ACK=0; PROXY_CONTROLLER_TOKEN=1
PROXY_CAPTURE_RECEIVER_PID=''; PROXY_CAPTURE_SAVED_STDOUT=''
PROXY_CONTROLLER_PREPARED=1; PROXY_WORKLOAD_EXIT=unknown; PROXY_WORKLOAD_PHASE=none
PROXY_FIXTURE_CLEANUP_EXIT=not-run
cleanup_guest() { return 0; }
cleanup_on_exit() { printf 'CONTROL receiver=%s failed=%s\\n' "$PROXY_CAPTURE_RECEIVER_PID" "$PROXY_CAPTURE_FAILED"; exit "$2"; }
run_guest() { printf 'SUBYARD_PUBLICATION_CAPTURE_BEGIN 100 %064d 1\\n' 0; kill -TERM "$BASHPID"; return 143; }
''' + wrapper + cleanup + '''
trap controller_cleanup EXIT
trap 'exit 143' TERM
proxy_run_guest handoff
'''
        result = subprocess.run(["bash", "-c", script, "interrupt", str(HERE.parent.parent),
            str(self.root), str(self.output)], stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            cwd=self.root, timeout=5)
        self.assertEqual(result.returncode, 143)
        self.assertIn(b"CONTROL receiver= failed=1", result.stdout)
        self.assertIn(b"fixture_cleanup=skipped", result.stderr)

if __name__ == "__main__":
    unittest.main(verbosity=2)

