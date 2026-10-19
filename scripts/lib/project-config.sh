#!/usr/bin/env bash
# Pure rendering shared by ordinary project setup and original owner recipes.

subyard_project_owner_config() { # <operator-uid>
  SUBYARD_PROJECT_OWNER_CONFIG=(
    user.subyard.installation=1 "user.subyard.operator_uid=${1:?operator uid is required}"
    "user.subyard.context=${SUBYARD_INSTALLATION_CONTEXT:?}"
    "user.subyard.data_home=${SUBYARD_HOME?}" "user.subyard.config_home=${SUBYARD_CONFIG_HOME?}"
    "user.subyard.yard_name=${YARD_NAME:-default}" "user.subyard.instance_name=${YARD_INSTANCE_NAME:?}"
    "user.subyard.host_base=${HOST_BASE?}" "user.subyard.bridge=${INCUS_NETWORK:-${INCUS_BRIDGE:-incusbr0}}"
    "user.subyard.srv_pool=${SRV_POOL?}" "user.subyard.srv_volume=${SRV_VOLUME?}"
    "user.subyard.storage_pool=${STORAGE_POOL:-$SRV_POOL}" "user.subyard.storage_path=${STORAGE_PATH?}"
  )
}

subyard_project_policy_config() {
  SUBYARD_PROJECT_POLICY_HEAD=(restricted=true restricted.containers.nesting=allow restricted.containers.privilege=unprivileged)
  if [ "${VM_FREE_PAGE_REPORTING:-0}" = 1 ]; then
    [ "${YARD_KIND:-}" = vm ] || return 1
    SUBYARD_PROJECT_POLICY_HEAD+=(restricted.virtual-machines.lowlevel=allow)
  fi
  if [ "${NESTED_E2E_VMS:-0}" = 1 ]; then
    SUBYARD_PROJECT_POLICY_HEAD+=(restricted.containers.interception=allow)
  else
    SUBYARD_PROJECT_POLICY_HEAD+=(restricted.containers.interception=block)
  fi
  SUBYARD_PROJECT_POLICY_HEAD+=(restricted.devices.disk=allow)
  SUBYARD_PROJECT_POLICY_TAIL=(restricted.devices.unix-char=allow restricted.devices.proxy=allow)
}

subyard_project_config_render() {
  local operator_uid
  operator_uid="$(id -u "${SUBYARD_USER:-$(id -un)}")" || return
  subyard_project_owner_config "$operator_uid" || return
  subyard_project_policy_config || return
  # NUL framing retains every admitted byte, including non-UTF8 host paths.
  printf '%s\0' "${SUBYARD_PROJECT_OWNER_CONFIG[@]}" "${SUBYARD_PROJECT_POLICY_HEAD[@]}" "${SUBYARD_PROJECT_POLICY_TAIL[@]}"
}
