#!/bin/sh
# Cargo hashes this checkout-local path for workspace members. This prevents a
# different checkout reusing built-in manifest paths while sharing dependencies.
if [ -n "${SUBYARD_RUSTC_WORKSPACE_WRAPPER:-}" ]; then
  exec "$SUBYARD_RUSTC_WORKSPACE_WRAPPER" "$@"
fi
exec "$@"
