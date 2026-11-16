#!/usr/bin/env bash
# Release preparation shared by the owning fixture and its host-free regression.

orca_prepare_bootstrap_release() {
  # Handler acceptance and stuck recovery install the unmodified candidate.
  local unmodified=0
  [ -z "$HANDLER_ACCEPTANCE" ] && [ "${STUCK_RECOVERY:-0}" != 1 ] || unmodified=1
  if [ -e "$ROOT/.subyard-acceptance/candidate.json" ] && [ -n "$UPGRADE_FROM" ] && [ "$unmodified" = 0 ]; then
    die 'the mutated upgrade fixture requires a separate candidate'
  fi

  local package_source="$ROOT"
  if [ -n "$UPGRADE_FROM" ] && [ "$unmodified" = 0 ]; then
    # Only the source-mutating upgrade fixture needs a disposable public copy.
    mkdir "$STATE/source"
    bash "$ROOT/tests/helpers/source-files.sh" \
      | tar -C "$ROOT" --null -T - -cf - | tar -C "$STATE/source" -xf -
    package_source="$STATE/source"
    # The predecessor must be converged: its released updater cannot change
    # targets while an existing activation needs repair. Only the candidate
    # introduces this new desired value, in both local yards.
    python3 - "$STATE/source/config/agents/claude/settings.json" <<'PY_UPGRADE_DEFAULT'
import json
import pathlib
import sys
path = pathlib.Path(sys.argv[1])
value = json.loads(path.read_text())
value["autoMemoryDirectory"] = "~/.claude-memory-upgrade-fixture"
path.write_text(json.dumps(value) + "\n")
PY_UPGRADE_DEFAULT
  fi
  release_version=0.29.0-orca-bootstrap-e2e
  [ -z "$HANDLER_ACCEPTANCE" ] || release_version=0.29.0-orca-handler-e2e
  # Replacing a stuck 0.29.0 journal requires a strictly newer candidate.
  [ "${STUCK_RECOVERY:-0}" != 1 ] || release_version=0.29.1-orca-recovery-e2e
  if [ -e "$ROOT/.subyard-acceptance/candidate.json" ]; then
    release_version="$(jq -er '.version' "$ROOT/.subyard-acceptance/candidate.json")"
    mkdir -p "$STATE/release"
    cp -a "$ROOT/.subyard-acceptance/release/." "$STATE/release/"
  else
    bash "$package_source/dev/package-engine.sh" --version "$release_version" \
      --output-dir "$STATE/release" >/dev/null
  fi
}
