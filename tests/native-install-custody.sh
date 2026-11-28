#!/usr/bin/env bash
# Native installer generation records and lock descriptors, without root access.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
python3 -B "$ROOT/tests/native-install-custody.py"
printf 'ok: native install generation records and lock descriptors are fenced\n'
