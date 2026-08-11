# Orca is a yard-wide shared service, not a coding agent or project toolchain.
COMMAND=orca
HANDLER=resources/orca/handler.sh
TITLE="Orca remote server"
ACTION="up up host-change recreatable"
ACTION="is-up is-up read-only not-needed"
ACTION="status status read-only not-needed"
ACTION="pair pair external-change reversible"
ACTION="sync sync bounded-write not-needed"
ACTION="logs logs read-only not-needed"
ACTION="down down host-change reversible"
BRINGUP=up
SHUTDOWN=down
