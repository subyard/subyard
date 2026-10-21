"""Optional owning-Popen Linux kernel rusage observations.

ru_maxrss_kib is the kernel's RUSAGE_BOTH high-water accounting, including
already-reaped descendants. It is neither continuous RSS nor family RSS.
Only the exact bound CPython/Linux x86_64 implementation is instrumented.
No external waiter may consume these children. Failed constructor/exec paths
are UNKNOWN: CPython may already have internally created and reaped a child.
"""
import ctypes
import errno
import hashlib
import inspect
import os
from pathlib import Path
import platform
import subprocess
import sys
import threading
import time


_METHOD_HASHES = {'__init__': 'ab3ecc02f8bb796a6b7fc56c721222b8c06accb2ddbda46b9c9e0ac3e0b3406b', '_execute_child': '8ddae96975433851f95c535e00cc55857f1b1867ce02bda2fd9aee6ba828689c', '_internal_poll': '6698b8cbf8511076a6d2c4347efc8b3e52420955d24b0b75c87e09e80ee3f484', '_try_wait': '46f0245a487f7ae5941c4ebc6c1e541be6dced8f95f0e795f27e910b57b9ed9d', '_wait': '773f67be387e442975136954e06b0113780c904d6075cb6957d5cfa7d3ad5fbd', '_handle_exitstatus': '40ccdb9b0795bba2b286930e484d0aa44b743bc59b7677e997614c17fea413f2', 'send_signal': '4d9a77155fddf56437eaf76ba686ff074d32045519ee3f36b1629d36ea765212', '__exit__': 'a950da58bcec3542712daeb88de88272930659d0c470b2f2390793204c2bbd30', '__del__': '9b56e45207b9a13df838ae30c59d949b4050e954e80416f26a2f6a2a3a488b8c', 'wait': 'de1f4374da534008a58ceef9d5ad9639a1b6492f9c456dbf28914566cc08fe96', 'poll': '2f9c25acf0a5aac316600af15708528beb9f68095550ff2b0b11849bca963b62'}
_SUBPROCESS_SHA256 = '21ecc4c8f4fcf641974fc0d9cc28e97b07670ed1cef7e9d96769b31bb8345586'
_USAGE_FIELDS = ('utime_sec', 'utime_usec', 'stime_sec', 'stime_usec',
                 'maxrss', 'ixrss', 'idrss', 'isrss', 'minflt', 'majflt',
                 'nswap', 'inblock', 'oublock', 'msgsnd', 'msgrcv',
                 'nsignals', 'nvcsw', 'nivcsw')


class _Rusage(ctypes.Structure):
    _fields_ = [(name, ctypes.c_long) for name in _USAGE_FIELDS]


class _Siginfo(ctypes.Structure):
    # Linux native x86_64 siginfo_t: header16, SIGCHLD pid/uid/status at16/20/24.
    _fields_ = [('signo', ctypes.c_int), ('error', ctypes.c_int),
                ('code', ctypes.c_int), ('pad', ctypes.c_int),
                ('pid', ctypes.c_int), ('uid', ctypes.c_uint),
                ('status', ctypes.c_int), ('rest', ctypes.c_byte * 100)]


def _runtime_binding():
    methods = {name: hashlib.sha256(inspect.getsource(
        getattr(subprocess.Popen, name)).encode()).hexdigest()
               for name in _METHOD_HASHES}
    return {'python_version': sys.version, 'architecture': platform.machine(),
            'implementation': platform.python_implementation(),
            'subprocess_sha256': hashlib.sha256(Path(subprocess.__file__).read_bytes()).hexdigest(),
            'long_size': ctypes.sizeof(ctypes.c_long),
            'pointer_size': ctypes.sizeof(ctypes.c_void_p),
            'siginfo_size': ctypes.sizeof(_Siginfo),
            'rusage_size': ctypes.sizeof(_Rusage),
            'raw_waitid_number': 247, 'methods': methods}


def _supported(binding):
    return (sys.platform == 'linux' and binding['architecture'] == 'x86_64'
            and binding['implementation'] == 'CPython'
            and sys.version_info[:3] == (3, 13, 5)
            and sys.byteorder == 'little'
            and binding['long_size'] == binding['pointer_size'] == 8
            and binding['siginfo_size'] == 128 and binding['rusage_size'] == 144
            and binding['subprocess_sha256'] == _SUBPROCESS_SHA256
            and binding['methods'] == _METHOD_HASHES)


def _raw_terminal(pid, nohang, syscall):
    info, usage = _Siginfo(), _Rusage()
    flags = os.WEXITED | os.WNOWAIT | (os.WNOHANG if nohang else 0)
    while True:
        result = syscall(247, os.P_PID, pid, ctypes.byref(info), flags,
                         ctypes.byref(usage))
        if result == 0:
            break
        error = ctypes.get_errno()
        if error != errno.EINTR:
            raise OSError(error, 'owning usage capture failed')
    if info.pid == 0 and nohang:
        return None
    if (info.pid != pid or info.signo != 17 or info.error != 0
            or info.code not in (os.CLD_EXITED, os.CLD_KILLED, os.CLD_DUMPED)):
        raise OSError(errno.EPROTO, 'owning usage terminal mismatch')
    return (info.pid, info.code, info.status,
            tuple(getattr(usage, name) for name in _USAGE_FIELDS))


class UsageLedger:
    """Bounded scalar attempt records. Overflow is explicit UNKNOWN coverage.

    Keep the ledger alive through child cleanup. Records retain no argv,
    environment, path, exception text, output payload or executable identity.
    snapshots are safe copies; caller mutation cannot change owning state.
    """
    def __init__(self, *, capacity=64):
        if type(capacity) is not int or capacity < 0:
            raise ValueError('invalid usage record capacity')
        self.capacity = capacity
        self._origin_ns = time.monotonic_ns()
        self._records = []
        self._lock = threading.Lock()
        self.attempts = 0
        self.overflow = 0
        try:
            self.binding = _runtime_binding()
            self.supported = _supported(self.binding)
        except Exception:
            self.binding = None
            self.supported = False
        self._libc = ctypes.CDLL(None, use_errno=True) if self.supported else None
        self._syscall = self._libc.syscall if self.supported else None
        if self._syscall is not None:
            self._syscall.restype = ctypes.c_long

    def _begin(self, started):
        with self._lock:
            self.attempts += 1
            if len(self._records) >= self.capacity:
                self.overflow += 1
                return None
            record = {'attempt': self.attempts, 'pre_spawn_ns': started,
                      'owned_pid': None, 'terminal_pid': None,
                      'terminal_code': None, 'terminal_status': None,
                      'raw_wait_status': None, 'returncode': None,
                      'post_owning_wait_ns': None, 'capture_count': 0,
                      'reap_count': 0, 'state': 'UNKNOWN',
                      'reason': 'not_spawned', 'capture_errno': None}
            for name in _USAGE_FIELDS:
                record['ru_' + ('maxrss_kib' if name == 'maxrss' else name)] = None
            self._records.append(record)
            return record

    def snapshot(self):
        with self._lock:
            records = [dict(record) for record in self._records]
            for record in records:
                for name, endpoint, upward in (('interval_start_micros', 'pre_spawn_ns', False), ('interval_end_micros', 'post_owning_wait_ns', True)):
                    at = record[endpoint]
                    delta = at - self._origin_ns if type(at) is int else -1
                    rounded = (delta + (999 if upward else 0)) // 1000
                    record[name] = rounded if delta >= 0 and rounded < (1 << 64) else None
            return {'attempts': self.attempts, 'overflow': self.overflow,
                    'clock': 'PythonMonotonicNs', 'origin_ns': self._origin_ns,
                    'coverage': 'UNKNOWN' if self.overflow else 'RECORDED_ATTEMPTS',
                    'records': records}


class _OwningPopen(subprocess.Popen):
    def __init__(self, args, *, _usage, _record, **kwargs):
        # Bind everything needed by destructor/active-list cleanup to this owner.
        self._usage = _usage
        self._usage_record = _record
        self._usage_terminal = False
        self._usage_failed = False
        self._usage_probe = _raw_terminal
        self._usage_clock = time.monotonic_ns
        try:
            super().__init__(args, **kwargs)
        except BaseException:
            _record['reason'] = 'spawn_or_exec_failed_internal_child_unknown'
            self._usage_failed = True
            raise
        _record['owned_pid'] = self.pid
        _record['reason'] = 'awaiting_terminal'

    def _capture(self, nohang):
        if self._usage_terminal or self._usage_failed:
            return True
        record = self._usage_record
        try:
            terminal = self._usage_probe(self.pid, nohang, self._usage._syscall)
        except Exception as error:
            record['reason'] = 'capture_failed'
            record['capture_errno'] = error.errno if isinstance(error, OSError) else None
            self._usage_failed = True
            return True  # Cleanup continues via the original owning wait.
        if terminal is None:
            return False
        pid, code, status, usage = terminal
        record.update(terminal_pid=pid, terminal_code=code, terminal_status=status,
                      capture_count=1, reason='terminal_captured')
        for name, value in zip(_USAGE_FIELDS, usage):
            record['ru_' + ('maxrss_kib' if name == 'maxrss' else name)] = value
        self._usage_terminal = True
        return True

    def _owned_waitpid(self, flags, waitpid=os.waitpid):
        # Caller holds the original Popen lock. A live WNOHANG observation MUST
        # return without a subsequent consuming poll (child can exit meanwhile).
        if not self._capture(bool(flags & os.WNOHANG)):
            return (0, 0)
        try:
            result = waitpid(self.pid, flags)
            finished = self._usage_clock()  # Immediately after original wait.
        except OSError:
            self._usage_failed = True
            self._usage_record.update(state='UNKNOWN', reason='owning_wait_failed')
            raise
        pid, status = result
        if pid == self.pid:
            record = self._usage_record
            record.update(raw_wait_status=status, post_owning_wait_ns=finished,
                          reap_count=record['reap_count'] + 1)
        return result

    def _handle_exitstatus(self, sts, _original=subprocess.Popen._handle_exitstatus):
        _original(self, sts)
        record = self._usage_record
        record['returncode'] = self.returncode
        qualified = (self._usage_terminal and not self._usage_failed
                     and record['reap_count'] == 1
                     and record['raw_wait_status'] is not None
                     and record['post_owning_wait_ns'] is not None)
        record['state'] = 'CAPTURED' if qualified else 'UNKNOWN'

    def _try_wait(self, wait_flags):
        try:
            return self._owned_waitpid(wait_flags)
        except ChildProcessError:
            # Preserve CPython's existing ECHILD fallback status/cache behavior.
            return (self.pid, 0)

    def _internal_poll(self, _deadstate=None, _del_safe=subprocess._del_safe):
        if self.returncode is None:
            if not self._waitpid_lock.acquire(False):
                return None
            try:
                if self.returncode is not None:
                    return self.returncode
                pid, sts = self._owned_waitpid(_del_safe.WNOHANG, _del_safe.waitpid)
                if pid == self.pid:
                    self._handle_exitstatus(sts)
            except OSError as error:
                if _deadstate is not None:
                    self.returncode = _deadstate
                elif error.errno == _del_safe.ECHILD:
                    self.returncode = 0
                self._usage_record['returncode'] = self.returncode
            finally:
                self._waitpid_lock.release()
        return self.returncode


def popen(args, *, usage=None, **kwargs):
    """Launch one optional GUI leader; None is exactly the existing Popen path."""
    if usage is None:
        return subprocess.Popen(args, **kwargs)
    if not isinstance(usage, UsageLedger):
        raise TypeError('usage must be a UsageLedger')
    record = usage._begin(time.monotonic_ns())
    if record is None:
        return subprocess.Popen(args, **kwargs)
    if not usage.supported:
        record['reason'] = 'unsupported_runtime_or_abi'
        try:
            process = subprocess.Popen(args, **kwargs)
        except BaseException:
            record['reason'] = 'spawn_or_exec_failed_internal_child_unknown'
            raise
        record['owned_pid'] = process.pid
        return process
    return _OwningPopen(args, _usage=usage, _record=record, **kwargs)
