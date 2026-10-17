"""Lock pip entrypoints in the existing venv without replacing its interpreter."""
import fcntl
import math
import os
import stat
import sys
import time


def _pip_cache_lock():
    argv = sys.orig_argv[1:]
    is_pip = os.path.basename(sys.argv[0]).split(".")[0] in ("pip", "pip3")
    is_pip = is_pip or any(argv[i:i + 2] == ["-m", "pip"] for i in range(len(argv) - 1))
    if not is_pip or "--no-cache-dir" in argv:
        return
    if os.environ.get("PIP_NO_CACHE_DIR", "").lower() in ("1", "true", "yes"):
        return
    if "--isolated" in argv:
        raise RuntimeError("pip --isolated ignores the keyed cache environment; use --no-cache-dir")
    lock = os.environ.get("SY_CACHE_LOCK", "/srv/cache/.sy-cache.lock")
    info = os.lstat(lock)
    if not stat.S_ISREG(info.st_mode) or (info.st_uid, info.st_gid) != (os.geteuid(), os.getegid()):
        raise RuntimeError("unsafe cache lock")
    mode = "exclusive" if "cache" in argv else "shared"
    inherited = os.environ.get("_SY_CACHE_FD")
    if inherited:
        held = os.environ.get("_SY_CACHE_MODE")
        try:
            fd = int(inherited)
            opened = os.fstat(fd)
            valid = (opened.st_dev, opened.st_ino) == (info.st_dev, info.st_ino)
        except (ValueError, OSError):
            valid = False
        if held not in ("shared", "exclusive") or (held == "shared" and mode == "exclusive"):
            raise RuntimeError("cache mutation inside shared operation")
        if valid:
            expected_lock = "WRITE" if held == "exclusive" else "READ"
            with open(f"/proc/self/fdinfo/{fd}", encoding="ascii") as fdinfo:
                held_locks = [line.split() for line in fdinfo if line.startswith("lock:")]
            if not any(fields[2:5] == ["FLOCK", "ADVISORY", expected_lock]
                       and fields[-2:] == ["0", "EOF"] for fields in held_locks):
                raise RuntimeError("inherited cache lock is not held in the declared mode")
        if not valid:
            if held != "shared" or mode != "shared":
                raise RuntimeError("invalid inherited cache lock")
            inherited = None  # child launchers may close extra FDs; acquire a fresh shared lock
    if not inherited:
        fd = os.open(lock, os.O_RDONLY | os.O_NOFOLLOW)
        opened = os.fstat(fd)
        if (opened.st_dev, opened.st_ino) != (info.st_dev, info.st_ino):
            raise RuntimeError("cache lock changed")
        operation = fcntl.LOCK_EX if mode == "exclusive" else fcntl.LOCK_SH
        wait_seconds = float(os.environ.get("SY_CACHE_WAIT_SECONDS", "600"))
        if not math.isfinite(wait_seconds) or wait_seconds < 0:
            raise RuntimeError("invalid cache lock timeout")
        deadline = time.monotonic() + wait_seconds
        while True:
            try:
                fcntl.flock(fd, operation | fcntl.LOCK_NB)
                break
            except BlockingIOError:
                if time.monotonic() >= deadline:
                    raise RuntimeError("cache lock timeout") from None
                time.sleep(0.05)
        os.set_inheritable(fd, True)
        os.environ["_SY_CACHE_FD"] = str(fd)
        os.environ["_SY_CACHE_MODE"] = mode
    root = os.environ.get("PIP_CACHE_DIR")
    if root is None:
        with open(os.path.join(os.path.dirname(__file__), "subyard_cache_root"), encoding="utf-8") as configured:
            root = configured.read().rstrip("\n")
    # pip's wheel tags and http-v2 namespace own format versions within this native key.
    key = f"{sys.platform}-{os.uname().machine}/{sys.implementation.cache_tag}"
    os.environ["PIP_CACHE_DIR"] = root if root.endswith("/" + key) else root + "/" + key


try:
    _pip_cache_lock()
except Exception:
    # .pth import errors normally warn and continue: coordination must fail closed instead.
    os.write(2, b"subyard pip: cache coordination failed; provision the profile or run maintenance separately\n")
    os._exit(1)
