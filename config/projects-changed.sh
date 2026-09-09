#!/usr/bin/env bash
# Shared resources own hooks in projects-changed.d; selected agent hooks live in the list.
set -euo pipefail
status=0
for hook in /usr/local/libexec/subyard/projects-changed.d/*; do
  [ -x "$hook" ] || continue
  "$hook" || status=1
done
while IFS= read -r hook; do
  [ -n "$hook" ] || continue
  "$hook" || status=1
done < /etc/subyard/agent-project-hooks
exit "$status"
