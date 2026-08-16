# Owner-host route to an already-running Hermes browser endpoint.
COMMAND=dashboard
HANDLER=resources/dashboard/handler.sh
TITLE="Hermes browser route"
PROXY="hermes-dashboard HERMES_DASHBOARD_ADVERTISE_HOST HERMES_DASHBOARD_HOST_PORT tcp:127.0.0.1:9119 owner-metadata-v1 tailscale-only"
ACTION="up up security-change reversible"
ACTION="is-up is-up read-only not-needed"
ACTION="status status read-only not-needed"
ACTION="down down security-change reversible"
BRINGUP=up
SHUTDOWN=down
