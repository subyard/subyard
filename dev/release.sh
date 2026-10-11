#!/usr/bin/env bash
# Prepare, accept and publish one immutable release candidate.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
exec python3 -B "$ROOT/dev/release-publication.py" "$@"
