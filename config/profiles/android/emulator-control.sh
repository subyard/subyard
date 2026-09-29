#!/usr/bin/env bash
# Legacy process controller deliberately cannot bypass the lease owner.
printf 'Manual emulator control retired; use android-broker run/release or operator yard emu down\n' >&2
exit 1
