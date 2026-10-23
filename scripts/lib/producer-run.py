#!/usr/bin/env python3
"""Own the disk scratch and PID namespace of the two temporary producers."""
import ctypes
import fcntl
import json
import hashlib
import re
import tempfile
import os
from pathlib import Path
import signal
import stat
import subprocess
import sys
import tarfile
import threading

LIBC = ctypes.CDLL(None, use_errno=True)
LIBC.mount.argtypes = (ctypes.c_char_p, ctypes.c_char_p, ctypes.c_char_p, ctypes.c_ulong, ctypes.c_void_p)
VERSION = "subyard-producer-v3"


def pdeath(parent):
    if LIBC.prctl(1, signal.SIGKILL, 0, 0, 0) or os.getppid() != parent:
        os._exit(130)


def code(status):
    return status if status >= 0 else 128 - status


def directory(path, create=False):
    fd = os.open("/", os.O_RDONLY | os.O_DIRECTORY)
    try:
        for part in Path(path).parts[1:]:
            if create:
                try:
                    os.mkdir(part, 0o700, dir_fd=fd)
                except FileExistsError:
                    pass
            child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
            os.close(fd)
            fd = child
        return fd
    except BaseException:
        os.close(fd)
        raise


def read_state(fd):
    info = os.fstat(fd)
    if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_nlink != 1 or info.st_mode & 0o077:
        raise ValueError("unsafe operation receipt")
    os.lseek(fd, 0, os.SEEK_SET)
    state = json.loads(os.read(fd, 2048))
    if not isinstance(state, dict):
        raise ValueError("invalid operation receipt")
    return state


def write_state(fd, state):
    # Keep one fixed-size record: interruption cannot leave a shorter JSON value
    # followed by the previous record's suffix. The stable inode owns the flock.
    data = json.dumps(state).encode()
    if len(data) > 2048 or os.pwrite(fd, data.ljust(2048, b" "), 0) != 2048:
        raise ValueError("operation receipt write failed")
    os.fsync(fd)


def inode(fd):
    info = os.fstat(fd)
    return [info.st_dev, info.st_ino]


def clear(fd):
    """Never resolve an owned tree through a replaceable pathname."""
    for name in os.listdir(fd):
        info = os.stat(name, dir_fd=fd, follow_symlinks=False)
        if stat.S_ISDIR(info.st_mode):
            child = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
            try:
                if inode(child) != [info.st_dev, info.st_ino]:
                    raise ValueError("operation directory replaced")
                clear(child)
                current = os.stat(name, dir_fd=fd, follow_symlinks=False)
                if inode(child) != [current.st_dev, current.st_ino]:
                    raise ValueError("operation directory replaced")
                os.rmdir(name, dir_fd=fd)
            finally:
                os.close(child)
        else:
            os.unlink(name, dir_fd=fd)


def active(identity):
    if not identity or identity[2] != Path("/proc/sys/kernel/random/boot_id").read_text().strip():
        return False
    try:
        fields = Path(f"/proc/{identity[0]}/stat").read_text().rsplit(") ", 1)[1].split()
        return fields[19] == identity[1] and fields[0] not in ("Z", "X")
    except FileNotFoundError:
        return False


def inside():
    data = int(os.environ["SUBYARD_PRODUCER_DATA_FD"])
    original_ns = int(os.environ["SUBYARD_PRODUCER_NS_FD"])
    before = os.readlink(f"/proc/self/fd/{original_ns}")
    if not before.startswith("pid:[") or before == os.readlink("/proc/self/ns/pid"):
        raise ValueError("private PID namespace missing")
    expected = os.stat("tmp", dir_fd=data, follow_symlinks=False)
    actual = os.stat("/tmp")
    if [expected.st_dev, expected.st_ino] != [actual.st_dev, actual.st_ino]:
        raise ValueError("private disk temporary mount missing")


def bind(fd, target):
    before = os.open(".", os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fchdir(fd)
        if LIBC.mount(b".", os.fsencode(target), None, 4096, None):
            raise OSError(ctypes.get_errno(), "private temporary bind failed: " + target)
    finally:
        os.fchdir(before)
        os.close(before)


def extract(mode):
    target = Path("/tmp/profile")
    target.mkdir(mode=0o700)
    with tarfile.open(fileobj=sys.stdin.buffer, mode="r|") as archive:
        for member in archive:
            path = Path(member.name)
            if path.is_absolute() or ".." in path.parts or not (member.isfile() or member.isdir()):
                raise ValueError("unsafe profile archive")
            destination = target / path
            if member.isdir():
                destination.mkdir(parents=True, exist_ok=True)
            else:
                destination.parent.mkdir(parents=True, exist_ok=True)
                with destination.open("xb") as out, archive.extractfile(member) as source:
                    while block := source.read(65536):
                        out.write(block)
                destination.chmod(member.mode & 0o777)
    return ["/bin/bash", str(target / "provision.sh"), *(["--check"] if mode == "check" else [])]


def worker(kind, lock, data, original_ns, bindings, command):
    if os.getpid() != 1:
        raise ValueError("private PID namespace missing")
    # Inherited directory FDs pin the parent's mount namespace. Reopen and check
    # their identities in this mount namespace before using them as bind sources.
    def local(fd):
        current = directory(os.readlink(f"/proc/self/fd/{fd}"))
        if inode(current) != inode(fd):
            raise ValueError("operation bind source replaced")
        return current
    data = local(data)
    bindings = [(path, local(fd)) for path, fd in bindings]
    # /proc stays visible: hooks must still detect pre-existing guest workloads.
    fields = Path("/proc/self/stat").read_text().rsplit(") ", 1)[1].split()
    outer_pid = int(Path("/proc/self/stat").read_text().split(" ", 1)[0])
    state = read_state(lock)
    state["active"] = [outer_pid, fields[19], Path("/proc/sys/kernel/random/boot_id").read_text().strip()]
    write_state(lock, state)
    for name, target in (("tmp", "/tmp"), ("var-tmp", "/var/tmp"), ("shm", "/dev/shm")):
        os.mkdir(name, 0o777, dir_fd=data)
        os.chmod(name, 0o1777, dir_fd=data)
        child = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=data)
        try:
            bind(child, target)
        finally:
            os.close(child)
    for target, fd in sorted(bindings, key=lambda item: len(item[0])):
        Path(target).mkdir(parents=True, exist_ok=True)
        bind(fd, target)
    os.environ.update(TMPDIR="/tmp", SUBYARD_PRODUCER_DATA_FD=str(data), SUBYARD_PRODUCER_NS_FD=str(original_ns))
    if kind == "profile":
        # Root's direct manager socket rejects an ancestor's PID-zero peercred.
        # Use systemctl's documented route through the guest's existing bus.
        os.environ["SYSTEMCTL_FORCE_BUS"] = "1"
        command = extract(command[0])
    child = subprocess.Popen(command, stdin=subprocess.DEVNULL, pass_fds=(data, original_ns),
                             stdout=subprocess.DEVNULL if kind == "profile" and command[-1] == "--check" else None)
    if kind == "profile":
        def disconnected():
            # Controller retains stdin after tar EOF; only loss of its lifetime
            # pipe reaches this read. PID1 death kills even detached children.
            while os.read(0, 65536):
                pass
            os._exit(130)
        threading.Thread(target=disconnected, daemon=True).start()
    status = code(child.wait())
    try:
        os.kill(-1, signal.SIGKILL)
    except ProcessLookupError:
        pass
    while True:
        try:
            os.waitpid(-1, 0)
        except ChildProcessError:
            break
    return status


def launch(source, kind, root, command):
    root = str(Path(root).resolve()) if root else ""
    storage = Path(root) / f".build/paseo-operation-{os.getuid()}" if kind == "paseo" else Path("/var/lib/subyard/profile-operation")
    parent = os.getppid()
    pdeath(parent)
    parent_fd = directory(str(storage.parent), create=True)
    try:
        os.mkdir(storage.name, 0o700, dir_fd=parent_fd)
        os.chmod(storage.name, 0o700, dir_fd=parent_fd, follow_symlinks=False)
        created = True
    except FileExistsError:
        created = False
    try:
        control = os.open(storage.name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=parent_fd)
        info = os.fstat(control)
        if info.st_uid != os.getuid() or info.st_mode & 0o077:
            raise ValueError("unsafe operation directory")
        fs = subprocess.check_output(["findmnt", "-n", "-o", "FSTYPE", "-T", str(storage)], text=True).strip()
        if fs not in ("ext2", "ext3", "ext4", "xfs", "btrfs", "zfs", "f2fs", "bcachefs"):
            raise ValueError("operation scratch requires verified disk storage")
        if created:
            lock = os.open(".owner", os.O_RDWR | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600, dir_fd=control)
            os.fchmod(lock, 0o600)
            os.mkdir("data", 0o700, dir_fd=control)
            os.chmod("data", 0o700, dir_fd=control, follow_symlinks=False)
            data = os.open("data", os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=control)
            write_state(lock, dict(version=VERSION, kind=kind, root=inode(control), owner=inode(lock), data=inode(data), active=None))
        else:
            lock = os.open(".owner", os.O_RDWR | os.O_NONBLOCK | os.O_NOFOLLOW, dir_fd=control)
            data = os.open("data", os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=control)
    except (OSError, ValueError) as error:
        raise ValueError(f"preserved operation storage {storage}: {error}; inspect the state before retrying") from error
    try:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        raise ValueError("producer busy")
    try:
        state = read_state(lock)
        if state.get("version") != VERSION or state.get("kind") != kind or state.get("root") != inode(control) or state.get("owner") != inode(lock) or state.get("data") != inode(data):
            raise ValueError("foreign operation storage")
    except (OSError, ValueError) as error:
        raise ValueError(f"preserved operation storage {storage}: {error}; inspect the state before retrying") from error
    def verify_paths():
        for parent, name, fd in ((parent_fd, storage.name, control), (control, ".owner", lock), (control, "data", data)):
            current = os.stat(name, dir_fd=parent, follow_symlinks=False)
            if inode(fd) != [current.st_dev, current.st_ino]:
                raise ValueError("operation path replaced; preserved")
    # The receipt also fences the brief exit_files -> PID namespace teardown
    # interval after SIGKILL, when the flock can already have been released.
    if active(state.get("active")):
        raise ValueError("producer busy while namespace exits")
    verify_paths()
    clear(data)
    original_ns = os.open("/proc/self/ns/pid", os.O_RDONLY)
    bindings = []
    if root:
        output = Path(command[command.index("--output-dir") + 1]).resolve()
        output.mkdir(parents=True, exist_ok=True)
        for path in sorted({root, str(output)}):
            if path in ("/", "/tmp", "/var/tmp", "/dev/shm"):
                raise ValueError("source/output would reopen a temporary root")
            if any(path.startswith(prefix + "/") for prefix in ("/tmp", "/var/tmp", "/dev/shm")):
                bindings.append((path, os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)))
    else:
        # Explicit migration directories stay visible; all other scratch is private.
        for path in json.loads(command.pop(0)):
            if str(Path(path)) != path or not any(path.startswith(prefix + "/") for prefix in ("/tmp", "/var/tmp", "/dev/shm")) or ".." in Path(path).parts:
                raise ValueError("invalid shared temporary directory")
            try:
                fd = directory(path)
            except FileNotFoundError:
                continue
            bindings.append((path, fd))
    inherited = (lock, data, original_ns, *(fd for _, fd in bindings))
    args = ["unshare", *(["--user", "--map-root-user"] if os.getuid() else []),
            "--mount", "--pid", "--fork", "--kill-child=SIGKILL",
            "/usr/bin/python3", "-B", "-c", source, "worker", kind, str(lock), str(data), str(original_ns),
            json.dumps(bindings), *command]
    owner = os.getpid()
    process = subprocess.Popen(args, pass_fds=inherited, preexec_fn=lambda: pdeath(owner))
    cancelled = 0
    def interrupted(signum, _frame):
        nonlocal cancelled
        cancelled = signum
        if process.poll() is None:
            process.kill()
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    signal.signal(signal.SIGHUP, interrupted)
    status = code(process.wait())
    if cancelled:
        status = 128 + cancelled
    try:
        verify_paths()
        clear(data)
        state["active"] = None
        write_state(lock, state)
    except (OSError, ValueError) as error:
        print("producer: cleanup failed: " + str(error), file=sys.stderr)
        return status or 125
    return status


def source_identity(info, digest=None):
    result = dict(mode=info.st_mode, device=info.st_dev, inode=info.st_ino,
                  size=info.st_size, modified=info.st_mtime_ns, changed=info.st_ctime_ns,
                  links=info.st_nlink, uid=info.st_uid, gid=info.st_gid)
    if digest is not None:
        result["digest"] = digest
    return result


def capture_additional_source(target, name, source):
    """Use the resolved source's native authority, never an environment path."""
    if not re.fullmatch(r"[a-zA-Z0-9_.-]+", name) or name in (".", ".."):
        raise ValueError("invalid additional provision source")
    path = Path(source["path"])
    root = str(path.parent)
    anchor = re.match(r"^/proc/[1-9][0-9]*/fd/(?:0|[1-9][0-9]*)(?=/|$)", root)
    fd = os.open(anchor[0] if anchor else "/", os.O_RDONLY | os.O_DIRECTORY)
    parts = Path(root[len(anchor[0]):]).parts if anchor else path.parent.parts[1:]
    try:
        for part in parts:
            if part in ("/", ""):
                continue
            child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
            os.close(fd)
            fd = child
        def check_parents():
            for parent, expected in source["parents"].items():
                info = os.stat(parent) if re.fullmatch(r"/proc/[1-9][0-9]*/fd/(?:0|[1-9][0-9]*)", parent) else os.lstat(parent)
                actual = dict(mode=info.st_mode, device=info.st_dev, inode=info.st_ino,
                              size=0, modified=0, changed=0, links=0, uid=info.st_uid, gid=info.st_gid)
                if actual != expected:
                    raise ValueError("additional provision source parent changed")
        check_parents()
        child = os.open(path.name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=fd)
        with os.fdopen(child, "rb") as payload:
            info = os.fstat(payload.fileno())
            if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1:
                raise ValueError("unsafe additional provision source")
            digest = hashlib.file_digest(payload, "sha256").hexdigest()
            if source_identity(info, digest) != source["identity"]:
                raise ValueError("additional provision source changed")
            payload.seek(0)
            member = tarfile.TarInfo(name)
            member.size, member.mode = info.st_size, stat.S_IMODE(info.st_mode)
            target.addfile(member, payload)
            if source_identity(os.fstat(payload.fileno()), digest) != source["identity"]:
                raise ValueError("additional provision source changed during capture")
        check_parents()
    finally:
        os.close(fd)


def capture_bundle(binding):
    """Capture the approved regular-file tree before starting the guest command."""
    root = binding["root"]
    anchor = re.match(r"^/proc/[1-9][0-9]*/fd/(?:0|[1-9][0-9]*)(?=/|$)", root)
    if anchor:
        fd = os.open(anchor[0], os.O_RDONLY | os.O_DIRECTORY)
        parts = Path(root[len(anchor[0]):]).parts
    else:
        fd = os.open("/", os.O_RDONLY | os.O_DIRECTORY)
        parts = Path(root).parts[1:]
    try:
        for part in parts:
            if part in ("/", ""):
                continue
            child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
            os.close(fd)
            fd = child
        def check_parents():
            for parent, expected in binding["parents"].items():
                info = os.stat(parent) if re.fullmatch(r"/proc/[1-9][0-9]*/fd/(?:0|[1-9][0-9]*)", parent) else os.lstat(parent)
                actual = dict(mode=info.st_mode, device=info.st_dev, inode=info.st_ino,
                              size=0, modified=0, changed=0, links=0, uid=info.st_uid, gid=info.st_gid)
                if actual != expected:
                    raise ValueError("profile source parent changed")
        check_parents()
        entries = binding["entries"]
        archive = tempfile.TemporaryFile(dir="/var/tmp")
        shared_tmp = []
        try:
            with tarfile.open(fileobj=archive, mode="w") as target:
                def walk(parent, relative):
                    expected = entries[relative]
                    if source_identity(os.fstat(parent)) != expected:
                        raise ValueError("profile directory changed")
                    names = sorted(os.listdir(parent))
                    children = sorted(name for name in entries if name != "." and str(Path(name).parent) == relative)
                    actual = [str(Path(relative) / name) for name in names]
                    if actual != children:
                        raise ValueError("profile bundle inventory changed")
                    for name, path in zip(names, actual):
                        approved = entries[path]
                        child = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=parent)
                        try:
                            info = os.fstat(child)
                            if stat.S_ISDIR(info.st_mode):
                                member = tarfile.TarInfo(path)
                                member.type, member.mode = tarfile.DIRTYPE, stat.S_IMODE(info.st_mode)
                                target.addfile(member)
                                walk(child, path)
                            elif stat.S_ISREG(info.st_mode) and info.st_nlink == 1:
                                with os.fdopen(os.dup(child), "rb") as source:
                                    digest = hashlib.file_digest(source, "sha256").hexdigest()
                                    if source_identity(info, digest) != approved:
                                        raise ValueError("profile source changed")
                                    source.seek(0)
                                    member = tarfile.TarInfo(path)
                                    member.size, member.mode = info.st_size, stat.S_IMODE(info.st_mode)
                                    target.addfile(member, source)
                                    if path == "provision-tmp-paths":
                                        source.seek(0)
                                        shared_tmp.extend(source.read().decode().splitlines())
                                if source_identity(os.fstat(child), digest) != approved:
                                    raise ValueError("profile source changed during capture")
                            else:
                                raise ValueError("unsafe profile bundle source")
                        finally:
                            os.close(child)
                    if source_identity(os.fstat(parent)) != expected or sorted(os.listdir(parent)) != names:
                        raise ValueError("profile directory changed during capture")
                walk(fd, ".")
                for name, source in sorted(binding.get("sources", {}).items()):
                    if name in entries:
                        raise ValueError("additional provision source collides with package")
                    capture_additional_source(target, name, source)
            check_parents()
            archive.seek(0)
            return archive, shared_tmp
        except BaseException:
            archive.close()
            raise
    finally:
        os.close(fd)


def capture(mode, profile, command):
    pdeath(os.getppid())
    with os.fdopen(int(os.environ["SUBYARD_METADATA_FD"])) as metadata:
        request = json.load(metadata)
    binding = request["input"]["profileBinding"]
    if Path(binding["root"]).name != profile or not re.fullmatch(r"[a-zA-Z0-9_-]+", profile):
        raise ValueError("invalid bound profile")
    archive, shared_tmp = capture_bundle(binding)
    environment = binding["environment"]
    env_args = []
    for key, value in sorted(environment.items()):
        if not re.fullmatch(r"[a-zA-Z_][a-zA-Z0-9_]*", key) or "\0" in value:
            raise ValueError("invalid profile environment")
        env_args.extend(["--env", key + "=" + value])
    separator = command.index("--")
    command[separator:separator] = env_args
    command[-2] = json.dumps(shared_tmp)
    owner = os.getpid()
    process = subprocess.Popen(command, stdin=subprocess.PIPE, stderr=subprocess.PIPE,
                               preexec_fn=lambda: pdeath(owner))
    cancelled = 0
    def interrupted(signum, _frame):
        nonlocal cancelled
        cancelled = signum
        for child in (process,):
            if child.poll() is None:
                child.kill()
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    signal.signal(signal.SIGHUP, interrupted)
    def send():
        try:
            while block := archive.read(65536):
                process.stdin.write(block)
            process.stdin.flush()
            # Keep stdin open until the guest operation completes.
        except (BrokenPipeError, ValueError):
            pass
        finally:
            archive.close()
    sender = threading.Thread(target=send, daemon=True)
    sender.start()
    retained = bytearray()
    truncated = False
    while block := process.stderr.read(65536):
        space = 65536 - len(retained)
        retained.extend(block[:space])
        truncated |= len(block) > space
    status = code(process.wait())
    if cancelled:
        status = 128 + cancelled
    try:
        process.stdin.close()
    except BrokenPipeError:
        pass
    sender.join()
    if mode != "check" or status not in (0, 10):
        sys.stderr.buffer.write(retained)
    if truncated and (mode != "check" or status not in (0, 10)):
        print("provision: stderr truncated at 64 KiB", file=sys.stderr)
    if mode == "check" and status in (0, 10):
        print("converged" if status == 0 else "changed")
        status = 0
    return status


def main():
    action, *args = sys.argv[1:]
    if action == "inside":
        inside()
        return 0
    if action == "worker":
        return worker(args[0], int(args[1]), int(args[2]), int(args[3]), json.loads(args[4]), args[5:])
    if action == "capture":
        return capture(args[0], args[1], args[2:])
    if action == "launch":
        return launch(globals().get("_SOURCE") or Path(__file__).read_text(), args[0], args[1], args[2:])
    raise ValueError("unknown producer operation")


if __name__ == "__main__":
    try:
        # Captured before fork: the child checks the actual launcher relationship.
        launcher = os.getpid()
        sys.exit(main())
    except (OSError, ValueError, KeyError, tarfile.TarError) as error:
        print("producer: " + str(error), file=sys.stderr)
        sys.exit(125)
