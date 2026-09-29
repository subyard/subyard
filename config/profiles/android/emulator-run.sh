#!/usr/bin/env bash
# Legacy manual entrypoint deliberately cannot bypass the lease owner.
printf 'Manual emulator launch retired; use android-broker run -- COMMAND or android-broker acquire --lease-file FILE\n' >&2
exit 1
