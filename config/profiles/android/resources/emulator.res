# config/profiles/android/resources/emulator.res — a profile shared-resource descriptor.
# Parsed as assignments by the Go resource registry. It declares how the
# yard core discovers/dispatches/probes this resource; the resource's mechanics live entirely in
# its profile-owned handler directory, which the core never has to know about.
COMMAND=emu
HANDLER=resources/emulator/handler.sh
TITLE="Android emulator (in the yard; up/down include the host adb bridge)"
ACTION="up up host-change reversible"
ACTION="down down host-change reversible"
ACTION="status status read-only not-needed"
ACTION="view view session not-needed"
BRINGUP=up
SHUTDOWN=down
