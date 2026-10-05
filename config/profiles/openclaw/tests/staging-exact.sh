#!/usr/bin/env bash
# Native staging scope containment and read-only postconditions; no live yard.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
HANDLER="$ROOT/config/profiles/openclaw/resources/staging-gateway/handler.sh"
TMP="$(mktemp -d)"
trap 'rm -rf -- "$TMP"' EXIT
. "$ROOT/tests/helpers/test-context.sh"
setup_test_context "$TMP"
if [ "${1:-}" = --prepare-parser-fixture ]; then
  export INCUS_PROJECT=subyard-openclaw-bootstrap-5b9fe2b7ea30
  export YARD_INSTANCE_NAME=yard-openclaw-bootstrap-5b9fe2b7ea30
fi
export SUBYARD_CONFIG_HOST_DIR="$TMP/config/host" SUBYARD_CONFIG_GENERATED_DIR="$TMP/config/generated"
export STAGING_EXACT_FIXTURE="$TMP" PATH="$TMP/bin:$PATH"
mkdir -p "$TMP/bin" "$TMP/config/host" "$TMP/guest/srv/staging/canonical" "$TMP/guest/srv/env-secrets/staging-canonical"
printf '%064d\n' 1 > "$TMP/container-id"
printf 'true\n' > "$TMP/running"
printf 'owned input\n' > "$TMP/guest/srv/env-secrets/staging-canonical/input"
cat > "$TMP/bin/incus" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
root="${STAGING_EXACT_FIXTURE:?}"
case "$1" in
 info) exit 0 ;;
 list) printf 'RUNNING\n'; exit 0 ;;
 exec)
  shift; while [ "$1" != -- ]; do shift; done; shift
  if [ "$1" = docker ]; then
   shift
   case "$1" in
    inspect)
     [ -f "$root/container-id" ] || exit 1
     case "${3:-}" in
      '{{.Id}}') cat "$root/container-id" ;;
      '{{.State.Running}}') cat "$root/running" ;;
      *) printf '%s %s canonical\n' "$(cat "$root/container-id")" "$(cat "$root/running")" ;;
     esac ;;
    exec) exit 1 ;;
    stop) printf 'false\n' > "$root/running" ;;
    rm) rm -f "$root/container-id" "$root/running" ;;
    *) exit 1 ;;
   esac
   exit 0
  fi
  arguments=()
  for argument in "$@"; do
   case "$argument" in /srv/*) argument="$root/guest$argument" ;; esac
   arguments+=("$argument")
  done
  exec "${arguments[@]}"
  ;;
 *) exit 1 ;;
esac
MOCK
chmod 0755 "$TMP/bin/incus"
prepare() { SUBYARD_RESOURCE_MODE=prepare "$HANDLER" "$@" > "$TMP/plan"; }
apply() {
 SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION="$(jq -r .action "$TMP/plan")" \
 SUBYARD_RESOURCE_BINDING="$(jq -r .binding "$TMP/plan")" SUBYARD_RESOURCE_STEPS="$(jq -c .steps "$TMP/plan")" \
 SUBYARD_OPERATION_ID=exact-staging "$HANDLER" "$@" > "$TMP/output" 2>&1
}
verify() {
 SUBYARD_RESOURCE_MODE=verify "$HANDLER" "$@" > "$TMP/verified"
 jq -e '.schema=="yard.resource-action-assessment.v2" and .changed==false and all(.steps[];.observed==.desired)' "$TMP/verified" >/dev/null
 [ "$(jq -r .binding "$TMP/plan")" = "$(jq -r .binding "$TMP/verified")" ]
}
prepare down
if [ "${1:-}" = --prepare-parser-fixture ]; then
  cp "$TMP/plan" "$2/down.json"
  for verb in stop destroy; do prepare "$verb"; cp "$TMP/plan" "$2/$verb.json"; done
  prepare destroy --purge; cp "$TMP/plan" "$2/destroy-purge.json"
  exit 0
fi
printf '%064d\n' 2 > "$TMP/container-id"
if apply down; then printf 'FAIL: replaced staging runner accepted\n' >&2; exit 1; fi
[ "$(cat "$TMP/running")" = true ]
printf '%064d\n' 1 > "$TMP/container-id"
apply down
verify down
prepare destroy
printf 'new input\n' > "$TMP/guest/srv/env-secrets/staging-canonical/new"
if apply destroy; then printf 'FAIL: newly observed staging artifact deleted\n' >&2; exit 1; fi
rm "$TMP/guest/srv/env-secrets/staging-canonical/new"
prepare destroy
apply destroy
verify destroy
[ -d "$TMP/guest/srv/staging/canonical" ]
prepare destroy --purge
printf 'new persistent input\n' > "$TMP/guest/srv/staging/canonical/new"
if apply destroy --purge; then printf 'FAIL: newly observed staging data deleted\n' >&2; exit 1; fi
prepare destroy --purge
apply destroy --purge
verify destroy --purge
[ ! -e "$TMP/guest/srv/staging/canonical" ]
printf 'ok: exact staging rejects replaced runtime and expanded artifacts, verifies cleanup and retention\n'
