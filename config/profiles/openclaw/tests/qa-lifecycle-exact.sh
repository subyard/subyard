#!/usr/bin/env bash
# Native lifecycle identity and artifact guards; no credentials are read.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
HANDLER="$ROOT/config/profiles/openclaw/resources/qa-bot-broker/handler.sh"
TMP="$(mktemp -d)"
trap 'rm -rf -- "$TMP"' EXIT
. "$ROOT/tests/helpers/test-context.sh"
setup_test_context "$TMP"
if [ "${1:-}" = --prepare-parser-fixture ]; then
  export INCUS_PROJECT=subyard-openclaw-bootstrap-5b9fe2b7ea30
  export YARD_INSTANCE_NAME=yard-openclaw-bootstrap-5b9fe2b7ea30
else
  # Exercise guest ownership that an unprivileged controller cannot assign locally.
  export DEV_UID="$(($(id -u) + 1))"
fi
export SUBYARD_CONFIG_HOST_DIR="$TMP/config/host" SUBYARD_CONFIG_GENERATED_DIR="$TMP/config/generated"
export QA_EXACT_FIXTURE="$TMP" PATH="$TMP/bin:$PATH"
mkdir -p "$TMP/bin" "$TMP/guest/srv/env-secrets/qa-pool" "$TMP/guest/srv/qa-pool"
printf '%064d\n' 1 > "$TMP/container-id"
printf 'true\n' > "$TMP/running"
printf 'fixture\n' > "$TMP/guest/srv/env-secrets/qa-pool/input"
cat > "$TMP/bin/incus" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
root="${QA_EXACT_FIXTURE:?}"
case "$1" in
  info) exit 0 ;;
  list) printf 'RUNNING\n'; exit 0 ;;
  file)
    [ "$2" = push ] || exit 1
    target="${4#*\/srv\/}"
    target="$root/guest/srv/$target"
    mkdir -p "$(dirname "$target")"
    cp "$3" "$target"
    chmod 0600 "$target"
    exit 0
    ;;
  exec)
    shift
    while [ "$1" != -- ]; do shift; done
    shift
    if [ "$1" = docker ]; then
      shift
      case "$1" in
        inspect)
          [ -f "$root/container-id" ] || exit 1
          case "${3:-}" in
            '{{.Id}}') cat "$root/container-id" ;;
            '{{.State.Running}}') cat "$root/running" ;;
          esac
          ;;
        stop) printf 'false\n' > "$root/running" ;;
        rm) rm -f "$root/container-id" "$root/running" ;;
        *) exit 1 ;;
      esac
      exit 0
    fi
    if [ "$1" = install ]; then
      # Guest ownership is independent of the unprivileged controller's UID and GID.
      [ "$#" -eq 9 ] && [ "${*:1:8}" = "install -d -m 0700 -o $DEV_UID -g $DEV_UID" ] || exit 1
      set -- install -d -m 0700 -o "$(id -u)" -g "$(id -g)" "$9"
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
  local rc=0
  SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION="$(jq -r .action "$TMP/plan")" \
    SUBYARD_RESOURCE_BINDING="$(jq -r .binding "$TMP/plan")" \
    SUBYARD_RESOURCE_STEPS="$(jq -c .steps "$TMP/plan")" SUBYARD_OPERATION_ID=exact-lifecycle \
    "$HANDLER" "$@" > "$TMP/output" 2>&1 || rc=$?
  [ "$rc" -eq 0 ] || cat "$TMP/output" >&2
  return "$rc"
}
verify() {
  SUBYARD_RESOURCE_MODE=verify "$HANDLER" "$@" > "$TMP/verified"
  jq -e '.schema == "yard.resource-action-assessment.v2" and .changed == false and all(.steps[]; .observed == .desired)' "$TMP/verified" >/dev/null
  [ "$(jq -r .binding "$TMP/plan")" = "$(jq -r .binding "$TMP/verified")" ]
}
# Credential inputs stay owner-local: only their fingerprints cross consent.
mkdir -p "$SUBYARD_CONFIG_GENERATED_DIR/qa-pool" "$SUBYARD_CONFIG_HOST_DIR"
printf 'OPENCLAW_QA_CONVEX_SECRET_MAINTAINER=synthetic-maintainer\nOPENCLAW_QA_CONVEX_SECRET_CI=synthetic-ci\n' > "$SUBYARD_CONFIG_GENERATED_DIR/qa-pool/secrets.env"
chmod 0600 "$SUBYARD_CONFIG_GENERATED_DIR/qa-pool/secrets.env"
prepare expose
if [ "${1:-}" = --prepare-parser-fixture ]; then
  cp "$TMP/plan" "$2/expose.json"
  for verb in down destroy; do prepare "$verb"; cp "$TMP/plan" "$2/$verb.json"; done
  prepare destroy --purge; cp "$TMP/plan" "$2/destroy-purge.json"
  exit 0
fi
jq -e '.schema == "yard.resource-action-assessment.v2" and (.binding|length)==64 and (.steps|length)>0' "$TMP/plan" >/dev/null
if grep -q 'synthetic-' "$TMP/plan"; then printf 'FAIL: prepare serialized credential input\n' >&2; exit 1; fi
printf 'OPENCLAW_QA_CONVEX_SECRET_CI=changed-fixture\n' >> "$SUBYARD_CONFIG_GENERATED_DIR/qa-pool/secrets.env"
if apply expose > "$TMP/stale-input.out" 2>&1; then printf 'FAIL: changed prepared credential input accepted\n' >&2; exit 1; fi
[ ! -e "$TMP/guest/srv/qa-pool/client.env" ]
prepare expose
apply expose
[ "$(stat -c '%u:%g:%a' "$TMP/guest/srv/env-secrets/qa-pool")" = "$(id -u):$(id -g):700" ] \
  || { printf 'FAIL: guest directory did not retain private controller ownership\n' >&2; exit 1; }
verify expose
printf 'export OPENCLAW_QA_CONVEX_SECRET_CI=tampered-fixture\n' > "$TMP/guest/srv/qa-pool/client.env"
if SUBYARD_RESOURCE_MODE=verify "$HANDLER" expose > "$TMP/bad-verify" 2>&1; then printf 'FAIL: tampered worker credential environment verified\n' >&2; exit 1; fi
prepare down
printf '%064d\n' 2 > "$TMP/container-id"
if apply down > "$TMP/replaced-container.out" 2>&1; then printf 'FAIL: replacement container accepted\n' >&2; exit 1; fi
[ "$(cat "$TMP/running")" = true ]
printf '%064d\n' 1 > "$TMP/container-id"
apply down
verify down
prepare destroy
printf 'new\n' > "$TMP/guest/srv/env-secrets/qa-pool/new-input"
if apply destroy > "$TMP/new-artifact.out" 2>&1; then printf 'FAIL: new staged artifact accepted\n' >&2; exit 1; fi
rm "$TMP/guest/srv/env-secrets/qa-pool/new-input"
# Restore the original observed metadata after removing the added directory entry.
prepare destroy
apply destroy
verify destroy
[ -d "$TMP/guest/srv/qa-pool" ]
prepare destroy --purge
apply destroy --purge
verify destroy --purge
[ ! -e "$TMP/guest/srv/qa-pool" ]
printf 'ok: exact QA lifecycle rejects replacement identities and new artifacts, verifies removal and retention\n'
