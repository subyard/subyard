#!/usr/bin/env python3
"""Exercise supported update contracts with unmodified released updaters."""

import argparse
import ctypes
from dataclasses import dataclass
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import runpy
import select
import signal
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


# Immutable amd64 archive pins retained from the published-baseline table history.
RECENT_BASELINES = {
    "0.25.5": {"amd64": "061fa1e4616224e3a32c6424a62c8c2eff95818186cb089642265f75056113fd"},
    "0.26.0": {"amd64": "12c443cd103b014de27047c6eca1ea8af3612236b3031940b8b0df0cbad1022c"},
}


def published_anchor(root):
    """Return the unique update anchor's version and immutable asset pins."""
    rows = [fields for line in (root / "dev/e2e/published-upgrade-baselines.tsv").read_text().splitlines()
            if len(fields := line.split("\t")) == 4 and fields[3] == "anchor"]
    if len(rows) != 1:
        raise ValueError("expected exactly one published anchor")
    return rows[0]


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
    """Name only selected pinned inputs before creating any fixture."""
    selected = ordinary_baselines(args) if not args.only_legacy_retirement else []
    if not args.only_source_anchor:
        selected += [(v, hashes, args.legacy_baseline_dir if v == LEGACY_BASELINE else None)
                     for v, hashes in RETIRED_BASELINES.items()]
    archives = []
    for version, hashes, directory in selected:
        if arch not in hashes:
            continue
        bundle = directory.resolve() / f"subyard-{version}-linux-{arch}.tar.gz" if directory else None
        if bundle is not None and bundle.is_file():
            require(digest(bundle) == hashes[arch], "official baseline checksum does not match the pinned release")
        archives.append(bundle)
    return archives


def ordinary_baselines(args):
    return [(MINIMUM_BASELINE, MINIMUM_BASELINE_SHA256, None),
            (BASELINE, BASELINE_SHA256, args.baseline_dir),
            (ACTIVATION_BASELINE, ACTIVATION_BASELINE_SHA256, args.activation_baseline_dir),
            (PUBLISHED_BASELINE, PUBLISHED_BASELINE_SHA256, args.published_baseline_dir),
            (SOURCE_BASELINE, SOURCE_BASELINE_SHA256, args.source_baseline_dir),
            (DIRECT_BASELINE, DIRECT_BASELINE_SHA256, args.direct_baseline_dir),
            (MIGRATION_OWNER_BASELINE, MIGRATION_OWNER_SHA256, args.migration_owner_baseline_dir),
            *[(version, hashes, None) for version, hashes in RECENT_BASELINES.items()]]


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
    stdin_probes = []
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
            for arguments, payload, response, code, digest_command in provider.get("read_only_stdin_commands", lambda _: [])(package):
                require(arguments.count(None) == 1 and all(isinstance(arg, str) or arg is None for arg in arguments)
                        and isinstance(payload, bytes) and isinstance(response, str)
                        and type(code) is int and 0 <= code <= 255
                        and isinstance(digest_command, list) and all(isinstance(arg, str) for arg in digest_command),
                        "package stdin observation fixture is invalid")
                stdin_probes.append((arguments, payload, response, code, digest_command))
            for program, arguments, response, code in provider.get("read_only_host_commands", lambda _: [])(package):
                require(program in ("sudo", "systemctl") and type(code) is int and 0 <= code <= 255,
                        "package host observation fixture is invalid")
                key = (program, tuple(arguments))
                require(key not in host_probes or host_probes[key] == (response, code),
                        "package host observation fixtures disagree on an exact command")
                host_probes[key] = (response, code)
    incus = shims / "incus"
    # Exact observations only; profile digest helpers never execute guest input.
    incus.write_text("#!/usr/bin/python3\nimport hashlib, json, pathlib, subprocess, sys, tarfile\n"
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
                     f"stdin_probes = {stdin_probes!r}\n"
                     "incoming = None\n"
                     "for expected, payload, response, code, digest_command in stdin_probes:\n"
                     "    if len(arguments) != len(expected): continue\n"
                     "    dynamic = expected.index(None)\n"
                     "    if arguments[:dynamic] != expected[:dynamic] or arguments[dynamic+1:] != expected[dynamic+1:]: continue\n"
                     "    digest = subprocess.run(digest_command, capture_output=True, text=True)\n"
                     "    if incoming is None: incoming = sys.stdin.buffer.read()\n"
                     "    if digest.returncode == 0 and arguments[dynamic] == digest.stdout.strip() and incoming == payload:\n"
                     "        sys.stdout.write(response)\n"
                     "        sys.exit(code)\n"
                     "if tuple(arguments) in probes:\n"
                     "    sys.stdin.buffer.read()\n"
                     "    response, code = probes[tuple(arguments)]\n"
                     "    sys.stdout.write(response)\n"
                     "    sys.exit(code)\n"
                     "if arguments[:7] == ['list', 'yard', '--project', 'subyard', '-f', 'csv', '-c'] and arguments[7:] in (['s'], ['ns']):\n"
                     f"    observation = json.loads(pathlib.Path({str(control)!r}).read_text())\n"
                     "    # The exact-name read lists the name column; a neighbour shares the prefix.\n"
                     "    if arguments[7] == 'ns': print('yard-neighbour,STOPPED')\n"
                     "    if observation.get('instances'):\n"
                     "        print(('yard,' if arguments[7] == 'ns' else '') + observation['instances'][0]['info']['status'].upper())\n"
                     "    elif arguments[7] == 's': print('')\n"
                     "    sys.exit(0)\n"
                     "if len(arguments) == 9 and arguments[:8] == ['exec', 'yard', '--project', 'subyard', '--', 'systemctl', 'is-active', '--quiet'] and arguments[8].endswith('.service'):\n"
                     "    sys.exit(3)\n"
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


def snapshot(root):
    return {
        str(path.relative_to(root)): (path.stat().st_mode, digest(path))
        for path in root.rglob("*") if path.is_file()
    }


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


def set_fixture_observation(control, status=None, journal=None, history=None, owned_project=None, guest=None):
    instances = [] if status is None else [{"project": "subyard", "name": "yard",
                                            "info": {"name": "yard", "status": status,
                                                     "type": "container", "config": {}, "devices": {}}}]
    if (history is not None or owned_project is not None) and instances:
        # Explicit owned cases model an existing managed, disk-backed container;
        # it exercises materialized activation, not physical legacy enrollment.
        config = {"volatile.uuid": "history-owned-yard", "user.subyard.managed": "true",
                  "user.subyard.name": "default", "user.subyard.bridge": "incusbr0",
                  "user.subyard.desired_power": status.lower(), "user.subyard.disk_tmp": "v1"}
        instances[0]["info"].update(config=config, expanded_config=config,
                                    devices={}, expanded_devices={})
    observation = {"instances": instances, "holdJournal": str(journal) if journal else "", "migrationHistory": history}
    if guest is not None and instances:
        observation["observedGuest"] = guest
    project_config = history.get("projectConfig") if history is not None else owned_project
    if project_config is not None:
        observation["projects"] = {"subyard": {"name": "subyard", "config": project_config}}
    payload = json.dumps(observation)
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
    shutil.rmtree(fixture.root)
    print(f"PASS: actual released {baseline_version} refuses upgrade, stages cleanup without activation, "
          "fully uninstalls and permits a separate clean install", flush=True)


def verify_source_anchor_refusal(fixture, installer=None):
    before = snapshot(fixture.config)
    links = {name: os.readlink(fixture.runtime / name) if (fixture.runtime / name).is_symlink() else None
             for name in ("current", "previous", "next")}
    marker = fixture.project_marker.read_bytes()
    # Select the local packaged target without a GitHub latest-release lookup.
    # Both invocations otherwise use the real released caller's ordinary path.
    environment = dict(fixture.env, YARD_RELEASE_VERSION=fixture.version)
    attempts = [(arguments, [str(fixture.old_launcher), "update", *arguments])
                for arguments in ([], ["--check"])]
    if installer is not None:
        attempts.append((["installer"], ["bash", str(installer), "--version", fixture.version, "--yes"]))
    for arguments, command in attempts:
        refused = run_process(command, environment, timeout=180)
        reported_refusal = refused.returncode != 0
        if arguments == ["--check"] and not reported_refusal:
            # Older callers can return success for a completed inspection whose
            # JSON outcome still reports that the requested goal was refused.
            try:
                report = json.loads(refused.stdout)
            except json.JSONDecodeError:
                report = {}
            outcome = report.get("outcome") if isinstance(report, dict) else None
            reported_refusal = (isinstance(outcome, dict) and outcome.get("reachedGoal") is False
                                and isinstance(outcome.get("status"), str)
                                and outcome["status"] not in ("", "ready"))
        require(reported_refusal and "source-anchor-required" in refused.stderr
                and f"next: yard update --version {published_anchor(Path(__file__).resolve().parent.parent)[0]} && yard update" in refused.stderr
                and "uninstall --full" not in refused.stderr,
                f"released {fixture.baseline_version} update {arguments} lost anchor guidance "
                f"(exit={refused.returncode}): "
                + refused.stdout + refused.stderr)
        require(snapshot(fixture.config) == before and fixture.project_marker.read_bytes() == marker
                and digest(fixture.retained) == fixture.retained_hash
                and all((os.readlink(fixture.runtime / name) if (fixture.runtime / name).is_symlink() else None) == value
                        for name, value in links.items()),
                "source-anchor refusal changed protected config, runtime links or project data")
    # Immutable release staging is allowed; config/journal, stable links and data are not.
    print(f"PASS: released {fixture.baseline_version} update/check" + ("/installer" if installer else "")
          + " refuse below-anchor admission without mutation (immutable staging allowed)",
          flush=True)


def verify_minimum_source(release, version, baseline, arch, root):
    fixture = Fixture(root / "minimum-source", release, version, baseline, arch, MINIMUM_BASELINE)
    verify_source_anchor_refusal(fixture, release / "subyard-install.sh")
    shutil.rmtree(fixture.root)


def verify(release, version, baseline, arch, root, baseline_version=BASELINE):
    normal = Fixture(root / f"normal-{baseline_version}", release, version, baseline, arch, baseline_version)
    verify_source_anchor_refusal(normal, release / "subyard-install.sh" if baseline_version == MINIMUM_BASELINE else None)
    shutil.rmtree(normal.root)


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


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--release-dir", type=Path, required=True)
    parser.add_argument("--version", required=True)
    for name in ("baseline", "legacy-baseline", "activation-baseline", "published-baseline",
                 "source-baseline", "direct-baseline", "migration-owner-baseline"):
        parser.add_argument("--" + name + "-dir", type=Path)
    focused = parser.add_mutually_exclusive_group()
    focused.add_argument("--only-source-anchor", action="store_true",
                         help="run update/check refusal with every pinned ordinary released caller")
    focused.add_argument("--only-legacy-retirement", action="store_true",
                         help="run retired-source refusal, staged full uninstall and separate clean installation")
    args = parser.parse_args()
    arch = {"x86_64": "amd64", "aarch64": "arm64"}.get(platform.machine())
    require(platform.system() == "Linux" and arch in BASELINE_SHA256, "unsupported platform")
    release = args.release_dir.resolve()
    require(args.version not in {version for version, _, _ in ordinary_baselines(args)},
            "candidate must differ from the released baselines")
    check_scratch_capacity(release / f"subyard-{args.version}-linux-{arch}.tar.gz",
                           selected_baseline_archives(args, release, arch), "before preparation")
    os.umask(0o077)
    with tempfile.TemporaryDirectory(prefix="subyard-release-compat-", dir="/tmp") as directory:
        root = Path(directory)
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
                readiness = server.stdout.readline().strip() if readable else "readiness timeout"
                require(readiness == "ready", "Incus fixture did not become ready: " + readiness)
                if not args.only_legacy_retirement:
                    for version, hashes, directory in ordinary_baselines(args):
                        if arch in hashes:
                            baseline = baseline_assets(root, directory, version, hashes, arch)
                            verify(release, args.version, baseline, arch, root, version)
                if not args.only_source_anchor:
                    for version, hashes in RETIRED_BASELINES.items():
                        directory = args.legacy_baseline_dir if version == LEGACY_BASELINE else None
                        baseline = baseline_assets(root, directory, version, hashes, arch)
                        verify_legacy(release, args.version, baseline, arch, root, version)
                    if args.only_legacy_retirement:
                        baseline = baseline_assets(root, None, MINIMUM_BASELINE, MINIMUM_BASELINE_SHA256, arch)
                        verify_minimum_source(release, args.version, baseline, arch, root)
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
