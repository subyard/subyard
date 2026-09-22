#!/usr/bin/env bash
# Run interrupt and recover on successive leases of the same slot and VM.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"
[ -n "${SUBYARD_E2E_VM:-}" ] || exit 2
mode="${1:?interrupt, recover or early-init required}"
token="${2:?numeric fixture token required}"
[[ "$token" =~ ^[0-9]+$ ]] || exit 2
project=subyard-p0-real-incus
marker=agent-e2e-p0
state="$HOME/.cache/subyard-cleanup-recovery-$token"
fail() { printf 'cleanup-recovery: %s\n' "$*" >&2; exit 1; }
observe() { timeout --foreground 30 incus "$@" </dev/null; }

case "$mode" in
  early-init)
    scratch="$(mktemp -d /tmp/subyard-cleanup-init.XXXXXX)"
    trap 'find "$scratch" -depth -delete' EXIT
    install -d "$scratch/dev/e2e" "$scratch/.build"
    ln -s "$ROOT/scripts" "$scratch/scripts"
    cp dev/e2e/nested-teardown-data-boundary.sh "$scratch/dev/e2e/"
    cat >"$scratch/.build/yard" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[ "$1" = -Y ] && [ "$3" = init ] || exit 90
config="$SUBYARD_CONFIG_HOME/yards/$2/config.env"
bridge="$(sed -n 's/^INCUS_BRIDGE=//p' "$config")"
pool="$(sed -n 's/^SRV_POOL=//p' "$config")"
printf '%s\n%s\n' "$bridge" "$pool" >"$CLEANUP_INIT_RESOURCES"
[ "$(incus network get "$bridge" user.subyard.owner --project default)" = nested-teardown-e2e-v1 ] || exit 91
[ "$(incus storage get "$pool" user.subyard.owner --project default)" = nested-teardown-e2e-v1 ] || exit 92
exit 23
EOF
    chmod 0700 "$scratch/.build/yard"
    rc=0
    CLEANUP_INIT_RESOURCES="$scratch/resources" \
      bash "$scratch/dev/e2e/nested-teardown-data-boundary.sh" || rc=$?
    [ "$rc" = 23 ] || fail "expected injected init failure, got $rc"
    mapfile -t resources <"$scratch/resources"
    networks="$(observe network list --project default --format csv -c n)"
    pools="$(observe storage list --project default --format csv -c n)"
    ! grep -Fxq "${resources[0]}" <<<"$networks" || fail 'network survived early init failure'
    ! grep -Fxq "${resources[1]}" <<<"$pools" || fail 'pool survived early init failure'
    printf 'ok: early init failure removed atomically marked native network and pool\n'
    ;;
  interrupt)
    [ ! -e "$state" ] || fail 'fixture state already exists'
    bash dev/e2e/p0-guest.sh capacity-preflight "$token"
    install -d -m 0700 "$state"
    printf '%s\n' "$token" >"$state/marker"
    printf '%s\n' "$SUBYARD_E2E_GENERATION" >"$state/generation"
    child=''
    cleanup_interruption() {
      if [ -n "$child" ]; then
        kill -KILL -- "-$child" 2>/dev/null || true
        wait "$child" 2>/dev/null || true
        bash dev/e2e/p0-real-incus.sh --cleanup-only
      fi
    }
    trap cleanup_interruption EXIT
    setsid bash dev/e2e/p0-guest.sh real-incus "$token" >"$state/child.log" 2>&1 &
    child=$!
    deadline=$((SECONDS + 1800))
    while :; do
      kill -0 "$child" 2>/dev/null || { tail -n 40 "$state/child.log"; fail 'fixture exited before interruption'; }
      if command -v incus >/dev/null 2>&1 \
        && [ "$(observe list p0-vm --project "$project" -f csv -c s 2>/dev/null || true)" = RUNNING ]; then
        break
      fi
      [ "$SECONDS" -lt "$deadline" ] || fail 'fixture launch exceeded deadline'
      sleep 5
    done
    [ "$(observe project get "$project" user.subyard.p0)" = "$marker" ] || fail 'project marker missing'
    for name in p0-container p0-vm; do
      [ "$(observe config get "$name" user.subyard.p0 --project "$project")" = "$marker" ] \
        || fail 'instance marker missing'
    done
    observe image list --project default --format csv -c f | sort >"$state/images"
    kill -KILL -- "-$child"
    wait "$child" 2>/dev/null || true
    child=''
    observe list --project "$project" -f csv -c n | sort >"$state/instances"
    diff -u <(printf 'p0-container\np0-vm\n') "$state/instances"
    printf 'ok: interrupted only the owned fixture; marked resources retained for the next lease\n'
    ;;
  recover)
    [ "$(cat "$state/marker")" = "$token" ] || fail 'recovery state marker mismatch'
    [ "$(cat "$state/generation")" = "$SUBYARD_E2E_GENERATION" ] || fail 'retained VM generation changed'
    observe list --project "$project" -f csv -c n | sort | diff -u "$state/instances" -
    bash dev/e2e/p0-guest.sh capacity-preflight "$token"
    projects="$(observe project list --format csv -c n)"
    ! grep -Fxq "$project" <<<"$projects" || fail 'real-Incus project remains'
    observe image list --project default --format csv -c f | sort | diff -u "$state/images" -
    # shellcheck source=dev/e2e/lib-p0-capacity.sh
    . "$ROOT/dev/e2e/lib-p0-capacity.sh"
    p0_capacity_init "$token"
    p0_capacity_remove_build_cache
    p0_capacity_remove_root_if_empty
    bash dev/e2e/p0-guest.sh capacity-verify-cleanup "$token"
    find "$state" -depth -delete
    printf 'ok: reacquired VM recovered before capacity checks and preserved image cache\n'
    ;;
  *) fail 'expected interrupt, recover or early-init' ;;
esac
