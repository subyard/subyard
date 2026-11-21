# Go and Python safefs parity

The shared publication vectors cover cases with the same expected result.
Two Python parity regressions also pin the approved behavior for cases that
still differ in Go.

| Case | Approved behavior and Python expectation | Go follow-up |
| --- | --- | --- |
| Finish an owned directory with sticky or set-ID bits | An owned `01500` directory becomes `01700`. Owned `02500` and `04500` directories retain their full mode and ownership. | [Go Finish](../dir_linux.go) can set each of these modes to `0700`, clearing the special bits. |
| Resolve a dangling final symlink to a user file | Return `FileNotFoundError`. A follow-enabled write leaves the link intact and the target absent. | [Go ResolveFile](../resolve_linux.go) returns the missing target path. A writer given that path can create the target. |

The shared vectors are limited to the publication outcomes that already agree.
The Go follow-up can add these cases to the shared vectors once the directory
and symlink contracts match.

The no-replace fallback also needs follow-up. Both implementations currently link
then unlink staged regular files when `renameat2` is unavailable. Interruption
between those calls can leave two links, which single-link readers in either
implementation refuse. The Python rename helper also accepts directory sources
and returns `EOPNOTSUPP` without `renameat2`. Go exposes no corresponding
directory-publication helper.
