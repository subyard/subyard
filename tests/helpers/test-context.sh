#!/usr/bin/env bash
# Deterministic, host-free context shared by tests that bypass the normal config layers.

setup_test_repository() { # <fixture-root> <source-root> [engine-path]
  local fixture="${1:?}" source="${2:?}"
  install -d -m 0700 "$fixture/runtime/bin" "$fixture/runtime/config"
  cp -a "$source/scripts" "$fixture/runtime/"
  cp -a "$source/config/." "$fixture/runtime/config/"
  find "$fixture/runtime/config/profiles" -depth -delete
  install -d -m 0700 "$fixture/runtime/config/profiles/synthetic"
  printf 'PROFILE_NAME=synthetic\nPROJECT_ENV_BASE_IMAGE=debian:13\n' \
    > "$fixture/runtime/config/profiles/synthetic/profile.conf"
  printf '%s\n' '{"schema_version":1,"consumers":[{"id":"synthetic-consumer","zone":"*","path":"fixture/{zone}.env","format":"file"}]}' \
    > "$fixture/runtime/config/profiles/synthetic/profile.json"
  cp "$source/bin/yard" "$fixture/runtime/bin/yard"
  export YARD_ENGINE_PATH="${3:-${YARD_ENGINE_PATH:-$source/.build/yard}}"
}

setup_test_context() { # <temp-root> [incus-project] [instance-name]
  local root="${1:?setup_test_context needs a temp root}"
  install -d -m 0700 "$root/home" "$root/config" "$root/subyard"
  export SUBYARD_OPERATOR_HOME="$root/home"
  export SUBYARD_CONFIG_DIR="$root/public-config"
  export SUBYARD_CONFIG_HOME="$root/config"
  export SUBYARD_HOME="$root/subyard"
  export SUBYARD_STATE_DIR="$SUBYARD_HOME/state"
  export SUBYARD_ENGINE_CONTEXT=1
  export SUBYARD_ENGINE_CONTEXT_SCHEMA=1
  export SUBYARD_CONFIG_LOADED=1
  export STORAGE_PATH="$SUBYARD_HOME/incus/storage"
  export HOST_BASE="$root/host-data"
  export RESTRICTED_DISK_PATHS="$HOST_BASE"
  export YARD_KIND=container
  export SHIFT_MODE=shift
  export FORWARD_SSH_AGENT=0
  export DEV_SUDO=0
  export DEV_UID=1000
  export NESTED_E2E_VMS=0
  export E2E_VM_IMAGE=images:debian/13/cloud
  export E2E_VM_CPU=2
  export E2E_VM_MEMORY=4GiB
  export E2E_VM_DISK=10GiB
  export E2E_VM_SLOT_COUNT=2
  export E2E_VM_BOOT_TIMEOUT=300
  export INCUS_PROJECT="${2:-subyard}"
  export INCUS_BRIDGE=incusbr0
  export YARD_INSTANCE_NAME="${3:-yard}"
  export ACCESS_KIND=local
  export SSH_HOST=yard
  export SSH_CODE_HOST=yard.code
  export DEV_USER=dev
  export SSH_PORT=2222
}
