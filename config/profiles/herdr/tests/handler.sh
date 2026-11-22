#!/usr/bin/env bash
set -euo pipefail

for setting in SUBYARD_OPERATOR_HOME SUBYARD_CONFIG_DIR SUBYARD_CONFIG_HOME SUBYARD_HOME \
  STORAGE_PATH HOST_BASE RESTRICTED_DISK_PATHS INCUS_BRIDGE; do
  export "$setting="
done
export SUBYARD_ENGINE_CONTEXT=1 SUBYARD_ENGINE_CONTEXT_SCHEMA=1 SUBYARD_RESOURCE_MODE=prepare
export ACCESS_KIND=local YARD_INSTANCE_NAME=fixture INCUS_PROJECT=fixture SSH_HOST=fixture
export DEV_USER=dev DEV_UID=1000 DEV_SUDO=no FORWARD_SSH_AGENT=0 NESTED_E2E_VMS=0 YARD_KIND=container
handler=config/profiles/herdr/resources/herdr/handler.sh
"$handler" status | jq -e '.action == "status" and .changed == false' >/dev/null
"$handler" up | jq -e '.action == "up" and .changed and .steps[0].decision == "conditional"' >/dev/null
if result=$(YARD_KIND=vm "$handler" up 2>&1); then exit 1; fi
[[ "$result" == *'container yards only'* ]]
if result=$(NESTED_E2E_VMS=1 "$handler" up 2>&1); then exit 1; fi
[[ "$result" == *'unavailable in nested E2E mode'* ]]
