#!/usr/bin/env bash
# Cross-profile integration belongs to the provider of the optional behavior.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
grep -Fxq 'ENVIRONMENT_PROFILES="hermes github"' "$ROOT/config/profiles/hermes/yard.env"
grep -Fq 'hermes_guest test -L /home/dev/.hermes/skills/subyard-github' \
  "$ROOT/config/profiles/github/tests/e2e/acceptance.sh"
printf 'ok: GitHub composition with Hermes has preset and live skill coverage\n'
