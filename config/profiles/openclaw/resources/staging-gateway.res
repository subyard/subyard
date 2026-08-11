# config/profiles/openclaw/resources/staging-gateway.res — a profile shared-resource descriptor.
# Parsed as assignments by the Go resource registry. The mechanics live
# in the profile-owned handler directory; the core only consults this descriptor.
COMMAND=staging
HANDLER=resources/staging-gateway/handler.sh
TITLE="Live staging gateway zone (isolated from prod)"
ACTION="up up shared-workload-change recreatable"
ACTION="start start shared-workload-change reversible"
ACTION="stop stop shared-workload-change reversible"
ACTION="status status read-only not-needed"
ACTION="logs logs read-only not-needed"
ACTION="shell shell session not-needed"
ACTION="down down runtime-destruction recreatable"
ACTION="destroy destroy runtime-destruction recreatable"
ACTION="destroy-purge destroy persistent-data-destruction irreversible"
ACTION="list list read-only not-needed"
BRINGUP=start
SHUTDOWN=stop
