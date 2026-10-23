#!/usr/bin/env bash
# Shared delivery and preview migration contracts; no optional-profile content.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
for mask in 0022 0002 0077; do
  printf 'Agent skill delivery contracts: umask=%s\n' "$mask"
  (umask "$mask"; python3 -B "$ROOT/config/skills/tests/delivery-test.py")
done
