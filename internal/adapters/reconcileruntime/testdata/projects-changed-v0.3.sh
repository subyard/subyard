#!/usr/bin/env bash
set -euo pipefail
status=0
while IFS= read -r hook; do
  [ -n "$hook" ] || continue
  "$hook" || status=1
done < /etc/subyard/agent-project-hooks
exit "$status"
