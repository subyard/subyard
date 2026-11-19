# Guest compare-and-write of one configuration file, prepended to its writers.
# A consent token is "absent" or "sha256:<hex>" of the destination's exact bytes;
# an unreadable destination has no token. One exec compares the destination with
# the token and replaces it. Concurrent guest changes are detected, never lost:
# an inode is unlinked only when it is this writer's own payload, or the hashed
# destination still matching the token. Any other displaced file stays in place
# under its temporary name and is reported.
import ctypes as _cas_ctypes
import errno as _cas_errno
import hashlib as _cas_hashlib
import json as _cas_json
import os as _cas_os
import secrets as _cas_secrets
import stat as _cas_stat
import sys as _cas_sys

CAS_NOREPLACE, CAS_EXCHANGE = 1, 2
CAS_MAX_BYTES = 32 << 20
CAS_EXCHANGE_BOUND = 8
CAS_EXIT = {"unreadable": 3, "changed": 4, "leftover": 5}
CAS_UNSUPPORTED = (_cas_errno.ENOSYS, _cas_errno.EINVAL, _cas_errno.ENOTSUP)
_CAS_DIRECTORY = _cas_os.O_RDONLY | _cas_os.O_DIRECTORY | _cas_os.O_NOFOLLOW | _cas_os.O_CLOEXEC


class CasRefusal(Exception):
    def __init__(self, outcome, leftovers=()):
        Exception.__init__(self, outcome)
        self.outcome, self.leftovers = outcome, list(leftovers)


def _cas_hook(point):
    # Tests define CAS_HOOK to interleave guest edits, crashes or an
    # unsupported renameat2; production programs never define it.
    hook = globals().get("CAS_HOOK")
    if hook is not None:
        hook(point)


def _cas_renameat2(directory, old, new, flags):
    _cas_hook("renameat2")
    try:
        function = _cas_ctypes.CDLL(None, use_errno=True).renameat2
    except AttributeError:
        raise OSError(_cas_errno.ENOSYS, "renameat2 is unavailable")
    function.argtypes = [_cas_ctypes.c_int, _cas_ctypes.c_char_p, _cas_ctypes.c_int, _cas_ctypes.c_char_p, _cas_ctypes.c_uint]
    function.restype = _cas_ctypes.c_int
    if function(directory, _cas_os.fsencode(old), directory, _cas_os.fsencode(new), flags) != 0:
        code = _cas_ctypes.get_errno()
        raise OSError(code, _cas_os.strerror(code))


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
        info = _cas_os.fstat(descriptor)
    except BaseException:
        _cas_os.unlink(temporary, dir_fd=directory)  # created above with O_EXCL: ours
        raise
    finally:
        _cas_os.close(descriptor)
    _cas_hook("staged")
    return temporary, (info.st_dev, info.st_ino)


def _cas_fallback(directory, temporary, name, expect, descriptor, hashed):
    # Without renameat2 (for example ZFS before 2.2) the compare is repeated
    # immediately before a plain rename. A guest change between that check and
    # the rename is lost; this window is accepted. An absent token keeps its
    # no-overwrite semantics through link.
    _cas_hook("fallback")
    if expect == "absent":
        try:
            _cas_os.link(temporary, name, src_dir_fd=directory, dst_dir_fd=directory, follow_symlinks=False)
        except FileExistsError:
            raise CasRefusal("changed")
        finally:
            _cas_os.unlink(temporary, dir_fd=directory)
        return
    if _cas_identity(directory, name) != hashed or _cas_hash(descriptor) != expect:
        _cas_os.unlink(temporary, dir_fd=directory)
        raise CasRefusal("changed")
    _cas_os.rename(temporary, name, src_dir_fd=directory, dst_dir_fd=directory)


def _cas_verified(directory, temporary, expect, descriptor, hashed):
    # Identity first: only the hashed inode, still matching the token, may go.
    # A file that can no longer be read is not verified either.
    if _cas_identity(directory, temporary) != hashed:
        return False
    try:
        return _cas_hash(descriptor) == expect
    except CasRefusal:
        return False


def _cas_exchange(directory, temporary, name, ours, expect, descriptor, hashed):
    _cas_renameat2(directory, temporary, name, CAS_EXCHANGE)
    _cas_hook("exchanged")
    if _cas_verified(directory, temporary, expect, descriptor, hashed):
        _cas_hook("verified")
        _cas_os.unlink(temporary, dir_fd=directory)
        return
    # The guest changed the destination: give it back. A guest save between
    # two exchanges lands under the temporary name; exchange again, within a
    # bound, so the newest guest file ends at the destination.
    expected, moved = ours, _cas_identity(directory, temporary)
    for _ in range(CAS_EXCHANGE_BOUND):
        _cas_renameat2(directory, temporary, name, CAS_EXCHANGE)
        _cas_hook("exchanged-back")
        displaced = _cas_identity(directory, temporary)
        if displaced == expected:
            break
        expected, moved = moved, displaced
    else:
        raise CasRefusal("changed", [temporary])
    if expected == ours:
        _cas_os.unlink(temporary, dir_fd=directory)
        raise CasRefusal("changed")
    raise CasRefusal("changed", [temporary])


def _cas_leftovers(directory, destination):
    prefix = "." + _cas_os.path.basename(destination) + ".subyard-cas."
    return sorted(_cas_os.path.join(_cas_os.path.dirname(destination), entry)
                  for entry in _cas_os.listdir(directory) if entry.startswith(prefix))


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
        # Guest bytes that a refused write preserved stay the operator's to
        # resolve: no write, and no convergence, until they are gone.
        leftovers = _cas_leftovers(directory, destination)
        if leftovers:
            raise CasRefusal("leftover", leftovers)
        current, descriptor, hashed = _cas_open(directory, name)
        if current == cas_token(payload):
            # A converged file still gets the owner and mode a write gives it.
            info = _cas_os.fstat(descriptor)
            if (info.st_uid, info.st_gid) != (uid, uid):
                _cas_os.fchown(descriptor, uid, uid)
            if _cas_stat.S_IMODE(info.st_mode) != mode:
                _cas_os.fchmod(descriptor, mode)
            return "converged"
        if current != expect:
            raise CasRefusal("changed")
        temporary, ours = _cas_stage(directory, name, payload, uid, mode)
        try:
            if expect == "absent":
                _cas_renameat2(directory, temporary, name, CAS_NOREPLACE)
            else:
                _cas_exchange(directory, temporary, name, ours, expect, descriptor, hashed)
        except CasRefusal as refusal:
            refusal.leftovers = [_cas_os.path.join(_cas_os.path.dirname(destination), item) for item in refusal.leftovers]
            raise
        except OSError as error:
            if _cas_identity(directory, temporary) != ours:
                # A failed exchange-back: the guest's file stays where it is.
                raise CasRefusal("changed", [_cas_os.path.join(_cas_os.path.dirname(destination), temporary)])
            if error.errno in CAS_UNSUPPORTED:
                _cas_fallback(directory, temporary, name, expect, descriptor, hashed)
            else:
                _cas_os.unlink(temporary, dir_fd=directory)
                if error.errno in (_cas_errno.EEXIST, _cas_errno.ENOENT):
                    raise CasRefusal("changed")
                raise
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
    """Token of one destination and the preserved files beside it."""
    try:
        directory = _cas_parent(home, destination, 0, False)
    except CasRefusal:
        return {"destination": destination, "token": "unreadable", "leftovers": []}
    if directory is None:
        return {"destination": destination, "token": "absent", "leftovers": []}
    name = _cas_os.path.basename(destination)
    try:
        try:
            token, descriptor, _ = _cas_open(directory, name)
            if descriptor is not None:
                _cas_os.close(descriptor)
        except CasRefusal:
            token = "unreadable"
        return {"destination": destination, "token": token, "leftovers": _cas_leftovers(directory, destination)}
    finally:
        _cas_os.close(directory)


def cas_refuse(refusal):
    print(_cas_json.dumps({"outcome": refusal.outcome, "leftovers": refusal.leftovers}, separators=(",", ":"), sort_keys=True))
    _cas_sys.stdout.flush()
    raise SystemExit(CAS_EXIT[refusal.outcome])


def cas_main():
    # observe HOME DESTINATION... | write HOME DESTINATION UID EXPECT PAYLOAD_DIGEST
    arguments = _cas_sys.argv[1:]
    if len(arguments) >= 2 and arguments[0] == "observe":
        files = [cas_observe(arguments[1], destination) for destination in arguments[2:]]
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
