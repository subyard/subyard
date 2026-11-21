# Go and Python safefs parity

The publication vectors use `outcome` for Go and, by default, Python.
The `python_outcome` field records cases where Python differs.

| Case | Approved behavior and Python expectation | Go follow-up |
| --- | --- | --- |
| Finish an owned directory with sticky or set-ID bits | Modes `01500`, `02500`, and `04500` become `01700`, `02700`, and `04700`. A setgid directory created as `02700` under umask `077` can finish to `02755`. A non-root caller outside the directory's group skips chmod to preserve setgid. | [Go Finish](../dir_linux.go) can clear these special bits by applying the requested mode. |
| Resolve a dangling final symlink to a user file | Return `FileNotFoundError`. A follow-enabled write leaves the link intact and the target absent. | [Go ResolveFile](../resolve_linux.go) returns the missing target path. A writer given that path can create the target. |
| Atomic no-replace publication | A missing `renameat2` symbol or failed `RENAME_NOREPLACE` call returns `EOPNOTSUPP`, including a destination collision. | [Go no-replace writes](../write_linux.go) preserve the collision error and keep the link-then-unlink fallback for staged files when the syscall is unavailable. An interruption in the fallback can leave two links. |
