#!/usr/bin/env bash
# Run inside the renamed guest after ordinary init has converged.
set -euo pipefail
systemctl is-active --quiet subyard-telemetry.service
systemctl is-active --quiet subyard-telemetry-runs.service
