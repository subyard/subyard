# Go and Python safefs parity

The shared vectors use `outcome` for Go and, by default, Python.
The `python_outcome` field records cases where Python differs.

| Case | Approved behavior and Python expectation | Go follow-up |
| --- | --- | --- |
| Finish an owned directory with sticky or set-ID bits | Modes `01500`, `02500`, `04500`, and `07500` become `01700`, `02700`, `04700`, and `07700`. A non-root caller outside the directory's group skips chmod when setgid is present, since the kernel would clear setgid. An owner outside the group can still finish a setuid-only directory and keep setuid. | [Go Finish](../dir_linux.go) can clear these special bits by applying the requested mode. |
| Resolve a dangling final symlink to a user file | Return `FileNotFoundError`. A follow-enabled write leaves the link intact and the target absent. | [Go ResolveFile](../resolve_linux.go) returns the missing target path. A writer given that path can create the target. |
| Atomic no-replace publication | A missing `renameat2` symbol or `ENOSYS`, `EINVAL`, or `EOPNOTSUPP` from the call produces the unsupported-operation refusal. Other errors, including a collision or a missing source, preserve their errno. | [Go no-replace writes](../write_linux.go) keep the link-then-unlink fallback for staged files when the syscall is unavailable. An interruption in the fallback can leave two links. |
