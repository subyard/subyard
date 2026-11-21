# Go and Python safefs parity

The shared publication vectors cover cases with the same expected result.
Two Python parity regressions also pin the approved behavior for cases that
still differ in Go.

| Case | Approved behavior and Python expectation | Go follow-up |
| --- | --- | --- |
| Finish an owned directory with sticky or set-ID bits | Keep sticky while finishing a narrower mode. Leave setuid and setgid directories untouched. | Go Finish currently applies the requested mode and can clear these bits. |
| Resolve a dangling final symlink to a user file | Refuse the missing target and leave the link intact. | Go ResolveFile currently returns the missing target path. |
