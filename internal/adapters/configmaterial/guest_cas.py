# Guest compare-and-write of one configuration file, prepended to its writers.
# A consent token is "absent" or "sha256:<hex>" of the destination's exact bytes;
# an unreadable destination has no token. One exec compares the destination with
# the token and publishes the staged payload: link() for an absent destination,
# otherwise a repeated identity and hash check right before a plain rename. A
# guest change between that check and the rename is lost; this window is
# accepted. Only this writer's payload is ever under its temporary name.
import errno as _cas_errno
import hashlib as _cas_hashlib
import json as _cas_json
import os as _cas_os
import secrets as _cas_secrets
import stat as _cas_stat
import sys as _cas_sys

CAS_MAX_BYTES = 32 << 20
CAS_EXIT = {"unreadable": 3, "changed": 4}
_CAS_DIRECTORY = _cas_os.O_RDONLY | _cas_os.O_DIRECTORY | _cas_os.O_NOFOLLOW | _cas_os.O_CLOEXEC


class CasRefusal(Exception):
    def __init__(self, outcome):
        Exception.__init__(self, outcome)
        self.outcome = outcome


def _cas_hook(point):
    # Tests define CAS_HOOK to interleave guest edits or crashes; production
    # programs never define it.
    hook = globals().get("CAS_HOOK")
    if hook is not None:
        hook(point)


def cas_token(payload):
    return "absent" if payload is None else "sha256:" + _cas_hashlib.sha256(payload).hexdigest()


def _cas_hash(descriptor):
    digest, offset = _cas_hashlib.sha256(), 0
    try:
        while True:
            chunk = _cas_os.pread(descriptor, 1 << 20, offset)
            if not chunk:
                return "sha256:" + digest.hexdigest()
            offset += len(chunk)
            if offset > CAS_MAX_BYTES:
                raise CasRefusal("unreadable")
            digest.update(chunk)
    except OSError:
        raise CasRefusal("unreadable")


def _cas_identity(directory, name):
    try:
        info = _cas_os.stat(name, dir_fd=directory, follow_symlinks=False)
    except FileNotFoundError:
        return None
    return info.st_dev, info.st_ino


def _cas_parent(home, destination, uid, create):
    """The destination's directory, walked from home without following links,
    or None when it is missing and may not be created."""
    if destination == home or not destination.startswith(home + "/") or destination.endswith("/"):
        raise CasRefusal("unreadable")
    relative = _cas_os.path.dirname(destination)[len(home) + 1:]
    try:
        descriptor = _cas_os.open(home, _CAS_DIRECTORY)
    except OSError:
        raise CasRefusal("unreadable")
    try:
        for part in [part for part in relative.split("/") if part]:
            try:
                child = _cas_os.open(part, _CAS_DIRECTORY, dir_fd=descriptor)
            except FileNotFoundError:
                if not create:
                    _cas_os.close(descriptor)
                    return None
                _cas_os.mkdir(part, 0o755, dir_fd=descriptor)
                child = _cas_os.open(part, _CAS_DIRECTORY, dir_fd=descriptor)
                _cas_os.fchown(child, uid, uid)
                _cas_os.fchmod(child, 0o755)
            _cas_os.close(descriptor)
            descriptor = child
        return descriptor
    except OSError:
        _cas_os.close(descriptor)
        raise CasRefusal("unreadable")


def _cas_open(directory, name):
    """(token, descriptor, identity) of the destination entry."""
    try:
        descriptor = _cas_os.open(name, _cas_os.O_RDONLY | _cas_os.O_NOFOLLOW | _cas_os.O_NONBLOCK | _cas_os.O_CLOEXEC, dir_fd=directory)
    except FileNotFoundError:
        return "absent", None, None
    except OSError:
        raise CasRefusal("unreadable")
    try:
        info = _cas_os.fstat(descriptor)
        if not _cas_stat.S_ISREG(info.st_mode) or info.st_size > CAS_MAX_BYTES:
            raise CasRefusal("unreadable")
        return _cas_hash(descriptor), descriptor, (info.st_dev, info.st_ino)
    except BaseException:
        _cas_os.close(descriptor)
        raise


def _cas_stage(directory, name, payload, uid, mode):
    temporary = "." + name + ".subyard-cas." + _cas_secrets.token_hex(8)
    descriptor = _cas_os.open(temporary, _cas_os.O_RDWR | _cas_os.O_CREAT | _cas_os.O_EXCL | _cas_os.O_NOFOLLOW | _cas_os.O_CLOEXEC, 0o600, dir_fd=directory)
    try:
        view = memoryview(payload)
        while view:
            view = view[_cas_os.write(descriptor, view):]
        _cas_os.fchown(descriptor, uid, uid)
        _cas_os.fchmod(descriptor, mode)
        _cas_os.fsync(descriptor)
        if _cas_hash(descriptor) != cas_token(payload):
            raise OSError(_cas_errno.EIO, "temporary payload verification failed")
    except BaseException:
        _cas_os.unlink(temporary, dir_fd=directory)  # created above with O_EXCL: ours
        raise
    finally:
        _cas_os.close(descriptor)
    _cas_hook("staged")
    return temporary


def _cas_unchanged(directory, name, expect, descriptor, hashed):
    if _cas_identity(directory, name) != hashed:
        return False
    try:
        return _cas_hash(descriptor) == expect
    except CasRefusal:
        return False


def _cas_publish(directory, temporary, name, expect, descriptor, hashed):
    try:
        if expect == "absent":
            try:
                _cas_os.link(temporary, name, src_dir_fd=directory, dst_dir_fd=directory, follow_symlinks=False)
            except FileExistsError:
                raise CasRefusal("changed")
            return
        if not _cas_unchanged(directory, name, expect, descriptor, hashed):
            raise CasRefusal("changed")
        _cas_hook("rename")
        _cas_os.rename(temporary, name, src_dir_fd=directory, dst_dir_fd=directory)
        temporary = None
    finally:
        if temporary is not None:
            _cas_os.unlink(temporary, dir_fd=directory)


def cas_replace(home, destination, expect, payload, uid, mode):
    """Replace destination with payload only if it still matches expect.
    Returns "written" or "converged"; raises CasRefusal otherwise."""
    if expect != "absent" and not (expect.startswith("sha256:") and len(expect) == 71):
        raise CasRefusal("changed")
    # Missing parents are created only once the write is decided, so no
    # refusal leaves new directories behind.
    directory = _cas_parent(home, destination, uid, False)
    if directory is None:
        if expect != "absent":
            raise CasRefusal("changed")
        directory = _cas_parent(home, destination, uid, True)
    name, descriptor = _cas_os.path.basename(destination), None
    try:
        current, descriptor, hashed = _cas_open(directory, name)
        if current == cas_token(payload):
            # A converged file still gets the owner and mode a write gives it.
            info = _cas_os.fstat(descriptor)
            if info.st_nlink != 1:
                # Another name shares this inode; its owner and mode stay.
                return "converged"
            if (info.st_uid, info.st_gid) != (uid, uid):
                _cas_os.fchown(descriptor, uid, uid)
            if _cas_stat.S_IMODE(info.st_mode) != mode:
                _cas_os.fchmod(descriptor, mode)
            return "converged"
        if current != expect:
            raise CasRefusal("changed")
        _cas_publish(directory, _cas_stage(directory, name, payload, uid, mode), name, expect, descriptor, hashed)
        _cas_os.fsync(directory)
        written, check, _ = _cas_open(directory, name)
        if check is not None:
            _cas_os.close(check)
        if written != cas_token(payload):
            raise CasRefusal("changed")  # a guest save after the replacement wins
        return "written"
    finally:
        if descriptor is not None:
            _cas_os.close(descriptor)
        _cas_os.close(directory)


def cas_observe(home, destination):
    """Token of one destination, or "unreadable"."""
    try:
        directory = _cas_parent(home, destination, 0, False)
    except CasRefusal:
        return "unreadable"
    if directory is None:
        return "absent"
    try:
        token, descriptor, _ = _cas_open(directory, _cas_os.path.basename(destination))
        if descriptor is not None:
            _cas_os.close(descriptor)
        return token
    except CasRefusal:
        return "unreadable"
    finally:
        _cas_os.close(directory)


def cas_refuse(refusal):
    print("config write: " + refusal.outcome, file=_cas_sys.stderr)
    raise SystemExit(CAS_EXIT[refusal.outcome])


def cas_main():
    # observe HOME DESTINATION... | write HOME DESTINATION UID EXPECT PAYLOAD_DIGEST
    arguments = _cas_sys.argv[1:]
    if len(arguments) >= 2 and arguments[0] == "observe":
        files = [{"destination": destination, "token": cas_observe(arguments[1], destination)} for destination in arguments[2:]]
        print(_cas_json.dumps({"files": files}, separators=(",", ":"), sort_keys=True))
        return
    if len(arguments) != 6 or arguments[0] != "write":
        print("config write: invalid request", file=_cas_sys.stderr)
        raise SystemExit(2)
    _, home, destination, uid, expect, digest = arguments
    payload = _cas_sys.stdin.buffer.read()
    if _cas_hashlib.sha256(payload).hexdigest() != digest:
        print("config write: desired payload verification failed", file=_cas_sys.stderr)
        raise SystemExit(2)
    try:
        cas_replace(home, destination, expect, payload, int(uid), 0o644)
    except CasRefusal as refusal:
        cas_refuse(refusal)
