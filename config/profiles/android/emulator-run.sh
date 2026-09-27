#!/usr/bin/env bash
# Legacy manual entrypoint deliberately cannot bypass the lease owner.
printf 'Manual emulator launch retired; use yard emu run -- COMMAND or yard emu acquire --lease-file FILE\n' >&2
exit 1
