# Orca is a yard-wide shared service, not a coding agent or project toolchain.
COMMAND=orca
HANDLER=resources/orca/handler.sh
TITLE="Orca remote server"
PROXY="orca-server ORCA_ADVERTISE_HOST ORCA_HOST_PORT tcp:127.0.0.1:6768 loopback-or-tailscale"
ACTION="up up host-change recreatable"
ACTION="is-up is-up read-only not-needed"
ACTION="status status read-only not-needed"
ACTION="pair pair external-change reversible"
ACTION="restart restart yard-change reversible"
ACTION="sync sync bounded-write not-needed"
ACTION="logs logs read-only not-needed"
ACTION="down down host-change reversible"
BRINGUP=up
SHUTDOWN=down
