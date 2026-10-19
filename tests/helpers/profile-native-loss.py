#!/usr/bin/env python3
"""Observe three fixed cuts of a marked installed-profile migration fixture."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import signal
import stat
import subprocess
import tarfile
import tempfile
import time


DECLARATION = dict(schemaVersion=1, kind='rust-release-owner', protocolVersion=1,
                   owner='bin/subyard-release-owner', helper='bin/yard-engine',
                   schema='config/release-owner.schema.json', buildInputs='config/release-owner-build.json')


def sha(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def runtime_identity(root, expected):
    """Bind the actual normal images to the coordinator-verified whole manifest."""
    root = Path(root).resolve(strict=True)
    manifest = root/'runtime-files.sha256'
    if manifest.is_symlink() or sha(manifest) != expected:
        raise ValueError('installed manifest differs from the frozen target')
    inventory = {}
    for line in manifest.read_text().splitlines():
        digest, name = line.split(None, 1)
        relative = Path(name.removeprefix('./'))
        if relative.is_absolute() or '..' in relative.parts or str(relative) in inventory:
            raise ValueError('unsafe or duplicate runtime manifest entry')
        path = root/relative
        if path.is_symlink() or not path.is_file() or sha(path) != digest:
            raise ValueError('installed runtime manifest mismatch')
        inventory[str(relative)] = digest
    if json.loads((root/'config/release-owner.json').read_text()) != DECLARATION:
        raise ValueError('loss fixture requires the declared Rust target contract')
    for name in ('bin/yard', 'config/release-owner.json',
                 *[DECLARATION[key] for key in ('owner', 'helper', 'schema', 'buildInputs')]):
        if name not in inventory:
            raise ValueError('owner asset is not in the verified whole manifest')
    for name in ('bin/yard', DECLARATION['owner'], DECLARATION['helper']):
        mode = (root/name).stat().st_mode
        if not mode & 0o100 or mode & 0o022:
            raise ValueError('owner executable mode is unsafe')
    build = json.loads((root/DECLARATION['buildInputs']).read_text())
    if (build.get('schemaVersion') != 1 or build.get('kind') != 'rust-release-owner-build' or build.get('features') != []
            or build.get('locked') is not True or build.get('imageSHA256') != inventory[DECLARATION['owner']]):
        raise ValueError('loss fixture requires a normal locked Rust image')
    return root, inventory


def checked_version(root, version):
    # Use the existing release-fixture version contract after checking every byte.
    result = subprocess.run([str(root/'bin/yard-engine'), '--version'], capture_output=True, timeout=10)
    if result.returncode != 0 or result.stdout.decode().strip() != 'yard-engine '+version:
        raise ValueError('checked runtime engine version differs from release metadata')


def successor_identity(directory, current_version, current_manifest):
    arch = {'x86_64': 'amd64', 'aarch64': 'arm64'}.get(os.uname().machine)
    choices = list(Path(directory).glob(f'subyard-*-linux-{arch}.tar.gz.manifest.json'))
    if len(choices) != 1:
        raise ValueError('loss fixture requires one checked compatible successor for this architecture')
    metadata = json.loads(choices[0].read_text())
    version = metadata['version']
    archive = choices[0].with_name(choices[0].name.removesuffix('.manifest.json'))
    installer = Path(directory)/'subyard-install.sh'
    if installer.is_symlink() or not installer.is_file():
        raise ValueError('successor public release installer is unavailable')
    if (metadata.get('kind') != 'runtime' or metadata.get('arch') != arch or metadata.get('os') != 'linux'
            or version == current_version or archive.name != f'subyard-{version}-linux-{arch}.tar.gz'):
        raise ValueError('successor identity is not a distinct declared runtime')
    checksum, name = archive.with_name(archive.name+'.sha256').read_text().split()
    if name != archive.name or sha(archive) != checksum:
        raise ValueError('successor archive checksum mismatch')
    if metadata.get('artifact', archive.name) != archive.name or metadata.get('sha256', checksum) != checksum:
        raise ValueError('successor manifest artifact identity mismatch')
    provenance = archive.with_name(archive.name+'.provenance.json')
    if provenance.exists():
        value = json.loads(provenance.read_text())
        if (value.get('artifact'), value.get('sha256'), value.get('version')) != (archive.name, checksum, version):
            raise ValueError('successor provenance identity mismatch')
    with tempfile.TemporaryDirectory(prefix='subyard-loss-successor-', dir='/tmp') as temporary:
        with tarfile.open(archive, 'r:gz') as bundle:
            members = bundle.getmembers()
            if any(m.name.startswith('/') or '..' in m.name.split('/') or not (m.isfile() or m.isdir()) for m in members):
                raise ValueError('unsafe successor archive')
            bundle.extractall(temporary, filter='data')
        root = Path(temporary)
        manifest = sha(root/'runtime-files.sha256')
        _, inventory = runtime_identity(root, manifest)
        checked_version(root, version)
        if manifest == current_manifest:
            raise ValueError('successor checked runtime identity is unchanged')
    return dict(version=version, archive_sha256=checksum, manifest_sha256=manifest,
                engine_sha256=inventory['bin/yard-engine'])


def extract(archive, target):
    with tarfile.open(archive, 'r:gz') as bundle:
        members = bundle.getmembers()
        if any(m.name.startswith('/') or '..' in m.name.split('/') or not (m.isfile() or m.isdir()) for m in members):
            raise ValueError('unsafe frozen fixture bundle')
        bundle.extractall(target, filter='data')


def bundle_preflight(bundle, expected):
    if bundle.is_symlink() or sha(bundle) != expected:
        raise ValueError('frozen fixture bundle checksum mismatch')
    with tempfile.TemporaryDirectory(prefix='subyard-loss-preflight-', dir='/tmp') as temporary:
        root = Path(temporary)
        extract(bundle, root)
        acceptance = root/'.subyard-acceptance'
        metadata = json.loads((acceptance/'candidate.json').read_text())
        name = metadata['runtime_file']
        if '/' in name or name in ('', '.', '..'):
            raise ValueError('unsafe candidate archive name')
        archive = acceptance/'release'/name
        if sha(archive) != metadata['runtime_sha256']:
            raise ValueError('frozen candidate archive checksum mismatch')
        runtime = root/'checked-runtime'
        runtime.mkdir()
        extract(archive, runtime)
        manifest = sha(runtime/'runtime-files.sha256')
        runtime_identity(runtime, manifest)
        checked_version(runtime, metadata['version'])
        return successor_identity(acceptance/'fixture-b', metadata['version'], manifest)


def process_identity(pid):
    path = Path('/proc')/str(pid)
    fields = (path/'stat').read_text().rsplit(') ', 1)[1].split()
    return dict(pid=pid, parent=int(fields[1]), group=int(fields[2]), start=fields[19],
                state=fields[0], uid=path.stat().st_uid)


def same(record):
    try:
        current = process_identity(record['pid'])
        return all(current[key] == record[key] for key in ('pid', 'start', 'group', 'uid'))
    except (FileNotFoundError, ProcessLookupError):
        return False


def live(record):
    return same(record) and process_identity(record['pid'])['state'] != 'Z'


def tree(pid):
    result = []
    pending = [pid]
    while pending:
        child = pending.pop()
        try:
            record = process_identity(child)
            if record['uid'] != os.getuid():
                raise ValueError('fixture child changed account')
            result.append(record)
            pending.extend(map(int, (Path('/proc')/str(child)/'task'/str(child)/'children').read_text().split()))
        except (FileNotFoundError, ProcessLookupError):
            continue
    return result


def routed_processes(records, paused, inventory):
    ancestors = []
    current = paused
    by_pid = {record['pid']: record for record in records}
    while current in by_pid:
        record = by_pid[current]
        ancestors.append(record)
        current = record['parent']
    owner = helper = None
    for record in ancestors:
        path = Path('/proc')/str(record['pid'])
        arguments = (path/'cmdline').read_bytes().split(b'\0')
        if arguments[1:2] == [b'_release-owner-native-v1']:
            if helper is not None or sha(path/'exe') != inventory['bin/yard-engine']:
                raise ValueError('ambiguous or unverified native helper')
            helper = record
        if arguments[1:2] == [b'_release-owner-operation-v1']:
            if owner is not None or sha(path/'exe') != inventory['bin/subyard-release-owner']:
                raise ValueError('ambiguous or unverified Rust owner')
            owner = record
    if owner is None or helper is None or ancestors.index(helper) >= ancestors.index(owner):
        raise ValueError('paused owning mutation did not traverse the declared Rust/native chain')
    return owner, helper


def barrier_identity(home, paused):
    parent = Path('/tmp')/('subyard-owner-locks-'+str(os.getuid()))
    lock = parent/(hashlib.sha256(os.path.normpath(home).encode()).hexdigest()+'.lock')
    p, named = parent.lstat(), lock.lstat()
    inherited = (Path('/proc')/str(paused)/'fd/3').stat()
    fdinfo = (Path('/proc')/str(paused)/'fdinfo/3').read_text()
    if (not stat.S_ISDIR(p.st_mode) or p.st_uid != os.getuid() or stat.S_IMODE(p.st_mode) != 0o700
            or not stat.S_ISREG(named.st_mode) or named.st_uid != os.getuid()
            or stat.S_IMODE(named.st_mode) != 0o600 or named.st_nlink != 1
            or (named.st_dev, named.st_ino) != (inherited.st_dev, inherited.st_ino)
            or not any(line.startswith('lock:') and 'OFDLCK' in line and 'WRITE' in line for line in fdinfo.splitlines())):
        raise ValueError('paused child did not retain the original protected account descriptor')
    return dict(device=named.st_dev, inode=named.st_ino)


def barrier_held(identity):
    key = f'{os.major(identity["device"]):02x}:{os.minor(identity["device"]):02x}:{identity["inode"]}'
    return any('OFDLCK' in fields and 'WRITE' in fields and key in fields
               for fields in (line.split() for line in Path('/proc/locks').read_text().splitlines()))


def file_identity(path):
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1:
        raise ValueError('unsafe native proof file')
    value = dict(device=info.st_dev, inode=info.st_ino, mode=info.st_mode,
                 uid=info.st_uid, gid=info.st_gid, size=info.st_size,
                 mtime=info.st_mtime_ns, ctime=info.st_ctime_ns, sha256=sha(path))
    after = path.lstat()
    if any(getattr(info, key) != getattr(after, key) for key in
           ('st_dev', 'st_ino', 'st_mode', 'st_uid', 'st_gid', 'st_nlink', 'st_size', 'st_mtime_ns', 'st_ctime_ns')):
        raise ValueError('native proof changed while observing it')
    return value


def facts(home):
    root = Path(home)/'migrations/native-custody/v1'
    result = {}
    if root.exists():
        for path in sorted(root.rglob('*')):
            if path.is_symlink():
                raise ValueError('unsafe native fact')
            if path.is_dir():
                continue
            result[str(path.relative_to(root))] = file_identity(path)
    return result


def protected_state(case):
    journal = Path(os.environ['SUBYARD_CONFIG_HOME'])/'release-transition/v2/journal.json'
    if journal.is_symlink() or not journal.is_file():
        raise ValueError('the original durable journal is unavailable')
    return dict(journal=file_identity(journal), facts=facts(os.environ['SUBYARD_CONFIG_HOME']),
                calls=file_identity(case/'calls'))


def require_unchanged(case):
    if protected_state(case) != json.loads((case/'after-cut.json').read_text())['protected']:
        raise ValueError('fresh operation changed the pending journal, native proofs or owning dispatch count')


def save(root, name, value):
    path = root/name
    with path.open('x') as output:
        os.chmod(path, 0o600)
        json.dump(value, output, sort_keys=True, indent=2)
        output.write('\n')


def wait_for(condition, deadline, message):
    while not condition():
        if time.monotonic() >= deadline:
            raise TimeoutError(message)
        time.sleep(0.025)


def run_case(case, runtime, inventory, mode, activation_id, successor):
    """Only the first, owning-shell-authorized migration is started here."""
    home = os.environ['SUBYARD_HOME']
    config_home = os.environ['SUBYARD_CONFIG_HOME']
    pause, release = case/'paused', case/'release'
    recorded = []
    process = None
    initial = facts(config_home)
    with (case/'original.log').open('xb') as log:
        os.chmod(case/'original.log', 0o600)
        try:
            process = subprocess.Popen([str(runtime/'bin/yard'), 'migrate', '--yes'],
                                       stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
            deadline = time.monotonic()+180
            def paused():
                nonlocal recorded
                recorded = tree(process.pid)
                return pause.exists()
            wait_for(paused, deadline, 'owning mutating chain did not reach its fixed pause')
            pid = int(pause.read_text())
            recorded = tree(process.pid)
            owner, helper = routed_processes(recorded, pid, inventory)
            barrier = barrier_identity(home, pid)
            before = facts(config_home)
            pending = []
            for name in before.keys()-initial.keys():
                if name.endswith('.intent.json') and name.removesuffix('.intent.json')+'.terminal.json' not in before:
                    intent = json.loads((Path(config_home)/'migrations/native-custody/v1'/name).read_text())
                    if intent.get('owner') == activation_id:
                        pending.append(name)
            if len(pending) != 1 or not barrier_held(barrier):
                raise ValueError('paused work lacks durable native intent or original account custody')
            save(case, 'cut.json', dict(mode=mode, owner=owner, helper=helper, paused=pid,
                                       processes=recorded, barrier=barrier, facts=before, pending=pending,
                                       activation_id=activation_id, manifest_sha256=sha(runtime/'runtime-files.sha256'),
                                       successor=successor,
                                       images={name: inventory[name] for name in
                                               ('bin/yard', 'bin/yard-engine', 'bin/subyard-release-owner',
                                                'config/release-owner.json', 'config/release-owner-build.json')}))
            if mode == 'group':
                # Freeze the captured ancestry before taking the second child
                # snapshot; group membership is never inferred from an exited PID.
                for record in recorded:
                    if same(record):
                        os.kill(record['pid'], signal.SIGSTOP)
                recorded = tree(process.pid)
                for record in reversed(recorded):
                    if same(record):
                        os.kill(record['pid'], signal.SIGKILL)
            else:
                victim = owner if mode == 'owner' else helper
                if not same(victim):
                    raise ValueError('selected cut identity changed')
                os.kill(victim['pid'], signal.SIGKILL)
                if (not live(next(record for record in recorded if record['pid'] == pid))
                        or barrier_identity(home, pid) != barrier or not barrier_held(barrier)):
                    raise ValueError('surviving owning child lost original account custody')
                release.touch(mode=0o600)
            # Caller/native exit and kernel custody are separate observations.
            native_exit = process.wait(timeout=max(1, deadline-time.monotonic()))
            save(case, 'original-terminal.json', dict(exit_code=native_exit))
            if native_exit == 0:
                raise ValueError('the interrupted original migration reported success')
            wait_for(lambda: not barrier_held(barrier), deadline, 'original native account custody did not drain')
            save(case, 'after-cut.json', dict(protected=protected_state(case), barrier=barrier,
                                             processes=[record for record in recorded if live(record)]))
            print(json.dumps(dict(mode=mode, original_exit=native_exit,
                                  account_barrier_released=True, cut=json.loads((case/'cut.json').read_text()),
                                  after=json.loads((case/'after-cut.json').read_text())), sort_keys=True))
        finally:
            # Retire only identities captured while they belonged to our child.
            # This is fixture cleanup, never evidence of native effect closure.
            for record in reversed(recorded):
                if same(record) and live(record):
                    os.kill(record['pid'], signal.SIGKILL)
            if process is not None and process.poll() is None:
                process.kill()
                process.wait(timeout=5)
            if process is not None and not (case/'original-terminal.json').exists():
                save(case, 'original-terminal.json', dict(exit_code=process.wait(timeout=5),
                                                        fixture_cleanup_after_failure=True))
            if process is not None:
                log.flush()
                print(json.dumps(dict(original_terminal=json.loads((case/'original-terminal.json').read_text()),
                                      original_log=(case/'original.log').read_text(errors='replace')), sort_keys=True))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--runtime', type=Path)
    parser.add_argument('--manifest-sha256')
    parser.add_argument('--case-root', type=Path)
    parser.add_argument('--mode', choices=('owner', 'helper', 'group'))
    parser.add_argument('--verify-unchanged', action='store_true')
    parser.add_argument('--verify-unknown', action='store_true')
    parser.add_argument('--preflight', action='store_true')
    parser.add_argument('--successor', type=Path)
    parser.add_argument('--current-version')
    parser.add_argument('--bundle', type=Path)
    parser.add_argument('--bundle-sha256')
    parser.add_argument('--activation-id')
    args = parser.parse_args()
    if args.bundle is not None:
        if not args.preflight or not args.bundle_sha256:
            parser.error('bundle validation requires --preflight and --bundle-sha256')
        print(json.dumps(bundle_preflight(args.bundle, args.bundle_sha256), sort_keys=True))
        return
    if any(value is None for value in (args.runtime, args.manifest_sha256, args.case_root,
                                       args.mode, args.successor, args.current_version, args.activation_id)):
        parser.error('installed loss observation requires all bound runtime and case arguments')
    case = args.case_root.resolve(strict=True)
    info = case.lstat()
    if (not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o700
            or (case/'.marker').read_text() != 'subyard-profile-native-loss-v1\n'):
        raise ValueError('loss fixture requires its private marked case directory')
    runtime, inventory = runtime_identity(args.runtime, args.manifest_sha256)
    checked_version(runtime, args.current_version)
    successor = successor_identity(args.successor, args.current_version, args.manifest_sha256)
    if args.preflight:
        print(json.dumps(successor, sort_keys=True))
    elif args.verify_unknown:
        result = subprocess.run([str(runtime/'bin/yard'), 'migrate', '--check', '--json'],
                                capture_output=True, timeout=45)
        # Save the actual native result before parsing its projection.
        prefix = 'inspection-'+str(time.time_ns())
        save(case, prefix+'-terminal.json', dict(exit_code=result.returncode))
        (case/(prefix+'.stdout')).write_bytes(result.stdout)
        (case/(prefix+'.stderr')).write_bytes(result.stderr)
        (case/(prefix+'.stdout')).chmod(0o600)
        (case/(prefix+'.stderr')).chmod(0o600)
        print(json.dumps(dict(inspection_terminal=dict(exit_code=result.returncode),
                              stdout=result.stdout.decode(errors='replace'), stderr=result.stderr.decode(errors='replace')),
                         sort_keys=True))
        outcome = json.loads(result.stdout)['outcome']
        if (outcome.get('status') != 'operator-action-required' or outcome.get('code') != 'recovery-ambiguous'
                or outcome.get('reachedGoal') is not False):
            raise ValueError('opaque pending native work did not retain the public UNKNOWN refusal')
        require_unchanged(case)
    elif args.verify_unchanged:
        require_unchanged(case)
    else:
        run_case(case, runtime, inventory, args.mode, args.activation_id, successor)


if __name__ == '__main__':
    main()
