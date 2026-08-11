# config/profiles/openclaw/resources/qa-bot-broker.res — a profile shared-resource descriptor.
# Parsed as assignments by the Go resource registry. The mechanics live
# in the profile-owned handler directory; the core only consults this descriptor.
COMMAND=qa-pool
HANDLER=resources/qa-bot-broker/handler.sh
TITLE="QA bot pool (in-yard credential broker)"
ACTION="up up shared-workload-change recreatable"
ACTION="seed seed shared-workload-change reversible"
ACTION="expose expose security-change reversible"
ACTION="status status read-only not-needed"
ACTION="logs logs read-only not-needed"
ACTION="smoke smoke shared-workload-change reversible"
ACTION="down down runtime-destruction recreatable"
ACTION="destroy destroy runtime-destruction recreatable"
ACTION="destroy-purge destroy persistent-data-destruction irreversible"
BRINGUP=up
SHUTDOWN=down
