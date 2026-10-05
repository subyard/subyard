#!/usr/bin/env bash
# Stock AmneziaVPN GUI actions on disposable VM2; credentials stay in private files.
set -euo pipefail
exec /usr/bin/python3 -B "$(dirname "${BASH_SOURCE[0]}")/native-app.py" "$@"
