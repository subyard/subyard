#!/usr/bin/env bash
# Rollback scenario of owner.sh, from the newest published release that shares the candidate's ledger.
PASEO_SCENARIO=rollback exec bash "$(dirname "${BASH_SOURCE[0]}")/owner.sh" "$@"
