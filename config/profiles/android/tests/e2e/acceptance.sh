#!/usr/bin/env bash
# Controller for Android's disposable single-VM acceptance.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd)"
slot=''
lane=full
while [ "$#" -gt 0 ]; do
  case "$1" in
    --slot) [ "$#" -ge 2 ] || { printf 'acceptance: --slot needs a value\n' >&2; exit 2; }; slot="$2"; shift 2 ;;
    --lane) [ "$#" -ge 2 ] || { printf 'acceptance: --lane needs a value\n' >&2; exit 2; }; lane="$2"; shift 2 ;;
    *) printf 'usage: acceptance.sh --slot N [--lane full|skills|diagnostics|projects|recovery|viewer|viewer-native-debug|sdk-images|memory-lease]\n' >&2; exit 2 ;;
  esac
done
[[ "$slot" =~ ^[1-9][0-9]*$ ]] || { printf 'acceptance: --slot N is required\n' >&2; exit 2; }
case "$lane" in full|skills|diagnostics|projects|recovery|viewer|viewer-native-debug|sdk-images|memory-lease) ;; *) printf 'acceptance: invalid Android lane\n' >&2; exit 2 ;; esac
vm_type=android-test
# Skill discovery provisions the SDK and pool, but never allocates an emulator.
[ "$lane" != skills ] || vm_type=subyard-pair
bash "$ROOT/dev/agent-e2e.sh" --slot "$slot" --type "$vm_type" --vm-count 1 \
  --purpose android-pool-runtime --vm 1 -- \
  bash config/profiles/android/tests/e2e/android-pool-runtime.sh --lane "$lane"
