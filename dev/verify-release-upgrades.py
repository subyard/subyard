#!/usr/bin/env python3
"""Exercise supported update contracts with unmodified released updaters."""

import argparse
import ctypes
from dataclasses import asdict, dataclass
import hashlib
import http.client
import json
import os
from pathlib import Path
import platform
import re
import runpy
import select
import signal
import socket
import shutil
import stat
import subprocess
import sys
import tarfile
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
MINIMUM_BASELINE = "0.11.0"
MINIMUM_BASELINE_SHA256 = {
    "amd64": "30ff2437219dcaaa568599a9df68a62d3f9203d66579d7ee6c802e527ca92163",
    "arm64": "cf89b7319ad9fdb31872ba5272921b84d585c3baf20ecff52056d843448d53a3",
}
RETIRED_BASELINES = {
    "0.4.1": {"amd64": "fc5b030785089039177292b8127a793defdb54644d00fdcf0d8067106e9c3700", "arm64": "04bfe62cc5964ae841a2827e260dcba6afb4d511f41f62b54c45d9c74930048f"},
    "0.4.2": {"amd64": "17608894ab0959d3eed5e1467da2174eb934683af10d8a774329f338cd82eda2", "arm64": "cac9f6791c03610ea35869875f6c1b5c013d30c0077b5530e1d6d182086b1654"},
    LEGACY_BASELINE: LEGACY_BASELINE_SHA256,
    "0.10.0": {"amd64": "e61405d6539ccc3b953be1dd24b9d7101476a4380225b866fe8bd9896ae7c659", "arm64": "ab11255d001025f9a099294b71a06c20d8c99d8b70adee536d128f85174e84f0"},
}
RETIRED_INSTALLER_SHA256 = {
    "0.4.1": "28c45b916d8eaedc5a607bf0895fb0f3caad620f475a3744beb75c9c5528724e",
    "0.4.2": "28c45b916d8eaedc5a607bf0895fb0f3caad620f475a3744beb75c9c5528724e",
    LEGACY_BASELINE: LEGACY_INSTALLER_SHA256,
    "0.10.0": LEGACY_INSTALLER_SHA256,
}

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
DIRECT_BASELINE = "0.19.0"
DIRECT_BASELINE_SHA256 = {
    "amd64": "81c55205368ef891976f28ef21b303da5ef97114d80f76c6ec60e9f3a2b698a9",
    "arm64": "5b8646eabda155eb48203eac80fba81bde2a773050a35839fc6d9216b6ec47d7",
}
MIGRATION_OWNER_BASELINE = "0.21.0"
MIGRATION_OWNER_SHA256 = {"amd64": "1fee6251856ede2c1cbd0c01f48d226fbce64915f712df97085d35abb4991282"}
SOURCE_BASELINE = "0.18.6"
SOURCE_BASELINE_SHA256 = {
    "amd64": "458950eccd3831df8d4ed059ccc51a5dece6f8a85aedaae38f863b7252f635f2",
    "arm64": "a3b52c39edfc4930f0e0ab4c697d60db6b39a00b022d344053bc4f4131fca4fa",
}


HISTORY_BASELINE = "0.19.0"
HISTORY_BASELINE_SHA256 = {"amd64": "81c55205368ef891976f28ef21b303da5ef97114d80f76c6ec60e9f3a2b698a9", "arm64": "5b8646eabda155eb48203eac80fba81bde2a773050a35839fc6d9216b6ec47d7"}
HISTORY_BASELINES = {BASELINE: BASELINE_SHA256, PUBLISHED_BASELINE: PUBLISHED_BASELINE_SHA256,
                     SOURCE_BASELINE: SOURCE_BASELINE_SHA256, HISTORY_BASELINE: HISTORY_BASELINE_SHA256}
HISTORY_EVIDENCE = None
HISTORY_RESULTS = []


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def archive_footprint(bundle):
    """Count known unpacked payload only; this is not a peak-space estimate."""
    if bundle is None or not bundle.is_file():
        return None
    entries = {}
    parents = set()
    try:
        with tarfile.open(bundle, "r:gz") as archive:
            for member in archive:
                path = Path(member.name)
                if path != Path("."):
                    entries[path] = member
                    parents.update(parent for parent in path.parents if parent != Path("."))
    except tarfile.TarError as error:
        raise RuntimeError(f"cannot inspect scratch payload {bundle.name}: {error}") from None
    size = sum(member.size for member in entries.values() if member.isfile())
    # Hard links reuse an inode; implicit parent directories still need one.
    inodes = len(parents | {path for path, member in entries.items() if not member.islnk()})
    return size, inodes


def check_scratch_capacity(candidate, baselines=(), stage="before fixture install"):
    # Selected baselines are alternatives: at least one baseline and candidate
    # coexist in a Fixture. Other retained cases and all build/output overhead
    # are unknown, rather than an invented reserve or a complete peak budget.
    candidate_size = archive_footprint(candidate)
    baseline_sizes = [archive_footprint(bundle) for bundle in baselines]
    known = [size for size in baseline_sizes if size is not None]
    byte_floor, inode_floor = candidate_size or (0, 0)
    byte_floor += max((size[0] for size in known), default=0)
    inode_floor += max((size[1] for size in known), default=0)
    available = os.statvfs("/tmp")
    available_bytes = available.f_bavail * available.f_frsize
    available_inodes = (available.f_favail if available.f_files > 0
                        and 0 <= available.f_favail <= available.f_files else None)
    unknown_assets = (candidate_size is None) + sum(size is None for size in baseline_sizes)
    print(f"Scratch capacity ({stage}): known unpacked floor {byte_floor} bytes, {inode_floor} inodes; "
          f"available {available_bytes} bytes, "
          f"{available_inodes if available_inodes is not None else 'UNKNOWN'} inodes. "
          f"UNKNOWN: {unknown_assets} unavailable archive(s), download/build/output overhead and total peak; "
          "passing this floor does not guarantee capacity.", flush=True)
    require(available_bytes >= byte_floor,
            f"insufficient /tmp bytes {stage}: available {available_bytes}, known floor {byte_floor}")
    require(available_inodes is None or available_inodes >= inode_floor,
            f"insufficient /tmp inodes {stage}: available {available_inodes}, known floor {inode_floor}")


def selected_baseline_archives(args, release, arch):
    """Name only inputs used by the selected case, without downloading them."""
    normal = [(BASELINE, args.baseline_dir), (PUBLISHED_BASELINE, args.published_baseline_dir),
              (SOURCE_BASELINE, args.source_baseline_dir)]
    activation = (ACTIVATION_BASELINE, args.activation_baseline_dir)
    direct = (DIRECT_BASELINE, args.direct_baseline_dir)
    history = [(version, args.migration_history_baselines / ("baseline-" + version)
                if args.migration_history_baselines is not None else None)
               for version in HISTORY_BASELINES
               if (args.migration_history_case in ("all", "checkpoint")
                   if version in (SOURCE_BASELINE, HISTORY_BASELINE)
                   else args.migration_history_case in ("all", "legacy", "cuts"))]
    if args.only_migration_history:
        selected = history
    elif args.only_legacy_retirement:
        selected = [(v, args.legacy_baseline_dir if v == LEGACY_BASELINE else None)
                    for v in RETIRED_BASELINES] + [(MINIMUM_BASELINE, None)]
    elif args.only_direct_019:
        selected = [direct]
    elif args.only_migration_owner_replacement:
        selected = [(MIGRATION_OWNER_BASELINE, args.migration_owner_baseline_dir), direct]
    elif args.only_fresh_activation_recovery:
        selected = normal[:1]
    elif args.only_activation_recovery:
        selected = [activation, normal[2]]
    elif args.only_runtime_recovery:
        selected = [normal[2], activation]
    elif args.only_source_repair:
        selected = [normal[2]]
    elif args.only_power_publication or args.only_power_transition_regression:
        selected = normal[1:] if args.only_power_publication else normal
        selected = [(v, directory if directory is not None else
                     release.parent / "power-publication-baselines" / v) for v, directory in selected]
    elif args.only_checkpoint_source:
        selected = normal
    else:
        selected = normal + [activation, direct] + history + [
            (v, args.legacy_baseline_dir if v == LEGACY_BASELINE else None)
            for v in RETIRED_BASELINES] + [(MINIMUM_BASELINE, None)]
    pins = {BASELINE: BASELINE_SHA256, ACTIVATION_BASELINE: ACTIVATION_BASELINE_SHA256,
            PUBLISHED_BASELINE: PUBLISHED_BASELINE_SHA256, SOURCE_BASELINE: SOURCE_BASELINE_SHA256,
            DIRECT_BASELINE: DIRECT_BASELINE_SHA256, MIGRATION_OWNER_BASELINE: MIGRATION_OWNER_SHA256,
            MINIMUM_BASELINE: MINIMUM_BASELINE_SHA256, **RETIRED_BASELINES}
    archives = []
    for version, directory in selected:
        bundle = directory.resolve() / f"subyard-{version}-linux-{arch}.tar.gz" if directory is not None else None
        if bundle is not None and bundle.is_file():
            require(digest(bundle) == pins[version][arch],
                    "official baseline checksum does not match the pinned release")
        archives.append(bundle)
    return archives


def skills_transport_contract(root):
    """Describe the current transport from explicit package assets, never shell text."""
    package = runpy.run_path(str(root / 'config/skills/package.py'))
    members = {}
    for name in package['archive_members'](root):
        path = root / name
        members[name] = ('directory', 0o700, None) if path.is_dir() else ('file', 0o600, digest(path))
    program = (root / 'config/skills/refresh.sh').read_text().rstrip('\n')
    prefix = ['exec', 'yard', '--project', 'subyard', '--env', 'DEV_USER=dev',
              '--env', 'ENVIRONMENT_PROFILES=', '--env', 'ALLOWS_CODING_TOOLS=true',
              '--', 'bash', '-eu', '-c', program, '--']
    return [(tuple(prefix + [mode]), members) for mode in ('apply', 'check')]


def write_observation_incus(shims, control, unexpected, package_roots=(), activation_failure=None, *, skills_roots=None):
    source = Path(__file__).resolve().parent.parent
    probes = {}
    host_probes = {}
    skills = {}
    for package_root in ((source, *package_roots) if skills_roots is None else skills_roots):
        if (package_root / 'config/skills/package.py').is_file():
            for command, members in skills_transport_contract(package_root):
                skills.setdefault(command, []).append(members)
    fixtures = [fixture for owner in ("agents", "profiles", "retired-integrations")
                for fixture in source.glob(f"config/{owner}/*/tests/release-observation.py")]
    for fixture in sorted(fixtures):
        provider = runpy.run_path(str(fixture))
        relative = fixture.parent.parent.relative_to(source)
        for package_root in (source, *package_roots):
            package = package_root / relative
            if not package.is_dir():
                continue
            commands = provider["read_only_commands"](package)
            for entry in commands:
                arguments, response, *status = entry
                code = status[0] if status else 0
                require(len(status) <= 1 and type(code) is int and 0 <= code <= 255,
                        "package observation fixture has invalid exit status")
                key = tuple(arguments)
                require(key not in probes or probes[key] == (response, code),
                        "package observation fixtures disagree on an exact command")
                probes[key] = (response, code)
            for program, arguments, response, code in provider.get("read_only_host_commands", lambda _: [])(package):
                require(program in ("sudo", "systemctl") and type(code) is int and 0 <= code <= 255,
                        "package host observation fixture is invalid")
                key = (program, tuple(arguments))
                require(key not in host_probes or host_probes[key] == (response, code),
                        "package host observation fixtures disagree on an exact command")
                host_probes[key] = (response, code)
    incus = shims / "incus"
    # Exact canned observations only; no command or guest script is executed.
    incus.write_text("#!/usr/bin/python3\nimport hashlib, json, pathlib, sys, tarfile\n"
                     "arguments = sys.argv[1:]\n"
                     f"probes = {probes!r}\n"
                     f"activation_failure = {str(activation_failure) if activation_failure else None!r}\n"
                     "if activation_failure and pathlib.Path(activation_failure).exists():\n"
                     "    journal = json.loads(pathlib.Path(activation_failure).read_text())\n"
                     "    state_path = pathlib.Path(journal['journal'])\n"
                     "    state = json.loads(state_path.read_text()) if state_path.exists() else {}\n"
                     "    if state.get('checkpoint') == 'reconciling' and arguments[:1] == ['exec']:\n"
                     "        pathlib.Path(journal['marker']).write_text('controlled activation observation failure after migration effects\\n')\n"
                     "        sys.stderr.write('controlled activation observation failure after migration effects\\n')\n"
                     "        sys.exit(65)\n"
                     f"skills = {skills!r}\n"
                     "if tuple(arguments) in skills:\n"
                     "    inventories = skills[tuple(arguments)]\n"
                     "    actual = {}\n"
                     "    seen = set()\n"
                     "    valid = True\n"
                     "    try:\n"
                     "        with tarfile.open(fileobj=sys.stdin.buffer, mode='r|') as archive:\n"
                     "            for member in archive:\n"
                     "                kind = 'directory' if member.isdir() else 'file' if member.isfile() else 'unsafe'\n"
                     "                valid = valid and (not member.isdir() or member.size == 0)\n"
                     "                digest = hashlib.sha256(archive.extractfile(member).read()).hexdigest() if member.isfile() else None\n"
                     "                valid = valid and member.name not in seen\n"
                     "                actual[member.name] = (kind, member.mode, digest)\n"
                     "                seen.add(member.name)\n"
                     "        valid = valid and actual in inventories\n"
                     "    except (OSError, ValueError, tarfile.TarError):\n"
                     "        valid = False\n"
                     "    if valid: sys.exit(0)\n"
                     f"    pathlib.Path({str(unexpected)!r}).write_text('skill transport rejected')\n"
                     "    sys.exit(99)\n"
                     "if arguments == ['query', '/1.0/instances/yard?project=subyard']:\n"
                     f"    observation = json.loads(pathlib.Path({str(control)!r}).read_text())\n"
                     "    if not observation.get('instances'): sys.exit(1)\n"
                     "    print(json.dumps(observation['instances'][0]['info']))\n"
                     "    sys.exit(0)\n"
                     "if tuple(arguments) in probes:\n"
                     "    sys.stdin.buffer.read()\n"
                     "    response, code = probes[tuple(arguments)]\n"
                     "    sys.stdout.write(response)\n"
                     "    sys.exit(code)\n"
                     "if arguments == ['list', 'yard', '--project', 'subyard', '-f', 'csv', '-c', 's']:\n"
                     f"    observation = json.loads(pathlib.Path({str(control)!r}).read_text())\n"
                     "    print(observation['instances'][0]['info']['status'].upper() if observation.get('instances') else '')\n"
                     "    sys.exit(0)\n"
                     "if len(arguments) == 9 and arguments[:8] == ['exec', 'yard', '--project', 'subyard', '--', 'systemctl', 'is-active', '--quiet'] and arguments[8].endswith('.service'):\n"
                     "    sys.exit(3)\n"
                     "if arguments and arguments[0] == 'exec' and '--' in arguments:\n"
                     "    command = arguments[arguments.index('--') + 1:]\n"
                     "    if command[:3] == ['bash', '-se', '--'] and '\"state\":\"absent\"' in sys.stdin.read():\n"
                     "        print('{\"state\":\"absent\",\"actual\":\"\",\"desired\":\"\"}')\n"
                     "        sys.exit(0)\n"
                     f"pathlib.Path({str(unexpected)!r}).write_text('incus operation rejected')\n"
                     f"pathlib.Path({str(unexpected) + '.argv.json'!r}).write_text(json.dumps(['incus', *arguments]))\n"
                     "sys.exit(99)\n")
    incus.chmod(0o700)
    for name in ("sudo", "systemctl"):
        commands = {arguments: result for (program, arguments), result in host_probes.items() if program == name}
        shim = shims / name
        shim.write_text("#!/usr/bin/python3\nimport json, pathlib, sys\n"
                        "arguments = sys.argv[1:]\n"
                        "if sys.argv[0].endswith('/sudo') and arguments == ['-n', 'true']:\n"
                        "    sys.exit(0)\n"
                        f"probes = {commands!r}\n"
                        "if tuple(arguments) in probes:\n"
                        "    response, code = probes[tuple(arguments)]\n"
                        "    sys.stdout.write(response)\n"
                        "    sys.exit(code)\n"
                        f"pathlib.Path({str(unexpected)!r}).write_text('host mutation rejected')\n"
                        f"pathlib.Path({str(unexpected) + '.argv.json'!r}).write_text(json.dumps([pathlib.Path(sys.argv[0]).name, *arguments]))\n"
                        "sys.exit(99)\n")
        shim.chmod(0o700)



def sealed_checkpoint_reader(release_root):
    relative = "./config/release-checkpoint.json"
    capability = release_root / relative
    entries = [line.split("  ", 1) for line in
               (release_root / "runtime-files.sha256").read_text().splitlines()
               if line.endswith("  " + relative)]
    if not capability.exists():
        require(not entries and not capability.is_symlink(),
                "sealed checkpoint-reader contract is missing")
        return False
    require(capability.is_file() and not capability.is_symlink() and len(entries) == 1
            and len(entries[0][0]) == 64 and digest(capability) == entries[0][0],
            "checkpoint-reader contract is not bound by its installed manifest")
    payload = capability.read_bytes()
    require(0 < len(payload) <= 1024, "checkpoint-reader contract exceeds its bound")
    try:
        contract = json.loads(payload)
    except (ValueError, UnicodeDecodeError):
        raise RuntimeError("checkpoint-reader contract is invalid") from None
    require(isinstance(contract, dict) and set(contract) == {"schemaVersion", "contract"}
            and type(contract["schemaVersion"]) is int and contract["schemaVersion"] == 1
            and contract["contract"] == "migration-history-checkpoint-v1",
            "checkpoint-reader contract is unsupported")
    return True


def snapshot(root):
    return {
        str(path.relative_to(root)): (path.stat().st_mode, digest(path))
        for path in root.rglob("*") if path.is_file()
    }


def verify_observed_instance_removed(fixture):
    """Verify exact fixture cleanup through the actual shared API inventory."""
    connection = http.client.HTTPConnection("fixture", timeout=5)
    try:
        connection.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        connection.sock.settimeout(5)
        connection.sock.connect(fixture.env["SUBYARD_INCUS_SOCKET"])
        connection.request("GET", "/1.0/instances?recursion=1&all-projects=true")
        response = connection.getresponse()
        payload = json.loads(response.read())
        require(response.status == 200 and isinstance(payload.get("metadata"), list),
                "cannot verify owned fixture instance cleanup")
        require(not any(instance.get("project") == "subyard" and instance.get("name") == "yard"
                        for instance in payload["metadata"]),
                "owned fixture left its instance in the shared inventory")
        (fixture.root / "instance-cleanup.json").write_text(json.dumps(payload) + "\n")
    finally:
        connection.close()


def run_process(command, env, timeout, input_text=None, interrupt_marker=None,
                interrupt_replacement=None, interrupt_checkpoint=None, allow_failed_checkpoint=False):
    watch = None
    if interrupt_replacement is not None or interrupt_checkpoint is not None:
        journal, _, _ = interrupt_replacement or interrupt_checkpoint
        libc = ctypes.CDLL(None, use_errno=True)
        watch = libc.inotify_init1(os.O_CLOEXEC | os.O_NONBLOCK)
        require(watch >= 0, "cannot observe the owned recovery journal")
        require(libc.inotify_add_watch(watch, os.fsencode(journal.parent), 0x00000080) >= 0,
                "cannot watch the owned recovery journal directory")
    started = time.monotonic()
    child = subprocess.Popen(command, env=env,
                             stdin=subprocess.DEVNULL if input_text is None else subprocess.PIPE,
                             stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                             text=True, start_new_session=True)
    failure = None
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
                    require(current["transaction"].startswith(("recovery-v1-", "recovery-v2-", "migration-owner-v1-"))
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
        if interrupt_checkpoint is not None:
            journal, checkpoint, marker = interrupt_checkpoint
            deadline = time.monotonic() + timeout
            observed = False
            while child.poll() is None and time.monotonic() < deadline:
                readable, _, _ = select.select([watch], [], [], 0.02)
                if not readable:
                    continue
                os.read(watch, 65536)
                os.killpg(child.pid, signal.SIGSTOP)
                current = json.loads(journal.read_text())
                if current["checkpoint"] == checkpoint:
                    marker.write_text(json.dumps({"transaction": current["transaction"],
                                                  "checkpoint": checkpoint}) + "\n")
                    marker.chmod(0o600)
                    os.killpg(child.pid, signal.SIGKILL)
                    observed = True
                    break
                os.killpg(child.pid, signal.SIGCONT)
            if not observed and allow_failed_checkpoint and child.poll() is not None:
                current = json.loads(journal.read_text())
                if child.returncode != 0 and current.get("checkpoint") == checkpoint:
                    marker.write_text(json.dumps({"transaction": current["transaction"],
                                                  "checkpoint": checkpoint,
                                                  "nativeFailureExit": child.returncode}) + "\n")
                    marker.chmod(0o600)
                    observed = True
            require(observed, "released migration owner did not reach its checkpoint")
        if interrupt_marker is not None:
            deadline = time.monotonic() + timeout
            while not interrupt_marker.is_file() and child.poll() is None:
                if time.monotonic() >= deadline:
                    raise subprocess.TimeoutExpired(command, timeout)
                time.sleep(0.02)
            if interrupt_marker.is_file():
                os.killpg(child.pid, signal.SIGKILL)
        stdout, stderr = child.communicate(input=input_text, timeout=timeout)
        result = subprocess.CompletedProcess(command, child.returncode, stdout, stderr)
        if HISTORY_EVIDENCE is not None:
            index = len(HISTORY_RESULTS) + 1
            prefix = f"native-{index:03d}"
            (HISTORY_EVIDENCE / (prefix + ".stdout")).write_text(stdout)
            (HISTORY_EVIDENCE / (prefix + ".stderr")).write_text(stderr)
            HISTORY_RESULTS.append({"argv": list(map(str, command)), "exit": child.returncode,
                                    "duration_seconds": time.monotonic() - started,
                                    "stdout": prefix + ".stdout", "stderr": prefix + ".stderr"})
            (HISTORY_EVIDENCE / "native-results.json").write_text(json.dumps(HISTORY_RESULTS, indent=2) + "\n")
        return result
    except (subprocess.TimeoutExpired, RuntimeError) as error:
        failure = error
        error.child_before_cleanup_returncode = child.poll()
        raise
    finally:
        if watch is not None:
            os.close(watch)
        # A timed-out updater may still have a candidate holding the fixture's
        # locks. Stop every process in this owned session before removing state.
        try:
            os.killpg(child.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        final_stdout, final_stderr = child.communicate()
        if failure is not None:
            failure.stdout = final_stdout
            failure.stderr = final_stderr
            failure.child_after_cleanup_returncode = child.returncode
        interrupted = interrupt_replacement or interrupt_checkpoint
        marker = interrupted[2] if interrupted is not None else interrupt_marker
        if marker is not None:
            for suffix, payload in ((".stdout", final_stdout), (".stderr", final_stderr)):
                with os.fdopen(os.open(str(marker) + suffix, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600), "w") as output:
                    os.fchmod(output.fileno(), 0o600)
                    output.write(payload)


def set_fixture_observation(control, status=None, journal=None, history=None):
    instances = [] if status is None else [{"project": "subyard", "name": "yard",
                                            "info": {"name": "yard", "status": status,
                                                     "type": "container", "config": {}, "devices": {}}}]
    payload = json.dumps({"instances": instances, "holdJournal": str(journal) if journal else "", "migrationHistory": history})
    control.write_text(payload)
    control.chmod(0o600)
    applied = Path(str(control) + ".applied")
    deadline = time.monotonic() + 10
    while time.monotonic() < deadline:
        if applied.exists() and applied.read_text() == payload:
            return
        time.sleep(0.02)
    require(False, "Incus fixture did not apply the requested observation")


class Fixture:
    def __init__(self, root, release, version, baseline, arch, baseline_version=BASELINE,
                 preserve_observation=False):
        check_scratch_capacity(release / f"subyard-{version}-linux-{arch}.tar.gz",
                               [baseline / f"subyard-{baseline_version}-linux-{arch}.tar.gz"])
        self.root = root
        self.home = root / "home"
        self.data = self.home / ".subyard"
        self.config = self.home / ".config/subyard"
        self.runtime = self.data / "runtime"
        self.journal = self.config / "release-transition/v2/journal.json"
        self.version = version
        self.arch = arch
        self.baseline_version = baseline_version
        self.config.mkdir(parents=True)
        control = root.parent / "incus-observation.json"
        if control.exists() and not preserve_observation:
            set_fixture_observation(control)
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
        installer_root = baseline if baseline_version in RETIRED_BASELINES else release
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
    boundary = Fixture(root / f"checkpoint-boundary-{baseline_version}", release, version,
                       baseline, arch, baseline_version)
    boundary.update("--version", version, "--yes")
    boundary.complete()
    verify_power_rollback_refusal(boundary)
    # Preserve matching-epoch rollback and future settings/reader-bridge coverage
    # independently of the real candidate's irreversible power reader boundary.
    release, version = compatible_power_release(release, version, arch, root)
    fixture = Fixture(root / f"checkpoint-source-{baseline_version}", release, version, baseline, arch,
                      baseline_version)
    source_reader = sealed_checkpoint_reader(fixture.runtime / fixture.initial)
    fixture.update("--version", version, "--yes")
    fixture.complete()
    verify_epoch_one_publication_gate(fixture)
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
    registry = json.loads((fixture.runtime / bridge / "config/release-transition.json").read_text())
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
    if source_reader:
        require(outcome["status"] == "migration-required" and
                outcome["code"] == "transition-required" and
                inspection.get("plan", "").startswith("plan-v1-") and
                inspection.get("resume") is None and outcome.get("transaction") is None and
                inspection["assessment"]["changed"],
                "sealed reader did not assess a fresh future-epoch plan: " +
                json.dumps({key: outcome.get(key) for key in ("status", "code")}))
        require(snapshot(fixture.config) == before and links() == prior_links,
                "reader future inspection changed protected state or runtime links")
        refused = run_process([str(fixture.old_launcher), "update", "--version", future_version],
                              fixture.env, timeout=180)
        require(refused.returncode != 0 and
                "confirmation" in (refused.stdout + refused.stderr).lower() and
                snapshot(fixture.config) == before and links() == prior_links,
                "sealed reader advanced future history without fresh consent")
        expected_previous = fixture.initial
    else:
        require(outcome["status"] == "operator-action-required" and
                outcome["code"] == "rollback-incompatible" and
                "verified checkpoint-reader bridge" in outcome["retry"],
                "released caller omitted the checkpoint-reader bridge prerequisite: " +
                json.dumps({key: outcome.get(key) for key in ("status", "code")}))
        require(snapshot(fixture.config) == before and links() == prior_links,
                "blocked future inspection changed protected state or runtime links")
        refused = run_process([str(fixture.old_launcher), "update", "--version", future_version, "--yes"],
                              fixture.env, timeout=180)
        require(refused.returncode != 0 and
                "verified checkpoint-reader bridge" in refused.stdout + refused.stderr and
                snapshot(fixture.config) == before and links() == prior_links,
                "released caller advanced checkpoint history from an unaware retained source")

        # The exact supplied epoch-2 candidate is the reader bridge before
        # checkpoint history advances from an unaware retained source.
        fixture.update("--version", version, "--yes")
        fixture.complete()
        require(os.readlink(fixture.runtime / "current") == bridge,
                "released caller did not install the exact assessed reader bridge")
        expected_previous = bridge
    fixture.update("--version", future_version, "--yes")
    fixture.version = future_version
    fixture.complete(expected_previous=expected_previous)
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
    capability_path = "uses its sealed reader" if source_reader else "requires the sealed reader bridge"
    print(f"PASS: released {baseline_version} caller {capability_path} before synthetic "
          "epoch 3; both callers refuse rollback to epoch 2 and preserve the pinned projection", flush=True)


def verify_legacy(release, version, baseline, arch, root, baseline_version=LEGACY_BASELINE):
    fixture = Fixture(root / f"legacy-{baseline_version}", release, version, baseline, arch, baseline_version)
    owned_marker = fixture.data / "projects/retained-project-data"
    owned_marker.parent.mkdir()
    fixture.project_marker.rename(owned_marker)
    fixture.project_marker = owned_marker
    def links():
        return tuple(os.readlink(path) if path.is_symlink() else None
                     for path in (fixture.runtime / "current", fixture.runtime / "previous"))
    initial_links = links()
    before = snapshot(fixture.config)
    installer = (release / "subyard-install.sh").read_text()
    def bootstrap(*arguments):
        return run_process(["bash", "-s", "--", "--version", version, *arguments],
                           fixture.env, timeout=180, input_text=installer)
    # The original released updater and the current standalone entrypoint both
    # refuse before changing the active pair or protected settings.
    for result in (run_process([str(fixture.old_launcher), "update", "--version", version, "--yes"],
                               fixture.env, timeout=180), bootstrap("--yes")):
        require(result.returncode != 0, "unsupported source activated the candidate")
        require("v0.11.0" in result.stdout + result.stderr
                and "uninstall --full" in result.stdout + result.stderr,
                f"unsupported source lacks actionable removal guidance: {result.stdout}{result.stderr}")
        require(snapshot(fixture.config) == before and links() == initial_links,
                "unsupported upgrade changed protected config or active links")
    result = bootstrap("--publish-only")
    require(result.returncode == 0 and links() == initial_links and snapshot(fixture.config) == before,
            f"cleanup tooling staging required migration/activation: {result.stdout}{result.stderr}")
    candidates = list((fixture.runtime / "releases").glob(f"{version}-*"))
    require(len(candidates) == 1, "staging did not produce one exact candidate")
    candidate = candidates[0] / "bin/yard"
    # No yard workload is created here; physical inventory and interrupted
    # deletion belong to the owning full-uninstall lane.
    foreign = fixture.home / "foreign.control"
    foreign.write_text("foreign content\n")
    tools = fixture.root / "cold-tools"
    tools.mkdir()
    for command in ("bash", "dirname", "readlink", "python3", "env"):
        (tools / command).symlink_to(shutil.which(command))
    removal_env = dict(fixture.env, PATH=str(tools), INCUS_DIR=str(fixture.root / "absent-incus"))
    removal_env.pop("SUBYARD_INCUS_SOCKET", None)
    result = run_process([str(candidate), "uninstall", "--full", "--yes"], removal_env, timeout=180)
    require(result.returncode == 0, f"staged full uninstall failed: {result.stdout}{result.stderr}")
    require(not fixture.runtime.exists() and not fixture.config.exists() and not fixture.data.exists(),
            f"full uninstall retained owned old state: {[str(path.relative_to(fixture.home)) for path in fixture.data.rglob('*')]}")
    require(foreign.read_text() == "foreign content\n", "uninstall removed foreign content")
    require(not Path(str(fixture.data) + ".uninstall").exists(), "uninstall did not verify closure")
    # A separate invocation models the user's later clean installation.
    result = bootstrap("--yes")
    require(result.returncode == 0, f"separate clean install failed: {result.stdout}{result.stderr}")
    require(not fixture.retained.exists() and not fixture.project_marker.exists(),
            "clean installation reused retired settings or data")
    print(f"PASS: actual released {baseline_version} refuses upgrade, stages cleanup without activation, "
          "fully uninstalls and permits a separate clean install", flush=True)


def verify_minimum_source(release, version, baseline, arch, root):
    fixture = Fixture(root / "minimum-source", release, version, baseline, arch, MINIMUM_BASELINE)
    require(fixture.check()["outcome"]["status"] == "migration-required",
            "actual v0.11.0 lower-bound caller cannot inspect the update")
    fixture.update("--version", version, "--yes")
    fixture.complete()
    print(f"PASS: actual released v0.11.0 inclusive lower-bound update -> {version}", flush=True)


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
            require(fixture.baseline_version in (PUBLISHED_BASELINE, SOURCE_BASELINE)
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


def verify_power_rollback_refusal(fixture):
    checkpoint = json.loads((fixture.config / "release-transition/v2/history-checkpoint.json").read_text())
    require(checkpoint["domains"]["power-metadata"]["epoch"] == 2,
            "optional publication reader boundary did not commit")
    for launcher in (fixture.old_launcher, fixture.runtime / "current/bin/yard"):
        before = snapshot(fixture.config)
        links = tuple(os.readlink(fixture.runtime / name) for name in ("current", "previous"))
        refused = run_process([str(launcher), "update", "--rollback", "--yes"], fixture.env, timeout=180)
        diagnostic = (refused.stdout + refused.stderr).lower()
        after = snapshot(fixture.config)
        changed = sorted(key for key in before.keys() | after.keys() if before.get(key) != after.get(key))
        after_links = tuple(os.readlink(fixture.runtime / name) for name in ("current", "previous"))
        require(refused.returncode != 0 and "code=rollback-incompatible" in diagnostic,
                f"epoch-1 rollback omitted its typed refusal: exit={refused.returncode} diagnostic={diagnostic}")
        require(not changed and after_links == links,
                f"epoch-1 rollback mutated protected state: changed={changed} links_changed={after_links != links}")
    print(f"PASS: actual released {fixture.baseline_version} and current callers refuse epoch-1 rollback after epoch 2 without mutation", flush=True)


def verify_power_publication_boundary(release, version, baseline, arch, root, baseline_version):
    fixture = Fixture(root / f"power-boundary-{baseline_version}", release, version, baseline, arch, baseline_version)
    require(fixture.check()["outcome"]["status"] == "migration-required",
            "epoch-1 released caller cannot inspect the power reader upgrade")
    fixture.update("--version", version, "--yes")
    fixture.complete()
    verify_power_rollback_refusal(fixture)


def compatible_power_release(release, version, arch, root):
    """Build a sealed fixture bridge with historical power-1/settings-2 epochs.

    Its production reader gate remains enabled, so this package cannot mint a
    publication snapshot or write deferred state. This is never the candidate.
    """
    destination = root / "compatible-power-release"
    bridge_version = version + "-power1-fixture"
    if destination.exists():
        return destination, bridge_version
    source = Path(__file__).resolve().parent.parent
    copied = root / "compatible-power-source"
    selected = run_process(["bash", str(source / "tests/helpers/source-files.sh"), str(source)],
                           env=None, timeout=30)
    require(selected.returncode == 0, "cannot select public fixture bridge inputs")
    for relative in filter(None, selected.stdout.split("\0")):
        path = source / relative
        require(path.is_file() and not path.is_symlink(), "bridge input is not a regular public file")
        target = copied / relative
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(path, target)
    registry_path = copied / "config/release-transition.json"
    registry = json.loads(registry_path.read_text())
    exact = {"id": "introduce-owner-publication-state-v1", "domain": "power-metadata",
             "fromEpoch": 1, "toEpoch": 2, "kind": "owner-publication-state-v1"}
    exact_settings = {"id": "localize-ssh-port-settings-v3", "domain": "settings",
                      "fromEpoch": 2, "toEpoch": 3, "kind": "local-ssh-port-settings-v2-to-v3",
                      "dependsOn": ["canonicalize-test-vms-settings-v2", "canonicalize-test-yard-owner-v2"]}
    require(registry["currentEpochs"]["power-metadata"] == 2
            and registry["currentEpochs"]["settings"] == 3
            and registry["migrations"].count(exact) == 1
            and registry["migrations"].count(exact_settings) == 1, "fixture bridge boundary differs")
    registry["migrations"].remove(exact)
    registry["migrations"].remove(exact_settings)
    registry["currentEpochs"]["power-metadata"] = 1
    registry["currentEpochs"]["settings"] = 2
    require(registry["currentEpochs"] == {"owner-registration": 2, "power-metadata": 1,
                                          "project-state": 1, "settings": 2}
            and [entry["id"] for entry in registry["migrations"]] == [
                "canonicalize-test-vms-settings-v2", "canonicalize-test-yard-owner-v2"],
            "fixture bridge is not the exact historical compatible registry")
    registry_path.write_text(json.dumps(registry, indent=2) + "\n")
    destination.mkdir()
    for path in release.iterdir():
        if path.is_file():
            shutil.copy2(path, destination / path.name)
    built = run_process(["bash", str(copied / "dev/package-engine.sh"), "--version", bridge_version,
                         "--arch", arch, "--output-dir", str(destination)], env=None, timeout=180)
    require(built.returncode == 0, f"cannot package compatible fixture bridge: {built.stdout}{built.stderr}")
    bundle = destination / f"subyard-{bridge_version}-linux-{arch}.tar.gz"
    print(f"FIXTURE: sealed compatible power-1/settings-2 bridge {bridge_version} sha256={digest(bundle)}; "
          "exact publication and SSH-settings migrations removed; production reader gate retained", flush=True)
    return destination, bridge_version


def verify_epoch_one_publication_gate(fixture):
    before = snapshot(fixture.config)
    refused = run_process([str(fixture.runtime / "current/bin/yard"), "_publication-contracts"],
                          fixture.env, timeout=30)
    require(refused.returncode != 0 and not refused.stdout
            and "epoch 2" in refused.stderr and snapshot(fixture.config) == before,
            "compatible fixture bridge minted deferred-publication authority before epoch 2: "
            + refused.stdout + refused.stderr)


def verify(release, version, baseline, arch, root, baseline_version=BASELINE):
    normal = Fixture(root / f"normal-{baseline_version}", release, version, baseline, arch, baseline_version)
    require(normal.check()["outcome"]["status"] == "migration-required",
            "old updater could not inspect candidate activation")
    normal.update("--version", version, "--yes")
    normal.complete()
    verify_checkpoint_refusal(normal)
    verify_completed_activation_drift(normal)
    verify_power_rollback_refusal(normal)
    normal.update("--offline", "--version", version, "--yes")
    normal.complete()
    shutil.rmtree(normal.root)
    print(f"PASS: released {baseline_version} updater -> {version}, fixed point and power rollback refusal", flush=True)

    bridge_release, bridge_version = compatible_power_release(release, version, arch, root)
    compatible = Fixture(root / f"normal-compatible-{baseline_version}", bridge_release, bridge_version,
                         baseline, arch, baseline_version)
    compatible.update("--version", bridge_version, "--yes")
    compatible.complete()
    verify_epoch_one_publication_gate(compatible)
    compatible.update("--rollback", "--yes", old=False)
    require(os.readlink(compatible.runtime / "current") == compatible.initial,
            "matching-epoch rollback lost the old runtime")
    compatible.update("--offline", "--version", bridge_version, "--yes")
    compatible.complete()
    shutil.rmtree(compatible.root)
    print(f"PASS: actual released {baseline_version} matching-epoch rollback and forward retry "
          "with sealed compatible fixture bridge", flush=True)

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
    shutil.rmtree(interrupted.root)
    print(f"PASS: released {baseline_version} updater resumes the candidate's journal after SIGKILL", flush=True)


def verify_activation_only_recovery(release, version, baseline, arch, root,
                                    baseline_version=ACTIVATION_BASELINE, source_repair=False,
                                    recovery_baseline=None):
    started = time.monotonic()
    fixture = Fixture(root / f"activation-only-{baseline_version}", release, version, baseline, arch,
                      baseline_version)
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
        set_fixture_observation(control, status, fixture.journal if hold else None)

    observe("Running", True)
    shims = fixture.root / "commands"
    shims.mkdir()
    unexpected = fixture.root / "unexpected-host-command"
    interrupted = Path(str(control) + ".held")
    # Each phase needs its own observed interruption. A prior fixture's marker
    # must never kill this session before it has published its own journal.
    interrupted.unlink(missing_ok=True)
    write_observation_incus(shims, control, unexpected)
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
    if source_repair:
        candidate = verify_alias_source_repair(fixture, release, unfinished=True)
        before = snapshot(fixture.config)
        fresh = json.loads(fixture.run([str(fixture.old_launcher), "migrate", "--check", "--json"]))
        require(fresh["outcome"]["status"] == "recovering" and not fresh.get("blockers"),
                "original owner did not offer a fresh recovery assessment after source repair")
        declined = run_process([str(fixture.old_launcher), "migrate"], fixture.env, timeout=60)
        require(declined.returncode != 0 and "confirmation" in (declined.stdout + declined.stderr).lower()
                and snapshot(fixture.config) == before,
                "original-owner recovery reused source-repair consent")
        fixture.run([str(fixture.old_launcher), "migrate", "--yes"])
        completed = json.loads(fixture.journal.read_text())
        require(completed["checkpoint"] == "complete" and completed["transaction"] != journal["transaction"],
                "original owner did not complete its separately authorized replacement")
        ready = json.loads(fixture.run([str(fixture.old_launcher), "migrate", "--check", "--json"]))
        require(ready["outcome"]["status"] == "ready", "original owner could not verify recovery readiness")
        fixture.update("--version", version, "--yes")
        fixture.complete(old=False)
        print(f"PASS: actual {baseline_version} original owner separately authorizes source-repair recovery", flush=True)
        return
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
    latest = candidate
    original_registry = fixture.runtime / fixture.initial / "config/release-transition.json"
    registry_advanced = digest(candidate.parent.parent / "config/release-transition.json") != digest(original_registry)
    recovery_version = version
    if registry_advanced:
        # Delegation requires identical sealed registries and compiled catalogs.
        # A latest runtime with a new migration cannot execute historical work.
        refused = json.loads(fixture.run([str(latest), "migrate", "--check", "--json"]))
        declined = run_process([str(latest), "migrate", "--yes"], fixture.env, timeout=60)
        require(refused["outcome"]["status"] == "operator-action-required"
                and refused.get("blockers") == blocked.get("blockers") and declined.returncode != 0
                and snapshot(fixture.config) == config_before and fixture.journal.read_bytes() == journal_before
                and os.readlink(fixture.runtime / "current") == fixture.initial
                and not (fixture.runtime / "previous").exists() and not unexpected.exists(),
                "cross-registry standalone caller changed or resumed original history")
        cached = root / f"baseline-{SOURCE_BASELINE}"
        directory = recovery_baseline or (cached if cached.is_dir() else None)
        bridge = baseline_assets(root, directory, SOURCE_BASELINE, SOURCE_BASELINE_SHA256, arch)
        contract = Fixture(root / "activation-recovery-bridge-contract", release, version,
                           bridge, arch, SOURCE_BASELINE, preserve_observation=True)
        contract.run([str(contract.old_launcher), "migrate", "--yes"])
        bridge_journal = json.loads(contract.journal.read_text())
        require(bridge_journal["registryDigest"] == journal["registryDigest"]
                and bridge_journal["catalogDigest"] == journal["catalogDigest"],
                "pinned recovery bridge does not own the exact original registry and catalog")
        bridge_bundle = bridge / f"subyard-{SOURCE_BASELINE}-linux-{arch}.tar.gz"
        published_bridge = fixture.run(["bash", str(release / "subyard-install-runtime-release.sh"),
                                        "--publish-only", "--runtime-root", str(fixture.runtime),
                                        "--bundle", str(bridge_bundle), "--checksum", str(bridge_bundle) + ".sha256",
                                        "--manifest", str(bridge_bundle) + ".manifest.json",
                                        "--provenance", str(bridge_bundle) + ".provenance.json"]).strip()
        require(published_bridge.startswith(f"releases/{SOURCE_BASELINE}-")
                and "/" not in published_bridge[len("releases/"):], "recovery bridge identity is invalid")
        candidate = fixture.runtime / published_bridge / "bin/yard"
        require(digest(candidate.parent.parent / "config/release-transition.json") == digest(original_registry),
                "published recovery bridge changed the original registry")
        recovery_version = SOURCE_BASELINE
        print(f"PASS: latest {version} refuses cross-registry recovery without mutation; "
              f"pinned official {SOURCE_BASELINE} matches the original registry and catalog", flush=True)
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
    print(f"PASS: unmodified released {ACTIVATION_BASELINE} activation-only journal resumes with {recovery_version} code and original assets",
          flush=True)
    fixture.update("--version", version, "--yes")
    fixture.complete(old=False)
    require(os.readlink(fixture.runtime / "previous") == fixture.initial
            and registration.read_bytes() == registration_before
            and not unexpected.exists(), "ordinary update changed retained state or invoked a host mutation")
    if registry_advanced:
        before_ledger, after_ledger = json.loads(ledger_before), json.loads(ledger.read_text())
        current_registry = json.loads((fixture.runtime / "current/config/release-transition.json").read_text())
        settings_migration = {"id": "localize-ssh-port-settings-v3", "domain": "settings",
                              "fromEpoch": 2, "toEpoch": 3, "kind": "local-ssh-port-settings-v2-to-v3",
                              "dependsOn": ["canonicalize-test-vms-settings-v2", "canonicalize-test-yard-owner-v2"]}
        require(current_registry["currentEpochs"]["settings"] == 3
                and current_registry["migrations"].count(settings_migration) == 1,
                "ordinary update fixture differs from the declared settings migration")
        require(before_ledger["domains"]["power-metadata"] == {"epoch": 1, "applied": []}
                and after_ledger["domains"]["power-metadata"] == {
                    "epoch": 2, "applied": ["introduce-owner-publication-state-v1"]}
                and before_ledger["domains"]["settings"]["epoch"] == 2
                and after_ledger["domains"]["settings"] == {
                    "epoch": 3, "applied": before_ledger["domains"]["settings"]["applied"]
                    + [settings_migration["id"]]}
                and {key: value for key, value in before_ledger.items() if key != "domains"}
                == {key: value for key, value in after_ledger.items() if key != "domains"}
                and {key: value for key, value in before_ledger["domains"].items()
                     if key not in ("power-metadata", "settings")}
                == {key: value for key, value in after_ledger["domains"].items()
                    if key not in ("power-metadata", "settings")},
                "ordinary update changed undeclared history or failed the publication/settings migrations")
        print(f"PASS: separately authorized ordinary update reaches {version} publication epoch 2/settings epoch 3 "
              "and preserves the settings prefix and every other original ledger domain", flush=True)
    else:
        require(ledger.read_bytes() == ledger_before, "same-registry ordinary update changed migration ledger")
    print(f"PASS: released {ACTIVATION_BASELINE} ordinary updater reaches {version} after delegated recovery "
          f"({time.monotonic() - started:.1f}s)", flush=True)


def verify_recovery_preservation(fixture, expected, transaction, links_before, unexpected, failure_dir):
    paths = {"journal": fixture.journal,
             "receipt": fixture.config / "release-transition/recovery/v2/transactions" / (transaction + ".json"),
             "ledger": fixture.config / "release-transition/v2/ledger.json",
             "selection": fixture.config / "config.env", "unexpected": unexpected,
             "rejected-argv": Path(str(unexpected) + ".argv.json")}
    actual, statuses = {}, {}
    for name, path in paths.items():
        descriptor = None
        try:
            descriptor = os.open(path.anchor, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
            for part in path.parts[1:-1]:
                child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=descriptor)
                os.close(descriptor)
                descriptor = child
            child = os.open(path.name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=descriptor)
            with os.fdopen(child, "rb") as stream:
                require(stat.S_ISREG(os.fstat(stream.fileno()).st_mode), "capture input is not regular")
                payload = stream.read((1 << 20) + 1)
                require(len(payload) <= 1 << 20, "capture input exceeds its bound")
            actual[name], statuses[name] = payload, "read"
        except FileNotFoundError:
            statuses[name] = "missing"
        except (OSError, RuntimeError) as error:
            statuses[name] = "refused:" + type(error).__name__
        finally:
            if descriptor is not None:
                os.close(descriptor)
    try:
        journal_transaction = json.loads(actual.get("journal", b""))["transaction"]
    except (ValueError, KeyError, TypeError):
        journal_transaction = None
    try:
        links = tuple(os.readlink(fixture.runtime / name) for name in ("current", "previous"))
    except OSError:
        links = None
    checks = {"journal-transaction": journal_transaction == transaction,
              **{name: actual.get(name) == expected[name] for name in ("receipt", "ledger", "selection")},
              "runtime-links": links == links_before, "unexpected-command": statuses["unexpected"] == "missing"}
    failed = [name for name, passed in checks.items() if not passed]
    if not failed:
        return
    evidence = None
    try:
        failure_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
        require(not failure_dir.is_symlink() and failure_dir.is_dir(), "capture directory is unsafe")
        failure_dir.chmod(0o700)
        evidence = Path(tempfile.mkdtemp(prefix="recovery-preservation-", dir=failure_dir))
        evidence.chmod(0o700)
        report = {"failed_checks": failed, "read_statuses": statuses,
                  "expected_links": links_before, "actual_links": links}
        payloads = {"comparison.json": (json.dumps(report, indent=2) + "\n").encode()}
        for label, entries in (("expected", expected), ("actual", actual)):
            for name, payload in entries.items():
                require(len(payload) <= 1 << 20, "capture output exceeds its bound")
                payloads[f"{name}-{label}.bin"] = payload
        for name, payload in payloads.items():
            descriptor = os.open(evidence / name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
            with os.fdopen(descriptor, "wb") as stream:
                os.fchmod(stream.fileno(), 0o600)
                stream.write(payload)
    except (OSError, RuntimeError) as error:
        print("recovery failure capture refused: " + type(error).__name__, file=sys.stderr, flush=True)
    print(f"recovery preservation failed_checks={','.join(failed)} evidence={evidence}", file=sys.stderr, flush=True)
    require(False, "released-caller resume changed recovery evidence, selections, ledger or runtime links")


def verify_fresh_activation_recovery(release, version, baseline, arch, root, compatible=False, failure_dir=None, evidence_dir=None):
    failure_dir = failure_dir or Path.cwd() / ".build/release-upgrade-failures"
    fixture = Fixture(root / ("fresh-activation-compatible" if compatible else "fresh-activation"),
                      release, version, baseline, arch)
    host_settings = fixture.config / "config.env"
    # This core recovery fixture has no installed optional profile runtimes.
    host_settings.write_text("ENVIRONMENT_PROFILES=\nCODING_TOOL_INTEGRATIONS=codex\n")
    host_settings.chmod(0o600)
    fixture.update("--version", version, "--yes")
    fixture.complete()
    if compatible:
        verify_epoch_one_publication_gate(fixture)
    launcher = fixture.runtime / "current/bin/yard"
    ledger = fixture.config / "release-transition/v2/ledger.json"
    ledger_before = ledger.read_bytes()
    links_before = tuple(os.readlink(fixture.runtime / name) for name in ("current", "previous"))
    control = root / "incus-observation.json"

    def observe(status, hold):
        set_fixture_observation(control, status, fixture.journal if hold else None)

    # Native observations are synthetic; any privileged or mutating host command
    # is rejected. This exercises the actual candidate engine and wire contract.
    shims = fixture.root / "commands"
    shims.mkdir()
    unexpected = fixture.root / "unexpected-host-command"
    write_observation_incus(shims, control, unexpected)
    fixture.env["PATH"] = str(shims) + ":/usr/bin:/bin"
    interrupted = Path(str(control) + ".held")
    if interrupted.exists():
        interrupted.unlink()
    try:
        observe("Running", True)
        result = None
        try:
            # This budget covers the whole native operation, including repeated
            # completed owner observations before the backend hold. Match
            # Fixture.run; it is not a timeout for an individual owner probe.
            result = run_process([str(launcher), "migrate", "--yes"], fixture.env,
                                 timeout=180, interrupt_marker=interrupted)
            require(result.returncode == -signal.SIGKILL and interrupted.is_file(),
                    "candidate did not interrupt activation-only reconciliation: " + result.stdout + result.stderr)
        except (subprocess.TimeoutExpired, RuntimeError) as error:
            try:
                if evidence_dir:
                    destination = evidence_dir.resolve() / ("fresh-activation-compatible" if compatible else "fresh-activation") / "interruption-failure"
                    destination.mkdir(parents=True, mode=0o700)
                    destination.chmod(0o700)
                    records = {"journal.json": fixture.journal, "ledger.json": ledger,
                               "checkpoint.json": fixture.config / "release-transition/v2/history-checkpoint.json",
                               "control.json": control, "control.applied": Path(str(control) + ".applied"),
                               "control.held": interrupted, "unexpected-command.bytes": unexpected}
                    diagnostic = {"reason": type(error).__name__,
                                  "childBeforeCleanupReturncode": getattr(error, "child_before_cleanup_returncode", result.returncode if result is not None else None),
                                  "childAfterCleanupReturncode": getattr(error, "child_after_cleanup_returncode", None),
                                  "records": {}, "selectionSHA256": {}, "links": {}}
                    artifacts = {"stdout": (getattr(error, "stdout", None) or (result.stdout if result is not None else "")).encode(),
                                 "stderr": (getattr(error, "stderr", None) or (result.stderr if result is not None else "")).encode()}
                    for name, path in records.items():
                        diagnostic["records"][name] = {"path": str(path), "present": path.is_file()}
                        if path.is_file():
                            artifacts[name] = path.read_bytes()
                            diagnostic["records"][name]["sha256"] = hashlib.sha256(artifacts[name]).hexdigest()
                    for name in ("config.env", "settings.env"):
                        path = fixture.config / name
                        diagnostic["selectionSHA256"][name] = digest(path) if path.is_file() else None
                    for name in ("current", "previous"):
                        path = fixture.runtime / name
                        diagnostic["links"][name] = os.readlink(path) if path.is_symlink() else None
                    artifacts["state.json"] = (json.dumps(diagnostic, indent=2) + "\n").encode()
                    for name, payload in artifacts.items():
                        with os.fdopen(os.open(destination / name, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), "wb") as output:
                            output.write(payload)
            except Exception as capture_error:
                error.add_note("fresh interruption capture failed: " + type(capture_error).__name__)
            raise
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
        host_settings.write_text("CODING_TOOL_INTEGRATIONS=claude\nENVIRONMENT_PROFILES=\n")
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
        host_settings.write_text("CODING_TOOL_INTEGRATIONS=\nENVIRONMENT_PROFILES=\n")
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
        verify_recovery_preservation(
            fixture, {"receipt": receipt_bytes, "ledger": ledger_before, "selection": changed_bytes},
            transaction, links_before, unexpected, failure_dir)
        if compatible:
            # A stopped guest cannot establish absence for owners missing from the
            # retained release; the ordinary rollback guard must refuse without mutation.
            before_stopped = snapshot(fixture.config)
            stopped_links = tuple(os.readlink(fixture.runtime / name) for name in ("current", "previous"))
            stopped = run_process([str(fixture.runtime / "current/bin/yard"), "update", "--rollback", "--yes"], fixture.env, timeout=60)
            require(stopped.returncode != 0 and "code=rollback-incompatible" in (stopped.stdout + stopped.stderr)
                    and snapshot(fixture.config) == before_stopped and not unexpected.exists()
                    and tuple(os.readlink(fixture.runtime / name) for name in ("current", "previous")) == stopped_links,
                    "stopped runtime rollback lost its typed refusal or changed protected state")
            # This positive case proves an absent installation through the API.
            # Real running activation belongs to installed-runtime acceptance.
            observe(None, False)
            verify_observed_instance_removed(fixture)
            # The changed native observation requires an ordinary fresh assessment
            # and consent for the current target before selecting another goal.
            fixture.update("--offline", "--version", version, "--yes", old=False)
            fixture.complete(old=False)
            fixture.update("--rollback", "--yes", old=False)
            require(os.readlink(fixture.runtime / "current") == fixture.initial,
                    "API-proved absent installation lost eligible matching-epoch rollback")
        else:
            verify_power_rollback_refusal(fixture)
            require(receipt_path.read_bytes() == receipt_bytes and ledger.read_bytes() == ledger_before
                    and host_settings.read_bytes() == changed_bytes and not unexpected.exists(),
                    "refused power rollback changed protected recovery evidence")
        fixture.update("--offline", "--version", version, "--yes")
        fixture.complete()
        terminal_path = fixture.config / "release-transition/recovery/v2/archive" / (transaction + ".json")
        retained_receipt = receipt_path.read_bytes() if receipt_path.exists() else (
            json.dumps(json.loads(terminal_path.read_text())["receipt"], separators=(",", ":")) + "\n").encode()
        require(retained_receipt == receipt_bytes and ledger.read_bytes() == ledger_before
                and host_settings.read_bytes() == changed_bytes and not unexpected.exists(),
                "rollback/refusal and forward update changed protected recovery history")
    finally:
        observe(None, False)
        verify_observed_instance_removed(fixture)
    print(f"PASS: capable {version} fresh activation recovery preserves ledger and links; released {BASELINE} "
          f"caller inspects and resumes V2 successor, {'absent-installation rollback' if compatible else 'power rollback refusal'} and forward", flush=True)
    if not compatible:
        bridge_release, bridge_version = compatible_power_release(release, version, arch, root)
        verify_fresh_activation_recovery(bridge_release, bridge_version, baseline, arch, root, compatible=True, failure_dir=failure_dir, evidence_dir=evidence_dir)


def verify_alias_source_repair(fixture, release, unfinished=False):
    registration = fixture.config / "yards/hermes/config.env"
    registration.parent.mkdir(parents=True, mode=0o700, exist_ok=True)
    registration.write_text("# Retained local source record.\nYARD_PROFILES=\nSSH_PORT=2237\n")
    registration.chmod(0o640)
    if unfinished:
        # Literal source drift follows authentic journal publication. The old
        # ordinary writer is correctly blocked by the unfinished mutation gate.
        with registration.open("a") as source:
            source.write("ENVIRONMENT_PROFILES=orca-runtime\n")
    else:
        # The unmodified released writer reproduces the defect; no journal editing.
        fixture.run([str(fixture.old_launcher), "-Y", "hermes", "config", "set",
                     "ENVIRONMENT_PROFILES", "orca-runtime", "--scope", "yard", "--local", "--yes"])
    require("YARD_PROFILES=" in registration.read_text()
            and "ENVIRONMENT_PROFILES=" in registration.read_text(), "released writer did not retain alias")
    blocked = run_process([str(fixture.old_launcher), "-Y", "hermes", "config", "show"],
                          fixture.env, timeout=30)
    require(blocked.returncode != 0, "released caller accepted the conflicting source")
    if not unfinished:
        # A released completed owner reports its own native blocker, but refuses
        # a different target before rendering that protected inspection.
        before = snapshot(fixture.config)
        links = (os.readlink(fixture.runtime / "current"),
                 os.readlink(fixture.runtime / "previous") if (fixture.runtime / "previous").is_symlink() else None)
        same_owner = run_process([str(fixture.old_launcher), "update", "--check", "--json"],
                                 fixture.env, timeout=60)
        require(same_owner.returncode == 0, "released same-owner assessment was unavailable")
        inspection = json.loads(same_owner.stdout)
        require(inspection.get("outcome", {}).get("status") == "operator-action-required"
                and any(blocker.get("code") == "dependency-unavailable"
                        for blocker in inspection.get("blockers", [])),
                "released same-owner assessment did not retain its native blocker")
        different_target = run_process([str(fixture.old_launcher), "update", "--check", "--json",
                                        "--version", fixture.version], fixture.env, timeout=60)
        require(different_target.returncode == 1
                and "permits only exact target" in different_target.stderr,
                f"released different-target guard differed (exit {different_target.returncode})")
        require(snapshot(fixture.config) == before and
                (os.readlink(fixture.runtime / "current"),
                 os.readlink(fixture.runtime / "previous") if (fixture.runtime / "previous").is_symlink() else None) == links,
                "released blocker inspections changed protected state")
        print(f"PASS: actual {fixture.baseline_version} same-owner blocker assessment and "
              "different-target original-owner refusal preserve protected state", flush=True)
    staged = fixture.run(["bash", str(release / "subyard-install.sh"), "--publish-only",
                          "--version", fixture.version])
    stage_line = next((line for line in staged.splitlines() if line.startswith("staged release=")), "")
    require(stage_line, "staging did not return a candidate identity")
    candidate = Path(stage_line.split(" candidate=", 1)[1]) / "bin/yard"
    protected = fixture.config / "release-transition"
    before = snapshot(protected)
    links = (os.readlink(fixture.runtime / "current"),
             os.readlink(fixture.runtime / "previous") if (fixture.runtime / "previous").is_symlink() else None)
    original = registration.read_bytes()
    original_mode = registration.stat().st_mode & 0o777
    command = [str(candidate), "-Y", "hermes", "config", "repair-alias", "ENVIRONMENT_PROFILES", "",
               "--scope", "yard", "--local"]
    fixture.run(command + ["--check"])
    declined = run_process(command, fixture.env, timeout=60)
    require(declined.returncode != 0 and "confirmation" in (declined.stdout + declined.stderr).lower(),
            "source repair reused an old authorization")
    require(registration.read_bytes() == original and snapshot(protected) == before,
            "preview or decline changed source/history")
    fixture.run(command + ["--yes"])
    require("YARD_PROFILES=" not in registration.read_text()
            and "ENVIRONMENT_PROFILES=''" in registration.read_text()
            and "SSH_PORT=2237" in registration.read_text()
            and registration.stat().st_mode & 0o777 == original_mode,
            "explicit source repair changed unrelated records or permissions")
    require(snapshot(protected) == before, "source repair changed original protected metadata")
    require((os.readlink(fixture.runtime / "current"),
             os.readlink(fixture.runtime / "previous") if (fixture.runtime / "previous").is_symlink() else None)
            == links, "source repair activated staged tooling")
    fixture.run([str(candidate), "-Y", "hermes", "config", "validate", "--json"])
    fixture.run(command + ["--yes"])
    require(snapshot(protected) == before, "source repair no-op changed history")
    if unfinished:
        require(json.loads(fixture.journal.read_text())["checkpoint"] == "reconciling",
                "new tooling executed unfinished original history")
    print(f"PASS: actual {fixture.baseline_version} {'unfinished' if unfinished else 'completed'} "
          "owner source repair stages tooling, requires new consent and preserves protected history", flush=True)
    return candidate


def verify_completed_source_repair(release, version, baseline, arch, root):
    fixture = Fixture(root / "source-repair-completed", release, version, baseline, arch, SOURCE_BASELINE)
    fixture.run([str(fixture.old_launcher), "migrate", "--yes"])
    require(json.loads(fixture.journal.read_text())["checkpoint"] == "complete",
            "actual source owner did not produce terminal history")
    verify_alias_source_repair(fixture, release)
    # Retain a canonical Git selection so removing a local override restores it.
    source = fixture.root / "selection-source"
    host = source / "hosts/fixture"
    host.mkdir(parents=True, mode=0o700)
    (source / "subyard-config.json").write_text('{"schemaVersion":1}\n')
    (host / "config.env").write_text("ENVIRONMENT_PROFILES=\nCODING_TOOL_INTEGRATIONS='codex paseo'\nSSH_PORT=2228\n")
    for path in source.rglob("*"):
        path.chmod(0o700 if path.is_dir() else 0o600)
    for command in (["git", "-C", str(source), "init", "-q"],
                    ["git", "-C", str(source), "add", "."],
                    ["git", "-C", str(source), "-c", "user.name=Fixture",
                     "-c", "user.email=fixture@example.invalid", "commit", "-qm", "Seed selection"]):
        fixture.run(command)
    fixture.run([str(fixture.old_launcher), "config", "sync", "connect", str(source),
                 "--host-id", "fixture", "--checkout", str(fixture.root / "checkout"), "--yes"])
    fixture.run([str(fixture.old_launcher), "-Y", "hermes", "config", "set",
                 "CODING_TOOL_INTEGRATIONS", "codex", "--scope", "yard", "--local", "--yes"])
    fixture.update("--version", version, "--yes")
    fixture.complete()
    verify_completed_source_selection(fixture, root)


def verify_completed_source_selection(fixture, root):
    launcher = str(fixture.runtime / "current/bin/yard")
    before = snapshot(fixture.config / "release-transition")
    links = (os.readlink(fixture.runtime / "current"), os.readlink(fixture.runtime / "previous"))
    fixture.run([launcher, "-Y", "hermes", "config", "unset", "CODING_TOOL_INTEGRATIONS",
                 "--scope", "yard", "--local", "--yes"])
    # A running consumer exposes ordinary materialized drift. Keep every other
    # runtime absent/current so this exercises completed config.set admission.
    control = root / "incus-observation.json"
    set_fixture_observation(control, "Running")
    shims = fixture.root / "selection-commands"
    shims.mkdir(mode=0o700)
    unexpected = fixture.root / "unexpected-selection-host-command"
    write_observation_incus(shims, control, unexpected)
    fixture.env["PATH"] = str(shims) + ":/usr/bin:/bin"
    gate = json.loads(fixture.run([launcher, "migrate", "--check", "--json"]))
    require(gate["outcome"]["status"] == "migration-required" and not gate.get("blockers")
            and [(decision["resource"], decision["decision"]) for decision in gate.get("decisions", [])
                 if decision["decision"] != "preserve"]
            == [("activation.materialized-config", "canonicalize")],
            "completed selection fixture must have only materialized config drift")
    fixture.run([launcher, "-Y", "hermes", "config", "set", "CODING_TOOL_INTEGRATIONS", "",
                 "--scope", "yard", "--local", "--yes"])
    registration = fixture.config / "yards/hermes/config.env"
    require("CODING_TOOL_INTEGRATIONS=''" in registration.read_text()
            and snapshot(fixture.config / "release-transition") == before
            and (os.readlink(fixture.runtime / "current"), os.readlink(fixture.runtime / "previous")) == links
            and not unexpected.exists(),
            "completed selection authoring changed protected history, links or host state")
    set_fixture_observation(control)
    fixture.complete()
    print(f"PASS: actual {fixture.baseline_version} upgrade retains historical domain prefixes; "
          "completed materialized drift admits local unset then empty set without changing history", flush=True)


def verify_migration_owner_replacement(release, version, baseline, source_baseline,
                                       source_version, history_baseline, arch, root):
    fixture = Fixture(root / "migration-owner", release, MIGRATION_OWNER_BASELINE,
                      history_baseline, arch, DIRECT_BASELINE)
    host_settings = fixture.config / "config.env"
    host_settings.write_text("CODING_TOOL_INTEGRATIONS=codex\n")
    host_settings.chmod(0o600)
    fixture.run([str(fixture.old_launcher), "migrate", "--yes"])
    official_journal = fixture.journal.read_bytes()
    official_release = os.readlink(fixture.runtime / "current")
    projection = fixture.config / "release-transition/v2/ledger.json"
    projection_bytes = projection.read_bytes()
    fixture.env["YARD_RELEASE_BASE_URL"] = source_baseline.as_uri()
    fixture.update("--version", source_version, "--yes")
    source_journal = json.loads(fixture.journal.read_text())
    require(source_journal["checkpoint"] == "complete" and source_journal["steps"] == []
            and source_journal["releases"]["from"] == Path(official_release).name,
            "authentic same-registry custom update did not close with zero steps")
    require(projection.read_bytes() == projection_bytes,
            "same-registry custom update changed the exact official projection")
    source_release = os.readlink(fixture.runtime / "current")
    fixture.old_launcher = fixture.runtime / source_release / "bin/yard"
    require(fixture.run([str(fixture.old_launcher), "--version"]).strip() == f"yard {source_version}",
            "authentic custom source did not become active")
    (fixture.root / "official-source-journal.json").write_bytes(official_journal)
    (fixture.root / "custom-source-journal.json").write_bytes(fixture.journal.read_bytes())
    transactions = fixture.config / "release-transition/v2/transactions"
    retained_before = sorted(path.name for path in transactions.iterdir()) if transactions.exists() else []
    (fixture.root / "source-history.json").write_text(json.dumps({
        "officialRelease": Path(official_release).name, "customRelease": Path(source_release).name,
        "retainedTransactionsAfterCustom": retained_before,
        "projectionSHA256": hashlib.sha256(projection_bytes).hexdigest()}, indent=2) + "\n")
    registration = fixture.config / "yards/unrelated/config.env"
    registration.parent.mkdir(parents=True, mode=0o700)
    registration.write_text("YARD_TEMPLATE=''\nSSH_PORT=2226\n")
    registration.chmod(0o600)
    control = root / "incus-observation.json"
    def observe(status, hold):
        payload = json.dumps({"instances": [{"project": "subyard", "name": "yard",
                   "info": {"name": "yard", "status": status, "type": "container",
                            "config": {}, "devices": {}}}],
                   "holdJournal": str(fixture.journal) if hold else ""})
        control.write_text(payload)
        control.chmod(0o600)
        deadline = time.monotonic() + 10
        applied = Path(str(control) + ".applied")
        while time.monotonic() < deadline:
            if applied.exists() and applied.read_text() == payload:
                return
            time.sleep(0.02)
        require(False, "migration owner observation fixture did not apply")
    observe("Running", True)
    shims = fixture.root / "commands"
    shims.mkdir()
    unexpected = fixture.root / "unexpected-host-command"
    write_observation_incus(shims, control, unexpected)
    fixture.env["PATH"] = str(shims) + ":/usr/bin:/bin"
    fixture.env["YARD_RELEASE_BASE_URL"] = baseline.as_uri()
    marker = Path(str(control) + ".held")
    result = run_process([str(fixture.old_launcher), "update", "--version", MIGRATION_OWNER_BASELINE,
                          "--yes"], fixture.env, timeout=180, interrupt_marker=marker)
    require(result.returncode != 0,
            "actual released owner unexpectedly completed: " + result.stdout + result.stderr)
    (fixture.root / "released-owner-failure.log").write_text(result.stdout + result.stderr)
    original_bytes = fixture.journal.read_bytes()
    (fixture.root / "predecessor.json").write_bytes(original_bytes)
    original = json.loads(original_bytes)
    require(original["checkpoint"] == "reconciling" and
            [step["migration"] for step in original["steps"]] ==
            ["introduce-owner-publication-state-v1", "localize-ssh-port-settings-v3"] and
            all(step["checkpoint"] == "verified" for step in original["steps"]),
            "actual released owner did not produce the supported completed ledger steps")
    transactions = fixture.config / "release-transition/v2/transactions"
    require((transactions / original["transaction"]).is_dir() and
            (fixture.runtime / official_release).is_dir() and (fixture.runtime / source_release).is_dir()
            and original["releases"]["from"] == Path(source_release).name
            and original["releases"]["previous"] == Path(official_release).name,
            "multi-update fixture lost its exact authentic release generations")
    require(sorted(path.name for path in transactions.iterdir()) ==
            sorted(retained_before + [original["transaction"]]),
            "official owner changed the native retained transaction graph unexpectedly")
    require(projection.read_bytes() == projection_bytes, "old owner rewrote its frozen projection")
    # The authentic owner can fail an unavailable retained runtime observation
    # before reaching the optional hold. Keep that baseline failure evidence;
    # candidate activation must independently satisfy only recognized probes.
    if unexpected.exists():
        unexpected.rename(fixture.root / "released-owner-unavailable-observation")
    observe("Stopped", False)
    owner = fixture.runtime / "current/bin/yard"
    before = snapshot(fixture.config)
    rejected = run_process([str(owner), "update", "--check", "--offline", "--version",
                            MIGRATION_OWNER_BASELINE], fixture.env, timeout=60)
    require(rejected.returncode != 0 or "operator-action-required" in rejected.stdout,
            "actual affected owner unexpectedly admitted its interrupted journal")
    require("journaled ledger before state is not a registry prefix" in rejected.stderr,
            "actual affected owner did not reproduce its independent-domain reader defect")
    (fixture.root / "released-owner-inspection.log").write_text(rejected.stdout + rejected.stderr)
    require(snapshot(fixture.config) == before, "old inspection changed protected state")
    fixture.version = version
    fixture.env["YARD_RELEASE_BASE_URL"] = release.as_uri()
    staged = fixture.run(["bash", str(release / "subyard-install.sh"), "--publish-only",
                          "--version", version])
    line = next((line for line in staged.splitlines() if line.startswith("staged release=")), "")
    require(line, "standalone candidate was not staged")
    candidate = Path(line.split(" candidate=", 1)[1]) / "bin/yard"
    command = [str(candidate), "update", "--offline", "--version", version]
    # Exercise inherited nonempty shipped defaults across separate sealed native
    # processes; an explicit selection would hide process-owned source pins.
    saved_host_settings = host_settings.read_bytes()
    saved_host_mode = host_settings.stat().st_mode & 0o7777
    try:
        host_settings.write_text("ENVIRONMENT_PROFILES=\n")
        host_settings.chmod(0o600)
        observe("Running", False)
        provenance_before = snapshot(fixture.config)
        provenance_checks = [json.loads(fixture.run(command + ["--check", "--json"])) for _ in range(2)]
        consequences = [value["assessment"].get("consequences", []) for value in provenance_checks]
        provenance_after = snapshot(fixture.config)
        stability = {
            "samePlan": provenance_checks[0]["plan"] == provenance_checks[1]["plan"],
            "sameConsequences": consequences[0] == consequences[1],
            "nonemptyShippedOrigin": any("config/agents.env:" in line and "from shipped defaults" in line
                                         and "requested coding integrations []" not in line for line in consequences[0]),
            "protectedStateUnchanged": provenance_after == provenance_before,
        }
        snapshot_hashes = {name: hashlib.sha256(json.dumps(value, sort_keys=True).encode()).hexdigest()
                           for name, value in (("before", provenance_before), ("after", provenance_after))}
        with os.fdopen(os.open(fixture.root / "shipped-default-plan-stability.json",
                               os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), "w") as output:
            json.dump({"probeOnlyRunning": True, "checks": provenance_checks, "conditions": stability, "snapshotHashes": snapshot_hashes}, output, indent=2)
            output.write("\n")
        print("FIXTURE: Running read-only shipped-default plan stability " + json.dumps({"conditions": stability, "snapshotHashes": snapshot_hashes}), flush=True)
        require(all(stability.values()), "same shipped-default source did not produce a stable exact native plan")
    finally:
        host_settings.write_bytes(saved_host_settings)
        host_settings.chmod(saved_host_mode)
        observe("Stopped", False)
    # Ordinary consent/CAS/resume below retains the original fixture selection;
    # the Running probe does not claim default integration installation coverage.
    previous_registry = fixture.runtime / official_release / "config/release-transition.json"
    previous_payload = previous_registry.read_bytes()
    before = snapshot(fixture.config)
    before_links = tuple(os.readlink(fixture.runtime / name) for name in ("current", "previous"))
    try:
        previous_registry.write_bytes(previous_payload + b"\n")
        refused = run_process(command + ["--check", "--json"], fixture.env, timeout=60)
        require(refused.returncode != 0 and snapshot(fixture.config) == before
                and tuple(os.readlink(fixture.runtime / name) for name in ("current", "previous")) == before_links,
                "changed journal-bound previous assets admitted recovery or mutated protected state")
        (fixture.root / "previous-asset-refusal.log").write_text(refused.stdout + refused.stderr)
    finally:
        previous_registry.write_bytes(previous_payload)
    before = snapshot(fixture.config)
    inspection = json.loads(fixture.run(command + ["--check", "--json"]))
    require(not inspection.get("blockers") and inspection.get("resume") is None and
            inspection["assessment"]["changed"] and
            inspection["plan"] not in (original["authorizationPlan"], original["resumePlan"]),
            "candidate did not assess a fresh proved supersession")
    declined = run_process(command, fixture.env, timeout=60)
    require(declined.returncode != 0 and "confirmation" in
            (declined.stdout + declined.stderr).lower() and snapshot(fixture.config) == before,
            "migration owner replacement reused old consent or mutated while inspecting")
    marker = fixture.root / "migration-owner-successor.json"
    result = run_process(command + ["--yes"], fixture.env, timeout=180,
                         interrupt_replacement=(fixture.journal, original["transaction"], marker))
    require(result.returncode == -signal.SIGKILL and marker.exists(),
            "candidate supersession did not reach observed durable successor publication")
    successor = json.loads(fixture.journal.read_text())
    transaction = successor["transaction"]
    require(transaction.startswith("migration-owner-v1-") and successor["steps"] == [],
            "candidate did not publish an ordinary activation successor")
    receipt_path = fixture.config / "release-transition/migration-owner/v1/transactions" / (transaction + ".json")
    receipt_bytes = receipt_path.read_bytes()
    receipt = json.loads(receipt_bytes)
    require(receipt["contract"] == "migration-ledger-owner-replacement-v1" and
            (json.dumps(receipt["predecessor"], separators=(",", ":")) + "\n").encode() == original_bytes,
            "typed receipt did not preserve canonical unfinished predecessor")
    fixture.run(command)
    fixture.complete(old=False, expected_previous="releases/" + original["goal"]["target"])
    fixture.run([str(fixture.runtime / "current/bin/yard"), "config", "apply", "--yes"])
    require(json.loads(fixture.journal.read_text())["transaction"] == transaction and
            receipt_path.read_bytes() == receipt_bytes and projection.read_bytes() == projection_bytes and
            (transactions / original["transaction"]).is_dir() and not unexpected.exists(),
            "candidate resume changed predecessor evidence or attempted an unowned host mutation")
    print(f"PASS: authentic {source_version} history -> actual {MIGRATION_OWNER_BASELINE} reader defect -> "
          f"negotiated {version} supersession, fresh consent, interrupted CAS successor and ordinary resume", flush=True)


def verify_direct_019(release, version, baseline, baseline_version, arch, root):
    fixture = Fixture(root / ("direct-" + baseline_version), release, version,
                      baseline, arch, baseline_version)
    host = fixture.config / "config.env"
    host.write_text("CODING_TOOL_INTEGRATIONS=codex\n")
    host.chmod(0o600)
    fixture.run([str(fixture.old_launcher), "migrate", "--yes"])
    source = json.loads(fixture.journal.read_text())
    require(source["checkpoint"] == "complete", "direct predecessor did not complete")
    (fixture.root / "source-journal.json").write_bytes(fixture.journal.read_bytes())
    source_transaction = source["transaction"]
    projection = fixture.config / "release-transition/v2/ledger.json"
    projection_before = projection.read_bytes()
    registration = fixture.config / "yards/unrelated/config.env"
    registration.parent.mkdir(parents=True, mode=0o700)
    registration.write_text("YARD_TEMPLATE=''\nSSH_PORT=2226\n")
    registration.chmod(0o600)
    control = root / "incus-observation.json"
    def observe(status):
        value = ({"removeInstances": [{"project": "subyard", "name": "yard"}]} if status is None else
                 {"instances": [{"project": "subyard", "name": "yard",
                   "info": {"name": "yard", "status": status, "type": "container",
                            "config": {}, "devices": {}}}]})
        payload = json.dumps(value)
        control.write_text(payload)
        control.chmod(0o600)
        applied = Path(str(control) + ".applied")
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            if applied.exists() and applied.read_text() == payload:
                return
            time.sleep(0.02)
        require(False, "direct upgrade observation fixture did not apply")
    try:
        observe("Running")
        shims = fixture.root / "commands"
        shims.mkdir()
        unexpected = fixture.root / "unexpected-host-command"
        failure_control = fixture.root / "activation-failure.json"
        failure_marker = fixture.root / "activation-failure-observed"
        failure_control.write_text(json.dumps({"journal": str(fixture.journal), "marker": str(failure_marker)}))
        write_observation_incus(shims, control, unexpected, activation_failure=failure_control)
        fixture.env["PATH"] = str(shims) + ":/usr/bin:/bin"
        transactions = fixture.config / "release-transition/v2/transactions"
        require((transactions / source_transaction).is_dir(), "direct fixture omitted retained terminal evidence")
        result = run_process([str(fixture.old_launcher), "update", "--version", version, "--yes"],
                             fixture.env, timeout=180)
        (fixture.root / "post-effects-failure.log").write_text(result.stdout + result.stderr)
        require(result.returncode != 0 and failure_marker.is_file(),
                "direct fixed update did not reach controlled post-effects failure: " + result.stdout + result.stderr)
        failed = json.loads(fixture.journal.read_text())
        require(failed["checkpoint"] == "reconciling" and
                [step["migration"] for step in failed["steps"]] ==
                ["introduce-owner-publication-state-v1", "localize-ssh-port-settings-v3"] and
                all(step["checkpoint"] == "verified" for step in failed["steps"]),
                "controlled activation failure preceded migration verification")
        require(projection.read_bytes() == projection_before, "direct update rewrote frozen projection")
        evidence = snapshot(transactions / failed["transaction"])
        candidate = fixture.runtime / "current/bin/yard"
        before = snapshot(fixture.config)
        reread = json.loads(fixture.run([str(candidate), "update", "--check", "--offline",
                                        "--version", version, "--json"]))
        require(reread.get("resume") == failed["transaction"] and
                reread["outcome"]["code"] == "dependency-unavailable" and
                "activation" in json.dumps(reread.get("blockers", [])).lower() and
                snapshot(fixture.config) == before,
                "post-error reobservation lost its owning diagnostic or valid history")
        (fixture.root / "post-effects-inspection.json").write_text(json.dumps(reread) + "\n")
        failure_control.unlink()
        observe("Stopped")
        resumed = json.loads(fixture.run([str(candidate), "update", "--check", "--offline",
                                         "--version", version, "--json"]))
        (fixture.root / "resume-inspection.json").write_text(json.dumps(resumed) + "\n")
        require(resumed.get("resume") == failed["transaction"] and not resumed.get("blockers"),
                "direct fixed reader could not resume the valid independent-domain history")
        fixture.run([str(candidate), "update", "--offline", "--version", version])
        fixture.complete(old=False)
        fixture.run([str(candidate), "config", "apply", "--yes"])
        completed = json.loads(fixture.journal.read_text())
        require(completed["transaction"] == failed["transaction"] and
                completed["steps"] == failed["steps"] and
                snapshot(transactions / failed["transaction"]) == evidence and
                projection.read_bytes() == projection_before,
                "ordinary restart replayed or replaced completed migration evidence")
    finally:
        observe(None)
        # Read the actual API inventory after the removal acknowledgement, not
        # merely the desired control file. Other canned instances remain intact.
        verify_observed_instance_removed(fixture)
    print(f"PASS: authentic {baseline_version} -> fixed {version} directly; controlled post-effects "
          "activation error, owning diagnostic/reobservation, new-process resume, no migration replay and gated operation", flush=True)

def history_snapshot(root):
    return {str(path.relative_to(root)): (path.stat().st_mode, path.stat().st_ino,
                                         path.stat().st_mtime_ns, digest(path))
            for path in root.rglob("*") if path.is_file()}


@dataclass(frozen=True)
class PublishedEvidence:
    relative_path: str
    mode: int
    inode: int
    mtime_ns: int
    digest: str


@dataclass(frozen=True)
class StagedEvidence:
    destination: str
    mode: int
    inode: int
    captured_prefix: bytes


def history_evidence_destinations(journal, *, transaction_prefix=True):
    prefix = Path(journal['transaction']) if transaction_prefix else Path()
    return {str(prefix / 'evidence' / (step['id']+'.'+phase+'.json'))
            for step in journal['steps'] for phase in ('captured', 'applied', 'verified')}


def capture_history_evidence(root, allowed_destinations=()):
    """Capture published identity separately from resumable private staging.

    The pending and checkpoint names are the writer's persistence format here.
    Full record validity remains the ordinary transition validator's responsibility.
    """
    root = Path(root)
    allowed = set(allowed_destinations)
    for destination in allowed:
        path = Path(destination)
        require(not path.is_absolute() and all(part not in ('.', '..') and not part.startswith('.') for part in path.parts)
                and re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]*\.(captured|applied|verified)\.json', path.name),
                'invalid evidence staging destination')
    records = {}
    if not root.exists() and not root.is_symlink():
        return records
    require(stat.S_ISDIR(root.lstat().st_mode), 'unsafe evidence root')
    paths = []
    for directory, directories, files in os.walk(root, followlinks=False,
            onerror=lambda error: require(False, 'cannot inspect evidence directory')):
        for name in directories:
            path = Path(directory) / name
            require(not name.startswith('.') and stat.S_ISDIR(path.lstat().st_mode), 'unsafe evidence directory')
        for name in files:
            path = Path(directory) / name
            info = path.lstat()
            require(stat.S_ISREG(info.st_mode) and info.st_nlink == 1, 'unsafe evidence file')
            paths.append((path, info))
    names = {str(path.relative_to(root)) for path, _ in paths}
    for path, info in paths:
        relative = str(path.relative_to(root))
        mode = stat.S_IMODE(info.st_mode)
        if path.name.startswith('.'):
            require(path.name.endswith('.pending') and len(path.name) > len('.pending')+1,
                    'unknown hidden evidence file')
            destination = str(path.relative_to(root).with_name(path.name[1:-len('.pending')]))
            require(destination in allowed and destination not in names, 'unknown or colliding evidence staging')
            records[relative] = StagedEvidence(destination, mode, info.st_ino, path.read_bytes())
        else:
            require(not path.name.endswith('.pending'), 'unknown evidence staging file')
            records[relative] = PublishedEvidence(relative, mode, info.st_ino, info.st_mtime_ns, digest(path))
    return records


def verify_history_evidence(root, before, allowed_destinations=()):
    """Verify retention after a successful retry, permitting staging publication."""
    allowed = set(allowed_destinations)
    after = capture_history_evidence(root, allowed)
    require(not any(isinstance(record, StagedEvidence) for record in after.values()), 'completed retry left evidence staging')
    for name, record in before.items():
        if isinstance(record, PublishedEvidence):
            require(after.get(name) == record, 'retry changed published evidence identity: '+name)
        elif isinstance(record, StagedEvidence):
            published = after.get(record.destination)
            require(record.destination in allowed and isinstance(published, PublishedEvidence)
                    and published.mode == record.mode and published.inode == record.inode
                    and (Path(root) / record.destination).read_bytes().startswith(record.captured_prefix),
                    'retry replaced or changed captured staging: '+name)
        else:
            require(False, 'unknown captured evidence state')


def history_links(fixture):
    return tuple((os.readlink(path), path.lstat().st_mode, path.lstat().st_ino, path.lstat().st_mtime_ns)
                 if path.is_symlink() else None
                 for path in (fixture.runtime / "current", fixture.runtime / "previous"))


def history_effects(control):
    path = Path(str(control) + ".effects.jsonl")
    return path.read_bytes() if path.exists() else b""


def history_inspect(fixture, control, launcher=None):
    before, links, effects = history_snapshot(fixture.config), history_links(fixture), history_effects(control)
    result = run_process([str(launcher or fixture.runtime / "current/bin/yard"), "migrate", "--check", "--json"],
                         fixture.env, timeout=90)
    require(result.returncode == 0, "migration-history inspection failed: " + result.stdout + result.stderr)
    inspection = json.loads(result.stdout)
    require(history_snapshot(fixture.config) == before and history_links(fixture) == links
            and history_effects(control) == effects, "migration-history inspection changed protected or guest state")
    return inspection


def history_chain(release, version, arch, root, baselines, checkpointed, name):
    start, prior = (SOURCE_BASELINE, HISTORY_BASELINE) if checkpointed else (BASELINE, PUBLISHED_BASELINE)
    inputs = {}
    for baseline_version in (start, prior):
        inputs[baseline_version] = baseline_assets(root, baselines[baseline_version], baseline_version,
                                                   HISTORY_BASELINES[baseline_version], arch)
    mirror = root / (name + "-prior-release")
    mirror.mkdir()
    for asset in inputs[prior].iterdir():
        if asset.is_file(): shutil.copy2(asset, mirror / asset.name)
    shutil.copy2(release / "subyard-install-runtime-release.sh", mirror / "subyard-install-runtime-release.sh")
    fixture = Fixture(root / name, mirror, prior, inputs[start], arch, start)
    fixture.update("--version", prior, "--yes")
    fixture.complete(old=False)
    prior_current = os.readlink(fixture.runtime / "current")
    prior_journal = json.loads(fixture.journal.read_text())
    require(prior_journal["checkpoint"] == "complete" and len(prior_journal["steps"]) == 2
            and all(step["checkpoint"] == "verified" for step in prior_journal["steps"]),
            "authentic predecessor did not complete its original migration graph")
    transactions = fixture.config / "release-transition/v2/transactions"
    prior_evidence = capture_history_evidence(transactions)
    require(len(prior_evidence) == 6, "authentic predecessor has no complete evidence triples")
    if HISTORY_EVIDENCE is not None:
        shutil.copytree(fixture.config, HISTORY_EVIDENCE / (name + "-closed-source"))
    checkpoint = fixture.config / "release-transition/v2/history-checkpoint.json"
    require(checkpoint.exists() == checkpointed, "authentic chain has the wrong authority role")
    fixture.old_launcher = fixture.runtime / prior_current / "bin/yard"
    fixture.baseline_version, fixture.version = prior, version
    fixture.env["YARD_RELEASE_BASE_URL"] = release.as_uri()
    return fixture, prior_current, prior_evidence


def history_materialized_fixture(fixture, root, control, fail_once=True):
    source, guest = fixture.root / "fixture-source.txt", fixture.root / "fixture-guest.txt"
    drift = fixture.root / "fixture-drift.txt"
    source.write_text("generic migration history fixture desired\n")
    guest.write_bytes(source.read_bytes())
    drift.write_text("generic migration history fixture prior\n")
    for path in (source, guest, drift):
        path.chmod(0o600)
    yard = fixture.config / "yards/default/config.env"
    yard.parent.mkdir(parents=True, mode=0o700)
    (fixture.config / "yards").chmod(0o700)
    yard.parent.chmod(0o700)
    yard.write_text("SSH_PORT=2224\nENVIRONMENT_PROFILES=\nCODING_TOOL_INTEGRATIONS=fixture\n"
                    f"AGENT_fixture_CONFIG='{source}'\nAGENT_fixture_CONFIG_DEST=.subyard-fixture/config.txt\n")
    yard.chmod(0o600)
    history = {"journal": str(fixture.journal), "runtime": str(fixture.runtime),
               "target": "", "source": str(source), "guest": str(guest), "initialDrift": str(drift),
               "destination": "/home/dev/.subyard-fixture/config.txt", "failOnce": fail_once,
               "migrations": ["introduce-owner-publication-state-v1", "localize-ssh-port-settings-v3"]}
    # The exact candidate identity comes from verified package bytes, not a
    # patched installed asset or a journal supplied by this fixture.
    bundle = (Path(fixture.env["YARD_RELEASE_BASE_URL"].removeprefix("file://")) /
              f"subyard-{fixture.version}-linux-{fixture.arch}.tar.gz")
    history["target"] = fixture.version + "-" + digest(bundle)[:12]
    inventory = (Path(__file__).resolve().parent.parent / "internal/adapters/reconcileruntime/integration_inventory.py").read_text()
    history["inventoryProgramSHA256"] = hashlib.sha256(inventory.replace("@STATE_ROOT@", "/var/lib/subyard/integrations").replace("@STATE_UID@", "0").encode()).hexdigest()
    package_root = Path(__file__).resolve().parent.parent
    package = runpy.run_path(str(package_root / 'config/skills/package.py'))
    history['skills'] = {'files': {name: digest(package_root / name) for name in package['source_files'](package_root)},
                         'programSHA256': digest(package_root / 'config/skills/probe.py'),
                         'user': 'dev', 'profiles': '', 'coding': True}
    shims = fixture.root / "commands"
    shims.mkdir(mode=0o700)
    unexpected = fixture.root / "unexpected-host-command"
    write_observation_incus(shims, control, unexpected, [fixture.old_launcher.parent.parent])
    fixture.env["PATH"] = str(shims) + ":/usr/bin:/bin"
    set_fixture_observation(control, "Running", history=history)
    return history, source, guest, unexpected


def history_preserves_evidence(fixture, before):
    journal = json.loads(fixture.journal.read_text())
    verify_history_evidence(fixture.config / 'release-transition/v2/transactions', before,
                            history_evidence_destinations(journal))


def history_protocol(fixture, request):
    payload = json.dumps(request)
    (HISTORY_EVIDENCE / f"native-{len(HISTORY_RESULTS) + 1:03d}.request.json").write_text(payload + "\n")
    return run_process([str(fixture.runtime / "current/bin/yard"), "_release-transition"],
                       fixture.env, timeout=90, input_text=payload)


def history_request(fixture, plan, mode="converge"):
    journal = json.loads(fixture.journal.read_text())
    return {"schemaVersion": 1, "mode": mode, "runtimeRoot": str(fixture.runtime),
            "configHome": str(fixture.config), "yard": "default", "target": journal["goal"]["target"],
            "direction": "activate-target", "artifactDigest": journal["artifactDigest"],
            "registryDigest": journal["registryDigest"], "execution": {"plan": plan, "authorization": ""}}


def history_no_effects(fixture, control):
    guest = json.loads(control.read_text())["migrationHistory"]["guest"]
    return history_snapshot(fixture.config), history_links(fixture), digest(Path(guest)), history_effects(control)


def verify_history_safety(fixture, control, checkpointed):
    journal_bytes = fixture.journal.read_bytes()
    journal = json.loads(journal_bytes)
    root = fixture.config / "release-transition/v2"
    registry = fixture.runtime / "current/config/release-transition.json"
    cases = [("pin", fixture.journal, lambda payload: payload.replace(journal["steps"][0]["desiredFingerprint"].encode(), b"f" * 64, 1)),
             ("registry", registry, lambda payload: payload + b"\n"),
             ("evidence-embedded", fixture.journal, lambda payload: payload.replace(b'"observedFingerprint":"', b'"observedFingerprint":"f', 1)),
             ("unknown-effect", fixture.journal, lambda payload: payload.replace(journal["steps"][0]["resource"].encode(), b"unrecognized.effect", 1))]
    if checkpointed:
        cases.append(("projection", root / "ledger.json", lambda payload: payload + b"\n"))
    refused = []
    for name, path, mutate in cases:
        original = path.read_bytes()
        try:
            mutated = mutate(original)
            require(mutated != original, name + " mutant did not change its intended input")
            path.write_bytes(mutated)
            before = history_no_effects(fixture, control)
            result = run_process([str(fixture.runtime / "current/bin/yard"), "migrate", "--check", "--json"], fixture.env, timeout=90)
            try:
                outcome = json.loads(result.stdout)["outcome"]
            except (ValueError, KeyError):
                outcome = {}
            diagnostic = (result.stdout + result.stderr).lower()
            require(outcome.get("status") == "operator-action-required" or
                    result.returncode != 0 and any(word in diagnostic for word in ("invalid", "manifest", "checksum", "registry", "projection", "evidence", "integrity")),
                    f"{name} mutant did not produce a concrete refusal: {result.stdout}{result.stderr}")
            require(history_no_effects(fixture, control) == before, name + " refusal performed additional effects")
            refused.append({"case": name, "exit": result.returncode, "status": outcome.get("status"), "code": outcome.get("code")})
        finally:
            path.write_bytes(original)
    # A stale process plan is not a fresh authorization. The ordinary same-goal
    # idempotent retry was already checked separately above.
    link = fixture.runtime / "previous"
    original_link = os.readlink(link)
    try:
        link.unlink()
        link.symlink_to(fixture.initial)
        before = history_no_effects(fixture, control)
        result = history_protocol(fixture, history_request(fixture, journal["resumePlan"]))
        require(result.returncode == 0 and json.loads(result.stdout)["outcome"]["status"] != "ready"
                and history_no_effects(fixture, control) == before, "changed links reused a completed plan")
        refused.append({"case": "links", "exit": result.returncode,
                        "code": json.loads(result.stdout)["outcome"]["code"]})
    finally:
        link.unlink()
        link.symlink_to(original_link)
    (HISTORY_EVIDENCE / ("safety-checkpoint.json" if checkpointed else "safety-legacy.json")).write_text(json.dumps(refused, indent=2) + "\n")


def verify_history_source_admission(fixture, control, checkpointed, phase, mutate=False):
    command = [str(fixture.runtime / "current/bin/yard"), "-Y", "default", "config", "repair-alias",
               "ENVIRONMENT_PROFILES", "", "--scope", "yard", "--local", "--check"]
    before = history_no_effects(fixture, control)
    result = run_process(command, fixture.env, timeout=90)
    require(history_no_effects(fixture, control) == before, "source admission check performed effects")
    supported = result.returncode == 0
    if checkpointed:
        require(supported and "source repair has not been applied" in result.stdout,
                "checkpoint history must support source admission: " + result.stdout + result.stderr)
    else:
        expected = ("yard: config repair-alias: release source admission: protected metadata is unsupported or unresolved; "
                    "finish pending publications or recovery intent with the exact sealed original owner before source repair")
        require(result.returncode == 1 and result.stderr.strip() == expected,
                "legacy history must retain the exact known historical-anchor refusal: " + result.stdout + result.stderr)
        print("UNSUPPORTED: legacy source metadata admission " + phase +
              "; known historical-anchor refusal; ordinary readiness is separate", flush=True)
    report = {"status": "supported" if supported else "unsupported", "exit": result.returncode}
    if supported and mutate:
        journal = json.loads(fixture.journal.read_text())
        evidence = fixture.config / "release-transition/v2/transactions" / journal["transaction"] / "evidence"
        path = next(evidence.glob("*.verified.json"))
        original = path.read_bytes()
        try:
            changed = original.replace(b'"observedFingerprint":"', b'"observedFingerprint":"f', 1)
            require(changed != original, "source sidecar mutant did not change its input")
            path.write_bytes(changed)
            before = history_no_effects(fixture, control)
            refused = run_process(command, fixture.env, timeout=90)
            require(refused.returncode != 0 and "release source admission:" in refused.stderr and
                    history_no_effects(fixture, control) == before,
                    "source admission did not refuse changed sidecar evidence without effects")
            report["changedSidecar"] = "refused-without-effects"
        finally:
            path.write_bytes(original)
    name = "source-admission-" + ("checkpoint" if checkpointed else "legacy") + "-" + phase + ".json"
    (HISTORY_EVIDENCE / name).write_text(json.dumps(report, indent=2) + "\n")
    return report


def verify_history_fresh_repair(fixture, control, history, guest, checkpointed):
    journal = json.loads(fixture.journal.read_text())
    preserved = capture_history_evidence(fixture.config / "release-transition/v2/transactions")
    guest.write_text("known fresh runtime drift\n")
    history["migrations"] = []
    set_fixture_observation(control, "Running", history=history)
    fresh = history_inspect(fixture, control)
    require(fresh["outcome"]["status"] == "migration-required" and fresh.get("resume") is None,
            "completed history did not require a fresh activation plan")
    before = history_no_effects(fixture, control)
    stale = history_protocol(fixture, history_request(fixture, "plan-v1-" + "f" * 64))
    require(stale.returncode == 0 and json.loads(stale.stdout)["outcome"]["code"] == "plan-stale"
            and history_no_effects(fixture, control) == before, "stale process plan authorized fresh repair")
    replay = history_protocol(fixture, history_request(fixture, journal["resumePlan"]))
    require(replay.returncode == 0 and json.loads(replay.stdout)["outcome"]["code"] == "plan-stale",
            "old durable consent authorized a new activation repair scope")
    declined = run_process([str(fixture.runtime / "current/bin/yard"), "migrate"], fixture.env, timeout=90)
    require(declined.returncode != 0 and "confirmation" in (declined.stdout + declined.stderr).lower()
            and history_no_effects(fixture, control) == before,
            "fresh repair changed state without fresh consent")
    prior = os.readlink(fixture.runtime / "previous")
    fixture.update("--version", fixture.version, "--yes", old=False)
    fixture.complete(old=False, expected_previous=prior)
    completed = json.loads(fixture.journal.read_text())
    require(completed["transaction"] != journal["transaction"] and not completed["steps"],
            "fresh activation repair replayed migration steps")
    history_preserves_evidence(fixture, preserved)
    gate = fixture.check(old=False)
    require(gate["outcome"]["status"] == "ready", "fresh repair did not reach runtime readiness")
    admission = verify_history_source_admission(fixture, control, checkpointed, "after-repair")
    if admission["status"] == "supported":
        require(any(decision.get("resource") == "transition.source-metadata" and decision.get("decision") == "preserve"
                    for decision in gate.get("decisions", [])), "supported source metadata admission lacks its preservation decision")
    return admission



def history_final_pending_migration(bundle, ledger_path):
    """Bind the ledger observer to the last pending edge in packaged registry order."""
    with tarfile.open(bundle, 'r:gz') as archive:
        members = [member for member in archive if member.name.removeprefix('./') == 'config/release-transition.json']
        require(len(members) == 1 and members[0].isfile(), 'candidate migration registry is unavailable or ambiguous')
        payload = archive.extractfile(members[0]).read((1 << 20)+1)
        require(len(payload) <= 1 << 20, 'candidate migration registry exceeds its bound')
        registry = json.loads(payload)
    ledger = json.loads(ledger_path.read_text())
    pending = [migration for migration in registry['migrations']
               if migration['toEpoch'] > ledger['domains'][migration['domain']]['epoch']]
    require(pending, 'ledger-cut fixture has no pending migration')
    return pending[-1]['id']


def verify_history_process_cuts(release, version, arch, root, baselines):
    observer = Path(__file__).resolve().parent / "e2e/release-transition-post-cas-observer.py"
    for cut in ("ledger", "checkpoint"):
        fixture, prior, _ = history_chain(release, version, arch, root, baselines, False, "history-cut-" + cut)
        original = json.loads(fixture.journal.read_text())
        checkpoint = fixture.config / "release-transition/v2/history-checkpoint.json"
        marker = fixture.root / (cut + "-observed.json")
        bundle = release / f"subyard-{version}-linux-{arch}.tar.gz"
        target = version + "-" + digest(bundle)[:12]
        command = ["python3", str(observer), "--runtime-root", str(fixture.runtime), "--journal", str(fixture.journal),
                   "--source-transaction", original["transaction"], "--candidate-target", target,
                   "--marker", str(marker), "--timeout", "90"]
        if cut == "checkpoint": command += ["--checkpoint", str(checkpoint)]
        else:
            ledger = checkpoint.parent / 'ledger.json'
            # Earlier edges can already be superseded when inotify is delivered.
            # The last pending CAS remains observable through activation readiness.
            command += ['--ledger', str(ledger), '--migration', history_final_pending_migration(bundle, ledger)]
        command += ["--", str(fixture.old_launcher), "update", "--version", version, "--yes"]
        result = run_process(command, fixture.env, timeout=120)
        require(result.returncode == -signal.SIGKILL and marker.exists(),
                f"{cut} process did not stop at its observed durable CAS: {result.stdout}{result.stderr}")
        (HISTORY_EVIDENCE / (cut + "-cut.json")).write_bytes(marker.read_bytes())
        (HISTORY_EVIDENCE / (cut + "-stopped-journal.json")).write_bytes(fixture.journal.read_bytes())
        shutil.copytree(fixture.config, HISTORY_EVIDENCE / (cut + "-stopped-config"))
        (HISTORY_EVIDENCE / (cut + "-stopped-links.json")).write_text(json.dumps(history_links(fixture)) + "\n")
        stopped = json.loads(fixture.journal.read_text())
        evidence_root = checkpoint.parent / 'transactions' / stopped['transaction']
        allowed_destinations = history_evidence_destinations(stopped, transaction_prefix=False)
        stopped_evidence = capture_history_evidence(evidence_root, allowed_destinations)
        binding = tuple(stopped[key] for key in ("transaction", "authorizationPlan", "resumePlan", "observationScope"))
        if cut == "ledger": require(not checkpoint.exists(), "history converted before activation readiness")
        before = snapshot(fixture.config)
        launcher = fixture.runtime / "releases" / target / "bin/yard"
        checked = run_process([str(launcher), "update", "--check", "--version", version], fixture.env, timeout=90)
        inspection = json.loads(checked.stdout)
        require(checked.returncode == 0 and snapshot(fixture.config) == before and inspection["outcome"]["status"] in {"recovering", "ready"},
                "fresh packaged process rejected a valid durable process cut: " + checked.stdout + checked.stderr)
        try:
            fixture.run([str(launcher), "update", "--version", version, "--yes"])
            fixture.complete(old=False, expected_previous=prior)
        except Exception:
            shutil.copytree(fixture.config, HISTORY_EVIDENCE / (cut + "-failed-config"))
            (HISTORY_EVIDENCE / (cut + "-failed-links.json")).write_text(json.dumps(history_links(fixture)) + "\n")
            raise
        complete = json.loads(fixture.journal.read_text())
        verify_history_evidence(evidence_root, stopped_evidence, allowed_destinations)
        require(tuple(complete[key] for key in ("transaction", "authorizationPlan", "resumePlan", "observationScope")) == binding,
                "process cut lost the durable authorization or scope")
        (HISTORY_EVIDENCE / (cut + "-cut.json")).write_bytes(marker.read_bytes())
        print(f"PASS: real {cut} CAS interruption resumes its durable grant in a fresh process", flush=True)



def verify_migration_history(release, version, arch, root, baselines, evidence, case="all"):
    global HISTORY_EVIDENCE, HISTORY_RESULTS
    evidence = evidence.resolve()
    if (evidence / "native-results.json").exists():
        evidence = evidence.with_name(evidence.name + "-" + str(time.time_ns()))
    evidence.mkdir(parents=True, exist_ok=True)
    print("Migration history evidence: " + str(evidence), flush=True)
    HISTORY_EVIDENCE, HISTORY_RESULTS = evidence, []
    control = root / "incus-observation.json"
    results = []
    try:
        required = []
        if case in ("all", "cuts", "legacy"):
            required.extend((BASELINE, PUBLISHED_BASELINE))
        if case in ("all", "checkpoint"):
            required.extend((SOURCE_BASELINE, HISTORY_BASELINE))
        # Own downloads once for this corpus, separate from later compatibility
        # baselines. Every chain still revalidates through the pinned-dir helper.
        inputs_root = root / "migration-history-baselines"
        inputs_root.mkdir(mode=0o700)
        inputs = {}
        for baseline_version in required:
            directory = baselines / ("baseline-" + baseline_version) if baselines is not None else None
            inputs[baseline_version] = baseline_assets(inputs_root, directory, baseline_version,
                                                       HISTORY_BASELINES[baseline_version], arch)
        if case in ("all", "cuts"):
            verify_history_process_cuts(release, version, arch, root, inputs)
        if case == "cuts":
            return
        authorities = (True, False) if case == "all" else (case == "checkpoint",)
        for checkpointed in authorities:
            label = "checkpoint" if checkpointed else "legacy"
            fixture, prior, prior_evidence = history_chain(release, version, arch, root, inputs,
                                                          checkpointed, "history-" + label)
            projection = fixture.config / "release-transition/v2/ledger.json"
            projection_before = projection.read_bytes()
            history, source, guest, unexpected = history_materialized_fixture(fixture, root, control)
            effects_before_apply = history_effects(control)
            result = run_process([str(fixture.old_launcher), "update", "--version", version, "--yes"], fixture.env, timeout=180)
            require(result.returncode != 0 and Path(str(control) + ".failed").exists(),
                    "materialized config did not fail at the actual post-migration apply: " + result.stdout + result.stderr)
            journal = json.loads(fixture.journal.read_text())
            require(journal["checkpoint"] == "reconciling" and os.readlink(fixture.runtime / "current") == "releases/" + history["target"]
                    and len(journal["steps"]) == 2 and all(step["checkpoint"] == "verified" for step in journal["steps"]),
                    "controlled apply error did not leave verified migrations and unfinished activation")
            diagnostic = result.stdout + result.stderr
            require("yard default: cannot apply materialized config for release activation" in diagnostic
                    and "run yard -Y default config status" in diagnostic,
                    "original materialized-config diagnostic or retry disappeared: " + diagnostic)
            effects_after_apply = history_effects(control)
            require(effects_after_apply.startswith(effects_before_apply), "apply rewrote prior fixture effects")
            effects = [json.loads(line) for line in effects_after_apply[len(effects_before_apply):].splitlines()]
            failed_apply = [event for event in effects if event.get("exit") == 37]
            apply_script = (Path(__file__).resolve().parent.parent /
                            "internal/testkit/cmd/empty-incus/config-write.sh").read_text()
            require(len(failed_apply) == 1
                    and failed_apply[0].get("command") == ["sh", "-eu", "-c", apply_script,
                                                           "subyard", history["destination"], "1000"]
                    and failed_apply[0].get("stdinSHA256") == digest(source)
                    and failed_apply[0].get("transaction") == journal,
                    "controlled failure did not capture the exact seeded apply command/stdin and verified journal")
            if checkpointed: require(projection.read_bytes() == projection_before, "checkpoint authority changed the frozen projection")
            history_preserves_evidence(fixture, prior_evidence)
            verify_history_safety(fixture, control, checkpointed)
            migration_evidence = capture_history_evidence(fixture.config / "release-transition/v2/transactions",
                                                         history_evidence_destinations(journal))
            (evidence / (label + "-evidence-before-retry.json")).write_text(json.dumps(migration_evidence, indent=2,
                default=lambda record: record.hex() if isinstance(record, bytes) else asdict(record)) + "\n")
            shutil.copytree(fixture.config, evidence / (label + "-before-retry-config"))
            inspection = history_inspect(fixture, control)
            require(inspection["outcome"]["status"] == "recovering" and not inspection.get("blockers"),
                    "new packaged process rejected valid completed domain history: " + json.dumps(inspection))
            fixture.update("--version", version, "--yes", old=False)
            fixture.complete(old=False, expected_previous=prior)
            history_preserves_evidence(fixture, migration_evidence)
            require(guest.read_bytes() == source.read_bytes() and not unexpected.exists()
                    and not Path(str(control) + ".unexpected").exists(), "retry did not apply only the expected fixture effect")
            if checkpointed: require(projection.read_bytes() == projection_before, "ready retry rewrote the frozen projection")
            before = history_no_effects(fixture, control)
            fixture.update("--version", version, "--yes", old=False)
            require(history_no_effects(fixture, control) == before,
                    "repeated Ready update performed additional writes")
            before = history_no_effects(fixture, control)
            current = fixture.check(old=False)
            retained = fixture.check(old=True)
            require(current["outcome"]["status"] == retained["outcome"]["status"] == "ready",
                    "current and actual retained updater disagree on completed history")
            require(history_no_effects(fixture, control) == before, "completed caller inspections performed effects")
            source_before = verify_history_source_admission(fixture, control, checkpointed, "before-repair", mutate=True)
            source_after = verify_history_fresh_repair(fixture, control, history, guest, checkpointed)
            results.append({"case": label, "status": "passed", "prior": prior,
                            "transaction": journal["transaction"], "projectionSHA256": digest(projection),
                            "priorEvidenceFiles": len(prior_evidence), "migrationEvidenceFiles": len(migration_evidence),
                            "sourceAdmissionBefore": source_before, "sourceAdmissionAfter": source_after})
            (evidence / (label + "-journal.json")).write_bytes(fixture.journal.read_bytes())
            (evidence / "summary.json").write_text(json.dumps(results, indent=2) + "\n")
            set_fixture_observation(control)
            Path(str(control) + ".failed").unlink(missing_ok=True)
            Path(str(control) + ".drift").unlink(missing_ok=True)
            print(f"PASS: {label} authentic history survives materialized apply error and fresh-process retry", flush=True)
    except Exception as error:
        (evidence / "failed.json").write_text(json.dumps({"error": str(error), "cases": results}, indent=2) + "\n")
        if "fixture" in locals():
            shutil.copytree(fixture.config, evidence / "failed-config", dirs_exist_ok=True)
            (evidence / "failed-links.json").write_text(json.dumps(history_links(fixture)) + "\n")
            for rejected in fixture.root.glob("unexpected-host-command*"):
                shutil.copy2(rejected, evidence / rejected.name)
        raise
    finally:
        for suffix in (".effects.jsonl", ".unexpected", ".failed", ".drift"):
            artifact = Path(str(control) + suffix)
            if artifact.exists(): shutil.copy2(artifact, evidence / ("fixture" + suffix))
        HISTORY_EVIDENCE = None



def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--release-dir", type=Path, required=True)
    parser.add_argument("--version", required=True)
    parser.add_argument("--failure-dir", type=Path,
                        default=Path.cwd() / ".build/release-upgrade-failures",
                        help="private directory for bounded recovery-preservation failure artifacts")
    parser.add_argument("--baseline-dir", type=Path,
                        help="optional directory containing the pinned official baseline assets")
    parser.add_argument("--legacy-baseline-dir", type=Path,
                        help="optional directory containing the pinned official legacy baseline assets")
    parser.add_argument("--activation-baseline-dir", type=Path,
                        help="optional directory containing the pinned activation-recovery baseline assets")
    parser.add_argument("--published-baseline-dir", type=Path,
                        help="optional directory containing the pinned published-caller baseline assets")
    parser.add_argument("--source-baseline-dir", type=Path,
                        help="optional directory containing the pinned source-repair baseline assets")
    parser.add_argument("--only-legacy-retirement", action="store_true",
                        help="run retired-source refusal/full-uninstall and actual v0.11.0 admission")
    parser.add_argument("--only-source-repair", action="store_true",
                        help="run actual released completed and interrupted source-repair contracts")
    parser.add_argument("--only-activation-recovery", action="store_true",
                        help="run only the published activation-only recovery regression")
    parser.add_argument("--only-checkpoint-source", action="store_true",
                        help="run only released-caller checkpoint source and future rollback compatibility")
    parser.add_argument("--only-runtime-recovery", action="store_true",
                        help="run published runtime continuation/rollback and exact sealed activation-owner recovery")
    parser.add_argument("--only-fresh-activation-recovery", action="store_true",
                        help="run the owning fresh activation-recovery contract and matching-epoch bridge")
    parser.add_argument("--only-power-publication", action="store_true",
                        help="run actual released caller power reader upgrade and rollback refusal")
    parser.add_argument("--only-power-transition-regression", action="store_true",
                        help="run affected rollback, checkpoint-reader bridge and fresh recovery contracts")
    parser.add_argument("--only-migration-owner-replacement", action="store_true",
                        help="run exact released migration-ledger owner supersession regression")
    parser.add_argument("--migration-owner-baseline-dir", type=Path)
    parser.add_argument("--migration-owner-source-dir", type=Path)
    parser.add_argument("--migration-owner-source-version")
    parser.add_argument("--migration-owner-evidence-dir", type=Path)
    parser.add_argument("--only-direct-019", action="store_true")
    parser.add_argument("--direct-baseline-dir", type=Path)
    parser.add_argument("--only-migration-history", action="store_true",
                        help="run authentic independent-domain history, recovery and safety contracts")
    parser.add_argument("--migration-history-case", choices=("all", "checkpoint", "legacy", "cuts"), default="all",
                        help="isolate one migration-history authority or the short interruption diagnostics")
    parser.add_argument("--migration-history-baselines", type=Path,
                        help="optional parent of pinned baseline-VERSION directories")
    args = parser.parse_args()
    focused = (args.only_direct_019, args.only_migration_owner_replacement, args.only_fresh_activation_recovery,
               args.only_activation_recovery, args.only_checkpoint_source, args.only_source_repair,
               args.only_runtime_recovery, args.only_power_publication, args.only_power_transition_regression,
               args.only_legacy_retirement, args.only_migration_history)
    require(sum(focused) <= 1,
            "choose only one focused compatibility regression")
    require(args.migration_history_case == "all" or args.only_migration_history,
            "--migration-history-case requires --only-migration-history")
    arch = {"x86_64": "amd64", "aarch64": "arm64"}.get(platform.machine())
    require(platform.system() == "Linux" and arch in BASELINE_SHA256, "unsupported platform")
    release = args.release_dir.resolve()
    require(args.version not in (BASELINE, LEGACY_BASELINE, ACTIVATION_BASELINE, PUBLISHED_BASELINE, SOURCE_BASELINE),
            "candidate must differ from the released baselines")
    check_scratch_capacity(release / f"subyard-{args.version}-linux-{arch}.tar.gz",
                           selected_baseline_archives(args, release, arch), "before preparation")
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
                if args.only_fresh_activation_recovery:
                    baseline = baseline_assets(root, args.baseline_dir, BASELINE, BASELINE_SHA256, arch)
                    verify_fresh_activation_recovery(release, args.version, baseline, arch, root,
                                                     failure_dir=args.failure_dir, evidence_dir=args.migration_owner_evidence_dir)
                    return
                if args.only_direct_019:
                    require(arch in DIRECT_BASELINE_SHA256, "published direct baseline is not pinned for this architecture")
                    require(bool(args.migration_owner_source_dir) == bool(args.migration_owner_source_version),
                            "custom predecessor directory and version must be supplied together")
                    direct = baseline_assets(root, args.direct_baseline_dir, DIRECT_BASELINE,
                                             DIRECT_BASELINE_SHA256, arch)
                    predecessors = [(direct, DIRECT_BASELINE)]
                    if args.migration_owner_source_dir:
                        predecessors.append((args.migration_owner_source_dir.resolve(), args.migration_owner_source_version))
                    for baseline, baseline_version in predecessors:
                        try:
                            verify_direct_019(release, args.version, baseline, baseline_version, arch, root)
                        finally:
                            if args.migration_owner_evidence_dir:
                                evidence = args.migration_owner_evidence_dir.resolve() / baseline_version
                                evidence.mkdir(parents=True, exist_ok=True)
                                fixture_root = root / ("direct-" + baseline_version)
                                for name in ("source-journal.json", "post-effects-failure.log",
                                             "post-effects-inspection.json", "resume-inspection.json", "instance-cleanup.json"):
                                    source_file = fixture_root / name
                                    if source_file.is_file():
                                        shutil.copy2(source_file, evidence / name)
                                protected = fixture_root / "home/.config/subyard/release-transition"
                                if protected.is_dir():
                                    shutil.copytree(protected, evidence / "protected-state", dirs_exist_ok=True)
                    return
                if args.only_migration_owner_replacement:
                    require(arch in MIGRATION_OWNER_SHA256, "no verified affected owner artifact is pinned for this architecture")
                    require(args.migration_owner_source_dir and args.migration_owner_source_version,
                            "authentic retained source bundle directory and version are required")
                    baseline = baseline_assets(root, args.migration_owner_baseline_dir,
                                               MIGRATION_OWNER_BASELINE, MIGRATION_OWNER_SHA256, arch)
                    history = baseline_assets(root, args.direct_baseline_dir, DIRECT_BASELINE,
                                              DIRECT_BASELINE_SHA256, arch)
                    try:
                        verify_migration_owner_replacement(release, args.version, baseline,
                            args.migration_owner_source_dir.resolve(), args.migration_owner_source_version, history, arch, root)
                    finally:
                        if args.migration_owner_evidence_dir:
                            evidence = args.migration_owner_evidence_dir.resolve()
                            evidence.mkdir(parents=True, exist_ok=True)
                            fixture_root = root / "migration-owner"
                            for name in ("official-source-journal.json", "custom-source-journal.json", "source-history.json", "shipped-default-plan-stability.json", "previous-asset-refusal.log",
                                         "predecessor.json", "released-owner-failure.log",
                                         "released-owner-inspection.log", "released-owner-unavailable-observation",
                                         "migration-owner-successor.json", "migration-owner-successor.json.stdout",
                                         "migration-owner-successor.json.stderr"):
                                source_file = fixture_root / name
                                if source_file.is_file():
                                    shutil.copy2(source_file, evidence / name)
                            protected = fixture_root / "home/.config/subyard/release-transition"
                            if protected.is_dir():
                                shutil.copytree(protected, evidence / "protected-state", dirs_exist_ok=True)
                    return
                if args.only_migration_history or not any(focused):
                    verify_migration_history(release, args.version, arch, root,
                                             args.migration_history_baselines, args.failure_dir / "migration-history", args.migration_history_case)
                    if args.only_migration_history:
                        return
                if args.only_legacy_retirement or not any(focused):
                    for baseline_version, hashes in RETIRED_BASELINES.items():
                        directory = args.legacy_baseline_dir if baseline_version == LEGACY_BASELINE else None
                        baseline = baseline_assets(root, directory, baseline_version, hashes, arch)
                        verify_legacy(release, args.version, baseline, arch, root, baseline_version)
                    baseline = baseline_assets(root, None, MINIMUM_BASELINE, MINIMUM_BASELINE_SHA256, arch)
                    verify_minimum_source(release, args.version, baseline, arch, root)
                    if args.only_legacy_retirement:
                        return
                if args.only_power_transition_regression:
                    for baseline_version, hashes, directory in (
                            (BASELINE, BASELINE_SHA256, args.baseline_dir),
                            (PUBLISHED_BASELINE, PUBLISHED_BASELINE_SHA256, args.published_baseline_dir),
                            (SOURCE_BASELINE, SOURCE_BASELINE_SHA256, args.source_baseline_dir)):
                        cached = release.parent / "power-publication-baselines" / baseline_version
                        if directory is None and cached.is_dir():
                            directory = cached
                        baseline = baseline_assets(root, directory, baseline_version, hashes, arch)
                        if directory is None:
                            cached.mkdir(parents=True, exist_ok=True)
                            for asset in baseline.iterdir():
                                if asset.is_file():
                                    shutil.copy2(asset, cached / asset.name)
                        verify(release, args.version, baseline, arch, root, baseline_version)
                        verify_checkpoint_source(release, args.version, baseline, arch, root, baseline_version)
                        if baseline_version == BASELINE:
                            verify_fresh_activation_recovery(release, args.version, baseline, arch, root, failure_dir=args.failure_dir, evidence_dir=args.migration_owner_evidence_dir)
                    return
                if args.only_power_publication:
                    for baseline_version, hashes, directory in (
                            (PUBLISHED_BASELINE, PUBLISHED_BASELINE_SHA256, args.published_baseline_dir),
                            (SOURCE_BASELINE, SOURCE_BASELINE_SHA256, args.source_baseline_dir)):
                        cached = release.parent / "power-publication-baselines" / baseline_version
                        if directory is None and cached.is_dir():
                            directory = cached
                        baseline = baseline_assets(root, directory, baseline_version, hashes, arch)
                        if directory is None:
                            cached.mkdir(parents=True, exist_ok=True)
                            for asset in baseline.iterdir():
                                if asset.is_file():
                                    shutil.copy2(asset, cached / asset.name)
                        verify_power_publication_boundary(release, args.version, baseline, arch, root, baseline_version)
                    return
                if args.only_runtime_recovery:
                    baseline = baseline_assets(root, args.source_baseline_dir, SOURCE_BASELINE,
                                               SOURCE_BASELINE_SHA256, arch)
                    verify(release, args.version, baseline, arch, root, SOURCE_BASELINE)
                    activation = baseline_assets(root, args.activation_baseline_dir, ACTIVATION_BASELINE,
                                                 ACTIVATION_BASELINE_SHA256, arch)
                    verify_activation_only_recovery(release, args.version, activation, arch, root,
                                                   recovery_baseline=baseline)
                    return
                if args.only_source_repair:
                    source_baseline = baseline_assets(root, args.source_baseline_dir, SOURCE_BASELINE,
                                                      SOURCE_BASELINE_SHA256, arch)
                    verify_completed_source_repair(release, args.version, source_baseline, arch, root)
                    verify_activation_only_recovery(release, args.version, source_baseline, arch, root,
                                                   SOURCE_BASELINE, source_repair=True)
                    return
                if not args.only_activation_recovery and not args.only_checkpoint_source:
                    direct = baseline_assets(root, args.direct_baseline_dir, DIRECT_BASELINE,
                                             DIRECT_BASELINE_SHA256, arch)
                    verify_direct_019(release, args.version, direct, DIRECT_BASELINE, arch, root)
                if not args.only_activation_recovery:
                    for baseline_version, hashes, directory in (
                            (BASELINE, BASELINE_SHA256, args.baseline_dir),
                            (PUBLISHED_BASELINE, PUBLISHED_BASELINE_SHA256, args.published_baseline_dir),
                            (SOURCE_BASELINE, SOURCE_BASELINE_SHA256, args.source_baseline_dir)):
                        baseline = baseline_assets(root, directory, baseline_version, hashes, arch)
                        if not args.only_checkpoint_source:
                            verify(release, args.version, baseline, arch, root, baseline_version)
                        verify_checkpoint_source(release, args.version, baseline, arch, root, baseline_version)
                        if baseline_version == BASELINE and not args.only_checkpoint_source:
                            verify_fresh_activation_recovery(release, args.version, baseline, arch, root, failure_dir=args.failure_dir, evidence_dir=args.migration_owner_evidence_dir)
                        if baseline_version == SOURCE_BASELINE and not args.only_checkpoint_source:
                            verify_completed_source_repair(release, args.version, baseline, arch, root)
                            verify_activation_only_recovery(release, args.version, baseline, arch, root,
                                                           SOURCE_BASELINE, source_repair=True)
                    if args.only_checkpoint_source:
                        return
                activation = baseline_assets(root, args.activation_baseline_dir, ACTIVATION_BASELINE,
                                             ACTIVATION_BASELINE_SHA256, arch)
                verify_activation_only_recovery(release, args.version, activation, arch, root,
                                               recovery_baseline=args.source_baseline_dir)
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
        if version in RETIRED_BASELINES:
            names.append("subyard-install-runtime-release.sh")
        for name in names:
            url = f"https://github.com/Subyard/Subyard/releases/download/v{version}/{name}"
            with urllib.request.urlopen(url, timeout=60) as response:
                (baseline / name).write_bytes(response.read())
    require(digest(baseline / bundle_name) == hashes[arch],
            "official baseline checksum does not match the pinned release")
    if version in RETIRED_BASELINES:
        require(digest(baseline / "subyard-install-runtime-release.sh") == RETIRED_INSTALLER_SHA256[version],
                "official legacy installer does not match the pinned release")
    check_scratch_capacity(baseline / bundle_name, stage=f"after {version} assets")
    return baseline


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, KeyError, RuntimeError, subprocess.TimeoutExpired) as error:
        print(f"release upgrade compatibility: {error}", file=sys.stderr)
        sys.exit(1)
